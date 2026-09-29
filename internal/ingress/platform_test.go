package ingress

// E1-3 平台证书控制器 与 8423 TLS 配置端点测试（E1 多节点设计 §2.4）：
//   - base_domain 非空：控制器签发重试次序（假签发器：先败后成）→ 证书落盘
//     （多 SAN：ctrl/registry/console）→ Traefik provider endpoint 翻转
//     https://ctrl.<base>:8423/configs → PlatformTLSCertificate 可服务；
//   - base_domain 为空：控制器惰性（零签发、零 endpoint 变化）——单节点
//     v0.1 形态逐字等价（金样：既有 gateway/ingress 测试零改动通过）；
//   - TLS 配置端点 handler：token 鉴权沿用、只承载 /configs。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/registration"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// newPlatformTestManager 构造 base_domain 非空 + ACME 启用的测试管理器：
// 注册 sidecar 预置（ensureAccount 不触网）、短重试退避（1ms——重试次序
// 断言不等待真实 30s）。返回 obtainFn 的调用计数器。
func newPlatformTestManager(t *testing.T, baseDomain string) (*Manager, *fakeDocker, *state.Store, *atomic.Int64) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := true
	cfg := Config{
		TokenFile:         filepath.Join(dir, "ingress.token"),
		CertDir:           filepath.Join(dir, "certs"),
		ConfigAdvertiseIP: "127.0.0.1",
		BaseDomain:        baseDomain,
		ACME:              ACMEConfig{Enabled: &enabled, CADirURL: "http://unused.test/dir"},
		RenewBefore:       time.Hour,
	}
	dc := newFakeDocker()
	m := NewManagerWithDocker(cfg, st, dc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.platformRetryInterval = time.Millisecond

	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o750); err != nil {
		t.Fatalf("mkdir cert dir: %v", err)
	}
	sidecar := filepath.Join(dir, "certs", "acme-account.key.json")
	if err := os.WriteFile(sidecar, []byte(`{"uri":"https://ca.test/reg/1"}`), 0o600); err != nil {
		t.Fatalf("seed account sidecar: %v", err)
	}

	var calls atomic.Int64
	m.obtainFn = func(_ context.Context, _ string, _ registration.User, _ []string) ([]byte, []byte, error) {
		calls.Add(1)
		return nil, nil, errors.New("obtain not stubbed in this test")
	}
	return m, dc, st, &calls
}

// selfSignedTestCertMultiSAN 生成多 SAN 自签证书对（平台证书三子域形态）。
func selfSignedTestCertMultiSAN(t *testing.T, domains []string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     append([]string{}, domains...),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// awaitUntil 轮询等待条件成立（控制器是后台 goroutine——以终态轮询驱动次
// 序断言；deadline 内不成立即失败并打印现场）。
func awaitUntil(t *testing.T, what string, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// traefikEndpointArg 从 fake 服务实况提取 provider endpoint 参数值（服务
// 未创建/参数未出现返回空串——轮询等待的谓词形态，不触发 FailNow；经
// serviceState 加锁读——控制器后台 goroutine 并发写）。
func traefikEndpointArg(t *testing.T, dc *fakeDocker) string {
	t.Helper()
	for _, a := range dc.serviceState(IngressServiceName).Args {
		if strings.HasPrefix(a, "--providers.http.endpoint=") {
			return strings.TrimPrefix(a, "--providers.http.endpoint=")
		}
	}
	return ""
}

// TestPlatformDomainsAndTLSEnabled 域名派生与启用门（D-MN-6 多 SAN 一张）。
func TestPlatformDomainsAndTLSEnabled(t *testing.T) {
	m, _, _, _ := newPlatformTestManager(t, "example.test")
	if !m.ConfigTLSEnabled() {
		t.Fatal("base_domain non-empty must enable the TLS config face")
	}
	got := m.PlatformDomains()
	want := []string{"ctrl.example.test", "registry.example.test", "console.example.test"}
	if len(got) != len(want) {
		t.Fatalf("platform domains = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("platform domains = %v, want %v", got, want)
		}
	}
}

// TestPlatformCertControllerRetryThenEndpointSwitch 控制器全序（fake 签发器：先败
// 两次后成）：签发重试（3 次调用）→ 证书落盘（cert_dir 真源 + 多 SAN）→
// endpoint 翻转 https://ctrl.<base>:8423/configs → PlatformTLSCertificate
// 可供握手。翻转前 endpoint 维持 8422 形态（容忍期语义，设计 §2.4 次序②③）。
func TestPlatformCertControllerRetryThenEndpointSwitch(t *testing.T) {
	m, dc, _, calls := newPlatformTestManager(t, "example.test")
	certPEM, keyPEM := selfSignedTestCertMultiSAN(t, m.PlatformDomains())
	m.obtainFn = func(_ context.Context, _ string, _ registration.User, _ []string) ([]byte, []byte, error) {
		if calls.Add(1) <= 2 {
			return nil, nil, errors.New("ca temporarily unavailable")
		}
		return certPEM, keyPEM, nil
	}

	// 证书未就绪：PlatformTLSCertificate 显式不可用。
	if _, err := m.PlatformTLSCertificate(); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("platform cert must be unavailable before issuance, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.runPlatformCertController(ctx)

	// 终态①：恰好 3 次 Obtain（2 败 1 成——重试退避生效、成功即停）。
	awaitUntil(t, "successful issuance after retries", 5*time.Second, func() bool {
		return m.platformCertOnDisk()
	})
	if n := calls.Load(); n != 3 {
		t.Fatalf("obtain calls = %d, want exactly 3 (2 failures + 1 success)", n)
	}
	// 终态②：落盘证书多 SAN 齐全（D-MN-6：一张覆盖三子域）。
	pair, err := m.certs.Load(platformCertApp)
	if err != nil {
		t.Fatalf("load platform cert: %v", err)
	}
	if !sameDomainSet(pair.Domains, m.PlatformDomains()) {
		t.Fatalf("platform cert SANs = %v, want %v", pair.Domains, m.PlatformDomains())
	}
	// 终态③：provider endpoint 翻转（容忍期为 8422 明文——语义对照）。
	awaitUntil(t, "provider endpoint switch to TLS face", 5*time.Second, func() bool {
		return traefikEndpointArg(t, dc) == "https://127.0.0.1:8423/configs"
	})
	// 终态④：TLS 握手材料就绪（GetCertificate 出口形态）。
	cert, err := m.PlatformTLSCertificate()
	if err != nil {
		t.Fatalf("platform TLS certificate: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("tls certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if !sameDomainSet(leaf.DNSNames, m.PlatformDomains()) {
		t.Fatalf("served leaf SANs = %v, want %v", leaf.DNSNames, m.PlatformDomains())
	}
	// 终态⑤（F8，2026-09-21 真机发现钉住）：证书就绪同拍视图已重发布——
	// websecure 证书段在线（内联 PEM），指纹与平台证书一致；不等 12h sweep。
	// await 形态：endpoint 翻转与视图换入是控制器内先后两步（重发布含台账
	// 与设置现读的数次查询），轮询窗口内到达即符合 F8 语义；超时 = 重发布
	// 缺失，F8 回归（S4 注：E3-6 在 publishWithCerts 增加了 s3 设置现读，
	// 拉长了翻转→换入间隙，即时快照假设在本机高频轮询下曾偶发踩空）。
	awaitUntil(t, "F8: view republished with tls.certificates after cert converged", 5*time.Second, func() bool {
		snap, _ := m.vw.snapshot()
		return snap.TLS != nil && len(snap.TLS.Certificates) > 0
	})
	snap, _ := m.vw.snapshot()
	if snap.TLS == nil || len(snap.TLS.Certificates) == 0 {
		t.Fatalf("F8: view carries no tls.certificates after platform cert converged")
	}
	viewFP := pemCertFingerprint(t, []byte(snap.TLS.Certificates[0].CertFile))
	diskSum := sha256.Sum256(cert.Certificate[0])
	if viewFP != hex.EncodeToString(diskSum[:]) {
		t.Fatalf("F8: published view cert fingerprint %s != platform leaf fingerprint %s",
			viewFP, hex.EncodeToString(diskSum[:]))
	}

	// 审计留痕：issued 成功行（失败尝试各带 error 行——审计即事实）。
	rows, err := m.store.RecentAudits(context.Background(), 50)
	if err != nil {
		t.Fatalf("recent audits: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "ingress.cert_issued" && r.Target == "app:"+platformCertApp && r.Result == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("platform cert issuance audit row missing: %+v", rows)
	}
}

// TestProviderEndpointVPCIPForm F9 修订二（2026-09-21 真机）：多节点就绪后
// endpoint = https://<advertise>:8423（VPC IP 直连，公网 8423 零暴露）+
// tls.insecureSkipVerify 参数（IP 端点无 SAN 可校验；传输加密 + token 保
// 留）；容器 spec 不携带 Hosts（Docker 29.8.1 swarm 任务不应用该字段，
// extra_hosts 通道不可靠）。单节点：8422 明文 + 无 skip 参数差异断言由既有
// 金样测试承担。
func TestProviderEndpointVPCIPForm(t *testing.T) {
	m, dc, _, _ := newPlatformTestManager(t, "example.test")
	// 平台证书在盘 → endpoint 就绪形态。
	certPEM, keyPEM := selfSignedTestCertMultiSAN(t, []string{"ctrl.example.test"})
	pair, err := ParsePair(platformCertApp, []string{"ctrl.example.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if err := m.certs.Save(pair); err != nil {
		t.Fatalf("save pair: %v", err)
	}
	if err := m.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	dc.mu.Lock()
	st := dc.services[IngressServiceName]
	dc.mu.Unlock()
	if !st.Exists {
		t.Fatal("ingress service not created")
	}
	if got := traefikEndpointArg(t, dc); got != "https://127.0.0.1:8423/configs" {
		t.Fatalf("endpoint = %s, want https://127.0.0.1:8423/configs (F9: VPC IP form)", got)
	}
	foundSkip := false
	for _, a := range st.Args {
		if a == "--providers.http.tls.insecureSkipVerify=true" {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Fatal("F9: insecureSkipVerify arg missing (IP endpoint cannot SAN-verify)")
	}
	if len(st.Hosts) != 0 {
		t.Fatalf("F9: spec must not rely on extra_hosts (unreliable on 29.8.1 swarm tasks), got %v", st.Hosts)
	}
}

// TestPlatformCertControllerInertWhenBaseDomainEmpty base_domain 为空 = 控制器惰性：
// 零签发、零 endpoint 变化（单节点 v0.1 形态逐字等价——验收 2 的空侧）。
func TestPlatformCertControllerInertWhenBaseDomainEmpty(t *testing.T) {
	m, dc, _, calls := newPlatformTestManager(t, "")
	// 磁盘上预置一张平台证书（最不利形态：证书在盘也不得翻转 endpoint）。
	certPEM, keyPEM := selfSignedTestCertMultiSAN(t, []string{"ctrl.x.test"})
	pair, err := ParsePair(platformCertApp, []string{"ctrl.x.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if err := m.certs.Save(pair); err != nil {
		t.Fatalf("save pair: %v", err)
	}

	// 控制器直接调用即返回（不启动循环、不签发、不收敛）。
	done := make(chan struct{})
	go func() { m.runPlatformCertController(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller must return immediately when base_domain is empty")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("obtain calls = %d, want 0 (controller inert)", n)
	}
	if err := m.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	if got := traefikEndpointArg(t, dc); got != "http://127.0.0.1:8422/configs" {
		t.Fatalf("provider endpoint = %q, want plaintext 8422 form (v0.1 equivalence)", got)
	}
	// 参数形态补钉（8423 硬化票收口，2026-09-21）：insecureSkipVerify 是
	// buildTraefikSpec 的无条件参数——明文形态下 Traefik 容忍（无害）。
	// 该断言把「参数存在」从隐式行为升级为显式契约：硬化票改为按 endpoint
	// 形态条件下发时，此处红即是提示（单节点金样不受影响）。
	st := dc.services[IngressServiceName]
	foundSkip := false
	for _, a := range st.Args {
		if a == "--providers.http.tls.insecureSkipVerify=true" {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Fatal("insecureSkipVerify arg missing in plaintext form (buildTraefikSpec pins it unconditionally; a conditional form must update this test deliberately)")
	}
}

// TestTLSConfigEndpoint TLS 面 handler（E1-3 端口分面）：只承载 /configs
// （挑战面与 /healthz 不在此复制）、token 鉴权沿用、载荷与明文面同源
// （markServed 语义共享——挑战收敛门不因面而异）。
func TestTLSConfigEndpoint(t *testing.T) {
	m, _, _, _ := newPlatformTestManager(t, "example.test")
	handler, err := m.TLSHandler(context.Background())
	if err != nil {
		t.Fatalf("tls handler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// 无 token → 401。
	resp, err := http.Get(srv.URL + "/configs")
	if err != nil {
		t.Fatalf("get /configs: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token /configs = %d, want 401", resp.StatusCode)
	}
	// 对 token → 200 + 合法动态配置 JSON。
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/configs", nil)
	token, terr := m.token(context.Background())
	if terr != nil {
		t.Fatalf("token: %v", terr)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get /configs ok token: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("ok-token /configs = %d, want 200", resp2.StatusCode)
	}
	raw, _ := io.ReadAll(resp2.Body)
	var decoded DynamicConfig
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload must be valid dynamic config JSON: %v", err)
	}
	// /healthz 与挑战路径不在 TLS 面（8422 明文面专属——端口分面表）。
	resp3, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get /healthz: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("TLS face /healthz = %d, want 404 (8422-only facet)", resp3.StatusCode)
	}
	resp4, err := http.Get(srv.URL + "/.well-known/acme-challenge/x")
	if err != nil {
		t.Fatalf("get challenge path: %v", err)
	}
	_ = resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("TLS face challenge path = %d, want 404 (8422-only facet)", resp4.StatusCode)
	}
}

// TestTLSHandshakeGatedByCert 「证书就绪后 8423 才可用」的 TLS 层验证：
// GetCertificate 出口在证书未就绪时握手失败，就绪后握手成功且对端实收
// SAN = 平台三子域（runtime 服务壳装配的即本形态的 tls.Config）。
func TestTLSHandshakeGatedByCert(t *testing.T) {
	m, _, _, _ := newPlatformTestManager(t, "example.test")
	tlsCfg := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return m.PlatformTLSCertificate()
		},
		MinVersion: tls.VersionTLS12,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srvLog := log.New(io.Discard, "", 0) // 未就绪期的握手失败是预期形态，静默
	go func() {
		s := &http.Server{Handler: http.NewServeMux(), ErrorLog: srvLog, ReadHeaderTimeout: 5 * time.Second}
		_ = s.Serve(tls.NewListener(ln, tlsCfg))
	}()

	// 未就绪：握手失败。
	client := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 测试探测面，不做信任判定
	if _, err := tls.Dial("tcp", ln.Addr().String(), client); err == nil {
		t.Fatal("handshake must fail before the platform certificate is issued")
	}

	// 签发后：握手成功 + 对端证书 SAN 齐全。
	certPEM, keyPEM := selfSignedTestCertMultiSAN(t, m.PlatformDomains())
	pair, err := ParsePair(platformCertApp, m.PlatformDomains(), certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if err := m.certs.Save(pair); err != nil {
		t.Fatalf("save pair: %v", err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), client)
	if err != nil {
		t.Fatalf("handshake after issuance: %v", err)
	}
	defer func() { _ = conn.Close() }()
	peer := conn.ConnectionState().PeerCertificates
	if len(peer) == 0 || !sameDomainSet(peer[0].DNSNames, m.PlatformDomains()) {
		t.Fatalf("served cert SANs = %v, want %v", peer[0].DNSNames, m.PlatformDomains())
	}
}

// TestPlatformCertPathsAndTLSName S6 预留钩子（E7 §3.3 参数面，W5-S5 只导
// 出不消费）：路径指向平台证书库的 crt/key 真源文件、TLS 名 = ctrl.<base>；
// base_domain 为空显式报错（与 ConfigTLSEnabled 门禁同口径）。
func TestPlatformCertPathsAndTLSName(t *testing.T) {
	m, _, _, _ := newPlatformTestManager(t, "example.test")
	certFile, keyFile, err := m.PlatformCertPaths()
	if err != nil {
		t.Fatalf("PlatformCertPaths: %v", err)
	}
	wantCert := filepath.Join(m.cfg.CertDir, "_fleetly-platform.crt")
	wantKey := filepath.Join(m.cfg.CertDir, "_fleetly-platform.key")
	if certFile != wantCert || keyFile != wantKey {
		t.Fatalf("platform cert paths = (%s, %s), want (%s, %s)", certFile, keyFile, wantCert, wantKey)
	}
	name, err := m.PlatformTLSName()
	if err != nil {
		t.Fatalf("PlatformTLSName: %v", err)
	}
	if name != "ctrl.example.test" {
		t.Fatalf("platform TLS name = %q, want %q", name, "ctrl.example.test")
	}

	empty, _, _, _ := newPlatformTestManager(t, "")
	if _, _, err := empty.PlatformCertPaths(); err == nil {
		t.Fatal("PlatformCertPaths with empty base_domain must fail")
	}
	if _, err := empty.PlatformTLSName(); err == nil {
		t.Fatal("PlatformTLSName with empty base_domain must fail")
	}
}
