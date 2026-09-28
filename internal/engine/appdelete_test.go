package engine

// H10/MG-3（B6）：app 删除生命周期第二拍的回收 duty 回归测试。
//   - 完整收敛：deleting 应用 → 受管服务全部移除 → lifecycle=deleted +
//     app.deleted 事件与 app.deleted 审计落库；
//   - 底座瞬态（ServiceList 失败）：不落 app 终态，下拍重试成功；
//   - 在途部署让位：deleting 但有非终态部署 → 本拍跳过，部署终态后收敛；
//   - 空集零噪音：无 deleting 应用时扫描无副作用。

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// deletingHarness 在 demo 应用上构造「已成功部署 + 已置 deleting」的现场：
// 受管服务在假底座上真实存在（对账创建），返回应用行。
func deletingAppWithServices(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	if len(h.sub.services) == 0 {
		t.Fatal("fake substrate has no managed services after deploy")
	}
	if err := h.store.MarkAppDeleting(ctx, mustAppID(t, h, "demo")); err != nil {
		t.Fatalf("MarkAppDeleting: %v", err)
	}
	return h
}

// mustAppID 按名取应用 ID（断言辅助）。
func mustAppID(t *testing.T, h *harness, name string) string {
	t.Helper()
	app, err := h.store.GetAppByName(context.Background(), name)
	if err != nil {
		t.Fatalf("GetAppByName %s: %v", name, err)
	}
	return app.ID
}

// mustLifecycle 读应用当前生命周期位。
func mustLifecycle(t *testing.T, h *harness, name string) state.AppLifecycle {
	t.Helper()
	app, err := h.store.GetAppByName(context.Background(), name)
	if err != nil {
		t.Fatalf("GetAppByName %s: %v", name, err)
	}
	return app.Lifecycle
}

// TestReapDeletingAppsCompletesDeletion 主链路：deleting → 受管服务移除 →
// deleted + 终局事件与审计（此前第二拍无执行者：服务永久运行、名字不释放）。
func TestReapDeletingAppsCompletesDeletion(t *testing.T) {
	h := deletingAppWithServices(t)
	ctx := context.Background()
	h.eng.ReapDeletingApps(ctx)

	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle = %s, want deleted", got)
	}
	if len(h.sub.services) != 0 {
		t.Fatalf("managed services remain: %v", serviceNames(h.sub))
	}
	if len(h.sub.removed) == 0 {
		t.Fatal("no ServiceRemove recorded")
	}
	if !hasEvent(h.events(), "app.deleted") {
		t.Fatal("app.deleted event missing")
	}
	found := false
	audits, err := h.store.RecentAudits(ctx, 50)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	for _, a := range audits {
		if a.Action == "app.deleted" {
			found = true
			if a.Result != "ok" {
				t.Fatalf("app.deleted audit result = %s, want ok", a.Result)
			}
		}
	}
	if !found {
		t.Fatal("app.deleted audit missing")
	}
	// 幂等：对已 deleted 的应用重复扫描不产生第二条终局事件。
	before := len(h.events())
	h.eng.ReapDeletingApps(ctx)
	if after := len(h.events()); after != before {
		t.Fatalf("repeat scan emitted %d new events (must be idempotent)", after-before)
	}
}

// TestReapDeletingAppsRetriesAfterTransientServiceList 底座瞬态：ServiceList
// 失败时本拍消化（不落 deleted、不删服务），故障清除后下一拍收敛成功。
func TestReapDeletingAppsRetriesAfterTransientServiceList(t *testing.T) {
	h := deletingAppWithServices(t)
	ctx := context.Background()
	transient := errors.New("dockerd temporarily unreachable (injected)")

	h.sub.failServiceListErr = transient
	h.eng.ReapDeletingApps(ctx)
	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleting {
		t.Fatalf("lifecycle after transient = %s, want still deleting", got)
	}
	if len(h.sub.removed) != 0 {
		t.Fatal("services must not be removed while scan fails")
	}
	if hasEvent(h.events(), "app.deleted") {
		t.Fatal("app.deleted must not be emitted on transient failure")
	}

	h.sub.failServiceListErr = nil
	h.eng.ReapDeletingApps(ctx)
	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle after retry = %s, want deleted", got)
	}
	if len(h.sub.services) != 0 {
		t.Fatalf("managed services remain after retry: %v", serviceNames(h.sub))
	}
}

// TestReapDeletingAppsWaitsForInFlightDeployment 在途部署让位：发布对账会
// 重建被删服务，duty 必须等部署终态后再收敛（本拍跳过不误删）。
func TestReapDeletingAppsWaitsForInFlightDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 先成功部署一版（服务存在），再入队第二条并置 deleting——第二条在途。
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	rec := h.enqueue(h.writeCompose(composeV1))
	if err := h.store.MarkAppDeleting(ctx, mustAppID(t, h, "demo")); err != nil {
		t.Fatalf("MarkAppDeleting: %v", err)
	}
	h.eng.ReapDeletingApps(ctx)
	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleting {
		t.Fatalf("lifecycle with in-flight deployment = %s, want still deleting (must wait)", got)
	}
	if len(h.sub.removed) != 0 {
		t.Fatal("services removed while deployment in flight (delete vs reconcile race)")
	}
	// 部署终态后（成功或失败均可）：下一拍收敛。
	h.runToTerminal(rec)
	h.eng.ReapDeletingApps(ctx)
	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle after deployment terminal = %s, want deleted", got)
	}
}

// TestReapDeletingAppsThrottledByTimeGate 频控闸：tick 驱动形态受
// deleteScanGate 约束——闸内重拍不触达底座（ServiceList 计数不变），
// 时钟推进过闸后恢复扫描。闸显式清零启动（runToTerminal 的 tick 已可能
// 推过闸，清零使首拍确定性直通）。
func TestReapDeletingAppsThrottledByTimeGate(t *testing.T) {
	h := deletingAppWithServices(t)
	ctx := context.Background()
	h.eng.deleteScanGate = scanGate{}

	// 第一拍（force=false，闸清零直通）：执行扫描（ServiceList ×1）。
	h.eng.reapDeletingApps(ctx, false)
	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("first scan lifecycle = %s, want deleted", got)
	}
	h.sub.mu.Lock()
	callsAfterFirst := h.sub.serviceListCalls
	h.sub.mu.Unlock()
	if callsAfterFirst == 0 {
		t.Fatal("first scan did not reach substrate")
	}

	// 闸内重拍：不应触达底座。
	h.eng.reapDeletingApps(ctx, false)
	h.sub.mu.Lock()
	callsGated := h.sub.serviceListCalls
	h.sub.mu.Unlock()
	if callsGated != callsAfterFirst {
		t.Fatalf("gated scan reached substrate: calls %d → %d (time gate not applied)", callsAfterFirst, callsGated)
	}

	// 时钟过闸后恢复扫描：新置 deleting 的应用（无受管服务——直接落第二拍）
	// 在过闸后的非 force 拍里被收敛（证明扫描确实恢复执行）。
	h.clk.Advance(appDeleteScanInterval + time.Second)
	if _, err := testsupport.SeedAppE(t, h.store, "other"); err != nil {
		t.Fatalf("create other app: %v", err)
	}
	if err := h.store.MarkAppDeleting(ctx, mustAppID(t, h, "other")); err != nil {
		t.Fatalf("mark other deleting: %v", err)
	}
	h.eng.reapDeletingApps(ctx, false)
	if got := mustLifecycle(t, h, "other"); got != state.LifecycleDeleted {
		t.Fatalf("post-gate scan did not converge new deleting app: lifecycle = %s, want deleted", got)
	}
}

// serviceNames 列出假底座当前服务名（断言辅助）。
func serviceNames(f *fakeSubstrate) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.services))
	for name := range f.services {
		out = append(out, name)
	}
	return out
}

// fakeSecretReaper 是 SecretReaper 端口的内存假实现（app 删除 secret 扫尾
// 测试注入；按 label 等值过滤，记录移除序）。
type fakeSecretReaper struct {
	mu      sync.Mutex
	secrets map[string]map[string]string // name → labels
	removed []string
}

func newFakeSecretReaper() *fakeSecretReaper {
	return &fakeSecretReaper{secrets: map[string]map[string]string{}}
}

func (f *fakeSecretReaper) SecretList(_ context.Context, labels map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name, lbs := range f.secrets {
		match := true
		for k, v := range labels {
			if lbs[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeSecretReaper) SecretRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeSecretReaper) add(name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[name] = labels
}

// TestReapDeletingAppsSweepsAppSecrets 删除扫尾：服务全部移除后，按归属
// label（fleetly.managed+fleetly.app）登记的 Swarm secret 一并清场；他 app
// 的 secret 与库凭据 secret（fleetly.db 归属，非本 app label）不受牵连。
func TestReapDeletingAppsSweepsAppSecrets(t *testing.T) {
	h := deletingAppWithServices(t)
	ctx := context.Background()
	reaper := newFakeSecretReaper()
	reaper.add(h.demoSecretPrefix("apikey")+"1a2b3c4d", h.demoSecretLabels())
	reaper.add(h.demoSecretPrefix("tls")+"9f8e7d6c", h.demoSecretLabels())
	reaper.add("fleetly-other-key-11223344", map[string]string{
		"fleetly.managed": "true",
		"fleetly.app":     "other/app",
	})
	reaper.add("fleetly-db-pgprod-password-55667788", map[string]string{
		"fleetly.managed": "true",
		"fleetly.db":      "pgprod",
	})
	h.eng.WithSecretReaper(reaper)

	h.eng.ReapDeletingApps(ctx)

	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle = %s, want deleted", got)
	}
	reaper.mu.Lock()
	removed := append([]string(nil), reaper.removed...)
	left := len(reaper.secrets)
	reaper.mu.Unlock()
	if len(removed) != 2 {
		t.Fatalf("removed secrets = %v, want exactly the two demo-app secrets", removed)
	}
	for _, name := range removed {
		if !strings.Contains(name, "demo") {
			t.Fatalf("foreign secret %s removed", name)
		}
	}
	if left != 2 {
		t.Fatalf("%d secrets remain, want the other-app + database secrets untouched", left)
	}
}

// TestReapDeletingAppsSecretSweepNotWired 端口未接线：删除照常收敛
//（best-effort 纪律——扫尾缺席不阻塞 tombstone 第二拍）。
func TestReapDeletingAppsSecretSweepNotWired(t *testing.T) {
	h := deletingAppWithServices(t)
	ctx := context.Background()

	h.eng.ReapDeletingApps(ctx)

	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle = %s, want deleted (sweep absence must not block deletion)", got)
	}
}
