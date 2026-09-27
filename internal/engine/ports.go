package engine

import (
	"context"
	"errors"
	"time"
)

// Substrate 是发布引擎的底座（Swarm 服务/任务面）端口（架构 §2.8：端口在
// 核心、第三方适配在 internal/substrate，按签名隐式实现）。观测走 API 轮询
// （Spike B：Engine 29.x 无 task 事件）；服务写幂等语义由引擎对账层保证，
// 适配器只做忠实的翻译与错误归一。

// ServiceSpec 是一次目标服务形态（引擎期望态的执行投影；canonical JSON
// 序列化后参与 desired-hash——与底座 spec 深度对齐的字段集，受管字段
// failure_action=pause/monitor=5s 由适配器固定补齐，不进本结构与哈希）。
type ServiceSpec struct {
	// Name 是 Swarm 服务名（fleetly-<app>-<service>，naming.ServiceName）。
	Name string `json:"name"`
	// Image 是 digest 钉定引用（`repo@sha256:...`，D9）。
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	// Env 是合并后的注入环境（KEY=VALUE，按 key 字典序；值明文只存活于
	// 本结构与密文快照，不进日志/事件/审计）。
	Env []string `json:"env,omitempty"`
	// ServiceLabels 是服务级 label（naming.ServiceLabels 最小集 +
	// fleetly.desired-hash；service-label 变更不触发任务重建，Spike B2）。
	ServiceLabels map[string]string `json:"service_labels,omitempty"`
	// ContainerLabels 是任务容器 label（naming.ContainerLabels：仅
	// fleetly.app——跨部署稳定，归位零任务替换的前提，Spike B2）。
	ContainerLabels map[string]string `json:"container_labels,omitempty"`
	// Global 是 global 模式（compose deploy.mode=global）；false = replicated。
	Global bool `json:"global,omitempty"`
	// Job 是一次性 replicated-job 模式（E5 Cron / DT-4 init job，架构 §4.3
	// 执行行）：带该标记的 spec 不参与长驻对账（decodeSpecs 对外投影过滤
	// Job——发布/漂移/存在性对账只见长驻集），由一次性 job 运行器克隆为
	// job 服务（改名、replicas 1、restart-condition=none）按点/按发布创建。
	// 快照（desired_spec 密文）保留 Job 模板——cron 调度集与 init 相位的
	// 执行形态来源。
	Job bool `json:"job,omitempty"`
	// InitJob 标记 init job 模板（DT-4；Job=true 且 InitJob=true）：发布
	// 管线在晋级（长驻服务对账）前创建一次性 job 并等待全部成功；cron
	// 模板此位恒 false（JobKind 语义由该位与 Job 的组合表达，老快照
	// Job=true 无 InitJob 即为 cron——零回归兼容面）。
	InitJob bool `json:"init_job,omitempty"`
	// InitJobTimeout 是 init job 看门狗预算（fleetly.job.timeout 归一值；
	// 0 = 平台默认 engine.Config.InitJobTimeout）。每个 init job 独立计时，
	// 锚 = deployments.release_started_at。
	InitJobTimeout time.Duration `json:"init_job_timeout,omitempty"`
	// Replicas 是期望副本（replicated 模式；0 = scale-0 保留现场）。
	Replicas uint64 `json:"replicas"`
	// Networks 是服务接入网络（per-app 专属网络 + 别名 = compose 服务名）。
	Networks []NetworkAttach `json:"networks,omitempty"`
	// Mounts 是命名卷挂载（docker 卷名经卷注册表映射）。
	Mounts []MountSpec `json:"mounts,omitempty"`
	// Secrets 是 Swarm secret 挂载（file target = compose 名）。
	Secrets []SecretMount `json:"secrets,omitempty"`
	// Configs 是 Swarm config 文件挂载（OT-3/IMPL-T1-4）：ConfigName 是
	// 内容寻址的 swarm config 对象名（naming.ConfigName——内容变更即换名，
	// 引用随 desired-hash 变化触发服务滚动），Target 是容器内绝对路径
	//（只读）。明文值只存活于「app_configs → EnsureConfig 载荷」内存链，
	// 进快照的只有名字。
	Configs []ConfigMount `json:"configs,omitempty"`
	// Healthcheck 为 nil = health_gate=none（健康门退化为退出/副本水位）。
	Healthcheck *HealthcheckSpec `json:"healthcheck,omitempty"`
	// UpdateOrder 是更新顺序（start-first 默认；有卷/固定端口/global 强制
	// stop-first——组合裁决在规划层，适配器照用）。
	UpdateOrder string `json:"update_order"`
	// UpdateParallelism / UpdateDelay 照用 compose（parallelism 缺省 1）。
	UpdateParallelism uint64        `json:"update_parallelism"`
	UpdateDelay       time.Duration `json:"update_delay,omitempty"`
	// RestartPolicy 为 nil 时适配器补平台缺省（condition=any / delay=5s，
	// architecture §2.5 运行期语义）。
	RestartPolicy *RestartPolicySpec `json:"restart_policy,omitempty"`
	Resources     *ResourcesSpec     `json:"resources,omitempty"`
	// Constraints 是放置约束（绑定钉住编译 + compose 声明的
	// node.labels.fleetly.* 约束）。
	Constraints []string `json:"constraints,omitempty"`
	StopSignal  string   `json:"stop_signal,omitempty"`
	// StopGracePeriod ≤0 = 底座缺省（compose stop_grace_period 照用）。
	StopGracePeriod time.Duration `json:"stop_grace_period,omitempty"`
}

// DesiredHash 计算期望态哈希（对账变更判据，state-model §2.5 desired-hash
// 纪律：廉价、稳定、不泄露 env——哈希输入是全字段 canonical JSON，落库与
// label 的只是哈希）。服务级 label 不参与（含部署 ID——归位重放时服务
// label 更新不触发任务替换，Spike B2；container label 参与且跨部署稳定）。
func (s ServiceSpec) DesiredHash() string {
	c := s
	c.ServiceLabels = nil
	return canonicalHash(c)
}

// NetworkAttach 是一次服务网络接入。
type NetworkAttach struct {
	Name string `json:"name"`
	// Aliases 是网络内别名（= compose 服务名，app 内短名互访；项目网恒
	// <app>-<service>——OT-1 别名隔离，见 naming.ProjectNetworkAlias）。
	Aliases []string `json:"aliases,omitempty"`
}

// NetworkState 是一次网络对象实况投影（IMPL-T15-1 项目网对账/GC 的读面；
// ServiceState 同纪律：第三方类型不出适配器）。
type NetworkState struct {
	Name string
	// ID 是底座对象 ID（诊断面）。
	ID string
	// Labels 是网络对象 label 集（对账归因面：managed=true 平台网 +
	// fleetly.project-network=<projectID> 项目网归属锚）。
	Labels map[string]string
	// Driver 是网络驱动（overlay/bridge/...；只读披露）。
	Driver string
	// Containers 是当前挂接的容器数（GC 安全性判据：零端点才回收；daemon
	// 对 in-use 网络的移除另有 FailedPrecondition 拒绝兜底——真机实证）。
	Containers int
	// Services 是引用该网络的 Swarm 服务数（network inspect 的 Services
	// 字段）。GC 安全性判据与 Containers 同款（零引用零端点才回收）；真机
	// 实测注记（Docker 29.7.2）：受管 overlay 上该字段未填充（恒 0），
	// 实际兜底 = Containers 计数 + daemon in-use 拒绝（FailedPrecondition
	// 原样上抛、下拍重试）——零引用语义不因此失效（见 runbook 实测块）。
	Services int
}

// MountSpec 是一次命名卷挂载。
type MountSpec struct {
	// VolumeName 是平台卷注册表的 docker 卷名（fleetly-<app>-<key>-<id8>）。
	VolumeName string `json:"volume"`
	Target     string `json:"target"`
	ReadOnly   bool   `json:"read_only,omitempty"`
}

// SecretMount 是一次 Swarm secret 文件挂载。
type SecretMount struct {
	// SecretName 是 Swarm secret 名（fleetly-<app>-<name>-<hash8>）。
	SecretName string `json:"secret"`
	// Target 是容器内挂载文件路径（/run/secrets/<compose 名>）。
	Target string `json:"target"`
}

// ConfigMount 是一次 Swarm config 文件挂载（OT-3，IMPL-T1-4）：ConfigName
// 是内容寻址的 swarm config 对象名（naming.ConfigName——值变更即换名换
// 引用），Target 是容器内绝对路径（config 恒只读，/run/secrets 前缀在
// compose 校验层拒绝——secret 固定根不可撞）。
type ConfigMount struct {
	// ConfigName 是 Swarm config 对象名
	//（fleetly-<team>-<prj>-<app>-config-<name>-<hash8>）。
	ConfigName string `json:"config"`
	// Target 是容器内挂载文件绝对路径（compose configs.target）。
	Target string `json:"target"`
}

// HealthcheckSpec 是健康检查（平台缺省 5s/3s/3/10s 由规划层补齐——治理
// 参数取当前平台配置，release-semantics §2.4）。
type HealthcheckSpec struct {
	Test        []string      `json:"test"`
	Interval    time.Duration `json:"interval"`
	Timeout     time.Duration `json:"timeout"`
	Retries     uint64        `json:"retries"`
	StartPeriod time.Duration `json:"start_period"`
}

// RestartPolicySpec 是重启策略（architecture §2.5：缺省 condition=any、
// delay=5s——Swarm 重启自愈语义）。
type RestartPolicySpec struct {
	Condition   string        `json:"condition"`
	Delay       time.Duration `json:"delay,omitempty"`
	MaxAttempts uint64        `json:"max_attempts,omitempty"`
	Window      time.Duration `json:"window,omitempty"`
}

// ResourcesSpec 是资源限额（仅 limits；cpus 以 nano CPUs 计）。
type ResourcesSpec struct {
	NanoCPUs    int64 `json:"nano_cpus,omitempty"`
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
}

// ServiceState 是一次 Swarm 服务实况投影（对账/健康门观测输入；T2.13 起
// 兼作运行域漂移反解载体——spec 侧字段由适配器从 swarm.Service.Spec 逐字
// 抄出，漂移投影只取 state-model §2.5 受控子集字段，env 值只进 sha256 不
// 出端口消费面）。
type ServiceState struct {
	Name string
	// Version 是底座对象版本（乐观令牌；诊断用——服务写经适配器内部取版
	// 本，引擎不做跨调用令牌传递，避免写前直读令牌陈旧化）。
	Version uint64
	Labels  map[string]string
	// DesiredHash 是服务 label 里的期望态哈希（'' = 非平台写或旧形态）。
	DesiredHash string
	// Image 是当前 spec 镜像引用。
	Image string
	// Replicas 是期望副本数（replicated；global 服务为 0）。
	Replicas uint64
	// UpdateState / UpdateMessage 逐字镜像底座 UpdateStatus
	//（''/updating/paused/completed，Spike B：更新失败归 paused+Message）。
	UpdateState   string
	UpdateMessage string

	// ── 运行域漂移反解字段（T2.13；与 ServiceSpec 同构，由适配器填充）──
	Command         []string
	Env             []string
	ContainerLabels map[string]string
	Global          bool
	Networks        []NetworkAttach
	Mounts          []MountSpec
	Secrets         []SecretMount
	Configs         []ConfigMount
	Healthcheck     *HealthcheckSpec
	RestartPolicy   *RestartPolicySpec
	Resources       *ResourcesSpec
	Constraints     []string
	StopSignal      string
	StopGracePeriod time.Duration
	// ── S18-A8：Spec.UpdateConfig 投影补齐（外部 docker service update
	// --update-* 篡改的判定面）。Order/Parallelism/Delay 三字段随
	// serviceSpecOf 进漂移哈希；FailureAction 是平台受管字段（适配器固定
	// pause，不进 ServiceSpec/哈希），篡改走 drift 的专报项。──
	UpdateOrder         string
	UpdateParallelism   uint64
	UpdateDelay         time.Duration
	UpdateFailureAction string
}

// serviceSpecOf 把服务实况投影还原为 ServiceSpec 形态（漂移投影的实况侧
// 输入；仅投影字段参与，Version/UpdateState 等观测字段不进哈希）。A8 起
// UpdateConfig 三字段（order/parallelism/delay）随行——外部篡改与期望态
// 同构可比。
func serviceSpecOf(s ServiceState) ServiceSpec {
	return ServiceSpec{
		Name:              s.Name,
		Image:             s.Image,
		Command:           s.Command,
		Env:               s.Env,
		ServiceLabels:     s.Labels,
		ContainerLabels:   s.ContainerLabels,
		Global:            s.Global,
		Replicas:          s.Replicas,
		Networks:          s.Networks,
		Mounts:            s.Mounts,
		Secrets:           s.Secrets,
		Configs:           s.Configs,
		Healthcheck:       s.Healthcheck,
		RestartPolicy:     s.RestartPolicy,
		Resources:         s.Resources,
		Constraints:       s.Constraints,
		StopSignal:        s.StopSignal,
		StopGracePeriod:   s.StopGracePeriod,
		UpdateOrder:       s.UpdateOrder,
		UpdateParallelism: s.UpdateParallelism,
		UpdateDelay:       s.UpdateDelay,
	}
}

// TaskState 是一次任务实况投影（service ps 轮询；Spike B 观测纪律）。
type TaskState struct {
	ID   string
	Slot int
	// State 逐字镜像底座任务状态（new/pending/running/failed/complete/
	// shutdown/rejected/...）。
	State string
	// DesiredState 是期望态（running/remove/...）——旧任务下线判据。
	DesiredState string
	// Err 是任务失败原因（逐字；health 判定归 E_HEALTH_TIMEOUT 的来源）。
	Err string
	// Timestamp 是任务状态时间戳。
	Timestamp time.Time
	// Image 是任务 spec 镜像引用（新旧版本判据：与目标 digest 比对）。
	Image string
}

// Substrate 是底座服务/任务面端口（internal/substrate.Client 隐式实现）。
type Substrate interface {
	// SwarmReady 确认本机为 active swarm manager；未 init 返回可判别错误
	//（errors.Is(err, ErrNotSwarmReady)）。
	SwarmReady(ctx context.Context) error
	// NetworkEnsure 确认 overlay 网络存在（幂等；缺失创建）。
	NetworkEnsure(ctx context.Context, name string) error
	// ServiceInspect 按名取服务实况；缺失返回 ErrServiceNotFound。
	ServiceInspect(ctx context.Context, name string) (ServiceState, error)
	// ServiceCreate 创建服务（幂等性由调用方对账保证：仅缺失时调用）。
	ServiceCreate(ctx context.Context, spec ServiceSpec) error
	// ServiceUpdate 以乐观令牌推进服务到目标形态。适配器内部读版本并短
	// 重试并发冲突；ForceUpdate 恒不递增（归位零成本纪律，Spike B2）。
	ServiceUpdate(ctx context.Context, name string, spec ServiceSpec) error
	// ServiceRemove 删除服务（省略=删除语义；幂等：缺失视为成功）。
	ServiceRemove(ctx context.Context, name string) error
	// ServiceList 按 label 选择器返回服务（managed+app 前缀过滤）。
	ServiceList(ctx context.Context, labels map[string]string) ([]ServiceState, error)
	// TaskList 返回服务的全部任务（含历史；观测轮询的数据源）。
	TaskList(ctx context.Context, service string) ([]TaskState, error)
}

// ErrServiceNotFound / ErrNotSwarmReady 是端口哨兵（适配器归一）。
var (
	ErrServiceNotFound = errors.New("substrate service not found")
	// ErrNotSwarmReady 表示引擎未启用 Swarm（或本机非 active manager）。
	ErrNotSwarmReady = errors.New("docker engine is not an active swarm manager")
)

// NetworkSubstrate 是底座网络对象面端口（IMPL-T15-1 项目网生命周期 + 对账
// 扩面；实现 = substrate.Client）。**独立小端口**（不并入 Substrate：既有
// 端口只承担服务/任务面与 app 网 NetworkEnsure，测试替身零波及——仓库
// 「新能力 = 新端口 + With 注入」惯例，SecretEnsurer/ConfigEnsurer 同款）。
type NetworkSubstrate interface {
	// NetworkEnsureWithLabels 幂等确保网络存在（缺失创建；自描述 label
	// 随创建写入——项目网 = managed + fleetly.project-network=<projectID>）。
	NetworkEnsureWithLabels(ctx context.Context, name string, labels map[string]string) error
	// NetworkList 按 label 选择器（key=value）返回网络投影。
	NetworkList(ctx context.Context, labels map[string]string) ([]NetworkState, error)
	// NetworkInspect 按名取网络投影；缺失返回 ErrNetworkNotFound。
	NetworkInspect(ctx context.Context, name string) (NetworkState, error)
	// NetworkRemove 删除网络（幂等：缺失视为成功；in-use 由底座拒绝——
	// 调用方 best-effort 消化，下一拍重试）。
	NetworkRemove(ctx context.Context, name string) error
}

// ErrNetworkNotFound 表示目标网络不存在（端口哨兵，适配器归一）。
var ErrNetworkNotFound = errors.New("substrate network not found")

// 镜像可见性端口（复用 build 层端口形态；适配器同源）。
type ImageChecker interface {
	// ImageDigest 返回镜像本机 ID（`sha256:<hex>`）；缺失返回
	// ErrImageMissing。
	ImageDigest(ctx context.Context, ref string) (string, error)
}

// ErrImageMissing 表示镜像本机不可得（preflight → E_IMAGE_PULL_FAILED）。
var ErrImageMissing = errors.New("image not available on local daemon")

// Clock 是可注入时钟（看门狗/观察窗单测的时间控制点）。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Config 是引擎治理参数（release-semantics §2.8：v0.1 平台默认；config 节
// 仅作部署面覆盖入口，文件缺省即文档默认）。
type Config struct {
	// DeployTimeout 是 L2 看门狗预算（默认 300s；有效值 ≥ health 预算）。
	DeployTimeout time.Duration
	// InitJobTimeout 是 init job 看门狗的平台缺省预算（默认 10m，沿 cron
	// DefaultJobTimeout 同值；fleetly.job.timeout label 逐服务覆盖——
	// DT-4）。init 相位由它起算看门狗；job 全过后重臂 DeployTimeout 给
	// 健康门完整预算。
	InitJobTimeout time.Duration
	// ObserveWindow 是 L3 观察窗时长（默认 60s）。
	ObserveWindow time.Duration
	// UnstableReplicasBelow 是副本水位的持续不足判定时长（默认 10s，
	// release-semantics §2.2 L3 信号）。
	ReplicasBelowFor time.Duration
	// PollInterval 是引擎轮询周期（默认 2s）。
	PollInterval time.Duration
	// DriftInterval 是运行域漂移检测扫描周期（默认 30s，可配
	// engine.drift_interval_seconds；D11：检测默认开）。
	DriftInterval time.Duration
}

// Normalize 回落文档默认值。
func (c Config) Normalize() Config {
	if c.DeployTimeout <= 0 {
		c.DeployTimeout = 300 * time.Second
	}
	if c.InitJobTimeout <= 0 {
		c.InitJobTimeout = DefaultJobTimeout
	}
	if c.ObserveWindow <= 0 {
		c.ObserveWindow = 60 * time.Second
	}
	if c.ReplicasBelowFor <= 0 {
		c.ReplicasBelowFor = 10 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.DriftInterval <= 0 {
		c.DriftInterval = 30 * time.Second
	}
	return c
}
