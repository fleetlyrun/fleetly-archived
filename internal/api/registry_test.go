package api

// 平台 registry 凭证设置面集成测试（IMPL-T1-2/DT-2）：走与生产同构的鉴权
// 链（bufconn + seed token），断言 admin scope 与平台管理员双门、密码只写
// 不读（读面指纹）、envelope 密文落库、留空保留语义、清除语义、非法 host
// 400、事件/审计脱敏（凭据材料零出现）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// newRegistryTestEnv 起一个带鉴权链 + SystemService（注入 box）的 bufconn
// server，返回 client、store、admin/read token。
func newRegistryTestEnv(t *testing.T) (serverv1.SystemServiceClient, *state.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "registry.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "registry.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	admin := seedTokenPlain(t, st, "admin")
	read := seedTokenPlain(t, st, "read")

	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srv,
		NewSystemService("dev", st, nil, nil, nil).WithSecretsBox(box))
	conn := serveBufconn(t, srv)
	return serverv1.NewSystemServiceClient(conn), st, admin, read
}

// TestRegistrySettingsFace 设置面主链：scope 把门 → 保存（密码加密落库、
// 指纹读面）→ 留空保留语义 → 清除语义 → 事件脱敏。
func TestRegistrySettingsFace(t *testing.T) {
	cl, st, admin, read := newRegistryTestEnv(t)
	ctx := context.Background()
	const password = "ghp-REGISTRY-PASSWORD-MARKER" //nolint:gosec // G101：测试标记值，非真实凭据

	// scope 把门：read token → PermissionDenied（两面同门）。
	if _, err := cl.GetRegistrySettings(authCtx(ctx, read), &serverv1.GetRegistrySettingsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token on get = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := cl.UpdateRegistrySettings(authCtx(ctx, read),
		&serverv1.UpdateRegistrySettingsRequest{Host: "ghcr.io"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token on update = %v, want PermissionDenied", status.Code(err))
	}

	// 未配置缺省态：host/用户名/指纹全空。
	got, err := cl.GetRegistrySettings(authCtx(ctx, admin), &serverv1.GetRegistrySettingsRequest{})
	if err != nil {
		t.Fatalf("GetRegistrySettings (empty): %v", err)
	}
	if s := got.GetSettings(); s.GetHost() != "" || s.GetUsername() != "" || s.GetPasswordFingerprint() != "" {
		t.Fatalf("empty settings = %+v, want all empty", s)
	}

	// 保存（含密码）：读面指纹 = 明文 sha256 前 8；host 归一（scheme 剥离）。
	up, err := cl.UpdateRegistrySettings(authCtx(ctx, admin), &serverv1.UpdateRegistrySettingsRequest{
		Host: "https://GHCR.io/", Username: "robot", Password: password,
	})
	if err != nil {
		t.Fatalf("UpdateRegistrySettings: %v", err)
	}
	wantFingerprint := secretFingerprint([]byte(password))
	if s := up.GetSettings(); s.GetHost() != "ghcr.io" || s.GetUsername() != "robot" ||
		s.GetPasswordFingerprint() != wantFingerprint {
		t.Fatalf("saved settings = %+v, want host ghcr.io / fingerprint %s", s, wantFingerprint)
	}

	// 密码只写不读：读面指纹稳定，明文零出现；持久层是 envelope 密文。
	got, err = cl.GetRegistrySettings(authCtx(ctx, admin), &serverv1.GetRegistrySettingsRequest{})
	if err != nil {
		t.Fatalf("GetRegistrySettings: %v", err)
	}
	if got.GetSettings().GetPasswordFingerprint() != wantFingerprint {
		t.Fatalf("read-back fingerprint = %q, want %q", got.GetSettings().GetPasswordFingerprint(), wantFingerprint)
	}
	stored, err := st.LoadRegistrySettings(ctx)
	if err != nil {
		t.Fatalf("LoadRegistrySettings: %v", err)
	}
	if !strings.Contains(stored.PasswordCipher, "age-encryption.org") {
		t.Fatalf("stored password is not an age envelope: %q", stored.PasswordCipher)
	}

	// 留空保留语义：改用户名不重录密码——密文与指纹原样沿用。
	if _, err := cl.UpdateRegistrySettings(authCtx(ctx, admin), &serverv1.UpdateRegistrySettingsRequest{
		Host: "ghcr.io", Username: "robot2",
	}); err != nil {
		t.Fatalf("UpdateRegistrySettings (keep password): %v", err)
	}
	kept, err := st.LoadRegistrySettings(ctx)
	if err != nil {
		t.Fatalf("LoadRegistrySettings (kept): %v", err)
	}
	if kept.Username != "robot2" || kept.PasswordCipher != stored.PasswordCipher ||
		kept.PasswordFingerprint != wantFingerprint {
		t.Fatalf("kept settings = %+v, want username change with cipher/fingerprint kept", kept)
	}

	// 事件 registry.updated：只带 host 与指纹，密码明文/密文零出现。
	events, err := st.EventsSince(ctx, 0, 20)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	var eventSeen bool
	for _, ev := range events {
		if ev.Name != "registry.updated" {
			continue
		}
		eventSeen = true
		if strings.Contains(ev.Payload, password) || strings.Contains(ev.Payload, stored.PasswordCipher) {
			t.Fatalf("registry.updated payload leaks credentials: %s", ev.Payload)
		}
		if !strings.Contains(ev.Payload, "ghcr.io") || !strings.Contains(ev.Payload, wantFingerprint) {
			t.Fatalf("registry.updated payload should carry host and fingerprint: %s", ev.Payload)
		}
	}
	if !eventSeen {
		t.Fatal("event registry.updated missing")
	}

	// 非法 host 形态：400 点名（scheme 归一后残留路径段）。
	if _, err := cl.UpdateRegistrySettings(authCtx(ctx, admin),
		&serverv1.UpdateRegistrySettingsRequest{Host: "ghcr.io/owner"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("path host err = %v, want InvalidArgument", err)
	}

	// 清除语义：host 留空 = 四键齐清。
	if _, err := cl.UpdateRegistrySettings(authCtx(ctx, admin),
		&serverv1.UpdateRegistrySettingsRequest{}); err != nil {
		t.Fatalf("UpdateRegistrySettings (clear): %v", err)
	}
	cleared, err := st.LoadRegistrySettings(ctx)
	if err != nil {
		t.Fatalf("LoadRegistrySettings (cleared): %v", err)
	}
	if cleared.Host != "" || cleared.Username != "" || cleared.PasswordCipher != "" {
		t.Fatalf("cleared settings = %+v, want all four keys empty", cleared)
	}
}

// TestRegistrySettingsPlatformAdminGate 用户 principal 平台管理员门：
// 非平台管理员用户 PAT（显式 admin scope）403；平台管理员用户 PAT 200；
// 机具 admin 沿 scope 门放行。
func TestRegistrySettingsPlatformAdminGate(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "gate.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "gate.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	uRoot := mustUser(t, st, "root@example.test")
	uPlain := mustUser(t, st, "plain@example.test")
	tokRoot := seedUserPAT(t, st, uRoot.ID, ScopeAdmin)
	tokUser := seedUserPAT(t, st, uPlain.ID, ScopeAdmin)
	tokMachine := seedTokenPlain(t, st, ScopeAdmin)

	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srv, NewSystemService("dev", st, nil, nil, nil).WithSecretsBox(box))
	ctx := context.Background()

	// 非平台管理员用户：两 RPC 均 403（凭据声明 admin——门在平台面）。
	cl := serverv1.NewSystemServiceClient(serveBufconn(t, srv))
	if _, err := cl.GetRegistrySettings(authCtx(ctx, tokUser), &serverv1.GetRegistrySettingsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("user PAT get = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := cl.UpdateRegistrySettings(authCtx(ctx, tokUser), &serverv1.UpdateRegistrySettingsRequest{Host: "ghcr.io"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("user PAT update = %v, want PermissionDenied", status.Code(err))
	}

	// 平台管理员与机具令牌：写面 200。
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	if _, err := cl.UpdateRegistrySettings(authCtx(ctx, tokRoot), &serverv1.UpdateRegistrySettingsRequest{
		Host: "ghcr.io", Username: "admin-pull", Password: "pw-gate-only",
	}); err != nil {
		t.Fatalf("platform admin update: %v", err)
	}
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	if _, err := cl.UpdateRegistrySettings(authCtx(ctx, tokMachine), &serverv1.UpdateRegistrySettingsRequest{
		Host: "ghcr.io", Username: "machine-pull", Password: "pw-gate-only-2",
	}); err != nil {
		t.Fatalf("machine admin update: %v", err)
	}
}

// TestRegistrySettingsFingerprintMatchesSHA256 指纹口径锚：明文 sha256 前
// 8 字节（16 hex；与 S3/ACME 的 secretFingerprint 同一函数）。
func TestRegistrySettingsFingerprintMatchesSHA256(t *testing.T) {
	plain := []byte("pw-fingerprint-anchor")
	sum := sha256.Sum256(plain)
	want := hex.EncodeToString(sum[:8])
	if got := secretFingerprint(plain); got != want {
		t.Fatalf("secretFingerprint = %q, want %q", got, want)
	}
}
