package cmd

// auth 命令的验收测试（v0.3 W1-S4）：status 三态（无凭据 / 有效 / 失效，
// 另含机具令牌态）、login 的收取三通道（flag / 管道直收 / 拒绝路径）与
// 落盘语义（验证通过才落、保留既有上下文、401 不落盘）、logout 的幂等清
// 理。服务面夹具 = startCLI 的进程内真实服务（不 mock）：用户态经
// SeedUser/SeedUserToken 取真实用户 PAT；解析矩阵本体在 config_test.go。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestAuthStatusNoCredential 三态之一「无凭据」：flag/env/config 全空 →
// 本地即可裁决（不起请求），exit 1 + 可行动指引（auth login / FLEETLY_TOKEN）。
func TestAuthStatusNoCredential(t *testing.T) {
	useTempConfig(t)
	t.Setenv("FLEETLY_TOKEN", "")
	code, _, errOut := runCLI(t, "auth", "status")
	if code != 1 {
		t.Fatalf("no credential: code=%d, want 1\nstderr=%s", code, errOut)
	}
	for _, want := range []string{"not logged in", "fleetly auth login", "FLEETLY_TOKEN"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// TestAuthStatusValidCredential 三态之二「有效」：config 落盘的用户 PAT 经
// dial 解析矩阵生效（env 置空 = 未设置），Me 投影 email/显示名/平台管理员/
// 团队×角色 + 上下文（team/project 及来源）。人读与 --json 双形态。
func TestAuthStatusValidCredential(t *testing.T) {
	env := startCLI(t)
	useTempConfig(t)
	rr := env.SeedUser(t, "ada@example.com", "password-123")
	pat := env.SeedUserToken(t, rr.User.ID)
	if err := saveCLIConfig(cliConfig{
		Token:          pat,
		CurrentTeam:    rr.Team.Slug,
		CurrentProject: "default",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	// env 置空：逼迫 config 环生效（来源展示 = config；含 startCLI 注入的
	// 夹具 FLEETLY_PROJECT——上下文断言要 config 来源）。
	t.Setenv("FLEETLY_TOKEN", "")
	t.Setenv("FLEETLY_TEAM", "")
	t.Setenv("FLEETLY_PROJECT", "")

	code, out, errOut := runCLI(t, "auth", "status")
	if code != 0 {
		t.Fatalf("valid: code=%d\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	for _, want := range []string{
		"logged in as ada@example.com",
		"display name: ada",
		"platform admin: yes",
		"credential source: config",
		"- " + rr.Team.Slug,
		"role: owner",
		"context: team=" + rr.Team.Slug + " (config)",
		"project=default (config)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}

	code, out, _ = runCLI(t, "auth", "status", "--json")
	if code != 0 {
		t.Fatalf("valid --json: code=%d", code)
	}
	var st struct {
		Authenticated    bool   `json:"authenticated"`
		CredentialSource string `json:"credential_source"`
		User             struct {
			Email           string `json:"email"`
			DisplayName     string `json:"display_name"`
			IsPlatformAdmin bool   `json:"is_platform_admin"`
		} `json:"user"`
		Teams []struct {
			TeamSlug string `json:"team_slug"`
			Role     string `json:"role"`
		} `json:"teams"`
		Context struct {
			Team          string `json:"team"`
			Project       string `json:"project"`
			TeamSource    string `json:"team_source"`
			ProjectSource string `json:"project_source"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, out)
	}
	if !st.Authenticated || st.CredentialSource != sourceConfig ||
		st.User.Email != "ada@example.com" || st.User.DisplayName != "ada" ||
		!st.User.IsPlatformAdmin || len(st.Teams) != 1 ||
		st.Teams[0].TeamSlug != rr.Team.Slug || st.Teams[0].Role != "owner" ||
		st.Context.Team != rr.Team.Slug || st.Context.Project != "default" ||
		st.Context.TeamSource != sourceConfig || st.Context.ProjectSource != sourceConfig {
		t.Fatalf("status --json = %+v", st)
	}
}

// TestAuthStatusFlagOverridesContext --team/--project flag（公共 flag 面）
// 在 status 的消费展示：值与来源（flag）随解析矩阵覆盖 config。
func TestAuthStatusFlagOverridesContext(t *testing.T) {
	env := startCLI(t)
	useTempConfig(t)
	rr := env.SeedUser(t, "ada@example.com", "password-123")
	pat := env.SeedUserToken(t, rr.User.ID)
	if err := saveCLIConfig(cliConfig{
		Token:          pat,
		CurrentTeam:    rr.Team.Slug,
		CurrentProject: "default",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("FLEETLY_TOKEN", "")
	code, out, errOut := runCLI(t, "auth", "status", "--team", "flagteam", "--project", "flagprj")
	if code != 0 {
		t.Fatalf("code=%d\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "team=flagteam (flag)") || !strings.Contains(out, "project=flagprj (flag)") {
		t.Fatalf("flag context override missing:\n%s", out)
	}
}

// TestAuthStatusInvalidCredential 三态之三「失效」：token 在但服务端 401
// → exit 1 + 重新 login 指引（不渲染原始信封；可行动文案自足）。
func TestAuthStatusInvalidCredential(t *testing.T) {
	startCLI(t)
	useTempConfig(t)
	t.Setenv("FLEETLY_TOKEN", "flt_"+strings.Repeat("0", 48))
	code, _, errOut := runCLI(t, "auth", "status")
	if code != 1 {
		t.Fatalf("invalid: code=%d, want 1\nstderr=%s", code, errOut)
	}
	for _, want := range []string{"credential rejected", "401", "fleetly auth logout", "fleetly auth login"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// TestAuthStatusMachineToken 机具令牌态（第四可达态）：平台机具令牌能过
// 拦截器但拿不到 Me 投影（InvalidArgument）——status 如实告知这不是登录态，
// 机具用法不受影响。
func TestAuthStatusMachineToken(t *testing.T) {
	env := startCLI(t)
	useTempConfig(t)
	code, _, errOut := runCLI(t, "auth", "status") // startCLI 的 env token = 机具 admin token
	if code != 1 {
		t.Fatalf("machine token: code=%d, want 1\nstderr=%s", code, errOut)
	}
	for _, want := range []string{"machine token", "not a user PAT"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	_ = env
}

// TestAuthLoginStoresVerifiedToken login 全链（flag 通道）：Me 验证通过才
// 落盘；落盘保留既有 team/project 上下文；随后 status 以落盘凭据可用
// （端到端闭环）；env 高于 config 的 CLI 面证据（机具 admin 经 env 覆盖
// 落盘 PAT → status 投影机具态）。
func TestAuthLoginStoresVerifiedToken(t *testing.T) {
	env := startCLI(t)
	path := useTempConfig(t)
	rr := env.SeedUser(t, "ada@example.com", "password-123")
	pat := env.SeedUserToken(t, rr.User.ID)
	// 既有上下文必须在 login 后保留。
	if err := saveCLIConfig(cliConfig{CurrentTeam: "keepme", CurrentProject: "keepprj"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("FLEETLY_TOKEN", "")

	code, out, errOut := runCLI(t, "auth", "login", "--token", pat)
	if code != 0 {
		t.Fatalf("login: code=%d\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "logged in as ada@example.com") || !strings.Contains(out, path) {
		t.Fatalf("login output missing identity/config path:\n%s", out)
	}
	cfg, err := loadCLIConfig()
	if err != nil || cfg.Token != pat || cfg.CurrentTeam != "keepme" || cfg.CurrentProject != "keepprj" {
		t.Fatalf("stored config = (%+v, %v), want token kept with context intact", cfg, err)
	}

	// 闭环：status 以落盘凭据走通（config 环生效）。
	code, out, _ = runCLI(t, "auth", "status")
	if code != 0 || !strings.Contains(out, "ada@example.com") {
		t.Fatalf("status after login: code=%d out=%s", code, out)
	}

	// env > config：env 放机具 admin token 后，解析矩阵让 env 赢（投影
	// 机具态而非用户身份）——存量 env 用户零行为变化的 CLI 面证据。
	t.Setenv("FLEETLY_TOKEN", env.AdminToken)
	code, _, errOut = runCLI(t, "auth", "status")
	if code != 1 || !strings.Contains(errOut, "machine token") {
		t.Fatalf("env must beat stored config: code=%d stderr=%s", code, errOut)
	}
}

// TestAuthLoginRejectsBadToken login 拒绝路径：无效 token（401）不落盘、
// 机具令牌拒收、多余位置参数 = 用法错误（64）。
func TestAuthLoginRejectsBadToken(t *testing.T) {
	env := startCLI(t)
	path := useTempConfig(t)
	t.Setenv("FLEETLY_TOKEN", "")

	code, _, errOut := runCLI(t, "auth", "login", "--token", "flt_"+strings.Repeat("0", 48))
	if code != 1 || !strings.Contains(errOut, "login rejected") || !strings.Contains(errOut, "401") {
		t.Fatalf("invalid token: code=%d stderr=%s", code, errOut)
	}
	if cfg, err := loadCLIConfig(); err != nil || cfg.Token != "" {
		t.Fatalf("rejected token must not be stored: cfg=%+v err=%v", cfg, err)
	}

	// 机具令牌（能过鉴权、非用户凭据）同样拒收。
	code, _, errOut = runCLI(t, "auth", "login", "--token", env.AdminToken)
	if code != 1 || !strings.Contains(errOut, "machine token") {
		t.Fatalf("machine token: code=%d stderr=%s", code, errOut)
	}
	if cfg, err := loadCLIConfig(); err != nil || cfg.Token != "" {
		t.Fatalf("machine token must not be stored: cfg=%+v err=%v", cfg, err)
	}
	_ = path

	// 位置参数违规 → 64。
	if code, _, _ := runCLI(t, "auth", "login", "extra"); code != 64 {
		t.Fatalf("extra arg: code=%d, want 64", code)
	}
}

// TestAuthLoginPipedStdin login 管道通道（非 TTY 直收）：stdin 注入 =
// 非 *os.File → 管道语义，读全部输入修剪空白后验证落盘；空输入 = 用法
// 错误（64）。
func TestAuthLoginPipedStdin(t *testing.T) {
	env := startCLI(t)
	useTempConfig(t)
	rr := env.SeedUser(t, "ada@example.com", "password-123")
	pat := env.SeedUserToken(t, rr.User.ID)
	t.Setenv("FLEETLY_TOKEN", "")

	saved := stdin
	stdin = strings.NewReader(pat + "\n\n")
	t.Cleanup(func() { stdin = saved })

	code, out, errOut := runCLI(t, "auth", "login")
	if code != 0 {
		t.Fatalf("piped login: code=%d\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	cfg, err := loadCLIConfig()
	if err != nil || cfg.Token != pat {
		t.Fatalf("piped token not stored: cfg=%+v err=%v", cfg, err)
	}

	// --json 形态：user 投影 + stored 标记 + config 路径。
	stdin = strings.NewReader(pat)
	code, out, _ = runCLI(t, "auth", "login", "--json")
	if code != 0 {
		t.Fatalf("piped login --json: code=%d", code)
	}
	var lj struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		Stored bool   `json:"stored"`
		Config string `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &lj); err != nil || !lj.Stored ||
		lj.User.Email != "ada@example.com" || lj.Config == "" {
		t.Fatalf("login --json = (%+v, %v), out=%s", lj, err, out)
	}

	// 空管道输入 → 用法错误。
	stdin = strings.NewReader("  \n")
	if code, _, errOut := runCLI(t, "auth", "login"); code != 64 {
		t.Fatalf("empty stdin: code=%d, want 64\nstderr=%s", code, errOut)
	}
}

// TestAuthLogoutClearsLocalState logout：清凭据与上下文（文件回 pristine）、
// 幂等（无凭据也是成功）、服务端不动的文案指引、--json 形态、清完 status
// 回到无凭据态。
func TestAuthLogoutClearsLocalState(t *testing.T) {
	env := startCLI(t)
	path := useTempConfig(t)
	rr := env.SeedUser(t, "ada@example.com", "password-123")
	pat := env.SeedUserToken(t, rr.User.ID)
	t.Setenv("FLEETLY_TOKEN", "")
	if err := saveCLIConfig(cliConfig{Token: pat, CurrentTeam: "acme"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	code, out, _ := runCLI(t, "auth", "logout")
	if code != 0 || !strings.Contains(out, "logged out") || !strings.Contains(out, "tokens revoke") {
		t.Fatalf("logout: code=%d out=%s", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config file must be removed by logout, stat err = %v", err)
	}
	// 清完即无凭据态（同进程链路回归）。
	if code, _, errOut := runCLI(t, "auth", "status"); code != 1 || !strings.Contains(errOut, "not logged in") {
		t.Fatalf("status after logout: code=%d stderr=%s", code, errOut)
	}

	// 幂等：再 logout 仍 exit 0（already logged out）。
	code, out, _ = runCLI(t, "auth", "logout")
	if code != 0 || !strings.Contains(out, "already logged out") {
		t.Fatalf("idempotent logout: code=%d out=%s", code, out)
	}

	// --json 形态。
	code, out, _ = runCLI(t, "auth", "logout", "--json")
	if code != 0 {
		t.Fatalf("logout --json: code=%d", code)
	}
	var lj struct {
		LoggedOut bool   `json:"logged_out"`
		Cleared   bool   `json:"cleared"`
		Config    string `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &lj); err != nil || !lj.LoggedOut || lj.Cleared || lj.Config == "" {
		t.Fatalf("logout --json = (%+v, %v)", lj, err)
	}
}

// TestAuthVerbSurfaces auth 动词面：顶层 help 列出 auth、help auth 列出
// 三个子命令、auth 无子命令 = 用法错误 64、未知子命令 = 64、login 的 -h
// 帮助面不回显 FLEETLY_TOKEN（H1 纪律对新增 flag 面的复检——--team/
// --project 同款空缺省）。
func TestAuthVerbSurfaces(t *testing.T) {
	code, out, _ := runCLI(t)
	if code != 0 || !strings.Contains(out, "auth") {
		t.Fatalf("top-level help missing auth verb: code=%d", code)
	}
	code, out, _ = runCLI(t, "help", "auth")
	if code != 0 {
		t.Fatalf("help auth: code=%d", code)
	}
	for _, want := range []string{"login", "status", "logout"} {
		if !strings.Contains(out, want) {
			t.Errorf("help auth missing %q:\n%s", want, out)
		}
	}
	if code, _, errOut := runCLI(t, "auth"); code != 64 || !strings.Contains(errOut, "missing subcommand") {
		t.Fatalf("bare auth: code=%d stderr=%s, want 64/usage", code, errOut)
	}
	if code, _, _ := runCLI(t, "auth", "frobnicate"); code != 64 {
		t.Fatalf("unknown subcommand: code=%d, want 64", code)
	}

	const secret = "flt_h1_no_env_echo_regression" //nolint:gosec // G101：回显回归钉测标记，非真实凭据
	t.Setenv("FLEETLY_TOKEN", secret)
	code, out, _ = runCLI(t, "auth", "login", "-h")
	if code != 0 {
		t.Fatalf("auth login -h: code=%d", code)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("auth login -h echoed the FLEETLY_TOKEN value:\n%s", out)
	}
	if !strings.Contains(out, "FLEETLY_TEAM") || !strings.Contains(out, "FLEETLY_PROJECT") {
		t.Errorf("auth login -h missing team/project env guidance:\n%s", out)
	}
}

// TestTeamProjectFlagsOnRemoteVerbs --team/--project 公共 flag 面在任何远
// 程动词可解析（本票仅 status 消费；资源命令按上下文过滤是 W2——flag 面
// 先立好、值不改变既有行为：apps list 照常返回）。
func TestTeamProjectFlagsOnRemoteVerbs(t *testing.T) {
	startCLI(t)
	code, out, errOut := runCLI(t, "apps", "list", "--team", "acme", "--project", "web", "--json")
	if code != 0 || !strings.Contains(out, `"apps": []`) {
		t.Fatalf("apps list with context flags: code=%d out=%s stderr=%s", code, out, errOut)
	}
}
