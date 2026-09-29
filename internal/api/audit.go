package api

import (
	"context"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AuditService 实现 server.v1.AuditService（v0.3 W3-S1，rbac-teams §5/§6
// 裁决 D-W0-6）：审计台账读面（过滤 + 分页），Console 审计页（W3-S3）与
// CLI `audit list/export` 的共同后端。
//
// 双门（设计 §4.2 第 3 条，与 UsersService 同款）：
//  1. scope 门（拦截器，scope.go 登记整体 admin）——机具令牌（user NULL）
//     沿此门全权（平台管理员等价，§2.3 平台级凭据的设计语义）；
//  2. 平台面门（requirePlatformAdminPrincipal 共享单点）——用户 principal
//     要求 is_platform_admin 且属主在册，否则 403（与鉴权 403 同口径）。
//
// 审计行本身由写侧同事务落档（写面审计纪律不变）；本服务只读、零审计
// （读面不自审——读动作无副作用，api.* 审计词表只记写面）。
type AuditService struct {
	serverv1.UnimplementedAuditServiceServer
	st *state.Store
}

// NewAuditService 构造 AuditService。
func NewAuditService(st *state.Store) *AuditService {
	return &AuditService{st: st}
}

// ListAudit 按过滤集分页检索审计台账（at 倒序；total = 过滤生效、分页生
// 效前的全量命中数）。过滤语义与 state.AuditQuery 同源（audit.go 包注释）：
// actor/target 子串包含、action 前缀匹配、result 精确、since/until 闭区间。
func (s *AuditService) ListAudit(ctx context.Context, req *serverv1.ListAuditRequest) (*serverv1.ListAuditResponse, error) {
	if err := requirePlatformAdminPrincipal(ctx, s.st); err != nil {
		return nil, err
	}
	q := state.AuditQuery{
		Actor:  req.GetActor(),
		Action: req.GetAction(),
		Result: req.GetResult(),
		Target: req.GetTarget(),
		Limit:  int(req.GetLimit()),
		Offset: int(req.GetOffset()),
	}
	if since := req.GetSince(); since != nil {
		q.Since = since.AsTime()
	}
	if until := req.GetUntil(); until != nil {
		q.Until = until.AsTime()
	}
	rows, total, err := s.st.ListAudits(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.AuditView, 0, len(rows))
	for _, r := range rows {
		out = append(out, auditView(r))
	}
	return &serverv1.ListAuditResponse{Audits: out, Total: int32(total)}, nil //nolint:gosec // G115：total ≤ int32 值域（分页面行数计数）
}

// auditView 是 state 审计行 → proto 投影（AuditRecord 全字段——含 W3-S1
// 补披露的 request_id；空串字段经 gateway JSONPb EmitUnpopulated=true
// 显式输出 ""——2026-09-29 JSON 口径反转）。
func auditView(r state.AuditRecord) *serverv1.AuditView {
	v := &serverv1.AuditView{
		Id:          r.ID,
		Actor:       r.Actor,
		Action:      r.Action,
		Target:      r.Target,
		Result:      r.Result,
		ErrorCode:   r.ErrorCode,
		RequestId:   r.RequestID,
		DiffSummary: r.DiffSummary,
	}
	if !r.At.IsZero() {
		v.At = timestamppb.New(r.At)
	}
	return v
}
