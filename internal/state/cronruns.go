package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// cron_runs 运行台账读写通道（E5 Cron，object-storage 设计 §8 契约面）：
// 每条触发链（到点/手动）一行——started 在途行承载完成检测与启动残留收口
// 的扫描谓词；终态行（succeeded|failed|timeout|skipped）是「每 schedule
// 最近 20 条」留存窗的计数对象（janitor 增项，PruneCronRunsKeepPerSchedule）。
// 写路径全部经 Tx（与 cron.* 事件同事务 = Outbox，state-model §2.9）。

// CronRun 状态词表（status，无 CHECK——00001 词表先例，非法值经写入通道
// 防御）。§8 列出的是终态值；started 是在途位（job 服务已创建、尚未收口）。
const (
	// CronRunStarted 在途（job 服务已创建、等待任务终态或看门狗）。
	CronRunStarted = "started"
	// CronRunSucceeded 任务 complete（一次性 job 的成功终态）。
	CronRunSucceeded = "succeeded"
	// CronRunFailed 任务 failed/rejected/shutdown（不重试——restart-condition=none）。
	CronRunFailed = "failed"
	// CronRunTimeout 看门狗超时（默认 10m，label fleetly.cron.timeout 覆盖）。
	CronRunTimeout = "timeout"
	// CronRunSkipped 触发被跳过（不创建 job；原因在 skip_reason）。
	CronRunSkipped = "skipped"
)

// CronRun skip_reason 词表（skipped 行的原因细分）。
const (
	// CronSkipOverlap 该 schedule 已有在途 run（重叠 skip，max-concurrent 1）。
	CronSkipOverlap = "overlap"
	// CronSkipNodeUnavailable 绑定节点前哨不通过（不 ready/已移除——与控制
	// 面停机 skip 同型，架构 §4.3 触发前哨行）。
	CronSkipNodeUnavailable = "node_unavailable"
	// CronSkipMissedDowntime 控制面停机期间错过的点（不补跑；每 schedule
	// 启动后披露一次）。
	CronSkipMissedDowntime = "missed_downtime"
	// CronSkipInterrupted 启动残留收口：控制面重启打断的在途 run（任务终态
	// 无法判定时的保守收口）。
	CronSkipInterrupted = "interrupted"
)

// CronRunKeeper 是每 schedule 的留存窗宽度（架构 §4.3 留存行：最近 20 条）。
const CronRunKeeper = 20

// ErrCronRunNotStarted 表示收口目标不在在途态（已被并发收口/已是终态）——
// FinishCronRun 的幂等收敛信号，调用方按 no-op 处置。
var ErrCronRunNotStarted = errors.New("cron run is not in the started state")

// CronRun 是一次 cron 触发的台账行（Times 零值 = 列为空）。
type CronRun struct {
	ID string
	// AppID/Service 是归属应用平台 ID 与 compose 服务名。
	AppID   string
	Service string
	// Expression 是触发时的五段标准表达式（触发面如实抄录）。
	Expression string
	// ScheduledAt 是命中的 cron 点（手动触发 = 触发时刻）。
	ScheduledAt time.Time
	// StartedAt 是 job 服务创建成功时刻（skipped 行为零值）。
	StartedAt time.Time
	// FinishedAt 是终态收口时刻（在途/skipped 行为零值）。
	FinishedAt time.Time
	// Status 取 CronRun* 词表。
	Status string
	// SkipReason 是 skipped 行的原因（CronSkip* 词表；其余行为空）。
	SkipReason string
	// JobService 是一次性 job 的 Swarm 服务名（skipped 行为空）。
	JobService string
	// Error 是失败/超时原因摘要（单行化；成功/skipped 为空）。
	Error string
}

// CreateCronRun 是事务内建行（与 cron.* 事件同事务组合）。
func (t *Tx) CreateCronRun(ctx context.Context, r CronRun) (CronRun, error) {
	if r.ID == "" {
		r.ID = ulid.Make().String()
	}
	if r.Status == "" {
		return r, errors.New("state: create cron run: status is empty")
	}
	var started, finished any
	if !r.StartedAt.IsZero() {
		started = r.StartedAt.UnixNano()
	}
	if !r.FinishedAt.IsZero() {
		finished = r.FinishedAt.UnixNano()
	}
	const q = `INSERT INTO cron_runs
		(id, app_id, service, expression, scheduled_at, started_at, finished_at, status, skip_reason, job_service, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := t.ExecContext(ctx, q,
		r.ID, r.AppID, r.Service, r.Expression, r.ScheduledAt.UnixNano(),
		started, finished, r.Status, r.SkipReason, r.JobService, r.Error); err != nil {
		return r, fmt.Errorf("state: insert cron run: %w", err)
	}
	return r, nil
}

// FinishCronRun 是事务内在途行收口（status 置终态 + finished_at + error）。
// 谓词限 status='started'：行已被并发收口（启动残留收口与完成检测竞态）
// 时零行更新、返回 ErrCronRunNotStarted（幂等收敛，调用方按 no-op 处置）。
func (t *Tx) FinishCronRun(ctx context.Context, id, status, runError string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := t.ExecContext(ctx,
		`UPDATE cron_runs SET status = ?, finished_at = ?, error = ?
		WHERE id = ? AND status = ?`,
		status, at.UnixNano(), runError, id, CronRunStarted)
	if err != nil {
		return fmt.Errorf("state: finish cron run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read cron run finish count: %w", err)
	}
	if n == 0 {
		return ErrCronRunNotStarted
	}
	return nil
}

// ListInFlightCronRuns 返回全部在途行（status='started'，scheduled_at 升序
// ——完成检测与启动残留收口的扫描面）。
func (s *Store) ListInFlightCronRuns(ctx context.Context) ([]CronRun, error) {
	const q = `SELECT id, app_id, service, expression, scheduled_at, started_at, finished_at, status, skip_reason, job_service, error
		FROM cron_runs WHERE status = ? ORDER BY scheduled_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, CronRunStarted)
	if err != nil {
		return nil, fmt.Errorf("state: query in-flight cron runs: %w", err)
	}
	return scanCronRuns(rows)
}

// LatestInFlightCronRun 返回该 schedule 的在途行（重叠 skip 判据：
// max-concurrent 1）；无在途返回 (zero, false, nil)。
func (s *Store) LatestInFlightCronRun(ctx context.Context, appID, service string) (CronRun, bool, error) {
	const q = `SELECT id, app_id, service, expression, scheduled_at, started_at, finished_at, status, skip_reason, job_service, error
		FROM cron_runs WHERE app_id = ? AND service = ? AND status = ?
		ORDER BY scheduled_at DESC, id DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, appID, service, CronRunStarted)
	r, err := scanCronRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CronRun{}, false, nil
	}
	if err != nil {
		return CronRun{}, false, err
	}
	return r, true, nil
}

// LatestCronRun 返回该 schedule 最新一行（任意状态；启动后「停机错过点」
// 的披露判据——上一handled点之后存在整点未处理即 missed_during_downtime）。
// 无任何行返回 (zero, false, nil)。
func (s *Store) LatestCronRun(ctx context.Context, appID, service string) (CronRun, bool, error) {
	const q = `SELECT id, app_id, service, expression, scheduled_at, started_at, finished_at, status, skip_reason, job_service, error
		FROM cron_runs WHERE app_id = ? AND service = ?
		ORDER BY scheduled_at DESC, id DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, appID, service)
	r, err := scanCronRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CronRun{}, false, nil
	}
	if err != nil {
		return CronRun{}, false, err
	}
	return r, true, nil
}

// ListCronRuns 返回该 app 的台账行（scheduled_at 倒序；service 非空时收窄
// 到单 schedule——ListCronRuns API/CLI 读面）。limit ≤0 回落 20。
func (s *Store) ListCronRuns(ctx context.Context, appID, service string, limit int) ([]CronRun, error) {
	if limit <= 0 {
		limit = CronRunKeeper
	}
	q := `SELECT id, app_id, service, expression, scheduled_at, started_at, finished_at, status, skip_reason, job_service, error
		FROM cron_runs WHERE app_id = ?`
	args := []any{appID}
	if service != "" {
		q += ` AND service = ?`
		args = append(args, service)
	}
	q += ` ORDER BY scheduled_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: query cron runs: %w", err)
	}
	return scanCronRuns(rows)
}

// PruneCronRunsKeepPerSchedule 是 janitor 增项：每 (app_id, service) 保留
// scheduled_at 最新的 keep 条，其余删行。窗口函数按分区编号（modernc
// SQLite 支持窗口函数）；幂等，返回删除条数。清理失败只告警不中断整轮
// （janitor 各步同纪律）。
func (s *Store) PruneCronRunsKeepPerSchedule(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = CronRunKeeper
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM cron_runs WHERE id IN (
		SELECT id FROM (
			SELECT id, ROW_NUMBER() OVER (
				PARTITION BY app_id, service
				ORDER BY scheduled_at DESC, id DESC
			) AS rn FROM cron_runs
		) WHERE rn > ?)`, keep)
	if err != nil {
		return 0, fmt.Errorf("state: prune cron runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("state: read cron runs prune count: %w", err)
	}
	return n, nil
}

// scanCronRun 从单行构造 CronRun。
func scanCronRun(row interface{ Scan(dest ...any) error }) (CronRun, error) {
	var r CronRun
	var scheduled int64
	var started, finished sql.NullInt64
	var skipReason, jobService, runError sql.NullString
	if err := row.Scan(&r.ID, &r.AppID, &r.Service, &r.Expression, &scheduled,
		&started, &finished, &r.Status, &skipReason, &jobService, &runError); err != nil {
		return CronRun{}, err
	}
	r.ScheduledAt = time.Unix(0, scheduled).UTC()
	if started.Valid {
		r.StartedAt = time.Unix(0, started.Int64).UTC()
	}
	if finished.Valid {
		r.FinishedAt = time.Unix(0, finished.Int64).UTC()
	}
	r.SkipReason = skipReason.String
	r.JobService = jobService.String
	r.Error = runError.String
	return r, nil
}

// scanCronRuns 迭代多行结果。
func scanCronRuns(rows *sql.Rows) ([]CronRun, error) {
	defer func() { _ = rows.Close() }()
	var out []CronRun
	for rows.Next() {
		r, err := scanCronRun(rows)
		if err != nil {
			return nil, fmt.Errorf("state: scan cron run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate cron runs: %w", err)
	}
	return out, nil
}
