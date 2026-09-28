// Package naming 是 fleetly 平台对象命名与最小 label 集的唯一定义点
// （state-model §2.4 对象标记契约 + architecture §2.4 服务命名与网络行）。
//
// 纪律：
//   - 命名公式逐字对齐设计文档，任何字符串改动都意味着契约破坏（表驱动
//     测试对照文档原文钉死，naming_test.go）；
//   - 平台命名只存在于适配器——归一化 compose 不出现平台名（§2.4「平台
//     命名不进归一化 compose」）；调用方 = internal/substrate / 发布引擎；
//   - label 密钥/payload 永不进 label（state-model §2.4）。
//
// v0.3 三段命名（D-W0-4 二修，rbac-teams §4.3）：app/库名唯一性降为
// project 内唯一，全局唯一由 team·prj 段承载——app 族与库族的 service/
// network/secret/cron 公式全部在对象名内插入 `<team>-<prj>` 两段（两个
// slug 均单词制 [a-z0-9]{2,32}、不可变，拼接无歧义）。**卷两族公式不变**
// （appid8/id8 = ULID 前 8 位尾缀已天然全局防撞——零卷迁移/零数据搬移，
// MoveApp 换名重部署时卷引用原样保留）。
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// 平台命名公式（文档原文逐字；state-model §2.4 / architecture §2.4 /
// rbac-teams §4.3 对照表）：
//
//	Swarm 服务名   fleetly-<team>-<prj>-<app>-<service>
//	Secret 名      fleetly-<team>-<prj>-<app>-<name>-<hash8>（hash8 = 内容
//	               sha256 前 8）
//	Config 名      fleetly-<team>-<prj>-<app>-config-<name>-<hash8>（OT-3：
//	               明文配置的 swarm config 对象，内容寻址同 Secret）
//	卷名           fleetly-<app>-<key>-<appid8>   （v0.3 不变；appid8 = app
//	               ID 前 8）
//	网络名         fleetly-<team>-<prj>-<app>-net （每 app 专属 overlay；
//	               文档未钉字符串，保守补全，见包内注释与遗留记录）
//	项目网名       fleetly-project-<projectID>      （OT-1 共享作用域 overlay；
//	               项目平台 ID 全量——ULID 前 8 只承载 40 位时间戳成分，
//	               256ms 窗口内创建的项目撞名（本票测试实证），截断即
//	               有「网络静默合并」风险；Crockford 字符集与三段 slug
//	               网络名结构不相交）
//	项目网别名     <app>-<service>                 （仅项目网；短名别名
//	               <service> 仅在 app 专属网——OT-1 别名隔离）
//	任务服务名     fleetly-task-<taskID>            （DT-5 程序化动态工作负载：
//	               稳定 DNS 名 = 服务名；taskID = 任务平台 ULID 全量）
//	task-group 网名 fleetly-taskgroup-<ref>         （DT-5 长活共享作用域；
//	               ref = 调用方 slug [a-z0-9]{2,32}，不含 '-'——与三段 app
//	               网名结构不相交）
//	日志流标签 app 值  <team>/<prj>/<app>（三段限定形，QualifiedName）
const (
	// namePrefix 是全部平台对象名的公共前缀（防集群全局命名空间撞名）。
	namePrefix = "fleetly-"
)

// PlatformPrefix 是平台对象名公共前缀的导出形态（`fleetly-`）——命名空间
// 判据的唯一来源（对账/清扫面以「managed label + 前缀」双条件圈定平台对象；
// engine 等包不复制字面量）。
const PlatformPrefix = namePrefix

// QualifiedName 返回 app/库实例的三段限定形 `<team>/<prj>/<name>`（D-W0-9
// 引用解析口径；'/' 不是命名成分合法字符，结构性与 '-' 拼接歧义互斥）。
// 该形态是 fleetly.app label 值与日志流标签 app 值的统一口径（rbac-teams
// §4.3 流标签行）。
func QualifiedName(team, prj, name string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("name", name); err != nil {
		return "", err
	}
	return team + "/" + prj + "/" + name, nil
}

// ServiceName 返回 Swarm 服务名 `fleetly-<team>-<prj>-<app>-<service>`：
// 全局唯一由 team·prj 段保证（app 名 project 内唯一——不同项目的同名服务
// 不冲突）；服务别名 = compose 服务名（app 内短名互访与 compose 语义一致，
// 见 NetworkAlias）。
func ServiceName(team, prj, app, service string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	if err := validateComponent("service", service); err != nil {
		return "", err
	}
	return joinName(team, prj, app, service), nil
}

// ServiceNameQualified 由三段限定形 app 标识（QualifiedName 产物）与
// compose 服务名返回 Swarm 服务名——QualifiedName 的逆消费面（'/' 在命名
// 成分字符集外，按 '/' 拆分无歧义）。terminal 会话等只持限定形的调用方
// 消费。
func ServiceNameQualified(qualifiedApp, service string) (string, error) {
	parts := strings.Split(qualifiedApp, "/")
	if len(parts) != 3 {
		return "", fmt.Errorf("naming: qualified app %q is not team/prj/app form", qualifiedApp)
	}
	return ServiceName(parts[0], parts[1], parts[2], service)
}

// SecretName 返回 Swarm secret 名 `fleetly-<team>-<prj>-<app>-<name>-<hash8>`。
// hash8 由调用方经 Hash8(内容) 计算——**值轮换即换名换引用**（architecture
// §2.4 密钥行；desired-hash 以 secret 引用参与，轮换天然触发重部署）。
func SecretName(team, prj, app, name, hash8 string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	if err := validateComponent("name", name); err != nil {
		return "", err
	}
	if err := validateHash8(hash8); err != nil {
		return "", err
	}
	return joinName(team, prj, app, name, hash8), nil
}

// ConfigName 返回 Swarm config 名
// `fleetly-<team>-<prj>-<app>-config-<name>-<hash8>`（OT-3/IMPL-T1-4：明文
// 配置资源的内容寻址底座对象名）。hash8 由调用方经 Hash8(内容) 计算——
// **内容变更即换名换引用**（architecture §2.4 密钥行同款语义：引用进
// desired-hash，变更随下次部署换挂并触发服务滚动）。config 段是公式内的
// 固定标识位（name 成分字符集不含 '-' 之外的分隔符，段位无歧义）。
func ConfigName(team, prj, app, name, hash8 string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	if err := validateComponent("name", name); err != nil {
		return "", err
	}
	if err := validateHash8(hash8); err != nil {
		return "", err
	}
	return joinName(team, prj, app, "config", name, hash8), nil
}

// VolumeName 返回平台卷名 `fleetly-<app>-<key>-<appid8>`（**v0.3 公式不变**
// ——rbac-teams §4.3 卷行：appid8 = app 平台 ID 前 8 位已天然全局防撞，零卷
// 迁移/零数据搬移；MoveApp 换名重部署时卷引用原样保留）。卷无 label，用命
// 名约定承载归属（state-model §2.4；VolumeOptions.Labels 生效前的收敛路径，
// stateful-placement §6）。
func VolumeName(app, key, appID string) (string, error) {
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	if err := validateComponent("key", key); err != nil {
		return "", err
	}
	id8, err := AppID8(appID)
	if err != nil {
		return "", err
	}
	return joinName(app, key, id8), nil
}

// NetworkName 返回 app 专属 overlay 网络名（保守补全
// `fleetly-<team>-<prj>-<app>-net`；文档钉死的是「每 app 专属 overlay 网络
// + 服务别名 = compose 服务名」语义，未钉网络名字符串——命名与前缀纪律保
// 持一致，跨 app 网络隔离由专属网络承载，architecture §2.4 服务命名与网络
// 行；team·prj 段承载全局唯一）。
func NetworkName(team, prj, app string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	return joinName(team, prj, app, "net"), nil
}

// NetworkAlias 返回服务在 app 网络内的别名 = compose 服务名（app 内短名
// 互访与 compose 语义一致；跨 app 不可见——隔离由每 app 专属网络保证）。
// v0.3 不变（别名是 compose 语义面，不进平台命名空间）。**仅 app 专属网**
// 使用；项目网别名恒为 ProjectNetworkAlias（<app>-<service>）。
func NetworkAlias(service string) (string, error) {
	if err := validateComponent("service", service); err != nil {
		return "", err
	}
	return service, nil
}

// projectNetworkPrefix 是项目网（OT-1 共享作用域 overlay）名固定前缀
// `fleetly-project-`。完整名 = 前缀 + **项目平台 ID 全量**（ULID 26 位
// Crockford 大写字母+数字）——不截断：ULID 前 8 位只承载 40 位时间戳成分
// （256ms 窗口内创建的项目会撞名，本票测试实证两个夹具项目撞名），截断即
// 存在「第二个项目的项目网静默并入第一个项目网络」的合并风险；全量 ID
// 结构性唯一（项目行主键），且与三段 slug 网络名 `fleetly-<team>-<prj>-<app>-net`
// 结构不相交（后者含小写字母与 '-'，Crockford 字符集不含 '-'）。
// 该前缀族同时是识别面（IsProjectNetworkName），与 app 网/库网/组件网互不
// 误伤；网络名不是 DNS 名，42 字符长度无解析面约束。
const projectNetworkPrefix = namePrefix + "project-"

// ProjectNetworkName 返回项目网 overlay 名 `fleetly-project-<projectID>`
// （OT-1：Project = 网络共享作用域资源，平台建、成员服务双挂；ID 全量不
// 截断——唯一性由项目主键承载，见前缀常量注）。
func ProjectNetworkName(projectID string) (string, error) {
	if err := validateProjectID(projectID); err != nil {
		return "", err
	}
	return projectNetworkPrefix + projectID, nil
}

// validateProjectID 校验项目 ID 形态（26 位 ULID Crockford 大写字母+数字；
// 截断/非法字符显式拒绝——命名对象的唯一性以 ID 全量为前提，形态违约必须
// 在入口暴露而非产生一个看似合法却不再唯一的网络名）。
func validateProjectID(id string) error {
	if id == "" {
		return fmt.Errorf("naming: project id is empty")
	}
	if len(id) != 26 {
		return fmt.Errorf("naming: project id %q must be a 26-char ULID", id)
	}
	for _, r := range id {
		isCrockford := (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')
		if !isCrockford {
			return fmt.Errorf("naming: project id %q contains character %q outside the Crockford alphabet", id, r)
		}
	}
	return nil
}

// IsProjectNetworkName 报告网络名是否为项目网（前缀 + 全量项目 ID
// Crockford 尾段；前缀族识别谓词，naming.IsCronJobName 同款纪律）。
func IsProjectNetworkName(name string) bool {
	rest, ok := strings.CutPrefix(name, projectNetworkPrefix)
	if !ok || rest == "" {
		return false
	}
	return validateProjectID(rest) == nil
}

// ProjectNetworkAlias 返回服务在项目网内的别名 `<app>-<service>`
// （OT-1 蓝图原条款：项目网别名 = app-service 拼接；**短名 <service> 只在
// app 专属网**——DNS 解析域 = 容器所属网络，项目网内不存在短名记录，
// 跨 app 短名混流在结构上不可能）。app 名 project 内唯一 + 成员双挂使该
// 别名在项目网内唯一（同项目同名 app 非法——UNIQUE(project_id,name)）。
func ProjectNetworkAlias(app, service string) (string, error) {
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	if err := validateComponent("service", service); err != nil {
		return "", err
	}
	return app + "-" + service, nil
}

// taskNamePrefix / taskGroupNetworkPrefix 是程序化动态工作负载（T 线 DT-5）
// 的命名族：任务服务名 `fleetly-task-<taskID>`（taskID = 任务平台 ULID 全量），
// task-group 网络名 `fleetly-taskgroup-<ref>`（ref = 调用方 slug）。两族与既有
// 前缀族结构不相交的论证：
//   - task 前缀族 vs 三段 app 服务名/网络名（team slug="task" 时
//     `fleetly-task-<prj>-<app>-…` 含小写与 '-'）——taskID 是 26 位 Crockford
//     大写 ULID，字符集不相交（IsTaskServiceName 以 ULID 形态校验尾段）；
//   - taskgroup 前缀族 vs 三段 app 网名（team slug="taskgroup" 时
//     `fleetly-taskgroup-<prj>-<app>-net`）——ref 禁止 '-'（[a-z0-9]{2,32}），
//     结构不相交；
//   - 与 cron/init/db/dbjob 前缀族首段不同，天然不相交。
//
// 任务服务是瞬时/半瞬时对象（TTL 回收），识别谓词同时服务对账豁免、日志
// 归因与孤儿 task 清扫（IsCronJobName 同款纪律）。
const (
	taskNamePrefix         = namePrefix + "task-"
	taskGroupNetworkPrefix = namePrefix + "taskgroup-"
)

// TaskServiceName 返回任务承载的 Swarm 服务名 `fleetly-task-<taskID>`
// （DT-5：稳定 DNS 名 = 服务名，任务实例免 inspect 寻址）。
func TaskServiceName(taskID string) (string, error) {
	if err := validateResourceID("task", taskID); err != nil {
		return "", err
	}
	return taskNamePrefix + taskID, nil
}

// IsTaskServiceName 报告 Swarm 服务名是否为任务服务（前缀 + 全量 ULID 尾段
// 形态——前缀族识别谓词，IsProjectNetworkName 同款纪律）。
func IsTaskServiceName(name string) bool {
	rest, ok := strings.CutPrefix(name, taskNamePrefix)
	if !ok || rest == "" {
		return false
	}
	return validateResourceID("task", rest) == nil
}

// TaskGroupNetworkName 返回 task-group 网络名 `fleetly-taskgroup-<ref>`
// （DT-5：task-group 网长活，经幂等 EnsureTaskNetwork 创建；任务只按名加入）。
func TaskGroupNetworkName(ref string) (string, error) {
	if err := ValidateTaskGroupRef(ref); err != nil {
		return "", err
	}
	return taskGroupNetworkPrefix + ref, nil
}

// IsTaskGroupNetworkName 报告网络名是否为 task-group 网络（前缀 + 合法 ref
// 尾段；前缀族识别谓词）。
func IsTaskGroupNetworkName(name string) bool {
	rest, ok := strings.CutPrefix(name, taskGroupNetworkPrefix)
	if !ok || rest == "" {
		return false
	}
	return ValidateTaskGroupRef(rest) == nil
}

// ValidateTaskGroupRef 校验 task-group ref：小写字母数字 2-32 位、不含 '-'
// （与三段 app 网名结构不相交的前提；见前缀族注）。
func ValidateTaskGroupRef(ref string) error {
	if len(ref) < 2 || len(ref) > 32 {
		return fmt.Errorf("naming: task-group ref %q must be 2-32 characters", ref)
	}
	for _, r := range ref {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return fmt.Errorf("naming: task-group ref %q contains character %q outside [a-z0-9]", ref, r)
		}
	}
	return nil
}

// validateResourceID 校验平台 ULID 形态（26 位 Crockford 大写字母+数字；
// validateProjectID 的泛化形——任务/项目同用平台主键口径）。
func validateResourceID(field, id string) error {
	if id == "" {
		return fmt.Errorf("naming: %s id is empty", field)
	}
	if len(id) != 26 {
		return fmt.Errorf("naming: %s id %q must be a 26-char ULID", field, id)
	}
	for _, r := range id {
		isCrockford := (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')
		if !isCrockford {
			return fmt.Errorf("naming: %s id %q contains character %q outside the Crockford alphabet", field, id, r)
		}
	}
	return nil
}

// cronJobNamePrefix 是一次性 cron job 服务名的固定前缀（E5 Cron）：完整名
// `fleetly-cron-<team>-<prj>-<app>-<service>-<ulid8>`。前缀隔离了长驻服务的
// `fleetly-<team>-<prj>-<app>-<service>` 命名空间——引擎对账（省略=删除的
// 删除扫描）与启动残留收口都以该前缀识别 job 服务的瞬时性（在途 job 不得
// 被发布对账误删；ulid8 使同 schedule 的重叠触发命名天然不冲突）。
const cronJobNamePrefix = namePrefix + "cron-"

// CronJobName 返回一次性 cron job 的 Swarm 服务名
// `fleetly-cron-<team>-<prj>-<app>-<service>-<ulid8>`（ulid8 = 触发 run 的
// ULID 前 8 位——同 schedule 串行〔max-concurrent 1〕下的唯一性兜底；前缀
// 族不变，IsCronJobName 沿用，rbac-teams §4.3 cron 行）。
func CronJobName(team, prj, app, service, runID string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	if err := validateComponent("service", service); err != nil {
		return "", err
	}
	if len(runID) < 8 {
		return "", fmt.Errorf("naming: run id %q shorter than 8 chars", runID)
	}
	return cronJobNamePrefix + team + "-" + prj + "-" + app + "-" + service + "-" + runID[:8], nil
}

// IsCronJobName 报告 Swarm 服务名是否为一次性 cron job 服务（引擎对账的
// 删除扫描与残留收口按此前缀排除/识别瞬时 job 服务；前缀族 v0.3 不变）。
func IsCronJobName(name string) bool {
	return strings.HasPrefix(name, cronJobNamePrefix)
}

// initJobNamePrefix 是部署期 init job 服务名的固定前缀（DT-4）：完整名
// `fleetly-init-<team>-<prj>-<app>-<service>-<deployid8>`。**独立于 cron
// 前缀族**——一次性 job 服务的瞬时性必须可按名识别：cron 孤儿清扫
// （internal/cron sweepOrphanJobs 只认 fleetly-cron-）、引擎对账（删除扫描
// 与漂移 Extra 判定按此前缀豁免）、MoveApp 摘旧名（在途 job 让位）与日志
// 管线（JobServiceStates 两族并列）都按前缀/label 边界识别；与 cron 共享
// 前缀会被对方的清扫误伤（在途 init job 被当 cron 残留删除 = 迁移静默
// 丢失）。deployid8 = deployment ID 前 8 位——同一部署跨 tick/重启命名
// 确定（创建幂等、续跑可寻址；cron 的 ulid8 是「每次触发一服务」，init
// 是「每次部署一服务」）。
const initJobNamePrefix = namePrefix + "init-"

// InitJobName 返回部署期 init job 的 Swarm 服务名
// `fleetly-init-<team>-<prj>-<app>-<service>-<deployid8>`（DT-4）。
func InitJobName(team, prj, app, service, deploymentID string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("app", app); err != nil {
		return "", err
	}
	if err := validateComponent("service", service); err != nil {
		return "", err
	}
	id8, err := instanceID8(deploymentID)
	if err != nil {
		return "", err
	}
	return initJobNamePrefix + team + "-" + prj + "-" + app + "-" + service + "-" + id8, nil
}

// IsInitJobName 报告 Swarm 服务名是否为部署期 init job 服务（对账删除
// 豁免、漂移 Extra 豁免、MoveApp 让位与孤儿清扫的瞬时性识别谓词，
// IsCronJobName 同款纪律）。
func IsInitJobName(name string) bool {
	return strings.HasPrefix(name, initJobNamePrefix)
}

// 库族命名公式（E4 数据库托管，managed-databases §5.4 文档锚 + rbac-teams
// §4.3 库行 v0.3 三段化；与 app 名族 `fleetly-<team>-<prj>-<app>-*` 解耦的
// 独立前缀族，库实例与 app 可重名、对象不撞，§2.1 名字空间独立）：
//
//	库服务名   fleetly-db-<team>-<prj>-<name>-<service>
//	库网络名   fleetly-db-<team>-<prj>-<name>-net（每实例专属共享 overlay）
//	库卷名     fleetly-db-<name>-<key>-<id8>     （v0.3 不变；id8 = 实例 ID
//	                                             前 8，天然全局防撞）
//	库 secret 名 fleetly-db-<team>-<prj>-<name>-<secret>-<hash8>
//
// 前缀族常量 dbNamePrefix 同时是引擎对账的识别面（IsDbServiceName——库
// 服务不受发布对账「省略=删除」扫描管辖，后续阶段消费）。
const (
	// dbNamePrefix 是库族对象名的固定前缀（fleetly-db-）。
	dbNamePrefix = namePrefix + "db-"
)

// DBServiceName 返回库服务名 `fleetly-db-<team>-<prj>-<name>-<service>`
// （name = 库实例名〔project 内唯一〕，service = 模板服务名——模板渲染层
// 供给，internal/dbtemplate）。
func DBServiceName(team, prj, name, service string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("name", name); err != nil {
		return "", err
	}
	if err := validateComponent("service", service); err != nil {
		return "", err
	}
	return dbNamePrefix + team + "-" + prj + "-" + name + "-" + service, nil
}

// DBNetworkName 返回库实例专属共享 overlay 网络名
// `fleetly-db-<team>-<prj>-<name>-net`（库服务挂该网络、别名 = 实例名；
// 引用方服务部署时由平台附加挂载）。
func DBNetworkName(team, prj, name string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("name", name); err != nil {
		return "", err
	}
	return dbNamePrefix + team + "-" + prj + "-" + name + "-net", nil
}

// DBVolumeName 返回库数据卷名 `fleetly-db-<name>-<key>-<id8>`（**v0.3 公式
// 不变**——rbac-teams §4.3 库卷行：id8 = 库实例平台 ID 前 8 位天然全局防撞，
// 零卷迁移；与 app 卷同纪律）。
func DBVolumeName(name, key, instanceID string) (string, error) {
	if err := validateComponent("name", name); err != nil {
		return "", err
	}
	if err := validateComponent("key", key); err != nil {
		return "", err
	}
	id8, err := instanceID8(instanceID)
	if err != nil {
		return "", err
	}
	return dbNamePrefix + name + "-" + key + "-" + id8, nil
}

// DBSecretName 返回库 secret 名 `fleetly-db-<team>-<prj>-<name>-<secret>-<hash8>`
// （hash8 = 值 sha256 前 8——值轮换即换名换引用，app secret 同纪律）。
func DBSecretName(team, prj, name, secret, hash8 string) (string, error) {
	if err := validateComponent("team", team); err != nil {
		return "", err
	}
	if err := validateComponent("prj", prj); err != nil {
		return "", err
	}
	if err := validateComponent("name", name); err != nil {
		return "", err
	}
	if err := validateComponent("secret", secret); err != nil {
		return "", err
	}
	if err := validateHash8(hash8); err != nil {
		return "", err
	}
	return dbNamePrefix + team + "-" + prj + "-" + name + "-" + secret + "-" + hash8, nil
}

// IsDbServiceName 报告 Swarm 服务名是否为库族服务（fleetly-db- 前缀——
// 引擎对账的删除扫描与 reconcile 按此前缀豁免库服务：库收敛器自管，
// 不受发布对账「省略=删除」管辖；前缀族 v0.3 不变）。
func IsDbServiceName(name string) bool {
	return strings.HasPrefix(name, dbNamePrefix)
}

// dbJobNamePrefix 是库备份/恢复/校验一次性 job 服务名的固定前缀（E4 S5，
// cron 前缀纪律同款）：完整名 `fleetly-dbjob-<instance>-<purpose>-<ulid8>`。
// **独立于库服务前缀 fleetly-db-**——一次性 job 服务的瞬时性必须可按名
// 识别：cron 孤儿清扫（internal/cron sweepOrphanJobs 只认 fleetly-cron-）、
// 引擎对账（按 app label 列举，job 服务不带 app label）、库收敛拍（只
// inspect 实例自己的服务名）与日志管线（CronJobServiceStates 只认
// fleetly-cron-）都按前缀/label 边界忽略它——前缀不独立会被某一方的
// 清扫误伤（在途备份 job 被删 = 备份静默丢失）。ulid8 使同实例重叠触发
// 的命名天然不冲突（重叠受理由操作互斥哨兵拒绝，此处只兜底）。
//
// v0.3 注：dbjob 前缀族不在 rbac-teams §4.3 对照表内（不在三段化清单）——
// job 服务是瞬时对象（创建即追踪、完成即删除），实例段重复只发生在「不同
// 项目同名实例」之间且 ulid8 尾缀天然互异，无碰撞面；公式保持不变（最保守
// 改动面）。
const dbJobNamePrefix = namePrefix + "dbjob-"

// DBJobName 返回库一次性 job 的 Swarm 服务名
// `fleetly-dbjob-<instance>-<purpose>-<ulid8>`（purpose = backup/verify/
// restore/prune/rotate 语义段——rotate 为 IMPL-ARCH-F F-2 起凭据轮换 job
// 的归宿，此前误落 fleetly-db- 库服务族；ulid8 = run ULID 前 8 位）。
func DBJobName(instance, purpose, runID string) (string, error) {
	if err := validateComponent("instance", instance); err != nil {
		return "", err
	}
	if err := validateComponent("purpose", purpose); err != nil {
		return "", err
	}
	if len(runID) < 8 {
		return "", fmt.Errorf("naming: run id %q shorter than 8 chars", runID)
	}
	return dbJobNamePrefix + instance + "-" + purpose + "-" + runID[:8], nil
}

// IsDBJobName 报告 Swarm 服务名是否为库一次性 job 服务（清扫/采集面的
// 瞬时性识别谓词，CronJobName 同款纪律）。
func IsDBJobName(name string) bool {
	return strings.HasPrefix(name, dbJobNamePrefix)
}

// instanceID8 返回资源平台 ID 的前 8 位（库卷命名尾缀；AppID8 的泛化形
// ——ULID 前 8 位已含高精度时间戳成分）。
func instanceID8(id string) (string, error) {
	if len(id) < 8 {
		return "", fmt.Errorf("naming: instance id %q shorter than 8 chars", id)
	}
	return id[:8], nil
}

// AppID8 返回 app 平台 ID 的前 8 位（卷命名防撞尾缀）。ULID 为 26 位，
// 前 8 位已含高精度时间戳成分。
func AppID8(appID string) (string, error) {
	if len(appID) < 8 {
		return "", fmt.Errorf("naming: app id %q shorter than 8 chars", appID)
	}
	return appID[:8], nil
}

// Hash8 返回内容 sha256 hex 前 8 位（secret 命名尾缀：值轮换即换名）。
func Hash8(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:8]
}

// reservedTeamSlugReasons 是 team slug 的保留字清单（rbac-teams §4.3 保留
// 字迁移：v0.2 的 8 个 app 名保留字整体迁到 team slug——v0.3 命名公式以
// team slug 为第一参数，app 名从此不再紧邻 `fleetly-` 前缀〔结构性安全，
// E_APP_NAME_RESERVED 退役〕；team slug 顶到该位置必须守前缀族与平台固定
// 名，E_TEAM_SLUG_RESERVED 消费本清单）。project 段居第二位不邻前缀族
// （`fleetly-acme-db-…` 撞不上 `fleetly-db-`）——project slug 不新增保留
// 约束。审计证据链（撞键面逐条实证，W3 撞键票 2026-09-21 + D-W0-4 迁移）：
//
//	cron          前缀族：team slug="cron" 时服务名 fleetly-cron-<prj>-… 命中
//	               IsCronJobName——cron 孤儿清扫会把长驻服务当一次性 job 删除
//	               （破坏性撞键，最高优先）；
//	init          前缀族：team slug="init" 时服务名 fleetly-init-<prj>-… 命中
//	               IsInitJobName——对账删除/漂移 Extra 对长驻服务静默豁免
//	               （服务删除被吞），init 孤儿清扫反向误删在途长驻任务；
//	db            前缀族：fleetly-db-<prj>-… 命中 IsDbServiceName——发布对账
//	               「省略=删除」扫描豁免库服务，服务删除被静默吞掉；
//	dbjob         前缀族：fleetly-dbjob-… 命中 IsDBJobName——清扫/采集面把
//	               长驻服务隐形成瞬时 job；
//	rustfs        网络名：NetworkName("rustfs",…) 无 team 段固定名形
//	               fleetly-rustfs-net 与 state.RustfsNetworkName 同串——用户
//	               app 直挂平台 RustFS 网络（隔离面击穿）；另撞 s3 公网路由键
//	               与 rustfs secret 名族（同构指纹公式）；
//	registry      路由键：与平台 registry 路由段（Name 覆写 fleetly-registry）
//	               的键空间同形冲突——多节点 registry 模式启用即触发；
//	acme          路由键：与 ACME 挑战路由键 fleetly-acme-challenge 同串
//	               （挑战期配置互相覆盖）；
//	metrics       网络名：与 metrics 三件套内部 overlay fleetly-metrics-net
//	               同串（opt-in 启用即撞）；
//	victorialogs  网络名：与日志库内部 overlay fleetly-victorialogs-net 同串
//	               （默认捆绑组件）。
//
// 不入清单的近名（ingress/exec/system/console/ctrl/victoriametrics/
// cadvisor/node-exporter 等）：审计证明其服务/网络/路由三面均撞不上
// （固定名无后缀段、固定网络名带 -net/-system 尾且无同形公式产物、固定
// 服务名不进动态配置键空间）——保留字最小化，不预防性扩列。
//
// project（OT-1 项目网前缀族 `fleetly-project-<projectID>`，IMPL-T15-1）：team
// slug=project 时 app 网名为 `fleetly-project-<prj>-<app>-net`（小写 + '-'），
// 项目网名为前缀 + 26 位 Crockford（全量项目 ID，大写字母+数字）——两族
// 结构不相交，无需保留（naming_test.TestProjectNetworkNameCannotCollideWithAppNetwork
// 钉死）。
var reservedTeamSlugReasons = map[string]string{
	"cron":         "IsCronJobName prefix family (the cron orphan sweep would delete the team's long-running services as transient jobs)",
	"init":         "IsInitJobName prefix family (reconcile-deletion and drift-extra exemptions would silently spare the team's long-running services, and the init orphan sweep would delete their in-flight tasks)",
	"db":           "IsDbServiceName prefix family (publish reconcile exempts the team's services from omission-means-deletion)",
	"dbjob":        "IsDBJobName prefix family (sweeps/collection treat the team's services as transient jobs)",
	"rustfs":       "collides with the managed RustFS overlay network fleetly-rustfs-net (state.RustfsNetworkName), the s3 public route keys and the rustfs secret name family",
	"registry":     "collides with the platform registry route keys fleetly-registry-web/-websecure (router key overwrite in Synthesize)",
	"acme":         "collides with the ACME challenge router/service key fleetly-acme-challenge",
	"metrics":      "collides with the managed metrics overlay network fleetly-metrics-net",
	"victorialogs": "collides with the managed VictoriaLogs overlay network fleetly-victorialogs-net",
	// taskgroup（T 线 DT-5 / IMPL-T2-1）：team slug=taskgroup 时 app 网名
	// `fleetly-taskgroup-<prj>-<app>-net` 落进 task-group 网络前缀族空间。
	// task-group ref 禁止 '-'（naming.ValidateTaskGroupRef），两族结构本不
	// 相交——本保留字是纵深防御（防未来 ref 字符集放宽时静默合并）。
	"taskgroup": "collides with the task-group network prefix family fleetly-taskgroup-<ref> (DT-5 dynamic workloads)",
}

// IsReservedTeamSlug 报告 team slug 是否与平台组件命名空间撞键（团队受理
// 前置校验，E_TEAM_SLUG_RESERVED——naming 单点，api 层消费）。
func IsReservedTeamSlug(slug string) bool {
	_, ok := reservedTeamSlugReasons[slug]
	return ok
}

// ReservedTeamSlugs 返回保留字清单的字典序副本（错误文案与测试面）。
func ReservedTeamSlugs() []string {
	out := make([]string, 0, len(reservedTeamSlugReasons))
	for k := range reservedTeamSlugReasons {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReservedTeamSlugReason 返回单个保留 slug 的撞键证据（错误文案/审计引用；
// 未知名返回空串）。
func ReservedTeamSlugReason(slug string) string {
	return reservedTeamSlugReasons[slug]
}

// joinName 以 '-' 连接命名成分。
func joinName(parts ...string) string {
	return namePrefix + strings.Join(parts, "-")
}

// validateComponent 校验命名成分：非空、不含命名分隔符 '-' 之外的非法字符
// 以外交界字符（compose 标识符字符集 [A-Za-z0-9._-]），防止注入分隔层级或
// 产生非法 DNS 名。
func validateComponent(field, v string) error {
	if v == "" {
		return fmt.Errorf("naming: %s is empty", field)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("naming: %s %q contains character %q outside [A-Za-z0-9._-]", field, v, r)
		}
	}
	return nil
}

// validateHash8 校验 hash8 形态（8 位小写 hex）。
func validateHash8(h string) error {
	if len(h) != 8 {
		return fmt.Errorf("naming: hash8 %q must be 8 hex chars", h)
	}
	for _, r := range h {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return fmt.Errorf("naming: hash8 %q must be lowercase hex", h)
		}
	}
	return nil
}
