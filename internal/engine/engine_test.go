package engine

// 发布引擎主链路测试：状态机全链（真实 compose fixture + 假底座/假放置/
// 假时钟）——互斥、env 三层合并与 pending promote、失败分流两分支、首发
// scale=0、stop-first 停机账、观察窗判定、cancel 语义、重启恢复、L4 只
// 告警一次、场景矩阵错误码。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"strings"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// harness 是一只测试环境（store + box + 假底座/解析器/时钟/网络面）。
type harness struct {
	t        *testing.T
	store    *state.Store
	box      *secrets.Box
	sub      *fakeSubstrate
	nets     *fakeNetworkSubstrate
	resolver *fakeResolver
	images   *fakeImages
	clk      *fakeClock
	eng      *Engine
}

var testStart = time.Now().UTC().Truncate(time.Second)

// svc 推导 harness 播种 demo 应用的指定 compose 服务名（v0.3 三段公式——
// team/prj slug 取自 app 行，机械改写自旧字面量 fleetly-demo-<service>）。
// app 行尚未播种时先播种（与 enqueue 的 ensure 语义一致——服务名推导可能
// 早于入队发生）。
func (h *harness) svc(service string) string {
	h.t.Helper()
	app := h.demoApp()
	name, nerr := naming.ServiceName(app.TeamSlug, app.ProjectSlug, app.Name, service)
	if nerr != nil {
		h.t.Fatalf("service name: %v", nerr)
	}
	return name
}

// demoNet 推导 demo 应用的 per-app overlay 网络名（三段公式）。
func (h *harness) demoNet() string {
	h.t.Helper()
	app := h.demoApp()
	name, nerr := naming.NetworkName(app.TeamSlug, app.ProjectSlug, app.Name)
	if nerr != nil {
		h.t.Fatalf("network name: %v", nerr)
	}
	return name
}

// demoSecretPrefix 推导 demo 应用的 Swarm secret 名前缀（name + '-'）。
func (h *harness) demoSecretPrefix(name string) string {
	h.t.Helper()
	app := h.demoApp()
	out, nerr := naming.SecretName(app.TeamSlug, app.ProjectSlug, app.Name, name, strings.Repeat("0", 8))
	if nerr != nil {
		h.t.Fatalf("secret name: %v", nerr)
	}
	return strings.TrimSuffix(out, strings.Repeat("0", 8))
}

// demoSecretLabels 返回 demo 应用 secret 的归属 label 集（值 = 三段限定形）。
func (h *harness) demoSecretLabels() map[string]string {
	h.t.Helper()
	return secretLabels(h.demoApp().QualifiedName())
}

// demoApp 取（必要时播种）harness 的 demo 应用行。
func (h *harness) demoApp() state.App {
	h.t.Helper()
	app, err := h.store.GetAppByName(context.Background(), "demo")
	if err == nil {
		return app
	}
	if err != state.ErrAppNotFound {
		h.t.Fatalf("resolve demo app: %v", err)
	}
	seeded, err := testsupport.SeedAppE(h.t, h.store, "demo")
	if err != nil {
		h.t.Fatalf("seed demo app: %v", err)
	}
	return seeded
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "fleetly.key"))
	if err != nil {
		t.Fatalf("ensure key: %v", err)
	}
	sub := newFakeSubstrate()
	netSub := newFakeNetworkSubstrate()
	res := &fakeResolver{store: st}
	images := &fakeImages{missing: map[string]bool{}}
	clk := newFakeClock(testStart)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := NewEngine(Config{}, st, sub, images, res, box, logger).
		WithClock(clk).
		WithNetworkSubstrate(netSub).
		WithPlatformNetworks("fleetly-system", state.RustfsNetworkName)
	return &harness{t: t, store: st, box: box, sub: sub, nets: netSub, resolver: res, images: images, clk: clk, eng: eng}
}

// writeCompose 落一份 compose fixture 并返回绝对路径。
func (h *harness) writeCompose(content string) string {
	h.t.Helper()
	path := filepath.Join(h.t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatalf("write compose: %v", err)
	}
	return path
}

// enqueue 以 CLI 同构语义入队（deployment.queued 事件 + deployment.create
// 审计与建行同事务）。
func (h *harness) enqueue(composePath string) state.DeployRecord {
	h.t.Helper()
	ctx := context.Background()
	app, err := ensureAppForTest(h.t, ctx, h.store, "demo")
	if err != nil {
		h.t.Fatalf("ensure app: %v", err)
	}
	rec, err := h.store.CreateDeployment(ctx, state.DeployRecord{
		AppID:       app.ID,
		AppName:     app.Name,
		Kind:        "deploy",
		SpecHash:    specHashOf(composePath),
		ComposePath: composePath,
	})
	if err != nil {
		h.t.Fatalf("create deployment: %v", err)
	}
	if err := h.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.AppendEvent(ctx, state.Event{Name: "deployment.queued", Subject: "deployment:" + rec.ID})
		return err
	}); err != nil {
		h.t.Fatalf("enqueue event: %v", err)
	}
	return rec
}

// runToTerminal 驱动 tick 至终态（上限 64 拍防死循环）。每拍推进假时钟
// 2s（真实时钟自行流动；2s×64=128s 覆盖观察窗且不触 300s 看门狗）。
func (h *harness) runToTerminal(rec state.DeployRecord) state.DeployRecord {
	h.t.Helper()
	ctx := context.Background()
	for i := 0; i < 64; i++ {
		row, err := h.store.GetDeployment(ctx, rec.ID)
		if err != nil {
			h.t.Fatalf("get deployment: %v", err)
		}
		if row.Status.Terminal() {
			return row
		}
		h.clk.Advance(2 * time.Second)
		h.eng.Tick(ctx)
	}
	h.t.Fatalf("deployment %s did not reach terminal state within 64 ticks", rec.ID)
	return state.DeployRecord{}
}

// events 返回至今的全部事件名序列。
func (h *harness) events() []string {
	h.t.Helper()
	rows, err := h.store.EventsSince(context.Background(), 0, 1000)
	if err != nil {
		h.t.Fatalf("events: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, e := range rows {
		out = append(out, e.Name)
	}
	return out
}

func hasEvent(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// ensureAppForTest / specHashOf 是测试侧的 CLI 同构助手（compose.Load 校验
// 后取 spec_hash；应用不存在则创建）。
func ensureAppForTest(t *testing.T, ctx context.Context, st *state.Store, name string) (state.App, error) {
	if app, err := st.GetAppByName(ctx, name); err == nil {
		return app, nil
	} else if err != state.ErrAppNotFound {
		return state.App{}, err
	}
	return testsupport.SeedAppE(t, st, name)
}

func specHashOf(path string) string {
	// 与 CLI enqueue 同构：compose.Load 的归一化哈希。
	spec, _, err := compose.Load(context.Background(), path)
	if err != nil {
		panic(err)
	}
	return spec.SpecHash
}

// placementUnavailable / placementGone 构造放置前哨错误（场景 15/16）。
func placementUnavailable() error {
	return apperr.New("E_PLACEMENT_NODE_UNAVAILABLE", "bound node not ready (test injection)")
}

func placementGone() error {
	return apperr.New("E_PLACEMENT_NODE_GONE", "bound node removed (test injection)")
}

// compose fixtures（受控子集内的最小形态；alpine + 真 healthcheck 语义由
// 假底座承载，实机验证用真实 daemon）。
const composeV1 = `name: demo
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
`

func TestFirstDeploySucceeds(t *testing.T) {
	h := newHarness(t)
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	final := h.runToTerminal(rec)

	if final.Status != state.DeploySucceeded {
		t.Fatalf("status = %s error=%s, want succeeded", final.Status, final.ErrorCode)
	}
	if final.RevisionID == "" {
		t.Fatal("revision_id empty on success")
	}
	// 服务存在 + managed label + 三段命名 + fleetly.app 限定形容器 label
	//（v0.3 label 集：+fleetly.team / fleetly.project，rbac-teams §4.3）。
	app, err := h.store.GetAppByName(context.Background(), "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	svc, err := h.sub.ServiceInspect(context.Background(), h.svc("web"))
	if err != nil {
		t.Fatalf("service inspect: %v", err)
	}
	if svc.Labels[state.LabelManaged] != "true" || svc.Labels[state.LabelApp] != app.QualifiedName() ||
		svc.Labels[state.LabelProcess] != "web" {
		t.Fatalf("service labels = %v, want managed/app/process set", svc.Labels)
	}
	if svc.Labels[state.LabelTeam] != app.TeamSlug || svc.Labels[state.LabelProject] != app.ProjectSlug {
		t.Fatalf("ownership labels = %v, want team=%s project=%s",
			svc.Labels, app.TeamSlug, app.ProjectSlug)
	}
	if svc.ContainerLabels[state.LabelApp] != app.QualifiedName() {
		t.Fatalf("container app label = %s, want %s", svc.ContainerLabels[state.LabelApp], app.QualifiedName())
	}
	if svc.Labels[state.LabelDeployment] != rec.ID {
		t.Fatalf("deployment label = %s, want %s", svc.Labels[state.LabelDeployment], rec.ID)
	}
	if svc.DesiredHash == "" {
		t.Fatal("desired-hash label empty")
	}
	// 事件链完整。
	names := h.events()
	for _, want := range []string{"deployment.queued", "deployment.release_started",
		"deployment.healthy", "deployment.switched", "deployment.observe_started",
		"deployment.succeeded"} {
		if !hasEvent(names, want) {
			t.Fatalf("events missing %s: %v", want, names)
		}
	}
	// app 派生状态 = running。
	got, err := h.eng.AppDerivedState(context.Background(), rec.AppID)
	if err != nil {
		t.Fatalf("derived state: %v", err)
	}
	if got != DerivedRunning {
		t.Fatalf("app state = %s, want running", got)
	}
}

// TestPostDeployHookFiresOnSuccess 备份挂钩（T2.22）：部署成功终态后挂钩
// 以成功记录为载荷异步触发一次；失败路径不触发（失败分流不经成功终态）。
func TestPostDeployHookFiresOnSuccess(t *testing.T) {
	h := newHarness(t)
	fired := make(chan state.DeployRecord, 4)
	h.eng.WithPostDeployHook(func(rec state.DeployRecord) {
		fired <- rec
	})
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	final := h.runToTerminal(rec)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("status = %s, want succeeded", final.Status)
	}
	select {
	case got := <-fired:
		if got.ID != rec.ID {
			t.Fatalf("hook payload deployment = %s, want %s", got.ID, rec.ID)
		}
		if got.Status != state.DeploySucceeded {
			t.Fatalf("hook payload status = %s, want succeeded", got.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-deploy hook did not fire on success")
	}
}

// TestPostDeployHookNotFiredOnFailure 失败部署不触发备份挂钩（备份语义 =
// 部署成功后；失败路径的快照由 daily/pre_upgrade 承担）。
func TestPostDeployHookNotFiredOnFailure(t *testing.T) {
	h := newHarness(t)
	fired := make(chan state.DeployRecord, 4)
	h.eng.WithPostDeployHook(func(rec state.DeployRecord) {
		fired <- rec
	})
	h.images.missing["alpine:3"] = true
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	final := h.runToTerminal(rec)
	if final.Status != state.DeployFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	select {
	case got := <-fired:
		t.Fatalf("hook fired on failed deployment %s", got.ID)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDeployMutexSecondStaysQueued(t *testing.T) {
	h := newHarness(t)
	path := h.writeCompose(composeV1)
	first := h.enqueue(path)
	second := h.enqueue(path)

	ctx := context.Background()
	h.eng.Tick(ctx) // 第一条应被启动，第二条必须保持 queued

	row1, _ := h.store.GetDeployment(ctx, first.ID)
	row2, _ := h.store.GetDeployment(ctx, second.ID)
	if row1.Status == state.DeployQueued {
		t.Fatalf("first deployment still queued: %s", row1.Status)
	}
	if row2.Status != state.DeployQueued {
		t.Fatalf("second deployment status = %s, want queued (mutex wait)", row2.Status)
	}
}

func TestFailureUnswitchedRestoresPreviousVersion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// v1：成功部署（有效版本在位）。
	pathV1 := h.writeCompose(composeV1)
	rec1 := h.enqueue(pathV1)
	if final := h.runToTerminal(rec1); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	v1Image := h.sub.services[h.svc("web")].spec.Image
	v1Running := h.runningTaskIDs(h.svc("web"), v1Image)
	if len(v1Running) == 0 {
		t.Fatal("no running v1 tasks after success")
	}

	// v2：健康门永不通过（改 image 触发更新 + paused-health 行为）。
	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "false"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePausedHealth)
	rec2 := h.enqueue(pathV2)
	final := h.runToTerminal(rec2)

	if final.Status != state.DeployFailed || final.ErrorCode != "E_HEALTH_TIMEOUT" {
		t.Fatalf("v2 deploy = %s (%s), want failed E_HEALTH_TIMEOUT", final.Status, final.ErrorCode)
	}
	if final.Recovery != state.RecoveryReplay {
		t.Fatalf("recovery = %q, want replay (unswitched same-record recovery)", final.Recovery)
	}
	if !final.FirstHealthyAt.IsZero() {
		t.Fatal("first_healthy_at set on unswitched failure")
	}
	// 归位重放：最后有效 spec 被重新应用（ServiceUpdate 调用带 v1 镜像）。
	found := false
	for _, u := range h.sub.updates {
		if u[0] == h.svc("web") && u[1] == v1Image {
			found = true
		}
	}
	if !found {
		t.Fatalf("no restore update with v1 image %s: %v", v1Image, h.sub.updates)
	}
	// 归位零任务替换（同内容重放：v1 运行任务 id 不变、零新增运行任务，
	// Spike B2 同构断言）。
	after := h.runningTaskIDs(h.svc("web"), v1Image)
	if !equalSets(after, v1Running) {
		t.Fatalf("running v1 tasks changed after restore: %v -> %v", v1Running, after)
	}
	// 旧版本持续服务（failed 任务在列但 running 旧任务仍在）。
	names := h.events()
	if !hasEvent(names, "deployment.failed") {
		t.Fatalf("events missing deployment.failed: %v", names)
	}
	// app 派生状态：旧版本仍在服务、无 verdict → running。
	got, _ := h.eng.AppDerivedState(ctx, rec2.AppID)
	if got != DerivedRunning {
		t.Fatalf("app state = %s, want running", got)
	}
}

func TestFirstDeployFailureScalesToZero(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := h.writeCompose(composeV1)
	h.sub.setMode(h.svc("web"), modePausedStart)
	rec := h.enqueue(path)
	final := h.runToTerminal(rec)

	if final.Status != state.DeployFailed || final.ErrorCode != "E_TASK_START_FAILED" {
		t.Fatalf("deploy = %s (%s), want failed E_TASK_START_FAILED", final.Status, final.ErrorCode)
	}
	if !final.SubstrateHalted {
		t.Fatal("substrate_halted not set (first deploy failure keeps the scene at scale=0)")
	}
	svc, err := h.sub.ServiceInspect(ctx, h.svc("web"))
	if err != nil {
		t.Fatalf("service should exist (scene kept at scale=0): %v", err)
	}
	if svc.Replicas != 0 {
		t.Fatalf("replicas = %d, want 0", svc.Replicas)
	}
	if !hasEvent(h.events(), "deployment.substrate_halted") {
		t.Fatal("missing deployment.substrate_halted event")
	}
	got, _ := h.eng.AppDerivedState(ctx, rec.AppID)
	if got != DerivedDown {
		t.Fatalf("app state = %s, want down (no valid revision)", got)
	}
}

func TestObserveCrashLoopFailsUnstable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	// 推进到切流（releasing）。
	for i := 0; i < 16; i++ {
		row, _ := h.store.GetDeployment(ctx, rec.ID)
		if row.Status == state.DeployObserving {
			break
		}
		h.eng.Tick(ctx)
	}
	// 注入崩溃循环（≥2 次退出）。
	h.sub.crashNewRunning(h.svc("web"), 2, h.clk.Now())
	final := h.runToTerminal(rec)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_OBSERVE_CRASH_LOOP" {
		t.Fatalf("deploy = %s (%s), want failed E_OBSERVE_CRASH_LOOP", final.Status, final.ErrorCode)
	}
	if final.Verdict != state.VerdictUnstable {
		t.Fatalf("verdict = %q, want unstable", final.Verdict)
	}
	got, _ := h.eng.AppDerivedState(ctx, rec.AppID)
	if got != DerivedDegraded {
		t.Fatalf("app state = %s, want degraded", got)
	}
	names := h.events()
	if !hasEvent(names, "app.degraded") {
		t.Fatalf("missing app.degraded: %v", names)
	}
}

func TestObserveIgnoresPriorDeploymentCrashHistory(t *testing.T) {
	h := newHarness(t)
	// 首次部署成功。
	rec1 := h.enqueue(h.writeCompose(composeV1))
	if row := h.runToTerminal(rec1); row.Status != state.DeploySucceeded {
		t.Fatalf("first deploy = %s (%s)", row.Status, row.ErrorCode)
	}
	// 注入两条同镜像的历史崩溃任务：时间戳在第二次部署发布开始之前
	// （上一部署的崩溃史，与目标镜像相同——时间界是唯一判据）。
	h.sub.crashNewRunning(h.svc("web"), 2, h.clk.Now())
	// 同镜像连续部署：旧崩溃史不得计入新观察窗（回归：曾误判
	// E_OBSERVE_CRASH_LOOP，见 T2-6 实机发现）。
	rec2 := h.enqueue(h.writeCompose(composeV1))
	row := h.runToTerminal(rec2)
	if row.Status != state.DeploySucceeded {
		t.Fatalf("second deploy = %s (%s), want succeeded (prior crash history must not misread as a crash loop)", row.Status, row.ErrorCode)
	}
}

func TestObserveWindowPassesAfterFreshWindow(t *testing.T) {
	h := newHarness(t)
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	row := h.runToTerminal(rec)
	if row.Status != state.DeploySucceeded {
		t.Fatalf("status = %s (%s)", row.Status, row.ErrorCode)
	}
	// 观察窗确实走满默认 60s（fake clock 推进量 > observe window）。
	if h.clk.Now().Sub(testStart) < 60*time.Second {
		t.Fatalf("clock advanced only %s, expect >= observe window", h.clk.Now().Sub(testStart))
	}
}

func TestObserveSingleCrashSelfHealsWarns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	for i := 0; i < 16; i++ {
		row, _ := h.store.GetDeployment(ctx, rec.ID)
		if row.Status == state.DeployObserving {
			break
		}
		h.eng.Tick(ctx)
	}
	// 单次退出（Swarm 重启自愈：运行任务保留、失败记录在列）。
	h.sub.crashNewRunning(h.svc("web"), 1, h.clk.Now())
	final := h.runToTerminal(rec)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("status = %s (%s), want succeeded (warning pass)", final.Status, final.ErrorCode)
	}
	if final.Flags&state.DeployFlagInstabilityWarning == 0 {
		t.Fatal("instability warning flag not set")
	}
	if !hasEvent(h.events(), "deployment.warning") {
		t.Fatal("missing deployment.warning event")
	}
}

func TestCancelUnswitchedRestoresThenCancels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	v1Image := h.sub.services[h.svc("web")].spec.Image

	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePending) // 迟迟未切流
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8; i++ {
		row, _ := h.store.GetDeployment(ctx, rec2.ID)
		if row.Status == state.DeployReleasing {
			break
		}
		h.eng.Tick(ctx)
	}
	eng := h.eng
	if err := eng.CancelRequest(ctx, mustGet(h, rec2.ID)); err != nil {
		t.Fatalf("cancel request: %v", err)
	}
	final := h.runToTerminal(rec2)
	if final.Status != state.DeployCancelled {
		t.Fatalf("status = %s (%s), want cancelled", final.Status, final.ErrorCode)
	}
	if final.Recovery != state.RecoveryReplay {
		t.Fatalf("recovery = %q, want restore", final.Recovery)
	}
	found := false
	for _, u := range h.sub.updates {
		if u[1] == v1Image {
			found = true
		}
	}
	if !found {
		t.Fatal("cancel did not restore previous version before terminal")
	}
	if !hasEvent(h.events(), "deployment.cancelled") {
		t.Fatal("missing deployment.cancelled event")
	}
}

func TestCancelRejectedAfterSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	for i := 0; i < 16; i++ {
		row, _ := h.store.GetDeployment(ctx, rec.ID)
		if row.Status == state.DeployObserving {
			break
		}
		h.eng.Tick(ctx)
	}
	err := h.eng.CancelRequest(ctx, mustGet(h, rec.ID))
	if err == nil {
		t.Fatal("cancel after switch accepted, want 409")
	}
	var ae *apperr.Error
	if !asAppErr(err, &ae) || ae == nil {
		t.Fatalf("cancel error not apperr: %v", err)
	}
	if ae.Code() != "E_STATE_VERSION_CONFLICT" {
		t.Fatalf("cancel code = %s, want E_STATE_VERSION_CONFLICT (409)", ae.Code())
	}
}

func TestWatchdogPendingTimeoutRestores(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	v1Image := h.sub.services[h.svc("web")].spec.Image

	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePending)
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8 && !h.releasing(rec2.ID); i++ {
		h.eng.Tick(ctx)
	}
	// 推进时钟越过看门狗 deadline（300s）。
	h.clk.Advance(301 * time.Second)
	final := h.runToTerminal(rec2)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_SCHEDULER_PENDING_TIMEOUT" {
		t.Fatalf("deploy = %s (%s), want failed E_SCHEDULER_PENDING_TIMEOUT", final.Status, final.ErrorCode)
	}
	if final.Recovery != state.RecoveryReplay {
		t.Fatalf("recovery = %q, want restore", final.Recovery)
	}
	found := false
	for _, u := range h.sub.updates {
		if u[1] == v1Image {
			found = true
		}
	}
	if !found {
		t.Fatal("watchdog timeout did not restore previous version")
	}
}

func TestBlockedWaitingPausesWatchdogAndResumes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	// v2 部署中绑定节点 DOWN（场景 15）。
	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePending)
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8 && !h.releasing(rec2.ID); i++ {
		h.eng.Tick(ctx)
	}
	h.resolver.preflightErrs = []error{placementUnavailable()}
	h.eng.Tick(ctx)
	row, _ := h.store.GetDeployment(ctx, rec2.ID)
	if row.Phase != state.PhaseBlockedWaiting {
		t.Fatalf("phase = %q, want blocked_waiting", row.Phase)
	}
	deadline := row.WatchdogDeadlineAt

	// blocked 期间时钟大幅推进（10min > 300s 看门狗）：看门狗暂停、不判死。
	h.clk.Advance(10 * time.Minute)
	h.eng.Tick(ctx)
	row, _ = h.store.GetDeployment(ctx, rec2.ID)
	if row.Status != state.DeployReleasing {
		t.Fatalf("watchdog fired during blocked_waiting: %s (%s)", row.Status, row.ErrorCode)
	}
	if !hasEvent(h.events(), "deployment.recovery_blocked") {
		t.Fatal("missing deployment.recovery_blocked event")
	}
	if !hasEvent(h.events(), "placement.blocked") {
		t.Fatal("missing placement.blocked event")
	}

	// 节点恢复：退出 blocked_waiting、看门狗重新起算（重置）。
	h.eng.Tick(ctx)
	row, _ = h.store.GetDeployment(ctx, rec2.ID)
	if row.Phase != "" {
		t.Fatalf("phase = %q, want cleared", row.Phase)
	}
	if !row.WatchdogDeadlineAt.After(deadline) {
		t.Fatalf("deadline not re-armed after resume: %v -> %v", deadline, row.WatchdogDeadlineAt)
	}
	if !hasEvent(h.events(), "placement.recovered") {
		t.Fatal("missing placement.recovered event")
	}
}

// TestPrepareBudgetAnchoredAtPickupNotEnqueue（H11 机制测试）：排队时长不
// 计入准备预算。入队已久（10min > 300s）的 queued 行被拾取——基线随
// queued→preparing 写入 = 拾取时刻——第一拍不得立即假失败 E_RUNTIME_UNAVAILABLE
// （旧缺陷：预算自 created_at 起算，未触底座、无副作用的假失败，用户必须
// 重发），链路正常走完。
func TestPrepareBudgetAnchoredAtPickupNotEnqueue(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	// 排队 10min：同 app 互斥等待前序部署终态（releasing 300s + observing
	// 60s）或控制面停机窗口的形态。
	h.clk.Advance(10 * time.Minute)
	h.eng.Tick(ctx)
	row := mustGet(h, rec.ID)
	if row.Status == state.DeployFailed {
		t.Fatalf("long-queued deploy failed on pickup tick: %s (queue time must not count into the preparing budget)",
			row.ErrorCode)
	}
	if row.PhaseStartedAt.IsZero() || !row.PhaseStartedAt.After(rec.CreatedAt) {
		t.Fatalf("phase_started_at = %v, want pick-up anchor after created_at %v",
			row.PhaseStartedAt, rec.CreatedAt)
	}
	final := h.runToTerminal(rec)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded (budget counts from the pickup baseline)",
			final.Status, final.ErrorCode)
	}
}

// TestPrepareBudgetLegacyRowFallsBackToCreatedAt（H11 对照组）：迁移 00009
// 之前的存量 preparing 行（phase_started_at 为 NULL）回落 created_at——旧
// 语义（入队起算、超时失败）不变。
func TestPrepareBudgetLegacyRowFallsBackToCreatedAt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	// 存量行形态：升级时已在 preparing、基线未写（NULL）。
	if err := h.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE deployments SET status = 'preparing', phase_started_at = NULL WHERE id = ?`, rec.ID)
		return err
	}); err != nil {
		t.Fatalf("seed legacy preparing row: %v", err)
	}
	h.clk.Advance(10 * time.Minute)
	h.eng.Tick(ctx)
	final := mustGet(h, rec.ID)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_RUNTIME_UNAVAILABLE" {
		t.Fatalf("legacy row = %s (%s), want failed E_RUNTIME_UNAVAILABLE (missing baseline falls back to created_at, legacy semantics unchanged)",
			final.Status, final.ErrorCode)
	}
	// 未触底座：无服务创建。
	if len(h.sub.services) != 0 {
		t.Fatalf("substrate touched on budget failure: %v", h.sub.services)
	}
}

// TestRollbackPrepareBudgetAnchoredAtPickupRetries（H11 回滚路径机制测试）：
// 入队已久的回滚被拾取后遇底座不可达（ErrNotSwarmReady 暂态）——预算自拾取
// 基线起算：预算内逐拍重试（行留 preparing、不触底座），耗尽后才以
// E_RUNTIME_UNAVAILABLE 落 failed（rollback_failed reason=preflight）。
func TestRollbackPrepareBudgetAnchoredAtPickupRetries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v1, _ := rollbackFixture(t, h)
	rec, err := EnqueueRollback(ctx, h.store, RollbackInput{
		AppName:          "demo",
		TargetRevisionID: v1.RevisionID,
	})
	if err != nil {
		t.Fatalf("enqueue rollback: %v", err)
	}
	// 排队 10min + 底座不可达。
	h.clk.Advance(10 * time.Minute)
	h.sub.swarmErr = ErrNotSwarmReady
	h.eng.Tick(ctx)
	row := mustGet(h, rec.ID)
	// 旧缺陷：预算自 created_at 起算 → 拾取第一拍即 failed（未触底座）。
	if row.Status != state.DeployPreparing {
		t.Fatalf("rollback row = %s (%s), want preparing (retry within budget, no immediate terminal failure)",
			row.Status, row.ErrorCode)
	}
	if row.PhaseStartedAt.IsZero() || !row.PhaseStartedAt.After(rec.CreatedAt) {
		t.Fatalf("phase_started_at = %v, want pick-up anchor after created_at %v",
			row.PhaseStartedAt, rec.CreatedAt)
	}
	// 暂态窗口内逐拍重试：仍非终态、无归位重放（不触底座）。
	updatesBefore := len(h.sub.updates)
	for i := 0; i < 3; i++ {
		h.clk.Advance(30 * time.Second) // 自基线累计 90s < 300s
		h.eng.Tick(ctx)
	}
	row = mustGet(h, rec.ID)
	if row.Status.Terminal() {
		t.Fatalf("rollback terminally failed within budget: %s (%s)", row.Status, row.ErrorCode)
	}
	if len(h.sub.updates) != updatesBefore {
		t.Fatalf("substrate touched during unavailable window: %v", h.sub.updates)
	}
	// 预算耗尽（基线 + 301s > 300s）：E_RUNTIME_UNAVAILABLE + preflight 失败事件。
	h.clk.Advance(301 * time.Second)
	h.eng.Tick(ctx)
	final := mustGet(h, rec.ID)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_RUNTIME_UNAVAILABLE" {
		t.Fatalf("rollback = %s (%s), want failed E_RUNTIME_UNAVAILABLE (budget counted from the baseline and exhausted)",
			final.Status, final.ErrorCode)
	}
	if !hasEvent(h.events(), "deployment.rollback_failed") {
		t.Fatal("missing deployment.rollback_failed（reason=preflight）")
	}
}

func TestBoundNodeRemovedFailsDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePending)
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8 && !h.releasing(rec2.ID); i++ {
		h.eng.Tick(ctx)
	}
	h.resolver.preflightErrs = []error{placementGone()}
	final := h.runToTerminal(rec2)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_PLACEMENT_NODE_GONE" {
		t.Fatalf("deploy = %s (%s), want failed E_PLACEMENT_NODE_GONE", final.Status, final.ErrorCode)
	}
}

func TestRestartRecoveryClassifiesReleasing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	v1Image := h.sub.services[h.svc("web")].spec.Image

	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePausedHealth)
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8 && !h.releasing(rec2.ID); i++ {
		h.eng.Tick(ctx)
	}
	// 控制面重启：新引擎实例（同 store/底座）执行启动扫描分类恢复。
	eng2 := NewEngine(Config{}, h.store, h.sub, h.images, h.resolver, h.box, slog.New(slog.NewTextHandler(io.Discard, nil))).WithClock(h.clk)
	eng2.recoverInterrupted(ctx)
	final := h.runToTerminalWith(rec2, eng2)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_HEALTH_TIMEOUT" {
		t.Fatalf("recovered deploy = %s (%s), want classified failure E_HEALTH_TIMEOUT", final.Status, final.ErrorCode)
	}
	if final.Recovery != state.RecoveryReplay {
		t.Fatalf("recovery = %q, want restore", final.Recovery)
	}
	found := false
	for _, u := range h.sub.updates {
		if u[1] == v1Image {
			found = true
		}
	}
	if !found {
		t.Fatal("recovery classification did not restore previous version")
	}
}

// TestRestartRecoveryPreservesBlockedWaiting（H12 机制测试，场景 15）：节点
// down 期间控制面重启（如升级）——releasing + phase=blocked_waiting 的行
// 不得被启动扫描分类改写：不落 E_DEPLOY_INTERRUPTED、不归位（节点仍不可用，
// 归位同样滞留），交回 tick 的 watchBoundNode；节点恢复后续跑并重新起算，
// 链路走完。
func TestRestartRecoveryPreservesBlockedWaiting(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	// v2 发布中绑定节点 DOWN（场景 15）→ releasing + blocked_waiting。
	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePending)
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8 && !h.releasing(rec2.ID); i++ {
		h.eng.Tick(ctx)
	}
	h.resolver.preflightErrs = []error{placementUnavailable()}
	h.eng.Tick(ctx)
	row := mustGet(h, rec2.ID)
	if row.Phase != state.PhaseBlockedWaiting {
		t.Fatalf("phase = %q, want blocked_waiting", row.Phase)
	}

	// 控制面重启（节点仍 down）：新引擎实例的启动扫描——旧缺陷落入
	// default 分类分支：E_DEPLOY_INTERRUPTED 假失败 + restoreSnapshot 归位。
	eng2 := NewEngine(Config{}, h.store, h.sub, h.images, h.resolver, h.box,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithClock(h.clk)
	eng2.recoverInterrupted(ctx)
	after := mustGet(h, rec2.ID)
	if after.Status != state.DeployReleasing || after.Phase != state.PhaseBlockedWaiting {
		t.Fatalf("restart rewrote blocked_waiting row: status=%s phase=%s (waiting semantics must not be lost)",
			after.Status, after.Phase)
	}
	if after.ErrorCode != "" {
		t.Fatalf("restart wrote error_code %q on blocked_waiting row", after.ErrorCode)
	}

	// 节点恢复：底座任务转健康（节点回来后 Swarm 调度启动）→ tick 续跑
	//（resumeFromBlocked 重臂看门狗）→ 链路走完。
	svc := h.sub.services[h.svc("web")]
	svc.update = "completed"
	svc.tasks = h.sub.runningTasks(svc, "t-resumed")
	final := h.runToTerminalWith(rec2, eng2)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("resumed deploy = %s (%s), want succeeded (node recovery resumes and re-arms the budget)",
			final.Status, final.ErrorCode)
	}
	if !hasEvent(h.events(), "placement.recovered") {
		t.Fatal("missing placement.recovered (resume event)")
	}
}

// TestRestartRecoveryUndeterminableFailsInterrupted（H12 对照组）：无 phase
// 的 releasing 行走既有分类——现场无法判定（更新中、无进展）→
// E_DEPLOY_INTERRUPTED 失败 + 归位（§2.3）。
func TestRestartRecoveryUndeterminableFailsInterrupted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	v1Image := h.sub.services[h.svc("web")].spec.Image
	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePending)
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8 && !h.releasing(rec2.ID); i++ {
		h.eng.Tick(ctx)
	}
	// 控制面重启：任务滞留 PENDING、更新进行中 → 无法判定 → 失败 + 归位。
	eng2 := NewEngine(Config{}, h.store, h.sub, h.images, h.resolver, h.box,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithClock(h.clk)
	eng2.recoverInterrupted(ctx)
	final := mustGet(h, rec2.ID)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_DEPLOY_INTERRUPTED" {
		t.Fatalf("recovered deploy = %s (%s), want failed E_DEPLOY_INTERRUPTED (phase-less row follows the existing classification)",
			final.Status, final.ErrorCode)
	}
	if final.Recovery != state.RecoveryReplay {
		t.Fatalf("recovery = %q, want restore", final.Recovery)
	}
	found := false
	for _, u := range h.sub.updates {
		if u[1] == v1Image {
			found = true
		}
	}
	if !found {
		t.Fatal("undeterminable classification did not restore previous version")
	}
}

func TestRestartRecoveryReopensObserveWindow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	for i := 0; i < 16 && !h.observing(rec.ID); i++ {
		h.eng.Tick(ctx)
	}
	before, _ := h.store.GetDeployment(ctx, rec.ID)
	// 时钟前进（重启恢复发生在原观察窗开启之后）。
	h.clk.Advance(5 * time.Second)
	// 控制面重启：健康 → 重开完整观察窗。
	eng2 := NewEngine(Config{}, h.store, h.sub, h.images, h.resolver, h.box, slog.New(slog.NewTextHandler(io.Discard, nil))).WithClock(h.clk)
	eng2.recoverInterrupted(ctx)
	after, _ := h.store.GetDeployment(ctx, rec.ID)
	if !after.ObserveStartedAt.After(before.ObserveStartedAt) {
		t.Fatalf("observe window not reopened: %v -> %v", before.ObserveStartedAt, after.ObserveStartedAt)
	}
	final := h.runToTerminalWith(rec, eng2)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("status = %s (%s), want succeeded after reopened window", final.Status, final.ErrorCode)
	}
}

func TestReconcileRemovesDroppedService(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 双服务部署。
	path2 := h.writeCompose(`name: demo
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
  worker:
    image: alpine:3
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	if final := h.runToTerminal(h.enqueue(path2)); final.Status != state.DeploySucceeded {
		t.Fatalf("two-service deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	if _, err := h.sub.ServiceInspect(ctx, h.svc("worker")); err != nil {
		t.Fatalf("worker service missing: %v", err)
	}
	// 移除 worker 再部署 → 对账删除（省略=删除）。
	path1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(path1)); final.Status != state.DeploySucceeded {
		t.Fatalf("reduced deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	if _, err := h.sub.ServiceInspect(ctx, h.svc("worker")); err != ErrServiceNotFound {
		t.Fatalf("worker service should be removed, got %v", err)
	}
	if len(h.sub.removed) == 0 || h.sub.removed[len(h.sub.removed)-1] != h.svc("worker") {
		t.Fatalf("removed = %v, want fleetly-demo-worker", h.sub.removed)
	}
}

func TestEnvPendingPromotedOnSuccess(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 平台 env pending（密文入库——CLI 同构）。
	app, err := ensureAppForTest(t, ctx, h.store, "demo")
	if err != nil {
		t.Fatalf("ensure app: %v", err)
	}
	plain := []byte("v=1")
	ct, err := h.box.Encrypt(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := h.store.SetAppEnv(ctx, app.ID, "DEMO_TOKEN", string(ct), "platform", "human"); err != nil {
		t.Fatalf("set env: %v", err)
	}
	path := h.writeCompose(composeV1)
	final := h.runToTerminal(h.enqueue(path))
	if final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	// pending → effective。
	row, err := h.store.GetAppEnv(ctx, app.ID, "DEMO_TOKEN")
	if err != nil {
		t.Fatalf("get env: %v", err)
	}
	if row.Status != state.EnvStatusEffective {
		t.Fatalf("env status = %s, want effective (takes effect with the deploy)", row.Status)
	}
	// 合并结果注入容器 env（key 在列；值不进断言输出）。
	env := h.sub.services[h.svc("web")].spec.Env
	found := false
	for _, kv := range env {
		if len(kv) > 10 && kv[:10] == "DEMO_TOKEN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DEMO_TOKEN not injected into service env: %d entries", len(env))
	}
}

// TestEnvChangedHookFiresOnPromote H9：部署成功且实际提升 pending env
// （n>0）时触发 env 变更挂钩（日志脱敏值集失效联动）；二次成功部署无
// pending（n=0）不重复触发。
func TestEnvChangedHookFiresOnPromote(t *testing.T) {
	h := newHarness(t)
	fired := make(chan string, 4)
	h.eng.WithEnvChangedHook(func(appID string) { fired <- appID })
	ctx := context.Background()
	app, err := ensureAppForTest(t, ctx, h.store, "demo")
	if err != nil {
		t.Fatalf("ensure app: %v", err)
	}
	ct, err := h.box.Encrypt([]byte("v=1-with-promote"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := h.store.SetAppEnv(ctx, app.ID, "DEMO_TOKEN", string(ct), "platform", "human"); err != nil {
		t.Fatalf("set env: %v", err)
	}
	path := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(path)); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	select {
	case got := <-fired:
		if got != app.ID {
			t.Fatalf("hook payload appID = %s, want %s", got, app.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("env changed hook did not fire on promote")
	}
	// 二次部署：无 pending（n=0）不触发。
	pathV2 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV2)); final.Status != state.DeploySucceeded {
		t.Fatalf("second deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	select {
	case got := <-fired:
		t.Fatalf("hook fired with no pending env promoted: %s", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStopFirstDowntimeAccounted(t *testing.T) {
	h := newHarness(t)
	// 有卷服务：order 强制 stop-first（配置在 fixture 中声明命名卷）。
	pathV1 := h.writeCompose(`name: demo
services:
  db:
    image: alpine:3
    command: ["sleep", "infinity"]
    volumes:
      - data:/data
volumes:
  data:
`)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s (%s)", final.Status, final.ErrorCode)
	}
	spec := h.sub.services[h.svc("db")].spec
	if spec.UpdateOrder != "stop-first" {
		t.Fatalf("volume service order = %q, want stop-first (platform-enforced)", spec.UpdateOrder)
	}
	v1Image := spec.Image

	pathV2 := h.writeCompose(`name: demo
services:
  db:
    image: alpine:4
    command: ["sleep", "infinity"]
    volumes:
      - data:/data
volumes:
  data:
`)
	h.sub.setMode(h.svc("db"), modePausedStart)
	rec2 := h.enqueue(pathV2)
	final := h.runToTerminal(rec2)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_TASK_START_FAILED" {
		t.Fatalf("deploy = %s (%s), want failed E_TASK_START_FAILED", final.Status, final.ErrorCode)
	}
	if final.Recovery != state.RecoveryReplay {
		t.Fatalf("recovery = %q, want restore (stop-first forces recovery)", final.Recovery)
	}
	// 停机账（§2.6 如实累计）：起止时间戳齐备，ms 与起止差一致。假时钟单
	// tick 内不流动 → 差值为 0 合法；实机 stop-first 归位 10–12s 量级
	//（Spike B），ms 如实为正。
	if final.DowntimeStartedAt.IsZero() || final.DowntimeEndedAt.IsZero() {
		t.Fatal("downtime timestamps missing")
	}
	want := final.DowntimeEndedAt.Sub(final.DowntimeStartedAt).Milliseconds()
	if final.DowntimeMS != want {
		t.Fatalf("downtime_ms = %d, want %d (= ended-started, accounted truthfully)", final.DowntimeMS, want)
	}
	found := false
	for _, u := range h.sub.updates {
		if u[1] == v1Image {
			found = true
		}
	}
	if !found {
		t.Fatal("stop-first failure did not force-restore previous version")
	}
}

func TestPostWindowUnstableAlertsOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := h.writeCompose(composeV1)
	rec := h.enqueue(path)
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s", final.Status)
	}
	// 窗后崩溃（L4）：注入 failed 且时间戳晚于窗末。
	h.clk.Advance(30 * time.Second)
	h.sub.crashNewRunning(h.svc("web"), 1, h.clk.Now())
	h.eng.Tick(ctx)
	got, _ := h.eng.AppDerivedState(ctx, rec.AppID)
	if got != DerivedDegraded {
		t.Fatalf("app state = %s, want degraded (post-window instability)", got)
	}
	names := h.events()
	if !hasEvent(names, "app.instability_detected") || !hasEvent(names, "deployment.warning") {
		t.Fatalf("missing post-window events: %v", names)
	}
	// 只告警一次：再次巡检不重复。
	before := len(h.events())
	h.eng.Tick(ctx)
	if after := len(h.events()); after != before {
		t.Fatalf("post-window alert repeated: %d -> %d", before, after)
	}
}

func TestImageMissingFailsFast(t *testing.T) {
	h := newHarness(t)
	path := h.writeCompose(composeV1)
	h.images.missing["alpine:3"] = true
	rec := h.enqueue(path)
	final := h.runToTerminal(rec)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_IMAGE_PULL_FAILED" {
		t.Fatalf("deploy = %s (%s), want failed E_IMAGE_PULL_FAILED", final.Status, final.ErrorCode)
	}
	// 不动底座：无服务创建。
	if len(h.sub.services) != 0 {
		t.Fatalf("substrate touched on preflight failure: %v", h.sub.services)
	}
}

func TestBuildMissingFailsWithBuildCode(t *testing.T) {
	h := newHarness(t)
	path := h.writeCompose(`name: demo
services:
  web:
    build:
      context: .
    command: ["sleep", "infinity"]
`)
	rec := h.enqueue(path)
	final := h.runToTerminal(rec)
	if final.Status != state.DeployFailed || final.ErrorCode != "E_BUILD_FAILED" {
		t.Fatalf("deploy = %s (%s), want failed E_BUILD_FAILED (no matching build)", final.Status, final.ErrorCode)
	}
}

// TestBlockedWaitingHoldsAcrossDeadlineWithPersistentDown 场景 15 回归
// （B4/MG-2/H2）：绑定节点持续 DOWN（持续前哨失败，非一次性）越过
// WatchdogDeadlineAt 再 +10min——L2 看门狗暂停计时：部署不失败、phase 维持
// blocked_waiting；节点恢复后 resumeFromBlocked 重置 deadline 并续跑到终态。
// 旧缺陷被一次性 fake 掩盖（下一拍即恢复，看门狗判定不可达）：blocked 维持
// 态的评估继续下行到无 phase 守卫的看门狗判定，每拍触发
// E_SCHEDULER_PENDING_TIMEOUT 假失败。
func TestBlockedWaitingHoldsAcrossDeadlineWithPersistentDown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	pathV1 := h.writeCompose(composeV1)
	if final := h.runToTerminal(h.enqueue(pathV1)); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	// v2 发布中绑定节点 DOWN（场景 15）→ blocked_waiting。
	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`)
	h.sub.setMode(h.svc("web"), modePending)
	rec2 := h.enqueue(pathV2)
	for i := 0; i < 8 && !h.releasing(rec2.ID); i++ {
		h.eng.Tick(ctx)
	}
	// 持续 DOWN：persistentPreflightFail 每拍都失败（区别于一次性队列）。
	h.resolver.persistentPreflightFail = placementUnavailable()
	h.eng.Tick(ctx)
	row := mustGet(h, rec2.ID)
	if row.Phase != state.PhaseBlockedWaiting {
		t.Fatalf("phase = %q, want blocked_waiting", row.Phase)
	}
	deadline := row.WatchdogDeadlineAt

	// 越过 deadline + 10min，逐拍维持：不失败、phase 不变（看门狗暂停）。
	h.clk.Advance(deadline.Sub(h.clk.Now()) + 10*time.Minute)
	for i := 0; i < 3; i++ {
		h.eng.Tick(ctx)
	}
	row = mustGet(h, rec2.ID)
	if row.Status != state.DeployReleasing || row.Phase != state.PhaseBlockedWaiting {
		t.Fatalf("persistent down: status=%s phase=%s error=%s (watchdog must pause its clock in the blocked maintained state)",
			row.Status, row.Phase, row.ErrorCode)
	}
	if hasEvent(h.events(), "deployment.failed") {
		t.Fatal("deployment.failed emitted during blocked_waiting (spurious failure)")
	}

	// 节点恢复 + 任务转健康：resumeFromBlocked 重置 deadline 并续跑到成功。
	h.resolver.persistentPreflightFail = nil
	svc := h.sub.services[h.svc("web")]
	svc.update = "completed"
	svc.message = ""
	svc.tasks = h.sub.runningTasks(svc, "t-new")
	h.eng.Tick(ctx)
	row = mustGet(h, rec2.ID)
	if row.Phase != "" {
		t.Fatalf("phase = %q after resume, want cleared", row.Phase)
	}
	if !row.WatchdogDeadlineAt.After(deadline) {
		t.Fatalf("deadline not re-armed after resume: %v -> %v", deadline, row.WatchdogDeadlineAt)
	}
	final := h.runToTerminal(rec2)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("final = %s (%s), want succeeded (recovery resumes with a re-armed budget)", final.Status, final.ErrorCode)
	}
}

// TestReconcileFailureRoutesThroughFailureDispatch M1-2 回归：releasing 对账
// 执行失败（半应用现场——web 已更新、worker 创建失败）必须走失败分流
// （D-REL-4 唯一入口）：未切流 → 同记录归位重放（最后有效版本），而非直接
// 落 failed 留下无人处置的半应用现场。
func TestReconcileFailureRoutesThroughFailureDispatch(t *testing.T) {
	h := newHarness(t)
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	v1Image := h.sub.services[h.svc("web")].spec.Image

	// v2 双服务：web 更新成功、worker 创建失败（对账中途失败）。
	h.sub.failUpdates[h.svc("worker")] = errors.New("injected reconcile failure: worker create")
	pathV2 := h.writeCompose(`name: demo
services:
  web:
    image: alpine:4
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
  worker:
    image: alpine:3
    command: ["sleep", "infinity"]
`)
	rec2 := h.enqueue(pathV2)
	final := h.runToTerminal(rec2)
	if final.Status != state.DeployFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	if final.ErrorCode != "E_RUNTIME_UNAVAILABLE" {
		t.Fatalf("error_code = %s, want E_RUNTIME_UNAVAILABLE (underlying err normalized)", final.ErrorCode)
	}
	// 失败分流生效：未切流 → recovery=restore + v1 快照重放（web 被归位回
	// v1 镜像——旧缺陷直接 failed，无归位动作）。
	if final.Recovery != state.RecoveryReplay {
		t.Fatalf("recovery = %q, want restore (reconcile failure routed through failure dispatch)", final.Recovery)
	}
	restored := false
	for _, u := range h.sub.updates {
		if u[0] == h.svc("web") && u[1] == v1Image {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("reconcile failure did not restore previous version: %v", h.sub.updates)
	}
}

// TestFirstDeployReconcileFailureScalesToZero M1-2 首发分支：无版本可归位 →
// scale=0 保留现场（substrate_halted + app down），而非直接 failed 无处置。
func TestFirstDeployReconcileFailureScalesToZero(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.sub.failUpdates[h.svc("worker")] = errors.New("injected reconcile failure: worker create")
	path := h.writeCompose(`name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
  worker:
    image: alpine:3
    command: ["sleep", "infinity"]
`)
	rec := h.enqueue(path)
	final := h.runToTerminal(rec)
	if final.Status != state.DeployFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	if !final.SubstrateHalted {
		t.Fatal("substrate_halted not set (first deploy failure keeps the scene at scale=0)")
	}
	// 已创建的 web 副本清零（worker 未创建：ServiceInspect NotFound 跳过）。
	svc, err := h.sub.ServiceInspect(ctx, h.svc("web"))
	if err != nil {
		t.Fatalf("web service should exist (scene kept at scale=0): %v", err)
	}
	if svc.Replicas != 0 {
		t.Fatalf("web replicas = %d, want 0", svc.Replicas)
	}
	if !hasEvent(h.events(), "deployment.substrate_halted") {
		t.Fatal("missing deployment.substrate_halted event")
	}
	got, _ := h.eng.AppDerivedState(ctx, rec.AppID)
	if got != DerivedDown {
		t.Fatalf("app state = %s, want down (first deploy failure leaves no desired instances)", got)
	}
}

// TestRecoveryRetriesWhenSwarmUnavailableAtStartup M1-8 回归：启动恢复遇
// SwarmReady 失败不再一次性尽力——tick 以 30s 频控重试；底座恢复后观察窗
// 部署被正确 reopen（完整窗口），而非带陈旧 ObserveStartedAt 直接判窗末。
func TestRecoveryRetriesWhenSwarmUnavailableAtStartup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	obs := h.runToStatus(t, rec, state.DeployObserving)
	before := obs.ObserveStartedAt

	// 控制面重启 + 底座不可达：启动扫描登记重试（不放弃）。
	eng2 := NewEngine(Config{}, h.store, h.sub, h.images, h.resolver, h.box,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithClock(h.clk)
	h.sub.swarmErr = ErrNotSwarmReady
	eng2.recoverInterrupted(ctx)
	if !eng2.recoveryPending {
		t.Fatal("recovery not marked pending after swarm-unavailable startup")
	}

	// 频控未到（< 30s）：tick 不重试；窗口陈旧化推进越过原窗末——若恢复
	// 缺失（旧缺陷），下一拍即以陈旧 ObserveStartedAt 判窗末。
	h.clk.Advance(10 * time.Second)
	eng2.Tick(ctx)
	row := mustGet(h, rec.ID)
	if row.Status != state.DeployObserving {
		t.Fatalf("status = %s, want observing (inside rate-control window: no retry, no stale window-end misread)", row.Status)
	}

	// 越过 30s 频控 + 底座恢复：重试成功 → 观察窗完整重开。
	h.sub.swarmErr = nil
	h.clk.Advance(h.eng.cfg.ObserveWindow + time.Second)
	eng2.Tick(ctx)
	row = mustGet(h, rec.ID)
	if !row.ObserveStartedAt.After(before) {
		t.Fatalf("observe window not reopened on retry: %v -> %v", before, row.ObserveStartedAt)
	}
	reopened := row.ObserveStartedAt
	// 重开后的窗口内不判窗末（新窗口起点 + 60s 才到窗末）。
	h.clk.Advance(30 * time.Second)
	eng2.Tick(ctx)
	row = mustGet(h, rec.ID)
	if row.Status != state.DeployObserving {
		t.Fatalf("status = %s, want observing (a reopened window must not be judged at its end from the stale window)", row.Status)
	}
	_ = reopened
	final := h.runToTerminalWith(rec, eng2)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("final = %s (%s), want succeeded (reopened window runs to completion)", final.Status, final.ErrorCode)
	}
	if eng2.recoveryPending {
		t.Fatal("recovery still pending after successful retry")
	}
}

// TestObserveIgnoresPreSwitchCrashes M1-16 回归：任务在过健康门切流之前
// 崩溃 2 次、随后健康切流——观察窗不得把发布期崩溃计入窗口（窗口自切流点
// first_healthy_at 起算）而误判 E_OBSERVE_CRASH_LOOP。
func TestObserveIgnoresPreSwitchCrashes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 首更新滞留 PENDING：部署停在 releasing（健康门未过，未切流）。
	h.sub.setMode(h.svc("web"), modePending)
	rec := h.enqueue(h.writeCompose(composeV1))
	h.eng.Tick(ctx)
	row := mustGet(h, rec.ID)
	if row.Status != state.DeployReleasing {
		t.Fatalf("status = %s after first tick, want releasing", row.Status)
	}
	// 发布期崩溃 2 次（时间戳晚于 ReleaseStartedAt、早于切流点）。
	h.clk.Advance(time.Second)
	h.sub.crashNewRunning(h.svc("web"), 2, h.clk.Now())
	// 健康门通过 → 切流（FirstHealthyAt 晚于崩溃时间戳）→ 观察窗首拍即
	// 旧缺陷的误判点（since=ReleaseStartedAt 把 2 次发布期崩溃计入窗口 →
	// E_OBSERVE_CRASH_LOOP）。
	svc := h.sub.services[h.svc("web")]
	svc.update = "completed"
	svc.message = ""
	svc.tasks = h.sub.runningTasks(svc, "t-new")
	h.clk.Advance(2 * time.Second)
	h.eng.Tick(ctx)
	row = mustGet(h, rec.ID)
	if row.Status != state.DeployObserving {
		t.Fatalf("status = %s, want observing (healthy switch)", row.Status)
	}
	final := h.runToTerminal(rec)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded (releasing-phase crashes do not count into the observe window)", final.Status, final.ErrorCode)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (h *harness) releasing(id string) bool {
	row, err := h.store.GetDeployment(context.Background(), id)
	return err == nil && row.Status == state.DeployReleasing
}

func (h *harness) observing(id string) bool {
	row, err := h.store.GetDeployment(context.Background(), id)
	return err == nil && row.Status == state.DeployObserving
}

func (h *harness) runToTerminalWith(rec state.DeployRecord, eng *Engine) state.DeployRecord {
	h.t.Helper()
	ctx := context.Background()
	for i := 0; i < 64; i++ {
		row, err := h.store.GetDeployment(ctx, rec.ID)
		if err != nil {
			h.t.Fatalf("get deployment: %v", err)
		}
		if row.Status.Terminal() {
			return row
		}
		h.clk.Advance(2 * time.Second)
		eng.Tick(ctx)
	}
	h.t.Fatalf("deployment %s did not reach terminal state within 64 ticks", rec.ID)
	return state.DeployRecord{}
}

func mustGet(h *harness, id string) state.DeployRecord {
	h.t.Helper()
	rec, err := h.store.GetDeployment(context.Background(), id)
	if err != nil {
		h.t.Fatalf("get deployment %s: %v", id, err)
	}
	return rec
}

// runningTaskIDs 返回指定镜像的运行中任务 ID 集合。
func (h *harness) runningTaskIDs(service, image string) map[string]bool {
	h.t.Helper()
	tasks, err := h.sub.TaskList(context.Background(), service)
	if err != nil {
		h.t.Fatalf("task list: %v", err)
	}
	out := map[string]bool{}
	for _, t := range tasks {
		if t.Image == image && t.State == "running" && t.DesiredState == "running" {
			out[t.ID] = true
		}
	}
	return out
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
