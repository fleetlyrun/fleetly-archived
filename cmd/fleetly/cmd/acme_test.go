package cmd

// acme 动词组的 CLI 表驱动测试（B 线 W5-S3，D-V3W5-3/D-V3W5-4）：进程内真
// 实服务面夹具（internal/apitest，生产同路径 CLI-over-SDK）驱动 show/
// dns set/dns test/wildcard，断言往返投影、token 的 env 回退（不进 shell
// 历史形态）、留空保留语义、wildcard 联动门信封与期望域名集展示。探针的
// 真实网络面不在 CLI 夹具（internal/acmedns httptest 单测覆盖）——单节点
// 夹具按 D-MN-13 门诚实拒绝。

import (
	"strings"
	"testing"
)

// TestCLIAcmeShowDefaults show 缺省态（人读 + JSON 双形态）：provider=none、
// 无凭证、wildcard off、按域 HTTP-01 证书面注记。
func TestCLIAcmeShowDefaults(t *testing.T) {
	startCLI(t)

	code, out, errOut := runCLIConn(t, "acme", "show")
	if code != 0 {
		t.Fatalf("show: code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{
		"dns provider: none",
		"credentials: not set",
		"wildcard: false",
		"certificate domains: per-domain HTTP-01",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}

	code, out, _ = runCLIConn(t, "acme", "show", "--json")
	// protojson 缺省省略零值字段：缺省态 JSON 只有 dns_provider 行。
	if code != 0 || !strings.Contains(out, `"dns_provider": "none"`) || strings.Contains(out, `"wildcard": true`) {
		t.Fatalf("show json: code=%d out=%s", code, out)
	}
}

// TestCLIAcmeDNSSetAndEnvFallback dns set：provider 必填（64 用法错）；
// --token 明文保存 → 指纹回显 + 响应无明文；env FLEETLY_ACME_DNS_TOKEN 回
// 退（不进 shell 历史）；留空 = 保留已存（指纹不变）；provider=none 清空。
func TestCLIAcmeDNSSetAndEnvFallback(t *testing.T) {
	env := startCLI(t)
	_ = env
	const token = "77,cli-secret-TOKEN-MARKER" //nolint:gosec // G101：测试标记值，非真实凭据

	// provider 缺位 → CLI 侧用法拒绝（64）。
	code, _, errOut := runCLIConn(t, "acme", "dns", "set")
	if code != 64 || !strings.Contains(errOut, "--provider is required") {
		t.Fatalf("set without provider: code=%d stderr=%q", code, errOut)
	}

	// --api-token 保存：指纹回显（16 hex 前 8）、响应零明文、带 test 指引。
	//（注：--token 是 CLI 连接鉴权旗标，服务商凭证旗标为 --api-token。）
	code, out, errOut := runCLIConn(t, "acme", "dns", "set", "--provider", "dnspod", "--api-token", token)
	if code != 0 {
		t.Fatalf("set with token: code=%d stderr=%s", code, errOut)
	}
	if strings.Contains(out, token) {
		t.Fatal("set output must not echo the token plaintext")
	}
	if !strings.Contains(out, "provider=dnspod") || !strings.Contains(out, "token fingerprint=") ||
		!strings.Contains(out, "acme dns test") {
		t.Fatalf("set output missing fingerprint/guidance:\n%s", out)
	}

	// env 回退：token 旗标缺位、env 注入 → 保存生效（轮换语义：指纹变化）。
	t.Setenv("FLEETLY_ACME_DNS_TOKEN", "88,env-fallback-TOKEN")
	code, out, _ = runCLIConn(t, "acme", "dns", "set", "--provider", "dnspod")
	if code != 0 || !strings.Contains(out, "(token source: env FLEETLY_ACME_DNS_TOKEN)") {
		t.Fatalf("set with env fallback: code=%d out=\n%s", code, out)
	}
	if strings.Contains(out, "88,env-fallback-TOKEN") {
		t.Fatal("set output must not echo the env token")
	}

	// 留空保留语义：无 --token 无 env → 密文沿用（指纹与上一轮一致、无
	// token source 注记）。
	t.Setenv("FLEETLY_ACME_DNS_TOKEN", "")
	code, out, _ = runCLIConn(t, "acme", "dns", "set", "--provider", "cloudflare")
	if code != 0 {
		t.Fatalf("set blank-keep: code=%d stderr=%s", code, errOut)
	}
	if strings.Contains(out, "token source:") {
		t.Fatalf("blank save must not claim a token source:\n%s", out)
	}
	if !strings.Contains(out, "token fingerprint=") {
		t.Fatalf("blank save must keep the stored fingerprint:\n%s", out)
	}

	// provider=none 恒清空。
	code, out, _ = runCLIConn(t, "acme", "dns", "set", "--provider", "none")
	if code != 0 || !strings.Contains(out, "provider=none token=not set") {
		t.Fatalf("set none: code=%d out=\n%s", code, out)
	}
}

// TestCLIAcmeWildcardGateAndFlow wildcard 开关全链（base_domain 在位夹具）：
// 无 provider → 服务端 422 信封 E_ACME_WILDCARD_REQUIRES_PROVIDER；dns set
// 后 on 放行 → show 展示四元通配域集；off 回退注记。单节点夹具（base_domain
// 空）→ 409 E_ACME_WILDCARD_REQUIRES_BASE_DOMAIN。
func TestCLIAcmeWildcardGateAndFlow(t *testing.T) {
	// 单节点形态：provider 就位后 wildcard on → 409（E_ACME_WILDCARD_
	// REQUIRES_BASE_DOMAIN——联动门先查 provider、后查 base_domain）。
	startCLI(t)
	code, _, errOut := runCLIConn(t, "acme", "dns", "set", "--provider", "dnspod", "--api-token", "1,t")
	if code != 0 {
		t.Fatalf("dns set (single-node): code=%d stderr=%s", code, errOut)
	}
	code, _, errOut = runCLIConn(t, "acme", "wildcard", "on")
	if code != 1 || !strings.Contains(errOut, "E_ACME_WILDCARD_REQUIRES_BASE_DOMAIN") {
		t.Fatalf("wildcard on single-node: code=%d stderr=%q", code, errOut)
	}

	// 非法词 CLI 侧前置拒绝（64 用法错）。
	code, _, errOut = runCLIConn(t, "acme", "wildcard", "bogus")
	if code != 64 || !strings.Contains(errOut, "expected on|off") {
		t.Fatalf("wildcard bogus: code=%d stderr=%q", code, errOut)
	}

	// 多节点夹具：联动门（无 provider → 422）→ 就位全链。
	startCLIJoin(t)

	code, _, errOut = runCLIConn(t, "acme", "wildcard", "on")
	if code != 1 || !strings.Contains(errOut, "E_ACME_WILDCARD_REQUIRES_PROVIDER") {
		t.Fatalf("wildcard on without provider: code=%d stderr=%q", code, errOut)
	}

	code, _, errOut = runCLIConn(t, "acme", "dns", "set", "--provider", "dnspod", "--api-token", "1,staging-tok")
	if code != 0 {
		t.Fatalf("dns set: code=%d stderr=%s", code, errOut)
	}
	code, out, errOut := runCLIConn(t, "acme", "wildcard", "on")
	if code != 0 {
		t.Fatalf("wildcard on: code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{
		"wildcard on (provider=dnspod)",
		"*.example.test",
		"console.example.test",
		"ctrl.example.test",
		"registry.example.test",
		"app routing is unchanged",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("wildcard on output missing %q:\n%s", want, out)
		}
	}

	// show：通配域集展示（服务端派生实值）+ SNI 覆盖注记。
	code, out, _ = runCLIConn(t, "acme", "show")
	if code != 0 {
		t.Fatalf("show after on: code=%d", code)
	}
	for _, want := range []string{
		"wildcard: true",
		"certificate domains (wildcard set):",
		"per-app HTTP-01 issuance stays for custom domains",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}

	// off：回三 SAN 注记。
	code, out, _ = runCLIConn(t, "acme", "wildcard", "off")
	if code != 0 || !strings.Contains(out, "wildcard off (provider=dnspod)") ||
		!strings.Contains(out, "three-SAN set") {
		t.Fatalf("wildcard off: code=%d out=\n%s", code, out)
	}
}

// TestCLIAcmeDNSTestGates dns test 的门禁形态（探针真实网络面不在 CLI 夹
// 具——internal/acmedns 的 httptest 单测覆盖请求形态）：单节点夹具 →
// E_MULTI_NODE_REQUIRES_BASE_DOMAIN；多节点夹具无已存配置且无候选 → 形状
// 拒绝（InvalidArgument 信封）。
func TestCLIAcmeDNSTestGates(t *testing.T) {
	startCLI(t)
	code, _, errOut := runCLIConn(t, "acme", "dns", "test")
	if code != 1 || !strings.Contains(errOut, "E_MULTI_NODE_REQUIRES_BASE_DOMAIN") {
		t.Fatalf("test single-node: code=%d stderr=%q", code, errOut)
	}

	startCLIJoin(t)
	code, _, errOut = runCLIConn(t, "acme", "dns", "test")
	if code != 1 || !strings.Contains(errOut, "no DNS provider configured yet") {
		t.Fatalf("test without config: code=%d stderr=%q", code, errOut)
	}
}
