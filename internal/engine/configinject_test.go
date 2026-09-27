package engine

// config 挂载面测试（T 线 OT-3 / IMPL-T1-4）：preflight E_CONFIG_NOT_FOUND
// （缺失点名 + plan-time fail-fast，守卫①引擎侧）、ConfigMount 进服务 spec
// （内容寻址名 + 显式 target）、底座 ensure 收到明文内容（装载链面）、
// 内容变更 → 新对象 + 服务滚动 + 旧对象回收（守卫②）、快照重放对内容
// 寻址名的现值解析（内容换版 → 悬空名诚实失败）、init job 投影同源、
// app 删除 reap 全清；配置内容零出现在事件载荷。

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// fakeConfigEnsure 是 ConfigEnsurer 端口的测试替身（记录 ensure 调用的
// 名字与内容——内容只在本替身内存，断言后即弃）。
type fakeConfigEnsure struct {
	ensured map[string][]byte
	labels  map[string]map[string]string
	failOn  string
	// onEnsure 在成功 ensure 后回调（测试可把它桥到假 substrate 的对象
	// 清册——GC 读面看到真实「ensure 创建、换版留存」的对象集）。
	onEnsure func(name string)
}

func newFakeConfigEnsure() *fakeConfigEnsure {
	return &fakeConfigEnsure{ensured: map[string][]byte{}, labels: map[string]map[string]string{}}
}

func (f *fakeConfigEnsure) EnsureConfig(_ context.Context, name string, data []byte, labels map[string]string) (string, error) {
	if f.failOn == name {
		return "", errors.New("simulated substrate failure")
	}
	f.ensured[name] = data
	f.labels[name] = labels
	if f.onEnsure != nil {
		f.onEnsure(name)
	}
	return "cid-" + name, nil
}

// fakeConfigReap 是 ConfigReaper 端口的内存假实现（按 label 等值过滤，记录
// 移除序）。
type fakeConfigReap struct {
	mu      sync.Mutex
	configs map[string]map[string]string // name → labels
	removed []string
}

func newFakeConfigReap() *fakeConfigReap {
	return &fakeConfigReap{configs: map[string]map[string]string{}}
}

func (f *fakeConfigReap) ConfigList(_ context.Context, labels map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name, lbs := range f.configs {
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

func (f *fakeConfigReap) ConfigRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.configs, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeConfigReap) add(name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[name] = labels
}

// configCompose 是 external config 部署 fixture：web 与 worker 引用同一
// source（去重对照）、每服务显式绝对 target（长语法）。
const configCompose = `name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
    configs: [{ source: app_conf, target: /etc/demo/app.yaml }]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
  worker:
    image: alpine:3
    command: ["sleep", "infinity"]
    configs:
      - source: app_conf
        target: /etc/demo/worker.yaml
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
configs:
  app_conf: { external: true }
`

// mustSetAppConfig 落一条平台配置资源（hash8 由测试侧按 naming.Hash8 计算）。
func mustSetAppConfig(t *testing.T, h *harness, appID, name, value string) string {
	t.Helper()
	if _, err := h.store.UpsertAppConfig(context.Background(), state.AppConfig{
		AppID: appID,
		Name:  name,
		Value: value,
		Hash8: naming.Hash8(value),
	}); err != nil {
		t.Fatalf("upsert app config: %v", err)
	}
	return naming.Hash8(value)
}

// demoConfigPrefix 推导 demo 应用的 Swarm config 名前缀（naming.ConfigName
// 去掉 hash8 尾缀）。
func (h *harness) demoConfigPrefix(name string) string {
	h.t.Helper()
	app := h.demoApp()
	out, err := naming.ConfigName(app.TeamSlug, app.ProjectSlug, app.Name, name, strings.Repeat("0", 8))
	if err != nil {
		h.t.Fatalf("config name: %v", err)
	}
	return strings.TrimSuffix(out, strings.Repeat("0", 8))
}

// configTaskIDs 取指定服务当前任务 ID 集合（滚动断言辅助）。
func configTaskIDs(h *harness, service string) []string {
	h.sub.mu.Lock()
	defer h.sub.mu.Unlock()
	svc, ok := h.sub.services[service]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(svc.tasks))
	for _, task := range svc.tasks {
		out = append(out, task.ID)
	}
	sort.Strings(out)
	return out
}

// TestDeployWithExternalConfig 主链路：external config 声明 → spec 挂载
// （内容寻址名 + 显式绝对 target）→ 底座 ensure 收到明文内容 → 双服务
// 引用去重（单次 ensure）→ 内容零进事件载荷。
func TestDeployWithExternalConfig(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeConfigEnsure()
	h.eng.WithConfigEnsurer(ensure)

	const configValue = "listen: 8080\nlog_level: info\n"
	app, err := testsupport.SeedAppE(t, h.store, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	hash8 := mustSetAppConfig(t, h, app.ID, "app_conf", configValue)

	final := h.runToTerminal(h.enqueue(h.writeCompose(configCompose)))
	if final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}

	wantName := h.demoConfigPrefix("app_conf") + hash8
	web := h.sub.services[h.svc("web")].spec
	if len(web.Configs) != 1 {
		t.Fatalf("web configs = %+v, want exactly one mount", web.Configs)
	}
	if web.Configs[0].ConfigName != wantName {
		t.Errorf("web config name = %q, want %q (content-addressed contract)", web.Configs[0].ConfigName, wantName)
	}
	if web.Configs[0].Target != "/etc/demo/app.yaml" {
		t.Errorf("web config target = %q, want the explicit absolute path", web.Configs[0].Target)
	}
	worker := h.sub.services[h.svc("worker")].spec
	if len(worker.Configs) != 1 || worker.Configs[0].Target != "/etc/demo/worker.yaml" {
		t.Fatalf("worker config mount = %+v, want the explicit long-syntax target", worker.Configs)
	}

	// 底座 ensure：内容 = app_configs 明文；双服务引用去重一次。
	if data, ok := ensure.ensured[wantName]; !ok {
		t.Fatalf("swarm config %s was not ensured", wantName)
	} else if string(data) != configValue {
		t.Errorf("ensured content mismatch (len %d vs %d)", len(data), len(configValue))
	}
	if len(ensure.ensured) != 1 {
		t.Errorf("ensure calls = %d, want 1 (same source deduped across services)", len(ensure.ensured))
	}
	demo := h.demoApp()
	if lbl := ensure.labels[wantName]; lbl[state.LabelApp] != demo.QualifiedName() || lbl[state.LabelManaged] != state.ManagedLabelValue {
		t.Errorf("ensure labels = %+v, want managed + app=%s ownership anchors", lbl, demo.QualifiedName())
	}

	// 内容零出现：事件载荷（快照只带对象名；desired_spec 密文不在断言面）。
	for name, payload := range eventPayloads(t, h) {
		if strings.Contains(payload, "listen: 8080") {
			t.Errorf("event %s payload contains the config content", name)
		}
	}
}

// TestDeployConfigMissingPreflight 守卫①引擎侧：声明名不在 app_configs →
// E_CONFIG_NOT_FOUND（422）点名缺失名；底座零 ensure、零服务写。
func TestDeployConfigMissingPreflight(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeConfigEnsure()
	h.eng.WithConfigEnsurer(ensure)
	testsupport.SeedApp(t, h.store, "demo")

	final := h.runToTerminal(h.enqueue(h.writeCompose(configCompose)))
	if final.Status != state.DeployFailed {
		t.Fatalf("deploy = %s, want failed (preflight fail-fast)", final.Status)
	}
	if final.ErrorCode != "E_CONFIG_NOT_FOUND" {
		t.Fatalf("error code = %s, want E_CONFIG_NOT_FOUND", final.ErrorCode)
	}
	payload := eventPayloads(t, h)["deployment.failed"]
	if !strings.Contains(payload, "app_conf") {
		t.Fatalf("failure disclosure must name the missing config: %s", payload)
	}
	if len(ensure.ensured) != 0 {
		t.Errorf("substrate must not be touched on preflight failure, saw %d ensures", len(ensure.ensured))
	}
	if len(h.sub.services) != 0 {
		t.Errorf("services created on preflight failure: %v", serviceNames(h.sub))
	}
}

// TestConfigContentChangeCreatesNewObjectAndRollsService 守卫②：内容变更 →
// 新 config 对象（内容寻址名变化）+ 服务滚动（spec 快照 + 任务替换）+ 旧
// 对象回收（keep-set 之外的旧版清场，当前版保留）。
func TestConfigContentChangeCreatesNewObjectAndRollsService(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeConfigEnsure()
	reap := newFakeConfigReap()
	h.eng.WithConfigEnsurer(ensure).WithConfigReaper(reap)
	// ensure 创建的对象进入假 substrate 清册（GC 读面与真实底座同构）。
	ensure.onEnsure = func(name string) { reap.add(name, configLabels(h.demoApp().QualifiedName())) }

	app, err := testsupport.SeedAppE(t, h.store, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	hashA := mustSetAppConfig(t, h, app.ID, "app_conf", "content-v1")
	composePath := h.writeCompose(configCompose)

	if final := h.runToTerminal(h.enqueue(composePath)); final.Status != state.DeploySucceeded {
		t.Fatalf("first deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	nameA := h.demoConfigPrefix("app_conf") + hashA
	firstTasks := configTaskIDs(h, h.svc("web"))
	if len(firstTasks) == 0 {
		t.Fatal("first deploy left no tasks")
	}

	// 内容换版 + 重部署（compose 文件不变——期望态差异来自 config 内容寻址名）。
	hashB := mustSetAppConfig(t, h, app.ID, "app_conf", "content-v2")
	if hashB == hashA {
		t.Fatal("content change did not move the fingerprint")
	}
	if final := h.runToTerminal(h.enqueue(composePath)); final.Status != state.DeploySucceeded {
		t.Fatalf("second deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	nameB := h.demoConfigPrefix("app_conf") + hashB

	// 新对象确保在位 + 服务 spec 指向新名（滚动由 spec 变更驱动）。
	if _, ok := ensure.ensured[nameB]; !ok {
		t.Fatalf("rotated swarm config %s was not ensured", nameB)
	}
	web := h.sub.services[h.svc("web")].spec
	if len(web.Configs) != 1 || web.Configs[0].ConfigName != nameB {
		t.Fatalf("web configs after rotation = %+v, want %s", web.Configs, nameB)
	}
	// 服务滚动：假底座把 config 引用变更视为任务替换（真实 swarm 同语义）。
	secondTasks := configTaskIDs(h, h.svc("web"))
	if strings.Join(firstTasks, ",") == strings.Join(secondTasks, ",") {
		t.Fatalf("config rotation did not roll the service (tasks unchanged: %v)", secondTasks)
	}

	// 旧对象回收：A 被清场、B 保留。
	reap.mu.Lock()
	removed := append([]string(nil), reap.removed...)
	reap.mu.Unlock()
	if len(removed) != 1 || removed[0] != nameA {
		t.Fatalf("gc removed = %v, want exactly the stale object %s", removed, nameA)
	}
	reap.mu.Lock()
	_, keptB := reap.configs[nameB]
	reap.mu.Unlock()
	if !keptB {
		t.Fatal("current config object was removed by gc (keep-set broken)")
	}
}

// TestSnapshotConfigResolution 快照重放（回滚/归位/init 续跑共用）：挂载名
// 对 app_configs 现值解析——内容匹配 → ensure 且通过；内容换版 → 名悬空 →
// E_CONFIG_NOT_FOUND 诚实失败（不静默改写）。
func TestSnapshotConfigResolution(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeConfigEnsure()
	h.eng.WithConfigEnsurer(ensure)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, h.store, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	hash8 := mustSetAppConfig(t, h, app.ID, "app_conf", "value-v1-AAA")

	specs := []ServiceSpec{{
		Name:  h.svc("web"),
		Image: "alpine:3@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Configs: []ConfigMount{{
			ConfigName: h.demoConfigPrefix("app_conf") + hash8,
			Target:     "/etc/demo/app.yaml",
		}},
	}}
	rec := state.DeployRecord{ID: "dep-1", AppID: app.ID, AppName: "demo"}

	// 匹配：preflight 通过 + ensure 在位。
	if err := h.eng.preflightRollback(ctx, rec, specs); err != nil {
		t.Fatalf("preflight with matching config: %v", err)
	}
	if _, ok := ensure.ensured[h.demoConfigPrefix("app_conf")+hash8]; !ok {
		t.Fatal("replay path did not ensure the swarm config")
	}

	// 内容换版：名变 → 快照挂载名悬空 → E_CONFIG_NOT_FOUND。
	mustSetAppConfig(t, h, app.ID, "app_conf", "value-v2-BBB")
	err = h.eng.preflightRollback(ctx, rec, specs)
	if err == nil {
		t.Fatal("preflight with rotated-away config must fail")
	}
	ae, ok := asAppErrEnvelope(err)
	if !ok || ae.Code() != "E_CONFIG_NOT_FOUND" {
		t.Fatalf("error = %v, want E_CONFIG_NOT_FOUND envelope", err)
	}
}

// TestInitJobConfigProjectionSharedSource init job 与长驻服务同一投影链
// （T1-3 审查第 4 条落地断言）：job 模板与 web 携带同一 ConfigMount，底座
// ensure 单次（跨模板去重）。
func TestInitJobConfigProjectionSharedSource(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeConfigEnsure()
	h.eng.WithConfigEnsurer(ensure)
	ctx := context.Background()

	app, err := testsupport.SeedAppE(t, h.store, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	hash8 := mustSetAppConfig(t, h, app.ID, "app_conf", "migrations: 001\n")
	yaml := `name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
    configs: [{ source: app_conf, target: /etc/demo/app.yaml }]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
  migrate:
    image: alpine:3
    command: ["true"]
    configs: [{ source: app_conf, target: /etc/demo/bootstrap.sql }]
    labels:
      fleetly.job: init
configs:
  app_conf: { external: true }
`
	rec := h.enqueue(h.writeCompose(yaml))
	// 推进到 init 相位首拍（job 服务已在位）。
	h.eng.Tick(ctx)
	row := mustGet(h, rec.ID)
	if row.Status != state.DeployReleasing || row.Phase != state.PhaseInitJobs {
		t.Fatalf("row = %s/%s, want releasing/init_jobs", row.Status, row.Phase)
	}
	jobName := h.initJobServiceName(row, "migrate")
	job, ok := h.sub.services[jobName]
	if !ok {
		t.Fatalf("init job service %s not created (have %v)", jobName, serviceNames(h.sub))
	}
	wantName := h.demoConfigPrefix("app_conf") + hash8
	if len(job.spec.Configs) != 1 || job.spec.Configs[0].ConfigName != wantName {
		t.Fatalf("init job configs = %+v, want the shared content-addressed mount %s", job.spec.Configs, wantName)
	}
	if job.spec.Configs[0].Target != "/etc/demo/bootstrap.sql" {
		t.Fatalf("init job config target = %q, want the job's own explicit target", job.spec.Configs[0].Target)
	}
	if len(ensure.ensured) != 1 {
		t.Fatalf("ensure calls = %d, want 1 (job + long-running share the source)", len(ensure.ensured))
	}
}

// TestReapDeletingAppsSweepsAppConfigs 删除扫尾：app 归属 config 对象随
// tombstone 第二拍全清；他 app 对象不受牵连（best-effort，不阻塞删除）。
func TestReapDeletingAppsSweepsAppConfigs(t *testing.T) {
	h := deletingAppWithServices(t)
	ctx := context.Background()
	reap := newFakeConfigReap()
	reap.add(h.demoConfigPrefix("app_conf")+"1a2b3c4d", configLabels(h.demoApp().QualifiedName()))
	reap.add("fleetly-other-app-config-k-11223344", map[string]string{
		"fleetly.managed": "true",
		"fleetly.app":     "other/app",
	})
	h.eng.WithConfigReaper(reap)

	h.eng.ReapDeletingApps(ctx)

	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle = %s, want deleted", got)
	}
	reap.mu.Lock()
	removed := append([]string(nil), reap.removed...)
	left := len(reap.configs)
	reap.mu.Unlock()
	if len(removed) != 1 || !strings.Contains(removed[0], "demo") {
		t.Fatalf("removed configs = %v, want exactly the demo-app object", removed)
	}
	if left != 1 {
		t.Fatalf("%d config objects remain, want the other-app object untouched", left)
	}
}

// TestReapDeletingAppsConfigSweepNotWired 端口未接线：删除照常收敛
// （best-effort 纪律——扫尾缺席不阻塞 tombstone 第二拍）。
func TestReapDeletingAppsConfigSweepNotWired(t *testing.T) {
	h := deletingAppWithServices(t)
	h.eng.ReapDeletingApps(context.Background())
	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle = %s, want deleted (sweep absence must not block deletion)", got)
	}
}

// TestDriftDetectsConfigReferenceTamper 运行域漂移：实况 config 引用被外部
// 改写（docker service update --config-*）→ 投影差异可见（configs 进漂移
// 哈希与字段级 diff）；平台自身部署的形态零误报。
func TestDriftDetectsConfigReferenceTamper(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeConfigEnsure()
	h.eng.WithConfigEnsurer(ensure)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, h.store, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	mustSetAppConfig(t, h, app.ID, "app_conf", "listen: 8080\n")
	if final := h.runToTerminal(h.enqueue(h.writeCompose(configCompose))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}

	report, err := h.eng.DriftShow(ctx, "demo")
	if err != nil {
		t.Fatalf("DriftShow: %v", err)
	}
	if report.Drifted {
		t.Fatalf("freshly deployed config mount reported as drift: %+v", report.Services)
	}

	// 外部篡改：实况 config 引用换成不存在平台的旧形态（绕过平台写语义）。
	h.sub.mutateExternal(h.svc("web"), func(spec *ServiceSpec) {
		spec.Configs = []ConfigMount{{ConfigName: "fleetly-demo-config-app_conf-deadbeef", Target: "/etc/demo/app.yaml"}}
	})
	report, err = h.eng.DriftShow(ctx, "demo")
	if err != nil {
		t.Fatalf("DriftShow after tamper: %v", err)
	}
	if !report.Drifted {
		t.Fatal("externally rewritten config reference not detected as drift")
	}
	found := false
	for _, svc := range report.Services {
		for _, f := range svc.Diff {
			if f.Field == "configs" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("drift report missing the configs field diff: %+v", report.Services)
	}
}
