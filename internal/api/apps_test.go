package api

// app 删除管线测试（H9 路由撤销通道接线）：DeleteApp 的 deleting 第一拍
// 后同步撤销该 app 全部路由。撤销的完整链路（台账清理 + 空视图落 noop
// 兜底）由 internal/ingress 测试覆盖；这里钉死 api 层契约——撤销以
// appID 精确触发、失败不回滚 lifecycle 且落失败审计披露。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// fakeWithdrawer 是 RouteWithdrawer 的记录型假实现（台账面与
// ingress.Manager.WithdrawAppRoutes 契约同构；err 注入失败路径）。
type fakeWithdrawer struct {
	st    *state.Store
	calls []string
	err   error
}

func (f *fakeWithdrawer) WithdrawAppRoutes(ctx context.Context, appID string) error {
	f.calls = append(f.calls, appID)
	if f.err != nil {
		return f.err
	}
	return f.st.DeleteAppDomains(ctx, appID)
}

// newAppsTestEnv 构造最小服务装配（真实 store + box；不经 gRPC——鉴权
// 矩阵已覆盖 DeleteApp 的 scope 面）。
func newAppsTestEnv(t *testing.T, withdraw RouteWithdrawer) (*AppsService, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	return NewAppsService(st, box, withdraw), st
}

// seedAppWithDomains 落一个带域名台账行的 app（发布路径的等价预置）。
func seedAppWithDomains(t *testing.T, st *state.Store, name string) state.App {
	t.Helper()
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, name)
	if err != nil {
		t.Fatalf("create app %s: %v", name, err)
	}
	if _, err := st.CreateAppDomain(ctx, app.ID, state.DomainInput{
		Domain: name + ".example.test", Service: "web", Port: "80", Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("seed domains for %s: %v", name, err)
	}
	return app
}

// TestDeleteAppWithdrawsRoutes 删除管线接通（H9）：DeleteApp 成功 →
// 撤销以 appID 精确触发一次 + 域名台账被清。
func TestDeleteAppWithdrawsRoutes(t *testing.T) {
	fake := &fakeWithdrawer{}
	svc, store := newAppsTestEnv(t, fake)
	fake.st = store
	ctx := context.Background()
	app := seedAppWithDomains(t, store, "gone")

	if _, err := svc.DeleteApp(directCtx(ctx), &serverv1.DeleteAppRequest{Name: "gone"}); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != app.ID {
		t.Fatalf("withdraw calls = %v, want exactly [%s]", fake.calls, app.ID)
	}
	rows, err := store.ListAppDomains(ctx, app.ID)
	if err != nil {
		t.Fatalf("ListAppDomains: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("domain ledger must be cleared on delete: %+v", rows)
	}
	got, err := store.GetAppByName(ctx, "gone")
	if err != nil || got.Lifecycle != state.LifecycleDeleting {
		t.Fatalf("lifecycle after delete = %v/%s, want deleting", err, got.Lifecycle)
	}
}

// TestDeleteAppWithdrawFailureAlarms 撤销失败语义：不回滚 lifecycle
// （deleting 已提交、DeleteApp 不可重入）+ 错误返回 + 失败审计行披露。
func TestDeleteAppWithdrawFailureAlarms(t *testing.T) {
	fake := &fakeWithdrawer{err: errors.New("substrate unreachable")}
	svc, store := newAppsTestEnv(t, fake)
	ctx := context.Background()
	app := seedAppWithDomains(t, store, "stuck")

	_, err := svc.DeleteApp(directCtx(ctx), &serverv1.DeleteAppRequest{Name: "stuck"})
	if err == nil {
		t.Fatal("DeleteApp must surface withdrawal failure")
	}
	got, gerr := store.GetAppByName(ctx, "stuck")
	if gerr != nil || got.Lifecycle != state.LifecycleDeleting {
		t.Fatalf("lifecycle must remain deleting (no rollback): %v/%s", gerr, got.Lifecycle)
	}
	// 失败审计在（result=error 的 DeleteApp 行）。
	audits, aerr := store.RecentAudits(ctx, 10)
	if aerr != nil {
		t.Fatalf("RecentAudits: %v", aerr)
	}
	found := false
	for _, a := range audits {
		if a.Result == "error" && a.Target == "app:"+app.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("failure audit row missing: %+v", audits)
	}
}

// TestSetAppSourceValidation E7④⑤（S19）：SetAppSource 参数校验——
// auth_secret 非 none 配对时最小长度 16（与 webhook secret 同标）；
// https_token 认证强制 https:// 源（http:// 明文链路泄露 token）。
func TestSetAppSourceValidation(t *testing.T) {
	svc, st := newAppsTestEnv(t, nil)
	ctx := context.Background()
	if _, err := testsupport.SeedAppE(t, st, "srcapp"); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	// E7④：https_token 配对时 auth_secret ≥16。
	_, err := svc.SetAppSource(directCtx(ctx), &serverv1.SetAppSourceRequest{
		Name: "srcapp", SourceUrl: "https://example.com/acme/web.git",
		SourceAuthKind: "https_token", SourceAuthSecret: "short",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("short auth_secret err = %v, want InvalidArgument", err)
	}
	// E7⑤：https_token 拒绝 http:// 明文源。
	_, err = svc.SetAppSource(directCtx(ctx), &serverv1.SetAppSourceRequest{
		Name: "srcapp", SourceUrl: "http://example.com/acme/web.git",
		SourceAuthKind: "https_token", SourceAuthSecret: "long-enough-token-16",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("http + https_token err = %v, want InvalidArgument", err)
	}
	// 合法形态：https + 16+ 材料。
	if _, err := svc.SetAppSource(directCtx(ctx), &serverv1.SetAppSourceRequest{
		Name: "srcapp", SourceUrl: "https://example.com/acme/web.git",
		SourceAuthKind: "https_token", SourceAuthSecret: "long-enough-token-16",
	}); err != nil {
		t.Fatalf("valid https_token source rejected: %v", err)
	}
	// http:// + none 仍允许（匿名明文拉取是合法形态）。
	if _, err := svc.SetAppSource(directCtx(ctx), &serverv1.SetAppSourceRequest{
		Name: "srcapp", SourceUrl: "http://example.com/acme/web.git", SourceAuthKind: "none",
	}); err != nil {
		t.Fatalf("http + none must stay allowed: %v", err)
	}
}

// TestListAppsLimitDefaultAndBatchDerived S18-A4：limit 语义（0 → 缺省 100
// 截断，proto 注释口径落实；>0 照用）+ 批量派生（两次 IN 查询装配）与
// 单 app 读面（GetApp 的 per-app 查询路径）产出同源一致——多种派生形态
// 抽查（无部署 down / 绑定 blocked / 在途 running / 成功终态 running）。
func TestListAppsLimitDefaultAndBatchDerived(t *testing.T) {
	svc, store := newAppsTestEnv(t, nil)
	ctx := context.Background()

	// 120 个 app（超过缺省 100）。
	for i := 0; i < 120; i++ {
		if _, err := testsupport.SeedAppE(t, store, fmt.Sprintf("app-%03d", i)); err != nil {
			t.Fatalf("create app %d: %v", i, err)
		}
	}
	// 派生输入多样化（均在缺省窗口前 100 内）。
	appByName := func(name string) state.App {
		t.Helper()
		app, err := store.GetAppByName(ctx, name)
		if err != nil {
			t.Fatalf("GetAppByName %s: %v", name, err)
		}
		return app
	}
	// app-001：绑定 blocked + 在途部署行（down 优先于 blocked——无部署即
	// 无期望实例恒 down，blocked 形态需要先有部署记录）→ blocked。
	if _, err := store.BindPlacement(ctx, state.PlacementWrite{
		AppID: appByName("app-001").ID, PlatformNodeID: "n_bad",
		State: state.PlacementBlocked, Source: state.PlacementSourcePlatform,
	}); err != nil {
		t.Fatalf("bind blocked placement: %v", err)
	}
	if _, err := store.CreateDeployment(ctx, state.DeployRecord{
		AppID: appByName("app-001").ID, AppName: "app-001", Kind: "deploy",
	}); err != nil {
		t.Fatalf("seed app-001 deployment: %v", err)
	}
	// app-002：queued 部署行 → running（无失败、无成功终态）。
	if _, err := store.CreateDeployment(ctx, state.DeployRecord{
		AppID: appByName("app-002").ID, AppName: "app-002", Kind: "deploy",
	}); err != nil {
		t.Fatalf("seed queued deployment: %v", err)
	}
	// app-003：全链推进到 succeeded → running。
	dep, err := store.CreateDeployment(ctx, state.DeployRecord{
		AppID: appByName("app-003").ID, AppName: "app-003", Kind: "deploy",
	})
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	prev := state.DeployQueued
	for _, next := range []state.DeploymentStatus{
		state.DeployPreparing, state.DeployBuilding, state.DeployReleasing,
		state.DeployObserving, state.DeploySucceeded,
	} {
		n, p := next, prev
		if err := store.UpdateDeployment(ctx, dep.ID, state.DeploymentPatch{Status: &n, PrevStatus: &p}); err != nil {
			t.Fatalf("transition %s -> %s: %v", p, n, err)
		}
		prev = next
	}

	// limit=0：缺省 100 截断（命名升序 = created_at 升序的前 100 个）。
	resp, err := svc.ListApps(directCtx(ctx), &serverv1.ListAppsRequest{})
	if err != nil {
		t.Fatalf("ListApps default: %v", err)
	}
	if len(resp.GetApps()) != 100 {
		t.Fatalf("default limit apps = %d, want 100", len(resp.GetApps()))
	}
	if first, last := resp.GetApps()[0].GetName(), resp.GetApps()[99].GetName(); first != "app-000" || last != "app-099" {
		t.Fatalf("default window = [%s, %s], want [app-000, app-099]", first, last)
	}

	// limit=5：照用（前 5）。
	resp5, err := svc.ListApps(directCtx(ctx), &serverv1.ListAppsRequest{Limit: 5})
	if err != nil || len(resp5.GetApps()) != 5 {
		t.Fatalf("limit=5 apps = %d err=%v, want 5", len(resp5.GetApps()), err)
	}
	// limit=150：超集不截断（全部 120）。
	resp150, err := svc.ListApps(directCtx(ctx), &serverv1.ListAppsRequest{Limit: 150})
	if err != nil || len(resp150.GetApps()) != 120 {
		t.Fatalf("limit=150 apps = %d err=%v, want 120", len(resp150.GetApps()), err)
	}

	// 批量派生 vs 单 app 读面（GetApp per-app 路径）同源一致。
	derivedOf := func(apps []*serverv1.AppView, name string) string {
		t.Helper()
		for _, a := range apps {
			if a.GetName() == name {
				return a.GetDerivedState()
			}
		}
		t.Fatalf("app %s not in list", name)
		return ""
	}
	for name, want := range map[string]string{
		"app-000": "down",    // 无部署无绑定
		"app-001": "blocked", // 绑定 blocked
		"app-002": "running", // 在途 queued
		"app-003": "running", // 成功终态
	} {
		if got := derivedOf(resp.GetApps(), name); got != want {
			t.Fatalf("batch derived %s = %s, want %s", name, got, want)
		}
		detail, derr := svc.GetApp(directCtx(ctx), &serverv1.GetAppRequest{Name: name})
		if derr != nil {
			t.Fatalf("GetApp %s: %v", name, derr)
		}
		if detail.GetDerivedState() != want {
			t.Fatalf("GetApp derived %s = %s, want %s (batch and single-read surfaces use different sources)", name, detail.GetDerivedState(), want)
		}
	}
}
