package victorialogs

// 期望 spec 的形态表单测（rustfs spec_test 同型——部署器输入的权威钉定面）：
// 钉版镜像字面、回环监听参数（D-W5-4 零公网面不变量）、retention 对齐、
// manager 约束公式、host 网络任务、无端口发布面、幂等比对的漂移捕获。

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// TestDefaultImageIsPinnedDigest 镜像钉版纪律：tag@sha256 双锚字面（台账
// docs/runbooks/image-prepull.md #14 同源——改任一侧须同步）。
func TestDefaultImageIsPinnedDigest(t *testing.T) {
	if !strings.Contains(DefaultVictoriaLogsImage, "victoriametrics/victoria-logs:") {
		t.Fatalf("image %q is not the official victoria-logs repo", DefaultVictoriaLogsImage)
	}
	if !strings.Contains(DefaultVictoriaLogsImage, "@sha256:47b820890d64c4575a2a0a46415dcd8a4fd59a0f1fcd6a377693d7aea639442e") {
		t.Fatalf("image %q is not pinned to the ledger digest", DefaultVictoriaLogsImage)
	}
}

// TestBuildSpecInvariants 期望 spec 全部执行面（设计 §2.1 形态表逐条）。
func TestBuildSpecInvariants(t *testing.T) {
	spec := buildSpec("n_TESTNODEID01", 7)
	if spec.Name != ServiceName {
		t.Fatalf("service name = %s, want %s", spec.Name, ServiceName)
	}
	cs := spec.TaskTemplate.ContainerSpec
	if cs.Image != DefaultVictoriaLogsImage {
		t.Fatalf("image = %s, want pinned default", cs.Image)
	}
	// 参数三件：数据路径 / retention 对齐 / 回环监听（零公网面）。
	wantArgs := []string{
		"-storageDataPath=/vlstorage",
		"-retentionPeriod=7d",
		"-httpListenAddr=127.0.0.1:9428",
	}
	if len(cs.Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", cs.Args, wantArgs)
	}
	for i := range wantArgs {
		if cs.Args[i] != wantArgs[i] {
			t.Fatalf("args[%d] = %s, want %s", i, cs.Args[i], wantArgs[i])
		}
	}
	// 数据卷挂载。
	if len(cs.Mounts) != 1 || cs.Mounts[0].Source != VolumeName || cs.Mounts[0].Target != "/vlstorage" {
		t.Fatalf("mounts = %+v, want single volume %s -> /vlstorage", cs.Mounts, VolumeName)
	}
	// host 网络任务（回环监听的承载形态；见 spec.go 头注记）。
	if len(spec.TaskTemplate.Networks) != 1 || spec.TaskTemplate.Networks[0].Target != "host" {
		t.Fatalf("networks = %+v, want single host attachment", spec.TaskTemplate.Networks)
	}
	// 单副本 + manager 约束。
	if spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas != 1 {
		t.Fatalf("mode = %+v, want replicated-1", spec.Mode)
	}
	wantConstraint := "node.labels." + state.LabelNodeID + " == n_TESTNODEID01"
	if spec.TaskTemplate.Placement == nil ||
		len(spec.TaskTemplate.Placement.Constraints) != 1 ||
		spec.TaskTemplate.Placement.Constraints[0] != wantConstraint {
		t.Fatalf("constraints = %+v, want [%s]", spec.TaskTemplate.Placement, wantConstraint)
	}
	// 内存限额 128MB。
	if spec.TaskTemplate.Resources == nil || spec.TaskTemplate.Resources.Limits == nil ||
		spec.TaskTemplate.Resources.Limits.MemoryBytes != int64(128)<<20 {
		t.Fatalf("resources = %+v, want 128MiB limit", spec.TaskTemplate.Resources)
	}
	// 无端口发布面（host 网络任务自绑回环——EndpointSpec 恒空）。
	if spec.EndpointSpec != nil {
		t.Fatalf("endpoint spec = %+v, want nil (host network task binds loopback itself)", spec.EndpointSpec)
	}
}

// TestRetentionArg 保留天数 → VL 参数（缺省回落 7d；自定义天数直译）。
func TestRetentionArg(t *testing.T) {
	for _, tc := range []struct {
		days int
		want string
	}{
		{days: 0, want: "-retentionPeriod=7d"},
		{days: -3, want: "-retentionPeriod=7d"},
		{days: 7, want: "-retentionPeriod=7d"},
		{days: 14, want: "-retentionPeriod=14d"},
	} {
		if got := retentionArg(tc.days); got != tc.want {
			t.Errorf("retentionArg(%d) = %s, want %s", tc.days, got, tc.want)
		}
	}
}

// TestSpecEqualDriftMatrix 幂等比对：全等通过；任一执行面漂移捕获
//（镜像/参数〔含回环监听〕/挂载/网络/约束/副本/限额）。
func TestSpecEqualDriftMatrix(t *testing.T) {
	spec := buildSpec("n_TESTNODEID01", 7)
	cur := stateOf(spec)
	if !specEqual(cur, spec) {
		t.Fatal("specEqual(cur, desired) = false on identical spec, want true")
	}
	// mutate 从干净 spec 重建（ServiceSpec 内含指针，浅拷贝会共享可变面
	// ——每次重渲染避免污染基准）。
	mutate := func(f func(d *swarm.ServiceSpec)) {
		d := buildSpec("n_TESTNODEID01", 7)
		f(&d)
		if specEqual(stateOf(spec), d) {
			t.Fatalf("specEqual did not detect drift: %+v", d)
		}
	}
	mutate(func(d *swarm.ServiceSpec) { d.TaskTemplate.ContainerSpec.Image = "victoriametrics/victoria-logs:latest" })
	mutate(func(d *swarm.ServiceSpec) {
		// 回环监听被改掉 = 零公网面漂移，必须捕获。
		d.TaskTemplate.ContainerSpec.Args[2] = "-httpListenAddr=0.0.0.0:9428"
	})
	mutate(func(d *swarm.ServiceSpec) { d.TaskTemplate.ContainerSpec.Args[1] = "-retentionPeriod=30d" })
	mutate(func(d *swarm.ServiceSpec) { d.TaskTemplate.ContainerSpec.Mounts[0].Source = "other-volume" })
	mutate(func(d *swarm.ServiceSpec) { d.TaskTemplate.Networks = nil })
	mutate(func(d *swarm.ServiceSpec) { d.TaskTemplate.Placement.Constraints = []string{"node.role==worker"} })
	one := uint64(2)
	mutate(func(d *swarm.ServiceSpec) { d.Mode.Replicated.Replicas = &one })
	mem := int64(256) << 20
	mutate(func(d *swarm.ServiceSpec) { d.TaskTemplate.Resources.Limits.MemoryBytes = mem })
}

// stateOf 把 spec 投影为实况形态（与 dockerapi.snapshotOf 的投影同构——
// fake 注入用）。
func stateOf(spec swarm.ServiceSpec) dockerapi.ServiceSnapshot {
	out := dockerapi.ServiceSnapshot{Exists: true, Version: 1}
	fillSnapshotFrom(&out, spec)
	return out
}

// fillSnapshotFrom 用期望 spec 填充实况投影（dockerapi.snapshotOf 同构——
// 消费面子集：fake 只填本包比对用到的字段）。
func fillSnapshotFrom(s *dockerapi.ServiceSnapshot, spec swarm.ServiceSpec) {
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		s.Image = cs.Image
		s.Args = append([]string{}, cs.Args...)
		s.Mounts = append(s.Mounts, cs.Mounts...)
	}
	if pl := spec.TaskTemplate.Placement; pl != nil {
		s.Constraints = append([]string{}, pl.Constraints...)
	}
	if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil {
		s.Replicas = *spec.Mode.Replicated.Replicas
	}
	if task := spec.TaskTemplate; task.Resources != nil && task.Resources.Limits != nil {
		s.MemoryBytes = task.Resources.Limits.MemoryBytes
	}
	for _, n := range spec.TaskTemplate.Networks {
		s.Networks = append(s.Networks, n.Target)
	}
}
