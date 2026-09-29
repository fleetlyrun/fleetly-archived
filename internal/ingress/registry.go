package ingress

// zot registry 部署器（E1-4；E1 多节点设计 §2.5 + D-MN-5「配置即部署」）：
// base_domain 非空时，幂等部署平台 registry Swarm service——
//
//	服务名     fleetly-registry（集群全局命名空间，fleetly.* 前缀纪律）
//	镜像       zot 钉版（多架构 index digest；R7 + image-prepull 台账）
//	模式/约束  replicated-1，钉 manager 约束（node.labels.fleetly.node-id ==
//	           <manager 平台 ID>——zot 是 placement 绑定的第一个平台级用户）
//	存储       本地命名卷 fleetly-registry-data → /var/lib/registry（不进控制
//	           面备份；镜像可重建 = 重建-重部署）
//	网络       平台 overlay fleetly-system（label fleetly.managed=true），
//	           zot 单挂；Traefik 由本控制器 接入（registry 路由后端 VIP 可达面）
//	鉴权       HTTP Basic：平台生成随机 user/pass 写 registry.auth_file
//	           （`<user>:<password>` 单行 0600——与 ingress token 同形，不入
//	           SQLite），派生 zot 消费的 bcrypt htpasswd 工件与 zot 配置工件
//	           （zot 仅接受 bcrypt/SHA-crypt htpasswd，明文密码本体不进容器）
//	路由       Host(`registry.<base>`) → http://fleetly-registry:5000 进动态
//	           配置（平台路由段，platformRegistryRoute；控制面自有，不属任何
//	           app）；zot overlay 内 5000 明文，不经 Traefik 的内网面保持
//
// 部署时点：startup/sweep 收敛（runRegistryController 独立 goroutine，与平台证书
// 控制器无次序依赖——设计 §2.4 次序⑤「zot 部署不依赖证书」）。base_domain
// 为空时 控制器不启动（单节点 v0.1 形态零成本）。
//
// 与 Traefik 部署器同款幂等收敛：不存在创建、存在比对 spec（镜像/挂载/
// 网络/约束/副本数）差异才更新。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"golang.org/x/crypto/bcrypt"

	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 平台 registry 常量（设计 §2.5 形态表；不走配置面）。
const (
	// RegistryServiceName 是 registry 服务名（集群全局命名空间）。
	RegistryServiceName = "fleetly-registry"
	// RegistryVolumeName 是镜像数据本地命名卷（钉 manager 节点）。
	RegistryVolumeName = "fleetly-registry-data"
	// RegistryNetworkName 是平台 overlay 网络（Traefik 与 zot 的共享面；
	// 应用不进本网络——per-app 网络模型不变）。
	RegistryNetworkName = "fleetly-system"
	// registryLabelRegistry 是 registry 服务自描述 label（CLI/运维识别）。
	registryLabelRegistry = "fleetly.registry"
	// registryDataMountPath 是镜像数据在 zot 容器内的挂载点（zot 缺省
	// storage.rootDirectory，见 registryZotConfig）。
	registryDataMountPath = "/var/lib/registry"
	// registryConfigMountPath 是 zot 配置文件挂载点（镜像 entrypoint 消费
	// serve /etc/zot/config.json——v2.1.21 镜像实况）。
	registryConfigMountPath = "/etc/zot/config.json"
	// registryHTPasswdMountPath 是 htpasswd 工件在容器内的只读挂载点。
	registryHTPasswdMountPath = "/fleetly/registry/htpasswd" //nolint:gosec // G101：挂载路径常量，非凭据
	// RegistryBackendPort 是 zot 监听端口（overlay 内明文 5000；Traefik 经
	// fleetly-system overlay 反代该 VIP 端口）。
	RegistryBackendPort = "5000"
	// registryRetryInterval 是 registry 控制器 的重试退避缺省（与平台证书
	// 控制器同款注入缝——Manager.platformRetryInterval 可覆盖，单测驱动）。
	registryRetryInterval = 30 * time.Second
)

// DefaultZotImage 是 zot 钉版镜像（多架构 OCI index digest 钉定，R7 纪律；
// digest 台账见 docs/runbooks/image-prepull.md）。版本选型（2026-09-20 解析，
// docker buildx imagetools inspect 双向复核）：project-zot/zot 当前最新
// stable release v2.1.21（2026-09-06 发布，GitHub releases 非 prerelease/
// draft 的最新版），其多架构 index digest 与 latest tag 当前所指一致——
// 选 release tag 钉定而非 latest（R7：可变 tag 是供应链反面教材）。升级
// = 镜像钉版换版票（digest + 台账 + 回归），不自动追新。
const DefaultZotImage = "ghcr.io/project-zot/zot:v2.1.21@sha256:6b69512c00dceaad05b1144e6079aac6aa7309d7fd200f9947ecb1de09cf48c8"

// registryUserPrefix 是平台生成 registry 用户名前缀（设计 §2.5：平台生成
// 随机 user/pass；随机段在后缀，凭据文件是唯一真源）。
const registryUserPrefix = "fleetly-"

// registryZotConfig 是 zot 容器的期望配置（确定性 JSON——内容漂移即工件
// 重写 + 服务收敛）。鉴权 = htpasswd（bcrypt，容器内只读挂载点）；扩展
// 全关（平台只需要 registry v2 推拉面——搜索/UI/CVE 扫描非平台用途，
// CVE 库还需外网下载，最小暴露面纪律）。
type registryZotConfig struct {
	Storage struct {
		RootDirectory string `json:"rootDirectory"`
	} `json:"storage"`
	HTTP struct {
		Address string `json:"address"`
		Port    string `json:"port"`
		Auth    struct {
			HTPasswd struct {
				Path string `json:"path"`
			} `json:"htpasswd"`
		} `json:"auth"`
	} `json:"http"`
	Log struct {
		Level string `json:"level"`
	} `json:"log"`
}

// newRegistryZotConfig 构造期望配置。
func newRegistryZotConfig() *registryZotConfig {
	cfg := &registryZotConfig{}
	cfg.Storage.RootDirectory = registryDataMountPath
	cfg.HTTP.Address = "0.0.0.0"
	cfg.HTTP.Port = RegistryBackendPort
	cfg.HTTP.Auth.HTPasswd.Path = registryHTPasswdMountPath
	cfg.Log.Level = "info"
	return cfg
}

// registryCredentials 是部署器持有的一段凭据（auth_file 真源 + 派生工件
// 路径）。
type registryCredentials struct {
	User          string
	Password      string
	authFile      string
	htpasswdFile  string
	zotConfigFile string
}

// registryCredentialPaths 由 auth_file 路径派生工件路径（与凭据文件同目录：
// `<stem>.htpasswd`（zot 消费）与 `<stem>.config.json`（zot 消费）——部署器
// 生成物与凭据同处数据根，0600/0644 分级）。
func registryCredentialPaths(authFile string) (htpasswd, zotConfig string) {
	dir, file := filepath.Split(authFile)
	ext := filepath.Ext(file)
	stem := strings.TrimSuffix(file, ext)
	return filepath.Join(dir, stem+".htpasswd"), filepath.Join(dir, stem+".config.json")
}

// ensureRegistryCredentials 读写凭据文件与派生工件（幂等）：
//  1. auth_file 缺失 → 生成随机 user/pass 落盘 0600（`user:password` 单行
//     ——与 ingress token 同形的平台生成凭据文件，设计 §2.5）；存在 → 解析
//     复用（轮换 = 删除文件 + 服务收敛重建）；
//  2. htpasswd 工件：缺失或既有 hash 与口令不匹配（bcrypt 校验）才重写
//     （bcrypt 每次盐不同——按内容校验写，不按 mtime，保证幂等无 churn）；
//  3. zot 配置工件：内容与期望不一致才重写（确定性 JSON）。
func (m *Manager) ensureRegistryCredentials() (*registryCredentials, error) {
	authFile := m.cfg.RegistryAuthFile
	htFile, zotCfgFile := registryCredentialPaths(authFile)
	creds := &registryCredentials{
		authFile:      authFile,
		htpasswdFile:  htFile,
		zotConfigFile: zotCfgFile,
	}
	raw, err := os.ReadFile(authFile) //nolint:gosec // G304：path 是平台配置的 registry.auth_file（registry.* 键）
	switch {
	case err == nil:
		line := strings.TrimSpace(string(raw))
		user, pass, ok := strings.Cut(line, ":")
		if !ok || user == "" || pass == "" || strings.ContainsAny(line, "\r\n") {
			return nil, fmt.Errorf("ingress: registry credentials %s malformed (want a single user:password line)", authFile)
		}
		creds.User, creds.Password = user, pass
	case errors.Is(err, os.ErrNotExist):
		user, pass, gerr := generateRegistryCredentials()
		if gerr != nil {
			return nil, fmt.Errorf("ingress: generate registry credentials: %w", gerr)
		}
		creds.User, creds.Password = user, pass
		// 数据根目录就位（auth_file 缺省落数据根；显式路径的父目录同样收敛）。
		if merr := os.MkdirAll(filepath.Dir(authFile), 0o750); merr != nil {
			return nil, fmt.Errorf("ingress: create registry credential dir: %w", merr)
		}
		if werr := os.WriteFile(authFile, []byte(user+":"+pass+"\n"), 0o600); werr != nil {
			return nil, fmt.Errorf("ingress: write registry credentials %s: %w", authFile, werr)
		}
		m.log.Info("ingress: registry credentials generated", "file", authFile)
	default:
		return nil, fmt.Errorf("ingress: read registry credentials %s: %w", authFile, err)
	}

	if err := m.ensureHTPasswdArtifact(creds); err != nil {
		return nil, err
	}
	if err := m.ensureZotConfigArtifact(creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// ensureHTPasswdArtifact 收敛 bcrypt htpasswd 工件（zot 消费面——zot 不接受
// 明文 htpasswd，明文口令本体不进容器挂载）。
func (m *Manager) ensureHTPasswdArtifact(creds *registryCredentials) error {
	raw, err := os.ReadFile(creds.htpasswdFile) //nolint:gosec // G304：派生工件路径（auth_file 同目录确定性派生）
	stale := false
	switch {
	case err == nil:
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		match := false
		for _, line := range lines {
			user, hash, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || user != creds.User {
				continue
			}
			if bcrypt.CompareHashAndPassword([]byte(hash), []byte(creds.Password)) == nil {
				match = true
			}
		}
		stale = !match
	case errors.Is(err, os.ErrNotExist):
		stale = true
	default:
		return fmt.Errorf("ingress: read registry htpasswd %s: %w", creds.htpasswdFile, err)
	}
	if !stale {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(creds.Password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("ingress: hash registry password: %w", err)
	}
	content := creds.User + ":" + string(hash) + "\n"
	if err := os.WriteFile(creds.htpasswdFile, []byte(content), 0o600); err != nil {
		return fmt.Errorf("ingress: write registry htpasswd %s: %w", creds.htpasswdFile, err)
	}
	m.log.Info("ingress: registry htpasswd artifact written", "file", creds.htpasswdFile)
	return nil
}

// ensureZotConfigArtifact 收敛 zot 配置工件（确定性 JSON；内容漂移才重写）。
func (m *Manager) ensureZotConfigArtifact(creds *registryCredentials) error {
	want, err := json.MarshalIndent(newRegistryZotConfig(), "", "  ")
	if err != nil {
		return fmt.Errorf("ingress: marshal zot config: %w", err)
	}
	want = append(want, '\n')
	raw, rerr := os.ReadFile(creds.zotConfigFile) //nolint:gosec // G304：派生工件路径（auth_file 同目录确定性派生）
	if rerr == nil && string(raw) == string(want) {
		return nil
	}
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("ingress: read zot config %s: %w", creds.zotConfigFile, rerr)
	}
	// 配置只含路径与端口，无秘密材料——0644（凭据/htpasswd 是 0600）。
	if err := os.WriteFile(creds.zotConfigFile, want, 0o644); err != nil { //nolint:gosec // G306：刻意 0644——内容无秘密材料（上方注释即决策）
		return fmt.Errorf("ingress: write zot config %s: %w", creds.zotConfigFile, err)
	}
	m.log.Info("ingress: registry zot config artifact written", "file", creds.zotConfigFile)
	return nil
}

// generateRegistryCredentials 生成随机 user/pass（crypto/rand；pass 用
// base64rawurl 字形——docker login/htpasswd 全兼容，无转义面）。包级变量
// 形态供单测注入确定性序列。
var generateRegistryCredentials = func() (string, string, error) {
	var userRand [4]byte
	if _, err := rand.Read(userRand[:]); err != nil {
		return "", "", err
	}
	user := registryUserPrefix + hex.EncodeToString(userRand[:])
	var passRand [24]byte
	if _, err := rand.Read(passRand[:]); err != nil {
		return "", "", err
	}
	pass := base64RawURL(passRand[:])
	return user, pass, nil
}

// base64RawURL 无填充 raw url base64（口令字形：字母数字加 -_，全兼容）。
func base64RawURL(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var sb strings.Builder
	bits := 0
	buf := 0
	for _, v := range b {
		buf = buf<<8 | int(v)
		bits += 8
		for bits >= 6 {
			bits -= 6
			sb.WriteByte(alphabet[(buf>>bits)&0x3f])
		}
	}
	if bits > 0 {
		sb.WriteByte(alphabet[(buf<<(6-bits))&0x3f])
	}
	return sb.String()
}

// registryConstraintFor 是 manager 钉定约束（placement.ConstraintFor 同公式
// ——入口层本地重写避免适配器反向依赖放置包；公式由测试钉死，本包不得
// 改写）。zot 是 placement 绑定的第一个平台级用户（设计 §2.5：同一锚）。
func registryConstraintFor(platformNodeID string) string {
	return "node.labels." + state.LabelNodeID + " == " + platformNodeID
}

// EnsureRegistry 幂等收敛平台 registry（D-MN-5：base_domain 配置即部署；
// 单节点 base_domain 空 = no-op）。swarm 未就绪/平台 ID 未铸返回可重试
// 错误（收敛循环退避收敛）。步骤：
//  1. 凭据与派生工件（auth_file/htpasswd/zot 配置——挂载前置物）；
//  2. 数据卷 + fleetly-system overlay；
//  3. 期望 spec（钉 manager 约束）→ inspect → 缺失创建/漂移更新；
//  4. Traefik 接入 fleetly-system（registry.<base> 路由的后端可达面）。
func (m *Manager) EnsureRegistry(ctx context.Context) error {
	if !m.ConfigTLSEnabled() {
		return nil // 单节点 v0.1 形态：零部署、零网络、零挂载
	}
	info, err := m.docker.Info(ctx)
	if err != nil {
		return err
	}
	if !info.SwarmActive {
		return ErrNotSwarmReady
	}
	creds, err := m.ensureRegistryCredentials()
	if err != nil {
		return err
	}
	// ① 数据卷（本地命名卷——swarm 对 task 卷挂载亦有按节点创建语义，
	// 显式收敛让部署器自证前置物存在）。
	if err := m.docker.VolumeEnsure(ctx, RegistryVolumeName); err != nil {
		return err
	}
	// ② 平台 overlay（label fleetly.managed=true；Traefik 与 zot 的共享面）。
	if err := m.docker.NetworkEnsure(ctx, RegistryNetworkName, false); err != nil {
		return err
	}
	netID, err := m.docker.NetworkID(ctx, RegistryNetworkName)
	if err != nil {
		return err
	}
	// ③ manager 平台 ID（meta 单值真源；identity 铸造尚未完成时显式失败
	// 退避重试——约束引用空 ID 会得到永不调度的任务，宁缺毋错）。
	platformID, err := m.store.GetMeta(ctx, state.MetaKeyPlatformNodeID)
	if err != nil {
		return fmt.Errorf("ingress: read platform node id: %w", err)
	}
	if platformID == "" {
		return fmt.Errorf("ingress: platform node id not ensured yet (identity bootstrap pending; registry pin constraint requires it)")
	}
	desired := m.buildRegistrySpec(creds, netID, platformID)
	cur, err := m.docker.ServiceInspect(ctx, RegistryServiceName)
	if err != nil {
		return err
	}
	if !cur.Exists {
		if err := m.docker.ServiceCreate(ctx, desired); err != nil {
			return err
		}
		m.log.Info("ingress: registry service created",
			"image", m.cfg.RegistryImage, "network", RegistryNetworkName,
			"constraint", registryConstraintFor(platformID))
	}
	if cur.Exists && !registrySpecEqual(cur, desired) {
		// 漂移收敛：整份期望 spec 提交（zot 的网络/挂载/约束全部由期望
		// spec 权威表达，无 attach 增量面——与 traefik 的实况合并不同）。
		if err := m.docker.ServiceUpdate(ctx, RegistryServiceName, cur.Version, desired); err != nil {
			return err
		}
		m.log.Info("ingress: registry service updated to desired spec", "image", m.cfg.RegistryImage)
	}
	// ④ Traefik 接入平台 overlay（幂等；路由面就位后 registry.<base> 即
	// 经 443 反代 zot——证书就绪与否只影响 TLS 腿，不影响部署）。
	if err := m.attachNetworkByName(ctx, RegistryNetworkName); err != nil {
		return err
	}
	return nil
}

// buildRegistrySpec 构造 registry 服务的期望 swarm spec（replicated-1 +
// manager 约束 + 单挂 fleetly-system + 卷/bind 挂载；不发布宿主端口——
// overlay 内 5000 明文，对外面只有 Traefik 443 反代）。
func (m *Manager) buildRegistrySpec(creds *registryCredentials, netID, platformID string) swarm.ServiceSpec {
	one := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: RegistryServiceName,
			Labels: map[string]string{
				state.LabelManaged:    state.ManagedLabelValue,
				registryLabelRegistry: "true",
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: m.cfg.RegistryImage,
				Mounts: []mount.Mount{
					{Type: mount.TypeVolume, Source: RegistryVolumeName, Target: registryDataMountPath},
					{Type: mount.TypeBind, Source: creds.htpasswdFile, Target: registryHTPasswdMountPath, ReadOnly: true},
					{Type: mount.TypeBind, Source: creds.zotConfigFile, Target: registryConfigMountPath, ReadOnly: true},
				},
			},
			Networks: []swarm.NetworkAttachmentConfig{{Target: netID}},
			Placement: &swarm.Placement{
				Constraints: []string{registryConstraintFor(platformID)},
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

// registrySpecEqual 幂等比对（镜像/挂载/网络/约束/副本数——registry 的全
// 部执行面都由期望 spec 权威表达；label 不参与比对，服务名即身份）。
func registrySpecEqual(cur dockerapi.ServiceSnapshot, desired swarm.ServiceSpec) bool {
	cs := desired.TaskTemplate.ContainerSpec
	if cur.Image != cs.Image {
		return false
	}
	if len(cur.Mounts) != len(cs.Mounts) {
		return false
	}
	for i, wm := range cs.Mounts {
		cm := cur.Mounts[i]
		if cm.Type != wm.Type || cm.Source != wm.Source || cm.Target != wm.Target || cm.ReadOnly != wm.ReadOnly {
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
	return true
}

// platformRegistryRoute 返回 registry 路由段（E1-4：Host(`registry.<base>`)
// → http://fleetly-registry:5000——设计 §2.5 路由行「控制面自有，不属任何
// app」）。App 取平台证书保留名：publishWithCerts 的按 app 挂证书循环因此
// 零改动复用（平台证书就绪即自动获得 443 路由 + 内联证书段）。Name 覆写
// router/service 键为 fleetly-registry（后端 URL = 键 + :5000 = swarm 服务
// 的 overlay VIP，与 app 路由的后端命名公式同构）。
func (m *Manager) platformRegistryRoute() Route {
	return Route{
		App:     platformCertApp,
		Service: "registry",
		Port:    RegistryBackendPort,
		Domains: []string{"registry." + m.cfg.BaseDomain},
		Name:    RegistryServiceName,
	}
}

// runRegistryController 是 registry 部署的常驻收敛循环（Manager.Run 启动的独立
// goroutine；ctx 取消返回）：
//
//	base_domain 为空 → 不启动（单节点 v0.1 形态零成本，D-MN-5）；
//	否则启动即收敛（失败退避重试——与平台证书控制器 无次序依赖，设计
//	§2.4 次序⑤：zot 不依赖证书），收敛后按续期扫描周期复检漂移。
func (m *Manager) runRegistryController(ctx context.Context) {
	if !m.ConfigTLSEnabled() {
		return
	}
	retry := m.platformRetryInterval
	if retry <= 0 {
		retry = registryRetryInterval
	}
	converged := false
	for {
		err := m.EnsureRegistry(ctx)
		if err == nil {
			if !converged {
				converged = true
				m.log.Info("ingress: registry converged (config-as-deployment; base_domain set)",
					"service", RegistryServiceName, "route", "registry."+m.cfg.BaseDomain)
			}
		} else {
			converged = false
			m.log.Warn("ingress: registry converge deferred (retrying)",
				"error", err, "retry_in", retry.String())
		}
		if !sleepCtx(ctx, retryOrScan(retry, m.cfg.RenewScanInterval, converged)) {
			return
		}
	}
}

// retryOrScan 收敛失败/未收敛走短退避，已收敛走扫描周期（漂移复检节奏）。
func retryOrScan(retry, scan time.Duration, converged bool) time.Duration {
	if converged && scan > 0 {
		return scan
	}
	return retry
}
