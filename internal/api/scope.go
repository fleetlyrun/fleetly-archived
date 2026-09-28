package api

// 方法级 scope 登记表（T2.17 鉴权矩阵；IMPL-ARCH-J 换源）：登记的唯一人类
// 编辑点 = proto 注解——每个 RPC 以
//
//	option (fleetly.annotations.v1.scope) = "<词>";
//
// 注解（定义在 proto/fleetly/annotations/v1/annotations.proto，字段号 50000；
// 注解值逐条迁自原手工 methodScopes 表，迁移经 descriptor 全集 ⇆ 旧表零 diff
// 校验背书）。本文件不再持有第二份手写方法清单：包初始化期从 protoregistry
// 遍历 fleetly.* 服务方法读注解，生成与换源前同形态的 map（键 = gRPC
// FullMethod "/<package>.<Service>/<Method>"，值 = scope 词）。
//
// 词表语义（本文件头注的历史口径，随登记迁至 proto 注释与本处摘要）：
//
//	read   — 全部只读面（List/Get/Show/Watch/Follow/History/Status）
//	deploy — 部署/回滚/取消、env 写（env-set 影响下次部署）、漂移收敛与
//	         opt-in 置位（写运行域/影响下次部署语义）
//	admin  — token 管理、env 明文读、app 删除（破坏性）、构建触发（H14
//	         整改：TriggerBuild 的 base_dir 可指向宿主任意目录）与平台级
//	         设置/凭据/审计面
//	terminal / tasks / build — 独立 scope（read/deploy 不蕴含，admin 蕴含；
//	         E7 W5-S6 / DT-5 / DT-6）
//
//	逐方法的取舍注记住在各 proto 文件的对应 rpc 注释（与登记同址）。
//
// 词与蕴含语义的单点是 auth.go 的 scopeWords（IMPL-ARCH-I）；未注解方法
// 不进表——调用方（拦截器）按 fail-closed 默认 admin 拒绝，逐字保持
// （auth.go UnaryAuthInterceptor/StreamAuthInterceptor 零改动）。鉴权豁免
// （无凭据可达面）不进 proto：豁免住 auth.go authExemptMethods。
//
// 纪律：新增 RPC 必须在 proto 注解 scope；未注解方法在拦截器按 admin 拒绝
// （fail-closed，见 auth.go）；注解词必须在 scopeWords 词表内——init 校验
// fail-fast（写错词在启动期爆，不静默成永不可达端点），scope_completeness_
// test.go 的 descriptor-walk 双向钉死（改登记 = 改测试守卫不变）。

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	annotationsv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/annotations/v1"

	// fleetly.server.v1 的文件描述符必须链接进二进制，登记表才有源
	// （生产路径经服务装配必然链接；此处显式钉住依赖，防空转——描述符
	// 缺位时 init 直接 panic，不会静默降级成全表空）。
	_ "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// methodScopes 是方法 → scope 的运行时登记表（形态与换源前的手工表一致）。
// 内容 = init 期 descriptor walk 读 scope 注解（scopeRegistryFromDescriptors），
// 不手写。
var methodScopes = map[string]string{}

func init() {
	methodScopes = scopeRegistryFromDescriptors()
	known := knownScopeWords()
	// 启动期校验（fail-fast）：注解词必须 ∈ scopeWords 已知词——词表外的
	// 词若静默入库，端点会被 containsScope 恒拒（containsScope 对未知词不
	// 满足任何需求），等于不可达端点；在启动期爆，不留给运行时。
	for fullMethod, scope := range methodScopes {
		if !known[scope] {
			panic("api: scope option on " + fullMethod + " is " + scope +
				", not a known scope word (vocabulary is scopeWords in auth.go)")
		}
	}
	// 空表即描述符缺位或注解全失——fail-closed 会把每个方法静默降级为
	// admin-only，属静默行为漂移，启动期直接拒绝。
	if len(methodScopes) == 0 {
		panic("api: scope registry generated empty from descriptors — " +
			"genproto/fleetly/server/v1 descriptors are not linked, or no RPC carries the scope annotation")
	}
}

// scopeRegistryFromDescriptors 遍历 protoregistry 中 fleetly.* 命名空间的
// 全部服务方法，读 scope 注解生成登记表；无注解的方法不进表（调用方按
// fail-closed 处理）。
func scopeRegistryFromDescriptors() map[string]string {
	methodScopes := make(map[string]string)
	protoregistry.GlobalFiles.RangeFiles(func(fileDescriptor protoreflect.FileDescriptor) bool {
		// 只收 fleetly.* 命名空间（与 scope_completeness_test.go 的 walk
		// 同口径；client/console 两包无 service、shared 只有消息——实际
		// 来源即 fleetly.server.v1 的全部服务）。
		if !strings.HasPrefix(string(fileDescriptor.Package()), "fleetly.") {
			return true
		}
		services := fileDescriptor.Services()
		for i := 0; i < services.Len(); i++ {
			service := services.Get(i)
			methods := service.Methods()
			for j := 0; j < methods.Len(); j++ {
				method := methods.Get(j)
				scope, ok := registeredScope(method)
				if !ok {
					continue
				}
				// gRPC FullMethod 形如 /<package>.<Service>/<Method>——
				// service.FullName() 已含 proto 包前缀。
				fullMethod := "/" + string(service.FullName()) + "/" + string(method.Name())
				methodScopes[fullMethod] = scope
			}
		}
		return true
	})
	return methodScopes
}

// registeredScope 读单个方法描述符的 scope 注解（未注解返回 ok=false）。
func registeredScope(method protoreflect.MethodDescriptor) (string, bool) {
	opts, ok := method.Options().(*descriptorpb.MethodOptions)
	if !ok || opts == nil {
		return "", false
	}
	if !proto.HasExtension(opts, annotationsv1.E_Scope) {
		return "", false
	}
	scope, ok := proto.GetExtension(opts, annotationsv1.E_Scope).(string)
	if !ok {
		panic("api: scope extension on " + method.FullName() + " is not a string")
	}
	return scope, true
}

// knownScopeWords 投影 scopeWords 词表为集合（词表单点在 auth.go，本处只
// 消费，不复制词形）。
func knownScopeWords() map[string]bool {
	set := make(map[string]bool, len(scopeWords))
	for _, spec := range scopeWords {
		set[spec.word] = true
	}
	return set
}

// RequiredScope 返回方法所需 scope（未登记返回 false——调用方按 admin
// 拒绝路径处理）。
func RequiredScope(fullMethod string) (string, bool) {
	s, ok := methodScopes[fullMethod]
	return s, ok
}

// methodAction 是审计 action 的方法级词根（api.<Service>.<Method>——审计
// action 记 token/方法级，不发明新事件名）。
func methodAction(fullMethod string) string {
	trimmed := strings.TrimPrefix(fullMethod, "/fleetly.server.v1.")
	return "api." + strings.ReplaceAll(trimmed, "/", ".")
}
