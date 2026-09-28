package engine

// 部署期一次性作业（DT-4，torchwood 线；IMPL-T1-3）：compose `fleetly.job:
// init` 声明的服务在发布管线**晋级之前**以新 spec 跑一次性 job——迁移
// 先于新代码跑在旧库上（新 revision 的长驻服务对账 applyDesired 必须晚于
// 全部 init job 成功）。语义要点：
//
//   - 相位：preparing/building → releasing 的 EnterPhase 同事务携带
//     phase=init_jobs（与快照/看门狗锚原子落位）；job 全过 → 清相位 +
//     重臂看门狗 + 对账长驻服务 → 后续健康门/观察窗照旧。init 相位优先于
//     blocked_waiting（相位列单值：init 期节点不可用由 job 看门狗兜底，
//     cancel 可用，清收在收尾路径）；无 init 模板的部署相位零写（守卫⑤：
//     无 init job 的 release 路径行为零变化）。
//   - 执行形态：每个模板经 engine.JobSpecFrom 克隆（单副本/restart=none/
//     一次性 label 锚 fleetly.init.run=<deploymentID>）；命名 =
//     naming.InitJobName（独立前缀族 fleetly-init-，确定性 deployid8 使
//     创建幂等、重启续跑可寻址）；env/secret/卷/网络投影与长驻同源
//     （buildServiceSpec 共用）——secret 底座对象在创建前幂等确保
//     （SecretReference 必须携底座 ID，W3 真机教训）。
//   - 等待与看门狗：每拍任务判定（engine.JobTaskVerdict）；每个 job 独立
//     预算（fleetly.job.timeout label 或平台默认 InitJobTimeout），锚 =
//     release_started_at（含重启——预算不重置）；相位级
//     watchdog_deadline_at = release + max(各预算) 作兜底。超时 →
//     release.job_timed_out + E_INIT_JOB_TIMED_OUT；失败 →
//     release.job_failed + E_INIT_JOB_FAILED；随后统一走
//     failUnswitchedOrSwitched（D-REL-4 唯一失败入口，不绕道）。
//   - 多 job 并行、全过才晋级；任一 failed/rejected/shutdown 立即失败
//     （fail-fast；已完成的 job 不回滚——迁移前向语义）。
//   - 零残留：job 全过/失败/超时/取消后服务即刻移除（best-effort），
//     兜底 = sweepInitJobs 周期对账（部署行缺失/终态/相位已清的
//     fleetly-init- 服务移除）+ applyDesired/MoveApp/漂移三处前缀豁免
//     （见各调用点注释）。
//   - 崩溃窗口（诚实记录）：清相位落库后、applyDesired 前控制面崩溃 →
//     重启按「releasing 无法判定」分类失败（E_DEPLOY_INTERRUPTED + 归位
//     旧版本）——安全失败（迁移已执行、新代码未晋级）；不重跑迁移
//     （重跑风险高于一次显式失败）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// initJobSweepInterval 是 init 孤儿服务对账扫描的频控间隔（与
// substrateRecon/appDeleteScanInterval 同量级：残留在部署行终态/相位清位
// 后一个窗口内暴露；扫描只读底座 + 单事务落库，幂等）。
const initJobSweepInterval = 30 * time.Second

// initJobRuntime 是一次 init job 的运行时形态（模板 + 确定性服务名 +
// 预算）。
type initJobRuntime struct {
	template ServiceSpec
	name     string
	budget   time.Duration
}

// evaluateInitJobs 推进 init 相位一个周期（releasing 的 init 子相位评估
// 入口；evaluateReleasing 在 watchBoundNode 之前分流至此）。
func (e *Engine) evaluateInitJobs(ctx context.Context, rec state.DeployRecord) error {
	templates, err := e.decodeInitJobTemplates(rec)
	if err != nil {
		return e.failTransitionErr(ctx, rec, errorf("E_DEPLOY_INTERRUPTED",
			"init job templates unreadable (%v): manual intervention required", err))
	}
	if len(templates) == 0 {
		// 防御（理论不可达：相位与快照同事务落位）：不静默卡死——清相位
		// 继续走长驻对账。
		e.log.Warn("engine: init phase without init job templates (defensive clear; continuing the release)",
			"deployment", rec.ID)
		return e.finishInitJobs(ctx, rec, nil)
	}
	if err := e.provisionInitJobServices(ctx, rec, templates); err != nil {
		return err
	}

	jobs := make([]initJobRuntime, 0, len(templates))
	for i := range templates {
		tmpl := templates[i]
		jobName, nerr := e.initJobNameOf(rec, tmpl)
		if nerr != nil {
			return e.failInitJob(ctx, rec, templates,
				initJobRuntime{template: tmpl, name: "(unnamed)", budget: e.initJobBudget(tmpl)},
				"E_INIT_JOB_FAILED", errMessageForEvent(nerr))
		}
		jobs = append(jobs, initJobRuntime{
			template: tmpl,
			name:     jobName,
			budget:   e.initJobBudget(tmpl),
		})
	}

	var unfinished []initJobRuntime
	for _, job := range jobs {
		verdict, reason := JobTaskVerdict(ctx, e.sub, job.name)
		switch verdict {
		case JobVerdictFailed:
			return e.failInitJob(ctx, rec, templates, job, "E_INIT_JOB_FAILED", reason)
		case JobVerdictSucceeded:
			continue
		default:
			unfinished = append(unfinished, job)
		}
	}
	if len(unfinished) == 0 {
		return e.finishInitJobs(ctx, rec, templates)
	}
	// 超时判定（per-job 预算；锚 = release_started_at，重启不重置）。
	elapsed := e.initJobElapsed(rec)
	for _, job := range unfinished {
		if elapsed > job.budget {
			return e.failInitJob(ctx, rec, templates, job, "E_INIT_JOB_TIMED_OUT",
				fmt.Sprintf("exceeded its watchdog budget %s (fleetly.job.timeout label or platform default)", job.budget))
		}
	}
	// 相位级兜底（watchdog_deadline_at = release + max(各预算)；正常路径
	// 不会先于 per-job 判定触达，防时钟/边界漏判）。
	if !rec.WatchdogDeadlineAt.IsZero() && e.now().After(rec.WatchdogDeadlineAt) {
		return e.failInitJob(ctx, rec, templates, unfinished[0], "E_INIT_JOB_TIMED_OUT",
			fmt.Sprintf("init phase exceeded its watchdog deadline %s", rec.WatchdogDeadlineAt.UTC().Format(time.RFC3339)))
	}
	return nil
}

// provisionInitJobServices 确保 init job 服务在位：确定性错误（apperr 信封
// ——secret 不可解析/命名违约等，重试无意义）直接走失败入口（不空烧看门狗
// 成 timed_out 误归因）；底座暂态告警后返回 nil，下一拍重试（预算由
// deadline 守）。
func (e *Engine) provisionInitJobServices(ctx context.Context, rec state.DeployRecord, templates []ServiceSpec) error {
	if err := e.ensureInitJobServices(ctx, rec, templates); err != nil {
		var ae *apperr.Error
		if asAppErr(err, &ae) && ae != nil {
			return e.failTransitionErr(ctx, rec, ae)
		}
		e.log.Warn("engine: init job service create failed (retried next tick within the init watchdog)",
			"deployment", rec.ID, "error", err)
	}
	return nil
}

// ensureInitJobServices 幂等确保全部 init job 服务在位：缺失的经
// JobSpecFrom 克隆创建（网络先行；secret 底座对象在任一待创建服务存在时
// 先一次性确保——与 applyDesired 同一幂等纪律）。已在位（创建半程崩溃/
// 重启续跑/前拍已建）不触碰。
func (e *Engine) ensureInitJobServices(ctx context.Context, rec state.DeployRecord, templates []ServiceSpec) error {
	var pending []ServiceSpec
	for i := range templates {
		tmpl := templates[i]
		jobName, err := e.initJobNameOf(rec, tmpl)
		if err != nil {
			return errorf("E_RUNTIME_UNAVAILABLE", "%v", err)
		}
		if _, err := e.sub.ServiceInspect(ctx, jobName); err == nil {
			continue // 已在位
		} else if !errors.Is(err, ErrServiceNotFound) {
			return err // 底座暂态：调用方下一拍重试
		}
		pending = append(pending, JobSpecFrom(tmpl, jobName, map[string]string{state.LabelInitRun: rec.ID}))
	}
	if len(pending) == 0 {
		return nil
	}
	if err := e.ensureSnapshotSecrets(ctx, rec, templates); err != nil {
		return err
	}
	// config 底座对象同源确保（OT-3/IMPL-T1-4：init 模板与长驻服务共用
	// buildServiceSpec 的投影链——config 挂载在 job 服务创建前同样必须
	// 在位；ConfigReference 需要底座对象 ID，W3 真机教训同族）。
	if err := e.ensureSnapshotConfigs(ctx, rec, templates); err != nil {
		return err
	}
	for i := range pending {
		job := pending[i]
		for _, n := range job.Networks {
			if err := e.sub.NetworkEnsure(ctx, n.Name); err != nil {
				return err
			}
		}
		if err := e.sub.ServiceCreate(ctx, job); err != nil {
			return err
		}
		e.log.Info("engine: init job service created", "deployment", rec.ID, "service", job.Name)
	}
	return nil
}

// initJobsMaxBudget 返回 init 相位看门狗预算（= max(各 job 预算)；用于
// 相位级兜底 deadline——per-job 判定仍是主路径）。
func (e *Engine) initJobsMaxBudget(templates []ServiceSpec) time.Duration {
	max := time.Duration(0)
	for i := range templates {
		if b := e.initJobBudget(templates[i]); b > max {
			max = b
		}
	}
	if max <= 0 {
		max = e.cfg.InitJobTimeout
	}
	return max
}

// finishInitJobs 收尾 init 相位并晋级：完成标记先行（清相位 + 看门狗重臂
// ——崩溃窗口语义见文件头注）→ job 服务清场（best-effort）→ 以新 spec
// 对账长驻服务（applyDesired；失败走失败分流唯一入口）。
func (e *Engine) finishInitJobs(ctx context.Context, rec state.DeployRecord, templates []ServiceSpec) error {
	empty := ""
	deadline := e.now().Add(e.cfg.DeployTimeout)
	if err := e.store.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{
		Phase:              &empty,
		WatchdogDeadlineAt: &deadline,
	}); err != nil {
		return err
	}
	rec.Phase = empty
	rec.WatchdogDeadlineAt = deadline

	e.removeInitJobServices(ctx, rec, templates)

	specs, err := e.decodeSpecs(rec)
	if err != nil {
		return e.failTransitionErr(ctx, rec, errorf("E_DEPLOY_INTERRUPTED",
			"desired-state snapshot unreadable (%v): manual intervention required", err))
	}
	if err := e.applyDesired(ctx, rec, specs, false); err != nil {
		ae := appErrOf(err, rec.ID)
		return e.failUnswitchedOrSwitched(ctx, rec, ae.Code(),
			fmt.Sprintf("release reconcile failed (half-applied state routed per failure policy — replay/scale=0): %s", ae.Message()))
	}
	e.log.Info("engine: init jobs passed, promoting the new revision", "deployment", rec.ID)
	return nil
}

// failInitJob 落 init job 失败/超时：先落 job 自身的事件（release.job_failed
// / release.job_timed_out——job 的独立记录，失败重试面），随后统一走失败
// 分流唯一入口（failUnswitchedOrSwitched：归位/首发 scale=0 语义照旧），
// 最后清场 job 服务（残留由 sweepInitJobs 兜底）。事件先于终态写的理由：
// job 失败的事实独立于部署终态；终态写失败（瞬态）时下一拍重判重试，
// 事件可能重复（重复优于漏报），且**不会**重跑 job（服务仍在位，判定
// 继续命中失败终态）。
func (e *Engine) failInitJob(ctx context.Context, rec state.DeployRecord, templates []ServiceSpec,
	job initJobRuntime, code, reason string) error {
	eventName := "release.job_failed"
	verb := "failed"
	if code == "E_INIT_JOB_TIMED_OUT" {
		eventName = "release.job_timed_out"
		verb = "timed out"
	}
	if err := e.recordInitJobEvent(ctx, rec, eventName, job, reason); err != nil {
		return err
	}
	detail := fmt.Sprintf("init job %s of service %s %s: %s",
		job.name, job.template.ServiceLabels[state.LabelProcess], verb, reason)
	if err := e.failUnswitchedOrSwitched(ctx, rec, code, detail); err != nil {
		return err
	}
	e.removeInitJobServices(ctx, rec, templates)
	return nil
}

// recordInitJobEvent 落 job 失败/超时事件（payload 只带事实字段；reason
// 已单行化）。
func (e *Engine) recordInitJobEvent(ctx context.Context, rec state.DeployRecord, name string,
	job initJobRuntime, reason string) error {
	ev := deploymentEvent(name, rec.ID,
		"app", rec.AppName, "app_id", rec.AppID,
		"service", job.template.ServiceLabels[state.LabelProcess],
		"job_service", job.name, "budget", job.budget.String(), "error", reason)
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		return appendEvents(ctx, tx, ev)
	})
}

// removeInitJobServices 清场 init job 服务（best-effort：失败只告警——
// 残留由 sweepInitJobs 按前缀与部署行归属兜底清除）。
func (e *Engine) removeInitJobServices(ctx context.Context, rec state.DeployRecord, templates []ServiceSpec) {
	for i := range templates {
		jobName, err := e.initJobNameOf(rec, templates[i])
		if err != nil {
			e.log.Warn("engine: init job name resolve failed during cleanup", "deployment", rec.ID, "error", err)
			continue
		}
		if err := e.sub.ServiceRemove(ctx, jobName); err != nil {
			e.log.Warn("engine: init job service remove failed (orphan sweep will retry)",
				"deployment", rec.ID, "service", jobName, "error", err)
		}
	}
}

// initJobNameOf 由 init 模板推导确定性 job 服务名：team/prj 取模板 label
// （快照是权威来源——二次查库可能读到 MoveApp 换派后的新归属而与在途
// 模板错位，cron 同口径），app 取部署行的入队名，段 = compose 服务名，
// 尾缀 = deploymentID 前 8 位。
func (e *Engine) initJobNameOf(rec state.DeployRecord, template ServiceSpec) (string, error) {
	team := template.ServiceLabels[state.LabelTeam]
	prj := template.ServiceLabels[state.LabelProject]
	service := template.ServiceLabels[state.LabelProcess]
	if team == "" || prj == "" || service == "" {
		return "", fmt.Errorf("engine: init job template %s lacks team/project/process labels (snapshot invariant broken)", template.Name)
	}
	return naming.InitJobName(team, prj, rec.AppName, service, rec.ID)
}

// initJobBudget 返回单 job 看门狗预算（label 归一值优先；缺省平台默认）。
func (e *Engine) initJobBudget(template ServiceSpec) time.Duration {
	if template.InitJobTimeout > 0 {
		return template.InitJobTimeout
	}
	return e.cfg.InitJobTimeout
}

// initJobElapsed 返回 init 相位的已消费时长（锚 = release_started_at；
// 存量/异常行回落 created_at 保持有界语义）。
func (e *Engine) initJobElapsed(rec state.DeployRecord) time.Duration {
	anchor := rec.ReleaseStartedAt
	if anchor.IsZero() {
		anchor = rec.CreatedAt
	}
	return e.now().Sub(anchor)
}

// decodeInitJobTemplates 从部署快照解码 init job 模板（Job 且 InitJob；
// 重启续跑与在途评估的统一读取面——快照是执行形态权威来源）。
func (e *Engine) decodeInitJobTemplates(rec state.DeployRecord) ([]ServiceSpec, error) {
	if rec.DesiredSpec == "" {
		return nil, fmt.Errorf("engine: deployment %s has no desired-spec snapshot", rec.ID)
	}
	plain, err := e.box.Decrypt([]byte(rec.DesiredSpec))
	if err != nil {
		return nil, fmt.Errorf("engine: decrypt desired-spec %s: %w", rec.ID, err)
	}
	var all []ServiceSpec
	if err := json.Unmarshal(plain, &all); err != nil {
		return nil, fmt.Errorf("engine: decode desired-spec %s: %w", rec.ID, err)
	}
	return filterInitJobs(all), nil
}

// filterInitJobs 提取 init job 模板（Job=true 且 InitJob=true；cron 模板
// InitJob 恒 false——老快照零回归）。
func filterInitJobs(specs []ServiceSpec) []ServiceSpec {
	out := make([]ServiceSpec, 0, len(specs))
	for _, s := range specs {
		if s.Job && s.InitJob {
			out = append(out, s)
		}
	}
	return out
}

// SweepInitJobs 单步执行 init 孤儿服务清扫（测试与诊断显式入口——直通
// 频控闸；生产由 tick 周期驱动）。
func (e *Engine) SweepInitJobs(ctx context.Context) { e.sweepInitJobs(ctx, true) }

// sweepInitJobs 是 tick 的 init 孤儿清扫 duty（零残留的 janitor 腿）：
// 扫描全部受管服务中的 fleetly-init- 前缀族，移除「部署行缺失/终态/相位
// 已离开 init_jobs」的残留（完成清场失败、失败清场失败、崩溃半程、外部
// 注入）。非终态且仍在 init 相位的服务=管线自有对象，不动。频控：非
// force 形态受 initScanGate 时间闸（tick goroutine 专用，scanGate 单点
// 见 disclosure.go）。幂等：ServiceRemove 缺失视为成功。
func (e *Engine) sweepInitJobs(ctx context.Context, force bool) {
	if !e.initScanGate.due(e.now(), force, initJobSweepInterval) {
		return
	}
	services, err := e.sub.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
	})
	if err != nil {
		e.log.Warn("engine: init job sweep service list failed", "error", err)
		return
	}
	for _, s := range services {
		if !naming.IsInitJobName(s.Name) {
			continue
		}
		if !e.initJobServiceIsOrphan(ctx, s) {
			continue
		}
		if err := e.sub.ServiceRemove(ctx, s.Name); err != nil {
			e.log.Warn("engine: init job orphan remove failed (retried next sweep)", "service", s.Name, "error", err)
			continue
		}
		e.log.Info("engine: orphan init job service removed",
			"service", s.Name, "deployment", s.Labels[state.LabelInitRun])
	}
}

// initJobServiceIsOrphan 判定一只 fleetly-init- 服务是否孤儿：归属 label
// 缺失 / 部署行不存在 / 部署行终态 / 部署行相位已离开 init_jobs——任一
// 成立即无管线所有者，可清。底座读失败（store 错误）保守保留（下一拍
// 重判）。
func (e *Engine) initJobServiceIsOrphan(ctx context.Context, s ServiceState) bool {
	deployID := s.Labels[state.LabelInitRun]
	if deployID == "" {
		return true
	}
	rec, err := e.store.GetDeployment(ctx, deployID)
	if err != nil {
		if errors.Is(err, state.ErrDeploymentNotFound) {
			return true
		}
		e.log.Warn("engine: init job sweep read deployment failed (keeping the service this beat)",
			"service", s.Name, "deployment", deployID, "error", err)
		return false
	}
	if rec.Status.Terminal() {
		return true
	}
	return rec.Phase != state.PhaseInitJobs
}
