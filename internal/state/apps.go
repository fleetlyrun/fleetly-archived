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

// 应用权威态与 tombstone-first 删除（state-model §2.6）：删除走
// deleting → deleted 状态位 + 保留期；恢复（DB 回填/备份重放）不得把
// deleted 行翻回 active（「恢复不复活」，apps_test.go 钉死）。state 层
// 用哨兵错误表达生命周期冲突，HTTP 码映射随 API 面票落地。

// AppLifecycle 是应用生命周期状态位。
type AppLifecycle string

const (
	// LifecycleActive 正常在册。
	LifecycleActive AppLifecycle = "active"
	// LifecycleDeleting 删除进行中（deleting，等待清理与保留期）。
	LifecycleDeleting AppLifecycle = "deleting"
	// LifecycleDeleted 已删除（tombstone，保留期内名字仍占用）。
	LifecycleDeleted AppLifecycle = "deleted"
)

// 应用生命周期哨兵错误。
var (
	// ErrAppExists 表示同名应用已在册（任意生命周期态名字均占用）。
	ErrAppExists = errors.New("app name already registered")
	// ErrAppTombstoned 表示目标应用处于删除状态位（deleting/deleted）：
	// 不可复活、不可在其上继续业务写。
	ErrAppTombstoned = errors.New("app is tombstoned")
	// ErrAppNotFound 表示应用不存在。
	ErrAppNotFound = errors.New("app not found")
	// ErrAppAmbiguous 表示按裸名解析命中多行（v0.3 D-W0-4 二修：app 名
	// project 内唯一后，跨项目同名 app 合法——裸名仅解析域内唯一时可用，
	// 跨项目重名须以限定形 team/prj/app（GetAppByName 兼容）或 id 显性
	// 寻址；本哨兵兜底显性拒绝，不静默取任意行）。
	ErrAppAmbiguous = errors.New("app name is ambiguous across projects")
	// ErrInvalidLifecycleTransition 表示生命周期状态位迁移非法
	// （状态机：active → deleting → deleted，不可跳越、不可回退）。
	ErrInvalidLifecycleTransition = errors.New("invalid app lifecycle transition")
	// ErrAppSuspendedConflict 表示挂起位 CAS 落败（并发翻转已发生——重复
	// suspend/resume 幂等面由调用方以重读投影消化，本哨兵不按错误文案呈现）。
	ErrAppSuspendedConflict = errors.New("app suspend state changed concurrently")
)

// App 是应用权威态行（v0.1 最小面：期望态根 + tombstone 状态位；v0.3 W2-S3
// 增项目归属——project_id/team_id NOT NULL（00019 收紧）+ 两个不可变 slug
// （projects/teams join 反解，命名公式三段 team-prj-app 与流标签限定形的
// 参数源；slug 不可变故随行装载即缓存）。
type App struct {
	ID         string
	Name       string
	Lifecycle  AppLifecycle
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletingAt time.Time // 零值 = 未进入删除
	DeletedAt  time.Time // 零值 = 未完成删除
	// ProjectID / TeamID 是归属（00019 起 NOT NULL；MoveApp 换派即改写）。
	ProjectID string
	TeamID    string
	// TeamSlug / ProjectSlug 是归属两个 slug（命名公式段；不可变——随行
	// join 装载，调用方免二次查询）。
	TeamSlug    string
	ProjectSlug string
	// ProjectNetworkAttached 是项目网参与位（OT-1/IMPL-T15-1，00025 加法列）：
	// true = 成员服务在 app 私网之外双挂当前项目的项目网；唯一改变路径 =
	// ProjectsService attach/detach RPC。缺省 false = 不参加（既有行为零变化）。
	ProjectNetworkAttached bool
	// Suspended 是挂起位（app Stop/Start，00028 加法列）：true = 用户请求
	// 停止——引擎周期对账把受管长驻服务排水到副本 0（服务对象保留，DB
	// paused 同款「状态驱动渲染」形态）；部署入队/drift/autoscaler 按位
	// 豁免。唯一改变路径 = AppsService suspend/resume RPC。
	Suspended bool
}

// QualifiedName 返回三段限定形 `team/prj/app`（D-W0-9 引用口径；naming.
// QualifiedName 同式——state 层不 import naming（分层：naming 是底座命名
// 契约层），公式由 apps_test.go 对照钉死）。该值是 fleetly.app label 与
// 日志流标签的统一口径。
func (a App) QualifiedName() string {
	return a.TeamSlug + "/" + a.ProjectSlug + "/" + a.Name
}

// appScanCols 是应用行查询列清单（归属 slug 经 projects/teams join 反解；
// 新增列只加在此与 scanApp）。
const appScanCols = `a.id, a.name, a.lifecycle, a.created_at, a.updated_at, a.deleting_at, a.deleted_at,
	a.project_id, a.team_id, t.slug, p.slug, a.project_network_attached, a.suspended`

// appScanFrom 是应用行查询的 FROM 子句（slug join 单点）。
const appScanFrom = `FROM apps a
	JOIN projects p ON p.id = a.project_id
	JOIN teams t ON t.id = a.team_id`

// CreateApp 创建应用行（active）。归属必填（v0.3 W2-S3 归属管道：首次部署
// 写 apps.project_id/team_id——projectID/teamID 空 = 调用面违约，显性拒绝
// 而非落 NULL 行与 00019 NOT NULL 冲突）。同名（任意生命周期态）返回
// ErrAppExists；同项目同名（UNIQUE(project_id,name)）同名占用即冲突——
// 跨项目同名 app 合法（D-W0-4 二修）。appID 留空自动生成 ULID。
func (s *Store) CreateApp(ctx context.Context, appID, name, projectID, teamID string) (App, error) {
	var created App
	err := s.InTx(ctx, func(tx *Tx) error {
		app, err := tx.CreateApp(ctx, appID, name, projectID, teamID)
		if err != nil {
			return err
		}
		created = app
		return nil
	})
	if err != nil {
		return App{}, err
	}
	return created, nil
}

// CreateApp 是事务内创建应用。
func (t *Tx) CreateApp(ctx context.Context, appID, name, projectID, teamID string) (App, error) {
	if projectID == "" || teamID == "" {
		return App{}, errors.New("state: create app: project id and team id are required (v0.3 ownership pipeline)")
	}
	if appID == "" {
		appID = ulid.Make().String()
	}
	now := nowNano()
	const q = `INSERT INTO apps (id, name, lifecycle, created_at, updated_at, project_id, team_id)
		VALUES (?, ?, 'active', ?, ?, ?, ?)`
	if _, err := t.ExecContext(ctx, q, appID, name, now, now, projectID, teamID); err != nil {
		if isUniqueViolation(err) {
			return App{}, fmt.Errorf("%w: %s", ErrAppExists, name)
		}
		return App{}, fmt.Errorf("state: insert app %s: %w", name, err)
	}
	// 写后回读（slug join 反解）——返回行与读面同构（QualifiedName 等派生
	// 值对调用方立即可用）。
	return scanApp(t.QueryRowContext(ctx,
		`SELECT `+appScanCols+` `+appScanFrom+` WHERE a.id = ?`, appID))
}

// GetAppByName 按名取应用行；不存在返回 ErrAppNotFound。跨项目同名 app 多
// 行命中时返回 ErrAppAmbiguous（不静默取任意行——D-W0-4 二修后的按名解析
// 纪律：裸名仅域内唯一时可用）。三段限定形 team/prj/app 输入走精确解析
// （GetAppByQualifiedName——按裸名重解析的引擎/日志共享面凭限定形免疫跨项
// 目重名歧义；应用名词表不含 `/`〔compose 名 ^[a-z0-9][a-z0-9_-]*$〕，分支
// 与裸名查询不相交）。ID 形态归调用面解析（GetAppByID 全库唯一）。
func (s *Store) GetAppByName(ctx context.Context, name string) (App, error) {
	if teamSlug, rest, found := strings.Cut(name, "/"); found {
		if prjSlug, appName, ok := strings.Cut(rest, "/"); ok && teamSlug != "" && prjSlug != "" && appName != "" {
			return s.GetAppByQualifiedName(ctx, teamSlug, prjSlug, appName)
		}
	}
	const q = `SELECT ` + appScanCols + ` ` + appScanFrom + ` WHERE a.name = ? ORDER BY a.id LIMIT 2`
	rows, err := s.db.QueryContext(ctx, q, name)
	if err != nil {
		return App{}, fmt.Errorf("state: query app by name: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAppSingle(rows, name)
}

// GetAppByQualifiedName 按三段限定形 team/prj/app 取应用行（两段 slug 与
// 名字精确匹配；不存在返回 ErrAppNotFound）。跨项目同名 app（D-W0-4 二修
// 后合法）的无歧义读取通道——UNIQUE(project_id, name) 的 slug 投影形态。
func (s *Store) GetAppByQualifiedName(ctx context.Context, teamSlug, prjSlug, appName string) (App, error) {
	const q = `SELECT ` + appScanCols + ` ` + appScanFrom + `
		WHERE t.slug = ? AND p.slug = ? AND a.name = ?`
	return scanApp(s.db.QueryRowContext(ctx, q, teamSlug, prjSlug, appName))
}

// ListAppRowsByName 返回该裸名的全部应用行（跨项目、任意生命周期态）——
// E_APP_AMBIGUOUS 候选列与可见域解析的支撑原语（v0.3 W2-S4）。
func (s *Store) ListAppRowsByName(ctx context.Context, name string) ([]App, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+appScanCols+` `+appScanFrom+` WHERE a.name = ? ORDER BY a.id`, name)
	if err != nil {
		return nil, fmt.Errorf("state: list app rows by name: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []App
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// GetAppByNameInProject 按项目 + 名取应用行（创建面/归属一致性校验的精确
// 通道——UNIQUE(project_id,name) 语义的读取形态）。
func (s *Store) GetAppByNameInProject(ctx context.Context, projectID, name string) (App, error) {
	const q = `SELECT ` + appScanCols + ` ` + appScanFrom + ` WHERE a.project_id = ? AND a.name = ?`
	row := s.db.QueryRowContext(ctx, q, projectID, name)
	return scanApp(row)
}

// GetAppByID 按平台 ID 取应用行；不存在返回 ErrAppNotFound（入口路由
// 合成时的 app 名反查消费）。
func (s *Store) GetAppByID(ctx context.Context, id string) (App, error) {
	const q = `SELECT ` + appScanCols + ` ` + appScanFrom + ` WHERE a.id = ?`
	row := s.db.QueryRowContext(ctx, q, id)
	return scanApp(row)
}

// GetAppByID 是事务内按平台 ID 取应用行（MoveApp 等同事务写路径的前置
// 读取）。
func (t *Tx) GetAppByID(ctx context.Context, id string) (App, error) {
	const q = `SELECT ` + appScanCols + ` ` + appScanFrom + ` WHERE a.id = ?`
	return scanApp(t.QueryRowContext(ctx, q, id))
}

// scanApp 从单行构造 App（row 接口同时覆盖 *sql.Row 与 *sql.Rows）。
func scanApp(row interface{ Scan(dest ...any) error }) (App, error) {
	var a App
	var lifecycle string
	var created, updated int64
	var deleting, deleted sql.NullInt64
	var attached, suspended int
	if err := row.Scan(&a.ID, &a.Name, &lifecycle, &created, &updated, &deleting, &deleted,
		&a.ProjectID, &a.TeamID, &a.TeamSlug, &a.ProjectSlug, &attached, &suspended); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return App{}, ErrAppNotFound
		}
		return App{}, fmt.Errorf("state: scan app: %w", err)
	}
	a.ProjectNetworkAttached = attached != 0
	a.Suspended = suspended != 0
	return finishScanApp(a, lifecycle, created, updated, deleting, deleted), nil
}

// scanAppSingle 消费至多两行的名字查询：0 行 = NotFound；2 行 = Ambiguous
// （跨项目同名——按裸名解析不可裁决，D-W0-4/D-W0-9）。
func scanAppSingle(rows *sql.Rows, name string) (App, error) {
	var out App
	n := 0
	for rows.Next() {
		n++
		if n > 1 {
			return App{}, fmt.Errorf("%w: %s (qualify as team/prj/%s or reference by id)", ErrAppAmbiguous, name, name)
		}
		app, err := scanApp(rows)
		if err != nil {
			return App{}, err
		}
		out = app
	}
	if err := rows.Err(); err != nil {
		return App{}, fmt.Errorf("state: iterate apps by name: %w", err)
	}
	if n == 0 {
		return App{}, ErrAppNotFound
	}
	return out, nil
}

// finishScanApp 收尾时间戳字段的装配（scanApp/批量路径共用）。
func finishScanApp(a App, lifecycle string, created, updated int64, deleting, deleted sql.NullInt64) App {
	a.Lifecycle = AppLifecycle(lifecycle)
	a.CreatedAt = time.Unix(0, created).UTC()
	a.UpdatedAt = time.Unix(0, updated).UTC()
	if deleting.Valid {
		a.DeletingAt = time.Unix(0, deleting.Int64).UTC()
	}
	if deleted.Valid {
		a.DeletedAt = time.Unix(0, deleted.Int64).UTC()
	}
	return a
}

// MarkAppDeleting 推进 active → deleting（tombstone 第一拍）。仅允许从
// active 出发；deleting/deleted 重复进入返回 ErrInvalidLifecycleTransition。
func (s *Store) MarkAppDeleting(ctx context.Context, appID string) error {
	return s.transitionApp(ctx, appID, LifecycleActive, LifecycleDeleting, "deleting_at")
}

// MarkAppDeleting 是事务内 tombstone 第一拍（供与审计/事件同事务组合——
// T2.17 API 删除走 fail-closed 审计）。
func (t *Tx) MarkAppDeleting(ctx context.Context, appID string) error {
	res, err := t.ExecContext(ctx,
		`UPDATE apps SET lifecycle = ?, updated_at = ?, deleting_at = ?
		WHERE id = ? AND lifecycle = ?`,
		string(LifecycleDeleting), nowNano(), nowNano(), appID, string(LifecycleActive))
	if err != nil {
		return fmt.Errorf("state: update app lifecycle: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read lifecycle update count: %w", err)
	}
	if n == 0 {
		return ErrInvalidLifecycleTransition
	}
	return nil
}

// MarkAppDeleted 推进 deleting → deleted（tombstone 第二拍）。仅允许从
// deleting 出发——active 直达 deleted 被拒绝（状态机纪律，防跳过清理）。
func (s *Store) MarkAppDeleted(ctx context.Context, appID string) error {
	return s.transitionApp(ctx, appID, LifecycleDeleting, LifecycleDeleted, "deleted_at")
}

// MarkAppDeleted 是事务内 tombstone 第二拍（H10/MG-3：与终局事件/审计
// 同事务组合的形态——引擎 deleting 回收步 在受管服务全部移除后原子
// 落终态，进程在「迁移已落、事件未发」之间崩溃的披露缺口不存在）。
// IMPL-T15-1：同步清项目网参与位（删后不参加任何项目网——成员计数与项目网
// GC 不因墓碑行悬挂；无独立事件，终局 app.deleted 已承载删除语义）。
func (t *Tx) MarkAppDeleted(ctx context.Context, appID string) error {
	res, err := t.ExecContext(ctx,
		`UPDATE apps SET lifecycle = ?, updated_at = ?, deleted_at = ?, project_network_attached = 0
		WHERE id = ? AND lifecycle = ?`,
		string(LifecycleDeleted), nowNano(), nowNano(), appID, string(LifecycleDeleting))
	if err != nil {
		return fmt.Errorf("state: update app lifecycle: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: read lifecycle update count: %w", err)
	}
	if n == 0 {
		return ErrInvalidLifecycleTransition
	}
	return nil
}

// SetAppSuspended 翻转应用挂起位（app Stop/Start，00028 加法列；语义见
// App.Suspended 与 DB paused 的「状态驱动渲染」对照——本写点只动权威位，
// 副本排水由引擎周期对账按位执行）。CAS + tombstone 守卫 + 事件同事务
// （Outbox，形态同 EnterDbPhase——事件由调用方构造传入，位写与披露原子：
// CAS 落败事务回滚、不产生事件）。行不存在 = ErrAppNotFound；
// deleting/deleted = ErrAppTombstoned（墓碑不接受业务写）；现值 ≠ 期望 =
// ErrAppSuspendedConflict（并发翻转已发生，调用方重读投影即可）。
func (s *Store) SetAppSuspended(ctx context.Context, appID string, suspended bool, events ...Event) (App, error) {
	var out App
	err := s.InTx(ctx, func(tx *Tx) error {
		app, err := tx.SetAppSuspended(ctx, appID, suspended, events...)
		if err != nil {
			return err
		}
		out = app
		return nil
	})
	if err != nil {
		return App{}, err
	}
	return out, nil
}

// SetAppSuspended 是事务内挂起位翻转写点（语义见 Store.SetAppSuspended）。
func (t *Tx) SetAppSuspended(ctx context.Context, appID string, suspended bool, events ...Event) (App, error) {
	want := 0
	if suspended {
		want = 1
	}
	res, err := t.ExecContext(ctx,
		`UPDATE apps SET suspended = ?, updated_at = ?
		WHERE id = ? AND lifecycle = ? AND suspended != ?`,
		want, nowNano(), appID, string(LifecycleActive), want)
	if err != nil {
		return App{}, fmt.Errorf("state: update app suspended: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return App{}, fmt.Errorf("state: read suspended update count: %w", err)
	}
	if n == 0 {
		// 归类零命中：不存在 / 墓碑 / 并发翻转——三态显式区分（调用面映射
		// 409 族；不静默幂等成功，前端投影以重读为准）。
		app, rowErr := t.GetAppByID(ctx, appID)
		if errors.Is(rowErr, ErrAppNotFound) {
			return App{}, ErrAppNotFound
		}
		if rowErr != nil {
			return App{}, rowErr
		}
		if app.Lifecycle != LifecycleActive {
			return App{}, fmt.Errorf("%w: app %s is %s", ErrAppTombstoned, appID, app.Lifecycle)
		}
		return App{}, fmt.Errorf("%w: app %s suspended already %t", ErrAppSuspendedConflict, appID, app.Suspended)
	}
	for _, ev := range events {
		if _, err := t.AppendEvent(ctx, ev); err != nil {
			return App{}, err
		}
	}
	// 写后回读（slug join 反解）——返回行与读面同构。
	return scanApp(t.QueryRowContext(ctx,
		`SELECT `+appScanCols+` `+appScanFrom+` WHERE a.id = ?`, appID))
}

// MoveApp 资源改派（rbac-teams §3.4/§5，W2-S3）：写归属（project_id +
// team_id 冗余列同步自目标项目行）+ 审计 app.moved 同事务 fail-closed。
// 换名重部署（底座对象的 team/prj 段随新归属推导）由引擎编排层执行——
// 本原语只动权威态；同名占用守卫：目标项目已有同名 app（UNIQUE
// (project_id,name)，D-W0-4 二修）→ ErrAppExists，跨团队改派先过目标
// 项目唯一性（409，调用面映射）。占用与 CreateApp 同语义：**任意生命周期
// 性行均占名**（active/deleting/deleted——UNIQUE(project_id,name) 无生命
// 周期豁免，探针只负责把违反提前映射成 ErrAppExists 而非裸约束错误）。
func (s *Store) MoveApp(ctx context.Context, appID, toProjectID, actorUserID, actorTokenID string) (App, error) {
	if toProjectID == "" {
		return App{}, errors.New("state: move app: target project id is empty")
	}
	var out App
	err := s.InTx(ctx, func(tx *Tx) error {
		app, err := tx.GetAppByID(ctx, appID)
		if err != nil {
			return err
		}
		proj, err := tx.GetProject(ctx, toProjectID)
		if err != nil {
			return err
		}
		if app.ProjectID == toProjectID {
			return fmt.Errorf("%w: app %s is already in project %s", ErrMoveSameTarget, app.Name, toProjectID)
		}
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM apps WHERE project_id = ? AND name = ? AND id != ?`,
			toProjectID, app.Name, appID).Scan(&taken); err == nil {
			return fmt.Errorf("%w: %s already exists in the target project", ErrAppExists, app.Name)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("state: probe target project name: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE apps SET project_id = ?, team_id = ?, updated_at = ? WHERE id = ?`,
			proj.ID, proj.TeamID, nowNano(), appID); err != nil {
			return fmt.Errorf("state: move app: %w", err)
		}
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        auditActor(actorUserID),
			ActorTokenID: actorTokenID,
			Action:       "app.moved",
			Target:       "app:" + app.ID,
			Result:       "ok",
			DiffSummary:  DiffSummary("app", app.Name, "from_project", app.ProjectID, "to_project", proj.ID, "to_team", proj.TeamID),
		}); err != nil {
			return err
		}
		moved, err := scanApp(tx.QueryRowContext(ctx,
			`SELECT `+appScanCols+` `+appScanFrom+` WHERE a.id = ?`, appID))
		if err != nil {
			return err
		}
		out = moved
		return nil
	})
	if err != nil {
		return App{}, err
	}
	return out, nil
}

// ErrMoveSameTarget 表示改派目标与当前归属相同（无操作语义，409 退化）。
var ErrMoveSameTarget = errors.New("resource already belongs to the target project")

// transitionApp 执行 from → to 的生命周期迁移并盖对应时间位列。
func (s *Store) transitionApp(ctx context.Context, appID string, from, to AppLifecycle, stampCol string) error {
	err := s.InTx(ctx, func(tx *Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE apps SET lifecycle = ?, updated_at = ?, `+stampCol+` = ?
			WHERE id = ? AND lifecycle = ?`,
			string(to), nowNano(), nowNano(), appID, string(from))
		if err != nil {
			return fmt.Errorf("state: update app lifecycle: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("state: read lifecycle update count: %w", err)
		}
		if n == 0 {
			return ErrInvalidLifecycleTransition
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: transition app %s %s→%s: %w", appID, from, to, err)
	}
	return nil
}

// isUniqueViolation 报告 err 是否为 SQLite 唯一约束冲突（modernc 驱动
// 暂无公开错误码类型，按错误串归类；只用于 ErrAppExists 归因，不作为
// 安全判定）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
