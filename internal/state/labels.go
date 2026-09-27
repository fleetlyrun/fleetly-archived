package state

import "strings"

// fleetly.* label 最小契约（state-model §2.4）：保留命名空间 fleetly.*
// 为平台独占（用户占用 → E_LABEL_RESERVED 422，映射随 API 面落地）；写者
// 唯一 = 适配器 Marker 端口（本阶段仅节点身份锚经 DockerClient.UpdateNodeLabel
// 写入，业务对象 label 下发随 T2.14）；密钥/payload 永不入 label；平台保留
// 未知 fleetly.* 键不改写。
const (
	// LabelNamespace 是平台 label 保留命名空间前缀。
	LabelNamespace = "fleetly."

	// LabelManaged 标记平台受管对象（服务 label，"true"）：归属判定、
	// 孤儿检测、删除保护的依据。
	LabelManaged = "fleetly.managed"
	// LabelApp 标记归属应用（服务/容器 label；值 = 三段限定形
	// `team/prj/app`——v0.3 流标签口径，rbac-teams §4.3； QualifiedName）。
	LabelApp = "fleetly.app"
	// LabelTeam / LabelProject 标记归属 team 与 project（服务 label，值 =
	// 两个单词制 slug；v0.3 rbac-teams §4.3「label 集 +fleetly.team /
	// fleetly.project——服务创建时写入，对账/清扫/管理查询的识别面」）。
	LabelTeam = "fleetly.team"
	// LabelProject 标记归属 project slug（同上）。
	LabelProject = "fleetly.project"
	// LabelProcess 标记 compose 服务名（应用内进程名）。
	LabelProcess = "fleetly.process"
	// LabelDeployment 标记发布归属（值 = deployment ID）。
	LabelDeployment = "fleetly.deployment"
	// LabelDesiredHash 是服务级期望态哈希（对账变更判据的落点 label，
	// state-model §2.5 desired-hash 纪律；service-label 变更不触发任务
	// 重建——归位零成本的配套，Spike B2）。
	LabelDesiredHash = "fleetly.desired-hash"
	// LabelCron 标记定时任务 schedule（v0.2 契约，常量先行，state-model §2.4）。
	LabelCron = "fleetly.cron"
	// LabelCronRun 标记一次性 cron job 服务归属的运行行（E5 Cron；值 =
	// cron_runs.id——残留 job 服务与台账行的对账锚）。
	LabelCronRun = "fleetly.cron.run"
	// LabelInitRun 标记部署期 init job 服务归属的部署行（DT-4；值 =
	// deployments.id——孤儿清扫（引擎 sweepInitJobs）与日志采集（JobServiceStates）
	// 的归属锚；fleetly.cron.run 同款纪律）。
	LabelInitRun = "fleetly.init.run"

	// LabelDatabase 标记库实例归属（E4 数据库托管，managed-databases §2.1
	// D-DB-1：库服务/网络/卷/secret 的自描述 marker，值 = 库实例名）。与
	// LabelApp 平行的独立归属锚——库实例与 app 可重名、对象前缀族解耦
	//（fleetly-db-*），label 面同样独立；库收敛器（internal/database）
	// 与 secret 清场按此 label 选择。
	LabelDatabase = "fleetly.db"

	// LabelProjectNetwork 标记项目网 overlay 归属（网络对象 label，值 =
	// 项目平台 ID；OT-1/IMPL-T15-1）。识别面用途：①对账/GC 无需从前缀
	// 反解项目（id8 是有损截断）即可归因；②孤儿网判定不靠前缀猜测
	//（前缀族众多，票面明确要求自描述 label）。服务 label 集不含此键
	// （仅网络对象写入）。
	LabelProjectNetwork = "fleetly.project-network"

	// LabelNodeID 是节点身份锚（node label，值 = 平台节点 ID n_<ULID>，
	// state-model §2.3）。
	LabelNodeID = "fleetly.node-id"

	// LabelDomains 是平台约定路由域名列表（compose 服务 label，逗号分隔；
	// IMPL-T1-1 起仅首部署 bootstrap 种子——state 域名行是路由声明真值，
	// 有行后 label 一律忽略并派 route.label_ignored 事件）。
	LabelDomains = "fleetly.domains"
	// LabelPlacementNode 是放置意图（compose 服务 label，v0.2 多节点消费，
	// 常量先行）。
	LabelPlacementNode = "fleetly.placement.node"
)

// IsReservedLabel 报告 key 是否落在平台保留命名空间（fleetly.*）：
// 用户声明该命名空间内的 label 时上游应拒绝（E_LABEL_RESERVED）。
func IsReservedLabel(key string) bool {
	return strings.HasPrefix(key, LabelNamespace)
}

// ManagedLabelValue 是 LabelManaged 的约定取值。
const ManagedLabelValue = "true"
