package execrelay

// 托管 fleetly-exec relay 服务的期望 spec 构造与幂等收敛比对（E7 设计
// §2.1/§2.2/§3.3，W5-S6；internal/victorialogs spec 同款纪律）：不存在创建、
// 存在比对（镜像/env/挂载/secret/网络/global/限额）漂移即更新。
//
// 部署形态（设计 §2.1 D-W5-3）：
//   - swarm **global** 服务 `fleetly-exec`——每节点一任务（会话路由按节点）；
//   - **host 网络**：relay 要出站拨控制面 advertise 地址 + 挂本节点 docker
//     sock——host 网络任务出站走宿主栈直达（gwbridge→VPC TCP），无 overlay
//     依赖（W3-F2 类环境下终端仍可用的结构性保证）；不发布任何 host 端口；
//   - `/var/run/docker.sock` 只读 bind 挂载（exec 的 API 通道——socket 连接
//     不经文件写位，RO 保护挂载点替换）；
//   - Swarm secret `fleetly-exec-token`（集群 token，平台生成 48B——明文只
//     存在于 secret，控制面持 sha256 哈希；用户面轮换不做，runbook 记运维
//     路径）挂 `/run/secrets/fleetly-exec-token`；
//   - env：FLEETLY_CONTROL_ADDR=<advertise>:<http 端口>、FLEETLY_CONTROL_
//     TLS_NAME=<ctrl.<base>>（platform TLS 模式才有——wss + ServerName 校验；
//     off/manual = 空 = ws:// 明文，诚实降级，设计 §3.3）。

import (
	"fmt"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台常量（spec 形态表）。
const (
	// relayLabel 是服务自描述 label（运维识别面）。
	relayLabel = "fleetly.execrelay"
	// dockerSockHost / dockerSockTarget 是 docker sock 的宿主路径与容器内
	// 挂载点（RO bind）。
	dockerSockHost  = "/var/run/docker.sock"
	dockerSockMount = "/var/run/docker.sock"
	// relaySecretTarget 是集群 token secret 的容器内挂载路径（relay 缺省
	// 读取路径同锚——DefaultTokenPath）。
	relaySecretTarget = "/run/secrets/fleetly-exec-token" //nolint:gosec // G101：secret 挂载路径常量，非凭据
	// relayMemoryLimitBytes 是内存限额（relay 是事件驱动的小进程——64MB
	// 起步；预算台账 §4「exec 任务 idle 计入全栈台账」）。
	relayMemoryLimitBytes = int64(64) << 20
	// EnvControlAddr / EnvControlTLSName 是 relay 的配置 env 名（relay 侧
	// 读取同锚——单一事实源在本包）。
	EnvControlAddr    = "FLEETLY_CONTROL_ADDR"
	EnvControlTLSName = "FLEETLY_CONTROL_TLS_NAME"
)

// DefaultExecRelayImage 是 relay 的平台镜像引用（E7 设计 §2.2——第二个第
// 一方平台镜像：deploy/Dockerfile.exec 纯 COPY 多架构免 QEMU + CI exec.yml
// cosign 签名，dbtools 同款链路）。CI 首推 2026-09-22（run 35754500342），
// digest 已钉（多架构 index；staging 实拉 RepoDigest 一致）——中间态豁免
// 已摘除，供应链纪律与平台其余镜像同构。
const DefaultExecRelayImage = "ghcr.io/fleetlyrun/fleetly-exec:v0.2.0-exec.1@sha256:4de40017c620b55b76048c3369f64b3a747c875a4f7bf21ac8c6f88bc4b0793b"

// buildSpec 构造 relay 服务的期望 swarm spec（global + host 网络 + docker
// sock RO 挂载 + 集群 token secret + advertise env）。secretID 是底座 secret
// 对象 ID（收敛期由 SecretEnsure/Inspect 解析——spec 构造保持纯函数，ID 是
// 底座会话事实）。
func buildSpec(controlAddr, tlsName, secretID string) swarm.ServiceSpec {
	env := []string{EnvControlAddr + "=" + controlAddr}
	if tlsName != "" {
		env = append(env, EnvControlTLSName+"="+tlsName)
	}
	mem := relayMemoryLimitBytes
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: ExecRelayServiceName,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				relayLabel:         "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: DefaultExecRelayImage,
				Env:   env,
				Mounts: []mount.Mount{
					{Type: mount.TypeBind, Source: dockerSockHost, Target: dockerSockMount, ReadOnly: true},
				},
				Secrets: []*swarm.SecretReference{{
					SecretID:   secretID,
					SecretName: ExecRelaySecretName,
					// UID/GID/Mode 必须显式置零值安全形态（0:0/0444——docker
					// CLI 同款缺省）：留空会让 swarm agent 在任务启动期
					// strconv 解析空串直接失败（W3 真机教训，database/rustfs
					// 同注）。
					File: &swarm.SecretReferenceFileTarget{Name: relaySecretTarget, UID: "0", GID: "0", Mode: 0o444},
				}},
			},
			// host 网络：出站走宿主栈直达控制面 advertise（无 overlay 依赖
			// ——D-W5-3 的可用性结构保证）；网络附件恒为单 host 形态。
			Networks: []swarm.NetworkAttachmentConfig{{Target: "host"}},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: mem},
			},
		},
		// global——每节点一任务（会话路由按节点；relay 任务零发布端口）。
		Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			FailureAction: "pause",
			Order:         "stop-first",
		},
	}
	return spec
}

// specEqual 幂等比对（镜像/env/挂载/secret/网络/global/限额——服务的全部
// 执行面都由期望 spec 权威表达；label 不参与，服务名即身份。env 含
// FLEETLY_CONTROL_ADDR/TLS_NAME——relay 拨号面的漂移必被本比对捕获）。
func specEqual(cur dockerapi.ServiceSnapshot, desired swarm.ServiceSpec) bool {
	cs := desired.TaskTemplate.ContainerSpec
	if cur.Image != cs.Image {
		return false
	}
	if !sameStrings(cur.Env, cs.Env) {
		return false
	}
	if len(cur.Mounts) != len(cs.Mounts) {
		return false
	}
	for i, wm := range cs.Mounts {
		cm := cur.Mounts[i]
		if cm.Source != wm.Source || cm.Target != wm.Target {
			return false
		}
	}
	wantNets := make([]string, 0, len(desired.TaskTemplate.Networks))
	for _, n := range desired.TaskTemplate.Networks {
		wantNets = append(wantNets, n.Target)
	}
	if !sameStrings(cur.Networks, wantNets) {
		return false
	}
	// secret 引用比对（ID + 名成对）。
	if len(cur.SecretIDs) != len(cs.Secrets) || len(cur.SecretNames) != len(cs.Secrets) {
		return false
	}
	for i, ref := range cs.Secrets {
		if cur.SecretIDs[i] != ref.SecretID || cur.SecretNames[i] != ref.SecretName {
			return false
		}
	}
	if cur.Global != (desired.Mode.Global != nil) {
		return false
	}
	wantMem := int64(0)
	if res := desired.TaskTemplate.Resources; res != nil && res.Limits != nil {
		wantMem = res.Limits.MemoryBytes
	}
	return cur.MemoryBytes == wantMem
}

// sameStrings 序列相等（顺序敏感——spec 各面以期望序权威表达）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fmtControlAddr 拼 advertise:port（部署 spec 的 FLEETLY_CONTROL_ADDR 值）。
func fmtControlAddr(advertise, httpPort string) string {
	return fmt.Sprintf("%s:%s", advertise, httpPort)
}
