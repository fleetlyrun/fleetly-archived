package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"buf.build/go/protovalidate"
	"github.com/lynx-go/lynx"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	sharedv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/shared/v1"
	"github.com/fleetlyrun/fleetly/internal/api"
)

// NewGRPCServer 创建控制面 gRPC 服务（lynx server/grpc，内置恢复/日志
// 拦截器、grpc.health.v1 与反射）：注册 v0.1 全部服务面（T2.17/T2.20/
// T2.18）。拦截链顺序 = auth（最外）→ rate limit → protovalidate（形状
// 校验，链尾）→ handler；流式（Follow/Watch）走 auth/ratelimit 流式拦截
// 器。服务必须在 Serve 之前注册（lynx Server 在 Start 时才 Serve，构造期
// 注册安全）。
func NewGRPCServer(
	app lynx.App,
	cfg *AppConfig,
	ctl *ControlPlaneTLS,
	auth *api.Authenticator,
	apps *api.AppsService,
	deploys *api.DeploymentsService,
	revisions *api.RevisionsService,
	builds *api.BuildsService,
	drift *api.DriftService,
	domains *api.DomainsService,
	env *api.EnvService,
	logsSvc *api.LogsService,
	metricsSvc *api.MetricsService,
	alertingSvc *api.AlertingService,
	notificationsSvc *api.NotificationsService,
	events *api.EventsService,
	placement *api.PlacementService,
	tokens *api.TokensService,
	gitkeys *api.GitKeysService,
	cronSvc *api.CronService,
	dbs *api.DatabaseService,
	secretsSvc *api.SecretsService,
	configsSvc *api.ConfigsService,
	execSvc *api.ExecService,
	authSvc *api.AuthService,
	usersSvc *api.UsersService,
	auditSvc *api.AuditService,
	teamsSvc *api.TeamsService,
	projectsSvc *api.ProjectsService,
	tasksSvc *api.TasksService,
	sys *api.SystemService,
) (*lynxgrpc.Server, error) {
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("construct protovalidate validator: %w", err)
	}
	// V2-8（E7 同批）：control_plane.tls 非 off 时 gRPC 面 TLS 化——证书经
	// GetCertificate 闭包供给（platform 模式热重载 / manual 装配期加载，
	// 见 tls.go）。off（缺省）不加 TLS 选项 = 今日明文行为逐字不变。
	opts := []lynxgrpc.Option{
		lynxgrpc.WithAddr(cfg.GRPCAddr()),
		lynxgrpc.WithLogger(app.Logger()),
		lynxgrpc.WithHealthCheckers(app.HealthCheckers),
		lynxgrpc.WithInterceptors(
			// 链序 = auth（含 per-token 限流判定，鉴权后语义）→
			// protovalidate（形状校验）→ handler。
			auth.UnaryAuthInterceptor(),
			validateUnaryInterceptor(validator),
		),
		lynxgrpc.WithStreamInterceptors(
			auth.StreamAuthInterceptor(),
		),
	}
	if ctl != nil {
		opts = append(opts, lynxgrpc.WithTLSConfig(ctl.TLSConfig()))
	}
	srv := lynxgrpc.NewServer(opts...)
	g := srv.GetServer()
	serverv1.RegisterSystemServiceServer(g, sys)
	serverv1.RegisterAppsServiceServer(g, apps)
	serverv1.RegisterDeploymentsServiceServer(g, deploys)
	serverv1.RegisterRevisionsServiceServer(g, revisions)
	serverv1.RegisterBuildsServiceServer(g, builds)
	serverv1.RegisterDriftServiceServer(g, drift)
	serverv1.RegisterDomainsServiceServer(g, domains)
	serverv1.RegisterEnvServiceServer(g, env)
	serverv1.RegisterLogsServiceServer(g, logsSvc)
	serverv1.RegisterMetricsServiceServer(g, metricsSvc)             // E6 W5-S3：metrics opt-in 面（查询/状态/模式）
	serverv1.RegisterAlertingServiceServer(g, alertingSvc)           // B 线 W5-S2：告警面（规则/mode/状态/试跑）
	serverv1.RegisterNotificationsServiceServer(g, notificationsSvc) // E6 W5-S4：通知 Webhook 面（端点/台账/测试）
	serverv1.RegisterExecServiceServer(g, execSvc)                   // E7 W5-S6：Web 终端受理面（ticket/状态；terminal scope）
	serverv1.RegisterEventsServiceServer(g, events)
	serverv1.RegisterPlacementServiceServer(g, placement)
	serverv1.RegisterTokensServiceServer(g, tokens)
	serverv1.RegisterGitKeysServiceServer(g, gitkeys)
	serverv1.RegisterCronServiceServer(g, cronSvc)
	serverv1.RegisterDatabaseServiceServer(g, dbs)
	serverv1.RegisterSecretsServiceServer(g, secretsSvc) // E4 W4-S4：平台密钥库面（D-DB-7，无值读回）
	serverv1.RegisterConfigsServiceServer(g, configsSvc) // T 线 OT-3/IMPL-T1-4：明文配置资源面（Get 明文走 admin）
	// 认证/用户面（v0.3 W1，rbac-teams §5）：注册/登录/注册状态三方法在
	// 拦截器豁免名单，Logout/LogoutAll/Me/AcceptInvite = 任意已认证，用户
	// 管理面 = admin scope + handler 内平台管理员判定（internal/api/users.go）。
	serverv1.RegisterAuthServiceServer(g, authSvc)
	serverv1.RegisterUsersServiceServer(g, usersSvc)
	// 审计读面（v0.3 W3-S1，rbac-teams §6 D-W0-6）：平台管理员双门
	//（scope admin + handler 判定——internal/api/audit.go 头注）。
	serverv1.RegisterAuditServiceServer(g, auditSvc)
	// 团队/项目面（v0.3 W2-S1，rbac-teams §5）：角色门在 handler 内强制
	// （机具令牌/非成员 403、平台管理员只读——internal/api/teams.go 头注）。
	serverv1.RegisterTeamsServiceServer(g, teamsSvc)
	serverv1.RegisterProjectsServiceServer(g, projectsSvc)
	// 程序化动态工作负载面（T 线 DT-5 / IMPL-T2-1）：整体 tasks 独立 scope
	//（scope.go 登记处）；跨令牌隔离与配额在 handler/state 面收口。
	serverv1.RegisterTasksServiceServer(g, tasksSvc)
	return srv, nil
}

// rateUnaryInterceptor / rateStreamInterceptor 已裁撤：per-token 限流在
// 鉴权之后才有身份可言，独立拦截器需要二次解析 metadata 与二次认证语义
// ——判定内联在 api.Authenticator.Authenticate（token 校验成功后立即
// 判限），链位注释保留以免后续误挂。

// validateExemptPrefixes 豁免框架服务：health/reflection 请求不带
// buf.validate 规则，跳过以求值零开销（torchwood 同款白名单）。
var validateExemptPrefixes = []string{
	"/grpc.health.v1.",
	"/grpc.reflection.",
}

// validateUnaryInterceptor 按 proto 上的 buf.validate 注解（protovalidate
// CEL 运行时求值）对请求消息做形状校验：形状约束在 proto 声明、在此统一
// 生效；跨字段与业务规则仍留在 app 用例层。仅 unary，与现有服务面一致。
func validateUnaryInterceptor(validator protovalidate.Validator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if validator == nil || validateExempt(info.FullMethod) {
			return handler(ctx, req)
		}
		msg, ok := req.(proto.Message)
		if !ok {
			// 非 proto 消息（理论不可达）不校验，交由后续链路处理。
			return handler(ctx, req)
		}
		if err := validator.Validate(msg); err != nil {
			return nil, validateStatusError(err)
		}
		return handler(ctx, req)
	}
}

// validateStatusError 映射校验错误为带 ErrorResponse 信封 detail 的 status：
//   - 规则违规 → InvalidArgument；不新造错误码（清单外码须 T0.5 冻结），
//     信封 code 留空（gateway 侧按退化信封渲染），违规明细进 message 与
//     context（键 = 字段路径，值 = 规则 ID + 说明）；
//   - CEL 编译/求值故障 → Internal（注解缺陷属服务端 bug，fail-closed
//     拒绝而非放行未校验请求），同样携带信封 detail。
func validateStatusError(err error) error {
	var violations *protovalidate.ValidationError
	if errors.As(err, &violations) {
		parts := make([]string, 0, len(violations.Violations))
		ctx := make(map[string]string, len(violations.Violations))
		for _, violation := range violations.Violations {
			parts = append(parts, violation.String())
			field := ""
			if violation.Proto != nil {
				field = protovalidate.FieldPathString(violation.Proto.GetField())
			}
			if field == "" {
				field = fmt.Sprintf("violation.%d", len(ctx))
			}
			ctx[field] = violation.Proto.GetRuleId() + ": " + violation.Proto.GetMessage()
		}
		return statusWithEnvelope(codes.InvalidArgument, strings.Join(parts, "; "), ctx)
	}
	msg := "request validation rules failed to evaluate: " + err.Error()
	return statusWithEnvelope(codes.Internal, msg, nil)
}

// statusWithEnvelope 构造携带 ErrorResponse detail 的 status（信封 code
// 留空、message 承载文案；detail 附加失败时退化为无 detail status，gateway
// 走退化信封路径）。
func statusWithEnvelope(c codes.Code, message string, context map[string]string) error {
	st := status.New(c, message)
	withDetail, derr := st.WithDetails(&sharedv1.ErrorResponse{Message: message, Context: context})
	if derr != nil {
		return st.Err()
	}
	return withDetail.Err()
}

func validateExempt(fullMethod string) bool {
	for _, prefix := range validateExemptPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return true
		}
	}
	return false
}
