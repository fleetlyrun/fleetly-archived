// Package dutydocker 是受管组件部署器与收敛 duty 对 Docker API 的共享消费面
// （2026-09-29 架构评审 C1 深化：此前 ingress/victorialogs/metrics/rustfs/
// execrelay/database 六包各持一份逐字同构的 moby 适配层——连接构造、服务
// 实况投影、幂等 ensure/remove 原语——拷贝漂移的温床；本包将其收编为一份）。
//
// 形态纪律（沿六包既立惯例收拢）：
//   - moby/swarm 类型止步于本包出口的「构造载荷」与「实况投影」——
//     swarm.ServiceSpec 是部署器构造载荷只进不出，出口只有投影与 error；
//   - 服务写幂等语义（inspect → 比对 → create/update）由各 duty 收敛层
//     保证——本包只做忠实翻译与错误包装，不含任何收敛决策；
//   - 各包端口仍在消费方按需裁剪定义（消费方定义端口的 Go 惯例），本包
//     *Client 以方法集超集满足之；测试假件留在各包；
//   - swarm 未就绪哨兵（ErrNotSwarmReady 族）刻意由各包自持——同语义不
//     共享类型，duty 以 Info 投影的 SwarmActive 位自行判定；
//   - 一次性执行体（database 的 ContainerRun/JobRun 等领域语义原语）不属
//     本包——各包经嵌入 *Client 自行扩展。
package dutydocker

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"
)

// Client 是 Docker API 的 moby 适配实现（六包原 realDockerClient 的唯一
// 存活份：DOCKER_HOST/本机套接字连接形态）。错误前缀统一 dutydocker——
// 调用方（duty）的日志与哨兵负责组件归属语境。
type Client struct {
	cli *mobyclient.Client
}

// New 构造真实客户端（host 空 = FromEnv）。
func New(host string) (*Client, error) {
	opts := []mobyclient.Opt{mobyclient.FromEnv}
	if host != "" {
		opts = []mobyclient.Opt{mobyclient.WithHost(host), mobyclient.FromEnv}
	}
	cli, err := mobyclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("dutydocker: construct docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// NewWithClient 以既有 moby 连接构造（单连接复用形态：消费方持有自有
// 领域扩展原语——如一次性 job/容器执行体——需要原始连接面时，与本包
// 共享同一条连接；Close 归连接持有者，本包不重复关闭）。
func NewWithClient(cli *mobyclient.Client) *Client {
	return &Client{cli: cli}
}

// Close 释放底层连接（Wire cleanup）。
func (c *Client) Close() error { return c.cli.Close() }

// InfoSnapshot 是部署器/duty 关心的 Info 投影（advertise addr + swarm
// active——ingress swarmInfo 与 execrelay duty Info 面的同一形状）。
type InfoSnapshot struct {
	// SwarmActive 报告本机是否 active swarm manager。
	SwarmActive bool
	// NodeAddr 是 swarm advertise addr（控制面可达地址的兜底源；worker
	// 无此值——duty 只在 manager 收敛）。
	NodeAddr string
}

// Info 报告 swarm 状态投影。
func (c *Client) Info(ctx context.Context) (InfoSnapshot, error) {
	res, err := c.cli.Info(ctx, mobyclient.InfoOptions{})
	if err != nil {
		return InfoSnapshot{}, fmt.Errorf("dutydocker: docker info: %w", err)
	}
	return InfoSnapshot{
		SwarmActive: res.Info.Swarm.NodeID != "" &&
			res.Info.Swarm.LocalNodeState == swarm.LocalNodeStateActive,
		NodeAddr: res.Info.Swarm.NodeAddr,
	}, nil
}
