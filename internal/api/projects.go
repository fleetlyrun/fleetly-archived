package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
)

// ProjectsService 实现 server.v1.ProjectsService（v0.3 W2-S1，rbac-teams
// 设计 §3.3/§3.4/§5）：项目与队内覆写成员面。IMPL-T15-1 增项目网参与面
// （attach/detach 两 RPC，internal/api/projectnetwork.go）。
//
// 项目面权限（本票自含，W2-S4 通用角色门之前的实现切面）：
//   - 全部方法要求用户凭据（requireTeamUser——机具令牌恒 403）；
//   - 读面：归属团队成员（可见性零级联——可见项目集 = 团队归属，D-W0-2）；
//     平台管理员只读放行；
//   - 项目生命周期写面（创建/改名/删除）= 团队 owner（§3.2 矩阵「项目
//     创建/删除 → owner」；改名未入矩阵，保守取 owner——票面偏差记录）；
//   - 队内覆写成员面 = 团队 admin/owner（§3.3 明文）；覆写角色不授予成员
//     管理权（权限怪圈防线——项目内被覆写为 admin 者仍按团队角色判定）。
//
// MoveApp / MoveDatabase 不在本票（改派 = 换名重部署，依赖 W2-S3 命名
// 三段化——设计 §3.4/§5）。
//
// 审计与事件：项目原语的审计（project.created/deleted/member_role_changed/
// member_removed）与事件（project.created/deleted/member_changed）由 state
// 层随业务写同事务落（Outbox），本服务不重复落 api.* 审计。
type ProjectsService struct {
	serverv1.UnimplementedProjectsServiceServer
	st *state.Store
	// move 编排端口（W2-S3；nil = 未装配——MoveApp/MoveDatabase 如实报
	// 不可用，不静默退化）。实现 = engine / database / ingress（runtime
	// 装配；接口在本包定义——方向纪律同 GitDeployTriggers）。
	appMove AppMovePort
	dbMove  DBMovePort
	ingMove IngressMovePort
	// netPort 是项目网参与编排端口（IMPL-T15-1；nil = attach/detach 如实
	// 报不可用）。实现 = engine（projectnetwork.go）。
	netPort ProjectNetworkPort
}

// NewProjectsService 构造 ProjectsService。
func NewProjectsService(st *state.Store) *ProjectsService {
	return &ProjectsService{st: st}
}

// WithMovePorts 注入改派编排端口（链式装配；nil 端口 = 对应 RPC 如实报
// 不可用）。
func (s *ProjectsService) WithMovePorts(app AppMovePort, db DBMovePort, ing IngressMovePort) *ProjectsService {
	s.appMove, s.dbMove, s.ingMove = app, db, ing
	return s
}

// requireProjectOverrideManager 是队内覆写成员面门（§3.3 明文：团队
// admin/owner 经 ProjectsService 成员面增删改；平台管理员不代写）。
func requireProjectOverrideManager(ctx context.Context, st *state.Store, teamID string) (Principal, error) {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return Principal{}, err
	}
	m, err := st.GetMembership(ctx, teamID, p.UserID)
	if err != nil {
		if errors.Is(err, state.ErrTeamMemberNotFound) {
			return Principal{}, statusEnvelope(codes.PermissionDenied, "team admin or owner role required")
		}
		return Principal{}, err
	}
	if m.Role != state.TeamRoleOwner && m.Role != state.TeamRoleAdmin {
		return Principal{}, statusEnvelope(codes.PermissionDenied, "team admin or owner role required")
	}
	return p, nil
}

// state 项目哨兵（ErrProjectNotFound/ErrProjectSlugTaken/ErrProjectNotEmpty/
// ErrProjectMemberNotFound/ErrProjectOwnerOverride/ErrNotTeamMember）的 api
// 语义已收进 errors.go 的哨兵登记表（退化信封）——本文件经 mapStoreErr 消费；
// 改派面的 ErrMoveSameTarget（conflict(err.Error())——裹链原文）与
// ErrDatabaseExists（改派面异文案）是登记表外的调用面特例。

// getProjectForRead 取项目行并过读面门（归属团队成员或平台管理员）。
func (s *ProjectsService) getProjectForRead(ctx context.Context, id string) (state.Project, error) {
	if _, err := requireTeamUser(ctx); err != nil {
		return state.Project{}, err
	}
	p, err := s.st.GetProject(ctx, id)
	if err != nil {
		return state.Project{}, mapStoreErr(err)
	}
	if err := requireTeamReadAccess(ctx, s.st, p.TeamID); err != nil {
		return state.Project{}, err
	}
	return p, nil
}

// ── ProjectsService RPC ──────────────────────────────────────────────────────

// CreateProject 在团队下建项目（owner 专属，§3.2 矩阵「项目创建/删除」行）。
func (s *ProjectsService) CreateProject(ctx context.Context, req *serverv1.CreateProjectRequest) (*serverv1.CreateProjectResponse, error) {
	p, err := requireTeamOwner(ctx, s.st, req.GetTeamId())
	if err != nil {
		return nil, err
	}
	if err := validateProjectSlug(req.GetSlug()); err != nil {
		return nil, err
	}
	proj, err := s.st.CreateProject(ctx, state.ProjectWrite{
		TeamID:       req.GetTeamId(),
		Slug:         req.GetSlug(),
		Name:         req.GetName(),
		Description:  req.GetDescription(),
		ActorUserID:  p.UserID,
		ActorTokenID: callerTokenID(ctx),
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	view, err := s.projectView(ctx, proj, nil)
	if err != nil {
		return nil, err
	}
	return &serverv1.CreateProjectResponse{Project: view}, nil
}

// ListProjects 我可见项目（团队归属集；可见性零级联）；带 team_id 收窄到
// 单队（须为该队成员或平台管理员）；平台管理员不带过滤 = 全部。
func (s *ProjectsService) ListProjects(ctx context.Context, req *serverv1.ListProjectsRequest) (*serverv1.ListProjectsResponse, error) {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	if req.GetTeamId() != "" {
		if err := requireTeamReadAccess(ctx, s.st, req.GetTeamId()); err != nil {
			return nil, err
		}
		allowed[req.GetTeamId()] = true
	} else if !isPlatformAdminUser(ctx, s.st) {
		memberships, err := s.st.ListUserMemberships(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		for _, m := range memberships {
			allowed[m.TeamID] = true
		}
	}
	projects, err := s.st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	// 项目网成员计数（IMPL-T15-1）：单分组查询全量共享——列表投影免 N+1。
	counts, err := s.st.ProjectNetworkMemberCounts(ctx)
	if err != nil {
		return nil, err
	}
	teamFiltered := req.GetTeamId() != ""
	out := make([]*serverv1.ProjectView, 0, len(projects))
	for _, proj := range projects {
		// team_id 收窄 = 严格归属匹配（曾是 `req.GetTeamId() != ""` 恒真
		// 条件——过滤完全失效，团队设置页串出全部团队的项目，2026-09-25
		// staging 真机走查实爆后修正）。
		if (teamFiltered && proj.TeamID == req.GetTeamId()) ||
			(!teamFiltered && (isPlatformAdminUser(ctx, s.st) || allowed[proj.TeamID])) {
			view, err := s.projectView(ctx, proj, counts)
			if err != nil {
				return nil, err
			}
			out = append(out, view)
		}
	}
	return &serverv1.ListProjectsResponse{Projects: out}, nil
}

// GetProject 项目投影（读面门）。
func (s *ProjectsService) GetProject(ctx context.Context, req *serverv1.GetProjectRequest) (*serverv1.GetProjectResponse, error) {
	proj, err := s.getProjectForRead(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	view, err := s.projectView(ctx, proj, nil)
	if err != nil {
		return nil, err
	}
	return &serverv1.GetProjectResponse{Project: view}, nil
}

// UpdateProject 仅改 name/description（slug 不可变——请求无 slug 字段）。
func (s *ProjectsService) UpdateProject(ctx context.Context, req *serverv1.UpdateProjectRequest) (*serverv1.UpdateProjectResponse, error) {
	proj, err := s.getProjectForRead(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if _, err := requireTeamOwner(ctx, s.st, proj.TeamID); err != nil {
		return nil, err
	}
	updated, err := s.st.UpdateProject(ctx, state.ProjectUpdate{
		ID:          req.GetId(),
		Name:        req.GetName(),
		Description: req.GetDescription(),
		ActorUserID: principalOf(ctx).UserID,
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	view, err := s.projectView(ctx, updated, nil)
	if err != nil {
		return nil, err
	}
	return &serverv1.UpdateProjectResponse{Project: view}, nil
}

// DeleteProject 删除空项目（state 非空守卫：apps 非 tombstone /
// db_instances 非 deleted 终态阻塞——资源先迁走或删光，不做隐式级联）。
func (s *ProjectsService) DeleteProject(ctx context.Context, req *serverv1.DeleteProjectRequest) (*serverv1.DeleteProjectResponse, error) {
	proj, err := s.getProjectForRead(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	p, err := requireTeamOwner(ctx, s.st, proj.TeamID)
	if err != nil {
		return nil, err
	}
	if err := s.st.DeleteProject(ctx, req.GetId(), p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.DeleteProjectResponse{}, nil
}

// ListProjectMembers 覆写行列表（读面门；无覆写行的成员不在列表——其权限
// 走团队角色）。
func (s *ProjectsService) ListProjectMembers(ctx context.Context, req *serverv1.ListProjectMembersRequest) (*serverv1.ListProjectMembersResponse, error) {
	proj, err := s.getProjectForRead(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	members, err := s.st.ListProjectMembers(ctx, proj.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.ProjectMemberView, 0, len(members))
	for _, m := range members {
		u, _ := s.st.GetUser(ctx, m.UserID) // 属主行缺失按空投影（不阻塞列表）
		out = append(out, projectMemberView(m, u))
	}
	return &serverv1.ListProjectMembersResponse{Members: out}, nil
}

// SetProjectMemberRole 队内覆写 upsert（团队 admin/owner；目标须为团队
// 成员；owner 不可覆写——守卫全在 state 原语同事务）。
func (s *ProjectsService) SetProjectMemberRole(ctx context.Context, req *serverv1.SetProjectMemberRoleRequest) (*serverv1.SetProjectMemberRoleResponse, error) {
	if !validProjectRoleValue(req.GetRole()) {
		return nil, statusInvalidArgument("project role must be one of: admin, developer, viewer (owners cannot be overridden)")
	}
	proj, err := s.getProjectForRead(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	p, err := requireProjectOverrideManager(ctx, s.st, proj.TeamID)
	if err != nil {
		return nil, err
	}
	m, err := s.st.SetProjectMemberRole(ctx, proj.ID, req.GetUserId(), req.GetRole(), p.UserID, callerTokenID(ctx))
	if err != nil {
		return nil, mapStoreErr(err)
	}
	u, _ := s.st.GetUser(ctx, m.UserID) // 属主行缺失按空投影（不阻塞响应）
	return &serverv1.SetProjectMemberRoleResponse{Member: projectMemberView(m, u)}, nil
}

// RemoveProjectMember 删除覆写行（该成员回退用团队角色）。
func (s *ProjectsService) RemoveProjectMember(ctx context.Context, req *serverv1.RemoveProjectMemberRequest) (*serverv1.RemoveProjectMemberResponse, error) {
	proj, err := s.getProjectForRead(ctx, req.GetProjectId())
	if err != nil {
		return nil, err
	}
	p, err := requireProjectOverrideManager(ctx, s.st, proj.TeamID)
	if err != nil {
		return nil, err
	}
	if err := s.st.RemoveProjectMember(ctx, proj.ID, req.GetUserId(), p.UserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.RemoveProjectMemberResponse{}, nil
}

// ── 资源改派面（W2-S3，rbac-teams §3.4/§5；平台管理员专属）──────────────────

// requireMoveAdmin 是改派面权限门：平台管理员（用户 principal；机具令牌
// 恒 403——改派是平台管理员的迁移权，§3.2「全团队项目只读 + 认领迁移权」
// 的写面例外，职责分离纪律下唯一由平台管理员执行的资源写动作）。
func requireMoveAdmin(ctx context.Context, st *state.Store) (Principal, error) {
	p, err := requireTeamUser(ctx)
	if err != nil {
		return Principal{}, err
	}
	if !isPlatformAdminUser(ctx, st) {
		return Principal{}, statusEnvelope(codes.PermissionDenied,
			"resource move requires a platform administrator (machine tokens are not eligible)")
	}
	return p, nil
}

// resolveAppRefForMove 解析改派目标 app（裸名域内唯一 / team/prj/app 限定
// 形 / 平台 ID；限定形读面在此落位——改派是管理面，D-W0-9 恒可解析形态）。
func (s *ProjectsService) resolveAppRefForMove(ctx context.Context, ref string) (state.App, error) {
	if strings.Contains(ref, "/") {
		teamSlug, rest, err := cutQualifiedRef(ref)
		if err != nil {
			return state.App{}, statusInvalidArgument(err.Error())
		}
		prjSlug, appName, err := cutQualifiedRef(rest)
		if err != nil {
			return state.App{}, statusInvalidArgument(err.Error())
		}
		proj, err := resolveQualifiedProject(ctx, s.st, teamSlug, prjSlug)
		if err != nil {
			return state.App{}, err
		}
		app, err := s.st.GetAppByNameInProject(ctx, proj.ID, appName)
		if err != nil {
			return state.App{}, mapStoreErr(err, ref)
		}
		return app, nil
	}
	app, err := s.st.GetAppByID(ctx, ref)
	if err == nil {
		return app, nil
	}
	if !errors.Is(err, state.ErrAppNotFound) {
		return state.App{}, err
	}
	row, err := s.st.GetAppByName(ctx, ref)
	if err != nil {
		if errors.Is(err, state.ErrAppAmbiguous) {
			return state.App{}, apperr.New("E_APP_AMBIGUOUS",
				"app %q resolves to multiple rows across projects; use the team/prj/app qualified form or the platform id", ref).
				WithContext("app", ref)
		}
		return state.App{}, mapStoreErr(err, ref)
	}
	return row, nil
}

// cutQualifiedRef 按 '/' 拆分限定引用的一段（空段/越界 → 400 语义错误）。
func cutQualifiedRef(ref string) (string, string, error) {
	i := strings.IndexByte(ref, '/')
	if i <= 0 || i == len(ref)-1 {
		return "", "", fmt.Errorf("qualified reference %q must be team/prj/... form", ref)
	}
	return ref[:i], ref[i+1:], nil
}

// MoveApp 资源改派（平台管理员；写归属 + 换名重部署——编排组合：
// ①改派前捕获旧归属上下文（旧限定形 label 与旧 slug——旧名清扫与摘网的
// 参数）→ ②state.MoveApp 同事务审计（app.moved，跨项目唯一性 409）→
// ③入队重部署（正常发布管线在新命名上下文产出快照/revision——无成功部署
// 史 = 无底座对象随迁，accepted 形态）→ ④等待新名服务就位 → ⑤摘旧名
// 服务 + 摘旧网（best-effort——摘网失败降级日志）。④超时 = 保守放弃清扫
// （流量仍在旧名上，删旧 = 主动停机）并以错误应答——改派半程由「重部署
// 已入队 + 归属已切换」承载，人工重跑 MoveApp 同参数即幂等收尾（幂等面：
// ②同项目返回 ErrMoveSameTarget、①捕获以当前行归属为准——报告偏差记录）。
func (s *ProjectsService) MoveApp(ctx context.Context, req *serverv1.MoveAppRequest) (*serverv1.MoveAppResponse, error) {
	p, err := requireMoveAdmin(ctx, s.st)
	if err != nil {
		return nil, err
	}
	if s.appMove == nil {
		return nil, statusEnvelope(codes.Unavailable, "move orchestration is not assembled in this build")
	}
	app, err := s.resolveAppRefForMove(ctx, req.GetApp())
	if err != nil {
		return nil, err
	}
	toProj, err := s.st.GetProject(ctx, req.GetToProjectId())
	if err != nil {
		return nil, mapStoreErr(err)
	}
	// ① 旧上下文捕获（行上 slug 是旧归属——MoveApp 原语落库前的快照）。
	from := app
	// ② 归属切换（state 同事务审计 app.moved；同项目 409）。
	moved, err := s.st.MoveApp(ctx, app.ID, toProj.ID, p.UserID, callerTokenID(ctx))
	if err != nil {
		if errors.Is(err, state.ErrMoveSameTarget) {
			// 裹链原文进 conflict 信封（message = err.Error()，非固定文案
			// ——登记表外特例）。
			return nil, conflict(err.Error())
		}
		// ErrAppExists 走登记行（跨项目名字占用 409）；其余原样透传。
		return nil, mapStoreErr(err, app.Name)
	}
	resp := &serverv1.MoveAppResponse{
		App:           moved.Name,
		FromProjectId: from.ProjectID,
		ToProjectId:   toProj.ID,
		ToProject:     toProj.Slug,
	}
	// ③ 入队重部署（无成功部署史 = accepted：无底座对象随迁）。
	deployID, err := s.appMove.EnqueueMoveRedeploy(ctx, moved.ID)
	switch {
	case err == nil:
		resp.DeploymentId = deployID
	case errors.Is(err, engine.ErrNoRedeploySource):
		resp.Status = "accepted"
		return resp, nil
	default:
		return nil, err
	}
	// ④ 等待新命名服务就位（预算超时 = 保守放弃清扫，错误应答）。
	if err := s.appMove.AwaitAppSwap(ctx, moved.ID, MoveAwaitTimeout); err != nil {
		return nil, err
	}
	// ⑤ 摘旧名服务 + 摘旧网（旧网 best-effort——引用未清场时下一拍/人工）。
	if _, err := s.appMove.SweepMovedServices(ctx, from.QualifiedName()); err != nil {
		return nil, err
	}
	if s.ingMove != nil {
		if err := s.ingMove.DetachAppNetwork(ctx, from.TeamSlug, from.ProjectSlug, from.Name); err != nil {
			// 摘网失败不回滚改派（旧网无服务挂接后即成无害空网；收尾重试面）。
			_ = err
		}
	}
	resp.Status = "moved"
	return resp, nil
}

// resolveDatabaseRefForMove 解析改派目标库实例（同 resolveAppRefForMove 的
// 三形态；裸名多行命中 E_APP_AMBIGUOUS）。
func (s *ProjectsService) resolveDatabaseRefForMove(ctx context.Context, ref string) (state.DatabaseInstance, error) {
	if strings.Contains(ref, "/") {
		parts := strings.Split(ref, "/")
		if len(parts) != 3 {
			return state.DatabaseInstance{}, statusInvalidArgument("database reference must be team/prj/name form")
		}
		proj, err := resolveQualifiedProject(ctx, s.st, parts[0], parts[1])
		if err != nil {
			return state.DatabaseInstance{}, err
		}
		inst, err := s.st.GetDatabaseInstanceByNameInProject(ctx, proj.ID, parts[2])
		if err != nil {
			if errors.Is(err, state.ErrDatabaseNotFound) {
				return state.DatabaseInstance{}, databaseNotFound(parts[2], "not found in project "+proj.Slug)
			}
			return state.DatabaseInstance{}, err
		}
		return inst, nil
	}
	inst, err := s.st.GetDatabaseInstance(ctx, ref)
	if err == nil {
		return inst, nil
	}
	if !errors.Is(err, state.ErrDatabaseNotFound) {
		return state.DatabaseInstance{}, err
	}
	row, err := s.st.GetDatabaseInstanceByName(ctx, ref)
	if err != nil {
		if errors.Is(err, state.ErrDatabaseAmbiguous) {
			return state.DatabaseInstance{}, apperr.New("E_APP_AMBIGUOUS",
				"database %q resolves to multiple rows across projects; use the team/prj/name qualified form or the platform id", ref).
				WithContext("database", ref)
		}
		return state.DatabaseInstance{}, databaseNotFound(ref, "not found")
	}
	return row, nil
}

// MoveDatabase 资源改派（平台管理员；写归属 + 换名重部署——旧名服务移除
// （卷独占：短暂停机窗口）→ 旧 secret/网络清场 → 新名收敛等待；库卷公式
// 不变 = 零卷迁移）。
func (s *ProjectsService) MoveDatabase(ctx context.Context, req *serverv1.MoveDatabaseRequest) (*serverv1.MoveDatabaseResponse, error) {
	p, err := requireMoveAdmin(ctx, s.st)
	if err != nil {
		return nil, err
	}
	if s.dbMove == nil {
		return nil, statusEnvelope(codes.Unavailable, "move orchestration is not assembled in this build")
	}
	inst, err := s.resolveDatabaseRefForMove(ctx, req.GetDatabase())
	if err != nil {
		return nil, err
	}
	toProj, err := s.st.GetProject(ctx, req.GetToProjectId())
	if err != nil {
		return nil, mapStoreErr(err)
	}
	from := inst
	moved, err := s.st.MoveDatabase(ctx, inst.ID, toProj.ID, p.UserID, callerTokenID(ctx))
	if err != nil {
		if errors.Is(err, state.ErrMoveSameTarget) {
			return nil, conflict(err.Error())
		}
		if errors.Is(err, state.ErrDatabaseExists) {
			return nil, conflict(fmt.Sprintf("database %q already exists in the target project (names are unique per project); choose another target or rename first", inst.Name))
		}
		return nil, err
	}
	if err := s.dbMove.MoveDatabaseRedeploy(ctx, moved, from.TeamSlug, from.ProjectSlug); err != nil {
		return nil, err
	}
	return &serverv1.MoveDatabaseResponse{
		Database:      moved.Name,
		FromProjectId: from.ProjectID,
		ToProjectId:   toProj.ID,
		ToProject:     toProj.Slug,
		Status:        "moved",
	}, nil
}

// ── 投影 helpers ─────────────────────────────────────────────────────────────

// projectView 是 state.Project → proto 投影（team_slug 补全——限定形
// team/project 展示面，D-W0-9；团队行缺失按空串投影，不阻塞列表。
// IMPL-T15-1 增项目网投影：network_name = 项目网 overlay 名（公式现推，
// 免查询）；network_members = 参与位在位的 active app 数）。counts 为批量
// 成员计数（ListProjects 单查询共享；nil = 单行路径现查）。
func (s *ProjectsService) projectView(ctx context.Context, p state.Project, counts map[string]int) (*serverv1.ProjectView, error) {
	teamSlug := ""
	if t, err := s.st.GetTeam(ctx, p.TeamID); err == nil {
		teamSlug = t.Slug
	} else if !errors.Is(err, state.ErrTeamNotFound) {
		return nil, err
	}
	network, err := naming.ProjectNetworkName(p.ID)
	if err != nil {
		return nil, err
	}
	if counts == nil {
		counts, err = s.st.ProjectNetworkMemberCounts(ctx)
		if err != nil {
			return nil, err
		}
	}
	// 成员计数窄化（gosec G115）：上界 = 项目内 app 数，显式钳制到 int32
	// 量程内（实际远不可能触顶）。
	members := counts[p.ID]
	if members > math.MaxInt32 {
		members = math.MaxInt32
	}
	return &serverv1.ProjectView{
		Id:             p.ID,
		TeamId:         p.TeamID,
		TeamSlug:       teamSlug,
		Slug:           p.Slug,
		Name:           p.Name,
		Description:    p.Description,
		CreatedAt:      tstamp(p.CreatedAt),
		NetworkName:    network,
		NetworkMembers: int32(members), //nolint:gosec // G115：已显式钳制（见上）
	}, nil
}

// projectMemberView 是 state.ProjectMember → proto 投影（属主投影随行补全；
// 属主行缺失按空投影——不阻塞列表）。
func projectMemberView(m state.ProjectMember, u state.User) *serverv1.ProjectMemberView {
	return &serverv1.ProjectMemberView{
		ProjectId:   m.ProjectID,
		UserId:      m.UserID,
		Role:        m.Role,
		CreatedAt:   tstamp(m.CreatedAt),
		Email:       u.Email,
		DisplayName: u.DisplayName,
	}
}
