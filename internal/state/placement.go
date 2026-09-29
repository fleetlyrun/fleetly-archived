package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// placements 表读写（stateful-placement §2.4）：绑定 = 平台层记录，平台
// 节点 ID 为锚（D-PLC-2）。state ∈ bound/blocked/unresolved（00001 词表；
// bound 即设计文档的 ok 态，ALTER 不能改约束、词表不回收）。etag 是绑定
// 行的乐观令牌（写前比对，冲突 → ErrVersionConflict——复用状态版本冲突
// 语义，E_STATE_VERSION_CONFLICT 随 API 面映射）。source ∈ platform|label：
// 首次自动选点（含 label 引导）落 platform、显式经 label 语义钉住落 label；
// label_ref 保留用户书写原值（可解释性）。
//
// 生命周期纪律（stateful-placement §2.1）：绑定仅经四类操作变更——首次
// 自动选点、显式换点（破坏性确认）、备份恢复迁移、人工重绑；本层只提供
// 读写原语，准入裁决在调用方（internal/placement）。

// PlacementState 是绑定状态位（00001 词表）。
type PlacementState string

const (
	// PlacementBound 已绑定（设计文档的 ok 态）。
	PlacementBound PlacementState = "bound"
	// PlacementBlocked 绑定节点不可用/已移除，应用 blocked。
	PlacementBlocked PlacementState = "blocked"
	// PlacementUnresolved DR 后绑定无法判定，要求显式放置（不猜测）。
	PlacementUnresolved PlacementState = "unresolved"
)

// PlacementSource 是绑定来源词表。
type PlacementSource string

const (
	// PlacementSourcePlatform 平台自动选点（含首次自动绑定本机）。
	PlacementSourcePlatform PlacementSource = "platform"
	// PlacementSourceLabel 用户 label 显式钉住（解析成功后落账）。
	PlacementSourceLabel PlacementSource = "label"
)

// Placement 是 placements 行（绑定记录投影）。
type Placement struct {
	AppID          string
	PlatformNodeID string
	State          PlacementState
	Source         PlacementSource
	// LabelRef 是用户 label 书写原值（显示名或 n_<ULID>；空 = 未声明）。
	LabelRef string
	// Reason 是进入当前状态的派生原因（如 node_down / node_gone；可空）。
	Reason string
	// Etag 是绑定行乐观令牌（每次写重新生成）。
	Etag string
	// PinnedAt 首次钉住时刻（零值 = 未钉）。
	PinnedAt  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrPlacementNotFound 表示目标应用无绑定记录（有卷应用不存在「无绑定」
// 的合法运行态——出现即调用方该走自动选点，本哨兵不外溢为 API 错误）。
var ErrPlacementNotFound = errors.New("placement not found")

// PlacementWrite 是一次绑定写入：ExpectedEtag 非空时为 CAS（compare-and-
// swap，令牌不符 → ErrVersionConflict）；空 = 无条件 upsert（首次绑定）。
type PlacementWrite struct {
	AppID          string
	PlatformNodeID string
	State          PlacementState
	Source         PlacementSource
	LabelRef       string
	Reason         string
	ExpectedEtag   string
	// Pinned 首次钉住时置位（pinned_at 取写入时刻；已有值保持不动）。
	Pinned bool
}

// GetPlacement 取绑定记录；无记录返回 ErrPlacementNotFound。
func (s *Store) GetPlacement(ctx context.Context, appID string) (Placement, error) {
	const q = `SELECT app_id, platform_node_id, state, source, label_ref, reason, etag,
		pinned_at, created_at, updated_at
		FROM placements WHERE app_id = ?`
	return scanPlacement(s.db.QueryRowContext(ctx, q, appID))
}

// PlacementByApp 批量取一组应用的绑定记录。S18-A4：ListApps 派生状态的
// N+1 收口——N 个 app 的绑定从 N 次 GetPlacement 并为一次 IN 查询。
// 空 appIDs 直接返回空 map（不发 SQL）；map 中不出现的键 = 该 app 无
// 绑定记录（ErrPlacementNotFound 的批量等价形态——无绑定是合法运行态，
// 不以错误表达）。
func (s *Store) PlacementByApp(ctx context.Context, appIDs []string) (map[string]Placement, error) {
	out := make(map[string]Placement, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	//nolint:gosec // G202：拼接的是列清单与占位符串，值全部走 ? 参数（下方 args）
	q := `SELECT app_id, platform_node_id, state, source, label_ref, reason, etag,
		pinned_at, created_at, updated_at
		FROM placements WHERE app_id IN (` + placeholders(len(appIDs)) + `)`
	args := make([]any, 0, len(appIDs))
	for _, id := range appIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: placements by app: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		p, err := scanPlacement(rows)
		if err != nil {
			return nil, err
		}
		out[p.AppID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate placements by app: %w", err)
	}
	return out, nil
}

// GetPlacement 是事务内取绑定（供 CAS 前置读取）。
func (t *Tx) GetPlacement(ctx context.Context, appID string) (Placement, error) {
	const q = `SELECT app_id, platform_node_id, state, source, label_ref, reason, etag,
		pinned_at, created_at, updated_at
		FROM placements WHERE app_id = ?`
	return scanPlacement(t.QueryRowContext(ctx, q, appID))
}

// BindPlacement 写入绑定（无条件 upsert，etag 重新生成）。CAS 语义见
// PlacementWrite.ExpectedEtag；pinned_at 只在首次置位、此后保持（数据诞生
// 点叙事），显式换点不洗掉历史钉住时刻。
func (s *Store) BindPlacement(ctx context.Context, w PlacementWrite) (Placement, error) {
	var out Placement
	err := s.InTx(ctx, func(tx *Tx) error {
		row, err := tx.BindPlacement(ctx, w)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return Placement{}, fmt.Errorf("state: bind placement %s: %w", w.AppID, err)
	}
	return out, nil
}

// BindPlacement 是事务内绑定写（供与审计/事件同事务组合）。
func (t *Tx) BindPlacement(ctx context.Context, w PlacementWrite) (Placement, error) {
	if w.State == "" {
		w.State = PlacementBound
	}
	if w.Source == "" {
		w.Source = PlacementSourcePlatform
	}
	switch w.State {
	case PlacementBound, PlacementBlocked, PlacementUnresolved:
	default:
		return Placement{}, fmt.Errorf("state: placement state %q not in {bound, blocked, unresolved}", w.State)
	}
	switch w.Source {
	case PlacementSourcePlatform, PlacementSourceLabel:
	default:
		return Placement{}, fmt.Errorf("state: placement source %q not in {platform, label}", w.Source)
	}

	if w.ExpectedEtag != "" {
		cur, err := t.GetPlacement(ctx, w.AppID)
		if err != nil {
			return Placement{}, err
		}
		if cur.Etag != w.ExpectedEtag {
			return Placement{}, fmt.Errorf("%w: placement etag stale (have %s, want %s)",
				ErrVersionConflict, cur.Etag, w.ExpectedEtag)
		}
	}

	now := nowNano()
	etag := ulid.Make().String()
	const q = `INSERT INTO placements
		(app_id, platform_node_id, state, source, label_ref, reason, etag, pinned_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET
			platform_node_id = excluded.platform_node_id,
			state = excluded.state,
			source = excluded.source,
			label_ref = excluded.label_ref,
			reason = excluded.reason,
			etag = excluded.etag,
			pinned_at = COALESCE(placements.pinned_at, excluded.pinned_at),
			updated_at = excluded.updated_at`
	var pa any
	if w.Pinned {
		pa = now
	}
	if _, err := t.ExecContext(ctx, q, w.AppID, w.PlatformNodeID, string(w.State),
		string(w.Source), w.LabelRef, w.Reason, etag, pa, now, now); err != nil {
		return Placement{}, fmt.Errorf("state: upsert placement: %w", err)
	}
	return t.GetPlacement(ctx, w.AppID)
}

// PlacementCountByNode 返回平台节点 ID → 已钉应用数（placements 权威表
// 计数，multi-node §2.6 选点三因子第二因子的数据面——「已钉数少」优先；
// 权威 SQLite，非观测缓存）。绑定全状态计入（blocked/unresolved 仍是该
// 节点的占用——节点恢复后回岗）。空表返回空 map。
func (s *Store) PlacementCountByNode(ctx context.Context) (map[string]int, error) {
	const q = `SELECT platform_node_id, COUNT(*) FROM placements
		WHERE platform_node_id <> '' GROUP BY platform_node_id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("state: placement count by node: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var nodeID string
		var n int
		if err := rows.Scan(&nodeID, &n); err != nil {
			return nil, fmt.Errorf("state: scan placement count: %w", err)
		}
		out[nodeID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate placement counts: %w", err)
	}
	return out, nil
}

// PlacementAppsByNode 返回平台节点 ID → 绑定其上的 app_id 清单（字典序；
// multi-node §2.7 NodeView.pinned_app_ids 交叉引用面的数据源——读时 join
// placements 权威表，无迁移）。绑定全状态、非空锚计入；空表返回空 map。
func (s *Store) PlacementAppsByNode(ctx context.Context) (map[string][]string, error) {
	const q = `SELECT platform_node_id, app_id FROM placements
		WHERE platform_node_id <> '' ORDER BY platform_node_id ASC, app_id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("state: placement apps by node: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var nodeID, appID string
		if err := rows.Scan(&nodeID, &appID); err != nil {
			return nil, fmt.Errorf("state: scan placement app: %w", err)
		}
		out[nodeID] = append(out[nodeID], appID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate placement apps: %w", err)
	}
	return out, nil
}

// scanPlacement 从单行构造 Placement。
func scanPlacement(row interface{ Scan(dest ...any) error }) (Placement, error) {
	var p Placement
	var state, source string
	var pinned, created, updated sql.NullInt64
	if err := row.Scan(&p.AppID, &p.PlatformNodeID, &state, &source, &p.LabelRef,
		&p.Reason, &p.Etag, &pinned, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Placement{}, ErrPlacementNotFound
		}
		return Placement{}, fmt.Errorf("state: scan placement: %w", err)
	}
	p.State = PlacementState(state)
	p.Source = PlacementSource(source)
	if pinned.Valid {
		p.PinnedAt = time.Unix(0, pinned.Int64).UTC()
	}
	p.CreatedAt = time.Unix(0, created.Int64).UTC()
	p.UpdatedAt = time.Unix(0, updated.Int64).UTC()
	return p, nil
}
