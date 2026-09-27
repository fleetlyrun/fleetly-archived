package compose

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/fleetlyrun/fleetly/internal/apperr"
)

// 本文件实现 §2.4 受控子集校验的第一层：在 compose-go canonical dict 上
// 做「逐字段白名单 + 显式拒绝清单 + 受管字段」判定。选择在 dict 层而非
// typed 层做白名单的原因：typed 解码会丢失「键存在但值为零值」的信息
// （如 privileged: false 与未写不可区分），而受控子集契约的对象是书写
// 面本身——写了不支持的字段就要显式报错，不静默放行。
//
// 校验不静默（§2.4）：每处违规产出独立错误并携带违规字段路径上下文。
// 多处违规时报告首个（fail-fast，与 docker stack 逐字段报错习惯一致）。

// topLevelWhitelist 是顶层键白名单（§2.4 支持清单；version 为 compose 规范
// 废弃键、loader 在 schema 校验后删除，此处天然不可见；x-* 扩展键为
// compose 标准扩展位、loader 移入 Extensions，同样不可见；secrets 自 E4
// managed-databases 起入白名单——受控形态见 validateSecretsDict；configs
// 自 T 线 OT-3/IMPL-T1-4 起入白名单——受控形态见 validateConfigsDict）。
var topLevelWhitelist = map[string]bool{
	"name":     true,
	"services": true,
	"networks": true,
	"volumes":  true,
	"secrets":  true,
	"configs":  true,
}

// topLevelRejectList 是顶层键显式拒绝清单（优先于白名单缺省拒绝，message
// 给出契约理由）。
//
// S16-C1 裁决预留口的显式解除（managed-databases §2.7/§6 只增纪律行）：
// `secrets` 两项拒绝条目（顶层 + 服务级）随 E4 W4-S4 移除——原条目注释
// 「v0.2 平台密钥库接入后解除」即本解除的预授权；平台密钥库
// （app_secrets + SecretsService，D-DB-7）已接入，开放面收窄为 external-
// true 形态（值不进 git 仓库是硬边界，file:/environment: 仍拒——拒绝
// 语义移入 validateSecretsDict 的形态校验，不再是整键拒绝）。
var topLevelRejectList = map[string]string{}

// serviceRejectList 是拒绝清单+危险字段的显式条目（优先于通用白名单缺省
// 拒绝，message 给出契约理由）。reason 前缀约定见各条目。
var serviceRejectList = map[string]string{
	// v0.1 拒绝清单（架构 §2.4）
	"depends_on": "rejected field in v0.1 (orchestration order is managed by the platform release pipeline)",
	"extends":    "rejected field in v0.1 (service reuse is unsupported)",
	"include":    "rejected field in v0.1 (multi-file merge is unsupported)",
	"profiles":   "rejected field in v0.1 (services deploy in full; no profile gating)",
	// 服务级 configs 的 v0.1 拒绝条目已随 T 线 OT-3/IMPL-T1-4 移除（原理由
	// 「config injection is carried by environment/secrets」被 Config 资源
	// 取代：compose 标准 configs 节 + 平台明文配置资源，受控形态见
	// validateServiceConfigsDict——顶层 configs 只接受 external: true）。
	// S16-C1 的 secrets 拒绝条目已随 E4 W4-S4 移除（C1 预授权的显式解除，
	// 见 topLevelRejectList 注释）；secrets 入 serviceWhitelist，受控形态
	// 校验在 validateServiceSecretsDict（短语法 / {source,target}，
	// uid/gid/mode 拒绝）。
	// 危险字段（Coolify CVE-2025-34159 根因类；默认拒绝，admin 显式开启
	// + 审计的旁路为后续票 TODO）
	"privileged":          "dangerous field: privileged containers are denied by default (admin bypass TODO)",
	"cap_add":             "dangerous field: Linux capability escalation is denied by default (admin bypass TODO)",
	"cap_drop":            "dangerous field family: Linux capability operations are denied by default (admin bypass TODO)",
	"pid":                 "dangerous field: host PID namespace is denied by default (admin bypass TODO)",
	"devices":             "dangerous field: device mounts are denied by default (admin bypass TODO)",
	"device_cgroup_rules": "dangerous field: device cgroup rules are denied by default (admin bypass TODO)",
	"network_mode":        "host network modes such as network_mode: host are on the reject list (services always go through the app-dedicated network)",
	"ports":               "host port publishing is not in the v0.1 controlled subset (route via expose + fleetly.domains seed, managed as domain resources afterwards)",
	"external_links":      "rejected field in v0.1 (cross-stack links are unsupported)",
	"links":               "legacy links are not in the controlled subset (services reach each other by compose service name)",
	"container_name":      "container names are managed by the platform (Swarm service name fleetly-<app>-<service>)",
}

// serviceWhitelist 是服务级键白名单（§2.4 支持清单逐项）。
var serviceWhitelist = map[string]bool{
	"build":             true,
	"image":             true,
	"command":           true, // §2.4 示例（worker: command: node worker.js）
	"expose":            true, // 路由目标端口
	"labels":            true, // 平台 label 约定载体
	"healthcheck":       true,
	"environment":       true,
	"env_file":          true, // 允许但仅限非密钥（架构 §2.4 密钥行）
	"volumes":           true, // 命名卷挂载（bind/tmpfs 语义层拒绝）
	"networks":          true, // 栈内网络
	"deploy":            true,
	"stop_signal":       true,
	"stop_grace_period": true,
	"secrets":           true, // E4 W4-S4：external secret 挂载（受控形态见 validateServiceSecretsDict）
	"configs":           true, // T 线 OT-3/IMPL-T1-4：external config 挂载（受控形态见 validateServiceConfigsDict）
}

// buildWhitelist：build 段仅保留平台消费子键（§2.4 build 注释只定义
// context/dockerfile 的模式裁决；构建引擎参数不在支持清单）。
var buildWhitelist = map[string]bool{
	"context":    true,
	"dockerfile": true,
}

// healthcheckWhitelist：平台默认值覆盖 interval/timeout/retries/start_period
// 四子字段（5s/3s/3/10s，§2.4）；disable/start_interval 不在支持清单。
var healthcheckWhitelist = map[string]bool{
	"test":         true,
	"interval":     true,
	"timeout":      true,
	"retries":      true,
	"start_period": true,
}

// deployWhitelist：deploy.* 除受管字段外照用（release-semantics §2.8 列举：
// parallelism/delay/restart_policy/resources/replicas/placement），加上
// update_config 与 mode（mode=global 的取值级拒绝见 validateDeployDict，
// M1-4：v0.1 单节点不支持 global）。endpoint_mode/rollback_config/labels
// 不在列举内（rollback 平台侧 opt-in、永不使用 Swarm 原生回滚，D-REL-1）。
var deployWhitelist = map[string]bool{
	"mode":           true,
	"replicas":       true,
	"update_config":  true,
	"resources":      true,
	"restart_policy": true,
	"placement":      true,
}

var updateConfigWhitelist = map[string]bool{
	"order":          true,
	"parallelism":    true,
	"delay":          true,
	"failure_action": true, // 受管字段：必须 pause（校验见后）
	"monitor":        true, // 受管字段：省略或 5s
}

var resourcesWhitelist = map[string]bool{"limits": true}
var resourceLimitsWhitelist = map[string]bool{"cpus": true, "memory": true}
var restartPolicyWhitelist = map[string]bool{
	"condition": true, "delay": true, "max_attempts": true, "window": true,
}
var placementWhitelist = map[string]bool{"constraints": true}

// volumeLongWhitelist 是服务卷挂载允许的子键。短语法经 canonical transform
// 解析为长语法映射，encode 会附带空对象子键（volume/bind）——bind 形态
// 仍由 type=volume 限定拒绝（错误信息更精确）。
var volumeLongWhitelist = map[string]bool{
	"type": true, "source": true, "target": true, "read_only": true,
	"volume": true, "bind": true,
}

// secrets 开放（managed-databases §2.7，D-DB-7，E4 W4-S4）：顶层 `secrets:`
// 仅接受 `{name?, external: true}` 形态（external 必须是字面布尔 true——
// 平台密钥库是唯一值来源，值进 git 仓库是硬边界）；服务级 `secrets:` 仅
// 短语法字符串或 `{source, target}`（uid/gid/mode 拒绝——文件属主/权限
// 由平台固定 0:0/0444）。声明名 / 引用名的字符集校验 = 合法的
// /run/secrets/<name> 文件名（compose 标识符字符集）。

// secretLongWhitelist 是服务级 secret 长语法允许的子键。
var secretLongWhitelist = map[string]bool{"source": true, "target": true}

// topLevelSecretWhitelist 是顶层 secret 定义允许的子键（external-only；
// name 是 external 形态的合法伴随键，平台忽略之——Swarm secret 名由平台
// 按 fleetly-<app>-<name>-<hash8> 命名）。
var topLevelSecretWhitelist = map[string]bool{"name": true, "external": true}

// validSecretName 判定 secret 声明名是否合法（/run/secrets/<name> 文件名
// 安全 + naming.SecretName 组件字符集 [A-Za-z0-9._-]；首字符限字母数字，
// 防点文件/分隔符歧义）。
func validSecretName(name string) bool {
	return validComposeIdentifier(name)
}

// validConfigName 判定 config 声明名是否合法（与 secret 同字符集；config
// 不落 /run/secrets 文件语义——它只进 swarm config 对象名与容器内 target
// 路径）。
func validConfigName(name string) bool {
	return validComposeIdentifier(name)
}

// validComposeIdentifier 是 secret/config 声明名的公共字符集契约
// （^[A-Za-z0-9][A-Za-z0-9._-]*$，≤63；naming 组件字符集 + 文件名字符集
// 的交集；首字符限字母数字，防点文件/分隔符歧义）。
func validComposeIdentifier(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '_' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// validateSecretsDict 校验顶层 secrets 定义：external-only。file:/environment:
// 等任何取值来源拒绝（值不进仓库是硬边界，managed-databases §2.7）；缺
// external 或 external 非布尔 true 拒绝（平台不做本地 secret 生命周期）。
func validateSecretsDict(dict map[string]any) error {
	secsAny, ok := dict["secrets"]
	if !ok || secsAny == nil {
		return nil
	}
	secs, ok := secsAny.(map[string]any)
	if !ok {
		return errCompose("top-level secrets must be a mapping").WithContext("path", "secrets")
	}
	for _, name := range sortedKeys(secs) {
		path := "secrets." + name
		if !validSecretName(name) {
			return errCompose("secret name %q is not a valid secret identifier (must match ^[A-Za-z0-9][A-Za-z0-9._-]*$, max 63 chars: it names the /run/secrets/<name> file inside the container)", name).
				WithContext("path", path)
		}
		def, ok := secs[name].(map[string]any)
		if !ok || def == nil {
			// `secrets: {name:}` 裸键（缺 external）——compose schema 允许、
			// 平台拒绝：无 external 即本地生命周期，平台不承载。
			return errCompose("secret %q must declare external: true (values come from the platform secret store: set them with the secrets API; local file/environment sources are a hard boundary)", name).
				WithContext("path", path)
		}
		for _, key := range sortedKeys(def) {
			if strings.HasPrefix(key, "x-") {
				continue
			}
			if !topLevelSecretWhitelist[key] {
				return errCompose("secret %q, field %q: secret values never live in the repository (file/environment sources are a hard boundary; the platform secret store is the only source — declare external: true and set the value via the secrets API)", name, key).
					WithContext("path", path+"."+key)
			}
		}
		ext, present := def["external"]
		if !present {
			return errCompose("secret %q must declare external: true (values come from the platform secret store; declare the name only and set the value via the secrets API)", name).
				WithContext("path", path+".external")
		}
		if b, isBool := ext.(bool); !isBool || !b {
			return errCompose("secret %q declares external=%v: only the literal boolean true is accepted (external names a value held by the platform secret store, not a local resource)", name, fmt.Sprint(ext)).
				WithContext("path", path+".external")
		}
	}
	return nil
}

// validateServiceSecretsDict 校验服务级 secrets 挂载形态：短语法字符串
// （source=target）或 {source, target} 长语法；uid/gid/mode 拒绝（文件
// 属主与权限由平台固定 0:0/0444——与卷挂载的平台受管口径同型）。
func validateServiceSecretsDict(name, prefix string, secretsAny any) error {
	if secretsAny == nil {
		return nil
	}
	items, ok := secretsAny.([]any)
	if !ok {
		return errCompose("secrets of service %q must be a list", name).WithContext("path", prefix+".secrets")
	}
	for i, item := range items {
		path := fmt.Sprintf("%s.secrets[%d]", prefix, i)
		switch v := item.(type) {
		case string:
			if !validSecretName(v) {
				return errCompose("secret reference %q of service %q is not a valid secret identifier (must match ^[A-Za-z0-9][A-Za-z0-9._-]*$, max 63 chars)", name, v).
					WithContext("path", path)
			}
		case map[string]any:
			for _, key := range sortedKeys(v) {
				if strings.HasPrefix(key, "x-") {
					continue
				}
				if !secretLongWhitelist[key] {
					return errCompose("secret mount of service %q, field %q: uid/gid/mode are managed by the platform (files are mounted root-owned 0444); only source and target are accepted", name, key).
						WithContext("path", path+"."+key)
				}
			}
			source, _ := v["source"].(string)
			if !validSecretName(source) {
				return errCompose("secret mount of service %q has an invalid or missing source (must match ^[A-Za-z0-9][A-Za-z0-9._-]*$, max 63 chars)", name).
					WithContext("path", path+".source")
			}
			if target, present := v["target"]; present {
				t, _ := target.(string)
				if t == "" {
					return errCompose("secret mount target of service %q must be a non-empty file name (mounted as /run/secrets/<target>)", name).
						WithContext("path", path+".target")
				}
			}
		default:
			return errCompose("unsupported secrets entry type for service %q", name).WithContext("path", path)
		}
	}
	return nil
}

// envFileLongWhitelist 是 env_file 长语法允许的子键。
var envFileLongWhitelist = map[string]bool{"path": true, "required": true, "format": true}

// configs 开放（T 线 OT-3 / IMPL-T1-4 Config 资源）：顶层 `configs:` 仅
// 接受 `{name?}` 空定义或 `{name?, external: true}`（external 必须是字面
// 布尔 true——平台 app_configs 是唯一值来源，值进 git 仓库是硬边界，与
// secrets 的 external-only 口径同族）；服务级 `configs:` 仅 `{source,
// target}`（uid/gid/mode 拒绝——文件属主/权限由平台固定 0:0/0444；config
// 恒只读）。形态裁决：**载体用 compose 标准 configs 节**（设计档 OT-3 字面
// 的 `volumes: type: config` 被 compose-go v2.15.0 的卷 type 枚举硬拒——
// bind/volume/tmpfs/cluster/npipe/image 不含 config，见 IMPL-T1-4 审查记录；
// 标准 configs 节与 secrets 面 1:1 同构且是 docker stack 原生形状）。
//
// target 契约（票面：绝对路径、只读、禁撞 /run/secrets 前缀）：必须显式
// 声明（不默认 `/<source>`——隐式根内文件挂载是「劣化 bind」的温床）；
// 绝对路径、不以 '/' 结尾（单文件，不映射目录级文件树）；`/run/secrets`
// 及其子路径拒绝（secret 固定根不可撞）。

// topLevelConfigWhitelist 是顶层 config 定义允许的子键（external-only）。
var topLevelConfigWhitelist = map[string]bool{"name": true, "external": true}

// configLongWhitelist 是服务级 config 长语法允许的子键。
var configLongWhitelist = map[string]bool{"source": true, "target": true}

// validConfigTarget 判定 config 挂载目标是否合法（规范化的绝对路径、单
// 文件、不撞 /run/secrets 固定根）。规范化校验（path.Clean 恒等）拒绝
// `/run//secrets/x`、`/etc/../etc/x` 这类以非规范形态绕过前缀禁撞的书写。
func validConfigTarget(target string) bool {
	if target == "" || !strings.HasPrefix(target, "/") {
		return false
	}
	if path.Clean(target) != target {
		return false
	}
	if strings.HasSuffix(target, "/") {
		return false
	}
	if target == secretMountRoot || strings.HasPrefix(target, secretMountRoot+"/") {
		return false
	}
	return true
}

// secretMountRoot 是 secret 固定挂载根（/run/secrets；config target 禁撞）。
const secretMountRoot = "/run/secrets"

// validateConfigsDict 校验顶层 configs 定义：external-only（value 不进仓库
// 是硬边界；本地 file/content/environment 来源拒绝）。
func validateConfigsDict(dict map[string]any) error {
	cfgsAny, ok := dict["configs"]
	if !ok || cfgsAny == nil {
		return nil
	}
	cfgs, ok := cfgsAny.(map[string]any)
	if !ok {
		return errCompose("top-level configs must be a mapping").WithContext("path", "configs")
	}
	for _, name := range sortedKeys(cfgs) {
		path := "configs." + name
		if !validConfigName(name) {
			return errCompose("config name %q is not a valid config identifier (must match ^[A-Za-z0-9][A-Za-z0-9._-]*$, max 63 chars)", name).
				WithContext("path", path)
		}
		def, ok := cfgs[name].(map[string]any)
		if !ok || def == nil {
			// `configs: {name:}` 裸键（缺 external）——拒绝：无 external 即
			// 本地内容生命周期，平台不承载。
			return errCompose("config %q must declare external: true (values come from the platform config store: set them with the configs API; local file/content sources are a hard boundary)", name).
				WithContext("path", path)
		}
		for _, key := range sortedKeys(def) {
			if strings.HasPrefix(key, "x-") {
				continue
			}
			if !topLevelConfigWhitelist[key] {
				return errCompose("config %q, field %q: config content never lives in the repository (file/content/environment sources are a hard boundary; the platform config store is the only source — declare external: true and set the value via the configs API)", name, key).
					WithContext("path", path+"."+key)
			}
		}
		ext, present := def["external"]
		if !present {
			return errCompose("config %q must declare external: true (values come from the platform config store; declare the name only and set the value via the configs API)", name).
				WithContext("path", path+".external")
		}
		if b, isBool := ext.(bool); !isBool || !b {
			return errCompose("config %q declares external=%v: only the literal boolean true is accepted (external names a value held by the platform config store, not a local resource)", name, fmt.Sprint(ext)).
				WithContext("path", path+".external")
		}
	}
	return nil
}

// validateServiceConfigsDict 校验服务级 configs 挂载形态：canonical transform
// 把短语法字符串归一为 `{source}`（无 target）；平台要求显式 {source,
// target} 长语法（uid/gid/mode 拒绝——平台固定 0:0/0444，config 恒只读）。
func validateServiceConfigsDict(name, prefix string, configsAny any) error {
	if configsAny == nil {
		return nil
	}
	items, ok := configsAny.([]any)
	if !ok {
		return errCompose("configs of service %q must be a list", name).WithContext("path", prefix+".configs")
	}
	for i, item := range items {
		path := fmt.Sprintf("%s.configs[%d]", prefix, i)
		v, isMap := item.(map[string]any)
		if !isMap {
			// 短语法（字符串，canonical 化为 {source}）与任何非映射形态：
			// 显式 target 是平台契约（不默认 /<source>）。
			return errCompose("config mount of service %q must be the long syntax {source: <config>, target: <absolute path>} (a bare config reference has no target path and is not in the controlled subset)", name).
				WithContext("path", path)
		}
		for _, key := range sortedKeys(v) {
			if strings.HasPrefix(key, "x-") {
				continue
			}
			if !configLongWhitelist[key] {
				return errCompose("config mount of service %q, field %q: uid/gid/mode are managed by the platform (files are mounted root-owned 0444 and configs are always read-only); only source and target are accepted", name, key).
					WithContext("path", path+"."+key)
			}
		}
		source, _ := v["source"].(string)
		if !validConfigName(source) {
			return errCompose("config mount of service %q has an invalid or missing source (must match ^[A-Za-z0-9][A-Za-z0-9._-]*$, max 63 chars)", name).
				WithContext("path", path+".source")
		}
		target, present := v["target"]
		t, _ := target.(string)
		if !present || t == "" {
			return errCompose("config mount of service %q must declare an absolute target path (configs mount a single read-only file at an explicit path; no implicit target)", name).
				WithContext("path", path+".target")
		}
		if !validConfigTarget(t) {
			if t == secretMountRoot || strings.HasPrefix(t, secretMountRoot+"/") {
				return errCompose("config mount target %q of service %q hits the secret mount root %s (secrets are mounted at fixed /run/secrets paths; configs must use a different absolute path)", t, name, secretMountRoot).
					WithContext("path", path+".target")
			}
			return errCompose("config mount target %q of service %q must be an absolute single-file path in canonical form (starts with '/', no trailing '/', no '../duplicate separators': configs do not map directory trees)", t, name).
				WithContext("path", path+".target")
		}
	}
	return nil
}

// networkDefReject：顶层网络定义拒绝的键（外部网络在 v0.1 拒绝清单；name
// 覆写与平台命名纪律冲突——网络名由平台按 app 专属命名）。
// 其余 compose 标准网络键（driver/driver_opts/ipam/internal/attachable/
// labels）按「栈内网络」支持面放行。
var networkDefReject = map[string]string{
	"external": "external networks are on the v0.1 reject list (the app-dedicated overlay network is created by the platform)",
	"name":     "network resource names are managed by the platform; name overrides are not accepted",
}

// volumeDefReject：顶层卷定义拒绝的键（卷由平台卷注册表管理）。
var volumeDefReject = map[string]string{
	"external": "external volumes are unsupported (app volumes are created and registered per app by the platform)",
	"name":     "volume resource names are managed by the platform (fleetly-<app>-<key>); name overrides are not accepted",
}

// specNamePattern 校验顶层 name 形态（与 compose 项目名字符集一致）。
var specNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validSpecName 判定应用标识是否合法。
func validSpecName(name string) bool { return specNamePattern.MatchString(name) }

// validateDict 在 canonical dict 上执行受控子集校验。abs 仅用于错误信息
// 定位；返回首个违规（fail-fast）。
func validateDict(abs string, dict map[string]any) error {
	// 顶层 name = 应用标识（平台按它命名 Swarm 资源 fleetly-<app>-*）。
	// 缺失在 loader 层已报错（schema 校验开启 + SkipNormalization 组合）；
	// 此处校验形态：小写字母/数字开头，仅含小写字母/数字/-/_（与 compose
	// 项目名字符集一致，保证 Swarm/卷/网络命名安全）。
	name, _ := dict["name"].(string)
	if name == "" {
		return errCompose("compose is missing the top-level name (the app identity, e.g. name: my-api)").WithContext("path", "name")
	}
	if !validSpecName(name) {
		return errCompose("invalid top-level compose name %q (must match ^[a-z0-9][a-z0-9_-]*$: starts with a lowercase letter or digit, only lowercase letters/digits/-/_ allowed)", name).
			WithContext("path", "name")
	}
	// 保留字撞键校验已随 v0.3 命名三段化退役（rbac-teams §4.3 保留字迁移）：
	// 命名公式以 team·prj slug 为前两段，app 名不再紧邻 fleetly- 前缀——
	// 与平台组件命名空间的撞键面结构性消失（E_APP_NAME_RESERVED 退役，
	// 8 保留字迁 team slug 清单，E_TEAM_SLUG_RESERVED 在团队受理层消费；
	// internal/naming 保留字表保留证据链）。

	for _, key := range sortedKeys(dict) {
		if strings.HasPrefix(key, "x-") {
			continue
		}
		if reason, rejected := topLevelRejectList[key]; rejected {
			return errCompose("compose top-level field %q: %s", key, reason).
				WithContext("path", key)
		}
		if !topLevelWhitelist[key] {
			return errCompose("compose top-level field %q is not in the controlled subset (supported: name/services/networks/volumes/secrets/configs)", key).
				WithContext("path", key)
		}
	}

	servicesDict, _ := dict["services"].(map[string]any)
	if len(servicesDict) == 0 {
		return errCompose("compose declares no services (services must not be empty)").WithContext("path", "services")
	}

	// 顶层 secrets 声明名集合（服务级引用完整性哨兵的判定面；external-only
	// 形态校验在 validateSecretsDict）。
	declaredSecrets := map[string]bool{}
	if secs, ok := dict["secrets"].(map[string]any); ok {
		for name := range secs {
			declaredSecrets[name] = true
		}
	}
	// 顶层 configs 声明名集合（T 线 OT-3：服务级引用的声明面自洽哨兵；
	// app_configs 值的存在性在发布引擎 preparing 期前哨 E_CONFIG_NOT_FOUND）。
	declaredConfigs := map[string]bool{}
	if cfgs, ok := dict["configs"].(map[string]any); ok {
		for name := range cfgs {
			declaredConfigs[name] = true
		}
	}

	for _, name := range sortedKeys(servicesDict) {
		svcDict, ok := servicesDict[name].(map[string]any)
		if !ok {
			return errCompose("definition of service %q must be a mapping", name).WithContext("path", "services."+name)
		}
		if err := validateServiceDict(name, svcDict); err != nil {
			return err
		}
		// 引用完整性哨兵：服务级 secret 的 source 必须在顶层 secrets 声明
		//（compose 引用语义；平台侧值的解析在发布引擎——app_secrets 缺失
		// → 部署 preflight E_SECRET_NOT_FOUND，此处只校验声明面自洽）。
		items, _ := svcDict["secrets"].([]any)
		for i, item := range items {
			source := ""
			switch v := item.(type) {
			case string:
				source = v
			case map[string]any:
				source, _ = v["source"].(string)
			}
			if !declaredSecrets[source] {
				return errCompose("service %q references secret %q which is not declared in the top-level secrets section (declare it as %q: {external: true} and set the value via the secrets API)", name, source, source).
					WithContext("path", fmt.Sprintf("services.%s.secrets[%d]", name, i))
			}
		}
		// 引用完整性哨兵：服务级 config 的 source 必须在顶层 configs 声明
		//（compose 引用语义；app_configs 缺失 → 部署 preflight
		// E_CONFIG_NOT_FOUND，此处只校验声明面自洽——secrets 同款分层）。
		cfgItems, _ := svcDict["configs"].([]any)
		for i, item := range cfgItems {
			v, _ := item.(map[string]any)
			source, _ := v["source"].(string)
			if !declaredConfigs[source] {
				return errCompose("service %q references config %q which is not declared in the top-level configs section (declare it as %q: {external: true} and set the value via the configs API)", name, source, source).
					WithContext("path", fmt.Sprintf("services.%s.configs[%d]", name, i))
			}
		}
	}

	if err := validateNetworksDict(dict); err != nil {
		return err
	}
	if err := validateVolumesDict(dict); err != nil {
		return err
	}
	if err := validateSecretsDict(dict); err != nil {
		return err
	}
	if err := validateConfigsDict(dict); err != nil {
		return err
	}
	return nil
}

// validateServiceDict 对单个服务执行白名单/拒绝清单/受管字段校验。
func validateServiceDict(name string, svc map[string]any) error {
	prefix := "services." + name
	for _, key := range sortedKeys(svc) {
		if strings.HasPrefix(key, "x-") {
			continue
		}
		if reason, rejected := serviceRejectList[key]; rejected {
			return errCompose("service %q uses an unsupported field %q: %s", name, key, reason).
				WithContext("path", prefix+"."+key)
		}
		if !serviceWhitelist[key] {
			return errCompose("field %q of service %q is not in the controlled-subset support list", name, key).
				WithContext("path", prefix+"."+key)
		}
	}

	// 服务必须有运行体：image 或 build 至少声明一项（§2.4：build/image 两
	// 种模式；两者皆无则无物可部署）。
	_, hasImage := svc["image"]
	_, hasBuild := svc["build"]
	if !hasImage && !hasBuild {
		return errCompose("service %q declares neither image nor build (nothing to run)", name).WithContext("path", prefix)
	}

	if buildDict, ok := svc["build"].(map[string]any); ok {
		if err := checkSubKeys(name, prefix+".build", "build", buildDict, buildWhitelist); err != nil {
			return err
		}
	}
	if hcDict, ok := svc["healthcheck"].(map[string]any); ok {
		if err := checkSubKeys(name, prefix+".healthcheck", "healthcheck", hcDict, healthcheckWhitelist); err != nil {
			return err
		}
	}
	if envDict, ok := svc["environment"].(map[string]any); ok {
		// 变量插值关闭 → 裸键（无值，期望从宿主环境透传）无法确定性归一化，
		// 显式拒绝。
		for _, k := range sortedKeys(envDict) {
			if envDict[k] == nil {
				return errCompose("environment entry %q of service %q has no literal value (fleetly disables variable interpolation and environment pass-through)", name, k).
					WithContext("path", prefix+".environment."+k)
			}
		}
	} else if envList, ok := svc["environment"].([]any); ok {
		// 列表形态（canonical transform 不改写 environment）：每项必须是
		// "KEY=VALUE" 字面赋值；裸键（透传）拒绝，理由同上。
		for i, e := range envList {
			s, _ := e.(string)
			if !strings.Contains(s, "=") {
				return errCompose("environment entry %q of service %q has no literal value (fleetly disables variable interpolation and environment pass-through)", name, s).
					WithContext("path", fmt.Sprintf("%s.environment[%d]", prefix, i))
			}
		}
	}
	if err := validateEnvFileDict(name, prefix, svc["env_file"]); err != nil {
		return err
	}
	if err := validateServiceVolumesDict(name, prefix, svc["volumes"]); err != nil {
		return err
	}
	if err := validateServiceNetworksDict(name, prefix, svc["networks"]); err != nil {
		return err
	}
	if err := validateServiceSecretsDict(name, prefix, svc["secrets"]); err != nil {
		return err
	}
	if err := validateServiceConfigsDict(name, prefix, svc["configs"]); err != nil {
		return err
	}
	if err := validateDeployDict(name, prefix, svc); err != nil {
		return err
	}
	return nil
}

// validateDeployDict 校验 deploy 段：白名单 + 受管字段 + 更新策略安全性。
// svcDict 为整个服务定义（order 安全性判定需要卷挂载与 mode 信息）。
func validateDeployDict(name, prefix string, svcDict map[string]any) error {
	deployAny, present := svcDict["deploy"]
	if !present || deployAny == nil {
		return nil
	}
	deploy, ok := deployAny.(map[string]any)
	if !ok {
		return errCompose("deploy of service %q must be a mapping", name).WithContext("path", prefix+".deploy")
	}
	if err := checkSubKeys(name, prefix+".deploy", "deploy", deploy, deployWhitelist); err != nil {
		return err
	}

	// M1-4（v0.1 诚实裁决）：deploy.mode: global 显式拒绝——单节点拓扑下
	// global 的副本语义（每节点一实例，单机即恒 1）与失败停机语义（scale=0
	// 对 global 无效，首发失败后崩溃循环不会停）均未实现；放行只会得到
	// 无法停机的失败现场（对齐 C1 secrets 先例：Load 期即拒优于发布期晚败
	// 且误导）。v0.2 多节点开放后解除。
	if mode, present := deploy["mode"]; present {
		if s, _ := mode.(string); s == "global" {
			return apperr.New("E_COMPOSE_UNSUPPORTED",
				"service %q declares deploy.mode: global: single-node v0.1 does not support global mode (replica semantics and fail-stop semantics are unimplemented; available in v0.2 — use replicated + replicas instead)", name).
				WithContext("path", prefix+".deploy.mode")
		}
	}

	if ucAny, ok := deploy["update_config"]; ok && ucAny != nil {
		uc, ok := ucAny.(map[string]any)
		if !ok {
			return errCompose("deploy.update_config of service %q must be a mapping", name).
				WithContext("path", prefix+".deploy.update_config")
		}
		if err := checkSubKeys(name, prefix+".deploy.update_config", "update_config", uc, updateConfigWhitelist); err != nil {
			return err
		}
		// 受管字段政策（release-semantics §2.8）：校验拒绝、不静默覆盖。
		if fa, present := uc["failure_action"]; present {
			if s, _ := fa.(string); s != "pause" {
				return apperr.New("E_COMPOSE_MANAGED_FIELD",
					"deploy.update_config.failure_action=%q of service %q violates platform governance (must be pause or omitted — the release failure action is fixed to pause by the platform)", name, fmt.Sprint(fa)).
					WithContext("path", prefix+".deploy.update_config.failure_action")
			}
		}
		if mon, present := uc["monitor"]; present {
			if s, _ := mon.(string); s != "5s" {
				return apperr.New("E_COMPOSE_MANAGED_FIELD",
					"deploy.update_config.monitor=%q of service %q violates platform governance (must be 5s or omitted — the update monitor window is fixed by the platform)", name, fmt.Sprint(mon)).
					WithContext("path", prefix+".deploy.update_config.monitor")
			}
		}
		// 更新顺序安全性：有卷服务强制 stop-first（发布降级边界：双任务并发
		// 挂同一本地卷有数据风险），显式 start-first 冲突 → UNSAFE_STRATEGY。
		//（global + start-first 的同型检查已随 M1-4 删除——mode: global 在
		// 本函数更早处整体拒绝，该分支不可达。）
		if order, present := uc["order"]; present {
			if s, _ := order.(string); s == "start-first" && serviceHasVolumes(svcDict) {
				return apperr.New("E_COMPOSE_UNSAFE_STRATEGY",
					"service %q mounts named volumes; explicit start-first conflicts with the platform-enforced stop-first (two tasks mounting the same local volume concurrently risk data corruption)", name).
					WithContext("path", prefix+".deploy.update_config.order")
			}
		}
	}

	if resAny, ok := deploy["resources"]; ok && resAny != nil {
		res, ok := resAny.(map[string]any)
		if !ok {
			return errCompose("deploy.resources of service %q must be a mapping", name).
				WithContext("path", prefix+".deploy.resources")
		}
		if err := checkSubKeys(name, prefix+".deploy.resources", "resources", res, resourcesWhitelist); err != nil {
			return err
		}
		if limitsAny, ok := res["limits"]; ok && limitsAny != nil {
			limits, ok := limitsAny.(map[string]any)
			if !ok {
				return errCompose("deploy.resources.limits of service %q must be a mapping", name).
					WithContext("path", prefix+".deploy.resources.limits")
			}
			if err := checkSubKeys(name, prefix+".deploy.resources.limits", "limits", limits, resourceLimitsWhitelist); err != nil {
				return err
			}
		}
	}

	if rpAny, ok := deploy["restart_policy"]; ok && rpAny != nil {
		rp, ok := rpAny.(map[string]any)
		if !ok {
			return errCompose("deploy.restart_policy of service %q must be a mapping", name).
				WithContext("path", prefix+".deploy.restart_policy")
		}
		if err := checkSubKeys(name, prefix+".deploy.restart_policy", "restart_policy", rp, restartPolicyWhitelist); err != nil {
			return err
		}
	}

	if placeAny, ok := deploy["placement"]; ok && placeAny != nil {
		place, ok := placeAny.(map[string]any)
		if !ok {
			return errCompose("deploy.placement of service %q must be a mapping", name).
				WithContext("path", prefix+".deploy.placement")
		}
		if err := checkSubKeys(name, prefix+".deploy.placement", "placement", place, placementWhitelist); err != nil {
			return err
		}
		if constraints, ok := place["constraints"].([]any); ok {
			for i, c := range constraints {
				expr, _ := c.(string)
				if !placementConstraintAllowed(expr) {
					return errCompose("placement constraint %q of service %q is outside the allowed namespace (only node.labels.fleetly.*)", name, expr).
						WithContext("path", fmt.Sprintf("%s.deploy.placement.constraints[%d]", prefix, i))
				}
			}
		}
	}
	return nil
}

// serviceHasVolumes 判断服务是否声明了卷挂载（canonical dict 层：长语法
// 映射或短语法字符串）。
func serviceHasVolumes(svc map[string]any) bool {
	switch v := svc["volumes"].(type) {
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return false
	}
}

// placementConstraintAllowed 判定单条 Swarm 约束表达式是否落在
// node.labels.fleetly.* 命名空间（stateful-placement §2.3）。支持
// ==/!=/in/notin 运算形态；无法识别的形态一律拒绝（白名单纪律）。
func placementConstraintAllowed(expr string) bool {
	s := strings.TrimSpace(expr)
	ops := []string{"==", "!=", " notin ", " in ", "notin ", "in "}
	head := ""
	for _, op := range ops {
		if idx := strings.Index(s, op); idx > 0 {
			head = strings.TrimSpace(s[:idx])
			break
		}
	}
	if head == "" {
		// 纯存在性约束（如 node.labels.fleetly.rack）也视为合法头。
		head = strings.TrimSuffix(s, " ")
	}
	return strings.HasPrefix(head, "node.labels.fleetly.")
}

// validateEnvFileDict 校验 env_file 形态。canonical transform（transformEnvFile）
// 把所有形态归一为「映射列表」：字符串项 → {path, required: true}，映射项
// 补 required 缺省；白名单子键 {path, required, format}。
func validateEnvFileDict(name, prefix string, envFileAny any) error {
	if envFileAny == nil {
		return nil
	}
	items, ok := envFileAny.([]any)
	if !ok {
		// canonical 化后理论上不可达；兜底拒绝异常形态。
		return errCompose("unsupported env_file form for service %q (expected a string or a list)", name).
			WithContext("path", prefix+".env_file")
	}
	for i, item := range items {
		path := fmt.Sprintf("%s.env_file[%d]", prefix, i)
		m, ok := item.(map[string]any)
		if !ok {
			return errCompose("unsupported env_file list item type for service %q", name).WithContext("path", path)
		}
		if err := checkSubKeysAt(name, path, "env_file", m, envFileLongWhitelist); err != nil {
			return err
		}
	}
	return nil
}

// validateServiceVolumesDict 校验服务卷挂载形态。canonical transform
// （transformVolumeMount）把短语法解析为长语法映射：{type, source, target,
// read_only?}；不可解析形态（ignoreParseError）保留字符串、由 typed 解码
// 兜底报错。type 限定 volume（bind/tmpfs 拒绝）；宿主 bind 与 docker.sock
// 的语义判定在 typed 层补刀。
func validateServiceVolumesDict(name, prefix string, volumesAny any) error {
	if volumesAny == nil {
		return nil
	}
	items, ok := volumesAny.([]any)
	if !ok {
		return errCompose("volumes of service %q must be a list", name).WithContext("path", prefix+".volumes")
	}
	for i, item := range items {
		path := fmt.Sprintf("%s.volumes[%d]", prefix, i)
		switch v := item.(type) {
		case string:
			// canonical 化残留的不可解析短语法：仅接受 mode 段 ro/rw。
			parts := strings.Split(v, ":")
			if len(parts) == 3 {
				mode := strings.TrimSpace(parts[2])
				if mode != "ro" && mode != "rw" {
					return errCompose("unsupported volume mount option %q for service %q (only ro/rw)", name, parts[2]).WithContext("path", path)
				}
			}
		case map[string]any:
			if err := checkSubKeysAt(name, path, "volumes", v, volumeLongWhitelist); err != nil {
				return err
			}
			if t, present := v["type"]; present {
				if s, _ := t.(string); s != "volume" {
					return errCompose("unsupported volume mount type=%q for service %q (v0.1 allows named volumes only; bind/tmpfs are rejected)", name, fmt.Sprint(t)).
						WithContext("path", path+".type")
				}
			}
		default:
			return errCompose("unsupported volume mount entry type for service %q", name).WithContext("path", path)
		}
	}
	return nil
}

// validateServiceNetworksDict 校验服务网络引用形态：短语法列表或「键为
// 网络名、值为空」的映射（aliases/ipam 等每网络配置不在支持清单——服务
// 别名 = compose 服务名，平台管理）。
func validateServiceNetworksDict(name, prefix string, networksAny any) error {
	if networksAny == nil {
		return nil
	}
	switch v := networksAny.(type) {
	case []any:
		return nil // 短语法
	case map[string]any:
		for _, net := range sortedKeys(v) {
			if conf := v[net]; conf != nil {
				if m, ok := conf.(map[string]any); !ok || len(m) > 0 {
					return errCompose("network %q of service %q carries configuration (aliases/ipam etc. are not in the support list; service aliases are managed by the platform from compose service names)", name, net).
						WithContext("path", prefix+".networks."+net)
				}
			}
		}
		return nil
	default:
		return errCompose("unsupported networks form for service %q", name).WithContext("path", prefix+".networks")
	}
}

// validateNetworksDict 校验顶层网络定义（外部网络/name 覆写拒绝）。
func validateNetworksDict(dict map[string]any) error {
	netsAny, ok := dict["networks"]
	if !ok || netsAny == nil {
		return nil
	}
	nets, ok := netsAny.(map[string]any)
	if !ok {
		return errCompose("top-level networks must be a mapping").WithContext("path", "networks")
	}
	for _, net := range sortedKeys(nets) {
		def, ok := nets[net].(map[string]any)
		if !ok {
			continue // 空定义合法（栈内默认网络）
		}
		for _, key := range sortedKeys(def) {
			if reason, rejected := networkDefReject[key]; rejected {
				return errCompose("network %q, field %q: %s", net, key, reason).
					WithContext("path", "networks."+net+"."+key)
			}
		}
	}
	return nil
}

// validateVolumesDict 校验顶层卷定义。
func validateVolumesDict(dict map[string]any) error {
	volsAny, ok := dict["volumes"]
	if !ok || volsAny == nil {
		return nil
	}
	vols, ok := volsAny.(map[string]any)
	if !ok {
		return errCompose("top-level volumes must be a mapping").WithContext("path", "volumes")
	}
	for _, vol := range sortedKeys(vols) {
		def, ok := vols[vol].(map[string]any)
		if !ok {
			continue // 空定义合法（§2.4 示例 volumes: data:）
		}
		for _, key := range sortedKeys(def) {
			if reason, rejected := volumeDefReject[key]; rejected {
				return errCompose("volume %q, field %q: %s", vol, key, reason).
					WithContext("path", "volumes."+vol+"."+key)
			}
		}
	}
	return nil
}

// checkSubKeys 是子段白名单的统一入口（带服务名）。
func checkSubKeys(name, path, section string, dict map[string]any, whitelist map[string]bool) error {
	return checkSubKeysAt(name, path, section, dict, whitelist)
}

// checkSubKeysAt 对映射子段逐键白名单校验。
func checkSubKeysAt(name, path, section string, dict map[string]any, whitelist map[string]bool) error {
	for _, key := range sortedKeys(dict) {
		if strings.HasPrefix(key, "x-") {
			continue
		}
		if !whitelist[key] {
			return errCompose("%s.%s of service %q is not in the controlled-subset support list", name, section, key).
				WithContext("path", path+"."+key)
		}
	}
	return nil
}
