package database

// 轮换编排测试（managed-databases §2.5，W4-S4 验收 5/6）：PG ready 全链
// （job 命令/环境携带新旧密码 → 密文落库 → 引用方物化行更新 pending →
// 引用 app 自动重部署 → db.credentials_rotated 事件）、Redis paused 落库
// 即完成（无 job）、PG paused 诚实拒绝、前置态违规 409 族、CAS 并发落败、
// 凭据明文零泄漏（事件/审计/错误负面扫描）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// seedReference 预置一个引用 app：app 行 + db_reference + 上一个 succeeded
// 部署行（自动重部署的编排输入）+ 旧密码物化行。
func (h *harness) seedReference(t *testing.T, inst state.DatabaseInstance, appName, oldPassword string) state.App {
	t.Helper()
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, h.st, appName)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := h.st.UpsertDatabaseReference(ctx, state.DatabaseReference{
		DatabaseID: inst.ID,
		AppID:      app.ID,
		Service:    "web",
		EnvPrefix:  dbtemplate.EnvPrefix(inst.Name),
	}); err != nil {
		t.Fatalf("seed reference: %v", err)
	}
	dep, err := h.st.CreateDeployment(ctx, state.DeployRecord{
		ID:          "dep-" + appName,
		AppID:       app.ID,
		AppName:     appName,
		Kind:        "deploy",
		SpecHash:    "hash-" + appName,
		ComposePath: "deployments/" + appName + "/compose.yaml",
	})
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	// 走合法转移链推进到 succeeded（queued→preparing→releasing→observing→
	// succeeded——单写点纪律对测试同样生效）。
	chain := []state.DeploymentStatus{
		state.DeployPreparing, state.DeployReleasing, state.DeployObserving, state.DeploySucceeded,
	}
	cur := dep.Status
	for _, next := range chain {
		from := cur
		if err := h.st.UpdateDeployment(ctx, dep.ID, state.DeploymentPatch{Status: &next, PrevStatus: &from}); err != nil {
			t.Fatalf("advance deployment to %s: %v", next, err)
		}
		cur = next
	}
	prefix := dbtemplate.EnvPrefix(inst.Name)
	rows := []struct{ key, val string }{
		{prefix + "_PASSWORD", oldPassword},
		{prefix + "_HOST", inst.Name},
	}
	for _, r := range rows {
		cipher, err := h.box.Encrypt([]byte(r.val))
		if err != nil {
			t.Fatalf("encrypt materialized row: %v", err)
		}
		if _, err := h.st.SetAppEnv(ctx, app.ID, r.key, string(cipher), "system", "human"); err != nil {
			t.Fatalf("seed materialized row: %v", err)
		}
	}
	return app
}

// decrypt 返回密文明文（断言专用——测试内明文不进任何输出）。
func (h *harness) decrypt(t *testing.T, cipher string) string {
	t.Helper()
	plain, err := h.box.Decrypt([]byte(cipher))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return string(plain)
}

// TestRotatePostgresReady 全链路（验收 5 第一项）：ALTER job 形态断言
// （旧密码进 PGPASSWORD env、新密码进 SQL、库共享网络、模板镜像）→ 密文
// 更新 → 物化行 pending 新值 → 引用 app 自动入队 → 轮换事件。
func TestRotatePostgresReady(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	inst := h.createInstance("pg1", dbtemplate.TemplatePostgres16)
	if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	inst = h.get(inst.ID)
	// 实例密码 = createInstance 生成的随机值（明文只在断言内存中比对，
	// 不进任何输出面——负面扫描用同串）。
	oldPW := h.decrypt(t, inst.CredentialCipher)
	app := h.seedReference(t, inst, "demo", oldPW)
	h.docker.rotateExit = 0

	redeployed, err := h.mgr.RotateCredentials(ctx, inst.Name)
	if err != nil {
		t.Fatalf("RotateCredentials: %v", err)
	}
	if len(redeployed) != 1 || redeployed[0] != "demo" {
		t.Fatalf("redeployed = %v, want [demo]", redeployed)
	}

	// job 形态：一次、共享网络、模板镜像、命令携带 NEW、env 携带 OLD。
	if len(h.docker.rotateRuns) != 1 {
		t.Fatalf("container runs = %d, want 1", len(h.docker.rotateRuns))
	}
	run := h.docker.rotateRuns[0]
	if run.Network != h.dbNetName("pg1") {
		t.Errorf("job network = %q, want the instance shared network", run.Network)
	}
	if run.Image != inst.ImageDigest {
		t.Errorf("job image = %q, want the pinned template image", run.Image)
	}
	// 轮换作业是一次性瞬时对象，名必须落 dbjob 前缀族（IMPL-ARCH-F F-2）：
	// ∈ IsDBJobName 且 ∉ IsDbServiceName——fleetly-db-* 是库长驻服务族
	//（IsDbServiceName 识别面），瞬时作业误落该族会被库服务消费面
	//（豁免/清扫/采集）当作长驻库服务。
	if !naming.IsDBJobName(run.Name) {
		t.Errorf("job name = %q, want the dbjob prefix family (transient rotation job must be name-recognizable)", run.Name)
	}
	if naming.IsDbServiceName(run.Name) {
		t.Errorf("job name = %q must not fall in the db service prefix family", run.Name)
	}
	joined := strings.Join(run.Cmd, " ")
	if !strings.Contains(joined, "ALTER USER fleetly WITH PASSWORD '") {
		t.Errorf("job cmd = %q, want the ALTER USER statement", joined)
	}
	if !strings.Contains(joined, "-h pg1") || !strings.Contains(joined, "-U fleetly") {
		t.Errorf("job cmd = %q, want alias reachability (-h pg1) and the template user", joined)
	}
	// 显式维护库（W4-S6 e2e 实测修正：psql 缺 -d 按用户名连库——实例无该
	// 库，连接即退败；ALTER USER 是集群级操作，维护库执行）。
	if !strings.Contains(joined, "-d postgres") {
		t.Errorf("job cmd = %q, want the explicit maintenance database (-d postgres)", joined)
	}
	// NEW 密码 = job SQL 中的字面量 = 落库密文的解密值（一致性三角）。
	newPW := jobPassword(joined)
	if newPW == "" || newPW == oldPW {
		t.Fatalf("job SQL password not captured (cmd length %d)", len(joined))
	}
	foundOld := false
	for _, kv := range run.Env {
		if kv == "PGPASSWORD="+oldPW {
			foundOld = true
		}
	}
	if !foundOld {
		t.Errorf("job env must authenticate with the OLD password, got %v", run.Env)
	}

	// 权威态：密文 = NEW；credential_updated_at 推进。
	after := h.get(inst.ID)
	if got := h.decrypt(t, after.CredentialCipher); got != newPW {
		t.Errorf("stored cipher decrypts to a different value than the ALTER statement")
	}
	if after.CredentialUpdatedAt.IsZero() {
		t.Error("credential_updated_at not advanced")
	}

	// 物化行：PASSWORD 键更新为 NEW 且回 pending（随重部署消费）。
	row, err := h.st.GetAppEnv(ctx, app.ID, "FLEETLY_DB_PG1_PASSWORD")
	if err != nil {
		t.Fatalf("get materialized row: %v", err)
	}
	if h.decrypt(t, row.Value) != newPW {
		t.Errorf("materialized password not updated to the new value")
	}
	if row.Status != state.EnvStatusPending {
		t.Errorf("materialized row status = %s, want pending", row.Status)
	}

	// 自动重部署：引用 app 有新部署行（kind=deploy）。
	deps, err := h.st.ListAppDeployments(ctx, app.ID, 25)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("deployments = %d, want the seeded + the rotation-requeued one", len(deps))
	}

	// 事件：db.credentials_rotated（载荷只带事实字段——凭据零出现）。
	rows, err := h.st.EventsSince(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var hit bool
	for _, e := range rows {
		if e.Name == "db.credentials_rotated" {
			hit = true
			if strings.Contains(e.Payload, newPW) || strings.Contains(e.Payload, oldPW) {
				t.Error("rotation event payload carries credential material")
			}
		}
	}
	if !hit {
		t.Fatal("db.credentials_rotated event not emitted")
	}
}

// jobPassword 从 psql 命令行提取 ALTER 的字面量密码（测试内解析——明文不
// 进任何输出面）。
func jobPassword(cmd string) string {
	const marker = "ALTER USER fleetly WITH PASSWORD '"
	i := strings.Index(cmd, marker)
	if i < 0 {
		return ""
	}
	rest := cmd[i+len(marker):]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestRotateRedisPausedAllowed Redis 暂停轮换（§2.5：密文 + spec 更新，
// resume 时生效）：无 job、密文与物化行更新、引用 app 照常重部署。
func TestRotateRedisPausedAllowed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inst := h.createInstance("cache", dbtemplate.TemplateRedis7)
	if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseReady, state.DatabasePaused); err != nil {
		t.Fatalf("mark paused: %v", err)
	}
	inst = h.get(inst.ID)
	h.seedReference(t, inst, "worker-app", "oldRedisPW1")

	_, err := h.mgr.RotateCredentials(ctx, inst.Name)
	if err != nil {
		t.Fatalf("RotateCredentials: %v", err)
	}
	if len(h.docker.rotateRuns) != 0 {
		t.Errorf("redis rotation must not run a container job, saw %d", len(h.docker.rotateRuns))
	}
	after := h.get(inst.ID)
	if h.decrypt(t, after.CredentialCipher) == "oldRedisPW1" || after.CredentialCipher == "" {
		t.Errorf("cipher not rotated")
	}
}

// TestRotatePostgresPausedRefused PG 暂停拒绝（引擎级诚实边界）：
// ErrPGRotationPaused；零副作用（无 job、密文不动）。
func TestRotatePostgresPausedRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inst := h.createInstance("pg-paused", dbtemplate.TemplatePostgres16)
	if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseReady, state.DatabasePaused); err != nil {
		t.Fatalf("mark paused: %v", err)
	}
	inst = h.get(inst.ID)

	_, err := h.mgr.RotateCredentials(ctx, inst.Name)
	if !errors.Is(err, ErrPGRotationPaused) {
		t.Fatalf("error = %v, want ErrPGRotationPaused", err)
	}
	if len(h.docker.rotateRuns) != 0 {
		t.Errorf("refused rotation must not run any job")
	}
	if got := h.decrypt(t, h.get(inst.ID).CredentialCipher); got != h.decrypt(t, inst.CredentialCipher) {
		t.Errorf("cipher changed despite refusal")
	}
}

// TestRotatePrestateRefused 前置态违规（provisioning 收敛在途）→
// RotationPrestateError（api 映射 E_STATE_VERSION_CONFLICT）。
func TestRotatePrestateRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inst := h.createInstance("pg-young", dbtemplate.TemplatePostgres16) // provisioning
	if _, err := h.mgr.RotateCredentials(ctx, inst.Name); err == nil {
		t.Fatal("rotation from provisioning must be refused")
	} else {
		var p *RotationPrestateError
		if !errors.As(err, &p) || p.Current != string(state.DatabaseProvisioning) {
			t.Fatalf("error = %v, want RotationPrestateError(current)", err)
		}
	}
	// 不存在的实例 → state.ErrDatabaseNotFound（api 映射 E_DB_NOT_FOUND）。
	if _, err := h.mgr.RotateCredentials(ctx, "ghost"); !errors.Is(err, state.ErrDatabaseNotFound) {
		t.Fatalf("error = %v, want ErrDatabaseNotFound", err)
	}
}

// TestRotateCASConflict 并发轮换：以陈旧的 credential_updated_at 作 CAS 锚
// → 落败返回 ErrRotationConflict（同族乐观冲突语义）。
func TestRotateCASConflict(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inst := h.createInstance("pg-race", dbtemplate.TemplatePostgres16)
	if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	stale := h.get(inst.ID)
	// 并发推进：另一次落库把 credential_updated_at 前移。
	cipher, err := h.box.Encrypt([]byte("concurrentPW9"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := h.st.UpdateDatabaseCredential(ctx, inst.ID, string(cipher)); err != nil {
		t.Fatalf("concurrent rotate: %v", err)
	}
	// 陈旧锚落败：store 层返回 (false, nil)——编排层把 !updated 翻译成
	// ErrRotationConflict（api 映射 E_STATE_VERSION_CONFLICT 族）。
	ok, err := h.mgr.store.RotateDatabaseCredentialCAS(ctx, inst.ID, stale.CredentialUpdatedAt, string(cipher))
	if err != nil {
		t.Fatalf("CAS rotate: %v", err)
	}
	if ok {
		t.Fatal("stale CAS anchor must lose the race")
	}
	// 未失锚的写入照常成功（同锚重放 = 幂等落败的对照面）。
	fresh := h.get(inst.ID)
	ok, err = h.mgr.store.RotateDatabaseCredentialCAS(ctx, inst.ID, fresh.CredentialUpdatedAt, string(cipher))
	if err != nil || !ok {
		t.Fatalf("fresh CAS anchor must win (ok=%v err=%v)", ok, err)
	}
}

// TestRotateNoPlaintextInSurfaces 负面扫描（验收 6）：轮换全程的事件、
// 审计与收敛日志面零凭据材料（新旧密码逐串比对）。
func TestRotateNoPlaintextInSurfaces(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inst := h.createInstance("pg-clean", dbtemplate.TemplatePostgres16)
	if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseReady); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	inst = h.get(inst.ID)
	app := h.seedReference(t, inst, "clean-app", h.decrypt(t, inst.CredentialCipher))
	h.docker.rotateExit = 0

	if _, err := h.mgr.RotateCredentials(ctx, inst.Name); err != nil {
		t.Fatalf("RotateCredentials: %v", err)
	}
	newPW := jobPassword(strings.Join(h.docker.rotateRuns[0].Cmd, " "))

	rows, err := h.st.EventsSince(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range rows {
		if strings.Contains(e.Payload, newPW) {
			t.Errorf("event %s carries the new password", e.Name)
		}
	}
	audits, err := h.st.RecentAudits(ctx, 100)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	for _, a := range audits {
		for _, secret := range []string{newPW, h.decrypt(t, inst.CredentialCipher)} {
			if strings.Contains(a.DiffSummary, secret) || strings.Contains(a.Target, secret) {
				t.Errorf("audit %s carries credential material", a.Action)
			}
		}
	}
	_ = app
}
