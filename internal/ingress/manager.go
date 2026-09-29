package ingress

// Manager 是入口适配器门面：Traefik 收敛 + 配置端点 + 路由发布/撤销 +
// ACME 证书。发布引擎经 RoutePublisher 端口（PublishRoutes）消费；app
// 删除管线经 WithdrawAppRoutes 撤销路由（H9）；fleetlyd ingress 服务壳
// 消费 EnsureTraefik/Run/Handler。

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/registration"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// PublishInput 是一次路由发布的输入（engine.RoutePublisher 契约的载荷）。
// IMPL-T1-1 起声明真值 = state 域名行（发布点现读；消除「读快照 ↔ 写点」
// 竞态），Declared 只承载 compose label 声明（bootstrap 种子候选：
// state 无行时播种，state 有行时一律忽略并派事件——单一写点仲裁）。
// TeamSlug/PrjSlug 是归属两个 slug（v0.3：per-app 网络接入与路由键三段
// 公式的参数，rbac-teams §4.3）。
type PublishInput struct {
	AppID    string
	AppName  string
	TeamSlug string
	PrjSlug  string
	// Declared 是 compose label 域名声明（归一化 spec 产出；仅首部署
	// state 无行时作为种子落库，此后永久让位给 state 行）。
	Declared []ServiceRoutes
}

// ServiceRoutes 是 compose label 的单个入口服务声明（种子形态）。
type ServiceRoutes struct {
	Service string
	// Port 是路由目标端口（compose expose 首端口）。
	Port    string
	Domains []string
}

// Manager 是入口与证书管理器。
type Manager struct {
	cfg    Config
	store  *state.Store
	docker dockerClient
	log    *slog.Logger
	// nowFunc 时钟出口（续期窗口单测注入）。
	nowFunc func() time.Time

	// vw 是配置视图（Validate 通过才换入）。
	vw *view
	// certs 是证书库。
	certs *certStore
	// cfgPortOverride 由服务壳在监听确定后回填（挑战应答 URL 依赖）。
	cfgPortOverride int
	// tokenOnce 缓存 token（生成/加载一次）。
	tokenOnce sync.Once
	tokenVal  string
	tokenErr  error
	// mu 保护 advertiseIP/lastSpec（EnsureTraefik 与 attachNetwork 的
	// 生产者-消费者）与 user（E1：账号缓存的读写统一走本锁）。
	mu          sync.Mutex
	advertiseIP string
	lastSpec    *swarm.ServiceSpec
	// user 是 ACME 账号缓存。
	user *acmeUser
	// issueMu 是签发互斥（E1，S19）：ensureCertificate 全程串行——v0.1
	// 单签发容量。调用方有两路并发来源（engine PublishRoutes 与 Run 的
	// sweep/续期扫描），两个 goroutine 对同一 app 并发 Obtain 会打 LE
	// duplicate 证书限额；串行后后到者重读证书库即命中刚落盘的证书
	//（幂等返回，不二次签发）。
	issueMu sync.Mutex
	// obtainFn 是 lego Obtain 步骤的注入缝（E1）：生产恒为 legoObtain；
	// 单测注入假签发器（计数 + 自签证书）断言并发串行化与单次签发，
	// 不依赖真实 CA。
	obtainFn func(ctx context.Context, app string, user registration.User, domains []string) (certPEM, keyPEM []byte, err error)
	// platformRetryInterval 是平台证书 duty 的重试退避（E1-3；零值回落
	// platformCertRetryInterval 常量，单测注入短退避驱动重试次序断言）。
	platformRetryInterval time.Duration
	// s3PublicScanInterval 是 s3 公网开关 duty 的稳态扫描周期（E3-6；零值
	// 回落 s3PublicScanInterval 常量，单测注入短周期驱动开关收敛断言）。
	s3PublicScanInterval time.Duration
	// dnsProviderFn 是 DNS-01 插件解析缝（W5-S3，dns01.go；生产 = 装配点
	// 闭包，nil = DNS-01 面未装配——wildcard 开启时平台证书签发如实失败）。
	dnsProviderFn DNSProviderResolver
}

// NewManager 构造入口管理器（cfg 缺省回落；docker client 按
// DOCKER_HOST/本机套接字构造）。返回 cleanup 释放 docker 连接。
func NewManager(cfg Config, store *state.Store, log *slog.Logger) (*Manager, func(), error) {
	norm := cfg.Normalize()
	if err := norm.Validate(); err != nil {
		return nil, nil, err
	}
	dc, err := dutydocker.New("")
	if err != nil {
		return nil, nil, err
	}
	m := newManagerWithDocker(norm, store, dc, log)
	return m, func() { _ = dc.Close() }, nil
}

// NewManagerWithDocker 以注入的 dockerClient 构造（单测）。
func NewManagerWithDocker(cfg Config, store *state.Store, dc dockerClient, log *slog.Logger) *Manager {
	return newManagerWithDocker(cfg.Normalize(), store, dc, log)
}

func newManagerWithDocker(norm Config, store *state.Store, dc dockerClient, log *slog.Logger) *Manager {
	m := &Manager{
		cfg:     norm,
		store:   store,
		docker:  dc,
		log:     log,
		nowFunc: time.Now().UTC,
		vw:      newView(""),
		certs:   newCertStore(norm.CertDir),
	}
	// E1：签发缝默认接 lego 实现（单测覆盖为假签发器）。
	m.obtainFn = m.legoObtain
	return m
}

// WithClock 注入时钟（单测）。
func (m *Manager) WithClock(f func() time.Time) *Manager { m.nowFunc = f; return m }

// Config 返回生效配置（诊断）。
func (m *Manager) Config() Config { return m.cfg }

// SetConfigPort 由服务壳在监听确定后回填（挑战应答 URL 依赖）。
func (m *Manager) SetConfigPort(port int) { m.cfgPortOverride = port }

// cfgPort 是配置端点端口（override 优先，ConfigAddr 解析兜底）。
func (m *Manager) cfgPort() int {
	if m.cfgPortOverride != 0 {
		return m.cfgPortOverride
	}
	if _, port, err := net.SplitHostPort(m.cfg.ConfigAddr); err == nil {
		var p int
		if _, scanErr := fmt.Sscanf(port, "%d", &p); scanErr == nil {
			return p
		}
	}
	return 8422
}

// token 加载或生成配置端点 token（幂等；缓存）。
func (m *Manager) token(_ context.Context) (string, error) {
	m.tokenOnce.Do(func() {
		m.tokenVal, _, m.tokenErr = tokenLoadOrGenerate(m.cfg.TokenFile)
	})
	return m.tokenVal, m.tokenErr
}

// Token 返回平台 ingress token 明文（W5-S2 告警接收器的同源凭据消费面——
// POST /internal/alerts 与本配置端点共用同一 token file，加载/生成语义与
// token() 完全一致〔幂等、缓存、首启生成落 0600 文件〕；这是 token 明文的
// 唯一进程内出口，接收器侧只做常量时间比对不落日志）。
func (m *Manager) Token(ctx context.Context) (string, error) {
	return m.token(ctx)
}

// setResponderURL / responderURLValue 是挑战应答基址的写读（EnsureTraefik
// 探测 advertise addr 后回填；view 的快照合成消费——只改基址字段，路由
// 与挑战态不受影响）。
func (m *Manager) setResponderURL(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vw.setResponder(url)
	if u, err := neturl.Parse(url); err == nil {
		m.advertiseIP = u.Hostname()
	}
}

func (m *Manager) responderURLValue() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vw.responderURL
}

// Handler 返回配置端点 HTTP handler（服务壳挂到独立内部端口——取舍见
// provider.go 顶部注释）。
func (m *Manager) Handler(ctx context.Context) (http.Handler, error) {
	token, err := m.token(ctx)
	if err != nil {
		return nil, err
	}
	return newProviderHandler(m.vw, token), nil
}

// TLSHandler 返回 8423 TLS 面的 HTTP handler（E1-3：仅 /configs，见
// provider.go newTLSConfigHandler；runtime 服务壳在 ConfigTLSEnabled 时
// 以平台证书装配 TLS 监听）。
func (m *Manager) TLSHandler(ctx context.Context) (http.Handler, error) {
	token, err := m.token(ctx)
	if err != nil {
		return nil, err
	}
	return newTLSConfigHandler(m.vw, token), nil
}

// Run 是周期任务：Traefik 收敛 + 全量重发布 + 证书续期扫描（sweep）+
// 平台证书 duty（E1-3，仅 base_domain 非空时活动）+ registry 部署 duty
// （E1-4，仅 base_domain 非空时活动——与证书 duty 无次序依赖，设计 §2.4
// 次序⑤）。由 fleetlyd ingress 服务壳调用（ctx 取消返回）。收敛失败只
// 降级日志（下轮重试），不影响控制面其余服务。
func (m *Manager) Run(ctx context.Context) error {
	go m.runPlatformCertDuty(ctx)
	go m.runRegistryDuty(ctx)
	go m.runS3PublicDuty(ctx)
	m.sweep(ctx)
	ticker := time.NewTicker(m.cfg.RenewScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.sweep(ctx)
		}
	}
}

// sweep 一轮收敛：Traefik 幂等收敛（失败降级）→ 全量重发布（控制面重启
// 的视图重建路径）→ 证书续期扫描。
func (m *Manager) sweep(ctx context.Context) {
	if err := m.EnsureTraefik(ctx); err != nil {
		m.log.Warn("ingress: traefik converge deferred (retry next sweep)", "error", err)
		return
	}
	if err := m.republishAll(ctx); err != nil {
		m.log.Warn("ingress: republish sweep failed", "error", err)
		return
	}
	if err := m.renewDue(ctx); err != nil {
		m.log.Warn("ingress: cert renewal scan failed", "error", err)
	}
}

// renewDue 续期扫描：台账内全部带域名 app，进入续期窗口（默认到期前
// 30 天）或证书缺失的重签。逐 app 失败不阻断其余（审计 error 行 + 日志；
// 不发明新事件名——事件纪律见 acme.go）。
func (m *Manager) renewDue(ctx context.Context) error {
	routes, err := m.routesFromStore(ctx)
	if err != nil {
		return err
	}
	byApp := map[string][]string{}
	for _, r := range routes {
		byApp[r.App] = append(byApp[r.App], r.Domains...)
	}
	return m.renewDueByApp(ctx, byApp)
}

// renewDueByApp 按 app 域名集聚合执行续期（renewDue 的分段：byApp 来自
// routesFromStore 快照，appID 在本段解析——两步之间 app 可能已被删除）。
// E3（S19）：GetAppByName 解析失败（app 刚删除/改名竞态）→ 跳过该 app +
// warn——原实现把空串 appID 下传 ensureCertificate，SetDomainCert 全部落空
// 且每个域名误报一条「revoked domain row」warn；跳过是如实的语义（路由
// 集快照时 app 还在，续期已无台账可登记）。
func (m *Manager) renewDueByApp(ctx context.Context, byApp map[string][]string) error {
	appIDs := map[string]string{}
	for _, app := range sortedKeys(byApp) {
		appRow, err := m.store.GetAppByName(ctx, app)
		if err != nil {
			m.log.Warn("ingress: cert renewal skip (app id unresolvable)", "app", app, "error", err)
			continue // E3：不再空串下传
		}
		appIDs[app] = appRow.ID
	}
	for _, app := range sortedKeys(appIDs) {
		domains := uniqueSorted(byApp[app])
		if _, err := m.ensureCertificate(ctx, appIDs[app], app, domains, true); err != nil {
			m.log.Warn("ingress: cert renewal", "app", app, "error", err)
		}
	}
	return m.publishWithCerts(ctx)
}

// routesFromStore 从域名台账构建全量路由集（app × service 聚合）。
func (m *Manager) routesFromStore(ctx context.Context) ([]Route, error) {
	rows, err := m.store.ListAllDomains(ctx)
	if err != nil {
		return nil, err
	}
	return routesFromLedger(ctx, m.store, rows)
}

// withPlatformRoutes（E1-4 registry 路由段 + E3-6 s3 公网路由段）定义于
// s3public.go——平台路由段的组装单点（ctx 感知：s3 公网开关每次发布现读
// 设置）。

// publish 换入全量视图：**统一带证书段**（F10 修复，2026-09-21 真机发现：
// 无证书段的 publish 会把既有 websecure/内联证书整体擦出视图——无域名
// 应用部署、签发竞态后的下一拍都会让全平台 TLS 消失到下次带证书重发布；
// publishWithCerts 对无证书 app 本就自动降级 HTTP-only，全量视图无理由
// 存在无证书形态）。校验不过（键缺失/悬空引用类坏形态）= 不换视图、不落
// 库（Spike B 纪律）。空路由集经兜底路由恒过（H9 合法空态——撤销即真实
// 下发空态，而非拒绝后残留旧路由）。
func (m *Manager) publish(ctx context.Context) error {
	return m.publishWithCerts(ctx)
}

// PublishRoutes 实现 engine.RoutePublisher（发布链路：单一写点仲裁 →
// Traefik 收敛与网络接入 → 全量配置收敛 → 证书保障）。声明的真值 = state
// 域名行（发布点现读）：
//
//   - state 有行：Declared（compose label）一律忽略并派 route.label_ignored
//     事件（label 仅 bootstrap 种子形态；dokploy「Domains UI 与 label 并存
//     时 Traefik 二选一不可控」的事故类在数据模型层消灭）；
//   - state 无行 + Declared 非空：首部署播种（SeedAppDomainsIfEmpty 原子
//     落库，此后
//     state 即真值）；
//   - 两者皆空：无路由声明，照常收敛（幂等空操作，不误删任何既有行）。
//
// 证书保障：本 app 域名集缺/变/临期即签发（HTTP-01 完整链；失败不阻断
// 已生效的 HTTP 路由，TLS 段随下次成功签发收敛）。
//
// 语义注释（T2.15 设计取舍）：发布失败的处置权在调用方（引擎）——部署
// 不回滚、route.publish_failed 单独告警 + 审计；本方法自身是幂等收敛
// （重调即重试）。
func (m *Manager) PublishRoutes(ctx context.Context, in PublishInput) error {
	if in.AppName == "" {
		return fmt.Errorf("ingress: publish requires app name")
	}
	rows, err := m.store.ListAppDomains(ctx, in.AppID)
	if err != nil {
		return err
	}
	switch {
	case len(rows) > 0:
		if len(in.Declared) > 0 {
			m.discloseIgnoredLabels(ctx, in, rows)
		}
	case len(in.Declared) > 0:
		seeds := make([]state.DomainServiceRoutes, 0, len(in.Declared))
		for _, svc := range in.Declared {
			seeds = append(seeds, state.DomainServiceRoutes{
				Service: svc.Service,
				Port:    svc.Port,
				Domains: append([]string{}, svc.Domains...),
			})
		}
		// 播种走原子仲裁原语（检空 + 插入同事务）：并发 API 写行不被覆盖/
		// 删除；播种与否都重读行集——真值恒来自 state。
		if _, err := m.store.SeedAppDomainsIfEmpty(ctx, in.AppID, seeds); err != nil {
			return err
		}
		if rows, err = m.store.ListAppDomains(ctx, in.AppID); err != nil {
			return err
		}
	}
	if err := m.converge(ctx, in.AppID, in.AppName, in.TeamSlug, in.PrjSlug, rows); err != nil {
		return err
	}
	return nil
}

// ConvergeAppDomains 是域名资源写面（API CRUD）后的收敛入口：state 行即
// 真值（无种子仲裁——写面已落库），收敛 = 网络接入 + 全量发布 + 证书保障。
// 失败上抛给调用方处置（API 面按「资源已保存、收敛随下次发布/续期扫描
// 重试」的语义披露，不回滚资源行）。
func (m *Manager) ConvergeAppDomains(ctx context.Context, appID string) error {
	if appID == "" {
		return fmt.Errorf("ingress: converge requires app id")
	}
	app, err := m.store.GetAppByID(ctx, appID)
	if err != nil {
		return err
	}
	rows, err := m.store.ListAppDomains(ctx, appID)
	if err != nil {
		return err
	}
	return m.converge(ctx, appID, app.Name, app.TeamSlug, app.ProjectSlug, rows)
}

// converge 是发布/写面共用的收敛段（行集已就位：Traefik 收敛 → 网络接入
// → 全量发布 → 证书保障 → 带证书重发布）。
func (m *Manager) converge(ctx context.Context, appID, appName, teamSlug, prjSlug string, rows []state.Domain) error {
	// ① Traefik 收敛 + app 网络接入（swarm 未就绪显式失败——调用方降级
	// 告警语义的输入；不猜测底座状态）。
	if err := m.EnsureTraefik(ctx); err != nil {
		return err
	}
	if len(rows) > 0 {
		if err := m.attachNetwork(ctx, teamSlug, prjSlug, appName); err != nil {
			return err
		}
	}
	// ② 全量配置收敛（全量视图语义：其他 app 的路由同盘——单应用坏配置
	// 不影响其他应用路由的边界在本方法的 Validate/合成层面成立）。
	if err := m.publish(ctx); err != nil {
		return err
	}
	// ③ 证书保障：本 app 域名集的证书缺/变/临期即签发（HTTP-01 完整链；
	// 失败返回错误——HTTP 路由已在②生效）。成功后带 TLS 段重发布一次。
	domains := domainsOfRows(rows)
	if len(domains) > 0 {
		if _, err := m.ensureCertificate(ctx, appID, appName, domains, false); err != nil {
			return err
		}
		if err := m.publishWithCerts(ctx); err != nil {
			return err
		}
	}
	return nil
}

// discloseIgnoredLabels 派发 label 被忽略事件（仲裁守卫的披露面；事件写
// 失败只降级日志——路由收敛不受披露失败影响，与证书审计同纪律）。
func (m *Manager) discloseIgnoredLabels(ctx context.Context, in PublishInput, rows []state.Domain) {
	declaredDomains, declaredServices := 0, len(in.Declared)
	for _, svc := range in.Declared {
		declaredDomains += len(svc.Domains)
	}
	payload := fmt.Sprintf(
		`{"app":%q,"reason":"state_rows_exist","declared_services":%d,"declared_domains":%d,"state_domains":%d}`,
		in.AppName, declaredServices, declaredDomains, len(rows))
	err := m.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.AppendEvent(ctx, state.Event{
			Name:    "route.label_ignored",
			Subject: "app:" + in.AppName,
			Payload: payload,
		})
		return err
	})
	if err != nil {
		m.log.Warn("ingress: record label-ignored event failed", "app", in.AppName, "error", err)
	}
	m.log.Info("ingress: compose domain labels ignored (state domain rows are the single source of truth; labels are bootstrap-only)",
		"app", in.AppName, "declared_services", declaredServices, "declared_domains", declaredDomains, "state_rows", len(rows))
}

// publishWithCerts 全量重发布（带证书段）：证书库就绪的 app 路由挂
// CertificateRef（443 路由 + tls.certificates）；无证书 app 仅 HTTP。平台
// 路由段（E1-4/E3-6）同盘：registry 与（公网开关开启时的）s3.<base> 路由
// 的 App 都是平台证书保留名，平台证书落库后由既有按 app 挂证书循环自动
// 获得 443 路由与内联证书段（零特判）。
func (m *Manager) publishWithCerts(ctx context.Context) error {
	routes, err := m.routesFromStore(ctx)
	if err != nil {
		return err
	}
	routes = m.withPlatformRoutes(ctx, routes)
	withCerts := make([]Route, 0, len(routes))
	apps := map[string]bool{}
	for _, r := range routes {
		apps[r.App] = true
		withCerts = append(withCerts, r)
	}
	for _, app := range sortedKeys(apps) {
		pair, err := m.certs.Load(app)
		if err != nil {
			continue // 无证书 app：仅 HTTP 路由（Load 缺失/损坏均不阻断）
		}
		for i := range withCerts {
			if withCerts[i].App == app {
				// 内联分发载荷（E1-2）：PEM 现读自证书库（真源），随视图
				// 进 tls.certificates——不再经证书卷/seed 容器分发。
				withCerts[i].Cert = &CertificateRef{
					App:      app,
					SHA256:   pair.SHA256,
					NotAfter: pair.NotAfter.UnixNano(),
					CertPEM:  pair.CertPEM,
					KeyPEM:   pair.KeyPEM,
				}
			}
		}
	}
	cfg := Synthesize(withCerts)
	if err := Validate(cfg); err != nil {
		return err
	}
	m.vw.setRoutes(withCerts)
	return nil
}

// WithdrawAppRoutes 撤销 app 的全部路由（app 删除管线的显式撤销入口，
// H9）：删该 app 域名台账行 → 全量重发布（台账空出的视图自然落 noop
// 兜底——Traefik 侧真实撤销旧路由）。幂等（无行删除为空操作、重发布
// 幂等）；台账写入方唯一纪律（本包）决定域名清理必须经本通道。失败
// 语义与 PublishRoutes 同构：处置权在调用方（DeleteApp 侧失败告警）。
func (m *Manager) WithdrawAppRoutes(ctx context.Context, appID string) error {
	if appID == "" {
		return fmt.Errorf("ingress: withdraw requires app id")
	}
	if err := m.store.DeleteAppDomains(ctx, appID); err != nil {
		return err
	}
	return m.republishAll(ctx)
}

// DetachAppNetwork 是 MoveApp 摘旧网的收尾面（v0.3 W2-S3，rbac-teams
// §4.3「网络 prune 空旧网」）：traefik 从旧 app 专属 overlay 摘挂（以实况
// 网络集为基准删除目标 ID——attachNetworkID 的镜像语义）+ 网络移除
// （best-effort：仍有端点挂接返回错误——引用方清场后由调用方重试/文档消化）。
// 幂等：traefik 未挂接该网 = 摘挂 no-op；网络已不存在 = 移除 no-op。
func (m *Manager) DetachAppNetwork(ctx context.Context, team, prj, app string) error {
	netName, err := appNetworkName(team, prj, app)
	if err != nil {
		return err
	}
	netID, err := m.docker.NetworkID(ctx, netName)
	if err != nil {
		// 网络已不存在：摘挂无从谈起，幂等成功。
		return nil
	}
	cur, err := m.docker.ServiceInspect(ctx, IngressServiceName)
	if err != nil {
		return err
	}
	if cur.Exists {
		targets := make([]string, 0, len(cur.Networks))
		detached := false
		for _, n := range cur.Networks {
			if n == netID {
				detached = true
				continue
			}
			targets = append(targets, n)
		}
		if detached {
			m.mu.Lock()
			base := m.lastSpec
			m.mu.Unlock()
			if base == nil {
				return fmt.Errorf("ingress: no desired spec available (ensure traefik first)")
			}
			spec := specWithNetworks(*base, targets)
			if err := m.docker.ServiceUpdate(ctx, IngressServiceName, cur.Version, spec); err != nil {
				return err
			}
			m.mu.Lock()
			m.lastSpec = &spec
			m.mu.Unlock()
			m.log.Info("ingress: traefik detached from app overlay network (app move)", "network", netName)
		}
	}
	// 空网移除（仍挂端点返回错误——调用方 best-effort）。
	return m.docker.NetworkRemove(ctx, netName)
}

// republishAll 是 sweep 的全量重发布（HTTP 段 + 证书段；幂等）。
func (m *Manager) republishAll(ctx context.Context) error {
	if err := m.publish(ctx); err != nil {
		return err
	}
	return m.publishWithCerts(ctx)
}

// Status 是入口状态投影（fleetlyd 诊断）。
type Status struct {
	Traefik     dutydocker.ServiceSnapshot
	AdvertiseIP string
	Responder   string
}

// Status 读取入口服务实况（Traefik 侧；配置端点健康经 Handler 面探测）。
func (m *Manager) Status(ctx context.Context) (Status, error) {
	cur, err := m.docker.ServiceInspect(ctx, IngressServiceName)
	if err != nil {
		return Status{}, err
	}
	return Status{
		Traefik:     cur,
		AdvertiseIP: m.advertiseIP,
		Responder:   m.vw.responderURL,
	}, nil
}

// domainsOfRows 收集行集域名（排序去重；多 SAN 单证书/app）。
func domainsOfRows(rows []state.Domain) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Domain)
	}
	return uniqueSorted(out)
}

// domainGroupKey 是路由分组键（app × service × port × protocol：同一服务的
// 不同后端各占一条 Route）。
type domainGroupKey struct{ app, service, port, protocol string }

// lessRouteGroup 是路由分组的全序（app/service 字典序 → 端口数值序 →
// 协议序 http < h2c）。端口不可解析时按字符串序兜底并排在可解析值之后。
func lessRouteGroup(a, b domainGroupKey) bool {
	if a.app != b.app {
		return a.app < b.app
	}
	if a.service != b.service {
		return a.service < b.service
	}
	an, aerr := strconv.Atoi(a.port)
	bn, berr := strconv.Atoi(b.port)
	switch {
	case aerr == nil && berr == nil:
		if an != bn {
			return an < bn
		}
	case aerr == nil:
		return true
	case berr == nil:
		return false
	}
	if a.port != b.port {
		return a.port < b.port
	}
	return protocolRank(a.protocol) < protocolRank(b.protocol)
}

// protocolRank 是协议排序序位（http < h2c——同端口双协议时 http 组取
// RouterName 本体，机器面 h2c 组带后缀）。
func protocolRank(protocol string) int {
	if protocol == "h2c" {
		return 1
	}
	return 0
}

// routeKeySuffix 是分组键后缀：`~<port>[~h2c]`。'~' 不在服务名字符集
// （[a-z0-9._-]）内，结构上不可能与任何 RouterName 撞键；端口缺省（旧行
// 未同步）以 0 占位——键空间唯一性优先。
func routeKeySuffix(port, protocol string) string {
	p := port
	if p == "" {
		p = "0"
	}
	suffix := "~" + p
	if protocol == "h2c" {
		suffix += "~h2c"
	}
	return suffix
}

// routesFromLedger 把域名行聚合成路由集（app 名经 GetAppByID 解析；
// tombstone/删除中的 app 路由不发布）。IMPL-T1-1：分组键 = (app, service,
// port, protocol)——同一服务多后端各占一条 Route，第 2..n 组附
// `~<port>[~h2c]` 键后缀消歧（第 1 组保持 RouterName 本体，单后端服务
// 公式零变化）；确定性排序见 lessRouteGroup。
func routesFromLedger(ctx context.Context, st *state.Store, rows []state.Domain) ([]Route, error) {
	nameByAppID := map[string]string{}
	slugByAppID := map[string][2]string{}
	order := []domainGroupKey{}
	byKey := map[domainGroupKey]*Route{}
	for _, row := range rows {
		if _, ok := nameByAppID[row.AppID]; !ok {
			appRow, err := st.GetAppByID(ctx, row.AppID)
			if err != nil {
				nameByAppID[row.AppID] = "" // 不可解析（已删除）：路由不发布
				continue
			}
			nameByAppID[row.AppID] = appRow.Name
			slugByAppID[row.AppID] = [2]string{appRow.TeamSlug, appRow.ProjectSlug}
		}
		app := nameByAppID[row.AppID]
		if app == "" {
			continue
		}
		slugs := slugByAppID[row.AppID]
		k := domainGroupKey{app: app, service: row.Service, port: row.Port, protocol: row.Protocol}
		r, exists := byKey[k]
		if !exists {
			r = &Route{
				App: app, Service: row.Service, TeamSlug: slugs[0], PrjSlug: slugs[1],
				Port: row.Port, Protocol: row.Protocol,
			}
			byKey[k] = r
			order = append(order, k)
		}
		r.Domains = append(r.Domains, row.Domain)
	}
	sort.Slice(order, func(i, j int) bool { return lessRouteGroup(order[i], order[j]) })
	firstOfService := map[string]bool{}
	out := make([]Route, 0, len(order))
	for _, k := range order {
		r := byKey[k]
		svcKey := k.app + "\x00" + k.service
		if firstOfService[svcKey] {
			r.KeySuffix = routeKeySuffix(k.port, k.protocol)
		} else {
			firstOfService[svcKey] = true
		}
		r.Domains = uniqueSorted(r.Domains)
		out = append(out, *r)
	}
	return out, nil
}

// uniqueSorted 去重排序。
func uniqueSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// sortedKeys map 键字典序。
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
