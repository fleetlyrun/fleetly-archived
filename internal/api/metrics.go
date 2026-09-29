package api

import (
	"context"
	"errors"
	"strings"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/metrics"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// MetricsService 实现 server.v1.MetricsService（E6 观测专项设计 §4，W5-S3；
// D-W5-2 opt-in）：PromQL 查询（透传——操作员工具，设计 §4.2 诚实口径）、
// 栈状态视图（N/M nodes reporting）与模式切换（set 即生效——后台收敛由
// metrics.Manager 常驻循环承载，本面只落设置）。mb/mm 可为 nil（测试/精简
// 装配形态——查询如实报后端不可用，status 的部署态/节点比如实报 unknown/
// 0）。
type MetricsService struct {
	serverv1.UnimplementedMetricsServiceServer
	st *state.Store
	// mb 是 VM 查询消费端（nil = 未装配——SearchMetrics 如实报不可用）。
	mb *metrics.Backend
	// mm 是 metrics 收敛管理器（nil = 未装配——status 部署态 unknown）。
	mm *metrics.Manager
	// nodesTotal 是集群节点总数供给（观测缓存投影；nil = 0——单测形态）。
	nodesTotal func(ctx context.Context) (int, error)
	// retentionDaysOverride 是 VM -retentionPeriod 的对齐天数（config 键
	// metrics.retention_days；装配点注入——0 = 包缺省 DefaultRetentionDays）。
	retentionDaysOverride int
}

// NewMetricsService 构造 MetricsService。
func NewMetricsService(st *state.Store) *MetricsService {
	return &MetricsService{st: st}
}

// WithRetentionDays 注入 retention 对齐天数（装配点接线；0 = 包缺省）。
func (s *MetricsService) WithRetentionDays(days int) *MetricsService {
	s.retentionDaysOverride = days
	return s
}

// WithBackend 注入 VM 消费端与收敛管理器（链式装配，nil 合法）。
func (s *MetricsService) WithBackend(mb *metrics.Backend, mm *metrics.Manager) *MetricsService {
	s.mb = mb
	s.mm = mm
	return s
}

// WithNodesTotal 注入集群节点总数供给（生产装配 = 观测缓存计数；nil 合法
// ——测试形态计 0）。
func (s *MetricsService) WithNodesTotal(fn func(ctx context.Context) (int, error)) *MetricsService {
	s.nodesTotal = fn
	return s
}

// Search limit 缺省与天花板（与 SearchLogs/proto 契约同口径）。
const (
	defaultMetricsLimit = 200
	maxMetricsLimit     = 1000
)

// SearchMetrics 执行 PromQL 区间查询（设计 §4.2）：诚实边界（同设计两分
// 支 + 坏查询）：① metrics.mode 未开 → E_METRICS_NOT_ENABLED（opt-in 默认
// 关——不返回空序列冒充有数）；② VM 不可达 → E_METRICS_BACKEND_UNAVAILABLE
// （查询面降级，采集面不受影响）；③ 坏 PromQL → 退化信封 InvalidArgument
// （VM 错误原文透传——操作员可定位；透传面不做任何改写）。
func (s *MetricsService) SearchMetrics(ctx context.Context, req *serverv1.SearchMetricsRequest) (*serverv1.SearchMetricsResponse, error) {
	if err := s.requireEnabled(ctx); err != nil {
		return nil, err
	}
	if s.mb == nil {
		return nil, apperrMetricsBackendUnavailable(
			"metrics query is unavailable: the VictoriaMetrics query face is not assembled in this build")
	}
	q := metrics.RangeQuery{
		Query:       req.GetQuery(),
		StepSeconds: int(req.GetStepSeconds()),
		Limit:       int(req.GetLimit()),
	}
	if q.Limit <= 0 {
		q.Limit = defaultMetricsLimit
	}
	if q.Limit > maxMetricsLimit {
		q.Limit = maxMetricsLimit
	}
	if req.GetTimeStart() != nil {
		q.Start = req.GetTimeStart().AsTime()
	}
	if req.GetTimeEnd() != nil {
		q.End = req.GetTimeEnd().AsTime()
	}
	series, err := s.mb.Search(ctx, q)
	if err != nil {
		switch {
		case errors.Is(err, metrics.ErrBadQuery):
			return nil, statusInvalidArgument(strings.TrimSpace(err.Error()))
		case errors.Is(err, metrics.ErrBackendUnavailable):
			return nil, apperrMetricsBackendUnavailable(
				"metrics query is unavailable: VictoriaMetrics did not answer on the loopback face (query face degraded; the scrape face is unaffected)")
		default:
			return nil, err
		}
	}
	out := make([]*serverv1.MetricsSeries, 0, len(series))
	for _, sr := range series {
		ps := make([]*serverv1.MetricsPoint, 0, len(sr.Points))
		for _, p := range sr.Points {
			ps = append(ps, &serverv1.MetricsPoint{T: p.T, V: p.V})
		}
		out = append(out, &serverv1.MetricsSeries{Metric: sr.Metric, Points: ps})
	}
	return &serverv1.SearchMetricsResponse{Series: out}, nil
}

// requireEnabled 是查询面的 opt-in 门（E_METRICS_NOT_ENABLED 409——不返回
// 空序列冒充有数）。
func (s *MetricsService) requireEnabled(ctx context.Context) error {
	in, err := s.st.LoadMetricsSettings(ctx)
	if err != nil {
		return err
	}
	if in.Mode != state.MetricsModeOn {
		return apperr.New("E_METRICS_NOT_ENABLED",
			"metrics collection is not enabled (metrics.mode=unset): enable it with 'fleetly metrics mode set on' or the Console metrics card")
	}
	return nil
}

// GetMetricsStatus 状态视图（CLI metrics status / Console 卡共面）：模式 +
// 三件部署态 + N/M 节点上报比 + retention。诚实口径：VM 不可达时
// nodes_reporting=0（配合 components 部署态解读）；查询计数面失败不推翻
// status 本身（部署态来自底座实况）。
func (s *MetricsService) GetMetricsStatus(ctx context.Context, _ *serverv1.GetMetricsStatusRequest) (*serverv1.GetMetricsStatusResponse, error) {
	in, err := s.st.LoadMetricsSettings(ctx)
	if err != nil {
		return nil, err
	}
	resp := &serverv1.GetMetricsStatusResponse{
		Mode:       in.Mode,
		ModeSet:    in.Set,
		Components: []*serverv1.MetricsComponentView{},
	}
	if s.mm != nil {
		dep, err := s.mm.DeploymentStatus(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range dep.Components {
			resp.Components = append(resp.Components, &serverv1.MetricsComponentView{
				Name: c.Name, Exists: c.Exists, Image: c.Image,
			})
		}
	}
	if s.nodesTotal != nil {
		if n, err := s.nodesTotal(ctx); err == nil {
			resp.NodesTotal = int32(n) //nolint:gosec // G115：集群节点计数，量级极小
		}
	}
	if in.Mode == state.MetricsModeOn && s.mb != nil {
		// 上报节点数 = VM 实抓的 cAdvisor 目标数（设计 §4.1「N/M 诚实口径」
		// + §6 挂账票修订：分子分母同拓扑——分母为 Ready 节点集（装配点
		// 注入），分子为 advertise 直连实抓数；VM 不可达/栈未收敛 = 0——
		// 配合 components 解读，不谎报全量）。
		if n, err := s.mb.CountInstant(ctx, nodesReportingPromQL); err == nil {
			resp.NodesReporting = int32(n) //nolint:gosec // G115：节点计数，量级极小
		}
	}
	resp.RetentionDays = int32(s.retentionDays()) //nolint:gosec // G115：保留天数，量级极小
	return resp, nil
}

// nodesReportingPromQL 是上报节点数的瞬时查询（以 cAdvisor 的 up 目标计
// ——每节点恰好一个任务；node_exporter 同数，取其一即可）。
const nodesReportingPromQL = `count(up{job="fleetly-cadvisor"} == 1)`

// retentionDays 是 VM -retentionPeriod 的对齐天数（config 键 metrics.
// retention_days 经装配点注入；未注入回落包缺省 DefaultRetentionDays——
// 单一事实源在 internal/metrics）。
func (s *MetricsService) retentionDays() int {
	if s.retentionDaysOverride > 0 {
		return s.retentionDaysOverride
	}
	return metrics.DefaultRetentionDays
}

// SetMetricsMode 切换 metrics 模式（unset | on）：设置保存 + 审计 + 事件
// 同事务（state 层 fail-closed）；收敛循环下一拍按新值收敛（三件部署或移除，
// 数据卷保留）。返回保存后的状态视图。
func (s *MetricsService) SetMetricsMode(ctx context.Context, req *serverv1.SetMetricsModeRequest) (*serverv1.SetMetricsModeResponse, error) {
	// 平台面写门（v0.3 W3-S2 扩全，rbac-teams §3.2「全局设置 → 仅平台管理
	// 员」）：metrics 模式切换触发三件套部署/移除，属平台全局设置写面——
	// 用户 principal 须平台管理员（机具令牌沿 scope 门现状）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	opts := state.MetricsSaveOptions{Actor: "human"}
	if p, ok := PrincipalFromContext(ctx); ok {
		opts.ActorTokenID = p.TokenID
	}
	if err := s.st.SaveMetricsSettings(ctx, req.GetMode(), opts); err != nil {
		return nil, err
	}
	status, err := s.GetMetricsStatus(ctx, &serverv1.GetMetricsStatusRequest{})
	if err != nil {
		return nil, err
	}
	return &serverv1.SetMetricsModeResponse{Status: status}, nil
}

// apperrMetricsBackendUnavailable 构造 E_METRICS_BACKEND_UNAVAILABLE 信封
// （503——查询面降级的诚实报错；不返回空序列冒充）。
func apperrMetricsBackendUnavailable(msg string) error {
	return apperr.New("E_METRICS_BACKEND_UNAVAILABLE", "%s", msg).WithStage("metrics.search")
}

var _ serverv1.MetricsServiceServer = (*MetricsService)(nil)
