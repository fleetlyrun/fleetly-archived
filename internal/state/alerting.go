package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/oklog/ulid/v2"
)

// alert_rules（B 线 W5 设计 §2.2，D-V3W5-1，v0.3 W5-S2）：Prometheus 告警
// 规则的平台权威态——vmalert rule 文件的渲染源（internal/metrics 管理器消费
// 渲染）。CRUD 原语 + 校验 + 审计 alerting.rule_changed（同事务
// fail-closed）；**零事件**（告警全链不经事件流——防回环红线，设计 §2.3；
// 可见面 = 审计 + API/CLI/Console）。
//
// 校验纪律（webhooks.go 同型双保险）：API 面做 400 形状门（buf.validate +
// handler），本层是存储不变量的第二道闸。规则名平台级 UNIQUE（端点同域，
// 设计 §2.2 裁决形态）；expr 非空 ≤2048；for_duration ≥0；labels=JSON 对象
// string→string；channels=JSON 字符串数组（空 = 缺省投全部端点）。channels
// 引用端点的存在性不在保存期校验——端点可后删，投递期解析缺失即跳过。

// AlertRule 相关哨兵错误（api 面映射 404/409 信封）。
var (
	// ErrAlertRuleNotFound 表示目标规则不存在。
	ErrAlertRuleNotFound = errors.New("alert rule not found")
	// ErrAlertRuleNameConflict 表示规则名已被占用（UNIQUE 冲突的语义化投影）。
	ErrAlertRuleNameConflict = errors.New("alert rule name conflict")
)

// alertRuleNameRe 是规则名白名单（与 webhook 端点名同词表——平台级人读名，
// CLI 位置参数与 Console 表单共用）。
var alertRuleNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// MaxAlertRuleExprLength 是 expr 的长度上限（告警面契约：与 SearchMetrics
// 的 4096 不同档——规则常驻求值，表达式刻意收敛到单条件形态）。
const MaxAlertRuleExprLength = 2048

// ValidateAlertRuleName 校验规则名（创建与改名共用；防御性第二道闸）。
func ValidateAlertRuleName(name string) error {
	if !alertRuleNameRe.MatchString(name) {
		return fmt.Errorf("state: alert rule name %q is invalid (1..64 chars; letters/digits first, then letters/digits/dot/underscore/hyphen)", name)
	}
	return nil
}

// ValidateAlertRuleExpr 校验 expr 形状（非空 ≤2048；语法合法性由
// vmalert/VM 裁决——平台不做 PromQL 解析，查询面透传口径同源）。
func ValidateAlertRuleExpr(expr string) error {
	if len(strings.TrimSpace(expr)) == 0 {
		return apperr.New("E_ALERT_RULE_EXPR_INVALID", "alert rule expr must not be empty")
	}
	if len(expr) > MaxAlertRuleExprLength {
		return apperr.New("E_ALERT_RULE_EXPR_INVALID",
			"alert rule expr is %d chars (max %d)", len(expr), MaxAlertRuleExprLength)
	}
	return nil
}

// ValidateAlertRuleLabels 校验 labels 形状（JSON 对象 string→string；键
// 非空；空对象合法）。返回归一形态（nil map → 空对象——存储恒为合法 JSON）。
func ValidateAlertRuleLabels(labels map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if strings.TrimSpace(k) == "" {
			return nil, apperr.New("E_ALERT_RULE_LABELS_INVALID", "alert rule labels must not carry an empty key")
		}
		out[k] = v
	}
	return out, nil
}

// ValidateAlertRuleChannels 校验 channels 形状（字符串数组；每项非空；
// 去重保序后返回；空/nil = 缺省投全部端点的合法空集）。
func ValidateAlertRuleChannels(channels []string) ([]string, error) {
	seen := make(map[string]struct{}, len(channels))
	out := make([]string, 0, len(channels))
	for _, c := range channels {
		if strings.TrimSpace(c) == "" {
			return nil, apperr.New("E_ALERT_RULE_CHANNELS_INVALID", "alert rule channels must not carry an empty endpoint id")
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

// AlertRule 是一条告警规则行（只读投影）。
type AlertRule struct {
	ID   string
	Name string
	// Expr 是 PromQL 表达式（原样存储原样渲染——透传纪律）。
	Expr string
	// ForDurationSeconds 是 for 子句的秒数（0 = 无 for 子句；渲染为「Ns」）。
	ForDurationSeconds int64
	// Labels 是规则的 label 集（含 severity——severity 进通知文案的源头）。
	Labels map[string]string
	// Channels 是通知端点 id 集（空 = 缺省投全部启用端点）。
	Channels  []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// alertRuleCols 是规则行查询列清单。
const alertRuleCols = `id, name, expr, for_duration_seconds, labels, channels, created_at, updated_at`

// scanAlertRule 从行扫描规则投影（labels/channels 存储恒为合法 JSON——
// 写入通道保证；读出畸形 loud-fail 不静默）。
func scanAlertRule(row scanner) (AlertRule, error) {
	var (
		r          AlertRule
		forSeconds int64
		labelsJSON string
		chansJSON  string
		createdAt  int64
		updatedAt  int64
	)
	if err := row.Scan(&r.ID, &r.Name, &r.Expr, &forSeconds, &labelsJSON, &chansJSON, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AlertRule{}, ErrAlertRuleNotFound
		}
		return AlertRule{}, fmt.Errorf("state: scan alert rule: %w", err)
	}
	r.ForDurationSeconds = forSeconds
	if err := json.Unmarshal([]byte(labelsJSON), &r.Labels); err != nil {
		return AlertRule{}, fmt.Errorf("state: scan alert rule %s: labels json: %w", r.ID, err)
	}
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	if err := json.Unmarshal([]byte(chansJSON), &r.Channels); err != nil {
		return AlertRule{}, fmt.Errorf("state: scan alert rule %s: channels json: %w", r.ID, err)
	}
	if r.Channels == nil {
		r.Channels = []string{}
	}
	r.CreatedAt = time.Unix(0, createdAt).UTC()
	r.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return r, nil
}

// AlertRuleWrite 是一次规则创建写入。
type AlertRuleWrite struct {
	// ID 留空自动生成 ULID。
	ID   string
	Name string
	Expr string
	// ForDurationSeconds ≥0（负值拒绝）。
	ForDurationSeconds int64
	Labels             map[string]string
	Channels           []string
	// Actor/ActorTokenID 进审计（API 面传调用者身份，同事务 fail-closed）。
	Actor        string
	ActorTokenID string
}

// CreateAlertRule 在事务内落一条规则行并与审计同事务 fail-closed。名字
// 冲突（UNIQUE）投影为 ErrAlertRuleNameConflict。
func (s *Store) CreateAlertRule(ctx context.Context, w AlertRuleWrite) (AlertRule, error) {
	if err := ValidateAlertRuleName(w.Name); err != nil {
		return AlertRule{}, err
	}
	if err := ValidateAlertRuleExpr(w.Expr); err != nil {
		return AlertRule{}, err
	}
	if w.ForDurationSeconds < 0 {
		return AlertRule{}, apperr.New("E_ALERT_RULE_FOR_INVALID",
			"alert rule for_duration must be >= 0 seconds, got %d", w.ForDurationSeconds)
	}
	labels, err := ValidateAlertRuleLabels(w.Labels)
	if err != nil {
		return AlertRule{}, err
	}
	channels, err := ValidateAlertRuleChannels(w.Channels)
	if err != nil {
		return AlertRule{}, err
	}
	id := w.ID
	if id == "" {
		id = ulid.Make().String()
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return AlertRule{}, fmt.Errorf("state: create alert rule: marshal labels: %w", err)
	}
	chansJSON, err := json.Marshal(channels)
	if err != nil {
		return AlertRule{}, fmt.Errorf("state: create alert rule: marshal channels: %w", err)
	}
	actor := w.Actor
	if actor == "" {
		actor = "human"
	}
	err = s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		const q = `INSERT INTO alert_rules
			(id, name, expr, for_duration_seconds, labels, channels, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, q, id, w.Name, w.Expr, w.ForDurationSeconds,
			string(labelsJSON), string(chansJSON), now, now); err != nil {
			if isUniqueViolation(err) {
				return ErrAlertRuleNameConflict
			}
			return fmt.Errorf("state: insert alert rule: %w", err)
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        actor,
			ActorTokenID: w.ActorTokenID,
			Action:       "alerting.rule_changed",
			Target:       "alert_rule:" + id,
			Result:       "ok",
			DiffSummary: DiffSummary("op", "create", "name", w.Name,
				"for_duration_seconds", w.ForDurationSeconds, "channels", len(channels)),
		})
	})
	if err != nil {
		return AlertRule{}, err
	}
	return s.GetAlertRule(ctx, id)
}

// ListAlertRules 返回全部规则（name 字典序——渲染序的稳定性锚：同样规则集
// 恒渲染同内容，内容寻址 config 名才稳）。
func (s *Store) ListAlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+alertRuleCols+` FROM alert_rules ORDER BY name ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("state: list alert rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []AlertRule{}
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate alert rules: %w", err)
	}
	return out, nil
}

// GetAlertRule 按 ID 取规则（不存在返回 ErrAlertRuleNotFound）。
func (s *Store) GetAlertRule(ctx context.Context, id string) (AlertRule, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+alertRuleCols+` FROM alert_rules WHERE id = ?`, id)
	return scanAlertRule(row)
}

// AlertRuleUpdate 是规则更新的可选字段集（nil = 不变）。
type AlertRuleUpdate struct {
	Name *string
	Expr *string
	// ForDurationSeconds 非 nil = 整体替换（≥0 校验）。
	ForDurationSeconds *int64
	// Labels 非 nil = 整体替换（指针判存在——map 零值与缺省的歧义由显式
	// 指针收敛；nil map 内容 = 空对象合法）。
	Labels *map[string]string
	// Channels 非 nil = 整体替换。
	Channels     []string
	Actor        string
	ActorTokenID string
}

// UpdateAlertRule 更新规则可选字段 + 审计（同一事务 fail-closed）。至少
// 一个字段必须提供（全空更新 = 无操作，显式拒绝防误调用）。
func (s *Store) UpdateAlertRule(ctx context.Context, id string, u AlertRuleUpdate) (AlertRule, error) {
	anyField := u.Name != nil || u.Expr != nil || u.ForDurationSeconds != nil ||
		u.Labels != nil || u.Channels != nil
	if !anyField {
		return AlertRule{}, fmt.Errorf("state: update alert rule: no fields to update")
	}
	if u.Name != nil {
		if err := ValidateAlertRuleName(*u.Name); err != nil {
			return AlertRule{}, err
		}
	}
	if u.Expr != nil {
		if err := ValidateAlertRuleExpr(*u.Expr); err != nil {
			return AlertRule{}, err
		}
	}
	if u.ForDurationSeconds != nil && *u.ForDurationSeconds < 0 {
		return AlertRule{}, apperr.New("E_ALERT_RULE_FOR_INVALID",
			"alert rule for_duration must be >= 0 seconds, got %d", *u.ForDurationSeconds)
	}
	var labelsJSON *string
	if u.Labels != nil {
		labels, err := ValidateAlertRuleLabels(*u.Labels)
		if err != nil {
			return AlertRule{}, err
		}
		raw, err := json.Marshal(labels)
		if err != nil {
			return AlertRule{}, fmt.Errorf("state: update alert rule: marshal labels: %w", err)
		}
		labelsJSON = new(string)
		*labelsJSON = string(raw)
	}
	var chansJSON *string
	if u.Channels != nil {
		channels, err := ValidateAlertRuleChannels(u.Channels)
		if err != nil {
			return AlertRule{}, err
		}
		raw, err := json.Marshal(channels)
		if err != nil {
			return AlertRule{}, fmt.Errorf("state: update alert rule: marshal channels: %w", err)
		}
		chansJSON = new(string)
		*chansJSON = string(raw)
	}
	actor := u.Actor
	if actor == "" {
		actor = "human"
	}
	changed := []any{}
	err := s.InTx(ctx, func(tx *Tx) error {
		// 行存在性先行（不存在即 NotFound——审计不落幽灵行）。
		if _, err := scanAlertRule(tx.QueryRowContext(ctx,
			`SELECT `+alertRuleCols+` FROM alert_rules WHERE id = ?`, id)); err != nil {
			return err
		}
		now := nowNano()
		if u.Name != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE alert_rules SET name = ?, updated_at = ? WHERE id = ?`,
				*u.Name, now, id); err != nil {
				if isUniqueViolation(err) {
					return ErrAlertRuleNameConflict
				}
				return fmt.Errorf("state: update alert rule name: %w", err)
			}
			changed = append(changed, "name", *u.Name)
		}
		if u.Expr != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE alert_rules SET expr = ?, updated_at = ? WHERE id = ?`,
				*u.Expr, now, id); err != nil {
				return fmt.Errorf("state: update alert rule expr: %w", err)
			}
			changed = append(changed, "expr_changed", true)
		}
		if u.ForDurationSeconds != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE alert_rules SET for_duration_seconds = ?, updated_at = ? WHERE id = ?`,
				*u.ForDurationSeconds, now, id); err != nil {
				return fmt.Errorf("state: update alert rule for_duration: %w", err)
			}
			changed = append(changed, "for_duration_seconds", *u.ForDurationSeconds)
		}
		if labelsJSON != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE alert_rules SET labels = ?, updated_at = ? WHERE id = ?`,
				*labelsJSON, now, id); err != nil {
				return fmt.Errorf("state: update alert rule labels: %w", err)
			}
			changed = append(changed, "labels_changed", true)
		}
		if chansJSON != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE alert_rules SET channels = ?, updated_at = ? WHERE id = ?`,
				*chansJSON, now, id); err != nil {
				return fmt.Errorf("state: update alert rule channels: %w", err)
			}
			changed = append(changed, "channels", len(u.Channels))
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE alert_rules SET updated_at = ? WHERE id = ?`, now, id); err != nil {
			return fmt.Errorf("state: touch alert rule: %w", err)
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        actor,
			ActorTokenID: u.ActorTokenID,
			Action:       "alerting.rule_changed",
			Target:       "alert_rule:" + id,
			Result:       "ok",
			DiffSummary:  DiffSummary(changed...),
		})
	})
	if err != nil {
		return AlertRule{}, err
	}
	return s.GetAlertRule(ctx, id)
}

// DeleteAlertRule 删除规则 + 审计（幂等语义与 webhook 端点同形：不存在即
// ErrAlertRuleNotFound——rm 面需要明确 NotFound 反馈）。
func (s *Store) DeleteAlertRule(ctx context.Context, id, actor, actorTokenID string) error {
	if actor == "" {
		actor = "human"
	}
	return s.InTx(ctx, func(tx *Tx) error {
		// 先取行（审计要带名字；不存在即 NotFound——不落删除审计）。
		prev, err := scanAlertRule(tx.QueryRowContext(ctx,
			`SELECT `+alertRuleCols+` FROM alert_rules WHERE id = ?`, id))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM alert_rules WHERE id = ?`, id); err != nil {
			return fmt.Errorf("state: delete alert rule: %w", err)
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        actor,
			ActorTokenID: actorTokenID,
			Action:       "alerting.rule_changed",
			Target:       "alert_rule:" + id,
			Result:       "ok",
			DiffSummary:  DiffSummary("op", "delete", "name", prev.Name),
		})
	})
}

// CountAlertRules 是规则数计数（GetAlertsStatus 的规则数投影面；空表 = 0）。
func (s *Store) CountAlertRules(ctx context.Context) (int, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM alert_rules`).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: count alert rules: %w", err)
	}
	return int(n), nil
}
