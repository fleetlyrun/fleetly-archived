package apperr

import (
	"errors"
	"net/http"
	"testing"

	sharedv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/shared/v1"
	"github.com/fleetlyrun/fleetly/internal/errcode"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestNewPanicsOnUnregisteredCode：信封 code 只允许注册表内码（构造期
// fail-fast，保证不出现文档外码）。
func TestNewPanicsOnUnregisteredCode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with unregistered code must panic")
		}
	}()
	_ = New("E_NOT_IN_REGISTRY", "boom")
}

// TestNewPanicsOnWarningCode：W_ 警告码不得构造为错误（警告不作为
// HTTP/gRPC 错误返回）。
func TestNewPanicsOnWarningCode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with W_ warning code must panic")
		}
	}()
	_ = New("W_DEPLOY_INSTABILITY", "boom")
}

// TestDefaultsFromRegistry：suggestion/docs 取注册表默认；code/message 保真。
func TestDefaultsFromRegistry(t *testing.T) {
	c, _ := errcode.Get("E_VOLUME_NODE_MISMATCH")
	e := New("E_VOLUME_NODE_MISMATCH", "volume data node ≠ target node (volume=%s)", "v_abc")
	if e.Code() != "E_VOLUME_NODE_MISMATCH" {
		t.Fatalf("code = %q", e.Code())
	}
	if e.Message() != "volume data node ≠ target node (volume=v_abc)" {
		t.Fatalf("message = %q", e.Message())
	}
	if e.suggestion != c.Suggestion || e.docs != c.Docs() {
		t.Fatalf("defaults not applied: suggestion=%q docs=%q", e.suggestion, e.docs)
	}
	if e.HTTPStatus() != 409 { // 文档显式：前哨 409
		t.Fatalf("HTTPStatus = %d, want 409", e.HTTPStatus())
	}
	if e.Error() != "E_VOLUME_NODE_MISMATCH: volume data node ≠ target node (volume=v_abc)" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

// TestFluentFields：stage/deployment_id/context/cause 链式附加。
func TestFluentFields(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:2377: connect refused")
	e := New("E_RUNTIME_UNAVAILABLE", "substrate unreachable").
		WithStage("deploy").
		WithDeploymentID("d_01J").
		WithContext("node", "n_01J").
		WithCause(cause)
	env := e.Envelope()
	if env.GetStage() != "deploy" || env.GetDeploymentId() != "d_01J" {
		t.Fatalf("envelope = %+v", env)
	}
	if env.GetContext()["node"] != "n_01J" {
		t.Fatalf("context = %v", env.GetContext())
	}
	if !errors.Is(e, cause) {
		t.Fatal("errors.Is must traverse WithCause chain")
	}
}

// TestToFromGRPCStatusRoundTrip：ToGRPCStatus → status detail → FromGRPCStatus
// 还原七字段。
func TestToFromGRPCStatusRoundTrip(t *testing.T) {
	e := New("E_HEALTH_TIMEOUT", "health gate timeout").
		WithStage("releasing").
		WithDeploymentID("d_42").
		WithContext("service", "web").
		WithContext("budget", "300s")
	st := e.ToGRPCStatus()
	if st.Code() != codes.Internal { // 文档未给定 HTTP → 缺省 500 → Internal
		t.Fatalf("grpc code = %s, want Internal", st.Code())
	}
	got, ok := FromGRPCStatus(st)
	if !ok {
		t.Fatal("FromGRPCStatus lost the envelope detail")
	}
	if got.Code() != "E_HEALTH_TIMEOUT" || got.Message() != "health gate timeout" ||
		got.stage != "releasing" || got.deploymentID != "d_42" ||
		got.context["service"] != "web" || got.context["budget"] != "300s" ||
		got.suggestion == "" || got.docs == "" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if !errors.Is(e, got) || !errors.Is(got, e) {
		t.Fatal("errors.Is must match by stable code both ways")
	}
}

// TestFromGRPCStatusWithoutDetail：无 detail → (nil,false)，由调用方走退化信封。
func TestFromGRPCStatusWithoutDetail(t *testing.T) {
	st := status.New(codes.NotFound, "no route")
	if _, ok := FromGRPCStatus(st); ok {
		t.Fatal("plain status must not yield an apperr")
	}
	if _, ok := FromError(errors.New("plain")); ok {
		t.Fatal("non-status error must not yield an apperr")
	}
}

// TestEnvelopeFromGRPCStatusDegraded：兜底取舍——code 留空串、message 保底、
// HTTP 由 grpc code 机械映射。
func TestEnvelopeFromGRPCStatusDegraded(t *testing.T) {
	env, httpStatus := EnvelopeFromGRPCStatus(status.New(codes.NotFound, "missing"))
	if env.GetCode() != "" {
		t.Fatalf("degraded code = %q, want empty string (must not invent out-of-list codes)", env.GetCode())
	}
	if env.GetMessage() != "missing" {
		t.Fatalf("degraded message = %q", env.GetMessage())
	}
	if httpStatus != http.StatusNotFound {
		t.Fatalf("degraded HTTP = %d, want 404", httpStatus)
	}
}

// TestEnvelopeFromGRPCStatusRegistryHTTP：有信封 detail 时 HTTP 按注册表。
func TestEnvelopeFromGRPCStatusRegistryHTTP(t *testing.T) {
	st := New("E_EVENT_CURSOR_EXPIRED", "cursor predates the retention window").ToGRPCStatus()
	env, httpStatus := EnvelopeFromGRPCStatus(st)
	if env.GetCode() != "E_EVENT_CURSOR_EXPIRED" || httpStatus != 410 {
		t.Fatalf("code=%q http=%d, want E_EVENT_CURSOR_EXPIRED/410 (documented explicitly)", env.GetCode(), httpStatus)
	}
}

// TestEnvelopeFromGRPCStatusEmptyCodeDetail X-5：空码 detail 信封（api 层
// 业务冲突通道构造的退化信封，如 conflict() 的 409）——HTTP 按传输 grpc
// code 机械映射（不再被 HTTPStatus 的空码 500 兜底误伤）、message 原文
// 保留（携带 detail 即业务文案，不走 B1 脱敏——脱敏只认「无 detail」形态）。
func TestEnvelopeFromGRPCStatusEmptyCodeDetail(t *testing.T) {
	st := status.New(codes.FailedPrecondition, "app not deletable from current lifecycle: demo")
	withDetail, err := st.WithDetails(&sharedv1.ErrorResponse{
		Message: "app not deletable from current lifecycle: demo",
		Context: map[string]string{"conflict": "app not deletable from current lifecycle: demo"},
	})
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	env, httpStatus := EnvelopeFromGRPCStatus(withDetail)
	if env.GetCode() != "" {
		t.Fatalf("empty-code envelope code = %q, want empty", env.GetCode())
	}
	if httpStatus != http.StatusConflict {
		t.Fatalf("empty-code detail envelope HTTP = %d, want 409 (mechanical mapping from grpc code)", httpStatus)
	}
	if env.GetMessage() != "app not deletable from current lifecycle: demo" {
		t.Fatalf("message = %q, want business copy preserved (X-5)", env.GetMessage())
	}
	if env.GetMessage() == RedactedDegradedMessage {
		t.Fatal("detail envelope must not be redacted (B1 redacts only the no-detail form)")
	}
	// 同码无 detail 形态仍走 B1 脱敏（防线不削弱）。
	redacted, _ := EnvelopeFromGRPCStatus(status.New(codes.FailedPrecondition, "raw sql SELECT"))
	if redacted.GetMessage() != RedactedDegradedMessage {
		t.Fatal("no-detail FailedPrecondition must stay redacted (B1 defense line)")
	}
}

// TestCodeMappingsSanity：HTTP↔gRPC 机械映射抽检（逆向为多对一：
// 409/410 都映射 FailedPrecondition，再逆向回 409 属预期，细分由信封
// code 表达）。
func TestCodeMappingsSanity(t *testing.T) {
	pairs := []struct {
		httpStatus int
		code       codes.Code
	}{
		{400, codes.InvalidArgument},
		{422, codes.InvalidArgument},
		{409, codes.FailedPrecondition},
		{410, codes.FailedPrecondition},
		{413, codes.ResourceExhausted},
		{503, codes.Unavailable},
		{500, codes.Internal},
	}
	for _, p := range pairs {
		if got := HTTPToGRPCCode(p.httpStatus); got != p.code {
			t.Errorf("HTTPToGRPCCode(%d) = %s, want %s", p.httpStatus, got, p.code)
		}
	}
	if GRPCCodeToHTTP(codes.Unavailable) != 503 || GRPCCodeToHTTP(codes.InvalidArgument) != 400 {
		t.Fatal("reverse mapping sanity failed")
	}
}
