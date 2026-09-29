package state

// 任务状态写的源码扫描守卫（IMPL-ARCH-C2，labelscan_test.go 同款 idiom）：
// internal/** 的非测试 .go 文件不得再出现单写点之外的
// `UPDATE tasks SET status` 裸写——任务状态写的唯一站点是 state/tasks.go
// 的 updateTask 内核（Status 写必须携带 PrevStatus，CAS 前按 machine.go
// taskTransitions 校验）。白名单外的裸写即本测试红——deployments 表已由
// updateDeployment 结构性封死的缺陷类（M3-2：裸状态写绕过转移表校验与
// CAS 竞争保护）在 tasks 表的守卫化复现。
//
// 只扫状态写（`UPDATE tasks SET status`）；行删除（DELETE FROM tasks：
// DeleteTaskRow 的墓碑出口 / PruneTerminalTasksOlderThan 的保留期回收）与
// 受理插入（INSERT）不是状态转移，不在管辖（tasks 表写点盘点见票面）。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// taskStatusWriteWhitelist 是允许出现 `UPDATE tasks SET status` 的文件
// （相对 internal/ 的斜杠路径）与一行理由。白名单必须随代码演进收缩/修正
// ——某文件不再命中模式时本测试会提示清理（清单与代码双向钉死）。
var taskStatusWriteWhitelist = map[string]string{
	"state/tasks.go": "任务状态单写点内核 updateTask（IMPL-ARCH-C2）：Status 写必带 PrevStatus、CAS 前按 taskTransitions 校验，六个意图原语共享",
}

// TestNoBareTaskStatusWrites 扫描 internal/** 非 _test.go 文件中的
// `UPDATE tasks SET status` 裸写：白名单外一律失败（任务状态写必须经
// state/tasks.go 的 updateTask 单点——转移表校验与 PrevStatus 纪律不可绕过）。
func TestNoBareTaskStatusWrites(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	internalRoot := filepath.Dir(filepath.Dir(thisFile)) // <repo>/internal

	violations := map[string][]string{} // 相对路径 → 命中行
	whitelistHits := map[string]bool{}
	walkErr := filepath.WalkDir(internalRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // G304：扫描本仓源码树，路径自 WalkDir
		if rerr != nil {
			return rerr
		}
		// 行尾归一到 LF：Windows 检出（core.autocrlf=true）把磁盘文件写成
		// CRLF——行尾敏感的判定不归一则结果随检出环境漂移（labelscan_test.go
		// 同款教训）。
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		rel, rerr := filepath.Rel(internalRoot, path)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)
		for i, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, "UPDATE tasks SET status") {
				continue
			}
			whitelistHits[relSlash] = true
			if _, allowed := taskStatusWriteWhitelist[relSlash]; !allowed {
				violations[relSlash] = append(violations[relSlash],
					strings.TrimSpace(line)+fmt.Sprintf(" (line %d)", i+1))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk internal sources: %v", walkErr)
	}

	for file, lines := range violations {
		t.Errorf("bare tasks status write in %s (task status writes go only through state/tasks.go updateTask; see IMPL-ARCH-C2):", file)
		for _, l := range lines {
			t.Errorf("  %s", l)
		}
	}
	// 白名单保鲜：条目不再命中模式即提示收缩（清单与代码一致纪律）。
	for file, reason := range taskStatusWriteWhitelist {
		if !whitelistHits[file] {
			t.Errorf("whitelist entry %s no longer matches (remove it or fix the reason: %s)", file, reason)
		}
	}
}
