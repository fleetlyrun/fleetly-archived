package engine

// 规划层（期望态构建）：归一化 compose + 放置裁决 + env 三层合并 + 镜像
// digest + 卷注册表 → []ServiceSpec 与期望态哈希。受管字段与平台缺省在此
// 固定（release-semantics §2.8）：
//   - failure_action=pause / monitor=5s（适配器固定补齐，本层不管）；
//   - order：compose 声明照用；有卷 / global 强制 stop-first（显式
//     start-first 的冲突已在校验层拒绝）；
//   - healthcheck 子字段缺省 5s/3s/3/10s；无 healthcheck = health_gate=none
//     （W_DEPLOY_NO_HEALTHCHECK 由 compose.Load 警告承载）；
//   - restart_policy 缺省 condition=any / delay=5s；
//   - parallelism 缺省 1（swarm 语义 0=不限流，必须显式落 1）。

import (
	"sort"
	"time"

	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/envlayer"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台缺省健康检查参数（release-semantics §2.8 治理参数表：未写子字段取
// 平台默认 5s/3s/3/10s）。
const (
	defaultHealthInterval    = 5 * time.Second
	defaultHealthTimeout     = 3 * time.Second
	defaultHealthRetries     = 3
	defaultHealthStartPeriod = 10 * time.Second

	// defaultParallelism 是更新并行度缺省（compose 缺省 1；swarm 0=无限）。
	defaultParallelism = uint64(1)
)

// PlanInput 是一次期望态规划的输入（preparing 阶段收集齐）。
type PlanInput struct {
	AppID   string
	AppName string
	// TeamSlug / PrjSlug 是归属两个 slug（v0.3 三段命名公式的参数——
	// rbac-teams §4.3；从 app 行 join projects/teams 反解，slug 不可变）。
	TeamSlug string
	PrjSlug  string
	// DeploymentID 是发布归属（服务 label fleetly.deployment）。
	DeploymentID string
	// Spec 是归一化 compose（daemon 侧重载，二次校验防御）。
	Spec *compose.Spec
	// FileEnv / ComposeEnv 是 services.* 的文件层明文（extractServiceEnvs）。
	FileEnv    map[string]map[string]string
	ComposeEnv map[string]map[string]string
	// PlatformEnv 是已生效（effective）平台 env（三层合并的第三层输入；
	// pending 不参与合并——「随下次部署生效」由部署成功后的 promote 承载）。
	PlatformEnv []envlayer.PlatformVar
	// SystemEnv 是 system 层 env 的按服务注入面（E3-4：fleetly.s3=true
	// 服务的 S3 凭证组；键缺省 = 该服务无注入面）。system > platform >
	// 文件层；随三层合并参与快照与 desired-hash（脱敏形态）。
	SystemEnv map[string][]envlayer.PlatformVar
	// AttachRustfsNetwork 是 rustfs 模式网络牵线开关（E3-4：带 fleetly.s3
	// label 的服务附加 fleetly-rustfs-net；external 模式 false——应用自行
	// 出网）。
	AttachRustfsNetwork bool
	// DBNetworks 是库引用的网络牵线面（E4 managed-databases §2.4：带
	// fleetly.databases label 的服务 → 引用实例的共享网络名列表，无别名
	// ——以 Swarm 服务名可达；desired-hash 含网络）。列表按键名字典序
	//（resolveDatabaseReferences 产出顺序确定）。
	DBNetworks map[string][]string
	// SecretMounts 是服务级 secret 挂载面（E4 managed-databases §2.7：带
	// compose secrets 声明的服务 → 已解析的 SecretMount 列表——声明名在
	// app_secrets 缺失时 resolveSecretMounts 已在 preparing 期
	// E_SECRET_NOT_FOUND 拒绝，此处只做纯装配，键缺省 = 该服务无挂载）。
	// 按 target 字典序（desired-hash 确定性）。
	SecretMounts map[string][]SecretMount
	// ConfigMounts 是服务级 config 挂载面（OT-3/IMPL-T1-4：带 compose
	// configs 声明的服务 → 已解析的 ConfigMount 列表——声明名在 app_configs
	// 缺失时 resolveConfigMounts 已在 preparing 期 E_CONFIG_NOT_FOUND 拒绝；
	// 底座对象已 ensure；键缺省 = 该服务无挂载）。按 target 字典序
	//（desired-hash 确定性）。
	ConfigMounts map[string][]ConfigMount
	// ProjectNetwork 是项目网投影（OT-1/IMPL-T15-1）：app 参与位在位时
	// 非空 = 项目网 overlay 名（naming.ProjectNetworkName）；非空时每个
	// 成员服务在 app 私网之外**双挂**项目网，别名 = <app>-<service>
	//（naming.ProjectNetworkAlias；短名仅 app 私网）。空 = 不参加（缺省，
	// 既有行为零变化）。投影进 spec.Networks ⇒ 进 desired-hash 与快照
	// ⇒ attach/detach 由下一次重部署滚动收敛。
	ProjectNetwork string
	// Images 是服务 → digest 钉定镜像引用（building 阶段产出）。
	Images map[string]string
	// Decision 是放置裁决（绑定约束编译结果）。
	Decision placement.Decision
	Volumes  []state.Volume
}

// Plan 是一次期望态规划产出。
type Plan struct {
	// Services 按服务名字典序（对账与快照确定性）。
	Services []ServiceSpec
	// InitJobs 是 init job 模板（DT-4；Job=true 且 InitJob=true，按服务名
	// 字典序）：发布管线在晋级前按此集合创建一次性 job——执行形态与快照
	// 同源（重启续跑从 rec.DesiredSpec 现读同一形态）。
	InitJobs []ServiceSpec
	// EnvSnapshotHash 是 env 三层合并结果快照哈希（key:sha256+来源；
	// 值明文永不进哈希输入）。
	EnvSnapshotHash string
	// DesiredHash 是部署级期望态哈希（spec + env + 各服务哈希合成）。
	DesiredHash string
	// DesiredSpecJSON 是 []ServiceSpec 的 canonical JSON（密文化的快照明文
	// 形态——归位/重放的执行依据）。
	DesiredSpecJSON []byte
	// Warnings 是规划期警告（W_ENV_PLATFORM_OVERRIDE 等）。
	Warnings []compose.Warning
}

// BuildPlan 执行期望态规划。失败（服务无镜像/卷无登记/命名非法）返回
// apperr 信封错误。
//
// E5 Cron 口径（架构 §4.3 声明行「声明只在 compose，不建并行期望态」）：
// cron 服务（fleetly.cron label）进快照为 Job 模板（执行形态——镜像 digest
// 钉定/env 三层合并/网络/卷/资源限额/绑定约束与长驻服务同源编译，Job=true），
// 但不进 plan.Services（长驻对账集）——「只声明不部署」的装配面跳过由
// decodeSpecs 的 Job 过滤与对账单源保证。plan 对 cron 服务以警告如实披露
// （无注册码，Kind 标识）。
//
// DT-4 口径：init job 服务（fleetly.job: init label）同走 Job 模板路（不进
// plan.Services），另以 InitJob=true 标记并进入 plan.InitJobs——发布管线
// 在晋级前以该集合跑一次性作业；与 cron 的区分位 = InitJob（老快照
// Job=true 无 InitJob 即 cron，零回归）。
func BuildPlan(in PlanInput) (*Plan, error) {
	volByKey := map[string]state.Volume{}
	for _, v := range in.Volumes {
		volByKey[v.Key] = v
	}

	plan := &Plan{}
	all := make([]ServiceSpec, 0, len(in.Spec.Services))
	services := make([]ServiceSpec, 0, len(in.Spec.Services))
	envByService := map[string][]envSnapshotEntry{}
	for i := range in.Spec.Services {
		svc := &in.Spec.Services[i]
		image, ok := in.Images[svc.Name]
		if !ok || image == "" {
			return nil, errorf("E_BUILD_FAILED", "service %s is missing an image reference (not produced by the build/passthrough stage)", svc.Name)
		}
		spec, merged, err := buildServiceSpec(in, svc, image, volByKey)
		if err != nil {
			return nil, err
		}
		switch {
		case svc.Cron != nil:
			spec.Job = true
			plan.Warnings = append(plan.Warnings, compose.Warning{
				Kind:    compose.WarningKindCronServiceScheduled,
				Service: svc.Name,
				Message: "service " + svc.Name + " declares the cron schedule " + svc.Cron.Expression +
					" (declared-only: not deployed as a long-running service; the cron scheduler creates one-shot jobs on schedule)",
			})
		case svc.InitJob:
			// DT-4：init job 模板——不按长驻部署（发布管线在晋级前创建
			// 一次性 job，全过得进长驻对账）；执行形态与长驻同源编译
			//（buildServiceSpec 共用）。
			spec.Job = true
			spec.InitJob = true
			if svc.InitJobTimeout != "" {
				if d, perr := time.ParseDuration(svc.InitJobTimeout); perr == nil && d > 0 {
					spec.InitJobTimeout = d
				}
			}
			plan.InitJobs = append(plan.InitJobs, spec)
			plan.Warnings = append(plan.Warnings, compose.Warning{
				Kind:    compose.WarningKindInitJobDeclared,
				Service: svc.Name,
				Message: "service " + svc.Name + " declares " + compose.LabelJob + ": init" +
					" (declared-only: not deployed as a long-running service; the release pipeline runs it once before promoting the new revision)",
			})
		default:
			services = append(services, spec)
		}
		all = append(all, spec)
		envByService[svc.Name] = envEntriesOf(merged)
		plan.Warnings = append(plan.Warnings, envlayer.PlatformOverrideWarnings(&compose.Spec{
			Name:     in.Spec.Name,
			Services: []compose.Service{*svc},
		}, platformVarsFor(in, svc.Name))...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	sort.Slice(plan.InitJobs, func(i, j int) bool { return plan.InitJobs[i].Name < plan.InitJobs[j].Name })
	plan.Services = services
	plan.EnvSnapshotHash = canonicalHash(envByService)

	// 快照与期望态哈希先于服务 label 附加（label 含部署 ID——不进哈希与
	// 快照；归位重放时由对账层以当前发布归属重写服务 label，任务零替换）。
	// 快照含 Job 模板（E5 Cron 调度集的执行形态来源，decodeSpecs 对外过滤）。
	desiredJSON, err := canonicalJSON(all)
	if err != nil {
		return nil, errorf("E_RUNTIME_UNAVAILABLE", "failed to serialize desired state: %v", err)
	}
	for i := range services {
		if services[i].ServiceLabels == nil {
			services[i].ServiceLabels = map[string]string{}
		}
		services[i].ServiceLabels[state.LabelDesiredHash] = services[i].DesiredHash()
	}
	plan.DesiredSpecJSON = desiredJSON

	// 部署级哈希：spec + env + 各服务哈希。
	type desiredSummary struct {
		SpecHash        string            `json:"spec_hash"`
		EnvSnapshotHash string            `json:"env_snapshot_hash"`
		Services        map[string]string `json:"services"`
	}
	summary := desiredSummary{
		SpecHash:        in.Spec.SpecHash,
		EnvSnapshotHash: plan.EnvSnapshotHash,
		Services:        map[string]string{},
	}
	for i := range all {
		summary.Services[all[i].Name] = all[i].DesiredHash()
	}
	plan.DesiredHash = canonicalHash(summary)
	return plan, nil
}

// platformVarsFor 返回单个服务的平台层合并输入（app 全量行 + 本服务的
// system 层行——E3-4 注入面；system > platform 的覆盖序由 MergeChain 内部
// 保证）。警告面同盘：PlatformOverrideWarnings 用同一行集，system 键命中
// 文件层同键时产出 W_ENV_PLATFORM_OVERRIDE 既有警告。
func platformVarsFor(in PlanInput, service string) []envlayer.PlatformVar {
	sys, ok := in.SystemEnv[service]
	if !ok || len(sys) == 0 {
		return in.PlatformEnv
	}
	out := make([]envlayer.PlatformVar, 0, len(in.PlatformEnv)+len(sys))
	out = append(out, in.PlatformEnv...)
	out = append(out, sys...)
	return out
}

// buildServiceSpec 规划单个服务（返回 spec 与合并结果；spec 此时不带服务
// label——调用方统一附加）。
func buildServiceSpec(in PlanInput, svc *compose.Service, image string, volByKey map[string]state.Volume) (ServiceSpec, []envlayer.Merged, error) {
	swarmName, err := naming.ServiceName(in.TeamSlug, in.PrjSlug, in.AppName, svc.Name)
	if err != nil {
		return ServiceSpec{}, nil, errorf("E_RUNTIME_UNAVAILABLE", "naming failed for service %s: %v", svc.Name, err)
	}
	alias, err := naming.NetworkAlias(svc.Name)
	if err != nil {
		return ServiceSpec{}, nil, errorf("E_RUNTIME_UNAVAILABLE", "alias failed for service %s: %v", svc.Name, err)
	}
	netName, err := naming.NetworkName(in.TeamSlug, in.PrjSlug, in.AppName)
	if err != nil {
		return ServiceSpec{}, nil, errorf("E_RUNTIME_UNAVAILABLE", "network naming failed for app %s: %v", in.AppName, err)
	}

	// env 三层合并（文件层明文 × effective 平台层 × system 层——system 层
	// 仅对带 fleetly.s3 label 的服务注入，E3-4）。
	fileEnv := in.FileEnv[svc.Name]
	composeEnv := in.ComposeEnv[svc.Name]
	if fileEnv == nil {
		fileEnv = map[string]string{}
	}
	if composeEnv == nil {
		composeEnv = map[string]string{}
	}
	platform := platformVarsFor(in, svc.Name)
	merged, _ := envlayer.MergeChain(fileEnv, composeEnv, platform)
	envList := make([]string, 0, len(merged))
	for _, m := range merged {
		envList = append(envList, m.Key+"="+m.Value)
	}

	// 卷挂载：compose 卷 key → 卷注册表 docker 名。
	var mounts []MountSpec
	for _, m := range svc.Volumes {
		vol, ok := volByKey[m.Volume]
		if !ok || vol.Name == "" {
			return ServiceSpec{}, nil, errorf("E_PLACEMENT_NODE_UNAVAILABLE",
				"volume %s of service %s is not registered in the volume registry (placement Apply must run before planning)", svc.Name, m.Volume)
		}
		mounts = append(mounts, MountSpec{VolumeName: vol.Name, Target: m.Target, ReadOnly: m.ReadOnly})
	}

	// secret 挂载（E4 managed-databases §2.7）：在字面量内装配（见 spec
	// 构造处 Secrets 字段——resolveSecretMounts 已在 preparing 期完成存在性
	// 哨兵与底座确保，E_SECRET_NOT_FOUND fail-fast；本层纯装配，值只以
	// Swarm secret 名引用进投影与 desired-hash——换名即换 hash，轮换随
	// 下次部署换挂——明文零出现）。
	//
	// config 挂载（OT-3/IMPL-T1-4）：同款装配（ConfigMounts——config 名是
	// 内容寻址对象名，内容变更即换名换引用，随 desired-hash 触发服务滚动；
	// 值零出现——快照只带对象名）。init job 模板与长驻服务共用本函数，
	// config/secret 投影天然同源。

	// 更新顺序：有卷 / global 强制 stop-first；其余 compose 声明照用（缺省
	// start-first）。
	order := composeOrder(svc)
	if len(mounts) > 0 || svc.Deploy != nil && svc.Deploy.Mode == "global" {
		order = "stop-first"
	}

	// 网络接入：per-app 专属网络（别名 = compose 服务名）+ E3-4 rustfs
	// 牵线（仅 rustfs 模式且带 fleetly.s3 label 的服务——应用→RustFS 内网
	// 单向可达；external 模式不加，应用自行出网）+ E4 库共享网络（带
	// fleetly.databases label 的服务逐实例附加，**无别名**——引用方以
	// Swarm 服务名可达；managed-databases §2.4）+ OT-1 项目网（参与位
	// 在位时双挂——别名 = <app>-<service>，短名只在 app 私网；项目网名
	// 由装配层按 app 当前项目推导，MoveApp 保留参与位、投影随新项目）。
	networks := []NetworkAttach{{Name: netName, Aliases: []string{alias}}}
	if svc.S3 && in.AttachRustfsNetwork {
		networks = append(networks, NetworkAttach{Name: state.RustfsNetworkName})
	}
	for _, dbNet := range in.DBNetworks[svc.Name] {
		networks = append(networks, NetworkAttach{Name: dbNet})
	}
	if in.ProjectNetwork != "" {
		projectAlias, perr := naming.ProjectNetworkAlias(in.AppName, svc.Name)
		if perr != nil {
			return ServiceSpec{}, nil, errorf("E_RUNTIME_UNAVAILABLE", "project network alias failed for service %s: %v", svc.Name, perr)
		}
		networks = append(networks, NetworkAttach{Name: in.ProjectNetwork, Aliases: []string{projectAlias}})
	}

	spec := ServiceSpec{
		Name:    swarmName,
		Image:   image,
		Command: append([]string{}, svc.Command...),
		Env:     envList,
		ContainerLabels: map[string]string{
			// 容器 label 仅 fleetly.app，值 = 三段限定形（流标签口径）。
			state.LabelApp: qualifiedAppName(in),
		},
		ServiceLabels:     namingServiceLabels(in, svc.Name, in.DeploymentID),
		Global:            svc.Deploy != nil && svc.Deploy.Mode == "global",
		Replicas:          composeReplicas(svc),
		Networks:          networks,
		Mounts:            mounts,
		Secrets:           append([]SecretMount{}, in.SecretMounts[svc.Name]...),
		Configs:           append([]ConfigMount{}, in.ConfigMounts[svc.Name]...),
		Healthcheck:       composeHealthcheck(svc.Healthcheck),
		UpdateOrder:       order,
		UpdateParallelism: composeParallelism(svc),
		UpdateDelay:       composeDelay(svc),
		RestartPolicy:     composeRestartPolicy(svc),
		Resources:         composeResources(svc),
		StopSignal:        svc.StopSignal,
		StopGracePeriod:   composeStopGrace(svc),
	}
	// 放置约束：绑定钉住编译 + compose 声明（node.labels.fleetly.* 命名空间）。
	if in.Decision.Bind && in.Decision.Constraint != "" && len(mounts) > 0 {
		spec.Constraints = append(spec.Constraints, in.Decision.Constraint)
	}
	if svc.Deploy != nil && svc.Deploy.Placement != nil {
		spec.Constraints = append(spec.Constraints, svc.Deploy.Placement.Constraints...)
	}
	return spec, merged, nil
}

// qualifiedAppName 返回 app 的三段限定形 `team/prj/app`（流标签口径，
// rbac-teams §4.3；命名公式同源参数，失败视为不可达的命名违约——受控子集
// 已保证字符集）。
func qualifiedAppName(in PlanInput) string {
	q, err := naming.QualifiedName(in.TeamSlug, in.PrjSlug, in.AppName)
	if err != nil {
		return in.AppName
	}
	return q
}

// namingServiceLabels 构造服务 label 最小集（含 fleetly.team/fleetly.project
// 两键与三段限定形 app 值；naming 失败视为不可达的命名违约——受控子集已
// 保证字符集）。
func namingServiceLabels(in PlanInput, service, deploymentID string) map[string]string {
	labels, err := naming.ServiceLabels(in.TeamSlug, in.PrjSlug, in.AppName, service, deploymentID)
	if err != nil {
		return map[string]string{
			state.LabelManaged:    state.ManagedLabelValue,
			state.LabelApp:        qualifiedAppName(in),
			state.LabelProcess:    service,
			state.LabelDeployment: deploymentID,
			state.LabelTeam:       in.TeamSlug,
			state.LabelProject:    in.PrjSlug,
		}
	}
	return labels
}

// envSnapshotEntry 是 env 快照的脱敏条目（key:sha256+来源；值明文与长度
// 都不进输入）。
type envSnapshotEntry struct {
	Key    string `json:"key"`
	Hash   string `json:"hash"`
	Source string `json:"source"`
}

// envEntriesOf 投影合并结果为快照条目。
func envEntriesOf(merged []envlayer.Merged) []envSnapshotEntry {
	entries := make([]envSnapshotEntry, 0, len(merged))
	for _, m := range merged {
		entries = append(entries, envSnapshotEntry{Key: m.Key, Hash: m.Hash, Source: string(m.Source)})
	}
	return entries
}

// composeVolumes 汇总归一化 spec 的命名卷挂载（放置层的输入；key 去重）。
func composeVolumes(spec *compose.Spec) []placement.VolumeMount {
	seen := map[string]bool{}
	var out []placement.VolumeMount
	for i := range spec.Services {
		for _, m := range spec.Services[i].Volumes {
			if seen[m.Volume] {
				continue
			}
			seen[m.Volume] = true
			out = append(out, placement.VolumeMount{Key: m.Volume, Target: m.Target, ReadOnly: m.ReadOnly})
		}
	}
	return out
}

// composePlacementLabel 提取放置意图 label（受控子集校验已保证跨服务一致；
// 取首个非空）。
func composePlacementLabel(spec *compose.Spec) string {
	for i := range spec.Services {
		if spec.Services[i].PlacementNode != "" {
			return spec.Services[i].PlacementNode
		}
	}
	return ""
}

// envKeySetsMatch 交叉核对提取器与归一化形态的 env 键集（防 compose 文件
// 在入队后被并发改写——键集漂移即拒绝）。
func envKeySetsMatch(specEnv []compose.EnvVar, fileEnv, composeEnv map[string]string) bool {
	union := map[string]bool{}
	for k := range fileEnv {
		union[k] = true
	}
	for k := range composeEnv {
		union[k] = true
	}
	if len(union) != len(specEnv) {
		return false
	}
	for _, e := range specEnv {
		if !union[e.Key] {
			return false
		}
	}
	return true
}

// composeOrder 读 compose 声明的更新顺序（缺省 start-first）。
func composeOrder(svc *compose.Service) string {
	if svc.Deploy != nil && svc.Deploy.UpdateConfig != nil && svc.Deploy.UpdateConfig.Order != "" {
		return svc.Deploy.UpdateConfig.Order
	}
	return "start-first"
}

// composeReplicas 读期望副本（缺省 1；global 模式忽略）。
func composeReplicas(svc *compose.Service) uint64 {
	if svc.Deploy != nil && svc.Deploy.Replicas > 0 {
		return uint64(svc.Deploy.Replicas)
	}
	return 1
}

// composeParallelism 读更新并行度（缺省 1——swarm 0=不限流，必须显式落 1）。
func composeParallelism(svc *compose.Service) uint64 {
	if svc.Deploy != nil && svc.Deploy.UpdateConfig != nil && svc.Deploy.UpdateConfig.Parallelism > 0 {
		return svc.Deploy.UpdateConfig.Parallelism
	}
	return defaultParallelism
}

// composeDelay 读更新间隔。
func composeDelay(svc *compose.Service) time.Duration {
	if svc.Deploy != nil && svc.Deploy.UpdateConfig != nil && svc.Deploy.UpdateConfig.Delay != "" {
		if d, err := time.ParseDuration(svc.Deploy.UpdateConfig.Delay); err == nil {
			return d
		}
	}
	return 0
}

// composeHealthcheck 翻译健康检查（未写子字段取平台缺省 5s/3s/3/10s；
// nil = health_gate=none）。
func composeHealthcheck(hc *compose.Healthcheck) *HealthcheckSpec {
	if hc == nil || len(hc.Test) == 0 {
		return nil
	}
	out := &HealthcheckSpec{
		Test:        append([]string{}, hc.Test...),
		Interval:    defaultHealthInterval,
		Timeout:     defaultHealthTimeout,
		Retries:     defaultHealthRetries,
		StartPeriod: defaultHealthStartPeriod,
	}
	if hc.Interval != "" {
		if d, err := time.ParseDuration(hc.Interval); err == nil {
			out.Interval = d
		}
	}
	if hc.Timeout != "" {
		if d, err := time.ParseDuration(hc.Timeout); err == nil {
			out.Timeout = d
		}
	}
	if hc.Retries > 0 {
		out.Retries = hc.Retries
	}
	if hc.StartPeriod != "" {
		if d, err := time.ParseDuration(hc.StartPeriod); err == nil {
			out.StartPeriod = d
		}
	}
	return out
}

// composeRestartPolicy 翻译重启策略（缺省 condition=any / delay=5s）。
func composeRestartPolicy(svc *compose.Service) *RestartPolicySpec {
	out := &RestartPolicySpec{Condition: string(conditionAny), Delay: defaultRestartDelay}
	if svc.Deploy != nil && svc.Deploy.RestartPolicy != nil {
		rp := svc.Deploy.RestartPolicy
		if rp.Condition != "" {
			out.Condition = rp.Condition
		}
		if rp.Delay != "" {
			if d, err := time.ParseDuration(rp.Delay); err == nil {
				out.Delay = d
			}
		}
		out.MaxAttempts = rp.MaxAttempts
		if rp.Window != "" {
			if d, err := time.ParseDuration(rp.Window); err == nil {
				out.Window = d
			}
		}
	}
	return out
}

// conditionAny 与适配器缺省保持同一字面值。
const conditionAny = "any"

// defaultRestartDelay 是重启策略缺省间隔（architecture §2.5 运行期语义）。
const defaultRestartDelay = 5 * time.Second

// composeResources 翻译资源限额（仅 limits；cpus → nano CPUs）。
func composeResources(svc *compose.Service) *ResourcesSpec {
	if svc.Deploy == nil || svc.Deploy.Resources == nil || svc.Deploy.Resources.Limits == nil {
		return nil
	}
	limits := svc.Deploy.Resources.Limits
	out := &ResourcesSpec{}
	if limits.CPUS > 0 {
		out.NanoCPUs = int64(limits.CPUS * 1e9)
	}
	if limits.MemoryBytes > 0 {
		out.MemoryBytes = limits.MemoryBytes
	}
	if out.NanoCPUs == 0 && out.MemoryBytes == 0 {
		return nil
	}
	return out
}

// composeStopGrace 读停止宽限（≤0 = 底座缺省）。
func composeStopGrace(svc *compose.Service) time.Duration {
	if svc.StopGracePeriod == "" {
		return 0
	}
	if d, err := time.ParseDuration(svc.StopGracePeriod); err == nil {
		return d
	}
	return 0
}
