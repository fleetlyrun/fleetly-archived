package cmd

// audit 命令的验收测试（v0.3 W3-S1，rbac-teams §6 D-W0-6 CLI 读面）：过滤
// 旗标矩阵（list/export 同源）、--json 形态、CSV 导出（表头 + RFC 4180
// 转义）、用法面与权限差分。服务面夹具 = startCLI 的进程内真实服务
//（不 mock）；审计行经 env.Store 事务内 WriteAudit 直播。
//
// 锚定口径：apitest.Start 自带一枚 bootstrap token（token.create 审计行，
// actor=human）——表内既有行不可清零，绝对计数只出现在**动作前缀锚定**
// 的过滤查询里（auth./api./audit. 三族均不与 token.create 相交）；无过滤
// 查询只做集合存在性与自洽断言。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// seedCLIAuthRows 播种四行可区分审计（返回行集；diff 特意带引号/逗号/
// 换行——CSV 转义断言的锚）。at 升序播种，读面恒倒序。
func seedCLIAuthRows(t *testing.T, env *apitestFixture) []state.AuditEntry {
	t.Helper()
	rows := []state.AuditEntry{
		{ID: "01CLIAUDITLOGINFAIL000000", At: env.base, Actor: "human", Action: "auth.login_failed", Target: "login:ada@example.com", Result: "error", ErrorCode: "E_AUTH_INVALID_CREDENTIALS"},
		{ID: "01CLIAUDITAPIDELETE000000", At: env.base.Add(time.Minute), Actor: "user:01USERADA000000000000", Action: "api.AppsService.DeleteApp", Target: "app:01APP00000000000000", Result: "ok", RequestID: "req-7"},
		{ID: "01CLIAUDITWEBHOOKSET00000", At: env.base.Add(2 * time.Minute), Actor: "user:01USERADA000000000000", Action: "api.AppsService.SetAppWebhookSecret", Target: "app:01APP00000000000000", Result: "ok"},
		{ID: "01CLIAUDITRETENTION000000", At: env.base.Add(3 * time.Minute), Actor: "system", Action: "audit.retention_changed", Target: "platform:audit", Result: "ok", DiffSummary: state.DiffSummary("note", "a,b") + "\nline2"},
	}
	for _, r := range rows {
		if err := env.Store.InTx(context.Background(), func(tx *state.Tx) error {
			return tx.WriteAudit(context.Background(), r)
		}); err != nil {
			t.Fatalf("seed audit %s: %v", r.Action, err)
		}
	}
	return rows
}

// apitestFixture 是本文件的夹具视图（startCLI 的 Env + 时间锚）。
type apitestFixture struct {
	Store *state.Store
	base  time.Time
}

// newAuditFixture 起 CLI 夹具并播种审计行。
func newAuditFixture(t *testing.T) *apitestFixture {
	env := startCLI(t)
	f := &apitestFixture{Store: env.Store, base: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	seedCLIAuthRows(t, f)
	return f
}

// auditListJSON 驱动 `audit list --json` 并解出响应（失败即 Fatal）。
func auditListJSON(t *testing.T, args ...string) (rows []map[string]any, total int) {
	t.Helper()
	code, out, errOut := runCLIConn(t, append([]string{"audit", "list", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("audit list %v: code=%d stderr=%s", args, code, errOut)
	}
	var resp struct {
		Audits []map[string]any `json:"audits"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("audit list %v: --json not parseable: %v\n%s", args, err, out)
	}
	return resp.Audits, resp.Total
}

// TestAuditListFilters 过滤旗标矩阵（票面裁决逐维钉 CLI 参数不吞不吃；
// 绝对计数全部锚在动作前缀/actor 互异的查询上——fixture 既有 token.create
// 行不与三族动作前缀相交）。
func TestAuditListFilters(t *testing.T) {
	f := newAuditFixture(t)
	since90 := f.base.Add(90 * time.Second).Format(time.RFC3339)

	// 人读形态：动作列 + 总数行（锚定 auth. 族恰 1 行）。
	code, out, errOut := runCLIConn(t, "audit", "list", "--action", "auth.")
	if code != 0 {
		t.Fatalf("list auth.: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "auth.login_failed") || !strings.Contains(out, "showing 1 of 1 matching entries") {
		t.Fatalf("list auth. output:\n%s", out)
	}

	for _, tc := range []struct {
		name  string
		args  []string
		wantN int
	}{
		{"action prefix auth", []string{"--action", "auth."}, 1},
		{"action prefix api", []string{"--action", "api."}, 2},
		{"action prefix full verb", []string{"--action", "audit.retention"}, 1},
		{"actor substring user id", []string{"--actor", "01USERADA"}, 2},
		{"actor substring system", []string{"--actor", "system"}, 1},
		{"actor+action anchor", []string{"--actor", "human", "--action", "auth."}, 1},
		{"result exact error", []string{"--result", "error"}, 1},
		{"action+result ok", []string{"--action", "api.", "--result", "ok"}, 2},
		{"target substring app", []string{"--target", "app:01APP"}, 2},
		{"target substring platform", []string{"--target", "platform:audit"}, 1},
		{"since window anchored", []string{"--action", "api.", "--since", since90}, 1},
		{"until window anchored", []string{"--action", "api.", "--until", since90}, 1},
		{"combined no match", []string{"--action", "api.", "--result", "error"}, 0},
		{"limit within action family", []string{"--action", "api.", "--limit", "1"}, 1},
		{"limit+offset page two", []string{"--action", "api.", "--limit", "1", "--offset", "1"}, 1},
	} {
		rows, _ := auditListJSON(t, tc.args...)
		if len(rows) != tc.wantN {
			t.Fatalf("%s: rows=%d, want %d", tc.name, len(rows), tc.wantN)
		}
	}

	// total 恒为「过滤生效、分页生效前」的全量命中：api. 族 = 2，无论分页。
	for _, args := range [][]string{{"--action", "api."}, {"--action", "api.", "--limit", "1"}, {"--action", "api.", "--limit", "1", "--offset", "1"}} {
		if _, total := auditListJSON(t, args...); total != 2 {
			t.Fatalf("total for %v = %d, want 2", args, total)
		}
	}

	// 无过滤查询：自洽（total = 行数）+ 四行种子全在 + bootstrap 行共存
	//（既有面不被清零的诚实投影）。
	rows, total := auditListJSON(t)
	if total != len(rows) || total < 5 {
		t.Fatalf("unfiltered: rows=%d total=%d, want >=5 self-consistent", len(rows), total)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r["id"].(string)] = true
	}
	for _, id := range []string{
		"01CLIAUDITLOGINFAIL000000", "01CLIAUDITAPIDELETE000000",
		"01CLIAUDITWEBHOOKSET00000", "01CLIAUDITRETENTION000000",
	} {
		if !seen[id] {
			t.Fatalf("seeded row %s missing from the unfiltered page", id)
		}
	}

	// --json 投影全字段：error_code / request_id / diff_summary / at 就位。
	code, out, _ = runCLIConn(t, "audit", "list", "--json", "--actor", "human", "--action", "auth.")
	if code != 0 || !strings.Contains(out, `"error_code": "E_AUTH_INVALID_CREDENTIALS"`) ||
		!strings.Contains(out, `"at": "`) {
		t.Fatalf("projection of the login_failed row:\n%s", out)
	}
	code, out, _ = runCLIConn(t, "audit", "list", "--json", "--action", "api.AppsService.DeleteApp")
	if code != 0 || !strings.Contains(out, `"request_id": "req-7"`) {
		t.Fatalf("request_id projection:\n%s", out)
	}
}

// TestAuditExportCSV 导出：表头精确、行数、RFC 4180 转义（引号翻倍 +
// 含分隔符/换行字段整体加引号）、CRLF 行尾、过滤集与 list 同源、
// --csv 缺失显式拒绝。
func TestAuditExportCSV(t *testing.T) {
	_ = newAuditFixture(t) // 播种四行审计；导出面只消费 CLI 输出与落盘文件

	csvPath := filepath.Join(t.TempDir(), "audit.csv")
	code, out, errOut := runCLIConn(t, "audit", "export", "--csv", csvPath)
	if code != 0 {
		t.Fatalf("export: code=%d stderr=%s", code, errOut)
	}
	raw, err := os.ReadFile(csvPath) //nolint:gosec // G304：测试受控临时路径
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	content := string(raw)
	if !strings.HasSuffix(content, "\r\n") {
		t.Fatalf("csv must end with CRLF (RFC 4180): %q", tailOf(content))
	}
	lines := strings.Split(strings.TrimSuffix(content, "\r\n"), "\r\n")
	const wantHeader = "id,at,actor,action,target,result,error_code,request_id,diff_summary"
	if lines[0] != wantHeader {
		t.Fatalf("csv header = %q, want %q", lines[0], wantHeader)
	}
	// 数据行：四行种子全部在盘（夹具自带 team/project/token.created 行——
	// 绝对总数不可锚，按 ID 前缀逐行定位断言）。
	var escLine, plainLine string
	seededSeen := map[string]bool{}
	for _, l := range lines[1:] {
		for _, id := range []string{
			"01CLIAUDITLOGINFAIL000000", "01CLIAUDITAPIDELETE000000",
			"01CLIAUDITWEBHOOKSET00000", "01CLIAUDITRETENTION000000",
		} {
			if strings.HasPrefix(l, id+",") {
				seededSeen[id] = true
			}
		}
		switch {
		case strings.HasPrefix(l, "01CLIAUDITRETENTION000000,"):
			escLine = l
		case strings.HasPrefix(l, "01CLIAUDITAPIDELETE000000,"):
			plainLine = l
		}
	}
	for id, seen := range seededSeen {
		if !seen {
			t.Fatalf("seeded row %s missing from the export", id)
		}
	}
	// 转义：diff_summary 含引号/逗号/换行 → 字段整体加引号、内部引号翻倍。
	if !strings.Contains(escLine, `"{""note"":""a,b""}`+"\nline2\"") {
		t.Fatalf("escaping broken:\n%q", escLine)
	}
	// 无特殊字符的行不加引号（RFC 4180 最小化形态）。
	if strings.Contains(plainLine, `"`) {
		t.Fatalf("plain row must not be quoted:\n%q", plainLine)
	}
	// 摘要行 = 数据行数（表内全量命中，含夹具既有行）。
	if want := "exported " + strconv.Itoa(len(lines)-1) + " audit entries"; !strings.Contains(out, want) {
		t.Fatalf("export summary %q missing in:\n%s", want, out)
	}

	// 过滤集同源：action 前缀过滤进导出（auth. 族恰 1 行 + 表头）。
	csvPath2 := filepath.Join(t.TempDir(), "audit-filtered.csv")
	code, out, _ = runCLIConn(t, "audit", "export", "--csv", csvPath2, "--action", "auth.")
	if code != 0 || !strings.Contains(out, "exported 1 audit entries") {
		t.Fatalf("filtered export: code=%d out=%s", code, out)
	}
	raw2, _ := os.ReadFile(csvPath2) //nolint:gosec // G304：测试受控临时路径
	if n := strings.Count(string(raw2), "\r\n"); n != 2 {
		t.Fatalf("filtered csv lines = %d, want 2\n%q", n, raw2)
	}
	if !strings.Contains(string(raw2), "auth.login_failed") {
		t.Fatalf("filtered csv missing the login_failed row:\n%q", raw2)
	}

	// --csv 缺失：显式拒绝（不用法错误兜底吞掉——64 + 可行动文案）。
	code, _, errOut = runCLIConn(t, "audit", "export")
	if code != 64 || !strings.Contains(errOut, "--csv <path> is required") {
		t.Fatalf("export without --csv: code=%d stderr=%s, want 64", code, errOut)
	}
}

// TestAuditUsageSurfaces 用法面：坏 --since（RFC 3339 之外显式拒绝——1）、
// 多余位置参数（64）、裸动词（64 + 子命令指引）、help 面列出两子命令、
// 顶层 help 列出 audit 动词。
func TestAuditUsageSurfaces(t *testing.T) {
	startCLI(t)
	code, _, errOut := runCLIConn(t, "audit", "list", "--since", "yesterday")
	if code != 1 || !strings.Contains(errOut, "invalid RFC 3339 timestamp") {
		t.Fatalf("bad --since: code=%d stderr=%s, want 1", code, errOut)
	}
	code, _, errOut = runCLIConn(t, "audit", "export", "--since", "not-a-time", "--csv", filepath.Join(t.TempDir(), "x.csv"))
	if code != 1 || !strings.Contains(errOut, "--since: invalid RFC 3339 timestamp") {
		t.Fatalf("bad --since on export: code=%d stderr=%s, want 1", code, errOut)
	}
	if code, _, _ := runCLIConn(t, "audit", "list", "extra"); code != 64 {
		t.Fatalf("extra arg: code=%d, want 64", code)
	}
	code, _, errOut = runCLIConn(t, "audit")
	if code != 64 || !strings.Contains(errOut, "missing subcommand (list|export)") {
		t.Fatalf("bare audit: code=%d stderr=%s, want 64/usage", code, errOut)
	}
	if code, _, _ := runCLIConn(t, "audit", "frobnicate"); code != 64 {
		t.Fatalf("unknown subcommand: code=%d, want 64", code)
	}

	code, out, _ := runCLIConn(t, "help", "audit")
	if code != 0 {
		t.Fatalf("help audit: code=%d", code)
	}
	for _, want := range []string{"list", "export"} {
		if !strings.Contains(out, want) {
			t.Errorf("help audit missing %q:\n%s", want, out)
		}
	}
	code, out, _ = runCLIConn(t)
	if code != 0 || !strings.Contains(out, "audit") {
		t.Fatalf("top-level help missing audit: code=%d", code)
	}
}

// TestAuditPlatformAdminGate CLI 面的权限差分（矩阵本体在 api 单测钉死）：
// admin 机具令牌（startCLI 的 env 凭据）通；非平台管理员用户的 admin-scope
// 用户 PAT 撒开 scope 门、命中 handler 的平台面门（403 文案、退出 1）；
// read-scope 用户 PAT 在 scope 门先拒（双层门各自可见）。
func TestAuditPlatformAdminGate(t *testing.T) {
	env := startCLI(t)

	// 机具令牌正面。
	if code, _, errOut := runCLIConn(t, "audit", "list", "--json"); code != 0 {
		t.Fatalf("machine admin list: code=%d stderr=%s", code, errOut)
	}

	// 非平台管理员用户（founder 先注册占首用户位 = 平台管理员；mate =
	// 第二注册用户——个人队 owner、非平台管理员）。
	if err := env.Store.SaveRegistration(context.Background(), state.AuthRegistrationOpen, state.AuthSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("open registration: %v", err)
	}
	_ = env.SeedUser(t, "founder@example.com", "password-123")
	mate := env.SeedUser(t, "mate@example.com", "password-123")

	// admin-scope 用户 PAT：scope 门放行、平台面门拒（403 文案可见）。
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate token: %v", err)
	}
	adminPat := "flt_" + hex.EncodeToString(raw)
	if _, err := env.Store.CreateToken(context.Background(), state.TokenWrite{
		Hash: state.HashToken(adminPat), Name: "audit gate probe", Scopes: "admin", UserID: mate.User.ID, Actor: "human",
	}); err != nil {
		t.Fatalf("create admin-scope user PAT: %v", err)
	}
	t.Setenv("FLEETLY_TOKEN", adminPat)
	code, _, errOut := runCLIConn(t, "audit", "list")
	if code != 1 || !strings.Contains(errOut, "platform administrator privileges required") {
		t.Fatalf("non-admin admin-scope PAT list: code=%d stderr=%s, want platform-gate wording", code, errOut)
	}

	// read-scope 用户 PAT：scope 门先拒（fail-closed 第一道）。
	readPat := env.SeedUserToken(t, mate.User.ID)
	t.Setenv("FLEETLY_TOKEN", readPat)
	code, _, errOut = runCLIConn(t, "audit", "list")
	if code != 1 || !strings.Contains(errOut, "token scope insufficient") {
		t.Fatalf("non-admin read-scope PAT list: code=%d stderr=%s, want scope-gate wording", code, errOut)
	}
}

// tailOf 是失败信息用的短尾片段（调试可读性，非断言面）。
func tailOf(s string) string {
	if len(s) > 80 {
		return s[len(s)-80:]
	}
	return s
}
