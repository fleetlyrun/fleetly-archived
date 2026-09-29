// Package victorialogs 是托管 VictoriaLogs 的默认捆绑日志库（E6 观测专项
// 设计 §2/§3.1，W5-S1；V2-1 默认捆绑——logs.backend 缺省 victorialogs）。
//
// 两个职责面：
//
//  1. duty（本文件 + spec.go + docker.go，rustfs manager 同款形态）：设置
//     驱动——logs.backend=victorialogs 时幂等部署/收敛 Swarm 服务
//     fleetly-victorialogs（钉版镜像、卷钉 manager、内部网络、host-mode
//     回环发布 9428〔D-W5-4〕、-retentionPeriod 对齐 logs.retention_days）；
//     切回 jsonl 时移除服务**保留数据卷**（rustfs 禁用同型数据安全语义）。
//     常驻收敛循环（失败退避重试、收敛后按扫描周期复检漂移），差分事件
//     logs.victorialogs_deployed / logs.victorialogs_removed（注册表只增；
//     payload 不含任何敏感材料——VL 无凭据面）。
//
//  2. 查询后端（backend.go）：fleetlyd 侧 SearchLogs 的 VL 消费面——
//     LogsQL 查询构造（keyword 转义注入安全）、/select/logsql/query 查询
//     与 /health 健康拨测。
//
// 与 rustfs duty 的差异（设计裁决的落地）：无凭据（VL 无认证端点，隔离 =
// 回环绑定 + 内网）；无探针容器（宿主进程经回环直连 9428——D-W5-4 的
// host-mode 发布使宿主可达，不再需要入网容器）。
package victorialogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// settingsLoadTimeout 是 status/健康检查面的设置读取预算（CheckHealth
// 无 ctx 形态的自有预算，rustfs 同款）。
const settingsLoadTimeout = 3 * time.Second

// Manager 是托管 VictoriaLogs duty 管理器。
type Manager struct {
	store  *state.Store
	docker dockerPort
	log    *slog.Logger
	// retentionDays 是 -retentionPeriod 的对齐源（config 键
	// logs.retention_days；构造期注入，spec 渲染消费）。
	retentionDays int

	// retryInterval 是收敛失败的退避（零值回落 retryInterval 常量——
	// 单测注入短退避驱动重试断言）。
	retryInterval time.Duration
	// health 是回环健康拨测端口（CheckHealth 的可达性面；单测注入。
	// nil = 不拨测——部署在位即视为健康，单测/精简装配形态）。
	health func(ctx context.Context) error
}

// NewManager 构造 duty 管理器（共享 Docker 适配层自建连接；cleanup 释放）。
func NewManager(store *state.Store, retentionDays int, log *slog.Logger) (*Manager, func(), error) {
	dc, err := dutydocker.New("")
	if err != nil {
		return nil, nil, err
	}
	return NewManagerWithDocker(store, retentionDays, dc, log), func() { _ = dc.Close() }, nil
}

// NewManagerWithDocker 以注入的 dockerPort 构造（单测）。
func NewManagerWithDocker(store *state.Store, retentionDays int, dc dockerPort, log *slog.Logger) *Manager {
	return &Manager{store: store, retentionDays: retentionDays, docker: dc, log: log}
}

// WithHealth 注入回环健康拨测端口（生产装配 = Backend.Ping；nil = 不拨测）。
func (m *Manager) WithHealth(h func(ctx context.Context) error) *Manager {
	m.health = h
	return m
}

// Run 是常驻收敛循环（rustfs Run 同构；fleetlyd 装配壳调用；ctx 取消返回）：
// 每拍 LoadLogsSettings 现读（运行期设置不缓存长驻）——backend=victorialogs
// 走部署收敛，其他值走移除清场（幂等，收敛即稳态）；失败退避重试，收敛后
// 按扫描周期复检漂移。
func (m *Manager) Run(ctx context.Context) error {
	retry := m.retryInterval
	if retry <= 0 {
		retry = retryInterval
	}
	scan := scanInterval
	converged := false
	for {
		oc, err := m.Ensure(ctx)
		switch {
		case err != nil:
			converged = false
			m.log.Warn("victorialogs: converge deferred (retrying)", "error", err, "retry_in", retry.String())
		case oc == outcomeDeployed && !converged:
			converged = true
			m.log.Info("victorialogs: converged (managed VictoriaLogs deployed; loopback-only host publish, "+
				"the data volume persists across backend switch)", "service", ServiceName, "volume", VolumeName)
		case oc == outcomeIdle && !converged:
			converged = true
			m.log.Info("victorialogs: quiesced (logs.backend != victorialogs; no managed deployment owed)")
		}
		if !sleepCtx(ctx, retryOrScan(retry, scan, converged)) {
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
	in, err := m.store.LoadLogsSettings(ctx)
	if err != nil {
		return outcomeIdle, fmt.Errorf("victorialogs: load logs settings: %w", err)
	}
	if in.Backend != state.LogsBackendVictorialogs {
		return outcomeIdle, m.removeIfPresent(ctx)
	}
	return outcomeDeployed, m.converge(ctx)
}

// converge 部署/漂移收敛（设计 §2.1；rustfs converge 同构，无凭据面）：
// 数据卷 → 期望 spec（钉 manager + 限额 + host 网络回环监听 + retention
// 参数）→ inspect 缺失创建/漂移更新。
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
		return fmt.Errorf("victorialogs: read platform node id: %w", err)
	}
	if platformID == "" {
		return errors.New("victorialogs: platform node id not ensured yet (identity duty pending; the pin constraint requires it)")
	}
	// ① 数据卷（本地命名卷——数据重力钉 manager）。
	if err := m.docker.VolumeEnsure(ctx, VolumeName); err != nil {
		return err
	}
	// ② 期望 spec → 幂等收敛。
	desired := buildSpec(platformID, m.retentionDays)
	cur, err := m.docker.ServiceInspect(ctx, ServiceName)
	if err != nil {
		return err
	}
	// 实况网络挂载目标（创建期被 engine 归一为网络 ID——"host" 亦然）解析
	// 回名后同锚比对；解析失败显式退避重试，不误判漂移（W5-S3 门上移植自
	// internal/metrics——此前每拍 ID≠名恒判漂移：ServiceUpdate 空转 + deployed
	// 事件每拍重发）。
	for i, t := range cur.Networks {
		n, err := m.docker.NetworkName(ctx, t)
		if err != nil {
			return err
		}
		cur.Networks[i] = n
	}
	switch {
	case !cur.Exists:
		if err := m.docker.ServiceCreate(ctx, desired); err != nil {
			return err
		}
		m.log.Info("victorialogs: service created",
			"service", ServiceName, "image", DefaultVictoriaLogsImage,
			"listen", listenArg(), "constraint", constraintFor(platformID))
		m.emitEvent(ctx, "logs.victorialogs_deployed", "platform:logs", map[string]string{
			"service": ServiceName, "image": DefaultVictoriaLogsImage, "reason": "created",
		})
	case !specEqual(cur, desired):
		if err := m.docker.ServiceUpdate(ctx, ServiceName, cur.Version, desired); err != nil {
			return err
		}
		m.log.Info("victorialogs: service updated to desired spec (spec drift)",
			"service", ServiceName)
		m.emitEvent(ctx, "logs.victorialogs_deployed", "platform:logs", map[string]string{
			"service": ServiceName, "image": DefaultVictoriaLogsImage, "reason": "updated",
		})
	}
	return nil
}

// removeIfPresent 是 backend 离开 victorialogs 的清场（幂等）：服务在 →
// 移除 + 事件 logs.victorialogs_removed。数据卷**永不删除**（rustfs 禁用
// 同型数据安全语义——切回 victorialogs 复用卷，历史检索面延续）；网络保留
//（零成本，为后续受管消费方留位）。
func (m *Manager) removeIfPresent(ctx context.Context) error {
	cur, err := m.docker.ServiceInspect(ctx, ServiceName)
	if err != nil {
		return err
	}
	if !cur.Exists {
		return nil
	}
	if err := m.docker.ServiceRemove(ctx, ServiceName); err != nil {
		return err
	}
	m.log.Info("victorialogs: service removed (logs.backend left victorialogs); "+
		"data volume retained — data survives; switch back to reattach it",
		"service", ServiceName, "volume", VolumeName)
	m.emitEvent(ctx, "logs.victorialogs_removed", "platform:logs", map[string]string{
		"service": ServiceName, "volume_retained": "true",
	})
	return nil
}

// emitEvent 追加平台事件（Outbox 单写；失败只日志——事件披露不阻断收敛）。
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
		m.log.Warn("victorialogs: event append failed", "event", name, "error", err)
	}
}

// DeploymentStatus 是 backend 视图面（CLI logs backend show / RPC 投影）的
// 部署态投影。
type DeploymentStatus struct {
	// Exists 报告服务是否在位（backend=victorialogs 且 Exists=false =
	// 部署中/未部署——duty 退避收敛中）。
	Exists bool
	// Image 是实况镜像引用（不在位为空）。
	Image string
}

// DeploymentStatus 读取服务在位实况（只读面；底座不可达如实报错）。
func (m *Manager) DeploymentStatus(ctx context.Context) (DeploymentStatus, error) {
	cur, err := m.docker.ServiceInspect(ctx, ServiceName)
	if err != nil {
		return DeploymentStatus{}, err
	}
	return DeploymentStatus{Exists: cur.Exists, Image: cur.Image}, nil
}

// CheckHealth 是 system status 组件检查器（victorialogs）的部署面：mode
// 非 victorialogs = 无所欠（健康）；victorialogs 模式下服务应在位——缺失
// 即收敛未完成（duty 会继续收敛，红是过渡态的如实表达）。ingest streak
// 面由 internal/logs 批量器承载，装配点组合（设计：healthy = 部署符合
// 预期且 streak 无降级）。健康检查是热路径：拨测预算 2s，不可达即红
//（检索降级，直播面不受影响——诚实口径）。
func (m *Manager) CheckHealth() error {
	ctx, cancel := context.WithTimeout(context.Background(), settingsLoadTimeout)
	defer cancel()
	in, err := m.store.LoadLogsSettings(ctx)
	if err != nil {
		return fmt.Errorf("victorialogs: load logs settings: %w", err)
	}
	if in.Backend != state.LogsBackendVictorialogs {
		return nil
	}
	cur, err := m.docker.ServiceInspect(ctx, ServiceName)
	if err != nil {
		return fmt.Errorf("victorialogs: service inspect: %w", err)
	}
	if !cur.Exists {
		return fmt.Errorf("victorialogs: logs.backend=victorialogs but service %s is not deployed yet (duty converging)", ServiceName)
	}
	if m.health != nil {
		hctx, hcancel := context.WithTimeout(ctx, 2*time.Second)
		defer hcancel()
		if err := m.health(hctx); err != nil {
			return fmt.Errorf("victorialogs: health probe failed (search degraded; live tail unaffected): %w", err)
		}
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

// retryOrScan 收敛失败/未收敛走短退避，已收敛走扫描周期（漂移复检节奏）。
func retryOrScan(retry, scan time.Duration, converged bool) time.Duration {
	if converged && scan > 0 {
		return scan
	}
	return retry
}
