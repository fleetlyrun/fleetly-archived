package api

import (
	"context"
	"errors"
	"regexp"
	"strings"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
)

// TeamsService 实现 server.v1.TeamsService（v0.3 W2-S1，rbac-teams 设计
// §3.1/§3.2/§5）：团队与成员/邀请面。
//
// 团队面权限（本票自含，W2-S4 通用角色门之前的实现切面）：
//   - 全部方法要求用户凭据——机具令牌（UserID 空）恒 403（无用户即无团队
//     成员身份，含读面；平台级凭据的设计语义 §2.3）；
//   - 读面（List/Get）：团队成员；平台管理员只读放行（support 视角，§3.2
//     「全团队项目只读」）；
//   - 写面按 §3.2 矩阵与 §3.1 明文取齐：team 改名/删除/成员角色增删改 =
//     owner（矩阵「成员/角色 → owner」直读）；邀请创建/列表/吊销 =
//     owner/admin（§3.1 明文「owner/admin 可邀」优先于矩阵行——所邀角色
//     不得高于邀请者自身，邀 owner 角色 = owner 专属）；平台管理员对写面
//     一律 403（不代团队做写操作，§3.2——职责分离，审计 actor 干净）。
//
// 审计与事件：成员/邀请原语的审计（team.member_added 等）与事件
// （team.member_changed / invite.accepted）由 state 层随业务写同事务落
// （Outbox），本服务不重复落 api.* 审计（W1 UsersService 同口径）。
type TeamsService struct {
	serverv1.UnimplementedTeamsServiceServer
	st *state.Store
}

// NewTeamsService 构造 TeamsService。
func NewTeamsService(st *state.Store) *TeamsService {
	return &TeamsService{st: st}
}

// slugPattern 是单词制 slug 词表（[a-z0-9]{2,32}，设计 §3.1/§4.3；禁连字
// 符——底座命名公式段拼接无歧义）。proto 形状注解（buf.validate pattern）
// 的服务端同款复核：bufconn 测试装配的拦截链无 protovalidate，服务端校验
// 是唯一强制点。
var slugPattern = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

// validateTeamSlug 校验团队 slug：词表 + 保留字守卫（8 个平台组件保留字守
// 前缀族——v0.3 命名公式以 team slug 为参数，E_TEAM_SLUG_RESERVED 422）。
// 词表违约走 400 退化信封（形状错误，无需稳定码）。
func validateTeamSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return statusInvalidArgument("team slug must be a lowercase word of 2..32 characters [a-z0-9]")
	}
	// 保留字清单已随 v0.3 命名三段化从 app 名迁到 team slug（rbac-teams
	// §4.3 保留字迁移；W2-S3 起消费 naming 单点——消 W2-S1 先行守卫的
	// 同源重复）。清单随迁移语义不变：同 8 词、同撞键证据链。
	if naming.IsReservedTeamSlug(slug) {
		return apperr.New("E_TEAM_SLUG_RESERVED",
			"team slug %q collides with a platform-reserved component name: %s", slug, naming.ReservedTeamSlugReason(slug)).
			WithContext("reserved_names", strings.Join(naming.ReservedTeamSlugs(), ","))
	}
	return nil
}

// validateProjectSlug 校验项目 slug：词表同形（无保留字守卫——§4.3：project
// 段居命名公式第二位，不邻前缀族，不新增保留约束）。
func validateProjectSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return statusInvalidArgument("project slug must be a lowercase word of 2..32 characters [a-z0-9]")
	}
	return nil
}

// validTeamRoleValue 报告角色是否在团队四档词表内（服务端复核——bufconn
// 测试装配的拦截链无 protovalidate，proto 形状注解在直连测试面不生效；
// state 写入通道的同款校验是双保险，但其拒绝是普通错误，须在本面提前
// 收敛成 400，避免词表违约以 Unknown 形态漏出）。
func validTeamRoleValue(role string) bool {
	switch role {
	case state.TeamRoleOwner, state.TeamRoleAdmin, state.TeamRoleDeveloper, state.TeamRoleViewer:
		return true
	}
	return false
}

// validProjectRoleValue 报告角色是否在项目覆写三档词表内（owner 不可覆写
// ——词表结构性排除，设计 §3.3）。
func validProjectRoleValue(role string) bool {
	switch role {
	case state.ProjectRoleAdmin, state.ProjectRoleDeveloper, state.ProjectRoleViewer:
		return true
	}
	return false
}

// ── 团队面权限门（TeamsService/ProjectsService 共用）─────────────────────────

// requireTeamUser 是团队/项目面的用户凭据门：机具令牌（UserID 空）恒 403
// ——无用户即无团队成员身份（含读面；fail-closed）。
func requireTeamUser(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return Principal{}, statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	if p.UserID == "" {
		return Principal{}, statusEnvelope(codes.PermissionDenied,
			"team face requires a user credential (machine tokens carry no team membership)")
	}
	return p, nil
}

// isPlatformAdminUser 报告调用方（用户 principal）是否为在册未禁用的平台
// 管理员（读故障/机具令牌/无 principal 一律 false——fail-closed）。
func isPlatformAdminUser(ctx context.Context, st *state.Store) bool {
	p, ok := PrincipalFromContext(ctx)
	if !ok || p.UserID == "" {
		return false
	}
	u, err := st.GetUser(ctx, p.UserID)
	if err != nil {
		return false
	}
	return u.IsPlatformAdmin && u.DisabledAt.IsZero()
}

// requireTeamReadAccess 是团队/项目读面门：团队成员，或平台管理员（只读
// 全域——support 视角，§3.2）。
func requireTeamReadAccess(ctx context.Context, st *state.Store, teamID string) error {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return err
	}
	if _, err := st.GetMembership(ctx, teamID, p.UserID); err == nil {
		return nil
	} else if !errors.Is(err, state.ErrTeamMemberNotFound) {
		return err
	}
	if isPlatformAdminUser(ctx, st) {
		return nil
	}
	return statusEnvelope(codes.PermissionDenied, "team membership required")
}

// requireTeamOwner 是 owner 门（团队生命周期写面 + 成员/角色管理；矩阵
// 「成员/角色 → owner」直读）。平台管理员不代写（403）。
func requireTeamOwner(ctx context.Context, st *state.Store, teamID string) (Principal, error) {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return Principal{}, err
	}
	m, err := st.GetMembership(ctx, teamID, p.UserID)
	if err != nil {
		if errors.Is(err, state.ErrTeamMemberNotFound) {
			return Principal{}, statusEnvelope(codes.PermissionDenied, "team owner role required")
		}
		return Principal{}, err
	}
	if m.Role != state.TeamRoleOwner {
		return Principal{}, statusEnvelope(codes.PermissionDenied, "team owner role required")
	}
	return p, nil
}

// requireTeamInviter 是邀请面门（创建/列表/吊销）：owner/admin（§3.1 明文
// 取齐；矩阵「邀请 → owner」行的冲突以 §3.1 明文为准——票面偏差记录）。
// 返回调用方团队角色供「所邀角色不得高于邀请者自身」校验。
func requireTeamInviter(ctx context.Context, st *state.Store, teamID string) (Principal, string, error) {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return Principal{}, "", err
	}
	m, err := st.GetMembership(ctx, teamID, p.UserID)
	if err != nil {
		if errors.Is(err, state.ErrTeamMemberNotFound) {
			return Principal{}, "", statusEnvelope(codes.PermissionDenied, "team owner or admin role required")
		}
		return Principal{}, "", err
	}
	if m.Role != state.TeamRoleOwner && m.Role != state.TeamRoleAdmin {
		return Principal{}, "", statusEnvelope(codes.PermissionDenied, "team owner or admin role required")
	}
	return p, m.Role, nil
}

// teamRoleRank 是四档团队角色的强弱序（邀请角色上限校验，§3.1：所邀角色
// 不得高于邀请者自身；邀 owner 角色 = owner 专属——owner 之外无人 rank ≥ 4）。
func teamRoleRank(role string) int {
	switch role {
	case state.TeamRoleOwner:
		return 4
	case state.TeamRoleAdmin:
		return 3
	case state.TeamRoleDeveloper:
		return 2
	case state.TeamRoleViewer:
		return 1
	}
	return 0
}

// 团队面 state 哨兵（ErrTeamNotFound/ErrTeamSlugTaken/ErrTeamNotEmpty/
// ErrTeamMemberExists/ErrTeamMemberNotFound/ErrTeamLastOwner/ErrInviteNotFound/
// ErrInviteInvalid）的 api 语义（退化信封 + E_TEAM_LAST_OWNER/E_INVITE_INVALID
// 注册表稳定码）已收进 errors.go 的哨兵登记表——本文件经 mapStoreErr 消费；
// 角色门（下方 requireTeam* 族）的 ErrTeamMemberNotFound 是「查无成员关系 →
// 403」的调用面特例，不入表（同哨兵异语义：成员操作面 404、权限门 403）。
// ── TeamsService RPC ─────────────────────────────────────────────────────────

// CreateTeam 建队 + 建队者 owner 落位（§3.1）——组合写入走 state 单写点
// 原语（CreateTeamWithOwner：团队行 + 成员行 + 审计 + 事件同事务，消除
// 「有队无 owner」的中断窗口——无 owner 的团队无人可管理）。
func (s *TeamsService) CreateTeam(ctx context.Context, req *serverv1.CreateTeamRequest) (*serverv1.CreateTeamResponse, error) {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateTeamSlug(req.GetSlug()); err != nil {
		return nil, err
	}
	t, err := s.st.CreateTeamWithOwner(ctx, state.TeamWrite{
		Slug:         req.GetSlug(),
		Name:         req.GetName(),
		CreatedBy:    p.UserID,
		ActorUserID:  p.UserID,
		ActorTokenID: callerTokenID(ctx),
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.CreateTeamResponse{Team: teamView(t)}, nil
}

// ListTeams 我所在团队；平台管理员 = 全部（§3.2 只读全域）。
func (s *TeamsService) ListTeams(ctx context.Context, _ *serverv1.ListTeamsRequest) (*serverv1.ListTeamsResponse, error) {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return nil, err
	}
	var teams []state.Team
	if isPlatformAdminUser(ctx, s.st) {
		teams, err = s.st.ListTeams(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		memberships, err := s.st.ListUserMemberships(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		for _, m := range memberships {
			t, err := s.st.GetTeam(ctx, m.TeamID)
			if err != nil {
				if errors.Is(err, state.ErrTeamNotFound) {
					continue // 成员关系残留而团队行缺失：跳过（Me 同口径）
				}
				return nil, err
			}
			teams = append(teams, t)
		}
	}
	out := make([]*serverv1.TeamView, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamView(t))
	}
	return &serverv1.ListTeamsResponse{Teams: out}, nil
}

// GetTeam 团队投影（成员或平台管理员只读）。
func (s *TeamsService) GetTeam(ctx context.Context, req *serverv1.GetTeamRequest) (*serverv1.GetTeamResponse, error) {
	if _, err := requireTeamUser(ctx); err != nil {
		return nil, err
	}
	if err := requireTeamReadAccess(ctx, s.st, req.GetId()); err != nil {
		return nil, err
	}
	t, err := s.st.GetTeam(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.GetTeamResponse{Team: teamView(t)}, nil
}

// UpdateTeam 仅改显示名（slug 不可变——请求无 slug 字段，结构性杜绝）。
func (s *TeamsService) UpdateTeam(ctx context.Context, req *serverv1.UpdateTeamRequest) (*serverv1.UpdateTeamResponse, error) {
	if _, err := requireTeamOwner(ctx, s.st, req.GetId()); err != nil {
		return nil, err
	}
	t, err := s.st.UpdateTeam(ctx, state.TeamUpdate{
		ID:          req.GetId(),
		Name:        req.GetName(),
		ActorUserID: principalOf(ctx).UserID,
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.UpdateTeamResponse{Team: teamView(t)}, nil
}

// DeleteTeam owner 专属两段式（confirm = slug，与 DeleteDatabase 同口径）
// + 项目须空守卫（state ErrTeamNotEmpty → 409）。
func (s *TeamsService) DeleteTeam(ctx context.Context, req *serverv1.DeleteTeamRequest) (*serverv1.DeleteTeamResponse, error) {
	p, err := requireTeamOwner(ctx, s.st, req.GetId())
	if err != nil {
		return nil, err
	}
	t, err := s.st.GetTeam(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if req.GetConfirm() != t.Slug {
		return nil, statusInvalidArgument(
			"destructive operation: pass confirm=\"" + t.Slug + "\" to accept team deletion (projects must be empty; membership and invites are removed with the team)")
	}
	if err := s.st.DeleteTeam(ctx, req.GetId(), p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.DeleteTeamResponse{}, nil
}

// ListTeamMembers 成员列表（读面：成员或平台管理员）。
func (s *TeamsService) ListTeamMembers(ctx context.Context, req *serverv1.ListTeamMembersRequest) (*serverv1.ListTeamMembersResponse, error) {
	if _, err := requireTeamUser(ctx); err != nil {
		return nil, err
	}
	if err := requireTeamReadAccess(ctx, s.st, req.GetTeamId()); err != nil {
		return nil, err
	}
	members, err := s.st.ListMembers(ctx, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.TeamMemberView, 0, len(members))
	for _, m := range members {
		u, _ := s.st.GetUser(ctx, m.UserID) // 属主行缺失按空投影（不阻塞列表）
		out = append(out, teamMemberView(m, u))
	}
	return &serverv1.ListTeamMembersResponse{Members: out}, nil
}

// SetTeamMemberRole owner 专属：调整成员角色（最后一名 owner 降级 →
// E_TEAM_LAST_OWNER，state 守卫同事务）。
func (s *TeamsService) SetTeamMemberRole(ctx context.Context, req *serverv1.SetTeamMemberRoleRequest) (*serverv1.SetTeamMemberRoleResponse, error) {
	if !validTeamRoleValue(req.GetRole()) {
		return nil, statusInvalidArgument("team role must be one of: owner, admin, developer, viewer")
	}
	p, err := requireTeamOwner(ctx, s.st, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	m, err := s.st.SetMemberRole(ctx, req.GetTeamId(), req.GetUserId(), req.GetRole(), p.UserID, callerTokenID(ctx))
	if err != nil {
		return nil, mapStoreErr(err)
	}
	u, _ := s.st.GetUser(ctx, m.UserID) // 属主行缺失按空投影（不阻塞响应）
	return &serverv1.SetTeamMemberRoleResponse{Member: teamMemberView(m, u)}, nil
}

// RemoveTeamMember owner 专属：移出成员（state 同事务联动清该团队全部
// 项目覆写行，D-W0-2；最后一名 owner → E_TEAM_LAST_OWNER）。
func (s *TeamsService) RemoveTeamMember(ctx context.Context, req *serverv1.RemoveTeamMemberRequest) (*serverv1.RemoveTeamMemberResponse, error) {
	p, err := requireTeamOwner(ctx, s.st, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	if err := s.st.RemoveMember(ctx, req.GetTeamId(), req.GetUserId(), p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.RemoveTeamMemberResponse{}, nil
}

// CreateInvite owner/admin 可邀（§3.1）：所邀角色不得高于邀请者自身（owner
// 角色仅 owner 可邀——rank 判定天然覆盖）；明文 token 仅本次响应一次性返回。
func (s *TeamsService) CreateInvite(ctx context.Context, req *serverv1.CreateInviteRequest) (*serverv1.CreateInviteResponse, error) {
	if !validTeamRoleValue(req.GetRole()) {
		return nil, statusInvalidArgument("team role must be one of: owner, admin, developer, viewer")
	}
	p, inviterRole, err := requireTeamInviter(ctx, s.st, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	if teamRoleRank(req.GetRole()) > teamRoleRank(inviterRole) {
		return nil, statusEnvelope(codes.PermissionDenied,
			"invited role must not exceed the inviter's own team role (inviting an owner requires the owner role)")
	}
	inv, token, err := s.st.CreateInvite(ctx, state.InviteWrite{
		TeamID:       req.GetTeamId(),
		Email:        req.GetEmail(),
		Role:         req.GetRole(),
		ActorUserID:  p.UserID,
		ActorTokenID: callerTokenID(ctx),
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.CreateInviteResponse{Invite: inviteView(inv), Token: token}, nil
}

// ListTeamInvites 邀请列表（邀请面读：owner/admin——与创建/吊销同面）。
func (s *TeamsService) ListTeamInvites(ctx context.Context, req *serverv1.ListTeamInvitesRequest) (*serverv1.ListTeamInvitesResponse, error) {
	if _, _, err := requireTeamInviter(ctx, s.st, req.GetTeamId()); err != nil {
		return nil, err
	}
	invs, err := s.st.ListInvites(ctx, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.InviteView, 0, len(invs))
	for _, inv := range invs {
		out = append(out, inviteView(inv))
	}
	return &serverv1.ListTeamInvitesResponse{Invites: out}, nil
}

// RevokeInvite 吊销未消费邀请（已消费/已吊销按幂等成功；team 归属校验在
// 本面做——邀请 ID 全局唯一，防止跨团队误吊销）。
func (s *TeamsService) RevokeInvite(ctx context.Context, req *serverv1.RevokeInviteRequest) (*serverv1.RevokeInviteResponse, error) {
	p, _, err := requireTeamInviter(ctx, s.st, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	invs, err := s.st.ListInvites(ctx, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	found := false
	for _, inv := range invs {
		if inv.ID == req.GetInviteId() {
			found = true
			break
		}
	}
	if !found {
		return nil, notFound("invite not found in team: " + req.GetInviteId())
	}
	if err := s.st.RevokeInvite(ctx, req.GetInviteId(), p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.RevokeInviteResponse{}, nil
}

// ── 投影 helpers（TeamsService/ProjectsService 共用）────────────────────────

// teamView 是 state.Team → proto 投影。
func teamView(t state.Team) *serverv1.TeamView {
	return &serverv1.TeamView{
		Id:        t.ID,
		Slug:      t.Slug,
		Name:      t.Name,
		CreatedBy: t.CreatedBy,
		CreatedAt: tstamp(t.CreatedAt),
	}
}

// teamMemberView 是 state.TeamMember → proto 投影（email/display_name 属主
// 投影随行补全；属主行缺失按空投影——不阻塞列表）。
func teamMemberView(m state.TeamMember, u state.User) *serverv1.TeamMemberView {
	return &serverv1.TeamMemberView{
		TeamId:      m.TeamID,
		UserId:      m.UserID,
		Role:        m.Role,
		CreatedAt:   tstamp(m.CreatedAt),
		Email:       u.Email,
		DisplayName: u.DisplayName,
	}
}

// inviteView 是 state.TeamInvite → proto 投影（token_hash 不在任何通道；
// 明文 token 只在 CreateInvite 响应一次性返回）。
func inviteView(inv state.TeamInvite) *serverv1.InviteView {
	return &serverv1.InviteView{
		Id:         inv.ID,
		TeamId:     inv.TeamID,
		Email:      inv.Email,
		Role:       inv.Role,
		ExpiresAt:  tstamp(inv.ExpiresAt),
		CreatedAt:  tstamp(inv.CreatedAt),
		AcceptedAt: tstamp(inv.AcceptedAt),
		RevokedAt:  tstamp(inv.RevokedAt),
	}
}
