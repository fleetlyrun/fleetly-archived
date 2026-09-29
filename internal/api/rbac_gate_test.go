package api

// RBAC 角色门与可见性过滤矩阵测试（v0.3 W2-S4，rbac-teams §3.2/§4.2）：
//
//   - 角色→层级矩阵抽检（viewer 只读 / developer 写面拒 admin 面 / owner
//     全权 / 机具令牌全库 / 平台管理员全库只读）；
//   - PAT 硬收缩（min(声明 scopes, 角色蕴含)——声明 admin 的 developer PAT
//     在角色门被拒）；
//   - 队内覆写双向（§3.3 B 形）+ 移出团队联动失效；
//   - 可见域解析（限定形恒可解析 / 裸名域内唯一 / E_APP_AMBIGUOUS 候选列）；
//   - 列表可见性过滤 + ?project= 收窄；
//   - SearchLogs 强制三段限定形选择器（非平台管理员）；
//   - WatchEvents app 主体按可见集过滤。
//
// 凭据形态：用户 PAT = state 层直接种子（scopes 显式——绕开创建面的防呆
// 校验正是本票语义的一部分：越权声明在角色门被硬性收缩）；机具令牌 =
// harness 的种子 token（user NULL）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// rbacFixture 是角色门测试夹具：两团队 × 多项目 × 同名 app + 全角色成员。
//
//	团队 acme：projA=default（app web）、projB=prod（app api）
//	团队 beta：projC=default（app web——与 acme 同名，裸名歧义源）
//
// 成员（全部经 CreateUser 建行——非首用户，无平台管理员标志）：
//	uOwner=acme owner、uDev=acme developer、uView=acme viewer、
//	uCross=acme developer + beta viewer、uB=beta developer；
//	uRoot = 库内首用户（CreateUser 首用户强制平台管理员）。
type rbacFixture struct {
	st    *state.Store
	box   *secrets.Box
	conn  *grpc.ClientConn
	teamA state.Team
	teamB state.Team
	projA state.Project // acme/default
	projB state.Project // acme/prod
	projC state.Project // beta/default
	uRoot *state.User
	uDev  *state.User
	uView *state.User
	uB    *state.User
}

// newRBACEnv 构造夹具 + 全资源面 bufconn（harness 鉴权链同构）。
func newRBACEnv(t *testing.T) *rbacFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), dir+"/test.db")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(dir + "/test.key")
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	f := &rbacFixture{st: st, box: box}

	mustUser := func(email string) *state.User {
		t.Helper()
		u, err := st.CreateUser(context.Background(), state.UserWrite{Email: email, Password: "pw-123456"})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", email, err)
		}
		return &u
	}
	f.uRoot = mustUser("root@example.com") // 首用户：平台管理员
	uOwner := mustUser("owner@example.com")
	f.uDev = mustUser("dev@example.com")
	f.uView = mustUser("view@example.com")
	uCross := mustUser("cross@example.com")
	f.uB = mustUser("b@example.com")

	f.teamA = mustTeam(t, st, "acme", "Acme", uOwner.ID)
	f.teamB = mustTeam(t, st, "beta", "Beta", f.uB.ID)
	mustMember(t, st, f.teamA.ID, uOwner.ID, state.TeamRoleOwner)
	mustMember(t, st, f.teamA.ID, f.uDev.ID, state.TeamRoleDeveloper)
	mustMember(t, st, f.teamA.ID, f.uView.ID, state.TeamRoleViewer)
	mustMember(t, st, f.teamA.ID, uCross.ID, state.TeamRoleDeveloper)
	mustMember(t, st, f.teamB.ID, uCross.ID, state.TeamRoleViewer)
	mustMember(t, st, f.teamB.ID, f.uB.ID, state.TeamRoleDeveloper)

	f.projA = mustProject(t, st, f.teamA.ID, "default")
	f.projB = mustProject(t, st, f.teamA.ID, "prod")
	f.projC = mustProject(t, st, f.teamB.ID, "default")
	mustApp(t, st, "web", f.projA)
	mustApp(t, st, "api", f.projB)
	mustApp(t, st, "web", f.projC)

	auth := NewAuthenticator(st)
	srv := newAuthServer(auth)
	serverv1.RegisterAppsServiceServer(srv, NewAppsService(st, box, nil))
	serverv1.RegisterDeploymentsServiceServer(srv, NewDeploymentsService(st))
	serverv1.RegisterEnvServiceServer(srv, NewEnvService(st, box))
	serverv1.RegisterLogsServiceServer(srv, NewLogsService(st, nil))
	serverv1.RegisterEventsServiceServer(srv, func() *EventsService {
		svc := NewEventsService(st)
		svc.interval = 20 * time.Millisecond
		return svc
	}())
	conn := serveBufconn(t, srv)
	f.conn = conn
	return f
}

// ── 夹具原语（直写 state——成员/角色/资源行的唯一播种通道）──────────────────

func mustUser(t *testing.T, st *state.Store, email string) *state.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), state.UserWrite{Email: email, Password: "pw-123456"})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	return &u
}

func mustTeam(t *testing.T, st *state.Store, slug, name, createdBy string) state.Team {
	t.Helper()
	team, err := st.CreateTeam(context.Background(), state.TeamWrite{Slug: slug, Name: name, CreatedBy: createdBy})
	if err != nil {
		t.Fatalf("CreateTeam %s: %v", slug, err)
	}
	return team
}

func mustMember(t *testing.T, st *state.Store, teamID, userID, role string) {
	t.Helper()
	if _, err := st.AddMember(context.Background(), teamID, userID, role, "", ""); err != nil {
		t.Fatalf("AddMember %s/%s: %v", teamID, userID, err)
	}
}

func mustProject(t *testing.T, st *state.Store, teamID, slug string) state.Project {
	t.Helper()
	proj, err := st.CreateProject(context.Background(), state.ProjectWrite{TeamID: teamID, Slug: slug, Name: slug})
	if err != nil {
		t.Fatalf("CreateProject %s: %v", slug, err)
	}
	return proj
}

func mustApp(t *testing.T, st *state.Store, name string, proj state.Project) state.App {
	t.Helper()
	app, err := st.CreateApp(context.Background(), "", name, proj.ID, proj.TeamID)
	if err != nil {
		t.Fatalf("CreateApp %s: %v", name, err)
	}
	return app
}

// seedUserPAT 直种一枚用户 PAT（绕开 CreateToken 防呆校验——越权声明在角色
// 门被硬性收缩正是本票的被测语义），返回明文。
func seedUserPAT(t *testing.T, st *state.Store, userID, scopes string) string {
	t.Helper()
	plaintext, err := generateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := st.CreateToken(context.Background(), state.TokenWrite{
		Hash:   state.HashToken(plaintext),
		Name:   "rbac " + scopes,
		Scopes: scopes,
		UserID: userID,
	}); err != nil {
		t.Fatalf("seed user token: %v", err)
	}
	return plaintext
}

// ── 矩阵抽检 ──────────────────────────────────────────────────────────────────

// TestRoleGateMatrix 角色→层级矩阵（§3.2）抽检 + PAT 硬收缩 + 机具全库 +
// 平台管理员只读。目标资源 = acme/default/web（限定形寻址——可见域与角色
// 门解耦，403 语义可稳定断言）。
func TestRoleGateMatrix(t *testing.T) {
	f := newRBACEnv(t)
	apps := serverv1.NewAppsServiceClient(f.conn)
	envs := serverv1.NewEnvServiceClient(f.conn)
	qualified := "acme/default/web"
	setEnv := func(token string) error {
		_, err := envs.SetEnv(authCtx(context.Background(), token), &serverv1.SetEnvRequest{
			App: qualified, Key: "K", Value: "v"})
		return err
	}

	t.Run("viewer_read_ok_write_denied", func(t *testing.T) {
		tok := seedUserPAT(t, f.st, f.uView.ID, ScopeRead)
		if _, err := apps.GetApp(authCtx(context.Background(), tok), &serverv1.GetAppRequest{Name: qualified}); err != nil {
			t.Fatalf("viewer GetApp: %v", err)
		}
		if err := setEnv(tok); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("viewer SetEnv err = %v, want PermissionDenied", err)
		}
	})
	t.Run("developer_write_ok_adminface_denied_hard_shrink", func(t *testing.T) {
		// 越权声明 admin 的 PAT（防呆面绕开）——角色门硬收缩到 developer 层
		//（min(声明 scopes, 角色蕴含)，W2-S2 注记的 S4 收口点）。
		tok := seedUserPAT(t, f.st, f.uDev.ID, ScopeAdmin)
		if err := setEnv(tok); err != nil {
			t.Fatalf("developer SetEnv: %v", err)
		}
		if _, err := envs.GetEnv(authCtx(context.Background(), tok), &serverv1.GetEnvRequest{App: qualified, Key: "K"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("developer GetEnv err = %v, want PermissionDenied (role gate hard shrink)", err)
		}
	})
	t.Run("owner_full", func(t *testing.T) {
		// 对照 PAT：developer 持越权声明的 admin scope——DeleteApp 在角色门
		// 被拒（owner 同 scope 可过，差只剩角色）。
		tokDevAdmin := seedUserPAT(t, f.st, f.uDev.ID, ScopeAdmin)
		uOwner := mustUser(t, f.st, "owner2@example.com")
		mustMember(t, f.st, f.teamA.ID, uOwner.ID, state.TeamRoleOwner)
		tokOwner := seedUserPAT(t, f.st, uOwner.ID, ScopeAdmin)
		mustApp(t, f.st, "tmp", f.projB)
		if _, err := apps.DeleteApp(authCtx(context.Background(), tokOwner), &serverv1.DeleteAppRequest{Name: "acme/prod/tmp"}); err != nil {
			t.Fatalf("owner DeleteApp: %v", err)
		}
		mustApp(t, f.st, "tmp2", f.projB)
		if _, err := apps.DeleteApp(authCtx(context.Background(), tokDevAdmin), &serverv1.DeleteAppRequest{Name: "acme/prod/tmp2"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("developer-with-admin-scope DeleteApp err = %v, want PermissionDenied", err)
		}
	})
	t.Run("platform_admin_read_only", func(t *testing.T) {
		tok := seedUserPAT(t, f.st, f.uRoot.ID, ScopeAdmin)
		if _, err := apps.GetApp(authCtx(context.Background(), tok), &serverv1.GetAppRequest{Name: qualified}); err != nil {
			t.Fatalf("platform admin GetApp: %v", err)
		}
		if err := setEnv(tok); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("platform admin SetEnv err = %v, want PermissionDenied (read-only support view)", err)
		}
	})
	t.Run("machine_token_full_library", func(t *testing.T) {
		tok := seedTokenPlain(t, f.st, ScopeAdmin)
		// 机具令牌跨团队写 beta（非 acme）资源 = 全库 admin 等价。
		if _, err := envs.SetEnv(authCtx(context.Background(), tok), &serverv1.SetEnvRequest{
			App: "beta/default/web", Key: "K", Value: "v"}); err != nil {
			t.Fatalf("machine SetEnv on beta app: %v", err)
		}
	})
}

// TestAccessLevelMapping scope→层级与角色→层级映射单点（terminal 对人类角色
// = developer+，§3.2 矩阵；未知值 fail-closed）。
func TestAccessLevelMapping(t *testing.T) {
	if got := accessLevelForScope(ScopeRead); got != levelRead {
		t.Fatalf("read scope → %v, want levelRead", got)
	}
	if got := accessLevelForScope(ScopeDeploy); got != levelDeploy {
		t.Fatalf("deploy scope → %v, want levelDeploy", got)
	}
	if got := accessLevelForScope(ScopeTerminal); got != levelDeploy {
		t.Fatalf("terminal scope → %v, want levelDeploy (terminal = developer+ for human roles)", got)
	}
	if got := accessLevelForScope("mystery"); got != levelAdmin {
		t.Fatalf("unknown scope → %v, want levelAdmin (fail-closed)", got)
	}
	if got := roleAccessLevel(state.TeamRoleOwner); got != levelAdmin {
		t.Fatalf("owner → %v, want levelAdmin", got)
	}
	if got := roleAccessLevel(state.TeamRoleViewer); got != levelRead {
		t.Fatalf("viewer → %v, want levelRead", got)
	}
	if got := roleAccessLevel(state.ProjectRoleDeveloper); got != levelDeploy {
		t.Fatalf("project developer override → %v, want levelDeploy", got)
	}
	if got := roleAccessLevel("nobody"); got != levelNone {
		t.Fatalf("unknown role → %v, want levelNone (fail-closed)", got)
	}
	if !levelAdmin.allows(levelRead) || levelDeploy.allows(levelAdmin) || levelNone.allows(levelRead) {
		t.Fatal("accessLevel ordering violated")
	}
}

// TestRoleGateProjectOverride 队内覆写双向（§3.3 B 形）+ 移出团队联动。
func TestRoleGateProjectOverride(t *testing.T) {
	f := newRBACEnv(t)
	envs := serverv1.NewEnvServiceClient(f.conn)
	set := func(token, app string) error {
		_, err := envs.SetEnv(authCtx(context.Background(), token), &serverv1.SetEnvRequest{App: app, Key: "K", Value: "v"})
		return err
	}
	// 覆写行：acme developer 在 default 降 viewer；acme viewer 在 default 升
	// developer（projB=prod 维持团队角色——双向生效的对照面）。
	if _, err := f.st.SetProjectMemberRole(context.Background(), f.projA.ID, f.uDev.ID, state.ProjectRoleViewer, "", ""); err != nil {
		t.Fatalf("override dev->viewer: %v", err)
	}
	if _, err := f.st.SetProjectMemberRole(context.Background(), f.projA.ID, f.uView.ID, state.ProjectRoleDeveloper, "", ""); err != nil {
		t.Fatalf("override view->developer: %v", err)
	}
	tokDev := seedUserPAT(t, f.st, f.uDev.ID, ScopeRead+","+ScopeDeploy)
	tokView := seedUserPAT(t, f.st, f.uView.ID, ScopeRead+","+ScopeDeploy)

	if err := set(tokDev, "acme/default/web"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("overridden(dev->viewer) SetEnv on default err = %v, want PermissionDenied", err)
	}
	if err := set(tokDev, "acme/prod/api"); err != nil {
		t.Fatalf("developer keeps deploy on prod: %v", err)
	}
	if err := set(tokView, "acme/default/web"); err != nil {
		t.Fatalf("overridden(viewer->developer) SetEnv on default: %v", err)
	}
	if err := set(tokView, "acme/prod/api"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer SetEnv on prod err = %v, want PermissionDenied", err)
	}

	// 移出团队：成员行与全部覆写行同事务清理（state.RemoveMember）→
	// 资源面对该用户全面关闭（限定形寻址 → 403）。
	uGone := mustUser(t, f.st, "gone@example.com")
	mustMember(t, f.st, f.teamA.ID, uGone.ID, state.TeamRoleDeveloper)
	if _, err := f.st.SetProjectMemberRole(context.Background(), f.projA.ID, uGone.ID, state.ProjectRoleAdmin, "", ""); err != nil {
		t.Fatalf("seed override for departing member: %v", err)
	}
	tokGone := seedUserPAT(t, f.st, uGone.ID, ScopeAdmin)
	if err := set(tokGone, "acme/default/web"); err != nil {
		t.Fatalf("member SetEnv before removal: %v", err)
	}
	if err := f.st.RemoveMember(context.Background(), f.teamA.ID, uGone.ID, "", ""); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if err := set(tokGone, "acme/default/web"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("removed member SetEnv err = %v, want PermissionDenied", err)
	}
	if _, err := f.st.GetProjectMembership(context.Background(), f.projA.ID, uGone.ID); err != state.ErrProjectMemberNotFound {
		t.Fatalf("override row after removal = %v, want purged", err)
	}
}

// TestResolveQualifiedAndBare 可见域解析：限定形恒可解析、裸名域内唯一、
// 多命中 E_APP_AMBIGUOUS（候选列限定形）、不可见资源 404 不泄漏、跨团队
// 限定形寻址越权 403。
func TestResolveQualifiedAndBare(t *testing.T) {
	f := newRBACEnv(t)
	apps := serverv1.NewAppsServiceClient(f.conn)
	get := func(token, ref string) (string, error) {
		resp, err := apps.GetApp(authCtx(context.Background(), token), &serverv1.GetAppRequest{Name: ref})
		if err != nil {
			return "", err
		}
		return resp.GetId(), nil
	}
	tokDev := seedUserPAT(t, f.st, f.uDev.ID, ScopeRead)
	tokB := seedUserPAT(t, f.st, f.uB.ID, ScopeRead)
	// uCross：acme developer + beta viewer——可见域含两个 "web"。
	uCrossID := mustUserIDByName(t, f.st, "cross@example.com")
	tokCross := seedUserPAT(t, f.st, uCrossID, ScopeRead)

	// 限定形恒可解析。
	if id, err := get(tokDev, "acme/default/web"); err != nil || id == "" {
		t.Fatalf("qualified resolve: err=%v id=%q", err, id)
	}
	// 裸名：解析域（acme 项目集）内唯一。
	if _, err := get(tokDev, "web"); err != nil {
		t.Fatalf("bare name unique in visible domain: %v", err)
	}
	// 裸名：跨团队可见域内双命中 → E_APP_AMBIGUOUS。
	_, err := get(tokCross, "web")
	if e, ok := apperr.FromError(err); !ok || e.Code() != "E_APP_AMBIGUOUS" {
		t.Fatalf("ambiguous bare name err = %v, want E_APP_AMBIGUOUS", err)
	}
	// 机具令牌：全库解析域，双命中同码。
	tokMachine := seedTokenPlain(t, f.st, ScopeRead)
	_, err = get(tokMachine, "web")
	if e, ok := apperr.FromError(err); !ok || e.Code() != "E_APP_AMBIGUOUS" {
		t.Fatalf("machine ambiguous bare name err = %v, want E_APP_AMBIGUOUS", err)
	}
	// 不可见资源：beta 成员看 acme 的 api（裸名在其可见域不存在）→ 404。
	if _, err := get(tokB, "api"); status.Code(err) != codes.NotFound {
		t.Fatalf("invisible bare name err = %v, want NotFound", err)
	}
	// 限定形寻址不可见团队资源：解析成功、角色门 403（明确拒绝）。
	if _, err := get(tokDev, "beta/default/web"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-team qualified resolve err = %v, want PermissionDenied", err)
	}
}

// mustUserIDByName 按 email 取用户 ID（歧义断言的夹具回读）。
func mustUserIDByName(t *testing.T, st *state.Store, email string) string {
	t.Helper()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, u := range users {
		if u.Email == email {
			return u.ID
		}
	}
	t.Fatalf("user %s not found", email)
	return ""
}

// TestListVisibilityAndProjectNarrow 列表可见性（用户=可见项目集；机具=
// 全库）+ ?project= 收窄（限定形/裸名）。
func TestListVisibilityAndProjectNarrow(t *testing.T) {
	f := newRBACEnv(t)
	apps := serverv1.NewAppsServiceClient(f.conn)
	list := func(token, project string) []string {
		resp, err := apps.ListApps(authCtx(context.Background(), token), &serverv1.ListAppsRequest{Project: project})
		if err != nil {
			t.Fatalf("ListApps: %v", err)
		}
		out := []string{}
		for _, a := range resp.GetApps() {
			out = append(out, a.GetName())
		}
		return out
	}
	tokView := seedUserPAT(t, f.st, f.uView.ID, ScopeRead)
	tokB := seedUserPAT(t, f.st, f.uB.ID, ScopeRead)
	tokMachine := seedTokenPlain(t, f.st, ScopeRead)

	if got := list(tokView, ""); len(got) != 2 {
		t.Fatalf("acme viewer ListApps = %v, want 2 apps (visible domain)", got)
	}
	if got := list(tokView, "acme/default"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("narrow acme/default = %v, want [web]", got)
	}
	if got := list(tokView, "default"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("narrow bare default = %v, want [web]", got)
	}
	if got := list(tokB, ""); len(got) != 1 || got[0] != "web" {
		t.Fatalf("beta member ListApps = %v, want [web]", got)
	}
	if got := list(tokMachine, ""); len(got) != 3 {
		t.Fatalf("machine ListApps = %v, want all 3", got)
	}
	if got := list(tokMachine, "beta/default"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("machine narrow beta/default = %v, want [web]", got)
	}
}

// TestSearchLogsSelectorConstraint SearchLogs 强制约束（§4.2）：非全局调用
// 方必须携带三段限定形选择器；无选择器/裸名 → 400 带指引；不可见限定形 →
// 403；通过约束后进入后端面（本装配 vl 未接 → E_LOGS_BACKEND_UNAVAILABLE
// 证明约束已过）。机具令牌裸名不受约束。
func TestSearchLogsSelectorConstraint(t *testing.T) {
	f := newRBACEnv(t)
	logs := serverv1.NewLogsServiceClient(f.conn)
	search := func(token, app string) error {
		_, err := logs.SearchLogs(authCtx(context.Background(), token), &serverv1.SearchLogsRequest{App: app})
		return err
	}
	tokDev := seedUserPAT(t, f.st, f.uDev.ID, ScopeRead)
	tokMachine := seedTokenPlain(t, f.st, ScopeRead)

	if err := search(tokDev, "web"); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "team/prj/app") {
		t.Fatalf("bare selector err = %v, want InvalidArgument with guidance", err)
	}
	err := search(tokDev, "acme/default/web")
	if e, ok := apperr.FromError(err); !ok || e.Code() != "E_LOGS_BACKEND_UNAVAILABLE" {
		t.Fatalf("qualified selector err = %v, want to pass constraint and reach backend face", err)
	}
	if err := search(tokDev, "beta/default/web"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("invisible qualified selector err = %v, want PermissionDenied", err)
	}
	// 机具令牌裸名不受限定形约束（唯一命中即下发；歧义仍由解析拒绝）。
	err = search(tokMachine, "api")
	if e, ok := apperr.FromError(err); !ok || e.Code() != "E_LOGS_BACKEND_UNAVAILABLE" {
		t.Fatalf("machine bare selector err = %v, want unconstrained backend face", err)
	}
}

// TestWatchEventsVisibilityFilter WatchEvents：app 主体事件按可见项目集服
// 务端过滤；非 app 主体（deployment:*）恒可见；机具令牌不过滤。
func TestWatchEventsVisibilityFilter(t *testing.T) {
	f := newRBACEnv(t)
	seedEvent := func(name, subject string) {
		if err := f.st.InTx(context.Background(), func(tx *state.Tx) error {
			_, err := tx.AppendEvent(context.Background(), state.Event{Name: name, Subject: subject, Payload: "{}"})
			return err
		}); err != nil {
			t.Fatalf("append event %s: %v", subject, err)
		}
	}
	seedEvent("deployment.queued", "app:api")   // acme（projB）——uView 可见
	seedEvent("deployment.queued", "app:solo")  // beta——uView 不可见
	seedEvent("deployment.queued", "deployment:z") // 非 app 主体——恒可见

	collect := func(token string) map[string]bool {
		stream, err := serverv1.NewEventsServiceClient(f.conn).WatchEvents(authCtx(context.Background(), token), &serverv1.WatchEventsRequest{})
		if err != nil {
			t.Fatalf("WatchEvents: %v", err)
		}
		// Recv 阻塞面与 deadline 分离（goroutine + channel——事件穷尽后
		// Recv 挂起，deadline 到点即收口）。
		subjects := make(chan string, 64)
		go func() {
			for {
				frame, err := stream.Recv()
				if err != nil {
					close(subjects)
					return
				}
				if ev := frame.GetEvent(); ev != nil {
					subjects <- ev.GetSubject()
				}
			}
		}()
		seen := map[string]bool{}
		deadline := time.After(600 * time.Millisecond)
		for {
			select {
			case <-deadline:
				_ = stream.CloseSend()
				return seen
			case s, ok := <-subjects:
				if !ok {
					return seen
				}
				seen[s] = true
			}
		}
	}
	tokView := seedUserPAT(t, f.st, f.uView.ID, ScopeRead)
	seen := collect(tokView)
	if !seen["app:api"] || !seen["deployment:z"] {
		t.Fatalf("viewer stream = %v, want app:api and deployment:z visible", seen)
	}
	if seen["app:solo"] {
		t.Fatalf("viewer stream saw invisible app-subject event app:solo: %v", seen)
	}
	tokMachine := seedTokenPlain(t, f.st, ScopeRead)
	seen = collect(tokMachine)
	if !seen["app:api"] || !seen["app:solo"] {
		t.Fatalf("machine stream = %v, want all app-subject events (unfiltered)", seen)
	}
}

// TestDeployRoleGate 部署受理的角色门：先门后建行——无权限调用方的部署不
// 入队、不建 app 行；developer 首署成功（teamA/prod 项目）。
func TestDeployRoleGate(t *testing.T) {
	f := newRBACEnv(t)
	deploys := serverv1.NewDeploymentsServiceClient(f.conn)
	composeBody := []byte("name: fresh\nservices:\n  web:\n    image: alpine:3\n    command: [\"sleep\", \"infinity\"]\n    healthcheck:\n      test: [\"CMD\", \"true\"]\n      interval: 1s\n      timeout: 1s\n      retries: 2\n      start_period: 1s\n")
	deploy := func(token, project string) error {
		_, err := deploys.Deploy(authCtx(context.Background(), token), &serverv1.DeployRequest{
			App: "fresh", Compose: composeBody, Project: project})
		return err
	}
	tokView := seedUserPAT(t, f.st, f.uView.ID, ScopeRead)
	if err := deploy(tokView, "acme/prod"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer deploy err = %v, want PermissionDenied", err)
	}
	if _, err := f.st.GetAppByNameInProject(context.Background(), f.projB.ID, "fresh"); err != state.ErrAppNotFound {
		t.Fatalf("denied deploy must not create the app row: %v", err)
	}
	tokRoot := seedUserPAT(t, f.st, f.uRoot.ID, ScopeAdmin)
	if err := deploy(tokRoot, "acme/prod"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("platform admin deploy err = %v, want PermissionDenied (read-only)", err)
	}
	tokDev := seedUserPAT(t, f.st, f.uDev.ID, ScopeRead+","+ScopeDeploy)
	if err := deploy(tokDev, "acme/prod"); err != nil {
		t.Fatalf("developer deploy: %v", err)
	}
	if _, err := f.st.GetAppByNameInProject(context.Background(), f.projB.ID, "fresh"); err != nil {
		t.Fatalf("developer deploy must create the app row: %v", err)
	}
}

// TestDeploySameNameSecondProject（staging SV-11b 真机抓出，2026-09-24）：
// 同名 app 在目标项目不存在 → **新建**（D-W0-4 二修：project 内唯一 = 跨项目
// 同名是两个不同应用）；目标项目已有 → 沿用且归属一致。S3 实现曾取
// 「全局按名解析 → 409 E_APP_PROJECT_MISMATCH 指引 MoveApp」的偏差裁决，
// 与设计 §3.4/§8「同团队跨项目同名 app 亦成功」矛盾，本测试钉死对齐后语义。
func TestDeploySameNameSecondProject(t *testing.T) {
	f := newRBACEnv(t)
	deploys := serverv1.NewDeploymentsServiceClient(f.conn)
	composeBody := []byte("name: dup\nservices:\n  web:\n    image: alpine:3\n    command: [\"sleep\", \"infinity\"]\n    healthcheck:\n      test: [\"CMD\", \"true\"]\n      interval: 1s\n      timeout: 1s\n      retries: 2\n      start_period: 1s\n")
	deploy := func(token, project string) error {
		_, err := deploys.Deploy(authCtx(context.Background(), token), &serverv1.DeployRequest{
			App: "dup", Compose: composeBody, Project: project})
		return err
	}
	tokDev := seedUserPAT(t, f.st, f.uDev.ID, ScopeRead+","+ScopeDeploy)
	if err := deploy(tokDev, "acme/prod"); err != nil {
		t.Fatalf("first deploy (acme/prod): %v", err)
	}
	// 同名第二署（跨团队跨项目）：必须新建行而非 409。
	tokMachine := seedTokenPlain(t, f.st, ScopeAdmin)
	if err := deploy(tokMachine, "beta/default"); err != nil {
		t.Fatalf("same-name deploy into a second project must create a new app: %v", err)
	}
	if _, err := f.st.GetAppByNameInProject(context.Background(), f.projC.ID, "dup"); err != nil {
		t.Fatalf("second-project row missing: %v", err)
	}
	// 原行归属不动。
	row, err := f.st.GetAppByNameInProject(context.Background(), f.projB.ID, "dup")
	if err != nil || row.ProjectID != f.projB.ID {
		t.Fatalf("original row ownership changed: row=%+v err=%v", row, err)
	}
}
