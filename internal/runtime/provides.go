package runtime

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"

	"github.com/fleetlyrun/fleetly/internal/acmedns"
	"github.com/fleetlyrun/fleetly/internal/api"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/cron"
	"github.com/fleetlyrun/fleetly/internal/database"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/execrelay"
	"github.com/fleetlyrun/fleetly/internal/gitserver"
	"github.com/fleetlyrun/fleetly/internal/ingress"
	"github.com/fleetlyrun/fleetly/internal/logs"
	"github.com/fleetlyrun/fleetly/internal/metrics"
	"github.com/fleetlyrun/fleetly/internal/notify"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/rustfs"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/statebackup"
	"github.com/fleetlyrun/fleetly/internal/substrate"
	"github.com/fleetlyrun/fleetly/internal/victorialogs"
)

//go:generate go run -mod=mod github.com/google/wire/cmd/wire

// ProviderSet 是 fleetlyd 的 Wire 依赖集（装配形态对齐 lynx
// _examples/boot：Bootstrap 聚合钩子与服务，cleanup 由 main 挂 OnPostStop）。
var ProviderSet = wire.NewSet(
	boot.New,
	NewConfig,
	NewStore,
	NewSubstrateClient,
	NewDockerClient,
	NewNodeIdentity,
	NewObserver,
	NewJanitor,
	NewSecretsBox,
	NewBackupManager,
	NewRustfsManager,
	NewVictorialogsBackend,
	NewVictorialogsManager,
	NewMetricsBackend,
	NewMetricsManager,
	NewDatabaseManager,
	NewBuilder,
	NewBuildQueue,
	NewPlacementResolver,
	NewIngressManager,
	NewEngine,
	NewDaemonManager,
	NewLogsManager,
	NewCronManager,
	NewNotifyManager,
	NewExecRelayTaskSource,
	NewExecRelayHub,
	NewExecRelayManager,
	NewExecService,
	NewTerminalNativeHandler,
	NewAlertsReceiverHandler,
	NewAuthenticator,
	NewSystemService,
	NewAppsService,
	NewCronService,
	NewDatabaseService,
	NewSecretsService,
	NewConfigsService,
	NewDeploymentsService,
	NewRevisionsService,
	NewBuildsService,
	NewDriftService,
	NewRuntimeService,
	NewDomainsService,
	NewEnvService,
	NewLogsService,
	NewMetricsService,
	NewAlertingService,
	NewNotificationsService,
	NewEventsService,
	NewPlacementService,
	NewTokensService,
	NewAuthService,
	NewUsersService,
	NewAuditService,
	NewTeamsService,
	NewProjectsService,
	NewTasksService,
	NewGitTriggers,
	NewGitKeysService,
	NewGRPCServer,
	NewControlPlaneTLS,
	NewHTTPServer,
	NewServices,
	NewServiceFactories,
	NewPreStarts,
	NewDrains,
	NewPreStops,
	NewPostStops,
)

// NewConfig 从应用配置（flags + 配置文件）解出 AppConfig，缺省值在此回落。
func NewConfig(app lynx.App) (*AppConfig, error) {
	c := new(AppConfig)
	if err := app.Config().Unmarshal(c); err != nil {
		return nil, err
	}
	if c.Addr == "" {
		c.Addr = DefaultHTTPAddr
	}
	if c.GRPC.Addr == "" {
		c.GRPC.Addr = defaultGRPCAddr
	}
	return c, nil
}

// NewStore 打开状态库（启动即建库迁移，失败 fail-fast 拒绝启动）；资源
// 释放交给 Wire cleanup（main 挂 OnPostStop，晚于全部服务 Stop）。
func NewStore(cfg *AppConfig) (*state.Store, func(), error) {
	st, err := state.Open(context.Background(), cfg.DBPath())
	if err != nil {
		return nil, nil, err
	}
	return st, func() { _ = st.Close() }, nil
}

// NewSubstrateClient 构造底座适配器客户端（moby/client）。它是多个端口
// 的实现载体：state.DockerClient（节点观测/身份）与 build 包端口（镜像
// inspect/load、buildkitd 容器编排、registry manifest HEAD 前哨）；
// 连接资源由 Wire cleanup 释放。base_domain 非空时装配平台 registry 适配
// （E1-4/E1-5）：registry 模式镜像引用经 manifest HEAD 前哨核验、service
// 写自动附带 --with-registry-auth 凭据（惰性现读 registry.auth_file——
// 凭据可能由 zot 部署 duty 晚于装配期生成）；单节点不装配（零行为差异）。
// IMPL-T1-2/DT-2：装配外部 registry 凭证读取缝（state registry.* 设置
// 现读 + Box 解密；保存即对下一次部署生效——tag 解析与 service 写两处
// 消费）与回落留痕（registry 腿失败回落本机 inspect 的日志）。
func NewSubstrateClient(app lynx.App, cfg *AppConfig, st *state.Store, sb *secrets.Box) (*substrate.Client, func(), error) {
	c, err := substrate.NewClient(cfg.State.DockerHost)
	if err != nil {
		return nil, nil, err
	}
	if host := cfg.RegistryHost(); host != "" {
		authFile := cfg.RegistryAuthFile()
		c.WithPlatformRegistry(host, func() (build.RegistryCredentials, error) {
			return build.LoadRegistryCredentials(authFile)
		})
	}
	c.WithImageRegistryCredentials(func() (substrate.ExternalRegistrySettings, error) {
		in, err := st.LoadRegistrySettings(context.Background())
		if err != nil {
			return substrate.ExternalRegistrySettings{}, err
		}
		out := substrate.ExternalRegistrySettings{Host: in.Host, Username: in.Username}
		if in.PasswordCipher != "" {
			if sb == nil {
				return substrate.ExternalRegistrySettings{}, fmt.Errorf("secrets box not assembled (registry credentials cannot be decrypted)")
			}
			plain, err := sb.Decrypt([]byte(in.PasswordCipher))
			if err != nil {
				return substrate.ExternalRegistrySettings{}, fmt.Errorf("decrypt registry password: %w", err)
			}
			out.Password = string(plain)
		}
		return out, nil
	})
	c.WithImageRegistryTrace(app.Logger().Warn)
	return c, func() { _ = c.Close() }, nil
}

// NewDockerClient 以 substrate 客户端实现 state.DockerClient 端口（连接
// 生命周期归 NewSubstrateClient 的 cleanup，此处不重复释放）。
func NewDockerClient(sc *substrate.Client) (state.DockerClient, func(), error) {
	return sc, func() {}, nil
}

// NewDaemonManager 以 substrate 客户端实现 build.DaemonManager 端口
// （buildkitd 容器编排：镜像拉取/卷/容器幂等收敛）。
func NewDaemonManager(sc *substrate.Client) build.DaemonManager {
	return sc
}

// NewNodeIdentity 构造节点身份管理器（平台节点 ID n_<ULID> 生成持久化
// 与 Swarm label 锚定，state-model §2.3）。
func NewNodeIdentity(app lynx.App, st *state.Store, dc state.DockerClient) *state.NodeIdentity {
	return state.NewNodeIdentity(st, dc, app.Logger())
}

// NewObserver 构造节点观测缓存刷新器（30s 全量 resync + 事件驱动失效，
// 底座不可达置 stale + 指数退避），并挂观测拍后处理 = 集群锚定 duty
// （ClusterAnchor：worker 身份收编 + node.* 差分事件，multi-node §2.7
// E1-6）+ auto-rotate 触发链（D-MN-1：mode 取 join.token_rotate 归一值
// ——auto 缺省，manual 显式 opt-out；轮换端口 = substrate 客户端，state
// 层不依赖 substrate 的方向纪律由端口注入维持）。观测同步本体语义逐字
// 不变；挂钩自吞错误、不推翻同步成功。
func NewObserver(app lynx.App, cfg *AppConfig, st *state.Store, dc state.DockerClient, sc *substrate.Client) *state.Observer {
	ob := state.NewObserver(st, dc, app.Logger())
	anchor := state.NewClusterAnchor(st, dc, app.Logger()).
		WithTokenRotate(cfg.JoinTokenRotate(), sc)
	return ob.WithPostSync(anchor.PostSync)
}

// NewJanitor 构造保留期清理守护（事件/审计过期清理，周期可配）。
// A7/A10（S18）扩展：部署 compose 目录 30 天窗、builds 终态行 90 天、
// artifacts 目录 30 天保留窗与非终态超龄扫描（预算从 engine/build 配置
// 派生：2×（init job 预算+发布看门狗+观察窗）/ 2×构建超时——DT-4 起含
// init 相位，含 init job 的部署合法停留可达 InitJobTimeout+DeployTimeout，
// 不含即假告警）。
func NewJanitor(app lynx.App, st *state.Store, cfg *AppConfig) *state.Janitor {
	engineCfg := cfg.EngineSettings().Normalize()
	buildCfg := cfg.BuildSettings()
	return state.NewJanitor(st, state.JanitorConfig{
		EventRetentionDays:     cfg.State.EventRetentionDays,
		AuditRetentionDays:     cfg.State.AuditRetentionDays,
		BuildRetentionDays:     cfg.State.BuildRetentionDays,
		ArtifactsDir:           buildCfg.ArtifactsDir,
		ArtifactsRetentionDays: cfg.Build.ArtifactsRetentionDays,
		DeploymentsRoot:        cfg.DeploymentsRoot(),
		// 部署目录 30 天窗取注册默认（DeploymentDirRetentionDays 零值回落）。
		StaleDeploymentBudget: 2 * (engineCfg.InitJobTimeout + engineCfg.DeployTimeout + engineCfg.ObserveWindow),
		StaleBuildBudget:      2 * buildCfg.Timeout,
		// IMPL-T2-2：上传会话目录孤儿兜底清扫（保留窗取注册默认 1 天——
		// 终态清理由构建管线钩子承担）。
		UploadsRoot: buildCfg.UploadsRoot,
	}, app.Logger())
}

// NewSecretsBox 初始化 envelope 主密钥（fail-fast：加载/权限/生成任一失败
// 拒绝启动——密钥是全部平台 env 密文的生命线，architecture §2.3）。首启
// 生成时日志明示妥善保存（与备份分离；丢失 = env 密文不可解）。装配期
// 完成即 fleetlyd 主密钥初始化语义；健康面由 secrets 服务壳 CheckHealth
// 持续上报。
func NewSecretsBox(app lynx.App, cfg *AppConfig) (*secrets.Box, error) {
	box, created, err := secrets.EnsureKey(cfg.KeyPath())
	if err != nil {
		return nil, err
	}
	if created {
		app.Logger().Warn("master key generated at " + box.Path() +
			" — store it safely and keep it separate from backups (platform env ciphertext cannot be decrypted if it is lost)")
	}
	return box, nil
}

// NewRustfsManager 构建托管 RustFS duty 管理器（E3-5，D-S3-7：s3.mode=
// rustfs 时幂等部署/收敛 fleetly-rustfs，mode 离开时移除服务保留卷；
// 常驻收敛循环由服务壳 Start 承载，资源层——晚于 engine 停）。自建
// Docker 连接（zot 部署器同款形态），cleanup 释放；探针/建桶容器执行器
// = substrate 客户端（statebackup.ResticRunner 端口，与上传轨同源）。
func NewRustfsManager(app lynx.App, st *state.Store, sb *secrets.Box, sc *substrate.Client) (*rustfs.Manager, func(), error) {
	mgr, cleanup, err := rustfs.NewManager(st, sb, app.Logger())
	if err != nil {
		return nil, nil, err
	}
	return mgr.WithProbeRunner(sc), cleanup, nil
}

// NewVictorialogsBackend 构造 VL 回环消费端（E6 W5-S1：hub 入湖传输面与
// SearchLogs 查询后端共用——指向 127.0.0.1:9428，D-W5-4 host-mode 回环
// 发布的宿主可达面；无状态，无资源释放）。
func NewVictorialogsBackend() *victorialogs.Backend {
	return victorialogs.NewBackend()
}

// NewVictorialogsManager 构建托管 VictoriaLogs duty 管理器（E6 W5-S1，
// 设计 §2.1：logs.backend=victorialogs〔缺省〕时幂等部署/收敛
// fleetly-victorialogs——单副本钉 manager、卷/内部网络/host-mode 回环发布
// 9428、-retentionPeriod 对齐 logs.retention_days；切回 jsonl 移除服务
// 保留卷。常驻收敛循环由服务壳 Start 承载，资源层——晚于 engine 停）。
// 自建 Docker 连接（rustfs Manager 同款形态），cleanup 释放；健康拨测 =
// Backend.Ping（宿主回环直达——无探针容器，与 rustfs 的差异点）。
func NewVictorialogsManager(app lynx.App, cfg *AppConfig, st *state.Store, vl *victorialogs.Backend) (*victorialogs.Manager, func(), error) {
	mgr, cleanup, err := victorialogs.NewManager(st, cfg.LogsSettings().RetentionDays, app.Logger())
	if err != nil {
		return nil, nil, err
	}
	if vl != nil {
		mgr = mgr.WithHealth(vl.Ping)
	}
	return mgr, cleanup, nil
}

// NewMetricsBackend 构造 VM 回环查询消费端（E6 W5-S3：SearchMetrics 的
// 查询后端与 duty 健康拨测共用——指向 127.0.0.1:8428，D-W5-4 等价承载
// 形态的宿主可达面；无状态，无资源释放）。
func NewMetricsBackend() *metrics.Backend {
	return metrics.NewBackend()
}

// NewMetricsManager 构建托管 metrics 栈 duty 管理器（E6 W5-S3，设计 §4.1，
// D-W5-2 opt-in：metrics.mode=on 时幂等部署/收敛三件——VM 单副本钉
// manager/卷/host 网络回环监听 8428/-retentionPeriod 对齐 metrics.
// retention_days/-promscrape.config 经 swarm config 对象分发；cAdvisor 与
// node_exporter global。切回 unset 三件移除保留卷。W5-S2 起 alerts.mode=on
// 时第四件 vmalert 同收敛——notifier URL 指向本进程 gateway 的
// /internal/alerts（回环形态：vmalert 钉 manager 且 host 网络，与 gateway
// 同 netns，恒可达；tokenFile = ingress token 文件，bind 只读进任务——
// 凭据材料不进服务 spec）。常驻收敛循环由服务壳 Start 承载，资源层——晚于
// engine 停）。自建 Docker 连接（victorialogs Manager 同款形态），cleanup
// 释放；健康拨测 = Backend.Ping（宿主回环直达——无探针容器）。
func NewMetricsManager(app lynx.App, cfg *AppConfig, st *state.Store, mb *metrics.Backend) (*metrics.Manager, func(), error) {
	mgr, cleanup, err := metrics.NewManager(st, cfg.MetricsSettings().RetentionDays, app.Logger())
	if err != nil {
		return nil, nil, err
	}
	if mb != nil {
		mgr = mgr.WithHealth(mb.Ping)
	}
	// notifier scheme 跟随网关 TLS 形态（staging 真机 2026-09-25 实爆：TLS 常开
	// 下明文拨 8420 = connection reset——与 console 直访段 0d84e6a 同族教训）；
	// 回环 IP 无 SAN，TLS 形态下接收器 spec 侧补 -notifier.tls.insecureSkipVerify。
	scheme := "http"
	if cfg.TLSMode() != ControlPlaneTLSOff {
		scheme = "https"
	}
	notifierURL := fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, httpPortOf(cfg.HTTPAddr()), internalAlertsPath)
	mgr = mgr.WithAlertsNotifier(notifierURL, cfg.IngressSettings().TokenFile)
	return mgr, cleanup, nil
}

// NewDatabaseManager 构建库实例收敛 duty 管理器（E4 W4-S2，managed-
// databases §2.1/§2.3 的 provisioner：按生命周期态分派收敛——provisioning
// 建现场过健康门、ready/degraded 健康观察、paused 保持 scale-0、deleting
// 幂等 reap；状态写全部经 state.EnterDbPhase 单写点）。自建 Docker 连接
// （rustfs Manager 同款形态——引擎凭据 secret 的 SecretReference 翻译需要
// 底座对象 ID 与完整 File UID/GID/Mode，通用投影装不下），cleanup 释放；
// 放置裁决消费 placement.Resolver（接口在 internal/database 定义，方向
// 纪律同 cron 的 NodePreflight）。
func NewDatabaseManager(app lynx.App, st *state.Store, sb *secrets.Box, pl *placement.Resolver) (*database.Manager, func(), error) {
	return database.NewManager(database.Config{}, st, sb, pl, app.Logger())
}

// NewBuilder 构建构建执行器（build.Builder：railpack/dockerfile 双驱动 +
// buildkit solve + 产物归档 + 执行前 buildkitd 就绪收敛）。镜像端口与容器
// 编排由 substrate.Client 隐式实现 build 包端口（适配器方向：
// substrate → build 核心接口）。W5-S1：注入构建日志行分流目标 =
// logs.Manager（build.log 行级 tee——文件写入逐字不变，分流喂入湖批量器
// source=build；lm 未装配时 WithBuildLogSink(nil) 为零差异透传）。
func NewBuilder(cfg *AppConfig, st *state.Store, dc *substrate.Client, dm build.DaemonManager, app lynx.App, lm *logs.Manager) *build.Builder {
	return build.NewBuilder(cfg.BuildSettings(), st, dc, dm, app.Logger()).
		WithBuildLogSink(lm)
}

// NewBackupManager 构建状态备份管理器（T2.22：热备快照 + 回读校验 +
// manifest/台账 + 每日循环；backup.* 配置节，dir 缺省回落数据根下 backups/
// ——主密钥分离性由构造期 fail-fast 守卫）。E3-3：上传轨接线——restic
// 钉版容器一次性执行端口由 substrate 客户端实现（Docker API create/
// start/wait/rm），envelope 加解密器承担 restic repo 口令的解密与惰性
// 生成落库（D-S3-5）；上传轨开关 = s3.mode（unset 即停，运行期设置现读）。
func NewBackupManager(app lynx.App, cfg *AppConfig, st *state.Store, sb *secrets.Box, sc *substrate.Client, version Version) (*statebackup.Manager, error) {
	mgr, err := statebackup.NewManager(cfg.BackupSettings(), cfg.BackupRoot(), sb.Path(),
		string(version), st, app.Logger())
	if err != nil {
		return nil, err
	}
	return mgr.WithUpload(sc, sb), nil
}

// NewBuildQueue 构建构建队列调度器（信号量并发上限 + builds 行扫描认领 +
// per-build 超时预算 + 启动复位中断构建）。IMPL-T2-2：上传会话根随装配
// 注入（收敛/复位路径的 ephemeral 上下文清理锚点）。
func NewBuildQueue(app lynx.App, cfg *AppConfig, st *state.Store, b *build.Builder) *build.Queue {
	settings := cfg.BuildSettings()
	return build.NewQueue(st, b, settings.Concurrency, settings.PollInterval, settings.Timeout, app.Logger()).
		WithUploadsRoot(settings.UploadsRoot)
}

// NewPlacementResolver 构造放置解析器（放置意图解析/绑定落库/卷登记/
// 部署前哨；引擎 preparing 与 releasing 全程消费）。
func NewPlacementResolver(st *state.Store, dc state.DockerClient) *placement.Resolver {
	return placement.NewResolver(st, dc)
}

// NewIngressManager 构建入口/证书管理器（T2.15/T2.16：Traefik 部署 +
// 配置端点 + 集中 ACME；ingress.* 配置节，缺省回落 internal/ingress）。
// docker 连接经 manager cleanup 释放（Wire cleanup，OnPostStop 时点）。
// W5-S3：DNS-01 插件解析缝随 envelope 解密器接线（dns01.go）——读 acme.*
// 设置 → 解密凭证 → acmedns.New（零第三方 SDK 插件）；每签发现读（凭证
// 轮换即生效）；provider 未配置/解密失败显式报错（wildcard 签发诚实失败，
// duty 退避重试，不回落 HTTP-01）。
func NewIngressManager(app lynx.App, cfg *AppConfig, st *state.Store, sb *secrets.Box) (*ingress.Manager, func(), error) {
	mgr, cleanup, err := ingress.NewManager(cfg.IngressSettings(), st, app.Logger())
	if err != nil {
		return nil, nil, err
	}
	mgr.WithDNSProviderResolver(func(ctx context.Context) (acmedns.Provider, error) {
		if sb == nil {
			return nil, fmt.Errorf("secrets box not assembled (acme dns credentials cannot be decrypted)")
		}
		in, err := st.LoadAcmeSettings(ctx)
		if err != nil {
			return nil, fmt.Errorf("load acme dns settings: %w", err)
		}
		if in.DNSProvider == state.AcmeDNSProviderNone || !in.CredentialsSet() {
			return nil, fmt.Errorf("dns provider not configured (acme.dns.provider=%s)", in.DNSProvider)
		}
		plain, err := sb.Decrypt([]byte(in.CredentialsCipher))
		if err != nil {
			return nil, fmt.Errorf("decrypt acme dns credentials: %w", err)
		}
		return acmedns.New(in.DNSProvider, plain)
	})
	return mgr, cleanup, nil
}

// ingressPublisher 是 engine.RoutePublisher 的载荷转换适配器：引擎侧
// RoutePublishInput（核心类型）→ ingress.PublishInput（适配器类型）。
// 方向纪律：核心不感知适配器类型，转换只在此处。
type ingressPublisher struct {
	m *ingress.Manager
}

func (p ingressPublisher) PublishRoutes(ctx context.Context, in engine.RoutePublishInput) error {
	out := ingress.PublishInput{AppID: in.AppID, AppName: in.AppName, TeamSlug: in.TeamSlug, PrjSlug: in.PrjSlug}
	for _, svc := range in.Declared {
		out.Declared = append(out.Declared, ingress.ServiceRoutes{
			Service: svc.Service, Port: svc.Port, Domains: svc.Domains,
		})
	}
	return p.m.PublishRoutes(ctx, out)
}

// NewEngine 构建发布引擎（T2-5a：状态机/对账/窗口语义；治理参数取 engine.*
// 配置节，缺省回落文档默认）。底座服务/任务面由 substrate.Client 隐式实现
// engine.Substrate + engine.ImageChecker（适配器方向：substrate → engine
// 核心接口）；路由发布端口由 ingress.Manager 经载荷适配实现（T2.15——
// 健康门后挂点）。mb 是 VM 回环查询消费端（W5-S1 自动扩缩评估器的数据面
// ——经 WithMetricsQuerier 注入 engine.MetricsQuerier 端口；nil = 扩缩
// duty 空转，装配形态诚实空转不冒充运行态）。
func NewEngine(app lynx.App, cfg *AppConfig, st *state.Store, sc *substrate.Client, pl *placement.Resolver, box *secrets.Box, m *ingress.Manager, bm *statebackup.Manager, lm *logs.Manager, mb *metrics.Backend) *engine.Engine {
	settings := cfg.EngineSettings()
	// 控制面地址注入（ctrlinject.go：集群内工作负载回拨控制面的零配置
	// 通路）——值三要素：advertise（swarm NodeAddr，VPC 内网）、gRPC 端口
	// （grpc.addr 配置段）、TLS 校验名（platform TLS 模式的 ctrl.<base>，
	// 与 exec relay 的 FLEETLY_CONTROL_TLS_NAME 同源）。advertise 探测失败
	// 容忍为空（注入面不接线——显式 endpoint 配置的消费者不受影响），warn
	// 落日志。
	controlGRPCAddr := ""
	if advertise, err := sc.AdvertiseAddr(context.Background()); err != nil {
		app.Logger().Warn("engine: control-plane address injection disabled (swarm advertise unavailable)", "error", err.Error())
	} else if advertise != "" {
		controlGRPCAddr = advertise + ":" + portOfAddr(cfg.GRPCAddr(), "127.0.0.1:8421")
	}
	controlTLSName := ""
	if cfg.TLSMode() == ControlPlaneTLSPlatform {
		if name, err := m.PlatformTLSName(); err == nil {
			controlTLSName = name
		}
	}
	settings.ControlGRPCAddr = controlGRPCAddr
	settings.ControlTLSName = controlTLSName
	return engine.NewEngine(settings, st, sc, sc, pl, box, app.Logger()).
		WithRoutePublisher(ingressPublisher{m: m}).
		// 备份挂钩（T2.22）：每次部署成功后异步触发一次热备快照
		// （kind=post_deploy；失败只落台账/审计/组件三面红，不影响部署）。
		WithPostDeployHook(bm.RunPostDeploy).
		// H9：部署 env 提升点联动失效日志脱敏值集（观察窗成功 + 实际
		// 提升 pending 时触发；回调只做缓存删除，非阻塞）。
		WithEnvChangedHook(lm.InvalidateRedaction).
		// E4 managed-databases：库模板连接信息端口（fleetly.databases 引用
		// 面的物化键值与前缀唯一定义点在 dbtemplate——渲染投影消费
		// engine.ServiceSpec，dbtemplate 在 engine 之上，只能装配层注入）。
		WithDatabaseTemplate(dbTemplatePort{}).
		// E4 W4-S4：Swarm secret 确保端口（compose secrets 注入链的底座
		// 原语——ensure 幂等由 substrate.Client.EnsureSecret 承载）。
		WithSecretEnsurer(sc).
		// E4 W4-S6：Swarm secret 清场端口（app 删除 reap 的扫尾面——按归属
		// label 扫描移除，best-effort 不阻塞删除收敛）。
		WithSecretReaper(sc).
		// T 线 OT-3/IMPL-T1-4：Swarm config 确保端口（compose configs 注入
		// 链的底座原语——内容寻址对象，ensure 幂等由 substrate.Client.
		// EnsureConfig 承载）。
		WithConfigEnsurer(sc).
		// T 线 OT-3/IMPL-T1-4：Swarm config 清场端口（内容换版的旧对象 GC
		// 与 app 删除 reap 的扫尾面，best-effort）。
		WithConfigReaper(sc).
		// W5-S1 自动扩缩（D-V3W5-2）：VM 瞬时查询端口——metrics.Backend 隐式
		// 实现 engine.MetricsQuerier（评估器的 CPU/内存采样面）。
		WithMetricsQuerier(mb).
		// IMPL-T15-1（OT-1）：底座网络对象面端口（项目网 ensure/对账/GC）
		// + recon networks 面的平台组件网白名单（组件固定名常量在此注入
		// ——engine 不 import ingress，方向纪律；组件网生命周期归各组件
		// duty，网络对账不判罚）。
		WithNetworkSubstrate(sc).
		WithPlatformNetworks(state.RustfsNetworkName, ingress.RegistryNetworkName)
}

// dbTemplatePort 是引擎对库模板连接信息面的装配层适配（engine.DatabaseTemplatePort；
// 薄委托到 dbtemplate.EnvPrefix / dbtemplate.ConnectionVars——前缀与连接串
// 键值的唯一定义点保持单源）。
type dbTemplatePort struct{}

func (dbTemplatePort) EnvPrefix(instance string) string { return dbtemplate.EnvPrefix(instance) }

func (dbTemplatePort) ConnectionVars(templateID, instance, password string) (map[string]string, error) {
	return dbtemplate.ConnectionVars(templateID, instance, password)
}

// NewLogsManager 构建日志管线管理器（T2.20：采集/Follow/History/清理；
// logs.* 配置节，缺省回落 internal/logs）。底座端口由 substrate.Client
// 隐式实现 logs.Port（适配器方向：substrate → logs 核心接口）。B3：注入
// git 触发面为补充脱敏值集供给（钩子 token 明文只在钩子文件）。W5-S1：
// 注入入湖传输面 = VL 回环消费端（E6 设计 §2.3——批量器在 logs.Manager
// 内部，flush/溢出/streak 面承载于此；vl.backend 门每拍设置现读）。
func NewLogsManager(app lynx.App, cfg *AppConfig, st *state.Store, sc *substrate.Client, sb *secrets.Box, src *gitserver.GitTriggers, vl *victorialogs.Backend) *logs.Manager {
	return logs.NewManager(cfg.LogsSettings(), st, sc, sb, app.Logger()).
		WithSecretSource(src).
		WithIngestBackend(vl)
}

// NewNotifyManager 构建通知投递器（E6 W5-S4，observability §5.2：webhook_state
// 游标经 EventsSince 消费 → 订阅匹配 → 签名 POST + 退避重试。进程内消费者
// ——与 WatchEvents 平行，不经 token/流机制；常驻循环由服务壳 Start 承载，
// 资源层停机时 drain 在途尝试）。box 供台账投递时的密钥解密；零外部资源
// （HTTP 客户端无连接池清理面），无 cleanup。
func NewNotifyManager(app lynx.App, st *state.Store, sb *secrets.Box) *notify.Manager {
	return notify.NewManager(st, sb, notify.Config{}, app.Logger())
}

// NewCronManager 构建定时任务调度器（E5 Cron，架构 §4.3 细则：tick 循环
// 扫描拍 10s、看门狗默认 10m——平台常量，无配置面；调度集每拍现读 state）。
// 底座服务/任务面由 substrate.Client 隐式实现 engine.Substrate（与引擎同源
// 适配器）；节点前哨复用放置解析器（placement.Preflight——绑定节点不 ready
// 即 skip(node_unavailable)）。
func NewCronManager(app lynx.App, st *state.Store, sb *secrets.Box, sc *substrate.Client, pl *placement.Resolver) *cron.Manager {
	return cron.NewManager(cron.Config{}, st, sb, sc, pl, app.Logger())
}

// execRelayTaskSource 是 execrelay.TaskSource 的装配层适配（substrate.
// Client 的 TaskRuntimes/NodeHostnames 投影 → execrelay.TaskRuntime——
// 底座类型不出 substrate、核心类型不出 execrelay，转换只在此处）。
type execRelayTaskSource struct {
	sc *substrate.Client
}

// ListTaskRuntimes 实现 execrelay.TaskSource。
func (s execRelayTaskSource) ListTaskRuntimes(ctx context.Context, service string) ([]execrelay.TaskRuntime, error) {
	tasks, err := s.sc.TaskRuntimes(ctx, service)
	if err != nil {
		return nil, err
	}
	out := make([]execrelay.TaskRuntime, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, execrelay.TaskRuntime{
			ID: t.ID, NodeID: t.NodeID, ContainerID: t.ContainerID, Slot: t.Slot,
			State: t.State, DesiredState: t.DesiredState, Timestamp: t.Timestamp,
		})
	}
	return out, nil
}

// NodeHostnames 实现 execrelay.TaskSource（成员发现的节点反查面）。
func (s execRelayTaskSource) NodeHostnames(ctx context.Context) (map[string]string, error) {
	return s.sc.NodeHostnames(ctx)
}

// NewExecRelayTaskSource 构造 hub 的任务投影端口（substrate 客户端适配）。
func NewExecRelayTaskSource(sc *substrate.Client) execrelay.TaskSource {
	return execRelayTaskSource{sc: sc}
}

// NewExecRelayHub 构造终端 hub（E7 W5-S6：relay 连接表 + 会话桥接 + 限额
// + terminal.opened/closed 审计/事件；功能开关与控制面 HTTP 端口经
// AppConfig 静态注入——nil Enabled 供给 = 恒开，测试形态）。
func NewExecRelayHub(app lynx.App, cfg *AppConfig, st *state.Store, tasks execrelay.TaskSource) *execrelay.Hub {
	enabled := cfg.TerminalEnabled()
	return execrelay.NewHub(execrelay.HubConfig{
		Store:   st,
		Tasks:   tasks,
		Tickets: execrelay.NewTicketStore(0),
		Log:     app.Logger(),
		Enabled: func() bool { return enabled },
	})
}

// NewExecRelayManager 构建托管 relay duty 管理器（E7 W5-S6，web-terminal
// §2.1：terminal.enabled=true 时幂等收敛 global 服务 fleetly-exec；false 时
// 移除。集群 token secret 首拍生成、哈希落 meta。wss 校验名 = 平台 TLS 模式
// 为 platform 时下发的 ctrl.<base>——off/manual 诚实降级为 ws:// 明文，设计
// §3.3。自建 Docker 连接，cleanup 释放；常驻收敛循环由服务壳承载——资源层，
// 晚于 engine 停）。
func NewExecRelayManager(app lynx.App, cfg *AppConfig, st *state.Store, ing *ingress.Manager) (*execrelay.Manager, func(), error) {
	tlsName := ""
	if cfg.TLSMode() == ControlPlaneTLSPlatform {
		name, err := ing.PlatformTLSName()
		if err != nil {
			return nil, nil, fmt.Errorf("execrelay: resolve control TLS name: %w", err)
		}
		tlsName = name
	}
	httpPort := portOfAddr(cfg.Addr, DefaultHTTPAddr)
	mgr, cleanup, err := execrelay.NewManager(st, cfg.TerminalEnabled(), httpPort, tlsName, app.Logger())
	if err != nil {
		return nil, nil, err
	}
	return mgr, cleanup, nil
}

// portOfAddr 取监听地址的端口段（空/无端口回落 defaultAddr 的端口——
// FLEETLY_CONTROL_ADDR 的端口段与 HTTP 面同源）。
func portOfAddr(addr, defaultAddr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		_, port, _ = net.SplitHostPort(defaultAddr)
	}
	return port
}

// NewAppsService 构造应用资源面服务（T2.17；T2.19 增补 webhook/git 触发
// 配置面——box 加密 webhook secret 与拉源认证材料，gitEndpoint 拼 remote
// 提示；H9 增补路由撤销端口——app 删除管线经 ingress.Manager 撤销路由）。
func NewAppsService(st *state.Store, sb *secrets.Box, cfg *AppConfig, m *ingress.Manager) *api.AppsService {
	return api.NewAppsService(st, sb, gitEndpointForHint(cfg.Git.Addr, cfg.Git.PublicEndpoint, cfg.BaseDomain), m)
}

// NewCronService 构造定时任务面服务（E5 Cron：手动触发走调度器同链路 +
// cron.manual_triggered 审计；runs 读面直读台账）。
func NewCronService(st *state.Store, cm *cron.Manager) *api.CronService {
	return api.NewCronService(st, cm)
}

// NewDatabaseService 构造库实例资源面服务（E4 W4-S2：受理/守卫/脱敏投影
// ——box 承载凭据生成与指纹；kicker = database.Manager——受理后即时触发
// 收敛拍，收敛本体由 duty 异步承载。W4-S4 增补 rotator = 同一 Manager——
// 轮换编排（引擎侧 job/收敛触发/引用重部署）在底座邻接层，api 只受理与
// 审计。W4-S5 增补 ops = 同一 Manager——备份/恢复/升级编排（一次性 job/
// 备份门/健康门/归位）同款形态）。
func NewDatabaseService(st *state.Store, sb *secrets.Box, dm *database.Manager) *api.DatabaseService {
	return api.NewDatabaseService(st, sb, dm, dm, dm)
}

// NewSecretsService 构造平台密钥库资源面服务（E4 W4-S4，D-DB-7：external
// secret 的唯一写入口——box 加密落库，无值读回面）。
func NewSecretsService(st *state.Store, sb *secrets.Box) *api.SecretsService {
	return api.NewSecretsService(st, sb)
}

// NewConfigsService 构造明文配置资源面服务（T 线 OT-3/IMPL-T1-4：compose
// external configs 的唯一写入口——明文落 app_configs，GetConfig 走 admin
// 回读面）。
func NewConfigsService(st *state.Store) *api.ConfigsService {
	return api.NewConfigsService(st)
}

// NewTasksService 构造任务面（T 线 DT-5 / IMPL-T2-1）：执行面编排端口 =
// engine（ResolveTaskScope / ResolveTaskImage / EnsureTaskNetwork 隐式实现
// api.TasksOrchestrator；方向纪律同 appNetworkPort——装配层接线）。
func NewTasksService(st *state.Store, sb *secrets.Box, eng *engine.Engine) *api.TasksService {
	return api.NewTasksService(st, sb, eng)
}

// gitEndpointForHint 把 SSH 监听地址归一为 remote 提示的 host:port。主机位
// 解析链（2026-09-26 走查 W2-2：通配监听回落 127.0.0.1 使远程用户复制出
// 不可用 remote——服务端无法自行得知公网主机名，按「显式告知 > 平台域名 >
// 监听地址」推导）：git.public_endpoint 显式配置 > base_domain 域名（平台
// 公网面，DNS/证书与 git SSH 同宿主）> 监听地址主机位（通配/空回落
// 127.0.0.1——纯本机形态提示面永不输出空 host）。
func gitEndpointForHint(addr, publicEndpoint, baseDomain string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8424"
	}
	if publicEndpoint != "" {
		if _, _, perr := net.SplitHostPort(publicEndpoint); perr != nil {
			publicEndpoint = net.JoinHostPort(publicEndpoint, port)
		}
		return publicEndpoint
	}
	if baseDomain != "" {
		return net.JoinHostPort(strings.TrimSuffix(baseDomain, "."), port)
	}
	host, _, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// NewDeploymentsService 构造部署资源面服务（T2.17；T2.19 增补 DeployFromGit
// ——git 源端口由 internal/gitserver 实现，方向纪律：api 定义端口）。
func NewDeploymentsService(st *state.Store, src *gitserver.GitTriggers) *api.DeploymentsService {
	return api.NewDeploymentsService(st, src)
}

// NewGitTriggers 构建 git 触发入口核心（T2.19：bare 仓库管理 + post-receive
// 钩子 + DeployFromCommit + webhook 验签/防重放/去重/拉源 + SSH 服务器）。
// git.*/webhook.* 配置节经 AppConfig.GitSettings 翻译（缺省回落
// internal/gitserver 单一事实源）。
func NewGitTriggers(cfg *AppConfig, st *state.Store, sb *secrets.Box, app lynx.App) *gitserver.GitTriggers {
	return gitserver.NewGitTriggers(cfg.GitSettings(), st, sb, app.Logger())
}

// NewGitKeysService 构造 git 公钥管理面服务（T2.19；admin scope）。
func NewGitKeysService(st *state.Store) *api.GitKeysService {
	return api.NewGitKeysService(st)
}

// NewRevisionsService 构造版本快照只读面服务。
func NewRevisionsService(st *state.Store) *api.RevisionsService {
	return api.NewRevisionsService(st)
}

// NewBuildsService 构造构建资源面服务（T2.18；A11/S18 增补队列接线——
// TriggerBuild 经 Queue.Enqueue 入队，同进程触发立即唤醒扫描；IMPL-T2-2
// 增补上传构建面配置——BuildFromUpload 的落点与配额）。
func NewBuildsService(cfg *AppConfig, st *state.Store, q *build.Queue) *api.BuildsService {
	return api.NewBuildsService(st, q, cfg.BuildSettings().UploadConfig())
}

// NewDriftService 构造漂移面服务（T2.18；复用引擎对账原语）。
func NewDriftService(st *state.Store, eng *engine.Engine) *api.DriftService {
	return api.NewDriftService(st, eng)
}

// NewRuntimeService 构造运行实况面服务（2026-09-29 Console IA 重设计；
// 引擎观测端口上抛，只读投影）。
func NewRuntimeService(st *state.Store, eng *engine.Engine) *api.RuntimeService {
	return api.NewRuntimeService(st, eng)
}

// NewSystemService 构造系统/集群观察面服务（T2.18 起 SystemService 实现在
// internal/api；健康组件集在本装配点命名——lynx Checker 接口无名。Traefik
// 是降级设计（收敛/续期失败只日志告警，ingress 服务壳 CheckHealth 恒健康）
// ——组件集与装配壳同语义如实上报恒健康）。备份组件（T2.22）的检查器 =
// statebackup.Manager.CheckHealth：无 verified 备份 / 最近一次 verify 失败
// → 不健康（红色告警面：台账 failed 行 + backup.failed 审计 + 此组件）。
// E1-8：join 向导面随 cfg.BaseDomain 与 substrate join 端口接线（base_domain
// 空 = 单节点形态，GetJoinGuide 以 D-MN-13 门禁 409 拒绝、不触底座）。
// E3-2：S3 设置面随 envelope 加解密器接线（secret 密文落库/指纹读面/探针
// 解密）；baseDomain 同供 s3.public_exposed 门禁（E_S3_PUBLIC_REQUIRES_
// BASE_DOMAIN）。E3-5：托管 RustFS 组件（objectstore.rustfs）与 TestConnection
// 的 rustfs 分支随 duty 管理器接线——mode=rustfs 且服务未在位 = 红（收敛
// 过渡态如实可见）；mode 非 rustfs = 无所欠恒绿。E6 W5-S1：victorialogs
// 组件随 duty 管理器与日志管线接线（设计 §2.3：healthy = duty 部署符合
// 预期且 ingest streak 无降级；降级时 Error 带丢弃计数——诚实红面）。E6
// W5-S4：notifications 组件随投递器接线（设计 §5.2——启用端点连续终败即
// 红，Error 带端点名与最近错误；无终败 = 无所欠恒绿）。W3-S2：git SSH
// host key 指纹源随 GitTriggers 接线（FZ-12 披露面，D-W0-8——
// GetSystemStatus 的 git_ssh_fingerprint 现读）。
func NewSystemService(cfg *AppConfig, st *state.Store, id *state.NodeIdentity, ob *state.Observer, sb *secrets.Box, ing *ingress.Manager, bm *statebackup.Manager, rm *rustfs.Manager, sc *substrate.Client, lm *logs.Manager, vm *victorialogs.Manager, mm *metrics.Manager, nm *notify.Manager, erm *execrelay.Manager, gt *gitserver.GitTriggers, version Version) *api.SystemService {
	components := func() []api.SystemComponent {
		return []api.SystemComponent{
			{Name: "state.store", Check: st.CheckHealth},
			{Name: "state.identity", Check: id.CheckHealth},
			{Name: "state.observer", Check: ob.CheckHealth},
			{Name: "state.secrets", Check: sb.CheckHealth},
			{Name: "state.backup", Check: bm.CheckHealth},
			{Name: "objectstore.rustfs", Check: rm.CheckHealth},
			{
				Name: "victorialogs",
				Check: func() error {
					if vm != nil {
						if err := vm.CheckHealth(); err != nil {
							return err
						}
					}
					if lm.IngestDegraded() {
						return &ingestDegradedError{
							since:   lm.IngestStreakSince(),
							dropped: lm.IngestDroppedTotal(),
						}
					}
					return nil
				},
			},
			// E6 W5-S3：metrics 组件（设计 §4.1——mode=on 且三件未收敛/
			// VM 拨测不可达时红；mode 非 on = 无所欠恒绿，opt-in 缺省零
			// 常驻的诚实表达）。
			{Name: "metrics", Check: func() error {
				if mm == nil {
					return nil
				}
				return mm.CheckHealth()
			}},
			// E6 W5-S4：notifications 组件（设计 §5.2——启用端点连续终败
			// 即红，Error 带端点名与最近错误；通知自身零事件，红面只经
			// 此组件与台账可见）。
			{Name: "notifications", Check: func() error {
				if nm == nil {
					return nil
				}
				return nm.CheckHealth()
			}},
			// E7 W5-S6：execrelay 组件（terminal.enabled=true 时 relay 服务
			// 应在位——缺失即收敛未完成；false = 无所欠恒绿，victorialogs
			// 组件同口径）。
			{Name: "execrelay", Check: func() error {
				if erm == nil {
					return nil
				}
				return erm.CheckHealth()
			}},
			{Name: "ingress.traefik", Check: func() error { return nil }},
		}
	}
	return api.NewSystemService(string(version), st, components, ing, rm).WithBackupManager(bm).
		WithJoinGuide(cfg.BaseDomain, sc).
		WithSecretsBox(sb).
		WithGitHostKey(gt)
}

// ingestDegradedError 是日志入湖降级的组件健康错误（Error 文本带丢弃
// 计数与 streak 起点——设计 §2.3「降级时 Error 带丢弃计数」）。
type ingestDegradedError struct {
	since   time.Time
	dropped uint64
}

func (e *ingestDegradedError) Error() string {
	msg := fmt.Sprintf("log ingestion to VictoriaLogs is degraded (live tail unaffected; dropped_total=%d", e.dropped)
	if !e.since.IsZero() {
		msg += ", since=" + e.since.UTC().Format(time.RFC3339)
	}
	return msg + ")"
}

// NewDomainsService 构造域名台账/验证面服务。
func NewDomainsService(st *state.Store, m *ingress.Manager) *api.DomainsService {
	return api.NewDomainsService(st, m)
}

// NewEnvService 构造平台 env 面服务（值加密边界在服务实现内）。H9：注入
// env 写路径联动回调（set/remove 成功后即时失效日志脱敏值集，把 secret
// 明文采集暴露窗从 TTL 30s 收敛到单次重建）。
func NewEnvService(st *state.Store, sb *secrets.Box, lm *logs.Manager) *api.EnvService {
	return api.NewEnvService(st, sb).WithEnvChangedHook(lm.InvalidateRedaction)
}

// NewLogsService 构造日志面服务（T2.20：Follow/History 接管线管理器）。
// W5-S1：注入 VL 消费端与 duty 管理器（SearchLogs 检索面 + backend 视图
// 的部署态——nil 形态如实报不可用/unknown）。
func NewLogsService(st *state.Store, mg *logs.Manager, vl *victorialogs.Backend, vm *victorialogs.Manager) *api.LogsService {
	return api.NewLogsService(st, mg).WithVictorialogs(vl, vm)
}

// NewMetricsService 构造 metrics 面服务（E6 W5-S3，D-W5-2 opt-in：PromQL
// 查询透传面 + 状态视图 + 模式切换；注入 VM 消费端与 duty 管理器——nil
// 形态如实报不可用/unknown。retention 对齐 metrics.* 配置节；集群节点
// 总数 = 观测缓存计数（展示/诊断读面，非决策路径）——「N/M nodes
// reporting」的分母；§6 挂账票修订后分母口径与抓取目标集一致 = Ready 且
// availability=active（drain/pause 节点无 global 采集器，不进分母——
// 分子分母同拓扑，N<M 只剩「不 Ready」与「VPC 不可达」两种因由）。
func NewMetricsService(cfg *AppConfig, st *state.Store, mb *metrics.Backend, mm *metrics.Manager) *api.MetricsService {
	return api.NewMetricsService(st).WithBackend(mb, mm).
		WithRetentionDays(cfg.MetricsSettings().RetentionDays).
		WithNodesTotal(func(ctx context.Context) (int, error) {
			nodes, err := st.ListCachedNodes(ctx)
			if err != nil {
				return 0, err
			}
			total := 0
			for _, n := range nodes {
				if n.State == "ready" && n.Availability == "active" {
					total++
				}
			}
			return total, nil
		})
}

// NewEventsService 构造事件流面服务（seq 游标）。
func NewEventsService(st *state.Store) *api.EventsService {
	return api.NewEventsService(st)
}

// NewNotificationsService 构造通知订阅/投递面服务（E6 W5-S4 / W4-S3 通道
// 扩展：端点 CRUD + 台账读面 + TestWebhook（按类型试发）+ SMTP 设置三
// RPC——box 承载 secret/密码 envelope 加解密，投递本体由 notify.Manager
// 常驻循环承载）。
func NewNotificationsService(st *state.Store, sb *secrets.Box) *api.NotificationsService {
	return api.NewNotificationsService(st, sb)
}

// NewAlertingService 构造告警面服务（B 线 W5-S2，D-V3W5-1：规则 CRUD +
// alerts.mode 开关 + 栈状态视图 + TestAlertRule 即时求值——查询后端与
// metrics duty 管理器注入，与 NewMetricsService 同款装配缝）。
func NewAlertingService(st *state.Store, mb *metrics.Backend, mm *metrics.Manager) *api.AlertingService {
	return api.NewAlertingService(st).WithBackend(mb, mm)
}

// NewExecService 构造 Web 终端受理面服务（E7 W5-S6：ticket 签发 + 状态
// 视图——整体 terminal scope；注入 relay duty 管理器承接 status 部署态）。
func NewExecService(st *state.Store, hub *execrelay.Hub, erm *execrelay.Manager) *api.ExecService {
	return api.NewExecService(st, hub).WithDutyManager(erm)
}

// NewTerminalNativeHandler 构造 Web 终端 native 端点 handler（E7 W5-S6：
// GET /internal/exec-relay 的 relay 反向常连 + GET /v1/terminal 的浏览器
// WS——newRootHandler 的精确路径分派面，例外清单见 gateway.go）。
func NewTerminalNativeHandler(app lynx.App, hub *execrelay.Hub) *execrelay.NativeHandler {
	return execrelay.NewNativeHandler(execrelay.NativeConfig{Hub: hub, Log: app.Logger()})
}

// NewAlertsReceiverHandler 构造平台内建告警接收器（B 线 W5-S2，D-V3W5-1：
// POST /internal/alerts——newRootHandler 的精确路径分派面，例外清单见
// gateway.go）。凭据 = ingress token 同源（Manager.Token 幂等加载/生成）；
// 投递面 = notify.Manager（告警映射：channels 缺省全端点、RESOLVED 前缀、
// 零事件红线在 notify/state 层钉死）。ing 为 nil（测试精简装配）= 不挂载。
func NewAlertsReceiverHandler(app lynx.App, ing *ingress.Manager, nm *notify.Manager) (http.Handler, error) {
	if ing == nil || nm == nil {
		return nil, nil
	}
	token, err := ing.Token(context.Background())
	if err != nil {
		return nil, fmt.Errorf("alerts receiver: load ingress token: %w", err)
	}
	return newAlertsReceiverHandler(token, nm, app.Logger()), nil
}

// NewPlacementService 构造放置面服务（T2.17 只读 + E1-7 显式换点/卷清单/
// 迁移 runbook——换点裁决经 placement.Resolver，api 面只做解析与投影）。
func NewPlacementService(st *state.Store, res *placement.Resolver) *api.PlacementService {
	return api.NewPlacementService(st, res)
}

// NewTokensService 构造 token 管理面服务。
func NewTokensService(st *state.Store) *api.TokensService {
	return api.NewTokensService(st)
}

// NewAuthService 构造认证面服务（v0.3 W1，rbac-teams §2：注册/登录/会话；
// 注册与登录的 email+IP 双键限流内置。W2-S4 收口：会话 cookie Secure 位随
// 控制面 TLS 模式（off 不带 / platform+manual 带）、滑动窗口 TTL 取 config
// auth.session_ttl_hours——rbac-teams §2.2 的装配面注入）。
func NewAuthService(cfg *AppConfig, st *state.Store) *api.AuthService {
	auth := api.NewAuthService(st)
	return auth.WithSessionSecurity(cfg.TLSMode() != ControlPlaneTLSOff, cfg.SessionTTL())
}

// NewUsersService 构造平台用户管理面服务（v0.3 W1，rbac-teams §2.1/§3.2；
// 平台管理员判定在 handler 内强制）。
func NewUsersService(st *state.Store) *api.UsersService {
	return api.NewUsersService(st)
}

// NewAuditService 构造审计读面服务（v0.3 W3-S1，rbac-teams §5/§6 裁决
// D-W0-6：过滤+分页的台账检索；平台管理员双门——scope admin + handler
// requirePlatformAdminPrincipal 共享单点，internal/api/audit.go 头注）。
func NewAuditService(st *state.Store) *api.AuditService {
	return api.NewAuditService(st)
}

// NewTeamsService 构造团队/成员/邀请面服务（v0.3 W2-S1，rbac-teams §3.1/
// §3.2；角色门在 handler 内强制——scope 登记见 internal/api/scope.go）。
func NewTeamsService(st *state.Store) *api.TeamsService {
	return api.NewTeamsService(st)
}

// NewProjectsService 构造项目/队内覆写成员面服务（v0.3 W2-S1，rbac-teams
// §3.3/§3.4；覆写管理权判定在 handler 内强制）。W2-S3：注入改派换名重部署
// 编排端口（engine 换名发布/等待/清扫 + database 收敛 + ingress 摘网——
// api.AppMovePort / api.DBMovePort / api.IngressMovePort 的实现粘合）。
func NewProjectsService(st *state.Store, eng *engine.Engine, dbm *database.Manager, ing *ingress.Manager) *api.ProjectsService {
	svc := api.NewProjectsService(st)
	svc = svc.WithMovePorts(
		appMovePort{eng: eng},
		dbMovePort{mgr: dbm},
		ingressMovePort{mgr: ing},
	)
	// IMPL-T15-1 项目网参与面：ensure + 参与变更重部署（engine 实现）。
	svc = svc.WithNetworkPort(appNetworkPort{eng: eng})
	return svc
}

// appNetworkPort 是 api.ProjectNetworkPort 的 engine.Engine 实现（IMPL-T15-1
// 项目网生命周期与参与变更重部署；internal/engine/projectnetwork.go）。
type appNetworkPort struct {
	eng *engine.Engine
}

func (p appNetworkPort) EnsureProjectNetwork(ctx context.Context, projectID string) error {
	return p.eng.EnsureProjectNetwork(ctx, projectID)
}

func (p appNetworkPort) EnqueueNetworkRedeploy(ctx context.Context, appID string) (string, error) {
	return p.eng.EnqueueNetworkRedeploy(ctx, appID)
}

// appMovePort 是 api.AppMovePort 的 engine.Engine 实现（MoveApp 换名重
// 部署编排；internal/engine/move.go）。
type appMovePort struct {
	eng *engine.Engine
}

func (p appMovePort) EnqueueMoveRedeploy(ctx context.Context, appID string) (string, error) {
	return engine.EnqueueMoveRedeploy(ctx, p.eng.Store(), appID)
}

func (p appMovePort) AwaitAppSwap(ctx context.Context, appID string, timeout time.Duration) error {
	return p.eng.AwaitAppSwap(ctx, appID, timeout)
}

func (p appMovePort) SweepMovedServices(ctx context.Context, oldQualified string) (int, error) {
	return p.eng.SweepMovedServices(ctx, oldQualified)
}

// dbMovePort 是 api.DBMovePort 的 database.Manager 实现（MoveDatabase 换名
// 重部署收敛编排；internal/database/move.go）。
type dbMovePort struct {
	mgr *database.Manager
}

func (p dbMovePort) MoveDatabaseRedeploy(ctx context.Context, inst state.DatabaseInstance, oldTeamSlug, oldPrjSlug string) error {
	return p.mgr.MoveDatabaseRedeploy(ctx, inst, oldTeamSlug, oldPrjSlug)
}

// ingressMovePort 是 api.IngressMovePort 的 ingress.Manager 实现（摘旧网
// 收尾面）。
type ingressMovePort struct {
	mgr *ingress.Manager
}

func (p ingressMovePort) DetachAppNetwork(ctx context.Context, team, prj, app string) error {
	return p.mgr.DetachAppNetwork(ctx, team, prj, app)
}

// NewHTTPServer 创建控制面 HTTP 服务：根 handler 是 grpc-gateway mux
// （REST /v1/** 经 gateway 反代到本进程 gRPC，见 newGatewayMux）外包原生
// 端点分派（newRootHandler——webhook、GET / 引导页、Console /ui/ 静态
// 托管与 POST /internal/alerts 告警接收器〔W5-S2〕，例外清单见 gateway.go）；
// /healthz/liveness 与 /healthz/readiness 由 lynxhttp.Server 自行挂载，与
// gateway 路由共存（torchwood 同款双面单端口形态）。Console 静态托管在
// console.static_dir 非空时挂载；未配置时回落镜像内置目录（consoleBakedDir
// ——存在才启用：容器形态开箱即有 /ui/，原生形态无此目录缺省关闭，口径
// 不变），目录缺 index.html 时 fail-fast 拒绝启动。
//
// M4-3：经 WithServerOptions 放宽 WriteTimeout（见 tuneHTTPServer——lynx
// 缺省 60s 绝对超时会静默掐断 /v1/events/stream 与 logs stream）。
func NewHTTPServer(app lynx.App, cfg *AppConfig, src *gitserver.GitTriggers, ctl *ControlPlaneTLS, terminal *execrelay.NativeHandler, alertsHandler http.Handler) (*lynxhttp.Server, error) {
	// 回拨凭据跟随控制面 TLS 形态（W5-S5 缺陷修复：TLS 形态下明文回拨使
	// 全量 REST /v1 断——见 newGatewayMuxWithTLS 注记；回环自拨 + 进程自
	// 身信任锚的 InsecureSkipVerify，外部面 TLS 由监听器强制）。
	var gwTLS *tls.Config
	if cfg.TLSMode() != ControlPlaneTLSOff {
		//nolint:gosec // G402：回环自拨的既定语义——`?insecure` 网关回拨形态（外部面 TLS 由监听器强制）
		gwTLS = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
	}
	mux, err := newGatewayMuxWithTLS(grpcEndpointFromAddr(cfg.GRPCAddr()), gwTLS)
	if err != nil {
		return nil, err
	}
	var consoleUI http.Handler
	if dir := consoleDirOrDefault(cfg.Console.StaticDir, consoleBakedDir); dir != "" {
		consoleUI, err = newConsoleUIHandler(dir)
		if err != nil {
			return nil, err
		}
	}
	root := newRootHandler(gitserver.NewWebhookHandler(src), consoleUI, terminal, alertsHandler, mux)
	opts := []lynxhttp.Option{
		lynxhttp.WithAddr(cfg.Addr),
		lynxhttp.WithHealthCheckers(app.HealthCheckers),
		lynxhttp.WithLogger(app.Logger("logger", "http-requestlog")),
		lynxhttp.WithServerOptions(tuneHTTPServer),
	}
	// V2-8（E7 同批）：control_plane.tls 非 off 时 HTTP 面 TLS 化（gateway +
	// native 端点 + /ui + healthz 同证书——双面单端口形态不变，只是传输层
	// 换 TLS）。off（缺省）不加选项 = 今日明文行为逐字不变。
	if ctl != nil {
		opts = append(opts, lynxhttp.WithTLSConfig(ctl.TLSConfig()))
	}
	return lynxhttp.NewServer(root, opts...), nil
}

// streamWriteTimeout 是长驻流式端点的 WriteTimeout（M4-3）：15 分钟——
// /v1/events/stream（Watch）与 logs Follow 是无界流，lynx 缺省 60s 绝对
// WriteTimeout 每 60s 把连接静默掐断一次（客户端无任何信令），Console
// 断线重连退避虽能兜住但掐断频率不可接受。15min 覆盖长驻观察会话；到期
// 掐断由客户端重连（seq 游标/Follow 位点续传）消化，连接不应无限存活。
const streamWriteTimeout = 15 * time.Minute

// tuneHTTPServer 是 M4-3 的底层 *http.Server 调优钩子（lynxhttp 在内部
// 超时配置之后应用 ServerOptions，只覆写 WriteTimeout）：
//   - ReadHeaderTimeout / ReadTimeout 保持 lynx 缺省 60s 不动——慢速攻击
//     （slowloris）防线维持紧口径；body 物化面由 H7 的 MaxBytesReader
//     （32MiB）单独设界，ReadTimeout 对合法上传的约束因此可接受
//     （REST 载荷 JSON+compose、webhook 投递 10s 内完成是现实基线）；
//   - WriteTimeout 放宽到 streamWriteTimeout（15min），覆盖流式端点。
func tuneHTTPServer(srv *http.Server) {
	srv.WriteTimeout = streamWriteTimeout
}

// NewServices 聚合全部受托管服务。MG-4（X-3，B6）停止不变量——注册顺序
// 即停止顺序：lynx 把每个服务登记为 oklog/run actor，run.Group 在关停时
// **按注册顺序逐个调用 interrupt**，而 lynx 的 interrupt 包壳阻塞在该服务
// 的 Stop 上——首注册者最先停。Start 则是并发触达（run.Group 为每个 actor
// 起 goroutine，无跨服务等待），不存在硬性启动顺序；Init 严格按注册序，
// 但全部 Init 相互独立（store 迁移在 wire 装配期完成，非 Init 阶段）。
//
// 三段停止不变量（顺序不可倒置）：
//  1. 入口面最先停（HTTP/gRPC/git SSH + webhook worker）——SIGTERM 后
//     立即拒绝新工作（连接排水），消灭「Deploy/TriggerBuild 仍假成功入队
//     但引擎/队列已死」的窗口。webhook worker 的排空也在本段：drain 中
//     处理的 job 写部署行，此时引擎已停、行只会排队待重启恢复——可接受；
//  2. 写入者随后（build queue → engine → ingress → 日志采集）——入口已
//     关，写入者安心排空在途（队列认领、状态机 tick、路由发布、日志尾随）。
//     ingress 在 engine 之后保持原相对序（引擎 tick 触发发布时配置端点
//     已监听的弱偏置；Traefik 对配置端点不可达保留旧配置，先停无害）；
//  3. 资源层最后（identity/observer/janitor → backup → secrets → store）
//     ——backup 必须晚于 engine：post-deploy 备份挂钩是引擎成功路径逸出
//     的异步 goroutine，backup.Stop 等待在途快照收口（X-7）；store 最后
//     （其 Stop 无资源动作，连接池由 Wire cleanup 在 OnPostStop 释放——
//     晚于全部服务 Stop，排水期在途请求仍可读库）。
//
// lynx 集成级 SIGTERM 端到端测试（真进程信号→逐服务 Stop 时序）成本过高
// 不做，挂账：顺序契约由 TestNewServicesStopOrder（结构断言）+ lynx 自身
// ordered/stopRange 语义共同钉住。
func NewServices(
	app lynx.App,
	st *state.Store,
	id *state.NodeIdentity,
	ob *state.Observer,
	jr *state.Janitor,
	bm *statebackup.Manager,
	sb *secrets.Box,
	q *build.Queue,
	b *build.Builder,
	eng *engine.Engine,
	ing *ingress.Manager,
	lm *logs.Manager,
	cm *cron.Manager,
	nm *notify.Manager,
	src *gitserver.GitTriggers,
	cfg *AppConfig,
	rm *rustfs.Manager,
	vm *victorialogs.Manager,
	mm *metrics.Manager,
	dm *database.Manager,
	erm *execrelay.Manager,
	hs *lynxhttp.Server,
	gs *lynxgrpc.Server,
) []lynx.Service {
	return []lynx.Service{
		// ── 第一段：入口面（最先注册 = 最先停：拒绝新工作）──
		hs,
		gs,
		newGitService(src, app, cfg.GitSettings().Enabled, cfg.GitSettings().Addr),
		// ── 第二段：写入者（入口关后排空在途）──
		newBuilderService(q, b, app.Logger()),
		newEngineService(eng),
		newIngressService(ing, app, cfg.IngressSettings().ConfigAddr, cfg.IngressSettings().ConfigTLSAddr),
		newLogsService(lm),
		newNotifyService(nm),
		// ── 第三段：资源层（最后停：backup 晚于 engine 等 post-deploy
		//     在途快照；rustfs/victorialogs/database 收敛 duty 同层——在途
		//     收敛拍随 ctx 排水；cron 调度器同层——触发链与收口拍随 ctx
		//     排水，残留 job 由下次启动首拍收口兜底；store 最后）──
		newIdentityService(id),
		newObserverService(ob),
		newJanitorService(jr),
		newBackupService(bm),
		newRustfsService(rm),
		newVictorialogsService(vm),
		newMetricsService(mm),
		newDatabaseService(dm),
		newExecRelayService(erm),
		newCronSchedulerService(cm),
		newSecretsService(sb),
		newStoreService(st),
	}
}

func NewServiceFactories() []lynx.ServiceFactory {
	return []lynx.ServiceFactory{}
}

func NewPreStarts(app lynx.App) boot.PreStartHooks {
	return boot.PreStartHooks{
		func(ctx context.Context) error {
			app.Logger().Info("fleetlyd starting")
			return nil
		},
	}
}

func NewDrains() boot.DrainHooks {
	return boot.DrainHooks{}
}

func NewPreStops(app lynx.App) boot.PreStopHooks {
	return boot.PreStopHooks{
		func(ctx context.Context) error {
			app.Logger().Info("fleetlyd stopping")
			return nil
		},
	}
}

func NewPostStops() boot.PostStopHooks {
	return boot.PostStopHooks{}
}
