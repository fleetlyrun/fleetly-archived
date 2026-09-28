package cmd

// golden 快照测试（T2.18 票面验收项，防 --json 输出漂移）：对每个动词的
// --json 输出做 golden 快照（testdata/golden/）。夹具 = 进程内真实 api
// 服务面（internal/apitest：真实 state.Store + 生产同构拦截链 + bufconn
// gRPC server），CLI 经生产同路径（fleetly.NewClient + --addr/--token，
// env 覆盖 FLEETLY_ADDR/FLEETLY_TOKEN）连入——不 mock、不旁路。
//
// 时间戳/ID/哈希/token 等非确定字段在快照前归一为占位符（normalizeVolatile
// ——顺序：token → ULID → sha256 → RFC3339）。`go test -update` 重新生成
// golden（契约有意变更时）。
//
// 退出码三态断言（架构 §2.4）一并落在本文件：plan 有变化→2、无变化→0；
// deploy 失败→1（信封渲染）；401→1（可行动提示）。

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lynx-go/commands"

	"github.com/fleetlyrun/fleetly/internal/apitest"
)

// goldenUpdate 重写 golden 文件（契约有意变更时的再生成开关）。
var goldenUpdate = flag.Bool("update", false, "rewrite golden snapshot files")

// goldenDir 是 golden 快照目录（相对 cmd/fleetly）。
const goldenDir = "testdata/golden"

// volatile 归一规则（顺序敏感：token 在 sha256 之前——token 是 48 hex，
// 不会被 64 规则命中，但先归一保持意图清晰）。
var (
	volatileToken   = regexp.MustCompile(`flt_[0-9a-f]{48}`)
	volatileULID    = regexp.MustCompile(`[0-9A-HJ-NP-TV-Z]{26}`)
	volatileSHA256  = regexp.MustCompile(`[0-9a-f]{64}`)
	volatileRFC3339 = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	// volatileAppID8 是命名约定里的 app ID 前 8 位（卷名 fleetly-<app>-
	// <key>-<appid8>；ULID 片段，逐次不同）。
	volatileAppID8 = regexp.MustCompile(`-([0-9A-HJ-NP-TV-Z]{8})"`)
	// volatileHashPrefix 是 token 列表的哈希前缀（sha256 前 12 hex——
	// 识别用非凭据，但逐 token 不同）。
	volatileHashPre = regexp.MustCompile(`"hash_prefix": "[0-9a-f]{12}"`)
	// volatileFingerprint 是 git key 的 SHA256 指纹（T2.19；识别用非凭据，
	// 逐 key 不同）。
	volatileFingerprint = regexp.MustCompile(`SHA256:[A-Za-z0-9+/]+={0,3}`)
	// volatileQualifiedName 是三段限定形 app 值（v0.3 W2-S3 流标签口径：
	// t<pkg>/p<pkg> 夹具 slug 段逐次不同——golden 只保留裸名段；紧凑与
	// protojson 两种冒号形态都归一）。
	volatileQualifiedName   = regexp.MustCompile(`"app": "t[0-9a-z]{16}/p[0-9a-z]{16}/`)
	volatileQualifiedNameCp = regexp.MustCompile(`"app":"t[0-9a-z]{16}/p[0-9a-z]{16}/`)
	// volatileSlugPair 是归属 slug 投影（v0.3 W2-S3 AppView/GetAppResponse
	// 新增 team_slug/project_slug——夹具 slug 逐次随机，golden 归一为占位）。
	volatileSlugPair = regexp.MustCompile(`"(team|project)_slug": "[tp][0-9a-z]{16}"`)
)

// normalizeVolatile 把非确定字段替换为占位符（golden 的确定性边界）。
func normalizeVolatile(s string) string {
	s = volatileToken.ReplaceAllString(s, "<token>")
	s = volatileULID.ReplaceAllString(s, "<ulid>")
	s = volatileSHA256.ReplaceAllString(s, "<sha256>")
	s = volatileRFC3339.ReplaceAllString(s, "<rfc3339>")
	s = volatileAppID8.ReplaceAllString(s, `-<appid8>"`)
	s = volatileHashPre.ReplaceAllString(s, `"hash_prefix": "<hashprefix>"`)
	s = volatileFingerprint.ReplaceAllString(s, "SHA256:<fingerprint>")
	s = volatileQualifiedName.ReplaceAllString(s, `"app": "`)
	s = volatileQualifiedNameCp.ReplaceAllString(s, `"app":"`)
	s = volatileSlugPair.ReplaceAllString(s, `"${1}_slug": "<${1}-slug>"`)
	return s
}

// startCLI 拨通进程内服务面：起 apitest 环境并注入拨号器 + FLEETLY_*
// 环境覆盖（env 路径与 flag 路径共用同一 connFlags 读点）。配置文件隔离
//（W1-S4）：dial 的 token 解析在 env 缺失时回落 ~/.fleetly/config.yaml
//（resolveToken）——测试把 configPathOverride 指到临时目录，开发机上的
// 真实登录态不得影响夹具语义（凭据一律经 env/flag 显式注入）。
func startCLI(t *testing.T) *apitest.Env {
	t.Helper()
	return startCLIWith(t, apitest.Start(t))
}

// startCLIWithBuildQueue 是 startCLI 的构建执行面变体（IMPL-T2-2：上传构建
// 的 CLI 端到端——真实队列 + 确定性假执行器；纯入队夹具语义见 startCLI）。
func startCLIWithBuildQueue(t *testing.T) *apitest.Env {
	t.Helper()
	return startCLIWith(t, apitest.StartWithBuildQueue(t))
}

// startCLIWith 是 CLI 夹具的共用装配（startCLI 与构建队列变体同点接线）。
func startCLIWith(t *testing.T, env *apitest.Env) *apitest.Env {
	t.Helper()
	restore := env.DialOptions()
	saved := extraDialOptions
	extraDialOptions = restore
	t.Cleanup(func() { extraDialOptions = saved })
	savedPath := configPathOverride
	configPathOverride = filepath.Join(t.TempDir(), "home", ".fleetly", "config.yaml")
	t.Cleanup(func() { configPathOverride = savedPath })
	// v0.3 归属管道（W2-S3）：AdminToken 是机具令牌——无缺省项目，CLI 请求
	// 必须显式携带。夹具项目经 FLEETLY_PROJECT 上下文注入（connFlags 解析
	// 序 flag > env，全部 CLI 命令零改动继承）。
	proj := env.SeedProject(t)
	t.Setenv("FLEETLY_PROJECT", proj.ID)
	t.Setenv("FLEETLY_ADDR", "passthrough:///bufnet")
	t.Setenv("FLEETLY_TOKEN", env.AdminToken)
	return env
}

// runCLIConn 在服务面夹具上驱动完整 CLI（返回退出码与 stdout/stderr）。
func runCLIConn(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	e := &commands.Environment{Stdout: &stdout, Stderr: &stderr}
	code := NewApp(testAppVersion).Run(context.Background(), e, args)
	return code, stdout.String(), stderr.String()
}

// compareGolden 对照/更新 golden 快照。
func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	got = normalizeVolatile(got)
	path := filepath.Join(goldenDir, name+".golden")
	if *goldenUpdate {
		if err := os.MkdirAll(goldenDir, 0o750); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil { //nolint:gosec // golden 文件为测试受控路径
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304：golden 路径为测试受控目录拼接
	if err != nil {
		t.Fatalf("golden %s missing (generate with go test ./cmd/fleetly/cmd -run TestGolden -update): %v\n--- got ---\n%s", path, err, got)
	}
	if string(want) != got {
		t.Fatalf("golden %s drifted (an intentional --json contract change requires an explicit -update regeneration):\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// writeCLIFixture 落盘 compose 夹具（服务面测试用——与 app_test.go 的
// writeFixture 同形，独立命名避免跨文件语义混淆）。
func writeCLIFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// cliWebApp 是最小合法 compose（image 模式，无外部依赖）。
const cliWebApp = `
name: my-api
services:
  web:
    image: nginx:1.27
    expose: ["8080"]
    healthcheck:
      test: ["CMD", "hc"]
`

// TestGoldenVersion version --json（本地动词；无服务面依赖）。
func TestGoldenVersion(t *testing.T) {
	code, out, errOut := runCLIConn(t, "version", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "version", out)
}

// TestGoldenValidate validate --json：干净夹具与 cron-label 夹具各一。
// E5 Cron 起 fleetly.cron 为生效契约——合法声明不再产 v0.1 的「v0.2 生效」
// 警告（夹具仅剩无 healthcheck 的既有警告），--json artifact 如实反映。
func TestGoldenValidate(t *testing.T) {
	clean := writeCLIFixture(t, cliWebApp)
	code, out, errOut := runCLIConn(t, "validate", "--json", clean)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "validate", out)

	cronFixture := writeCLIFixture(t, `
name: my-api
services:
  web:
    image: nginx:1.27
    labels:
      fleetly.cron: "*/5 * * * *"
`)
	code, out, _ = runCLIConn(t, "validate", "--json", cronFixture)
	if code != 0 {
		t.Fatalf("cron: code=%d", code)
	}
	compareGolden(t, "validate_cron_live", out)
}

// TestGoldenDiff diff --json（本地归一化差异；有变化 → 退出 2）。
func TestGoldenDiff(t *testing.T) {
	a := writeCLIFixture(t, cliWebApp)
	b := writeCLIFixture(t, strings.Replace(cliWebApp, "nginx:1.27", "nginx:1.29", 1))
	code, out, errOut := runCLIConn(t, "diff", "--json", a, b)
	if code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "diff_changed", out)
}

// TestGoldenPlanFirstDeploy plan --json 无 --baseline（RPC 基线路径）：
// app 无 revision → 首部署语义，一切新增 → 退出 2。
func TestGoldenPlanFirstDeploy(t *testing.T) {
	startCLI(t)
	target := writeCLIFixture(t, cliWebApp)
	code, out, errOut := runCLIConn(t, "plan", "--json", target)
	if code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, `"web"`) {
		t.Fatalf("plan artifact missing new service web:\n%s", out)
	}
	compareGolden(t, "plan_first_deploy", out)
}

// TestGoldenPlanRPCBaselineNoChanges plan RPC 基线的无变化分支：最近成功
// revision 快照 == 目标 → 退出 0（基线不经文件、经 GetRevisionSpec 快照）。
func TestGoldenPlanRPCBaselineNoChanges(t *testing.T) {
	env := startCLI(t)
	env.SeedRevisionFromYAML(t, "my-api", cliWebApp)
	target := writeCLIFixture(t, cliWebApp)
	code, out, errOut := runCLIConn(t, "plan", "--json", target)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "plan_no_changes", out)
}

// TestGoldenAppsList apps list --json（空列表与单应用两态）。
func TestGoldenAppsList(t *testing.T) {
	env := startCLI(t)
	code, out, errOut := runCLIConn(t, "apps", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "apps_list_empty", out)

	env.CreateApp(t, "my-api")
	code, out, _ = runCLIConn(t, "apps", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	compareGolden(t, "apps_list", out)
}

// TestGoldenAppsGet apps get --json（protojson 形态：placement 未绑定不
// 输出、recent_deployments 空集）。
func TestGoldenAppsGet(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")
	code, out, errOut := runCLIConn(t, "apps", "get", "--json", "my-api")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "apps_get", out)
}

// TestGoldenDeployLifecycle deploy 入队 → queued 超时（无引擎推进的确定性
// 失败）→ deployments list 可查。三处快照 + 退出码 1。
func TestGoldenDeployLifecycle(t *testing.T) {
	startCLI(t)
	composeFile := writeCLIFixture(t, cliWebApp)

	code, out, errOut := runCLIConn(t, "deploy", "--timeout", "1200ms", "--json", composeFile)
	if code != 1 {
		t.Fatalf("deploy queued-timeout: code=%d, want 1\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	compareGolden(t, "deploy_queued_timeout", out)

	// 人读失败形态：stderr 带可行动提示（daemon 未运行的归因）。
	if !strings.Contains(errOut, "is fleetlyd running") {
		t.Fatalf("deploy timeout stderr missing actionable hint: %q", errOut)
	}

	// deployments list --json：入队的 queued 行可查。
	code, out, errOut = runCLIConn(t, "deployments", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("deployments list: code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "deployments_list", out)
}

// TestGoldenDeployInvalidCompose deploy 的 compose 违约：服务端受控子集
// 校验拒绝（E_COMPOSE_* 信封经 status detail 回流），退出 1、不入队。
func TestGoldenDeployInvalidCompose(t *testing.T) {
	startCLI(t)
	bad := writeCLIFixture(t, `
name: my-api
services:
  web:
    image: nginx:1.27
    deploy:
      update_config:
        failure_action: rollback
`)
	code, out, errOut := runCLIConn(t, "deploy", "--json", bad)
	if code != 1 {
		t.Fatalf("code=%d, want 1\nstdout=%s", code, out)
	}
	for _, want := range []string{"E_COMPOSE_MANAGED_FIELD", "suggestion:", "docs:"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	// 违约不入队：应用行不存在（随首次部署创建的语义未被触发）。
	code, out, _ = runCLIConn(t, "apps", "list", "--json")
	if code != 0 || !strings.Contains(out, `"apps": []`) {
		t.Fatalf("invalid deploy must not create the app row: code=%d out=%s", code, out)
	}
}

// TestGoldenRevisionsList revisions list --json（空与保留窗单条）。
func TestGoldenRevisionsList(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")
	code, out, errOut := runCLIConn(t, "revisions", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "revisions_list_empty", out)

	env.SeedRevisionFromYAML(t, "my-api", cliWebApp)
	code, out, _ = runCLIConn(t, "revisions", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	compareGolden(t, "revisions_list", out)
}

// TestGoldenEnvLifecycle env set/list/get/rm --json 全生命周期 + 脱敏负面
// 断言（值不出现在 list；get 为 admin 显式路径）。
func TestGoldenEnvLifecycle(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")

	code, out, errOut := runCLIConn(t, "env", "set", "--json", "my-api", "DATABASE_URL", "postgres://secret-conn")
	if code != 0 {
		t.Fatalf("set: code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "env_set", out)

	code, out, errOut = runCLIConn(t, "env", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("list: code=%d stderr=%s", code, errOut)
	}
	if strings.Contains(out, "postgres://secret-conn") {
		t.Fatal("env list leaks the value (redaction broken)")
	}
	compareGolden(t, "env_list", out)

	// get（admin token）：显式读回明文。
	code, out, _ = runCLIConn(t, "env", "get", "--json", "my-api", "DATABASE_URL")
	if code != 0 || !strings.Contains(out, "postgres://secret-conn") {
		t.Fatalf("get: code=%d out=%s", code, out)
	}
	compareGolden(t, "env_get", out)

	code, out, _ = runCLIConn(t, "env", "rm", "--json", "my-api", "DATABASE_URL")
	if code != 0 {
		t.Fatalf("rm: code=%d", code)
	}
	compareGolden(t, "env_rm", out)
}

// TestGoldenPlacementShow placement show --json：未绑定（合法展示态）与
// 已绑定 + 卷 + 约束两态。
func TestGoldenPlacementShow(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")
	code, out, errOut := runCLIConn(t, "placement", "show", "--json", "my-api")
	if code != 0 {
		t.Fatalf("unbound: code=%d stderr=%s", code, errOut)
	}
	if strings.Contains(out, "not found") {
		t.Fatalf("unbound app must render, not error: %s", out)
	}
	compareGolden(t, "placement_unbound", out)

	env.SeedPlacement(t, "my-api")
	code, out, _ = runCLIConn(t, "placement", "show", "--json", "my-api")
	if code != 0 {
		t.Fatalf("bound: code=%d", code)
	}
	compareGolden(t, "placement_bound", out)
}

// TestGoldenNodesList nodes list --json（观测缓存；空与单节点两态）。
func TestGoldenNodesList(t *testing.T) {
	env := startCLI(t)
	code, out, errOut := runCLIConn(t, "nodes", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "nodes_list_empty", out)

	env.SeedNode(t)
	code, out, _ = runCLIConn(t, "nodes", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	compareGolden(t, "nodes_list", out)
}

// TestGoldenBackupsList backups list --json（台账空态 + verified/failed
// 两行态——失败行如实出现在列表是红色告警面的一部分）。
func TestGoldenBackupsList(t *testing.T) {
	env := startCLI(t)
	code, out, errOut := runCLIConn(t, "backups", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "backups_list_empty", out)

	env.SeedBackup(t, "pre_upgrade", "verified", "")
	env.SeedBackup(t, "daily", "failed", "statebackup: integrity_check reported \"corrupt page\"")
	code, out, _ = runCLIConn(t, "backups", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	compareGolden(t, "backups_list", out)
}

// TestGoldenDomainsList domains list --json（空与单行两态）。
func TestGoldenDomainsList(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")
	code, out, errOut := runCLIConn(t, "domains", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "domains_list_empty", out)

	env.SeedDomain(t, "my-api", "web", "api.example.com", "8080")
	code, out, _ = runCLIConn(t, "domains", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	compareGolden(t, "domains_list", out)
}

// TestGoldenDriftSurface drift show/converge/enable/disable --json：无成功
// 部署时 show 为空报告、converge 拒绝（无目标）、enable/disable 置位回执。
func TestGoldenDriftSurface(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")

	code, out, errOut := runCLIConn(t, "drift", "show", "--json", "my-api")
	if code != 0 {
		t.Fatalf("show: code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "drift_show_no_baseline", out)

	code, out, _ = runCLIConn(t, "drift", "enable", "--json", "my-api")
	if code != 0 {
		t.Fatalf("enable: code=%d", code)
	}
	compareGolden(t, "drift_enable", out)

	code, out, _ = runCLIConn(t, "drift", "disable", "--json", "my-api")
	if code != 0 {
		t.Fatalf("disable: code=%d", code)
	}
	compareGolden(t, "drift_disable", out)

	// converge 无成功部署 → 拒绝（退出 1；服务端错误原文）。
	code, _, errOut = runCLIConn(t, "drift", "converge", "--json", "my-api")
	if code != 1 || !strings.Contains(errOut, "no succeeded deployment") {
		t.Fatalf("converge: code=%d stderr=%q", code, errOut)
	}
}

// TestGoldenIngressStatus ingress status --json（nil ingress 端口形态：
// Traefik 如实报告不可用——入口面未装配）。
func TestGoldenIngressStatus(t *testing.T) {
	startCLI(t)
	code, out, errOut := runCLIConn(t, "ingress", "status", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "ingress_status", out)
}

// TestGoldenBuildsList builds list --json（空与 queued 单条两态）+ build
// 直通（image 模式无构建行）。
func TestGoldenBuildsList(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")
	code, out, errOut := runCLIConn(t, "builds", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "builds_list_empty", out)

	env.SeedBuild(t, "my-api", "web")
	code, out, _ = runCLIConn(t, "builds", "list", "--json", "my-api")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	compareGolden(t, "builds_list", out)

	// build 触发（image 模式 → 直通、零构建行、退出 0）。
	composeFile := writeCLIFixture(t, cliWebApp)
	code, out, _ = runCLIConn(t, "build", "--json", composeFile)
	if code != 0 {
		t.Fatalf("build passthrough: code=%d out=%s", code, out)
	}
	compareGolden(t, "build_passthrough", out)
}

// TestGoldenTokensLifecycle tokens create/list/revoke --json 全生命周期：
// 新签发 token 立即可用（apps list 成功——非 bootstrap token 生效证明），
// revoke 后同 token 失效（401 + 可行动提示）。
func TestGoldenTokensLifecycle(t *testing.T) {
	startCLI(t)
	admin := os.Getenv("FLEETLY_TOKEN")

	// read scope 的新 token。
	code, out, errOut := runCLIConn(t, "tokens", "create", "--json",
		"--scopes", "read", "--note", "golden ci")
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%s", code, errOut)
	}
	var created struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Token == "" {
		t.Fatalf("create resp: %s err=%v", out, err)
	}
	compareGolden(t, "tokens_create", out)

	code, out, _ = runCLIConn(t, "tokens", "list", "--json")
	if code != 0 {
		t.Fatalf("list: code=%d", code)
	}
	compareGolden(t, "tokens_list", out)

	// 新 token 可用（read scope 的只读面）。
	t.Setenv("FLEETLY_TOKEN", created.Token)
	code, out, _ = runCLIConn(t, "apps", "list", "--json")
	if code != 0 {
		t.Fatalf("new token apps list: code=%d out=%s", code, out)
	}
	// read scope 的机具令牌写 token 管理面 → 403 信封、退出 1（W2 语义
	// 迁移：机具令牌造 token 属平台级写面，须 admin scope——handler 收口，
	// scope 门 read 只做形状约束）。
	code, _, errOut = runCLIConn(t, "tokens", "create", "--json", "--scopes", "read")
	if code != 1 || !strings.Contains(errOut, "PermissionDenied") ||
		!strings.Contains(errOut, "machine token lacks the admin scope") {
		t.Fatalf("read machine token creating a token should 403: code=%d stderr=%q", code, errOut)
	}

	// revoke 需要 admin——切回 admin token 执行。
	t.Setenv("FLEETLY_TOKEN", admin)
	code, out, _ = runCLIConn(t, "tokens", "revoke", "--json", created.ID)
	if code != 0 {
		t.Fatalf("revoke: code=%d out=%s", code, out)
	}
	compareGolden(t, "tokens_revoke", out)

	// 被吊销 token 再调 → 401（invalid or revoked token）+ hint，退出 1。
	t.Setenv("FLEETLY_TOKEN", created.Token)
	code, _, errOut = runCLIConn(t, "apps", "list", "--json")
	if code != 1 || !strings.Contains(errOut, "invalid or revoked token") ||
		!strings.Contains(errOut, "hint:") {
		t.Fatalf("revoked token: code=%d stderr=%q", code, errOut)
	}
}

// TestGoldenLogsHistoryAndFollow logs history --json（落盘检索，夹具管线
// 恰好一行）与 logs follow --json（ring 回放 + 实时扇出的 JSONL 单帧）。
func TestGoldenLogsHistoryAndFollow(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")

	// 管线首轮扫描后恰好一行（fakeLogPort 单行 + 永久阻塞）。
	waitFor(t, 3*time.Second, func() bool {
		code, out, _ := runCLIConn(t, "logs", "history", "--json", "my-api")
		return code == 0 && strings.Contains(out, "hello from web")
	})

	code, out, errOut := runCLIConn(t, "logs", "history", "--json", "my-api")
	if code != 0 {
		t.Fatalf("history: code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "logs_history", out)

	// follow：ring 回放一帧后阻塞，ctx 超时收口（JSONL 单行）。
	followCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	e := &commands.Environment{Stdout: &stdout, Stderr: &stderr}
	code = NewApp(testAppVersion).Run(followCtx, e, []string{"logs", "follow", "--json", "my-api"})
	if code != 1 {
		t.Fatalf("follow: code=%d stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); strings.Count(got, "\n") != 1 {
		t.Fatalf("follow JSONL should be exactly one frame: %q", got)
	}
	compareGolden(t, "logs_follow", stdout.String())
}

// TestGoldenEventsWatch events watch --json：播种一帧后 ctx 超时收口
// （JSONL 单行；重连续读语义由 events_test.go 的游标断言承载）。watch 游标
// 跳过夹具团队/项目的播种事件（--since-seq = 最后一帧播种 seq）。
func TestGoldenEventsWatch(t *testing.T) {
	env := startCLI(t)
	appended := env.AppendEvent(t, "deployment.queued", "deployment:01TEST", `{"deployment":"01TEST","app":"my-api"}`)

	watchCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	e := &commands.Environment{Stdout: &stdout, Stderr: &stderr}
	code := NewApp(testAppVersion).Run(watchCtx, e, []string{"events", "watch", "--json", "--since-seq", fmt.Sprintf("%d", appended-1)})
	if code != 1 {
		t.Fatalf("watch: code=%d stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); strings.Count(got, "\n") != 1 {
		t.Fatalf("watch JSONL should be exactly one frame: %q", got)
	}
	compareGolden(t, "events_watch", stdout.String())
}

// TestCLITokenMissingHint 无 token 的 apps list：服务端 401（信封退化形态）
// + CLI 附加 bootstrap token 可行动提示，退出 1。
func TestCLITokenMissingHint(t *testing.T) {
	startCLI(t)
	t.Setenv("FLEETLY_TOKEN", "")
	code, _, errOut := runCLIConn(t, "apps", "list", "--json")
	if code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
	for _, want := range []string{"missing bearer token", "hint:", "bootstrap admin token"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// TestCLIWrongTokenEnvelope 错 token：服务端 401 信封（invalid or revoked
// token）+ hint，退出 1。
func TestCLIWrongTokenEnvelope(t *testing.T) {
	startCLI(t)
	t.Setenv("FLEETLY_TOKEN", "flt_"+strings.Repeat("0", 48))
	code, _, errOut := runCLIConn(t, "apps", "list", "--json")
	if code != 1 || !strings.Contains(errOut, "invalid or revoked token") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// TestCLIRollbackNoTarget rollback 无可回滚目标：E_ROLLBACK_NO_TARGET 信封
// （注册码 + suggestion/docs 渲染），退出 1。
func TestCLIRollbackNoTarget(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")
	code, _, errOut := runCLIConn(t, "rollback", "--json", "my-api")
	if code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
	for _, want := range []string{"E_ROLLBACK_NO_TARGET", "suggestion:", "docs:"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// waitFor 周期轮询谓词直至通过或超时（日志管线首轮扫描的同步点）。
func waitFor(t *testing.T, budget time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("waitFor: predicate not satisfied in budget")
}
