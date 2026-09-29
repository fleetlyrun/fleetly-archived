package gitserver

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// bare 仓库管理与 git 对象库读取（全部 exec 系统 git——宿主 git 为前置
// 条件；无 shell 参与，参数白名单形态注入）。

// appNamePattern 是 app 名严格校验（DNS 类词形；点与路径分隔符不允许
// ——仓库路径 = <root>/<app>.git，校验即路径注入防线）。
var appNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// shaPattern 是 git commit 的严格校验（40 位十六进制）。
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidAppName 报告 app 名是否可用于 bare 仓库路径。
func ValidAppName(name string) bool { return appNamePattern.MatchString(name) }

// ValidSHA 报告 commit 是否为 40 位十六进制。
func ValidSHA(sha string) bool { return shaPattern.MatchString(sha) }

// repoPath 返回 app 的 bare 仓库路径（app 名已经 ValidAppName 校验）。
func (s *GitTriggers) repoPath(app string) string {
	return filepath.Join(s.cfg.Root, app+".git")
}

// EnsureBareRepo 懒创建 app 的 bare 仓库（webhook 拉源的 fetch 落点与
// compose 字节的读取源——fetch.go 把 source fetch 进它，deployFromCommit
// 经 gitShow 从它取 compose）。
//
// 幂等；返回仓库路径与是否发生了创建。E7③（S19）：全程持 per-app 互斥
// （stat 判定与 init 之间的 TOCTOU 收口——并发同 app 的 webhook 拉源
// 交错不再产生半初始化仓库）。
func (s *GitTriggers) EnsureBareRepo(ctx context.Context, app string) (string, bool, error) {
	if !ValidAppName(app) {
		return "", false, fmt.Errorf("gitserver: invalid app name %q", app)
	}
	unlock := s.lockRepo(app)
	defer unlock()
	if err := os.MkdirAll(s.cfg.Root, 0o750); err != nil {
		return "", false, fmt.Errorf("gitserver: create git root %s: %w", s.cfg.Root, err)
	}
	path := s.repoPath(app)
	_, statErr := os.Stat(filepath.Join(path, "HEAD"))
	switch {
	case os.IsNotExist(statErr):
		if out, err := execGit(ctx, "", "init", "--bare", "--quiet", path); err != nil {
			return "", false, fmt.Errorf("gitserver: git init --bare %s: %w (%s)", path, err, out)
		}
		return path, true, nil
	case statErr != nil:
		return "", false, fmt.Errorf("gitserver: stat repo %s: %w", path, statErr)
	}
	return path, false, nil
}

// lockRepo 取 app 的仓库写入互斥并加锁（E7③）：返回解锁函数；map 自身
// 由 repoMu 保护——分段锁不放大跨 app 的并发代价。
func (s *GitTriggers) lockRepo(app string) func() {
	s.repoMu.Lock()
	mu, ok := s.repoLocks[app]
	if !ok {
		mu = &sync.Mutex{}
		s.repoLocks[app] = mu
	}
	s.repoMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// composeFromCommit 从 bare 仓库读指定 commit 的 compose 文件：约定在仓库
// 根的 compose.yaml / compose.yml——两者都在 → 拒绝（歧义信封，compose
// 族）；都缺 → 拒绝。compose 真源在 git 对象库，不信任客户端传字节。
func (s *GitTriggers) composeFromCommit(ctx context.Context, app, sha string) ([]byte, error) {
	path := s.repoPath(app)
	yamlBytes, yamlErr := gitShow(ctx, path, sha, "compose.yaml")
	ymlBytes, ymlErr := gitShow(ctx, path, sha, "compose.yml")
	switch {
	case yamlErr == nil && ymlErr == nil:
		return nil, fmt.Errorf("gitserver: %w: app %s at %s has both compose.yaml and compose.yml (ambiguous; refusing to deploy)",
			errComposeRejected, app, sha)
	case yamlErr == nil:
		return yamlBytes, nil
	case ymlErr == nil:
		return ymlBytes, nil
	default:
		return nil, fmt.Errorf("gitserver: %w: app %s at %s: no compose.yaml / compose.yml at the repo root",
			errComposeRejected, app, sha)
	}
}

// errComposeRejected 是 compose 读取失败的包内哨兵（webhook 层映射为
// E_COMPOSE_UNSUPPORTED——errcode 零新增，复用既有 compose 码族）。
var errComposeRejected = fmt.Errorf("compose unavailable")

// gitShow 执行 git show <sha>:<file>（cwd = bare 仓库）。
func gitShow(ctx context.Context, repoPath, sha, file string) ([]byte, error) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "show", sha+":"+file) //nolint:gosec // G204：参数为校验后的常量词形（sha 40hex + 固定文件名）
	cmd.Dir = repoPath
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show %s:%s: %s", sha, file, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// execGit 执行系统 git（cwd 可空）。
func execGit(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204：参数为包内常量白名单词形
	if cwd != "" {
		cmd.Dir = cwd
	}
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()) + strings.TrimSpace(errOut.String()),
			fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}
