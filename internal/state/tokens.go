package state

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// tokens 表读写（T2.17；state-model §2.9）：哈希存储，明文永不落库。
// 调用方（internal/api）生成明文 token 并计算 sha256 hex 落库；本层提供
// 建/列/吊销/哈希认证原语与 fail-closed 审计（token 生命周期动作必入审计）。
//
// scope 词表（read/deploy/admin）不进数据库约束——scopes 列存逗号分隔词表，
// 语义与蕴含判定（admin ⊃ deploy ⊃ read）在 internal/api 承载；本层只做
// 非空防御。
//
// 用户化扩展（v0.3 W1 建列，RBAC 设计 §2.3）：user_id（NULL = 平台机具
// 令牌——平台级凭据的设计语义；非空 = 用户 PAT）与 project_id（NULL =
// 不绑定）加列后，CreateToken/ListTokens 投影与 AuthenticateToken 认证
// 原语带出两列；PAT 认证联动属主禁用态（AuthenticateToken 内 users JOIN，
// disabled_at 非空 → ErrTokenInvalid 同码拒认）。scopes 语义迁移（用户
// 自服务面）与角色双门是 W2 API 票面，不在本层。

// Token 是一条 API token 行（只读投影；明文不存在于任何通道）。
type Token struct {
	ID string
	// TokenHash 是明文 token 的 sha256 hex（64 字符）。
	TokenHash string
	// Name 是人读备注（proto 面 note；建行时落）。
	Name string
	// Scopes 是逗号分隔 scope 词表（如 "read,deploy"）。
	Scopes string
	// HashPrefix 是 TokenHash 前 12 hex（识别用，非凭据）。
	HashPrefix string
	// UserID 是属主用户（'' = 平台机具令牌——设计 §2.3：平台管理员显式
	// 创建的平台级凭据；非空 = 用户 PAT，认证路径联动用户禁用态）。
	UserID string
	// ProjectID 是绑定项目（'' = 不绑定；role 双门的收窄维度，设计 §2.3）。
	ProjectID string
	CreatedAt time.Time
	// LastUsedAt 零值 = 从未使用。
	LastUsedAt time.Time
	// RevokedAt 零值 = 在册。
	RevokedAt time.Time
}

// BootstrapTokenName 是首启 bootstrap admin token 的人读备注（note 列值，
// internal/runtime 种子通道与本常量同源——识别 bootstrap 的唯一面：name
// 精确匹配 + 机具令牌（user_id IS NULL）+ 在册（revoked_at IS NULL）。
// 首用户注册事务按此定位自动吊销（设计 §2.3：桥梁凭据，目的达成即死）。
const BootstrapTokenName = "bootstrap admin (initial install)"

// tokRowCols 是 token 行查询列清单（命名避开 "token" 前缀——gosec G101
// 对凭据样常量名误报；列清单本身非凭据）。
const tokRowCols = `id, token_hash, name, scopes, created_at, last_used_at, revoked_at, user_id, project_id`

// token 相关哨兵错误。
var (
	// ErrTokenNotFound 表示目标 token 不存在。
	ErrTokenNotFound = errors.New("token not found")
	// ErrTokenRevoked 表示 token 已吊销（认证拒绝路径）。
	ErrTokenRevoked = errors.New("token revoked")
	// ErrTokenInvalid 表示凭据不匹配（哈希查无此行或常量时间比对失败）。
	ErrTokenInvalid = errors.New("token invalid")
	// ErrTokenLastAdmin 表示最后管理员守卫拒绝（M4-6）：本次吊销会使
	// 平台不存在任何未吊销 admin token——依次吊销全部 admin 后平台锁死
	// （重启不补种 bootstrap：HasAnyToken 已见 token 行，一次性语义），
	// 在吊销事务内拒绝。调用方映射为 409 业务冲突信封。
	ErrTokenLastAdmin = errors.New("last admin token")
)

// HashToken 计算明文 token 的存储哈希（sha256 hex，64 字符）——落库与认证
// 双方共用此形态，防止出现两种哈希口径。
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// TokenWrite 是一次 token 写入。
type TokenWrite struct {
	// ID 留空自动生成 ULID。
	ID string
	// Hash 必须是 HashToken 产物形态（64 hex sha256）；非法形态拒绝
	//（防御性，防旁路写入明文）。
	Hash string
	// Name 是人读备注（proto 面 note）。
	Name string
	// Scopes 是逗号分隔 scope 词表（调用方已归一，如 "read,deploy"）。
	Scopes string
	// ActorTokenID 是发起写入的调用方 token（可空——bootstrap 种子无调用
	// 方 token）；入审计 fail-closed。
	ActorTokenID string
	// Actor 是审计主体（human/ai_agent/system；空回落 human——系统代签发
	// 的 token 如 git push 钩子回调 token 传 system，T2.19）。
	Actor string
	// UserID 是属主用户（'' = 平台机具令牌；非空 = 用户 PAT——设计 §2.3。
	// 存在性校验在上层，本层是纯写通道）。
	UserID string
	// ProjectID 是绑定项目（'' = 不绑定）。
	ProjectID string
}

// CreateToken 在事务内落一条 token 行（哈希形态入参）并与审计同事务
// fail-closed。
func (s *Store) CreateToken(ctx context.Context, w TokenWrite) (Token, error) {
	if len(w.Hash) != 64 {
		return Token{}, fmt.Errorf("state: create token: hash must be 64-char sha256 hex, got %d chars", len(w.Hash))
	}
	if w.Scopes == "" {
		return Token{}, fmt.Errorf("state: create token: scopes is empty")
	}
	id := w.ID
	if id == "" {
		id = ulid.Make().String()
	}
	actor := w.Actor
	if actor == "" {
		actor = "human"
	}
	var out Token
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		const q = `INSERT INTO tokens (id, token_hash, name, scopes, created_at, user_id, project_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, q, id, w.Hash, w.Name, w.Scopes, now, nullableText(w.UserID), nullableText(w.ProjectID)); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("state: create token: duplicate hash (token already exists)")
			}
			return fmt.Errorf("state: insert token: %w", err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        actor,
			ActorTokenID: w.ActorTokenID,
			Action:       "token.create",
			Target:       "token:" + id,
			Result:       "ok",
			DiffSummary:  DiffSummary("scopes", w.Scopes), // MG-6：构造器替换手拼 JSON
		}); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `SELECT `+tokRowCols+` FROM tokens WHERE id = ?`, id)
		t, err := scanToken(row)
		if err != nil {
			return err
		}
		out = t
		return nil
	})
	if err != nil {
		return Token{}, err
	}
	return out, nil
}

// ListTokens 返回全部在册（未吊销）token（created_at 升序；平台管理员/
// 机具令牌全列消费）。
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	return s.listTokensWhere(ctx, ``)
}

// ListTokensForUser 返回属主用户的在册 token（created_at 升序；用户自
// 服务列表消费——设计 §2.3：登录用户只看自己的 PAT）。
func (s *Store) ListTokensForUser(ctx context.Context, userID string) ([]Token, error) {
	return s.listTokensWhere(ctx, ` AND user_id = ?`, userID)
}

// listTokensWhere 是 token 列表的共享通道（extra 空 = 全列；带占位实参 =
// 按属主过滤）。
func (s *Store) listTokensWhere(ctx context.Context, cond string, args ...any) ([]Token, error) {
	//nolint:gosec // G202：拼接的是列清单与固定 cond 段，值全部走 ? 参数
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokRowCols+` FROM tokens WHERE revoked_at IS NULL`+cond+` ORDER BY created_at ASC, id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate tokens: %w", err)
	}
	return out, nil
}

// GetToken 按 id 取 token 行（吊销前的归属判定查点；无敏感面——投影含
// 哈希，调用方不得外带）。不存在返回 ErrTokenNotFound。
func (s *Store) GetToken(ctx context.Context, id string) (Token, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+tokRowCols+` FROM tokens WHERE id = ?`, id)
	return scanToken(row)
}

// HasAnyToken 报告是否已存在任意 token 行（含已吊销）——bootstrap 判定
// 「首启无任何 token」的谓词（吊销过的也算存在过：bootstrap 一次性语义）。
func (s *Store) HasAnyToken(ctx context.Context) (bool, error) {
	const q = `SELECT COUNT(1) FROM tokens`
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return false, fmt.Errorf("state: count tokens: %w", err)
	}
	return n > 0, nil
}

// AuthenticateToken 按明文认证：哈希查行 → 常量时间二次比对（belt-and-
// suspenders：行查找按哈希等值走索引，不泄漏明文时序；二次比对保证比对
// 通道本身常量时间）→ 吊销检查 → 属主禁用检查（LEFT JOIN users：user PAT
// 的属主 disabled_at 非空 → ErrTokenInvalid 同码拒认——设计 §2.1/§2.3
// 禁用联动，不泄漏存在性；机具令牌 user NULL 无属主行，不触发本检查）。
// 成功返回在册 token。**不盖 last_used_at**（S18-A2：每请求同步写是 SQLite
// 写放大——盖写职责上移到调用方 internal/api 的认证路径，经进程内节流后
// 调 TouchTokenUsed），本层保持纯认证语义。
func (s *Store) AuthenticateToken(ctx context.Context, plaintext string) (Token, error) {
	hash := HashToken(plaintext)
	// 列清单带表别名限定（JOIN users 后 id 等列名有歧义；列序与
	// tokRowCols 一致）。
	row := s.db.QueryRowContext(ctx, `SELECT t.id, t.token_hash, t.name, t.scopes, t.created_at, t.last_used_at, t.revoked_at, t.user_id, t.project_id, u.disabled_at
		FROM tokens t LEFT JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?`, hash)
	var t Token
	var ownerDisabled sql.NullInt64
	if err := scanTokenInto(row, &t, &ownerDisabled); err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			return Token{}, ErrTokenInvalid
		}
		return Token{}, err
	}
	if subtle.ConstantTimeCompare([]byte(t.TokenHash), []byte(hash)) != 1 {
		return Token{}, ErrTokenInvalid
	}
	if !t.RevokedAt.IsZero() {
		return Token{}, ErrTokenRevoked
	}
	if ownerDisabled.Valid {
		// 属主用户已禁用：PAT 联动拒认（ErrTokenInvalid 同码——与查无此
		// token 不可区分，不泄漏账号状态）。
		return Token{}, ErrTokenInvalid
	}
	return t, nil
}

// TouchTokenUsed 刷新 last_used_at（尽力而为观测面；token 已消失时静默）。
func (s *Store) TouchTokenUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, nowNano(), id)
	if err != nil {
		return fmt.Errorf("state: touch token used: %w", err)
	}
	return nil
}

// RevokeToken 吊销 token（幂等：已吊销返回成功；不存在返回
// ErrTokenNotFound）。与审计同事务 fail-closed。
func (s *Store) RevokeToken(ctx context.Context, id, actorTokenID string) error {
	return s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		res, err := tx.ExecContext(ctx,
			`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, id)
		if err != nil {
			return fmt.Errorf("state: revoke token: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read revoke count: %w", err)
		}
		if n == 0 {
			// 区分「已吊销（幂等成功）」与「不存在」。
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM tokens WHERE id = ?`, id).Scan(&one)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrTokenNotFound
				}
				return fmt.Errorf("state: probe token: %w", err)
			}
			return nil // 已吊销：幂等成功
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        "human",
			ActorTokenID: actorTokenID,
			Action:       "token.revoke",
			Target:       "token:" + id,
			Result:       "ok",
		})
	})
}

// RevokeTokenGuardLastAdmin 是带最后管理员守卫的吊销（M4-6）：吊销生效后
// 仍须存在 ≥1 枚未吊销 admin token，否则整笔回滚返回 ErrTokenLastAdmin
// （幂等面与 RevokeToken 一致：目标已吊销 = 幂等成功，不触发守卫——没有
// 新的吊销发生；不存在仍 ErrTokenNotFound）。守卫判定与吊销在同一事务内
// （先 UPDATE 再复查剩余在册行，不满足即返回错误回滚），消除「检查与吊销
// 分离」的竞态窗。isAdmin 由调用方注入（scope 蕴含判定 admin ⊃ deploy ⊃
// read 属 api 层语义，本层不解释 scopes 词表——本层只提供原子性）。
func (s *Store) RevokeTokenGuardLastAdmin(ctx context.Context, id, actorTokenID string, isAdmin func(scopes string) bool) error {
	return s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		res, err := tx.ExecContext(ctx,
			`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, id)
		if err != nil {
			return fmt.Errorf("state: revoke token: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read revoke count: %w", err)
		}
		if n == 0 {
			// 区分「已吊销（幂等成功）」与「不存在」（与 RevokeToken 同形）。
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM tokens WHERE id = ?`, id).Scan(&one)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrTokenNotFound
				}
				return fmt.Errorf("state: probe token: %w", err)
			}
			return nil // 已吊销：幂等成功（无新吊销，守卫不适用）
		}
		// 守卫复查：吊销已生效（同事务可见），清点剩余在册行中是否仍有
		// admin scope。查询失败同样回滚（fail-closed：无法证明安全即拒绝
		// 吊销，而不是放行后可能锁死平台）。rows 在审计写之前显式关闭
		//（Tx 单连接：开放游标上的后续 Exec 会与游标交错）。
		rows, err := tx.QueryContext(ctx, `SELECT scopes FROM tokens WHERE revoked_at IS NULL`)
		if err != nil {
			return fmt.Errorf("state: scan remaining tokens for last-admin guard: %w", err)
		}
		hasAdmin := false
		for rows.Next() {
			var scopes string
			if err := rows.Scan(&scopes); err != nil {
				_ = rows.Close()
				return fmt.Errorf("state: scan remaining token scopes: %w", err)
			}
			if isAdmin(scopes) {
				hasAdmin = true
				break
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("state: iterate remaining tokens: %w", err)
		}
		_ = rows.Close()
		if !hasAdmin {
			return ErrTokenLastAdmin // 事务回滚：吊销不生效
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        "human",
			ActorTokenID: actorTokenID,
			Action:       "token.revoke",
			Target:       "token:" + id,
			Result:       "ok",
		})
	})
}

// nullableText 是 '' ↔ SQL NULL 的写入口径（'' = 列不归属，落 NULL）。
func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// scanToken 从单行构造 Token。
func scanToken(row interface{ Scan(dest ...any) error }) (Token, error) {
	var t Token
	if err := scanTokenInto(row, &t); err != nil {
		return Token{}, err
	}
	return t, nil
}

// scanTokenInto 把 token 行列扫入 t（extra 追加列由调用方提供，如
// AuthenticateToken 的属主禁用列）。
func scanTokenInto(row interface{ Scan(dest ...any) error }, t *Token, extra ...any) error {
	var lastUsed, revoked sql.NullInt64
	var userID, projectID sql.NullString
	var created int64
	dest := []any{&t.ID, &t.TokenHash, &t.Name, &t.Scopes, &created, &lastUsed, &revoked, &userID, &projectID}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTokenNotFound
		}
		return fmt.Errorf("state: scan token: %w", err)
	}
	t.UserID = userID.String
	t.ProjectID = projectID.String
	t.CreatedAt = time.Unix(0, created).UTC()
	if lastUsed.Valid {
		t.LastUsedAt = time.Unix(0, lastUsed.Int64).UTC()
	}
	if revoked.Valid {
		t.RevokedAt = time.Unix(0, revoked.Int64).UTC()
	}
	if len(t.TokenHash) >= 12 {
		t.HashPrefix = t.TokenHash[:12]
	}
	return nil
}
