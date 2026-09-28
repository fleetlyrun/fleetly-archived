package engine

// 服务收敛原语的守卫与直接测试（IMPL-ARCH-B）：「缺失→建；期望哈希不符→
// 全量重申」在引擎里曾有两份手写拷贝（applyDesired 部署线 + 任务线
// convergeQueuedTask），且任务线从不打哈希标 ⇒ 哈希比较恒假 ⇒ 每拍无差别
// 重申。本文件钉死收敛幂等语义（重复收敛零 ServiceUpdate）与原语各分支。

import (
	"context"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// TestTaskConvergenceRepeatBeatSkipsRewrite 是收敛幂等守卫（B1）：对已存在
// 且形态未变的任务服务重复收敛，第二次收敛零 ServiceUpdate。场景 =「
// MarkTaskRunning 失败后的重试拍」——行快照仍是 queued 的下一拍会再次执行
// convergeQueuedTask（以同一行快照直接重入建模：收敛判定只依赖底座实况与
// 期望 spec，行状态只影响末尾的 MarkTaskRunning 落点，与重申判定无关）。
func TestTaskConvergenceRepeatBeatSkipsRewrite(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.nets.injectNetwork("fleetly-taskgroup-tenant1", map[string]string{state.LabelManaged: "true"})
	task := taskFixture(t, h, TaskScopeTaskGroup, "tenant1", "fleetly-taskgroup-tenant1", 10*time.Minute)

	// 首拍：服务缺失 → ServiceCreate。
	h.eng.convergeQueuedTask(ctx, task)
	if _, ok := h.sub.services[task.Service]; !ok {
		t.Fatalf("task service %s was not created on the first beat", task.Service)
	}
	if got := h.sub.serviceUpdateCalls(); got != 0 {
		t.Fatalf("first convergence issued %d ServiceUpdate calls, want 0 (a missing service converges via ServiceCreate)", got)
	}

	// 重试拍：服务已在且形态未变 → 零 ServiceUpdate（哈希命中即收敛；
	// 修复前任务 spec 从不打哈希标 ⇒ cur.DesiredHash 恒空 ⇒ 此拍必重申）。
	h.eng.convergeQueuedTask(ctx, task)
	if got := h.sub.serviceUpdateCalls(); got != 0 {
		t.Fatalf("repeat convergence issued %d ServiceUpdate calls, want 0 (idempotent convergence must not rewrite an unchanged service)", got)
	}
}

// convergeSpecFixture 是原语直接测试的期望 spec（两次收敛间以 Env 变体制造
// 哈希差异）。
func convergeSpecFixture() ServiceSpec {
	return ServiceSpec{
		Name:     "fleetly-demo-web",
		Image:    "registry.fleetly.internal/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Replicas: 2,
		Env:      []string{"A=1"},
	}
}

// TestConvergeServiceCreatesMissingAndStampsHash 原语直接测试①：缺失 → 建，
// 且哈希标戳在原语内落齐（调用方零介入）。
func TestConvergeServiceCreatesMissingAndStampsHash(t *testing.T) {
	h := newHarness(t)
	spec := convergeSpecFixture()

	if err := h.eng.convergeService(context.Background(), spec, convergeOptions{}); err != nil {
		t.Fatalf("converge missing service: %v", err)
	}
	svc, ok := h.sub.services[spec.Name]
	if !ok {
		t.Fatalf("service %s was not created", spec.Name)
	}
	stamped := svc.spec.ServiceLabels[state.LabelDesiredHash]
	if stamped == "" || stamped != svc.spec.DesiredHash() {
		t.Fatalf("desired-hash label = %q, want the spec hash %q (the primitive must stamp it)", stamped, svc.spec.DesiredHash())
	}
}

// TestConvergeServiceIdempotentWhenHashMatches 原语直接测试②：等哈希既有 →
// 零 update（幂等；底座对象版本不动）。
func TestConvergeServiceIdempotentWhenHashMatches(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	spec := convergeSpecFixture()
	if err := h.eng.convergeService(ctx, spec, convergeOptions{}); err != nil {
		t.Fatalf("initial converge: %v", err)
	}
	version := h.sub.services[spec.Name].version

	if err := h.eng.convergeService(ctx, spec, convergeOptions{}); err != nil {
		t.Fatalf("repeat converge: %v", err)
	}
	if got := h.sub.serviceUpdateCalls(); got != 0 {
		t.Fatalf("hash-matched convergence issued %d ServiceUpdate calls, want 0", got)
	}
	if got := h.sub.services[spec.Name].version; got != version {
		t.Fatalf("service version = %d, want %d (idempotent convergence must not touch the object)", got, version)
	}
}

// TestConvergeServiceReassertsOnHashMismatch 原语直接测试③：哈希不符（外部
// 篡改/上一代残留）→ 恰一次全量重申，标戳随新形态刷新；再次收敛零写。
func TestConvergeServiceReassertsOnHashMismatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	spec := convergeSpecFixture()
	if err := h.eng.convergeService(ctx, spec, convergeOptions{}); err != nil {
		t.Fatalf("initial converge: %v", err)
	}

	tampered := spec
	tampered.Env = []string{"A=2"}
	if err := h.eng.convergeService(ctx, tampered, convergeOptions{}); err != nil {
		t.Fatalf("converge after tamper: %v", err)
	}
	if got := h.sub.serviceUpdateCalls(); got != 1 {
		t.Fatalf("hash-mismatch convergence issued %d ServiceUpdate calls, want 1", got)
	}
	svc := h.sub.services[spec.Name]
	if stamped := svc.spec.ServiceLabels[state.LabelDesiredHash]; stamped != svc.spec.DesiredHash() {
		t.Fatalf("desired-hash label = %q after reassert, want the reasserted hash %q", stamped, svc.spec.DesiredHash())
	}

	if err := h.eng.convergeService(ctx, tampered, convergeOptions{}); err != nil {
		t.Fatalf("post-reassert converge: %v", err)
	}
	if got := h.sub.serviceUpdateCalls(); got != 1 {
		t.Fatalf("post-reassert convergence issued %d ServiceUpdate calls, want 0 new (total 1)", got)
	}
}

// TestConvergeServiceForceReasserts 原语直接测试④：force → 哈希命中也恒重
// 申（重放路径以 swarm 侧 spec 重申为准——label 存根不证明实况未被篡改）。
func TestConvergeServiceForceReasserts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	spec := convergeSpecFixture()
	if err := h.eng.convergeService(ctx, spec, convergeOptions{}); err != nil {
		t.Fatalf("initial converge: %v", err)
	}

	if err := h.eng.convergeService(ctx, spec, convergeOptions{force: true}); err != nil {
		t.Fatalf("forced converge: %v", err)
	}
	if got := h.sub.serviceUpdateCalls(); got != 1 {
		t.Fatalf("forced convergence issued %d ServiceUpdate calls, want 1 (force must bypass the hash shortcut)", got)
	}
}

// TestConvergeServiceDeploymentChangeReasserts 部署线特有条件保全：归属换代
// （LabelDeployment 与本次发布不符）即重申，哈希命中也不例外；换代完成后
// 回归零写。走 convergeServiceObserved（applyDesired 的批量实况入口）。
func TestConvergeServiceDeploymentChangeReasserts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	spec := convergeSpecFixture()
	if err := h.eng.convergeServiceObserved(ctx, spec, ServiceState{}, false, convergeOptions{deploymentID: "dep-1"}); err != nil {
		t.Fatalf("initial converge: %v", err)
	}
	svc := h.sub.services[spec.Name]
	if svc.spec.ServiceLabels[state.LabelDeployment] != "dep-1" {
		t.Fatalf("deployment label = %q, want dep-1", svc.spec.ServiceLabels[state.LabelDeployment])
	}

	// 换代（新发布 ID）：哈希命中但归属不符 → 重申 + 归属标重写。
	if err := h.eng.convergeServiceObserved(ctx, spec, h.sub.stateOf(svc), true, convergeOptions{deploymentID: "dep-2"}); err != nil {
		t.Fatalf("converge after deployment change: %v", err)
	}
	if got := h.sub.serviceUpdateCalls(); got != 1 {
		t.Fatalf("deployment-change convergence issued %d ServiceUpdate calls, want 1 (ownership change must reassert)", got)
	}
	if got := h.sub.services[spec.Name].spec.ServiceLabels[state.LabelDeployment]; got != "dep-2" {
		t.Fatalf("deployment label = %q, want dep-2 after reassert", got)
	}

	// 同代重放（force=false）：零写。
	if err := h.eng.convergeServiceObserved(ctx, spec, h.sub.stateOf(h.sub.services[spec.Name]), true, convergeOptions{deploymentID: "dep-2"}); err != nil {
		t.Fatalf("same-generation converge: %v", err)
	}
	if got := h.sub.serviceUpdateCalls(); got != 1 {
		t.Fatalf("same-generation convergence issued %d ServiceUpdate calls, want 0 new (total 1)", got)
	}
}
