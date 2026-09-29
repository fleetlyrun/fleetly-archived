package state

import (
	"context"
	"fmt"
	"time"
)

// logs.* 运行期设置（E6 观测专项设计 §2.2，W5-S1）：platform_settings
// 库内设置，s3.* 同型（读取点每次现读不缓存长驻；保存 = 校验 + 审计 +
// 事件同事务 fail-closed）。
//
// 缺省语义（V2-1 默认捆绑，用户直裁）：logs.backend 未显式设置 = 缺省
// `victorialogs`——存量安装升级后收敛管理器即部署 VL；显式设过 `jsonl` 的
// 安装不动。Load 投影区分「未设置」（Set=false）与「显式设置」（Set=true），
// 供 CLI/Console 诚实展示（mode 值恒为生效形态，绝不回空串让消费方各自
// 猜缺省——缺省的唯一事实源在本包）。

// LogsKeyBackend 是日志后端设置键（词表只增；键名常量为本包唯一登记点）。
const LogsKeyBackend = "logs.backend"

// logs.backend 词表（§2.2）：缺省 victorialogs（默认捆绑）。
const (
	LogsBackendVictorialogs = "victorialogs"
	LogsBackendJSONL        = "jsonl"
)

// LogsSettings 是 logs.* 设置的 typed 视图。Backend 恒为生效形态
//（未设置时 = 缺省 victorialogs）；Set 报告是否显式设置过。
type LogsSettings struct {
	// Backend 是生效的日志后端（victorialogs | jsonl）。
	Backend string
	// Set 报告该键是否被显式保存过（false = 缺省态生效）。
	Set bool
	// UpdatedAt 是设置行 updated_at（只读投影；未设置 = 零值）。
	UpdatedAt time.Time
}

// ValidateLogsBackend 校验日志后端值域（保存与读侧共用；非法值显式拒绝
// ——设置面 loud-fail，不静默回落）。
func ValidateLogsBackend(backend string) error {
	switch backend {
	case LogsBackendVictorialogs, LogsBackendJSONL:
		return nil
	default:
		return fmt.Errorf("state: logs.backend %q not in {victorialogs, jsonl}", backend)
	}
}

// LoadLogsSettings 读取 logs.* 设置（每次现读）。空库/无行 → 缺省态
//（Backend=victorialogs，Set=false）。存储值畸形显式报错：设置损坏
// loud-fail，不静默回落缺省（与 s3 布尔同口径）。
func (s *Store) LoadLogsSettings(ctx context.Context) (LogsSettings, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT value, updated_at FROM platform_settings WHERE key = ?`, LogsKeyBackend)
	if err != nil {
		return LogsSettings{}, fmt.Errorf("state: query logs settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := LogsSettings{Backend: LogsBackendVictorialogs}
	for rows.Next() {
		var value string
		var updatedAt int64
		if err := rows.Scan(&value, &updatedAt); err != nil {
			return LogsSettings{}, fmt.Errorf("state: scan logs setting: %w", err)
		}
		if err := ValidateLogsBackend(value); err != nil {
			return LogsSettings{}, fmt.Errorf("state: stored logs setting invalid: %w", err)
		}
		out.Backend = value
		out.Set = true
		out.UpdatedAt = time.Unix(0, updatedAt).UTC()
	}
	if err := rows.Err(); err != nil {
		return LogsSettings{}, fmt.Errorf("state: iterate logs settings: %w", err)
	}
	return out, nil
}

// LogsSaveOptions 是保存的上下文选项（actor 进审计，s3 同型）。
type LogsSaveOptions struct {
	Actor        string // "human"（API 面）——审计 actor 词表不扩
	ActorTokenID string
}

// SaveLogsSettings 保存 logs.backend + 审计 + 事件 logs.backend_updated
//（同一事务，fail-closed；s3settings 模式）。payload/审计 diff 只带后端
// 值（词表内枚举，无敏感材料）。
func (s *Store) SaveLogsSettings(ctx context.Context, backend string, opts LogsSaveOptions) error {
	if err := ValidateLogsBackend(backend); err != nil {
		return err
	}
	err := s.InTx(ctx, func(tx *Tx) error {
		const q = `INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, q, LogsKeyBackend, backend, nowNano()); err != nil {
			return fmt.Errorf("state: upsert logs setting %s: %w", LogsKeyBackend, err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        opts.Actor,
			ActorTokenID: opts.ActorTokenID,
			Action:       "logs.backend_updated",
			Target:       "platform:logs",
			Result:       "ok",
			DiffSummary:  DiffSummary("backend", backend),
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, Event{
			Name:    "logs.backend_updated",
			Subject: "platform:logs",
			Payload: DiffSummary("backend", backend),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: save logs settings: %w", err)
	}
	return nil
}
