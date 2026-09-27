package cmd

// 任务命令（T 线 DT-5 / IMPL-T2-1）：程序化动态工作负载的 CLI 面。
//
//	fleetly tasks run     受理任务（queued；引擎收敛到 running 后可按
//	                      dns 名在作用域网内寻址）
//	fleetly tasks ls      列本令牌任务（缺省只出在途；--all 出台账）
//	fleetly tasks stop    停止任务（幂等；底座服务由引擎移除）
//	fleetly tasks rm      删除任务（停止 + 移除底座服务 + 台账行删除）
//	fleetly tasks logs    检索任务日志（VL 日志库；task 标签归因）
//	fleetly tasks network ensure  task-group 网络幂等 ensure（长活）+ 控制面
//	                     服务一次性挂靠（--member app=service，可重复）
//
// 凭据：整体 tasks 独立 scope（机具令牌典型持有者；read/deploy 不蕴含——
// tokens create --scopes tasks）。任务 env 值只在 CreateTask 请求里出现
// （服务端密文落库）；CLI 不复述值。

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// tasksCmd 是外层动词 `tasks`。
type tasksCmd struct {
	sub *commands.App
}

func newTasksCmd() *tasksCmd {
	sub := commands.New()
	sub.Register(&tasksRunCmd{}, &tasksListCmd{}, &tasksStopCmd{}, &tasksRemoveCmd{}, &tasksLogsCmd{}, newTasksNetworkCmd())
	sub.VerbTitle = "tasks subcommands:"
	return &tasksCmd{sub: sub}
}

func (c *tasksCmd) Name() string { return "tasks" }
func (c *tasksCmd) Synopsis() string {
	return "dynamic workloads (swarm-backed tasks: hardened by default, TTL-reclaimed, scoped to an existing network)"
}
func (c *tasksCmd) Usage() string { return "tasks <run|ls|stop|rm|logs|network> [flags] ..." }

func (c *tasksCmd) SetFlags(_ *flag.FlagSet) {}

func (c *tasksCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (run|ls|stop|rm|logs|network)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// stringListFlag 是可重复字符串 flag（--env K=V / --command ... / --member ...）。
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// tasksRunCmd 实现 `fleetly tasks run`。
type tasksRunCmd struct {
	name      string
	image     string
	scopeKind string
	scopeRef  string
	internal  bool
	ttl       time.Duration
	cpuMillis int64
	memoryMB  int64
	command   stringListFlag
	args      stringListFlag
	env       stringListFlag
	jsonOut   bool
	conn      connFlags
}

func (c *tasksRunCmd) Name() string { return "run" }
func (c *tasksRunCmd) Synopsis() string {
	return "accept a task (digest-pinned image, server-enforced hardening, owner/TTL labels); it converges asynchronously — poll with 'tasks ls'"
}
func (c *tasksRunCmd) Usage() string {
	return "tasks run --image <ref> --scope-kind <app|project|task-group> --scope-ref <ref> [--internal] [--name <n>] [--command <c>]... [--arg <a>]... [--env K=V]... [--ttl 10m] [--cpu-millis 1000] [--memory-mb 256] [--json]"
}

func (c *tasksRunCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.name, "name", "", "human-readable task name")
	fs.StringVar(&c.image, "image", "", "image reference (tag is resolved to a digest server-side)")
	fs.StringVar(&c.scopeKind, "scope-kind", "", "scope kind: app | project | task-group")
	fs.StringVar(&c.scopeRef, "scope-ref", "", "scope reference (app name / project id or team/prj / task-group ref)")
	fs.BoolVar(&c.internal, "internal", false, "internal network variant (task-group scope only: no egress, untrusted workloads)")
	fs.DurationVar(&c.ttl, "ttl", 10*time.Minute, "task TTL (60s..24h; reclaimed by the janitor on expiry)")
	fs.Int64Var(&c.cpuMillis, "cpu-millis", 1000, "CPU request in millicores (1..4000)")
	fs.Int64Var(&c.memoryMB, "memory-mb", 256, "memory request in MiB (1..4096)")
	fs.Var(&c.command, "command", "override the image ENTRYPOINT (repeatable)")
	fs.Var(&c.args, "arg", "override the image CMD (repeatable)")
	fs.Var(&c.env, "env", "environment variable KEY=VALUE (repeatable; stored encrypted server-side)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *tasksRunCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 0); err != nil {
		return err
	}
	if strings.TrimSpace(c.image) == "" || strings.TrimSpace(c.scopeKind) == "" || strings.TrimSpace(c.scopeRef) == "" {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("--image, --scope-kind and --scope-ref are required")}
	}
	envMap, err := parseTaskEnvFlags(c.env)
	if err != nil {
		return &commands.UsageError{Usage: c.Usage(), Err: err}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Tasks().CreateTask(ctx, &serverv1.CreateTaskRequest{
			Name:        c.name,
			Image:       c.image,
			Command:     c.command,
			Args:        c.args,
			Env:         envMap,
			Scope:       &serverv1.TaskScope{Kind: c.scopeKind, Ref: c.conn.ref(c.scopeRef), Internal: c.internal},
			TtlSeconds:  int64(c.ttl / time.Second),
			CpuMillis:   c.cpuMillis,
			MemoryBytes: c.memoryMB << 20,
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, taskJSON(resp.GetTask()))
		}
		_, err = fmt.Fprintln(env.Stdout, taskLine(resp.GetTask()))
		return err
	})
}

// tasksListCmd 实现 `fleetly tasks ls`。
type tasksListCmd struct {
	all     bool
	limit   int
	jsonOut bool
	conn    connFlags
}

func (c *tasksListCmd) Name() string { return "ls" }
func (c *tasksListCmd) Synopsis() string {
	return "list tasks owned by this token (non-terminal by default; --all includes the terminal ledger)"
}
func (c *tasksListCmd) Usage() string {
	return "tasks ls [--addr <host:port>] [--token <tok>] [--all] [--limit 100] [--json]"
}

func (c *tasksListCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.all, "all", false, "include terminal tasks (stopped/failed)")
	fs.IntVar(&c.limit, "limit", 100, "maximum rows (<= 500)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *tasksListCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 0); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Tasks().ListTasks(ctx, &serverv1.ListTasksRequest{
			IncludeTerminal: c.all,
			Limit:           int32(c.limit),
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			rows := make([]map[string]any, 0, len(resp.GetTasks()))
			for _, t := range resp.GetTasks() {
				rows = append(rows, taskJSON(t))
			}
			return writeJSON(env.Stdout, map[string]any{"tasks": rows})
		}
		if len(resp.GetTasks()) == 0 {
			_, err := fmt.Fprintln(env.Stdout, "no tasks")
			return err
		}
		for _, t := range resp.GetTasks() {
			if _, err := fmt.Fprintln(env.Stdout, taskLine(t)); err != nil {
				return err
			}
		}
		return nil
	})
}

// tasksStopCmd 实现 `fleetly tasks stop`。
type tasksStopCmd struct {
	conn connFlags
}

func (c *tasksStopCmd) Name() string { return "stop" }
func (c *tasksStopCmd) Synopsis() string {
	return "stop a task (idempotent; the engine removes the substrate service)"
}
func (c *tasksStopCmd) Usage() string {
	return "tasks stop [--addr <host:port>] [--token <tok>] <id>"
}

func (c *tasksStopCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *tasksStopCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Tasks().StopTask(ctx, &serverv1.StopTaskRequest{Id: args[0]})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(env.Stdout, taskLine(resp.GetTask()))
		return err
	})
}

// tasksRemoveCmd 实现 `fleetly tasks rm`。
type tasksRemoveCmd struct {
	conn connFlags
}

func (c *tasksRemoveCmd) Name() string { return "rm" }
func (c *tasksRemoveCmd) Synopsis() string {
	return "delete a task (stops it, removes the substrate service, then removes the ledger row)"
}
func (c *tasksRemoveCmd) Usage() string {
	return "tasks rm [--addr <host:port>] [--token <tok>] <id>"
}

func (c *tasksRemoveCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *tasksRemoveCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Tasks().DeleteTask(ctx, &serverv1.DeleteTaskRequest{Id: args[0]})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "task %s deleted\n", resp.GetId())
		return err
	})
}

// tasksLogsCmd 实现 `fleetly tasks logs`：复用日志检索面（SearchLogs 的
// task 选择器）——日志库必须为 victorialogs（jsonl 形态无任务检索面，
// 服务端以 E_LOGS_BACKEND_UNAVAILABLE 诚实报错）。
type tasksLogsCmd struct {
	keyword string
	limit   int
	jsonOut bool
	conn    connFlags
}

func (c *tasksLogsCmd) Name() string { return "logs" }
func (c *tasksLogsCmd) Synopsis() string {
	return "search task logs in the log backend (VictoriaLogs; task-labelled stream)"
}
func (c *tasksLogsCmd) Usage() string {
	return "tasks logs [--addr <host:port>] [--token <tok>] [--keyword <k>] [--limit 200] [--json] <id>"
}

func (c *tasksLogsCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.keyword, "keyword", "", "literal keyword phrase filter")
	fs.IntVar(&c.limit, "limit", 200, "maximum rows (<= 1000)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *tasksLogsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Logs().SearchLogs(ctx, &serverv1.SearchLogsRequest{
			Tasks:   []string{args[0]},
			Keyword: c.keyword,
			Limit:   int32(c.limit),
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, resp)
		}
		if len(resp.GetRows()) == 0 {
			_, err := fmt.Fprintln(env.Stdout, "no log rows")
			return err
		}
		for _, row := range resp.GetRows() {
			at := ""
			if row.GetAt() != nil {
				at = row.GetAt().AsTime().UTC().Format(time.RFC3339)
			}
			if _, err := fmt.Fprintf(env.Stdout, "%s %s\n", at, row.GetMsg()); err != nil {
				return err
			}
		}
		return nil
	})
}

// tasksNetworkCmd 是 `tasks network` 子命令组。
type tasksNetworkCmd struct {
	sub *commands.App
}

func newTasksNetworkCmd() *tasksNetworkCmd {
	sub := commands.New()
	sub.Register(&tasksNetworkEnsureCmd{})
	sub.VerbTitle = "tasks network subcommands:"
	return &tasksNetworkCmd{sub: sub}
}

func (c *tasksNetworkCmd) Name() string { return "network" }
func (c *tasksNetworkCmd) Synopsis() string {
	return "task-group networks (long-lived scope networks: tasks join them by name; control-plane services attach once)"
}
func (c *tasksNetworkCmd) Usage() string { return "network <ensure> [flags] <ref>" }

func (c *tasksNetworkCmd) SetFlags(_ *flag.FlagSet) {}

func (c *tasksNetworkCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (ensure)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// tasksNetworkEnsureCmd 实现 `fleetly tasks network ensure`。
type tasksNetworkEnsureCmd struct {
	internal bool
	members  stringListFlag
	conn     connFlags
}

func (c *tasksNetworkEnsureCmd) Name() string { return "ensure" }
func (c *tasksNetworkEnsureCmd) Synopsis() string {
	return "ensure a task-group network (idempotent, long-lived) and declare control-plane members attached once"
}
func (c *tasksNetworkEnsureCmd) Usage() string {
	return "tasks network ensure [--addr <host:port>] [--token <tok>] [--internal] [--member app=service]... <ref>"
}

func (c *tasksNetworkEnsureCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.internal, "internal", false, "internal variant (no egress, no external DNS — for untrusted workloads)")
	fs.Var(&c.members, "member", "control-plane member to attach once, as app=service (repeatable)")
}

func (c *tasksNetworkEnsureCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	members := make([]*serverv1.TaskNetworkMember, 0, len(c.members))
	for _, raw := range c.members {
		app, service, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(app) == "" || strings.TrimSpace(service) == "" {
			return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("--member must be app=service, got %q", raw)}
		}
		members = append(members, &serverv1.TaskNetworkMember{App: strings.TrimSpace(app), Service: strings.TrimSpace(service)})
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Tasks().EnsureTaskNetwork(ctx, &serverv1.EnsureTaskNetworkRequest{
			Ref:      args[0],
			Internal: c.internal,
			Members:  members,
		})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(env.Stdout, "task-group network %s internal=%v\n", resp.GetName(), resp.GetInternal()); err != nil {
			return err
		}
		for _, m := range resp.GetMembers() {
			deployment := m.GetDeploymentId()
			if deployment == "" {
				deployment = "-"
			}
			if _, err := fmt.Fprintf(env.Stdout, "  member %s service %s status=%s deployment=%s\n",
				m.GetApp(), m.GetService(), m.GetStatus(), deployment); err != nil {
				return err
			}
		}
		return nil
	})
}

// parseTaskEnvFlags 解析 --env KEY=VALUE 集（空键拒绝；重复键后者覆盖）。
func parseTaskEnvFlags(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for _, kv := range raw {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--env must be KEY=VALUE, got %q", kv)
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, nil
}

// taskLine 渲染任务（单行、字段齐备——脚本可解析）。
func taskLine(t *serverv1.TaskView) string {
	if t == nil {
		return "task <none>"
	}
	reason := t.GetStopReason()
	if reason == "" {
		reason = "-"
	}
	errText := t.GetError()
	if errText != "" {
		errText = " error=" + strconv.Quote(errText)
	}
	return fmt.Sprintf("task %s status=%s image=%s scope=%s/%s%s network-dns=%s ttl=%ds expires=%s reason=%s%s",
		t.GetId(), t.GetStatus(), t.GetImage(),
		t.GetScope().GetKind(), t.GetScope().GetRef(), internalSuffix(t.GetScope().GetInternal()),
		t.GetDnsName(), t.GetTtlSeconds(), taskTime(t.GetExpiresAt()), reason, errText)
}

func internalSuffix(internal bool) string {
	if internal {
		return " internal=true"
	}
	return ""
}

func taskTime(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "-"
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// taskJSON 是 --json 投影（视图字段全量；env 值不回显——GetTask 面才有
// env，且 values 是调用方自己的输入）。
func taskJSON(t *serverv1.TaskView) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	return map[string]any{
		"id":           t.GetId(),
		"name":         t.GetName(),
		"image":        t.GetImage(),
		"command":      t.GetCommand(),
		"args":         t.GetArgs(),
		"status":       t.GetStatus(),
		"scope_kind":   t.GetScope().GetKind(),
		"scope_ref":    t.GetScope().GetRef(),
		"internal":     t.GetScope().GetInternal(),
		"ttl_seconds":  t.GetTtlSeconds(),
		"cpu_millis":   t.GetCpuMillis(),
		"memory_bytes": t.GetMemoryBytes(),
		"service":      t.GetService(),
		"dns_name":     t.GetDnsName(),
		"error":        t.GetError(),
		"stop_reason":  t.GetStopReason(),
		"created_at":   taskTime(t.GetCreatedAt()),
		"started_at":   taskTime(t.GetStartedAt()),
		"expires_at":   taskTime(t.GetExpiresAt()),
		"stopped_at":   taskTime(t.GetStoppedAt()),
	}
}
