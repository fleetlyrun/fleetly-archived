package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// git_keys 表读写（T2.19）：平台管理的 git 公钥（SSH push 认证）。公钥
// 指纹（SHA256，ssh-keygen -lf 同格式）唯一；公钥本体为公开材料入库，
// 私钥永不经过平台。生命周期动作与审计同事务 fail-closed（state-model
// §2.9：管理操作必入审计）。
//
// 用户化（v0.3 W2，rbac-teams 设计 §2.3）：user_id（00018 建列）非空 =
// 登录用户自服务注册的 key，SSH push 认证回调按指纹取行后把署名用户带
// 给 gitserver（push 审计 actor = user:<id>）；NULL = 存量无主键（迁移
// 口径：只读展示归平台管理员全列，push 审计 actor 落原口径 system）。

// GitKey 是一条平台管理的 git 公钥行（只读投影；平台从不接触私钥）。
type GitKey struct {
	ID string
	// Fingerprint 是公钥 SHA256 指纹（"SHA256:<base64>"）。
	Fingerprint string
	// PublicKey 是 authorized_keys 单行原文（公开材料，可回读）。
	PublicKey string
	// KeyType 是公钥类型词（ssh-ed25519 / ssh-rsa / ecdsa-sha2-nistp256…）。
	KeyType string
	// Note 是人读备注。
	Note string
	// UserID 是属主用户（'' = 存量无主键——00018 之前注册的 key 不回填；
	// 非空 = 用户自服务 key，push 审计 actor 维度，设计 §2.3）。
	UserID    string
	CreatedAt time.Time
}

// git key 哨兵错误。
var (
	// ErrGitKeyNotFound 表示目标 git key 不存在。
	ErrGitKeyNotFound = errors.New("git key not found")
	// ErrGitKeyExists 表示同指纹公钥已在册。
	ErrGitKeyExists = errors.New("git key already registered")
)

// GitKeyWrite 是一次 git key 写入。
type GitKeyWrite struct {
	// ID 留空自动生成 ULID。
	ID string
	// Fingerprint 必须非空（SHA256 指纹，调用方以 golang.org/x/crypto/ssh
	// 的 FingerprintSHA256 计算后传入）。
	Fingerprint string
	// PublicKey 是 authorized_keys 单行原文。
	PublicKey string
	// KeyType 是公钥类型词。
	KeyType string
	// Note 是人读备注。
	Note string
	// ActorTokenID 是发起写入的调用方 token（审计 actor_token_id）。
	ActorTokenID string
	// UserID 是属主用户（'' = 无主——存量兼容形态；非空 = 用户自服务 key，
	// 设计 §2.3。存在性校验在上层，本层是纯写通道）。
	UserID string
	// ActorUserID 是发起写入的用户（审计 actor user:<id>；空 = human——
	// 存量兼容形态）。
	ActorUserID string
}

// gitKeyRowCols 是 git key 行查询列清单（列序 = scanGitKey 消费序）。
const gitKeyRowCols = `id, fingerprint, public_key, key_type, note, created_at, user_id`

// CreateGitKey 落一条 git key 行并与审计（gitkey.add）同事务 fail-closed。
// 同指纹重复注册返回 ErrGitKeyExists。
func (s *Store) CreateGitKey(ctx context.Context, w GitKeyWrite) (GitKey, error) {
	if w.Fingerprint == "" {
		return GitKey{}, fmt.Errorf("state: create git key: fingerprint is empty")
	}
	if w.PublicKey == "" {
		return GitKey{}, fmt.Errorf("state: create git key: public key is empty")
	}
	id := w.ID
	if id == "" {
		id = ulid.Make().String()
	}
	actor := auditActor(w.ActorUserID)
	var out GitKey
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		const q = `INSERT INTO git_keys (id, fingerprint, public_key, key_type, note, created_at, user_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, q, id, w.Fingerprint, w.PublicKey, w.KeyType, w.Note, now, nullableText(w.UserID)); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", ErrGitKeyExists, w.Fingerprint)
			}
			return fmt.Errorf("state: insert git key: %w", err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        actor,
			ActorTokenID: w.ActorTokenID,
			Action:       "gitkey.add",
			Target:       "gitkey:" + id,
			Result:       "ok",
			DiffSummary:  DiffSummary("fingerprint", w.Fingerprint, "key_type", w.KeyType), // B4：构造器替换手拼 JSON
		}); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `SELECT `+gitKeyRowCols+` FROM git_keys WHERE id = ?`, id)
		k, err := scanGitKey(row)
		if err != nil {
			return err
		}
		out = k
		return nil
	})
	if err != nil {
		return GitKey{}, err
	}
	return out, nil
}

// GetGitKey 按 id 取 key 行（吊销/删除前的归属判定查点）；不存在返回
// ErrGitKeyNotFound。
func (s *Store) GetGitKey(ctx context.Context, id string) (GitKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+gitKeyRowCols+` FROM git_keys WHERE id = ?`, id)
	return scanGitKey(row)
}

// GetGitKeyByFingerprint 按指纹取 key 行（SSH publickey 回调的认证查点，
// user_id 供 push 审计 actor 署名——设计 §2.3）；不存在返回
// ErrGitKeyNotFound。
func (s *Store) GetGitKeyByFingerprint(ctx context.Context, fingerprint string) (GitKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+gitKeyRowCols+` FROM git_keys WHERE fingerprint = ?`, fingerprint)
	return scanGitKey(row)
}

// ListGitKeys 返回全部在册 git key（created_at 升序；平台管理员/机具令牌
// 全列消费）。
func (s *Store) ListGitKeys(ctx context.Context) ([]GitKey, error) {
	return s.listGitKeysWhere(ctx, ``)
}

// ListGitKeysForUser 返回属主用户的在册 git key（created_at 升序；用户
// 自服务列表消费）。
func (s *Store) ListGitKeysForUser(ctx context.Context, userID string) ([]GitKey, error) {
	return s.listGitKeysWhere(ctx, ` WHERE user_id = ?`, userID)
}

// listGitKeysWhere 是 git key 列表的共享通道（extra 为空 = 全列；带占位
// 实参 = 按属主过滤）。
func (s *Store) listGitKeysWhere(ctx context.Context, where string, args ...any) ([]GitKey, error) {
	//nolint:gosec // G202：拼接的是列清单与固定 where 段，值全部走 ? 参数
	rows, err := s.db.QueryContext(ctx, `SELECT `+gitKeyRowCols+` FROM git_keys`+where+` ORDER BY created_at ASC, id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list git keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []GitKey
	for rows.Next() {
		k, err := scanGitKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate git keys: %w", err)
	}
	return out, nil
}

// RemoveGitKey 物理删除一条 git key（历史在审计可查）并与审计（gitkey.
// remove）同事务 fail-closed；不存在返回 ErrGitKeyNotFound。actorUserID
// 供审计主体归一（user:<id>；空 = human——机具令牌删除时由调用方传 ""）。
func (s *Store) RemoveGitKey(ctx context.Context, id, actorUserID, actorTokenID string) error {
	return s.InTx(ctx, func(tx *Tx) error {
		var fingerprint string
		err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM git_keys WHERE id = ?`, id).Scan(&fingerprint)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGitKeyNotFound
			}
			return fmt.Errorf("state: probe git key: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM git_keys WHERE id = ?`, id); err != nil {
			return fmt.Errorf("state: delete git key: %w", err)
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        auditActor(actorUserID),
			ActorTokenID: actorTokenID,
			Action:       "gitkey.remove",
			Target:       "gitkey:" + id,
			Result:       "ok",
			DiffSummary:  DiffSummary("fingerprint", fingerprint), // B4：构造器替换手拼 JSON
		})
	})
}

// scanGitKey 从单行构造 GitKey。
func scanGitKey(row interface{ Scan(dest ...any) error }) (GitKey, error) {
	var k GitKey
	var created int64
	var userID sql.NullString
	if err := row.Scan(&k.ID, &k.Fingerprint, &k.PublicKey, &k.KeyType, &k.Note, &created, &userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GitKey{}, ErrGitKeyNotFound
		}
		return GitKey{}, fmt.Errorf("state: scan git key: %w", err)
	}
	k.UserID = userID.String
	k.CreatedAt = time.Unix(0, created).UTC()
	return k, nil
}
