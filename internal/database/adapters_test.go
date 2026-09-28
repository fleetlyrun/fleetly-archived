package database

// IMPL-DB-0 适配器轴迁移与工具面版本纪律的单测（不依赖 DB-1 词表——用
// 合成模板钉住「按实例 major 选 PG 工具面」行为）：
//   - 分派轴 = dbtemplate.Template.Engine（同族发行版/大版本共享；未知引擎
//     诚实报错）；
//   - PG 工具二进制 = /usr/lib/postgresql/<major>/bin（显式绝对路径——两代
//     并存后 /usr/bin 的 pg_wrapper 会把裸名解析为最新 major）；
//   - 恢复 PGDATA = VolumeTarget + "/pgdata"（PG16 现值逐字不变）；
//   - 既有四模板全脚本可构建（行为零变化锚）。

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
)

// TestPostgresToolFaceSelectedByInstanceMajor 工具面版本/发行版纪律（IMPL-DB-0/
// DB-1 守卫核心）：dump/verify/restore 三段脚本的 PG 二进制全部取模板
// （发行版 × major）的显式路径；恢复 PGDATA 取 VolumeTarget + /pgdata；
// 缺面前置点名；脚本不得出现别代/别发行版的工具路径（混版面零容忍）。
func TestPostgresToolFaceSelectedByInstanceMajor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tpl       dbtemplate.Template
		wantDir   string
		mountPath string
	}{
		{"vanilla16", mustTemplate(t, dbtemplate.TemplatePostgres16), "/usr/lib/postgresql/16/bin", "/var/lib/postgresql/data"},
		{"vanilla18", mustTemplate(t, dbtemplate.TemplatePostgres18), "/usr/lib/postgresql/18/bin", "/var/lib/postgresql/data"},
		{"percona18", mustTemplate(t, dbtemplate.TemplatePerconaPostgresql18), "/usr/pgsql-18/bin", "/data/db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.wantDir
			instance := "pgface"
			backupIn := dbtemplate.BackupInput{Instance: instance, TemplateID: tc.tpl.ID}
			backupScript, err := backupJobScript(tc.tpl, backupIn)
			if err != nil {
				t.Fatalf("backupJobScript: %v", err)
			}
			wantDump := fmt.Sprintf("%s/pg_dump -h %s -U fleetly -d %s -Fc", dir, instance, dbtemplate.DatabaseName(instance))
			if !strings.Contains(strings.Join(backupScript, " "), wantDump) {
				t.Fatalf("backup script missing the selected pg_dump %q:\n%s", wantDump, strings.Join(backupScript, " "))
			}

			outcome := dbtemplate.BackupOutcome{Instance: instance, TemplateID: tc.tpl.ID, SnapshotID: "snap-face"}
			verifyScript, err := verifyJobScript(tc.tpl, outcome)
			if err != nil {
				t.Fatalf("verifyJobScript: %v", err)
			}
			wantRestoreList := fmt.Sprintf("%s/pg_restore --list /tmp/v.dump", dir)
			if !strings.Contains(strings.Join(verifyScript, " "), wantRestoreList) {
				t.Fatalf("verify script missing the selected pg_restore %q:\n%s", wantRestoreList, strings.Join(verifyScript, " "))
			}

			restoreScript, err := restorePostgresJobScript(tc.tpl, dbtemplate.RestoreInput{
				Instance: instance, TemplateID: tc.tpl.ID, VolumeTarget: tc.mountPath, SnapshotID: "snap-face",
			})
			if err != nil {
				t.Fatalf("restorePostgresJobScript: %v", err)
			}
			joined := strings.Join(restoreScript, "\n")
			for _, want := range []string{
				fmt.Sprintf(`test -x %s/postgres`, dir),
				`PGDATA=` + tc.mountPath + `/pgdata; export PGDATA`,
				`$PRIVDROP "$PGUID" ` + dir + `/postgres &`,
				`until ` + dir + `/pg_isready -h /var/run/postgresql -U fleetly`,
				dir + `/psql -h /var/run/postgresql -U fleetly -d postgres`,
				dir + `/pg_restore -h /var/run/postgresql -U fleetly -d "` + dbtemplate.DatabaseName(instance) + `" --no-owner`,
				`$PRIVDROP "$PGUID" ` + dir + `/pg_ctl -D "$PGDATA" -m fast stop`,
			} {
				if !strings.Contains(joined, want) {
					t.Fatalf("restore script missing %q:\n%s", want, joined)
				}
			}
			// 反向钉：脚本不得出现另一发行版的工具面路径（混版面零容忍）；
			// 同发行版另代 major 路径同样不得出现。
			switch tc.tpl.Distribution {
			case dbtemplate.DistributionPercona:
				if strings.Contains(joined, "/usr/lib/postgresql/") {
					t.Fatalf("percona restore script carries the vanilla tool face:\n%s", joined)
				}
				if strings.Contains(joined, "/usr/pgsql-16/") {
					t.Fatalf("percona restore script carries the other major's percona face:\n%s", joined)
				}
			case dbtemplate.DistributionVanilla:
				if strings.Contains(joined, "/usr/pgsql-") {
					t.Fatalf("vanilla restore script carries the percona tool face:\n%s", joined)
				}
				other := "/usr/lib/postgresql/16/bin"
				if tc.tpl.Major == 16 {
					other = "/usr/lib/postgresql/18/bin"
				}
				if strings.Contains(joined, other) {
					t.Fatalf("restore script carries the other major's tool face %s:\n%s", other, joined)
				}
			}
		})
	}
}

// TestPostgres16ToolFaceValuesUnchanged PG16 现值钉：显式路径即今日裸名经
// pg_wrapper 的解析结果（同一二进制）；PGDATA 与命令形态逐字不变。
func TestPostgres16ToolFaceValuesUnchanged(t *testing.T) {
	tpl := mustTemplate(t, dbtemplate.TemplatePostgres16)
	script, err := restorePostgresJobScript(tpl, dbtemplate.RestoreInput{
		Instance: "pg-rs", TemplateID: tpl.ID, VolumeTarget: tpl.VolumeMountPath, SnapshotID: "snap-1",
	})
	if err != nil {
		t.Fatalf("restorePostgresJobScript: %v", err)
	}
	joined := strings.Join(script, "\n")
	for _, want := range []string{
		`PGDATA=/var/lib/postgresql/data/pgdata; export PGDATA`,
		`/usr/lib/postgresql/16/bin/pg_restore -h /var/run/postgresql -U fleetly -d "pg_rs" --no-owner /var/lib/postgresql/data/fleetly-replay.dump`,
		`/usr/lib/postgresql/16/bin/pg_ctl -D "$PGDATA" -m fast stop`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("postgres-16 restore script drifted, missing %q:\n%s", want, joined)
		}
	}
}

// TestEngineDispatchCoversEveryRegistryTemplate 分派轴 = Engine 的覆盖钉：
// 注册表每个条目在四个适配器词表（文件名/备份/校验/恢复/prune）都有实现
// ——新增条目漏适配器即红（「发行版不新增适配器」的结构前提）。
func TestEngineDispatchCoversEveryRegistryTemplate(t *testing.T) {
	for _, tpl := range dbtemplate.List() {
		t.Run(tpl.ID, func(t *testing.T) {
			if _, err := backupFilename(tpl); err != nil {
				t.Fatalf("backupFilename(%s): %v", tpl.ID, err)
			}
			backupIn := dbtemplate.BackupInput{Instance: "dbx", TemplateID: tpl.ID}
			if _, err := backupJobScript(tpl, backupIn); err != nil {
				t.Fatalf("backupJobScript(%s): %v", tpl.ID, err)
			}
			outcome := dbtemplate.BackupOutcome{Instance: "dbx", TemplateID: tpl.ID, SnapshotID: "snap-x"}
			if _, err := verifyJobScript(tpl, outcome); err != nil {
				t.Fatalf("verifyJobScript(%s): %v", tpl.ID, err)
			}
			if _, err := pruneJobScript(tpl, outcome, 7); err != nil {
				t.Fatalf("pruneJobScript(%s): %v", tpl.ID, err)
			}
			restoreIn := dbtemplate.RestoreInput{Instance: "dbx", TemplateID: tpl.ID, VolumeTarget: tpl.VolumeMountPath, SnapshotID: "snap-x"}
			var err error
			switch tpl.Engine {
			case dbtemplate.EnginePostgres:
				_, err = restorePostgresJobScript(tpl, restoreIn)
			case dbtemplate.EngineRedis:
				_, err = restoreRedisFetchScript(tpl, restoreIn)
			case dbtemplate.EngineMySQL:
				_, err = restoreMySQLJobScript(tpl, restoreIn)
			case dbtemplate.EngineMongo:
				_, err = restoreMongoJobScript(tpl, restoreIn)
			default:
				t.Fatalf("template %s has unknown engine %q", tpl.ID, tpl.Engine)
			}
			if err != nil {
				t.Fatalf("restore script (%s): %v", tpl.ID, err)
			}
		})
	}
}

// TestEngineDispatchUnknownEngineAndMissingMajorFailLoud 未知引擎/缺 major
// 诚实报错（不静默回落别代工具面）。
func TestEngineDispatchUnknownEngineAndMissingMajorFailLoud(t *testing.T) {
	alien := dbtemplate.Template{ID: "cassandra-5", Engine: dbtemplate.Engine("cassandra"), Major: 5}
	if _, err := backupFilename(alien); err == nil {
		t.Fatal("backupFilename accepted an unknown engine")
	}
	if _, err := backupJobScript(alien, dbtemplate.BackupInput{Instance: "x", TemplateID: alien.ID}); err == nil {
		t.Fatal("backupJobScript accepted an unknown engine")
	}
	if _, err := verifyJobScript(alien, dbtemplate.BackupOutcome{Instance: "x", TemplateID: alien.ID}); err == nil {
		t.Fatal("verifyJobScript accepted an unknown engine")
	}
	if _, err := restorePostgresJobScript(alien, dbtemplate.RestoreInput{Instance: "x"}); err == nil {
		t.Fatal("restorePostgresJobScript accepted a non-postgres engine")
	}
	if _, err := pgToolDir(dbtemplate.Template{ID: "redis-9", Engine: dbtemplate.EngineRedis, Major: 9}); err == nil {
		t.Fatal("pgToolDir accepted a non-postgres engine")
	}
	if _, err := pgToolDir(dbtemplate.Template{ID: "postgres-x", Engine: dbtemplate.EnginePostgres}); err == nil {
		t.Fatal("pgToolDir accepted a postgres template without a major")
	}
	if _, err := pgToolDir(dbtemplate.Template{ID: "postgres-18", Engine: dbtemplate.EnginePostgres, Major: 18}); err == nil {
		t.Fatal("pgToolDir accepted a postgres template without a distribution")
	}
	if _, err := pgToolDir(dbtemplate.Template{ID: "postgres-18", Engine: dbtemplate.EnginePostgres, Major: 18, Distribution: dbtemplate.Distribution("supabase")}); err == nil {
		t.Fatal("pgToolDir accepted an unknown distribution")
	}
}

// TestEngineToolsTableConsistency 表一致性（IMPL-ARCH-F F-3）：遍历
// engineToolsTable——①表 ⇆ 注册表引擎双向覆盖（表内无幻影条目、注册表
// 引擎无缺条目——新增模板漏适配即红，与 TestEngineDispatchCoversEveryRegistry-
// Template 的测试侧枚举互为独立守卫）；②每引擎四件套非空且脚本可生成；
// ③rotate 为 nil 当且仅当 Redis（新引擎静默无动作即红——nil 是「无引擎侧
// 动作」的显式契约，不是缺省）。
func TestEngineToolsTableConsistency(t *testing.T) {
	registryEngines := map[dbtemplate.Engine]bool{}
	for _, tpl := range dbtemplate.List() {
		registryEngines[tpl.Engine] = true
	}
	tableEngines := map[dbtemplate.Engine]bool{}
	for engine := range engineToolsTable {
		tableEngines[engine] = true
	}
	for engine := range tableEngines {
		if !registryEngines[engine] {
			t.Errorf("tool table engine %q has no registry template (phantom entry)", engine)
		}
	}
	for engine := range registryEngines {
		if !tableEngines[engine] {
			t.Errorf("registry engine %q has no tool table entry (add one before registering templates)", engine)
		}
	}
	for engine, tools := range engineToolsTable {
		if tools.backupFilename == "" {
			t.Errorf("engine %s: empty backupFilename", engine)
		}
		if tools.backupExport == nil || tools.verifyScript == nil || tools.restoreScript == nil {
			t.Errorf("engine %s: incomplete tool set (backupExport/verifyScript/restoreScript must all be set)", engine)
		}
		wantRotate := engine != dbtemplate.EngineRedis
		if (tools.rotate != nil) != wantRotate {
			t.Errorf("engine %s: rotate set = %v, want %v (nil rotate iff Redis — the only engine without an engine-side action)", engine, tools.rotate != nil, wantRotate)
		}
	}
	for _, tpl := range dbtemplate.List() {
		t.Run(tpl.ID, func(t *testing.T) {
			tools, err := engineToolsFor(tpl)
			if err != nil {
				t.Fatalf("engineToolsFor(%s): %v", tpl.ID, err)
			}
			backupIn := dbtemplate.BackupInput{Instance: "dbx", TemplateID: tpl.ID}
			if _, err := backupJobScript(tpl, backupIn); err != nil {
				t.Fatalf("backupJobScript(%s): %v", tpl.ID, err)
			}
			outcome := dbtemplate.BackupOutcome{Instance: "dbx", TemplateID: tpl.ID, SnapshotID: "snap-x"}
			if _, err := verifyJobScript(tpl, outcome); err != nil {
				t.Fatalf("verifyJobScript(%s): %v", tpl.ID, err)
			}
			if _, err := pruneJobScript(tpl, outcome, 7); err != nil {
				t.Fatalf("pruneJobScript(%s): %v", tpl.ID, err)
			}
			restoreIn := dbtemplate.RestoreInput{Instance: "dbx", TemplateID: tpl.ID, VolumeTarget: tpl.VolumeMountPath, SnapshotID: "snap-x"}
			if _, err := tools.restoreScript(tpl, restoreIn); err != nil {
				t.Fatalf("restoreScript(%s): %v", tpl.ID, err)
			}
		})
	}
}

// mustTemplate 取注册表条目（测试夹具；失败即 Fatal）。
func mustTemplate(t *testing.T, id string) dbtemplate.Template {
	t.Helper()
	tpl, err := dbtemplate.Get(id)
	if err != nil {
		t.Fatalf("dbtemplate.Get(%s): %v", id, err)
	}
	return tpl
}
