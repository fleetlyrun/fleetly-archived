package ingress

// 管理器/端点/证书窗口测试：发布收敛（假 docker 客户端）、token 鉴权
// 负面测试、空视图落 noop 兜底（H9 合法空态 + Spike B「先校验后写」）、
// 路由撤销链路、续期窗口判定、证书落盘/解析。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// fakeDocker 是 dockerClient 的假实现（服务/网络内存态 + 调用记录；E1-2
// 起无卷/seed 路径——迁移收敛面以 legacySeedPresent 模拟 v0.1 残留）。
// mu 串行化全部方法（底座 client 是并发安全的，假实现同构——平台证书
// duty 后台 goroutine 与测试轮询并发访问）。
type fakeDocker struct {
	mu          sync.Mutex
	services    map[string]dutydocker.ServiceSnapshot
	networks    map[string]bool
	creates     []string
	updates     []string
	updateSpecs []swarm.ServiceSpec
	netEns      []string
	netRemoved  []string
	volumeEns   []string
	info        dutydocker.InfoSnapshot
	// legacySeedPresent 模拟 v0.1 证书 seed 容器残留（ContainerRemoveForce
	// 消费并清零——底座语义：移除后不复存在）。
	legacySeedPresent bool
	seedRemoved       []string
	// netMissing 模拟网络缺位（NetworkID 对名单内名字返回错误——E3-6：
	// 「rustfs duty 尚未建网」的 attach 负路径底座语义）。
	netMissing map[string]bool
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		services:   map[string]dutydocker.ServiceSnapshot{},
		networks:   map[string]bool{},
		netMissing: map[string]bool{},
		info:       dutydocker.InfoSnapshot{SwarmActive: true, NodeAddr: "127.0.0.1"},
	}
}

// serviceState 是服务实况的加锁读取出口（并发轮询场景的规范读法）。
func (f *fakeDocker) serviceState(name string) dutydocker.ServiceSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services[name]
}

func (f *fakeDocker) Info(context.Context) (dutydocker.InfoSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, nil
}

func (f *fakeDocker) ServiceInspect(_ context.Context, name string) (dutydocker.ServiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services[name], nil
}

func (f *fakeDocker) ServiceCreate(_ context.Context, spec swarm.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, spec.Name)
	f.services[spec.Name] = dutydocker.ServiceSnapshot{
		Exists:  true,
		Version: 1,
		Image:   spec.TaskTemplate.ContainerSpec.Image,
		Args:    append([]string{}, spec.TaskTemplate.ContainerSpec.Args...),
		Hosts:   append([]string{}, spec.TaskTemplate.ContainerSpec.Hosts...),
		Ports: func() []swarm.PortConfig {
			// registry 服务不发布宿主端口（EndpointSpec 缺省）——同构底座语义。
			if spec.EndpointSpec == nil {
				return nil
			}
			return append([]swarm.PortConfig{}, spec.EndpointSpec.Ports...)
		}(),
		Mounts: append([]mount.Mount{}, spec.TaskTemplate.ContainerSpec.Mounts...),
		HealthTest: func() []string {
			if spec.TaskTemplate.ContainerSpec.Healthcheck != nil {
				return spec.TaskTemplate.ContainerSpec.Healthcheck.Test
			}
			return nil
		}(),
		Constraints: func() []string {
			if spec.TaskTemplate.Placement == nil {
				return nil
			}
			return append([]string{}, spec.TaskTemplate.Placement.Constraints...)
		}(),
		Replicas: func() uint64 {
			if spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas == nil {
				return 0
			}
			return *spec.Mode.Replicated.Replicas
		}(),
		Networks: netTargets(spec),
	}
	return nil
}

func (f *fakeDocker) ServiceUpdate(_ context.Context, name string, _ uint64, spec swarm.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, name)
	// 记录提交的整份 spec 载荷（网络保留断言用——收敛更新是否保留既有
	// app 挂载只能在载荷上断言）。
	f.updateSpecs = append(f.updateSpecs, spec)
	cur := f.services[name]
	cur.Version++
	// 同构底座语义：service update 是整份 spec 替换——镜像/参数/挂载/健康
	// 检查/端口随载荷换入（与 ServiceCreate 同构），仅 Networks 单独记录
	//（网络目标是整组替换语义的最常断言位）。
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		cur.Image = cs.Image
		cur.Args = append([]string{}, cs.Args...)
		cur.Hosts = append([]string{}, cs.Hosts...)
		if cs.Healthcheck != nil {
			cur.HealthTest = append([]string{}, cs.Healthcheck.Test...)
		}
		cur.Mounts = append([]mount.Mount{}, cs.Mounts...)
	}
	if spec.TaskTemplate.Placement != nil {
		cur.Constraints = append([]string{}, spec.TaskTemplate.Placement.Constraints...)
	}
	if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil {
		cur.Replicas = *spec.Mode.Replicated.Replicas
	}
	if spec.EndpointSpec != nil {
		cur.Ports = append([]swarm.PortConfig{}, spec.EndpointSpec.Ports...)
	}
	cur.Networks = netTargets(spec)
	f.services[name] = cur
	return nil
}

func netTargets(spec swarm.ServiceSpec) []string {
	out := []string{}
	for _, n := range spec.TaskTemplate.Networks {
		out = append(out, n.Target)
	}
	return out
}

func (f *fakeDocker) NetworkEnsure(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netEns = append(f.netEns, name)
	f.networks[name] = true
	return nil
}

func (f *fakeDocker) NetworkID(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.netMissing[name] {
		return "", errors.New("network not found: " + name)
	}
	// 同构底座语义：ID 形态与名字不同（swarm 归一），幂等判据走 ID。
	return "netid-" + name, nil
}

// NetworkRemove 记录网络移除调用（MoveApp 摘旧网；同构底座语义：缺失视为
// 成功；仍有端点挂接 = 错误——本假件以 netMissing 模拟缺失路径）。
func (f *fakeDocker) NetworkRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.netRemoved = append(f.netRemoved, name)
	if f.netMissing[name] {
		return nil
	}
	delete(f.networks, name)
	return nil
}

// VolumeEnsure 记录卷收敛调用（同构底座语义：存在即 no-op，缺失创建）。
func (f *fakeDocker) VolumeEnsure(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumeEns = append(f.volumeEns, name)
	return nil
}

// ContainerRemoveForce 消费 legacySeedPresent（同构底座语义：容器
// 不存在 = false 且无副作用；存在 = 移除并记录）。
func (f *fakeDocker) ContainerRemoveForce(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.legacySeedPresent {
		return false, nil
	}
	f.legacySeedPresent = false
	f.seedRemoved = append(f.seedRemoved, name)
	return true, nil
}

// newTestManager 构造 ACME 关闭的测试管理器（独立 token/cert 目录）。
func newTestManager(t *testing.T) (*Manager, *fakeDocker, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dc := newFakeDocker()
	enabled := false
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{
		TokenFile:         filepath.Join(dir, "ingress.token"),
		CertDir:           filepath.Join(dir, "certs"),
		ConfigAdvertiseIP: "127.0.0.1",
		ACME:              ACMEConfig{Enabled: &enabled, CADirURL: "http://unused.test/dir"},
	}
	m := NewManagerWithDocker(cfg, st, dc, log)
	return m, dc, st
}

func TestPublishRoutesConvergesTraefikAndView(t *testing.T) {
	m, dc, st := newTestManager(t)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	in := PublishInput{AppID: app.ID, AppName: "demo", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "8080", Domains: []string{"test.example.internal"}},
	}}
	if err := m.PublishRoutes(ctx, in); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Traefik 收敛：一次创建、参数含 HTTP provider 端点与 token header。
	if len(dc.creates) != 1 || dc.creates[0] != IngressServiceName {
		t.Fatalf("traefik create calls = %v", dc.creates)
	}
	svc := dc.services[IngressServiceName]
	if svc.Image != DefaultTraefikImage {
		t.Fatalf("traefik image = %s, want pinned %s", svc.Image, DefaultTraefikImage)
	}
	endpointArg := ""
	for _, a := range svc.Args {
		if strings.HasPrefix(a, "--providers.http.endpoint=") {
			endpointArg = a
		}
	}
	if !strings.Contains(endpointArg, "http://127.0.0.1:8422/configs") {
		t.Fatalf("provider endpoint arg = %q", endpointArg)
	}
	authArg := ""
	for _, a := range svc.Args {
		if strings.HasPrefix(a, "--providers.http.headers.Authorization=") {
			authArg = a
		}
	}
	if !strings.HasPrefix(authArg, "--providers.http.headers.Authorization=Bearer ") {
		t.Fatalf("token header arg = %q", authArg)
	}
	// host 80/443 端口形态。
	if len(svc.Ports) != 2 {
		t.Fatalf("traefik ports = %+v", svc.Ports)
	}
	// E1-2：证书卷挂载退役（期望 spec 零挂载——证书经动态配置内联下发）。
	if len(svc.Mounts) != 0 {
		t.Fatalf("traefik spec must have no cert mounts since E1-2: %+v", svc.Mounts)
	}

	// app 网络接入（一次 update）。
	if len(dc.updates) != 1 {
		t.Fatalf("expected 1 network-attach update, got %v", dc.updates)
	}

	// 视图：路由已合成（带 port）。
	snap, _ := m.vw.snapshot()
	if snap.HTTP.Routers["fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-demo-web-web"] == nil {
		t.Fatalf("route not in view: %+v", snap.HTTP.Routers)
	}
	if got := snap.HTTP.Services["fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-demo-web"].LoadBalancer.Servers[0].URL; got != "http://fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-demo-web:8080" {
		t.Fatalf("server url = %s", got)
	}

	// 台账已同步。
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows) != 1 || rows[0].Domain != "test.example.internal" || rows[0].Port != "8080" {
		t.Fatalf("ledger rows = %+v", rows)
	}

	// 幂等重发布：无新建、无多余更新。
	before := len(dc.creates) + len(dc.updates)
	if err := m.PublishRoutes(ctx, in); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if after := len(dc.creates) + len(dc.updates); after != before {
		t.Fatalf("idempotent republish changed calls: %d -> %d", before, after)
	}
}

// TestPublishEmptyDeclarationKeepsStateRows 「省略 = 删除」退役（IMPL-T1-1
// 单一写点仲裁）：声明真值 = state 行——空 Declared（compose 无域名 label /
// 文件漂移）不删除既有行、路由保持；撤销唯一入口 = WithdrawAppRoutes 与
// API 删除（label 仅首部署播种）。
func TestPublishEmptyDeclarationKeepsStateRows(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	app, _ := testsupport.SeedAppE(t, st, "solo")
	in := PublishInput{AppID: app.ID, AppName: "solo", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "80", Domains: []string{"solo.example.test"}},
	}}
	if err := m.PublishRoutes(ctx, in); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if snap, _ := m.vw.snapshot(); snap.HTTP.Routers["fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-solo-web-web"] == nil {
		t.Fatal("route should be in view after publish")
	}

	// 空 Declared：state 行保持、路由保持——label 不再拥有对账删除权。
	if err := m.PublishRoutes(ctx, PublishInput{AppID: app.ID, AppName: "solo", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug}); err != nil {
		t.Fatalf("publishing without declarations must not fail: %v", err)
	}
	snap, _ := m.vw.snapshot()
	if snap.HTTP.Routers["fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-solo-web-web"] == nil {
		t.Fatal("state rows must survive an empty declaration (omission-means-deletion retired)")
	}
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows) != 1 || rows[0].Domain != "solo.example.test" {
		t.Fatalf("state rows must be kept: %+v", rows)
	}
}

// TestWithdrawAppRoutes 撤销链路（app 删除管线的入口，H9）：发布域名
// 路由 → WithdrawAppRoutes → 台账清空 + 视图落兜底；幂等；sweep 的全量
// 重发布（republishAll）对空集成功（不再产生永久 warn 的拒绝路径）。
func TestWithdrawAppRoutes(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, "gone")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := m.PublishRoutes(ctx, PublishInput{AppID: app.ID, AppName: "gone", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "80", Domains: []string{"gone.example.test"}},
	}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if snap, _ := m.vw.snapshot(); snap.HTTP.Routers["fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-gone-web-web"] == nil {
		t.Fatal("route should be in view before withdraw")
	}

	// 撤销：台账清 + 视图落兜底。
	if err := m.WithdrawAppRoutes(ctx, app.ID); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows) != 0 {
		t.Fatalf("ledger rows must be deleted on withdraw: %+v", rows)
	}
	snap, _ := m.vw.snapshot()
	if snap.HTTP.Routers["fleetly-"+app.TeamSlug+"-"+app.ProjectSlug+"-gone-web-web"] != nil {
		t.Fatal("route must be withdrawn from view")
	}
	if snap.HTTP.Routers[fallbackRouterName] == nil {
		t.Fatalf("view must fall back to noop router after withdraw: %+v", snap.HTTP.Routers)
	}

	// 幂等：重复撤销无副作用。
	if err := m.WithdrawAppRoutes(ctx, app.ID); err != nil {
		t.Fatalf("withdraw must be idempotent: %v", err)
	}
	// sweep 路径（republishAll）对空路由集成功——空视图不再被 Validate
	// 拒绝（12h sweep 不再永久 warn）。
	if err := m.republishAll(ctx); err != nil {
		t.Fatalf("republishAll on empty ledger must succeed via fallback: %v", err)
	}
	// 空输入拒绝。
	if err := m.WithdrawAppRoutes(ctx, ""); err == nil {
		t.Fatal("withdraw with empty app id must fail")
	}
}

// TestPublishSeedsFromLabelsWhenStateEmpty 守卫②的种子里程（IMPL-T1-1）：
// state 无行 + compose label 声明 → 首部署播种（默认 protocol=http /
// cert_mode=http01），路由按 state 行渲染；重复发布（种子已落库）不再改行，
// 转为 label 忽略 + 事件披露。
func TestPublishSeedsFromLabelsWhenStateEmpty(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	app, _ := testsupport.SeedAppE(t, st, "seed")
	in := PublishInput{AppID: app.ID, AppName: "seed", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "8080", Domains: []string{"seed.example.test"}},
	}}
	if err := m.PublishRoutes(ctx, in); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows) != 1 || rows[0].Domain != "seed.example.test" || rows[0].Port != "8080" ||
		rows[0].Protocol != "http" || rows[0].CertMode != "http01" {
		t.Fatalf("seeded row wrong: %+v", rows)
	}
	if got := len(eventsNamed(t, st, "route.label_ignored")); got != 0 {
		t.Fatalf("seed publish must not raise label_ignored (got %d)", got)
	}
	// 二次发布（同声明）：state 已存在 → 不播种、不改行，派忽略事件一次。
	if err := m.PublishRoutes(ctx, in); err != nil {
		t.Fatalf("republish: %v", err)
	}
	rows2, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows2) != 1 || rows2[0].Domain != "seed.example.test" {
		t.Fatalf("republish must not change seeded rows: %+v", rows2)
	}
	if got := len(eventsNamed(t, st, "route.label_ignored")); got != 1 {
		t.Fatalf("republish with labels must raise label_ignored once (got %d)", got)
	}
}

// TestPublishIgnoresLabelsWhenStateRowsExist 守卫②（仲裁验收，IMPL-T1-1）：
// state 已有域名行时 compose label 一律忽略——不播种、不合并、不删除，
// 路由按 state 行渲染，并派 route.label_ignored 事件（label 仅 bootstrap）。
func TestPublishIgnoresLabelsWhenStateRowsExist(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	app, _ := testsupport.SeedAppE(t, st, "arb")
	// state 行（API 形态）：api.example.test → web:8080 http。
	if _, err := st.CreateAppDomain(ctx, app.ID, state.DomainInput{
		Domain: "api.example.test", Service: "web", Port: "8080", Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	// compose label 声明另一域名/端口（旧写法）：必须被忽略。
	in := PublishInput{AppID: app.ID, AppName: "arb", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "9090", Domains: []string{"label.example.test"}},
	}}
	if err := m.PublishRoutes(ctx, in); err != nil {
		t.Fatalf("publish: %v", err)
	}
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows) != 1 || rows[0].Domain != "api.example.test" || rows[0].Port != "8080" {
		t.Fatalf("labels must be ignored when state rows exist: %+v", rows)
	}
	base := "fleetly-" + app.TeamSlug + "-" + app.ProjectSlug + "-arb-web"
	snap, _ := m.vw.snapshot()
	if snap.HTTP.Routers[base+"-web"] == nil {
		t.Fatalf("state row route missing: %+v", snap.HTTP.Routers)
	}
	if rule := snap.HTTP.Routers[base+"-web"].Rule; !strings.Contains(rule, "api.example.test") || strings.Contains(rule, "label.example.test") {
		t.Fatalf("view must render state rows only, rule = %q", rule)
	}
	got := eventsNamed(t, st, "route.label_ignored")
	if len(got) != 1 {
		t.Fatalf("route.label_ignored events = %d, want 1", len(got))
	}
	if got[0].Subject != "app:arb" {
		t.Fatalf("event subject = %q, want app:arb", got[0].Subject)
	}
}

// TestPublishMultiBackendServicePerDomainRouting 守卫①/③（IMPL-T1-1）：同一
// 服务两域名不同端口不同协议 → dynamic config 各自正确——第 1 组（http）
// 取 RouterName 本体 + http://，第 2 组（h2c）取 `~<port>~h2c` 后缀 +
// h2c://（scheme=h2c 直出）；后端 DNS 名恒为 Swarm 服务名（后缀只进键
// 空间），80 路由各自带所属 host 规则。
func TestPublishMultiBackendServicePerDomainRouting(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	app, _ := testsupport.SeedAppE(t, st, "multi")
	if _, err := st.CreateAppDomain(ctx, app.ID, state.DomainInput{
		Domain: "ws.multi.example.test", Service: "web", Port: "9080", Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create ws domain: %v", err)
	}
	if _, err := st.CreateAppDomain(ctx, app.ID, state.DomainInput{
		Domain: "grpc.multi.example.test", Service: "web", Port: "9090", Protocol: "h2c", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create grpc domain: %v", err)
	}
	if err := m.PublishRoutes(ctx, PublishInput{AppID: app.ID, AppName: "multi", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	snap, _ := m.vw.snapshot()
	base := "fleetly-" + app.TeamSlug + "-" + app.ProjectSlug + "-multi-web"
	// 第 1 组（数值端口序 http:9080）：RouterName 本体。
	if got := snap.HTTP.Services[base].LoadBalancer.Servers[0].URL; got != "http://"+base+":9080" {
		t.Fatalf("http group backend = %q", got)
	}
	if rule := snap.HTTP.Routers[base+"-web"].Rule; rule != "Host(`ws.multi.example.test`)" {
		t.Fatalf("http group rule = %q", rule)
	}
	// 第 2 组（h2c:9090）：键后缀消歧，后端 DNS 仍是服务名。
	h2cKey := base + "~9090~h2c"
	if snap.HTTP.Services[h2cKey] == nil {
		t.Fatalf("h2c group service missing (keys: %v)", mapKeys(snap.HTTP.Services))
	}
	if got := snap.HTTP.Services[h2cKey].LoadBalancer.Servers[0].URL; got != "h2c://"+base+":9090" {
		t.Fatalf("h2c group backend = %q, want h2c://%s:9090 (scheme=h2c direct)", got, base)
	}
	if snap.HTTP.Routers[h2cKey+"-web"] == nil {
		t.Fatalf("h2c group router missing: %v", mapKeys(snap.HTTP.Routers))
	}
	if rule := snap.HTTP.Routers[h2cKey+"-web"].Rule; rule != "Host(`grpc.multi.example.test`)" {
		t.Fatalf("h2c group rule = %q", rule)
	}
	// 单后端服务公式零变化（既有用例已覆盖），此处钉键空间唯一性：两组键
	// 互异且后缀与任何 RouterName 不可能撞（'~' 不在服务名字符集内）。
	if h2cKey == base {
		t.Fatal("group keys must be distinct")
	}
}

// TestConvergeAppDomainsFromState API 写面收敛入口（IMPL-T1-1）：资源行落库
// 后收敛出路由；删除行再收敛 → 路由撤销（视图落兜底）。
func TestConvergeAppDomainsFromState(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	app, _ := testsupport.SeedAppE(t, st, "api")
	if _, err := st.CreateAppDomain(ctx, app.ID, state.DomainInput{
		Domain: "api.example.test", Service: "web", Port: "8080", Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if err := m.ConvergeAppDomains(ctx, app.ID); err != nil {
		t.Fatalf("converge: %v", err)
	}
	base := "fleetly-" + app.TeamSlug + "-" + app.ProjectSlug + "-api-web"
	if snap, _ := m.vw.snapshot(); snap.HTTP.Routers[base+"-web"] == nil {
		t.Fatal("route missing after converge")
	}
	if err := st.RemoveAppDomain(ctx, app.ID, "api.example.test"); err != nil {
		t.Fatalf("remove domain: %v", err)
	}
	if err := m.ConvergeAppDomains(ctx, app.ID); err != nil {
		t.Fatalf("converge after removal: %v", err)
	}
	snap, _ := m.vw.snapshot()
	if snap.HTTP.Routers[base+"-web"] != nil {
		t.Fatal("route must be withdrawn after the state row is removed")
	}
	if snap.HTTP.Routers[fallbackRouterName] == nil {
		t.Fatalf("empty view must fall back to the noop router: %v", mapKeys(snap.HTTP.Routers))
	}
}

// eventsNamed 返回店内指定名字的事件（升序）。
func eventsNamed(t *testing.T, st *state.Store, name string) []state.Event {
	t.Helper()
	evs, err := st.EventsSince(context.Background(), 0, 200)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	out := []state.Event{}
	for _, ev := range evs {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	return out
}

// mapKeys 是配置键集合的排错视图（测试失败输出用）。
func mapKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestProviderEndpointTokenAuth 配置端点鉴权负面测试（验收 3：带错
// token 401）+ 挑战应答面 + /healthz。
func TestProviderEndpointTokenAuth(t *testing.T) {
	m, _, _ := newTestManager(t)
	v := m.vw
	v.setRoutes([]Route{{App: "demo", Service: "web", Port: "80", Domains: []string{"d.test"}}})
	handler := newProviderHandler(v, "secret-token")
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
	// 错 token → 401。
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/configs", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get /configs wrong token: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token /configs = %d, want 401", resp.StatusCode)
	}
	// 对 token → 200 + 合法动态配置 JSON。
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/configs", nil)
	req2.Header.Set("Authorization", "Bearer secret-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("get /configs ok token: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("ok-token /configs = %d, want 200", resp2.StatusCode)
	}
	raw, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(raw), `"routers"`) || !strings.Contains(string(raw), `"services"`) {
		t.Fatalf("config payload missing keys: %s", raw)
	}
	// /healthz 无鉴权 200。
	resp3, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp3.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %v/%d", err, resp3.StatusCode)
	}
	_ = resp3.Body.Close()
	// 挑战应答：在途 token 命中、未知 404。
	v.addChallenge("challenge-token", "challenge-token.thumbprint")
	resp4, err := http.Get(srv.URL + "/.well-known/acme-challenge/challenge-token")
	if err != nil {
		t.Fatalf("challenge fetch: %v", err)
	}
	body, _ := io.ReadAll(resp4.Body)
	_ = resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK || string(body) != "challenge-token.thumbprint" {
		t.Fatalf("challenge response = %d %q", resp4.StatusCode, body)
	}
	resp5, err := http.Get(srv.URL + "/.well-known/acme-challenge/unknown")
	if err != nil {
		t.Fatalf("unknown challenge: %v", err)
	}
	_ = resp5.Body.Close()
	if resp5.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown challenge = %d, want 404", resp5.StatusCode)
	}
}

// TestNeedsRenewalWindow 续期窗口判定（30 天窗）：缺失/域名集变化/临期
// 重签，健康证书跳过。
func TestNeedsRenewalWindow(t *testing.T) {
	now := time.Now().UTC()
	if !needsRenewal(nil, []string{"a.test"}, now, 30*24*time.Hour) {
		t.Fatal("nil cert needs issuance")
	}
	// 域名集变化（多 SAN 单证书契约）。
	pair := &CertificatePair{Domains: []string{"a.test"}, NotAfter: now.Add(90 * 24 * time.Hour)}
	if !needsRenewal(pair, []string{"a.test", "b.test"}, now, 30*24*time.Hour) {
		t.Fatal("domain set change must trigger re-issue")
	}
	// 进入窗口（还剩 10 天 < 30 天窗）。
	pair2 := &CertificatePair{Domains: []string{"a.test"}, NotAfter: now.Add(10 * 24 * time.Hour)}
	if !needsRenewal(pair2, []string{"a.test"}, now, 30*24*time.Hour) {
		t.Fatal("cert within renewal window must be renewed")
	}
	// 窗外（还剩 40 天）。
	pair3 := &CertificatePair{Domains: []string{"a.test"}, NotAfter: now.Add(40 * 24 * time.Hour)}
	if needsRenewal(pair3, []string{"a.test"}, now, 30*24*time.Hour) {
		t.Fatal("healthy cert outside window must NOT be renewed")
	}
}

// TestCertStoreRoundTripAndParse 证书库落盘/回读/解析（合成叶证书）。
func TestCertStoreRoundTripAndParse(t *testing.T) {
	dir := t.TempDir()
	store := newCertStore(dir)
	certPEM, keyPEM, notAfter := selfSignedTestCert(t, "round.test")
	pair, err := ParsePair("round", []string{"round.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if pair.SHA256 == "" || len(pair.SHA256) != 64 {
		t.Fatalf("sha256 malformed: %q", pair.SHA256)
	}
	if !pair.NotAfter.Equal(notAfter) {
		t.Fatalf("not after = %v, want %v", pair.NotAfter, notAfter)
	}
	if err := store.Save(pair); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.Load("round")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.SHA256 != pair.SHA256 || !sameDomainSet(got.Domains, pair.Domains) {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if _, err := store.Load("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cert should surface os.ErrNotExist, got %v", err)
	}
}

// TestCertStoreSaveAtomicReplaceAndNoResidue E5：tmp+rename 原子写语义——
// 同名重写换入新内容（rename 覆盖既有目标，Windows 同样成立）、目录内
// 不残留 .tmp 中间文件（半写形态不可能被 Traefik/Load 观测）。
func TestCertStoreSaveAtomicReplaceAndNoResidue(t *testing.T) {
	dir := t.TempDir()
	store := newCertStore(dir)
	certPEM, keyPEM, _ := selfSignedTestCert(t, "first.test")
	pair, err := ParsePair("app-x", []string{"first.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if err := store.Save(pair); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 重写（续期形态）：同名换入新内容。
	certPEM2, keyPEM2, _ := selfSignedTestCert(t, "second.test")
	pair2, err := ParsePair("app-x", []string{"second.test"}, certPEM2, keyPEM2)
	if err != nil {
		t.Fatalf("parse pair2: %v", err)
	}
	if err := store.Save(pair2); err != nil {
		t.Fatalf("re-save (rename over existing): %v", err)
	}
	got, err := store.Load("app-x")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.SHA256 != pair2.SHA256 || !sameDomainSet(got.Domains, pair2.Domains) {
		t.Fatalf("content after re-save = %+v, want the second pair (rename must replace)", got)
	}
	// 无 .tmp 残留。
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temp file residue after atomic save: %v", leftovers)
	}
}

// TestCertStoreSaveDetectsTampering E5 损坏注入：回读内容与内存值 sha256
// 不一致 → Save 显式报错（兑现注释承诺；调用方按既有重试语义承接）。
func TestCertStoreSaveDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	store := newCertStore(dir)
	// 回读出口注入篡改（磁盘/写路径损坏形态）。
	store.readFile = func(path string) ([]byte, error) {
		raw, err := os.ReadFile(path) //nolint:gosec // G304：读 t.TempDir 受控工件，路径自注入出口
		if err != nil {
			return nil, err
		}
		tampered := append([]byte{}, raw...)
		tampered[0] ^= 0xff
		return tampered, nil
	}
	certPEM, keyPEM, _ := selfSignedTestCert(t, "tamper.test")
	pair, err := ParsePair("tamper", []string{"tamper.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	err = store.Save(pair)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("tampered read-back must fail Save with verification error, got %v", err)
	}
}

// TestAttachNetworkKeepsPreviousApps 多 app 网络回归：逐个接入两个 app
// 的网络后，实况网络集必须同时包含两者（不得互相覆盖——实机验证发现的
// 回归，T2.15 修复钉死）。
func TestAttachNetworkKeepsPreviousApps(t *testing.T) {
	m, dc, st := newTestManager(t)
	ctx := context.Background()
	for _, name := range []string{"app1", "app2"} {
		appRow, err := testsupport.SeedAppE(t, st, name)
		if err != nil {
			t.Fatalf("create app: %v", err)
		}
		if err := m.PublishRoutes(ctx, PublishInput{AppID: appRow.ID, AppName: name, TeamSlug: appRow.TeamSlug, PrjSlug: appRow.ProjectSlug, Declared: []ServiceRoutes{
			{Service: "web", Port: "80", Domains: []string{name + ".example.test"}},
		}}); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
	}
	nets := dc.services[IngressServiceName].Networks
	has := func(s string) bool {
		for _, n := range nets {
			if n == s {
				return true
			}
		}
		return false
	}
	// 底座把 attach 目标归一为 ID（fake 同构：netid-<name>）——按 ID 断言。
	// 网络名 = 三段公式（team/prj slug 取自各自 app 行）。
	app1, err := st.GetAppByName(ctx, "app1")
	if err != nil {
		t.Fatalf("get app1: %v", err)
	}
	app2, err := st.GetAppByName(ctx, "app2")
	if err != nil {
		t.Fatalf("get app2: %v", err)
	}
	n1, nerr := appNetworkName(app1.TeamSlug, app1.ProjectSlug, "app1")
	if nerr != nil {
		t.Fatalf("app1 net name: %v", nerr)
	}
	n2, nerr := appNetworkName(app2.TeamSlug, app2.ProjectSlug, "app2")
	if nerr != nil {
		t.Fatalf("app2 net name: %v", nerr)
	}
	if !has("netid-"+n1) || !has("netid-"+n2) {
		t.Fatalf("networks after two app attaches = %v, want both app nets present", nets)
	}
	// 幂等重发布（同一 app）：attach 以 ID 判等——不再产生更新/重复项。
	before := len(dc.updates)
	if err := m.PublishRoutes(ctx, PublishInput{AppID: app1.ID, AppName: "app1", TeamSlug: app1.TeamSlug, PrjSlug: app1.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "80", Domains: []string{"app1.example.test"}},
	}}); err != nil {
		t.Fatalf("republish app1: %v", err)
	}
	if after := len(dc.updates); after != before {
		t.Fatalf("idempotent attach produced %d extra updates", after-before)
	}
	// 网络目标无重复项（ID 判等防重复 attach，实机回归钉死）。
	seen := map[string]int{}
	for _, n := range dc.services[IngressServiceName].Networks {
		seen[n]++
		if seen[n] > 1 {
			t.Fatalf("duplicate network attachment %s (attach must be idempotent)", n)
		}
	}
}

// TestEnsureTraefikUpdatePreservesAttachedNetworks 收敛更新保留全部既有
// app 网络挂载（架构评审 H8）：spec 漂移（如镜像钉版变更）触发的更新不得
// 清空 TaskTemplate.Networks——Traefik 必须同时挂在所有 app 网络上才能
// 反代各 app 容器；一次漂移收敛断网 = 全路由 502，只有各 app 再部署才逐个
// 恢复的实机回归钉死。
func TestEnsureTraefikUpdatePreservesAttachedNetworks(t *testing.T) {
	m, dc, st := newTestManager(t)
	ctx := context.Background()
	// 预置：两个 app 各自接入网络（经真实发布路径建起多 app 挂载态）。
	for _, name := range []string{"app1", "app2"} {
		appRow, err := testsupport.SeedAppE(t, st, name)
		if err != nil {
			t.Fatalf("create app: %v", err)
		}
		if err := m.PublishRoutes(ctx, PublishInput{AppID: appRow.ID, AppName: name, TeamSlug: appRow.TeamSlug, PrjSlug: appRow.ProjectSlug, Declared: []ServiceRoutes{
			{Service: "web", Port: "80", Domains: []string{name + ".example.test"}},
		}}); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
	}
	// 制造漂移：实况镜像与期望钉版不一致（如镜像升级后的收敛场景）。
	svc := dc.services[IngressServiceName]
	svc.Image = "traefik:v3.4"
	dc.services[IngressServiceName] = svc
	if err := m.EnsureTraefik(ctx); err != nil {
		t.Fatalf("converge traefik: %v", err)
	}
	// 更新已提交，且载荷是期望镜像（证明这是漂移收敛更新而非 attach）。
	last := dc.updateSpecs[len(dc.updateSpecs)-1]
	if last.TaskTemplate.ContainerSpec.Image != DefaultTraefikImage {
		t.Fatalf("update payload image = %s, want %s", last.TaskTemplate.ContainerSpec.Image, DefaultTraefikImage)
	}
	// 核心断言：两个既有网络仍在提交载荷里、且无新增（以实况为基准合并）。
	// 网络名 = 三段公式（各自 app 行 slug）。
	app1, err := st.GetAppByName(ctx, "app1")
	if err != nil {
		t.Fatalf("get app1: %v", err)
	}
	app2, err := st.GetAppByName(ctx, "app2")
	if err != nil {
		t.Fatalf("get app2: %v", err)
	}
	net1, nerr := appNetworkName(app1.TeamSlug, app1.ProjectSlug, "app1")
	if nerr != nil {
		t.Fatalf("app1 net: %v", nerr)
	}
	net2, nerr := appNetworkName(app2.TeamSlug, app2.ProjectSlug, "app2")
	if nerr != nil {
		t.Fatalf("app2 net: %v", nerr)
	}
	got := map[string]bool{}
	for _, n := range last.TaskTemplate.Networks {
		got[n.Target] = true
	}
	if len(got) != 2 || !got["netid-"+net1] || !got["netid-"+net2] {
		t.Fatalf("update payload networks = %v, want exactly both attached app nets", last.TaskTemplate.Networks)
	}
	// 服务实况同构：整组替换后两网络仍在（底座视角未断网）。
	if nets := dc.services[IngressServiceName].Networks; len(nets) != 2 {
		t.Fatalf("service networks after converge = %v, want both preserved", nets)
	}
}

// TestEnsureTraefikUpdateWithoutNetworks 对照位：实况无任何挂载时收敛
// 更新不引入 Networks 字段（首次创建后未 attach 的行为不变位）。
func TestEnsureTraefikUpdateWithoutNetworks(t *testing.T) {
	m, dc, _ := newTestManager(t)
	ctx := context.Background()
	if err := m.EnsureTraefik(ctx); err != nil {
		t.Fatalf("create traefik: %v", err)
	}
	// 制造漂移：实况镜像与期望钉版不一致。
	svc := dc.services[IngressServiceName]
	svc.Image = "traefik:v3.4"
	dc.services[IngressServiceName] = svc
	if err := m.EnsureTraefik(ctx); err != nil {
		t.Fatalf("converge traefik: %v", err)
	}
	last := dc.updateSpecs[len(dc.updateSpecs)-1]
	if len(last.TaskTemplate.Networks) != 0 {
		t.Fatalf("update payload networks = %v, want none when service has no attachments", last.TaskTemplate.Networks)
	}
}

// TestEnsureTraefikRetiresLegacyCertDistribution E1-2 迁移收敛：既有 v0.1
// 安装升级后的残留形态（服务实况带证书卷只读挂载 + seed 容器在）——启动
// sweep 收敛后：① seed 容器被移除（幂等：第二次收敛不再产生移除调用）；
// ② 收敛更新提交的 spec 无挂载（traefikSpecEqual 对挂载的差异即触发更新
// ——旧挂载由此清除）；③ 证书数据面不被触碰（fake 无任何卷写操作出口，
// 结构上不可能）。升级前快照是回退路径（部署面纪律，测试面钉行为）。
func TestEnsureTraefikRetiresLegacyCertDistribution(t *testing.T) {
	m, dc, _ := newTestManager(t)
	ctx := context.Background()
	// 先按当前形态创建服务（无挂载），再注入 v0.1 残留态。
	if err := m.EnsureTraefik(ctx); err != nil {
		t.Fatalf("create traefik: %v", err)
	}
	svc := dc.services[IngressServiceName]
	svc.Mounts = []mount.Mount{{
		Type:     mount.TypeVolume,
		Source:   "fleetly-ingress-certs",
		Target:   "/fleetly-certs",
		ReadOnly: true,
	}}
	dc.services[IngressServiceName] = svc
	dc.legacySeedPresent = true

	// 首轮收敛：挂载差异触发更新 + seed 容器移除。
	if err := m.EnsureTraefik(ctx); err != nil {
		t.Fatalf("converge traefik with legacy residue: %v", err)
	}
	if got := dc.seedRemoved; len(got) != 1 || got[0] != "fleetly-ingress-cert-seeder" {
		t.Fatalf("legacy seed removals = %v, want exactly [fleetly-ingress-cert-seeder]", got)
	}
	last := dc.updateSpecs[len(dc.updateSpecs)-1]
	if len(last.TaskTemplate.ContainerSpec.Mounts) != 0 {
		t.Fatalf("converged spec must carry no cert mounts: %+v", last.TaskTemplate.ContainerSpec.Mounts)
	}
	if got := dc.services[IngressServiceName].Mounts; len(got) != 0 {
		t.Fatalf("service mounts after converge = %+v, want none (volume model retired)", got)
	}

	// 幂等：残留清零后重复收敛无更新、无再移除。
	updates, removals := len(dc.updates), len(dc.seedRemoved)
	if err := m.EnsureTraefik(ctx); err != nil {
		t.Fatalf("second converge: %v", err)
	}
	if len(dc.updates) != updates || len(dc.seedRemoved) != removals {
		t.Fatalf("retirement must be idempotent: updates %d->%d, removals %d->%d",
			updates, len(dc.updates), removals, len(dc.seedRemoved))
	}
}

// TestViewCarriesInlinePEM E1-2：证书经视图进 tls.certificates 内联下发
// （V-MN 证据锚定：certFile/keyFile 键携内联 PEM，Traefik FileOrContent
// 契约按内容消费；443 所服证书指纹 = 内联证书指纹）。发布路径 → 视图
// 快照 → JSON 载荷三级断言：载荷含 certFile 键、其值可解析出与签发证书
// DER 指纹一致的证书。
func TestViewCarriesInlinePEM(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, "shop")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	certPEM, keyPEM, _ := selfSignedTestCert(t, "shop.example.test")
	pair, err := ParsePair("shop", []string{"shop.example.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if err := m.certs.Save(pair); err != nil {
		t.Fatalf("save pair: %v", err)
	}

	// 发布（带证书段的全量重发布）→ 视图。
	in := PublishInput{AppID: app.ID, AppName: "shop", TeamSlug: app.TeamSlug, PrjSlug: app.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "80", Domains: []string{"shop.example.test"}},
	}}
	if err := m.PublishRoutes(ctx, in); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := m.publishWithCerts(ctx); err != nil {
		t.Fatalf("publish with certs: %v", err)
	}
	snap, _ := m.vw.snapshot()
	if snap.TLS == nil || len(snap.TLS.Certificates) != 1 {
		t.Fatalf("tls.certificates segment missing: %+v", snap.TLS)
	}
	cert := snap.TLS.Certificates[0]
	if !strings.Contains(cert.CertFile, "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("certFile must carry inline PEM, got %.80q", cert.CertFile)
	}
	if !strings.Contains(cert.KeyFile, "-----BEGIN") {
		t.Fatalf("keyFile must carry inline PEM, got %.40q", cert.KeyFile)
	}
	// 指纹级比对（spike 证据形态）：载荷中的证书 DER sha256 = 落盘证书
	// DER sha256——Traefik 443 所服证书由该载荷决定，指纹一致即通道等价。
	gotFP := pemCertFingerprint(t, []byte(cert.CertFile))
	wantFP := pemCertFingerprint(t, certPEM)
	if gotFP != wantFP {
		t.Fatalf("inline cert fingerprint mismatch: got %s want %s", gotFP, wantFP)
	}
	// 序列化后键名契约（FileOrContent 修正项钉死）。
	raw, err := json.Marshal(snap.TLS)
	if err != nil {
		t.Fatalf("marshal tls segment: %v", err)
	}
	if !strings.Contains(string(raw), `"certFile"`) || !strings.Contains(string(raw), `"keyFile"`) {
		t.Fatalf("tls payload keys must be certFile/keyFile (FileOrContent contract): %s", raw)
	}
}

// TestDomainlessPublishKeepsTLSegments F10（2026-09-21 真机发现）：无域名
// 应用的发布（publish 全量换视图）不得把既有应用的 websecure 路由与
// tls.certificates 段擦出视图——publish 统一走带证书重发布后，任何全量
// 视图换入都保留盘上证书。回归形态：修复前 websecure 计数归零（全平台
// TLS 消失到下次带证书重发布）。
func TestDomainlessPublishKeepsTLSegments(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	// app A：带域名 + 证书（既有 TLS 面）。
	appA, err := testsupport.SeedAppE(t, st, "shop")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	certPEM, keyPEM, _ := selfSignedTestCert(t, "shop.example.test")
	pair, err := ParsePair("shop", []string{"shop.example.test"}, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse pair: %v", err)
	}
	if err := m.certs.Save(pair); err != nil {
		t.Fatalf("save pair: %v", err)
	}
	inA := PublishInput{AppID: appA.ID, AppName: "shop", TeamSlug: appA.TeamSlug, PrjSlug: appA.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "web", Port: "80", Domains: []string{"shop.example.test"}},
	}}
	if err := m.PublishRoutes(ctx, inA); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	snapA, _ := m.vw.snapshot()
	if snapA.TLS == nil || len(snapA.TLS.Certificates) != 1 {
		t.Fatalf("precondition: A carries TLS segment, got %+v", snapA.TLS)
	}
	// app B：无域名（内部应用）发布——全量视图换入不得擦掉 A 的 TLS。
	appB, err := testsupport.SeedAppE(t, st, "internal")
	if err != nil {
		t.Fatalf("create app B: %v", err)
	}
	inB := PublishInput{AppID: appB.ID, AppName: "internal", TeamSlug: appB.TeamSlug, PrjSlug: appB.ProjectSlug, Declared: []ServiceRoutes{
		{Service: "svc", Port: "8080"},
	}}
	if err := m.PublishRoutes(ctx, inB); err != nil {
		t.Fatalf("publish B (domainless): %v", err)
	}
	snapB, _ := m.vw.snapshot()
	if snapB.TLS == nil || len(snapB.TLS.Certificates) != 1 {
		t.Fatalf("F10: domainless publish wiped TLS segments: %+v", snapB.TLS)
	}
	hasWebsecure := false
	for _, r := range snapB.HTTP.Routers {
		for _, ep := range r.EntryPoints {
			if ep == "websecure" {
				hasWebsecure = true
			}
		}
	}
	if !hasWebsecure {
		t.Fatalf("F10: domainless publish wiped websecure routers: %+v", snapB.HTTP.Routers)
	}
}

// pemCertFingerprint 解析 PEM 首个 CERTIFICATE 块并返回 DER sha256（十六
// 进制；指纹级断言出口）。
func pemCertFingerprint(t *testing.T, pemBytes []byte) string {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("no CERTIFICATE PEM block found")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// TestTokenLoadOrGenerateStable token 持久化：首启生成、重启复用同值。
func TestTokenLoadOrGenerateStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ingress.token")
	first, created, err := tokenLoadOrGenerate(path)
	if err != nil || !created {
		t.Fatalf("first load: created=%t err=%v", created, err)
	}
	second, created2, err := tokenLoadOrGenerate(path)
	if err != nil || created2 {
		t.Fatalf("second load should reuse: created=%t err=%v", created2, err)
	}
	if first != second || len(first) < 32 {
		t.Fatalf("token not stable/generated: %q vs %q", first, second)
	}
}

// selfSignedTestCert 生成自签测试证书（返回 PEM 与 NotAfter）。
func selfSignedTestCert(t *testing.T, domain string) ([]byte, []byte, time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
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
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, tmpl.NotAfter
}
