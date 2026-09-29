package execrelay

// relay 侧的 Docker API 消费面（exec attach）：端口在本包定义、moby 实现
// 在本文件、假实现注入单测（internal/victorialogs docker.go 同款纪律——
// 第三方类型不出端口消费面）。
//
// 安全面（D19 不变项，全部 relay 本地强制、不依赖控制面）：
//   - label 卫兵：目标容器必须带 fleetly.app label，否则 403 拒绝——
//     会话只能落在平台受管应用容器上（E_TERMINAL 面外的任意容器一律不
//     可执行）；
//   - shell 白名单：/bin/bash、/bin/sh（设计 §2.3 照抄 R1 标准做法）——
//     按容器内存在性探测择一（探测 = 一次性 `exit 0` exec，退出码非 0
//     视为不可用），白名单就是命令面，用户自定命令 v0.2 不开。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// 探测轮询参数（shell 探测的一次性预算——每会话 open 至多 |白名单| 次探测，
// 预算内未退出按探测失败处理）。
const (
	probePollInterval = 50 * time.Millisecond
	probePollBudget   = 5 * time.Second
)

// ShellWhitelist 是可探测的 shell 词表（设计 §2.3 原文：/bin/bash、/bin/sh
// ——bash 存在优先 bash，否则退 sh）。
var ShellWhitelist = []string{"/bin/bash", "/bin/sh"}

// execDocker 是 relay 会话执行对 Docker API 的最小消费面。
type execDocker interface {
	// ContainerInspect 取容器实况（label 卫兵的输入；缺失返回
	// ErrContainerNotFound）。
	ContainerInspect(ctx context.Context, containerID string) (ContainerInfo, error)
	// ExecProbe 在容器内执行一次性命令并返回退出码（shell 探测用；
	// 创建/启动失败返回错误——调用方按「该 shell 不可用」处理）。
	ExecProbe(ctx context.Context, containerID string, cmd []string) (exitCode int, err error)
	// ExecAttachPTY 创建 TTY exec 并挂接（返回原始双向流——Tty=true 时
	// daemon 不做流复用，读写即终端字节）。
	ExecAttachPTY(ctx context.Context, containerID string, shell string, cols, rows uint16) (ExecStream, error)
	// ExecResize 调整在途 exec 的终端尺寸。
	ExecResize(ctx context.Context, execID string, cols, rows uint16) error
}

// ContainerInfo 是 label 卫兵需要的容器实况投影。
type ContainerInfo struct {
	// AppLabel 是 fleetly.app label 值（空 = 无 label——卫兵拒绝）。
	AppLabel string
}

// ExecStream 是一条已挂接的 TTY exec 双向流（原始字节；Close 掐断挂接——
// daemon 对 TTY exec 的挂接断开发送 SIGHUP，会话进程随之收尾）。
type ExecStream interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	// ExecID 是在途 exec 的底座 ID（resize 透传的句柄）。
	ExecID() string
}

// ErrContainerNotFound 是目标容器不存在的哨兵（session.close 404 面）。
var ErrContainerNotFound = fmt.Errorf("execrelay: target container not found")

// ErrLabelDenied 是 label 卫兵拒绝的哨兵（session.close 403 面；D19 原文
// 「无 label 容器 403」的唯一判定点）。
var ErrLabelDenied = fmt.Errorf("execrelay: target container has no %s label (only fleetly-managed app containers are executable)", state.LabelApp)

// ErrNoWhitelistedShell 是白名单 shell 全部探测失败的哨兵。
var ErrNoWhitelistedShell = fmt.Errorf("execrelay: none of the whitelisted shells (%s) are available in the target container", strings.Join(ShellWhitelist, ", "))

// relayDocker 是 execDocker 的 moby 实现（DOCKER_HOST/本机套接字）。
type relayDocker struct {
	cli *mobyclient.Client
}

// newRelayDocker 构造真实客户端（host 空 = FromEnv——relay 容器内经挂载的
// /var/run/docker.sock 缺省可达）。
func newRelayDocker(host string) (*relayDocker, error) {
	opts := []mobyclient.Opt{mobyclient.FromEnv}
	if host != "" {
		opts = []mobyclient.Opt{mobyclient.WithHost(host), mobyclient.FromEnv}
	}
	cli, err := mobyclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("execrelay: construct docker client: %w", err)
	}
	return &relayDocker{cli: cli}, nil
}

// Close 释放连接。
func (d *relayDocker) Close() error { return d.cli.Close() }

// ContainerInspect 实现 execDocker（label 卫兵的实况输入）。
func (d *relayDocker) ContainerInspect(ctx context.Context, containerID string) (ContainerInfo, error) {
	res, err := d.cli.ContainerInspect(ctx, containerID, mobyclient.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ContainerInfo{}, ErrContainerNotFound
		}
		return ContainerInfo{}, fmt.Errorf("execrelay: container inspect %s: %w", shortID(containerID), err)
	}
	if res.Container.Config == nil {
		return ContainerInfo{}, nil
	}
	return ContainerInfo{AppLabel: res.Container.Config.Labels[state.LabelApp]}, nil
}

// ExecProbe 实现 execDocker：创建（不挂 IO）→ 脱离启动 → 轮询 inspect 至
// 进程退出读退出码（脱离形态下启动失败多数在 start 同步报错；退出码以
// inspect 终态为准——短轮询到 Running=false，预算内未退出按失败处理）。
// 退出码非 0（含 126/127 类不可执行形态）由调用方按探测失败处理。
func (d *relayDocker) ExecProbe(ctx context.Context, containerID string, cmd []string) (int, error) {
	created, err := d.cli.ExecCreate(ctx, containerID, mobyclient.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdin:  false,
		AttachStdout: false,
		AttachStderr: false,
	})
	if err != nil {
		return 0, fmt.Errorf("execrelay: probe exec create: %w", err)
	}
	if _, err := d.cli.ExecStart(ctx, created.ID, mobyclient.ExecStartOptions{Detach: true}); err != nil {
		return 0, fmt.Errorf("execrelay: probe exec start: %w", err)
	}
	deadline := time.Now().Add(probePollBudget)
	for {
		ins, err := d.cli.ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("execrelay: probe exec inspect: %w", err)
		}
		if !ins.Running {
			return ins.ExitCode, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("execrelay: probe exec %s did not exit within %s", shortID(created.ID), probePollBudget)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(probePollInterval):
		}
	}
}

// ExecAttachPTY 实现 execDocker：Tty exec 创建（初始尺寸）→ hijack 挂接。
// Tty=true 的挂接流是原始终端字节（无 8 字节复用头），读写直通。
func (d *relayDocker) ExecAttachPTY(ctx context.Context, containerID string, shell string, cols, rows uint16) (ExecStream, error) {
	created, err := d.cli.ExecCreate(ctx, containerID, mobyclient.ExecCreateOptions{
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{shell},
		ConsoleSize:  mobyclient.ConsoleSize{Height: uint(cols), Width: uint(rows)}, //nolint:gosec // G115：终端尺寸量级极小
	})
	if err != nil {
		return nil, fmt.Errorf("execrelay: exec create %s: %w", shortID(containerID), err)
	}
	att, err := d.cli.ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{TTY: true})
	if err != nil {
		return nil, fmt.Errorf("execrelay: exec attach %s: %w", shortID(containerID), err)
	}
	return &execStream{hijacked: att.HijackedResponse, execID: created.ID}, nil
}

// ExecResize 实现 execDocker（尺寸在途可调——Console resize 同步面）。
func (d *relayDocker) ExecResize(ctx context.Context, execID string, cols, rows uint16) error {
	if _, err := d.cli.ExecResize(ctx, execID, mobyclient.ExecResizeOptions{
		Height: uint(rows), //nolint:gosec // G115：终端尺寸量级极小
		Width:  uint(cols), //nolint:gosec // G115：终端尺寸量级极小
	}); err != nil {
		return fmt.Errorf("execrelay: exec resize %s: %w", shortID(execID), err)
	}
	return nil
}

// execStream 是 ExecStream 的 hijacked 实现（原始 TTY 字节流）。
type execStream struct {
	hijacked mobyclient.HijackedResponse
	execID   string
}

func (s *execStream) Read(p []byte) (int, error)  { return s.hijacked.Reader.Read(p) }
func (s *execStream) Write(p []byte) (int, error) { return s.hijacked.Conn.Write(p) }
func (s *execStream) Close() error                { _ = s.hijacked.CloseWrite(); s.hijacked.Close(); return nil }
func (s *execStream) ExecID() string              { return s.execID }

// shortID 是日志/错误文本里的容器 ID 截断（全 ID 不进错误面的卫生习惯
// ——与审计只带元数据的口径一致）。
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// guardLabel 是 label 卫兵（D19 不变项）：容器必须带 fleetly.app label。
// relay 本地强制——即使控制面路由被绕过（版本错配/误用），无 label 容器
// 依然拿不到会话。
func guardLabel(info ContainerInfo) error {
	if info.AppLabel == "" {
		return ErrLabelDenied
	}
	return nil
}

// probeShell 按白名单次序探测容器内可用 shell（设计 §2.3：存在性探测择
// 一——探测命令 `exit 0`，退出码 0 即可用；全部失败返回 ErrNoWhitelistedShell）。
// open 帧显式携带 shell 时只校验白名单（不重复探测——控制面输入错误按
// CloseBadShell 拒绝）。
func probeShell(ctx context.Context, d execDocker, containerID, requested string) (string, error) {
	if requested != "" {
		for _, s := range ShellWhitelist {
			if s == requested {
				return requested, nil
			}
		}
		return "", fmt.Errorf("execrelay: shell %q is not in the whitelist", requested)
	}
	var lastErr error
	for _, shell := range ShellWhitelist {
		code, err := d.ExecProbe(ctx, containerID, []string{shell, "-c", "exit 0"})
		if err != nil {
			lastErr = err
			continue
		}
		if code == 0 {
			return shell, nil
		}
		lastErr = fmt.Errorf("execrelay: probe %s exited %d", shell, code)
	}
	if lastErr != nil {
		return "", fmt.Errorf("%w (last probe: %v)", ErrNoWhitelistedShell, lastErr)
	}
	return "", ErrNoWhitelistedShell
}
