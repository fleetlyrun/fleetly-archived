package execrelay

// duty 的 Docker API 消费面（2026-09-29 架构评审 C1 起收编进
// internal/dutydocker：连接构造/服务写原语/实况投影由共享适配层唯一承载
// ——此前本包自持一份逐字同构的 dutyDockerClient，六包同构拷贝的收编对象
// 之一）。本文件只保留消费方窄端口与包内哨兵；端口面按 relay duty 需要
// 裁剪：服务收敛 + secret 原语 + 网络/Info 投影，无卷/无任务面。第三方
// （moby/swarm）类型不出本包的端口消费面——swarm.ServiceSpec 是部署器构
// 造载荷，只进不出（出口只有投影与 error）；实况投影与 Info 投影是共享
// 类型（dutydocker.ServiceSnapshot / InfoSnapshot）——消费方只读自己比对
// 用到的字段。
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
var ErrNotSwarmReady = errors.New("docker engine is not an active swarm manager (execrelay duty)")

// dutyDocker 是 duty 对 Docker API 的最小消费面（*dutydocker.Client 以方
// 法集超集满足；测试假件在本包注入）。
type dutyDocker interface {
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
	// SecretInspect 报告 secret 在位与对象 ID（ensureToken 的幂等判据；
	// exists=true 时 ID 是服务 spec 的 secret 引用要件——仅名字是 malformed
	// reference）。
	SecretInspect(ctx context.Context, name string) (id string, exists bool, err error)
	// SecretEnsure 幂等创建 secret 并返回对象 ID：缺失创建；并发创建竞态
	// inspect 兜回 ID。data 只进创建载荷，绝不进日志/错误文本。
	SecretEnsure(ctx context.Context, name string, data []byte, labels map[string]string) (string, error)
	// NetworkName 把服务实况里的网络挂载目标（创建期 "host" 被归一为网络
	// ID）解析回网络名（幂等比对的同锚面）。解析失败返回错误，duty 退避
	// 重试不误判漂移。
	NetworkName(ctx context.Context, target string) (string, error)
}
