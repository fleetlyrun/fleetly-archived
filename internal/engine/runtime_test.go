package engine

// 运行实况投影测试（2026-09-29 Console IA 重设计）：期望∪实况并集、任务
// 全量（含历史）与实际运行水位判定（state=running 且 desired=running）、
// 期望有实况无的 missing、期望集外的实况多余殿后、活覆盖钉入声明副本
//（与 drift 同款红线②——平台自己写的副本是期望）、无成功部署早退零底座。

import (
	"context"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

func TestAppRuntimeEmptyBeforeFirstDeployment(t *testing.T) {
	h := newHarness(t)
	// 先播种 app 行（无任何部署记录）。
	h.demoApp()
	report, err := h.eng.AppRuntime(context.Background(), "demo")
	if err != nil {
		t.Fatalf("AppRuntime on never-deployed app: %v", err)
	}
	if report.DesiredDeployment != "" || len(report.Services) != 0 {
		t.Fatalf("report = %+v, want empty (no desired state)", report)
	}
}

func TestAppRuntimeProjectsUnionTasksAndWatermark(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}

	report, err := h.eng.AppRuntime(ctx, "demo")
	if err != nil {
		t.Fatalf("AppRuntime: %v", err)
	}
	if report.DesiredDeployment != rec.ID {
		t.Fatalf("desired deployment = %q, want %q", report.DesiredDeployment, rec.ID)
	}
	if len(report.Services) != 1 {
		t.Fatalf("services = %d, want 1 (web)", len(report.Services))
	}
	web := report.Services[0]
	if web.Name != h.svc("web") {
		t.Fatalf("service name = %q, want %q", web.Name, h.svc("web"))
	}
	if web.Missing || web.Global {
		t.Fatalf("web missing=%v global=%v, want present replicated", web.Missing, web.Global)
	}
	if web.DeclaredReplicas != 1 {
		t.Fatalf("declared replicas = %d, want 1", web.DeclaredReplicas)
	}
	if web.ActualReplicas != 1 {
		t.Fatalf("actual replicas = %d, want 1 (fake seeds running task)", web.ActualReplicas)
	}
	if len(web.Tasks) == 0 {
		t.Fatal("tasks empty, want the running task")
	}
	running := false
	for _, tk := range web.Tasks {
		if tk.State == "running" && tk.DesiredState == "running" {
			running = true
			if tk.Image == "" {
				t.Fatal("task image not projected")
			}
		}
	}
	if !running {
		t.Fatalf("no running task in %+v", web.Tasks)
	}
}

func TestAppRuntimeMarksMissingDesiredService(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	// 实况消失（外部 docker service rm——swarm 真值缺位）。
	if err := h.sub.ServiceRemove(ctx, h.svc("web")); err != nil {
		t.Fatalf("remove service: %v", err)
	}

	report, err := h.eng.AppRuntime(ctx, "demo")
	if err != nil {
		t.Fatalf("AppRuntime: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(report.Services))
	}
	web := report.Services[0]
	if !web.Missing {
		t.Fatalf("missing flag = %v, want true (desired but absent)", web.Missing)
	}
	// absent 服务的声明值仍从期望快照投影（这是 UI「声明 vs 实况」对比的
	// 另一侧）。
	if web.DeclaredReplicas != 1 || web.ActualReplicas != 0 || len(web.Tasks) != 0 {
		t.Fatalf("absent service projection = %+v", web)
	}
}

func TestAppRuntimeListsExtraManagedServicesLast(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	// 期望集之外的受管服务（外部创建，带归属 label——scopeManagedServices
	// 的圈定域）。labels 尾参 deployment 占位值不影响 ServiceList 过滤。
	app := h.demoApp()
	labels, err := naming.ServiceLabels(app.TeamSlug, app.ProjectSlug, app.Name, "extra", rec.ID)
	if err != nil {
		t.Fatalf("service labels: %v", err)
	}
	extraName, nerr := naming.ServiceName(app.TeamSlug, app.ProjectSlug, app.Name, "extra")
	if nerr != nil {
		t.Fatalf("service name: %v", nerr)
	}
	if err := h.sub.ServiceCreate(ctx, ServiceSpec{
		Name:          extraName,
		Image:         "alpine:3",
		ServiceLabels: labels,
	}); err != nil {
		t.Fatalf("create extra service: %v", err)
	}

	report, err := h.eng.AppRuntime(ctx, "demo")
	if err != nil {
		t.Fatalf("AppRuntime: %v", err)
	}
	if len(report.Services) != 2 {
		t.Fatalf("services = %d, want 2 (web + extra)", len(report.Services))
	}
	if report.Services[0].Name != h.svc("web") {
		t.Fatalf("first service = %q, want desired-order web first", report.Services[0].Name)
	}
	extra := report.Services[1]
	if extra.Name != extraName {
		t.Fatalf("second service = %q, want extra %q", extra.Name, extraName)
	}
	if extra.Missing {
		t.Fatal("extra service must not be marked missing (actual-only)")
	}
	if extra.DeclaredReplicas != 0 {
		t.Fatalf("extra declared replicas = %d, want 0 (no desired spec)", extra.DeclaredReplicas)
	}
}

func TestAppRuntimePinsScalingReplicaOverrides(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	// 平台运行期副本覆盖（autoscaler 写通道）：声明副本钉 3——实况仍是
	// fake 播种的 1 个 running 任务（水位不撒谎，两侧各报各的）。
	if err := h.store.UpsertScalingReplicaOverride(ctx, state.ScalingReplicaOverride{
		AppID:        h.demoApp().ID,
		Service:      "web",
		DeploymentID: rec.ID,
		Replicas:     3,
		UpdatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	report, err := h.eng.AppRuntime(ctx, "demo")
	if err != nil {
		t.Fatalf("AppRuntime: %v", err)
	}
	if len(report.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(report.Services))
	}
	web := report.Services[0]
	if web.DeclaredReplicas != 3 {
		t.Fatalf("declared replicas = %d, want 3 (platform override pinned)", web.DeclaredReplicas)
	}
	if web.ActualReplicas != 1 {
		t.Fatalf("actual replicas = %d, want 1 (fake running task)", web.ActualReplicas)
	}
}

func TestAppRuntimeActualReplicasCountRunningDesiredOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	// 注入崩溃循环（failed 任务，desired 仍 running）——水位不涨，任务表
	// 如实携带（含历史）。
	h.sub.crashNewRunning(h.svc("web"), 2, h.clk.Now())

	report, err := h.eng.AppRuntime(ctx, "demo")
	if err != nil {
		t.Fatalf("AppRuntime: %v", err)
	}
	web := report.Services[0]
	if web.ActualReplicas != 1 {
		t.Fatalf("actual replicas = %d, want 1 (failed tasks excluded)", web.ActualReplicas)
	}
	if len(web.Tasks) != 3 {
		t.Fatalf("tasks = %d, want 3 (1 running + 2 failed history)", len(web.Tasks))
	}
}
