package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// 节点观测缓存刷新器（state-model §2.2）：统一 30s 全量 resync + 事件
// 驱动失效（node/service/task 变更 1s 内触发刷新，带 1s 节流）；底座
// 不可达 → 全部行置 stale + 指数退避重试，服务不崩溃；读契约字段
// observed_at/stale 随每次写入维护。启动先做一次全量同步（成功前
// readiness 报不健康——「启动先全量同步再服务」）。
//
// 定位纪律：nodes 表是观测缓存，禁止用于决策。v0.1 注记「本刷新器不产
// 生产品事件」自 v0.2 起解除：每拍同步成功后运行 PostSync 挂钩（锚定
// 循环 ClusterAnchor——收编 + node.* 差分事件，multi-node §2.7）；挂钩
// 缺省为 nil（未装配 = 零行为差异，观测同步语义逐字不变）。

const (
	// ObservationInterval 是全量 resync 周期（state-model §2.2：统一 30s）。
	ObservationInterval = 30 * time.Second
	// observationSyncTimeout 是单次全量同步的执行上限。
	observationSyncTimeout = 15 * time.Second
	// observationEventThrottle 是事件驱动刷新的最小间隔（事件风暴节流；
	// 变更 1s 内刷新的口径由此与失效标记共同满足）。
	observationEventThrottle = 1 * time.Second
	// observationBackoffInitial / observationBackoffMax 是底座不可达的
	// 重试退避（指数，封顶 10 分钟）。
	observationBackoffInitial = 1 * time.Second
	observationBackoffMax     = 10 * time.Minute
)

// PostSyncFunc 是观测拍后处理挂钩：prev = 同步前的缓存行（差分基线），
// next = 本次全量快照。nil 挂钩 = 缺省无后处理。
type PostSyncFunc func(ctx context.Context, prev []CachedNode, next []SubstrateNode)

// Observer 维护 nodes 观测缓存。CheckHealth 结构性实现 lynx.Checker：
// 最近一次全量同步成功即健康。
type Observer struct {
	store  *Store
	docker DockerClient
	log    *slog.Logger
	// postSync 是观测拍后处理挂钩（WithPostSync 注入；nil = 无后处理）。
	postSync PostSyncFunc

	syncOK atomic.Bool

	invalidate chan struct{}
	stopOnce   sync.Once
	stop       chan struct{}
	done       chan struct{}
}

// WithPostSync 挂观测拍后处理（链式装配；每次成功同步后调用一次）。
func (o *Observer) WithPostSync(fn PostSyncFunc) *Observer {
	o.postSync = fn
	return o
}

// NewObserver 构造观测缓存刷新器。
func NewObserver(store *Store, d DockerClient, log *slog.Logger) *Observer {
	done := make(chan struct{})
	close(done)
	return &Observer{
		store:      store,
		docker:     d,
		log:        log,
		invalidate: make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       done,
	}
}

// CheckHealth 实现 lynx.Checker：最近一次全量观测同步成功（含启动后的
// 首次同步）才健康；底座不可达 → 不健康（readiness 如实反映底座健康）。
func (o *Observer) CheckHealth() error {
	if !o.syncOK.Load() {
		return errors.New("substrate observation unavailable: last full resync failed or not run yet")
	}
	return nil
}

// Invalidate 请求立即刷新（事件驱动失效信号入口；非阻塞，合并连发）。
func (o *Observer) Invalidate() {
	select {
	case o.invalidate <- struct{}{}:
	default:
	}
}

// Start 非阻塞启动刷新循环（同步首拍 + 周期拍 + 事件拍 + 事件流订阅）。
func (o *Observer) Start(ctx context.Context) error {
	o.done = make(chan struct{})
	go o.loop(ctx)
	go o.eventsLoop(ctx)
	return nil
}

// Stop 停止刷新循环并等待 goroutine 退出。
func (o *Observer) Stop(_ context.Context) error {
	o.stopOnce.Do(func() { close(o.stop) })
	<-o.done
	return nil
}

func (o *Observer) loop(ctx context.Context) {
	defer close(o.done)
	backoff := observationBackoffInitial
	// nextAttempt 是下一次允许同步的最早时刻：成功后 = 现在 + 周期；
	// 失败后 = 现在 + 指数退避（1s→2s→…封顶 10min）。调度用动态 timer
	// 而非固定 ticker——退避语义才能真实生效（固定 ticker 下短退避窗口
	// 会被 30s 周期淹没）。
	var nextAttempt time.Time

	schedule := func(d time.Duration) {
		nextAttempt = time.Now().Add(d)
	}

	resync := func() {
		if err := o.syncOnce(ctx); err != nil {
			o.syncOK.Store(false)
			if err := o.store.MarkAllNodesStale(ctx); err != nil {
				o.log.Error("mark all cached nodes stale failed", "error", err)
			}
			o.log.Warn("substrate observation sync failed, all cached nodes marked stale",
				"error", err, "retry_backoff", backoff.String())
			schedule(backoff)
			backoff *= 2
			if backoff > observationBackoffMax {
				backoff = observationBackoffMax
			}
			return
		}
		backoff = observationBackoffInitial
		o.syncOK.Store(true)
		schedule(ObservationInterval)
	}

	// armTimer 把同步 timer 定位到「下一次该动作的时刻」= min(nextAttempt,
	// 周期)：成功后即 30s 周期拍；失败后即指数退避边界（真实退避等待，
	// 而非被 30s 固定周期淹没）。
	armTimer := func(t *time.Timer) {
		d := ObservationInterval
		if delay := time.Until(nextAttempt); delay < d {
			if delay < 0 {
				delay = 0
			}
			d = delay
		}
		t.Reset(d)
	}

	// 启动先全量同步再服务：首拍同步完成前 readiness 由 CheckHealth
	// 报不健康；首拍失败不阻塞启动（退避重试由循环接管）。
	resync()
	timer := time.NewTimer(ObservationInterval)
	defer timer.Stop()
	armTimer(timer)
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.stop:
			return
		case <-timer.C:
			if delay := time.Until(nextAttempt); delay > 0 {
				// 退避窗口未过：顺延到窗口边界（指数退避的真实等待）。
				timer.Reset(delay)
				continue
			}
			resync()
			armTimer(timer)
		case <-o.invalidate:
			// 事件驱动拍：吞并积压信号；节流窗（1s）内不重复刷新，
			// 退避窗口未过则顺延（底座不可达时事件拍不放大压力）。
			o.drainInvalidate()
			delay := time.Until(nextAttempt)
			if delay < 0 {
				delay = 0
			}
			if delay > observationEventThrottle {
				delay = observationEventThrottle
			}
			if delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-o.stop:
					return
				case <-time.After(delay):
				}
			}
			resync()
			armTimer(timer)
		}
	}
}

func (o *Observer) drainInvalidate() {
	for {
		select {
		case <-o.invalidate:
		default:
			return
		}
	}
}

// syncOnce 执行一次全量同步：Ping 先行（不可达快速失败，不产生半程
// 写入），随后取全量快照并同事务落库；挂钩已装配时先读差分基线（同步
// 前的缓存行），落库成功后运行后处理（锚定循环 + node.* 差分事件——
// 挂钩自吞错误，不推翻同步成功）。
func (o *Observer) syncOnce(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, observationSyncTimeout)
	defer cancel()
	if err := o.docker.Ping(pingCtx); err != nil {
		return fmt.Errorf("substrate ping: %w", err)
	}
	snapCtx, cancel := context.WithTimeout(ctx, observationSyncTimeout)
	defer cancel()
	nodes, err := o.docker.ListNodeObservations(snapCtx)
	if err != nil {
		return fmt.Errorf("list node observations: %w", err)
	}
	var prev []CachedNode
	if o.postSync != nil {
		// 差分基线（SyncNodeObservations 会覆写缓存表，须在其前读取）。
		if prev, err = o.store.ListCachedNodes(snapCtx); err != nil {
			return fmt.Errorf("read previous node observations: %w", err)
		}
	}
	if err := o.store.SyncNodeObservations(snapCtx, nodes, time.Now().UTC()); err != nil {
		return err
	}
	if o.postSync != nil {
		o.postSync(snapCtx, prev, nodes)
	}
	return nil
}

// eventsLoop 订阅底座事件流（node/service/task 变更 → Invalidate）；
// 流断开自动重连（退避），仅作缓存失效信号、不产生产品事件。
func (o *Observer) eventsLoop(ctx context.Context) {
	backoff := observationBackoffInitial
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-o.stop:
			return
		default:
		}
		streamCtx, cancel := context.WithCancel(ctx)
		ch, err := o.docker.SubscribeEvents(streamCtx)
		if err != nil {
			cancel()
			o.log.Warn("substrate event subscription failed", "error", err, "retry_backoff", backoff.String())
			select {
			case <-ctx.Done():
				return
			case <-o.stop:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > observationBackoffMax {
				backoff = observationBackoffMax
			}
			continue
		}
		backoff = observationBackoffInitial
		o.consumeEvents(ch)
		cancel()
	}
}

func (o *Observer) consumeEvents(ch <-chan SubstrateEvent) {
	for {
		select {
		case <-o.stop:
			return
		case ev, ok := <-ch:
			if !ok {
				return // 流结束，由 eventsLoop 重连
			}
			if RelevantForObservation(ev) {
				o.Invalidate()
			}
		}
	}
}
