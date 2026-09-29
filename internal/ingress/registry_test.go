package ingress

// E1-4 zot 部署器测试（E1 多节点设计 §2.5/D-MN-5）：
//   - 幂等收敛：缺失创建、漂移更新、已收敛 no-op（fake substrate 断言
//     镜像钉版/replicated-1/manager 约束/fleetly-system 网络/卷与只读
//     挂载/零宿主端口）；
//   - 凭据链：auth_file 生成（user:password 单行）→ bcrypt htpasswd 工件
//     （zot 消费面）→ zot 配置工件（鉴权路径/端口），幂等无 churn；
//   - 前置门：swarm 未就绪 / 平台 ID 未铸显式失败（duty 退避收敛面）；
//   - 单节点等价：base_domain 空 = 零部署、零网络、零挂载、duty 不活动；
//   - 路由段：registry.<base> 路由进动态配置（80 恒在；平台证书就绪后
//     443 + 内联证书段）；单节点视图零平台路由；
//   - 镜像钉版缺省与约束公式的字面钉死。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"golang.org/x/crypto/bcrypt"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// newRegistryTestManager 构造 base_domain 非空 + ACME 关闭的部署器测试
// 管理器（独立凭据文件；短退避供 duty 用例）。
func newRegistryTestManager(t *testing.T, baseDomain string) (*Manager, *fakeDocker, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enabled := false
	cfg := Config{
		TokenFile:         filepath.Join(dir, "ingress.token"),
		CertDir:           filepath.Join(dir, "certs"),
		ConfigAdvertiseIP: "127.0.0.1",
		BaseDomain:        baseDomain,
		RegistryAuthFile:  filepath.Join(dir, "fleetly-registry.auth"),
		ACME:              ACMEConfig{Enabled: &enabled, CADirURL: "http://unused.test/dir"},
	}
	dc := newFakeDocker()
	m := NewManagerWithDocker(cfg, st, dc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.platformRetryInterval = time.Millisecond
	return m, dc, st
}

// ensureManagerPlatformID 在 meta 铸造平台 ID（identity duty 的装配期半——
// EnsureRegistry 的约束锚输入）。
func ensureManagerPlatformID(t *testing.T, st *state.Store) string {
	t.Helper()
	id, err := st.GetMeta(context.Background(), state.MetaKeyPlatformNodeID)
	if err != nil || id == "" {
		id = "n_01TESTNODEID"
		if err := st.InTx(context.Background(), func(tx *state.Tx) error {
			return tx.SetMeta(context.Background(), state.MetaKeyPlatformNodeID, id)
		}); err != nil {
			t.Fatalf("seed platform id: %v", err)
		}
	}
	return id
}

func TestEnsureRegistryCreatesPinnedService(t *testing.T) {
	m, dc, st := newRegistryTestManager(t, "x.test")
	platformID := ensureManagerPlatformID(t, st)
	// Traefik 先收敛（registry 路由段的 attach 前置；生产由 sweep 时序保证）。
	if err := m.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	if err := m.EnsureRegistry(context.Background()); err != nil {
		t.Fatalf("ensure registry: %v", err)
	}

	svc := dc.serviceState(RegistryServiceName)
	if !svc.Exists {
		t.Fatal("registry service not created")
	}
	// 镜像钉版（Normalize 回落 DefaultZotImage——digest 钉定形态）。
	if svc.Image != DefaultZotImage {
		t.Fatalf("registry image = %q, want pinned default %q", svc.Image, DefaultZotImage)
	}
	// replicated-1 + manager 约束（placement 同一锚）。
	if svc.Replicas != 1 {
		t.Fatalf("replicas = %d, want 1 (replicated-1)", svc.Replicas)
	}
	wantConstraint := registryConstraintFor(platformID)
	if len(svc.Constraints) != 1 || svc.Constraints[0] != wantConstraint {
		t.Fatalf("constraints = %v, want [%s]", svc.Constraints, wantConstraint)
	}
	// 单挂 fleetly-system（ID 形态）；零宿主端口（overlay 内 5000 明文）。
	if len(svc.Networks) != 1 || svc.Networks[0] != "netid-"+RegistryNetworkName {
		t.Fatalf("networks = %v, want [netid-%s]", svc.Networks, RegistryNetworkName)
	}
	if len(svc.Ports) != 0 {
		t.Fatalf("ports = %v, want none (no host publishing; Traefik 443 is the only public face)", svc.Ports)
	}
	// 挂载：数据卷 + htpasswd/配置工件只读 bind。
	if len(svc.Mounts) != 3 {
		t.Fatalf("mounts = %+v, want 3 (data volume + htpasswd + zot config)", svc.Mounts)
	}
	if svc.Mounts[0].Type != mount.TypeVolume || svc.Mounts[0].Source != RegistryVolumeName ||
		svc.Mounts[0].Target != registryDataMountPath {
		t.Fatalf("data mount = %+v", svc.Mounts[0])
	}
	if svc.Mounts[1].Type != mount.TypeBind || svc.Mounts[1].Target != registryHTPasswdMountPath || !svc.Mounts[1].ReadOnly {
		t.Fatalf("htpasswd mount = %+v", svc.Mounts[1])
	}
	if svc.Mounts[2].Type != mount.TypeBind || svc.Mounts[2].Target != registryConfigMountPath || !svc.Mounts[2].ReadOnly {
		t.Fatalf("zot config mount = %+v", svc.Mounts[2])
	}
	// 数据卷显式收敛调用。
	if len(dc.volumeEns) == 0 || dc.volumeEns[0] != RegistryVolumeName {
		t.Fatalf("volume ensures = %v, want [%s]", dc.volumeEns, RegistryVolumeName)
	}
	// Traefik 接入平台 overlay（registry 路由后端的可达面）。
	traefik := dc.serviceState(IngressServiceName)
	attached := false
	for _, n := range traefik.Networks {
		if n == "netid-"+RegistryNetworkName {
			attached = true
		}
	}
	if !attached {
		t.Fatalf("traefik networks = %v, want fleetly-system attached", traefik.Networks)
	}
}

func TestEnsureRegistryCredentialsArtifacts(t *testing.T) {
	m, _, st := newRegistryTestManager(t, "x.test")
	ensureManagerPlatformID(t, st)
	if err := m.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	authFile := m.cfg.RegistryAuthFile
	htFile, cfgFile := registryCredentialPaths(authFile)

	if err := m.EnsureRegistry(context.Background()); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// 凭据文件：user:password 单行（与 ingress token 同形的平台生成物）。
	raw, err := os.ReadFile(authFile) //nolint:gosec // G304：读测试临时目录工件
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	user, pass, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")
	if !ok || !strings.HasPrefix(user, registryUserPrefix) || len(pass) < 20 {
		t.Fatalf("auth file content malformed: %q (user=%q pass len=%d)", raw, user, len(pass))
	}
	// htpasswd 工件：bcrypt hash 校验通过（zot 消费面——明文不进容器）。
	htRaw, err := os.ReadFile(htFile) //nolint:gosec // G304：读测试临时目录工件
	if err != nil {
		t.Fatalf("read htpasswd: %v", err)
	}
	htUser, htHash, _ := strings.Cut(strings.TrimSpace(string(htRaw)), ":")
	if htUser != user {
		t.Fatalf("htpasswd user = %q, want %q", htUser, user)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(htHash), []byte(pass)); err != nil {
		t.Fatalf("htpasswd hash does not verify against the generated password: %v", err)
	}
	// zot 配置工件：确定性 JSON，鉴权指向容器内挂载点、端口 5000。
	cfgRaw, err := os.ReadFile(cfgFile) //nolint:gosec // G304：读测试临时目录工件
	if err != nil {
		t.Fatalf("read zot config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(cfgRaw, &parsed); err != nil {
		t.Fatalf("zot config not JSON: %v", err)
	}
	if !strings.Contains(string(cfgRaw), registryHTPasswdMountPath) ||
		!strings.Contains(string(cfgRaw), `"port": "5000"`) {
		t.Fatalf("zot config missing auth/port contract: %s", cfgRaw)
	}
	// 幂等：再收敛一轮，全部工件字节级不变（bcrypt 按校验写、配置按内容写）。
	snap := map[string]string{}
	for _, f := range []string{authFile, htFile, cfgFile} {
		b, _ := os.ReadFile(f) //nolint:gosec // G304：读测试临时目录工件
		snap[f] = string(b)
	}
	if err := m.EnsureRegistry(context.Background()); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	for _, f := range []string{authFile, htFile, cfgFile} {
		b, _ := os.ReadFile(f) //nolint:gosec // G304：读测试临时目录工件
		if string(b) != snap[f] {
			t.Fatalf("artifact %s churned across idempotent ensure", f)
		}
	}
}

func TestEnsureRegistryDriftConverges(t *testing.T) {
	m, dc, st := newRegistryTestManager(t, "x.test")
	ensureManagerPlatformID(t, st)
	if err := m.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	if err := m.EnsureRegistry(context.Background()); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// 已收敛：重跑零写操作。
	if err := m.EnsureRegistry(context.Background()); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}
	if n := len(dc.updates); n != 1 { // 仅 traefik attach 的那一次 update
		t.Fatalf("updates after convergence = %d, want 1 (traefik attach only)", n)
	}
	// 外部漂移（人改/版本遗留）：镜像、约束、挂载、网络、副本数全变。
	dc.mu.Lock()
	dc.services[RegistryServiceName] = ingressServiceState{
		Exists:  true,
		Version: 7,
		Image:   "ghcr.io/project-zot/zot:v0.1.0",
	}
	dc.mu.Unlock()
	if err := m.EnsureRegistry(context.Background()); err != nil {
		t.Fatalf("converge after drift: %v", err)
	}
	svc := dc.serviceState(RegistryServiceName)
	if svc.Version != 8 {
		t.Fatalf("version = %d, want 8 (one drift-convergence update)", svc.Version)
	}
	if svc.Image != DefaultZotImage || svc.Replicas != 1 || len(svc.Mounts) != 3 ||
		len(svc.Networks) != 1 || svc.Networks[0] != "netid-"+RegistryNetworkName {
		t.Fatalf("post-convergence state = %+v, want desired spec", svc)
	}
}

func TestEnsureRegistryPreconditions(t *testing.T) {
	// swarm 未就绪：显式哨兵（duty 退避重试面）。
	m, _, st := newRegistryTestManager(t, "x.test")
	ensureManagerPlatformID(t, st)
	m.docker.(*fakeDocker).mu.Lock()
	m.docker.(*fakeDocker).info.SwarmActive = false
	m.docker.(*fakeDocker).mu.Unlock()
	if err := m.EnsureRegistry(context.Background()); !errors.Is(err, ErrNotSwarmReady) {
		t.Fatalf("err = %v, want ErrNotSwarmReady", err)
	}
	// 平台 ID 未铸：显式失败（空 ID 约束 = 永不调度，宁缺毋错）。
	m2, dc2, st2 := newRegistryTestManager(t, "x.test")
	if id, _ := st2.GetMeta(context.Background(), state.MetaKeyPlatformNodeID); id != "" {
		t.Fatalf("precondition broken: meta pre-seeded %q", id)
	}
	if err := m2.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	if err := m2.EnsureRegistry(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "platform node id") {
		t.Fatalf("err = %v, want platform-id-not-ensured failure", err)
	}
	// 底座上不应有半成品服务。
	if svc := dc2.serviceState(RegistryServiceName); svc.Exists {
		t.Fatal("registry service must not be created when the pin anchor is missing")
	}
	_ = m
}

func TestEnsureRegistryNoopSingleNode(t *testing.T) {
	m, dc, _ := newRegistryTestManager(t, "") // base_domain 空 = 单节点形态
	if err := m.EnsureRegistry(context.Background()); err != nil {
		t.Fatalf("ensure on single node: %v", err)
	}
	if len(dc.volumeEns) != 0 || len(dc.netEns) != 0 || len(dc.creates) != 0 {
		t.Fatalf("single-node ensure must be a full no-op, got volumes=%v nets=%v creates=%v",
			dc.volumeEns, dc.netEns, dc.creates)
	}
	// duty 不活动：同步调用立即返回。
	done := make(chan struct{})
	go func() {
		m.runRegistryDuty(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("registry duty must not run on a single-node install")
	}
}

func TestRunRegistryDutyConverges(t *testing.T) {
	m, dc, st := newRegistryTestManager(t, "x.test")
	ensureManagerPlatformID(t, st)
	if err := m.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.runRegistryDuty(ctx)
	awaitUntil(t, "registry service creation by the duty", 5*time.Second, func() bool {
		return dc.serviceState(RegistryServiceName).Exists
	})
}

func TestRegistryRouteInDynamicConfig(t *testing.T) {
	m, dc, st := newRegistryTestManager(t, "x.test")
	ensureManagerPlatformID(t, st)
	if err := m.EnsureTraefik(context.Background()); err != nil {
		t.Fatalf("ensure traefik: %v", err)
	}
	ctx := context.Background()
	if err := m.publish(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}
	snap, _ := m.vw.snapshot()
	// 80 路由恒在（证书就绪前 registry.<base> 仍可达——诚实形态）。
	web := snap.HTTP.Routers[RegistryServiceName+"-web"]
	if web == nil || web.Rule != "Host(`registry.x.test`)" {
		t.Fatalf("registry web router = %+v", web)
	}
	svc := snap.HTTP.Services[RegistryServiceName]
	if svc == nil || len(svc.LoadBalancer.Servers) != 1 ||
		svc.LoadBalancer.Servers[0].URL != "http://fleetly-registry:5000" {
		t.Fatalf("registry service = %+v", svc)
	}
	if _, ok := snap.HTTP.Routers[RegistryServiceName+"-websecure"]; ok {
		t.Fatal("443 route must be absent before the platform certificate is issued")
	}
	if snap.TLS != nil && len(snap.TLS.Certificates) != 0 {
		t.Fatalf("tls section = %+v, want empty before cert issuance", snap.TLS)
	}
	_ = dc
	_ = st
}

func TestRegistryRouteGainsTLSWithPlatformCert(t *testing.T) {
	m, _, _ := newRegistryTestManager(t, "x.test")
	ctx := context.Background()
	// 平台证书落库（自签代签发——E1-3 测试同款手法）。
	certPEM, keyPEM := selfSignedTestCertMultiSAN(t, m.PlatformDomains())
	pair, err := ParsePair(platformCertApp, m.PlatformDomains(), certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if err := m.certs.Save(pair); err != nil {
		t.Fatalf("save platform cert: %v", err)
	}
	if err := m.publishWithCerts(ctx); err != nil {
		t.Fatalf("publish with certs: %v", err)
	}
	snap, _ := m.vw.snapshot()
	ws := snap.HTTP.Routers[RegistryServiceName+"-websecure"]
	if ws == nil || ws.TLS == nil {
		t.Fatalf("registry websecure router = %+v, want TLS-enabled 443 route", ws)
	}
	if snap.TLS == nil || len(snap.TLS.Certificates) != 1 {
		t.Fatalf("tls certificates = %+v, want the platform certificate", snap.TLS)
	}
	if !strings.Contains(snap.TLS.Certificates[0].CertFile, "CERTIFICATE") ||
		!strings.Contains(snap.TLS.Certificates[0].KeyFile, "PRIVATE KEY") {
		t.Fatal("platform certificate must be distributed inline (E1-2 contract)")
	}
}

func TestRegistryRouteAbsentSingleNode(t *testing.T) {
	m, _, _ := newRegistryTestManager(t, "")
	if err := m.publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	snap, _ := m.vw.snapshot()
	if _, ok := snap.HTTP.Routers[RegistryServiceName+"-web"]; ok {
		t.Fatal("single-node view must not carry a platform registry route (v0.1 equivalence)")
	}
	if snap.HTTP.Routers[fallbackRouterName] == nil {
		t.Fatal("empty route set must fall back to the noop router (H9)")
	}
}

// TestDefaultZotImagePinnedForm 钉版字面契约：name:tag@sha256:<64hex>——
// R7 纪律（tag 保留可读性，digest 为准；台账见 docs/runbooks/image-prepull.md）。
func TestDefaultZotImagePinnedForm(t *testing.T) {
	re := regexp.MustCompile(`^ghcr\.io/project-zot/zot:v2\.1\.21@sha256:[0-9a-f]{64}$`)
	if !re.MatchString(DefaultZotImage) {
		t.Fatalf("DefaultZotImage = %q, want name:tag@sha256:<64hex> pinned form", DefaultZotImage)
	}
}

// TestRegistryConstraintFormula 约束公式钉死（与 placement.ConstraintFor
// 同公式——zot 与应用绑定共用同一平台节点 ID 锚）。
func TestRegistryConstraintFormula(t *testing.T) {
	if got, want := registryConstraintFor("n_01HZX"), "node.labels.fleetly.node-id == n_01HZX"; got != want {
		t.Fatalf("constraint = %q, want %q", got, want)
	}
}
