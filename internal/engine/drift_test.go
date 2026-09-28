package engine

// 漂移检测测试（T2.13）：投影哈希稳定性与敏感性（env 只进 key:hash）、
// 检测 → reconcile.drift_detected 事件（mock 注入外部改动；不重复报）、
// 收敛 opt-in 默认关 / 开启即收敛、报告与事件不泄露 env 明文、人工一次性
// 收敛与在途部署拒绝。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// driftProbeValue 是注入外部改动的"密钥形态"明文（负面断言：任何事件/
// 审计载荷都不得出现）。
const driftProbeValue = "super-secret-plaintext-9f2c"

func TestDriftHashStabilityAndSensitivity(t *testing.T) {
	// label 夹具经 naming 写入器造数（生产 planner 同源形态：三段限定形
	// fleetly.app + team/project 两键；deployment/desired-hash 是簿记键，
	// 测试自拟占位值）。IMPL-ARCH-A：不再手写前 W2-S3 的裸名 label 夹具。
	labelsFor := func(deployment, desiredHash string) map[string]string {
		t.Helper()
		labels, err := naming.ServiceLabels("acme", "prod", "demo", "web", deployment)
		if err != nil {
			t.Fatalf("service labels: %v", err)
		}
		labels[state.LabelDesiredHash] = desiredHash
		return labels
	}
	containerLabels, err := naming.ContainerLabels("acme", "prod", "demo")
	if err != nil {
		t.Fatalf("container labels: %v", err)
	}
	base := ServiceSpec{
		Name:            "fleetly-acme-prod-demo-web",
		Image:           "alpine:3@sha256:aaa",
		Command:         []string{"sleep", "infinity"},
		Env:             []string{"A=1", "B=2"},
		Replicas:        2,
		Networks:        []NetworkAttach{{Name: "fleetly-acme-prod-demo-net", Aliases: []string{"web"}}},
		ServiceLabels:   labelsFor("d1", "h1"),
		ContainerLabels: containerLabels,
		Healthcheck:     &HealthcheckSpec{Test: []string{"CMD", "true"}, Interval: time.Second},
	}
	baseHash := driftHash(base)

	// 稳定性：env 顺序、label 迭代序、平台簿记 label、swarm 的 repo 前缀
	// 归一——都不影响哈希。
	reordered := base
	reordered.Env = []string{"B=2", "A=1"}
	if driftHash(reordered) != baseHash {
		t.Fatal("env order changed the drift hash")
	}
	bookkeeping := base
	bookkeeping.ServiceLabels = labelsFor("OTHER", "OTHER")
	if driftHash(bookkeeping) != baseHash {
		t.Fatal("bookkeeping labels changed the drift hash")
	}
	renamed := base
	renamed.Image = "docker.io/library/alpine@sha256:aaa"
	if driftHash(renamed) != baseHash {
		t.Fatal("repo string changed the drift hash despite equal digest")
	}

	// 敏感性：env 值变化 / 键集变化 / 副本变化 / 别名变化都改变哈希。
	envChanged := base
	envChanged.Env = []string{"A=1", "B=3"}
	if driftHash(envChanged) == baseHash {
		t.Fatal("env value change not detected")
	}
	keyAdded := base
	keyAdded.Env = []string{"A=1", "B=2", "C=3"}
	if driftHash(keyAdded) == baseHash {
		t.Fatal("env key addition not detected")
	}
	replicasChanged := base
	replicasChanged.Replicas = 3
	if driftHash(replicasChanged) == baseHash {
		t.Fatal("replica change not detected")
	}
	aliasChanged := base
	aliasChanged.Networks = []NetworkAttach{{Name: "x", Aliases: []string{"web2"}}}
	if driftHash(aliasChanged) == baseHash {
		t.Fatal("network alias change not detected")
	}
}

func TestDriftDetectionEmitsEventOnceAndDoesNotConvergeByDefault(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}

	// 基线：无漂移、无事件。
	h.eng.DriftScan(ctx)
	if hasEvent(h.events(), "reconcile.drift_detected") {
		t.Fatal("drift event on clean baseline")
	}

	// 注入外部改动（模拟手动 docker service update --env）。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) {
		spec.Env = append(spec.Env, "EVIL="+driftProbeValue)
	})

	// 检测：事件 + 报告漂移（env 只报键名 + hash）。
	h.eng.DriftScan(ctx)
	if !hasEvent(h.events(), "reconcile.drift_detected") {
		t.Fatal("drift not detected after external mutation")
	}
	report, err := h.eng.DriftShow(ctx, "demo")
	if err != nil || !report.Drifted {
		t.Fatalf("drift show = %+v (%v), want drifted", report, err)
	}
	if len(report.Services) != 1 || len(report.Services[0].Diff) == 0 {
		t.Fatalf("report diff empty: %+v", report.Services)
	}
	foundEnvDiff := false
	for _, d := range report.Services[0].Diff {
		if d.Field == "env.EVIL" {
			foundEnvDiff = true
			if strings.Contains(d.Expected, driftProbeValue) || strings.Contains(d.Actual, driftProbeValue) {
				t.Fatalf("env diff leaks plaintext: %+v", d)
			}
			if !strings.HasPrefix(d.Expected, "<absent>") && len(d.Expected) != 64 {
				t.Fatalf("env diff hash form unexpected: %q", d.Expected)
			}
		}
	}
	if !foundEnvDiff {
		t.Fatalf("no env.EVIL diff: %+v", report.Services[0].Diff)
	}
	// 事件载荷同样不泄露明文。
	for _, ev := range mustEvents(t, h) {
		if strings.Contains(ev.Payload, driftProbeValue) {
			t.Fatalf("event %s leaks env plaintext", ev.Name)
		}
	}

	// 同一漂移存续：不重复发事件（迁移判定）。
	h.eng.DriftScan(ctx)
	if n := countEvents(t, h, "reconcile.drift_detected"); n != 1 {
		t.Fatalf("drift event count = %d, want 1 (only the transition is reported)", n)
	}

	// opt-in 默认关：漂移不被自动收敛（外部改动保持原样）。
	app, err := h.store.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if on, _ := h.store.GetAppDriftConverge(ctx, app.ID); on {
		t.Fatal("drift converge default is on")
	}
	joined := strings.Join(h.sub.services[h.svc("web")].spec.Env, ",")
	if !strings.Contains(joined, driftProbeValue) {
		t.Fatalf("drift was converged while opt-in is off: %v", joined)
	}
}

func TestDriftConvergeOptInAndManual(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}

	// 开启 opt-in（人工置位面）。
	if err := h.eng.SetDriftConverge(ctx, "demo", true, "human"); err != nil {
		t.Fatalf("enable converge: %v", err)
	}
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) {
		spec.Env = append(spec.Env, "EVIL="+driftProbeValue)
	})
	h.eng.DriftScan(ctx)

	// 开启时漂移 → 按当前期望态收敛（归位重放原语：同内容重放零任务
	// 替换——外部 env 改动被写回）。
	got := h.sub.services[h.svc("web")].spec.Env
	for _, kv := range got {
		if strings.HasPrefix(kv, "EVIL=") {
			t.Fatalf("opt-in converge did not remove drift: %v", got)
		}
	}
	// 收敛审计。
	found := false
	audits, _ := h.store.RecentAudits(ctx, 50)
	for _, a := range audits {
		if a.Action == "reconcile.converge" && a.Actor == "system" {
			found = true
		}
	}
	if !found {
		t.Fatal("no reconcile.converge audit after auto-converge")
	}
	// 下一拍：漂移消除、无新事件。
	h.eng.DriftScan(ctx)
	if n := countEvents(t, h, "reconcile.drift_detected"); n != 1 {
		t.Fatalf("drift event count after converge = %d, want 1", n)
	}

	// 人工一次性收敛（actor=human；不经 opt-in 位）。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) {
		spec.Env = append(spec.Env, "MANUAL=1")
	})
	source, err := h.eng.ConvergeApp(ctx, "demo", "human")
	if err != nil {
		t.Fatalf("manual converge: %v", err)
	}
	if source.ID == "" {
		t.Fatal("manual converge returned no source deployment")
	}
	audits, _ = h.store.RecentAudits(ctx, 50)
	humanConverge := false
	for _, a := range audits {
		if a.Action == "reconcile.converge" && a.Actor == "human" {
			humanConverge = true
		}
	}
	if !humanConverge {
		t.Fatal("no human-actor converge audit")
	}

	// 在途部署存在时拒绝收敛（409 语义）。
	rec := h.enqueue(h.writeCompose(composeV1))
	if _, err := h.eng.ConvergeApp(ctx, "demo", "human"); err == nil {
		t.Fatal("converge accepted with in-flight deployment")
	} else if got := appErrCodeOf(t, err); got != "E_STATE_VERSION_CONFLICT" {
		t.Fatalf("in-flight converge code = %s, want E_STATE_VERSION_CONFLICT", got)
	}
	h.runToTerminal(rec)
}

func mustEvents(t *testing.T, h *harness) []state.Event {
	t.Helper()
	rows, err := h.store.EventsSince(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return rows
}

// TestDriftDetectsUpdateConfigTamper A8（S18）：update config 投影补齐——
// 外部 docker service update --update-order 篡改 → 漂移项出现
// （update_order 字段级 diff）；受管字段 failure_action 改 rollback →
// 专报项（update_failure_action，平台恒写 pause、实况非 pause 即篡改）。
func TestDriftDetectsUpdateConfigTamper(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}

	// 基线：无漂移（投影含三字段后两侧同构）。
	report, err := h.eng.DriftShow(ctx, "demo")
	if err != nil || report.Drifted {
		t.Fatalf("clean baseline drifted: %+v (%v)", report, err)
	}

	// 外部改 update-order（start-first → stop-first）→ 漂移项出现。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) {
		spec.UpdateOrder = "stop-first"
	})
	report, err = h.eng.DriftShow(ctx, "demo")
	if err != nil || !report.Drifted {
		t.Fatalf("update-order tamper not detected: %+v (%v)", report, err)
	}
	foundOrder := false
	for _, d := range report.Services[0].Diff {
		if d.Field == "update_order" {
			foundOrder = true
			if d.Expected != "start-first" || d.Actual != "stop-first" {
				t.Fatalf("update_order diff = %+v", d)
			}
		}
	}
	if !foundOrder {
		t.Fatalf("no update_order diff item: %+v", report.Services[0].Diff)
	}
	// 事件面披露（drift_detected 载荷含字段级 diff）。
	h.eng.DriftScan(ctx)
	if !hasEvent(h.events(), "reconcile.drift_detected") {
		t.Fatal("drift event missing for update-order tamper")
	}

	// 归位 order，改受管字段 failure_action → 专报项（不进期望态哈希的
	// 受管字段走独立比对）。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) {
		spec.UpdateOrder = "start-first"
	})
	h.sub.mutateUpdateFailureAction(h.svc("web"), "rollback")
	report, err = h.eng.DriftShow(ctx, "demo")
	if err != nil || !report.Drifted {
		t.Fatalf("failure_action tamper not detected: %+v (%v)", report, err)
	}
	foundFA := false
	for _, d := range report.Services[0].Diff {
		if d.Field == "update_failure_action" {
			foundFA = true
			if d.Expected != "pause" || d.Actual != "rollback" {
				t.Fatalf("update_failure_action diff = %+v", d)
			}
		}
	}
	if !foundFA {
		t.Fatalf("no update_failure_action diff item: %+v", report.Services[0].Diff)
	}
}

func countEvents(t *testing.T, h *harness, name string) int {
	t.Helper()
	n := 0
	for _, ev := range mustEvents(t, h) {
		if ev.Name == name {
			n++
		}
	}
	return n
}

// TestDriftGlobalServiceNoFalsePositive M1-3 回归（B4）：global 服务（存量
// 快照形态——v0.1 已在校验层拒绝新部署声明 mode: global，本测试防存量
// 数据/回滚路径）成功在位后 driftScan 零漂移——期望侧副本（规划层对
// global 写缺省 1）与实况侧（Swarm global 服务 Mode.Replicated 为 nil、
// 副本读回 0）在漂移投影层归一同值（副本数对 global 非受管字段）。
func TestDriftGlobalServiceNoFalsePositive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app, err := ensureAppForTest(t, ctx, h.store, "demo")
	if err != nil {
		t.Fatalf("ensure app: %v", err)
	}

	// 期望态来源：succeeded 部署行 + 密文快照（绕过引擎主链——受控子集已
	// 拒 global，只有存量行能到达该形态）。label 夹具经 naming 写入器造数
	//（三段限定形 fleetly.app——IMPL-ARCH-A：不再手写前 W2-S3 裸名形态）。
	rec, err := h.store.CreateDeployment(ctx, state.DeployRecord{
		AppID: app.ID, AppName: "demo", Kind: "deploy",
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	serviceLabels, lerr := naming.ServiceLabels(app.TeamSlug, app.ProjectSlug, app.Name, "agent", rec.ID)
	if lerr != nil {
		t.Fatalf("service labels: %v", lerr)
	}
	containerLabels, clerr := naming.ContainerLabels(app.TeamSlug, app.ProjectSlug, app.Name)
	if clerr != nil {
		t.Fatalf("container labels: %v", clerr)
	}
	// 期望侧：规划层对 global 服务的产出形态（副本缺省 1 + stop-first）。
	spec := ServiceSpec{
		Name:              "fleetly-demo-agent",
		Image:             "alpine:3",
		Global:            true,
		Replicas:          1,
		ServiceLabels:     serviceLabels,
		ContainerLabels:   containerLabels,
		Networks:          []NetworkAttach{{Name: h.demoNet(), Aliases: []string{"agent"}}},
		UpdateOrder:       "stop-first",
		UpdateParallelism: 1,
	}
	// 底座实况：global 服务（实况投影 Replicas=0——与真实适配器
	// serviceToState 的 Mode.Global → 0 同构，见 fakes_test.globalReplicasOf）。
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
	if err := h.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE deployments SET status = 'succeeded', desired_hash = ?, desired_spec = ? WHERE id = ?`,
			spec.DesiredHash(), string(ct), rec.ID)
		return err
	}); err != nil {
		t.Fatalf("seed succeeded row: %v", err)
	}

	// 投影归一断言：两侧哈希一致（旧缺陷：期望 1 vs 实况 0 永久假阳性）。
	h.eng.DriftScan(ctx)
	if hasEvent(h.events(), "reconcile.drift_detected") {
		t.Fatal("global service replica-semantics difference was misreported as drift (M1-3 false positive)")
	}
	report, err := h.eng.DriftShow(ctx, "demo")
	if err != nil || report.Drifted {
		t.Fatalf("drift show = %+v (%v), want no drift (global replica semantics normalized)", report, err)
	}
}
