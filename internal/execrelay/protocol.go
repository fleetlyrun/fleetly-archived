// Package execrelay 是 Web 终端的执行中继面（E7，设计
// docs/design/2026-09-22-web-terminal.md §2 全部，W5-S6；D19 修订版
// D-W5-3 反向常连——安全面条款全部保留）。
//
// 包内四个职责面：
//
//  1. 帧协议（protocol.go）：WS 二进制帧 = 长度前缀 + 类型字节（设计 §2.3
//     词表：register / session.open / stdin / stdout / stderr / resize /
//     session.close / ping）。relay 侧与控制面侧共用同一编解码。
//
//  2. relay 运行时（relay.go + docker.go + conn.go）：cmd/fleetly-exec 的
//     执行体——出站反向常连控制面（断线指数退避 1s→30s、15s ping）、注册
//     自报 hostname、按 open 帧执行 label 卫兵（无 fleetly.app label → 403，
//     relay 本地强制，不依赖控制面）→ shell 白名单探测（/bin/bash、/bin/sh）
//     → Docker exec attach（Tty + 三流）→ resize 透传 → 会话时限（空闲
//     10min / 硬上限 30min）。
//
//  3. 部署收敛（manager.go + spec.go）：控制面收敛 global 服务 fleetly-exec（host
//     网络 + docker.sock 只读挂载 + Swarm secret fleetly-exec-token——平台
//     生成 48B、哈希落 meta，不暴露用户面轮换）；terminal.enabled=false 时
//     移除服务。
//
//  4. 控制面 hub（hub.go + ticket.go + native.go）：relay 连接表（每节点一
//     连接、新连接顶旧）、一次性 ticket（60s、绑 token+app+service）、
//     native WS 端点（/internal/exec-relay 与 /v1/terminal）、浏览器 WS ↔
//     relay WS 的帧桥接、并发限额（per-token 2 / 全局 8）、terminal.opened
//     / terminal.closed 审计+事件（payload 只带元数据——明文纪律）。
package execrelay

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// ── 帧协议（设计 §2.3：长度前缀 + 类型字节）─────────────────────────────────
//
// 线路形态（每条 WS binary 消息 = 一帧）：
//
//	+----------------+----------+---------------------+
//	| len (4B big-E) | type (1B)| payload (len bytes) |
//	+----------------+----------+---------------------+
//
// len 是 payload 字节数（不含头）。类型词表：
//
//	0x01 register       控制面 ← relay：自报 hostname（task container ID
//	                    前缀——控制面经底座 task 反查 NodeID，成员发现零自研）
//	0x02 session.open   控制面 → relay：开会话（目标容器 + 可选 shell）
//	0x03 stdin          双向桥接：浏览器键入 → relay（→ exec stdin）
//	0x04 stdout         relay → 浏览器（Tty exec 的合并输出走本类型）
//	0x05 stderr         relay → 浏览器（Tty=true 时 daemon 不再分流出
//	                    stderr，类型保留给协议完备性——设计词表原文）
//	0x06 resize         浏览器 → relay（→ ExecResize）
//	0x07 session.close  双向：任一侧关会话（带 code + reason）
//	0x08 ping           relay → 控制面（15s keepalive）
//	0x09 pong           控制面 → relay（ping 应答）
//
// 结构化载荷（register/open/resize/close）= JSON；流载荷（stdin/stdout/
// stderr）= u16be 会话 ID 长度 + 会话 ID + 原始字节（终端数据高频低结构，
// 不做 JSON 包裹）。ping/pong 无载荷。
const (
	frameHeaderLen = 5 // 4B len + 1B type
	// maxPayload 是单帧载荷上限：终端输出突发（xterm 常见 4-64KB chunk）
	// 的宽裕上界；超限帧 = 协议违例，立即断连（fail-closed）。
	maxPayload = 512 << 10
	// maxSessionIDLen 是会话 ID 合法长度上界（ULID 26；余量为防御）。
	maxSessionIDLen = 64
)

// FrameType 是帧类型字节（词表见包注释；只增不改）。
type FrameType uint8

const (
	TypeRegister     FrameType = 0x01
	TypeSessionOpen  FrameType = 0x02
	TypeStdin        FrameType = 0x03
	TypeStdout       FrameType = 0x04
	TypeStderr       FrameType = 0x05
	TypeResize       FrameType = 0x06
	TypeSessionClose FrameType = 0x07
	TypePing         FrameType = 0x08
	TypePong         FrameType = 0x09
)

// String 是帧类型的日志/测试可读名（未知值原样十六进制——协议只增纪律下
// 旧对端发来的新类型名如实呈现）。
func (t FrameType) String() string {
	switch t {
	case TypeRegister:
		return "register"
	case TypeSessionOpen:
		return "session.open"
	case TypeStdin:
		return "stdin"
	case TypeStdout:
		return "stdout"
	case TypeStderr:
		return "stderr"
	case TypeResize:
		return "resize"
	case TypeSessionClose:
		return "session.close"
	case TypePing:
		return "ping"
	case TypePong:
		return "pong"
	}
	return fmt.Sprintf("frame(%#02x)", uint8(t))
}

// validFrameType 报告类型字节是否在已登记词表内（解码侧 fail-closed 判据；
// 词表只增——旧版本收到新类型按未知拒收，不做静默透传）。
func validFrameType(t FrameType) bool {
	switch t {
	case TypeRegister, TypeSessionOpen, TypeStdin, TypeStdout, TypeStderr,
		TypeResize, TypeSessionClose, TypePing, TypePong:
		return true
	}
	return false
}

// RegisterFrame 是 register 帧载荷（relay → 控制面）。
type RegisterFrame struct {
	// Hostname 是 relay 任务容器 hostname（swarm 任务缺省 = 容器 ID）。
	Hostname string `json:"hostname"`
}

// SessionOpenFrame 是 session.open 帧载荷（控制面 → relay）。
type SessionOpenFrame struct {
	// ID 是控制面铸造的会话 ID（ULID——桥接路由的路由键）。
	ID string `json:"id"`
	// ContainerID 是目标容器完整 ID（控制面从 app service 的 running task
	// 反解；用户/浏览器永不见容器选择面——设计 §2.3）。
	ContainerID string `json:"container_id"`
	// Shell 是白名单 shell 绝对路径（空 = relay 按容器内存在性探测择一）。
	Shell string `json:"shell,omitempty"`
	// Cols/Rows 是初始终端尺寸（0 = relay 取缺省 80x24）。
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// ResizeFrame 是 resize 帧载荷（浏览器 → relay）。
type ResizeFrame struct {
	ID   string `json:"id"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// 会话关闭码（session.close.code；语义对齐 HTTP 状态的习惯分类，取值只增）。
const (
	CloseOK           = 0   // 正常收尾（用户退出 / 对端关闭）
	CloseDenied       = 403 // label 卫兵拒绝（无 fleetly.app label——D19 原文）
	CloseNoTarget     = 404 // 容器不存在 / shell 白名单全灭
	CloseBadShell     = 400 // 指定 shell 不在白名单（控制面输入错误）
	CloseTimeout      = 408 // 空闲超时（10min）掐断
	CloseHardLimit    = 429 // 会话时限/并发上限掐断
	CloseInternal     = 500 // relay 侧执行故障（docker API 等）
	CloseDisconnected = 410 // 任一侧连接断开（ relay 重连/浏览器断开）
)

// SessionCloseFrame 是 session.close 帧载荷（双向）。
type SessionCloseFrame struct {
	ID string `json:"id"`
	// Code 是关闭码（上方 Close* 常量；0 = 正常）。
	Code int `json:"code"`
	// Reason 是人读原因（英文、单行——断线原因在 Console 状态行呈现）。
	Reason string `json:"reason"`
}

// EncodeFrame 把类型与载荷编码为完整帧（含头）。
func EncodeFrame(t FrameType, payload []byte) ([]byte, error) {
	if len(payload) > maxPayload {
		return nil, fmt.Errorf("execrelay: frame payload %d bytes exceeds limit %d", len(payload), maxPayload)
	}
	out := make([]byte, frameHeaderLen+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(payload))) //nolint:gosec // G115：载荷受 maxPayload 上界约束
	out[4] = byte(t)
	copy(out[frameHeaderLen:], payload)
	return out, nil
}

// DecodeFrame 拆一条完整 WS 消息为类型与载荷：长度与头不符、载荷超限、
// 类型未知一律报错（调用方 fail-closed 断连——不猜测续读）。
func DecodeFrame(msg []byte) (FrameType, []byte, error) {
	if len(msg) < frameHeaderLen {
		return 0, nil, fmt.Errorf("execrelay: frame too short (%d bytes)", len(msg))
	}
	n := binary.BigEndian.Uint32(msg[:4])
	if int(n) != len(msg)-frameHeaderLen {
		return 0, nil, fmt.Errorf("execrelay: frame length prefix %d does not match message size %d", n, len(msg)-frameHeaderLen)
	}
	if int(n) > maxPayload {
		return 0, nil, fmt.Errorf("execrelay: frame payload %d exceeds limit %d", n, maxPayload)
	}
	t := FrameType(msg[4])
	if !validFrameType(t) {
		return 0, nil, fmt.Errorf("execrelay: unknown frame type %#02x", uint8(t))
	}
	return t, msg[frameHeaderLen:], nil
}

// EncodeJSONPayload 是结构化帧载荷的统一编码入口。
func EncodeJSONPayload(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("execrelay: encode frame payload: %w", err)
	}
	return raw, nil
}

// DecodeJSONPayload 是结构化帧载荷的统一解码入口（DiscardUnknown 语义由
// encoding/json 缺省提供——协议只增纪律下的前后兼容面）。
func DecodeJSONPayload(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("execrelay: decode frame payload: %w", err)
	}
	return nil
}

// EncodeStreamPayload 编码流帧载荷：u16be 会话 ID 长度 + ID + 数据。
func EncodeStreamPayload(sessionID string, data []byte) ([]byte, error) {
	if len(sessionID) == 0 || len(sessionID) > maxSessionIDLen {
		return nil, fmt.Errorf("execrelay: stream frame session id %q out of range", sessionID)
	}
	if len(data) > maxPayload-len(sessionID)-2 {
		return nil, fmt.Errorf("execrelay: stream frame data %d bytes exceeds per-session limit", len(data))
	}
	out := make([]byte, 2+len(sessionID)+len(data))
	binary.BigEndian.PutUint16(out[:2], uint16(len(sessionID))) //nolint:gosec // G115：ID 长度受 maxSessionIDLen 约束
	copy(out[2:], sessionID)
	copy(out[2+len(sessionID):], data)
	return out, nil
}

// DecodeStreamPayload 解码流帧载荷（会话 ID + 数据）。
func DecodeStreamPayload(raw []byte) (sessionID string, data []byte, err error) {
	if len(raw) < 2 {
		return "", nil, fmt.Errorf("execrelay: stream frame too short")
	}
	n := binary.BigEndian.Uint16(raw[:2])
	if int(n) == 0 || int(n) > maxSessionIDLen || 2+int(n) > len(raw) {
		return "", nil, fmt.Errorf("execrelay: stream frame session id length %d invalid", n)
	}
	return string(raw[2 : 2+n]), raw[2+n:], nil
}

// EncodeOpenFrame 编码 session.open 帧（控制面侧出口）。
func EncodeOpenFrame(f SessionOpenFrame) ([]byte, error) {
	raw, err := EncodeJSONPayload(f)
	if err != nil {
		return nil, err
	}
	return EncodeFrame(TypeSessionOpen, raw)
}

// EncodeCloseFrame 编码 session.close 帧（双向出口）。
func EncodeCloseFrame(f SessionCloseFrame) ([]byte, error) {
	raw, err := EncodeJSONPayload(f)
	if err != nil {
		return nil, err
	}
	return EncodeFrame(TypeSessionClose, raw)
}

// EncodeResizeFrame 编码 resize 帧（浏览器 → relay）。
func EncodeResizeFrame(f ResizeFrame) ([]byte, error) {
	raw, err := EncodeJSONPayload(f)
	if err != nil {
		return nil, err
	}
	return EncodeFrame(TypeResize, raw)
}

// EncodeRegisterFrame 编码 register 帧（relay → 控制面）。
func EncodeRegisterFrame(f RegisterFrame) ([]byte, error) {
	raw, err := EncodeJSONPayload(f)
	if err != nil {
		return nil, err
	}
	return EncodeFrame(TypeRegister, raw)
}
