package engine

// 应用挂起引擎面测试（app Stop/Start，00028 位；2026-09-29 三轮）：排水
// 保持器（副本压 0 + 派生基线直投影）、drift 豁免（opt-in 也不得把副本
// 恢复回快照——挂起撤销=只经 resume）、入队门（挂起期 rollback 拒绝、
// resume 清位后可入队）。派生第一判短路（suspended 压倒观察态）钉在纯函数
// 用例。

import (
	"context"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// suspendSetup 驱动一次部署至 succeeded（replicas 1），返回夹具与应用行。
func suspendSetup(t *testing.T) (*harness, state.App) {
	t.Helper()
	h := newHarness(t)
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
`)
	if final := h.runToTerminal(h.enqueue(path)); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	return h, h.demoApp()
}

func suspendForTest(t *testing.T, h *harness, app state.App) {
	t.Helper()
	if _, err := h.store.SetAppSuspended(context.Background(), app.ID, true, state.Event{
		Name: "app.suspended", Subject: "app:" + app.ID,
	}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
}

// TestSuspendDrainsReplicasAndDerived 主链：挂起 → 一拍对账把服务副本压 0
// （服务对象保留）→ 派生基线直投影 suspended。
func TestSuspendDrainsReplicasAndDerived(t *testing.T) {
	h, app := suspendSetup(t)
	ctx := context.Background()
	svc := h.svc("web")
	suspendForTest(t, h, app)

	h.eng.SubstrateRecon(ctx)

	st, err := h.sub.ServiceInspect(ctx, svc)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Replicas != 0 {
		t.Fatalf("replicas = %d, want 0 (drained)", st.Replicas)
	}
	derived, err := h.store.GetAppDerivedState(ctx, app.ID)
	if err != nil {
		t.Fatalf("derived state: %v", err)
	}
	if derived != "suspended" {
		t.Fatalf("derived = %s, want suspended (direct projection)", derived)
	}
}

// TestSuspendKeepsZeroAgainstConvergence 保持器语义：外部把副本改回 2（模拟
// 手工拉起），挂起期下一拍对账纠回 0——位是期望，排水是保持器（DB paused
// 的「外部 scale 回 1 被哈希判据纠回」同型）。
func TestSuspendKeepsZeroAgainstConvergence(t *testing.T) {
	h, app := suspendSetup(t)
	ctx := context.Background()
	svc := h.svc("web")
	suspendForTest(t, h, app)
	h.eng.SubstrateRecon(ctx)

	// 外部拉起：以期望 spec 直改副本（模拟外部 docker service update）。
	specs := h.expectedSpecs(t, app.ID)
	for i := range specs {
		if specs[i].Name != svc {
			continue
		}
		specs[i].Replicas = 2
		if err := h.sub.ServiceUpdate(ctx, svc, specs[i]); err != nil {
			t.Fatalf("external scale up: %v", err)
		}
	}

	h.eng.SubstrateRecon(ctx)

	if st, err := h.sub.ServiceInspect(ctx, svc); err != nil || st.Replicas != 0 {
		t.Fatalf("replicas = %d (err %v), want 0 (suspension is the desired state)", st.Replicas, err)
	}
}

// TestDriftScanSkipsSuspended 豁免语义：挂起 + 收敛 opt-in 开启时，漂移扫描
// 既不报副本 diff 也不收敛（否则 opt-in 会把副本恢复回快照 N——挂起被静默
// 撤销）。外部拉起的副本原样保留。
func TestDriftScanSkipsSuspended(t *testing.T) {
	h, app := suspendSetup(t)
	ctx := context.Background()
	svc := h.svc("web")
	suspendForTest(t, h, app)
	if err := h.store.InTx(ctx, func(tx *state.Tx) error {
		return tx.SetAppDriftConverge(ctx, app.ID, true)
	}); err != nil {
		t.Fatalf("converge opt-in: %v", err)
	}
	// 外部拉起到 3（与快照 1 形成真实副本 diff——若未被豁免即是漂移）。
	specs := h.expectedSpecs(t, app.ID)
	for i := range specs {
		if specs[i].Name != svc {
			continue
		}
		specs[i].Replicas = 3
		if err := h.sub.ServiceUpdate(ctx, svc, specs[i]); err != nil {
			t.Fatalf("external scale up: %v", err)
		}
	}

	h.eng.DriftScan(ctx)

	if st, err := h.sub.ServiceInspect(ctx, svc); err != nil || st.Replicas != 3 {
		t.Fatalf("replicas = %d (err %v), want 3 (drift scan must skip suspended apps, no auto-converge)", st.Replicas, err)
	}
}

// TestEnqueueRollbackRefusesSuspended 入队门：挂起期回滚/重部署入队显式
// 拒绝（E_APP_SUSPENDED——排水会立即拉回 0 并污染部署史）；清位后同输入
// 可入队（resume 自带重部署的实现基座）。
func TestEnqueueRollbackRefusesSuspended(t *testing.T) {
	h, app := suspendSetup(t)
	suspendForTest(t, h, app)

	_, err := EnqueueRollback(context.Background(), h.store, RollbackInput{AppName: app.QualifiedName(), Actor: "test"})
	if err == nil || !strings.Contains(err.Error(), "E_APP_SUSPENDED") {
		t.Fatalf("err = %v, want E_APP_SUSPENDED", err)
	}

	if _, err := h.store.SetAppSuspended(context.Background(), app.ID, false, state.Event{
		Name: "app.resumed", Subject: "app:" + app.ID,
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	rec, err := EnqueueRollback(context.Background(), h.store, RollbackInput{AppName: app.QualifiedName(), Actor: "test"})
	if err != nil {
		t.Fatalf("enqueue after resume: %v", err)
	}
	if rec.ID == "" || rec.Kind != "rollback" {
		t.Fatalf("rec = %s/%s, want non-empty rollback deployment", rec.ID, rec.Kind)
	}
}

// TestDeriveAppStateSuspendedFirst 派生第一判：挂起位压倒一切观察态——
// 即使最近部署失败/无绑定，直投影优先（用户请求 ≠ 观察结论）。
func TestDeriveAppStateSuspendedFirst(t *testing.T) {
	facts := AppFacts{Suspended: true} // 零观察输入（本会推导 down）＋挂起位
	if got := DeriveAppState(facts); got != "suspended" {
		t.Fatalf("derive = %s, want suspended (user bit outranks observations)", got)
	}
	if got := DeriveAppState(AppFacts{}); got != "down" {
		t.Fatalf("derive without bit = %s, want down (sanity)", got)
	}
}

// expectedSpecs 从最近 succeeded 部署快照解出期望 spec（测试的外部拉起
// 载体——与引擎 lastSucceededSpecs 同源，改副本后直写底座）。
func (h *harness) expectedSpecs(t *testing.T, appID string) []ServiceSpec {
	t.Helper()
	_, specs, err := h.eng.lastSucceededSpecs(context.Background(), appID)
	if err != nil || len(specs) == 0 {
		t.Fatalf("lastSucceededSpecs: %v (n=%d)", err, len(specs))
	}
	return specs
}
