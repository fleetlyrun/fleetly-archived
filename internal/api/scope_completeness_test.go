package api

// scope 登记表完整性守卫（IMPL-ARCH-D，W5 收官）：descriptor-walk 三向
// 完备性测试。
//
// 背景：methodScopes（scope.go）是 gRPC 方法级 scope 的唯一登记点，auth
// 拦截器消费；未登记方法走拦截器 fail-closed 默认（按 admin 拒绝，见
// auth.go）——安全但不可见。历史上 MoveApp/MoveDatabase 确实漏登过
//（scope.go 资源改派注记自述），当时静默降级 admin-only 且无任何测试
// 兜底；W5 下一波 E2 MCP 将成打新增 RPC，同类漏登会复发。
//
// 本测试用 proto descriptor 枚举全部 fleetly.* gRPC 方法（方法全集读自
// protoregistry.GlobalFiles——测试二进制链接的 genproto/fleetly/server/v1
// 包在 init 时注册全部文件描述符；不手抄方法清单，proto 增删方法测试
// 自动跟随），三向钉死：
//
//  ① descriptor 每个方法要么在 methodScopes、要么在本测试的显式豁免清单
//     （缺登记即红，输出点名方法）；
//  ② methodScopes 无幻影键（方法改名/删除后登记残留即红）；
//  ③ 豁免清单同样无幻影键；
//  ④ 豁免清单与 authExemptMethods（auth.go）完全一致——豁免的唯一正当
//     来源是「拦截器本就不做 scope 鉴权」：若某方法进了本测试的豁免清单
//     却不在 authExemptMethods，运行时会走 fail-closed 静默降级为
//     admin-only（正是本票要治的病，且①③④全测不出来），故双向钉死。
//     authExemptPrefixes（/grpc.health.v1.、/grpc.reflection.）在 fleetly.*
//     命名空间之外，descriptor-walk 天然覆盖不到，与本清单无交集。
//
// 纪律对应 scope.go 头注：新增 RPC 必须在 methodScopes 登记；fail-closed
// 默认是兜底不是豁免，「懒得登记」不是入豁免清单的理由。

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// scopeExemptMethods 显式豁免清单（测试内维护，每条一行理由）：只收
// 「scope 语义确实不适用」的方法——即拦截器在 scope 判定之前就放行的
// 方法（authExemptMethods）。豁免即公开面，理由必须成立。
var scopeExemptMethods = map[string]string{
	// 无鉴权存活探针（T2.17 契约：Ping 与 healthz 豁免鉴权，登录前可达）。
	"/fleetly.server.v1.SystemService/Ping": "无鉴权存活探针（T2.17 契约：Ping 与 healthz 同豁免，登录前可达）",
	// 认证三面（v0.3 W1，rbac-teams §2.2）：无凭据可用的登录前端点。
	"/fleetly.server.v1.AuthService/Register":             "注册端点——无凭据可用（登录页开关注册入口）",
	"/fleetly.server.v1.AuthService/Login":                "登录端点——无凭据可用（认证凭据的颁发面）",
	"/fleetly.server.v1.AuthService/GetRegistrationState": "驱动登录页注册入口——无凭据可用（注册开关的只读探测）",
}

// TestMethodScopeRegistryCoversDescriptor 三向完备性主测试（分四子测试，
// 各方向独立红，失败输出点名具体方法）。
func TestMethodScopeRegistryCoversDescriptor(t *testing.T) {
	descriptorMethods := map[string]bool{}
	fileCount := 0
	protoregistry.GlobalFiles.RangeFiles(func(fileDescriptor protoreflect.FileDescriptor) bool {
		// 只收 fleetly.* 命名空间（client/console 两包无 service、shared
		// 只有消息——实际来源即 fleetly.server.v1 的全部服务；grpc.health/
		// reflection 等框架服务不在此命名空间，不进本测试的视野）。
		if !strings.HasPrefix(string(fileDescriptor.Package()), "fleetly.") {
			return true
		}
		fileCount++
		services := fileDescriptor.Services()
		for i := 0; i < services.Len(); i++ {
			service := services.Get(i)
			methods := service.Methods()
			for j := 0; j < methods.Len(); j++ {
				method := methods.Get(j)
				// gRPC FullMethod 形如 /<package>.<Service>/<Method>——
				// service.FullName() 已含 proto 包前缀。
				fullMethod := "/" + string(service.FullName()) + "/" + string(method.Name())
				descriptorMethods[fullMethod] = true
			}
		}
		return true
	})
	// 防空转：descriptor-walk 一无所获时（描述符未链接进测试二进制之类的
	// 环境回归）直接 fatal——否则三向断言全部空集恒真，守卫形同虚设。
	if fileCount == 0 || len(descriptorMethods) == 0 {
		t.Fatalf("descriptor walk found no fleetly.* services (%d files, %d methods) — genproto file descriptors are not linked into the test binary, the completeness assertions would pass vacuously",
			fileCount, len(descriptorMethods))
	}
	t.Logf("descriptor walk: %d fleetly.* proto file(s), %d gRPC method(s), %d registered in methodScopes, %d exempted",
		fileCount, len(descriptorMethods), len(methodScopes), len(scopeExemptMethods))

	t.Run("every descriptor method is registered or explicitly exempt", func(t *testing.T) {
		var missing []string
		for fullMethod := range descriptorMethods {
			if _, registered := methodScopes[fullMethod]; registered {
				continue
			}
			if _, exempt := scopeExemptMethods[fullMethod]; exempt {
				continue
			}
			missing = append(missing, fullMethod)
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("%d gRPC method(s) missing from methodScopes (unregistered RPCs silently degrade to admin-only via the fail-closed default):\n%s\n→ register each in scope.go methodScopes, or add it to scopeExemptMethods in this test with a one-line reason if scope semantics genuinely do not apply",
				len(missing), bulletList(missing))
		}
	})

	t.Run("methodScopes has no phantom keys", func(t *testing.T) {
		var phantom []string
		for fullMethod := range methodScopes {
			if !descriptorMethods[fullMethod] {
				phantom = append(phantom, fullMethod)
			}
		}
		if len(phantom) > 0 {
			sort.Strings(phantom)
			t.Errorf("%d methodScopes key(s) not found in proto descriptors (stale registrations left behind by renamed/deleted RPCs):\n%s\n→ remove the stale entries from scope.go methodScopes",
				len(phantom), bulletList(phantom))
		}
	})

	t.Run("exempt list has no phantom keys", func(t *testing.T) {
		var phantom []string
		for fullMethod := range scopeExemptMethods {
			if !descriptorMethods[fullMethod] {
				phantom = append(phantom, fullMethod)
			}
		}
		if len(phantom) > 0 {
			sort.Strings(phantom)
			t.Errorf("%d scopeExemptMethods key(s) not found in proto descriptors (stale exemptions left behind by renamed/deleted RPCs):\n%s\n→ remove the stale entries from scopeExemptMethods in this test",
				len(phantom), bulletList(phantom))
		}
	})

	t.Run("exempt list matches authExemptMethods", func(t *testing.T) {
		var unexempted, unauthExempt []string
		// authExemptMethods 有而豁免清单没有：新的鉴权豁免面没进本测试的
		// 视野（豁免面扩大必须过本测试的理由关）。
		for fullMethod := range authExemptMethods {
			if _, exempt := scopeExemptMethods[fullMethod]; !exempt {
				unexempted = append(unexempted, fullMethod)
			}
		}
		// 豁免清单有而 authExemptMethods 没有：运行时该方法并不豁免鉴权，
		// 未登记 scope 会被 fail-closed 静默降级为 admin-only——本票要治
		// 的病在测试侧的替身形态。
		for fullMethod := range scopeExemptMethods {
			if !authExemptMethods[fullMethod] {
				unauthExempt = append(unauthExempt, fullMethod)
			}
		}
		if len(unexempted) > 0 {
			sort.Strings(unexempted)
			t.Errorf("%d authExemptMethods entry(ies) missing from scopeExemptMethods:\n%s\n→ add each to scopeExemptMethods in this test with a one-line reason (the auth-exempt face is out of scope semantics by construction)",
				len(unexempted), bulletList(unexempted))
		}
		if len(unauthExempt) > 0 {
			sort.Strings(unauthExempt)
			t.Errorf("%d scopeExemptMethods entry(ies) not in authExemptMethods (at runtime these are NOT auth-exempt; an unregistered scope would silently degrade to admin-only):\n%s\n→ either register them in scope.go methodScopes, or add them to authExemptMethods in auth.go if they genuinely need no authentication",
				len(unauthExempt), bulletList(unauthExempt))
		}
	})
}

// bulletList 把方法名列表格式化为可操作的多行输出（每行一个方法，带
// 缩进符号——失败信息直接点名，不用翻 map）。
func bulletList(methods []string) string {
	var b strings.Builder
	for _, fullMethod := range methods {
		fmt.Fprintf(&b, "  - %s\n", fullMethod)
	}
	return strings.TrimRight(b.String(), "\n")
}
