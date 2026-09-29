// Package ingress 是入口与证书适配器（T2.15/T2.16；架构 §2.6 入口真源）：
// 平台自管 Traefik（global service 每节点，v0.1 单机 1 实例）、路由与证书
// 经 HTTP provider 由控制面集中下发、控制面内嵌 ACME（lego）集中签发。
//
// 纪律（Spike B 实测，spike/b/README.md §6——硬约束）：
//   - Traefik 只拒绝「显式空 map」与解析失败，**裸 {} 会清空全部路由**；
//     因此控制面合成配置必须保证 http.routers/services 键存在且非空，
//     先校验后写（Validate），坏配置不换入内存视图、Traefik 端永远取
//     不到残缺配置；
//   - 配置服务不可达时 Traefik 保留上一份成功配置（原生兜底，实测）；
//   - 路由发布严格晚于健康门（release-semantics §2.5 不变量）——本包的
//     PublishRoutes 由发布引擎在首健康→切流之后调用。
//
// 核心不出现第三方概念的一般化纪律在本包的反向落实：Traefik/lego 类型
// 只存在于本包内部，出口是 Route/DynamicConfig 等本包类型；发布引擎经
// engine.RoutePublisher 端口（本包 Manager 隐式实现）消费，不感知
// Traefik 存在。
package ingress

import (
	"fmt"
	"time"
)

// 平台钉版与保守缺省（architecture §2.6 入口行 + §2.5 连接治理行）。
const (
	// DefaultTraefikImage 是 Traefik 钉版镜像（Spike B 实测版本 v3.5；
	// 升级走镜像钉版变更 + 回归，不追 latest）。
	DefaultTraefikImage = "traefik:v3.5"
	// TraefikCertMountPath 是证书目录在 Traefik 容器内的只读挂载点
	//（tls.certificates 的 certFile/keyFile 以该路径书写）。
	TraefikCertMountPath = "/fleetly-certs"
	// DefaultServersTransportIdleTimeout 是 serversTransport 空闲连接
	// 回收时长。依据 V4 实测（architecture §2.5 连接治理行）：keep-alive
	// 连接池会复用已退出的任务，治理主键是应用侧优雅退出，
	// serversTransport 降为辅助——只治理空闲池（90s 默认 → 15s 保守值），
	// 对 in-flight 请求无效。
	DefaultServersTransportIdleTimeout = 15 * time.Second
	// DefaultServersTransportDialTimeout 是后端拨号超时（保守值）。
	DefaultServersTransportDialTimeout = 5 * time.Second
	// DefaultConfigTLSAddr 是配置端点 TLS 面的监听地址（ingress.
	// config_tls_addr；E1 多节点设计 §2.4——8423 上是全量动态配置 +
	// 内联证书的 HTTPS 面，各节点 Traefik 的 provider endpoint 经平台
	// 证书直连，静态 bearer token 鉴权沿用）。仅 base_domain 非空时由
	// 控制面启用（单节点 v0.1 形态维持明文 8422，行为逐字不变）；启用
	// 判定在 Manager.ConfigTLSEnabled，端点装配在 runtime ingress 服务
	// 壳（E1-3 接线）。
	DefaultConfigTLSAddr = "0.0.0.0:8423"
	// DefaultRenewBefore 是证书续期窗口（到期前 30 天，T2.16 交付物）。
	DefaultRenewBefore = 30 * 24 * time.Hour
	// DefaultRenewScanInterval 是续期扫描周期。
	DefaultRenewScanInterval = 12 * time.Hour
	// DefaultACMECADirURL 是 LE production 目录端点（测试用 Pebble：
	// 配置 ingress.acme.ca_dir_url 指向本地 pebble /dir）。
	DefaultACMECADirURL = "https://acme-v02.api.letsencrypt.org/directory"
	// acmeChallengePathPrefix 是 HTTP-01 挑战路径前缀（ACME 契约常量；
	// 各节点 Traefik 把它反代到控制面挑战应答端点，架构 §2.6 证书行）。
	acmeChallengePathPrefix = "/.well-known/acme-challenge/"
)

// Config 是入口适配器配置（config 键 ingress.*；缺省值经 Normalize 回落，
// 单一事实源在本包）。
type Config struct {
	// TraefikImage 是入口镜像（钉版；traefik_image）。
	TraefikImage string
	// HTTPPort / HTTPSPort 是宿主发布端口（host 模式 80/443；http_port/
	// https_port）。
	HTTPPort  int
	HTTPSPort int
	// ConfigAddr 是控制面配置端点监听地址（config_addr）。默认 0.0.0.0
	// ——Traefik 任务（容器 netns）须经宿主 IP 访问；鉴权 token 强制，
	// 非回环绑定的暴露面由 token 承担（取舍见 provider.go 注释）。
	ConfigAddr string
	// ConfigTLSAddr 是配置端点 TLS 面的监听地址（config_tls_addr；默认
	// 0.0.0.0:8423，E1 多节点设计 §2.4）。多节点形态下承载 /configs 的
	// HTTPS 面（全量动态配置 + 内联证书，平台证书 SAN 含 ctrl.<base>）；
	// 单节点（base_domain 空）不启用，缺省值仅是配置面就绪、零行为变化。
	ConfigTLSAddr string
	// ConfigAdvertiseIP 是下发给 Traefik 的控制面可达 IP
	//（config_advertise_ip；空 = 自动探测：Swarm advertise addr 优先，
	// 出口本地地址兜底）。Docker Desktop 形态 advertise addr 是 VM 内部
	// IP，须显式配置为宿主可达地址。
	ConfigAdvertiseIP string
	// TokenFile 是配置端点 bearer token 的持久化文件（token_file；首启
	// 生成，与 Traefik 静态配置 --providers.http.headers 同步传递）。
	TokenFile string
	// CertDir 是证书存储根目录（cert_dir；控制面侧明文 PEM，文件权限
	// 0600）。state-model §2.1：证书材料属控制面、独立备份目录。E1-2 起
	// 证书库是平台证书唯一真源：Traefik 的证书消费经动态配置内联下发
	//（D-MN-4，dynamic.go TLSCertificate），v0.1 的「本地卷 + seed 容器」
	// 分发路径已退役。
	CertDir string
	// BaseDomain 是平台域名（runtime base_domain 键的透传，E1 多节点设计
	// §2.2；ingress 侧派生面）。空 = 单节点 v0.1 形态：8423 TLS 面不启用、
	// provider endpoint 维持明文 8422、zot 部署器不活动、无平台路由段，
	// 行为逐字不变。非空 = 派生平台子域（ctrl/registry/console.<base>）+
	// 启用平台证书控制器 与 8423 配置端点 TLS 面（E1-3）+ zot registry 部署
	// 与 registry.<base> 路由段（E1-4，D-MN-5 配置即部署）。本包不做 DNS
	// 校验，字符串原样进入域名合成。
	BaseDomain string
	// RegistryImage 是 zot 镜像引用（registry.image 配置键的透传，E1-4）。
	// 空 = 回落钉版缺省 DefaultZotImage（多架构 index digest 钉定——R7
	// 纪律，digest 台账见 docs/runbooks/image-prepull.md；升级 = 换版票）。
	RegistryImage string
	// RegistryAuthFile 是 registry 凭据文件路径（registry.auth_file 装配期
	// 回落后的绝对路径；`<user>:<password>` 单行 0600——与 ingress token
	// 同形的平台生成文件，不入 SQLite；设计 §2.5）。zot 部署器读写（生成
	// + 派生 htpasswd/zot 配置工件）；空 = 回落相对缺省 fleetly-registry
	// .auth（生产装配恒注入数据根形态）。
	RegistryAuthFile string
	// ACME 是集中签发器配置。
	ACME ACMEConfig
	// RenewBefore 是续期窗口（到期前；renew_before_days）。
	RenewBefore time.Duration
	// RenewScanInterval 是续期扫描周期（renew_scan_seconds）。
	RenewScanInterval time.Duration
	// PollInterval 是 Traefik 拉取配置的轮询周期（静态配置参数；供收敛
	// 等待逻辑对齐节奏）。
	PollInterval time.Duration
	// ControlGatewayPort 是控制面网关（REST /v1 + Console /ui）端口，由
	// runtime 装配注入（HTTP 面 addr 的端口位；0 = 回落 8420）。console
	// 免端口直访路由段的后端端口（2026-09-24）——注意与 cfgPort（配置
	// 端点 8422/8423）区分。
	ControlGatewayPort int
	// ControlGatewayTLS 报告控制面网关是否以 TLS 形态服务（control_plane.
	// tls.mode != off，runtime 注入）。console 直访段据此选 https 后端 +
	// 跳过服务器认证的 transport（IP 端点无 SAN——F9 修订二同口径）。
	ControlGatewayTLS bool
}

// consoleGatewayPortFallback 是 ControlGatewayPort 未注入时的回落（与
// runtime DefaultHTTPAddr 端口位一致）。
const consoleGatewayPortFallback = 8420

// ACMEConfig 是集中 ACME 配置（config 键 ingress.acme.*）。
type ACMEConfig struct {
	// CADirURL 是 ACME 目录端点（ca_dir_url；默认 LE production，测试用
	// Pebble URL）。
	CADirURL string
	// Email 是 ACME 账号邮箱（email；空 = 无 contact 注册）。
	Email string
	// CAPoolFile 是 CA 根证书池 PEM（ca_pool_file；Pebble/私有 CA 场景
	// 信任自定义根）。空 = 系统信任池。
	CAPoolFile string
	// AccountKeyFile 是 ACME 账号私钥文件（account_key_file；空 =
	// <CertDir>/acme-account.key）。
	AccountKeyFile string
	// Enabled 报告是否启用集中签发（enabled；false = 只发布路由不下发
	// TLS 证书——无域名/离线环境的显式关闭位）。nil 缺省 = true。
	Enabled *bool
}

// ACMEEnabled 报告集中签发是否启用。
func (c ACMEConfig) ACMEEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Normalize 回落文档缺省值。
func (c Config) Normalize() Config {
	if c.TraefikImage == "" {
		c.TraefikImage = DefaultTraefikImage
	}
	if c.HTTPPort == 0 {
		c.HTTPPort = 80
	}
	if c.HTTPSPort == 0 {
		c.HTTPSPort = 443
	}
	if c.ConfigAddr == "" {
		c.ConfigAddr = "0.0.0.0:8422"
	}
	if c.ConfigTLSAddr == "" {
		c.ConfigTLSAddr = DefaultConfigTLSAddr
	}
	if c.TokenFile == "" {
		c.TokenFile = "fleetly-ingress.token"
	}
	if c.CertDir == "" {
		c.CertDir = "fleetly-certs"
	}
	if c.RegistryImage == "" {
		c.RegistryImage = DefaultZotImage
	}
	if c.RegistryAuthFile == "" {
		c.RegistryAuthFile = "fleetly-registry.auth"
	}
	if c.ACME.CADirURL == "" {
		c.ACME.CADirURL = DefaultACMECADirURL
	}
	if c.ACME.AccountKeyFile == "" {
		c.ACME.AccountKeyFile = ""
	}
	if c.RenewBefore <= 0 {
		c.RenewBefore = DefaultRenewBefore
	}
	if c.RenewScanInterval <= 0 {
		c.RenewScanInterval = DefaultRenewScanInterval
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	return c
}

// Validate 校验配置不变量（fail-fast 于装配期）。
func (c Config) Validate() error {
	n := c.Normalize()
	if n.HTTPPort <= 0 || n.HTTPSPort <= 0 {
		return fmt.Errorf("ingress: http/https ports must be positive")
	}
	if n.ACME.ACMEEnabled() && n.ACME.CADirURL == "" {
		return fmt.Errorf("ingress: acme.ca_dir_url is required when acme enabled")
	}
	return nil
}
