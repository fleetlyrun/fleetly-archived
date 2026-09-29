package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestJanitorPrunesExpired 保留期清理：事件与审计按各自保留期清理，
// 窗口内记录不受影响；被清理区段的游标查询显式 410（断档契约联动）。
func TestJanitorPrunesExpired(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// 旧事件（40 天前）+ 新事件（1 天前）；旧审计（400 天前）+ 新审计（10 天前）。
	old := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-24 * time.Hour)
	for _, at := range []time.Time{old, old.Add(time.Second), recent} {
		if err := st.InTx(ctx, func(tx *Tx) error {
			_, err := tx.AppendEvent(ctx, Event{Name: "node.up", At: at})
			return err
		}); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
	for _, at := range []time.Time{now.Add(-400 * 24 * time.Hour), now.Add(-10 * 24 * time.Hour)} {
		if err := st.InTx(ctx, func(tx *Tx) error {
			return tx.WriteAudit(ctx, AuditEntry{Actor: "system", Action: "app.create", Result: "ok", At: at})
		}); err != nil {
			t.Fatalf("write audit: %v", err)
		}
	}

	jr := NewJanitor(st, JanitorConfig{
		EventRetentionDays: DefaultEventRetentionDays,
		AuditRetentionDays: DefaultAuditRetentionDays,
	}, testLogger())
	eventsPruned, auditsPruned, err := jr.PruneOnce(ctx, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if eventsPruned != 2 {
		t.Fatalf("events pruned = %d, want 2 (two rows older than 40 days)", eventsPruned)
	}
	if auditsPruned != 1 {
		t.Fatalf("audits pruned = %d, want 1 (one row older than 400 days)", auditsPruned)
	}

	// 窗口内记录保持：从头游标因断档 410（见下方断言），从清理边界后
	// 的游标（since=2）查询只剩 1 条。
	gotOldest, ok, err := st.OldestSeq(ctx)
	if err != nil || !ok {
		t.Fatalf("oldest seq after prune: %v %v", ok, err)
	}
	evs, err := st.EventsSince(ctx, gotOldest-1, 10)
	if err != nil {
		t.Fatalf("events after prune: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("remaining events = %d, want 1", len(evs))
	}
	audits, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("audits after prune: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("remaining audits = %d, want 1", len(audits))
	}

	// 清理制造了断档：从头游标显式 410（不静默跳号）。
	if _, err := st.EventsSince(ctx, 0, 10); err == nil {
		t.Fatal("cursor 0 after head prune must return E_EVENT_CURSOR_EXPIRED")
	}
}

// TestJanitorRetentionDefaults 非正值保留期回落默认（不允许配置关闭清理）。
func TestJanitorRetentionDefaults(t *testing.T) {
	jr := NewJanitor(nil, JanitorConfig{}, testLogger())
	if jr.EventRetention() != DefaultEventRetentionDays*24*time.Hour {
		t.Fatalf("event retention = %v, want default %d days", jr.EventRetention(), DefaultEventRetentionDays)
	}
	if jr.AuditRetention() != DefaultAuditRetentionDays*24*time.Hour {
		t.Fatalf("audit retention = %v, want default %d days", jr.AuditRetention(), DefaultAuditRetentionDays)
	}
	jr2 := NewJanitor(nil, JanitorConfig{EventRetentionDays: 7, AuditRetentionDays: 90}, testLogger())
	if jr2.EventRetention() != 7*24*time.Hour || jr2.AuditRetention() != 90*24*time.Hour {
		t.Fatalf("configured retentions wrong: %v / %v", jr2.EventRetention(), jr2.AuditRetention())
	}
}

// TestJanitorAuditRetentionPriority W3-S1 D-W0-6 留存回落链三态：
// platform_settings audit.retention_days > config state.audit_retention_days
//（经 JanitorConfig 装配）> 缺省 90。行为锚 = 实际清理轮的删除边界。
func TestJanitorAuditRetentionPriority(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	day := 24 * time.Hour
	seed := func(t *testing.T, st *Store, ages ...time.Duration) {
		t.Helper()
		for i, age := range ages {
			if err := st.InTx(ctx, func(tx *Tx) error {
				return tx.WriteAudit(ctx, AuditEntry{
					Actor: "system", Action: "app.create", Result: "ok",
					ID: fmt.Sprintf("01PRIOTEST%02d0000000000000000", i), At: now.Add(-age),
				})
			}); err != nil {
				t.Fatalf("seed audit %d: %v", i, err)
			}
		}
	}
	remaining := func(t *testing.T, st *Store) int {
		t.Helper()
		// 只数播种的 app.create 行——SaveRetentionDays 自身落的
		// audit.retention_changed 审计行不进计数（它在窗内，恒存活）。
		rows, _, err := st.ListAudits(ctx, AuditQuery{Action: "app.create"})
		if err != nil {
			t.Fatalf("list audits: %v", err)
		}
		return len(rows)
	}

	t.Run("default_90_no_settings_no_config", func(t *testing.T) {
		st := newTestStore(t)
		seed(t, st, 91*day, 89*day) // 91 天行出窗、89 天行在窗内
		jr := NewJanitor(st, JanitorConfig{}, testLogger())
		if _, _, err := jr.PruneOnce(ctx, now); err != nil {
			t.Fatalf("prune: %v", err)
		}
		if got := remaining(t, st); got != 1 {
			t.Fatalf("default 90d window: remaining = %d, want 1 (the 89d row)", got)
		}
	})

	t.Run("platform_settings_overrides_config", func(t *testing.T) {
		st := newTestStore(t)
		seed(t, st, 31*day, 29*day)
		if err := st.SaveRetentionDays(ctx, 30, AuditSaveOptions{Actor: "system"}); err != nil {
			t.Fatalf("save retention: %v", err)
		}
		// config 说 365（构造期回落值），settings=30 必须赢。
		jr := NewJanitor(st, JanitorConfig{AuditRetentionDays: 365}, testLogger())
		if _, _, err := jr.PruneOnce(ctx, now); err != nil {
			t.Fatalf("prune: %v", err)
		}
		if got := remaining(t, st); got != 1 {
			t.Fatalf("settings 30d window: remaining = %d, want 1 (the 29d row)", got)
		}
	})

	t.Run("config_fallback_when_unset", func(t *testing.T) {
		st := newTestStore(t)
		seed(t, st, 366*day, 364*day)
		jr := NewJanitor(st, JanitorConfig{AuditRetentionDays: 365}, testLogger())
		if _, _, err := jr.PruneOnce(ctx, now); err != nil {
			t.Fatalf("prune: %v", err)
		}
		if got := remaining(t, st); got != 1 {
			t.Fatalf("config 365d fallback: remaining = %d, want 1 (the 364d row)", got)
		}
	})

	// 设置保存即生效：运行中改设置，下一拍新窗生效（每拍现读，不缓存长驻
	// ——logs/metrics 设置的 janitor 消费先例同形态）。
	t.Run("setting_change_takes_effect_next_round", func(t *testing.T) {
		st := newTestStore(t)
		seed(t, st, 91*day, 89*day)
		jr := NewJanitor(st, JanitorConfig{}, testLogger())
		if _, _, err := jr.PruneOnce(ctx, now); err != nil {
			t.Fatalf("prune 1: %v", err)
		}
		if got := remaining(t, st); got != 1 {
			t.Fatalf("pre-change: remaining = %d, want 1", got)
		}
		// 收紧到 30 天：上一轮在窗内的 89 天行下一拍出窗。
		if err := st.SaveRetentionDays(ctx, 30, AuditSaveOptions{Actor: "system"}); err != nil {
			t.Fatalf("save retention: %v", err)
		}
		if _, _, err := jr.PruneOnce(ctx, now); err != nil {
			t.Fatalf("prune 2: %v", err)
		}
		if got := remaining(t, st); got != 0 {
			t.Fatalf("post-change: remaining = %d, want 0 (89d row left the new 30d window)", got)
		}
	})
}

// TestJanitorServiceLoop Start/Stop 生命周期：启动先清一拍，Stop 退出。
func TestJanitorServiceLoop(t *testing.T) {
	st := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 先放一条已过期事件。
	if err := st.InTx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(ctx, Event{Name: "node.up", At: time.Now().UTC().Add(-48 * time.Hour)})
		return err
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	jr := NewJanitor(st, JanitorConfig{
		EventRetentionDays: 1, // 1 天保留
		AuditRetentionDays: DefaultAuditRetentionDays,
	}, testLogger())
	if err := jr.Start(ctx); err != nil {
		t.Fatalf("start janitor: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evs, _ := st.EventsSince(ctx, 0, 10)
		if len(evs) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	evs, err := st.EventsSince(ctx, 0, 10)
	if err != nil {
		t.Fatalf("query after janitor: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("janitor must prune expired events on start tick, got %d", len(evs))
	}
	if err := jr.Stop(ctx); err != nil {
		t.Fatalf("stop janitor: %v", err)
	}
}

// ── S18-A7/A10：部署目录清理、分批删除、builds/artifacts 保留窗、非终态
//    超龄扫描 ─────────────────────────────────────────────────────────────

// backdateDeployment 直写 created_at/updated_at（测试夹具：把行龄与终态
// 时刻拨回过去——API 层 updated_at 恒盖当前时刻、created_at 建行即定，
// 均无法经写入通道造旧行）。
func backdateDeployment(t *testing.T, st *Store, id string, at time.Time) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE deployments SET created_at = ?, updated_at = ? WHERE id = ?`,
		at.UnixNano(), at.UnixNano(), id); err != nil {
		t.Fatalf("backdate deployment %s: %v", id, err)
	}
}

// backdateBuild 直写 created_at（同上夹具语义；终态行另有 finished_at
// 锚点，见 backdateBuildFinish）。
func backdateBuild(t *testing.T, st *Store, id string, at time.Time) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE builds SET created_at = ? WHERE id = ?`, at.UnixNano(), id); err != nil {
		t.Fatalf("backdate build %s: %v", id, err)
	}
}

// backdateBuildFinish 直写 finished_at（终态时刻拨回——保留窗锚点）。
func backdateBuildFinish(t *testing.T, st *Store, id string, at time.Time) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE builds SET finished_at = ? WHERE id = ?`, at.UnixNano(), id); err != nil {
		t.Fatalf("backdate build finish %s: %v", id, err)
	}
}

// ptrStatus 是测试侧状态指针便捷形态。
func ptrStatus(s DeploymentStatus) *DeploymentStatus { return &s }

// TestJanitorPrunesBatchedEvents A10①：超一批（500）的过期事件被分批循环
// 删净（modernc SQLite 子查询 LIMIT 形态验证），窗口内记录保留。
func TestJanitorPrunesBatchedEvents(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	// 700 条旧事件 + 1 条新事件：跨过 pruneBatchSize 边界。
	if err := st.InTx(ctx, func(tx *Tx) error {
		for i := 0; i < 700; i++ {
			if _, err := tx.AppendEvent(ctx, Event{Name: "node.up", At: old}); err != nil {
				return err
			}
		}
		_, err := tx.AppendEvent(ctx, Event{Name: "node.up", At: now.Add(-time.Hour)})
		return err
	}); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	jr := NewJanitor(st, JanitorConfig{}, testLogger())
	eventsPruned, _, err := jr.PruneOnce(ctx, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if eventsPruned != 700 {
		t.Fatalf("events pruned = %d, want 700 (batched loop removes all)", eventsPruned)
	}
	oldest, ok, err := st.OldestSeq(ctx)
	if err != nil || !ok {
		t.Fatalf("oldest seq: %v %v", ok, err)
	}
	evs, err := st.EventsSince(ctx, oldest-1, 10)
	if err != nil {
		t.Fatalf("events since oldest: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("remaining events = %d, want 1 (kept within the window)", len(evs))
	}
}

// TestJanitorPrunesDeploymentDirs A7：deployments/<id>/ 目录按部署终态后
// 30 天窗清理——终态且超窗的删；非终态（无论多旧）与窗口内终态不删；
// 无部署行对应的孤儿目录按目录 mtime 同窗回收。
func TestJanitorPrunesDeploymentDirs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)

	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	seed := func() DeployRecord {
		rec, err := st.CreateDeployment(ctx, DeployRecord{AppID: app.ID, AppName: "demo", Kind: "deploy"})
		if err != nil {
			t.Fatalf("create deployment: %v", err)
		}
		return rec
	}
	terminalOld, terminalNew, activeOld := seed(), seed(), seed()
	for _, rec := range []DeployRecord{terminalOld, terminalNew} {
		to := DeployFailed
		if err := st.UpdateDeployment(ctx, rec.ID, DeploymentPatch{Status: &to, PrevStatus: ptrStatus(DeployQueued)}); err != nil {
			t.Fatalf("terminalize %s: %v", rec.ID, err)
		}
	}
	backdateDeployment(t, st, terminalOld.ID, old)
	backdateDeployment(t, st, activeOld.ID, old) // 非终态旧行：不清理

	root := t.TempDir()
	mkdir := func(name string, mtime time.Time) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		file := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(file, []byte("name: demo\n"), 0o600); err != nil {
			t.Fatalf("write compose: %v", err)
		}
		if err := os.Chtimes(file, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", file, err)
		}
		// 目录自身 mtime 同步拨回（孤儿目录判据按目录 mtime）。
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", dir, err)
		}
		return dir
	}
	oldDir := mkdir(terminalOld.ID, old)
	keptTerminal := mkdir(terminalNew.ID, now)
	keptActive := mkdir(activeOld.ID, old)
	orphanOld := mkdir("d_orphan_old", old)
	orphanNew := mkdir("d_orphan_new", now)

	jr := NewJanitor(st, JanitorConfig{DeploymentsRoot: root}, testLogger())
	if _, _, err := jr.PruneOnce(ctx, now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("old terminal deployment dir must be pruned")
	}
	if _, err := os.Stat(orphanOld); !os.IsNotExist(err) {
		t.Fatal("old orphan deployment dir must be pruned")
	}
	for _, kept := range []string{keptTerminal, keptActive, orphanNew} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("dir %s must survive: %v", kept, err)
		}
	}
}

// TestJanitorOrphanProbeErrorSkipsDir M3-1 回归：GetDeployment 的非
// ErrDeploymentNotFound 错误（行存在但扫描失败——库故障/脏数据等真实
// 错误形态）不得走孤儿回收分支——「读取失败」不是「无部署行」的证据，
// 误删会连带删除在役部署的 compose 持久化目录。注入方式：把行的
// substrate_halted 列写为非整数文本（SQLite 动态类型允许），scanDeployment
// 的 int64 扫描即失败。断言该目录原样保留、真孤儿目录仍被回收（对照）。
func TestJanitorOrphanProbeErrorSkipsDir(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)

	app, err := seedAppE(t, st, "probe-app")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	rec, err := st.CreateDeployment(ctx, DeployRecord{AppID: app.ID, AppName: "probe-app", Kind: "deploy"})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	// 行存在但不可扫描：substrate_halted（INTEGER 列）写文本值——
	// GetDeployment 返回 scan 错误（非 ErrDeploymentNotFound）。
	if _, err := st.db.ExecContext(ctx,
		`UPDATE deployments SET substrate_halted = 'corrupted-text' WHERE id = ?`, rec.ID); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}
	if _, err := st.GetDeployment(ctx, rec.ID); err == nil || errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("corrupted row must produce non-NotFound probe error, got %v", err)
	}

	root := t.TempDir()
	mkdir := func(name string, mtime time.Time) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", dir, err)
		}
		return dir
	}
	probeErrDir := mkdir(rec.ID, old)       // 行在但探测失败：不得回收
	orphanDir := mkdir("d_orphan_old", old) // 无行真孤儿：正常回收

	jr := NewJanitor(st, JanitorConfig{DeploymentsRoot: root}, testLogger())
	jr.pruneDeploymentDirs(ctx, now)

	if _, err := os.Stat(probeErrDir); err != nil {
		t.Fatalf("dir with probe error must survive (M3-1): %v", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("true orphan dir must be pruned: %v", err)
	}
}

// TestJanitorPrunesTerminalBuilds A10②：builds 终态行按 90 天窗清理
// （finished_at 锚点）；非终态行不清理（超龄由扫描步告警，不自愈）。
func TestJanitorPrunesTerminalBuilds(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)

	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	seedBuild := func(service string) BuildRecord {
		rec, err := st.CreateBuild(ctx, BuildRecord{AppID: app.ID, Service: service, Driver: DriverRailpack})
		if err != nil {
			t.Fatalf("create build: %v", err)
		}
		return rec
	}
	oldDone, newDone, queuedOld := seedBuild("s-old"), seedBuild("s-new"), seedBuild("s-queued")
	for _, id := range []string{oldDone.ID, newDone.ID} {
		if err := st.ClaimBuild(ctx, id); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		if err := st.FinishBuildFailed(ctx, id, "E_BUILD_FAILED"); err != nil {
			t.Fatalf("fail %s: %v", id, err)
		}
	}
	backdateBuild(t, st, oldDone.ID, old)
	backdateBuildFinish(t, st, oldDone.ID, old)
	backdateBuild(t, st, queuedOld.ID, old)

	jr := NewJanitor(st, JanitorConfig{}, testLogger())
	if _, _, err := jr.PruneOnce(ctx, now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := st.GetBuild(ctx, oldDone.ID); err == nil {
		t.Fatal("old terminal build must be pruned")
	}
	for _, id := range []string{newDone.ID, queuedOld.ID} {
		if _, err := st.GetBuild(ctx, id); err != nil {
			t.Fatalf("build %s must survive: %v", id, err)
		}
	}
}

// TestJanitorPrunesArtifactsDir A10②：build artifacts 目录按 mtime 30 天窗
// 清理；新鲜产物保留。
func TestJanitorPrunesArtifactsDir(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)

	root := t.TempDir()
	oldFile := filepath.Join(root, "plan-old.json")
	newFile := filepath.Join(root, "plan-new.json")
	for _, f := range []struct {
		path  string
		mtime time.Time
	}{
		{oldFile, old}, {newFile, now},
	} {
		if err := os.WriteFile(f.path, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f.path, err)
		}
		if err := os.Chtimes(f.path, f.mtime, f.mtime); err != nil {
			t.Fatalf("chtimes %s: %v", f.path, err)
		}
	}
	jr := NewJanitor(st, JanitorConfig{ArtifactsDir: root}, testLogger())
	if _, _, err := jr.PruneOnce(ctx, now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("old artifact must be pruned")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("fresh artifact must survive: %v", err)
	}
}

// TestJanitorStaleNonTerminalScan A10③（§9 并入）：非终态超龄行 → 事件
// engine.stale_nonterminal / build.stale_nonterminal + 只告警不自愈（行
// 保持原状）；正常行不触发；同生命周期重扫不重复告警（进程内记忆）。
func TestJanitorStaleNonTerminalScan(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	staleDep, err := st.CreateDeployment(ctx, DeployRecord{AppID: app.ID, AppName: "demo", Kind: "deploy"})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	freshDep, err := st.CreateDeployment(ctx, DeployRecord{AppID: app.ID, AppName: "demo", Kind: "deploy"})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	staleBuild, err := st.CreateBuild(ctx, BuildRecord{AppID: app.ID, Service: "web", Driver: DriverRailpack})
	if err != nil {
		t.Fatalf("create build: %v", err)
	}
	// 锚点拨回：超 2×预算（预算 1h，锚点拨回 3h 前）；fresh 行锚点不动。
	backdateDeployment(t, st, staleDep.ID, now.Add(-3*time.Hour))
	backdateBuild(t, st, staleBuild.ID, now.Add(-3*time.Hour))

	jr := NewJanitor(st, JanitorConfig{
		StaleDeploymentBudget: time.Hour,
		StaleBuildBudget:      time.Hour,
	}, testLogger())
	if _, _, err := jr.PruneOnce(ctx, now); err != nil {
		t.Fatalf("prune: %v", err)
	}

	count := map[string]int{}
	events, err := st.EventsSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		count[ev.Name+"|"+ev.Subject]++
		if ev.Subject == "deployment:"+freshDep.ID {
			t.Fatalf("fresh deployment must not be reported stale: %+v", ev)
		}
	}
	if count["engine.stale_nonterminal|deployment:"+staleDep.ID] != 1 {
		t.Fatalf("stale deployment event missing/duplicated: %v", count)
	}
	if count["build.stale_nonterminal|build:"+staleBuild.ID] != 1 {
		t.Fatalf("stale build event missing/duplicated: %v", count)
	}
	// 只告警不自愈：超龄行保持非终态原状。
	row, err := st.GetDeployment(ctx, staleDep.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if row.Status != DeployQueued {
		t.Fatalf("stale deployment must stay untouched (only alert), got %s", row.Status)
	}
	// 同生命周期内重扫：不重复告警。
	if _, _, err := jr.PruneOnce(ctx, now); err != nil {
		t.Fatalf("prune again: %v", err)
	}
	events, err = st.EventsSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		if ev.Name == "engine.stale_nonterminal" && ev.Subject == "deployment:"+staleDep.ID {
			if count["engine.stale_nonterminal|deployment:"+staleDep.ID] != 1 {
				t.Fatalf("stale deployment alerted more than once")
			}
		}
	}
}
