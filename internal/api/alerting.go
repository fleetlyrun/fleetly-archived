package api

import (
	"context"
	"strings"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/metrics"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AlertingService 实现 server.v1.AlertingService（B 线 W5 设计 §2，D-V3W5-1，
// v0.3 W5-S2）：告警规则 CRUD + alerts.mode 开关 + 栈状态视图 + TestAlertRule
//（VM instant query 直接执行 expr——规则编写的即时校验面）。
//
// 本服务是纯受理/投影面：规则渲染与 vmalert 部署由 metrics 管理器承载，告警
// 投递由接收器（runtime）→ notify 管线承载。mb/mm 可为 nil（测试/精简装配
// ——TestAlertRule 如实报后端不可用，status 部署态如实报 unknown）。
type AlertingService struct {
	serverv1.UnimplementedAlertingServiceServer
	st *state.Store
	// mb 是 VM 查询消费端（nil = 未装配——TestAlertRule 如实报不可用）。
	mb *metrics.Backend
	// mm 是 metrics 收敛管理器（nil = 未装配——status 部署态如实报 unknown）。
	mm *metrics.Manager
}

// NewAlertingService 构造 AlertingService。
func NewAlertingService(st *state.Store) *AlertingService {
	return &AlertingService{st: st}
}

// WithBackend 注入 VM 消费端与收敛管理器（链式装配，nil 合法）。
func (s *AlertingService) WithBackend(mb *metrics.Backend, mm *metrics.Manager) *AlertingService {
	s.mb = mb
	s.mm = mm
	return s
}

// CreateAlertRule 创建规则（平台级唯一名；写面平台门）。
func (s *AlertingService) CreateAlertRule(ctx context.Context, req *serverv1.CreateAlertRuleRequest) (*serverv1.CreateAlertRuleResponse, error) {
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	r, err := s.st.CreateAlertRule(ctx, state.AlertRuleWrite{
		Name:               req.GetName(),
		Expr:               req.GetExpr(),
		ForDurationSeconds: req.GetForDurationSeconds(),
		Labels:             req.GetLabels(),
		Channels:           req.GetChannels(),
		Actor:              "human",
		ActorTokenID:       callerTokenID(ctx),
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.CreateAlertRuleResponse{Rule: alertRuleView(r)}, nil
}

// ListAlertRules 规则清单（read——任意已认证凭据）。
func (s *AlertingService) ListAlertRules(ctx context.Context, _ *serverv1.ListAlertRulesRequest) (*serverv1.ListAlertRulesResponse, error) {
	rows, err := s.st.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.AlertRuleView, 0, len(rows))
	for _, r := range rows {
		out = append(out, alertRuleView(r))
	}
	return &serverv1.ListAlertRulesResponse{Rules: out}, nil
}

// UpdateAlertRule 部分更新（optional 字段语义；写面平台门）。
func (s *AlertingService) UpdateAlertRule(ctx context.Context, req *serverv1.UpdateAlertRuleRequest) (*serverv1.UpdateAlertRuleResponse, error) {
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	u := state.AlertRuleUpdate{Actor: "human", ActorTokenID: callerTokenID(ctx)}
	if req.Name != nil {
		u.Name = req.Name
	}
	if req.Expr != nil {
		u.Expr = req.Expr
	}
	if req.ForDurationSeconds != nil {
		u.ForDurationSeconds = req.ForDurationSeconds
	}
	if req.Labels != nil {
		labels := req.GetLabels()
		u.Labels = &labels
	}
	if len(req.GetChannels()) > 0 {
		u.Channels = req.GetChannels()
	}
	r, err := s.st.UpdateAlertRule(ctx, req.GetId(), u)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.UpdateAlertRuleResponse{Rule: alertRuleView(r)}, nil
}

// DeleteAlertRule 删除规则（写面平台门；收敛循环下一拍重渲染规则文件）。
func (s *AlertingService) DeleteAlertRule(ctx context.Context, req *serverv1.DeleteAlertRuleRequest) (*serverv1.DeleteAlertRuleResponse, error) {
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if err := s.st.DeleteAlertRule(ctx, req.GetId(), "human", callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.DeleteAlertRuleResponse{}, nil
}

// SetAlertsMode 切换 alerts.mode（unset | on）：前置门 metrics.mode=on 在
// state 层（409 E_ALERTS_METRICS_REQUIRED 信封透传）；设置保存 + 审计同
// 事务，收敛循环下一拍收敛（部署 vmalert + 规则文件，或移除）。
func (s *AlertingService) SetAlertsMode(ctx context.Context, req *serverv1.SetAlertsModeRequest) (*serverv1.SetAlertsModeResponse, error) {
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	opts := state.AlertsSaveOptions{Actor: "human"}
	if p, ok := PrincipalFromContext(ctx); ok {
		opts.ActorTokenID = p.TokenID
	}
	if err := s.st.SaveAlertsSettings(ctx, req.GetMode(), opts); err != nil {
		return nil, err
	}
	st, err := s.GetAlertsStatus(ctx, &serverv1.GetAlertsStatusRequest{})
	if err != nil {
		return nil, err
	}
	return &serverv1.SetAlertsModeResponse{Status: st}, nil
}

// GetAlertsStatus 告警栈状态视图（read）：mode + vmalert 部署态 + 规则数 +
// metrics.mode 并列（前置门的可见面——alerts on 而 metrics off = vmalert
// 被移除的休眠形态，如实并列让消费方自判，不聚合谎报）。
func (s *AlertingService) GetAlertsStatus(ctx context.Context, _ *serverv1.GetAlertsStatusRequest) (*serverv1.GetAlertsStatusResponse, error) {
	in, err := s.st.LoadAlertsSettings(ctx)
	if err != nil {
		return nil, err
	}
	metricsIn, err := s.st.LoadMetricsSettings(ctx)
	if err != nil {
		return nil, err
	}
	count, err := s.st.CountAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	resp := &serverv1.GetAlertsStatusResponse{
		Mode:        in.Mode,
		ModeSet:     in.Set,
		RuleCount:   int32(count), //nolint:gosec // G115：规则计数，量级极小
		MetricsMode: metricsIn.Mode,
	}
	if s.mm != nil {
		comp, err := s.mm.VMAlertStatus(ctx)
		if err != nil {
			return nil, err
		}
		resp.VmalertExists = comp.Exists
		resp.VmalertImage = comp.Image
	}
	return resp, nil
}

// TestAlertRule 单次求值试跑（写面同门——规则编写伴随面且消耗 VM 查询
// 资源）：expr 经 VM /api/v1/query 直接执行返回样本；空集 = 表达式合法但
// 当前无匹配序列。mode 门沿用 metrics 查询面（E_ALERTS_NOT_ENABLED 不另立
// ——查询面的 opt-in 语义就是 metrics.mode）。
func (s *AlertingService) TestAlertRule(ctx context.Context, req *serverv1.TestAlertRuleRequest) (*serverv1.TestAlertRuleResponse, error) {
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	metricsIn, err := s.st.LoadMetricsSettings(ctx)
	if err != nil {
		return nil, err
	}
	if metricsIn.Mode != state.MetricsModeOn {
		return nil, apperr.New("E_METRICS_NOT_ENABLED",
			"alert rule test requires metrics.mode=on (vmalert evaluates rules against the managed VictoriaMetrics): enable it with 'fleetly metrics mode set on' first")
	}
	if s.mb == nil {
		return nil, apperrMetricsBackendUnavailable(
			"alert rule test is unavailable: the VictoriaMetrics query face is not assembled in this build")
	}
	series, err := s.mb.InstantSeries(ctx, strings.TrimSpace(req.GetExpr()), defaultMetricsLimit)
	if err != nil {
		return nil, errMetricsSearchEnvelope(err,
			"alert rule test is unavailable: VictoriaMetrics did not answer on the loopback face (query face degraded; the scrape face is unaffected)")
	}
	return &serverv1.TestAlertRuleResponse{Series: metricsSeriesViews(series)}, nil
}

// alertRuleView 是规则行的 proto 投影。
func alertRuleView(r state.AlertRule) *serverv1.AlertRuleView {
	return &serverv1.AlertRuleView{
		Id:                 r.ID,
		Name:               r.Name,
		Expr:               r.Expr,
		ForDurationSeconds: r.ForDurationSeconds,
		Labels:             r.Labels,
		Channels:           r.Channels,
		CreatedAt:          timestamppb.New(r.CreatedAt),
		UpdatedAt:          timestamppb.New(r.UpdatedAt),
	}
}

// state 层哨兵（ErrAlertRuleNotFound/ErrAlertRuleNameConflict）→ 信封的
// 投影已收进 errors.go 的哨兵登记表（E_ALERT_RULE_NOT_FOUND 404 /
// E_ALERT_RULE_NAME_CONFLICT 409 注册码族见 errcode codes.go 告警面注记）
// ——本文件经 mapStoreErr 消费。
var _ serverv1.AlertingServiceServer = (*AlertingService)(nil)
