//go:build manual

package substrate_test

// IMPL-T2-1/DT-5 本机真机探针（默认不跑；对本地 swarm 实测——本票 hardening
// 与 internal 作用域的底座层证据）：
//
//	FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualTaskHardening -v
//
// 覆盖面（原始输出见实施记录）：
//  1. task-group internal 网络：NetworkEnsureWithOptions(internal=true) → 底座
//     NetworkInspect 读回 internal=true（DT-7 变体的真机落点）；
//  2. 任务服务加固：engine.ServiceSpec（User 非 root / 只读 rootfs /
//     CapabilityDrop ALL / pids 限额 / 显式 stop_grace_period / restart none /
//     Args）→ ServiceCreate → ServiceInspect 逐字段回读 —— 证明映射被真实
//     daemon 接受并非破坏性丢弃；
//  3. internal 出网封死复核（T2-0① 的单容器快速腿）：同一 internal 网络内的
//     标准容器 `wget http://1.1.1.1` 非零退出（Network unreachable）。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

func TestManualTaskHardening(t *testing.T) {
	if os.Getenv("FLEETLY_MANUAL_SWARM") != "1" {
		t.Skip("set FLEETLY_MANUAL_SWARM=1 to probe the local swarm")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	c, err := substrate.NewClient("")
	if err != nil {
		t.Fatalf("construct client: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	raw, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	defer func() { _ = raw.Close() }()

	const (
		ref      = "manualt21"
		taskID   = "01JMANUALTASKHARDEN0000000"
		netName  = "fleetly-taskgroup-manualt21"
		svcName  = "fleetly-task-01JMANUALTASKHARDEN0000000"
		probeImg = "alpine:3.19"
	)
	t.Cleanup(func() {
		cleanupCtx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		_ = c.ServiceRemove(cleanupCtx, svcName)
		_ = c.NetworkRemove(cleanupCtx, netName)
	})

	// ① internal 变体真机落点。
	if err := c.NetworkEnsureWithOptions(ctx, netName, map[string]string{
		state.LabelManaged:   state.ManagedLabelValue,
		state.LabelTaskGroup: ref,
	}, true); err != nil {
		t.Fatalf("ensure internal task-group network: %v", err)
	}
	netState, err := c.NetworkInspect(ctx, netName)
	if err != nil {
		t.Fatalf("inspect task-group network: %v", err)
	}
	t.Logf("task-group network: name=%s internal=%v labels=%v driver=%s", netState.Name, netState.Internal, netState.Labels, netState.Driver)
	if !netState.Internal {
		t.Fatalf("internal variant not honored by the daemon: %+v", netState)
	}

	// ② 加固服务 spec 真机创建 + 回读。
	name, err := naming.TaskServiceName(taskID)
	if err != nil {
		t.Fatalf("task service name: %v", err)
	}
	if name != svcName {
		t.Fatalf("task service name = %q, want %q", name, svcName)
	}
	spec := engine.ServiceSpec{
		Name:           svcName,
		Image:          probeImg,
		Args:           []string{"sleep", "120"},
		User:           "65534:65534",
		ReadOnlyRootfs: true,
		CapDrop:        []string{"ALL"},
		PidsLimit:      512,
		ServiceLabels: map[string]string{
			state.LabelManaged:   state.ManagedLabelValue,
			state.LabelTasks:     "true",
			state.LabelTaskID:    taskID,
			state.LabelTaskOwner: "tok-manual",
			state.LabelTaskTTL:   "600",
		},
		ContainerLabels: map[string]string{state.LabelTaskID: taskID},
		Replicas:        1,
		Networks:        []engine.NetworkAttach{{Name: netName}},
		UpdateOrder:     "start-first",
		RestartPolicy:   &engine.RestartPolicySpec{Condition: "none"},
		StopGracePeriod: 5 * time.Second,
		Resources:       &engine.ResourcesSpec{NanoCPUs: 500_000_000, MemoryBytes: 128 << 20},
	}
	if err := c.ServiceCreate(ctx, spec); err != nil {
		t.Fatalf("create hardened task service: %v", err)
	}
	got, err := c.ServiceInspect(ctx, svcName)
	if err != nil {
		t.Fatalf("inspect task service: %v", err)
	}
	t.Logf("task service inspect: user=%q read_only_rootfs=%v cap_drop=%v pids=%d args=%v restart=%+v stop_grace=%s networks=%+v resources=%+v",
		got.User, got.ReadOnlyRootfs, got.CapDrop, got.PidsLimit, got.Args, got.RestartPolicy, got.StopGracePeriod, got.Networks, got.Resources)
	if got.User != "65534:65534" || !got.ReadOnlyRootfs || len(got.CapDrop) != 1 || got.CapDrop[0] != "ALL" || got.PidsLimit != 512 {
		t.Fatalf("hardening not round-tripped by the daemon: %+v", got)
	}
	if got.RestartPolicy == nil || got.RestartPolicy.Condition != "none" {
		t.Fatalf("restart-condition none not honored: %+v", got.RestartPolicy)
	}
	if len(got.Args) != 2 || got.Args[0] != "sleep" {
		t.Fatalf("args not honored: %+v", got.Args)
	}

	// ③ internal 出网封死（单容器快速腿）：探针网单独建 attachable 变体
	//（平台的 task-group 网刻意不可 attachable——容器侧无法挂入是安全姿态，
	// 不是缺陷；探针沿用 T2-0 的 attachable internal 形态）。
	probeNet := "fleetly-manual-t21-egress-net"
	if _, err := raw.NetworkCreate(ctx, probeNet, mobyclient.NetworkCreateOptions{
		Driver: "overlay", Internal: true, Attachable: true,
	}); err != nil {
		t.Fatalf("create probe internal network: %v", err)
	}
	defer func() { _, _ = raw.NetworkRemove(context.Background(), probeNet, mobyclient.NetworkRemoveOptions{}) }()
	created, err := raw.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &container.Config{
			Image: probeImg,
			Cmd:   []string{"wget", "-T", "3", "-q", "-O", "-", "http://1.1.1.1/"},
		},
		HostConfig: &container.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{probeNet: {}},
		},
		Name: "fleetly-manual-t21-egress",
	})
	if err != nil {
		t.Fatalf("create egress probe container: %v", err)
	}
	probeID := created.ID
	defer func() {
		_, _ = raw.ContainerRemove(context.Background(), probeID, mobyclient.ContainerRemoveOptions{Force: true})
	}()
	if _, err := raw.ContainerStart(ctx, probeID, mobyclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start egress probe: %v", err)
	}
	waitRes := raw.ContainerWait(ctx, probeID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-waitRes.Result:
		if res.StatusCode == 0 {
			t.Fatalf("internal network egress probe exited 0 (expected no route): %+v", res)
		}
		t.Logf("internal egress probe exit code = %d (no egress — DT-7 contract holds)", res.StatusCode)
	case err := <-waitRes.Error:
		t.Fatalf("wait for egress probe: %v", err)
	case <-ctx.Done():
		t.Fatal("egress probe timed out")
	}
	logs, err := raw.ContainerLogs(ctx, probeID, mobyclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("probe logs: %v", err)
	}
	defer func() { _ = logs.Close() }()
	buf := make([]byte, 512)
	n, _ := logs.Read(buf)
	t.Logf("internal egress probe output: %s", strings.TrimSpace(string(buf[:n])))
}
