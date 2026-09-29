package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
)

// alerting.* 运行期设置（B 线 W5 设计 §2.1，D-V3W5-1，v0.3 W5-S2）：
// platform_settings 库内设置，metricssettings 同型（读取点每次现读不缓存
// 长驻；保存 = 校验 + 审计同事务 fail-closed）。
//
// 缺省语义（与 metrics.mode 同型的 opt-in）：alerts.mode 未显式设置 = 缺省
// `unset`——vmalert 不部署（metrics 三件套有自己的 opt-in 门，两层独立）；
// 显式 `on` 后 metrics 管理器在三件之外加部署 vmalert。
//
// 前置门（设计 §2.1 原文）：alerts.mode=on 的**前置门 = metrics.mode=on**
// ——vmalert 没有 datasource 无意义；metrics.mode 非 on 时设置拒绝 409
//（E_ALERTS_METRICS_REQUIRED，带指引）。反向（metrics 先关而 alerts 仍
// on）不在设置门拦截：收敛管理器以「两门同为 on」为 vmalert 应许态，metrics
// 关闭即 vmalert 一并移除（status 面如实投影），metrics 回 on 自动恢复。

// AlertsKeyMode 是 alerts 模式设置键（词表只增；键名常量为本包唯一登记点）。
const AlertsKeyMode = "alerts.mode"

// alerts.mode 词表（§2.1）：缺省 unset（opt-in）。
const (
	AlertsModeUnset = "unset"
	AlertsModeOn    = "on"
)

// AlertsSettings 是 alerts.* 设置的 typed 视图。Mode 恒为生效形态（未设置
// 时 = 缺省 unset）；Set 报告是否显式设置过。
type AlertsSettings struct {
	// Mode 是生效的 alerts 模式（unset | on）。
	Mode string
	// Set 报告该键是否被显式保存过（false = 缺省态生效）。
	Set bool
	// UpdatedAt 是设置行 updated_at（只读投影；未设置 = 零值）。
	UpdatedAt time.Time
}

// ValidateAlertsMode 校验 alerts 模式值域（保存与读侧共用；非法值显式拒绝
// ——设置面 loud-fail，不静默回落）。
func ValidateAlertsMode(mode string) error {
	switch mode {
	case AlertsModeUnset, AlertsModeOn:
		return nil
	default:
		return fmt.Errorf("state: alerts.mode %q not in {unset, on}", mode)
	}
}

// LoadAlertsSettings 读取 alerts.* 设置（每次现读）。空库/无行 → 缺省态
// （Mode=unset，Set=false）。存储值畸形显式报错（metrics.mode 同口径）。
func (s *Store) LoadAlertsSettings(ctx context.Context) (AlertsSettings, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT value, updated_at FROM platform_settings WHERE key = ?`, AlertsKeyMode)
	if err != nil {
		return AlertsSettings{}, fmt.Errorf("state: query alerts settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := AlertsSettings{Mode: AlertsModeUnset}
	for rows.Next() {
		var value string
		var updatedAt int64
		if err := rows.Scan(&value, &updatedAt); err != nil {
			return AlertsSettings{}, fmt.Errorf("state: scan alerts setting: %w", err)
		}
		if err := ValidateAlertsMode(value); err != nil {
			return AlertsSettings{}, fmt.Errorf("state: stored alerts setting invalid: %w", err)
		}
		out.Mode = value
		out.Set = true
		out.UpdatedAt = time.Unix(0, updatedAt).UTC()
	}
	if err := rows.Err(); err != nil {
		return AlertsSettings{}, fmt.Errorf("state: iterate alerts settings: %w", err)
	}
	return out, nil
}

// AlertsSaveOptions 是保存的上下文选项（actor 进审计，metrics 同型）。
type AlertsSaveOptions struct {
	Actor        string // "human"（API 面）——审计 actor 词表不扩
	ActorTokenID string
}

// SaveAlertsSettings 保存 alerts.mode + 审计（同一事务 fail-closed）。
//
// 前置门（设计 §2.1）：mode=on 时先读 metrics.mode，非 on 拒绝 409
// E_ALERTS_METRICS_REQUIRED（信封 suggestion 即指引——先开 metrics 再开
// alerts）。mode=unset 恒允许（关闭不依赖 metrics 形态）。门检查与写入同
// 事务读一致（metrics.mode 的并发翻转窗口收敛到事务序列化语义）。
func (s *Store) SaveAlertsSettings(ctx context.Context, mode string, opts AlertsSaveOptions) error {
	if err := ValidateAlertsMode(mode); err != nil {
		return err
	}
	err := s.InTx(ctx, func(tx *Tx) error {
		if mode == AlertsModeOn {
			var mrow sql.NullString
			if err := tx.QueryRowContext(ctx,
				`SELECT value FROM platform_settings WHERE key = ?`, MetricsKeyMode).Scan(&mrow); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("state: read metrics.mode for alerts gate: %w", err)
			}
			if mrow.String != MetricsModeOn {
				return apperr.New("E_ALERTS_METRICS_REQUIRED",
					"alerts.mode=on requires metrics.mode=on first (vmalert has no datasource without the metrics stack): current metrics.mode=%s",
					nonEmptyOr(mrow.String, MetricsModeUnset))
			}
		}
		const q = `INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, q, AlertsKeyMode, mode, nowNano()); err != nil {
			return fmt.Errorf("state: upsert alerts setting %s: %w", AlertsKeyMode, err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        opts.Actor,
			ActorTokenID: opts.ActorTokenID,
			Action:       "alerting.mode_changed",
			Target:       "platform:alerting",
			Result:       "ok",
			DiffSummary:  DiffSummary("mode", mode),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: save alerts settings: %w", err)
	}
	return nil
}

// nonEmptyOr 空串回落缺省（设置行缺失时 mode 投影用）。
func nonEmptyOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
