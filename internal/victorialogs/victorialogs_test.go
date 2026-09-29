package victorialogs

// duty 收敛流程的 hermetic 单测（fake dockerPort——rustfs duty 测试同型）：
// 缺省即部署（V2-1 默认捆绑语义——logs.backend 未设置 = victorialogs）、
// 幂等稳态、漂移更新、切回 jsonl 移除服务保留卷、差分事件、负面
//（swarm 未就绪、平台 ID 未铸）、健康检查面。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// fakeDocker 是 dockerPort 的假件（收敛账面记录 + 幂等填充）。
type fakeDocker struct {
	mu sync.Mutex

	swarmActive bool

	services map[string]ServiceState
	created  []string
	updated  []string
	removed  []string

	volumesEnsured []string

	inspectErr error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{services: map[string]ServiceState{}}
}

func (f *fakeDocker) Info(_ context.Context) (bool, error) { return f.swarmActive, nil }

func (f *fakeDocker) ServiceInspect(_ context.Context, name string) (ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return ServiceState{}, f.inspectErr
	}
	return f.services[name], nil
}

func (f *fakeDocker) ServiceCreate(_ context.Context, spec swarm.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, spec.Name)
	cur := ServiceState{Exists: true, Version: 1}
	cur.fillFrom(spec)
	cur.normalizeNetworkIDs()
	f.services[spec.Name] = cur
	return nil
}

func (f *fakeDocker) ServiceUpdate(_ context.Context, name string, _ uint64, spec swarm.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, name)
	cur := f.services[name]
	cur.Version++
	cur.fillFrom(spec)
	cur.normalizeNetworkIDs()
	f.services[name] = cur
	return nil
}

// normalizeNetworkIDs 模拟 engine 创建期行为：网络挂载目标按名归一为网络
// ID 存储（"host" 亦然——W5-S3 真机/dind 实证）。duty 的幂等比对必须经
// NetworkName 解析回名同锚比较（见 converge 注记）。
func (s *ServiceState) normalizeNetworkIDs() {
	for i, t := range s.Networks {
		s.Networks[i] = "netid:" + t
	}
}

// NetworkName 把 ID 形态目标解析回名（realDockerClient 同语义的假件；
// 非 ID 形态原样透传——容错对齐 NetworkInspect 的 by-name 命中）。
func (f *fakeDocker) NetworkName(_ context.Context, target string) (string, error) {
	if name, ok := strings.CutPrefix(target, "netid:"); ok {
		return name, nil
	}
	return target, nil
}

func (f *fakeDocker) ServiceRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	delete(f.services, name)
	return nil
}

func (f *fakeDocker) VolumeEnsure(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumesEnsured = append(f.volumesEnsured, name)
	return nil
}

// eventsSinceZero 抓取事件（测试辅助）。
func eventsSinceZero(t *testing.T, st *state.Store) []state.Event {
	t.Helper()
	evs, err := st.EventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	return evs
}

// harness 是收敛测试的公共装配（真 state.Store + fake dockerPort）。
type harness struct {
	t   *testing.T
	st  *state.Store
	fk  *fakeDocker
	mgr *Manager
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InTx(context.Background(), func(tx *state.Tx) error {
		return tx.SetMeta(context.Background(), state.MetaKeyPlatformNodeID, "n_TESTNODEID01")
	}); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	fk := newFakeDocker()
	fk.swarmActive = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManagerWithDocker(st, 7, fk, logger)
	return &harness{t: t, st: st, fk: fk, mgr: mgr}
}

// setBackend 保存 logs.backend（SaveLogsSettings 同事务审计+事件路径）。
func (h *harness) setBackend(backend string) {
	h.t.Helper()
	if err := h.st.SaveLogsSettings(context.Background(), backend, state.LogsSaveOptions{Actor: "system"}); err != nil {
		h.t.Fatalf("save logs.backend %s: %v", backend, err)
	}
}

// eventsOf 名字过滤事件（测试辅助）。
func (h *harness) eventsOf(name string) []state.Event {
	h.t.Helper()
	var out []state.Event
	for _, ev := range eventsSinceZero(h.t, h.st) {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	return out
}

// TestEnsureDeployedOnDefault 缺省即部署（V2-1）：未显式设置过
// logs.backend 的库 = victorialogs 生效 → 首拍创建服务 + deployed 事件；
// 第二拍稳态零写（幂等——无 churn）。
func TestEnsureDeployedOnDefault(t *testing.T) {
	h := newHarness(t)
	// 不保存任何设置：LoadLogsSettings 返回缺省 victorialogs（Set=false）。
	in, err := h.st.LoadLogsSettings(context.Background())
	if err != nil {
		t.Fatalf("load default: %v", err)
	}
	if in.Backend != state.LogsBackendVictorialogs || in.Set {
		t.Fatalf("default settings = %+v, want victorialogs unset", in)
	}

	oc, err := h.mgr.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	if oc != outcomeDeployed {
		t.Fatalf("outcome = %v, want deployed", oc)
	}
	if len(h.fk.created) != 1 || h.fk.created[0] != ServiceName {
		t.Fatalf("created = %v, want [%s]", h.fk.created, ServiceName)
	}
	if len(h.fk.volumesEnsured) != 1 || h.fk.volumesEnsured[0] != VolumeName {
		t.Fatalf("volumes = %v, want [%s]", h.fk.volumesEnsured, VolumeName)
	}
	evs := h.eventsOf("logs.victorialogs_deployed")
	if len(evs) != 1 || !strings.Contains(evs[0].Payload, `"reason":"created"`) {
		t.Fatalf("deployed events = %+v, want one created", evs)
	}

	// 第二拍：稳态零写。
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if len(h.fk.created) != 1 || len(h.fk.updated) != 0 {
		t.Fatalf("steady state churn: created=%v updated=%v", h.fk.created, h.fk.updated)
	}
	if len(h.eventsOf("logs.victorialogs_deployed")) != 1 {
		t.Fatal("steady state re-emitted deployed event")
	}
}

// TestEnsureUpdatesOnDrift 漂移更新：实况偏离期望 spec（镜像/参数/限额）
// → 下一拍收敛更新 + deployed(updated) 事件。
func TestEnsureUpdatesOnDrift(t *testing.T) {
	h := newHarness(t)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	// 外部漂移：镜像被手改。
	cur := h.fk.services[ServiceName]
	cur.Image = "victoriametrics/victoria-logs:latest"
	h.fk.services[ServiceName] = cur

	oc, err := h.mgr.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if oc != outcomeDeployed {
		t.Fatalf("outcome = %v, want deployed", oc)
	}
	if len(h.fk.updated) != 1 {
		t.Fatalf("updated = %v, want one drift update", h.fk.updated)
	}
	evs := h.eventsOf("logs.victorialogs_deployed")
	if len(evs) != 2 || !strings.Contains(evs[1].Payload, `"reason":"updated"`) {
		t.Fatalf("deployed events = %+v, want second updated", evs)
	}
}

// TestEnsureRemovesOnJSONL 切回 jsonl：服务移除 + removed 事件（payload
// 带 volume_retained=true）；卷对象永不被 duty 删除（数据安全语义）；
// 稳态幂等（无服务时 removeIfPresent no-op）。
func TestEnsureRemovesOnJSONL(t *testing.T) {
	h := newHarness(t)
	h.setBackend(state.LogsBackendVictorialogs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure deploy: %v", err)
	}
	h.setBackend(state.LogsBackendJSONL)

	oc, err := h.mgr.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure remove: %v", err)
	}
	if oc != outcomeIdle {
		t.Fatalf("outcome = %v, want idle", oc)
	}
	if len(h.fk.removed) != 1 || h.fk.services[ServiceName].Exists {
		t.Fatalf("removed = %v services=%+v, want service removed", h.fk.removed, h.fk.services)
	}
	evs := h.eventsOf("logs.victorialogs_removed")
	if len(evs) != 1 || !strings.Contains(evs[0].Payload, "volume_retained") {
		t.Fatalf("removed events = %+v, want one with volume_retained", evs)
	}
	// 数据卷不删：VolumeRemove 不在端口面上（编译期保证——fake 无该方法）。
	// 再一拍：无服务 → 零动作。
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure idle: %v", err)
	}
	if len(h.fk.removed) != 1 {
		t.Fatalf("idle churn: removed=%v", h.fk.removed)
	}
}

// TestEnsureSwarmNotReady 负面：swarm 未就绪 → ErrNotSwarmReady（可重试
// 态，Run 循环退避重试）。
func TestEnsureSwarmNotReady(t *testing.T) {
	h := newHarness(t)
	h.fk.swarmActive = false
	if _, err := h.mgr.Ensure(context.Background()); !errors.Is(err, ErrNotSwarmReady) {
		t.Fatalf("Ensure err = %v, want ErrNotSwarmReady", err)
	}
}

// TestEnsureMissingPlatformID 负面：平台 ID 未铸（identity duty 未跑）→
// 显式错误退避重试，宁缺毋错（约束引用空 ID = 永不调度）。
func TestEnsureMissingPlatformID(t *testing.T) {
	h := newHarness(t)
	if err := h.st.InTx(context.Background(), func(tx *state.Tx) error {
		return tx.SetMeta(context.Background(), state.MetaKeyPlatformNodeID, "")
	}); err != nil {
		t.Fatalf("clear meta: %v", err)
	}
	_, err := h.mgr.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "platform node id") {
		t.Fatalf("Ensure err = %v, want missing platform node id", err)
	}
}

// TestCheckHealth 健康检查面：jsonl = 无所欠恒绿；victorialogs + 服务
// 缺失 = 红（收敛过渡态如实表达）；健康拨测失败 = 红（检索降级诚实面）。
func TestCheckHealth(t *testing.T) {
	h := newHarness(t)
	h.setBackend(state.LogsBackendJSONL)
	if err := h.mgr.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth(jsonl) = %v, want nil", err)
	}

	h.setBackend(state.LogsBackendVictorialogs)
	if err := h.mgr.CheckHealth(); err == nil || !strings.Contains(err.Error(), "not deployed yet") {
		t.Fatalf("CheckHealth(missing service) = %v, want converging red", err)
	}

	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// 健康拨测未装配（nil）= 部署在位即健康。
	if err := h.mgr.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth(deployed, no probe) = %v, want nil", err)
	}
	// 健康拨测失败 = 红。
	h.mgr.WithHealth(func(_ context.Context) error { return errors.New("connection refused") })
	if err := h.mgr.CheckHealth(); err == nil || !strings.Contains(err.Error(), "health probe failed") {
		t.Fatalf("CheckHealth(probe fail) = %v, want red", err)
	}
	// 拨测恢复 = 绿。
	h.mgr.WithHealth(func(_ context.Context) error { return nil })
	if err := h.mgr.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth(probe ok) = %v, want nil", err)
	}
}

// TestDeploymentStatus 视图面：在位/缺失投影。
func TestDeploymentStatus(t *testing.T) {
	h := newHarness(t)
	st, err := h.mgr.DeploymentStatus(context.Background())
	if err != nil || st.Exists {
		t.Fatalf("status = %+v err=%v, want not exists", st, err)
	}
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	st, err = h.mgr.DeploymentStatus(context.Background())
	if err != nil || !st.Exists || st.Image != DefaultVictoriaLogsImage {
		t.Fatalf("status = %+v err=%v, want deployed with pinned image", st, err)
	}
}
