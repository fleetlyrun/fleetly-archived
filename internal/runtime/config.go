package runtime

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/gitserver"
	"github.com/fleetlyrun/fleetly/internal/ingress"
	"github.com/fleetlyrun/fleetly/internal/logs"
	"github.com/fleetlyrun/fleetly/internal/metrics"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/statebackup"
)

// DefaultHTTPAddr 是 HTTP 面的缺省监听地址（导出供 cmd/fleetlyd 的 -addr
// flag 缺省值与本包 NewConfig 回落共用——单一事实源）：默认只绑回环，
// 避免控制面未配置时暴露公网（架构 §4.2 安全默认基线）。可用 -addr
// 覆盖，或经 -c config.yaml 中的 addr 键覆盖（显式 flag 优先于配置文件）。
//
// defaultGRPCAddr 同为 gRPC 面缺省监听地址（仅包内消费：NewConfig 回落
// 与 gateway 转发目标推导），经 config.yaml 的 grpc.addr 配置。
const (
	DefaultHTTPAddr = "127.0.0.1:8420"
	defaultGRPCAddr = "127.0.0.1:8421"
	// defaultDBPath 是状态库缺省路径（config state.db_path 的回落值）。
	defaultDBPath = "./fleetly.db"
)

// AppConfig 应用配置，与 config.yaml 及 flags 对应（lynx Config，默认
// Viper 适配）。随阶段推进只增字段，不回收键名。
type AppConfig struct {
	// Addr 是 HTTP 面（框架 healthz 端点 + grpc-gateway 挂载的 REST /v1/**）
	// 的监听地址。
	Addr string `mapstructure:"addr"`
	// BaseDomain 是平台域名（config 键 base_domain；E1 多节点设计 §2.2，
	// V2-7 可选安装项）。空 = 单节点 v0.1 形态（本地 digest、明文 8422
	// provider，行为逐字不变）；非空 = 控制面派生三平台子域
	//（ctrl/registry/console.<base>）、启用 8423 配置端点 TLS 面与平台
	// 证书 duty（E1-2/E1-3：证书经动态配置内联下发、Traefik endpoint 切
	// https://ctrl.<base>:8423）、多节点 join 门禁放行（D-MN-13：join 时
	// 为空即 E_MULTI_NODE_REQUIRES_BASE_DOMAIN——E1-8 接线）。
	BaseDomain string `mapstructure:"base_domain"`
	// GRPC 是 gRPC 面配置（server.v1 服务承载于此，gateway 反向代理目标）。
	GRPC GRPCConfig `mapstructure:"grpc"`
	// State 是状态层配置（config 键 state.*）。
	State StateConfig `mapstructure:"state"`
	// Secrets 是平台密钥配置（config 键 secrets.*，architecture §2.3：
	// envelope 主密钥存控制面主机文件、权限保护、与备份数据分离）。
	Secrets SecretsConfig `mapstructure:"secrets"`
	// Build 是构建管线配置（config 键 build.*，T2.8）。
	Build BuildConfig `mapstructure:"build"`
	// Engine 是发布引擎配置（config 键 engine.*，T2.10/T2.11；治理参数
	// v0.1 平台默认——文件缺省即文档默认，见 engine.Config.Normalize）。
	Engine EngineConfig `mapstructure:"engine"`
	// Ingress 是入口/证书配置节（config 键 ingress.*，T2.15/T2.16；
	// Traefik 部署 + 配置端点 + 集中 ACME——缺省值经 ingress.Config.
	// Normalize 回落，单一事实源在 internal/ingress）。
	Ingress IngressConfig `mapstructure:"ingress"`
	// Registry 是平台 registry（zot）配置节（config 键 registry.*；E1
	// 多节点设计 §2.5。缺省零值 = 部署器未接线时的显式空缺——zot 镜像
	// 钉版缺省随 E1-4 部署器票据落定，届时以 image-prepull 台账增行）。
	Registry RegistryConfig `mapstructure:"registry"`
	// Join 是节点加入配置节（config 键 join.*；E1 多节点设计 §2.3/
	// D-MN-1）。
	Join JoinConfig `mapstructure:"join"`
	// Logs 是日志管线配置节（config 键 logs.*，T2.20；缺省值经 logs.
	// Config.Normalize 回落——单一事实源在 internal/logs）。
	Logs LogsConfig `mapstructure:"logs"`
	// Metrics 是托管 metrics 栈配置节（config 键 metrics.*，E6 W5-S3；
	// D-W5-2 opt-in——mode 是运行期设置不走本节，本节只承载 retention）。
	Metrics MetricsConfig `mapstructure:"metrics"`
	// Git 是 git push(SSH) 触发入口配置节（config 键 git.*，T2.19；缺省值
	// 经 gitserver.Config.Normalize 回落——单一事实源在 internal/gitserver）。
	Git GitConfig `mapstructure:"git"`
	// Webhook 是 webhook 触发入口配置节（config 键 webhook.*，T2.19）。
	Webhook WebhookConfig `mapstructure:"webhook"`
	// Console 是 Console 端静态托管配置节（config 键 console.*，T2.21）。
	Console ConsoleConfig `mapstructure:"console"`
	// ControlPlane 是控制面自身配置节（config 键 control_plane.*，E7 同批
	// V2-8；本节只承载 TLS 基座——8420/8421 双面的服务端证书面）。
	ControlPlane ControlPlaneConfig `mapstructure:"control_plane"`
	// Terminal 是 Web 终端功能配置节（config 键 terminal.*，E7 W5-S6）。
	Terminal TerminalConfig `mapstructure:"terminal"`
	// Backup 是状态备份配置节（config 键 backup.*，T2.22；缺省值经
	// statebackup.Config.Normalize 回落——单一事实源在 internal/statebackup）。
	Backup BackupConfig `mapstructure:"backup"`
	// Auth 是认证面配置节（config 键 auth.*，v0.3 W2-S4 会话 TTL 收口）。
	Auth AuthConfig `mapstructure:"auth"`
}

// AuthConfig 是认证面配置节（config 键 auth.*，rbac-teams §2.2）。
type AuthConfig struct {
	// SessionTTLHours 是会话滑动窗口时长小时数（auth.session_ttl_hours；
	// 缺省/非正值 = 168 即 7 天滑动。绝对寿命上限 30 天是 state 层硬封顶
	// 常量（state.MaxSessionLifetime）——本项只能调短生效，配置超过上限时
	// 封顶到上限）。
	SessionTTLHours int `mapstructure:"session_ttl_hours"`
}

// SessionTTL 返回会话滑动窗口时长（config auth.session_ttl_hours；非正值回
// 落 state.DefaultSessionTTL = 7 天；超过 30 天绝对上限封顶）。
func (c *AppConfig) SessionTTL() time.Duration {
	ttl := time.Duration(c.Auth.SessionTTLHours) * time.Hour
	if ttl <= 0 {
		return state.DefaultSessionTTL
	}
	if ttl > state.MaxSessionLifetime {
		return state.MaxSessionLifetime
	}
	return ttl
}

// TerminalConfig 是 Web 终端功能配置节（config 键 terminal.*，E7 W5-S6）。
// 静态配置项——改动需重启。安全基线：admin-only（terminal 独立 scope）+
// 限额（per-token 2 / 全局 8）+ 会话时限已足，不另设 opt-in——缺省 true。
type TerminalConfig struct {
	// Enabled 报告是否启用 Web 终端（terminal.enabled；缺省 true）。false =
	// duty 移除 relay 服务 + API 报 E_TERMINAL_DISABLED（Console 面板按
	// 禁用态渲染）。
	Enabled *bool `mapstructure:"enabled"`
}

// TerminalEnabled 返回归一后的终端开关（nil = 缺省启用）。
func (c *AppConfig) TerminalEnabled() bool {
	return c.Terminal.Enabled == nil || *c.Terminal.Enabled
}

// BackupConfig 是状态备份配置节（config 键 backup.*，T2.22）。dir 留空时
// 由装配点回落 <state 库同目录>/backups（依赖 state 库路径，缺省在
// AppConfig.BackupRoot 计算——与 GitRoot 同款装配期回落）。
type BackupConfig struct {
	// Dir 是备份根目录（backup.dir）。主密钥（fleetly.key）绝不进备份目录
	// ——manifest 只记密钥文件 sha256 指纹；配置把密钥放进备份目录会被
	// statebackup.NewManager 构造期拒绝（fail-fast）。
	Dir string `mapstructure:"dir"`
	// Keep 是保留份数上限（backup.keep；非正值回落 DefaultKeep=7）。
	Keep int `mapstructure:"keep"`
}

// BackupSettings 把 backup.* 配置节翻译为备份核心配置（statebackup.Config，
// 缺省值经 Normalize 回落——单一事实源在 internal/statebackup）。
func (c *AppConfig) BackupSettings() statebackup.Config {
	return statebackup.Config{Dir: c.Backup.Dir, Keep: c.Backup.Keep}
}

// BackupRoot 回落备份根目录缺省值（与 state 库同目录下 backups/）。
func (c *AppConfig) BackupRoot() string {
	if c.Backup.Dir != "" {
		return c.Backup.Dir
	}
	return filepath.Join(filepath.Dir(c.DBPath()), "backups")
}

// BootstrapTokenPath 返回 bootstrap token 文件路径（B5：数据根 = state 库
// 同目录，与 BackupRoot/GitRoot 同款装配期回落）。首启种子写此文件
// （0600），不再打印进日志/journald。
func (c *AppConfig) BootstrapTokenPath() string {
	return filepath.Join(filepath.Dir(c.DBPath()), "bootstrap-token")
}

// DeploymentsRoot 返回部署 compose 持久化根目录（A7，S18：<数据根>/
// deployments——数据根 = state 库同目录，与 BackupRoot/GitRoot 同款派生；
// 布局单一事实源在 state.DeploymentsRoot）。janitor 按 30 天窗清理终态
// 部署的目录。
func (c *AppConfig) DeploymentsRoot() string {
	return state.DeploymentsRoot(c.DBPath())
}

// ConsoleConfig 是 Console 静态托管配置节（config 键 console.*，T2.21）。
// Console SPA 的数据面恒走 REST /v1（鉴权在 gRPC 拦截器链，不因静态托管
// 放宽）；静态资源豁免精确到 /ui/ 前缀（例外清单登记见 gateway.go）。
type ConsoleConfig struct {
	// StaticDir 是 Console SPA 构建产物的静态根目录（console.static_dir）。
	// 空 = 关闭（/ui/ 前缀不分派——豁免面 = 分派面，缺省零暴露）。指向
	// console/dist（`pnpm build` 产物）时 gateway 在 /ui/ 前缀托管静态文件
	// 并做 SPA 回退（未命中文件的路径一律回 index.html）。
	StaticDir string `mapstructure:"static_dir"`
}

// ControlPlaneConfig 是控制面自身配置节（config 键 control_plane.*，E7
// 同批 V2-8：控制面 TLS 专项设计 §3.1）。静态配置项——mode 改动需重启。
type ControlPlaneConfig struct {
	// TLS 是控制面 TLS 配置子节（config 键 control_plane.tls.*）。
	TLS ControlPlaneTLSConfig `mapstructure:"tls"`
}

// ControlPlaneTLSConfig 是控制面双面（8420 HTTP / 8421 gRPC）TLS 配置
//（E7 同批 V2-8，设计 §3.1：off = 今日行为零变化；platform 复用平台证书
// ——LE 签发/续期由平台证书 duty 既有机制承接；manual = 显式证书文件对）。
// 不含 mTLS/客户端证书（v0.2 不做——Bearer 仍是唯一认证）。
type ControlPlaneTLSConfig struct {
	// Mode 是 TLS 模式（control_plane.tls.mode）：off（缺省/空串，明文）|
	// platform（复用平台证书，需 base_domain 非空）| manual（cert_file/
	// key_file 显式证书对）。
	Mode string `mapstructure:"mode"`
	// CertFile 是 manual 模式的证书链 PEM 路径（control_plane.tls.cert_file）。
	CertFile string `mapstructure:"cert_file"`
	// KeyFile 是 manual 模式的私钥 PEM 路径（control_plane.tls.key_file）。
	KeyFile string `mapstructure:"key_file"`
	// MinVersion 是最低 TLS 版本（control_plane.tls.min_version）：空串 =
	// tls1.2（缺省）；取值 tls1.2 | tls1.3。
	MinVersion string `mapstructure:"min_version"`
}

// 控制面 TLS 模式常量（ControlPlaneTLSConfig.Mode 的归一取值）。
const (
	ControlPlaneTLSOff      = "off"
	ControlPlaneTLSPlatform = "platform"
	ControlPlaneTLSManual   = "manual"
)

// TLSMode 返回归一后的控制面 TLS 模式：空串回落 off（缺省 = 今日明文行为
// 逐字不变）；未知值原样返回（由 ValidateControlPlaneTLS 启动期 loud-fail
// ——校验与归一分离，本方法不做报错）。
func (c *AppConfig) TLSMode() string {
	switch c.ControlPlane.TLS.Mode {
	case "":
		return ControlPlaneTLSOff
	default:
		return c.ControlPlane.TLS.Mode
	}
}

// TLSMinVersion 返回最低 TLS 协议版本（tls.Config.MinVersion 用）：空串 =
// TLS 1.2（缺省基线，gosec 口径）；未知值返回 0（由 ValidateControlPlaneTLS
// 报错拦截——0 不是合法 tls.Config 版本常量，防御性不至于静默放行）。
func (c *AppConfig) TLSMinVersion() uint16 {
	switch c.ControlPlane.TLS.MinVersion {
	case "", "tls1.2":
		return tls.VersionTLS12
	case "tls1.3":
		return tls.VersionTLS13
	default:
		return 0
	}
}

// ValidateControlPlaneTLS 校验控制面 TLS 配置（启动期 loud-fail，装配期
// NewControlPlaneTLS 调用）：mode 取值合法；platform 需 base_domain 非空
//（平台证书 duty 依赖它签发/落盘）；manual 需 cert_file/key_file 双路径
// 且文件可读。off（缺省）无约束——cert_file 等键在 off 下被忽略（不报错，
// 键位只增惯例下的宽容口径）。
func (c *AppConfig) ValidateControlPlaneTLS() error {
	switch mode := c.TLSMode(); mode {
	case ControlPlaneTLSOff:
		return nil
	case ControlPlaneTLSPlatform:
		if c.BaseDomain == "" {
			return fmt.Errorf("control_plane.tls.mode=platform requires base_domain to be set (the platform certificate duty issues and stores the certificate under it)")
		}
		if c.TLSMinVersion() == 0 {
			return fmt.Errorf("control_plane.tls.min_version: unknown value %q (supported: tls1.2, tls1.3)", c.ControlPlane.TLS.MinVersion)
		}
		return nil
	case ControlPlaneTLSManual:
		if c.ControlPlane.TLS.CertFile == "" || c.ControlPlane.TLS.KeyFile == "" {
			return fmt.Errorf("control_plane.tls.mode=manual requires control_plane.tls.cert_file and control_plane.tls.key_file")
		}
		for _, path := range []string{c.ControlPlane.TLS.CertFile, c.ControlPlane.TLS.KeyFile} {
			f, err := os.Open(path) //nolint:gosec // G304：路径为操作者配置文件的显式配置项
			if err != nil {
				return fmt.Errorf("control_plane.tls: %w", err)
			}
			_ = f.Close()
		}
		if c.TLSMinVersion() == 0 {
			return fmt.Errorf("control_plane.tls.min_version: unknown value %q (supported: tls1.2, tls1.3)", c.ControlPlane.TLS.MinVersion)
		}
		return nil
	default:
		return fmt.Errorf("control_plane.tls.mode: unknown value %q (supported: off, platform, manual)", mode)
	}
}

// LogsConfig 是日志管线配置节（config 键 logs.*）。字段与 internal/logs.
// Config 一一对应；缺省回落 internal/logs（fleetly-logs 目录 / 保留 7 天 /
// 轮询 2s / ring 1000）。
type LogsConfig struct {
	// Dir 是落盘根目录（logs.dir；缺省 ./fleetly-logs）。
	Dir string `mapstructure:"dir"`
	// RetentionDays 是落盘保留天数（logs.retention_days；缺省 7——架构
	// §2.3 数据保留：应用日志 7 天轮转）。
	RetentionDays int `mapstructure:"retention_days"`
	// ScanIntervalMillis 是采集轮询周期毫秒数（logs.scan_interval_millis；
	// 缺省 2000）。
	ScanIntervalMillis int `mapstructure:"scan_interval_millis"`
	// RingSize 是 per app-service 内存环形缓冲深度（logs.ring_size；缺省
	// 1000）。
	RingSize int `mapstructure:"ring_size"`
}

// LogsSettings 把 logs.* 配置节翻译为日志管线核心配置（logs.Config，
// 缺省值经 Normalize 回落——单一事实源在 internal/logs）。
func (c *AppConfig) LogsSettings() logs.Config {
	return logs.Config{
		Dir:                c.Logs.Dir,
		RetentionDays:      c.Logs.RetentionDays,
		ScanIntervalMillis: c.Logs.ScanIntervalMillis,
		RingSize:           c.Logs.RingSize,
	}.Normalize()
}

// MetricsConfig 是托管 metrics 栈配置节（config 键 metrics.*，E6 W5-S3）。
// metrics.mode 是运行期设置（platform_settings，D-W5-2 opt-in）不走本节；
// 本节只承载 VM 数据保留天数（缺省 14——单一事实源在 internal/metrics
// DefaultRetentionDays）。
type MetricsConfig struct {
	// RetentionDays 是 VM -retentionPeriod 对齐天数（metrics.retention_days；
	// 缺省 14）。非正值回落缺省（不允许误配成 0 静默关闭保留）。
	RetentionDays int `mapstructure:"retention_days"`
}

// MetricsSettings 把 metrics.* 配置节翻译为 metrics 栈核心配置（metrics.
// Config，缺省值经 Normalize 回落——单一事实源在 internal/metrics）。
func (c *AppConfig) MetricsSettings() metrics.Config {
	return metrics.Config{
		RetentionDays: c.Metrics.RetentionDays,
	}.Normalize()
}

// EngineConfig 是发布引擎配置节（config 键 engine.*）。默认值与
// release-semantics §2.8 治理参数表一致（deployTimeout=300s、观察窗 60s、
// 水位判定 10s、轮询 2s），经 engine.Config.Normalize 回落。
type EngineConfig struct {
	// DeployTimeoutSeconds 是 L2 发布看门狗秒数（engine.deploy_timeout_seconds；
	// 缺省 300——含 PENDING/停滞，有效值 ≥ health 预算）。
	DeployTimeoutSeconds int `mapstructure:"deploy_timeout_seconds"`
	// ObserveSeconds 是 L3 观察窗秒数（engine.observe_seconds；缺省 60）。
	ObserveSeconds int `mapstructure:"observe_seconds"`
	// ReplicasBelowSeconds 是观察窗副本水位不足判定的持续秒数
	//（engine.replicas_below_seconds；缺省 10）。
	ReplicasBelowSeconds int `mapstructure:"replicas_below_seconds"`
	// PollSeconds 是引擎轮询周期秒数（engine.poll_seconds；缺省 2）。
	PollSeconds int `mapstructure:"poll_seconds"`
	// DriftIntervalSeconds 是运行域漂移检测扫描周期秒数
	//（engine.drift_interval_seconds；缺省 30，T2.13）。
	DriftIntervalSeconds int `mapstructure:"drift_interval_seconds"`
}

// EngineSettings 把 engine.* 配置节翻译为引擎核心配置（engine.Config，
// 缺省值经 Normalize 回落——单一事实源在 internal/engine）。
func (c *AppConfig) EngineSettings() engine.Config {
	return engine.Config{
		DeployTimeout:    time.Duration(c.Engine.DeployTimeoutSeconds) * time.Second,
		ObserveWindow:    time.Duration(c.Engine.ObserveSeconds) * time.Second,
		ReplicasBelowFor: time.Duration(c.Engine.ReplicasBelowSeconds) * time.Second,
		PollInterval:     time.Duration(c.Engine.PollSeconds) * time.Second,
		DriftInterval:    time.Duration(c.Engine.DriftIntervalSeconds) * time.Second,
	}
}

// BuildConfig 是构建管线配置节（config 键 build.*）。缺省值经
// build.Config.Normalize 回落（并发 2、限额 1GiB/1.5CPU、缓存命名卷）。
type BuildConfig struct {
	// BuildkitHost 是 buildkit 端点（build.buildkit_host）；空 = 缺省
	// docker-container://fleetly-buildkit（平台自管容器）。指向外部
	// buildkitd（tcp://…）时应把 manage_daemon 置 false。
	BuildkitHost string `mapstructure:"buildkit_host"`
	// ManageDaemon 报告平台是否自管 buildkitd 容器（build.manage_daemon，
	// 缺省 true；外部端点形态置 false）。
	ManageDaemon *bool `mapstructure:"manage_daemon"`
	// DaemonContainerName 是自管 buildkitd 容器名（build.daemon_container_name）。
	DaemonContainerName string `mapstructure:"daemon_container_name"`
	// CacheVolume 是 buildkitd 内部工作缓存的持久化命名卷
	// （build.cache_volume，挂 /var/lib/buildkit）。
	CacheVolume string `mapstructure:"cache_volume"`
	// CacheDir 是 local cache 导入/导出的宿主目录（build.cache_dir；
	// 客户端侧数据根，缺省 ./build-cache）。
	CacheDir string `mapstructure:"cache_dir"`
	// ArtifactsDir 是 plan JSON / 构建日志归档根目录（build.artifacts_dir）。
	ArtifactsDir string `mapstructure:"artifacts_dir"`
	// Concurrency 是构建队列并发上限（build.concurrency；缺省 2，天花板 8）。
	Concurrency int `mapstructure:"concurrency"`
	// MemoryBytes 是 buildkitd 容器内存限额（build.memory_bytes；缺省 1GiB）。
	MemoryBytes int64 `mapstructure:"memory_bytes"`
	// CPUS 是 buildkitd 容器 CPU 限额（build.cpus；缺省 1.5）。
	CPUS float64 `mapstructure:"cpus"`
	// PollSeconds 是队列扫描周期秒数（build.poll_seconds；缺省 2）。
	PollSeconds int `mapstructure:"poll_seconds"`
	// TimeoutSeconds 是单条构建执行超时预算秒数（build.timeout_seconds；
	// 缺省 1800=30min。超时 → 终态 failed（E_BUILD_FAILED）——挂起构建
	// 不永久占用并发槽）。
	TimeoutSeconds int `mapstructure:"timeout_seconds"`
	// ArtifactsRetentionDays 是产物归档目录保留天数（build.artifacts_
	// retention_days；缺省 30，A10/S18——janitor 按 mtime 清理）。非正值
	// 回落默认（不允许误配成 0 静默关闭清理）。
	ArtifactsRetentionDays int `mapstructure:"artifacts_retention_days"`
	// ContextRoots 是构建上下文受管根的额外配置根（build.context_roots；
	// H14 宿主目录信任边界：context_dir 必须位于受管根内，越界构建终态
	// 失败）。缺省集合 = 系统 temp 根（build.Config.Normalize 恒并入）+
	// git 裸仓库根（GitSettings 装配并入）+ 上传会话根（BuildSettings
	// 装配并入）；单机同宿主形态下 CLI 构建目录在此显式扩根接入（信任由
	// TriggerBuild 的 admin scope 把门）。
	ContextRoots []string `mapstructure:"context_roots"`
	// MaxUploadMB 是上传构建的上下文 tar 字节上限（MiB；build.
	// max_upload_mb，IMPL-T2-2/DT-6）。≤0 回落 build.DefaultMaxUploadBytes
	//（256MiB）——超限在流式上传中 fail-closed 拒绝（E_BUILD_UPLOAD_TOO_
	// LARGE），不落盘不建行。
	MaxUploadMB int `mapstructure:"max_upload_mb"`
}

// GRPCConfig 是 gRPC 面的配置节（config 键 grpc.*）。
type GRPCConfig struct {
	// Addr 是 gRPC 监听地址（grpc.addr）。
	Addr string `mapstructure:"addr"`
}

// GitConfig 是 git push(SSH) 入口配置节（config 键 git.*，T2.19）。字段与
// internal/gitserver.Config 一一对应；安全默认基线：enabled 缺省 true、
// addr 缺省 127.0.0.1:8424（内部服务默认不暴露公网——VPS 上由安装/文档
// 指引改为对外）、root 缺省与 state 库同目录下 git/。
type GitConfig struct {
	// Enabled 报告是否启用 SSH git 面（git.enabled；缺省 true。false =
	// 显式关闭位——webhook 拉源不依赖 SSH 面，但 bare 仓库根共用）。
	Enabled *bool `mapstructure:"enabled"`
	// Addr 是 SSH 监听地址（git.addr；缺省 127.0.0.1:8424）。
	Addr string `mapstructure:"addr"`
	// PublicEndpoint 是 git remote 提示的对外 host:port（git.public_endpoint；
	// 可空）。解析链首位：显式对外地址（如 "git.example.com:8424"，省端口位
	// 则补监听端口）——服务端无法自行得知公网主机名，须显式告知或由
	// base_domain 推导（gitEndpointForHint）。空 = 按 base_domain 推导。
	PublicEndpoint string `mapstructure:"public_endpoint"`
	// Root 是 bare 仓库根目录（git.root；空 = <state 库同目录>/git）。
	Root string `mapstructure:"root"`
	// HostKeyFile 是 SSH host key 文件（git.host_key_file；空 =
	// <root>/host_ed25519。ed25519 首启生成持久化，绝不打印私钥）。
	HostKeyFile string `mapstructure:"host_key_file"`
}

// WebhookConfig 是 webhook 入口配置节（config 键 webhook.*，T2.19）。
type WebhookConfig struct {
	// ReplayTTLSecs 是 delivery ID 防重放窗口秒数
	//（webhook.replay_ttl_seconds；缺省 900 = 15 分钟）。
	ReplayTTLSecs int `mapstructure:"replay_ttl_seconds"`
}

// IngressConfig 是入口/证书配置节（config 键 ingress.*）。字段与
// internal/ingress.Config 一一对应；缺省值在 ingress.Config.Normalize
// （traefik:v3.5 钉版、host 80/443、配置端点 :8422、LE production ACME、
// 续期窗口 30 天）。
type IngressConfig struct {
	// TraefikImage 是入口镜像（traefik_image；钉版，升级 = 改配置 +
	// 回归，不追 latest）。
	TraefikImage string `mapstructure:"traefik_image"`
	// HTTPPort / HTTPSPort 是宿主发布端口（host 模式）。
	HTTPPort  int `mapstructure:"http_port"`
	HTTPSPort int `mapstructure:"https_port"`
	// ConfigAddr 是控制面配置端点监听地址（config_addr；默认 0.0.0.0:8422
	// ——Traefik 任务经宿主 IP 访问，鉴权 token 强制）。
	ConfigAddr string `mapstructure:"config_addr"`
	// ConfigTLSAddr 是配置端点 TLS 面监听地址（config_tls_addr；默认
	// 0.0.0.0:8423，E1 多节点设计 §2.4——base_domain 非空时 ingress 服务
	// 壳在该地址起 TLS 监听（E1-3 接线）；单节点不启用，缺省值零行为）。
	ConfigTLSAddr string `mapstructure:"config_tls_addr"`
	// ConfigAdvertiseIP 是下发给 Traefik 的控制面可达 IP
	//（config_advertise_ip；空 = 自动探测。Docker Desktop 形态 advertise
	// addr 是 VM 内部 IP，须显式配置宿主可达地址）。
	ConfigAdvertiseIP string `mapstructure:"config_advertise_ip"`
	// TokenFile 是配置端点 bearer token 文件（token_file；首启生成）。
	TokenFile string `mapstructure:"token_file"`
	// CertDir 是证书存储根目录（cert_dir；控制面侧明文 PEM，独立备份目录
	// ——state-model §2.1；bind 挂载进 Traefik 只读）。
	CertDir string `mapstructure:"cert_dir"`
	// ACME 是集中签发器配置。
	ACME IngressACMEConfig `mapstructure:"acme"`
	// RenewBeforeDays 是续期窗口天数（renew_before_days；缺省 30）。
	RenewBeforeDays int `mapstructure:"renew_before_days"`
	// RenewScanSeconds 是续期扫描周期秒数（renew_scan_seconds；缺省 12h）。
	RenewScanSeconds int `mapstructure:"renew_scan_seconds"`
}

// IngressACMEConfig 是集中 ACME 配置（config 键 ingress.acme.*）。
type IngressACMEConfig struct {
	// Enabled 报告是否启用集中签发（enabled；缺省 true。false = 只发布
	// HTTP 路由——无域名/离线环境的显式关闭位）。
	Enabled *bool `mapstructure:"enabled"`
	// CADirURL 是 ACME 目录端点（ca_dir_url；默认 LE production，测试用
	// Pebble URL）。
	CADirURL string `mapstructure:"ca_dir_url"`
	// Email 是 ACME 账号邮箱（email）。
	Email string `mapstructure:"email"`
	// CAPoolFile 是 CA 根证书池 PEM（ca_pool_file；Pebble/私有 CA 信任）。
	CAPoolFile string `mapstructure:"ca_pool_file"`
	// AccountKeyFile 是 ACME 账号私钥文件（account_key_file；空 =
	// <cert_dir>/acme-account.key）。
	AccountKeyFile string `mapstructure:"account_key_file"`
}

// RegistryConfig 是平台 registry（zot）配置节（config 键 registry.*，E1
// 多节点设计 §2.5：Swarm service 钉 manager + 本地卷 + fleetly-system
// overlay + Basic Auth）。E1-1 只落配置面（键位只增、缺省零值不改任何
// v0.1 行为）；服务名/卷名/overlay 名（fleetly-registry、fleetly-registry-
// data、fleetly-system）是平台常量，不走配置。
type RegistryConfig struct {
	// Image 是 zot 镜像引用（registry.image；钉版形态 name:tag@sha256:…）。
	// 空 = 未配置（E1-4 部署器票据落定钉版缺省并接入 R7 门禁与镜像台账；
	// 在此留空避免缺省引用先于 digest 台账存在）。
	Image string `mapstructure:"image"`
	// AuthFile 是 registry Basic Auth 凭据文件路径（registry.auth_file；
	// 空 = RegistryAuthFile() 回落 <数据根>/fleetly-registry.auth——与
	// ingress token 同形：平台生成随机 user/pass 落 0600 文件，不入
	// SQLite；轮换 = 重新生成 + 服务重建，设计 §2.5）。
	AuthFile string `mapstructure:"auth_file"`
}

// JoinConfig 是节点加入配置节（config 键 join.*，E1 多节点设计 §2.3）。
type JoinConfig struct {
	// TokenRotate 是 worker join-token 自动轮换开关（join.token_rotate；
	// auto|manual，缺省 auto——D-MN-1：锚定完成后自动 rotate 把泄露窗口
	// 收敛到分钟级；批量加节点场景配 manual，全部完成后手动
	// fleetly nodes rotate-token）。
	TokenRotate string `mapstructure:"token_rotate"`
}

// JoinTokenRotate 返回归一后的 join token 轮换策略（缺省/未知值一律
// auto——保守缺省：自动轮换是安全默认，manual 是显式 opt-out）。
func (c *AppConfig) JoinTokenRotate() string {
	if c.Join.TokenRotate == "manual" {
		return "manual"
	}
	return "auto"
}

// RegistryAuthFile 返回 registry 凭据文件路径，未配置时回落数据根下
// fleetly-registry.auth（数据根 = state 库同目录，与 BootstrapTokenPath
// 同款装配期回落）。
func (c *AppConfig) RegistryAuthFile() string {
	if c.Registry.AuthFile != "" {
		return c.Registry.AuthFile
	}
	return filepath.Join(filepath.Dir(c.DBPath()), "fleetly-registry.auth")
}

// RegistryHost 返回平台 registry 主机（registry.<base>；base_domain 派生，
// E1-5）。空 = 单节点本地模式（构建不推送、镜像本地 digest 引用——v0.1
// 逐字等价）。单点派生公式在此（与 IngressSettings 的平台子域派生同源）。
func (c *AppConfig) RegistryHost() string {
	if c.BaseDomain == "" {
		return ""
	}
	return "registry." + c.BaseDomain
}

// registryAuthFileForBuild 是构建管线的凭据文件下发面：仅 registry 模式
// 携带（本地模式 build.Config 零 registry 字段——v0.1 等价的装配面保证）。
func registryAuthFileForBuild(c *AppConfig) string {
	if c.BaseDomain == "" {
		return ""
	}
	return c.RegistryAuthFile()
}

// IngressSettings 把 ingress.* 配置节翻译为入口适配器核心配置（ingress.
// Config，缺省值经 Normalize 回落——单一事实源在 internal/ingress）。归一
// 必须发生在装配入口：ingress 服务的配置端点监听地址取自此处的 ConfigAddr
// ——不经 Normalize 的空串会让 net.Listen 落到随机端口（与 manager 内部
// 归一值漂移，T2.18 实机扫描发现并修正）。
func (c *AppConfig) IngressSettings() ingress.Config {
	return ingress.Config{
		TraefikImage:      c.Ingress.TraefikImage,
		HTTPPort:          c.Ingress.HTTPPort,
		HTTPSPort:         c.Ingress.HTTPSPort,
		ConfigAddr:        c.Ingress.ConfigAddr,
		ConfigTLSAddr:     c.Ingress.ConfigTLSAddr,
		ConfigAdvertiseIP: c.Ingress.ConfigAdvertiseIP,
		TokenFile:         c.Ingress.TokenFile,
		CertDir:           c.Ingress.CertDir,
		// BaseDomain 透传（E1-3）：非空启用 8423 TLS 配置面与平台证书
		// duty；空 = 单节点 v0.1 形态（ingress 侧零行为差异）。
		BaseDomain: c.BaseDomain,
		// zot 部署面（E1-4）：镜像钉版（registry.image 显式配置优先，空 =
		// ingress.Normalize 回落钉版缺省 DefaultZotImage）与凭据文件绝对
		// 路径（RegistryAuthFile 装配期回落数据根形态）。base_domain 空
		// 时部署器不活动，字段闲置无害。
		RegistryImage:    c.Registry.Image,
		RegistryAuthFile: c.RegistryAuthFile(),
		ACME: ingress.ACMEConfig{
			Enabled:        c.Ingress.ACME.Enabled,
			CADirURL:       c.Ingress.ACME.CADirURL,
			Email:          c.Ingress.ACME.Email,
			CAPoolFile:     c.Ingress.ACME.CAPoolFile,
			AccountKeyFile: c.Ingress.ACME.AccountKeyFile,
		},
		RenewBefore:       time.Duration(c.Ingress.RenewBeforeDays) * 24 * time.Hour,
		RenewScanInterval: time.Duration(c.Ingress.RenewScanSeconds) * time.Second,
		// console 免端口直访段（2026-09-24）：网关端口与 TLS 形态由控制面
		// 配置注入（ingress 侧只认这两个投影位，不重复端口语义）。
		ControlGatewayPort: httpPortOf(c.HTTPAddr()),
		ControlGatewayTLS:  c.TLSMode() != ControlPlaneTLSOff,
	}.Normalize()
}

// httpPortOf 提取 host:port 的端口位（畸形/缺失回落 8420——与
// DefaultHTTPAddr 端口一致；控制面启动对 addr 的合法性另有校验）。
func httpPortOf(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 8420
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 {
		return 8420
	}
	return p
}

// StateConfig 是状态层配置节（config 键 state.*）。保留期天数取非正值
// 时回落注册默认（事件 30 天 / 审计 90 天 / builds 终态行 90 天——保留期
// 是契约默认，不允许误配成 0 静默关闭清理）。审计留存另有 platform_settings
// 键 audit.retention_days 的运行期设置面（W3-S1 D-W0-6）：显式设置 > 本节
// config > 缺省 90，janitor 每拍现读（internal/state/janitor.go）。
type StateConfig struct {
	// DBPath 是 SQLite 状态库文件路径（state.db_path）。
	DBPath string `mapstructure:"db_path"`
	// EventRetentionDays 是事件保留天数（state.event_retention_days）。
	EventRetentionDays int `mapstructure:"event_retention_days"`
	// AuditRetentionDays 是审计保留天数（state.audit_retention_days；缺省
	// 90——D-W0-6，platform_settings audit.retention_days 显式设置时覆盖
	// 本值）。
	AuditRetentionDays int `mapstructure:"audit_retention_days"`
	// BuildRetentionDays 是 builds 终态行保留天数（state.build_retention_
	// days；缺省 90，A10/S18——janitor 清理终态构建台账行）。
	BuildRetentionDays int `mapstructure:"build_retention_days"`
	// DockerHost 是底座连接地址（state.docker_host）；空 = DOCKER_HOST
	// 环境变量，再缺省本机套接字。
	DockerHost string `mapstructure:"docker_host"`
}

// SecretsConfig 是平台密钥配置节（config 键 secrets.*）。主密钥文件与
// 备份数据分离保存（architecture §2.3）；权限过宽 fail-fast 拒绝启动
// （internal/secrets ErrWeakPermissions，POSIX 面）。
type SecretsConfig struct {
	// KeyPath 是 envelope 主密钥文件路径（secrets.key_path）；空 = 缺省
	// ./fleetly.key。首启不存在则生成并日志提示妥善保存。
	KeyPath string `mapstructure:"key_path"`
}

// GRPCAddr 返回 gRPC 监听地址，未配置时回落缺省值。
func (c *AppConfig) GRPCAddr() string {
	if c.GRPC.Addr == "" {
		return defaultGRPCAddr
	}
	return c.GRPC.Addr
}

// HTTPAddr 返回 HTTP 面（gateway）监听地址，未配置时回落缺省值。
func (c *AppConfig) HTTPAddr() string {
	if c.Addr == "" {
		return DefaultHTTPAddr
	}
	return c.Addr
}

// DBPath 返回状态库路径，未配置时回落缺省值。
func (c *AppConfig) DBPath() string {
	if c.State.DBPath == "" {
		return defaultDBPath
	}
	return c.State.DBPath
}

// KeyPath 返回主密钥文件路径，未配置时回落缺省值（单一事实源 =
// secrets.DefaultKeyPath）。
func (c *AppConfig) KeyPath() string {
	if c.Secrets.KeyPath == "" {
		return secrets.DefaultKeyPath
	}
	return c.Secrets.KeyPath
}

// GitSettings 把 git.*/webhook.* 配置节翻译为 git 触发入口核心配置
// （gitserver.Config，缺省值经 Normalize 回落——单一事实源在
// internal/gitserver）。Root 依赖 state 库路径，缺省在此计算（<db 同目录>/
// git）；HookEndpoint 由 HTTP addr 推导（host 位为通配/空时回落 127.0.0.1
// ——钩子回调走 loopback）。gitEndpoint() 是 SSH 面的 host:port 投影
// （apps 面的 git remote 提示原料）。
func (c *AppConfig) GitSettings() gitserver.Config {
	endpoint := hookEndpointFromAddr(c.Addr)
	return gitserver.Config{
		Enabled:      c.Git.Enabled == nil || *c.Git.Enabled,
		Addr:         c.Git.Addr,
		Root:         c.GitRoot(),
		HostKeyFile:  c.Git.HostKeyFile,
		HookEndpoint: endpoint,
		ReplayTTL:    time.Duration(c.Webhook.ReplayTTLSecs) * time.Second,
	}
}

// GitRoot 回落 bare 仓库根目录缺省值（与 state 库同目录下 git/）。
func (c *AppConfig) GitRoot() string {
	if c.Git.Root != "" {
		return c.Git.Root
	}
	return filepath.Join(filepath.Dir(c.DBPath()), "git")
}

// hookEndpointFromAddr 由 HTTP 监听地址推导钩子回调基址（host 位通配或
// 空回落 127.0.0.1；host 已是具体地址则原样使用）。
func hookEndpointFromAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "http://127.0.0.1:8420"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// BuildSettings 把 build.* 配置节翻译为构建管线核心配置（build.Config，
// 缺省值经 Normalize 回落——单一事实源在 internal/build）。
func (c *AppConfig) BuildSettings() build.Config {
	cfg := build.Config{
		BuildkitHost:        c.Build.BuildkitHost,
		DaemonContainerName: c.Build.DaemonContainerName,
		CacheVolume:         c.Build.CacheVolume,
		CacheDir:            c.Build.CacheDir,
		ArtifactsDir:        c.Build.ArtifactsDir,
		Concurrency:         c.Build.Concurrency,
		MemoryBytes:         c.Build.MemoryBytes,
		NanoCPUs:            int64(c.Build.CPUS * 1e9),
		PollInterval:        time.Duration(c.Build.PollSeconds) * time.Second,
		Timeout:             time.Duration(c.Build.TimeoutSeconds) * time.Second,
		ManageDaemon:        c.Build.ManageDaemon == nil || *c.Build.ManageDaemon,
		// registry 模式（E1-5）：base_domain 非空时产物推送 zot 并按
		// registry digest 引用记账；凭据文件经 RegistryAuthFile 回落（构建
		// 执行时点现读——凭据可能由 zot 部署 duty 晚于装配期生成）。空
		// base_domain = 本地模式（零额外字段，v0.1 管线逐字不变——凭据
		// 路径仅在 registry 模式下发）。
		RegistryHost:     c.RegistryHost(),
		RegistryAuthFile: registryAuthFileForBuild(c),
		// 受管根（H14）：显式配置根 + git 裸仓库根（v0.2 worktree 物化
		// 路径的前缀形态；当前 git 入口不直接产构建上下文，并入是前瞻
		// 接线）；build.Config.Normalize 再恒并入系统 temp 根。
		ContextRoots: append(append([]string{}, c.Build.ContextRoots...), c.GitRoot()),
		// 上传构建面（IMPL-T2-2/DT-6）：会话根 <数据根>/build-uploads
		// （由 Normalize 并入 ContextRoots）；上限经 build.max_upload_mb
		// （≤0 回落缺省）。
		UploadsRoot:    state.BuildUploadsRoot(c.DBPath()),
		MaxUploadBytes: int64(c.Build.MaxUploadMB) << 20,
	}
	return cfg.Normalize()
}
