package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
)

// 迁移体系（goose embed 模式）：SQL 迁移内嵌进二进制，进程启动（Store
// 打开）即应用，依赖零外部工具。迁移只加法——已应用文件禁止改写（内容
// sha256 由 migrations_test.go 的 golden 钉死），回滚 = 恢复快照、不写
// down migration（架构 §2.8 契约版本化纪律）。

//go:embed migrations/*.sql
var migrationsFS embed.FS

// gooseVersionTableName 是 goose 版本表名（默认值；实机验收以
// `SELECT * FROM goose_db_version` 观察已应用版本）。
const gooseVersionTableName = "goose_db_version"

// migrationFiles 返回内嵌迁移文件系统（goose provider 直接消费；
// fs.Sub 剥掉 migrations/ 目录前缀，goose 以平铺文件名解析版本号）。
func migrationFiles() (fs.FS, error) {
	return fs.Sub(migrationsFS, "migrations")
}

// maxEmbeddedMigration 解析内嵌迁移文件名的最大版本号（本二进制的已知
// 最大 schema 版本）。迁移链只加法，该值随二进制单调递增——高于它的库
// 一定出自更新版本的 fleetlyd。
func maxEmbeddedMigration() (int64, error) {
	fsys, err := migrationFiles()
	if err != nil {
		return 0, fmt.Errorf("state: open embedded migrations: %w", err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("state: read embedded migrations: %w", err)
	}
	var max int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lead, _, found := strings.Cut(e.Name(), "_")
		if !found {
			return 0, fmt.Errorf("state: migration %s: filename must be <version>_<name>.sql", e.Name())
		}
		v, err := strconv.ParseInt(lead, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("state: migration %s: parse leading version: %w", e.Name(), err)
		}
		if v > max {
			max = v
		}
	}
	return max, nil
}

// ensureMigrated 在 db 上应用全部未执行的迁移，返回迁移后的当前版本号
// （空库 → 最大版本；重复调用幂等，goose 版本表记录已应用版本）。
//
// 高版本守卫（升级回退错配整改③核心）：DB schema 版本 > 本二进制已知
// 最大迁移版本 → 拒绝打开。迁移只加法 + 回滚 = 恢复快照（架构 §2.8
// 契约）意味着旧二进制读不懂新 schema；静默 no-op 运行是「升级失败回退
// 只换二进制」路径上的错配陷阱——这里把错配变成显式失败，错误信息直接
// 给出可行动路径（见 docs/runbooks/upgrade.md §4）。
func ensureMigrated(ctx context.Context, db *sql.DB) (int64, error) {
	fsys, err := migrationFiles()
	if err != nil {
		return 0, fmt.Errorf("state: open embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return 0, fmt.Errorf("state: construct migration provider: %w", err)
	}
	// 空库（goose 自动建版本表并落 version 0）照常放行；有版本记录的库
	// 先对照本二进制的迁移天花板。
	dbVersion, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("state: read migration version: %w", err)
	}
	maxVersion, err := maxEmbeddedMigration()
	if err != nil {
		return 0, err
	}
	if dbVersion > maxVersion {
		return 0, fmt.Errorf(
			"state: database is from a newer version (schema %d is above the highest migration known to this binary, %d); "+
				"restore the state store from a backup snapshot before rolling the binary back, see docs/runbooks/backup-restore.md "+
				"(upgrade rollback semantics: docs/runbooks/upgrade.md)",
			dbVersion, maxVersion)
	}
	if _, err := provider.Up(ctx); err != nil {
		return 0, fmt.Errorf("state: apply migrations: %w", err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("state: read migration version: %w", err)
	}
	return version, nil
}

// migrationHashes 返回内嵌迁移的文件名 → 内容 sha256 映射（升序文件名）。
// 供只加法纪律测试使用：已应用迁移一旦被改写，哈希即失配。
func migrationHashes() (map[string]string, error) {
	fsys, err := migrationFiles()
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := fsys.Open(e.Name())
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		out[e.Name()] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}

// SchemaVersions 只读返回（DB 当前 schema 版本，本二进制内嵌迁移的最大
// 版本）。不开 Store、不应用迁移、不触发 ensureMigrated 的高版本守卫——
// 升级/回退编排（fleetlyd schema-version 子命令，F5/S20）需要在旧二进制
// 「拒绝打开新 schema 库」之前比对两侧版本，给操作员可行动指引或自动
// 恢复，而不是 start 失败后留下 DEGRADED 现场让人猜。
//
// 版本读取口径 = goose 版本表的 MAX(version_id)（本平台迁移只加法、不写
// down 行，MAX 即已应用最高版本）。库文件不存在 / 版本表不存在（从未被
// fleetlyd 打开过）→ dbVersion 0（空库语义，goose 首启建表落 0）。
func SchemaVersions(ctx context.Context, path string) (dbVersion, maxVersion int64, err error) {
	maxVersion, err = maxEmbeddedMigration()
	if err != nil {
		return 0, 0, err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return 0, maxVersion, nil
		}
		return 0, maxVersion, fmt.Errorf("state: stat %s: %w", path, statErr)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return 0, maxVersion, fmt.Errorf("state: open sqlite %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	var table string
	scanErr := db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?",
		gooseVersionTableName).Scan(&table)
	if scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return 0, maxVersion, nil
		}
		return 0, maxVersion, fmt.Errorf("state: inspect sqlite_master: %w", scanErr)
	}
	var v sql.NullInt64
	if qErr := db.QueryRowContext(ctx,
		"SELECT MAX(version_id) FROM "+gooseVersionTableName).Scan(&v); qErr != nil {
		return 0, maxVersion, fmt.Errorf("state: read %s: %w", gooseVersionTableName, qErr)
	}
	if !v.Valid {
		return 0, maxVersion, nil
	}
	return v.Int64, maxVersion, nil
}
