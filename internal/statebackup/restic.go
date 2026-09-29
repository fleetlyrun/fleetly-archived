package statebackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// 状态备份上传轨（E3-3，对象存储专项设计 §2.3/D-S3-3/4/5）：
//
// 本地备份核一字不动——VACUUM INTO → 回读校验 → manifest → 台账的信任闭
// 环不改写；上传是 verify 成功落账之后的**追加步**（同一 Manager 任务内，
// 受既有 mu 串行）。restic 以钉版容器一次性执行（D-S3-3）：fleetlyd 经
// 底座 Docker API create→start→wait→rm（生产执行器 = *substrate.Client，
// 本包只定义端口与产物契约）。
//
// 诚实契约延伸（D-S3-4）：`restic backup --json` 输出的 snapshot id 必须
// 出现在 `restic snapshots --json <id>` 的回读列表里才记 upload_status=ok
// ——「绿色成功但实际没上传」的路径结构性不存在（与本地 verify 同型纪律）。
// 上传失败 = upload_status=failed + 事件 backup.upload_failed + system
// status backup 组件红；本地份不受影响（verify_status 不回写）。失败后当
// 日短退避重试（W3-F3 改进票，uploadretry.go：5m/15m/1h 至多 3 次——中间
// 失败只 Debug 日志，事件流不刷屏）；上一份 failed、本份 ok 时发
// backup.upload_recovered（恢复绿；失败行的重试成功同发）。
//
// 开关 = s3.mode：unset 即不上传（upload_status 保持 none——外部端点未
// 配置是合法态，不算失败不红）。rustfs 模式上传经平台托管凭据（E3-5
// 管理器生成落密钥库）与 fleetly-rustfs-net 网络挂接——管理器未收敛时以
// 凭据/端点缺失如实失败（诚实行为，不静默跳过）。
//
// secret 纪律（state-model §2.9）：restic env 的凭证值（口令/AK/SK）绝不
// 进日志/事件/台账/错误——本文件所有落笔点（台账 upload_error、事件
// payload、日志）经 scrubSecrets 兜底擦除。

// DefaultResticImage 是上传轨执行镜像的钉定形态（D-S3-3，zot 同款双锚
// 纪律：tag 保留可读性、digest 为准；多架构 index 摘要——amd64/arm64
// 通吃）。解析口径与独立复验命令见 docs/runbooks/image-prepull.md 台账
// 行（2026-09-21 docker buildx imagetools inspect restic/restic:0.19.1，
// 最新稳定版 v0.19.1）。换版须同步该台账行。
const DefaultResticImage = "restic/restic:0.19.1@sha256:136600b6ff6843d61d355f7f71f460a166429f35de6fd11b568fece3c9a4d510"

// uploadTimeout 是单次上传步的自身预算（D-S3-4）：覆盖 restic backup
//（首传含镜像拉取）+ snapshots 回读 + forget/prune 三段容器执行。与本地
// 快照轨预算（TriggerTimeout）分离——Trigger 的上传 ctx 以本预算封顶、
// 只收紧不放宽（调用方更短时以调用方为准）。
const uploadTimeout = 10 * time.Minute

// repoPathSuffix 是 restic repo 在桶内的路径后缀（设计 §2.3：两种 mode
// 同一路径形态 s3:<endpoint>/<bucket>/statebackups，无分叉）。
const repoPathSuffix = "statebackups"

// containerMountDir 是备份目录在 restic 容器内的挂载点（只读 bind）。
const containerMountDir = "/data"

// resticPasswordBytes 是 repo 口令的随机字节数（D-S3-5：32B crypto/rand，
// hex 存储后为 64 字符——restic 口令无形态约束，hex 保证容器 env 单行
// 无空白）。
const resticPasswordBytes = 32

// ResticSpec 是一次 restic 容器执行的规格（端口载荷；生产实现 =
// substrate.Client.RunRestic——容器 create/start/wait/rm + 只读 bind +
// env 注入；测试实现 = fake 断言命令构造）。
type ResticSpec struct {
	// Image 是钉定镜像引用（DefaultResticImage；执行器不得自行换镜像）。
	Image string
	// Args 是 restic 命令行（含子命令与全局扩展选项，如
	// `-o s3.bucket-lookup=path`）。
	Args []string
	// Env 是容器环境变量（RESTIC_REPOSITORY / RESTIC_PASSWORD /
	// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_DEFAULT_REGION）。
	// 值含 secret——执行器负责不落日志（substrate 侧容器 spec 组装不打印
	// env；Docker inspect 面的脱敏由底座自身访问控制承担）。
	Env map[string]string
	// HostMountDir 是宿主侧备份目录（bind 源；只读挂载）。
	HostMountDir string
	// ContainerMountDir 是容器内挂载点（只读）。
	ContainerMountDir string
	// Networks 是容器挂接的网络名（E3-5：rustfs 模式 = [fleetly-rustfs-net]
	// ——托管端点 http://rustfs:9000 只在该 overlay 内可解析；空 = 缺省
	// bridge（external 模式出网即可）。网络由 rustfs 管理器以 attachable
	// 形态创建，独立容器可挂接）。
	Networks []string
}

// ResticRunner 是 restic 钉版容器一次性执行端口（实现在 internal/substrate
// ——适配器方向：substrate → 本包核心端口，与 build.DaemonManager /
// logs.Port 同型；导出面仅限端口与载荷——生产/测试实现经隐式满足装配）。
// RunRestic 的 output 是容器 stdout 全文（restic --json 的结构化消息来源）；
// err 非 nil 表示容器执行失败（非零退出/超时/底座错误）。
type ResticRunner interface {
	RunRestic(ctx context.Context, spec ResticSpec) (output string, err error)
}

// resticTarget 把 s3 设置解析为 restic repo 目标（repo URL + bucket-lookup
// 形态 + 凭证）。设计 §2.3：
//   - external：设置值直出——repo = s3:<endpoint_url>/<bucket>/statebackups；
//     path-style 跟随 s3.path_style 设置（与 objectstore.PathStyle 同源同义）；
//   - rustfs：服务端派生 http://rustfs:9000 + 平台单桶 + path-style（凭据
//     随 E3-5 管理器落库，resticCredentials 消费）；
//   - unset：found=false（合法态，上传轨整体停摆）。
//
// restic 0.19.x 事实核对（v0.19.1 internal/backend/s3/s3.go + `restic
// options`）：端点由 RESTIC_REPOSITORY 的 `s3:<endpoint>/<bucket>` 形态
// 携带（s3: 后带 scheme 时按其定 http/https）；path-style 用扩展选项
// `-o s3.bucket-lookup=path`（默认 auto——对 IP/自定义主机可能误选
// virtual-host，显式跟随设置值）；凭证 = AWS_ACCESS_KEY_ID/
// AWS_SECRET_ACCESS_KEY；区域 = AWS_DEFAULT_REGION。
func resticTarget(in state.S3Settings) (repo string, pathStyle bool, creds s3Credentials, found bool, err error) {
	switch state.NormalizeMode(in.Mode) {
	case state.S3ModeUnset:
		return "", false, s3Credentials{}, false, nil
	case state.S3ModeExternal:
		if strings.TrimSpace(in.EndpointURL) == "" || strings.TrimSpace(in.Bucket) == "" {
			return "", false, s3Credentials{}, true, fmt.Errorf(
				"statebackup: s3.mode=external requires endpoint_url and bucket (settings incomplete)")
		}
		repo = "s3:" + in.EndpointURL + "/" + in.Bucket + "/" + repoPathSuffix
		creds = s3Credentials{AccessKeyID: in.AccessKeyID, SecretKey: in.SecretAccessKey, Region: in.Region}
		return repo, in.PathStyle, creds, true, nil
	case state.S3ModeRustfs:
		repo = "s3:" + state.RustfsEndpointURL + "/" + state.RustfsBucketName + "/" + repoPathSuffix
		return repo, true, s3Credentials{}, true, nil
	default:
		return "", false, s3Credentials{}, true, fmt.Errorf("statebackup: unknown s3.mode %q", in.Mode)
	}
}

// s3Credentials 是解密后的 S3 凭证集（内存存活，零落笔——只进容器 env）。
type s3Credentials struct {
	AccessKeyID string
	SecretKey   string
	Region      string
}

// resticPassword 取回 repo 口令明文（D-S3-5 惰性生成）：platform_settings
// 键 s3.restic_password 存 envelope 密文——已有则解密；没有则生成
//（crypto/rand 32B → hex）→ envelope 加密落库 → 日志记指纹（不建事件、
// 不扩审计 action 词表——§2.3 裁决：平台内部凭据生命周期，非用户可见变更）。
// 与主密钥文件（fleetly.key）的分离语义 = 独立条目独立轮换能力：口令泄露
// 不暴露数据、主密钥泄露不暴露远端 repo；envelope 主密钥仍是同一把 age key。
func (m *Manager) resticPassword(ctx context.Context) (string, error) {
	ct, found, err := m.store.LoadResticPasswordCiphertext(ctx)
	if err != nil {
		return "", fmt.Errorf("statebackup: load restic password: %w", err)
	}
	if found {
		if ct == "" {
			return "", errors.New("statebackup: stored restic password is empty")
		}
		plain, err := m.box.Decrypt([]byte(ct))
		if err != nil {
			return "", fmt.Errorf("statebackup: decrypt restic password: %w", err)
		}
		return string(plain), nil
	}
	raw := make([]byte, resticPasswordBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("statebackup: generate restic password: %w", err)
	}
	plain := hex.EncodeToString(raw)
	ctBytes, err := m.box.Encrypt([]byte(plain))
	if err != nil {
		return "", fmt.Errorf("statebackup: encrypt restic password: %w", err)
	}
	if err := m.store.SaveResticPasswordCiphertext(ctx, string(ctBytes)); err != nil {
		return "", fmt.Errorf("statebackup: store restic password: %w", err)
	}
	m.log.Info("backup: restic repository password generated (stored encrypted; "+
		"fingerprint for operator cross-check only)", "fingerprint", shortFingerprint(plain))
	return plain, nil
}

// shortFingerprint 是 secret 的展示指纹（明文 sha256 前 8 hex——只判
// 「是不是那个 secret」，材料零出现；与 api 面 secretFingerprint 同口径）。
func shortFingerprint(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:8])
}

// uploadSnapshot 是上传步入口（verify 成功落账与保留期清理之后调用；ctx
// 应携带上传预算）。返回回读台账后的最终行投影（Trigger 的同步响应据此
// 反映上传结论）。本函数绝不推翻本地 verified 结论——任何上传失败只红
// 上传面（upload_status / 事件 / 组件），不影响函数签名上的备份成功语义。
//
// retry 区分首传与退避重试（W3-F3 改进票）：首传失败 = backup.upload_
// failed 事件（诚实红的唯一落点）；重试失败只 Debug 日志 + 台账 upload_
// error 刷新（行保持 failed，事件流不刷屏）；重试成功 = ok + backup.
// upload_recovered（行自身 failed → ok 的恢复绿，不依赖前行的失败状态）。
func (m *Manager) uploadSnapshot(ctx context.Context, rec state.StateBackup, retry bool) state.StateBackup {
	out := rec
	finalRow := func() state.StateBackup {
		if r, err := m.store.GetStateBackup(ctx, rec.ID); err == nil {
			return r
		}
		return out // 台账回读失败时保底返回内存投影（诚实面已由台账落定）
	}

	in, err := m.store.LoadS3Settings(ctx)
	if err != nil {
		return m.uploadFail(ctx, rec, nil, retry, fmt.Errorf("load s3 settings: %w", err))
	}
	repo, pathStyle, _, found, err := resticTarget(in)
	if err != nil {
		return m.uploadFail(ctx, rec, nil, retry, err)
	}
	if !found {
		// s3.mode=unset：合法态——不上传、不算失败、不红（设计 §2.3：
		// 上传轨开关 = s3.mode unset 即停；负面测试钉住）。
		m.log.Info("backup: upload skipped (s3.mode=unset; upload track off)", "id", rec.ID)
		return finalRow()
	}
	if m.runner == nil || m.box == nil {
		// 装配缺位（测试形态/精简装配）：不标 failed（不是上传失败，是
		// 上传轨未接线）——upload_status 保持 none，如实可见。
		m.log.Warn("backup: upload track not assembled (runner/secrets box missing); upload_status stays none", "id", rec.ID)
		return finalRow()
	}

	// 口令（惰性生成）与凭证解密。
	password, err := m.resticPassword(ctx)
	if err != nil {
		return m.uploadFail(ctx, rec, nil, retry, err)
	}
	creds, err := m.resticCredentials(ctx, in)
	if err != nil {
		return m.uploadFail(ctx, rec, nil, retry, err)
	}
	scrub := []string{password, creds.AccessKeyID, creds.SecretKey}

	// rustfs 模式的网络挂接（E3-5）：托管端点 http://rustfs:9000 只在
	// fleetly-rustfs-net 内可解析——restic 一次性容器 attach 该网（rustfs
	// 管理器以 attachable 形态建网）；external 模式零挂接（缺省 bridge 出网）。
	networks := []string(nil)
	if state.NormalizeMode(in.Mode) == state.S3ModeRustfs {
		networks = []string{state.RustfsNetworkName}
	}

	// 上一份的上传结论（恢复绿判定依据；须在本份落结论之前取）。
	prev := m.previousUploadStatus(ctx, rec.ID)

	baseEnv := map[string]string{
		"RESTIC_REPOSITORY": repo,
		"RESTIC_PASSWORD":   password,
	}
	if creds.AccessKeyID != "" || creds.SecretKey != "" {
		baseEnv["AWS_ACCESS_KEY_ID"] = creds.AccessKeyID
		baseEnv["AWS_SECRET_ACCESS_KEY"] = creds.SecretKey
	}
	if creds.Region != "" {
		baseEnv["AWS_DEFAULT_REGION"] = creds.Region
	}
	lookupArgs := []string{}
	if pathStyle {
		lookupArgs = []string{"-o", "s3.bucket-lookup=path"}
	}
	spec := func(args ...string) ResticSpec {
		return ResticSpec{
			Image:             DefaultResticImage,
			Args:              append(append([]string{}, lookupArgs...), args...),
			Env:               baseEnv,
			HostMountDir:      filepath.Dir(rec.Path),
			ContainerMountDir: containerMountDir,
			Networks:          networks,
		}
	}

	// ⓪ 仓库惰性初始化（首传自举；两种 mode 同一路径形态）。仓库不存在
	// 时 restic backup 必然失败——init 前置使「首传即成」成立；已初始化
	// 时幂等通过（W3-F1 同族真机发现：restic 0.19.1 对已初始化仓库有两种
	// 文案——"config file already exists" 与 "repository master key and
	// config already initialized"，后者见真机 RustFS 第二次上传）。口令 =
	// resticPassword（同一把，D-S3-5）；repo 经 RESTIC_REPOSITORY env 携带
	//（init 不收位置参数）。
	if _, ierr := m.runner.RunRestic(ctx, spec("init", "--repository-version", "2")); ierr != nil &&
		!strings.Contains(ierr.Error(), "config file already exists") &&
		!strings.Contains(ierr.Error(), "already initialized") {
		return m.uploadFail(ctx, rec, scrub, retry, fmt.Errorf("restic init: %w", ierr))
	}

	// ① 上传本体（输出含 summary.snapshot_id——restic 0.19 --json 契约）。
	outStr, err := m.runner.RunRestic(ctx, spec("backup", containerMountDir, "--json"))
	if err != nil {
		return m.uploadFail(ctx, rec, scrub, retry, fmt.Errorf("restic backup: %w", err))
	}
	snapID := parseBackupSnapshotID(outStr)
	if snapID == "" {
		return m.uploadFail(ctx, rec, scrub, retry, errors.New(
			"restic backup produced no snapshot id (summary message missing or snapshot creation skipped)"))
	}

	// ② 回读校验（D-S3-4 诚实契约）：该 id 必须在远端 snapshots 列表中。
	snapsOut, err := m.runner.RunRestic(ctx, spec("snapshots", "--json", snapID))
	if err != nil {
		return m.uploadFail(ctx, rec, scrub, retry, fmt.Errorf("restic snapshots readback: %w", err))
	}
	if !snapshotsContain(snapsOut, snapID) {
		return m.uploadFail(ctx, rec, scrub, retry, fmt.Errorf(
			"restic readback: snapshot %s not listed in remote repository (green-but-not-uploaded guard)", snapID))
	}

	// ③ 结论落账：ok（+ 恢复绿事件——前行 failed 或本行是失败重试，二者
	// 任一即红→绿闭环）。
	if err := m.store.UpdateStateBackupUpload(ctx, rec.ID, state.BackupUploadOK, ""); err != nil {
		m.log.Error("backup: upload ok but ledger update failed", "id", rec.ID, "error", err)
		return finalRow()
	}
	m.log.Info("backup: uploaded and read-back verified", "id", rec.ID,
		"snapshot", snapID, "repo_path_suffix", repoPathSuffix)
	if prev == state.BackupUploadFailed || retry {
		m.emitEvent(ctx, "backup.upload_recovered", rec.ID,
			state.DiffSummary("backup_id", rec.ID))
	}

	// ④ 保留对齐尾部：restic forget --keep-last <keep> --prune（与本地
	// 保留份数对齐；失败只告警不回滚——远端多留几份无害，本地份不受影响）。
	if _, err := m.runner.RunRestic(ctx, spec("forget",
		"--keep-last", strconv.Itoa(m.cfg.Keep), "--prune")); err != nil {
		m.log.Warn("backup: restic forget failed (remote retention not aligned; local backup unaffected)",
			"id", rec.ID, "error", scrubText(err.Error(), scrub))
	}
	return finalRow()
}

// resticCredentials 解密上传凭证（external 模式 secret 密文 → 明文；
// rustfs 模式 → 平台托管凭据，E3-5 管理器生成后经 envelope 密文落库——
// 凭据缺失/密钥损坏显式失败，不静默跳过——诚实红）。
func (m *Manager) resticCredentials(ctx context.Context, in state.S3Settings) (s3Credentials, error) {
	switch state.NormalizeMode(in.Mode) {
	case state.S3ModeExternal:
		creds := s3Credentials{AccessKeyID: in.AccessKeyID, SecretKey: in.SecretAccessKey, Region: in.Region}
		if creds.AccessKeyID == "" || creds.SecretKey == "" {
			return s3Credentials{}, errors.New(
				"statebackup: s3.mode=external requires access_key_id and secret_access_key (credentials incomplete)")
		}
		plain, err := m.box.Decrypt([]byte(creds.SecretKey))
		if err != nil {
			return s3Credentials{}, fmt.Errorf("statebackup: decrypt s3 secret: %w", err)
		}
		creds.SecretKey = string(plain)
		return creds, nil
	case state.S3ModeRustfs:
		// 托管凭据（E3-5）：rustfs 管理器在 mode=rustfs 收敛时生成并存
		// envelope 密文；此路径只读不生成（生成职责唯一归管理器）。未备便
		//（生成拍未到）显式失败——上传轨下次触发自然重试。
		accessCT, secretCT, found, err := m.store.LoadRustfsCredentialsCiphertext(ctx)
		if err != nil {
			return s3Credentials{}, fmt.Errorf("statebackup: load rustfs credentials: %w", err)
		}
		if !found {
			return s3Credentials{}, errors.New(
				"statebackup: managed rustfs credentials not provisioned yet (the rustfs manager provisions them shortly after s3.mode=rustfs is saved)")
		}
		accessPlain, err := m.box.Decrypt([]byte(accessCT))
		if err != nil {
			return s3Credentials{}, fmt.Errorf("statebackup: decrypt rustfs access key: %w", err)
		}
		secretPlain, err := m.box.Decrypt([]byte(secretCT))
		if err != nil {
			return s3Credentials{}, fmt.Errorf("statebackup: decrypt rustfs secret key: %w", err)
		}
		return s3Credentials{AccessKeyID: string(accessPlain), SecretKey: string(secretPlain)}, nil
	default:
		return s3Credentials{}, fmt.Errorf("statebackup: unknown s3.mode %q", in.Mode)
	}
}

// previousUploadStatus 返回比 rec 更早的最近一行非 none 上传结论（恢复绿
// 判定：failed → 本份 ok 发 recovered）。none（含从未上传）返回 none。
func (m *Manager) previousUploadStatus(ctx context.Context, id string) string {
	rows, err := m.store.ListStateBackups(ctx, 0)
	if err != nil {
		return state.BackupUploadNone // 判定面降级为无前史——只影响事件，不影响结论
	}
	for _, r := range rows { // created_at 倒序：跳过本份后取第一个非 none
		if r.ID == id {
			continue
		}
		if r.UploadStatus == state.BackupUploadOK || r.UploadStatus == state.BackupUploadFailed {
			return r.UploadStatus
		}
	}
	return state.BackupUploadNone
}

// uploadFail 落上传失败面：台账 failed 行 + （首传时）事件 backup.upload_
// failed + （组件面）健康红（CheckHealth 读台账）。重试路径（W3-F3 退避
// 重试）不重发事件——只 Debug 日志 + 刷新台账 upload_error（行保持 failed，
// 事件流不刷屏）。scrub 为已知 secret 值集（可空——凭证解密之前的失败没
// 有可擦材料），错误摘要经擦除后入台账/事件。
func (m *Manager) uploadFail(ctx context.Context, rec state.StateBackup, scrub []string, retry bool, err error) state.StateBackup {
	summary := scrubText(err.Error(), scrub)
	if uerr := m.store.UpdateStateBackupUpload(ctx, rec.ID, state.BackupUploadFailed, summary); uerr != nil {
		m.log.Error("backup: upload failed AND ledger update failed", "id", rec.ID,
			"upload_error", summary, "ledger_error", uerr.Error())
	}
	if retry {
		// 重试中间失败：台账保持 failed（错误原文刷新供诊断），事件面静默
		// ——诚实红的首次落点在首传，这里只留运行面踪迹。
		m.log.Debug("backup: upload retry attempt failed (row stays failed, no event)",
			"id", rec.ID, "error", summary)
	} else {
		m.emitEvent(ctx, "backup.upload_failed", rec.ID,
			state.DiffSummary("backup_id", rec.ID, "error", summary))
		m.log.Error("backup: remote upload failed (upload_status=failed recorded; "+
			"local snapshot unaffected; system status backup component is degraded until next ok upload)",
			"id", rec.ID, "error", summary)
	}
	out := rec
	if r, rerr := m.store.GetStateBackup(ctx, rec.ID); rerr == nil {
		out = r
	}
	return out
}

// emitEvent 追加平台事件（Outbox 单写；upload 轨事件与业务写不同事务——
// 结论已在台账先行落定，事件是披露面）。
func (m *Manager) emitEvent(ctx context.Context, name, id, payload string) {
	if err := m.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.AppendEvent(ctx, state.Event{Name: name, Subject: "backup:" + id, Payload: payload})
		return err
	}); err != nil {
		m.log.Warn("backup: event append failed", "event", name, "id", id, "error", err)
	}
}

// scrubText 把已知 secret 值从文本中擦除（兜底：restic/底座错误文本若
// 意外回显材料，台账/事件/日志面不落明文）。
func scrubText(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// backupSummary 是 restic backup --json 的 summary 消息（restic 0.19
// scripting 契约：message_type=summary、snapshot_id 在快照创建成功时
// 非空）。
type backupSummary struct {
	MessageType string `json:"message_type"`
	SnapshotID  string `json:"snapshot_id"`
}

// parseBackupSnapshotID 从 restic backup --json 输出提取本次快照 id
//（逐行 JSON：找 message_type=summary 的 snapshot_id 字段；无 → 空串）。
func parseBackupSnapshotID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m backupSummary
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.MessageType == "summary" && m.SnapshotID != "" {
			return m.SnapshotID
		}
	}
	return ""
}

// snapshotsContain 解析 restic snapshots --json <id> 输出（JSON 数组），
// 报告目标 id 是否在列（比较忽略 short-id/long-id 形差：前缀匹配任一行）。
func snapshotsContain(output, snapID string) bool {
	var snaps []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &snaps) != nil {
		return false // 解析失败 = 回读未证实（诚实契约：不含糊放行）
	}
	for _, s := range snaps {
		if s.ID == snapID || strings.HasPrefix(snapID, s.ID) || strings.HasPrefix(s.ID, snapID) {
			return true
		}
	}
	return false
}
