package engine

// 项目网生命周期与对账（T 线 OT-1 / IMPL-T15-1 + DT-5 对账兜底守卫）。
//
// 三条机制边（各管一面，互不越权）：
//  1. attach 前置 ensure：ProjectsService attach RPC 经 api 端口调
//     EnsureProjectNetwork——幂等创建带自描述 label 的项目网 overlay
//     （managed=true + fleetly.project-network=<projectID>）；attach 后的
//     「参与变更重部署」由 EnqueueNetworkRedeploy 入队正常发布管线（新
//     revision 的成员服务双挂项目网，滚动收敛）；
//  2. 对账披露（substrateRecon 的 networks 面，reconNetworks）：读面列举
//     managed 网络 + state 推导期望集——state 外 `fleetly-` 前缀网 →
//     network.orphaned（只披露不删：无法归因 ⇒ 不静默删他人物件）；带归属
//     锚 label 的对象不进孤儿面（项目网漏网对象归第 3 条 GC 分支；task-group
//     长活网按 LabelTaskGroup 自描述锚豁免——IMPL-F1）；期望项目网缺失 →
//     network.missing（披露；修正归第 3 条 duty）。瞬态读错不结论
//     （services 面同纪律）；持续形态每进程只报一次（seen 记忆，恢复清零
//     可再报）。
//  3. 收敛（reconcileProjectNetworks duty，30s 频控）：有成员项目的项目网
//     幂等 ensure（缺失自愈——`network.missing` 的派生修正）+ 无成员、
//     零端点的项目网回收（成员清空/项目删除后的回收残留）。in-use 由底座
//     FailedPrecondition 拒绝兜底（真机实证），失败留下拍重试。
//
// 归属与参与分离（OT-1）：apps.project_id 是唯一 tenancy 轴；参与位
// （apps.project_network_attached）只表达网络参与，唯一改变路径 = attach/
// detach RPC；MoveApp 保留参与位、投影随当前项目（换名重部署天然重投影）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// projectNetworkSweepInterval 是项目网收敛 duty 的频控间隔（substrateRecon
// 同量级：成员清空后的回收延迟以 30s 计可接受）。
const projectNetworkSweepInterval = 30 * time.Second

// projectNetworkFor 返回 app 的项目网投影名（未参与 = 空串）。参与位在位的
// app 的项目 ID 形态违约（理论不可达——ULID 由状态库生成）视为运行时不可用
// 并显式失败（不静默丢投影——会让 attach 语义静默失效）。
func projectNetworkFor(app state.App) (string, error) {
	if !app.ProjectNetworkAttached {
		return "", nil
	}
	name, err := naming.ProjectNetworkName(app.ProjectID)
	if err != nil {
		return "", errorf("E_RUNTIME_UNAVAILABLE",
			"project network name for app %s (project %s): %v", app.Name, app.ProjectID, err)
	}
	return name, nil
}

// projectNetworkLabels 返回项目网对象的自描述 label 集（managed + 归属锚；
// 孤儿识别/GC 归因不靠前缀反解——id8 是有损截断）。
func projectNetworkLabels(projectID string) map[string]string {
	return map[string]string{
		state.LabelManaged:        state.ManagedLabelValue,
		state.LabelProjectNetwork: projectID,
	}
}

// EnsureProjectNetwork 幂等确保项目网 overlay 在位（attach RPC 前置；平台建、
// 缺失创建、并发竞态已存在即成功）。netSub 未装配显式报错（不静默跳过）。
func (e *Engine) EnsureProjectNetwork(ctx context.Context, projectID string) error {
	if e.netSub == nil {
		return errorf("E_RUNTIME_UNAVAILABLE",
			"network substrate is not assembled in this build: cannot ensure the project network")
	}
	name, err := naming.ProjectNetworkName(projectID)
	if err != nil {
		return errorf("E_RUNTIME_UNAVAILABLE", "project network name for project %s: %v", projectID, err)
	}
	return e.netSub.NetworkEnsureWithLabels(ctx, name, projectNetworkLabels(projectID))
}

// EnqueueNetworkRedeploy 为项目网参与变更后的 app 入队重部署（attach/
// detach 的生效腿；复用「最近 succeeded 部署的 compose/spec_hash」原语——
// 新 revision 的规划投影按 app 当前参与位与项目归属重建，服务滚动由发布
// 管内 applyDesired 承载）。无成功部署史返回 ErrNoRedeploySource（调用方
// 语义：无底座对象需重投影）。
func (e *Engine) EnqueueNetworkRedeploy(ctx context.Context, appID string) (string, error) {
	return enqueueRedeploy(ctx, e.store, appID, "project_network", "app.network_redeploy")
}

// ── 对账 networks 面（substrateRecon 扩面）──────────────────────────────────

// reconNetworks 是存在性对账的 networks 面（IMPL-T15-1；由 substrateRecon
// 同拍调用）。读错不结论（不事件、不清记忆、不动作——services 面同纪律）。
func (e *Engine) reconNetworks(ctx context.Context) {
	if e.netSub == nil {
		return // 未接线：对账面整体跳过（MetricsQuerier 空转同纪律）
	}
	actual, err := e.netSub.NetworkList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
	})
	if err != nil {
		e.log.Warn("engine: network recon list failed (transient, no conclusions)", "error", err)
		return
	}
	expected, projectNets, err := e.expectedNetworks(ctx)
	if err != nil {
		e.log.Warn("engine: network recon expectations failed (transient, no conclusions)", "error", err)
		return
	}
	actualNames := map[string]bool{}
	sort.Slice(actual, func(i, j int) bool { return actual[i].Name < actual[j].Name })
	// ① 孤儿方向：managed + fleetly- 前缀 + 不在期望集 + 无归属锚 label。
	//    项目网漏网对象归 GC 分支——归属明确，零端点即回收；task-group 长活
	//    网按 LabelTaskGroup 自描述锚豁免（IMPL-F1）——网本身无 state 行
	//    （不入台账，零成员 ensure 合法），state 期望集不可枚举，label 由
	//    唯一写者（EnsureTaskNetwork）落下、fleetly.* 命名空间用户不可伪造，
	//    是归属事实而非猜测；命名前缀（IsTaskGroupNetworkName）不作豁免
	//    依据——防异物冒名，冒名对象照常披露。
	orphanSet := map[string]bool{}
	for _, net := range actual {
		actualNames[net.Name] = true
		if !strings.HasPrefix(net.Name, naming.PlatformPrefix) {
			continue
		}
		if expected[net.Name] {
			continue
		}
		if net.Labels[state.LabelProjectNetwork] != "" {
			continue
		}
		if net.Labels[state.LabelTaskGroup] != "" {
			continue // task-group 长活网：自描述 label 归属明确，不进孤儿面
		}
		orphanSet[net.Name] = true
		if e.networkOrphanSeen[net.Name] {
			continue // 持续形态已报过：不重复（恢复清零可再报）
		}
		if err := e.reportNetworkOrphan(ctx, net); err != nil {
			e.log.Warn("engine: report orphan network", "network", net.Name, "error", err)
			continue
		}
		e.networkOrphanSeen[net.Name] = true
		e.log.Warn("engine: platform-prefixed managed network without state attribution (disclosed only, never deleted silently)",
			"network", net.Name, "driver", net.Driver, "containers", net.Containers)
	}
	for name := range e.networkOrphanSeen {
		if !orphanSet[name] {
			delete(e.networkOrphanSeen, name) // 恢复/归因成功：记忆清零可再报
		}
	}
	// ② 缺失方向：期望项目网不在底座（外部移除）→ 披露；修正归收敛 duty。
	missingSet := map[string]bool{}
	names := make([]string, 0, len(projectNets))
	for name := range projectNets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if actualNames[name] {
			continue
		}
		missingSet[name] = true
		if e.networkMissingSeen[name] {
			continue
		}
		if err := e.reportNetworkMissing(ctx, name, projectNets[name]); err != nil {
			e.log.Warn("engine: report missing project network", "network", name, "error", err)
			continue
		}
		e.networkMissingSeen[name] = true
		e.log.Warn("engine: expected project network absent from the substrate (the convergence duty re-ensures it)",
			"network", name, "project_id", projectNets[name])
	}
	for name := range e.networkMissingSeen {
		if !missingSet[name] {
			delete(e.networkMissingSeen, name)
		}
	}
}

// expectedNetworks 从 state 推导 networks 对账的期望面（读错即整体放弃本拍）。
// 返回：expected = 全部合法平台网名集（含项目网）；projectNets = 期望项目网
// → 项目 ID（缺失方向与收敛 duty 共用）。
//
//	app 网   = 非 deleted 生命周期（active/deleting——deleting 的服务仍在
//	           收敛中，网不得判孤儿）的 app 网；
//	库网     = 非 deleted 终态的库实例网；
//	项目网   = 有成员（active 且参与位在位）的项目网；期望名由项目 ID 推导。
//	组件网   = 装配层注入白名单（ingress/state 常量；生命周期归各组件 duty）。
//
// task-group 长活网不进期望集（网无 state 行——不入台账、零成员 ensure
// 合法，state 不可枚举）；豁免走 reconNetworks 的 LabelTaskGroup 自描述锚
// （IMPL-F1）。
func (e *Engine) expectedNetworks(ctx context.Context) (map[string]bool, map[string]string, error) {
	expected := map[string]bool{}
	for name := range e.platformNetworks {
		expected[name] = true
	}
	for _, lifecycle := range []state.AppLifecycle{state.LifecycleActive, state.LifecycleDeleting} {
		apps, err := e.store.ListAppsByLifecycle(ctx, lifecycle)
		if err != nil {
			return nil, nil, err
		}
		for _, app := range apps {
			name, nerr := naming.NetworkName(app.TeamSlug, app.ProjectSlug, app.Name)
			if nerr != nil {
				return nil, nil, fmt.Errorf("engine: app network name for %s: %w", app.Name, nerr)
			}
			expected[name] = true
		}
	}
	instances, err := e.store.ListDatabaseInstances(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, inst := range instances {
		if inst.State.Terminal() {
			continue // deleted 终态：网应已被回收（残留 = 孤儿，如实披露）
		}
		name, nerr := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
		if nerr != nil {
			return nil, nil, fmt.Errorf("engine: database network name for %s: %w", inst.Name, nerr)
		}
		expected[name] = true
	}
	members, err := e.store.ProjectNetworkMembers(ctx)
	if err != nil {
		return nil, nil, err
	}
	projectNets := map[string]string{}
	for _, m := range members {
		name, nerr := naming.ProjectNetworkName(m.ProjectID)
		if nerr != nil {
			return nil, nil, fmt.Errorf("engine: project network name for %s: %w", m.ProjectID, nerr)
		}
		expected[name] = true
		projectNets[name] = m.ProjectID
	}
	return expected, projectNets, nil
}

// reportNetworkOrphan 落孤儿网披露（事件 + 审计，同事务 fail-closed）。
// payload 只带事实字段（网络名/驱动/挂接容器数），零敏感材料。
func (e *Engine) reportNetworkOrphan(ctx context.Context, net NetworkState) error {
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		if err := appendEvents(ctx, tx, eventOf("network.orphaned", "network:"+net.Name,
			"network", net.Name,
			"driver", net.Driver,
			"containers", fmt.Sprint(net.Containers))); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:  "system",
			Action: "reconcile.network_orphaned",
			Target: "network:" + net.Name,
			Result: "ok",
			DiffSummary: state.DiffSummary("network", net.Name,
				"driver", net.Driver, "containers", net.Containers),
		})
	})
}

// reportNetworkMissing 落期望项目网缺失披露（事件 + 审计，同事务 fail-closed）。
func (e *Engine) reportNetworkMissing(ctx context.Context, name, projectID string) error {
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		if err := appendEvents(ctx, tx, eventOf("network.missing", "network:"+name,
			"network", name,
			"project_id", projectID)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      "reconcile.network_missing",
			Target:      "network:" + name,
			Result:      "ok",
			DiffSummary: state.DiffSummary("network", name, "project_id", projectID),
		})
	})
}

// ── 项目网收敛 duty（成员 ensure + 空网回收）────────────────────────────────

// ReconcileProjectNetworks 单步执行项目网收敛（测试与诊断显式入口——直通
// 频控闸；生产由 tick 周期驱动）。
func (e *Engine) ReconcileProjectNetworks(ctx context.Context) { e.reconcileProjectNetworks(ctx, true) }

// reconcileProjectNetworks 是 tick 的项目网收敛 duty（IMPL-T15-1）：
//  1. 有成员项目的项目网幂等 ensure（缺失自愈——network.missing 的派生修正）；
//  2. 无成员项目网回收：带自描述 label 归属、零挂接端点才删（in-use 由底座
//     拒绝兜底——真机实证 FailedPrecondition）；失败留下拍重试。
//
// 频控：非 force 形态受 projectNetNextAt 时间闸（tick goroutine 专用字段，
// substrateNextAt 同模式；重启即清零 = 重启后立即扫一拍）。读错不动作。
func (e *Engine) reconcileProjectNetworks(ctx context.Context, force bool) {
	now := e.now()
	if !force && now.Before(e.projectNetNextAt) {
		return
	}
	e.projectNetNextAt = now.Add(projectNetworkSweepInterval)
	if e.netSub == nil {
		return // 未接线：duty 空转
	}
	members, err := e.store.ProjectNetworkMembers(ctx)
	if err != nil {
		e.log.Warn("engine: project network members read failed", "error", err)
		return
	}
	wanted := map[string]string{} // 网络名 → 项目 ID
	for _, m := range members {
		name, nerr := naming.ProjectNetworkName(m.ProjectID)
		if nerr != nil {
			e.log.Warn("engine: project network name derive failed", "project", m.ProjectID, "error", nerr)
			continue
		}
		wanted[name] = m.ProjectID
	}
	for name, projectID := range wanted {
		if err := e.netSub.NetworkEnsureWithLabels(ctx, name, projectNetworkLabels(projectID)); err != nil {
			e.log.Warn("engine: project network ensure failed (next beat retries)", "network", name, "error", err)
		}
	}
	nets, err := e.netSub.NetworkList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
	})
	if err != nil {
		e.log.Warn("engine: project network list failed", "error", err)
		return
	}
	sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
	for _, net := range nets {
		projectID := net.Labels[state.LabelProjectNetwork]
		if projectID == "" {
			continue // 非项目网（或未标注对象——孤儿面覆盖，不越权）
		}
		if _, ok := wanted[net.Name]; ok {
			continue // 仍有成员：保留
		}
		st, ierr := e.netSub.NetworkInspect(ctx, net.Name)
		if ierr != nil {
			if errors.Is(ierr, ErrNetworkNotFound) {
				continue // 已消失：幂等
			}
			e.log.Warn("engine: project network inspect failed (next beat retries)", "network", net.Name, "error", ierr)
			continue
		}
		if st.Containers > 0 || st.Services > 0 {
			e.log.Info("engine: project network kept (no members but endpoints/services still attached; reclaim on a later beat)",
				"network", net.Name, "containers", st.Containers, "services", st.Services)
			continue
		}
		if rerr := e.netSub.NetworkRemove(ctx, net.Name); rerr != nil {
			e.log.Warn("engine: project network reclaim failed (next beat retries)", "network", net.Name, "error", rerr)
			continue
		}
		e.log.Info("engine: project network reclaimed (no members, no endpoints)",
			"network", net.Name, "project_id", projectID)
	}
}
