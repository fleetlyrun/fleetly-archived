package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// app_secrets 读写（managed-databases 设计 §2.7，D-DB-7）：平台密钥库——
// compose external secret 的唯一来源，开放面 = 所有 app。value_cipher 是
// age 密文（调用方经 internal/secrets envelope 加密后传入——本层不解释
// 密文/明文，同 env_vars 口径）；hash8 = 值 sha256 前 8 位（naming.Hash8
// ——值轮换即换 Swarm secret 名换引用，轮换天然触发重部署）。**无值读回
// 面**：忘记即轮换——本通道只提供密文装载（planner 构造 SecretMount 用
// 密文经 secret 管道投递），明文永不回 API/日志/事件。

// ErrAppSecretNotFound 表示目标 secret 不存在（compose 声明名在库中缺失
// → 部署 preflight E_SECRET_NOT_FOUND 的映射源，api 层 S2/S4 承载）。
var ErrAppSecretNotFound = errors.New("app secret not found")

// AppSecret 是一行平台密钥库条目。
type AppSecret struct {
	ID string
	// AppID 是归属 app 平台 ID。
	AppID string
	// Name 是声明名（compose secrets 短名；UNIQUE (app_id, name)）。
	Name string
	// ValueCipher 是 age 密文（明文永不进本结构之外的任何面）。
	ValueCipher string
	// Hash8 是明文值 sha256 前 8 位（引用比对/换名轮换锚——不泄露值）。
	Hash8     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertAppSecret 写入（覆盖即轮换：cipher 与 hash8 同拍重盖，updated_at
// 恒重盖；Swarm secret 换名由调用方按新 hash8 经 naming.SecretName 承载）。
func (s *Store) UpsertAppSecret(ctx context.Context, sec AppSecret) (AppSecret, error) {
	if err := validateAppSecret(sec); err != nil {
		return AppSecret{}, err
	}
	var out AppSecret
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		const q = `INSERT INTO app_secrets (id, app_id, name, value_cipher, hash8, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(app_id, name) DO UPDATE SET
				value_cipher = excluded.value_cipher,
				hash8 = excluded.hash8,
				updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, q,
			ulid.Make().String(), sec.AppID, sec.Name, sec.ValueCipher, sec.Hash8, now, now); err != nil {
			return fmt.Errorf("state: upsert app secret %s: %w", sec.Name, err)
		}
		row, err := tx.GetAppSecret(ctx, sec.AppID, sec.Name)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return AppSecret{}, err
	}
	return out, nil
}

// UpsertAppSecret 是事务内写入（供与审计/事件同事务组合）。
func (t *Tx) UpsertAppSecret(ctx context.Context, sec AppSecret) error {
	if err := validateAppSecret(sec); err != nil {
		return err
	}
	now := nowNano()
	const q = `INSERT INTO app_secrets (id, app_id, name, value_cipher, hash8, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id, name) DO UPDATE SET
			value_cipher = excluded.value_cipher,
			hash8 = excluded.hash8,
			updated_at = excluded.updated_at`
	if _, err := t.ExecContext(ctx, q,
		ulid.Make().String(), sec.AppID, sec.Name, sec.ValueCipher, sec.Hash8, now, now); err != nil {
		return fmt.Errorf("state: upsert app secret %s: %w", sec.Name, err)
	}
	return nil
}

// validateAppSecret 是写入通道校验（密文与 hash8 必填——hash8 由调用方
// 经 naming.Hash8(明文) 计算；明文本身不经过本层）。
func validateAppSecret(sec AppSecret) error {
	if sec.AppID == "" {
		return errors.New("state: app secret requires app_id")
	}
	if sec.Name == "" {
		return errors.New("state: app secret requires name")
	}
	if sec.ValueCipher == "" {
		return errors.New("state: app secret requires value cipher")
	}
	if sec.Hash8 == "" {
		return errors.New("state: app secret requires hash8")
	}
	return nil
}

// GetAppSecretCipher 取密文装载形态（planner 构造 SecretMount 的凭据源；
// 不存在返回 ErrAppSecretNotFound）。无明文读回面——返回的只有密文。
func (s *Store) GetAppSecretCipher(ctx context.Context, appID, name string) (string, error) {
	row, err := s.GetAppSecret(ctx, appID, name)
	if err != nil {
		return "", err
	}
	return row.ValueCipher, nil
}

// GetAppSecret 取整行（hash8 供引用比对）。不存在返回 ErrAppSecretNotFound。
func (s *Store) GetAppSecret(ctx context.Context, appID, name string) (AppSecret, error) {
	const q = `SELECT ` + appSecretScanCols + ` FROM app_secrets WHERE app_id = ? AND name = ?`
	return scanAppSecret(s.db.QueryRowContext(ctx, q, appID, name))
}

// GetAppSecret 是事务内取整行（写后回读）。
func (t *Tx) GetAppSecret(ctx context.Context, appID, name string) (AppSecret, error) {
	const q = `SELECT ` + appSecretScanCols + ` FROM app_secrets WHERE app_id = ? AND name = ?`
	return scanAppSecret(t.QueryRowContext(ctx, q, appID, name))
}

// ListAppSecrets 返回该 app 全部 secret（按 name 字典序；list 面只投影
// 名称/时间锚——密文列仅装载给凭据管道消费方）。
func (s *Store) ListAppSecrets(ctx context.Context, appID string) ([]AppSecret, error) {
	const q = `SELECT ` + appSecretScanCols + ` FROM app_secrets WHERE app_id = ? ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q, appID)
	if err != nil {
		return nil, fmt.Errorf("state: query app secrets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AppSecret
	for rows.Next() {
		sec, err := scanAppSecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate app secrets: %w", err)
	}
	return out, nil
}

// RemoveAppSecret 删除单条（幂等：不存在返回 ErrAppSecretNotFound）。
func (s *Store) RemoveAppSecret(ctx context.Context, appID, name string) error {
	return s.InTx(ctx, func(tx *Tx) error {
		return tx.RemoveAppSecret(ctx, appID, name)
	})
}

// RemoveAppSecret 是事务内删除（供与审计同事务 fail-closed 组合——api 面
// 「审计黑洞零容忍」纪律的密钥库行写点；不存在返回 ErrAppSecretNotFound）。
func (t *Tx) RemoveAppSecret(ctx context.Context, appID, name string) error {
	res, err := t.ExecContext(ctx, `DELETE FROM app_secrets WHERE app_id = ? AND name = ?`, appID, name)
	if err != nil {
		return fmt.Errorf("state: delete app secret %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read app secret delete count: %w", err)
	}
	if n == 0 {
		return ErrAppSecretNotFound
	}
	return nil
}

// appSecretScanCols 是密钥库行查询列清单（新增列只加在此与扫描函数）。
const appSecretScanCols = `id, app_id, name, value_cipher, hash8, created_at, updated_at` //nolint:gosec // G101：SQL 列名清单，非凭据

// scanAppSecret 从单行构造 AppSecret。
func scanAppSecret(row interface{ Scan(dest ...any) error }) (AppSecret, error) {
	var sec AppSecret
	var created, updated int64
	if err := row.Scan(&sec.ID, &sec.AppID, &sec.Name, &sec.ValueCipher, &sec.Hash8, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AppSecret{}, ErrAppSecretNotFound
		}
		return AppSecret{}, fmt.Errorf("state: scan app secret: %w", err)
	}
	sec.CreatedAt = time.Unix(0, created).UTC()
	sec.UpdatedAt = time.Unix(0, updated).UTC()
	return sec, nil
}
