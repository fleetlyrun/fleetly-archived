package compose

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// EnvSource 是归一化 env 条目的来源标注（release-semantics §2.4：三层合并
// 链 env_file < environment < 平台层；平台层随引擎/env 票接入，本包只产出
// 文件内两层）。值取稳定字符串，进快照与 plan/diff 输出。
const (
	EnvSourceEnvFile     = "env_file"
	EnvSourceEnvironment = "environment"
)

// Spec 是 compose 文件经受控子集校验后的稳定归一化形态（架构 §2.4、
// release-semantics §2.4）：受控子集内的服务与卷定义；env 以
// key:sha256(value)+来源标注表示（值本身不进归一化结果）；无插值、字面值。
// 该形态即 revisions.compose_normalized 快照与 desired-hash 的地基。
//
// 稳定性纪律：所有切片输出前排序（服务/卷/网络/secret/域名/env 按名称或
// 字典序；expose/volumes 挂载顺序语义保留原序），canonical JSON 由
// CanonicalJSON 确定性编码，spec_hash = sha256(canonical_json)。
type Spec struct {
	Name     string    `json:"name,omitempty"`
	Services []Service `json:"services"`
	Volumes  []Volume  `json:"volumes,omitempty"`
	Networks []string  `json:"networks,omitempty"`
	// Secrets 是顶层 secrets 声明名的排序集合（E4 managed-databases §2.7：
	// 仅 {external: true} 形态——平台密钥库是唯一值来源，归一化形态只留
	// 声明名，值零出现）。
	Secrets []string `json:"secrets,omitempty"`
	// Configs 是顶层 configs 声明名的排序集合（T 线 OT-3/IMPL-T1-4：仅
	// {external: true} 形态——平台 app_configs 是唯一值来源，归一化形态只
	// 留声明名，值零出现）。
	Configs []string `json:"configs,omitempty"`
	// SpecHash 是 CanonicalJSON 的 sha256 hex（载入时填定）。
	SpecHash string `json:"spec_hash"`
}

// CanonicalJSON 输出确定性 JSON（键排序由 encoding/json 保证、HTML 转义
// 关闭、无尾部换行），作为 spec_hash 的哈希输入。
func (s *Spec) CanonicalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// hashSpec 计算 spec_hash 并回填（Load 内部使用）。
func (s *Spec) hashSpec() error {
	canonical, err := s.CanonicalJSON()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	s.SpecHash = hex.EncodeToString(sum[:])
	return nil
}

// Service 是归一化后的服务定义。只承载受控子集字段；平台管理项（镜像
// digest、secret 值、路由绑定、节点绑定）不进文件、不进快照。
type Service struct {
	Name string `json:"name"`
	// Build/Image 互斥或同现均可（compose 允许 image+build；构建模式裁决
	// 随构建票：无 dockerfile → Railpack，有 build.dockerfile → Dockerfile）。
	Build   *Build   `json:"build,omitempty"`
	Image   string   `json:"image,omitempty"`
	Command []string `json:"command,omitempty"`
	// Expose 保持原序（域名资源的播种端口取首个，架构 §2.4 / IMPL-T1-1）。
	Expose []string `json:"expose,omitempty"`
	// Domains 是 fleetly.domains label 解析后的域名列表（trim/小写/
	// IDN→punycode 归一化后排序）。IMPL-T1-1 起 label 仅首部署播种——
	// 域名资源（state 行/API CRUD）是路由声明真值。
	Domains []string `json:"domains,omitempty"`
	// PlacementNode 是 fleetly.placement.node label 的字面值（放置意图；
	// 名或 n_<ULID> 的解析/绑定校验属放置层，语法校验见 validate.go）。
	PlacementNode string `json:"placement_node,omitempty"`
	// S3 是 fleetly.s3=true 开关（E3-4）：true = 发布引擎为该服务注入
	// S3 system env（rustfs 模式附加平台内部网络）。进归一化快照与
	// spec_hash——label 变更即期望态变更，随下次部署生效。
	S3 bool `json:"s3,omitempty"`
	// Cron 是 fleetly.cron label 家族的归一化结果（E5 Cron，架构 §4.3）。
	// 非 nil = 该服务是 cron schedule：不按长驻部署（发布引擎跳过其服务
	// 装配），由调度器按点创建一次性 Swarm job。进归一化快照与 spec_hash
	//——调度集组装从 revision 的 compose_normalized 快照现读（internal/cron）。
	Cron *CronSchedule `json:"cron,omitempty"`
	// InitJob 是 fleetly.job label 的归一化结果（DT-4 部署期一次性作业）：
	// true = 该服务是 init job——不按长驻部署（Job 模板进快照），发布管线
	// 在晋级（长驻服务对账）前以新 spec 创建一次性 job，全部成功才推进；
	// 失败/超时即本次发布失败。与 Cron 互斥（解析期拒绝）。进归一化快照
	// 与 spec_hash——声明变更即期望态变更。
	InitJob bool `json:"init_job,omitempty"`
	// InitJobTimeout 是 fleetly.job.timeout 的归一化值（看门狗预算的
	// time.Duration.String() 形态；空 = 平台默认）。仅 InitJob=true 时有
	// 意义（孤儿 label 在解析期拒绝）。
	InitJobTimeout string `json:"init_job_timeout,omitempty"`
	// Databases 是 fleetly.databases label 的归一化结果（E4 托管数据库，
	// managed-databases §2.4/D-DB-4）：引用的库实例名列表（trim/排序——
	// 逗号分隔书写形态与顺序无关，快照取字典序）。非空 = 该服务引用库
	// 实例：发布引擎解析引用（存在性哨兵 + 前缀冲突哨兵）、物化 system
	// env 连接串并附加库共享网络。进归一化快照与 spec_hash——label 变更
	// 即期望态变更，随下次部署生效（引用登记由发布引擎从本声明重建）。
	Databases []string `json:"databases,omitempty"`

	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	// Environment 是 env 合并结果（env_file < environment），按 key 排序；
	// 只含 key+hash+source，值永不进归一化形态（脱敏结构性成立）。
	Environment []EnvVar `json:"environment,omitempty"`
	// Secrets 是服务级 secret 挂载（E4 managed-databases §2.7：external
	// secret 引用，source 必须在顶层 secrets 声明；target 缺省 = source——
	// 归一化时显式落位）。按 source 排序。声明名进快照与 spec_hash；值
	// 永不进归一化形态（值在平台密钥库，发布引擎按名装载）。
	Secrets []ServiceSecret `json:"secrets,omitempty"`
	// Configs 是服务级 config 挂载（T 线 OT-3/IMPL-T1-4：external config
	// 引用，source 必须在顶层 configs 声明；target 是显式绝对路径——平台
	// 不默认 /<source>）。按 source 排序。声明名与 target 进快照与
	// spec_hash；值永不进归一化形态（值在平台 app_configs，发布引擎按内容
	// 寻址装载到 swarm config 对象）。
	Configs []ServiceConfig `json:"configs,omitempty"`
	// Volumes 是命名卷挂载（v0.1 受控子集：仅命名卷；bind/tmpfs 拒绝），
	// 保持声明顺序（挂载点集合有语义）。
	Volumes  []Mount  `json:"volumes,omitempty"`
	Networks []string `json:"networks,omitempty"`

	Deploy          *Deploy `json:"deploy,omitempty"`
	StopSignal      string  `json:"stop_signal,omitempty"`
	StopGracePeriod string  `json:"stop_grace_period,omitempty"`
}

// Build 是归一化后的构建声明（仅保留平台消费的子键；构建引擎参数不在
// §2.4 支持清单，白名单拒绝）。
type Build struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

// Healthcheck 是按书写形态归一化的健康检查（平台默认 5s/3s/3/10s 只补
// 运行态缺省，不烘焙进快照——治理参数取当前平台配置，release-semantics
// §2.4）。时长为归一化字符串（time.Duration.String() 形态，如 10s）。
type Healthcheck struct {
	Test        []string `json:"test,omitempty"`
	Interval    string   `json:"interval,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	Retries     uint64   `json:"retries,omitempty"`
	StartPeriod string   `json:"start_period,omitempty"`
}

// EnvVar 是一条 env 合并结果：值只以 sha256 hex 表示（spec_hash 参与；
// 明文与明文长度都不泄露），Source 标注三层合并链中的文件内层级。
type EnvVar struct {
	Key    string `json:"key"`
	Hash   string `json:"hash"`
	Source string `json:"source"`
}

// Mount 是一次命名卷挂载。
type Mount struct {
	Volume   string `json:"volume"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ServiceSecret 是一次服务级 secret 挂载引用（E4 managed-databases §2.7，
// D-DB-7）：Source 是平台密钥库声明名（顶层 secrets 的 external 条目），
// Target 是容器内文件名（/run/secrets/<target>；缺省 = Source，归一化时
// 显式落位）。uid/gid/mode 平台受管（0:0/0444），不进归一化形态。
type ServiceSecret struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// ServiceConfig 是一次服务级 config 挂载引用（T 线 OT-3/IMPL-T1-4）：Source
// 是平台 app_configs 声明名（顶层 configs 的 external 条目），Target 是
// 容器内绝对路径（显式声明，只读——swarm config 恒只读）。uid/gid/mode
// 平台受管（0:0/0444），不进归一化形态；目录级文件树不映射（target 是
// 单文件路径）。
type ServiceConfig struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Deploy 是归一化后的 deploy 段（受管字段 failure_action/monitor 校验通过
// 后不进快照——值恒为 pause/5s 平台常量，写进快照只会放大噪音；order 及
// 其余字段照用）。
type Deploy struct {
	Mode          string         `json:"mode,omitempty"`
	Replicas      int64          `json:"replicas,omitempty"`
	UpdateConfig  *UpdateConfig  `json:"update_config,omitempty"`
	RestartPolicy *RestartPolicy `json:"restart_policy,omitempty"`
	Resources     *Resources     `json:"resources,omitempty"`
	Placement     *Placement     `json:"placement,omitempty"`
}

// UpdateConfig 归一化更新策略（order 照用；有卷服务显式 start-first 在
// 校验层拒绝 → E_COMPOSE_UNSAFE_STRATEGY）。
type UpdateConfig struct {
	Order       string `json:"order,omitempty"`
	Parallelism uint64 `json:"parallelism,omitempty"`
	Delay       string `json:"delay,omitempty"`
}

// RestartPolicy 归一化重启策略（release-semantics §2.8：restart_policy 照用）。
type RestartPolicy struct {
	Condition   string `json:"condition,omitempty"`
	Delay       string `json:"delay,omitempty"`
	MaxAttempts uint64 `json:"max_attempts,omitempty"`
	Window      string `json:"window,omitempty"`
}

// Resources 归一化资源限额（仅 limits；reservations 不在支持清单）。
type Resources struct {
	Limits *ResourceLimits `json:"limits,omitempty"`
}

// ResourceLimits 是 limits 的平台消费子集。
type ResourceLimits struct {
	CPUS        float64 `json:"cpus,omitempty"`
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
}

// Placement 归一化放置约束（constraints 仅允许 node.labels.fleetly.*
// 命名空间，stateful-placement §2.3）。
type Placement struct {
	Constraints []string `json:"constraints,omitempty"`
}

// Volume 是顶层命名卷声明（卷数据生命周期归平台卷注册表，声明仅承载
// driver 等卷级参数；external/name 拒绝）。
type Volume struct {
	Key    string `json:"key"`
	Driver string `json:"driver,omitempty"`
}

// CronSchedule 是 fleetly.cron label 家族的归一化值（E5 Cron，架构 §4.3
// 声明行）。表达式恰为五段标准式（cron.ParseStandard 契约，六段含秒的
// 书写在解析期即拒）；时区缺省 UTC（Timezone 空 = UTC）；Timeout 是看门狗
// 预算的归一化字符串（time.Duration.String() 形态，空 = 平台默认 10m）。
type CronSchedule struct {
	Expression string `json:"expression"`
	Timezone   string `json:"timezone,omitempty"`
	Timeout    string `json:"timeout,omitempty"`
}
