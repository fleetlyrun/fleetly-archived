package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynx-go/commands"
)

// testAppVersion 是测试装配 CLI 用的版本值（与未注入 ldflags 的生产缺省
// 一致，version 动词的 golden 快照按此值匹配）。
const testAppVersion = "dev"

// runCLI 以给定参数驱动完整 CLI（同路径复用 NewApp；不起子进程、不改
// os.Args/CWD——lynx-go/commands 不读进程参数，无 Runner 隔离问题）。
// 返回退出码与 stdout/stderr 内容。
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := &commands.Environment{Stdout: &stdout, Stderr: &stderr}
	code := NewApp(testAppVersion).Run(context.Background(), env, args)
	return code, stdout.String(), stderr.String()
}

// writeFixture 落盘 compose 夹具并返回绝对路径。
func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

const cliValid = `
name: my-api
services:
  web:
    image: nginx:1.27
    expose: ["8080"]
    healthcheck:
      test: ["CMD", "hc"]
    labels:
      fleetly.domains: "api.example.com"
`

const cliValidWithEnvEntry = `
name: my-api
services:
  web:
    image: nginx:1.27
    environment:
      DB_CREDENTIAL: super-secret-plaintext
    healthcheck:
      test: ["CMD", "hc"]
`

const cliInvalid = `
name: my-api
services:
  web:
    image: nginx:1.27
    deploy:
      update_config:
        failure_action: rollback
`

const cliOther = `
name: my-api
services:
  web:
    image: nginx:1.27
    expose: ["8080"]
    healthcheck:
      test: ["CMD", "hc"]
    labels:
      fleetly.domains: "api.example.com"
    environment:
      LOG_LEVEL: debug
  worker:
    image: my/worker
    command: run
`

// TestCLIVersion 冒烟：动词注册与帮助面完好。
func TestCLIVersion(t *testing.T) {
	code, out, _ := runCLI(t, "version")
	if code != 0 || !strings.Contains(out, "fleetly") {
		t.Fatalf("version: code=%d out=%q", code, out)
	}
	code, out, _ = runCLI(t)
	if code != 0 || !strings.Contains(out, "validate") || !strings.Contains(out, "plan") || !strings.Contains(out, "diff") {
		t.Fatalf("help: code=%d out=%q", code, out)
	}
}

// TestCLIValidateExitCodes 验收 5：validate 二态（0 有效 / 1 校验失败，
// 错误信封四件套上 stderr）。
func TestCLIValidateExitCodes(t *testing.T) {
	code, out, _ := runCLI(t, "validate", writeFixture(t, cliValid))
	if code != 0 || !strings.Contains(out, "my-api: valid") {
		t.Fatalf("valid compose: code=%d out=%q", code, out)
	}

	code, _, errOut := runCLI(t, "validate", writeFixture(t, cliInvalid))
	if code != 1 {
		t.Fatalf("invalid compose: code=%d, want 1", code)
	}
	for _, want := range []string{"E_COMPOSE_MANAGED_FIELD", "suggestion:", "docs:"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}

	// 不存在的文件 → 1。
	code, _, _ = runCLI(t, "validate", filepath.Join(t.TempDir(), "nope.yaml"))
	if code != 1 {
		t.Fatalf("missing file: code=%d, want 1", code)
	}
}

// TestCLIValidateJSON --json 形态可解析且携带 spec_hash。
func TestCLIValidateJSON(t *testing.T) {
	code, out, _ := runCLI(t, "validate", "--json", writeFixture(t, cliValid))
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var result struct {
		Valid    bool   `json:"valid"`
		Name     string `json:"name"`
		SpecHash string `json:"spec_hash"`
		Services int    `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if !result.Valid || result.Name != "my-api" || result.SpecHash == "" || result.Services != 1 {
		t.Errorf("validate JSON = %+v", result)
	}
}

// TestCLIPlanThreeState 验收 4/5：plan 三态退出码——0 无变化（自基线）、
// 2 有变化（异基线）、1 错误（--baseline 本地形态；RPC 基线形态在
// golden_test.go 的服务面夹具上覆盖）。
func TestCLIPlanThreeState(t *testing.T) {
	good := writeFixture(t, cliValid)

	// 自基线（同文件）→ 无变化 → 0。
	code, out, _ := runCLI(t, "plan", "--baseline", good, good)
	if code != 0 {
		t.Fatalf("no changes: code=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "no changes") {
		t.Errorf("no-change report missing \"no changes\":\n%s", out)
	}

	// 异基线 → 有变化 → 2。
	code, _, _ = runCLI(t, "plan", "--baseline", writeFixture(t, cliOther), good)
	if code != 2 {
		t.Fatalf("changed: code=%d, want 2", code)
	}

	// 校验失败 → 1（stderr 出错误码）。
	code, _, errOut := runCLI(t, "plan", writeFixture(t, cliInvalid))
	if code != 1 || !strings.Contains(errOut, "E_COMPOSE_MANAGED_FIELD") {
		t.Fatalf("error: code=%d stderr=%q", code, errOut)
	}
}

// TestCLIPlanJSONArtifact 验收 4：--json artifact 含 etag（spec_hash）；
// 脱敏负面测试——env 明文值不出现在任何输出流。
func TestCLIPlanJSONArtifact(t *testing.T) {
	target := writeFixture(t, cliValidWithEnvEntry)
	// 基线 = 同服务但 env 旧值（首部署只有 added，字段级差分需基线）。
	baseline := writeFixture(t, strings.Replace(cliValidWithEnvEntry, "super-secret-plaintext", "old-plaintext", 1))
	code, out, errOut := runCLI(t, "plan", "--baseline", baseline, "--json", target)
	if code != 2 {
		t.Fatalf("code=%d\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	var plan struct {
		SpecHash                   string `json:"spec_hash"`
		HasChanges                 bool   `json:"has_changes"`
		Destructive                bool   `json:"destructive"`
		RequiresConfirmDestructive bool   `json:"requires_confirm_destructive"`
		Services                   struct {
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
			Updated []struct {
				Name   string `json:"name"`
				Fields []struct {
					Path string `json:"path"`
					To   any    `json:"to"`
				} `json:"fields"`
			} `json:"updated"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("artifact is not JSON: %v\n%s", err, out)
	}
	if plan.SpecHash == "" || !plan.HasChanges {
		t.Errorf("artifact = %+v", plan)
	}
	if len(plan.Services.Updated) != 1 || plan.Services.Updated[0].Name != "web" {
		t.Fatalf("Updated = %+v", plan.Services.Updated)
	}
	// env 字段差分只含 hash 形态。
	foundEnvHash := false
	for _, f := range plan.Services.Updated[0].Fields {
		if strings.HasPrefix(f.Path, "environment.DB_CREDENTIAL") {
			foundEnvHash = true
		}
	}
	if !foundEnvHash {
		t.Errorf("missing environment.DB_CREDENTIAL diff: %+v", plan.Services.Updated[0].Fields)
	}
	// 负面测试：明文值（新值与基线旧值）不得出现在 stdout/stderr 任一流。
	if strings.Contains(out, "super-secret-plaintext") || strings.Contains(errOut, "super-secret-plaintext") ||
		strings.Contains(out, "old-plaintext") || strings.Contains(errOut, "old-plaintext") {
		t.Error("plan output leaks the env plaintext value")
	}
}

// TestCLIPlanDestructive 验收 4：破坏性标记（服务删除）出现在 artifact，
// 人读报告提示 --confirm-destructive。
func TestCLIPlanDestructive(t *testing.T) {
	base := writeFixture(t, cliOther)
	target := writeFixture(t, cliValid) // 少了 worker 服务

	code, out, _ := runCLI(t, "plan", "--baseline", base, "--json", target)
	if code != 2 {
		t.Fatalf("code=%d\n%s", code, out)
	}
	var plan struct {
		Destructive                bool `json:"destructive"`
		RequiresConfirmDestructive bool `json:"requires_confirm_destructive"`
		Services                   struct {
			Removed []string `json:"removed"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if len(plan.Services.Removed) != 1 || plan.Services.Removed[0] != "worker" {
		t.Fatalf("Removed = %v, want [worker]", plan.Services.Removed)
	}
	if !plan.Destructive || !plan.RequiresConfirmDestructive {
		t.Errorf("destructive flags missing: %+v", plan)
	}

	// 人读报告带 --confirm-destructive 提示。
	code, out, _ = runCLI(t, "plan", "--baseline", base, target)
	if code != 2 || !strings.Contains(out, "--confirm-destructive") {
		t.Fatalf("human-readable destructive hint missing: code=%d\n%s", code, out)
	}
}

// TestCLIPlanOutputArtifact --output 落盘 artifact（etag 机制位；--baseline
// 本地形态，不依赖 daemon）。
func TestCLIPlanOutputArtifact(t *testing.T) {
	dir := t.TempDir()
	artifactPath := filepath.Join(dir, "plan.json")
	base := writeFixture(t, cliValid)
	target := writeFixture(t, strings.Replace(cliValid, "nginx:1.27", "nginx:1.29", 1))
	code, out, _ := runCLI(t, "plan", "--baseline", base, "--output", artifactPath, target)
	if code != 2 || !strings.Contains(out, "plan artifact written to") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	raw, err := os.ReadFile(artifactPath) //nolint:gosec // artifact 为本测试 --output 落盘的临时路径
	if err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	var plan struct {
		SpecHash string `json:"spec_hash"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil || plan.SpecHash == "" {
		t.Fatalf("written artifact invalid: %v %s", err, raw)
	}
}

// TestCLIDiffThreeState diff 与 plan 同三态。
func TestCLIDiffThreeState(t *testing.T) {
	a := writeFixture(t, cliValid)
	code, out, _ := runCLI(t, "diff", a, a)
	if code != 0 || !strings.Contains(out, "no changes") {
		t.Fatalf("same: code=%d out=%q", code, out)
	}
	code, _, _ = runCLI(t, "diff", a, writeFixture(t, cliOther))
	if code != 2 {
		t.Fatalf("different: code=%d, want 2", code)
	}
	code, _, errOut := runCLI(t, "diff", a, writeFixture(t, cliInvalid))
	if code != 1 || !strings.Contains(errOut, "E_COMPOSE_MANAGED_FIELD") {
		t.Fatalf("error: code=%d stderr=%q", code, errOut)
	}
	// 参数数量违规 → 用法错误（64，S17-D3）。
	code, _, _ = runCLI(t, "diff", a)
	if code != 64 {
		t.Fatalf("usage: code=%d, want 64", code)
	}
}

// TestCLIUsageExitCodes S17-D3：用法类错误（未知动词/flag 解析失败/位置
// 参数违规）退出 64（EX_USAGE 惯例）——与 plan/diff「检测到变化」的 2
// 分离，Agent/脚本可区分"有漂移"与"调用姿势错误"。
func TestCLIUsageExitCodes(t *testing.T) {
	// 未知动词 → 64。
	code, _, _ := runCLI(t, "no-such-verb")
	if code != 64 {
		t.Fatalf("unknown verb: code=%d, want 64", code)
	}
	// flag 解析失败 → 64。
	code, _, _ = runCLI(t, "validate", "--nope", writeFixture(t, cliValid))
	if code != 64 {
		t.Fatalf("bad flag: code=%d, want 64", code)
	}
	// 位置参数缺失 → 64。
	code, _, _ = runCLI(t, "validate")
	if code != 64 {
		t.Fatalf("missing arg: code=%d, want 64", code)
	}
	// 对照：plan 有变化仍是 2（不与用法错误混用）。
	code, _, _ = runCLI(t, "plan", "--baseline", writeFixture(t, cliOther), writeFixture(t, cliValid))
	if code != 2 {
		t.Fatalf("changes: code=%d, want 2", code)
	}
}

// TestCLINestedUnknownSubcommandExitCode H12 回归：嵌套未知子命令与顶层
// 未知动词同为用法错误（exit 64，README 退出码契约）——框架 SubDispatch
// 会把内层 miss 重写成 plain error（丢类型 → 顶层 exitCodeFor 判不中 →
// exit 1），外层动词统一经 subDispatchUsage（app.go）收口保住用法类语
// 义：64 + "unknown subcommand" 文案 + 外层动词的 usage 提示行。
func TestCLINestedUnknownSubcommandExitCode(t *testing.T) {
	for _, args := range [][]string{
		{"apps", "frobnicate"},   // 单层嵌套（apps 收口）
		{"tokens", "frobnicate"}, // 第二条外层动词（同一收口的复抽）
		{"audit", "frobnicate"},  // 第三条外层动词（深嵌套面——audit query 的外层）
	} {
		code, _, errOut := runCLI(t, args...)
		if code != 64 {
			t.Fatalf("%v: code=%d, want 64 (nested unknown subcommand = usage error)\nstderr=%s", args, code, errOut)
		}
		if !strings.Contains(errOut, `unknown subcommand "frobnicate"`) {
			t.Errorf("%v: stderr missing \"unknown subcommand\" text:\n%s", args, errOut)
		}
		if !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: stderr missing usage hint line:\n%s", args, errOut)
		}
	}
}

// TestREADMEExamplesFlagsBeforePositional H13 回归：README 的 CLI 示例
// 必须是 flags 前置形态——std flag 在首个位置参数处停止解析，flags 后置
// 会被原样留在位置参数里（requireArgs 随即报参数数量违规）。逐条以
// ParseFlags 钉死 README 改过的两条示例（logs follow / apps webhook
// set-source），并对照演示后置形态确属非法。
func TestREADMEExamplesFlagsBeforePositional(t *testing.T) {
	app := commands.New()
	env := &commands.Environment{Stdout: io.Discard, Stderr: io.Discard}

	// `fleetly logs follow --service web my-api`
	logs := &logsFollowCmd{}
	rest, err := app.ParseFlags(logs, env, []string{"--service", "web", "my-api"})
	if err != nil {
		t.Fatalf("logs follow parse failed: %v", err)
	}
	if logs.service != "web" || len(rest) != 1 || rest[0] != "my-api" {
		t.Fatalf("logs follow: service=%q rest=%v, want web / [my-api]", logs.service, rest)
	}

	// `fleetly apps webhook set-source --branch main --auth-kind none my-api https://…`
	src := &webhookSourceSetCmd{}
	rest, err = app.ParseFlags(src, env, []string{"--branch", "main", "--auth-kind", "none",
		"my-api", "https://github.com/acme/web.git"})
	if err != nil {
		t.Fatalf("apps webhook set-source parse failed: %v", err)
	}
	if src.branch != "main" || src.authKind != "none" || len(rest) != 2 ||
		rest[0] != "my-api" || rest[1] != "https://github.com/acme/web.git" {
		t.Fatalf("set-source: branch=%q authKind=%q rest=%v, want main/none/[my-api url]", src.branch, src.authKind, rest)
	}

	// 对照：flags 后置（README 修正前的形态）解析停在首个位置参数——flag
	// 未消费、残留 3 个位置参数（requireArgs 将判参数数量违规，exit 64）。
	after := &logsFollowCmd{}
	rest, err = app.ParseFlags(after, env, []string{"my-api", "--service", "web"})
	if err != nil || after.service != "" || len(rest) != 3 {
		t.Fatalf("trailing-flags form behaves unexpectedly: err=%v service=%q rest=%v (want nil/empty/3 leftovers)", err, after.service, rest)
	}
}
