package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// domains 台账读写测试（T2.15/T2.16 + IMPL-T1-1 资源面）：域名资源 CRUD
// （归一化后形态）、配额与冲突、播种仲裁、证书材料登记（sha256 + 到期）与
// 未命中哨兵。T2.15 的「声明集对账（省略=删除）/归属迁移」随 label 降级为
// bootstrap 种子退役——对应回归用例已删除（写面 = API CRUD/首部署播种）。

// TestSetDomainCertLedger 证书材料登记与删除路径。
func TestSetDomainCertLedger(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, _ := seedAppE(t, st, "shop")
	if _, err := st.CreateAppDomain(ctx, app.ID, DomainInput{
		Domain: "shop.example.test", Service: "web", Port: "80", Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create domain: %v", err)
	}

	notAfter := time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.SetDomainCert(ctx, app.ID, "shop.example.test", "abc123", notAfter); err != nil {
		t.Fatalf("set cert: %v", err)
	}
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if rows[0].CertSHA256 != "abc123" {
		t.Fatalf("cert sha not persisted: %+v", rows[0])
	}
	if !rows[0].CertNotAfter.Equal(notAfter) {
		t.Fatalf("cert not_after not persisted: %+v", rows[0])
	}
	if rows[0].CertUpdatedAt.IsZero() {
		t.Fatalf("cert_updated_at not stamped: %+v", rows[0])
	}

	// 未命中（域名已撤销后的签发竞态）→ ErrDomainNotFound。
	if err := st.SetDomainCert(ctx, app.ID, "gone.example.test", "abc123", notAfter); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("missing domain should return ErrDomainNotFound, got %v", err)
	}

	// 台账删除（应用移除路径）。
	if err := st.DeleteAppDomains(ctx, app.ID); err != nil {
		t.Fatalf("delete app domains: %v", err)
	}
	rows, _ = st.ListAppDomains(ctx, app.ID)
	if len(rows) != 0 {
		t.Fatalf("rows remain after delete: %+v", rows)
	}
}

func TestListAllDomainsAcrossApps(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app1, _ := seedAppE(t, st, "one")
	app2, _ := seedAppE(t, st, "two")
	if _, err := st.CreateAppDomain(ctx, app1.ID, DomainInput{
		Domain: "one.example.test", Service: "web", Port: "80", Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create app1 domain: %v", err)
	}
	if _, err := st.CreateAppDomain(ctx, app2.ID, DomainInput{
		Domain: "two.example.test", Service: "web", Port: "80", Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create app2 domain: %v", err)
	}
	all, err := st.ListAllDomains(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 global rows, got %d", len(all))
	}
	if all[0].Domain >= all[1].Domain {
		t.Fatalf("rows not domain-ordered: %+v", all)
	}
}

// TestCreateAppDomainRoundTrip 域名资源 CRUD 基线（IMPL-T1-1）：显式
// protocol/cert_mode 落库可回读；Get 单行寻址。
func TestCreateAppDomainRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, err := seedAppE(t, st, "shop")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	row, err := st.CreateAppDomain(ctx, app.ID, DomainInput{
		Domain: "api.example.test", Service: "web", Port: "9090",
		Protocol: "h2c", CertMode: "wildcard",
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if row.Protocol != "h2c" || row.CertMode != "wildcard" || row.Port != "9090" {
		t.Fatalf("create returned wrong fields: %+v", row)
	}
	got, err := st.GetAppDomain(ctx, app.ID, "api.example.test")
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if got != row {
		t.Fatalf("get domain = %+v, want %+v", got, row)
	}
	if _, err := st.GetAppDomain(ctx, app.ID, "missing.example.test"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("missing domain err = %v, want ErrDomainNotFound", err)
	}
}

// TestSeedAppDomainsDefaults 播种路径（SeedAppDomainsIfEmpty）写入的行取
// 迁移默认值 http/http01（现行行为不回退：既有应用升级后路由语义不变）。
func TestSeedAppDomainsDefaults(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, _ := seedAppE(t, st, "legacy")
	if _, err := st.SeedAppDomainsIfEmpty(ctx, app.ID, []DomainServiceRoutes{
		{Service: "web", Port: "8080", Domains: []string{"legacy.example.test"}},
	}); err != nil {
		t.Fatalf("seed domains: %v", err)
	}
	rows, err := st.ListAppDomains(ctx, app.ID)
	if err != nil {
		t.Fatalf("list domains: %v", err)
	}
	if len(rows) != 1 || rows[0].Protocol != "http" || rows[0].CertMode != "http01" {
		t.Fatalf("seeded row defaults = %+v, want protocol=http cert_mode=http01", rows)
	}
}

// TestCreateAppDomainConflictAndLimits 写面约束（IMPL-T1-1 守卫④的 state
// 取证）：host 全局独占 → ErrDomainConflict；每服务 ≤5 / 每 app ≤10 →
// *DomainLimitError（scope 点名）。
func TestCreateAppDomainConflictAndLimits(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, _ := seedAppE(t, st, "limits")
	other, _ := seedAppE(t, st, "other")

	mk := func(appID, domain, service string) error {
		_, err := st.CreateAppDomain(ctx, appID, DomainInput{
			Domain: domain, Service: service, Port: "8080",
			Protocol: "http", CertMode: "http01",
		})
		return err
	}
	if err := mk(app.ID, "one.example.test", "web"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// host 冲突：同 host 不能二次落（跨 app 同禁——UNIQUE 是全局约束）。
	if err := mk(app.ID, "one.example.test", "api"); !errors.Is(err, ErrDomainConflict) {
		t.Fatalf("duplicate host err = %v, want ErrDomainConflict", err)
	}
	if err := mk(other.ID, "one.example.test", "web"); !errors.Is(err, ErrDomainConflict) {
		t.Fatalf("cross-app duplicate host err = %v, want ErrDomainConflict", err)
	}
	// 每服务 ≤5：web 已有 1，补 4 后第 6 个拒绝。
	for i := 0; i < 4; i++ {
		if err := mk(app.ID, fmt.Sprintf("w%d.example.test", i), "web"); err != nil {
			t.Fatalf("fill web service: %v", err)
		}
	}
	var limitErr *DomainLimitError
	if err := mk(app.ID, "w-over.example.test", "web"); !errors.As(err, &limitErr) || limitErr.Scope != "service" {
		t.Fatalf("service limit err = %v, want *DomainLimitError(service)", err)
	}
	// 每 app ≤10：web 5 + api 5 后第 11 个拒绝（第三个服务上触发 app 门）。
	for i := 0; i < 5; i++ {
		if err := mk(app.ID, fmt.Sprintf("a%d.example.test", i), "api"); err != nil {
			t.Fatalf("fill api service: %v", err)
		}
	}
	limitErr = nil
	if err := mk(app.ID, "app-over.example.test", "worker"); !errors.As(err, &limitErr) || limitErr.Scope != "app" {
		t.Fatalf("app limit err = %v, want *DomainLimitError(app)", err)
	}
}

// TestSeedAppDomainsIfEmptyArbitration 首部署播种的原子仲裁（IMPL-T1-1）：
// 无行 → 播种默认值（http/http01）返回 true；已有行 → false 且行原样
//（并发 API 写行不被覆盖/删除——检空与插入同事务）。
func TestSeedAppDomainsIfEmptyArbitration(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, _ := seedAppE(t, st, "seedarb")

	seeded, err := st.SeedAppDomainsIfEmpty(ctx, app.ID, []DomainServiceRoutes{
		{Service: "web", Port: "8080", Domains: []string{"seed.example.test"}},
	})
	if err != nil || !seeded {
		t.Fatalf("first seed = (%v, %v), want (true, nil)", seeded, err)
	}
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows) != 1 || rows[0].Protocol != "http" || rows[0].CertMode != "http01" {
		t.Fatalf("seeded rows wrong: %+v", rows)
	}
	// 已有行（API 写面形态）：播种原语 no-op，绝不删除/覆盖既有行。
	if _, err := st.CreateAppDomain(ctx, app.ID, DomainInput{
		Domain: "api.example.test", Service: "api", Port: "9090", Protocol: "h2c", CertMode: "wildcard",
	}); err != nil {
		t.Fatalf("api create: %v", err)
	}
	seeded, err = st.SeedAppDomainsIfEmpty(ctx, app.ID, []DomainServiceRoutes{
		{Service: "web", Port: "8080", Domains: []string{"other.example.test"}},
	})
	if err != nil || seeded {
		t.Fatalf("second seed = (%v, %v), want (false, nil)", seeded, err)
	}
	rows, _ = st.ListAppDomains(ctx, app.ID)
	if len(rows) != 2 {
		t.Fatalf("seed must not touch existing rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Domain == "api.example.test" && (r.Protocol != "h2c" || r.CertMode != "wildcard") {
			t.Fatalf("concurrent API row overwritten by seed: %+v", r)
		}
		if r.Domain == "other.example.test" {
			t.Fatal("seed must be a no-op when rows exist")
		}
	}
}

// TestUpdateAppDomainKeepsHostAndMovesService 资源更新：host 是身份不改名；
// service 迁移后的行字段完整切换；不存在 → ErrDomainNotFound；删除幂等
// 哨兵。
func TestUpdateAppDomainKeepsHostAndMovesService(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, _ := seedAppE(t, st, "upd")
	if _, err := st.CreateAppDomain(ctx, app.ID, DomainInput{
		Domain: "move.example.test", Service: "web", Port: "8080",
		Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	row, err := st.UpdateAppDomain(ctx, app.ID, "move.example.test", DomainInput{
		Service: "api", Port: "9090", Protocol: "h2c", CertMode: "wildcard",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if row.Service != "api" || row.Port != "9090" || row.Protocol != "h2c" || row.CertMode != "wildcard" {
		t.Fatalf("updated row wrong: %+v", row)
	}
	rows, _ := st.ListAppDomains(ctx, app.ID)
	if len(rows) != 1 || rows[0].Service != "api" {
		t.Fatalf("ledger after update: %+v", rows)
	}
	if _, err := st.UpdateAppDomain(ctx, app.ID, "missing.example.test", DomainInput{Service: "web", Port: "80", Protocol: "http", CertMode: "http01"}); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("update missing err = %v, want ErrDomainNotFound", err)
	}
	if err := st.RemoveAppDomain(ctx, app.ID, "move.example.test"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := st.RemoveAppDomain(ctx, app.ID, "move.example.test"); !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("second remove err = %v, want ErrDomainNotFound", err)
	}
}

// TestDomainsEndpointColumnsMigrationUpDown 迁移 00023 的 Up/Down 往返
// （IMPL-F2 整改：实施记录守卫表曾误引 TestPlatformSettingsMigrationUpDown
// ——00023 的 Down 腿此前零测试执行。goose 演练；生产回滚 = 恢复快照——
// Down 仅证明回滚 SQL 可执行）。
func TestDomainsEndpointColumnsMigrationUpDown(t *testing.T) {
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
	if !columnExists(t, db, "domains", "protocol") || !columnExists(t, db, "domains", "cert_mode") {
		t.Fatal("domains.protocol/cert_mode should exist after Up")
	}
	// 行为冒烟：归属父行先行保证 FK 干净；一条显式 h2c/wildcard 落库，一条
	// 缺省行取 NOT NULL DEFAULT（http/http01 = 既有行现行行为，迁移文件头
	// 注释口径）。
	if _, err := db.Exec(`INSERT INTO teams (id, slug, name, created_by, created_at) VALUES ('t1', 't1', 'x', 'u', 1)`); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, team_id, slug, name, description, created_at) VALUES ('p1', 't1', 'p1', 'x', '', 1)`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO apps (id, name, lifecycle, created_at, updated_at, project_id, team_id) VALUES ('a1', 'x', 'active', 1, 1, 'p1', 't1')`); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO domains (id, app_id, service, domain, created_at, port, protocol, cert_mode)
		VALUES ('d1', 'a1', 'web', 'h2c.example.test', 1, '8080', 'h2c', 'wildcard')`); err != nil {
		t.Fatalf("insert domain (explicit): %v", err)
	}
	if _, err := db.Exec(`INSERT INTO domains (id, app_id, service, domain, created_at, port)
		VALUES ('d2', 'a1', 'web', 'legacy.example.test', 2, '80')`); err != nil {
		t.Fatalf("insert domain (defaults): %v", err)
	}
	var protocol, certMode string
	if err := db.QueryRow(`SELECT protocol, cert_mode FROM domains WHERE id = 'd2'`).Scan(&protocol, &certMode); err != nil || protocol != "http" || certMode != "http01" {
		t.Fatalf("default columns = (%s, %s) (err %v), want (http, http01)", protocol, certMode, err)
	}

	// Down 到 00022：00023 的 Down 腿 DROP cert_mode 再 DROP protocol。
	// DownTo 从 00027 一路穿过 00023（此前零执行的那条腿），版本号钉在 22。
	if _, err := provider.DownTo(ctx, 22); err != nil {
		t.Fatalf("DownTo 22: %v", err)
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != 22 {
		t.Fatalf("version after DownTo = %d (err %v), want 22", version, err)
	}
	if columnExists(t, db, "domains", "protocol") || columnExists(t, db, "domains", "cert_mode") {
		t.Fatal("domains.protocol/cert_mode should be gone after DownTo 22")
	}
	// Down 只摘本迁移的列：00006 的列与数据行原样保留（DROP COLUMN 语义
	// 不牵连同行其余列）。
	if !columnExists(t, db, "domains", "port") {
		t.Fatal("domains.port (from 00006) should survive DownTo 22")
	}
	var service, port string
	if err := db.QueryRow(`SELECT service, port FROM domains WHERE id = 'd1'`).Scan(&service, &port); err != nil || service != "web" || port != "8080" {
		t.Fatalf("row after DownTo = (%s, %s) (err %v), want (web, 8080)", service, port, err)
	}

	// 再 Up：列恢复；既有行取 NOT NULL DEFAULT——h2c/wildcard 已随 Down
	// 丢弃（DROP COLUMN 丢列数据），回来只有 DEFAULT 可填（http/http01）。
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up (again): %v", err)
	}
	if !columnExists(t, db, "domains", "protocol") || !columnExists(t, db, "domains", "cert_mode") {
		t.Fatal("domains.protocol/cert_mode should exist after re-Up")
	}
	for _, id := range []string{"d1", "d2"} {
		if err := db.QueryRow(`SELECT protocol, cert_mode FROM domains WHERE id = ?`, id).Scan(&protocol, &certMode); err != nil {
			t.Fatalf("read %s after re-Up: %v", id, err)
		}
		if protocol != "http" || certMode != "http01" {
			t.Fatalf("row %s after re-Up = (%s, %s), want defaults (http, http01)", id, protocol, certMode)
		}
	}
}
