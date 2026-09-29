package cmd

//	fleetly metrics 命令（E6 观测专项设计 §4/§4.2，W5-S3；D-W5-2 opt-in）：
//
//	status  —— 托管三件套状态视图（模式/三件部署态/节点上报比/retention——
//	           「N/M nodes reporting」的诚实口径：跨节点采集走节点 advertise
//	           地址直连（VPC/LAN），缺席 = 节点不 Ready 或防火墙拦 VPC）；
//	mode    —— 模式切换（unset|on；保存即生效——后台收敛部署/移除，
//	           数据卷保留）；
//	query   —— PromQL 区间查询（透传——操作员工具，不做查询沙箱；
//	           VM 不可达/未启用以服务端信封诚实报错）。
//
// 旗标在位置参数前（本仓 CLI 约定）；--json 输出机器可读 JSON。

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/lynx-go/commands"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// metricsCmd 是外层动词 `metrics`：分发 status/mode/query。
type metricsCmd struct {
	sub *commands.App
}

func newMetricsCmd() *metricsCmd {
	sub := commands.New()
	sub.Register(&metricsStatusCmd{}, newMetricsModeCmd(), &metricsQueryCmd{})
	sub.VerbTitle = "metrics subcommands:"
	return &metricsCmd{sub: sub}
}

func (c *metricsCmd) Name() string     { return "metrics" }
func (c *metricsCmd) Synopsis() string { return "managed metrics stack (status / mode / PromQL query)" }
func (c *metricsCmd) Usage() string    { return "metrics <status|mode|query> [flags] [args]" }

func (c *metricsCmd) SetFlags(_ *flag.FlagSet) {}

func (c *metricsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (status|mode|query)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// metricsStatusCmd 实现 `fleetly metrics status`。
type metricsStatusCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *metricsStatusCmd) Name() string { return "status" }
func (c *metricsStatusCmd) Synopsis() string {
	return "show the managed metrics stack (mode, components, nodes reporting, retention)"
}
func (c *metricsStatusCmd) Usage() string {
	return "metrics status [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *metricsStatusCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *metricsStatusCmd) Run(ctx context.Context, env *commands.Environment, _ []string) error {
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Metrics().GetMetricsStatus(ctx, &serverv1.GetMetricsStatusRequest{})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		var b strings.Builder
		st := resp
		fmt.Fprintf(&b, "mode: %s", st.GetMode())
		if !st.GetModeSet() {
			b.WriteString(" (default; not explicitly set)")
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "retention_days: %d\n", st.GetRetentionDays())
		for _, comp := range st.GetComponents() {
			state := "not deployed"
			if comp.GetExists() {
				state = "deployed"
			}
			fmt.Fprintf(&b, "component %s: %s\n", comp.GetName(), state)
		}
		fmt.Fprintf(&b, "nodes_reporting: %d/%d\n", st.GetNodesReporting(), st.GetNodesTotal())
		if st.GetMode() == "on" {
			// 暴露面诚实口径（spec 注释 / Console 文案三处同锚）：采集端口
			// 绑 0.0.0.0 对节点全部接口开放——采集面 = 内网面，公网访问由
			// 节点/云防火墙负责。
			b.WriteString("note: collector ports listen on all node interfaces (VPC/LAN face); " +
				"the scrape face is an intranet face — public access is expected to be blocked by the node firewall\n")
			if st.GetNodesReporting() < st.GetNodesTotal() {
				// 诚实口径（设计 §4.1 + §6 挂账票修订）：跨节点采集走节点
				// advertise 地址直连（VPC/LAN，不依赖 overlay 数据面）——
				// 缺席只剩两种因由：节点不 Ready，或 manager 不可达它
				//（防火墙拦 VPC 内 8080/9100）。不谎报全量。
				b.WriteString("note: cross-node collection goes over direct node addresses (VPC/LAN), not the overlay data plane; " +
					"a node below full count is not Ready or unreachable from the manager (check the node firewall for tcp/8080 and tcp/9100)\n")
			}
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// metricsModeCmd 是外层动词 `metrics mode`：分发 show/set。
type metricsModeCmd struct {
	sub *commands.App
}

func newMetricsModeCmd() *metricsModeCmd {
	sub := commands.New()
	sub.Register(&metricsModeShowCmd{}, &metricsModeSetCmd{})
	sub.VerbTitle = "mode subcommands:"
	return &metricsModeCmd{sub: sub}
}

func (c *metricsModeCmd) Name() string     { return "mode" }
func (c *metricsModeCmd) Synopsis() string { return "view and switch the metrics mode (unset | on)" }
func (c *metricsModeCmd) Usage() string    { return "metrics mode <show|set> [flags] [unset|on]" }

func (c *metricsModeCmd) SetFlags(_ *flag.FlagSet) {}

func (c *metricsModeCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (show|set)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// metricsModeShowCmd 实现 `fleetly metrics mode show`（status 同面去组件
// 明细——只读模式视图）。
type metricsModeShowCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *metricsModeShowCmd) Name() string { return "show" }
func (c *metricsModeShowCmd) Synopsis() string {
	return "show the current metrics mode (unset = opt-in default, nothing deployed)"
}
func (c *metricsModeShowCmd) Usage() string {
	return "metrics mode show [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *metricsModeShowCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *metricsModeShowCmd) Run(ctx context.Context, env *commands.Environment, _ []string) error {
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Metrics().GetMetricsStatus(ctx, &serverv1.GetMetricsStatusRequest{})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		st := resp
		var b strings.Builder
		fmt.Fprintf(&b, "mode: %s", st.GetMode())
		if !st.GetModeSet() {
			b.WriteString(" (default; not explicitly set)")
		}
		b.WriteString("\n")
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// metricsModeSetCmd 实现 `fleetly metrics mode set <unset|on>`（位置参数在
// 后，旗标在前——本仓 CLI 约定）。set on = 三件套部署的 opt-in 置位（deploy
// scope）；set unset = 三件移除、数据卷保留。
type metricsModeSetCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *metricsModeSetCmd) Name() string { return "set" }
func (c *metricsModeSetCmd) Synopsis() string {
	return "switch the metrics mode (on deploys VictoriaMetrics/cAdvisor/node-exporter; unset removes the services, the data volume is retained)"
}
func (c *metricsModeSetCmd) Usage() string {
	return "metrics mode set [--addr <host:port>] [--token <tok>] [--json] <unset|on>"
}

func (c *metricsModeSetCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *metricsModeSetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Metrics().SetMetricsMode(ctx, &serverv1.SetMetricsModeRequest{Mode: args[0]})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		st := resp.GetStatus()
		deployed := 0
		for _, comp := range st.GetComponents() {
			if comp.GetExists() {
				deployed++
			}
		}
		_, err = fmt.Fprintf(env.Stdout,
			"metrics mode set to %s (components deployed: %d/3; the manager converges the stack shortly; the data volume survives switching off)\n",
			st.GetMode(), deployed)
		return err
	})
}

// metricsQueryCmd 实现 `fleetly metrics query <promql> [--since 1h]
// [--step 60] [--json]`（E6 设计 §4.2 CLI 面）：PromQL 透传区间查询。
// 时间窗旗标复用 logs search 的相对时长解析（30m/1h/7d/RFC3339）。
type metricsQueryCmd struct {
	since   string
	until   string
	step    int
	jsonOut bool
	conn    connFlags
}

func (c *metricsQueryCmd) Name() string { return "query" }
func (c *metricsQueryCmd) Synopsis() string {
	return "run a PromQL range query against the managed VictoriaMetrics (passed through verbatim — operator tool)"
}
func (c *metricsQueryCmd) Usage() string {
	return "metrics query [--addr <host:port>] [--token <tok>] [--since 1h|30m|RFC3339] [--until ...] [--step 60] [--json] <promql>"
}

func (c *metricsQueryCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.since, "since", "1h", "window start: relative to now (30m, 1h, 7d) or RFC3339")
	fs.StringVar(&c.until, "until", "", "window end: relative to now (30m, 1h, 7d) or RFC3339 (default = now)")
	fs.IntVar(&c.step, "step", 60, "step seconds")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *metricsQueryCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
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
		req := &serverv1.SearchMetricsRequest{
			Query:       args[0],
			StepSeconds: int32Clamp(c.step),
		}
		if !start.IsZero() {
			req.TimeStart = timestamppb.New(start)
		}
		if !end.IsZero() {
			req.TimeEnd = timestamppb.New(end)
		}
		resp, err := cl.Metrics().SearchMetrics(ctx, req)
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
				fmt.Fprintf(&b, "  %s  %s\n",
					time.Unix(p.GetT(), 0).UTC().Format(time.RFC3339), renderPointValue(p.GetV()))
			}
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// renderSeriesLabels 是序列的 label 集渲染（{name="up",job="x"} 形态；
// __name__ 提升为首列名——PromQL 惯例的可读投影）。
func renderSeriesLabels(metric map[string]string) string {
	pairs := make([]string, 0, len(metric))
	for k, v := range metric {
		if k == "__name__" {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%q", k, v))
	}
	name := metric["__name__"]
	if name == "" {
		name = "{...}"
	}
	return fmt.Sprintf("%s{%s}", name, strings.Join(pairs, ","))
}

// renderPointValue 是采样值渲染（整数不带小数点；NaN 如实输出）。
func renderPointValue(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// 编译期断言：metrics 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &metricsCmd{}
	_ commands.Flagged = &metricsCmd{}
	_ commands.Command = &metricsStatusCmd{}
	_ commands.Flagged = &metricsStatusCmd{}
	_ commands.Command = &metricsModeCmd{}
	_ commands.Flagged = &metricsModeCmd{}
	_ commands.Command = &metricsModeShowCmd{}
	_ commands.Flagged = &metricsModeShowCmd{}
	_ commands.Command = &metricsModeSetCmd{}
	_ commands.Flagged = &metricsModeSetCmd{}
	_ commands.Command = &metricsQueryCmd{}
	_ commands.Flagged = &metricsQueryCmd{}
)
