package gitserver

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// DeployFromCommit 测试（webhook 入队点，幂等口径绑定断言面）：(app, sha)
// 去重恒开；来源字段落库；compose 名与仓库名不一致拒绝。

// isolateProcessTemp 把进程级临时根重定向到本测试私有目录（W3-S4 flake
// 诊治）。背景：MG-6 的中转目录回收断言（countComposeTempDirs）采样的是
// 进程全局系统 temp——全仓满载时并行的其他包测试进程在同一系统 temp 里
// 创建/回收同前缀（fleetly-compose-）中转目录，采样被外部进程污染：
// .w3out 三份全量日志同一签名「fleetly-compose-* dir count 2704 → 2705 /
// 2705 → 2707 / 2762 → 2769」（存量孤儿数千 + 并行包增量；单包隔离恒绿）。
// 重定向后（TMPDIR/TMP/TEMP 同设，覆盖 os.TempDir 的跨平台读取序）采样
// 空间仅含本进程目录，断言保持真实——产品代码 os.MkdirTemp("", ...) 每次
// 调用现取 os.TempDir()，回收语义未被放松。纯测试夹具层修复，产品不动。
func isolateProcessTemp(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
}

func TestDeployFromCommitEnqueue(t *testing.T) {
	requireGit(t)
	isolateProcessTemp(t)
	src, st, _, _ := newTestSource(t, 0)
	ctx := context.Background()

	sourceDir, sha := newSourceRepo(t, composeFixture)
	if _, _, err := src.EnsureBareRepo(ctx, "my-api"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sourceDir, "push", "--quiet", src.repoPath("my-api"), "main")

	// MG-6 回归前置采样：解析中转临时目录（os.MkdirTemp("fleetly-compose-")）
	// 必须随请求回收——结束时对照，不留孤儿 tmp。
	tmpBefore := countComposeTempDirs(t)

	// app 行经夹具项目预置（webhook 是既有 app 的触发通道——签名密钥随
	// app 行配置；未知 app 的诚实拒绝面见 TestDeployFromCommitRejections）。
	testsupport.SeedApp(t, st, "my-api")

	// 首次入队 → queued 部署 + 来源字段。
	rec, warnings, err := src.DeployFromCommit(ctx, "my-api", sha, "refs/heads/main")
	if err != nil {
		t.Fatalf("DeployFromCommit: %v", err)
	}
	if rec.Status != state.DeployQueued || rec.SourceGitSHA != sha || rec.SourceGitRef != "refs/heads/main" {
		t.Fatalf("record = %+v (warnings=%v)", rec, warnings)
	}

	// 审计与事件同事务落库（git.webhook_deploy / actor system；事件复用
	// deployment.queued）。
	audits, err := st.RecentAudits(ctx, 10)
	if err != nil || len(audits) == 0 {
		t.Fatalf("audits: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "git.webhook_deploy" && a.Result == "ok" && a.Actor == "system" && strings.Contains(a.DiffSummary, sha) {
			found = true
		}
	}
	if !found {
		t.Fatal("git.webhook_deploy audit missing")
	}

	// 同 (app, sha) 重投（webhook 幂等口径恒开）→ ErrDuplicateGitDeployment，
	// 不建新行。
	if _, _, err := src.DeployFromCommit(ctx, "my-api", sha, "refs/heads/main"); !errors.Is(err, state.ErrDuplicateGitDeployment) {
		t.Fatalf("second enqueue err = %v, want ErrDuplicateGitDeployment", err)
	}

	// MG-6 回归：入队的解析中转目录均已回收（defer os.RemoveAll——
	// 持久化副本在 <数据根>/deployments/<id>/compose.yaml，tmp 不是契约面）。
	if after := countComposeTempDirs(t); after != tmpBefore {
		t.Fatalf("parse scratch temp dirs not reclaimed: fleetly-compose-* dir count %d → %d", tmpBefore, after)
	}
}

// countComposeTempDirs 数系统 temp 里 fleetly-compose- 前缀目录数（MG-6
// 临时目录回收断言的采样点；只数前缀，不触碰其他测试的临时物）。
func countComposeTempDirs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fleetly-compose-") {
			n++
		}
	}
	return n
}

func TestDeployFromCommitRejections(t *testing.T) {
	requireGit(t)
	src, st, _, _ := newTestSource(t, 0)
	ctx := context.Background()

	// compose name ≠ 仓库名 → 拒绝（E_COMPOSE_UNSUPPORTED——errcode 零新增）。
	mismatch := strings.Replace(composeFixture, "my-api", "other-name", 1)
	sourceDir, sha := newSourceRepo(t, mismatch)
	if _, _, err := src.EnsureBareRepo(ctx, "my-api"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sourceDir, "push", "--quiet", src.repoPath("my-api"), "main")
	// app 行缺失先于 compose 校验暴露（防御分支：受理与执行间 app 被删）——
	// 用名字一致的夹具驱动到 app 行取用点。
	matchingDir, matchingSHA := newSourceRepo(t, composeFixture)
	gitRun(t, matchingDir, "push", "--quiet", src.repoPath("my-api"), "main", "--force")
	if _, _, err := src.DeployFromCommit(ctx, "my-api", matchingSHA, "refs/heads/main"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unknown app err = %v, want app-not-exist rejection", err)
	}
	testsupport.SeedApp(t, st, "my-api")
	_, _, err := src.DeployFromCommit(ctx, "my-api", sha, "refs/heads/main")
	var appErr *apperr.Error
	if err == nil || !errors.As(err, &appErr) || appErr.Code() != "E_COMPOSE_UNSUPPORTED" {
		t.Fatalf("name mismatch err = %v, want E_COMPOSE_UNSUPPORTED", err)
	}

	// 非法 sha / 非法 app 名 → 输入防御拒绝。
	if _, _, err := src.DeployFromCommit(ctx, "my-api", "zz", "refs/heads/main"); err == nil {
		t.Fatal("invalid sha accepted")
	}
	if _, _, err := src.DeployFromCommit(ctx, "../evil", sha, "refs/heads/main"); err == nil {
		t.Fatal("invalid app name accepted")
	}
}

// 分支过滤契约（H2 修复 / MG-C1）：投递非配置分支不建部署（哨兵
// ErrBranchNotTracked），配置分支（缺省回落 main + 显式配置分支）正常
// 入队——daemon 侧权威过滤， ServeHTTP 预查之外的第二道（纵深防御）。
func TestDeployFromCommitBranchFilter(t *testing.T) {
	requireGit(t)
	src, st, _, _ := newTestSource(t, 0)
	ctx := context.Background()

	sourceDir, sha1 := newSourceRepo(t, composeFixture)
	if _, _, err := src.EnsureBareRepo(ctx, "my-api"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sourceDir, "push", "--quiet", src.repoPath("my-api"), "main")

	// 首次形态（app 行不存在）：按缺省分支 main 比对——dev 哨兵拒止且
	// 零副作用（不建 app 行、不建部署行）。
	if _, _, err := src.DeployFromCommit(ctx, "my-api", sha1, "refs/heads/dev"); !errors.Is(err, ErrBranchNotTracked) {
		t.Fatalf("untracked branch (app missing) err = %v, want ErrBranchNotTracked", err)
	}
	if _, err := st.GetAppByName(ctx, "my-api"); !errors.Is(err, state.ErrAppNotFound) {
		t.Fatalf("filtered delivery created app row (err = %v)", err)
	}

	// 配置分支（缺省 main）正常入队；app 行经夹具项目预置。
	testsupport.SeedApp(t, st, "my-api")
	if _, _, err := src.DeployFromCommit(ctx, "my-api", sha1, "refs/heads/main"); err != nil {
		t.Fatalf("tracked branch delivery: %v", err)
	}
	appRow, err := st.GetAppByName(ctx, "my-api")
	if err != nil {
		t.Fatal(err)
	}

	// app 行在、分支缺省 main：dev 仍拒止，(app, sha) 部署计数不变。
	if _, _, err := src.DeployFromCommit(ctx, "my-api", sha1, "refs/heads/dev"); !errors.Is(err, ErrBranchNotTracked) {
		t.Fatalf("untracked branch (app exists) err = %v, want ErrBranchNotTracked", err)
	}
	n, err := st.CountGitDeploymentsForSHA(ctx, appRow.ID, sha1)
	if err != nil || n != 1 {
		t.Fatalf("deployments after filtered delivery = %d (err = %v), want 1", n, err)
	}

	// 显式配置分支 release：main 反被拒止；release 放行（新 commit sha2
	// ——同 sha 已被 (app, sha) 去重判据覆盖，分支无关）。
	setAppSource(t, st, appRow.ID, fileURL(sourceDir), "release")
	if _, _, err := src.DeployFromCommit(ctx, "my-api", sha1, "refs/heads/main"); !errors.Is(err, ErrBranchNotTracked) {
		t.Fatalf("main after reconfig err = %v, want ErrBranchNotTracked", err)
	}
	gitRun(t, sourceDir, "commit", "--quiet", "--allow-empty", "-m", "second")
	sha2 := gitRun(t, sourceDir, "rev-parse", "HEAD")
	gitRun(t, sourceDir, "push", "--quiet", src.repoPath("my-api"), "main")
	rec2, _, err := src.DeployFromCommit(ctx, "my-api", sha2, "refs/heads/release")
	if err != nil {
		t.Fatalf("reconfigured branch delivery: %v", err)
	}
	if rec2.SourceGitSHA != sha2 {
		t.Fatalf("release delivery record sha = %s, want %s", rec2.SourceGitSHA, sha2)
	}

	// 空引用不做特例容忍——webhook 载荷恒携带 ref，非 refs/heads/<branch>
	// 词形一律拒止（受理侧预查同词形，此处为纵深防御的第二道）。
	if _, _, err := src.DeployFromCommit(ctx, "my-api", sha2, ""); !errors.Is(err, ErrBranchNotTracked) {
		t.Fatalf("empty ref err = %v, want ErrBranchNotTracked", err)
	}
}
