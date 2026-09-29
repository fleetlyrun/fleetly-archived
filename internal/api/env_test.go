package api

// env 写路径联动测试（H9：env set/remove 成功后即时失效日志脱敏值集——
// 30s TTL 窗内新 secret 值会被明文采集并按天落盘保留 7 天，写点失效把
// 暴露窗收敛到单次重建）。服务直连单测（不经 gRPC）：回调注入与触发点位
// 是本测的对象，鉴权/传输链与其正交。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// TestEnvServiceInvalidationHook H9：SetEnv/RemoveEnv 写点成功后触发失效
// 回调（appID 载荷）；失败路径（删除不存在的键 → 404）不触发。
func TestEnvServiceInvalidationHook(t *testing.T) {
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
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, "hookapp")
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	var mu sync.Mutex
	var fired []string
	svc := NewEnvService(st, box).WithEnvChangedHook(func(appID string) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, appID)
	})

	if _, err := svc.SetEnv(directCtx(ctx), &serverv1.SetEnvRequest{App: "hookapp", Key: "API_KEY", Value: "v-1-secret-value"}); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	if _, err := svc.RemoveEnv(directCtx(ctx), &serverv1.RemoveEnvRequest{App: "hookapp", Key: "API_KEY"}); err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	mu.Lock()
	got := append([]string{}, fired...)
	mu.Unlock()
	if len(got) != 2 || got[0] != app.ID || got[1] != app.ID {
		t.Fatalf("invalidation calls = %v, want [%s %s]", got, app.ID, app.ID)
	}

	// 失败路径不触发：删除不存在的键 → 404，回调不调用。
	if _, err := svc.RemoveEnv(directCtx(ctx), &serverv1.RemoveEnvRequest{App: "hookapp", Key: "GONE"}); err == nil {
		t.Fatal("remove missing key should fail (404)")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 2 {
		t.Fatalf("failure path fired callback: %v", fired)
	}
}

// TestSetEnvAuditActorAttribution（W2-12，2026-09-26 走查）：用户会话写 env
// 的审计主体 = "user:<id>"——此前 state 层 actor 硬编码 "system"，用户动作
// 被记成系统动作（审计归因断裂）。机制守卫 = SetAppEnv 签名要求显式
// actor（空 actor 构造错误拒写），本测同时验证用户路径署名与空 actor 拒绝。
func TestSetEnvAuditActorAttribution(t *testing.T) {
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
	ctx := context.Background()
	proj := testsupport.SeedProject(t, st)
	// 首用户强制平台管理员（资源面只读）——先造引导用户占位，被测用户是
	// 普通成员（developer）。
	if _, err := st.CreateUser(ctx, state.UserWrite{
		Email: "bootstrap@fleetly.run", Password: "bootstrap-pass-1",
	}); err != nil {
		t.Fatalf("CreateUser bootstrap: %v", err)
	}
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	user, err := st.CreateUser(ctx, state.UserWrite{
		Email: "env-actor@fleetly.run", Password: "env-actor-pass-1",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, merr := st.AddMember(ctx, proj.TeamID, user.ID, "developer", "", ""); merr != nil {
		t.Fatalf("AddMember: %v", merr)
	}
	app := testsupport.SeedAppInProject(t, st, "actorapp", proj)

	svc := NewEnvService(st, box)
	// 机具令牌路径（UserID 空）：审计主体 = "human"（terminal 同约定）。
	if _, err := svc.SetEnv(directCtx(ctx), &serverv1.SetEnvRequest{App: "actorapp", Key: "MACHINE_KEY", Value: "v"}); err != nil {
		t.Fatalf("SetEnv machine: %v", err)
	}
	// 用户会话路径：审计主体 = "user:<id>"（直调 ctx 须同时注入拦截器态的
	// 方法所需 scope——deploy，与生产拦截链同语义）。
	userCtx := putRequiredScope(context.WithValue(ctx, principalKey{}, Principal{
		TokenID: "tok-1", Scopes: []string{ScopeDeploy}, UserID: user.ID,
	}), ScopeDeploy)
	if _, err := svc.SetEnv(userCtx, &serverv1.SetEnvRequest{App: "actorapp", Key: "USER_KEY", Value: "v"}); err != nil {
		t.Fatalf("SetEnv user: %v", err)
	}
	audits, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	actors := map[string]string{} // key → actor
	for _, a := range audits {
		if a.Action != "app.env_set" {
			continue
		}
		var d struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(a.DiffSummary), &d); err != nil {
			t.Fatalf("decode diff %s: %v", a.DiffSummary, err)
		}
		actors[d.Key] = a.Actor
	}
	if got := actors["USER_KEY"]; got != "user:"+user.ID {
		t.Fatalf("user env_set audit actor = %q, want %q", got, "user:"+user.ID)
	}
	if got := actors["MACHINE_KEY"]; got != "human" {
		t.Fatalf("machine env_set audit actor = %q, want human", got)
	}
	// 空 actor 在 state 层拒绝（签名约束：审计主体不可缺省）。
	if _, err := st.SetAppEnv(ctx, app.ID, "NO_ACTOR", "v", "platform", ""); err == nil {
		t.Fatal("empty audit actor must be rejected")
	}
}
