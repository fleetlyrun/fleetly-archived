package engine

// releasing 阶段语义（T2.11 窗口与失败语义，release-semantics §2.2/§2.3/
// §2.5/§2.6）：L2 看门狗、blocked_waiting 子状态（场景 15/16）、Swarm pause
// 判定的失败分类（场景 3/4/5/6）、健康门与切流、失败分流（first_healthy_at：
// 未切流=同记录归位 restore；已切流=verdict=unstable 告警）、首发失败
// scale=0（场景 14）、stop-first 强制归位与停机账（场景 13）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// evaluateReleasing 推进一个 releasing 周期。
func (e *Engine) evaluateReleasing(ctx context.Context, rec state.DeployRecord) error {
	// cancel 准入：releasing 且未切流可取消（曾健康 409）。
	if rec.CancelRequested {
		if !rec.FirstHealthyAt.IsZero() {
			return e.rejectCancel(ctx, rec)
		}
		return e.cancelDeployment(ctx, rec)
	}

	// DT-4 init 子相位：晋级前一次性作业（迁移先于新代码）——job 全过才
	// 进长驻对账/健康门。分支先于 watchBoundNode：相位列单值约束下 init
	// 相位优先（init 期节点不可用由 job 看门狗兜底，cancel 准入在上方已
	// 处置；无 init 模板的部署相位恒空、不触达本分支——守卫⑤）。
	if rec.Phase == state.PhaseInitJobs {
		return e.evaluateInitJobs(ctx, rec)
	}

	specs, err := e.decodeSpecs(rec)
	if err != nil {
		return e.failTransitionErr(ctx, rec, errorf("E_DEPLOY_INTERRUPTED",
			"desired-state snapshot unreadable (%v): manual intervention required", err))
	}

	// 绑定节点观测（场景 15/16）：DOWN → blocked_waiting（看门狗暂停、可
	// cancel）；REMOVED → 直接失败。仅钉住应用检查（自由调度应用无此路径）。
	if err := e.watchBoundNode(ctx, &rec); err != nil {
		return err
	}
	// blocked 维持态提前返回（MG-2，场景 15「暂停计时、恢复续跑重新起算」）：
	// 本拍 Preflight 仍失败、部署处于 blocked_waiting——L2 看门狗暂停计时，
	// 下方看门狗判定对本部署不可达（旧缺陷：评估继续下行，节点持续 DOWN
	// 越过 deadline 后每拍触发 E_SCHEDULER_PENDING_TIMEOUT 假失败，把
	// 「等待节点恢复」误判成「发布超时」）。节点恢复拍由 watchBoundNode 内
	// resumeFromBlocked 清除 phase 并重臂看门狗后，评估才继续下行。
	if rec.Phase == state.PhaseBlockedWaiting {
		return nil
	}

	// 逐服务实况 → 判定。E5 Cron：全 cron 应用（无长驻服务，Job 模板已被
	// decodeSpecs 过滤）specs 为空——allSwitched 判真直接进观察窗（发布
	// 语义上「没有长驻服务要等健康」，只声明的 cron 面不阻塞部署终态）。
	anyPaused := false
	allSwitched := true
	anyNewRan := false
	for i := range specs {
		spec := specs[i]
		svc, err := e.sub.ServiceInspect(ctx, spec.Name)
		if err != nil {
			if errors.Is(err, ErrServiceNotFound) {
				// 服务在发布中被外力删除：按更新失败处置（归位会重建）。
				anyPaused = true
				allSwitched = false
				break
			}
			return e.transientOr(ctx, rec, err)
		}
		tasks, err := e.sub.TaskList(ctx, spec.Name)
		if err != nil {
			return e.transientOr(ctx, rec, err)
		}
		if svc.UpdateState == "paused" {
			anyPaused = true
		}
		if countNewRunning(tasks, spec.Image) >= desiredReplicasOf(spec) &&
			(svc.UpdateState == "" || svc.UpdateState == "completed") {
			// 该服务已切换（Swarm 健康门通过 + 平台复核副本水位）。
			continue
		}
		allSwitched = false
		if countNewEverRan(tasks, spec.Image) > 0 {
			anyNewRan = true
		}
	}

	// Swarm pause = 更新失败（D-REL-1：旧任务保留服务、平台唯一决策者）。
	if anyPaused {
		code, detail := e.classifyUpdateFailure(ctx, specs)
		return e.failUnswitchedOrSwitched(ctx, rec, code, detail)
	}

	// L2 看门狗：deadline 已过 → 滞留 PENDING / 健康门不通过。
	if !rec.WatchdogDeadlineAt.IsZero() && e.now().After(rec.WatchdogDeadlineAt) {
		code := "E_HEALTH_TIMEOUT"
		if !anyNewRan {
			code = "E_SCHEDULER_PENDING_TIMEOUT"
		}
		return e.failUnswitchedOrSwitched(ctx, rec, code,
			fmt.Sprintf("deploy watchdog timeout (%s): new version did not pass the health gate within budget", e.cfg.DeployTimeout))
	}

	// 全部服务切换 → 切流点（首个目标实例健康即起算观察窗，§2.2 L3；
	// specs 为空 = 全 cron 应用，无长驻服务可等，同样进观察窗）。
	if allSwitched {
		return e.enterObserving(ctx, rec)
	}
	return nil
}

// watchBoundNode 观测钉住应用的绑定节点（releasing 全程）：DOWN 进入
// blocked_waiting（暂停看门狗、可 cancel、app blocked）；恢复后重新起算；
// REMOVED 直接失败（数据安全优先，场景 16）。返回值 error 仅携带已处置的
// 终态分支（失败路径），暂态由内部消化。
func (e *Engine) watchBoundNode(ctx context.Context, rec *state.DeployRecord) error {
	err := e.resolver.Preflight(ctx, rec.AppID)
	if err == nil {
		if rec.Phase == state.PhaseBlockedWaiting {
			return e.resumeFromBlocked(ctx, rec)
		}
		return nil
	}
	var ae *apperr.Error
	if !asAppErr(err, &ae) || ae == nil {
		return e.transientOr(ctx, *rec, err)
	}
	switch ae.Code() {
	case "E_PLACEMENT_NODE_UNAVAILABLE", "E_PLACEMENT_NO_ELIGIBLE_NODE":
		return e.enterBlockedWaiting(ctx, rec)
	case "E_PLACEMENT_NODE_GONE":
		return e.failUnswitchedOrSwitched(ctx, *rec, "E_PLACEMENT_NODE_GONE",
			"bound node was removed (data safety first; app stays blocked(node_gone) awaiting manual rebind)")
	default:
		// 卷前哨等确定性失败：直接走失败分流。
		return e.failUnswitchedOrSwitched(ctx, *rec, ae.Code(), ae.Message())
	}
}

// enterBlockedWaiting 进入 blocked_waiting 子状态（幂等）：看门狗暂停计时、
// 可 cancel、应用 blocked（场景 15）。
func (e *Engine) enterBlockedWaiting(ctx context.Context, rec *state.DeployRecord) error {
	if rec.Phase == state.PhaseBlockedWaiting {
		return nil
	}
	phase := state.PhaseBlockedWaiting
	if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{Phase: &phase}); err != nil {
		return err
	}
	rec.Phase = phase
	if err := e.store.InTx(ctx, func(tx *state.Tx) error {
		return appendEvents(ctx, tx,
			deploymentEvent("deployment.recovery_blocked", rec.ID,
				"reason", "bound_node_down"),
			placementEvent("placement.blocked", rec.AppName, rec.AppID, "node_down"))
	}); err != nil {
		return err
	}
	return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
}

// resumeFromBlocked 节点恢复：退出子状态、看门狗重新起算（场景 15「恢复后
// 续跑并重新起算」）、placement.recovered 事件。
func (e *Engine) resumeFromBlocked(ctx context.Context, rec *state.DeployRecord) error {
	phase := ""
	deadline := e.now().Add(e.cfg.DeployTimeout)
	if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{
		Phase:              &phase,
		WatchdogDeadlineAt: &deadline,
	}); err != nil {
		return err
	}
	rec.Phase = phase
	rec.WatchdogDeadlineAt = deadline
	if err := e.store.InTx(ctx, func(tx *state.Tx) error {
		return appendEvents(ctx, tx, placementEvent("placement.recovered", rec.AppName, rec.AppID, "node_ready"))
	}); err != nil {
		return err
	}
	return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
}

// enterObserving 切流：first_healthy_at 落定、事件 healthy/switched/
// observe_started、观察窗起点写入。转换经单写点（T0-V2.2）：releasing →
// observing、切流/观察窗标记与三个披露事件同一事务（事件序列与既有形态
// 一致：healthy → switched → observe_started）。
func (e *Engine) enterObserving(ctx context.Context, rec state.DeployRecord) error {
	now := e.now()
	observeStart := now
	var firstHealthy *time.Time
	healthy := rec.FirstHealthyAt.IsZero()
	if healthy {
		firstHealthy = &now
	}
	var events []state.Event
	if healthy {
		events = append(events,
			deploymentEvent("deployment.healthy", rec.ID),
			deploymentEvent("deployment.switched", rec.ID))
	}
	events = append(events, deploymentEvent("deployment.observe_started", rec.ID,
		"window_seconds", fmt.Sprintf("%d", int64(e.cfg.ObserveWindow/time.Second))))
	if err := e.store.EnterPhase(ctx, rec.ID, state.DeployReleasing, state.DeployObserving, state.DeploymentPatch{
		FirstHealthyAt:   firstHealthy,
		ObserveStartedAt: &observeStart,
	}, events...); err != nil {
		return err
	}
	rec.Status = state.DeployObserving
	rec.ObserveStartedAt = observeStart
	if healthy {
		rec.FirstHealthyAt = now
	}
	// 路由发布挂点（T2.15；architecture §2.5 不变量）：严格晚于健康门
	//（切流/observe_started 之后）——端点入集晚于 healthy 的 V1/B3 语义。
	// 失败只告警，不影响部署状态机（publishRoutes 内部消化）。
	e.publishRoutes(ctx, rec)
	return nil
}

// failUnswitchedOrSwitched 是失败分流唯一入口（D-REL-4，判据 =
// first_healthy_at）：
//   - 未切流（null）：同记录归位 recovery=replay（重放最后有效 revision；
//     首发无版本 → scale=0 保留现场）；stop-first 停机如实累计；
//   - 已切流：verdict=unstable 终态告警（不建新 deployment——默认只告警，
//     D-REL-6；app=degraded）。
//
// kind=rollback 分流到 failRollbackDeployment（D-REL-10：回滚失败不再二次
// 自动恢复——归位/重放都省略）。
func (e *Engine) failUnswitchedOrSwitched(ctx context.Context, rec state.DeployRecord, code, detail string) error {
	if rec.Kind == kindRollback {
		return e.failRollbackDeployment(ctx, rec, code, detail)
	}
	if !rec.FirstHealthyAt.IsZero() {
		return e.failSwitched(ctx, rec, code, detail)
	}
	return e.failUnswitched(ctx, rec, code, detail)
}

// failSwitched 已切流失败（场景 7/8）：deployment failed（verdict=unstable）
// + app degraded 告警；不自动回滚。kind=rollback 见 failRollbackDeployment。
func (e *Engine) failSwitched(ctx context.Context, rec state.DeployRecord, code, detail string) error {
	if rec.Kind == kindRollback {
		return e.failRollbackDeployment(ctx, rec, code, detail)
	}
	verdict := state.VerdictUnstable
	if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{Verdict: &verdict}); err != nil {
		return err
	}
	if err := e.failTransition(ctx, rec, code, detail); err != nil {
		return err
	}
	return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
}

// failUnswitched 未切流失败：先归位（或首发 scale=0）再落 failed 终态。
func (e *Engine) failUnswitched(ctx context.Context, rec state.DeployRecord, code, detail string) error {
	stopFirst := e.deploymentIsStopFirst(ctx, rec)
	previous, err := e.lastActiveSnapshot(ctx, rec)
	if err != nil {
		return err
	}

	if previous == nil {
		// 首发失败（无有效版本可归位）：scale=0 保留现场（D-REL-5，
		// 场景 14）。
		specs, derr := e.decodeSpecs(rec)
		if derr == nil {
			if serr := e.scaleToZero(ctx, rec, specs); serr != nil {
				return e.failCriticalRestore(ctx, rec, detail, serr, stopFirst)
			}
		}
		halted := true
		if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{SubstrateHalted: &halted}); err != nil {
			return err
		}
		if err := e.store.InTx(ctx, func(tx *state.Tx) error {
			return appendEvents(ctx, tx, deploymentEvent("deployment.substrate_halted", rec.ID, "code", code))
		}); err != nil {
			return err
		}
		if err := e.failTransition(ctx, rec, code, detail); err != nil {
			return err
		}
		return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
	}

	// stop-first：停机自判定起持续到归位完成（如实账，§2.6）。
	if stopFirst {
		started := e.now()
		if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{DowntimeStartedAt: &started}); err != nil {
			return err
		}
		rec.DowntimeStartedAt = started
	}

	// 归位 = 最后有效 revision 的完整快照重放（禁 --force；同内容重放任务
	// 零替换，Spike B2）。
	if err := e.restoreSnapshot(ctx, rec, previous); err != nil {
		// 恢复失败 = critical，不再二次自动（D-REL-10）。
		restoreCode := ""
		if stopFirst {
			restoreCode = "E_DEPLOY_DOWNTIME_FAILED"
		}
		return e.failCriticalRestore(ctx, rec, detail, err, stopFirst, restoreCode)
	}

	// 归位完成：recovery=replay 记录在同一 deployment 条目内（D-REL-7）。
	recovery := state.RecoveryReplay
	patch := state.DeploymentPatch{Recovery: &recovery}
	if !rec.DowntimeStartedAt.IsZero() {
		ended := e.now()
		ms := ended.Sub(rec.DowntimeStartedAt).Milliseconds()
		patch.DowntimeEndedAt = &ended
		patch.DowntimeMS = &ms
	}
	if err := e.store.UpdateDeployment(ctx, rec.ID, patch); err != nil {
		return err
	}
	if err := e.failTransition(ctx, rec, code, detail); err != nil {
		return err
	}
	return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
}

// failCriticalRestore 归位自身失败：critical 终态、不再二次自动恢复
// （D-REL-10；stop-first 路径用 E_DEPLOY_DOWNTIME_FAILED，§2.6）。
func (e *Engine) failCriticalRestore(ctx context.Context, rec state.DeployRecord, detail string, restoreErr error, stopFirst bool, restoreCode ...string) error {
	final := "E_ROLLBACK_FAILED"
	if len(restoreCode) > 0 && restoreCode[0] != "" {
		final = restoreCode[0]
	} else if stopFirst {
		final = "E_DEPLOY_DOWNTIME_FAILED"
	}
	msg := fmt.Sprintf("%s; replay recovery failed (critical, no second automatic recovery): %v", detail, restoreErr)
	if err := e.failTransition(ctx, rec, final, msg); err != nil {
		return err
	}
	return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
}

// restoreSnapshot 按快照重放（归位/回滚/漂移收敛共用原语，不变量 3）：
// 对期望快照执行完整对账（含删除多余服务）——确定性、幂等、无 --force。
// 重放路径恒下发 ServiceUpdate（applyDesired force=true）：desired-hash
// label 是上次平台写的存根，外部改动不清理它——相等不代表实况未被篡改；
// 同内容 ServiceUpdate 不触发任务重建（Spike B2 零替换语义不受影响）。
// D-DB-11（E4 托管数据库）：进入对账前对 source=system env 行按 key 取
// 当前值修正快照——归位/回滚/漂移收敛全部重放路径统一兑现 D-REL-9
//「secret 值永远取当前」（replaySystemEnvCurrent，rollback.go）。
func (e *Engine) restoreSnapshot(ctx context.Context, rec state.DeployRecord, snapshot []ServiceSpec) error {
	// D-DB-11：system env 当前值代换（轮换后重放不复活旧密码）。
	if err := e.replaySystemEnvCurrent(ctx, rec, snapshot); err != nil {
		return err
	}
	// 快照里的服务 label 补当前发布归属（服务 label 更新不触发任务替换）。
	for i := range snapshot {
		if snapshot[i].ServiceLabels == nil {
			snapshot[i].ServiceLabels = map[string]string{}
		}
		snapshot[i].ServiceLabels[state.LabelDeployment] = rec.ID
		snapshot[i].ServiceLabels[state.LabelDesiredHash] = snapshot[i].DesiredHash()
	}
	return e.applyDesired(ctx, rec, snapshot, true)
}

// lastActiveSnapshot 取最近一次 succeeded deployment 的期望快照（最后有效
// revision 的执行形态；无 → nil）。E5 Cron：与 decodeSpecs 同口径——Job
// 模板（cron 一次性服务）不进归位重放的期望集（长驻对账只见长驻服务）。
func (e *Engine) lastActiveSnapshot(ctx context.Context, rec state.DeployRecord) ([]ServiceSpec, error) {
	rows, err := e.store.ListAppDeployments(ctx, rec.AppID, 25)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ID == rec.ID || r.Status != state.DeploySucceeded || r.DesiredSpec == "" {
			continue
		}
		plain, err := e.box.Decrypt([]byte(r.DesiredSpec))
		if err != nil {
			return nil, fmt.Errorf("engine: decrypt snapshot of %s: %w", r.ID, err)
		}
		var all []ServiceSpec
		if err := json.Unmarshal(plain, &all); err != nil {
			return nil, fmt.Errorf("engine: decode snapshot of %s: %w", r.ID, err)
		}
		specs := make([]ServiceSpec, 0, len(all))
		for _, s := range all {
			if s.Job {
				continue
			}
			specs = append(specs, s)
		}
		return specs, nil
	}
	return nil, nil
}

// deploymentIsStopFirst 报告该部署是否含 stop-first 服务（有卷/固定端口/
// global——强制归位与停机账的判定域，§2.6）。
func (e *Engine) deploymentIsStopFirst(_ context.Context, rec state.DeployRecord) bool {
	specs, err := e.decodeSpecs(rec)
	if err != nil {
		return false
	}
	for i := range specs {
		if specs[i].UpdateOrder == "stop-first" {
			return true
		}
	}
	return false
}

// classifyUpdateFailure 把 Swarm pause 判定映射为注册表错误码（场景 3/5/6）：
// 新任务 Status.Err 逐字归因——health 判定 → E_HEALTH_TIMEOUT；镜像/拉取 →
// E_IMAGE_PULL_FAILED；其余 → E_TASK_START_FAILED。
func (e *Engine) classifyUpdateFailure(ctx context.Context, specs []ServiceSpec) (string, string) {
	for i := range specs {
		tasks, err := e.sub.TaskList(ctx, specs[i].Name)
		if err != nil {
			continue // 归因降级为启动失败（底座读失败由重试/看门狗兜底）
		}
		for _, t := range tasks {
			if !isNewVersionTask(t, specs[i].Image) {
				continue
			}
			if t.State != "failed" && t.State != "rejected" && t.State != "complete" {
				continue
			}
			msg := strings.ToLower(t.Err)
			switch {
			case strings.Contains(msg, "health"):
				return "E_HEALTH_TIMEOUT",
					fmt.Sprintf("new task of service %s failed the health gate: %s", specs[i].Name, t.Err)
			case strings.Contains(msg, "pull"), strings.Contains(msg, "image"), strings.Contains(msg, "manifest"):
				return "E_IMAGE_PULL_FAILED",
					fmt.Sprintf("image of the new task for service %s is unavailable: %s", specs[i].Name, t.Err)
			default:
				return "E_TASK_START_FAILED",
					fmt.Sprintf("new task of service %s failed to start: %s", specs[i].Name, t.Err)
			}
		}
	}
	return "E_TASK_START_FAILED", "update frozen by Swarm (failure_action=pause); no task-level failure reason observed"
}

// transientOr 底座读失败的暂态处理：底座不可用不落终态，交给看门狗预算
// （场景 12 的引擎内面：退避重试、deadline 兜底）。
func (e *Engine) transientOr(ctx context.Context, rec state.DeployRecord, err error) error {
	e.log.Warn("engine: substrate read failed (retrying within watchdog)", "deployment", rec.ID, "error", err)
	return nil
}

// desiredReplicasOf 是观测用的期望副本（global 服务单机按 1 计；副本数
// 来自受控子集校验后的 compose，量级有限，int 收窄安全——上界守卫兜底）。
func desiredReplicasOf(spec ServiceSpec) int {
	if spec.Global {
		return 1
	}
	if spec.Replicas > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(spec.Replicas)
}

// isNewVersionTask 判定任务是否属于目标版本（镜像 digest 引用一致）。
func isNewVersionTask(t TaskState, targetImage string) bool {
	return t.Image == targetImage
}

// countNewRunning 统计目标版本、期望态 running 的任务数。
func countNewRunning(tasks []TaskState, targetImage string) int {
	n := 0
	for _, t := range tasks {
		if t.State == "running" && t.DesiredState == "running" && isNewVersionTask(t, targetImage) {
			n++
		}
	}
	return n
}

// hasNewPendingTask 已由 countNewEverRan 取代（看门狗超时归因：目标版本
// 是否有任何任务运行过——从未运行 = PENDING 滞留域）。
// countNewEverRan 统计目标版本任务中运行过（正在运行或曾运行后退出）的数量。
func countNewEverRan(tasks []TaskState, targetImage string) int {
	n := 0
	for _, t := range tasks {
		if !isNewVersionTask(t, targetImage) {
			continue
		}
		switch t.State {
		case "running", "complete", "failed", "shutdown":
			n++
		}
	}
	return n
}
