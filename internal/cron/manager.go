// Package cron 是定时任务调度器（E5 Cron，架构 §4.3 细则 + object-storage
// 设计 §8 契约面；D-CR-1 已裁 robfig/cron/v3）。
//
// 定位与边界：
//   - robfig 只做「表达式 → Schedule」的解析与 next-fire 计算（cron.
//     ParseStandard 恰为平台契约的五段标准式——六段含秒在 compose 解析期
//     即拒，契约天然成立）；触发、重叠 skip、节点前哨、看门狗、残留收口
//     全是本包逻辑，不使用 robfig 的 runner/并发模型。
//   - tick 循环骨架沿用 v0.1 备份 ticker 模式（stop/done channel）；扫描拍
//     缺省 10s，每拍现读 state 组装调度集（active apps × 最新 revision 的
//     compose 快照 × 最新 succeeded 部署的 desired_spec 快照中的 Job 模板）
//     ——无缓存失效面，读代价与平台规模线性。
//   - 调度集的执行形态（job 模板）来自发布引擎的期望态快照（BuildPlan 对
//     cron 服务产出 Job=true 的 ServiceSpec，engine.decodeSpecs 对外过滤
//     ——长驻对账只见长驻服务）。
//   - 策略：重叠 skip（每 schedule max-concurrent 1）；绑定节点前哨不通过
//     skip（与控制面停机 skip 同型；无卷未绑定的 app 由 Swarm 默认放置，
//     不做节点前哨）；停机错过点不补跑（每拍以 now 为锚取下一触发点，窗
//     口外的过去点丢弃；启动首拍对「上一 handled 点之后存在整点未处理」
//     的 schedule 披露一次 cron.skipped(missed_downtime)）；失败只记录
//     （通知面 W5）。
//   - 看门狗：默认 10m，label fleetly.cron.timeout 覆盖（compose 归一化期
//     校验）；超时删 job 服务 + 行记 timeout + cron.timed_out（FZ-4 钉名）。
package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/robfig/cron/v3"

	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台缺省参数（架构 §4.3 执行行：看门狗 cronJobTimeout 默认 10m；扫描拍
// 缺省 10s——分钟级 cron 的触发延迟上界，对齐 Dokploy 观感）。
const (
	DefaultScanInterval = 10 * time.Second
	// DefaultJobTimeout 是一次性 job 看门狗的平台缺省预算（10m；DT-4 起
	// 与 init 相位的 engine.DefaultJobTimeout 同源同值）。
	DefaultJobTimeout = engine.DefaultJobTimeout
)

// TriggerSource 是触发来源词表（cron_runs 无来源列——来源进 cron.triggered
// 事件 payload 与手动路径的审计）。
const (
	SourceSchedule = "system"
	SourceManual   = "manual"
)

// ErrNoSchedule 表示目标 app/service 不构成 cron schedule（服务不存在于
// 当前 compose，或未声明 fleetly.cron）——手动触发面的显式拒绝（API 层映
// 射 404 语义）。
var ErrNoSchedule = errors.New("no cron schedule for the given app/service")

// NodePreflight 是调度器对放置前哨的消费端口（placement.Resolver 隐式实
// 现；接口在本包定义以便单测注入——绑定节点不 ready 时返回
// E_PLACEMENT_NODE_UNAVAILABLE 信封，无绑定自由调度恒通过）。
type NodePreflight interface {
	Preflight(ctx context.Context, appID string) error
}

// Clock 是可注入时钟（触发点计算与看门狗单测的时间控制点；engine.Clock
// 同形）。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Config 是调度器参数（缺省回落平台默认）。
type Config struct {
	// ScanInterval 是扫描拍周期（缺省 10s）。
	ScanInterval time.Duration
	// DefaultJobTimeout 是看门狗缺省预算（label 未覆盖时；缺省 10m）。
	DefaultJobTimeout time.Duration
}

// Normalize 回落文档默认值。
func (c Config) Normalize() Config {
	if c.ScanInterval <= 0 {
		c.ScanInterval = DefaultScanInterval
	}
	if c.DefaultJobTimeout <= 0 {
		c.DefaultJobTimeout = DefaultJobTimeout
	}
	return c
}

// Manager 是 cron 调度器。零框架依赖——lynx.Service 装配壳在
// internal/runtime；手动触发经 api 面的端口接口消费。
type Manager struct {
	cfg       Config
	store     *state.Store
	box       *secrets.Box
	sub       engine.Substrate
	preflight NodePreflight
	clock     Clock
	log       *slog.Logger

	// mu 串行化触发链与对账拍（tick 与手动触发共享同一条链——重叠 skip
	// 与孤儿清扫的正确性前提：服务创建与台账行写入之间不并发扫描）。
	mu sync.Mutex

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	// firstBeat 报告下一拍是启动首拍（停机错过点披露只做一次——每拍披露
	// 会把同一缺口重复记账；重启重报一次属漏报劣于重复的既有语义）。
	firstBeat bool
}

// NewManager 构造调度器（preflight 为放置前哨端口，生产装配
// placement.Resolver）。
func NewManager(cfg Config, store *state.Store, box *secrets.Box, sub engine.Substrate,
	preflight NodePreflight, log *slog.Logger) *Manager {
	return &Manager{
		cfg:       cfg.Normalize(),
		store:     store,
		box:       box,
		sub:       sub,
		preflight: preflight,
		clock:     realClock{},
		log:       log,
		stop:      make(chan struct{}),
		firstBeat: true,
	}
}

// WithClock 注入时钟（单测）。
func (m *Manager) WithClock(c Clock) *Manager { m.clock = c; return m }

// Start 非阻塞启动扫描循环（启动即扫一拍——残留收口与错过点披露在首拍
// 完成，此后每拍周期驱动）。
func (m *Manager) Start(ctx context.Context) error {
	m.done = make(chan struct{})
	go m.loop(ctx)
	return nil
}

// Stop 停止扫描循环并等待退出（在途 job 的看门狗随拍停止——进程退出后
// 残留 job 由下一次启动首拍的收口兜底，台账不丢）。
func (m *Manager) Stop(_ context.Context) error {
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.done
	return nil
}

func (m *Manager) loop(ctx context.Context) {
	defer close(m.done)
	m.beat(ctx)
	ticker := time.NewTicker(m.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			m.beat(ctx)
		}
	}
}

// now 是时钟出口（单测注入）。
func (m *Manager) now() time.Time { return m.clock.Now() }

// schedule 是一个可触发的 cron schedule（声明 + 执行形态编译结果）。
type schedule struct {
	appID    string
	app      string
	service  string
	cs       compose.CronSchedule
	loc      *time.Location
	timeout  time.Duration
	next     cron.Schedule // robfig 解析结果（next-fire 计算）
	template engine.ServiceSpec
}

// beat 是一个扫描拍：残留与在途收口 → 错过点披露（首拍）→ 到点触发 →
// 在途完成检测。整体在 mu 内（与手动触发互斥）。
func (m *Manager) beat(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()

	scheds := m.assembleSchedules(ctx, nil)
	byKey := make(map[string]schedule, len(scheds))
	for _, s := range scheds {
		byKey[s.key()] = s
	}

	m.pollInFlight(ctx, byKey, now)
	m.sweepOrphanJobs(ctx, byKey)

	if m.firstBeat {
		m.discloseMissedDuringDowntime(ctx, scheds, now)
		m.firstBeat = false
	}
	for _, s := range scheds {
		// 到点判定以 now 为锚取下一触发点（窗口 = 上一拍至今）：窗口内的
		// 点触发一次，窗口外的过去点丢弃——错过点不补跑（架构 §4.3 策略行）。
		due := s.next.Next(now.Add(-m.cfg.ScanInterval))
		if due.IsZero() || due.After(now) {
			continue
		}
		if _, err := m.trigger(ctx, s, due, SourceSchedule, ""); err != nil {
			m.log.Warn("cron: trigger failed (retried next beat if transient)", "app", s.app, "service", s.service, "error", err)
		}
	}
}

// key 是 schedule 的台账键（app_id + compose 服务名）。
func (s schedule) key() string { return s.appID + "/" + s.service }

// TriggerRun 是手动触发入口（API/CLI 走同一触发链；调用方写审计之外的事
// 件与台账与到点触发完全同路径）。actorTokenID 记录调用方 token（审计归
// 因；空 = 未鉴权上下文）。重叠/节点不可用不报错——返回 skipped 行（与
// 到点触发的处置一致，「走同一路径」的字面兑现）。
func (m *Manager) TriggerRun(ctx context.Context, appID, appName, service, actorTokenID string) (state.CronRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scheds := m.assembleSchedules(ctx, &appID)
	for _, s := range scheds {
		if s.service != service {
			continue
		}
		return m.trigger(ctx, s, m.now().UTC(), SourceManual, actorTokenID)
	}
	return state.CronRun{}, fmt.Errorf("%w: %s/%s (the current compose declares no fleetly.cron schedule for this service)", ErrNoSchedule, appName, service)
}

// assembleSchedules 组装调度集（每拍现读）：active apps → 最新 succeeded
// 部署的 desired_spec 快照（Job 模板，E5 快照口径）+ 最新 revision 的
// compose 快照（fleetly.cron 声明）。onlyAppID 非 nil 时收窄到单 app（手动
// 触发面；不做在途部署过滤——手动触发不因部署过程失联）。组合缺一（无成
// 功部署 / 无 revision / 快照无该服务的 Job 模板）的 schedule 跳过并告警
// ——升级过渡态（旧部署快照无 Job 模板）在下次部署后自然就位。
func (m *Manager) assembleSchedules(ctx context.Context, onlyAppID *string) []schedule {
	apps, err := m.store.ListActiveApps(ctx)
	if err != nil {
		m.log.Warn("cron: list apps failed", "error", err)
		return nil
	}
	inFlight := map[string]bool{}
	if onlyAppID == nil {
		rows, err := m.store.ListNonTerminalDeployments(ctx)
		if err != nil {
			m.log.Warn("cron: list in-flight deployments failed", "error", err)
			return nil
		}
		for _, r := range rows {
			inFlight[r.AppID] = true
		}
	}
	var out []schedule
	for _, app := range apps {
		if onlyAppID == nil && inFlight[app.ID] {
			continue // 发布过程本身就是期望态迁移：以在途期望触发会竞态
		}
		// 挂起豁免（app Stop/Start，00028 位）：挂起 = 用户请求停止——周期
		// 触发不入队（手动触发的 API 面同门显式 409；此处双保险覆盖调度拍
		// 与直连入口）。恢复后随下一拍自然回集。
		if app.Suspended {
			continue
		}
		if onlyAppID != nil && app.ID != *onlyAppID {
			continue
		}
		scheds, err := m.loadAppSchedules(ctx, app)
		if err != nil {
			m.log.Warn("cron: assemble schedules failed", "app", app.Name, "error", err)
			continue
		}
		out = append(out, scheds...)
	}
	return out
}

// loadAppSchedules 组装单 app 的调度集（声明侧 = 最新 revision 的 compose
// 快照；执行侧 = 最新 succeeded 部署快照中的 Job 模板，按 fleetly.process
// label 对位）。
func (m *Manager) loadAppSchedules(ctx context.Context, app state.App) ([]schedule, error) {
	revs, err := m.store.ListRevisions(ctx, app.ID)
	if err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	if len(revs) == 0 {
		return nil, nil // 无成功部署：无声明快照
	}
	var spec compose.Spec
	if err := json.Unmarshal([]byte(revs[0].ComposeNormalized), &spec); err != nil {
		return nil, fmt.Errorf("decode compose snapshot of revision %s: %w", revs[0].ID, err)
	}
	templates, err := m.latestJobTemplates(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	var out []schedule
	for i := range spec.Services {
		svc := &spec.Services[i]
		if svc.Cron == nil {
			continue
		}
		tmpl, ok := templates[svc.Name]
		if !ok {
			m.log.Warn("cron: compose declares a schedule but the latest succeeded deployment carries no job template for it (redeploy to arm; pre-E5 snapshots lack job specs)",
				"app", app.Name, "service", svc.Name)
			continue
		}
		parsed, err := cron.ParseStandard(svc.Cron.Expression)
		if err != nil {
			m.log.Warn("cron: stored expression no longer parses (schedule skipped until the compose is fixed)",
				"app", app.Name, "service", svc.Name, "expression", svc.Cron.Expression, "error", err)
			continue
		}
		loc := time.UTC
		if svc.Cron.Timezone != "" {
			if l, lerr := time.LoadLocation(svc.Cron.Timezone); lerr == nil {
				loc = l
			}
		}
		timeout := m.cfg.DefaultJobTimeout
		if svc.Cron.Timeout != "" {
			if d, perr := time.ParseDuration(svc.Cron.Timeout); perr == nil && d > 0 {
				timeout = d
			}
		}
		out = append(out, schedule{
			appID:    app.ID,
			app:      app.Name,
			service:  svc.Name,
			cs:       *svc.Cron,
			loc:      loc,
			timeout:  timeout,
			next:     parsed,
			template: tmpl,
		})
	}
	return out, nil
}

// latestJobTemplates 取最新 succeeded 部署快照中的 Job 模板（按服务 label
// fleetly.process 对位；快照密文经 envelope 解密——与引擎 decodeSpecs 同源，
// 但保留 Job 项）。
func (m *Manager) latestJobTemplates(ctx context.Context, appID string) (map[string]engine.ServiceSpec, error) {
	rows, err := m.store.ListAppDeployments(ctx, appID, 25)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	for i := range rows {
		r := rows[i]
		if r.Status != state.DeploySucceeded || r.DesiredSpec == "" {
			continue
		}
		plain, err := m.box.Decrypt([]byte(r.DesiredSpec))
		if err != nil {
			return nil, fmt.Errorf("decrypt desired-spec %s: %w", r.ID, err)
		}
		var all []engine.ServiceSpec
		if err := json.Unmarshal(plain, &all); err != nil {
			return nil, fmt.Errorf("decode desired-spec %s: %w", r.ID, err)
		}
		out := map[string]engine.ServiceSpec{}
		for _, s := range all {
			if !s.Job {
				continue
			}
			if p := s.ServiceLabels[state.LabelProcess]; p != "" {
				out[p] = s
			}
		}
		return out, nil
	}
	return map[string]engine.ServiceSpec{}, nil
}

// trigger 是触发链（到点与手动共用）：同点去重 → 重叠 skip → 绑定节点前哨
// → 创建一次性 job 服务 → 台账行 started + cron.triggered 事件（手动路径同
// 事务追加审计 cron.manual_triggered）。服务创建失败向上透传（瞬态下一拍
// 重试/手动路径给调用方错误信封），不落行。
func (m *Manager) trigger(ctx context.Context, s schedule, scheduledAt time.Time, source, actorTokenID string) (state.CronRun, error) {
	// 同点去重（先于重叠）：该 schedule 的最新台账行已在本点或更晚（拍间
	// 窗口重叠重判、任务在拍内收口后的二次触发判定）→ 静默跳过——cron 点
	// 至多触发一次，重复行只会稀释 20 条留存窗。手动触发（scheduled_at =
	// now，不与 cron 点重合）不走此门。
	if last, ok, err := m.store.LatestCronRun(ctx, s.appID, s.service); err != nil {
		return state.CronRun{}, fmt.Errorf("point dedup check: %w", err)
	} else if ok && source == SourceSchedule && !last.ScheduledAt.Before(scheduledAt) {
		return last, nil
	}

	if _, ok, err := m.store.LatestInFlightCronRun(ctx, s.appID, s.service); err != nil {
		return state.CronRun{}, fmt.Errorf("overlap check: %w", err)
	} else if ok {
		return m.recordSkip(ctx, s, scheduledAt, state.CronSkipOverlap, "")
	}

	// 绑定节点前哨（架构 §4.3 触发前哨行）：绑定存在且节点不可用即 skip，
	// 不创建 job（与控制面停机 skip 同型）。无卷未绑定的 app Preflight 恒
	// 通过——job 由 Swarm 默认放置（细则：有卷应用才继承绑定节点）。
	if err := m.preflight.Preflight(ctx, s.appID); err != nil {
		return m.recordSkip(ctx, s, scheduledAt, state.CronSkipNodeUnavailable, singleLine(err))
	}

	runID := ulid.Make().String()
	// v0.3 三段命名（rbac-teams §4.3 cron 行）：team/prj 段取自 Job 模板的
	// 服务 label（fleetly.team/fleetly.project——规划期随归属写入，模板与
	// app 行同源同构；快照是 cron 调度集的权威来源，二次查库反而可能读到
	// MoveApp 换派后的新归属而与在途模板错位）。
	jobName, err := naming.CronJobName(
		s.template.ServiceLabels[state.LabelTeam],
		s.template.ServiceLabels[state.LabelProject],
		s.app, s.service, runID)
	if err != nil {
		return state.CronRun{}, fmt.Errorf("job naming: %w", err)
	}
	// 一次性 job 执行形态由 engine.JobSpecFrom 统一克隆（DT-4 抽取：init
	// 相位共用同一形态约束——单副本/restart=none/label 收敛；cron 行为
	// 逐字保持）。
	job := engine.JobSpecFrom(s.template, jobName, map[string]string{state.LabelCronRun: runID})
	// 网络先行（对账路径同语义）：per-app 专属网络 + rustfs 牵线（E3-4）
	// 等模板引用的全部网络幂等确认。
	for _, n := range job.Networks {
		if err := m.sub.NetworkEnsure(ctx, n.Name); err != nil {
			return state.CronRun{}, fmt.Errorf("ensure network %s: %w", n.Name, err)
		}
	}
	if err := m.sub.ServiceCreate(ctx, job); err != nil {
		return state.CronRun{}, fmt.Errorf("create job service %s: %w", jobName, err)
	}

	now := m.now().UTC()
	run := state.CronRun{
		ID:          runID,
		AppID:       s.appID,
		Service:     s.service,
		Expression:  s.cs.Expression,
		ScheduledAt: scheduledAt.UTC(),
		StartedAt:   now,
		Status:      state.CronRunStarted,
		JobService:  jobName,
	}
	err = m.store.InTx(ctx, func(tx *state.Tx) error {
		if _, err := tx.CreateCronRun(ctx, run); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, state.Event{
			Name:    "cron.triggered",
			Subject: "app:" + s.app,
			Payload: state.DiffSummary(
				"app", s.app, "app_id", s.appID, "service", s.service,
				"run", runID, "job_service", jobName,
				"scheduled_at", scheduledAt.UTC().Format(time.RFC3339),
				"expression", s.cs.Expression, "source", source),
		}); err != nil {
			return err
		}
		if source == SourceManual {
			return tx.WriteAudit(ctx, state.AuditEntry{
				Actor:        "human",
				ActorTokenID: actorTokenID,
				Action:       "cron.manual_triggered",
				Target:       "app:" + s.appID,
				Result:       "ok",
				DiffSummary: state.DiffSummary(
					"app", s.app, "service", s.service, "run", runID, "job_service", jobName),
			})
		}
		return nil
	})
	if err != nil {
		// 行写失败：服务已成（孤儿）——本拍不重复触发（互斥拍内），孤儿由
		// 下一拍清扫回收；错误向上披露（手动路径可见）。
		return run, fmt.Errorf("record cron run %s: %w", runID, err)
	}
	m.log.Info("cron: triggered", "app", s.app, "service", s.service,
		"run", runID, "job_service", jobName, "scheduled_at", run.ScheduledAt.Format(time.RFC3339), "source", source)
	return run, nil
}

// recordSkip 落 skipped 行 + cron.skipped 事件（重叠/节点不可用/停机错过
// 点/启动打断四类；与触发链同一路径的显性化面）。
func (m *Manager) recordSkip(ctx context.Context, s schedule, scheduledAt time.Time, reason, detail string) (state.CronRun, error) {
	run := state.CronRun{
		AppID:       s.appID,
		Service:     s.service,
		Expression:  s.cs.Expression,
		ScheduledAt: scheduledAt.UTC(),
		Status:      state.CronRunSkipped,
		SkipReason:  reason,
	}
	err := m.store.InTx(ctx, func(tx *state.Tx) error {
		r, err := tx.CreateCronRun(ctx, run)
		if err != nil {
			return err
		}
		run = r
		payload := []any{
			"app", s.app, "app_id", s.appID, "service", s.service,
			"scheduled_at", scheduledAt.UTC().Format(time.RFC3339), "reason", reason,
		}
		if detail != "" {
			payload = append(payload, "detail", detail)
		}
		_, err = tx.AppendEvent(ctx, state.Event{
			Name:    "cron.skipped",
			Subject: "app:" + s.app,
			Payload: state.DiffSummary(payload...),
		})
		return err
	})
	if err != nil {
		return run, fmt.Errorf("record cron skip (%s): %w", reason, err)
	}
	m.log.Info("cron: trigger skipped", "app", s.app, "service", s.service,
		"reason", reason, "scheduled_at", run.ScheduledAt.Format(time.RFC3339), "detail", detail)
	return run, nil
}

// discloseMissedDuringDowntime 启动首拍对「上一 handled 点之后存在整点未
// 处理」的 schedule 披露一次 cron.skipped(missed_downtime)（设计 §8 调度
// 核抽取行：控制面重启后 next-fire 按当前时间重算，过去点丢弃——丢弃要留
// 痕；每个缺口只记首点）。无台账行（从未触发过）不构成缺口证据。
func (m *Manager) discloseMissedDuringDowntime(ctx context.Context, scheds []schedule, now time.Time) {
	cutoff := now.Add(-m.cfg.ScanInterval)
	for _, s := range scheds {
		last, ok, err := m.store.LatestCronRun(ctx, s.appID, s.service)
		if err != nil {
			m.log.Warn("cron: read latest run for missed-point disclosure", "app", s.app, "service", s.service, "error", err)
			continue
		}
		if !ok {
			continue
		}
		nextAfterHandled := s.next.Next(last.ScheduledAt)
		if nextAfterHandled.IsZero() || !nextAfterHandled.Before(cutoff) {
			continue // 上一点之后的首个触发点仍在本拍窗口内/未来：无缺口
		}
		if _, err := m.recordSkip(ctx, s, nextAfterHandled, state.CronSkipMissedDowntime, ""); err != nil {
			m.log.Warn("cron: record missed-during-downtime skip", "app", s.app, "service", s.service, "error", err)
		}
	}
}

// pollInFlight 在途完成检测（每拍；byKey 提供当前看门狗预算——schedule 已
// 消失的 run 回落平台缺省）：任务终态收口（complete→succeeded /
// failed|rejected|shutdown→failed）与看门狗超时收口（超预算删服务 + 行记
// timeout + cron.timed_out，FZ-4）。收口与事件同事务；服务删除先行（幂等，
// 失败由孤儿清扫兜底）。
func (m *Manager) pollInFlight(ctx context.Context, byKey map[string]schedule, now time.Time) {
	rows, err := m.store.ListInFlightCronRuns(ctx)
	if err != nil {
		m.log.Warn("cron: list in-flight runs failed", "error", err)
		return
	}
	for _, r := range rows {
		s, known := byKey[r.AppID+"/"+r.Service]
		timeout := m.cfg.DefaultJobTimeout
		if known {
			timeout = s.timeout
		}
		status, runErr := m.jobTaskVerdict(ctx, r)
		switch {
		case status == state.CronRunSucceeded:
			m.closeRun(ctx, r, s, status, "")
		case status == state.CronRunFailed:
			m.closeRun(ctx, r, s, status, runErr)
		case !r.StartedAt.IsZero() && now.Sub(r.StartedAt) > timeout:
			m.closeRun(ctx, r, s, state.CronRunTimeout,
				fmt.Sprintf("watchdog budget %s exceeded (fleetly.cron.timeout label or platform default)", timeout))
		}
	}
}

// jobTaskVerdict 判定在途 run 的任务侧结论：succeeded / failed（含原因）/
// 空串（仍在途——无任务、任务运行中或底座读失败〔下一拍重试〕）。判定
// 内核 = engine.JobTaskVerdict（DT-4 抽取，init 相位共用；词表映射保 cron
// 台账语义不变）。
func (m *Manager) jobTaskVerdict(ctx context.Context, r state.CronRun) (string, string) {
	verdict, reason := engine.JobTaskVerdict(ctx, m.sub, r.JobService)
	switch verdict {
	case engine.JobVerdictSucceeded:
		return state.CronRunSucceeded, ""
	case engine.JobVerdictFailed:
		return state.CronRunFailed, reason
	}
	return "", ""
}

// closeRun 收口一条在途 run：删 job 服务（幂等；失败只告警——孤儿清扫兜
// 底）→ 行置终态 + cron.succeeded/failed/timed_out 事件同事务。行已被并
// 发收口（启动残留收口竞态）按 no-op 处置。
func (m *Manager) closeRun(ctx context.Context, r state.CronRun, s schedule, status, runErr string) {
	if err := m.sub.ServiceRemove(ctx, r.JobService); err != nil {
		m.log.Warn("cron: remove finished job service failed (orphan sweep will retry)",
			"job_service", r.JobService, "error", err)
	}
	appName := s.app
	if appName == "" {
		// schedule 已消失（cron 声明移除/app 变更）：按行归属反查应用名，
		// 事件主体保持 app:<name> 可读形态。
		if app, aerr := m.store.GetAppByID(ctx, r.AppID); aerr == nil {
			appName = app.Name
		} else {
			appName = r.AppID
		}
	}
	err := m.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.FinishCronRun(ctx, r.ID, status, runErr, m.now().UTC()); err != nil {
			return err
		}
		name := "cron.failed"
		switch status {
		case state.CronRunSucceeded:
			name = "cron.succeeded"
		case state.CronRunTimeout:
			name = "cron.timed_out"
		}
		payload := []any{
			"app", appName, "app_id", r.AppID, "service", r.Service,
			"run", r.ID, "job_service", r.JobService,
		}
		if runErr != "" {
			payload = append(payload, "error", runErr)
		}
		_, err := tx.AppendEvent(ctx, state.Event{
			Name:    name,
			Subject: "app:" + appName,
			Payload: state.DiffSummary(payload...),
		})
		return err
	})
	if err != nil {
		if errors.Is(err, state.ErrCronRunNotStarted) {
			return // 并发收口竞态：幂等收敛
		}
		m.log.Warn("cron: finish run failed", "run", r.ID, "error", err)
		return
	}
	m.log.Info("cron: run closed", "app", appName, "service", r.Service, "run", r.ID, "status", status, "error", runErr)
}

// sweepOrphanJobs 清扫孤儿 job 服务：受管服务中带 fleetly-cron- 前缀、且
// 不在任何在途行的服务 = 触发链半程残留（行写失败/进程在创建与建行间崩
// 溃/收口删服务失败）。幂等；失败下一拍重试。
func (m *Manager) sweepOrphanJobs(ctx context.Context, byKey map[string]schedule) {
	rows, err := m.store.ListInFlightCronRuns(ctx)
	if err != nil {
		m.log.Warn("cron: list in-flight runs for orphan sweep failed", "error", err)
		return
	}
	inflight := make(map[string]bool, len(rows))
	for _, r := range rows {
		inflight[r.JobService] = true
	}
	services, err := m.sub.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
	})
	if err != nil {
		m.log.Warn("cron: list services for orphan sweep failed", "error", err)
		return
	}
	for _, svc := range services {
		if !naming.IsCronJobName(svc.Name) || inflight[svc.Name] {
			continue
		}
		if err := m.sub.ServiceRemove(ctx, svc.Name); err != nil {
			m.log.Warn("cron: remove orphan job service failed", "service", svc.Name, "error", err)
			continue
		}
		app := "unknown"
		if s, ok := byKey[svc.Labels[state.LabelApp]+"/"+svc.Labels[state.LabelProcess]]; ok {
			app = s.app
		}
		m.log.Info("cron: orphan job service removed", "service", svc.Name, "app", app)
	}
}

// jobSpecFrom / errOrText 已抽取到 engine（DT-4 共享原语）：形态克隆 =
// engine.JobSpecFrom（cron 调用点传 fleetly.cron.run 运行锚），任务判定 =
// engine.JobTaskVerdict（词表映射见 jobTaskVerdict）。抽取时 cron 行为逐字
// 保持——本注释保留调用关系索引，防再次分叉出第二份实现。

// singleLine 是事件 payload 的错误单行化（禁换行——事件 JSON 脱敏契约，
// engine.errMessageForEvent 同口径）。
func singleLine(v any) string {
	msg := fmt.Sprint(v)
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
