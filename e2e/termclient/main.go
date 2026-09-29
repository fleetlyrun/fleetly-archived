// termclient 是 Web 终端 e2e 的 WS 测试客户端（E7 W5-S6；e2e/terminal.sh
// 专用——dind 内 busybox 无 WebSocket 能力，stdlib + 仓内协议包编译的静态
// 小工具是诚实解）。协议编解码直接消费 internal/execrelay——帧形态单一
// 事实源，测试端与生产端零漂移。
//
// 用法（全部面以 -mode 选择）：
//
//	termclient -mode ticket -addr 127.0.0.1:8420 -token <tok> -app demo -service web
//	  → 签发 ticket，仅打印 ticket 本体（一次性消费测试的取票步骤）。
//	termclient -mode run -addr … -token … -app … -service … -command 'echo term-ok-xyz' -expect term-ok-xyz
//	  → 取票接入，发送 command，等待输出命中 expect（真 PTY 回显断言）。
//	termclient -mode run -ticket <t> …
//	  → 用既有 ticket 接入（一次性重放拒绝的负路径：第二次应 HTTP 401）。
//	termclient -mode run -ticket <t> -expect-close 429
//	  → 接入并等待服务端 close 帧（并发上限/禁用等会话级拒绝的断言面）。
//	termclient -mode hold -ticket <t> -hold-seconds 30
//	  → 接入并保持会话（并发上限占用面——后台 -d 形态消费）。
//
// 退出码：0 = 断言成立；3 = 断言不成立（expect 未命中 / close 码不符 /
// HTTP 拒绝）；1 = 运行故障（网络/协议）。HTTP 状态打印到 stderr 供脚本
// 取证（grep 401/403）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/fleetlyrun/fleetly/internal/execrelay"
)

func main() {
	fs := flag.CommandLine
	mode := fs.String("mode", "run", "ticket | run | hold")
	addr := fs.String("addr", "127.0.0.1:8420", "control plane HTTP address")
	token := fs.String("token", "", "API token (Bearer) for the ticket RPC")
	ticket := fs.String("ticket", "", "existing ticket (skips the ticket RPC)")
	app := fs.String("app", "", "target app")
	service := fs.String("service", "", "target service")
	command := fs.String("command", "", "shell input sent after connect")
	expect := fs.String("expect", "", "substring awaited on stdout")
	expectClose := fs.Int("expect-close", -1, "await a session close frame with this code")
	holdSecs := fs.Int("hold-seconds", 30, "hold-mode session lifetime")
	budget := fs.Duration("timeout", 30*time.Second, "overall budget")
	fs.Parse(os.Args[1:]) //nolint:errcheck,gosec // flag 解析失败自带 Usage 退出

	ctx, cancel := context.WithTimeout(context.Background(), *budget)
	defer cancel()
	tk := *ticket
	if tk == "" {
		var err error
		tk, err = acquireTicket(ctx, *addr, *token, *app, *service)
		if err != nil {
			// 401/403 是断言面（scope 缺失/鉴权拒）——打印状态供脚本取证。
			fmt.Fprintf(os.Stderr, "termclient: ticket rpc failed: %v\n", err)
			if strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403") {
				os.Exit(3)
			}
			os.Exit(1)
		}
		if *mode == "ticket" {
			fmt.Println(tk)
			return
		}
	}

	conn, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s%s", *addr, "/v1/terminal?ticket="+tk), nil)
	if err != nil {
		// 升级失败 = 服务端 HTTP 拒绝（重放 401 / 无效 ticket）——断言面。
		fmt.Fprintf(os.Stderr, "termclient: upgrade failed: %v\n", err)
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
			os.Exit(3)
		}
		os.Exit(1)
	}
	defer func() { _ = conn.CloseNow() }()

	if *mode == "hold" {
		// 保持会话：整消息读（丢弃载荷——coder/websocket 要求整消息消费，
		// 否则下一帧到达即错），到点或断开即回。会话在服务端保持 active
		// 直到本端断开（并发上限占面）。
		deadline := time.Now().Add(time.Duration(*holdSecs) * time.Second)
		for time.Now().Before(deadline) {
			rctx, rcancel := context.WithTimeout(ctx, time.Until(deadline))
			_, _, err := conn.Read(rctx)
			rcancel()
			if err != nil {
				break
			}
		}
		return
	}

	// run 模式：接入后发命令 + 等 expect 或 close。
	sent := false
	var out bytes.Buffer
	deadline := time.Now().Add(*budget)
	for {
		rctx, rcancel := context.WithTimeout(ctx, time.Until(deadline))
		kind, reader, err := conn.Reader(rctx)
		if err != nil {
			rcancel()
			// 升级后连接断开且带 401 语义的形态（重放拒）已由 Dial 覆盖；
			// 此处断开 = 服务端 WS 关闭。
			fmt.Fprintf(os.Stderr, "termclient: connection closed while waiting (sent=%v out=%q)\n", sent, out.String())
			os.Exit(3)
		}
		_ = kind
		msg := readAll(rctx, reader)
		rcancel()
		t, payload, derr := execrelay.DecodeFrame(msg)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "termclient: bad frame: %v\n", derr)
			os.Exit(1)
		}
		switch t {
		case execrelay.TypeStdout, execrelay.TypeStderr:
			_, data, serr := execrelay.DecodeStreamPayload(payload)
			if serr != nil {
				fmt.Fprintf(os.Stderr, "termclient: bad stream frame: %v\n", serr)
				os.Exit(1)
			}
			out.Write(data)
			if !sent && *command != "" {
				sent = true
				sp, _ := execrelay.EncodeStreamPayload("-", []byte(*command+"\n"))
				frame, _ := execrelay.EncodeFrame(execrelay.TypeStdin, sp)
				if werr := conn.Write(ctx, websocket.MessageBinary, frame); werr != nil {
					fmt.Fprintf(os.Stderr, "termclient: stdin write failed: %v\n", werr)
					os.Exit(1)
				}
			}
			if *expect != "" && strings.Contains(out.String(), *expect) {
				fmt.Println("MATCH")
				return
			}
		case execrelay.TypeSessionClose:
			var c execrelay.SessionCloseFrame
			if jerr := execrelay.DecodeJSONPayload(payload, &c); jerr != nil {
				fmt.Fprintf(os.Stderr, "termclient: bad close frame: %v\n", jerr)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "termclient: session closed code=%d reason=%q\n", c.Code, c.Reason)
			if *expectClose >= 0 && c.Code == *expectClose {
				os.Exit(0)
			}
			os.Exit(3)
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "termclient: budget exhausted (out=%q)\n", out.String())
			os.Exit(3)
		}
	}
}

// acquireTicket 走 REST 面（POST /v1/terminal/tickets——Bearer + terminal
// scope 的真实受理路径）。
func acquireTicket(ctx context.Context, addr, token, app, service string) (string, error) {
	body, _ := json.Marshal(map[string]string{"app": app, "service": service})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://%s/v1/terminal/tickets", addr), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Ticket        string `json:"ticket"`
		WebsocketPath string `json:"websocket_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Ticket == "" {
		return "", fmt.Errorf("empty ticket in response")
	}
	return out.Ticket, nil
}

// readAll 读完整消息（Reader 形态——coder/websocket 的流式读接口）。
func readAll(ctx context.Context, r io.Reader) []byte {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out
		}
		if ctx.Err() != nil {
			return out
		}
	}
}
