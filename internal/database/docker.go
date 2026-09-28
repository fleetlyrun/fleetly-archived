// Package database 是库实例的专属收敛器（provisioner，managed-databases
// 设计 §2.1/§2.3，E4 W4-S2）：扫描 db_instances 按生命周期态分派收敛——
// provisioning 走「网络 → 选点钉住 → 卷登记 → 引擎凭据 secret → 模板渲染
// 服务收敛 → 健康门」，ready/degraded 走漂移复检与健康观察，paused 走
// scale-0 保全，deleting 走幂等 reap。状态写全部经 state.EnterDbPhase 单
// 写点（同事务 CAS + 事件）；本包不做任何裸状态写。
//
// duty 形态与 internal/rustfs 同款（常驻 tick 循环 + 事件驱动 kick；API
// 受理生命周期操作后 Kick 立即收敛，不等下一拍）。底座消费面 = 本包私有
// 端口（rustfs dockerPort 同构——引擎凭据 secret 的 SecretReference 需要
// SecretID 与完整 File UID/GID/Mode，engine.ServiceSpec 通用投影装不下，
// W3 真机教训：空 UID/GID/Mode 会让 swarm agent 在任务启动期解析失败）。
//
// 明文纪律：凭据明文只存活于「解密 → 渲染投影 → secret 创建载荷」的内存
// 链；日志/事件/审计/错误文本零出现（负面测试钉死）。
package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	mobyclient "github.com/moby/moby/client"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// ErrNotSwarmReady 表示本机不是 active swarm manager（duty 可重试态；
// rustfs.ErrNotSwarmReady 同语义不共享类型——端口在本包定义）。
var ErrNotSwarmReady = errors.New("docker engine is not an active swarm manager (database duty)")

// TaskObservation 是一次任务实况观测（健康门与健康观察的输入；engine 包
// 的 TaskState 投影同构——本 API 代的 swarm 任务对象不携带容器健康位，
// 引擎级健康判定经 healthcheck 的 swarm 原生闭环落到任务状态：探测连续
// 失败耗尽 retries → 任务 failed + Err。判定语义与引擎健康门同源）。
type TaskObservation struct {
	// State 逐字镜像底座任务状态（running/failed/rejected/shutdown/...）。
	State string
	// DesiredState 是期望态（running/shutdown/...）——旧任务下线判据。
	DesiredState string
	// Err 是任务失败原因（逐字；provisioning 失败落 last_error 的原料——
	// 引擎/调度层文本，不含凭据材料）。
	Err string
	// Image 是任务 spec 镜像引用（目标版本判据——升级换 digest 期间旧任务
	// 的 running 不算新版本健康）。
	Image string
}

// running 期望 running 的任务（当前代——旧代任务 desired=shutdown 不计）。
func (t TaskObservation) running() bool {
	return t.DesiredState == string(swarm.TaskStateRunning)
}

// ServiceState 是库服务的实况投影（收敛比对的实况侧；rustfs ServiceState
// 同构 + labels——desired-hash 比对的落点）。
type ServiceState struct {
	Exists bool
	// Version 是底座对象版本（乐观令牌）。
	Version uint64
	// Labels 是服务级 label（fleetly.desired-hash 比对 + 归属识别）。
	Labels map[string]string
	// Replicas 是期望副本数。
	Replicas uint64
}

// dockerPort 是收敛 duty 对 Docker API 的最小消费面（rustfs dockerPort
// 同款形态：端口在本包定义、moby 实现在本文件、假实现注入单测；第三方
// 类型只进不出——swarm.ServiceSpec 是收敛器构造载荷，出口只有投影与
// error）。服务写幂等语义由收敛层保证（inspect → 比对 → create/update）。
type dockerPort interface {
	// Info 报告 swarm 是否 active。
	Info(ctx context.Context) (bool, error)
	// ServiceInspect 按名取服务实况；缺失返回 Exists=false（不是错误——
	// 「不存在」是收敛的正常输入）。
	ServiceInspect(ctx context.Context, name string) (ServiceState, error)
	// ServiceCreate 创建服务（收敛器保证仅缺失时调用）。
	ServiceCreate(ctx context.Context, spec swarm.ServiceSpec) error
	// ServiceUpdate 以乐观令牌推进服务（version 取自先前的 ServiceInspect）。
	ServiceUpdate(ctx context.Context, name string, version uint64, spec swarm.ServiceSpec) error
	// ServiceRemove 删除服务（幂等：缺失视为成功；删除到对象消失有传播
	// 延迟——reap 以再 inspect 确认消失）。
	ServiceRemove(ctx context.Context, name string) error
	// NetworkEnsure 确认 overlay 网络存在（幂等；缺失创建——managed label，
	// attachable：S3 备份 job 的一次性容器经它入网——库网络是共享网络的
	// 设计口径，§2.4）。
	NetworkEnsure(ctx context.Context, name string) error
	// NetworkRemove 删除网络（幂等：缺失视为成功；仍有端点挂接返回错误
	// ——reap 下一拍重试直至引用方清场）。
	NetworkRemove(ctx context.Context, name string) error // SecretInspect 按名取 swarm secret（引擎凭据 secret 的幂等创建判据；
	// exists=true 时返回对象 ID——服务 spec 的 secret 引用必须携带 ID，
	// 仅名字是 malformed reference；value 不可读——Docker API 从不回吐
	// secret 数据）。
	SecretInspect(ctx context.Context, name string) (id string, exists bool, err error)
	// SecretCreate 创建 swarm secret 并返回其 ID（收敛器保证仅缺失时调用；
	// data 只进创建载荷，绝不进日志/错误）。
	SecretCreate(ctx context.Context, spec swarm.SecretSpec) (string, error)
	// SecretList 按 label 选择器返回 secret 名（reap 清场路径：凭据材料
	// 不残留）。
	SecretList(ctx context.Context, labels map[string]string) ([]string, error)
	// SecretRemove 删除 secret（幂等：缺失视为成功；in-use 返回错误由
	// duty 下一拍重试——服务删除到引用释放有传播延迟）。
	SecretRemove(ctx context.Context, name string) error
	// VolumeEnsure 确认命名卷存在（幂等；缺失创建——数据诞生点显式收敛，
	// swarm 对任务卷挂载亦有按节点创建语义，显式创建让部署器自证前置物
	// 在位；rustfs 同口径）。
	VolumeEnsure(ctx context.Context, name string) error
	// VolumeRemove 删除卷（显式 delete_volumes 的 reap 路径；幂等：缺失
	// 视为成功。平台对库数据卷的默认路径是保留转 orphaned——本原语只在
	// 用户显式选择丢弃数据时触达）。
	VolumeRemove(ctx context.Context, name string) error
	// TaskList 返回服务的全部任务观测（含历史；健康门轮询的数据源）。
	TaskList(ctx context.Context, service string) ([]TaskObservation, error)
	// ContainerRun 启动一次性容器并等待退出（轮换的 ALTER USER 执行体，
	// S4）：create（入库共享网络）→ start → wait(next-exit) → remove。返回
	// 退出码；env/cmd 的凭据材料只进创建载荷，绝不进日志/错误文本。
	ContainerRun(ctx context.Context, in ContainerRunInput) (int, error)
	// JobRun 运行一次性 Swarm job 并等待其完成（S5 备份/恢复/校验执行体，
	// §2.6「执行体」行：replicated-job TotalCompletions=1、restart-policy
	// none、放置约束钉实例绑定节点——cron 一次性 job 同款语义）。完成判定
	// 以任务终态为准（complete=成功；failed/rejected/shutdown=失败），终态
	// 任务的 stdout/stderr 有界采集（restic --json 的结构化输出来源；命令
	// 词表保证凭据零输出），随后删除 job 服务（best-effort，失败不掩盖主
	// 结果——残留由 IsDBJobName 前缀可识别，人工可清）。ctx 取消/超预算 =
	// 失败返回（job 服务同拍删除）。
	JobRun(ctx context.Context, in JobRunInput) (JobRunOutcome, error)
}

// JobMount 是一次性 job 的卷挂载（restore 的 rw 数据卷挂载面）。
type JobMount struct {
	// VolumeName 是 docker 卷名。
	VolumeName string
	// Target 是容器内挂载点。
	Target string
	// ReadOnly 是只读挂载位（restore 必须 rw=false——重放写数据卷）。
	ReadOnly bool
}

// JobRunInput 是一次性 Swarm job 的运行参数（adapters 的命令拼装产物；
// 凭据材料只在 Env——dockerPort 实现零日志纪律与 ContainerRun 同款）。
type JobRunInput struct {
	// Name 是 job 服务名（fleetly-dbjob-*——naming.DBJobName 产物，清扫
	// 各方按前缀豁免的识别面）。
	Name string
	// Image 是钉定镜像引用（DefaultDatabaseToolsImage）。
	Image string
	// Cmd 是容器命令（["sh","-c",script] 形态）。
	Cmd []string
	// Env 是 KEY=VALUE 环境集（RESTIC_*/AWS_*/PGPASSWORD/HOME）。
	Env []string
	// Networks 是挂接的 overlay 网络名（实例共享网络 + rustfs 模式的
	// fleetly-rustfs-net）。
	Networks []string
	// Mounts 是卷挂载（restore 挂数据卷 rw；backup 不挂卷——网络导出）。
	Mounts []JobMount
	// Constraints 是放置约束（["node.id == <绑定节点>"]——远端 local 卷
	// 不可经 manager 读的既有硬约束，备份/恢复 job 同钉）。
	Constraints []string
	// Labels 是服务 label（managed + fleetly.db 归属）。
	Labels map[string]string
}

// JobRunOutcome 是一次性 job 的终态产出（任务侧结论 + 有界输出采集）。
type JobRunOutcome struct {
	// State 是任务终态（complete / failed）。
	State string
	// Err 是任务失败原因（任务 Err 字段逐字；complete 时为空）。
	Err string
	// Stdout 是 stdout+stderr 的有界采集文本（restic --json 解析面；命令
	// 词表保证无凭据——pg_dump 走管道不进日志、restic 进度只含路径）。
	Stdout string
	// ExitCode 是任务容器退出码（-1 = 未知——任务被拒绝/底座错误无退出码）。
	ExitCode int
}

// Success 报告任务是否以 complete 终态成功收场。
func (o JobRunOutcome) Success() bool { return o.State == "complete" }

// ContainerRunInput 是一次性容器的运行参数（轮换 job 的最小面：库共享
// 网络内以服务名可达实例；镜像 = 实例模板钉定镜像——工具随引擎镜像）。
type ContainerRunInput struct {
	// Name 是容器名（fleetly-dbjob-<instance>-rotate-<ulid8>——naming.DBJobName
	// 产物，dbjob 前缀族可识别；IMPL-ARCH-F F-2 起不再误落 fleetly-db- 库
	// 服务族）。
	Name string
	// Image 是钉定镜像引用。
	Image string
	// Cmd 是容器命令（psql ...）。
	Cmd []string
	// Env 是 KEY=VALUE 环境集（PGPASSWORD=<旧密码> 等）。
	Env []string
	// Network 是容器接入的 overlay 网络（库共享网络）。
	Network string
}

// realDockerClient 是 dockerPort 的 moby 实现（连接形态与 rustfs 部署器
// 同款：DOCKER_HOST/本机套接字）。
type realDockerClient struct {
	cli *mobyclient.Client
}

// newRealDockerClient 构造真实客户端（host 空 = FromEnv）。
func newRealDockerClient(host string) (*realDockerClient, error) {
	opts := []mobyclient.Opt{mobyclient.FromEnv}
	if host != "" {
		opts = []mobyclient.Opt{mobyclient.WithHost(host), mobyclient.FromEnv}
	}
	cli, err := mobyclient.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("database: construct docker client: %w", err)
	}
	return &realDockerClient{cli: cli}, nil
}

// Close 释放底层连接（Wire cleanup）。
func (c *realDockerClient) Close() error { return c.cli.Close() }

func (c *realDockerClient) Info(ctx context.Context) (bool, error) {
	res, err := c.cli.Info(ctx, mobyclient.InfoOptions{})
	if err != nil {
		return false, fmt.Errorf("database: docker info: %w", err)
	}
	return res.Info.Swarm.NodeID != "" &&
		res.Info.Swarm.LocalNodeState == swarm.LocalNodeStateActive, nil
}

func (c *realDockerClient) ServiceInspect(ctx context.Context, name string) (ServiceState, error) {
	res, err := c.cli.ServiceInspect(ctx, name, mobyclient.ServiceInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ServiceState{}, nil
		}
		return ServiceState{}, fmt.Errorf("database: service inspect %s: %w", name, err)
	}
	svc := res.Service
	out := ServiceState{Exists: true, Version: svc.Version.Index, Labels: map[string]string{}}
	if svc.Spec.Annotations.Labels != nil {
		for k, v := range svc.Spec.Annotations.Labels {
			out.Labels[k] = v
		}
	}
	if svc.Spec.Mode.Replicated != nil && svc.Spec.Mode.Replicated.Replicas != nil {
		out.Replicas = *svc.Spec.Mode.Replicated.Replicas
	}
	return out, nil
}

func (c *realDockerClient) ServiceCreate(ctx context.Context, spec swarm.ServiceSpec) error {
	if _, err := c.cli.ServiceCreate(ctx, mobyclient.ServiceCreateOptions{Spec: spec}); err != nil {
		return fmt.Errorf("database: service create %s: %w", spec.Name, err)
	}
	return nil
}

func (c *realDockerClient) ServiceUpdate(ctx context.Context, name string, version uint64, spec swarm.ServiceSpec) error {
	if _, err := c.cli.ServiceUpdate(ctx, name, mobyclient.ServiceUpdateOptions{
		Version: swarm.Version{Index: version},
		Spec:    spec,
	}); err != nil {
		return fmt.Errorf("database: service update %s: %w", name, err)
	}
	return nil
}

func (c *realDockerClient) ServiceRemove(ctx context.Context, name string) error {
	if _, err := c.cli.ServiceRemove(ctx, name, mobyclient.ServiceRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("database: service remove %s: %w", name, err)
	}
	return nil
}

func (c *realDockerClient) NetworkEnsure(ctx context.Context, name string) error {
	if _, err := c.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{}); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("database: network inspect %s: %w", name, err)
	}
	if _, err := c.cli.NetworkCreate(ctx, name, mobyclient.NetworkCreateOptions{
		Driver: "overlay",
		// attachable：备份 job（S5 的一次性容器执行体）与库适配器经它入网
		// ——非 attachable overlay 拒绝独立容器挂接（rustfs 同口径）。
		Attachable: true,
		Labels: map[string]string{
			state.LabelManaged: state.ManagedLabelValue,
		},
	}); err != nil {
		if _, ierr := c.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{}); ierr == nil {
			return nil // 并发创建竞态：已存在即成功
		}
		return fmt.Errorf("database: network create %s: %w", name, err)
	}
	return nil
}

func (c *realDockerClient) NetworkRemove(ctx context.Context, name string) error {
	if _, err := c.cli.NetworkRemove(ctx, name, mobyclient.NetworkRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("database: network remove %s: %w", name, err)
	}
	return nil
}

func (c *realDockerClient) SecretInspect(ctx context.Context, name string) (string, bool, error) {
	res, err := c.cli.SecretInspect(ctx, name, mobyclient.SecretInspectOptions{})
	if err == nil {
		return res.Secret.ID, true, nil
	}
	if errdefs.IsNotFound(err) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("database: secret inspect %s: %w", name, err)
}

func (c *realDockerClient) SecretCreate(ctx context.Context, spec swarm.SecretSpec) (string, error) {
	res, err := c.cli.SecretCreate(ctx, mobyclient.SecretCreateOptions{Spec: spec})
	if err != nil {
		return "", fmt.Errorf("database: secret create %s: %w", spec.Name, err)
	}
	return res.ID, nil
}

func (c *realDockerClient) SecretList(ctx context.Context, labels map[string]string) ([]string, error) {
	filters := mobyclient.Filters{}
	for k, v := range labels {
		filters = filters.Add("label", k+"="+v)
	}
	res, err := c.cli.SecretList(ctx, mobyclient.SecretListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("database: secret list: %w", err)
	}
	out := make([]string, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, s.Spec.Name)
	}
	return out, nil
}

func (c *realDockerClient) SecretRemove(ctx context.Context, name string) error {
	if _, err := c.cli.SecretRemove(ctx, name, mobyclient.SecretRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("database: secret remove %s: %w", name, err)
	}
	return nil
}

func (c *realDockerClient) VolumeEnsure(ctx context.Context, name string) error {
	if _, err := c.cli.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{}); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("database: volume inspect %s: %w", name, err)
	}
	if _, err := c.cli.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{
		Driver: "local",
		Name:   name,
		Labels: map[string]string{state.LabelManaged: state.ManagedLabelValue},
	}); err != nil {
		if _, ierr := c.cli.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{}); ierr == nil {
			return nil // 并发创建竞态：已存在即成功
		}
		return fmt.Errorf("database: volume create %s: %w", name, err)
	}
	return nil
}

func (c *realDockerClient) VolumeRemove(ctx context.Context, name string) error {
	if _, err := c.cli.VolumeRemove(ctx, name, mobyclient.VolumeRemoveOptions{}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("database: volume remove %s: %w", name, err)
	}
	return nil
}

func (c *realDockerClient) TaskList(ctx context.Context, service string) ([]TaskObservation, error) {
	res, err := c.cli.TaskList(ctx, mobyclient.TaskListOptions{
		Filters: mobyclient.Filters{}.Add("service", service),
	})
	if err != nil {
		return nil, fmt.Errorf("database: task list %s: %w", service, err)
	}
	out := make([]TaskObservation, 0, len(res.Items))
	for _, t := range res.Items {
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
	return out, nil
}

// ContainerRun 一次性容器执行体（轮换 job，S4）：create → start → wait →
// remove（remove 失败不掩盖主结果——残留由外部 `docker rm` 兜底，容器名
// 可识别）。输出不采集（psql 失败文本可能回显语句材料——退出码 + 阶段
// 上下文已是诚实诊断的最小面，明文纪律优先）。
func (c *realDockerClient) ContainerRun(ctx context.Context, in ContainerRunInput) (int, error) {
	create, err := c.cli.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Name: in.Name,
		Config: &container.Config{
			Image:  in.Image,
			Cmd:    in.Cmd,
			Env:    in.Env,
			Labels: map[string]string{state.LabelManaged: state.ManagedLabelValue},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				in.Network: {},
			},
		},
	})
	if err != nil {
		return -1, fmt.Errorf("database: rotation container create %s: %w", in.Name, err)
	}
	if _, err := c.cli.ContainerStart(ctx, create.ID, mobyclient.ContainerStartOptions{}); err != nil {
		_ = c.removeContainer(ctx, create.ID)
		return -1, fmt.Errorf("database: rotation container start %s: %w", in.Name, err)
	}
	wait := c.cli.ContainerWait(ctx, create.ID, mobyclient.ContainerWaitOptions{
		Condition: container.WaitConditionNextExit,
	})
	select {
	case err := <-wait.Error:
		_ = c.removeContainer(ctx, create.ID)
		return -1, fmt.Errorf("database: rotation container wait %s: %w", in.Name, err)
	case resp := <-wait.Result:
		if resp.Error != nil {
			_ = c.removeContainer(ctx, create.ID)
			return -1, fmt.Errorf("database: rotation container %s: %s", in.Name, resp.Error.Message)
		}
		_, _ = c.cli.ContainerRemove(ctx, create.ID, mobyclient.ContainerRemoveOptions{})
		return int(resp.StatusCode), nil
	case <-ctx.Done():
		_ = c.removeContainer(ctx, create.ID)
		return -1, fmt.Errorf("database: rotation container %s: %w", in.Name, ctx.Err())
	}
}

// removeContainer 移除一次性容器（幂等 best-effort；错误只进返回值由调用
// 方静默忽略——清场失败不掩盖主诊断）。
func (c *realDockerClient) removeContainer(ctx context.Context, id string) error {
	_, err := c.cli.ContainerRemove(ctx, id, mobyclient.ContainerRemoveOptions{Force: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// jobPollInterval 是 job 任务终态的轮询节奏（replicated-job 无 wait 原语
// ——cron 用台账拍等价物；这里 1s 轮询 = 秒级完成感知，远低于任一 job 的
// 执行时长量级）。
const jobPollInterval = time.Second

// jobLogCap 是任务输出的采集上限（restic --json 的 summary 在最后一行，
// 状态消息可多行——1 MiB 有界防失控；超出只损失头部进度行，不损失结论）。
const jobLogCap = 1 << 20

// JobRun 一次性 Swarm job 执行体（端口契约见 dockerPort.JobRun）：建服务
// （replicated-job TotalCompletions=1 + restart none + 放置约束 + 卷挂载）
// → 轮询任务终态 → 有界采集终态任务日志 → 删服务。凭据材料零落日志：
// 本函数不打印 spec/env，采集输出由命令词表保证（见 JobRunOutcome.Stdout）。
func (c *realDockerClient) JobRun(ctx context.Context, in JobRunInput) (JobRunOutcome, error) {
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: in.Name, Labels: in.Labels},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: in.Image,
				// 本 API 代的 ContainerSpec 拆分 Command（可执行）与 Args
				//（参数）——["sh","-c",script] 形态按首元素/余量切分；镜像
				// ENTRYPOINT（dbtools = 官方 postgres 入口脚本）对非 postgres
				// 首参 exec "$@" 透传，root 身份执行（恢复脚本的降权在脚本
				// 内 gosu/su-exec 探测——见 restoreJobScript 注）。
				Command: in.Cmd[:1],
				Args:    append([]string(nil), in.Cmd[1:]...),
				Env:     in.Env,
				Labels:  map[string]string{state.LabelManaged: state.ManagedLabelValue},
			},
			// 失败即 failed 终态、不重试（cron 一次性 job 同口径——备份的
			// 重试语义归编排层：诚实红 + 下一计划窗重试，盲目重启会放大
			// 半程产物）。
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
		},
		// 一次性 replicated-job（substrate 的 Job 翻译同款：TotalCompletions=1
		// / MaxConcurrent=1）。
		Mode: swarm.ServiceMode{
			ReplicatedJob: &swarm.ReplicatedJob{
				MaxConcurrent:    ptrOne(),
				TotalCompletions: ptrOne(),
			},
		},
	}
	for _, m := range in.Mounts {
		spec.TaskTemplate.ContainerSpec.Mounts = append(spec.TaskTemplate.ContainerSpec.Mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.VolumeName,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	for _, n := range in.Networks {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks,
			swarm.NetworkAttachmentConfig{Target: n})
	}
	if len(in.Constraints) > 0 {
		spec.TaskTemplate.Placement = &swarm.Placement{Constraints: in.Constraints}
	}
	if _, err := c.cli.ServiceCreate(ctx, mobyclient.ServiceCreateOptions{Spec: spec}); err != nil {
		return JobRunOutcome{}, fmt.Errorf("database: job service create %s: %w", in.Name, err)
	}
	outcome := c.awaitJob(ctx, in.Name)
	// 服务删除 best-effort（终态已取得——清场失败只留 IsDBJobName 可识别
	// 残留，不掩盖主结果）。
	if _, err := c.cli.ServiceRemove(ctx, in.Name, mobyclient.ServiceRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		if outcome.Err == "" && outcome.State == "complete" {
			outcome.Err = fmt.Sprintf("job service cleanup failed: %v", err)
		}
	}
	return outcome, nil
}

// awaitJob 轮询服务任务至终态并采集输出（complete=成功；failed/rejected/
// shutdown=失败——shutdown 是重启策略 none 下的异常下线，诚实判败）。
func (c *realDockerClient) awaitJob(ctx context.Context, service string) JobRunOutcome {
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()
	for {
		res, err := c.cli.TaskList(ctx, mobyclient.TaskListOptions{
			Filters: mobyclient.Filters{}.Add("service", service),
		})
		if err == nil {
			for _, t := range res.Items {
				switch t.Status.State {
				case swarm.TaskStateComplete:
					return c.collectJobOutcome(ctx, t, "complete", "")
				case swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateShutdown:
					reason := strings.TrimSpace(t.Status.Err)
					if reason == "" {
						reason = "task state " + string(t.Status.State)
					}
					return c.collectJobOutcome(ctx, t, "failed", singleLine(reason))
				}
			}
		}
		// 底座读失败/任务未终态：等下一轮；ctx 超预算在此退出（job 服务
		// 删除由调用方收口）。
		select {
		case <-ctx.Done():
			return JobRunOutcome{State: "failed", Err: singleLine("job budget exceeded (" + ctx.Err().Error() + ")"), ExitCode: -1}
		case <-ticker.C:
		}
	}
}

// collectJobOutcome 采集终态任务的输出（stdout+stderr，有界）并归一结论。
func (c *realDockerClient) collectJobOutcome(ctx context.Context, t swarm.Task, state, jobErr string) JobRunOutcome {
	out := JobRunOutcome{State: state, Err: jobErr, ExitCode: -1}
	if cs := t.Status.ContainerStatus; cs != nil {
		out.ExitCode = int(cs.ExitCode)
	}
	if t.ID != "" {
		res, err := c.cli.TaskLogs(ctx, t.ID, mobyclient.TaskLogsOptions{ShowStdout: true, ShowStderr: true})
		if err == nil {
			raw, _ := io.ReadAll(io.LimitReader(res, jobLogCap))
			_ = res.Close()
			out.Stdout = stripDockerLogHeaders(raw)
		}
	}
	return out
}

// stripDockerLogHeaders 剥离 docker 多路复用流的 8 字节帧头（stdout/stderr
// 流交混时的 raw 形态——JSON 行解析对帧头零容忍，逐帧重拼）。
func stripDockerLogHeaders(raw []byte) string {
	var b strings.Builder
	for i := 0; i+8 <= len(raw); {
		stream := raw[i]
		n := int(raw[i+4])<<24 | int(raw[i+5])<<16 | int(raw[i+6])<<8 | int(raw[i+7])
		i += 8
		if n < 0 || i+n > len(raw) {
			break // 非多路复用形态（已是裸文本）——原样返回剩余
		}
		if stream == 1 || stream == 2 {
			b.Write(raw[i : i+n])
		}
		i += n
	}
	if b.Len() == 0 && len(raw) > 0 {
		return string(raw) // 无帧头 = 裸文本流（TTY 形态）
	}
	return b.String()
}

// ptrOne 返回 1 的指针（replicated-job 计数字面量——Go 无常量指针）。
func ptrOne() *uint64 {
	one := uint64(1)
	return &one
}
