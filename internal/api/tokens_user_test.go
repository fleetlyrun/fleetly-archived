package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// TokensService 用户化语义矩阵（v0.3 W2-S2，rbac-teams 设计 §2.3 验收）：
//   - CreateToken：用户自服务造 PAT（user_id = 自己；声明 scopes ⊆ 可达
//     集——viewer 造 admin PAT → 400 带指引；项目绑定可选且须在册）；
//     机具令牌 = 平台级凭据显式创建（平台管理员用户 machine 旗标 / admin
//     scope 机具令牌），非授权调用方 403；
//   - ListTokens：用户 = 自己的；平台管理员 = 全部（user_id 注记机具）；
//     机具令牌按「平台管理员等价」放行全列；
//   - RevokeToken：自己的或平台管理员；机具令牌吊销 = admin scope；可见
//     集外一律 404（不泄漏存在性）。

// tokEnv 是 token 面测试环境（独立 store；用户经 state.RegisterUser 播种
// ——首用户=平台管理员，其后须先开注册窗口）。
type tokEnv struct {
	st   *state.Store
	conn *grpc.ClientConn
}

func newTokEnv(t *testing.T) *tokEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env := &tokEnv{st: st}
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterTokensServiceServer(srv, NewTokensService(st))
	serverv1.RegisterGitKeysServiceServer(srv, NewGitKeysService(st))
	env.conn = serveBufconn(t, srv)
	return env
}

// seedUser 播种用户（首用户=平台管理员；其后自动开窗注册）。
func (e *tokEnv) seedUser(t *testing.T, email string) state.RegisterResult {
	t.Helper()
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	rr, err := e.st.RegisterUser(context.Background(), state.RegisterWrite{Email: email, Password: "pw-tokens-123"})
	if err == nil {
		return rr
	}
	if !errors.Is(err, state.ErrRegistrationClosed) {
		t.Fatalf("RegisterUser(%s): %v", email, err)
	}
	if err := e.st.SaveRegistration(context.Background(), state.AuthRegistrationOpen, state.AuthSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("SaveRegistration open: %v", err)
	}
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	rr, err = e.st.RegisterUser(context.Background(), state.RegisterWrite{Email: email, Password: "pw-tokens-123"})
	if err != nil {
		t.Fatalf("RegisterUser(%s) after opening: %v", email, err)
	}
	return rr
}

// userToken 为用户签发用户 PAT（scope 可指定），返回明文。
func (e *tokEnv) userToken(t *testing.T, userID, scopes string) string {
	t.Helper()
	plaintext, err := generateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := e.st.CreateToken(context.Background(), state.TokenWrite{
		Hash:   state.HashToken(plaintext),
		Name:   "test user pat",
		Scopes: scopes,
		UserID: userID,
		Actor:  "human",
	}); err != nil {
		t.Fatalf("create user token: %v", err)
	}
	return plaintext
}

// machineToken 落一枚机具令牌（user NULL），返回明文。
func (e *tokEnv) machineToken(t *testing.T, scopes string) string {
	t.Helper()
	return seedTokenPlain(t, e.st, scopes)
}

// TestCreateTokenUserSelfService 用户 principal 造自己的 PAT：行归属
// user_id=自己、新 PAT 即刻可用、审计 actor 署名用户。
func TestCreateTokenUserSelfService(t *testing.T) {
	env := newTokEnv(t)
	ctx := context.Background()
	founder := env.seedUser(t, "founder@example.com")
	tok := env.userToken(t, founder.User.ID, ScopeRead)
	tokens := serverv1.NewTokensServiceClient(env.conn)

	cr, err := tokens.CreateToken(authCtx(ctx, tok), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"}, Note: "laptop",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !strings.HasPrefix(cr.GetToken(), "flt_") {
		t.Fatalf("token = %q", cr.GetToken())
	}
	// 行归属：user_id = 调用方用户；project 未绑定。
	row, err := env.st.AuthenticateToken(ctx, cr.GetToken())
	if err != nil {
		t.Fatalf("new PAT does not authenticate: %v", err)
	}
	if row.UserID != founder.User.ID || row.ProjectID != "" {
		t.Fatalf("row owner = %q project = %q, want user %s/unbound", row.UserID, row.ProjectID, founder.User.ID)
	}
	// 新 PAT 即刻可用（自服务列表：只见自己）。
	lr, err := tokens.ListTokens(authCtx(ctx, cr.GetToken()), &serverv1.ListTokensRequest{})
	if err != nil {
		t.Fatalf("ListTokens with fresh PAT: %v", err)
	}
	if len(lr.GetTokens()) != 2 {
		t.Fatalf("founder list = %d rows, want 2 (both founder PATs)", len(lr.GetTokens()))
	}
}

// TestCreateTokenScopeSubsetGuard 声明 scopes ⊆ 用户可达集。可达集口径
// （设计 §2.3「用户可达集」= 各团队角色蕴含的并集，本票裁决）：注册用户
// 天然持有个人队 owner 角色 → 可达全集（其 admin PAT 的实际效力被 S4
// 角色门按 min(scopes, 目标项目角色) 硬性收缩——防呆非防险）；负例落在
// 无成员关系的用户（CreateUser 通道建的用户，无个人队）上：缺省 {read}、
// 团队 developer 角色补入 deploy/terminal、admin 恒 400 带指引。
func TestCreateTokenScopeSubsetGuard(t *testing.T) {
	env := newTokEnv(t)
	ctx := context.Background()
	tokens := serverv1.NewTokensServiceClient(env.conn)

	// founder（首用户=平台管理员）建队；worker（CreateUser 通道）无个人
	// 队——其可达集由 acme 队内角色决定。
	founder := env.seedUser(t, "owner@example.com")
	team, err := env.st.CreateTeamWithOwner(ctx, state.TeamWrite{
		Slug: "acme", Name: "Acme", CreatedBy: founder.User.ID,
		ActorUserID: founder.User.ID,
	})
	if err != nil {
		t.Fatalf("CreateTeamWithOwner: %v", err)
	}
	//nolint:gosec // G101：测试夹具口令，非真实凭据
	worker, err := env.st.CreateUser(ctx, state.UserWrite{
		Email: "worker@example.com", Password: "temp-pw-worker-123", ActorUserID: founder.User.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser worker: %v", err)
	}

	// admin scope 机具令牌垫底：M4-6 最后管理员守卫要求平台恒有 ≥1 枚
	// admin token，否则一切吊销被拒（本文件吊销断言的前置）。
	env.machineToken(t, ScopeAdmin)

	workerTok := env.userToken(t, worker.ID, ScopeRead)
	founderTok := env.userToken(t, founder.User.ID, ScopeRead)

	// 无成员关系用户（缺省 {read}）声明 admin → 400 + 可行动指引。
	_, err = tokens.CreateToken(authCtx(ctx, workerTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"admin"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("membership-less admin-scope PAT code = %v, want InvalidArgument (400)", status.Code(err))
	}
	if !strings.Contains(err.Error(), "exceeds your granted capabilities") {
		t.Fatalf("admin-scope PAT err = %v (missing guidance)", err)
	}
	// 缺省 {read}：声明 read → OK；声明 terminal → 400（terminal 不随
	// read 下放——§3.2 开发者档才蕴含）。
	if _, err := tokens.CreateToken(authCtx(ctx, workerTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"},
	}); err != nil {
		t.Fatalf("read PAT: %v", err)
	}
	if _, err := tokens.CreateToken(authCtx(ctx, workerTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"terminal"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("membership-less terminal PAT code = %v, want 400", status.Code(err))
	}

	// 团队 developer 角色：deploy+terminal 进入可达集；admin 仍 400。
	if _, err := env.st.AddMember(ctx, team.ID, worker.ID, state.TeamRoleDeveloper, founder.User.ID, ""); err != nil {
		t.Fatalf("AddMember developer: %v", err)
	}
	if _, err := tokens.CreateToken(authCtx(ctx, workerTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"deploy", "terminal"},
	}); err != nil {
		t.Fatalf("developer deploy+terminal PAT: %v", err)
	}
	if _, err := tokens.CreateToken(authCtx(ctx, workerTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"admin"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("developer admin-scope PAT code = %v, want 400", status.Code(err))
	}

	// 注册用户（个人队 owner）可达全集：admin 声明放行（S4 角色门硬性
	// 收缩其实际效力——防呆非防险的口径注记见函数头）。
	if _, err := tokens.CreateToken(authCtx(ctx, founderTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"admin"},
	}); err != nil {
		t.Fatalf("personal-team owner admin-scope PAT: %v", err)
	}
}

// TestCreateTokenProjectBinding 项目绑定可选且须在册；绑定落库并在列表
// 投影回显。
func TestCreateTokenProjectBinding(t *testing.T) {
	env := newTokEnv(t)
	ctx := context.Background()
	founder := env.seedUser(t, "founder@example.com") // 注册落位个人队 + default 项目
	tok := env.userToken(t, founder.User.ID, ScopeRead)
	tokens := serverv1.NewTokensServiceClient(env.conn)

	// 未知项目 → 400。
	_, err := tokens.CreateToken(authCtx(ctx, tok), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"}, ProjectId: "01HNOPROJECT0000000000000000",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown project binding code = %v, want 400", status.Code(err))
	}
	// 绑定注册落位的默认项目 → 行落 project_id，列表回显。
	cr, err := tokens.CreateToken(authCtx(ctx, tok), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"}, ProjectId: founder.Project.ID,
	})
	if err != nil {
		t.Fatalf("bound CreateToken: %v", err)
	}
	row, err := env.st.AuthenticateToken(ctx, cr.GetToken())
	if err != nil {
		t.Fatal(err)
	}
	if row.ProjectID != founder.Project.ID {
		t.Fatalf("row project = %q, want %s", row.ProjectID, founder.Project.ID)
	}
	lr, err := tokens.ListTokens(authCtx(ctx, tok), &serverv1.ListTokensRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var bound *serverv1.TokenView
	for _, v := range lr.GetTokens() {
		if v.GetId() == cr.GetId() {
			bound = v
		}
	}
	if bound == nil || bound.GetProjectId() != founder.Project.ID {
		t.Fatalf("bound token in list = %+v", bound)
	}
}

// TestCreateTokenMachineSemantics 机具令牌语义：admin scope 机具令牌造
// 机具令牌（user NULL，CI 形态保留）；read 机具令牌 403；平台管理员用户
// 经 machine 旗标造机具令牌；非管理员用户 machine 旗标 403。
func TestCreateTokenMachineSemantics(t *testing.T) {
	env := newTokEnv(t)
	ctx := context.Background()
	tokens := serverv1.NewTokensServiceClient(env.conn)
	founder := env.seedUser(t, "founder@example.com")
	mate := env.seedUser(t, "mate@example.com")

	// admin 机具令牌：造机具令牌（行 user NULL）。
	adminMach := env.machineToken(t, ScopeAdmin)
	cr, err := tokens.CreateToken(authCtx(ctx, adminMach), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"}, Note: "ci",
	})
	if err != nil {
		t.Fatalf("admin machine CreateToken: %v", err)
	}
	row, err := env.st.AuthenticateToken(ctx, cr.GetToken())
	if err != nil {
		t.Fatal(err)
	}
	if row.UserID != "" {
		t.Fatalf("machine-created token owner = %q, want empty (machine token)", row.UserID)
	}
	// read 机具令牌：403。
	readMach := env.machineToken(t, ScopeRead)
	if _, err := tokens.CreateToken(authCtx(ctx, readMach), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"},
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read machine CreateToken code = %v, want PermissionDenied", status.Code(err))
	}
	// 平台管理员用户 + machine 旗标 → 机具令牌。
	founderTok := env.userToken(t, founder.User.ID, ScopeRead)
	cr2, err := tokens.CreateToken(authCtx(ctx, founderTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"}, Machine: true,
	})
	if err != nil {
		t.Fatalf("platform admin machine flag CreateToken: %v", err)
	}
	row2, err := env.st.AuthenticateToken(ctx, cr2.GetToken())
	if err != nil {
		t.Fatal(err)
	}
	if row2.UserID != "" {
		t.Fatalf("machine flag token owner = %q, want empty", row2.UserID)
	}
	// 非管理员用户 + machine 旗标 → 403。
	mateTok := env.userToken(t, mate.User.ID, ScopeRead)
	if _, err := tokens.CreateToken(authCtx(ctx, mateTok), &serverv1.CreateTokenRequest{
		Scopes: []string{"read"}, Machine: true,
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin machine flag code = %v, want PermissionDenied", status.Code(err))
	}
}

// TestListTokensVisibility 列表可见性：用户 = 自己的；平台管理员 =
// 全部（user_id 注记）；机具令牌 = 全部。
func TestListTokensVisibility(t *testing.T) {
	env := newTokEnv(t)
	ctx := context.Background()
	tokens := serverv1.NewTokensServiceClient(env.conn)
	founder := env.seedUser(t, "founder@example.com")
	mate := env.seedUser(t, "mate@example.com")

	founderTok := env.userToken(t, founder.User.ID, ScopeRead)
	mateTok := env.userToken(t, mate.User.ID, ScopeRead)
	machTok := env.machineToken(t, ScopeRead) // 库内共 3 枚

	// mate：只见自己的。
	lr, err := tokens.ListTokens(authCtx(ctx, mateTok), &serverv1.ListTokensRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.GetTokens()) != 1 || lr.GetTokens()[0].GetUserId() != mate.User.ID {
		t.Fatalf("mate list = %+v, want own single row", lr.GetTokens())
	}
	// 平台管理员：全部（3 枚，机具行 user_id 缺省）。
	lr, err = tokens.ListTokens(authCtx(ctx, founderTok), &serverv1.ListTokensRequest{})
	if err != nil {
		t.Fatal(err)
	}
	machineRows := 0
	for _, v := range lr.GetTokens() {
		if v.GetUserId() == "" {
			machineRows++
		}
	}
	if len(lr.GetTokens()) != 3 || machineRows != 1 {
		t.Fatalf("platform admin list = %d rows (%d machine), want 3 rows / 1 machine", len(lr.GetTokens()), machineRows)
	}
	// 机具令牌：全部（平台管理员等价放行）。
	lr, err = tokens.ListTokens(authCtx(ctx, machTok), &serverv1.ListTokensRequest{})
	if err != nil {
		t.Fatalf("machine list: %v", err)
	}
	if len(lr.GetTokens()) != 3 {
		t.Fatalf("machine list = %d rows, want 3", len(lr.GetTokens()))
	}
}

// TestRevokeTokenPermissions 吊销权矩阵：自己的 OK；他人/机具令牌对普通
// 用户 404（可见集外不泄漏存在性）；read 机具令牌 403；admin 机具令牌与
// 平台管理员任意。
func TestRevokeTokenPermissions(t *testing.T) {
	env := newTokEnv(t)
	ctx := context.Background()
	tokens := serverv1.NewTokensServiceClient(env.conn)
	founder := env.seedUser(t, "founder@example.com")
	mate := env.seedUser(t, "mate@example.com")

	founderTok := env.userToken(t, founder.User.ID, ScopeRead)
	mateTok := env.userToken(t, mate.User.ID, ScopeRead)      // 被吊销对象
	mateCallTok := env.userToken(t, mate.User.ID, ScopeRead) // 负例断言的调用方
	adminMach := env.machineToken(t, ScopeAdmin)
	readMach := env.machineToken(t, ScopeRead)
	machTarget := env.machineToken(t, ScopeRead) // 被吊销的机具令牌

	// mate 吊销自己的 → OK（幂等面：再吊一次仍 OK）。
	if _, err := tokens.RevokeToken(authCtx(ctx, mateCallTok), &serverv1.RevokeTokenRequest{Id: tokenIDAt(t, env, mateTok)}); err != nil {
		t.Fatalf("revoke own: %v", err)
	}
	// mate 吊销 founder 的 → 404。
	if _, err := tokens.RevokeToken(authCtx(ctx, mateCallTok), &serverv1.RevokeTokenRequest{Id: tokenIDAt(t, env, founderTok)}); status.Code(err) != codes.NotFound {
		t.Fatalf("revoke other user's token code = %v, want NotFound", status.Code(err))
	}
	// mate 吊销机具令牌 → 404（机具令牌不在普通用户可见集）。
	if _, err := tokens.RevokeToken(authCtx(ctx, mateCallTok), &serverv1.RevokeTokenRequest{Id: tokenIDAt(t, env, machTarget)}); status.Code(err) != codes.NotFound {
		t.Fatalf("user revoking machine token code = %v, want NotFound", status.Code(err))
	}
	// read 机具令牌吊销 → 403。
	if _, err := tokens.RevokeToken(authCtx(ctx, readMach), &serverv1.RevokeTokenRequest{Id: tokenIDAt(t, env, machTarget)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read machine revoke code = %v, want PermissionDenied", status.Code(err))
	}
	// 平台管理员吊销机具令牌 → OK。
	if _, err := tokens.RevokeToken(authCtx(ctx, founderTok), &serverv1.RevokeTokenRequest{Id: tokenIDAt(t, env, machTarget)}); err != nil {
		t.Fatalf("platform admin revoking machine token: %v", err)
	}
	// admin 机具令牌吊销用户 PAT → OK。
	if _, err := tokens.RevokeToken(authCtx(ctx, adminMach), &serverv1.RevokeTokenRequest{Id: tokenIDAt(t, env, founderTok)}); err != nil {
		t.Fatalf("admin machine revoking user PAT: %v", err)
	}
	// 吊销真实生效：founder PAT 已死 → 401。
	if _, err := tokens.ListTokens(authCtx(ctx, founderTok), &serverv1.ListTokensRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("revoked PAT still authenticates: %v", err)
	}
}

// tokenIDAt 由明文反查 token 行 id（断言辅助——吊销目标定位）。
func tokenIDAt(t *testing.T, env *tokEnv, plaintext string) string {
	t.Helper()
	row, err := env.st.AuthenticateToken(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("probe token: %v", err)
	}
	return row.ID
}
