package build

// 构建队列（T2.8）：CLI 入队（builds 行，跨进程通道）→ daemon 队列 worker
// 扫描认领执行。并发上限经 semaphore 承载（架构 §4.2 容量边界「并发构建
// 2」；配置上限天花板 MaxConcurrency）。排队可见性 = builds 行 status=queued
// （fleetly builds list）；认领（queued→building）是行级谓词原子操作，多
// worker/多 daemon 竞争下恰好一个 claimer 胜出。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// errCodeBuildFailed 是构建失败终态的注册表错误码（internal/errcode 既有
// 项——中断复位/超时兜底路径复用该码、以审计 reason 文案区分场景，不新增
// 码避免注册表膨胀）。
const errCodeBuildFailed = "E_BUILD_FAILED"

// buildInterruptedReason 是启动复位 building 行的审计 reason 文案（崩溃/
// 关停遗留行的失败归因）。
const buildInterruptedReason = "build interrupted (daemon restart/shutdown)"

// Queue 是构建队列调度器（dispatcher 单 goroutine + 信号量限并发执行）。
type Queue struct {
	store        *state.Store
	exec         Executor
	sem          chan struct{}
	wake         chan struct{}
	pollInterval time.Duration
	timeout      time.Duration
	log          *slog.Logger
	// uploadsRoot 是上传构建会话根（IMPL-T2-2：收敛/复位路径清理
	// request.ephemeral_dir 的受根锚点；空 = 不清理——纯 compose 构建
	// 夹具形态零差异）。经 WithUploadsRoot 注入。
	uploadsRoot string
}

// NewQueue 构建队列。concurrency ≤0 回落缺省 2、pollInterval/timeout ≤0
// 回落各自缺省（Normalize 语义）。
func NewQueue(store *state.Store, exec Executor, concurrency int, pollInterval, timeout time.Duration, log *slog.Logger) *Queue {
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	if concurrency > MaxConcurrency {
		concurrency = MaxConcurrency
	}
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Queue{
		store:        store,
		exec:         exec,
		sem:          make(chan struct{}, concurrency),
		wake:         make(chan struct{}, 1),
		pollInterval: pollInterval,
		timeout:      timeout,
		log:          log,
	}
}

// Concurrency 返回并发上限（诊断用）。
func (q *Queue) Concurrency() int { return cap(q.sem) }

// WithUploadsRoot 注入上传构建会话根（IMPL-T2-2：收敛/复位路径的
// ephemeral_dir 清理锚点；链式装配，空串 = 关闭清理）。
func (q *Queue) WithUploadsRoot(root string) *Queue { q.uploadsRoot = root; return q }

// Enqueue 入队一条构建（builds queued 行 + 唤醒信号加速同进程拾取）。
// audit（可选，至多一枚）透传给 state 层 build.create 审计的调用方归因
// （M4-8：TriggerBuild 带 API 调用方 token；缺省 = system 归因，与内部
// 入队路径一致）。variadic 形态保持既有调用点签名兼容。
func (q *Queue) Enqueue(ctx context.Context, rec state.BuildRecord, audit ...state.AuditEntry) (state.BuildRecord, error) {
	var entry *state.AuditEntry
	if len(audit) > 0 {
		entry = &audit[0]
	}
	created, err := q.store.CreateBuildAs(ctx, rec, entry)
	if err != nil {
		return state.BuildRecord{}, fmtErr("enqueue build: %w", err)
	}
	q.Wake()
	return created, nil
}

// Wake 非阻塞唤醒扫描（通道容量 1，合并连续唤醒）。
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Run 是调度主循环（阻塞到 ctx 取消；lynx.Service.Start 的 actor 形态）。
// 启动先复位中断构建（daemon 重启恢复，见 resetInterrupted），再进入扫描。
func (q *Queue) Run(ctx context.Context) error {
	q.resetInterrupted(ctx)
	ticker := time.NewTicker(q.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-q.wake:
			q.drainOnce(ctx)
		case <-ticker.C:
			q.drainOnce(ctx)
		}
	}
}

// resetInterrupted 启动复位：上一进程崩溃/关停在途的 building 行收敛为
// failed 终态（无人接手的 building 行永不重跑——NextQueuedBuilds 只扫
// queued；等待它的部署在引擎侧空转到发布超时）。queued 行不动：重启后
// 队列自然重扫认领。复位失败只告警不阻塞调度（queued 流量不受影响，遗留
// 行待下次重启再试）。
//
// IMPL-T2-2：复位前先清理 building 行对应的上传会话目录（进程崩溃窗口的
// 残留；此时队列尚未进入扫描循环，无并发执行者）。
func (q *Queue) resetInterrupted(ctx context.Context) {
	if q.uploadsRoot != "" {
		if rows, err := q.store.ListNonTerminalBuilds(ctx); err != nil {
			q.log.Warn("scan non-terminal builds for upload cleanup", "error", err.Error())
		} else {
			for _, rec := range rows {
				if rec.Status == state.BuildBuilding {
					q.cleanupEphemeral(rec)
				}
			}
		}
	}
	n, err := q.store.ResetInterruptedBuilds(ctx, errCodeBuildFailed, buildInterruptedReason)
	if err != nil {
		q.log.Error("reset interrupted builds", "error", err)
		return
	}
	if n > 0 {
		q.log.Warn("reset interrupted builds to failed", "count", n, "reason", buildInterruptedReason)
	}
}

// cleanupEphemeral 尝试清理构建行的上传会话目录（best-effort：request
// 解码失败/非上传构建/越界形态静默跳过；删除失败只告警——janitor 的
// mtime 兜底仍会收尾）。
func (q *Queue) cleanupEphemeral(rec state.BuildRecord) {
	if q.uploadsRoot == "" {
		return
	}
	req, err := DecodeRequest(rec.Request)
	if err != nil || req.EphemeralDir == "" {
		return
	}
	if err := CleanupUploadDir(req.EphemeralDir, q.uploadsRoot); err != nil {
		q.log.Warn("cleanup ephemeral build context", "build", rec.ID, "dir", req.EphemeralDir, "error", err.Error())
	}
}

// drainOnce 一轮扫描：在信号量有空位时持续认领最早 queued 记录并派发；
// 信号量满或无待处理记录即返回（不阻塞调度循环）。
func (q *Queue) drainOnce(ctx context.Context) {
	for {
		select {
		case q.sem <- struct{}{}:
		default:
			return // 并发已满：排队中记录保持 queued（排队可见）
		}
		recs, err := q.store.NextQueuedBuilds(ctx, 1)
		if err != nil {
			<-q.sem
			if ctx.Err() != nil {
				return
			}
			q.log.Error("scan queued builds", "error", err)
			return
		}
		if len(recs) == 0 {
			<-q.sem
			return
		}
		rec := recs[0]
		if err := q.store.ClaimBuild(ctx, rec.ID); err != nil {
			<-q.sem
			if !errors.Is(err, state.ErrBuildStateTransition) {
				// 非 claim 竞争（库错误等）：停止本轮避免热循环。
				if ctx.Err() != nil {
					return
				}
				q.log.Error("claim build", "build", rec.ID, "error", err)
				return
			}
			continue // 被其他 worker 抢先：看下一条
		}
		claimed, err := q.store.GetBuild(ctx, rec.ID)
		if err != nil {
			<-q.sem
			q.log.Error("read claimed build", "build", rec.ID, "error", err.Error())
			// M2-6：认领后行读取失败（瞬时错误）不得滞留 building——此路径
			// 无执行 goroutine 也无失败兜底，行将滞留到重启（期间无人接手、
			// 等它的部署空转到发布超时）。直接收敛 failed 终态。
			q.convergeClaimedUnreadable(ctx, rec.ID)
			return
		}
		go func(claimed state.BuildRecord) {
			// M2-10：槽位释放与补位唤醒同在收尾 defer——先 <-sem 释放、再
			// Wake。原形态在持槽期（goroutine 启动时）发送 Wake：唤醒的
			// drainOnce 撞满信号量立即返回，是无效信号；真正的补位时机是
			// 槽位释放之后（完成一条立即补位，不再等 poll tick）。
			defer func() {
				<-q.sem
				q.Wake()
			}()
			// per-build 超时预算（config build.timeout_seconds，缺省 30min）：
			// 挂起的执行（网络挂起/buildkitd 半死）到点取消——信号量槽不再被
			// 永久占用（并发 2 时两个挂起即堵死全队列）。执行器契约：ctx
			// 取消即返回（buildkit 客户端随 ctx 终止 solve）。
			execCtx, cancel := context.WithTimeout(ctx, q.timeout)
			defer cancel()
			// M2-7：panic 边界（MG-1 同族——外部输入驱动的执行路径必须有
			// panic 边界，对齐 engine A9 先例）：builds.request 是跨进程输入，
			// 单条恶意/异常构建触发的 panic 不得打崩 fleetlyd 调度——记
			// Error（含栈）+ 兜底收敛 failed，调度继续。
			defer func() {
				if r := recover(); r != nil {
					q.log.Error("execute build panicked",
						"build", claimed.ID,
						"panic", fmt.Sprintf("%v", r),
						"stack", string(debug.Stack()))
					q.convergeStranded(execCtx, claimed.ID)
				}
			}()
			if _, err := q.exec.Execute(execCtx, claimed); err != nil {
				q.log.Error("execute build", "build", claimed.ID, "error", err.Error())
				q.convergeStranded(execCtx, claimed.ID)
			}
		}(claimed)
	}
}

// claimedUnreadableReason 是认领后行读取失败兜底收敛的审计归因（M2-6）。
const claimedUnreadableReason = "row read failed after build claim (transient error; converged without executing)"

// convergeClaimedUnreadable 收敛「已认领但行读取失败」的 building 行
// （M2-6）：FailStrandedBuild 的行级 CAS 保证行已并发离开 building（终态/
// 复位竞争）时跳过不误伤；终态写用 WithoutCancel——读取失败的诱因可能是
// ctx 层瞬时故障，兜底写必达。
func (q *Queue) convergeClaimedUnreadable(ctx context.Context, buildID string) {
	finCtx := context.WithoutCancel(ctx)
	err := q.store.FailStrandedBuild(finCtx, buildID, errCodeBuildFailed, claimedUnreadableReason)
	if err != nil && !errors.Is(err, state.ErrBuildStateTransition) {
		q.log.Error("converge claimed-unreadable build", "build", buildID, "error", err.Error())
		return
	}
	// IMPL-T2-2：读失败可能只是瞬态——重试一次读取用于清理上传会话
	// （行确已离开 building 的竞争形态同样尽力清理，幂等）。
	if rec, gerr := q.store.GetBuild(finCtx, buildID); gerr == nil {
		q.cleanupEphemeral(rec)
	}
	if err == nil {
		q.log.Warn("converged claimed-unreadable build to failed", "build", buildID, "reason", claimedUnreadableReason)
	}
}

// convergeStranded 兜底终态：执行器返回错误后行仍停留 building（超时取消
// 后未收敛、异常路径直接返回等）→ 队列侧收敛 failed，防 builds 行永久停留
// building。终态写用 WithoutCancel——execCtx 此时多半已取消（超时/关停），
// 兜底写必达（保留 trace/log 上下文，不继承取消与 deadline）。
func (q *Queue) convergeStranded(execCtx context.Context, buildID string) {
	finCtx := context.WithoutCancel(execCtx)
	row, err := q.store.GetBuild(finCtx, buildID)
	if err != nil {
		q.log.Error("read stranded build", "build", buildID, "error", err)
		return
	}
	// IMPL-T2-2：上传会话清理先于终态判定——执行器已收敛终态的路径
	// （Builder defer 已清理）重复调用无害；异常路径在此收口。
	q.cleanupEphemeral(row)
	if row.Status != state.BuildBuilding {
		return // 执行器已收敛终态（正常失败路径）
	}
	var reason string
	switch {
	case errors.Is(execCtx.Err(), context.DeadlineExceeded):
		reason = "build timed out: exceeded " + q.timeout.String() + " budget (config build.timeout_seconds)"
	case execCtx.Err() != nil:
		reason = "build interrupted (daemon shutdown; executor did not converge a terminal state)"
	default:
		reason = "build executor exited abnormally (no terminal state)"
	}
	if err := q.store.FailStrandedBuild(finCtx, buildID, errCodeBuildFailed, reason); err != nil {
		q.log.Error("converge stranded build", "build", buildID, "error", err)
		return
	}
	q.log.Warn("converged stranded build to failed", "build", buildID, "reason", reason)
}
