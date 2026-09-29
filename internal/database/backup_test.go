package database

// 备份编排与 job 载荷的单测（S5 验收 2/5 + 明文纪律负面扫描）：备份 job
// 规格（digest 钉定 dbtools、网络挂接含 rustfs 模式、凭据只在 env、命令
// 构造）、verify → 台账生命周期（verified/failed 三态）、S3 未配置诚实拒
// 绝、per 实例操作互斥、调度窗与保留对齐（restic forget 路径过滤 + 台账
// 行镜像）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// TestBackupJobSpecAssertions 备份 job 的载荷断言（验收 2）：镜像 = digest
// 钉定 dbtools、放置约束钉绑定节点、挂实例共享网络、凭据只在 env、PG/Redis
// 各自的命令词表形态。
func TestBackupJobSpecAssertions(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-bk", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	h.saveS3Settings()
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		if strings.Contains(strings.Join(in.Cmd, " "), "backup --stdin") {
			return JobRunOutcome{State: "complete", Stdout: resticSummaryOutput("snap-pg-1", 1024)}
		}
		return JobRunOutcome{State: "complete", ExitCode: 0} // verify/prune
	}

	if err := h.mgr.TriggerBackup(context.Background(), "pg-bk"); err != nil {
		t.Fatalf("TriggerBackup: %v", err)
	}
	waitUntil(t, 5*time.Second, "backup ledger row", func() bool {
		rows, err := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
		return err == nil && len(rows) == 1
	})

	job := h.docker.jobsWithPurpose("backup")
	if len(job) != 1 {
		t.Fatalf("backup jobs = %d, want 1", len(job))
	}
	j := job[0]
	if j.Image != DefaultDatabaseToolsImage {
		t.Errorf("image = %s, want the digest-pinned dbtools image", j.Image)
	}
	if len(j.Constraints) != 1 || j.Constraints[0] != "node.labels.fleetly.node-id == n_node" {
		t.Errorf("constraints = %v, want the platform node-label pin (swarm node.id never equals the platform id)", j.Constraints)
	}
	if !h.docker.networks[h.dbNetName("pg-bk")] {
		t.Errorf("instance shared network %v not attached", j.Networks)
	}
	if !strings.HasPrefix(j.Name, "fleetly-dbjob-pg-bk-backup-") {
		t.Errorf("job name = %s, want the sweep-safe dbjob prefix family", j.Name)
	}
	script := strings.Join(j.Cmd, " ")
	// 命令词表：pg_dump 以实例别名连接、--stdin 入库、repo 路径命名空间。
	for _, want := range []string{"pg_dump -h pg-bk -U fleetly -d pg_bk -Fc", "--stdin --stdin-filename db/pg-bk/db.dump", "backup --stdin"} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
	// 凭据只在 env：命令零密码；env 带 PGPASSWORD + restic 材料。
	plain, err := h.box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if strings.Contains(script, string(plain)) {
		t.Error("credential plaintext leaked into the job command")
	}
	envStr := strings.Join(j.Env, "\n")
	if !strings.Contains(envStr, "PGPASSWORD="+string(plain)) {
		t.Error("PGPASSWORD env missing (pg_dump cannot authenticate without it)")
	}
	if !strings.Contains(envStr, "RESTIC_PASSWORD=test-repo-pass") || !strings.Contains(envStr, "AWS_ACCESS_KEY_ID=testak") || !strings.Contains(envStr, "AWS_SECRET_ACCESS_KEY=test-s3-secret") {
		t.Errorf("restic env incomplete: %s", envStr)
	}
	// repo 目标必须进 env（W4-S6 e2e 实测修正：缺 RESTIC_REPOSITORY = restic
	// 以「Please specify repository location」退败——fake 底座不看 env 真实
	// 性，只有这里能钉）。
	if !strings.Contains(envStr, "RESTIC_REPOSITORY=s3:http://s3.test:9000/testbucket/statebackups") {
		t.Errorf("RESTIC_REPOSITORY env missing: %s", envStr)
	}
	// 外部 S3（path_style）不经 rustfs 网络。
	for _, e := range j.Env {
		if e == "" {
			t.Error("empty env entry")
		}
	}
	if len(j.Networks) != 1 || j.Networks[0] != h.dbNetName("pg-bk") {
		t.Errorf("networks = %v, want exactly the instance network in external mode", j.Networks)
	}
	// 台账生命周期：verified 行 + 快照 + 字节量。
	row, err := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
	if err != nil || len(row) != 1 {
		t.Fatalf("ledger rows = %v err=%v, want 1", row, err)
	}
	if row[0].VerifyStatus != state.DatabaseVerifyVerified || row[0].ResticSnapshot != "snap-pg-1" || row[0].SizeBytes != 1024 {
		t.Errorf("row = %+v, want verified snap-pg-1 1024B", row[0])
	}
	if row[0].Kind != state.DatabaseBackupManual {
		t.Errorf("kind = %s, want manual", row[0].Kind)
	}
}

// TestRedisBackupJobSpec Redis 备份的命令词表与 rustfs 网络挂接（验收 2）。
func TestRedisBackupJobSpec(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("rd-bk", dbtemplate.TemplateRedis7)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	// rustfs 模式：restic 目标 = 托管端点派生 + job 挂 fleetly-rustfs-net。
	if err := h.st.SaveS3Settings(context.Background(), state.S3Settings{Mode: state.S3ModeRustfs}, state.S3SaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save s3: %v", err)
	}
	ak, pw, sk := "rs-ak", "rs-pass", "rs-s3-secret"
	aCT, _ := h.box.Encrypt([]byte(ak))
	sCT, _ := h.box.Encrypt([]byte(sk))
	pCT, _ := h.box.Encrypt([]byte(pw))
	if err := h.st.SaveRustfsCredentialsCiphertext(context.Background(), string(aCT), string(sCT)); err != nil {
		t.Fatalf("save rustfs creds: %v", err)
	}
	if err := h.st.SaveResticPasswordCiphertext(context.Background(), string(pCT)); err != nil {
		t.Fatalf("save restic pw: %v", err)
	}
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		if strings.Contains(strings.Join(in.Cmd, " "), "backup --stdin") {
			return JobRunOutcome{State: "complete", Stdout: resticSummaryOutput("snap-rd-1", 512)}
		}
		return JobRunOutcome{State: "complete", ExitCode: 0}
	}
	if err := h.mgr.TriggerBackup(context.Background(), "rd-bk"); err != nil {
		t.Fatalf("TriggerBackup: %v", err)
	}
	waitUntil(t, 5*time.Second, "redis backup row", func() bool {
		rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
		return len(rows) == 1
	})
	jobs := h.docker.jobsWithPurpose("backup")
	if len(jobs) != 1 {
		t.Fatalf("backup jobs = %d, want 1", len(jobs))
	}
	j := jobs[0]
	script := strings.Join(j.Cmd, " ")
	for _, want := range []string{"redis-cli -h rd-bk --no-auth-warning --rdb /dev/stdout", "backup --stdin --stdin-filename db/rd-bk/dump.rdb"} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
	// rustfs 模式网络：实例网络 + rustfs 网络；restic 目标带 path-style 扩
	// 展选项（rustfs 端点 path-style 寻址）。
	if len(j.Networks) != 2 {
		t.Fatalf("networks = %v, want [instance-net rustfs-net]", j.Networks)
	}
	if j.Networks[1] != state.RustfsNetworkName {
		t.Errorf("second network = %s, want %s", j.Networks[1], state.RustfsNetworkName)
	}
	if !strings.Contains(script, "-o s3.bucket-lookup=path") {
		t.Errorf("rustfs mode requires the path-style lookup option, script: %s", script)
	}
	// redis 凭据经 REDISCLI_AUTH env（命令零 -a 密码字面量）。
	plain, err := h.box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if strings.Contains(script, " -a ") || strings.Contains(script, string(plain)) {
		t.Error("redis credential leaked into the command (must travel via REDISCLI_AUTH env)")
	}
	if !strings.Contains(strings.Join(j.Env, "\n"), "REDISCLI_AUTH="+string(plain)) {
		t.Error("REDISCLI_AUTH env missing")
	}
}

// TestBackupS3UnsetHonest S3 未配置的诚实拒绝（验收负面）：手动触发同步回
// 哨兵、事件面 db.backup_failed、无台账行。
func TestBackupS3UnsetHonest(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-nos3", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()

	err := h.mgr.TriggerBackup(context.Background(), "pg-nos3")
	if err == nil {
		t.Fatal("TriggerBackup must refuse when s3.mode=unset")
	}
	if !strings.Contains(err.Error(), "s3.mode=unset") && !strings.Contains(err.Error(), "object storage is not configured") {
		t.Errorf("error = %v, want the honest s3-not-configured reason", err)
	}
	waitUntil(t, 5*time.Second, "backup_failed event", func() bool {
		events, eerr := h.st.EventsSince(context.Background(), 0, 100)
		if eerr != nil {
			return false
		}
		for _, ev := range events {
			if ev.Name == "db.backup_failed" && strings.Contains(ev.Payload, "pg-nos3") {
				return true
			}
		}
		return false
	})
	rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
	if len(rows) != 0 {
		t.Errorf("ledger rows = %d, want 0 (failed backups record no row)", len(rows))
	}
	if jobs := h.docker.jobsWithPurpose("backup"); len(jobs) != 0 {
		t.Errorf("no job may run without a restic target, got %d", len(jobs))
	}
}

// TestBackupVerifyFailedHonesty verify 失败的红色面（「备份假成功」零容忍
// ——台账行 verify_status=failed + db.backup_failed 事件，不冒充成功）。
func TestBackupVerifyFailedHonesty(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-vf", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	h.saveS3Settings()
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		script := strings.Join(in.Cmd, " ")
		switch {
		case strings.Contains(script, "backup --stdin"):
			return JobRunOutcome{State: "complete", Stdout: resticSummaryOutput("snap-vf-1", 2048)}
		case strings.Contains(script, "pg_restore --list"):
			return JobRunOutcome{State: "failed", Err: "verify job failed: pg_restore reported a corrupt archive", ExitCode: 1}
		default:
			return JobRunOutcome{State: "complete", ExitCode: 0}
		}
	}
	if err := h.mgr.TriggerBackup(context.Background(), "pg-vf"); err != nil {
		t.Fatalf("TriggerBackup: %v", err)
	}
	waitUntil(t, 5*time.Second, "failed verify row", func() bool {
		rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
		return len(rows) == 1 && rows[0].VerifyStatus == state.DatabaseVerifyFailed
	})
	rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
	if rows[0].Error == "" {
		t.Error("verify failure must carry the reason in the ledger error column")
	}
	if rows[0].ResticSnapshot != "snap-vf-1" {
		t.Errorf("snapshot = %s, want snap-vf-1 (the artifact exists; the VERDICT is failed)", rows[0].ResticSnapshot)
	}
	events, _ := h.st.EventsSince(context.Background(), 0, 100)
	for _, ev := range events {
		if ev.Name == "db.backup_succeeded" {
			t.Error("a verify-failed backup must not surface as db.backup_succeeded")
		}
	}
}

// TestBackupOperationMutex per 实例操作互斥（并发第二笔拒绝）。
func TestBackupOperationMutex(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-mtx", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	h.saveS3Settings()
	release := make(chan struct{})
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		<-release // 备份 job 在途悬挂——互斥窗口
		return JobRunOutcome{State: "complete", Stdout: resticSummaryOutput("snap-mtx", 1)}
	}
	if err := h.mgr.TriggerBackup(context.Background(), "pg-mtx"); err != nil {
		t.Fatalf("first TriggerBackup: %v", err)
	}
	waitUntil(t, 5*time.Second, "job to start", func() bool {
		return len(h.docker.jobsWithPurpose("backup")) == 1
	})
	if err := h.mgr.TriggerBackup(context.Background(), "pg-mtx"); err == nil {
		t.Fatal("concurrent backup must be rejected")
	} else if !strings.Contains(err.Error(), "in flight") {
		t.Errorf("conflict error = %v, want the in-flight sentinel", err)
	}
	close(release)
	waitUntil(t, 5*time.Second, "backup to settle", func() bool {
		rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
		return len(rows) == 1
	})
}

// TestBackupSchedulingWindowAndPrune 调度窗 + 保留对齐（验收 5）：now 落
// hour_utc 窗且台账旧于 interval → daily 触发；restic forget 按本实例路径
// 过滤 keep-last N；台账行镜像删除。
func TestBackupSchedulingWindowAndPrune(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-sched", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	h.saveS3Settings()
	// now = 03:30 UTC（平台缺省窗 hour_utc=3 内）。钉定必须先于台账回拨——
	// stale 从钉定 now 派生而非真实时钟（两者混用 = 日期漂移定时炸弹：真实
	// 今天越过钉定日后，回拨台账距窗内节拍不足 interval → 被判「新鲜」→
	// duty 跳过 → 超时；2026-09-23 实爆，第二颗〔第一颗见 database_test.go
	// 12:30 钉定〕）。
	h.now = time.Date(2026, 9, 21, 3, 30, 0, 0, time.UTC)
	// 旧台账：8 份历史（keep 缺省 7——应被 prune 到 7），created_at 回拨到
	// 3 天前（InsertDatabaseBackup 落当前时刻——台账 freshness 判据要求
	// 「上一份 daily 早于 interval」的历史事实）。
	stale := h.now.Add(-72 * time.Hour).UnixNano()
	for i := 0; i < 8; i++ {
		if _, err := h.st.InsertDatabaseBackup(context.Background(), state.DatabaseBackup{
			DatabaseID:     inst.ID,
			Kind:           state.DatabaseBackupDaily,
			ResticSnapshot: "old-" + string(rune('a'+i)),
			VerifyStatus:   state.DatabaseVerifyVerified,
		}); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}
	if err := h.st.InTx(context.Background(), func(tx *state.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`UPDATE db_backups SET created_at = ? WHERE db_id = ?`, stale, inst.ID)
		return err
	}); err != nil {
		t.Fatalf("backdate ledger: %v", err)
	}
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		script := strings.Join(in.Cmd, " ")
		switch {
		case strings.Contains(script, "forget --keep-last"):
			// forget 必须按本实例路径过滤（共享 repo 下无过滤会波及他实例
			// 与控制面快照——D-DB-6 同 repo 裁决的安全前提）。
			if !strings.Contains(script, "--path db/pg-sched/db.dump") || !strings.Contains(script, "--keep-last 7") {
				t.Errorf("forget script not scoped to the instance: %s", script)
			}
			return JobRunOutcome{State: "complete", ExitCode: 0}
		case strings.Contains(script, "backup --stdin"):
			return JobRunOutcome{State: "complete", Stdout: resticSummaryOutput("snap-daily-9", 4096)}
		default:
			return JobRunOutcome{State: "complete", ExitCode: 0}
		}
	}
	// 窗内节拍（now 已钉定于台账回拨之前，见上）。等待收敛终态 = 新行在册
	// 且 prune 镜像删完（8 旧 + 1 新 → 7）——只等新行落库会把断言提前到
	// prune 异步链未走完的窗口（CI 实爆：9 行 ≠ 7）。
	h.beatRun()
	waitUntil(t, 5*time.Second, "scheduled daily row pruned to keep", func() bool {
		rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 50)
		return len(rows) == 7 && rows[0].ResticSnapshot == "snap-daily-9"
	})
	// 台账镜像：8 旧 + 1 新 = 9 行 → prune 到 keep 7。
	rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 50)
	if len(rows) != 7 {
		t.Errorf("ledger rows after prune = %d, want 7", len(rows))
	}
	// 窗口外不触发（04:30 同拍）：无新行。
	h.now = time.Date(2026, 9, 21, 4, 30, 0, 0, time.UTC)
	h.beatRun()
	time.Sleep(50 * time.Millisecond)
	rows2, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 50)
	if len(rows2) != 7 {
		t.Errorf("rows after out-of-window beat = %d, want 7 (no trigger outside hour_utc)", len(rows2))
	}
}
