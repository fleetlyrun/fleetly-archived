package rustfs

// 托管 RustFS 管理器的 Docker API 消费面（E3-5；2026-09-29 架构评审 C1 起
// 收编进 internal/dockerapi：连接构造/服务写原语/实况投影由共享适配层
// 唯一承载——本文件只保留消费方窄端口与包内哨兵；此前本包自持一份逐字
// 拷贝的 realDockerClient，六包同构拷贝的收编对象之一）。端口面按 rustfs
// 需要裁剪：网络 ensure 走 attachable 形态（restic 上传轨的一次性容器经
// 它入网）、凭据 secret 全套（幂等创建/轮换清场）与任务 IP 直达——平台
// 侧 S3 消费面（EnsureBucket/探针）经同网任务 IP 拨号。第三方（moby/
// swarm）类型不出消费面——swarm.ServiceSpec 是部署器构造载荷，只进不出
//（出口只有投影与 error）。
//
// 服务写幂等语义由收敛层保证（inspect → 比对 → create/update）；
// 适配器做忠实的翻译与错误包装。swarm 未就绪返回哨兵 ErrNotSwarmReady
//（收敛循环退避重试——各包哨兵同语义刻意不共享类型）。

import (
	"context"
	"errors"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dockerapi"
)

// ErrNotSwarmReady 表示本机不是 active swarm manager（收敛循环可重试态）。
var ErrNotSwarmReady = errors.New("docker engine is not an active swarm manager (rustfs manager)")

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
	// NetworkEnsure 确认 overlay 网络存在（幂等；缺失创建；**attachable**
	// 形态——restic 上传轨的一次性容器经它入网，非 attachable overlay 拒绝
	// 独立容器挂接）。
	NetworkEnsure(ctx context.Context, name string, attachable bool) error
	// NetworkID 解析网络名 → 底座 ID（服务网络挂载与任务投影都以 ID 表达）。
	NetworkID(ctx context.Context, name string) (string, error)
	// SecretInspect 按名取 swarm secret（凭据 secret 的幂等创建判据；
	// exists=true 时返回对象 ID——服务 spec 的 secret 引用必须携带 ID，
	// 仅名字是 malformed reference；value 不可读——Docker API 从不回吐
	// secret 数据）。
	SecretInspect(ctx context.Context, name string) (id string, exists bool, err error)
	// SecretCreate 创建 swarm secret 并返回其 ID（调用方保证仅缺失时调用；
	// data 只进创建载荷，绝不进日志/错误）。
	SecretCreate(ctx context.Context, spec swarm.SecretSpec) (string, error)
	// SecretList 按 label 选择器返回 secret 名（清场路径：mode 离开
	// rustfs 后移除凭据 secret，凭据是运行时配置不残留）。
	SecretList(ctx context.Context, labels map[string]string) ([]string, error)
	// SecretRemove 删除 secret（幂等：缺失视为成功；in-use 返回错误由
	// 收敛循环退避重试——服务删除到 secret 引用释放有传播延迟）。
	SecretRemove(ctx context.Context, name string) error
	// TaskAddress 返回服务当前 running 任务在指定网络上的 IP（平台侧
	// S3 消费面——EnsureBucket/探针——的可达拨号地址；overlay VIP 只在
	// 网络内可解析，fleetlyd 经同网任务 IP 直达）。ok=false = 无 running
	// 任务或未挂目标网络（收敛未完成，不是错误）。
	TaskAddress(ctx context.Context, service, networkID string) (ip string, ok bool, err error)
}
