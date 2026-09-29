package victorialogs

// 托管 VictoriaLogs duty 的 Docker API 消费面（2026-09-29 架构评审 C1 起
// 收编进 internal/dutydocker：连接构造/服务写原语/实况投影由共享适配层
// 唯一承载——本文件只保留消费方窄端口与包内哨兵；此前本包自持一份逐字
// 拷贝的 realDockerClient，六包同构拷贝的收编对象之一）。端口面按 VL
// 需要裁剪：无凭据 secret、无网络 ensure（任务挂 host 网络——见 spec.go
// 头注记）、无任务 IP 直达——健康检查走宿主回环。第三方（moby/swarm）
// 类型不出消费面——swarm.ServiceSpec 是部署器构造载荷，只进不出（出口
// 只有投影与 error）。
//
// 服务写幂等语义由 duty 收敛层保证（inspect → 比对 → create/update）。
// swarm 未就绪返回哨兵 ErrNotSwarmReady（duty 退避重试——各包哨兵同语义
// 刻意不共享类型）。

import (
	"context"
	"errors"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
)

// ErrNotSwarmReady 表示本机不是 active swarm manager（duty 可重试态）。
var ErrNotSwarmReady = errors.New("docker engine is not an active swarm manager (victorialogs duty)")

// dockerPort 是 duty 对 Docker API 的最小消费面（*dutydocker.Client 以
// 方法集超集满足；测试假件在本包注入）。实况投影与 Info 投影是共享类型
// （dutydocker.ServiceSnapshot / InfoSnapshot）——消费方只读自己比对用到
// 的字段。
type dockerPort interface {
	// Info 报告 swarm 状态投影（active 位由 duty 判定并映射包内哨兵）。
	Info(ctx context.Context) (dutydocker.InfoSnapshot, error)
	// ServiceInspect 按名取服务实况；缺失返回 Exists=false（不是错误——
	// 「不存在」是收敛的正常输入）。
	ServiceInspect(ctx context.Context, name string) (dutydocker.ServiceSnapshot, error)
	// ServiceCreate 创建服务（duty 保证仅缺失时调用）。
	ServiceCreate(ctx context.Context, spec swarm.ServiceSpec) error
	// ServiceUpdate 以乐观令牌推进服务（version 取自先前的 ServiceInspect）。
	ServiceUpdate(ctx context.Context, name string, version uint64, spec swarm.ServiceSpec) error
	// ServiceRemove 删除服务（幂等：缺失视为成功）。
	ServiceRemove(ctx context.Context, name string) error
	// VolumeEnsure 确认命名卷存在（幂等；缺失创建）。
	VolumeEnsure(ctx context.Context, name string) error
	// NetworkName 把服务实况里的网络挂载目标（创建期 "host" 被归一为网络
	// ID）解析回网络名（幂等比对的同锚面）。解析失败返回错误，duty
	// 退避重试不误判漂移。
	NetworkName(ctx context.Context, target string) (string, error)
}
