package api

// v0.3 W3-S2 收口三件的服务面测试：
//   - 平台写面门扩全（rbac-teams §3.2「全局设置/通知 → 仅平台管理员」）：
//     SetLogsBackend / SetMetricsMode + NotificationsService 五写面
//     （Create/Update/Delete/Rotate/Test）——非平台管理员用户 PAT 403、
//     机具 admin 200、平台管理员 200；读面任意已认证 read 现状不动；
//   - Me 投影带项目覆写行（§3.3：出现/移除联动）；
//   - GetSystemStatus 指纹披露（FZ-12：端口注入形态与未装配形态）。
//
// 用户 PAT 一律**显式声明 admin scope**——排除 scope 门干扰，403 只能来自
// 平台写面门（被测语义）。

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// pwfEnv 平台写面门测试夹具：logs/metrics/notifications/auth/system 同
// bufconn 面 + 三种凭据（非平台管理员用户 PAT / 机具 admin / 平台管理员
// 用户 PAT）。
type pwfEnv struct {
	st         *state.Store
	conn       *grpc.ClientConn
	tokUser    string // 非平台管理员用户 PAT（声明 admin scope）
	tokRoot    string // 平台管理员用户 PAT（声明 admin scope）
	tokMachine string // 机具 admin 令牌
}

func newPlatformWriteFaceEnv(t *testing.T) *pwfEnv {
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
	// 首用户 = 平台管理员；第二名用户无标志。
	uRoot := mustUser(t, st, "root@example.com")
	uPlain := mustUser(t, st, "plain@example.com")

	env := &pwfEnv{st: st}
	env.tokRoot = seedUserPAT(t, st, uRoot.ID, ScopeAdmin)
	env.tokUser = seedUserPAT(t, st, uPlain.ID, ScopeAdmin)
	env.tokMachine = seedTokenPlain(t, st, ScopeAdmin)

	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterLogsServiceServer(srv, NewLogsService(st, nil))
	serverv1.RegisterMetricsServiceServer(srv, NewMetricsService(st))
	serverv1.RegisterNotificationsServiceServer(srv, NewNotificationsService(st, box))
	serverv1.RegisterAuthServiceServer(srv, NewAuthService(st))
	env.conn = serveBufconn(t, srv)
	return env
}

// TestPlatformWriteFaceGate 扩全矩阵：用户（非平台管理员）403 × 各写面；
// 机具 admin 与平台管理员 200（读面旁证不动）。
func TestPlatformWriteFaceGate(t *testing.T) {
	env := newPlatformWriteFaceEnv(t)
	ctx := context.Background()
	logs := serverv1.NewLogsServiceClient(env.conn)
	metrics := serverv1.NewMetricsServiceClient(env.conn)
	notify := serverv1.NewNotificationsServiceClient(env.conn)

	setBackend := func(tok string) error {
		_, err := logs.SetLogsBackend(authCtx(ctx, tok), &serverv1.SetLogsBackendRequest{Backend: state.LogsBackendJSONL})
		return err
	}
	setMode := func(tok string) error {
		_, err := metrics.SetMetricsMode(authCtx(ctx, tok), &serverv1.SetMetricsModeRequest{Mode: state.MetricsModeOn})
		return err
	}
	createEndpoint := func(tok string) (string, error) {
		resp, err := notify.CreateWebhookEndpoint(authCtx(ctx, tok), &serverv1.CreateWebhookEndpointRequest{
			Name: "ops-hook", Url: "http://127.0.0.1:1/hook",
			EventPatterns: []string{"deployment.*"},
		})
		if err != nil {
			return "", err
		}
		return resp.GetEndpoint().GetId(), nil
	}

	// 非平台管理员用户 PAT：十个写面全部 403（凭据声明 admin——门在平台面）。
	for name, call := range map[string]func() error{
		"SetLogsBackend":        func() error { return setBackend(env.tokUser) },
		"SetMetricsMode":        func() error { return setMode(env.tokUser) },
		"CreateWebhookEndpoint": func() error { _, err := createEndpoint(env.tokUser); return err },
		"UpdateWebhookEndpoint": func() error {
			_, err := notify.UpdateWebhookEndpoint(authCtx(ctx, env.tokUser), &serverv1.UpdateWebhookEndpointRequest{Id: "01TEST"})
			return err
		},
		"DeleteWebhookEndpoint": func() error {
			_, err := notify.DeleteWebhookEndpoint(authCtx(ctx, env.tokUser), &serverv1.DeleteWebhookEndpointRequest{Id: "01TEST"})
			return err
		},
		"RotateWebhookSecret": func() error {
			_, err := notify.RotateWebhookSecret(authCtx(ctx, env.tokUser), &serverv1.RotateWebhookSecretRequest{Id: "01TEST"})
			return err
		},
		"TestEndpoint": func() error {
			_, err := notify.TestWebhook(authCtx(ctx, env.tokUser), &serverv1.TestWebhookRequest{Id: "01TEST"})
			return err
		},
		"GetSmtpSettings": func() error {
			_, err := notify.GetSmtpSettings(authCtx(ctx, env.tokUser), &serverv1.GetSmtpSettingsRequest{})
			return err
		},
		"UpdateSmtpSettings": func() error {
			_, err := notify.UpdateSmtpSettings(authCtx(ctx, env.tokUser), &serverv1.UpdateSmtpSettingsRequest{
				Host: "smtp.example.test", Port: 25, From: "f@example.test",
			})
			return err
		},
		"TestSmtp": func() error {
			_, err := notify.TestSmtp(authCtx(ctx, env.tokUser), &serverv1.TestSmtpRequest{To: "probe@example.test"})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s with non-platform-admin user PAT err = %v, want PermissionDenied", name, err)
		}
	}

	// 机具 admin：全部写面 200（scope 门 = 平台管理员等价）。
	if err := setBackend(env.tokMachine); err != nil {
		t.Fatalf("machine SetLogsBackend: %v", err)
	}
	if err := setMode(env.tokMachine); err != nil {
		t.Fatalf("machine SetMetricsMode: %v", err)
	}
	id, err := createEndpoint(env.tokMachine)
	if err != nil {
		t.Fatalf("machine CreateWebhookEndpoint: %v", err)
	}
	if _, err := notify.UpdateWebhookEndpoint(authCtx(ctx, env.tokMachine), &serverv1.UpdateWebhookEndpointRequest{Id: id, Enabled: protoBool(false)}); err != nil {
		t.Fatalf("machine UpdateWebhookEndpoint: %v", err)
	}
	if _, err := notify.RotateWebhookSecret(authCtx(ctx, env.tokMachine), &serverv1.RotateWebhookSecretRequest{Id: id}); err != nil {
		t.Fatalf("machine RotateWebhookSecret: %v", err)
	}
	// TestEndpoint 指向不可达环回端点：RPC 成功（响应 ok=false）即 200 证明
	// ——投递结论是业务事实，不是凭据拒绝。
	if resp, err := notify.TestWebhook(authCtx(ctx, env.tokMachine), &serverv1.TestWebhookRequest{Id: id}); err != nil {
		t.Fatalf("machine TestEndpoint: %v", err)
	} else if resp.GetOk() {
		t.Fatal("test endpoint probe to an unreachable receiver must report ok=false")
	}
	// SMTP 面：Get 返回缺省态；Update 保存（真实设置面写入）；TestSmtp 对
	// 不可达中继探针（RPC 成功 + ok=false 即 200 证明）。Update 后清理：
	// 再 Update 成空设置（同 PUT 语义覆盖）。
	if _, err := notify.GetSmtpSettings(authCtx(ctx, env.tokMachine), &serverv1.GetSmtpSettingsRequest{}); err != nil {
		t.Fatalf("machine GetSmtpSettings: %v", err)
	}
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	if _, err := notify.UpdateSmtpSettings(authCtx(ctx, env.tokMachine), &serverv1.UpdateSmtpSettingsRequest{
		Host: "127.0.0.1", Port: 1, From: "fleetly@example.test", Password: "pw-e2e-only",
	}); err != nil {
		t.Fatalf("machine UpdateSmtpSettings: %v", err)
	}
	if resp, err := notify.TestSmtp(authCtx(ctx, env.tokMachine), &serverv1.TestSmtpRequest{To: "probe@example.test"}); err != nil {
		t.Fatalf("machine TestSmtp: %v", err)
	} else if resp.GetOk() {
		t.Fatal("smtp probe against an unreachable relay must report ok=false")
	}
	if _, err := notify.DeleteWebhookEndpoint(authCtx(ctx, env.tokMachine), &serverv1.DeleteWebhookEndpointRequest{Id: id}); err != nil {
		t.Fatalf("machine DeleteWebhookEndpoint: %v", err)
	}

	// 平台管理员用户 PAT：抽检两写面 200。
	if err := setBackend(env.tokRoot); err != nil {
		t.Fatalf("platform admin SetLogsBackend: %v", err)
	}
	if _, err := createEndpoint(env.tokRoot); err != nil {
		t.Fatalf("platform admin CreateWebhookEndpoint: %v", err)
	}

	// 读面现状不动：任意已认证 read（非平台管理员用户 PAT）200。
	if _, err := logs.GetLogsBackend(authCtx(ctx, env.tokUser), &serverv1.GetLogsBackendRequest{}); err != nil {
		t.Fatalf("read face GetLogsBackend must stay open: %v", err)
	}
	if _, err := metrics.GetMetricsStatus(authCtx(ctx, env.tokUser), &serverv1.GetMetricsStatusRequest{}); err != nil {
		t.Fatalf("read face GetMetricsStatus must stay open: %v", err)
	}
	if _, err := notify.ListWebhookEndpoints(authCtx(ctx, env.tokUser), &serverv1.ListWebhookEndpointsRequest{}); err != nil {
		t.Fatalf("read face ListWebhookEndpoints must stay open: %v", err)
	}
}

// TestMeProjectOverrideProjection Me 投影带覆写行（§3.3）：行随
// SetProjectMemberRole 出现、随 RemoveProjectMember 消失；字段投影
// project_id/team_id/prj_slug/role 全对；团队投影不受影响。
func TestMeProjectOverrideProjection(t *testing.T) {
	env := newPlatformWriteFaceEnv(t)
	ctx := context.Background()
	me := serverv1.NewAuthServiceClient(env.conn)

	st := env.st
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var uPlain state.User
	for _, u := range users {
		if u.Email == "plain@example.com" {
			uPlain = u
		}
	}
	if uPlain.ID == "" {
		t.Fatal("fixture user plain@example.com missing")
	}

	team := mustTeam(t, st, "acme", "Acme", "seed")
	mustMember(t, st, team.ID, uPlain.ID, state.TeamRoleDeveloper)
	proj := mustProject(t, st, team.ID, "default")

	// 无覆写行：project_overrides 空。
	base, err := me.Me(authCtx(ctx, env.tokUser), &serverv1.MeRequest{})
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if len(base.GetProjectOverrides()) != 0 {
		t.Fatalf("Me without override rows = %+v, want empty project_overrides", base.GetProjectOverrides())
	}

	// 覆写行出现：团队 developer 在 default 降 viewer。
	if _, err := st.SetProjectMemberRole(ctx, proj.ID, uPlain.ID, state.ProjectRoleViewer, "", ""); err != nil {
		t.Fatalf("SetProjectMemberRole: %v", err)
	}
	withOverride, err := me.Me(authCtx(ctx, env.tokUser), &serverv1.MeRequest{})
	if err != nil {
		t.Fatalf("Me with override: %v", err)
	}
	overrides := withOverride.GetProjectOverrides()
	if len(overrides) != 1 {
		t.Fatalf("project_overrides = %+v, want exactly 1 row", overrides)
	}
	row := overrides[0]
	if row.GetProjectId() != proj.ID || row.GetTeamId() != team.ID ||
		row.GetPrjSlug() != "default" || row.GetRole() != state.ProjectRoleViewer {
		t.Fatalf("override row = %+v, want project=%s team=%s slug=default role=viewer",
			row, proj.ID, team.ID)
	}
	// 团队投影不受覆写影响。
	if len(withOverride.GetTeams()) != 1 || withOverride.GetTeams()[0].GetRole() != state.TeamRoleDeveloper {
		t.Fatalf("teams projection = %+v, want single developer membership", withOverride.GetTeams())
	}

	// 覆写行移除：投影联动消失。
	if err := st.RemoveProjectMember(ctx, proj.ID, uPlain.ID, "", ""); err != nil {
		t.Fatalf("RemoveProjectMember: %v", err)
	}
	after, err := me.Me(authCtx(ctx, env.tokUser), &serverv1.MeRequest{})
	if err != nil {
		t.Fatalf("Me after removal: %v", err)
	}
	if len(after.GetProjectOverrides()) != 0 {
		t.Fatalf("project_overrides after removal = %+v, want empty", after.GetProjectOverrides())
	}
}

// newSettingsStoreForAPITest 起一个独立临时目录 store（api 侧纯投影测试）。
func newSettingsStoreForAPITest(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// protoBool 是 optional bool 字段的测试构造。
func protoBool(v bool) *bool { return &v }
