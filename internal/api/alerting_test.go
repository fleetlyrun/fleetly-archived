package api

// AlertingService 的 W5-S2 单测（B 线 W5 设计 §2，D-V3W5-1）：
//   - 门矩阵：读面（List/Status）任意已认证 read；写面（Create/Update/
//     Delete/SetAlertsMode/TestAlertRule）非平台管理员用户 PAT 403
//     （requirePlatformWriteFace——凭据显式声明 admin，排除 scope 门干扰）、
//     机具 admin 与平台管理员放行；
//   - alerts.mode 前置门 409（E_ALERTS_METRICS_REQUIRED——state 层信封透传）；
//   - CRUD 往返 + 平台级唯一名 409 投影；
//   - TestAlertRule 求值往返（VM instant query 假后端）；
//   - GetAlertsStatus 投影（mode/vmalert 态/规则数/metrics.mode 并列）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/metrics"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// alertingEnv 是告警面门测试夹具（pwfEnv 同型：bufconn + 三种凭据）。
type alertingEnv struct {
	st         *state.Store
	conn       *grpc.ClientConn
	tokUser    string // 非平台管理员用户 PAT（声明 admin scope）
	tokRoot    string // 平台管理员用户 PAT（声明 admin scope）
	tokMachine string // 机具 admin 令牌
}

func newAlertingEnv(t *testing.T) *alertingEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key")); err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	uRoot := mustUser(t, st, "alert-root@example.com")
	uPlain := mustUser(t, st, "alert-plain@example.com")

	env := &alertingEnv{st: st}
	env.tokRoot = seedUserPAT(t, st, uRoot.ID, ScopeAdmin)
	env.tokUser = seedUserPAT(t, st, uPlain.ID, ScopeAdmin)
	env.tokMachine = seedTokenPlain(t, st, ScopeAdmin)

	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterAlertingServiceServer(srv, NewAlertingService(st))
	env.conn = serveBufconn(t, srv)
	return env
}

// TestAlertingServiceGateMatrix 门矩阵：写面 × 三种凭据；读面任意已认证。
func TestAlertingServiceGateMatrix(t *testing.T) {
	env := newAlertingEnv(t)
	ctx := context.Background()
	client := serverv1.NewAlertingServiceClient(env.conn)

	// 非 platform-admin 用户 PAT：全部写面 403（凭据声明 admin——门在平台面）。
	for name, call := range map[string]func() error{
		"CreateAlertRule": func() error {
			_, err := client.CreateAlertRule(authCtx(ctx, env.tokUser), &serverv1.CreateAlertRuleRequest{Name: "r1", Expr: "up == 0"})
			return err
		},
		"UpdateAlertRule": func() error {
			_, err := client.UpdateAlertRule(authCtx(ctx, env.tokUser), &serverv1.UpdateAlertRuleRequest{Id: "01TEST"})
			return err
		},
		"DeleteAlertRule": func() error {
			_, err := client.DeleteAlertRule(authCtx(ctx, env.tokUser), &serverv1.DeleteAlertRuleRequest{Id: "01TEST"})
			return err
		},
		"SetAlertsMode": func() error {
			_, err := client.SetAlertsMode(authCtx(ctx, env.tokUser), &serverv1.SetAlertsModeRequest{Mode: state.AlertsModeOn})
			return err
		},
		"TestAlertRule": func() error {
			_, err := client.TestAlertRule(authCtx(ctx, env.tokUser), &serverv1.TestAlertRuleRequest{Expr: "up == 0"})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s with non-platform-admin user PAT err = %v, want PermissionDenied", name, err)
		}
	}

	// 读面任意已认证 read（非平台管理员用户 PAT）200。
	if _, err := client.ListAlertRules(authCtx(ctx, env.tokUser), &serverv1.ListAlertRulesRequest{}); err != nil {
		t.Fatalf("read face ListAlertRules must stay open: %v", err)
	}
	if _, err := client.GetAlertsStatus(authCtx(ctx, env.tokUser), &serverv1.GetAlertsStatusRequest{}); err != nil {
		t.Fatalf("read face GetAlertsStatus must stay open: %v", err)
	}

	// 机具 admin：SetAlertsMode 触达前置门（metrics off → 409
	// E_ALERTS_METRICS_REQUIRED）——证明 scope 门放行、门在业务语义层。
	_, err := client.SetAlertsMode(authCtx(ctx, env.tokMachine), &serverv1.SetAlertsModeRequest{Mode: state.AlertsModeOn})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("machine SetAlertsMode without metrics err = %v, want FailedPrecondition(409)", err)
	}
	assertWireApperrCode(t, err, "E_ALERTS_METRICS_REQUIRED")

	// 平台管理员：CRUD 全通（先开 metrics 再开 alerts——前置门放行路径）。
	if err := env.st.SaveMetricsSettings(ctx, state.MetricsModeOn, state.MetricsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("seed metrics on: %v", err)
	}
	if _, err := client.SetAlertsMode(authCtx(ctx, env.tokRoot), &serverv1.SetAlertsModeRequest{Mode: state.AlertsModeOn}); err != nil {
		t.Fatalf("platform admin SetAlertsMode: %v", err)
	}
	created, err := client.CreateAlertRule(authCtx(ctx, env.tokRoot), &serverv1.CreateAlertRuleRequest{
		Name: "high-cpu", Expr: "cpu > 90", ForDurationSeconds: 300,
		Labels: map[string]string{"severity": "critical"}, Channels: []string{"ep-1"},
	})
	if err != nil {
		t.Fatalf("platform admin CreateAlertRule: %v", err)
	}
	if _, err := client.UpdateAlertRule(authCtx(ctx, env.tokRoot), &serverv1.UpdateAlertRuleRequest{
		Id: created.GetRule().GetId(), Expr: protoStr("up == 0"),
	}); err != nil {
		t.Fatalf("platform admin UpdateAlertRule: %v", err)
	}
	if _, err := client.DeleteAlertRule(authCtx(ctx, env.tokRoot), &serverv1.DeleteAlertRuleRequest{Id: created.GetRule().GetId()}); err != nil {
		t.Fatalf("platform admin DeleteAlertRule: %v", err)
	}
}

// TestAlertingServiceCRUDAndConflict CRUD 往返 + 平台级唯一名的 409 投影。
func TestAlertingServiceCRUDAndConflict(t *testing.T) {
	env := newAlertingEnv(t)
	ctx := context.Background()
	client := serverv1.NewAlertingServiceClient(env.conn)

	created, err := client.CreateAlertRule(authCtx(ctx, env.tokMachine), &serverv1.CreateAlertRuleRequest{
		Name: "dup-check", Expr: "up == 0",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.GetRule().GetId() == "" || created.GetRule().GetForDurationSeconds() != 0 {
		t.Fatalf("created = %+v", created.GetRule())
	}
	// 同名再建 → 409 E_ALERT_RULE_NAME_CONFLICT。
	_, err = client.CreateAlertRule(authCtx(ctx, env.tokMachine), &serverv1.CreateAlertRuleRequest{
		Name: "dup-check", Expr: "up == 1",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("conflict err = %v, want FailedPrecondition(409)", err)
	}
	assertWireApperrCode(t, err, "E_ALERT_RULE_NAME_CONFLICT")

	// 清场，避免影响其他用例的规则数投影。
	if _, err := client.DeleteAlertRule(authCtx(ctx, env.tokMachine), &serverv1.DeleteAlertRuleRequest{Id: created.GetRule().GetId()}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// 再删 → 404 E_ALERT_RULE_NOT_FOUND。
	_, err = client.DeleteAlertRule(authCtx(ctx, env.tokMachine), &serverv1.DeleteAlertRuleRequest{Id: created.GetRule().GetId()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("double delete err = %v, want NotFound", err)
	}
	assertWireApperrCode(t, err, "E_ALERT_RULE_NOT_FOUND")
}

// assertWireApperrCode 是 bufconn 线上的信封码断言：detail 经 apperr.FromError
// 还原（errors.As 对线上 status 错误不可达——信封在 detail，不在错误链）。
func assertWireApperrCode(t *testing.T, err error, code string) {
	t.Helper()
	ae, ok := apperr.FromError(err)
	if !ok || ae.Code() != code {
		t.Fatalf("err = %v, want %s envelope over the wire", err, code)
	}
}

// TestTestAlertRuleEvaluationRoundtrip TestAlertRule 求值往返：mode 门 →
// expr 透传到 VM instant query → 样本回投影。
func TestTestAlertRuleEvaluationRoundtrip(t *testing.T) {
	st := newSettingsStoreForAPITest(t)
	if err := st.SaveMetricsSettings(context.Background(), state.MetricsModeOn, state.MetricsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("seed metrics on: %v", err)
	}
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != metrics.QueryInstantPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotQuery = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"__name__":"up","job":"fleetly-cadvisor"},"value":[1790000000,"1"]}]}}`))
	}))
	defer srv.Close()

	svc := NewAlertingService(st).WithBackend(metrics.NewBackendWithBase(srv.URL), nil)

	// metrics off → E_METRICS_NOT_ENABLED（查询面 opt-in 语义沿用；直调面
	// 注入 directCtx——TestAlertRule 的平台写面门对无 principal fail-closed）。
	offStore := newSettingsStoreForAPITest(t)
	offSvc := NewAlertingService(offStore).WithBackend(metrics.NewBackendWithBase(srv.URL), nil)
	_, err := offSvc.TestAlertRule(directCtx(context.Background()), &serverv1.TestAlertRuleRequest{Expr: "up == 0"})
	assertApperrCode(t, err, "E_METRICS_NOT_ENABLED")

	resp, err := svc.TestAlertRule(directCtx(context.Background()), &serverv1.TestAlertRuleRequest{Expr: `up{job="fleetly-cadvisor"} == 0`})
	if err != nil {
		t.Fatalf("TestAlertRule: %v", err)
	}
	if gotQuery != `up{job="fleetly-cadvisor"} == 0` {
		t.Fatalf("query = %q, want passthrough", gotQuery)
	}
	if len(resp.GetSeries()) != 1 || resp.GetSeries()[0].GetPoints()[0].GetV() != 1 {
		t.Fatalf("series = %+v, want one point v=1", resp.GetSeries())
	}

	// 坏 PromQL（VM 4xx）→ InvalidArgument 且带 VM 原文。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","error":"parse error at针织"}`))
	}))
	defer bad.Close()
	badSvc := NewAlertingService(st).WithBackend(metrics.NewBackendWithBase(bad.URL), nil)
	_, err = badSvc.TestAlertRule(directCtx(context.Background()), &serverv1.TestAlertRuleRequest{Expr: "up{"})
	if stErr, ok := status.FromError(err); !ok || stErr.Code() != codes.InvalidArgument {
		t.Fatalf("bad expr err = %v, want InvalidArgument", err)
	} else if !strings.Contains(stErr.Message(), "parse error") {
		t.Fatalf("message = %q, want VM original text", stErr.Message())
	}
}

// TestGetAlertsStatusProjection 状态投影：mode/set/vmalert 部署态（mm 未
// 装配 = exists false 如实报）/规则数/metrics.mode 并列。
func TestGetAlertsStatusProjection(t *testing.T) {
	st := newSettingsStoreForAPITest(t)
	svc := NewAlertingService(st)

	// 缺省态。
	resp, err := svc.GetAlertsStatus(context.Background(), &serverv1.GetAlertsStatusRequest{})
	if err != nil {
		t.Fatalf("GetAlertsStatus: %v", err)
	}
	if resp.GetMode() != state.AlertsModeUnset || resp.GetModeSet() {
		t.Fatalf("default status = %+v, want unset/set=false", resp)
	}
	if resp.GetMetricsMode() != state.MetricsModeUnset || resp.GetRuleCount() != 0 {
		t.Fatalf("default metrics/rule projection = %+v", resp)
	}

	// on + 一条规则。
	if err := st.SaveMetricsSettings(context.Background(), state.MetricsModeOn, state.MetricsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("seed metrics on: %v", err)
	}
	if err := st.SaveAlertsSettings(context.Background(), state.AlertsModeOn, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("seed alerts on: %v", err)
	}
	if _, err := st.CreateAlertRule(context.Background(), state.AlertRuleWrite{Name: "r", Expr: "up == 0"}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	resp, err = svc.GetAlertsStatus(context.Background(), &serverv1.GetAlertsStatusRequest{})
	if err != nil {
		t.Fatalf("GetAlertsStatus 2: %v", err)
	}
	if resp.GetMode() != state.AlertsModeOn || !resp.GetModeSet() || resp.GetRuleCount() != 1 {
		t.Fatalf("status = %+v, want on/set=true/rules=1", resp)
	}
	if resp.GetMetricsMode() != state.MetricsModeOn {
		t.Fatalf("metrics_mode = %q, want on", resp.GetMetricsMode())
	}
	if resp.GetVmalertExists() {
		t.Fatal("vmalert must report absent when the manager is not assembled")
	}
}

// protoStr 是 optional string 字段的测试构造。
func protoStr(v string) *string { return &v }
