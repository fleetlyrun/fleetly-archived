package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"

	"github.com/fleetlyrun/fleetly/internal/api"
	"github.com/fleetlyrun/fleetly/internal/gitserver"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// TestGatewayWebhookNativeEndpoints（T2.19 验收：路径豁免精确性）：
// 生产同构装配（gRPC 拦截链 + gateway mux + webhook 原生端点分派）下——
//   - 非 webhook 路径无 token 仍 401（豁免没有放宽到任何其他路径）；
//   - webhook 路径无 Bearer token 但签名正确 → 通过（签名即认证）；
//   - webhook 路径错签名 → 401 退化信封；
//   - 未知 provider（前缀近似路径）→ 404（豁免面 = 分派面，不放宽）。
func TestGatewayWebhookNativeEndpoints(t *testing.T) {
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

	app, err := testsupport.SeedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	const secret = "hook-secret-at-least-16ch"
	cipher, err := box.Encrypt([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppWebhookSecret(context.Background(), app.ID, string(cipher), ""); err != nil {
		t.Fatalf("SetAppWebhookSecret: %v", err)
	}

	src := gitserver.NewGitTriggers(gitserver.Config{
		Root:      filepath.Join(dir, "git"),
		ReplayTTL: time.Minute,
	}, st, box, discardLogger())

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

	// --- HTTP 面：newGatewayMux + webhook 原生端点分派（newRootHandler）。---
	mux, err := newGatewayMux(gs.Addr())
	if err != nil {
		t.Fatalf("newGatewayMux: %v", err)
	}
	base := startRootHTTP(t, newRootHandler(gitserver.NewWebhookHandler(src), nil, nil, nil, mux))

	post := func(method, path string, headers map[string]string, body string) (int, string) {
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	body := `{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// 1. 非 webhook 路径无 token 仍 401（既有全路径口径不被本阶段放宽；
	// 请求方法与路由形态匹配——gateway 的方法分派先于鉴权，405 会掩盖 401）。
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/apps"},
		{http.MethodGet, "/v1/tokens"},
		{http.MethodGet, "/v1/apps/demo/deployments"},
		{http.MethodPost, "/v1/apps/demo/rollbacks"},
	} {
		if code, raw := post(tc.method, tc.path, nil, "{}"); code != 401 {
			t.Fatalf("no-token %s %s = %d, want 401 (%s)", tc.method, tc.path, code, raw)
		}
	}
	// 近似路径（webhooks 段拼写变体）不属于豁免面：gateway 无此路由 → 404
	//（不进入任何处理路径；分派面 = 豁免面，变体路径绝不复用 webhook 语义）。
	if code, raw := post(http.MethodPost, "/v1/apps/demo/webhookz/github", nil, body); code != 404 {
		t.Fatalf("typo path = %d, want 404 (%s)", code, raw)
	}

	// 2. webhook 路径：无 Bearer、签名正确 → 通过认证链（签名即认证）。
	// after 为全零（分支删除形态）→ ignored 回执（不触 fetch，纯语义断言）。
	code, raw := post(http.MethodPost, "/v1/apps/demo/webhooks/github", map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Delivery":   "gw-1",
		"Content-Type":        "application/json",
	}, body)
	if code != 200 {
		t.Fatalf("valid signature = %d (%s)", code, raw)
	}
	var rec webhookReceiptShape
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Status != "ignored" {
		t.Fatalf("receipt = %s (err=%v)", raw, err)
	}

	// 3. 错签名 → 401 退化信封（code 空，FZ-2）。
	code, raw = post(http.MethodPost, "/v1/apps/demo/webhooks/github", map[string]string{
		"X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64),
		"X-GitHub-Delivery":   "gw-2",
	}, body)
	if code != 401 {
		t.Fatalf("bad signature = %d", code)
	}
	var env map[string]any
	_ = json.Unmarshal([]byte(raw), &env)
	if c, _ := env["code"].(string); c != "" {
		t.Fatalf("degraded envelope code = %q, want empty", c)
	}

	// 4. 未知 provider → 404。
	if code, _ = post(http.MethodPost, "/v1/apps/demo/webhooks/unknown", map[string]string{"X-GitHub-Delivery": "gw-3"}, body); code != 404 {
		t.Fatalf("unknown provider = %d, want 404", code)
	}
}

// webhookReceiptShape 是 webhook 回执的测试投影（避免跨包取私有类型）。
type webhookReceiptShape struct {
	Status string `json:"status"`
}

// startRootHTTP 起 HTTP server 并等待 ready（复用 gateway_rest_test 形态）。
func startRootHTTP(t *testing.T, root http.Handler) string {
	t.Helper()
	hs := lynxhttp.NewServer(root,
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
	return "http://" + hs.Addr()
}
