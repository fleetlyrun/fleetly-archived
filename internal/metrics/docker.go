package metrics

// 托管 metrics 栈的 Docker API 消费面（2026-09-29 架构评审 C1 起
// 收编进 internal/dockerapi：连接构造/服务写原语/实况投影由共享适配层
// 唯一承载——本文件只保留消费方窄端口与包内哨兵；此前本包自持一份逐字
// 拷贝的 realDockerClient，六包同构拷贝的收编对象之一）。端口面按 metrics
// 需要裁剪，相对 victorialogs 的增面：swarm **config 对象**四面（抓取配置
// 的内容寻址分发，见 spec.go 头注记）与 ReadyNodeAddresses 节点地址投影
//（动态 targets 的节点注册表读面）。第三方（moby/swarm）类型不出本包的
// 端口消费面——swarm.ServiceSpec 是部署器构造载荷，只进不出（出口只有
// 投影与 error）。
//
// 服务写幂等语义由收敛层保证（inspect → 比对 → create/update）。
// swarm 未就绪返回哨兵 ErrNotSwarmReady（收敛循环退避重试——各包哨兵同语义
// 刻意不共享类型）。

import (
	"context"
	"errors"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dockerapi"
)

// ErrNotSwarmReady 表示本机不是 active swarm manager（收敛循环可重试态）。
var ErrNotSwarmReady = errors.New("docker engine is not an active swarm manager (metrics manager)")

// dockerPort 是收敛管理器对 Docker API 的最小消费面（*dockerapi.Client 以
// 方法集超集满足；测试假件在本包注入）。实况投影与 Info 投影是共享类型
// （dockerapi.ServiceSnapshot / InfoSnapshot）——消费方只读自己比对用到
// 的字段。
type dockerPort interface {
	// Info 报告 swarm 状态投影（active 位由收敛循环判定并映射包内哨兵）。
	Info(ctx context.Context) (dockerapi.InfoSnapshot, error)
	// ServiceInspect 按名取服务实况；缺失返回 Exists=false（不是错误——
	// 「不存在」是收敛的正常输入）。
	ServiceInspect(ctx context.Context, name string) (dockerapi.ServiceSnapshot, error)
	// ServiceCreate 创建服务（调用方保证仅缺失时调用）。
	ServiceCreate(ctx context.Context, spec swarm.ServiceSpec) error
	// ServiceUpdate 以乐观令牌推进服务（version 取自先前的 ServiceInspect）。
	ServiceUpdate(ctx context.Context, name string, version uint64, spec swarm.ServiceSpec) error
	// ServiceRemove 删除服务（幂等：缺失视为成功）。
	ServiceRemove(ctx context.Context, name string) error
	// VolumeEnsure 确认命名卷存在（幂等；缺失创建）。
	VolumeEnsure(ctx context.Context, name string) error
	// ConfigEnsure 确认抓取配置对象存在（幂等；缺失创建——内容寻址命名，
	// 同名即同内容）。返回底座对象 ID（服务 spec 的 ConfigReference 需要
	// ID+名双写——只写名会被 swarm 以 "malformed config reference" 拒绝，
	// W3 secret-ID 同族真机教训，2026-09-22 dind 实证）。
	ConfigEnsure(ctx context.Context, name string, spec swarm.ConfigSpec) (string, error)
	// NetworkName 把服务实况里的网络目标（创建期被 engine 归一为网络 ID）
	// 解析回网络名——幂等比对的同锚面（"host" 是 local-scope 网络，其
	// swarm 侧对象 ID 与本地 ID 不同，正向查名不可行；反向按 ID 解析返回
	// swarm scope 对象名，2026-09-22 dind 实证）。解析失败返回错误，收敛循环
	// 退避重试。
	NetworkName(ctx context.Context, target string) (string, error)
	// ReadyNodeAddresses 返回可抓取节点的 advertise 地址集（§6 挂账票的
	// 动态 targets 投影面）：State=ready 且 availability=active 的节点取
	// Status.Addr（worker 的节点注册地址 / manager 的 advertise 地址——
	// VM 经它直连采集器；Status.Addr 罕见缺省时回落 ManagerStatus.Addr）。
	// availability 非 active（drain/pause）不入选——global 采集器不在其上
	// 运行，抓了必 down。返回已去重的原序清单（渲染序的排序在 spec 层
	// scrapeAddrs）。读取失败返回错误，收敛循环退避重试。
	ReadyNodeAddresses(ctx context.Context) ([]string, error)
	// ConfigListNamesByLabel 列出带指定 label 键值对的 config 对象名（GC
	// 面——抓取配置与 vmalert 规则两族 config 各持自有自描述 label，调用
	// 点传本包常量选择器〔scrapeLabel / rulesConfigLabel，值恒 "true"〕，
	// 各族只认自己的行）。
	ConfigListNamesByLabel(ctx context.Context, labelKey, labelValue string) ([]string, error)
	// ConfigRemove 删除 config 对象（幂等：缺失视为成功）。
	ConfigRemove(ctx context.Context, name string) error
}
