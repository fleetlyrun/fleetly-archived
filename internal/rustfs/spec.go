package rustfs

// 托管 RustFS 服务的期望 spec 构造与幂等比对（E3-5，设计 §2.5；zot 部署器
// 同款纪律——internal/ingress/registry.go）：不存在创建、存在比对（镜像/
// 挂载/网络/约束/副本/限额/env/凭据 secret 引用）漂移即更新。
//
// 凭据注入形态（官方文档口径）：RustFS 支持 RUSTFS_ACCESS_KEY_FILE /
// RUSTFS_SECRET_KEY_FILE 文件注入（docs.rustfs.com「Credential Management：
// File-Based Injection (Docker/Kubernetes Secrets)」，与直注 env 互斥）——
// 服务以 Swarm secret 文件挂载消费（D-S3-7「凭据经 Swarm secret 注入」），
// 凭据明文不进服务 env、不进任务日志面。secret 名内嵌内容指纹
//（fleetly-rustfs-*-<hash8>，api 面 secretFingerprint 同口径）：凭据再生成
// = 新名 secret + 服务 spec 漂移 → 收敛更新，旧 secret 清场。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台 rustfs 常量（设计 §2.5 形态表；不走配置面）。
const (
	// ServiceName 是托管 RustFS 服务名（集群全局命名空间，fleetly.* 前缀）。
	ServiceName = "fleetly-rustfs"
	// VolumeName 是数据本地命名卷（钉 manager——数据重力：控制面状态备份
	// 的便捷目标在控制面同机；禁用保留，再启用复用）。
	VolumeName = "fleetly-rustfs-data"
	// serviceNetworkAlias 是服务在内部网络上的 DNS alias——state.
	// RustfsEndpointURL 的 host 段（http://rustfs:9000）必须与它一致
	//（应用注入面/上传轨都按该端点名解析；测试钉住不变量）。
	serviceNetworkAlias = "rustfs"
	// rustfsLabel 是服务自描述 label（CLI/运维识别 + secret 选择器）。
	rustfsLabel = "fleetly.rustfs"
	// dataMountPath 是数据目录挂载点（官方缺省 RUSTFS_VOLUMES=/data）。
	dataMountPath = "/data"
	// accessKeyFileTarget / secretKeyFileTarget 是凭据 secret 的容器内
	// 挂载路径（官方 _FILE 形态的文档惯例路径 /run/secrets/<name>）。
	accessKeyFileTarget = "/run/secrets/rustfs_access_key"
	secretKeyFileTarget = "/run/secrets/rustfs_secret_key" //nolint:gosec // G101：secret 挂载路径常量，非凭据本体
	// backendPort 是 S3 API 监听端口（RUSTFS_ADDRESS=:9000，官方缺省）。
	backendPort = "9000"
	// memoryLimitBytes 是内存限额（设计 §2.5：对齐 zot 口径 256MB）。
	memoryLimitBytes = int64(256) << 20

	// retryInterval 是 duty 收敛失败的退避缺省（ingress duty 同款注入缝）。
	retryInterval = 30 * time.Second
	// scanInterval 是已收敛后的漂移复检周期。
	scanInterval = 60 * time.Second
)

// DefaultRustFSImage 是托管 RustFS 的钉定镜像（D-S3-10：钉 1.0.x GA 最新
// stable——2026-09-16 发布的 1.0.0，与 latest tag 当前所指同 digest；多架构
// OCI index，amd64/arm64 通吃。解析：2026-09-21 docker buildx imagetools
// inspect rustfs/rustfs:1.0.0 → Digest sha256:8cc98017…，台账见
// docs/runbooks/image-prepull.md。选 release tag 钉定而非 latest（R7：
// 可变 tag 是供应链反面教材）。升级 = 镜像钉版换版票（digest + 台账 +
// 回归），不自动追新。
const DefaultRustFSImage = "rustfs/rustfs:1.0.0@sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff"

// credentials 是托管 RustFS root 凭据的明文形态（只在内存存活；持久层
// 走 envelope——internal/state.rustfscredentials，注入面走 swarm secret）。
type credentials struct {
	AccessKey string
	SecretKey string
}

// fingerprint 是凭据材料的展示指纹（明文 sha256 前 8 hex——只判「是不是
// 那个 secret」，材料零出现；与 statebackup.shortFingerprint 同口径）。
func fingerprint(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:8])
}

// accessKeyAlphabet 是 access key 的字形（官方文档建议：大写字母+数字、
// 不含 `/`——SigV4 credential scope 以 `/` 分段，斜杠会破坏解析）。
const accessKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// generateCredentials 是凭据生成（crypto/rand）：access key 20 字符大写
// base32 字形（100bit）；secret key 20B hex（40 字符，160bit）。包级变量
// 形态供单测注入确定性序列（ingress.generateRegistryCredentials 同型）。
var generateCredentials = func() (credentials, error) {
	access, err := randomString(accessKeyAlphabet, 20)
	if err != nil {
		return credentials{}, fmt.Errorf("rustfs: generate access key: %w", err)
	}
	var secretRaw [20]byte
	if _, err := rand.Read(secretRaw[:]); err != nil {
		return credentials{}, fmt.Errorf("rustfs: generate secret key: %w", err)
	}
	return credentials{AccessKey: access, SecretKey: hex.EncodeToString(secretRaw[:])}, nil
}

// randomString 以 crypto/rand 从 alphabet 取 n 个字符（均匀拒绝采样）。
func randomString(alphabet string, n int) (string, error) {
	out := make([]byte, 0, n)
	max := 256 - (256 % len(alphabet))
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= max {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// accessSecretName / secretSecretName 由凭据明文派生 swarm secret 名
//（内容指纹内嵌：凭据再生成即新名，服务 spec 引用变化驱动收敛）。
func accessSecretName(c credentials) string {
	return "fleetly-rustfs-access-key-" + fingerprint(c.AccessKey)
}

func secretSecretName(c credentials) string {
	return "fleetly-rustfs-secret-key-" + fingerprint(c.SecretKey)
}

// constraintFor 是 manager 钉定约束（与 ingress.registryConstraintFor 同
// 公式：node.labels.<LabelNodeID> == <platformID>——入口层本地重写避免
// 适配器反向依赖，公式由测试钉死）。
func constraintFor(platformNodeID string) string {
	return "node.labels." + state.LabelNodeID + " == " + platformNodeID
}

// secretRef 是服务 spec 的凭据 secret 引用（名字内嵌凭据指纹 + 底座对象
// ID——swarm service create 要求 secret 引用携带 ID，仅名字是 malformed
// reference；Target 是容器内挂载路径）。
type secretRef struct {
	Name  string
	ID    string
	Target string
}

// buildSpec 构造托管 RustFS 服务的期望 swarm spec（replicated-1 + manager
// 约束 + 单挂内部网络（alias=rustfs——端点名解析锚）+ 数据卷 + 内存限额
// 256MB + 凭据 secret 文件挂载；不发布任何 host 端口——网络只在内网）。
//
// env 形态（官方文档口径，docs.rustfs.com）：RUSTFS_ADDRESS=":9000"（S3
// API 监听）；RUSTFS_CONSOLE_ENABLE="false"（管理控制台关闭——平台面只有
// S3 API，最小暴露）；凭据经 _FILE 指向 secret 挂载文件。数据目录用官方
// 缺省 /data（RUSTFS_VOLUMES 不写 = 缺省）。
func buildSpec(netID, platformID string, c credentials, refs []secretRef) swarm.ServiceSpec {
	one := uint64(1)
	mem := memoryLimitBytes
	secretRefs := make([]*swarm.SecretReference, 0, len(refs))
	for _, r := range refs {
		secretRefs = append(secretRefs, &swarm.SecretReference{
			SecretID:   r.ID,
			SecretName: r.Name,
			// UID/GID/Mode 必须显式置零值安全形态（0:0/0444——docker CLI
			// 同款缺省）：留空字符串/0 会让 swarm agent 在容器启动期以
			// strconv.Atoi 解析空串直接失败（真机 dind 实证）。
			File: &swarm.SecretReferenceFileTarget{Name: r.Target, UID: "0", GID: "0", Mode: 0o444},
		})
	}
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: ServiceName,
			Labels: map[string]string{
				state.LabelManaged: state.ManagedLabelValue,
				rustfsLabel:        "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: DefaultRustFSImage,
				Env: []string{
					"RUSTFS_ADDRESS=:" + backendPort,
					"RUSTFS_CONSOLE_ENABLE=false",
					// 官方 _FILE 注入形态：指向 swarm secret 挂载文件
					//（与直注 env 互斥——凭据明文不进 env 值）。
					"RUSTFS_ACCESS_KEY_FILE=" + accessKeyFileTarget,
					"RUSTFS_SECRET_KEY_FILE=" + secretKeyFileTarget,
				},
				Mounts: []mount.Mount{
					{Type: mount.TypeVolume, Source: VolumeName, Target: dataMountPath},
				},
				Secrets: secretRefs,
			},
			Networks: []swarm.NetworkAttachmentConfig{{
				Target:  netID,
				Aliases: []string{serviceNetworkAlias},
			}},
			Placement: &swarm.Placement{
				Constraints: []string{constraintFor(platformID)},
			},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{MemoryBytes: mem},
			},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			FailureAction: "pause",
			Order:         "stop-first",
		},
	}
	return spec
}

// specEqual 幂等比对（镜像/env/挂载/网络/约束/副本/限额/凭据 secret 引用
// ——服务的全部执行面都由期望 spec 权威表达；label 不参与，服务名即身份；
// secret 名内嵌凭据指纹 → 凭据轮换必被本比对捕获）。
func specEqual(cur ServiceState, desired swarm.ServiceSpec) bool {
	cs := desired.TaskTemplate.ContainerSpec
	if cur.Image != cs.Image {
		return false
	}
	if !sameStrings(cur.Env, cs.Env) {
		return false
	}
	if len(cur.MountSources) != len(cs.Mounts) {
		return false
	}
	for i, wm := range cs.Mounts {
		if cur.MountSources[i] != wm.Source || cur.MountTargets[i] != wm.Target {
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
	wantConstraints := []string{}
	if pl := desired.TaskTemplate.Placement; pl != nil {
		wantConstraints = pl.Constraints
	}
	if !sameStrings(cur.Constraints, wantConstraints) {
		return false
	}
	wantReplicas := uint64(0)
	if desired.Mode.Replicated != nil && desired.Mode.Replicated.Replicas != nil {
		wantReplicas = *desired.Mode.Replicated.Replicas
	}
	if cur.Replicas != wantReplicas {
		return false
	}
	wantMem := int64(0)
	if res := desired.TaskTemplate.Resources; res != nil && res.Limits != nil {
		wantMem = res.Limits.MemoryBytes
	}
	if cur.MemoryBytes != wantMem {
		return false
	}
	wantSecrets := make([]string, 0, len(cs.Secrets))
	for _, s := range cs.Secrets {
		wantSecrets = append(wantSecrets, s.SecretName)
	}
	sort.Strings(wantSecrets)
	got := append([]string{}, cur.SecretNames...)
	sort.Strings(got)
	return sameStrings(got, wantSecrets)
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
