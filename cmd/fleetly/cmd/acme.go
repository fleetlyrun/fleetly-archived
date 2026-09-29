package cmd

// fleetly acme 命令（B 线 W5 设计 §3，D-V3W5-3/D-V3W5-4，W5-S3 新增动词，
// admin scope 专用）：
//
//	show         —— ACME DNS 设置与通配证书面只读展示（凭证只回指纹；含服务
//	                端派生的通配期望域名集——当前证书域集形态）；
//	dns set      —— 保存 DNS 服务商与凭证（token 经 --token 或 env
//	                FLEETLY_ACME_DNS_TOKEN 回退——env 形态不进 shell 历史）；
//	dns test     —— 探针：在 _acme-challenge-test.<base_domain> 建 TXT →
//	                删 TXT 两步真实往返（通过 = 能认证/能写/能删）；
//	wildcard     —— 通配证书 opt-in 开关（on = 平台证书签
//	                [*.base, console/ctrl/registry.<base>] 走 DNS-01）。
//
// 英文文案（仓库文案纪律）；api_token 明文只写不读（读面只见指纹——传输
// 面 TLS 承载机密性，持久层 envelope 加密；留空 = 保留已存凭证）。

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lynx-go/commands"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// acmeDNSProviderWord 是 dns set 的 provider 词表提示（与 proto 词表一致；
// 空 = none）。
const acmeDNSProviderWord = "none|dnspod|cloudflare"

// acmeEnvToken 是凭证的 env 回退变量名（实现票：token 经 env 回退不进
// shell 历史——--token 旗标缺位时读取）。
const acmeEnvToken = "FLEETLY_ACME_DNS_TOKEN" //nolint:gosec // G101：env 变量名常量，非凭据

// acmeCmd 是外层动词 `acme`：分发 show/dns/wildcard。
type acmeCmd struct {
	sub *commands.App
}

func newAcmeCmd() *acmeCmd {
	sub := commands.New()
	sub.Register(&acmeShowCmd{}, newAcmeDNSCmd(), &acmeWildcardCmd{})
	sub.VerbTitle = "acme subcommands:"
	return &acmeCmd{sub: sub}
}

func (c *acmeCmd) Name() string { return "acme" }
func (c *acmeCmd) Synopsis() string {
	return "ACME wildcard certificate and DNS-01 provider settings (admin scope)"
}
func (c *acmeCmd) Usage() string { return "acme <show|dns|wildcard> [flags] [args]" }

func (c *acmeCmd) SetFlags(_ *flag.FlagSet) {}

func (c *acmeCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (show|dns|wildcard)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// acmeShowCmd 实现 `fleetly acme show`：设置只读展示（脱敏）+ 通配期望域
// 名集（服务端派生实值——当前证书域集形态）。
type acmeShowCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *acmeShowCmd) Name() string { return "show" }
func (c *acmeShowCmd) Synopsis() string {
	return "show ACME DNS provider settings and the wildcard certificate domain set (token shown as fingerprint only)"
}
func (c *acmeShowCmd) Usage() string {
	return "acme show [--addr <host:port>] [--token <tok>] [--json]"
}

func (c *acmeShowCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *acmeShowCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.System().GetAcmeSettings(ctx, &serverv1.GetAcmeSettingsRequest{})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		st := resp.GetSettings()
		var b strings.Builder
		fmt.Fprintf(&b, "dns provider: %s\n", st.GetDnsProvider())
		if st.GetCredentialsFingerprint() != "" {
			fmt.Fprintf(&b, "credentials: set (fingerprint %s)\n", st.GetCredentialsFingerprint())
		} else {
			b.WriteString("credentials: not set\n")
		}
		fmt.Fprintf(&b, "wildcard: %t\n", st.GetWildcard())
		if st.GetBaseDomain() != "" {
			fmt.Fprintf(&b, "base domain: %s\n", st.GetBaseDomain())
		}
		if st.GetWildcard() && len(st.GetWildcardDomains()) > 0 {
			b.WriteString("certificate domains (wildcard set):\n")
			for _, d := range st.GetWildcardDomains() {
				fmt.Fprintf(&b, "  %s\n", d)
			}
			b.WriteString("note: the wildcard certificate covers every app domain under the base domain via SNI; " +
				"per-app HTTP-01 issuance stays for custom domains outside the base domain\n")
		} else if st.GetWildcard() {
			b.WriteString("certificate domains: wildcard on (base domain missing — the platform certificate face is inert)\n")
		} else {
			b.WriteString("certificate domains: per-domain HTTP-01 (default; the platform certificate covers console/ctrl/registry.<base>)\n")
		}
		if t := st.GetUpdatedAt(); t != nil {
			fmt.Fprintf(&b, "updated at: %s\n", tstampRFC3339(t))
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// acmeDNSCmd 是中层动词 `acme dns`：分发 set/test。
type acmeDNSCmd struct {
	sub *commands.App
}

func newAcmeDNSCmd() *acmeDNSCmd {
	sub := commands.New()
	sub.Register(&acmeDNSSetCmd{}, &acmeDNSTestCmd{})
	sub.VerbTitle = "dns subcommands:"
	return &acmeDNSCmd{sub: sub}
}

func (c *acmeDNSCmd) Name() string     { return "dns" }
func (c *acmeDNSCmd) Synopsis() string { return "DNS-01 provider credentials (set / probe)" }
func (c *acmeDNSCmd) Usage() string    { return "acme dns <set|test> [flags] [args]" }

func (c *acmeDNSCmd) SetFlags(_ *flag.FlagSet) {}

func (c *acmeDNSCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (set|test)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// acmeDNSSetCmd 实现 `fleetly acme dns set --provider <...> [--token <tok>]`：
// token 缺位回退 env FLEETLY_ACME_DNS_TOKEN（不进 shell 历史）；两处皆缺 =
// 保留已存凭证（provider=none 恒清空）。保存前置通读当前 wildcard 开关并随
// 请求回传（单一 Update 全量语义下开关不被 dns set 误翻）。
type acmeDNSSetCmd struct {
	provider string
	token    string
	jsonOut  bool
	conn     connFlags
}

func (c *acmeDNSSetCmd) Name() string { return "set" }
func (c *acmeDNSSetCmd) Synopsis() string {
	return "save the DNS-01 provider and its credentials (token: --api-token, or env FLEETLY_ACME_DNS_TOKEN; blank = keep stored)"
}
func (c *acmeDNSSetCmd) Usage() string {
	return "acme dns set [--addr <host:port>] [--token <tok>] --provider <none|dnspod|cloudflare> [--api-token <tok>] [--json]"
}

func (c *acmeDNSSetCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.provider, "provider", "", "DNS-01 provider: none | dnspod | cloudflare")
	// 注：--token 是 CLI 的连接鉴权旗标（connFlags，FLEETLY_TOKEN 同源），
	// 服务商凭证取 --api-token——词形冲突下不改全局连接旗标语义。
	fs.StringVar(&c.token, "api-token", "", "provider API token (dnspod: \"<id>,<token>\"; cloudflare: single token); omit to fall back to FLEETLY_ACME_DNS_TOKEN")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *acmeDNSSetCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	if c.provider == "" {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("--provider is required (%s)", acmeDNSProviderWord)}
	}
	token := c.token
	source := "flag"
	if token == "" && os.Getenv(acmeEnvToken) != "" {
		token = os.Getenv(acmeEnvToken)
		source = "env " + acmeEnvToken
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		// 读当前 wildcard 并随保存回传（Update 全量语义——dns set 不改变
		// 开关；wildcard=true 且 provider=none 的组合由服务端联动门拒绝）。
		cur, err := cl.System().GetAcmeSettings(ctx, &serverv1.GetAcmeSettingsRequest{})
		if err != nil {
			return err
		}
		resp, err := cl.System().UpdateAcmeSettings(ctx, &serverv1.UpdateAcmeSettingsRequest{
			DnsProvider: c.provider,
			ApiToken:    token,
			Wildcard:    cur.GetSettings().GetWildcard(),
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		st := resp.GetSettings()
		var b strings.Builder
		fmt.Fprintf(&b, "acme dns settings saved: provider=%s", st.GetDnsProvider())
		if st.GetCredentialsFingerprint() != "" {
			fmt.Fprintf(&b, " token fingerprint=%s", st.GetCredentialsFingerprint())
		} else {
			b.WriteString(" token=not set")
		}
		if token != "" {
			fmt.Fprintf(&b, " (token source: %s)", source)
		}
		if st.GetWildcard() {
			b.WriteString(" (wildcard stays on)")
		}
		b.WriteString("\nnext: run 'fleetly acme dns test' to verify the credentials with a real TXT create/delete probe\n")
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// acmeDNSTestCmd 实现 `fleetly acme dns test [--provider --token]`：候选
// 凭证（未保存也能测）或已存凭证（两旗标全缺）。探针 = 建删真实 TXT。
type acmeDNSTestCmd struct {
	provider string
	token    string
	jsonOut  bool
	conn     connFlags
}

func (c *acmeDNSTestCmd) Name() string { return "test" }
func (c *acmeDNSTestCmd) Synopsis() string {
	return "probe the DNS provider (creates and deletes a real TXT record at _acme-challenge-test.<base_domain>; pass = can authenticate, write and delete)"
}
func (c *acmeDNSTestCmd) Usage() string {
	return "acme dns test [--addr <host:port>] [--token <tok>] [--provider <dnspod|cloudflare>] [--api-token <tok>] [--json]"
}

func (c *acmeDNSTestCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.StringVar(&c.provider, "provider", "", "candidate provider (omit both flags to test the stored credentials)")
	fs.StringVar(&c.token, "api-token", "", "candidate API token (omit to fall back to FLEETLY_ACME_DNS_TOKEN)")
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *acmeDNSTestCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	token := c.token
	if token == "" && os.Getenv(acmeEnvToken) != "" {
		token = os.Getenv(acmeEnvToken)
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		resp, err := cl.System().TestDnsProvider(ctx, &serverv1.TestDnsProviderRequest{
			DnsProvider: c.provider,
			ApiToken:    token,
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		r := resp.GetResult()
		verdict := "ok"
		if !r.GetOk() {
			verdict = "FAILED"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "dns provider test %s (probe: create -> delete TXT %s; pass = can authenticate, can write, can delete)\n",
			verdict, r.GetRecordName())
		fmt.Fprintf(&b, "  provider: %s\n", r.GetDnsProvider())
		for _, st := range r.GetSteps() {
			if st.GetOk() {
				fmt.Fprintf(&b, "  %-6s ok   (%dms)\n", st.GetStep(), st.GetDurationMs())
			} else {
				fmt.Fprintf(&b, "  %-6s FAIL (%dms): %s\n", st.GetStep(), st.GetDurationMs(), st.GetError())
			}
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// acmeWildcardCmd 实现 `fleetly acme wildcard <on|off>`：通配证书 opt-in
// 开关。on 时服务端联动校验（无 provider → 422 带指引；无 base_domain →
// 409）。provider 与凭证随保存回传（单一 Update 全量语义下不被误清）。
type acmeWildcardCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *acmeWildcardCmd) Name() string { return "wildcard" }
func (c *acmeWildcardCmd) Synopsis() string {
	return "switch the wildcard certificate mode (on signs *.base via DNS-01; off returns to the three-SAN HTTP-01 form)"
}
func (c *acmeWildcardCmd) Usage() string {
	return "acme wildcard [--addr <host:port>] [--token <tok>] [--json] <on|off>"
}

func (c *acmeWildcardCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *acmeWildcardCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if err := requireArgs(c.Usage(), args, 1); err != nil {
		return err
	}
	var on bool
	switch args[0] {
	case "on":
		on = true
	case "off":
		on = false
	default:
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected on|off, got %q", args[0])}
	}
	return c.conn.withClient(func(cl *fleetlyClient) error {
		cur, err := cl.System().GetAcmeSettings(ctx, &serverv1.GetAcmeSettingsRequest{})
		if err != nil {
			return err
		}
		resp, err := cl.System().UpdateAcmeSettings(ctx, &serverv1.UpdateAcmeSettingsRequest{
			DnsProvider: cur.GetSettings().GetDnsProvider(),
			// token 留空 = 保留已存凭证（服务端语义；wildcard 切换不要求
			// 重录凭证）。
			Wildcard: on,
		})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return writeProtoJSON(env.Stdout, resp)
		}
		st := resp.GetSettings()
		var b strings.Builder
		fmt.Fprintf(&b, "wildcard %s (provider=%s)\n", map[bool]string{true: "on", false: "off"}[on], st.GetDnsProvider())
		if st.GetWildcard() && len(st.GetWildcardDomains()) > 0 {
			b.WriteString("the platform certificate will be reissued for:\n")
			for _, d := range st.GetWildcardDomains() {
				fmt.Fprintf(&b, "  %s\n", d)
			}
			b.WriteString("issuance switches to DNS-01 (converges within a scan cycle; app routing is unchanged — 443 is covered by the wildcard via SNI)\n")
		} else if !st.GetWildcard() {
			b.WriteString("the platform certificate will return to the three-SAN set (console/ctrl/registry.<base>) on the next convergence\n")
		}
		_, err = fmt.Fprint(env.Stdout, b.String())
		return err
	})
}

// 编译期断言：acme 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &acmeCmd{}
	_ commands.Flagged = &acmeCmd{}
	_ commands.Command = &acmeShowCmd{}
	_ commands.Flagged = &acmeShowCmd{}
	_ commands.Command = &acmeDNSCmd{}
	_ commands.Flagged = &acmeDNSCmd{}
	_ commands.Command = &acmeDNSSetCmd{}
	_ commands.Flagged = &acmeDNSSetCmd{}
	_ commands.Command = &acmeDNSTestCmd{}
	_ commands.Flagged = &acmeDNSTestCmd{}
	_ commands.Command = &acmeWildcardCmd{}
	_ commands.Flagged = &acmeWildcardCmd{}
)
