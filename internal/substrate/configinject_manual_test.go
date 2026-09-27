//go:build manual

package substrate_test

// IMPL-T1-4/OT-3 的本地真机探针（默认不跑；对本地 swarm 实证两项必查项）：
//
//	FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualSwarmConfigLifecycle -v
//
// 断言面（结论写进 IMPL-T1-4 审查记录）：
//  1. ConfigReference 形态：缺 File 的引用被 daemon 拒绝（"either File or
//     Runtime should be set"）；带 File 的引用被接受并可被服务实况读回
//     （W3 secret-ID 同族教训的 config 版；ID 与 File 双写形态同 internal/
//     metrics anchorSpec 已有实证）；
//  2. 引用整体替换 → 服务滚动：service update 后旧任务下线、新任务以新
//     config 内容运行（任务容器内 `cat` 内容断言）；
//  3. 旧 config 对象 GC 时机：仍被服务 spec 引用的对象删除被底座拒绝
//     （in-use）；引用换版后旧对象可删（best-effort 清场语义的底座依据）。
//
// 探针只证明单节点底座语义；多节点分发与 staging 行为归 T2 真机窗口。

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

func TestManualSwarmConfigLifecycle(t *testing.T) {
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

	const (
		serviceName = "fleetly-probe-t1-4-config"
		configNameA = "fleetly-probe-t1-4-config-a"
		configNameB = "fleetly-probe-t1-4-config-b"
		mountTarget = "/etc/probe/cfg"
		valueA      = "value-a"
		valueB      = "value-b"
	)
	removeService := func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer rcancel()
		_ = c.ServiceRemove(rctx, serviceName)
	}
	removeConfigs := func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer rcancel()
		_ = c.ConfigRemove(rctx, configNameA)
		_ = c.ConfigRemove(rctx, configNameB)
	}
	removeService()
	removeConfigs()
	t.Cleanup(func() { removeService(); removeConfigs() })

	labels := map[string]string{"fleetly.managed": "true", "fleetly.app": "probe/t1-4/config"}
	idA, err := c.EnsureConfig(ctx, configNameA, []byte(valueA), labels)
	if err != nil {
		t.Fatalf("ensure config a: %v", err)
	}
	idB, err := c.EnsureConfig(ctx, configNameB, []byte(valueB), labels)
	if err != nil {
		t.Fatalf("ensure config b: %v", err)
	}
	_ = idB // 引用形态矩阵用 name/id 双写；B 的 id 由 ServiceUpdate 内部解析
	raw, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	defer func() { _ = raw.Close() }()

	// ① 引用形态矩阵：缺 File 拒绝；ID+File（无 Name）探测；Name+File（无
	//    ID）探测——记录 daemon 的实际判定面。
	probeShape := func(label string, ref *swarm.ConfigReference) {
		spec := swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: serviceName + "-shape-" + label},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
				Image: "alpine:3", Command: []string{"sleep", "5"}, Configs: []*swarm.ConfigReference{ref},
			}},
			Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: u64ptr(1)}},
		}
		if _, err := raw.ServiceCreate(ctx, mobyclient.ServiceCreateOptions{Spec: spec}); err != nil {
			t.Logf("shape %s rejected: %v", label, err)
			return
		}
		t.Logf("shape %s accepted", label)
		_, _ = raw.ServiceRemove(ctx, spec.Name, mobyclient.ServiceRemoveOptions{})
	}
	probeShape("no-file", &swarm.ConfigReference{ConfigID: idA, ConfigName: configNameA})
	probeShape("id-file-no-name", &swarm.ConfigReference{ConfigID: idA, File: &swarm.ConfigReferenceFileTarget{Name: mountTarget, UID: "0", GID: "0", Mode: 0o444}})
	probeShape("name-file-no-id", &swarm.ConfigReference{ConfigName: configNameA, File: &swarm.ConfigReferenceFileTarget{Name: mountTarget, UID: "0", GID: "0", Mode: 0o444}})

	// ② 长驻服务引用 config A → 容器内读到 value-a。start-first（引擎缺省
	//    更新序）让新旧任务在滚动窗口内并存——清场时机在并存窗口的断言面。
	specA := engine.ServiceSpec{
		Name: serviceName, Image: "alpine:3",
		Command:           []string{"sleep", "600"},
		Replicas:          1,
		UpdateOrder:       "start-first",
		UpdateParallelism: 1,
		Configs:           []engine.ConfigMount{{ConfigName: configNameA, Target: mountTarget}},
	}
	if err := c.ServiceCreate(ctx, specA); err != nil {
		t.Fatalf("service create with config reference (ID anchoring required): %v", err)
	}
	firstTask := waitRunningTask(t, ctx, c, serviceName, 90*time.Second)
	cur, err := c.ServiceInspect(ctx, serviceName)
	if err != nil {
		t.Fatalf("service inspect: %v", err)
	}
	if len(cur.Configs) != 1 || cur.Configs[0].ConfigName != configNameA || cur.Configs[0].Target != mountTarget {
		t.Fatalf("inspect config projection = %+v, want name+target round-trip", cur.Configs)
	}
	if got := containerFile(t, ctx, raw, firstTask, mountTarget); got != valueA {
		t.Fatalf("mounted config content = %q, want %q", got, valueA)
	}

	// ③ 引用在位时删除被底座拒绝（in-use）——GC 必须等引用换版。
	if err := c.ConfigRemove(ctx, configNameA); err == nil {
		t.Errorf("in-use config removal accepted: GC would silently break a referenced mount")
	} else {
		t.Logf("in-use config removal rejected: %v", err)
	}

	// ④ 引用整体替换 → 滚动：新任务以 value-b 运行、旧任务下线。start-first
	//    下先观察新旧并存窗口，再在旧任务仍运行（若捕获到）时清场旧对象——
	//    证明底座 in-use 判定基于服务 spec 引用而非任务态（已物化的文件不
	//    受对象删除影响）。
	specB := specA
	specB.Configs = []engine.ConfigMount{{ConfigName: configNameB, Target: mountTarget}}
	if err := c.ServiceUpdate(ctx, serviceName, specB); err != nil {
		t.Fatalf("service update with rotated config reference: %v", err)
	}
	secondTask := waitNewRunningTask(t, ctx, c, serviceName, firstTask, 90*time.Second)
	oldStateAtRotation := taskStateOf(t, ctx, c, serviceName, firstTask)
	t.Logf("old task %s state at rotation: %s (start-first overlap)", firstTask, oldStateAtRotation)
	if got := containerFile(t, ctx, raw, secondTask, mountTarget); got != valueB {
		t.Fatalf("rotated config content = %q, want %q", got, valueB)
	}
	t.Logf("config rotation rolled the service: task %s (A) -> %s (B)", firstTask, secondTask)

	// ⑤ 换版后旧对象可删（GC 语义的底座依据）。
	if err := c.ConfigRemove(ctx, configNameA); err != nil {
		t.Fatalf("stale config removal after reference rotation failed (GC blocked): %v", err)
	}
	names, err := c.ConfigList(ctx, labels)
	if err != nil {
		t.Fatalf("config list: %v", err)
	}
	for _, n := range names {
		if n == configNameA {
			t.Fatalf("stale config %s still listed after removal", n)
		}
	}
	t.Logf("stale config removed after reference rotation; remaining: %s", strings.Join(names, ","))
}

func u64ptr(v uint64) *uint64 { return &v }

// waitRunningTask 轮询直到出现运行中的任务（失败/拒绝即报错）。
func waitRunningTask(t *testing.T, ctx context.Context, c *substrate.Client, service string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		tasks, err := c.TaskList(ctx, service)
		if err != nil {
			t.Fatalf("task list: %v", err)
		}
		for _, task := range tasks {
			switch task.State {
			case "failed", "rejected":
				t.Fatalf("task %s %s err=%q", task.ID, task.State, task.Err)
			case "running":
				if task.DesiredState == "running" {
					return task.ID
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no running task within budget (last: %+v)", tasks)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitNewRunningTask 轮询直到出现不同于 previous 的运行中任务（滚动证据）。
func waitNewRunningTask(t *testing.T, ctx context.Context, c *substrate.Client, service, previous string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		tasks, err := c.TaskList(ctx, service)
		if err != nil {
			t.Fatalf("task list: %v", err)
		}
		for _, task := range tasks {
			if task.ID == previous {
				continue
			}
			switch task.State {
			case "failed", "rejected":
				t.Fatalf("new task %s %s err=%q", task.ID, task.State, task.Err)
			case "running":
				if task.DesiredState == "running" {
					return task.ID
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not roll to a new task within budget (last: %+v)", tasks)
		}
		time.Sleep(2 * time.Second)
	}
}

// taskStateOf 返回指定任务的最新状态串（缺失返回 "<absent>"）。
func taskStateOf(t *testing.T, ctx context.Context, c *substrate.Client, service, taskID string) string {
	t.Helper()
	tasks, err := c.TaskList(ctx, service)
	if err != nil {
		t.Fatalf("task list: %v", err)
	}
	for _, task := range tasks {
		if task.ID == taskID {
			return task.State + "/desired=" + task.DesiredState
		}
	}
	return "<absent>"
}

// containerFile 在任务容器内执行 `cat <path>` 并返回 stdout（stdcopy 解复用）。
func containerFile(t *testing.T, ctx context.Context, raw *mobyclient.Client, taskID, path string) string {
	t.Helper()
	list, err := raw.ContainerList(ctx, mobyclient.ContainerListOptions{
		All:     true,
		Filters: mobyclient.Filters{}.Add("label", "com.docker.swarm.task.id="+taskID),
	})
	if err != nil {
		t.Fatalf("container list for task %s: %v", taskID, err)
	}
	if len(list.Items) == 0 {
		t.Fatalf("no container found for task %s", taskID)
	}
	containerID := list.Items[0].ID
	created, err := raw.ExecCreate(ctx, containerID, mobyclient.ExecCreateOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{"cat", path},
	})
	if err != nil {
		t.Fatalf("exec create: %v", err)
	}
	attached, err := raw.ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach: %v", err)
	}
	defer attached.Close()
	var out, errBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errBuf, attached.Reader); err != nil {
		t.Fatalf("exec stdcopy: %v", err)
	}
	if errBuf.Len() > 0 {
		t.Fatalf("exec stderr: %s", errBuf.String())
	}
	return strings.TrimSpace(out.String())
}
