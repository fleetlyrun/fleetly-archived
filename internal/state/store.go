package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（无 CGO；CI linux 无 CGO 环境必需）
)

// Store 是控制面状态层门面：SQLite 权威态存储 + 底座观测缓存写入 +
// 写前直读端口消费。零框架依赖——lynx.Service/Checker 装配壳在
// cmd/fleetlyd（CheckHealth 为结构性实现，进 healthz readiness）。
type Store struct {
	db   *sql.DB
	path string
}

// sqliteBusyTimeout / journalMode / synchronous 是连接级 PRAGMA 取值：
//   - busy_timeout：WAL 单写者下写竞争等待 5s（默认 0 立即 SQLITE_BUSY）；
//   - journal_mode=WAL：备份一致性快照（VACUUM INTO）与读写并行的前提；
//   - foreign_keys：核心表引用完整性（apps ← deployments/…）；
//   - synchronous=NORMAL：WAL 推荐档（进程崩溃不丢，掉电窗口由备份兜底）。
const (
	sqliteBusyTimeout = 5000
)

// dsn 把裸路径转换为带连接参数的 SQLite DSN：modernc 驱动按第一个 `?`
// 切分文件名与查询参数（Windows 盘符路径原样可用）。_txlock=immediate 使
// 所有事务以 BEGIN IMMEDIATE 开启——SQLite 在 WAL 下「读升级写」不经过
// busy handler，deferred 事务并发升级会立即 SQLITE_BUSY，故写事务必须
// 起手即取写锁。
func dsn(path string) string {
	return path + "?_txlock=immediate&_busy_timeout=" + strconv.Itoa(sqliteBusyTimeout) +
		"&_journal_mode=WAL&_foreign_keys=1&_synchronous=NORMAL"
}

// Open 打开（必要时创建）状态库并应用全部迁移：启动即建库迁移、失败
// fail-fast 拒绝启动。path 为数据库文件路径（默认 ./fleetly.db，由
// config state.db_path 提供）。
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("state: db path is empty")
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("state: open sqlite %s: %w", path, err)
	}
	// modernc 驱动无空闲连接上限问题；上限放开让 database/sql 自行管理。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state: ping sqlite %s: %w", path, err)
	}
	if _, err := ensureMigrated(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// Close 关闭连接池。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// DeploymentsRoot 返回部署 compose 持久化根目录（S18-A7：<数据根>/
// deployments；数据根 = 状态库同目录——与 cmd 装配的 BackupRoot/GitRoot
// 同款派生，入队点（api/gitserver）与 janitor 清理共用同一布局）。
func DeploymentsRoot(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "deployments")
}

// BuildUploadsRoot 返回上传构建会话根目录（IMPL-T2-2/DT-6：<数据根>/
// build-uploads；数据根 = 状态库同目录，与 DeploymentsRoot 同款派生——
// api 入队点、builder 清理钩子与 janitor 兜底清扫共用同一布局）。
func BuildUploadsRoot(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "build-uploads")
}

// PersistDeploymentCompose 把部署 compose 字节持久化到
// <root>/<deploymentID>/compose.yaml（S18-A7 入队点共享写入通道：先写
// 文件后建行——部署行不指向缺失文件；0600 权限；OS 临时目录自此仅作解析
// 中转，tmpfiles 清理不再影响引擎 preparing 重载与成功固化重载）。
func PersistDeploymentCompose(root, deploymentID string, compose []byte) (string, error) {
	dir := filepath.Join(root, deploymentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("state: mkdir deployment compose dir: %w", err)
	}
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, compose, 0o600); err != nil {
		return "", fmt.Errorf("state: write deployment compose: %w", err)
	}
	return path, nil
}

// Ping 探测数据库可用性。
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("state: db ping: %w", err)
	}
	return nil
}

// CheckHealth 实现 lynx.Checker（结构性满足，本包不 import 框架）：
// 数据库可达即健康。检查自带 2s 上限（readiness 探测挂死防护由框架
// 另有 3s 兜底，此处自查优先快速失败）。
func (s *Store) CheckHealth() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.Ping(ctx)
}

// InTx 在单个写事务内执行 fn：事务以 BEGIN IMMEDIATE 开启（见 dsn），
// fn 返回错误或 panic 均回滚（panic 回滚后原样重抛，不吞栈）——审计/
// 事件与业务写共享同一事务即由此组合（fail-closed）。
func (s *Store) InTx(ctx context.Context, fn func(tx *Tx) error) (err error) {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin tx: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = sqlTx.Rollback() // panic 路径必须释放事务连接（Windows 句柄/WAL 锁）
			panic(r)
		}
	}()
	t := &Tx{Tx: sqlTx}
	if err := fn(t); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("state: commit tx: %w", err)
	}
	return nil
}

// Tx 是写事务句柄：业务写 + 审计 + 事件在同一事务内组合。
type Tx struct {
	*sql.Tx
}

// nowNano 统一取 UTC UnixNano（时间列约定）。
func nowNano() int64 { return time.Now().UTC().UnixNano() }
