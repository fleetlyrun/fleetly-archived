// fleetly-exec 是 Web 终端的执行中继二进制（E7，设计
// docs/design/2026-09-22-web-terminal.md §2.1/§2.3/§3.3，W5-S6；独立 main
// ——不进 fleetlyd 主二进制，设计 §2.2 原文「relay 逻辑 = 独立小 main」）。
//
// 运行形态：swarm global 服务 fleetly-exec 的任务容器（host 网络 + 本节点
// docker.sock 挂载 + 集群 token secret），启动即**出站**反向常连控制面
// wss(s)://<FLEETLY_CONTROL_ADDR>/internal/exec-relay——零入站端口；断线
// 指数退避重连（1s→30s）、15s ping keepalive；注册帧自报容器 hostname
// （控制面经底座 task 反查 NodeID——成员发现零自研）。
//
// 配置全部经 env（duty 渲染 spec 时注入——单一事实源在 internal/execrelay
// spec.go）：
//
//	FLEETLY_CONTROL_ADDR        控制面 host:port（native HTTP 面——必填）
//	FLEETLY_CONTROL_TLS_NAME    wss 校验名（ctrl.<base>；空 = ws:// 明文——
//	                            control_plane.tls off/manual 的诚实降级）
//	FLEETLY_EXEC_TOKEN_PATH     集群 token secret 文件（缺省
//	                            /run/secrets/fleetly-exec-token）
//	DOCKER_HOST                 docker 连接（缺省本机套接字）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/fleetlyrun/fleetly/internal/execrelay"
)

// main 是 relay 进程入口：env 解析 → Relay 构造（loud-fail：控制面地址缺
// 失/token 不可读拒绝启动——swarm 按 restart-policy 自愈重试）→ 信号驱动
// 的常驻主循环。
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetly-exec:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.CommandLine
	addr := fs.String("addr", os.Getenv(execrelay.EnvControlAddr), "control plane host:port (env FLEETLY_CONTROL_ADDR)")
	tlsName := fs.String("tls-name", os.Getenv(execrelay.EnvControlTLSName), "wss ServerName (env FLEETLY_CONTROL_TLS_NAME; empty = ws:// plaintext)")
	tokenPath := fs.String("token-path", envOr("FLEETLY_EXEC_TOKEN_PATH", execrelay.DefaultTokenPath), "cluster token file path")
	dockerHost := fs.String("docker-host", os.Getenv("DOCKER_HOST"), "docker endpoint (env DOCKER_HOST)")
	fs.Parse(os.Args[1:]) //nolint:errcheck,gosec // flag 解析失败自带 Usage 退出

	if *addr == "" {
		return fmt.Errorf("control address is empty (set FLEETLY_CONTROL_ADDR or -addr)")
	}
	relay, err := execrelay.NewRelay(execrelay.RelayConfig{
		ControlAddr: *addr,
		TLSName:     *tlsName,
		TokenPath:   *tokenPath,
		DockerHost:  *dockerHost,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return relay.Run(ctx)
}

// envOr 读 env（空回落缺省）。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
