package ingress

// S3 公网子域开关的消费面（E3-6，设计 docs/design/2026-09-20-object-storage.md
// §2.6，D-S3-9「内网 + 可选公网」）：s3.mode=rustfs 且 s3.public_exposed=true
// 且 base_domain 非空时，三面条件收敛——
//
//	路由面   动态配置增 s3.<base> 路由（Host 规则 s3.<base>，websecure TLS
//	         → http://fleetly-rustfs:9000）；关闭即摘除（publishWithCerts
//	         的全量视图幂等比对自然收敛，与 F10 同口径：全量换入恒带证书段）。
//	证书面   平台证书 _fleetly-platform 多 SAN 集条件增第四 SAN s3.<base>；
//	         SAN 集是期望态函数（每次判定现读 s3 设置，不另存状态），集合
//	         增/缩 → needsRenewal 的「域名集变化即重签」既有收敛语义覆盖，
//	         零特判。
//	网络面   Traefik 解析后端名 fleetly-rustfs 需挂在 fleetly-rustfs-net
//	         overlay 上；开启 attach、关闭 detach（幂等，以服务实况网络集
//	         为基准合并——traefik.go 的实机回归纪律：网络目标集不由期望
//	         spec 重建）。
//
// 收敛载体：常驻控制器（runS3PublicController，registry 控制器同款节奏——设置变更
// 在一个扫描周期内收敛，稳态期只做网络挂接的幂等漂移复检；路由/证书面另
// 由 sweep 与发布路径对账兜底）。默认关 = 单节点零成本、零路由、零 SAN 变
// 化（单节点 v0.1 形态逐字不变）；开关门禁（rustfs 才可开、需 base_domain）
// 在设置保存面（state.ValidateS3Settings，E_S3_PUBLIC_REQUIRES_BASE_DOMAIN），
// 消费面对不满足形态一律按「未暴露」处理（fail-closed 到内网姿态）。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台 rustfs 路由常量（与 internal/rustfs 的 ServiceName/backendPort 同一
// 字面——入口层本地重写避免适配器反向依赖（registry 部署器同款纪律）；
// 一致性由 s3public_test.go 对 internal/rustfs 常量互钉）。
const (
	// rustfsServiceName 是托管 RustFS 的 swarm 服务名（后端 DNS 名 =
	// router/service 键覆写，platformRegistryRoute 同构）。
	rustfsServiceName = "fleetly-rustfs"
	// rustfsBackendPort 是 RustFS 的 S3 API 监听端口（overlay 内明文）。
	rustfsBackendPort = "9000"

	// s3PublicRetryInterval 是公网开关控制器 的收敛失败退避缺省（平台证书/
	// registry 控制器 同款注入缝——Manager.platformRetryInterval 可覆盖）。
	s3PublicRetryInterval = 30 * time.Second
	// s3PublicScanInterval 是已收敛后的设置变更侦测/漂移复检周期（rustfs
	// 控制器同款 60s——设置保存到公网面收敛的窗口与托管部署同量级）。
	s3PublicScanInterval = 60 * time.Second
)

// s3PublicExposed 报告公网子域开关的当前期望态（每次判定现读 s3 设置——
// 运行期设置不缓存长驻，§2.2 变更路径）。base_domain 为空（单节点形态）
// 恒 false；mode≠rustfs 恒 false（external 端点本就在公网，§2.6）。
func (m *Manager) s3PublicExposed(ctx context.Context) (bool, error) {
	if !m.ConfigTLSEnabled() {
		return false, nil
	}
	in, err := m.store.LoadS3Settings(ctx)
	if err != nil {
		return false, fmt.Errorf("ingress: load s3 settings: %w", err)
	}
	return state.NormalizeMode(in.Mode) == state.S3ModeRustfs && in.PublicExposed, nil
}

// platformS3Route 返回 s3.<base> 路由段（platformRegistryRoute 同构：App
// 取平台证书保留名——publishWithCerts 的按 app 挂证书循环零改动复用，平台
// 证书就绪即自动获得 443 路由与内联证书段；Name 覆写 router/service 键为
// fleetly-rustfs，后端 URL = 键 + :9000 = swarm 服务的 overlay VIP）。
func (m *Manager) platformS3Route() Route {
	return Route{
		App:     platformCertApp,
		Service: "s3",
		Port:    rustfsBackendPort,
		Domains: []string{"s3." + m.cfg.BaseDomain},
		Name:    rustfsServiceName,
	}
}

// withPlatformRoutes 追加平台路由段：registry 路由（E1-4，base_domain 门）
// + console 免端口直访路由（2026-09-24，base_domain 门）+ s3.<base> 路由
//（E3-6，公网开关门）。开关关闭或设置读取失败 → 不追加（fail-closed：设置
// 损坏时路由面缺席 = 公网面关闭，全量发布照常——设置读取故障不放大成全
// 平台入口发布失败；控制器侧同错误走退避重试并告警）。
func (m *Manager) withPlatformRoutes(ctx context.Context, routes []Route) []Route {
	if !m.ConfigTLSEnabled() {
		return routes
	}
	routes = append(routes, m.platformRegistryRoute())
	if r, ok := m.platformConsoleRoute(); ok {
		routes = append(routes, r)
	}
	exposed, err := m.s3PublicExposed(ctx)
	if err != nil {
		m.log.Warn("ingress: s3 public route omitted (settings unreadable; failing closed to internal-only)", "error", err)
		return routes
	}
	if exposed {
		routes = append(routes, m.platformS3Route())
	}
	return routes
}

// console 直访段常量：router/service 键与后端 transport 键（fleetly- 前缀
// 纪律；键空间与用户对象公式解耦）。
const (
	consoleRouterName     = "fleetly-console"
	consoleTransportName  = "fleetly-console-transport"
	consoleIngressBackend = "console"
)

// platformConsoleRoute 返回 console 免端口直访路由段（2026-09-24 用户实报
// 易错点收口：https://console.<base>/ 免 8420 端口与 /ui/ 前缀两个输入位）。
// Host(`console.<base>`) → 控制面网关（advertise 地址 : ControlGatewayPort
// ——网关端口由 runtime 注入，与配置端点 8422/8423 无关）；根路径 302 到
// /ui/（RootRedirect 中间件）。网关 TLS 形态（ControlGatewayTLS，
// control_plane.tls.mode != off）时后端走 https + 跳过服务器认证的专用
// transport（IP 端点无 SAN——与 providerEndpoint F9 修订二同口径，服务器
// 认证由 VPC 边界承担）；明文形态走 http。App=平台证书保留名：
// publishWithCerts 的按 app 挂证书循环自动挂 443 路由与内联证书段
//（registry 路由同构，零特判）。advertiseIP 未定（EnsureTraefik 未跑过）
// 时不追加——路由面无地址可指；控制器收敛链恒在 EnsureTraefik 之后重发布，
// 最终一致。前提（诚实记录）：网关须绑定非回环（远程访问 Console 的安装
// 形态天然满足；纯回环绑定下本路由 502）。
func (m *Manager) platformConsoleRoute() (Route, bool) {
	m.mu.Lock()
	advertise := m.advertiseIP
	m.mu.Unlock()
	if advertise == "" {
		return Route{}, false
	}
	port := m.cfg.ControlGatewayPort
	if port <= 0 {
		port = consoleGatewayPortFallback
	}
	scheme := "http"
	transport := ""
	if m.cfg.ControlGatewayTLS {
		scheme = "https"
		transport = consoleTransportName
	}
	return Route{
		App:          platformCertApp,
		Service:      consoleIngressBackend,
		Domains:      []string{"console." + m.cfg.BaseDomain},
		Name:         consoleRouterName,
		BackendURL:   scheme + "://" + net.JoinHostPort(advertise, fmt.Sprint(port)),
		Transport:    transport,
		RootRedirect: "/ui/",
	}, true
}

// platformDomainsWithS3 是平台证书 SAN 期望态集（PlatformDomains 的
// 设置感知形态）：acme.wildcard=true 时期望集变
// [*.base, console.<base>, ctrl.<base>, registry.<base>]（W5-S3，
// D-V3W5-4——通配一张覆盖全部 app 域名与平台子域，s3.<base> 亦被通配
// 收编，不再单列第四 SAN）；否则公网开关开启时条件增第四 SAN s3.<base>。
// SAN 集不另存状态——证书库台账里的既有 SAN 集与之失配即触发重签发
//（needsRenewal 的「域名集变化即重签」），增/缩双向同语义。
func (m *Manager) platformDomainsWithS3(ctx context.Context) ([]string, error) {
	domains := m.PlatformDomains()
	wildcard, err := m.acmeWildcard(ctx)
	if err != nil {
		return nil, err
	}
	if wildcard {
		return []string{
			"*." + m.cfg.BaseDomain,
			"console." + m.cfg.BaseDomain,
			"ctrl." + m.cfg.BaseDomain,
			"registry." + m.cfg.BaseDomain,
		}, nil
	}
	exposed, err := m.s3PublicExposed(ctx)
	if err != nil {
		return nil, err
	}
	if exposed {
		domains = append(domains, "s3."+m.cfg.BaseDomain)
	}
	return domains, nil
}

// acmeWildcard 读 acme.wildcard 开关（每次判定现读——运行期设置不缓存
// 长驻；设置读取失败显式报错，调用方按退避重试/省略段处理）。
func (m *Manager) acmeWildcard(ctx context.Context) (bool, error) {
	in, err := m.store.LoadAcmeSettings(ctx)
	if err != nil {
		return false, fmt.Errorf("ingress: load acme settings: %w", err)
	}
	return in.Wildcard, nil
}

// convergeS3Public 执行一拍公网面收敛（开关状态变化后的全链；幂等）：
// Traefik 就绪（lastSpec 前置）→ 网络挂接/摘除 → 视图重发布（路由增/摘）
// → 平台证书 SAN 期望态对账（集合变化即重签发）→ 新证书换入视图。
// ACME 关闭时证书步惰性告警（路由/网络面照常收敛——与平台证书控制器 的
// inert 口径一致：无签发面就没有 SAN 面，诚实降级不静默）。
func (m *Manager) convergeS3Public(ctx context.Context, exposed bool) error {
	if err := m.EnsureTraefik(ctx); err != nil {
		return err
	}
	if err := m.convergeS3PublicNetwork(ctx, exposed); err != nil {
		return err
	}
	if err := m.republishAll(ctx); err != nil {
		return err
	}
	if !m.cfg.ACME.ACMEEnabled() {
		m.log.Warn("ingress: s3 public exposure converged without certificate SAN convergence (acme disabled; the platform certificate face is inert)")
		return nil
	}
	before, err := m.certs.Load(platformCertApp)
	beforeSHA := ""
	switch {
	case err == nil:
		beforeSHA = before.SHA256
	case errors.Is(err, os.ErrNotExist):
		before = nil
	default:
		return fmt.Errorf("ingress: load platform cert: %w", err)
	}
	if _, err := m.ensurePlatformCertificate(ctx, before != nil); err != nil {
		return err
	}
	after, err := m.certs.Load(platformCertApp)
	if err != nil {
		return fmt.Errorf("ingress: load platform cert: %w", err)
	}
	if after.SHA256 != beforeSHA {
		// SAN 集增/缩触发重签发：新证书换入视图（publishWithCerts 恒带证书
		// 段——F10 口径，路由与既有 app 的 TLS 面不受影响）。
		if err := m.publishWithCerts(ctx); err != nil {
			return err
		}
		m.log.Info("ingress: platform certificate reissued for the s3 public exposure toggle",
			"exposed", exposed, "sha256", after.SHA256)
	}
	return nil
}

// convergeS3PublicNetwork 收敛 Traefik 对 fleetly-rustfs-net 的挂接态：
// 开启 attach（后端 VIP 可达面先于路由就位）、关闭 detach（路由摘除后端
// 面再收回——convergeS3Public 的次序保证）。稳态拍亦走本函数做幂等漂移
// 复检（手工 detach/底座漂移在下一个扫描周期自愈）。
func (m *Manager) convergeS3PublicNetwork(ctx context.Context, exposed bool) error {
	if exposed {
		return m.attachPlatformNetworkIfPresent(ctx, state.RustfsNetworkName)
	}
	return m.detachPlatformNetwork(ctx, state.RustfsNetworkName)
}

// attachPlatformNetworkIfPresent 是平台网络挂接的守恒形态：与
// attachNetworkByName 的唯一差别是**不代建网络**——NetworkEnsure 会创建非
// attachable 的普通 overlay，而 fleetly-rustfs-net 必须由 rustfs 收敛循环以
// attachable 形态创建（restic 上传轨/探针一次性容器经它入网，设计 §2.5/
// §2.6）；此处抢建会在「mode=rustfs + public_exposed 同拍保存」的竞态下
// 破坏该不变量。网络未建（rustfs 收敛循环尚未收敛）→ 可重试错误，收敛循环退避。
func (m *Manager) attachPlatformNetworkIfPresent(ctx context.Context, netName string) error {
	netID, err := m.docker.NetworkID(ctx, netName)
	if err != nil {
		return fmt.Errorf("ingress: platform network %s not present yet (rustfs convergence pending): %w", netName, err)
	}
	return m.attachNetworkID(ctx, netName, netID)
}

// detachPlatformNetwork 确保 Traefik **不**挂接指定平台网络（公网开关关闭
// 侧；幂等：未挂接即 no-op）。网络目标集以服务实况为基准做减法（其余挂载
// ——app 网络等——原样保留，specWithNetworks 整组替换语义；期望 spec 不得
// 参与重建，traefik.go 实机回归纪律）。网络本体不动（rustfs 收敛循环的数据
// 面语义：禁用保留网络与卷）。
func (m *Manager) detachPlatformNetwork(ctx context.Context, netName string) error {
	netID, err := m.docker.NetworkID(ctx, netName)
	if err != nil {
		// 网络不存在 = 必然未挂接（幂等空操作；错误文本不上浮——关闭态的
		// 「网络缺位」就是期望态本身）。
		return nil
	}
	cur, err := m.docker.ServiceInspect(ctx, IngressServiceName)
	if err != nil {
		return err
	}
	if !cur.Exists {
		return nil
	}
	found := false
	targets := make([]string, 0, len(cur.Networks))
	for _, n := range cur.Networks {
		if n == netID {
			found = true
			continue
		}
		targets = append(targets, n)
	}
	if !found {
		return nil
	}
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
	m.log.Info("ingress: traefik detached from platform network (s3 public exposure off)",
		"network", netName, "attached_total", len(spec.TaskTemplate.Networks))
	return nil
}

// runS3PublicController 是公网子域开关的常驻收敛循环（Manager.Run 启动的独立
// goroutine；ctx 取消返回）。W5-S3 起同拍守望 acme.wildcard（两者都只改
// 平台证书 SAN 期望态集 + 平台路由段——收敛链共用 convergeS3Public：证书
// 集合变化经 ensurePlatformCertificate 的「域名集变化即重签」判据收敛）：
//
//	base_domain 为空 → 不启动（单节点形态零成本，开关在设置面即被拒）；
//	状态变化拍 → convergeS3Public 全链（失败退避重试，registry 控制器 同款）；
//	稳态拍 → 只做网络挂接的幂等漂移复检（路由/证书面由 sweep 与发布路径
//	对账兜底——设置现读语义使任何一次全量发布都按当前开关取态）。
func (m *Manager) runS3PublicController(ctx context.Context) {
	if !m.ConfigTLSEnabled() {
		return
	}
	retry := m.platformRetryInterval
	if retry <= 0 {
		retry = s3PublicRetryInterval
	}
	scan := m.s3PublicScanInterval
	if scan <= 0 {
		scan = s3PublicScanInterval
	}
	converged := false
	hasState := false
	lastExposed := false
	lastWildcard := false
	for {
		exposed, err := m.s3PublicExposed(ctx)
		wildcard, werr := m.acmeWildcard(ctx)
		switch {
		case err != nil || werr != nil:
			converged = false
			if err != nil {
				m.log.Warn("ingress: s3 public exposure check deferred (retrying)", "error", err)
			}
			if werr != nil {
				m.log.Warn("ingress: acme wildcard check deferred (retrying)", "error", werr)
			}
		case !hasState || exposed != lastExposed || wildcard != lastWildcard:
			if cerr := m.convergeS3Public(ctx, exposed); cerr != nil {
				converged = false
				m.log.Warn("ingress: platform face convergence deferred (retrying)",
					"exposed", exposed, "wildcard", wildcard, "error", cerr)
			} else {
				converged = true
				switch {
				case hasState && wildcard != lastWildcard:
					m.log.Info("ingress: platform certificate domain set converged for the wildcard toggle",
						"wildcard", wildcard, "wildcard_domain", "*."+m.cfg.BaseDomain,
						"note", "platform certificate reissued for the new SAN set; app routing is unchanged (443 is covered by the wildcard via SNI)")
				case exposed:
					m.log.Info("ingress: s3 public subdomain exposed",
						"route", "s3."+m.cfg.BaseDomain, "backend", rustfsServiceName+":"+rustfsBackendPort,
						"note", "publicly reachable surface +1; authentication = rustfs credentials")
				case hasState:
					m.log.Info("ingress: s3 public subdomain withdrawn (internal-only posture restored)",
						"route", "s3."+m.cfg.BaseDomain)
				default:
					m.log.Info("ingress: s3 public exposure converged (off; internal-only posture)",
						"service", rustfsServiceName)
				}
				hasState = true
				lastExposed = exposed
				lastWildcard = wildcard
			}
		default:
			if derr := m.convergeS3PublicNetwork(ctx, exposed); derr != nil {
				converged = false
				m.log.Warn("ingress: s3 public network drift converge deferred (retrying)", "error", derr)
			} else {
				converged = true
			}
		}
		if !sleepCtx(ctx, retryOrScan(retry, scan, converged)) {
			return
		}
	}
}
