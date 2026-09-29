package ingress

// E3-6 公网子域开关（s3.public_exposed）消费面测试（设计
// docs/design/2026-09-20-object-storage.md §2.6，D-S3-9）：
//   - 视图增/摘：开关往返 s3.<base> 路由进/出动态配置（fake substrate）；
//   - SAN 条件：平台证书期望态集随开关增第四 SAN / 回缩（重签发收敛）；
//   - Traefik 网络清单条件：fleetly-rustfs-net 挂接/摘除（app 网络保留）；
//   - F10 同型回归：开关往返不得擦既有路由/TLS 段；
//   - 常量不变量：路由键/端口与 internal/rustfs 的服务事实互钉。

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/registration"

	"github.com/fleetlyrun/fleetly/internal/rustfs"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// newS3PublicTestManager 构造 base_domain 非空 + ACME 启用 + 短退避/短扫描
// 的测试管理器（公网开关控制器 的收敛断言不等待真实周期）。返回 obtainFn
// 的调用计数器（签发计数断言：SAN 集增/缩各触发一次重签发）。
func newS3PublicTestManager(t *testing.T, baseDomain string) (*Manager, *fakeDocker, *state.Store, *atomic.Int64) {
	t.Helper()
	m, dc, st, calls := newPlatformTestManager(t, baseDomain)
	m.s3PublicScanInterval = time.Millisecond
	return m, dc, st, calls
}

// setS3Settings 保存一拍 s3 设置（公网开关测试的唯一写入口；走真实校验面
// ——public_exposed=true 的 rustfs 门禁在保存面，消费面默认输入合法态）。
func setS3Settings(t *testing.T, st *state.Store, baseDomain, mode string, exposed bool) {
	t.Helper()
	err := st.SaveS3Settings(context.Background(), state.S3Settings{
		Mode:          mode,
		PublicExposed: exposed,
	}, state.S3SaveOptions{BaseDomain: baseDomain, Actor: "test"})
	if err != nil {
		t.Fatalf("save s3 settings (mode=%s exposed=%t): %v", mode, exposed, err)
	}
}

// hasRouterKey 视图快照键存在性。
func hasRouterKey(t *testing.T, m *Manager, key string) bool {
	t.Helper()
	snap, _ := m.vw.snapshot()
	_, ok := snap.HTTP.Routers[key]
	return ok
}

// traefikHasNetwork 服务实况网络集含指定网络 ID（ID 形态——swarm 归一口径）。
func traefikHasNetwork(dc *fakeDocker, netID string) bool {
	for _, n := range dc.serviceState(IngressServiceName).Networks {
		if n == netID {
			return true
		}
	}
	return false
}

// certDomainsOnDisk 落盘平台证书的 SAN 集（空集 = 未签发）。
func certDomainsOnDisk(t *testing.T, m *Manager) []string {
	t.Helper()
	pair, err := m.certs.Load(platformCertApp)
	if err != nil {
		return nil
	}
	return pair.Domains
}

// TestPlatformS3RouteViewToggle 视图增/摘（fake substrate）：开关开启一次
// 全量发布后 s3.<base> 路由进视图（web/websecure 双入口 + 后端
// fleetly-rustfs:9000），关闭后摘除；路由键与后端公式和 registry 平台段同
// 构（Name 覆写）。已发布开关关闭态与未设置态视图逐键一致（幂等收敛）。
func TestPlatformS3RouteViewToggle(t *testing.T) {
	m, _, st, _ := newS3PublicTestManager(t, "example.test")
	ctx := context.Background()

	// 未设置（mode=unset）：无 s3 路由。
	if err := m.publishWithCerts(ctx); err != nil {
		t.Fatalf("publish (unset): %v", err)
	}
	if hasRouterKey(t, m, "fleetly-rustfs-web") {
		t.Fatal("s3 route must be absent when s3.mode=unset")
	}

	// rustfs + public_exposed=false：仍无路由（开关默认关）。
	setS3Settings(t, st, "example.test", state.S3ModeRustfs, false)
	if err := m.publishWithCerts(ctx); err != nil {
		t.Fatalf("publish (exposed=false): %v", err)
	}
	if hasRouterKey(t, m, "fleetly-rustfs-web") {
		t.Fatal("s3 route must be absent when public_exposed=false")
	}

	// 开启：路由进视图（web + websecure；websecure 的 TLS 位由平台证书挂载
	// 循环承载，本测试无平台证书 → 纯 HTTP 形态——证书联动在 控制器测试）。
	setS3Settings(t, st, "example.test", state.S3ModeRustfs, true)
	if err := m.publishWithCerts(ctx); err != nil {
		t.Fatalf("publish (exposed=true): %v", err)
	}
	snap, _ := m.vw.snapshot()
	rWeb := snap.HTTP.Routers["fleetly-rustfs-web"]
	if rWeb == nil || rWeb.Rule != "Host(`s3.example.test`)" {
		t.Fatalf("s3 web router = %+v, want Host(`s3.example.test`)", rWeb)
	}
	svc := snap.HTTP.Services["fleetly-rustfs"]
	if svc == nil || len(svc.LoadBalancer.Servers) != 1 ||
		svc.LoadBalancer.Servers[0].URL != "http://fleetly-rustfs:9000" {
		t.Fatalf("s3 backend service = %+v, want single server http://fleetly-rustfs:9000", svc)
	}

	// 关闭：路由摘除（全量视图换入的自然收敛——不存在残留键）。
	setS3Settings(t, st, "example.test", state.S3ModeRustfs, false)
	if err := m.publishWithCerts(ctx); err != nil {
		t.Fatalf("publish (exposed=false again): %v", err)
	}
	if hasRouterKey(t, m, "fleetly-rustfs-web") || hasRouterKey(t, m, "fleetly-rustfs-websecure") {
		t.Fatal("s3 route must be withdrawn after toggling off")
	}
}

// TestPlatformDomainsS3Conditional SAN 期望态集条件增减：关闭 = 基础三
// SAN；开启 = +s3.<base>（四 SAN）；mode≠rustfs 恒基础集（external 端点
// 本就在公网，§2.6）。
func TestPlatformDomainsS3Conditional(t *testing.T) {
	m, _, st, _ := newS3PublicTestManager(t, "example.test")
	ctx := context.Background()

	base := []string{"ctrl.example.test", "registry.example.test", "console.example.test"}
	got, err := m.platformDomainsWithS3(ctx)
	if err != nil || !sameDomainSet(got, base) {
		t.Fatalf("default domains = %v (%v), want the three-SAN base set", got, err)
	}

	setS3Settings(t, st, "example.test", state.S3ModeRustfs, true)
	got, err = m.platformDomainsWithS3(ctx)
	want4 := append(append([]string{}, base...), "s3.example.test")
	if err != nil || !sameDomainSet(got, want4) {
		t.Fatalf("exposed domains = %v (%v), want base + s3.example.test", got, err)
	}

	// mode 离开 rustfs（unset 形态）：恒基础集（无公网 SAN）。
	setS3Settings(t, st, "example.test", state.S3ModeUnset, false)
	got, err = m.platformDomainsWithS3(ctx)
	if err != nil || !sameDomainSet(got, base) {
		t.Fatalf("unset-mode domains = %v (%v), want the base set (no s3 SAN)", got, err)
	}

	// SAN 集增/缩都触发重签发判定（needsRenewal 的域名集变化语义——重签发
	// 收敛的机制锚）。
	now := m.nowFunc()
	cert4 := &CertificatePair{Domains: want4, NotAfter: now.Add(90 * 24 * time.Hour)}
	if !needsRenewal(cert4, base, now, m.cfg.RenewBefore) {
		t.Fatal("SAN shrink (4 -> 3) must trigger re-issue")
	}
	if !needsRenewal(&CertificatePair{Domains: base, NotAfter: now.Add(90 * 24 * time.Hour)}, want4, now, m.cfg.RenewBefore) {
		t.Fatal("SAN growth (3 -> 4) must trigger re-issue")
	}
}

// TestS3PublicControllerConvergesToggle 开关往返全链（控制器驱动，fake substrate）：
// 开启 → Traefik 挂 fleetly-rustfs-net、s3.<base> 进视图、平台证书按四 SAN
// 重签发；关闭 → 网络摘除（app 网络保留）、路由摘除、证书按三 SAN 重签。
// F10 同型回归：往返全程既有 app 的 websecure 路由与 tls.certificates 段
// 恒在（全量视图换入不得擦 TLS——publish 恒带证书段）。
func TestS3PublicControllerConvergesToggle(t *testing.T) {
	m, dc, st, calls := newS3PublicTestManager(t, "example.test")
	ctx := context.Background()

	// 前置：一个带域名 app + 在盘证书（既有 TLS 面——往返回归的被保护对象）。
	app, err := testsupport.SeedAppE(t, st, "shop")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	certPEM, keyPEM, _ := selfSignedTestCert(t, "shop.example.test")
	appPair, err := ParsePair("shop", []string{"shop.example.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse app pair: %v", err)
	}
	if err := m.certs.Save(appPair); err != nil {
		t.Fatalf("save app pair: %v", err)
	}
	if err := m.PublishRoutes(ctx, PublishInput{AppID: app.ID, AppName: "shop", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "80", Domains: []string{"shop.example.test"}},
	}}); err != nil {
		t.Fatalf("publish app: %v", err)
	}

	// 假签发器：按请求的域名集出多 SAN 自签证书（计数断言 SAN 增/缩的收敛
	// 与稳态停机——见测试尾部对计数的窗口断言与平台注记）。
	m.obtainFn = func(_ context.Context, _ string, _ registration.User, domains []string) ([]byte, []byte, error) {
		calls.Add(1)
		certPEM, keyPEM := selfSignedTestCertMultiSAN(t, domains)
		return certPEM, keyPEM, nil
	}

	// 开启（rustfs 收敛循环之外本测试只关心 ingress 面——rustfs 网络由 fake 的
	// NetworkEnsure/NetworkID 语义直接可解析，与「rustfs 收敛循环已建网」同构）。
	setS3Settings(t, st, "example.test", state.S3ModeRustfs, true)
	go m.runS3PublicController(ctx)

	awaitUntil(t, "s3 route in view after enable", 5*time.Second, func() bool {
		return hasRouterKey(t, m, "fleetly-rustfs-websecure")
	})
	awaitUntil(t, "traefik attached to rustfs network", 5*time.Second, func() bool {
		return traefikHasNetwork(dc, "netid-"+state.RustfsNetworkName)
	})
	awaitUntil(t, "platform cert reissued with 4 SANs", 5*time.Second, func() bool {
		domains := certDomainsOnDisk(t, m)
		return sameDomainSet(domains, append(m.PlatformDomains(), "s3.example.test"))
	})
	snapOn, _ := m.vw.snapshot()
	if r := snapOn.HTTP.Routers["fleetly-rustfs-websecure"]; r == nil || r.TLS == nil {
		t.Fatalf("exposed s3 route must be websecure+TLS once the platform cert exists: %+v", r)
	}
	if n := calls.Load(); n < 1 {
		t.Fatalf("obtain calls after enable = %d, want >= 1 (SAN growth re-issue)", n)
	}

	assertShopTLSIntact := func(stage string) {
		t.Helper()
		snap, _ := m.vw.snapshot()
		if snap.TLS == nil || len(snap.TLS.Certificates) == 0 {
			t.Fatalf("%s: tls.certificates wiped (F10 regression)", stage)
		}
		if !hasRouterKey(t, m, "fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-shop-web-websecure") {
			t.Fatalf("%s: existing app websecure router wiped (F10 regression)", stage)
		}
	}
	assertShopTLSIntact("after enable")

	// 关闭：路由/网络摘除、证书回缩三 SAN、app 面原样。
	setS3Settings(t, st, "example.test", state.S3ModeRustfs, false)
	awaitUntil(t, "s3 route withdrawn after disable", 5*time.Second, func() bool {
		return !hasRouterKey(t, m, "fleetly-rustfs-websecure")
	})
	awaitUntil(t, "traefik detached from rustfs network", 5*time.Second, func() bool {
		return !traefikHasNetwork(dc, "netid-"+state.RustfsNetworkName)
	})
	awaitUntil(t, "platform cert reissued back to 3 SANs", 5*time.Second, func() bool {
		return sameDomainSet(certDomainsOnDisk(t, m), m.PlatformDomains())
	})
	awaitUntil(t, "app network preserved after detach", 5*time.Second, func() bool {
		shopNet, nerr := appNetworkName(app.TeamSlug, app.ProjectSlug, "shop")
		if nerr != nil {
			t.Fatalf("shop net name: %v", nerr)
		}
		return traefikHasNetwork(dc, "netid-"+shopNet)
	})
	assertShopTLSIntact("after disable")
	if n := calls.Load(); n < 2 {
		t.Fatalf("obtain calls after roundtrip = %d, want >= 2 (growth + shrink)", n)
	}

	// 幂等稳态：状态不再变化 → 签发停止（抽两窗计数相等——重签发收敛有界，
	// 不存在循环重签）。注意签发计数不判精确值：测试的 awaitUntil 轮询
	// （certs.Load）与控制器的原子 Save（tmp+rename）在 Windows 上存在
	// rename-vs-open-reader 的瞬时失败（Access is denied），控制器按既有退避
	// 语义重试会多消耗一次签发——这正是收敛重试的设计行为（Linux 生产/CI
	// 上 rename 对 open reader 原子，无此重试）。
	time.Sleep(150 * time.Millisecond)
	steadyA := calls.Load()
	time.Sleep(150 * time.Millisecond)
	steadyB := calls.Load()
	if steadyA != steadyB {
		t.Fatalf("steady-state re-issue leaked: obtain calls %d -> %d across 150ms (must stop growing)", steadyA, steadyB)
	}
}

// TestS3PublicNetworkAttachDoesNotCreateNetwork 守恒形态：公网开关开启但
// rustfs 网络未建（rustfs 收敛循环未收敛）→ attach 不代建（NetworkEnsure 零
// 调用——fleetly-rustfs-net 的 attachable 形态归 rustfs 收敛循环权威创建），
// 返回可重试错误。
func TestS3PublicNetworkAttachDoesNotCreateNetwork(t *testing.T) {
	m, dc, _, _ := newS3PublicTestManager(t, "example.test")
	ctx := context.Background()
	if err := m.EnsureTraefik(ctx); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	dc.mu.Lock()
	dc.netMissing[state.RustfsNetworkName] = true
	dc.mu.Unlock()
	err := m.attachPlatformNetworkIfPresent(ctx, state.RustfsNetworkName)
	if err == nil {
		t.Fatal("attach must fail (retryable) when the rustfs network is absent")
	}
	if len(dc.netEns) != 0 {
		t.Fatalf("attach must not NetworkEnsure the rustfs net (attachable ownership is the rustfs manager's), got %v", dc.netEns)
	}
}

// TestS3PublicControllerInertSingleNode 单节点形态（base_domain 空）：控制器直接
// 返回（零收敛、零签发）——公网面在单节点不存在（设置面 E_S3_PUBLIC_
// REQUIRES_BASE_DOMAIN 的消费面镜像）。
func TestS3PublicControllerInertSingleNode(t *testing.T) {
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
		BaseDomain:        "",
		ACME:              ACMEConfig{Enabled: &enabled, CADirURL: "http://unused.test/dir"},
	}
	m := NewManagerWithDocker(cfg, st, newFakeDocker(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { m.runS3PublicController(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller must return immediately when base_domain is empty")
	}
}

// TestS3PublicSettingsUnreadableFailsClosed 设置读取故障：全量发布不失败
// （s3 路由缺席 = fail-closed 到内网姿态），其余平台路由照常——设置面故障
// 不放大成全平台入口发布失败。
func TestS3PublicSettingsUnreadableFailsClosed(t *testing.T) {
	m, _, st, _ := newS3PublicTestManager(t, "example.test")
	ctx := context.Background()
	setS3Settings(t, st, "example.test", state.S3ModeRustfs, true)
	app, err := testsupport.SeedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	// 假签发器（demo 域名证书路径——本测试不关心签发计数）。
	m.obtainFn = func(_ context.Context, _ string, _ registration.User, _ []string) ([]byte, []byte, error) {
		certPEM, keyPEM, _ := selfSignedTestCert(t, "demo.example.test")
		return certPEM, keyPEM, nil
	}
	// 先正常发布一次（demo 路由进台账与视图），再注入损坏设置值
	// （布尔位畸形——LoadS3Settings loud-fail 的形态）。
	if err := m.PublishRoutes(ctx, PublishInput{AppID: app.ID, AppName: "demo", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "80", Domains: []string{"demo.example.test"}},
	}}); err != nil {
		t.Fatalf("publish demo: %v", err)
	}
	if err := st.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE platform_settings SET value = 'not-a-bool' WHERE key = ?`, state.S3KeyPublicExposed)
		return err
	}); err != nil {
		t.Fatalf("corrupt setting: %v", err)
	}
	if err := m.publishWithCerts(ctx); err != nil {
		t.Fatalf("publish must survive unreadable s3 settings: %v", err)
	}
	if hasRouterKey(t, m, "fleetly-rustfs-web") {
		t.Fatal("s3 route must be omitted (fail-closed) when settings are unreadable")
	}
	if !hasRouterKey(t, m, "fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-demo-web-web") {
		t.Fatal("app routes must publish normally when s3 settings are unreadable")
	}
	// 控制器对同一故障显式退避（不静默吞掉）。
	if _, err := m.s3PublicExposed(ctx); err == nil {
		t.Fatal("s3PublicExposed must surface the read error (controller retry input)")
	}
}

// TestS3RouteConstantsMatchRustfsFacts 常量不变量：ingress 侧的 rustfs 服
// 务名/端口字面与 internal/rustfs 的权威常量一致（入口层本地重写纪律的
// 互钉——漂移即红）。后端 = 服务名（swarm 服务的 overlay VIP DNS）；内网
// 端点 host 段 = 网络 alias（rustfs，internal/rustfs 测试钉住的不变量）——
// 同一服务的两个 DNS 名，各归其消费面。
func TestS3RouteConstantsMatchRustfsFacts(t *testing.T) {
	if rustfsServiceName != rustfs.ServiceName {
		t.Fatalf("ingress rustfsServiceName = %q, want internal/rustfs.ServiceName %q", rustfsServiceName, rustfs.ServiceName)
	}
	if rustfsBackendPort != "9000" {
		t.Fatalf("rustfsBackendPort = %q, want the RustFS S3 API port 9000", rustfsBackendPort)
	}
	if state.RustfsEndpointURL != "http://rustfs:9000" {
		t.Fatalf("RustfsEndpointURL = %q, want the alias-form internal endpoint http://rustfs:9000", state.RustfsEndpointURL)
	}
}
