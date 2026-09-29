package dutydocker

// 对象面：卷/网络/secret/config 的幂等 ensure/remove 原语与容器移除。
// 幂等语义（六包原实现逐字收拢）：已有即 no-op、缺失创建、并发创建竞态
// 已存在即成功（inspect 兜回）；remove 缺失视为成功；in-use（网络/secret
// 仍有引用）返回错误由调用方退避重试——删除到引用释放有传播延迟。

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// VolumeEnsure 确认命名卷存在（幂等；缺失创建——managed label；数据诞生点
// 显式收敛，部署器自证前置物在位）。
func (c *Client) VolumeEnsure(ctx context.Context, name string) error {
	if _, err := c.cli.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{}); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("dutydocker: volume inspect %s: %w", name, err)
	}
	if _, err := c.cli.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{
		Driver: "local",
		Name:   name,
		Labels: map[string]string{state.LabelManaged: state.ManagedLabelValue},
	}); err != nil {
		if _, ierr := c.cli.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{}); ierr == nil {
			return nil // 并发创建竞态：已存在即成功
		}
		return fmt.Errorf("dutydocker: volume create %s: %w", name, err)
	}
	return nil
}

// VolumeRemove 删除卷（幂等：缺失视为成功。显式数据丢弃路径专用——平台
// 对数据卷的默认路径是保留转 orphaned）。
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	if _, err := c.cli.VolumeRemove(ctx, name, mobyclient.VolumeRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("dutydocker: volume remove %s: %w", name, err)
	}
	return nil
}

// NetworkEnsure 确认 overlay 网络存在（幂等；缺失创建——managed label）。
// attachable=true 时一次性容器（独立容器形态：备份 job / 上传轨执行体）可
// 挂接——非 attachable overlay 拒绝独立容器（rustfs/database 同口径）。
func (c *Client) NetworkEnsure(ctx context.Context, name string, attachable bool) error {
	if _, err := c.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{}); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("dutydocker: network inspect %s: %w", name, err)
	}
	if _, err := c.cli.NetworkCreate(ctx, name, mobyclient.NetworkCreateOptions{
		Driver:     "overlay",
		Attachable: attachable,
		Labels:     map[string]string{state.LabelManaged: state.ManagedLabelValue},
	}); err != nil {
		if _, ierr := c.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{}); ierr == nil {
			return nil // 并发创建竞态：已存在即成功
		}
		return fmt.Errorf("dutydocker: network create %s: %w", name, err)
	}
	return nil
}

// NetworkID 解析网络名 → 底座 ID（服务网络挂载与任务投影都以 ID 表达）。
func (c *Client) NetworkID(ctx context.Context, name string) (string, error) {
	res, err := c.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("dutydocker: network inspect %s: %w", name, err)
	}
	return res.Network.ID, nil
}

// NetworkName 把服务实况里的网络挂载目标（创建期被 engine 归一为网络 ID
// ——"host" 亦然）解析回网络名，幂等比对的同锚面（"host" 是 local-scope
// 网络，其 swarm 侧对象 ID 与本地 ID 不同，正向查名不可行；反向按 ID 解析
// 返回 swarm scope 对象名，2026-09-22 dind 实证）。解析失败返回错误，duty
// 退避重试不误判漂移。
func (c *Client) NetworkName(ctx context.Context, target string) (string, error) {
	res, err := c.cli.NetworkInspect(ctx, target, mobyclient.NetworkInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("dutydocker: network inspect %s: %w", target, err)
	}
	return res.Network.Name, nil
}

// NetworkRemove 删除网络（幂等：缺失视为成功；仍有端点挂接返回错误——
// 调用方 best-effort 消化，引用方清场后可重试）。
func (c *Client) NetworkRemove(ctx context.Context, name string) error {
	if _, err := c.cli.NetworkRemove(ctx, name, mobyclient.NetworkRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("dutydocker: network remove %s: %w", name, err)
	}
	return nil
}

// SecretInspect 按名取 swarm secret（凭据 secret 的幂等创建判据；exists=true
// 时返回对象 ID——服务 spec 的 secret 引用必须携带 ID，仅名字是 malformed
// reference；value 不可读——Docker API 从不回吐 secret 数据）。
func (c *Client) SecretInspect(ctx context.Context, name string) (id string, exists bool, err error) {
	res, err := c.cli.SecretInspect(ctx, name, mobyclient.SecretInspectOptions{})
	if err == nil {
		return res.Secret.ID, true, nil
	}
	if errdefs.IsNotFound(err) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("dutydocker: secret inspect %s: %w", name, err)
}

// SecretCreate 创建 swarm secret 并返回其 ID（duty 保证仅缺失时调用；data
// 只进创建载荷，绝不进日志/错误）。
func (c *Client) SecretCreate(ctx context.Context, spec swarm.SecretSpec) (string, error) {
	res, err := c.cli.SecretCreate(ctx, mobyclient.SecretCreateOptions{Spec: spec})
	if err != nil {
		return "", fmt.Errorf("dutydocker: secret create %s: %w", spec.Name, err)
	}
	return res.ID, nil
}

// SecretEnsure 幂等创建 secret 并返回对象 ID：缺失创建；并发创建竞态
// （Conflict/AlreadyExists）inspect 兜回 ID——已存在即成功（substrate.
// EnsureSecret 同型）。data 只进创建载荷，绝不进日志/错误文本。
func (c *Client) SecretEnsure(ctx context.Context, name string, data []byte, labels map[string]string) (string, error) {
	spec := swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: name, Labels: labels},
		Data:        data,
	}
	created, err := c.cli.SecretCreate(ctx, mobyclient.SecretCreateOptions{Spec: spec})
	if err == nil {
		return created.ID, nil
	}
	if errdefs.IsConflict(err) || errdefs.IsAlreadyExists(err) {
		if res, ierr := c.cli.SecretInspect(ctx, name, mobyclient.SecretInspectOptions{}); ierr == nil {
			return res.Secret.ID, nil
		}
	}
	return "", fmt.Errorf("dutydocker: secret create %s: %w", name, err)
}

// SecretList 按 label 选择器返回 secret 名（清场路径：凭据材料不残留）。
func (c *Client) SecretList(ctx context.Context, labels map[string]string) ([]string, error) {
	filters := mobyclient.Filters{}
	for k, v := range labels {
		filters = filters.Add("label", k+"="+v)
	}
	res, err := c.cli.SecretList(ctx, mobyclient.SecretListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("dutydocker: secret list: %w", err)
	}
	out := make([]string, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, s.Spec.Name)
	}
	return out, nil
}

// SecretRemove 删除 secret（幂等：缺失视为成功；in-use 返回错误由 duty
// 退避重试——服务删除到引用释放有传播延迟）。
func (c *Client) SecretRemove(ctx context.Context, name string) error {
	if _, err := c.cli.SecretRemove(ctx, name, mobyclient.SecretRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("dutydocker: secret remove %s: %w", name, err)
	}
	return nil
}

// ConfigEnsure 确认 swarm config 对象存在（幂等；缺失创建——内容寻址命名，
// 同名即同内容）。返回底座对象 ID（服务 spec 的 ConfigReference 需要 ID+名
// 双写——只写名会被 swarm 以 "malformed config reference" 拒绝，W3
// secret-ID 同族真机教训，2026-09-22 dind 实证）。
func (c *Client) ConfigEnsure(ctx context.Context, name string, spec swarm.ConfigSpec) (string, error) {
	if res, err := c.cli.ConfigInspect(ctx, name, mobyclient.ConfigInspectOptions{}); err == nil {
		return res.Config.ID, nil
	} else if !errdefs.IsNotFound(err) {
		return "", fmt.Errorf("dutydocker: config inspect %s: %w", name, err)
	}
	created, err := c.cli.ConfigCreate(ctx, mobyclient.ConfigCreateOptions{Spec: spec})
	if err != nil {
		if res, ierr := c.cli.ConfigInspect(ctx, name, mobyclient.ConfigInspectOptions{}); ierr == nil {
			return res.Config.ID, nil // 并发创建竞态：已存在即成功
		}
		return "", fmt.Errorf("dutydocker: config create %s: %w", name, err)
	}
	return created.ID, nil
}

// ConfigRemove 删除 config 对象（幂等：缺失视为成功）。
func (c *Client) ConfigRemove(ctx context.Context, name string) error {
	if _, err := c.cli.ConfigRemove(ctx, name, mobyclient.ConfigRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("dutydocker: config remove %s: %w", name, err)
	}
	return nil
}

// ConfigListNamesByLabel 列出带指定 label 键值对的 config 对象名（GC 面；
// 键值由消费方传入——各 config 族只认自己的 label 行）。
func (c *Client) ConfigListNamesByLabel(ctx context.Context, labelKey, labelValue string) ([]string, error) {
	res, err := c.cli.ConfigList(ctx, mobyclient.ConfigListOptions{})
	if err != nil {
		return nil, fmt.Errorf("dutydocker: config list: %w", err)
	}
	var out []string
	for _, cfg := range res.Items {
		if cfg.Spec.Labels[labelKey] != labelValue {
			continue
		}
		out = append(out, cfg.Spec.Name)
	}
	return out, nil
}

// ContainerRemoveForce 强制移除容器（幂等：缺失视为成功；返回是否实际移除
// ——迁移收敛的检测语义：removed=false 即本无残留）。
func (c *Client) ContainerRemoveForce(ctx context.Context, name string) (bool, error) {
	if _, err := c.cli.ContainerRemove(ctx, name, mobyclient.ContainerRemoveOptions{Force: true}); err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("dutydocker: container remove %s: %w", name, err)
	}
	return true, nil
}
