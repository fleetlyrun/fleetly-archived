package state

// app_configs 读写（T 线 OT-3 / IMPL-T1-4 Config 资源）：app 级明文配置
// 资源——compose configs 声明的唯一来源。value 是明文（设计 OT-3：明文、
// 版本化、审计、可回读；与 app_secrets 的 age 密文相反——本表就是配置面
// 的可读值，回读走 admin 门 GetConfig）；hash8 = 值 sha256 前 8（naming.
// Hash8——内容寻址的 swarm config 对象名尾缀与引用比对锚）。
//
// 换版语义：同名 Upsert 重盖 value/hash8/updated_at（compose 侧引用名不变，
// 内容变更 → hash8 变 → swarm config 对象名变 → 服务引用换版并滚动；旧
// 对象由发布对账/删除 reap 回收，见 internal/engine/configinject.go）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// ErrAppConfigNotFound 表示目标 config 不存在（compose 声明名在库中缺失
// → 部署 preflight E_CONFIG_NOT_FOUND 的映射源，api 层承载）。
var ErrAppConfigNotFound = errors.New("app config not found")

// AppConfig 是一行 app 级配置资源。
type AppConfig struct {
	ID string
	// AppID 是归属 app 平台 ID。
	AppID string
	// Name 是声明名（compose configs 短名；UNIQUE (app_id, name)）。
	Name string
	// Value 是配置内容明文（OT-3 明文资源——不加密，回读走 admin 门）。
	Value string
	// Hash8 是内容 sha256 前 8（swarm config 对象名的内容寻址尾缀与引用
	// 比对锚；内容变更即换版换引用）。
	Hash8     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertAppConfig 写入（覆盖即换版：value 与 hash8 同拍重盖，updated_at
// 恒重盖；Swarm config 换名由调用方按新 hash8 经 naming.ConfigName 承载）。
func (s *Store) UpsertAppConfig(ctx context.Context, cfg AppConfig) (AppConfig, error) {
	if err := validateAppConfig(cfg); err != nil {
		return AppConfig{}, err
	}
	var out AppConfig
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		const q = `INSERT INTO app_configs (id, app_id, name, value, hash8, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(app_id, name) DO UPDATE SET
				value = excluded.value,
				hash8 = excluded.hash8,
				updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, q,
			ulid.Make().String(), cfg.AppID, cfg.Name, cfg.Value, cfg.Hash8, now, now); err != nil {
			return fmt.Errorf("state: upsert app config %s: %w", cfg.Name, err)
		}
		row, err := tx.GetAppConfig(ctx, cfg.AppID, cfg.Name)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return AppConfig{}, err
	}
	return out, nil
}

// UpsertAppConfig 是事务内写入（供与审计/事件同事务组合）。
func (t *Tx) UpsertAppConfig(ctx context.Context, cfg AppConfig) error {
	if err := validateAppConfig(cfg); err != nil {
		return err
	}
	now := nowNano()
	const q = `INSERT INTO app_configs (id, app_id, name, value, hash8, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id, name) DO UPDATE SET
			value = excluded.value,
			hash8 = excluded.hash8,
			updated_at = excluded.updated_at`
	if _, err := t.ExecContext(ctx, q,
		ulid.Make().String(), cfg.AppID, cfg.Name, cfg.Value, cfg.Hash8, now, now); err != nil {
		return fmt.Errorf("state: upsert app config %s: %w", cfg.Name, err)
	}
	return nil
}

// validateAppConfig 是写入通道校验（值与 hash8 必填；hash8 由调用方经
// naming.Hash8(明文) 计算——与 app_secrets 写通道同纪律）。
func validateAppConfig(cfg AppConfig) error {
	if cfg.AppID == "" {
		return errors.New("state: app config requires app_id")
	}
	if cfg.Name == "" {
		return errors.New("state: app config requires name")
	}
	if cfg.Value == "" {
		return errors.New("state: app config requires value")
	}
	if cfg.Hash8 == "" {
		return errors.New("state: app config requires hash8")
	}
	return nil
}

// GetAppConfig 取整行（存在性哨兵与回读的装载形态）。不存在返回
// ErrAppConfigNotFound。
func (s *Store) GetAppConfig(ctx context.Context, appID, name string) (AppConfig, error) {
	const q = `SELECT ` + appConfigScanCols + ` FROM app_configs WHERE app_id = ? AND name = ?`
	return scanAppConfig(s.db.QueryRowContext(ctx, q, appID, name))
}

// GetAppConfig 是事务内取整行（写后回读）。
func (t *Tx) GetAppConfig(ctx context.Context, appID, name string) (AppConfig, error) {
	const q = `SELECT ` + appConfigScanCols + ` FROM app_configs WHERE app_id = ? AND name = ?`
	return scanAppConfig(t.QueryRowContext(ctx, q, appID, name))
}

// ListAppConfigs 返回该 app 全部 config（按 name 字典序；list 面只投影
// 名称/指纹/时间锚——值只在 GetConfig 的 admin 回读路径出现）。
func (s *Store) ListAppConfigs(ctx context.Context, appID string) ([]AppConfig, error) {
	const q = `SELECT ` + appConfigScanCols + ` FROM app_configs WHERE app_id = ? ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q, appID)
	if err != nil {
		return nil, fmt.Errorf("state: query app configs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AppConfig
	for rows.Next() {
		cfg, err := scanAppConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate app configs: %w", err)
	}
	return out, nil
}

// RemoveAppConfig 删除单条（幂等：不存在返回 ErrAppConfigNotFound）。
func (s *Store) RemoveAppConfig(ctx context.Context, appID, name string) error {
	return s.InTx(ctx, func(tx *Tx) error {
		return tx.RemoveAppConfig(ctx, appID, name)
	})
}

// RemoveAppConfig 是事务内删除（供与审计同事务 fail-closed 组合；不存在
// 返回 ErrAppConfigNotFound——remove API 的 404 映射源）。
func (t *Tx) RemoveAppConfig(ctx context.Context, appID, name string) error {
	res, err := t.ExecContext(ctx, `DELETE FROM app_configs WHERE app_id = ? AND name = ?`, appID, name)
	if err != nil {
		return fmt.Errorf("state: delete app config %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read app config delete count: %w", err)
	}
	if n == 0 {
		return ErrAppConfigNotFound
	}
	return nil
}

// appConfigScanCols 是配置资源行查询列清单（新增列只加在此与扫描函数）。
const appConfigScanCols = `id, app_id, name, value, hash8, created_at, updated_at`

// scanAppConfig 从单行构造 AppConfig。
func scanAppConfig(row interface{ Scan(dest ...any) error }) (AppConfig, error) {
	var cfg AppConfig
	var created, updated int64
	if err := row.Scan(&cfg.ID, &cfg.AppID, &cfg.Name, &cfg.Value, &cfg.Hash8, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AppConfig{}, ErrAppConfigNotFound
		}
		return AppConfig{}, fmt.Errorf("state: scan app config: %w", err)
	}
	cfg.CreatedAt = time.Unix(0, created).UTC()
	cfg.UpdatedAt = time.Unix(0, updated).UTC()
	return cfg, nil
}
