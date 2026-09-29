package api

// 项目网参与面 API 测试（T 线 OT-1 / IMPL-T15-1）：attach/detach 全链
//（状态 + 审计 + 事件 + 网络 ensure + 重部署入队）、幂等重跑、scope/角色
// 双门（admin scope + 项目角色 admin；平台管理员只读不代写）、在途部署
// 409 守卫、端口未装配如实报不可用、ProjectView/AppView 网络投影。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// fakeProjectNetworkPort 记录项目网编排调用（ensure 前置与重部署入队面）。
type fakeProjectNetworkPort struct {
	ensured      []string
	enqueueCalls []string
	enqueueID    string
	enqueueErr   error
	ensureErr    error
}

func (f *fakeProjectNetworkPort) EnsureProjectNetwork(_ context.Context, projectID string) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured = append(f.ensured, projectID)
	return nil
}

func (f *fakeProjectNetworkPort) EnqueueNetworkRedeploy(_ context.Context, appID string) (string, error) {
	f.enqueueCalls = append(f.enqueueCalls, appID)
	if f.enqueueErr != nil {
		return "", f.enqueueErr
	}
	return f.enqueueID, nil
}

// projectNetworkEnv 是项目网 API 测试环境（ProjectsService 装配记录型假
// 端口 + AppsService 读面投影）。
type projectNetworkEnv struct {
	st   *state.Store
	port *fakeProjectNetworkPort
	proj serverv1.ProjectsServiceClient
	apps serverv1.AppsServiceClient
	team state.Team
}

func newProjectNetworkEnv(t *testing.T) *projectNetworkEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "fleetly.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	port := &fakeProjectNetworkPort{enqueueID: "01JABCDEDEPLOY000000000000"}
	auth := NewAuthenticator(st)
	srv := newAuthServer(auth)
	serverv1.RegisterProjectsServiceServer(srv, NewProjectsService(st).WithNetworkPort(port))
	serverv1.RegisterAppsServiceServer(srv, NewAppsService(st, box, nil))
	conn := serveBufconn(t, srv)
	return &projectNetworkEnv{
		st:   st,
		port: port,
		proj: serverv1.NewProjectsServiceClient(conn),
		apps: serverv1.NewAppsServiceClient(conn),
	}
}

// seedProjectNetworkApp 播种团队 + 项目 + app（机器令牌面测试用）。
func seedProjectNetworkApp(t *testing.T, env *projectNetworkEnv, appName string) state.App {
	t.Helper()
	team, err := env.st.CreateTeam(context.Background(), state.TeamWrite{Slug: "acmenet", Name: "Acme", CreatedBy: "fixture"})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	env.team = team
	proj, err := env.st.CreateProject(context.Background(), state.ProjectWrite{TeamID: team.ID, Slug: "default", Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app, err := env.st.CreateApp(context.Background(), "", appName, proj.ID, team.ID)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// TestProjectNetworkAttachDetachFlow 主链：attach → 网络 ensure 前置 + 状态
// 落位 + 审计/事件 + 重部署入队；GetApp 投影随行；幂等重跑零重复披露；
// detach 反向对称。
func TestProjectNetworkAttachDetachFlow(t *testing.T) {
	env := newProjectNetworkEnv(t)
	app := seedProjectNetworkApp(t, env, "web")
	ctx := authCtx(context.Background(), seedTokenPlain(t, env.st, "admin"))
	wantNet, err := naming.ProjectNetworkName(app.ProjectID)
	if err != nil {
		t.Fatalf("project network name: %v", err)
	}

	resp, err := env.proj.AttachAppProjectNetwork(ctx, &serverv1.AttachAppProjectNetworkRequest{App: "web"})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	m := resp.GetMembership()
	if m.GetApp() != "web" || !m.GetAttached() || !m.GetChanged() {
		t.Fatalf("attach membership = %+v, want web/attached/changed", m)
	}
	if m.GetNetwork() != wantNet || m.GetProjectId() != app.ProjectID || m.GetProject() != "acmenet/default" {
		t.Fatalf("attach projection = %+v, want network %s project acmenet/default", m, wantNet)
	}
	if m.GetStatus() != "rolling" || m.GetDeploymentId() != env.port.enqueueID {
		t.Fatalf("attach status = %q deployment=%q, want rolling + enqueued id", m.GetStatus(), m.GetDeploymentId())
	}
	if len(env.port.ensured) != 1 || env.port.ensured[0] != app.ProjectID {
		t.Fatalf("ensure calls = %v, want the project id once (attach 前置)", env.port.ensured)
	}
	if len(env.port.enqueueCalls) != 1 || env.port.enqueueCalls[0] != app.ID {
		t.Fatalf("enqueue calls = %v, want the app id once", env.port.enqueueCalls)
	}
	// 状态 + 审计 + 事件。
	row, err := env.st.GetAppByID(context.Background(), app.ID)
	if err != nil || !row.ProjectNetworkAttached {
		t.Fatalf("stored flag = %v err=%v, want attached", row.ProjectNetworkAttached, err)
	}
	audits, err := env.st.RecentAudits(context.Background(), 50)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	attachAudits := 0
	for _, a := range audits {
		if a.Action == "app.project_network_attached" {
			attachAudits++
			if !strings.Contains(a.DiffSummary, app.ProjectID) {
				t.Fatalf("attach audit without project id: %s", a.DiffSummary)
			}
		}
	}
	if attachAudits != 1 {
		t.Fatalf("attach audits = %d, want 1", attachAudits)
	}
	events, err := env.st.EventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	networkEvents := 0
	for _, e := range events {
		if e.Name == "project.network_changed" && strings.Contains(e.Payload, `"attached"`) {
			networkEvents++
		}
	}
	if networkEvents != 1 {
		t.Fatalf("project.network_changed attached events = %d, want 1", networkEvents)
	}

	// GetApp 投影随行（Console 数据源）。
	got, err := env.apps.GetApp(ctx, &serverv1.GetAppRequest{Name: "web"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if !got.GetProjectNetworkAttached() || got.GetProjectNetwork() != wantNet || got.GetProjectId() != app.ProjectID {
		t.Fatalf("GetApp projection = %+v, want attached + %s", got, wantNet)
	}

	// 幂等重跑：changed=false、零重复审计/事件；重部署仍重试入队（重跑收尾
	// 语义——半程失败的重试路径）。
	resp2, err := env.proj.AttachAppProjectNetwork(ctx, &serverv1.AttachAppProjectNetworkRequest{App: "web"})
	if err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if resp2.GetMembership().GetChanged() {
		t.Fatal("re-attach reported changed=true (idempotence broken)")
	}
	if len(env.port.enqueueCalls) != 2 {
		t.Fatalf("re-attach enqueue calls = %d, want 2 (retry-safe redeploy)", len(env.port.enqueueCalls))
	}
	audits, _ = env.st.RecentAudits(context.Background(), 50)
	attachAudits = 0
	for _, a := range audits {
		if a.Action == "app.project_network_attached" {
			attachAudits++
		}
	}
	if attachAudits != 1 {
		t.Fatalf("re-attach duplicated the audit (count=%d)", attachAudits)
	}

	// detach：反向对称（不 ensure；审计 app.project_network_detached）。
	ensuresBeforeDetach := len(env.port.ensured)
	det, err := env.proj.DetachAppProjectNetwork(ctx, &serverv1.DetachAppProjectNetworkRequest{App: "web"})
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if det.GetMembership().GetAttached() || !det.GetMembership().GetChanged() || det.GetMembership().GetStatus() != "rolling" {
		t.Fatalf("detach membership = %+v, want detached/changed/rolling", det.GetMembership())
	}
	if len(env.port.ensured) != ensuresBeforeDetach {
		t.Fatalf("detach must not ensure the network (ensures grew: %v)", env.port.ensured)
	}
	row, _ = env.st.GetAppByID(context.Background(), app.ID)
	if row.ProjectNetworkAttached {
		t.Fatal("detach did not clear the flag")
	}
	if got, _ := env.apps.GetApp(ctx, &serverv1.GetAppRequest{Name: "web"}); got.GetProjectNetworkAttached() || got.GetProjectNetwork() != "" {
		t.Fatalf("GetApp after detach = %+v, want detached with empty network", got)
	}
}

// TestProjectNetworkNoRedeployHistoryIsAccepted 无成功部署史：状态落位 +
// attach 应答落 attached/detached（无底座对象需重投影）。
func TestProjectNetworkNoRedeployHistoryIsAccepted(t *testing.T) {
	env := newProjectNetworkEnv(t)
	env.port.enqueueErr = engine.ErrNoRedeploySource
	seedProjectNetworkApp(t, env, "web")
	ctx := authCtx(context.Background(), seedTokenPlain(t, env.st, "admin"))

	resp, err := env.proj.AttachAppProjectNetwork(ctx, &serverv1.AttachAppProjectNetworkRequest{App: "web"})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if resp.GetMembership().GetStatus() != "attached" || resp.GetMembership().GetDeploymentId() != "" {
		t.Fatalf("membership = %+v, want attached with no deployment id", resp.GetMembership())
	}
}

// TestProjectNetworkAttachInFlightGuard 在途部署 409 拒绝：重部署以最近
// 成功部署为基，不得覆盖在途发布；状态零变更、端口零调用。
func TestProjectNetworkAttachInFlightGuard(t *testing.T) {
	env := newProjectNetworkEnv(t)
	app := seedProjectNetworkApp(t, env, "web")
	ctx := authCtx(context.Background(), seedTokenPlain(t, env.st, "admin"))

	rec, err := env.st.CreateDeployment(context.Background(), state.DeployRecord{
		AppID: app.ID, AppName: app.Name, Kind: "deploy", SpecHash: "h", ComposePath: "/tmp/c.yaml",
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	_ = rec
	_, err = env.proj.AttachAppProjectNetwork(ctx, &serverv1.AttachAppProjectNetworkRequest{App: "web"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("in-flight attach code = %v, want FailedPrecondition (409)", status.Code(err))
	}
	if !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("in-flight rejection must be explicit: %v", err)
	}
	if len(env.port.ensured) != 0 || len(env.port.enqueueCalls) != 0 {
		t.Fatalf("in-flight rejection touched the port (ensured=%v enqueue=%v)", env.port.ensured, env.port.enqueueCalls)
	}
	row, _ := env.st.GetAppByID(context.Background(), app.ID)
	if row.ProjectNetworkAttached {
		t.Fatal("in-flight rejection must leave the flag untouched")
	}
}

// TestProjectNetworkScopeAndRoleGates 双门：scope 登记 admin（登记面）+ 真
// 拦截链（read/deploy 凭据拒、机具令牌放行）；角色门——平台管理员只读
// （403）、团队 developer 拒、团队 owner 放行。
func TestProjectNetworkScopeAndRoleGates(t *testing.T) {
	for _, method := range []string{"AttachAppProjectNetwork", "DetachAppProjectNetwork"} {
		got, ok := RequiredScope("/fleetly.server.v1.ProjectsService/" + method)
		if !ok || got != ScopeAdmin {
			t.Fatalf("%s scope = %q ok=%v, want admin", method, got, ok)
		}
	}

	env := newProjectNetworkEnv(t)
	app := seedProjectNetworkApp(t, env, "web")
	// 平台管理员 = 库内首用户（先建，保证 IsPlatformAdmin）；再建团队里的
	// developer 与 owner（角色门区分面）。
	admin, err := env.st.CreateUser(context.Background(), state.UserWrite{Email: "root@example.com", Password: "pw-123456"})
	if err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}
	if !admin.IsPlatformAdmin {
		t.Fatal("first user must be a platform admin (fixture assumption)")
	}
	dev, err := env.st.CreateUser(context.Background(), state.UserWrite{Email: "dev@example.com", Password: "pw-123456"})
	if err != nil {
		t.Fatalf("CreateUser dev: %v", err)
	}
	owner, err := env.st.CreateUser(context.Background(), state.UserWrite{Email: "owner@example.com", Password: "pw-123456"})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	if _, err := env.st.AddMember(context.Background(), env.team.ID, dev.ID, state.TeamRoleDeveloper, "", ""); err != nil {
		t.Fatalf("AddMember dev: %v", err)
	}
	if _, err := env.st.AddMember(context.Background(), env.team.ID, owner.ID, state.TeamRoleOwner, "", ""); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}

	// read 凭据：scope 门即拒（登记的 admin scope 形状门）。
	readCtx := authCtx(context.Background(), seedTokenPlain(t, env.st, "read"))
	if _, err := env.proj.AttachAppProjectNetwork(readCtx, &serverv1.AttachAppProjectNetworkRequest{App: "web"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token attach code = %v, want PermissionDenied", status.Code(err))
	}
	// 团队 developer（越权声明 admin 的 PAT——角色门硬收缩到 deploy）。
	devCtx := authCtx(context.Background(), seedUserPAT(t, env.st, dev.ID, ScopeAdmin))
	if _, err := env.proj.AttachAppProjectNetwork(devCtx, &serverv1.AttachAppProjectNetworkRequest{App: "web"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("developer attach code = %v, want PermissionDenied (admin-level network posture change)", status.Code(err))
	}
	// 平台管理员：只读不代写（403）。
	adminCtx := authCtx(context.Background(), seedUserPAT(t, env.st, admin.ID, ScopeAdmin))
	if _, err := env.proj.AttachAppProjectNetwork(adminCtx, &serverv1.AttachAppProjectNetworkRequest{App: "web"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("platform admin attach code = %v, want PermissionDenied (separation of duties)", status.Code(err))
	}
	// 团队 owner：放行。
	ownerCtx := authCtx(context.Background(), seedUserPAT(t, env.st, owner.ID, ScopeAdmin))
	if _, err := env.proj.AttachAppProjectNetwork(ownerCtx, &serverv1.AttachAppProjectNetworkRequest{App: "web"}); err != nil {
		t.Fatalf("team owner attach: %v", err)
	}
	row, _ := env.st.GetAppByID(context.Background(), app.ID)
	if !row.ProjectNetworkAttached {
		t.Fatal("team owner attach did not take effect")
	}
}

// TestProjectNetworkPortNotAssembledAndNotFound 端口未装配如实报不可用
// （不静默退化）；app 不存在 404。
func TestProjectNetworkPortNotAssembledAndNotFound(t *testing.T) {
	env := newProjectNetworkEnv(t)
	seedProjectNetworkApp(t, env, "web")
	ctx := authCtx(context.Background(), seedTokenPlain(t, env.st, "admin"))

	if _, err := env.proj.AttachAppProjectNetwork(ctx, &serverv1.AttachAppProjectNetworkRequest{App: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing app code = %v, want NotFound", status.Code(err))
	}

	// 未装配端口的形态：独立注册一个无端口服务。
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "bare.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	auth := NewAuthenticator(st)
	srv := newAuthServer(auth)
	serverv1.RegisterProjectsServiceServer(srv, NewProjectsService(st))
	conn := serveBufconn(t, srv)
	token := seedTokenPlain(t, st, "admin")
	team, err := st.CreateTeam(context.Background(), state.TeamWrite{Slug: "bareteam", Name: "Bare", CreatedBy: "fixture"})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	proj, err := st.CreateProject(context.Background(), state.ProjectWrite{TeamID: team.ID, Slug: "default", Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateApp(context.Background(), "", "web", proj.ID, team.ID); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	bare := serverv1.NewProjectsServiceClient(conn)
	if _, err := bare.AttachAppProjectNetwork(authCtx(context.Background(), token), &serverv1.AttachAppProjectNetworkRequest{App: "web"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unassembled port code = %v, want Unavailable", status.Code(err))
	}
}

// TestProjectNetworkProjectViewProjection ProjectView 网络面投影：network_name
// 公式现推；network_members 随参与位计数（列表批量查询同一数据源）。项目
// 读面要求用户凭据（requireTeamUser——机具令牌无团队身份，设计 §2.3），
// 故以团队 owner 的 read 凭据读面、admin 凭据做 attach。
func TestProjectNetworkProjectViewProjection(t *testing.T) {
	env := newProjectNetworkEnv(t)
	app := seedProjectNetworkApp(t, env, "web")
	wantNet, _ := naming.ProjectNetworkName(app.ProjectID)
	// 首位用户 = 平台管理员（只读不代写）；owner 用户承担本测试的读写。
	if _, err := env.st.CreateUser(context.Background(), state.UserWrite{Email: "root@example.com", Password: "pw-123456"}); err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}
	owner, err := env.st.CreateUser(context.Background(), state.UserWrite{Email: "owner@example.com", Password: "pw-123456"})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	if _, err := env.st.AddMember(context.Background(), env.team.ID, owner.ID, state.TeamRoleOwner, "", ""); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}
	readCtx := authCtx(context.Background(), seedUserPAT(t, env.st, owner.ID, ScopeRead))
	adminCtx := authCtx(context.Background(), seedUserPAT(t, env.st, owner.ID, ScopeAdmin))

	got, err := env.proj.GetProject(readCtx, &serverv1.GetProjectRequest{Id: app.ProjectID})
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.GetProject().GetNetworkName() != wantNet || got.GetProject().GetNetworkMembers() != 0 {
		t.Fatalf("project view = %+v, want network %s members 0", got.GetProject(), wantNet)
	}
	if _, err := env.proj.AttachAppProjectNetwork(adminCtx, &serverv1.AttachAppProjectNetworkRequest{App: "web"}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	list, err := env.proj.ListProjects(readCtx, &serverv1.ListProjectsRequest{})
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(list.GetProjects()) != 1 || list.GetProjects()[0].GetNetworkMembers() != 1 {
		t.Fatalf("list projects = %+v, want one project with 1 network member", list.GetProjects())
	}
}
