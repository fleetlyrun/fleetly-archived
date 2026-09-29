package engine

// 自动扩缩测试（W5-S1，D-V3W5-2）：判据表驱动（扩/缩/死区/夹逼/零变化/
// stateful 只扩不缩/无限额跳过/无数据不动作）+ 三红线（①策略调整生效
// ②漂移对账不回滚平台写的副本 ③外部 docker service scale 漂移检测仍有效）
// + 冷却窗抑制 + 休眠/无数据一次性披露。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// fakeQuerier 是 MetricsQuerier 的注入实现（按 PromQL 字面出样本；缺省
// 无序列——诚实无数据形态）。
type fakeQuerier struct {
	values map[string]scalingSample
	err    error
}

func (f *fakeQuerier) InstantValue(_ context.Context, promql string) (float64, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	s, ok := f.values[promql]
	if !ok {
		return 0, false, nil
	}
	return s.value, s.ok, nil
}

func (f *fakeQuerier) set(promql string, v float64) {
	if f.values == nil {
		f.values = map[string]scalingSample{}
	}
	f.values[promql] = scalingSample{value: v, ok: true}
}

// sampleOf 便捷构造（有序列样本）。
func sampleOf(v float64) scalingSample { return scalingSample{value: v, ok: true} }

// noSeries 是「查不到序列」样本。
var noSeries = scalingSample{}

// polOf 是策略便捷构造（缺省 min1/max4/cpu60/mem70/cooldown180）。
func polOf(mutate func(p *state.ScalingPolicy)) state.ScalingPolicy {
	p := state.ScalingPolicy{Service: "web", MinReplicas: 1, MaxReplicas: 4,
		TargetCPUPct: 60, TargetMemPct: 70, CooldownSeconds: 180}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

// limitsAndSamples 是评估表驱动常用的分母（1 核 / 1GiB——样本值即百分数）。
const (
	testCores = 1.0
	testBytes = 1 << 30
)

// cpuRaw / memRaw 把「水位百分数」换算为评估器的 raw 样本（分母固定
// cur×1 核 / 1GiB——表驱动用例以水位语义书写，可读性优先）。
func cpuRaw(pct float64, cur uint64) float64 { return pct / 100 * float64(cur) * testCores }

func memRaw(pct float64) float64 { return pct / 100 * float64(testBytes) }

func TestScalingEvaluationCriteria(t *testing.T) {
	cases := []struct {
		name     string
		policy   state.ScalingPolicy
		cur      uint64
		stateful bool
		cores    float64
		bytes    int64
		cpu      scalingSample
		mem      scalingSample
		want     scalingDecision
	}{
		{
			// cpu 90% > 66（60×1.1）扩带 → ceil(1×90/60)=2；mem 60% ∈
			// (49,77) 死区无候选。
			name:   "cpu above up-threshold scales up",
			policy: polOf(nil), cur: 1, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(90, 1)), mem: sampleOf(memRaw(60)),
			want: scalingDecision{actionable: true, proposed: 2, dimension: "cpu"},
		},
		{
			// mem 160% > 77（70×1.1）扩带 → ceil(2×160/70)=5 → 夹 max=4；
			// cpu 60% 死区。
			name:   "mem above up-threshold scales up (clamped to max)",
			policy: polOf(nil), cur: 2, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(60, 2)), mem: sampleOf(memRaw(160)),
			want: scalingDecision{actionable: true, proposed: 4, dimension: "mem"},
		},
		{
			// 两维均入缩带（cpu 20% < 42、mem 20% < 49）→ 各反解 2，max=2。
			name:   "both dimensions below down-threshold scale down",
			policy: polOf(nil), cur: 4, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(20, 4)), mem: sampleOf(memRaw(20)),
			want: scalingDecision{actionable: true, proposed: 2, dimension: "cpu+mem"},
		},
		{
			// 双维触发取最大反解：cpu 240% → 8（夹 4）；mem 100% → 3；max=4。
			name:   "multi-dimension takes the max proposal",
			policy: polOf(nil), cur: 2, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(240, 2)), mem: sampleOf(memRaw(100)),
			want: scalingDecision{actionable: true, proposed: 4, dimension: "cpu+mem"},
		},
		{
			// 死区（cpu 65% ∈ (42,66)、mem 50% ∈ (49,77)）：不动作。
			name:   "dead zone does not act",
			policy: polOf(nil), cur: 4, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(65, 4)), mem: sampleOf(memRaw(50)),
			want: scalingDecision{actionable: false},
		},
		{
			// 触发但反解经 ceil 折回当前副本：变化为 0 不动作（缩带 + 单副本
			// 形态，min=1 夹逼同理）。
			name:   "zero change does not act",
			policy: polOf(nil), cur: 1, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(30, 1)), mem: sampleOf(memRaw(35)),
			want: scalingDecision{actionable: false},
		},
		{
			// stateful 只扩不缩护栏：扩向放行。
			name:   "scale-up on stateful is allowed",
			policy: polOf(nil), cur: 1, stateful: true, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(90, 1)), mem: sampleOf(memRaw(60)),
			want: scalingDecision{actionable: true, proposed: 2, dimension: "cpu"},
		},
		{
			// stateful 只扩不缩护栏：低水位触发缩 → 抑制。
			name:   "scale-down on stateful is suppressed (only-up guard)",
			policy: polOf(nil), cur: 2, stateful: true, cores: testCores, bytes: testBytes,
			cpu: sampleOf(cpuRaw(20, 2)), mem: sampleOf(memRaw(20)),
			want: scalingDecision{actionable: false},
		},
		{
			// 目标维度查不到序列：诚实无数据不动作。
			name:   "no series on a targeted dimension does not act",
			policy: polOf(nil), cur: 1, cores: testCores, bytes: testBytes,
			cpu: noSeries, mem: sampleOf(memRaw(10)),
			want: scalingDecision{actionable: false},
		},
		{
			// 两维都无限额：无可评估分母，恒不动作（归一口径的诚实缺省）。
			name:   "unlimited dimensions are skipped",
			policy: polOf(nil), cur: 1, cores: 0, bytes: 0,
			cpu: sampleOf(0.9), mem: sampleOf(0.9),
			want: scalingDecision{actionable: false},
		},
		{
			// scale-0 保留现场：副本数归发布链路管辖，不评估。
			name:   "zero current replicas is not autoscaler's domain",
			policy: polOf(nil), cur: 0, cores: testCores, bytes: testBytes,
			cpu: sampleOf(0), mem: sampleOf(0),
			want: scalingDecision{actionable: false},
		},
		{
			// 未设目标的维度不评估（无限额 + 无序列也不阻断另一维）。
			name:   "untargeted dimension does not evaluate",
			policy: polOf(func(p *state.ScalingPolicy) { p.TargetMemPct = 0 }),
			cur:    1, cores: testCores, bytes: 0,
			cpu: sampleOf(cpuRaw(90, 1)), mem: noSeries,
			want: scalingDecision{actionable: true, proposed: 2, dimension: "cpu"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateScaling(tc.policy, tc.cur, tc.stateful, tc.cores, tc.bytes, tc.cpu, tc.mem)
			if got.actionable != tc.want.actionable {
				t.Fatalf("actionable = %v (got %+v), want %v", got.actionable, got, tc.want.actionable)
			}
			if tc.want.actionable {
				if got.proposed != tc.want.proposed || got.dimension != tc.want.dimension {
					t.Fatalf("decision = %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

// scalingSetup 驱动一次带限额部署至 succeeded，布好 metrics on + 策略，
// 返回查询注入口。cpu/mem 限额 1 核 / 1GiB——样本值即水位百分数小数形态。
func scalingSetup(t *testing.T) (*harness, *fakeQuerier, state.App) {
	t.Helper()
	h := newHarness(t)
	q := &fakeQuerier{}
	h.eng = h.eng.WithMetricsQuerier(q)
	ctx := context.Background()
	path := h.writeCompose(`name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
    deploy:
      resources:
        limits:
          cpus: "1.0"
          memory: "1073741824"
`)
	if final := h.runToTerminal(h.enqueue(path)); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	if err := h.store.SaveMetricsSettings(ctx, state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("metrics on: %v", err)
	}
	app := h.demoApp()
	if _, err := h.store.SetScalingPolicy(ctx, app.ID, polOf(nil), state.ScalingPolicyOptions{Actor: "human"}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	return h, q, app
}

// TestAutoscalingAppsAdjustsReplicas 红线①：策略调整经平台写通道生效——底座
// 副本更新 + 覆盖行落库 + scaling.adjusted 事件/审计。
func TestAutoscalingAppsAdjustsReplicas(t *testing.T) {
	h, q, app := scalingSetup(t)
	ctx := context.Background()
	svcName := h.svc("web")
	cpuQ, memQ := scalingCPUPromQL(svcName), scalingMemPromQL(svcName)
	q.set(cpuQ, 0.9) // 90% > 66 → 扩
	q.set(memQ, 0.1)

	h.eng.AutoscalingTick(ctx)

	st, err := h.sub.ServiceInspect(ctx, svcName)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Replicas != 2 {
		t.Fatalf("replicas = %d, want 2 (ceil(1×90/60))", st.Replicas)
	}
	ov, err := h.store.GetScalingReplicaOverride(ctx, app.ID, "web")
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if ov.Replicas != 2 || ov.DeploymentID == "" {
		t.Fatalf("overlay = %+v, want replicas 2 pinned to the source deployment", ov)
	}
	if !hasEvent(h.events(), "scaling.adjusted") {
		t.Fatalf("events missing scaling.adjusted: %v", h.events())
	}
	entries, _, err := h.store.ListAudits(ctx, state.AuditQuery{Action: "scaling.adjusted", Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit scaling.adjusted = %v (%d entries), want 1", err, len(entries))
	}
}

// TestScalingDriftRedLines 红线②③：平台写的副本不被漂移误报、不被收敛
// 回滚；外部 docker service scale 的漂移检测仍有效。
func TestScalingDriftRedLines(t *testing.T) {
	h, q, app := scalingSetup(t)
	ctx := context.Background()
	svcName := h.svc("web")
	q.set(scalingCPUPromQL(svcName), 0.9)
	q.set(scalingMemPromQL(svcName), 0.1)
	h.eng.AutoscalingTick(ctx) // 1 → 2（平台写）

	// 基线：平台写后无漂移（期望 = 覆盖副本 2，实况 2）。
	report, err := h.eng.DriftShow(ctx, "demo")
	if err != nil {
		t.Fatalf("drift show: %v", err)
	}
	if report.Drifted {
		t.Fatalf("red line ② violated: platform-written replicas reported as drift: %+v", report)
	}

	// 红线②：人工收敛不回滚平台写的副本（归位重放以活覆盖为期望）。
	if _, err := h.eng.ConvergeApp(ctx, "demo", "human"); err != nil {
		t.Fatalf("converge: %v", err)
	}
	st, err := h.sub.ServiceInspect(ctx, svcName)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Replicas != 2 {
		t.Fatalf("red line ② violated: converge rolled platform-written replicas back to %d, want 2", st.Replicas)
	}

	// 红线③：外部 docker service scale（2 → 5，绕过平台写）仍被漂移检出。
	h.sub.mutateExternal(svcName, func(spec *ServiceSpec) { spec.Replicas = 5 })
	h.eng.DriftScan(ctx)
	report, err = h.eng.DriftShow(ctx, "demo")
	if err != nil {
		t.Fatalf("drift show: %v", err)
	}
	if !report.Drifted {
		t.Fatalf("red line ③ violated: external docker service scale not detected: %+v", report)
	}
	found := false
	for _, s := range report.Services {
		for _, d := range s.Diff {
			if d.Field == "replicas" {
				found = true
				if d.Expected != "2" || d.Actual != "5" {
					t.Fatalf("replicas diff = %v→%v, want 2→5 (expected pins the platform-written overlay)", d.Expected, d.Actual)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no replicas diff in report: %+v", report.Services)
	}
	_ = app
}

// TestScalingCooldownSuppressesRepeat 冷却窗内不重复动作；过期后恢复。
func TestScalingCooldownSuppressesRepeat(t *testing.T) {
	h, q, _ := scalingSetup(t)
	ctx := context.Background()
	svcName := h.svc("web")
	q.set(scalingCPUPromQL(svcName), 3.0) // 持续 300%（夹上限 4 也仍超载）
	q.set(scalingMemPromQL(svcName), 0.1)

	h.eng.AutoscalingTick(ctx)
	if st, err := h.sub.ServiceInspect(ctx, svcName); err != nil || st.Replicas != 4 {
		t.Fatalf("first adjustment = %d (%v), want 4 (clamp to max)", stReplicasOf(h, svcName), err)
	}

	// 冷却窗内（180s；推进 60s）：不重复动作。
	h.clk.Advance(60 * time.Second)
	h.eng.AutoscalingTick(ctx)
	if n := countScalingAdjustEvents(t, h); n != 1 {
		t.Fatalf("cooldown violated: %d adjustments, want 1", n)
	}

	// 冷却窗过期（再推进 200s）：恢复动作（副本已在 max=4，零变化不动作
	// ——把 max 提到 8 验证动作恢复）。
	app := h.demoApp()
	if _, err := h.store.SetScalingPolicy(ctx, app.ID, polOf(func(p *state.ScalingPolicy) { p.MaxReplicas = 8 }),
		state.ScalingPolicyOptions{Actor: "human"}); err != nil {
		t.Fatalf("raise max: %v", err)
	}
	h.clk.Advance(200 * time.Second)
	h.eng.AutoscalingTick(ctx)
	if n := countScalingAdjustEvents(t, h); n != 2 {
		t.Fatalf("post-cooldown adjustment missing: %d adjustments, want 2", n)
	}
}

// TestScalingDormantDisclosure metrics off：休眠一次性披露 + 零动作；回 on
// 解除，再 off 可再披露。
func TestScalingDormantDisclosure(t *testing.T) {
	h, q, app := scalingSetup(t)
	ctx := context.Background()
	svcName := h.svc("web")
	q.set(scalingCPUPromQL(svcName), 0.9)
	q.set(scalingMemPromQL(svcName), 0.1)

	// metrics off：披露一次、不动副本。
	if err := h.store.SaveMetricsSettings(ctx, state.MetricsModeUnset, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("metrics off: %v", err)
	}
	h.eng.AutoscalingTick(ctx)
	h.eng.AutoscalingTick(ctx) // 存续不重复
	if n := countEvents(t, h, "scaling.dormant"); n != 1 {
		t.Fatalf("dormant events = %d, want 1 (one-time disclosure)", n)
	}
	if st, _ := h.sub.ServiceInspect(ctx, svcName); st.Replicas != 1 {
		t.Fatalf("replicas moved while dormant = %d, want 1", st.Replicas)
	}

	// 回 on（记忆解除）→ 再 off（可再披露）。
	if err := h.store.SaveMetricsSettings(ctx, state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("metrics on: %v", err)
	}
	h.eng.AutoscalingTick(ctx) // on：正常动作（1→2）
	if err := h.store.SaveMetricsSettings(ctx, state.MetricsModeUnset, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("metrics off: %v", err)
	}
	h.eng.AutoscalingTick(ctx)
	if n := countEvents(t, h, "scaling.dormant"); n != 2 {
		t.Fatalf("dormant events after re-arm = %d, want 2", n)
	}
	_ = app
}

// TestScalingNoDataDisclosure 有策略、metrics on、查不到序列：不动作 +
// scaling.no_data 一次性披露；数据出现后解除。
func TestScalingNoDataDisclosure(t *testing.T) {
	h, q, _ := scalingSetup(t)
	ctx := context.Background()
	svcName := h.svc("web")

	h.eng.AutoscalingTick(ctx)
	h.eng.AutoscalingTick(ctx)
	if n := countEvents(t, h, "scaling.no_data"); n != 1 {
		t.Fatalf("no_data events = %d, want 1 (one-time disclosure)", n)
	}
	if st, _ := h.sub.ServiceInspect(ctx, svcName); st.Replicas != 1 {
		t.Fatalf("replicas moved without data = %d, want 1", st.Replicas)
	}
	if !hasEvent(h.events(), "scaling.adjusted") {
		// 数据出现（CPU 超载）→ 动作恢复，no_data 不再发。
		q.set(scalingCPUPromQL(svcName), 0.9)
		q.set(scalingMemPromQL(svcName), 0.1)
		h.eng.AutoscalingTick(ctx)
		if n := countEvents(t, h, "scaling.no_data"); n != 1 {
			t.Fatalf("no_data re-emitted with data present = %d, want 1", n)
		}
		if st, _ := h.sub.ServiceInspect(ctx, svcName); st.Replicas != 2 {
			t.Fatalf("replicas after data = %d, want 2", st.Replicas)
		}
	}
}

// TestScalingBackendErrorIsNotNoData 查询面故障 ≠ 无数据：不动作、不披露
// （诚实口径——披露只属于「查不到序列」）。
func TestScalingBackendErrorIsNotNoData(t *testing.T) {
	h, q, _ := scalingSetup(t)
	ctx := context.Background()
	q.err = errors.New("vm unreachable")

	h.eng.AutoscalingTick(ctx)
	if n := countEvents(t, h, "scaling.no_data"); n != 0 {
		t.Fatalf("no_data disclosed on backend error = %d, want 0 (error is not no-data)", n)
	}
	if hasEvent(h.events(), "scaling.adjusted") {
		t.Fatal("action taken while backend unreachable")
	}
}

// TestScalingUnlimitedServiceNeverActs 无限额服务：无可评估分母恒不动作
// （归一口径的诚实缺省——臆造分母即伪造利用率）。
func TestScalingUnlimitedServiceNeverActs(t *testing.T) {
	h := newHarness(t)
	q := &fakeQuerier{}
	h.eng = h.eng.WithMetricsQuerier(q)
	ctx := context.Background()
	path := h.writeCompose(composeV1) // 无 deploy.resources
	if final := h.runToTerminal(h.enqueue(path)); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	if err := h.store.SaveMetricsSettings(ctx, state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("metrics on: %v", err)
	}
	app := h.demoApp()
	if _, err := h.store.SetScalingPolicy(ctx, app.ID, polOf(nil), state.ScalingPolicyOptions{Actor: "human"}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	q.set(scalingCPUPromQL(h.svc("web")), 0.99)
	q.set(scalingMemPromQL(h.svc("web")), 0.99)
	h.eng.AutoscalingTick(ctx)
	if st, _ := h.sub.ServiceInspect(ctx, h.svc("web")); st.Replicas != 1 {
		t.Fatalf("unlimited service scaled = %d, want 1 (no evaluable denominator)", st.Replicas)
	}
}

// TestScalingGlobalServiceSkipped global 服务无副本语义：不评估不动作。
// 受控子集已拒绝新声明 global（v0.1 起）——沿 TestDriftGlobalServiceNoFalsePositive
// 的存量行播种形态（只有存量快照能到达该形态）。
func TestScalingGlobalServiceSkipped(t *testing.T) {
	h := newHarness(t)
	q := &fakeQuerier{}
	h.eng = h.eng.WithMetricsQuerier(q)
	ctx := context.Background()
	app, err := ensureAppForTest(t, ctx, h.store, "demo")
	if err != nil {
		t.Fatalf("ensure app: %v", err)
	}
	spec := ServiceSpec{
		Name:     "fleetly-demo-agent",
		Image:    "alpine:3",
		Global:   true,
		Replicas: 1,
		ServiceLabels: map[string]string{
			state.LabelManaged: "true", state.LabelApp: "demo", state.LabelProcess: "web",
		},
		ContainerLabels: map[string]string{state.LabelApp: "demo"},
		Networks:        []NetworkAttach{{Name: h.demoNet(), Aliases: []string{"agent"}}},
		Resources:       &ResourcesSpec{NanoCPUs: 1_000_000_000, MemoryBytes: 1 << 30},
		UpdateOrder:     "stop-first", UpdateParallelism: 1,
	}
	if err := h.sub.ServiceCreate(ctx, spec); err != nil {
		t.Fatalf("seed global service: %v", err)
	}
	raw, err := canonicalJSON([]ServiceSpec{spec})
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	ct, err := h.box.Encrypt(raw)
	if err != nil {
		t.Fatalf("encrypt snapshot: %v", err)
	}
	rec, err := h.store.CreateDeployment(ctx, state.DeployRecord{
		AppID: app.ID, AppName: "demo", Kind: "deploy", DesiredSpec: string(ct),
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if err := h.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE deployments SET status = 'succeeded', desired_hash = ? WHERE id = ?`,
			spec.DesiredHash(), rec.ID)
		return err
	}); err != nil {
		t.Fatalf("seed succeeded row: %v", err)
	}
	// 派生态推导（本步的 running 门会查；直推一次使门槛通过——本测试的
	// 被测面是 global 跳过，不是 running 门）。
	if err := h.eng.refreshDerivedState(ctx, app.ID, "demo"); err != nil {
		t.Fatalf("refresh derived: %v", err)
	}
	if err := h.store.SaveMetricsSettings(ctx, state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("metrics on: %v", err)
	}
	if _, err := h.store.SetScalingPolicy(ctx, app.ID, polOf(nil), state.ScalingPolicyOptions{Actor: "human"}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	q.set(scalingCPUPromQL(spec.Name), 0.9)
	q.set(scalingMemPromQL(spec.Name), 0.1)
	h.eng.AutoscalingTick(ctx)
	if hasEvent(h.events(), "scaling.adjusted") {
		t.Fatal("global service evaluated by the autoscaler (no replica semantics)")
	}
}

// TestScalingSkipsWhileDeploymentInFlight 在途部署在场：不评估（发布本身
// 就是期望态迁移）。
func TestScalingSkipsWhileDeploymentInFlight(t *testing.T) {
	h, q, _ := scalingSetup(t)
	ctx := context.Background()
	svcName := h.svc("web")
	q.set(scalingCPUPromQL(svcName), 0.9)
	q.set(scalingMemPromQL(svcName), 0.1)

	// 入队一个新部署但不推进（queued 在场 = 在途）。
	path := h.writeCompose(`name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
    deploy:
      resources:
        limits:
          cpus: "1.0"
          memory: "1073741824"
`)
	_ = h.enqueue(path)
	h.eng.AutoscalingTick(ctx)
	if st, _ := h.sub.ServiceInspect(ctx, svcName); st.Replicas != 1 {
		t.Fatalf("replicas changed while a deployment is in flight = %d, want 1", st.Replicas)
	}
}

// TestScalingStatefulGuardEndToEnd stateful（有卷）服务端到端：超载只扩；
// 低水位不缩（缩向抑制直接落回底座实况断言）。
func TestScalingStatefulGuardEndToEnd(t *testing.T) {
	h := newHarness(t)
	q := &fakeQuerier{}
	h.eng = h.eng.WithMetricsQuerier(q)
	ctx := context.Background()
	path := h.writeCompose(`name: demo
services:
  db:
    image: alpine:3
    command: ["sleep", "infinity"]
    volumes:
      - data:/data
    deploy:
      resources:
        limits:
          cpus: "1.0"
          memory: "1073741824"
volumes:
  data:
`)
	if final := h.runToTerminal(h.enqueue(path)); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	if err := h.store.SaveMetricsSettings(ctx, state.MetricsModeOn, state.MetricsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("metrics on: %v", err)
	}
	app := h.demoApp()
	pol := polOf(func(p *state.ScalingPolicy) {
		p.Service = "db"
		p.TargetMemPct = 0 // 单维 CPU
	})
	if _, err := h.store.SetScalingPolicy(ctx, app.ID, pol, state.ScalingPolicyOptions{Actor: "human"}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	svcName := h.svc("db")
	cpuQ := scalingCPUPromQL(svcName)

	// 超载：只扩护栏放行扩向（1 → 2）。
	q.set(cpuQ, cpuRaw(90, 1))
	h.eng.AutoscalingTick(ctx)
	if st, err := h.sub.ServiceInspect(ctx, svcName); err != nil || st.Replicas != 2 {
		t.Fatalf("stateful scale-up = %d (%v), want 2", stReplicasOf(h, svcName), err)
	}

	// 低水位：缩向被护栏抑制（2 → 拟 1 不动作）。
	q.set(cpuQ, cpuRaw(10, 2))
	h.eng.AutoscalingTick(ctx)
	if st, err := h.sub.ServiceInspect(ctx, svcName); err != nil || st.Replicas != 2 {
		t.Fatalf("stateful shrink not suppressed = %d (%v), want 2 (only-up guard)", stReplicasOf(h, svcName), err)
	}
}

// stReplicasOf 是测试读副本的便捷形态。
func stReplicasOf(h *harness, svc string) uint64 {
	st, err := h.sub.ServiceInspect(context.Background(), svc)
	if err != nil {
		return 0
	}
	return st.Replicas
}

// countScalingAdjustEvents 统计 scaling.adjusted 事件数。
func countScalingAdjustEvents(t *testing.T, h *harness) int {
	t.Helper()
	return countEvents(t, h, "scaling.adjusted")
}
