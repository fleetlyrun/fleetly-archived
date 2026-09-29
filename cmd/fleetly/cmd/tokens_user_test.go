package cmd

// tokens 命令的用户态验收测试（v0.3 W2-S2 语义迁移）：CLI 自服务造 PAT、
// 越权声明提示、--machine 平台管理员语义、列表可见性注记、自服务吊销。
// 服务面夹具 = startCLI 进程内真实服务；用户凭据经 apitest SeedUser /
// SeedUserToken（真用户 PAT），无队用户经 env.Store.CreateUser 直播。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// TestTokensCreateSelfService 用户 PAT 自服务造 PAT 全链（普通用户——
// 非平台管理员的第二注册用户）：create 返回明文（即签即用——新 PAT 的
// tokens list 可见自己）、列表只含自己的 PAT（不见机具令牌）、revoke 后
// 新 PAT 失效。
func TestTokensCreateSelfService(t *testing.T) {
	env := startCLI(t)
	// 首用户 = 平台管理员（夹具垫底）；mate = 第二注册用户（非管理员）。
	if err := env.Store.SaveRegistration(context.Background(), state.AuthRegistrationOpen, state.AuthSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("open registration: %v", err)
	}
	_ = env.SeedUser(t, "admin@example.com", "password-123")
	if err := env.Store.SaveRegistration(context.Background(), state.AuthRegistrationOpen, state.AuthSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("reopen registration: %v", err)
	}
	mate := env.SeedUser(t, "ada@example.com", "password-123")
	pat := env.SeedUserToken(t, mate.User.ID)
	t.Setenv("FLEETLY_TOKEN", pat)

	// 自服务 create（read）：明文仅此一次。
	code, out, errOut := runCLIConn(t, "tokens", "create", "--json", "--scopes", "read", "--note", "laptop")
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%s", code, errOut)
	}
	var created struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Token == "" {
		t.Fatalf("create resp: %s err=%v", out, err)
	}
	if !strings.Contains(out, `"scopes": ["read"]`) && !strings.Contains(out, "read") {
		t.Fatalf("create resp missing scopes: %s", out)
	}

	// 新 PAT 即刻可用：list 只见自己的两枚（机具令牌零出现）。
	code, out, _ = runCLIConn(t, "tokens", "list", "--json")
	if code != 0 {
		t.Fatalf("list: code=%d", code)
	}
	var listed struct {
		Tokens []struct {
			ID     string `json:"id"`
			UserID string `json:"user_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("list resp: %s err=%v", out, err)
	}
	if len(listed.Tokens) != 2 {
		t.Fatalf("self list = %d rows, want 2 (both ada PATs): %s", len(listed.Tokens), out)
	}
	for _, tok := range listed.Tokens {
		if tok.UserID != mate.User.ID {
			t.Fatalf("self list leaked a foreign/machine row: %s", out)
		}
	}

	// 人读形态：user=<id> 注记（属主维度）。
	code, out, _ = runCLIConn(t, "tokens", "list")
	if code != 0 || !strings.Contains(out, "user="+mate.User.ID) {
		t.Fatalf("plain list missing user annotation: code=%d out=%s", code, out)
	}

	// 自服务 revoke → 新 PAT 失效（401 + 指引）。
	t.Setenv("FLEETLY_TOKEN", created.Token)
	code, out, _ = runCLIConn(t, "tokens", "revoke", "--json", created.ID)
	if code != 0 {
		t.Fatalf("revoke: code=%d out=%s", code, out)
	}
	code, _, errOut = runCLIConn(t, "tokens", "list")
	if code != 1 || !strings.Contains(errOut, "invalid or revoked token") {
		t.Fatalf("revoked PAT still works: code=%d stderr=%s", code, errOut)
	}
}

// TestTokensCreateScopeGuardDecline 越权声明提示（表驱动）：无队用户
// （CreateUser 通道，可达集 = {read}）声明 admin/terminal → 400 信封带
// 「exceeds your granted capabilities」可行动指引，退出 1。
func TestTokensCreateScopeGuardDecline(t *testing.T) {
	env := startCLI(t)
	founder := env.SeedUser(t, "admin@example.com", "password-123")
	worker, err := env.Store.CreateUser(context.Background(), state.UserWrite{ //nolint:gosec // G101：测试夹具口令，非真实凭据
		Email: "worker@example.com", Password: "temp-pw-worker-123", ActorUserID: founder.User.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pat := env.SeedUserToken(t, worker.ID)
	t.Setenv("FLEETLY_TOKEN", pat)

	for _, tc := range []struct{ scopes string }{{"admin"}, {"terminal"}, {"deploy"}} {
		code, _, errOut := runCLIConn(t, "tokens", "create", "--json", "--scopes", tc.scopes)
		if code != 1 || !strings.Contains(errOut, "PermissionDenied") && !strings.Contains(errOut, "exceeds your granted capabilities") {
			t.Fatalf("scopes %s: code=%d stderr=%s (want 400 guidance)", tc.scopes, code, errOut)
		}
		if !strings.Contains(errOut, "exceeds your granted capabilities") {
			t.Fatalf("scopes %s missing guidance: %s", tc.scopes, errOut)
		}
	}
}

// TestTokensCreateMachineFlag --machine 旗标（平台级机具令牌）：平台管理
// 员用户 OK（列表 machine 注记）；非管理员注册用户 403。
func TestTokensCreateMachineFlag(t *testing.T) {
	env := startCLI(t)
	// 首用户 = 平台管理员。
	admin := env.SeedUser(t, "admin@example.com", "password-123")
	adminPat := env.SeedUserToken(t, admin.User.ID)
	t.Setenv("FLEETLY_TOKEN", adminPat)

	code, out, errOut := runCLIConn(t, "tokens", "create", "--json", "--scopes", "read", "--machine", "--note", "ci")
	if code != 0 {
		t.Fatalf("platform admin --machine: code=%d stderr=%s", code, errOut)
	}
	var created struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Token == "" {
		t.Fatalf("create resp: %s err=%v", out, err)
	}
	// 平台管理员全列：新行 machine 注记（user_id 缺省）。
	code, out, _ = runCLIConn(t, "tokens", "list", "--json")
	if code != 0 || !strings.Contains(out, `"ci"`) {
		t.Fatalf("admin list: code=%d out=%s", code, out)
	}

	// 非管理员注册用户（第二用户 = 个人队 owner，非平台管理员）→ 403。
	if err := env.Store.SaveRegistration(context.Background(), state.AuthRegistrationOpen, state.AuthSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("open registration: %v", err)
	}
	mate := env.SeedUser(t, "mate@example.com", "password-123")
	matePat := env.SeedUserToken(t, mate.User.ID)
	t.Setenv("FLEETLY_TOKEN", matePat)
	code, _, errOut = runCLIConn(t, "tokens", "create", "--json", "--scopes", "read", "--machine")
	if code != 1 || !strings.Contains(errOut, "only platform admins can create machine tokens") {
		t.Fatalf("non-admin --machine: code=%d stderr=%s", code, errOut)
	}
	// 但自服务 PAT 照常（个人队 owner 可达全集）。
	code, _, _ = runCLIConn(t, "tokens", "create", "--json", "--scopes", "admin")
	if code != 0 {
		t.Fatalf("self-service admin PAT (personal team owner): got code=%d", code)
	}
}
