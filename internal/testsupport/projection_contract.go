package testsupport

// 投影字段往返契约（2026-09-29 架构评审 C3）：engine↔substrate seam 的
// 「两个 adapter 咬合一个 seam」hermetic 形态——internal/engine 的
// fakeSubstrate（stateOf 投影面）与 internal/substrate 的纯翻译层
// （buildSwarmSpec → serviceToState）各自过**同一张契约表**：同一份极大
// spec，逐字段断言往返存活。表按字段名反射断言，本包因此不 import engine
// （engine 测试包引用零循环的既有纪律，见 fixture.go 头注）；字段名拼写由
// RequireSpecFieldsCovered / RequireStateFieldsAccounted 的反射枚举双向兜底。
//
// 「一个 adapter=假想 seam，两个=真」在此的落地：fake 与 real 的投影漂移
// （一侧补字段另一侧漏抄——UpdateFailureAction 是事后补的历史先例）提交期
// 即红，不再依赖真机篡改场景暴露。真机行为腿（swarm daemon 双跑的端口协
// 议集：not-found 哨兵/删除幂等/版本令牌语义）挂账——CI 无 dind 基建，
// 见 internal/substrate/spec_roundtrip_test.go 头注。

import (
	"reflect"
	"testing"
)

// ProjectionFieldPair 是契约表的一行：Spec 字段 → State 字段的往返要求。
// Skip 非空 = 持理由豁免往返（字段不进实况投影——模式位/预算位等）。
type ProjectionFieldPair struct {
	Spec  string
	State string
	Skip  string
}

// ServiceProjectionPairs 是 engine.ServiceSpec → engine.ServiceState 的
// 投影契约表（两侧适配器消费的唯一一份；改表 = 同时改两侧的语义宣言）。
func ServiceProjectionPairs() []ProjectionFieldPair {
	return []ProjectionFieldPair{
		{Spec: "Name", State: "Name"},
		{Spec: "Image", State: "Image"},
		{Spec: "Command", State: "Command"},
		{Spec: "Args", State: "Args"},
		{Spec: "User", State: "User"},
		{Spec: "ReadOnlyRootfs", State: "ReadOnlyRootfs"},
		{Spec: "CapDrop", State: "CapDrop"},
		{Spec: "PidsLimit", State: "PidsLimit"},
		{Spec: "Env", State: "Env"},
		{Spec: "ServiceLabels", State: "Labels"},
		{Spec: "ContainerLabels", State: "ContainerLabels"},
		{Spec: "Global", State: "Global"},
		{Spec: "Replicas", State: "Replicas"},
		{Spec: "Networks", State: "Networks"},
		{Spec: "Mounts", State: "Mounts"},
		{Spec: "Secrets", State: "Secrets"},
		{Spec: "Configs", State: "Configs"},
		{Spec: "Healthcheck", State: "Healthcheck"},
		{Spec: "RestartPolicy", State: "RestartPolicy"},
		{Spec: "Resources", State: "Resources"},
		{Spec: "Constraints", State: "Constraints"},
		{Spec: "StopSignal", State: "StopSignal"},
		{Spec: "StopGracePeriod", State: "StopGracePeriod"},
		{Spec: "UpdateOrder", State: "UpdateOrder"},
		{Spec: "UpdateParallelism", State: "UpdateParallelism"},
		{Spec: "UpdateDelay", State: "UpdateDelay"},
		// 模式/预算位：不进实况投影——job 服务不参与长驻对账（decodeSpecs
		// 对外投影过滤 Job），init 看门狗预算与底座无关。
		{Spec: "Job", State: "", Skip: "job 模式位不进实况投影（长驻对账不见 job 集）"},
		{Spec: "InitJob", State: "", Skip: "init job 模板位（Job+InitJob 组合语义，不落底座）"},
		{Spec: "InitJobTimeout", State: "", Skip: "init 看门狗预算（发布管线的锚，与底座投影无关）"},
	}
}

// AssertFieldRoundTrip 逐行断言 spec.<Spec> 与 state.<State> 深相等；
// 豁免行（Skip 非空）只验证 Spec 字段存在。表行引用了不存在的字段 = 表
// 漂移，同样报红。
func AssertFieldRoundTrip(t *testing.T, spec, state any, pairs []ProjectionFieldPair) {
	t.Helper()
	sv := reflect.ValueOf(spec)
	st := reflect.ValueOf(state)
	if sv.Kind() != reflect.Struct || st.Kind() != reflect.Struct {
		t.Fatalf("契约断言需要 struct（%T / %T）", spec, state)
	}
	for _, p := range pairs {
		sf := sv.FieldByName(p.Spec)
		if !sf.IsValid() {
			t.Errorf("契约表 Spec 字段 %q 在 %T 上不存在——表漂移", p.Spec, spec)
			continue
		}
		if p.Skip != "" {
			continue
		}
		tf := st.FieldByName(p.State)
		if !tf.IsValid() {
			t.Errorf("契约表 State 字段 %q 在 %T 上不存在——表漂移", p.State, state)
			continue
		}
		if !reflect.DeepEqual(sf.Interface(), tf.Interface()) {
			t.Errorf("往返断裂：%s → %s：spec=%v state=%v", p.Spec, p.State, sf.Interface(), tf.Interface())
		}
	}
}

// RequireSpecFieldsCovered 枚举守卫：spec 类型的每个导出字段必须出现在
// 契约表（含豁免行）——新增字段不入表即红，逼当场分诊（入表往返或持理由
// Skip）。「穷尽性从提交者记性搬进 CI 枚举守卫」idiom 在投影面上的应用。
func RequireSpecFieldsCovered(t *testing.T, specSample any, pairs []ProjectionFieldPair) {
	t.Helper()
	sv := reflect.TypeOf(specSample)
	covered := map[string]bool{}
	for _, p := range pairs {
		covered[p.Spec] = true
	}
	for i := 0; i < sv.NumField(); i++ {
		name := sv.Field(i).Name
		if !covered[name] {
			t.Errorf("ServiceSpec 新增字段 %q 未入投影契约表——入表（spec→state 往返）或持理由 Skip（testsupport.ServiceProjectionPairs）", name)
		}
	}
}

// RequireStateFieldsAccounted 对账 state 侧：每个导出字段要么是契约表目标，
// 要么在 reserved 里持理由（观测字段/受管字段——各有专用断言，不进往返表）。
// state 加字段无人认领即红，防投影面单侧膨胀。
func RequireStateFieldsAccounted(t *testing.T, stateSample any, pairs []ProjectionFieldPair, reserved map[string]string) {
	t.Helper()
	st := reflect.TypeOf(stateSample)
	targets := map[string]bool{}
	for _, p := range pairs {
		if p.Skip == "" {
			targets[p.State] = true
		}
	}
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		if targets[name] {
			continue
		}
		if reason, ok := reserved[name]; ok {
			if reason == "" {
				t.Errorf("state 字段 %q 的 reserved 理由为空——持理由或入表", name)
			}
			continue
		}
		t.Errorf("ServiceState 字段 %q 无人认领——入契约表（有 spec 对应）或 reserved 持理由（观测/受管字段）", name)
	}
}

// RequireSpecFieldsPopulated 极大性守卫：契约表的非豁免行在 spec 样本上
// 必须非零值——两侧（engine/substrate）各自的极大 spec 构造器都被本断言
// 钉住，构造器漂移（漏填某字段）即红，往返断言才不可能静默退化。bool 字
// 段例外：零值 false 是有义形态（如 Global=false = replicated——极大 spec
// 的主形态；true 形态由专项形态测试覆盖），真值侧仍受本断言约束。
func RequireSpecFieldsPopulated(t *testing.T, spec any, pairs []ProjectionFieldPair) {
	t.Helper()
	sv := reflect.ValueOf(spec)
	for _, p := range pairs {
		if p.Skip != "" {
			continue
		}
		f := sv.FieldByName(p.Spec)
		if !f.IsValid() {
			t.Fatalf("契约表 Spec 字段 %q 在 %T 上不存在", p.Spec, spec)
		}
		if f.Kind() == reflect.Bool {
			continue
		}
		if f.IsZero() {
			t.Errorf("极大 spec 的 %q 字段是零值——往返契约被静默退化（构造器漏填）", p.Spec)
		}
	}
}

// ReservedStateObservationFields 是 ServiceState 侧不入往返表的字段及其
// 理由（Version/UpdateState/UpdateMessage 是底座观测镜像；DesiredHash 由
// Labels 派生（往返表经 ServiceLabels→Labels 已覆盖源）；UpdateFailureAction
// 是平台受管字段（适配器恒写 pause，专用断言钉死，A8/S18）。
func ReservedStateObservationFields() map[string]string {
	return map[string]string{
		"Version":             "底座乐观令牌（观测位，spec 无对应）",
		"UpdateState":         "底座 UpdateStatus 逐字镜像（观测位）",
		"UpdateMessage":       "底座 UpdateStatus.Message 逐字镜像（观测位）",
		"DesiredHash":         "由 Labels[fleetly.desired-hash] 派生（源已入表）",
		"UpdateFailureAction": "平台受管字段（适配器恒写 pause，专用断言钉死）",
	}
}
