package execrelay

// native HTTP 端点（gateway.go 原生端点例外清单登记的两个路径——torchwood
// 同纪律：非 proto 派生的 HTTP 端点显式登记，禁止在别处悄悄挂载）：
//
//   - GET /internal/exec-relay —— relay 反向常连（集群 token Bearer + WS
//     升级 + 注册帧；此后该连接进入读循环，relay 的全部会话流量复用此连接
//     ——D-W5-3 反向常连：零入站端口、零新增常驻组件）。
//   - GET /v1/terminal        —— 浏览器 WS 升级（ticket 鉴权——长效 token
//     不进 URL，设计 §2.5；升级前以 HTTP 错误信封拒绝，升级后以 close 帧
//     + WS 关闭通知会话级错误）。
//
// 挂载点 = runtime.newRootHandler 的精确路径分派（豁免面 = 分派面；REST
// gateway 不收这两个路径的 GET）。H7 请求体上限在分派前已生效（GET 无体
// ——无害）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	sharedv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/shared/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// 桥接面预算（native 端点的注册帧到达预算——relay 侧 registerTimeout 同值
// 语义）。
const relayRegisterBudget = 10 * time.Second

// NativeHandler 是两个终端 native 端点的分派 handler（GET 精确路径；
// 其余路径/方法 404/405——豁免面 = 分派面）。
type NativeHandler struct {
	cfg NativeConfig
}

// NativeConfig 是 native 端点装配配置。
type NativeConfig struct {
	Hub *Hub
	Log *slog.Logger
}

// NewNativeHandler 构造（runtime 装配点消费——newRootHandler 的分派面）。
func NewNativeHandler(cfg NativeConfig) *NativeHandler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &NativeHandler{cfg: cfg}
}

// ServeHTTP 分派两个精确路径。
func (h *NativeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case RelayPath:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed) //nolint:gosec // G705：固定文案
			return
		}
		h.serveRelay(w, r)
	case TerminalWSPath:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed) //nolint:gosec // G705：固定文案
			return
		}
		h.serveTerminal(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveRelay 处理 relay 反向常连：集群 token → WS 升级 → 注册帧（预算内）
// → 连接表登记 → 读循环（阻塞至连接结束）。
func (h *NativeHandler) serveRelay(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r.Header.Get("Authorization"))
	if err := h.cfg.Hub.VerifyClusterToken(r.Context(), token); err != nil {
		h.cfg.Log.Warn("execrelay: relay authentication rejected", "error", err)
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "invalid cluster token", http.StatusUnauthorized) //nolint:gosec // G705：固定文案
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws := newLockedConn(conn)
	// 注册帧：预算内第一帧必须是 register（协议序的强制点）。
	rctx, rcancel := context.WithTimeout(r.Context(), relayRegisterBudget)
	defer rcancel()
	msg, rerr := ws.Read(rctx)
	if rerr == nil {
		var t FrameType
		t, msg, rerr = DecodeFrame(msg)
		if rerr == nil && t != TypeRegister {
			rerr = fmt.Errorf("execrelay: first frame is %s, want register", t)
		}
		if rerr == nil {
			var reg RegisterFrame
			if jerr := DecodeJSONPayload(msg, &reg); jerr != nil {
				rerr = jerr
			} else {
				rc, rerr2 := h.cfg.Hub.RegisterRelay(r.Context(), ws, reg.Hostname)
				if rerr2 != nil {
					rerr = rerr2
				} else {
					// 读循环阻塞至连接结束（退出时 UnregisterRelay 清扫）。
					_ = h.cfg.Hub.RelayReadLoop(r.Context(), rc)
					_ = ws.Close()
					return
				}
			}
		}
	}
	_ = ws.Close()
	h.cfg.Log.Warn("execrelay: relay connection rejected", "error", rerr)
}

// serveTerminal 处理浏览器 WS：ticket（升级前 HTTP 校验）→ 升级 → 开会话
// → 桥接（浏览器帧 ↔ relay 帧）→ 收尾。
func (h *NativeHandler) serveTerminal(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	binding, err := h.cfg.Hub.Tickets().Redeem(ticket)
	if err != nil {
		// 升级前的 HTTP 拒绝：一次性/过期/无效统一文案（不泄漏状态细节；
		// 信封退化形态——message-only JSON 与 FZ-2 口径一致）。
		writeTerminalError(w, http.StatusUnauthorized,
			"terminal ticket is invalid, expired, or already used")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws := newLockedConn(conn)
	s, err := h.cfg.Hub.OpenSession(r.Context(), binding, 0, 0)
	if err != nil {
		// 已升级后的会话级错误：close 帧承载语义（Console 状态行消费）+
		// WS 正常关闭。
		code, reason := terminalErrorFor(err)
		if frame, ferr := EncodeCloseFrame(SessionCloseFrame{ID: "", Code: code, Reason: reason}); ferr == nil {
			_ = ws.Write(r.Context(), frame)
		}
		_ = ws.Close()
		return
	}
	h.bridgeTerminal(r.Context(), ws, s)
}

// bridgeTerminal 是浏览器会话的桥接循环：读侧 goroutine（浏览器帧 → relay）
// + 写侧（relay 事件 → 浏览器），任一侧结束即收尾（关闭 WS）。
func (h *NativeHandler) bridgeTerminal(ctx context.Context, ws *lockedConn, s *hubSession) {
	browserGone := make(chan struct{})
	go func() {
		defer close(browserGone)
		for {
			// 无读预算：终端会话的空闲上限（10min）由 hub watchdog 权威
			// 承载——浏览器面 60s 级读超时会把空闲会话错误掐断。断开由
			// 读错误/ctx 取消暴露。
			msg, err := ws.Read(ctx)
			if err != nil {
				return
			}
			t, payload, derr := DecodeFrame(msg)
			if derr != nil {
				s.end("protocol violation on the browser face", CloseInternal)
				return
			}
			switch t {
			case TypeStdin:
				// 浏览器面按连接路由（一条 WS = 恰一个会话）——帧自报的 ID
				// 无语义（客户端持 ticket 而非会话 ID，设计 §2.5），只校验
				// 流载荷形态合法。
				_, data, serr := DecodeStreamPayload(payload)
				if serr != nil {
					s.end("malformed stdin frame", CloseInternal)
					return
				}
				if ferr := s.ForwardStdin(ctx, data); ferr != nil {
					s.end("relay write failed", CloseDisconnected)
					return
				}
			case TypeResize:
				var f ResizeFrame
				if jerr := DecodeJSONPayload(payload, &f); jerr != nil {
					s.end("malformed resize frame", CloseInternal)
					return
				}
				_ = s.ForwardResize(ctx, f.Cols, f.Rows)
			case TypeSessionClose:
				s.end("closed by operator", CloseOK)
				return
			case TypePing:
				if p, perr := EncodeFrame(TypePong, nil); perr == nil {
					_ = ws.Write(ctx, p)
				}
			case TypePong:
				// relay 对 ping 的应答在 hub 连接面——浏览器面 pong 亦容忍。
			default:
				s.end(fmt.Sprintf("unexpected %s frame on the browser face", t), CloseInternal)
				return
			}
		}
	}()
	// 写侧：relay 事件 → 浏览器；终帧后关闭 WS（读侧随之退出）。
	for {
		select {
		case <-browserGone:
			s.end("browser disconnected", CloseDisconnected)
			_ = ws.Close()
			return
		case <-ctx.Done():
			s.end("server shutting down", CloseDisconnected)
			_ = ws.Close()
			return
		case ev := <-s.Events():
			if ev.close != nil {
				if frame, ferr := EncodeCloseFrame(*ev.close); ferr == nil {
					_ = ws.Write(ctx, frame)
				}
				_ = ws.Close()
				return
			}
			typ := TypeStdout
			if ev.stderr {
				typ = TypeStderr
			}
			payload, perr := EncodeStreamPayload(s.ID(), ev.data)
			if perr == nil {
				if frame, ferr := EncodeFrame(typ, payload); ferr == nil {
					if werr := ws.Write(ctx, frame); werr != nil {
						s.end("browser write failed", CloseDisconnected)
						_ = ws.Close()
						return
					}
				}
			}
		}
	}
}

// terminalErrorFor 映射 OpenSession 错误为（关闭码, 人读原因）——Console
// 状态行的断线原因承载面。
func terminalErrorFor(err error) (int, string) {
	switch {
	case errors.Is(err, ErrTerminalDisabled):
		return CloseDenied, "the web terminal is disabled (terminal.enabled=false)"
	case errors.Is(err, ErrSessionLimit):
		return CloseHardLimit, "terminal session limit reached (2 per token, 8 platform-wide)"
	case errors.Is(err, ErrNoRunningTask):
		return CloseNoTarget, "the target service has no running task"
	case errors.Is(err, ErrNoRelayConnection):
		return CloseNoTarget, "the exec relay is not connected for the target node (relay still converging)"
	default:
		return CloseInternal, "terminal session could not be opened"
	}
}

// writeTerminalError 是升级前 HTTP 拒绝的信封形态（message-only JSON——
// 退化信封与 FZ-2 口径一致）。
func writeTerminalError(w http.ResponseWriter, status int, message string) {
	env := &sharedv1.ErrorResponse{Message: message}
	raw, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(env)
	if err != nil {
		http.Error(w, message, status) //nolint:gosec // G705：固定文案
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw) //nolint:gosec // G705：protojson 固定信封，无用户可控标记
}

// bearerToken 解析 "Bearer <token>" 头（relay 面；大小写不敏感 scheme）。
func bearerToken(authorization string) string {
	parts := strings.SplitN(strings.TrimSpace(authorization), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
