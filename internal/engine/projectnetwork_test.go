package engine

// IMPL-T15-1/OT-1 项目网连线与对账测试：成员服务双挂投影（守卫①/④的 spec
// 级断言）、attach/detach 重部署滚动语义、recon networks 扩面（孤儿网注入
// 一个对账周期内暴露——守卫②；期望项目网缺失披露）、项目网收敛 duty
// （缺失 ensure + 空网回收）、瞬态读错不结论、MoveApp 保留参与位。

import (
	"context"
	"errors"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// projectNetOf 推导 demo 应用当前项目的项目网名。
func (h *harness) projectNetOf(app state.App) string {
	h.t.Helper()
	name, err := naming.ProjectNetworkName(app.ProjectID)
	if err != nil {
		h.t.Fatalf("project network name: %v", err)
	}
	return name
}

// attachDemo 把 demo 应用置入项目网参与位（直调 state 原语——API 层组合
// 由 api 包测试覆盖）。
func attachDemo(t *testing.T, h *harness, attached bool) state.App {
	t.Helper()
	app := h.demoApp()
	updated, changed, err := h.store.SetAppProjectNetworkAttached(context.Background(), app.ID, attached, "tester", "")
	if err != nil {
		t.Fatalf("set project network attached=%v: %v", attached, err)
	}
	if !changed {
		t.Fatalf("precondition: participation flag already %v (fixture error)", attached)
	}
	return updated
}

// networkSpecOf 取服务实况的网络接入集。
func networkSpecOf(t *testing.T, h *harness, service string) []NetworkAttach {
	t.Helper()
	svc, err := h.sub.ServiceInspect(context.Background(), service)
	if err != nil {
		t.Fatalf("inspect %s: %v", service, err)
	}
	return svc.Networks
}

// TestProjectNetworkAttachmentProjectsMembersServices 守卫①④的 spec 级断言：
// 参与位在位的 app 成员服务**双挂**——app 私网（别名 = 短名）+
// 项目网（别名 = <app>-<service>，且只有这一个别名）；缺省（未参与）只挂
// app 私网（既有行为零变化）。
func TestProjectNetworkAttachmentProjectsMembersServices(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 基线：未参与的 app 首发——只挂 app 私网（零变化回归）。
	path := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(path)); final.Status != state.DeploySucceeded {
		t.Fatalf("baseline deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	baseline := networkSpecOf(t, h, h.svc("web"))
	if len(baseline) != 1 || baseline[0].Name != h.demoNet() || len(baseline[0].Aliases) != 1 || baseline[0].Aliases[0] != "web" {
		t.Fatalf("baseline networks = %+v, want the app private network only (default off)", baseline)
	}

	// attach + 参与变更重部署（API 层 attach 的组合腿：state 置位 → 入队）。
	app := attachDemo(t, h, true)
	projectNet := h.projectNetOf(app)
	rec, err := h.eng.EnqueueNetworkRedeploy(ctx, app.ID)
	if err != nil {
		t.Fatalf("enqueue network redeploy: %v", err)
	}
	if final := h.runToTerminal(h.storeDeployment(rec)); final.Status != state.DeploySucceeded {
		t.Fatalf("attach redeploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	attached := networkSpecOf(t, h, h.svc("web"))
	if len(attached) != 2 {
		t.Fatalf("attached networks = %+v, want exactly app net + project net", attached)
	}
	appNet, projectAttach := attached[0], attached[1]
	if appNet.Name != h.demoNet() || len(appNet.Aliases) != 1 || appNet.Aliases[0] != "web" {
		t.Fatalf("app net attach = %+v, want alias=web (short name only on the app private network)", appNet)
	}
	if projectAttach.Name != projectNet {
		t.Fatalf("project attach = %+v, want %s", projectAttach, projectNet)
	}
	// 守卫④：项目网别名只有 <app>-<service>（短名不在项目网出现）。
	if len(projectAttach.Aliases) != 1 || projectAttach.Aliases[0] != "demo-web" {
		t.Fatalf("project net aliases = %v, want [demo-web] only (cross-app short-name mixing is structurally impossible)", projectAttach.Aliases)
	}
	// 平台侧 ensure 面（attach 前置由 API 组合；此处 duty 亦幂等确保）。
	if !h.nets.has(projectNet) {
		t.Fatalf("project network %s not ensured", projectNet)
	}

	// detach + 重部署：投影移除项目网（摘网随 task template 变更滚动）。
	detached := attachDemo(t, h, false)
	if detached.ProjectNetworkAttached {
		t.Fatal("detach did not clear the flag")
	}
	rec2, err := h.eng.EnqueueNetworkRedeploy(ctx, app.ID)
	if err != nil {
		t.Fatalf("enqueue detach redeploy: %v", err)
	}
	if final := h.runToTerminal(h.storeDeployment(rec2)); final.Status != state.DeploySucceeded {
		t.Fatalf("detach redeploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	afterDetach := networkSpecOf(t, h, h.svc("web"))
	if len(afterDetach) != 1 || afterDetach[0].Name != h.demoNet() {
		t.Fatalf("networks after detach = %+v, want the app private network only", afterDetach)
	}
}

// TestAttachDetachRollMemberTasks attach/detach 的滚动语义（真机实证的假底座
// 同构）：网络集合变化 = task template 变化 → 任务替换（旧任务 desired=
// shutdown、新任务 running）。
func TestAttachDetachRollMemberTasks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("baseline deploy = %s, want succeeded", final.Status)
	}
	before := h.sub.tasksOf(t, h.svc("web"))
	if len(before) != 1 {
		t.Fatalf("baseline tasks = %+v, want one running task", before)
	}

	app := attachDemo(t, h, true)
	rec, err := h.eng.EnqueueNetworkRedeploy(ctx, app.ID)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if final := h.runToTerminal(h.storeDeployment(rec)); final.Status != state.DeploySucceeded {
		t.Fatalf("attach redeploy = %s, want succeeded", final.Status)
	}
	after := h.sub.tasksOf(t, h.svc("web"))
	if len(after) != 1 || after[0].ID == before[0].ID {
		t.Fatalf("attach did not roll tasks: before=%v after=%v (network set change must replace tasks)", before, after)
	}
}

// TestProjectNetworkReconDisclosesInjectedOrphanWithinOneScan 守卫②（机制
// 验收）：state 外 `fleetly-` 前缀受管网注入 → 一个对账周期内暴露
// （network.orphaned 事件 + 审计），持续形态节流，条件解除后可再报；
// 平台组件网（白名单）与项目网 label 对象不误报。
func TestProjectNetworkReconDisclosesInjectedOrphanWithinOneScan(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 平台组件网（白名单）与一只带项目网 label 的漏网对象（GC 分支专属）。
	h.nets.injectNetwork("fleetly-system", map[string]string{state.LabelManaged: state.ManagedLabelValue})
	h.nets.injectNetwork("fleetly-project-01JABCDE", map[string]string{
		state.LabelManaged:        state.ManagedLabelValue,
		state.LabelProjectNetwork: "01JABCDE000000000000000000",
	})
	// 无法归因的孤儿网注入（managed + fleetly- 前缀 + 无归属事实）。
	h.nets.injectOrphan("fleetly-orphan-drill-net")

	h.eng.SubstrateRecon(ctx)

	if got := countEventsByName(t, h, "network.orphaned"); got != 1 {
		t.Fatalf("network.orphaned events = %d, want exactly 1 within one recon period", got)
	}
	if got := countEventsByName(t, h, "network.missing"); got != 0 {
		t.Fatalf("network.missing events = %d, want 0 (no expected project networks)", got)
	}
	rows, err := h.store.RecentAudits(ctx, 100)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	auditFound := false
	for _, r := range rows {
		if r.Action == "reconcile.network_orphaned" && r.Target == "network:fleetly-orphan-drill-net" {
			auditFound = true
		}
	}
	if !auditFound {
		t.Fatalf("reconcile.network_orphaned audit row missing: %+v", rows)
	}
	// 组件网与项目网 label 对象不得误报（白名单/GC 分支纪律）。
	for _, r := range rows {
		if r.Action == "reconcile.network_orphaned" &&
			(r.Target == "network:fleetly-system" || r.Target == "network:fleetly-project-01JABCDE") {
			t.Fatalf("whitelisted/attributed network misjudged as orphan: %s", r.Target)
		}
	}

	// 节流：持续形态不重复发事件。
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.orphaned"); got != 1 {
		t.Fatalf("network.orphaned events = %d after rescan, want still 1 (throttle broken)", got)
	}

	// 孤儿消失（人工清理）→ 记忆清零；再次注入可再报。
	_ = h.nets.NetworkRemove(ctx, "fleetly-orphan-drill-net")
	h.eng.SubstrateRecon(ctx)
	h.nets.injectOrphan("fleetly-orphan-drill-net")
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.orphaned"); got != 2 {
		t.Fatalf("network.orphaned events = %d, want 2 (seen memory cleared after recovery)", got)
	}
}

// TestNetworkReconExemptsEnsuredTaskGroupNetworks IMPL-F1 回归①：ensure 过的
// task-group 长活网（公开与 internal 两变体）在连续多个对账周期内零 orphaned
// 披露、零 missing 误判、对象原位不动（修正前每拍误披露 network.orphaned——
// 披露噪声 + 记录虚报）。
func TestNetworkReconExemptsEnsuredTaskGroupNetworks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	publicNet, _, err := h.eng.EnsureTaskNetwork(ctx, "drillgrp", false, nil)
	if err != nil {
		t.Fatalf("ensure public task-group network: %v", err)
	}
	internalNet, _, err := h.eng.EnsureTaskNetwork(ctx, "drillgrpinternal", true, nil)
	if err != nil {
		t.Fatalf("ensure internal task-group network: %v", err)
	}
	if !h.nets.has(publicNet) || !h.nets.has(internalNet) {
		t.Fatalf("task-group networks missing from the substrate: %s / %s", publicNet, internalNet)
	}

	// 连续三个对账周期：零披露（豁免必须跨周期稳定，不是单拍巧合）。
	for range 3 {
		h.eng.SubstrateRecon(ctx)
	}
	if got := countEventsByName(t, h, "network.orphaned"); got != 0 {
		t.Fatalf("network.orphaned events = %d for ensured task-group networks, want 0 (exemption broken)", got)
	}
	if got := countEventsByName(t, h, "network.missing"); got != 0 {
		t.Fatalf("network.missing events = %d, want 0 (task-group networks are not project networks)", got)
	}
	// 披露面只读：豁免对象原位不动（长活语义——不随对账回收）。
	if !h.nets.has(publicNet) || !h.nets.has(internalNet) {
		t.Fatal("ensured task-group networks must stay in place across recon cycles")
	}
}

// TestNetworkReconStillDisclosesNonTaskGroupOrphans IMPL-F1 回归②：豁免不
// 回退机制验收——state 外注入的真正无法归因孤儿（非任务网）仍在一个对账
// 周期内被披露（事件 + 审计在）；连「冒名」形态（task-group 命名前缀 + 合法
// ref 尾段，但不带 LabelTaskGroup 归属 label）也不豁免——豁免锚是 label 归属
// 事实，不是命名前缀（TestProjectNetworkReconDisclosesInjectedOrphanWithinOneScan
// 的注入手法同源）。
func TestNetworkReconStillDisclosesNonTaskGroupOrphans(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ensured, _, err := h.eng.EnsureTaskNetwork(ctx, "drillgrp", true, nil)
	if err != nil {
		t.Fatalf("ensure task-group network: %v", err)
	}
	h.nets.injectOrphan("fleetly-orphan-drill-net")
	h.nets.injectNetwork("fleetly-taskgroup-impostor", map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
	})

	h.eng.SubstrateRecon(ctx)

	if got := countEventsByName(t, h, "network.orphaned"); got != 2 {
		t.Fatalf("network.orphaned events = %d, want 2 (generic orphan and name impostor both disclosed)", got)
	}
	rows, err := h.store.RecentAudits(ctx, 100)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	audited := map[string]bool{}
	for _, r := range rows {
		if r.Action == "reconcile.network_orphaned" {
			audited[r.Target] = true
		}
	}
	for _, target := range []string{"network:fleetly-orphan-drill-net", "network:fleetly-taskgroup-impostor"} {
		if !audited[target] {
			t.Fatalf("reconcile.network_orphaned audit row missing for %s", target)
		}
	}
	if audited["network:"+ensured] {
		t.Fatalf("ensured task-group network %s misjudged as orphan (exemption anchor is the label, and the label is present)", ensured)
	}
}

// TestProjectNetworkReconDisclosesMissingMemberNetwork state→swarm 方向：
// 成员项目的项目网在底座缺失（外部移除）→ network.missing 披露（一个对账
// 周期内暴露）；项目网收敛 duty 幂等重 ensure（派生修正）后不再重复披露。
func TestProjectNetworkReconDisclosesMissingMemberNetwork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := attachDemo(t, h, true)
	projectNet := h.projectNetOf(app)

	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.missing"); got != 1 {
		t.Fatalf("network.missing events = %d, want exactly 1 after the substrate removal", got)
	}
	if h.nets.has(projectNet) {
		t.Fatalf("recon must not auto-create the network (disclosure only; recreation is the convergence duty's role)")
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.missing"); got != 1 {
		t.Fatalf("network.missing events = %d after rescan, want still 1 (throttle broken)", got)
	}

	// 收敛 duty：幂等重 ensure（缺失自愈）→ 下一拍 recon 清记忆、零新事件。
	h.eng.ReconcileProjectNetworks(ctx)
	if !h.nets.has(projectNet) {
		t.Fatalf("convergence duty did not re-ensure the missing project network %s", projectNet)
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.missing"); got != 1 {
		t.Fatalf("network.missing events = %d after self-heal, want still 1", got)
	}
}

// TestReconcileProjectNetworksReclaimsDetachedEmptyNetwork 成员回收脚本：
// 有成员 → 保留；成员清空但仍有端点/服务引用 → 保留（下拍重试）；成员清空
// 且零端点零服务 → 回收（成员清空后项目网回收）。
func TestReconcileProjectNetworksReclaimsDetachedEmptyNetwork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := attachDemo(t, h, true)
	projectNet := h.projectNetOf(app)

	// 有成员：ensure 且不回收。
	h.eng.ReconcileProjectNetworks(ctx)
	if !h.nets.has(projectNet) {
		t.Fatalf("member project network %s not ensured", projectNet)
	}
	if h.nets.removeCount() != 0 {
		t.Fatalf("removes = %d while members exist, want 0", h.nets.removeCount())
	}

	// 成员清空但摘网重部署在途（端点/服务引用仍在）：保留。
	attachDemo(t, h, false)
	h.nets.setEndpoints(projectNet, 1, 1)
	h.eng.ReconcileProjectNetworks(ctx)
	if !h.nets.has(projectNet) || h.nets.removeCount() != 0 {
		t.Fatalf("project network reclaimed while endpoints/services still attached (unsafe reclaim)")
	}

	// 端点与服务引用清空：回收。
	h.nets.setEndpoints(projectNet, 0, 0)
	h.eng.ReconcileProjectNetworks(ctx)
	if h.nets.has(projectNet) {
		t.Fatalf("empty detached project network %s not reclaimed", projectNet)
	}
	if h.nets.removeCount() != 1 {
		t.Fatalf("removes = %d, want exactly 1", h.nets.removeCount())
	}
}

// TestNetworkReconSubstrateReadErrorDisclosesNothing 瞬态纪律：底座读错
// （List 失败）不下任何结论——零事件；错误清除后真实孤儿照常检出。
func TestNetworkReconSubstrateReadErrorDisclosesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.nets.injectOrphan("fleetly-orphan-drill-net")
	h.nets.failListErr = errors.New("docker daemon timeout (injected)")

	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.orphaned"); got != 0 {
		t.Fatalf("substrate error misjudged as orphan: %d events", got)
	}
	h.eng.ReconcileProjectNetworks(ctx)
	if h.nets.removeCount() != 0 {
		t.Fatalf("convergence duty acted on a failed read: removes=%d", h.nets.removeCount())
	}

	h.nets.failListErr = nil
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.orphaned"); got != 1 {
		t.Fatalf("network.orphaned events = %d after error cleared, want 1", got)
	}
}

// TestMoveAppPreservesProjectNetworkParticipation MoveApp 交叉语义（票面
// 裁决）：参与位是 app 级属性，改派保留；投影随**当前**项目——换名重部署
// 后的新服务双挂新项目网（别名 = <app>-<service> 不变），旧项目网在失去
// 最后一名成员后由收敛 duty 回收。
func TestMoveAppPreservesProjectNetworkParticipation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("baseline deploy = %s, want succeeded", final.Status)
	}
	app := attachDemo(t, h, true)
	oldProjectNet := h.projectNetOf(app)

	// 目标项目（独立夹具）+ 改派（state 原语；API 编排由 api 包测试覆盖）。
	target := testsupport.SeedProject(t, h.store)
	moved, err := h.store.MoveApp(ctx, app.ID, target.ID, "admin", "")
	if err != nil {
		t.Fatalf("move app: %v", err)
	}
	if !moved.ProjectNetworkAttached {
		t.Fatal("MoveApp must preserve the project-network participation flag (app-level intent follows the app)")
	}
	newProjectNet, err := naming.ProjectNetworkName(target.ID)
	if err != nil {
		t.Fatalf("new project network name: %v", err)
	}
	if newProjectNet == oldProjectNet {
		t.Fatal("fixture error: target project net equals the old one")
	}

	// 改派重部署（MoveApp 编排的引擎腿）：新命名上下文的服务双挂新项目网。
	rec, err := EnqueueMoveRedeploy(ctx, h.store, moved.ID)
	if err != nil {
		t.Fatalf("enqueue move redeploy: %v", err)
	}
	if final := h.runToTerminal(h.storeDeployment(rec)); final.Status != state.DeploySucceeded {
		t.Fatalf("move redeploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	newSvcName, err := naming.ServiceName(moved.TeamSlug, moved.ProjectSlug, moved.Name, "web")
	if err != nil {
		t.Fatalf("new service name: %v", err)
	}
	nets := networkSpecOf(t, h, newSvcName)
	if len(nets) != 2 || nets[1].Name != newProjectNet {
		t.Fatalf("moved service networks = %+v, want app net + new project net %s", nets, newProjectNet)
	}
	if len(nets[1].Aliases) != 1 || nets[1].Aliases[0] != "demo-web" {
		t.Fatalf("project alias after move = %v, want [demo-web] (app name unchanged)", nets[1].Aliases)
	}

	// 旧项目网：无成员 + 零端点 → 收敛 duty 回收。
	h.eng.ReconcileProjectNetworks(ctx)
	if h.nets.has(oldProjectNet) {
		t.Fatalf("old project network %s not reclaimed after the last member moved away", oldProjectNet)
	}
}

// TestDriftDetectsProjectNetworkDetach 漂移投影的网络面：外部把项目网接入
// 从服务 spec 摘掉（docker service update --network-rm 等价）→ 运行域漂移
// 检出（networks 字段 diff；别名集合形状）。
func TestDriftDetectsProjectNetworkDetach(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("baseline deploy = %s, want succeeded", final.Status)
	}
	app := attachDemo(t, h, true)
	rec, err := h.eng.EnqueueNetworkRedeploy(ctx, app.ID)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if final := h.runToTerminal(h.storeDeployment(rec)); final.Status != state.DeploySucceeded {
		t.Fatalf("attach redeploy = %s, want succeeded", final.Status)
	}
	// 平台形态基线：零漂移。
	report, err := h.eng.DriftShow(ctx, "demo")
	if err != nil {
		t.Fatalf("drift show: %v", err)
	}
	if report.Drifted {
		t.Fatalf("platform-shaped state reported drift: %+v", report.Services)
	}

	// 外部摘网（绕过平台写语义）：漂移必须可见（networks diff）。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) {
		spec.Networks = spec.Networks[:1]
	})
	report, err = h.eng.DriftShow(ctx, "demo")
	if err != nil {
		t.Fatalf("drift show after tamper: %v", err)
	}
	if !report.Drifted {
		t.Fatal("external project-network detach not detected (drift blind spot)")
	}
	found := false
	for _, sd := range report.Services {
		for _, d := range sd.Diff {
			if d.Field == "networks" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("drift report lacks the networks field diff: %+v", report.Services)
	}
}

// TestNetworkReconDisclosesDeletedAppNetworkLeak 已知泄漏类的机制证据：app
// 删除路径（reapDeletingApp）不回收 app 私网——deleted tombstone 的 app 网
// 会被 networks 对账面如实披露为孤儿（真实泄漏可见化即本机制价值；回收
// 路径挂账后续票）。同时验证 active app 的网不误报。
func TestNetworkReconDisclosesDeletedAppNetworkLeak(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("baseline deploy = %s, want succeeded", final.Status)
	}
	app := h.demoApp()
	appNet := h.demoNet()
	// 平台形态：active app 网在期望集 → 零孤儿。
	h.nets.injectNetwork(appNet, map[string]string{state.LabelManaged: state.ManagedLabelValue})
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.orphaned"); got != 0 {
		t.Fatalf("active app network misjudged as orphan: %d events", got)
	}

	// app 走完 tombstone 两拍（删除路径不回收网络——本票已知泄漏）。
	if err := h.store.MarkAppDeleting(ctx, app.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	if err := h.store.InTx(ctx, func(tx *state.Tx) error { return tx.MarkAppDeleted(ctx, app.ID) }); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "network.orphaned"); got != 1 {
		t.Fatalf("deleted app network leak not disclosed: %d events, want 1", got)
	}
}

// TestRollbackReplaysSnapshotNetworkFace 回滚 = 该 revision 期望态的**点时
// 重放**（网络面随快照——与 env 同口径）：attach 后的 revision 在 detach 后
// 仍可被回滚重放（项目网随快照重新挂上），当前参与位不变——下一次发布收敛
// 回当前参与位（「网络参与唯一改变路径 = attach/detach」在回滚语义下的边界，
// 与 env 回滚不复活当前值的既定口径一致；审查记录/runbook 载明）。
func TestRollbackReplaysSnapshotNetworkFace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A：基线（未参与）。
	baseline := h.runToTerminal(h.enqueue(h.writeCompose(composeV1)))
	if baseline.Status != state.DeploySucceeded {
		t.Fatalf("baseline deploy = %s, want succeeded", baseline.Status)
	}
	// B：attach 重部署（快照含项目网）。
	app := attachDemo(t, h, true)
	projectNet := h.projectNetOf(app)
	recB, err := h.eng.EnqueueNetworkRedeploy(ctx, app.ID)
	if err != nil {
		t.Fatalf("enqueue attach redeploy: %v", err)
	}
	finalB := h.runToTerminal(h.storeDeployment(recB))
	if finalB.Status != state.DeploySucceeded {
		t.Fatalf("attach redeploy = %s, want succeeded", finalB.Status)
	}
	// C：detach 重部署（项目网摘除）。
	attachDemo(t, h, false)
	recC, err := h.eng.EnqueueNetworkRedeploy(ctx, app.ID)
	if err != nil {
		t.Fatalf("enqueue detach redeploy: %v", err)
	}
	if finalC := h.runToTerminal(h.storeDeployment(recC)); finalC.Status != state.DeploySucceeded {
		t.Fatalf("detach redeploy = %s, want succeeded", finalC.Status)
	}
	if nets := networkSpecOf(t, h, h.svc("web")); len(nets) != 1 {
		t.Fatalf("post-detach networks = %+v, want app net only", nets)
	}

	// 回滚到 B：项目网随快照重放挂回；当前参与位仍为 off（不因回滚复活）。
	rollbackRec, err := EnqueueRollback(ctx, h.store, RollbackInput{
		AppName:          "demo",
		TargetRevisionID: finalB.RevisionID,
		Actor:            "human",
	})
	if err != nil {
		t.Fatalf("enqueue rollback: %v", err)
	}
	final := h.runToTerminal(rollbackRec)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("rollback = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	nets := networkSpecOf(t, h, h.svc("web"))
	if len(nets) != 2 || nets[1].Name != projectNet {
		t.Fatalf("rollback networks = %+v, want the snapshot's project net %s replayed", nets, projectNet)
	}
	row, err := h.store.GetAppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("read app: %v", err)
	}
	if row.ProjectNetworkAttached {
		t.Fatal("rollback must not flip the participation flag (the flag is current state, not revision state)")
	}
}

// storeDeployment 按 ID 取部署行（EnqueueNetworkRedeploy 返回 ID，测试面
// 与 enqueue 的返回形态对齐）。
func (h *harness) storeDeployment(id string) state.DeployRecord {
	h.t.Helper()
	row, err := h.store.GetDeployment(context.Background(), id)
	if err != nil {
		h.t.Fatalf("get deployment %s: %v", id, err)
	}
	return row
}

// tasksOf 取服务任务集（滚动断言面）。
func (f *fakeSubstrate) tasksOf(t *testing.T, service string) []TaskState {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	svc, ok := f.services[service]
	if !ok {
		t.Fatalf("service %s not found", service)
	}
	out := make([]TaskState, len(svc.tasks))
	copy(out, svc.tasks)
	return out
}
