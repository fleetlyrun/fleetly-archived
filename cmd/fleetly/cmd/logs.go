package cmd

// fleetly logs 命令（T2.18 新增动词——日志管线 RPC 面自 T2.20 起已有）：
//
//	follow  —— 实时跟随（server-stream；ctx 取消即断流，重连 = 重新
//	           follow，回放窗口由服务端 ring 承载）；
//	history —— 落盘检索（时间窗/服务/来源过滤；source ∈ container|build）；
//	search  —— 日志库统一检索（W5-S2，VictoriaLogs LogsQL 后端；
//	           source ∈ container|build|access）。
//
// --json 时流式输出 JSONL 逐行（一帧一行）；人读模式带时间戳/服务前缀。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lynx-go/commands"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// logsCmd 是外层动词 `logs`：分发 follow/history/search。
type logsCmd struct {
	sub *commands.App
}

func newLogsCmd() *logsCmd {
	sub := commands.New()
	sub.Register(&logsFollowCmd{}, &logsHistoryCmd{}, &logsSearchCmd{}, newLogsBackendCmd())
	sub.VerbTitle = "logs subcommands:"
	return &logsCmd{sub: sub}
}

func (c *logsCmd) Name() string     { return "logs" }
func (c *logsCmd) Synopsis() string { return "app logs (live follow + history + log-store search)" }
func (c *logsCmd) Usage() string    { return "logs <follow|history|search|backend> [flags] [args]" }

func (c *logsCmd) SetFlags(_ *flag.FlagSet) {}

func (c *logsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (follow|history|search)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// logsFollowCmd 实现 `fleetly logs follow <app> [--service <svc>]`。
type logsFollowCmd struct {
	service string
	jsonOut bool
	conn    connFlags
}

func (c *logsFollowCmd) Name() string { return "follow" }
func (c *logsFollowCmd) Synopsis() string {
	return "follow app logs (live stream; Ctrl-C / cancel to stop)"
}
func (c *logsFollowCmd) Usage() string {
	return "logs follow [--addr <host:port>] [--token <tok>] [--service <svc>] [--json] <app>"
}

func (c *logsFollowCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.service, "service", "", "compose service name (empty = all services of the app)")
	fs.BoolVar(&c.jsonOut, "json", false, "output JSONL (one frame per line)")
}

func (c *logsFollowCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	err := c.conn.withClient(func(cl *fleetlyClient) error {
		return cl.FollowLogs(ctx, c.conn.ref(args[0]), c.service, func(frame *serverv1.FollowLogsResponse) error {
			return emitLogEntry(env, c.jsonOut, frame.GetEntry())
		})
	})
	if isCleanCancel(ctx, err) {
		return nil // Ctrl-C/SIGTERM：流随 ctx 取消干净收尾，exit 0（S17-D3）
	}
	return err
}

// logsHistoryCmd 实现 `fleetly logs history <app> [--service] [--source]
// [--limit]`。
type logsHistoryCmd struct {
	service string
	source  string
	limit   int
	jsonOut bool
	conn    connFlags
}

func (c *logsHistoryCmd) Name() string { return "history" }
func (c *logsHistoryCmd) Synopsis() string {
	return "search persisted logs (time window / service / source filter)"
}
func (c *logsHistoryCmd) Usage() string {
	return "logs history [--addr <host:port>] [--token <tok>] [--service <svc>] [--source container|build] [--limit N] [--json] <app>"
}

func (c *logsHistoryCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.service, "service", "", "compose service name (empty = all)")
	fs.StringVar(&c.source, "source", "", "source filter: container | build (empty = all)")
	fs.IntVar(&c.limit, "limit", 200, "max rows (ceiling 1000; newest kept)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *logsHistoryCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Logs().ListHistoryLogs(ctx, &serverv1.ListHistoryLogsRequest{
			App: c.conn.ref(args[0]), Service: c.service, Source: c.source, Limit: int32Clamp(c.limit),
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "app %s: %d log entries\n", args[0], len(resp.GetEntries()))
		_, err = fmt.Fprint(env.Stdout, b.String())
		if err != nil {
			return err
		}
		for _, e := range resp.GetEntries() {
			if err := emitLogEntry(env, false, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// emitLogEntry 输出单条日志（--json：JSONL 一行；人读：时间/服务/通道前
// 缀 + 原行）。
func emitLogEntry(env *commands.Environment, jsonOut bool, e *serverv1.LogEntryView) error {
	if jsonOut {
		return writeProtoJSONL(env.Stdout, e)
	}
	at := ""
	if t := e.GetAt(); t != nil {
		at = t.AsTime().Format(time.RFC3339)
	}
	channel := "out"
	if e.GetStderr() {
		channel = "err"
	}
	_, err := fmt.Fprintf(env.Stdout, "%s [%s/%s] %s\n", at, e.GetService(), channel, e.GetLine())
	return err
}

// logsSearchCmd 实现 `fleetly logs search <app> --keyword <kw> [--since]
// [--until] [--source] [--service] [--limit]`（W5-S2，E6 设计 §3.1/§3.3
// CLI 面）：日志库（VictoriaLogs）统一检索。时间窗旗标接受相对时长
//（30m/1h/7d——相对当下）或 RFC3339 绝对时刻。空结果与后端不可达诚实
// 区分：前者 0 行 + 提示行（exit 0），后者以 E_LOGS_BACKEND_UNAVAILABLE
// 信封报错（服务端语义，renderCLIError 原样呈现）。
type logsSearchCmd struct {
	keyword string
	since   string
	until   string
	source  string
	service string
	limit   int
	jsonOut bool
	conn    connFlags
}

func (c *logsSearchCmd) Name() string { return "search" }
func (c *logsSearchCmd) Synopsis() string {
	return "search the log store (VictoriaLogs; keyword / time window / service / source)"
}
func (c *logsSearchCmd) Usage() string {
	return "logs search [--addr <host:port>] [--token <tok>] [--keyword <kw>] [--since 1h|30m|RFC3339] [--until ...] [--source container|build|access] [--service <svc>] [--limit N] [--json] <app>"
}

func (c *logsSearchCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.keyword, "keyword", "", "full-text keyword (matched as a literal phrase)")
	fs.StringVar(&c.since, "since", "", "window start: relative to now (30m, 1h, 7d) or RFC3339")
	fs.StringVar(&c.until, "until", "", "window end: relative to now (30m, 1h, 7d) or RFC3339")
	fs.StringVar(&c.source, "source", "", "source filter: container | build | access (empty = all)")
	fs.StringVar(&c.service, "service", "", "compose service name filter (empty = all)")
	fs.IntVar(&c.limit, "limit", 200, "max rows (ceiling 1000; newest kept)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *logsSearchCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	start, err := parseTimeArg(c.since)
	if err != nil {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("invalid --since %q: %w", c.since, err)}
	}
	end, err := parseTimeArg(c.until)
	if err != nil {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("invalid --until %q: %w", c.until, err)}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		req := &serverv1.SearchLogsRequest{
			App:     c.conn.ref(args[0]),
			Keyword: c.keyword,
			Limit:   int32Clamp(c.limit),
		}
		if c.service != "" {
			req.Services = []string{c.service}
		}
		if c.source != "" {
			req.Sources = []string{c.source}
		}
		if !start.IsZero() {
			req.TimeStart = timestamppb.New(start)
		}
		if !end.IsZero() {
			req.TimeEnd = timestamppb.New(end)
		}
		resp, err := cl.Logs().SearchLogs(ctx, req)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "app %s: %d matches\n", args[0], len(resp.GetRows()))
		if len(resp.GetRows()) == 0 {
			// 诚实空结果：检索面只覆盖入湖窗口（backend 切换点之前的
			// JSONL 历史不在检索面——设计 §2.3「检索不跨界」）。
			b.WriteString("no matches in the searched window (the log-store search covers only the VictoriaLogs ingestion window)\n")
		}
		for _, r := range resp.GetRows() {
			fmt.Fprintf(&b, "%s [%s/%s] %s\n",
				renderRowTime(r.GetAt()), r.GetService(), r.GetSource(), r.GetMsg())
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// renderRowTime 是检索行的时间列（零值时间 = 底座未给时戳——占位不伪造）。
func renderRowTime(ts interface{ AsTime() time.Time }) string {
	if ts == nil {
		return "-"
	}
	t := ts.AsTime()
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// parseTimeArg 解析时间窗旗标：RFC3339 绝对时刻，或相对当下的时长
//（Go duration 语法外加天单位 d——运维高频；负值/零值拒绝，窗口语义
// 只有「往回看」）。
func parseTimeArg(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if strings.Contains(s, "T") {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, err
		}
		return t, nil
	}
	d, err := parseWindowDuration(s)
	if err != nil {
		return time.Time{}, err
	}
	return time.Now().Add(-d), nil
}

// parseWindowDuration 解析相对时长：time.ParseDuration 语法外加 `d`（天）。
// 展开实现：数字段后随 'd' 即折算为 24h 单位；其余原样交给 ParseDuration。
func parseWindowDuration(s string) (time.Duration, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] < '0' || s[i] > '9' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		num := s[i:j]
		if j < len(s) && s[j] == 'd' {
			n, err := strconv.Atoi(num)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad day count in %q", s)
			}
			fmt.Fprintf(&b, "%dh", n*24)
			i = j + 1
			continue
		}
		b.WriteString(num)
		i = j
	}
	d, err := time.ParseDuration(b.String())
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("window must be positive")
	}
	return d, nil
}

// logsBackendCmd 实现 `fleetly logs backend <show|set>`（E6 W5-S1，设计
// §2.2）：show 输出后端模式/是否显式设置/部署态/ingest streak/丢弃计数；
// set 切换 backend（保存即生效——后台收敛部署或移除，数据卷保留）。
type logsBackendCmd struct {
	sub *commands.App
}

func newLogsBackendCmd() *logsBackendCmd {
	sub := commands.New()
	sub.Register(&logsBackendShowCmd{}, &logsBackendSetCmd{})
	sub.VerbTitle = "backend subcommands:"
	return &logsBackendCmd{sub: sub}
}

func (c *logsBackendCmd) Name() string { return "backend" }
func (c *logsBackendCmd) Synopsis() string {
	return "log backend view and switch (victorialogs | jsonl)"
}
func (c *logsBackendCmd) Usage() string {
	return "logs backend <show|set> [flags] [victorialogs|jsonl]"
}

func (c *logsBackendCmd) SetFlags(_ *flag.FlagSet) {}

func (c *logsBackendCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (show|set)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// logsBackendShowCmd 实现 `fleetly logs backend show`。
type logsBackendShowCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *logsBackendShowCmd) Name() string { return "show" }
func (c *logsBackendShowCmd) Synopsis() string {
	return "show the active log backend, deployment state, ingest streak and dropped counter"
}
func (c *logsBackendShowCmd) Usage() string {
	return "logs backend show [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *logsBackendShowCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *logsBackendShowCmd) Run(ctx context.Context, env *commands.Environment, _ []string) error {
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Logs().GetLogsBackend(ctx, &serverv1.GetLogsBackendRequest{})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		v := resp.GetView()
		var b strings.Builder
		fmt.Fprintf(&b, "backend: %s", v.GetBackend())
		if !v.GetBackendSet() {
			b.WriteString(" (default; not explicitly set)")
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "deployment: %s\n", v.GetDeployment())
		if v.GetIngestDegraded() {
			since := ""
			if t := v.GetIngestDegradedSince(); t != nil {
				since = " since " + t.AsTime().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "ingest: degraded%s (search degraded; live tail unaffected)\n", since)
		} else {
			b.WriteString("ingest: ok\n")
		}
		fmt.Fprintf(&b, "dropped_total: %d\n", v.GetDroppedTotal())
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// logsBackendSetCmd 实现 `fleetly logs backend set <victorialogs|jsonl>`
//（位置参数在后，旗标在前——本仓 CLI 约定）。
type logsBackendSetCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *logsBackendSetCmd) Name() string { return "set" }
func (c *logsBackendSetCmd) Synopsis() string {
	return "switch the log backend (deploying or removing the managed VictoriaLogs; the data volume is retained)"
}
func (c *logsBackendSetCmd) Usage() string {
	return "logs backend set [--addr <host:port>] [--token <tok>] [--json] <victorialogs|jsonl>"
}

func (c *logsBackendSetCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *logsBackendSetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Logs().SetLogsBackend(ctx, &serverv1.SetLogsBackendRequest{Backend: args[0]})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		v := resp.GetView()
		_, err = fmt.Fprintf(env.Stdout,
			"backend set to %s (deployment: %s; the manager converges the managed service shortly)\n",
			v.GetBackend(), v.GetDeployment())
		return err
	})
}

// 编译期断言：logs 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &logsCmd{}
	_ commands.Flagged = &logsCmd{}
	_ commands.Command = &logsFollowCmd{}
	_ commands.Flagged = &logsFollowCmd{}
	_ commands.Command = &logsHistoryCmd{}
	_ commands.Flagged = &logsHistoryCmd{}
	_ commands.Command = &logsSearchCmd{}
	_ commands.Flagged = &logsSearchCmd{}
	_ commands.Command = &logsBackendCmd{}
	_ commands.Flagged = &logsBackendCmd{}
	_ commands.Command = &logsBackendShowCmd{}
	_ commands.Flagged = &logsBackendShowCmd{}
	_ commands.Command = &logsBackendSetCmd{}
	_ commands.Flagged = &logsBackendSetCmd{}
)
