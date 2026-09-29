package cmd

// fleetly auth 命令（v0.3 W1-S4，rbac-teams 设计 §2.4「CLI 登录与上下文」）：
// login / status / logout。凭据形态 = 粘贴式 PAT（Bearer——device flow 挂账
// 设计 §14）：login 指引到 Console 用户菜单创建 PAT 后粘贴（非 TTY 或
// --token 传参直接收取），验证走 Auth()->Me（401 即拒——无效/吊销 token
// 不落盘），通过后写入 ~/.fleetly/config.yaml（token + 上下文文件，见
// config.go——本仓此前的 CLI 无任何 token 落盘形态，本票引入）。logout 只
// 清本地（服务端吊销是 tokens 面——PAT 自服务页随 W2 TokensService 迁移，
// 文案如实指引）。team use / project use 切换子命令不做（W2 随团队/项目
// 服务一起）——当前 team/project 上下文经 --team/--project 或 config 承载。

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lynx-go/commands"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// stdin 是标准输入注入点（extraDialOptions 同款测试纪律：生产 = os.Stdin，
// 测试注入 bytes.Buffer）。非 *os.File 的读取器一律按非 TTY 管道语义处理
// ——测试天然走「直接收取」分支。
var stdin io.Reader = os.Stdin

// stdinIsTerminal 报告 stdin 是否接终端：仅 *os.File 做真实判定
// （ModeCharDevice）；注入的读取器恒 false（管道语义）。
func stdinIsTerminal() bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ── fleetly auth ────────────────────────────────────────────────────────────

// authCmd 是外层动词 `auth`：分发 login/status/logout。
type authCmd struct {
	sub *commands.App
}

func newAuthCmd() *authCmd {
	sub := commands.New()
	sub.Register(&authLoginCmd{}, &authStatusCmd{}, &authLogoutCmd{})
	sub.VerbTitle = "auth subcommands:"
	return &authCmd{sub: sub}
}

func (c *authCmd) Name() string     { return "auth" }
func (c *authCmd) Synopsis() string { return "CLI login, identity and local credential management" }
func (c *authCmd) Usage() string    { return "auth <login|status|logout> [flags] ..." }

func (c *authCmd) SetFlags(_ *flag.FlagSet) {}

func (c *authCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("missing subcommand (login|status|logout)")}
	}
	return subDispatchUsage(c, c.sub, ctx, env, args)
}

// ── 投影（--json 的 CLI 合成形态；snake_case 与 REST 面同源）────────────────

// authUserJSON 是 UserView 的 CLI 投影（时间 RFC3339；无敏感字段——口令
// 哈希不在任何通道）。
type authUserJSON struct {
	ID              string `json:"id"`
	Email           string `json:"email"`
	DisplayName     string `json:"display_name"`
	IsPlatformAdmin bool   `json:"is_platform_admin"`
	CreatedAt       string `json:"created_at,omitempty"`
	DisabledAt      string `json:"disabled_at,omitempty"`
}

// authTeamJSON 是 TeamMembership 的 CLI 投影。
type authTeamJSON struct {
	TeamID   string `json:"team_id"`
	TeamSlug string `json:"team_slug"`
	TeamName string `json:"team_name"`
	Role     string `json:"role"`
}

func userToJSON(u *serverv1.UserView) authUserJSON {
	return authUserJSON{
		ID:              u.GetId(),
		Email:           u.GetEmail(),
		DisplayName:     u.GetDisplayName(),
		IsPlatformAdmin: u.GetIsPlatformAdmin(),
		CreatedAt:       tstampRFC3339(u.GetCreatedAt()),
		DisabledAt:      tstampRFC3339(u.GetDisabledAt()),
	}
}

func teamsToJSON(ts []*serverv1.TeamMembership) []authTeamJSON {
	out := make([]authTeamJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, authTeamJSON{
			TeamID: t.GetTeamId(), TeamSlug: t.GetTeamSlug(),
			TeamName: t.GetTeamName(), Role: t.GetRole(),
		})
	}
	return out
}

// sourceLabel 把解析来源渲染成人读标签（token 面带 env 名——env 有专名；
// context 面共用 FLEETLY_TEAM/FLEETLY_PROJECT 两名，由调用方传入）。
func sourceLabel(source, envName string) string {
	switch source {
	case sourceFlag:
		return "flag"
	case sourceEnv:
		return "env " + envName
	case sourceConfig:
		return "config"
	default:
		return "(not set)"
	}
}

// configPathLabel 返回配置路径的人读形态（解析失败降级为 "(unavailable)"——
// 仅展示面，不参与控制流）。
func configPathLabel() string {
	p, err := fleetlyConfigPath()
	if err != nil {
		return "(unavailable)"
	}
	return p
}

// ── fleetly auth login ──────────────────────────────────────────────────────

// authLoginCmd 实现 `fleetly auth login [--token <pat>]`：收取 PAT（flag >
// TTY 粘贴 > 管道直收）→ Auth().Me 验证（401 即拒、机具令牌拒收——Me 只
// 认用户凭据）→ 落盘 config.yaml（保留既有 team/project 上下文）。
type authLoginCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *authLoginCmd) Name() string { return "login" }
func (c *authLoginCmd) Synopsis() string {
	return "verify a PAT (via Me) and store it in the local config"
}
func (c *authLoginCmd) Usage() string {
	return "auth login [--addr <host:port>] [--token <pat>] [--json]"
}

func (c *authLoginCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

// readPastedToken 收取待验证的 PAT：--token 传参直接用；TTY 交互式（指引
// 到 Console 后读一行——PAT 明文回显不掩码，Console 创建面本就明文展示
// 一次，掩码需要 term 依赖，挂账报告）；非 TTY（管道）直收全部 stdin 并
// 修剪空白（echo/pbpaste 形态）。空值返回用法错误。提示文案写 out（框架
// 注入的 stdout——测试面可拦截）。
func readPastedToken(flagToken string, out io.Writer) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if stdinIsTerminal() {
		_, _ = fmt.Fprintln(out, "Create a PAT in the Console (user menu > API tokens), then paste it here (input is echoed):")
		_, _ = fmt.Fprint(out, "token: ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read token from terminal: %w", err)
		}
		tok := strings.TrimSpace(line)
		if tok == "" {
			return "", &commands.UsageError{Usage: (&authLoginCmd{}).Usage(), Err: fmt.Errorf("no token entered")}
		}
		return tok, nil
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read token from stdin: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", &commands.UsageError{Usage: (&authLoginCmd{}).Usage(), Err: fmt.Errorf("no token provided (paste one interactively, pass --token, or pipe it via stdin)")}
	}
	return tok, nil
}

func (c *authLoginCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	token, err := readPastedToken(c.conn.token, env.Stdout)
	if err != nil {
		return err
	}
	// 候选 token 置入 flag 位后走生产同路径 dial（解析矩阵 flag 层恒赢，
	// env/config 不参与本次验证）。Me 401 即拒：无效/吊销 token 不落盘。
	c.conn.token = token
	var me *serverv1.MeResponse
	if err := c.conn.withClient(func(cl *fleetlyClient) (err error) {
		me, err = cl.Auth().Me(ctx, &serverv1.MeRequest{})
		if err != nil {
			return rejectLoginError(err)
		}
		return nil
	}); err != nil {
		return err
	}
	// 验证通过落盘：读既有配置保留 team/project 上下文，仅写 token 位。
	cfg, err := loadCLIConfig()
	if err != nil {
		return err
	}
	cfg.Token = token
	if err := saveCLIConfig(cfg); err != nil {
		return err
	}
	if c.jsonOut {
		return writeJSON(env.Stdout, map[string]any{
			"user":    userToJSON(me.GetUser()),
			"teams":   teamsToJSON(me.GetTeams()),
			"config":  configPathLabel(),
			"source":  sourceLabel(sourceConfig, ""),
			"stored":  true,
			"address": c.conn.addr,
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "logged in as %s (display name: %s)\n", me.GetUser().GetEmail(), displayOrDash(me.GetUser().GetDisplayName()))
	fmt.Fprintf(&b, "  credential stored in %s (priority: --token > FLEETLY_TOKEN > this config)\n", configPathLabel())
	writeTeamsSection(&b, me.GetTeams())
	_, err = fmt.Fprint(env.Stdout, b.String())
	return err
}

// rejectLoginError 把 Me 的失败映射成 login 语义的可行动错误（plain error
// ——renderCLIError 不再叠加 Unauthenticated 通用提示，login 的失败文案
// 自足）。Unavailable 等连接面错误原样上抛（保留通用可行动提示）。
func rejectLoginError(err error) error {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return errors.New("login rejected: the server did not accept this token (401) — it is missing, invalid or revoked; create a fresh PAT in the Console and paste it again")
	case codes.InvalidArgument:
		// Me 只认用户凭据（session / user PAT）——能通过拦截器鉴权却拿不
		// 到 Me 投影 = 平台机具令牌（user NULL）。机具令牌继续走
		// --token/FLEETLY_TOKEN（CI 形态），不是 CLI login 的对象。
		return errors.New("login rejected: this is a platform machine token, not a user PAT — 'fleetly auth login' stores a personal credential; machine tokens keep working via --token / FLEETLY_TOKEN")
	default:
		return err
	}
}

// ── fleetly auth status ─────────────────────────────────────────────────────

// authStatusCmd 实现 `fleetly auth status`：三态——无凭据（指引 login）、
// 有效（Me 投影：email/显示名/平台管理员/团队×角色 + 当前上下文）、失效
// （服务端拒绝：指引重新 login）。
type authStatusCmd struct {
	jsonOut bool
	conn    connFlags
}

func (c *authStatusCmd) Name() string { return "status" }
func (c *authStatusCmd) Synopsis() string {
	return "show the current identity (Me) and team/project context"
}
func (c *authStatusCmd) Usage() string {
	return "auth status [--addr <host:port>] [--token <tok>] [--team <slug>] [--project <slug>] [--json]"
}

func (c *authStatusCmd) SetFlags(fs *flag.FlagSet) {
	c.conn.register(fs)
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *authStatusCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	token, source, err := resolveToken(c.conn.token)
	if err != nil {
		return err
	}
	if token == "" {
		// 三态之一「无凭据」：不起请求（本地即可裁决），指引可行动出口。
		return errors.New("not logged in — run 'fleetly auth login' and paste a PAT (create one in the Console's user menu > API tokens); or set --token / FLEETLY_TOKEN for one-off use")
	}
	rc, err := resolveContext(c.conn.team, c.conn.project)
	if err != nil {
		return err
	}
	var me *serverv1.MeResponse
	if err := c.conn.withClient(func(cl *fleetlyClient) (err error) {
		me, err = cl.Auth().Me(ctx, &serverv1.MeRequest{})
		if err != nil {
			return rejectStatusError(err)
		}
		return nil
	}); err != nil {
		return err
	}
	if c.jsonOut {
		return writeJSON(env.Stdout, map[string]any{
			"authenticated":     true,
			"credential_source": source,
			"user":              userToJSON(me.GetUser()),
			"teams":             teamsToJSON(me.GetTeams()),
			"context":           rc,
			"address":           c.conn.addr,
			"config":            configPathLabel(),
		})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "logged in as %s (display name: %s)\n", me.GetUser().GetEmail(), displayOrDash(me.GetUser().GetDisplayName()))
	fmt.Fprintf(&b, "  platform admin: %s\n", yesNo(me.GetUser().GetIsPlatformAdmin()))
	fmt.Fprintf(&b, "  credential source: %s\n", sourceLabel(source, "FLEETLY_TOKEN"))
	writeTeamsSection(&b, me.GetTeams())
	teamLabel := "(not set)"
	if rc.Team != "" {
		teamLabel = fmt.Sprintf("%s (%s)", rc.Team, sourceLabel(rc.TeamSource, "FLEETLY_TEAM"))
	}
	projectLabel := "(not set)"
	if rc.Project != "" {
		projectLabel = fmt.Sprintf("%s (%s)", rc.Project, sourceLabel(rc.ProjectSource, "FLEETLY_PROJECT"))
	}
	fmt.Fprintf(&b, "  context: team=%s project=%s\n", teamLabel, projectLabel)
	_, err = fmt.Fprint(env.Stdout, b.String())
	return err
}

// rejectStatusError 把 status 的服务失败映射成三态中「失效」的可行动文案
// （plain error，理由同 rejectLoginError）；连接面错误原样上抛。
func rejectStatusError(err error) error {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return errors.New("credential rejected by the server (401) — the PAT is invalid or revoked; run 'fleetly auth logout' to clear it, then 'fleetly auth login' with a fresh PAT")
	case codes.InvalidArgument:
		// 机具令牌对本机可用但不是登录态：如实告知身份面不可投影的原因。
		return errors.New("the configured credential is a platform machine token (not a user PAT) — 'auth status' projects a user identity; machine tokens keep working via --token / FLEETLY_TOKEN")
	default:
		return err
	}
}

// ── 展示 helpers（login/status 共用）────────────────────────────────────────

func displayOrDash(name string) string {
	if name == "" {
		return "-"
	}
	return name
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// writeTeamsSection 渲染「所属团队×角色」段（login 与 status 同形；空集
// 如实出 "(none)"——注册用户的个人队恒在，空集 = 服务端投影异常态）。
func writeTeamsSection(b *strings.Builder, teams []*serverv1.TeamMembership) {
	if len(teams) == 0 {
		b.WriteString("  teams: (none)\n")
		return
	}
	b.WriteString("  teams:\n")
	for _, t := range teams {
		fmt.Fprintf(b, "    - %s — %s (role: %s)\n", t.GetTeamSlug(), t.GetTeamName(), t.GetRole())
	}
}

// ── fleetly auth logout ─────────────────────────────────────────────────────

// authLogoutCmd 实现 `fleetly auth logout`：清本地凭据与上下文（幂等——
// 无凭据也是成功收尾）。纯本地操作：不嵌 conn flags（无网络语义）、不触
// 服务端——PAT 服务端吊销是 tokens 面（admin 经 `fleetly tokens revoke`，
// 用户自服务页随 W2 TokensService 迁移），文案如实指引。配置文件损坏时
// 依旧可清（clear 直接删文件，不经 load——排障出口）。
type authLogoutCmd struct {
	jsonOut bool
}

func (c *authLogoutCmd) Name() string { return "logout" }
func (c *authLogoutCmd) Synopsis() string {
	return "clear the locally stored credential and team/project context"
}
func (c *authLogoutCmd) Usage() string {
	return "auth logout [--json]"
}

func (c *authLogoutCmd) SetFlags(fs *flag.FlagSet) {
	fs.BoolVar(&c.jsonOut, "json", false, "output machine-readable JSON")
}

func (c *authLogoutCmd) Run(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) != 0 {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("expected 0 arguments, got %d", len(args))}
	}
	before, err := loadCLIConfig()
	if err != nil {
		return err
	}
	if err := clearCLIConfig(); err != nil {
		return err
	}
	path := configPathLabel()
	if c.jsonOut {
		return writeJSON(env.Stdout, map[string]any{
			"logged_out": true,
			"cleared":    !before.empty(),
			"config":     path,
			"note":       "the PAT is not revoked server-side — revoke it with 'fleetly tokens revoke <id>' (admin) or the Console self-service page (v0.3 W2)",
		})
	}
	var b strings.Builder
	if before.empty() {
		fmt.Fprintf(&b, "already logged out (no local credential in %s)\n", path)
	} else {
		fmt.Fprintf(&b, "logged out: local credential and team/project context cleared (%s)\n", path)
	}
	b.WriteString("  note: the PAT itself is not revoked server-side — revoke it with 'fleetly tokens revoke <id>' (admin) or the Console self-service page (lands in v0.3 W2)\n")
	_, err = fmt.Fprint(env.Stdout, b.String())
	return err
}

// 编译期断言：auth 命令实现 commands.Command/Flagged 契约。
var (
	_ commands.Command = &authCmd{}
	_ commands.Flagged = &authCmd{}
	_ commands.Command = &authLoginCmd{}
	_ commands.Flagged = &authLoginCmd{}
	_ commands.Command = &authStatusCmd{}
	_ commands.Flagged = &authStatusCmd{}
	_ commands.Command = &authLogoutCmd{}
	_ commands.Flagged = &authLogoutCmd{}
)
