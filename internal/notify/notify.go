package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 投递器语义（E6 观测专项设计 §5.2，W5-S4）：
//
//	游标轮询：webhook_state 单行游标经 EventsSince 消费（WatchEvents 同
//	路径，不建进程内总线）→ 模式匹配 → 每匹配端点落 pending 台账行（与
//	游标推进同事务）→ 投递 worker POST。
//
//	并发：per 端点 ≤2、全局 ≤8（设计原文）；HTTP 超时 10s/次。
//
//	重试：失败退避 30s/5m，3 次尝试后终态 failed（设计 §5.2「失败 3 次
//	退避 30s/5m/30m（3 次尝试后终态）」——按 3 次尝试落地：首发失败
//	+30s、二试失败 +5m、三试失败终态；30m 档只在「首发 + 3 次重试」读法
//	下可达，保留在退避表注记中待设计复核）。非 2xx = 失败（记
//	response_code）；成功 = ok。
//
//	停机：drain 在途（Run 随服务 ctx 退出后关队列、等在途尝试收口）；
//	重启恢复：pending/到期行扫描继续投递（attempts 保留——台账列不重置）。
//
//	间隙（诚实形态）：游标早于保留窗（E_EVENT_CURSOR_EXPIRED）→ warn 日
//	志 + 游标对齐当前最大 seq 继续——跳过间隙事件、不回放不可达历史、不
//	落合成台账行（间隙是全局事实而非某端点的投递失败）。
const (
	// DefaultPollInterval 是事件消费/到期扫描的轮询周期（WatchEvents 同
	// 口径 1s）。
	DefaultPollInterval = time.Second
	// DefaultConsumeBatch 是单轮消费的事件批量上限。
	DefaultConsumeBatch = 200
	// DefaultAttemptTimeout 是单次 POST 的预算（设计 §5.2：10s/次）。
	DefaultAttemptTimeout = 10 * time.Second
	// MaxWorkers / MaxPerEndpoint 是全局与单端点并发上限（设计 §5.2）。
	MaxWorkers     = 8
	MaxPerEndpoint = 2
	// MaxDeliveryAttempts 是单条投递的总尝试预算（终态门槛）。
	MaxDeliveryAttempts = 3
	// DefaultNextRetryDelay 是无退避表覆盖时的兜底重试间隔。
	DefaultNextRetryDelay = 30 * time.Second
)

// retryBackoffSchedule 是发布在案的退避表（设计 §5.2：30s/5m/30m）。
// 消费口径：第 n 次尝试失败后、第 n+1 次尝试之前取表内第 n-1 档
// （n=1 → 30s、n=2 → 5m）；n 达到 MaxDeliveryAttempts（3）即终态，
// 第三档（30m）在 3 次尝试语义下无消费点——保留常量以对齐设计字面，
// 若设计复核改为「首发 + 3 次重试」则 MaxDeliveryAttempts 随之升 4。
var retryBackoffSchedule = []time.Duration{
	30 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
}

// retryDelay 返回第 attemptAttempts 次尝试失败后的重试间隔（超出表长
// 回落兜底——理论不可达：终态门槛先于表耗尽）。
// 已由 Config.retryDelayFor 承载（可注入退避表）；包级函数删除——单一
// 事实源。

// Config 是投递器可调参数（零值字段回落包缺省——单测注入短周期/短退避
// 驱动重试与 drain 断言）。
type Config struct {
	PollInterval   time.Duration
	ConsumeBatch   int
	AttemptTimeout time.Duration
	// MaxAttempts 是单条投递的总尝试预算（0 = 包缺省 3）。
	MaxAttempts int
	// Backoff 是重试退避表（nil = 包缺省 30s/5m/30m；单测注入秒级表）。
	Backoff []time.Duration
	// Workers / PerEndpoint 覆盖并发上限（0 = 包缺省 8/2；测试可收小）。
	Workers     int
	PerEndpoint int
}

func (c Config) pollInterval() time.Duration {
	if c.PollInterval > 0 {
		return c.PollInterval
	}
	return DefaultPollInterval
}

func (c Config) consumeBatch() int {
	if c.ConsumeBatch > 0 {
		return c.ConsumeBatch
	}
	return DefaultConsumeBatch
}

func (c Config) attemptTimeout() time.Duration {
	if c.AttemptTimeout > 0 {
		return c.AttemptTimeout
	}
	return DefaultAttemptTimeout
}

func (c Config) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return MaxDeliveryAttempts
}

func (c Config) backoff() []time.Duration {
	if len(c.Backoff) > 0 {
		return c.Backoff
	}
	return retryBackoffSchedule
}

func (c Config) workers() int {
	if c.Workers > 0 {
		return c.Workers
	}
	return MaxWorkers
}

func (c Config) perEndpoint() int {
	if c.PerEndpoint > 0 {
		return c.PerEndpoint
	}
	return MaxPerEndpoint
}

// retryDelayFor 是 Manager 视角的重试间隔（退避表可注入）。
func (c Config) retryDelayFor(attempts int) time.Duration {
	if attempts < 1 {
		return c.backoff()[0]
	}
	idx := attempts - 1
	table := c.backoff()
	if idx >= len(table) {
		return DefaultNextRetryDelay
	}
	return table[idx]
}

// Manager 是通知投递器。零框架依赖——lynx.Service 装配壳在 internal/runtime
// （Start = Run 阻塞到关停；Stop 无动作——drain 在 Run 内收口）。
type Manager struct {
	store  *state.Store
	box    *secrets.Box
	log    *slog.Logger
	cfg    Config
	client *http.Client

	// jobs 是投递任务队列。容量 256：溢出时非阻塞丢弃、
	// 行留台账由下一拍到期扫描重新认领——不丢投递事实，只延迟。
	jobs chan deliveryJob
	// inflight 是已入队未收口的行集合（到期扫描的重复入队防御）。
	inflightMu sync.Mutex
	inflight   map[string]struct{}
	// activeMu/active 是 per 端点在途计数（per 端点 ≤2 并发的限流面）。
	activeMu sync.Mutex
	active   map[string]int

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// NewManager 构造投递器（box 供台账投递时的密钥解密——state 层存密文不
// 解释，加密边界与 s3/env 同纪律）。
func NewManager(store *state.Store, box *secrets.Box, cfg Config, log *slog.Logger) *Manager {
	return &Manager{
		store:    store,
		box:      box,
		log:      log,
		cfg:      cfg,
		client:   &http.Client{},
		jobs:     make(chan deliveryJob, 256),
		inflight: make(map[string]struct{}),
		active:   make(map[string]int),
		stop:     make(chan struct{}),
	}
}

// deliveryJob 是队列载荷（端点 ID 随行——per 端点限流键）。
type deliveryJob struct {
	deliveryID string
	endpointID string
}

// Run 是常驻主循环（服务壳调用；ctx 取消返回——返回前 drain 在途尝试）。
func (m *Manager) Run(ctx context.Context) error {
	// 重启恢复：游标未初始化 → 对齐当前最大 seq（订阅从现在开始，不对
	// 历史事件补投——首启不轰炸接收方；已初始化为 no-op）。
	if err := m.initializeCursor(ctx); err != nil {
		return fmt.Errorf("notify: initialize cursor: %w", err)
	}
	// worker 先起（与主循环并发；jobs 关闭且排空后退出）。
	for i := 0; i < m.cfg.workers(); i++ {
		m.wg.Add(1)
		go m.worker(ctx)
	}
	ticker := time.NewTicker(m.cfg.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.drain()
			return nil
		case <-m.stop:
			m.drain()
			return nil
		case <-ticker.C:
		}
		m.tick(ctx)
	}
}

// Stop 提前触发停机（Stop(ctx) 语义对齐 lynx Service：等待 drain 完成）。
func (m *Manager) Stop(_ context.Context) error {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	return nil
}

// drain 关闭任务队列并等待在途尝试收口（in-flight HTTP ≤10s 自有预算；
// 未派发的行留台账，重启/下一拍扫描续投——不丢事实）。
func (m *Manager) drain() {
	close(m.jobs)
	m.wg.Wait()
}

// initializeCursor 首启对齐游标（InitializeWebhookCursorIfEmpty——订阅从
// 现在开始）。
func (m *Manager) initializeCursor(ctx context.Context) error {
	maxSeq, has, err := m.store.MaxEventSeq(ctx)
	if err != nil {
		return err
	}
	if !has {
		maxSeq = 0
	}
	initialized, err := m.store.InitializeWebhookCursorIfEmpty(ctx, maxSeq)
	if err != nil {
		return err
	}
	if initialized {
		m.log.Info("notify: delivery cursor initialized to the current max event seq (no historical replay)", "max_seq", maxSeq)
	}
	return nil
}

// tick 是单拍：事件消费 + 到期扫描。
func (m *Manager) tick(ctx context.Context) {
	m.consume(ctx)
	m.scanDue(ctx)
}

// consume 消费新事件：读游标 → EventsSince → 匹配 → 落 pending 行（与
// 游标推进同事务）→ 入队。间隙（游标过期）诚实跳过：warn + 对齐最大 seq。
func (m *Manager) consume(ctx context.Context) {
	cursor, err := m.store.GetWebhookCursor(ctx)
	if err != nil {
		m.log.Error("notify: read cursor failed", "error", err)
		return
	}
	events, err := m.store.EventsSince(ctx, cursor, m.cfg.consumeBatch())
	if err != nil {
		if isCursorExpired(err) {
			maxSeq, has, merr := m.store.MaxEventSeq(ctx)
			if merr != nil {
				m.log.Error("notify: cursor expired and max seq probe failed", "error", merr)
				return
			}
			if !has {
				maxSeq = cursor
			}
			if serr := m.store.SetWebhookCursor(ctx, maxSeq); serr != nil {
				m.log.Error("notify: cursor reset after gap failed", "error", serr)
				return
			}
			// 设计红线延伸：间隙只落日志——不落事件、不落合成台账行
			//（间隙是保留期清理的全局事实，不是任何端点的投递失败）。
			m.log.Warn("notify: event cursor expired (events pruned past the retention window); skipping the gap and continuing from the current max seq",
				"from_seq", cursor, "resumed_at_seq", maxSeq)
			return
		}
		m.log.Error("notify: read events failed", "error", err)
		return
	}
	if len(events) == 0 {
		return
	}
	endpoints, err := m.store.ListWebhookEndpoints(ctx)
	if err != nil {
		m.log.Error("notify: list endpoints failed (cursor still advances on retry)", "error", err)
		return
	}
	// 模式编译每拍一次（端点集小；编译失败=脏数据，loud 不静默）。
	matchers := make([]struct {
		id string
		m  *EndpointMatcher
	}, 0, len(endpoints))
	for _, ep := range endpoints {
		if !ep.Enabled {
			continue // 订阅开关：停用端点不消费（暂停投递不删台账）
		}
		em, err := NewEndpointMatcher(ep.EventPatterns)
		if err != nil {
			m.log.Error("notify: endpoint patterns failed to compile", "endpoint", ep.ID, "error", err)
			continue
		}
		matchers = append(matchers, struct {
			id string
			m  *EndpointMatcher
		}{ep.ID, em})
	}
	for _, ev := range events {
		matched := make([]string, 0, len(matchers))
		for _, mi := range matchers {
			if mi.m.Match(ev.Name) {
				matched = append(matched, mi.id)
			}
		}
		rows, err := m.store.CreateWebhookDeliveriesAndAdvance(ctx, ev.Seq, matched)
		if err != nil {
			// 游标与台账行同事务：失败一起回滚——下一拍重放同一事件，
			// 不双投不漏投。
			m.log.Error("notify: record deliveries failed (will retry next tick)", "event_seq", ev.Seq, "error", err)
			return
		}
		for _, row := range rows {
			m.enqueue(deliveryJob{deliveryID: row.ID, endpointID: row.EndpointID})
		}
	}
}

// scanDue 到期扫描：pending 且（next_retry_at 到期或为 NULL）的行重新
// 入队——重启恢复路径（attempts 保留）与退避定时器的库内形态。
func (m *Manager) scanDue(ctx context.Context) {
	due, err := m.store.DueWebhookDeliveries(ctx, time.Now().UTC(), 100, m.inflightSnapshot())
	if err != nil {
		m.log.Error("notify: scan due deliveries failed", "error", err)
		return
	}
	for _, d := range due {
		m.enqueue(deliveryJob{deliveryID: d.ID, endpointID: d.EndpointID})
	}
}

// inflightSnapshot 复制在途集合（查询过滤用）。
func (m *Manager) inflightSnapshot() map[string]struct{} {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	out := make(map[string]struct{}, len(m.inflight))
	for id := range m.inflight {
		out[id] = struct{}{}
	}
	return out
}

// enqueue 非阻塞入队并入在途集合（队列满 → 丢弃、行留台账待下一拍——
// 投递事实不丢）。
func (m *Manager) enqueue(job deliveryJob) {
	m.inflightMu.Lock()
	if _, dup := m.inflight[job.deliveryID]; dup {
		m.inflightMu.Unlock()
		return
	}
	m.inflight[job.deliveryID] = struct{}{}
	m.inflightMu.Unlock()
	select {
	case m.jobs <- job:
	default:
		m.forget(job.deliveryID)
	}
}

// forget 摘除在途标记（入队失败或尝试收口后）。
func (m *Manager) forget(deliveryID string) {
	m.inflightMu.Lock()
	delete(m.inflight, deliveryID)
	m.inflightMu.Unlock()
}

// worker 是投递执行体：取任务 → per 端点限流等待 → 尝试 → 收口。
func (m *Manager) worker(ctx context.Context) {
	defer m.wg.Done()
	for job := range m.jobs {
		if !m.acquire(job.endpointID) {
			// 关停期：不再等待限流（drain 语义 = 收口在途，不新开等待）。
			m.forget(job.deliveryID)
			continue
		}
		m.attempt(ctx, job)
		m.release(job.endpointID)
		m.forget(job.deliveryID)
	}
}

// acquire 占用一个端点投递槽（per 端点 ≤ m.cfg.perEndpoint()）；关停时
// 返回 false 不再等待。
func (m *Manager) acquire(endpointID string) bool {
	for {
		m.activeMu.Lock()
		if m.active[endpointID] < m.cfg.perEndpoint() {
			m.active[endpointID]++
			m.activeMu.Unlock()
			return true
		}
		m.activeMu.Unlock()
		select {
		case <-m.stop:
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// release 归还端点投递槽。
func (m *Manager) release(endpointID string) {
	m.activeMu.Lock()
	if m.active[endpointID] <= 1 {
		delete(m.active, endpointID)
	} else {
		m.active[endpointID]--
	}
	m.activeMu.Unlock()
}

// attempt 执行一次投递尝试并落台账（attempts 恒增；成功=ok；失败按退避
// 表判 pending/终态）。端点缺失（竞态删除）→ 放弃；端点停用 → 推迟 30s
// （不计尝试——停用不是投递失败）。簿记写使用去取消 ctx（自带 5s 预算）
// ——停机 drain 语义的关键：ctx 取消中止在途 HTTP，但这次尝试的记账必须
// 落账（store 连接池晚于全部服务 Stop 释放，写入安全）。
func (m *Manager) attempt(ctx context.Context, job deliveryJob) {
	// bookCtx 是簿记专用 ctx：与投递 ctx 解耦取消、保留 trace 值，预算
	// 5s（SQLite 单行写远低于此；预算防库故障时 drain 挂死）。
	bookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	d, err := m.store.GetWebhookDelivery(bookCtx, job.deliveryID)
	if err != nil {
		m.forget(job.deliveryID)
		return
	}
	ep, err := m.store.GetWebhookEndpoint(bookCtx, job.endpointID)
	if err != nil {
		// 端点已删（删除会清理台账行——此为窗口期竞态）：放弃本次结果。
		m.log.Warn("notify: endpoint vanished mid-delivery, dropping attempt", "endpoint", job.endpointID)
		return
	}
	if !ep.Enabled {
		deferAt := time.Now().UTC().Add(m.cfg.retryDelayFor(1))
		if err := m.store.PostponeWebhookDelivery(bookCtx, job.deliveryID, deferAt); err != nil {
			m.log.Error("notify: postpone disabled-endpoint delivery failed", "delivery", job.deliveryID, "error", err)
		}
		return
	}
	// 通道分发（设计 §8.1）：webhook 需要签名密钥（解密失败按失败计尝试
	// ——平台密钥面故障不该无限占用重试预算）；slack 的鉴权是 URL 本身、
	// email 的凭据在平台级 SMTP 设置——都不消费端点级签名密钥。
	epCh := Endpoint{Type: ep.Type, URL: ep.URL, Target: ep.Target}
	var smtpCfg *SmtpConfig
	if ep.Type == state.WebhookChannelWebhook || ep.Type == "" {
		secret, err := m.box.Decrypt([]byte(ep.SecretCipher))
		if err != nil {
			// 密钥解密失败 = 平台密钥面故障（非接收方故障）：按失败计尝试——
			// 密钥损坏的端点不该无限占用重试预算；终态后由台账可见。
			m.recordFailure(bookCtx, d, 0, "secret decrypt failed (platform key error)")
			return
		}
		epCh.Secret = secret
	}
	if ep.Type == state.WebhookChannelEmail {
		cfg, err := m.smtpConfig(bookCtx)
		if err != nil {
			m.recordFailure(bookCtx, d, 0, "smtp settings unavailable: "+err.Error())
			return
		}
		smtpCfg = cfg
	}
	// 告警分支（W5-S2，D-V3W5-1）：alert_payload 非空 = 告警投递行——载荷
	// 取自台账行自身（零事件，事件流零依赖），渲染按告警文案（FIRING/
	// RESOLVED 头行 + severity），预算与事件投递同一套（本函数剩余逻辑）。
	if d.AlertPayload != "" {
		if d.EventSeq != 0 {
			// 形态守卫：告警行 event_seq 恒 0（脏数据防御——loud 终态）。
			m.recordFailure(bookCtx, d, 0, "alert delivery row carries a non-zero event seq (dirty row)")
			return
		}
		ok, code, errText := m.attemptAlert(ctx, d.AlertPayload, epCh, smtpCfg)
		if ok {
			if err := m.store.RecordWebhookAttempt(bookCtx, job.deliveryID, state.WebhookAttemptResult{
				OK:           true,
				ResponseCode: code,
			}); err != nil {
				m.log.Error("notify: record ok attempt failed", "delivery", job.deliveryID, "error", err)
			}
			return
		}
		m.recordFailure(bookCtx, d, code, errText)
		return
	}
	payload, err := m.eventPayload(ctx, d.EventSeq)
	if err != nil {
		m.recordFailure(bookCtx, d, 0, "event lookup failed: "+err.Error())
		return
	}
	ok, code, errText := deliver(ctx, m.client, epCh, payload, smtpCfg, m.cfg.attemptTimeout())
	if ok {
		if err := m.store.RecordWebhookAttempt(bookCtx, job.deliveryID, state.WebhookAttemptResult{
			OK:           true,
			ResponseCode: code,
		}); err != nil {
			m.log.Error("notify: record ok attempt failed", "delivery", job.deliveryID, "error", err)
		}
		return
	}
	m.recordFailure(bookCtx, d, code, errText)
}

// smtpConfig 读取并解密平台级 SMTP 设置（email 通道投递前现读——保存即对
// 下一次投递生效；密码解密失败与未配置同面呈现，不带密码材料）。
func (m *Manager) smtpConfig(ctx context.Context) (*SmtpConfig, error) {
	in, err := m.store.LoadSmtpSettings(ctx)
	if err != nil {
		return nil, err
	}
	if in.Host == "" || in.Port == 0 || in.From == "" {
		return nil, errors.New("notify.smtp settings are incomplete (host/port/from are required)")
	}
	cfg := &SmtpConfig{Host: in.Host, Port: in.Port, Username: in.Username, From: in.From}
	if in.PasswordCipher != "" {
		plain, err := m.box.Decrypt([]byte(in.PasswordCipher))
		if err != nil {
			return nil, errors.New("smtp password decrypt failed (platform key error)")
		}
		cfg.Password = string(plain)
	}
	return cfg, nil
}

// recordFailure 记失败尝试：未达预算 → pending + 退避定时；达预算 → 终态
// failed（零事件——失败可见面 = 台账 + system 组件 + Console）。
func (m *Manager) recordFailure(ctx context.Context, d state.WebhookDelivery, code int, errText string) {
	next := time.Time{}
	if d.Attempts+1 < m.cfg.maxAttempts() {
		next = time.Now().UTC().Add(m.cfg.retryDelayFor(d.Attempts + 1))
	}
	if err := m.store.RecordWebhookAttempt(ctx, d.ID, state.WebhookAttemptResult{
		OK:           false,
		ResponseCode: code,
		ErrText:      errText,
		NextRetryAt:  next,
	}); err != nil {
		m.log.Error("notify: record failure attempt failed", "delivery", d.ID, "error", err)
		return
	}
	if next.IsZero() {
		m.log.Warn("notify: delivery terminally failed (no event emitted by design; see the delivery ledger and the notifications component)",
			"delivery", d.ID, "endpoint", d.EndpointID, "event_seq", d.EventSeq, "attempts", d.Attempts+1, "error", errText)
	}
}

// eventPayload 取事件行构造投递载荷（EventsSince 的 seq 定位读——游标语义
// 保证事件行在消费窗内不被清理，保留期 30d 远长于投递窗；竞态清理按缺省
// 载荷处理，投递事实仍成立）。
func (m *Manager) eventPayload(ctx context.Context, seq int64) (Payload, error) {
	evs, err := m.store.EventsSince(ctx, seq-1, 1)
	if err != nil {
		return Payload{}, err
	}
	if len(evs) == 0 || evs[0].Seq != seq {
		return NewEventPayload(seq, time.Now().UTC().Unix(), "unknown", "", "{}"),
			errors.New("event row missing (pruned past the retention window?)")
	}
	ev := evs[0]
	return NewEventPayload(ev.Seq, ev.At.Unix(), ev.Name, ev.Subject, ev.Payload), nil
}

// isCursorExpired 判定 E_EVENT_CURSOR_EXPIRED（state 层 apperr 的 code）。
func isCursorExpired(err error) bool {
	e, ok := apperr.FromError(err)
	return ok && e.Code() == "E_EVENT_CURSOR_EXPIRED"
}

// ── system status 组件（notifications）──────────────────────────────────

// CheckHealth 实现 system status 的 notifications 组件（设计 §5.2）：
// 无启用端点的连续终败 = 绿；有启用端点最近终态 = failed 即红（Error 带
// 端点名与最近错误）。检查自带 3s 预算（CheckHealth 无 ctx 形态，metrics
// 管理器同款）。
func (m *Manager) CheckHealth() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	terminals, err := m.store.LatestWebhookTerminalDeliveries(ctx)
	if err != nil {
		return fmt.Errorf("notifications: delivery ledger probe failed: %w", err)
	}
	if len(terminals) == 0 {
		return nil
	}
	// 端点名映射（红面带人读名）。
	endpoints, err := m.store.ListWebhookEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("notifications: endpoint probe failed: %w", err)
	}
	names := make(map[string]string, len(endpoints))
	enabled := make(map[string]bool, len(endpoints))
	for _, ep := range endpoints {
		names[ep.ID] = ep.Name
		enabled[ep.ID] = ep.Enabled
	}
	failing := 0
	var sample string
	for epID, d := range terminals {
		if d.Status != state.WebhookDeliveryFailed || !enabled[epID] {
			continue
		}
		failing++
		if sample == "" {
			name := names[epID]
			if name == "" {
				name = epID
			}
			sample = fmt.Sprintf("%q (attempts=%d, last_error=%s)", name, d.Attempts, truncateLine(d.LastError, 120))
		}
	}
	if failing == 0 {
		return nil
	}
	msg := fmt.Sprintf("%d enabled endpoint(s) terminally failing; latest failed delivery: %s", failing, sample)
	if failing > 1 {
		msg += " — check 'fleetly notifications deliveries --status failed'"
	}
	return errors.New(msg)
}

// truncateLine 单行化并截断（组件错误文本纪律）。
func truncateLine(s string, n int) string {
	out := make([]rune, 0, n)
	for _, r := range s {
		if r == '\n' || r == '\r' {
			r = ' '
		}
		out = append(out, r)
		if len(out) >= n {
			break
		}
	}
	return string(out)
}
