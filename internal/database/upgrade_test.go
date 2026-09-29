package database

// 受控升级编排的单测（S5 验收 4/7）：成功路径（备份门 → digest 换新受控
// 重建 → 健康门 → upgrade_finished + 可升级位归零）、失败路径（新版本健
// 康门失败 → digest 归位 + 状态落 degraded + upgrade_failed + 系统审计）、
// paused spec-only（不触任务重建，resume 时以新版本重建）、备份门失败中止
// 不动、S3 未配置的诚实路径。
//
// 异步编排的收口等待用 waitUntil；健康门观察窗经 mgr.upgradeWatch 注入短
// 窗（构造点 100ms——超时路径不能等真实预算）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/dockerapi"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// upgradeSeed 装配 ready 实例 + 备份材料 + 假 job 结论（备份门通过），返回
// 实例与新镜像引用。可升级事实的构造：先收敛到 ready（模板镜像任务健康），
// 再把实例 digest 回写过期值（§2.2——平台 release 携带新 digest，存量实例
// 不自动变；实例列与模板注册表解耦，注册表只增纪律不动）。
func upgradeSeed(t *testing.T, h *harness, name string) (state.DatabaseInstance, string) {
	t.Helper()
	inst := h.createInstance(name, dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dockerapi.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseReady {
		t.Fatalf("seed: state = %s, want ready", got.State)
	}
	h.saveS3Settings()
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	if err := h.st.UpdateImageDigest(context.Background(), inst.ID, "postgres:16@sha256:000000000000000000000000000000000000000000000000000000000000old0"); err != nil {
		t.Fatalf("seed stale digest: %v", err)
	}
	h.beatRun() // 过期 digest 收敛（ready 观察拍：spec 更新 + 公告步落位）
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		script := strings.Join(in.Cmd, " ")
		switch {
		case strings.Contains(script, "backup --stdin"):
			return JobRunOutcome{State: "complete", Stdout: resticSummaryOutput("snap-upgrade-1", 4096)}
		default:
			return JobRunOutcome{State: "complete", ExitCode: 0} // verify/prune
		}
	}
	return inst, tpl.Image
}

// eventSeen 报告事件流中是否存在指定事件（payload 含 needle）。
func eventSeen(t *testing.T, h *harness, name, needle string) bool {
	t.Helper()
	events, err := h.st.EventsSince(context.Background(), 0, 300)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		if ev.Name == name && strings.Contains(ev.Payload, needle) {
			return true
		}
	}
	return false
}

// TestUpgradeSuccessPath 升级成功（验收 7 正面）：备份门（pre_upgrade verified
// 行）→ digest 换新 + 服务重建 → 健康门通过 → db.upgrade_started/finished
// 事件；实例 digest = 模板当前（可升级位归零）。
func TestUpgradeSuccessPath(t *testing.T) {
	h := newHarness(t)
	inst, newImage := upgradeSeed(t, h, "pg-up")
	// 新版本任务健康（升级 applyDesiredService 后健康门首拍即绿）。
	h.setTasks(h.svcName(inst), dockerapi.TaskObservation{State: "running", DesiredState: "running", Image: newImage})

	oldDigest, newDigest, err := h.mgr.Upgrade(context.Background(), "pg-up")
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if !strings.HasPrefix(oldDigest, "postgres:16@sha256:0000") || newDigest != newImage {
		t.Fatalf("digests = (%s, %s), want (seeded-old, template-current)", oldDigest, newDigest)
	}
	waitUntil(t, 5*time.Second, "upgrade_finished event", func() bool {
		return eventSeen(t, h, "db.upgrade_finished", "pg-up")
	})
	got := h.get(inst.ID)
	if got.ImageDigest != newImage {
		t.Errorf("digest = %s, want the template current image", got.ImageDigest)
	}
	if got.State != state.DatabaseReady {
		t.Errorf("state = %s, want ready (upgrade is an operation)", got.State)
	}
	if !eventSeen(t, h, "db.upgrade_started", "pg-up") {
		t.Error("db.upgrade_started event missing")
	}
	// 备份门产物：pre_upgrade 行 verified。
	rows, _ := h.st.ListDatabaseBackups(context.Background(), inst.ID, 10)
	foundPre := false
	for _, r := range rows {
		if r.Kind == state.DatabaseBackupPreUpgrade && r.VerifyStatus == state.DatabaseVerifyVerified {
			foundPre = true
		}
	}
	if !foundPre {
		t.Errorf("no verified pre_upgrade row in %+v (backup gate must run first)", rows)
	}
	// 服务已按新 digest 重建（desired-hash 更新 + 新镜像任务在位）。
	svc := h.docker.services[h.svcName(inst)]
	if !svc.Exists || svc.Replicas != 1 {
		t.Errorf("service = %+v, want rebuilt with replicas 1", svc)
	}
}

// TestUpgradeFailureRollbackDigest 失败路径（验收 7 负面——设计 §2.2「失败
// = digest 归位 + db.upgrade_failed + 状态落 degraded」）：新版本健康门失
// 败 → digest 回写旧值 + 重建 + degraded + 系统审计。
func TestUpgradeFailureRollbackDigest(t *testing.T) {
	h := newHarness(t)
	inst, _ := upgradeSeed(t, h, "pg-rollback")
	oldDigest := h.get(inst.ID).ImageDigest
	// 新版本任务硬失败（健康门判败证据）。
	tpl, _ := dbtemplate.Get(inst.Template)
	h.setTasks(h.svcName(inst), dockerapi.TaskObservation{
		State: "failed", DesiredState: "running",
		Err:   "new image task failed the health gate (simulated bad digest)",
		Image: tpl.Image,
	})

	if _, _, err := h.mgr.Upgrade(context.Background(), "pg-rollback"); err != nil {
		t.Fatalf("Upgrade accept: %v", err)
	}
	waitUntil(t, 5*time.Second, "upgrade_failed event", func() bool {
		return eventSeen(t, h, "db.upgrade_failed", "pg-rollback")
	})
	got := h.get(inst.ID)
	// digest 归位（回写旧值重建——不是 revision 重放）。
	if got.ImageDigest != oldDigest {
		t.Errorf("digest = %s, want rolled back to %s", got.ImageDigest, oldDigest)
	}
	// 状态落 degraded + 现场落 last_error。
	if got.State != state.DatabaseDegraded {
		t.Errorf("state = %s, want degraded (design §2.2 failure terminal)", got.State)
	}
	if !strings.Contains(got.LastError, "health_gate") {
		t.Errorf("last_error = %q, want the stage context", got.LastError)
	}
	// 归位重建已推送（服务更新记录 + 旧 digest 任务健康后由观察路径收敛回
	// ready——这里只断言编排期的归位写）。
	if len(h.docker.updated) == 0 {
		t.Error("rollback rebuild must push the old spec (ensureService update)")
	}
	// 系统审计（digest 归位 = 自动动作必入审计）。
	audits, err := h.st.RecentAudits(context.Background(), 50)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "db.upgrade" && strings.Contains(a.DiffSummary, "rolled_back") {
			found = true
		}
	}
	if !found {
		t.Error("digest rollback must land a system db.upgrade audit entry")
	}
}

// TestUpgradePausedSpecOnly paused 升级 = 仅换 spec 不重启（验收 7——
// §2.3「paused 下仅换 spec 不重启，收敛推迟到 resume」；备份门降格为
// verified 备份存在性——停摆引擎跑不了 pre_upgrade，裁决边界见 upgrade.go）。
func TestUpgradePausedSpecOnly(t *testing.T) {
	h := newHarness(t)
	inst, newImage := upgradeSeed(t, h, "pg-pausedup")
	// 门材料：台账内一行 verified 备份。
	if _, err := h.st.InsertDatabaseBackup(context.Background(), state.DatabaseBackup{
		DatabaseID:     inst.ID,
		Kind:           state.DatabaseBackupDaily,
		ResticSnapshot: "snap-verified",
		VerifyStatus:   state.DatabaseVerifyVerified,
	}); err != nil {
		t.Fatalf("seed verified backup: %v", err)
	}
	// 暂停（scale 0 由收敛拍保持）。
	if err := h.st.EnterDbPhase(context.Background(), inst.ID, state.DatabaseReady, state.DatabasePaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.beatRun()
	before := len(h.docker.updated)

	if _, _, err := h.mgr.Upgrade(context.Background(), "pg-pausedup"); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	waitUntil(t, 5*time.Second, "paused upgrade finished", func() bool {
		return eventSeen(t, h, "db.upgrade_finished", "paused instance")
	})
	got := h.get(inst.ID)
	if got.ImageDigest != newImage {
		t.Errorf("digest = %s, want the template current (spec-only upgrade)", got.ImageDigest)
	}
	if got.State != state.DatabasePaused {
		t.Errorf("state = %s, want paused (no restart until resume)", got.State)
	}
	// spec 更新推送了（paused 投影 = 新 digest + 副本 0），无任务侧重建。
	if len(h.docker.updated) <= before {
		t.Error("paused upgrade must update the service spec (new digest at replicas 0)")
	}
	svc := h.docker.services[h.svcName(inst)]
	if svc.Replicas != 0 {
		t.Errorf("replicas = %d, want 0 (paused spec-only)", svc.Replicas)
	}
}

// TestUpgradePausedGateRefusesUnverified paused 门禁负面：无任何 verified
// 备份 → 如实拒绝（假门禁零容忍——裁决边界的拒绝侧）。
func TestUpgradePausedGateRefusesUnverified(t *testing.T) {
	h := newHarness(t)
	inst, _ := upgradeSeed(t, h, "pg-pgate")
	digestBefore := h.get(inst.ID).ImageDigest
	if err := h.st.EnterDbPhase(context.Background(), inst.ID, state.DatabaseReady, state.DatabasePaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.beatRun()
	if _, _, err := h.mgr.Upgrade(context.Background(), "pg-pgate"); err != nil {
		t.Fatalf("Upgrade accept: %v", err)
	}
	waitUntil(t, 5*time.Second, "paused gate refusal", func() bool {
		return eventSeen(t, h, "db.upgrade_failed", "backup_gate")
	})
	got := h.get(inst.ID)
	if got.ImageDigest != digestBefore {
		t.Errorf("digest = %s, want untouched (gate aborts before any write)", got.ImageDigest)
	}
	if got.State != state.DatabasePaused {
		t.Errorf("state = %s, want paused (instance untouched)", got.State)
	}
}

// TestUpgradeBackupGateAbortsOnBackupFailure 备份门失败中止（验收 7——设
// 计 §2.2「失败 → E_DB_BACKUP_FAILED 中止，实例不动」）：备份 job 失败 →
// digest 不变 + db.backup_failed 与 db.upgrade_failed 双披露 + 服务零写。
func TestUpgradeBackupGateAbortsOnBackupFailure(t *testing.T) {
	h := newHarness(t)
	inst, _ := upgradeSeed(t, h, "pg-gatefail")
	oldDigest := h.get(inst.ID).ImageDigest
	before := len(h.docker.updated)
	h.docker.jobOutFn = func(in JobRunInput) JobRunOutcome {
		if strings.Contains(strings.Join(in.Cmd, " "), "backup --stdin") {
			return JobRunOutcome{State: "failed", Err: "pg_dump: connection refused (simulated gate failure)", ExitCode: 1}
		}
		return JobRunOutcome{State: "complete", ExitCode: 0}
	}

	if _, _, err := h.mgr.Upgrade(context.Background(), "pg-gatefail"); err != nil {
		t.Fatalf("Upgrade accept: %v", err)
	}
	waitUntil(t, 5*time.Second, "gate abort", func() bool {
		return eventSeen(t, h, "db.upgrade_failed", "backup_gate")
	})
	got := h.get(inst.ID)
	if got.ImageDigest != oldDigest {
		t.Errorf("digest = %s, want untouched (backup gate failure aborts the upgrade)", got.ImageDigest)
	}
	if got.State != state.DatabaseReady {
		t.Errorf("state = %s, want ready (instance untouched)", got.State)
	}
	if len(h.docker.updated) != before {
		t.Errorf("service updates = %d (before %d), want none (abort leaves the scene)", len(h.docker.updated), before)
	}
	if !eventSeen(t, h, "db.backup_failed", "pg-gatefail") {
		t.Error("db.backup_failed event missing (the backup failure must be disclosed on its own face)")
	}
}

// TestUpgradeS3UnsetHonest S3 未配置 → 备份门诚实失败（验收 6 的升级面：
// unset 是合法态但升级如实拒绝）。
func TestUpgradeS3UnsetHonest(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-ups3", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dockerapi.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseReady {
		t.Fatalf("seed: state = %s, want ready", got.State)
	}
	if err := h.st.UpdateImageDigest(context.Background(), inst.ID, "postgres:16@sha256:1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
		t.Fatalf("seed stale digest: %v", err)
	}
	h.beatRun()

	if _, _, err := h.mgr.Upgrade(context.Background(), "pg-ups3"); err != nil {
		t.Fatalf("Upgrade accept: %v", err)
	}
	waitUntil(t, 5*time.Second, "s3-unset gate abort", func() bool {
		return eventSeen(t, h, "db.upgrade_failed", "backup_gate")
	})
	if got := h.get(inst.ID); got.State != state.DatabaseReady {
		t.Errorf("state = %s, want ready (instance untouched)", got.State)
	}
	if !eventSeen(t, h, "db.backup_failed", "pg-ups3") {
		t.Error("the s3-unset backup failure must surface on db.backup_failed")
	}
}

// TestUpgradeAvailableAdvertisement 可升级公告步：实例 digest 落后模板 →
// db.upgrade_available 一次（per 目标 digest 去重）；升级完成后同拍不再公
// 告。
func TestUpgradeAvailableAdvertisement(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-adv", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dockerapi.TaskObservation{State: "running", DesiredState: "running", Image: inst.ImageDigest})
	// 实例 digest 置为过期值（模拟平台 release 携带新 digest 后的存量实例）。
	if err := h.st.UpdateImageDigest(context.Background(), inst.ID, "postgres:16@sha256:2222222222222222222222222222222222222222222222222222222222222222"); err != nil {
		t.Fatalf("seed stale digest: %v", err)
	}
	h.beatRun()
	if !eventSeen(t, h, "db.upgrade_available", "pg-adv") {
		t.Fatal("db.upgrade_available event missing (stale instance after a template digest move)")
	}
	h.beatRun()
	count := 0
	events, _ := h.st.EventsSince(context.Background(), 0, 300)
	for _, ev := range events {
		if ev.Name == "db.upgrade_available" && strings.Contains(ev.Payload, "pg-adv") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("db.upgrade_available emitted %d times, want 1 (per-target dedup)", count)
	}
}
