package acmedns

// 插件单测（实现票 §7）：httptest 假 API 端点断言两家的请求形态（认证/
// 表单字段/信封）与错误分支；探针 Present/CleanUp 生命周期与幂等清理。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestNewUnknownProvider 注册表错误分支：未知 provider 显式拒绝并列出词表。
func TestNewUnknownProvider(t *testing.T) {
	_, err := New("aliyun", []byte(`{"api_token":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown dns provider") {
		t.Fatalf("want unknown provider error, got %v", err)
	}
	if !strings.Contains(err.Error(), "cloudflare, dnspod") {
		t.Fatalf("error must list the provider vocabulary, got %v", err)
	}
}

// TestNewCredentialValidation 凭证形状校验：JSON 破损 / api_token 空 /
// dnspod 缺 ID 段 → 显式拒绝；cloudflare 单 token 合法。
func TestNewCredentialValidation(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		credentials string
		wantInErr   string
	}{
		{"bad json", "dnspod", `{`, "parse credentials"},
		{"empty token", "cloudflare", `{"api_token":"  "}`, "non-empty api_token"},
		{"dnspod missing id", "dnspod", `{"api_token":",token"}`, `<id>,<token>`},
		{"dnspod no comma", "dnspod", `{"api_token":"onlytoken"}`, `<id>,<token>`},
		{"cloudflare ok minimal", "cloudflare", `{"api_token":"tok"}`, ""},
	}
	for _, tc := range cases {
		_, err := New(tc.provider, []byte(tc.credentials))
		if tc.wantInErr == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantInErr) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.wantInErr, err)
		}
	}
}

// TestCandidateSuffixes zone 候选推导：最长优先、剥首标签、TLD 兜底排除。
func TestCandidateSuffixes(t *testing.T) {
	got := candidateSuffixes("_acme-challenge.console.example.com")
	want := []string{"console.example.com", "example.com"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
	// 契约：输入恒为带挑战前缀的完整记录名——首标签（前缀）被剥去。裸
	// 两标签名剥去首标签后只剩 TLD（单标签不是 zone）→ 空候选。
	if got := candidateSuffixes("example.com"); len(got) != 0 {
		t.Fatalf("bare two-label name yields no candidates, got %v", got)
	}
	if got := candidateSuffixes("_acme-challenge-test.example.com"); len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("probe-name candidates = %v, want [example.com]", got)
	}
	if got := subDomainOf("_acme-challenge.console.example.com", "example.com"); got != "_acme-challenge.console" {
		t.Fatalf("subDomainOf = %q", got)
	}
}

// ── DNSPod 假端点 ──────────────────────────────────────────────────────

type fakeDNSPodRecord struct {
	id    string
	value string
}

// fakeDNSPod 是 DNSPod 传统 API 的假端点：记录请求形态，回放状态机。
type fakeDNSPod struct {
	t        *testing.T
	domains  map[string]string             // 域名 → domain_id
	records  map[string][]fakeDNSPodRecord // sub_domain → records
	lastForm map[string]string             // 最近一次请求表单（形态断言）
	lastPath string                        // 最近一次请求路径
	// failAction 非空时该接口返回业务错误码（failCode 覆盖缺省码）。
	failAction string
	failCode   string
}

func newFakeDNSPod(t *testing.T) (*fakeDNSPod, *httptest.Server) {
	f := &fakeDNSPod{
		t:       t,
		domains: map[string]string{"example.com": "42"},
		records: map[string][]fakeDNSPodRecord{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeDNSPod) handle(w http.ResponseWriter, r *http.Request) {
	f.lastPath = strings.TrimPrefix(r.URL.Path, "/")
	_ = r.ParseForm()
	f.lastForm = map[string]string{}
	for k := range r.PostForm {
		f.lastForm[k] = r.PostForm.Get(k)
	}
	if f.lastForm["format"] != "json" {
		f.t.Errorf("%s: format = %q, want json", f.lastPath, f.lastForm["format"])
	}
	if f.lastForm["login_token"] != "77,tok" {
		f.t.Errorf("%s: login_token = %q, want the id,token form", f.lastPath, f.lastForm["login_token"])
	}
	switch f.lastPath {
	case "Domain.Info":
		name := f.lastForm["domain"]
		id, ok := f.domains[name]
		if !ok || f.failAction == "Domain.Info" {
			f.write(w, f.status(f.codeOr("-1"), "Domain name error"))
			return
		}
		f.write(w, map[string]any{
			"status": map[string]any{"code": "1", "message": "Action completed successful"},
			"domain": map[string]any{"id": json.Number(id), "name": name},
		})
	case "Record.Create":
		if f.failAction == "Record.Create" {
			f.write(w, f.status(f.codeOr("8"), "invalid record value"))
			return
		}
		if got := f.lastForm["record_type"]; got != "TXT" {
			f.t.Errorf("Record.Create record_type = %q, want TXT", got)
		}
		if got := f.lastForm["record_line"]; got == "" {
			f.t.Errorf("Record.Create record_line missing (dnspod requires a line)")
		}
		if got := f.lastForm["domain_id"]; got != "42" {
			f.t.Errorf("Record.Create domain_id = %q, want the resolved zone id", got)
		}
		if got := f.lastForm["value"]; got == "" {
			f.t.Errorf("Record.Create value missing")
		}
		sub := f.lastForm["sub_domain"]
		id := strconv.Itoa(100 + len(f.records[sub]))
		f.records[sub] = append(f.records[sub], fakeDNSPodRecord{id: id, value: f.lastForm["value"]})
		f.write(w, map[string]any{
			"status": map[string]any{"code": "1", "message": "Action completed successful"},
			"record": map[string]any{"id": json.Number(id), "name": sub},
		})
	case "Record.List":
		if f.failAction == "Record.List" {
			f.write(w, f.status(f.codeOr("6"), "wrong domain id"))
			return
		}
		sub := f.lastForm["sub_domain"]
		rs := f.records[sub]
		items := make([]map[string]any, 0, len(rs))
		for _, rec := range rs {
			items = append(items, map[string]any{
				"id": json.Number(rec.id), "name": sub, "type": "TXT", "value": rec.value,
			})
		}
		f.write(w, map[string]any{
			"status":  map[string]any{"code": "1", "message": "Action completed successful"},
			"info":    map[string]any{"record_total": len(items)},
			"records": items,
		})
	case "Record.Remove":
		if f.failAction == "Record.Remove" {
			f.write(w, f.status(f.codeOr("21"), "Domain is locked"))
			return
		}
		id := f.lastForm["record_id"]
		for sub, rs := range f.records {
			for i, rec := range rs {
				if rec.id == id {
					f.records[sub] = append(rs[:i:i], rs[i+1:]...)
				}
			}
		}
		f.write(w, f.status("1", "Action completed successful"))
	default:
		f.t.Fatalf("unexpected action %s", f.lastPath)
	}
}

func (f *fakeDNSPod) status(code, message string) map[string]any {
	return map[string]any{"status": map[string]any{"code": code, "message": message}}
}

func (f *fakeDNSPod) write(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func (f *fakeDNSPod) codeOr(def string) string {
	if f.failCode != "" {
		return f.failCode
	}
	return def
}

func newDNSPodTest(t *testing.T) (Provider, *fakeDNSPod) {
	f, srv := newFakeDNSPod(t)
	p, err := New(ProviderDNSPod, []byte(`{"api_token":"77,tok"}`), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New dnspod: %v", err)
	}
	return p, f
}

// TestDNSPodPresentCleanUpLifecycle DNSPod 全生命周期：Present 走
// Domain.Info（zone 解析→domain_id）→ Record.Create（表单字段断言）；
// CleanUp 走 Record.List（值匹配）→ Record.Remove；清理后记录消失，
// 幂等重入不报错。
func TestDNSPodPresentCleanUpLifecycle(t *testing.T) {
	p, f := newDNSPodTest(t)
	ctx := testContext(t)
	const fqdn = "_acme-challenge.console.example.com"
	if err := p.Present(ctx, fqdn, "digest-value"); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if got := f.records["_acme-challenge.console"]; len(got) != 1 || got[0].value != "digest-value" {
		t.Fatalf("record not created as expected: %+v", f.records)
	}
	if err := p.CleanUp(ctx, fqdn, "digest-value"); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := f.records["_acme-challenge.console"]; len(got) != 0 {
		t.Fatalf("record not removed: %+v", got)
	}
	// 幂等重入：无匹配记录的 CleanUp 是 no-op 不报错。
	if err := p.CleanUp(ctx, fqdn, "digest-value"); err != nil {
		t.Fatalf("idempotent CleanUp: %v", err)
	}
}

// TestDNSPodCleanUpValueMatchOnly CleanUp 只删值匹配的记录：同名不同值的
// 他人记录保留。
func TestDNSPodCleanUpValueMatchOnly(t *testing.T) {
	p, f := newDNSPodTest(t)
	ctx := testContext(t)
	f.records["_acme-challenge"] = []fakeDNSPodRecord{
		{id: "55", value: "someone-elses"},
		{id: "56", value: "digest-value"},
	}
	if err := p.CleanUp(ctx, "_acme-challenge.example.com", "digest-value"); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	got := f.records["_acme-challenge"]
	if len(got) != 1 || got[0].value != "someone-elses" {
		t.Fatalf("only the matching record must be removed: %+v", got)
	}
}

// TestDNSPodErrorBranches 错误分支：Create 业务错误码如实上抛（文案带状态
// 码与 message）；Remove 失败上抛；无覆盖 zone 的记录名显式报错。
func TestDNSPodErrorBranches(t *testing.T) {
	p, f := newDNSPodTest(t)
	ctx := testContext(t)

	f.failAction = "Record.Create"
	err := p.Present(ctx, "_acme-challenge.example.com", "v")
	if err == nil || !strings.Contains(err.Error(), "status.code=8") || !strings.Contains(err.Error(), "invalid record value") {
		t.Fatalf("create failure must surface status code and message, got %v", err)
	}

	f.failAction = "Record.Remove"
	f.records["_acme-challenge"] = []fakeDNSPodRecord{{id: "55", value: "v"}}
	if err := p.CleanUp(ctx, "_acme-challenge.example.com", "v"); err == nil ||
		!strings.Contains(err.Error(), "Domain is locked") {
		t.Fatalf("remove failure must surface provider message, got %v", err)
	}

	f.failAction = ""
	f.records["_acme-challenge"] = nil
	if err := p.Present(ctx, "_acme-challenge.other.test", "v"); err == nil ||
		!strings.Contains(err.Error(), "no zone owned by this account") {
		t.Fatalf("uncovered record name must fail explicitly, got %v", err)
	}
}

// ── Cloudflare 假端点 ─────────────────────────────────────────────────

// fakeCloudflare 是 Cloudflare API v4 的假端点：Bearer 头 + JSON 信封。
type fakeCloudflare struct {
	t      *testing.T
	zones  map[string]string             // zone 名 → id
	shards map[string][]cloudflareRecord // 记录名 → records
	// lastAuth 是最近一次请求的 Authorization 头（Bearer 形态断言）。
	lastAuth string
	// failCreate 置 true 时 create 返回失败信封（HTTP 400）。
	failCreate bool
	// authBad 置 true 时 zone 查询恒返回鉴权失败信封。
	authBad bool
	// zoneCalls / listCalls 是调用计数（候选迭代形态断言）。
	zoneCalls int
	listCalls int
}

func newFakeCloudflare(t *testing.T) (*fakeCloudflare, *httptest.Server) {
	f := &fakeCloudflare{
		t:      t,
		zones:  map[string]string{"example.com": "zone-1"},
		shards: map[string][]cloudflareRecord{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCloudflare) handle(w http.ResponseWriter, r *http.Request) {
	f.lastAuth = r.Header.Get("Authorization")
	if f.lastAuth != "Bearer cf-tok" {
		f.t.Errorf("Authorization = %q, want the Bearer token form", f.lastAuth)
	}
	switch {
	case r.URL.Path == "/zones":
		f.zoneCalls++
		name := r.URL.Query().Get("name")
		if name == "" {
			f.t.Errorf("zone lookup must filter by exact name")
		}
		if f.authBad {
			f.write(w, http.StatusForbidden, false, nil, []map[string]any{{"code": 9103, "message": "Invalid request headers"}})
			return
		}
		id, ok := f.zones[name]
		if !ok {
			f.write(w, http.StatusOK, true, []any{}, nil)
			return
		}
		f.write(w, http.StatusOK, true, []map[string]any{{"id": id, "name": name}}, nil)
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodPost:
		if f.failCreate {
			f.write(w, http.StatusBadRequest, false, nil, []map[string]any{{"code": 9103, "message": "Invalid request headers"}})
			return
		}
		var body struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Content string `json:"content"`
			TTL     int    `json:"ttl"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Type != "TXT" || body.Name == "" || body.Content == "" {
			f.t.Errorf("create body incomplete: %+v", body)
		}
		rec := cloudflareRecord{
			ID:      strconv.Itoa(900 + len(f.shards[body.Name])),
			Type:    body.Type,
			Name:    body.Name,
			Content: body.Content,
		}
		f.shards[body.Name] = append(f.shards[body.Name], rec)
		f.write(w, http.StatusOK, true, rec, nil)
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodGet:
		f.listCalls++
		if r.URL.Query().Get("type") != "TXT" {
			f.t.Errorf("list must filter type=TXT, got %q", r.URL.Query().Get("type"))
		}
		f.write(w, http.StatusOK, true, f.shards[r.URL.Query().Get("name")], nil)
	case strings.Contains(r.URL.Path, "/dns_records/") && r.Method == http.MethodDelete:
		parts := strings.Split(r.URL.Path, "/")
		id := parts[len(parts)-1]
		for name, rs := range f.shards {
			for i, rec := range rs {
				if rec.ID == id {
					f.shards[name] = append(rs[:i:i], rs[i+1:]...)
				}
			}
		}
		f.write(w, http.StatusOK, true, map[string]any{"id": id}, nil)
	default:
		f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}
}

func (f *fakeCloudflare) write(w http.ResponseWriter, status int, ok bool, result any, errs []map[string]any) {
	payload := map[string]any{"success": ok, "result": result, "errors": errs}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func newCloudflareTest(t *testing.T) (Provider, *fakeCloudflare) {
	f, srv := newFakeCloudflare(t)
	p, err := New(ProviderCloudflare, []byte(`{"api_token":"cf-tok"}`), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("New cloudflare: %v", err)
	}
	return p, f
}

// TestCloudflarePresentCleanUpLifecycle Cloudflare 全生命周期：zone 精确名
// 查询 → 建记录（Bearer + JSON 体断言）→ 值匹配清理 → 幂等重入。
func TestCloudflarePresentCleanUpLifecycle(t *testing.T) {
	p, f := newCloudflareTest(t)
	ctx := testContext(t)
	const fqdn = "_acme-challenge.console.example.com"
	if err := p.Present(ctx, fqdn, "digest-value"); err != nil {
		t.Fatalf("Present: %v", err)
	}
	got := f.shards[fqdn]
	if len(got) != 1 || got[0].Content != "digest-value" || got[0].Type != "TXT" {
		t.Fatalf("record not created as expected: %+v", got)
	}
	// zone 查询按候选迭代：首候选 console.example.com 未命中，次候选
	// example.com 命中 → zoneCalls 恰为 2。
	if f.zoneCalls != 2 {
		t.Fatalf("zone calls = %d, want 2 (longest-candidate-first resolution)", f.zoneCalls)
	}
	if err := p.CleanUp(ctx, fqdn, "digest-value"); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := f.shards[fqdn]; len(got) != 0 {
		t.Fatalf("record not removed: %+v", got)
	}
	if err := p.CleanUp(ctx, fqdn, "digest-value"); err != nil {
		t.Fatalf("idempotent CleanUp: %v", err)
	}
}

// TestCloudflareCleanUpValueMatchOnly 同名不同值的记录在 CleanUp 后保留。
func TestCloudflareCleanUpValueMatchOnly(t *testing.T) {
	p, f := newCloudflareTest(t)
	ctx := testContext(t)
	const fqdn = "_acme-challenge.example.com"
	f.shards[fqdn] = []cloudflareRecord{
		{ID: "1", Type: "TXT", Name: fqdn, Content: "someone-elses"},
		{ID: "2", Type: "TXT", Name: fqdn, Content: "digest-value"},
	}
	if err := p.CleanUp(ctx, fqdn, "digest-value"); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	got := f.shards[fqdn]
	if len(got) != 1 || got[0].Content != "someone-elses" {
		t.Fatalf("only the matching record must be removed: %+v", got)
	}
}

// TestCloudflareErrorBranches 错误分支：建记录失败信封如实上抛（code+
// message）；鉴权失败（zone 查询 403 信封）不与「无覆盖 zone」混淆。
func TestCloudflareErrorBranches(t *testing.T) {
	p, f := newCloudflareTest(t)
	ctx := testContext(t)

	f.failCreate = true
	err := p.Present(ctx, "_acme-challenge.example.com", "v")
	if err == nil || !strings.Contains(err.Error(), "code=9103") {
		t.Fatalf("create failure must surface envelope errors, got %v", err)
	}
	f.failCreate = false

	f.authBad = true
	err = p.Present(ctx, "_acme-challenge.example.com", "v")
	if err == nil || !strings.Contains(err.Error(), "Invalid request headers") {
		t.Fatalf("auth failure must surface as query error, got %v", err)
	}
	f.authBad = false

	if err := p.Present(ctx, "_acme-challenge.other.test", "v"); err == nil ||
		!strings.Contains(err.Error(), "no zone accessible to this token") {
		t.Fatalf("uncovered record name must fail explicitly, got %v", err)
	}
}

// TestProbe 探针生命周期：成功 = 两步全绿；create 失败在 create 步短路
// （delete 不执行——凭证错时无记录可删）；delete 失败如实落 delete 步。
func TestProbe(t *testing.T) {
	p, f := newDNSPodTest(t)
	ctx := testContext(t)

	res := Probe(ctx, p, ProbeRecordName("example.com"))
	if !res.OK || res.FailedStep != "" {
		t.Fatalf("probe should pass: %+v", res)
	}
	if len(res.Steps) != 2 || res.Steps[0].Step != "create" || res.Steps[1].Step != "delete" {
		t.Fatalf("probe steps = %+v, want create then delete", res.Steps)
	}

	f.failAction = "Record.Create"
	res = Probe(ctx, p, ProbeRecordName("example.com"))
	if res.OK || res.FailedStep != "create" {
		t.Fatalf("create failure must short-circuit at create: %+v", res)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps after create failure = %+v, want only create", res.Steps)
	}

	f.failAction = "Record.Remove"
	f.records["_acme-challenge-test"] = []fakeDNSPodRecord{{id: "55", value: "fleetly-dns-probe"}}
	res = Probe(ctx, p, ProbeRecordName("example.com"))
	if res.OK || res.FailedStep != "delete" {
		t.Fatalf("delete failure must surface at delete step: %+v", res)
	}
}

// TestProbeRecordName 探针名公式（实现票口径：_acme-challenge-test.<base>）。
func TestProbeRecordName(t *testing.T) {
	if got := ProbeRecordName("example.test"); got != "_acme-challenge-test.example.test" {
		t.Fatalf("ProbeRecordName = %q", got)
	}
}

// TestTrimTrailingDot 尾点裁剪（lego EffectiveFQDN 形态的入口归一）。
func TestTrimTrailingDot(t *testing.T) {
	if got := trimTrailingDot("_acme-challenge.example.com."); got != "_acme-challenge.example.com" {
		t.Fatalf("trimTrailingDot = %q", got)
	}
	if got := trimTrailingDot("a.b"); got != "a.b" {
		t.Fatalf("trimTrailingDot = %q", got)
	}
}
