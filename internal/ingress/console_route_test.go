package ingress

// platformConsoleRoute 的门控与形态测试（2026-09-24）：base_domain 门 /
// advertise 未定缺席 / 明文与 TLS 两态后端 / withPlatformRoutes 集成。

import (
	"context"
	"strings"
	"testing"
)

func TestPlatformConsoleRoute(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.SetConfigPort(8420)

	// base_domain 空：直访段缺席（ConfigTLSEnEnabled 门——单节点 v0.1 形态
	// 零变化）。
	if _, ok := m.platformConsoleRoute(); ok {
		t.Fatal("console route must be absent without base_domain")
	}
	if got := m.withPlatformRoutes(context.Background(), nil); len(got) != 0 {
		t.Fatalf("platform routes without base_domain = %v, want none", got)
	}

	m.cfg.BaseDomain = "example.test"
	// advertise 未定（EnsureTraefik 未跑过）：无地址可指，不追加——控制
	// 器收敛链在 EnsureTraefik 之后重发布，最终一致。
	if _, ok := m.platformConsoleRoute(); ok {
		t.Fatal("console route must be absent before advertise is known")
	}

	m.advertiseIP = "10.124.0.3"
	r, ok := m.platformConsoleRoute()
	if !ok || r.Name != consoleRouterName || r.Service != "console" {
		t.Fatalf("console route missing: %+v ok=%v", r, ok)
	}
	// 明文形态（ControlGatewayTLS=false）：http 后端、无 transport 覆写、
	// 根路径 302 到 /ui/。网关端口取注入位（ControlGatewayPort）。
	m.cfg.ControlGatewayPort = 8420
	r, _ = m.platformConsoleRoute()
	if r.BackendURL != "http://10.124.0.3:8420" || r.Transport != "" || r.RootRedirect != "/ui/" {
		t.Fatalf("plaintext form = %+v", r)
	}
	if r.App != platformCertApp {
		t.Fatalf("console route app = %q, want platform cert reserved name", r.App)
	}

	// 网关 TLS 形态（ControlGatewayTLS=true，runtime 按控制面 tls.mode 注入）：
	// https 后端 + 跳过服务器认证的专用 transport（IP 端点无 SAN——
	// providerEndpoint F9 修订二同口径）。
	m.cfg.ControlGatewayTLS = true
	r, _ = m.platformConsoleRoute()
	if r.BackendURL != "https://10.124.0.3:8420" || r.Transport != consoleTransportName {
		t.Fatalf("tls form = %+v", r)
	}

	// 端口未注入（零值）：回落 8420。
	m.cfg.ControlGatewayPort = 0
	m.cfg.ControlGatewayTLS = false
	r, _ = m.platformConsoleRoute()
	if r.BackendURL != "http://10.124.0.3:8420" {
		t.Fatalf("fallback port form = %+v", r)
	}

	// withPlatformRoutes 集成：registry 与 console 两段都在（s3 关闭态）。
	got := m.withPlatformRoutes(context.Background(), nil)
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, RegistryServiceName) || !strings.Contains(joined, consoleRouterName) {
		t.Fatalf("platform routes = %v, want registry + console", names)
	}
}
