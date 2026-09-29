package gitserver

// D1（S17 类 D：超时与取消闭环）——webhook 最小异步化：受理与执行分离。
// 背景：GitHub webhook 投递有 10s 硬超时，原同步链「拉源(FetchRemote) →
// 入队(DeployFromCommit)」全程绑 r.Context()——大仓库 fetch 必被客户端
// 掐断，且 GitHub 对 5xx 无限重投，形成永久失败循环。裁决：
//   - 安全面（验签/时间窗/重放占坑/分支过滤/去重）保持在响应前同步完成
//     （ServeHTTP 不变），受理即回 202 {status:"accepted"}；
//   - fetch + 入队移交本文件的后台 worker：带界队列（chan 32，满则受理侧
//     503 + 撤坑——背压可见），单 worker 串行消费，per-item 预算
//     context.WithTimeout(Background, 30min)（不绑请求 ctx，也不绑服务
//     ctx——停机排空语义见 loop）；
//   - 结果披露走事件流与审计（官方不会重投 202）：fetch 失败 → 事件
//     app.webhook_fetch_failed + 审计 + 撤坑（同 delivery ID 的手动
//     redeliver 可重试）；入队拒绝沿用既有审计路径；成功即既有
//     deployment.queued 事件。
//   - X-6/MG-4（B6）停机披露：排空预算尽（在处理项未返回）时，队列中
//     **尚未开始处理** 的 job 落披露审计（app.webhook_interrupted，记
//     delivery id 与 sha）+ 撤坑后确定性丢弃——停机丢失从「静默」变
//     「可对账 + 可手动 redeliver」。预算内排空则零噪音。

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// webhookQueueCapacity 是受理队列的硬上界（D1）：满则受理侧 503——背压
// 可见（v0.1 常量：32 足够吸收瞬时重投峰，worker 串行消费）。
const webhookQueueCapacity = 32

// webhookJobTimeout 是单个后台任务的执行预算（D1）：30 分钟上界覆盖最慢
// 的首次大仓库 fetch；v0.1 常量，配置面随实际需要引入。
const webhookJobTimeout = 30 * time.Minute

// webhookDrainBudget 是停机排空的预算上界（X-6/MG-4）：必须显著小于 lynx
// StopTimeout（缺省 5s——超时后 lynx 放弃等待 Stop、进程退出杀死一切），
// 保证披露审计在进程死亡前落库。包级 var 形态供测试缩短（同
// substrate.defaultCallTimeout 先例），生产语义即常量。
var webhookDrainBudget = 3 * time.Second

// webhookJob 是受理后移交后台执行的投递项（安全面校验已在 ServeHTTP
// 全部通过；此结构不含 secret/body 等敏感材料）。
type webhookJob struct {
	appID      string
	app        string
	deliveryID string
	sha        string
	ref        string
}

// webhookRunner 承载 webhook 异步执行面（带界队列 + 单 worker + idle 等待
// 点）。零值不可用，由 NewGitTriggers 经 init 构造；生命周期由服务壳驱动
// （cmd/fleetlyd git_service 的 lynx Start/Stop → StartWebhookWorker/
// StopWebhookWorker）。
type webhookRunner struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    chan webhookJob
	pending  int // 已受理未完成的任务数（idle 判定 = pending == 0）
	stopping atomic.Bool
	done     chan struct{} // worker 排空退出信号（nil = 未启动）
	// shutdown 是 Stop 的显式排水信号：lynx 停机序是「先 Stop 后 cancel
	// 服务 ctx」（服务 ctx 特意不继承取消信号，lynx.go 注释明示）——join
	// 若依赖 ctx 取消解除，就构成 Stop 等看门狗、看门狗等 cancel、cancel
	// 等 Stop 返回的循环等待，只能靠 lynx StopTimeout（5s）打破（SIGTERM
	// 优雅停机非零退出的实爆根因，smoke 断言 5 揪出）。
	shutdown     chan struct{}
	drainBudget  time.Duration
	watchdogDone chan struct{} // 看门狗退出信号（nil = 未启动）
}

// init 构造队列与 idle 条件变量。
func (r *webhookRunner) init() {
	r.queue = make(chan webhookJob, webhookQueueCapacity)
	r.cond = sync.NewCond(&r.mu)
	r.shutdown = make(chan struct{})
	r.drainBudget = webhookDrainBudget
}

// start 启动 worker（幂等：已启动 no-op——服务壳单次启停纪律）。worker
// 生命周期绑 ctx：ctx 取消进入停机排空（先立排水位拒绝新任务，再处理完
// 队列既有项——在处理项的 per-item 预算独立于 ctx，不被取消打断；排空
// 预算尽时剩余队列转披露丢弃，见 drain）。disclose 是预算尽路径的披露
// 写入器（GitTriggers.webhookAuditInterrupted——审计 + 撤坑）。
func (r *webhookRunner) start(ctx context.Context, process, disclose func(webhookJob)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil {
		return
	}
	done := make(chan struct{})
	r.done = done
	watchdogDone := make(chan struct{})
	r.watchdogDone = watchdogDone
	go r.loop(ctx, done, watchdogDone, process, disclose)
}

// stop 主动触发排空并等待 worker 退出（lynx 停机序里 Stop 先于服务 ctx
// cancel——收口不依赖 ctx 取消，见 shutdown 字段注记）；ctx 先到则让位返回
// （后台排空继续，进程退出兜底）。未启动为 no-op。看门狗 goroutine 同属
// join 面：主循环正常收口（done 关闭 = 无在处理项且队列已空）时它立即退出
//（免睡扑空）；预算尽路径它至多再存活一个 drainBudget。
func (r *webhookRunner) stop(ctx context.Context) error {
	r.mu.Lock()
	done := r.done
	watchdogDone := r.watchdogDone
	if done != nil {
		select {
		case <-r.shutdown:
		default:
			close(r.shutdown)
		}
	}
	r.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-watchdogDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// accept 把受理的投递入队（D1 受理侧唯一入口）：停机排水期或队列满 →
// false（调用方 503 + 撤坑）。X-6：stopping 检查与入队（buffered chan
// 非阻塞发送）在 r.mu 的同一临界区内完成——与 loop 的排水位置位、
// discardQueued 的清队列段互斥，杜绝「受理侧已过检查、排空已结束」的
// 遗留窗口（该窗口会把 job 留在无人消费的队列里且无任何披露）。
func (r *webhookRunner) accept(job webhookJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping.Load() {
		return false
	}
	select {
	case r.queue <- job:
		// pending 在同一临界区内自增：waitIdle 的同步点因此覆盖「202 已回
		// 但 worker 尚未拾起」的窗口。
		r.pending++
		return true
	default:
		return false
	}
}

// waitIdle 阻塞至已受理任务全部处理完成（测试同步点——替代 sleep 轮询，
// 消除竞态；生产路径不调用）。
func (r *webhookRunner) waitIdle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.pending > 0 {
		r.cond.Wait()
	}
}

// loop 是 worker 主循环：运行期消费队列；ctx 取消后在锁内立排水位（与
// accept 的检查-入队段互斥，X-6）再排空剩余项，close(done) 是
// StopWebhookWorker 的等待点。
//
// X-6/MG-4：停机看门狗随 ctx 取消**立即**武装（与在处理项并发）——排空
// 语义不能等在处理项返回：其 per-item 预算最长 30min，而 lynx StopTimeout
// （缺省 5s）放弃等待后进程退出，队列剩余项将无人消费也无人披露。预算尽
// 时看门狗自行置排水位并把剩余队列转披露丢弃（shutdownDiscard）；预算内
// 在处理项返回则主循环进入 drain 正常排空，看门狗届时扑空（队列已空，
// 零噪音）。
func (r *webhookRunner) loop(ctx context.Context, done, watchdogDone chan struct{}, process, disclose func(webhookJob)) {
	defer close(done)
	go func() {
		defer close(watchdogDone)
		// 主循环正常收口（done 关闭）= 无在处理项且队列已空：看门狗无
		// 物可弃，立即退出（免睡 drainBudget 的扑空等待）。非阻塞优先探
		// 测防 select 随机性把已就绪的 done 错过。
		select {
		case <-done:
			return
		default:
		}
		select {
		case <-ctx.Done():
		case <-r.shutdown:
		case <-done:
			return
		}
		time.Sleep(r.drainBudget)
		r.shutdownDiscard(disclose)
	}()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.stopping.Store(true)
			r.mu.Unlock()
			r.drain(process)
			return
		case <-r.shutdown:
			r.mu.Lock()
			r.stopping.Store(true)
			r.mu.Unlock()
			r.drain(process)
			return
		case job := <-r.queue:
			r.run(process, job)
		}
	}
}

// drain 排空队列既有项（ctx 取消且在处理项已返回的主循环路径；排水位已
// 在锁内立起——accept 不再放行，队列只减不增）。
func (r *webhookRunner) drain(process func(webhookJob)) {
	for {
		select {
		case job := <-r.queue:
			r.run(process, job)
		default:
			return
		}
	}
}

// shutdownDiscard 是停机预算尽的终局处置（X-6 看门狗路径）：排水位置位
// （后续受理 503）+ 队列剩余项披露丢弃。与在处理项并发执行——只动队列
// 内容，不触碰在处理项（channel 接收语义保证每个 job 恰到一个消费者）。
func (r *webhookRunner) shutdownDiscard(disclose func(webhookJob)) {
	r.mu.Lock()
	r.stopping.Store(true)
	r.discardQueuedLocked(disclose)
	r.mu.Unlock()
}

// discardQueuedLocked 披露并丢弃队列剩余项（调用方须持 r.mu）：accept 的
// 「stopping 检查 + 发送」在同一把锁的临界区内——清空时刻之后到达的受理
// 已被 stopping 拒绝，清空时刻之前通过的受理必然已在队列内，不会漏披露、
// 也不会双重处置。
func (r *webhookRunner) discardQueuedLocked(disclose func(webhookJob)) {
	for {
		select {
		case job := <-r.queue:
			disclose(job)
			r.pending--
			if r.pending == 0 {
				r.cond.Broadcast()
			}
		default:
			return
		}
	}
}

// run 包裹单任务执行：完成后扣 pending，归零时广播唤醒 waitIdle。
func (r *webhookRunner) run(process func(webhookJob), job webhookJob) {
	defer func() {
		r.mu.Lock()
		r.pending--
		if r.pending == 0 {
			r.cond.Broadcast()
		}
		r.mu.Unlock()
	}()
	process(job)
}

// StartWebhookWorker 启动 webhook 后台 worker（D1）：随 git 服务壳的
// lynx Start 启动（webhook 面是 gateway 原生端点，服务壳恒承担 worker
// 生命周期）；ctx 取消后 worker 排空队列退出（排空
// 预算尽时剩余项转披露丢弃，X-6——见 webhookAuditInterrupted），
// StopWebhookWorker 是排空完成等待点。
func (s *GitTriggers) StartWebhookWorker(ctx context.Context) {
	s.hooks.start(ctx, s.runWebhookJob, s.webhookAuditInterrupted)
}

// StopWebhookWorker 等待 worker 排空退出（D1）；未启动为 no-op。
func (s *GitTriggers) StopWebhookWorker(ctx context.Context) error {
	return s.hooks.stop(ctx)
}

// runWebhookJob 执行单个受理投递（原同步链的拉源+入队两步原样移入）：
// per-item 预算独立于请求与服务 ctx——D1 的要点（fetch 不再被 GitHub
// 10s 投递超时掐断，停机排空也不打断在处理项）。
func (s *GitTriggers) runWebhookJob(job webhookJob) {
	ctx, cancel := context.WithTimeout(context.Background(), webhookJobTimeout)
	defer cancel()

	// 拉源（fetch 失败 → 披露三件套，见 discloseFetchFailure）。
	if err := s.FetchRemote(ctx, job.app); err != nil {
		if !errors.Is(err, ErrFetchFailed) {
			// 非 ErrFetchFailed 形态（理论不可达：FetchRemote 全路径包装）：
			// 原文只进日志（B2 口径）。
			s.log.Warn("gitserver: webhook fetch failed with unexpected error", "app", job.app, "error", err.Error())
		}
		s.discloseFetchFailure(ctx, job)
		return
	}

	rec, _, err := s.DeployFromCommit(ctx, job.app, job.sha, job.ref)
	if err != nil {
		// 判重命中（M3-4 竞态兜底）：终局与受理侧 duplicate 回执同语义
		// ——落 duplicate 处置审计、保持已占坑（4xx 类确定性终局，非 5xx
		// 不撤坑——同 delivery ID 的官方重投不该再触发一轮 fetch）。
		if errors.Is(err, state.ErrDuplicateGitDeployment) {
			s.webhookAuditOutcome(ctx, job.appID, job.app, job.deliveryID, "duplicate", job.sha)
			s.log.Info("gitserver: webhook deployment deduplicated (concurrent delivery)",
				"app", job.app, "sha", job.sha, "delivery", job.deliveryID)
			return
		}
		// 与原同步路径同语义：审计 rejected；仅 5xx 形态撤坑（4xx 校验拒绝
		// 如 compose 词形错是确定性终局，保持已占坑）。异步后无 HTTP 回执
		// 可写——本审计即结果披露（事件面沿用既有 deployment.* 词汇，compose
		// 拒绝无专门事件）。
		s.webhookAuditErr(ctx, job.appID, job.app, job.deliveryID, "deploy rejected: "+err.Error())
		status := http.StatusInternalServerError
		var ae *apperr.Error
		if errors.As(err, &ae) {
			status = ae.HTTPStatus()
		}
		if status >= http.StatusInternalServerError {
			s.replay.Unmark(job.deliveryID)
		}
		return
	}
	s.log.Info("gitserver: webhook deployment enqueued",
		"app", job.app, "sha", job.sha, "ref", job.ref, "deployment", rec.ID)
}

// discloseFetchFailure 落拉源失败披露三件套（D1）：事件
// app.webhook_fetch_failed + 审计（detail 只记错误码与阶段——B2 出站字节
// 口径）+ 撤坑（同 delivery ID 的官方 redeliver 可重试；受理期已回 202，
// 失败只能走事件流披露——官方不会自动重投 202）。
func (s *GitTriggers) discloseFetchFailure(ctx context.Context, job webhookJob) {
	payload := state.DiffSummary("app", job.app, "delivery", job.deliveryID,
		"sha", job.sha, "code", "E_RUNTIME_UNAVAILABLE", "stage", "fetch")
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.AppendEvent(ctx, state.Event{
			Name:    "app.webhook_fetch_failed",
			Subject: "app:" + job.appID,
			Payload: payload,
		})
		return err
	}); err != nil {
		s.log.Warn("gitserver: webhook fetch-failed event write failed", "app", job.app, "error", err.Error())
	}
	s.webhookAuditErr(ctx, job.appID, job.app, job.deliveryID, "fetch failed: code=E_RUNTIME_UNAVAILABLE, stage=fetch")
	s.replay.Unmark(job.deliveryID)
}

// webhookAuditOutcome 写 webhook 非错误终局的处置审计（M3-4 起 worker 侧
// 异步 duplicate 终局共用；action=app.webhook_accepted、result=ok，outcome
// 进 diff——与 handler 同步路径 h.audit 同词形，两条路径在审计面可分）。
func (s *GitTriggers) webhookAuditOutcome(ctx context.Context, appID, app, deliveryID, outcome, detail string) {
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      "app.webhook_accepted",
			Target:      "app:" + appID,
			Result:      "ok",
			DiffSummary: auditDiff(app, deliveryID, outcome, detail),
		})
	}); err != nil {
		s.log.Warn("gitserver: webhook audit write failed", "app", app, "error", err.Error())
	}
}

// webhookAuditErr 写 webhook 拒绝审计（handler 同步拒绝路径与 worker 异步
// 失败路径共用；result=error，action 词根独立可分）。
func (s *GitTriggers) webhookAuditErr(ctx context.Context, appID, app, deliveryID, detail string) {
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      "app.webhook_rejected",
			Target:      "app:" + appID,
			Result:      "error",
			DiffSummary: auditDiff(app, deliveryID, "rejected", detail),
		})
	}); err != nil {
		s.log.Warn("gitserver: webhook audit write failed", "app", app, "error", err.Error())
	}
}

// webhookAuditInterrupted 是停机排空预算尽路径的披露写入器（X-6/MG-4）：
// 对**仍在队列、尚未开始处理**的已受理 job 落审计（action=
// app.webhook_interrupted，result=error，diff 记 delivery id 与 sha——与
// app.webhook_accepted 的 detail=sha 同口径，受理-中断两条审计可按
// delivery id 对账）+ 撤坑（官方 redeliver 链保持通，手动重投即恢复）。
// 与本包既有 webhook 审计风格一致：受理/拒绝只入审计，事件面留给有独立
// 语义的异步结果（app.webhook_fetch_failed）——不新增事件码。
func (s *GitTriggers) webhookAuditInterrupted(job webhookJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      "app.webhook_interrupted",
			Target:      "app:" + job.appID,
			Result:      "error",
			DiffSummary: auditDiff(job.app, job.deliveryID, "interrupted", job.sha),
		})
	}); err != nil {
		// 披露自身失败只进日志（进程退出在即，无重试点）；此时撤坑仍执行
		// ——redeliver 链保持通是可恢复性的下限。
		s.log.Warn("gitserver: webhook interrupted audit write failed",
			"app", job.app, "delivery", job.deliveryID, "error", err.Error())
	}
	s.replay.Unmark(job.deliveryID)
}
