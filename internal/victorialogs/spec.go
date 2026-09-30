package victorialogs

// 托管 VictoriaLogs 服务的期望 spec 构造与幂等比对（E6 观测专项设计 §2.1，
// W5-S1；internal/rustfs spec 同款纪律）：不存在创建、存在比对（镜像/参数/
// 挂载/约束/副本/限额）漂移即更新。
//
// 部署形态（设计 §2.1）与一处**已记录的实现偏离**：
//   - swarm service `fleetly-victorialogs`，单副本钉 manager（日志采集单写
//     点 = hub 所在控制面，数据重力同源）；卷 `fleetly-victorialogs-data`；
//     内存限额起步 128MB（FZ-9 实测校准门挂账）；
//   - **宿主可达（D-W5-4 回环不变量）**：设计字面 = host-mode 端口发布 +
//     PortConfig.HostIP=127.0.0.1。实现时点真机实证（docker 29.8.1 dind，
//     2026-09-21）：Engine API 对 swarm 服务端口配置的 HostIp 字段**静默
//     丢弃**（moby/moby/api v1.56.0 的 swarm.PortConfig 亦无该字段；上游
//     master 同），host-mode 发布实测绑 `*:9428`——**基座不支持按 IP 发布
//     swarm 服务端口**。故取保守等价形态：**服务任务挂 host 网络 +
//     `-httpListenAddr=127.0.0.1:9428`（VL 原生监听地址 flag）**——进程只
//     绑宿主回环，回环不变量（零公网面）比设计字面更严格地成立；hub 直推
//     与 SearchLogs 查询照走 `127.0.0.1:9428`（实现者汇总「设计偏离点」
//     条目一，含取证命令）。附带效应：host 网络任务不能同时挂 overlay，
//     `fleetly-victorialogs-net`（设计「为后续受管消费方留位」）本阶段
//     不创建——留位网络的可达性语义依赖该访问面裁决，随设计修订票补。
//   - VL 无凭据面（回环 + 无认证端点不暴露），不注入任何 secret。
//
// 参数（设计 §2.1）：`-storageDataPath=<卷挂载点>`；`-retentionPeriod`
// 对齐 config 键 logs.retention_days（一窗两载体同口径，缺省 7d）。
// LogsQL 无需额外 flag。

import (
	"fmt"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/componentloop"
	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台 victorialogs 常量（设计 §2.1 形态表；不走配置面）。
const (
	// ServiceName 是托管 VictoriaLogs 服务名（集群全局命名空间）。
	ServiceName = "fleetly-victorialogs"
	// VolumeName 是数据本地命名卷（钉 manager——数据重力；禁用保留，
	// 再启用复用；设计 §2.5：VL 数据不进 state_backups，卷损 = 丢失
	// 保留窗内的日志检索面，不损平台状态）。
	VolumeName = "fleetly-victorialogs-data"
	// vlLabel 是服务自描述 label（CLI/运维识别面）。
	vlLabel = "fleetly.victorialogs"
	// dataMountPath 是数据目录挂载点（-storageDataPath 指向它）。
	dataMountPath = "/vlstorage"
	// IngestPort 是 HTTP API 端口（VL 缺省 9428：ingest/query/health 同
	// 端口；host 网络 + 回环监听的目标地址）。
	IngestPort = 9428
	// hostIP 是回环监听绑定地址（D-W5-4：127.0.0.1 = 零公网面）。
	hostIP = "127.0.0.1"
	// memoryLimitBytes 是内存限额起步值（设计 §2.1：128MB，实测校准
	// 门 FZ-9 挂账——先测 idle 再定）。
	memoryLimitBytes = int64(128) << 20

	// retryInterval 是 收敛失败的退避缺省（rustfs 管理器同款注入缝）。
	retryInterval = 30 * time.Second
	// scanInterval 是已收敛后的漂移复检周期。
	scanInterval = 60 * time.Second
)

// DefaultVictoriaLogsImage 是托管 VictoriaLogs 的钉定镜像（钉 release tag
// 而非 latest——R7：可变 tag 是供应链反面教材；多架构 OCI index digest，
// amd64/arm64 通吃）。选版：2026-09-21 解析，v1.52.0 为实现时点最新稳定
//（GitHub releases 2026-07-16 发布，latest tag 当前所指同 digest——两侧
// pull 解析 digest 一致）。取证：`docker buildx imagetools inspect
// victoriametrics/victoria-logs:v1.52.0` → Digest sha256:47b82089…，
// 台账 docs/runbooks/image-prepull.md #14。升级 = 镜像钉版换版票
//（digest + 台账 + 回归），不自动追新。
//
// 注：设计文档字面写作 `victorialogs/victoria-logs`——该 repo 在 Docker
// Hub 不存在（拉取 denied）；官方发布 repo 是 `victoriametrics/victoria-logs`
//（VictoriaMetrics/VictoriaLogs 项目镜像）。按实现时点核实取官方 repo，
// 偏离已记录（实现者汇总「设计偏离点」条目二）。
const DefaultVictoriaLogsImage = "victoriametrics/victoria-logs:v1.52.0@sha256:47b820890d64c4575a2a0a46415dcd8a4fd59a0f1fcd6a377693d7aea639442e"

// HealthPath / ingest/query 路径常量（VL v1.52 实测核实，2026-09-21）：
// GET /health → 200 "OK"；POST /insert/elasticsearch/_bulk（ES bulk 形态，
// 设计原文「ES bulk」）；GET/POST /select/logsql/query（LogsQL）。
// 设计文档字面 `/insert/elasticbulk` 在 v1.52 无此端点——按 VL 实测规范
// 取 `_bulk` 端点（实现者汇总「设计偏离点」条目三）。
const (
	HealthPath = "/health"
	// BulkIngestPath 是 ES bulk 入湖端点（_stream_fields=app,service,source
	// 查询参数由批量器拼接——流字段词表是管线契约）。
	BulkIngestPath = "/insert/elasticsearch/_bulk"
	// QueryPath 是 LogsQL 查询端点。
	QueryPath = "/select/logsql/query"
)

// streamFields 是入湖流字段词表（设计 §2.3：_stream_fields={app,service,
// source}——流粒度 = app × service × source，检索面的服务/来源过滤走
// 流过滤）。DT-5 起增 task（任务日志面：任务行 app/service 为空，task =
// 任务平台 ID，流标签即归因面）。单一事实源在本包；internal/logs 批量器
// 经本包消费。
const streamFields = "app,service,source,task"

// constraintFor 是 manager 钉定约束（与 rustfs/ingress 同公式：node.
// labels.<LabelNodeID> == <platformID>；本地重写避免适配器反向依赖，
// 公式由测试钉死）。
func constraintFor(platformNodeID string) string {
	return "node.labels." + state.LabelNodeID + " == " + platformNodeID
}

// retentionArg 把保留天数翻译为 VL 参数值（-retentionPeriod 对齐 config
// 键 logs.retention_days；`d` 后缀天数形态——VL 接受 `<n>d`/`<n>`(月)/
// `<n>y`，天数形态最直读）。
func retentionArg(days int) string {
	if days <= 0 {
		days = 7
	}
	return fmt.Sprintf("-retentionPeriod=%dd", days)
}

// buildSpec 构造托管 VictoriaLogs 服务的期望 swarm spec（replicated-1 +
// manager 约束 + host 网络任务（回环监听——D-W5-4 不变量的基座可行承载
// 形态，见文件头注记）+ 数据卷 + 内存限额 128MB；无端口发布面——host
// 网络任务的进程自绑 127.0.0.1，EndpointSpec 恒空）。
func buildSpec(platformID string, retentionDays int) swarm.ServiceSpec {
	one := uint64(1)
	mem := memoryLimitBytes
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: ServiceName,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				vlLabel:            "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: DefaultVictoriaLogsImage,
				Args: []string{
					"-storageDataPath=" + dataMountPath,
					retentionArg(retentionDays),
					listenArg(),
				},
				Mounts: []mount.Mount{
					{Type: mount.TypeVolume, Source: VolumeName, Target: dataMountPath},
				},
			},
			// host 网络：任务共享宿主网络命名空间，-httpListenAddr 把监听
			// 面收缩到宿主回环（零公网面）；网络附件恒为单 host 形态。
			Networks: []swarm.NetworkAttachmentConfig{{
				Target: "host",
			}},
			Placement: &swarm.Placement{
				Constraints: []string{constraintFor(platformID)},
			},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: mem},
			},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			FailureAction: "pause",
			Order:         "stop-first",
		},
	}
	return spec
}

// listenArg 构造回环监听参数（单一事实源——spec 渲染与注释/测试同锚）。
func listenArg() string {
	return fmt.Sprintf("-httpListenAddr=%s:%d", hostIP, IngestPort)
}

// specEqual 幂等比对（镜像/参数/挂载/网络/约束/副本/限额——服务的全部
// 执行面都由期望 spec 权威表达；label 不参与，服务名即身份。参数含
// -httpListenAddr 回环监听——零公网面不变量漂移必被本比对捕获）。
func specEqual(cur dockerapi.ServiceSnapshot, desired swarm.ServiceSpec) bool {
	cs := desired.TaskTemplate.ContainerSpec
	if cur.Image != cs.Image {
		return false
	}
	if !componentloop.SameStrings(cur.Args, cs.Args) {
		return false
	}
	if len(cur.Mounts) != len(cs.Mounts) {
		return false
	}
	for i, wm := range cs.Mounts {
		cm := cur.Mounts[i]
		if cm.Source != wm.Source || cm.Target != wm.Target {
			return false
		}
	}
	wantNets := make([]string, 0, len(desired.TaskTemplate.Networks))
	for _, n := range desired.TaskTemplate.Networks {
		wantNets = append(wantNets, n.Target)
	}
	if !componentloop.SameStrings(cur.Networks, wantNets) {
		return false
	}
	wantConstraints := []string{}
	if pl := desired.TaskTemplate.Placement; pl != nil {
		wantConstraints = pl.Constraints
	}
	if !componentloop.SameStrings(cur.Constraints, wantConstraints) {
		return false
	}
	wantReplicas := uint64(0)
	if desired.Mode.Replicated != nil && desired.Mode.Replicated.Replicas != nil {
		wantReplicas = *desired.Mode.Replicated.Replicas
	}
	if cur.Replicas != wantReplicas {
		return false
	}
	wantMem := int64(0)
	if res := desired.TaskTemplate.Resources; res != nil && res.Limits != nil {
		wantMem = res.Limits.MemoryBytes
	}
	return cur.MemoryBytes == wantMem
}
