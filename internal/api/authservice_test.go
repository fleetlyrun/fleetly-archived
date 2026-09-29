package api

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 认证/用户面单测（v0.3 W1，rbac-teams 设计 §2/§5/§10 验收）：
//   - 首用户注册：平台管理员投影 + 会话 cookie 下发（响应 header metadata
//     set-cookie）+ state 全量落位 + 窗口规则（缺省 closed →
//     E_REGISTRATION_CLOSED）；
//   - 登录/注销/全部注销/Me 的 RPC 面 + 会话 cookie 认证分支（拦截器级；
//     gateway 形态端到端在 internal/runtime gateway_rest_test.go）；
//   - UsersService 平台面双门 + 临时口令一次性 + 重置即全端下线；
//   - 注册/登录 email+IP 双键限流；
//   - scope 登记完整性（fail-closed 兜底面）。

// authEnv 是认证面测试环境（独立 store——注册窗口从零用户开始）。
type authEnv struct {
	st    *state.Store
	conn  *grpc.ClientConn
	auth  *AuthService
	users *UsersService
	authn *Authenticator
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env := &authEnv{st: st}
	env.authn = NewAuthenticator(st)
	env.auth = NewAuthService(st)
	env.users = NewUsersService(st)

	srv := newAuthServer(env.authn)
	serverv1.RegisterAuthServiceServer(srv, env.auth)
	serverv1.RegisterUsersServiceServer(srv, env.users)
	env.conn = serveBufconn(t, srv)
	return env
}

// ctxIP 带固定来源 IP 的 ctx（限流双键的 IP 维度可预测）。bufconn 形态
// 的 gRPC 对端是 "bufnet"（非环回）——W3-S4 起 clientIPFromContext 只在
// 环回对端（gateway 回拨形态）采信 x-forwarded-for metadata，bufconn 上
// 的 XFF 不参与限流键；需要精确控制 IP 键的测试用 peerCtx 直调 handler。
func ctxIP(ip string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-forwarded-for", ip+":1234")
}

// peerCtx 构造「对端 + XFF metadata」齐备的直调 handler ctx：loopp 为真
// 时对端是环回（gateway 回拨形态——XFF 被采信），否则是远程直连形态
//（XFF 视为客户端可控、不采信）。
func peerCtx(loop bool, peerIP, xff string) context.Context {
	ipp := "127.0.0.1"
	if !loop {
		ipp = peerIP
	}
	base := context.Background()
	if xff != "" {
		base = metadata.NewIncomingContext(base, metadata.Pairs("x-forwarded-for", xff))
	}
	return peer.NewContext(base, &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(ipp), Port: 43210}})
}

// register 注册并返回（用户投影, 会话 cookie 值）。捕获响应 header metadata
// ——Set-Cookie 透传形态的 gRPC 侧原貌。
func (e *authEnv) register(t *testing.T, ip, email, password string) (*serverv1.UserView, string) {
	t.Helper()
	var hdr metadata.MD
	res, err := serverv1.NewAuthServiceClient(e.conn).Register(ctxIP(ip),
		&serverv1.RegisterRequest{Email: email, Password: password}, grpc.Header(&hdr))
	if err != nil {
		t.Fatalf("Register(%s): %v", email, err)
	}
	cookies := hdr.Get("set-cookie")
	if len(cookies) == 0 {
		t.Fatal("Register must carry set-cookie response header metadata")
	}
	value, ok := strings.CutPrefix(cookies[0], sessionCookieName+"=")
	if !ok {
		t.Fatalf("set-cookie %q must target %s", cookies[0], sessionCookieName)
	}
	return res.GetUser(), strings.Split(value, ";")[0]
}

// login 登录并返回（用户投影, 会话值, 原始 error 供断言）。
func (e *authEnv) login(t *testing.T, ip, email, password string) (*serverv1.UserView, string, error) {
	t.Helper()
	var hdr metadata.MD
	res, err := serverv1.NewAuthServiceClient(e.conn).Login(ctxIP(ip),
		&serverv1.LoginRequest{Email: email, Password: password}, grpc.Header(&hdr))
	cookie := ""
	if vals := hdr.Get("set-cookie"); len(vals) > 0 {
		if value, ok := strings.CutPrefix(vals[0], sessionCookieName+"="); ok {
			cookie = strings.Split(value, ";")[0]
		}
	}
	if err != nil {
		return nil, "", err
	}
	return res.GetUser(), cookie, nil
}

// cookieCtx 构造携带会话 cookie 的直连 ctx（gateway 透传形态 grpcgateway-
// cookie 优先；"cookie" 键兜底——两形态都被认证分支消费）。
func cookieCtx(cookie string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "grpcgateway-cookie", sessionCookieName+"="+cookie)
}

// TestAuthRegisterAndWindowRules：首用户注册全量落位（平台管理员 + 会话
// 下发 + 个人队投影）+ 窗口规则（缺省 closed / open 放行 / 关闭稳定码）。
func TestAuthRegisterAndWindowRules(t *testing.T) {
	env := newAuthEnv(t)
	client := serverv1.NewAuthServiceClient(env.conn)

	root, rootCookie := env.register(t, "203.0.113.7", "Root@Example.COM", "pw-root-123")
	if !root.GetIsPlatformAdmin() || root.GetEmail() != "root@example.com" {
		t.Fatalf("first user = %+v, want platform admin with normalized email", root)
	}
	if len(rootCookie) < 32 {
		t.Fatalf("session cookie value too short: %q", rootCookie)
	}
	// 会话凭据立即生效：Me 返回本人 + 个人队 owner 投影。
	me, err := serverv1.NewAuthServiceClient(env.conn).Me(cookieCtx(rootCookie), &serverv1.MeRequest{})
	if err != nil {
		t.Fatalf("Me with session cookie: %v", err)
	}
	if me.GetUser().GetId() != root.GetId() || len(me.GetTeams()) != 1 ||
		me.GetTeams()[0].GetTeamSlug() != "root" || me.GetTeams()[0].GetRole() != "owner" {
		t.Fatalf("Me = %+v, want own user + personal team owner projection", me)
	}

	// GetRegistrationState：无用户窗口已过 → 缺省 closed。
	rs, err := client.GetRegistrationState(ctxIP("203.0.113.7"), &serverv1.GetRegistrationStateRequest{})
	if err != nil {
		t.Fatalf("GetRegistrationState: %v", err)
	}
	if !rs.GetHasUsers() || rs.GetOpen() {
		t.Fatalf("registration state = %+v, want has_users=true open=false (default closed)", rs)
	}

	// 第二用户注册 → E_REGISTRATION_CLOSED（403 + 稳定码信封 detail）。
	_, err = client.Register(ctxIP("198.51.100.1"), &serverv1.RegisterRequest{Email: "second@example.com", Password: "pw-second-9"})
	stt := status.Convert(err)
	if stt.Code() != codes.PermissionDenied || !strings.Contains(stt.Message(), "registration is closed") {
		t.Fatalf("second register err = %v (%s), want PermissionDenied/registration closed", err, stt.Message())
	}
	var found bool
	for _, d := range stt.Details() {
		if msg, ok := d.(interface{ GetCode() string }); ok && msg.GetCode() == "E_REGISTRATION_CLOSED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("envelope must carry E_REGISTRATION_CLOSED detail: %v", stt.Details())
	}

	// 平台管理员切 open → 第二用户注册成功（非管理员 + 个人队 R3 落位）。
	if err := env.st.SaveRegistration(context.Background(), "open", state.AuthSaveOptions{Actor: "user:x"}); err != nil {
		t.Fatalf("open the window: %v", err)
	}
	second, _ := env.register(t, "198.51.100.1", "second@example.com", "pw-second-9")
	if second.GetIsPlatformAdmin() {
		t.Fatal("second registered user must not be platform admin")
	}
	// email 冲突 → 409 退化信封（conflict）。
	_, err = client.Register(ctxIP("198.51.100.1"), &serverv1.RegisterRequest{Email: "SECOND@example.com", Password: "pw-second-9"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("duplicate email err = %v, want FailedPrecondition (409 conflict envelope)", err)
	}

	// 管理员关窗 → 再拒。
	if err := env.st.SaveRegistration(context.Background(), "closed", state.AuthSaveOptions{Actor: "user:x"}); err != nil {
		t.Fatalf("close the window: %v", err)
	}
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	if _, err := client.Register(ctxIP("198.51.100.2"), &serverv1.RegisterRequest{Email: "third@example.com", Password: "pw-third-88"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("closed-window registration err = %v, want PermissionDenied", err)
	}
}

// TestAuthLoginLogoutFlow：登录（成功/错口令/禁用账号）+ 注销后旧会话失效
// + 全部注销 + 登录失败审计（result=error，不落口令）。
func TestAuthLoginLogoutFlow(t *testing.T) {
	env := newAuthEnv(t)
	client := serverv1.NewAuthServiceClient(env.conn)
	env.register(t, "203.0.113.7", "dev@example.com", "pw-dev-1234")

	// 登录成功：cookie 下发 + Me 可用。
	u, cookie, err := env.login(t, "203.0.113.7", "DEV@example.com", "pw-dev-1234")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if u.GetEmail() != "dev@example.com" || cookie == "" {
		t.Fatalf("login = %+v cookie=%q", u, cookie)
	}
	if _, err := serverv1.NewAuthServiceClient(env.conn).Me(cookieCtx(cookie), &serverv1.MeRequest{}); err != nil {
		t.Fatalf("Me after login: %v", err)
	}

	// 错口令 → 401（不泄漏存在性文案）；登录失败审计落档。
	if _, _, err := env.login(t, "203.0.113.7", "dev@example.com", "wrong-password"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong password err = %v, want Unauthenticated", err)
	}
	audits, aerr := env.st.RecentAudits(context.Background(), 5)
	if aerr != nil {
		t.Fatalf("RecentAudits: %v", aerr)
	}
	if len(audits) == 0 || audits[0].Action != "auth.login_failed" || audits[0].Result != "error" {
		t.Fatalf("top audit = %+v, want auth.login_failed result=error", audits[0])
	}
	if strings.Contains(audits[0].DiffSummary, "wrong-password") {
		t.Fatal("failed-login audit must never carry the password")
	}

	// 注销：会话删行 + cookie 清除；旧 cookie 立即失效。
	if _, err := client.Logout(cookieCtx(cookie), &serverv1.LogoutRequest{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := serverv1.NewAuthServiceClient(env.conn).Me(cookieCtx(cookie), &serverv1.MeRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Me after logout err = %v, want Unauthenticated", err)
	}
	// Bearer 凭据（无会话）调 Logout → 400。
	tok := seedTokenPlain(t, env.st, "admin")
	if _, err := client.Logout(authCtx(context.Background(), tok), &serverv1.LogoutRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Logout with bearer err = %v, want InvalidArgument", err)
	}

	// 全部注销：两台设备（两个会话）一次清空。
	_, c1, err := env.login(t, "203.0.113.7", "dev@example.com", "pw-dev-1234")
	if err != nil {
		t.Fatalf("re-login 1: %v", err)
	}
	_, c2, err := env.login(t, "203.0.113.8", "dev@example.com", "pw-dev-1234")
	if err != nil {
		t.Fatalf("re-login 2: %v", err)
	}
	lar, err := client.LogoutAll(cookieCtx(c1), &serverv1.LogoutAllRequest{})
	if err != nil {
		t.Fatalf("LogoutAll: %v", err)
	}
	if lar.GetSessionsRevoked() != 3 {
		// 注册会话 + 两次登录会话 = 3（注册同样下发会话）。
		t.Fatalf("sessions_revoked = %d, want 3 (register session + two login sessions)", lar.GetSessionsRevoked())
	}
	for _, c := range []string{c1, c2} {
		if _, err := serverv1.NewAuthServiceClient(env.conn).Me(cookieCtx(c), &serverv1.MeRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("session %q must be dead after logout-all", c[:12])
		}
	}
}

// TestSessionCookieAuthenticationBranch：拦截器 cookie 分支细节——两形态
// metadata 键均消费、坏 cookie 401、Bearer 存在时优先于 cookie、过期会话
// 拒认、禁用属主会话拒认（联动面）、cookie 解析器。
func TestSessionCookieAuthenticationBranch(t *testing.T) {
	env := newAuthEnv(t)
	auth := env.authn
	ctx := context.Background()

	u, err := env.st.CreateUser(ctx, state.UserWrite{Email: "sess@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, plaintext, err := env.st.CreateSession(ctx, u.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// gateway 透传形态与直连兜底形态都进会话分支（直连形态 =
	// NewIncomingContext——authenticateAndAuthorize 读入向 metadata；
	// RPC 形态经 bufconn 的出→入向转换已由 Me 断言覆盖）。
	for _, key := range []string{"grpcgateway-cookie", "cookie"} {
		c := metadata.NewIncomingContext(ctx, metadata.Pairs(key, sessionCookieName+"="+plaintext))
		p, err := auth.authenticateAndAuthorize(c, "/fleetly.server.v1.AuthService/Me", ScopeRead)
		if err != nil {
			t.Fatalf("cookie via %s: %v", key, err)
		}
		if p.SessionID == "" || p.UserID != u.ID {
			t.Fatalf("principal via %s = %+v, want session identity", key, p)
		}
		// 会话 scope = 角色可达集（W2-S4 硬收缩后的口径）。本用户是库内首
		// 用户（state.CreateUser 首用户强制平台管理员）→ 可达集 = 全集。
		if err := auth.authorize(p, ScopeAdmin); err != nil {
			t.Fatalf("platform admin session scopes must cover admin: %v", err)
		}
	}
	// 非管理员、无成员关系用户的会话 = {read} 最小集（W2-S4 硬收缩——admin
	// 与 deploy 均不可达；资源面真授权在角色门逐请求解析，rbac-teams §4.2）。
	plain, err := env.st.CreateUser(ctx, state.UserWrite{Email: "plain@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser(plain): %v", err)
	}
	_, plainSess, err := env.st.CreateSession(ctx, plain.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession(plain): %v", err)
	}
	pPlain, err := auth.AuthenticateSessionCookie(ctx, sessionCookieName+"="+plainSess)
	if err != nil {
		t.Fatalf("AuthenticateSessionCookie(plain): %v", err)
	}
	if err := auth.authorize(pPlain, ScopeAdmin); err == nil {
		t.Fatalf("membership-less non-admin session must NOT cover admin (W2-S4 hard shrink)")
	}
	if err := auth.authorize(pPlain, ScopeDeploy); err == nil {
		t.Fatalf("membership-less non-admin session must NOT cover deploy (W2-S4 hard shrink)")
	}
	if err := auth.authorize(pPlain, ScopeRead); err != nil {
		t.Fatalf("session scopes must cover read (minimal set): %v", err)
	}

	// Bearer 优先：带合法 Bearer + 垃圾 cookie 的请求走 token 分支。
	tok := seedTokenPlain(t, env.st, "read")
	c := metadata.NewIncomingContext(ctx, metadata.Pairs(
		"authorization", "Bearer "+tok,
		"grpcgateway-cookie", sessionCookieName+"=garbage"))
	if _, err := auth.authenticateAndAuthorize(c, "/fleetly.server.v1.AuthService/Me", ScopeRead); err != nil {
		t.Fatalf("bearer precedence: %v", err)
	}
	// 只有垃圾 cookie → 401。
	c = metadata.NewIncomingContext(ctx, metadata.Pairs("grpcgateway-cookie", sessionCookieName+"=garbage"))
	if _, err := auth.authenticateAndAuthorize(c, "/fleetly.server.v1.AuthService/Me", ScopeRead); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("garbage cookie err = %v, want Unauthenticated", err)
	}

	// 会话 Principal 形态：TokenID 恒空（审计 actor 走 user:<id> 维）。
	p, err := auth.AuthenticateSessionCookie(ctx, sessionCookieName+"="+plaintext)
	if err != nil || p.TokenID != "" || p.SessionID == "" {
		t.Fatalf("AuthenticateSessionCookie = %+v err=%v", p, err)
	}

	// 禁用属主：会话联动删行（state.DisableUser 同事务）→ 401。
	if err := env.st.DisableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if _, err := auth.AuthenticateSessionCookie(ctx, sessionCookieName+"="+plaintext); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("disabled owner session err = %v, want Unauthenticated", err)
	}

	// cookie 解析器：多 cookie、空格、引号包裹、缺失目标名。
	if got := sessionCookieValue("other=1; " + sessionCookieName + "=\"abc\"; x=2"); got != "abc" {
		t.Fatalf("quoted cookie value = %q, want abc", got)
	}
	if got := sessionCookieValue("a=1; b=2"); got != "" {
		t.Fatalf("missing-target cookie = %q, want empty", got)
	}
}

// TestUsersServicePlatformGate：平台面双门——普通用户 PAT/会话 403、平台
// 管理员会话放行、机具令牌（user NULL + admin scope）沿 scope 门放行、
// 未登记 fail-closed 兜底不适用（全方法已登记 admin）。
func TestUsersServicePlatformGate(t *testing.T) {
	env := newAuthEnv(t)
	users := serverv1.NewUsersServiceClient(env.conn)

	root, rootCookie := env.register(t, "203.0.113.7", "root@example.com", "pw-root-123")
	// 平台管理员建第二用户（一次性临时口令）。
	cr, err := users.CreateUser(cookieCtx(rootCookie), &serverv1.CreateUserRequest{Email: "mate@example.com", DisplayName: "Mate"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tempPassword := cr.GetTemporaryPassword()
	if len(tempPassword) < 16 {
		t.Fatalf("temporary password too short: %q", tempPassword)
	}
	// 临时口令立即可登录。
	mate, mateCookie, err := env.login(t, "203.0.113.9", "mate@example.com", tempPassword)
	if err != nil {
		t.Fatalf("login with temporary password: %v", err)
	}
	if mate.GetId() != cr.GetUser().GetId() {
		t.Fatalf("login user id = %s, want %s", mate.GetId(), cr.GetUser().GetId())
	}

	// 非平台管理员的用户 PAT → 403（平台面门）。
	patTok := ""
	{
		ctx := context.Background()
		plaintext, gerr := generateToken()
		if gerr != nil {
			t.Fatalf("generate pat: %v", gerr)
		}
		if _, err := env.st.CreateToken(ctx, state.TokenWrite{
			Hash: state.HashToken(plaintext), Name: "mate pat", Scopes: "admin", UserID: mate.GetId(),
		}); err != nil {
			t.Fatalf("seed pat: %v", err)
		}
		patTok = plaintext
	}
	if _, err := users.ListUsers(authCtx(context.Background(), patTok), &serverv1.ListUsersRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin PAT ListUsers err = %v, want PermissionDenied", err)
	}
	// 非平台管理员的会话 → 403。
	if _, err := users.ListUsers(cookieCtx(mateCookie), &serverv1.ListUsersRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin session ListUsers err = %v, want PermissionDenied", err)
	}
	// 平台管理员会话 → 200。
	lr, err := users.ListUsers(cookieCtx(rootCookie), &serverv1.ListUsersRequest{})
	if err != nil {
		t.Fatalf("admin session ListUsers: %v", err)
	}
	if len(lr.GetUsers()) != 2 {
		t.Fatalf("users = %d, want 2", len(lr.GetUsers()))
	}
	// 机具令牌（user NULL + admin scope）沿 scope 门放行。
	machine := seedTokenPlain(t, env.st, "admin")
	if _, err := users.ListUsers(authCtx(context.Background(), machine), &serverv1.ListUsersRequest{}); err != nil {
		t.Fatalf("machine token ListUsers: %v", err)
	}
	// read-scope 机具令牌 → scope 门 403。
	readTok := seedTokenPlain(t, env.st, "read")
	if _, err := users.ListUsers(authCtx(context.Background(), readTok), &serverv1.ListUsersRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read machine token ListUsers err = %v, want PermissionDenied", err)
	}
	_ = root
}

// TestUsersServiceLifecycleAndReset：禁用→登录 401、解禁恢复、口令重置
// 一次性临时口令 + 全端下线、平台管理员授予/撤销。
func TestUsersServiceLifecycleAndReset(t *testing.T) {
	env := newAuthEnv(t)
	users := serverv1.NewUsersServiceClient(env.conn)
	root, rootCookie := env.register(t, "203.0.113.7", "root@example.com", "pw-root-123")
	cr, err := users.CreateUser(cookieCtx(rootCookie), &serverv1.CreateUserRequest{Email: "op@example.com"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	target := cr.GetUser().GetId()

	// 目标用户登录两台设备。
	_, c1, err := env.login(t, "203.0.113.9", "op@example.com", cr.GetTemporaryPassword())
	if err != nil {
		t.Fatalf("login 1: %v", err)
	}
	if _, _, err := env.login(t, "203.0.113.9", "op@example.com", cr.GetTemporaryPassword()); err != nil {
		t.Fatalf("login 2: %v", err)
	}

	// 重置口令：临时口令一次性返回 + 该用户全部会话同事务吊销（全端下线）。
	rr, err := users.ResetUserPassword(cookieCtx(rootCookie), &serverv1.ResetUserPasswordRequest{Id: target})
	if err != nil {
		t.Fatalf("ResetUserPassword: %v", err)
	}
	if rr.GetTemporaryPassword() == "" || rr.GetTemporaryPassword() == cr.GetTemporaryPassword() {
		t.Fatalf("reset password = %q, want a fresh one-time secret", rr.GetTemporaryPassword())
	}
	if _, err := serverv1.NewAuthServiceClient(env.conn).Me(cookieCtx(c1), &serverv1.MeRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("session after password reset err = %v, want Unauthenticated (reset = logout everywhere)", err)
	}
	// 旧口令失效、新临时口令可用。
	if _, _, err := env.login(t, "203.0.113.9", "op@example.com", cr.GetTemporaryPassword()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("old temporary password must fail after reset: %v", err)
	}
	if _, _, err := env.login(t, "203.0.113.9", "op@example.com", rr.GetTemporaryPassword()); err != nil {
		t.Fatalf("new temporary password login: %v", err)
	}

	// 禁用 → 登录 401 + PAT 拒认 + 会话联动清空；解禁恢复登录。
	dr, err := users.DisableUser(cookieCtx(rootCookie), &serverv1.DisableUserRequest{Id: target})
	if err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if dr.GetUser().GetDisabledAt() == nil {
		t.Fatalf("DisableUser projection = %+v, want disabled_at set", dr.GetUser())
	}
	if _, _, err := env.login(t, "203.0.113.9", "op@example.com", rr.GetTemporaryPassword()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("disabled login err = %v, want Unauthenticated", err)
	}
	if _, err := users.EnableUser(cookieCtx(rootCookie), &serverv1.EnableUserRequest{Id: target}); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}
	if _, _, err := env.login(t, "203.0.113.9", "op@example.com", rr.GetTemporaryPassword()); err != nil {
		t.Fatalf("enabled login: %v", err)
	}

	// 平台管理员授予/撤销（幂等面在 state 层单测覆盖；这里验 RPC 面 +
	// 404 投影）。
	if _, err := users.GrantPlatformAdmin(cookieCtx(rootCookie), &serverv1.GrantPlatformAdminRequest{Id: target}); err != nil {
		t.Fatalf("GrantPlatformAdmin: %v", err)
	}
	if _, err := users.ListUsers(cookieCtx(c1), &serverv1.ListUsersRequest{}); err != nil {
		// c1 已被重置吊销——这里应 401；确认后用新会话走撤销断言。
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("stale session must be 401, got %v", err)
		}
	}
	_, freshCookie, err := env.login(t, "203.0.113.9", "op@example.com", rr.GetTemporaryPassword())
	if err != nil {
		t.Fatalf("fresh login: %v", err)
	}
	if _, err := users.ListUsers(cookieCtx(freshCookie), &serverv1.ListUsersRequest{}); err != nil {
		t.Fatalf("granted admin ListUsers: %v", err)
	}
	if _, err := users.RevokePlatformAdmin(cookieCtx(rootCookie), &serverv1.RevokePlatformAdminRequest{Id: target}); err != nil {
		t.Fatalf("RevokePlatformAdmin: %v", err)
	}
	if _, err := users.ListUsers(cookieCtx(freshCookie), &serverv1.ListUsersRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked admin ListUsers err = %v, want PermissionDenied", err)
	}
	// 不存在用户 → 404。
	if _, err := users.DisableUser(cookieCtx(rootCookie), &serverv1.DisableUserRequest{Id: "01MISSING000000000000000000"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing user DisableUser err = %v, want NotFound", err)
	}
	_ = root
}

// TestAuthRateLimit（设计 §2.1：10 次/分钟，429 退化信封）：email 单键 10
// 次耗尽后第 11 次 429；换 email 但同 IP 也 429（IP 键独立计数已耗尽）。
func TestAuthRateLimit(t *testing.T) {
	env := newAuthEnv(t)
	// 假时钟注入限流器（窗口不推进——持续耗尽）。
	clk := &fakeClock{cur: time.Now()}
	env.auth.authLimiter.now = clk.Now

	// 无此 email 的登录：哑哈希路径时序成立、恒 401，直到限流 429。
	for i := 0; i < 10; i++ {
		if _, _, err := env.login(t, "203.0.113.7", "ghost@example.com", "whatever-pw"); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("attempt %d err = %v, want Unauthenticated before rate limit", i+1, err)
		}
	}
	// email 键已耗尽 → 429。
	if _, _, err := env.login(t, "203.0.113.7", "ghost@example.com", "whatever-pw"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("11th attempt err = %v, want ResourceExhausted (429)", err)
	}
	// 换 email：IP 键同样已耗尽 → 429（双键语义：任一耗尽即拒）。
	if _, _, err := env.login(t, "203.0.113.7", "other@example.com", "whatever-pw"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("other email same IP err = %v, want ResourceExhausted", err)
	}
	// 换 IP：email 键是独立桶（other@ 首次消耗）→ 401 正常路径。IP 维度
	// 经 peerCtx 直调 handler 控制（W3-S4 起 XFF 只在环回对端——gateway
	// 回拨形态——被采信；环回 + XFF 即 gateway 清洗后的分桶形态）。
	if _, err := env.auth.Login(peerCtx(true, "", "203.0.113.8:1234"),
		&serverv1.LoginRequest{Email: "other@example.com", Password: "whatever-pw"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("other IP err = %v, want Unauthenticated (fresh IP bucket)", err)
	}
}

// TestClientIPFromContextTrustSemantics（v0.3 W3-S4，W1-S2 XFF 伪造面收口
// 的单点语义钉死）：环回对端（gateway 回拨形态）采信清洗后的
// x-forwarded-for metadata；远程直连对端的 XFF metadata 客户端可控不采信
// ——IP 键回落真实对端；均不可得 "unknown"。
func TestClientIPFromContextTrustSemantics(t *testing.T) {
	// 环回对端 + XFF（gateway 清洗形态 = 单一真实 HTTP 对端值）→ 采信。
	if got := clientIPFromContext(peerCtx(true, "", "203.0.113.7:1234")); got != "203.0.113.7" {
		t.Fatalf("loopback peer + XFF = %q, want 203.0.113.7", got)
	}
	// 环回对端、无 XFF → 对端地址本身。
	if got := clientIPFromContext(peerCtx(true, "", "")); got != "127.0.0.1" {
		t.Fatalf("loopback peer without XFF = %q, want 127.0.0.1", got)
	}
	// 远程直连对端 + 伪造 XFF → 不采信，用真实对端（不可伪造断言）。
	if got := clientIPFromContext(peerCtx(false, "198.51.100.9", "9.9.9.9:1234")); got != "198.51.100.9" {
		t.Fatalf("remote peer + forged XFF = %q, want peer 198.51.100.9 (forgery must not win)", got)
	}
	// 远程直连对端、无 XFF → 对端地址。
	if got := clientIPFromContext(peerCtx(false, "198.51.100.9", "")); got != "198.51.100.9" {
		t.Fatalf("remote peer without XFF = %q, want 198.51.100.9", got)
	}
	// 无对端（进程内直调未注入 peer）→ "unknown" 占位。
	if got := clientIPFromContext(ctxIP("203.0.113.7")); got != "unknown" {
		t.Fatalf("no-peer ctx = %q, want unknown", got)
	}
}

// TestAuthRateLimitIPKeyUnforgeable（W3-S4 api 侧限流键不可伪造断言）：
// 远程直连对端每次换一枚伪造 XFF metadata 也无法稀释 IP 桶——10 次（每次
// 换 email 规避 email 键）后第 11 次 429。对照（同函数语义单点）：环回
// 对端 + 不同 XFF = gateway 清洗后按真实 HTTP 对端分桶，桶各自独立。
func TestAuthRateLimitIPKeyUnforgeable(t *testing.T) {
	env := newAuthEnv(t)
	clk := &fakeClock{cur: time.Now()}
	env.auth.authLimiter.now = clk.Now

	// 远程对端 198.51.100.9：10 次登录，email 与伪造 XFF 逐次全新。
	for i := 0; i < 10; i++ {
		email := strings.Repeat("x", i+1) + "@forge.example.com"
		xff := ipToString(203, i) + ":1234"
		if _, err := env.auth.Login(peerCtx(false, "198.51.100.9", xff),
			&serverv1.LoginRequest{Email: email, Password: "whatever-pw"}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("forged attempt %d err = %v, want Unauthenticated", i+1, err)
		}
	}
	// 第 11 次（又是全新 email + 全新伪造 XFF）→ 429：XFF 不参与远程对端
	// 的 IP 键，桶按真实对端计满。
	if _, err := env.auth.Login(peerCtx(false, "198.51.100.9", "203.0.113.250:1234"),
		&serverv1.LoginRequest{Email: "final@forge.example.com", Password: "whatever-pw"}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("11th forged attempt err = %v, want ResourceExhausted (IP key not forgeable)", err)
	}

	// 对照：环回对端 + 不同 XFF（gateway 清洗后的分桶形态）桶各自独立。
	for i := 0; i < 3; i++ {
		email := strings.Repeat("g", i+1) + "@gateway.example.com"
		if _, err := env.auth.Login(peerCtx(true, "", ipToString(198, i)+":1234"),
			&serverv1.LoginRequest{Email: email, Password: "whatever-pw"}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("gateway-shape attempt %d err = %v, want Unauthenticated (per-client buckets)", i+1, err)
		}
	}
}

// ipToString 构造 203.0.x.y / 198.0.x.y 形态的测试 IP 串（避免主保留段）。
func ipToString(base, i int) string {
	return net.IPv4(byte(base), byte(10), byte(i/256), byte(i%256)).String() //nolint:gosec // G115：base∈{198,203}/i<65536 由测试构造保证
}

// TestAuthFaceScopeRegistration：认证/用户面方法级登记完整性——豁免三面 +
// scope 表八方法；漏登记的新方法由拦截器 fail-closed 兜底（admin 拒），
// 本测试钉住登记表本身。
func TestAuthFaceScopeRegistration(t *testing.T) {
	for _, m := range []string{"Register", "Login", "GetRegistrationState"} {
		if !authExempt("/fleetly.server.v1.AuthService/" + m) {
			t.Errorf("AuthService.%s must be auth-exempt", m)
		}
	}
	for _, m := range []string{"Logout", "LogoutAll", "Me", "AcceptInvite"} {
		if authExempt("/fleetly.server.v1.AuthService/" + m) {
			t.Errorf("AuthService.%s must require authentication", m)
		}
		if s, ok := RequiredScope("/fleetly.server.v1.AuthService/" + m); !ok || s != ScopeRead {
			t.Errorf("AuthService.%s scope = %q ok=%v, want read", m, s, ok)
		}
	}
	for _, m := range []string{
		"ListUsers", "CreateUser", "DisableUser", "EnableUser",
		"ResetUserPassword", "GrantPlatformAdmin", "RevokePlatformAdmin", "SetRegistration",
	} {
		if authExempt("/fleetly.server.v1.UsersService/" + m) {
			t.Errorf("UsersService.%s must not be exempt", m)
		}
		if s, ok := RequiredScope("/fleetly.server.v1.UsersService/" + m); !ok || s != ScopeAdmin {
			t.Errorf("UsersService.%s scope = %q ok=%v, want admin", m, s, ok)
		}
	}
	// 会话 cookie 名与缺省 TTL（设计 §2.2 形态；W2-S4 起 TTL 是 AuthService
	// 的注入字段——缺省 7 天滑动、绝对上限 30 天在 state 层封顶）。
	if sessionCookieName != "fleetly_session" {
		t.Fatalf("session cookie name = %q, want fleetly_session", sessionCookieName)
	}
	if NewAuthService(nil).sessionTTL != 7*24*time.Hour {
		t.Fatalf("session TTL = %v, want 7d (design §2.2)", NewAuthService(nil).sessionTTL)
	}
}

// TestAuthRegisterWithInviteToken（v0.3 W3-S4，设计 §3.1「未注册→注册即
// 自动 accept」）：关窗状态下携带有效邀请 token 的注册成功（豁免注册窗）
// 且 Me 即见受邀队与角色；无效 token → E_INVITE_INVALID（不泄漏存在性）；
// 无 token 原路径回归由 TestAuthRegisterAndWindowRules 承载。
func TestAuthRegisterWithInviteToken(t *testing.T) {
	env := newAuthEnv(t)
	client := serverv1.NewAuthServiceClient(env.conn)
	ctx := context.Background()

	// 首用户（平台管理员）+ 团队 + 邀请（state 面构造产生侧；注册窗在
	// 首用户后缺省 closed）。
	root, _ := env.register(t, "203.0.113.7", "root@example.com", "pw-root-123")
	team, err := env.st.CreateTeam(ctx, state.TeamWrite{Slug: "acme", Name: "Acme", CreatedBy: root.GetId()})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := env.st.AddMember(ctx, team.ID, root.GetId(), state.TeamRoleOwner, "", ""); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}
	_, token, err := env.st.CreateInvite(ctx, state.InviteWrite{
		TeamID: team.ID, Email: "newbie@example.com", Role: state.TeamRoleDeveloper, ActorUserID: root.GetId(),
	})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// 关窗 + 有效 token → 注册成功（豁免注册窗），会话照常下发。
	newbie, cookie := env.registerWithInvite(t, "198.51.100.5", "Newbie@Example.COM", "pw-newbie-9", token)
	if newbie.GetIsPlatformAdmin() {
		t.Fatal("invited registrant must not be platform admin")
	}
	// Me 即见个人队 owner + 受邀队 developer（注册即自动 accept）。
	me, err := client.Me(cookieCtx(cookie), &serverv1.MeRequest{})
	if err != nil {
		t.Fatalf("Me after invite registration: %v", err)
	}
	roles := map[string]string{}
	for _, tm := range me.GetTeams() {
		roles[tm.GetTeamId()] = tm.GetRole()
	}
	if roles[team.ID] != state.TeamRoleDeveloper {
		t.Fatalf("Me teams = %+v, want developer on invited team %s", me.GetTeams(), team.ID)
	}

	// 关窗 + 无效 token → E_INVITE_INVALID（查无此 token 形态；已消费/
	// 已吊销/已过期的同码拒绝由 state 层测试钉死）。
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	_, err = client.Register(ctxIP("198.51.100.6"), &serverv1.RegisterRequest{
		Email: "ghost@example.com", Password: "pw-ghost-12", InviteToken: "bogus-token",
	})
	wantCode(t, err, "E_INVITE_INVALID")

	// 负例零副作用：失败注册未建用户行，同 email 开窗后原路径可注册。
	if err := env.st.SaveRegistration(ctx, state.AuthRegistrationOpen, state.AuthSaveOptions{Actor: "user:x"}); err != nil {
		t.Fatalf("open window: %v", err)
	}
	plain, _ := env.register(t, "198.51.100.6", "ghost@example.com", "pw-ghost-12")
	if plain.GetEmail() != "ghost@example.com" {
		t.Fatalf("plain registration = %+v", plain)
	}
}

// registerWithInvite 携带邀请 token 注册并返回（用户投影, 会话 cookie 值）。
func (e *authEnv) registerWithInvite(t *testing.T, ip, email, password, inviteToken string) (*serverv1.UserView, string) {
	t.Helper()
	var hdr metadata.MD
	res, err := serverv1.NewAuthServiceClient(e.conn).Register(ctxIP(ip),
		&serverv1.RegisterRequest{Email: email, Password: password, InviteToken: inviteToken}, grpc.Header(&hdr))
	if err != nil {
		t.Fatalf("Register(with invite): %v", err)
	}
	cookies := hdr.Get("set-cookie")
	if len(cookies) == 0 {
		t.Fatal("Register must carry set-cookie response header metadata")
	}
	value, ok := strings.CutPrefix(cookies[0], sessionCookieName+"=")
	if !ok {
		t.Fatalf("set-cookie %q must target %s", cookies[0], sessionCookieName)
	}
	return res.GetUser(), strings.Split(value, ";")[0]
}

// TestAuditEntryActorDimension（设计 §6 actor 增维）：用户凭据 actor =
// user:<id>；机具令牌回落 human；ActorTokenID 两形态都保留。
func TestAuditEntryActorDimension(t *testing.T) {
	base := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	pCtx := context.WithValue(base, principalKey{}, Principal{TokenID: "tok1", UserID: "user-42"})
	e := auditEntry(pCtx, "app:x", "{}")
	if e.Actor != "user:user-42" || e.ActorTokenID != "tok1" {
		t.Fatalf("user principal audit = %+v, want actor user:user-42 + token preserved", e)
	}
	mCtx := context.WithValue(base, principalKey{}, Principal{TokenID: "tok2"})
	e = auditEntry(mCtx, "app:x", "{}")
	if e.Actor != "human" || e.ActorTokenID != "tok2" {
		t.Fatalf("machine principal audit = %+v, want actor human + token preserved", e)
	}
}
