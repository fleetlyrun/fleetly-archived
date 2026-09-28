package logs

// hub 直推批量器（E6 观测专项设计 §2.3，W5-S1）：脱敏之后的行入湖
// VictoriaLogs（ES bulk 传输面由 IngestBackend 端口承载——实现 =
// victorialogs.Backend，方向纪律：logs 定义端口、组件包实现，logs 不反向
// 感知 VL）。
//
// 批量纪律（设计 §2.3）：2s 或 512 行先到先发；内存队列上限 8192 行，
// 溢出丢最旧 + 计数（直播面零影响——ring/fan-out 不经过批量器，VL 故障
// 不破坏 FollowLogs，A3 直读不动条款的结构性兑现）。
//
// 失败诚实面（设计 §2.3）：VL 不可达 → streak 进入/退出各发一事件
//（logs.ingest_degraded / logs.ingest_recovered，去抖 = 状态翻转才发，
// 不逐行红、不静默）+ 丢弃计数常驻暴露（system status 组件 victorialogs
// 与 CLI logs backend show 消费）。
//
// 脱敏纪律（state-model §2.9 延续）：进本批量器的行必须已脱敏——容器行
// 在 pollStream 的 redact（appSink）之后接入；build 行在 IngestBuildLine
// 内过同一 redactor。批量器自身不做二次脱敏（单一管线纪律）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// 批量纪律常量（设计 §2.3 逐字；测试按此钉住）。
const (
	// flushInterval 是时间触发的 flush 周期。
	flushInterval = 2 * time.Second
	// flushLines 是行数触发的 flush 阈值。
	flushLines = 512
	// queueCapacity 是内存队列上限（溢出丢最旧）。
	queueCapacity = 8192
	// flushBudget 是单次 flush 的失败预算（不阻塞采集主循环——batcher
	// goroutine 自有；失败按 streak 面披露）。
	flushBudget = 5 * time.Second
)

// IngestBackend 是入湖传输端口（实现 = victorialogs.Backend.IngestBulk；
// 单测注入 fake）。rows 全部为已脱敏行。
type IngestBackend interface {
	IngestBulk(ctx context.Context, entries []Entry) error
}

// Ingester 是日志入湖批量器（Manager 持有；Run 承载 flush 循环）。
type Ingester struct {
	backend IngestBackend
	st      *state.Store
	log     *slog.Logger
	clock   func() time.Time

	// eventFn 是 streak 翻转事件的发出缝（默认 = store.Outbox 事务；
	// 单测注入替换——事件名与 payload 形态仍由生产路径钉住）。
	eventFn func(ctx context.Context, name string, payload map[string]string)

	mu      sync.Mutex
	pending []Entry
	dropped uint64
	// failing 是 streak 当前态（true = VL 不可达降级中）。
	failing bool
	// failStreakSince 是当前降级 streak 的起点（去抖窗口的展示面）。
	failStreakSince time.Time
	// wake 唤醒 flush 循环（行数阈值触发/立即重试）。
	wake chan struct{}
}

// NewIngester 构造批量器（不启动循环；Manager.Run 拉起）。
func NewIngester(backend IngestBackend, st *state.Store, log *slog.Logger) *Ingester {
	if log == nil {
		log = discardLogger()
	}
	return &Ingester{
		backend: backend,
		st:      st,
		log:     log,
		clock:   time.Now,
		eventFn: func(ctx context.Context, name string, payload map[string]string) {
			emitEventTx(ctx, st, name, payload, log)
		},
		wake: make(chan struct{}, 1),
	}
}

// WithClock 覆盖时钟（测试注入）。
func (i *Ingester) WithClock(f func() time.Time) *Ingester { i.clock = f; return i }

// Add 入队一条已脱敏行（溢出丢最旧 + 计数——直播面零影响的关键形态：
// 本方法永不阻塞、永不报错）。达到行数阈值即唤醒 flush。
func (i *Ingester) Add(e Entry) {
	i.mu.Lock()
	if len(i.pending) >= queueCapacity {
		// 溢出：丢最旧（队头）保新行——检索面的近期可见性优先。
		copy(i.pending, i.pending[1:])
		i.pending[len(i.pending)-1] = e
		i.dropped++
		n := len(i.pending)
		i.mu.Unlock()
		if n >= flushLines {
			i.signal()
		}
		return
	}
	i.pending = append(i.pending, e)
	n := len(i.pending)
	i.mu.Unlock()
	if n >= flushLines {
		i.signal()
	}
}

// signal 非阻塞唤醒 flush 循环（已有未消费信号则合并）。
func (i *Ingester) signal() {
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

// Degraded 报告 ingest streak 是否降级中（system status 组件面）。
func (i *Ingester) Degraded() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.failing
}

// StreakSince 返回当前降级 streak 的起点（零值 = 未降级）。
func (i *Ingester) StreakSince() time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.failStreakSince
}

// DroppedTotal 返回进程启动以来溢出丢弃的累计行数（诚实面的常驻计数）。
func (i *Ingester) DroppedTotal() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.dropped
}

// Pending 返回当前在队行数（观测面）。
func (i *Ingester) Pending() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.pending)
}

// Run 承载 flush 循环（ctx 取消即退出；退出前 best-effort 排空一次——
// 关停时在途行尽力入湖，失败不阻塞关停）。
func (i *Ingester) Run(ctx context.Context) error {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			i.drain(context.WithoutCancel(ctx))
			return nil
		case <-ticker.C:
			i.flushTick(ctx)
		case <-i.wake:
			i.flushTick(ctx)
		}
	}
}

// flushTick 执行一拍 flush（空队列 no-op；失败保持 pending 并按 streak
// 面披露——不重试风暴，下一拍/下一信号再试）。
func (i *Ingester) flushTick(ctx context.Context) {
	i.mu.Lock()
	if len(i.pending) == 0 {
		i.mu.Unlock()
		return
	}
	batch := i.pending
	i.pending = nil
	i.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, flushBudget)
	err := i.backend.IngestBulk(fctx, batch)
	cancel()
	if err == nil {
		i.recordSuccess()
		return
	}
	i.recordFailure(ctx, err, batch)
}

// drain 关停前 best-effort 排空（短预算；失败丢弃——进程退出语义）。
func (i *Ingester) drain(ctx context.Context) {
	i.mu.Lock()
	if len(i.pending) == 0 {
		i.mu.Unlock()
		return
	}
	batch := i.pending
	i.pending = nil
	i.mu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, flushBudget)
	defer cancel()
	if err := i.backend.IngestBulk(dctx, batch); err != nil {
		i.mu.Lock()
		i.dropped += uint64(len(batch))
		i.mu.Unlock()
		i.log.Warn("logs: final ingest flush failed on shutdown (dropped)",
			"lines", len(batch), "error", err.Error())
	}
}

// recordSuccess 记录成功（streak 退出沿 → logs.ingest_recovered，去抖：
// 仅状态翻转才发）。
func (i *Ingester) recordSuccess() {
	i.mu.Lock()
	wasFailing := i.failing
	i.failing = false
	i.failStreakSince = time.Time{}
	i.mu.Unlock()
	if wasFailing {
		i.log.Info("logs: ingest to the log backend recovered (streak exited)")
		i.eventFn(context.Background(), "logs.ingest_recovered", map[string]string{})
	}
}

// recordFailure 记录失败（streak 进入沿 → logs.ingest_degraded，去抖同上；
// 失败批回灌队头——最旧行在溢出时最先再被丢弃，保近期可见性的口径不变）。
func (i *Ingester) recordFailure(ctx context.Context, err error, batch []Entry) {
	i.mu.Lock()
	justEntered := !i.failing
	if justEntered {
		i.failing = true
		i.failStreakSince = i.clock()
	}
	// 回灌队头：失败批先行重试；溢出从队头丢最旧（含回灌批的头部）。
	re := append([]Entry{}, batch...)
	re = append(re, i.pending...)
	if len(re) > queueCapacity {
		i.dropped += uint64(len(re) - queueCapacity)
		re = re[len(re)-queueCapacity:]
	}
	i.pending = re
	dropped := i.dropped
	i.mu.Unlock()

	if justEntered {
		i.log.Warn("logs: ingest to the log backend degraded (streak entered; live tail unaffected)",
			"error", err.Error())
		i.eventFn(ctx, "logs.ingest_degraded", map[string]string{
			"dropped_total": fmt.Sprint(dropped),
			"error":         sanitizeIngestError(err.Error()),
		})
	}
}

// sanitizeIngestError 是事件 payload 的错误摘要清洗（单行化 + 限长——
// 传输错误文本不进事件原文全量）。
func sanitizeIngestError(s string) string {
	const max = 256
	if len(s) > max {
		s = s[:max]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' {
			c = ' '
		}
		out = append(out, c)
	}
	return string(out)
}

// emitEventTx 是 streak 事件的默认发出缝（Outbox 单写；失败只日志——
// 事件披露不阻断采集）。
func emitEventTx(ctx context.Context, st *state.Store, name string, payload map[string]string, log *slog.Logger) {
	if st == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	err = st.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.AppendEvent(ctx, state.Event{Name: name, Subject: "platform:logs", Payload: string(raw)})
		return err
	})
	if err != nil {
		log.Warn("logs: ingest streak event append failed", "event", name, "error", err.Error())
	}
}
