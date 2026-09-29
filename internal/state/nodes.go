package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// nodes 表 = 派生观测缓存（state-model §2.2）：Docker/Swarm 观测快照，
// 可整表重建，禁止用于决策。每行携带读契约字段 observed_at/stale；
// last_seen_at 是平台观测语义（最近一次成功观测到该节点的时刻）——
// 平台不承诺任何底座侧存活时间戳（Swarm 不暴露该类数据），本包与
// 上层文案均不得如此声称（wording_test.go 断言）。
//
// 写路径：observer 全量 resync（SyncNodeObservations，同事务 upsert +
// 修剪消失节点）与底座不可达置陈旧（MarkAllNodesStale）。读路径仅限
// 展示/诊断；写操作的乐观令牌一律走写前直读（resolve.go）。

// CachedNode 是 nodes 表行（观测缓存读投影）。
type CachedNode struct {
	SwarmNodeID      string
	Hostname         string
	State            string
	Availability     string
	IsManager        bool
	SubstrateVersion uint64
	Labels           map[string]string
	// LastSeenAt 平台最近一次成功观测到该节点的时刻（观测语义，非底座
	// 提供的时间戳）。
	LastSeenAt time.Time
	// ObservedAt 本行快照写入时刻（读契约字段）。
	ObservedAt time.Time
	// Stale 底座最近一次不可达时被整表置位；为真时本行内容可能是旧值，
	// 禁止当事实消费（读契约字段）。
	Stale bool
}

// SyncNodeObservations 全量写入一拍观测快照：upsert 全部节点（observed_at
// = at、stale 清零、last_seen_at 取 max(旧值, at)）并在同事务内修剪快照
// 中已消失的节点行（缓存可整表重建，消失即删；节点下线叙事属 events 表
// 历史，产品事件随后续票接入）。
func (s *Store) SyncNodeObservations(ctx context.Context, nodes []SubstrateNode, at time.Time) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		for _, n := range nodes {
			labels, err := json.Marshal(n.Labels)
			if err != nil {
				return fmt.Errorf("state: marshal node labels %s: %w", n.SwarmNodeID, err)
			}
			// last_seen_at 单调：只在观测拍推进，失败置陈旧时保持旧值。
			const upsert = `INSERT INTO nodes
				(swarm_node_id, hostname, state, availability, is_manager, substrate_version,
				 labels, last_seen_at, observed_at, stale)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
				ON CONFLICT(swarm_node_id) DO UPDATE SET
					hostname = excluded.hostname,
					state = excluded.state,
					availability = excluded.availability,
					is_manager = excluded.is_manager,
					substrate_version = excluded.substrate_version,
					labels = excluded.labels,
					last_seen_at = MAX(nodes.last_seen_at, excluded.last_seen_at),
					observed_at = excluded.observed_at,
					stale = 0`
			if _, err := tx.ExecContext(ctx, upsert,
				n.SwarmNodeID, n.Hostname, n.State, n.Availability, boolToInt(n.IsManager),
				n.Version.Index, string(labels), at.UnixNano(), at.UnixNano()); err != nil {
				return fmt.Errorf("state: upsert node %s: %w", n.SwarmNodeID, err)
			}
		}
		// 修剪消失节点（无节点时清空全表——空快照也是事实）。
		if len(nodes) == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM nodes`); err != nil {
				return fmt.Errorf("state: clear nodes: %w", err)
			}
			return nil
		}
		placeholders := strings.Repeat("?,", len(nodes))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(nodes))
		for _, n := range nodes {
			args = append(args, n.SwarmNodeID)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM nodes WHERE swarm_node_id NOT IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("state: prune nodes: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: sync node observations: %w", err)
	}
	return nil
}

// MarkAllNodesStale 将全部缓存行置 stale=1（底座不可达的降级读契约：
// 内容可能是旧值），observed_at/last_seen_at 不动（仍指最后一次真实
// 观测）。无行时为 no-op。
func (s *Store) MarkAllNodesStale(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET stale = 1`); err != nil {
		return fmt.Errorf("state: mark nodes stale: %w", err)
	}
	return nil
}

// ListCachedNodes 返回全部观测缓存行（按 swarm_node_id 字典序）。
// 展示/诊断专用：决策路径禁用（见包注释与 state-model §2.2）。
func (s *Store) ListCachedNodes(ctx context.Context) ([]CachedNode, error) {
	const q = `SELECT swarm_node_id, hostname, state, availability, is_manager,
		substrate_version, labels, last_seen_at, observed_at, stale
		FROM nodes ORDER BY swarm_node_id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("state: query nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CachedNode
	for rows.Next() {
		var n CachedNode
		var labelsJSON string
		var lastSeen, observed int64
		var isManager, stale int
		if err := rows.Scan(&n.SwarmNodeID, &n.Hostname, &n.State, &n.Availability,
			&isManager, &n.SubstrateVersion, &labelsJSON, &lastSeen, &observed, &stale); err != nil {
			return nil, fmt.Errorf("state: scan node: %w", err)
		}
		n.IsManager = isManager == 1
		n.Stale = stale == 1
		n.LastSeenAt = time.Unix(0, lastSeen).UTC()
		n.ObservedAt = time.Unix(0, observed).UTC()
		if labelsJSON != "" && labelsJSON != "{}" {
			_ = json.Unmarshal([]byte(labelsJSON), &n.Labels) // 缓存 label 解析失败不致命，置空即可
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate nodes: %w", err)
	}
	return out, nil
}

// UpsertRuntimeNodeRef 维护平台节点 ID ↔ Swarm node ID 适配器映射
// （state-model §2.3：映射本身可从节点 label 反建；身份 label 被删改后
// 对账器自动重放的依据）。返回是否发生了变更（供调用方决定是否审计）。
// 同一 swarm_node_id 换绑到新 platform_id 视为换机/重建，拒绝并返回
// ErrVersionConflict 复用冲突语义（重绑走人工 rebind，平台不猜测）。
func (s *Store) UpsertRuntimeNodeRef(ctx context.Context, platformID, swarmNodeID string, anchoredAt time.Time) (bool, error) {
	var changed bool
	err := s.InTx(ctx, func(tx *Tx) error {
		const q = `INSERT INTO runtime_node_refs (platform_id, swarm_node_id, created_at, updated_at, anchored_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(platform_id) DO UPDATE SET
				swarm_node_id = excluded.swarm_node_id,
				updated_at = excluded.updated_at,
				anchored_at = excluded.anchored_at
			WHERE runtime_node_refs.swarm_node_id <> excluded.swarm_node_id`
		at := anchoredAt.UnixNano()
		if anchoredAt.IsZero() {
			at = nowNano()
		}
		res, err := tx.ExecContext(ctx, q, platformID, swarmNodeID, at, at, at)
		if err != nil {
			// swarm_node_id UNIQUE 冲突 = 该底座节点已被另一平台 ID 占用
			// （换机/重建场景），保守拒绝，不做自动消解。
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: swarm node %s already mapped to another platform id", ErrVersionConflict, swarmNodeID)
			}
			return fmt.Errorf("state: upsert runtime node ref: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read runtime node ref count: %w", err)
		}
		changed = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// RuntimeNodeRef 是适配器映射行。
type RuntimeNodeRef struct {
	PlatformID  string
	SwarmNodeID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	AnchoredAt  time.Time
}

// GetRuntimeNodeRef 按 platform_id 取映射；不存在返回 ("", ErrAppNotFound)
// 语义即未锚定——用独立哨兵 ErrRefNotFound 表达。
var ErrRefNotFound = fmt.Errorf("runtime node ref not found")

// GetRuntimeNodeRef 返回映射行（未锚定返回 ErrRefNotFound）。
func (s *Store) GetRuntimeNodeRef(ctx context.Context, platformID string) (RuntimeNodeRef, error) {
	const q = `SELECT platform_id, swarm_node_id, created_at, updated_at, anchored_at
		FROM runtime_node_refs WHERE platform_id = ?`
	var r RuntimeNodeRef
	var created, updated int64
	var anchored sql.NullInt64
	err := s.db.QueryRowContext(ctx, q, platformID).Scan(&r.PlatformID, &r.SwarmNodeID, &created, &updated, &anchored)
	if err == sql.ErrNoRows {
		return RuntimeNodeRef{}, ErrRefNotFound
	}
	if err != nil {
		return RuntimeNodeRef{}, fmt.Errorf("state: get runtime node ref: %w", err)
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	r.UpdatedAt = time.Unix(0, updated).UTC()
	if anchored.Valid {
		r.AnchoredAt = time.Unix(0, anchored.Int64).UTC()
	}
	return r, nil
}

// GetRuntimeNodeRefBySwarmID 按 swarm node ID 反查映射（锚定循环的冲突
// 判定面：label 缺失时判断该底座节点是否已有平台身份占用）。未锚定返回
// ErrRefNotFound。
func (s *Store) GetRuntimeNodeRefBySwarmID(ctx context.Context, swarmNodeID string) (RuntimeNodeRef, error) {
	const q = `SELECT platform_id, swarm_node_id, created_at, updated_at, anchored_at
		FROM runtime_node_refs WHERE swarm_node_id = ?`
	var r RuntimeNodeRef
	var created, updated int64
	var anchored sql.NullInt64
	err := s.db.QueryRowContext(ctx, q, swarmNodeID).Scan(&r.PlatformID, &r.SwarmNodeID, &created, &updated, &anchored)
	if err == sql.ErrNoRows {
		return RuntimeNodeRef{}, ErrRefNotFound
	}
	if err != nil {
		return RuntimeNodeRef{}, fmt.Errorf("state: get runtime node ref by swarm id: %w", err)
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	r.UpdatedAt = time.Unix(0, updated).UTC()
	if anchored.Valid {
		r.AnchoredAt = time.Unix(0, anchored.Int64).UTC()
	}
	return r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
