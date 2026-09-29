package engine

// 运行期存在性对账测试（T0-V2.2/R2）：service 缺失 → 事件落库 + 派生态
// 修正（running → down）+ 持续缺失节流（不重复发事件）+ 服务恢复后可再报
//（非永久静音）；service 存在（含外部 scale=0）→ 零事件；非 running 派生
// 态不是候选；substrate 瞬态错误 → 零事件、有日志、不被当成缺失；tick
// tick 步的时间闸频控。

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// captureHandler 捕获日志记录（对账步「瞬态错误只进日志」断言用）。
type captureHandler struct {
	msgs []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// countEventsByName 统计至今某事件名的落库条数。
func countEventsByName(t *testing.T, h *harness, name string) int {
	t.Helper()
	got := 0
	for _, n := range h.events() {
		if n == name {
			got++
		}
	}
	return got
}

// derivedStateOf 读应用派生状态缓存（测试断言面）。
func derivedStateOf(t *testing.T, h *harness, appID string) string {
	t.Helper()
	v, err := h.store.GetAppDerivedState(context.Background(), appID)
	if err != nil {
		t.Fatalf("read derived state: %v", err)
	}
	return v
}

// flipDerivedState 手工翻转派生状态（模拟 refreshDerivedState 按 DB 事实
// 重推导的并发写——节流断言的再置位面）。
func flipDerivedState(t *testing.T, h *harness, appID, expected, next string) {
	t.Helper()
	err := h.store.InTx(context.Background(), func(tx *state.Tx) error {
		return tx.SetAppDerivedState(context.Background(), appID, expected, next)
	})
	if err != nil {
		t.Fatalf("flip derived state %s -> %s: %v", expected, next, err)
	}
}

// deployDemoSucceeded 是对账测试的前置：demo 应用首发成功（派生态 =
// running，service fleetly-demo-web 在底座存在）。
func deployDemoSucceeded(t *testing.T, h *harness) state.App {
	t.Helper()
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	app, err := h.store.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "running" {
		t.Fatalf("precondition: derived state = %q, want running", derived)
	}
	return app
}

// TestSubstrateReconMissingServiceDisclosesCorrectsAndThrottles 核心路径：
// 外部 docker service rm → app.substrate_missing 事件 + 派生态 running →
// down；持续缺失（即使派生态被重推导回 running）不重复发事件（节流）；
// 服务恢复 → 记忆清零，再次缺失可再报（非永久静音）。
func TestSubstrateReconMissingServiceDisclosesCorrectsAndThrottles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := deployDemoSucceeded(t, h)

	// 外部 docker service rm：期望服务整体缺失。
	if err := h.sub.ServiceRemove(ctx, h.svc("web")); err != nil {
		t.Fatalf("remove service: %v", err)
	}

	h.eng.SubstrateRecon(ctx)

	if got := countEventsByName(t, h, "app.substrate_missing"); got != 1 {
		t.Fatalf("app.substrate_missing events = %d, want exactly 1 after first scan", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "down" {
		t.Fatalf("derived state = %q, want down (honest correction)", derived)
	}

	// 节流：派生态被其他路径重推导回 running（refreshDerivedState 只看 DB
	// 事实，会翻转回来）后再次扫描——缺失存续不得事件风暴。
	flipDerivedState(t, h, app.ID, "down", "running")
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 1 {
		t.Fatalf("app.substrate_missing events = %d after rescans, want still 1 (throttle broken)", got)
	}

	// 服务恢复（重新存在即收敛：判据是存在性）→ 记忆清零、零事件。
	if err := h.sub.ServiceCreate(ctx, ServiceSpec{Name: h.svc("web"), Image: "alpine:3", Replicas: 1}); err != nil {
		t.Fatalf("recreate service: %v", err)
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 1 {
		t.Fatalf("unexpected extra events on recovery: %d", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "running" {
		t.Fatalf("derived state = %q after recovery, want running (untouched)", derived)
	}

	// 再次缺失 → 可再报（非永久静音），派生态再次修正。
	if err := h.sub.ServiceRemove(ctx, h.svc("web")); err != nil {
		t.Fatalf("remove service again: %v", err)
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 2 {
		t.Fatalf("app.substrate_missing events = %d, want 2 (memory cleared on recovery)", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "down" {
		t.Fatalf("derived state = %q after second absence, want down", derived)
	}
}

// TestSubstrateReconSilentWhenServiceExists 反向保护：service 存在（健康
// 与外部 scale=0 形态）→ 零事件、派生态不动——判据是存在性，不是副本数
// （paused/保留现场应用 service 仍在，不得误报）。
func TestSubstrateReconSilentWhenServiceExists(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := deployDemoSucceeded(t, h)

	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 0 {
		t.Fatalf("false missing on healthy baseline: %d events", got)
	}

	// 外部 docker service scale --replicas=0：service 仍在、副本归零。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) { spec.Replicas = 0 })
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 0 {
		t.Fatalf("replicas=0 misjudged as missing (criterion must be service existence): %d events", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "running" {
		t.Fatalf("derived state = %q, want running (untouched)", derived)
	}

	// 非 running 派生态不是候选（degraded/blocked/down 各有归属路径）。
	flipDerivedState(t, h, app.ID, "running", "degraded")
	if err := h.sub.ServiceRemove(ctx, h.svc("web")); err != nil {
		t.Fatalf("remove service: %v", err)
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 0 {
		t.Fatalf("non-running app wrongly reconciled: %d events", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "degraded" {
		t.Fatalf("derived state = %q, want degraded (untouched)", derived)
	}
}

// TestSubstrateReconSubstrateErrorIsNotMissing 平台故障保护：substrate
// API 错误（超时/不可达）≠ 服务缺失——零事件、派生态不动、错误只进日志；
// 错误恢复后对账正常工作（记忆未被污染，真实缺失仍可检出）。
func TestSubstrateReconSubstrateErrorIsNotMissing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := deployDemoSucceeded(t, h)

	cap := &captureHandler{}
	h.eng.log = slog.New(cap)

	h.sub.failInspectErr = errors.New("docker daemon timeout (injected)")
	h.eng.SubstrateRecon(ctx)

	if got := countEventsByName(t, h, "app.substrate_missing"); got != 0 {
		t.Fatalf("substrate error misjudged as missing: %d events", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "running" {
		t.Fatalf("derived state = %q, want running (error must not correct the view)", derived)
	}
	found := false
	for _, m := range cap.msgs {
		if m == "engine: substrate recon inspect failed (transient, not counted as missing)" {
			found = true
		}
	}
	if !found {
		t.Fatalf("substrate error swallowed silently, want a Warn log; got %v", cap.msgs)
	}

	// 错误恢复 + 真实缺失：下一拍正常检出（瞬态错误未被当成缺失，也未
	// 污染节流记忆）。
	h.sub.failInspectErr = nil
	if err := h.sub.ServiceRemove(ctx, h.svc("web")); err != nil {
		t.Fatalf("remove service: %v", err)
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 1 {
		t.Fatalf("app.substrate_missing events = %d, want 1 after error cleared", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "down" {
		t.Fatalf("derived state = %q, want down after real absence detected", derived)
	}
}

// TestSubstrateReconTimeGateSkipsBeats tick tick 步的时间闸频控：闸内拍子不
// 触达底座（ServiceInspect 计数不增长），闸过恢复扫描。
func TestSubstrateReconTimeGateSkipsBeats(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	deployDemoSucceeded(t, h)

	h.eng.substrateRecon(ctx, false) // 首拍即扫（scanGate 零值同语义：重启后立即扫一拍）
	afterFirst := h.sub.inspectCalls

	h.eng.substrateRecon(ctx, false) // 时钟未动：闸内跳过，不触底座
	if h.sub.inspectCalls != afterFirst {
		t.Fatalf("gated beat touched the substrate (inspect calls %d -> %d, frequency control broken)",
			afterFirst, h.sub.inspectCalls)
	}

	h.clk.Advance(substrateReconInterval + time.Second)
	h.eng.substrateRecon(ctx, false)
	if h.sub.inspectCalls <= afterFirst {
		t.Fatalf("gate never reopened (inspect calls still %d)", h.sub.inspectCalls)
	}
}

// TestSubstrateReconDrainedTasksDiscloseDegradedAndRecover F11（settled
// 应用 drain 事件面）：服务在、期望副本不变、running 任务=0（节点 drain /
// 任务被外部停掉——存在性判据探不到的盲区）→ app.degraded 披露（payload
// 带水位摘要）+ 派生态 running → degraded + 审计 app.substrate_drained；
// 持续无任务节流；任务回岗 → refreshDerivedState 单写点重推导回 running
// 并发 app.recovered；再次 drain 可再报（非永久静音）。
func TestSubstrateReconDrainedTasksDiscloseDegradedAndRecover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := deployDemoSucceeded(t, h)

	// drain 形态：任务旧代 shutdown + 新代滞留 pending（desired 副本不变）。
	drainedTasks := []TaskState{
		{ID: "t-old", State: "shutdown", DesiredState: "shutdown", Image: "img"},
		{ID: "t-new-1", State: "pending", DesiredState: "running", Image: "img"},
	}
	h.sub.setExternalTasks(h.svc("web"), drainedTasks)

	h.eng.SubstrateRecon(ctx)

	if got := countEventsByName(t, h, "app.degraded"); got != 1 {
		t.Fatalf("app.degraded events = %d, want exactly 1 after drain detected", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "degraded" {
		t.Fatalf("derived state = %q, want degraded (honest correction for zero running tasks)", derived)
	}
	rows, err := h.store.RecentAudits(ctx, 50)
	if err != nil {
		t.Fatalf("recent audits: %v", err)
	}
	auditFound := false
	for _, r := range rows {
		if r.Action == "app.substrate_drained" && r.Target == "app:"+app.Name && r.Result == "ok" {
			auditFound = true
		}
	}
	if !auditFound {
		t.Fatalf("app.substrate_drained audit row missing: %+v", rows)
	}

	// 节流：drain 存续不得事件风暴。
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.degraded"); got != 1 {
		t.Fatalf("app.degraded events = %d after rescan, want still 1 (throttle broken)", got)
	}

	// 任务回岗（节点回岗，swarm 自行重调度）→ 派生态回 running + recovered。
	backTasks := []TaskState{
		{ID: "t-new-2", State: "running", DesiredState: "running", Image: "img"},
	}
	h.sub.setExternalTasks(h.svc("web"), backTasks)
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.recovered"); got != 1 {
		t.Fatalf("app.recovered events = %d, want exactly 1 after tasks return", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "running" {
		t.Fatalf("derived state = %q after recovery, want running", derived)
	}

	// 再次 drain → 可再报（记忆已清零，非永久静音）。
	h.sub.setExternalTasks(h.svc("web"), drainedTasks)
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.degraded"); got != 2 {
		t.Fatalf("app.degraded events = %d, want 2 (memory cleared on recovery)", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "degraded" {
		t.Fatalf("derived state = %q after second drain, want degraded", derived)
	}
}

// TestSubstrateReconDrainedGuardRails 误报防线：外部 scale=0（期望实例=0）
// 与部分在岗（running>0）都不是 drained 形态——判据是「期望>0 且全零」；
// drained 与缺失并存时缺失揭示优先（视图修正 down > degraded，不叠加）。
func TestSubstrateReconDrainedGuardRails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := deployDemoSucceeded(t, h)

	// 外部 scale=0：服务在、期望副本=0、任务全灭——不得判 drained。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) { spec.Replicas = 0 })
	h.sub.setExternalTasks(h.svc("web"), []TaskState{
		{ID: "t-old", State: "shutdown", DesiredState: "shutdown", Image: "img"},
	})
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.degraded"); got != 0 {
		t.Fatalf("external scale=0 misjudged as drained: %d events", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "running" {
		t.Fatalf("derived state = %q, want running (scale=0 is existence-judged, untouched)", derived)
	}

	// 恢复期望副本但部分在岗（running>0）→ 不披露。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) { spec.Replicas = 2 })
	h.sub.setExternalTasks(h.svc("web"), []TaskState{
		{ID: "t-run", State: "running", DesiredState: "running", Image: "img"},
		{ID: "t-pend", State: "pending", DesiredState: "running", Image: "img"},
	})
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.degraded"); got != 0 {
		t.Fatalf("partial running misjudged as drained: %d events", got)
	}

	// drained 与缺失并存：缺失路径优先（down 修正），drained 不叠加事件。
	h.sub.setExternalTasks(h.svc("web"), []TaskState{
		{ID: "t-pend", State: "pending", DesiredState: "running", Image: "img"},
	})
	if err := h.sub.ServiceRemove(ctx, h.svc("web")); err != nil {
		t.Fatalf("remove service: %v", err)
	}
	h.eng.SubstrateRecon(ctx)
	if got := countEventsByName(t, h, "app.degraded"); got != 0 {
		t.Fatalf("drained disclosure fired alongside missing: %d events", got)
	}
	if got := countEventsByName(t, h, "app.substrate_missing"); got != 1 {
		t.Fatalf("app.substrate_missing events = %d, want 1 (missing takes precedence)", got)
	}
	if derived := derivedStateOf(t, h, app.ID); derived != "down" {
		t.Fatalf("derived state = %q, want down (missing wins)", derived)
	}
}
