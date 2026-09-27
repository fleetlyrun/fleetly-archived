// Package fleetly 是 fleetly 平台的 Go SDK：对 fleetlyd gRPC 面的客户端
// 封装（CLI/集成方共用，契约来自 proto 生成物 genproto——proto 唯一真源，
// D21）。覆盖 v0.1 全部 13 个服务面（system/apps/deployments/revisions/
// builds/drift/domains/env/logs/events/placement/tokens/gitkeys）；服务
// 方法随 proto 模块扩展同步添加，不在 SDK 层发明契约外语义。
package fleetly

import (
	"context"
	"crypto/tls"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// DefaultAddr 是 SDK 默认连接地址（与 fleetlyd gRPC 面缺省监听一致，
// 见 cmd/fleetlyd/config.go 的 defaultGRPCAddr）。
const DefaultAddr = "127.0.0.1:8421"

// Option 修改 Client 配置。
type Option func(*clientConfig)

type clientConfig struct {
	addr        string
	token       string
	dialOptions []grpc.DialOption
	// tlsCfg 是显式 TLS 客户端配置（WithTLS；nil = 未显式给全量配置）。
	tlsCfg *tls.Config
	// tlsInsecure 是「TLS 拨号但跳过证书校验」开关（WithTLSInsecure）。
	tlsInsecure bool
}

// WithAddr 覆盖 gRPC 目标地址（默认 127.0.0.1:8421）。
func WithAddr(addr string) Option { return func(c *clientConfig) { c.addr = addr } }

// WithTLS 启用 TLS 拨号并校验服务端证书：cfg 至少装配信任面（RootCAs 指向
// 控制面证书的 CA——platform 模式即 LE 根，私 CA 场景经 ca_pool_file 体系；
// 或系统信任池）。ServerName 缺省取拨号地址的 host 位（tls.Config 零值语
// 义）——拨 IP 直连时按需显式设 cfg.ServerName 为证书 SAN 名（如
// ctrl.<base>）。与 WithTLSInsecure 同传时本选项优先（全量配置更明确）。
func WithTLS(cfg *tls.Config) Option { return func(c *clientConfig) { c.tlsCfg = cfg } }

// WithTLSInsecure 启用 TLS 拨号但跳过证书校验（staging 自签/IP 直连的显式
// 旁路——等价 WithTLS(&tls.Config{InsecureSkipVerify: true})，传输仍加密，
// 只是不验链；不要对公网控制面使用）。
func WithTLSInsecure() Option { return func(c *clientConfig) { c.tlsInsecure = true } }

// transportCredentials 按解析序装配传输凭据：显式 TLS 配置 > insecure 开关
// > 明文（缺省——存量单节点回环形态逐字兼容）。
func (c *clientConfig) transportCredentials() credentials.TransportCredentials {
	switch {
	case c.tlsCfg != nil:
		return credentials.NewTLS(c.tlsCfg)
	case c.tlsInsecure:
		return credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // G402：WithTLSInsecure 的显式语义
	default:
		return insecure.NewCredentials()
	}
}

// tlsSelected 报告拨号是否走 TLS（明文 = false）——Bearer 凭据的
// RequireTransportSecurity 跟随之（见 bearerCredentials）。
func (c *clientConfig) tlsSelected() bool { return c.tlsCfg != nil || c.tlsInsecure }

// WithToken 设置 Bearer token：经 PerRPCCredentials 挂进每个请求的
// authorization metadata（含流式首帧）。token 只进请求 metadata，不进
// SDK 侧任何日志/错误文本（凭据不外溢纪律）。
func WithToken(token string) Option { return func(c *clientConfig) { c.token = token } }

// WithDialOptions 附加底层 gRPC 拨号选项（如 TLS 凭据、消息大小上限）。
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *clientConfig) { c.dialOptions = append(c.dialOptions, opts...) }
}

// Client 是 fleetlyd gRPC 面的客户端封装：单连接复用，goroutine 安全。
// 各服务面经同名访问器取用（返回 proto 生成客户端——方法契约即 proto）。
type Client struct {
	conn     *grpc.ClientConn
	system   serverv1.SystemServiceClient
	apps     serverv1.AppsServiceClient
	deploy   serverv1.DeploymentsServiceClient
	revs     serverv1.RevisionsServiceClient
	builds   serverv1.BuildsServiceClient
	drift    serverv1.DriftServiceClient
	doms     serverv1.DomainsServiceClient
	env      serverv1.EnvServiceClient
	logs     serverv1.LogsServiceClient
	metrics  serverv1.MetricsServiceClient
	alerts   serverv1.AlertingServiceClient
	notifs   serverv1.NotificationsServiceClient
	events   serverv1.EventsServiceClient
	place    serverv1.PlacementServiceClient
	tokens   serverv1.TokensServiceClient
	gitkey   serverv1.GitKeysServiceClient
	auth     serverv1.AuthServiceClient
	audit    serverv1.AuditServiceClient
	cron     serverv1.CronServiceClient
	dbs      serverv1.DatabaseServiceClient
	secs     serverv1.SecretsServiceClient
	configs  serverv1.ConfigsServiceClient
	projects serverv1.ProjectsServiceClient
	tasks    serverv1.TasksServiceClient
}

// NewClient 建立 gRPC 连接（默认 127.0.0.1:8421，明文——TLS 经 WithTLS/
// WithTLSInsecure 显式启用，V2-8；连接惰性建立）。调用方负责 Close。
func NewClient(opts ...Option) (*Client, error) {
	cfg := clientConfig{addr: DefaultAddr}
	for _, opt := range opts {
		opt(&cfg)
	}
	dialOpts := append(
		[]grpc.DialOption{grpc.WithTransportCredentials(cfg.transportCredentials())},
		cfg.dialOptions...,
	)
	// token 非空才挂凭据——空 token 让服务端以 401 显式拒绝（信封退化
	// 形态），客户端侧不伪造「已鉴权」表象。凭据的 RequireTransportSecurity
	// 跟随拨号面：TLS 拨号 = true（凭据拒绝经明文连接发送），明文拨号 =
	// false（存量兼容——gRPC 会对「要求安全却走明文」的请求面直接失败，
	// 不存在静默降级）。
	if cfg.token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(bearerCredentials{token: cfg.token, requireTLS: cfg.tlsSelected()}))
	}
	conn, err := grpc.NewClient(cfg.addr, dialOpts...)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn:     conn,
		system:   serverv1.NewSystemServiceClient(conn),
		apps:     serverv1.NewAppsServiceClient(conn),
		deploy:   serverv1.NewDeploymentsServiceClient(conn),
		revs:     serverv1.NewRevisionsServiceClient(conn),
		builds:   serverv1.NewBuildsServiceClient(conn),
		drift:    serverv1.NewDriftServiceClient(conn),
		doms:     serverv1.NewDomainsServiceClient(conn),
		env:      serverv1.NewEnvServiceClient(conn),
		logs:     serverv1.NewLogsServiceClient(conn),
		metrics:  serverv1.NewMetricsServiceClient(conn),
		alerts:   serverv1.NewAlertingServiceClient(conn),
		notifs:   serverv1.NewNotificationsServiceClient(conn),
		events:   serverv1.NewEventsServiceClient(conn),
		place:    serverv1.NewPlacementServiceClient(conn),
		tokens:   serverv1.NewTokensServiceClient(conn),
		gitkey:   serverv1.NewGitKeysServiceClient(conn),
		auth:     serverv1.NewAuthServiceClient(conn),
		audit:    serverv1.NewAuditServiceClient(conn),
		cron:     serverv1.NewCronServiceClient(conn),
		dbs:      serverv1.NewDatabaseServiceClient(conn),
		secs:     serverv1.NewSecretsServiceClient(conn),
		configs:  serverv1.NewConfigsServiceClient(conn),
		projects: serverv1.NewProjectsServiceClient(conn),
		tasks:    serverv1.NewTasksServiceClient(conn),
	}, nil
}

// bearerCredentials 是 Bearer token 的 PerRPCCredentials 实现：每个请求
// （一元与流式）自动携带 authorization metadata。
type bearerCredentials struct {
	token string
	// requireTLS 报告凭据是否只经 TLS 连接发送（V2-8，设计 §3.2）：跟随
	// 拨号面——TLS 拨号（WithTLS/WithTLSInsecure）= true，gRPC 传输层据此
	// 拒绝把凭据发上明文连接（安全面由传输凭据与凭据声明一致性强制，不靠
	// 调用方自律）；明文拨号（缺省）= false——v0.1 控制面缺省明文回环
	//（fleetlyd 默认只绑 127.0.0.1）的存量形态逐字兼容。
	requireTLS bool
}

func (b bearerCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

// RequireTransportSecurity 报告凭据是否要求 TLS：跟随拨号面（见字段注释）。
func (b bearerCredentials) RequireTransportSecurity() bool { return b.requireTLS }

// System 取系统/集群观察面（Ping/Status/Nodes/Ingress）。
func (c *Client) System() serverv1.SystemServiceClient { return c.system }

// Apps 取应用资源面。
func (c *Client) Apps() serverv1.AppsServiceClient { return c.apps }

// Deployments 取部署资源面。
func (c *Client) Deployments() serverv1.DeploymentsServiceClient { return c.deploy }

// Revisions 取版本快照只读面。
func (c *Client) Revisions() serverv1.RevisionsServiceClient { return c.revs }

// Builds 取构建资源面。
func (c *Client) Builds() serverv1.BuildsServiceClient { return c.builds }

// Drift 取运行域漂移面。
func (c *Client) Drift() serverv1.DriftServiceClient { return c.drift }

// Domains 取域名台账/验证面。
func (c *Client) Domains() serverv1.DomainsServiceClient { return c.doms }

// Env 取平台 env 面。
func (c *Client) Env() serverv1.EnvServiceClient { return c.env }

// Logs 取应用日志面。
// Logs 取日志面客户端（Follow/History/SearchLogs/backend 视图）。
func (c *Client) Logs() serverv1.LogsServiceClient { return c.logs }

// Metrics 取 metrics 面客户端（E6 W5-S3：SearchMetrics/GetMetricsStatus/
// SetMetricsMode）。
func (c *Client) Metrics() serverv1.MetricsServiceClient { return c.metrics }

// Alerts 取告警面客户端（B 线 W5-S2，D-V3W5-1：规则 CRUD/SetAlertsMode/
// GetAlertsStatus/TestAlertRule）。
func (c *Client) Alerts() serverv1.AlertingServiceClient { return c.alerts }

// Notifications 取通知 Webhook 面（E6 W5-S4：端点 CRUD/轮换/测试 + 投递
// 台账读面）。
func (c *Client) Notifications() serverv1.NotificationsServiceClient { return c.notifs }

// Events 取事件流面。
func (c *Client) Events() serverv1.EventsServiceClient { return c.events }

// Placement 取放置绑定只读面。
func (c *Client) Placement() serverv1.PlacementServiceClient { return c.place }

// Tokens 取 token 管理面（admin scope）。
func (c *Client) Tokens() serverv1.TokensServiceClient { return c.tokens }

// Auth 取认证面（v0.3 W1：Me/Register/Login/Logout/LogoutAll/AcceptInvite/
// GetRegistrationState——CLI 的登录验证与身份投影经此消费；注册/登录下发
// 的会话 cookie 是浏览器面凭据，CLI 不消费，见 AuthService 注释）。
func (c *Client) Auth() serverv1.AuthServiceClient { return c.auth }

// GitKeys 取 git 公钥管理面（admin scope，T2.19）。
func (c *Client) GitKeys() serverv1.GitKeysServiceClient { return c.gitkey }

// Audit 取审计读面（v0.3 W3-S1，rbac-teams §6 D-W0-6：过滤+分页的台账
// 检索——平台管理员用户凭据或 admin 机具令牌）。
func (c *Client) Audit() serverv1.AuditServiceClient { return c.audit }

// Cron 取定时任务面（E5 Cron：手动触发 + 运行台账读面）。
func (c *Client) Cron() serverv1.CronServiceClient { return c.cron }

// Databases 取库实例资源面（E4 数据库托管：生命周期 + rotate/reveal RPC +
// 脱敏连接视图）。
func (c *Client) Databases() serverv1.DatabaseServiceClient { return c.dbs }

// Secrets 取平台密钥库面（E4 managed-databases §2.7，D-DB-7：external
// secret 写面——无值读回，list 只出名称/指纹）。
func (c *Client) Secrets() serverv1.SecretsServiceClient { return c.secs }

// Configs 取明文配置资源面（T 线 OT-3/IMPL-T1-4：external config CRUD——
// 明文可回读，GetConfig 走 admin scope；list 只出名称/指纹）。
func (c *Client) Configs() serverv1.ConfigsServiceClient { return c.configs }

// Projects 取项目面（v0.3 W2-S1 项目/覆写成员 + W2-S3 资源改派 +
// IMPL-T15-1 项目网参与 attach/detach）。
func (c *Client) Projects() serverv1.ProjectsServiceClient { return c.projects }

// Tasks 取程序化动态工作负载面（T 线 DT-5 / IMPL-T2-1）：任务受理/视图/
// 停止/删除 + task-group 网络 ensure（整体 tasks 独立 scope；机具令牌为
// 典型持有者——CLI `fleetly tasks` 的消费面）。
func (c *Client) Tasks() serverv1.TasksServiceClient { return c.tasks }

// Ping 探测控制面存活并取回 service / version（豁免鉴权——装面前的
// 存活检查路径）。
func (c *Client) Ping(ctx context.Context) (*serverv1.PingResponse, error) {
	return c.system.Ping(ctx, &serverv1.PingRequest{})
}

// FollowLogs 跟随应用日志流：逐帧回调直至流结束（服务端关闭）或 ctx 取消
// ——断线重连 = 重新调用（Follow 无跨断线游标，回放窗口由服务端 ring
// 承载）。返回值携带流的最终状态（ctx 取消返回 ctx.Err）。
func (c *Client) FollowLogs(ctx context.Context, app, service string, fn func(*serverv1.FollowLogsResponse) error) error {
	stream, err := c.logs.FollowLogs(ctx, &serverv1.FollowLogsRequest{App: app, Service: service})
	if err != nil {
		return err
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err // io.EOF = 服务端正常关闭
		}
		if err := fn(frame); err != nil {
			return err
		}
	}
}

// WatchEvents 消费事件流：逐帧回调直至流结束或 ctx 取消。since_seq=0 =
// 从保留窗起点；游标早于保留窗时流上会先收到一帧 cursor_expired 信封帧
// 后正常关闭（消费方以 oldest_seq 重新拉全量——events.proto 取舍注记）。
func (c *Client) WatchEvents(ctx context.Context, sinceSeq int64, fn func(*serverv1.WatchEventsResponse) error) error {
	stream, err := c.events.WatchEvents(ctx, &serverv1.WatchEventsRequest{SinceSeq: sinceSeq})
	if err != nil {
		return err
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err // io.EOF = 服务端正常关闭
		}
		if err := fn(frame); err != nil {
			return err
		}
	}
}

// Close 释放底层连接。
func (c *Client) Close() error { return c.conn.Close() }
