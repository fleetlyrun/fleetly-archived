package dbtemplate

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
)

// 渲染器（managed-databases §2.2/§2.4/§2.5）：模板 → 平台受管的 Swarm
// 服务形态（engine.ServiceSpec 投影——复用既有执行投影，desired-hash 免费获得）。
// 确定性纪律：同输入必得同投影（desired-hash 稳定的前提，golden 测试钉死
// ——env 按键字典序、集合字段零随机）。
//
// 明文纪律：凭据明文只存活于 RenderInput → ServiceSpec（env/command/secret
// 引用）内存投影与 age 密文存储；不进日志/事件/错误信息（负面测试钉死）。

// Credentials 是模板凭据（§2.2 凭据规格：生成 32 位 [a-zA-Z0-9] 密码）。
// USER/DATABASE 不在凭据里——PG 由实例名与模板确定性推导（§2.5），不落库。
type Credentials struct {
	// Password 是引擎密码明文（只存活于内存投影）。
	Password string
}

// RenderInput 是一次渲染输入。
type RenderInput struct {
	// Instance 是库实例名（服务/网络/卷/secret 命名成分 + 共享网络 DNS 别名）。
	Instance string
	// InstanceID 是库实例平台 ID（卷命名 id8 尾缀——防代际静默复用）。
	InstanceID string
	// TeamSlug / PrjSlug 是归属两个 slug（v0.3 库族三段命名公式的参数，
	// rbac-teams §4.3 库行；卷公式不变故不参与卷名）。
	TeamSlug string
	PrjSlug  string
	// TemplateID 是注册表键。
	TemplateID string
	// Limits 是用户限额（零值字段回落模板缺省；设置面只有限额与备份计划
	// ——镜像/引擎参数受管）。
	Limits Limits
	// Credentials 是已生成的凭据（GeneratePassword 产物；由调用方持有并
	// 加密落库——渲染器不负责持久化）。
	Credentials Credentials
}

// render 固定常量（凭据规格，PG 首发后为全引擎共用口径：固定
// USER=fleetly；DATABASE = 实例名 '-'→'_'；密码经 Swarm secret 文件投递
// ——secret 文件路径对 mysql/mongo 官方入口的 _FILE 变体同形复用，
// v0.3 W4）。
const (
	pgUser           = "fleetly"
	pgPasswordFile   = "/run/secrets/password"
	pgSecretName     = "password"
	redisPasswordEnv = "FLEETLY_DB_PASSWORD"
)

// pgDataDirectory 返回 PG 的 PGDATA（数据目录 = 挂载点 + pgdata 子目录
// 约定，§2.2 卷行：官方镜像 initdb 在挂载点根目录会撞 lost+found，子目录
// 是官方文档的规避形态）。由模板 VolumeMountPath 派生（IMPL-DB-0 参数化）：
// PG16 现值 /var/lib/postgresql/data/pgdata 逐字不变；发行版按条目携带的
// 原生挂载点自然成立（percona /data/db → /data/db/pgdata）。
func pgDataDirectory(tpl Template) string {
	return tpl.VolumeMountPath + "/pgdata"
}

// Render 渲染库实例的 Swarm 服务形态投影。网络 = 实例专属共享 overlay
// （别名 = 实例名——防两个 PG 实例的通用别名 postgres 在共享网络互撞 DNS，
// §2.4）；有卷 → stop-first（平台强制纪律，停机窗口如实累计）；副本 1。
func Render(in RenderInput) (engine.ServiceSpec, error) {
	tpl, err := Get(in.TemplateID)
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	if in.Instance == "" {
		return engine.ServiceSpec{}, fmt.Errorf("dbtemplate: instance name is empty")
	}
	if in.Credentials.Password == "" {
		return engine.ServiceSpec{}, fmt.Errorf("dbtemplate: credential password is empty")
	}
	if len(in.InstanceID) < 8 {
		return engine.ServiceSpec{}, fmt.Errorf("dbtemplate: instance id %q shorter than 8 chars", in.InstanceID)
	}

	svcName, err := naming.DBServiceName(in.TeamSlug, in.PrjSlug, in.Instance, tpl.ServiceName)
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	netName, err := naming.DBNetworkName(in.TeamSlug, in.PrjSlug, in.Instance)
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	volName, err := naming.DBVolumeName(in.Instance, tpl.VolumeKey, in.InstanceID)
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	// secret 名 hash8 = 值 sha256 前 8：值轮换即换名换引用（app secret
	// 同纪律——引用进 desired-hash，轮换天然触发重部署）。
	secretName, err := naming.DBSecretName(in.TeamSlug, in.PrjSlug, in.Instance, pgSecretName, naming.Hash8(in.Credentials.Password))
	if err != nil {
		return engine.ServiceSpec{}, err
	}

	limits := resolveLimits(tpl, in.Limits)

	spec := engine.ServiceSpec{
		Name:  svcName,
		Image: tpl.Image,
		Env:   sortEnv(renderEnv(tpl, in)),
		Networks: []engine.NetworkAttach{
			{Name: netName, Aliases: []string{in.Instance}},
		},
		Mounts: []engine.MountSpec{
			{VolumeName: volName, Target: tpl.VolumeMountPath},
		},
		Replicas: 1,
		Healthcheck: &engine.HealthcheckSpec{
			Test:        append([]string(nil), tpl.HealthGate.Test...),
			Interval:    tpl.HealthGate.Interval,
			Timeout:     tpl.HealthGate.Timeout,
			Retries:     tpl.HealthGate.Retries,
			StartPeriod: tpl.HealthGate.StartPeriod,
		},
		// 有卷强制 stop-first（组合裁决在规划层——模板恒带数据卷，渲染层
		// 直接固定；停机窗口如实累计）。
		UpdateOrder: "stop-first",
		Resources: &engine.ResourcesSpec{
			NanoCPUs:    int64(limits.CPUSeconds * 1e9),
			MemoryBytes: limits.MemoryBytes,
		},
	}
	switch tpl.CredentialDelivery {
	case CredentialSecretFile:
		spec.Secrets = []engine.SecretMount{{SecretName: secretName, Target: pgPasswordFile}}
	case CredentialSpecArg:
		// Redis 凭据 = 启动参数 --requirepass（Swarm spec 参数明文——与
		// env 同暴露类，D-DB-10 已知边界）。健康门经 FLEETLY_DB_PASSWORD
		// env 引用同一凭据（见 redisHealthGate 注记）；entrypoint 透传形态
		// 与官方镜像 CMD 约定一致（docker-entrypoint.sh 接收 redis-server
		// 参数）。
		spec.Command = []string{"redis-server", "--requirepass", in.Credentials.Password}
	default:
		return engine.ServiceSpec{}, fmt.Errorf("dbtemplate: template %s has unknown credential delivery %q", tpl.ID, tpl.CredentialDelivery)
	}
	return spec, nil
}

// renderEnv 组装引擎注入 env（调用方 sortEnv 排序）。分派轴 = Engine（同族
// 发行版/大版本共享——IMPL-DB-0）：PG：固定 USER、DATABASE（实例名 '-'→'_'）、
// 密码经 secret 文件、PGDATA 子目录约定（挂载点派生）；Redis：健康门用的
// 密码 env 引用源；MySQL/Mongo：官方入口 _FILE 变体消费同一 secret 文件
// （v0.3 W4 D-W4-1/2，设计 managed-databases §8.2 表）。
func renderEnv(tpl Template, in RenderInput) []string {
	var env []string
	switch tpl.Engine {
	case EnginePostgres:
		env = []string{
			"PGDATA=" + pgDataDirectory(tpl),
			"POSTGRES_DB=" + DatabaseName(in.Instance),
			"POSTGRES_PASSWORD_FILE=" + pgPasswordFile,
			"POSTGRES_USER=" + pgUser,
		}
	case EngineRedis:
		env = []string{
			redisPasswordEnv + "=" + in.Credentials.Password,
		}
	case EngineMySQL:
		// 官方镜像凭据规格：USER/DATABASE 由实例名与模板确定性推导；
		// MYSQL_ROOT_PASSWORD 与应用密码同值（root 不在投影/用户面暴露，
		// 仅满足官方镜像 initdb 必填——轮换只动 fleetly@'%'）。
		env = []string{
			"MYSQL_DATABASE=" + DatabaseName(in.Instance),
			"MYSQL_PASSWORD_FILE=" + pgPasswordFile,
			"MYSQL_ROOT_PASSWORD_FILE=" + pgPasswordFile,
			"MYSQL_USER=" + pgUser,
		}
	case EngineMongo:
		// 官方入口把 initdb root 恒建于 admin 库；MONGO_INITDB_DATABASE
		// 只声明 initdb 缺省库（库本体由首写惰性创建——MongoDB 语义）。
		// 连接串投影因此带 ?authSource=admin（§8.1 实现注记）。
		env = []string{
			"MONGO_INITDB_DATABASE=" + DatabaseName(in.Instance),
			"MONGO_INITDB_ROOT_PASSWORD_FILE=" + pgPasswordFile,
			"MONGO_INITDB_ROOT_USERNAME=" + pgUser,
		}
	}
	return env
}

// sortEnv 按 KEY 字典序排序（engine.ServiceSpec.Env 契约：确定性投影）。
func sortEnv(env []string) []string {
	out := append([]string(nil), env...)
	sort.Strings(out)
	return out
}

// resolveLimits 合成限额：用户零值字段回落模板缺省（D-DB-9 缺省限额表）。
func resolveLimits(tpl Template, in Limits) Limits {
	out := tpl.DefaultLimits
	if in.CPUSeconds > 0 {
		out.CPUSeconds = in.CPUSeconds
	}
	if in.MemoryBytes > 0 {
		out.MemoryBytes = in.MemoryBytes
	}
	return out
}

// EnvPrefix 返回引用方物化 env 的前缀 `FLEETLY_DB_<NAME>`（实例名 '-'→'_'
// 大写形，§2.5）。同 app 引用两个前缀撞名的冲突判定在 S3 前哨
// （E_DB_ENV_PREFIX_CONFLICT）——本函数是前缀计算的唯一定义点。
func EnvPrefix(instance string) string {
	return "FLEETLY_DB_" + EnvName(instance)
}

// EnvName 返回实例名的 env 键段（'-'→'_' 大写）。
func EnvName(instance string) string {
	return strings.ToUpper(strings.ReplaceAll(instance, "-", "_"))
}

// DatabaseName 返回 PG 数据库名（实例名 '-'→'_'；§2.2 凭据规格行）。
func DatabaseName(instance string) string {
	return strings.ReplaceAll(instance, "-", "_")
}

// ConnectionVars 渲染引用方物化 env 键值集（§2.5 键集表——planner 物化为
// 引用 app 的 env_vars source=system 行；v0.3 W4 扩 MySQL/Mongo 全键行，
// 设计 managed-databases §8.1 投影表）。PG/MySQL/Mongo：URL/HOST/PORT/
// USER/PASSWORD/DATABASE；Redis 子集：URL/HOST/PORT/PASSWORD。URL 密码
// 字符集 [a-zA-Z0-9] 免 percent-encode（§2.5 连接串格式行）。
func ConnectionVars(templateID, instance, password string) (map[string]string, error) {
	tpl, err := Get(templateID)
	if err != nil {
		return nil, err
	}
	if instance == "" {
		return nil, fmt.Errorf("dbtemplate: instance name is empty")
	}
	if password == "" {
		return nil, fmt.Errorf("dbtemplate: credential password is empty")
	}
	prefix := EnvPrefix(instance)
	out := map[string]string{
		prefix + "_HOST":     instance,
		prefix + "_PORT":     fmt.Sprintf("%d", tpl.EnginePort),
		prefix + "_PASSWORD": password,
	}
	switch tpl.Engine {
	case EnginePostgres:
		dbName := DatabaseName(instance)
		out[prefix+"_URL"] = fmt.Sprintf("postgres://%s:%s@%s:%d/%s", pgUser, password, instance, tpl.EnginePort, dbName)
		out[prefix+"_USER"] = pgUser
		out[prefix+"_DATABASE"] = dbName
	case EngineRedis:
		out[prefix+"_URL"] = fmt.Sprintf("redis://:%s@%s:%d/0", password, instance, tpl.EnginePort)
	case EngineMySQL:
		dbName := DatabaseName(instance)
		out[prefix+"_URL"] = fmt.Sprintf("mysql://%s:%s@%s:%d/%s", pgUser, password, instance, tpl.EnginePort, dbName)
		out[prefix+"_USER"] = pgUser
		out[prefix+"_DATABASE"] = dbName
	case EngineMongo:
		// ?authSource=admin：官方入口把 initdb root 恒建于 admin 库（镜像
		// 入口脚本硬编码，8.0 无 MONGO_INITDB_ROOT_DATABASE），驱动缺省按
		// URI path 库认证——无该参数认证必败（§8.1 实现注记）。
		dbName := DatabaseName(instance)
		out[prefix+"_URL"] = fmt.Sprintf("mongodb://%s:%s@%s:%d/%s?authSource=admin", pgUser, password, instance, tpl.EnginePort, dbName)
		out[prefix+"_USER"] = pgUser
		out[prefix+"_DATABASE"] = dbName
	}
	return out, nil
}

// 密码生成（§2.2 凭据规格：32 位 [a-zA-Z0-9]，crypto/rand）。掩码拒绝
// 采样：62 不整除 256，直接取模会引入可测偏差——mask=0x3F 保留 62/64 个
// 取值（期望拒绝率 ~3.1%），偏差量级远低于密码学噪声。
const (
	passwordLength  = 32
	passwordCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789" // 62
)

// GeneratePassword 生成模板凭据密码。
func GeneratePassword() (string, error) {
	out := make([]byte, 0, passwordLength)
	buf := make([]byte, passwordLength)
	for len(out) < passwordLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("dbtemplate: generate password: %w", err)
		}
		for _, b := range buf {
			if idx := int(b & 0x3F); idx < len(passwordCharset) {
				out = append(out, passwordCharset[idx])
				if len(out) == passwordLength {
					break
				}
			}
		}
	}
	return string(out), nil
}
