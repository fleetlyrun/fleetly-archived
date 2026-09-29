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

	sharedv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/shared/v1"
	"github.com/fleetlyrun/fleetly/internal/api"
)

// NewGRPCServer 创建控制面 gRPC 服务（lynx server/grpc，内置恢复/日志
// 拦截器、grpc.health.v1 与反射）：注册 v0.1 全部服务面（T2.17/T2.20/
// T2.18）。拦截链顺序 = auth（最外）→ rate limit → protovalidate（形状
// 校验，链尾）→ handler；流式（Follow/Watch）走 auth/ratelimit 流式拦截
// 器。服务必须在 Serve 之前注册（lynx Server 在 Start 时才 Serve，构造期
// 注册安全）。
//
// 服务面清单 = registration.go 的 ServiceRegistrations（唯一登记表，
// IMPL-ARCH-I）：本函数只把 wire 供给的实例按名对位成装配集，注册本体
// （顺序/register 函数）在表单点；表 ⇆ 装配集双向对账缺一即红。
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
	runtimeSvc *api.RuntimeService,
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
	// 服务装配集（键 = 登记表的服务短名；值 = wire 供给的生产实例）。
	// 注册顺序、register 函数与 gateway 挂载一律以登记表为准——此处不是
	// 第三份清单，只做名对位（漏传参/多传参在对账处红，见
	// RegisterGRPCServices）。
	instances := map[string]any{
		"SystemService":        sys,
		"AppsService":          apps,
		"DeploymentsService":   deploys,
		"RevisionsService":     revisions,
		"BuildsService":        builds,
		"DriftService":         drift,
		"RuntimeService":       runtimeSvc,
		"DomainsService":       domains,
		"EnvService":           env,
		"LogsService":          logsSvc,
		"MetricsService":       metricsSvc,
		"AlertingService":      alertingSvc,
		"NotificationsService": notificationsSvc,
		"ExecService":          execSvc,
		"EventsService":        events,
		"PlacementService":     placement,
		"TokensService":        tokens,
		"GitKeysService":       gitkeys,
		"CronService":          cronSvc,
		"DatabaseService":      dbs,
		"SecretsService":       secretsSvc,
		"ConfigsService":       configsSvc,
		"AuthService":          authSvc,
		"UsersService":         usersSvc,
		"AuditService":         auditSvc,
		"TeamsService":         teamsSvc,
		"ProjectsService":      projectsSvc,
		"TasksService":         tasksSvc,
	}
	if err := RegisterGRPCServices(g, instances); err != nil {
		return nil, err
	}
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
