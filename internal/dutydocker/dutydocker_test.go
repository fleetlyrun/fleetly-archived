package dutydocker

// 纯函数单测：投影提取的穷尽矩阵（snapshotOf/taskObservationsOf/
// readyNodeAddresses——原散在各包不可直测的翻译层，收编后经纯函数直测；
// 2026-09-28 整改批「原语直接测试」守卫 idiom 的同族应用）。

import (
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
)

func TestSnapshotOfExtractsEveryComparedField(t *testing.T) {
	two := uint64(2)
	svc := swarm.Service{
		ID:   "svc-1",
		Meta: swarm.Meta{Version: swarm.Version{Index: 7}},
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{
				Name:   "fleetly-test-svc",
				Labels: map[string]string{"fleetly.desired-hash": "abc", "fleetly.app": "x"},
			},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{
					Image: "ghcr.example/app:1",
					Args:  []string{"--a", "--b"},
					Env:   []string{"K=V"},
					Hosts: []string{"10.0.0.1 ctrl.example"},
					Mounts: []mount.Mount{
						{Type: mount.TypeVolume, Source: "data", Target: "/data", ReadOnly: true},
					},
					Configs: []*swarm.ConfigReference{
						{ConfigName: "cfg-1"},
					},
					Secrets: []*swarm.SecretReference{
						{SecretID: "sec-id-1", SecretName: "sec-1"},
					},
					Healthcheck: &container.HealthConfig{Test: []string{"CMD", "ping"}},
				},
				Placement: &swarm.Placement{Constraints: []string{"node.role==manager"}},
				Networks: []swarm.NetworkAttachmentConfig{
					{Target: "netid-1"},
					{Target: "host"},
				},
				Resources: &swarm.ResourceRequirements{Limits: &swarm.Limit{MemoryBytes: 128 << 20}},
			},
			Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &two}},
			EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{
				{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: swarm.PortConfigPublishModeHost},
			}},
		},
	}

	got := snapshotOf(svc)
	if !got.Exists || got.Version != 7 {
		t.Fatalf("exists/version = %v/%d, want true/7", got.Exists, got.Version)
	}
	if !reflect.DeepEqual(got.Labels, map[string]string{"fleetly.desired-hash": "abc", "fleetly.app": "x"}) {
		t.Fatalf("labels = %v", got.Labels)
	}
	if got.Image != "ghcr.example/app:1" {
		t.Fatalf("image = %q", got.Image)
	}
	if !reflect.DeepEqual(got.Args, []string{"--a", "--b"}) {
		t.Fatalf("args = %v", got.Args)
	}
	if !reflect.DeepEqual(got.Env, []string{"K=V"}) {
		t.Fatalf("env = %v", got.Env)
	}
	if !reflect.DeepEqual(got.Hosts, []string{"10.0.0.1 ctrl.example"}) {
		t.Fatalf("hosts = %v", got.Hosts)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Source != "data" || got.Mounts[0].Target != "/data" || !got.Mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v", got.Mounts)
	}
	if !reflect.DeepEqual(got.ConfigNames, []string{"cfg-1"}) {
		t.Fatalf("config names = %v", got.ConfigNames)
	}
	if !reflect.DeepEqual(got.SecretIDs, []string{"sec-id-1"}) || !reflect.DeepEqual(got.SecretNames, []string{"sec-1"}) {
		t.Fatalf("secrets = %v/%v", got.SecretIDs, got.SecretNames)
	}
	if !reflect.DeepEqual(got.HealthTest, []string{"CMD", "ping"}) {
		t.Fatalf("health test = %v", got.HealthTest)
	}
	if !reflect.DeepEqual(got.Constraints, []string{"node.role==manager"}) {
		t.Fatalf("constraints = %v", got.Constraints)
	}
	if got.Global {
		t.Fatalf("global = true, want false")
	}
	if got.Replicas != 2 {
		t.Fatalf("replicas = %d", got.Replicas)
	}
	if got.MemoryBytes != 128<<20 {
		t.Fatalf("memory = %d", got.MemoryBytes)
	}
	if len(got.Ports) != 1 || got.Ports[0].PublishedPort != 8080 || got.Ports[0].PublishMode != swarm.PortConfigPublishModeHost {
		t.Fatalf("ports = %+v", got.Ports)
	}
	if !reflect.DeepEqual(got.Networks, []string{"netid-1", "host"}) {
		t.Fatalf("networks = %v", got.Networks)
	}
}

func TestSnapshotOfMinimalService(t *testing.T) {
	// 最小服务（global 无 healthcheck/挂载/约束）：可选面零值、Labels 恒非
	// nil、Networks 空——收敛比对的可缺失输入形态。
	svc := swarm.Service{
		Meta: swarm.Meta{Version: swarm.Version{Index: 1}},
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "fleetly-min"},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{Image: "img"},
			},
			Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		},
	}
	got := snapshotOf(svc)
	if !got.Global || got.Replicas != 0 {
		t.Fatalf("global/replicas = %v/%d, want true/0", got.Global, got.Replicas)
	}
	if got.Labels == nil || len(got.Labels) != 0 {
		t.Fatalf("labels = %v, want non-nil empty", got.Labels)
	}
	// 可选面零长度（append([]T{}, nil...) 形态与六包原实现一致——空非 nil）。
	if len(got.HealthTest) != 0 || len(got.Mounts) != 0 || len(got.Constraints) != 0 ||
		len(got.SecretNames) != 0 || len(got.ConfigNames) != 0 {
		t.Fatalf("optional faces should be empty, got %+v", got)
	}
}

func TestSnapshotOfReplicasUnset(t *testing.T) {
	// replicated 未显式设副本（swarm 缺省 1 不落 spec）：投影 0——与既有
	// 六包投影同语义（消费方期望态显式置数，0 即不一致信号）。
	svc := swarm.Service{
		Meta: swarm.Meta{Version: swarm.Version{Index: 1}},
		Spec: swarm.ServiceSpec{
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "img"}},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}},
		},
	}
	if got := snapshotOf(svc); got.Replicas != 0 {
		t.Fatalf("replicas = %d, want 0", got.Replicas)
	}
}

func TestTaskObservationsOfProjectsStateDesiredErrImage(t *testing.T) {
	tasks := []swarm.Task{
		{
			ID:           "t1",
			DesiredState: swarm.TaskStateRunning,
			Status:       swarm.TaskStatus{State: swarm.TaskStateRunning},
			Spec:         swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "app:v2"}},
		},
		{
			ID:           "t2",
			DesiredState: swarm.TaskStateShutdown,
			Status:       swarm.TaskStatus{State: swarm.TaskStateFailed, Err: "boom"},
			Spec:         swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "app:v1"}},
		},
	}
	got := taskObservationsOf(tasks)
	want := []TaskObservation{
		{State: "running", DesiredState: "running", Image: "app:v2"},
		{State: "failed", DesiredState: "shutdown", Err: "boom", Image: "app:v1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observations = %+v, want %+v", got, want)
	}
}

func TestReadyNodeAddressesMatrix(t *testing.T) {
	// 矩阵自 metrics/docker_test.go 迁入（语义逐字保持）：ready+active 入选、
	// 非 ready/非 active 排除、Status.Addr 缺省回落 ManagerStatus.Addr、
	// 无地址如实跳过。
	nodes := []swarm.Node{
		{ID: "n1", Spec: swarm.NodeSpec{Availability: swarm.NodeAvailabilityActive},
			Status: swarm.NodeStatus{State: swarm.NodeStateReady, Addr: "10.1.0.1"}},
		{ID: "n2", Spec: swarm.NodeSpec{Availability: swarm.NodeAvailabilityDrain},
			Status: swarm.NodeStatus{State: swarm.NodeStateReady, Addr: "10.1.0.2"}},
		{ID: "n3", Spec: swarm.NodeSpec{Availability: swarm.NodeAvailabilityActive},
			Status: swarm.NodeStatus{State: swarm.NodeStateDown, Addr: "10.1.0.3"}},
		{ID: "n4", Spec: swarm.NodeSpec{Availability: swarm.NodeAvailabilityActive},
			Status:        swarm.NodeStatus{State: swarm.NodeStateReady},
			ManagerStatus: &swarm.ManagerStatus{Addr: "10.1.0.4"}},
		{ID: "n5", Spec: swarm.NodeSpec{Availability: swarm.NodeAvailabilityActive},
			Status: swarm.NodeStatus{State: swarm.NodeStateReady}},
	}
	got := readyNodeAddresses(nodes)
	if want := []string{"10.1.0.1", "10.1.0.4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	if got := readyNodeAddresses(nil); len(got) != 0 {
		t.Fatalf("nil input should yield empty, got %v", got)
	}
}
