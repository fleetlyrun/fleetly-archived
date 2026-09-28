package api

import (
	"context"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UsersService 实现 server.v1.UsersService（v0.3 W1，rbac-teams 设计
// §2.1/§3.2/§5）：平台用户管理面。
//
// 双门（设计 §4.2 第 3 条）：
//  1. scope 门（拦截器，scope.go 登记整体 admin）——机具令牌（user NULL）
//     沿此门全权（平台级凭据的设计语义，§2.3）；
//  2. 平台面门（本文件 requirePlatformAdmin）——用户 principal（会话或
//     用户 PAT）要求 is_platform_admin 且属主在册，否则 403（信封退化
//     形态，与鉴权 403 同口径——无专用稳定码）。
//
// 临时口令（CreateUser/ResetUserPassword）：服务端生成、明文仅响应一次；
// 口令重置即全端下线（state.ResetPassword 同事务吊销会话——本票裁决）。
// 用户生命周期审计由 state 层原语随业务写同事务落档（user.created /
// disabled / enabled / password_reset / platform_admin_granted / _revoked，
// actor=user:<id> 经入参传递），本服务不重复落 api.* 审计。
type UsersService struct {
	serverv1.UnimplementedUsersServiceServer
	st *state.Store
}

// NewUsersService 构造 UsersService。
func NewUsersService(st *state.Store) *UsersService {
	return &UsersService{st: st}
}

// ListUsers 全量用户列表（created_at 升序）。
func (s *UsersService) ListUsers(ctx context.Context, _ *serverv1.ListUsersRequest) (*serverv1.ListUsersResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	users, err := s.st.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.UserView, 0, len(users))
	for _, u := range users {
		out = append(out, userView(u))
	}
	return &serverv1.ListUsersResponse{Users: out}, nil
}

// CreateUser 平台管理员添加用户（R2）：服务端生成临时口令一次性返回。
func (s *UsersService) CreateUser(ctx context.Context, req *serverv1.CreateUserRequest) (*serverv1.CreateUserResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	tempPassword, err := generateTemporaryPassword()
	if err != nil {
		return nil, err
	}
	p := principalOf(ctx)
	u, err := s.st.CreateUser(ctx, state.UserWrite{
		Email:        req.GetEmail(),
		DisplayName:  req.GetDisplayName(),
		Password:     tempPassword,
		ActorUserID:  p.UserID,
		ActorTokenID: callerTokenID(ctx),
	})
	if err != nil {
		return nil, mapStoreErr(err, req.GetEmail())
	}
	return &serverv1.CreateUserResponse{User: userView(u), TemporaryPassword: tempPassword}, nil
}

// DisableUser 禁用用户（会话同事务吊销 + PAT 认证路径联动拒认）。
func (s *UsersService) DisableUser(ctx context.Context, req *serverv1.DisableUserRequest) (*serverv1.DisableUserResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	p := principalOf(ctx)
	if err := s.st.DisableUser(ctx, req.GetId(), p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.DisableUserResponse{User: s.mustUserView(ctx, req.GetId())}, nil
}

// EnableUser 解除禁用。
func (s *UsersService) EnableUser(ctx context.Context, req *serverv1.EnableUserRequest) (*serverv1.EnableUserResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	p := principalOf(ctx)
	if err := s.st.EnableUser(ctx, req.GetId(), p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.EnableUserResponse{User: s.mustUserView(ctx, req.GetId())}, nil
}

// ResetUserPassword 重设口令：临时口令一次性返回；旧口令立即失效且该用户
// 全部会话同事务吊销（重置即全端下线，rbac-teams §2.1 本票裁决）。
func (s *UsersService) ResetUserPassword(ctx context.Context, req *serverv1.ResetUserPasswordRequest) (*serverv1.ResetUserPasswordResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	tempPassword, err := generateTemporaryPassword()
	if err != nil {
		return nil, err
	}
	p := principalOf(ctx)
	if err := s.st.ResetPassword(ctx, req.GetId(), tempPassword, p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.ResetUserPasswordResponse{Id: req.GetId(), TemporaryPassword: tempPassword}, nil
}

// GrantPlatformAdmin 授予平台管理员标志（幂等）。
func (s *UsersService) GrantPlatformAdmin(ctx context.Context, req *serverv1.GrantPlatformAdminRequest) (*serverv1.GrantPlatformAdminResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	p := principalOf(ctx)
	if err := s.st.SetPlatformAdmin(ctx, req.GetId(), true, p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.GrantPlatformAdminResponse{User: s.mustUserView(ctx, req.GetId())}, nil
}

// RevokePlatformAdmin 撤销平台管理员标志（幂等；机具 admin 令牌是平台
// 管理面兜底凭据，无「最后一名平台管理员」守卫——与 E_TOKEN_LAST_ADMIN
// 的 token 守卫不同层，不留自锁面）。
func (s *UsersService) RevokePlatformAdmin(ctx context.Context, req *serverv1.RevokePlatformAdminRequest) (*serverv1.RevokePlatformAdminResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	p := principalOf(ctx)
	if err := s.st.SetPlatformAdmin(ctx, req.GetId(), false, p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.RevokePlatformAdminResponse{User: s.mustUserView(ctx, req.GetId())}, nil
}

// SetRegistration 注册开关（R2）：open|closed（写 platform_settings +
// 审计 auth.registration_changed，state 层同事务）。
func (s *UsersService) SetRegistration(ctx context.Context, req *serverv1.SetRegistrationRequest) (*serverv1.SetRegistrationResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	mode := state.AuthRegistrationClosed
	if req.GetOpen() {
		mode = state.AuthRegistrationOpen
	}
	p := principalOf(ctx)
	actor := "human"
	if p.UserID != "" {
		actor = "user:" + p.UserID
	}
	if err := s.st.SaveRegistration(ctx, mode, state.AuthSaveOptions{
		Actor:        actor,
		ActorTokenID: callerTokenID(ctx),
	}); err != nil {
		return nil, err
	}
	return &serverv1.SetRegistrationResponse{Open: req.GetOpen()}, nil
}

// GetAuditRetention 审计留存设置只读投影（v0.3 W3-S3，rbac-teams §6
// D-W0-6 收口）：set=false 时 days/updated_at 不输出——生效值回落链
// （config > 缺省 90）由消费方裁决，本层不投影缺省数值（state 层语义）。
func (s *UsersService) GetAuditRetention(ctx context.Context, _ *serverv1.GetAuditRetentionRequest) (*serverv1.GetAuditRetentionResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	settings, err := s.st.LoadAuditSettings(ctx)
	if err != nil {
		return nil, err
	}
	resp := &serverv1.GetAuditRetentionResponse{Set: settings.Set}
	if settings.Set {
		resp.Days = int32(settings.RetentionDays) //nolint:gosec // G115：天数 ≤ int32 值域（校验后小整数）
		resp.UpdatedAt = timestamppb.New(settings.UpdatedAt)
	}
	return resp, nil
}

// SetAuditRetention 审计留存天数设置（写面 API 本票补齐——state
// SaveRetentionDays W3-S1 已备，挂 UsersService 与 SetRegistration 同族：
// 平台面写语义 + audit.retention_changed 审计在 state 层同事务落档）。
func (s *UsersService) SetAuditRetention(ctx context.Context, req *serverv1.SetAuditRetentionRequest) (*serverv1.SetAuditRetentionResponse, error) {
	if err := s.requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	days := int(req.GetDays())
	// 双门之二：buf.validate 拦 HTTP 面，此处拦进程内调用（词表口径与
	// state.ValidateRetentionDays 一致，信封显式 422 形态）。
	if err := state.ValidateRetentionDays(days); err != nil {
		return nil, statusEnvelope(codes.InvalidArgument, "audit retention days must be >= 1")
	}
	p := principalOf(ctx)
	actor := "human"
	if p.UserID != "" {
		actor = "user:" + p.UserID
	}
	if err := s.st.SaveRetentionDays(ctx, days, state.AuditSaveOptions{
		Actor:        actor,
		ActorTokenID: callerTokenID(ctx),
	}); err != nil {
		return nil, err
	}
	return &serverv1.SetAuditRetentionResponse{Days: req.GetDays()}, nil
}

// ── 内部 helpers ─────────────────────────────────────────────────────────────

// requirePlatformAdminPrincipal 是平台面判定的共享单点（设计 §4.2 第 3 条
// ——UsersService 与 AuditService（W3-S1 D-W0-6 审计读面）同门）：机具令牌
// （UserID 空）沿 scope 门放行（拦截器已强制 admin）；用户 principal 要求
// 属主在册且 is_platform_admin。任何读取故障按拒绝处理（fail-closed）。
func requirePlatformAdminPrincipal(ctx context.Context, st UserLookupStore) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	if p.UserID == "" {
		return nil // 机具令牌：平台级凭据（rbac-teams §2.3 设计语义）
	}
	u, err := st.GetUser(ctx, p.UserID)
	if err != nil {
		return statusEnvelope(codes.Unauthenticated, "invalid credential")
	}
	if !u.IsPlatformAdmin || !u.DisabledAt.IsZero() {
		return statusEnvelope(codes.PermissionDenied, "platform administrator privileges required")
	}
	return nil
}

// UserLookupStore 是平台面判定所需的最小读取面（*state.Store 满足）——
// 共享门不绑定具体服务，判定的输入只有 principal 与属主行。
type UserLookupStore interface {
	GetUser(ctx context.Context, id string) (state.User, error)
}

// requirePlatformAdmin 是 UsersService 的平台面判定（共享单点委托——
// 审计读面同门，语义不可分叉）。
func (s *UsersService) requirePlatformAdmin(ctx context.Context) error {
	return requirePlatformAdminPrincipal(ctx, s.st)
}

// principalOf 取调用方身份（拦截器保证存在；兜底零值）。
func principalOf(ctx context.Context) Principal {
	p, _ := PrincipalFromContext(ctx)
	return p
}

// state 用户哨兵（ErrUserNotFound）→ 404 退化信封的映射已收进 errors.go 的
// 哨兵登记表——本文件经 mapStoreErr 消费（authservice.Me 的同哨兵 401 特例
// 见彼处注记）。

// mustUserView 取用户行投影（写路径成功后的回读；行消失属理论不可达，
// fail-closed 返回 404 退化信封）。
func (s *UsersService) mustUserView(ctx context.Context, id string) *serverv1.UserView {
	u, err := s.st.GetUser(ctx, id)
	if err != nil {
		return nil
	}
	return userView(u)
}
