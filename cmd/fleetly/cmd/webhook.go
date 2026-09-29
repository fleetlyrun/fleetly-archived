package cmd

// fleetly apps webhook 命令（T2.19）：webhook 触发入口的 per-app 配置面
// （admin scope）。动词面：
//
//	apps webhook set-secret  <app> <secret>   设置 HMAC-SHA256 签名密钥
//	                                         （≥16 字符；envelope 加密落库，
//	                                          永不回读）；
//	apps webhook show         <app>            回读配置（secret 只回
//	                                          configured 位）；
//	apps webhook set-source   <app> …          设置拉源 remote（url +
//	                                          branch + 认证形态；认证材料
//	                                          envelope 加密，整体替换
//	                                          语义）。
//
// webhook 端点：POST /v1/apps/<app>/webhooks/github|gitea——secret 未配置
// 即端点未启用（404 语义）。

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// webhookCmd 是 `apps` 下的中层动词 `webhook`：分发 set-secret/show/
// set-source。
type webhookCmd struct {
	sub *commands.App
}

func newWebhookCmd() *webhookCmd {
	sub := commands.New()
	sub.Register(&webhookSecretSetCmd{}, &webhookShowCmd{}, &webhookSourceSetCmd{})
	sub.VerbTitle = "webhook subcommands:"
	return &webhookCmd{sub: sub}
}

func (c *webhookCmd) Name() string     { return "webhook" }
func (c *webhookCmd) Synopsis() string { return "per-app webhook trigger config (admin scope)" }
func (c *webhookCmd) Usage() string {
	return "apps webhook <set-secret|show|set-source> [flags] ..."
}

func (c *webhookCmd) SetFlags(_ *flag.FlagSet) {}

func (c *webhookCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (set-secret|show|set-source)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// webhookSecretSetCmd 实现 `fleetly apps webhook set-secret <app> <secret>`。
type webhookSecretSetCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *webhookSecretSetCmd) Name() string { return "set-secret" }
func (c *webhookSecretSetCmd) Synopsis() string {
	return "set the app's webhook signing secret (HMAC-SHA256; shown never again)"
}
func (c *webhookSecretSetCmd) Usage() string {
	return "apps webhook set-secret [--addr <host:port>] [--token <tok>] [--json] <app> <secret>"
}

func (c *webhookSecretSetCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *webhookSecretSetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 2); err != nil {
		return err
	}
	if len(args[1]) < 16 {
		return fmt.Errorf("secret must be at least 16 characters (signature verification is the webhook endpoint's only authentication)")
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Apps().SetAppWebhookSecret(ctx, &serverv1.SetAppWebhookSecretRequest{
			Name: c.conn.ref(args[0]), Secret: args[1],
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		_, err = fmt.Fprintf(env.Stdout, "webhook secret set for %s (configured=%v; the value is stored encrypted and never read back)\n",
			resp.GetName(), resp.GetConfigured())
		return err
	})
}

// webhookShowCmd 实现 `fleetly apps webhook show <app>`（无敏感投影）。
type webhookShowCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *webhookShowCmd) Name() string { return "show" }
func (c *webhookShowCmd) Synopsis() string {
	return "show the app's webhook/git trigger config (no sensitive projection)"
}
func (c *webhookShowCmd) Usage() string {
	return "apps webhook show [--addr <host:port>] [--token <tok>] [--json] <app>"
}

func (c *webhookShowCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *webhookShowCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Apps().ShowAppWebhook(ctx, &serverv1.ShowAppWebhookRequest{Name: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "app: %s\n", resp.GetName())
		fmt.Fprintf(&b, "  webhook secret: %s\n", configuredWord(resp.GetSecretConfigured()))
		fmt.Fprintf(&b, "  branch: %s (webhook trigger/fetch)\n", resp.GetSourceBranch())
		fmt.Fprintf(&b, "  source url: %s\n", orDash(resp.GetSourceUrl()))
		fmt.Fprintf(&b, "  source auth: %s\n", resp.GetSourceAuthKind())
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// webhookSourceSetCmd 实现 `fleetly apps webhook set-source`（url/branch/
// auth-kind/auth-secret；整体替换语义——认证材料加密落库无法「留旧」）。
type webhookSourceSetCmd struct {
	branch     string
	authKind   string
	authSecret string
	jsonOut    bool
	conn       connFlags
}

func (c *webhookSourceSetCmd) Name() string { return "set-source" }
func (c *webhookSourceSetCmd) Synopsis() string {
	return "set the app's webhook fetch source (url + branch + auth)"
}
func (c *webhookSourceSetCmd) Usage() string {
	return "apps webhook set-source [--addr <host:port>] [--token <tok>] [--branch <b>] [--auth-kind none|https_token|ssh_key] [--auth-secret <material>] [--json] <app> <url>"
}

func (c *webhookSourceSetCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.branch, "branch", "main", "push/fetch branch (the app's configured branch, default main)")
	fs.StringVar(&c.authKind, "auth-kind", "none", "auth kind: none | https_token | ssh_key")
	fs.StringVar(&c.authSecret, "auth-secret", "", "auth material (token or PEM private key; encrypted at rest, never echoed)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *webhookSourceSetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 2); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Apps().SetAppSource(ctx, &serverv1.SetAppSourceRequest{
			Name: c.conn.ref(args[0]), SourceUrl: args[1], SourceBranch: c.branch,
			SourceAuthKind: c.authKind, SourceAuthSecret: c.authSecret,
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		_, err = fmt.Fprintf(env.Stdout, "source set for %s\n  url: %s\n  branch: %s\n  auth: %s\n",
			resp.GetName(), resp.GetSourceUrl(), resp.GetSourceBranch(), resp.GetSourceAuthKind())
		return err
	})
}

// configuredWord / orDash 是人读投影的小词形。
func configuredWord(v bool) string {
	if v {
		return "configured"
	}
	return "not configured (endpoint disabled)"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// 编译期断言：webhook 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &webhookCmd{}
	_ commands.Flagged = &webhookCmd{}
	_ commands.Command = &webhookSecretSetCmd{}
	_ commands.Flagged = &webhookSecretSetCmd{}
	_ commands.Command = &webhookShowCmd{}
	_ commands.Flagged = &webhookShowCmd{}
	_ commands.Command = &webhookSourceSetCmd{}
	_ commands.Flagged = &webhookSourceSetCmd{}
)
