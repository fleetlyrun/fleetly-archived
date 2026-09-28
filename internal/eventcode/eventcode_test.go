package eventcode

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// sourceAnchorPattern 强制出处记载形如文档锚。注册表接受的 Source 形态：
//   - 设计文档章节引用：含 §节号（"release-semantics §2.7"、"E4 managed-
//     databases §5.3 (…)"）；
//   - 实现期票据注记："added during implementation"（DT-4/E5/T2.15/S17-D1/
//     S18-A10/B6/H10/T0-V2.2/E3/IMPL-* 等文档外事件名的出处形态）；
//   - 设计线引用："T-line "（T 线票据，如 T-line OT-1/IMPL-T15-1）。
//
// 新形态必须显式在此扩充（出处形态是契约，不许静默放行第四种写法）。
var sourceAnchorPattern = regexp.MustCompile(`§[0-9]|added during implementation|T-line `)

// TestDocEventSetMatchesRegistry 的投影形态：原 docEvents 手抄清单已删除
// （集相等由注册表自证——builtins 即唯一真源，事件集漂移由 golden 快照与
// usage 扫描兜底），本测试改为强制每事件携带非空且形如文档锚的 Source
// 出处：新增事件忘带出处即红。
func TestDocEventSetMatchesRegistry(t *testing.T) {
	for _, e := range Default().All() {
		if e.Source == "" {
			t.Errorf("event %s carries an empty Source (the provenance anchor travels with the name in events.go — the registry is the single source; do not reintroduce a test-side copy)", e.Name)
			continue
		}
		if !sourceAnchorPattern.MatchString(e.Source) {
			t.Errorf("event %s Source %q does not look like a documentation anchor (want a doc §section reference, an \"added during implementation\" ticket note, or a T-line design citation)", e.Name, e.Source)
		}
	}
}

// TestDuplicateRegistrationRejected：重复注册 fail-fast。
func TestDuplicateRegistrationRejected(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Event{Name: "test.duplicate", Summary: "s"})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate event registration must panic (fail-fast)")
		}
	}()
	r.MustRegister(Event{Name: "test.duplicate", Summary: "s2"})
}

// TestInvalidFormatRejected：非法格式 fail-fast（大写、缺 namespace 段、
// 多段、空串、数字开头）。
func TestInvalidFormatRejected(t *testing.T) {
	cases := []string{
		"",
		"UPPER.case",
		"noDot",
		"two.dots.here",
		".leading",
		"trailing.",
		"1number.start",
		"deployment.failed_extra_dash-", // 非法字符
	}
	for _, name := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("invalid event name %q must panic at registration", name)
				}
			}()
			NewRegistry().MustRegister(Event{Name: name, Summary: "s"})
		}()
	}
}

// TestGoldenSnapshot 事件集 golden 快照（防静默变更；快照含 Source 出处
// 列——出处是事件名的知识，漂移同样过门）。
func TestGoldenSnapshot(t *testing.T) {
	golden := filepath.Join("testdata", "events.golden")
	got := Default().Snapshot()
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden) //nolint:gosec // golden 为 testdata 固定路径
	if err != nil {
		t.Fatalf("read golden (run go test -update to regenerate): %v", err)
	}
	if string(want) != got {
		t.Fatalf("event set drifted from golden:\n--- golden ---\n%s\n--- registry ---\n%s", want, got)
	}
}

// TestNamespaces 命名空间覆盖核对：deployment/app/placement/node/volume/
// reconcile/restore/cron 八个域均非空。
func TestNamespaces(t *testing.T) {
	want := []string{"deployment", "app", "placement", "node", "volume", "reconcile", "restore", "cron"}
	seen := make(map[string]int)
	for _, name := range Default().Names() {
		seen[strings.SplitN(name, ".", 2)[0]]++
	}
	for _, ns := range want {
		if seen[ns] == 0 {
			t.Errorf("namespace %q has no registered events", ns)
		}
	}
}
