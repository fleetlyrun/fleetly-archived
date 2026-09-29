package dockerapi

// 服务面：实况投影（ServiceSnapshot）与幂等写原语。收敛决策（何时 create/
// update）在消费方收敛层；本文件只做翻译——「不存在」是收敛的正常输入
//（Exists=false 不是错误），update 以乐观令牌推进（version 取自先前的
// ServiceInspect）。

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"
)

// ServiceSnapshot 是托管服务的实况投影（收敛比对的实况侧——六包投影的
// 并集；消费方只读自己比对用到的字段，其余如实携带不参与决策）。
type ServiceSnapshot struct {
	Exists bool
	// Version 是底座对象版本（乐观令牌）。
	Version uint64
	// Labels 是服务级 label（desired-hash 比对 + 归属识别；恒非 nil）。
	Labels map[string]string
	Image  string
	Args   []string
	Env    []string
	// Hosts 是容器 /etc/hosts 注入。
	Hosts []string
	// Ports 是发布端口面（EndpointSpec 投影；入口部署器的比对位）。
	Ports []swarm.PortConfig
	// Networks 是任务网络挂载目标（ID 形态——创建期名字被 swarm 归一；
	// host 网络任务 = ["host"]）。
	Networks []string
	// Mounts 是挂载（类型化投影：Type/Source/Target/ReadOnly）。
	Mounts []mount.Mount
	// ConfigNames 是任务引用的 swarm config 对象名。
	ConfigNames []string
	// SecretNames / SecretIDs 是容器 secret 引用（比对凭据轮换的判据；
	// ID 形态是 attach 幂等的同锚面）。
	SecretNames []string
	SecretIDs   []string
	// HealthTest 是容器健康检查的 Test 序列（nil = 未设——健康检查是执行
	// 面，漂移必被比对捕获）。
	HealthTest []string
	// Constraints 是放置约束。
	Constraints []string
	// Global 报告服务是否 global 形态。
	Global bool
	// Replicas 是期望副本数（replicated 形态；global 恒 0）。
	Replicas uint64
	// MemoryBytes 是内存限额（0 = 未设）。
	MemoryBytes int64
}

// ServiceInspect 按名取服务实况；缺失返回 Exists=false（不是错误——
// 「不存在」是收敛的正常输入）。
func (c *Client) ServiceInspect(ctx context.Context, name string) (ServiceSnapshot, error) {
	res, err := c.cli.ServiceInspect(ctx, name, mobyclient.ServiceInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ServiceSnapshot{}, nil
		}
		return ServiceSnapshot{}, fmt.Errorf("dockerapi: service inspect %s: %w", name, err)
	}
	return snapshotOf(res.Service), nil
}

// snapshotOf 是 swarm.Service → 投影的纯函数（单测面：字段提取的穷尽
// 矩阵在 dockerapi_test.go）。
func snapshotOf(svc swarm.Service) ServiceSnapshot {
	out := ServiceSnapshot{
		Exists:   true,
		Version:  svc.Version.Index,
		Labels:   map[string]string{},
		Networks: []string{},
	}
	for k, v := range svc.Spec.Labels {
		out.Labels[k] = v
	}
	if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		out.Image = cs.Image
		out.Args = append([]string{}, cs.Args...)
		out.Env = append([]string{}, cs.Env...)
		out.Hosts = append([]string{}, cs.Hosts...)
		out.Mounts = append([]mount.Mount{}, cs.Mounts...)
		for _, cfg := range cs.Configs {
			out.ConfigNames = append(out.ConfigNames, cfg.ConfigName)
		}
		for _, s := range cs.Secrets {
			out.SecretNames = append(out.SecretNames, s.SecretName)
			out.SecretIDs = append(out.SecretIDs, s.SecretID)
		}
		if cs.Healthcheck != nil {
			out.HealthTest = append([]string{}, cs.Healthcheck.Test...)
		}
	}
	if pl := svc.Spec.TaskTemplate.Placement; pl != nil {
		out.Constraints = append([]string{}, pl.Constraints...)
	}
	if svc.Spec.Mode.Global != nil {
		out.Global = true
	}
	if svc.Spec.Mode.Replicated != nil && svc.Spec.Mode.Replicated.Replicas != nil {
		out.Replicas = *svc.Spec.Mode.Replicated.Replicas
	}
	if task := svc.Spec.TaskTemplate; task.Resources != nil && task.Resources.Limits != nil {
		out.MemoryBytes = task.Resources.Limits.MemoryBytes
	}
	if svc.Spec.EndpointSpec != nil {
		out.Ports = append([]swarm.PortConfig{}, svc.Spec.EndpointSpec.Ports...)
	}
	for _, n := range svc.Spec.TaskTemplate.Networks {
		out.Networks = append(out.Networks, n.Target)
	}
	return out
}

// ServiceCreate 创建服务（调用方保证仅缺失时调用）。
func (c *Client) ServiceCreate(ctx context.Context, spec swarm.ServiceSpec) error {
	if _, err := c.cli.ServiceCreate(ctx, mobyclient.ServiceCreateOptions{Spec: spec}); err != nil {
		return fmt.Errorf("dockerapi: service create %s: %w", spec.Name, err)
	}
	return nil
}

// ServiceUpdate 以乐观令牌推进服务。
func (c *Client) ServiceUpdate(ctx context.Context, name string, version uint64, spec swarm.ServiceSpec) error {
	if _, err := c.cli.ServiceUpdate(ctx, name, mobyclient.ServiceUpdateOptions{
		Version: swarm.Version{Index: version},
		Spec:    spec,
	}); err != nil {
		return fmt.Errorf("dockerapi: service update %s: %w", name, err)
	}
	return nil
}

// ServiceRemove 删除服务（幂等：缺失视为成功）。
func (c *Client) ServiceRemove(ctx context.Context, name string) error {
	if _, err := c.cli.ServiceRemove(ctx, name, mobyclient.ServiceRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("dockerapi: service remove %s: %w", name, err)
	}
	return nil
}

// TaskObservation 是一次任务实况观测（健康门与健康观察的输入；本 API 代的
// swarm 任务对象不携带容器健康位——引擎级健康判定经 healthcheck 的 swarm
// 原生闭环落到任务状态，database 包头注的观察纪律）。
type TaskObservation struct {
	// State 逐字镜像底座任务状态（running/failed/rejected/shutdown/...）。
	State string
	// DesiredState 是期望态（running/shutdown/...）——旧任务下线判据。
	DesiredState string
	// Err 是任务失败原因（逐字；失败诊断面的原料）。
	Err string
	// Image 是任务 spec 镜像引用（目标版本判据）。
	Image string
}

// TaskList 返回服务的全部任务观测（含历史；健康门轮询的数据源）。
func (c *Client) TaskList(ctx context.Context, service string) ([]TaskObservation, error) {
	res, err := c.cli.TaskList(ctx, mobyclient.TaskListOptions{
		Filters: mobyclient.Filters{}.Add("service", service),
	})
	if err != nil {
		return nil, fmt.Errorf("dockerapi: task list %s: %w", service, err)
	}
	return taskObservationsOf(res.Items), nil
}

// taskObservationsOf 是任务列表 → 观测集的纯函数（单测面）。
func taskObservationsOf(tasks []swarm.Task) []TaskObservation {
	out := make([]TaskObservation, 0, len(tasks))
	for _, t := range tasks {
		obs := TaskObservation{
			State:        string(t.Status.State),
			DesiredState: string(t.DesiredState),
			Err:          t.Status.Err,
		}
		if t.Spec.ContainerSpec != nil {
			obs.Image = t.Spec.ContainerSpec.Image
		}
		out = append(out, obs)
	}
	return out
}

// TaskAddress 返回服务当前 running 任务在指定网络上的 IP（平台侧消费面的
// 可达拨号地址；overlay VIP 只在网络内可解析，控制面经同网任务 IP 直达）。
// ok=false = 无 running 任务或未挂目标网络（收敛未完成，不是错误）。
func (c *Client) TaskAddress(ctx context.Context, service, networkID string) (ip string, ok bool, err error) {
	res, err := c.cli.TaskList(ctx, mobyclient.TaskListOptions{
		Filters: mobyclient.Filters{}.Add("service", service),
	})
	if err != nil {
		return "", false, fmt.Errorf("dockerapi: task list %s: %w", service, err)
	}
	for _, t := range res.Items {
		if t.Status.State != swarm.TaskStateRunning || t.DesiredState != swarm.TaskStateRunning {
			continue
		}
		for _, nats := range t.NetworksAttachments {
			if nats.Network.ID != networkID || len(nats.Addresses) == 0 {
				continue
			}
			// 地址是 ipam 网段形态（netip.Prefix，如 10.0.0.3/24）——取 IP 段。
			if ip := nats.Addresses[0].Addr(); ip.IsValid() && !ip.IsUnspecified() {
				return ip.String(), true, nil
			}
		}
	}
	return "", false, nil
}

// ReadyNodeAddresses 返回可抓取节点的 advertise 地址集（动态 targets 的
// 节点注册表读面）：State=ready 且 availability=active 的节点取 Status.Addr
// （worker 的节点注册地址 / manager 的 advertise 地址；Status.Addr 罕见缺省
// 时回落 ManagerStatus.Addr）。availability 非 active（drain/pause）不入选。
// 返回已去重的原序清单（渲染序的排序在消费方）。
func (c *Client) ReadyNodeAddresses(ctx context.Context) ([]string, error) {
	res, err := c.cli.NodeList(ctx, mobyclient.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("dockerapi: node list: %w", err)
	}
	return readyNodeAddresses(res.Items), nil
}

// readyNodeAddresses 是 NodeList 条目 → 可抓取地址集的纯投影（单测矩阵在
// dockerapi_test.go，自 metrics/docker.go 迁入）。
func readyNodeAddresses(nodes []swarm.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Status.State != swarm.NodeStateReady || n.Spec.Availability != swarm.NodeAvailabilityActive {
			continue
		}
		addr := n.Status.Addr
		if addr == "" && n.ManagerStatus != nil {
			addr = n.ManagerStatus.Addr
		}
		if addr == "" {
			continue
		}
		out = append(out, addr)
	}
	return out
}
