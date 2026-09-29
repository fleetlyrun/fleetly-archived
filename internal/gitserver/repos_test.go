package gitserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 仓库面测试：app 名校验、bare 仓库懒创建、compose 双文件拒绝。

func TestValidAppName(t *testing.T) {
	for _, name := range []string{"my-api", "web", "a", "a-1-b", "api123"} {
		if !ValidAppName(name) {
			t.Errorf("ValidAppName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "-lead", "Up", "with.dot", "a/b", "a\\b", "..", "with space", "has_underscore"} {
		if ValidAppName(name) {
			t.Errorf("ValidAppName(%q) = true, want false", name)
		}
	}
}

func TestEnsureBareRepoAndCompose(t *testing.T) {
	requireGit(t)
	src, _, _, _ := newTestSource(t, 0)
	ctx := context.Background()

	// 首次：懒创建。
	path, created, err := src.EnsureBareRepo(ctx, "my-api")
	if err != nil || !created {
		t.Fatalf("EnsureBareRepo: created=%v err=%v", created, err)
	}
	if fi, err := os.Stat(filepath.Join(path, "HEAD")); err != nil || fi.IsDir() {
		t.Fatalf("bare repo not initialized at %s", path)
	}

	// 二次：幂等（不重建）。
	_, created2, err := src.EnsureBareRepo(ctx, "my-api")
	if err != nil || created2 {
		t.Fatalf("second EnsureBareRepo: created=%v err=%v", created2, err)
	}

	// composeFromCommit 前置：把含 compose 的 commit 送进 bare 仓库。
	sourceDir, sha := newSourceRepo(t, composeFixture)
	gitRun(t, sourceDir, "push", "--quiet", src.repoPath("my-api"), "main")
	got, err := src.composeFromCommit(ctx, "my-api", sha)
	if err != nil || !strings.Contains(string(got), "nginx:1.27-alpine") {
		t.Fatalf("composeFromCommit: %v (%s)", err, got)
	}
}

// TestEnsureBareRepoConcurrentInit E7③（S19）：并发 EnsureBareRepo 同 app
// ——per-app 互斥下 stat 判定与 init 的 TOCTOU 收口：恰一次创建、全部成
// 功返回（无半初始化仓库）。
func TestEnsureBareRepoConcurrentInit(t *testing.T) {
	requireGit(t)
	src, _, _, _ := newTestSource(t, 0)
	ctx := context.Background()

	const k = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	created := make([]bool, k)
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			path, c, err := src.EnsureBareRepo(ctx, "race-app")
			if err != nil {
				t.Errorf("concurrent EnsureBareRepo: %v", err)
				return
			}
			if fi, statErr := os.Stat(filepath.Join(path, "HEAD")); statErr != nil || fi.IsDir() {
				t.Errorf("repo not initialized after EnsureBareRepo: %v", statErr)
			}
			created[i] = c
		}(i)
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		t.Fatal("concurrent EnsureBareRepo observed init race (E7③ regression)")
	}
	inits := 0
	for _, c := range created {
		if c {
			inits++
		}
	}
	if inits != 1 {
		t.Fatalf("concurrent EnsureBareRepo created %d repos, want exactly 1", inits)
	}
}

func TestComposeFromCommit(t *testing.T) {
	requireGit(t)
	src, _, _, _ := newTestSource(t, 0)
	ctx := context.Background()

	// 正常：单 compose.yaml。
	sourceDir, sha := newSourceRepo(t, composeFixture)
	if _, _, err := src.EnsureBareRepo(ctx, "my-api"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, sourceDir, "push", "--quiet", src.repoPath("my-api"), "main")
	got, err := src.composeFromCommit(ctx, "my-api", sha)
	if err != nil || !strings.Contains(string(got), "nginx:1.27-alpine") {
		t.Fatalf("composeFromCommit: %v (%s)", err, got)
	}

	// 双文件 → 拒绝。
	dir2, _ := newSourceRepo(t, composeFixture)
	if err := os.WriteFile(filepath.Join(dir2, "compose.yml"), []byte(composeFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir2, "add", ".")
	gitRun(t, dir2, "commit", "--quiet", "--amend", "--no-edit")
	shaBoth := gitRun(t, dir2, "rev-parse", "HEAD")
	if _, _, err := src.EnsureBareRepo(ctx, "my-api"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir2, "push", "--quiet", src.repoPath("my-api"), "main", "--force")
	_, err = src.composeFromCommit(ctx, "my-api", shaBoth)
	if err == nil || !errors.Is(err, errComposeRejected) {
		t.Fatalf("both-files: err=%v, want errComposeRejected", err)
	}

	// 无 compose → 拒绝。
	dir3 := t.TempDir()
	gitRun(t, dir3, "init", "--initial-branch=main", "--quiet")
	if err := os.WriteFile(filepath.Join(dir3, "README.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir3, "add", ".")
	gitRun(t, dir3, "commit", "--quiet", "-m", "no compose")
	sha3 := gitRun(t, dir3, "rev-parse", "HEAD")
	gitRun(t, dir3, "push", "--quiet", src.repoPath("my-api"), "main", "--force")
	_, err = src.composeFromCommit(ctx, "my-api", sha3)
	if err == nil || !errors.Is(err, errComposeRejected) {
		t.Fatalf("no-compose: err=%v, want errComposeRejected", err)
	}

	// 形态防御：非法 sha 拒绝。
	if _, err := src.composeFromCommit(ctx, "my-api", "not-a-sha"); err == nil {
		t.Fatal("invalid sha accepted")
	}
}
