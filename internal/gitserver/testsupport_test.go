package gitserver

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 测试支撑：真实 git（宿主 git 为前置条件——缺失则 skip，CI 的 linux 无
// git 环境不误报）；compose 夹具仓库的构造与对象投递。

// requireGit 跳过无 git 环境（gitserver 的仓库面测试 exec 系统 git）。
func requireGit(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skipf("host git not available: %v", err)
	}
}

// gitRun 在 dir 下执行 git 子命令（测试夹具专用；作者/提交者身份固定）。
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204：测试夹具固定 git 词形
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=fleetly", "GIT_AUTHOR_EMAIL=fleetly@test",
		"GIT_COMMITTER_NAME=fleetly", "GIT_COMMITTER_EMAIL=fleetly@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newSourceRepo 建一个含 compose.yaml 的工作仓库（branch=main），返回
// (仓库目录, HEAD sha)。
func newSourceRepo(t *testing.T, composeYAML string) (string, string) {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeYAML), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "--quiet", "-m", "init")
	sha := gitRun(t, dir, "rev-parse", "HEAD")
	return dir, sha
}

// composeFixture 是最小合法 compose（nginx image 模式，与 CLI golden 夹具
// 同形态）。
const composeFixture = `
name: my-api
services:
  web:
    image: nginx:1.27-alpine
    expose: ["80"]
`

// newTestSource 构造测试用 GitTriggers（真实 store + box；Root 在临时目录）。
// replayTTL ≤ 0 时用配置默认（Normalize 回落 15 分钟）。
func newTestSource(t *testing.T, replayTTL time.Duration) (*GitTriggers, *state.Store, *secrets.Box, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	cfg := Config{
		Root:      filepath.Join(dir, "git"),
		ReplayTTL: replayTTL,
	}
	src := NewGitTriggers(cfg, st, box, testLogger())
	return src, st, box, dir
}

// fileURL 把本地仓库目录转成 file:/// URL（整改②白名单放行的本地裸仓库
// 形态；跨平台：Windows 盘符路径 C:/x → file:///C:/x，POSIX /tmp/x →
// file:///tmp/x）。
func fileURL(dir string) string {
	u := filepath.ToSlash(dir)
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return "file://" + u
}

// testLogger 是测试侧静默日志（与生产装配解耦）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
