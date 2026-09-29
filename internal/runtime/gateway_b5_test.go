package runtime

// B5 批次评审整改的 fleetlyd 装配面回归：
//   - H7 REST 面请求体上限（root handler 最外层，鉴权/解码前置 413）；
//   - M4-3 lynxhttp WriteTimeout 调优钩子（流式端点 15min，读侧保持紧）。
//   （M4-2 GitKeysService gateway 注册用例随 git push 面 2026-09-29 移除。）

import (
	"bytes"
	"encoding/json"
	"io"
	gohttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGatewayRequestBodyLimit H7：超限请求体在分派（webhook/console/
// gateway 解码与鉴权）之前被 413 拦截——无 token 亦然（防线先于鉴权），
// 且后端 fallback handler 从未被触达；声明 Content-Length 超限走预检短路，
// 谎报/缺省长度（chunked）经 MaxBytesReader 在读取时截断。
func TestGatewayRequestBodyLimit(t *testing.T) {
	reached := false
	fallback := gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		reached = true
		// 模拟 gateway 面读 body 的行为：MaxBytesReader 超限在 Read 处报错
		//（真实链路由 marshaler 解码时消费——此处断言读取面被截断即可）。
		buf := make([]byte, 64)
		if _, err := r.Body.Read(buf); err == nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.WriteHeader(gohttp.StatusOK)
	})
	root := newRootHandler(gohttp.NotFoundHandler(), nil, nil, nil, fallback)

	// 面 1：声明超限 Content-Length 的 POST /v1/**（无 token）→ 413，先于
	// fallback（鉴权与解码都在其后）。
	oversized := bytes.Repeat([]byte("x"), maxRequestBodyBytes+1)
	req := httptest.NewRequest(gohttp.MethodPost, "/v1/apps", bytes.NewReader(oversized))
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, req)
	if rec.Code != gohttp.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", rec.Code)
	}
	if reached {
		t.Fatal("oversized request must be rejected before reaching gateway handler (H7 auth runs first)")
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("413 envelope not JSON: %s", rec.Body.String())
	}
	if _, has := env["message"]; !has {
		t.Fatalf("413 envelope missing message: %s", rec.Body.String())
	}

	// 面 2：chunked（无 Content-Length）超限体——进入 fallback（分派正常），
	// 但 body 读取被 MaxBytesReader 截断（错误而非吞完 33MiB）。
	reached = false
	var readErr error
	fallback2 := gohttp.HandlerFunc(func(w gohttp.ResponseWriter, r *gohttp.Request) {
		reached = true
		_, readErr = io.Copy(io.Discard, r.Body)
		w.WriteHeader(gohttp.StatusOK)
	})
	root2 := newRootHandler(gohttp.NotFoundHandler(), nil, nil, nil, fallback2)
	req2 := httptest.NewRequest(gohttp.MethodPost, "/v1/apps", io.MultiReader(
		bytes.NewReader(oversized), // 超限体
		strings.NewReader("tail"))) // 确保总长 > 上限
	req2.ContentLength = -1 // httptest 对未知 reader 类型缺省即此（chunked 形态）
	rec2 := httptest.NewRecorder()
	root2.ServeHTTP(rec2, req2)
	if !reached {
		t.Fatal("chunked request should dispatch to gateway (the preflight does not block undeclared lengths)")
	}
	if readErr == nil || !strings.Contains(readErr.Error(), "too large") {
		t.Fatalf("chunked oversized body read err = %v, want MaxBytesReader truncation", readErr)
	}

	// 面 3：合法小请求体不受影响（透传 fallback）。
	reached = false
	req3 := httptest.NewRequest(gohttp.MethodPost, "/v1/apps", strings.NewReader(`{"compose":"x"}`))
	rec3 := httptest.NewRecorder()
	root2.ServeHTTP(rec3, req3)
	if rec3.Code != gohttp.StatusOK || !reached {
		t.Fatalf("normal body status = %d reached=%v, want 200/true", rec3.Code, reached)
	}
}

// TestTuneHTTPServerWriteTimeout M4-3：调优钩子只放宽 WriteTimeout 到
// 15min（lynx 缺省 60s 会静默掐断 /v1/events/stream 与 logs stream），读侧
// （ReadHeaderTimeout/ReadTimeout）保持 lynx 缺省紧口径不动。lynxhttp 在
// 内部超时之后应用 ServerOptions（server.go Start），本用例按同序构造后
// 断言终态。
func TestTuneHTTPServerWriteTimeout(t *testing.T) {
	srv := &gohttp.Server{
		ReadHeaderTimeout: 60 * time.Second, // lynx DefaultTimeout 缺省形态
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	tuneHTTPServer(srv)
	if srv.WriteTimeout < 15*time.Minute {
		t.Fatalf("WriteTimeout = %v, want >= 15min (M4-3 streaming endpoints)", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != 60*time.Second || srv.ReadTimeout != 60*time.Second {
		t.Fatalf("read-side timeouts must stay tight: header=%v read=%v",
			srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}
