package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// deployments 表读写（release-semantics §2.3 发布状态机的行驱动）：一行 =
// 一次部署的状态机载体。status 列（00001 建、无 CHECK）承载主状态词表
// queued/preparing/building/releasing/observing/succeeded/failed/cancelled；
// 00004 加法列承载子状态（phase=blocked_waiting）、失败分流判据
// （first_healthy_at）、同记录恢复（recovery）、判定（verdict）、停机账
// （downtime_ms）、看门狗/观察窗时间标记与期望态快照（desired_spec 密文）；
// 00009 加法列 phase_started_at 是准备/构建预算基线（拾取时刻，排队等待
// 不计入预算，H11）。
//
// 迁移全部走 from→to 谓词 + RowsAffected 校验（与 builds 同纪律）：终态
// 不可逆、并发扫描下恰好一个推进者胜出。转换写点 = EnterPhase（单写点，
// 同事务完成转移校验 + 字段写 + 拾取锚点 + 事件，见函数注）；子状态/时间
// 锚/标志位等非转换就地更新走 UpdateDeployment（不带 Status）。审计由调用
// 方与业务写同事务组合（fail-closed，state-model §2.9）。

// DeploymentStatus 是发布状态机主状态（release-semantics §2.3）。
type DeploymentStatus string

const (
	// DeployQueued 已入队（同 app 互斥：等待在途部署终态）。
	DeployQueued DeploymentStatus = "queued"
	// DeployPreparing 准备中（compose 重载/放置解析/env 合并/镜像 preflight）。
	DeployPreparing DeploymentStatus = "preparing"
	// DeployBuilding 构建核对（build 层已完成则直通）。
	DeployBuilding DeploymentStatus = "building"
	// DeployReleasing 发布中（Swarm service 对账 + 健康门；含
	// blocked_waiting 子状态 = phase 列）。
	DeployReleasing DeploymentStatus = "releasing"
	// DeployObserving 观察窗（默认 60s，只告警）。
	DeployObserving DeploymentStatus = "observing"
	// DeploySucceeded 成功终态（观察窗通过；active revision 已前移）。
	DeploySucceeded DeploymentStatus = "succeeded"
	// DeployFailed 失败终态（error_code 必非空；归位/分流结果在同记录）。
	DeployFailed DeploymentStatus = "failed"
	// DeployCancelled 取消终态（先归位再落 cancelled）。
	DeployCancelled DeploymentStatus = "cancelled"
)

// Valid 报告状态是否为已定义状态位。
func (s DeploymentStatus) Valid() bool {
	switch s {
	case DeployQueued, DeployPreparing, DeployBuilding, DeployReleasing,
		DeployObserving, DeploySucceeded, DeployFailed, DeployCancelled:
		return true
	}
	return false
}

// Terminal 报告是否终态（succeeded/failed/cancelled）。
func (s DeploymentStatus) Terminal() bool {
	switch s {
	case DeploySucceeded, DeployFailed, DeployCancelled:
		return true
	}
	return false
}

// 非终态集合（重启恢复扫描与互斥检查的候选谓词）。
const nonTerminalStatuses = `('queued','preparing','building','releasing','observing')`

// deployment 子状态（phase 列词表；空 = 无子状态）。
const (
	// PhaseBlockedWaiting 发布中绑定节点 DOWN：看门狗暂停计时、可 cancel
	// （release-semantics §2.3，场景 15）。
	PhaseBlockedWaiting = "blocked_waiting"
	// PhaseInitJobs 发布中的 init job 子相位（DT-4，torchwood 线）：新
	// revision 的长驻服务对账（applyDesired）之前执行一次性 init job——迁移
	// 先于新代码跑在旧库上；job 全过才清相位晋级。init 相位优先于
	// blocked_waiting（相位列单值：init 期节点不可用由 job 看门狗兜底，
	// cancel 可用，清收在收尾路径）。
	PhaseInitJobs = "init_jobs"
)

// deployment verdict 词表（verdict 列；unstable 仅为 deployment 判定）。
const (
	// VerdictUnstable 已切流观察窗失败（app=degraded 的来源之一）。
	VerdictUnstable = "unstable"
)

// deployment recovery 词表（recovery 列；同记录恢复记录，D-REL-7）。
const (
	// RecoveryReplay 已按最后有效 revision 归位重放（未切流失败/cancel；
	// 值取 replay——restore 专指 DR 灾难恢复，见词汇表 restore 词条）。
	RecoveryReplay = "replay"
	// RecoveryBlocked 恢复被阻塞（引擎不可达等；退避重试可续跑）。
	RecoveryBlocked = "blocked"
)

// DeployRecord 是一次部署的状态机行。
type DeployRecord struct {
	ID string
	// AppID / AppName：app 外键与 compose 名（冗余快照便于跨进程渲染）。
	AppID   string
	AppName string
	// Kind ∈ deploy|rollback（00001 CHECK）：仅已切流回滚建新记录（D-REL-7）。
	Kind string
	// Status 是状态机主状态；Phase 是子状态（blocked_waiting 或空）。
	Status DeploymentStatus
	Phase  string
	// RevisionID 是成功后固化的版本行（成功时回填；可空）。
	RevisionID string
	// RecoveryOf 是 rollback 记录指向的原 deployment（可空）。
	RecoveryOf string
	// SubstrateHalted：首发失败 scale=0 保留现场（场景 14）。
	SubstrateHalted bool
	// FirstHealthyAt 是切流判据（零值 = 未切流，D-REL-4）。
	FirstHealthyAt time.Time
	// Recovery / Verdict：同记录恢复记录与终态判定（可空）。
	Recovery string
	Verdict  string
	// ErrorCode 是失败终态的注册表错误码（可空）。
	ErrorCode string
	// 停机账（stop-first 如实累计）。
	DowntimeMS        int64
	DowntimeStartedAt time.Time
	DowntimeEndedAt   time.Time
	// 看门狗/观察窗时间标记（零值 = 未进入对应阶段）。
	ReleaseStartedAt   time.Time
	WatchdogDeadlineAt time.Time
	ObserveStartedAt   time.Time
	// PhaseStartedAt 是准备/构建预算基线（00009 加法列）：queued → preparing
	// 转换（引擎拾取）时刻——排队等待不计入预算（H11）；零值（存量行未写）
	// 时消费方回落 CreatedAt。releasing 由 ReleaseStartedAt 起算，两者互不
	// 干扰。
	PhaseStartedAt time.Time
	// 期望态快照（spec_hash/env_snapshot_hash 明文哈希；desired_spec 为
	// box envelope 密文，本层不解释）。
	SpecHash        string
	EnvSnapshotHash string
	DesiredHash     string
	DesiredSpec     string
	ComposePath     string
	// CancelRequested：CLI 置位、引擎消费（曾健康拒绝并清位）。
	CancelRequested bool
	// Flags 是部署级告警/标志位（bitmask；PostWindowAlerted /
	// InstabilityWarning——L4 只告警一次与观察窗警告通过）。
	Flags int64
	// git 触发来源（T2.19 迁移 00007；空串 = API/CLI 直传 compose）。
	SourceGitSHA string
	SourceGitRef string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// 部署级标志位（deployments.flags，只增位不回收）。
const (
	// DeployFlagPostWindowAlerted：观察窗后不稳定已告警（L4 一次）。
	DeployFlagPostWindowAlerted int64 = 1 << 0
	// DeployFlagInstabilityWarning：观察窗警告通过（W_DEPLOY_INSTABILITY）。
	DeployFlagInstabilityWarning int64 = 1 << 1
)

// DeploymentNotFound 哨兵与状态迁移哨兵。
var (
	// ErrDeploymentNotFound 表示部署记录不存在。
	ErrDeploymentNotFound = errors.New("deployment not found")
	// ErrDeploymentStateTransition 表示部署状态迁移非法（终态不可逆、
	// 竞争落败等——调用方重读状态后裁决）。
	ErrDeploymentStateTransition = errors.New("invalid deployment state transition")
	// ErrDuplicateGitDeployment 表示同一 (app, sha) 已有 enqueued/active/
	// succeeded 部署、本次入队被判重拒绝（M3-4：webhook sha 幂等去重的
	// 事务内复查哨兵——check-then-insert 竞态在入队事务原语内闭合，
	// 并发重投只建一行）。消费方把它映射为 duplicate 回执（200），非错误
	// 面披露。
	ErrDuplicateGitDeployment = errors.New("duplicate git deployment for sha")
)

// DeploymentPatch 是一次行更新承载的可选字段（零值 = 不写；时间字段用
// 指针区分「不写」与「清空」——NULL 语义字段才允许置回 NULL）。
type DeploymentPatch struct {
	Status             *DeploymentStatus
	PrevStatus         *DeploymentStatus // 非 nil 时作 from 谓词（CAS 推进）
	Phase              *string
	RevisionID         *string
	SubstrateHalted    *bool
	FirstHealthyAt     *time.Time
	Recovery           *string
	Verdict            *string
	ErrorCode          *string
	DowntimeMS         *int64
	DowntimeStartedAt  *time.Time
	DowntimeEndedAt    *time.Time
	ReleaseStartedAt   *time.Time
	WatchdogDeadlineAt *time.Time
	ObserveStartedAt   *time.Time
	PhaseStartedAt     *time.Time
	SpecHash           *string
	EnvSnapshotHash    *string
	DesiredHash        *string
	DesiredSpec        *string
	ComposePath        *string
	CancelRequested    *bool
	Flags              *int64
	SourceGitSHA       *string
	SourceGitRef       *string
}

// CreateDeployment 创建 queued 部署行（发布入队；独立事务薄壳，兼容既有
// 调用方与测试夹具）。kind 取 deploy|rollback。部署入队需要事件/审计
// fail-closed 同事务（state-model §2.9，H13）的调用方必须改用
// Store.InTx + Tx.CreateDeployment 组合——本壳内的事件/审计无从共享事务。
func (s *Store) CreateDeployment(ctx context.Context, rec DeployRecord) (DeployRecord, error) {
	var created DeployRecord
	if err := s.InTx(ctx, func(tx *Tx) error {
		r, err := tx.CreateDeployment(ctx, rec)
		if err != nil {
			return err
		}
		created = r
		return nil
	}); err != nil {
		return DeployRecord{}, err
	}
	return created, nil
}

// CreateDeployment 在事务内创建 queued 部署行（发布入队原语，H13）。
// fail-closed 语义：入队调用方应把本 INSERT 与 deployment 事件、审计写在
// 同一 InTx 回调内——回调内任一写失败（含事件/审计写失败）即整体回滚，
// 部署行不落库，杜绝「部署行已存在、引擎照常执行但事件与审计缺失」的
// 审计黑洞。kind 取 deploy|rollback（校验在此层）；actor 语义在调用方
// （CLI=human、引擎=system）。
func (t *Tx) CreateDeployment(ctx context.Context, rec DeployRecord) (DeployRecord, error) {
	if rec.Kind != "deploy" && rec.Kind != "rollback" {
		return DeployRecord{}, fmt.Errorf("state: create deployment: invalid kind %q", rec.Kind)
	}
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	var recoveryOf any
	if rec.RecoveryOf != "" {
		recoveryOf = rec.RecoveryOf // FK 列：空串不落库（NULL = 无关联）
	}
	now := nowNano()
	const q = `INSERT INTO deployments
		(id, app_id, kind, status, revision_id, recovery_of, substrate_halted,
		 spec_hash, env_snapshot_hash, desired_hash, desired_spec, compose_path,
		 source_git_sha, source_git_ref, created_at, updated_at)
		VALUES (?, ?, ?, 'queued', NULL, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := t.ExecContext(ctx, q,
		rec.ID, rec.AppID, rec.Kind, recoveryOf,
		rec.SpecHash, rec.EnvSnapshotHash, rec.DesiredHash, rec.DesiredSpec, rec.ComposePath,
		rec.SourceGitSHA, rec.SourceGitRef,
		now, now)
	if err != nil {
		return DeployRecord{}, fmt.Errorf("state: insert deployment %s: %w", rec.ID, err)
	}
	rec.Status = DeployQueued
	rec.CreatedAt = time.Unix(0, now).UTC()
	rec.UpdatedAt = rec.CreatedAt
	return rec, nil
}

const deploymentScanCols = `d.id, d.app_id, a.name, d.kind, d.status, d.phase,
	d.revision_id, d.recovery_of, d.substrate_halted, d.first_healthy_at,
	d.recovery, d.verdict, d.error_code, d.downtime_ms, d.downtime_started_at,
	d.downtime_ended_at, d.release_started_at, d.watchdog_deadline_at,
	d.observe_started_at, d.phase_started_at, d.spec_hash, d.env_snapshot_hash,
	d.desired_hash, d.desired_spec, d.compose_path, d.cancel_requested, d.flags,
	d.source_git_sha, d.source_git_ref, d.created_at, d.updated_at`

const deploymentScanFrom = ` FROM deployments d JOIN apps a ON a.id = d.app_id `

// GetDeployment 按ID取部署行；不存在返回 ErrDeploymentNotFound。
func (s *Store) GetDeployment(ctx context.Context, id string) (DeployRecord, error) {
	q := `SELECT ` + deploymentScanCols + deploymentScanFrom + `WHERE d.id = ?`
	return scanDeployment(s.db.QueryRowContext(ctx, q, id))
}

// ListAppDeployments 按应用返回部署记录（created_at 倒序，至多 limit 条）。
func (s *Store) ListAppDeployments(ctx context.Context, appID string, limit int) ([]DeployRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT ` + deploymentScanCols + deploymentScanFrom +
		`WHERE d.app_id = ? ORDER BY d.created_at DESC, d.id DESC LIMIT ?`
	return queryDeployments(ctx, s.db, q, appID, limit)
}

// LatestSucceededDeploymentID 返回该 app 最近一次 succeeded 部署的 ID
//（W5-S2 访问日志部署归因的 state 侧查询面；排序与 ListAppDeployments 同
// 口径：created_at DESC, id DESC——同毫秒入队的次序以 id 兜底）。无成功
// 部署返回空串（调用方按「无归因」诚实呈现，不伪造）。非 succeeded 的
// 在途/失败部署不参与——访问日志归因的是**正在服务**的版本，切流前的
// releasing 不改变服务面。
func (s *Store) LatestSucceededDeploymentID(ctx context.Context, appID string) (string, error) {
	const q = `SELECT id FROM deployments WHERE app_id = ? AND status = 'succeeded'
		ORDER BY created_at DESC, id DESC LIMIT 1`
	var id string
	if err := s.db.QueryRowContext(ctx, q, appID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("state: latest succeeded deployment for app: %w", err)
	}
	return id, nil
}

// LatestDeploymentsByApp 批量返回一组应用各自的最近部署窗口（每 app 至多
// perApp 条，created_at 倒序——与逐 app 调 ListAppDeployments 同序同窗）。
// S18-A4：ListApps 派生状态的 N+1 收口——N 个 app 的派生输入从 N 次
// per-app 查询并为一次 IN 查询（窗口截断经 ROW_NUMBER，不拉全量行）。
// 空 appIDs 直接返回空 map（不发 SQL）；map 中不出现的键 = 该 app 无
// 部署记录。
func (s *Store) LatestDeploymentsByApp(ctx context.Context, appIDs []string, perApp int) (map[string][]DeployRecord, error) {
	out := make(map[string][]DeployRecord, len(appIDs))
	if len(appIDs) == 0 {
		return out, nil
	}
	if perApp <= 0 {
		perApp = 20
	}
	// 子查询产出裸列名（外层不可见 d./a. 前缀），外层投影列按
	// scanDeployment 的位置约定取同名裸形态（前缀剥离仅作用于表别名点号，
	// deploymentScanCols 无其他点号出现）。
	plainCols := strings.ReplaceAll(strings.ReplaceAll(deploymentScanCols, "d.", ""), "a.", "")
	// modernc sqlite ≥3.25 支持 ROW_NUMBER 窗口函数；rn 截断与
	// ListAppDeployments 的 LIMIT 语义等价，外层排序保证逐 app 窗口内
	// created_at 倒序稳定。
	//nolint:gosec // G202：拼接的是列清单与占位符串，值全部走 ? 参数（下方 args）
	q := `SELECT ` + plainCols + ` FROM (
			SELECT ` + deploymentScanCols + `,
				ROW_NUMBER() OVER (PARTITION BY d.app_id ORDER BY d.created_at DESC, d.id DESC) AS rn
			FROM deployments d JOIN apps a ON a.id = d.app_id
			WHERE d.app_id IN (` + placeholders(len(appIDs)) + `)
		) WHERE rn <= ? ORDER BY app_id, created_at DESC, id DESC`
	args := make([]any, 0, len(appIDs)+1)
	for _, id := range appIDs {
		args = append(args, id)
	}
	args = append(args, perApp)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: latest deployments by app: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		rec, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out[rec.AppID] = append(out[rec.AppID], rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate latest deployments: %w", err)
	}
	return out, nil
}

// placeholders 生成长度为 n 的 "?,?,…" IN 子句占位串（n=0 返回空串，
// 调用方自行保证非空调用）。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	p := strings.Repeat("?,", n)
	return p[:len(p)-1]
}

// ListNonTerminalDeployments 返回全部非终态部署（重启恢复扫描）。
func (s *Store) ListNonTerminalDeployments(ctx context.Context) ([]DeployRecord, error) {
	q := `SELECT ` + deploymentScanCols + deploymentScanFrom +
		`WHERE d.status IN ` + nonTerminalStatuses + ` ORDER BY d.created_at ASC, d.id ASC`
	return queryDeployments(ctx, s.db, q)
}

// NextQueuedDeployments 返回最早入队的至多 limit 条 queued 记录（FIFO）。
func (s *Store) NextQueuedDeployments(ctx context.Context, limit int) ([]DeployRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	q := `SELECT ` + deploymentScanCols + deploymentScanFrom +
		`WHERE d.status = 'queued' ORDER BY d.created_at ASC, d.id ASC LIMIT ?`
	return queryDeployments(ctx, s.db, q, limit)
}

// AppHasNonTerminalDeployment 报告应用是否存在在途部署（同 app 互斥）。
func (s *Store) AppHasNonTerminalDeployment(ctx context.Context, appID string) (bool, error) {
	q := `SELECT COUNT(1) FROM deployments WHERE app_id = ? AND status IN ` + nonTerminalStatuses
	var n int64
	if err := s.db.QueryRowContext(ctx, q, appID).Scan(&n); err != nil {
		return false, fmt.Errorf("state: count non-terminal deployments: %w", err)
	}
	return n > 0, nil
}

// UpdateDeployment 按补丁更新部署行。PrevStatus 非 nil 时为 CAS 迁移
// （当前状态 ≠ PrevStatus → ErrDeploymentStateTransition，RowsAffected=0），
// 且**先经转移表校验** PrevStatus → Status 合法（S16-C5：表外组合是确定性
// 编码错误，拒写返回 ErrIllegalTransition——即使当前行恰好仍是 from 状态
// 也不落库）；携带 Status 而 PrevStatus 为 nil 时拒绝（M3-2：状态写必须
// 带 from 谓词，引擎单写点纪律）；两者皆 nil 时不带状态谓词（子状态/时间
// 锚/告警位等就地更新，仅受终态不可逆守卫）。updated_at 恒重盖。同一补丁
// 内 Status 与字段一并原子生效。
func (s *Store) UpdateDeployment(ctx context.Context, id string, p DeploymentPatch) error {
	return updateDeployment(ctx, s.db, id, p)
}

// UpdateDeployment 是事务内的行更新原语（S18-A6：成功终态 CAS 与 revision
// 固化、事件、审计组合在同一 InTx——两事务间崩溃不再留下「revision 已
// 固化、部署仍 observing」的重放窗口）。语义与 Store 同名方法逐字一致。
func (t *Tx) UpdateDeployment(ctx context.Context, id string, p DeploymentPatch) error {
	return updateDeployment(ctx, t.Tx, id, p)
}

// EnterPhase 是部署状态转换的单一写点（v0.2 T0-V2.2，设计出处：评审报告
// 类 A 补齐①/整改方案 §9 挂账项「EnterPhase 全量单写点」）。同一事务内
// 完成四件事：
//
//  1. 转移合法性校验：CanTransitionDeployment(from, to)，表外组合拒写并
//     返回 ErrIllegalTransition（语义与 S16-C5 逐字一致）；
//  2. status/phase/error 等伴随字段写入：extra 补丁承载（ReleaseStartedAt/
//     WatchdogDeadlineAt/ErrorCode/Verdict/RevisionID/Recovery…），与 CAS
//     一并原子生效（复用 updateDeployment 内核——终态不可逆守卫与
//     RowsAffected 竞争裁决语义不变）；extra 不得携带 Status/PrevStatus
//     （转换边由 from/to 参数权威给出，双源即编码错误）；
//  3. 计时相位锚点：queued → preparing（引擎拾取边）必须携带
//     extra.PhaseStartedAt = 拾取时刻（S9/H11：预算自拾取起算、排队等待
//     不计入；时间戳由调用方时钟提供——引擎单测的假时钟下写点自取墙钟
//     会错位预算基准）。缺锚即拒：锚点漏写/误用因单写点不可发生。其余
//     边不刷新锚点——准备+构建共用拾取基线（preparing→building 刷新即
//     私自续预算）、releasing 由 watchdog_deadline_at 起算，两者互不干扰；
//  4. 既有事件追加：events 逐条经 Tx.AppendEvent（eventcode 注册表校验）
//     写入同一事务（Outbox，state-model §2.9）——转换落库与事件披露原子；
//     只允许既有事件码（不新增）；CAS 落败（RowsAffected=0）时事务回滚、
//     不产生任何事件。
//
// Store 形态 = 独立事务薄壳（独立转换调用方）；需要把转换与 revision 固化、
// 审计等业务写组合在同一事务的调用方（成功终态、回滚失败终态——S18-A6）
// 使用 Tx 形态嵌入既有 InTx。
func (s *Store) EnterPhase(ctx context.Context, id string, from, to DeploymentStatus, extra DeploymentPatch, events ...Event) error {
	return s.InTx(ctx, func(tx *Tx) error {
		return tx.EnterPhase(ctx, id, from, to, extra, events...)
	})
}

// EnterPhase 是事务内的转换写点原语（语义见 Store.EnterPhase）。
func (t *Tx) EnterPhase(ctx context.Context, id string, from, to DeploymentStatus, extra DeploymentPatch, events ...Event) error {
	if extra.Status != nil || extra.PrevStatus != nil {
		return fmt.Errorf("state: enter phase %s: patch must not carry Status/PrevStatus (the from/to arguments are authoritative)", id)
	}
	if from == DeployQueued && to == DeployPreparing && extra.PhaseStartedAt == nil {
		return fmt.Errorf("state: enter phase %s: queued -> preparing requires the phase_started_at anchor (S9/H11: budget starts at pickup, queue wait excluded)", id)
	}
	p := extra
	p.Status = &to
	p.PrevStatus = &from
	if err := updateDeployment(ctx, t.Tx, id, p); err != nil {
		return err
	}
	for _, ev := range events {
		if _, err := t.AppendEvent(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// execer 是 Store 连接池与事务共有的执行面（updateDeployment 共享内核）。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// updateDeployment 是 Store/Tx 共享的行更新内核（见 Store.UpdateDeployment
// 的语义说明）。
func updateDeployment(ctx context.Context, db execer, id string, p DeploymentPatch) error {
	if p.Status != nil {
		if !p.Status.Valid() {
			return fmt.Errorf("state: update deployment %s: unknown status %q", id, *p.Status)
		}
		// M3-2：状态写必须携带 PrevStatus（引擎单写点纪律）。裸状态写
		//（无 from 谓词）绕过转移表校验（CanTransitionDeployment）与 CAS
		// 竞争保护，只受终态不可逆守卫——任何调用点都不该以该形态改写
		// 主状态（全调用面复核：engine/api 现网状态写恒带 PrevStatus）。
		// 仅就地更新子状态/时间锚/标志位（CancelRequested/Flags 等）的
		// patch 不带 Status，不受影响。
		if p.PrevStatus == nil {
			return fmt.Errorf("state: update deployment %s: status writes must carry PrevStatus (engine single-writer discipline; a bare status write bypasses the transition table)", id)
		}
		// S16-C5：CAS 前按转移表校验 from→to（非法即拒写——engine/machine.go
		// 的文档性转移表自此在写路径咬合，机器真源见 state/machine.go）。
		if !CanTransitionDeployment(*p.PrevStatus, *p.Status) {
			return fmt.Errorf("%w: deployments %s: %s -> %s",
				ErrIllegalTransition, id, *p.PrevStatus, *p.Status)
		}
	}
	sets := []string{"updated_at = ?"}
	args := []any{nowNano()}
	if p.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*p.Status))
	}
	if p.Phase != nil {
		sets = append(sets, "phase = ?")
		args = append(args, *p.Phase)
	}
	if p.RevisionID != nil {
		sets = append(sets, "revision_id = ?")
		args = append(args, *p.RevisionID)
	}
	if p.SubstrateHalted != nil {
		sets = append(sets, "substrate_halted = ?")
		args = append(args, boolToInt(*p.SubstrateHalted))
	}
	if p.FirstHealthyAt != nil {
		sets = append(sets, "first_healthy_at = ?")
		args = append(args, p.FirstHealthyAt.UnixNano())
	}
	if p.Recovery != nil {
		sets = append(sets, "recovery = ?")
		args = append(args, *p.Recovery)
	}
	if p.Verdict != nil {
		sets = append(sets, "verdict = ?")
		args = append(args, *p.Verdict)
	}
	if p.ErrorCode != nil {
		sets = append(sets, "error_code = ?")
		args = append(args, *p.ErrorCode)
	}
	if p.DowntimeMS != nil {
		sets = append(sets, "downtime_ms = ?")
		args = append(args, *p.DowntimeMS)
	}
	if p.DowntimeStartedAt != nil {
		sets = append(sets, "downtime_started_at = ?")
		args = append(args, p.DowntimeStartedAt.UnixNano())
	}
	if p.DowntimeEndedAt != nil {
		sets = append(sets, "downtime_ended_at = ?")
		args = append(args, p.DowntimeEndedAt.UnixNano())
	}
	if p.ReleaseStartedAt != nil {
		sets = append(sets, "release_started_at = ?")
		args = append(args, p.ReleaseStartedAt.UnixNano())
	}
	if p.WatchdogDeadlineAt != nil {
		sets = append(sets, "watchdog_deadline_at = ?")
		args = append(args, p.WatchdogDeadlineAt.UnixNano())
	}
	if p.ObserveStartedAt != nil {
		sets = append(sets, "observe_started_at = ?")
		args = append(args, p.ObserveStartedAt.UnixNano())
	}
	if p.PhaseStartedAt != nil {
		sets = append(sets, "phase_started_at = ?")
		args = append(args, p.PhaseStartedAt.UnixNano())
	}
	if p.SpecHash != nil {
		sets = append(sets, "spec_hash = ?")
		args = append(args, *p.SpecHash)
	}
	if p.EnvSnapshotHash != nil {
		sets = append(sets, "env_snapshot_hash = ?")
		args = append(args, *p.EnvSnapshotHash)
	}
	if p.DesiredHash != nil {
		sets = append(sets, "desired_hash = ?")
		args = append(args, *p.DesiredHash)
	}
	if p.DesiredSpec != nil {
		sets = append(sets, "desired_spec = ?")
		args = append(args, *p.DesiredSpec)
	}
	if p.ComposePath != nil {
		sets = append(sets, "compose_path = ?")
		args = append(args, *p.ComposePath)
	}
	if p.CancelRequested != nil {
		sets = append(sets, "cancel_requested = ?")
		args = append(args, boolToInt(*p.CancelRequested))
	}
	if p.Flags != nil {
		sets = append(sets, "flags = ?")
		args = append(args, *p.Flags)
	}
	if p.SourceGitSHA != nil {
		sets = append(sets, "source_git_sha = ?")
		args = append(args, *p.SourceGitSHA)
	}
	if p.SourceGitRef != nil {
		sets = append(sets, "source_git_ref = ?")
		args = append(args, *p.SourceGitRef)
	}
	q := `UPDATE deployments SET ` + sets[0]
	for _, s := range sets[1:] {
		q += ", " + s //nolint:gosec // G202：sets 全为编译期字面量白名单，值经 ? 参数绑定
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	if p.PrevStatus != nil {
		q += ` AND status = ?`
		args = append(args, string(*p.PrevStatus))
	}
	if p.Status != nil {
		// 终态不可逆是结构性保证：任何状态改写都不得作用于终态行
		//（succeeded/failed/cancelled 无出边，release-semantics §2.3）。
		q += ` AND status NOT IN ('succeeded','failed','cancelled')`
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("state: update deployment %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read deployment update count: %w", err)
	}
	if n == 0 {
		if p.PrevStatus != nil {
			return ErrDeploymentStateTransition
		}
		return ErrDeploymentNotFound
	}
	return nil
}

// scanDeployment 从单行构造 DeployRecord（app 名经 JOIN 带出）。
func scanDeployment(row interface{ Scan(dest ...any) error }) (DeployRecord, error) {
	var r DeployRecord
	var kind, status string
	var revisionID, recoveryOf, recovery, verdict, errorCode sql.NullString
	var substrateHalted, cancelRequested int64
	var flags int64
	var downtimeMS int64
	var firstHealthy, downtimeStarted, downtimeEnded, releaseStarted,
		watchdogDeadline, observeStarted, phaseStarted sql.NullInt64
	var specHash, envSnapshotHash, desiredHash, desiredSpec, composePath string
	var sourceSHA, sourceRef string
	var phase string
	var created, updated int64
	err := row.Scan(&r.ID, &r.AppID, &r.AppName, &kind, &status, &phase,
		&revisionID, &recoveryOf, &substrateHalted, &firstHealthy,
		&recovery, &verdict, &errorCode, &downtimeMS, &downtimeStarted,
		&downtimeEnded, &releaseStarted, &watchdogDeadline, &observeStarted,
		&phaseStarted,
		&specHash, &envSnapshotHash, &desiredHash, &desiredSpec, &composePath,
		&cancelRequested, &flags, &sourceSHA, &sourceRef, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeployRecord{}, ErrDeploymentNotFound
		}
		return DeployRecord{}, fmt.Errorf("state: scan deployment: %w", err)
	}
	r.Kind = kind
	r.Status = DeploymentStatus(status)
	r.Phase = phase
	r.RevisionID = revisionID.String
	r.RecoveryOf = recoveryOf.String
	r.SubstrateHalted = substrateHalted != 0
	r.Recovery = recovery.String
	r.Verdict = verdict.String
	r.ErrorCode = errorCode.String
	r.DowntimeMS = downtimeMS
	r.FirstHealthyAt = nullTime(firstHealthy)
	r.DowntimeStartedAt = nullTime(downtimeStarted)
	r.DowntimeEndedAt = nullTime(downtimeEnded)
	r.ReleaseStartedAt = nullTime(releaseStarted)
	r.WatchdogDeadlineAt = nullTime(watchdogDeadline)
	r.ObserveStartedAt = nullTime(observeStarted)
	r.PhaseStartedAt = nullTime(phaseStarted)
	r.SpecHash = specHash
	r.EnvSnapshotHash = envSnapshotHash
	r.DesiredHash = desiredHash
	r.DesiredSpec = desiredSpec
	r.ComposePath = composePath
	r.CancelRequested = cancelRequested != 0
	r.Flags = flags
	r.SourceGitSHA = sourceSHA
	r.SourceGitRef = sourceRef
	r.CreatedAt = time.Unix(0, created).UTC()
	r.UpdatedAt = time.Unix(0, updated).UTC()
	return r, nil
}

// nullTime 把可空整数列转为 time.Time（NULL = 零值）。
func nullTime(v sql.NullInt64) time.Time {
	if !v.Valid || v.Int64 == 0 {
		return time.Time{}
	}
	return time.Unix(0, v.Int64).UTC()
}

// queryDeployments 执行多行部署查询并按序返回。
func queryDeployments(ctx context.Context, db *sql.DB, query string, args ...any) ([]DeployRecord, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("state: query deployments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DeployRecord
	for rows.Next() {
		rec, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate deployments: %w", err)
	}
	return out, nil
}

// CountGitDeploymentsForSHA 统计应用在给定 git commit 上处于 enqueued/
// active/succeeded 的部署数（T2.19 webhook 幂等去重判据：> 0 = 该 sha 已有
// 进行中或成功的部署，重投不建新记录；failed/cancelled 不计入——失败后重投
// 应允许重试）。source_git_sha 为空串的部署（API/CLI 直传）不参与匹配。
func (s *Store) CountGitDeploymentsForSHA(ctx context.Context, appID, sha string) (int64, error) {
	const q = `SELECT COUNT(1) FROM deployments
		WHERE app_id = ? AND source_git_sha = ?
		AND status IN ` + gitSHADupStatuses
	var n int64
	if err := s.db.QueryRowContext(ctx, q, appID, sha).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: count git deployments for sha: %w", err)
	}
	return n, nil
}

// gitSHADupStatuses 是 sha 去重计入的状态集（与 CountGitDeploymentsForSHA
// 同口径；Tx 事务内复查共用，两处必须一起改）。
const gitSHADupStatuses = `('queued','preparing','building','releasing','observing','succeeded')`

// CountGitDeploymentsForSHA 是事务内的同口径复查（M3-4：webhook sha 幂等
// 去重的竞态闭合——COUNT 与 INSERT 必须同事务。Store 层各事务以 BEGIN
// IMMEDIATE 起手取写锁（见 store.go dsn），两路并发同 sha 入队在写锁上
// 串行化，后到事务的 COUNT 必然看到先行事务已提交的行 → 返回
// ErrDuplicateGitDeployment 判据由调用方在建行前消费）。词法口径与
// Store.CountGitDeploymentsForSHA 逐字一致。
func (t *Tx) CountGitDeploymentsForSHA(ctx context.Context, appID, sha string) (int64, error) {
	const q = `SELECT COUNT(1) FROM deployments
		WHERE app_id = ? AND source_git_sha = ?
		AND status IN ` + gitSHADupStatuses
	var n int64
	if err := t.QueryRowContext(ctx, q, appID, sha).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: count git deployments for sha: %w", err)
	}
	return n, nil
}

// TerminalDeploymentIDsOlderThan 返回终态早于 cutoff 的部署 ID 集合
// （S18-A7：janitor 按「终态后 30 天窗」清理 deployments/<id>/ compose
// 持久化目录的判据来源；updated_at 是终态写入时刻的最近似代理——终态
// 不可逆，终态行此后只有 flag 类就地更新）。单查询全量返回，janitor 周期
// （小时级）消费。
func (s *Store) TerminalDeploymentIDsOlderThan(ctx context.Context, cutoff time.Time) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM deployments
		WHERE status IN ('succeeded','failed','cancelled') AND updated_at < ?`,
		cutoff.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("state: query terminal deployments older than cutoff: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("state: scan terminal deployment id: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate terminal deployments: %w", err)
	}
	return out, nil
}
