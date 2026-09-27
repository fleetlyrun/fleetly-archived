package state

// 程序化动态工作负载的 state 面（T 线 DT-5 / IMPL-T2-1，迁移 00026）：
// tasks 台账 + task_network_members（控制面一次性挂靠声明）+ task_quotas
//（每令牌并发/资源配额覆盖）。
//
// 纪律：
//   - 本表是任务状态机的唯一写点（API 受理/停止/删除 + 引擎 tick duty 收敛
//     全部经本文件原语）；每次转移与审计/事件同事务 fail-closed（Outbox）；
//   - env 值以 envelope 密文入行（env_cipher；明文只存活于「API 解密 →
//     底座 spec」内存链，值不进事件/审计/日志——app env 同纪律）；
//   - 配额核对在 CreateTask 的同一事务内（非终态行并发数 + CPU/内存合计），
//     fail-closed 拒绝——单写点 + 同事务使「检查↔插入」无竞态窗；
//   - 跨令牌隔离由 owner_token_id 承载（列表/取行都以属主过滤，越权恒
//     不可见——API 层把它映射为 404）。
//
// 状态机：queued → running → stopping → stopped / failed；deleting 是删除
// 墓碑（引擎移除底座服务后 DeleteTaskRow 清行）。terminal = stopped|failed。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// TaskStatus 是任务状态机词表。
type TaskStatus string

const (
	// TaskQueued 已受理、底座服务尚未收敛。
	TaskQueued TaskStatus = "queued"
	// TaskRunning 底座服务在位（任务容器已创建）。
	TaskRunning TaskStatus = "running"
	// TaskStopping 停止/到期/删除请求已受理，等待底座服务移除。
	TaskStopping TaskStatus = "stopping"
	// TaskStopped 停止完成（终态；stop_reason 记录 owner|expired|exited）。
	TaskStopped TaskStatus = "stopped"
	// TaskFailed 失败终态（收敛失败或底座任务失败——restart:none 上抛）。
	TaskFailed TaskStatus = "failed"
	// TaskDeleting 删除墓碑（终态服务的移除 + 行删除；引擎驱动）。
	TaskDeleting TaskStatus = "deleting"
)

// Terminal 报告是否为终态（配额清点与列表面过滤的判据）。
func (s TaskStatus) Terminal() bool { return s == TaskStopped || s == TaskFailed }

// Task 是一条任务台账行（只读投影；env 明文不在本结构——密文列由
// 调用方按需解密）。
type Task struct {
	ID string
	// Name 是人读名（可空；不参与寻址）。
	Name string
	// OwnerTokenID 是属主令牌（隔离与配额维度）。
	OwnerTokenID string
	Status       TaskStatus
	// Image 是钉定后的镜像引用（digest 形态）。
	Image string
	// Command / Args 是镜像 ENTRYPOINT / CMD 覆盖（空 = 镜像缺省）。
	Command []string
	Args    []string
	// EnvCipher 是 env JSON 的 envelope 密文（'' = 无 env）。
	EnvCipher string
	// ScopeKind / ScopeRef / Network 是作用域引用与解析后的网络名
	//（scope_internal 只对 task-group 有效）。
	ScopeKind     string
	ScopeRef      string
	ScopeInternal bool
	Network       string
	TTLSeconds    int64
	CPUMillis     int64
	MemoryBytes   int64
	// Service 是承载任务的 Swarm 服务名（fleetly-task-<id>）。
	Service string
	// StopReason ∈ '' | owner | expired | exited（终态/停止中的原因）。
	StopReason string
	// Error 是失败原因（failed 时非空；单行、有界长）。
	Error     string
	CreatedAt time.Time
	// StartedAt 零值 = 尚未收敛到 running。
	StartedAt time.Time
	// ExpiresAt 是 TTL 到期时刻（created_at + ttl）。
	ExpiresAt time.Time
	// StoppedAt 零值 = 尚未停止完成。
	StoppedAt time.Time
}

// TaskWrite 是一次任务受理写入。
type TaskWrite struct {
	// ID 留空自动生成 ULID（Service 名随 ID 派生，调用方先取 ID）。
	ID string
	// Name / Image / Command / Args / EnvCipher 见 Task。
	Name         string
	OwnerTokenID string
	Image        string
	Command      []string
	Args         []string
	EnvCipher    string
	// ScopeKind / ScopeRef / ScopeInternal / Network 见 Task（Network 已由
	// 调用方解析钉定）。
	ScopeKind     string
	ScopeRef      string
	ScopeInternal bool
	Network       string
	TTLSeconds    int64
	CPUMillis     int64
	MemoryBytes   int64
	// Service 是承载服务名（调用方按 naming.TaskServiceName(ID) 计算）。
	Service string
	// ExpiresAt 缺省 = now + TTLSeconds。
	ExpiresAt time.Time
	// Actor 是审计主体（空回落 human；机具令牌路径传 human + ActorTokenID，
	// 引擎路径传 system）。
	Actor        string
	ActorTokenID string
}

// TaskQuota 是每令牌配额（缺行 = 平台默认；见 DefaultTaskQuota）。
type TaskQuota struct {
	// MaxConcurrent 是并发（非终态）任务上限。
	MaxConcurrent int64
	// MaxCPUMillis / MaxMemoryBytes 是全部非终态任务的资源合计上限。
	MaxCPUMillis   int64
	MaxMemoryBytes int64
}

// 平台默认配额（DT-5「每令牌并发/资源配额」；无设置面行时的兜底——本票
// 写面仅 state 原语，Console/CLI 设置面挂 backlog，见实施记录）。
const (
	DefaultTaskMaxConcurrent  int64 = 16
	DefaultTaskMaxCPUMillis   int64 = 8000
	DefaultTaskMaxMemoryBytes int64 = 8 << 30
	DefaultTaskTTLSeconds     int64 = 600
	MinTaskTTLSeconds         int64 = 60
	MaxTaskTTLSeconds         int64 = 24 * 60 * 60
	DefaultTaskCPUMillis      int64 = 1000
	MaxTaskCPUMillis          int64 = 4000
	DefaultTaskMemoryBytes    int64 = 256 << 20
	MaxTaskMemoryBytes        int64 = 4 << 30
	DefaultTaskListLimit            = 100
	MaxTaskListLimit                = 500
	// taskFailureMessageMax 是失败原因落库/披露的单行有界长。
	taskFailureMessageMax = 512
)

// DefaultTaskQuota 返回平台默认配额。
func DefaultTaskQuota() TaskQuota {
	return TaskQuota{
		MaxConcurrent:  DefaultTaskMaxConcurrent,
		MaxCPUMillis:   DefaultTaskMaxCPUMillis,
		MaxMemoryBytes: DefaultTaskMaxMemoryBytes,
	}
}

// 任务相关哨兵错误。
var (
	// ErrTaskNotFound 表示目标任务不存在（含跨令牌越权——不可见即不存在）。
	ErrTaskNotFound = errors.New("task not found")
	// ErrTaskQuotaExceeded 表示配额 fail-closed 拒绝（并发数或资源合计超限）。
	ErrTaskQuotaExceeded = errors.New("task quota exceeded")
)

const taskRowCols = `id, name, owner_token_id, status, image, command_json, args_json, env_cipher,
	scope_kind, scope_ref, scope_internal, network, ttl_seconds, cpu_millis, memory_bytes,
	service, stop_reason, error, created_at, started_at, expires_at, stopped_at`

// CreateTask 受理一条任务：同事务核对配额（st.quota 由调用方加载；零值
// 回落平台默认）→ 插入行 → 审计 + task.created 事件（fail-closed）。
func (s *Store) CreateTask(ctx context.Context, w TaskWrite, quota TaskQuota) (Task, error) {
	if w.OwnerTokenID == "" {
		return Task{}, fmt.Errorf("state: create task: owner token id is required")
	}
	if w.Image == "" || w.Network == "" || w.Service == "" || w.ScopeKind == "" || w.ScopeRef == "" {
		return Task{}, fmt.Errorf("state: create task: image/network/service/scope are required")
	}
	if w.TTLSeconds <= 0 {
		return Task{}, fmt.Errorf("state: create task: ttl_seconds must be positive")
	}
	if quota.MaxConcurrent <= 0 {
		quota = DefaultTaskQuota()
	}
	id := w.ID
	if id == "" {
		id = ulid.Make().String()
	}
	commandJSON, err := marshalTaskStrings(w.Command)
	if err != nil {
		return Task{}, err
	}
	argsJSON, err := marshalTaskStrings(w.Args)
	if err != nil {
		return Task{}, err
	}
	actor := w.Actor
	if actor == "" {
		actor = "human"
	}
	var out Task
	err = s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		nowT := time.Unix(0, now).UTC()
		expires := w.ExpiresAt
		if expires.IsZero() {
			expires = nowT.Add(time.Duration(w.TTLSeconds) * time.Second)
		}
		// 配额核对（同事务；单写点 SQLite 使检查与插入原子）。
		var live int64
		var usedCPU, usedMemory sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1), COALESCE(SUM(cpu_millis), 0), COALESCE(SUM(memory_bytes), 0)
			 FROM tasks WHERE owner_token_id = ? AND status NOT IN ('stopped','failed')`,
			w.OwnerTokenID).Scan(&live, &usedCPU, &usedMemory); err != nil {
			return fmt.Errorf("state: task quota probe: %w", err)
		}
		if live >= quota.MaxConcurrent {
			return fmt.Errorf("%w: concurrent tasks %d at the token limit %d", ErrTaskQuotaExceeded, live, quota.MaxConcurrent)
		}
		if usedCPU.Int64+w.CPUMillis > quota.MaxCPUMillis {
			return fmt.Errorf("%w: requested cpu_millis %d would exceed the token total %d (in use %d)",
				ErrTaskQuotaExceeded, w.CPUMillis, quota.MaxCPUMillis, usedCPU.Int64)
		}
		if usedMemory.Int64+w.MemoryBytes > quota.MaxMemoryBytes {
			return fmt.Errorf("%w: requested memory_bytes %d would exceed the token total %d (in use %d)",
				ErrTaskQuotaExceeded, w.MemoryBytes, quota.MaxMemoryBytes, usedMemory.Int64)
		}
		const q = `INSERT INTO tasks (id, name, owner_token_id, status, image, command_json, args_json,
			env_cipher, scope_kind, scope_ref, scope_internal, network, ttl_seconds, cpu_millis,
			memory_bytes, service, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, q, id, w.Name, w.OwnerTokenID, string(TaskQueued), w.Image,
			commandJSON, argsJSON, w.EnvCipher, w.ScopeKind, w.ScopeRef, boolToInt(w.ScopeInternal),
			w.Network, w.TTLSeconds, w.CPUMillis, w.MemoryBytes, w.Service, now, expires.UnixNano()); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("state: create task: duplicate id %s", id)
			}
			return fmt.Errorf("state: insert task: %w", err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        actor,
			ActorTokenID: w.ActorTokenID,
			Action:       "task.create",
			Target:       "task:" + id,
			Result:       "ok",
			DiffSummary: DiffSummary("task", id, "scope_kind", w.ScopeKind, "scope_ref", w.ScopeRef,
				"network", w.Network, "ttl_seconds", fmt.Sprint(w.TTLSeconds)),
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, Event{
			Name:    "task.created",
			Subject: "task:" + id,
			Payload: DiffSummary("task", id, "image", w.Image, "scope_kind", w.ScopeKind,
				"scope_ref", w.ScopeRef, "network", w.Network, "ttl_seconds", fmt.Sprint(w.TTLSeconds)),
			At: nowT,
		}); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `SELECT `+taskRowCols+` FROM tasks WHERE id = ?`, id)
		t, err := scanTask(row)
		if err != nil {
			return err
		}
		out = t
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// GetTask 按 id 取任务行（不存在返回 ErrTaskNotFound；调用方按属主过滤
// ——本原语不做可见域判定，可见域在 API 层，跨令牌以 404 收口）。
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskRowCols+` FROM tasks WHERE id = ?`, id)
	return scanTask(row)
}

// ListTasks 返回属主令牌的任务（created_at 倒序；includeTerminal=false 只出
// 非终态）。limit ≤ 0 取缺省、超上限夹紧（读面永不无界）。
func (s *Store) ListTasks(ctx context.Context, ownerTokenID string, includeTerminal bool, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = DefaultTaskListLimit
	}
	if limit > MaxTaskListLimit {
		limit = MaxTaskListLimit
	}
	cond := ` AND status NOT IN ('stopped','failed')`
	if includeTerminal {
		cond = ``
	}
	return s.listTasksWhere(ctx, ` WHERE owner_token_id = ?`+cond+` ORDER BY created_at DESC, id DESC LIMIT ?`,
		ownerTokenID, limit)
}

// ListNonTerminalTasks 返回全部非终态任务（引擎收敛 duty 的工作集；跨令牌
// ——收敛是平台职责，配额/隔离在 API 面收口）。
func (s *Store) ListNonTerminalTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = MaxTaskListLimit
	}
	return s.listTasksWhere(ctx,
		` WHERE status NOT IN ('stopped','failed') ORDER BY created_at ASC, id ASC LIMIT ?`, limit)
}

// ExpiredTasks 返回到期未清的非终态任务（TTL 回收扫描的工作集；stopping/
// deleting 已在回收路径中，不重复入选）。
func (s *Store) ExpiredTasks(ctx context.Context, now time.Time, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = MaxTaskListLimit
	}
	return s.listTasksWhere(ctx,
		` WHERE expires_at <= ? AND status IN ('queued','running') ORDER BY expires_at ASC, id ASC LIMIT ?`,
		now.UnixNano(), limit)
}

// listTasksWhere 是任务列表的共享通道。
func (s *Store) listTasksWhere(ctx context.Context, tail string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskRowCols+` FROM tasks`+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate tasks: %w", err)
	}
	return out, nil
}

// MarkTaskRunning 把 queued 任务标为 running（CAS；changed=false = 并发
// 已离开 queued）。事件 task.started + 审计同事务。
func (s *Store) MarkTaskRunning(ctx context.Context, id string) (Task, bool, error) {
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, started_at = ? WHERE id = ? AND status = ?`,
			string(TaskRunning), nowNano(), id, string(TaskQueued))
		if err != nil {
			return fmt.Errorf("state: mark task running: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read task running count: %w", err)
		}
		if n == 0 {
			return nil // 幂等/并发翻转：调用方按当前行判定
		}
		changed = true
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor: "system", Action: "task.started", Target: "task:" + id, Result: "ok",
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, Event{Name: "task.started", Subject: "task:" + id, Payload: "{}"}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Task{}, false, err
	}
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, false, err
	}
	return t, changed, nil
}

// RequestTaskStop 请求停止任务（幂等）：queued/running → stopping（记录
// stop_reason）；已在 stopping/stopped 零变更成功。reason=expired 时事件
// task.expired（TTL 回收的披露面），owner 时事件随停止完成落 task.stopped。
func (s *Store) RequestTaskStop(ctx context.Context, id, reason, actorTokenID string) (Task, bool, error) {
	if reason == "" {
		reason = "owner"
	}
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id)
		var status string
		if err := row.Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTaskNotFound
			}
			return fmt.Errorf("state: probe task stop: %w", err)
		}
		cur := TaskStatus(status)
		if cur == TaskStopping || cur.Terminal() || cur == TaskDeleting {
			return nil // 幂等：停止中/已终态/删除中零变更
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, stop_reason = ? WHERE id = ? AND status = ?`,
			string(TaskStopping), reason, id, status)
		if err != nil {
			return fmt.Errorf("state: request task stop: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return nil // 并发翻转：幂等
		}
		changed = true
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor: "system", ActorTokenID: actorTokenID, Action: "task.stop",
			Target: "task:" + id, Result: "ok", DiffSummary: DiffSummary("reason", reason),
		}); err != nil {
			return err
		}
		if reason == "expired" {
			if _, err := tx.AppendEvent(ctx, Event{
				Name: "task.expired", Subject: "task:" + id,
				Payload: DiffSummary("task", id, "reason", reason),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Task{}, false, err
	}
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, false, err
	}
	return t, changed, nil
}

// RequestTaskDelete 请求删除任务：任何非 deleting 状态 → deleting（终态行
// 也进入删除墓碑——台账行随底座对象一起清）；审计 task.delete_requested。
func (s *Store) RequestTaskDelete(ctx context.Context, id, actorTokenID string) (Task, bool, error) {
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id)
		var status string
		if err := row.Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTaskNotFound
			}
			return fmt.Errorf("state: probe task delete: %w", err)
		}
		if TaskStatus(status) == TaskDeleting {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, delete_requested = 1 WHERE id = ?`, string(TaskDeleting), id); err != nil {
			return fmt.Errorf("state: request task delete: %w", err)
		}
		changed = true
		return tx.WriteAudit(ctx, AuditEntry{
			Actor: "system", ActorTokenID: actorTokenID, Action: "task.delete_requested",
			Target: "task:" + id, Result: "ok",
		})
	})
	if err != nil {
		return Task{}, false, err
	}
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return Task{}, false, err
	}
	return t, changed, nil
}

// MarkTaskStopped 把 stopping 任务落为 stopped 终态（CAS；事件 task.stopped
// 携带 stop_reason）。changed=false = 行已不在 stopping。
func (s *Store) MarkTaskStopped(ctx context.Context, id string) (bool, error) {
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		var reason string
		if err := tx.QueryRowContext(ctx, `SELECT stop_reason FROM tasks WHERE id = ?`, id).Scan(&reason); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTaskNotFound
			}
			return fmt.Errorf("state: probe task stopped: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, stopped_at = ? WHERE id = ? AND status = ?`,
			string(TaskStopped), nowNano(), id, string(TaskStopping))
		if err != nil {
			return fmt.Errorf("state: mark task stopped: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return nil
		}
		changed = true
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor: "system", Action: "task.stopped", Target: "task:" + id, Result: "ok",
			DiffSummary: DiffSummary("reason", reason),
		}); err != nil {
			return err
		}
		_, err = tx.AppendEvent(ctx, Event{
			Name: "task.stopped", Subject: "task:" + id,
			Payload: DiffSummary("task", id, "reason", reason),
		})
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// MarkTaskFailed 把非终态任务落为 failed 终态（CAS；message 单行有界化后
// 落行与事件）。changed=false = 行已终态。
func (s *Store) MarkTaskFailed(ctx context.Context, id, message string) (bool, error) {
	message = sanitizeTaskMessage(message)
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, error = ?, stopped_at = ? WHERE id = ? AND status NOT IN ('stopped','failed','deleting')`,
			string(TaskFailed), message, nowNano(), id)
		if err != nil {
			return fmt.Errorf("state: mark task failed: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read task failed count: %w", err)
		}
		if n == 0 {
			return nil
		}
		changed = true
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor: "system", Action: "task.failed", Target: "task:" + id, Result: "error",
			DiffSummary: DiffSummary("error", message),
		}); err != nil {
			return err
		}
		_, err = tx.AppendEvent(ctx, Event{
			Name: "task.failed", Subject: "task:" + id,
			Payload: DiffSummary("task", id, "error", message),
		})
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// DeleteTaskRow 删除 deleting 墓碑行（引擎已移除底座服务；事件
// task.deleted + 审计同事务）。changed=false = 行不在 deleting（幂等）。
func (s *Store) DeleteTaskRow(ctx context.Context, id string) (bool, error) {
	changed := false
	err := s.InTx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ? AND status = ?`, id, string(TaskDeleting))
		if err != nil {
			return fmt.Errorf("state: delete task row: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return nil
		}
		changed = true
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor: "system", Action: "task.deleted", Target: "task:" + id, Result: "ok",
		}); err != nil {
			return err
		}
		_, aerr := tx.AppendEvent(ctx, Event{Name: "task.deleted", Subject: "task:" + id, Payload: "{}"})
		return aerr
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// PruneTerminalTasksOlderThan 回收超保留窗的终态任务台账行（state janitor
// 的保留期 duty；返回删除行数——事件/审计随行删除不回溯）。
func (s *Store) PruneTerminalTasksOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tasks WHERE status IN ('stopped','failed') AND COALESCE(stopped_at, created_at) < ?`,
		cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("state: prune terminal tasks: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("state: read pruned task count: %w", err)
	}
	return n, nil
}

// LoadTaskQuota 读令牌配额覆盖（缺行回落平台默认）。
func (s *Store) LoadTaskQuota(ctx context.Context, tokenID string) (TaskQuota, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT max_concurrent, max_cpu_millis, max_memory_bytes FROM task_quotas WHERE token_id = ?`, tokenID)
	var q TaskQuota
	if err := row.Scan(&q.MaxConcurrent, &q.MaxCPUMillis, &q.MaxMemoryBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultTaskQuota(), nil
		}
		return TaskQuota{}, fmt.Errorf("state: load task quota: %w", err)
	}
	return q, nil
}

// SaveTaskQuota 写入令牌配额覆盖（幂等 upsert；值非正拒绝——配额是安全
// 默认，不允许被误配成 0 而关死或静默放开）。
func (s *Store) SaveTaskQuota(ctx context.Context, tokenID string, q TaskQuota) error {
	if tokenID == "" {
		return fmt.Errorf("state: save task quota: token id is empty")
	}
	if q.MaxConcurrent <= 0 || q.MaxCPUMillis <= 0 || q.MaxMemoryBytes <= 0 {
		return fmt.Errorf("state: save task quota: all limits must be positive")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_quotas (token_id, max_concurrent, max_cpu_millis, max_memory_bytes, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (token_id) DO UPDATE SET max_concurrent = excluded.max_concurrent,
		   max_cpu_millis = excluded.max_cpu_millis, max_memory_bytes = excluded.max_memory_bytes,
		   updated_at = excluded.updated_at`,
		tokenID, q.MaxConcurrent, q.MaxCPUMillis, q.MaxMemoryBytes, nowNano())
	if err != nil {
		return fmt.Errorf("state: save task quota: %w", err)
	}
	return nil
}

// TaskNetworkMember 是一条控制面挂靠声明（task-group 网络 ← app 服务）。
type TaskNetworkMember struct {
	// NetworkRef 是 task-group ref（网络名参数源）。
	NetworkRef string
	AppID      string
	// Service 是成员 app 的 compose 服务名。
	Service string
}

// UpsertTaskNetworkMember 记录一条挂靠声明（幂等：已存在零变更 false）。
func (s *Store) UpsertTaskNetworkMember(ctx context.Context, ref, appID, service string) (bool, error) {
	if ref == "" || appID == "" || service == "" {
		return false, fmt.Errorf("state: upsert task network member: ref/app/service are required")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO task_network_members (network_ref, app_id, service, created_at)
		 VALUES (?, ?, ?, ?) ON CONFLICT (network_ref, app_id, service) DO NOTHING`,
		ref, appID, service, nowNano())
	if err != nil {
		return false, fmt.Errorf("state: upsert task network member: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: read task network member count: %w", err)
	}
	return n > 0, nil
}

// ListTaskNetworkMembersForApp 返回 app 的全部挂靠声明（planner 投影面：
// 服务名 → 网络名的双挂输入；按 (network_ref, service) 字典序稳定）。
func (s *Store) ListTaskNetworkMembersForApp(ctx context.Context, appID string) ([]TaskNetworkMember, error) {
	return s.listTaskNetworkMembers(ctx,
		` WHERE app_id = ? ORDER BY network_ref ASC, service ASC`, appID)
}

// ListTaskNetworkMembersForRef 返回某网络的全部挂靠声明（EnsureTaskNetwork
// 的幂等对账面；角色：members 变更检测）。
func (s *Store) ListTaskNetworkMembersForRef(ctx context.Context, ref string) ([]TaskNetworkMember, error) {
	return s.listTaskNetworkMembers(ctx,
		` WHERE network_ref = ? ORDER BY app_id ASC, service ASC`, ref)
}

func (s *Store) listTaskNetworkMembers(ctx context.Context, tail string, args ...any) ([]TaskNetworkMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT network_ref, app_id, service FROM task_network_members`+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("state: list task network members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TaskNetworkMember
	for rows.Next() {
		var m TaskNetworkMember
		if err := rows.Scan(&m.NetworkRef, &m.AppID, &m.Service); err != nil {
			return nil, fmt.Errorf("state: scan task network member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate task network members: %w", err)
	}
	return out, nil
}

// scanTask 从单行构造 Task。
func scanTask(row interface{ Scan(dest ...any) error }) (Task, error) {
	var (
		t             Task
		status        string
		commandJSON   string
		argsJSON      string
		scopeInternal int
		startedAt     sql.NullInt64
		stoppedAt     sql.NullInt64
		createdAt     int64
		expiresAt     int64
	)
	if err := row.Scan(&t.ID, &t.Name, &t.OwnerTokenID, &status, &t.Image, &commandJSON, &argsJSON,
		&t.EnvCipher, &t.ScopeKind, &t.ScopeRef, &scopeInternal, &t.Network, &t.TTLSeconds,
		&t.CPUMillis, &t.MemoryBytes, &t.Service, &t.StopReason, &t.Error,
		&createdAt, &startedAt, &expiresAt, &stoppedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, fmt.Errorf("state: scan task: %w", err)
	}
	t.Status = TaskStatus(status)
	t.ScopeInternal = scopeInternal != 0
	if err := json.Unmarshal([]byte(commandJSON), &t.Command); err != nil {
		return Task{}, fmt.Errorf("state: decode task command: %w", err)
	}
	if err := json.Unmarshal([]byte(argsJSON), &t.Args); err != nil {
		return Task{}, fmt.Errorf("state: decode task args: %w", err)
	}
	t.CreatedAt = time.Unix(0, createdAt).UTC()
	t.ExpiresAt = time.Unix(0, expiresAt).UTC()
	if startedAt.Valid {
		t.StartedAt = time.Unix(0, startedAt.Int64).UTC()
	}
	if stoppedAt.Valid {
		t.StoppedAt = time.Unix(0, stoppedAt.Int64).UTC()
	}
	return t, nil
}

// marshalTaskStrings 把命令/参数切片序列化（nil → '[]'，行读面恒可解）。
func marshalTaskStrings(in []string) (string, error) {
	if len(in) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("state: marshal task strings: %w", err)
	}
	return string(raw), nil
}

// sanitizeTaskMessage 把失败原因单行化并截断到有界长（事件/审计/行三面
// 同口径；底层错误文本可能多行且超长）。
func sanitizeTaskMessage(message string) string {
	out := make([]byte, 0, len(message))
	for i := 0; i < len(message) && len(out) < taskFailureMessageMax; i++ {
		c := message[i]
		if c == '\n' || c == '\r' {
			c = ' '
		}
		out = append(out, c)
	}
	return string(out)
}
