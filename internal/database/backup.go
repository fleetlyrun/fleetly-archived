package database

// 备份编排与调度（managed-databases §2.6/§5.4 的 S5 落地）：TriggerBackup
// 异步受理（API 快回，job 数分钟级）→ runBackup 同步链（材料解析 → 备份
// job → 回读校验 → 台账 → 事件 → 保留对齐）；调度 duty 挂收敛拍（per 实
// 例计划：interval/hour_utc/keep，平台缺省 24h/7 份/03:00 UTC——§5.4 配置
// 键的常量形态，adapters.go）。
//
// 备份目标（D-DB-6 裁决：独立 repo 被否）：与控制面上传轨**同一** restic
// repo——URL 同构（s3:<endpoint>/<bucket>/statebackups）、口令同源
//（platform_settings 键 s3.restic_password，envelope 密文），repo 内以
// 路径命名空间 db/<instance>/ 分立。本文件对 resticTarget/resticCredentials
// 的同构实现以注释锚定 statebackup 侧逐字对应（跨包共享端口是 E3 内部
// 事实，不值得为此抽公共包引入反向依赖）。
//
// 诚实口径：
//   - s3.mode=unset → ErrBackupS3NotConfigured（手动触发同步回 409 族；
//     调度触发经 db.backup_failed 事件披露——两种入口同一错误事实）；
//   - 备份 job 失败 → db.backup_failed 事件，无台账行（台账只记完成行
//     ——state 层 InsertDatabaseBackup 的快照标识必填与该口径一致）；
//   - 备份成功必经回读校验：verified 才算绿；verify 失败 → 台账行
//     verify_status=failed（红色告警面）+ db.backup_failed 事件；
//   - 在途备份无台账行（完成时刻落账——db_backups.created_at 语义）。
//
// 明文纪律：凭据/repo 口令/S3 秘钥只进 job env；错误文本经 scrubSecrets
// 兜底后进事件/台账/日志（statebackup 同款负面测试面）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ErrBackupS3NotConfigured 表示备份触发时对象存储未配置（s3.mode=unset；
// api 映射 E_S3_NOT_CONFIGURED——与引擎 s3inject 的注入前哨同码同语义：
// 「目标缺失是合法态但该操作拒绝假成功」）。
var ErrBackupS3NotConfigured = errors.New("object storage is not configured (s3.mode=unset); database backups require s3.mode=external or rustfs")

// OperationPrestateError 是备份/恢复/升级的前置态违规（§2.3 操作表；api
// 映射 E_STATE_VERSION_CONFLICT 409，context 带 current_state 与合法前置
// 态清单——轮换 RotationPrestateError 同型）。
type OperationPrestateError struct {
	Operation string
	Current   string
	Legal     string
}

func (e *OperationPrestateError) Error() string {
	return fmt.Sprintf("%s requires state %s (current: %s)", e.Operation, e.Legal, e.Current)
}

// ErrOperationInFlight 表示该实例已有备份/恢复/升级在途（per 实例操作互
// 斥哨兵；api 映射 E_STATE_VERSION_CONFLICT 409——同族乐观冲突语义）。
type ErrOperationInFlight struct{ Current string }

func (e ErrOperationInFlight) Error() string {
	return "another database operation is already in flight (" + e.Current + "); wait for it to finish"
}

// dbRepoPathSuffix 是库备份与控制面快照共享 repo 的桶内路径尾段（**与
// statebackup.repoPathSuffix 逐字同值**——D-DB-6「同一 repo」裁决的唯一
// 事实源在两处注释锚定；改动任一侧必须同步另一侧，负面测试不做字符串
// 硬编码以防两侧悄然分叉的假阴性）。
const dbRepoPathSuffix = "statebackups"

// beginOp 占用 per 实例操作互斥哨兵（占用失败 = 已有在途——返回 false；
// 幂等收口 endOp 由 defer 保证）。op 词表：backup/restore/upgrade。
func (m *Manager) beginOp(instanceID, op string) bool {
	m.opsMu.Lock()
	defer m.opsMu.Unlock()
	if cur, ok := m.inflightOps[instanceID]; ok {
		_ = cur
		return false
	}
	if m.inflightOps == nil {
		m.inflightOps = map[string]string{}
	}
	m.inflightOps[instanceID] = op
	return true
}

// endOp 释放操作互斥哨兵（编排出口单点）。
func (m *Manager) endOp(instanceID string) {
	m.opsMu.Lock()
	defer m.opsMu.Unlock()
	delete(m.inflightOps, instanceID)
}

// opBusy 报告实例是否操作在途（收敛拍跳过实例的判据——restore/upgrade
// 期间收敛器不得以副本水位/健康观察干扰在途编排）。
func (m *Manager) opBusy(instanceID string) bool {
	m.opsMu.Lock()
	defer m.opsMu.Unlock()
	_, busy := m.inflightOps[instanceID]
	return busy
}

// TriggerBackup 异步受理一次备份（API 面：快回 accepted；kind=manual）。
// 前置态/互斥/S3 配置在此同步裁决（错误即时回调用方）；job 链异步执行，
// 结论经台账 + db.backup_* 事件披露。
func (m *Manager) TriggerBackup(ctx context.Context, name string) error {
	inst, err := m.store.GetDatabaseInstanceByName(ctx, name)
	if err != nil {
		return err
	}
	if inst.State == state.DatabaseDeleting || inst.State == state.DatabaseDeleted {
		return fmt.Errorf("%w: %s", state.ErrDatabaseNotFound, inst.Name)
	}
	if err := checkBackupPrestate(&inst); err != nil {
		return err
	}
	if !m.beginOp(inst.ID, "backup") {
		return ErrOperationInFlight{Current: "backup/restore/upgrade"}
	}
	// S3 配置同步前哨（手动触发的诚实拒绝面——材料缺失即不受理、不启 job；
	// 错误同步回调用方 + db.backup_failed 事件披露。调度路径无同步调用方，
	// 完全走 runBackup 内的同一解析失败面）。设置在受理与 job 之间的变更
	// 由 runBackup 重解析兜底（竞态窗口的失败 = 事件面诚实红）。
	if _, err := m.resolveBackupTarget(ctx); err != nil {
		m.endOp(inst.ID)
		m.backupFailed(ctx, &inst, dbtemplate.BackupManual, "", err)
		return err
	}
	m.opsWG.Add(1)
	go func() {
		defer m.opsWG.Done()
		defer m.endOp(inst.ID)
		// 异步链脱离请求 ctx（受理已返回）——预算 = 各 job 步自身，整体
		// 上界护栏取备份链步预算之和的宽限形态。
		bctx, cancel := context.WithTimeout(context.Background(), backupJobTimeout+verifyJobTimeout+pruneJobTimeout)
		defer cancel()
		if _, err := m.runBackup(bctx, &inst, dbtemplate.BackupManual); err != nil {
			m.log.Warn("database: backup failed", "instance", inst.Name, "error", err.Error())
		}
	}()
	return nil
}

// checkBackupPrestate 备份前置态（§2.3 操作表：ready/degraded——paused 引
// 擎停摆导不出一致性快照、provisioning 在途收敛）。
func checkBackupPrestate(inst *state.DatabaseInstance) error {
	switch inst.State {
	case state.DatabaseReady, state.DatabaseDegraded:
		return nil
	default:
		return &OperationPrestateError{Operation: "backup", Current: string(inst.State), Legal: "ready, degraded"}
	}
}

// backupTarget 是一次备份/恢复链的执行材料（编排层解析一次、适配器消费；
// 凭据明文只存活于内存链）。
type backupTarget struct {
	Repository   string
	ResticPass   string
	AccessKeyID  string
	SecretKey    string
	Region       string
	PathStyle    bool
	AttachRustfs bool
}

// resolveBackupTarget 解析共享 restic repo 目标与全部凭证材料（D-DB-6 同
// repo 口径——resticTarget/resticCredentials 的同构实现，与 statebackup
// 逐字对应；s3.mode=unset 是合法态 → ErrBackupS3NotConfigured）。
func (m *Manager) resolveBackupTarget(ctx context.Context) (backupTarget, error) {
	in, err := m.store.LoadS3Settings(ctx)
	if err != nil {
		return backupTarget{}, fmt.Errorf("database: load s3 settings: %w", err)
	}
	var t backupTarget
	switch state.NormalizeMode(in.Mode) {
	case state.S3ModeUnset:
		return backupTarget{}, ErrBackupS3NotConfigured
	case state.S3ModeExternal:
		if strings.TrimSpace(in.EndpointURL) == "" || strings.TrimSpace(in.Bucket) == "" {
			return backupTarget{}, errors.New("database: s3.mode=external requires endpoint_url and bucket (settings incomplete)")
		}
		t.Repository = "s3:" + in.EndpointURL + "/" + in.Bucket + "/" + dbRepoPathSuffix
		t.PathStyle = in.PathStyle
		if in.AccessKeyID == "" || in.SecretAccessKey == "" {
			return backupTarget{}, errors.New("database: s3.mode=external requires access_key_id and secret_access_key (credentials incomplete)")
		}
		plain, derr := m.box.Decrypt([]byte(in.SecretAccessKey))
		if derr != nil {
			return backupTarget{}, fmt.Errorf("database: decrypt s3 secret: %w", derr)
		}
		t.AccessKeyID, t.SecretKey, t.Region = in.AccessKeyID, string(plain), in.Region
	case state.S3ModeRustfs:
		// 托管派生端点 + 平台单桶 + path-style；托管凭据（rustfs duty 生成
		// 落库）——未备便显式失败，上传轨同口径的诚实红。
		t.Repository = "s3:" + state.RustfsEndpointURL + "/" + state.RustfsBucketName + "/" + dbRepoPathSuffix
		t.PathStyle = true
		t.AttachRustfs = true
		accessCT, secretCT, found, lerr := m.store.LoadRustfsCredentialsCiphertext(ctx)
		if lerr != nil {
			return backupTarget{}, fmt.Errorf("database: load rustfs credentials: %w", lerr)
		}
		if !found {
			return backupTarget{}, errors.New("database: managed rustfs credentials not provisioned yet (the rustfs duty provisions them shortly after s3.mode=rustfs is saved)")
		}
		accessPlain, aerr := m.box.Decrypt([]byte(accessCT))
		if aerr != nil {
			return backupTarget{}, fmt.Errorf("database: decrypt rustfs access key: %w", aerr)
		}
		secretPlain, serr := m.box.Decrypt([]byte(secretCT))
		if serr != nil {
			return backupTarget{}, fmt.Errorf("database: decrypt rustfs secret key: %w", serr)
		}
		t.AccessKeyID, t.SecretKey = string(accessPlain), string(secretPlain)
	default:
		return backupTarget{}, fmt.Errorf("database: unknown s3.mode %q", in.Mode)
	}
	// repo 口令（与控制面上传轨同一把——只读不生成：生成职责唯一归
	// statebackup 上传轨，双写者惰性生成会竞态换锁。未生成 = 控制面备份
	// 尚未跑过第一传，错误文本给可行动指引）。
	ct, found, err := m.store.LoadResticPasswordCiphertext(ctx)
	if err != nil {
		return backupTarget{}, fmt.Errorf("database: load restic password: %w", err)
	}
	if !found || ct == "" {
		return backupTarget{}, errors.New("database: restic repository password is not provisioned yet (trigger a control-plane backup once — the upload track provisions it on first upload)")
	}
	plain, err := m.box.Decrypt([]byte(ct))
	if err != nil {
		return backupTarget{}, fmt.Errorf("database: decrypt restic password: %w", err)
	}
	t.ResticPass = string(plain)
	return t, nil
}

// runBackup 是备份同步链（TriggerBackup 异步路径与升级备份门共用；调用
// 方持有操作互斥哨兵）：备份 job → 回读校验 → 台账 + 事件 → 保留对齐。
// 返回产出（升级门消费 verify 结论）。
func (m *Manager) runBackup(ctx context.Context, inst *state.DatabaseInstance, kind dbtemplate.BackupKind) (dbtemplate.BackupOutcome, error) {
	if err := checkBackupPrestate(inst); err != nil {
		return dbtemplate.BackupOutcome{}, err
	}
	if _, err := dbtemplate.Get(inst.Template); err != nil {
		return dbtemplate.BackupOutcome{}, fmt.Errorf("template %q is not in the platform registry", inst.Template)
	}
	password, err := m.decryptCredential(inst)
	if err != nil {
		return dbtemplate.BackupOutcome{}, err
	}
	target, err := m.resolveBackupTarget(ctx)
	if err != nil {
		// S3 未配置是合法态的诚实拒绝（哨兵原样回传——api 同步映射
		// E_S3_NOT_CONFIGURED；其余材料失败 = E_DB_BACKUP_FAILED 信封）。
		if errors.Is(err, ErrBackupS3NotConfigured) {
			m.backupFailed(ctx, inst, kind, "", err)
			return dbtemplate.BackupOutcome{}, err
		}
		failure := apperr.New("E_DB_BACKUP_FAILED", "%s", scrubErr(err, backupTarget{}, "").Error()).
			WithContext("instance", inst.Name).
			WithContext("kind", string(kind))
		m.backupFailed(ctx, inst, kind, "", failure)
		return dbtemplate.BackupOutcome{}, failure
	}
	in := dbtemplate.BackupInput{
		Instance:            inst.Name,
		TemplateID:          inst.Template,
		TeamSlug:            inst.TeamSlug,
		PrjSlug:             inst.ProjectSlug,
		Kind:                kind,
		BindNodeID:          inst.PlatformNodeID,
		Repository:          target.Repository,
		Password:            password,
		ResticPassword:      target.ResticPass,
		S3AccessKeyID:       target.AccessKeyID,
		S3SecretKey:         target.SecretKey,
		S3Region:            target.Region,
		S3PathStyle:         target.PathStyle,
		AttachRustfsNetwork: target.AttachRustfs,
	}
	out, err := m.Backup(ctx, in)
	if err != nil {
		failure := apperr.New("E_DB_BACKUP_FAILED", "%s", scrubErr(err, target, password).Error()).
			WithContext("instance", inst.Name).
			WithContext("kind", string(kind))
		m.backupFailed(ctx, inst, kind, "", failure)
		return dbtemplate.BackupOutcome{}, failure
	}
	// 回读校验紧随备份（「备份假成功」零容忍——verified 才算绿；失败 =
	// 台账行 verify_status=failed + db.backup_failed，不冒充成功）。
	verifyErr := m.Verify(ctx, out)
	row := state.DatabaseBackup{
		DatabaseID:     inst.ID,
		Kind:           state.DatabaseBackupKind(kind),
		ResticSnapshot: out.SnapshotID,
		SizeBytes:      out.SizeBytes,
		VerifyStatus:   state.DatabaseVerifyVerified,
	}
	if verifyErr != nil {
		row.VerifyStatus = state.DatabaseVerifyFailed
		row.Error = singleLine(scrubText(verifyErr.Error(), target.ResticPass, target.AccessKeyID, target.SecretKey, password))
	}
	recorded, err := m.store.InsertDatabaseBackup(ctx, row)
	if err != nil {
		// 台账写失败：备份本体已成——诚实披露「结论未落账」，日志带快照 id
		// 供人工对账（repo 内寻址不依赖台账；事件面仍按 verify 结论落）。
		m.log.Error("database: backup succeeded but ledger write failed", "instance", inst.Name, "snapshot", out.SnapshotID, "error", err)
	}
	_ = recorded
	if verifyErr != nil {
		failure := apperr.New("E_DB_BACKUP_FAILED", "backup recorded but verification failed: %s", scrubText(verifyErr.Error(), target.ResticPass, target.AccessKeyID, target.SecretKey, password)).
			WithContext("instance", inst.Name).
			WithContext("kind", string(kind)).
			WithContext("snapshot", out.SnapshotID).
			WithContext("verify_status", string(state.DatabaseVerifyFailed))
		m.backupFailed(ctx, inst, kind, out.SnapshotID, failure)
		return out, failure
	}
	m.emitEvent(ctx, "db.backup_succeeded", "database:"+inst.Name, map[string]string{
		"instance": inst.Name,
		"kind":     string(kind),
		"snapshot": out.SnapshotID,
		"bytes":    strconv.FormatInt(out.SizeBytes, 10),
		"verify":   string(state.DatabaseVerifyVerified),
	})
	m.log.Info("database: backup verified and recorded", "instance", inst.Name,
		"kind", string(kind), "snapshot", out.SnapshotID, "bytes", out.SizeBytes)
	// 保留对齐（restic forget 路径过滤 + 台账行镜像；失败只告警——多留几
	// 份无害，下一备份自然重对齐）。
	m.pruneBackups(ctx, inst, out)
	return out, nil
}

// backupFailed 落备份失败面：db.backup_failed 事件（reason 经 scrub 兜底；
// 无台账行——失败无快照标识可记）。kind/snapshot 进 payload 供对账。
func (m *Manager) backupFailed(ctx context.Context, inst *state.DatabaseInstance, kind dbtemplate.BackupKind, snapshot string, err error) {
	payload := map[string]string{
		"instance": inst.Name,
		"kind":     string(kind),
		"reason":   singleLine(err.Error()),
	}
	if snapshot != "" {
		payload["snapshot"] = snapshot
	}
	m.emitEvent(ctx, "db.backup_failed", "database:"+inst.Name, payload)
}

// pruneBackups 保留对齐（备份成功尾部）：restic forget（本实例路径过滤）
// + 台账行镜像删除。keep ≤ 0 视为平台缺省。失败只告警不回滚——远端多留
// 几份无害，下一备份自然重对齐（statebackup forget 尾部同口径）。
func (m *Manager) pruneBackups(ctx context.Context, inst *state.DatabaseInstance, out dbtemplate.BackupOutcome) {
	keep := backupKeepOf(inst)
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		m.log.Warn("database: prune skipped (template lookup)", "instance", inst.Name, "error", err)
		return
	}
	script, err := pruneJobScript(tpl, out, keep)
	if err != nil {
		m.log.Warn("database: prune skipped (script build)", "instance", inst.Name, "error", err)
		return
	}
	net, nerr := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
	if nerr != nil {
		m.log.Warn("database: prune skipped (network name)", "instance", inst.Name, "error", nerr)
		return
	}
	if _, err := m.runToolsJob(ctx, toolsJobInput{
		instance: out.Instance,
		purpose:  "prune",
		script:   script,
		env:      toolsJobEnv("", out.Repository, out.ResticPassword, out.S3AccessKeyID, out.S3SecretKey, out.S3Region),
		networks: jobNetworks(out.AttachRustfsNetwork, net),
		timeout:  pruneJobTimeout,
		bindNode: out.BindNodeID,
		teamSlug: out.TeamSlug,
		prjSlug:  out.PrjSlug,
	}); err != nil {
		m.log.Warn("database: restic forget failed (retention not aligned; backups unaffected)", "instance", inst.Name, "error", err)
	}
	removed, lerr := m.store.PruneDatabaseBackups(ctx, inst.ID, keep)
	if lerr != nil {
		m.log.Warn("database: ledger prune failed (retention not aligned)", "instance", inst.Name, "error", lerr)
		return
	}
	if removed > 0 {
		m.log.Info("database: backup retention aligned", "instance", inst.Name, "kept", keep, "pruned", removed)
	}
}

// dutyBackupScheduling 是收敛拍尾部的调度 duty（§5.4 per 实例计划）：对每
// 个 ready|degraded 且计划启用的实例——now 落在 hour_utc 日窗小时内、且最
// 近一份 daily 备份早于 interval → 异步触发 kind=daily。失败不被窗口反复
// 重放：进程内记每次尝试时刻，间隔未到不重试（重启重记——窗口内至多多试
// 一次，红色事件诚实披露，无静默丢失）。
func (m *Manager) dutyBackupScheduling(ctx context.Context, rows []state.DatabaseInstance) {
	now := m.now().UTC()
	for i := range rows {
		inst := &rows[i]
		if inst.State != state.DatabaseReady && inst.State != state.DatabaseDegraded {
			continue
		}
		plan := planOf(inst)
		if now.Hour() != plan.hourUTC {
			continue
		}
		interval := time.Duration(plan.intervalHours) * time.Hour
		if last := m.dailyAttempt[inst.ID]; !last.IsZero() && now.Sub(last) < interval {
			continue // 本间隔内已尝试过（成功或失败）：不重放
		}
		fresh, err := m.lastDailyBackupFresh(ctx, inst.ID, plan.intervalHours, now)
		if err != nil {
			m.log.Warn("database: backup duty deferred (ledger read)", "instance", inst.Name, "error", err)
			continue
		}
		m.dailyAttempt[inst.ID] = now
		if fresh {
			continue // 台账已新：无需备份
		}
		if !m.beginOp(inst.ID, "backup") {
			continue // 操作互斥（备份/恢复/升级在途）：本窗让位，下窗重判
		}
		m.opsWG.Add(1)
		go func(inst state.DatabaseInstance) {
			defer m.opsWG.Done()
			defer m.endOp(inst.ID)
			bctx, cancel := context.WithTimeout(context.Background(), backupJobTimeout+verifyJobTimeout+pruneJobTimeout)
			defer cancel()
			if _, err := m.runBackup(bctx, &inst, dbtemplate.BackupDaily); err != nil {
				m.log.Warn("database: scheduled backup failed", "instance", inst.Name, "error", err.Error())
			}
		}(*inst)
	}
}

// lastDailyBackupFresh 报告最近一份 daily 备份是否仍在 interval 之内（台
// 账倒序首份 kind=daily；无行 = 不新鲜）。
func (m *Manager) lastDailyBackupFresh(ctx context.Context, dbID string, intervalHours int, now time.Time) (bool, error) {
	rows, err := m.store.ListDatabaseBackups(ctx, dbID, 50)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Kind != state.DatabaseBackupDaily {
			continue
		}
		return now.Sub(r.CreatedAt) < time.Duration(intervalHours)*time.Hour, nil
	}
	return false, nil
}

// backupPlan 是 per 实例备份计划的合成形态（零值字段回落平台缺省）。
type backupPlan struct {
	intervalHours int
	keep          int
	hourUTC       int
}

// planOf 合成实例的生效计划（§5.4 配置键 databases.backup_* 的常量缺省）。
func planOf(inst *state.DatabaseInstance) backupPlan {
	p := inst.Settings.Backup
	out := backupPlan{
		intervalHours: p.IntervalHours,
		keep:          p.Keep,
		hourUTC:       p.HourUTC,
	}
	if out.intervalHours <= 0 {
		out.intervalHours = DefaultBackupIntervalHours
	}
	if out.keep <= 0 {
		out.keep = DefaultBackupKeep
	}
	// hour_utc=0 是合法窗（00:00 UTC）——零值回落只认「未设置」的缺省位
	// （settings JSON omitempty 缺省化：0 与未设置同形，平台缺省窗 03:00
	// 显式覆盖；显式 0 窗以 interval/hour 组合表达——v0.2 设置面语义按
	// 「0 = 平台缺省」受理，S2 展示面同口径）。
	if out.hourUTC <= 0 {
		out.hourUTC = DefaultBackupHourUTC
	}
	return out
}

// backupKeepOf 取保留份数（零值回落平台缺省）。
func backupKeepOf(inst *state.DatabaseInstance) int {
	return planOf(inst).keep
}

// ── 明文兜底（statebackup scrubText 同构——错误文本进持久面前的擦除）──

// scrubText 把已知 secret 值从文本中擦除（兜底：工具/底座错误文本若意外
// 回显材料，事件/台账/日志面不落明文）。
func scrubText(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// scrubErr 是错误值的擦除形态（runBackup 失败面构造用——repo 口令/端点
// 凭证/引擎凭据全值集兜底）。
func scrubErr(err error, t backupTarget, enginePassword string) error {
	return errors.New(scrubText(err.Error(),
		t.ResticPass, t.AccessKeyID, t.SecretKey, enginePassword))
}
