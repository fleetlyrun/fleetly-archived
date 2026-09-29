package execrelay

// 控制面 hub（设计 §2.1 会话路由 + §2.4 限额审计）：relay 连接表（每节点
// 一连接、新连接顶旧）、浏览器会话桥接（帧桥：浏览器 WS ↔ relay WS）、
// 并发限额（per-token 2 / 全局 8——对齐 WatchEvents per-token 5 的 A5 精
// 神，终端更重故更紧）、terminal.opened / terminal.closed 审计+事件
//（payload 只带元数据——明文纪律：会话内容零出现）。
//
// node liveness = 连接存在（设计 §2.1 原文——无独立心跳状态机；relay 断
// 线 → 连接表摘除 → 该节点的会话目标不可达，新会话在其他节点重选）。

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台常量（设计 §2.1/§2.2/§2.4；不走配置面）。
const (
	// RelayPath 是 relay 反向常连的 native WS 端点（控制面 HTTP 面）。
	RelayPath = "/internal/exec-relay"
	// TerminalWSPath 是浏览器 WS 升级端点（ticket 鉴权——长效 token 不进
	// URL，设计 §2.5）。
	TerminalWSPath = "/v1/terminal"
	// ExecRelayServiceName 是 relay 的 global 服务名（集群全局命名空间，
	// fleetly- 平台前缀族）。
	ExecRelayServiceName = "fleetly-exec"
	// ExecRelaySecretName 是集群 token 的 Swarm secret 名（平台生成 48B，
	// 哈希落 meta——用户面轮换不做，runbook 记运维路径）。
	ExecRelaySecretName = "fleetly-exec-token" //nolint:gosec // G101：Swarm secret 名常量，非凭据本体
	// MetaKeyClusterTokenHash 是集群 token sha256 hex 的 meta 键（认证比对
	// 真源——明文只存在于 Swarm secret）。
	MetaKeyClusterTokenHash = "execrelay_cluster_token_hash" //nolint:gosec // G101：meta 键名常量，非凭据
	// DefaultPerTokenSessions / DefaultMaxSessions 是并发限额（设计 §2.4）。
	DefaultPerTokenSessions = 2
	DefaultMaxSessions      = 8
)

// TaskRuntime 是会话路由需要的任务运行时投影（substrate.Client 的加法投
// 影经装配层适配转入本包类型——方向纪律：底座类型不出 substrate）。
type TaskRuntime struct {
	ID           string
	NodeID       string
	ContainerID  string
	Slot         int
	State        string
	DesiredState string
	Timestamp    time.Time
}

// TaskSource 是 hub 对底座任务面的消费端口（装配层以 substrate.Client 适配）。
type TaskSource interface {
	// ListTaskRuntimes 返回服务的全部任务投影（成员发现 + 会话目标选择）。
	ListTaskRuntimes(ctx context.Context, service string) ([]TaskRuntime, error)
	// NodeHostnames 返回 nodeID → 节点 hostname（成员发现的反查面——
	// swarm 任务容器缺省 hostname = 节点 hostname，非容器 ID）。
	NodeHostnames(ctx context.Context) (map[string]string, error)
}

// hub 哨兵错误（native 端点/api 面映射）。
var (
	// ErrTerminalDisabled 是功能开关关闭（terminal.enabled=false）。
	ErrTerminalDisabled = errors.New("execrelay: web terminal is disabled")
	// ErrNoRelayConnection 是目标节点无 relay 连接（relay 服务未收敛/节点刚断）。
	ErrNoRelayConnection = errors.New("execrelay: no exec relay connection for the target node")
	// ErrNoRunningTask 是目标服务无 running 任务。
	ErrNoRunningTask = errors.New("execrelay: the target service has no running task")
	// ErrSessionLimit 是并发上限拒绝（per-token 或全局）。
	ErrSessionLimit = errors.New("execrelay: terminal session limit reached")
)

// HubConfig 是 hub 装配配置（限额/时限可注入——单测缩短驱动）。
type HubConfig struct {
	Store   *state.Store
	Tasks   TaskSource
	Tickets *TicketStore
	Log     *slog.Logger
	// Enabled 是功能开关供给（装配层注入 terminal.enabled 现读；nil = 恒开
	// ——单测/精简形态）。
	Enabled func() bool
	// PerTokenLimit / GlobalLimit 并发上限（0 回落默认 2/8）。
	PerTokenLimit int
	GlobalLimit   int
	// Limits 是控制面连接侧的会话时限（与 relay 侧双保险——设计 §2.4）。
	Limits SessionLimits
	// verifyToken 是集群 token 校验端口（缺省 = meta 哈希常量时间比对；
	// 单测可注入）。
	verifyToken func(ctx context.Context, token string) error
	// now 可注入时钟。
	now func() time.Time
}

// Hub 是控制面终端 hub：relay 连接表 + 活跃会话表。
type Hub struct {
	cfg HubConfig
	log *slog.Logger

	mu       sync.Mutex
	conns    map[string]*relayConn  // nodeID → 连接（新顶旧）
	sessions map[string]*hubSession // sessionID → 会话
	tokens   map[string]int         // tokenID → 活跃会话数
}

// NewHub 构造 hub。
func NewHub(cfg HubConfig) *Hub {
	if cfg.PerTokenLimit <= 0 {
		cfg.PerTokenLimit = DefaultPerTokenSessions
	}
	if cfg.GlobalLimit <= 0 {
		cfg.GlobalLimit = DefaultMaxSessions
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Tickets == nil {
		cfg.Tickets = NewTicketStore(0)
	}
	return &Hub{
		cfg:      cfg,
		log:      cfg.Log,
		conns:    make(map[string]*relayConn),
		sessions: make(map[string]*hubSession),
		tokens:   make(map[string]int),
	}
}

// Tickets 暴露 ticket 表（ExecService 签发面消费）。
func (h *Hub) Tickets() *TicketStore { return h.cfg.Tickets }

// Enabled 报告功能开关（nil 供给 = 恒开）。
func (h *Hub) Enabled() bool {
	if h.cfg.Enabled == nil {
		return true
	}
	return h.cfg.Enabled()
}

// NodesConnected 是连接表大小（GetTerminalStatus 投影——node liveness =
// 连接存在）。
func (h *Hub) NodesConnected() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// ActiveSessions 是活跃会话数（GetTerminalStatus 投影）。
func (h *Hub) ActiveSessions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// now 是可注入时钟（缺省 time.Now——会话时长的计量源）。
func (h *Hub) now() time.Time {
	if h.cfg.now != nil {
		return h.cfg.now()
	}
	return time.Now()
}

// ── relay 连接面 ─────────────────────────────────────────────────────────────

// relayConn 是一条已注册的 relay 连接（节点身份在注册期反查钉死）。
type relayConn struct {
	nodeID   string
	hostname string
	conn     MessageConn
}

// VerifyClusterToken 校验 relay 出示的集群 token（Bearer——非 API token：
// 与 meta 里的 sha256 哈希常量时间比对；集群 token 明文只存在于 Swarm
// secret，控制面只持哈希）。
func (h *Hub) VerifyClusterToken(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("execrelay: missing cluster token")
	}
	if h.cfg.verifyToken != nil {
		return h.cfg.verifyToken(ctx, token)
	}
	hash, err := h.cfg.Store.GetMeta(ctx, MetaKeyClusterTokenHash)
	if err != nil {
		return fmt.Errorf("execrelay: read cluster token hash: %w", err)
	}
	if hash == "" {
		return errors.New("execrelay: cluster token not provisioned yet (relay deployment pending)")
	}
	// 常量时间比对（认证比对面的纪律形态——sha256 后长度恒定）。
	if subtle.ConstantTimeCompare([]byte(state.HashToken(token)), []byte(hash)) != 1 {
		return errors.New("execrelay: cluster token mismatch")
	}
	return nil
}

// RegisterRelay 处理注册帧：hostname（容器 ID 前缀）→ relay 自己的 task →
// NodeID 反查（成员发现零自研——D19 原文）→ 连接表登记（同节点旧连接顶
// 掉关闭）。返回登记后的连接句柄。
func (h *Hub) RegisterRelay(ctx context.Context, conn MessageConn, hostname string) (*relayConn, error) {
	if hostname == "" {
		return nil, errors.New("execrelay: register frame carries empty hostname")
	}
	nodeID, err := h.resolveRelayNode(ctx, hostname)
	if err != nil {
		return nil, err
	}
	rc := &relayConn{nodeID: nodeID, hostname: hostname, conn: conn}
	h.mu.Lock()
	old := h.conns[nodeID]
	h.conns[nodeID] = rc
	h.mu.Unlock()
	if old != nil {
		// 新连接顶旧：旧连接关闭（其读循环退出 → UnregisterRelay 清扫；
		// 挂在其上的会话随后收尾——重连语义：旧会话死、新会话可建）。
		_ = old.conn.Close()
	}
	h.log.Info("execrelay: relay registered", "node", nodeID, "hostname", hostname,
		"replaced", old != nil)
	return rc, nil
}

// resolveRelayNode 反查 relay 所在节点：fleetly-exec 服务任务里先按容器 ID
// 前缀匹配 hostname（daemon 以容器 ID 作缺省 hostname 的形态）；不中再按
// **节点 hostname** 匹配（swarm 任务容器缺省 hostname = 节点 hostname——
// dind 与常规 swarm 主机的实态，2026-09-22 dind 实证）。两路都依赖底座
// 只读清单——成员发现零自研（D19 原文）。
func (h *Hub) resolveRelayNode(ctx context.Context, hostname string) (string, error) {
	tasks, err := h.cfg.Tasks.ListTaskRuntimes(ctx, ExecRelayServiceName)
	if err != nil {
		return "", fmt.Errorf("execrelay: list relay tasks: %w", err)
	}
	// 最新优先（顶替竞态：旧任务容器先断开、新任务后注册时以时间序取新）。
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Timestamp.After(tasks[j].Timestamp) })
	for _, t := range tasks {
		if len(t.ContainerID) >= len(hostname) && t.ContainerID[:len(hostname)] == hostname && t.NodeID != "" {
			return t.NodeID, nil
		}
	}
	// 节点 hostname 反查（swarm 实态路径）。
	hosts, err := h.cfg.Tasks.NodeHostnames(ctx)
	if err != nil {
		return "", fmt.Errorf("execrelay: node hostname lookup: %w", err)
	}
	for _, t := range tasks {
		if t.NodeID == "" || t.State != "running" {
			continue
		}
		if hosts[t.NodeID] == hostname {
			return t.NodeID, nil
		}
	}
	return "", fmt.Errorf("execrelay: no running relay task matches hostname %q (membership discovery)", hostname)
}

// UnregisterRelay 摘除连接（读循环退出时调用）：仅当表内仍是本连接（被新
// 连接顶掉的旧连接不再动表）；其上的活跃会话按断线收尾（pop 语义回滚
// per-token 计数）。
func (h *Hub) UnregisterRelay(rc *relayConn) {
	h.mu.Lock()
	if h.conns[rc.nodeID] == rc {
		delete(h.conns, rc.nodeID)
	}
	var dying []*hubSession
	for id, s := range h.sessions {
		if s.rc == rc {
			if popped := h.popSessionLocked(id); popped != nil {
				dying = append(dying, popped)
			}
		}
	}
	h.mu.Unlock()
	for _, s := range dying {
		s.endAfterPop("relay connection lost", CloseDisconnected)
	}
	h.log.Info("execrelay: relay unregistered", "node", rc.nodeID, "hostname", rc.hostname)
}

// RelayReadLoop 是 relay 连接的读循环（native 端点 goroutine 阻塞运行）：
// 路由 stdout/stderr/close 到对应会话桥；协议违例断开（fail-closed）。
func (h *Hub) RelayReadLoop(ctx context.Context, rc *relayConn) error {
	defer h.UnregisterRelay(rc)
	for {
		msg, err := rc.conn.Read(ctx)
		if err != nil {
			return err
		}
		t, payload, derr := DecodeFrame(msg)
		if derr != nil {
			return derr
		}
		switch t {
		case TypeStdout, TypeStderr:
			id, data, serr := DecodeStreamPayload(payload)
			if serr != nil {
				return serr
			}
			if s := h.session(id); s != nil && s.rc == rc {
				s.deliverData(t == TypeStderr, data)
			}
		case TypeSessionClose:
			var f SessionCloseFrame
			if cerr := DecodeJSONPayload(payload, &f); cerr != nil {
				return cerr
			}
			if s := h.takeSession(f.ID); s != nil && s.rc == rc {
				if f.Code == CloseOK && f.Reason == "" {
					f.Reason = "closed by exec relay"
				}
				s.endAfterPop(f.Reason, f.Code)
			}
		case TypePong:
			// relay 对 ping 的应答（keepalive 的往返证据；读预算超时即断线
			// 的兜底已在读超时面）。
		case TypePing:
			// 控制面也可被 ping（对称词表）——应答 pong。
			p, perr := EncodeFrame(TypePong, nil)
			if perr == nil {
				_ = writeWithTimeout(ctx, rc.conn, p)
			}
		default:
			return fmt.Errorf("execrelay: unexpected %s frame on relay connection", t)
		}
	}
}

// ── 浏览器会话面 ─────────────────────────────────────────────────────────────

// OpenSession 受理浏览器会话（native 端点在 ticket Redeem 后调用）：功能
// 开关 → 会话目标选择（app service 的 running 任务最新优先——副本细节对
// 操作员透明）→ relay 路由 → 并发限额 → open 帧下发 → 审计+事件
// （fail-closed：审计写失败即会话不开）。
func (h *Hub) OpenSession(ctx context.Context, b TicketBinding, cols, rows uint16) (*hubSession, error) {
	if !h.Enabled() {
		return nil, ErrTerminalDisabled
	}
	// Swarm 服务名按限定形 label 推导（v0.3 三段公式——ticket 签发面已把
	// team/prj/app 限定形带入绑定；rbac-teams §4.3）。
	svcName, err := naming.ServiceNameQualified(b.SwarmAppLabel(), b.Service)
	if err != nil {
		return nil, fmt.Errorf("execrelay: derive service name: %w", err)
	}
	target, err := h.pickTask(ctx, svcName)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	rc := h.conns[target.NodeID]
	if rc == nil {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w (node %s)", ErrNoRelayConnection, target.NodeID)
	}
	if len(h.sessions) >= h.cfg.GlobalLimit {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w (global limit %d)", ErrSessionLimit, h.cfg.GlobalLimit)
	}
	if h.tokens[b.TokenID] >= h.cfg.PerTokenLimit {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w (per-token limit %d)", ErrSessionLimit, h.cfg.PerTokenLimit)
	}
	s := &hubSession{
		hub:         h,
		id:          newSessionID(),
		binding:     b,
		app:         b.App,
		service:     b.Service,
		nodeID:      target.NodeID,
		containerID: target.ContainerID,
		rc:          rc,
		events:      make(chan sessionEvent, sessionEventBuffer),
		activity:    make(chan struct{}, 1),
		done:        make(chan struct{}),
		started:     h.now(),
		cols:        cols,
		rows:        rows,
	}
	if s.cols == 0 || s.rows == 0 {
		s.cols, s.rows = defaultCols, defaultRows
	}
	h.sessions[s.id] = s
	h.tokens[b.TokenID]++
	h.mu.Unlock()

	// open 帧下发（失败即回滚登记——会话从未存在）。
	open, oerr := EncodeOpenFrame(SessionOpenFrame{
		ID: s.id, ContainerID: target.ContainerID, Cols: s.cols, Rows: s.rows,
	})
	if oerr == nil {
		oerr = writeWithTimeout(ctx, rc.conn, open)
	}
	if oerr != nil {
		h.removeSession(s)
		return nil, fmt.Errorf("execrelay: send session open: %w", oerr)
	}
	// 审计 + 事件（fail-closed：写入失败不开会话——审计纪律原文「起止入
	// 审计」是会话成立的定义部分）。
	if aerr := h.emit(ctx, s, "terminal.opened", nil); aerr != nil {
		s.end("audit write failed", CloseInternal)
		return nil, fmt.Errorf("execrelay: audit terminal.opened: %w", aerr)
	}
	go s.watchdog()
	h.log.Info("execrelay: terminal session opened", "session", s.id, "app", s.app,
		"service", s.service, "node", s.nodeID)
	return s, nil
}

// pickTask 选会话目标任务：running + desired running 中 Timestamp 最新
// （多副本选最新——设计 §2.3）；无候选返回 ErrNoRunningTask。
func (h *Hub) pickTask(ctx context.Context, svcName string) (TaskRuntime, error) {
	tasks, err := h.cfg.Tasks.ListTaskRuntimes(ctx, svcName)
	if err != nil {
		return TaskRuntime{}, fmt.Errorf("execrelay: list target tasks: %w", err)
	}
	latest := TaskRuntime{}
	for _, t := range tasks {
		if t.State != "running" || t.DesiredState != "running" {
			continue
		}
		if t.ContainerID == "" || t.NodeID == "" {
			continue
		}
		if latest.ID == "" || t.Timestamp.After(latest.Timestamp) {
			latest = t
		}
	}
	if latest.ID == "" {
		return TaskRuntime{}, ErrNoRunningTask
	}
	return latest, nil
}

// session(id) 取会话（不摘表）。
func (h *Hub) session(id string) *hubSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

// takeSession 摘除会话（关闭帧到达路径）。
func (h *Hub) takeSession(id string) *hubSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.popSessionLocked(id)
}

// popSessionLocked 摘会话 + 计数回滚（调用方持锁）。
func (h *Hub) popSessionLocked(id string) *hubSession {
	s := h.sessions[id]
	if s == nil {
		return nil
	}
	delete(h.sessions, id)
	if h.tokens[s.binding.TokenID] > 0 {
		h.tokens[s.binding.TokenID]--
	}
	return s
}

// removeSession 摘除会话（无锁版本——OpenSession 失败路径）。
func (h *Hub) removeSession(s *hubSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur := h.popSessionLocked(s.id); cur != nil && cur != s {
		// 理论不可达（ID 全局唯一）——防御性把误摘还回。
		h.sessions[cur.id] = cur
	}
}

// hubSession 是控制面侧一条活跃会话（浏览器 ↔ relay 的桥接枢纽）。
type hubSession struct {
	hub         *Hub
	id          string
	binding     TicketBinding
	app         string
	service     string
	nodeID      string
	containerID string
	rc          *relayConn
	events      chan sessionEvent
	// activity 是活动通知通道（缓冲 1；watchdog 启动前的通知丢弃无害）。
	activity chan struct{}
	// done 在收尾完成时关闭（watchdog 的退出面）。
	done    chan struct{}
	started time.Time
	cols    uint16
	rows    uint16

	endOnce sync.Once
}

// sessionEvent 是推给浏览器桥的事件（数据帧或终帧）。
type sessionEvent struct {
	stderr bool
	data   []byte
	close  *SessionCloseFrame
}

// sessionEventBuffer 是会话事件缓冲（出向积压上界——浏览器消费慢于 relay
// 输出时的短时裕量；满即按故障收尾，不阻塞 relay 读循环）。
const sessionEventBuffer = 256

// deliverData 投递输出数据（relay 读循环调用；缓冲满 = 会话收尾——终端
// 显示面已不可靠）。
func (s *hubSession) deliverData(stderr bool, data []byte) {
	s.markActivity()
	select {
	case s.events <- sessionEvent{stderr: stderr, data: append([]byte(nil), data...)}:
	default:
		s.end("output backpressure (client too slow)", CloseInternal)
	}
}

// ForwardStdin 是浏览器 stdin 出口（native 桥调用——转发 relay + 空闲刷新）。
func (s *hubSession) ForwardStdin(ctx context.Context, data []byte) error {
	s.markActivity()
	payload, err := EncodeStreamPayload(s.id, data)
	if err != nil {
		return err
	}
	frame, err := EncodeFrame(TypeStdin, payload)
	if err != nil {
		return err
	}
	return writeWithTimeout(ctx, s.rc.conn, frame)
}

// ForwardResize 是浏览器 resize 出口（透传 relay → ExecResize）。
func (s *hubSession) ForwardResize(ctx context.Context, cols, rows uint16) error {
	frame, err := EncodeResizeFrame(ResizeFrame{ID: s.id, Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	return writeWithTimeout(ctx, s.rc.conn, frame)
}

// Events 是浏览器桥的事件流出口（终帧 = close 非 nil；通道不关闭——终帧
// 即桥的退出信号，收尾后的迟到数据帧不可能再被取出）。
func (s *hubSession) Events() <-chan sessionEvent { return s.events }

// ID 是会话 ID（日志/测试锚）。
func (s *hubSession) ID() string { return s.id }

// markActivity 刷新活动（空闲计时复位源——输入/输出任一方向）。
func (s *hubSession) markActivity() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

// end 是会话收尾（幂等）：出表 → 终帧入事件流 → 关闭帧发 relay → 审计/
// 事件 terminal.closed（写入失败只日志——收尾不可被审计故障卡死）。
func (s *hubSession) end(reason string, code int) {
	s.hub.mu.Lock()
	s.hub.popSessionLocked(s.id)
	s.hub.mu.Unlock()
	s.endAfterPop(reason, code)
}

// endAfterPop 是「调用方已出表」的收尾变体（relay close 帧路径经
// takeSession、连接断开路径经 UnregisterRelay——per-token 计数已被 pop
// 回滚）。与 end 共享 endOnce 幂等栅。
func (s *hubSession) endAfterPop(reason string, code int) {
	s.endOnce.Do(func() {
		defer close(s.done)
		closeFrame, cerr := EncodeCloseFrame(SessionCloseFrame{ID: s.id, Code: code, Reason: reason})
		if cerr == nil {
			_ = writeWithTimeout(context.Background(), s.rc.conn, closeFrame)
		}
		// 终帧最后入流（通道不关闭——桥以 close 非 nil 判退出）。
		select {
		case s.events <- sessionEvent{close: &SessionCloseFrame{ID: s.id, Code: code, Reason: reason}}:
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteBudget)
		defer cancel()
		if aerr := s.hub.emit(ctx, s, "terminal.closed", map[string]string{
			"reason":           reason,
			"close_code":       fmt.Sprintf("%d", code),
			"duration_seconds": fmt.Sprintf("%.0f", s.hub.now().Sub(s.started).Seconds()),
		}); aerr != nil {
			s.hub.log.Warn("execrelay: terminal.closed audit/event write failed", "error", aerr, "session", s.id)
		}
		s.hub.log.Info("execrelay: terminal session closed", "session", s.id, "reason", reason, "code", code)
	})
}

// auditWriteBudget 是审计/事件落库的预算（收尾路径的自有预算）。
const auditWriteBudget = 3 * time.Second

// watchdog 是控制面连接侧的时限看门狗（设计 §2.4 双保险的连接半边）：空闲
// /硬上限触发即 end（注入缝 = HubConfig.Limits——单测缩短驱动）。
func (s *hubSession) watchdog() {
	idle := orDefault(s.hub.cfg.Limits.Idle, DefaultIdleTimeout)
	hard := orDefault(s.hub.cfg.Limits.Hard, DefaultHardTimeout)
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	hardT := time.NewTimer(hard)
	defer hardT.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-s.activity:
			if !idleT.Stop() {
				select {
				case <-idleT.C:
				default:
				}
			}
			idleT.Reset(idle)
		case <-idleT.C:
			s.end(idleTimeoutReason, CloseTimeout)
			return
		case <-hardT.C:
			s.end(hardTimeoutReason, CloseHardLimit)
			return
		}
	}
}

// emit 把 terminal.opened / terminal.closed 落审计与事件（同事务 Outbox；
// payload 只带元数据——app/service/container/node/token/时长/原因，会话
// 内容零出现——明文纪律）。
func (h *Hub) emit(ctx context.Context, s *hubSession, action string, extra map[string]string) error {
	payload := map[string]string{
		"app":       s.app,
		"service":   s.service,
		"container": s.containerID,
		"node":      s.nodeID,
		"token":     s.binding.TokenID,
		"session":   s.id,
	}
	for k, v := range extra {
		payload[k] = v
	}
	raw := mustJSON(payload)
	return h.cfg.Store.InTx(ctx, func(tx *state.Tx) error {
		if _, err := tx.AppendEvent(ctx, state.Event{Name: action, Subject: "app:" + s.app, Payload: raw}); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:        "human",
			ActorTokenID: s.binding.TokenID,
			Action:       action,
			Target:       "app:" + s.app,
			Result:       "ok",
			DiffSummary:  state.DiffSummary("app", s.app, "service", s.service, "node", s.nodeID),
		})
	})
}

// newSessionID 铸造会话 ID（ULID——桥接路由的全局唯一键）。
func newSessionID() string { return ulid.Make().String() }

// mustJSON 是事件 payload 的 marshal 兜底（map[string]string 序列化失败
// 理论不可达——失败回落 "{}" 保事件行合法）。
func mustJSON(m map[string]string) string {
	raw, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
