package engine

// 引擎单测的假底座/假放置/假时钟/假镜像检查器：以最小 Swarm 行为模型
// （创建/更新即收敛、pause 判定、PENDING 滞留、运行中任务注入崩溃）覆盖
// 状态机与失败语义的判定路径。时间由 fakeClock 控制（看门狗/观察窗判定）。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// fakeClock 是可控时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeImages 是可控镜像可见性。registryErrs 命中的 ref 返回 registry 前哨
// 信封（E_REGISTRY_UNAVAILABLE——substrate 适配器对平台 registry 引用的
// 真实行为同构，D-MN-11）；failOther 命中的 ref 返回普通传输类失败（既有
// E_RUNTIME_UNAVAILABLE 包装路径的对照），供 resolveImage 透传测试。
type fakeImages struct {
	missing      map[string]bool
	registryErrs map[string]bool
	failOther    map[string]bool
}

func (f *fakeImages) ImageDigest(_ context.Context, ref string) (string, error) {
	if f.registryErrs[ref] {
		return "", apperr.New("E_REGISTRY_UNAVAILABLE",
			"platform registry did not answer while checking %s (deploy preflight fails fast; no queueing)", ref).
			WithStage("preflight")
	}
	if f.failOther[ref] {
		return "", context.DeadlineExceeded
	}
	if f.missing[ref] {
		return "", ErrImageMissing
	}
	return "sha256:digest-" + ref, nil
}

// updateMode 是一次服务更新/创建的底座行为模型。
type updateMode string

const (
	modeHealthy      updateMode = "healthy"       // 新任务立即运行、更新完成（切流）
	modePausedHealth updateMode = "paused_health" // 新任务健康门失败 → paused
	modePausedStart  updateMode = "paused_start"  // 新任务启动即崩 → paused
	modePending      updateMode = "pending"       // 新任务滞留 PENDING
)

// fakeService 是一只受管服务的假实况。
type fakeService struct {
	spec    ServiceSpec
	version uint64
	update  string
	message string
	tasks   []TaskState
	// nextMode 是下一次 update 应表现的行为。
	nextMode updateMode
	// noTaskChurn 断言辅助：记录每次 update 前的任务 ID 集合。
	created bool
	// failureAction 是 UpdateConfig.FailureAction 的实况镜像（默认平台
	// 受管值 pause；A8 测试经 mutateUpdateFailureAction 注入外部篡改）。
	failureAction string
}

// fakeSubstrate 是 Substrate 端口的内存实现。
type fakeSubstrate struct {
	mu       sync.Mutex
	services map[string]*fakeService
	networks map[string]bool
	removed  []string
	// updates 记录每次 ServiceUpdate 的（服务, 镜像）——归位重放断言用。
	updates [][2]string
	// pendingModes 是服务创建/下一次更新应表现的行为（可先于服务存在设置）。
	pendingModes map[string]updateMode
	failUpdates  map[string]error
	// panicOn 注入 TaskList panic（A9 tick 隔离测试：命中服务名即 panic，
	// 模拟单条毒记录引发的引擎推进 panic）。
	panicOn map[string]bool
	// panicServiceList 注入 ServiceList panic（MG-5 测试：driftScan →
	// computeAppDrift → ServiceList 的真实毒点——Run 栈上的 drift 分支
	// 原无 recover）。
	panicServiceList bool
	// failServiceListErr 注入 ServiceList 瞬态错误（H10/MG-3 测试：deleting
	// 回收 duty 的底座瞬态重试路径——非 nil 即返回错误，模拟 dockerd 短暂
	// 不可达）。
	failServiceListErr error
	// failInspectErr 注入 ServiceInspect 瞬态错误（T0-V2.2 存在性对账测试：
	// 非 nil 即返回——模拟 dockerd 超时/不可达；对账 duty 必须把它与服务
	// 缺失区分，不得发事件、不得改派生态）。
	failInspectErr error
	// serviceListCalls 是 ServiceList 的调用计数（H10/MG-3 频控断言：duty
	// 时间闸内的拍子不应触达底座）。
	serviceListCalls int
	// inspectCalls 是 ServiceInspect 的调用计数（T0-V2.2 存在性对账频控
	// 断言：时间闸内的拍子不应触达底座——serviceListCalls 同模式）。
	inspectCalls int
	// swarmErr 是 SwarmReady 的错误注入（底座不可达）：ErrNotSwarmReady =
	// 暂态（引擎记警告后按各路径语义重试/推进）；其他错误 = 硬失败。
	swarmErr error
}

func newFakeSubstrate() *fakeSubstrate {
	return &fakeSubstrate{
		services:     map[string]*fakeService{},
		networks:     map[string]bool{},
		pendingModes: map[string]updateMode{},
		failUpdates:  map[string]error{},
		panicOn:      map[string]bool{},
	}
}

// serviceUpdateCalls 返回 ServiceUpdate 的累计调用次数（收敛幂等断言面：
// 与 updates 明细同源——每次 ServiceUpdate 恰好记一条）。
func (f *fakeSubstrate) serviceUpdateCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updates)
}

func (f *fakeSubstrate) SwarmReady(context.Context) error { return f.swarmErr }

func (f *fakeSubstrate) NetworkEnsure(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = true
	return nil
}

func (f *fakeSubstrate) ServiceInspect(_ context.Context, name string) (ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectCalls++
	if f.failInspectErr != nil {
		return ServiceState{}, f.failInspectErr
	}
	svc, ok := f.services[name]
	if !ok {
		return ServiceState{}, ErrServiceNotFound
	}
	return f.stateOf(svc), nil
}

func (f *fakeSubstrate) stateOf(svc *fakeService) ServiceState {
	failureAction := svc.failureAction
	if failureAction == "" {
		failureAction = "pause" // 平台受管缺省（适配器恒写 pause）
	}
	out := ServiceState{
		Name:          svc.spec.Name,
		Version:       svc.version,
		Labels:        map[string]string{},
		Image:         svc.spec.Image,
		Replicas:      globalReplicasOf(svc.spec),
		UpdateState:   svc.update,
		UpdateMessage: svc.message,
		// spec 侧深投影（真实适配器同构：漂移反解的实况侧输入。C3 契约腿
		// 钉住字段集——Args/User/ReadOnlyRootfs/CapDrop/PidsLimit 曾在 fake
		// 侧漏投影〔真适配器 serviceToState 全读回〕，2026-09-29 补齐）。
		Command:         append([]string{}, svc.spec.Command...),
		Args:            append([]string{}, svc.spec.Args...),
		User:            svc.spec.User,
		ReadOnlyRootfs:  svc.spec.ReadOnlyRootfs,
		CapDrop:         append([]string{}, svc.spec.CapDrop...),
		PidsLimit:       svc.spec.PidsLimit,
		Env:             append([]string{}, svc.spec.Env...),
		ContainerLabels: svc.spec.ContainerLabels,
		Global:          svc.spec.Global,
		Networks:        append([]NetworkAttach{}, svc.spec.Networks...),
		Mounts:          append([]MountSpec{}, svc.spec.Mounts...),
		Secrets:         append([]SecretMount{}, svc.spec.Secrets...),
		Configs:         append([]ConfigMount{}, svc.spec.Configs...),
		Healthcheck:     svc.spec.Healthcheck,
		RestartPolicy:   svc.spec.RestartPolicy,
		Resources:       svc.spec.Resources,
		Constraints:     append([]string{}, svc.spec.Constraints...),
		StopSignal:      svc.spec.StopSignal,
		StopGracePeriod: svc.spec.StopGracePeriod,
		// A8（S18）：UpdateConfig 投影补齐（真实适配器同构）。
		UpdateOrder:         svc.spec.UpdateOrder,
		UpdateParallelism:   svc.spec.UpdateParallelism,
		UpdateDelay:         svc.spec.UpdateDelay,
		UpdateFailureAction: failureAction,
	}
	for k, v := range svc.spec.ServiceLabels {
		out.Labels[k] = v
	}
	out.DesiredHash = svc.spec.ServiceLabels[state.LabelDesiredHash]
	return out
}

// globalReplicasOf 是实况投影的副本语义（与真实适配器 serviceToState 同构，
// M1-3 测试引入）：Swarm global 服务的 Mode.Replicated 为 nil → Replicas
// 读回 0（spec 侧写的期望副本不参与 global 实况）；replicated 照抄。
func globalReplicasOf(spec ServiceSpec) uint64 {
	if spec.Global {
		return 0
	}
	return spec.Replicas
}

// sameConfigMounts 比较 config 引用集（顺序敏感——期望序权威；config 引用
// 变化 = task template 变化 = 服务滚动，OT-3/IMPL-T1-4 真机实证）。
func sameConfigMounts(a, b []ConfigMount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameNetworkAttachments 比较网络接入集（名称 + 别名集合；网络集合变化 =
// task template 变化 = 服务滚动，IMPL-T15-1 真机实证——增/摘网必然重建任务）。
func sameNetworkAttachments(a, b []NetworkAttach) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || len(a[i].Aliases) != len(b[i].Aliases) {
			return false
		}
		for j := range a[i].Aliases {
			if a[i].Aliases[j] != b[i].Aliases[j] {
				return false
			}
		}
	}
	return true
}

// mutateExternal 模拟外部操作（手动 docker service update --env 等）：绕过
// 平台写语义直接改写运行服务形态并推进对象版本（不触碰 desired-hash
// label——外部改动不清理平台簿记，正是漂移检测要抓的形态）。
func (f *fakeSubstrate) mutateExternal(service string, mutate func(spec *ServiceSpec)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	svc, ok := f.services[service]
	if !ok {
		return
	}
	mutate(&svc.spec)
	svc.version++
}

// mutateUpdateFailureAction 模拟外部篡改平台受管字段（docker service
// update --update-failure-action rollback；A8 专报测试）——绕过平台写
// 语义（适配器恒写 pause），只改实况镜像。
func (f *fakeSubstrate) mutateUpdateFailureAction(service, action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if svc, ok := f.services[service]; ok {
		svc.failureAction = action
		svc.version++
	}
}

// setExternalTasks 整组重写服务任务实况（T0-V2.2 F11 对账测试：模拟节点
// drain / 任务被外部停掉后的任务面——期望副本不变、旧任务 shutdown、新
// 任务滞留 pending；与 mutateExternal 同族的「绕过平台写语义」注入缝）。
func (f *fakeSubstrate) setExternalTasks(service string, tasks []TaskState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if svc, ok := f.services[service]; ok {
		svc.tasks = append([]TaskState{}, tasks...)
	}
}

// panicOnTaskList 注入 TaskList panic（A9 测试）。
func (f *fakeSubstrate) panicOnTaskList(service string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panicOn[service] = true
}

func (f *fakeSubstrate) ServiceCreate(_ context.Context, spec ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failUpdates[spec.Name]; err != nil {
		return err
	}
	svc := &fakeService{spec: spec, version: 1, nextMode: modeHealthy}
	svc.created = true
	f.applyMode(svc, "", nil)
	delete(f.pendingModes, spec.Name)
	f.services[spec.Name] = svc
	return nil
}

func (f *fakeSubstrate) ServiceUpdate(_ context.Context, name string, spec ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failUpdates[name]; err != nil {
		return err
	}
	svc, ok := f.services[name]
	if !ok {
		// 更新一只不存在的服务：按创建收敛（幂等对账语义）。
		svc = &fakeService{version: 0, nextMode: modeHealthy}
		f.services[name] = svc
	}
	svc.version++
	f.updates = append(f.updates, [2]string{name, spec.Image})
	oldImage := svc.spec.Image
	oldTasks := append([]TaskState{}, svc.tasks...)
	// 判定是否有实质变更（镜像 / config 引用 / 网络集合变化才进入行为模型
	// ——真实 swarm 的 task template 变更即触发滚动：config 引用与网络集合
	// 都是模板的一部分〔IMPL-T1-4/T15-1 真机实证〕；纯 label 更新零任务
	// 变动——归位零成本的假底座同构，Spike B2）。
	imageChanged := oldImage != spec.Image
	configsChanged := !sameConfigMounts(svc.spec.Configs, spec.Configs)
	networksChanged := !sameNetworkAttachments(svc.spec.Networks, spec.Networks)
	svc.spec = spec
	if !imageChanged && !configsChanged && !networksChanged {
		svc.update = "completed"
		return nil
	}
	if (configsChanged || networksChanged) && !imageChanged {
		// 同镜像、config/网络集合变更：真实 swarm 以 task template 变更重建
		// 任务（同镜像也替换；IMPL-T1-4/T15-1 真机探针实证），更新即完成。
		svc.update = "completed"
		svc.message = ""
		svc.tasks = f.runningTasks(svc, "t-template")
		return nil
	}
	f.applyMode(svc, oldImage, oldTasks)
	return nil
}

// applyMode 依据 nextMode 生成任务与更新状态（oldTasks = 变更前的任务集，
// pause 冻结语义下旧版本任务继续服务）。pendingModes 为一次性消费。
func (f *fakeSubstrate) applyMode(svc *fakeService, oldImage string, oldTasks []TaskState) {
	mode, ok := f.pendingModes[svc.spec.Name]
	if ok {
		delete(f.pendingModes, svc.spec.Name)
		svc.nextMode = mode
	}
	replicas := 0
	if svc.spec.Replicas < 1024 { // 受控子集副本量级有限（gosec G115 窄化守卫）
		replicas = int(svc.spec.Replicas)
	}
	if svc.spec.Global {
		replicas = 1
	}
	runningOld := 0
	for _, t := range oldTasks {
		if t.State == "running" && t.DesiredState == "running" &&
			(oldImage == "" || t.Image == oldImage) {
			runningOld++
		}
	}
	switch svc.nextMode {
	case modeHealthy:
		svc.update = "completed"
		svc.message = ""
		// IsTaskDirty 同构：目标版本运行任务已满足副本水位 → 零任务替换
		//（同内容重放，Spike B2）。
		runningNew := 0
		for _, t := range oldTasks {
			if t.State == "running" && t.DesiredState == "running" && t.Image == svc.spec.Image {
				runningNew++
			}
		}
		if runningNew < replicas {
			svc.tasks = f.runningTasks(svc, "t-new")
		} else {
			svc.tasks = oldTasks
		}
	case modePausedHealth:
		// pause 冻结：旧任务继续服务、新任务带健康失败原因退出。
		svc.update = "paused"
		svc.message = "task t-new-1 container: Health check: command failed"
		svc.tasks = append([]TaskState{{
			ID: "t-new-1", State: "failed", DesiredState: "running",
			Err:   "task: non-zero exit (1): Health check: command failed",
			Image: svc.spec.Image,
		}}, runningOf(oldTasks)...)
	case modePausedStart:
		// pause 冻结：新任务启动即崩（无健康语义）。
		svc.update = "paused"
		svc.message = "task t-new-1 container exited"
		svc.tasks = append([]TaskState{{
			ID: "t-new-1", State: "failed", DesiredState: "running",
			Err:   "task: non-zero exit (1)",
			Image: svc.spec.Image,
		}}, runningOf(oldTasks)...)
	case modePending:
		svc.update = "updating"
		svc.tasks = []TaskState{{ID: "t-new-1", State: "pending", DesiredState: "running",
			Image: svc.spec.Image}}
	}
}

// runningOf 取旧任务集中仍在运行的（pause 冻结：旧版本继续服务）。
func runningOf(tasks []TaskState) []TaskState {
	var out []TaskState
	for _, t := range tasks {
		if t.State == "running" && t.DesiredState == "running" {
			out = append(out, t)
		}
	}
	return out
}

// runningTasks 生成 replicas 个运行中的目标版本任务。
func (f *fakeSubstrate) runningTasks(svc *fakeService, prefix string) []TaskState {
	n := int(min(svc.spec.Replicas, math.MaxInt32)) //nolint:gosec // 测试替身，受控值
	if svc.spec.Global {
		n = 1
	}
	if n == 0 {
		n = 0
	}
	out := make([]TaskState, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, TaskState{
			ID:           fmt.Sprintf("%s-%d", prefix, i),
			Slot:         i + 1,
			State:        "running",
			DesiredState: "running",
			Image:        svc.spec.Image,
		})
	}
	return out
}

func (f *fakeSubstrate) ServiceRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.services, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeSubstrate) ServiceList(_ context.Context, labels map[string]string) ([]ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicServiceList {
		panic("injected substrate panic (MG-5 driftScan isolation test)")
	}
	if f.failServiceListErr != nil {
		return nil, f.failServiceListErr
	}
	f.serviceListCalls++
	names := make([]string, 0, len(f.services))
	for name := range f.services {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []ServiceState
	for _, name := range names {
		svc := f.services[name]
		if labels[state.LabelManaged] != "" && svc.spec.ServiceLabels[state.LabelManaged] != labels[state.LabelManaged] {
			continue
		}
		if labels[state.LabelApp] != "" && svc.spec.ServiceLabels[state.LabelApp] != labels[state.LabelApp] {
			continue
		}
		out = append(out, f.stateOf(svc))
	}
	return out, nil
}

func (f *fakeSubstrate) TaskList(_ context.Context, serviceName string) ([]TaskState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicOn[serviceName] {
		panic("injected substrate panic (A9 tick isolation test)")
	}
	svc, ok := f.services[serviceName]
	if !ok {
		return nil, nil
	}
	out := make([]TaskState, len(svc.tasks))
	copy(out, svc.tasks)
	return out, nil
}

// setMode 配置某服务下一次更新/创建的行为（可先于服务存在设置——首发
// 失败路径在 ServiceCreate 时消费）。
func (f *fakeSubstrate) setMode(service string, mode updateMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingModes[service] = mode
	if svc, ok := f.services[service]; ok {
		svc.nextMode = mode
	}
}

// crashNewRunning 追加 n 条目标版本的失败任务记录（崩溃退出注入；重复
// 调用模拟崩溃循环——退出计数累加，运行任务保留表示 Swarm 重启自愈）。
func (f *fakeSubstrate) crashNewRunning(service string, n int, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	svc := f.services[service]
	for i := 0; i < n; i++ {
		svc.tasks = append(svc.tasks, TaskState{
			ID:           fmt.Sprintf("t-crash-%d-%d", at.UnixNano(), i),
			State:        "failed",
			DesiredState: "running",
			Image:        svc.spec.Image,
			Timestamp:    at,
		})
	}
}

// fakeResolver 是 PlacementResolver 的假实现（签名对齐 placement 真实类型；
// Preflight 错误序列驱动 blocked_waiting / node_gone / 前哨失败路径；Apply
// 同构真实放置层——卷注册表登记）。
type fakeResolver struct {
	store *state.Store
	// preflightErrs 依次弹出（nil = 通过）；弹尽后恒 nil。
	preflightErrs []error
	// persistentPreflightFail 是「持续模式」开关（MG-2 回归测试引入）：非 nil
	// 时 Preflight 恒返回该错误——模拟绑定节点持续 DOWN 多拍越过看门狗
	// deadline 的形态（一次性队列会立即转入恢复拍，掩盖看门狗暂停缺陷）。
	persistentPreflightFail error
	applyCalls              int
}

func (f *fakeResolver) Resolve(_ context.Context, in placement.Input) (placement.Decision, error) {
	return placement.Decision{}, nil
}

func (f *fakeResolver) Apply(ctx context.Context, in placement.Input) (placement.Decision, error) {
	f.applyCalls++
	// 卷登记（真实放置层 Apply 的最小同构：新卷 = 数据诞生点登记）。
	for _, m := range in.Volumes {
		if _, _, err := f.store.RegisterAppVolume(ctx, state.VolumeWrite{
			AppID:          in.AppID,
			Key:            m.Key,
			Name:           "fleetly-" + in.AppName + "-" + m.Key + "-test",
			Kind:           state.VolumeKindNamed,
			PlatformNodeID: "n_test000000000000000000",
			MountPath:      m.Target,
		}); err != nil {
			return placement.Decision{}, err
		}
	}
	if len(in.Volumes) > 0 {
		return placement.Decision{AppID: in.AppID, Bind: true,
			PlatformNodeID: "n_test000000000000000000"}, nil
	}
	return placement.Decision{AppID: in.AppID}, nil
}

func (f *fakeResolver) Preflight(context.Context, string) error {
	// 持续模式优先（不受一次性队列消费影响；清空即恢复）。
	if f.persistentPreflightFail != nil {
		return f.persistentPreflightFail
	}
	if len(f.preflightErrs) == 0 {
		return nil
	}
	err := f.preflightErrs[0]
	f.preflightErrs = f.preflightErrs[1:]
	return err
}

// fakeNetwork 是一只假网络对象的实况（label 集 + 挂接/引用计数）。
type fakeNetwork struct {
	labels     map[string]string
	containers int
	services   int
	// internal 是 internal 变体位（DT-5/DT-7：task-group 不可信隔离面）。
	internal bool
}

// fakeNetworkSubstrate 是 NetworkSubstrate 端口的内存实现（IMPL-T15-1 项目网
// 生命周期与对账测试）：ensure 幂等（已存在不覆盖 label——真实 daemon 同
// 语义）、list 按 managed label 过滤（**列表投影不含端点计数**——真实
// daemon 语义，GC 判据走 Inspect）、inspect 带 containers/services、
// remove 幂等记录。
type fakeNetworkSubstrate struct {
	mu   sync.Mutex
	nets map[string]*fakeNetwork
	// failListErr 注入 NetworkList 瞬态错误（读错不结论断言面）。
	failListErr error
	// failInspectErr 注入 NetworkInspect 瞬态错误（GC 读错不动作断言面）。
	failInspectErr error
	// ensures 记录 ensure 调用序列（收敛 duty 断言面）。
	ensures []string
	// removes 记录 remove 调用序列（回收断言面）。
	removes []string
}

func newFakeNetworkSubstrate() *fakeNetworkSubstrate {
	return &fakeNetworkSubstrate{nets: map[string]*fakeNetwork{}}
}

func (f *fakeNetworkSubstrate) NetworkEnsureWithLabels(_ context.Context, name string, labels map[string]string) error {
	return f.NetworkEnsureWithOptions(context.Background(), name, labels, false)
}

// NetworkEnsureWithOptions 实现带 internal 变体的 ensure（DT-5/DT-7 任务面
// 假底座：变体错配与真适配器同语义——显式失败）。
func (f *fakeNetworkSubstrate) NetworkEnsureWithOptions(_ context.Context, name string, labels map[string]string, internal bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, name)
	if net, ok := f.nets[name]; ok {
		if net.internal != internal {
			return fmt.Errorf("fake substrate: network %s exists with internal=%t but internal=%t was requested", name, net.internal, internal)
		}
		return nil // 已存在不覆盖（真实 daemon 语义）
	}
	copied := make(map[string]string, len(labels))
	for k, v := range labels {
		copied[k] = v
	}
	f.nets[name] = &fakeNetwork{labels: copied, internal: internal}
	return nil
}

func (f *fakeNetworkSubstrate) NetworkList(_ context.Context, labels map[string]string) ([]NetworkState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failListErr != nil {
		return nil, f.failListErr
	}
	names := make([]string, 0, len(f.nets))
	for name := range f.nets {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []NetworkState
	for _, name := range names {
		net := f.nets[name]
		match := true
		for k, v := range labels {
			if net.labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// 列表投影：Containers/Services 归零（真实 daemon 语义）。
		out = append(out, NetworkState{Name: name, Labels: copyLabels(net.labels), Driver: "overlay"})
	}
	return out, nil
}

func (f *fakeNetworkSubstrate) NetworkInspect(_ context.Context, name string) (NetworkState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInspectErr != nil {
		return NetworkState{}, f.failInspectErr
	}
	net, ok := f.nets[name]
	if !ok {
		return NetworkState{}, ErrNetworkNotFound
	}
	return NetworkState{
		Name: name, Labels: copyLabels(net.labels), Driver: "overlay",
		Internal:   net.internal,
		Containers: net.containers, Services: net.services,
	}, nil
}

func (f *fakeNetworkSubstrate) NetworkRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, name)
	delete(f.nets, name)
	return nil
}

// injectNetwork 注入一只网络对象（孤儿/归因测试的播种面）。
func (f *fakeNetworkSubstrate) injectNetwork(name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nets[name] = &fakeNetwork{labels: copyLabels(labels)}
}

// injectOrphan 注入一只受管但无法归因的平台前缀网（守卫②的孤儿形态）。
func (f *fakeNetworkSubstrate) injectOrphan(name string) {
	f.injectNetwork(name, map[string]string{state.LabelManaged: state.ManagedLabelValue})
}

// setEndpoints 设置网络的挂接容器/引用服务计数（GC 安全性判据注入面）。
func (f *fakeNetworkSubstrate) setEndpoints(name string, containers, services int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if net, ok := f.nets[name]; ok {
		net.containers = containers
		net.services = services
	}
}

// has 报告网络是否存在。
func (f *fakeNetworkSubstrate) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.nets[name]
	return ok
}

// removeCount 返回 remove 调用次数（回收断言面）。
func (f *fakeNetworkSubstrate) removeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removes)
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// 编译期断言：假底座/假解析器满足引擎端口。
var (
	_ Substrate         = (*fakeSubstrate)(nil)
	_ PlacementResolver = (*fakeResolver)(nil)
	_ ImageChecker      = (*fakeImages)(nil)
	_ Clock             = (*fakeClock)(nil)
	_ NetworkSubstrate  = (*fakeNetworkSubstrate)(nil)
)
