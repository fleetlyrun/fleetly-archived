package apitest

// B6（S15 出站字节出口收口）泄漏扫描负路径：遍历代表性错误/审计出口，
// 断言最终对外 message 与审计 diff 不含内部模式——绝对路径片段（`D:\`、
// `/tmp/`、`.git`）、`stderr`、`git fetch`、SQL 关键词（SELECT/INSERT）、
// `flt_` token 前缀。该测试即类 B 的验收本体（CI 层拦截同类回归）：
//   - B1：state 层错误直传 handler → 退化信封（gateway 出口渲染核心 =
//     apperr.EnvelopeFromGRPCStatus）为固定文案；
//   - B2：webhook 拉源失败（真实 git fetch 失败，stderr 含路径原文）→
//     信封固定文案 + 审计 detail 只记错误码/阶段；
//   - B4：审计构造含特殊字符（`"`、`\`）→ DiffSummary 产出合法 JSON。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/api"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/gitserver"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/testsupport"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// leakPatterns 是内部模式黑名单（B6 验收口径）：命中任一即视为出站字节
// 泄漏。
var leakPatterns = []string{`D:\`, "/tmp/", ".git", "stderr", "git fetch", "SELECT", "INSERT", "flt_"}

// assertNoLeak 断言 surface 文本不含任何内部模式。
func assertNoLeak(t *testing.T, surface, s string) {
	t.Helper()
	for _, p := range leakPatterns {
		if strings.Contains(s, p) {
			t.Errorf("%s leaks internal pattern %q: %s", surface, p, s)
		}
	}
}

// TestLeakScanDegradedEnvelope B1：无信封 detail 且 grpc code ∈ {Unknown,
// Internal, FailedPrecondition} → message 固定文案（构造的最坏原文含 SQL/
// 路径/stderr 全套模式）；业务码（NotFound/InvalidArgument）原文保留。
func TestLeakScanDegradedEnvelope(t *testing.T) {
	raw := `state: scan app: sql: SELECT id, name FROM apps; INSERT INTO audit_log ` +
		`-- D:\srv\fleetly\fleetly.db /tmp/fleetly.git: stderr: git fetch: exit status 128`
	for _, c := range []codes.Code{codes.Unknown, codes.Internal, codes.FailedPrecondition} {
		env, _ := apperr.EnvelopeFromGRPCStatus(status.New(c, raw))
		if env.GetMessage() != apperr.RedactedDegradedMessage {
			t.Fatalf("code=%s degraded message = %q, want fixed copy", c, env.GetMessage())
		}
		assertNoLeak(t, "degraded envelope (grpc "+c.String()+")", env.GetMessage())
	}
	for _, c := range []codes.Code{codes.NotFound, codes.InvalidArgument} {
		env, _ := apperr.EnvelopeFromGRPCStatus(status.New(c, raw))
		if env.GetMessage() != raw {
			t.Fatalf("code=%s business message must be preserved verbatim, got %q", c, env.GetMessage())
		}
	}
}

// TestLeakScanStateErrorThroughHandler B1 真实链路：state 层故障（库已关闭）
// → handler 裸 return 底层错误 → gRPC 传输层包装为 Unknown（gateway 收到
// 的即此形态）→ EnvelopeFromGRPCStatus（gateway 错误处理器的渲染核心）
// 输出固定文案，SQL 错误细节不出响应。
func TestLeakScanStateErrorThroughHandler(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	svc := api.NewAppsService(st, box, nil)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := svc.ListApps(ctx, &serverv1.ListAppsRequest{}); err == nil {
		t.Fatal("ListApps on closed store must fail")
	} else {
		// gRPC 传输层对非 status 错误的包装形态：code=Unknown、message=
		// 原文（gateway 收到的 status 即此）。
		env, httpStatus := apperr.EnvelopeFromGRPCStatus(status.New(codes.Unknown, err.Error()))
		if env.GetMessage() != apperr.RedactedDegradedMessage {
			t.Fatalf("gateway exit message = %q, want fixed copy", env.GetMessage())
		}
		assertNoLeak(t, "gateway exit (state error)", env.GetMessage())
		if httpStatus != http.StatusInternalServerError {
			t.Fatalf("gateway exit HTTP = %d, want 500", httpStatus)
		}
	}
}

// TestLeakScanWebhookFetchFailure B2：拉源失败（源 URL 指向不存在的仓库，
// git fetch 真实失败——stderr 携带路径原文）→ 对外不出内部细节。D1 异步
// 契约下：受理回 202（回执只有 status=accepted，无错误面）；fetch 在 worker
// 失败后经审计披露（detail 只记错误码与阶段）。响应体与审计 diff 均无内部
// 模式。
func TestLeakScanWebhookFetchFailure(t *testing.T) {
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skipf("host git not available: %v", err)
	}
	dir := t.TempDir()
	ctx := context.Background()
	st, err := state.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	src := gitserver.NewGitTriggers(gitserver.Config{
		Root: filepath.Join(dir, "git"),
	}, st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// D1：worker 随用例启停。StopWebhookWorker 等待的是「ctx 取消触发的
	// 排水 + drain」完成（生产面由 lynx Start ctx 取消驱动），用例需可
	//取消 ctx：wcancel 触发排空，Stop 等待完成（fetch 失败披露三件套
	// 落库后返回）。
	wctx, wcancel := context.WithCancel(ctx)
	src.StartWebhookWorker(wctx)
	t.Cleanup(func() { wcancel(); _ = src.StopWebhookWorker(context.Background()) })

	const secret = "hook-secret-at-least-16"
	appRow := testsupport.SeedApp(t, st, "leaky")
	cipher, err := box.Encrypt([]byte(secret))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := st.SetAppWebhookSecret(ctx, appRow.ID, string(cipher), ""); err != nil {
		t.Fatalf("SetAppWebhookSecret: %v", err)
	}
	missing := fileURLLeakScan(filepath.Join(dir, "missing-repo"))
	if err := st.SetAppSource(ctx, appRow.ID, state.AppSourceWrite{
		URL: missing, Branch: "main", AuthKind: state.SourceAuthNone,
	}); err != nil {
		t.Fatalf("SetAppSource: %v", err)
	}

	srv := httptest.NewServer(gitserver.NewWebhookHandler(src))
	t.Cleanup(srv.Close)

	body := `{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/apps/leaky/webhooks/github", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", "d-leakscan-1")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook accept status = %d, want 202 (%s)", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "accepted") {
		t.Fatalf("accept receipt missing status: %s", raw)
	}
	assertNoLeak(t, "webhook accept receipt", string(raw))

	// 排空 worker：取消 Start ctx 触发排水位 + drain（在处理项不被打断），
	// Stop 等待 fetch 失败披露（事件 + 审计 + 撤坑）完成后返回。
	wcancel()
	if err := src.StopWebhookWorker(ctx); err != nil {
		t.Fatalf("StopWebhookWorker: %v", err)
	}

	audits, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	var diff string
	for _, a := range audits {
		if a.Action == "app.webhook_rejected" {
			diff = a.DiffSummary
		}
	}
	if diff == "" {
		t.Fatal("fetch-failure audit (app.webhook_rejected) missing")
	}
	if !strings.Contains(diff, "E_RUNTIME_UNAVAILABLE") {
		t.Fatalf("audit detail must record error code: %s", diff)
	}
	assertNoLeak(t, "webhook fetch-failure audit diff", diff)
}

// fileURLLeakScan 把本地目录转 file:/// URL（与 gitserver 测试同款跨平台
// 形态）。
func fileURLLeakScan(dir string) string {
	u := filepath.ToSlash(dir)
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return "file://" + u
}

// TestLeakScanAuditDiffSpecialChars B4：DiffSummary 对含 `"`、`\` 与内部
// 模式词形的键值产出合法 JSON（json.Valid + 解析回读）；真实写路径
// （SetAppEnv 的键含特殊字符）审计 diff 同样合法可解析。
func TestLeakScanAuditDiffSpecialChars(t *testing.T) {
	key := `k"ey\` + "\n"
	value := `va"l\u` + strings.Join([]string{"D:\\x", "/tmp/y", ".git", "stderr", "git fetch", "SELECT 1", "flt_x"}, " ")
	s := state.DiffSummary(key, value, "n", 42, "b", true)
	if !json.Valid([]byte(s)) {
		t.Fatalf("DiffSummary produced invalid JSON: %s", s)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	if got, _ := m[key].(string); got != value {
		t.Fatalf("value round-trip = %q, want %q", got, value)
	}
	if got, _ := m["n"].(float64); got != 42 {
		t.Fatalf("numeric value = %v, want 42", m["n"])
	}

	// 真实写路径：env 键含特殊字符 → 审计 diff 仍为合法 JSON 且键回读一致。
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := testsupport.SeedApp(t, st, "weird")
	weirdKey := `we"ird\key`
	if _, err := st.SetAppEnv(context.Background(), app.ID, weirdKey, "cipher-blob", "platform", "human"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	audits, err := st.RecentAudits(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Action != "app.env_set" {
			continue
		}
		found = true
		if !json.Valid([]byte(a.DiffSummary)) {
			t.Fatalf("env audit diff invalid JSON: %s", a.DiffSummary)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(a.DiffSummary), &d); err != nil {
			t.Fatalf("decode env audit diff %s: %v", a.DiffSummary, err)
		}
		if got, _ := d["key"].(string); got != weirdKey {
			t.Fatalf("env audit diff key round-trip = %v, want %q", d, weirdKey)
		}
		if got, _ := d["status"].(string); got != "pending" {
			t.Fatalf("env audit diff status = %v, want pending", d)
		}
	}
	if !found {
		t.Fatal("app.env_set audit missing")
	}
}
