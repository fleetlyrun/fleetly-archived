package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// 构建记录（task-breakdown T2.8/T2.9、architecture §2.2 构建行）：状态机
// queued → building → succeeded | failed，全部迁移与审计同事务（fail-closed，
// 同 app.env_set 纪律）。构建阶段不广播事件——eventcode 注册表（35 项）无
// build.* 事件名且只增不发明；可观测性经本表读取通道（fleetly builds
// list）与 audit_log（build.create / build.start / build.finish）承载。
// 镜像身份：image_ref ↔ image_digest 即 T2.9 的本机 digest→ref 登记
// （部署引用取 digest，D9）。

// BuildStatus 是构建状态机的状态位。
type BuildStatus string

const (
	// BuildQueued 已入队待执行。
	BuildQueued BuildStatus = "queued"
	// BuildBuilding 执行中。
	BuildBuilding BuildStatus = "building"
	// BuildSucceeded 成功终态（image_digest 必非空）。
	BuildSucceeded BuildStatus = "succeeded"
	// BuildFailed 失败终态（error_code 必非空）。
	BuildFailed BuildStatus = "failed"
)

// Valid 报告状态是否为已定义状态位。
func (s BuildStatus) Valid() bool {
	switch s {
	case BuildQueued, BuildBuilding, BuildSucceeded, BuildFailed:
		return true
	}
	return false
}

// Driver 是构建驱动（与 compose 服务 build/image 两模式对应）。
type Driver string

const (
	// DriverRailpack 无 build.dockerfile → Railpack 自动检出（钉版 v0.39.0，
	// internal/build）。
	DriverRailpack Driver = "railpack"
	// DriverDockerfile 有 build.dockerfile → Dockerfile 前端（一等路径）。
	DriverDockerfile Driver = "dockerfile"
	// DriverPassthrough 仅 image 模式直通（无构建；v0.1 不建行，枚举位预留）。
	DriverPassthrough Driver = "passthrough"
)

// Valid 报告驱动是否为已定义值。
func (d Driver) Valid() bool {
	switch d {
	case DriverRailpack, DriverDockerfile, DriverPassthrough:
		return true
	}
	return false
}

// BuildRecord 是一次构建的权威记录行。
type BuildRecord struct {
	ID       string
	AppID    string
	Service  string
	Driver   Driver
	Status   BuildStatus
	ImageRef string
	// ImageDigest 是不可变镜像 ID（`sha256:<hex>`）；成功终态必非空（D9）。
	ImageDigest string
	// Request 是构建输入 JSON（构建队列的跨进程执行输入）。
	Request string
	// PlanPath / LogPath 是产物归档路径（railpack plan JSON / 构建日志）。
	PlanPath string
	LogPath  string
	// ErrorCode 是失败终态的注册表错误码。
	ErrorCode string
	CreatedAt time.Time
	// StartedAt / FinishedAt：零值 = 未开始 / 未到终态。
	StartedAt  time.Time
	FinishedAt time.Time
}

// 构建记录哨兵错误。
var (
	// ErrBuildNotFound 表示构建记录不存在。
	ErrBuildNotFound = errors.New("build not found")
	// ErrBuildStateTransition 表示构建状态迁移非法（终态不可逆、claim
	// 竞争失败等——调用方应重读状态后裁决）。
	ErrBuildStateTransition = errors.New("invalid build state transition")
)

// CreateBuild 创建 queued 构建记录（构建入队；id 留空自动生成 ULID）。
// 审计 build.create 与建行同事务（fail-closed），归因固定 system（内部
// 队列/复位路径）。调用方触发（API TriggerBuild）的归因见 CreateBuildAs。
func (s *Store) CreateBuild(ctx context.Context, rec BuildRecord) (BuildRecord, error) {
	return s.CreateBuildAs(ctx, rec, nil)
}

// CreateBuildAs 是 CreateBuild 的审计归因扩展（M4-8）：audit 非 nil 时
// build.create 审计行由调用方注入——TriggerBuild 的入队审计带调用方
// token（H14 敏感写面：base_dir 可指宿主任意目录，行为人必须可追溯）；
// nil 时回落 CreateBuild 既有 system 归因（与旧行为逐字一致）。建行与
// 审计仍同事务 fail-closed。
func (s *Store) CreateBuildAs(ctx context.Context, rec BuildRecord, audit *AuditEntry) (BuildRecord, error) {
	var created BuildRecord
	err := s.InTx(ctx, func(tx *Tx) error {
		b, err := tx.CreateBuild(ctx, rec)
		if err != nil {
			return err
		}
		created = b
		entry := AuditEntry{
			Actor:       "system",
			Action:      "build.create",
			Target:      "app:" + rec.AppID,
			Result:      "ok",
			DiffSummary: DiffSummary("build", b.ID, "service", rec.Service, "driver", string(rec.Driver)), // B4：构造器替换手拼 JSON
		}
		if audit != nil {
			entry = *audit // 调用方归因（action/target 结构由调用方完整给出）
		}
		return tx.WriteAudit(ctx, entry)
	})
	if err != nil {
		return BuildRecord{}, err
	}
	return created, nil
}

// CreateBuild 是事务内创建 queued 构建记录。
func (t *Tx) CreateBuild(ctx context.Context, rec BuildRecord) (BuildRecord, error) {
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	if rec.Status != "" && rec.Status != BuildQueued {
		return BuildRecord{}, fmt.Errorf("state: create build %s: initial status must be queued, got %s", rec.ID, rec.Status)
	}
	if !rec.Driver.Valid() {
		return BuildRecord{}, fmt.Errorf("state: create build %s: invalid driver %q", rec.ID, rec.Driver)
	}
	request := rec.Request
	if request == "" {
		request = "{}"
	}
	// app_id 可空（00027：上传构建无 app 归属——NULL 而非空串，FK 形态
	// 对非空值保持约束）。
	var appID any
	if rec.AppID != "" {
		appID = rec.AppID
	}
	now := nowNano()
	const q = `INSERT INTO builds
		(id, app_id, service, driver, status, image_ref, image_digest, request, plan_path, log_path, error_code, created_at)
		VALUES (?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, NULL, ?)`
	if _, err := t.ExecContext(ctx, q,
		rec.ID, appID, rec.Service, string(rec.Driver),
		rec.ImageRef, rec.ImageDigest, request, rec.PlanPath, rec.LogPath, now); err != nil {
		return BuildRecord{}, fmt.Errorf("state: insert build %s: %w", rec.ID, err)
	}
	return BuildRecord{
		ID:        rec.ID,
		AppID:     rec.AppID,
		Service:   rec.Service,
		Driver:    rec.Driver,
		Status:    BuildQueued,
		Request:   request,
		CreatedAt: time.Unix(0, now).UTC(),
	}, nil
}

// GetBuild 按ID取构建记录；不存在返回 ErrBuildNotFound。
func (s *Store) GetBuild(ctx context.Context, id string) (BuildRecord, error) {
	const q = `SELECT id, app_id, service, driver, status, image_ref, image_digest,
		request, plan_path, log_path, error_code, created_at, started_at, finished_at
		FROM builds WHERE id = ?`
	row := s.db.QueryRowContext(ctx, q, id)
	return scanBuild(row)
}

// ListAppBuilds 按应用返回构建记录（created_at 倒序，至多 limit 条；
// limit ≤ 0 取默认 50）。
func (s *Store) ListAppBuilds(ctx context.Context, appID string, limit int) ([]BuildRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	const q = `SELECT id, app_id, service, driver, status, image_ref, image_digest,
		request, plan_path, log_path, error_code, created_at, started_at, finished_at
		FROM builds WHERE app_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("state: query builds for app %s: %w", appID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []BuildRecord
	for rows.Next() {
		rec, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate builds: %w", err)
	}
	return out, nil
}

// NextQueuedBuilds 返回最早入队的至多 limit 条 queued 记录（FIFO 排队语义，
// 队列 worker 的候选扫描；claim 由 ClaimBuild 原子完成）。
func (s *Store) NextQueuedBuilds(ctx context.Context, limit int) ([]BuildRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	const q = `SELECT id, app_id, service, driver, status, image_ref, image_digest,
		request, plan_path, log_path, error_code, created_at, started_at, finished_at
		FROM builds WHERE status = 'queued' ORDER BY created_at ASC, id ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("state: query queued builds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BuildRecord
	for rows.Next() {
		rec, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate queued builds: %w", err)
	}
	return out, nil
}

// ClaimBuild 原子认领：queued → building（started_at 盖章），审计
// build.start 同事务。ErrBuildStateTransition 表示已被认领/终态（多 worker/
// 重复扫描竞争的安全路径——行级谓词保证恰好一个 claimer 胜出）。
func (s *Store) ClaimBuild(ctx context.Context, id string) error {
	return s.transitionBuild(ctx, id, BuildQueued, BuildBuilding, "started_at", "build.start",
		DiffSummary("build", id)) // B4：构造器替换手拼 JSON
}

// SetBuildLogPath 回填 building 行的 log_path（M2-5：构建产物目录就位即写
// ——log_path 原先唯一写点在 FinishBuildSucceeded，失败终态行恒空，失败
// 取证（日志尾部 + log_path context）落空）。行级谓词限定 building：终态
// 行不动（成功终态的 log_path 由 FinishBuildSucceeded 权威写入）；0 行更新
// （行已终态/不存在）静默成功——回填是 best-effort 观测面而非状态机事件，
// 无审计、不因竞态报错。
func (s *Store) SetBuildLogPath(ctx context.Context, id, logPath string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE builds SET log_path = ? WHERE id = ? AND status = 'building'`, logPath, id); err != nil {
		return fmt.Errorf("state: set build %s log path: %w", id, err)
	}
	return nil
}

// FinishBuildSucceeded 推进 building → succeeded 并落镜像身份（ref + 不可变
// digest，D9）。终态谓词防重复收敛；审计 build.finish 同事务。
func (s *Store) FinishBuildSucceeded(ctx context.Context, id, imageRef, imageDigest, planPath, logPath string) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		res, err := tx.ExecContext(ctx,
			`UPDATE builds SET status = 'succeeded', image_ref = ?, image_digest = ?,
				plan_path = ?, log_path = ?, finished_at = ?
			WHERE id = ? AND status = 'building'`,
			imageRef, imageDigest, planPath, logPath, now, id)
		if err != nil {
			return fmt.Errorf("state: finish build %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read finish count: %w", err)
		}
		if n == 0 {
			return ErrBuildStateTransition
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:       "system",
			Action:      "build.finish",
			Target:      "build:" + id,
			Result:      "ok",
			DiffSummary: DiffSummary("ref", imageRef, "digest", imageDigest), // B4：构造器替换手拼 JSON
		})
	})
	if err != nil {
		return fmt.Errorf("state: finish build %s succeeded: %w", id, err)
	}
	return nil
}

// FinishBuildFailed 推进 building → failed 并落注册表错误码；审计
// build.finish（result=error）同事务。
func (s *Store) FinishBuildFailed(ctx context.Context, id, errorCode string) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		res, err := tx.ExecContext(ctx,
			`UPDATE builds SET status = 'failed', error_code = ?, finished_at = ?
			WHERE id = ? AND status = 'building'`,
			errorCode, now, id)
		if err != nil {
			return fmt.Errorf("state: fail build %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read fail count: %w", err)
		}
		if n == 0 {
			return ErrBuildStateTransition
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:       "system",
			Action:      "build.finish",
			Target:      "build:" + id,
			Result:      "error",
			ErrorCode:   errorCode,
			DiffSummary: DiffSummary("build", id), // B4：构造器替换手拼 JSON
		})
	})
	if err != nil {
		return fmt.Errorf("state: finish build %s failed: %w", id, err)
	}
	return nil
}

// transitionBuild 执行 from → to 的构建状态迁移并同事务写审计。
func (s *Store) transitionBuild(ctx context.Context, id string, from, to BuildStatus, stampCol, action, diffSummary string) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE builds SET status = ?, `+stampCol+` = ?
			WHERE id = ? AND status = ?`,
			string(to), nowNano(), id, string(from))
		if err != nil {
			return fmt.Errorf("state: update build status: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read build status update count: %w", err)
		}
		if n == 0 {
			return ErrBuildStateTransition
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:       "system",
			Action:      action,
			Target:      "build:" + id,
			Result:      "ok",
			DiffSummary: diffSummary,
		})
	})
	if err != nil {
		return fmt.Errorf("state: transition build %s %s→%s: %w", id, from, to, err)
	}
	return nil
}

// FailStrandedBuild 把单条 building 行收敛为 failed 终态（队列侧兜底：执行
// 器返回后行仍停留 building——超时取消后未收敛、异常退出等路径）。CAS
// building→failed + finished_at 盖章 + error_code，审计 build.finish
// （result=error）同事务；reason 进审计 diff（builds 表只存注册表
// error_code，兜底上下文——超时预算/中断原因——的可观测落点在审计）。
func (s *Store) FailStrandedBuild(ctx context.Context, id, errorCode, reason string) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		res, err := tx.ExecContext(ctx,
			`UPDATE builds SET status = 'failed', error_code = ?, finished_at = ?
			WHERE id = ? AND status = 'building'`,
			errorCode, now, id)
		if err != nil {
			return fmt.Errorf("state: fail build %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read fail count: %w", err)
		}
		if n == 0 {
			return ErrBuildStateTransition
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor:       "system",
			Action:      "build.finish",
			Target:      "build:" + id,
			Result:      "error",
			ErrorCode:   errorCode,
			DiffSummary: DiffSummary("build", id, "reason", reason), // B4：构造器替换手拼 JSON
		})
	})
	if err != nil {
		return fmt.Errorf("state: fail build %s: %w", id, err)
	}
	return nil
}

// ResetInterruptedBuilds 复位全部 building 行为 failed 终态并返回实际复位
// 行数（daemon 启动恢复：崩溃/关停时在途构建无人收敛——NextQueuedBuilds
// 只扫 queued，遗留 building 行永不重跑，等它的部署在引擎侧空转到发布
// 超时）。逐行经 FailStrandedBuild 的 CAS building→failed（行级谓词，与
// ClaimBuild 同款竞争语义：复位时行已被并发收敛/已终态则跳过不误伤），
// finished_at 盖章、审计 build.finish 逐行同事务；reason 进审计 diff。
// queued 行不动（本方法只扫 building；重启后队列自然重扫认领）。
func (s *Store) ResetInterruptedBuilds(ctx context.Context, errorCode, reason string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM builds WHERE status = 'building'`)
	if err != nil {
		return 0, fmt.Errorf("state: query building builds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("state: scan building build: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("state: iterate building builds: %w", err)
	}
	reset := 0
	for _, id := range ids {
		if err := s.FailStrandedBuild(ctx, id, errorCode, reason); err != nil {
			if errors.Is(err, ErrBuildStateTransition) {
				continue // 行已离开 building（并发收敛竞争落败）
			}
			return reset, fmt.Errorf("state: reset interrupted build %s: %w", id, err)
		}
		reset++
	}
	return reset, nil
}

// FindBuildsByDigest 按不可变镜像 ID 反查登记（digest→ref 映射读通道，
// T2.9 镜像身份；返回含该 digest 的全部成功构建行）。
func (s *Store) FindBuildsByDigest(ctx context.Context, digest string) ([]BuildRecord, error) {
	const q = `SELECT id, app_id, service, driver, status, image_ref, image_digest,
		request, plan_path, log_path, error_code, created_at, started_at, finished_at
		FROM builds WHERE image_digest = ? ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, digest)
	if err != nil {
		return nil, fmt.Errorf("state: query builds by digest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BuildRecord
	for rows.Next() {
		rec, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate builds by digest: %w", err)
	}
	return out, nil
}

// ListNonTerminalBuilds 返回全部非终态构建行（S18-A10：janitor 非终态超龄
// 扫描的输入——正常态由队列 worker 认领/收敛，超龄停留即状态机漏洞的显性
// 化告警；只读不自愈）。queued 行基线 created_at，building 行基线 started_at。
func (s *Store) ListNonTerminalBuilds(ctx context.Context) ([]BuildRecord, error) {
	const q = `SELECT id, app_id, service, driver, status, image_ref, image_digest,
		request, plan_path, log_path, error_code, created_at, started_at, finished_at
		FROM builds WHERE status IN ('queued','building') ORDER BY created_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("state: query non-terminal builds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BuildRecord
	for rows.Next() {
		rec, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate non-terminal builds: %w", err)
	}
	return out, nil
}

// PruneTerminalBuildsOlderThan 分批删除终态早于 cutoff 的构建行（S18-A10：
// builds 台账 90 天保留窗；finished_at 是终态时刻，零值行不删——防御存量
// 脏数据）。分批形态与事件/审计清理一致（见 PruneExpiredEvents）。
func (s *Store) PruneTerminalBuildsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.deleteBatched(ctx,
		`DELETE FROM builds WHERE id IN (
			SELECT id FROM builds
			WHERE status IN ('succeeded','failed') AND finished_at > 0 AND finished_at < ?
			LIMIT ?)`,
		cutoff.UnixNano(), pruneBatchSize)
}

// deleteBatched 循环执行「子查询限定批量的 DELETE」直至不足一批
// （S18-A10：单语句全表 DELETE 在大台账下长时间持写锁，分批把每批锁窗口
// 收敛到 500 行；modernc SQLite 不支持 DELETE...LIMIT 语法，子查询形态
// 承载批量语义）。
func (s *Store) deleteBatched(ctx context.Context, query string, args ...any) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, query, args...)
		if err != nil {
			return total, fmt.Errorf("state: batched delete: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("state: read batched delete count: %w", err)
		}
		total += n
		if n < pruneBatchSize {
			return total, nil
		}
	}
}

// scanBuild 从单行构造 BuildRecord（row 接口同时覆盖 *sql.Row 与 *sql.Rows）。
func scanBuild(row interface{ Scan(dest ...any) error }) (BuildRecord, error) {
	var r BuildRecord
	var appID sql.NullString
	var driver, status string
	var created int64
	var started, finished sql.NullInt64
	var errorCode sql.NullString
	err := row.Scan(&r.ID, &appID, &r.Service, &driver, &status, &r.ImageRef, &r.ImageDigest,
		&r.Request, &r.PlanPath, &r.LogPath, &errorCode, &created, &started, &finished)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BuildRecord{}, ErrBuildNotFound
		}
		return BuildRecord{}, fmt.Errorf("state: scan build: %w", err)
	}
	// app_id 可空（00027：NULL = 上传构建——无 app 归属）。
	r.AppID = appID.String
	r.Driver = Driver(driver)
	r.Status = BuildStatus(status)
	if errorCode.Valid {
		r.ErrorCode = errorCode.String
	}
	r.CreatedAt = time.Unix(0, created).UTC()
	if started.Valid {
		r.StartedAt = time.Unix(0, started.Int64).UTC()
	}
	if finished.Valid {
		r.FinishedAt = time.Unix(0, finished.Int64).UTC()
	}
	return r, nil
}
