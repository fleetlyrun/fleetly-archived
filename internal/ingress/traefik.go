package ingress

// Traefik 部署器（T2.15；架构 §2.6 入口行）：平台自管 Traefik global
// service（每节点一个；v0.1 单机 1 实例）——钉版镜像 traefik:v3.5（Spike B
// 实测版本）、host 模式发布 80/443、动态配置经 HTTP provider 轮询控制面
// 端点（Header token）、不启用 Swarm/Docker provider 自动发现（Traefik 的
// Swarm provider 不检查健康——task running 即注册，破坏「路由晚于健康门」
// 不变量，architecture §2.5）、ping 健康检查。幂等收敛：服务存在则比对
// spec（镜像/参数/端口/挂载/健康检查），差异才更新。
//
// 网络拓扑：Traefik 服务按需接入各 app 专属 overlay 网络（attachNetwork）
// ——路由后端 fleetly-<app>-<service> 的 VIP 只在同网络内可达；网络集变化
// 是一次 service update（任务重建一次，v0.1 接受；配置视图在控制面，
// Traefik 重启即重新拉取，入口不丢配置）。
//
// Docker 消费面收编进 internal/dockerapi（2026-09-29 架构评审 C1）：连接
// 构造/服务写原语/实况投影由共享适配层唯一承载——本文件只保留消费方窄
// 端口与包内哨兵。本文件是第三方适配面：moby/swarm 类型不出本文件（出口
// 只有 error、Status 投影与部署器接口）。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// IngressServiceName 是平台入口服务名（契约：fleetly-ingress）。
const IngressServiceName = "fleetly-ingress"

// ingressLabelIngress 是入口服务自描述 label（CLI/运维识别）。
const ingressLabelIngress = "fleetly.ingress"

// legacyCertSeedContainerName 是 v0.1 证书 seed 容器名（退役形态，E1-2）。
// 保留为迁移收敛的检测常量：既有单节点安装升级后，启动 sweep 检测到该
// 容器残留即移除（幂等；分发面收敛——证书数据本体在控制面 cert_dir，
// 不经本路径触碰）。
const legacyCertSeedContainerName = "fleetly-ingress-cert-seeder"

// traefikHealthcheckArgs 是健康检查（traefik healthcheck 子命令打内置
// ping 端点；静态配置同步开 --ping）。
var traefikHealthcheckArgs = []string{"CMD", "traefik", "healthcheck", "--ping"}

// dockerClient 是部署器对 Docker API 的最小消费面（2026-09-29 架构评审 C1
// 起 Docker 适配面收编进 internal/dockerapi：连接构造/服务写原语/实况
// 投影由共享适配层唯一承载——此前本包自持一份逐字拷贝的 realDockerClient；
// *dockerapi.Client 以方法集超集满足，测试假件在本包注入）。端口面按入
// 口部署器需要裁剪：无 secret/config、无任务 IP 直达。第三方（moby/swarm）
// 类型不出消费面——swarm.ServiceSpec 是第三方构造载荷，只进不出（出口只
// 有投影与 error）；实况投影与 Info 投影是共享类型（dockerapi.
// ServiceSnapshot / InfoSnapshot）——消费方只读自己比对用到的字段。
type dockerClient interface {
	// Info 报告 swarm 状态投影（active 位由部署器判定并映射包内哨兵）。
	Info(ctx context.Context) (dockerapi.InfoSnapshot, error)
	// ServiceInspect 按名取服务实况；缺失返回 Exists=false（不是错误——
	// 「不存在」是收敛的正常输入）。
	ServiceInspect(ctx context.Context, name string) (dockerapi.ServiceSnapshot, error)
	// ServiceCreate 创建服务（收敛保证仅缺失时调用）。
	ServiceCreate(ctx context.Context, spec swarm.ServiceSpec) error
	// ServiceUpdate 以乐观令牌推进服务（version 取自先前的 ServiceInspect）。
	ServiceUpdate(ctx context.Context, name string, version uint64, spec swarm.ServiceSpec) error
	// NetworkEnsure 确认 overlay 网络存在（attach 的前置对象；幂等：已有
	// 即 no-op、缺失创建、并发竞态已存在即成功。attachable=true 时一次性
	// 容器可挂接——入口代建的是普通 overlay，恒 false）。
	NetworkEnsure(ctx context.Context, name string, attachable bool) error
	// NetworkID 解析网络名 → 底座 ID（attach 幂等判据：服务实况里的
	// 网络目标是 ID 形态）。
	NetworkID(ctx context.Context, name string) (string, error)
	// NetworkRemove 删除网络（MoveApp 摘旧网；幂等：缺失视为成功；仍有
	// 端点挂接返回错误——调用方 best-effort 消化）。
	NetworkRemove(ctx context.Context, name string) error
	// VolumeEnsure 确认命名卷存在（幂等；E1-4 registry 数据卷的前置对象
	// ——swarm 对 task 卷挂载亦有按节点创建语义，显式收敛使部署器自证）。
	VolumeEnsure(ctx context.Context, name string) error
	// ContainerRemoveForce 强制移除容器（E1-2 迁移收敛：v0.1 证书 seed
	// 容器退役；不存在返回 false，幂等——removed=false 即本无残留）。
	ContainerRemoveForce(ctx context.Context, name string) (bool, error)
}

// EnsureTraefik 幂等收敛入口服务（不存在创建；存在比对差异更新）。
// swarm 未就绪返回 ErrNotSwarmReady（服务壳降级重试）。
func (m *Manager) EnsureTraefik(ctx context.Context) error {
	info, err := m.docker.Info(ctx)
	if err != nil {
		return err
	}
	if !info.SwarmActive {
		return ErrNotSwarmReady
	}
	// 控制面可达地址：显式配置 > swarm advertise addr > 出口本地地址。
	advertise := m.cfg.ConfigAdvertiseIP
	if advertise == "" {
		advertise = info.NodeAddr
	}
	if advertise == "" {
		advertise = outboundLocalIP()
	}
	m.mu.Lock()
	m.advertiseIP = advertise
	m.mu.Unlock()

	m.setResponderURL("http://" + net.JoinHostPort(advertise, fmt.Sprint(m.cfgPort())))
	// E1-2 迁移收敛：v0.1 证书卷形态残留（seed 容器）幂等移除。旧挂载
	// 由下方 spec 收敛自动清除（期望 spec 不含挂载，traefikSpecEqual 对
	// 比出差异即更新）。证书数据本体在控制面 cert_dir，本步不触碰。
	if err := m.retireLegacyCertDistribution(ctx); err != nil {
		return err
	}
	token, err := m.token(ctx)
	if err != nil {
		return err
	}
	// F7（S20）：provider_endpoint 直接引用构造 spec 时使用的 responder 变量
	// ——此前取 Args[3] 是按位猜（"--providers.http.endpoint=<url>" 在 args
	// 中的位置），args 顺序一变日志字段即错位（曾把 pollInterval 记成端点）。
	endpoint := m.providerEndpoint(advertise)
	desired := m.buildTraefikSpec(endpoint, token)
	cur, err := m.docker.ServiceInspect(ctx, IngressServiceName)
	if err != nil {
		return err
	}
	if !cur.Exists {
		if err := m.docker.ServiceCreate(ctx, desired); err != nil {
			return err
		}
		m.log.Info("ingress: traefik service created", "image", m.cfg.TraefikImage,
			"http_port", m.cfg.HTTPPort, "https_port", m.cfg.HTTPSPort,
			"provider_endpoint", endpoint)
		m.mu.Lock()
		m.lastSpec = &desired
		m.mu.Unlock()
		return nil
	}
	if traefikSpecEqual(cur, desired) {
		m.mu.Lock()
		m.lastSpec = &desired
		m.mu.Unlock()
		return nil
	}
	// 收敛更新不裸用 desired：期望 spec 不含 TaskTemplate.Networks（app
	// 挂载由 attachNetwork 增量管理），整体替换会把 Traefik 从全部 app
	// 网络上踢下线（一次漂移收敛 → 全路由 502）——以服务实况网络集为基准
	// 合并后再提交（traefik.go 既有实机回归教训：网络目标集不能由期望
	// spec 重建）。实况无挂载（首次创建/确未 attach）时行为不变。
	merged := desired
	if len(cur.Networks) > 0 {
		merged = specWithNetworks(desired, cur.Networks)
	}
	if err := m.docker.ServiceUpdate(ctx, IngressServiceName, cur.Version, merged); err != nil {
		return err
	}
	m.log.Info("ingress: traefik service updated to desired spec", "image", m.cfg.TraefikImage)
	m.mu.Lock()
	m.lastSpec = &merged
	m.mu.Unlock()
	return nil
}

// attachNetwork 确保 Traefik 接入 app 专属 overlay 网络（幂等：已接入
// no-op；新增网络一次 service update——任务重建一次，入口配置即回）。
// 网络目标集以服务实况（cur.Networks）为基准追加——不能用 lastSpec 重建
// （lastSpec 不含历史 attach，会互相覆盖丢失其他 app 的网络，实机验证
// 发现的多 app 回归）。幂等判据用网络 ID（swarm 把 attach 目标归一为
// ID——名字比对永不命中，产生重复 attach，实机验证发现的第二处）。
func (m *Manager) attachNetwork(ctx context.Context, team, prj, app string) error {
	netName, err := appNetworkName(team, prj, app)
	if err != nil {
		return err
	}
	return m.attachNetworkByName(ctx, netName)
}

// attachNetworkByName 是网络接入的通用形态（E1-4：平台 overlay
// fleetly-system 由 registry 部署控制器 接入 Traefik——registry 路由段
// 的后端 VIP 只在同网络内可达）。语义与 attachNetwork 一致：幂等、以
// 服务实况网络集为基准、ID 判据。网络不存在时**代建**（普通 overlay——
// 仅限允许代建的平台网络；attachable 语义的网络走
// attachPlatformNetworkIfPresent，s3public.go）。
func (m *Manager) attachNetworkByName(ctx context.Context, netName string) error {
	if err := m.docker.NetworkEnsure(ctx, netName, false); err != nil {
		return err
	}
	netID, err := m.docker.NetworkID(ctx, netName)
	if err != nil {
		return err
	}
	return m.attachNetworkID(ctx, netName, netID)
}

// attachNetworkID 是网络挂接的幂等核（attachNetworkByName 与
// attachPlatformNetworkIfPresent 的共用段——E3-6 拆出，消灭第三份手写
// 合并逻辑）：幂等判据用网络 ID（swarm 把 attach 目标归一为 ID——名字比
// 对永不命中，产生重复 attach，实机验证发现的第二处）；网络目标集以服务
// 实况（cur.Networks）为基准追加——不能用 lastSpec 重建（lastSpec 不含
// 历史 attach，会互相覆盖丢失其他 app 的网络，实机验证发现的多 app 回归）。
func (m *Manager) attachNetworkID(ctx context.Context, netName, netID string) error {
	cur, err := m.docker.ServiceInspect(ctx, IngressServiceName)
	if err != nil {
		return err
	}
	if !cur.Exists {
		return fmt.Errorf("ingress: %s missing (ensure traefik first)", IngressServiceName)
	}
	for _, n := range cur.Networks {
		if n == netID {
			return nil
		}
	}
	m.mu.Lock()
	base := m.lastSpec
	m.mu.Unlock()
	if base == nil {
		return fmt.Errorf("ingress: no desired spec available (ensure traefik first)")
	}
	// 网络目标集 = 实况集 + 新网络（specWithNetworks 统一构造；以实况为
	// 基准的理由见其注释）。
	targets := make([]string, 0, len(cur.Networks)+1)
	targets = append(targets, cur.Networks...)
	targets = append(targets, netID)
	spec := specWithNetworks(*base, targets)
	if err := m.docker.ServiceUpdate(ctx, IngressServiceName, cur.Version, spec); err != nil {
		return err
	}
	m.mu.Lock()
	m.lastSpec = &spec
	m.mu.Unlock()
	m.log.Info("ingress: traefik attached to overlay network", "network", netName,
		"attached_total", len(spec.TaskTemplate.Networks))
	return nil
}

// specWithNetworks 是网络挂载构造的单点：把目标集（ID 形态）整组写入
// spec.TaskTemplate.Networks。调用方必须以服务实况（cur.Networks）为基准
// 传入目标集——不能用期望 spec 重建（期望不含历史 attach，整体替换会丢失
// 其他 app 的网络，实机验证发现的多 app 回归）。EnsureTraefik 的收敛更新
// 分支与 attachNetwork 共用本口径（消灭两份手写漂移——正是更新分支绕开
// 本纪律导致的全挂载丢失缺陷，架构评审 H8）。
func specWithNetworks(base swarm.ServiceSpec, netIDs []string) swarm.ServiceSpec {
	spec := base
	spec.TaskTemplate.Networks = make([]swarm.NetworkAttachmentConfig, 0, len(netIDs))
	for _, id := range netIDs {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: id})
	}
	return spec
}

// retireLegacyCertDistribution 是 E1-2 的迁移收敛步：v0.1「证书本地卷 +
// seed 容器」分发形态退役——检测到 seed 容器残留即移除并日志留痕（幂等：
// 不存在即 no-op）。旧挂载的清除在 spec 收敛（期望 spec 无挂载，差异即
// 更新）；证书命名卷本体保留（卷内是证书副本，移除属数据面动作——「迁移
// 只动分发面不动证书数据」，由操作者按指引自行清理）。
func (m *Manager) retireLegacyCertDistribution(ctx context.Context) error {
	removed, err := m.docker.ContainerRemoveForce(ctx, legacyCertSeedContainerName)
	if err != nil {
		return err
	}
	if removed {
		m.log.Info("ingress: legacy cert seed container removed (volume model retired; certs are distributed inline via dynamic config since E1-2)",
			"container", legacyCertSeedContainerName)
	}
	return nil
}

// buildTraefikSpec 构造入口服务的期望 swarm spec（host 80/443 + HTTP
// provider 静态配置 + ping 健康检查；E1-2 起无证书挂载——证书经动态配置
// 内联下发，见 dynamic.go TLSCertificate）。
func (m *Manager) buildTraefikSpec(endpoint, token string) swarm.ServiceSpec {
	args := []string{
		"--entryPoints.web.address=:" + fmt.Sprint(m.cfg.HTTPPort),
		"--entryPoints.websecure.address=:" + fmt.Sprint(m.cfg.HTTPSPort),
		// 控制面下发（唯一配置面；Swarm/Docker 自动发现显式不启用——
		// architecture §2.5 路由时机不变量 + Spike B 四态纪律）。endpoint
		// 由 providerEndpoint 决定：单节点明文 8422；多节点证书就绪后切
		// https://ctrl.<base>:8423（E1-3）。
		"--providers.http.endpoint=" + endpoint + "/configs",
		"--providers.http.pollInterval=" + m.cfg.PollInterval.String(),
		"--providers.http.pollTimeout=5s",
		"--providers.http.headers.Authorization=Bearer " + token,
		// F9 修订二：多节点 endpoint 是 advertise IP（VPC），IP 端点无 SAN
		// 可校验——Traefik 侧跳过服务器认证（传输仍 TLS 加密 + token；
		// 服务器认证由 VPC 边界承担，内部 CA 硬化挂 v0.2.x）。单节点 8422
		// 明文形态无 TLS，参数无效但无害（Traefik 容忍）。
		//
		// TODO(harden-8423, v0.2.x 硬化票未排期): 服务器认证缺口依赖链——
		// 按主机名（ctrl.<base>，平台证书 SAN 项）拨号需满足其一：
		//   a) worker 节点把 ctrl.<base> 解析到 advertise IP——extra_hosts
		//      通道被真机证伪（Docker 29.8.1 swarm 任务不应用
		//      ContainerSpec.Hosts，F9 修订二）；改走节点 /etc/hosts 指引则
		//      把 DNS 正确性变成部署前置，收益不抵脆弱度；
		//   b) 公网 DNS ctrl.<base> → manager 公网 IP——8423 必须公网可达，
		//      与「动态配置含全平台 TLS 私钥，公网 8423 零暴露」裁决冲突
		//      （F9 起因）；IP 直拨形式下 (a)(b) 均不可行，除非底座支持
		//      拨号主机名与连接地址分离（Traefik providers.http 无此配置）。
		//   诚实行内态：IP 端点 + insecureSkipVerify + VPC 边界（传输 TLS
		//   加密与 token 鉴权不受影响）；spec 参数形态由
		//   TestProviderEndpointVPCIPForm / TestPlatformCertControllerInertWhen
		//   BaseDomainEmpty 钉死——改参数必先改测试，防无声回退。
		//   出路（设计级，需立项）：内部 CA + swarm secret 分发 CA 到各节点
		//   （控制面外通道，无鸡生蛋）+ 8423 换 CA 签发的 IP SAN 证书 +
		//   --providers.http.tls.ca；或上游 swarm 任务修复 extra_hosts 后
		//   回到主机名拨号。
		"--providers.http.tls.insecureSkipVerify=true",
		// ping 健康面（healthcheck 子命令消费；容器内 8080，不发布）。
		"--ping=true",
		// 访问日志（E6 观测专项设计 §3.2，W5-S2）：JSON 行含 RouterName
		// 等结构化字段——hub 平台采集（internal/logs access 采集器）反解
		// app/service 的输入面。存量部署经 spec 漂移比对自动收敛（args
		// 全量比对，改参数即触发一次 service update）。
		"--accesslog=true",
		"--accesslog.format=json",
		"--log.level=INFO",
		"--api.dashboard=false",
	}
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: IngressServiceName,
			Labels: map[string]string{
				state.LabelManaged:  state.ManagedLabelValue,
				ingressLabelIngress: "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: m.cfg.TraefikImage,
				Args:  args,
				Healthcheck: &container.HealthConfig{
					Test:        traefikHealthcheckArgs,
					Interval:    10 * time.Second,
					Timeout:     5 * time.Second,
					Retries:     3,
					StartPeriod: 5 * time.Second,
				},
			},
		},
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		EndpointSpec: &swarm.EndpointSpec{
			Ports: []swarm.PortConfig{
				{Protocol: "tcp", TargetPort: portUint(m.cfg.HTTPPort), PublishedPort: portUint(m.cfg.HTTPPort), PublishMode: swarm.PortConfigPublishModeHost},
				{Protocol: "tcp", TargetPort: portUint(m.cfg.HTTPSPort), PublishedPort: portUint(m.cfg.HTTPSPort), PublishMode: swarm.PortConfigPublishModeHost},
			},
		},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			FailureAction: "pause",
			Order:         "stop-first",
		},
	}
	return spec
}

// providerEndpoint 计算下发给 Traefik 的静态 provider endpoint（不含
// /configs 路径后缀）：
//   - base_domain 为空（单节点 v0.1 形态）：http://<advertise>:8422——
//     行为与 v0.1 逐字一致（金样：既有 gateway/ingress 测试）；
//   - base_domain 非空且平台证书未就绪：同 8422 形态——bootstrap 容忍期
//     （设计 §2.4 次序②③）：挑战路由经动态配置可达 8422 应答器，HTTP-01
//     得以完成；Traefik 拿到的动态配置此窗口内不含秘密载荷以外的证书段
//     （平台证书尚未存在）；
//   - base_domain 非空且平台证书就绪：https://<advertise>:<config_tls_addr
//     端口>/configs + tls.insecureSkipVerify（F9 修订二，2026-09-21 真机：
//     Docker 29.8.1 的 swarm 任务不应用 ContainerSpec.Hosts，extra_hosts
//     钉定通道不可靠；endpoint 直用 advertise【VPC】IP——公网 8423 零暴
//     露【动态配置含全平台 TLS 私钥】；传输 TLS 加密 + ingress token 双
//     保，服务器认证由 VPC 边界承担【IP 端点无法 SAN 校验】；内部 CA +
//     tls.ca 挂 v0.2.x 硬化票）。
//
// 就绪判定 sticky：平台证书一经落盘持续存在（续期同路径换入），endpoint
// 不回摆。
func (m *Manager) providerEndpoint(advertise string) string {
	if m.ConfigTLSEnabled() && m.platformCertOnDisk() {
		return "https://" + net.JoinHostPort(advertise, configTLSPort(m.cfg.ConfigTLSAddr))
	}
	return "http://" + net.JoinHostPort(advertise, fmt.Sprint(m.cfgPort()))
}

// configTLSPort 从 config_tls_addr 提取端口位（Normalize 已回落设计缺省
// 0.0.0.0:8423，本函数只防显式畸形值——回落 8423 与缺省字面一致，字面值
// 由 TestDefaultConfigTLSAddrIsLiteral8423 钉死）。
func configTLSPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "8423"
	}
	return port
}

// portUint 是端口的有界窄化（配置校验保证 1..65535；越界收敛为 0 让
// 底座显式拒绝而不是静默回绕）。
func portUint(port int) uint32 {
	if port <= 0 || port > 65535 {
		return 0
	}
	return uint32(port) //nolint:gosec // 上界守卫已收敛到 1..65535
}

// traefikSpecEqual 幂等比对（镜像/参数/端口/挂载/健康检查；网络集由
// attachNetwork 增量管理，不参与本比对）。
func traefikSpecEqual(cur dockerapi.ServiceSnapshot, desired swarm.ServiceSpec) bool {
	cs := desired.TaskTemplate.ContainerSpec
	if cur.Image != cs.Image {
		return false
	}
	if !sameStrings(cur.Args, cs.Args) {
		return false
	}
	if !sameStrings(cur.Hosts, cs.Hosts) {
		return false
	}
	if !sameStrings(cur.HealthTest, cs.Healthcheck.Test) {
		return false
	}
	want := desired.EndpointSpec.Ports
	if len(cur.Ports) != len(want) {
		return false
	}
	curByPub := map[uint32]swarm.PortConfig{}
	for _, p := range cur.Ports {
		curByPub[p.PublishedPort] = p
	}
	for _, p := range want {
		got, ok := curByPub[p.PublishedPort]
		if !ok || got.TargetPort != p.TargetPort || got.PublishMode != p.PublishMode {
			return false
		}
	}
	if len(cur.Mounts) != len(cs.Mounts) {
		return false
	}
	for i, wm := range cs.Mounts {
		cm := cur.Mounts[i]
		if cm.Type != wm.Type || cm.Source != wm.Source || cm.Target != wm.Target || cm.ReadOnly != wm.ReadOnly {
			return false
		}
	}
	return true
}

// ErrNotSwarmReady 是 swarm 未就绪的哨兵（服务壳据此降级重试）。
var ErrNotSwarmReady = errors.New("docker engine is not an active swarm manager")

// outboundLocalIP 出口本地地址兜底（UDP dial 不发包）。
func outboundLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// appNetworkName 是 per-app overlay 网络名（naming.NetworkName 同公式；
// v0.3 三段形——team/prj 段进公式，rbac-teams §4.3。入口层本地重写避免
// 适配器反向依赖引擎/核心——公式由 naming_test 钉死，本包不得改写）。
func appNetworkName(team, prj, app string) (string, error) {
	if team == "" || prj == "" || app == "" {
		return "", fmt.Errorf("ingress: app network naming requires team/project/app slugs (got %q/%q/%q)", team, prj, app)
	}
	for _, v := range []string{team, prj, app} {
		if strings.ContainsAny(v, " \t/\\") {
			return "", fmt.Errorf("ingress: app network naming component %q contains invalid characters", v)
		}
	}
	return "fleetly-" + team + "-" + prj + "-" + app + "-net", nil
}

// sameStrings 切片相等（同序）。
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
