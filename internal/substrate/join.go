package substrate

// swarm join-token 面（multi-node §2.3/D-MN-1，E1-8 join 向导）：
//   - SwarmJoinInfo：manager advertise addr（docker info Swarm.NodeAddr）与
//     worker join token（swarm inspect JoinTokens.Worker）——join 向导材料；
//   - SwarmRotateJoinToken：轮换 worker/manager join token（swarm update
//     的 rotate 标志），返回新 token；rotate 后旧 token 立即失效。
//
// 错误归一为端口哨兵（state.ErrNotSwarmManager——未启用 Swarm 即无 join
// 面）。第三方类型不出包（出口 = string）。

import (
	"context"
	"fmt"

	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// AdvertiseAddr 返回本节点（manager）的 swarm advertise 地址（docker info
// Swarm.NodeAddr；控制面地址注入 ctrlinject 的值源—— advertise 是 VPC 内网
// 地址，集群内工作负载经它回拨控制面不出集群）。未启用 Swarm 返回
// state.ErrNotSwarmManager。
func (c *Client) AdvertiseAddr(ctx context.Context) (string, error) {
	ictx, icancel := withCallTimeout(ctx)
	info, ierr := c.cli.Info(ictx, mobyclient.InfoOptions{})
	icancel()
	if ierr != nil {
		return "", fmt.Errorf("substrate: info: %w", ierr)
	}
	if info.Info.Swarm.NodeID == "" || info.Info.Swarm.LocalNodeState != swarm.LocalNodeStateActive {
		return "", state.ErrNotSwarmManager
	}
	return info.Info.Swarm.NodeAddr, nil
}

// SwarmJoinInfo 返回 join 向导材料：manager advertise addr 与 worker join
// token。未启用 Swarm（非 active manager）返回 state.ErrNotSwarmManager。
func (c *Client) SwarmJoinInfo(ctx context.Context) (string, string, error) {
	ictx, icancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	info, ierr := c.cli.Info(ictx, mobyclient.InfoOptions{})
	icancel()
	if ierr != nil {
		return "", "", fmt.Errorf("substrate: info: %w", ierr)
	}
	if info.Info.Swarm.NodeID == "" || info.Info.Swarm.LocalNodeState != swarm.LocalNodeStateActive {
		return "", "", state.ErrNotSwarmManager
	}
	sctx, scancel := withCallTimeout(ctx)
	res, serr := c.cli.SwarmInspect(sctx, mobyclient.SwarmInspectOptions{})
	scancel()
	if serr != nil {
		return "", "", fmt.Errorf("substrate: swarm inspect: %w", serr)
	}
	return info.Info.Swarm.NodeAddr, res.Swarm.JoinTokens.Worker, nil
}

// SwarmRotateJoinToken 轮换指定角色（worker|manager）的 join token 并返回
// 新 token（rotate 后旧 token 立即失效——D-MN-1 自动轮换语义的执行面）。
// 未启用 Swarm 返回 state.ErrNotSwarmManager；角色取值非法按 worker 处理
// （api 面已在 buf.validate 限制词表，此处防御性收敛）。
func (c *Client) SwarmRotateJoinToken(ctx context.Context, role string) (string, error) {
	sctx, scancel := withCallTimeout(ctx)
	res, serr := c.cli.SwarmInspect(sctx, mobyclient.SwarmInspectOptions{})
	scancel()
	if serr != nil {
		return "", fmt.Errorf("substrate: swarm inspect: %w", serr)
	}
	uctx, ucancel := withCallTimeout(ctx)
	_, uerr := c.cli.SwarmUpdate(uctx, mobyclient.SwarmUpdateOptions{
		Version:            res.Swarm.Version,
		Spec:               res.Swarm.Spec,
		RotateWorkerToken:  role != "manager",
		RotateManagerToken: role == "manager",
	})
	ucancel()
	if uerr != nil {
		return "", fmt.Errorf("substrate: swarm update (rotate %s join token): %w", role, uerr)
	}
	// 轮换后回读新 token（swarm update 本体不返回 token）。
	rctx, rcancel := withCallTimeout(ctx)
	res2, rerr := c.cli.SwarmInspect(rctx, mobyclient.SwarmInspectOptions{})
	rcancel()
	if rerr != nil {
		return "", fmt.Errorf("substrate: swarm inspect (post-rotate): %w", rerr)
	}
	if role == "manager" {
		return res2.Swarm.JoinTokens.Manager, nil
	}
	return res2.Swarm.JoinTokens.Worker, nil
}
