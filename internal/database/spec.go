package database

// 库服务的期望 swarm spec 构造与幂等收敛比对（managed-databases §2.1
// 「模板渲染产物 = 平台受管的 Swarm 服务形态」+ rustfs/zot 部署器同款
// 纪律）：dbtemplate.Render 产出 engine.ServiceSpec 投影（确定性渲染，
// desired-hash 免费），本文件把它翻译为 swarm.ServiceSpec——翻译点唯一
// 补齐两件通用投影装不下的事：
//
//  1. SecretReference 的 SecretID 与完整 File UID/GID/Mode（W3 真机教训：
//     留空会让 swarm agent 在任务启动期 strconv 解析空串直接失败；
//     internal/substrate 的通用翻译无 secret 来源，故库收敛器自带翻译）；
//  2. 服务级 label 的期望集（fleetly.managed / fleetly.db / fleetly.process
//     / fleetly.desired-hash——desired-hash 取自渲染投影的 DesiredHash()，
//     与 app 发布面同款落点纪律，service-label 变更不触发任务重建）。
//
// 幂等比对：desired-hash label 与渲染投影哈希不一致 → ServiceUpdate
//（限额变更/镜像升级/凭据轮换/副本伸缩全走该判据——spec 全字段进哈希，
// 漂移必被捕获）。副本数（paused=0 / 其余=1）由收敛器在渲染产物上覆写
// 后再计哈希——dbtemplate.Render 保持纯渲染（恒 1），暂停语义不入模板。

import (
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// buildServiceSpec 把渲染投影翻译为期望 swarm.ServiceSpec。secretIDs 是
// 「secret 名 → 底座对象 ID」的已解析引用集（引擎凭据 secret 在服务收敛
// 前由 ensureCredentialSecret 确保；spec 声明了 secret 而集合缺 ID = 收敛
// 器编码错误，显式报错返回）。instanceMarker 值 = 库实例名（归属自描述
// ——reap 清场/运维识别的选择器锚）。
func buildServiceSpec(desired engine.ServiceSpec, instanceMarker string, secretIDs map[string]string) (swarm.ServiceSpec, error) {
	labels := map[string]string{
		state.LabelManaged:     state.ManagedLabelValue,
		state.LabelDatabase:    instanceMarker,
		state.LabelProcess:     processLabelOf(desired),
		state.LabelDesiredHash: desired.DesiredHash(),
	}
	containerLabels := map[string]string{
		// 容器 label 与 app 面同款归属锚——但库实例无 fleetly.app（独立
		// 资源，绝不进 app 扫描域：引擎对账/漂移/删除扫描都按 fleetly.app
		// 选择器过滤，库服务由此构造性豁免）。
		state.LabelDatabase: instanceMarker,
	}

	var secretRefs []*swarm.SecretReference
	for _, s := range desired.Secrets {
		id, ok := secretIDs[s.SecretName]
		if !ok {
			return swarm.ServiceSpec{}, fmt.Errorf("database: secret %s not ensured before service convergence (converger ordering bug)", s.SecretName)
		}
		secretRefs = append(secretRefs, &swarm.SecretReference{
			SecretID:   id,
			SecretName: s.SecretName,
			// UID/GID/Mode 必须显式置零值安全形态（0:0/0444——docker CLI
			// 同款缺省）：留空字符串/0 会让 swarm agent 在容器启动期以
			// strconv.Atoi 解析空串直接失败（W3 真机 dind 实证，rustfs 同注）。
			File: &swarm.SecretReferenceFileTarget{Name: s.Target, UID: "0", GID: "0", Mode: 0o444},
		})
	}

	container := &swarm.ContainerSpec{
		Image:           desired.Image,
		Labels:          containerLabels,
		Env:             desired.Env,
		Mounts:          mountSpecsOf(desired),
		Secrets:         secretRefs,
		StopGracePeriod: graceOf(desired.StopGracePeriod),
	}
	if len(desired.Command) > 0 {
		container.Command = append([]string(nil), desired.Command...)
	}
	if desired.Healthcheck != nil {
		container.Healthcheck = healthcheckOf(*desired.Healthcheck)
	}

	task := swarm.TaskSpec{ContainerSpec: container}
	for _, n := range desired.Networks {
		task.Networks = append(task.Networks, swarm.NetworkAttachmentConfig{
			Target:  n.Name,
			Aliases: append([]string(nil), n.Aliases...),
		})
	}
	if len(desired.Constraints) > 0 {
		task.Placement = &swarm.Placement{Constraints: append([]string(nil), desired.Constraints...)}
	}
	if desired.Resources != nil {
		task.Resources = &swarm.ResourceRequirements{
			Limits: &swarm.Limit{
				NanoCPUs:    desired.Resources.NanoCPUs,
				MemoryBytes: desired.Resources.MemoryBytes,
			},
		}
	}
	// 重启策略缺省照 engine 侧平台缺省（substrate 同口径：condition=any /
	// delay 5s——Swarm 重启自愈语义，degraded→recovered 观察的前提）。
	policy := &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny}
	if desired.RestartPolicy != nil {
		policy.Condition = swarm.RestartPolicyCondition(desired.RestartPolicy.Condition)
		if desired.RestartPolicy.Delay > 0 {
			d := desired.RestartPolicy.Delay
			policy.Delay = &d
		}
	}
	task.RestartPolicy = policy

	replicas := desired.Replicas
	return swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: desired.Name, Labels: labels},
		TaskTemplate: task,
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   desired.UpdateParallelism,
			Delay:         desired.UpdateDelay,
			FailureAction: swarm.UpdateFailureActionPause,
			Order:         swarm.UpdateOrder(desired.UpdateOrder),
		},
	}, nil
}

// secretSpecOf 构造引擎凭据 secret 的创建载荷（名内嵌值指纹 + fleetly.db
// 归属 label——reap/旧值清场的选择器锚；data 只进创建载荷，绝不进日志/
// 错误——W3 教训与 rustfs 同纪律的延续）。
func secretSpecOf(instanceName, secretName, password string) swarm.SecretSpec {
	return swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: secretName,
			Labels: map[string]string{
				state.LabelManaged:  state.ManagedLabelValue,
				state.LabelDatabase: instanceName,
			},
		},
		Data: []byte(password),
	}
}

// processLabelOf 取进程名（模板服务名——fleetly.process 语义与 app 面同款：
// 应用内进程标识；库实例内即模板服务名）。
func processLabelOf(desired engine.ServiceSpec) string {
	// 渲染投影不带 compose 进程名——服务名末段即模板服务名
	//（fleetly-db-<name>-<service>；dbtemplate 渲染的命名公式）。
	for i := len(desired.Name) - 1; i >= 0; i-- {
		if desired.Name[i] == '-' {
			return desired.Name[i+1:]
		}
	}
	return desired.Name
}

func mountSpecsOf(desired engine.ServiceSpec) []mount.Mount {
	if len(desired.Mounts) == 0 {
		return nil
	}
	out := make([]mount.Mount, 0, len(desired.Mounts))
	for _, m := range desired.Mounts {
		out = append(out, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.VolumeName,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

func healthcheckOf(hc engine.HealthcheckSpec) *container.HealthConfig {
	return &container.HealthConfig{
		Test:        append([]string(nil), hc.Test...),
		Interval:    hc.Interval,
		Timeout:     hc.Timeout,
		Retries:     int(hc.Retries), //nolint:gosec // G115：compose healthcheck retries，量级极小
		StartPeriod: hc.StartPeriod,
	}
}

func graceOf(d time.Duration) *time.Duration {
	if d <= 0 {
		return nil
	}
	return &d
}
