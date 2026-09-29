package runtime

import (
	"context"
	"encoding/json"
	gohttp "net/http"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	"github.com/fleetlyrun/fleetly/internal/api"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// TestGatewayTeamsRestFlow（v0.3 W2-S1 验收：团队/项目面走 gateway REST 形
// 端到端——REST 注解 + gateway 注册 + HTTP 码语义）：生产同构装配下走
// REST 面：
//   - POST /v1/auth/register → 会话 cookie（豁免面，W1 同款）；
//   - POST /v1/teams（会话）→ 200 + TeamView；GET /v1/teams → 队伍在列；
//   - PATCH /v1/teams/{id} 改名 → 200 且 slug 不动；
//   - 机具令牌（admin scope Bearer）GET/POST /v1/teams → 403（团队面无
//     用户即无成员身份——含读面）；
//   - DELETE /v1/teams/{id} 无 confirm → 400（两段式）；?confirm=slug → 200。
func TestGatewayTeamsRestFlow(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	gs := lynxgrpc.NewServer(
		lynxgrpc.WithAddr("127.0.0.1:0"),
		lynxgrpc.WithLogger(discardLogger()),
		lynxgrpc.WithHealthCheckers(noCheckers),
		lynxgrpc.WithInterceptors(api.NewAuthenticator(st).UnaryAuthInterceptor()),
	)
	g := gs.GetServer()
	serverv1.RegisterAuthServiceServer(g, api.NewAuthService(st))
	serverv1.RegisterTeamsServiceServer(g, api.NewTeamsService(st))
	serverv1.RegisterProjectsServiceServer(g, api.NewProjectsService(st))
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

	gw, err := newGatewayMux(gs.Addr())
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

	// do 发一次 REST 请求，返回（status, body, Set-Cookie 值列表）。
	do := func(method, path, body, cookie, bearer string) (int, string, []string) {
		t.Helper()
		var req *gohttp.Request
		if body == "" {
			req, err = gohttp.NewRequest(method, base+path, nil)
		} else {
			req, err = gohttp.NewRequest(method, base+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		if cookie != "" {
			req.AddCookie(&gohttp.Cookie{Name: "fleetly_session", Value: cookie}) //nolint:gosec // G124：客户端注入会话 cookie 测鉴权路径，属性由服务端 Set-Cookie 语义承担
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b), resp.Header.Values("Set-Cookie")
	}

	// 首用户注册（豁免面）→ 会话 cookie；顺手落一枚机具令牌（admin scope
	// 的平台级凭据）。
	code, body, cookies := do(gohttp.MethodPost, "/v1/auth/register",
		`{"email":"restuser@example.com","password":"pw-rest-123"}`, "", "")
	if code != 200 || len(cookies) == 0 {
		t.Fatalf("POST /v1/auth/register = %d %s cookies=%v, want 200 + Set-Cookie", code, body, cookies)
	}
	session := strings.Split(strings.TrimPrefix(cookies[0], "fleetly_session="), ";")[0]
	machine, err := api.GenerateBootstrapAdminToken(context.Background(), st, "gateway teams machine")
	if err != nil {
		t.Fatalf("seed machine token: %v", err)
	}

	// 建队：200 + 投影。
	code, body, _ = do(gohttp.MethodPost, "/v1/teams", `{"slug":"restteam","name":"Rest Team"}`, session, "")
	if code != 200 || !strings.Contains(body, `"slug":"restteam"`) {
		t.Fatalf("POST /v1/teams = %d %s, want 200 with team projection", code, body)
	}
	var created struct {
		Team struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"team"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Team.ID == "" {
		t.Fatalf("team projection parse: %v (%s)", err, body)
	}

	// 列表：我所在（注册个人队 + restteam）。
	code, body, _ = do(gohttp.MethodGet, "/v1/teams", "", session, "")
	if code != 200 || !strings.Contains(body, "restteam") {
		t.Fatalf("GET /v1/teams = %d %s, want 200 containing restteam", code, body)
	}

	// 改名：slug 不动。
	code, body, _ = do(gohttp.MethodPatch, "/v1/teams/"+created.Team.ID, `{"name":"Rest Team Renamed"}`, session, "")
	if code != 200 || !strings.Contains(body, "Rest Team Renamed") || !strings.Contains(body, `"slug":"restteam"`) {
		t.Fatalf("PATCH /v1/teams = %d %s, want 200 with new name and unchanged slug", code, body)
	}

	// 机具令牌：团队面恒 403（读面与写面）。
	if code, _, _ = do(gohttp.MethodGet, "/v1/teams", "", "", machine); code != 403 {
		t.Fatalf("machine GET /v1/teams = %d, want 403", code)
	}
	if code, _, _ = do(gohttp.MethodPost, "/v1/teams", `{"slug":"mach","name":"M"}`, "", machine); code != 403 {
		t.Fatalf("machine POST /v1/teams = %d, want 403", code)
	}

	// 删除两段式：无 confirm → 400；confirm = slug → 200（query 形态——
	// DELETE 无 body，grpc-gateway 按查询参数映射请求字段）。
	if code, _, _ = do(gohttp.MethodDelete, "/v1/teams/"+created.Team.ID, "", session, ""); code != 400 {
		t.Fatalf("DELETE without confirm = %d, want 400", code)
	}
	if code, _, _ = do(gohttp.MethodDelete, "/v1/teams/"+created.Team.ID+"?confirm=restteam", "", session, ""); code != 200 {
		t.Fatalf("DELETE with confirm = %d, want 200", code)
	}
	if code, body, _ = do(gohttp.MethodGet, "/v1/teams", "", session, ""); code != 200 || strings.Contains(body, "restteam") {
		t.Fatalf("GET /v1/teams after delete must not list restteam (code=%d body=%s)", code, body)
	}
}
