package engine

// 库引用面测试（E4 managed-databases §2.4/§2.5，D-DB-4/D-DB-5/D-DB-3/
// D-DB-11）：label 解析 → 引用物化（system env upsert → pending → 本次
// 部署合并 → 成功提升）、库共享网络逐服务附加（无别名）、db_references
// 登记、存在性哨兵（E_DB_NOT_FOUND 附候选）、前缀冲突哨兵
//（E_DB_ENV_PREFIX_CONFLICT）、未就绪计划警告（W_DB_REFERENCE_NOT_READY
// 不阻塞）、label 移除的清理（引用 + 物化行 + 网络）、app 删除级联、
// D-DB-11 回滚重放取 system 当前值、凭据明文零泄露。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/testsupport"
)

// 库实例模板 ID 字面量（测试不得 import dbtemplate——它在 engine 之上，
// 渲染投影 import engine，test 二进制构成环；键集契约按设计 §2.5 独立钉
// 死在 fake 里，与 dbtemplate 自身的 golden 测试互为对照）。
const (
	testTemplatePostgres16 = "postgres-16"
	testTemplateRedis7     = "redis-7"
)

// dbTemplateTestAdapter 是 DatabaseTemplatePort 的测试替身：按设计 §2.5
// 键集表独立实现（生产适配 = internal/dbtemplate 的 EngineTemplatePort
// 薄委托，其键集正确性由 dbtemplate_test.go golden 钉死）。
type dbTemplateTestAdapter struct{}

func (dbTemplateTestAdapter) EnvPrefix(instance string) string {
	return "FLEETLY_DB_" + strings.ToUpper(strings.ReplaceAll(instance, "-", "_"))
}

func (a dbTemplateTestAdapter) ConnectionVars(templateID, instance, password string) (map[string]string, error) {
	if instance == "" || password == "" {
		return nil, fmt.Errorf("dbTemplateTestAdapter: instance and password are required")
	}
	prefix := a.EnvPrefix(instance)
	switch templateID {
	case testTemplatePostgres16:
		dbName := strings.ReplaceAll(instance, "-", "_")
		return map[string]string{
			prefix + "_URL":      "postgres://fleetly:" + password + "@" + instance + ":5432/" + dbName,
			prefix + "_HOST":     instance,
			prefix + "_PORT":     "5432",
			prefix + "_USER":     "fleetly",
			prefix + "_PASSWORD": password,
			prefix + "_DATABASE": dbName,
		}, nil
	case testTemplateRedis7:
		return map[string]string{
			prefix + "_URL":      "redis://:" + password + "@" + instance + ":6379/0",
			prefix + "_HOST":     instance,
			prefix + "_PORT":     "6379",
			prefix + "_PASSWORD": password,
		}, nil
	}
	return nil, fmt.Errorf("dbTemplateTestAdapter: unknown template %q", templateID)
}

// dbRefCompose 是库引用部署 fixture：web 引用 pg1，worker 无引用（网络
// 附加的作用域对照面）。
const dbRefCompose = `name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
    labels:
      fleetly.databases: "pg1"
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
  worker:
    image: alpine:3
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`

// dbRefComposeNoLabel 是同一 app 摘除引用 label 后的形态（清理用例）。
const dbRefComposeNoLabel = `name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
  worker:
    image: alpine:3
    command: ["sleep", "infinity"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
`

// createDBInstanceForTest 建库实例行（受理即 provisioning），want 状态非
// provisioning 时经 EnterDbPhase 推进（转移表内合法边）。实例落在 demo 应
// 用的同一项目（v0.3 W2-S4 E4 跨项目守卫的正例基准——跨项目引用由
// TestDatabaseReferenceCrossProjectRejected 单独钉负例）。
func createDBInstanceForTest(t *testing.T, h *harness, name, template, password string, want state.DatabaseState) state.DatabaseInstance {
	t.Helper()
	ctx := context.Background()
	cipher, err := h.box.Encrypt([]byte(password))
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	app := h.demoApp()
	inst, err := h.store.CreateDatabaseInstance(ctx, state.DatabaseInstance{
		Name:             name,
		Template:         template,
		ImageDigest:      "postgres:16@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CredentialCipher: string(cipher),
		ProjectID:        app.ProjectID,
		TeamID:           app.TeamID,
	})
	if err != nil {
		t.Fatalf("create database instance %s: %v", name, err)
	}
	if want != state.DatabaseProvisioning {
		if err := h.store.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, want); err != nil {
			t.Fatalf("advance %s to %s: %v", name, want, err)
		}
		inst.State = want
	}
	return inst
}

// eventPayloads 返回事件名 → payload 串的清单（脱敏断言与警告断言用）。
func eventPayloads(t *testing.T, h *harness) map[string]string {
	t.Helper()
	rows, err := h.store.EventsSince(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	out := map[string]string{}
	for _, e := range rows {
		out[e.Name] += e.Payload + "\n"
	}
	return out
}

// serviceEnvMap 取假底座服务的 env 键值映射。
func serviceEnvMap(t *testing.T, h *harness, service string) map[string]string {
	t.Helper()
	svc, ok := h.sub.services[service]
	if !ok {
		t.Fatalf("service %s not on fake substrate", service)
	}
	out := map[string]string{}
	for _, kv := range svc.spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// TestDeployWithDatabaseReference 主链路：引用部署 → 物化行合并进 spec env
// （值明文注入）、库共享网络仅附加带 label 服务（无别名）、db_references
// 登记一行、成功后物化行 pending → effective。
func TestDeployWithDatabaseReference(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	const pw = "oldpasswordA1"
	inst := createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, pw, state.DatabaseReady)

	final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose)))
	if final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}

	// 合并结果注入：六键齐、值来自物化行（本次部署消费 pending）。
	env := serviceEnvMap(t, h, h.svc("web"))
	for key, want := range map[string]string{
		"FLEETLY_DB_PG1_URL":      "postgres://fleetly:" + pw + "@pg1:5432/pg1",
		"FLEETLY_DB_PG1_HOST":     "pg1",
		"FLEETLY_DB_PG1_PORT":     "5432",
		"FLEETLY_DB_PG1_USER":     "fleetly",
		"FLEETLY_DB_PG1_PASSWORD": pw,
		"FLEETLY_DB_PG1_DATABASE": "pg1",
	} {
		if env[key] != want {
			t.Errorf("web env %s = %q, want %q", key, env[key], want)
		}
	}
	// 物化行是 app 侧 env_vars 行——经合并链流向 app 全体服务（S16-C4
	// 既有语义，与 `fleetly env set` 同源）；网络附加才是 label 作用域。
	workerEnv := serviceEnvMap(t, h, h.svc("worker"))
	if workerEnv["FLEETLY_DB_PG1_PASSWORD"] != pw {
		t.Errorf("worker env FLEETLY_DB_PG1_PASSWORD = %q, want app-wide materialized value", workerEnv["FLEETLY_DB_PG1_PASSWORD"])
	}

	// 网络牵线：web = app 网络（别名 = 服务名）+ 库共享网络（无别名）；
	// worker 仅 app 网络。
	svc := h.sub.services[h.svc("web")].spec
	if len(svc.Networks) != 2 {
		t.Fatalf("web networks = %+v, want app net + db net", svc.Networks)
	}
	dbNet := svc.Networks[1]
	wantDBNet, nerr := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
	if nerr != nil {
		t.Fatalf("db network name: %v", nerr)
	}
	if dbNet.Name != wantDBNet || len(dbNet.Aliases) != 0 {
		t.Fatalf("db network attach = %+v, want %s without aliases", dbNet, wantDBNet)
	}
	workerNets := h.sub.services[h.svc("worker")].spec.Networks
	if len(workerNets) != 1 || workerNets[0].Name != h.demoNet() {
		t.Fatalf("worker networks = %+v, want app net only", workerNets)
	}
	// 库网络在发布时被 NetworkEnsure（applyDesired 既有幂等确认循环）。
	if !h.sub.networks[wantDBNet] {
		t.Fatal("db network was not ensured at release")
	}

	// 引用登记一行：(db_id, app_id, service, env_prefix)。
	app, err := h.store.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	refs, err := h.store.ListDatabaseReferencesByApp(ctx, app.ID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("references = %+v (%v), want exactly one row", refs, err)
	}
	if refs[0].DatabaseID != inst.ID || refs[0].Service != "web" || refs[0].EnvPrefix != "FLEETLY_DB_PG1" {
		t.Fatalf("reference row = %+v, want db=%s service=web prefix=FLEETLY_DB_PG1", refs[0], inst.ID)
	}

	// 生效语义：部署成功 → 物化行提升 effective。
	for _, key := range []string{"FLEETLY_DB_PG1_URL", "FLEETLY_DB_PG1_PASSWORD"} {
		row, err := h.store.GetAppEnv(ctx, app.ID, key)
		if err != nil {
			t.Fatalf("get env %s: %v", key, err)
		}
		if row.Status != state.EnvStatusEffective || row.Source != "system" {
			t.Fatalf("env %s = %s/%s after success, want effective/system", key, row.Status, row.Source)
		}
	}
}

// TestDeployReferenceValueChangeRepends 值变更回 pending（验收口径 3）：
// 轮换物化行值（模拟 S4 rotate 的台账面）→ 行回 pending、值未生效；再次
// 部署消费新值 → effective + 运行 spec 携带新密码。
func TestDeployReferenceValueChangeRepends(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "passwordV1x", state.DatabaseReady)
	if final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose))); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}

	app, _ := h.store.GetAppByName(ctx, "demo")
	// 轮换（S4 rotate 的台账面）：实例密文列 + 物化行同步更新——物化行的
	// 权威来源是 db_instances.credential_cipher（重部署时重物化以其为准）。
	newPW := "passwordV2y"
	cipher, err := h.box.Encrypt([]byte(newPW))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	inst, err := h.store.GetDatabaseInstanceByName(ctx, "pg1")
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if err := h.store.UpdateDatabaseCredential(ctx, inst.ID, string(cipher)); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	if _, err := h.store.SetAppEnv(ctx, app.ID, "FLEETLY_DB_PG1_PASSWORD", string(cipher), "system", "human"); err != nil {
		t.Fatalf("rotate materialized row: %v", err)
	}
	row, err := h.store.GetAppEnv(ctx, app.ID, "FLEETLY_DB_PG1_PASSWORD")
	if err != nil || row.Status != state.EnvStatusPending {
		t.Fatalf("rotated row = %+v (%v), want pending (value change re-pends)", row, err)
	}
	// 运行 spec 仍是旧值（pending 未消费）。
	if env := serviceEnvMap(t, h, h.svc("web")); env["FLEETLY_DB_PG1_PASSWORD"] != "passwordV1x" {
		t.Fatalf("running spec must keep the consumed value until redeploy, got %q", env["FLEETLY_DB_PG1_PASSWORD"])
	}

	if final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose))); final.Status != state.DeploySucceeded {
		t.Fatalf("v2 = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	if env := serviceEnvMap(t, h, h.svc("web")); env["FLEETLY_DB_PG1_PASSWORD"] != newPW {
		t.Fatalf("redeployed spec password = %q, want %q", env["FLEETLY_DB_PG1_PASSWORD"], newPW)
	}
	row, _ = h.store.GetAppEnv(ctx, app.ID, "FLEETLY_DB_PG1_PASSWORD")
	if row.Status != state.EnvStatusEffective {
		t.Fatalf("row status after redeploy = %s, want effective", row.Status)
	}
}

// TestDeployMissingDatabaseReference 存在性哨兵（D-DB-5）：引用不存在的实
// 例 → 部署失败 E_DB_NOT_FOUND（404 族），失败披露携带候选清单。
func TestDeployMissingDatabaseReference(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "pw", state.DatabaseReady)

	yaml := strings.Replace(dbRefCompose, `"pg1"`, `"ghost"`, 1)
	final := h.runToTerminal(h.enqueue(h.writeCompose(yaml)))
	if final.Status != state.DeployFailed || final.ErrorCode != "E_DB_NOT_FOUND" {
		t.Fatalf("deploy = %s (%s), want failed E_DB_NOT_FOUND", final.Status, final.ErrorCode)
	}
	payload := eventPayloads(t, h)["deployment.failed"]
	if !strings.Contains(payload, "known instances") || !strings.Contains(payload, "pg1") {
		t.Fatalf("failure disclosure lacks the candidate list: %s", payload)
	}
	// 未触底座（规划期拒绝）：无服务创建、无引用登记。
	if len(h.sub.services) != 0 {
		t.Fatalf("services created despite plan-time refusal: %v", serviceNames(h.sub))
	}
	refs, _ := h.store.ListDatabaseReferencesByApp(ctx, mustAppID(t, h, "demo"))
	if len(refs) != 0 {
		t.Fatalf("references registered despite plan-time refusal: %+v", refs)
	}
}

// TestDeployDeletingDatabaseReference 引用 deleting 实例与不存在同码面
//（E_DB_NOT_FOUND——设计 §5.2 语义行「不存在或已进入 deleting/deleted」）。
func TestDeployDeletingDatabaseReference(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	inst := createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "pw", state.DatabaseReady)
	if err := h.store.EnterDbPhase(context.Background(), inst.ID, state.DatabaseReady, state.DatabaseDeleting); err != nil {
		t.Fatalf("advance to deleting: %v", err)
	}
	final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose)))
	if final.Status != state.DeployFailed || final.ErrorCode != "E_DB_NOT_FOUND" {
		t.Fatalf("deploy = %s (%s), want failed E_DB_NOT_FOUND (deleting instance)", final.Status, final.ErrorCode)
	}
}

// TestDeployPrefixConflict 前缀冲突哨兵（§2.4 时序行）：同 app 引用
// pg-prod 与 pg_prod（前缀同形 FLEETLY_DB_PG_PROD）→ 失败
// E_DB_ENV_PREFIX_CONFLICT，披露点名冲突双方。
func TestDeployPrefixConflict(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	createDBInstanceForTest(t, h, "pg-prod", testTemplatePostgres16, "pw1", state.DatabaseReady)
	createDBInstanceForTest(t, h, "pg_prod", testTemplatePostgres16, "pw2", state.DatabaseReady)

	yaml := strings.Replace(dbRefCompose, `"pg1"`, `"pg-prod,pg_prod"`, 1)
	final := h.runToTerminal(h.enqueue(h.writeCompose(yaml)))
	if final.Status != state.DeployFailed || final.ErrorCode != "E_DB_ENV_PREFIX_CONFLICT" {
		t.Fatalf("deploy = %s (%s), want failed E_DB_ENV_PREFIX_CONFLICT", final.Status, final.ErrorCode)
	}
	payload := eventPayloads(t, h)["deployment.failed"]
	if !strings.Contains(payload, "pg-prod") || !strings.Contains(payload, "pg_prod") {
		t.Fatalf("failure disclosure does not name both instances: %s", payload)
	}
	// 同一实例被多服务引用合法（同名放行）：sanity——双服务都引用 pg1 的
	// 部署成功（独立 fixture，web/worker 各自声明同一实例）。
	h2 := newHarness(t)
	h2.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	createDBInstanceForTest(t, h2, "pg1", testTemplatePostgres16, "pw", state.DatabaseReady)
	multi := strings.Replace(dbRefComposeNoLabel, "  web:", "  web:\n    labels:\n      fleetly.databases: \"pg1\"", 1)
	multi = strings.Replace(multi, "  worker:", "  worker:\n    labels:\n      fleetly.databases: \"pg1\"", 1)
	if final := h2.runToTerminal(h2.enqueue(h2.writeCompose(multi))); final.Status != state.DeploySucceeded {
		t.Fatalf("same-instance dual reference = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	refs2, err := h2.store.ListDatabaseReferencesByApp(context.Background(), mustAppID(t, h2, "demo"))
	if err != nil || len(refs2) != 2 {
		t.Fatalf("dual reference rows = %+v (%v), want two rows (one per service)", refs2, err)
	}
}

// TestDeployNotReadyReferenceWarns 未就绪引用（D-DB-5：不查就绪只查存在）
// → 部署不阻塞、以 W_DB_REFERENCE_NOT_READY 计划警告事件披露。
func TestDeployNotReadyReferenceWarns(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "pw", state.DatabaseProvisioning)

	final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose)))
	if final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded (readiness must not block)", final.Status, final.ErrorCode)
	}
	// 事件披露（payload 只含 code/service——既有事件管道纪律）。
	payload := eventPayloads(t, h)["deployment.warning"]
	if !strings.Contains(payload, "W_DB_REFERENCE_NOT_READY") || !strings.Contains(payload, `"service":"web"`) {
		t.Fatalf("missing W_DB_REFERENCE_NOT_READY warning event: %s", payload)
	}
	// 警告文案（resolver 层）：如实点名实例名、当前状态与「就绪前连接失败」。
	spec, _, err := composeLoadForTest(t, dbRefCompose)
	if err != nil {
		t.Fatalf("compose load: %v", err)
	}
	_, warnings, err := h.eng.resolveDatabaseReferences(context.Background(), mustAppID(t, h, "demo"), "demo", spec)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Code != "W_DB_REFERENCE_NOT_READY" {
		t.Fatalf("warnings = %+v, want exactly one W_DB_REFERENCE_NOT_READY", warnings)
	}
	for _, want := range []string{"pg1", "provisioning", "will fail"} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("warning message %q lacks %q", warnings[0].Message, want)
		}
	}
}

// composeLoadForTest 加载 fixture compose（resolver 直调断言用）。
func composeLoadForTest(t *testing.T, yaml string) (*compose.Spec, []compose.Warning, error) {
	t.Helper()
	return compose.Load(context.Background(), writeTemp(t, yaml))
}

// TestReferenceRemovalCleanup label 移除 + 重部署：引用行删除、物化行删除、
// 运行 spec 摘除库网络（desired-hash 感知——网络变化即期望态变化）。
func TestReferenceRemovalCleanup(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "pw", state.DatabaseReady)
	if final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose))); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	before := h.sub.services[h.svc("web")].spec.DesiredHash()

	if final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefComposeNoLabel))); final.Status != state.DeploySucceeded {
		t.Fatalf("v2 = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}

	// 引用行清空。
	refs, err := h.store.ListDatabaseReferencesByApp(ctx, mustAppID(t, h, "demo"))
	if err != nil || len(refs) != 0 {
		t.Fatalf("references after removal = %+v (%v), want none", refs, err)
	}
	// 物化行全删（PG 全键）。
	app, _ := h.store.GetAppByName(ctx, "demo")
	for _, key := range []string{"FLEETLY_DB_PG1_URL", "FLEETLY_DB_PG1_HOST", "FLEETLY_DB_PG1_PORT",
		"FLEETLY_DB_PG1_USER", "FLEETLY_DB_PG1_PASSWORD", "FLEETLY_DB_PG1_DATABASE"} {
		if _, err := h.store.GetAppEnv(ctx, app.ID, key); err != state.ErrEnvNotFound {
			t.Fatalf("env %s after removal: err = %v, want ErrEnvNotFound", key, err)
		}
	}
	// 网络/哈希：spec 摘除库网络，desired-hash 变化。
	svc := h.sub.services[h.svc("web")].spec
	if len(svc.Networks) != 1 || svc.Networks[0].Name != h.demoNet() {
		t.Fatalf("networks after removal = %+v, want app net only", svc.Networks)
	}
	if svc.DesiredHash() == before {
		t.Fatal("desired hash unchanged after dropping the reference (network/env must participate)")
	}
	if env := serviceEnvMap(t, h, h.svc("web")); len(env) != 0 {
		t.Fatalf("env after removal = %v, want empty", env)
	}
}

// TestAppDeleteCleansReferences app 删除级联（§2.4「引用 app 删除 = 行级联
// 清理」）：tombstone 第二拍同事务清空 db_references。
func TestAppDeleteCleansReferences(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "pw", state.DatabaseReady)
	if final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	refs, _ := h.store.ListDatabaseReferencesByApp(ctx, mustAppID(t, h, "demo"))
	if len(refs) != 1 {
		t.Fatalf("precondition: references = %+v, want one row", refs)
	}

	if err := h.store.MarkAppDeleting(ctx, mustAppID(t, h, "demo")); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	h.eng.ReapDeletingApps(ctx)
	if got := mustLifecycle(t, h, "demo"); got != state.LifecycleDeleted {
		t.Fatalf("lifecycle = %s, want deleted", got)
	}
	refs, err := h.store.ListDatabaseReferencesByApp(ctx, mustAppID(t, h, "demo"))
	if err != nil || len(refs) != 0 {
		t.Fatalf("references after app deletion = %+v (%v), want cascaded away", refs, err)
	}
}

// TestRollbackReplaysCurrentSystemEnv D-DB-11 主断言：轮换（system 物化行
// 直写新值）后回滚 → 重放 spec 携带**当前**密码，不复活快照旧值。
func TestRollbackReplaysCurrentSystemEnv(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	inst := createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "oldpasswordA1", state.DatabaseReady)
	v1 := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose)))
	if v1.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s (%s), want succeeded", v1.Status, v1.ErrorCode)
	}

	// 模拟 S4 轮换台账面：实例密文 + 物化行（upsert → pending）。
	newPW := "newpasswordB2"
	cipher, err := h.box.Encrypt([]byte(newPW))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := h.store.UpdateDatabaseCredential(ctx, inst.ID, string(cipher)); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	app, _ := h.store.GetAppByName(ctx, "demo")
	if _, err := h.store.SetAppEnv(ctx, app.ID, "FLEETLY_DB_PG1_PASSWORD", string(cipher), "system", "human"); err != nil {
		t.Fatalf("rotate materialized row: %v", err)
	}

	rec, err := EnqueueRollback(ctx, h.store, RollbackInput{AppName: "demo"})
	if err != nil {
		t.Fatalf("enqueue rollback: %v", err)
	}
	final := h.runToTerminal(rec)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("rollback = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	env := serviceEnvMap(t, h, h.svc("web"))
	if env["FLEETLY_DB_PG1_PASSWORD"] != newPW {
		t.Fatalf("replayed password = %q, want current %q (D-DB-11: snapshot value must not revive)", env["FLEETLY_DB_PG1_PASSWORD"], newPW)
	}
	// 其余快照字段照常回放（非 system 键不修正——用镜像引用作对照）。
	v1Specs := decodeForTest(t, h, v1)
	if h.sub.services[h.svc("web")].spec.Image != v1Specs[0].Image {
		t.Fatal("non-system snapshot fields must replay verbatim")
	}
}

// TestRollbackRetainsSnapshotValueWhenSystemRowGone D-DB-11 边界（诚实口
// 径）：键已不在 env_vars 的条目保留快照原值——重物化归一发生在下一次常
// 规部署，回滚路径不做物化。
func TestRollbackRetainsSnapshotValueWhenSystemRowGone(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "oldpasswordA1", state.DatabaseReady)
	v1 := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose)))
	if v1.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s (%s), want succeeded", v1.Status, v1.ErrorCode)
	}
	app, _ := h.store.GetAppByName(ctx, "demo")
	if err := h.store.DeleteAppEnv(ctx, app.ID, "FLEETLY_DB_PG1_PASSWORD"); err != nil {
		t.Fatalf("delete system row: %v", err)
	}

	rec, err := EnqueueRollback(ctx, h.store, RollbackInput{AppName: "demo"})
	if err != nil {
		t.Fatalf("enqueue rollback: %v", err)
	}
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("rollback = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	env := serviceEnvMap(t, h, h.svc("web"))
	if env["FLEETLY_DB_PG1_PASSWORD"] != "oldpasswordA1" {
		t.Fatalf("replayed password = %q, want snapshot value oldpasswordA1 (documented boundary)", env["FLEETLY_DB_PG1_PASSWORD"])
	}
}

// TestRestoreSnapshotSubstitutesCurrentSystemEnv D-DB-11 下沉回归：代换
// 落在 restoreSnapshot 共享原语——归位/回退/漂移收敛四条重放路径统一过
// 此修正（不依赖 kind=rollback 的调用位置）。直接驱动原语：快照旧密码 +
// 台账新密码 → 重放产物带新密码。
func TestRestoreSnapshotSubstitutesCurrentSystemEnv(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	inst := createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, "oldpasswordA1", state.DatabaseReady)
	v1 := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose)))
	if v1.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s (%s), want succeeded", v1.Status, v1.ErrorCode)
	}
	newPW := "newpasswordC3"
	cipher, err := h.box.Encrypt([]byte(newPW))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := h.store.UpdateDatabaseCredential(ctx, inst.ID, string(cipher)); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	app, _ := h.store.GetAppByName(ctx, "demo")
	if _, err := h.store.SetAppEnv(ctx, app.ID, "FLEETLY_DB_PG1_PASSWORD", string(cipher), "system", "human"); err != nil {
		t.Fatalf("rotate materialized row: %v", err)
	}

	specs := decodeForTest(t, h, v1)
	if err := h.eng.restoreSnapshot(ctx, v1, specs); err != nil {
		t.Fatalf("restoreSnapshot: %v", err)
	}
	env := serviceEnvMap(t, h, h.svc("web"))
	if env["FLEETLY_DB_PG1_PASSWORD"] != newPW {
		t.Fatalf("restored password = %q, want current %q (D-DB-11 must apply on every replay path)", env["FLEETLY_DB_PG1_PASSWORD"], newPW)
	}
}

// TestDatabaseReferenceNoCredentialLeak 明文纪律负面测试：引用部署全链
// （成功与失败路径）的事件 payload 与审计摘要零密码材料。
func TestDatabaseReferenceNoCredentialLeak(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	ctx := context.Background()
	const pw = "secretpasswordZ9"
	createDBInstanceForTest(t, h, "pg1", testTemplatePostgres16, pw, state.DatabaseReady)

	// 成功路径。
	if final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}
	// 失败路径（引用缺失 → 规划期拒绝）。
	yaml := strings.Replace(dbRefComposeNoLabel, "  web:", "  web:\n    labels:\n      fleetly.databases: \"ghost\"", 1)
	if final := h.runToTerminal(h.enqueue(h.writeCompose(yaml))); final.Status != state.DeployFailed {
		t.Fatalf("ghost deploy = %s, want failed", final.Status)
	}
	// 全事件 payload 扫描。
	for name, payload := range eventPayloads(t, h) {
		if strings.Contains(payload, pw) {
			t.Errorf("event %s leaks the credential password", name)
		}
	}
	// 全审计摘要扫描。
	audits, err := h.store.RecentAudits(ctx, 200)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	for _, a := range audits {
		if strings.Contains(a.DiffSummary, pw) {
			t.Errorf("audit %s leaks the credential password", a.Action)
		}
	}
}

// TestDatabaseReferenceCrossProjectRejected（v0.3 W2-S4 E4 守卫，rbac-teams
// §4.1「E4 库网络是唯一跨 app 连通通道，准入守门」）：引用实例与 app 不同
// 项目 → 部署失败 E_DB_PROJECT_MISMATCH——跨项目牵线在受理期拒绝（守卫
// 单点在 resolveDatabaseReferences，覆盖 API Deploy / git push 全部入队路
// 径）。正例（同项目引用成功）由上方既有用例承载——夹具已统一为同项目。
func TestDatabaseReferenceCrossProjectRejected(t *testing.T) {
	h := newHarness(t)
	h.eng.WithDatabaseTemplate(dbTemplateTestAdapter{})
	// 实例落在与 demo app 无关的独立项目（SeedProject 每次播种新团队）。
	proj := testsupport.SeedProject(t, h.store)
	cipher, err := h.box.Encrypt([]byte("pw"))
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	if _, err := h.store.CreateDatabaseInstance(context.Background(), state.DatabaseInstance{
		Name:             "pg1",
		Template:         testTemplatePostgres16,
		ImageDigest:      "postgres:16@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CredentialCipher: string(cipher),
		ProjectID:        proj.ID,
		TeamID:           proj.TeamID,
	}); err != nil {
		t.Fatalf("create cross-project instance: %v", err)
	}
	final := h.runToTerminal(h.enqueue(h.writeCompose(dbRefCompose)))
	if final.Status != state.DeployFailed || final.ErrorCode != "E_DB_PROJECT_MISMATCH" {
		t.Fatalf("deploy = %s (%s), want failed E_DB_PROJECT_MISMATCH", final.Status, final.ErrorCode)
	}
	// 负例不落引用登记：跨项目引用零副作用。
	refs, rerr := h.store.ListDatabaseReferencesByApp(context.Background(), mustAppID(t, h, "demo"))
	if rerr != nil || len(refs) != 0 {
		t.Fatalf("references after rejected deploy = %v (err=%v), want empty", refs, rerr)
	}
}
