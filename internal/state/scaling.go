package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// 自动扩缩策略与运行期副本覆盖层（B 线 W5 设计 §1，D-V3W5-2，v0.3 W5-S1）：
// scaling_policies（per app × per compose service 的策略权威态）+
// scaling_replica_overrides（引擎 autoscaler 平台写通道的期望落点——设计
// §1.2「spec 的运行期副本覆盖层」）。
//
// 约束词表（设计 §1.1 原文）：min ≥1、max ≤16（swarm 单服务上限口径，防误
// 配爆炸）；target ∈ [20,90]%；cooldown ∈ [60,3600]s 缺省 180。词表校验在
// 本文件写入通道单点（SQLite CHECK 不承载词表——webhook_deliveries.status
// 同纪律）；目标水位 0 = 该维度不设目标（至少一维必设）。策略变更审计
// scaling.policy_changed 与业务写同事务 fail-closed（事件面零新增——设计
// §1.2 的事件是运行期动作面，策略 CRUD 只记审计）。

// ScalingPolicyNotFound / 覆盖层哨兵（api 面映射 404 信封）。
var (
	// ErrScalingPolicyNotFound 表示目标 app×service 无策略行。
	ErrScalingPolicyNotFound = errors.New("scaling policy not found")
	// ErrScalingOverrideNotFound 表示目标 app×service 无运行期副本覆盖行。
	ErrScalingOverrideNotFound = errors.New("scaling replica override not found")
)

// 策略约束词表（常量为校验与 CLI/Console 帮助文案的单一事实源）。
const (
	// ScalingMinReplicasFloor 是 min_replicas 的下界（设计 §1.1：min ≥1）。
	ScalingMinReplicasFloor = 1
	// ScalingMaxReplicasCeiling 是 max_replicas 的上界（设计 §1.1：max ≤16
	// ——swarm 单服务上限口径）。
	ScalingMaxReplicasCeiling = 16
	// ScalingTargetPctFloor / ScalingTargetPctCeiling 是目标水位的值域
	//（设计 §1.1：target ∈ [20,90]%）。
	ScalingTargetPctFloor = 20
	// ScalingTargetPctCeiling 是目标水位上界。
	ScalingTargetPctCeiling = 90
	// ScalingCooldownFloorSeconds / ScalingCooldownCeilingSeconds 是冷却窗
	// 值域（设计 §1.1：cooldown ∈ [60,3600]s）。
	ScalingCooldownFloorSeconds = 60
	// ScalingCooldownCeilingSeconds 是冷却窗上界。
	ScalingCooldownCeilingSeconds = 3600
	// ScalingDefaultCooldownSeconds 是冷却窗缺省（设计 §1.1：缺省 180）。
	ScalingDefaultCooldownSeconds = 180
)

// scalingServicePattern 是 compose 服务名词表（[A-Za-z0-9._-]+——naming.
// validateComponent 的字符集；state 层不 import naming（分层：naming 是
// 底座命名契约层），字符集由本 regexp 单点承载并由测试对照钉死）。
var scalingServicePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ScalingPolicy 是一条扩缩策略行（只读投影）。
type ScalingPolicy struct {
	AppID   string
	Service string
	// MinReplicas / MaxReplicas 是副本夹逼区间（1 ≤ min ≤ max ≤ 16）。
	MinReplicas int
	MaxReplicas int
	// TargetCPUPct / TargetMemPct 是扩缩目标水位（百分数 [20,90]；0 = 该
	// 维度不设目标——引擎评估时跳过该维度）。
	TargetCPUPct int
	TargetMemPct int
	// CooldownSeconds 是动作冷却窗（[60,3600]s）。
	CooldownSeconds int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ValidateScalingPolicy 校验并归一策略（写入前单点；cooldown 零值回落缺省
// 180——「缺省 180」的设计语义在写侧落定，读侧不做二次回落）。非法值显式
// 拒绝（设置面 loud-fail，不静默夹紧）。
func ValidateScalingPolicy(p *ScalingPolicy) error {
	if !scalingServicePattern.MatchString(p.Service) {
		return fmt.Errorf("state: scaling policy service %q outside [A-Za-z0-9._-]", p.Service)
	}
	if p.MinReplicas < ScalingMinReplicasFloor {
		return fmt.Errorf("state: scaling min_replicas %d below floor %d", p.MinReplicas, ScalingMinReplicasFloor)
	}
	if p.MaxReplicas > ScalingMaxReplicasCeiling {
		return fmt.Errorf("state: scaling max_replicas %d above ceiling %d", p.MaxReplicas, ScalingMaxReplicasCeiling)
	}
	if p.MaxReplicas < p.MinReplicas {
		return fmt.Errorf("state: scaling max_replicas %d below min_replicas %d", p.MaxReplicas, p.MinReplicas)
	}
	for name, target := range map[string]int{"target_cpu_pct": p.TargetCPUPct, "target_mem_pct": p.TargetMemPct} {
		if target == 0 {
			continue // 0 = 该维度不设目标
		}
		if target < ScalingTargetPctFloor || target > ScalingTargetPctCeiling {
			return fmt.Errorf("state: scaling %s %d outside [%d,%d]",
				name, target, ScalingTargetPctFloor, ScalingTargetPctCeiling)
		}
	}
	if p.TargetCPUPct == 0 && p.TargetMemPct == 0 {
		return fmt.Errorf("state: scaling policy needs at least one target (target_cpu_pct or target_mem_pct)")
	}
	if p.CooldownSeconds == 0 {
		p.CooldownSeconds = ScalingDefaultCooldownSeconds
	}
	if p.CooldownSeconds < ScalingCooldownFloorSeconds || p.CooldownSeconds > ScalingCooldownCeilingSeconds {
		return fmt.Errorf("state: scaling cooldown_seconds %d outside [%d,%d]",
			p.CooldownSeconds, ScalingCooldownFloorSeconds, ScalingCooldownCeilingSeconds)
	}
	return nil
}

const scalingPolicyScanCols = `app_id, service, min_replicas, max_replicas,
	target_cpu_pct, target_mem_pct, cooldown_seconds, created_at, updated_at`

func scanScalingPolicy(row interface{ Scan(...any) error }) (ScalingPolicy, error) {
	var p ScalingPolicy
	var created, updated int64
	if err := row.Scan(&p.AppID, &p.Service, &p.MinReplicas, &p.MaxReplicas,
		&p.TargetCPUPct, &p.TargetMemPct, &p.CooldownSeconds, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ScalingPolicy{}, ErrScalingPolicyNotFound
		}
		return ScalingPolicy{}, fmt.Errorf("state: scan scaling policy: %w", err)
	}
	p.CreatedAt = time.Unix(0, created).UTC()
	p.UpdatedAt = time.Unix(0, updated).UTC()
	return p, nil
}

// GetScalingPolicy 读单条策略（不存在 → ErrScalingPolicyNotFound）。
func (s *Store) GetScalingPolicy(ctx context.Context, appID, service string) (ScalingPolicy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scalingPolicyScanCols+` FROM scaling_policies WHERE app_id = ? AND service = ?`,
		appID, service)
	return scanScalingPolicy(row)
}

// ScalingPolicyOptions 是策略写面的上下文（actor 进审计——metricssettings
// 同型）。
type ScalingPolicyOptions struct {
	Actor        string // "human"（API 面）——审计 actor 词表不扩
	ActorTokenID string
}

// SetScalingPolicy 写入（整行替换 upsert）策略 + 审计 scaling.policy_changed
// （同事务 fail-closed）。app 存在性与 active 生命周期在此单点校验（api 面
// 已做引用解析——此处是写通道的双保险）；服务名词表由 ValidateScalingPolicy
// 承载。返回写入后的行。
func (s *Store) SetScalingPolicy(ctx context.Context, appID string, p ScalingPolicy, opts ScalingPolicyOptions) (ScalingPolicy, error) {
	p.AppID = appID
	if err := ValidateScalingPolicy(&p); err != nil {
		return ScalingPolicy{}, err
	}
	var out ScalingPolicy
	err := s.InTx(ctx, func(tx *Tx) error {
		app, err := tx.GetAppByID(ctx, appID)
		if err != nil {
			return fmt.Errorf("state: set scaling policy: app %s: %w", appID, err)
		}
		if app.Lifecycle != LifecycleActive {
			return fmt.Errorf("state: set scaling policy: app %s is %s (only active apps accept policies)", app.Name, app.Lifecycle)
		}
		now := nowNano()
		const q = `INSERT INTO scaling_policies (app_id, service, min_replicas, max_replicas,
				target_cpu_pct, target_mem_pct, cooldown_seconds, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(app_id, service) DO UPDATE SET
				min_replicas = excluded.min_replicas,
				max_replicas = excluded.max_replicas,
				target_cpu_pct = excluded.target_cpu_pct,
				target_mem_pct = excluded.target_mem_pct,
				cooldown_seconds = excluded.cooldown_seconds,
				updated_at = excluded.updated_at`
		if _, err := tx.ExecContext(ctx, q, appID, p.Service, p.MinReplicas, p.MaxReplicas,
			p.TargetCPUPct, p.TargetMemPct, p.CooldownSeconds, now, now); err != nil {
			return fmt.Errorf("state: upsert scaling policy %s/%s: %w", appID, p.Service, err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        opts.Actor,
			ActorTokenID: opts.ActorTokenID,
			Action:       "scaling.policy_changed",
			Target:       "app:" + app.Name + "/" + p.Service,
			Result:       "ok",
			DiffSummary: DiffSummary("app", app.Name, "service", p.Service,
				"min_replicas", p.MinReplicas, "max_replicas", p.MaxReplicas,
				"target_cpu_pct", p.TargetCPUPct, "target_mem_pct", p.TargetMemPct,
				"cooldown_seconds", p.CooldownSeconds),
		}); err != nil {
			return err
		}
		out = p
		out.CreatedAt = time.Unix(0, now).UTC()
		out.UpdatedAt = out.CreatedAt
		return nil
	})
	if err != nil {
		return ScalingPolicy{}, err
	}
	return out, nil
}

// RemoveScalingPolicy 删除策略与同键运行期副本覆盖行 + 审计（同事务
// fail-closed）。覆盖行随删：策略撤销后期望副本回落 compose 快照——引擎
// 下一拍起不再以平台运行期覆盖为期望（外部改动照常走漂移判据）。
func (s *Store) RemoveScalingPolicy(ctx context.Context, appID, service string, opts ScalingPolicyOptions) error {
	return s.InTx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM scaling_policies WHERE app_id = ? AND service = ?`, appID, service)
		if err != nil {
			return fmt.Errorf("state: delete scaling policy %s/%s: %w", appID, service, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: delete scaling policy %s/%s: %w", appID, service, err)
		}
		if n == 0 {
			return ErrScalingPolicyNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM scaling_replica_overrides WHERE app_id = ? AND service = ?`, appID, service); err != nil {
			return fmt.Errorf("state: delete scaling replica override %s/%s: %w", appID, service, err)
		}
		app, aerr := tx.GetAppByID(ctx, appID)
		appName := appID
		if aerr == nil {
			appName = app.Name
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:        opts.Actor,
			ActorTokenID: opts.ActorTokenID,
			Action:       "scaling.policy_changed",
			Target:       "app:" + appName + "/" + service,
			Result:       "ok",
			DiffSummary:  DiffSummary("app", appName, "service", service, "removed", true),
		})
	})
}

// ListScalingPolicies 返回全部策略行（引擎收敛步的评估候选集；创建序无关
// ——调用方按 app/service 自行分组）。
func (s *Store) ListScalingPolicies(ctx context.Context) ([]ScalingPolicy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scalingPolicyScanCols+` FROM scaling_policies ORDER BY app_id, service`)
	if err != nil {
		return nil, fmt.Errorf("state: list scaling policies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ScalingPolicy
	for rows.Next() {
		p, err := scanScalingPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate scaling policies: %w", err)
	}
	return out, nil
}

// ScalingReplicaOverride 是一行运行期副本覆盖（autoscaler 平台写通道的期望
// 落点）。Service 是 compose 服务名（与 scaling_policies 同键域——策略撤销
// 的联动清场按同键命中；引擎侧经服务 label fleetly.process 与 Swarm 服务名
// 互译）。DeploymentID 钉期望态来源部署：覆盖仅在与漂移对账/归位收敛的
// 来源部署一致时生效（部署换代即自然失活——发布以 compose 期望重置基线，
// autoscaler 按新基线重新评估）。
type ScalingReplicaOverride struct {
	AppID        string
	Service      string // compose 服务名（与 scaling_policies 同键域）
	DeploymentID string
	Replicas     uint64
	UpdatedAt    time.Time
}

// UpsertScalingReplicaOverride 写入运行期副本覆盖。UpdatedAt 由调用方供给
// （引擎侧传引擎时钟——冷却窗时间锚与看门狗/相位锚同钟；零值回落
// nowNano，state 直调面/测试形态）。
func (s *Store) UpsertScalingReplicaOverride(ctx context.Context, ov ScalingReplicaOverride) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		return tx.UpsertScalingReplicaOverride(ctx, ov)
	})
	if err != nil {
		return fmt.Errorf("state: upsert scaling replica override %s/%s: %w", ov.AppID, ov.Service, err)
	}
	return nil
}

// UpsertScalingReplicaOverride 是事务内写覆盖（引擎动作的组合点——覆盖
// upsert 与 scaling.adjusted 事件、审计同事务 fail-closed）。
func (t *Tx) UpsertScalingReplicaOverride(ctx context.Context, ov ScalingReplicaOverride) error {
	updated := ov.UpdatedAt
	if updated.IsZero() {
		updated = time.Unix(0, nowNano()).UTC()
	}
	const q = `INSERT INTO scaling_replica_overrides (app_id, service, deployment_id, replicas, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(app_id, service) DO UPDATE SET
			deployment_id = excluded.deployment_id,
			replicas = excluded.replicas,
			updated_at = excluded.updated_at`
	if _, err := t.ExecContext(ctx, q, ov.AppID, ov.Service, ov.DeploymentID, ov.Replicas, updated.UnixNano()); err != nil {
		return fmt.Errorf("state: upsert scaling replica override %s/%s: %w", ov.AppID, ov.Service, err)
	}
	return nil
}

// GetScalingReplicaOverride 读单条覆盖（不存在 → ErrScalingOverrideNotFound）。
func (s *Store) GetScalingReplicaOverride(ctx context.Context, appID, service string) (ScalingReplicaOverride, error) {
	var ov ScalingReplicaOverride
	var updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT app_id, service, deployment_id, replicas, updated_at
			FROM scaling_replica_overrides WHERE app_id = ? AND service = ?`,
		appID, service).Scan(&ov.AppID, &ov.Service, &ov.DeploymentID, &ov.Replicas, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ScalingReplicaOverride{}, ErrScalingOverrideNotFound
		}
		return ScalingReplicaOverride{}, fmt.Errorf("state: get scaling replica override %s/%s: %w", appID, service, err)
	}
	ov.UpdatedAt = time.Unix(0, updated).UTC()
	return ov, nil
}

// ListScalingReplicaOverrides 返回应用的全部覆盖行（漂移对账/归位收敛的
// 期望钉面）。
func (s *Store) ListScalingReplicaOverrides(ctx context.Context, appID string) ([]ScalingReplicaOverride, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT app_id, service, deployment_id, replicas, updated_at
			FROM scaling_replica_overrides WHERE app_id = ? ORDER BY service`, appID)
	if err != nil {
		return nil, fmt.Errorf("state: list scaling replica overrides %s: %w", appID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ScalingReplicaOverride
	for rows.Next() {
		var ov ScalingReplicaOverride
		var updated int64
		if err := rows.Scan(&ov.AppID, &ov.Service, &ov.DeploymentID, &ov.Replicas, &updated); err != nil {
			return nil, fmt.Errorf("state: scan scaling replica override: %w", err)
		}
		ov.UpdatedAt = time.Unix(0, updated).UTC()
		out = append(out, ov)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate scaling replica overrides: %w", err)
	}
	return out, nil
}
