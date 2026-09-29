package api

// 应用挂起/恢复 RPC 测试（app Stop/Start，2026-09-29 Console dokploy 对齐
// 三轮）：位翻转 + 派生直投影 + 哨兵冲突映射 + resume 无目标吞尾。排水/恢
// 复执行腿与入队门的引擎侧语义在 internal/engine/suspend_test.go。

import (
	"context"
	"path/filepath"
	"testing"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// suspendEnv 是挂起面测试环境（AppsService 单服务装配；无 ingress/无 git）。
type suspendEnv struct {
	st   *state.Store
	apps serverv1.AppsServiceClient
}

func newSuspendEnv(t *testing.T) *suspendEnv {
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
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterAppsServiceServer(srv, NewAppsService(st, box, nil))
	conn := serveBufconn(t, srv)
	return &suspendEnv{st: st, apps: serverv1.NewAppsServiceClient(conn)}
}

func TestAppSuspendResumeRPCs(t *testing.T) {
	env := newSuspendEnv(t)
	team, err := env.st.CreateTeam(context.Background(), state.TeamWrite{Slug: "acmenet", Name: "Acme", CreatedBy: "fixture"})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	proj, err := env.st.CreateProject(context.Background(), state.ProjectWrite{TeamID: team.ID, Slug: "default", Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := env.st.CreateApp(context.Background(), "", "web", proj.ID, team.ID); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	ctx := authCtx(context.Background(), seedTokenPlain(t, env.st, "admin"))

	// suspend：位翻转 + 派生直投影 suspended（无部署窗口也成立——第一判短路，
	// 不是对底座的观察结论）。
	resp, err := env.apps.SuspendApp(ctx, &serverv1.SuspendAppRequest{Name: "web"})
	if err != nil {
		t.Fatalf("SuspendApp: %v", err)
	}
	if !resp.GetApp().GetSuspended() || resp.GetApp().GetDerivedState() != "suspended" {
		t.Fatalf("suspended = %t derived = %s, want true/suspended", resp.GetApp().GetSuspended(), resp.GetApp().GetDerivedState())
	}
	// GetApp 读面投影随行。
	got, err := env.apps.GetApp(ctx, &serverv1.GetAppRequest{Name: "web"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if !got.GetSuspended() || got.GetDerivedState() != "suspended" {
		t.Fatalf("GetApp suspended = %t derived = %s, want true/suspended", got.GetSuspended(), got.GetDerivedState())
	}

	// 重复 suspend：CAS 冲突 → 409 E_APP_SUSPEND_CONFLICT（幂等面以读面
	// 投影消化，写面显式拒绝——不静默成功）。
	_, suspErr := env.apps.SuspendApp(ctx, &serverv1.SuspendAppRequest{Name: "web"})
	envCode(t, suspErr, "E_APP_SUSPEND_CONFLICT")

	// resume（无成功部署：保留窗空 → E_ROLLBACK_NO_TARGET 如实吞尾）：
	// 位清零照常生效，deployment_id 留空。
	rresp, err := env.apps.ResumeApp(ctx, &serverv1.ResumeAppRequest{Name: "web"})
	if err != nil {
		t.Fatalf("ResumeApp: %v", err)
	}
	if rresp.GetApp().GetSuspended() {
		t.Fatal("resume left the app suspended")
	}
	if rresp.GetDeploymentId() != "" {
		t.Fatalf("deployment_id = %q, want empty (no revisions to replay)", rresp.GetDeploymentId())
	}
	// 派生回观察态（无部署窗口 → down——不再是 suspended 直投影）。
	got2, err := env.apps.GetApp(ctx, &serverv1.GetAppRequest{Name: "web"})
	if err != nil {
		t.Fatalf("GetApp after resume: %v", err)
	}
	if got2.GetSuspended() || got2.GetDerivedState() == "suspended" {
		t.Fatalf("after resume: suspended = %t derived = %s, want false/not-suspended", got2.GetSuspended(), got2.GetDerivedState())
	}
}
