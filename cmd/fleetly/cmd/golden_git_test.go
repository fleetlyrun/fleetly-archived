package cmd

// golden 快照（T2.19 动词面）：apps webhook 配置生命周期。（git keys /
// git fingerprint 用例随 git push(SSH) 收包面 2026-09-29 移除退役。）
// 夹具与归一化复用 golden_test.go 的框架（startCLI/compareGolden/normalize）
// ——`go test ./cmd/fleetly/cmd -run TestGolden -update` 再生成。

import (
	"strings"
	"testing"
)

// TestGoldenAppsWebhook webhook 配置面：set-secret → show → set-source →
// show（无敏感投影——secret 只回 configured 位）。
func TestGoldenAppsWebhook(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")

	code, out, errOut := runCLIConn(t, "apps", "webhook", "set-secret", "--json", "my-api", "a-webhook-secret-16ch")
	if code != 0 {
		t.Fatalf("set-secret: code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "apps_webhook_set_secret", out)

	code, out, _ = runCLIConn(t, "apps", "webhook", "show", "--json", "my-api")
	if code != 0 {
		t.Fatalf("show: code=%d", code)
	}
	compareGolden(t, "apps_webhook_show", out)

	code, out, errOut = runCLIConn(t, "apps", "webhook", "set-source", "--json",
		"--branch", "main", "--auth-kind", "none", "my-api", "https://example.com/acme/web.git")
	if code != 0 {
		t.Fatalf("set-source: code=%d stderr=%s", code, errOut)
	}
	compareGolden(t, "apps_webhook_set_source", out)

	// 弱 secret 服务端拒绝（≥16 字符——验签是端点唯一认证）。
	code, _, errOut = runCLIConn(t, "apps", "webhook", "set-secret", "--json", "my-api", "short")
	if code != 1 || !strings.Contains(errOut, "16") {
		t.Fatalf("weak secret: code=%d stderr=%q", code, errOut)
	}
}
