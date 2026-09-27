package cmd

// 明文配置资源 CLI 测试（T 线 OT-3/IMPL-T1-4）：set（--value / --from-file）
// → ls → get（明文回读）→ rm 全链经 RPC（apitest 装配 ConfigsService），并做
// 明文负面扫描（ls 输出不得含配置内容）与用法错误（值来源二选一）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigsCRUDSurface(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")

	// set --value：回执带字节数与指纹。
	const inlineContent = "listen: 8080"
	code, out, errOut := runCLIConn(t, "configs", "set", "--value", inlineContent, "my-api", "app.yaml")
	if code != 0 {
		t.Fatalf("configs set: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "12 bytes") || !strings.Contains(out, "fingerprint") {
		t.Fatalf("set output = %q", out)
	}

	// set --from-file：多行内容从文件读取（免转义）。
	fileContent := "a: 1\nb: 2\n"
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, []byte(fileContent), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	code, out, errOut = runCLIConn(t, "configs", "set", "--from-file", path, "my-api", "worker.yaml")
	if code != 0 {
		t.Fatalf("configs set --from-file: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "10 bytes") {
		t.Fatalf("set --from-file output = %q", out)
	}

	// ls：名称/指纹投影——内容零出现。
	code, out, errOut = runCLIConn(t, "configs", "ls", "my-api")
	if code != 0 {
		t.Fatalf("configs ls: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "app.yaml") || !strings.Contains(out, "worker.yaml") {
		t.Fatalf("ls output = %q", out)
	}
	if strings.Contains(out, "listen: 8080") || strings.Contains(out, "a: 1") {
		t.Fatalf("ls output leaks config content: %q", out)
	}
	code, out, errOut = runCLIConn(t, "configs", "ls", "--json", "my-api")
	if code != 0 {
		t.Fatalf("configs ls --json: code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{`"name": "app.yaml"`, `"hash8"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls --json missing %s: %s", want, out)
		}
	}

	// get：明文回读逐字（无附加换行）。
	code, out, errOut = runCLIConn(t, "configs", "get", "my-api", "worker.yaml")
	if code != 0 {
		t.Fatalf("configs get: code=%d stderr=%s", code, errOut)
	}
	if out != fileContent {
		t.Fatalf("get output = %q, want the exact plaintext %q", out, fileContent)
	}

	// rm：删除；再 ls 只剩其一。
	code, out, errOut = runCLIConn(t, "configs", "rm", "my-api", "app.yaml")
	if code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("configs rm: code=%d out=%q stderr=%s", code, out, errOut)
	}
	code, out, _ = runCLIConn(t, "configs", "ls", "--json", "my-api")
	if code != 0 || strings.Contains(out, "app.yaml") {
		t.Fatalf("ls after rm = %q (code=%d)", out, code)
	}

	// 用法错误：--value 与 --from-file 恰给其一。
	code, _, errOut = runCLIConn(t, "configs", "set", "my-api", "both.yaml")
	if code == 0 || !strings.Contains(errOut, "exactly one of --value or --from-file") {
		t.Fatalf("no value source: code=%d stderr=%s, want usage error", code, errOut)
	}
	code, _, errOut = runCLIConn(t, "configs", "set", "--value", "v", "--from-file", path, "my-api", "both.yaml")
	if code == 0 || !strings.Contains(errOut, "exactly one of --value or --from-file") {
		t.Fatalf("both value sources: code=%d stderr=%s, want usage error", code, errOut)
	}
}
