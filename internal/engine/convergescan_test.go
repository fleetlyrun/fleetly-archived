package engine

// 底座服务写面的源码扫描守卫（IMPL-ARCH-B，labelscan_test.go 同款 idiom）：
// internal/engine 的非测试 .go 文件不得再出现收敛原语之外的
// `e.sub.ServiceCreate(` / `e.sub.ServiceUpdate(` 直调——「缺失→建；期望哈
// 希不符→全量重申」的收敛 switch 与哈希标戳在 converge.go 单点产出。白名
// 单外的直调即本测试红（任务线无差别重申的守卫化复现：绕过原语 = 绕过哈
// 希捷径与标戳，收敛幂等语义退化）。
//
// 只扫 engine 包：原语住在引擎；其余包（database/execrelay/ingress/metrics/
// rustfs/victorialogs/cron）各自持有独立的 docker 写端口与自有服务的收敛
// 职责，不在本原语管辖（同构收敛诉求由各自票面裁量）。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// serviceWritePatterns 是受管直调模式（引擎包内底座服务写面的唯一形态；
// 接收者限定 e.sub——NetworkSubstrate 走 e.netSub，不涉服务写）。
var serviceWritePatterns = []string{"e.sub.ServiceCreate(", "e.sub.ServiceUpdate("}

// serviceWriteWhitelist 是允许直调底座服务写的文件（相对 internal/engine/
// 的文件名）与一行理由。白名单必须随代码演进收缩/修正——某文件不再命中
// 任一模式时本测试会提示清理（清单与代码双向钉死）。
var serviceWriteWhitelist = map[string]string{
	"converge.go":    "收敛原语本体：缺失建/哈希不符重申的唯一 switch 与哈希标戳单点（IMPL-ARCH-B）",
	"engine.go":      "scaleToZero 保留现场写通道（D-REL-5：首发失败/取消的无条件副本清零，非缺失/漂移对账，不适用收敛语义）",
	"autoscaling.go": "伸缩写通道（按评估结果无条件写副本 + 归属/哈希 label 同形重锚；事件性调容非收敛对账）",
	"initjobs.go":    "一次性 init job 相位的缺失即建（inspect-skip 相位幂等；运行中 job 永不重申——生命周期归 init 相位，与对账原语的保全语义不同）",
}

// TestNoConvergenceOutsidePrimitive 扫描 internal/engine 非测试 .go 文件中
// 的 `e.sub.ServiceCreate(` / `e.sub.ServiceUpdate(` 直调：白名单外一律失
// 败（收敛必须经 converge.go 原语——哈希标戳与幂等捷径不可绕过）。
func TestNoConvergenceOutsidePrimitive(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	engineRoot := filepath.Dir(thisFile) // <repo>/internal/engine

	violations := map[string][]string{} // 文件名 → 命中行
	whitelistHits := map[string]bool{}
	walkErr := filepath.WalkDir(engineRoot, func(path string, d os.DirEntry, err error) error {
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
		// CRLF——判定不归一则结果随检出环境漂移（labelscan_test.go 同款教训）。
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		name := filepath.Base(path)
		for i, line := range strings.Split(src, "\n") {
			matched := false
			for _, pattern := range serviceWritePatterns {
				if strings.Contains(line, pattern) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			whitelistHits[name] = true
			if _, allowed := serviceWriteWhitelist[name]; !allowed {
				violations[name] = append(violations[name],
					strings.TrimSpace(line)+fmt.Sprintf(" (line %d)", i+1))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk engine sources: %v", walkErr)
	}

	for file, lines := range violations {
		t.Errorf("direct substrate service write in %s (service convergence goes only through engine/converge.go; see IMPL-ARCH-B):", file)
		for _, l := range lines {
			t.Errorf("  %s", l)
		}
	}
	// 白名单保鲜：条目不再命中任一模式即提示收缩（清单与代码一致纪律）。
	for file, reason := range serviceWriteWhitelist {
		if !whitelistHits[file] {
			t.Errorf("whitelist entry %s no longer matches (remove it or fix the reason: %s)", file, reason)
		}
	}
}
