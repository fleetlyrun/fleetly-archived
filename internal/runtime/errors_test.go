package runtime

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/lynx-go/lynx"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var update = flag.Bool("update", false, "rewrite golden files")

// envelopeKeys 是信封契约的七字段（proto 声明名，snake_case）。
var envelopeKeys = []string{"code", "message", "stage", "deployment_id", "suggestion", "context", "docs"}

// sampleAppErr 构造七字段全非空的样例错误（前哨 409 语义）。
func sampleAppErr() *apperr.Error {
	return apperr.New("E_VOLUME_NODE_MISMATCH", "volume data node does not match the target deployment node").
		WithStage("preflight").
		WithDeploymentID("d_01JGOLDEN").
		WithContext("bound_node", "n_01").
		WithContext("target_node", "n_02").
		WithContext("volume", "data")
}

// TestGatewayErrorEnvelopeGolden 是验收标准 4 的 golden 面：apperr →
// ToGRPCStatus → HTTPErrorHandler → 完整 JSON（snake_case 七字段、HTTP 按
// 注册表映射 409）。golden 比对取「解析后深比较」：protojson 对输出随机
// 注入空格（反依赖设计，字节级不可复现），而信封的契约面是 JSON 结构——
// 字段集合与值。新增字段/改名/值变更都会使深比较失败，必须显式 -update
// 重生成 golden，防信封静默变更。
func TestGatewayErrorEnvelopeGolden(t *testing.T) {
	st := sampleAppErr().ToGRPCStatus()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/system/ping", nil)
	newGatewayErrorHandler()(context.Background(), nil, newJSONMarshaler(), rec, req, st.Err())

	if rec.Code != http.StatusConflict {
		t.Fatalf("HTTP status = %d, want 409 (registry: preflight maps to 409)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := rec.Body.String()

	// snake_case 七字段键集断言（恰为七键、无 camelCase 变体）。
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(raw) != len(envelopeKeys) {
		t.Fatalf("keys = %v (%d), want exactly %v", raw, len(raw), envelopeKeys)
	}
	for _, k := range envelopeKeys {
		if _, ok := raw[k]; !ok {
			t.Fatalf("key %q missing in %s (envelope must be the seven snake_case fields)", k, body)
		}
	}

	golden := filepath.Join("testdata", "envelope.golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, []byte(body), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	wantBytes, err := os.ReadFile(golden) //nolint:gosec // golden 为 testdata 固定路径
	if err != nil {
		t.Fatalf("read golden (run go test -update to regenerate): %v", err)
	}
	var want map[string]any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatalf("decode golden %s: %v", wantBytes, err)
	}
	if !reflect.DeepEqual(want, raw) {
		t.Fatalf("envelope drifted from golden:\n--- golden ---\n%s\n--- got ---\n%s", wantBytes, body)
	}
}

// TestGatewayDegradedEnvelope：无 detail 错误的退化信封——code 不得是发明
// 出来的注册码（gateway EmitUnpopulated=true 下空 code 显式输出 ""——2026-09-29
// JSON 口径反转；「空」语义不变，只是从缺席变显式）、message 保底、HTTP 由
// grpc code 机械映射（取舍待 T0.5 确认）。
func TestGatewayDegradedEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/system/ping", nil)
	err := status.New(codes.NotFound, "no route").Err()
	newGatewayErrorHandler()(context.Background(), nil, newJSONMarshaler(), rec, req, err)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("HTTP status = %d, want 404 (mechanical grpc code mapping)", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if _, ok := raw["message"]; !ok {
		t.Fatalf("degraded envelope must keep message, got %s", rec.Body.String())
	}
	// code 不得是发明的注册码：EmitUnpopulated=true 下空 code 显式输出 ""，
	// 除此之外（缺席或非空码）都不对。
	if codeRaw, ok := raw["code"]; ok && string(codeRaw) != `""` {
		t.Fatalf("degraded envelope must not invent a code, got %s", rec.Body.String())
	}
}

// TestValidateStatusErrorEnvelope：校验违规 → InvalidArgument + 信封 detail
// （code 留空、违规明细进 message 与 context；不新造清单外码）。用真
// protovalidate 对 PingResponse{} 求值（service/version 空违反 min_len），
// 不依赖手工拼装违规结构。
func TestValidateStatusErrorEnvelope(t *testing.T) {
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatalf("protovalidate.New: %v", err)
	}
	vErr := validator.Validate(&serverv1.PingResponse{})
	if vErr == nil {
		t.Fatal("expected validation error for empty PingResponse")
	}

	err = validateStatusError(vErr)
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("validateStatusError produced non-status error: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", st.Code())
	}
	decoded, ok := apperr.FromGRPCStatus(st)
	if !ok {
		t.Fatalf("status lost envelope detail: %v", st)
	}
	if decoded.Code() != "" {
		t.Fatalf("validation envelope code = %q, want empty (no invented codes)", decoded.Code())
	}
	if decoded.Context()["service"] == "" || decoded.Context()["version"] == "" {
		// protovalidate 违规的 field path 是消息相对路径（FieldPathString），
		// 键 = 字段路径、值 = 规则 ID + 说明。
		t.Fatalf("context must carry field-path keyed violations, got %v", decoded.Context())
	}

	// CEL 求值故障分支：Internal + 信封 detail。
	st2, ok := status.FromError(validateStatusError(io.ErrUnexpectedEOF))
	if !ok || st2.Code() != codes.Internal {
		t.Fatalf("CEL failure branch: code=%v ok=%v", st2.Code(), ok)
	}
	if _, ok := apperr.FromGRPCStatus(st2); !ok {
		t.Fatal("CEL failure branch lost envelope detail")
	}
}

// failingSystemService 是返回预置错误的 SystemService 测试实现
// （端到端用：不碰 proto，经真实 gRPC + gateway FromEndpoint 链路）。
type failingSystemService struct {
	serverv1.UnimplementedSystemServiceServer
	err error
}

func (s *failingSystemService) Ping(ctx context.Context, req *serverv1.PingRequest) (*serverv1.PingResponse, error) {
	return nil, s.err
}

// TestRESTErrorEndToEnd 是验收标准 4 的端到端面：测试内注册返回 apperr 的
// handler → 生产形态 gRPC（lynx + protovalidate 拦截器）→ newGatewayMux
// （FromEndpoint + 自定义 HTTPErrorHandler）→ REST 响应断言 ErrorResponse
// 形态与 HTTP 码；gRPC 面同錯误可被 FromError 还原；healthz 与错误路由共存。
func TestRESTErrorEndToEnd(t *testing.T) {
	appErr := sampleAppErr()
	gs := lynxgrpc.NewServer(
		lynxgrpc.WithAddr("127.0.0.1:0"),
		lynxgrpc.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		lynxgrpc.WithHealthCheckers(func() []lynx.Checker { return nil }),
	)
	serverv1.RegisterSystemServiceServer(gs.GetServer(), &failingSystemService{err: appErr})
	if err := gs.Init(nil); err != nil {
		t.Fatalf("grpc Init: %v", err)
	}
	gsErr := make(chan error, 1)
	go func() { gsErr <- gs.Start(context.Background()) }()
	t.Cleanup(func() {
		if err := gs.Stop(context.Background()); err != nil {
			t.Errorf("grpc Stop: %v", err)
		}
		<-gsErr
	})
	select {
	case <-gs.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("grpc server not ready within 5s")
	}
	grpcAddr := gs.Addr()

	gw, err := newGatewayMux(grpcAddr)
	if err != nil {
		t.Fatalf("newGatewayMux: %v", err)
	}
	hs := lynxhttp.NewServer(gw,
		lynxhttp.WithAddr("127.0.0.1:0"),
		lynxhttp.WithHealthCheckers(func() []lynx.Checker { return nil }),
		lynxhttp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err := hs.Init(nil); err != nil {
		t.Fatalf("http Init: %v", err)
	}
	hsErr := make(chan error, 1)
	go func() { hsErr <- hs.Start(context.Background()) }()
	t.Cleanup(func() {
		if err := hs.Stop(context.Background()); err != nil {
			t.Errorf("http Stop: %v", err)
		}
		<-hsErr
	})
	select {
	case <-hs.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("http server not ready within 5s")
	}
	httpAddr := "http://" + hs.Addr()
	client := &http.Client{Timeout: 3 * time.Second}

	// --- REST 面：完整错误链。---
	resp, err := client.Get(httpAddr + "/v1/system/ping")
	if err != nil {
		t.Fatalf("GET /v1/system/ping: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("HTTP status = %d, want 409, body = %s", resp.StatusCode, body)
	}
	t.Logf("REST error wire body (HTTP %d): %s", resp.StatusCode, body)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for _, k := range envelopeKeys {
		if _, ok := raw[k]; !ok {
			t.Fatalf("key %q missing in %s (REST error response must be an ErrorResponse envelope)", k, body)
		}
	}
	var env struct {
		Code         string            `json:"code"`
		Message      string            `json:"message"`
		Stage        string            `json:"stage"`
		DeploymentID string            `json:"deployment_id"`
		Suggestion   string            `json:"suggestion"`
		Context      map[string]string `json:"context"`
		Docs         string            `json:"docs"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode into envelope struct: %v", err)
	}
	if env.Code != "E_VOLUME_NODE_MISMATCH" || env.Stage != "preflight" || env.DeploymentID != "d_01JGOLDEN" {
		t.Fatalf("envelope fields mismatch: %+v", env)
	}
	if env.Context["bound_node"] != "n_01" || env.Context["target_node"] != "n_02" {
		t.Fatalf("envelope context mismatch: %v", env.Context)
	}
	if !strings.HasPrefix(env.Docs, "https://docs.fleetly.dev/errors/E_VOLUME_NODE_MISMATCH") {
		t.Fatalf("docs anchor = %q", env.Docs)
	}

	// --- gRPC 面：detail 过线后可还原信封（SDK 同款调用路径）。---
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, rpcErr := serverv1.NewSystemServiceClient(conn).Ping(ctx, &serverv1.PingRequest{})
	if rpcErr == nil {
		t.Fatal("gRPC Ping on failing service must fail")
	}
	got, ok := apperr.FromError(rpcErr)
	if !ok {
		t.Fatalf("gRPC error lost envelope detail: %v", rpcErr)
	}
	if got.Code() != "E_VOLUME_NODE_MISMATCH" || got.HTTPStatus() != 409 {
		t.Fatalf("restored apperr mismatch: code=%q http=%d", got.Code(), got.HTTPStatus())
	}

	// --- 共存：healthz 不经错误链，仍 200（阶段 1 能力未破坏）。---
	hResp, err := client.Get(httpAddr + "/healthz/liveness")
	if err != nil {
		t.Fatalf("GET /healthz/liveness: %v", err)
	}
	_ = hResp.Body.Close()
	if hResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz/liveness: status = %d, want 200", hResp.StatusCode)
	}
}
