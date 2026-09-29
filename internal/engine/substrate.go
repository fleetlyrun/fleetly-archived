package engine

// 运行期 DB↔Swarm 存在性对账（T0-V2.2，调研 R2）：zane-ops 在每次部署
// 收尾做 cleanup_previous_unclean_deployments 对账；本平台推广为周期任务。
// 现状缺口：外部 docker service rm 后，平台 DB 仍声称 running，视图长期
// 说谎——只有控制面重启分类恢复会纠正，运行期无人对账。
//
// 定位与判据：
//   - 判据是 service 存在性（ServiceInspect），不是副本数——漂移 = 服务
//     存在但 spec 被改（drift.go 管）；本对账 = 服务整个不存在。
//     replicas=0 的 paused/保留现场应用 service 仍在，不误报；
//   - F11 补齐（settled 应用 drain 事件面，2026-09-21）：服务存在性之外
//     增「任务水位」判据——服务在、期望实例>0、running 任务=0（节点
//     drain / 任务被外部停掉的盲区：watchPostWindow 只看失败形任务且每
//     部署一次、watchBoundNode 只覆盖 releasing 窗，settled 稳态三路径
//     全漏）→ 降级披露 + 派生态 running → degraded；外部 scale=0（期望
//     实例=0）不在此列，判据仍是存在性；
//   - 候选集：派生态声称 running 的 active app（无在途部署——发布过程
//     本身就是期望态迁移，服务可能尚未创建），期望态 = 最新 succeeded
//     deployment 的 desired_spec 快照（与漂移检测同源，lastSucceededSpecs）；
//   - 处置只披露与修正视图：缺失发 app.substrate_missing 事件 + running →
//     down；drained 复用 app.degraded 映射事件（payload 带水位摘要）+
//     running → degraded。均不自动重建/重部署/删除——重建是部署链路
//     （用户/恢复器）的职责；drained 的恢复是自主的（节点回岗 swarm 自行
//     重调度），任务回岗后经 refreshDerivedState 单写点按 DB 事实重推导；
//   - 节流与幂等：持续缺失/持续无任务各只报一次（substrateMissingSeen /
//     substrateDrainedSeen 进程内记忆，恢复后清零可再报；重启清零 = 重报
//     一次——重复优于漏报，与 driftSeen 同语义）；substrate API 错误
//     （超时/不可达）≠ 服务缺失/任务全无，只进日志不发事件——不得把平台
//     自身故障当成 substrate 异常（那会把平台故障放大成假披露）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// substrateReconInterval 是存在性对账扫描的频控间隔（与漂移检测默认周期
// 同量级：外部 docker service rm 的发现延迟以 30s 计可接受；扫描只读
// 底座 + 单事务落库，幂等——重复执行只做确定性收敛）。
const substrateReconInterval = 30 * time.Second

// SubstrateRecon 单步执行存在性对账扫描（测试与诊断显式入口——直通频控
// 闸；生产由 tick 周期驱动）。
func (e *Engine) SubstrateRecon(ctx context.Context) { e.substrateRecon(ctx, true) }

// substrateRecon 是 tick 的存在性对账步（T0-V2.2/R2）。IMPL-T15-1 起
// 含 networks 面（reconNetworks——孤儿网/缺失项目网披露；票面 DT-5 对账
// 兜底守卫）。频控：非 force 形态受 substrateScanGate 时间闸（tick
// goroutine 专用，scanGate 单点见 disclosure.go；重启即清零 = 重启后立即
// 扫一拍）。
func (e *Engine) substrateRecon(ctx context.Context, force bool) {
	if !e.substrateScanGate.due(e.now(), force, substrateReconInterval) {
		return
	}
	// networks 面先行（读面披露——与 services 面共享同一频控闸；读错各自
	// 独立消化，互不阻塞）。
	e.reconNetworks(ctx)
	// tasks 面（DT-5 对账兜底守卫）：孤儿 task 服务披露 + 回收（与 networks
	// 面同拍；读错不结论）。
	e.reconTasks(ctx)
	apps, err := e.store.ListActiveApps(ctx)
	if err != nil {
		e.log.Warn("engine: substrate recon list apps", "error", err)
		return
	}
	inFlight := map[string]bool{}
	if rows, err := e.store.ListNonTerminalDeployments(ctx); err != nil {
		e.log.Warn("engine: substrate recon in-flight check", "error", err)
		return
	} else {
		for _, r := range rows {
			inFlight[r.AppID] = true
		}
	}
	for _, app := range apps {
		if inFlight[app.ID] {
			continue // 发布过程本身就是期望态迁移：服务可能尚未创建，不判缺失
		}
		// 挂起应用（app Stop/Start，00028 位）走专用排水腿：受管长驻服务
		// 副本压到 0（状态驱动渲染——期望形态，不是故障披露），缺失不补建、
		// 多余不回收；派生基线随排水刷新（suspended 直投影）。写通道在
		// suspend.go（convergescan 白名单行：无条件副本清零保持器，非收敛
		// 对账——scaleToZero 同族）。
		if app.Suspended {
			e.drainSuspendedApp(ctx, app.ID, app.Name)
			continue
		}
		derived, err := e.store.GetAppDerivedState(ctx, app.ID)
		if err != nil {
			if errors.Is(err, state.ErrAppNotFound) {
				continue // 已删：无视图可修
			}
			e.log.Warn("engine: substrate recon read derived state", "app", app.Name, "error", err)
			continue
		}
		if derived != DerivedRunning && derived != DerivedDegraded {
			continue // 只对声称 running/degraded 的 app 对账（down/blocked 各有归属路径）；
			// degraded 入选是 F11 恢复腿：drain 降级后的任务回岗自愈在派生态
			// 回 running 之前，本对账是唯一周期观察者（CAS 门保证只读不越权）。
		}
		e.reconAppSubstrate(ctx, app.ID, app.Name, derived)
	}
}

// reconAppSubstrate 核实单个 running 应用的服务存在性与任务水位：任一期望
// 服务缺失 → 缺失事件 + 派生态修正（running → down）；服务齐整但期望实例
// 全部无 running 任务（F11：settled 应用被节点 drain / 任务被外部停掉的
// 对账盲区——服务对象仍在，存在性判据探不到）→ 降级披露 + 派生态修正
// （running → degraded）；全部在岗 → 清两类记忆（服务恢复/任务回岗后可
// 再报）；底座读错误 → 只记日志（瞬态，下一拍重核）。
func (e *Engine) reconAppSubstrate(ctx context.Context, appID, appName, derived string) {
	source, specs, err := e.lastSucceededSpecs(ctx, appID)
	if err != nil {
		e.log.Warn("engine: substrate recon read expectations", "app", appName, "error", err)
		return
	}
	if source == nil {
		return // 无成功部署：无期望态（判据无从建立，不在此扩权）
	}
	var missing []string
	// drained 集：服务名 → 任务水位摘要（期望实例>0 且 running=0 的服务）。
	drained := map[string]string{}
	for i := range specs {
		st, err := e.sub.ServiceInspect(ctx, specs[i].Name)
		if err != nil {
			if errors.Is(err, ErrServiceNotFound) {
				missing = append(missing, specs[i].Name)
				continue
			}
			// substrate API 错误（超时/不可达）≠ 服务缺失：不判缺失、不发
			// 事件、不改派生态，只进日志，下一拍重核。整应用放弃本拍——
			// 部分服务未核实就下缺失结论同样构成假披露。
			e.log.Warn("engine: substrate recon inspect failed (transient, not counted as missing)",
				"app", appName, "service", specs[i].Name, "error", err)
			return
		}
		// 任务水位判据只对「期望实例>0」的服务生效（实况副本/ global 位）：
		// 外部 scale=0 与保留现场形态的判据仍是存在性（服务在 = 不误报，
		// 既有测试钉死）；global 服务期望 = 每节点一任务，至少一。
		if !st.Global && st.Replicas == 0 {
			continue
		}
		tasks, err := e.sub.TaskList(ctx, specs[i].Name)
		if err != nil {
			// 与 inspect 同纪律：瞬态错误不下任何结论，整应用放弃本拍。
			e.log.Warn("engine: substrate recon task list failed (transient, not counted as drained)",
				"app", appName, "service", specs[i].Name, "error", err)
			return
		}
		if taskWatermark(tasks) > 0 {
			continue
		}
		drained[specs[i].Name] = taskStateSummary(st, tasks)
	}
	if len(missing) > 0 {
		e.substrateDrainedSeen.clear(appID) // 缺失揭示吞并 drained 形态（视图修正优先级 down > degraded）
		if e.substrateMissingSeen.reported(appID) {
			return // 持续缺失已报过：不重复发事件（节流；派生态已修，扫描候选过滤同样挡住重入）
		}
		if err := e.reportSubstrateMissing(ctx, appID, appName, *source, missing); err != nil {
			if errors.Is(err, state.ErrAppDerivedStateConflict) {
				return // 并发翻转已离开 running：幂等跳过，下一拍候选过滤生效
			}
			e.log.Warn("engine: substrate recon report", "app", appName, "error", err)
			return // 未记缺失记忆：下一拍重试（事件与修正同事务，失败即整体未生效）
		}
		e.substrateMissingSeen.mark(appID)
		e.log.Warn("engine: running app's service(s) absent from the substrate (view corrected to down)",
			"app", appName, "deployment", source.ID, "missing", strings.Join(missing, ","))
		return
	}
	// 服务齐整：清缺失记忆（服务恢复后可再报——非永久静音）。
	e.substrateMissingSeen.clear(appID)
	if len(drained) > 0 {
		if e.substrateDrainedSeen.reported(appID) {
			return // 持续无任务已报过：节流（与缺失记忆同语义）
		}
		if err := e.reportSubstrateDrained(ctx, appID, appName, *source, drained); err != nil {
			if errors.Is(err, state.ErrAppDerivedStateConflict) {
				return // 并发翻转已离开 running：幂等跳过
			}
			e.log.Warn("engine: substrate recon report drained", "app", appName, "error", err)
			return
		}
		e.substrateDrainedSeen.mark(appID)
		e.log.Warn("engine: running app's services have zero running tasks (view corrected to degraded)",
			"app", appName, "deployment", source.ID, "services", strings.Join(drainedSummary(drained), ","))
		return
	}
	// 全部在岗：任务回岗 → 清 drained 记忆，派生态按 DB 事实重推导（单写
	// 点 refreshDerivedState：drain 期间的 degraded 修正不在 DB 事实里，重
	// 推导即回 running，映射事件 app.recovered 由其自带；若 degraded 来自
	// DB 事实〔如窗后告警旗标〕，重推导维持 degraded——不越权翻转他人语义，
	// 这也是 degraded 候选在健康拍重复刷新无害的原因）。
	if e.substrateDrainedSeen.reported(appID) || derived == DerivedDegraded {
		e.substrateDrainedSeen.clear(appID)
		if err := e.refreshDerivedState(ctx, appID, appName); err != nil {
			e.log.Warn("engine: substrate recon refresh derived state (drain recovery)",
				"app", appName, "error", err)
		}
	}
}

// taskWatermark 统计 running 任务数（State 与 DesiredState 双 running——
// drain 后旧任务 shutdown / 新任务 pending 均不计入在岗水位）。
func taskWatermark(tasks []TaskState) int {
	n := 0
	for _, t := range tasks {
		if t.State == "running" && t.DesiredState == "running" {
			n++
		}
	}
	return n
}

// taskStateSummary 是 drained 披露的每服务水位摘要（诚实事实：期望副本、
// 任务总数与状态分布——只陈述观察，不臆断成因是 drain 还是节点失联）。
func taskStateSummary(st ServiceState, tasks []TaskState) string {
	counts := map[string]int{}
	for _, t := range tasks {
		counts[t.State]++
	}
	states := make([]string, 0, len(counts))
	for _, s := range sortedStateNames(counts) {
		states = append(states, fmt.Sprintf("%s=%d", s, counts[s]))
	}
	return fmt.Sprintf("desired=%d tasks=%d states=%s", st.Replicas, len(tasks), strings.Join(states, "+"))
}

// sortedStateNames 是状态计数键的字典序（摘要可重放）。
func sortedStateNames(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// drainedSummary 把 per-service 摘要压成稳定序的行集（字典序——披露可重放）。
func drainedSummary(drained map[string]string) []string {
	keys := make([]string, 0, len(drained))
	for k := range drained {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+" ("+drained[k]+")")
	}
	return out
}

// reportSubstrateMissing 落缺失事件与审计，并把派生态修正为如实值
// （running → down）。事件、审计与派生 CAS 同事务（fail-closed：任一失败
// 整体回滚，不产生只有事件没有修正的半程披露，也不产生只有修正没有披露
// 的静默改写）。
func (e *Engine) reportSubstrateMissing(ctx context.Context, appID, appName string,
	source state.DeployRecord, missing []string) error {
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetAppDerivedState(ctx, appID)
		if err != nil {
			if errors.Is(err, state.ErrAppNotFound) {
				return nil // app 已删：无视图可修
			}
			return err
		}
		if cur != DerivedRunning {
			return nil // 并发翻转已离开 running：离开本步范围（扫描时与写入时双重校验）
		}
		if err := tx.SetAppDerivedState(ctx, appID, cur, DerivedDown); err != nil {
			return err
		}
		// 事件 + 审计配对经 disclosure.go 单点（派生态 CAS 之上的同事务组合）。
		return writeDisclosure(ctx, tx, disclosure{
			eventCode:   "app.substrate_missing",
			subject:     "app:" + appName,
			payload:     []string{"app", appName, "app_id", appID, "desired_deployment", source.ID, "missing_services", strings.Join(missing, ",")},
			auditAction: "app.substrate_missing",
			auditTarget: "app:" + appName,
			auditDiff: state.DiffSummary("app", appName, "deployment", source.ID,
				"missing_services", strings.Join(missing, ",")),
		})
	})
}

// reportSubstrateDrained 落 F11 的降级披露（settled 应用「服务在、任务全
// 无」）与审计，并把派生态修正为 degraded（running → degraded，CAS 门与
// 缺失修正同款）。事件复用 app.degraded 映射事件（事件码只增纪律——本披露
// 的语义本就是「应用不再健康服务」，与既有 degraded 事件面一致），payload
// 携带 per-service 水位摘要承载 drain 语义（只陈述观察不臆断成因）；审计
// action 用机制名 app.substrate_drained（审计面非事件码表管辖面）。事件、
// 审计与派生 CAS 同事务（fail-closed，与 reportSubstrateMissing 同纪律）。
//
// 恢复路径不在此函数：任务回岗时 reconAppSubstrate 清 drained 记忆并经
// refreshDerivedState（单写点）按 DB 事实重推导——drain 修正不在事实里，
// 重推导即回 running 并自带 app.recovered。
func (e *Engine) reportSubstrateDrained(ctx context.Context, appID, appName string,
	source state.DeployRecord, drained map[string]string) error {
	rows := drainedSummary(drained)
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetAppDerivedState(ctx, appID)
		if err != nil {
			if errors.Is(err, state.ErrAppNotFound) {
				return nil // app 已删：无视图可修
			}
			return err
		}
		if cur != DerivedRunning {
			return nil // 已非 running（degraded/blocked/down 各有归属路径）：不覆盖他人语义
		}
		if err := tx.SetAppDerivedState(ctx, appID, cur, DerivedDegraded); err != nil {
			return err
		}
		// 事件 + 审计配对经 disclosure.go 单点（派生态 CAS 之上的同事务组合）。
		return writeDisclosure(ctx, tx, disclosure{
			eventCode:   "app.degraded",
			subject:     "app:" + appName,
			payload:     []string{"app", appName, "app_id", appID, "desired_deployment", source.ID, "detection", "substrate_recon", "services", strings.Join(rows, "; ")},
			auditAction: "app.substrate_drained",
			auditTarget: "app:" + appName,
			auditDiff: state.DiffSummary("app", appName, "deployment", source.ID,
				"services", strings.Join(rows, "; ")),
		})
	})
}
