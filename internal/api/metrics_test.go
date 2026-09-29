package api

// MetricsService 的 W5-S3 单测（E6 观测专项设计 §4.1/§4.2，D-W5-2 opt-in）：
//   - 查询诚实边界：mode=unset → E_METRICS_NOT_ENABLED（不返回空序列冒充
//     有数）；VM 不可达 → E_METRICS_BACKEND_UNAVAILABLE；坏 PromQL →
//     退化信封 InvalidArgument（VM 原文透传）；
//   - 状态视图：nodes_reporting 计算（VM 计数 + 观测缓存分母）、retention
//     对齐、三件部署态投影；
//   - 模式切换：SaveMetricsSettings 同事务路径 + 切换后视图；
//   - scope 矩阵登记（read/read/deploy）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/metrics"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// openMetricsStore 起独立临时库（含平台节点 meta——VM spec 收敛路径
// 不被本测触达，meta 只为完整性）。
func openMetricsStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "metricsapi.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// vmQueryServer 是 VM Prometheus API 的假后端（query_range/query 形态分派）。
func vmQueryServer(t *testing.T, rangeBody, instantBody string, instantCalls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case metrics.QueryRangePath:
			_, _ = w.Write([]byte(rangeBody))
		case metrics.QueryInstantPath:
			if instantCalls != nil {
				*instantCalls++
			}
			_, _ = w.Write([]byte(instantBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestSearchMetricsNotEnabled mode=unset → E_METRICS_NOT_ENABLED（409 族
// ——opt-in 查询面的诚实报错；且绝不触达 VM）。
func TestSearchMetricsNotEnabled(t *testing.T) {
	st := openMetricsStore(t)
	svc := NewMetricsService(st).WithBackend(metrics.NewBackendWithBase("http://127.0.0.1:1"), nil)
	_, err := svc.SearchMetrics(context.Background(), &serverv1.SearchMetricsRequest{Query: "up"})
	assertApperrCode(t, err, "E_METRICS_NOT_ENABLED")
}

// TestSearchMetricsBackendUnavailableAndBadQuery mode=on 的两处后端分支：
// VM 不可达 → E_METRICS_BACKEND_UNAVAILABLE；坏 PromQL（VM 4xx）→ 退化
// 信封 InvalidArgument 且错误文本带 VM 原文。
func TestSearchMetricsBackendUnavailableAndBadQuery(t *testing.T) {
	st := openMetricsStore(t)
	if err := st.SaveMetricsSettings(context.Background(), state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save mode on: %v", err)
	}

	// 不可达。
	svc := NewMetricsService(st).WithBackend(metrics.NewBackendWithBase("http://127.0.0.1:1"), nil)
	_, err := svc.SearchMetrics(context.Background(), &serverv1.SearchMetricsRequest{Query: "up"})
	assertApperrCode(t, err, "E_METRICS_BACKEND_UNAVAILABLE")

	// 坏查询：VM 4xx 原文透传为 InvalidArgument。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","error":"parse error: unexpected }"}`))
	}))
	defer bad.Close()
	svc2 := NewMetricsService(st).WithBackend(metrics.NewBackendWithBase(bad.URL), nil)
	_, err = svc2.SearchMetrics(context.Background(), &serverv1.SearchMetricsRequest{Query: "up{"})
	if err == nil {
		t.Fatal("bad promql must fail")
	}
	if stErr, ok := status.FromError(err); !ok || stErr.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", err)
	} else if !strings.Contains(stErr.Message(), "parse error: unexpected }") {
		t.Fatalf("message = %q, want VM original text", stErr.Message())
	}
}

// TestSearchMetricsPassthroughWindow 透传 + 窗口参数化：query 原样到达 VM
//（含花括号/正则），start/end 投影为 unix 秒。
func TestSearchMetricsPassthroughWindow(t *testing.T) {
	st := openMetricsStore(t)
	if err := st.SaveMetricsSettings(context.Background(), state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save mode on: %v", err)
	}
	var gotQuery, gotStart, gotEnd, gotStep string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery, gotStart, gotEnd, gotStep = q.Get("query"), q.Get("start"), q.Get("end"), q.Get("step")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()
	svc := NewMetricsService(st).WithBackend(metrics.NewBackendWithBase(srv.URL), nil)

	start := time.Unix(1700000000, 0)
	end := time.Unix(1700000600, 0)
	promql := `sum by (container_label_com_docker_swarm_service_name) (container_memory_usage_bytes{container_label_com_docker_swarm_service_name=~"metricsapp-.*"})`
	resp, err := svc.SearchMetrics(context.Background(), &serverv1.SearchMetricsRequest{
		Query: promql, StepSeconds: 30,
		TimeStart: timestamppb.New(start), TimeEnd: timestamppb.New(end),
	})
	if err != nil {
		t.Fatalf("SearchMetrics: %v", err)
	}
	if len(resp.GetSeries()) != 0 {
		t.Fatalf("series = %+v, want empty", resp.GetSeries())
	}
	if gotQuery != promql {
		t.Fatalf("query not passed verbatim:\n got %q\nwant %q", gotQuery, promql)
	}
	if gotStart != "1700000000" || gotEnd != "1700000600" || gotStep != "30" {
		t.Fatalf("window params = %s/%s/%s", gotStart, gotEnd, gotStep)
	}
}

// TestGetMetricsStatusHonestCounts 状态视图：nodes_reporting = VM up 计数
//（不可达 = 0，不谎报）、nodes_total = 观测缓存分母、retention 注入对齐、
// 三件部署态投影。
func TestGetMetricsStatusHonestCounts(t *testing.T) {
	st := openMetricsStore(t)
	if err := st.SaveMetricsSettings(context.Background(), state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save mode on: %v", err)
	}
	var instantCalls int
	srv := vmQueryServer(t, "", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"1"]}]}}`, &instantCalls)
	defer srv.Close()

	mb := metrics.NewBackendWithBase(srv.URL)
	// mm = nil（管理器未装配——部署态缺省；本测聚焦计数面）。
	svc := NewMetricsService(st).WithBackend(mb, nil).
		WithRetentionDays(21).
		WithNodesTotal(func(context.Context) (int, error) { return 3, nil })

	resp, err := svc.GetMetricsStatus(context.Background(), &serverv1.GetMetricsStatusRequest{})
	if err != nil {
		t.Fatalf("GetMetricsStatus: %v", err)
	}
	if !resp.GetModeSet() || resp.GetMode() != state.MetricsModeOn {
		t.Fatalf("mode = %s/%v, want on/true", resp.GetMode(), resp.GetModeSet())
	}
	if resp.GetNodesReporting() != 1 || resp.GetNodesTotal() != 3 {
		t.Fatalf("nodes = %d/%d, want 1/3 (honest N/M)", resp.GetNodesReporting(), resp.GetNodesTotal())
	}
	if resp.GetRetentionDays() != 21 {
		t.Fatalf("retention = %d, want 21 (config-aligned)", resp.GetRetentionDays())
	}
	if instantCalls == 0 {
		t.Fatal("nodes_reporting must query the VM instant endpoint")
	}
}

// TestGetMetricsStatusVMUnreachableReportingZero mode=on 但 VM 不可达 →
// nodes_reporting=0（配合部署态解读，不谎报；status 本身不失败——部署态
// 来自底座实况）。
func TestGetMetricsStatusVMUnreachableReportingZero(t *testing.T) {
	st := openMetricsStore(t)
	if err := st.SaveMetricsSettings(context.Background(), state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save mode on: %v", err)
	}
	svc := NewMetricsService(st).WithBackend(metrics.NewBackendWithBase("http://127.0.0.1:1"), nil).
		WithNodesTotal(func(context.Context) (int, error) { return 1, nil })
	resp, err := svc.GetMetricsStatus(context.Background(), &serverv1.GetMetricsStatusRequest{})
	if err != nil {
		t.Fatalf("GetMetricsStatus must not fail on VM unreachable: %v", err)
	}
	if resp.GetNodesReporting() != 0 {
		t.Fatalf("nodes_reporting = %d, want 0 (unreachable)", resp.GetNodesReporting())
	}
}

// TestSetMetricsModeRoundTrip 模式切换：set 走 SaveMetricsSettings 同事务
// 路径；切换后状态视图即时反映（事件/审计断言在 state 层 metricssettings_
// test——此处测 api 投影面）。
func TestSetMetricsModeRoundTrip(t *testing.T) {
	st := openMetricsStore(t)
	svc := NewMetricsService(st)
	// directCtx = 机具 admin 等效 principal（W3-S2 平台写面门：直调夹具的
	// 授权形态，harness 惯例）。
	resp, err := svc.SetMetricsMode(directCtx(context.Background()), &serverv1.SetMetricsModeRequest{Mode: state.MetricsModeOn})
	if err != nil {
		t.Fatalf("SetMetricsMode(on): %v", err)
	}
	if resp.GetStatus().GetMode() != state.MetricsModeOn || !resp.GetStatus().GetModeSet() {
		t.Fatalf("status = %+v, want on/set", resp.GetStatus())
	}
	// 非法值被 state 层校验拒绝（fail-closed）。
	if _, err := svc.SetMetricsMode(directCtx(context.Background()), &serverv1.SetMetricsModeRequest{Mode: "prometheus"}); err == nil {
		t.Fatal("invalid mode must fail")
	}
}

// TestMetricsScopeMatrix scope 矩阵登记（fail-closed 未注册=admin 的机制
// 下，三方法必须显式登记——漏登即静默提权到 admin）。
func TestMetricsScopeMatrix(t *testing.T) {
	for method, want := range map[string]string{
		"/fleetly.server.v1.MetricsService/SearchMetrics":    ScopeRead,
		"/fleetly.server.v1.MetricsService/GetMetricsStatus": ScopeRead,
		"/fleetly.server.v1.MetricsService/SetMetricsMode":   ScopeDeploy,
	} {
		got, ok := RequiredScope(method)
		if !ok || got != want {
			t.Errorf("scope(%s) = %s,%v; want %s,true", method, got, ok, want)
		}
	}
}
