package engine

// 活跃实施档的测试名引用存在性守卫（IMPL-ARCH-M，裁决四：③类「记录虚报」
// 守卫）。机制缺口排查把「记录虚报」判为第三元型：T1-1 守卫表声称
// TestPlatformSettingsMigrationUpDown 覆盖 00023 Down 腿、T2-1 实施记录声称
// 锚豁免已存在——两次实证都是「文档声称某测试存在，实际不存在」。本守卫扫
// docs/plan/ 活跃实施档中引用的 Go 测试函数名，断言仓内存在其 `func TestXxx(`
// / `func BenchmarkXxx(` 定义——只堵「引用不存在的测试」这一半；「存在但没
// 覆盖声称的行为」本守卫管不到（诚实挂账）。
//
// 活跃波次判据（2026-09-29 用户裁决：方案 a——只扫活跃波次，拒绝全量 +
// 白名单，因历史档引用后来改名/删除的测试必然误报腐烂）：
//   - 以「目录内最新档的文件名日期」为锚、回看 activeImplDocWindowDays 个
//     自然日窗内的全部日期档。锚定目录状态而非墙钟：守卫必须密闭，不随日历
//     推移空转；最新档前进时旧档自动滚出窗口。
//   - 判据失效形态（有意为之）：波次跨度超过窗宽时，较早的档滚出窗口 = 漏报
//     盲区。防误报优先于防漏报——漏报的代价只是守卫盲区（由周期性全量评审
//     兜底），误报的代价是守卫被禁用。
//
// 非现行主张的跳过规则（全部机械可判，来源 = 档案自身的既有行文约定；跳过
// 即「该引用不主张本仓当前存在此测试」，非现行主张不在管辖内）：
//   1. 族形通配：名字后紧跟 `*` 或 `_*`（如 `TestRedisRegistry_*`、
//      `TestReplaceAppDomains*`——族指称或已删用例的除名披露）。
//   2. -run/-bench 模式位：名字紧跟在 `-run `/`-run=`/`-bench `/`-bench=`
//      之后——go test 语义是无锚定正则前缀匹配，不要求同名词面存在。
//   3. 勘误/除名披露行：行内含 勘误/更名/历史锚/保留备查/同步删除（T1-1 勘
//      误「原文保留备查」、DB-1 更名「旧名保留为历史锚」等既有披露纪律）。
//      代价：勘误行上若引入新幻影名会被放过 = 漏报，按判据同一取舍接受。
//   4. 外仓票据小节：IMPL 票号在票面标题标注了施工仓库 torchwood/messageloop
//      （不区分大小写）者，该票号名下全部小节（票面、审查、实施记录及其
//      #### 子小节——无票号标题不切语境）按外仓语境处理——T 线在外仓施工的票
//      引用外仓测试套件，本仓无从核验。
//   5. 围栏代码块（``` 围栏内）：命令与原始输出 = 执行实录（历史事实，含
//      `-run` 模式、`=== RUN` 行、外仓 `go mod why` 图），非现行主张；围栏内
//      的 `#` 行也不是标题（外仓依赖图输出以 # 开头，不能据此切小节）。
//
// 白名单纪律：跳过规则之外仍被合法引用但确实不在仓内的名字（历史锚、示例
// 占位）逐条入 absentTestNameWhitelist 且必须带理由；双向保鲜——条目不再被
// 活跃档引用即提示清除，条目对应的测试若真实出现也提示清除（白名单语义 =
// 「合法缺席」而非「豁免一切」）。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// activeImplDocWindowDays 是活跃窗的自然日宽度。取 3：当前活跃波次 =
// 2026-09-26-torchwood-line-impl.md（09-26）与 2026-09-28-architecture-
// remediation.md（09-28）两档，窗 [最新-2, 最新] 恰好同窗；裁决依据见文件头。
const activeImplDocWindowDays = 3

// absentTestNameWhitelist：跳过规则之外、活跃档中合法引用但仓内确实不存在的
// 测试名 → 一行理由。语义 =「合法缺席」：条目必须仍被活跃档引用，且必须继续
// 不存在于仓内（双向保鲜，见文件头）。
var absentTestNameWhitelist = map[string]string{
	"TestManualDbtoolsPostgresMultiMajor": "IMPL-DB-0 真机探针原名；IMPL-DB-1 更名为 TestManualDbtoolsToolFaces 并明文「DB-0 记录中的旧名保留为历史锚」——DB-0/DB-1 档内 9 处引用是合法历史锚，非现行主张",
	"TestXxx":                             "remediation 档 §4 挂账表描述本守卫时的示例占位名（「TestXxx 名存在性扫描」），非真实引用",
}

var (
	// testReferencePattern 匹配文档中的 Go 测试名引用（Test/Benchmark 后随
	// 大写字母开头的标识符段；下划线不计入——子测试后缀由跳过规则 1 处理）。
	testReferencePattern = regexp.MustCompile(`\b(?:Test|Benchmark)[A-Z][A-Za-z0-9]*`)
	// testFuncDeclPattern 匹配仓内 *.go 的顶层测试函数声明（方法形态的
	// TestXxx 不是 go test 可执行测试，不计）。
	testFuncDeclPattern = regexp.MustCompile(`(?m)^func ((?:Test|Benchmark)[A-Z][A-Za-z0-9]*)\(`)
	// datedDocNamePattern 提取档案文件名前缀日期（YYYY-MM-DD-*.md）。
	datedDocNamePattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})-.*\.md$`)
	// runPatternPosition 检查引用名前的紧邻前缀是否为 -run/-bench 模式位。
	runPatternPosition = regexp.MustCompile(`(?:-run|-bench)[ =]$`)
	// erratumMarkers 是行内勘误/除名披露词（跳过规则 3）。
	erratumMarkers = []string{"勘误", "更名", "历史锚", "保留备查", "同步删除"}
	// ticketIDPattern 提取标题行中的 IMPL 票号（IMPL-T2-3 / IMPL-ARCH-F1 /
	// IMPL-DB-0 / IMPL-T15-1 等形态）；同一票号的票面标题与审查/实施记录标题
	// 共享票号，外仓标注据此继承（跳过规则 4）。
	ticketIDPattern = regexp.MustCompile(`IMPL-[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*`)
	// foreignRepoWordPattern 检查票据标题是否标注了外仓施工仓库。
	foreignRepoWordPattern = regexp.MustCompile(`(?i)torchwood|messageloop`)
)

// docTestReference 是档案中一处测试名引用的定位（斜杠相对路径 + 行号）。
type docTestReference struct {
	file string
	line int
	name string
}

// TestActiveImplDocsReferenceExistingTests 断言：活跃实施档中以现行主张口吻
// 引用的每个测试名，在仓内 *.go 存在顶层 `func TestXxx(`/`func BenchmarkXxx(`
// 声明；失败点名档案 file:line 与缺失名。
func TestActiveImplDocsReferenceExistingTests(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // <repo>
	docsPlanDir := filepath.Join(repoRoot, "docs", "plan")

	selectedDocs := selectActiveImplDocs(t, docsPlanDir)

	// 收集活跃档内的测试名引用（先全量，跳过规则在逐行扫描时套用）。
	var refs []docTestReference // 跳过规则后的现行主张引用
	rawRefCount := 0
	skipWildcard, skipRunPattern, skipErratum, skipForeignRepo, skipFence := 0, 0, 0, 0, 0
	foreignByTicket := map[string]bool{} // 票号 → 是否外仓施工票（规则 4）
	inForeignSection := false
	inFence := false
	for _, doc := range selectedDocs {
		raw, err := os.ReadFile(filepath.Join(docsPlanDir, filepath.FromSlash(doc.name))) //nolint:gosec // G304：读 docs/plan 在册文档，路径自清单
		if err != nil {
			t.Fatalf("read %s: %v", doc.path, err)
		}
		// 行尾归一到 LF：Windows 检出（core.autocrlf=true）不应影响判定
		// 与行号（labelscan_test.go 同款教训）。
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		for i, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence // 围栏开合（规则 5）
				continue
			}
			if inFence {
				hits := len(testReferencePattern.FindAllStringIndex(line, -1))
				rawRefCount += hits
				skipFence += hits
				continue
			}
			if strings.HasPrefix(line, "#") {
				// 标题行刷新外仓语境：有票号 → 继承/登记该票的外仓标注；
				// 无票号（#### 子小节、结构编号节）不切语境——继承所属票
				// （审查节以 #### 分小节，外语境随票号贯通）。
				if id := ticketIDPattern.FindString(line); id != "" {
					if foreignRepoWordPattern.MatchString(line) {
						foreignByTicket[id] = true
					}
					inForeignSection = foreignByTicket[id]
				}
			}
			for _, loc := range testReferencePattern.FindAllStringIndex(line, -1) {
				name := line[loc[0]:loc[1]]
				rawRefCount++
				switch {
				case strings.HasPrefix(line[loc[1]:], "*") || strings.HasPrefix(line[loc[1]:], "_*"):
					skipWildcard++ // 规则 1：族形通配
				case runPatternPosition.MatchString(line[:loc[0]]):
					skipRunPattern++ // 规则 2：-run/-bench 前缀模式位
				case containsAny(line, erratumMarkers):
					skipErratum++ // 规则 3：勘误/除名披露行
				case inForeignSection:
					skipForeignRepo++ // 规则 4：外仓小节
				default:
					refs = append(refs, docTestReference{file: doc.path, line: i + 1, name: name})
				}
			}
		}
	}
	// 防空转：活跃档扫到 0 个测试名引用 = 模式失配（判据或文档形态变了，
	// 守卫已静默盲化），宁可快红。
	if rawRefCount == 0 {
		t.Fatalf("no test-name references found in active impl docs (%v) — guard model out of date, investigate before trusting green",
			docPaths(selectedDocs))
	}

	defined := collectRepoTestFuncNames(t, repoRoot)

	var missing []string
	whitelistHits := map[string]bool{}
	for _, ref := range refs {
		if _, absent := absentTestNameWhitelist[ref.name]; absent {
			whitelistHits[ref.name] = true
			continue
		}
		if !defined[ref.name] {
			missing = append(missing, fmt.Sprintf("%s:%d: references test %q but no `func %s(` exists in the repository (IMPL-ARCH-M; if the mention is a historical anchor or an exemplary placeholder, add it to absentTestNameWhitelist with a reason)",
				ref.file, ref.line, ref.name, ref.name))
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Error(m)
	}

	// 白名单双向保鲜（清单与档案/代码一致纪律）。
	for _, name := range sortedWhitelistNames() {
		if !whitelistHits[name] {
			t.Errorf("whitelist entry %q is no longer referenced by any active impl doc (remove it or fix the reason: %s)", name, absentTestNameWhitelist[name])
		}
		if defined[name] {
			t.Errorf("whitelist entry %q now exists as a real test function (the whitelist means legitimately absent; remove the entry: %s)", name, absentTestNameWhitelist[name])
		}
	}

	t.Logf("active impl docs: %v; raw references: %d (skipped: wildcard %d, run-pattern %d, erratum %d, foreign-repo %d, fence %d); current-claim references checked: %d; whitelist: %d",
		docPaths(selectedDocs), rawRefCount, skipWildcard, skipRunPattern, skipErratum, skipForeignRepo, skipFence, len(refs), len(absentTestNameWhitelist))
}

// activeDoc 是一个活跃实施档：name = 目录内文件名；path = 报告用相对路径。
type activeDoc struct {
	name string
	path string
}

// selectActiveImplDocs 按「目录内最新档日期回看 activeImplDocWindowDays 自然
// 日」选出活跃实施档（文件名排序保证输出确定）。
func selectActiveImplDocs(t *testing.T, docsPlanDir string) []activeDoc {
	t.Helper()
	entries, err := os.ReadDir(docsPlanDir)
	if err != nil {
		t.Fatalf("read docs/plan: %v", err)
	}
	var newest time.Time
	type datedDoc struct {
		name string
		date time.Time
	}
	var dated []datedDoc
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := datedDocNamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue // 无日期前缀的档案不是实施档，天然不在活跃窗
		}
		day, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3])
		if err != nil {
			t.Fatalf("parse date in %s: %v", e.Name(), err)
		}
		dated = append(dated, datedDoc{name: e.Name(), date: day})
		if day.After(newest) {
			newest = day
		}
	}
	if newest.IsZero() {
		t.Fatal("no dated docs under docs/plan — guard model out of date")
	}
	windowStart := newest.AddDate(0, 0, -(activeImplDocWindowDays - 1))
	var selected []activeDoc
	for _, d := range dated {
		if !d.date.Before(windowStart) {
			selected = append(selected, activeDoc{name: d.name, path: "docs/plan/" + d.name})
		}
	}
	if len(selected) == 0 {
		t.Fatal("active window selected no docs — guard model out of date")
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].name < selected[j].name })
	return selected
}

// collectRepoTestFuncNames 遍历仓内 *.go（含 _test.go）收集顶层测试函数名。
func collectRepoTestFuncNames(t *testing.T, repoRoot string) map[string]bool {
	t.Helper()
	defined := map[string]bool{}
	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" {
				return filepath.SkipDir // 版本库/依赖/IDE 目录无仓内测试源
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G304：扫描本仓源码树，路径自 WalkDir
		if err != nil {
			return err
		}
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		for _, m := range testFuncDeclPattern.FindAllStringSubmatch(src, -1) {
			defined[m[1]] = true
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repository sources: %v", walkErr)
	}
	return defined
}

func containsAny(line string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}

func sortedWhitelistNames() []string {
	names := make([]string, 0, len(absentTestNameWhitelist))
	for name := range absentTestNameWhitelist {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func docPaths(docs []activeDoc) string {
	quoted := make([]string, 0, len(docs))
	for _, d := range docs {
		quoted = append(quoted, strconv.Quote(d.path))
	}
	return strings.Join(quoted, ", ")
}
