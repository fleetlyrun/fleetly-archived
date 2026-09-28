package api

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 测试装配：真实 state.Store（独立临时目录）+ 拦截链与生产同构的 gRPC
// server（bufconn）+ 预置三枚 scope token（read/deploy/admin）。

// testEnv 是 API 测试环境。
type testEnv struct {
	st      *state.Store
	box     *secrets.Box
	conn    *grpc.ClientConn
	readTok string
	depTok  string
	admTok  string
	// fixtureProject 是 env 级夹具项目（v0.3 归属必填：机具令牌的 Deploy/
	// CreateDatabase 请求必须显式携带 project——测试经 projectRef() 引用）。
	fixtureProject state.Project
}

// projectRef 返回夹具项目的引用（项目 ID——resolveProjectRef 支持裸名/
// team/project 限定形/ID 三形态，ID 免疫随机 slug）。
func (e *testEnv) projectRef() string { return e.fixtureProject.ID }

// seedFixtureProject 播种 env 级夹具团队 + 项目（slug 确定性 "fixture"，
// 幂等——同 store 重复调用返回既有行；机具令牌按裸名解析全域唯一命中）。
func seedFixtureProject(t *testing.T, st *state.Store) state.Project {
	t.Helper()
	if team, err := st.GetTeamBySlug(context.Background(), "tfixture"); err == nil {
		projects, perr := st.ListProjects(context.Background())
		if perr == nil {
			for _, p := range projects {
				if p.TeamID == team.ID && p.Slug == "fixture" {
					return p
				}
			}
		}
	}
	team, err := st.CreateTeam(context.Background(), state.TeamWrite{
		Slug: "tfixture", Name: "fixture team", CreatedBy: "fixture",
	})
	if err != nil {
		t.Fatalf("seed fixture team: %v", err)
	}
	proj, err := st.CreateProject(context.Background(), state.ProjectWrite{
		TeamID: team.ID, Slug: "fixture", Name: "fixture project",
	})
	if err != nil {
		t.Fatalf("seed fixture project: %v", err)
	}
	return proj
}

// newTestEnv 起一个带鉴权链的 gRPC server（bufconn），注册鉴权矩阵触达的
// 服务面（Apps/Deployments/Env/Tokens；流式面在 events/logs 专属测试装配）。
func newTestEnv(t *testing.T) *testEnv {
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

	env := &testEnv{st: st, box: box, fixtureProject: seedFixtureProject(t, st)}
	env.readTok = env.seedToken(t, "read")
	env.depTok = env.seedToken(t, "deploy")
	env.admTok = env.seedToken(t, "admin")

	auth := NewAuthenticator(st)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(auth.UnaryAuthInterceptor()),
		grpc.ChainStreamInterceptor(auth.StreamAuthInterceptor()),
	)
	serverv1.RegisterAppsServiceServer(srv, NewAppsService(st, box, "127.0.0.1:8424", nil))
	serverv1.RegisterDeploymentsServiceServer(srv, NewDeploymentsService(st, nil))
	serverv1.RegisterBuildsServiceServer(srv, NewBuildsService(st, nil, build.UploadConfig{}))
	serverv1.RegisterEnvServiceServer(srv, NewEnvService(st, box))
	serverv1.RegisterTokensServiceServer(srv, NewTokensService(st))
	serverv1.RegisterGitKeysServiceServer(srv, NewGitKeysService(st))

	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	env.conn = conn
	return env
}

// seedToken 生成并落一枚指定 scope 的 token，返回明文。
func (e *testEnv) seedToken(t *testing.T, scopes string) string {
	t.Helper()
	return seedTokenPlain(t, e.st, scopes)
}

// authCtx 带 Bearer 凭据的 ctx（测试直连 gRPC 用 metadata 形态）。
func authCtx(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// directCtx 是进程内直调 handler 的测试上下文：注入与「机具 admin 令牌过
// 生产拦截链」等效的 Principal（UserID 空 = 全库 admin 等价，rbac-teams
// §2.3 设计语义）。仅服务直调夹具使用——不经 bufconn 鉴权链的旧形态测试
// 随 W2-S4 角色门迁移到本形态（角色门对无 principal 一律 fail-closed）。
func directCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalKey{}, Principal{TokenID: "direct-test", Scopes: []string{ScopeAdmin}})
}

// directPrincipalInterceptor 是直调测试装配（bufconn 不挂生产鉴权链的
// 服务端）的 Principal 注入拦截器：与 directCtx 同一夹具语义，服务端形态
// （gRPC outgoing ctx 值不跨线，客户端注入对服务端 handler 不可见）。
func directPrincipalInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(directCtx(ctx), req)
	}
}

// newAuthServer 起一个只挂鉴权拦截链的空 server（各测试自行注册服务）。
func newAuthServer(auth *Authenticator) *grpc.Server {
	return grpc.NewServer(
		grpc.ChainUnaryInterceptor(auth.UnaryAuthInterceptor()),
		grpc.ChainStreamInterceptor(auth.StreamAuthInterceptor()),
	)
}

// serveBufconn 启动 server 并返回客户端连接（生命周期挂 t.Cleanup）。
func serveBufconn(t *testing.T, srv *grpc.Server) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// tokenFor 直接落一枚 admin token 并返回明文（流式面测试的鉴权凭据）。
func tokenFor(t *testing.T, st *state.Store) string {
	t.Helper()
	return seedTokenPlain(t, st, "admin")
}

// seedTokenPlain 生成并落一枚指定 scope 的 token，返回明文。
func seedTokenPlain(t *testing.T, st *state.Store, scopes string) string {
	t.Helper()
	plaintext, err := generateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := st.CreateToken(context.Background(), state.TokenWrite{
		Hash:   state.HashToken(plaintext),
		Name:   "test " + scopes,
		Scopes: scopes,
	}); err != nil {
		t.Fatalf("seed token %s: %v", scopes, err)
	}
	return plaintext
}
