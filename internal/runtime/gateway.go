package runtime

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	sharedv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/shared/v1"
	"github.com/fleetlyrun/fleetly/internal/execrelay"
	"github.com/fleetlyrun/fleetly/internal/gitserver"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
)

// newGatewayMux 构建 grpc-gateway mux：REST /v1/** 反向代理到本进程 gRPC
// （torchwood 同款 FromEndpoint 循环形态）。返回的 mux 直接作为 lynx HTTP
// 服务的根 handler，与 lynxhttp.Server 自挂的 /healthz/** 共存（healthz
// 不经此 mux）。
//
// 错误处理：自定义 HTTPErrorHandler（newGatewayErrorHandler，阶段 3/T0.2
// 落地）——gRPC 错误统一渲染为 fleetly.shared.v1.ErrorResponse 信封
// （snake_case 七字段），与 buf.gen.yaml disable_default_errors=true 对齐
// （默认 rpcStatus 错误体已从 OpenAPI 移除）。
//
// ── gateway 挂载清单（T2.17 纪律：gRPC-only 清单显式维护）────────────────
// 挂载（全部服务，读/写/流一致）：
//   - SystemService（Ping 豁免鉴权；Status/Nodes/Ingress 为 read）
//   - AppsService / DeploymentsService / RevisionsService / BuildsService
//   - DriftService / DomainsService / EnvService / PlacementService
//   - TokensService / GitKeysService（M4-2 补齐：SSH 公钥管理面与 token
//     管理面同属 Console 消费的 admin 资源面，此前只在 gRPC 侧注册，
//     REST 面 404——与新GRPCServer 的注册清单对齐）
//   - LogsService（Follow = chunked-JSON 流；Console SSE 直接消费）
//   - EventsService（Watch = chunked-JSON 流，seq 游标 + 过期信封帧）
//   - ExecService（E7 W5-S6：ticket 受理面 + 状态视图；WS 数据面不走
//     gateway——/v1/terminal 的 GET 是原生 WS 端点，见下方例外清单）
//   - AuthService / UsersService（v0.3 W1，rbac-teams §5：认证面与平台
//     用户管理面；Register/Login/GetRegistrationState 豁免鉴权，会话
//     cookie 经 metadata 透传——见 newGatewayMuxWithTLS 的 matcher 注释）
//   - AuditService（v0.3 W3-S1，rbac-teams §6 D-W0-6：审计读面，平台
//     管理员判定在 handler）
//
// gRPC-only 清单（v0.1 为空；IMPL-T2-2 首次登记）：不挂 gateway 的 RPC 在
// 此显式登记——写操作挂 gateway 供 Console 使用，Follow/Watch 的 JSON 帧
// 形态适宜 REST；二进制/多帧流不适宜 REST 的 RPC 在下方注册清单摘除
// HandlerFromEndpoint 并在 proto 注释同步登记。
//   - `BuildsService.BuildFromUpload`（IMPL-T2-2/DT-6）：client-streaming
//     （上下文 tar 分片）——grpc-gateway 对 client-streaming 只生成「单帧
//     消息」形态，且 REST 入向 body 另有 32MiB 上限（H7），无法承载流式
//     上下文；**不挂 gateway**（无 HTTP 注解，REST 面 404 是契约；CLI/SDK
//     消费）。其余 BuildsService 方法（TriggerBuild/GetBuild/ListBuilds）
//     照常挂载。
//
// ── 原生端点例外清单（T2.19 起；torchwood 同纪律：gateway 上非 proto
// 派生的 HTTP 端点在此显式登记，禁止在别处悄悄挂载）────────────────────
//   - POST /v1/apps/{app}/webhooks/github —— GitHub push 投递（T2.19）；
//     投递体是 JSON 不是 proto，验签必须对原始字节做，无法走 gateway 反代
//     形态（gRPC 侧收到的 body 已被 protojson 重组）。签名即认证：该路径
//     前缀精确豁免 Bearer 拦截（豁免面 = webhookPathPattern，绝未放宽到
//     其他路径——非 webhook 路径无 token 仍 401，gateway_rest_test 钉死）。
//   - POST /v1/apps/{app}/webhooks/gitea  —— Gitea push 投递（T2.19），
//     同上。
//   - GET /ui/** —— Console 端静态托管（T2.21，console.static_dir 指向
//     构建产物目录时启用；缺省空 = 不挂载）。静态资源不要求 token：SPA
//     无服务端会话，鉴权语义在其数据面（REST /v1 全部走 Bearer）。豁免
//     精确到 /ui/ 前缀（实现 = console_static.go，分派面 = 豁免面），
//     非 /ui/ 路径无 token 仍 401（gateway_console_test 钉死）。
//   - GET / —— 根路径引导页（landing.go）：裸访问控制面端口给出入口
//     指引（Console / REST / healthz），替代 grpc-gateway 的 404 JSON。
//     纯静态、无依赖、不读任何存储；豁免精确到 GET/HEAD 的根路径（其余
//     方法与其余路径原样进 gateway mux）。
//   - GET /internal/exec-relay 与 GET /v1/terminal —— Web 终端 WS 升级
//     （E7 W5-S6，internal/execrelay NativeHandler 同一 handler 分派两个
//     精确路径）：前者 = relay 反向常连（集群 token Bearer），后者 = 浏览
//     器接入（一次性 ticket——长效 token 不进 URL）。WS 二进制帧不是
//     proto/JSON，无法走 gateway 反代形态；豁免精确到两条 GET 精确路径
//     （分派面 = 豁免面；POST /v1/terminal/tickets 仍走 gateway → gRPC
//     拦截器链的 terminal scope）。
//   - POST /internal/alerts —— 平台内建告警接收器（B 线 W5-S2，D-V3W5-1，
//     alerts_receiver.go）：Alertmanager v2 webhook 载荷不是 proto/JSON
//     契约面，鉴权 = ingress token 同源静态凭据（Bearer/Basic 双形态），
//     与 gateway 鉴权体系完全不同；豁免精确到一条 POST 精确路径（分派面 =
//     豁免面）。
//
// 实现：newRootHandler 先按精确路径形态分派 webhook 与 /ui/ 静态 handler
// （console handler 未启用时为 nil——分派跳过）、终端 native 端点（terminal
// handler 未启用时为 nil）、告警接收器（alerts handler 未启用时为 nil），
// 其余一律交回 grpc-gateway mux。
func newGatewayMux(grpcEndpoint string) (http.Handler, error) {
	return newGatewayMuxWithTLS(grpcEndpoint, nil)
}

// newGatewayMuxWithTLS 是 newGatewayMux 的 TLS 形态（W5-S5 缺陷修复，
// 2026-09-22 staging 门上实证）：control_plane.tls 开启时 gRPC 面对 TLS，
// 回拨必须用 TLS 凭据——原明文 insecure 回拨在 TLS 形态下全量 REST /v1
// 断（「error reading server preface」——e2e 只测了 CLI 直连与 native
// 端点，漏了回拨路径，真机补获）。tlsCfg 仅限**本进程回环自拨**：
// InsecureSkipVerify 的信任锚是进程自身（外部面的 TLS 由监听器强制），
// 对自己的 8421 做完整校验需要 CA 链装载，属自证循环——诚实跳过并在此
// 注明；transport 加密本身照常生效。
//
// 返回的 handler 在 mux 外包一层 sanitizeForwardedFor（W3-S4，XFF 覆写
// 收口——见该函数注释）：生产装配（provides.go newHTTPServices）与测试
// （newGatewayMux 原样复用）走的同一构造，信任边界不存在旁路形态。
func newGatewayMuxWithTLS(grpcEndpoint string, tlsCfg *tls.Config) (http.Handler, error) {
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, newJSONMarshaler()),
		runtime.WithMarshalerOption("*/*", newJSONMarshaler()),
		runtime.WithMarshalerOption("application/json", newJSONMarshaler()),
		runtime.WithErrorHandler(newGatewayErrorHandler()),
		// 会话 cookie 透传（v0.3 W1，rbac-teams §2.2）：gRPC 服务经响应
		// header metadata（键 "set-cookie"，internal/api/authservice.go）
		// 下发 Set-Cookie——缺省 OutgoingHeaderMatcher 给一切键加
		// "Grpc-Metadata-" 前缀，浏览器不识别；此处对 set-cookie 显式
		// 还原为 HTTP 头原名（其余键维持缺省前缀形态不变）。入向 Cookie
		// 头经缺省 HeaderMatcher 以 "grpcgateway-cookie" 键进 metadata，
		// 认证分支消费（internal/api/auth.go cookieFromMetadata），零配置。
		runtime.WithOutgoingHeaderMatcher(outgoingHeaderMatcher),
	)
	var opts []grpc.DialOption
	if tlsCfg != nil {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}
	} else {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	for _, register := range []func(context.Context, *runtime.ServeMux, string, []grpc.DialOption) error{
		serverv1.RegisterSystemServiceHandlerFromEndpoint,
		serverv1.RegisterAppsServiceHandlerFromEndpoint,
		serverv1.RegisterDeploymentsServiceHandlerFromEndpoint,
		serverv1.RegisterRevisionsServiceHandlerFromEndpoint,
		serverv1.RegisterBuildsServiceHandlerFromEndpoint,
		serverv1.RegisterDriftServiceHandlerFromEndpoint,
		serverv1.RegisterDomainsServiceHandlerFromEndpoint,
		serverv1.RegisterEnvServiceHandlerFromEndpoint,
		serverv1.RegisterLogsServiceHandlerFromEndpoint,
		serverv1.RegisterMetricsServiceHandlerFromEndpoint,       // E6 W5-S3：metrics opt-in 面（PromQL 查询/状态/模式切换）
		serverv1.RegisterAlertingServiceHandlerFromEndpoint,      // B 线 W5-S2：告警面（规则/mode/状态/试跑）
		serverv1.RegisterNotificationsServiceHandlerFromEndpoint, // E6 W5-S4：通知 Webhook 面（端点/台账/测试）
		serverv1.RegisterExecServiceHandlerFromEndpoint,          // E7 W5-S6：Web 终端受理面（ticket/状态；WS 数据面走原生端点）
		serverv1.RegisterEventsServiceHandlerFromEndpoint,
		serverv1.RegisterPlacementServiceHandlerFromEndpoint,
		serverv1.RegisterTokensServiceHandlerFromEndpoint,
		serverv1.RegisterGitKeysServiceHandlerFromEndpoint,  // M4-2：与 gRPC 侧注册清单对齐
		serverv1.RegisterDatabaseServiceHandlerFromEndpoint, // E4 W4-S2：库实例资源面（生命周期 RPC；连接投影脱敏）
		serverv1.RegisterSecretsServiceHandlerFromEndpoint,  // E4 W4-S4：平台密钥库面（D-DB-7；无值读回——list 只出名称/指纹）
		serverv1.RegisterConfigsServiceHandlerFromEndpoint,  // T 线 OT-3/IMPL-T1-4：明文配置资源面（Get 明文 = admin）
		serverv1.RegisterAuthServiceHandlerFromEndpoint,     // v0.3 W1：认证面（注册/登录/会话；Register/Login/GetRegistrationState 豁免鉴权）
		serverv1.RegisterUsersServiceHandlerFromEndpoint,    // v0.3 W1：平台用户管理面（平台管理员判定在 handler）
		serverv1.RegisterAuditServiceHandlerFromEndpoint,    // v0.3 W3-S1：审计读面（平台管理员判定在 handler——D-W0-6）
		serverv1.RegisterTeamsServiceHandlerFromEndpoint,    // v0.3 W2-S1：团队/成员/邀请面（角色门在 handler）
		serverv1.RegisterProjectsServiceHandlerFromEndpoint, // v0.3 W2-S1：项目/队内覆写成员面（覆写管理权判定在 handler）
	} {
		if err := register(context.Background(), mux, grpcEndpoint, opts); err != nil {
			return nil, err
		}
	}
	return sanitizeForwardedFor(mux), nil
}

// sanitizeForwardedFor 是 gateway 的入向信任边界（W3-S4，W1-S2 披露的
// XFF 覆写挂账收口）：控制面自身就是反代边界——入向 X-Forwarded-For 头
// 不可信（直连方可伪造链首值，使 api 面 IP 键限流按任意自报地址分桶）。
// 此处删除客户端携带的 XFF 头；grpc-gateway v2 的 annotateContext 随后把
// 真实 TCP 对端地址（RemoteAddr host）并入 x-forwarded-for metadata
// （v2.30 语义：client 头拼接链尾再追加 RemoteAddr——client 侧已删，故
// metadata 恰为单一真实 peer 值）。api 侧（internal/api/authservice.go
// clientIPFromContext）只在 gRPC 对端为环回时采信该 metadata——环回 =
// gateway 回拨（gateway 与 gRPC 同进程同主机），远程直连 gRPC 方自带的
// x-forwarded-for metadata 一律不采信。
//
// 上游可信代理形态（外置 TLS 反代在 gateway 之前）如实披露：本层把代理
// 注入的 XFF 一并清除，全量客户端共享代理地址的限流桶——email 键限流
// 不受影响，主防线在；按代理放行（trusted_proxies 类开关）属配置面扩展
// 点，本票不发明（runbook 披露面）。
func sanitizeForwardedFor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("X-Forwarded-For")
		next.ServeHTTP(w, r)
	})
}

// outgoingHeaderMatcher 是 gateway 的出向 header 映射：set-cookie 还原为
// HTTP 原名（会话 cookie 下发，v0.3 W1——见 newGatewayMuxWithTLS 注释），
// 其余键维持缺省 "Grpc-Metadata-" 前缀形态（DefaultHeaderMatcher 同语义，
// 手写以避免对该缺省函数的隐式依赖漂移）。
func outgoingHeaderMatcher(key string) (string, bool) {
	if strings.EqualFold(key, "set-cookie") {
		return "Set-Cookie", true
	}
	return runtime.MetadataHeaderPrefix + key, true
}

// newJSONMarshaler 是 gateway 的 JSON marshaler：UseProtoNames 使字段名按
// proto 声明输出（snake_case，与 buf.gen.yaml 的 json_names_for_fields=false
// 及 swagger 声明一致）；EmitUnpopulated=false 时零值字段不输出（torchwood
// CustomMarshaler 同款语义）；DiscardUnknown 兼容客户端多发字段。
func newJSONMarshaler() *runtime.JSONPb {
	return &runtime.JSONPb{
		MarshalOptions: protojson.MarshalOptions{
			UseProtoNames:   true,
			EmitUnpopulated: false,
		},
		UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
	}
}

// grpcEndpointFromAddr 从 grpc.addr 推导 gateway 转发目标（torchwood 同款）：
// 保留原主机（默认回环），仅补齐缺失的 host/端口默认值。
func grpcEndpointFromAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		if addr != "" && !strings.HasPrefix(addr, ":") {
			return addr
		}
		return defaultGRPCAddr
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// newRootHandler 组装 HTTP 面根 handler：webhook 原生端点优先精确分派，
// 其次 GET/HEAD 的根路径引导页（landing——无配置依赖，恒挂载）、/ui/ 前缀
// 的 Console 静态托管（consoleUI 为 nil 时跳过——静态托管关闭形态），两者
// 路径形态不匹配的请求原样交回 grpc-gateway mux。各豁免面 = 各自分派面
// （线性词形判定，不存在「先豁免再分发」的放宽空间）。
// REST 面（fallback）外包 A3 匿名 401 per-IP 限速（newAuthFailureLimiter
// ——仅 gateway 面；webhook 与 /ui/ 分派不经限速层，不受影响）。
// H7：全根请求体上限中间件（limitRequestBody）最外层先行——鉴权与
// gateway 解码之前拒绝超限物化（见 maxRequestBodyBytes）。
func newRootHandler(webhook http.Handler, consoleUI http.Handler, terminal http.Handler, alerts http.Handler, fallback http.Handler) http.Handler {
	gateway := newAuthFailureLimiter(time.Minute, 10).wrap(fallback)
	landing := newLandingHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// H7：请求体上限在分派（webhook/console/terminal/gateway）之前执行
		// ——gateway 解码完 body 才到 gRPC 鉴权拦截器的旧形态下，未认证方
		// 可物化任意大内存；上限前置后 413 早于一切 body 消费。
		if !limitRequestBody(w, r) {
			return
		}
		if gitserver.WebhookPathPattern.MatchString(r.URL.Path) {
			webhook.ServeHTTP(w, r)
			return
		}
		// 引导页：分派面 = 豁免面 = 精确 GET/HEAD /（HEAD body 由 net/http
		// 自行抑制，handler 不感知）。
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			landing.ServeHTTP(w, r)
			return
		}
		if consoleUI != nil && (r.URL.Path == consoleUIPathPrefix || strings.HasPrefix(r.URL.Path, consoleUIPathPrefix+"/")) {
			consoleUI.ServeHTTP(w, r)
			return
		}
		// Web 终端 native WS 端点（E7 W5-S6）：handler 内部按两条精确 GET
		// 路径分派，其余路径 404 原样交回 gateway（nil = 未装配——永不分派）。
		if terminal != nil && (r.URL.Path == execrelay.RelayPath || r.URL.Path == execrelay.TerminalWSPath) {
			terminal.ServeHTTP(w, r)
			return
		}
		// 平台内建告警接收器（W5-S2）：POST 分派（nil = 未装配）。精确路径
		// + vmalert 追加形态两个入口——W5-S4 门上修复（e2e/notifications.sh
		// NOT-A6 实证补获）：vmalert 把 -notifier.url 当 Alertmanager base
		// URL，恒以 <url>/api/v2/alerts 投递；仅收精确路径时投递全数 404
		// （vmalert 侧按 notifier failure 退避重试）。
		if alerts != nil && r.Method == http.MethodPost &&
			(r.URL.Path == internalAlertsPath || strings.HasPrefix(r.URL.Path, internalAlertsPath+"/")) {
			alerts.ServeHTTP(w, r)
			return
		}
		gateway.ServeHTTP(w, r)
	})
}

// ── H7（S18）：REST/HTTP 面请求体上限 ────────────────────────────────────────
//
// 问题：gateway 面的 REST 请求体没有上限——grpc-gateway 解码完整 body 后
// 才进入 gRPC auth 拦截器，未认证方可让进程物化数 GB 内存（webhook 原生
// 端点自带 25MiB LimitReader，见 gitserver maxWebhookBody；gRPC 直连面有
// 框架缺省 4MiB recv 上限；唯独 gateway REST 面裸奔）。
//
// 防线形态（root handler 最外层，先于分派与鉴权）：
//   - 声明了 Content-Length 且超限 → 直接 413 信封（不触达任何 handler，
//     body 不读——连接由调用方自裁）；
//   - 其余请求统一包 http.MaxBytesReader（32MiB）：谎报/缺省长度
//     （chunked）的慢速物化在读第 N+1 字节时被截断，MaxBytesReader 同步
//     关闭连接（服务端不再排空剩余 body），内存物化以常数为上界。
//
// 上限取 32MiB：口径对齐 webhook 面 25MiB（GitHub push 大投递防御）取整档
// 上浮一档——REST 面载荷是 JSON 包裹的 compose 字节（Deploy/TriggerBuild），
// 与 webhook 同源同量级；32MiB 覆盖最大合法 compose 同时把未认证物化面
// 压到常数。/ui/ 静态与 webhook 面统一包裹（GET 无 body 无害；webhook
// 自身 25MiB LimitReader 先于此层生效，双层取小不冲突）。
const maxRequestBodyBytes = 32 << 20 // 32MiB

// limitRequestBody 是 H7 的入向守卫：超限返回 false（413 信封已写，调用方
// 停止分派）；未超限把 r.Body 包上 MaxBytesReader 后返回 true。
// 413 信封用 shared ErrorResponse 的 protojson 形态（与 webhook reject、
// gateway 错误处理器同款退化信封——code 留空，message 保底）。
func limitRequestBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength > maxRequestBodyBytes {
		writeRequestBodyTooLarge(w)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	return true
}

// writeRequestBodyTooLarge 输出 413 退化信封（与 FZ-2 口径一致：code 空、
// message 保底，无内部细节）。
func writeRequestBodyTooLarge(w http.ResponseWriter) {
	env := &sharedv1.ErrorResponse{Message: "request body exceeds limit (32MiB)"}
	raw, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(env)
	if err != nil {
		http.Error(w, "request body exceeds limit", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_, _ = w.Write(raw) //nolint:gosec // G705：protojson 固定信封，无用户可控标记
}

// ── A3（S18）：匿名 401 per-IP 限速 ─────────────────────────────────────────
//
// 问题：REST 面匿名请求（无 token / 错 token）没有 IP 维度限速——每个
// 401 都伴随一次 tokens 表哈希查询，匿名扫描可制造 DB 查询风暴。gRPC
// 直连面拿不到对端 IP，维持现状（token 熵防线：flt_ 前缀 + 192 bit 随机
// 的暴力空间）；本层仅 REST gateway 面生效（RemoteAddr host 可得）。
//
// 算法：固定窗失败桶——对最终 status 401 的响应按 RemoteAddr host 计数，
// 单 IP 单窗口（1 分钟）内第 10 次 401 后，该 IP 的后续请求直接 429
// （不再触达后端）。内存有界：单 map + mutex + 惰性过期（map 超阈值时
// 清除已出窗条目；键空间 = 触发过 401 的对端 IP 数，远小于任意请求集）。
// webhook 端点不在本层覆盖面（错签名 401 走原生分派，非鉴权失败路径，
// 按设计不受影响）；带有效 token 的请求不产生 401 计数，不受影响。
type authFailureLimiter struct {
	mu      sync.Mutex
	buckets map[string]*failBucket
	window  time.Duration
	limit   int
	now     func() time.Time
}

// failBucket 是单 IP 的固定窗计数。
type failBucket struct {
	count int
	start time.Time
}

// authFailureMaxIPs 是惰性过期阈值（触发过 401 的 distinct IP 上界）。
const authFailureMaxIPs = 10000

// newAuthFailureLimiter 构造限速器（window/limit 生产取 1min/10）。
func newAuthFailureLimiter(window time.Duration, limit int) *authFailureLimiter {
	return &authFailureLimiter{
		buckets: make(map[string]*failBucket),
		window:  window,
		limit:   limit,
		now:     time.Now,
	}
}

// wrap 把限速中间件包在 next 之外：入向先判该 IP 是否已被封禁（窗口内
// 401 计数达上限），出向用状态记录器观测最终 status——401 计数、其余
// 放行。
func (l *authFailureLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r.RemoteAddr)
		if l.blocked(ip) {
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusUnauthorized {
			l.recordFailure(ip)
		}
	})
}

// blocked 报告该 IP 是否仍处于封禁窗内（计数达上限且窗口未过期）。
func (l *authFailureLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		return false
	}
	if l.now().Sub(b.start) >= l.window {
		return false // 窗口已过期：放行（下次 401 时重开窗口）
	}
	return b.count >= l.limit
}

// recordFailure 记一次 401：出窗重开计数，在窗内累加；map 超阈值时惰性
// 清除全部已出窗条目（内存有界）。
func (l *authFailureLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) >= authFailureMaxIPs {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[ip]
	if !ok || now.Sub(b.start) >= l.window {
		l.buckets[ip] = &failBucket{count: 1, start: now}
		return
	}
	b.count++
}

// remoteIP 从 RemoteAddr 取 host（去掉端口；IPv6 字面量形态由
// SplitHostPort 正确处理）。拿不到对端 IP 的形态返回原串（计数键退化为
// 该串，不影响有界性）。
func remoteIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// statusRecorder 记录最终响应 status 的 ResponseWriter 包装（A3 观测面；
// Flush 透传——gateway 流式端点（Watch/Follow 的 chunked-JSON）依赖）。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Flush 透传底层 Flusher（流式响应不被缓冲截断）。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
