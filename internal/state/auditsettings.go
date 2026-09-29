package state

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// audit.* 运行期设置（v0.3 W3-S1，rbac-teams §6 裁决 D-W0-6）：platform_settings
// 库内设置，authsettings/logsettings 同型（读取点每次现读不缓存长驻；
// 保存 = 校验 + 审计同事务 fail-closed）。
//
// 缺省语义（D-W0-6：90 天可调）：audit.retention_days 未显式设置时**本层
// 不投影数值**（RetentionDays=0、Set=false）——生效值回落链在消费方裁决：
// platform_settings > config state.audit_retention_days > 缺省 90（janitor
// 的审计清理步是唯一消费点，见 janitor.go 的每拍现读）。本层只诚实
// 投影「未设置」与「显式设置」之别（logsettings 同款区分面）。
//
// 保存动作落审计 audit.retention_changed（既有 settings 先例的审计动作
// 形态——auth.registration_changed 的 _changed 后缀同款）；**不落事件**
//（设计 §6 事件注册表不含本键，不发明新事件）。

// AuditKeyRetentionDays 是审计留存天数设置键（词表只增；键名常量为本包
// 唯一登记点）。
const AuditKeyRetentionDays = "audit.retention_days"

// AuditSettings 是 audit.* 设置的 typed 视图。RetentionDays 仅在显式设置
// 过时有值（Set=true）；未设置 = 零值（消费方走 config > 缺省 90 回落链）。
type AuditSettings struct {
	// RetentionDays 是显式设置的审计留存天数（天）；未设置 = 0。
	RetentionDays int
	// Set 报告该键是否被显式保存过（false = 未设置，回落链生效）。
	Set bool
	// UpdatedAt 是设置行 updated_at（只读投影；未设置 = 零值）。
	UpdatedAt time.Time
}

// ValidateRetentionDays 校验留存天数值域（保存与读侧共用；非法值显式拒绝
// ——设置面 loud-fail，不静默回落）：天数须 ≥ 1（0/负数会把「关掉清理」
// 伪装成合法设置——与 janitor 的非正值回落缺省纪律冲突，设置面直接拒绝）。
func ValidateRetentionDays(days int) error {
	if days < 1 {
		return fmt.Errorf("state: audit.retention_days %d not >= 1", days)
	}
	return nil
}

// LoadAuditSettings 读取 audit.* 设置（每次现读）。空库/无行 → 未设置态
// （RetentionDays=0，Set=false）。存储值畸形（非整数 / 越值域）显式报错：
// 设置损坏 loud-fail，不静默回落（与 logs.backend 同口径——静默回落会让
// 「设置损坏」伪装成「从未设置」）。
func (s *Store) LoadAuditSettings(ctx context.Context) (AuditSettings, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT value, updated_at FROM platform_settings WHERE key = ?`, AuditKeyRetentionDays)
	if err != nil {
		return AuditSettings{}, fmt.Errorf("state: query audit settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := AuditSettings{}
	for rows.Next() {
		var value string
		var updatedAt int64
		if err := rows.Scan(&value, &updatedAt); err != nil {
			return AuditSettings{}, fmt.Errorf("state: scan audit setting: %w", err)
		}
		days, perr := strconv.Atoi(strings.TrimSpace(value))
		if perr != nil {
			return AuditSettings{}, fmt.Errorf("state: stored audit setting invalid: %q is not an integer", value)
		}
		if verr := ValidateRetentionDays(days); verr != nil {
			return AuditSettings{}, fmt.Errorf("state: stored audit setting invalid: %w", verr)
		}
		out.RetentionDays = days
		out.Set = true
		out.UpdatedAt = time.Unix(0, updatedAt).UTC()
	}
	if err := rows.Err(); err != nil {
		return AuditSettings{}, fmt.Errorf("state: iterate audit settings: %w", err)
	}
	return out, nil
}

// AuditSaveOptions 是保存的上下文选项（actor 进审计，logs 同型——api 面
// 传 user:<id> 或 human）。
type AuditSaveOptions struct {
	Actor        string
	ActorTokenID string
}

// SaveRetentionDays 保存 audit.retention_days + 审计 audit.retention_changed
// （同一事务，fail-closed；authsettings 模式——只审计不落事件，设计 §6
// 事件注册表不含本键）。payload/审计 diff 只带天数（非敏感材料）。
func (s *Store) SaveRetentionDays(ctx context.Context, days int, opts AuditSaveOptions) error {
	if err := ValidateRetentionDays(days); err != nil {
		return err
	}
	err := s.InTx(ctx, func(tx *Tx) error {
		const q = `INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, q, AuditKeyRetentionDays, strconv.Itoa(days), nowNano()); err != nil {
			return fmt.Errorf("state: upsert audit setting %s: %w", AuditKeyRetentionDays, err)
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        opts.Actor,
			ActorTokenID: opts.ActorTokenID,
			Action:       "audit.retention_changed",
			Target:       "platform:audit",
			Result:       "ok",
			DiffSummary:  DiffSummary("retention_days", days),
		})
	})
	if err != nil {
		return fmt.Errorf("state: save audit settings: %w", err)
	}
	return nil
}
