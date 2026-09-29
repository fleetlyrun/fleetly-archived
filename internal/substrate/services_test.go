package substrate

// engine.Substrate 适配层的 spec 翻译测试（纯函数，无 daemon）：受管字段
// 固定（failure_action=pause、monitor=5s、ForceUpdate 不出现——归位零成本
// 纪律）、字段映射与缺省补齐。

import (
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/engine"
)

// TestBuildSwarmSpecTaskHardeningMapping 断言任务加固字段的 swarm 翻译面
// （DT-5 守卫①的底座层快照：CapDrop→CapabilityDrop、只读 rootfs→ReadOnly、
// pids→Resources.Limits.Pids（swarm ContainerSpec 无该字段的真机落点）、
// 非 root user、restart-condition none、Args 覆盖面）。
func TestBuildSwarmSpecTaskHardeningMapping(t *testing.T) {
	spec := engine.ServiceSpec{
		Name:           "fleetly-task-01ABCDEFGHJKMNPQRSTVWXYZ0",
		Image:          "alpine:3.19@sha256:deadbeef",
		Args:           []string{"sleep", "60"},
		User:           "65534:65534",
		ReadOnlyRootfs: true,
		CapDrop:        []string{"ALL"},
		PidsLimit:      512,
		Replicas:       1,
		Networks:       []engine.NetworkAttach{{Name: "fleetly-taskgroup-tenant1"}},
		RestartPolicy:  &engine.RestartPolicySpec{Condition: "none"},
		Resources:      &engine.ResourcesSpec{NanoCPUs: 500_000_000, MemoryBytes: 128 << 20},
	}
	sw, err := buildSwarmSpec(spec, nil, nil)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	cs := sw.TaskTemplate.ContainerSpec
	if cs == nil {
		t.Fatal("container spec missing")
	}
	if cs.User != "65534:65534" {
		t.Errorf("User = %q, want 65534:65534 (non-root is server-enforced)", cs.User)
	}
	if !cs.ReadOnly {
		t.Error("ReadOnly = false, want true (read-only rootfs is server-enforced)")
	}
	if len(cs.CapabilityDrop) != 1 || cs.CapabilityDrop[0] != "ALL" {
		t.Errorf("CapabilityDrop = %v, want [ALL]", cs.CapabilityDrop)
	}
	if cs.Args == nil || len(cs.Args) != 2 || cs.Args[0] != "sleep" {
		t.Errorf("Args = %v, want [sleep 60]", cs.Args)
	}
	if sw.TaskTemplate.Resources == nil || sw.TaskTemplate.Resources.Limits == nil ||
		sw.TaskTemplate.Resources.Limits.Pids != 512 {
		t.Errorf("Resources.Limits = %+v, want Pids=512", sw.TaskTemplate.Resources)
	}
	if rp := sw.TaskTemplate.RestartPolicy; rp == nil || string(rp.Condition) != "none" {
		t.Errorf("RestartPolicy = %+v, want condition=none", rp)
	}
	if len(sw.TaskTemplate.Networks) != 1 || sw.TaskTemplate.Networks[0].Target != "fleetly-taskgroup-tenant1" {
		t.Errorf("Networks = %+v, want the single scope network (join by name, no attach inputs)", sw.TaskTemplate.Networks)
	}
}

func TestBuildSwarmSpecManagedFields(t *testing.T) {
	spec := engine.ServiceSpec{
		Name:              "fleetly-demo-web",
		Image:             "repo/app@sha256:abc",
		Command:           []string{"run"},
		Env:               []string{"A=1", "B=2"},
		ContainerLabels:   map[string]string{"fleetly.app": "demo"},
		ServiceLabels:     map[string]string{"fleetly.managed": "true", "fleetly.app": "demo"},
		Replicas:          2,
		UpdateOrder:       "start-first",
		UpdateParallelism: 1,
		Healthcheck: &engine.HealthcheckSpec{
			Test: []string{"CMD", "true"}, Interval: 5 * time.Second,
			Timeout: 3 * time.Second, Retries: 3, StartPeriod: 10 * time.Second,
		},
		RestartPolicy: &engine.RestartPolicySpec{Condition: "any", Delay: 5 * time.Second},
	}
	sw, err := buildSwarmSpec(spec, nil, nil)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}

	uc := sw.UpdateConfig
	if uc == nil {
		t.Fatal("update config missing")
	}
	if string(uc.FailureAction) != "pause" {
		t.Fatalf("failure_action = %s, want pause (platform-fixed, D-REL-1)", uc.FailureAction)
	}
	if uc.Monitor != 5*time.Second {
		t.Fatalf("monitor = %s, want 5s (platform-fixed, not scaled up)", uc.Monitor)
	}
	if uc.Parallelism != 1 || string(uc.Order) != "start-first" {
		t.Fatalf("parallelism/order = %d/%s", uc.Parallelism, uc.Order)
	}
	if sw.Name != "fleetly-demo-web" || sw.Labels["fleetly.managed"] != "true" {
		t.Fatalf("annotations = %+v", sw.Annotations)
	}
	cs := sw.TaskTemplate.ContainerSpec
	if cs == nil || cs.Image != "repo/app@sha256:abc" || len(cs.Env) != 2 {
		t.Fatalf("container spec wrong: %+v", cs)
	}
	if cs.Healthcheck == nil || cs.Healthcheck.Interval != 5*time.Second || cs.Healthcheck.Retries != 3 {
		t.Fatalf("healthcheck = %+v", cs.Healthcheck)
	}
	if sw.TaskTemplate.RestartPolicy == nil || string(sw.TaskTemplate.RestartPolicy.Condition) != "any" {
		t.Fatal("restart policy condition missing")
	}
	if sw.Mode.Replicated == nil || sw.Mode.Replicated.Replicas == nil || *sw.Mode.Replicated.Replicas != 2 {
		t.Fatal("replicated mode wrong")
	}
}

func TestBuildSwarmSpecDefaultsAndGlobal(t *testing.T) {
	// 无 restart_policy → 平台缺省 condition=any/delay=5s（architecture §2.5）。
	sw, err := buildSwarmSpec(engine.ServiceSpec{Name: "s", Image: "img", Replicas: 1}, nil, nil)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	rp := sw.TaskTemplate.RestartPolicy
	if rp == nil || string(rp.Condition) != "any" || rp.Delay == nil || *rp.Delay != 5*time.Second {
		t.Fatalf("default restart policy = %+v", rp)
	}

	g, err := buildSwarmSpec(engine.ServiceSpec{Name: "g", Image: "img", Global: true, UpdateOrder: "stop-first"}, nil, nil)
	if err != nil {
		t.Fatalf("buildSwarmSpec global: %v", err)
	}
	if g.Mode.Global == nil {
		t.Fatal("global mode missing")
	}
	if string(g.UpdateConfig.Order) != "stop-first" {
		t.Fatalf("global order = %s, want stop-first", g.UpdateConfig.Order)
	}
}

// TestBuildSwarmSpecReplicatedJob 一次性 job 翻译（E5 Cron）：replicated-job
// 模式（TotalCompletions=1/MaxConcurrent=1）、无 UpdateConfig（job 模式被
// daemon 拒绝）、重启策略 none（失败即 failed，不重试）。
func TestBuildSwarmSpecReplicatedJob(t *testing.T) {
	// cron 包的 job 模板显式带 condition=none。
	j, err := buildSwarmSpec(engine.ServiceSpec{
		Name:          "fleetly-cron-demo-task-abc",
		Image:         "repo/task@sha256:def",
		Job:           true,
		Replicas:      1,
		RestartPolicy: &engine.RestartPolicySpec{Condition: "none"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	if j.Mode.ReplicatedJob == nil || j.Mode.ReplicatedJob.TotalCompletions == nil || *j.Mode.ReplicatedJob.TotalCompletions != 1 {
		t.Fatalf("replicated-job mode wrong: %+v", j.Mode)
	}
	if j.Mode.ReplicatedJob.MaxConcurrent == nil || *j.Mode.ReplicatedJob.MaxConcurrent != 1 {
		t.Fatalf("max-concurrent wrong: %+v", j.Mode.ReplicatedJob)
	}
	if j.UpdateConfig != nil {
		t.Fatalf("job spec must not carry update config: %+v", j.UpdateConfig)
	}
	if j.TaskTemplate.RestartPolicy == nil || string(j.TaskTemplate.RestartPolicy.Condition) != "none" {
		t.Fatalf("restart condition = %+v, want none", j.TaskTemplate.RestartPolicy)
	}
	// nil 重启策略 → 适配器补 none（不落到长驻缺省 any）。
	j2, err := buildSwarmSpec(engine.ServiceSpec{Name: "j2", Image: "img", Job: true, Replicas: 1}, nil, nil)
	if err != nil {
		t.Fatalf("buildSwarmSpec j2: %v", err)
	}
	if j2.TaskTemplate.RestartPolicy == nil || string(j2.TaskTemplate.RestartPolicy.Condition) != "none" {
		t.Fatalf("default job restart condition = %+v, want none", j2.TaskTemplate.RestartPolicy)
	}
}

// TestBuildSwarmSpecSecretFileTargetFullValues 钉死 W3 真机教训的修复形态
// （W3-S3 发现的休眠隐患，收尾票 2026-09-21 补测试锚）：SecretReference 的
// File 字段必须携带完整 UID/GID/Mode（"0"/"0"/0o444——docker CLI 同款缺省
// 安全形态）。留空会让 swarm agent 在任务启动期 strconv 解析空串直接失败
// ——该失败只在「服务挂 secret」路径触发，单测不钉则修复可被无声回退。
// 同族正确形态参照：internal/execrelay/spec.go、internal/rustfs/spec.go、
// internal/database/spec.go（各自已有同款显式赋值）。
func TestBuildSwarmSpecSecretFileTargetFullValues(t *testing.T) {
	spec := engine.ServiceSpec{
		Name:     "fleetly-demo-web",
		Image:    "repo/app@sha256:abc",
		Replicas: 1,
		Secrets: []engine.SecretMount{
			{SecretName: "fleetly-demo-token-abc12345", Target: "/run/secrets/token"}, //nolint:gosec // G101：测试夹具 secret 名，非真实凭据
		},
	}
	sw, err := buildSwarmSpec(spec, map[string]string{"fleetly-demo-token-abc12345": "secret-id-1"}, nil)
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	cs := sw.TaskTemplate.ContainerSpec
	if cs == nil || len(cs.Secrets) != 1 {
		t.Fatalf("container spec secrets = %+v, want exactly 1 reference", cs)
	}
	ref := cs.Secrets[0]
	if ref.SecretID != "secret-id-1" || ref.SecretName != "fleetly-demo-token-abc12345" {
		t.Fatalf("secret reference ids = %q/%q, want resolved id + name", ref.SecretID, ref.SecretName)
	}
	if ref.File == nil {
		t.Fatal("secret File target missing (W3 hazard resurfaced: swarm agent would fail strconv on empty UID/GID at task start)")
	}
	if ref.File.Name != "/run/secrets/token" {
		t.Fatalf("file target name = %q, want /run/secrets/token", ref.File.Name)
	}
	if ref.File.UID != "0" || ref.File.GID != "0" {
		t.Fatalf("file target UID/GID = %q/%q, want \"0\"/\"0\" (empty string breaks swarm agent strconv)", ref.File.UID, ref.File.GID)
	}
	if ref.File.Mode != 0o444 {
		t.Fatalf("file target mode = %o, want 0444", ref.File.Mode)
	}
}

// TestBuildSwarmSpecSecretNotEnsuredFailsExplicitly 反向锚：引擎未先行
// EnsureSecret（缺 ID）= 对账层编码错误，必须显式报错而不是静默丢引用。
func TestBuildSwarmSpecSecretNotEnsuredFailsExplicitly(t *testing.T) {
	_, err := buildSwarmSpec(engine.ServiceSpec{
		Name:     "s",
		Image:    "img",
		Replicas: 1,
		Secrets:  []engine.SecretMount{{SecretName: "fleetly-s-token-abc12345", Target: "/run/secrets/token"}}, //nolint:gosec // G101：测试夹具 secret 名，非真实凭据
	}, nil, nil)
	if err == nil {
		t.Fatal("missing secret id must fail explicitly (engine ordering bug), got nil")
	}
}
