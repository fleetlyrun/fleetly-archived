package database

// v0.3 W4 新引擎（mysql-8.4 / mongodb-8.0，D-W4-1~3）适配器单测：备份
// job 命令词表与凭据 env（MYSQL_PWD / MONGO_PASSWORD 的 ${…} 引用形态）、
// 回读校验（mysqldump 文件头 / gzip 魔术）、原地恢复脚本（临时
// mysqld/mongod 重放——skip-grant socket-only / 免认证 loopback，PG 同
// 暴露类）、轮换原语（ALTER USER 'fleetly'@'%' / updateUser(admin)）与
// 暂停拒绝、明文纪律负面扫描（命令词表零凭据材料）。
//
// e2e 真跑腿挂账 S4（依赖 S2 的 dbtools 镜像扩展装载 mysqld/mongod）——
// 本文件的断言面 = fake 底座上的载荷形态（与既有 PG/Redis 测试同口径）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// runBackupToLedger 对实例触发一次手动备份并等台账行落账（备份/校验/prune
// 三步 job 全 complete 的 happy path），返回备份 job 载荷。
func runBackupToLedger(t *testing.T, h *harness, inst state.DatabaseInstance) JobRunInput {
	t.Helper()
	h.saveS3Settings()
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		if strings.Contains(strings.Join(in.Cmd, " "), "backup --stdin") {
			return JobRunOutcome{State: "complete", Stdout: resticSummaryOutput("snap-w4-"+inst.Name, 4096)}
		}
		return JobRunOutcome{State: "complete", ExitCode: 0} // verify/prune
	}
	if err := h.mgr.TriggerBackup(context.Background(), inst.Name); err != nil {
		t.Fatalf("TriggerBackup(%s): %v", inst.Name, err)
	}
	waitUntil(t, 5*time.Second, "backup ledger row", func() bool {
		rows, err := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
		return err == nil && len(rows) == 1 && rows[0].VerifyStatus == state.DatabaseVerifyVerified
	})
	jobs := h.docker.jobsWithPurpose("backup")
	if len(jobs) != 1 {
		t.Fatalf("backup jobs = %d, want 1", len(jobs))
	}
	return jobs[0]
}

// TestMySQLBackupJobSpec MySQL 备份 job 的载荷断言（D-W4-3）：mysqldump
// 一致性快照词表、repo 路径命名空间、MYSQL_PWD env、命令零明文。
func TestMySQLBackupJobSpec(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("my-bk", dbtemplate.TemplateMySQL84)
	h.setTasks(h.svcName(inst), dockerapi.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	j := runBackupToLedger(t, h, inst)

	if j.Image != DefaultDatabaseToolsImage {
		t.Errorf("image = %s, want the digest-pinned dbtools image", j.Image)
	}
	script := strings.Join(j.Cmd, " ")
	for _, want := range []string{
		"mysqldump -h my-bk -u fleetly --single-transaction --databases my_bk",
		"backup --stdin --stdin-filename db/my-bk/db.sql",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
	// 凭据只在 env（MYSQL_PWD——mysql 客户端原生消费），命令词表零明文。
	plain, err := h.box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if strings.Contains(script, string(plain)) {
		t.Error("credential plaintext leaked into the backup command")
	}
	if !strings.Contains(strings.Join(j.Env, "\n"), "MYSQL_PWD="+string(plain)) {
		t.Error("MYSQL_PWD env missing (mysqldump cannot authenticate without it)")
	}
	// 校验词表：mysqldump 文件头魔术串（「备份假成功」零容忍的引擎级实现）。
	verifies := h.docker.jobsWithPurpose("verify")
	if len(verifies) != 1 {
		t.Fatalf("verify jobs = %d, want 1", len(verifies))
	}
	vscript := strings.Join(verifies[0].Cmd, " ")
	for _, want := range []string{"dump snap-w4-my-bk db/my-bk/db.sql", `head -c 32`, `grep -q "MySQL dump"`} {
		if !strings.Contains(vscript, want) {
			t.Errorf("verify script %q missing %q", vscript, want)
		}
	}
	// 保留对齐路径过滤随新引擎生效（共享 repo 的安全前提）。
	prunes := h.docker.jobsWithPurpose("prune")
	if len(prunes) != 1 || !strings.Contains(strings.Join(prunes[0].Cmd, " "), "--path db/my-bk/db.sql") {
		t.Errorf("prune job not scoped to the instance path: %+v", prunes)
	}
}

// TestMongoBackupJobSpec MongoDB 备份 job 的载荷断言（D-W4-3）：
// mongodump --archive --gzip、URI 经 ${MONGO_PASSWORD} 引用展开（字面量
// 不进 job spec）、authSource=admin、gzip 魔术校验。
func TestMongoBackupJobSpec(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("mo-bk", dbtemplate.TemplateMongoDB80)
	h.setTasks(h.svcName(inst), dockerapi.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	j := runBackupToLedger(t, h, inst)

	script := strings.Join(j.Cmd, " ")
	for _, want := range []string{
		`mongodump --uri "mongodb://fleetly:${MONGO_PASSWORD}@mo-bk:27017/mo_bk?authSource=admin" --archive --gzip`,
		"backup --stdin --stdin-filename db/mo-bk/db.archive",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
	// 凭据纪律：命令词表只含 ${MONGO_PASSWORD} 引用（shell 运行时展开），
	// 明文只进 env——与 Redis 健康门 env 引用同暴露类。
	plain, err := h.box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if strings.Contains(script, string(plain)) {
		t.Error("credential plaintext leaked into the backup command (must stay a ${MONGO_PASSWORD} reference)")
	}
	if !strings.Contains(script, "${MONGO_PASSWORD}") {
		t.Error("backup command must reference ${MONGO_PASSWORD} (env expansion at runtime)")
	}
	if !strings.Contains(strings.Join(j.Env, "\n"), "MONGO_PASSWORD="+string(plain)) {
		t.Error("MONGO_PASSWORD env missing")
	}
	// 校验词表：gzip 魔术 1f 8b（--gzip 归档流头）。
	verifies := h.docker.jobsWithPurpose("verify")
	if len(verifies) != 1 {
		t.Fatalf("verify jobs = %d, want 1", len(verifies))
	}
	vscript := strings.Join(verifies[0].Cmd, " ")
	for _, want := range []string{"dump snap-w4-mo-bk db/mo-bk/db.archive", "head -c 2", "grep -q 1f8b"} {
		if !strings.Contains(vscript, want) {
			t.Errorf("verify script %q missing %q", vscript, want)
		}
	}
}

// TestMySQLAndMongoRestoreJobScripts 原地恢复 job 的命令词表（D-W4-3 停库
// 重放）：快照落卷根暂存 → 临时引擎实例重放 → 关停清场；挂载 rw + 钉绑定
// 节点；命令零凭据（skip-grant socket-only / 免认证 loopback，PG 同暴露
// 类）；重放完成副本回到 1、主状态不变。
func TestMySQLAndMongoRestoreJobScripts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		instance  string
		template  string
		snap      string
		mountPath string
		wants     []string
	}{
		{
			name:      "mysql",
			instance:  "my-rs",
			template:  dbtemplate.TemplateMySQL84,
			snap:      "snap-rs-my",
			mountPath: "/var/lib/mysql",
			wants: []string{
				"dump snap-rs-my db/my-rs/db.sql > /var/lib/mysql/fleetly-replay.sql",
				"mysqld --skip-grant-tables --skip-networking --socket=/run/mysqld/mysqld.sock --datadir=/var/lib/mysql",
				"DROP DATABASE IF EXISTS `my_rs`",
				"mysql --socket=/run/mysqld/mysqld.sock < /var/lib/mysql/fleetly-replay.sql",
				"mysqladmin --socket=/run/mysqld/mysqld.sock shutdown",
				"rm -f /var/lib/mysql/fleetly-replay.sql",
			},
		},
		{
			name:      "mongo",
			instance:  "mo-rs",
			template:  dbtemplate.TemplateMongoDB80,
			snap:      "snap-rs-mo",
			mountPath: "/data/db",
			wants: []string{
				"dump snap-rs-mo db/mo-rs/db.archive > /data/db/fleetly-replay.archive",
				"mongod --dbpath /data/db --bind_ip 127.0.0.1 --port 27017",
				"mongorestore --host 127.0.0.1 --port 27017 --archive=/data/db/fleetly-replay.archive --gzip --drop",
				"db.adminCommand({shutdown: 1})",
				"rm -f /data/db/fleetly-replay.archive",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			snap := tc.snap
			inst := seedReadyWithLedger(t, h, tc.instance, tc.template, snap)
			h.saveS3Settings()
			var restoreJob *JobRunInput
			h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
				if strings.Contains(strings.Join(in.Cmd, " "), "replay") {
					j := in
					restoreJob = &j
				}
				return JobRunOutcome{State: "complete", ExitCode: 0}
			}
			if err := h.mgr.RestoreBackup(context.Background(), tc.instance, snap); err != nil {
				t.Fatalf("RestoreBackup: %v", err)
			}
			waitUntil(t, 5*time.Second, "restore_completed event", func() bool {
				events, _ := h.st.EventsSince(context.Background(), 0, 200)
				for _, ev := range events {
					if ev.Name == "db.restore_completed" && strings.Contains(ev.Payload, snap) {
						return true
					}
				}
				return false
			})
			if restoreJob == nil {
				t.Fatal("no restore job ran")
			}
			m := restoreJob.Mounts[0]
			if m.VolumeName != "fleetly-db-"+tc.instance+"-data-"+inst.ID[:8] || m.Target != tc.mountPath || m.ReadOnly {
				t.Errorf("restore mount = %+v, want the data volume rw at %s", m, tc.mountPath)
			}
			if len(restoreJob.Constraints) != 1 || restoreJob.Constraints[0] != "node.labels.fleetly.node-id == n_node" {
				t.Errorf("restore constraints = %v, want the platform node-label pin", restoreJob.Constraints)
			}
			script := strings.Join(restoreJob.Cmd, " ")
			for _, want := range tc.wants {
				if !strings.Contains(script, want) {
					t.Errorf("restore script missing %q: %s", want, script)
				}
			}
			// 命令词表零凭据（临时实例不消费引擎凭据——卷内密码错位不影响
			// 重放的构造性证明）。
			plain, derr := h.box.Decrypt([]byte(inst.CredentialCipher))
			if derr != nil {
				t.Fatalf("decrypt: %v", derr)
			}
			if strings.Contains(script, string(plain)) {
				t.Error("credential plaintext leaked into the restore script")
			}
			// 重部署收口：副本 1、主状态不变（操作不换主状态）。
			if svc := h.docker.services[h.svcName(inst)]; svc.Replicas != 1 {
				t.Errorf("replicas after restore = %d, want 1", svc.Replicas)
			}
			if got := h.get(inst.ID); got.State != state.DatabaseReady {
				t.Errorf("state = %s, want ready (operation never changes the main state)", got.State)
			}
		})
	}
}

// mysqlRotatePassword 从 mysql 轮换命令提取 ALTER 字面量密码（测试内解析
// ——明文不进任何输出面）。
func mysqlRotatePassword(cmd string) string {
	const marker = "IDENTIFIED BY '"
	i := strings.Index(cmd, marker)
	if i < 0 {
		return ""
	}
	rest := cmd[i+len(marker):]
	return rest[:strings.Index(rest, "'")]
}

// mongoRotatePassword 从 mongo 轮换命令提取 updateUser 字面量密码。
func mongoRotatePassword(cmd string) string {
	const marker = "updateUser('fleetly', {pwd: '"
	i := strings.Index(cmd, marker)
	if i < 0 {
		return ""
	}
	rest := cmd[i+len(marker):]
	return rest[:strings.Index(rest, "'")]
}

// TestRotateMySQLAndMongo 新引擎轮换全链（D-W4-3 轮换原语 + §2.5 既有编
// 排）：job 形态（模板镜像 / 共享网络 / 旧密码 env / 新密码字面量）→ 密
// 文落库 → 物化行 pending → 引用 app 自动重部署 → 事件零凭据；暂停态如
// 实拒绝（ErrPGRotationPaused 共用哨兵）。
func TestRotateMySQLAndMongo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instance string
		template string
		cmdWants []string
		envKey   string
		pwdOf    func(string) string
	}{
		{
			name:     "mysql",
			instance: "my-rot",
			template: dbtemplate.TemplateMySQL84,
			cmdWants: []string{"-h my-rot", "-u fleetly", "ALTER USER 'fleetly'@'%' IDENTIFIED BY '"},
			envKey:   "MYSQL_PWD=",
			pwdOf:    mysqlRotatePassword,
		},
		{
			name:     "mongo",
			instance: "mo-rot",
			template: dbtemplate.TemplateMongoDB80,
			cmdWants: []string{
				`mongosh "mongodb://fleetly:${MONGO_PASSWORD}@mo-rot:27017/admin"`,
				`db.getSiblingDB('admin').updateUser('fleetly', {pwd: '`,
				"sh -c",
			},
			envKey: "MONGO_PASSWORD=",
			pwdOf:  mongoRotatePassword,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			inst := h.createInstance(tc.instance, tc.template)
			if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseReady); err != nil {
				t.Fatalf("mark ready: %v", err)
			}
			inst = h.get(inst.ID)
			oldPW := h.decrypt(t, inst.CredentialCipher)
			app := h.seedReference(t, inst, "w4-app", oldPW)
			h.docker.rotateExit = 0

			redeployed, err := h.mgr.RotateCredentials(ctx, inst.Name)
			if err != nil {
				t.Fatalf("RotateCredentials: %v", err)
			}
			if len(redeployed) != 1 || redeployed[0] != "w4-app" {
				t.Fatalf("redeployed = %v, want [w4-app]", redeployed)
			}
			if len(h.docker.rotateRuns) != 1 {
				t.Fatalf("container runs = %d, want 1", len(h.docker.rotateRuns))
			}
			run := h.docker.rotateRuns[0]
			if run.Image != inst.ImageDigest {
				t.Errorf("job image = %q, want the pinned template image", run.Image)
			}
			if run.Network != h.dbNetName(tc.instance) {
				t.Errorf("job network = %q, want the instance shared network", run.Network)
			}
			joined := strings.Join(run.Cmd, " ")
			for _, want := range tc.cmdWants {
				if !strings.Contains(joined, want) {
					t.Errorf("job cmd %q missing %q", joined, want)
				}
			}
			// NEW = 命令字面量 = 落库密文的解密值（一致性三角）。
			newPW := tc.pwdOf(joined)
			if newPW == "" || newPW == oldPW {
				t.Fatalf("rotation literal password not captured (cmd length %d)", len(joined))
			}
			if got := h.decrypt(t, h.get(inst.ID).CredentialCipher); got != newPW {
				t.Errorf("stored cipher decrypts to a different value than the engine-side statement")
			}
			// OLD 只进 env；mongo 的命令词表零旧密码明文（${…} 引用）。
			foundOld := false
			for _, kv := range run.Env {
				if kv == tc.envKey+oldPW {
					foundOld = true
				}
			}
			if !foundOld {
				t.Errorf("job env must authenticate with the OLD password via %s, got %v", tc.envKey, run.Env)
			}
			if strings.Contains(joined, oldPW) {
				t.Error("old credential plaintext leaked into the rotation command")
			}
			// 物化行 pending + 事件零凭据（§2.5 编排链对新引擎的既有纪律）。
			prefix := dbtemplate.EnvPrefix(inst.Name)
			row, err := h.st.GetAppEnv(ctx, app.ID, prefix+"_PASSWORD")
			if err != nil {
				t.Fatalf("get materialized row: %v", err)
			}
			if h.decrypt(t, row.Value) != newPW || row.Status != state.EnvStatusPending {
				t.Errorf("materialized row not rotated to pending with the new value: status=%s", row.Status)
			}
			events, _ := h.st.EventsSince(ctx, 0, 200)
			for _, e := range events {
				if strings.Contains(e.Payload, newPW) || strings.Contains(e.Payload, oldPW) {
					t.Errorf("event %s carries credential material", e.Name)
				}
			}

			// 暂停拒绝（引擎级边界——停摆实例无法执行改密语句）：零副作用。
			if err := h.st.EnterDbPhase(ctx, inst.ID, state.DatabaseReady, state.DatabasePaused); err != nil {
				t.Fatalf("mark paused: %v", err)
			}
			runsBefore := len(h.docker.rotateRuns)
			if _, err := h.mgr.RotateCredentials(ctx, tc.instance); !errors.Is(err, ErrPGRotationPaused) {
				t.Fatalf("paused rotation err = %v, want ErrPGRotationPaused", err)
			}
			if len(h.docker.rotateRuns) != runsBefore {
				t.Error("refused rotation must not run any job")
			}
		})
	}
}
