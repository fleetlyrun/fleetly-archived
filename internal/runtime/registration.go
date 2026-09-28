package runtime

// 控制面 API 服务登记表（IMPL-ARCH-I，2026-09-28 架构评审候选 6b 前半）。
//
// 此前「新增一个服务/RPC」要手改三份平行清单：grpc.go 的
// RegisterXxxServiceServer 调用列、gateway.go 的 RegisterXxxHandlerFromEndpoint
// 调用列、internal/apitest 的测试装配登记列——清单靠人肉同步，漂移已实际
// 发生（apitest 漏登 CronService/ExecService）。本表是服务面的唯一清单：
//
//   - grpc.go（生产装配）：wire 供给的服务实例按名对位入装配集，经
//     RegisterGRPCServices 按表全量注册——表 ⇆ 装配集双向对账，缺条目或
//     缺实例都在构造期红（新服务漏登记从「静默缺面」变「启动即报错」）；
//   - gateway.go（REST 反代）：经 RegisterGatewayHandlers 按表序挂载全部
//     双面服务；gateway 注册器为空的条目 = 整体 gRPC-only（REST 面 404
//     是契约，挂载纪律的文档面见 gateway.go 头注）；
//   - internal/apitest（CLI/SDK 集成测试夹具）：消费同一张表同一注册
//     函数——测试侧只提供自己的装配集（fake 端口在构造期注入）。
//
// 双 adapter seam：表条目只登记「注册本体」（register 函数 + gateway 挂载
// 函数），不登记实例——生产构造（wire 供给，grpc.go 装配集）与测试装配
// （apitest 装配集，同型构造替换确定性假端口）是同一 seam 的两个 adapter，
// 实例集形状相同（服务名 → 服务实现），注册路径完全同构。
//
// 保序：表序 = 收编前的生产 gRPC 注册序（快照逐位搬运）。gRPC 注册序与
// gateway 挂载序都按表序展开——gateway 侧 25 个双面条目的相对序与收编前
// 的手写清单逐位一致（CronService/TasksService 收编前即整体未挂 gateway，
// 摘除后余序不变），路由行为零变化。
//
// 泛型：条目的类型参数 = genproto 生成的 XxxServiceServer 接口——register
// 函数签名在表声明处编译期对型，装配集实例在 bind 期还原为接口类型（失配
// = 装配缺陷，fail-fast 报错）。异构表经非泛型 ServiceRegistration 接口
// 承载；条目只能由本文件的两个构造助手产出（接口方法不导出，外部不可
// 自造条目）。

import (
	"context"
	"fmt"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// ServiceRegistration 是登记表条目的非泛型承载（异构表容器的元素类型）。
// 方法不导出：条目只能经本包的 grpcOnlyService / mountedService 构造，
// 外部消费者（apitest 等）只读不写。
type ServiceRegistration interface {
	// serviceName 返回服务短名（装配集的对位键，如 "AppsService"）。
	serviceName() string
	// bindGRPC 把装配集中对位的实例注册到 gRPC registrar（实例缺失或
	// 类型不符返回错误——装配 ⇆ 表失配 fail-fast）。
	bindGRPC(registrar grpc.ServiceRegistrar, instance any) error
	// mountGateway 把该服务的 REST 反代 handler 挂到 gateway mux（无
	// gateway 注册器的条目为空操作——gRPC-only）。
	mountGateway(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error
}

// serviceRegistration 是单个服务的类型化条目：Server 类型参数 = genproto
// 的 XxxServiceServer 接口（register 函数的第二参类型）。
type serviceRegistration[Server any] struct {
	name string
	// registerGRPC 是生成的 RegisterXxxServiceServer（grpc.ServiceRegistrar
	// 兼容 *grpc.Server 与测试 server）。
	registerGRPC func(grpc.ServiceRegistrar, Server)
	// registerGateway 是生成的 RegisterXxxHandlerFromEndpoint；nil = 整体
	// gRPC-only（REST 面 404 是契约）。
	registerGateway func(context.Context, *runtime.ServeMux, string, []grpc.DialOption) error
}

func (r serviceRegistration[Server]) serviceName() string { return r.name }

func (r serviceRegistration[Server]) bindGRPC(registrar grpc.ServiceRegistrar, instance any) error {
	server, ok := instance.(Server)
	if !ok {
		return fmt.Errorf("service registration %s: assembled instance %T does not implement the registered server interface", r.name, instance)
	}
	r.registerGRPC(registrar, server)
	return nil
}

func (r serviceRegistration[Server]) mountGateway(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error {
	if r.registerGateway == nil {
		return nil
	}
	return r.registerGateway(ctx, mux, endpoint, opts)
}

// grpcOnlyService 登记一个 gRPC-only 服务（不挂 gateway——REST 面 404 是
// 契约，gRPC-only 清单纪律见 gateway.go 头注）。
func grpcOnlyService[Server any](name string, register func(grpc.ServiceRegistrar, Server)) ServiceRegistration {
	return serviceRegistration[Server]{name: name, registerGRPC: register}
}

// mountedService 登记一个 gRPC + gateway 双面服务。
func mountedService[Server any](name string, register func(grpc.ServiceRegistrar, Server), gateway func(context.Context, *runtime.ServeMux, string, []grpc.DialOption) error) ServiceRegistration {
	return serviceRegistration[Server]{name: name, registerGRPC: register, registerGateway: gateway}
}

// ServiceRegistrations 是控制面 v0.1 服务面的唯一登记表（保序——表序即
// 生产注册序，见文件头注）。行内注记 = 该服务面的出处与语义要点（收编自
// grpc.go / gateway.go 手写清单的行尾注释）。
var ServiceRegistrations = []ServiceRegistration{
	mountedService("SystemService", serverv1.RegisterSystemServiceServer,
		serverv1.RegisterSystemServiceHandlerFromEndpoint),
	mountedService("AppsService", serverv1.RegisterAppsServiceServer,
		serverv1.RegisterAppsServiceHandlerFromEndpoint),
	mountedService("DeploymentsService", serverv1.RegisterDeploymentsServiceServer,
		serverv1.RegisterDeploymentsServiceHandlerFromEndpoint),
	mountedService("RevisionsService", serverv1.RegisterRevisionsServiceServer,
		serverv1.RegisterRevisionsServiceHandlerFromEndpoint),
	// BuildsService 整体挂 gateway；唯一例外 RPC = BuildFromUpload（IMPL-T2-2/
	// DT-6：client-streaming 无法承载 REST——无 HTTP 注解，REST 面 404 是
	// 契约，见 gateway.go 头注 gRPC-only 清单）。
	mountedService("BuildsService", serverv1.RegisterBuildsServiceServer,
		serverv1.RegisterBuildsServiceHandlerFromEndpoint),
	mountedService("DriftService", serverv1.RegisterDriftServiceServer,
		serverv1.RegisterDriftServiceHandlerFromEndpoint),
	mountedService("DomainsService", serverv1.RegisterDomainsServiceServer,
		serverv1.RegisterDomainsServiceHandlerFromEndpoint),
	mountedService("EnvService", serverv1.RegisterEnvServiceServer,
		serverv1.RegisterEnvServiceHandlerFromEndpoint),
	mountedService("LogsService", serverv1.RegisterLogsServiceServer,
		serverv1.RegisterLogsServiceHandlerFromEndpoint), // Follow = chunked-JSON 流（Console SSE 直接消费）
	mountedService("MetricsService", serverv1.RegisterMetricsServiceServer,
		serverv1.RegisterMetricsServiceHandlerFromEndpoint), // E6 W5-S3：metrics opt-in 面（PromQL 查询/状态/模式切换）
	mountedService("AlertingService", serverv1.RegisterAlertingServiceServer,
		serverv1.RegisterAlertingServiceHandlerFromEndpoint), // B 线 W5-S2：告警面（规则/mode/状态/试跑）
	mountedService("NotificationsService", serverv1.RegisterNotificationsServiceServer,
		serverv1.RegisterNotificationsServiceHandlerFromEndpoint), // E6 W5-S4：通知 Webhook 面（端点/台账/测试）
	// E7 W5-S6：Web 终端受理面（ticket/状态；terminal scope）。WS 数据面
	// 不走 gateway——/v1/terminal 的 GET 是原生 WS 端点（gateway.go 头注
	// 例外清单），gateway 只挂 ticket 受理与状态视图。
	mountedService("ExecService", serverv1.RegisterExecServiceServer,
		serverv1.RegisterExecServiceHandlerFromEndpoint),
	mountedService("EventsService", serverv1.RegisterEventsServiceServer,
		serverv1.RegisterEventsServiceHandlerFromEndpoint), // Watch = chunked-JSON 流（seq 游标 + 过期信封帧）
	mountedService("PlacementService", serverv1.RegisterPlacementServiceServer,
		serverv1.RegisterPlacementServiceHandlerFromEndpoint),
	mountedService("TokensService", serverv1.RegisterTokensServiceServer,
		serverv1.RegisterTokensServiceHandlerFromEndpoint),
	// M4-2：SSH 公钥管理面与 token 管理面同属 Console 消费的 admin 资源面
	//（曾只在 gRPC 侧注册、REST 面 404——补齐对齐）。
	mountedService("GitKeysService", serverv1.RegisterGitKeysServiceServer,
		serverv1.RegisterGitKeysServiceHandlerFromEndpoint),
	// E5 Cron：整体 gRPC-only（手动触发/运行台账走 CLI/gRPC；gateway 无
	// handler 注册——REST 面 404 是契约）。
	grpcOnlyService("CronService", serverv1.RegisterCronServiceServer),
	// E4 W4-S2：库实例资源面（生命周期 RPC；连接投影脱敏）。
	mountedService("DatabaseService", serverv1.RegisterDatabaseServiceServer,
		serverv1.RegisterDatabaseServiceHandlerFromEndpoint),
	// E4 W4-S4：平台密钥库面（D-DB-7；无值读回——list 只出名称/指纹）。
	mountedService("SecretsService", serverv1.RegisterSecretsServiceServer,
		serverv1.RegisterSecretsServiceHandlerFromEndpoint),
	// T 线 OT-3/IMPL-T1-4：明文配置资源面（Get 明文 = admin）。
	mountedService("ConfigsService", serverv1.RegisterConfigsServiceServer,
		serverv1.RegisterConfigsServiceHandlerFromEndpoint),
	// 认证/用户/审计/团队/项目面（v0.3，rbac-teams §2/§5/§6）：认证三方法
	//（Register/Login/GetRegistrationState）在拦截器豁免名单；用户/审计/
	// 团队/项目的角色门与平台管理员判定在 handler 内强制（internal/api
	// 各文件头注）——scope 登记唯一来源 = internal/api/scope.go。
	mountedService("AuthService", serverv1.RegisterAuthServiceServer,
		serverv1.RegisterAuthServiceHandlerFromEndpoint),
	mountedService("UsersService", serverv1.RegisterUsersServiceServer,
		serverv1.RegisterUsersServiceHandlerFromEndpoint),
	mountedService("AuditService", serverv1.RegisterAuditServiceServer,
		serverv1.RegisterAuditServiceHandlerFromEndpoint),
	mountedService("TeamsService", serverv1.RegisterTeamsServiceServer,
		serverv1.RegisterTeamsServiceHandlerFromEndpoint),
	mountedService("ProjectsService", serverv1.RegisterProjectsServiceServer,
		serverv1.RegisterProjectsServiceHandlerFromEndpoint),
	// 程序化动态工作负载面（T 线 DT-5 / IMPL-T2-1）：整体 tasks 独立 scope
	//（scope.go 登记处）；跨令牌隔离与配额在 handler/state 面收口。整体
	// gRPC-only（机具令牌为典型持有者，不走 REST）。
	grpcOnlyService("TasksService", serverv1.RegisterTasksServiceServer),
}

// RegisterGRPCServices 把服务装配集经登记表全量注册到 gRPC registrar
//（grpc.go 生产装配与 internal/apitest 测试装配共用——注册本体单点）。
//
// instances = 服务短名 → 服务实现（生产侧为 wire 供给的实例；测试侧为
// 注入确定性假端口的构造）。表 ⇆ 装配集双向对账：表有条目而装配集缺实例、
// 或装配集有实例而表无条目，都返回错误（构造期红，不静默缺面）。
func RegisterGRPCServices(registrar grpc.ServiceRegistrar, instances map[string]any) error {
	registered := make(map[string]bool, len(ServiceRegistrations))
	for _, entry := range ServiceRegistrations {
		name := entry.serviceName()
		instance, ok := instances[name]
		if !ok {
			return fmt.Errorf("service registration: %s has no assembled instance (assembled set is behind the registration table)", name)
		}
		if err := entry.bindGRPC(registrar, instance); err != nil {
			return err
		}
		registered[name] = true
	}
	for name := range instances {
		if !registered[name] {
			return fmt.Errorf("service registration: assembled instance %s is not in ServiceRegistrations (add a table entry in internal/runtime/registration.go)", name)
		}
	}
	return nil
}

// RegisterGatewayHandlers 按表序把全部双面服务的 REST 反代 handler 挂到
// gateway mux（gateway.go 生产装配单消费点；gRPC-only 条目跳过）。任一
// 挂载失败即返回错误（fail-fast 拒绝半装配的 REST 面）。
func RegisterGatewayHandlers(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) error {
	for _, entry := range ServiceRegistrations {
		if err := entry.mountGateway(ctx, mux, endpoint, opts); err != nil {
			return fmt.Errorf("service registration: mount gateway handler for %s: %w", entry.serviceName(), err)
		}
	}
	return nil
}
