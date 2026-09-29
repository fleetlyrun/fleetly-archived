package database

// 原地恢复编排的单测（S5 验收 3）：job 载荷断言（rw 数据卷挂载 + 钉绑定
// 节点 + 单 job 取回/重放命令词表）、scale 0 → job → scale 1 全链、快照归
// 属守卫、中途失败 = 实例保持停止 + last_error + db.restore_failed
// + E_DB_RESTORE_FAILED 信封（实例主状态不变的口径断言）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// seedReadyWithLedger 装配 ready 实例 + 一行 verified 备份（恢复链的最小
// 前置），返回实例与快照 id。
func seedReadyWithLedger(t *testing.T, h *harness, name, template, snapshot string) state.DatabaseInstance {
	t.Helper()
	inst := h.createInstance(name, template)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	if _, err := h.st.InsertDatabaseBackup(context.Background(), state.DatabaseBackup{
		DatabaseID:     inst.ID,
		Kind:           state.DatabaseBackupManual,
		ResticSnapshot: snapshot,
		VerifyStatus:   state.DatabaseVerifyVerified,
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	return inst
}

// TestRestoreFlowScaleZeroToJobToResume 恢复全链（验收 3）：快照归属守卫
// 通过 → 副本 0 → 单恢复 job（dbtools：restic 取回落卷 + 临时 postgres
// 重放同 job——v0.2.1-dbtools.1 起基底与引擎同源 glibc）→ 副本 1 →
// restore_completed 事件；实例主状态全程 ready（操作不换主状态，§2.1）。
func TestRestoreFlowScaleZeroToJobToResume(t *testing.T) {
	h := newHarness(t)
	inst := seedReadyWithLedger(t, h, "pg-rs", dbtemplate.TemplatePostgres16, "snap-restore-1")
	h.saveS3Settings()
	var restoreJob *JobRunInput
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		if strings.Contains(strings.Join(in.Cmd, " "), "pg_restore") {
			j := in
			restoreJob = &j
		}
		return JobRunOutcome{State: "complete", ExitCode: 0}
	}

	if err := h.mgr.RestoreBackup(context.Background(), "pg-rs", "snap-restore-1"); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	waitUntil(t, 5*time.Second, "restore_completed event", func() bool {
		events, _ := h.st.EventsSince(context.Background(), 0, 200)
		for _, ev := range events {
			if ev.Name == "db.restore_completed" && strings.Contains(ev.Payload, "snap-restore-1") {
				return true
			}
		}
		return false
	})

	// 单 job 载荷：镜像 = dbtools（debian/glibc 基底，与引擎同源——不再
	// 需要引擎镜像承载重放，双 job 形态已回退）。
	if restoreJob == nil {
		t.Fatal("no restore job ran")
	}
	if restoreJob.Image != DefaultDatabaseToolsImage {
		t.Errorf("restore image = %s, want the dbtools image (single-job restore)", restoreJob.Image)
	}
	m := restoreJob.Mounts[0]
	volWant := "fleetly-db-pg-rs-data-" + inst.ID[:8]
	if m.VolumeName != volWant || m.Target != "/var/lib/postgresql/data" || m.ReadOnly {
		t.Errorf("restore mount = %+v, want %s rw at /var/lib/postgresql/data", m, volWant)
	}
	if len(restoreJob.Constraints) != 1 || restoreJob.Constraints[0] != "node.labels.fleetly.node-id == n_node" {
		t.Errorf("restore constraints = %v, want the platform node-label pin (swarm node.id never equals the platform id; remote local volume is unreadable via manager)", restoreJob.Constraints)
	}
	// 词表：restic dump 落卷根暂存文件 + temp postgres + drop/create +
	// pg_restore 取卷上 dump + 同 uid pg_ctl 停服 + 暂存文件清场；命令零
	// 凭据（PG 走 socket trust）。
	script := strings.Join(restoreJob.Cmd, " ")
	for _, want := range []string{
		"dump snap-restore-1 db/pg-rs/db.dump > /var/lib/postgresql/data/fleetly-replay.dump",
		`PGUID=$(stat -c %u "$PGDATA")`,
		`DROP DATABASE IF EXISTS "pg_rs"`,
		`CREATE DATABASE "pg_rs"`,
		`pg_restore -h /var/run/postgresql -U fleetly -d "pg_rs" --no-owner /var/lib/postgresql/data/fleetly-replay.dump`,
		`pg_ctl -D "$PGDATA" -m fast stop`,
		`rm -f /var/lib/postgresql/data/fleetly-replay.dump`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("restore script missing %q: %s", want, script)
		}
	}
	// scale 轨迹：副本 0（重放窗口）→ 副本 1（重部署）。
	svc := h.docker.services[h.svcName(inst)]
	if svc.Replicas != 1 {
		t.Errorf("replicas after restore = %d, want 1 (instance redeployed)", svc.Replicas)
	}
	// 主状态不变（操作事件承载，不经 EnterDbPhase）。
	if got := h.get(inst.ID); got.State != state.DatabaseReady {
		t.Errorf("state = %s, want ready (restore is an operation, not a phase)", got.State)
	}
}

// TestRestoreRefusesForeignSnapshot 快照归属守卫：他实例/未知快照拒绝（数
// 据安全前哨——跨实例误指在受理期拦下）。
func TestRestoreRefusesForeignSnapshot(t *testing.T) {
	h := newHarness(t)
	inst := seedReadyWithLedger(t, h, "pg-own", dbtemplate.TemplatePostgres16, "snap-own")
	other := h.createInstance("pg-other", dbtemplate.TemplatePostgres16)
	if _, err := h.st.InsertDatabaseBackup(context.Background(), state.DatabaseBackup{
		DatabaseID:     other.ID,
		Kind:           state.DatabaseBackupDaily,
		ResticSnapshot: "snap-foreign",
		VerifyStatus:   state.DatabaseVerifyVerified,
	}); err != nil {
		t.Fatalf("seed foreign ledger: %v", err)
	}
	err := h.mgr.RestoreBackup(context.Background(), "pg-own", "snap-foreign")
	if err == nil || !strings.Contains(err.Error(), "not in the ledger") {
		t.Fatalf("foreign snapshot err = %v, want the ledger-ownership refusal", err)
	}
	if jobs := h.docker.jobsWithPurpose("restore"); len(jobs) != 0 {
		t.Errorf("restore jobs = %d, want 0 (refusal precedes any job)", len(jobs))
	}
	_ = inst
}

// TestRestoreFailureLeavesStoppedHonest 中途失败（验收 3 负面）：重放 job
// 失败 → 实例保持停止（副本 0 不盲目重启）+ last_error 阶段上下文 +
// db.restore_failed 事件 + E_DB_RESTORE_FAILED 信封（runbook 指引随文本）
// ——主状态保持 ready（操作不换主状态；设计「原地恢复中断即 critical 告
// 警」口径）。
func TestRestoreFailureLeavesStoppedHonest(t *testing.T) {
	h := newHarness(t)
	inst := seedReadyWithLedger(t, h, "pg-rfail", dbtemplate.TemplatePostgres16, "snap-bad")
	h.saveS3Settings()
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		if strings.Contains(strings.Join(in.Cmd, " "), "pg_restore") {
			return JobRunOutcome{State: "failed", Err: "pg_restore: input file appears to be an invalid archive", ExitCode: 1}
		}
		return JobRunOutcome{State: "complete", ExitCode: 0}
	}
	if err := h.mgr.RestoreBackup(context.Background(), "pg-rfail", "snap-bad"); err != nil {
		t.Fatalf("RestoreBackup accept: %v", err)
	}
	waitUntil(t, 5*time.Second, "restore_failed event", func() bool {
		events, _ := h.st.EventsSince(context.Background(), 0, 200)
		for _, ev := range events {
			if ev.Name == "db.restore_failed" && strings.Contains(ev.Payload, "replay") {
				return true
			}
		}
		return false
	})
	// 实例保持停止 + 诚实现场。
	svc := h.docker.services[h.svcName(inst)]
	if svc.Replicas != 0 {
		t.Errorf("replicas = %d, want 0 (failed restore leaves the instance stopped for manual inspection)", svc.Replicas)
	}
	got := h.get(inst.ID)
	if got.State != state.DatabaseReady {
		t.Errorf("state = %s, want ready (operation never changes the main state)", got.State)
	}
	if !strings.Contains(got.LastError, "stage replay") {
		t.Errorf("last_error = %q, want the stage context", got.LastError)
	}
	if !strings.Contains(got.LastError, "resume") {
		t.Errorf("last_error = %q, want the manual recovery runbook pointer", got.LastError)
	}
	// 事件/错误文本零凭据（明文纪律——恢复链的负面扫描面）。
	plain, derr := h.box.Decrypt([]byte(inst.CredentialCipher))
	if derr != nil {
		t.Fatalf("decrypt: %v", derr)
	}
	events, _ := h.st.EventsSince(context.Background(), 0, 200)
	for _, ev := range events {
		if strings.Contains(ev.Payload, string(plain)) {
			t.Fatalf("credential plaintext leaked into event %s", ev.Name)
		}
	}
	if strings.Contains(got.LastError, string(plain)) {
		t.Error("credential plaintext leaked into last_error")
	}
}

// TestRestorePausedRefused paused 前置态拒绝（§2.3 操作表：恢复 ready/
// degraded——paused 实例无需恢复（数据未在役变化），provisioning 在途）。
func TestRestorePausedRefused(t *testing.T) {
	h := newHarness(t)
	inst := seedReadyWithLedger(t, h, "pg-rp", dbtemplate.TemplatePostgres16, "snap-p")
	if err := h.st.EnterDbPhase(context.Background(), inst.ID, state.DatabaseReady, state.DatabasePaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	err := h.mgr.RestoreBackup(context.Background(), "pg-rp", "snap-p")
	if err == nil || !strings.Contains(err.Error(), "requires state ready, degraded") {
		t.Fatalf("err = %v, want the prestate refusal", err)
	}
}
