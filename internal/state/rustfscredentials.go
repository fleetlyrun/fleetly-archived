package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 托管 RustFS root 凭据的库内存取（E3-5/D-S3-7，设计 §2.5）：
//
// access key/secret key 平台生成（生成在调用方 internal/rustfs，
// crypto/rand）以 envelope 密文（internal/secrets）存 platform_settings
// 内部键 s3.rustfs_access_key / s3.rustfs_secret_key——**内部键**：不在
// SaveS3Settings 的 PUT 全量键集、不进 LoadS3Settings 的 typed 八键投影
//（用户设置面天然不见它；负面测试在本包钉死）。存取模式与
// resticpassword.go（s3.restic_password）同型。
//
// 生命周期语义（设计 §2.5）：凭据是**运行时配置不烙进数据**——禁用
// （mode 离开 rustfs）后凭据键随服务收敛清场（swarm secret 同步移除），
// 再启用时重新生成并复用数据卷；RustFS 侧以 _FILE 形态消费（swarm
// secret 文件挂载），数据卷内容与凭据零绑定。
//
// 裁决注记（resticpassword 同款）：生成/轮换**不写审计、不发事件**——
// 平台内部凭据生命周期，非用户可见变更；可观测面 = 调用方日志中的
// 指纹（sha256 前 8 hex，不含材料）。

// s3.rustfs_* 内部键词表（词表只增；键名常量唯一登记点）。
const (
	S3KeyRustfsAccessKey = "s3.rustfs_access_key" //nolint:gosec // G101：设置键名字面量，非凭据材料
	S3KeyRustfsSecretKey = "s3.rustfs_secret_key" //nolint:gosec // G101：设置键名字面量，非凭据材料
)

// LoadRustfsCredentialsCiphertext 读取已存的托管 RustFS 凭据密文。
// found=false = 尚未生成（管理器据此走生成路径；探针面据此如实报
// 「未备便」）。任一键存在而另一键缺失 = 存储损坏（loud-fail，不静默
// 回落再生成——半份密文可能是磁盘/库损坏的信号，覆盖写会掩盖现场）。
func (s *Store) LoadRustfsCredentialsCiphertext(ctx context.Context) (accessCT, secretCT string, found bool, err error) {
	var a, k string
	a, aErr := s.loadSettingRaw(ctx, S3KeyRustfsAccessKey)
	if aErr != nil {
		return "", "", false, aErr
	}
	k, kErr := s.loadSettingRaw(ctx, S3KeyRustfsSecretKey)
	if kErr != nil {
		return "", "", false, kErr
	}
	switch {
	case a == "" && k == "":
		return "", "", false, nil
	case a == "" || k == "":
		return "", "", false, fmt.Errorf(
			"state: rustfs credentials partially stored (%s present=%t, %s present=%t) — refusing to regenerate over a possibly corrupted store",
			S3KeyRustfsAccessKey, a != "", S3KeyRustfsSecretKey, k != "")
	}
	return a, k, true, nil
}

// SaveRustfsCredentialsCiphertext 落库凭据密文（单事务同时写两键；密文
// 入参——本层不解释密文，绝不把任何值拼进错误）。不写审计、不发事件
//（§2.5 裁决：内部凭据生命周期，审计 action 词表不扩）。
func (s *Store) SaveRustfsCredentialsCiphertext(ctx context.Context, accessCT, secretCT string) error {
	if accessCT == "" || secretCT == "" {
		return fmt.Errorf("state: save rustfs credentials: empty ciphertext (access=%t secret=%t)", accessCT != "", secretCT != "")
	}
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		for key, val := range map[string]string{
			S3KeyRustfsAccessKey: accessCT,
			S3KeyRustfsSecretKey: secretCT,
		} {
			const q = `INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
			if _, err := tx.ExecContext(ctx, q, key, val, now); err != nil {
				return fmt.Errorf("state: upsert %s: %w", key, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: save rustfs credentials: %w", err)
	}
	return nil
}

// DeleteRustfsCredentialsCiphertext 移除凭据密文（mode 离开 rustfs 的清场
// 路径，E3-5 管理器调用；幂等——键不存在即成功）。凭据是运行时配置：
// 清场后再启用走重新生成，数据卷内容与凭据零绑定（设计 §2.5）。
func (s *Store) DeleteRustfsCredentialsCiphertext(ctx context.Context) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		for _, key := range []string{S3KeyRustfsAccessKey, S3KeyRustfsSecretKey} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM platform_settings WHERE key = ?`, key); err != nil {
				return fmt.Errorf("state: delete %s: %w", key, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: delete rustfs credentials: %w", err)
	}
	return nil
}

// loadSettingRaw 读单条 platform_settings 值（缺行 = 空串非错误——「未
// 生成」是合法态，与 typed 投影的缺省语义一致）。
func (s *Store) loadSettingRaw(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM platform_settings WHERE key = ?`, key).Scan(&v)
	if err == nil {
		return v, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return "", fmt.Errorf("state: load %s: %w", key, err)
}
