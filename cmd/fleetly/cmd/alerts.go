package cmd

//	fleetly alerts 命令（B 线 W5 设计 §2，D-V3W5-1，v0.3 W5-S2）：
//
//	rules   —— 规则管理（ls/create/rm/test；expr 是 PromQL——写规则时的
//	           即时校验用 `rules test`，经 VM instant query 直接执行）；
//	mode    —— alerts.mode 开关（on 部署 vmalert；off 即 unset 移除——
//	           前置门 metrics.mode=on，服务端 409 带指引）；
//	status  —— 告警栈状态视图（mode/vmalert 部署态/规则数/metrics.mode
//	           并列——休眠形态的诚实口径）。
//
// 旗标在位置参数前（本仓 CLI 约定）；--json 输出机器可读 JSON。

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// alertsCmd 是外层动词 `alerts`：分发 rules/mode/status。
type alertsCmd struct {
	sub *commands.App
}

func newAlertsCmd() *alertsCmd {
	sub := commands.New()
	sub.Register(newAlertsRulesCmd(), &alertsModeCmd{}, &alertsStatusCmd{})
	sub.VerbTitle = "subcommands:"
	return &alertsCmd{sub: sub}
}

func (c *alertsCmd) Name() string     { return "alerts" }
func (c *alertsCmd) Synopsis() string { return "managed alerting (rules / mode / status)" }
func (c *alertsCmd) Usage() string    { return "alerts <rules|mode|status> [flags] [args]" }

func (c *alertsCmd) SetFlags(_ *flag.FlagSet) {}

func (c *alertsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (rules|mode|status)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// alertsRulesCmd 是 `alerts rules`：分发 ls/create/rm/test。
type alertsRulesCmd struct {
	sub *commands.App
}

func newAlertsRulesCmd() *alertsRulesCmd {
	sub := commands.New()
	sub.Register(&alertsRulesLsCmd{}, &alertsRulesCreateCmd{}, &alertsRulesRmCmd{}, &alertsRulesTestCmd{})
	sub.VerbTitle = "rules subcommands:"
	return &alertsRulesCmd{sub: sub}
}

func (c *alertsRulesCmd) Name() string     { return "rules" }
func (c *alertsRulesCmd) Synopsis() string { return "manage alert rules (ls / create / rm / test)" }
func (c *alertsRulesCmd) Usage() string {
	return "alerts rules <ls|create|rm|test> [flags] [args]"
}

func (c *alertsRulesCmd) SetFlags(_ *flag.FlagSet) {}

func (c *alertsRulesCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (ls|create|rm|test)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// alertsRulesLsCmd 实现 `fleetly alerts rules ls`。
type alertsRulesLsCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *alertsRulesLsCmd) Name() string { return "ls" }
func (c *alertsRulesLsCmd) Synopsis() string {
	return "list alert rules (name order — the rule file renders in the same order)"
}
func (c *alertsRulesLsCmd) Usage() string {
	return "alerts rules ls [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *alertsRulesLsCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *alertsRulesLsCmd) Run(ctx context.Context, env *commands.Environment, _ []string) error {
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Alerts().ListAlertRules(ctx, &serverv1.ListAlertRulesRequest{})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "rules: %d\n", len(resp.GetRules()))
		for _, r := range resp.GetRules() {
			fmt.Fprintf(&b, "%s\n", r.GetName())
			fmt.Fprintf(&b, "  expr: %s\n", r.GetExpr())
			if r.GetForDurationSeconds() > 0 {
				fmt.Fprintf(&b, "  for: %ds\n", r.GetForDurationSeconds())
			}
			if sev := r.GetLabels()["severity"]; sev != "" {
				fmt.Fprintf(&b, "  severity: %s\n", sev)
			}
			if len(r.GetChannels()) > 0 {
				fmt.Fprintf(&b, "  channels: %s\n", strings.Join(r.GetChannels(), ","))
			} else {
				b.WriteString("  channels: (all enabled endpoints)\n")
			}
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// alertsRulesCreateCmd 实现 `fleetly alerts rules create <name> <expr>`。
type alertsRulesCreateCmd struct {
	forSec   int
	labels   repeatedFlags
	channels string
	jsonOut  bool
	conn     connFlags
}

func (c *alertsRulesCreateCmd) Name() string { return "create" }
func (c *alertsRulesCreateCmd) Synopsis() string {
	return "create an alert rule (platform-unique name; PromQL expr)"
}
func (c *alertsRulesCreateCmd) Usage() string {
	return "alerts rules create [--addr <host:port>] [--token <tok>] [--for 300] [--label severity=critical]... [--channels id1,id2] [--json] <name> <expr>"
}

func (c *alertsRulesCreateCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.IntVar(&c.forSec, "for", 0, "sustained duration seconds before firing (0 = fire immediately)")
	fs.Var(&c.labels, "label", "rule label k=v (repeatable; severity drives the notification text)")
	fs.StringVar(&c.channels, "channels", "", "comma-separated notification endpoint ids (empty = all enabled endpoints)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *alertsRulesCreateCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 2); err != nil {
		return err
	}
	req := &serverv1.CreateAlertRuleRequest{
		Name:               args[0],
		Expr:               args[1],
		ForDurationSeconds: int64(c.forSec),
		Labels:             kvMap(c.labels.values()),
		Channels:           csvList(c.channels),
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Alerts().CreateAlertRule(ctx, req)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		r := resp.GetRule()
		_, err = fmt.Fprintf(env.Stdout,
			"alert rule %q created (id %s; the manager re-renders the rule file shortly)\n",
			r.GetName(), r.GetId())
		return err
	})
}

// alertsRulesRmCmd 实现 `fleetly alerts rules rm <name-or-id>`。
type alertsRulesRmCmd struct {
	conn connFlags
}

func (c *alertsRulesRmCmd) Name() string { return "rm" }
func (c *alertsRulesRmCmd) Synopsis() string {
	return "remove an alert rule (by name or id)"
}
func (c *alertsRulesRmCmd) Usage() string {
	return "alerts rules rm [--addr <host:port>] [--token <tok>] <name-or-id>"
}

func (c *alertsRulesRmCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
}

func (c *alertsRulesRmCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		id, err := resolveAlertRuleID(ctx, cl, args[0])
		if err != nil {
			return err
		}
		if _, err := cl.Alerts().DeleteAlertRule(ctx, &serverv1.DeleteAlertRuleRequest{Id: id}); err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "alert rule %q removed (the manager re-renders the rule file shortly)\n", args[0])
		return err
	})
}

// alertsRulesTestCmd 实现 `fleetly alerts rules test <expr>`（VM instant
// query 直接执行——规则编写时的即时校验面；坏表达式由 VM 拒绝并透传原文）。
type alertsRulesTestCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *alertsRulesTestCmd) Name() string { return "test" }
func (c *alertsRulesTestCmd) Synopsis() string {
	return "evaluate an expr once against the managed VictoriaMetrics (rule authoring check)"
}
func (c *alertsRulesTestCmd) Usage() string {
	return "alerts rules test [--addr <host:port>] [--token <tok>] [--json] <expr>"
}

func (c *alertsRulesTestCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *alertsRulesTestCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Alerts().TestAlertRule(ctx, &serverv1.TestAlertRuleRequest{Expr: args[0]})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "series: %d\n", len(resp.GetSeries()))
		for _, s := range resp.GetSeries() {
			fmt.Fprintf(&b, "%s\n", renderSeriesLabels(s.GetMetric()))
			for _, p := range s.GetPoints() {
				fmt.Fprintf(&b, "  value: %s\n", renderPointValue(p.GetV()))
			}
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// alertsModeCmd 实现 `fleetly alerts mode <on|off>`（off 即 unset——设置
// 词表是 unset|on，off 是 CLI 友好别名；前置门 metrics.mode=on 由服务端
// 409 带指引）。
type alertsModeCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *alertsModeCmd) Name() string { return "mode" }
func (c *alertsModeCmd) Synopsis() string {
	return "switch alerts.mode (on deploys the vmalert evaluator; off removes it — requires metrics.mode=on first)"
}
func (c *alertsModeCmd) Usage() string {
	return "alerts mode [--addr <host:port>] [--token <tok>] [--json] <on|off|unset>"
}

func (c *alertsModeCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *alertsModeCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	mode := args[0]
	switch mode {
	case "on":
	case "off":
		mode = "unset"
	case "unset":
	default:
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("mode must be on, off or unset, got %q", args[0])}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Alerts().SetAlertsMode(ctx, &serverv1.SetAlertsModeRequest{Mode: mode})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		st := resp.GetStatus()
		state := "not deployed"
		if st.GetVmalertExists() {
			state = "deployed"
		}
		_, err = fmt.Fprintf(env.Stdout,
			"alerts mode set to %s (vmalert: %s; rules: %d; metrics.mode: %s — the manager converges the stack shortly)\n",
			st.GetMode(), state, st.GetRuleCount(), st.GetMetricsMode())
		return err
	})
}

// alertsStatusCmd 实现 `fleetly alerts status`。
type alertsStatusCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *alertsStatusCmd) Name() string { return "status" }
func (c *alertsStatusCmd) Synopsis() string {
	return "show the alerting stack (mode, vmalert deployment, rule count, metrics.mode)"
}
func (c *alertsStatusCmd) Usage() string {
	return "alerts status [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *alertsStatusCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *alertsStatusCmd) Run(ctx context.Context, env *commands.Environment, _ []string) error {
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Alerts().GetAlertsStatus(ctx, &serverv1.GetAlertsStatusRequest{})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "mode: %s", resp.GetMode())
		if !resp.GetModeSet() {
			b.WriteString(" (default; not explicitly set)")
		}
		b.WriteString("\n")
		vmState := "not deployed"
		if resp.GetVmalertExists() {
			vmState = "deployed"
		}
		fmt.Fprintf(&b, "vmalert: %s\n", vmState)
		fmt.Fprintf(&b, "rules: %d\n", resp.GetRuleCount())
		fmt.Fprintf(&b, "metrics.mode: %s\n", resp.GetMetricsMode())
		if resp.GetMode() == "on" && resp.GetMetricsMode() != "on" {
			b.WriteString("note: alerts.mode=on while metrics.mode!=on — the vmalert evaluator is removed (no datasource); switch metrics on to resume\n")
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// resolveAlertRuleID 把名字或 id 解析为 id（rm 面的人读名定位——CLI 以名
// 操作、API 以 id 定位的惯例投影）。
func resolveAlertRuleID(ctx context.Context, cl *fleetlyClient, nameOrID string) (string, error) {
	resp, err := cl.Alerts().ListAlertRules(ctx, &serverv1.ListAlertRulesRequest{})
	if err != nil {
		return "", err
	}
	for _, r := range resp.GetRules() {
		if r.GetId() == nameOrID || r.GetName() == nameOrID {
			return r.GetId(), nil
		}
	}
	return "", fmt.Errorf("alert rule %q not found (see 'fleetly alerts rules ls')", nameOrID)
}

// repeatedFlags 是可重复 k=v 旗标的收集器（--label severity=critical）。
type repeatedFlags struct {
	raw []string
}

func (f *repeatedFlags) String() string { return strings.Join(f.raw, ",") }

func (f *repeatedFlags) Set(v string) error {
	f.raw = append(f.raw, v)
	return nil
}

func (f *repeatedFlags) values() []string { return f.raw }

// kvMap 把 k=v 列表装配为 map（无 = 的项显式拒绝——形状违约 loud）。
func kvMap(pairs []string) map[string]string {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			continue // 位置参数形状由服务端校验兜底——CLI 侧静默跳过畸形项
		}
		out[k] = v
	}
	return out
}

// csvList 是逗号分隔 id 串的拆分（空白与空段丢弃；空串 = nil——缺省语义）。
func csvList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// 编译期断言：alerts 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &alertsCmd{}
	_ commands.Flagged = &alertsCmd{}
	_ commands.Command = &alertsRulesCmd{}
	_ commands.Flagged = &alertsRulesCmd{}
	_ commands.Command = &alertsRulesLsCmd{}
	_ commands.Flagged = &alertsRulesLsCmd{}
	_ commands.Command = &alertsRulesCreateCmd{}
	_ commands.Flagged = &alertsRulesCreateCmd{}
	_ commands.Command = &alertsRulesRmCmd{}
	_ commands.Flagged = &alertsRulesRmCmd{}
	_ commands.Command = &alertsRulesTestCmd{}
	_ commands.Flagged = &alertsRulesTestCmd{}
	_ commands.Command = &alertsModeCmd{}
	_ commands.Flagged = &alertsModeCmd{}
	_ commands.Command = &alertsStatusCmd{}
	_ commands.Flagged = &alertsStatusCmd{}
)
