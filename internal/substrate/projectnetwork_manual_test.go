//go:build manual

package substrate_test

// IMPL-T15-1/OT-1 的本地真机探针（默认不跑；对本地 swarm 实证四组必查项）：
//
//	FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualProjectNetwork -v
//
// 断言面（结论写进实施方案 §4 审查记录 / 实施记录）：
//  1. 项目网对象：NetworkEnsureWithLabels 幂等创建 + label 往返（managed +
//     fleetly.project-network）→ NetworkInspect 可读回；
//  2. 别名隔离（守卫①/④）：同行两个 app 各挂 app 私网（短别名 only1/only2）
//     与项目网（别名 app1-web/app2-web）——跨 app `ping app1-web` 通；
//     `ping only1` 在同一项目网内的对端解析不到（短名只在 app 私网）；
//  3. 增/摘网滚动语义：start-first 下 service update 增网/摘网 → 任务重建
//     且新旧并存滚动窗口（旧任务 desired=shutdown），update-order 单独变更
//     零任务替换；
//  4. GC 安全性判据：NetworkInspect 的 Containers/Services 计数可见；仍被
//     服务引用的网络移除被 daemon 拒绝（FailedPrecondition）——零引用零端点
//     才回收的双层安全。
//
// 探针只证明单节点底座语义；跨节点分发依赖 UDP 4789/7946 放行（见
// docs/runbooks/project-networks.md），归 T2 真机窗口。

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

func TestManualProjectNetwork(t *testing.T) {
	if os.Getenv("FLEETLY_MANUAL_SWARM") != "1" {
		t.Skip("set FLEETLY_MANUAL_SWARM=1 to probe the local swarm")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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
		projectID = "01JMANUALPROJECTNET0000000"
		appNet1   = "fleetly-probe-t15-app1-net"
		appNet2   = "fleetly-probe-t15-app2-net"
		extraNet  = "fleetly-probe-t15-extra-net"
		svc1      = "fleetly-probe-t15-app1-web"
		svc2      = "fleetly-probe-t15-app2-web"
	)
	projectNet, err := naming.ProjectNetworkName(projectID)
	if err != nil {
		t.Fatalf("project network name: %v", err)
	}
	removeAll := func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer rcancel()
		_ = c.ServiceRemove(rctx, svc1)
		_ = c.ServiceRemove(rctx, svc2)
		for _, n := range []string{appNet1, appNet2, extraNet, projectNet} {
			_ = c.NetworkRemove(rctx, n)
		}
	}
	removeAll()
	t.Cleanup(removeAll)

	// ①② 项目网 + app 私网就位（wait: swarm 网络移除有异步滞后，先等一拍）。
	time.Sleep(2 * time.Second)
	if err := c.NetworkEnsureWithLabels(ctx, projectNet, map[string]string{
		state.LabelManaged:        state.ManagedLabelValue,
		state.LabelProjectNetwork: projectID,
	}); err != nil {
		t.Fatalf("ensure project network: %v", err)
	}
	for _, n := range []string{appNet1, appNet2, extraNet} {
		if err := c.NetworkEnsureWithLabels(ctx, n, map[string]string{state.LabelManaged: state.ManagedLabelValue}); err != nil {
			t.Fatalf("ensure app network %s: %v", n, err)
		}
	}
	got, err := c.NetworkInspect(ctx, projectNet)
	if err != nil {
		t.Fatalf("inspect project network: %v", err)
	}
	if got.Labels[state.LabelProjectNetwork] != projectID || got.Labels[state.LabelManaged] != "true" {
		t.Fatalf("project network labels = %v, want managed + fleetly.project-network=%s", got.Labels, projectID)
	}
	t.Logf("project network ensured: %s labels=%v driver=%s", got.Name, got.Labels, got.Driver)
	// 幂等：二次 ensure 不报错、对象不变。
	if err := c.NetworkEnsureWithLabels(ctx, projectNet, map[string]string{state.LabelManaged: state.ManagedLabelValue}); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}

	// 两只同项目「app」：app 私网短别名 only1/only2 + 项目网别名 app1-web/app2-web。
	svc1Spec := engine.ServiceSpec{
		Name: svc1, Image: "alpine:3", Command: []string{"sleep", "900"}, Replicas: 1,
		UpdateOrder: "start-first", UpdateParallelism: 1,
		Networks: []engine.NetworkAttach{
			{Name: appNet1, Aliases: []string{"only1"}},
			{Name: projectNet, Aliases: []string{"app1-web"}},
		},
	}
	svc2Spec := engine.ServiceSpec{
		Name: svc2, Image: "alpine:3", Command: []string{"sleep", "900"}, Replicas: 1,
		UpdateOrder: "start-first", UpdateParallelism: 1,
		Networks: []engine.NetworkAttach{
			{Name: appNet2, Aliases: []string{"only2"}},
			{Name: projectNet, Aliases: []string{"app2-web"}},
		},
	}
	if err := c.ServiceCreate(ctx, svc1Spec); err != nil {
		t.Fatalf("create app1 service: %v", err)
	}
	if err := c.ServiceCreate(ctx, svc2Spec); err != nil {
		t.Fatalf("create app2 service: %v", err)
	}
	task1 := waitRunningTask(t, ctx, c, svc1, 120*time.Second)
	task2 := waitRunningTask(t, ctx, c, svc2, 120*time.Second)
	inspect1, err := c.ServiceInspect(ctx, svc1)
	if err != nil {
		t.Fatalf("inspect app1: %v", err)
	}
	if len(inspect1.Networks) != 2 {
		t.Fatalf("app1 networks = %+v, want app net + project net (double attach)", inspect1.Networks)
	}

	// 守卫①：跨 app 按 <app>-<service> 别名互访。
	out, code := execContainer(t, ctx, raw, task2, "ping", "-c", "1", "-W", "2", "app1-web")
	t.Logf("app2 -> app1-web: exit=%d out=%q", code, strings.TrimSpace(out))
	if code != 0 {
		t.Fatalf("cross-app project-network DNS/ping failed (guard 1): %s", out)
	}
	// 守卫④：短别名只在 app 私网——app2 容器解析不到 app1 的私网短别名。
	out, code = execContainer(t, ctx, raw, task2, "ping", "-c", "1", "-W", "2", "only1")
	t.Logf("app2 -> only1 (short alias of app1): exit=%d out=%q", code, strings.TrimSpace(out))
	if code == 0 {
		t.Fatalf("short alias leaked across apps (guard 4): %s", out)
	}
	// 反向对称：app1 → app2-web 通；app1 → only2 不通。
	out, code = execContainer(t, ctx, raw, task1, "ping", "-c", "1", "-W", "2", "app2-web")
	if code != 0 {
		t.Fatalf("reverse cross-app ping failed: %s", out)
	}
	out, code = execContainer(t, ctx, raw, task1, "ping", "-c", "1", "-W", "2", "only2")
	if code == 0 {
		t.Fatalf("short alias leaked across apps (reverse direction): %s", out)
	}

	// ③ 增网滚动（start-first：新旧并存窗口）。
	updated1 := svc1Spec
	updated1.Networks = append(append([]engine.NetworkAttach{}, svc1Spec.Networks...),
		engine.NetworkAttach{Name: extraNet, Aliases: []string{"extra"}})
	if err := c.ServiceUpdate(ctx, svc1, updated1); err != nil {
		t.Fatalf("service update (network add): %v", err)
	}
	task1b := waitNewRunningTask(t, ctx, c, svc1, task1, 120*time.Second)
	oldState := taskStateOf(t, ctx, c, svc1, task1)
	t.Logf("network add rolled the service: %s -> %s (old task at rotation: %s)", task1, task1b, oldState)
	if oldState != "shutdown/desired=shutdown" && oldState != "running/desired=shutdown" {
		t.Fatalf("old task after network add = %s, want shutdown(desired) — task template change must replace it", oldState)
	}
	// 摘网同样触发重建。
	updated1.Networks = svc1Spec.Networks
	if err := c.ServiceUpdate(ctx, svc1, updated1); err != nil {
		t.Fatalf("service update (network remove): %v", err)
	}
	task1c := waitNewRunningTask(t, ctx, c, svc1, task1b, 120*time.Second)
	t.Logf("network remove rolled the service again: %s -> %s", task1b, task1c)
	_ = task1c

	// 非模板字段（update-order 单独变更）零任务替换（Spike B2 口径复核）。
	noop := svc1Spec
	noop.UpdateOrder = "stop-first"
	if err := c.ServiceUpdate(ctx, svc1, noop); err != nil {
		t.Fatalf("service update (order only): %v", err)
	}
	time.Sleep(3 * time.Second)
	after := taskStateOf(t, ctx, c, svc1, task1c)
	t.Logf("order-only update task state: %s (want running/desired=running — zero replacement)", after)
	if after != "running/desired=running" {
		t.Fatalf("task replaced by a non-template field update: %s", after)
	}

	// ④ GC 安全性：仍被服务引用的网络移除被拒绝；inspect 可见计数。
	st, err := c.NetworkInspect(ctx, projectNet)
	if err != nil {
		t.Fatalf("inspect project network: %v", err)
	}
	t.Logf("project network endpoints: containers=%d services=%d", st.Containers, st.Services)
	if st.Containers == 0 && st.Services == 0 {
		t.Fatalf("project network reports no endpoints while two services reference it (projection broken)")
	}
	if err := c.NetworkRemove(ctx, projectNet); err == nil {
		t.Fatalf("in-use network removal accepted — GC safety net absent")
	} else {
		t.Logf("in-use network removal rejected: %v", err)
	}

	// 清场后（服务移除 → 端点排空）可回收。
	if err := c.ServiceRemove(ctx, svc1); err != nil {
		t.Fatalf("remove app1 service: %v", err)
	}
	if err := c.ServiceRemove(ctx, svc2); err != nil {
		t.Fatalf("remove app2 service: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, ierr := c.NetworkInspect(ctx, projectNet)
		if ierr == nil && st.Containers == 0 && st.Services == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoints did not drain within budget (last: containers=%d services=%d err=%v)", st.Containers, st.Services, ierr)
		}
		time.Sleep(2 * time.Second)
	}
	if err := c.NetworkRemove(ctx, projectNet); err != nil {
		t.Fatalf("empty project network removal failed: %v", err)
	}
	t.Logf("project network reclaimed after member services left (zero endpoints)")
}

// execContainer 在任务容器内执行命令，返回 stdout 与退出码（stdcopy 解复用
// + ExecInspect 取退出码；configinject_manual_test 的 containerFile 同款形态）。
func execContainer(t *testing.T, ctx context.Context, raw *mobyclient.Client, taskID string, cmd ...string) (string, int) {
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
		Cmd:          cmd,
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
	inspected, err := raw.ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
	if err != nil {
		t.Fatalf("exec inspect: %v", err)
	}
	if errBuf.Len() > 0 {
		return out.String() + errBuf.String(), inspected.ExitCode
	}
	return out.String(), inspected.ExitCode
}
