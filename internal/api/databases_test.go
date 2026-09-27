package api

// DatabaseService 的 API 面单测（E4 W4-S2 验收清单第 4 条）：逐 RPC 覆盖
// ——创建/读取/列表、脱敏连接投影（明文零出现断言）、删除 confirm 两段式
// 与引用守卫 409、状态机前置态冲突 → E_STATE_VERSION_CONFLICT、终端态
// 404、模板未知 400、重试/恢复/暂停受理。收敛行为不在本面（internal/
// database 单测覆盖）。

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// newDatabaseTestEnv 起带 DatabaseService 的 bufconn 测试环境（真实 store +
// envelope box + 鉴权链，harness 同构）+ 确定性夹具项目 "fixture"（v0.3 归属
// 必填——创建请求显式携带，机具令牌裸名解析全域唯一命中）。
func newDatabaseTestEnv(t *testing.T) (*state.Store, *secrets.Box, serverv1.DatabaseServiceClient, string) {
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
	auth := NewAuthenticator(st)
	srv := newAuthServer(auth)
	serverv1.RegisterDatabaseServiceServer(srv, NewDatabaseService(st, box, nil, nil, nil))
	conn := serveBufconn(t, srv)
	token := seedTokenPlain(t, st, "admin")
	seedFixtureProject(t, st)
	return st, box, serverv1.NewDatabaseServiceClient(conn), token
}

// mustCreate 用 admin 凭据创建一个实例并返回视图（归属 = 夹具项目）。
func mustCreate(t *testing.T, cl serverv1.DatabaseServiceClient, token, name, template string) *serverv1.DatabaseView {
	t.Helper()
	resp, err := cl.CreateDatabase(authCtx(context.Background(), token), &serverv1.CreateDatabaseRequest{
		Name: name, Template: template, Project: "fixture",
	})
	if err != nil {
		t.Fatalf("CreateDatabase %s: %v", name, err)
	}
	return resp.GetDatabase()
}

// envCode 断言错误携带指定稳定码（apperr 信封）。
func envCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s, got nil", want)
	}
	e, ok := apperr.FromError(err)
	if !ok || e.Code() != want {
		code := ""
		if ok {
			code = e.Code()
		}
		t.Fatalf("error code = %q (ok=%v), want %s (err=%v)", code, ok, want, err)
	}
}

func TestDatabaseCreateAndGetMasked(t *testing.T) {
	st, box, cl, token := newDatabaseTestEnv(t)
	ctx := authCtx(context.Background(), token)

	view := mustCreate(t, cl, token, "pg-prod", dbtemplate.TemplatePostgres16)
	if view.GetStatus() != string(state.DatabaseProvisioning) {
		t.Fatalf("status = %s, want provisioning (acceptance is provisioning)", view.GetStatus())
	}
	if view.GetImageDigest() == "" || view.GetTemplate() != dbtemplate.TemplatePostgres16 {
		t.Fatalf("view = %+v, want template digest pinned", view)
	}

	// 脱敏投影：URL 掩码 + 指纹，明文零出现。
	got, err := cl.GetDatabase(ctx, &serverv1.GetDatabaseRequest{Name: "pg-prod"})
	if err != nil {
		t.Fatalf("GetDatabase: %v", err)
	}
	conn := got.GetDatabase().GetConnection()
	if conn == nil || !strings.Contains(conn.GetUrl(), connectionPasswordMask) {
		t.Fatalf("connection url = %v, want masked", conn)
	}
	if conn.GetPasswordFingerprint() == "" || len(conn.GetPasswordFingerprint()) != 8 {
		t.Fatalf("fingerprint = %q, want 8-hex hash8", conn.GetPasswordFingerprint())
	}
	if conn.GetUser() != "fleetly" || conn.GetDatabase() != "pg_prod" {
		t.Fatalf("PG connection = %+v, want fleetly/pg_prod (name '-'→'_')", conn)
	}
	if conn.GetHost() != "pg-prod" || conn.GetPort() != 5432 {
		t.Fatalf("host/port = %s/%d, want pg-prod/5432", conn.GetHost(), conn.GetPort())
	}

	// 明文零出现：解密真值并扫描整响应序列化形态。
	inst, err := st.GetDatabaseInstanceByName(context.Background(), "pg-prod")
	if err != nil {
		t.Fatalf("load instance: %v", err)
	}
	plain, err := box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	raw := got.GetDatabase().String()
	if strings.Contains(raw, string(plain)) {
		t.Fatal("credential plaintext leaked into GetDatabase response")
	}

	// 未知模板 → E_DB_TEMPLATE_UNSUPPORTED。
	_, err = cl.CreateDatabase(ctx, &serverv1.CreateDatabaseRequest{Name: "pg-x", Template: "mysql-8"})
	envCode(t, err, "E_DB_TEMPLATE_UNSUPPORTED")
	// 词表人读形态（错误信息可行动面）覆盖全部注册表条目——IMPL-DB-1 的
	// 两新条目随 List() 自动扩，本断言把「词表同步」钉住。
	for _, tpl := range dbtemplate.List() {
		if !strings.Contains(templateIDList(), tpl.ID) {
			t.Fatalf("templateIDList() = %q, missing registry entry %s", templateIDList(), tpl.ID)
		}
	}
	// IMPL-DB-1：两新 PG 条目在词表内（受理 + 行上钉定镜像 digest）。
	for _, tc := range []struct{ name, template string }{
		{"pg18", dbtemplate.TemplatePostgres18},
		{"pgvec", dbtemplate.TemplatePerconaPostgresql18},
	} {
		row := mustCreate(t, cl, token, tc.name, tc.template)
		tpl, err := dbtemplate.Get(tc.template)
		if err != nil {
			t.Fatalf("dbtemplate.Get(%s): %v", tc.template, err)
		}
		if row.GetTemplate() != tc.template || row.GetImageDigest() != tpl.Image {
			t.Fatalf("%s row = template %q image %q, want %q/%q",
				tc.name, row.GetTemplate(), row.GetImageDigest(), tc.template, tpl.Image)
		}
	}
	// 非法名 → 400。
	_, err = cl.CreateDatabase(ctx, &serverv1.CreateDatabaseRequest{Name: "Bad_Name!", Template: dbtemplate.TemplateRedis7})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid name code = %v, want InvalidArgument", status.Code(err))
	}
	// 重名 → 409。
	_, err = cl.CreateDatabase(ctx, &serverv1.CreateDatabaseRequest{Name: "pg-prod", Template: dbtemplate.TemplateRedis7, Project: "fixture"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("duplicate name code = %v, want FailedPrecondition (409)", status.Code(err))
	}
}

// boxOf 是测试用 envelope box（与 harness 同构：独立 key 文件）。
func boxOf(t *testing.T) *secrets.Box {
	t.Helper()
	box, _, err := secrets.EnsureKey(t.TempDir() + "/box.key")
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	return box
}

func TestDatabaseListExcludesDeleted(t *testing.T) {
	_, _, cl, token := newDatabaseTestEnv(t)
	mustCreate(t, cl, token, "db-a", dbtemplate.TemplatePostgres16)
	mustCreate(t, cl, token, "db-b", dbtemplate.TemplateRedis7)
	resp, err := cl.ListDatabases(authCtx(context.Background(), token), &serverv1.ListDatabasesRequest{})
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	if len(resp.GetDatabases()) != 2 {
		t.Fatalf("rows = %d, want 2", len(resp.GetDatabases()))
	}
}

func TestDatabaseLifecycleRPCs(t *testing.T) {
	st, _, cl, token := newDatabaseTestEnv(t)
	ctx := authCtx(context.Background(), token)
	view := mustCreate(t, cl, token, "pg-life", dbtemplate.TemplatePostgres16)
	id := view.GetId()

	// 收敛过健康门（模拟 duty）：provisioning → ready。
	if err := st.EnterDbPhase(context.Background(), id,
		state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("duty ready: %v", err)
	}

	// suspend：ready → paused。
	resp, err := cl.SuspendDatabase(ctx, &serverv1.SuspendDatabaseRequest{Name: "pg-life"})
	if err != nil {
		t.Fatalf("SuspendDatabase: %v", err)
	}
	if resp.GetDatabase().GetStatus() != string(state.DatabasePaused) {
		t.Fatalf("status = %s, want paused", resp.GetDatabase().GetStatus())
	}
	// resume：paused → provisioning。
	resp2, err := cl.ResumeDatabase(ctx, &serverv1.ResumeDatabaseRequest{Name: "pg-life"})
	if err != nil {
		t.Fatalf("ResumeDatabase: %v", err)
	}
	if resp2.GetDatabase().GetStatus() != string(state.DatabaseProvisioning) {
		t.Fatalf("status = %s, want provisioning", resp2.GetDatabase().GetStatus())
	}
	// duty 落 ready → suspend 再入 paused（双前置态分支的 ready 路径已覆盖；
	// 这里补 degraded 前置态：provisioning → ready → degraded → suspend）。
	if err := st.EnterDbPhase(context.Background(), id,
		state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("duty ready 2: %v", err)
	}
	if err := st.EnterDbPhase(context.Background(), id,
		state.DatabaseReady, state.DatabaseDegraded); err != nil {
		t.Fatalf("duty degraded: %v", err)
	}
	if _, err := cl.SuspendDatabase(ctx, &serverv1.SuspendDatabaseRequest{Name: "pg-life"}); err != nil {
		t.Fatalf("SuspendDatabase from degraded: %v", err)
	}
	// 非法前置态：paused 上 suspend → 409 E_STATE_VERSION_CONFLICT。
	_, err = cl.SuspendDatabase(ctx, &serverv1.SuspendDatabaseRequest{Name: "pg-life"})
	envCode(t, err, "E_STATE_VERSION_CONFLICT")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("conflict grpc code = %v, want FailedPrecondition", status.Code(err))
	}
	// failed → retry → provisioning。
	if _, err := cl.ResumeDatabase(ctx, &serverv1.ResumeDatabaseRequest{Name: "pg-life"}); err != nil {
		t.Fatalf("ResumeDatabase: %v", err)
	}
	if err := st.SetDatabaseLastError(context.Background(), id, "health gate timeout after 5m0s"); err != nil {
		t.Fatalf("set last error: %v", err)
	}
	if err := st.EnterDbPhase(context.Background(), id,
		state.DatabaseProvisioning, state.DatabaseFailed); err != nil {
		t.Fatalf("duty failed: %v", err)
	}
	resp3, err := cl.RetryDatabase(ctx, &serverv1.RetryDatabaseRequest{Name: "pg-life"})
	if err != nil {
		t.Fatalf("RetryDatabase: %v", err)
	}
	if resp3.GetDatabase().GetStatus() != string(state.DatabaseProvisioning) {
		t.Fatalf("after retry status = %s, want provisioning", resp3.GetDatabase().GetStatus())
	}
	// settings：主状态不变 + 限额落列。
	resp4, err := cl.UpdateDatabaseSettings(ctx, &serverv1.UpdateDatabaseSettingsRequest{
		Name:   "pg-life",
		Limits: &serverv1.DatabaseLimits{CpuSeconds: 2.0, MemoryBytes: 1 << 30},
	})
	if err != nil {
		t.Fatalf("UpdateDatabaseSettings: %v", err)
	}
	if resp4.GetDatabase().GetLimits().GetCpuSeconds() != 2.0 {
		t.Fatalf("limits = %+v, want cpu 2.0", resp4.GetDatabase().GetLimits())
	}
	if resp4.GetDatabase().GetStatus() != string(state.DatabaseProvisioning) {
		t.Fatalf("settings changed state = %s, want unchanged", resp4.GetDatabase().GetStatus())
	}
}

func TestDatabaseDeleteGuardAndConfirm(t *testing.T) {
	st, _, cl, token := newDatabaseTestEnv(t)
	ctx := authCtx(context.Background(), token)
	view := mustCreate(t, cl, token, "pg-ref", dbtemplate.TemplatePostgres16)
	_ = mustCreate(t, cl, token, "pg-free", dbtemplate.TemplatePostgres16)
	// 收敛过健康门（模拟 duty）——ready 是删除的合法前置态。
	if err := st.EnterDbPhase(context.Background(), view.GetId(),
		state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("duty ready: %v", err)
	}

	// confirm mismatch → 400。
	_, err := cl.DeleteDatabase(ctx, &serverv1.DeleteDatabaseRequest{Name: "pg-ref", Confirm: "wrong"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("confirm mismatch code = %v, want InvalidArgument", status.Code(err))
	}

	// 引用守卫：db_references 非空 → 409 E_DB_REFERENCED 附清单。
	app, err := testsupport.SeedAppE(t, st, "consumer")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.UpsertDatabaseReference(context.Background(), state.DatabaseReference{
		DatabaseID: view.GetId(), AppID: app.ID, Service: "web", EnvPrefix: "FLEETLY_DB_PG_REF",
	}); err != nil {
		t.Fatalf("seed reference: %v", err)
	}
	_, err = cl.DeleteDatabase(ctx, &serverv1.DeleteDatabaseRequest{Name: "pg-ref", Confirm: "pg-ref"})
	envCode(t, err, "E_DB_REFERENCED")
	e, _ := apperr.FromError(err)
	if !strings.Contains(e.Message(), "consumer/web") {
		t.Fatalf("referenced message = %q, want app/service list", e.Message())
	}

	// 引用清零后删除 → deleting；再删 → 404（deleting 目标不可见）。
	if _, err := st.DeleteDatabaseReferencesForApp(context.Background(), app.ID); err != nil {
		t.Fatalf("clear references: %v", err)
	}
	// provisioning 无删除出边（S1 冻结转移表）→ 409 + 合法前置态清单。
	_ = mustCreate(t, cl, token, "pg-young", dbtemplate.TemplatePostgres16)
	_, err = cl.DeleteDatabase(ctx, &serverv1.DeleteDatabaseRequest{Name: "pg-young", Confirm: "pg-young"})
	envCode(t, err, "E_STATE_VERSION_CONFLICT")
	dresp, err := cl.DeleteDatabase(ctx, &serverv1.DeleteDatabaseRequest{Name: "pg-ref", Confirm: "pg-ref", DeleteVolumes: true})
	if err != nil {
		t.Fatalf("DeleteDatabase: %v", err)
	}
	if dresp.GetStatus() != string(state.DatabaseDeleting) {
		t.Fatalf("status = %s, want deleting", dresp.GetStatus())
	}
	_, err = cl.DeleteDatabase(ctx, &serverv1.DeleteDatabaseRequest{Name: "pg-ref", Confirm: "pg-ref"})
	envCode(t, err, "E_DB_NOT_FOUND")

	// 不存在 → 404。
	_, err = cl.GetDatabase(ctx, &serverv1.GetDatabaseRequest{Name: "nope"})
	envCode(t, err, "E_DB_NOT_FOUND")
}
