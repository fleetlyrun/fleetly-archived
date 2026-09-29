package substrate

// engine↔substrate seam 的投影契约腿（2026-09-29 架构评审 C3）：真翻译层
// 的纯函数往返——buildSwarmSpec（engine.ServiceSpec → swarm.ServiceSpec）→
// serviceToState（swarm.Service → engine.ServiceState）——过 testsupport 的
// 投影契约表（与 internal/engine fake_contract_test.go 的 fake 腿同一张表：
// 两个 adapter 咬合一个 seam；漏抄字段类缺陷〔UpdateFailureAction 是事后
// 补的历史先例〕提交期即红，不再等真机篡改场景暴露）。
//
// 真机行为腿（swarm daemon 上的端口协议集：not-found 哨兵/删除幂等/版本
// 令牌冲突重试）挂账——CI 无 dind 基建（pr.yml 门禁面无 docker 服务），
// 现有覆盖=协议点在 engine fake 腿钉死 + 适配器侧逐点注释即契约；引入
// dind 门禁时以 testsupport 套件为骨架补真机腿。

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/testsupport"
)

// contractSpecName 是契约测试的服务名（naming 公式外的测试专名——契约
// 测试不碰 naming 注册面）。
const contractSpecName = "fleetly-contract-web"

// maximalContractSpec 构造覆盖契约表全部非豁免字段的极大 spec（Require-
// SpecFieldsPopulated 钉住极大性；engine 侧 fake_contract_test.go 有一份
// 同构造器——两侧各自被同一守卫钉死，漂移即红）。
func maximalContractSpec() engine.ServiceSpec {
	return engine.ServiceSpec{
		Name:           contractSpecName,
		Image:          "registry.example/apps/contract@sha256:0123456789abcdef",
		Command:        []string{"sh", "-c", "exec server"},
		Args:           []string{"--flag"},
		User:           "1000:1000",
		ReadOnlyRootfs: true,
		CapDrop:        []string{"ALL"},
		PidsLimit:      512,
		Env:            []string{"A=1", "B=2"},
		ServiceLabels: map[string]string{
			state.LabelManaged:     state.ManagedLabelValue,
			state.LabelDesiredHash: "contract-hash",
		},
		ContainerLabels: map[string]string{state.LabelApp: "contract"},
		Replicas:        2,
		Networks: []engine.NetworkAttach{
			{Name: "fleetly-contract-net", Aliases: []string{"web"}},
		},
		Mounts:            []engine.MountSpec{{VolumeName: "fleetly-contract-data", Target: "/data", ReadOnly: true}},
		Secrets:           []engine.SecretMount{{SecretName: "fleetly-contract-secret", Target: "/run/secrets/tok"}},
		Configs:           []engine.ConfigMount{{ConfigName: "fleetly-contract-config", Target: "/app/config.yaml"}},
		Healthcheck:       &engine.HealthcheckSpec{Test: []string{"CMD", "true"}, Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 3, StartPeriod: 10 * time.Second},
		UpdateOrder:       "start-first",
		UpdateParallelism: 2,
		UpdateDelay:       7 * time.Second,
		RestartPolicy:     &engine.RestartPolicySpec{Condition: "any", Delay: 3 * time.Second, MaxAttempts: 5, Window: time.Minute},
		Resources:         &engine.ResourcesSpec{NanoCPUs: 1_000_000_000, MemoryBytes: 128 << 20},
		Constraints:       []string{"node.role==worker"},
		StopSignal:        "SIGTERM",
		StopGracePeriod:   25 * time.Second,
	}
}

// contractObjectIDs 是 secret/config 的已解析 ID 集（引擎先行 Ensure 后的
// 形态——buildSwarmSpec 的引用完整性输入）。
func contractObjectIDs() (map[string]string, map[string]string) {
	return map[string]string{"fleetly-contract-secret": "sec-id-contract"},
		map[string]string{"fleetly-contract-config": "cfg-id-contract"}
}

// TestPureTranslationRoundTripExhaustive 真翻译层往返：契约表逐字段断言
// + 枚举守卫（spec 新增字段不入表即红）+ state 侧对账（字段无人认领即红）。
func TestPureTranslationRoundTripExhaustive(t *testing.T) {
	spec := maximalContractSpec()
	testsupport.RequireSpecFieldsPopulated(t, spec, testsupport.ServiceProjectionPairs())
	testsupport.RequireSpecFieldsCovered(t, spec, testsupport.ServiceProjectionPairs())
	testsupport.RequireStateFieldsAccounted(t, engine.ServiceState{}, testsupport.ServiceProjectionPairs(),
		testsupport.ReservedStateObservationFields())

	secretIDs, configIDs := contractObjectIDs()
	sw, err := buildSwarmSpec(spec, secretIDs, configIDs)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	got := serviceToState(swarm.Service{
		Meta: swarm.Meta{Version: swarm.Version{Index: 1}},
		Spec: sw,
	})
	testsupport.AssertFieldRoundTrip(t, spec, got, testsupport.ServiceProjectionPairs())

	// DesiredHash 派生位：label 源入表（ServiceLabels→Labels），派生值单点
	// 复核（fake 腿同款断言——两侧派生逻辑漂移即红）。
	if got.DesiredHash != "contract-hash" {
		t.Fatalf("desired hash = %q, want contract-hash（label 派生断裂）", got.DesiredHash)
	}
}

// TestManagedUpdateConfigInvariants 受管字段纪律（release-semantics §2.8，
// services.go 头注）：FailureAction 恒 pause、Monitor 5s——适配器固定值，
// 期望 spec 无表达面。ForceUpdate「恒不递增」在本 API 代是结构性的
// （swarm.UpdateConfig 已无该字段，ServiceUpdate 只递 Version+Spec——无处
// 可写 force，Spike B2 纪律零成本成立）。
func TestManagedUpdateConfigInvariants(t *testing.T) {
	spec := maximalContractSpec()
	secretIDs, configIDs := contractObjectIDs()
	sw, err := buildSwarmSpec(spec, secretIDs, configIDs)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	uc := sw.UpdateConfig
	if uc == nil {
		t.Fatal("长驻服务必须有 UpdateConfig")
	}
	if uc.FailureAction != swarm.UpdateFailureActionPause {
		t.Fatalf("failure action = %q, want pause（平台受管字段）", uc.FailureAction)
	}
	if uc.Monitor != managedMonitor {
		t.Fatalf("monitor = %s, want %s（平台固定值）", uc.Monitor, managedMonitor)
	}
	if uc.Order != swarm.UpdateOrder(spec.UpdateOrder) || uc.Parallelism != spec.UpdateParallelism || uc.Delay != spec.UpdateDelay {
		t.Fatalf("update config 期望字段被受管值污染：order=%s parallelism=%d delay=%s", uc.Order, uc.Parallelism, uc.Delay)
	}
}

// TestJobSpecForm job 模式形态（架构 §4.3 执行行）：TotalCompletions=1 /
// MaxConcurrent=1 的单发 replicated-job；不接受 UpdateConfig（daemon 拒绝）
// ；重启策略缺省 none（失败即终态，不重试）。
func TestJobSpecForm(t *testing.T) {
	spec := maximalContractSpec()
	spec.Job = true
	spec.RestartPolicy = nil
	secretIDs, configIDs := contractObjectIDs()
	sw, err := buildSwarmSpec(spec, secretIDs, configIDs)
	if err != nil {
		t.Fatalf("buildSwarmSpec(job): %v", err)
	}
	rj := sw.Mode.ReplicatedJob
	if rj == nil || rj.TotalCompletions == nil || *rj.TotalCompletions != 1 || rj.MaxConcurrent == nil || *rj.MaxConcurrent != 1 {
		t.Fatalf("job mode = %+v, want replicated-job 1/1", sw.Mode)
	}
	if sw.UpdateConfig != nil {
		t.Fatal("job 模式不得携带 UpdateConfig（daemon 拒绝）")
	}
	if rp := sw.TaskTemplate.RestartPolicy; rp == nil || rp.Condition != swarm.RestartPolicyConditionNone {
		t.Fatalf("job 缺省重启策略 = %+v, want none", sw.TaskTemplate.RestartPolicy)
	}
}

// TestGlobalReplicaProjection global 实况副本语义（fake globalReplicasOf
// 同构注释，M1-3）：global 服务实况 Replicas 恒 0——spec 侧期望副本不参与
// global 实况。
func TestGlobalReplicaProjection(t *testing.T) {
	spec := maximalContractSpec()
	spec.Global = true
	spec.Replicas = 3
	secretIDs, configIDs := contractObjectIDs()
	sw, err := buildSwarmSpec(spec, secretIDs, configIDs)
	if err != nil {
		t.Fatalf("buildSwarmSpec(global): %v", err)
	}
	if sw.Mode.Global == nil {
		t.Fatal("global 位丢失")
	}
	got := serviceToState(swarm.Service{Meta: swarm.Meta{Version: swarm.Version{Index: 1}}, Spec: sw})
	if !got.Global || got.Replicas != 0 {
		t.Fatalf("global 实况 = %v/%d, want true/0", got.Global, got.Replicas)
	}
}

// TestRestartPolicyDefault 重启策略缺省注入（architecture §2.5）：nil 时
// 适配器补 condition=any / delay=5s——往返表用显式策略，缺省形态单点钉死。
func TestRestartPolicyDefault(t *testing.T) {
	spec := maximalContractSpec()
	spec.RestartPolicy = nil
	secretIDs, configIDs := contractObjectIDs()
	sw, err := buildSwarmSpec(spec, secretIDs, configIDs)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	rp := sw.TaskTemplate.RestartPolicy
	if rp == nil || rp.Condition != defaultRestartCondition {
		t.Fatalf("缺省重启策略 = %+v, want condition=%s", rp, defaultRestartCondition)
	}
	if rp.Delay == nil || *rp.Delay != defaultRestartDelay {
		t.Fatalf("缺省重启延迟 = %v, want %s", rp.Delay, defaultRestartDelay)
	}
}

// TestUnresolvedObjectReferenceIsExplicit secret/config 引用未先行 Ensure
// 的显式报错（对账层编码错误不静默丢引用——buildSwarmSpec 的引擎次序契约）。
func TestUnresolvedObjectReferenceIsExplicit(t *testing.T) {
	spec := maximalContractSpec()
	if _, err := buildSwarmSpec(spec, nil, nil); err == nil {
		t.Fatal("未解析的 secret/config 引用必须显式报错（engine ordering bug 信号）")
	}
}
