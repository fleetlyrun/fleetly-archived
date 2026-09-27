package state

// 项目网参与位读写测试（T 线 OT-1 / IMPL-T15-1）：置位/清位幂等（零重复
// 审计/事件）、审计与事件同事务形态（change=attached|detached）、成员计数
// 口径（仅 lifecycle='active' 且参与位在位）、app 删除清位（tombstone 第二拍
// 不悬挂成员计数）、迁移 00025 Up/Down 演练。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// countAuditsByAction 统计至今某审计动作的行数（幂等断言面）。
func countAuditsByAction(t *testing.T, st *Store, action string) int {
	t.Helper()
	rows, err := st.RecentAudits(context.Background(), 200)
	if err != nil {
		t.Fatalf("recent audits: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Action == action {
			n++
		}
	}
	return n
}

// countEventsNamed 统计至今某事件名的落库条数。
func countEventsNamed(t *testing.T, st *Store, name string) int {
	t.Helper()
	rows, err := st.EventsSince(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	n := 0
	for _, e := range rows {
		if e.Name == name {
			n++
		}
	}
	return n
}

// TestAppProjectNetworkAttachedLifecycle 置位/清位生命周期：变更即审计 +
// 事件（Outbox 同事务）；目标值与现值相同 = 零变更零审计零事件（重跑安全）。
func TestAppProjectNetworkAttachedLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app := seedApp(t, st, "netapp")
	if app.ProjectNetworkAttached {
		t.Fatal("precondition: a fresh app must not participate in any project network (OT-1 default off)")
	}

	updated, changed, err := st.SetAppProjectNetworkAttached(ctx, app.ID, true, "user-1", "")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !changed || !updated.ProjectNetworkAttached {
		t.Fatalf("attach result = changed=%v attached=%v, want true/true", changed, updated.ProjectNetworkAttached)
	}
	if countAuditsByAction(t, st, "app.project_network_attached") != 1 {
		t.Fatal("attach audit row missing")
	}
	if got := countEventsNamed(t, st, "project.network_changed"); got != 1 {
		t.Fatalf("project.network_changed events = %d, want 1", got)
	}

	// 幂等：目标值已是现值 → 零变更、零新审计/事件（changed=false 供调用
	// 面跳过重复披露；后置编排的重跑语义在 API 层）。
	if _, changed, err := st.SetAppProjectNetworkAttached(ctx, app.ID, true, "user-1", ""); err != nil || changed {
		t.Fatalf("re-attach = changed=%v err=%v, want false/nil (idempotent write)", changed, err)
	}
	if countAuditsByAction(t, st, "app.project_network_attached") != 1 {
		t.Fatal("idempotent re-attach duplicated the audit row")
	}
	if got := countEventsNamed(t, st, "project.network_changed"); got != 1 {
		t.Fatalf("idempotent re-attach duplicated events: %d", got)
	}

	// 清位（detach）：审计动作分记、事件 change=detached。
	detached, changed, err := st.SetAppProjectNetworkAttached(ctx, app.ID, false, "user-2", "tok-9")
	if err != nil || !changed || detached.ProjectNetworkAttached {
		t.Fatalf("detach = %+v changed=%v err=%v, want detached/true/nil", detached, changed, err)
	}
	if countAuditsByAction(t, st, "app.project_network_detached") != 1 {
		t.Fatal("detach audit row missing")
	}
	if got := countEventsNamed(t, st, "project.network_changed"); got != 2 {
		t.Fatalf("project.network_changed events = %d, want 2 after detach", got)
	}
	if _, changed, err := st.SetAppProjectNetworkAttached(ctx, app.ID, false, "user-2", ""); err != nil || changed {
		t.Fatalf("re-detach = changed=%v err=%v, want false/nil", changed, err)
	}

	// 读面随行（App 行投影携带参与位）。
	got, err := st.GetAppByID(ctx, app.ID)
	if err != nil || got.ProjectNetworkAttached {
		t.Fatalf("readback attached = %v err=%v, want false", got.ProjectNetworkAttached, err)
	}

	// 不存在 app：显式哨兵（不静默成功）。
	if _, _, err := st.SetAppProjectNetworkAttached(ctx, "01J00000000000000000000MISS", true, "u", ""); err != ErrAppNotFound {
		t.Fatalf("missing app err = %v, want ErrAppNotFound", err)
	}
}

// TestProjectNetworkMemberCounts 成员计数口径：仅 lifecycle='active' 且参与位
// 在位的 app 计入；deleted tombstone 清位（MarkAppDeleted 同事务）后不再
// 计数——项目网 GC 与 Console 投影的同一数据源。
func TestProjectNetworkMemberCounts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	projA := seedFixtureProject(t, st)
	projB := seedFixtureProject(t, st)
	appA1, err := st.CreateApp(ctx, "", "alpha", projA.ID, projA.TeamID)
	if err != nil {
		t.Fatalf("create appA1: %v", err)
	}
	appA2, err := st.CreateApp(ctx, "", "beta", projA.ID, projA.TeamID)
	if err != nil {
		t.Fatalf("create appA2: %v", err)
	}
	appA3, err := st.CreateApp(ctx, "", "gamma", projA.ID, projA.TeamID)
	if err != nil {
		t.Fatalf("create appA3: %v", err)
	}
	appB1, err := st.CreateApp(ctx, "", "delta", projB.ID, projB.TeamID)
	if err != nil {
		t.Fatalf("create appB1: %v", err)
	}
	for _, id := range []string{appA1.ID, appA2.ID, appB1.ID} {
		if _, _, err := st.SetAppProjectNetworkAttached(ctx, id, true, "u", ""); err != nil {
			t.Fatalf("attach %s: %v", id, err)
		}
	}

	counts, err := st.ProjectNetworkMemberCounts(ctx)
	if err != nil {
		t.Fatalf("member counts: %v", err)
	}
	if len(counts) != 2 || counts[projA.ID] != 2 || counts[projB.ID] != 1 {
		t.Fatalf("counts = %v, want {%s:2, %s:1} (detached appA3 excluded)", counts, projA.ID, projB.ID)
	}
	if _, ok := counts[appA3.ProjectID]; ok && counts[appA3.ProjectID] != 2 {
		t.Fatalf("detached app leaked into counts: %v", counts)
	}

	// 字典序稳定（事件/日志可重放的期望面）。
	members, err := st.ProjectNetworkMembers(ctx)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 2 || members[0].ProjectID > members[1].ProjectID {
		t.Fatalf("members = %+v, want id-ordered two rows", members)
	}

	// app 删除（tombstone 两拍）：参与位随 MarkAppDeleted 清 0，成员计数下降。
	if err := st.MarkAppDeleting(ctx, appA1.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	counts, err = st.ProjectNetworkMemberCounts(ctx)
	if err != nil {
		t.Fatalf("counts after deleting: %v", err)
	}
	if counts[projA.ID] != 1 {
		t.Fatalf("counts while deleting = %v, want projA:1 (deleting apps are not members)", counts)
	}
	err = st.InTx(ctx, func(tx *Tx) error { return tx.MarkAppDeleted(ctx, appA1.ID) })
	if err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	row, err := st.GetAppByID(ctx, appA1.ID)
	if err != nil {
		t.Fatalf("read deleted app: %v", err)
	}
	if row.ProjectNetworkAttached {
		t.Fatal("MarkAppDeleted must clear the project-network participation flag (no dangling membership)")
	}
}

// TestProjectDeleteGuardWithAttachmentCross 项目删除非空守卫与项目网交叉
// （守卫③既有语义不回退）：attach 不改变「有存活 app 即拒」；app 走完
// tombstone 两拍后项目可删（参与位已随 MarkAppDeleted 清 0，无悬挂成员）。
func TestProjectDeleteGuardWithAttachmentCross(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	proj := seedFixtureProject(t, st)
	app, err := st.CreateApp(ctx, "", "guarded", proj.ID, proj.TeamID)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, _, err := st.SetAppProjectNetworkAttached(ctx, app.ID, true, "u", ""); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := st.DeleteProject(ctx, proj.ID, "u", ""); err == nil {
		t.Fatal("project with a live attached app must refuse deletion (existing guard must not regress)")
	} else if !errors.Is(err, ErrProjectNotEmpty) {
		t.Fatalf("delete err = %v, want ErrProjectNotEmpty", err)
	}
	if err := st.MarkAppDeleting(ctx, app.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	if err := st.InTx(ctx, func(tx *Tx) error { return tx.MarkAppDeleted(ctx, app.ID) }); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	if err := st.DeleteProject(ctx, proj.ID, "u", ""); err != nil {
		t.Fatalf("delete empty project after app tombstone: %v", err)
	}
}

// TestProjectNetworkMembershipMigrationUpDown 迁移 00025 的 Up/Down 往返
// （goose 演练；生产回滚 = 恢复快照——Down 仅证明回滚 SQL 可执行）。
func TestProjectNetworkMembershipMigrationUpDown(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fsys, err := migrationFiles()
	if err != nil {
		t.Fatalf("migrationFiles: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	ctx := context.Background()
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// 行为冒烟：列存在、默认 0（缺省不参加——OT-1 隔离现状）。
	if _, err := db.Exec(`INSERT INTO teams (id, slug, name, created_by, created_at) VALUES ('t1', 't1', 'x', 'u', 1)`); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, team_id, slug, name, description, created_at) VALUES ('p1', 't1', 'p1', 'x', '', 1)`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO apps (id, name, lifecycle, created_at, updated_at, project_id, team_id) VALUES ('a1', 'x', 'active', 1, 1, 'p1', 't1')`); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var attached int
	if err := db.QueryRow(`SELECT project_network_attached FROM apps WHERE id = 'a1'`).Scan(&attached); err != nil || attached != 0 {
		t.Fatalf("default attached = %d (err %v), want 0", attached, err)
	}

	// Down 到 00024：列与索引删除；再 Up：恢复。
	if _, err := provider.DownTo(ctx, 24); err != nil {
		t.Fatalf("DownTo 24: %v", err)
	}
	if columnExists(t, db, "apps", "project_network_attached") {
		t.Fatal("project_network_attached should be gone after DownTo 24")
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up (again): %v", err)
	}
	if !columnExists(t, db, "apps", "project_network_attached") {
		t.Fatal("project_network_attached should be back after re-Up")
	}
}

// columnExists 报告表是否含指定列（迁移 Up/Down 演练断言面；tableExists
// 同族的 PRAGMA 读）。
func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma: %v", err)
	}
	return false
}
