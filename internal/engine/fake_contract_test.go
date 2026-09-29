package engine

// fakeSubstrate 的投影契约腿（2026-09-29 架构评审 C3）：fake 的 stateOf 是
// substrate.serviceToState 的手写同构镜像（applyMode 另模拟 swarm 滚动更新
// 语义）——镜像漂移此前无任何守卫抓（真 bug 藏在 fake 与真适配器的语义差
// 里）。本腿让 fake 过 testsupport 投影契约表：与 internal/substrate 的
// 纯翻译层腿（spec_roundtrip_test.go）同一张表、同一份极大 spec 形态——
// 两个 adapter 咬合一个 seam，「一个 adapter=假想 seam，两个=真」的
// hermetic 落地。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/testsupport"
)

// maximalContractSpecFake 是 engine 侧的极大 spec 构造器（substrate 侧
// maximalContractSpec 的同形副本——两侧各自被 RequireSpecFieldsPopulated
// 钉死极大性，漂移即红；契约表在 testsupport 唯一一份）。
func maximalContractSpecFake() ServiceSpec {
	return ServiceSpec{
		Name:           "fleetly-contract-web",
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
		Networks: []NetworkAttach{
			{Name: "fleetly-contract-net", Aliases: []string{"web"}},
		},
		Mounts:            []MountSpec{{VolumeName: "fleetly-contract-data", Target: "/data", ReadOnly: true}},
		Secrets:           []SecretMount{{SecretName: "fleetly-contract-secret", Target: "/run/secrets/tok"}},
		Configs:           []ConfigMount{{ConfigName: "fleetly-contract-config", Target: "/app/config.yaml"}},
		Healthcheck:       &HealthcheckSpec{Test: []string{"CMD", "true"}, Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 3, StartPeriod: 10 * time.Second},
		UpdateOrder:       "start-first",
		UpdateParallelism: 2,
		UpdateDelay:       7 * time.Second,
		RestartPolicy:     &RestartPolicySpec{Condition: "any", Delay: 3 * time.Second, MaxAttempts: 5, Window: time.Minute},
		Resources:         &ResourcesSpec{NanoCPUs: 1_000_000_000, MemoryBytes: 128 << 20},
		Constraints:       []string{"node.role==worker"},
		StopSignal:        "SIGTERM",
		StopGracePeriod:   25 * time.Second,
	}
}

// TestFakeSubstrateProjectionContract fake 投影面过契约表：create → inspect
// 的逐字段往返（与 substrate 纯翻译层腿同一张表——镜像漂移即红）。
func TestFakeSubstrateProjectionContract(t *testing.T) {
	f := newFakeSubstrate()
	spec := maximalContractSpecFake()
	testsupport.RequireSpecFieldsPopulated(t, spec, testsupport.ServiceProjectionPairs())

	ctx := context.Background()
	if err := f.ServiceCreate(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := f.ServiceInspect(ctx, spec.Name)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	testsupport.AssertFieldRoundTrip(t, spec, got, testsupport.ServiceProjectionPairs())
	if got.DesiredHash != "contract-hash" {
		t.Fatalf("desired hash = %q, want contract-hash（label 派生断裂）", got.DesiredHash)
	}
	if got.UpdateFailureAction != "pause" {
		t.Fatalf("failure action = %q, want pause（fake 的平台受管缺省——与真适配器恒写 pause 咬合）", got.UpdateFailureAction)
	}
}

// TestFakeSubstratePortProtocol 端口协议点（fake 腿单侧钉死；真适配器的
// 对应形态=substrate/services.go 逐点注释即契约，真机双跑挂账见
// testsupport/projection_contract.go 头注）：缺失哨兵、删除幂等、更新
// 可见与版本递增。
func TestFakeSubstratePortProtocol(t *testing.T) {
	ctx := context.Background()
	f := newFakeSubstrate()

	// ① 缺失 → ErrServiceNotFound（对账把「不存在」当正常输入）。
	if _, err := f.ServiceInspect(ctx, "fleetly-contract-missing"); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("missing inspect err = %v, want ErrServiceNotFound", err)
	}

	// ② create → update（换镜像）→ inspect 反映变更 + 版本递增。
	spec := maximalContractSpecFake()
	if err := f.ServiceCreate(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := f.ServiceInspect(ctx, spec.Name)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	spec.Image = "registry.example/apps/contract@sha256:ffffffffffffffff"
	if err := f.ServiceUpdate(ctx, spec.Name, spec); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := f.ServiceInspect(ctx, spec.Name)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if after.Image != spec.Image {
		t.Fatalf("update 后镜像 = %q, want %q", after.Image, spec.Image)
	}
	if after.Version <= before.Version {
		t.Fatalf("update 后版本 = %d, want > %d", after.Version, before.Version)
	}

	// ③ 删除幂等：首删清场、再删 no-op。
	if err := f.ServiceRemove(ctx, spec.Name); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := f.ServiceRemove(ctx, spec.Name); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	if _, err := f.ServiceInspect(ctx, spec.Name); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("post-remove inspect err = %v, want ErrServiceNotFound", err)
	}
}
