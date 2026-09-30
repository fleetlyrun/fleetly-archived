package execrelay

// relay 部署收敛管理器（控制面收敛循环——internal/victorialogs Manager
// 同款形态）：terminal.enabled=true 时幂等部署/收敛 global 服务
// fleetly-exec（集群 token 的 Swarm secret 确保与哈希落 meta 同拍完成）；
// false 时移除服务（secret 与 meta 哈希保留——重启用同一 token 身份，免
// 全集群 relay 换证）。常驻收敛循环由服务壳承载；**无差分事件**——事件注
// 册表只增，本域仅 terminal.opened/closed（hub 发出），收敛只日志。
//
// 集群 token 供给（设计 §2.1 平台侧生成/轮换/分发的落地口径）：
// 首拍生成 48B crypto/rand → hex，创建 Swarm secret，sha256 哈希落 meta
//（认证比对真源——控制面永不持明文）；后续拍只验 secret 在位，缺失（swarm
// 状态丢失）才重生成换哈希并触发服务更新（relay 任务重启领新 secret）。
// **用户面轮换不做**（runbook 记运维路径：删 secret + 清 meta 哈希 → 收敛
// 循环下一拍重生成换证）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fleetlyrun/fleetly/internal/componentloop"
	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// Manager 是 relay 部署收敛管理器。
type Manager struct {
	store  *state.Store
	docker dockerPort
	log    *slog.Logger
	// enabled 是功能开关（terminal.enabled 静态配置——装配期注入）。
	enabled bool
	// httpPort 是控制面 HTTP 端口（FLEETLY_CONTROL_ADDR 的端口段——native
	// 端点所在面）。
	httpPort string
	// tlsName 是 relay 拨号校验名（ctrl.<base>，platform TLS 模式注入；
	// 空 = ws:// 明文——off/manual 的诚实降级）。
	tlsName string

	// retryInterval 是收敛失败的退避（零值回落常量——单测注入）。
	retryInterval time.Duration
	// advertiseOverride 覆盖 advertise 探测（单测；生产 = Info.NodeAddr）。
	advertiseOverride string
}

// NewManager 构造 relay 部署收敛管理器（共享 Docker 适配层自建连接；cleanup 释放）。
func NewManager(store *state.Store, enabled bool, httpPort, tlsName string, log *slog.Logger) (*Manager, func(), error) {
	dc, err := dockerapi.New("")
	if err != nil {
		return nil, nil, err
	}
	return &Manager{store: store, docker: dc, log: log, enabled: enabled, httpPort: httpPort, tlsName: tlsName},
		func() { _ = dc.Close() }, nil
}

// NewManagerWithDocker 以注入端口构造（单测）。
func NewManagerWithDocker(store *state.Store, enabled bool, httpPort, tlsName string, dc dockerPort, log *slog.Logger) *Manager {
	return &Manager{store: store, docker: dc, log: log, enabled: enabled, httpPort: httpPort, tlsName: tlsName}
}

// WithAdvertiseOverride 注入 advertise 探测覆盖（单测）。
func (m *Manager) WithAdvertiseOverride(addr string) *Manager {
	m.advertiseOverride = addr
	return m
}

// WithRetryInterval 注入收敛退避（单测）。
func (m *Manager) WithRetryInterval(d time.Duration) *Manager {
	m.retryInterval = d
	return m
}

// Run 是常驻收敛循环（victorialogs Run 同构；ctx 取消返回）：每拍 Ensure
// ——enabled=false 走移除清场（幂等）；失败退避重试，收敛后按扫描周期复
// 检漂移。
func (m *Manager) Run(ctx context.Context) error {
	retry := m.retryInterval
	if retry <= 0 {
		retry = retryIntervalDefault
	}
	scan := scanIntervalDefault
	converged := false
	for {
		err := m.Ensure(ctx)
		switch {
		case err != nil:
			converged = false
			m.log.Warn("execrelay: converge deferred (retrying)", "error", err, "retry_in", retry.String())
		case !converged && m.enabled:
			converged = true
			m.log.Info("execrelay: converged (managed exec relay deployed; global host-network task per node, "+
				"reverse-dialing the control plane)", "service", ExecRelayServiceName)
		case !converged && !m.enabled:
			converged = true
			m.log.Info("execrelay: quiesced (terminal.enabled=false; no managed deployment owed)")
		}
		if !componentloop.SleepCtx(ctx, componentloop.RetryOrScan(retry, scan, converged)) {
			return nil
		}
	}
}

// 收敛节奏（victorialogs 同款缺省）。
const (
	retryIntervalDefault = 30 * time.Second
	scanIntervalDefault  = 60 * time.Second
)

// Ensure 执行一拍收敛。可重试错误显式返回（收敛循环退避）。
func (m *Manager) Ensure(ctx context.Context) error {
	if !m.enabled {
		return m.removeIfPresent(ctx)
	}
	info, err := m.docker.Info(ctx)
	if err != nil {
		return err
	}
	if !info.SwarmActive {
		return ErrNotSwarmReady
	}
	advertise := m.advertiseOverride
	if advertise == "" {
		advertise = info.NodeAddr
	}
	if advertise == "" {
		return errors.New("execrelay: control plane advertise address is undetectable (set ingress.config_advertise_ip)")
	}
	secretID, err := m.ensureToken(ctx)
	if err != nil {
		return err
	}
	desired := buildSpec(fmtControlAddr(advertise, m.httpPort), m.tlsName, secretID)
	cur, err := m.docker.ServiceInspect(ctx, ExecRelayServiceName)
	if err != nil {
		return err
	}
	// 实况网络挂载目标（创建期 "host" 被归一为网络 ID）解析回名后同锚比对
	//（victorialogs/metrics 同款——解析失败显式退避重试，不误判漂移）。
	if cur.Exists {
		if err := componentloop.ResolveNetworkNames(ctx, cur.Networks, m.docker.NetworkName); err != nil {
			return err
		}
	}
	switch {
	case !cur.Exists:
		if err := m.docker.ServiceCreate(ctx, desired); err != nil {
			return err
		}
		m.log.Info("execrelay: service created", "service", ExecRelayServiceName,
			"image", DefaultExecRelayImage, "control_addr", fmtControlAddr(advertise, m.httpPort),
			"tls", tlsModeForLog(m.tlsName))
	case !specEqual(cur, desired):
		if err := m.docker.ServiceUpdate(ctx, ExecRelayServiceName, cur.Version, desired); err != nil {
			return err
		}
		m.log.Info("execrelay: service updated to desired spec (spec drift)", "service", ExecRelayServiceName)
	}
	return nil
}

// tlsModeForLog 是日志面的 TLS 形态（wss 按名校验 / ws 明文——诚实降级的
// 可观测锚）。
func tlsModeForLog(tlsName string) string {
	if tlsName == "" {
		return "ws (plaintext; control_plane.tls is off/manual)"
	}
	return "wss (ServerName " + tlsName + ")"
}

// ensureToken 确保集群 token 的 secret 与 meta 哈希一致（幂等）：
// meta 无哈希（首拍）→ 生成 + 建 secret + 落哈希；meta 有哈希但 secret 缺
// （swarm 状态丢失）→ 重生成换哈希（服务 spec 的 secret ID 变化触发更新，
// relay 任务重启领新证）。
func (m *Manager) ensureToken(ctx context.Context) (string, error) {
	hash, err := m.store.GetMeta(ctx, MetaKeyClusterTokenHash)
	if err != nil {
		return "", fmt.Errorf("execrelay: read cluster token hash: %w", err)
	}
	secretID, exists, err := m.docker.SecretInspect(ctx, ExecRelaySecretName)
	if err != nil {
		return "", err
	}
	if exists && hash != "" {
		return secretID, nil
	}
	if !exists && hash != "" {
		m.log.Warn("execrelay: cluster token secret missing from the swarm state (regenerating a new token; relay tasks restart with the new secret)")
	}
	token, err := newClusterToken()
	if err != nil {
		return "", err
	}
	id, err := m.docker.SecretEnsure(ctx, ExecRelaySecretName, []byte(token),
		map[string]string{state.LabelManaged: state.ManagedLabelValue, relayLabel: "true"})
	if err != nil {
		return "", err
	}
	if err := m.store.SetMeta(ctx, MetaKeyClusterTokenHash, state.HashToken(token)); err != nil {
		return "", fmt.Errorf("execrelay: store cluster token hash: %w", err)
	}
	m.log.Info("execrelay: cluster token provisioned (48B, stored as a swarm secret; the control plane keeps only its sha256 hash)")
	return id, nil
}

// newClusterToken 生成 48B crypto/rand 的 hex token（384 bit 熵——机器对
// 机器长寿命凭据的保守宽度）。
func newClusterToken() (string, error) {
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("execrelay: generate cluster token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// removeIfPresent 是 enabled=false 的清场（幂等）：服务在 → 移除。secret
// 与 meta 哈希保留（重启用同一 token 身份——免全集群 relay 换证；runbook
// 的运维轮换路径另行覆盖两者）。
func (m *Manager) removeIfPresent(ctx context.Context) error {
	cur, err := m.docker.ServiceInspect(ctx, ExecRelayServiceName)
	if err != nil {
		return err
	}
	if !cur.Exists {
		return nil
	}
	if err := m.docker.ServiceRemove(ctx, ExecRelayServiceName); err != nil {
		return err
	}
	m.log.Info("execrelay: service removed (terminal.enabled=false); the cluster token secret is retained for re-enabling",
		"service", ExecRelayServiceName)
	return nil
}

// CheckHealth 是 system status 组件检查器（execrelay）的部署面：功能关闭
// = 无所欠恒绿；开启时 relay 服务应在位——缺失即收敛未完成（收敛循环会
// 继续，红是过渡态的如实表达）。与 victorialogs 组件同口径。
func (m *Manager) CheckHealth() error {
	if !m.enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), componentloop.SettingsLoadTimeout)
	defer cancel()
	cur, err := m.docker.ServiceInspect(ctx, ExecRelayServiceName)
	if err != nil {
		return fmt.Errorf("execrelay: service inspect: %w", err)
	}
	if !cur.Exists {
		return fmt.Errorf("execrelay: terminal.enabled=true but service %s is not deployed yet (convergence in progress)", ExecRelayServiceName)
	}
	return nil
}

// DeploymentStatus 是 relay 部署态投影（GetTerminalStatus 消费）。
type DeploymentStatus struct {
	Exists bool
	Image  string
}

// DeploymentStatus 读取服务在位实况（只读面；底座不可达如实报错）。
func (m *Manager) DeploymentStatus(ctx context.Context) (DeploymentStatus, error) {
	cur, err := m.docker.ServiceInspect(ctx, ExecRelayServiceName)
	if err != nil {
		return DeploymentStatus{}, err
	}
	return DeploymentStatus{Exists: cur.Exists, Image: cur.Image}, nil
}
