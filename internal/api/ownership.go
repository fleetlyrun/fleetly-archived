package api

// 资源归属解析与改派编排端口（v0.3 W2-S3，rbac-teams §3.4/§4.2 D-W0-9 解
// 析规则 + D-W0-4 二修）：
//
//   - project 引用 = 裸名或限定形 `team/prj`：限定形恒可解析；裸名仅在解
//     析域（用户 = 可见项目集；机具令牌/平台管理员 = 全库）内唯一时可用，
//     多命中 → E_PROJECT_AMBIGUOUS（错误 context 列候选 `team/prj`）；
//   - 用户 principal 缺省 project = 个人队默认项目 `default`；机具令牌
//     （UserID 空）缺省无——必须显式携带，否则 400 带指引；
//   - MoveApp / MoveDatabase 改派 = 写归属 + 换名重部署（编排经端口委托
//     engine/database/ingress——api 定义端口、不感知实现类型）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// DefaultProjectSlug 是注册默认项目 slug（state.DefaultProjectSlug 同源——
// 本常量保留为 api 面字面引用点；两者由 projects_test 钉同值）。
const DefaultProjectSlug = "default"

// ── 角色门：ResolvePermission 单点（v0.3 W2-S4，rbac-teams §4.2 双门的
// 第 2 门）─────────────────────────────────────────────────────────────────────
//
// 双门结构（§4.2）：第 1 门 token scope（拦截器，逐字不动）→ 第 2 门角色门
//（本节，资源方法触发）。角色是 scope 之上的归属解析层（§3.2）：
//
//   - 机具令牌（user NULL）= 全库 admin 等价（W2-S2 已定设计语义，非兼容
//     残留）——资源面不受角色门收缩，scope 门照旧；
//   - 平台管理员（is_platform_admin）= 全团队项目**只读**（support 视角），
//     不代团队写（写面 403，职责分离）；
//   - 用户 = project_members 覆写行优先（§3.3 B 形：有行则覆写、双向生效），
//     无行走 team_members 团队角色；owner/admin → levelAdmin，developer →
//     levelDeploy，viewer → levelRead；
//   - fail-closed：解析不出（无成员关系/归属缺失/读故障）即拒。
//
// 调用纪律：资源 handler 统一经 requireResourceAccess（或其资源形态便捷
// 封装 requireAppAccess / requireDatabaseAccess）进入本单点——不得在各自
// handler 内散落成员关系查询（§4.2「解析收口单点」）。

// accessLevel 是资源方法所需/角色所授的权限层级（§3.2 矩阵的方法级投影；
// 与 scope 词表的映射见 accessLevelForScope——单一映射点）。
type accessLevel int

const (
	levelNone accessLevel = iota // 无权限（fail-closed 解析失败的返回值）
	levelRead                    // 只读（viewer / read scope）
	levelDeploy                  // 读 + 部署/回滚/env 写/cron/终端（developer / deploy⊕terminal scope）
	levelAdmin                   // 全部资源面（admin/owner / admin scope）
)

// allows 报告所授层级是否满足所需层级（序：read < deploy < admin）。
func (l accessLevel) allows(need accessLevel) bool { return l >= need }

// accessLevelForScope 把方法所需 scope（scope.go 登记的唯一来源）映射为资
// 源面层级。terminal 是独立 scope（机具最小权限保留），对人类角色按 §3.2
// 矩阵「终端 developer+」落 levelDeploy；未登记/未知 scope 按 levelAdmin
// fail-closed（与拦截器未登记按 admin 拒的同款兜底）。
func accessLevelForScope(scope string) accessLevel {
	switch scope {
	case ScopeRead:
		return levelRead
	case ScopeDeploy, ScopeTerminal:
		return levelDeploy
	default:
		return levelAdmin
	}
}

// roleAccessLevel 把角色词表映射为资源层级（§3.2 矩阵列直读；团队与项目
// 覆写两词表同值同映射——admin/developer/viewer 三值两表同源；未知角色值
// 返回 levelNone——fail-closed）。
func roleAccessLevel(role string) accessLevel {
	switch role {
	case state.TeamRoleOwner:
		return levelAdmin
	case state.TeamRoleAdmin:
		// admin 值同项目覆写词表的 ProjectRoleAdmin（同串同映射）。
		return levelAdmin
	case state.TeamRoleDeveloper:
		return levelDeploy
	case state.TeamRoleViewer:
		return levelRead
	default:
		return levelNone
	}
}

// ResolvePermission 解析调用方在目标项目上的所授层级（§4.2 解析单点；判
// 定序：机具令牌 → 平台管理员 → 覆写行 → 团队角色；fail-closed）。projectID
// 为空（理论不可达——00019 起归属 NOT NULL）返回 levelNone。
func ResolvePermission(ctx context.Context, st *state.Store, projectID string) (accessLevel, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return levelNone, nil
	}
	if p.UserID == "" {
		// 机具令牌：全库 admin 等价（§2.3 设计语义；scope 门照旧承担形状）。
		return levelAdmin, nil
	}
	if isPlatformAdminUser(ctx, st) {
		// 平台管理员：全团队项目只读（support 视角，§3.2）；写面在下方层级
		// 判定中被拒（不代团队写——职责分离）。
		return levelRead, nil
	}
	if projectID == "" {
		return levelNone, nil
	}
	proj, err := st.GetProject(ctx, projectID)
	if err != nil {
		return levelNone, err // 归属解析不出即拒（fail-closed，§4.2）
	}
	// 覆写行优先（§3.3 B 形：有行则覆写、双向生效）。
	if m, err := st.GetProjectMembership(ctx, projectID, p.UserID); err == nil {
		return roleAccessLevel(m.Role), nil
	} else if !errors.Is(err, state.ErrProjectMemberNotFound) {
		return levelNone, err
	}
	// 无覆写行：团队角色。
	m, err := st.GetMembership(ctx, proj.TeamID, p.UserID)
	if err != nil {
		if errors.Is(err, state.ErrTeamMemberNotFound) {
			return levelNone, nil // 非成员：无权限（语义是 403，由调用面包装）
		}
		return levelNone, err
	}
	return roleAccessLevel(m.Role), nil
}

// requireResourceAccess 是资源 handler 的统一角色门：按 ctx 携带的方法所需
// scope（拦截器注入，scope.go 登记唯一来源）映射层级，与 ResolvePermission
// 的所授层级判定；不足 → 403（信封退化形态，与鉴权 403 同口径）。
func requireResourceAccess(ctx context.Context, st *state.Store, projectID string) error {
	granted, err := ResolvePermission(ctx, st, projectID)
	if err != nil {
		return err
	}
	need := accessLevelForScope(requiredScopeFromContext(ctx))
	if !granted.allows(need) {
		return statusEnvelope(codes.PermissionDenied,
			fmt.Sprintf("insufficient role on this project (requires %s; resolved %s) — ask a team owner for a higher role", accessLevelName(need), accessLevelName(granted)))
	}
	return nil
}

// requireAppAccess / requireDatabaseAccess 是两类资源行的门形态便捷封装
//（一律内部转发 ResolvePermission 单点）。
func requireAppAccess(ctx context.Context, st *state.Store, app state.App) error {
	return requireResourceAccess(ctx, st, app.ProjectID)
}

func requireDatabaseAccess(ctx context.Context, st *state.Store, inst state.DatabaseInstance) error {
	return requireResourceAccess(ctx, st, inst.ProjectID)
}

// accessLevelName 是层级的人读名（错误指引用）。
func accessLevelName(l accessLevel) string {
	switch l {
	case levelRead:
		return "read"
	case levelDeploy:
		return "deploy"
	case levelAdmin:
		return "admin"
	default:
		return "none"
	}
}

// ── 资源引用解析（可见域感知；D-W0-9 解析规则在资源引用上的推广）──────────────
//
// 引用形态（与 resolveProjectRef 同规）：
//   - 限定形恒可解析（app 三段 team/prj/app；库同形 team/prj/db）——解析是
//     寻址不是授权，越权在角色门（requireResourceAccess）收口；
//   - 裸名仅解析域内唯一时可用，解析域 = 用户可见项目集（成员团队归属，
//     §3.3 可见性零级联）/ 全库（机具令牌与平台管理员）；多命中 →
//     E_APP_AMBIGUOUS（候选列 team/prj/app）；零命中 → 404（不泄漏不可见
//     资源的存在性）。
//   - ID 形态：项目引用支持（D-W0-9 管理面惯例）；app/库引用自 2026-09-25
//     起支持平台 ID 短路（26 字符 ULID 精确命中即按 id 解析）——Console
//     详情导航以 id 寻址：同名 app 的裸名解析必然歧义（D-W0-4 二修），而
//     REST 单段路由参数吃不下 team/prj/app 三段限定形，id 是单段可承载的
//     唯一精确引用。寻址不是授权——越权仍由 requireResourceAccess 收口；
//     id 不存在按 404 报（不泄漏存在性）。

// callerIsGlobal 报告调用方的裸名解析域是否为全库（机具令牌 = 设计语义全库
// admin 等价；平台管理员 = 全库只读 support 视角）。读故障 fail-closed 按
// 非全域处理（收敛到用户可见域，两域的交集就是更严的可见域）。
func callerIsGlobal(ctx context.Context, st *state.Store) bool {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return false
	}
	if p.UserID == "" {
		return true
	}
	return isPlatformAdminUser(ctx, st)
}

// resolveApp 按引用取应用行（api 面统一入口；v0.3 W2-S4 起可见域感知——
// 裸名解析域 = 调用方可见项目集，机具令牌/平台管理员 = 全库）。限定形
// team/prj/app 恒可解析（寻址）；26 字符 ULID 按 id 精确解析（Console
// 详情导航面，头注 ID 形态）；NotFound 语义归一（errors.go 哨兵登记表）。
func resolveApp(ctx context.Context, st *state.Store, ref string) (state.App, error) {
	ref = strings.TrimSpace(ref)
	if teamSlug, rest, found := strings.Cut(ref, "/"); found {
		prjSlug, appName, _ := strings.Cut(rest, "/")
		if prjSlug != "" && appName != "" {
			return resolveQualifiedApp(ctx, st, teamSlug, prjSlug, appName)
		}
	}
	// id 形态：26 字符规范 ULID 精确命中（id 全库唯一，无歧义面）。
	if _, perr := ulid.ParseStrict(ref); perr == nil {
		app, err := st.GetAppByID(ctx, ref)
		if err != nil {
			return state.App{}, mapStoreErr(err, ref)
		}
		return app, nil
	}
	// 裸名：解析域内唯一才可用。
	if callerIsGlobal(ctx, st) {
		app, err := st.GetAppByName(ctx, ref)
		if err != nil {
			if errors.Is(err, state.ErrAppAmbiguous) {
				return state.App{}, appAmbiguousErr(ctx, st, ref)
			}
			return state.App{}, mapStoreErr(err, ref)
		}
		return app, nil
	}
	return resolveAppInVisibleProjects(ctx, st, ref)
}

// resolveQualifiedApp 解析三段限定形 team/prj/app（两段 slug 精确匹配）。
func resolveQualifiedApp(ctx context.Context, st *state.Store, teamSlug, prjSlug, appName string) (state.App, error) {
	proj, err := resolveQualifiedProject(ctx, st, teamSlug, prjSlug)
	if err != nil {
		return state.App{}, err
	}
	app, aerr := st.GetAppByNameInProject(ctx, proj.ID, appName)
	if aerr != nil {
		return state.App{}, mapStoreErr(aerr, teamSlug+"/"+prjSlug+"/"+appName)
	}
	return app, nil
}

// resolveAppInVisibleProjects 在用户可见项目集内解析裸 app 名（域内唯一才
// 可用；多命中 E_APP_AMBIGUOUS 列候选，零命中 404——不泄漏不可见资源）。
func resolveAppInVisibleProjects(ctx context.Context, st *state.Store, name string) (state.App, error) {
	projects, err := visibleProjectsForUser(ctx, st, principalUserIDOf(ctx))
	if err != nil {
		return state.App{}, err
	}
	var hits []state.App
	for _, proj := range projects {
		app, aerr := st.GetAppByNameInProject(ctx, proj.ID, name)
		if errors.Is(aerr, state.ErrAppNotFound) {
			continue
		}
		if aerr != nil {
			return state.App{}, mapStoreErr(aerr, name)
		}
		hits = append(hits, app)
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return state.App{}, notFound(fmt.Sprintf("app %q not found in your visible projects (pass team/prj/app to resolve across teams)", name))
	default:
		return state.App{}, appAmbiguousErr(ctx, st, name)
	}
}

// appAmbiguousErr 构造 app 裸名歧义信封（候选列 team/prj/app——可行动指引；
// 归属 slug 缺失的行退化列平台 ID，不阻塞错误面）。候选列在本站点枚举，
// 文案与信封经 ambiguousRefErr 统一产出（IMPL-ARCH-L：注册码 detail 全仓
// 唯一构造点）。
func appAmbiguousErr(ctx context.Context, st *state.Store, name string) error {
	var rows []state.App
	if apps, err := st.ListAppRowsByName(ctx, name); err == nil {
		rows = apps
	}
	candidates := make([]string, 0, len(rows))
	for _, app := range rows {
		if app.TeamSlug == "" || app.ProjectSlug == "" {
			candidates = append(candidates, app.ID)
			continue
		}
		candidates = append(candidates, app.QualifiedName())
	}
	return ambiguousRefErr("app", "team/prj/app", name, candidates)
}

// resolveDatabaseRef 按引用取库实例行（app 面同款可见域感知；限定形
// team/prj/db 恒可解析，裸名解析域内唯一，多命中 E_APP_AMBIGUOUS——库族
// 与 app 族同码，D-W0-4 二修的语义是「资源名 project 内唯一」共用的歧义
// 指引码）。
func resolveDatabaseRef(ctx context.Context, st *state.Store, ref string) (state.DatabaseInstance, error) {
	ref = strings.TrimSpace(ref)
	if teamSlug, rest, found := strings.Cut(ref, "/"); found {
		prjSlug, dbName, _ := strings.Cut(rest, "/")
		if prjSlug != "" && dbName != "" {
			proj, err := resolveQualifiedProject(ctx, st, teamSlug, prjSlug)
			if err != nil {
				return state.DatabaseInstance{}, err
			}
			inst, ierr := st.GetDatabaseInstanceByNameInProject(ctx, proj.ID, dbName)
			if ierr != nil {
				return state.DatabaseInstance{}, mapStoreErr(ierr)
			}
			return inst, nil
		}
	}
	// id 形态：26 字符规范 ULID 精确命中（app 族同款，resolveApp 头注 ID 形态）。
	if _, perr := ulid.ParseStrict(ref); perr == nil {
		inst, err := st.GetDatabaseInstanceByID(ctx, ref)
		if err != nil {
			return state.DatabaseInstance{}, mapStoreErr(err)
		}
		return inst, nil
	}
	if callerIsGlobal(ctx, st) {
		inst, err := st.GetDatabaseInstanceByName(ctx, ref)
		if err != nil {
			if errors.Is(err, state.ErrDatabaseAmbiguous) {
				return state.DatabaseInstance{}, ambiguousRefErr("database", "team/prj/db", ref, nil)
			}
			return state.DatabaseInstance{}, mapStoreErr(err)
		}
		return inst, nil
	}
	// 用户可见域内解析。
	projects, err := visibleProjectsForUser(ctx, st, principalUserIDOf(ctx))
	if err != nil {
		return state.DatabaseInstance{}, err
	}
	var hits []state.DatabaseInstance
	for _, proj := range projects {
		inst, ierr := st.GetDatabaseInstanceByNameInProject(ctx, proj.ID, ref)
		if errors.Is(ierr, state.ErrDatabaseNotFound) {
			continue
		}
		if ierr != nil {
			return state.DatabaseInstance{}, mapStoreErr(ierr)
		}
		hits = append(hits, inst)
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return state.DatabaseInstance{}, databaseNotFound(ref, "not found in your visible projects (pass team/prj/db to resolve across teams)")
	default:
		return state.DatabaseInstance{}, ambiguousRefErr("database", "team/prj/db", ref, nil)
	}
}

// isNotFoundErr 报告 err 是否为 404 退化信封（resolveApp 的「行不在册」
// 形态——DeployFromGit 首发放行的判定输入；稳定码 apperr 不在此列）。
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return status.Code(err) == codes.NotFound
}

// principalUserIDOf 取 ctx principal 的 UserID（无 principal 返回空——调用
// 面均为已过鉴权链的 handler，形态性兜底）。
func principalUserIDOf(ctx context.Context) string {
	if p, ok := PrincipalFromContext(ctx); ok {
		return p.UserID
	}
	return ""
}

// visibleProjectFilter 是列表面的可见性过滤器（ListApps/ListDatabases 共用）：
//   - 全局调用方（机具令牌/平台管理员）且未携带 project 收窄 → nil（全库
//     不过滤——map 查询不可与 nil 区分，故用 nil 表达）；
//   - 用户 principal → 可见项目集（成员团队归属）；携带 project 引用时与
//     收窄目标取交集（限定形/域内唯一裸名，resolveProjectRef 单点解析；
//     解析到不可见项目 = 空集——不泄漏）。
func visibleProjectFilter(ctx context.Context, st *state.Store, projectRef string) (map[string]bool, error) {
	global := callerIsGlobal(ctx, st)
	if global && projectRef == "" {
		return nil, nil
	}
	var visible map[string]bool
	if !global {
		projects, err := visibleProjectsForUser(ctx, st, principalUserIDOf(ctx))
		if err != nil {
			return nil, err
		}
		visible = make(map[string]bool, len(projects))
		for _, proj := range projects {
			visible[proj.ID] = true
		}
	}
	if projectRef == "" {
		return visible, nil
	}
	proj, err := resolveProjectRef(ctx, st, projectRef)
	if err != nil {
		return nil, err
	}
	narrowed := map[string]bool{proj.ID: true}
	if visible != nil && !visible[proj.ID] {
		return map[string]bool{}, nil // 收窄目标不可见：空集（不泄漏存在性）
	}
	return narrowed, nil
}

// ── 可达 scope 集单点（会话凭据口径 + PAT 防呆校验共用；v0.3 W2-S4 收口）───────

// reachableScopesForUser 计算用户的可达 scope 集（§3.2 角色→scope 蕴含；
// auth.go 会话凭据与 tokens.go PAT 声明校验的唯一来源——W2-S2 的临时口径
// 在此收口）：
//   - 平台管理员 = 全集（平台面 scope 门形状）；
//   - 否则 = 团队角色与全部队内覆写行角色蕴含的**并集**（覆写升级——团队
//     viewer 某项目升 developer——必须抬升可达集，否则升向在 scope 门被截，
//     覆写双向生效失效；精确的按项目收缩在第 2 门角色门硬性生效，本集只是
//     形状上界）；
//   - 无任何成员关系的用户 = {read} 最小集（viewer 等价）；
//   - 读故障按已并集收敛（fail-closed）。
func reachableScopesForUser(ctx context.Context, st *state.Store, userID string) map[string]bool {
	reachable := map[string]bool{ScopeRead: true}
	// 平台管理员判定直查用户行（不经过 isPlatformAdminUser——本函数在会话
	// 认证期调用，彼时 principal 尚未入 ctx，按 principal 读会恒 false）。
	if u, err := st.GetUser(ctx, userID); err == nil && u.IsPlatformAdmin && u.DisabledAt.IsZero() {
		return map[string]bool{ScopeRead: true, ScopeDeploy: true, ScopeTerminal: true, ScopeAdmin: true}
	}
	merge := func(role string) {
		// 注：项目覆写三档与团队角色后三档同串（admin/developer/viewer），
		// switch 不必重复列 ProjectRole* 常量（同值 case 编译期报重复）。
		switch role {
		case state.TeamRoleOwner, state.TeamRoleAdmin:
			reachable[ScopeAdmin] = true
			reachable[ScopeDeploy] = true
			reachable[ScopeTerminal] = true
		case state.TeamRoleDeveloper:
			reachable[ScopeDeploy] = true
			reachable[ScopeTerminal] = true
		case state.TeamRoleViewer:
			// read 已在缺省集。
		}
	}
	if memberships, err := st.ListUserMemberships(ctx, userID); err == nil {
		for _, m := range memberships {
			merge(m.Role)
		}
	}
	if overrides, err := st.ListProjectMembershipsByUser(ctx, userID); err == nil {
		for _, m := range overrides {
			merge(m.Role)
		}
	}
	return reachable
}

// reachableScopeList 是可达集的人读清单（错误指引用；固定词表序）。
func reachableScopeList(reachable map[string]bool) string {
	order := []string{ScopeRead, ScopeDeploy, ScopeTerminal, ScopeAdmin}
	out := make([]string, 0, len(order))
	for _, s := range order {
		if reachable[s] {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// resolveProjectRef 解析 project 引用为项目行。ref 空 = 缺省解析（见上）。
// 不存在 → 404 退化信封；多命中 → E_PROJECT_AMBIGUOUS。
func resolveProjectRef(ctx context.Context, st *state.Store, ref string) (state.Project, error) {
	p, ok := PrincipalFromContext(ctx)
	userSide := ok && p.UserID != ""
	ref = strings.TrimSpace(ref)

	// 缺省解析：用户 = 个人队 default 项目；机具令牌无缺省（必须显式）。
	if ref == "" {
		if !userSide {
			return state.Project{}, statusInvalidArgument(
				"machine tokens have no default project: pass project \"team/project\" (or \"project\" when unambiguous) on deploy/database-create requests")
		}
		return defaultProjectForUser(ctx, st, p.UserID)
	}

	// 限定形 team/prj：恒可解析（D-W0-9）。
	if teamSlug, prjSlug, ok := strings.Cut(ref, "/"); ok {
		return resolveQualifiedProject(ctx, st, teamSlug, prjSlug)
	}

	// 裸名：解析域内唯一才可用。解析域 = 用户可见项目集（成员团队归属）/
	// 全库（机具令牌与平台管理员）。ID 形态优先直查（D-W0-9：管理面/脚本
	// 用 ID，免疫同名歧义）——查无再按 slug 解析。
	if proj, err := st.GetProject(ctx, ref); err == nil {
		return proj, nil
	}
	var projects []state.Project
	var err error
	if userSide && !isPlatformAdminUser(ctx, st) {
		projects, err = visibleProjectsForUser(ctx, st, p.UserID)
	} else {
		projects, err = st.ListProjects(ctx)
	}
	if err != nil {
		return state.Project{}, err
	}
	var hits []state.Project
	for _, proj := range projects {
		if proj.Slug == ref {
			hits = append(hits, proj)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return state.Project{}, notFound(fmt.Sprintf("project %q not found in your visible projects (pass \"team/project\" to resolve across teams)", ref))
	default:
		candidates := make([]string, 0, len(hits))
		for _, proj := range hits {
			slug, serr := qualifiedProjectSlug(ctx, st, proj)
			if serr != nil {
				return state.Project{}, serr
			}
			candidates = append(candidates, slug)
		}
		sort.Strings(candidates)
		return state.Project{}, apperr.New("E_PROJECT_AMBIGUOUS",
			"project %q matches %d visible projects; qualify the reference as team/project",
			ref, len(hits)).
			WithContext("candidates", strings.Join(candidates, ","))
	}
}

// defaultProjectForUser 解析用户的个人队默认项目（state.ResolveUserDefault-
// Project 单点；注册时落位的 owner 队下 slug=default 项目）。无个人队/缺省
// 项目 = 请求侧显式携带 project 的可行动指引。
func defaultProjectForUser(ctx context.Context, st *state.Store, userID string) (state.Project, error) {
	proj, err := st.ResolveUserDefaultProject(ctx, userID)
	if err != nil {
		return state.Project{}, mapStoreErr(err)
	}
	return proj, nil
}

// visibleProjectsForUser 返回用户可见项目集（成员团队的全部项目——可见性
// 零级联，D-W0-2）。
func visibleProjectsForUser(ctx context.Context, st *state.Store, userID string) ([]state.Project, error) {
	memberships, err := st.ListUserMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, m := range memberships {
		allowed[m.TeamID] = true
	}
	all, err := st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]state.Project, 0, len(all))
	for _, proj := range all {
		if allowed[proj.TeamID] {
			out = append(out, proj)
		}
	}
	return out, nil
}

// resolveQualifiedProject 解析限定形 team/prj（两段 slug 精确匹配）。
func resolveQualifiedProject(ctx context.Context, st *state.Store, teamSlug, prjSlug string) (state.Project, error) {
	team, err := st.GetTeamBySlug(ctx, teamSlug)
	if err != nil {
		if errors.Is(err, state.ErrTeamNotFound) {
			return state.Project{}, notFound("team not found: " + teamSlug)
		}
		return state.Project{}, err
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return state.Project{}, err
	}
	for _, proj := range projects {
		if proj.TeamID == team.ID && proj.Slug == prjSlug {
			return proj, nil
		}
	}
	return state.Project{}, notFound(fmt.Sprintf("project %q not found in team %q", prjSlug, teamSlug))
}

// qualifiedProjectSlug 组装项目的 `team/prj` 展示串（候选列表/审计投影）。
func qualifiedProjectSlug(ctx context.Context, st *state.Store, p state.Project) (string, error) {
	t, err := st.GetTeam(ctx, p.TeamID)
	if err != nil {
		return p.Slug, nil // 团队行缺失退化裸名（不阻塞候选列示）
	}
	return t.Slug + "/" + p.Slug, nil
}

// ── MoveApp / MoveDatabase 编排端口（实现 = runtime 装配）──────────────────

// MoveAwaitTimeout 是改派换名交换的等待预算（api 层缺省；覆盖部署管线
// planning+releasing 到任务 running 的常规路径）。
const MoveAwaitTimeout = 120 * time.Second

// AppMovePort 是 MoveApp 换名重部署的引擎编排端口（实现 = engine.Engine，
// internal/engine/move.go；接口在 api 定义——方向纪律同 GitDeployTriggers）。
type AppMovePort interface {
	// EnqueueMoveRedeploy 归属切换后沿正常发布管线入队重部署；无成功部署
	// 史返回 engine.ErrNoRedeploySource（app 从未发布 = 无对象随迁）。
	EnqueueMoveRedeploy(ctx context.Context, appID string) (string, error)
	// AwaitAppSwap 等待新命名上下文的长驻服务就位（预算内未就位返回错误
	// ——调用方据此放弃旧服务清扫，流量仍在旧名上）。
	AwaitAppSwap(ctx context.Context, appID string, timeout time.Duration) error
	// SweepMovedServices 摘除旧命名上下文的长驻服务（在途 cron job 豁免）。
	SweepMovedServices(ctx context.Context, oldQualified string) (int, error)
}

// DBMovePort 是 MoveDatabase 换名重部署的收敛编排端口（实现 =
// database.Manager）。
type DBMovePort interface {
	MoveDatabaseRedeploy(ctx context.Context, inst state.DatabaseInstance, oldTeamSlug, oldPrjSlug string) error
}

// IngressMovePort 是 MoveApp 摘旧网的收尾端口（实现 = ingress.Manager；
// best-effort 语义——错误由调用方降级日志，不阻塞改派应答）。
type IngressMovePort interface {
	DetachAppNetwork(ctx context.Context, team, prj, app string) error
}
