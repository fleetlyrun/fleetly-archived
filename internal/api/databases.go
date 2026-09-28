package api

// DatabaseService 实现 server.v1.DatabaseService（E4 数据库托管，managed-
// databases §2.3 操作表 + §5.1 契约的 S2 子集）。库实例是独立一等资源
// （D-DB-1）：受理 = db_instances 行（provisioning）+ 凭据生成一次 + 事件
// 与审计同事务 fail-closed；收敛由 internal/database duty 异步承载——API
// 只做受理/守卫/投影，绝不触底座。
//
// 错误映射（D-DB-8 零新增码）：
//
//	state.ErrDatabaseNotFound           → E_DB_NOT_FOUND 404
//	state.ErrDatabaseStateConflict      → E_STATE_VERSION_CONFLICT 409
//	                                      （context 带 current_state + 合法
//	                                      前置态清单——同族乐观冲突语义）
//	state.ErrDatabaseIllegalTransition  → 同上 409 族
//	state.ErrDatabaseTerminal           → E_DB_NOT_FOUND 404（deleting/
//	                                      deleted 的操作目标按设计语义不可见）
//	dbtemplate.ErrUnknownTemplate       → E_DB_TEMPLATE_UNSUPPORTED 400
//	state.ErrDatabaseExists             → 409 退化信封（名字占用，无注册码）
//
// 明文纪律：凭据明文只存活于「生成 → 加密落库」与「解密 → 指纹/掩码投影」
// 的内存链；响应只带掩码 URL 与 hash8 指纹（reveal 面随 S4/S6）。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/database"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// connectionPasswordMask 是连接投影的密码掩码（固定形态——明文零离开存储；
// 显式 reveal 属 S4/S6 面）。
const connectionPasswordMask = "********"

// ProvisionKicker 是创建/恢复/重试受理后的收敛触发端口（实现方 = internal/
// database.Manager——接口在本包定义，方向纪律：api 定义端口、不感知实现
// 类型）。nil 容忍：未装配时受理照常成功，收敛等 duty 下一拍（≤10s）。
type ProvisionKicker interface {
	Kick()
}

// CredentialRotator 是凭据轮换编排的消费端口（S4；实现方 = internal/
// database.Manager——引擎侧动作与底座紧邻，api 只受理/守卫/审计）。
// 返回重部署的引用 app 名单；编排错误由 database 包定义类型化哨兵
//（Prestate/Paused/Conflict/Stage），本层统一映射注册表码。
type CredentialRotator interface {
	RotateCredentials(ctx context.Context, name string) ([]string, error)
}

// BackupOrchestrator 是备份/恢复/升级编排的消费端口（S5；实现方 =
// internal/database.Manager——job 编排与底座紧邻，api 只受理/守卫/审计）。
// 三个受理面全部异步（job 分钟级——受理即 accepted，结论经台账与 db.*
// 事件披露）；前置态/互斥/归属守卫在编排层同步裁决后以类型化哨兵回传，
// 本层统一映射注册表码。
type BackupOrchestrator interface {
	// TriggerBackup 异步受理一次手动备份（kind=manual）。
	TriggerBackup(ctx context.Context, name string) error
	// RestoreBackup 异步受理一次原地恢复（快照归属守卫在编排层）。
	RestoreBackup(ctx context.Context, name, snapshotID string) error
	// Upgrade 异步受理一次受控升级；返回 (旧 digest, 新 digest) 供响应与
	// 审计。
	Upgrade(ctx context.Context, name string) (string, string, error)
}

// DatabaseService 实现 server.v1.DatabaseService。
type DatabaseService struct {
	serverv1.UnimplementedDatabaseServiceServer
	st      *state.Store
	box     *secrets.Box
	kick    ProvisionKicker
	rotator CredentialRotator
	ops     BackupOrchestrator
}

// NewDatabaseService 构造 DatabaseService（box 是凭据生成/指纹的加解密器；
// kick 可 nil——受理后即时收敛拍，缺省时收敛由 duty 周期拍兜底；rotator
// 可 nil——轮换 RPC 未装配时显式报错，不静默退化；ops 同理——备份/恢复/
// 升级 RPC 未装配时显式报错）。
func NewDatabaseService(st *state.Store, box *secrets.Box, kick ProvisionKicker, rotator CredentialRotator, ops BackupOrchestrator) *DatabaseService {
	return &DatabaseService{st: st, box: box, kick: kick, rotator: rotator, ops: ops}
}

// kickOnce 受理成功后的即时收敛请求（失败静默——kick 只是提前，不承载正
// 确性：错过的 kick 由 duty 周期拍消化）。
func (s *DatabaseService) kickOnce() {
	if s.kick != nil {
		s.kick.Kick()
	}
}

// CreateDatabase 创建库实例：模板校验（未知 → 400）→ 名校验 → 归属解析与
// 一致性（v0.3 W2-S3：project 引用解析，行上归属已定必须一致——409 指引
// MoveDatabase）→ 凭据生成 + 加密 → 实例行（project_id/team_id NOT NULL）
// + db.provision_started 事件 + 审计 db.create 同事务 fail-closed。
func (s *DatabaseService) CreateDatabase(ctx context.Context, req *serverv1.CreateDatabaseRequest) (*serverv1.CreateDatabaseResponse, error) {
	tpl, err := dbtemplate.Get(req.GetTemplate())
	if err != nil {
		return nil, apperr.New("E_DB_TEMPLATE_UNSUPPORTED",
			"template %q is not in the platform registry (available: %s)", req.GetTemplate(), templateIDList()).
			WithContext("template", req.GetTemplate()).
			WithContext("available", templateIDList())
	}
	if err := state.ValidateDatabaseName(req.GetName()); err != nil {
		return nil, statusInvalidArgument(err.Error())
	}
	// 归属解析（D-W0-9：裸名域内唯一/限定形恒可解析；机具令牌必须显式）。
	proj, err := resolveProjectRef(ctx, s.st, req.GetProject())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：库生命周期 = admin 层级（scope 登记映射）；
	// 先门后建行。平台管理员不代写在此收口。
	if err := requireResourceAccess(ctx, s.st, proj.ID); err != nil {
		return nil, err
	}
	// 归属一致性：行存在于其他项目 → 409（校验一致裁决，MoveDatabase 指引
	// ——与 Deploy 同口径）；目标项目内已有同名 → 名字占用 409。
	existing, err := s.st.GetDatabaseInstanceByName(ctx, req.GetName())
	switch {
	case err == nil && existing.ProjectID != proj.ID:
		return nil, apperr.New("E_APP_PROJECT_MISMATCH",
			"database %q already belongs to project %q (ownership on the row wins once assigned); create with that project or move the instance first",
			req.GetName(), existing.QualifiedName()).
			WithContext("database", req.GetName()).
			WithContext("current_project", existing.ProjectID)
	case err == nil:
		return nil, conflict(fmt.Sprintf("database instance name %q is already registered in project %q (names stay reserved across the lifecycle)", req.GetName(), proj.Slug))
	case errors.Is(err, state.ErrDatabaseAmbiguous):
		return nil, ambiguousRefErr("database", "team/prj/db", req.GetName(), nil)
	case !errors.Is(err, state.ErrDatabaseNotFound):
		return nil, err
	}
	password, err := dbtemplate.GeneratePassword()
	if err != nil {
		return nil, fmt.Errorf("generate database credential: %w", err)
	}
	cipher, err := s.box.Encrypt([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("encrypt database credential: %w", err)
	}
	var created state.DatabaseInstance
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		row, err := tx.CreateDatabaseInstance(ctx, state.DatabaseInstance{
			Name:             req.GetName(),
			Template:         req.GetTemplate(),
			ImageDigest:      tpl.Image,
			Settings:         databaseSettingsOf(req.GetLimits(), req.GetBackupPlan()),
			CredentialCipher: string(cipher),
			ProjectID:        proj.ID,
			TeamID:           proj.TeamID,
		})
		if err != nil {
			return err
		}
		created = row
		if _, err := tx.AppendEvent(ctx, databaseLifecycleEvent("db.provision_started", row)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.create", "database:"+row.ID,
			state.DiffSummary("template", row.Template)))
	})
	if err != nil {
		// ErrDatabaseExists 走登记行（名字占用 409）；其余原样透传。
		return nil, mapStoreErr(err, req.GetName())
	}
	s.kickOnce()
	view, err := s.databaseView(ctx, created)
	if err != nil {
		return nil, err
	}
	return &serverv1.CreateDatabaseResponse{Database: view}, nil
}

// GetDatabase 库实例详情（脱敏投影；可见域解析 + 角色门，W2-S4）。
func (s *DatabaseService) GetDatabase(ctx context.Context, req *serverv1.GetDatabaseRequest) (*serverv1.GetDatabaseResponse, error) {
	inst, err := resolveDatabaseRef(ctx, s.st, req.GetName())
	if err != nil {
		return nil, err
	}
	if err := requireDatabaseAccess(ctx, s.st, inst); err != nil {
		return nil, err
	}
	view, err := s.databaseView(ctx, inst)
	if err != nil {
		return nil, err
	}
	return &serverv1.GetDatabaseResponse{Database: view}, nil
}

// ListDatabases 库实例列表（name 字典序；deleted tombstone 不进默认列表
// ——与 apps 列表同口径）。可见性过滤 + ?project= 收窄（W2-S4，与 ListApps
// 同款 visibleProjectFilter 单点）。
func (s *DatabaseService) ListDatabases(ctx context.Context, req *serverv1.ListDatabasesRequest) (*serverv1.ListDatabasesResponse, error) {
	rows, err := s.st.ListDatabaseInstances(ctx)
	if err != nil {
		return nil, err
	}
	allowed, err := visibleProjectFilter(ctx, s.st, req.GetProject())
	if err != nil {
		return nil, err
	}
	if allowed != nil {
		narrowed := rows[:0:0]
		for _, inst := range rows {
			if allowed[inst.ProjectID] {
				narrowed = append(narrowed, inst)
			}
		}
		rows = narrowed
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 100
	}
	out := make([]*serverv1.DatabaseView, 0, len(rows))
	for _, inst := range rows {
		if inst.State == state.DatabaseDeleted {
			continue
		}
		if len(out) >= limit {
			break
		}
		view, err := s.databaseView(ctx, inst)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return &serverv1.ListDatabasesResponse{Databases: out}, nil
}

// DeleteDatabase 删除受理：终端态守卫 → confirm 两段式 → 引用守卫
// （E_DB_REFERENCED 409 附引用清单）→ 卷处置选择落位 + deleting 转移 +
// 审计 db.delete 同事务；reap 由收敛 duty 幂等完成。
func (s *DatabaseService) DeleteDatabase(ctx context.Context, req *serverv1.DeleteDatabaseRequest) (*serverv1.DeleteDatabaseResponse, error) {
	inst, err := resolveDatabaseRef(ctx, s.st, req.GetName())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：删除 = admin 层级（getMutableInstance 未覆盖
	// 终态行——删除目标可能已 deleting，门先行）。
	if err := requireDatabaseAccess(ctx, s.st, inst); err != nil {
		return nil, err
	}
	if inst.State == state.DatabaseDeleting || inst.State == state.DatabaseDeleted {
		return nil, databaseNotFound(inst.Name, "already "+string(inst.State))
	}
	if req.GetConfirm() != inst.Name {
		return nil, statusInvalidArgument(
			"destructive operation: pass confirm=\"" + inst.Name + "\" to accept deletion (data volumes are kept by default; delete_volumes=true discards them irreversibly)")
	}
	// 删除前置态前哨（§2.3 操作表：ready/degraded/paused/failed——
	// provisioning 是收敛在途，等健康门收口或失败后再删；表外组合的显式
	// 409，context 带合法前置态清单）。
	switch inst.State {
	case state.DatabaseReady, state.DatabaseDegraded, state.DatabasePaused, state.DatabaseFailed:
	default:
		return nil, apperr.New("E_STATE_VERSION_CONFLICT",
			"database %q is in state %q; delete requires one of: ready, degraded, paused, failed (wait for the health gate or retry after failure)",
			inst.Name, inst.State).
			WithContext("current_state", string(inst.State)).
			WithContext("legal_prestates", "ready, degraded, paused, failed")
	}
	refs, err := s.st.ListDatabaseReferencesByDB(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		return nil, s.referencedErr(ctx, inst, refs)
	}
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.SetDatabaseDeleteVolumes(ctx, inst.ID, req.GetDeleteVolumes()); err != nil {
			return err
		}
		if err := tx.EnterDbPhase(ctx, inst.ID, inst.State, state.DatabaseDeleting,
			databaseLifecycleEvent("db.delete_started", inst)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.delete", "database:"+inst.ID,
			state.DiffSummary("from", string(inst.State), "delete_volumes", req.GetDeleteVolumes())))
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	s.kickOnce()
	return &serverv1.DeleteDatabaseResponse{Name: inst.Name, Status: string(state.DatabaseDeleting)}, nil
}

// SuspendDatabase 暂停（ready/degraded → paused）：scale-0 由收敛 duty 落
// 地；引用方连不上是诚实暴露（设计 §2.3）。
func (s *DatabaseService) SuspendDatabase(ctx context.Context, req *serverv1.SuspendDatabaseRequest) (*serverv1.SuspendDatabaseResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.EnterDbPhase(ctx, inst.ID, inst.State, state.DatabasePaused,
			databaseLifecycleEvent("db.suspended", inst)); err == nil {
			return tx.WriteAudit(ctx, databaseAudit(ctx, "db.suspend", "database:"+inst.ID,
				state.DiffSummary("from", string(inst.State))))
		} else if !errors.Is(err, state.ErrDatabaseStateConflict) || inst.State != state.DatabaseReady {
			return err
		}
		// ready 期 CAS 落败 = 并发已到 degraded（观察 duty 抢先）：按
		// degraded 前置态重试（操作表第二合法前置态）。
		return tx.EnterDbPhase(ctx, inst.ID, state.DatabaseDegraded, state.DatabasePaused,
			databaseLifecycleEvent("db.suspended", inst))
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	s.kickOnce()
	view, err := s.viewAfter(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return &serverv1.SuspendDatabaseResponse{Database: view}, nil
}

// ResumeDatabase 恢复（paused → provisioning 重收敛）。
func (s *DatabaseService) ResumeDatabase(ctx context.Context, req *serverv1.ResumeDatabaseRequest) (*serverv1.ResumeDatabaseResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.EnterDbPhase(ctx, inst.ID, state.DatabasePaused, state.DatabaseProvisioning,
			databaseLifecycleEvent("db.resumed", inst)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.resume", "database:"+inst.ID,
			state.DiffSummary("from", string(inst.State))))
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	s.kickOnce()
	view, err := s.viewAfter(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return &serverv1.ResumeDatabaseResponse{Database: view}, nil
}

// RetryDatabase 显式重试（failed → provisioning；失败现场保留语义下的唯一
// 出边；收敛过健康门时 last_error 清空）。
func (s *DatabaseService) RetryDatabase(ctx context.Context, req *serverv1.RetryDatabaseRequest) (*serverv1.RetryDatabaseResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.EnterDbPhase(ctx, inst.ID, state.DatabaseFailed, state.DatabaseProvisioning,
			databaseLifecycleEvent("db.provision_started", inst)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.retry", "database:"+inst.ID,
			state.DiffSummary("from", string(inst.State))))
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	s.kickOnce()
	view, err := s.viewAfter(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return &serverv1.RetryDatabaseResponse{Database: view}, nil
}

// UpdateDatabaseSettings 设置变更（限额 + 备份计划；任意非终态准入、主状态
// 不变；限额变更的 spec 重建由收敛器在下一拍以 desired-hash 判据承载——
// 文档口径，非本 RPC 内动作）。
func (s *DatabaseService) UpdateDatabaseSettings(ctx context.Context, req *serverv1.UpdateDatabaseSettingsRequest) (*serverv1.UpdateDatabaseSettingsResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	settings := databaseSettingsOf(req.GetLimits(), req.GetBackupPlan())
	if err := s.st.UpdateDatabaseSettings(ctx, inst.ID, settings); err != nil {
		return nil, mapStoreErr(err)
	}
	// 设置变更不在设计审计词表（§5.3）——走方法级 action（api.DatabaseService.
	// UpdateDatabaseSettings，拦截器注入的同款通用词根）。
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, auditEntry(ctx, "database:"+inst.ID,
			state.DiffSummary("cpu_seconds", settings.CPUSeconds, "memory_bytes", settings.MemoryBytes)))
	}); err != nil {
		return nil, err
	}
	view, err := s.viewAfter(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return &serverv1.UpdateDatabaseSettingsResponse{Database: view}, nil
}

// RotateDatabaseCredentials 凭据轮换受理（破坏性两段式 confirm；编排 =
// CredentialRotator 端口，引擎侧动作在 internal/database）：
//   - 编排错误映射：前置态违规 / PG 暂停拒绝 / 并发 CAS 落败 →
//     E_STATE_VERSION_CONFLICT 族 409（同族乐观冲突语义，D-DB-8）；
//     中途失败 → E_DB_ROTATE_FAILED 500（context 带已完成阶段——人工收尾）。
//   - 成功：db.rotate 审计（diff 只带 app 名单与指纹级事实，凭据材料零
//     出现）+ db.credentials_rotated 事件（database 包内落）。
func (s *DatabaseService) RotateDatabaseCredentials(ctx context.Context, req *serverv1.RotateDatabaseCredentialsRequest) (*serverv1.RotateDatabaseCredentialsResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetConfirm() != inst.Name {
		return nil, statusInvalidArgument(
			"destructive operation: pass confirm=\"" + inst.Name + "\" to accept credential rotation (referencing apps are auto-redeployed; the rotation cannot be undone)")
	}
	if s.rotator == nil {
		return nil, fmt.Errorf("database rotation orchestrator is not wired (assembly bug)")
	}
	redeployed, err := s.rotator.RotateCredentials(ctx, inst.Name)
	if err != nil {
		return nil, s.mapRotationErr(err)
	}
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.rotate", "database:"+inst.ID,
			state.DiffSummary("template", inst.Template, "redeployed_apps", strings.Join(redeployed, ","))))
	}); err != nil {
		return nil, err
	}
	s.kickOnce()
	view, err := s.viewAfter(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return &serverv1.RotateDatabaseCredentialsResponse{Database: view, RedeployedApps: redeployed}, nil
}

// mapRotationErr 是轮换编排错误 → 注册表码的映射面（database 包类型化
// 哨兵 → api 权威语义；message 全程无凭据材料——编排层已保证）。哨兵类型
// 随编排实现在 internal/database（端口在本包、错误类型属实现包——编排是
// 底座邻接动作，信封语义归 api 权威，类型归实现包所有）。
func (s *DatabaseService) mapRotationErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, state.ErrDatabaseNotFound):
		return mapStoreErr(err)
	case errors.Is(err, database.ErrPGRotationPaused):
		return apperr.New("E_STATE_VERSION_CONFLICT",
			"%s (resume the database first, then rotate)", err.Error()).
			WithContext("current_state", "paused").
			WithContext("legal_prestates", "ready, degraded")
	case errors.Is(err, database.ErrRotationConflict):
		return apperr.New("E_STATE_VERSION_CONFLICT",
			"database credentials were rotated concurrently — re-read the current state and retry if needed").
			WithContext("conflict", "credential_updated_at changed")
	}
	var prestate *database.RotationPrestateError
	if errors.As(err, &prestate) {
		return apperr.New("E_STATE_VERSION_CONFLICT", "%s", err.Error()).
			WithContext("current_state", prestate.Current).
			WithContext("legal_prestates", "ready, degraded, paused")
	}
	var stage *database.RotationStageError
	if errors.As(err, &stage) {
		return apperr.New("E_DB_ROTATE_FAILED", "%s", err.Error()).
			WithContext("stage", stage.Stage)
	}
	return err
}

// RevealDatabaseCredentials 连接信息显式展开（§2.5「密码默认脱敏、显式
// 展开」的 API 面；admin scope 在拦截器链强制）。设计审计词表未列 reveal
// ——按「敏感访问必留痕」补 db.reveal 审计（访问事实 + 指纹，值零出现）；
// 不产生事件（操作非状态转移）。
func (s *DatabaseService) RevealDatabaseCredentials(ctx context.Context, req *serverv1.RevealDatabaseCredentialsRequest) (*serverv1.RevealDatabaseCredentialsResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if inst.CredentialCipher == "" {
		return nil, databaseNotFound(inst.Name, "no credential stored")
	}
	plain, err := s.box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		return nil, fmt.Errorf("decrypt credential of database %s: %w", inst.Name, err)
	}
	vars, err := dbtemplate.ConnectionVars(inst.Template, inst.Name, string(plain))
	if err != nil {
		return nil, err
	}
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		return nil, err
	}
	prefix := dbtemplate.EnvPrefix(inst.Name)
	out := &serverv1.RevealDatabaseCredentialsResponse{
		Name:     inst.Name,
		Template: inst.Template,
		Host:     vars[prefix+"_HOST"],
		Port:     int32(tpl.EnginePort),
		Password: string(plain),
		Url:      vars[prefix+"_URL"],
	}
	if user, ok := vars[prefix+"_USER"]; ok {
		out.User = user
	}
	if pgDatabaseName, ok := vars[prefix+"_DATABASE"]; ok {
		out.Database = pgDatabaseName
	}
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.reveal", "database:"+inst.ID,
			state.DiffSummary("template", inst.Template, "fingerprint", naming.Hash8(string(plain)))))
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// TriggerDatabaseBackup 手动备份受理（E4 S5，§2.6）：异步受理——编排层
// 同步裁决前置态/互斥/S3 配置（类型化哨兵 → 注册表码映射），job 链异步执
// 行，结论经台账与本事件的披露面回读。审计 db.backup_trigger（kind 只为
// manual——daily/pre_upgrade 是平台内部类别，不从 API 受理）。
func (s *DatabaseService) TriggerDatabaseBackup(ctx context.Context, req *serverv1.TriggerDatabaseBackupRequest) (*serverv1.TriggerDatabaseBackupResponse, error) {
	switch k := req.GetKind(); k {
	case "", "manual":
	default:
		return nil, statusInvalidArgument(fmt.Sprintf(
			"backup kind %q is platform-internal; the API accepts manual backups only (omit kind or pass \"manual\")", k))
	}
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if s.ops == nil {
		return nil, fmt.Errorf("database backup orchestrator is not wired (assembly bug)")
	}
	if err := s.ops.TriggerBackup(ctx, inst.Name); err != nil {
		return nil, s.mapOperationErr(err)
	}
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.backup_trigger", "database:"+inst.ID,
			state.DiffSummary("kind", "manual")))
	}); err != nil {
		return nil, err
	}
	return &serverv1.TriggerDatabaseBackupResponse{
		Name:   inst.Name,
		Kind:   "manual",
		Status: "accepted",
	}, nil
}

// ListDatabaseBackups 备份台账列表（created_at 降序——恢复目标选择与备份
// 健康面的只读数据源）。
func (s *DatabaseService) ListDatabaseBackups(ctx context.Context, req *serverv1.ListDatabaseBackupsRequest) (*serverv1.ListDatabaseBackupsResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.st.ListDatabaseBackups(ctx, inst.ID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.DatabaseBackupView, 0, len(rows))
	for _, r := range rows {
		v := &serverv1.DatabaseBackupView{
			Id:           r.ID,
			Kind:         string(r.Kind),
			Snapshot:     r.ResticSnapshot,
			SizeBytes:    r.SizeBytes,
			VerifyStatus: string(r.VerifyStatus),
			Error:        r.Error,
			CreatedAt:    timestamppb.New(r.CreatedAt),
		}
		out = append(out, v)
	}
	return &serverv1.ListDatabaseBackupsResponse{Backups: out}, nil
}

// RestoreDatabaseBackup 原地恢复受理（破坏性两段式 confirm + 快照归属守
// 卫在编排层）：异步受理——停库重放分钟级，结论经 db.restore_* 事件披露。
// 审计 db.restore（快照标识进 diff——恢复是数据安全动作，目标必须留痕）。
func (s *DatabaseService) RestoreDatabaseBackup(ctx context.Context, req *serverv1.RestoreDatabaseBackupRequest) (*serverv1.RestoreDatabaseBackupResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetConfirm() != inst.Name {
		return nil, statusInvalidArgument(
			"destructive operation: pass confirm=\"" + inst.Name + "\" to accept an in-place restore (the current data on the volume is overwritten by the replayed snapshot)")
	}
	if s.ops == nil {
		return nil, fmt.Errorf("database restore orchestrator is not wired (assembly bug)")
	}
	if err := s.ops.RestoreBackup(ctx, inst.Name, req.GetSnapshot()); err != nil {
		return nil, s.mapOperationErr(err)
	}
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.restore", "database:"+inst.ID,
			state.DiffSummary("snapshot", req.GetSnapshot(), "from_state", string(inst.State))))
	}); err != nil {
		return nil, err
	}
	return &serverv1.RestoreDatabaseBackupResponse{
		Name:     inst.Name,
		Snapshot: req.GetSnapshot(),
		Status:   "accepted",
	}, nil
}

// UpgradeDatabase 受控升级受理（破坏性两段式 confirm；编排 = BackupOrchestrator
// 的升级面）：备份门/受控重建/健康门/归位在 internal/database 异步编排，
// 结论经 db.upgrade_* 事件披露。审计 db.upgrade（新旧 digest 进 diff——升
// 级是受管面的显式 opt-in 动作）。
func (s *DatabaseService) UpgradeDatabase(ctx context.Context, req *serverv1.UpgradeDatabaseRequest) (*serverv1.UpgradeDatabaseResponse, error) {
	inst, err := s.getMutableInstance(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetConfirm() != inst.Name {
		return nil, statusInvalidArgument(
			"destructive operation: pass confirm=\"" + inst.Name + "\" to accept a controlled upgrade (stop-first rebuild with a downtime window; failure rolls the digest back and lands the instance degraded)")
	}
	if s.ops == nil {
		return nil, fmt.Errorf("database upgrade orchestrator is not wired (assembly bug)")
	}
	oldDigest, newDigest, err := s.ops.Upgrade(ctx, inst.Name)
	if err != nil {
		return nil, s.mapOperationErr(err)
	}
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, databaseAudit(ctx, "db.upgrade", "database:"+inst.ID,
			state.DiffSummary("old_digest", oldDigest, "new_digest", newDigest)))
	}); err != nil {
		return nil, err
	}
	view, err := s.viewAfter(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return &serverv1.UpgradeDatabaseResponse{Database: view, Status: "accepted"}, nil
}

// mapOperationErr 是备份/恢复/升级编排错误 → 注册表码的映射面（database
// 包类型化哨兵 → api 权威语义；message 全程无凭据材料——编排层已保证）。
func (s *DatabaseService) mapOperationErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, state.ErrDatabaseNotFound):
		return mapStoreErr(err)
	case errors.Is(err, database.ErrBackupS3NotConfigured):
		return apperr.New("E_S3_NOT_CONFIGURED", "%s (configure object storage with 'fleetly s3 set' or the Console S3 settings card, then retry)", err.Error()).
			WithContext("reason", "s3.mode=unset")
	}
	var prestate *database.OperationPrestateError
	if errors.As(err, &prestate) {
		return apperr.New("E_STATE_VERSION_CONFLICT", "%s", err.Error()).
			WithContext("current_state", prestate.Current).
			WithContext("legal_prestates", prestate.Legal)
	}
	var inflight database.ErrOperationInFlight
	if errors.As(err, &inflight) {
		return apperr.New("E_STATE_VERSION_CONFLICT", "%s", err.Error()).
			WithContext("conflict", "operation in flight").
			WithContext("current_operation", inflight.Current)
	}
	var notAvailable database.ErrUpgradeNotAvailable
	if errors.As(err, &notAvailable) {
		return apperr.New("E_STATE_VERSION_CONFLICT", "%s", err.Error()).
			WithContext("current_state", "up_to_date").
			WithContext("instance_digest", notAvailable.Current).
			WithContext("template_digest", notAvailable.Template)
	}
	return err
}

// ── 内部协作者 ──────────────────────────────────────────────────────────────

// getMutableInstance 取非终态实例（deleting/deleted 的操作目标 → 404——
// E_DB_NOT_FOUND 的「已进入 deleting/deleted」语义行）。可见域解析 + 角色
// 门（W2-S4）：全部库写面/敏感面经此单点进角色门。
func (s *DatabaseService) getMutableInstance(ctx context.Context, name string) (state.DatabaseInstance, error) {
	inst, err := resolveDatabaseRef(ctx, s.st, name)
	if err != nil {
		return state.DatabaseInstance{}, err
	}
	if err := requireDatabaseAccess(ctx, s.st, inst); err != nil {
		return state.DatabaseInstance{}, err
	}
	if inst.State == state.DatabaseDeleting || inst.State == state.DatabaseDeleted {
		return state.DatabaseInstance{}, databaseNotFound(inst.Name, "already "+string(inst.State))
	}
	return inst, nil
}

// viewAfter 转移后重读并投影（Suspend/Resume/Retry/Settings 的响应面）。
func (s *DatabaseService) viewAfter(ctx context.Context, id string) (*serverv1.DatabaseView, error) {
	inst, err := s.st.GetDatabaseInstance(ctx, id)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return s.databaseView(ctx, inst)
}

// referencedErr 构造引用守卫冲突（E_DB_REFERENCED 409；context 列出引用
// app/服务清单——数据安全前哨的可行动面，设计 §2.4。清单以 app 名呈现——
// 引用行存 app_id，名字解析失败的孤儿行如实回退 ID）。
func (s *DatabaseService) referencedErr(ctx context.Context, inst state.DatabaseInstance, refs []state.DatabaseReference) error {
	list := make([]string, 0, len(refs))
	for _, r := range refs {
		refName := r.AppID
		if app, err := s.st.GetAppByID(ctx, r.AppID); err == nil {
			refName = app.Name
		}
		list = append(list, refName+"/"+r.Service)
	}
	return apperr.New("E_DB_REFERENCED",
		"database %q is referenced by %d app service(s) and cannot be deleted: %s (remove the fleetly.databases label and redeploy the referencing apps first)",
		inst.Name, len(refs), strings.Join(list, ", ")).
		WithContext("references", strings.Join(list, ",")).
		WithContext("count", fmt.Sprint(len(refs)))
}

// 状态层哨兵（ErrDatabaseNotFound/ErrDatabaseTerminal/ErrDatabaseStateConflict/
// ErrDatabaseIllegalTransition）→ api 语义的映射（D-DB-8：E_DB_NOT_FOUND 404
// / E_STATE_VERSION_CONFLICT 409 注册码投影）已收进 errors.go 的哨兵登记表
// ——本文件经 mapStoreErr 消费。

// databaseNotFound 构造 E_DB_NOT_FOUND（404；细节进 message 与 context——可行动
// 面设计 §5.2「附候选清单」口径）。带 name/detail 载荷的富投影是登记表外
// 的调用面特例（0 命中路径没有哨兵错误可映射）。
func databaseNotFound(name, detail string) error {
	msg := "database instance not found"
	if name != "" {
		msg += ": " + name
	}
	if detail != "" {
		msg += " (" + detail + ")"
	}
	return apperr.New("E_DB_NOT_FOUND", "%s", msg).
		WithContext("name", name).
		WithContext("detail", detail)
}

// databaseSettingsOf 组装 settings（proto → state）。
func databaseSettingsOf(l *serverv1.DatabaseLimits, p *serverv1.DatabaseBackupPlan) state.DatabaseSettings {
	return state.DatabaseSettings{
		CPUSeconds:  l.GetCpuSeconds(),
		MemoryBytes: l.GetMemoryBytes(),
		Backup:      databaseBackupPlanOf(p),
	}
}

func databaseBackupPlanOf(p *serverv1.DatabaseBackupPlan) state.DatabaseBackupPlan {
	return state.DatabaseBackupPlan{
		IntervalHours: int(p.GetIntervalHours()),
		Keep:          int(p.GetKeep()),
		HourUTC:       int(p.GetHourUtc()),
	}
}

// templateIDList 是注册表词表的人读形态（错误信息可行动面）。
func templateIDList() string {
	var ids []string
	for _, t := range dbtemplate.List() {
		ids = append(ids, t.ID)
	}
	return strings.Join(ids, ", ")
}

// databaseView 构造脱敏投影：解密凭据只为指纹（naming.Hash8——「是不是那
// 个值」的比对面），明文零离开本函数内存链。upgrade_available = 实例
// digest ≠ 模板当前钉定镜像（§2.2：既有实例不自动变，升级逐实例 opt-in）。
func (s *DatabaseService) databaseView(ctx context.Context, inst state.DatabaseInstance) (*serverv1.DatabaseView, error) {
	fingerprint := ""
	if inst.CredentialCipher != "" {
		plain, err := s.box.Decrypt([]byte(inst.CredentialCipher))
		if err != nil {
			return nil, fmt.Errorf("decrypt credential of database %s: %w", inst.Name, err)
		}
		fingerprint = naming.Hash8(string(plain))
	}
	upgradeAvailable := false
	if tpl, err := dbtemplate.Get(inst.Template); err == nil {
		upgradeAvailable = inst.ImageDigest != "" && inst.ImageDigest != tpl.Image
	}
	v := &serverv1.DatabaseView{
		Id:               inst.ID,
		Name:             inst.Name,
		Template:         inst.Template,
		ImageDigest:      inst.ImageDigest,
		Status:           string(inst.State),
		Placement:        inst.PlatformNodeID,
		LastError:        inst.LastError,
		UpgradeAvailable: upgradeAvailable,
		CreatedAt:        timestamppb.New(inst.CreatedAt),
		UpdatedAt:        timestamppb.New(inst.UpdatedAt),
		Limits: &serverv1.DatabaseLimits{
			CpuSeconds:  inst.Settings.CPUSeconds,
			MemoryBytes: inst.Settings.MemoryBytes,
		},
		BackupPlan: &serverv1.DatabaseBackupPlan{
			IntervalHours: int32(inst.Settings.Backup.IntervalHours),
			Keep:          int32(inst.Settings.Backup.Keep),
			HourUtc:       int32(inst.Settings.Backup.HourUTC),
		},
	}
	if !inst.CredentialUpdatedAt.IsZero() {
		v.CredentialUpdatedAt = timestamppb.New(inst.CredentialUpdatedAt)
	}
	if fingerprint != "" {
		v.Connection = connectionView(inst, fingerprint)
	}
	if vols, err := s.st.ListOwnerVolumes(ctx, state.VolumeOwnerDatabase, inst.ID); err == nil && len(vols) > 0 {
		v.Volume = &serverv1.DatabaseVolumeView{
			Name:           vols[0].Name,
			Status:         string(vols[0].Status),
			PlatformNodeId: vols[0].PlatformNodeID,
		}
	}
	return v, nil
}

// connectionView 连接信息脱敏投影（§2.5 键集只读子集；host = 实例名 DNS
// 别名——引用方 app 内的可达名；url 密码段为固定掩码）。
func connectionView(inst state.DatabaseInstance, fingerprint string) *serverv1.DatabaseConnectionView {
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		return nil // 模板在册期理论不可达（注册表只增）
	}
	vars, err := dbtemplate.ConnectionVars(inst.Template, inst.Name, connectionPasswordMask)
	if err != nil {
		return nil
	}
	prefix := dbtemplate.EnvPrefix(inst.Name)
	out := &serverv1.DatabaseConnectionView{
		Host:                vars[prefix+"_HOST"],
		Port:                int32(tpl.EnginePort),
		Url:                 vars[prefix+"_URL"],
		PasswordFingerprint: fingerprint,
	}
	if user, ok := vars[prefix+"_USER"]; ok {
		out.User = user
	}
	if pgDatabaseName, ok := vars[prefix+"_DATABASE"]; ok {
		out.Database = pgDatabaseName
	}
	return out
}

// databaseAudit 构造库操作审计条目（设计 §5.3 动作词表 db.*——与通用 auditEntry
// 的 api.* 方法级词根分立；actor 归因同款：API 无法区分人类/AI 代理，
// token 承载可追溯性）。
func databaseAudit(ctx context.Context, action, target, diff string) state.AuditEntry {
	tokenID := ""
	if p, ok := PrincipalFromContext(ctx); ok {
		tokenID = p.TokenID
	}
	return state.AuditEntry{
		Actor:        "human",
		ActorTokenID: tokenID,
		Action:       action,
		Target:       target,
		Result:       "ok",
		DiffSummary:  diff,
	}
}

// databaseLifecycleEvent 是生命周期受理事件（subject = database:<名>；payload 只
// 带事实字段——凭据材料零出现）。
func databaseLifecycleEvent(name string, inst state.DatabaseInstance) state.Event {
	return state.Event{
		Name:    name,
		Subject: "database:" + inst.Name,
		Payload: fmt.Sprintf(`{"instance":%q,"template":%q}`, inst.Name, inst.Template),
	}
}
