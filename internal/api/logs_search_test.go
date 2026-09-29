package api

// LogsService 的 W5-S1 扩展单测（E6 观测专项设计 §3.1/§2.2/§2.3）：
//   - SearchLogs 诚实边界：backend=jsonl 与面未装配两分支同码
//     E_LOGS_BACKEND_UNAVAILABLE（不返回空列表冒充——设计 §3.1）；
//   - 检索命中投影与游标分页（页签发 next_cursor、偏移推进、非法游标
//     InvalidArgument）；
//   - LogsBackend 视图（缺省模式投影 / 部署态映射）与切换；
//   - scope 矩阵登记（read/read/deploy）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/victorialogs"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// openLogsStore 起独立临时库。
func openLogsStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "logssearch.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newLogsSvc 构造 LogsService（logs.Manager 未注入——本测面只触达
// SearchLogs/Backend 视图；丢弃计数在 nil 管线态恒 0）。
func newLogsSvc(st *state.Store, vl *victorialogs.Backend, vm *victorialogs.Manager) *LogsService {
	return NewLogsService(st, nil).WithVictorialogs(vl, vm)
}

// discardTestLogger 是静默测试日志器。
func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// assertApperrCode 断言错误信封携带指定注册码。
func assertApperrCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %s", code)
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != code {
		t.Fatalf("err = %v, want %s envelope", err, code)
	}
}

// fakeVLManager 是 victorialogs dockerPort 端口的最小假件（按方法集
// 满足——部署态映射测试用；未导出接口不阻挡外部实现）。
type fakeVLManager struct {
	exists bool
}

func (f *fakeVLManager) Info(_ context.Context) (dockerapi.InfoSnapshot, error) {
	return dockerapi.InfoSnapshot{SwarmActive: true}, nil
}

func (f *fakeVLManager) ServiceInspect(_ context.Context, _ string) (dockerapi.ServiceSnapshot, error) {
	return dockerapi.ServiceSnapshot{Exists: f.exists, Image: "pinned"}, nil
}

func (f *fakeVLManager) ServiceCreate(_ context.Context, _ swarm.ServiceSpec) error {
	f.exists = true
	return nil
}

func (f *fakeVLManager) ServiceUpdate(_ context.Context, _ string, _ uint64, _ swarm.ServiceSpec) error {
	return nil
}

func (f *fakeVLManager) ServiceRemove(_ context.Context, _ string) error {
	f.exists = false
	return nil
}

func (f *fakeVLManager) VolumeEnsure(_ context.Context, _ string) error { return nil }

// NetworkName 恒等透传（W5-S3 门上 dockerPort 增面——收敛层网络目标反解；
// 本假件不覆盖该路径）。
func (f *fakeVLManager) NetworkName(_ context.Context, target string) (string, error) {
	return target, nil
}

// TestSearchLogsJSONLModeSameCode jsonl 模式 → E_LOGS_BACKEND_UNAVAILABLE
//（诚实：不支持，不返回空列表冒充——设计 §3.1）。
func TestSearchLogsJSONLModeSameCode(t *testing.T) {
	st := openLogsStore(t)
	ctx := context.Background()
	if err := st.SaveLogsSettings(ctx, state.LogsBackendJSONL, state.LogsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := testsupport.SeedAppE(t, st, "app"); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	svc := newLogsSvc(st, victorialogs.NewBackend(), nil)
	_, err := svc.SearchLogs(directCtx(ctx), &serverv1.SearchLogsRequest{App: "app", Keyword: "boom"})
	assertApperrCode(t, err, "E_LOGS_BACKEND_UNAVAILABLE")
}

// TestSearchLogsFaceNotAssembled 面未装配（nil backend）→ 同码。
func TestSearchLogsFaceNotAssembled(t *testing.T) {
	st := openLogsStore(t)
	ctx := context.Background()
	if _, err := testsupport.SeedAppE(t, st, "app"); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	svc := newLogsSvc(st, nil, nil)
	_, err := svc.SearchLogs(directCtx(ctx), &serverv1.SearchLogsRequest{App: "app"})
	assertApperrCode(t, err, "E_LOGS_BACKEND_UNAVAILABLE")
}

// TestSearchLogsVLUnreachableSameCode VL 不可达 → 同码（检索降级分支）。
func TestSearchLogsVLUnreachableSameCode(t *testing.T) {
	st := openLogsStore(t)
	ctx := context.Background()
	if _, err := testsupport.SeedAppE(t, st, "app"); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	vl := victorialogs.NewBackendWithBase("http://127.0.0.1:1")
	svc := newLogsSvc(st, vl, nil)
	_, err := svc.SearchLogs(directCtx(ctx), &serverv1.SearchLogsRequest{App: "app"})
	assertApperrCode(t, err, "E_LOGS_BACKEND_UNAVAILABLE")
}

// TestSearchLogsRowsAndCursor 命中投影（msg/stderr/维度字段）+ 游标分页
//（首页签发 next_cursor；游标解码为页偏移）。
func TestSearchLogsRowsAndCursor(t *testing.T) {
	st := openLogsStore(t)
	ctx := context.Background()
	if _, err := testsupport.SeedAppE(t, st, "app"); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		offset := 0
		_, _ = fmt.Sscanf(r.Form.Get("offset"), "%d", &offset)
		budget := 11
		_, _ = fmt.Sscanf(r.Form.Get("limit"), "%d", &budget)
		w.Header().Set("Content-Type", "application/x-ndjson")
		// VL 语义模拟：至多返回 limit 预算（含 +1 has-more 探测）内的
		// 行；全集 5 行。首页（offset=0，budget=5）返回 5 行 = limit(4)+1
		// → has-more 截 4 行并签发 cursor=4；次页（offset=4）返回 1 行
		// < 预算 → 无 cursor。
		for i := offset; i < 5 && i < offset+budget; i++ {
			stderr := "false"
			if i%2 == 1 {
				stderr = "true"
			}
			_, _ = fmt.Fprintf(w, "{\"_msg\":\"row-%d\",\"_time\":\"2026-09-21T10:00:00Z\",\"app\":\"app\",\"service\":\"web\",\"source\":\"container\",\"stderr\":%q}\n", i, stderr)
		}
	}))
	t.Cleanup(srv.Close)
	vl := victorialogs.NewBackendWithBase(srv.URL)

	svc := newLogsSvc(st, vl, nil)
	resp, err := svc.SearchLogs(directCtx(ctx), &serverv1.SearchLogsRequest{App: "app", Limit: 4})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(resp.GetRows()) != 4 || resp.GetRows()[0].GetMsg() != "row-0" || !resp.GetRows()[1].GetStderr() {
		t.Fatalf("rows = %+v", resp.GetRows())
	}
	if resp.GetNextCursor() == "" {
		t.Fatal("has-more page should carry next_cursor")
	}
	offset, err := decodeSearchCursor(resp.GetNextCursor())
	if err != nil || offset != 4 {
		t.Fatalf("cursor offset = %d err=%v, want 4", offset, err)
	}
	// 末页（offset=4 返回 1 行 < 预算 → has-more=false，无 cursor）。
	resp2, err := svc.SearchLogs(directCtx(ctx), &serverv1.SearchLogsRequest{App: "app", Limit: 4, Cursor: resp.GetNextCursor()})
	if err != nil {
		t.Fatalf("SearchLogs page2: %v", err)
	}
	if len(resp2.GetRows()) != 1 || resp2.GetNextCursor() != "" {
		t.Fatalf("page2 rows = %d cursor = %q, want 1 row and empty cursor", len(resp2.GetRows()), resp2.GetNextCursor())
	}
}

// TestSearchLogsCursorInvalid 非法游标 → InvalidArgument（不猜不将就）。
func TestSearchLogsCursorInvalid(t *testing.T) {
	st := openLogsStore(t)
	ctx := context.Background()
	if _, err := testsupport.SeedAppE(t, st, "app"); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	vl := victorialogs.NewBackendWithBase(srv.URL)
	svc := newLogsSvc(st, vl, nil)
	_, err := svc.SearchLogs(directCtx(ctx), &serverv1.SearchLogsRequest{App: "app", Cursor: "not-a-cursor!!"})
	if err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("err = %v, want invalid cursor", err)
	}
}

// TestSearchLogsAccessFieldsProjected W5-S2：访问行的结构化字段透传
//（白名单内回读、白名单外不透传——入湖/读侧双保险的第二道）。
func TestSearchLogsAccessFieldsProjected(t *testing.T) {
	st := openLogsStore(t)
	ctx := context.Background()
	if _, err := testsupport.SeedAppE(t, st, "app"); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprint(w, `{"_msg":"GET 200 h / 1ms","_time":"2026-09-21T10:00:00Z","app":"app","service":"web","source":"access",`+
			`"method":"GET","status":"200","route":"fleetly-app-web-websecure@http","deployment_id":"dep-9","rogue":"x"}`+"\n")
	}))
	t.Cleanup(srv.Close)
	vl := victorialogs.NewBackendWithBase(srv.URL)
	svc := newLogsSvc(st, vl, nil)

	resp, err := svc.SearchLogs(directCtx(ctx), &serverv1.SearchLogsRequest{App: "app", Sources: []string{"access"}})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(resp.GetRows()) != 1 {
		t.Fatalf("rows = %d, want 1", len(resp.GetRows()))
	}
	fields := resp.GetRows()[0].GetFields()
	if fields["method"] != "GET" || fields["status"] != "200" ||
		fields["route"] != "fleetly-app-web-websecure@http" || fields["deployment_id"] != "dep-9" {
		t.Fatalf("fields = %v", fields)
	}
	if _, ok := fields["rogue"]; ok {
		t.Fatal("non-whitelisted field must not be projected to the consumer")
	}
}

// TestGetSetLogsBackend 视图与切换：缺省视图（victorialogs/未显式设置/
// 部署态 pending〔服务缺失〕）→ set jsonl（removed）→ 服务在位 deployed。
func TestGetSetLogsBackend(t *testing.T) {
	st := openLogsStore(t)
	ctx := context.Background()
	fk := &fakeVLManager{}
	vm := victorialogs.NewManagerWithDocker(st, 7, fk, discardTestLogger())
	svc := newLogsSvc(st, nil, vm)

	resp, err := svc.GetLogsBackend(ctx, &serverv1.GetLogsBackendRequest{})
	if err != nil {
		t.Fatalf("GetLogsBackend: %v", err)
	}
	v := resp.GetView()
	if v.GetBackend() != state.LogsBackendVictorialogs || v.GetBackendSet() {
		t.Fatalf("view = %+v, want default victorialogs unset", v)
	}
	if v.GetDeployment() != logsBackendDeploymentPending {
		t.Fatalf("deployment = %q, want pending (service missing)", v.GetDeployment())
	}

	setResp, err := svc.SetLogsBackend(directCtx(ctx), &serverv1.SetLogsBackendRequest{Backend: state.LogsBackendJSONL})
	if err != nil {
		t.Fatalf("SetLogsBackend: %v", err)
	}
	if got := setResp.GetView().GetBackend(); got != state.LogsBackendJSONL {
		t.Fatalf("backend after set = %q", got)
	}
	resp2, _ := svc.GetLogsBackend(ctx, &serverv1.GetLogsBackendRequest{})
	if resp2.GetView().GetDeployment() != logsBackendDeploymentRemoved {
		t.Fatalf("deployment = %q, want removed", resp2.GetView().GetDeployment())
	}
	fk.exists = true
	if _, err := svc.SetLogsBackend(directCtx(ctx), &serverv1.SetLogsBackendRequest{Backend: state.LogsBackendVictorialogs}); err != nil {
		t.Fatalf("set back: %v", err)
	}
	resp3, _ := svc.GetLogsBackend(ctx, &serverv1.GetLogsBackendRequest{})
	if resp3.GetView().GetDeployment() != logsBackendDeploymentDeployed {
		t.Fatalf("deployment = %q, want deployed", resp3.GetView().GetDeployment())
	}
}

// TestLogsBackendScopes scope 矩阵登记（未登记即 admin fail-closed——
// 显式登记面单测）。
func TestLogsBackendScopes(t *testing.T) {
	for method, want := range map[string]string{
		"/fleetly.server.v1.LogsService/SearchLogs":     ScopeRead,
		"/fleetly.server.v1.LogsService/GetLogsBackend": ScopeRead,
		"/fleetly.server.v1.LogsService/SetLogsBackend": ScopeDeploy,
	} {
		if got, ok := RequiredScope(method); !ok || got != want {
			t.Errorf("scope(%s) = %q ok=%v, want %q", method, got, ok, want)
		}
	}
}
