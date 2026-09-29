package state

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 库状态机与 db_instances CRUD 的验收测试（managed-databases 设计 §2.1/
// §2.3，E4 S1）：转移表穷举钉死（合法集合 = 设计 §2.1 表逐行）、
// EnterDbPhase 单写点机制（非法拒写 + CAS 竞争 + 事件原子）、CRUD 往返。

// TestDatabaseTransitionsExhaustive 转移表穷举：CanTransitionDatabase 与
// 设计 §2.1 转移表的合法集合逐对一致（7×7 全对遍历 + DatabaseTransitions
// 表本身与合法集合互证——表外组合零容忍）。
func TestDatabaseTransitionsExhaustive(t *testing.T) {
	// 合法转移全集（§2.1 表原文逐行；deleted 终态无出边）。
	legal := map[DatabaseState]map[DatabaseState]bool{
		DatabaseProvisioning: {DatabaseReady: true, DatabaseFailed: true},
		DatabaseFailed:       {DatabaseProvisioning: true, DatabaseDeleting: true},
		DatabaseReady:        {DatabaseDegraded: true, DatabasePaused: true, DatabaseDeleting: true},
		DatabaseDegraded:     {DatabaseReady: true, DatabasePaused: true, DatabaseDeleting: true},
		DatabasePaused:       {DatabaseProvisioning: true, DatabaseDeleting: true},
		DatabaseDeleting:     {DatabaseDeleted: true},
		DatabaseDeleted:      {},
	}
	for _, from := range AllDatabaseStates() {
		for _, to := range AllDatabaseStates() {
			if got := CanTransitionDatabase(from, to); got != legal[from][to] {
				t.Fatalf("CanTransitionDatabase(%s, %s) = %v, want %v", from, to, got, legal[from][to])
			}
			// 表内容与合法集合互证（LegalDatabaseTransitions 零差异）。
			table := map[DatabaseState]bool{}
			for _, tgt := range LegalDatabaseTransitions(from) {
				table[tgt] = true
			}
			if table[to] != legal[from][to] {
				t.Fatalf("DatabaseTransitions[%s] contains %s = %v, want %v", from, to, table[to], legal[from][to])
			}
		}
	}
	// 未知状态一律非法。
	if CanTransitionDatabase(DatabaseState("bogus"), DatabaseReady) ||
		CanTransitionDatabase(DatabaseReady, DatabaseState("bogus")) {
		t.Fatal("unknown state should be illegal")
	}
}

// TestEnterDbPhaseSingleWritePoint EnterDbPhase 单写点机制验收：
//  1. 合法边：状态 + tombstone 锚 + 事件同事务落库；
//  2. 表外组合：拒写且不产生事件（事务回滚）；
//  3. CAS 竞争落败：ErrDatabaseStateConflict（携带当前状态）且无事件；
//  4. 行缺失：ErrDatabaseNotFound。
func TestEnterDbPhaseSingleWritePoint(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	newInstance := func(name string) DatabaseInstance {
		t.Helper()
		proj := seedFixtureProject(t, st)
		row, err := st.CreateDatabaseInstance(ctx, DatabaseInstance{
			Name: name, Template: "postgres-16", ImageDigest: "postgres:16@sha256:aaa",
			CredentialCipher: "age-cipher",
			ProjectID:        proj.ID, TeamID: proj.TeamID,
		})
		if err != nil {
			t.Fatalf("create instance %s: %v", name, err)
		}
		return row
	}
	readyEvent := func(id string) Event {
		return Event{Name: "db.ready", Subject: "database:" + id}
	}

	// 1. 合法边 provisioning → ready：事件与状态同拍。
	inst := newInstance("edge-app")
	if err := st.EnterDbPhase(ctx, inst.ID, DatabaseProvisioning, DatabaseReady, readyEvent(inst.ID)); err != nil {
		t.Fatalf("provisioning -> ready: %v", err)
	}
	row, err := st.GetDatabaseInstance(ctx, inst.ID)
	if err != nil || row.State != DatabaseReady {
		t.Fatalf("row after legal edge = %+v err=%v, want ready", row, err)
	}
	if names := eventNames(t, st); len(names) != 1 || names[0] != "db.ready" {
		t.Fatalf("events = %v, want exactly [db.ready]", names)
	}

	// 2. 表外组合（ready → provisioning 无边）：拒写、无事件。
	if err := st.EnterDbPhase(ctx, inst.ID, DatabaseReady, DatabaseProvisioning, readyEvent(inst.ID)); !errors.Is(err, ErrDatabaseIllegalTransition) {
		t.Fatalf("ready -> provisioning err = %v, want ErrDatabaseIllegalTransition", err)
	}
	if names := eventNames(t, st); len(names) != 1 {
		t.Fatalf("events after illegal transition = %v, want unchanged (same-transaction rollback)", names)
	}

	// 3. CAS 竞争落败（合法边 provisioning → ready，但行已在 ready——
	// 声明的 from 陈旧）：ErrDatabaseStateConflict 且消息携带当前状态。
	err = st.EnterDbPhase(ctx, inst.ID, DatabaseProvisioning, DatabaseReady)
	if !errors.Is(err, ErrDatabaseStateConflict) {
		t.Fatalf("stale from-state err = %v, want ErrDatabaseStateConflict", err)
	}
	if !strings.Contains(err.Error(), string(DatabaseReady)) {
		t.Fatalf("conflict message must carry current state: %s", err)
	}
	if names := eventNames(t, st); len(names) != 1 {
		t.Fatalf("events after CAS loss = %v, want unchanged", names)
	}

	// 4. 行缺失：ErrDatabaseNotFound（消息形态不做要求）。
	if err := st.EnterDbPhase(ctx, "01JMISSING", DatabaseProvisioning, DatabaseReady); !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("missing instance err = %v, want ErrDatabaseNotFound", err)
	}

	// 5. tombstone 两拍锚：deleting 落 deleting_at、deleted 落 deleted_at。
	inst2 := newInstance("tomb-app")
	if err := st.EnterDbPhase(ctx, inst2.ID, DatabaseProvisioning, DatabaseFailed); err != nil {
		t.Fatalf("provisioning -> failed: %v", err)
	}
	before := time.Now().UTC()
	if err := st.EnterDbPhase(ctx, inst2.ID, DatabaseFailed, DatabaseDeleting); err != nil {
		t.Fatalf("failed -> deleting: %v", err)
	}
	if err := st.EnterDbPhase(ctx, inst2.ID, DatabaseDeleting, DatabaseDeleted); err != nil {
		t.Fatalf("deleting -> deleted: %v", err)
	}
	row2, err := st.GetDatabaseInstance(ctx, inst2.ID)
	if err != nil || row2.State != DatabaseDeleted {
		t.Fatalf("row after tombstone = %+v err=%v, want deleted", row2, err)
	}
	if row2.DeletingAt.IsZero() || row2.DeletedAt.IsZero() {
		t.Fatalf("tombstone anchors not stamped: deleting=%v deleted=%v", row2.DeletingAt, row2.DeletedAt)
	}
	if row2.DeletingAt.Before(before.Add(-time.Minute)) || row2.DeletedAt.Before(before.Add(-time.Minute)) {
		t.Fatalf("tombstone anchors not ~now: deleting=%v deleted=%v before=%v", row2.DeletingAt, row2.DeletedAt, before)
	}
	// 终态无出边（deleted → 任何目标非法）。
	if err := st.EnterDbPhase(ctx, inst2.ID, DatabaseDeleted, DatabaseProvisioning); !errors.Is(err, ErrDatabaseIllegalTransition) {
		t.Fatalf("deleted -> provisioning err = %v, want ErrDatabaseIllegalTransition", err)
	}
}

// TestDatabaseInstanceCRUD CRUD 往返：创建校验（名字规则/必填位/初态）、
// 唯一名占用、按 id/name 取、列表、settings/凭据/绑定/digest 更新。
func TestDatabaseInstanceCRUD(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// 创建校验。
	bad := []DatabaseInstance{
		{Name: "Pg-Bad", Template: "postgres-16", ImageDigest: "x", CredentialCipher: "c"},
		{Name: "", Template: "postgres-16", ImageDigest: "x", CredentialCipher: "c"},
		{Name: "ok-name", Template: "", ImageDigest: "x", CredentialCipher: "c"},
		{Name: "ok-name", Template: "postgres-16", ImageDigest: "", CredentialCipher: "c"},
		{Name: "ok-name", Template: "postgres-16", ImageDigest: "x", CredentialCipher: ""},
		{Name: "ok-name", Template: "postgres-16", ImageDigest: "x", CredentialCipher: "c", State: DatabaseReady},
	}
	for i, in := range bad {
		if _, err := st.CreateDatabaseInstance(ctx, in); err == nil {
			t.Errorf("bad create #%d accepted: %+v", i, in)
		}
	}

	settings := DatabaseSettings{
		CPUSeconds: 2.0, MemoryBytes: 4 << 30,
		Backup: DatabaseBackupPlan{IntervalHours: 12, Keep: 14, HourUTC: 4},
	}
	proj := seedFixtureProject(t, st)
	//nolint:gosec // G101：测试夹具密文标记，非真实凭据
	inst, err := st.CreateDatabaseInstance(ctx, DatabaseInstance{
		Name: "pg-prod", Template: "postgres-16", ImageDigest: "postgres:16@sha256:aaa",
		CredentialCipher: "age-cipher-v1", PlatformNodeID: "n_01", Settings: settings,
		ProjectID: proj.ID, TeamID: proj.TeamID,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inst.State != DatabaseProvisioning {
		t.Fatalf("initial state = %s, want provisioning (acceptance is provisioning)", inst.State)
	}
	if inst.CreatedAt.IsZero() || inst.UpdatedAt.IsZero() {
		t.Fatal("created timestamps not stamped")
	}
	if !inst.CredentialUpdatedAt.IsZero() {
		t.Fatalf("credential_updated_at = %v, want zero (never rotated)", inst.CredentialUpdatedAt)
	}

	// 唯一名占用（同项目、任意生命周期态）。
	if _, err := st.CreateDatabaseInstance(ctx, DatabaseInstance{
		Name: "pg-prod", Template: "redis-7", ImageDigest: "y", CredentialCipher: "c",
		ProjectID: proj.ID, TeamID: proj.TeamID,
	}); !errors.Is(err, ErrDatabaseExists) {
		t.Fatalf("duplicate name err = %v, want ErrDatabaseExists", err)
	}

	// 按 id/name 取 + 列表（settings 往返）。
	byID, err := st.GetDatabaseInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	byName, err := st.GetDatabaseInstanceByName(ctx, "pg-prod")
	if err != nil || byName.ID != inst.ID {
		t.Fatalf("get by name = %+v err=%v", byName, err)
	}
	if byID.Settings != settings {
		t.Fatalf("settings round-trip = %+v, want %+v", byID.Settings, settings)
	}
	if len(byID.CredentialCipher) == 0 || byID.CredentialCipher != "age-cipher-v1" {
		t.Fatalf("credential cipher round-trip broken: %q", byID.CredentialCipher)
	}

	//nolint:gosec // G101：测试夹具密文标记，非真实凭据
	other, err := st.CreateDatabaseInstance(ctx, DatabaseInstance{
		Name: "redis-cache", Template: "redis-7", ImageDigest: "redis:7@sha256:bbb", CredentialCipher: "age-cipher-v2",
		ProjectID: proj.ID, TeamID: proj.TeamID,
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	rows, err := st.ListDatabaseInstances(ctx)
	if err != nil || len(rows) != 2 || rows[0].Name != "pg-prod" || rows[1].Name != "redis-cache" {
		t.Fatalf("list = %+v err=%v, want [pg-prod redis-cache] ordered by name", rows, err)
	}
	_ = other

	// 设置更新（含删除过程/终态拒绝）。
	newSettings := DatabaseSettings{CPUSeconds: 0.25, Backup: DatabaseBackupPlan{IntervalHours: 48, Keep: 3, HourUTC: 1}}
	if err := st.UpdateDatabaseSettings(ctx, inst.ID, newSettings); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	got, err := st.GetDatabaseInstance(ctx, inst.ID)
	if err != nil || got.Settings != newSettings {
		t.Fatalf("settings after update = %+v err=%v, want %+v", got.Settings, err, newSettings)
	}
	if err := st.UpdateDatabaseSettings(ctx, "01JMISSING", newSettings); !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("settings on missing instance err = %v, want ErrDatabaseNotFound", err)
	}

	// 凭据轮换落库（密文 + credential_updated_at 同拍）。
	if err := st.UpdateDatabaseCredential(ctx, inst.ID, ""); err == nil {
		t.Fatal("empty cipher accepted")
	}
	if err := st.UpdateDatabaseCredential(ctx, inst.ID, "age-cipher-v2"); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	rotated, err := st.GetDatabaseInstance(ctx, inst.ID)
	if err != nil || rotated.CredentialCipher != "age-cipher-v2" || rotated.CredentialUpdatedAt.IsZero() {
		t.Fatalf("after rotate = %+v err=%v, want new cipher + stamped credential_updated_at", rotated, err)
	}

	// 节点绑定与 digest 回写。
	if err := st.SetDatabaseNodeBinding(ctx, inst.ID, "n_02"); err != nil {
		t.Fatalf("set binding: %v", err)
	}
	if err := st.UpdateImageDigest(ctx, inst.ID, "postgres:16@sha256:ccc"); err != nil {
		t.Fatalf("update digest: %v", err)
	}
	bound, err := st.GetDatabaseInstance(ctx, inst.ID)
	if err != nil || bound.PlatformNodeID != "n_02" || bound.ImageDigest != "postgres:16@sha256:ccc" {
		t.Fatalf("binding/digest = %+v err=%v", bound, err)
	}

	// 删除过程态拒绝设置变更（deleting 非终态但不在「任意非终态」准入内
	// ——操作口径含 deleting 排除）。
	if err := st.EnterDbPhase(ctx, inst.ID, DatabaseProvisioning, DatabaseFailed); err != nil {
		t.Fatalf("to failed: %v", err)
	}
	if err := st.EnterDbPhase(ctx, inst.ID, DatabaseFailed, DatabaseDeleting); err != nil {
		t.Fatalf("to deleting: %v", err)
	}
	if err := st.UpdateDatabaseSettings(ctx, inst.ID, newSettings); !errors.Is(err, ErrDatabaseTerminal) {
		t.Fatalf("settings while deleting err = %v, want ErrDatabaseTerminal", err)
	}
}

// TestValidateDatabaseNameMatchesAppRule 名字规则与 app 名同集（compose
// specNamePattern `^[a-z0-9][a-z0-9_-]*$` 语料互证——规则漂移即测试失败）。
func TestValidateDatabaseNameMatchesAppRule(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	corpus := []string{
		"pg-prod", "pg_prod", "pg1", "a", "my-db-01", "0abc", "a-b_c9",
		"", "-pg", "_pg", "Pg", "pg prod", "pg.prod", "pg/prod", "大写",
	}
	for _, name := range corpus {
		want := pattern.MatchString(name)
		err := ValidateDatabaseName(name)
		if (err == nil) != want {
			t.Errorf("ValidateDatabaseName(%q) err = %v, want valid=%v (app name rule drift)", name, err, want)
		}
	}
}
