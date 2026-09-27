package substrate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 服务日志流（T2.20 日志管线的底座面）：docker service logs 的 moby/client
// 适配。第三方流格式（stdcopy 多路复用帧、Docker 时间戳头部）只存在于本
// 包内部，出口一律核心类型 LogLine——架构 §2.8 核心不出现第三方概念。

// LogLine 是服务日志单行（核心类型）。
type LogLine struct {
	// At 是 Docker 侧记录时间（timestamps=true 头部解析；解析失败取零值，
	// 调用方可回退本机时钟）。
	At time.Time
	// Stderr 报告该行来自 stderr 多路复用通道。
	Stderr bool
	// Line 是剥离时间戳头部后的原始内容（尾部换行已去除）。
	Line string
}

// ManagedServiceProcesses 返回指定应用的受管服务 compose 进程名集
// （label 过滤 fleetly.managed=true + fleetly.app=<app>，取
// fleetly.process label；字典序稳定）。日志采集的 per-app 服务发现入口
// （T2.20）；平台外同名服务不进结果。
func (c *Client) ManagedServiceProcesses(ctx context.Context, app string) ([]string, error) {
	rows, err := c.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelApp:     app,
	})
	if err != nil {
		return nil, fmt.Errorf("substrate: service list for app %s: %w", app, err)
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		name := r.Labels[state.LabelProcess]
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// JobServiceStates 实现 logs.Port（E5 Cron / DT-4 的「日志进现有采集」）：
// 列出该 app 当前存活的一次性 job 服务实况投影（fleetly-cron- 与
// fleetly-init- 两个前缀族的受管服务——cron 触发与发布期 init job 共享
// 采集面）。归属解析（job 名 + 受管 label → compose 服务）不在本层——
// logs.jobServiceRefOf 以 label 为权威承载（job 服务名含 app/service 成分，
// 字符串反解在含 '-' 时有歧义）。
func (c *Client) JobServiceStates(ctx context.Context, app string) ([]engine.ServiceState, error) {
	rows, err := c.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelApp:     app,
	})
	if err != nil {
		return nil, fmt.Errorf("substrate: service list for app %s: %w", app, err)
	}
	out := make([]engine.ServiceState, 0, len(rows))
	for _, r := range rows {
		if !naming.IsCronJobName(r.Name) && !naming.IsInitJobName(r.Name) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// TaskServiceStates 实现 logs.Port（DT-5 任务日志面）：列出全部存活的
// 任务服务实况投影（fleetly-task-<id> 前缀族 + 任务 label；任务无 app
// 归属，发现面不按 app 过滤）。归因以 label 为权威（fleetly.task-id）。
func (c *Client) TaskServiceStates(ctx context.Context) ([]engine.ServiceState, error) {
	rows, err := c.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelTasks:   "true",
	})
	if err != nil {
		return nil, fmt.Errorf("substrate: service list for tasks: %w", err)
	}
	out := make([]engine.ServiceState, 0, len(rows))
	for _, r := range rows {
		if !naming.IsTaskServiceName(r.Name) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// StreamServiceLogs 打开 Swarm 服务日志流（service 传 Swarm 服务名，
// fleetly 命名 = naming.ServiceName(app, service)）。follow=false 时读至
// 流自然结束（配合 since 做轮询拉取）；follow=true 持续跟随。since 非零
// 时只取该时刻之后的行。返回 channel 在流结束、出错或 ctx 取消时关闭
// （错误语义：日志流尽力而为，故障由采集器下轮重试，不单独暴露错误通道）。
// 服务不存在 / 底座不可达按 state 端口哨兵归类（mapSubstrateErr）。
// D2 排除面：流式调用不走 per-call 预算——生命周期由调用方 ctx 管理。
func (c *Client) StreamServiceLogs(ctx context.Context, service string, since time.Time, follow bool) (<-chan LogLine, error) {
	opts := mobyclient.ServiceLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Follow:     follow,
	}
	if !since.IsZero() {
		// Docker API 接受 RFC3339Nano；+1ns 防轮询边界重复（上一轮末行
		// 已入账）。
		opts.Since = since.Add(time.Nanosecond).Format(time.RFC3339Nano)
	}
	res, err := c.cli.ServiceLogs(ctx, service, opts)
	if err != nil {
		return nil, mapSubstrateErr(fmt.Errorf("substrate: service logs %s: %w", service, err))
	}
	out := make(chan LogLine)
	go func() {
		defer close(out)
		defer func() { _ = res.Close() }()
		streamLogs(ctx, res, out)
	}()
	return out, nil
}

// streamLogs 解多路复用帧并逐行投递（stdout/stderr 两路 bufio 扫描，
// StdCopy 按帧头分发到对应 pipe）。
func streamLogs(ctx context.Context, src io.Reader, out chan<- LogLine) {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	defer func() { _ = stdoutR.Close() }()
	defer func() { _ = stderrR.Close() }()

	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = stdcopy.StdCopy(stdoutW, stderrW, src)
		_ = stdoutW.Close()
		_ = stderrW.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanLines(ctx, stdoutR, false, out)
	}()
	go func() {
		defer wg.Done()
		scanLines(ctx, stderrR, true, out)
	}()
	wg.Wait()
	<-copyDone
}

// 单行投递上限与截断标记（MG-1）。
const (
	// maxLineLen 是单行投递上限（字节，含 Docker 时间戳头部）：超长行截断
	// 保留前缀 + truncatedSuffix 标记后照常投递——截断优于断流。
	maxLineLen = 1024 * 1024
	// truncatedSuffix 是截断行的尾部标记。
	truncatedSuffix = "...[truncated]"
)

// scanLines 逐行剥离 Docker 时间戳头部并投递；ctx 取消即停止读（管道
// 随 stdcopy goroutine 结束而关闭）。
//
// MG-1（排水契约）：本函数（连同 stderr 路）是 stdcopy 多路复用器写入
// io.Pipe 的唯一读者——无论行多长都必须持续排水。旧实现 bufio.Scanner
// 单行超 1MiB 即 Scan 返回 false 提前退出，StdCopy 从此阻塞在无读者的
// 管道写上，streamLogs 永不返回，采集/落盘/prune 全线停摆（静默）。
// 因此弃 Scanner 改 bufio.Reader 手动按行读：任意长度行不终止流，超长
// 行截断到 maxLineLen 并加标记后照常投递，剩余字节循环读丢弃至换行
// （内存驻留恒有界，不随行长增长）。
func scanLines(ctx context.Context, r io.Reader, stderr bool, out chan<- LogLine) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, truncated, err := readLineTruncating(br, maxLineLen)
		if truncated && line != "" {
			line += truncatedSuffix
		}
		if line != "" || err == nil {
			// 空完整行（连续 \n）照常投递（对齐 Scanner 语义）；err != nil
			// 且无内容 = 流尽。
			at, rest := splitTimestamp(line)
			select {
			case <-ctx.Done():
				return
			case out <- LogLine{At: at, Stderr: stderr, Line: rest}:
			}
		}
		if err != nil {
			return // io.EOF 或读错误：日志流尽力而为，由采集方下轮重试。
		}
	}
}

// readLineTruncating 读出一行（以 \n 终结，或流尽/读错误截尾）。行内容
// 超过 max 字节时只保留前 max 字节（truncated=true），剩余字节继续排水
// 丢弃直至换行。err 语义对齐 bufio：io.EOF = 流尽（返回内容为无换行的
// 末行，仍有效）；其余读错误原样透出，返回内容同样有效。
// 用 ReadSlice（而非 ReadString/ReadBytes——两者会把整行累积进单块内存，
// 行长多大就分配多大）：每段至多读缓冲大小，截断后只排水不驻留。
func readLineTruncating(br *bufio.Reader, max int) (string, bool, error) {
	var sb strings.Builder
	truncated := false
	for {
		chunk, err := br.ReadSlice('\n') // 段 ≤ 缓冲大小；ErrBufferFull = 行超缓冲未终结
		if !truncated {
			if room := max - sb.Len(); len(chunk) > room {
				if room > 0 {
					sb.Write(chunk[:room])
				}
				truncated = true
			} else {
				sb.Write(chunk)
			}
		} // 截断后的剩余段：只排水不驻留。
		if err == bufio.ErrBufferFull {
			continue // 行未终结（缓冲填满）：继续读下一段。
		}
		if err != nil {
			return sb.String(), truncated, err
		}
		if truncated {
			// 本段以 \n 结尾 = 行终结；保留段在截断点之前，不含结尾换行。
			return sb.String(), true, nil
		}
		return strings.TrimSuffix(sb.String(), "\n"), false, nil
	}
}

// splitTimestamp 剥离 Docker timestamps=true 头部（`<RFC3339Nano> <rest>`）。
func splitTimestamp(line string) (time.Time, string) {
	idx := strings.IndexByte(line, ' ')
	if idx <= 0 {
		return time.Time{}, line
	}
	if sec, err := strconv.ParseInt(line[:idx], 10, 64); err == nil && sec > 1_000_000_000 {
		// unix 秒形态（防御；Timestamps=true 恒 RFC3339Nano）。
		return time.Unix(sec, 0).UTC(), line[idx+1:]
	}
	if at, err := time.Parse(time.RFC3339Nano, line[:idx]); err == nil {
		return at.UTC(), line[idx+1:]
	}
	return time.Time{}, line
}
