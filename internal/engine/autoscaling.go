package engine

// 自动扩缩评估器（B 线 W5 设计 §1，D-V3W5-2，v0.3 W5-S1）：收敛拍尾部的
// 周期步——对「有策略 × metrics.mode=on × 服务 running」的 compose 服务
// 求 CPU/内存水位，超阈值经**平台副本写通道**调整期望副本。
//
// 副本动作通道（设计 §1.2「spec 的运行期副本覆盖层」的落形，W5-S1 读码
// 定形）：autoscaler 的调整 = 底座 ServiceUpdate（快照 spec 置新副本，label
// 与发布对账同形重锚）+ state.scaling_replica_overrides 行 upsert（compose
// 服务名键、deployment_id 钉期望来源部署、updated_at 即冷却窗时间锚）。
// 覆盖行只在与期望态来源部署一致时「活」：
//   - 漂移对账（drift.go computeAppDrift）与归位收敛（convergeApp）经
//     pinScalingReplicaOverrides 以活覆盖副本为期望——平台自己写的副本不
//     被漂移误报、不被收敛回滚（红线②）；
//   - 外部 docker service scale 不写覆盖 → 期望(覆盖或快照) vs 实况(外部
//     值) 失配 → 漂移判据照常触发（红线③）；
//   - 部署换代（新 succeeded 部署）后旧覆盖自然失活——发布以 compose 期望
//     重置基线，autoscaler 按新基线重新评估。
//
// 评估纪律（设计 §1.2 逐条）：
//   - 判据：任一维度超 target 的 +10%（扩）或低于 target 的 −30%（缩）；
//     拟调整副本 = ceil(当前 × 实测/target) 夹 [min,max]——HPA 同式（以目
//     标水位恢复实测的等比反解；变化为 0 不动作）。多维度同时触发取各维
//     反解的最大值（满足最高需求方——保守向）；
//   - 数据源：VM /api/v1/query 按 swarm 服务 label 聚合 cadvisor 指标
//     （CPU = 2m rate 总和；内存 = working_set 均值）。查不到序列 = 不动
//     作 + scaling.no_data 一次性披露（诚实无数据）；查询面故障 ≠ 无数据
//     ——只日志退避，不披露不动作；
//   - 限额归一口径：分母 = 服务的 compose 资源限额（CPU 限额核数 × 期望
//     副本；内存限额字节——内存取 per-replica 均值对 per-replica 限额）。
//     无限额服务的对应维度**不可评估即跳过**（cAdvisor 无服务侧宿主容量
//     概念，臆造分母 = 伪造利用率）；两维都无限额的策略恒不动作；
//   - 护栏：stateful（有卷）服务只扩不缩；global 服务无副本语义不评估；
//     冷却窗内不重复动作；评估前置实况一致性门（ServiceInspect 实况副本
//     == 平台期望）——失配即有外部改动或上次动作写失败在场，本拍不动作
//     （外部改动的披露归漂移判据，autoscaler 不越权收敛）；
//   - metrics off：策略休眠，scaling.dormant 每策略一次性披露（metrics 回
//     on 解除，可再披露）。进程内记忆（重复优于漏报的反面：披露优于刷屏
//     ——一次性语义与 driftSeen 同款迁移判定），重启清零可重报一次。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// autoscalingInterval 是评估扫描的频控间隔（tick 每拍都跑太密——VM 查询
// 以 30s 计的发现延迟可接受；冷却窗下界 60s，30s 评估节奏保证冷却窗过期
// 后一拍内响应）。
const autoscalingInterval = 30 * time.Second

// scalingUpThreshold / scalingDownThreshold 是扩/缩判据（设计 §1.2 原文
// 「超 target+10% / 低于 target−30%」——target 的相对偏移；乘法读法是
// target ∈ [20,90] 全值域下两阈值都有意义的唯一解释，加法读法在低 target
// 端会得到非正的缩容阈值）。
const (
	scalingUpThreshold   = 1.10
	scalingDownThreshold = 0.70
)

// MetricsQuerier 是引擎对 VM 瞬时查询面的消费端口（internal/metrics.Backend
// 隐式实现；接口在本包定义——测试注入缝与装配层适配同用）。ok=false 表示
// 查询成功但**查不到序列**（诚实无数据判据；与值为 0 严格区分）。
type MetricsQuerier interface {
	InstantValue(ctx context.Context, promql string) (value float64, ok bool, err error)
}

// WithMetricsQuerier 注入 VM 查询端口（生产装配 = metrics.Backend；nil =
// 本步整体不在评估域——装配形态而非运行态，不产生任何披露）。
func (e *Engine) WithMetricsQuerier(q MetricsQuerier) *Engine { e.metricsQ = q; return e }

// scalingCPUPromQL 是单服务 CPU 水位的瞬时查询（cadvisor 按 swarm 服务
// label 聚合 2m rate 总和；image!="" 剔除 pause 等非任务容器——Console
// 资源卡同款锚点）。服务名来自平台命名公式（[A-Za-z0-9._-]，无 PromQL
// 元字符），双引号字面量内插安全。
func scalingCPUPromQL(service string) string {
	return fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{container_label_com_docker_swarm_service_name=%q,image!=""}[2m]))`, service)
}

// scalingMemPromQL 是单服务内存水位的瞬时查询（working_set 均值——per
// replica 均值对 per-replica 限额求水位）。
func scalingMemPromQL(service string) string {
	return fmt.Sprintf(`avg(container_memory_working_set_bytes{container_label_com_docker_swarm_service_name=%q,image!=""})`, service)
}

// scalingSample 是一次瞬时查询的样本（ok=false = 无序列）。
type scalingSample struct {
	value float64
	ok    bool
}

// scalingDecision 是单服务单拍的评估结论（纯函数产出——判据表驱动的被测
// 面；actionable=false 时 proposed/dimension 无语义）。
type scalingDecision struct {
	actionable bool
	// proposed 是夹逼后的目标副本（proposed == current 时不可操作）。
	proposed uint64
	// dimension 是触发维度的人读串（"cpu" / "mem" / "cpu+mem"——事件载荷）。
	dimension string
	// cpuPct / memPct 是实测水位百分数（事件载荷；未评估维度为 NaN）。
	cpuPct float64
	memPct float64
}

// evaluateScaling 执行单服务判据评估（设计 §1.2 判据/夹逼/护栏的纯函数
// 形态）。输入：
//   - cur：当前期望副本（活覆盖值，否则快照值）；
//   - stateful：有卷服务（只扩不缩护栏）；
//   - cpuLimitCores / memLimitBytes：compose 资源限额（0 = 无限额——该维度
//     不可评估，如实跳过）；
//   - cpu / mem：瞬时样本（目标维度无序列时整体不动作——诚实无数据）。
func evaluateScaling(policy state.ScalingPolicy, cur uint64, stateful bool,
	cpuLimitCores float64, memLimitBytes int64, cpu, mem scalingSample) scalingDecision {
	noop := scalingDecision{cpuPct: math.NaN(), memPct: math.NaN()}
	if cur == 0 {
		// scale-0 保留现场（首发失败/取消）：无运行负载可测，副本数归发布
		// 链路管辖，autoscaler 不越权唤醒。
		return noop
	}
	targetCPU := float64(policy.TargetCPUPct)
	targetMem := float64(policy.TargetMemPct)
	cpuEvaluable := policy.TargetCPUPct > 0 && cpuLimitCores > 0
	memEvaluable := policy.TargetMemPct > 0 && memLimitBytes > 0

	var candidates []uint64
	dims := ""
	cpuPct, memPct := math.NaN(), math.NaN()
	if cpuEvaluable {
		if !cpu.ok {
			return noop // 目标维度查不到序列：诚实不动作（披露在本步侧）
		}
		cpuPct = cpu.value / (float64(cur) * cpuLimitCores) * 100
		if prop, ok := scalingProposal(cur, targetCPU, cpuPct); ok {
			candidates = append(candidates, prop)
			dims = joinDimension(dims, "cpu")
		}
	}
	if memEvaluable {
		if !mem.ok {
			return noop
		}
		memPct = mem.value / float64(memLimitBytes) * 100
		if prop, ok := scalingProposal(cur, targetMem, memPct); ok {
			candidates = append(candidates, prop)
			dims = joinDimension(dims, "mem")
		}
	}
	if len(candidates) == 0 {
		return noop
	}
	// 多维触发取最大反解（满足最高需求方——双向保守）；随后在 int 域夹逼
	// [min,max]（窄化守卫：反解天花板 MaxInt32，策略词表 [1,16]——防御式
	// 收窄，畸形策略行不参与夹逼方向）。
	proposed := candidates[0]
	for _, c := range candidates[1:] {
		if c > proposed {
			proposed = c
		}
	}
	proposedInt := math.MaxInt32
	if proposed < math.MaxInt32 {
		proposedInt = int(proposed)
	}
	if proposedInt < policy.MinReplicas && policy.MinReplicas >= 1 {
		proposedInt = policy.MinReplicas
	}
	if policy.MaxReplicas >= 1 && proposedInt > policy.MaxReplicas {
		proposedInt = policy.MaxReplicas
	}
	if proposedInt < 0 {
		return noop // 词表外的畸形策略行：不动作（loud 前置在 state 写入校验）
	}
	proposed = uint64(proposedInt)
	if proposed == cur {
		return noop // 变化为 0 不动作（设计 §1.2）
	}
	if proposed < cur && stateful {
		return noop // stateful 只扩不缩（数据安全纪律，设计 §1.2 护栏）
	}
	return scalingDecision{actionable: true, proposed: proposed, dimension: dims, cpuPct: cpuPct, memPct: memPct}
}

// scalingProposal 是单维度的副本反解（HPA 同式 ceil(当前×实测/target)）：
//   - 实测 > target×1.10（扩带）→ 反解 > 当前（扩）；
//   - 实测 < target×0.70（缩带）→ 反解 < 当前（缩）；
//   - 死区（两阈值之间）不产出候选。
//
// ok=false 表示该维度本拍不触发。
func scalingProposal(cur uint64, target, pct float64) (proposed uint64, ok bool) {
	if pct > target*scalingUpThreshold || pct < target*scalingDownThreshold {
		return ceilReplicas(cur, pct, target), true
	}
	return 0, false
}

// ceilReplicas 是反解的向上取整（副本为整数；向上 = 扩方向足量、缩方向
// 保守——两向都取对调用方更安全的整侧）。
func ceilReplicas(cur uint64, pct, target float64) uint64 {
	v := math.Ceil(float64(cur) * pct / target)
	if v < 0 {
		return 0
	}
	if v > math.MaxUint32 {
		// 副本量级受 [1,16] 夹逼（max ≤16 的设计上限），天花板只为溢出防御。
		return math.MaxUint32
	}
	return uint64(v) //nolint:gosec // G115：上界守卫后收窄
}

// joinDimension 是触发维度串的拼接（去重——同维双触发不重复）。
func joinDimension(acc, dir string) string {
	if acc == "" {
		return dir
	}
	if acc == dir {
		return acc
	}
	return acc + "+" + dir
}

// AutoscalingTick 单步执行扩缩评估（测试与诊断显式入口——直通频控闸；
// 生产由 tick 周期驱动，safeCall 收口 MG-5）。
func (e *Engine) AutoscalingTick(ctx context.Context) { e.autoscaleApps(ctx, true) }

// autoscaleApps 是收敛拍尾部的扩缩步：频控（scanGate 单点时间闸，
// disclosure.go）→ 候选集 = 全部策略行 → 逐策略门槛检查 → 求值 → 动作。
func (e *Engine) autoscaleApps(ctx context.Context, force bool) {
	if e.metricsQ == nil {
		return // 查询面未装配：本步不在评估域（装配形态；无披露——不冒充运行态）
	}
	now := e.now()
	if !e.scalingScanGate.due(now, force, autoscalingInterval) {
		return
	}
	policies, err := e.store.ListScalingPolicies(ctx)
	if err != nil {
		e.log.Warn("engine: autoscaling list policies", "error", err)
		return
	}
	if len(policies) == 0 {
		return
	}
	in, err := e.store.LoadMetricsSettings(ctx)
	if err != nil {
		e.log.Warn("engine: autoscaling load metrics settings", "error", err)
		return
	}
	if in.Mode != state.MetricsModeOn {
		// 策略休眠（设计 §1.2）：每策略一次性披露；metrics 回 on 时清记忆
		//（本步的 on 路径逐策略 clear）——再离线可再披露一次。
		for _, p := range policies {
			e.discloseScalingOnce(ctx, &e.scalingDormantSeen, "scaling.dormant", p, "metrics_off")
		}
		return
	}
	// 门槛输入批量化：active app 集 / 在途部署集（漂移与存在性对账同款
	// 候选过滤——发布过程本身就是期望态迁移，不评估）。
	active := map[string]state.App{}
	if apps, err := e.store.ListActiveApps(ctx); err != nil {
		e.log.Warn("engine: autoscaling list apps", "error", err)
		return
	} else {
		for _, a := range apps {
			active[a.ID] = a
		}
	}
	inFlight := map[string]bool{}
	if rows, err := e.store.ListNonTerminalDeployments(ctx); err != nil {
		e.log.Warn("engine: autoscaling in-flight check", "error", err)
		return
	} else {
		for _, r := range rows {
			inFlight[r.AppID] = true
		}
	}
	for _, p := range policies {
		e.scalingDormantSeen.clear(p.AppID + "/" + p.Service) // metrics on：休眠解除（可再披露）
		app, ok := active[p.AppID]
		if !ok || inFlight[p.AppID] {
			continue
		}
		// 服务 running 门（设计 §1.2）：派生态 running 才求值——degraded/
		// down/blocked 各有归属路径，autoscaler 不在应用不健康时动副本。
		derived, err := e.store.GetAppDerivedState(ctx, app.ID)
		if err != nil {
			if !errors.Is(err, state.ErrAppNotFound) {
				e.log.Warn("engine: autoscaling read derived state", "app", app.Name, "error", err)
			}
			continue
		}
		if derived != DerivedRunning {
			continue // 空串（尚未推导）/degraded/blocked/down：一律不在评估域
		}
		e.evaluateScalingPolicy(ctx, app, p, now)
	}
}

// evaluateScalingPolicy 求值单个策略：门槛（派生 running、快照在场、非
// global、实况一致性）→ VM 采样 → 判据 → 平台写通道动作。
func (e *Engine) evaluateScalingPolicy(ctx context.Context, app state.App, p state.ScalingPolicy, now time.Time) {
	source, specs, err := e.lastSucceededSpecs(ctx, app.ID)
	if err != nil {
		e.log.Warn("engine: autoscaling read expectations", "app", app.Name, "error", err)
		return
	}
	if source == nil {
		return // 无成功部署：无期望态（判据无从建立）
	}
	var spec *ServiceSpec
	for i := range specs {
		if specs[i].ServiceLabels[state.LabelProcess] == p.Service {
			spec = &specs[i]
			break
		}
	}
	if spec == nil {
		return // 策略指向的服务不在当前期望集（已从 compose 移除）：如实跳过
	}
	if spec.Global {
		return // global 无副本语义（漂移投影的归一口径同源）
	}
	// 期望副本 = 活覆盖值，否则快照值；冷却窗只对活覆盖生效（部署换代即
	// 重置基线与冷却）。
	expected := spec.Replicas
	ov, ovErr := e.store.GetScalingReplicaOverride(ctx, app.ID, p.Service)
	ovLive := ovErr == nil && ov.DeploymentID == source.ID
	if ovLive {
		expected = ov.Replicas
		if cd := time.Duration(p.CooldownSeconds) * time.Second; now.Sub(ov.UpdatedAt) < cd {
			return // 冷却窗内不重复动作（设计 §1.2）
		}
	}
	// 实况一致性门：实况 ≠ 平台期望 = 外部改动或上次动作写失败在场——
	// 本拍不评估不动作（披露归漂移判据；autoscaler 不越权收敛）。
	st, err := e.sub.ServiceInspect(ctx, spec.Name)
	if err != nil {
		if !errors.Is(err, ErrServiceNotFound) {
			e.log.Warn("engine: autoscaling inspect failed (transient)", "app", app.Name, "service", p.Service, "error", err)
		}
		return
	}
	if st.Global || st.Replicas != expected {
		return
	}
	// 采样：查询面故障 ≠ 无数据——只日志退避（不披露不动作）；无序列 =
	// 诚实无数据，一次性披露（scaling.no_data）。
	cpu, err := e.queryScalingSample(ctx, scalingCPUPromQL(spec.Name))
	if err != nil {
		e.log.Warn("engine: autoscaling cpu query failed (transient)", "app", app.Name, "service", p.Service, "error", err)
		return
	}
	mem, err := e.queryScalingSample(ctx, scalingMemPromQL(spec.Name))
	if err != nil {
		e.log.Warn("engine: autoscaling mem query failed (transient)", "app", app.Name, "service", p.Service, "error", err)
		return
	}
	cpuLimitCores, memLimitBytes := scalingLimitsOf(*spec)
	cpuEvaluable := p.TargetCPUPct > 0 && cpuLimitCores > 0
	memEvaluable := p.TargetMemPct > 0 && memLimitBytes > 0
	if !cpuEvaluable && !memEvaluable {
		// 目标维度全部无限额：无可评估分母，策略恒不动作（配置形态——
		// 归一口径见包头注；CLI 帮助与 Console 卡同口径披露）。
		return
	}
	if (cpuEvaluable && !cpu.ok) || (memEvaluable && !mem.ok) {
		e.discloseScalingOnce(ctx, &e.scalingNoDataSeen, "scaling.no_data", p, "no_series")
		return
	}
	d := evaluateScaling(p, expected, len(spec.Mounts) > 0, cpuLimitCores, memLimitBytes, cpu, mem)
	if !d.actionable {
		e.scalingNoDataSeen.clear(p.AppID + "/" + p.Service) // 有数据：no_data 解除（可再披露）
		return
	}
	e.applyScalingDecision(ctx, app, p, source.ID, *spec, expected, d)
}

// queryScalingSample 是单次瞬时查询的样本包装。
func (e *Engine) queryScalingSample(ctx context.Context, promql string) (scalingSample, error) {
	v, ok, err := e.metricsQ.InstantValue(ctx, promql)
	if err != nil {
		return scalingSample{}, err
	}
	return scalingSample{value: v, ok: ok}, nil
}

// scalingLimitsOf 读取服务的资源限额（评估分母；返回值 0 = 该维度无限额
// ——不可评估，evaluateScaling 如实跳过该维度）。
func scalingLimitsOf(spec ServiceSpec) (cpuCores float64, memBytes int64) {
	if spec.Resources == nil {
		return 0, 0
	}
	return float64(spec.Resources.NanoCPUs) / 1e9, spec.Resources.MemoryBytes
}

// applyScalingDecision 执行动作（平台写通道）：底座 ServiceUpdate（快照
// spec 置新副本，label 与发布对账同形重锚——desired-hash 相同内容更新不
// 触发任务重建）→ 覆盖 upsert + scaling.adjusted 事件 + 审计（同事务
// fail-closed）。底座写失败：无任何状态变化，下一拍重新评估（幂等）；
// 事务失败：底座已更新而覆盖未落——下一拍一致性门挡住评估，漂移判据如实
// 披露失配（收敛原语按活覆盖重放即自愈）。
func (e *Engine) applyScalingDecision(ctx context.Context, app state.App, p state.ScalingPolicy,
	deploymentID string, spec ServiceSpec, current uint64, d scalingDecision) {
	before := current
	after := d.proposed
	next := spec
	next.Replicas = after
	if next.ServiceLabels == nil {
		next.ServiceLabels = map[string]string{}
	}
	next.ServiceLabels[state.LabelDeployment] = deploymentID
	next.ServiceLabels[state.LabelDesiredHash] = next.DesiredHash()
	if err := e.sub.ServiceUpdate(ctx, spec.Name, next); err != nil {
		e.log.Warn("engine: autoscaling service update failed (retrying next evaluation)",
			"app", app.Name, "service", p.Service, "error", err)
		return
	}
	payload := scalingAdjustPayload(d, before)
	adjustAt := e.now()
	if err := e.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.UpsertScalingReplicaOverride(ctx, state.ScalingReplicaOverride{
			AppID:        app.ID,
			Service:      p.Service,
			DeploymentID: deploymentID,
			Replicas:     after,
			UpdatedAt:    adjustAt, // 冷却窗时间锚 = 引擎时钟（相位锚同钟纪律）
		}); err != nil {
			return err
		}
		if err := appendEvents(ctx, tx, appEvent("scaling.adjusted", app.Name,
			append([]string{"app_id", app.ID, "service", p.Service}, payload...)...)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:  "system",
			Action: "scaling.adjusted",
			Target: "app:" + app.Name + "/" + p.Service,
			Result: "ok",
			DiffSummary: state.DiffSummary("service", p.Service,
				"dimension", d.dimension, "replicas_before", before, "replicas_after", after),
		})
	}); err != nil {
		e.log.Warn("engine: autoscaling record adjustment failed (view converges via drift/discipline)",
			"app", app.Name, "service", p.Service, "error", err)
		return
	}
	e.scalingNoDataSeen.clear(p.AppID + "/" + p.Service)
	e.log.Info("engine: autoscaler adjusted replicas",
		"app", app.Name, "service", p.Service, "dimension", d.dimension,
		"replicas_before", before, "replicas_after", after)
}

// scalingAdjustPayload 是 scaling.adjusted 事件的载荷 kv（维度/前后副本/
// 实测水位——设计 §1.2 metadata 清单；水位只出已评估维度，保留一位小数）。
func scalingAdjustPayload(d scalingDecision, before uint64) []string {
	kv := []string{"dimension", d.dimension,
		"replicas_before", fmt.Sprint(before), "replicas_after", fmt.Sprint(d.proposed)}
	if !math.IsNaN(d.cpuPct) {
		kv = append(kv, "cpu_pct", fmt.Sprintf("%.1f", d.cpuPct))
	}
	if !math.IsNaN(d.memPct) {
		kv = append(kv, "mem_pct", fmt.Sprintf("%.1f", d.memPct))
	}
	return kv
}

// discloseScalingOnce 是一次性披露的共享形态（dormant/no_data 同款）：
// 记忆命中即跳过（保持既有读序——命中时不触 store 读 app 行）；仅事件
// 无审计面（scaling.dormant / scaling.no_data 族的既有契约）；事件落库
// 成功才置记忆（失败下一拍重试——披露不丢）。配对与 once 语义经
// disclosure.go 单点。
func (e *Engine) discloseScalingOnce(ctx context.Context, set *disclosureSet, event string, p state.ScalingPolicy, reason string) {
	key := p.AppID + "/" + p.Service
	if set.reported(key) {
		return
	}
	app, err := e.store.GetAppByID(ctx, p.AppID)
	appName := p.AppID
	if err == nil {
		appName = app.Name
	}
	if _, err := e.discloseOnce(ctx, set, key, disclosure{
		eventCode: event,
		subject:   "app:" + appName,
		payload:   []string{"app", appName, "app_id", p.AppID, "service", p.Service, "reason", reason},
	}); err != nil {
		e.log.Warn("engine: autoscaling disclosure failed (retrying)", "event", event, "app", appName, "error", err)
	}
}

// pinScalingReplicaOverrides 把活覆盖（deployment_id == 期望态来源部署）的
// 副本数钉进期望 spec 集——漂移投影（computeAppDrift）与归位收敛
// （convergeApp）的共享期望修正面：漂移对账以「平台自己写的运行期副本」
// 为期望（D-V3W5-2），平台写不被误报、不被收敛回滚；部署换代后覆盖自然
// 失活（旧 deployment_id 不命中）。覆盖行以 compose 服务名键（与策略同键
// 域），经服务 label fleetly.process 与 Swarm 服务名互译。
func pinScalingReplicaOverrides(specs []ServiceSpec, overrides []state.ScalingReplicaOverride, deploymentID string) {
	byService := make(map[string]uint64, len(overrides))
	for _, ov := range overrides {
		if ov.DeploymentID == deploymentID {
			byService[ov.Service] = ov.Replicas
		}
	}
	if len(byService) == 0 {
		return
	}
	for i := range specs {
		if n, ok := byService[specs[i].ServiceLabels[state.LabelProcess]]; ok {
			specs[i].Replicas = n
		}
	}
}
