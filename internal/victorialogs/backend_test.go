package victorialogs

// VL HTTP 消费面的 hermetic 单测（httptest 假端点——真机协议行为已于
// 2026-09-21 用 victoriametrics/victoria-logs:v1.52.0 容器实测核实；本测
// 面钉住契约形状与注入安全）：
//   - LogsQL 转义正负向（设计 §3.1 硬性：恶意 keyword 不得逃逸出字面量
//     短语语义——引号/反斜杠/管道/星号/正则元字符）；
//   - 白名单校验（服务名/来源）；
//   - ES bulk 载荷形状与 _stream_fields 契约；
//   - 后端不可达/非 2xx 的错误传播（api 层映射 E_LOGS_BACKEND_UNAVAILABLE）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/logs"
)

// atUTC 是测试用时间构造器（epoch+seconds 的 UTC 形态）。
func atUTC(t *testing.T, seconds int64) time.Time {
	t.Helper()
	return time.Unix(seconds, 0).UTC()
}

// TestEscapeLogsQLPhrasePositive 转义正向：反斜杠与双引号转义（VL v1.52
// 实测口径），其余字符保持字面。
func TestEscapeLogsQLPhrasePositive(t *testing.T) {
	cases := []struct{ in, want string }{
		{in: `hello`, want: `hello`},
		{in: `a"b`, want: `a\"b`},
		{in: `a\b`, want: `a\\b`},
		{in: `a\"b`, want: `a\\\"b`},
		{in: `a|b*c d`, want: `a|b*c d`},
		{in: `{app="x"}`, want: `{app=\"x\"}`},
	}
	for _, tc := range cases {
		if got := EscapeLogsQLPhrase(tc.in); got != tc.want {
			t.Errorf("EscapeLogsQLPhrase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildLogsQLInjectionNegative 注入负向（设计 §3.1 硬性钉住）：恶意
// keyword 构造的查询串中，用户输入只能以转义后的字面量短语存在——不得
// 出现第二个短语/过滤器 token、不得截断引号闭合、管道不得成为查询管道
// 语义。判定法：逐字符扫描（含转义态跟踪），**未转义**的引号恰为 2 个
//（短语开 + 闭）、转义符永不悬垂（成对出现）。
func TestBuildLogsQLInjectionNegative(t *testing.T) {
	malicious := []string{
		`x" or app=~"^(.*)$`,     // 引号闭合逃逸出短语
		`x" ; drop`,              // 引号 + 新语句形态
		`a\"b\c|d*e`,             // 混合（真机验证样本）
		`" | wrap {app=~".*"} "`, // 管道包装
		`\`,                      // 裸反斜杠（不得把闭合引号转义掉）
		`*" OR "`,                // 通配 + 引号
	}
	for _, kw := range malicious {
		q, err := BuildLogsQL(nil, nil, nil, nil, kw)
		if err != nil {
			t.Fatalf("BuildLogsQL(%q) unexpected error: %v", kw, err)
		}
		// 逐字符扫描：跟踪转义态，统计未转义引号数；校验 `\` 永不悬垂。
		unescapedQuotes := 0
		escaped := false
		for i := 0; i < len(q); i++ {
			c := q[i]
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				unescapedQuotes++
			}
		}
		if escaped {
			t.Errorf("BuildLogsQL(%q): query %q ends with a dangling escape", kw, q)
		}
		if unescapedQuotes != 2 {
			t.Errorf("BuildLogsQL(%q): query %q has %d unescaped quotes, want exactly 2 (phrase open+close)", kw, q, unescapedQuotes)
		}
	}
	// 端到端核对：真机样本 keyword 的查询 = match-all 前缀 + 单一转义
	// 短语（语义不逃逸的充分形态——VL v1.52 实测精确命中该消息）。
	q, err := BuildLogsQL(nil, nil, nil, nil, `a"b\c|d*e`)
	if err != nil {
		t.Fatalf("BuildLogsQL sample: %v", err)
	}
	if q != "* \"a\\\"b\\\\c|d*e\"" {
		t.Fatalf("query = %q, want the match-all prefix plus the single escaped phrase", q)
	}
}

// TestBuildLogsQLFilters 白名单与组合渲染。
func TestBuildLogsQLFilters(t *testing.T) {
	q, err := BuildLogsQL([]string{"demo", "shop"}, []string{"web"}, []string{"container"}, nil, "boom")
	if err != nil {
		t.Fatalf("BuildLogsQL: %v", err)
	}
	if q != `{app=~"^(demo|shop)$",service=~"^(web)$",source=~"^(container)$"} "boom"` {
		t.Fatalf("query = %q", q)
	}
	// 全空 = match-all（时间窗由 start/end 参数承载）。
	if q, err := BuildLogsQL(nil, nil, nil, nil, ""); err != nil || q != "*" {
		t.Fatalf("match-all query = %q err=%v, want *", q, err)
	}
	// 白名单负向：服务名/应用名/来源越界拒绝（ErrBadQuery 哨兵）。S2 起
	// access 进入来源词表（与采集面同拍放行），越界样本改用未放行值。
	for _, tc := range []struct {
		apps, services, sources []string
	}{
		{apps: []string{"Bad_Name"}},
		{apps: []string{"ok"}, services: []string{"a b"}},
		{apps: []string{"ok"}, services: []string{"ok"}, sources: []string{"network"}},
		{sources: []string{"container; drop"}},
	} {
		if _, err := BuildLogsQL(tc.apps, tc.services, tc.sources, nil, ""); !errors.Is(err, ErrBadQuery) {
			t.Errorf("BuildLogsQL(%v,%v,%v) err = %v, want ErrBadQuery", tc.apps, tc.services, tc.sources, err)
		}
	}
	// source=access 已入词表（W5-S2 访问日志采集同拍放行）。
	q, err = BuildLogsQL([]string{"demo"}, nil, []string{"access"}, nil, "")
	if err != nil {
		t.Fatalf("BuildLogsQL access source: %v", err)
	}
	if q != `{app=~"^(demo)$",source=~"^(access)$"}` {
		t.Fatalf("access filter query = %q", q)
	}
}

// TestParseLogRows 行流解析（字段形状 = VL 实测回读形态：布尔字段以
// 字符串回读；_time RFC3339）。
func TestParseLogRows(t *testing.T) {
	stream := `{"_msg":"first","_time":"2026-09-21T10:00:00Z","app":"demo","service":"web","source":"container","stderr":"false"}` + "\n" +
		"not-json-garbage\n" +
		`{"_msg":"second","_time":"2026-09-21T10:00:01Z","app":"demo","service":"web","source":"container","stderr":"true"}` + "\n"
	rows, err := parseLogRows(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseLogRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (garbage line skipped)", len(rows))
	}
	if rows[0].Msg != "first" || rows[0].App != "demo" || rows[0].Source != "container" || rows[0].Stderr {
		t.Fatalf("row0 = %+v", rows[0])
	}
	if !rows[1].Stderr {
		t.Fatalf("row1 stderr = false, want true (string form accepted)")
	}
	if rows[0].At.IsZero() || rows[0].At.Year() != 2026 {
		t.Fatalf("row0 at = %v, want parsed RFC3339", rows[0].At)
	}
}

// TestIngestBulkPayload ES bulk 入湖契约：POST 路径带 _stream_fields、
// 每行前置 {"create":{}} 动作行、行字段 _time/_msg/app/service/source/
// stderr；2xx 视为成功。
func TestIngestBulkPayload(t *testing.T) {
	var gotPath, gotBody string
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"took":1,"errors":false,"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	b := NewBackend()
	b.base = srv.URL

	entries := []logs.Entry{
		{App: "demo", Service: "web", Source: "container", Stderr: true,
			Line: `secret *** ok`, At: atUTC(t, 0)},
	}
	if err := b.IngestBulk(context.Background(), entries); err != nil {
		t.Fatalf("IngestBulk: %v", err)
	}
	if !strings.HasPrefix(gotPath, BulkIngestPath+"?") || !strings.Contains(gotPath, "_stream_fields=app%2Cservice%2Csource") {
		t.Fatalf("request path = %q, want %s with _stream_fields=app,service,source", gotPath, BulkIngestPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content type = %q", gotContentType)
	}
	lines := strings.Split(strings.TrimRight(gotBody, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("payload lines = %d, want 2 (action + row)", len(lines))
	}
	if strings.TrimSpace(lines[0]) != `{"create":{}}` {
		t.Fatalf("action line = %q", lines[0])
	}
	for _, want := range []string{`"_time"`, `"_msg":"secret *** ok"`, `"app":"demo"`, `"service":"web"`, `"source":"container"`, `"stderr":true`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row %s missing %s", lines[1], want)
		}
	}
}

// TestIngestBulkAccessFields W5-S2：访问行的结构化 Fields 展开为入湖行
// 顶层字段（白名单内）；白名单外键不入湖；读侧 parseLogRows 按同一词表
// 回读 Fields（container 行 Fields 恒空——往返闭环钉住）。
func TestIngestBulkAccessFields(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	b := NewBackend()
	b.base = srv.URL

	at := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	entries := []logs.Entry{
		{App: "demo", Service: "web", Source: logs.SourceAccess, Line: "GET 200 h / 1ms", At: at,
			Fields: map[string]string{
				"method": "GET", "status": "200", "host": "h", "path": "/",
				"route": "fleetly-demo-web-web@http", "duration_ms": "1",
				"client_ip": "10.0.0.1", "deployment_id": "dep-1",
				"rogue_key": "drop-me", // 白名单外——不入湖
			}},
	}
	if err := b.IngestBulk(context.Background(), entries); err != nil {
		t.Fatalf("IngestBulk: %v", err)
	}
	lines := strings.Split(strings.TrimRight(gotBody, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("payload lines = %d, want 2", len(lines))
	}
	for _, want := range []string{
		`"method":"GET"`, `"status":"200"`, `"host":"h"`, `"path":"/"`,
		`"route":"fleetly-demo-web-web@http"`, `"duration_ms":"1"`,
		`"client_ip":"10.0.0.1"`, `"deployment_id":"dep-1"`,
	} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("access row %s missing %s", lines[1], want)
		}
	}
	if strings.Contains(lines[1], "rogue_key") {
		t.Errorf("non-whitelisted key leaked into the bulk row: %s", lines[1])
	}
	// 读侧回读：行流 JSON（与入湖行同形）→ LogRow.Fields 按词表取回。
	rowJSON := `{"_time":"2026-09-21T08:00:00Z","_msg":"GET 200 h / 1ms","app":"demo","service":"web","source":"access",` +
		`"method":"GET","status":"200","host":"h","path":"/","route":"fleetly-demo-web-web@http",` +
		`"duration_ms":"1","client_ip":"10.0.0.1","deployment_id":"dep-1","rogue_key":"drop-me"}` + "\n" +
		`{"_time":"2026-09-21T08:00:01Z","_msg":"plain","app":"demo","service":"web","source":"container"}` + "\n"
	rows, err := parseLogRows(strings.NewReader(rowJSON))
	if err != nil {
		t.Fatalf("parseLogRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Fields["deployment_id"] != "dep-1" || rows[0].Fields["method"] != "GET" {
		t.Fatalf("access row fields = %v", rows[0].Fields)
	}
	if _, ok := rows[0].Fields["rogue_key"]; ok {
		t.Fatal("non-whitelisted key must not be read back")
	}
	if rows[1].Fields != nil {
		t.Fatalf("container row fields = %v, want nil", rows[1].Fields)
	}
}

// TestIngestBulkEmptyNoop 空批零请求（防呆——不发空 bulk）。
func TestIngestBulkEmptyNoop(t *testing.T) {	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	b := NewBackend()
	b.base = srv.URL
	if err := b.IngestBulk(context.Background(), nil); err != nil {
		t.Fatalf("IngestBulk(nil): %v", err)
	}
	if called {
		t.Fatal("empty batch must not issue a request")
	}
}

// TestSearchBackendUnavailable 非常响应/不可达 → 原生错误返回（api 层
// 映射 E_LOGS_BACKEND_UNAVAILABLE 的上游契约）。
func TestSearchBackendUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("storage is not ready"))
	}))
	t.Cleanup(srv.Close)
	b := NewBackend()
	b.base = srv.URL
	_, err := b.Search(context.Background(), SearchQuery{Limit: 10})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Search err = %v, want upstream status error", err)
	}
	// 不可达形态（连接拒绝）。
	b2 := NewBackend()
	b2.base = "http://127.0.0.1:1"
	if _, err := b2.Search(context.Background(), SearchQuery{Limit: 10}); err == nil {
		t.Fatal("Search on unreachable backend must fail")
	}
}

// TestSearchQueryParams 查询参数契约（query/limit/offset/start/end/
// timeout + limit+1 has-more 预算）。
func TestSearchQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotQuery = r.Form.Encode()
		_, _ = w.Write([]byte(""))
	}))
	t.Cleanup(srv.Close)
	b := NewBackend()
	b.base = srv.URL
	rows, err := b.Search(context.Background(), SearchQuery{
		Apps: []string{"demo"}, Keyword: "boom", Limit: 10, Offset: 30,
		Start: atUTC(t, 1000), End: atUTC(t, 2000),
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
	for _, want := range []string{
		"query=%7Bapp",            // {app=…} 流过滤
		"limit=11",                // limit+1 has-more 预算
		"offset=30",               // 分页偏移
		"start=", "end=",          // 时间窗
		"timeout=10s",             // 执行预算缺省
		"hello=never",             // 不存在的键必不存在（防手拼泄漏）
	} {
		if !strings.Contains(gotQuery, want) {
			if want == "hello=never" {
				continue // 哨兵负向：不含即通过
			}
			t.Errorf("form = %q missing %s", gotQuery, want)
		}
	}
	if strings.Contains(gotQuery, "hello=never") {
		t.Fatalf("form = %q contains sentinel", gotQuery)
	}
}
