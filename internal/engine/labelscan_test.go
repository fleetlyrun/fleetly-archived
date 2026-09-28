package engine

// 归属过滤的源码扫描守卫（IMPL-ARCH-A，safecall_test.go 同款 idiom）：
// internal/** 的非测试 .go 文件不得再手写 `state.LabelApp:` 的过滤 map 字
// 面量——「一个 app 的受管服务如何按 label 圈定」在 ownership.go 单点产出
// （qualifiedServiceFilter/appServiceFilter）。白名单外的手写构造（裸名或
// 二次推导）即本测试红——W2-S3 漂移 extras 死腿（drift.go 曾以裸 app.Name
// 过滤、测试夹具同形掩护）的守卫化复现。
//
// 只扫**写面**（map 字面量键 `state.LabelApp:`）；读面（`Labels[state.
// LabelApp]` 索引、错误文案引用）语义各异且不构造归属作用域，天然不命中
// 模式，不在管辖内。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// appLabelFilterWhitelist 是允许出现 `state.LabelApp:` 构造的文件（相对
// internal/ 的斜杠路径）与一行理由。白名单必须随代码演进收缩/修正——某文
// 件不再命中模式时本测试会提示清理（清单与代码双向钉死）。
var appLabelFilterWhitelist = map[string]string{
	"engine/ownership.go":    "归属 module 本体：过滤 map 的唯一产出点（qualifiedServiceFilter）",
	"engine/engine.go":       "applyDesired 省略=删除作用域：label 值自期望 spec 自推导（W2-S3 重放安全决策，票面禁止改动）",
	"engine/planner.go":      "期望 spec 的 label 写方（规划路径，值经 naming.QualifiedName 三段限定形）",
	"engine/jobrun.go":       "一次性 job 服务 label 写方（JobSpecFrom 从模板收敛最小集，非归属过滤）",
	"engine/configinject.go": "Swarm config 对象的归属选择器（对象族不同，非服务过滤）",
	"engine/secretinject.go": "Swarm secret 对象的归属选择器（对象族不同，非服务过滤）",
	"naming/labels.go":       "平台 label 唯一写方（Marker 端口契约：ServiceLabels/ContainerLabels）",
	"substrate/logs.go":      "日志采集的 per-app 服务发现（ManagedServiceProcesses 含 job 不豁免 / JobServiceStates 仅 job——方向相反的发现面，非对账域语义；入参限定形字符串）",
}

// TestNoHandWrittenAppLabelFilters 扫描 internal/** 非 _test.go 文件中的
// `state.LabelApp:` map 字面量构造：白名单外一律失败（手写归属过滤 =
// 漂移 extras 死腿的复发形态）。
func TestNoHandWrittenAppLabelFilters(t *testing.T) {
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
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// 行尾归一到 LF：Windows 检出（core.autocrlf=true）把磁盘文件写成
		// CRLF——行尾敏感的判定不归一则结果随检出环境漂移（safecall_test.go
		// 同款教训）。
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		rel, rerr := filepath.Rel(internalRoot, path)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)
		for i, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, "state.LabelApp:") {
				continue
			}
			whitelistHits[relSlash] = true
			if _, allowed := appLabelFilterWhitelist[relSlash]; !allowed {
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
		t.Errorf("hand-written fleetly.app filter map in %s (ownership filters are produced only by engine/ownership.go; see IMPL-ARCH-A):", file)
		for _, l := range lines {
			t.Errorf("  %s", l)
		}
	}
	// 白名单保鲜：条目不再命中模式即提示收缩（清单与代码一致纪律）。
	for file, reason := range appLabelFilterWhitelist {
		if !whitelistHits[file] {
			t.Errorf("whitelist entry %s no longer matches (remove it or fix the reason: %s)", file, reason)
		}
	}
}
