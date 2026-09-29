package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	gohttp "net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fleetlyrun/fleetly/internal/api"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// TestGatewayRESTDualFace T2.17 验收（REST 双面一致性抽测）：同进程按生产
// 装配形态起 gRPC（完整拦截链：auth → protovalidate，注册 v0.1 服务面）
// 与 gateway HTTP 面：
//   - 无 token GET /v1/apps → 401 + 信封（snake_case、code 空——退化形态）；
//   - admin token GET /v1/apps / GET /v1/apps/{name} / GET /v1/apps/{app}/
//     deployments 各一次成功，字段为 proto 声明名（snake_case），与 gRPC
//     直连同源一致；
//   - deploy token POST /v1/tokens → 403 信封（scope 判定经 gateway 面）。
func TestGatewayRESTDualFace(t *testing.T) {
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

	// 种子数据：一个应用 + 一次部署入队行（不经引擎——直接行写入，部署
	// 列表面只读投影验证）。
	app, err := testsupport.SeedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := st.CreateDeployment(context.Background(), state.DeployRecord{
		AppID: app.ID, AppName: "demo", Kind: "deploy",
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	adminTok := seedScopedToken(t, st, "admin")
	deployTok := seedScopedToken(t, st, "deploy")

	// --- gRPC 面：与 NewGRPCServer 同构（完整拦截链 + 服务注册）。---
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatalf("protovalidate.New: %v", err)
	}
	auth := api.NewAuthenticator(st)
	gs := lynxgrpc.NewServer(
		lynxgrpc.WithAddr("127.0.0.1:0"),
		lynxgrpc.WithLogger(discardLogger()),
		lynxgrpc.WithHealthCheckers(noCheckers),
		lynxgrpc.WithInterceptors(
			auth.UnaryAuthInterceptor(),
			validateUnaryInterceptor(validator),
		),
		lynxgrpc.WithStreamInterceptors(auth.StreamAuthInterceptor()),
	)
	g := gs.GetServer()
	serverv1.RegisterSystemServiceServer(g, api.NewSystemService("dev", st,
		func() []api.SystemComponent { return nil }, nil, nil))
	serverv1.RegisterAppsServiceServer(g, api.NewAppsService(st, box, nil))
	serverv1.RegisterDeploymentsServiceServer(g, api.NewDeploymentsService(st))
	serverv1.RegisterRevisionsServiceServer(g, api.NewRevisionsService(st))
	serverv1.RegisterBuildsServiceServer(g, api.NewBuildsService(st, nil, build.UploadConfig{}))
	serverv1.RegisterDriftServiceServer(g, api.NewDriftService(st, nil))
	serverv1.RegisterEnvServiceServer(g, api.NewEnvService(st, box))
	serverv1.RegisterTokensServiceServer(g, api.NewTokensService(st))
	if err := gs.Init(nil); err != nil {
		t.Fatalf("grpc Init: %v", err)
	}
	gsErr := make(chan error, 1)
	go func() { gsErr <- gs.Start(context.Background()) }()
	t.Cleanup(func() {
		_ = gs.Stop(context.Background())
		<-gsErr
	})
	select {
	case <-gs.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("grpc server not ready within 5s")
	}
	grpcAddr := gs.Addr()

	// --- HTTP 面：生产构造 newGatewayMux 原样复用。---
	gw, err := newGatewayMux(grpcAddr)
	if err != nil {
		t.Fatalf("newGatewayMux: %v", err)
	}
	hs := lynxhttp.NewServer(gw,
		lynxhttp.WithAddr("127.0.0.1:0"),
		lynxhttp.WithHealthCheckers(noCheckers),
		lynxhttp.WithLogger(discardLogger()),
	)
	if err := hs.Init(nil); err != nil {
		t.Fatalf("http Init: %v", err)
	}
	hsErr := make(chan error, 1)
	go func() { hsErr <- hs.Start(context.Background()) }()
	t.Cleanup(func() {
		_ = hs.Stop(context.Background())
		<-hsErr
	})
	select {
	case <-hs.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("http server not ready within 5s")
	}
	base := "http://" + hs.Addr()
	client := &gohttp.Client{Timeout: 5 * time.Second}

	get := func(path, token string) (int, string) {
		req, err := gohttp.NewRequest(gohttp.MethodGet, base+path, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", path, err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	// --- 面 1：无 token → 401 信封（code 空 = 退化形态）。---
	code, body := get("/v1/apps", "")
	if code != 401 {
		t.Fatalf("no token status = %d, body = %s", code, body)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope %s: %v", body, err)
	}
	if _, has := env["message"]; !has {
		t.Fatalf("envelope missing message: %s", body)
	}
	if c, _ := env["code"].(string); c != "" {
		t.Fatalf("degraded envelope code = %q, want empty (FZ-2 contract)", c)
	}

	// --- 面 2：admin token 走通三个读面。---
	code, body = get("/v1/apps", adminTok)
	if code != 200 {
		t.Fatalf("GET /v1/apps status = %d, body = %s", code, body)
	}
	var list struct {
		Apps []map[string]any `json:"apps"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode apps: %v (%s)", err, body)
	}
	if len(list.Apps) != 1 || list.Apps[0]["name"] != "demo" {
		t.Fatalf("apps = %s", body)
	}
	// snake_case 断言：派生状态字段名 = proto 声明名。
	if _, ok := list.Apps[0]["derived_state"]; !ok {
		t.Fatalf("apps[0] missing derived_state (snake_case): %s", body)
	}

	code, body = get("/v1/apps/demo", adminTok)
	if code != 200 {
		t.Fatalf("GET /v1/apps/demo status = %d, body = %s", code, body)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode app: %v (%s)", err, body)
	}
	// queued 行无成功终态、无失败——派生 = running（state-model §2.10）。
	if detail["name"] != "demo" || detail["derived_state"] != "running" {
		t.Fatalf("app detail = %s", body)
	}

	code, body = get("/v1/apps/demo/deployments", adminTok)
	if code != 200 {
		t.Fatalf("GET /v1/apps/demo/deployments status = %d, body = %s", code, body)
	}
	var deps struct {
		Deployments []map[string]any `json:"deployments"`
	}
	if err := json.Unmarshal([]byte(body), &deps); err != nil {
		t.Fatalf("decode deployments: %v (%s)", err, body)
	}
	if len(deps.Deployments) != 1 || deps.Deployments[0]["status"] != "queued" {
		t.Fatalf("deployments = %s", body)
	}

	// --- 面 3：deploy token 建 token → 403 信封（gateway 面的 scope 强制）。---
	req, err := gohttp.NewRequest(gohttp.MethodPost, base+"/v1/tokens",
		strings.NewReader(`{"scopes":["read"],"note":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+deployTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b403, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("deploy token POST /v1/tokens status = %d, body = %s", resp.StatusCode, b403)
	}

	// --- 面 4：REST 与 gRPC 双面同源（GetApp derived_state 一致）。---
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	gCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gResp, err := serverv1.NewAppsServiceClient(conn).GetApp(
		metadata.AppendToOutgoingContext(gCtx, "authorization", "Bearer "+adminTok),
		&serverv1.GetAppRequest{Name: "demo"})
	if err != nil {
		t.Fatalf("gRPC GetApp: %v", err)
	}
	if fmt.Sprint(detail["derived_state"]) != gResp.GetDerivedState() {
		t.Fatalf("REST derived_state %v != gRPC %q", detail["derived_state"], gResp.GetDerivedState())
	}
}

// seedScopedToken 生成并落一枚指定 scope 的 token，返回明文。
func seedScopedToken(t *testing.T, st *state.Store, scope string) string {
	t.Helper()
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plaintext := "flt_" + hex.EncodeToString(buf)
	if _, err := st.CreateToken(context.Background(), state.TokenWrite{
		Hash:   state.HashToken(plaintext),
		Name:   "test " + scope,
		Scopes: scope,
	}); err != nil {
		t.Fatalf("seed token %s: %v", scope, err)
	}
	return plaintext
}

// TestGatewayAuthFailureIPLimit A3（S18）：REST 面匿名 401 per-IP 限速——
// 生产同构装配（gRPC 拦截链 + newRootHandler 的 gateway 限速层）下：
//   - 同 IP 10 次坏 token 请求逐次 401，第 11 次直接 429（不再触达后端）；
//   - 不同 IP 不受牵连（仍 401 / 有效 token 200）。
//
// RemoteAddr 经 httptest 请求注入（真实网络面同一来源形态）。
func TestGatewayAuthFailureIPLimit(t *testing.T) {
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
	adminTok := seedScopedToken(t, st, "admin")

	auth := api.NewAuthenticator(st)
	gs := lynxgrpc.NewServer(
		lynxgrpc.WithAddr("127.0.0.1:0"),
		lynxgrpc.WithLogger(discardLogger()),
		lynxgrpc.WithHealthCheckers(noCheckers),
		lynxgrpc.WithInterceptors(auth.UnaryAuthInterceptor()),
	)
	g := gs.GetServer()
	serverv1.RegisterAppsServiceServer(g, api.NewAppsService(st, box, nil))
	if err := gs.Init(nil); err != nil {
		t.Fatalf("grpc Init: %v", err)
	}
	gsErr := make(chan error, 1)
	go func() { gsErr <- gs.Start(context.Background()) }()
	t.Cleanup(func() {
		_ = gs.Stop(context.Background())
		<-gsErr
	})
	select {
	case <-gs.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("grpc server not ready within 5s")
	}

	mux, err := newGatewayMux(gs.Addr())
	if err != nil {
		t.Fatalf("newGatewayMux: %v", err)
	}
	root := newRootHandler(gohttp.NotFoundHandler(), nil, nil, nil, mux)

	get := func(remoteAddr, token string) int {
		req := httptest.NewRequest(gohttp.MethodGet, "/v1/apps", nil)
		req.RemoteAddr = remoteAddr
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, req)
		return rec.Code
	}

	// 同 IP（10.9.9.1）：10 次坏 token 逐次 401（每次都触达后端鉴权）。
	for i := 0; i < 10; i++ {
		if code := get("10.9.9.1:5555", "flt_wrong"); code != 401 {
			t.Fatalf("bad-token request %d = %d, want 401", i+1, code)
		}
	}
	// 第 11 次：429（该 IP 进入封禁窗，不再触达后端）。
	if code := get("10.9.9.1:5555", "flt_wrong"); code != 429 {
		t.Fatalf("11th bad-token request = %d, want 429", code)
	}
	// 封禁窗内同 IP 携带有效 token 也直接 429（per-IP 语义：先判封禁
	// 再进后端——该 IP 的后续请求一律短路）。
	if code := get("10.9.9.1:5555", adminTok); code != 429 {
		t.Fatalf("banned IP with valid token = %d, want 429", code)
	}

	// 不同 IP 不受牵连：坏 token 仍 401、有效 token 200。
	if code := get("10.9.9.2:5555", "flt_wrong"); code != 401 {
		t.Fatalf("other IP bad token = %d, want 401", code)
	}
	if code := get("10.9.9.2:5555", adminTok); code != 200 {
		t.Fatalf("other IP valid token = %d, want 200", code)
	}
}
