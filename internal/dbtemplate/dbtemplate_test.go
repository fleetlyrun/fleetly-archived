package dbtemplate

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/engine"
)

// 模板注册表 + 渲染器的验收测试（managed-databases 设计 §2.2/§2.4/§2.5，
// E4 S1 验收 6）：渲染确定性 golden（同输入同投影同 desired-hash）、digest
// 钉定逐字、凭据明文不出错误信息、连接串键集与 §2.5 表一致。

const (
	testInstance = "pg-prod"
	testTeam     = "acme"
	testPrj      = "prod"
	testPassword = "abc123XYZdef456GHIjkl789MNO012" // 32 位词表内测试值（非真实凭据）
	testID       = "01JABCDEFDB000"
)

func renderInput(templateID string) RenderInput {
	return RenderInput{
		Instance:    testInstance,
		InstanceID:  testID,
		TeamSlug:    testTeam,
		PrjSlug:     testPrj,
		TemplateID:  templateID,
		Credentials: Credentials{Password: testPassword},
	}
}

// TestPinnedDigests 镜像钉定逐字（多架构 OCI index digest——平台 release
// 锁定，门禁同批；任何一侧改动都是平台 release 级事件）。
func TestPinnedDigests(t *testing.T) {
	pg, err := Get(TemplatePostgres16)
	if err != nil {
		t.Fatalf("get postgres-16: %v", err)
	}
	if pg.Image != "postgres:16@sha256:a3b7f434b2dc57ce85a67e171163eb8ab1a1ebcb39d27484661f26b1dfbe30d6" {
		t.Fatalf("postgres image = %s, want the pinned 16.15-trixie multi-arch digest", pg.Image)
	}
	if pg.EnginePort != 5432 || pg.ServiceName != "postgres" || pg.VolumeKey != "data" || pg.VolumeMountPath != "/var/lib/postgresql/data" {
		t.Fatalf("postgres template fields drifted: %+v", pg)
	}
	if pg.CredentialDelivery != CredentialSecretFile {
		t.Fatalf("postgres credential delivery = %s, want secret-file (D-DB-10)", pg.CredentialDelivery)
	}
	if pg.DefaultLimits != (Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}) {
		t.Fatalf("postgres default limits = %+v, want 1.0 CPU / 1GiB (D-DB-9)", pg.DefaultLimits)
	}

	rd, err := Get(TemplateRedis7)
	if err != nil {
		t.Fatalf("get redis-7: %v", err)
	}
	if rd.Image != "redis:7@sha256:c6eabf748fc7a61dbb5a705c78bcf3d6377b1127a97d0ce965c11c44ba46896f" {
		t.Fatalf("redis image = %s, want the pinned multi-arch digest", rd.Image)
	}
	if rd.EnginePort != 6379 || rd.ServiceName != "redis" || rd.VolumeKey != "data" || rd.VolumeMountPath != "/data" {
		t.Fatalf("redis template fields drifted: %+v", rd)
	}
	if rd.CredentialDelivery != CredentialSpecArg {
		t.Fatalf("redis credential delivery = %s, want spec-arg (D-DB-10)", rd.CredentialDelivery)
	}
	if rd.DefaultLimits != (Limits{CPUSeconds: 0.5, MemoryBytes: 256 << 20}) {
		t.Fatalf("redis default limits = %+v, want 0.5 CPU / 256MiB (D-DB-9)", rd.DefaultLimits)
	}

	// 注册表 = 四模板（v0.3 W4 扩 mysql-8.4/mongodb-8.0）；未知 ID →
	// ErrUnknownTemplate（S2 映射 E_DB_TEMPLATE_UNSUPPORTED）。
	list := List()
	if len(list) != 4 {
		t.Fatalf("registry list = %+v, want 4 templates", list)
	}
	for i, want := range []string{"mongodb-8.0", "mysql-8.4", "postgres-16", "redis-7"} {
		if list[i].ID != want {
			t.Fatalf("registry list[%d] = %s, want %s (sorted)", i, list[i].ID, want)
		}
	}
	if _, err := Get("mysql-8"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown template err = %v, want ErrUnknownTemplate", err)
	}
	if _, err := Get("mongo-8.0"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown template err = %v, want ErrUnknownTemplate (the mongodb key is mongodb-8.0)", err)
	}
}

// TestMySQLAndMongoTemplates 新模板注册表字段逐字（v0.3 W4 D-W4-1/2，设计
// managed-databases §8.2 表）：digest 双锚、端口、卷挂载点、secret 文件投
// 递、健康门（裁决命令逐字）、缺省限额。
func TestMySQLAndMongoTemplates(t *testing.T) {
	my, err := Get(TemplateMySQL84)
	if err != nil {
		t.Fatalf("get mysql-8.4: %v", err)
	}
	if my.Image != "mysql:8.4@sha256:0744ee5ef89ce6ccfa13de3e579fe6b9e27f93dd70da9c06d2c908b1b193fb8d" {
		t.Fatalf("mysql image = %s, want the pinned multi-arch digest (D-W4-1)", my.Image)
	}
	if my.EnginePort != 3306 || my.ServiceName != "mysql" || my.VolumeKey != "data" || my.VolumeMountPath != "/var/lib/mysql" {
		t.Fatalf("mysql template fields drifted: %+v", my)
	}
	if my.CredentialDelivery != CredentialSecretFile {
		t.Fatalf("mysql credential delivery = %s, want secret-file (MYSQL_*_FILE native _FILE support)", my.CredentialDelivery)
	}
	if my.DefaultLimits != (Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}) {
		t.Fatalf("mysql default limits = %+v, want 1.0 CPU / 1GiB", my.DefaultLimits)
	}
	if !reflect.DeepEqual(my.HealthGate.Test, []string{"CMD", "mysqladmin", "ping"}) {
		t.Fatalf("mysql health gate = %v, want the ruled mysqladmin ping", my.HealthGate.Test)
	}

	mo, err := Get(TemplateMongoDB80)
	if err != nil {
		t.Fatalf("get mongodb-8.0: %v", err)
	}
	if mo.Image != "mongo:8.0@sha256:4968f22d0c6c10ef29952f3e807f62872ba22b3312f25803564fbfc08255efc2" {
		t.Fatalf("mongo image = %s, want the pinned multi-arch digest (D-W4-2)", mo.Image)
	}
	if mo.EnginePort != 27017 || mo.ServiceName != "mongo" || mo.VolumeKey != "data" || mo.VolumeMountPath != "/data/db" {
		t.Fatalf("mongo template fields drifted: %+v", mo)
	}
	if mo.CredentialDelivery != CredentialSecretFile {
		t.Fatalf("mongo credential delivery = %s, want secret-file (MONGO_INITDB_ROOT_PASSWORD_FILE native _FILE support)", mo.CredentialDelivery)
	}
	if mo.DefaultLimits != (Limits{CPUSeconds: 1.0, MemoryBytes: 1 << 30}) {
		t.Fatalf("mongo default limits = %+v, want 1.0 CPU / 1GiB", mo.DefaultLimits)
	}
	if !reflect.DeepEqual(mo.HealthGate.Test, []string{"CMD", "mongosh", "--quiet", "--eval", "db.adminCommand('ping')"}) {
		t.Fatalf("mongo health gate = %v, want the ruled mongosh ping eval", mo.HealthGate.Test)
	}
	for _, tpl := range []Template{my, mo} {
		if tpl.HealthGate.Interval != 5*time.Second || tpl.HealthGate.Timeout != 3*time.Second ||
			tpl.HealthGate.Retries != 3 || tpl.HealthGate.StartPeriod != 30*time.Second {
			t.Fatalf("%s health gate timing = %+v, want the default 5s/3s/3/30s", tpl.ID, tpl.HealthGate)
		}
	}
}

// TestTemplateIdentityFields 身份字段（IMPL-DB-0 冻结的最小设计）：注册表
// 全条目 Engine/Distribution/Major 齐备（新增条目漏填即红）；既有四条目的
// 身份值逐字钉定（行为零变化的锚）。
func TestTemplateIdentityFields(t *testing.T) {
	want := map[string]struct {
		engine       Engine
		distribution Distribution
		major        int
	}{
		TemplatePostgres16: {EnginePostgres, DistributionVanilla, 16},
		TemplateRedis7:     {EngineRedis, DistributionVanilla, 7},
		TemplateMySQL84:    {EngineMySQL, DistributionVanilla, 8},
		TemplateMongoDB80:  {EngineMongo, DistributionVanilla, 8},
	}
	list := List()
	if len(list) != len(want) {
		t.Fatalf("registry list = %d entries, want %d", len(list), len(want))
	}
	for _, tpl := range list {
		w, ok := want[tpl.ID]
		if !ok {
			t.Fatalf("unexpected template %q in the registry", tpl.ID)
		}
		if tpl.Engine != w.engine || tpl.Distribution != w.distribution || tpl.Major != w.major {
			t.Fatalf("%s identity = (%s, %s, %d), want (%s, %s, %d)",
				tpl.ID, tpl.Engine, tpl.Distribution, tpl.Major, w.engine, w.distribution, w.major)
		}
	}
}

// TestRenderEnvPGDataFollowsTemplateMountPath（IMPL-DB-0 参数化）：PGDATA 由
// 模板 VolumeMountPath 派生（挂载点 + /pgdata 子目录约定）——PG16 现值逐字
// 不变；发行版按条目携带的原生挂载点自然成立（percona /data/db）。
func TestRenderEnvPGDataFollowsTemplateMountPath(t *testing.T) {
	in := renderInput(TemplatePostgres16)
	pg16, err := Get(TemplatePostgres16)
	if err != nil {
		t.Fatalf("get postgres-16: %v", err)
	}
	if got, want := pgDataDirectory(pg16), "/var/lib/postgresql/data/pgdata"; got != want {
		t.Fatalf("postgres-16 PGDATA = %q, want %q (现值逐字不变)", got, want)
	}
	if env := renderEnv(pg16, in); !containsString(env, "PGDATA=/var/lib/postgresql/data/pgdata") {
		t.Fatalf("postgres-16 env = %v, want the unchanged PGDATA value", env)
	}

	// 合成 percona 条目（DB-1 词表就绪前的参数化钉）：原生挂载点 /data/db。
	percona := Template{
		ID:                 "percona-postgresql-18",
		Engine:             EnginePostgres,
		Distribution:       DistributionPercona,
		Major:              18,
		ServiceName:        "postgres",
		EnginePort:         5432,
		VolumeKey:          "data",
		VolumeMountPath:    "/data/db",
		CredentialDelivery: CredentialSecretFile,
	}
	if got, want := pgDataDirectory(percona), "/data/db/pgdata"; got != want {
		t.Fatalf("percona PGDATA = %q, want %q", got, want)
	}
	if env := renderEnv(percona, in); !containsString(env, "PGDATA=/data/db/pgdata") {
		t.Fatalf("percona env = %v, want PGDATA=/data/db/pgdata", env)
	}

	// 连接串投影按引擎族分派（PG 家族 URL 形态对新条目零新增代码）。
	vars, err := ConnectionVars(TemplatePostgres16, testInstance, testPassword)
	if err != nil {
		t.Fatalf("ConnectionVars: %v", err)
	}
	if got, want := vars[EnvPrefix(testInstance)+"_URL"], "postgres://fleetly:"+testPassword+"@"+testInstance+":5432/pg_prod"; got != want {
		t.Fatalf("postgres URL = %q, want %q", got, want)
	}
}

// containsString 是测试内的字面包含判定（env 集为 []string）。
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestRenderMySQLAndMongo 新模板渲染 golden：secret 文件投递（官方入口
// _FILE 变体 env）+ 挂载点 + 健康门 + 确定性（同输入同投影同 desired-hash）。
func TestRenderMySQLAndMongo(t *testing.T) {
	for _, tc := range []struct {
		templateID string
		wantEnv    []string
	}{
		{
			TemplateMySQL84,
			[]string{
				"MYSQL_DATABASE=pg_prod",
				"MYSQL_PASSWORD_FILE=/run/secrets/password",
				"MYSQL_ROOT_PASSWORD_FILE=/run/secrets/password",
				"MYSQL_USER=fleetly",
			},
		},
		{
			TemplateMongoDB80,
			[]string{
				"MONGO_INITDB_DATABASE=pg_prod",
				"MONGO_INITDB_ROOT_PASSWORD_FILE=/run/secrets/password",
				"MONGO_INITDB_ROOT_USERNAME=fleetly",
			},
		},
	} {
		in := renderInput(tc.templateID)
		spec, err := Render(in)
		if err != nil {
			t.Fatalf("render %s: %v", tc.templateID, err)
		}
		again, err := Render(in)
		if err != nil {
			t.Fatalf("render %s again: %v", tc.templateID, err)
		}
		if !reflect.DeepEqual(spec, again) {
			t.Fatalf("%s render is not deterministic:\n %+v\n %+v", tc.templateID, spec, again)
		}
		if spec.DesiredHash() != again.DesiredHash() || spec.DesiredHash() == "" {
			t.Fatalf("%s desired hash unstable/empty", tc.templateID)
		}
		// env：按键字典序 + 官方入口 _FILE 变体消费同一 secret 文件 + 命令
		// 词表/投影零明文。
		if !reflect.DeepEqual(spec.Env, tc.wantEnv) {
			t.Fatalf("%s env = %v, want %v", tc.templateID, spec.Env, tc.wantEnv)
		}
		for _, kv := range spec.Env {
			if strings.Contains(kv, testPassword) {
				t.Fatalf("%s env carries credential plaintext: %s", tc.templateID, kv)
			}
		}
		// secret 文件投递：密码不进 Command/启动参数（与 Redis spec-arg 形
		// 态相反——新引擎官方镜像均原生支持 _FILE，零成本硬化走 secret 管
		// 道，D-DB-10 优先序）。
		if spec.Command != nil {
			t.Fatalf("%s must not carry a command override, got %v", tc.templateID, spec.Command)
		}
		if len(spec.Secrets) != 1 || spec.Secrets[0].Target != "/run/secrets/password" {
			t.Fatalf("%s secrets = %+v, want the password secret at /run/secrets/password", tc.templateID, spec.Secrets)
		}
		// 卷挂载点与平台受管面。
		wantMount := "/var/lib/mysql"
		if tc.templateID == TemplateMongoDB80 {
			wantMount = "/data/db"
		}
		if len(spec.Mounts) != 1 || spec.Mounts[0].Target != wantMount {
			t.Fatalf("%s mounts = %+v, want the data volume at %s", tc.templateID, spec.Mounts, wantMount)
		}
		if spec.Replicas != 1 || spec.UpdateOrder != "stop-first" {
			t.Fatalf("%s replicas/order = %d/%s, want 1/stop-first", tc.templateID, spec.Replicas, spec.UpdateOrder)
		}
		if spec.Resources == nil || spec.Resources.NanoCPUs != 1_000_000_000 || spec.Resources.MemoryBytes != 1<<30 {
			t.Fatalf("%s resources = %+v, want 1.0 CPU / 1GiB defaults", tc.templateID, spec.Resources)
		}
		if spec.Healthcheck == nil || spec.Healthcheck.Interval != 5*time.Second || spec.Healthcheck.StartPeriod != 30*time.Second {
			t.Fatalf("%s healthcheck = %+v, want the template health gate", tc.templateID, spec.Healthcheck)
		}
	}
}

// TestConnectionVarsMySQLAndMongo 新模板连接串/物化键集（设计 managed-
// databases §8.1 投影表）：全键同构 PG；mongo 的 URL 带 ?authSource=admin
// （官方入口 root 恒建于 admin 库的实现注记）；密码零出现在键名面。
func TestConnectionVarsMySQLAndMongo(t *testing.T) {
	myVars, err := ConnectionVars(TemplateMySQL84, testInstance, testPassword)
	if err != nil {
		t.Fatalf("mysql connection vars: %v", err)
	}
	wantMySQL := map[string]string{
		"FLEETLY_DB_PG_PROD_URL":      "mysql://fleetly:" + testPassword + "@pg-prod:3306/pg_prod",
		"FLEETLY_DB_PG_PROD_HOST":     "pg-prod",
		"FLEETLY_DB_PG_PROD_PORT":     "3306",
		"FLEETLY_DB_PG_PROD_USER":     "fleetly",
		"FLEETLY_DB_PG_PROD_PASSWORD": testPassword,
		"FLEETLY_DB_PG_PROD_DATABASE": "pg_prod",
	}
	if !reflect.DeepEqual(myVars, wantMySQL) {
		t.Fatalf("mysql vars = %v, want %v", myVars, wantMySQL)
	}

	moVars, err := ConnectionVars(TemplateMongoDB80, testInstance, testPassword)
	if err != nil {
		t.Fatalf("mongo connection vars: %v", err)
	}
	wantMongo := map[string]string{
		"FLEETLY_DB_PG_PROD_URL":      "mongodb://fleetly:" + testPassword + "@pg-prod:27017/pg_prod?authSource=admin",
		"FLEETLY_DB_PG_PROD_HOST":     "pg-prod",
		"FLEETLY_DB_PG_PROD_PORT":     "27017",
		"FLEETLY_DB_PG_PROD_USER":     "fleetly",
		"FLEETLY_DB_PG_PROD_PASSWORD": testPassword,
		"FLEETLY_DB_PG_PROD_DATABASE": "pg_prod",
	}
	if !reflect.DeepEqual(moVars, wantMongo) {
		t.Fatalf("mongo vars = %v, want %v", moVars, wantMongo)
	}
}

// TestRenderPostgresDeterministicGolden PG 渲染：确定性（同输入两次渲染
// 深度相等 + desired-hash 相等）+ 全字段逐项断言（§2.2/§2.4 表）。
func TestRenderPostgresDeterministicGolden(t *testing.T) {
	in := renderInput(TemplatePostgres16)
	spec, err := Render(in)
	if err != nil {
		t.Fatalf("render postgres: %v", err)
	}

	// 确定性：同输入同投影（stable field order——env 已排序、集合字段
	// 零随机），desired-hash 稳定。
	again, err := Render(in)
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if !reflect.DeepEqual(spec, again) {
		t.Fatalf("render is not deterministic:\n %+v\n %+v", spec, again)
	}
	if spec.DesiredHash() != again.DesiredHash() || spec.DesiredHash() == "" {
		t.Fatalf("desired hash unstable/empty: %s vs %s", spec.DesiredHash(), again.DesiredHash())
	}

	// 名族（fleetly-db- 前缀族 + 别名 = 实例名）。
	if spec.Name != "fleetly-db-acme-prod-pg-prod-postgres" {
		t.Fatalf("service name = %s, want fleetly-db-acme-prod-pg-prod-postgres", spec.Name)
	}
	if len(spec.Networks) != 1 || spec.Networks[0].Name != "fleetly-db-acme-prod-pg-prod-net" ||
		!reflect.DeepEqual(spec.Networks[0].Aliases, []string{"pg-prod"}) {
		t.Fatalf("networks = %+v, want the instance network with alias = instance name (no generic postgres alias)", spec.Networks)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].VolumeName != "fleetly-db-pg-prod-data-01JABCDE" ||
		spec.Mounts[0].Target != "/var/lib/postgresql/data" {
		t.Fatalf("mounts = %+v, want data volume at /var/lib/postgresql/data", spec.Mounts)
	}

	// env：按键字典序 + PG 凭据规格（固定 USER、DATABASE '-'→'_'、密码走
	// secret 文件）+ PGDATA 子目录约定。
	wantEnv := []string{
		"PGDATA=/var/lib/postgresql/data/pgdata",
		"POSTGRES_DB=pg_prod",
		"POSTGRES_PASSWORD_FILE=/run/secrets/password",
		"POSTGRES_USER=fleetly",
	}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Fatalf("env = %v, want %v (sorted, PG credential spec)", spec.Env, wantEnv)
	}
	// secret 管道投递：fleetly-db-<name>-password-<hash8> → /run/secrets/password。
	if len(spec.Secrets) != 1 || spec.Secrets[0].Target != "/run/secrets/password" {
		t.Fatalf("secrets = %+v, want the password secret at /run/secrets/password", spec.Secrets)
	}
	if !strings.HasPrefix(spec.Secrets[0].SecretName, "fleetly-db-acme-prod-pg-prod-password-") {
		t.Fatalf("secret name = %s, want the fleetly-db- family", spec.Secrets[0].SecretName)
	}

	// 健康门（设计 §2.2：pg_isready -U fleetly，5s/3s/3/30s）。
	if spec.Healthcheck == nil {
		t.Fatal("healthcheck missing")
	}
	wantHC := engine.HealthcheckSpec{
		Test:        []string{"CMD", "pg_isready", "-U", "fleetly"},
		Interval:    5 * time.Second,
		Timeout:     3 * time.Second,
		Retries:     3,
		StartPeriod: 30 * time.Second,
	}
	if !reflect.DeepEqual(*spec.Healthcheck, wantHC) {
		t.Fatalf("healthcheck = %+v, want %+v", *spec.Healthcheck, wantHC)
	}

	// 平台受管面：副本 1、有卷强制 stop-first、缺省限额落 Resources。
	if spec.Replicas != 1 || spec.UpdateOrder != "stop-first" {
		t.Fatalf("replicas=%d order=%s, want 1/stop-first", spec.Replicas, spec.UpdateOrder)
	}
	if spec.Resources == nil || spec.Resources.NanoCPUs != 1_000_000_000 || spec.Resources.MemoryBytes != 1<<30 {
		t.Fatalf("resources = %+v, want 1.0 CPU / 1GiB defaults", spec.Resources)
	}
	if spec.Image == "" || !strings.Contains(spec.Image, "@sha256:") {
		t.Fatalf("image not digest-pinned: %s", spec.Image)
	}

	// 限额覆盖（设置面唯一可改项）：零值回落缺省、显式值生效。
	overridden, err := Render(RenderInput{
		Instance: testInstance, InstanceID: testID, TeamSlug: testTeam, PrjSlug: testPrj, TemplateID: TemplatePostgres16,
		Limits:      Limits{CPUSeconds: 0.25, MemoryBytes: 256 << 20},
		Credentials: Credentials{Password: testPassword},
	})
	if err != nil {
		t.Fatalf("render overridden: %v", err)
	}
	if overridden.Resources.NanoCPUs != 250_000_000 || overridden.Resources.MemoryBytes != 256<<20 {
		t.Fatalf("overridden resources = %+v, want 0.25 CPU / 256MiB", overridden.Resources)
	}
	partial, err := Render(RenderInput{
		Instance: testInstance, InstanceID: testID, TeamSlug: testTeam, PrjSlug: testPrj, TemplateID: TemplatePostgres16,
		Limits:      Limits{MemoryBytes: 2 << 30},
		Credentials: Credentials{Password: testPassword},
	})
	if err != nil {
		t.Fatalf("render partial: %v", err)
	}
	if partial.Resources.NanoCPUs != 1_000_000_000 || partial.Resources.MemoryBytes != 2<<30 {
		t.Fatalf("partial override resources = %+v, want CPU default + memory override", partial.Resources)
	}
}

// TestRenderRedis redis 渲染：--requirepass 参数形态（D-DB-10 spec 明文
// 边界）+ 健康门引用 env 变量（密码不进 Test 字面量——假健康防护见
// redisHealthGate 注记）+ 无 secret 挂载。
func TestRenderRedis(t *testing.T) {
	spec, err := Render(renderInput(TemplateRedis7))
	if err != nil {
		t.Fatalf("render redis: %v", err)
	}
	if spec.Name != "fleetly-db-acme-prod-pg-prod-redis" {
		t.Fatalf("service name = %s, want fleetly-db-acme-prod-pg-prod-redis", spec.Name)
	}
	// 启动参数形态：redis-server --requirepass <pw>（官方镜像 entrypoint
	// 透传）。
	wantCmd := []string{"redis-server", "--requirepass", testPassword}
	if !reflect.DeepEqual(spec.Command, wantCmd) {
		t.Fatalf("command = %v, want %v", spec.Command, wantCmd)
	}
	// 密码 env（健康门引用源；与 spec 参数同暴露类——D-DB-10 已知边界）。
	if !reflect.DeepEqual(spec.Env, []string{redisPasswordEnv + "=" + testPassword}) {
		t.Fatalf("env = %v, want exactly the healthcheck password env", spec.Env)
	}
	// 健康门：认证化 ping（CMD-SHELL 引用 env）——设计表 `redis-cli ping`
	// 的语义意图，无认证即假健康（NOAUTH 退出码 0）。
	if spec.Healthcheck == nil || len(spec.Healthcheck.Test) != 2 || spec.Healthcheck.Test[0] != "CMD-SHELL" {
		t.Fatalf("redis healthcheck = %+v, want CMD-SHELL form", spec.Healthcheck)
	}
	shell := spec.Healthcheck.Test[1]
	if !strings.Contains(shell, `"${"`) && !strings.Contains(shell, "$"+redisPasswordEnv) {
		t.Fatalf("healthcheck must reference $%s (env var), got %q", redisPasswordEnv, shell)
	}
	if strings.Contains(shell, testPassword) {
		t.Fatal("healthcheck test must not embed the literal password (env reference only)")
	}
	if !strings.Contains(shell, "ping") || !strings.Contains(shell, "PONG") {
		t.Fatalf("healthcheck %q must assert a PONG reply", shell)
	}
	if spec.Healthcheck.Interval != 5*time.Second || spec.Healthcheck.Timeout != 3*time.Second ||
		spec.Healthcheck.Retries != 3 || spec.Healthcheck.StartPeriod != 30*time.Second {
		t.Fatalf("redis healthcheck timing = %+v, want the default 5s/3s/3/30s", spec.Healthcheck)
	}
	// Redis 凭据不走 secret 文件（spec 参数形态）。
	if len(spec.Secrets) != 0 {
		t.Fatalf("redis spec has secret mounts: %+v (credential delivery is spec-arg)", spec.Secrets)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Target != "/data" {
		t.Fatalf("mounts = %+v, want data volume at /data", spec.Mounts)
	}
}

// TestConnectionVars 连接串/物化键集（§2.5 表）：PG 全键、Redis 子集、
// 前缀 '-'→'_' 大写、URL 形态逐字。
func TestConnectionVars(t *testing.T) {
	vars, err := ConnectionVars(TemplatePostgres16, testInstance, testPassword)
	if err != nil {
		t.Fatalf("pg connection vars: %v", err)
	}
	wantPG := map[string]string{
		"FLEETLY_DB_PG_PROD_URL":      "postgres://fleetly:" + testPassword + "@pg-prod:5432/pg_prod",
		"FLEETLY_DB_PG_PROD_HOST":     "pg-prod",
		"FLEETLY_DB_PG_PROD_PORT":     "5432",
		"FLEETLY_DB_PG_PROD_USER":     "fleetly",
		"FLEETLY_DB_PG_PROD_PASSWORD": testPassword,
		"FLEETLY_DB_PG_PROD_DATABASE": "pg_prod",
	}
	if !reflect.DeepEqual(vars, wantPG) {
		t.Fatalf("pg vars = %v, want %v", vars, wantPG)
	}

	redisVars, err := ConnectionVars(TemplateRedis7, testInstance, testPassword)
	if err != nil {
		t.Fatalf("redis connection vars: %v", err)
	}
	wantRedis := map[string]string{
		"FLEETLY_DB_PG_PROD_URL":      "redis://:" + testPassword + "@pg-prod:6379/0",
		"FLEETLY_DB_PG_PROD_HOST":     "pg-prod",
		"FLEETLY_DB_PG_PROD_PORT":     "6379",
		"FLEETLY_DB_PG_PROD_PASSWORD": testPassword,
	}
	if !reflect.DeepEqual(redisVars, wantRedis) {
		t.Fatalf("redis vars = %v, want %v (subset without USER/DATABASE)", redisVars, wantRedis)
	}

	// 前缀定义点：EnvPrefix/'-'→'_'/大写。
	if EnvPrefix("my-Db_01") != "FLEETLY_DB_MY_DB_01" {
		t.Fatalf("EnvPrefix = %s, want FLEETLY_DB_MY_DB_01", EnvPrefix("my-Db_01"))
	}
	if DatabaseName("my-db") != "my_db" {
		t.Fatalf("DatabaseName = %s, want my_db", DatabaseName("my-db"))
	}

	// 错误路径不泄密。
	for _, tc := range []struct {
		templateID, instance, password string
	}{
		{"mysql-8", testInstance, testPassword},
		{TemplatePostgres16, "", testPassword},
		{TemplatePostgres16, testInstance, ""},
	} {
		if _, err := ConnectionVars(tc.templateID, tc.instance, tc.password); err == nil {
			t.Errorf("connection vars for %s/%q accepted", tc.templateID, tc.instance)
		} else if strings.Contains(err.Error(), testPassword) {
			t.Errorf("error message leaks credential material: %s", err)
		}
	}
}

// TestGeneratePassword 凭据生成（§2.2：32 位 [a-zA-Z0-9]；唯一性抽样）。
func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(pw) != 32 {
			t.Fatalf("password length = %d, want 32", len(pw))
		}
		for _, r := range pw {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !isAlnum {
				t.Fatalf("password %q contains character %q outside [a-zA-Z0-9]", pw, r)
			}
		}
		if seen[pw] {
			t.Fatalf("password repeated within %d draws (entropy broken)", 200)
		}
		seen[pw] = true
		// URL 安全（[a-zA-Z0-9] 免 percent-encode 的契约面）。
		if strings.ContainsAny(pw, ":/@?&#") {
			t.Fatalf("password %q is not URL-safe", pw)
		}
	}
}

// TestRenderRejectsBadInput 渲染输入校验：未知模板/空实例名/空密码/短
// 实例 ID 拒绝，错误信息不含凭据材料（明文纪律的负面测试）。
func TestRenderRejectsBadInput(t *testing.T) {
	cases := []RenderInput{
		{Instance: testInstance, InstanceID: testID, TemplateID: "mysql-8", Credentials: Credentials{Password: testPassword}},
		{Instance: "", InstanceID: testID, TemplateID: TemplatePostgres16, Credentials: Credentials{Password: testPassword}},
		{Instance: testInstance, InstanceID: testID, TemplateID: TemplatePostgres16},
		{Instance: testInstance, InstanceID: "01JA", TemplateID: TemplatePostgres16, Credentials: Credentials{Password: testPassword}},
	}
	for i, in := range cases {
		_, err := Render(in)
		if err == nil {
			t.Errorf("bad input #%d accepted", i)
		} else if strings.Contains(err.Error(), testPassword) {
			t.Errorf("render error #%d leaks credential material: %s", i, err)
		}
	}
}
