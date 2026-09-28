package api

// scope 登记完整性守卫（IMPL-ARCH-D 建立四向 descriptor-walk；IMPL-ARCH-J
// 登记面搬家后语义适配）。
//
// 背景：scope 的唯一登记点 = proto 注解——每个 RPC 以
// option (fleetly.annotations.v1.scope) = "<词>" 登记（定义见
// proto/fleetly/annotations/v1/annotations.proto）；scope.go 的运行时 map
// 由包初始化期 descriptor walk 生成，不再有第二份手写方法清单可对账，守卫
// 因此直接对 descriptor 本身与 auth 豁免表钉死。未注解方法走拦截器
// fail-closed 默认（按 admin 拒绝，见 auth.go）——安全但不可见。历史上
// MoveApp/MoveDatabase 确实漏登过（迁注解前的 scope.go 资源改派注记自述），
// 当时静默降级 admin-only 且无任何测试兜底；W5 下一波 E2 MCP 将成打新增
// RPC，同类漏登会复发。
//
// 本测试用 proto descriptor 枚举全部 fleetly.* gRPC 方法（方法全集读自
// protoregistry.GlobalFiles——测试二进制链接的 genproto/fleetly/server/v1
// 包在 init 时注册全部文件描述符；不手抄方法清单，proto 增删方法测试
// 自动跟随），四向钉死：
//
//  ① descriptor 每个方法要么带 scope 注解、要么 ∈ authExemptMethods
//     （缺注解即红，输出点名方法）；豁免面带注解同样红——豁免语义不进
//     proto，双登记即语义冲突；
//  ② 带注解的值 ∈ scopeWords 词表（未知词红；启动期 init 同样 fail-fast
//     ——双保险，测试拦提交期、init 拦运行期）；
//  ③ authExemptMethods 无幻影键（方法改名/删除后豁免残留即红）；
//  ④ 本测试的豁免清单（逐条理由）≡ authExemptMethods（auth.go 生产豁免
//     表）双向相等——豁免的唯一正当来源是「拦截器本就不做 scope 鉴权」：
//     若某方法进了本测试的豁免清单却不在 authExemptMethods，运行时该方法
//     无注解会被 fail-closed 静默降级为 admin-only（正是本票要治的病，且
//     ①③④全测不出来），故双向钉死。authExemptPrefixes（/grpc.health.v1.、
//     /grpc.reflection.）在 fleetly.* 命名空间之外，descriptor-walk 天然
//     覆盖不到，与本清单无交集。
//
// 纪律对应 annotations.proto 头注：新增 RPC 必须在 proto 注解 scope；
// fail-closed 默认是兜底不是豁免，「懒得登记」不是入豁免清单的理由。

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	annotationsv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/annotations/v1"
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

// TestMethodScopeRegistryCoversDescriptor 四向完备性主测试（分四子测试，
// 各方向独立红，失败输出点名具体方法）。
func TestMethodScopeRegistryCoversDescriptor(t *testing.T) {
	type methodScope struct {
		fullMethod string
		scope      string // 带注解时的注解值；不带注解为空
		annotated  bool
	}
	var descriptorMethods []methodScope
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
				entry := methodScope{
					fullMethod: "/" + string(service.FullName()) + "/" + string(method.Name()),
				}
				opts, ok := method.Options().(*descriptorpb.MethodOptions)
				if ok && opts != nil && proto.HasExtension(opts, annotationsv1.E_Scope) {
					entry.annotated = true
					entry.scope = proto.GetExtension(opts, annotationsv1.E_Scope).(string)
				}
				descriptorMethods = append(descriptorMethods, entry)
			}
		}
		return true
	})
	// 防空转：descriptor-walk 一无所获时（描述符未链接进测试二进制之类的
	// 环境回归）直接 fatal——否则四向断言全部空集恒真，守卫形同虚设。
	if fileCount == 0 || len(descriptorMethods) == 0 {
		t.Fatalf("descriptor walk found no fleetly.* services (%d files, %d methods) — genproto file descriptors are not linked into the test binary, the completeness assertions would pass vacuously",
			fileCount, len(descriptorMethods))
	}
	annotatedCount := 0
	for _, m := range descriptorMethods {
		if m.annotated {
			annotatedCount++
		}
	}
	t.Logf("descriptor walk: %d fleetly.* proto file(s), %d gRPC method(s), %d with scope annotation, %d auth-exempt",
		fileCount, len(descriptorMethods), annotatedCount, len(authExemptMethods))

	t.Run("every descriptor method carries a scope option or is auth-exempt", func(t *testing.T) {
		var missing, annotatedExempt []string
		for _, m := range descriptorMethods {
			if authExemptMethods[m.fullMethod] {
				if m.annotated {
					// 豁免面带注解 = 双登记（豁免语义不进 proto——
					// annotations.proto 头注不变式 2）。
					annotatedExempt = append(annotatedExempt, m.fullMethod)
				}
				continue
			}
			if !m.annotated {
				missing = append(missing, m.fullMethod)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			t.Errorf("%d gRPC method(s) without a scope annotation (unannotated RPCs silently degrade to admin-only via the fail-closed default):\n%s\n→ annotate each in its proto file with option (fleetly.annotations.v1.scope) = \"<word>\"",
				len(missing), bulletList(missing))
		}
		if len(annotatedExempt) > 0 {
			slices.Sort(annotatedExempt)
			t.Errorf("%d auth-exempt method(s) carry a scope annotation (auth-exempt semantics live in auth.go authExemptMethods and never in proto):\n%s\n→ remove the scope option from these RPCs in their proto files",
				len(annotatedExempt), bulletList(annotatedExempt))
		}
	})

	t.Run("every scope option value is a known scope word", func(t *testing.T) {
		known := knownScopeWords()
		var unknown []string
		for _, m := range descriptorMethods {
			if !m.annotated {
				continue
			}
			if !known[m.scope] {
				unknown = append(unknown, m.fullMethod+" (scope = \""+m.scope+"\")")
			}
		}
		if len(unknown) > 0 {
			slices.Sort(unknown)
			t.Errorf("%d scope annotation(s) use words outside the scopeWords vocabulary (unknown words make endpoints unreachable — containsScope never satisfies them; startup init panics on the same condition):\n%s\n→ fix the value in the proto file to a known word (read/deploy/admin/terminal/tasks/build)",
				len(unknown), bulletList(unknown))
		}
	})

	t.Run("authExemptMethods has no phantom keys", func(t *testing.T) {
		descriptorSet := make(map[string]bool, len(descriptorMethods))
		for _, m := range descriptorMethods {
			descriptorSet[m.fullMethod] = true
		}
		var phantom []string
		for fullMethod := range authExemptMethods {
			if !descriptorSet[fullMethod] {
				phantom = append(phantom, fullMethod)
			}
		}
		if len(phantom) > 0 {
			slices.Sort(phantom)
			t.Errorf("%d authExemptMethods key(s) not found in proto descriptors (stale exemptions left behind by renamed/deleted RPCs):\n%s\n→ remove the stale entries from authExemptMethods in auth.go",
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
		// 无注解 scope 会被 fail-closed 静默降级为 admin-only——本票要治
		// 的病在测试侧的替身形态。
		for fullMethod := range scopeExemptMethods {
			if !authExemptMethods[fullMethod] {
				unauthExempt = append(unauthExempt, fullMethod)
			}
		}
		if len(unexempted) > 0 {
			slices.Sort(unexempted)
			t.Errorf("%d authExemptMethods entry(ies) missing from scopeExemptMethods:\n%s\n→ add each to scopeExemptMethods in this test with a one-line reason (the auth-exempt face is out of scope semantics by construction)",
				len(unexempted), bulletList(unexempted))
		}
		if len(unauthExempt) > 0 {
			slices.Sort(unauthExempt)
			t.Errorf("%d scopeExemptMethods entry(ies) not in authExemptMethods (at runtime these are NOT auth-exempt; a missing scope annotation would silently degrade to admin-only):\n%s\n→ either annotate them in their proto files with a scope option, or add them to authExemptMethods in auth.go if they genuinely need no authentication",
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
