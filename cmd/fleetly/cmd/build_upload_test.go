package cmd

// IMPL-T2-2（DT-6）上传构建 CLI 端到端：`builds upload`（上下文 tar 文件 →
// client-streaming → 假执行器收敛）→ `builds get` 按 ID 回读；--name 必填
// （用法 64）与名称小写化契约。

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUploadTarFixture 构造上下文 tar 文件（Dockerfile + app.js）。
func writeUploadTarFixture(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct{ name, body string }{
		{"Dockerfile", "FROM scratch\nCOPY app.js /app.js\n"},
		{"app.js", "console.log('fixture')\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("tar body %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	path := filepath.Join(t.TempDir(), "context.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write tar: %v", err)
	}
	return path
}

// TestBuildsUploadCLIFlow builds upload --json → builds get --json 全链。
func TestBuildsUploadCLIFlow(t *testing.T) {
	startCLIWithBuildQueue(t)
	tarPath := writeUploadTarFixture(t)

	code, out, errOut := runCLIConn(t, "builds", "upload", "--json", "--name", "Demo-App", tarPath)
	if code != 0 {
		t.Fatalf("builds upload: code=%d stderr=%s", code, errOut)
	}
	var rec struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Driver      string `json:"driver"`
		ImageRef    string `json:"image_ref"`
		ImageDigest string `json:"image_digest"`
	}
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("unmarshal upload output %q: %v", out, err)
	}
	if rec.ID == "" || rec.Status != "succeeded" || rec.Driver != "dockerfile" {
		t.Fatalf("upload row = %+v, want succeeded dockerfile row", rec)
	}
	if !strings.HasPrefix(rec.ImageRef, "fleetly-local/demo-app:") {
		t.Fatalf("image_ref = %q, want lowercased fleetly-local/demo-app:*", rec.ImageRef)
	}
	if !strings.HasPrefix(rec.ImageDigest, "sha256:") {
		t.Fatalf("image_digest = %q, want sha256:*", rec.ImageDigest)
	}

	// builds get 回读同一行（上传构建无 app 归属的唯一 CLI 读面）。
	code, out, errOut = runCLIConn(t, "builds", "get", "--json", rec.ID)
	if code != 0 {
		t.Fatalf("builds get: code=%d stderr=%s", code, errOut)
	}
	var got struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		ImageDigest string `json:"image_digest"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal get output %q: %v", out, err)
	}
	if got.ID != rec.ID || got.Status != "succeeded" || got.ImageDigest != rec.ImageDigest {
		t.Fatalf("builds get mismatch: got %+v want id=%s digest=%s", got, rec.ID, rec.ImageDigest)
	}

	// 人读形态：一行摘要含 ref@digest。
	code, out, _ = runCLIConn(t, "builds", "get", rec.ID)
	if code != 0 || !strings.Contains(out, rec.ImageDigest) {
		t.Fatalf("builds get human output = %q (code=%d), want digest", out, code)
	}
}

// TestBuildsUploadCLIUsage --name 必填（用法错误 64）；未知构建 ID 退出 1。
func TestBuildsUploadCLIUsage(t *testing.T) {
	startCLIWithBuildQueue(t)
	tarPath := writeUploadTarFixture(t)

	code, _, errOut := runCLIConn(t, "builds", "upload", tarPath)
	if code != 64 || !strings.Contains(errOut, "--name") {
		t.Fatalf("missing --name: code=%d stderr=%q, want 64 + --name hint", code, errOut)
	}

	code, _, errOut = runCLIConn(t, "builds", "get", "01NOSUCHBUILD000000000000000")
	if code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("unknown build get: code=%d stderr=%q, want 1 + not found", code, errOut)
	}
}
