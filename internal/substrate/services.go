package substrate

// engine.Substrate 端口的 moby/client 实现（T2-5a 服务/任务面）：Swarm
// service 与 task 的读、幂等写与 swarm 就绪检查。第三方类型只在本包内部，
// 出口一律 engine 核心类型；错误归一为端口哨兵（engine.ErrServiceNotFound /
// engine.ErrNotSwarmReady / state.ErrObjectNotFound / state.ErrVersionConflict）。
//
// 受管字段纪律（release-semantics §2.8 / architecture §2.5，逐条固定）：
//   - UpdateConfig.FailureAction = pause（D-REL-1：Swarm 侧固定 pause，
//     平台是唯一回滚决策者）；
//   - UpdateConfig.Monitor = 5s（平台固定，只判定启动期失败，不放大）；
//   - ForceUpdate 恒不递增（归位重放零任务替换的前提，Spike B2；
//     --force 禁用是平台纪律）。
// 其余字段逐字来自 engine.ServiceSpec（order/parallelism/delay 的组合裁决
// 在引擎规划层，适配器不裁剪）。

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/imageregistry"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台受管常量（不暴露配置面；architecture §2.5 默认参数表）。
const (
	managedFailureAction = swarm.UpdateFailureActionPause
	managedMonitor       = 5 * time.Second
	// defaultRestartCondition / defaultRestartDelay 是重启策略缺省
	//（architecture §2.5 运行期语义：restart-condition=any、delay 5s）。
	defaultRestartCondition = swarm.RestartPolicyConditionAny
	defaultRestartDelay     = 5 * time.Second
)

// 编译期断言：Client 隐式实现 engine.Substrate 与 engine.ImageChecker 端口
// （第三方类型不出包的结构性证明）。
var (
	_ engine.Substrate    = (*Client)(nil)
	_ engine.ImageChecker = (*Client)(nil)
)

// ImageDigest 实现 engine.ImageChecker：返回用于 digest 钉定的不可变摘要。
// 次序（IMPL-T1-2/DT-2 设计冻结）：
//  1. 平台 registry 引用（`registry.<base>/apps/<app>@sha256:<hex>`，装配了
//     平台 registry 适配时）：部署前哨双模式的 registry 腿（D-MN-11）——
//     manifest HEAD 现场核验（build.PreflightRegistry 归一信封：registry
//     不可达 → E_REGISTRY_UNAVAILABLE 503；manifest 缺失 → ErrImageNotFound
//     家族，回滚前哨经 build.PreflightImage 归一 E_IMAGE_UNAVAILABLE）；
//     命中返回 manifest digest（既有语义不变）。
//  2. digest 钉定引用（含 `@sha256:` 与 tag@digest 叠加形态）：直通返回其
//     digest（免网络、免本机 inspect——swarm 逐节点按 digest 拉取）。
//  3. tag 引用：registry-first 解析（宿主命中平台设置 host 且凭证在位 →
//     携凭证；否则匿名）——公共/私有镜像免预拉；解析失败回落本机 inspect
//     （RepoDigests 清单摘要；airgap 不回归，回落留痕经装配缝）。
//  4. 本机 inspect 亦失败 → engine.ErrImageMissing 包装三因（image +
//     registry 原因 + 本机状态）；本机构建镜像（fleetly-local/…，无清单
//     摘要）解析腿跳过、inspect 返回空串由引擎按 tag 直用（旧语义）。
func (c *Client) ImageDigest(ctx context.Context, ref string) (string, error) {
	if c.platformRegistryEnabled() && build.IsRegistryImageRef(ref, c.registryHost) {
		res, err := build.PreflightRegistry(ctx, c, ref)
		if err != nil {
			return "", err
		}
		return res.Digest, nil
	}

	parsed, parseErr := imageregistry.Parse(ref)
	if parseErr == nil && parsed.Digest != "" {
		return parsed.Digest, nil
	}
	var registryErr error
	switch {
	case parseErr != nil:
		registryErr = fmt.Errorf("reference not resolvable via registry API: %w", parseErr)
	case strings.HasPrefix(ref, build.ImageRepoPrefix+"/"):
		// 平台本机构建产物（fleetly-local/…）：无 registry 身份——跳过
		// 解析腿，本机 inspect 语义零网络零行为变化（v0.1 本地面）。
		registryErr = nil
	default:
		digest, err := c.resolveTagDigestForImage(ctx, parsed)
		if err != nil {
			registryErr = err
		} else {
			return digest, nil
		}
	}

	localDigest, localErr := c.inspectLocalImageDigest(ctx, ref)
	if localErr == nil {
		c.traceImageDigestFallback(ref, registryErr)
		return localDigest, nil
	}
	if registryErr == nil {
		return "", localErr
	}
	return "", fmt.Errorf("%w: %s (registry resolution failed: %w; local image lookup failed: %w)",
		engine.ErrImageMissing, ref, registryErr, localErr)
}

// SwarmReady 确认本机为 active swarm manager：未 init / 非 manager 返回
// errors.Is(err, engine.ErrNotSwarmReady)（engine 据此映射
// E_RUNTIME_UNAVAILABLE + swarm init 建议）。
func (c *Client) SwarmReady(ctx context.Context) error {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	res, err := c.cli.Info(ctx, mobyclient.InfoOptions{})
	if err != nil {
		return fmt.Errorf("substrate: info: %w", err)
	}
	if res.Info.Swarm.NodeID == "" || res.Info.Swarm.LocalNodeState != swarm.LocalNodeStateActive {
		return fmt.Errorf("%w: swarm not initialized (the installer runs docker swarm init; run docker swarm init first on manual setups)", engine.ErrNotSwarmReady)
	}
	return nil
}

// NetworkEnsure 确认 overlay 网络存在（幂等：已有即 no-op；缺失创建——
// 每应用专属 overlay 网络，architecture §2.4 服务命名与网络行）。
func (c *Client) NetworkEnsure(ctx context.Context, name string) error {
	ictx, icancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	_, err := c.cli.NetworkInspect(ictx, name, mobyclient.NetworkInspectOptions{})
	icancel()
	if err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("substrate: network inspect %s: %w", name, err)
	}
	cctx, ccancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	_, cerr := c.cli.NetworkCreate(cctx, name, mobyclient.NetworkCreateOptions{
		Driver: "overlay",
		Labels: map[string]string{
			state.LabelManaged: state.ManagedLabelValue,
		},
	})
	ccancel()
	if cerr != nil {
		// 并发创建竞态：已存在即成功。
		rctx, rcancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
		_, ierr := c.cli.NetworkInspect(rctx, name, mobyclient.NetworkInspectOptions{})
		rcancel()
		if ierr == nil {
			return nil
		}
		return fmt.Errorf("substrate: network create %s: %w", name, cerr)
	}
	return nil
}

// buildSwarmSpec 把引擎核心 ServiceSpec 翻译为 swarm.ServiceSpec（第三方
// 类型不越过本函数）。secretIDs / configIDs 是「对象名 → 底座对象 ID」的
// 已解析引用集（EnsureSecret / EnsureConfig 确保在位后按名解析——
// SecretReference/ConfigReference 必须携带 ID 与完整 File UID/GID/Mode：
// W3 真机教训，留空会让 swarm agent 在任务启动期 strconv 解析空串直接
// 失败；internal/database 收敛器与 internal/metrics anchorSpec 同注）。
// 缺 ID 的声明 = 引擎未先行确保（对账层编码错误），显式报错。
func buildSwarmSpec(spec engine.ServiceSpec, secretIDs, configIDs map[string]string) (swarm.ServiceSpec, error) {
	container := &swarm.ContainerSpec{
		Image:  spec.Image,
		Labels: spec.ContainerLabels,
		Env:    spec.Env,
	}
	if len(spec.Command) > 0 {
		container.Command = spec.Command
	}
	// DT-5 平台加固字段（任务面服务端强制：CapDrop ALL / 只读 rootfs /
	// 非 root user / pids 限额；app 服务路径恒零值——compose 受控子集没有
	// 对应键，加固不进用户表达面）。
	if len(spec.Args) > 0 {
		container.Args = spec.Args
	}
	if spec.User != "" {
		container.User = spec.User
	}
	if spec.ReadOnlyRootfs {
		container.ReadOnly = true
	}
	if len(spec.CapDrop) > 0 {
		container.CapabilityDrop = append([]string{}, spec.CapDrop...)
	}
	if spec.StopSignal != "" {
		container.StopSignal = spec.StopSignal
	}
	if spec.StopGracePeriod > 0 {
		grace := spec.StopGracePeriod
		container.StopGracePeriod = &grace
	}
	if spec.Healthcheck != nil {
		container.Healthcheck = swarmHealthcheck(*spec.Healthcheck)
	}
	for _, m := range spec.Mounts {
		container.Mounts = append(container.Mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.VolumeName,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	for _, s := range spec.Secrets {
		id, ok := secretIDs[s.SecretName]
		if !ok {
			return swarm.ServiceSpec{}, fmt.Errorf("substrate: secret %s not ensured before service convergence (engine ordering bug)", s.SecretName)
		}
		container.Secrets = append(container.Secrets, &swarm.SecretReference{
			SecretID:   id,
			SecretName: s.SecretName,
			// UID/GID/Mode 显式置零值安全形态（0:0/0444——docker CLI 同款缺
			// 省；留空会让 swarm agent 启动期解析失败，W3 真机实证）。
			File: &swarm.SecretReferenceFileTarget{Name: s.Target, UID: "0", GID: "0", Mode: 0o444},
		})
	}
	for _, cfg := range spec.Configs {
		id, ok := configIDs[cfg.ConfigName]
		if !ok {
			return swarm.ServiceSpec{}, fmt.Errorf("substrate: config %s not ensured before service convergence (engine ordering bug)", cfg.ConfigName)
		}
		container.Configs = append(container.Configs, &swarm.ConfigReference{
			ConfigID:   id,
			ConfigName: cfg.ConfigName,
			// config 恒只读（swarm ConfigReference 无 ReadOnly 位）；UID/GID/
			// Mode 显式落 0:0/0444（docker CLI config 缺省形态；File 缺省在
			// swarm agent 启动期解析失败，secret 同族实证）。
			File: &swarm.ConfigReferenceFileTarget{Name: cfg.Target, UID: "0", GID: "0", Mode: 0o444},
		})
	}

	task := swarm.TaskSpec{
		ContainerSpec: container,
	}
	if len(spec.Networks) > 0 {
		for _, n := range spec.Networks {
			task.Networks = append(task.Networks, swarm.NetworkAttachmentConfig{
				Target:  n.Name,
				Aliases: n.Aliases,
			})
		}
	}
	if len(spec.Constraints) > 0 {
		task.Placement = &swarm.Placement{Constraints: spec.Constraints}
	}
	if spec.Resources != nil || spec.PidsLimit > 0 {
		limits := &swarm.Limit{}
		if spec.Resources != nil {
			limits.NanoCPUs = spec.Resources.NanoCPUs
			limits.MemoryBytes = spec.Resources.MemoryBytes
		}
		// pids 限额在 swarm API 的 Resources.Limits.Pids（DT-5 任务加固：
		// ContainerSpec 本体无该字段——真机 `docker service create
		// --limit-pids` 的等价落点）。
		limits.Pids = spec.PidsLimit
		task.Resources = &swarm.ResourceRequirements{Limits: limits}
	}
	rp := spec.RestartPolicy
	if rp == nil {
		rp = &engine.RestartPolicySpec{Condition: string(defaultRestartCondition), Delay: defaultRestartDelay}
	}
	policy := &swarm.RestartPolicy{Condition: swarm.RestartPolicyCondition(rp.Condition)}
	if rp.Delay > 0 {
		d := rp.Delay
		policy.Delay = &d
	}
	if rp.MaxAttempts > 0 {
		policy.MaxAttempts = &rp.MaxAttempts
	}
	if rp.Window > 0 {
		w := rp.Window
		policy.Window = &w
	}
	task.RestartPolicy = policy

	serviceSpec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   spec.Name,
			Labels: spec.ServiceLabels,
		},
		TaskTemplate: task,
	}
	if spec.Job {
		// 一次性 replicated-job（E5 Cron，架构 §4.3 执行行）：TotalCompletions=1
		// 的单发任务、MaxConcurrent=1（与平台重叠 skip〔max-concurrent 1〕
		// 同口径）；job 模式不接受 UpdateConfig（daemon 拒绝）——不写；重启
		// 策略缺省按 none（失败即 failed 终态，不重试；无 delay 残留）。
		one := uint64(1)
		serviceSpec.Mode = swarm.ServiceMode{
			ReplicatedJob: &swarm.ReplicatedJob{MaxConcurrent: &one, TotalCompletions: &one},
		}
		if spec.RestartPolicy == nil {
			serviceSpec.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone}
		}
	} else {
		// 长驻服务（global/replicated）：UpdateConfig 照常下发（受管字段
		// failure_action=pause/monitor=5s 由适配器固定）。
		if spec.Global {
			serviceSpec.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
		} else {
			replicas := spec.Replicas
			serviceSpec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}
		}
		serviceSpec.UpdateConfig = &swarm.UpdateConfig{
			Parallelism:   spec.UpdateParallelism,
			Delay:         spec.UpdateDelay,
			FailureAction: managedFailureAction,
			Monitor:       managedMonitor,
			Order:         swarm.UpdateOrder(spec.UpdateOrder),
		}
	}
	return serviceSpec, nil
}

// resolveSecretIDs 按名解析服务 spec 引用的 Swarm secret 对象 ID（服务
// create/update 前置：引擎已先行 EnsureSecret——此处缺失 = 引擎未确保或
// 对象被外部清理，如实报错不静默丢引用）。
func (c *Client) resolveSecretIDs(ctx context.Context, spec engine.ServiceSpec) (map[string]string, error) {
	if len(spec.Secrets) == 0 {
		return nil, nil
	}
	ids := make(map[string]string, len(spec.Secrets))
	for _, s := range spec.Secrets {
		if _, done := ids[s.SecretName]; done {
			continue
		}
		sctx, scancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
		res, err := c.cli.SecretInspect(sctx, s.SecretName, mobyclient.SecretInspectOptions{})
		scancel()
		if err != nil {
			return nil, fmt.Errorf("substrate: secret %s inspect: %w (the engine must ensure secrets before service convergence)", s.SecretName, err)
		}
		ids[s.SecretName] = res.Secret.ID
	}
	return ids, nil
}

// resolveConfigIDs 按名解析服务 spec 引用的 Swarm config 对象 ID（服务
// create/update 前置：引擎已先行 EnsureConfig——此处缺失 = 引擎未确保或
// 对象被外部清理，如实报错不静默丢引用）。
func (c *Client) resolveConfigIDs(ctx context.Context, spec engine.ServiceSpec) (map[string]string, error) {
	if len(spec.Configs) == 0 {
		return nil, nil
	}
	ids := make(map[string]string, len(spec.Configs))
	for _, cfg := range spec.Configs {
		if _, done := ids[cfg.ConfigName]; done {
			continue
		}
		cctx, ccancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
		res, err := c.cli.ConfigInspect(cctx, cfg.ConfigName, mobyclient.ConfigInspectOptions{})
		ccancel()
		if err != nil {
			return nil, fmt.Errorf("substrate: config %s inspect: %w (the engine must ensure configs before service convergence)", cfg.ConfigName, err)
		}
		ids[cfg.ConfigName] = res.Config.ID
	}
	return ids, nil
}

// swarmHealthcheck 翻译健康检查（平台缺省已由引擎规划层补齐）。
func swarmHealthcheck(hc engine.HealthcheckSpec) *container.HealthConfig {
	out := &container.HealthConfig{
		Test:     hc.Test,
		Interval: hc.Interval,
		Timeout:  hc.Timeout,
		Retries:  int(min(hc.Retries, math.MaxInt32)),
	}
	if hc.StartPeriod > 0 {
		out.StartPeriod = hc.StartPeriod
	}
	return out
}

// ServiceCreate 实现 engine.Substrate：创建服务（调用方对账保证仅缺失时
// 调用；已存在不覆盖——按 ErrObjectConflict 语义失败暴露竞态）。平台
// registry 引用的镜像附带 X-Registry-Auth（--with-registry-auth 语义，
// E1-5：Swarm 把凭据分发到拉取节点）；其余镜像零凭据面。
func (c *Client) ServiceCreate(ctx context.Context, spec engine.ServiceSpec) error {
	secretIDs, err := c.resolveSecretIDs(ctx, spec)
	if err != nil {
		return err
	}
	configIDs, err := c.resolveConfigIDs(ctx, spec)
	if err != nil {
		return err
	}
	sw, err := buildSwarmSpec(spec, secretIDs, configIDs)
	if err != nil {
		return err
	}
	opts := mobyclient.ServiceCreateOptions{Spec: sw}
	auth, err := c.registryAuthForImage(spec.Image)
	if err != nil {
		return err
	}
	opts.EncodedRegistryAuth = auth
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	if _, err := c.cli.ServiceCreate(ctx, opts); err != nil {
		return fmt.Errorf("substrate: service create %s: %w", spec.Name, err)
	}
	return nil
}

// serviceUpdateRetry 是乐观令牌冲突的短重试预算（适配器内部取版本 +
// 更新；引擎不做跨调用令牌传递）。
const serviceUpdateRetry = 3

// ServiceUpdate 实现 engine.Substrate：读当前版本 → 以目标 spec 推进。
// ForceUpdate 恒不递增（归位零成本，Spike B2）。并发冲突短重试。平台
// registry 引用的镜像附带 X-Registry-Auth（与 ServiceCreate 同语义，E1-5
// ——回滚/重放路径的 service update 同样要能把凭据交给拉取节点）。
func (c *Client) ServiceUpdate(ctx context.Context, name string, spec engine.ServiceSpec) error {
	secretIDs, err := c.resolveSecretIDs(ctx, spec)
	if err != nil {
		return err
	}
	configIDs, err := c.resolveConfigIDs(ctx, spec)
	if err != nil {
		return err
	}
	sw, err := buildSwarmSpec(spec, secretIDs, configIDs)
	if err != nil {
		return err
	}
	auth, err := c.registryAuthForImage(spec.Image)
	if err != nil {
		return err
	}
	var lastErr error
	for i := 0; i < serviceUpdateRetry; i++ {
		ictx, icancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
		res, err := c.cli.ServiceInspect(ictx, name, mobyclient.ServiceInspectOptions{})
		icancel()
		if err != nil {
			return mapSubstrateErr(fmt.Errorf("substrate: service inspect %s: %w", name, err))
		}
		uctx, ucancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
		_, uerr := c.cli.ServiceUpdate(uctx, name, mobyclient.ServiceUpdateOptions{
			Version:             swarm.Version{Index: res.Service.Version.Index},
			Spec:                sw,
			EncodedRegistryAuth: auth,
		})
		ucancel()
		if uerr != nil {
			lastErr = uerr
			if errdefs.IsConflict(uerr) {
				continue // 版本令牌被并发写推进：重读重试
			}
			return mapSubstrateErr(fmt.Errorf("substrate: service update %s: %w", name, uerr))
		}
		return nil
	}
	return mapSubstrateErr(fmt.Errorf("substrate: service update %s: %w", name, lastErr))
}

// ServiceRemove 实现 engine.Substrate：删除服务（省略=删除；幂等——缺失
// 视为成功，对账重放的常见形态）。
func (c *Client) ServiceRemove(ctx context.Context, name string) error {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	if _, err := c.cli.ServiceRemove(ctx, name, mobyclient.ServiceRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("substrate: service remove %s: %w", name, err)
	}
	return nil
}

// ServiceInspect 实现 engine.Substrate：按名取服务实况投影。
func (c *Client) ServiceInspect(ctx context.Context, name string) (engine.ServiceState, error) {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	res, err := c.cli.ServiceInspect(ctx, name, mobyclient.ServiceInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return engine.ServiceState{}, fmt.Errorf("%w: %s", engine.ErrServiceNotFound, name)
		}
		return engine.ServiceState{}, fmt.Errorf("substrate: service inspect %s: %w", name, err)
	}
	return serviceToState(res.Service), nil
}

// ServiceList 实现 engine.Substrate：按 label 选择器返回服务投影。
func (c *Client) ServiceList(ctx context.Context, labels map[string]string) ([]engine.ServiceState, error) {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	filters := mobyclient.Filters{}
	for k, v := range labels {
		filters = filters.Add("label", k+"="+v)
	}
	res, err := c.cli.ServiceList(ctx, mobyclient.ServiceListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("substrate: service list: %w", err)
	}
	out := make([]engine.ServiceState, 0, len(res.Items))
	for _, svc := range res.Items {
		out = append(out, serviceToState(svc))
	}
	return out, nil
}

// TaskList 实现 engine.Substrate：返回服务的全部任务（service ps 语义）。
func (c *Client) TaskList(ctx context.Context, serviceName string) ([]engine.TaskState, error) {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	res, err := c.cli.TaskList(ctx, mobyclient.TaskListOptions{
		Filters: mobyclient.Filters{}.Add("service", serviceName),
	})
	if err != nil {
		return nil, fmt.Errorf("substrate: task list %s: %w", serviceName, err)
	}
	out := make([]engine.TaskState, 0, len(res.Items))
	for _, t := range res.Items {
		item := engine.TaskState{
			ID:           t.ID,
			Slot:         t.Slot,
			State:        string(t.Status.State),
			DesiredState: string(t.DesiredState),
			Err:          t.Status.Err,
			Timestamp:    t.Status.Timestamp,
		}
		if t.Spec.ContainerSpec != nil {
			item.Image = t.Spec.ContainerSpec.Image
		}
		out = append(out, item)
	}
	return out, nil
}

// serviceToState 把 swarm.Service 投影为核心类型（UpdateStatus 逐字镜像：
// paused + Message 是更新失败的平台判定来源，Spike B）。T2.13 起同时抄出
// spec 侧受控子集字段（T2.13 运行域漂移反解的实况侧输入；第三方类型不出
// 本函数，出口一律 engine 核心类型）。
func serviceToState(svc swarm.Service) engine.ServiceState {
	out := engine.ServiceState{
		Name:    svc.Spec.Name,
		Version: svc.Version.Index,
		Labels:  svc.Spec.Labels,
	}
	task := svc.Spec.TaskTemplate
	if task.ContainerSpec != nil {
		c := task.ContainerSpec
		out.Image = c.Image
		out.Command = append([]string{}, c.Command...)
		out.Args = append([]string{}, c.Args...)
		out.User = c.User
		out.ReadOnlyRootfs = c.ReadOnly
		out.CapDrop = append([]string{}, c.CapabilityDrop...)
		out.Env = append([]string{}, c.Env...)
		out.ContainerLabels = c.Labels
		out.StopSignal = c.StopSignal
		if c.StopGracePeriod != nil {
			out.StopGracePeriod = *c.StopGracePeriod
		}
		if c.Healthcheck != nil {
			out.Healthcheck = &engine.HealthcheckSpec{
				Test:        append([]string{}, c.Healthcheck.Test...),
				Interval:    c.Healthcheck.Interval,
				Timeout:     c.Healthcheck.Timeout,
				Retries:     uint64(max(c.Healthcheck.Retries, 0)),
				StartPeriod: c.Healthcheck.StartPeriod,
			}
		}
		for _, m := range c.Mounts {
			if m.Type != mount.TypeVolume {
				continue // 受控子集只写命名卷；其余形态不进漂移投影
			}
			out.Mounts = append(out.Mounts, engine.MountSpec{
				VolumeName: m.Source, Target: m.Target, ReadOnly: m.ReadOnly,
			})
		}
		for _, s := range c.Secrets {
			target := ""
			if s.File != nil {
				target = s.File.Name
			}
			out.Secrets = append(out.Secrets, engine.SecretMount{SecretName: s.SecretName, Target: target})
		}
		for _, cfg := range c.Configs {
			target := ""
			if cfg.File != nil {
				target = cfg.File.Name
			}
			out.Configs = append(out.Configs, engine.ConfigMount{ConfigName: cfg.ConfigName, Target: target})
		}
	}
	for _, n := range task.Networks {
		out.Networks = append(out.Networks, engine.NetworkAttach{
			Name: n.Target, Aliases: append([]string{}, n.Aliases...),
		})
	}
	if task.Placement != nil {
		out.Constraints = append([]string{}, task.Placement.Constraints...)
	}
	if task.Resources != nil && task.Resources.Limits != nil {
		out.Resources = &engine.ResourcesSpec{
			NanoCPUs:    task.Resources.Limits.NanoCPUs,
			MemoryBytes: task.Resources.Limits.MemoryBytes,
		}
		out.PidsLimit = task.Resources.Limits.Pids
	}
	if task.RestartPolicy != nil {
		rp := &engine.RestartPolicySpec{Condition: string(task.RestartPolicy.Condition)}
		if task.RestartPolicy.Delay != nil {
			rp.Delay = *task.RestartPolicy.Delay
		}
		if task.RestartPolicy.MaxAttempts != nil {
			rp.MaxAttempts = *task.RestartPolicy.MaxAttempts
		}
		if task.RestartPolicy.Window != nil {
			rp.Window = *task.RestartPolicy.Window
		}
		out.RestartPolicy = rp
	}
	out.Global = svc.Spec.Mode.Global != nil
	if svc.Spec.Mode.Replicated != nil && svc.Spec.Mode.Replicated.Replicas != nil {
		out.Replicas = *svc.Spec.Mode.Replicated.Replicas
	}
	// A8（S18）：Spec.UpdateConfig 补抄进漂移反解投影——order/parallelism/
	// delay 三字段进漂移哈希（外部 docker service update --update-* 篡改
	// 的判定面）；FailureAction 是平台受管字段（本适配器恒写 pause），读回
	// 非 pause 即受管字段被篡改，引擎侧专报。
	if svc.Spec.UpdateConfig != nil {
		out.UpdateOrder = string(svc.Spec.UpdateConfig.Order)
		out.UpdateParallelism = svc.Spec.UpdateConfig.Parallelism
		out.UpdateDelay = svc.Spec.UpdateConfig.Delay
		out.UpdateFailureAction = string(svc.Spec.UpdateConfig.FailureAction)
	}
	if svc.UpdateStatus != nil {
		out.UpdateState = string(svc.UpdateStatus.State)
		out.UpdateMessage = svc.UpdateStatus.Message
	}
	if v, ok := out.Labels[state.LabelDesiredHash]; ok {
		out.DesiredHash = v
	}
	return out
}
