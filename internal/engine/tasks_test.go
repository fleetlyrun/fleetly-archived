package engine

// 任务执行面回归（T 线 DT-5 / IMPL-T2-1）：
//   - 守卫① hardening 默认落 service spec（服务端强制字段快照断言）；
//   - 守卫③ TTL 到期回收（stopping + task.expired → 服务移除 → stopped）；
//   - 守卫⑥ 孤儿 task 对账（披露 + 回收；非任务对象不误伤；节流）；
//   - 作用域解析矩阵（internal 仅 task-group；网络必须在位；变体错配拒绝）；
//   - 镜像 digest 钉定；task-group 网络 + 控制面挂靠投影。

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ensureApp 播种一个应用行（任务作用域/挂靠成员夹具；ensureAppForTest 的
// 宽签名包装）。
func ensureApp(t *testing.T, h *harness, name string) state.App {
	t.Helper()
	app, err := ensureAppForTest(t, context.Background(), h.store, name)
	if err != nil {
		t.Fatalf("seed app %s: %v", name, err)
	}
	return app
}

// taskFixture 在 state 落一条任务行（owner = 现铸机具令牌——owner_token_id
// 有 tokens 外键；引擎不核验属主，令牌只为满足完整性）。
func taskFixture(t *testing.T, h *harness, scopeKind, scopeRef, network string, ttl time.Duration) state.Task {
	t.Helper()
	id := ulid.Make().String()
	service, err := naming.TaskServiceName(id)
	if err != nil {
		t.Fatalf("task service name: %v", err)
	}
	tokenID := ulid.Make().String()
	if _, err := h.store.CreateToken(context.Background(), state.TokenWrite{
		ID: tokenID, Hash: state.HashToken("flt_fixture_" + tokenID), Name: "task fixture", Scopes: "tasks", Actor: "human",
	}); err != nil {
		t.Fatalf("create task owner token: %v", err)
	}
	task, err := h.store.CreateTask(context.Background(), state.TaskWrite{
		ID: id, Name: "engine fixture task",
		OwnerTokenID: tokenID,
		Image:        "alpine:3.19@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ScopeKind:    scopeKind, ScopeRef: scopeRef, Network: network,
		TTLSeconds: int64(ttl / time.Second), CPUMillis: 500, MemoryBytes: 128 << 20,
		Service: service,
		// ExpiresAt 以引擎（假）时钟为基：本夹具混用真实时钟落库与假时钟推进
		//（package 级 testStart），到期锚必须钉死，否则「到期」判定随测试在
		// 包内的执行时刻漂移。
		ExpiresAt: h.clk.Now().Add(ttl),
	}, state.DefaultTaskQuota())
	if err != nil {
		t.Fatalf("create task fixture: %v", err)
	}
	return task
}

func TestResolveTaskScopeMatrix(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := ensureApp(t, h, "scope-app")

	// task-group：在位（非 internal）→ 解析成功；缺失 → 显式拒绝。
	if _, err := h.eng.ResolveTaskScope(ctx, TaskScopeTaskGroup, "tenant1", false); err == nil {
		t.Fatal("missing task-group network must fail closed (tasks join existing networks only)")
	}
	h.nets.injectNetwork("fleetly-taskgroup-tenant1", map[string]string{state.LabelManaged: "true"})
	name, err := h.eng.ResolveTaskScope(ctx, TaskScopeTaskGroup, "tenant1", false)
	if err != nil || name != "fleetly-taskgroup-tenant1" {
		t.Fatalf("task-group resolve = %q (err=%v)", name, err)
	}
	// internal 变体只在 task-group 支持；非 internal 网请求 internal 拒绝。
	if _, err := h.eng.ResolveTaskScope(ctx, TaskScopeTaskGroup, "tenant1", true); err == nil {
		t.Fatal("internal mismatch on an existing non-internal network must fail closed")
	}
	h.nets.injectNetwork("fleetly-taskgroup-untrusted", map[string]string{state.LabelManaged: "true"})
	h.nets.nets["fleetly-taskgroup-untrusted"].internal = true
	if _, err := h.eng.ResolveTaskScope(ctx, TaskScopeTaskGroup, "untrusted", true); err != nil {
		t.Fatalf("internal task-group resolve: %v", err)
	}
	// app / project 作用域不支持 internal（不可静默降级）。
	if _, err := h.eng.ResolveTaskScope(ctx, TaskScopeApp, app.Name, true); err == nil {
		t.Fatal("app scope with internal=true must fail closed")
	}
	if _, err := h.eng.ResolveTaskScope(ctx, TaskScopeProject, app.ProjectID, true); err == nil {
		t.Fatal("project scope with internal=true must fail closed")
	}
	// app / project 作用域网络必须在位。
	if _, err := h.eng.ResolveTaskScope(ctx, TaskScopeApp, app.Name, false); err == nil {
		t.Fatal("app scope with an absent network must fail closed")
	}
	appNet, err := naming.NetworkName(app.TeamSlug, app.ProjectSlug, app.Name)
	if err != nil {
		t.Fatalf("app network name: %v", err)
	}
	h.nets.injectNetwork(appNet, map[string]string{state.LabelManaged: "true"})
	if name, err := h.eng.ResolveTaskScope(ctx, TaskScopeApp, app.Name, false); err != nil || name != appNet {
		t.Fatalf("app scope resolve = %q (err=%v)", name, err)
	}
	// 未知 kind 拒绝。
	if _, err := h.eng.ResolveTaskScope(ctx, "pod", "x", false); err == nil {
		t.Fatal("unknown scope kind must be rejected")
	}
}

func TestTaskConvergenceAppliesServerEnforcedHardening(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.nets.injectNetwork("fleetly-taskgroup-tenant1", map[string]string{state.LabelManaged: "true"})
	task := taskFixture(t, h, TaskScopeTaskGroup, "tenant1", "fleetly-taskgroup-tenant1", 10*time.Minute)

	h.eng.AdvanceTasks(ctx)

	svc, ok := h.sub.services[task.Service]
	if !ok {
		t.Fatalf("task service %s was not created", task.Service)
	}
	spec := svc.spec
	// 守卫①：服务端强制加固（proto 没有对应字段——不可关闭）。
	if spec.User != taskPlatformUser {
		t.Errorf("User = %q, want %q (non-root is server-enforced)", spec.User, taskPlatformUser)
	}
	if !spec.ReadOnlyRootfs {
		t.Error("ReadOnlyRootfs = false, want true (server-enforced)")
	}
	if len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", spec.CapDrop)
	}
	if spec.PidsLimit != taskPidsLimit {
		t.Errorf("PidsLimit = %d, want %d", spec.PidsLimit, taskPidsLimit)
	}
	if spec.RestartPolicy == nil || spec.RestartPolicy.Condition != "none" {
		t.Errorf("RestartPolicy = %+v, want condition=none (crash is surfaced, never self-healed)", spec.RestartPolicy)
	}
	if spec.StopGracePeriod != taskStopGracePeriod {
		t.Errorf("StopGracePeriod = %s, want %s (explicitly pinned, T2 baseline)", spec.StopGracePeriod, taskStopGracePeriod)
	}
	// 网络按名加入（无 attach 入参面）；资源进 limits。
	if len(spec.Networks) != 1 || spec.Networks[0].Name != task.Network {
		t.Errorf("Networks = %+v, want the single scope network %s", spec.Networks, task.Network)
	}
	if spec.Resources == nil || spec.Resources.NanoCPUs != 500*1_000_000 || spec.Resources.MemoryBytes != 128<<20 {
		t.Errorf("Resources = %+v, want cpu 500m / mem 128MiB", spec.Resources)
	}
	// owner/TTL label（DT-5 回收面归因）。
	if spec.ServiceLabels[state.LabelTaskID] != task.ID ||
		spec.ServiceLabels[state.LabelTaskOwner] != task.OwnerTokenID ||
		spec.ServiceLabels[state.LabelTaskTTL] != "600" ||
		spec.ServiceLabels[state.LabelTasks] != "true" {
		t.Errorf("task labels = %+v", spec.ServiceLabels)
	}
	// 状态收敛到 running（幂等：重跑不重建）。
	if got, err := h.store.GetTask(ctx, task.ID); err != nil || got.Status != state.TaskRunning {
		t.Fatalf("task status = %v (err=%v), want running", got.Status, err)
	}
	if _, _, err := h.store.MarkTaskRunning(ctx, task.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	h.eng.AdvanceTasks(ctx)
	if _, ok := h.sub.services[task.Service]; !ok {
		t.Fatal("idempotent convergence must not remove the service")
	}
	if !hasEvent(h.events(), "task.started") {
		t.Fatalf("missing task.started event (events=%v)", h.events())
	}
}

func TestTaskTTLReclaimStopsAndRemovesService(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.nets.injectNetwork("fleetly-taskgroup-tenant1", map[string]string{state.LabelManaged: "true"})
	task := taskFixture(t, h, TaskScopeTaskGroup, "tenant1", "fleetly-taskgroup-tenant1", time.Minute)

	h.eng.AdvanceTasks(ctx) // queued → running
	if _, ok := h.sub.services[task.Service]; !ok {
		t.Fatal("setup: task service must exist after convergence")
	}

	// 未到期：janitor 不动。
	h.eng.ReapExpiredTasks(ctx)
	if got, _ := h.store.GetTask(ctx, task.ID); got.Status != state.TaskRunning {
		t.Fatalf("task reclaimed before TTL: status=%s", got.Status)
	}

	// 到期：stopping + task.expired → 下一拍移除服务并落 stopped。
	h.clk.Advance(2 * time.Minute)
	h.eng.ReapExpiredTasks(ctx)
	got, err := h.store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != state.TaskStopping || got.StopReason != "expired" {
		t.Fatalf("expired task = status %s reason %q, want stopping/expired", got.Status, got.StopReason)
	}
	if !hasEvent(h.events(), "task.expired") {
		t.Fatalf("missing task.expired event (events=%v)", h.events())
	}
	h.eng.AdvanceTasks(ctx)
	if _, ok := h.sub.services[task.Service]; ok {
		t.Fatal("expired task service must be removed by the convergence beat")
	}
	got, err = h.store.GetTask(ctx, task.ID)
	if err != nil || got.Status != state.TaskStopped {
		t.Fatalf("expired task terminal = %+v (err=%v), want stopped", got, err)
	}
	if got.StoppedAt.IsZero() {
		t.Fatal("stopped_at must be recorded")
	}
}

func TestTaskFailureOnContainerTaskFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.nets.injectNetwork("fleetly-taskgroup-tenant1", map[string]string{state.LabelManaged: "true"})
	task := taskFixture(t, h, TaskScopeTaskGroup, "tenant1", "fleetly-taskgroup-tenant1", 10*time.Minute)
	h.eng.AdvanceTasks(ctx)

	// 底座任务 failed（restart-condition none：平台不静默自愈，上抛失败）。
	h.sub.mu.Lock()
	h.sub.services[task.Service].tasks = []TaskState{{
		ID: "task-failed-1", State: "failed", DesiredState: "shutdown",
		Err: "exit code 1", Timestamp: h.clk.Now(),
	}}
	h.sub.mu.Unlock()
	h.eng.AdvanceTasks(ctx)

	got, err := h.store.GetTask(ctx, task.ID)
	if err != nil || got.Status != state.TaskFailed {
		t.Fatalf("task = %+v (err=%v), want failed", got, err)
	}
	if got.Error == "" {
		t.Fatal("failed task must carry the error summary")
	}
	if !hasEvent(h.events(), "task.failed") {
		t.Fatalf("missing task.failed event (events=%v)", h.events())
	}
}

func TestReconTasksDisclosesAndReclaimsOrphan(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// 孤儿：底座任务服务（受管 + 任务 label + 前缀族）但 state 无行。
	orphanID := ulid.Make().String()
	orphanName, err := naming.TaskServiceName(orphanID)
	if err != nil {
		t.Fatalf("orphan name: %v", err)
	}
	h.sub.mu.Lock()
	h.sub.services[orphanName] = &fakeService{spec: ServiceSpec{
		Name: orphanName,
		ServiceLabels: map[string]string{
			state.LabelManaged:   state.ManagedLabelValue,
			state.LabelTasks:     "true",
			state.LabelTaskID:    orphanID,
			state.LabelTaskOwner: "tok-orphan",
		},
	}}
	// 对照：受管但非任务族的服务（不得误伤）。
	h.sub.services["fleetly-demo-web"] = &fakeService{spec: ServiceSpec{
		Name:          "fleetly-demo-web",
		ServiceLabels: map[string]string{state.LabelManaged: state.ManagedLabelValue},
	}}
	h.sub.mu.Unlock()

	h.eng.ReconcileTasks(ctx)

	if _, ok := h.sub.services[orphanName]; ok {
		t.Fatal("orphan task service must be reclaimed (platform-owned, label-attributed)")
	}
	if _, ok := h.sub.services["fleetly-demo-web"]; !ok {
		t.Fatal("non-task managed service must not be touched by the tasks recon")
	}
	if !hasEvent(h.events(), "task.orphaned") {
		t.Fatalf("missing task.orphaned event (events=%v)", h.events())
	}

	// 节流：再次对账（服务已被回收）→ 零新增事件。
	before := len(h.events())
	h.eng.ReconcileTasks(ctx)
	if after := len(h.events()); after != before {
		t.Fatalf("throttled recon emitted %d new events, want 0", after-before)
	}

	// 在途任务服务（state 行非终态）不得被判孤儿。
	task := taskFixture(t, h, TaskScopeTaskGroup, "tenant1", "fleetly-taskgroup-tenant1", 10*time.Minute)
	h.nets.injectNetwork("fleetly-taskgroup-tenant1", map[string]string{state.LabelManaged: "true"})
	h.eng.AdvanceTasks(ctx)
	h.eng.ReconcileTasks(ctx)
	if _, ok := h.sub.services[task.Service]; !ok {
		t.Fatal("non-terminal task service must survive the tasks recon")
	}
}

func TestResolveTaskImagePinsDigest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pinned, err := h.eng.ResolveTaskImage(ctx, "alpine:3.19")
	if err != nil || pinned != "alpine:3.19@sha256:digest-alpine:3.19" {
		t.Fatalf("pinned image = %q (err=%v)", pinned, err)
	}
	// 缺失镜像 → E_IMAGE_PULL_FAILED fail-closed。
	h.images.missing["ghcr.io/nope:1"] = true
	if _, err := h.eng.ResolveTaskImage(ctx, "ghcr.io/nope:1"); err == nil {
		t.Fatal("missing image must fail closed")
	}
}

func TestEnsureTaskNetworkMembersAndProjection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := ensureApp(t, h, "control-plane")

	name, statuses, err := h.eng.EnsureTaskNetwork(ctx, "tenant1", true, []TaskNetworkMember{
		{App: app.Name, Service: "server"},
	})
	if err != nil {
		t.Fatalf("ensure task network: %v", err)
	}
	if name != "fleetly-taskgroup-tenant1" {
		t.Fatalf("network name = %q", name)
	}
	net, ok := h.nets.nets[name]
	if !ok || !net.internal {
		t.Fatalf("task-group network = %+v (internal must be true)", net)
	}
	if net.labels[state.LabelTaskGroup] != "tenant1" || net.labels[state.LabelNetworkInternal] != "true" {
		t.Fatalf("task-group labels = %+v", net.labels)
	}
	// 无成功部署史：声明落位、状态 pending（下次发布生效）。
	if len(statuses) != 1 || statuses[0].Status != "pending" {
		t.Fatalf("member statuses = %+v, want pending", statuses)
	}
	projection, err := h.eng.taskNetworkProjection(ctx, app.ID)
	if err != nil || len(projection["server"]) != 1 || projection["server"][0] != name {
		t.Fatalf("projection = %+v (err=%v)", projection, err)
	}
	// 幂等：重复 ensure 零新增声明（成员仍单条）。
	if _, _, err := h.eng.EnsureTaskNetwork(ctx, "tenant1", true, []TaskNetworkMember{{App: app.Name, Service: "server"}}); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}
	members, err := h.store.ListTaskNetworkMembersForRef(ctx, "tenant1")
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %+v (err=%v), want exactly 1", members, err)
	}
	// 变体错配：已存在非 internal 网时请求 internal 拒绝。
	if _, _, err := h.eng.EnsureTaskNetwork(ctx, "tenant1", false, nil); err == nil {
		t.Fatal("existing internal network with internal=false request must fail closed")
	}
	// 成员 app 不存在：显式拒绝。
	if _, _, err := h.eng.EnsureTaskNetwork(ctx, "tenant1", true, []TaskNetworkMember{{App: "ghost", Service: "x"}}); err == nil {
		t.Fatal("unknown member app must be rejected")
	}
}

// TestPlanProjectsTaskNetworkMembership 断言挂靠声明进发布投影（planner 双挂
// ——控制面一次性挂靠经发布管线生效，不是 per-task attach）。
func TestPlanProjectsTaskNetworkMembership(t *testing.T) {
	in := PlanInput{
		AppID: "a1", AppName: "control-plane", TeamSlug: "t1", PrjSlug: "p1",
		DeploymentID: "d1",
		Spec: &compose.Spec{SpecHash: "h1", Services: []compose.Service{{
			Name: "server", Image: "alpine:3.19", Expose: []string{"8080"},
		}}},
		FileEnv:      map[string]map[string]string{"server": {}},
		ComposeEnv:   map[string]map[string]string{"server": {}},
		Images:       map[string]string{"server": "alpine:3.19@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		TaskNetworks: map[string][]string{"server": {"fleetly-taskgroup-tenant1"}},
	}
	plan, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	var got []string
	for _, svc := range plan.Services {
		if svc.Name != "fleetly-t1-p1-control-plane-server" {
			continue
		}
		for _, n := range svc.Networks {
			got = append(got, n.Name)
			if n.Name == "fleetly-taskgroup-tenant1" {
				if len(n.Aliases) != 1 || n.Aliases[0] != "control-plane-server" {
					t.Errorf("task-group alias = %v, want [control-plane-server]", n.Aliases)
				}
			}
		}
	}
	found := false
	for _, name := range got {
		if name == "fleetly-taskgroup-tenant1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("task-group network missing from the planned service networks: %v", got)
	}
}
