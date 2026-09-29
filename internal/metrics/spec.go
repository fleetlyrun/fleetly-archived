package metrics

// 托管 metrics 三件套（VictoriaMetrics 单机版 + cAdvisor + node_exporter）
// 的期望 spec 构造与幂等比对（E6 观测专项设计 §4.1，W5-S3；D-W5-2 opt-in；
// internal/victorialogs spec 同款纪律）：不存在创建、存在比对（镜像/参数/
// 挂载/网络/约束/副本/限额/抓取配置引用）漂移即更新。
//
// 部署拓扑（设计 §4.1 + §6 挂账票「跨节点 metrics 采集修订」的 v0.2.x S1
// 收口，2026-09-23；两处**已记录的实现偏离**，D-W5-4 同族取证）：
//   - 设计字面 = VM host-mode 端口发布回环 8428 + cAdvisor/node_exporter 挂
//     内部 overlay 网络 `fleetly-metrics-net` + VM 抓 `tasks.fleetly-*`
//     （overlay DNS RR）。实现时点核实两处基座事实：
//     ①Engine API 对 swarm 服务端口的 HostIp 静默丢弃（W5-S1 dind 实证，
//     victorialogs spec 头注记同源）——host-mode 发布绑 *:8428 = 公网暴露；
//     ②host 网络任务不能挂 overlay（S1 已实证）——VM 在宿主网络命名空间
//     内既解析不了 `tasks.*`（Docker 内嵌 DNS 不服务宿主）也路由不到
//     overlay 任务 IP，抓取面为空。→ 三件全部 host 网络任务。
//   - W5-S3 落地形态（三件回环监听）只覆盖 manager 节点（worker 采集器
//     在位但不被抓，`N/M nodes reporting` 诚实披露）→ 本票修订（§6 挂账
//     原文方向）：**采集器改绑 0.0.0.0**（cAdvisor `-listen_ip=0.0.0.0
//     -port=8080`、node_exporter `--web.listen-address=0.0.0.0:9100`——
//     flag 名经官方镜像 -help 实测核实，2026-09-22），VM（钉 manager）经
//     各节点 advertise 地址直连抓取——**VPC/LAN TCP 直连，不依赖 overlay
//     数据面**（W3-F2 免疫；与 exec relay 反向常连同理）。
//   - **暴露面诚实口径（spec 注释 / `metrics status` note / Console 文案
//     三处同锚）**：采集端口对节点全部网络接口开放（含公网接口）——
//     **metrics 采集面 = 内网面，公网访问由节点/云防火墙负责**（与
//     traefik 80/443 的 host-mode 发布同级暴露，但无鉴权——依赖宿主防火
//     墙拦公网、VPC 对内互通）。VM 自身保持回环
//     （`-httpListenAddr=127.0.0.1:8428`——查询面只在 manager 本地，零
//     公网面不变量对 VM 依旧成立）。
//   - **动态抓取面**：duty 每拍从底座节点注册表读全部 Ready 节点的
//     advertise 地址（dockerPort.ReadyNodeAddresses——Status.Addr，缺省
//     回落 ManagerStatus.Addr；availability 非 active〔drain/pause〕的
//     节点不入选——global 采集器不在其上运行，抓了必 down），scrape
//     config 的 static_targets = 每节点 `<addr>:8080` + `<addr>:9100`。
//     节点集变化 → 配置内容 sha 变 → 内容寻址 config 名变 → 服务引用
//     比对触发 VM 滚动更新 → 旧 config GC（既有机制复用，内容从静态变
//     动态）。空 Ready 集 = 异常显式失败退避重试（上一版配置原地保持，
//     不闪断抓取面）。
//
// 抓取配置分发（设计 §4.1 的 `-prometheus.config` 内联形态不可实现——
// 实测 VM v1.152.0 无该 flag，单机版抓取配置 flag 是 **`-promscrape.
// config=<文件路径>`**，只吃文件/http URL 不吃内联 YAML；2026-09-22 镜像
// -help 取证）：落地 = **swarm config 对象**（内容寻址命名
// `fleetly-vm-scrape-<hash8>`，不可变；内容变更 = 新对象 + 服务 spec 引用
// 更新 + 旧对象 GC），挂载到任务内 `/etc/fleetly/vm-scrape.yml`。
//
// 数据安全语义（rustfs/victorialogs 同型）：mode=unset → 三件服务移除 +
// 旧抓取 config 清场；数据卷 **永不删除**（再启用复用）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台 metrics 常量（设计 §4.1 形态表；不走配置面）。
const (
	// VictoriaServiceName 是托管 VictoriaMetrics 单机版服务名。
	VictoriaServiceName = "fleetly-victoriametrics"
	// CAdvisorServiceName 是托管 cAdvisor 服务名（global——每节点一任务）。
	CAdvisorServiceName = "fleetly-cadvisor"
	// NodeExporterServiceName 是托管 node_exporter 服务名（global）。
	NodeExporterServiceName = "fleetly-node-exporter"
	// VMAlertServiceName 是托管 vmalert 服务名（metrics 栈第四组件，
	// alerts.mode=on opt-in——D-V3W5-1；replicated-1 钉 manager 与 VM 同族）。
	VMAlertServiceName = "fleetly-vmalert"
	// VolumeName 是 VM 数据本地命名卷（钉 manager——数据重力；禁用保留，
	// 再启用复用；VM 数据不进 state_backups，卷损 = 丢失保留窗内的指标，
	// 不损平台状态）。
	VolumeName = "fleetly-victoriametrics-data"
	// scrapeConfigPrefix 是抓取配置 swarm config 对象名前缀（后随
	// 内容 hash8——内容寻址，不可变对象按引用换版）。
	scrapeConfigPrefix = "fleetly-vm-scrape-"
	// rulesConfigPrefix 是 vmalert 规则 swarm config 对象名前缀（内容寻址
	// 同款——规则集变化即换版，设计 §2.2「scrape config 同款增删换」）。
	rulesConfigPrefix = "fleetly-vmalert-rules-"
	// scrapeLabel 是抓取配置对象的自描述 label（GC 选择器锚）。
	scrapeLabel = "fleetly.victoriametrics-scrape"
	// rulesConfigLabel 是规则配置对象的自描述 label（GC 选择器锚）。
	rulesConfigLabel = "fleetly.vmalert-rules"
	// vmLabel / cadvisorLabel / nodeExporterLabel / vmalertLabel 是各服务的
	// 自描述 label（CLI/运维识别面）。
	vmLabel           = "fleetly.victoriametrics"
	cadvisorLabel     = "fleetly.cadvisor"
	nodeExporterLabel = "fleetly.node-exporter"
	vmalertLabel      = "fleetly.vmalert"
	// dataMountPath 是 VM 数据目录挂载点（-storageDataPath 指向它）。
	dataMountPath = "/vmdata"
	// scrapeConfigMountPath 是抓取配置在任务内的挂载路径
	//（-promscrape.config 指向它）。
	scrapeConfigMountPath = "/etc/fleetly/vm-scrape.yml"
	// rulesConfigMountPath 是规则配置在任务内的挂载路径（-rule 的 glob
	// `/etc/vmalert/rules/*.yaml` 精确命中该文件）。
	rulesConfigMountPath = "/etc/vmalert/rules/rules.yaml"
	// notifierTokenMountPath 是 ingress token 文件在 vmalert 任务内的只读
	// 挂载点（-notifier.basicAuth.passwordFile 指向它——凭据材料不进服务
	// spec，token file 复用，设计 §2.3）。
	notifierTokenMountPath = "/etc/fleetly/notifier-token" //nolint:gosec // G101：token 文件挂载路径常量，非凭据
	// notifierBasicAuthUsername 是 vmalert → 接收器 Basic 认证的用户名
	//（常量非凭据；凭据 = 密码位的 ingress token）。
	notifierBasicAuthUsername = "fleetly"
	// QueryPort 是 VM HTTP API 端口（VM 缺省 8428：query/ingest/health 同
	// 端口；host 网络 + 回环监听的目标地址）。
	QueryPort = 8428
	// VMAlertPort 是 vmalert 自身 HTTP 面（UI/-help）端口（官方缺省 8880；
	// host 网络任务回环监听——零公网面不变量对第四组件同样成立）。
	VMAlertPort = 8880
	// DefaultEvaluationInterval 是规则求值周期（设计 §2.1：30s）。
	DefaultEvaluationInterval = 30 * time.Second
	// CAdvisorPort / NodeExporterPort 是采集器的监听端口（cAdvisor 官方
	// 缺省 8080、node_exporter 官方缺省 9100——端口未改只收编；绑定面
	// 0.0.0.0，抓取目标 = 节点 advertise 地址 + 本端口）。
	CAdvisorPort     = 8080
	NodeExporterPort = 9100
	// hostIP 是 VM/vmalert 查询面的回环绑定地址（D-W5-4：127.0.0.1 = 零公
	// 网面；采集器的绑定面见 bindAllIP）。
	hostIP = "127.0.0.1"
	// bindAllIP 是采集器（cAdvisor/node_exporter）的绑定地址（§6 挂账票
	// 修订：0.0.0.0 = 对节点全部网络接口开放——VM 经节点 advertise 地址
	// 直连抓取；**采集面 = 内网面，公网访问由节点/云防火墙负责**，见文件
	// 头诚实口径注记。VM/vmalert 不用此值——查询面保持回环）。
	bindAllIP = "0.0.0.0"
	// vmMemoryBytes 是 VM 内存限额起步值（设计 §4.1：128MB，实测校准门
	// 挂账——先测 idle 再定）。
	vmMemoryBytes = int64(128) << 20
	// vmalertMemoryBytes 是 vmalert 内存限额（设计 §2.1 opt-in 豁免预算的
	// 起步值——与 VM 同档 128MB；规则求值是无状态面，idle RSS 远低于此）。
	vmalertMemoryBytes = int64(128) << 20
	// cadvisorMemoryBytes 是 cAdvisor 内存限额起步值（设计 §4.1 注：128MB
	// 踩线、192MB 可接受——cAdvisor 是预算大头，限额保守但别 OOM 杀循环）。
	cadvisorMemoryBytes = int64(192) << 20
	// nodeExporterMemoryBytes 是 node_exporter 内存限额（idle RSS 极小，
	// 64MB 裕量充足）。
	nodeExporterMemoryBytes = int64(64) << 20

	// retryInterval 是 duty 收敛失败的退避缺省（victorialogs duty 同款注入缝）。
	retryInterval = 30 * time.Second
	// scanInterval 是已收敛后的漂移复检周期。
	scanInterval = 60 * time.Second
)

// DefaultVictoriaMetricsImage 是托管 VictoriaMetrics 单机版的钉定镜像
// （钉 release tag 而非 latest——R7；多架构 OCI index digest，amd64/arm64
// 通吃）。选版：2026-09-22 解析，v1.152.0 为实现时点最新稳定（2026-09-14
// 发布；v1.151.0 / v1.148.4 为老分支续版）。取证：`docker buildx imagetools
// inspect victoriametrics/victoria-metrics:v1.152.0` → Digest
// sha256:86ca5fdb…，台账 docs/runbooks/image-prepull.md #15。升级 = 镜像
// 钉版换版票（digest + 台账 + 回归），不自动追新。
const DefaultVictoriaMetricsImage = "victoriametrics/victoria-metrics:v1.152.0@sha256:86ca5fdb6d87d56ba047b044039019ba2bd9042b36e35f6ea34e437b6c825cef"

// DefaultCAdvisorImage 是托管 cAdvisor 的钉定镜像。**repo 勘误（台账 #17
// 同源记录）**：设计字面「docker.io 系 google/cadvisor」在 Docker Hub 已
// 标注 DEPRECATED（「New images will NOT be pushed. Please use
// gcr.io/cadvisor/cadvisor instead」——Hub repo 描述原文，2026-09-22 实测；
// google/cadvisor 最后镜像 v0.33.0 停在 2019 年）——官方多架构发布 repo 是
// `gcr.io/cadvisor/cadvisor`。选版：v0.55.1 为实现时点最新稳定（gcr.io
// tags/list 实测，v0.54.1 之上）；digest sha256:3de2bd52…（manifest list
// 多架构 index）。
const DefaultCAdvisorImage = "gcr.io/cadvisor/cadvisor:v0.55.1@sha256:3de2bd5203120b866d74a9b283b2ffb8ec382fbf9dc321814700c6ea6f44ec57"

// DefaultNodeExporterImage 是托管 node_exporter 的钉定镜像（prom/node-
// exporter 官方 repo；v1.12.1 为实现时点最新 stable，2026-07-14 发布；
// digest sha256:1b4e4438…，manifest list 多架构 index）。台账 #16。
const DefaultNodeExporterImage = "prom/node-exporter:v1.12.1@sha256:1b4e4438faca4dd7e001dd445d161a4a2091b0fededa84093b3a8dfeae1f1be0"

// DefaultVMAlertImage 是托管 vmalert（metrics 栈第四组件，alerts.mode=on
// opt-in，D-V3W5-1）的钉定镜像。组件与 VictoriaMetrics 单版同发同版号：
// v1.152.0 为实现时点最新 stable（GitHub releases 2026-09-14，prerelease=
// false，与台账 #15 单机版同日核实）；digest sha256:ba005663…（多架构 OCI
// index，amd64/arm64 等通吃，`docker buildx imagetools inspect` 实测）。
// 台账 #21。flag 取证（同日镜像 -help 实测）：notifier 认证 flag 的真实
// 形态是 **-notifier.basicAuth.username / -notifier.basicAuth.password**
//（及 *File 变体，array 型），设计 §2.1 字面「-notifier.basicAuthUsername/
// Password」为笔误缩写——实现取核实后的最小正确形态（passwordFile 形态，
// 凭据材料不进服务 spec）。
const DefaultVMAlertImage = "victoriametrics/vmalert:v1.152.0@sha256:ba00566373eb8c72d70cbee123e27ee75292dc0239830f36218c72712ba396b2"

// HealthPath / query 路径常量（VM v1.152 单机版 HTTP API；/health 返回
// "OK"）。查询面：GET /api/v1/query_range（区间）与 /api/v1/query（瞬时）。
const (
	HealthPath = "/health"
	// QueryRangePath 是 PromQL 区间查询端点（SearchMetrics 消费）。
	QueryRangePath = "/api/v1/query_range"
	// QueryInstantPath 是 PromQL 瞬时查询端点（nodes_reporting 计数消费）。
	QueryInstantPath = "/api/v1/query"
)

// constraintFor 是 manager 钉定约束（victorialogs/rustfs/ingress 同公式：
// node.labels.<LabelNodeID> == <platformID>；本地重写避免适配器反向依赖，
// 公式由测试钉死）。只用于 VM（单写点钉 manager）；cAdvisor/node_exporter
// 是 global 服务——每节点一任务，无约束。
func constraintFor(platformNodeID string) string {
	return "node.labels." + state.LabelNodeID + " == " + platformNodeID
}

// retentionArg 把保留天数翻译为 VM 参数值（-retentionPeriod 对齐 config
// 键 metrics.retention_days；`d` 后缀天数形态——VM 接受 s/h/d/w/M/y 后缀，
// `d` 形态最直读；缺省回落 14d——DefaultRetentionDays 的抄送面）。
func retentionArg(days int) string {
	if days <= 0 {
		days = DefaultRetentionDays
	}
	return fmt.Sprintf("-retentionPeriod=%dd", days)
}

// scrapeConfigYAML 构造 VM 抓取配置（确定性渲染——内容寻址命名的哈希基）：
// static_targets = 每个 Ready 节点 advertise 地址上的 cAdvisor/node_exporter
// （VPC/LAN 直连——采集器绑 0.0.0.0；跨节点拓扑见文件头注记）。地址序经
// scrapeAddrs 规范化（排序去重），同节点集恒渲染同内容。
func scrapeConfigYAML(readyAddrs []string) string {
	return `global:
  scrape_interval: 15s
scrape_configs:
  - job_name: fleetly-cadvisor
    static_configs:
      - targets: ` + targetsBlock(readyAddrs, CAdvisorPort) + `
  - job_name: fleetly-node-exporter
    static_configs:
      - targets: ` + targetsBlock(readyAddrs, NodeExporterPort) + `
`
}

// targetsBlock 渲染一个 static_configs 的 targets 行（host:port 逗号列表；
// 空地址集渲染空列表——converge 对空集先行短路报错，本形态仅供单测矩阵
// 与渲染完备性）。
func targetsBlock(addrs []string, port int) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, fmt.Sprintf("%q", fmt.Sprintf("%s:%d", a, port)))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// scrapeAddrs 把 Ready 节点地址集规范化为确定性渲染序：排序（底座清单
// 顺序不保证——同节点集必须恒定渲染同内容，内容寻址才稳）+ 去重（同址
// 双登记的异常形态不放大进抓取配置）。
func scrapeAddrs(addrs []string) []string {
	seen := make(map[string]struct{}, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a == "" {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// scrapeConfigName 是抓取配置的 swarm config 对象名（内容寻址：内容变 =
// 名变 = 新对象；服务 spec 以名引用，specEqual 比对捕获漂移）。
func scrapeConfigName(readyAddrs []string) string {
	sum := sha256.Sum256([]byte(scrapeConfigYAML(readyAddrs)))
	return scrapeConfigPrefix + hex.EncodeToString(sum[:4])
}

// buildScrapeConfigSpec 构造抓取配置的期望 swarm.ConfigSpec。
func buildScrapeConfigSpec(readyAddrs []string) swarm.ConfigSpec {
	return swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name: scrapeConfigName(readyAddrs),
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				scrapeLabel:        "true",
			},
		},
		Data: []byte(scrapeConfigYAML(readyAddrs)),
	}
}

// buildVictoriaSpec 构造托管 VictoriaMetrics 服务的期望 swarm spec
//（replicated-1 + manager 约束 + host 网络任务（回环监听——查询面只在
// manager 本地）+ 数据卷 + 抓取配置引用（按 Ready 节点集内容寻址）+ 内存
// 限额 128MB；无端口发布面——host 网络任务的进程自绑 127.0.0.1，
// EndpointSpec 恒空）。
func buildVictoriaSpec(platformID string, retentionDays int, readyAddrs []string) swarm.ServiceSpec {
	one := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: VictoriaServiceName,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				vmLabel:            "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: DefaultVictoriaMetricsImage,
				Args: []string{
					"-storageDataPath=" + dataMountPath,
					retentionArg(retentionDays),
					fmt.Sprintf("-httpListenAddr=%s:%d", hostIP, QueryPort),
					"-promscrape.config=" + scrapeConfigMountPath,
				},
				Mounts: []mount.Mount{
					{Type: mount.TypeVolume, Source: VolumeName, Target: dataMountPath},
				},
				Configs: []*swarm.ConfigReference{{
					ConfigName: scrapeConfigName(readyAddrs),
					File: &swarm.ConfigReferenceFileTarget{
						Name: scrapeConfigMountPath,
						UID:  "0", GID: "0", Mode: 0o444,
					},
				}},
			},
			// host 网络：任务共享宿主网络命名空间，-httpListenAddr 把监听
			// 面收缩到宿主回环（查询面零公网面）；抓取出站 = 节点 advertise
			// 地址直连（动态 targets 见文件头注记）。
			Networks: []swarm.NetworkAttachmentConfig{{
				Target: hostNetworkName,
			}},
			Placement: &swarm.Placement{
				Constraints: []string{constraintFor(platformID)},
			},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: vmMemoryBytes},
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

// buildCAdvisorSpec 构造托管 cAdvisor 的期望 spec（global——每节点一任务；
// host 网络 + **0.0.0.0 绑定**（§6 挂账票修订——VM 经节点 advertise 地址
// 直连抓取；采集面 = 内网面、公网由节点防火墙负责，见文件头诚实口径）
// + 官方容器形态的宿主只读挂载 + 限额 192MB；无 overlay）。
func buildCAdvisorSpec() swarm.ServiceSpec {
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: CAdvisorServiceName,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				cadvisorLabel:      "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: DefaultCAdvisorImage,
				// 经 /bin/sh -c 自适应启动（2026-09-22 staging 实证补丁）：
				// containerd socket 的宿主形态有两种——dockerd 自管（dind：
				// /var/run/docker/containerd/containerd.sock）与系统 containerd
				//（多数宿主：/var/run/containerd/containerd.sock）。单值 flag
				// 无法在一枚 global spec 里覆盖两种宿主，启动期按存在性择一
				//（/var/run 已只读挂载，两种路径容器内均可见——staging 实证：
				// 系统形态 socket 在、私有路径缺；dind 恰反）。镜像基于 alpine
				// 带 /bin/sh（健康检查 CMD-SHELL 同依赖）。
				Command: []string{"/bin/sh", "-c"},
				Args: []string{
					`exec /usr/bin/cadvisor -logtostderr -listen_ip=` + bindAllIP +
						` -port=` + strconv.Itoa(CAdvisorPort) +
						// 只报 docker 容器（+root）——cgroup 序列基数与
						// housekeeping 负载的保守化（预算门 §4.3 先手减载；
						// 平台全部负载都是 docker 容器，无序列损失）+ moby
						// 命名空间（dockerd 的容器都在 moby，cAdvisor 缺省读
						// k8s.io）。
						` -docker_only -containerd="$([ -S /var/run/docker/containerd/containerd.sock ] && echo /var/run/docker/containerd/containerd.sock || echo /var/run/containerd/containerd.sock)"` +
						` -containerd-namespace=moby`,
				},
				Mounts: []mount.Mount{
					bindRO("/", "/rootfs"),
					bindRO("/var/run", "/var/run"),
					bindRO("/sys", "/sys"),
					bindRO("/var/lib/docker", "/var/lib/docker"),
				},
				// 显式健康检查（覆盖镜像自带的 localhost 探针——镜像缺省
				// wget 在双栈解析下走 ::1，而 -listen_ip=127.0.0.1 只绑 IPv4
				// 回环 → 探针恒败 → swarm 按 unhealthy 杀任务重启循环；
				// 2026-09-22 dind 实证。绑定面放宽到 0.0.0.0 后 127.0.0.1
				// 探针依旧可达〔0.0.0.0 含回环〕——探针保持回环形态不变）。
				Healthcheck: &container.HealthConfig{
					Test: []string{
						"CMD-SHELL",
						fmt.Sprintf("wget --quiet --tries=1 --spider http://%s:%d/healthz || exit 1", hostIP, CAdvisorPort),
					},
					Interval:    30 * time.Second,
					Timeout:     3 * time.Second,
					StartPeriod: 10 * time.Second,
					Retries:     3,
				},
			},
			Networks: []swarm.NetworkAttachmentConfig{{
				Target: hostNetworkName,
			}},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: cadvisorMemoryBytes},
			},
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
	}
}

// buildNodeExporterSpec 构造托管 node_exporter 的期望 spec（global；host
// 网络 + **0.0.0.0 绑定**（同 cAdvisor——采集面 = 内网面）+ 官方容器形态
// 的宿主只读挂载与 --path.* 参数 + 限额 64MB）。
func buildNodeExporterSpec() swarm.ServiceSpec {
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: NodeExporterServiceName,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				nodeExporterLabel:  "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: DefaultNodeExporterImage,
				Args: []string{
					"--path.rootfs=/host",
					"--path.procfs=/host/proc",
					"--path.sysfs=/host/sys",
					fmt.Sprintf("--web.listen-address=%s:%d", bindAllIP, NodeExporterPort),
				},
				Mounts: []mount.Mount{
					bindRO("/", "/host"),
					bindRO("/proc", "/host/proc"),
					bindRO("/sys", "/host/sys"),
				},
			},
			Networks: []swarm.NetworkAttachmentConfig{{
				Target: hostNetworkName,
			}},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: nodeExporterMemoryBytes},
			},
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
	}
}

// bindRO 是宿主只读 bind 挂载的构造器（三件 spec 共用）。
func bindRO(source, target string) mount.Mount {
	return mount.Mount{Type: mount.TypeBind, Source: source, Target: target, ReadOnly: true}
}

// ── vmalert（metrics 栈第四组件，D-V3W5-1，W5-S2）─────────────────────────

// notifierConfig 是 vmalert → 平台内建接收器的投递面配置（装配点注入——
// runtime 提供 gateway 端口与 ingress token 文件路径；零值 = 告警面未装配）。
type notifierConfig struct {
	// URL 是接收器完整地址（http://127.0.0.1:<gateway-port>/internal/alerts
	// ——vmalert 钉 manager 且 host 网络，与 gateway 同 netns，回环恒可达；
	// 设计字面「<advertise>:<网关端口>」的实现收敛为回环：gateway 缺省绑定
	// 127.0.0.1，advertise 形态在缺省绑定下不可达，见实现票偏差注记）。
	URL string
	// TokenFile 是 ingress token 文件的宿主路径（bind 只读挂载进任务，
	// -notifier.basicAuth.passwordFile 指向任务内路径——凭据材料不进服务
	// spec；token file 复用，设计 §2.3）。
	TokenFile string
}

// owed 报告告警面是否已装配（未装配时 alerts.mode=on 显式失败退避——
// 宁缺毋错）。
func (c notifierConfig) owed() bool { return c.URL != "" && c.TokenFile != "" }

// rulesYAML 把规则集渲染为 Prometheus rule 文件（设计 §2.2：groups 单组；
// `for` 映射 for_duration〔0 = 无 for 子句〕；annotation 携带 channels/
// severity/name）。确定性渲染（内容寻址命名的哈希基）：渲染前按 name 排序
//（存储读取序的同锚防线——同规则集恒同内容，与调用方的切片序无关）、
// label 键排序、expr 用块标量免转义。空规则集渲染空组（vmalert 合法输入；
// 内容寻址名稳定）。
func rulesYAML(rules []state.AlertRule) string {
	ordered := make([]state.AlertRule, len(rules))
	copy(ordered, rules)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].ID < ordered[j].ID
	})
	var b strings.Builder
	b.WriteString("groups:\n")
	b.WriteString("  - name: fleetly\n")
	b.WriteString("    rules:\n")
	for _, r := range ordered {
		fmt.Fprintf(&b, "      - alert: %s\n", r.Name)
		b.WriteString("        expr: >-\n")
		for _, line := range strings.Split(strings.TrimRight(r.Expr, "\n"), "\n") {
			fmt.Fprintf(&b, "          %s\n", strings.TrimSpace(line))
		}
		if r.ForDurationSeconds > 0 {
			fmt.Fprintf(&b, "        for: %ds\n", r.ForDurationSeconds)
		}
		keys := make([]string, 0, len(r.Labels))
		for k := range r.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			b.WriteString("        labels:\n")
			for _, k := range keys {
				fmt.Fprintf(&b, "          %s: %s\n", k, r.Labels[k])
			}
		}
		// annotation 携带 channels/severity/name（设计 §2.2 原文）：severity
		// 取规则 labels 的 severity（缺失则不写该 annotation）；channels =
		// 端点 id 逗号串（空 = 接收器缺省投全部端点）；summary = 规则名。
		channels := strings.Join(r.Channels, ",")
		severity := r.Labels["severity"]
		b.WriteString("        annotations:\n")
		fmt.Fprintf(&b, "          summary: %s\n", r.Name)
		fmt.Fprintf(&b, "          channels: %s\n", channels)
		if severity != "" {
			fmt.Fprintf(&b, "          severity: %s\n", severity)
		}
	}
	return b.String()
}

// rulesConfigName 是规则配置的 swarm config 对象名（内容寻址：规则集变 =
// 名变 = 新对象；服务 spec 以名引用，specEqual 比对捕获漂移）。
func rulesConfigName(rules []state.AlertRule) string {
	sum := sha256.Sum256([]byte(rulesYAML(rules)))
	return rulesConfigPrefix + hex.EncodeToString(sum[:4])
}

// buildRulesConfigSpec 构造规则配置的期望 swarm.ConfigSpec。
func buildRulesConfigSpec(rules []state.AlertRule) swarm.ConfigSpec {
	return swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name: rulesConfigName(rules),
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				rulesConfigLabel:   "true",
			},
		},
		Data: []byte(rulesYAML(rules)),
	}
}

// buildVMAlertSpec 构造托管 vmalert 服务的期望 swarm spec（replicated-1 +
// manager 约束 + host 网络回环监听 + 规则 config 引用（内容寻址）+ token
// 文件只读 bind + 内存限额 128MB；无端口发布面——参数按 v1.152 镜像 -help
// 实测 flag 形态，2026-09-25）。
func buildVMAlertSpec(platformID string, nc notifierConfig, rules []state.AlertRule) swarm.ServiceSpec {
	one := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: VMAlertServiceName,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				vmalertLabel:       "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: DefaultVMAlertImage,
				Args: []string{
					"-datasource.url=http://" + hostIP + ":" + strconv.Itoa(QueryPort),
					"-remoteRead.url=http://" + hostIP + ":" + strconv.Itoa(QueryPort),
					"-notifier.url=" + nc.URL,
					// TLS 形态的接收器（网关 control_plane.tls.mode!=off）：
					// 回环 IP 无 SAN 可校验——跳过服务器认证（传输仍加密+
					// basicAuth token；staging 真机 2026-09-25 实爆明文拨
					// TLS 口 = connection reset 后补，0d84e6a 同族）。
					"-notifier.tlsInsecureSkipVerify=true",
					"-notifier.basicAuth.username=" + notifierBasicAuthUsername,
					// 密码位 = ingress token（file 形态——凭据材料不进 spec）。
					"-notifier.basicAuth.passwordFile=" + notifierTokenMountPath,
					"-rule=/etc/vmalert/rules/*.yaml",
					fmt.Sprintf("-evaluationInterval=%s", DefaultEvaluationInterval),
					fmt.Sprintf("-httpListenAddr=%s:%d", hostIP, VMAlertPort),
				},
				Mounts: []mount.Mount{
					// ingress token 文件（平台生成凭据）只读进任务——挂载源
					// 是路径不是材料（token file 复用，设计 §2.3）。
					{Type: mount.TypeBind, Source: nc.TokenFile, Target: notifierTokenMountPath, ReadOnly: true},
				},
				Configs: []*swarm.ConfigReference{{
					ConfigName: rulesConfigName(rules),
					File: &swarm.ConfigReferenceFileTarget{
						Name: rulesConfigMountPath,
						UID:  "0", GID: "0", Mode: 0o444,
					},
				}},
			},
			// host 网络：datasource/remoteRead 走 VM 回环监听（同宿主 netns
			// 直达），notifier 走 gateway 回环监听（同上）。
			Networks: []swarm.NetworkAttachmentConfig{{
				Target: hostNetworkName,
			}},
			Placement: &swarm.Placement{
				Constraints: []string{constraintFor(platformID)},
			},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: vmalertMemoryBytes},
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

// anchorRulesConfig 把规则 config 的底座对象 ID 锚入期望 spec（anchorSpec
// 的规则族对偶——只写名会被 swarm 以 "malformed config reference" 拒绝，
// W3 secret-ID 同族教训）。
func anchorRulesConfig(spec *swarm.ServiceSpec, rulesName, rulesID string) {
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, ref := range cs.Configs {
			if ref.ConfigName == rulesName {
				ref.ConfigID = rulesID
			}
		}
	}
}

// specEqual 幂等比对（镜像/参数/挂载/网络/约束/副本/限额/抓取配置引用
// ——服务的全部执行面都由期望 spec 权威表达；label 不参与，服务名即身份。
// 参数含 -httpListenAddr 回环监听（VM 查询面零公网面）与采集器
// -listen_ip / --web.listen-address 的 0.0.0.0 绑定——绑定面漂移必被本
// 比对捕获）。
func specEqual(cur ServiceState, desired swarm.ServiceSpec) bool {
	cs := desired.TaskTemplate.ContainerSpec
	if cur.Image != cs.Image {
		return false
	}
	if !sameStrings(cur.Args, cs.Args) {
		return false
	}
	if len(cur.MountSources) != len(cs.Mounts) {
		return false
	}
	for i, wm := range cs.Mounts {
		if cur.MountSources[i] != wm.Source || cur.MountTargets[i] != wm.Target {
			return false
		}
	}
	wantNets := make([]string, 0, len(desired.TaskTemplate.Networks))
	for _, n := range desired.TaskTemplate.Networks {
		wantNets = append(wantNets, n.Target)
	}
	if !sameStrings(cur.Networks, wantNets) {
		return false
	}
	wantConstraints := []string{}
	if pl := desired.TaskTemplate.Placement; pl != nil {
		wantConstraints = pl.Constraints
	}
	if !sameStrings(cur.Constraints, wantConstraints) {
		return false
	}
	// 抓取配置引用（内容寻址名——scrape 配置漂移经服务 spec 比对收敛）。
	wantConfigs := make([]string, 0, len(cs.Configs))
	for _, c := range cs.Configs {
		wantConfigs = append(wantConfigs, c.ConfigName)
	}
	if !sameStrings(cur.ConfigNames, wantConfigs) {
		return false
	}
	// 健康检查（执行面——探针序列漂移必捕获；nil 与空序列视为同形）。
	wantHealth := []string(nil)
	if cs.Healthcheck != nil {
		wantHealth = cs.Healthcheck.Test
	}
	if !sameStrings(cur.HealthTest, wantHealth) {
		return false
	}
	// 副本形态：replicated 比数值；global 比形态（Replicas=0 且 global）。
	wantGlobal := desired.Mode.Global != nil
	if cur.Global != wantGlobal {
		return false
	}
	if !wantGlobal {
		wantReplicas := uint64(0)
		if desired.Mode.Replicated != nil && desired.Mode.Replicated.Replicas != nil {
			wantReplicas = *desired.Mode.Replicated.Replicas
		}
		if cur.Replicas != wantReplicas {
			return false
		}
	}
	wantMem := int64(0)
	if res := desired.TaskTemplate.Resources; res != nil && res.Limits != nil {
		wantMem = res.Limits.MemoryBytes
	}
	return cur.MemoryBytes == wantMem
}

// sameStrings 序列相等（顺序敏感——spec 各面以期望序权威表达）。
func sameStrings(a, b []string) bool {
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
