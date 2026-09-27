package api

// ConfigsService 的 API 面单测（T 线 OT-3 / IMPL-T1-4）：set（覆盖即换版）→
// list（名称/指纹投影——值零出现）→ get（明文回读）→ remove；审计动作词表
// config.set/config.removed 且内容零进审计；名称形状与值大小配额（守卫③
// 4xx 点名）；Get=admin、List=read 的 scope 矩阵与 secrets 同构（守卫⑤）。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// newConfigsTestEnv 起带 ConfigsService 的 bufconn 测试环境。
func newConfigsTestEnv(t *testing.T) (*state.Store, serverv1.ConfigsServiceClient, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	auth := NewAuthenticator(st)
	srv := newAuthServer(auth)
	serverv1.RegisterConfigsServiceServer(srv, NewConfigsService(st))
	conn := serveBufconn(t, srv)
	token := seedTokenPlain(t, st, "admin")
	return st, serverv1.NewConfigsServiceClient(conn), token
}

func TestConfigsSetListGetRemove(t *testing.T) {
	st, cl, token := newConfigsTestEnv(t)
	ctx := authCtx(context.Background(), token)
	if _, err := testsupport.SeedAppE(t, st, "cfgapp"); err != nil {
		t.Fatalf("create app: %v", err)
	}

	const contentV1 = "listen: 8080\nlog_level: info\n"
	set, err := cl.SetConfig(ctx, &serverv1.SetConfigRequest{App: "cfgapp", Name: "app.yaml", Value: contentV1})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if set.GetHash8() != naming.Hash8(contentV1) {
		t.Fatalf("hash8 = %s, want %s (content fingerprint)", set.GetHash8(), naming.Hash8(contentV1))
	}

	// 明文落库（config 不是凭据面——与 app_secrets 的密文口径刻意不同）。
	row, err := st.GetAppConfig(ctx, mustAppIDByName(t, st, "cfgapp"), "app.yaml")
	if err != nil {
		t.Fatalf("get config row: %v", err)
	}
	if row.Value != contentV1 {
		t.Fatalf("stored value = %q, want the plaintext content", row.Value)
	}

	// 覆盖即换版：指纹重盖 + Get 回读新内容。
	const contentV2 = "listen: 9090\n"
	set2, err := cl.SetConfig(ctx, &serverv1.SetConfigRequest{App: "cfgapp", Name: "app.yaml", Value: contentV2})
	if err != nil {
		t.Fatalf("rotate SetConfig: %v", err)
	}
	if set2.GetHash8() == set.GetHash8() {
		t.Fatal("content change must move the fingerprint (content-addressed object rotation)")
	}
	got, err := cl.GetConfig(ctx, &serverv1.GetConfigRequest{App: "cfgapp", Name: "app.yaml"})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got.GetValue() != contentV2 || got.GetHash8() != set2.GetHash8() {
		t.Fatalf("get = %+v, want the rotated plaintext value + fingerprint", got)
	}

	// list：只投影名称/指纹/时间锚——值零出现（结构性：视图无值字段）。
	list, err := cl.ListConfigs(ctx, &serverv1.ListConfigsRequest{App: "cfgapp"})
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(list.GetConfigs()) != 1 || list.GetConfigs()[0].GetName() != "app.yaml" {
		t.Fatalf("list = %+v, want one app.yaml entry", list)
	}
	if strings.Contains(list.String(), contentV1) || strings.Contains(list.String(), contentV2) {
		t.Fatal("list response carries config content")
	}

	// get 不存在 → 404；remove 成功后二次删除 404。
	if _, err := cl.GetConfig(ctx, &serverv1.GetConfigRequest{App: "cfgapp", Name: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("get missing code = %v, want NotFound", status.Code(err))
	}
	if _, err := cl.RemoveConfig(ctx, &serverv1.RemoveConfigRequest{App: "cfgapp", Name: "app.yaml"}); err != nil {
		t.Fatalf("RemoveConfig: %v", err)
	}
	if _, err := cl.RemoveConfig(ctx, &serverv1.RemoveConfigRequest{App: "cfgapp", Name: "app.yaml"}); status.Code(err) != codes.NotFound {
		t.Fatalf("second remove code = %v, want NotFound", status.Code(err))
	}

	// 审计词表：config.set / config.removed；内容零出现（diff 只带 hash8）。
	audits, err := st.RecentAudits(ctx, 50)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	setAudit, rmAudit := false, false
	for _, a := range audits {
		switch a.Action {
		case "config.set":
			setAudit = true
			if strings.Contains(a.DiffSummary, contentV1) || strings.Contains(a.DiffSummary, contentV2) {
				t.Fatal("config.set audit carries content")
			}
			if !strings.Contains(a.Target, "config:cfgapp/app.yaml") {
				t.Fatalf("config.set audit target = %q, want config:<app>/<name>", a.Target)
			}
		case "config.removed":
			rmAudit = true
		}
	}
	if !setAudit || !rmAudit {
		t.Fatalf("audit actions missing (set=%v removed=%v)", setAudit, rmAudit)
	}
}

// TestConfigsNameAndQuotaValidation 守卫③：名称形状与值大小上限（与 secrets
// 同口径 64KiB）在 handler 层 4xx 点名（proto 形状层之外的兜底镜像）。
func TestConfigsNameAndQuotaValidation(t *testing.T) {
	st, cl, token := newConfigsTestEnv(t)
	ctx := authCtx(context.Background(), token)
	if _, err := testsupport.SeedAppE(t, st, "shapeapp"); err != nil {
		t.Fatalf("create app: %v", err)
	}

	for _, name := range []string{"", "a/b", "..", "-lead", ".dot", strings.Repeat("x", 64)} {
		if _, err := cl.SetConfig(ctx, &serverv1.SetConfigRequest{App: "shapeapp", Name: name, Value: "v"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("name %q accepted (code %v), want InvalidArgument", name, status.Code(err))
		}
	}
	// 合法形态：字母数字开头 + ._- 组合。
	if _, err := cl.SetConfig(ctx, &serverv1.SetConfigRequest{App: "shapeapp", Name: "a1-B.c", Value: "v"}); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}

	// 值配额：空值与 >64KiB 拒绝，错误文案点名上限（与 secrets 同口径）。
	if _, err := cl.SetConfig(ctx, &serverv1.SetConfigRequest{App: "shapeapp", Name: "empty", Value: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty value code = %v, want InvalidArgument", status.Code(err))
	}
	_, err := cl.SetConfig(ctx, &serverv1.SetConfigRequest{
		App: "shapeapp", Name: "big", Value: strings.Repeat("x", 65537),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized value code = %v, want InvalidArgument", status.Code(err))
	}
	if !strings.Contains(err.Error(), "65536") {
		t.Fatalf("oversized value rejection must name the limit: %v", err)
	}
}

// TestConfigsScopeMatrixMatchesSecrets 守卫⑤：权限矩阵与 secrets 同构——
// List=read（只出名称/指纹）、Get 明文=admin、Set/Remove=admin（票面默认
// 对齐 secrets；实施记录第 7 条载裁决理由）。并做一次真拦截链的读 token
// 负面调用（写面与明文读面均拒、list 放行）。
func TestConfigsScopeMatrixMatchesSecrets(t *testing.T) {
	pairs := []struct {
		configMethod string
		secretMethod string
		want         string
	}{
		{"SetConfig", "SetSecret", ScopeAdmin},
		{"ListConfigs", "ListSecrets", ScopeRead},
		{"GetConfig", "", ScopeAdmin}, // 明文回读面（secrets 无对偶 RPC——与 GetEnv 同门）
		{"RemoveConfig", "RemoveSecret", ScopeAdmin},
	}
	for _, tc := range pairs {
		got, ok := RequiredScope("/fleetly.server.v1.ConfigsService/" + tc.configMethod)
		if !ok {
			t.Fatalf("ConfigsService/%s not registered in the scope matrix", tc.configMethod)
		}
		if got != tc.want {
			t.Fatalf("ConfigsService/%s scope = %s, want %s", tc.configMethod, got, tc.want)
		}
		if tc.secretMethod == "" {
			continue
		}
		secretScope, ok := RequiredScope("/fleetly.server.v1.SecretsService/" + tc.secretMethod)
		if !ok || secretScope != got {
			t.Fatalf("scope drift vs secrets: ConfigsService/%s=%s, SecretsService/%s=%s",
				tc.configMethod, got, tc.secretMethod, secretScope)
		}
	}
}

// TestConfigsReadTokenRejectedOnWriteAndPlaintextRead 读 token 经真拦截链：
// set/remove/get 拒绝（admin 门），list 放行（read 门）——权限矩阵的运行
// 面证据（登记面断言见上一测试）。
func TestConfigsReadTokenRejectedOnWriteAndPlaintextRead(t *testing.T) {
	st, cl, adminToken := newConfigsTestEnv(t)
	adminCtx := authCtx(context.Background(), adminToken)
	if _, err := testsupport.SeedAppE(t, st, "gatedapp"); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := cl.SetConfig(adminCtx, &serverv1.SetConfigRequest{App: "gatedapp", Name: "app.yaml", Value: "v1"}); err != nil {
		t.Fatalf("admin SetConfig: %v", err)
	}

	readCtx := authCtx(context.Background(), seedTokenPlain(t, st, "read"))
	if _, err := cl.SetConfig(readCtx, &serverv1.SetConfigRequest{App: "gatedapp", Name: "app.yaml", Value: "v2"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token SetConfig code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := cl.GetConfig(readCtx, &serverv1.GetConfigRequest{App: "gatedapp", Name: "app.yaml"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token GetConfig code = %v, want PermissionDenied (plaintext read is admin)", status.Code(err))
	}
	if _, err := cl.RemoveConfig(readCtx, &serverv1.RemoveConfigRequest{App: "gatedapp", Name: "app.yaml"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token RemoveConfig code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := cl.ListConfigs(readCtx, &serverv1.ListConfigsRequest{App: "gatedapp"}); err != nil {
		t.Fatalf("read token ListConfigs: %v (list is read scope)", err)
	}
}
