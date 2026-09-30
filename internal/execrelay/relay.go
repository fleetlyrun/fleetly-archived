package execrelay

// relay 运行时（cmd/fleetly-exec 的执行体）：出站反向常连控制面（D-W5-3
// ——「受管组件持集群 token 主动拨控制面」的同型先例 = 8423 provider 端
// 点），断线指数退避（1s 起、30s 封顶），15s ping keepalive；注册帧自报
// hostname；会话执行 = label 卫兵 → shell 白名单探测 → TTY exec attach →
// stdin/stdout 桥接 + resize 透传 → 会话时限（空闲 10min / 硬上限 30min
// ——与控制面连接侧双保险，设计 §2.4）。
//
// 时限注入缝：SessionLimits 字段零值回落生产常量；单测注入缩短驱动超时
// 路径（e2e 不等真实 10min——验收分工：时限行为由单测钉死，e2e 只验证
// 会话正常开闭）。
//
// 会话数据面（relay 侧半桥）：
//
//	控制面 stdin 帧 ─→ stdinCh(缓冲) ─→ pumpStdin ─→ exec 流 ─→ 容器 PTY
//	容器 PTY ─→ exec 流 ─→ pumpOutput ─→ stdout 帧 ─→ 控制面
//	resize 帧 ─→ ExecResize（在途 exec 尺寸透传）
//	watchdog ─→ 空闲/硬上限触发 → 关 exec 流 → session.close（带时限原因）

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/fleetlyrun/fleetly/internal/componentloop"
)

// 生产时限常量（设计 §2.4 原文：空闲 10min / 硬上限 30min）。
const (
	DefaultIdleTimeout = 10 * time.Minute
	DefaultHardTimeout = 30 * time.Minute
)

// 连接治理常量（设计 §2.1：断线指数退避 1s→30s；15s ping keepalive）。
const (
	DefaultBackoffInitial = 1 * time.Second
	DefaultBackoffMax     = 30 * time.Second
	DefaultPingInterval   = 15 * time.Second
	// defaultCols/defaultRows 是 open 帧未带尺寸时的终端缺省。
	defaultCols, defaultRows = 80, 24
	// writeTimeout 是单帧写出的预算（对端僵死时不拖垮会话 goroutine）。
	writeTimeout = 10 * time.Second
	// stdinBufferDepth 是 stdin 帧缓冲深度（键入速率远低于此；缓冲满即
	// 丢弃该帧——防御恶意控制面灌帧拖死会话 goroutine）。
	stdinBufferDepth = 256
)

// DefaultTokenPath 是集群 token secret 文件的缺省挂载路径（部署 spec 的
// secret target 同锚）。
const DefaultTokenPath = "/run/secrets/fleetly-exec-token" //nolint:gosec // G101：secret 挂载路径常量，非凭据

// SessionLimits 是会话时限（零值字段回落生产常量；单测注入缩短）。
type SessionLimits struct {
	Idle time.Duration
	Hard time.Duration
}

// normalized 回落零值字段。
func (l SessionLimits) normalized() SessionLimits {
	if l.Idle <= 0 {
		l.Idle = DefaultIdleTimeout
	}
	if l.Hard <= 0 {
		l.Hard = DefaultHardTimeout
	}
	return l
}

// RelayConfig 是 relay 运行时配置（env 解析后的 typed 形态；单测直接构造）。
type RelayConfig struct {
	// ControlAddr 是控制面地址 host:port（native HTTP 面——FLEETLY_CONTROL_ADDR，
	// 部署收敛渲染 spec 时注入 advertise 地址）。
	ControlAddr string
	// TLSName 非空 = wss:// 且按此名做服务器证书校验（平台证书 SAN 主机名
	// ctrl.<base>，FLEETLY_CONTROL_TLS_NAME 下发——拨 IP 但按 SAN 名校验，
	// 无 insecure skip；空 = ws:// 明文，TLS off/manual 的诚实降级形态）。
	TLSName string
	// Token 是集群 token 明文（注入形态——单测）；空则读 TokenPath。
	Token string
	// TokenPath 是集群 token 的 Swarm secret 挂载文件路径（空回落缺省）。
	TokenPath string
	// Hostname 覆盖自报主机名（空 = os.Hostname——容器形态即容器 ID）。
	Hostname string
	// DockerHost 是 relay 侧 docker 连接（空 = FromEnv/本机套接字）。
	DockerHost string
	// DockerPort 注入执行端口（单测假实现；nil = 真实 moby 客户端）。
	DockerPort execDocker
	// Dial 注入拨号端口（单测假控制面；nil = 真实 coder/websocket 拨号）。
	Dial func(ctx context.Context, cfg RelayConfig, token string) (MessageConn, error)
	// PingInterval / BackoffInitial / BackoffMax 治理参数（零值回落常量）。
	PingInterval   time.Duration
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	// SessionLimits 会话时限（注入缝——见文件头注记）。
	SessionLimits SessionLimits
	// Log 为空回落 slog 缺省。
	Log *slog.Logger
}

// Relay 是反向常连执行中继。
type Relay struct {
	cfg    RelayConfig
	docker execDocker
	log    *slog.Logger

	mu       sync.Mutex
	sessions map[string]*relaySession
}

// NewRelay 构造 relay 运行时（dockerPort 缺省惰性构造真实客户端）。
func NewRelay(cfg RelayConfig) (*Relay, error) {
	if cfg.ControlAddr == "" {
		return nil, errors.New("execrelay: control address is empty (set FLEETLY_CONTROL_ADDR)")
	}
	if cfg.Token == "" && cfg.TokenPath == "" {
		cfg.TokenPath = DefaultTokenPath
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	r := &Relay{cfg: cfg, log: log, sessions: make(map[string]*relaySession)}
	if cfg.DockerPort != nil {
		r.docker = cfg.DockerPort
	}
	return r, nil
}

// dialURL 组装反向常连 URL（wss/ws 依 TLSName 而定——设计 §3.3）。
func dialURL(cfg RelayConfig) string {
	scheme := "ws"
	if cfg.TLSName != "" {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s%s", scheme, cfg.ControlAddr, RelayPath)
}

// dialReal 是生产拨号实现：集群 token 走 Authorization 头；TLSName 非空时
// 服务器证书按该名校验（tls.Config.ServerName——拨 advertise IP 但按平台
// 证书 SAN 主机名校验，设计 §3.3 原文「无 insecure skip」；根信任 = 系统
// 池，测试形态可经 SSL_CERT_FILE 注入自签根）。
func dialReal(ctx context.Context, cfg RelayConfig, token string) (MessageConn, error) {
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	opts.HTTPHeader.Set("Authorization", "Bearer "+token)
	if cfg.TLSName != "" {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{
			//nolint:gosec // G402：MinVersion 显式 TLS 1.2 基线；ServerName 是设计 §3.3 的校验面（非跳过）
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.TLSName},
		}}
	}
	conn, _, err := websocket.Dial(ctx, dialURL(cfg), opts)
	if err != nil {
		return nil, fmt.Errorf("execrelay: dial %s: %w", dialURL(cfg), err)
	}
	return newLockedConn(conn), nil
}

// token 读取集群 token（注入优先；否则读 secret 文件——Swarm secret 以
// tmpfs 文件形态出现在任务内）。
func (r *Relay) token() (string, error) {
	if r.cfg.Token != "" {
		return r.cfg.Token, nil
	}
	raw, err := os.ReadFile(r.cfg.TokenPath) //nolint:gosec // G304：路径来自部署 spec 注入的 env/缺省常量
	if err != nil {
		return "", fmt.Errorf("execrelay: read cluster token at %s: %w", r.cfg.TokenPath, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("execrelay: cluster token at %s is empty", r.cfg.TokenPath)
	}
	return token, nil
}

// Run 是常驻主循环：拨号 → 注册 → 读帧分发；断开即取消全部会话并按指数
// 退避重连（1s 起、30s 封顶——成功连接后的断线复位重跳）；ctx 取消返回 nil。
func (r *Relay) Run(ctx context.Context) error {
	backoff := orDefault(r.cfg.BackoffInitial, DefaultBackoffInitial)
	maxBackoff := orDefault(r.cfg.BackoffMax, DefaultBackoffMax)
	ping := orDefault(r.cfg.PingInterval, DefaultPingInterval)
	limits := r.cfg.SessionLimits.normalized()
	if r.docker == nil {
		d, err := newRelayDocker(r.cfg.DockerHost)
		if err != nil {
			return err
		}
		r.docker = d
	}
	token, err := r.token()
	if err != nil {
		return err
	}
	hostname := r.cfg.Hostname
	if hostname == "" {
		hn, herr := os.Hostname()
		if herr != nil {
			return fmt.Errorf("execrelay: read hostname: %w", herr)
		}
		hostname = hn
	}
	dial := r.cfg.Dial
	if dial == nil {
		dial = dialReal
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		conn, err := dial(ctx, r.cfg, token)
		if err != nil {
			r.log.Warn("execrelay: connect deferred (retrying with backoff)", "error", err, "retry_in", backoff.String())
			if !componentloop.SleepCtx(ctx, backoff) {
				return nil
			}
			backoff = minDur(backoff*2, maxBackoff)
			continue
		}
		backoff = orDefault(r.cfg.BackoffInitial, DefaultBackoffInitial)
		if err := r.serveConn(ctx, conn, hostname, ping, limits); err != nil {
			r.log.Warn("execrelay: connection lost (reconnecting)", "error", err)
		}
		if !componentloop.SleepCtx(ctx, backoff) {
			return nil
		}
	}
}

// serveConn 是单条连接的生命周期：注册帧 → ping 循环 + 读帧分发；返回连接
// 层错误（调用方统一退避重连）。连接断开时取消全部在途会话。
func (r *Relay) serveConn(ctx context.Context, conn MessageConn, hostname string, ping time.Duration, limits SessionLimits) error {
	defer func() {
		_ = conn.Close()
		r.cancelAllSessions()
	}()
	reg, err := EncodeRegisterFrame(RegisterFrame{Hostname: hostname})
	if err != nil {
		return err
	}
	if err := writeWithTimeout(ctx, conn, reg); err != nil {
		return fmt.Errorf("execrelay: send register: %w", err)
	}
	// ping keepalive：写失败只结束本 goroutine——连接读循环会在控制面侧
	// pong 断流后的读预算内感知断线（读侧有超时预算，见 relayReadTimeout）。
	go func() {
		t := time.NewTicker(ping)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			payload, perr := EncodeFrame(TypePing, nil)
			if perr == nil {
				perr = writeWithTimeout(ctx, conn, payload)
			}
			if perr != nil {
				return
			}
		}
	}()
	for {
		rctx, rcancel := context.WithTimeout(ctx, relayReadTimeout)
		msg, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			return fmt.Errorf("execrelay: read: %w", err)
		}
		// dispatch 用父 ctx（读超时只约束 Read 本身——会话 goroutine 不能
		// 继承一个已取消的读预算 ctx）。
		if err := r.dispatch(ctx, conn, msg, limits); err != nil {
			return err
		}
	}
}

// relayReadTimeout 是单帧读取预算：ping 周期 15s 的宽裕倍数——僵死连接
// （无任何帧）的最长存活窗口，超时即断连重连。
const relayReadTimeout = 60 * time.Second

// dispatch 处理一帧入站消息（控制面 → relay 只有 open/stdin/resize/close/
// pong 合法；协议违例返回错误 → 连接断开重连——fail-closed）。
func (r *Relay) dispatch(ctx context.Context, conn MessageConn, msg []byte, limits SessionLimits) error {
	t, payload, err := DecodeFrame(msg)
	if err != nil {
		return err
	}
	switch t {
	case TypeSessionOpen:
		var open SessionOpenFrame
		if err := DecodeJSONPayload(payload, &open); err != nil {
			return err
		}
		r.startSession(ctx, conn, open, limits)
		return nil
	case TypeStdin:
		id, data, err := DecodeStreamPayload(payload)
		if err != nil {
			return err
		}
		if s := r.session(id); s != nil {
			s.onStdin(data)
		}
		return nil
	case TypeResize:
		var f ResizeFrame
		if err := DecodeJSONPayload(payload, &f); err != nil {
			return err
		}
		if s := r.session(f.ID); s != nil {
			s.onResize(ctx, f.Cols, f.Rows)
		}
		return nil
	case TypeSessionClose:
		var f SessionCloseFrame
		if err := DecodeJSONPayload(payload, &f); err != nil {
			return err
		}
		if s := r.takeSession(f.ID); s != nil {
			s.shutdown(fmt.Sprintf("closed by control plane: %s", f.Reason), CloseOK)
		}
		return nil
	case TypePong:
		return nil
	default:
		return fmt.Errorf("execrelay: unexpected %s frame on relay connection", t)
	}
}

// startSession 登记会话并起执行 goroutine（卫兵/探测/attach 的失败都转化
// 为 session.close 帧——会话级失败不拖垮连接）。
func (r *Relay) startSession(ctx context.Context, conn MessageConn, open SessionOpenFrame, limits SessionLimits) {
	s := newRelaySession(open, conn, r.docker, limits)
	s.remove = r.takeSession
	r.mu.Lock()
	if _, dup := r.sessions[open.ID]; dup {
		r.mu.Unlock()
		r.sendClose(ctx, conn, open.ID, CloseInternal, "duplicate session id")
		return
	}
	r.sessions[open.ID] = s
	r.mu.Unlock()
	go s.run(ctx, open)
}

// newRelaySession 构造会话（尺寸缺省回落 80x24）。
func newRelaySession(open SessionOpenFrame, conn MessageConn, d execDocker, limits SessionLimits) *relaySession {
	s := &relaySession{
		id:         open.ID,
		conn:       conn,
		docker:     d,
		limits:     limits.normalized(),
		stdinCh:    make(chan []byte, stdinBufferDepth),
		idleNotify: make(chan struct{}, 1),
		cols:       open.Cols,
		rows:       open.Rows,
	}
	if s.cols == 0 || s.rows == 0 {
		s.cols, s.rows = defaultCols, defaultRows
	}
	return s
}

// relaySession 是 relay 侧一条在途会话（单 goroutine 拥有生命周期；
// stdin/resize 入口由连接读循环并发调用——字段以 mu 保护）。
type relaySession struct {
	id      string
	conn    MessageConn
	docker  execDocker
	limits  SessionLimits
	stdinCh chan []byte
	// idleNotify 是活动通知通道（markActivity 非阻塞触发——watchdog 的
	// 空闲复位源；watchdog 启动前的通知丢弃无害）。
	idleNotify chan struct{}
	// remove 是会话出表回调（startSession 注入 Relay.takeSession——会话
	// 收尾时自我出表）。
	remove func(id string) *relaySession

	mu     sync.Mutex
	stream ExecStream
	cols   uint16
	rows   uint16
	// stopReason / stopCode 是会话收尾原因（首个 shutdown 写入者胜出）。
	stopReason string
	stopCode   int
	stop       chan struct{}
}

// run 执行会话主体：卫兵 → shell 探测 → attach → 双向泵 + 时限看门狗 →
// 收尾（session.close + 出表）。会话级失败全部落关闭帧，不返回错误。
func (s *relaySession) run(ctx context.Context, open SessionOpenFrame) {
	defer s.finish()
	info, err := s.docker.ContainerInspect(ctx, open.ContainerID)
	if err != nil {
		if errors.Is(err, ErrContainerNotFound) {
			s.shutdown("container not found", CloseNoTarget)
			return
		}
		s.shutdown(shortErr(err), CloseInternal)
		return
	}
	// label 卫兵（D19：无 fleetly.app label → 403——relay 本地强制，
	// 不依赖控制面路由的正确性）。
	if err := guardLabel(info); err != nil {
		s.shutdown("container has no fleetly.app label (denied)", CloseDenied)
		return
	}
	shell, err := probeShell(ctx, s.docker, open.ContainerID, open.Shell)
	if err != nil {
		s.shutdown(shortErr(err), CloseBadShell)
		return
	}
	stream, err := s.docker.ExecAttachPTY(ctx, open.ContainerID, shell, s.cols, s.rows)
	if err != nil {
		s.shutdown(shortErr(err), CloseInternal)
		return
	}
	s.mu.Lock()
	s.stream = stream
	s.stop = make(chan struct{})
	stop := s.stop
	s.mu.Unlock()

	go s.pumpStdin(stream)
	go s.pumpOutput(stream)
	go s.watchdog()

	// 等待任一收尾触发（时限 / 输出 EOF / stdin 写失败 / 控制面 close /
	// 连接断开 cancelAllSessions）。
	<-stop
	_ = stream.Close()
}

// finish 是会话统一收尾：发关闭帧 + 出表（幂等——stopReason 首写者胜出）。
func (s *relaySession) finish() {
	s.mu.Lock()
	reason, code := s.stopReason, s.stopCode
	s.mu.Unlock()
	frame, err := EncodeCloseFrame(SessionCloseFrame{ID: s.id, Code: code, Reason: reason})
	if err == nil {
		wctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		_ = s.conn.Write(wctx, frame)
	}
	if s.remove != nil {
		s.remove(s.id)
	}
}

// shutdown 是会话收尾的唯一入口（幂等；首个调用者定原因/码，关闭 stop
// 触发 run 主体的清理路径）。
func (s *relaySession) shutdown(reason string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		select {
		case <-s.stop:
			return // 已收尾
		default:
		}
		s.stopReason, s.stopCode = reason, code
		close(s.stop)
	}
	// attach 前失败（卫兵/探测阶段 stop 尚未创建）：只记原因，finish 直接
	// 消费。
	if s.stop == nil {
		s.stopReason, s.stopCode = reason, code
	}
}

// pumpStdin 把 stdin 帧缓冲写入 exec 流（写失败 = exec 侧已死 → 收尾）。
func (s *relaySession) pumpStdin(stream ExecStream) {
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	if stop == nil {
		return // attach 前失败路径——无泵
	}
	for {
		select {
		case <-stop:
			return
		case data := <-s.stdinCh:
			if _, err := stream.Write(data); err != nil {
				s.shutdown(shortErr(err), CloseInternal)
				return
			}
		}
	}
}

// pumpOutput 把 exec 输出泵为 stdout 帧（Tty 流是原始字节，读即终端回显；
// EOF/读错误 = 远端 shell 退出 → 正常收尾）。
func (s *relaySession) pumpOutput(stream ExecStream) {
	buf := make([]byte, 16<<10)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			s.markActivity()
			if serr := s.sendStream(TypeStdout, buf[:n]); serr != nil {
				s.shutdown(shortErr(serr), CloseInternal)
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				s.shutdown(shortErr(err), CloseOK)
				return
			}
			s.shutdown("remote shell exited", CloseOK)
			return
		}
	}
}

// 时限关闭原因（Console 断线原因行的承载文本；单测断言锚）。
const (
	idleTimeoutReason = "idle timeout (no data frames)"
	hardTimeoutReason = "hard session limit reached"
)

// watchdog 实现时限（设计 §2.4——连接侧与 relay 侧双保险的 relay 半边）：
// 空闲 = 无数据帧（stdin/stdout 任一方向）超 Idle；硬上限 = 会话时长超
// Hard。触发即 shutdown（run 主体关闭 exec 流 → 泵退出 → finish 发带时限
// 原因的关闭帧）。
func (s *relaySession) watchdog() {
	s.mu.Lock()
	idle, hard, stop := s.limits.Idle, s.limits.Hard, s.stop
	s.mu.Unlock()
	if stop == nil {
		return // attach 前失败路径——无看门狗
	}
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	hardT := time.NewTimer(hard)
	defer hardT.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.idleNotify:
			if !idleT.Stop() {
				select {
				case <-idleT.C:
				default:
				}
			}
			idleT.Reset(idle)
		case <-idleT.C:
			s.shutdown(idleTimeoutReason, CloseTimeout)
			return
		case <-hardT.C:
			s.shutdown(hardTimeoutReason, CloseHardLimit)
			return
		}
	}
}

// onStdin 是控制面 stdin 帧入口（Relay.dispatch 调用）：非阻塞入缓冲；
// 同时刷新空闲计时（用户键入即活动）。
func (s *relaySession) onStdin(data []byte) {
	s.markActivity()
	select {
	case s.stdinCh <- append([]byte(nil), data...):
	default:
	}
}

// onResize 是 resize 帧入口：尺寸透传 ExecResize + 记忆（attach 前到达的
// resize 以记忆尺寸参与 attach 初始 ConsoleSize）。
func (s *relaySession) onResize(ctx context.Context, cols, rows uint16) {
	s.mu.Lock()
	stream := s.stream
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	if stream != nil {
		// 尺寸透传失败不掐会话（终端继续可用，仅尺寸未变）。
		_ = s.docker.ExecResize(ctx, stream.ExecID(), cols, rows)
	}
}

// markActivity 非阻塞通知活动（空闲计时复位源）。
func (s *relaySession) markActivity() {
	select {
	case s.idleNotify <- struct{}{}:
	default:
	}
}

// sendStream 发送流帧（stdout 出站口）。
func (s *relaySession) sendStream(t FrameType, data []byte) error {
	payload, err := EncodeStreamPayload(s.id, data)
	if err != nil {
		return err
	}
	frame, err := EncodeFrame(t, payload)
	if err != nil {
		return err
	}
	return writeWithTimeout(context.Background(), s.conn, frame)
}

// session 取在途会话（无则 nil）。
func (r *Relay) session(id string) *relaySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

// takeSession 取出并移除会话。
func (r *Relay) takeSession(id string) *relaySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[id]
	delete(r.sessions, id)
	return s
}

// cancelAllSessions 掐断全部在途会话（连接断开——关闭 exec 流，泵随之
// 退出；关闭帧尽力而为）。
func (r *Relay) cancelAllSessions() {
	r.mu.Lock()
	ss := make([]*relaySession, 0, len(r.sessions))
	for id, s := range r.sessions {
		ss = append(ss, s)
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	for _, s := range ss {
		s.shutdown("relay connection lost", CloseDisconnected)
	}
}

// sendCloseConn 是连接级关闭帧出口（会话未登记时的拒绝路径）。
func (r *Relay) sendClose(ctx context.Context, conn MessageConn, id string, code int, reason string) {
	frame, err := EncodeCloseFrame(SessionCloseFrame{ID: id, Code: code, Reason: reason})
	if err != nil {
		return
	}
	_ = writeWithTimeout(ctx, conn, frame)
}

// writeWithTimeout 是带预算的单帧写出（对端僵死不拖垮调用方 goroutine）。
func writeWithTimeout(ctx context.Context, conn MessageConn, frame []byte) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, frame)
}

// shortErr 是错误文本的单行收敛（关闭原因面；协议帧载荷有界，总预算
// 200 字节含省略号）。截断保留尾部而非头部——docker 错误的诊断价值在尾部
//（"stat /bin/bash: no such file or directory"），头部是样板前缀；截断以
// 省略号如实标注。头部截断的事故：msg[:200] 把 "OCI runtime exec failed:
// exec" 后的文件名尾巴切掉（2026-09-26 走查 W2-5）。
func shortErr(err error) string {
	const max = 200
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) <= max {
		return msg
	}
	return "…" + msg[len(msg)-(max-3):]
}

// orDefault 回落零值时长。
func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// minDur 时长取小。
func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
