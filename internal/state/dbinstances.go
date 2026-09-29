package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// db_instances 权威态与库生命周期状态机（managed-databases 设计 §2.1/§2.3，
// E4 D-DB-1：库实例 = 独立一等资源——不复用 apps 表、不产生 deployments/
// revisions、不走部署管线）。库状态机全部转换收敛 EnterDbPhase 单写点：
// 同一事务内完成转移表校验（DatabaseTransitions 为唯一真源，穷举测试钉死）
// + CAS（WHERE state = <from>）+ 时间锚写 + 事件（Outbox）——与部署侧
// EnterPhase（deployments.go）同款纪律。收敛器/引擎侧裸状态写清零。
//
// 并发操作互斥由前置态前哨 + CAS 结构性成立（D-DB-8）：每笔操作声明合法
// 前置态，CAS 落败 → ErrDatabaseStateConflict（api 层在 S2 映射
// E_STATE_VERSION_CONFLICT 并附 current_state 与合法前置态清单）。
// 不建部署队列副本。

// DatabaseState 是库实例生命周期状态位（§2.1 七态）。
type DatabaseState string

const (
	// DatabaseProvisioning 收敛态（创建/resume/retry 共用——进入即重走服务
	// 收敛 + 健康门；§2.1「provisioning 兼作重收敛态」）。
	DatabaseProvisioning DatabaseState = "provisioning"
	// DatabaseReady 健康门通过、在役。
	DatabaseReady DatabaseState = "ready"
	// DatabaseFailed 平台收敛彻底失败、需显式 retry/人工（现场保留）。
	DatabaseFailed DatabaseState = "failed"
	// DatabaseDegraded 在役不健康、自愈可期（无平台收敛动作失败）。
	DatabaseDegraded DatabaseState = "degraded"
	// DatabasePaused 暂停（scale 0，保留服务与卷；引用方连不上是诚实暴露）。
	DatabasePaused DatabaseState = "paused"
	// DatabaseDeleting 删除受理（tombstone 第一拍；reap 步幂等重试）。
	DatabaseDeleting DatabaseState = "deleting"
	// DatabaseDeleted reap 完成（受管对象移除；名字保留期占用）。
	DatabaseDeleted DatabaseState = "deleted"
)

// DatabaseTransitions 是库生命周期合法转移表（§2.1 转移表逐行编码，唯一
// 真源；表外组合全部非法——EnterDbPhase 咬合 + 穷举测试 dbinstances_test.go
// 钉死。行 = from，列集合 = 允许的 to；deleted 终态无出边）。
//
// 设计对照（§2.1 表原文）：
//
//	— → provisioning（受理即 provisioning，无 created 态）
//	provisioning → ready / failed（健康门通过 / 收敛失败）
//	failed → provisioning（显式 retry）/ deleting（failed 可删）
//	ready → degraded / paused / deleting
//	degraded → ready / paused / deleting
//	paused → provisioning（resume 重收敛）/ deleting
//	deleting → deleted（reap 完成；deleting 无失败出边——幂等重试直至完成）
var DatabaseTransitions = map[DatabaseState][]DatabaseState{
	DatabaseProvisioning: {DatabaseReady, DatabaseFailed},
	DatabaseReady:        {DatabaseDegraded, DatabasePaused, DatabaseDeleting},
	DatabaseFailed:       {DatabaseProvisioning, DatabaseDeleting},
	DatabaseDegraded:     {DatabaseReady, DatabasePaused, DatabaseDeleting},
	DatabasePaused:       {DatabaseProvisioning, DatabaseDeleting},
	DatabaseDeleting:     {DatabaseDeleted},
	// deleted：终态无出边。
}

// CanTransitionDatabase 报告 from → to 是否合法转移（未知状态、终态出边、
// 表外组合一律非法）。
func CanTransitionDatabase(from, to DatabaseState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, t := range DatabaseTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// LegalDatabaseTransitions 返回 from 的合法目标集合（穷举测试断言用；
// from 为终态或未知状态返回空）。
func LegalDatabaseTransitions(from DatabaseState) []DatabaseState {
	return append([]DatabaseState(nil), DatabaseTransitions[from]...)
}

// AllDatabaseStates 返回主状态全词表（穷举测试遍历用）。
func AllDatabaseStates() []DatabaseState {
	return []DatabaseState{DatabaseProvisioning, DatabaseReady, DatabaseFailed,
		DatabaseDegraded, DatabasePaused, DatabaseDeleting, DatabaseDeleted}
}

// Valid 报告状态位在词表内。
func (s DatabaseState) Valid() bool {
	switch s {
	case DatabaseProvisioning, DatabaseReady, DatabaseFailed, DatabaseDegraded,
		DatabasePaused, DatabaseDeleting, DatabaseDeleted:
		return true
	}
	return false
}

// Terminal 报告是否终态（deleted；deleting 是过程态、无失败出边但可被
// reap 收口）。
func (s DatabaseState) Terminal() bool { return s == DatabaseDeleted }

// 库实例哨兵错误（HTTP 码映射随 S2 API 票落地：ErrDatabaseNotFound →
// E_DB_NOT_FOUND、ErrDatabaseStateConflict → E_STATE_VERSION_CONFLICT、
// ErrDatabaseIllegalTransition → 同族 409）。
var (
	// ErrDatabaseNotFound 表示目标库实例不存在（或已不可见）。
	ErrDatabaseNotFound = errors.New("database instance not found")
	// ErrDatabaseExists 表示同名库实例已在册（任意生命周期态名字占用）。
	ErrDatabaseExists = errors.New("database instance name already registered")
	// ErrDatabaseTerminal 表示目标处于终态/删除过程、业务写被拒（settings
	// 等就地更新的准入守卫——「任意非终态」口径含 deleting 排除）。
	ErrDatabaseTerminal = errors.New("database instance is in a terminal state")
	// ErrDatabaseIllegalTransition 表示按 DatabaseTransitions 判定的非法
	// 状态迁移（from→to 无边——确定性编码错误）。
	ErrDatabaseIllegalTransition = errors.New("database state transition illegal per transition table")
	// ErrDatabaseStateConflict 表示 CAS 竞争落败（当前状态 ≠ 声明的 from
	// ——同族乐观冲突语义；S2 映射 E_STATE_VERSION_CONFLICT）。
	ErrDatabaseStateConflict = errors.New("database state changed concurrently (CAS mismatch)")
	// ErrDatabaseAmbiguous 表示按裸名解析命中多行（v0.3 D-W0-4 二修：库名
	// project 内唯一后，跨项目同名实例合法——读面限定形支持归 S4，本哨兵
	// 显性拒绝）。
	ErrDatabaseAmbiguous = errors.New("database instance name is ambiguous across projects")
)

// DatabaseSettings 是 db_instances.settings 的 JSON 形态（限额 + 备份计划
// ——设置面只有限额与备份计划，镜像/引擎参数受管，§2.2「镜像受管」）。
// 零值字段回落模板/平台缺省（PG 1.0 CPU/1GiB、Redis 0.5/256Mi；备份
// interval_hours=24 / keep=7 / hour_utc=3，§5.4 配置键）。
type DatabaseSettings struct {
	// CPUSeconds 是 CPU 限额（核数；engine 侧换算 nano CPUs 落 spec）。
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	// MemoryBytes 是内存限额（字节）。
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
	// Backup 是备份计划（per 实例覆盖平台缺省）。
	Backup DatabaseBackupPlan `json:"backup,omitempty"`
}

// DatabaseBackupPlan 是库备份计划（§5.4 配置键 databases.backup_*）。
type DatabaseBackupPlan struct {
	// IntervalHours 是备份间隔（小时；平台缺省 24）。
	IntervalHours int `json:"interval_hours,omitempty"`
	// Keep 是保留份数（平台缺省 7；prune 沿用台账保留期删除语义）。
	Keep int `json:"keep,omitempty"`
	// HourUTC 是每日备份点（UTC 小时；平台缺省 3）。
	HourUTC int `json:"hour_utc,omitempty"`
}

// DatabaseInstance 是库实例权威态行。
type DatabaseInstance struct {
	ID       string
	Name     string
	Template string
	// ImageDigest 是模板镜像钉定引用（`repo@sha256:...`；升级 = 受控重建
	// 换值，失败 digest 归位回写——§2.2 升级语义）。
	ImageDigest string
	// Settings 是限额 + 备份计划（settings 列 JSON 反序列化形态）。
	Settings DatabaseSettings
	// CredentialCipher 是引擎凭据的 age 密文（明文永不落库/进日志/事件）。
	CredentialCipher string
	// CredentialUpdatedAt 是最近一次凭据轮换时刻（零值 = 从未轮换）。
	CredentialUpdatedAt time.Time
	// PlatformNodeID 是放置绑定（内嵌——不写 placements 表，D-DB-1）；
	// 空 = 未绑定。
	PlatformNodeID string
	State          DatabaseState
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// v0.3 W2-S3 增项目归属（rbac-teams §3.4）：project_id/team_id NOT NULL
	// （00019 收紧）+ 两个不可变 slug（projects/teams join 反解——库族命名
	// 公式三段 fleetly-db-<team>-<prj>-<name>-* 的参数源；slug 不可变故随行
	// 装载即缓存）。
	ProjectID string
	TeamID    string
	// TeamSlug / ProjectSlug 是归属两个 slug（命名公式段；不可变）。
	TeamSlug    string
	ProjectSlug string
	// LastError 是最近一次收敛失败的人读原因快照（迁移 00016；收敛器落
	// failed 时写入、retry 重收敛过健康门落 ready 时清空——诊断面跨重启
	// 存续。单行化、零凭据材料；'' = 当前无失败现场）。
	LastError string
	// DeleteVolumes 是删除受理时的卷处置选择（迁移 00016；delete API 落
	// deleting 同事务置位——reap 按位处置：false = 保留转 orphaned（默认），
	// true = 删底座卷 + 台账记 discarded）。
	DeleteVolumes bool
	// DeletingAt/DeletedAt 是 tombstone 两拍时间锚（零值 = 未进入；INTEGER
	// 列 UnixNano，与 apps 表同型）。
	DeletingAt time.Time
	DeletedAt  time.Time
}

// ValidateDatabaseName 校验库实例名：与 app 名同字符集规则（compose 顶层
// name 的 `^[a-z0-9][a-z0-9_-]*$`，internal/compose/validate.go
// specNamePattern——保证 Swarm/卷/网络/secret 命名安全；库对象前缀族
// fleetly-db-* 与 app 名族解耦，app 与库实例可重名，§2.1 名字空间独立）。
// state 包不 import compose（分层：compose 是用户输入解析层）——同规则
// 独立实现，dbinstances_test.go 钉死一致性。
func ValidateDatabaseName(name string) error {
	if name == "" {
		return errors.New("database name is empty")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			// 首字符与其余字符均允许小写字母/数字。
		case i > 0 && (r == '_' || r == '-'):
			// 下划线/连字符不允许作首字符（与 app 名规则一致）。
		default:
			return fmt.Errorf("database name %q must match ^[a-z0-9][a-z0-9_-]*$ (starts with a lowercase letter or digit, only lowercase letters/digits/-/_ allowed)", name)
		}
	}
	return nil
}

// QualifiedName 返回库实例的三段限定形 `team/prj/<name>`（D-W0-9 引用口径；
// App.QualifiedName 同式，公式由 dbinstances_test.go 对照钉死）。
func (d DatabaseInstance) QualifiedName() string {
	return d.TeamSlug + "/" + d.ProjectSlug + "/" + d.Name
}

// CreateDatabaseInstance 创建库实例行（受理即 provisioning——无 created
// 态，§2.1）。名字校验 + project 内唯一（v0.3 D-W0-4 二修：UNIQUE
// (project_id,name)，跨项目同名库实例合法；任意生命周期态名字占用，同
// app 纪律）；归属必填（W2-S3 归属管道，空 = 调用面违约显性拒绝）；
// 凭据密文必须由调用方先行生成（创建时一次，§2.5）。id 留空自动生成 ULID。
func (s *Store) CreateDatabaseInstance(ctx context.Context, in DatabaseInstance) (DatabaseInstance, error) {
	var created DatabaseInstance
	err := s.InTx(ctx, func(tx *Tx) error {
		row, err := tx.CreateDatabaseInstance(ctx, in)
		if err != nil {
			return err
		}
		created = row
		return nil
	})
	if err != nil {
		return DatabaseInstance{}, err
	}
	return created, nil
}

// CreateDatabaseInstance 是事务内创建库实例（供与事件/审计同事务组合）。
func (t *Tx) CreateDatabaseInstance(ctx context.Context, in DatabaseInstance) (DatabaseInstance, error) {
	if err := ValidateDatabaseName(in.Name); err != nil {
		return DatabaseInstance{}, fmt.Errorf("state: %w", err)
	}
	if in.Template == "" {
		return DatabaseInstance{}, errors.New("state: create database instance: template is empty")
	}
	if in.ImageDigest == "" {
		return DatabaseInstance{}, errors.New("state: create database instance: image digest is empty")
	}
	if in.CredentialCipher == "" {
		return DatabaseInstance{}, errors.New("state: create database instance: credential cipher is empty")
	}
	if in.State == "" {
		in.State = DatabaseProvisioning
	}
	if in.State != DatabaseProvisioning {
		return DatabaseInstance{}, fmt.Errorf("state: create database instance: initial state must be %q (acceptance is provisioning, got %q)", DatabaseProvisioning, in.State)
	}
	if in.ProjectID == "" || in.TeamID == "" {
		return DatabaseInstance{}, errors.New("state: create database instance: project id and team id are required (v0.3 ownership pipeline)")
	}
	if in.ID == "" {
		in.ID = ulid.Make().String()
	}
	settings, err := json.Marshal(in.Settings)
	if err != nil {
		return DatabaseInstance{}, fmt.Errorf("state: marshal database settings: %w", err)
	}
	now := nowNano()
	const q = `INSERT INTO db_instances
		(id, name, template, image_digest, settings, credential_cipher, platform_node_id, state, created_at, updated_at, project_id, team_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := t.ExecContext(ctx, q,
		in.ID, in.Name, in.Template, in.ImageDigest, string(settings), in.CredentialCipher,
		in.PlatformNodeID, string(in.State), now, now, in.ProjectID, in.TeamID); err != nil {
		if isUniqueViolation(err) {
			return DatabaseInstance{}, fmt.Errorf("%w: %s", ErrDatabaseExists, in.Name)
		}
		return DatabaseInstance{}, fmt.Errorf("state: insert database instance %s: %w", in.Name, err)
	}
	// 写后回读（slug join 反解）——返回行与读面同构。
	return t.GetDatabaseInstance(ctx, in.ID)
}

// GetDatabaseInstance 按平台 ID 取库实例；不存在返回 ErrDatabaseNotFound。
func (s *Store) GetDatabaseInstance(ctx context.Context, id string) (DatabaseInstance, error) {
	const q = `SELECT ` + dbInstanceScanCols + ` ` + dbInstanceScanFrom + ` WHERE d.id = ?`
	return scanDatabaseInstance(s.db.QueryRowContext(ctx, q, id))
}

// GetDatabaseInstance 是事务内按平台 ID 取库实例（写后回读/前置态前哨）。
func (t *Tx) GetDatabaseInstance(ctx context.Context, id string) (DatabaseInstance, error) {
	const q = `SELECT ` + dbInstanceScanCols + ` ` + dbInstanceScanFrom + ` WHERE d.id = ?`
	return scanDatabaseInstance(t.QueryRowContext(ctx, q, id))
}

// GetDatabaseInstanceByName 按名取库实例；不存在返回 ErrDatabaseNotFound。
// 跨项目同名多行命中时返回 ErrDatabaseAmbiguous（不静默取任意行——D-W0-4
// 二修后的按名解析纪律：裸名仅域内唯一时可用；限定形/ID 读面归 S4）。
func (s *Store) GetDatabaseInstanceByName(ctx context.Context, name string) (DatabaseInstance, error) {
	const q = `SELECT ` + dbInstanceScanCols + ` ` + dbInstanceScanFrom + ` WHERE d.name = ? ORDER BY d.id LIMIT 2`
	rows, err := s.db.QueryContext(ctx, q, name)
	if err != nil {
		return DatabaseInstance{}, fmt.Errorf("state: query database instance by name: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out DatabaseInstance
	n := 0
	for rows.Next() {
		n++
		if n > 1 {
			return DatabaseInstance{}, fmt.Errorf("%w: %s (qualify as team/prj/%s or reference by id)", ErrDatabaseAmbiguous, name, name)
		}
		row, err := scanDatabaseInstance(rows)
		if err != nil {
			return DatabaseInstance{}, err
		}
		out = row
	}
	if err := rows.Err(); err != nil {
		return DatabaseInstance{}, fmt.Errorf("state: iterate database instances by name: %w", err)
	}
	if n == 0 {
		return DatabaseInstance{}, ErrDatabaseNotFound
	}
	return out, nil
}

// GetDatabaseInstanceByNameInProject 按项目 + 名取库实例（创建面的占用
// 判定与归属一致性校验通道——UNIQUE(project_id,name) 语义的读取形态）。
func (s *Store) GetDatabaseInstanceByNameInProject(ctx context.Context, projectID, name string) (DatabaseInstance, error) {
	const q = `SELECT ` + dbInstanceScanCols + ` ` + dbInstanceScanFrom + ` WHERE d.project_id = ? AND d.name = ?`
	return scanDatabaseInstance(s.db.QueryRowContext(ctx, q, projectID, name))
}

// GetDatabaseInstanceByID 按平台 ID 取库实例；不存在返回 ErrDatabaseNotFound
//（api 面 resolveDatabaseRef 的 id 短路消费——与 GetAppByID 对称）。
func (s *Store) GetDatabaseInstanceByID(ctx context.Context, id string) (DatabaseInstance, error) {
	const q = `SELECT ` + dbInstanceScanCols + ` ` + dbInstanceScanFrom + ` WHERE d.id = ?`
	return scanDatabaseInstance(s.db.QueryRowContext(ctx, q, id))
}

// ListDatabaseInstances 返回全部库实例（按 name 字典序——展示面稳定序）。
func (s *Store) ListDatabaseInstances(ctx context.Context) ([]DatabaseInstance, error) {
	const q = `SELECT ` + dbInstanceScanCols + ` ` + dbInstanceScanFrom + ` ORDER BY d.name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("state: query database instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DatabaseInstance
	for rows.Next() {
		row, err := scanDatabaseInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate database instances: %w", err)
	}
	return out, nil
}

// UpdateDatabaseSettings 更新限额/备份计划（设置变更：任意非终态准入——
// §2.3 操作表，deleting/deleted 拒绝；主状态不变；限额变更 = spec 重建由
// 收敛器在 S3 承载）。
func (s *Store) UpdateDatabaseSettings(ctx context.Context, id string, settings DatabaseSettings) error {
	return s.InTx(ctx, func(tx *Tx) error {
		row, err := tx.GetDatabaseInstance(ctx, id)
		if err != nil {
			return err
		}
		if row.State == DatabaseDeleting || row.State.Terminal() {
			return fmt.Errorf("%w: %s is %s", ErrDatabaseTerminal, id, row.State)
		}
		return tx.UpdateDatabaseSettings(ctx, id, settings)
	})
}

// UpdateDatabaseSettings 是事务内设置更新（updated_at 恒重盖）。
func (t *Tx) UpdateDatabaseSettings(ctx context.Context, id string, settings DatabaseSettings) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("state: marshal database settings: %w", err)
	}
	const q = `UPDATE db_instances SET settings = ?, updated_at = ? WHERE id = ?`
	if _, err := t.ExecContext(ctx, q, string(raw), nowNano(), id); err != nil {
		return fmt.Errorf("state: update database settings %s: %w", id, err)
	}
	return nil
}

// UpdateDatabaseCredential 轮换凭据落库：密文列 + credential_updated_at
// 同拍重盖（轮换仅手动，§2.5；引擎侧热轮换/重启由适配器在 S5 承载——本
// 方法只管权威态）。凭据明文不落库、不进日志/事件。
func (s *Store) UpdateDatabaseCredential(ctx context.Context, id, cipher string) error {
	if cipher == "" {
		return errors.New("state: update database credential: cipher is empty")
	}
	return s.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.GetDatabaseInstance(ctx, id); err != nil {
			return err
		}
		now := nowNano()
		const q = `UPDATE db_instances SET credential_cipher = ?, credential_updated_at = ?, updated_at = ? WHERE id = ?`
		if _, err := tx.ExecContext(ctx, q, cipher, now, now, id); err != nil {
			return fmt.Errorf("state: update database credential %s: %w", id, err)
		}
		return nil
	})
}

// RotateDatabaseCredentialCAS 是轮换落库的乐观并发形态（S4）：密文列 +
// credential_updated_at 同拍重盖，CAS 锚 = 调用方先前读到的
// credential_updated_at——并发轮换的第二笔落败（RowsAffected=0 → 返回
// false），api 层映射 E_STATE_VERSION_CONFLICT 族（同族乐观冲突语义，
// D-DB-8）。prevUpdatedAt 零值锚定 NULL（创建态从未轮换——列可空，
// `= NULL` 恒假，必须显式 IS NULL 谓词）。凭据明文不落库、不进日志/事件。
func (s *Store) RotateDatabaseCredentialCAS(ctx context.Context, id string, prevUpdatedAt time.Time, cipher string) (bool, error) {
	if cipher == "" {
		return false, errors.New("state: rotate database credential: cipher is empty")
	}
	var ok bool
	err := s.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.GetDatabaseInstance(ctx, id); err != nil {
			return err
		}
		now := nowNano()
		prev := int64(0)
		if !prevUpdatedAt.IsZero() {
			prev = prevUpdatedAt.UnixNano()
		}
		const q = `UPDATE db_instances SET credential_cipher = ?, credential_updated_at = ?, updated_at = ?
			WHERE id = ? AND (credential_updated_at = ? OR (? = 0 AND credential_updated_at IS NULL))`
		res, err := tx.ExecContext(ctx, q, cipher, now, now, id, prev, prev)
		if err != nil {
			return fmt.Errorf("state: rotate database credential %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read database credential rotate count %s: %w", id, err)
		}
		ok = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

// SetDatabaseNodeBinding 内嵌放置绑定（D-DB-1：绑定进 db_instances 行、
// 不写 placements 表）。空串 = 解绑（数据安全由调用方裁决——绑定变更/
// rebind 编排在后续票据落地）。
func (s *Store) SetDatabaseNodeBinding(ctx context.Context, id, platformNodeID string) error {
	return s.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.GetDatabaseInstance(ctx, id); err != nil {
			return err
		}
		const q = `UPDATE db_instances SET platform_node_id = ?, updated_at = ? WHERE id = ?`
		if _, err := tx.ExecContext(ctx, q, platformNodeID, nowNano(), id); err != nil {
			return fmt.Errorf("state: set database node binding %s: %w", id, err)
		}
		return nil
	})
}

// UpdateImageDigest 回写模板镜像 digest（升级受控重建换值；失败 = digest
// 归位回写旧值——§2.2 升级语义的落库面）。
func (s *Store) UpdateImageDigest(ctx context.Context, id, imageDigest string) error {
	if imageDigest == "" {
		return errors.New("state: update image digest: digest is empty")
	}
	return s.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.GetDatabaseInstance(ctx, id); err != nil {
			return err
		}
		const q = `UPDATE db_instances SET image_digest = ?, updated_at = ? WHERE id = ?`
		if _, err := tx.ExecContext(ctx, q, imageDigest, nowNano(), id); err != nil {
			return fmt.Errorf("state: update image digest %s: %w", id, err)
		}
		return nil
	})
}

// EnterDbPhase 是库状态转换的单一写点（EnterPhase 同款纪律，§2.3 单写点
// 纪律）：同一事务内完成四件事——
//
//  1. 转移合法性校验：CanTransitionDatabase(from, to)，表外组合拒写并
//     返回 ErrDatabaseIllegalTransition；
//  2. CAS 推进：WHERE state = <from>；落败（RowsAffected=0）→ 行缺失
//     返回 ErrDatabaseNotFound、行在但已推进返回 ErrDatabaseStateConflict
//     （消息携带当前状态——S2 的 api 层映射 E_STATE_VERSION_CONFLICT 时
//     附 current_state 与合法前置态清单，D-DB-8）；
//  3. tombstone 时间锚：→ deleting 落 deleting_at、→ deleted 落
//     deleted_at（§2.1 两拍锚，与转换同拍原子生效，幂等由 CAS 保证）；
//  4. 事件追加：events 逐条经 Tx.AppendEvent（eventcode 注册表校验）写入
//     同一事务（Outbox）——转换落库与事件披露原子；CAS 落败时事务回滚、
//     不产生任何事件。
//
// Store 形态 = 独立事务薄壳；需要把转换与业务写组合在同一事务的调用方
// 使用 Tx 形态。
func (s *Store) EnterDbPhase(ctx context.Context, id string, from, to DatabaseState, events ...Event) error {
	return s.InTx(ctx, func(tx *Tx) error {
		return tx.EnterDbPhase(ctx, id, from, to, events...)
	})
}

// EnterDbPhase 是事务内的库转换写点原语（语义见 Store.EnterDbPhase）。
func (t *Tx) EnterDbPhase(ctx context.Context, id string, from, to DatabaseState, events ...Event) error {
	if !CanTransitionDatabase(from, to) {
		return fmt.Errorf("%w: db_instances %s: %s -> %s", ErrDatabaseIllegalTransition, id, from, to)
	}
	now := nowNano()
	q := `UPDATE db_instances SET state = ?, updated_at = ?`
	args := []any{string(to), now}
	switch to {
	case DatabaseDeleting:
		q += `, deleting_at = ?`
		args = append(args, now)
	case DatabaseDeleted:
		q += `, deleted_at = ?`
		args = append(args, now)
	}
	q += ` WHERE id = ? AND state = ?`
	args = append(args, id, string(from))
	res, err := t.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("state: enter database phase %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read database transition count %s: %w", id, err)
	}
	if n == 0 {
		row, rowErr := t.GetDatabaseInstance(ctx, id)
		if errors.Is(rowErr, ErrDatabaseNotFound) {
			return ErrDatabaseNotFound
		}
		if rowErr != nil {
			return rowErr
		}
		return fmt.Errorf("%w: db_instances %s: want %s, current %s", ErrDatabaseStateConflict, id, from, row.State)
	}
	for _, ev := range events {
		if _, err := t.AppendEvent(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// SetDatabaseLastError 记录/清除最近一次收敛失败的人读原因（迁移 00016）：
// 收敛器落 failed 前后写诊断快照、重收敛过健康门落 ready 时以空串清除。
// reason 单行化由调用方保证（收敛器归一）；本层只管原样落列——零凭据
// 材料纪律在写入方（internal/database）钉死。幂等：同值重写无害。
func (s *Store) SetDatabaseLastError(ctx context.Context, id, reason string) error {
	return s.InTx(ctx, func(tx *Tx) error {
		return tx.SetDatabaseLastError(ctx, id, reason)
	})
}

// SetDatabaseLastError 是事务内的失败原因写点（供与 EnterDbPhase 同事务
// 组合——failed 转移与原因落列原子生效）。
func (t *Tx) SetDatabaseLastError(ctx context.Context, id, reason string) error {
	if _, err := t.GetDatabaseInstance(ctx, id); err != nil {
		return err
	}
	const q = `UPDATE db_instances SET last_error = ?, updated_at = ? WHERE id = ?`
	if _, err := t.ExecContext(ctx, q, reason, nowNano(), id); err != nil {
		return fmt.Errorf("state: set database last error %s: %w", id, err)
	}
	return nil
}

// SetDatabaseDeleteVolumes 是事务内的卷处置选择落位（delete API 受理：与
// →deleting 转移、审计同一事务——reap 步读到的处置选择与 tombstone
// 第一拍原子一致，跨重启存续）。
func (t *Tx) SetDatabaseDeleteVolumes(ctx context.Context, id string, deleteVolumes bool) error {
	if _, err := t.GetDatabaseInstance(ctx, id); err != nil {
		return err
	}
	v := 0
	if deleteVolumes {
		v = 1
	}
	const q = `UPDATE db_instances SET delete_volumes = ?, updated_at = ? WHERE id = ?`
	if _, err := t.ExecContext(ctx, q, v, nowNano(), id); err != nil {
		return fmt.Errorf("state: set database delete volumes %s: %w", id, err)
	}
	return nil
}

// DatabaseBindingCountByNode 返回平台节点 ID → 在役库实例绑定数（E4 放置
// 选点的「已钉数」因子的库侧计数——placements 表以 app_id 为主键装不下库
// 绑定，绑定内嵌 db_instances.platform_node_id，D-DB-1；deleting/deleted
// 行不计——tombstone 不占容量账）。与 PlacementCountByNode 相加即节点的
// 全量已钉数（app + database 两类有状态绑定）。
func (s *Store) DatabaseBindingCountByNode(ctx context.Context) (map[string]int, error) {
	const q = `SELECT platform_node_id, COUNT(*) FROM db_instances
		WHERE platform_node_id <> '' AND state NOT IN ('deleting', 'deleted')
		GROUP BY platform_node_id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("state: count database bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return nil, fmt.Errorf("state: scan database binding count: %w", err)
		}
		out[node] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: iterate database binding counts: %w", err)
	}
	return out, nil
}

// dbInstanceScanCols 是库实例行查询列清单（新增列只加在此与扫描函数）。
const dbInstanceScanCols = `d.id, d.name, d.template, d.image_digest, d.settings, d.credential_cipher,
	d.credential_updated_at, d.platform_node_id, d.state, d.last_error, d.delete_volumes,
	d.created_at, d.updated_at, d.deleting_at, d.deleted_at,
	d.project_id, d.team_id, t.slug, p.slug`

// dbInstanceScanFrom 是库实例行查询的 FROM 子句（slug join 单点）。
const dbInstanceScanFrom = `FROM db_instances d
	JOIN projects p ON p.id = d.project_id
	JOIN teams t ON t.id = d.team_id`

// scanDatabaseInstance 从单行构造 DatabaseInstance（row 接口同时覆盖
// *sql.Row 与 *sql.Rows）。settings 列经 string 中转后 json.Unmarshal
// （database/sql 对非 Scanner 结构体不派生解析）。credential_cipher 随行
// 装载（写路径与 S5 适配器的凭据源）——展示投影在 api 层裁剪。
func scanDatabaseInstance(row interface{ Scan(dest ...any) error }) (DatabaseInstance, error) {
	var d DatabaseInstance
	var state, settings string
	var credentialUpdated, deleting, deleted sql.NullInt64
	var deleteVolumes int
	var created, updated int64
	if err := row.Scan(&d.ID, &d.Name, &d.Template, &d.ImageDigest, &settings,
		&d.CredentialCipher, &credentialUpdated, &d.PlatformNodeID, &state,
		&d.LastError, &deleteVolumes, &created, &updated, &deleting, &deleted,
		&d.ProjectID, &d.TeamID, &d.TeamSlug, &d.ProjectSlug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DatabaseInstance{}, ErrDatabaseNotFound
		}
		return DatabaseInstance{}, fmt.Errorf("state: scan database instance: %w", err)
	}
	if err := json.Unmarshal([]byte(settings), &d.Settings); err != nil {
		return DatabaseInstance{}, fmt.Errorf("state: unmarshal database settings %s: %w", d.ID, err)
	}
	d.State = DatabaseState(state)
	d.DeleteVolumes = deleteVolumes != 0
	if credentialUpdated.Valid {
		d.CredentialUpdatedAt = time.Unix(0, credentialUpdated.Int64).UTC()
	}
	d.CreatedAt = time.Unix(0, created).UTC()
	d.UpdatedAt = time.Unix(0, updated).UTC()
	if deleting.Valid {
		d.DeletingAt = time.Unix(0, deleting.Int64).UTC()
	}
	if deleted.Valid {
		d.DeletedAt = time.Unix(0, deleted.Int64).UTC()
	}
	return d, nil
}

// MoveDatabase 资源改派（rbac-teams §3.4/§5，W2-S3）：写归属（project_id +
// team_id 冗余列同步自目标项目行）+ 审计 db.moved 同事务 fail-closed。
// 换名重部署（库服务/网络/secret 底座对象的 team/prj 段随新归属推导）由
// 库收敛编排层执行——本原语只动权威态；同名占用守卫：目标项目已有同名
// 实例（UNIQUE(project_id,name)，D-W0-4 二修）→ ErrDatabaseExists（跨团队
// 改派先过目标项目唯一性，409，调用面映射）；非 deleted 终态行才可改派。
func (s *Store) MoveDatabase(ctx context.Context, id, toProjectID, actorUserID, actorTokenID string) (DatabaseInstance, error) {
	if toProjectID == "" {
		return DatabaseInstance{}, errors.New("state: move database: target project id is empty")
	}
	var out DatabaseInstance
	err := s.InTx(ctx, func(tx *Tx) error {
		inst, err := tx.GetDatabaseInstance(ctx, id)
		if err != nil {
			return err
		}
		if inst.State == DatabaseDeleting || inst.State.Terminal() {
			return fmt.Errorf("%w: %s is %s", ErrDatabaseTerminal, inst.Name, inst.State)
		}
		proj, err := tx.GetProject(ctx, toProjectID)
		if err != nil {
			return err
		}
		if inst.ProjectID == toProjectID {
			return fmt.Errorf("%w: database %s is already in project %s", ErrMoveSameTarget, inst.Name, toProjectID)
		}
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM db_instances WHERE project_id = ? AND name = ? AND state != 'deleted' AND id != ?`,
			toProjectID, inst.Name, id).Scan(&taken); err == nil {
			return fmt.Errorf("%w: %s already exists in the target project", ErrDatabaseExists, inst.Name)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("state: probe target project name: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE db_instances SET project_id = ?, team_id = ?, updated_at = ? WHERE id = ?`,
			proj.ID, proj.TeamID, nowNano(), id); err != nil {
			return fmt.Errorf("state: move database: %w", err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        auditActor(actorUserID),
			ActorTokenID: actorTokenID,
			Action:       "db.moved",
			Target:       "database:" + inst.ID,
			Result:       "ok",
			DiffSummary:  DiffSummary("database", inst.Name, "from_project", inst.ProjectID, "to_project", proj.ID, "to_team", proj.TeamID),
		}); err != nil {
			return err
		}
		moved, err := tx.GetDatabaseInstance(ctx, id)
		if err != nil {
			return err
		}
		out = moved
		return nil
	})
	if err != nil {
		return DatabaseInstance{}, err
	}
	return out, nil
}
