package engine

// 终态写入、cancel 语义与控制面重启恢复（release-semantics §2.3 尾部三条
// + §2.5 场景 11/14；state-model §2.10 app 派生状态）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// failTransition 落 failed 终态（CAS：当前状态 → failed）+ deployment.failed
// 事件（单写点同事务，T0-V2.2）+ auto_abort 审计（system + reason=错误码，
// release-semantics §2.7）。已终态的竞争落败返回 nil（幂等收敛——转换与
// 事件同事务回滚，不产生孤儿事件）。
func (e *Engine) failTransition(ctx context.Context, rec state.DeployRecord, code, detail string) error {
	errorCode := code
	if err := e.store.EnterPhase(ctx, rec.ID, rec.Status, state.DeployFailed,
		state.DeploymentPatch{ErrorCode: &errorCode},
		deploymentEvent("deployment.failed", rec.ID, "code", code, "detail", detail)); err != nil {
		if errors.Is(err, state.ErrDeploymentStateTransition) {
			return nil // 已被并发推进（重启恢复/CLI 竞争）：终态不可逆
		}
		return err
	}
	rec.Status = state.DeployFailed
	rec.ErrorCode = code
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      "deployment.auto_abort",
			Target:      "deployment:" + rec.ID,
			Result:      "ok",
			ErrorCode:   code,
			DiffSummary: state.DiffSummary("app", rec.AppName, "detail", detail), // MG-6：构造器替换手拼 JSON
		})
	})
}

// failTransitionErr 是信封错误的失败出口（从 *apperr.Error 取码与文案）。
func (e *Engine) failTransitionErr(ctx context.Context, rec state.DeployRecord, err error) error {
	ae := appErrOf(err, rec.ID)
	return e.failTransition(ctx, rec, ae.Code(), ae.Message())
}

// cancelTerminal 未触底座阶段（queued/preparing/building）的取消：直接落
// cancelled（无归位动作——底座未被改动）。终态写与 deployment.cancelled
// 事件同一事务（单写点，T0-V2.2）；stage 审计随后同事务落账。
func (e *Engine) cancelTerminal(ctx context.Context, rec state.DeployRecord) error {
	from := rec.Status
	if err := e.store.EnterPhase(ctx, rec.ID, from, state.DeployCancelled, state.DeploymentPatch{},
		deploymentEvent("deployment.cancelled", rec.ID)); err != nil {
		return err
	}
	rec.Status = state.DeployCancelled
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		return auditDeployment(ctx, tx, "human", "deployment.cancel", rec.ID, "ok", "",
			`{"stage":"`+string(from)+`"}`)
	})
}

// cancelDeployment releasing（未切流）取消：先归位再落 cancelled（§2.3）。
// 无有效版本时按首发语义 scale=0 保留现场（不置 substrate_halted——取消
// 不是失败）。DT-4：init 相位取消先清场在途 init job（best-effort，残留由
// sweepInitJobs 兜底），再走既有归位/scale=0 语义。
func (e *Engine) cancelDeployment(ctx context.Context, rec state.DeployRecord) error {
	if rec.Phase == state.PhaseInitJobs {
		if templates, derr := e.decodeInitJobTemplates(rec); derr == nil {
			e.removeInitJobServices(ctx, rec, templates)
		} else {
			e.log.Warn("engine: init job templates unreadable during cancel (orphan sweep will collect by prefix)",
				"deployment", rec.ID, "error", derr)
		}
	}
	previous, err := e.lastActiveSnapshot(ctx, rec)
	if err != nil {
		return err
	}
	recovery := state.RecoveryReplay
	clear := false
	patch := state.DeploymentPatch{CancelRequested: &clear}
	if previous != nil {
		if err := e.restoreSnapshot(ctx, rec, previous); err != nil {
			return e.failTransitionErr(ctx, rec, errorf("E_ROLLBACK_FAILED",
				"cancel replay recovery failed (critical, no second automatic recovery): %v", err))
		}
		patch.Recovery = &recovery
	} else if specs, derr := e.decodeSpecs(rec); derr == nil {
		if err := e.scaleToZero(ctx, rec, specs); err != nil {
			return e.failTransitionErr(ctx, rec, err)
		}
	}
	// 终态写（含归位/请求位清零字段）与 deployment.cancelled 事件同一事务
	//（单写点，T0-V2.2）；stage 审计随后同事务落账。
	if err := e.store.EnterPhase(ctx, rec.ID, rec.Status, state.DeployCancelled, patch,
		deploymentEvent("deployment.cancelled", rec.ID)); err != nil {
		return err
	}
	rec.Status = state.DeployCancelled
	if err := e.store.InTx(ctx, func(tx *state.Tx) error {
		return auditDeployment(ctx, tx, "human", "deployment.cancel", rec.ID, "ok", "",
			`{"stage":"releasing","restored":true}`)
	}); err != nil {
		return err
	}
	return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
}

// rejectCancel 曾健康（已切流）不可取消：409 语义（建议改用 rollback）。
// 清除请求位并写 error 审计；deployment 保持在途。
func (e *Engine) rejectCancel(ctx context.Context, rec state.DeployRecord) error {
	clear := false
	if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{CancelRequested: &clear}); err != nil {
		return err
	}
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		return auditDeployment(ctx, tx, "human", "deployment.cancel", rec.ID,
			"error", "E_STATE_VERSION_CONFLICT",
			`{"reason":"already_switched","message":"deployment was healthy (already switched), cannot cancel; use rollback instead"}`)
	})
}

// CancelRequest 是 CLI 的取消入口：准入预检（曾健康 → 409 信封）+ 请求位
// 置位（引擎消费执行归位）。
func (e *Engine) CancelRequest(ctx context.Context, rec state.DeployRecord) error {
	if !rec.FirstHealthyAt.IsZero() || rec.Status == state.DeployObserving {
		return apperr.New("E_STATE_VERSION_CONFLICT",
			"deployment %s already switched (was healthy), cannot cancel (409): use rollback instead", rec.ID).
			WithContext("deployment", rec.ID).
			WithContext("reason", "already_switched")
	}
	if rec.Status.Terminal() {
		return apperr.New("E_STATE_VERSION_CONFLICT",
			"deployment %s is already terminal (%s), cannot cancel", rec.ID, rec.Status).
			WithContext("deployment", rec.ID).
			WithContext("reason", "terminal")
	}
	set := true
	return e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{CancelRequested: &set})
}

// errRecoveryTransient 是恢复扫描内的底座瞬态标记（M1-8）：classify/
// reopen 遇底座读失败时返回（区别于「已处置」的 nil）——上层登记该记录
// 待重试，而不是把瞬态当完成吞掉（旧缺陷：一次性尽力，无人重试）。
var errRecoveryTransient = errors.New("engine: recovery deferred (substrate transient)")

// recoverInterrupted 控制面重启的启动扫描（§2.3）：非终态 deployment 分类
// 恢复——健康 → 重开完整观察窗；paused/failed → 分类 + 归位；无法判定 →
// 失败（E_DEPLOY_INTERRUPTED）+ 人工。底座未就绪时静默（M1-8 起不再是一次
// 性尽力：SwarmReady/扫描失败与单记录瞬态失败都登记重试，由 tick 内
// retryRecoveryIfNeeded 以 30s 频控重试至完成；恢复动作本身幂等——分类落
// 终态后行不在非终态集、观察窗重开是确定性收敛，重放安全）。
func (e *Engine) recoverInterrupted(ctx context.Context) {
	if err := e.sub.SwarmReady(ctx); err != nil {
		e.armRecoveryRetry(nil)
		return
	}
	rows, err := e.store.ListNonTerminalDeployments(ctx)
	if err != nil {
		e.log.Warn("engine: recovery scan", "error", err)
		e.armRecoveryRetry(nil)
		return
	}
	// 重试轮只处理仍卡住的记录（首轮 recoveryStuck 为空 = 全量）：避免
	// 反复重开已恢复记录的观察窗（别的记录瞬态失败不该拖长无辜窗口）。
	stuck := map[string]bool{}
	for _, rec := range rows {
		if len(e.recoveryStuck) > 0 && !e.recoveryStuck[rec.ID] {
			continue
		}
		switch rec.Status {
		case state.DeployQueued, state.DeployPreparing, state.DeployBuilding:
			// 底座未被改动：tick 幂等重入。
		case state.DeployReleasing:
			if err := e.classifyRecovering(ctx, rec); err != nil {
				e.log.Warn("engine: recovery classification", "deployment", rec.ID, "error", err)
				stuck[rec.ID] = true
			}
		case state.DeployObserving:
			if err := e.reopenObserveWindow(ctx, rec); err != nil {
				e.log.Warn("engine: reopen observe window", "deployment", rec.ID, "error", err)
				stuck[rec.ID] = true
			}
		}
	}
	e.recoveryStuck = stuck
	if len(stuck) > 0 {
		e.armRecoveryRetry(stuck)
		return
	}
	e.recoveryPending = false // 扫描完成（无卡住记录）：重试停摆
}

// armRecoveryRetry 登记恢复未完成（M1-8）：nil 表示整轮重来（SwarmReady/
// 扫描失败）；非 nil 集合是单记录瞬态失败的定向重试。频控 30s（Warn 已在
// 调用点/重试点记，无硬性次数上限——底座长时间不可用时靠频控限噪）。
func (e *Engine) armRecoveryRetry(stuck map[string]bool) {
	if stuck != nil {
		e.recoveryStuck = stuck
	}
	e.recoveryPending = true
	e.recoveryNextAt = e.now().Add(recoveryRetryInterval)
}

// recoveryRetryInterval 是重启恢复重试的频控间隔（M1-8：无硬性次数上限，
// 间隔限噪；恢复动作幂等，重复执行只做确定性收敛）。
const recoveryRetryInterval = 30 * time.Second

// retryRecoveryIfNeeded 是 tick 的恢复重试步（M1-8）：恢复未完成且频控
// 窗已过 → 重跑 recoverInterrupted（幂等：整轮或定向卡住记录）。
func (e *Engine) retryRecoveryIfNeeded(ctx context.Context) {
	if !e.recoveryPending || e.now().Before(e.recoveryNextAt) {
		return
	}
	e.log.Warn("engine: retrying interrupted-recovery (M1-8; substrate was unavailable at startup)",
		"stuck", len(e.recoveryStuck))
	e.recoverInterrupted(ctx)
}

// classifyRecovering 对重启时处于 releasing 的部署分类（场景 11）：
// Swarm pause → 失败分流；全部服务已切换 → 观察窗；无法判定 →
// E_DEPLOY_INTERRUPTED 失败 + 归位。
func (e *Engine) classifyRecovering(ctx context.Context, rec state.DeployRecord) error {
	// blocked_waiting 分流（H12，场景 15「节点恢复续跑并重新起算」）：
	// 节点 down 期间控制面重启不得丢失等待语义——不分类、不动状态，交回
	// tick 的 evaluateReleasing → watchBoundNode 续跑（节点恢复则退出子状态
	// 并重臂看门狗；仍 down 则继续等待）。若落入下方分类：节点不可用使任务
	// 滞留 PENDING → default 分支误判 E_DEPLOY_INTERRUPTED 失败 + 归位，
	// 而节点仍不可用，归位重放同样滞留——既假失败又空转底座。
	if rec.Phase == state.PhaseBlockedWaiting {
		return nil
	}
	// DT-4 init 子相位分流：job 服务确定性命名 + 快照是执行形态权威来源
	// ——重启续跑是幂等的（ServiceInspect→缺失即创建；任务判定续跑），
	// 不落入下方「长驻服务未切换 → 无法判定」的立即失败分类。预算/看门狗
	// 锚已在行上（release_started_at / watchdog_deadline_at），
	// evaluateInitJobs 续判。
	if rec.Phase == state.PhaseInitJobs {
		return nil
	}
	specs, err := e.decodeSpecs(rec)
	if err != nil {
		return e.failUnswitchedOrSwitched(ctx, rec, "E_DEPLOY_INTERRUPTED",
			"desired-state snapshot unreadable after control-plane restart: manual intervention required")
	}
	anyPaused := false
	allSwitched := len(specs) > 0
	for i := range specs {
		spec := specs[i]
		svc, err := e.sub.ServiceInspect(ctx, spec.Name)
		if err != nil {
			if errors.Is(err, ErrServiceNotFound) {
				anyPaused = true
				allSwitched = false
				break
			}
			return errRecoveryTransient // 底座暂态：M1-8 登记重试（看门狗兜底不覆盖重启分类）
		}
		tasks, err := e.sub.TaskList(ctx, spec.Name)
		if err != nil {
			return errRecoveryTransient // M1-8：底座暂态登记重试（无人重试即窗口丢失）
		}
		if svc.UpdateState == "paused" {
			anyPaused = true
		}
		if countNewRunning(tasks, spec.Image) >= desiredReplicasOf(spec) &&
			(svc.UpdateState == "" || svc.UpdateState == "completed") {
			continue
		}
		allSwitched = false
	}
	switch {
	case anyPaused:
		code, detail := e.classifyUpdateFailure(ctx, specs)
		return e.failUnswitchedOrSwitched(ctx, rec, code, "classified recovery after control-plane restart: "+detail)
	case allSwitched:
		return e.enterObserving(ctx, rec)
	default:
		// 无法判定（更新中/无进展）→ 失败 + 人工（§2.3；E_DEPLOY_INTERRUPTED）。
		return e.failUnswitchedOrSwitched(ctx, rec, "E_DEPLOY_INTERRUPTED",
			"deployment state indeterminate after control-plane restart (updating or no progress): confirm manually and start a new deployment")
	}
}

// reopenObserveWindow 观察窗重启恢复：健康 → 重开完整观察窗（§2.3）；
// 水位不齐 → E_DEPLOY_INTERRUPTED 失败。
func (e *Engine) reopenObserveWindow(ctx context.Context, rec state.DeployRecord) error {
	specs, err := e.decodeSpecs(rec)
	if err != nil {
		return e.failSwitched(ctx, rec, "E_DEPLOY_INTERRUPTED",
			"desired-state snapshot unreadable after control-plane restart: manual intervention required")
	}
	for i := range specs {
		tasks, err := e.sub.TaskList(ctx, specs[i].Name)
		if err != nil {
			// M1-8：底座暂态不再静默吞掉（旧形态 return nil = 恢复「完成」，
			// 无人重试——观察窗带陈旧 ObserveStartedAt 直接判窗末）；登记
			// 该记录待重试，重开完整窗口。
			return errRecoveryTransient
		}
		if countNewRunning(tasks, specs[i].Image) < desiredReplicasOf(specs[i]) {
			return e.failSwitched(ctx, rec, "E_DEPLOY_INTERRUPTED",
				"observe window state unhealthy after control-plane restart (replica watermarks not met): confirm manually and start a new deployment")
		}
	}
	observeStart := e.now()
	if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{ObserveStartedAt: &observeStart}); err != nil {
		return err
	}
	rec.ObserveStartedAt = observeStart
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		return appendEvents(ctx, tx, deploymentEvent("deployment.observe_started", rec.ID,
			"window_seconds", fmt.Sprintf("%d", int64(e.cfg.ObserveWindow/time.Second)),
			"reason", "control_plane_restart"))
	})
}

// ── app 派生状态（state-model §2.10）────────────────────────────────────────

// AppFacts 是派生裁决的输入事实。
type AppFacts struct {
	// PlacementState 是绑定状态位（'' = 无绑定记录——自由调度非 blocked）。
	PlacementState string
	// Latest 是最近一条部署记录（零值 = 无部署）。
	Latest state.DeployRecord
	// LatestSucceeded 是最近一条 succeeded 部署（零值 = 无有效版本）。
	LatestSucceeded state.DeployRecord
	// Suspended 是用户挂起位（app Stop/Start，apps.suspended 00028 列）的
	// 直投影输入——true 时派生裁决短路为 suspended（权威位直投影，DB paused
	// 同型：不是对底座的观察结论，优先级压倒一切观察态）。
	Suspended bool
}

// 派生状态词表（state 词表对齐：running/degraded/blocked/down/suspended）。
const (
	DerivedRunning   = "running"
	DerivedDegraded  = "degraded"
	DerivedBlocked   = "blocked"
	DerivedDown      = "down"
	DerivedSuspended = "suspended"
)

// DeriveAppState 是纯函数裁决：suspended > down > blocked > degraded >
// running。
//   - suspended：用户挂起位（apps.suspended）置位——用户请求的投影，不是
//     观察结论（DB paused 同型）；恢复走 resume 清位 + 重部署管线；
//   - down：首发失败 scale=0（substrate_halted），或无有效版本且最近一次
//     失败未切流（没有任何期望实例——归位为零动作/scale 0）；
//   - blocked：placement.state ∈ {blocked, unresolved}（绑定不可用/已移除）；
//   - degraded：观察窗失败（verdict=unstable——含首发已切流后失败的形态，
//     新版本仍在服务）/ 窗后不稳定 / 警告通过（W_DEPLOY_INSTABILITY）；
//   - running：以上皆否。
func DeriveAppState(f AppFacts) string {
	// suspended 第一判（用户权威位压倒观察态——挂起期排水到 0 是期望形态，
	// 不是故障；同拍观察输入一律不参与裁决）。
	if f.Suspended {
		return DerivedSuspended
	}
	latestFailed := f.Latest.ID != "" && f.Latest.Status == state.DeployFailed
	// down（最高优先级）。
	if latestFailed && f.Latest.SubstrateHalted {
		return DerivedDown
	}
	if f.LatestSucceeded.ID == "" {
		if f.Latest.ID == "" {
			return DerivedDown // 无任何部署记录（无期望实例）
		}
		if latestFailed && f.Latest.FirstHealthyAt.IsZero() {
			return DerivedDown // 首发未切流失败：归位无对象，无期望实例
		}
		// 首发已切流（观察窗失败 unstable）：新版本仍服务 → degraded。
	}
	// blocked。
	switch state.PlacementState(f.PlacementState) {
	case state.PlacementBlocked, state.PlacementUnresolved:
		return DerivedBlocked
	}
	// degraded。
	if latestFailed && f.Latest.Verdict == state.VerdictUnstable {
		return DerivedDegraded
	}
	if f.LatestSucceeded.ID != "" &&
		f.LatestSucceeded.Flags&(state.DeployFlagPostWindowAlerted|state.DeployFlagInstabilityWarning) != 0 {
		return DerivedDegraded
	}
	return DerivedRunning
}

// appFactsOf 读取派生输入事实。
func (e *Engine) appFactsOf(ctx context.Context, appID string) (AppFacts, error) {
	f := AppFacts{}
	// 挂起位（app Stop/Start）：派生裁决的第一输入——行不在即无事实可读。
	if app, err := e.store.GetAppByID(ctx, appID); err == nil {
		f.Suspended = app.Suspended
	} else if !errors.Is(err, state.ErrAppNotFound) {
		return f, err
	}
	if p, err := e.store.GetPlacement(ctx, appID); err == nil {
		f.PlacementState = string(p.State)
	} else if !errors.Is(err, state.ErrPlacementNotFound) {
		return f, err
	}
	if rows, err := e.store.ListAppDeployments(ctx, appID, 1); err == nil && len(rows) > 0 {
		f.Latest = rows[0]
	} else if err != nil {
		return f, err
	}
	rows, err := e.store.ListAppDeployments(ctx, appID, 25)
	if err != nil {
		return f, err
	}
	for _, r := range rows {
		if r.Status == state.DeploySucceeded {
			f.LatestSucceeded = r
			break
		}
	}
	return f, nil
}

// refreshDerivedState 推导并落库 app 派生状态；翻转时发映射事件
// （进入 degraded → app.degraded；消除 → app.recovered；blocked 由
// placement.* 驱动的事件承载——不重复发 app 事件）。
func (e *Engine) refreshDerivedState(ctx context.Context, appID, appName string) error {
	facts, err := e.appFactsOf(ctx, appID)
	if err != nil {
		return err
	}
	next := DeriveAppState(facts)
	var cur string
	err = e.store.InTx(ctx, func(tx *state.Tx) error {
		var err error
		cur, err = tx.GetAppDerivedState(ctx, appID)
		if err != nil {
			if errors.Is(err, state.ErrAppNotFound) {
				return nil
			}
			return err
		}
		if cur == next {
			return nil
		}
		if err := tx.SetAppDerivedState(ctx, appID, cur, next); err != nil {
			return err
		}
		// 事件映射（state-model §2.10）：degraded 进入/退出。
		if next == DerivedDegraded {
			return appendEvents(ctx, tx, appEvent("app.degraded", appName))
		}
		if cur == DerivedDegraded && next == DerivedRunning {
			return appendEvents(ctx, tx, appEvent("app.recovered", appName))
		}
		return nil
	})
	if err != nil && !errors.Is(err, state.ErrAppDerivedStateConflict) {
		return err
	}
	return nil
}

// AppDerivedState 是读面出口（CLI deploy 结果输出用）。
func (e *Engine) AppDerivedState(ctx context.Context, appID string) (string, error) {
	facts, err := e.appFactsOf(ctx, appID)
	if err != nil {
		return "", err
	}
	return DeriveAppState(facts), nil
}
