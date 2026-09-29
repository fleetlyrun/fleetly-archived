package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// app 派生状态缓存（state-model §2.10 应用状态机）：apps.derived_state 列
// （00004 加法）落 running/degraded/blocked/down。它不是第四层状态——读面
// 随时可由部署记录 + placement 状态重推导（优先级 down > blocked > degraded
// > running）；落库值是「进入/退出事件」的比较基准（app.degraded /
// app.recovered 只在状态翻转时发）。写通道带 CAS：旧值不符 = 并发翻转已
// 发生，调用方重读重算（幂等收敛，不重发事件）。

// AppDerivedState 词表（state-model §2.10）。
const (
	// AppStateRunning 以上皆否。
	AppStateRunning = "running"
	// AppStateDegraded 运行中但有不合格判定（观察窗失败 verdict=unstable /
	// 窗后不稳定 / W_DEPLOY_INSTABILITY）。
	AppStateDegraded = "degraded"
	// AppStateBlocked 平台侧无合法动作可执行（placement blocked/unresolved）。
	AppStateBlocked = "blocked"
	// AppStateDown 无有效版本或首发失败 scale=0（没有任何期望实例）。
	AppStateDown = "down"
	// AppStateSuspended 用户挂起（app Stop，00028 位；权威位直投影——DB
	// paused 同型：不是对底座的观察结论，是用户请求的投影，优先级最高）。
	// 恢复 = resume 清位 → 重部署管线收敛回既有词表值。
	AppStateSuspended = "suspended"
)

// GetAppDerivedState 读派生状态缓存（空串 = 尚未推导，读面按派生规则即时
// 计算的语义归调用方）。
func (s *Store) GetAppDerivedState(ctx context.Context, appID string) (string, error) {
	const q = `SELECT derived_state FROM apps WHERE id = ?`
	var v string
	if err := s.db.QueryRowContext(ctx, q, appID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrAppNotFound
		}
		return "", fmt.Errorf("state: read derived state: %w", err)
	}
	return v, nil
}

// GetAppDerivedState 是事务内读派生状态（事件翻转与 CAS 同事务的组合点）。
func (t *Tx) GetAppDerivedState(ctx context.Context, appID string) (string, error) {
	const q = `SELECT derived_state FROM apps WHERE id = ?`
	var v string
	if err := t.QueryRowContext(ctx, q, appID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrAppNotFound
		}
		return "", fmt.Errorf("state: read derived state: %w", err)
	}
	return v, nil
}

// ListActiveApps 返回全部 active 应用（created_at 升序；引擎运行期巡检的
// 候选集，v0.1 单机规模）。
func (s *Store) ListActiveApps(ctx context.Context) ([]App, error) {
	const q = `SELECT ` + appScanCols + ` ` + appScanFrom + ` WHERE a.lifecycle = 'active' ORDER BY a.created_at ASC, a.id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("state: list apps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate apps: %w", err)
	}
	return out, nil
}

// ListAppsByLifecycle 返回指定生命周期状态位的全部应用（created_at 升序；
// H10/MG-3：引擎 deleting 回收 duty 的候选集查询——tombstone 第二拍的
// 执行者据此发现待收敛应用）。lifecycle 必须是 AppLifecycle 词表值，
// 调用方（引擎 duty）直接传常量，本函数不做词表校验（空集 = 无待收敛）。
func (s *Store) ListAppsByLifecycle(ctx context.Context, lifecycle AppLifecycle) ([]App, error) {
	const q = `SELECT ` + appScanCols + ` ` + appScanFrom + ` WHERE a.lifecycle = ? ORDER BY a.created_at ASC, a.id ASC`
	rows, err := s.db.QueryContext(ctx, q, string(lifecycle))
	if err != nil {
		return nil, fmt.Errorf("state: list apps by lifecycle: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate apps: %w", err)
	}
	return out, nil
}

// SetAppDerivedStateIfCAS 派生状态翻转（CAS）：当前值 ≠ expected 时返回
// ErrAppDerivedStateConflict（并发翻转已发生，调用方重读重算、不重发事件）。
// 与事件写入同事务组合由调用方完成（传入同一 tx）。
func (t *Tx) SetAppDerivedState(ctx context.Context, appID, expected, next string) error {
	res, err := t.ExecContext(ctx,
		`UPDATE apps SET derived_state = ?, updated_at = ? WHERE id = ? AND derived_state = ?`,
		next, nowNano(), appID, expected)
	if err != nil {
		return fmt.Errorf("state: update derived state: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read derived state update count: %w", err)
	}
	if n == 0 {
		return ErrAppDerivedStateConflict
	}
	return nil
}

// ErrAppDerivedStateConflict 表示派生状态 CAS 落败（并发翻转已发生）。
var ErrAppDerivedStateConflict = errors.New("app derived state conflict")

// ── 漂移收敛 opt-in（T2.13，state-model §2.5 D11）───────────────────────────

// GetAppDriftConverge 读取漂移自动收敛 opt-in 位（apps.drift_converge，
// 00005 加法列）。默认关（D11：检测默认开、自动收敛默认关）——列不存在
// 语义即关，false 是唯一安全缺省。回滚失败时引擎强制清 0（D-REL-10），
// 人工经 CLI 重新置位（带审计）。
func (s *Store) GetAppDriftConverge(ctx context.Context, appID string) (bool, error) {
	const q = `SELECT drift_converge FROM apps WHERE id = ?`
	var v int64
	if err := s.db.QueryRowContext(ctx, q, appID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrAppNotFound
		}
		return false, fmt.Errorf("state: read drift converge flag: %w", err)
	}
	return v != 0, nil
}

// GetAppDriftConverge 是事务内读 opt-in 位（与清位写同事务的组合点）。
func (t *Tx) GetAppDriftConverge(ctx context.Context, appID string) (bool, error) {
	const q = `SELECT drift_converge FROM apps WHERE id = ?`
	var v int64
	if err := t.QueryRowContext(ctx, q, appID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrAppNotFound
		}
		return false, fmt.Errorf("state: read drift converge flag: %w", err)
	}
	return v != 0, nil
}

// SetAppDriftConverge 置位/清除漂移收敛 opt-in（幂等写；审计由调用方与
// 本写同事务组合——fail-closed）。
func (t *Tx) SetAppDriftConverge(ctx context.Context, appID string, on bool) error {
	res, err := t.ExecContext(ctx,
		`UPDATE apps SET drift_converge = ?, updated_at = ? WHERE id = ?`,
		boolToInt(on), nowNano(), appID)
	if err != nil {
		return fmt.Errorf("state: update drift converge flag: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read drift converge update count: %w", err)
	}
	if n == 0 {
		return ErrAppNotFound
	}
	return nil
}
