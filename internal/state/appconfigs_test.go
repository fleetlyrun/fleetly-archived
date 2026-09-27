package state

// app_configs 读写测试（T 线 OT-3 / IMPL-T1-4）：明文往返（与 app_secrets 的
// 密文面刻意不同）、覆盖即换版（hash8 重盖）、列表序、删除不存在的显式
// 哨兵、写通道校验、迁移 00024 Up/Down 演练。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// testHash8 是 state 包测试内的指纹计算（包内不能 import naming——naming
// 反向依赖 state，测试循环导入；公式与 naming.Hash8 逐字一致 = sha256 hex
// 前 8 位）。
func testHash8(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:8]
}

// TestAppConfigsStore 明文配置资源的存储面契约。
func TestAppConfigsStore(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	appID := createTestApp(t, st, "cfgapp")

	const v1 = "log_level: info\nlisten: 8080\n"
	row, err := st.UpsertAppConfig(ctx, AppConfig{AppID: appID, Name: "app.yaml", Value: v1, Hash8: testHash8(v1)})
	if err != nil {
		t.Fatalf("upsert config: %v", err)
	}
	if row.Name != "app.yaml" || row.Value != v1 {
		t.Fatalf("row = %+v, want the plaintext content round-trip", row)
	}
	if row.Hash8 != testHash8(v1) {
		t.Fatalf("hash8 = %s, want %s", row.Hash8, testHash8(v1))
	}

	// 覆盖即换版：值/指纹/updated_at 重盖，created_at 保留。
	const v2 = "log_level: debug\n"
	rotated, err := st.UpsertAppConfig(ctx, AppConfig{AppID: appID, Name: "app.yaml", Value: v2, Hash8: testHash8(v2)})
	if err != nil {
		t.Fatalf("rotate config: %v", err)
	}
	if rotated.Hash8 != testHash8(v2) || rotated.Value != v2 {
		t.Fatalf("rotated row = %+v, want new content + fingerprint", rotated)
	}
	if !rotated.CreatedAt.Equal(row.CreatedAt) {
		t.Fatalf("created_at moved on rotation: %v -> %v", row.CreatedAt, rotated.CreatedAt)
	}
	if rotated.UpdatedAt.Before(row.UpdatedAt) {
		t.Fatalf("updated_at did not advance: %v -> %v", row.UpdatedAt, rotated.UpdatedAt)
	}

	// 列表序（name 字典序）+ 明文回读。
	if _, err := st.UpsertAppConfig(ctx, AppConfig{AppID: appID, Name: "AAA.txt", Value: "a", Hash8: testHash8("a")}); err != nil {
		t.Fatalf("upsert second config: %v", err)
	}
	rows, err := st.ListAppConfigs(ctx, appID)
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "AAA.txt" || rows[1].Name != "app.yaml" {
		t.Fatalf("list = %+v, want name-ordered two rows", rows)
	}
	got, err := st.GetAppConfig(ctx, appID, "app.yaml")
	if err != nil || got.Value != v2 {
		t.Fatalf("get config = %+v (err %v), want plaintext readback", got, err)
	}
	if _, err := st.GetAppConfig(ctx, appID, "MISSING"); !errors.Is(err, ErrAppConfigNotFound) {
		t.Fatalf("missing get err = %v, want ErrAppConfigNotFound", err)
	}

	// 写通道校验：值与指纹必填（调用方按 naming.Hash8 计算）。
	for _, bad := range []AppConfig{
		{Name: "x", Value: "v", Hash8: "aaaaaaaa"},
		{AppID: appID, Value: "v", Hash8: "aaaaaaaa"},
		{AppID: appID, Name: "x", Hash8: "aaaaaaaa"},
		{AppID: appID, Name: "x", Value: "v"},
	} {
		if _, err := st.UpsertAppConfig(ctx, bad); err == nil {
			t.Fatalf("upsert %+v accepted, want validation error", bad)
		}
	}

	// 删除（幂等不做：二次删除显式哨兵）。
	if err := st.RemoveAppConfig(ctx, appID, "AAA.txt"); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	if err := st.RemoveAppConfig(ctx, appID, "AAA.txt"); !errors.Is(err, ErrAppConfigNotFound) {
		t.Fatalf("second remove err = %v, want ErrAppConfigNotFound", err)
	}
}

// TestAppConfigsMigrationUpDown 迁移 00024 的 Up/Down 往返（goose 演练；
// 生产回滚 = 恢复快照——Down 仅证明回滚 SQL 可执行）。
func TestAppConfigsMigrationUpDown(t *testing.T) {
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
	if !tableExists(t, db, "app_configs") {
		t.Fatal("app_configs should exist after Up")
	}
	// 行为冒烟：UNIQUE (app_id, name) 冲突即更新（覆盖即换版的口径底座）。
	if _, err := db.Exec(`INSERT INTO app_configs (id, app_id, name, value, hash8, created_at, updated_at)
		VALUES ('c1', 'a1', 'k', 'v1', 'h1', 1, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO app_configs (id, app_id, name, value, hash8, created_at, updated_at)
		VALUES ('c2', 'a1', 'k', 'v2', 'h2', 2, 2)
		ON CONFLICT(app_id, name) DO UPDATE SET value = excluded.value, hash8 = excluded.hash8`); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM app_configs WHERE app_id = 'a1' AND name = 'k'`).Scan(&v); err != nil || v != "v2" {
		t.Fatalf("upsert result = %s (err %v), want v2", v, err)
	}

	// Down 到 00023：表删除；再 Up：恢复。
	if _, err := provider.DownTo(ctx, 23); err != nil {
		t.Fatalf("DownTo 23: %v", err)
	}
	if tableExists(t, db, "app_configs") {
		t.Fatal("app_configs should be gone after DownTo 23")
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up (again): %v", err)
	}
	if !tableExists(t, db, "app_configs") {
		t.Fatal("app_configs should exist after re-Up")
	}
}
