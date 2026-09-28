package errcode

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
//     databases §5.2 (…)"）；
//   - 实现期票据注记："added during implementation"（DT-4/MG-C3/M4-6/T2.15
//     等文档外码的出处形态）；
//   - 设计线引用："T-line "（T 线票据，如 T-line DT-5/IMPL-T2-1）。
//
// 新形态必须显式在此扩充（出处形态是契约，不许静默放行第四种写法）。
var sourceAnchorPattern = regexp.MustCompile(`§[0-9]|added during implementation|T-line `)

// TestDocCodeSetMatchesRegistry 是验收标准 2 的投影形态：原 docCodes 手抄
// 清单已删除（集相等由注册表自证——builtins 即唯一真源，码集漂移由
// golden 快照与 usage 扫描兜底），本测试改为强制每码携带非空且形如文档锚
// 的 Source 出处：新增码忘带出处即红。
func TestDocCodeSetMatchesRegistry(t *testing.T) {
	for _, c := range Default().All() {
		if c.Source == "" {
			t.Errorf("code %s carries an empty Source (the provenance anchor travels with the code in codes.go — the registry is the single source; do not reintroduce a test-side copy)", c.ID)
			continue
		}
		if !sourceAnchorPattern.MatchString(c.Source) {
			t.Errorf("code %s Source %q does not look like a documentation anchor (want a doc §section reference, an \"added during implementation\" ticket note, or a T-line design citation)", c.ID, c.Source)
		}
	}
}

// TestDuplicateRegistrationRejected 验收标准 3：重复注册 fail-fast。
func TestDuplicateRegistrationRejected(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Code{ID: "E_TEST_DUPLICATE", HTTP: 400, Summary: "s", Suggestion: "x"})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic (fail-fast)")
		}
	}()
	r.MustRegister(Code{ID: "E_TEST_DUPLICATE", HTTP: 409, Summary: "s2", Suggestion: "y"})
}

// TestInvalidFormatRejected 验收标准 3：非法格式 fail-fast（不以 E_/W_ 开头、
// 含小写、空串、空段）。
func TestInvalidFormatRejected(t *testing.T) {
	cases := []string{
		"E_lower",
		"e_UPPER",
		"X_NOT_REGISTRY",
		"NOTPREFIXED",
		"",
		"E_",
		"E__DOUBLE",
		"E_TRAILING_",
		"W_lower_case",
	}
	for _, id := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("invalid code %q must panic at registration", id)
				}
			}()
			NewRegistry().MustRegister(Code{ID: id, HTTP: 400, Summary: "s", Suggestion: "x"})
		}()
	}
}

// TestHTTPMappingInvariants：E_ 码须 4xx/5xx；W_ 码不得携带 HTTP 状态。
func TestHTTPMappingInvariants(t *testing.T) {
	for _, c := range Default().All() {
		if strings.HasPrefix(c.ID, "W_") {
			if c.HTTP != 0 {
				t.Errorf("warning %s carries HTTP %d, want 0", c.ID, c.HTTP)
			}
			continue
		}
		if c.HTTP < 400 || c.HTTP > 599 {
			t.Errorf("error %s HTTP = %d, want 4xx/5xx", c.ID, c.HTTP)
		}
		if c.Docs() != DocsURLPrefix+c.ID {
			t.Errorf("%s docs anchor = %q, want prefix+ID", c.ID, c.Docs())
		}
	}
}

// TestDocumentedHTTPMappings 文档显式给定的 HTTP 映射照文档。
func TestDocumentedHTTPMappings(t *testing.T) {
	want := map[string]int{
		"E_DOMAIN_CONFLICT":                 409, // architecture §2.4
		"E_STATE_VERSION_CONFLICT":          409, // state-model §2.2
		"E_VOLUME_NODE_MISMATCH":            409, // stateful-placement §2.8（前哨 409）
		"E_PLACEMENT_MOVE_REQUIRES_ACK":     409, // stateful-placement §2.2
		"E_EVENT_CURSOR_EXPIRED":            410, // state-model §2.9
		"E_LABEL_RESERVED":                  422, // state-model §2.4
		"E_PLACEMENT_LABEL_CONFLICT":        422, // stateful-placement §2.2
		"E_PLACEMENT_NODE_INVALID":          422, // stateful-placement §2.2（解析失败 422+候选）
		"E_PLACEMENT_NODE_NOT_FOUND":        422, // stateful-placement §2.5
		"E_REGISTRY_UNAVAILABLE":            503, // multi-node §5.2（D-MN-11 前哨快速失败）
		"E_REGISTRY_PUSH_FAILED":            500, // multi-node §5.2（D-MN-11 推送失败）
		"E_MULTI_NODE_REQUIRES_BASE_DOMAIN": 409, // multi-node §5.2（D-MN-13 join 门禁）
		// E4 managed-databases §5.2（9 码的 HTTP 映射为文档显式给定）。
		"E_DB_NOT_FOUND":            404,
		"E_DB_REFERENCED":           409,
		"E_DB_TEMPLATE_UNSUPPORTED": 400,
		"E_DB_ENV_PREFIX_CONFLICT":  422,
		"E_DB_BACKUP_FAILED":        500,
		"E_DB_RESTORE_FAILED":       500,
		"E_DB_ROTATE_FAILED":        500,
		"E_SECRET_NOT_FOUND":        422,
		"E_ENV_KEY_RESERVED":        422,
	}
	for id, httpStatus := range want {
		c, ok := Default().Get(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		if c.HTTP != httpStatus {
			t.Errorf("%s HTTP = %d, want %d (documented explicitly)", id, c.HTTP, httpStatus)
		}
	}
}

// TestGoldenSnapshot 码集 golden 快照：新增/改写码必须显式更新 golden
// （防静默变更；-update 重生成；快照含 Source 出处列——出处是码的知识，
// 漂移同样过门）。
func TestGoldenSnapshot(t *testing.T) {
	golden := filepath.Join("testdata", "codes.golden")
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
		t.Fatalf("code set drifted from golden:\n--- golden ---\n%s\n--- registry ---\n%s", want, got)
	}
}
