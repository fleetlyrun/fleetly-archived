package state

import (
	"context"
	"fmt"
	"time"
)

// metrics.* 运行期设置（E6 观测专项设计 §4.1，W5-S3；D-W5-2 opt-in 用户
// 直裁）：platform_settings 库内设置，logsettings 同型（读取点每次现读
// 不缓存长驻；保存 = 校验 + 审计 + 事件同事务 fail-closed）。
//
// 缺省语义（D-W5-2 与 V2-2 rustfs 同型——**默认关**）：metrics.mode 未
// 显式设置 = 缺省 `unset`——三件套（VictoriaMetrics/cAdvisor/node_exporter）
// 不部署，零新增常驻（验收标准 7）；显式 `on` 后管理器部署三件。Load
// 投影区分「未设置」（Set=false）与「显式设置」（Set=true）。

// MetricsKeyMode 是 metrics 模式设置键（词表只增；键名常量为本包唯一
// 登记点）。
const MetricsKeyMode = "metrics.mode"

// metrics.mode 词表（§4.1）：缺省 unset（opt-in）。
const (
	MetricsModeUnset = "unset"
	MetricsModeOn    = "on"
)

// MetricsSettings 是 metrics.* 设置的 typed 视图。Mode 恒为生效形态
//（未设置时 = 缺省 unset）；Set 报告是否显式设置过。
type MetricsSettings struct {
	// Mode 是生效的 metrics 模式（unset | on）。
	Mode string
	// Set 报告该键是否被显式保存过（false = 缺省态生效）。
	Set bool
	// UpdatedAt 是设置行 updated_at（只读投影；未设置 = 零值）。
	UpdatedAt time.Time
}

// ValidateMetricsMode 校验 metrics 模式值域（保存与读侧共用；非法值显式
// 拒绝——设置面 loud-fail，不静默回落）。
func ValidateMetricsMode(mode string) error {
	switch mode {
	case MetricsModeUnset, MetricsModeOn:
		return nil
	default:
		return fmt.Errorf("state: metrics.mode %q not in {unset, on}", mode)
	}
}

// LoadMetricsSettings 读取 metrics.* 设置（每次现读）。空库/无行 → 缺省态
//（Mode=unset，Set=false）。存储值畸形显式报错：设置损坏 loud-fail，不
// 静默回落缺省（与 logs.backend 同口径）。
func (s *Store) LoadMetricsSettings(ctx context.Context) (MetricsSettings, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT value, updated_at FROM platform_settings WHERE key = ?`, MetricsKeyMode)
	if err != nil {
		return MetricsSettings{}, fmt.Errorf("state: query metrics settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := MetricsSettings{Mode: MetricsModeUnset}
	for rows.Next() {
		var value string
		var updatedAt int64
		if err := rows.Scan(&value, &updatedAt); err != nil {
			return MetricsSettings{}, fmt.Errorf("state: scan metrics setting: %w", err)
		}
		if err := ValidateMetricsMode(value); err != nil {
			return MetricsSettings{}, fmt.Errorf("state: stored metrics setting invalid: %w", err)
		}
		out.Mode = value
		out.Set = true
		out.UpdatedAt = time.Unix(0, updatedAt).UTC()
	}
	if err := rows.Err(); err != nil {
		return MetricsSettings{}, fmt.Errorf("state: iterate metrics settings: %w", err)
	}
	return out, nil
}

// MetricsSaveOptions 是保存的上下文选项（actor 进审计，logs 同型）。
type MetricsSaveOptions struct {
	Actor        string // "human"（API 面）——审计 actor 词表不扩
	ActorTokenID string
}

// SaveMetricsSettings 保存 metrics.mode + 审计 + 事件 metrics.mode_updated
//（同一事务，fail-closed；logsettings 模式）。payload/审计 diff 只带模式
// 值（词表内枚举，无敏感材料）。
func (s *Store) SaveMetricsSettings(ctx context.Context, mode string, opts MetricsSaveOptions) error {
	if err := ValidateMetricsMode(mode); err != nil {
		return err
	}
	err := s.InTx(ctx, func(tx *Tx) error {
		const q = `INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, q, MetricsKeyMode, mode, nowNano()); err != nil {
			return fmt.Errorf("state: upsert metrics setting %s: %w", MetricsKeyMode, err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        opts.Actor,
			ActorTokenID: opts.ActorTokenID,
			Action:       "metrics.mode_updated",
			Target:       "platform:metrics",
			Result:       "ok",
			DiffSummary:  DiffSummary("mode", mode),
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, Event{
			Name:    "metrics.mode_updated",
			Subject: "platform:metrics",
			Payload: DiffSummary("mode", mode),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: save metrics settings: %w", err)
	}
	return nil
}
