package database

// Manager 与收敛主循环（managed-databases §2.1/§2.3 的 provisioner 落地）：
// tick 扫描拍驱动的按态收敛——provisioning 建现场过健康门、ready/degraded
// 观察健康、paused 保持 scale-0、deleting 幂等 reap。状态写唯一通道 =
// state.EnterDbPhase 单写点；收敛器自己的判定材料（选点、卷、secret、spec
// 幂等比对）全部可重入——任一拍中途失败，下一拍整体重走（与 rustfs/引擎
// 收敛同款「失败退避、幂等重试」节奏）。
//
// 事件驱动 kick：API 受理生命周期操作（create/resume/retry/suspend）后
// Kick() 立即触发一拍——受理到收敛的时延从 tick 周期收敛到毫秒级；kick
// 只是提前，不改变幂等语义（错过 kick 也由下一拍兜底）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fleetlyrun/fleetly/internal/componentloop"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台缺省参数（对齐 rustfs 管理器的退避/扫描节奏与库健康门的预算形态）。
const (
	// DefaultTickInterval 是收敛扫描拍周期（provisioning 受理→服务创建的
	// 无 kick 时延上界；ready 观察拍的抖动下界——swarm 健康位自身已是
	// 5s×3 的持续证据，10s 拍不引入额外迟钝）。
	DefaultTickInterval = 10 * time.Second
	// DefaultProvisionTimeout 是 provisioning 健康门总预算（首次创建含
	// 镜像分发：start_period 30s + 探测窗 15s + 分发余量；超时即判失败
	// ——保留现场、显式 retry）。任务硬失败（failed/rejected）不等预算
	// 立即判败。
	DefaultProvisionTimeout = 5 * time.Minute
)

// PlacementSelector 是收敛器对放置裁决的消费端口（实现方 = internal/
// placement.Resolver——接口在本包定义以便单测注入；api 定义端口同纪律：
// 核心不感知实现类型）。
type PlacementSelector interface {
	ResolveDatabase(ctx context.Context, in DatabaseInput) (DatabaseDecision, error)
}

// DatabaseInput/DatabaseDecision 是 placement 包类型的本包别名（消费面
// 读感统一；方向纪律：database → placement 单向）。
type (
	DatabaseInput    = placement.DatabaseInput
	DatabaseDecision = placement.DatabaseDecision
)

// Config 是收敛器参数（缺省回落平台默认）。
type Config struct {
	// TickInterval 是扫描拍周期（缺省 10s）。
	TickInterval time.Duration
	// ProvisionTimeout 是 provisioning 健康门总预算（缺省 5m）。
	ProvisionTimeout time.Duration
}

// Normalize 回落文档默认值。
func (c Config) Normalize() Config {
	if c.TickInterval <= 0 {
		c.TickInterval = DefaultTickInterval
	}
	if c.ProvisionTimeout <= 0 {
		c.ProvisionTimeout = DefaultProvisionTimeout
	}
	return c
}

// Manager 是库实例收敛管理器（rustfs Manager 同款装配形态：自建
// Docker 连接，cleanup 释放；零框架依赖——lynx 服务壳在 internal/runtime）。
type Manager struct {
	cfg       Config
	store     *state.Store
	box       *secrets.Box
	placement PlacementSelector
	docker    dockerPort
	log       *slog.Logger

	// mu 串行化 beat 与 Stop（kick 触发的即时拍与周期拍不并发——单写点
	// 纪律在收敛器侧的延伸：同一实例的收敛拍严格串行）。
	mu sync.Mutex

	// provisioningSince 记录实例进入 provisioning 的首见时刻（健康门超时
	// 判定的进程内计时锚；原因与失败结论落 last_error 持久列——计时器本
	// 身丢失只损失一次超时判定的起点，重启后重记，不产生错误转移）。
	provisioningSince map[string]time.Time

	// ── S5 操作面（备份/恢复/升级编排）──
	// opsMu 串行化 per 实例操作互斥哨兵的占用/释放（API 受理 goroutine 与
	// 收敛拍并发——与 mu 分立：mu 串行化收敛拍，opsMu 串行化哨兵位）。
	opsMu sync.Mutex
	// inflightOps 是 per 实例在途操作哨兵（instanceID → backup/restore/
	// upgrade——并发第二笔 409 的判据 + 收敛拍跳过实例的判据）。
	inflightOps map[string]string
	// opsWG 计数在途操作 goroutine（Stop 排水——操作链写 store，关停竞态
	// 与 statebackup inflight 同款治理）。
	opsWG sync.WaitGroup
	// dailyAttempt 记实例最近一次调度备份尝试时刻（进程内——失败不被日
	// 窗反复重放；重启重记至多多试一次）。
	dailyAttempt map[string]time.Time
	// upgradeAdvertised 记实例最近公告的目标 digest（进程内去重——重启重
	// 公告一次，诚实冗余优于静默）。
	upgradeAdvertised map[string]string
	// upgradeWatch 是升级健康门观察窗（平台缺省 upgradeWatchWindow；单测
	// 注入短窗——async 编排的失败路径不能等真实预算）。
	upgradeWatch time.Duration

	// now 是时钟出口（单测注入超时路径）。
	now func() time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	kick     chan struct{}
}

// NewManager 构造库实例收敛管理器（自建 Docker 连接；cleanup 释放——rustfs
// NewManager 同款形态）。
func NewManager(cfg Config, store *state.Store, box *secrets.Box, selector PlacementSelector, log *slog.Logger) (*Manager, func(), error) {
	dc, err := newRealDockerClient("")
	if err != nil {
		return nil, nil, err
	}
	m := NewManagerWithDocker(cfg, store, box, selector, dc, log)
	return m, func() { _ = dc.Close() }, nil
}

// NewManagerWithDocker 以注入的 dockerPort 构造（单测）。
func NewManagerWithDocker(cfg Config, store *state.Store, box *secrets.Box, selector PlacementSelector, dc dockerPort, log *slog.Logger) *Manager {
	return &Manager{
		cfg:               cfg.Normalize(),
		store:             store,
		box:               box,
		placement:         selector,
		docker:            dc,
		log:               log,
		provisioningSince: map[string]time.Time{},
		inflightOps:       map[string]string{},
		dailyAttempt:      map[string]time.Time{},
		upgradeAdvertised: map[string]string{},
		upgradeWatch:      upgradeWatchWindow,
		now:               time.Now,
		stop:              make(chan struct{}),
		kick:              make(chan struct{}, 1),
	}
}

// WithClock 注入时钟（单测）。
func (m *Manager) WithClock(now func() time.Time) *Manager { m.now = now; return m }

// Start 非阻塞启动收敛循环（启动即扫一拍——重启续跑；此后周期拍与 kick
// 拍并行驱动，拍本体在 mu 内串行）。
func (m *Manager) Start(ctx context.Context) error {
	m.done = make(chan struct{})
	go m.loop(ctx)
	return nil
}

// Stop 停止收敛循环并等待退出（在途一拍收口后返回）；随后带预算排空在途
// 操作（备份/恢复/升级 goroutine——各自 ctx 有界，预算只防失控卡死；超
// 预算残留由进程退出如实暴露，store 关闭次序归装配层）。
func (m *Manager) Stop(_ context.Context) error {
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.done
	drained := make(chan struct{})
	go func() { m.opsWG.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(opsStopDrainBudget):
		m.log.Warn("database: in-flight operations did not drain within budget (process exit will surface the residue)")
	}
	return nil
}

// opsStopDrainBudget 是 Stop 排空在途操作的等待预算（所有操作链 ctx 自带
// 预算——该值只兜底失控阻塞，不覆盖正常时长）。
const opsStopDrainBudget = 30 * time.Second

// Kick 请求立即一拍（非阻塞——已有待处理 kick 时合并；API 生命周期受理
// 后调用，收敛时延不等下一拍）。
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) loop(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(m.cfg.TickInterval)
	defer ticker.Stop()
	for {
		m.beat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-m.kick:
		case <-ticker.C:
		}
	}
}

// beat 是一个收敛拍：swarm 就绪前哨 → 全量实例按态分派。单实例失败不中
// 断其余实例（每拍重试各自消化），整体失败（swarm 未就绪/读库失败）只
// 日志退避。
func (m *Manager) beat(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, err := m.docker.Info(ctx)
	if err != nil {
		m.log.Warn("database: swarm probe failed (retrying)", "error", err)
		return
	}
	if !info.SwarmActive {
		m.log.Warn("database: docker engine is not an active swarm manager (retrying)")
		return
	}
	rows, err := m.store.ListDatabaseInstances(ctx)
	if err != nil {
		m.log.Warn("database: list instances failed (retrying)", "error", err)
		return
	}
	for i := range rows {
		m.convergeInstance(ctx, &rows[i])
	}
	// S5 拍尾步：备份调度（§5.4 per 实例计划）与可升级公告（操作事件
	// 面——读台账 + 事件追加，不触底座收敛）。
	m.scheduleBackups(ctx, rows)
	m.advertiseUpgrades(ctx, rows)
	m.gcProvisioningTimers(rows)
}

// convergeInstance 是按生命周期态的收敛分派（§2.3 操作表的单写点协作面
// ——每个分支只做本态的义务，转移全部经 EnterDbPhase）。
func (m *Manager) convergeInstance(ctx context.Context, inst *state.DatabaseInstance) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("database: converge panic (bug; instance left for next beat)",
				"instance", inst.Name, "panic", fmt.Sprint(r))
		}
	}()
	// 操作互斥：备份/恢复/升级在途的实例本拍让位（恢复的 scale-0 与升级
	// 的重建期，收敛器不得以副本水位/健康观察干扰在途编排——操作链自己
	// 维护期望形态，收口后下一拍恢复常规收敛）。
	if m.opBusy(inst.ID) {
		return
	}
	switch inst.State {
	case state.DatabaseProvisioning:
		m.convergeProvisioning(ctx, inst)
	case state.DatabaseReady, state.DatabaseDegraded:
		m.watchHealthy(ctx, inst)
	case state.DatabasePaused:
		m.convergePaused(ctx, inst)
	case state.DatabaseDeleting:
		m.reapDeleting(ctx, inst)
	case state.DatabaseFailed:
		// 保留现场（服务与卷不删，§2.1）——收敛器零动作，等显式 retry。
	case state.DatabaseDeleted:
		// 终态：无义务。
	default:
		m.log.Warn("database: unknown instance state (skipped)", "instance", inst.Name, "state", string(inst.State))
	}
}

// gcProvisioningTimers 清理已离开 provisioning 的计时锚（进程内 map 不随
// 状态表自动失效——每拍对账一次，量级 = 实例数，代价可忽略）。
func (m *Manager) gcProvisioningTimers(rows []state.DatabaseInstance) {
	inProvisioning := map[string]bool{}
	for _, r := range rows {
		if r.State == state.DatabaseProvisioning {
			inProvisioning[r.ID] = true
		}
	}
	for id := range m.provisioningSince {
		if !inProvisioning[id] {
			delete(m.provisioningSince, id)
		}
	}
}

// emitEvent 追加平台事件（rustfs 同款：Outbox 单写、失败只日志——事件披
// 露不阻断收敛）。payload 只带事实字段，凭据材料零出现。骨架唯一实现见
// internal/componentloop（本方法只绑 store/log/前缀）。
func (m *Manager) emitEvent(ctx context.Context, name, subject string, payload map[string]string) {
	componentloop.EmitEvent(ctx, m.store, m.log, "database", name, subject, payload)
}

// writeAudit 系统自动动作的审计（reap 等——「自动动作必入审计」纪律；
// fail-closed 由调用方决定是否阻断收敛路径）。
func (m *Manager) writeAudit(ctx context.Context, action, target string, diff string) error {
	return m.store.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      action,
			Target:      target,
			Result:      "ok",
			DiffSummary: diff,
		})
	})
}

// decryptCredential 解密实例引擎凭据（明文只存活于内存链；零日志/错误
// 文本拼接）。
func (m *Manager) decryptCredential(inst *state.DatabaseInstance) (string, error) {
	plain, err := m.box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		return "", fmt.Errorf("database: decrypt credential of %s: %w", inst.Name, err)
	}
	return string(plain), nil
}

// renderService 渲染实例的期望投影（副本数按态覆写：paused=0、其余=1；
// 模板渲染保持纯——暂停语义不入 dbtemplate）。镜像取实例行 digest 列而非
// 模板当前值——这是**升级载体**（§2.2）：升级 = 实例 digest 换新 → 渲染投
// 影随行 → desired-hash 差异驱动受控重建；模板镜像只在创建受理时作为初值
// 落列。digest 列空 = 编码错误（创建期强制非空），回落模板值保持渲染可
// 用（不影响既有哈希判据的正确性——列非空恒成立）。
func renderService(inst *state.DatabaseInstance, password string, replicas uint64) (engine.ServiceSpec, error) {
	spec, err := dbtemplate.Render(dbtemplate.RenderInput{
		Instance:   inst.Name,
		InstanceID: inst.ID,
		TeamSlug:   inst.TeamSlug,
		PrjSlug:    inst.ProjectSlug,
		TemplateID: inst.Template,
		Limits: dbtemplate.Limits{
			CPUSeconds:  inst.Settings.CPUSeconds,
			MemoryBytes: inst.Settings.MemoryBytes,
		},
		Credentials: dbtemplate.Credentials{Password: password},
	})
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	if inst.ImageDigest != "" {
		spec.Image = inst.ImageDigest
	}
	spec.Replicas = replicas
	return spec, nil
}

// errIsNotFound 报告错误是否「实例不存在」（Get 的哨兵归一——实例被并发
// 删除时收敛拍静默让位）。
func errIsNotFound(err error) bool { return errors.Is(err, state.ErrDatabaseNotFound) }
