package engine

// 发布引擎主链路（T2.10/T2.11）：队列拾取（同 app 互斥）→ preparing
// （compose 重载/放置/env/secret）→ building（构建直通核对）→ releasing
// （Swarm 对账 + 健康门 + L2 看门狗）→ observing（L3 观察窗）→ succeeded，
// 失败分流/取消/重启恢复见 recovery.go 与 observing.go。单 goroutine tick
// 推进（行谓词 CAS 保证与 CLI 的并发安全）；状态全部落库——控制面随时
// 重启可续跑。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/envlayer"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// Engine 是发布引擎。依赖里的 *secrets.Box 承担两件事：平台 env 密文解密
// （合并输入）与期望态快照加密（desired_spec——归位/重放的执行依据，明文
// 纪律与 env_vars 相同：只以密文落库）。
type Engine struct {
	cfg      Config
	store    *state.Store
	sub      Substrate
	images   ImageChecker
	resolver PlacementResolver
	box      *secrets.Box
	clock    Clock
	log      *slog.Logger
	// routes 是入口路由发布端口（T2.15；nil = 未接入口面——发布挂点
	// 整体跳过。由 WithRoutePublisher 注入，fleetlyd 装配 ingress.Manager）。
	routes RoutePublisher
	// postDeploy 是部署成功后的备份挂钩（T2.22：每次部署成功后触发一次
	// 热备快照，异步不阻塞部署主链；nil = 未接备份面。由 WithPostDeployHook
	// 注入，fleetlyd 装配 statebackup.Manager）。
	postDeploy PostDeployHook
	// envChanged 是 env 变更生效后的联动挂钩（H9：观察窗成功的 pending env
	// 提升点触发日志脱敏值集失效；nil = 未接。engine 不 import logs——经
	// fleetlyd 装配层注入回调，挂点模式同 postDeploy）。
	envChanged EnvChangedHook
	// dbTemplate 是库模板连接信息端口（E4 managed-databases；nil = 未接线
	// ——带 fleetly.databases label 的部署规划期显式失败）。dbtemplate 在
	// engine 之上（渲染投影消费 engine.ServiceSpec），只能端口注入不能反向
	// import——适配器在装配层落（WithDatabaseTemplate）。
	dbTemplate DatabaseTemplatePort
	// secretEnsure 是 Swarm secret 对象的确保端口（E4 managed-databases
	// §2.7 compose secrets 注入链；nil = 未接线——带 secret 声明的部署
	// 规划期显式失败）。实现 = substrate.Client（WithSecretEnsurer）。
	secretEnsure SecretEnsurer
	// secretReap 是 Swarm secret 对象的清场端口（app 删除 reap 的扫尾面，
	// nil = 未接线——reap 扫尾如实跳过告警）。实现 = substrate.Client
	//（WithSecretReaper）。
	secretReap SecretReaper
	// configEnsure 是 Swarm config 对象的确保端口（T 线 OT-3/IMPL-T1-4
	// compose configs 注入链；nil = 未接线——带 config 声明的部署规划期显式
	// 失败）。实现 = substrate.Client（WithConfigEnsurer）。
	configEnsure ConfigEnsurer
	// configReap 是 Swarm config 对象的清场端口（换版旧对象 GC 与 app 删除
	// reap 的扫尾面；nil = 未接线——清场如实跳过告警）。实现 =
	// substrate.Client（WithConfigReaper）。
	configReap ConfigReaper
	// waterMarks 是副本水位不足判定的进程内计时（观察窗辅助信号；引擎
	// 重启后重摆——窗口本身持久化，重启代价可接受）。
	waterMarks map[string]time.Time
	// driftSeen 是漂移上报的进程内迁移记忆（no-drift → drift 只报一次；
	// 重启清零 = 漂移存续时重报一次，漏报劣于重复）。
	driftSeen map[string]bool
	// routeBudget 是单次路由发布的 tick 预算（H11/M1 相关评审 H11 项；
	// 缺省 routePublishBudget，字段化为单测注入缝——见 routes.go）。
	routeBudget time.Duration
	// ── M1-8：重启恢复的重试态（tick goroutine 专用——Run 单 goroutine
	// 驱动 tick 与恢复，无并发访问；重启即清零，重试幂等见 recovery.go）──
	// recoveryPending 报告启动恢复未完成（SwarmReady 未就绪/扫描失败），
	// 由 tick 以 30s 频控整轮重试。
	recoveryPending bool
	// recoveryStuck 是单记录瞬态失败（底座读失败等）的待重试集合；重试轮
	// 只处理仍卡住的记录，避免反复重开已恢复记录的观察窗。
	recoveryStuck map[string]bool
	// recoveryNextAt 是下一次恢复重试的最早时刻（频控）。
	recoveryNextAt time.Time
	// deleteScanNextAt 是 deleting 应用回收扫描的最早时刻（H10/MG-3 频控，
	// 与 recoveryNextAt 同模式：tick goroutine 专用——Run 单 goroutine 驱动，
	// 无并发访问；重启即清零 = 重启后立即扫一拍）。
	deleteScanNextAt time.Time
	// initScanNextAt 是 init 孤儿服务清扫的最早时刻（DT-4 频控，同上模式；
	// 重启即清零 = 重启后立即扫一拍——崩溃残留在一个窗口内被收口）。
	initScanNextAt time.Time
	// substrateNextAt 是运行期存在性对账扫描的最早时刻（T0-V2.2/R2 频控，
	// 与 deleteScanNextAt 同模式：tick goroutine 专用；重启即清零 = 重启后
	// 立即扫一拍）。
	substrateNextAt time.Time
	// substrateMissingSeen 是 substrate 服务缺失上报的进程内记忆（T0-V2.2/R2：
	// 持续缺失只报一次；服务恢复后清零可再报；重启清零 = 缺失存续时重报
	// 一次——重复优于漏报，与 driftSeen 同语义）。
	substrateMissingSeen map[string]bool
	// substrateDrainedSeen 是「服务在、任务全无」（F11，settled 应用 drain
	// 事件面）披露的进程内记忆：语义与 substrateMissingSeen 同款（持续形态
	// 只报一次；任务回岗清零可再报；重启清零 = 重报一次，重复优于漏报）。
	substrateDrainedSeen map[string]bool
	// metricsQ 是 VM 瞬时查询端口（autoscaler 评估器的数据面；nil = duty
	// 整体不在评估域——装配层经 WithMetricsQuerier 注入）。
	metricsQ MetricsQuerier
	// scalingNextAt 是扩缩评估扫描的最早时刻（autoscalingInterval 频控，
	// substrateNextAt 同模式：tick goroutine 专用；重启即清零 = 重启后立即
	// 评估一拍）。
	scalingNextAt time.Time
	// scalingDormantSeen / scalingNoDataSeen 是扩缩披露的进程内记忆（每策略
	// 一次性语义：条件存续只报一次；条件解除清零可再报；重启清零 = 重报
	// 一次，重复优于漏报——与 driftSeen 同族）。
	scalingDormantSeen map[string]bool
	scalingNoDataSeen  map[string]bool
	// dutyPanicOn / dutyCalls 是 safeCall 的测试注入缝（MG-5 覆盖面测试）：
	// 前者按 duty 名注入 panic（验证包壳隔离），后者记录经 safeCall 执行的
	// duty 名与次数（验证 duty 清单全部收口；Run goroutine 写、测试 goroutine
	// 读——dutyMu 保护）。生产恒为 nil/不读。
	dutyMu      sync.Mutex
	dutyPanicOn map[string]bool
	dutyCalls   map[string]int
}

// PlacementResolver 是引擎对放置层的消费端口（internal/placement.Resolver
// 隐式实现；接口在本包定义以便单测注入）。
type PlacementResolver interface {
	Resolve(ctx context.Context, in placement.Input) (placement.Decision, error)
	Apply(ctx context.Context, in placement.Input) (placement.Decision, error)
	Preflight(ctx context.Context, appID string) error
}

// NewEngine 构造发布引擎（时钟缺省真实时钟；水位表初始化）。
func NewEngine(cfg Config, store *state.Store, sub Substrate, images ImageChecker,
	resolver PlacementResolver, box *secrets.Box, log *slog.Logger) *Engine {
	return &Engine{
		cfg:                  cfg.Normalize(),
		store:                store,
		sub:                  sub,
		images:               images,
		resolver:             resolver,
		box:                  box,
		clock:                realClock{},
		log:                  log,
		waterMarks:           map[string]time.Time{},
		driftSeen:            map[string]bool{},
		routeBudget:          routePublishBudget,
		recoveryStuck:        map[string]bool{},
		substrateMissingSeen: map[string]bool{},
		substrateDrainedSeen: map[string]bool{},
		scalingDormantSeen:   map[string]bool{},
		scalingNoDataSeen:    map[string]bool{},
	}
}

// WithClock 注入时钟（单测）。
func (e *Engine) WithClock(c Clock) *Engine { e.clock = c; return e }

// PostDeployHook 是部署成功后的备份挂钩签名（rec 为成功部署记录的副本）。
type PostDeployHook func(rec state.DeployRecord)

// WithPostDeployHook 注入部署成功挂钩（T2.22 备份触发面：每次部署成功后
// 一次热备快照。引擎侧异步触发——go 例程 + 独立预算在实现方；挂钩失败
// 只落备份台账与告警，绝不回滚/阻塞已成功的部署）。
func (e *Engine) WithPostDeployHook(fn PostDeployHook) *Engine { e.postDeploy = fn; return e }

// EnvChangedHook 是 env 生效/变更后的联动挂钩签名（载荷 = appID；H9）。
type EnvChangedHook func(appID string)

// WithEnvChangedHook 注入 env 变更挂钩（H9：pending env 提升点触发日志
// 脱敏值集失效——装配层接到 logs.Manager.InvalidateRedaction。挂钩同步
// 执行，实现必须非阻塞（实现方只做缓存删除），失败不得影响部署终态）。
func (e *Engine) WithEnvChangedHook(fn EnvChangedHook) *Engine { e.envChanged = fn; return e }

// WithDatabaseTemplate 注入库模板连接信息端口（E4：dbtemplate.EnvPrefix /
// dbtemplate.ConnectionVars 的装配层适配——dbtemplate 在 engine 之上，只能
// 注入）。
func (e *Engine) WithDatabaseTemplate(p DatabaseTemplatePort) *Engine { e.dbTemplate = p; return e }

// WithSecretEnsurer 注入 Swarm secret 确保端口（E4 managed-databases §2.7
// compose secrets 注入链；实现 = substrate.Client——底座原语随底座适配器
// 落，装配层接线）。
func (e *Engine) WithSecretEnsurer(s SecretEnsurer) *Engine { e.secretEnsure = s; return e }

// Run 启动引擎主循环：启动扫描（控制面重启分类恢复）→ 周期 tick + 漂移
// 扫描。ctx 取消返回 nil（lynx actor 契约由服务壳负责阻塞语义）。
func (e *Engine) Run(ctx context.Context) error {
	e.safeCall("recoverInterrupted", func() { e.recoverInterrupted(ctx) })
	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()
	driftTicker := time.NewTicker(e.cfg.DriftInterval)
	defer driftTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			e.safeCall("tick", func() { e.tick(ctx) })
		case <-driftTicker.C:
			e.safeCall("driftScan", func() { e.driftScan(ctx) })
		}
	}
}

// Tick 单步推进（测试与诊断入口；生产由 Run 驱动）。
func (e *Engine) Tick(ctx context.Context) { e.tick(ctx) }

// dutyCallCount 是测试注入缝的读面（dutyMu 保护；测试外恒为零值——
// MG-5 测试专用，不进业务路径）。
func (e *Engine) dutyCallCount(name string) int {
	e.dutyMu.Lock()
	defer e.dutyMu.Unlock()
	return e.dutyCalls[name]
}

// safeCall 是 tick 路径 duty 的统一 panic 包壳（MG-5/M1-7）：defer recover
// → Error 日志（含栈）+ 跳过本拍——单个 duty 的 panic 不打死 tick 循环
// （毒数据/适配器违约只损失一拍，其余 duty 与后续 tick 照常）。
// advanceOne 另有 per-record 兜底（panic → 该部署 E_RUNTIME_UNAVAILABLE
// 终态），与本包壳分层：记录级处置在先、duty 级兜底在后。
//
// 契约：tick 与 drift ticker 的全部 duty 调用点必须经本包壳收口（新增
// duty 不走 safeCall 即违反 MG-5 覆盖面断言——safecall_test.go 的源扫描
// 测试钉死该契约）。dutyPanicOn/dutyCalls 是测试注入缝，生产恒 nil。
func (e *Engine) safeCall(name string, fn func()) {
	// recover 先装（含测试注入路径——包壳对入口注入同样兜底）。
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("engine: duty panic recovered (skipping this tick, tick continues)",
				"duty", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	e.dutyMu.Lock()
	if e.dutyPanicOn != nil && e.dutyPanicOn[name] {
		if e.dutyCalls != nil {
			e.dutyCalls[name]++
		}
		e.dutyMu.Unlock()
		panic("injected duty panic (MG-5 test): " + name)
	}
	if e.dutyCalls != nil {
		e.dutyCalls[name]++
	}
	e.dutyMu.Unlock()
	fn()
}

// tick 是一个推进周期：恢复重试（M1-8）→ 队列拾取 → 在途推进 → 窗后巡检
// → deleting 应用回收（H10/MG-3，时间闸降频）→ 运行期存在性对账
// （T0-V2.2/R2，时间闸降频）→ init 孤儿服务清扫（DT-4，时间闸降频）。
// 全部 duty 经 safeCall 收口（MG-5）。
func (e *Engine) tick(ctx context.Context) {
	e.safeCall("recoveryRetry", func() { e.retryRecoveryIfNeeded(ctx) })
	e.safeCall("pickQueued", func() { e.pickQueued(ctx) })
	e.safeCall("advanceActive", func() { e.advanceActive(ctx) })
	e.safeCall("watchPostWindow", func() { e.watchPostWindow(ctx) })
	e.safeCall("reapDeletingApps", func() { e.reapDeletingApps(ctx, false) })
	e.safeCall("substrateRecon", func() { e.substrateRecon(ctx, false) })
	e.safeCall("sweepInitJobs", func() { e.sweepInitJobs(ctx, false) })
	// 扩缩评估（W5-S1，D-V3W5-2）：收敛拍尾部——发布链路推进完毕后的稳态
	// 求值（自有 30s 频控闸；查询面未装配时 duty 空转）。
	e.safeCall("dutyAutoscaling", func() { e.dutyAutoscaling(ctx, false) })
}

// pickQueued 拾取可启动的 queued 部署：同 app 互斥——仅当该 app 无其他
// 在途部署时启动最早一条（第二个入队 queued 等待，release-semantics §2.5
// 并发控制行）。
//
// S18-A9：panic 隔离——本函数无单条部署的失败终态可落（行尚未拾取），
// 原内联 recover 已收口到 tick 的 safeCall("pickQueued")（MG-5 统一包壳，
// 行为等价：Error 日志含栈、不断 tick）；毒数据在拾取后的 advanceActive
// 内有 per-record 兜底。
func (e *Engine) pickQueued(ctx context.Context) {
	queued, err := e.store.NextQueuedDeployments(ctx, 20)
	if err != nil {
		e.log.Warn("engine: scan queued deployments", "error", err)
		return
	}
	startedApps := map[string]bool{}
	for _, q := range queued {
		if startedApps[q.AppID] {
			continue
		}
		// queued 即可取消（未触底座）。
		if q.CancelRequested {
			if err := e.cancelTerminal(ctx, q); err != nil {
				e.log.Warn("engine: cancel queued deployment", "deployment", q.ID, "error", err)
			}
			continue
		}
		has, err := e.store.AppHasNonTerminalDeployment(ctx, q.AppID)
		if err != nil {
			e.log.Warn("engine: mutex check", "app", q.AppID, "error", err)
			continue
		}
		// queued 行自身即在途——仅当该 app 的在途行全部是 queued（无
		// preparing/building/releasing/observing 竞争者）才可启动，且同
		// app 只启动最早一条。
		if has && !e.onlyQueuedForApp(ctx, q.AppID) {
			continue
		}
		if err := e.startQueued(ctx, q); err != nil {
			e.log.Warn("engine: start queued deployment", "deployment", q.ID, "error", err)
		}
		startedApps[q.AppID] = true
	}
}

// onlyQueuedForApp 报告该 app 的在途部署是否全部处于 queued（无执行中
// 竞争者）。
func (e *Engine) onlyQueuedForApp(ctx context.Context, appID string) bool {
	rows, err := e.store.ListNonTerminalDeployments(ctx)
	if err != nil {
		return false
	}
	for _, r := range rows {
		if r.AppID == appID && r.Status != state.DeployQueued {
			return false
		}
	}
	return true
}

// advanceActive 推进全部在途部署一个周期。
//
// S18-A9：每条部署的处理包 defer recover——单条毒记录（解码 nil 指针、
// 断言违约等）只失败该条（E_RUNTIME_UNAVAILABLE 终态 + Error 日志含栈），
// 不 crash-loop 控制面；其余部署与后续 tick 照常推进。
func (e *Engine) advanceActive(ctx context.Context) {
	active, err := e.store.ListNonTerminalDeployments(ctx)
	if err != nil {
		e.log.Warn("engine: scan active deployments", "error", err)
		return
	}
	for _, d := range active {
		if err := e.advanceOne(ctx, d); err != nil {
			e.log.Warn("engine: advance deployment", "deployment", d.ID, "status", d.Status, "error", err)
		}
	}
}

// advanceOne 推进单条在途部署一个周期（A9 panic 兜底包壳；状态分发与
// advanceActive 原实现逐字一致）。
func (e *Engine) advanceOne(ctx context.Context, d state.DeployRecord) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("engine: engine advance panic recovered",
				"deployment", d.ID, "status", d.Status,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			// 失败终态兜底：CAS 自当前快照状态出发，行已被并发推进（重启
			// 恢复/竞争）时落败即幂等收敛；失败码固定 E_RUNTIME_UNAVAILABLE
			//（panic 原文不进对外 detail，只进日志）。
			if ferr := e.failTransition(ctx, d, "E_RUNTIME_UNAVAILABLE",
				"engine advance panic recovered"); ferr != nil {
				e.log.Warn("engine: panic fallback failed to record terminal state", "deployment", d.ID, "error", ferr)
			}
			err = nil // 已按终态处置：不向上重复告警
		}
	}()
	switch d.Status {
	case state.DeployQueued:
		// 等待互斥窗口（pickQueued 负责）。
	case state.DeployPreparing:
		err = e.runPreparing(ctx, d)
	case state.DeployBuilding:
		err = e.runBuilding(ctx, d)
	case state.DeployReleasing:
		err = e.evaluateReleasing(ctx, d)
	case state.DeployObserving:
		err = e.evaluateObserving(ctx, d)
	}
	return err
}

// startQueued 启动一条 queued 部署：queued → preparing（CAS 谓词防多实例
// 竞争）并同 tick 执行准备。经单写点 EnterPhase（T0-V2.2）同一事务完成
// 转移、拾取锚点（H11：预算自拾取时刻起算，排队等待不计入）与
// deployment.release_started 事件。
func (e *Engine) startQueued(ctx context.Context, rec state.DeployRecord) error {
	anchor := e.now()
	if err := e.store.EnterPhase(ctx, rec.ID, state.DeployQueued, state.DeployPreparing,
		state.DeploymentPatch{PhaseStartedAt: &anchor},
		deploymentEvent("deployment.release_started", rec.ID)); err != nil {
		return fmt.Errorf("claim queued deployment: %w", err)
	}
	rec.Status = state.DeployPreparing
	rec.PhaseStartedAt = anchor
	return e.runPreparing(ctx, rec)
}

// prepareBudgetBaseline 返回准备/构建预算的起算基线（H11）：拾取时刻
// phase_started_at；迁移前存量行（基线未写、零值）回落 created_at 保持旧
// 语义。blocked_waiting 是 releasing 的子状态，基线在其间不重置也无消费
// （releasing 预算由 watchdog_deadline_at 起算，恢复续跑时重臂）——两者
// 互不冲突。
func prepareBudgetBaseline(rec state.DeployRecord) time.Time {
	if rec.PhaseStartedAt.IsZero() {
		return rec.CreatedAt
	}
	return rec.PhaseStartedAt
}

// prepareResult 是 preparing 阶段的中间产物（reload/放置/env；building 与
// preparing 共用——明文 env 不落库，building 重算，spec_hash 复核漂移）。
type prepareResult struct {
	spec        *compose.Spec
	fileEnv     map[string]map[string]string
	composeEnv  map[string]map[string]string
	platformEnv []envlayer.PlatformVar
	decision    placement.Decision
	// app 是本次部署的 app 权威行（v0.3 W2-S3：TeamSlug/ProjectSlug 随行
	// 装载——三段命名公式的参数源，规划期单点读取）。
	app state.App
	// s3Env / attachRustfs 是 S3 注入面（E3-4：fleetly.s3=true 服务的
	// system env 与 rustfs 网络牵线；nil/false = 本发布无注入面）。
	s3Env        map[string][]envlayer.PlatformVar
	attachRustfs bool
	// dbNetworks 是库引用的网络牵线面（E4：fleetly.databases label 服务 →
	// 库共享网络名列表；nil = 本发布无库引用）。
	dbNetworks map[string][]string
	// secretMounts 是服务级 secret 挂载面（E4 §2.7：compose secrets 声明 →
	// 已解析的 SecretMount 列表；nil = 本发布无 secret 声明）。
	secretMounts map[string][]SecretMount
	// configMounts 是服务级 config 挂载面（OT-3/IMPL-T1-4：compose configs
	// 声明 → 已解析的 ConfigMount 列表；nil = 本发布无 config 声明）。
	configMounts map[string][]ConfigMount
	// warnings 是 preparing 期产出的计划警告（E4 库引用
	// W_DB_REFERENCE_NOT_READY 等）——随 planAndRelease 以 deployment.warning
	// 事件披露（与 plan.Warnings 同管道）。
	warnings []compose.Warning
}

// runPreparing 执行准备阶段：底座就绪 → compose 重载 → 放置解析/绑定/前哨
// → env 提取 → secret 检查 → 分路（build-mode → building；纯镜像 → 直接
// 规划进 releasing）。kind=rollback 分路见 rollback.go（期望态已随入队
// 固化，preflight 后直接对账）。
func (e *Engine) runPreparing(ctx context.Context, rec state.DeployRecord) error {
	if rec.Kind == kindRollback {
		return e.runRollbackPreparing(ctx, rec)
	}
	// cancel（preparing 未触底座：直接落 cancelled）。
	if rec.CancelRequested {
		return e.cancelTerminal(ctx, rec)
	}
	// 准备预算：底座不可用等暂态错误重试到 基线+deployTimeout 为止（基线 =
	// 拾取时刻，排队等待不计入预算，H11；存量行锚点为 0 回落 created_at）。
	if anchor := prepareBudgetBaseline(rec); !anchor.IsZero() && e.now().Sub(anchor) > e.cfg.DeployTimeout {
		return e.failTransition(ctx, rec, "E_RUNTIME_UNAVAILABLE",
			"preparing phase exceeded the deploy watchdog budget (substrate unavailable or environment error)")
	}
	if err := e.requireSwarm(ctx, rec); err != nil {
		return err // 暂态：下一 tick 重试（预算由上守）
	}

	pre, err := e.prepareInputs(ctx, rec)
	if err != nil {
		if errors.Is(err, errS3WaitingRustfsCredentials) {
			// 暂态（rustfs 托管凭据生成拍未到）：停留 preparing 下一拍重试，
			// 预算由既有 preparing 看门狗守门（与 swarm 未就绪同型）。
			return nil
		}
		return e.failTransitionErr(ctx, rec, err)
	}

	// 分路：任一 build-mode 服务 → building（构建产物核对）。
	hasBuild := false
	for i := range pre.spec.Services {
		if pre.spec.Services[i].Build != nil {
			hasBuild = true
			break
		}
	}
	if hasBuild {
		if err := e.store.EnterPhase(ctx, rec.ID, state.DeployPreparing, state.DeployBuilding,
			state.DeploymentPatch{}); err != nil {
			return err
		}
		rec.Status = state.DeployBuilding
		return e.runBuilding(ctx, rec)
	}
	return e.planAndRelease(ctx, rec, pre)
}

// runBuilding 执行构建直通核对（release-semantics 场景 2）：build-mode 服务
// 必须已有匹配当前 compose（spec_hash）的成功构建；镜像 digest 取自 builds
// 行（D9）。全部可得 → 规划 → releasing。
func (e *Engine) runBuilding(ctx context.Context, rec state.DeployRecord) error {
	if rec.CancelRequested {
		return e.cancelTerminal(ctx, rec)
	}
	// 构建预算同 preparing：锚点起算（拾取时刻，排队不计入，H11）。
	if anchor := prepareBudgetBaseline(rec); !anchor.IsZero() && e.now().Sub(anchor) > e.cfg.DeployTimeout {
		return e.failTransition(ctx, rec, "E_RUNTIME_UNAVAILABLE",
			"building phase exceeded the deploy watchdog budget")
	}
	if err := e.requireSwarm(ctx, rec); err != nil {
		return err
	}
	pre, err := e.prepareInputs(ctx, rec)
	if err != nil {
		if errors.Is(err, errS3WaitingRustfsCredentials) {
			return nil // 暂态：停留 building 下一拍重试（同 preparing 哨兵）
		}
		return e.failTransitionErr(ctx, rec, err)
	}
	return e.planAndRelease(ctx, rec, pre)
}

// prepareInputs 重载 compose、解析放置、提取 env 与平台层明文（preparing/
// building 共用；幂等——重启后重入）。
func (e *Engine) prepareInputs(ctx context.Context, rec state.DeployRecord) (*prepareResult, error) {
	// app 行随行装载（v0.3 W2-S3：team/prj slug 是三段命名公式的参数源；
	// slug 不可变——部署期单点读取即缓存）。行缺失 = 部署队列与权威态的
	// 不变量破坏（app 行先于部署行存在），fail-fast。
	app, err := e.store.GetAppByID(ctx, rec.AppID)
	if err != nil {
		return nil, errorf("E_RUNTIME_UNAVAILABLE", "failed to load app %s for deployment %s: %v", rec.AppID, rec.ID, err)
	}
	spec, _, err := compose.Load(ctx, rec.ComposePath)
	if err != nil {
		return nil, err // compose.Load 已携带 E_COMPOSE_* 信封（场景 1）
	}
	if spec.SpecHash != rec.SpecHash {
		return nil, errorf("E_COMPOSE_UNSUPPORTED",
			"compose file changed since enqueue (spec_hash %s → %s): cancel and redeploy",
			rec.SpecHash, spec.SpecHash)
	}

	// 放置：意图解析 + 绑定落库 + 卷登记（幂等），随后部署前哨快速失败
	//（E_PLACEMENT_NODE_UNAVAILABLE / GONE / E_VOLUME_NODE_MISMATCH）。
	decision, err := e.resolver.Apply(ctx, placement.Input{
		AppID:    rec.AppID,
		AppName:  rec.AppName,
		Volumes:  composeVolumes(spec),
		LabelRef: composePlacementLabel(spec),
	})
	if err != nil {
		return nil, err
	}
	if err := e.resolver.Preflight(ctx, rec.AppID); err != nil {
		return nil, err
	}

	fileEnvs, err := extractServiceEnvs(rec.ComposePath)
	if err != nil {
		return nil, err
	}
	fileEnv := map[string]map[string]string{}
	composeEnv := map[string]map[string]string{}
	for i := range spec.Services {
		svc := &spec.Services[i]
		se := fileEnvs[svc.Name]
		fileEnv[svc.Name] = se.File
		composeEnv[svc.Name] = se.Compose
		// 键集交叉核对：提取器与归一化形态必须同源（文件被换写的防御）。
		if !envKeySetsMatch(svc.Environment, se.File, se.Compose) {
			return nil, errorf("E_COMPOSE_UNSUPPORTED",
				"env key set of service %s does not match the normalized form (compose file may have been rewritten concurrently)", svc.Name)
		}
	}

	// S3 注入面（E3-4）：label fleetly.s3=true 的服务解析 system env 与
	// 网络牵线。托管凭据未备便是暂态（duty 生成拍未到）——返回哨兵，调用
	// 方停留 preparing 下一拍重试（预算由 preparing 看门狗守门）。
	s3Env, attachRustfs, err := e.resolveS3Injection(ctx, spec)
	if err != nil {
		if errors.Is(err, errS3WaitingRustfsCredentials) {
			e.log.Warn("engine: s3 injection waiting for managed rustfs credentials (retrying next tick)", "deployment", rec.ID)
			return nil, err
		}
		return nil, err
	}
	// 库引用面（E4 managed-databases §2.4/§2.5）：label fleetly.databases
	// 的服务解析引用（存在性/前缀冲突哨兵 + 未就绪警告）、物化 system env
	// 连接串（upsert → pending——必须先于 platformEnvForMerge 读取，物化行
	// 才能随本次部署合并消费）并产出库共享网络牵线。
	dbNetworks, dbWarnings, err := e.resolveDatabaseReferences(ctx, rec.AppID, rec.AppName, spec)
	if err != nil {
		return nil, err
	}
	// secret 挂载面（E4 managed-databases §2.7）：compose secrets 声明 →
	// app_secrets 存在性哨兵（E_SECRET_NOT_FOUND fail-fast）+ Swarm secret
	// 确保 + SecretMount 装配（值零进规划产物）。
	secretMounts, err := e.resolveSecretMounts(ctx, app, spec)
	if err != nil {
		return nil, err
	}
	// config 挂载面（OT-3/IMPL-T1-4）：compose configs 声明 → app_configs
	// 存在性哨兵（E_CONFIG_NOT_FOUND fail-fast）+ Swarm config 确保（内容
	// 寻址对象名）+ ConfigMount 装配（值零进规划产物）。
	configMounts, err := e.resolveConfigMounts(ctx, app, spec)
	if err != nil {
		return nil, err
	}
	// 平台层读取（含上一步物化的 pending 行——pending 参与合并，S16-C4）。
	platform, err := e.platformEnvForMerge(ctx, rec.AppID)
	if err != nil {
		return nil, err
	}
	return &prepareResult{
		spec:         spec,
		fileEnv:      fileEnv,
		composeEnv:   composeEnv,
		platformEnv:  platform,
		decision:     decision,
		app:          app,
		s3Env:        s3Env,
		attachRustfs: attachRustfs,
		dbNetworks:   dbNetworks,
		secretMounts: secretMounts,
		configMounts: configMounts,
		warnings:     dbWarnings,
	}, nil
}

// planAndRelease 镜像 digest 核对（场景 3）→ 规划 → 快照落库 → releasing。
func (e *Engine) planAndRelease(ctx context.Context, rec state.DeployRecord, pre *prepareResult) error {
	images := map[string]string{}
	for i := range pre.spec.Services {
		svc := &pre.spec.Services[i]
		ref, err := e.resolveImage(ctx, rec, svc)
		if err != nil {
			return e.failTransitionErr(ctx, rec, err)
		}
		images[svc.Name] = ref
	}
	volumes, err := e.store.ListAppVolumes(ctx, rec.AppID)
	if err != nil {
		return e.failTransitionErr(ctx, rec, errorf("E_RUNTIME_UNAVAILABLE", "failed to read volume registry: %v", err))
	}
	plan, err := BuildPlan(PlanInput{
		AppID:               rec.AppID,
		AppName:             rec.AppName,
		TeamSlug:            pre.app.TeamSlug,
		PrjSlug:             pre.app.ProjectSlug,
		DeploymentID:        rec.ID,
		Spec:                pre.spec,
		FileEnv:             pre.fileEnv,
		ComposeEnv:          pre.composeEnv,
		PlatformEnv:         pre.platformEnv,
		SystemEnv:           pre.s3Env,
		AttachRustfsNetwork: pre.attachRustfs,
		DBNetworks:          pre.dbNetworks,
		SecretMounts:        pre.secretMounts,
		ConfigMounts:        pre.configMounts,
		Images:              images,
		Decision:            pre.decision,
		Volumes:             volumes,
	})
	if err != nil {
		return e.failTransitionErr(ctx, rec, err)
	}

	snapshot, err := e.box.Encrypt(plan.DesiredSpecJSON)
	if err != nil {
		return e.failTransitionErr(ctx, rec, errorf("E_RUNTIME_UNAVAILABLE", "failed to encrypt desired-state snapshot: %v", err))
	}

	// 规划警告（preparing 期产出 + plan 期产出；W_ENV_PLATFORM_OVERRIDE /
	// W_DB_REFERENCE_NOT_READY 等）以 deployment.warning 事件披露（payload
	// 只含 code/service——message 不进事件，明文纪律的结构性保障）。
	for _, w := range pre.warnings {
		if w.Code == "" {
			continue
		}
		if err := e.store.InTx(ctx, func(tx *state.Tx) error {
			return appendEvents(ctx, tx, deploymentEvent("deployment.warning", rec.ID,
				"code", w.Code, "service", w.Service))
		}); err != nil {
			return err
		}
	}
	for _, w := range plan.Warnings {
		if w.Code == "" {
			continue
		}
		if err := e.store.InTx(ctx, func(tx *state.Tx) error {
			return appendEvents(ctx, tx, deploymentEvent("deployment.warning", rec.ID,
				"code", w.Code, "service", w.Service))
		}); err != nil {
			return err
		}
	}

	releaseAt := e.now()
	deadline := releaseAt.Add(e.cfg.DeployTimeout)
	specHash := pre.spec.SpecHash
	envHash := plan.EnvSnapshotHash
	desiredHash := plan.DesiredHash
	desiredSpec := string(snapshot)
	// DT-4：含 init job 的发布进入 init 子相位——预算取 max(各 job 预算)
	// 作相位兜底（per-job 判定在 evaluateInitJobs）；相位与快照/看门狗锚
	// **同一 EnterPhase 原子落位**（不存在「已进 releasing 但 init 相位
	// 未记」的窗口）。无 init 模板时相位零写、预算 = DeployTimeout（守卫⑤：
	// 无 init job 的 release 路径行为零变化）。
	initJobs := plan.InitJobs
	patch := state.DeploymentPatch{
		ReleaseStartedAt:   &releaseAt,
		WatchdogDeadlineAt: &deadline,
		SpecHash:           &specHash,
		EnvSnapshotHash:    &envHash,
		DesiredHash:        &desiredHash,
		DesiredSpec:        &desiredSpec,
	}
	if len(initJobs) > 0 {
		phase := state.PhaseInitJobs
		patch.Phase = &phase
		deadline = releaseAt.Add(e.initJobsMaxBudget(initJobs))
		patch.WatchdogDeadlineAt = &deadline
	}
	// 释放迁移经单写点（T0-V2.2）：preparing/building → releasing 与快照
	// 字段（看门狗锚、哈希、密文快照、init 相位）同事务原子生效。
	if err := e.store.EnterPhase(ctx, rec.ID, rec.Status, state.DeployReleasing, patch); err != nil {
		return err
	}
	rec.Status = state.DeployReleasing
	rec.DesiredSpec = desiredSpec
	rec.ReleaseStartedAt = releaseAt
	rec.WatchdogDeadlineAt = deadline
	rec.Phase = ""
	if len(initJobs) > 0 {
		rec.Phase = state.PhaseInitJobs
	}

	if len(initJobs) > 0 {
		// 迁移先于新代码：先确保 job 服务在位（幂等），下一拍起逐拍判定；
		// 长驻服务的对账（applyDesired）推迟到 job 全过（finishInitJobs）。
		return e.provisionInitJobServices(ctx, rec, initJobs)
	}

	// 对账执行（新增/更新/删除）。失败走失败分流（M1-2，D-REL-4 唯一
	// 入口）：对账是半应用现场（部分服务已创建/更新/删除），直接落 failed
	// 会留下无人处置的中间态——未切流（first_healthy_at 未落定）分支自动
	// 套用归位重放（非首发）或 scale=0 保留现场（首发）；底层 err 摘要进
	// detail（错误码经 appErrOf 归一为注册码）。
	if err := e.applyDesired(ctx, rec, plan.Services, false); err != nil {
		ae := appErrOf(err, rec.ID)
		return e.failUnswitchedOrSwitched(ctx, rec, ae.Code(),
			fmt.Sprintf("release reconcile failed (half-applied state routed per failure policy — replay/scale=0): %s", ae.Message()))
	}
	return nil
}

// registryPreflightErr 报告 err 是否已是 registry 前哨信封
// （E_REGISTRY_UNAVAILABLE——registry 模式部署前哨的归一产物，multi-node
// 设计 §5.2/D-MN-11）：是则 resolveImage 原样透传，不再被 E_RUNTIME_UNAVAILABLE
// 包装（deploy 路径前哨码面保真——修复建议分层「registry 错 → 查 zot/网络/
// 凭据」不被通用码冲掉）；其余错误维持既有包装语义。
func registryPreflightErr(err error) bool {
	var ae *apperr.Error
	return asAppErr(err, &ae) && ae != nil && ae.Code() == "E_REGISTRY_UNAVAILABLE"
}

// resolveImage 裁决单个服务的镜像引用（digest 钉定，D9）：
//   - image 模式：compose 值直通 + 本机 digest 钉定（缺失 →
//     E_IMAGE_PULL_FAILED，场景 3）；
//   - build 模式：builds 表最近一次匹配当前 spec_hash 的成功构建
//     （缺失 → E_BUILD_FAILED，场景 2；`fleetly build` 先行的契约）。
func (e *Engine) resolveImage(ctx context.Context, rec state.DeployRecord, svc *compose.Service) (string, error) {
	if svc.Build == nil {
		ref := svc.Image
		digest, err := e.images.ImageDigest(ctx, ref)
		if err != nil {
			if errors.Is(err, ErrImageMissing) {
				// IMPL-T1-2/DT-2：文案点名 image + 底层原因（substrate 包装
				// registry 解析与本机 inspect 双因）；修复指引含平台 registry
				// 凭证设置面。
				return "", errorf("E_IMAGE_PULL_FAILED",
					"image %s of service %s is not available: %v (pull or build it locally, or configure registry credentials via 'fleetly registry set')",
					ref, svc.Name, err)
			}
			if registryPreflightErr(err) {
				return "", err // registry 前哨信封透传（码面保真，multi-node §5.2）
			}
			return "", errorf("E_RUNTIME_UNAVAILABLE", "image check failed %s: %v", ref, err)
		}
		return pinDigest(ref, digest), nil
	}

	builds, err := e.store.ListAppBuilds(ctx, rec.AppID, 50)
	if err != nil {
		return "", errorf("E_RUNTIME_UNAVAILABLE", "failed to read build history: %v", err)
	}
	for _, b := range builds {
		if b.Service != svc.Name || b.Status != state.BuildSucceeded || b.ImageDigest == "" {
			continue
		}
		req, err := build.DecodeRequest(b.Request)
		if err != nil {
			continue // 历史行损坏：跳过（更早的成功构建仍可命中）
		}
		if req.SpecHash != "" && req.SpecHash != rec.SpecHash {
			continue
		}
		// digest 钉定经镜像端口复核（清单摘要优先；本机构建镜像无清单摘要
		// 时按 tag 引用直用——免 registry 形态，镜像不被平台自动清理）。
		digest, err := e.images.ImageDigest(ctx, b.ImageRef)
		if err != nil {
			if errors.Is(err, ErrImageMissing) {
				continue // 构建产物已被清理：尝试更早的成功构建
			}
			if registryPreflightErr(err) {
				return "", err // registry 前哨信封透传（码面保真，multi-node §5.2）
			}
			return "", errorf("E_RUNTIME_UNAVAILABLE", "image check failed %s: %v", b.ImageRef, err)
		}
		return pinDigest(b.ImageRef, digest), nil
	}
	return "", errorf("E_BUILD_FAILED",
		"service %s has no available build matching the current compose (spec_hash %.12s): run fleetly build before deploying",
		svc.Name, rec.SpecHash)
}

// pinDigest 把镜像引用钉定为不可变 digest 形态（已是 digest 引用则原样）。
func pinDigest(ref, digest string) string {
	if digest == "" || strings.Contains(ref, "@") {
		return ref
	}
	return ref + "@" + digest
}

// requireSwarm 确认底座就绪；未就绪不是部署失败（暂态），由准备预算守门。
func (e *Engine) requireSwarm(ctx context.Context, rec state.DeployRecord) error {
	if err := e.sub.SwarmReady(ctx); err != nil {
		if errors.Is(err, ErrNotSwarmReady) {
			e.log.Warn("engine: swarm not ready, retrying next tick", "deployment", rec.ID)
			return nil
		}
		return e.failTransitionErr(ctx, rec, errorf("E_RUNTIME_UNAVAILABLE", "substrate check failed: %v", err))
	}
	return nil
}

// platformEnvForMerge 读取并解密平台 env（三层合并第三层）。pending 与
// effective 都参与合并——部署是 pending 的消费点（随本次部署注入，成功后
// MarkAppEnvEffective 提升；失败不提升、下次部署重试，architecture §2.4
// 变量合并行 + 发布引擎消费契约）。解密失败 = 密钥/密文损坏：显式失败不
// 静默降级。
func (e *Engine) platformEnvForMerge(ctx context.Context, appID string) ([]envlayer.PlatformVar, error) {
	rows, err := e.store.ListAppEnv(ctx, appID)
	if err != nil {
		return nil, errorf("E_RUNTIME_UNAVAILABLE", "failed to read platform env: %v", err)
	}
	out := make([]envlayer.PlatformVar, 0, len(rows))
	for _, row := range rows {
		plain, err := e.box.Decrypt([]byte(row.Value))
		if err != nil {
			return nil, errorf("E_RUNTIME_UNAVAILABLE",
				"failed to decrypt platform env %s (master key mismatch or corrupted ciphertext)", row.Key)
		}
		out = append(out, envlayer.PlatformVar{Key: row.Key, Value: string(plain), Source: row.Source})
	}
	return out, nil
}

// applyDesired 是 stack 对账核心（architecture §2.4 省略=删除）：期望服务
// 集 vs 实际（fleetly- 前缀 + managed label）——缺失创建、内容/归属更新、
// 多余删除。幂等；服务 label 以当前发布归属重写（任务零替换，Spike B2）。
//
// force 语义：false（发布对账）以 desired-hash label 做跳过捷径；true
// （归位/回滚/漂移收敛的重放路径）**恒下发 ServiceUpdate**——label 是上次
// 平台写的存根，外部改动不清理它：期望与 label 相等不代表实况未被篡改，
// 重放必须以 swarm 侧 spec 重申为准（同内容 ServiceUpdate 不触发任务重建，
// Spike B2 零替换语义不受影响）。
func (e *Engine) applyDesired(ctx context.Context, rec state.DeployRecord, desired []ServiceSpec, force bool) error {
	if err := e.sub.SwarmReady(ctx); err != nil {
		return appErrOf(err, rec.ID)
	}
	if len(desired) == 0 {
		return nil
	}
	// secret 底座对象确保（E4 managed-databases §2.7）：快照重放路径（回滚/
	// 归位/漂移收敛——不经 prepareInputs）的挂载名先对 app_secrets 现值解析
	// 并确保在位；发布路径的 ensure 已在 resolveSecretMounts 完成，此处幂等
	// 重入无害（inspect 命中即跳过）。解析失败（轮换悬空）→ E_SECRET_NOT_FOUND。
	if err := e.ensureSnapshotSecrets(ctx, rec, desired); err != nil {
		return appErrOf(err, rec.ID)
	}
	// config 底座对象确保（OT-3/IMPL-T1-4）：快照重放路径的挂载名先对
	// app_configs 现值解析并确保在位（内容换版 → 名字悬空 →
	// E_CONFIG_NOT_FOUND 诚实失败）；发布路径的 ensure 已在 resolveConfigMounts
	// 完成，此处幂等重入无害。
	if err := e.ensureSnapshotConfigs(ctx, rec, desired); err != nil {
		return appErrOf(err, rec.ID)
	}
	// 归属识别面从期望 spec 自身推导（v0.3 W2-S3：spec 的服务 label 携带
	// 三段限定形 app 值与 fleetly.team/fleetly.project 两键——快照重放路径
	// 与规划路径同源同构，不再以 rec.AppName 二次推导）。对账的 label 过滤
	// 与「省略=删除」的作用域即随归属走：换派（MoveApp）后的新发布只可见
	// 新命名上下文的同归属服务。
	appLabel := ""
	for i := range desired {
		if v := desired[i].ServiceLabels[state.LabelApp]; v != "" {
			appLabel = v
			break
		}
	}
	if appLabel == "" {
		return appErrOf(errorf("E_RUNTIME_UNAVAILABLE",
			"desired state for deployment %s carries no %s label (planner/snapshot invariant broken)", rec.ID, state.LabelApp), rec.ID)
	}
	// 网络前置确认：期望 spec 引用的全部平台侧网络幂等创建（per-app 专属
	// 网络 + E3-4 rustfs 牵线 + E4 库共享网络——设计 §2.4 时序行 3：
	// NetworkEnsure 全部附加网络；应用发布不因组件收敛时序失败）。
	nets := map[string]bool{}
	for i := range desired {
		for _, n := range desired[i].Networks {
			nets[n.Name] = true
		}
	}
	for name := range nets {
		if err := e.sub.NetworkEnsure(ctx, name); err != nil {
			return appErrOf(err, rec.ID)
		}
	}

	existing, err := e.sub.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelApp:     appLabel,
	})
	if err != nil {
		return appErrOf(err, rec.ID)
	}
	byName := map[string]ServiceState{}
	for _, s := range existing {
		byName[s.Name] = s
	}
	desiredNames := map[string]bool{}

	for i := range desired {
		spec := desired[i]
		desiredNames[spec.Name] = true
		// 快照/规划产出的服务 label 不含哈希（哈希后附加）——对账时以当前
		// 发布归属补齐，保证 label 与哈希一致。
		if spec.ServiceLabels == nil {
			spec.ServiceLabels = map[string]string{}
		}
		spec.ServiceLabels[state.LabelDeployment] = rec.ID
		spec.ServiceLabels[state.LabelDesiredHash] = spec.DesiredHash()

		cur, ok := byName[spec.Name]
		switch {
		case !ok:
			if err := e.sub.ServiceCreate(ctx, spec); err != nil {
				return appErrOf(err, rec.ID)
			}
		case force ||
			cur.DesiredHash != spec.DesiredHash() ||
			cur.Labels[state.LabelDeployment] != rec.ID:
			if err := e.sub.ServiceUpdate(ctx, spec.Name, spec); err != nil {
				return appErrOf(err, rec.ID)
			}
		}
	}
	// 省略=删除（compose 移除服务 → 删 Swarm service；卷数据不删）。
	// E5 Cron / DT-4：一次性 job 服务（fleetly-cron-* / fleetly-init-* 前缀）
	// 不在此对账域——它们是调度器/init 相位的瞬时对象（在途 job 被发布对账
	// 误删 = 运行/迁移静默丢失），生命周期归各自所有者（cron 调度器 /
	// initjobs 相位 + sweepInitJobs）。
	for _, s := range existing {
		if !desiredNames[s.Name] {
			if naming.IsCronJobName(s.Name) || naming.IsInitJobName(s.Name) {
				continue
			}
			if err := e.sub.ServiceRemove(ctx, s.Name); err != nil {
				return appErrOf(err, rec.ID)
			}
		}
	}
	// config 旧对象回收（OT-3/IMPL-T1-4）：服务收敛完成后，保留本次期望面
	// 引用的对象（长驻集 + 快照内 Job 模板），其余内容寻址旧版 best-effort
	// 清场（真机实证：引用换版后 daemon 即允许删除；in-use 拒绝 = 留待下一
	// 拍，绝不阻塞收敛）。
	e.gcAppConfigs(ctx, appLabel, e.configKeepSet(rec, desired))
	return nil
}

// scaleToZero 首发失败/取消的保留现场原语（D-REL-5）：期望服务副本清零、
// service/revision 与日志保留。服务不存在（发布未触底座）则跳过。
func (e *Engine) scaleToZero(ctx context.Context, rec state.DeployRecord, specs []ServiceSpec) error {
	for i := range specs {
		spec := specs[i]
		if _, err := e.sub.ServiceInspect(ctx, spec.Name); err != nil {
			if errors.Is(err, ErrServiceNotFound) {
				continue
			}
			return appErrOf(err, rec.ID)
		}
		spec.Replicas = 0
		if err := e.sub.ServiceUpdate(ctx, spec.Name, spec); err != nil {
			return appErrOf(err, rec.ID)
		}
	}
	return nil
}

// ── 快照编解码（desired_spec 密文 ↔ []ServiceSpec）─────────────────────────

func (e *Engine) decodeSpecs(rec state.DeployRecord) ([]ServiceSpec, error) {
	if rec.DesiredSpec == "" {
		return nil, fmt.Errorf("engine: deployment %s has no desired-spec snapshot", rec.ID)
	}
	plain, err := e.box.Decrypt([]byte(rec.DesiredSpec))
	if err != nil {
		return nil, fmt.Errorf("engine: decrypt desired-spec %s: %w", rec.ID, err)
	}
	var all []ServiceSpec
	if err := json.Unmarshal(plain, &all); err != nil {
		return nil, fmt.Errorf("engine: decode desired-spec %s: %w", rec.ID, err)
	}
	// E5 Cron：Job 模板（一次性 cron 服务的执行形态，E5 执行行）不出本投影
	// ——发布对账/回滚重放/漂移/存在性对账/释放判定的期望集恒为长驻服务。
	// Job spec 由 cron 调度器（internal/cron，自有解密读面）按点克隆执行；
	// 快照内保留模板是「spec/快照照记」的口径。
	specs := make([]ServiceSpec, 0, len(all))
	for _, s := range all {
		if s.Job {
			continue
		}
		specs = append(specs, s)
	}
	return specs, nil
}

// decodeAllSpecs 解出快照全量模板（含 Job——cron/init 服务的执行形态来源；
// config keep-set 汇总用：init job 引用的 config 对象属本 revision 期望引用
// 面，见 configinject.go configKeepSet）。
func (e *Engine) decodeAllSpecs(rec state.DeployRecord) ([]ServiceSpec, error) {
	if rec.DesiredSpec == "" {
		return nil, fmt.Errorf("engine: deployment %s has no desired-spec snapshot", rec.ID)
	}
	plain, err := e.box.Decrypt([]byte(rec.DesiredSpec))
	if err != nil {
		return nil, fmt.Errorf("engine: decrypt desired-spec %s: %w", rec.ID, err)
	}
	var all []ServiceSpec
	if err := json.Unmarshal(plain, &all); err != nil {
		return nil, fmt.Errorf("engine: decode desired-spec %s: %w", rec.ID, err)
	}
	return all, nil
}

// now 是时钟出口（单测注入）。
func (e *Engine) now() time.Time { return e.clock.Now() }
