// Package dbtemplate 是库引擎模板的平台内置注册表与渲染器（managed-
// databases 设计 §2.2，E4 D-DB-2）：模板 = 平台内置 Go 注册表条目，随平台
// 版本发布——不是文件、不是数据、不可热更（D13 禁动态插件面）。每个模板
// 定义镜像（digest 钉定，随平台 release 锁定）、引擎内部端口、数据卷、
// 凭据规格、健康门与缺省限额；渲染产物 = 平台受管的 Swarm 服务形态
// （engine.ServiceSpec 投影），由库专属收敛器（provisioner，S3）落地。
//
// 镜像受管（§2.2）：用户不可改库镜像/引擎参数（设置面只有限额与备份计划
// ——违规 → E_DB_TEMPLATE_UNSUPPORTED，S2 映射本包 ErrUnknownTemplate 与
// 受管面拒绝）。目录机制按引擎通用设计（Engine/Distribution/Major 身份
// 字段——PG 首个消费者；IMPL-DB-0/DB-1）：同族发行版与大版本共享适配器/
// 渲染器，仅镜像与默认参数层按条目携带。
//
// **大版本升级不做**（创建时钉死；DT-9/§7）：升 major = dump/restore 到
// 新实例（跨大版本的工具面是硬语义边界，见 internal/database pgToolDir
// 注），不提供原地跨 major 升级；minor 由 digest 钉定、随平台 release 以
// 同卷重建演进（§2.2 升级语义）。
package dbtemplate

import (
	"errors"
	"sort"
	"time"
)

// 平台钉定镜像（多架构 OCI index digest——amd64/arm64 通吃；随平台
// release 锁定，门禁同批。 digest 变更 = 平台 release 携带，既有实例
// 不自动变，升级逐实例 opt-in，§2.2 升级语义）。
const (
	// DefaultPostgresImage 是 postgres:16（16.15-trixie）的钉定引用。
	DefaultPostgresImage = "postgres:16@sha256:a3b7f434b2dc57ce85a67e171163eb8ab1a1ebcb39d27484661f26b1dfbe30d6"
	// DefaultPostgres18Image 是 postgres:18（18.6-trixie）的钉定引用
	// （IMPL-DB-1；台账 docs/runbooks/image-prepull.md #23——2026-09-27
	// 解析，多架构 index digest 经 amd64 拉取 RepoDigest 一致 + buildx
	// imagetools 逐平台 manifest 核对交付 arm64 双验）。
	DefaultPostgres18Image = "postgres:18@sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722"
	// DefaultPerconaPostgresql18Image 是 percona/percona-distribution-postgresql:18
	// （Percona Server for PostgreSQL 18.6.1，含 pgvector 0.8.6）的钉定引用
	// （IMPL-DB-1；台账 #24——双验方法同 #23）。发行版差异按条目携带：
	// 卷挂载点 /data/db（uid 26 + PGDATA 子目录约定），工具面
	// /usr/pgsql-18/bin（内部 dbtools 镜像契约）。
	DefaultPerconaPostgresql18Image = "percona/percona-distribution-postgresql:18@sha256:dae47360e8137cafc1e8d66f9a1be348f1405e3cf51daa383b94e6c277e6b256"
	// DefaultRedisImage 是 redis:7 的钉定引用。
	DefaultRedisImage = "redis:7@sha256:c6eabf748fc7a61dbb5a705c78bcf3d6377b1127a97d0ce965c11c44ba46896f"
	// DefaultMySQLImage 是 mysql:8.4（8.4 LTS）的钉定引用（v0.3 W4
	// D-W4-1；台账 docs/runbooks/image-prepull.md #19——2026-09-24 解析，
	// 多架构 index digest 经 amd64 拉取 RepoDigest 一致 + 按 digest 以
	// arm64 平台独立拉取交付 arm64 镜像双验）。
	DefaultMySQLImage = "mysql:8.4@sha256:0744ee5ef89ce6ccfa13de3e579fe6b9e27f93dd70da9c06d2c908b1b193fb8d"
	// DefaultMongoImage 是 mongo:8.0（8.0 Community）的钉定引用（v0.3 W4
	// D-W4-2；台账 #20——双验方法同上，2026-09-24）。
	DefaultMongoImage = "mongo:8.0@sha256:4968f22d0c6c10ef29952f3e807f62872ba22b3312f25803564fbfc08255efc2"
)

// 模板 ID（注册表键；API/CLI 面的 template 取值词表）。
const (
	// TemplatePostgres16 是 PostgreSQL 16 首发模板。
	TemplatePostgres16 = "postgres-16"
	// TemplatePostgres18 是 PostgreSQL 18 模板（IMPL-DB-1；官方镜像）。
	TemplatePostgres18 = "postgres-18"
	// TemplatePerconaPostgresql18 是 Percona Server for PostgreSQL 18 模板
	// （IMPL-DB-1；含 pgvector，torchwood 现役发行版——线协议同 postgres，
	// 零新增适配器，仅卷路径/工具面按发行版携带）。
	TemplatePerconaPostgresql18 = "percona-postgresql-18"
	// TemplateRedis7 是 Redis 7 首发模板。
	TemplateRedis7 = "redis-7"
	// TemplateMySQL84 是 MySQL 8.4 LTS 模板（v0.3 W4，D-W4-1）。
	TemplateMySQL84 = "mysql-8.4"
	// TemplateMongoDB80 是 MongoDB 8.0 Community 模板（v0.3 W4，D-W4-2）。
	TemplateMongoDB80 = "mongodb-8.0"
)

// Engine 是模板引擎族词表（身份字段，IMPL-DB-0 冻结的最小设计）：适配器/
// 渲染器/工具面的分派轴 = 引擎族而非模板 ID——同族发行版与大版本共享实现
// （postgres-16 与 percona-postgresql-18 同属 postgres 家族）。
type Engine string

const (
	// EnginePostgres 是 PostgreSQL 家族（线协议与工具面同族）。
	EnginePostgres Engine = "postgres"
	// EngineRedis 是 Redis 家族。
	EngineRedis Engine = "redis"
	// EngineMySQL 是 MySQL 家族。
	EngineMySQL Engine = "mysql"
	// EngineMongo 是 MongoDB 家族。
	EngineMongo Engine = "mongo"
)

// Distribution 是模板发行版词表（身份字段）：vanilla = 上游/官方镜像，
// percona = Percona 发行版（同引擎线协议；卷路径/扩展面按发行版携带）。
type Distribution string

const (
	// DistributionVanilla 是上游官方发行版。
	DistributionVanilla Distribution = "vanilla"
	// DistributionPercona 是 Percona 发行版。
	DistributionPercona Distribution = "percona"
)

// ErrUnknownTemplate 表示模板 ID 不在注册表（S2 映射 E_DB_TEMPLATE_
// UNSUPPORTED——400，违规即设置面错误而非资源缺失）。
var ErrUnknownTemplate = errors.New("dbtemplate: unknown template id")

// CredentialDelivery 是凭据投递形态词表（§2.2 凭据投递行，D-DB-10）。
type CredentialDelivery string

const (
	// CredentialSecretFile Swarm secret 文件投递（PG POSTGRES_PASSWORD_FILE
	// 原生支持——零成本硬化形态）。
	CredentialSecretFile CredentialDelivery = "secret-file"
	// CredentialSpecArg Swarm spec 参数明文投递（Redis --requirepass——与
	// env 同暴露类，文档明示的已知边界，D-DB-10）。
	CredentialSpecArg CredentialDelivery = "spec-arg"
)

// Limits 是资源限额（仅 limits——cpus 以核数计；零值 = 模板缺省）。
type Limits struct {
	// CPUSeconds 是 CPU 限额（核数）。
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	// MemoryBytes 是内存限额（字节）。
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
}

// HealthGate 是模板健康门（引擎探针命令 + 节奏——健康门参数是模板契约，
// 用户不可改；与 compose healthcheck 无关，库无用户 compose）。
type HealthGate struct {
	// Test 是健康检查命令（CMD / CMD-SHELL 形态，语义同 Swarm）。
	Test        []string      `json:"test"`
	Interval    time.Duration `json:"interval"`
	Timeout     time.Duration `json:"timeout"`
	Retries     uint64        `json:"retries"`
	StartPeriod time.Duration `json:"start_period"`
}

// Template 是一个库引擎模板（注册表条目；全字段平台受管）。
type Template struct {
	// ID 是注册表键（"postgres-16"）。
	ID string
	// Engine 是引擎族（分派轴：适配器/渲染器/工具面按此选择；IMPL-DB-0
	// 起替代「按 ID switch」——同族发行版/大版本共享实现）。
	Engine Engine
	// Distribution 是发行版（vanilla/percona；同引擎族内的差异面按条目
	// 携带——卷路径/扩展面；备份/健康门适配器仍单实现）。
	Distribution Distribution
	// Major 是引擎大版本号（int）。**工具面版本纪律：与实例数据目录同
	// major**——PG 的 pg_dump/pg_restore/postgres 全随此选二进制目录
	// （跨 major 硬语义边界见 internal/database pgToolDir 注）。
	Major int
	// Image 是模板镜像钉定引用（`repo:tag@sha256:...`——tag 保留可读性，
	// digest 为准）。
	Image string
	// ServiceName 是模板服务名（库服务 = fleetly-db-<实例名>-<ServiceName>；
	// 模板侧常量，非用户可配）。
	ServiceName string
	// EnginePort 是引擎内部端口（连接串渲染用；库服务不发布 host 端口、
	// 无 fleetly.domains 即不进路由——「数据库默认不暴露公网」由构造满足）。
	EnginePort int
	// VolumeKey/VolumeMountPath 是数据卷声明（key=模板卷声明名，挂载点 =
	// 引擎数据目录）。
	VolumeKey       string
	VolumeMountPath string
	// CredentialDelivery 是凭据投递形态（D-DB-10）。
	CredentialDelivery CredentialDelivery
	// HealthGate 是健康门。
	HealthGate HealthGate
	// DefaultLimits 是缺省限额（创建时可覆盖；D-DB-9：PG 1C/1Gi、
	// Redis 0.5C/256Mi）。
	DefaultLimits Limits
}

// pgHealthGate 是 PG 健康门（设计 §2.2：`pg_isready -U fleetly`，interval
// 5s / timeout 3s / retries 3 / start_period 30s）。
func pgHealthGate() HealthGate {
	return HealthGate{
		Test:        []string{"CMD", "pg_isready", "-U", "fleetly"},
		Interval:    5 * time.Second,
		Timeout:     3 * time.Second,
		Retries:     3,
		StartPeriod: 30 * time.Second,
	}
}

// redisHealthGate 是 Redis 健康门。实现注记（对设计 §2.2 表 `redis-cli
// ping` 的诚实偏离）：requirepass 服务器对未认证 ping 返回 NOAUTH——
// redis-cli 以退出码 0 收场，Swarm 健康检查会把假成功判为健康。设计表给
// 的是语义意图（「redis 可应答」）；实现必须带认证才是该意图的真值判定。
// 口径：密码已在 spec 启动参数明文（D-DB-10 已知边界）、且经 env 以同一
// 暴露类投递（spec env 明文——架构 §2.3 边界族），健康检查命令**引用 env
// 变量**而不把密码写进 Test 字面量——暴露类不新增（同一 spec、同类明文），
// 值轮换时 Test 字面量不变。--no-auth-warning 压掉 stderr 告警噪声。
func redisHealthGate() HealthGate {
	return HealthGate{
		Test:        []string{"CMD-SHELL", `redis-cli --no-auth-warning -a "$FLEETLY_DB_PASSWORD" ping | grep -q PONG`},
		Interval:    5 * time.Second,
		Timeout:     3 * time.Second,
		Retries:     3,
		StartPeriod: 30 * time.Second,
	}
}

// mysqlHealthGate 是 MySQL 健康门（v0.3 W4 D-W4 裁决逐字：`mysqladmin
// ping`，节奏与 PG/Redis 缺省同 5s/3s/3/30s）。实现注记：无认证 ping 的
// 判定语义 = mysqld 应答即存活——mysqladmin 对「应答但拒绝认证」的活服
// 务器返回 0（官方镜像缺省探针同形态），与 §2.2 Redis 行的 NOAUTH 假成
// 功不同类：Redis 是认证后每命令全 NOAUTH（存活≠可服务），MySQL 的
// access-denied 回应本身以退出码证明服务在答——存活即本门的真值。
func mysqlHealthGate() HealthGate {
	return HealthGate{
		Test:        []string{"CMD", "mysqladmin", "ping"},
		Interval:    5 * time.Second,
		Timeout:     3 * time.Second,
		Retries:     3,
		StartPeriod: 30 * time.Second,
	}
}

// mongoHealthGate 是 MongoDB 健康门（v0.3 W4 D-W4 裁决逐字：`mongosh
// --quiet --eval db.adminCommand('ping')`）。实现注记：`ping` 在认证开启
// 时属 MongoDB 免认证命令白名单——连通可答即真值判定；无认证通道依赖
// （凭据走 initdb，健康门不消费）。
func mongoHealthGate() HealthGate {
	return HealthGate{
		Test:        []string{"CMD", "mongosh", "--quiet", "--eval", "db.adminCommand('ping')"},
		Interval:    5 * time.Second,
		Timeout:     3 * time.Second,
		Retries:     3,
		StartPeriod: 30 * time.Second,
	}
}

// registry 是平台内置模板注册表（唯一真源；新引擎接入 = 一个条目 +
// 一个 EngineAdapter + 备份镜像工具，§2.2）。
var registry = map[string]Template{
	TemplatePostgres16: {
		ID:           TemplatePostgres16,
		Engine:       EnginePostgres,
		Distribution: DistributionVanilla,
		Major:        16,
		Image:        DefaultPostgresImage,
		ServiceName:  "postgres",
		EnginePort:   5432,
		// 卷 key=data，挂 /var/lib/postgresql/data；PGDATA 子目录约定
		// （/var/lib/postgresql/data/pgdata）由渲染器以 env 承载——官方
		// 镜像 initdb 在挂载点根目录会撞 lost+found（挂载卷根非空目录
		// initdb 拒绝），子目录是官方文档的规避形态（§2.2 卷行「PGDATA
		// 子目录约定由模板处理 initdb lost+found 问题」的实现落点）。
		VolumeKey:          "data",
		VolumeMountPath:    "/var/lib/postgresql/data",
		CredentialDelivery: CredentialSecretFile,
		HealthGate:         pgHealthGate(),
		DefaultLimits:      Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}, // 1GiB
	},
	// PostgreSQL 18（官方镜像，IMPL-DB-1）：与 postgres-16 同款形态（挂载点/
	// 子目录约定/健康门/端口/缺省限额），仅镜像与 Major 不同；镜像 digest
	// 与 internal/database/dbtools_manual_test.go 的引擎腿同源。
	TemplatePostgres18: {
		ID:                 TemplatePostgres18,
		Engine:             EnginePostgres,
		Distribution:       DistributionVanilla,
		Major:              18,
		Image:              DefaultPostgres18Image,
		ServiceName:        "postgres",
		EnginePort:         5432,
		VolumeKey:          "data",
		VolumeMountPath:    "/var/lib/postgresql/data",
		CredentialDelivery: CredentialSecretFile,
		HealthGate:         pgHealthGate(),
		DefaultLimits:      Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}, // 1GiB
	},
	// Percona Server for PostgreSQL 18（IMPL-DB-1，DT-9：含 pgvector 的
	// torchwood 现役发行版）。发行版差异只有两处、都按条目携带：①原生卷
	// 挂载点 /data/db（uid 26 + 空卷 root-owned——postgres-16 同款挂载路径
	// 实测 mkdir Permission denied，见 §4「IMPL-DB-1 方案可行性审查（2026-
	// 09-27 续）」）；②dbtools 内的工具面目录 /usr/pgsql-18/bin（发行版面
	// ——internal/database pgToolDir 按发行版选择）。其余字段与 vanilla 18
	// 逐字同款（线协议/凭据投递/健康门/限额——「发行版不新增适配器」）。
	TemplatePerconaPostgresql18: {
		ID:                 TemplatePerconaPostgresql18,
		Engine:             EnginePostgres,
		Distribution:       DistributionPercona,
		Major:              18,
		Image:              DefaultPerconaPostgresql18Image,
		ServiceName:        "postgres",
		EnginePort:         5432,
		VolumeKey:          "data",
		VolumeMountPath:    "/data/db",
		CredentialDelivery: CredentialSecretFile,
		HealthGate:         pgHealthGate(),
		DefaultLimits:      Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}, // 1GiB
	},
	TemplateRedis7: {
		ID:                 TemplateRedis7,
		Engine:             EngineRedis,
		Distribution:       DistributionVanilla,
		Major:              7,
		Image:              DefaultRedisImage,
		ServiceName:        "redis",
		EnginePort:         6379,
		VolumeKey:          "data",
		VolumeMountPath:    "/data",
		CredentialDelivery: CredentialSpecArg,
		HealthGate:         redisHealthGate(),
		DefaultLimits:      Limits{CPUSeconds: 0.5, MemoryBytes: 256 << 20}, // 256MiB
	},
	TemplateMySQL84: {
		ID:           TemplateMySQL84,
		Engine:       EngineMySQL,
		Distribution: DistributionVanilla,
		Major:        8,
		Image:        DefaultMySQLImage,
		ServiceName:  "mysql",
		EnginePort:   3306,
		// 卷 key=data 挂 /var/lib/mysql（官方镜像 DATADIR）；凭据经
		// MYSQL_*_FILE secret 文件投递（入口 file_env 原生支持——D-W4-1，
		// 设计 managed-databases §8.2 表）；root 密码 = 同一凭据值（root
		// 不在用户面/投影暴露，仅满足官方镜像 initdb 必填——轮换只动
		// fleetly@'%'）。
		VolumeKey:          "data",
		VolumeMountPath:    "/var/lib/mysql",
		CredentialDelivery: CredentialSecretFile,
		HealthGate:         mysqlHealthGate(),
		DefaultLimits:      Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}, // 1GiB
	},
	TemplateMongoDB80: {
		ID:           TemplateMongoDB80,
		Engine:       EngineMongo,
		Distribution: DistributionVanilla,
		Major:        8,
		Image:        DefaultMongoImage,
		ServiceName:  "mongo",
		EnginePort:   27017,
		// 卷 key=data 挂 /data/db（官方镜像 dbpath）；凭据经
		// MONGO_INITDB_ROOT_PASSWORD_FILE secret 文件投递（D-W4-2，设计
		// managed-databases §8.2 表）；root 用户由官方入口恒建于 admin 库
		// ——连接串投影带 ?authSource=admin（§8.1 实现注记）。
		VolumeKey:          "data",
		VolumeMountPath:    "/data/db",
		CredentialDelivery: CredentialSecretFile,
		HealthGate:         mongoHealthGate(),
		DefaultLimits:      Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}, // 1GiB
	},
}

// Get 按注册表键取模板；未知 ID 返回 ErrUnknownTemplate。
func Get(id string) (Template, error) {
	tpl, ok := registry[id]
	if !ok {
		return Template{}, ErrUnknownTemplate
	}
	return tpl, nil
}

// List 返回全部模板（按 ID 字典序——展示面稳定序）。
func List() []Template {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Template, 0, len(ids))
	for _, id := range ids {
		out = append(out, registry[id])
	}
	return out
}
