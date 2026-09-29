package api

// Rotate/Reveal 的 API 面单测（E4 W4-S4 验收 5/6）：两段式 confirm、编排
// 错误映射（前置态/CAS → E_STATE_VERSION_CONFLICT 族；中途失败 →
// E_DB_ROTATE_FAILED 带阶段上下文）、reveal 明文展开 + db.reveal 审计
// （值零进审计）、scope 面（admin 专用）。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/database"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/testsupport"
)

// fakeRotator 是 CredentialRotator 端口的测试替身（编排本体在 internal/
// database 单测；本面只验映射与受理语义）。
type fakeRotator struct {
	apps []string
	err  error
}

func (f *fakeRotator) RotateCredentials(_ context.Context, _ string) ([]string, error) {
	return f.apps, f.err
}

// newRotateTestEnv 起带 fake rotator/ops 的 DatabaseService bufconn 环境。
func newRotateTestEnv(t *testing.T, rotator CredentialRotator) (*state.Store, *secrets.Box, serverv1.DatabaseServiceClient, string) {
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
	auth := NewAuthenticator(st)
	srv := newAuthServer(auth)
	serverv1.RegisterDatabaseServiceServer(srv, NewDatabaseService(st, box, nil, rotator, &fakeOps{}))
	conn := serveBufconn(t, srv)
	token := seedTokenPlain(t, st, "admin")
	seedFixtureProject(t, st) // 归属夹具（确定性 slug "fixture"——mustCreate 消费）
	return st, box, serverv1.NewDatabaseServiceClient(conn), token
}

// fakeOps 是 BackupOrchestrator 端口的测试替身（S5 编排本体在 internal/
// database 单测；本面只验映射与受理语义——calls 记录受理转发）。
type fakeOps struct {
	err        error
	upgradeOld string
	upgradeNew string
	calls      []opsCall
}

// opsCall 是一次编排端口的受理转发记录（op + 目标实例名）。
type opsCall struct{ op, name string }

func (f *fakeOps) TriggerBackup(_ context.Context, name string) error {
	f.calls = append(f.calls, opsCall{"backup", name})
	return f.err
}

func (f *fakeOps) RestoreBackup(_ context.Context, name, _ string) error {
	f.calls = append(f.calls, opsCall{"restore", name})
	return f.err
}

func (f *fakeOps) Upgrade(_ context.Context, name string) (string, string, error) {
	f.calls = append(f.calls, opsCall{"upgrade", name})
	return f.upgradeOld, f.upgradeNew, f.err
}

// mustReady 推进实例到 ready（模拟收敛循环健康门）。
func mustReady(t *testing.T, st *state.Store, id string) {
	t.Helper()
	if err := st.EnterDbPhase(context.Background(), id, state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("converge ready: %v", err)
	}
}

func TestDatabaseRotateTwoPhase(t *testing.T) {
	rot := &fakeRotator{apps: []string{"consumer"}}
	st, _, cl, token := newRotateTestEnv(t, rot)
	ctx := authCtx(context.Background(), token)
	view := mustCreate(t, cl, token, "pg-r", dbtemplate.TemplatePostgres16)
	mustReady(t, st, view.GetId())

	// confirm 缺失/mismatch → 400（破坏性两段式）。
	if _, err := cl.RotateDatabaseCredentials(ctx, &serverv1.RotateDatabaseCredentialsRequest{Name: "pg-r"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing confirm code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := cl.RotateDatabaseCredentials(ctx, &serverv1.RotateDatabaseCredentialsRequest{Name: "pg-r", Confirm: "wrong"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("confirm mismatch code = %v, want InvalidArgument", status.Code(err))
	}

	resp, err := cl.RotateDatabaseCredentials(ctx, &serverv1.RotateDatabaseCredentialsRequest{Name: "pg-r", Confirm: "pg-r"})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if resp.GetDatabase().GetName() != "pg-r" || len(resp.GetRedeployedApps()) != 1 || resp.GetRedeployedApps()[0] != "consumer" {
		t.Fatalf("rotate response = %+v, want database + redeployed [consumer]", resp)
	}

	// 成功审计 db.rotate（diff 只带 app 名单——编排层数据）。
	audits, err := st.RecentAudits(context.Background(), 50)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	var hit bool
	for _, a := range audits {
		if a.Action == "db.rotate" && a.Target == "database:"+view.GetId() {
			hit = true
		}
	}
	if !hit {
		t.Fatal("db.rotate audit missing")
	}
}

func TestDatabaseRotateErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			name:     "prestate violation",
			err:      &database.RotationPrestateError{Current: "provisioning"},
			wantCode: "E_STATE_VERSION_CONFLICT",
		},
		{
			name:     "paused postgres refusal",
			err:      fmt.Errorf("%w: pg-paused", database.ErrPGRotationPaused),
			wantCode: "E_STATE_VERSION_CONFLICT",
		},
		{
			name:     "concurrent CAS conflict",
			err:      database.ErrRotationConflict,
			wantCode: "E_STATE_VERSION_CONFLICT",
		},
		{
			name:     "mid-flight stage failure",
			err:      &database.RotationStageError{Stage: "materialize", Err: errors.New("disk full")},
			wantCode: "E_DB_ROTATE_FAILED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, _, cl, token := newRotateTestEnv(t, &fakeRotator{err: tc.err})
			ctx := authCtx(context.Background(), token)
			view := mustCreate(t, cl, token, "pg-m", dbtemplate.TemplatePostgres16)
			mustReady(t, st, view.GetId())

			_, err := cl.RotateDatabaseCredentials(ctx, &serverv1.RotateDatabaseCredentialsRequest{Name: "pg-m", Confirm: "pg-m"})
			envCode(t, err, tc.wantCode)
			if tc.wantCode == "E_DB_ROTATE_FAILED" {
				e, _ := apperr.FromError(err)
				if e.Context()["stage"] != "materialize" {
					t.Fatalf("stage context = %v, want materialize", e.Context())
				}
			}
		})
	}
}

func TestDatabaseReveal(t *testing.T) {
	st, box, cl, token := newRotateTestEnv(t, nil)
	ctx := authCtx(context.Background(), token)
	view := mustCreate(t, cl, token, "pg-reveal", dbtemplate.TemplateRedis7)

	// 收敛前的凭据在创建时已生成——reveal 需要可解密密文；这里直接比对
	// 响应密码与库内密文的解密值（管理面自检）。
	inst, err := st.GetDatabaseInstance(context.Background(), view.GetId())
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	plainRaw, err := box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		t.Fatalf("decrypt for assertion: %v", err)
	}
	plain := string(plainRaw)

	resp, err := cl.RevealDatabaseCredentials(ctx, &serverv1.RevealDatabaseCredentialsRequest{Name: "pg-reveal"})
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if resp.GetPassword() != plain || resp.GetPassword() == "" {
		t.Fatal("reveal password mismatch")
	}
	if resp.GetUrl() == "" || !strings.Contains(resp.GetUrl(), plain) {
		t.Fatal("reveal URL must embed the live password")
	}

	// 审计 db.reveal：访问留痕 + 值零出现（验收 6 负面项）。
	audits, err := st.RecentAudits(context.Background(), 50)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	var hit bool
	for _, a := range audits {
		if a.Action == "db.reveal" {
			hit = true
			if strings.Contains(a.DiffSummary, plain) || strings.Contains(a.Target, plain) {
				t.Fatal("reveal audit carries credential material")
			}
		}
	}
	if !hit {
		t.Fatal("db.reveal audit missing")
	}
}

// scope 断言：rotate/reveal/setSecret 为 admin 专用——read token 一律 403。
func TestSecretSurfacesScopeAdminOnly(t *testing.T) {
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
	auth := NewAuthenticator(st)
	srv := newAuthServer(auth)
	dbs := NewDatabaseService(st, box, nil, &fakeRotator{}, &fakeOps{})
	serverv1.RegisterDatabaseServiceServer(srv, dbs)
	serverv1.RegisterSecretsServiceServer(srv, NewSecretsService(st, box))
	conn := serveBufconn(t, srv)
	readTok := seedTokenPlain(t, st, "read")
	adminTok := seedTokenPlain(t, st, "admin")

	readCtx := authCtx(context.Background(), readTok)
	if _, err := serverv1.NewDatabaseServiceClient(conn).RotateDatabaseCredentials(readCtx,
		&serverv1.RotateDatabaseCredentialsRequest{Name: "x", Confirm: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read rotate code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := serverv1.NewDatabaseServiceClient(conn).RevealDatabaseCredentials(readCtx,
		&serverv1.RevealDatabaseCredentialsRequest{Name: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read reveal code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := serverv1.NewSecretsServiceClient(conn).SetSecret(readCtx,
		&serverv1.SetSecretRequest{App: "a", Name: "n", Value: "v"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read setSecret code = %v, want PermissionDenied", status.Code(err))
	}
	// admin 侧形状错误可达 handler（鉴权放行；bufvalidate 拦截器不在本
	// 最小夹具链上——值上限由 handler 兜底校验承载）。
	testsupport.SeedApp(t, st, "a")
	if _, err := serverv1.NewSecretsServiceClient(conn).SetSecret(authCtx(context.Background(), adminTok),
		&serverv1.SetSecretRequest{App: "a", Name: "n", Value: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("admin empty value code = %v, want InvalidArgument", status.Code(err))
	}
}
