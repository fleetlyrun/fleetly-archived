package substrate

// 底座网络对象面（IMPL-T15-1 项目网生命周期 + 对账扩面）：engine.NetworkSubstrate
// 端口的 moby/client 实现。第三方类型只在本包内部，出口一律 engine 核心类型；
// 错误归一为端口哨兵（engine.ErrNetworkNotFound）。
//
// 与既有 NetworkEnsure 的关系：NetworkEnsure 是 app 专属网的幂等确认（固定
// managed label、被发布对账的既有循环消费）；本文件是**带 label 的**通用
// ensure + 读/删面（项目网自描述 label 与对账/GC 读面）。两者共用同一底层
// 语义（inspect 命中即 no-op、创建竞态已存在即成功），签名不合并是为了不动
// 既有端口与其测试替身（仓库「新能力 = 新端口」惯例）。

import (
	"context"
	"fmt"
	"sort"

	"github.com/containerd/errdefs"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/engine"
)

// 编译期断言：Client 隐式实现 engine.NetworkSubstrate 端口。
var _ engine.NetworkSubstrate = (*Client)(nil)

// NetworkEnsureWithLabels 幂等确保网络存在（缺失创建；labels 随创建写入，
// 已存在不覆盖——自描述 label 是平台对象契约，外部改写走对账披露）。
func (c *Client) NetworkEnsureWithLabels(ctx context.Context, name string, labels map[string]string) error {
	ictx, icancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	_, err := c.cli.NetworkInspect(ictx, name, mobyclient.NetworkInspectOptions{})
	icancel()
	if err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("substrate: network inspect %s: %w", name, err)
	}
	cctx, ccancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	_, cerr := c.cli.NetworkCreate(cctx, name, mobyclient.NetworkCreateOptions{
		Driver: "overlay",
		Labels: labels,
	})
	ccancel()
	if cerr != nil {
		// 并发创建竞态：已存在即成功。
		rctx, rcancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
		_, ierr := c.cli.NetworkInspect(rctx, name, mobyclient.NetworkInspectOptions{})
		rcancel()
		if ierr == nil {
			return nil
		}
		return fmt.Errorf("substrate: network create %s: %w", name, cerr)
	}
	return nil
}

// NetworkList 按 label 选择器（key=value）返回网络投影（managed 平台对象
// 列举面；结果按名字典序——披露/GC 的确定性序）。
func (c *Client) NetworkList(ctx context.Context, labels map[string]string) ([]engine.NetworkState, error) {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	filters := mobyclient.Filters{}
	for k, v := range labels {
		filters = filters.Add("label", k+"="+v)
	}
	res, err := c.cli.NetworkList(ctx, mobyclient.NetworkListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("substrate: network list: %w", err)
	}
	out := make([]engine.NetworkState, 0, len(res.Items))
	for _, n := range res.Items {
		// 列表面不含 Containers/Services（daemon 语义）——GC 判据走 Inspect。
		out = append(out, networkToState(n.Name, n.ID, n.Driver, n.Labels, 0, 0, n.Internal))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// NetworkInspect 按名取网络投影；缺失返回 engine.ErrNetworkNotFound
// （适配器归一，调用方 errors.Is 判定）。
func (c *Client) NetworkInspect(ctx context.Context, name string) (engine.NetworkState, error) {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	res, err := c.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return engine.NetworkState{}, fmt.Errorf("%w: %s", engine.ErrNetworkNotFound, name)
		}
		return engine.NetworkState{}, fmt.Errorf("substrate: network inspect %s: %w", name, err)
	}
	n := res.Network
	return networkToState(n.Name, n.ID, n.Driver, n.Labels, len(n.Containers), len(n.Services), n.Internal), nil
}

// NetworkEnsureWithOptions 是带 internal 变体的幂等 ensure（DT-5/DT-7
// task-group 网络：internal=true 以 `--driver overlay --internal` 创建——
// 无出网、无外部 DNS，跨节点数据面正常，T2-0① 实证）。已存在网络的
// internal 变体与请求不一致 → 显式失败（不可静默接受错变体——安全变体
// 必须可信；重建是显式操作）。
func (c *Client) NetworkEnsureWithOptions(ctx context.Context, name string, labels map[string]string, internal bool) error {
	ictx, icancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	_, err := c.cli.NetworkInspect(ictx, name, mobyclient.NetworkInspectOptions{})
	icancel()
	if err == nil {
		st, ierr := c.NetworkInspect(ctx, name)
		if ierr != nil {
			return fmt.Errorf("substrate: network inspect %s: %w", name, ierr)
		}
		if st.Internal != internal {
			return fmt.Errorf("substrate: network %s exists with internal=%t but internal=%t was requested (variant mismatch — recreate the network explicitly)",
				name, st.Internal, internal)
		}
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("substrate: network inspect %s: %w", name, err)
	}
	cctx, ccancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	_, cerr := c.cli.NetworkCreate(cctx, name, mobyclient.NetworkCreateOptions{
		Driver:   "overlay",
		Internal: internal,
		Labels:   labels,
	})
	ccancel()
	if cerr != nil {
		// 并发创建竞态：已存在即成功（存在性以再 inspect 为准；变体错配
		// 由下一拍/调用方复查暴露——本路径只处理「恰好同时创建」）。
		rctx, rcancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
		_, ierr := c.cli.NetworkInspect(rctx, name, mobyclient.NetworkInspectOptions{})
		rcancel()
		if ierr == nil {
			return nil
		}
		return fmt.Errorf("substrate: network create %s: %w", name, cerr)
	}
	return nil
}

// NetworkRemove 删除网络（幂等：缺失视为成功；in-use 由 daemon 拒绝——
// FailedPrecondition 原样上抛，调用方 best-effort 消化/下一拍重试）。
func (c *Client) NetworkRemove(ctx context.Context, name string) error {
	ctx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	if _, err := c.cli.NetworkRemove(ctx, name, mobyclient.NetworkRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("substrate: network remove %s: %w", name, err)
	}
	return nil
}

// networkToState 把 moby 网络对象投影为 engine 核心类型（Labels 复制防外部
// 别名；Containers/Services 计数 = 挂接/引用面——GC 零引用零端点判据）。
func networkToState(name, id, driver string, labels map[string]string, containers, services int, internal bool) engine.NetworkState {
	copied := make(map[string]string, len(labels))
	for k, v := range labels {
		copied[k] = v
	}
	return engine.NetworkState{
		Name:       name,
		ID:         id,
		Labels:     copied,
		Driver:     driver,
		Internal:   internal,
		Containers: containers,
		Services:   services,
	}
}
