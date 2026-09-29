// Package rustfs 是托管 RustFS 的 opt-in 管理组件（E3-5，设计 §2.5，
// D-S3-7/V2-2 用户直裁）：s3.mode=rustfs 时幂等部署/收敛 Swarm 服务
// fleetly-rustfs（zot 部署器同款纪律——钉版镜像、卷钉 manager、内部
// overlay 网络零 host 端口、凭据经 Swarm secret 注入、服务就绪后
// EnsureBucket）；mode 离开 rustfs 时移除服务**保留数据卷**（volume.
// detached 同型数据安全语义）+ 凭据清场（凭据是运行时配置不烙进数据，
// 再启用重新生成并复用卷）。
//
// duty 形态：常驻收敛循环（ingress registry duty 同款——失败退避重试、
// 收敛后按扫描周期复检漂移），由 fleetlyd 装配壳启动（runtime 服务壳，
// 资源层，晚于 engine 停）。差分事件：s3.rustfs_deployed / s3.rustfs_removed
// （注册表只增，node.* 同型；payload 不含任何凭据材料）。
//
// 诚实口径（D-S3-8 裁决核心，Console/CLI/文档三面常驻）：本机 RustFS =
// 便捷层（防误删/防单文件损坏），**非灾备**——主机整体损毁时该备份随
// 主机一同丢失；灾备请配置外部端点。
//
// 明文纪律（state-model §2.9）：凭据明文只在内存与 swarm secret 创建载荷
// 存活；持久层 envelope 密文（internal/state.rustfscredentials）；日志只落
// 指纹（sha256 前 8 hex）；错误文本零拼接材料。
package rustfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/statebackup"
)

// s3SettingsLoadTimeout 是 status/健康检查面的设置读取预算（CheckHealth
// 无 ctx 形态的自有预算）。
const s3SettingsLoadTimeout = 3 * time.Second

// Manager 是托管 RustFS duty 管理器。
type Manager struct {
	store  *state.Store
	box    *secrets.Box
	docker dockerPort
	log    *slog.Logger

	// retryInterval 是收敛失败的退避（零值回落 retryInterval 常量——
	// 单测注入短退避驱动重试断言）。
	retryInterval time.Duration
	// probeRunner 是探针/建桶容器执行端口（生产 = *substrate.Client 经
	// statebackup.ResticRunner 端口隐式实现；nil 时由构造方必接——建桶与
	// 探针都经一次性容器入内网，宿主进程不可达 overlay）。
	probeRunner statebackup.ResticRunner
	// ensureBucketFn 是建桶步骤的注入缝（单测；nil = 真实实现——探针容器
	// 的 init 步创建桶）。
	ensureBucketFn func(ctx context.Context) error
}

// NewManager 构造 duty 管理器（自建 Docker 连接；cleanup 释放）。
func NewManager(store *state.Store, box *secrets.Box, log *slog.Logger) (*Manager, func(), error) {
	dc, err := dutydocker.New("")
	if err != nil {
		return nil, nil, err
	}
	return NewManagerWithDocker(store, box, dc, log), func() { _ = dc.Close() }, nil
}

// NewManagerWithDocker 以注入的 dockerPort 构造（单测）。
func NewManagerWithDocker(store *state.Store, box *secrets.Box, dc dockerPort, log *slog.Logger) *Manager {
	return &Manager{store: store, box: box, docker: dc, log: log}
}

// WithProbeRunner 注入探针/建桶容器执行器（单测；生产装配 =
// statebackup.ResticRunner 端口的 substrate 实现）。
func (m *Manager) WithProbeRunner(r statebackup.ResticRunner) *Manager { m.probeRunner = r; return m }

// Run 是常驻收敛循环（fleetlyd 装配壳调用；ctx 取消返回）：
// 每拍 LoadS3Settings 现读（运行期设置不缓存长驻）——mode=rustfs 走部署
// 收敛，其他值走移除清场（幂等，收敛即稳态）；失败退避重试，收敛后按
// 扫描周期复检漂移（ingress registry duty 同款节奏）。
func (m *Manager) Run(ctx context.Context) error {
	retry := m.retryInterval
	if retry <= 0 {
		retry = retryInterval
	}
	converged := false
	for {
		oc, err := m.Ensure(ctx)
		switch {
		case err != nil:
			converged = false
			m.log.Warn("rustfs: converge deferred (retrying)", "error", err, "retry_in", retry.String())
		case oc == outcomeDeployed && !converged:
			converged = true
			m.log.Info("rustfs: converged (managed RustFS deployed; internal network only, no host ports; "+
				"the data volume persists across disable)", "service", ServiceName, "volume", VolumeName)
		case oc == outcomeIdle && !converged:
			converged = true
			m.log.Info("rustfs: quiesced (s3.mode != rustfs; no managed deployment owed)")
		}
		if !sleepCtx(ctx, retryOrScan(retry, scanInterval, converged)) {
			return nil
		}
	}
}

// outcome 是一拍收敛的结论（部署在位 / 非托管稳态）。
type outcome int

const (
	outcomeDeployed outcome = iota
	outcomeIdle
)

// Ensure 执行一拍收敛。返回当前应许态结论与可重试错误。
func (m *Manager) Ensure(ctx context.Context) (outcome, error) {
	in, err := m.store.LoadS3Settings(ctx)
	if err != nil {
		return outcomeIdle, fmt.Errorf("rustfs: load s3 settings: %w", err)
	}
	if state.NormalizeMode(in.Mode) != state.S3ModeRustfs {
		return outcomeIdle, m.removeIfPresent(ctx)
	}
	return outcomeDeployed, m.converge(ctx)
}

// converge 部署/漂移收敛（设计 §2.5；ingress EnsureRegistry 同构）：
// 凭据（惰性生成，envelope 落库）→ 数据卷/内部网络（attachable——上传轨
// 一次性容器经它入网）→ 凭据 swarm secret（名内嵌指纹，缺失即建）→
// 期望 spec（钉 manager + 限额 + alias）→ inspect 缺失创建/漂移更新 →
// 旧 secret 清场 → 服务就绪后 EnsureBucket（幂等；桶在 = 全收敛）。
func (m *Manager) converge(ctx context.Context) error {
	info, err := m.docker.Info(ctx)
	if err != nil {
		return err
	}
	if !info.SwarmActive {
		return ErrNotSwarmReady
	}
	// manager 平台 ID（meta 单值真源；identity duty 尚未铸造时显式失败
	// 退避重试——约束引用空 ID 会得到永不调度的任务，宁缺毋错）。
	platformID, err := m.store.GetMeta(ctx, state.MetaKeyPlatformNodeID)
	if err != nil {
		return fmt.Errorf("rustfs: read platform node id: %w", err)
	}
	if platformID == "" {
		return errors.New("rustfs: platform node id not ensured yet (identity duty pending; the pin constraint requires it)")
	}
	creds, err := m.ensureCredentials(ctx)
	if err != nil {
		return err
	}
	// ① 数据卷（本地命名卷——数据重力钉 manager；swarm 对 task 卷挂载亦有
	// 按节点创建语义，显式收敛让部署器自证前置物存在）。
	if err := m.docker.VolumeEnsure(ctx, VolumeName); err != nil {
		return err
	}
	// ② 内部 overlay（attachable；不发布任何 host 端口）。
	if err := m.docker.NetworkEnsure(ctx, state.RustfsNetworkName, true); err != nil {
		return err
	}
	netID, err := m.docker.NetworkID(ctx, state.RustfsNetworkName)
	if err != nil {
		return err
	}
	// ③ 凭据 secret（幂等创建；明文只进创建载荷；引用需底座对象 ID）。
	accessID, err := m.ensureSecret(ctx, accessSecretName(creds), []byte(creds.AccessKey))
	if err != nil {
		return err
	}
	secretID, err := m.ensureSecret(ctx, secretSecretName(creds), []byte(creds.SecretKey))
	if err != nil {
		return err
	}
	refs := []secretRef{
		{Name: accessSecretName(creds), ID: accessID, Target: accessKeyFileTarget},
		{Name: secretSecretName(creds), ID: secretID, Target: secretKeyFileTarget},
	}
	// ④ 期望 spec → 幂等收敛。
	desired := buildSpec(netID, platformID, creds, refs)
	cur, err := m.docker.ServiceInspect(ctx, ServiceName)
	if err != nil {
		return err
	}
	switch {
	case !cur.Exists:
		if err := m.docker.ServiceCreate(ctx, desired); err != nil {
			return err
		}
		m.log.Info("rustfs: service created",
			"service", ServiceName, "image", DefaultRustFSImage,
			"network", state.RustfsNetworkName, "constraint", constraintFor(platformID))
		m.emitEvent(ctx, "s3.rustfs_deployed", "platform:rustfs", map[string]string{
			"service": ServiceName, "image": DefaultRustFSImage, "reason": "created",
			"network": state.RustfsNetworkName, "bucket": state.RustfsBucketName,
		})
	case !specEqual(cur, desired):
		if err := m.docker.ServiceUpdate(ctx, ServiceName, cur.Version, desired); err != nil {
			return err
		}
		m.log.Info("rustfs: service updated to desired spec (spec drift or credential rotation)",
			"service", ServiceName)
		m.emitEvent(ctx, "s3.rustfs_deployed", "platform:rustfs", map[string]string{
			"service": ServiceName, "image": DefaultRustFSImage, "reason": "updated",
		})
	}
	// ⑤ 旧 secret 清场（凭据轮换/再生成后，不再被引用的凭据 secret 移除
	// ——凭据材料不残留；in-use 竞态由下一拍重试消化）。
	if err := m.removeStaleSecrets(ctx, accessSecretName(creds), secretSecretName(creds)); err != nil {
		return err
	}
	// ⑥ 服务就绪后 EnsureBucket（设计 §2.5；幂等——桶/探针仓库在即 no-op；
	// 宿主进程不可达 overlay，建桶经探针容器入网执行；服务未 running 时
	// restic 如实报错，duty 退避重试）。
	if m.ensureBucketFn != nil {
		return m.ensureBucketFn(ctx)
	}
	return m.EnsureBucketViaProbe(ctx)
}

// removeIfPresent 是 mode 离开 rustfs 的清场（幂等；分步收敛——服务移除、
// secret 清场、凭据键删除各步失败都由下一拍重试）：
//  1. 服务在 → 移除 + 事件 s3.rustfs_removed + 日志明示卷留存；
//  2. 凭据 swarm secret 移除（凭据材料不残留；服务刚删的引用释放延迟
//     以 in-use 错误退避重试消化）；
//  3. 库内凭据密文键删除（运行时配置，再启用重新生成——卷数据与凭据
//     零绑定，官方口径：容器以新凭据重建、数据卷保持完好）。
//
// 数据卷**永不删除**（数据安全语义与 volume.detached 一致）；网络保留
// （零成本，且已部署应用可能仍挂接直到下次重部署）。
func (m *Manager) removeIfPresent(ctx context.Context) error {
	cur, err := m.docker.ServiceInspect(ctx, ServiceName)
	if err != nil {
		return err
	}
	if cur.Exists {
		if err := m.docker.ServiceRemove(ctx, ServiceName); err != nil {
			return err
		}
		m.log.Info("rustfs: service removed (s3.mode left rustfs); "+
			"data volume retained — data survives; delete the volume explicitly to discard",
			"service", ServiceName, "volume", VolumeName)
		m.emitEvent(ctx, "s3.rustfs_removed", "platform:rustfs", map[string]string{
			"service": ServiceName, "volume_retained": "true",
		})
		// 本拍到此为止：secret 清场等下一拍（服务删除到引用释放有传播
		// 延迟，立即删 secret 多半 in-use——退避重试是既定节奏）。
		return nil
	}
	if err := m.removeStaleSecrets(ctx); err != nil {
		return err
	}
	if err := m.store.DeleteRustfsCredentialsCiphertext(ctx); err != nil {
		return err
	}
	return nil
}

// ensureCredentials 取得托管凭据（惰性生成；resticPassword 同型）：
// 已存 → 解密复用；未存 → crypto/rand 生成 → envelope 加密落库 → 日志
// 记指纹。**不写审计、不发事件**（§2.5 裁决：平台内部凭据生命周期）。
func (m *Manager) ensureCredentials(ctx context.Context) (credentials, error) {
	accessCT, secretCT, found, err := m.store.LoadRustfsCredentialsCiphertext(ctx)
	if err != nil {
		return credentials{}, fmt.Errorf("rustfs: load credentials: %w", err)
	}
	if found {
		plain, err := m.box.Decrypt([]byte(accessCT))
		if err != nil {
			return credentials{}, fmt.Errorf("rustfs: decrypt access key: %w", err)
		}
		access := string(plain)
		plain, err = m.box.Decrypt([]byte(secretCT))
		if err != nil {
			return credentials{}, fmt.Errorf("rustfs: decrypt secret key: %w", err)
		}
		return credentials{AccessKey: access, SecretKey: string(plain)}, nil
	}
	c, err := generateCredentials()
	if err != nil {
		return credentials{}, err
	}
	accessCTBytes, err := m.box.Encrypt([]byte(c.AccessKey))
	if err != nil {
		return credentials{}, fmt.Errorf("rustfs: encrypt access key: %w", err)
	}
	secretCTBytes, err := m.box.Encrypt([]byte(c.SecretKey))
	if err != nil {
		return credentials{}, fmt.Errorf("rustfs: encrypt secret key: %w", err)
	}
	if err := m.store.SaveRustfsCredentialsCiphertext(ctx, string(accessCTBytes), string(secretCTBytes)); err != nil {
		return credentials{}, err
	}
	m.log.Info("rustfs: root credentials generated (stored encrypted; "+
		"fingerprint for operator cross-check only; regenerated on re-enable — credentials are runtime config, not baked into the data volume)",
		"access_fingerprint", fingerprint(c.AccessKey), "secret_fingerprint", fingerprint(c.SecretKey))
	return c, nil
}

// loadCredentials 只读取数（探针面——不生成；未备便如实报错）。
func (m *Manager) loadCredentials(ctx context.Context) (credentials, error) {
	accessCT, secretCT, found, err := m.store.LoadRustfsCredentialsCiphertext(ctx)
	if err != nil {
		return credentials{}, fmt.Errorf("rustfs: load credentials: %w", err)
	}
	if !found {
		return credentials{}, errors.New(
			"rustfs: managed credentials not provisioned yet (the duty generates them shortly after s3.mode=rustfs is saved)")
	}
	plain, err := m.box.Decrypt([]byte(accessCT))
	if err != nil {
		return credentials{}, fmt.Errorf("rustfs: decrypt access key: %w", err)
	}
	access := string(plain)
	plain, err = m.box.Decrypt([]byte(secretCT))
	if err != nil {
		return credentials{}, fmt.Errorf("rustfs: decrypt secret key: %w", err)
	}
	return credentials{AccessKey: access, SecretKey: string(plain)}, nil
}

// ensureSecret 幂等创建 swarm secret（名内嵌凭据指纹 → 内容变化即新名，
// 「存在性」判据即幂等；返回底座对象 ID——服务 spec 引用需要；明文只进
// 创建载荷，绝不进日志/错误）。
func (m *Manager) ensureSecret(ctx context.Context, name string, data []byte) (string, error) {
	id, exists, err := m.docker.SecretInspect(ctx, name)
	if err != nil {
		return "", err
	}
	if exists {
		return id, nil
	}
	return m.docker.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: name,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				rustfsLabel:        "true",
			},
		},
		Data: data,
	})
}

// removeStaleSecrets 清场本组件的全部凭据 secret（label 选择），保留
// keep 集合（当前期望引用）。幂等；in-use（服务引用未释放）如实报错由
// duty 退避重试。
func (m *Manager) removeStaleSecrets(ctx context.Context, keep ...string) error {
	names, err := m.docker.SecretList(ctx, map[string]string{rustfsLabel: "true"})
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, name := range names {
		if kept[name] {
			continue
		}
		if err := m.docker.SecretRemove(ctx, name); err != nil {
			return err
		}
		m.log.Info("rustfs: stale credential secret removed", "secret", name)
	}
	return nil
}

// emitEvent 追加平台事件（Outbox 单写；失败只日志——事件披露不阻断收敛）。
// payload 只带服务/镜像/原因等非敏感形态，凭据材料零出现。
func (m *Manager) emitEvent(ctx context.Context, name, subject string, payload map[string]string) {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	err = m.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.AppendEvent(ctx, state.Event{Name: name, Subject: subject, Payload: string(raw)})
		return err
	})
	if err != nil {
		m.log.Warn("rustfs: event append failed", "event", name, "error", err)
	}
}

// CheckHealth 是 system status 组件检查器（objectstore.rustfs）：mode 非
// rustfs = 无所欠（健康）；rustfs 模式下服务应在位且凭据应已备便——缺失
// 即收敛未完成（duty 会继续收敛，红是过渡态的如实表达）。桶与可达性面由
// TestConnection 探针（容器内执行）承载（本检查不做网络往返——健康检查
// 是热路径）。
func (m *Manager) CheckHealth() error {
	ctx, cancel := context.WithTimeout(context.Background(), s3SettingsLoadTimeout)
	defer cancel()
	in, err := m.store.LoadS3Settings(ctx)
	if err != nil {
		return fmt.Errorf("rustfs: load s3 settings: %w", err)
	}
	if state.NormalizeMode(in.Mode) != state.S3ModeRustfs {
		return nil
	}
	cur, err := m.docker.ServiceInspect(ctx, ServiceName)
	if err != nil {
		return fmt.Errorf("rustfs: service inspect: %w", err)
	}
	if !cur.Exists {
		return fmt.Errorf("rustfs: s3.mode=rustfs but service %s is not deployed yet (duty converging)", ServiceName)
	}
	if _, _, found, err := m.store.LoadRustfsCredentialsCiphertext(ctx); err != nil || !found {
		return fmt.Errorf("rustfs: managed credentials not provisioned yet (duty converging)")
	}
	return nil
}

// sleepCtx 睡眠直到 d 到期或 ctx 取消（返回 false = ctx 已取消）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// retryOrScan 收敛失败/未收敛走短退避，已收敛走扫描周期（漂移复检节奏；
// ingress 同款公式）。
func retryOrScan(retry, scan time.Duration, converged bool) time.Duration {
	if converged && scan > 0 {
		return scan
	}
	return retry
}
