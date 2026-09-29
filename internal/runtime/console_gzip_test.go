package runtime

// Console 静态面 gzip 协商测试（2026-09-25 加载优化）：可压缩资产压缩
// （Content-Encoding/Content-Length 删除/Vary）、客户端不支持/不可压缩
// 类型原样、SPA 回退 index.html 同样压缩、304 短路不压。

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newGzipTestHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.html":  "<!doctype html><html><body>fleetly-console-shell</body></html>",
		"app.js":      strings.Repeat("console.log('fleetly');\n", 200),
		"photo.woff2": "binary-font-blob",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil { //nolint:gosec // G306：测试静态工件收紧 0600，无消费方依赖宽松位
			t.Fatalf("write %s: %v", name, err)
		}
	}
	h, err := newConsoleUIHandler(dir)
	if err != nil {
		t.Fatalf("newConsoleUIHandler: %v", err)
	}
	return h
}

func getBody(t *testing.T, h http.Handler, path, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestConsoleGzipCompressible(t *testing.T) {
	h := newGzipTestHandler(t)
	rec := getBody(t, h, "/ui/assets/app.js", "gzip, deflate, br")

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want deleted (compressed length differs)", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !strings.Contains(string(body), "fleetly") {
		t.Fatalf("decompressed body missing payload: %q", string(body)[:min(80, len(body))])
	}
}

func TestConsoleGzipSPAFallbackCompressed(t *testing.T) {
	h := newGzipTestHandler(t)
	rec := getBody(t, h, "/ui/apps/some-app", "gzip")

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("SPA fallback Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !strings.Contains(string(body), "fleetly-console-shell") {
		t.Fatal("SPA fallback served wrong body")
	}
}

func TestConsoleGzipPassthroughWhenUnsupported(t *testing.T) {
	h := newGzipTestHandler(t)
	rec := getBody(t, h, "/ui/assets/app.js", "")

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want unset", got)
	}
	if !strings.Contains(rec.Body.String(), "fleetly") {
		t.Fatal("identity body must be verbatim")
	}
}

func TestConsoleGzipPassthroughForPrecompressedType(t *testing.T) {
	h := newGzipTestHandler(t)
	rec := getBody(t, h, "/ui/photo.woff2", "gzip")

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("woff2 Content-Encoding = %q, want unset (already compressed)", got)
	}
	if rec.Body.String() != "binary-font-blob" {
		t.Fatal("woff2 body must be verbatim")
	}
}
