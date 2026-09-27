package substrate

// Swarm config 原语（T 线 OT-3，IMPL-T1-4）：引擎 ConfigEnsurer/ConfigReaper
// 端口的底座实现。ensure 幂等语义与 EnsureSecret 同型（inspect → 缺失才
// create——config 名内嵌内容指纹，「存在性」判据即幂等）；服务 spec 翻译侧
// 补齐 ConfigReference 的 ConfigID 与完整 File（只写名会被 swarm 以
// "malformed config reference" 拒绝——W3 secret-ID 同族教训，metrics 栈
// 2026-09-22 dind 实证，internal/metrics/spec.go anchorSpec 同注）。
//
// 明文纪律：config 内容按设计是明文（可回读），但 data 只进创建载荷与
// 服务 spec 引用链，绝不进日志/错误文本——平台不主动外泄用户配置内容
//（与 rustfs/database 的凭据纪律同族）。

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"
)

// EnsureConfig 实现 engine.ConfigEnsurer：确认 Swarm config 在位并返回其
// 对象 ID；缺失时以 data 创建（labels 为归属标注——清场/识别的选择器锚）。
// 内容只进创建载荷。
func (c *Client) EnsureConfig(ctx context.Context, name string, data []byte, labels map[string]string) (string, error) {
	ictx, icancel := withCallTimeout(ctx) // D2：非流式 per-call 超时（逐调用）
	res, err := c.cli.ConfigInspect(ictx, name, mobyclient.ConfigInspectOptions{})
	icancel()
	if err == nil {
		return res.Config.ID, nil
	}
	if !errdefs.IsNotFound(err) {
		return "", fmt.Errorf("substrate: config inspect %s: %w", name, err)
	}
	spec := swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: name, Labels: labels},
		Data:        data,
	}
	cctx, ccancel := withCallTimeout(ctx)
	created, err := c.cli.ConfigCreate(cctx, mobyclient.ConfigCreateOptions{Spec: spec})
	ccancel()
	if err != nil {
		// 并发创建竞态：已存在即成功（幂等判据 = 名字在位）。
		if errdefs.IsConflict(err) || errdefs.IsAlreadyExists(err) {
			rctx, rcancel := withCallTimeout(ctx)
			re, rerr := c.cli.ConfigInspect(rctx, name, mobyclient.ConfigInspectOptions{})
			rcancel()
			if rerr == nil {
				return re.Config.ID, nil
			}
		}
		return "", fmt.Errorf("substrate: config create %s: %w", name, err)
	}
	return created.ID, nil
}

// ConfigList 按 label 选择器返回 config 对象名（清场路径的选择面——内容
// 换版后的旧对象回收与 app 删除 reap 都按 fleetly.managed+fleetly.app
// 扫尾，SecretList 同款口径）。
func (c *Client) ConfigList(ctx context.Context, labels map[string]string) ([]string, error) {
	filters := mobyclient.Filters{}
	for k, v := range labels {
		filters = filters.Add("label", k+"="+v)
	}
	lctx, lcancel := withCallTimeout(ctx)
	res, err := c.cli.ConfigList(lctx, mobyclient.ConfigListOptions{Filters: filters})
	lcancel()
	if err != nil {
		return nil, fmt.Errorf("substrate: config list: %w", err)
	}
	out := make([]string, 0, len(res.Items))
	for _, cfg := range res.Items {
		out = append(out, cfg.Spec.Name)
	}
	return out, nil
}

// ConfigRemove 删除 config（幂等：缺失视为成功；in-use 返回错误由调用方
// 降级——底座拒绝删除仍被服务引用的对象，best-effort 清场不阻塞调用方
// 主链路，下一拍重扫重试）。
func (c *Client) ConfigRemove(ctx context.Context, name string) error {
	rctx, rcancel := withCallTimeout(ctx)
	_, err := c.cli.ConfigRemove(rctx, name, mobyclient.ConfigRemoveOptions{})
	rcancel()
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("substrate: config remove %s: %w", name, err)
	}
	return nil
}
