package ingress

// DNS-01 通配证书链测试（B 线 W5 设计 §3.2，W5-S3；实现票 §7 口径——ACME
// 面不依赖真实 CA：用假 challenge 驱动 Present/CleanUp 生命周期断言 +
// 域名集变化重签判据测试）：
//   - dns01ChallengeAdapter：lego 回调 (domain, token, keyAuth) → 插件
//     Present/CleanUp 的 fqdn/value 求形断言（dns01.GetChallengeInfo 同式）；
//   - PlatformDomains 期望集 wildcard 分支（D-V3W5-4 四条形态）；
//   - 域名集变化即重签：wildcard on → 四元通配集；off → 回三 SAN（复用
//     needsRenewal 既有判据，零特判）；
//   - 失败语义：wildcard=true 而插件缝未装配/设置不可读 → 签发显式失败，
//     不回落 HTTP-01。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/registration"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// fakeAcmeDNS 是 acmedns.Provider 的记录型假件（capture Present/CleanUp
// 的调用序列与参数——适配层求形断言的对照面）。
type fakeAcmeDNS struct {
	mu      sync.Mutex
	present []fakeDNSCall
	cleanup []fakeDNSCall
	// presentErr / cleanupErr 非 nil 时对应调用返回错误（失败分支）。
	presentErr error
	cleanupErr error
}

type fakeDNSCall struct {
	fqdn  string
	value string
}

func (f *fakeAcmeDNS) Present(_ context.Context, fqdn, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.present = append(f.present, fakeDNSCall{fqdn: fqdn, value: value})
	return f.presentErr
}

func (f *fakeAcmeDNS) CleanUp(_ context.Context, fqdn, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanup = append(f.cleanup, fakeDNSCall{fqdn: fqdn, value: value})
	return f.cleanupErr
}

// TestDNS01AdapterLifecycle 假 challenge 驱动 Present/CleanUp 生命周期：
// lego 形态的 (domain, token, keyAuth) 回调 → 插件收到
// "_acme-challenge.<domain>"（无尾点）与 keyAuth 的 digest（与
// dns01.GetChallengeInfo 同式），CleanUp 同名同值（幂等清理契约）。
func TestDNS01AdapterLifecycle(t *testing.T) {
	fake := &fakeAcmeDNS{}
	adapter := &dns01ChallengeAdapter{provider: fake}

	const domain = "console.example.test"
	const token = "challenge-token"
	keyAuth := token + ".key-auth-payload"
	info := dns01.GetChallengeInfo(domain, keyAuth)

	if err := adapter.Present(domain, token, keyAuth); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if len(fake.present) != 1 {
		t.Fatalf("present calls = %d, want 1", len(fake.present))
	}
	call := fake.present[0]
	if call.fqdn != "_acme-challenge."+domain {
		t.Fatalf("present fqdn = %q, want _acme-challenge.%s (trailing dot trimmed)", call.fqdn, domain)
	}
	if call.value != info.Value {
		t.Fatalf("present value = %q, want the dns01 digest %q", call.value, info.Value)
	}
	if call.value == keyAuth {
		t.Fatal("dns-01 TXT value must be the keyAuth digest, not the keyAuth itself")
	}

	if err := adapter.CleanUp(domain, token, keyAuth); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if len(fake.cleanup) != 1 || fake.cleanup[0] != call {
		t.Fatalf("cleanup calls = %+v, want one call matching the present (fqdn,value)", fake.cleanup)
	}
}

// TestDNS01AdapterErrorSurface 插件错误如实上抛（lego 上报 challenge 失败
// 的输入面——失败形态统一为 cert 审计 error 行）。
func TestDNS01AdapterErrorSurface(t *testing.T) {
	want := errors.New("provider boom")
	adapter := &dns01ChallengeAdapter{provider: &fakeAcmeDNS{presentErr: want}}
	if err := adapter.Present("example.test", "tok", "ka"); !errors.Is(err, want) {
		t.Fatalf("present error = %v, want the provider error", err)
	}
	adapter2 := &dns01ChallengeAdapter{provider: &fakeAcmeDNS{cleanupErr: want}}
	if err := adapter2.CleanUp("example.test", "tok", "ka"); !errors.Is(err, want) {
		t.Fatalf("cleanup error = %v, want the provider error", err)
	}
}

// TestPlatformDomainsWildcardBranch 期望域名集四形态（D-V3W5-4）：缺省三
// SAN；s3 公网 +s3.<base>；wildcard=true 四元通配集（通配收编 s3.<base>，
// 不再单列）；wildcard 与 s3 公网同开仍取通配集。
func TestPlatformDomainsWildcardBranch(t *testing.T) {
	ctx := context.Background()

	m, _, st, _ := newPlatformTestManager(t, "example.test")

	// ① 缺省（无设置）：三 SAN。
	got, err := m.platformDomainsWithS3(ctx)
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if want := []string{"ctrl.example.test", "registry.example.test", "console.example.test"}; !sameDomainSet(got, want) {
		t.Fatalf("default set = %v, want %v", got, want)
	}

	// ② wildcard=true：[*.base, console, ctrl, registry]。
	if err := st.SaveAcmeSettings(ctx, state.AcmeSettings{
		DNSProvider: state.AcmeDNSProviderDNSPod, CredentialsCipher: "ct", Wildcard: true,
	}, state.AcmeSaveOptions{BaseDomain: "example.test", Actor: "test"}); err != nil {
		t.Fatalf("save wildcard settings: %v", err)
	}
	got, err = m.platformDomainsWithS3(ctx)
	if err != nil {
		t.Fatalf("wildcard: %v", err)
	}
	want := []string{"*.example.test", "console.example.test", "ctrl.example.test", "registry.example.test"}
	if !sameDomainSet(got, want) {
		t.Fatalf("wildcard set = %v, want %v", got, want)
	}

	// ③ wildcard=true + s3 公网同开：通配收编 s3.<base>，仍四元。
	if err := st.SaveS3Settings(ctx, state.S3Settings{
		Mode: state.S3ModeRustfs, PublicExposed: true,
	}, state.S3SaveOptions{BaseDomain: "example.test", Actor: "test"}); err != nil {
		t.Fatalf("save s3 settings: %v", err)
	}
	got, err = m.platformDomainsWithS3(ctx)
	if err != nil {
		t.Fatalf("wildcard+s3: %v", err)
	}
	if !sameDomainSet(got, want) {
		t.Fatalf("wildcard+s3 set = %v, want %v (wildcard subsumes s3.<base>)", got, want)
	}

	// ④ wildcard=false：回三 SAN + s3.<base>（原形态）。
	if err := st.SaveAcmeSettings(ctx, state.AcmeSettings{
		DNSProvider: state.AcmeDNSProviderDNSPod, Wildcard: false,
	}, state.AcmeSaveOptions{BaseDomain: "example.test", Actor: "test"}); err != nil {
		t.Fatalf("save wildcard off: %v", err)
	}
	got, err = m.platformDomainsWithS3(ctx)
	if err != nil {
		t.Fatalf("s3 only: %v", err)
	}
	if !sameDomainSet(got, []string{
		"ctrl.example.test", "registry.example.test", "console.example.test", "s3.example.test",
	}) {
		t.Fatalf("s3-only set = %v", got)
	}
}

// TestEnsurePlatformCertificateWildcardReissue 域名集变化即重签判据直接复
// 用（实现票口径）：wildcard on → 首签发按四元通配集；off → 域名集变化触
// 发重签回三 SAN——零特判。
func TestEnsurePlatformCertificateWildcardReissue(t *testing.T) {
	ctx := context.Background()
	m, _, st, _ := newPlatformTestManager(t, "example.test")

	var issuedFor [][]string
	m.obtainFn = func(_ context.Context, _ string, _ registration.User, domains []string) ([]byte, []byte, error) {
		issuedFor = append(issuedFor, append([]string{}, domains...))
		certPEM, keyPEM := selfSignedTestCertMultiSAN(t, domains)
		return certPEM, keyPEM, nil
	}

	if err := st.SaveAcmeSettings(ctx, state.AcmeSettings{
		DNSProvider: state.AcmeDNSProviderCloudflare, CredentialsCipher: "ct", Wildcard: true,
	}, state.AcmeSaveOptions{BaseDomain: "example.test", Actor: "test"}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	pair, err := m.ensurePlatformCertificate(ctx, false)
	if err != nil {
		t.Fatalf("ensure (wildcard on): %v", err)
	}
	if len(issuedFor) != 1 || !sameDomainSet(issuedFor[0], []string{
		"*.example.test", "console.example.test", "ctrl.example.test", "registry.example.test",
	}) {
		t.Fatalf("first issue domains = %v, want the wildcard set", issuedFor)
	}
	if !hasWildcardSAN(pair.Domains) {
		t.Fatalf("certificate must carry the wildcard SAN: %v", pair.Domains)
	}

	// 关闭开关：期望集回三 SAN → needsRenewal 域名集变化判据触发重签。
	if err := st.SaveAcmeSettings(ctx, state.AcmeSettings{
		DNSProvider: state.AcmeDNSProviderCloudflare, Wildcard: false,
	}, state.AcmeSaveOptions{BaseDomain: "example.test", Actor: "test"}); err != nil {
		t.Fatalf("save wildcard off: %v", err)
	}
	pair2, err := m.ensurePlatformCertificate(ctx, true)
	if err != nil {
		t.Fatalf("ensure (wildcard off): %v", err)
	}
	if len(issuedFor) != 2 {
		t.Fatalf("obtain calls = %d, want 2 (domain-set change must reissue)", len(issuedFor))
	}
	if !sameDomainSet(pair2.Domains, []string{
		"ctrl.example.test", "registry.example.test", "console.example.test",
	}) {
		t.Fatalf("reissue domains = %v, want the base three-SAN set", pair2.Domains)
	}

	// 稳态：无设置变化不重签（域名集一致 + 未进续期窗）。
	if _, err := m.ensurePlatformCertificate(ctx, true); err != nil {
		t.Fatalf("steady state: %v", err)
	}
	if len(issuedFor) != 2 {
		t.Fatalf("obtain calls = %d, want 2 (steady state must not reissue)", len(issuedFor))
	}
}

// TestDNS01BranchFailsHonest 失败语义：wildcard=true 时 obtainFn（生产 =
// legoObtain 的 DNS-01 分支前置决策）要求插件缝就绪——dnsProviderFn 未装
// 配 → resolveDNSProvider 显式错误；设置不可读 → dns01ForPlatform 显式错
// 误（均不回落 HTTP-01）。
func TestDNS01BranchFailsHonest(t *testing.T) {
	ctx := context.Background()
	m, _, st, _ := newPlatformTestManager(t, "example.test")

	if err := st.SaveAcmeSettings(ctx, state.AcmeSettings{
		DNSProvider: state.AcmeDNSProviderDNSPod, CredentialsCipher: "ct", Wildcard: true,
	}, state.AcmeSaveOptions{BaseDomain: "example.test", Actor: "test"}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	on, err := m.dns01ForPlatform(ctx)
	if err != nil || !on {
		t.Fatalf("dns01ForPlatform = %v, %v; want true, nil", on, err)
	}
	if _, err := m.resolveDNSProvider(ctx); err == nil ||
		!strings.Contains(err.Error(), "resolver not assembled") {
		t.Fatalf("unresolved seam must fail explicitly, got %v", err)
	}

	// 设置不可读 → 决策显式失败（不按 false 静默走 HTTP-01）。
	_ = st.Close()
	if _, err := m.dns01ForPlatform(ctx); err == nil {
		t.Fatal("unreadable settings must fail the dns-01 decision")
	}
}

// TestDNS01OffForAppCertificates 分支适用面：非平台证书的 dns01ForPlatform
// 不适用——app 证书恒 HTTP-01（设计 §3.2：自定义域名场景保留）。决策函数
// 只被平台证书路径调用；此处以 wildcard=false 形态钉住平台路径的另一半。
func TestDNS01OffForAppCertificates(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := newPlatformTestManager(t, "example.test")
	on, err := m.dns01ForPlatform(ctx)
	if err != nil || on {
		t.Fatalf("dns01ForPlatform (no settings) = %v, %v; want false, nil", on, err)
	}
}

func hasWildcardSAN(domains []string) bool {
	for _, d := range domains {
		if strings.HasPrefix(d, "*.") {
			return true
		}
	}
	return false
}
