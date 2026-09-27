package cmd

// 明文配置资源命令（T 线 OT-3 / IMPL-T1-4）：`fleetly configs
// <set|get|ls|rm>`。全部经 RPC（CLI-over-SDK 纪律）；值明文可回读（与
// secrets 的只写面刻意不同——配置不是凭据材料，GetConfig 走 admin scope
// 的显式回读；ls 只出名称/内容指纹/时间锚）。
//
// set 的值来源二选一：`--value <v>` 或 `--from-file <path>`（配置文件内容
// 常为多行 YAML/JSON，从文件读取免 shell 转义与历史污染；两者互斥且必须
// 恰给其一）。

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// configsCmd 是外层动词 `configs`：分发配置资源子命令。
type configsCmd struct {
	sub *commands.App
}

func newConfigsCmd() *configsCmd {
	sub := commands.New()
	sub.Register(&configSetCmd{}, &configGetCmd{}, &configListCmd{}, &configRemoveCmd{})
	sub.VerbTitle = "configs subcommands:"
	return &configsCmd{sub: sub}
}

func (c *configsCmd) Name() string { return "configs" }
func (c *configsCmd) Synopsis() string {
	return "manage app configs (plaintext, versioned; mounted read-only via compose configs — directory trees should be baked into the image)"
}
func (c *configsCmd) Usage() string {
	return "configs <set|get|ls|rm> [flags] ..."
}

func (c *configsCmd) SetFlags(_ *flag.FlagSet) {}

func (c *configsCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (set|get|ls|rm)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// configSetCmd 实现 `fleetly configs set <app> <name> (--value <v> | --from-file <path>)`。
type configSetCmd struct {
	value    string
	fromFile string
	conn     connFlags
}

func (c *configSetCmd) Name() string { return "set" }
func (c *configSetCmd) Synopsis() string {
	return "set (or rotate) an app config: the value is stored in plaintext and mounted read-only on the next deploy (content change = new swarm object + service roll)"
}
func (c *configSetCmd) Usage() string {
	return "configs set (--value <value> | --from-file <path>) [--addr <host:port>] [--token <tok>] <app> <name>"
}

func (c *configSetCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.value, "value", "", "config content (inline; mutually exclusive with --from-file)")
	fs.StringVar(&c.fromFile, "from-file", "", "read config content from a local file (mutually exclusive with --value)")
}

func (c *configSetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 2); err != nil {
		return err
	}
	if (c.value == "") == (c.fromFile == "") {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("exactly one of --value or --from-file is required")}
	}
	value := c.value
	if c.fromFile != "" {
		raw, err := os.ReadFile(c.fromFile) //nolint:gosec // 用户显式指定的本地文件（CLI 语义）
		if err != nil {
			return fmt.Errorf("read config file %s: %w", c.fromFile, err)
		}
		value = string(raw)
	}
	if value == "" {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("config content is empty")}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Configs().SetConfig(ctx, &serverv1.SetConfigRequest{
			App:   c.conn.ref(args[0]),
			Name:  args[1],
			Value: value,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "config %s/%s set (%d bytes, fingerprint %s); referencing services pick up the new content on their next deploy (content change rolls them)\n",
			resp.GetApp(), resp.GetName(), len(value), resp.GetHash8())
		return err
	})
}

// configGetCmd 实现 `fleetly configs get <app> <name>`（明文回读；admin scope）。
type configGetCmd struct {
	conn connFlags
}

func (c *configGetCmd) Name() string { return "get" }
func (c *configGetCmd) Synopsis() string {
	return "print an app config's plaintext content (admin scope; pipe-friendly — trailing newline is not added)"
}
func (c *configGetCmd) Usage() string {
	return "configs get [--addr <host:port>] [--token <tok>] <app> <name>"
}

func (c *configGetCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *configGetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 2); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Configs().GetConfig(ctx, &serverv1.GetConfigRequest{
			App:  c.conn.ref(args[0]),
			Name: args[1],
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(env.Stdout, resp.GetValue())
		return err
	})
}

// configListCmd 实现 `fleetly configs ls <app>`。
type configListCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *configListCmd) Name() string { return "ls" }
func (c *configListCmd) Synopsis() string {
	return "list an app's configs (names and content fingerprints only — read a value with: fleetly configs get)"
}
func (c *configListCmd) Usage() string {
	return "configs ls [--addr <host:port>] [--token <tok>] [--json] <app>"
}

func (c *configListCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *configListCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Configs().ListConfigs(ctx, &serverv1.ListConfigsRequest{App: c.conn.ref(args[0])})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeJSON(env.Stdout, resp)
		}
		if len(resp.GetConfigs()) == 0 {
			_, err := fmt.Fprintln(env.Stdout, "no configs (set one with: fleetly configs set <app> <name> --from-file <path>)")
			return err
		}
		for _, cfg := range resp.GetConfigs() {
			line := fmt.Sprintf("%s  %s", cfg.GetName(), cfg.GetHash8())
			if ts := cfg.GetUpdatedAt(); ts != nil {
				line += "  " + tstampRFC3339(ts)
			}
			if _, err := fmt.Fprintln(env.Stdout, line); err != nil {
				return err
			}
		}
		return nil
	})
}

// configRemoveCmd 实现 `fleetly configs rm <app> <name>`。
type configRemoveCmd struct {
	conn connFlags
}

func (c *configRemoveCmd) Name() string { return "rm" }
func (c *configRemoveCmd) Synopsis() string {
	return "remove an app config (services still declaring it fail their next deploy with E_CONFIG_NOT_FOUND — remove the declaration too)"
}
func (c *configRemoveCmd) Usage() string {
	return "configs rm [--addr <host:port>] [--token <tok>] <app> <name>"
}

func (c *configRemoveCmd) SetFlags(fs *flag.FlagSet) { c.conn.register(fs) }

func (c *configRemoveCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 2); err != nil {
		return err
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.Configs().RemoveConfig(ctx, &serverv1.RemoveConfigRequest{
			App:  c.conn.ref(args[0]),
			Name: args[1],
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(env.Stdout, "config %s/%s removed\n", resp.GetApp(), resp.GetName())
		return err
	})
}

// 编译期断言：configs 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &configsCmd{}
	_ commands.Flagged = &configsCmd{}
)
