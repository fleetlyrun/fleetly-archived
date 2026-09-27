package cmd

// 项目命令（T 线 OT-1 / IMPL-T15-1 最小可用集）：`fleetly projects network
// <attach|detach|show>`——项目网参与的显式 opt-in 操作面与查询面。
//
// 语义（服务端为真值，CLI 只如实转述）：
//   - attach/detach 是显式变更；成员服务在下一次「参与变更重部署」中滚动
//     切换网络（attach 后双挂 app 私网 + 项目网，项目网别名 <app>-<service>；
//     detach 后摘除项目网）；
//   - 缺省不参加（既有 app 私网隔离现状）；平台角色门 = 项目角色 admin+
//    （admin scope + 第 2 门），平台管理员只读不代写；
//   - 在途部署存在时服务端 409 拒绝（重部署以最近一次成功部署为基）。
//
// show 读 GetApp 投影（项目限定形 + 参与状态 + 项目网名），无底座访问。

import (
	"context"
	"flag"
	"fmt"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// projectsCmd 是外层动词 `projects`：分派项目子命令面。
type projectsCmd struct {
	sub *commands.App
}

func newProjectsCmd() *projectsCmd {
	sub := commands.New()
	sub.Register(newProjectsNetworkCmd())
	sub.VerbTitle = "projects subcommands:"
	return &projectsCmd{sub: sub}
}

func (c *projectsCmd) Name() string { return "projects" }
func (c *projectsCmd) Synopsis() string {
	return "project resources (project network participation: explicit opt-in attach/detach)"
}
func (c *projectsCmd) Usage() string { return "projects <network> [flags] ..." }

func (c *projectsCmd) SetFlags(_ *flag.FlagSet) {}

func (c *projectsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (network)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// projectsNetworkCmd 是 `projects network` 子命令组：项目网参与三操作。
type projectsNetworkCmd struct {
	sub *commands.App
}

func newProjectsNetworkCmd() *projectsNetworkCmd {
	sub := commands.New()
	sub.Register(&projectNetworkAttachCmd{}, &projectNetworkDetachCmd{}, &projectNetworkShowCmd{})
	sub.VerbTitle = "projects network subcommands:"
	return &projectsNetworkCmd{sub: sub}
}

func (c *projectsNetworkCmd) Name() string { return "network" }
func (c *projectsNetworkCmd) Synopsis() string {
	return "project network participation (attach/detach an app; member services double-attach the app network and the project network)"
}
func (c *projectsNetworkCmd) Usage() string { return "network <attach|detach|show> [flags] <app>" }

func (c *projectsNetworkCmd) SetFlags(_ *flag.FlagSet) {}

func (c *projectsNetworkCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (attach|detach|show)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// projectNetworkAttachCmd 实现 `fleetly projects network attach <app>`。
type projectNetworkAttachCmd struct {
	conn connFlags
}

func (c *projectNetworkAttachCmd) Name() string { return "attach" }
func (c *projectNetworkAttachCmd) Synopsis() string {
	return "attach an app to its project network (admin role; member services roll onto the project network on the following redeploy)"
}
func (c *projectNetworkAttachCmd) Usage() string {
	return "projects network attach [--addr <host:port>] [--token <tok>] <app>"
}

func (c *projectNetworkAttachCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *projectNetworkAttachCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Projects().AttachAppProjectNetwork(ctx, &serverv1.AttachAppProjectNetworkRequest{
			App: c.conn.ref(args[0]),
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(env.Stdout, projectNetworkMembershipLine("attached to", resp.GetMembership()))
		return err
	})
}

// projectNetworkDetachCmd 实现 `fleetly projects network detach <app>`。
type projectNetworkDetachCmd struct {
	conn connFlags
}

func (c *projectNetworkDetachCmd) Name() string { return "detach" }
func (c *projectNetworkDetachCmd) Synopsis() string {
	return "detach an app from its project network (admin role; the member services leave the project network on the following redeploy)"
}
func (c *projectNetworkDetachCmd) Usage() string {
	return "projects network detach [--addr <host:port>] [--token <tok>] <app>"
}

func (c *projectNetworkDetachCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *projectNetworkDetachCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Projects().DetachAppProjectNetwork(ctx, &serverv1.DetachAppProjectNetworkRequest{
			App: c.conn.ref(args[0]),
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(env.Stdout, projectNetworkMembershipLine("detached from", resp.GetMembership()))
		return err
	})
}

// projectNetworkShowCmd 实现 `fleetly projects network show <app>`。
type projectNetworkShowCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *projectNetworkShowCmd) Name() string { return "show" }
func (c *projectNetworkShowCmd) Synopsis() string {
	return "show an app's project and project-network participation state (default off — apps are isolated in their own private network until attached)"
}
func (c *projectNetworkShowCmd) Usage() string {
	return "projects network show [--addr <host:port>] [--token <tok>] [--json] <app>"
}

func (c *projectNetworkShowCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *projectNetworkShowCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Apps().GetApp(ctx, &serverv1.GetAppRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, map[string]any{
				"app":                      resp.GetName(),
				"project":                  resp.GetTeamSlug() + "/" + resp.GetProjectSlug(),
				"project_id":               resp.GetProjectId(),
				"project_network":          resp.GetProjectNetwork(),
				"project_network_attached": resp.GetProjectNetworkAttached(),
			})
		}
		state := "detached (default: apps stay isolated in their own private network)"
		if resp.GetProjectNetworkAttached() {
			state = "attached (" + resp.GetProjectNetwork() + ")"
		}
		_, err = fmt.Fprintf(env.Stdout, "app %s  project %s/%s  project network: %s\n",
			resp.GetName(), resp.GetTeamSlug(), resp.GetProjectSlug(), state)
		return err
	})
}

// projectNetworkMembershipLine 渲染参与变更应答（单行、字段齐备——脚本
// 可解析；rolling = 重部署已入队，服务随发布滚动切换网络）。
func projectNetworkMembershipLine(verb string, m *serverv1.AppProjectNetworkMembership) string {
	deployment := m.GetDeploymentId()
	if deployment == "" {
		deployment = "-"
	}
	return fmt.Sprintf("app %s %s project network %s (project %s); changed=%v status=%s deployment=%s",
		m.GetApp(), verb, m.GetNetwork(), m.GetProject(), m.GetChanged(), m.GetStatus(), deployment)
}
