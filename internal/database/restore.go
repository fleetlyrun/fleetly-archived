package database

// 原地恢复编排（managed-databases §2.6「恢复」行 / §2.3 操作表，S5）：停
// 库重放——实例服务 scale 0（paused 收敛面同款直接 ServiceUpdate）→ 一次性
// job 钉绑定节点挂数据卷 rw 重放（PG = temp postgres 起停重放；Redis =
// RDB 落卷）→ 重部署（渲染 + ensureService 副本 1）。
//
// 状态机纪律（§2.1）：恢复是**操作**不换主状态——实例全程保持 ready/
// degraded；操作互斥哨兵挡住收敛拍对副本水位/健康的干扰（opBusy 跳过）。
// degraded 实例恢复后仍是 degraded，健康由既有观察路径自然恢复（设计明
// 示口径）。
//
// 失败诚实口径（设计 §2.6「原地恢复中断即 critical 告警」）：scale 0 之后
// 任一步失败 → 实例**保持停止**（不盲目重启——半程恢复的数据面状态未知，
// 人工裁决才是诚实出口）+ last_error 落阶段上下文 + db.restore_failed 事
// 件 + E_DB_RESTORE_FAILED 信封（错误文本带人工 runbook 指引：重试恢复或
// 手动 resume）。阶段词表：prestate/materialize/scale_zero/replay/resume。
//
// 明文纪律：凭据明文只进 job env；恢复命令词表零凭据（PG 走 socket trust
// ——见 adapters.go restoreJobScript 注）。

import (
	"context"
	"errors"
	"fmt"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// RestoreStageError 是恢复中途失败的阶段锚（E_DB_RESTORE_FAILED 的 context
// 面——人工收尾按阶段对账）。
type RestoreStageError struct {
	Stage string
	Err   error
}

func (e *RestoreStageError) Error() string {
	if e.Err == nil {
		return "restore failed at stage " + e.Stage
	}
	return fmt.Sprintf("restore failed at stage %s: %v", e.Stage, e.Err)
}

// restoreAbort 是编排层失败面的收口构造（阶段上下文 + E_DB_RESTORE_FAILED
// 信封 + runbook 指引——实例保持停止的现场说明随错误文本落 last_error 与
// 事件）。
func restoreAbort(inst *state.DatabaseInstance, stage string, err error) error {
	wrapped := &RestoreStageError{Stage: stage, Err: err}
	return apperr.New("E_DB_RESTORE_FAILED", "%s (the instance is left stopped for manual inspection: retry the restore, or run 'fleetly databases resume %s' to bring the previous engine back; recoverable data remains on the volume)", wrapped.Error(), inst.Name).
		WithContext("instance", inst.Name).
		WithContext("stage", stage)
}

// RestoreBackup 受理并执行一次原地恢复（编排本体与 backup 同款形态：API
// 快回 accepted，重放异步执行——job 分钟级）。前置态/互斥/快照归属在此同
// 步裁决；scale 0 之后的一切在异步链。
func (m *Manager) RestoreBackup(ctx context.Context, name, snapshotID string) error {
	inst, err := m.store.GetDatabaseInstanceByName(ctx, name)
	if err != nil {
		return err
	}
	if inst.State == state.DatabaseDeleting || inst.State == state.DatabaseDeleted {
		return fmt.Errorf("%w: %s", state.ErrDatabaseNotFound, inst.Name)
	}
	// 前置态前哨（§2.3 操作表：ready/degraded）。
	switch inst.State {
	case state.DatabaseReady, state.DatabaseDegraded:
	default:
		return &OperationPrestateError{Operation: "restore", Current: string(inst.State), Legal: "ready, degraded"}
	}
	if snapshotID == "" {
		return errors.New("restore requires a snapshot id (list backups with: fleetly databases backups <name>)")
	}
	// 快照归属守卫：只重放**本实例台账内**的快照（跨实例误指即在此拦下
	// ——数据安全前哨与 E_DB_REFERENCED 同型）。
	if _, err := m.store.GetDatabaseBackupBySnapshot(ctx, inst.ID, snapshotID); err != nil {
		if errors.Is(err, state.ErrDatabaseBackupNotFound) {
			return fmt.Errorf("snapshot %s is not in the ledger of database %q (cross-instance restore is refused; list backups with: fleetly databases backups %s)", snapshotID, inst.Name, inst.Name)
		}
		return err
	}
	if inst.PlatformNodeID == "" {
		return errors.New("restore requires the pinned node binding (placement not converged)")
	}
	if !m.beginOp(inst.ID, "restore") {
		return ErrOperationInFlight{Current: "backup/restore/upgrade"}
	}
	m.opsWG.Add(1)
	//nolint:gosec // G118：异步链刻意脱离请求 ctx（受理已返回）——预算取恢复链步之和的宽限形态
	go func() {
		defer m.opsWG.Done()
		defer m.endOp(inst.ID)
		actx, cancel := context.WithTimeout(context.Background(), restoreJobTimeout+2*backupJobTimeout)
		defer cancel()
		if err := m.runRestore(actx, &inst, snapshotID); err != nil {
			m.log.Warn("database: restore failed", "instance", inst.Name, "error", err.Error())
		}
	}()
	return nil
}

// runRestore 是恢复同步链（scale 0 → 重放 job → 重部署 → 事件/审计）。
// 事件口径 = 注册表 18 事件的 restore_completed/restore_failed（§5.3——无
// restore_started，受理审计在 api 层 db.restore 承载留痕）。
func (m *Manager) runRestore(ctx context.Context, inst *state.DatabaseInstance, snapshotID string) error {
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		// scale 0 之前的失败：实例未动，破坏性阶段未开始——常规错误上抛
		//（restore_failed 是「停库重放中断」的 critical 口径，不滥用）。
		return fmt.Errorf("template %q is not in the platform registry", inst.Template)
	}
	target, err := m.resolveBackupTarget(ctx)
	if err != nil {
		if errors.Is(err, ErrBackupS3NotConfigured) {
			return err
		}
		return fmt.Errorf("restore materials: %w", err)
	}
	// 凭据可解性前置（破坏性操作前验证权威态材料可读——解密失败的恢复不
	// 受理：重放后引擎会以备份内旧密码起服，凭据面错位必须先暴露）。
	if _, err := m.decryptCredential(inst); err != nil {
		return fmt.Errorf("restore materials: %w", err)
	}
	volName, err := naming.DBVolumeName(inst.Name, tpl.VolumeKey, inst.ID)
	if err != nil {
		return fmt.Errorf("restore materials: %w", err)
	}
	// 数据卷幂等确保（暂停恢复窗口/外部清理后的自愈——挂载不能悬空）。
	if err := m.docker.VolumeEnsure(ctx, volName); err != nil {
		return m.restoreFail(ctx, inst, snapshotID, "materials", err)
	}

	// ① scale 0（paused 收敛面同款：渲染副本 0 幂等收敛——stop-first 语义
	// 下任务下线；实例主状态不变，操作互斥挡住收敛拍纠偏）。
	if _, err := m.applyDesiredService(ctx, inst, 0); err != nil {
		return m.restoreFail(ctx, inst, snapshotID, "scale_zero", err)
	}

	// ② 停库重放（job 钉绑定节点挂数据卷 rw——远端 local 卷不可经 manager
	// 读的既有硬约束下的唯一执行位置。单 job：dbtools 自 v0.2.1-dbtools.1
	// 起是 debian/glibc 基底，restic 取回与临时实例重放同 job 完成——双
	// job 形态随 musl/glibc 跨 libc 重放风险的消除而回退）。
	restoreErr := m.Restore(ctx, dbtemplate.RestoreInput{
		Instance:            inst.Name,
		TemplateID:          inst.Template,
		TeamSlug:            inst.TeamSlug,
		PrjSlug:             inst.ProjectSlug,
		VolumeName:          volName,
		VolumeTarget:        tpl.VolumeMountPath,
		BindNodeID:          inst.PlatformNodeID,
		Repository:          target.Repository,
		SnapshotID:          snapshotID,
		ResticPassword:      target.ResticPass,
		S3AccessKeyID:       target.AccessKeyID,
		S3SecretKey:         target.SecretKey,
		S3Region:            target.Region,
		S3PathStyle:         target.PathStyle,
		AttachRustfsNetwork: target.AttachRustfs,
	})
	if restoreErr != nil {
		// 半程中断：实例保持停止 + 诚实现场（stage 上下文 + runbook 指引）。
		return m.restoreFail(ctx, inst, snapshotID, "replay", restoreErr)
	}

	// ③ 重部署（渲染 + ensureService 副本 1——resume 收敛面同款，不经状
	// 态机；degraded 实例保持 degraded，健康由观察路径自然收口）。
	if _, err := m.applyDesiredService(ctx, inst, 1); err != nil {
		return m.restoreFail(ctx, inst, snapshotID, "resume", err)
	}

	// ④ 完成面：事件 + last_error 清场（失败的失败现场由成功覆盖——恢复
	// 语义即「回到无现场」）。
	if err := m.store.SetDatabaseLastError(ctx, inst.ID, ""); err != nil {
		m.log.Warn("database: restore completed but last_error clear failed", "instance", inst.Name, "error", err)
	}
	m.emitEvent(ctx, "db.restore_completed", "database:"+inst.Name, map[string]string{
		"instance": inst.Name,
		"snapshot": snapshotID,
		"template": inst.Template,
	})
	m.log.Info("database: restore completed in place (instance redeployed)", "instance", inst.Name, "snapshot", snapshotID)
	return nil
}

// restoreFail 恢复失败收口：last_error 落现场 + db.restore_failed 事件 +
// E_DB_RESTORE_FAILED 信封（实例保持停止——调用方不再触碰副本水位）。
func (m *Manager) restoreFail(ctx context.Context, inst *state.DatabaseInstance, snapshotID, stage string, cause error) error {
	err := restoreAbort(inst, stage, cause)
	reason := singleLine(scrubText(err.Error(), m.credentialOf(inst)))
	if serr := m.store.SetDatabaseLastError(ctx, inst.ID, reason); serr != nil {
		m.log.Warn("database: restore failed AND last_error write failed", "instance", inst.Name, "error", serr)
	}
	m.emitEvent(ctx, "db.restore_failed", "database:"+inst.Name, map[string]string{
		"instance": inst.Name,
		"snapshot": snapshotID,
		"stage":    stage,
		"reason":   reason,
	})
	return err
}

// credentialOf 解密凭据明文的 scrub 材料形态（解密失败回空——兜底尽力）。
func (m *Manager) credentialOf(inst *state.DatabaseInstance) string {
	plain, err := m.decryptCredential(inst)
	if err != nil {
		return ""
	}
	return plain
}

// applyDesiredService 渲染实例期望投影并幂等收敛到指定副本数（恢复的
// scale 0/重部署与升级的受控重建共用——watchHealthy/convergePaused 的收敛
// 前奏同款：凭据 secret ensure → render → spec build → ensureService）。
// 返回渲染投影（升级健康门的镜像判据消费）。
func (m *Manager) applyDesiredService(ctx context.Context, inst *state.DatabaseInstance, replicas uint64) (engine.ServiceSpec, error) {
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		return engine.ServiceSpec{}, fmt.Errorf("template %q is not in the platform registry", inst.Template)
	}
	password, err := m.decryptCredential(inst)
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	secretIDs := map[string]string{}
	if tpl.CredentialDelivery == dbtemplate.CredentialSecretFile {
		secretName, err := naming.DBSecretName(inst.TeamSlug, inst.ProjectSlug, inst.Name, pgSecretKey, naming.Hash8(password))
		if err != nil {
			return engine.ServiceSpec{}, err
		}
		id, exists, err := m.docker.SecretInspect(ctx, secretName)
		if err != nil {
			return engine.ServiceSpec{}, err
		}
		if !exists {
			if id, err = m.ensureCredentialSecret(ctx, inst.QualifiedName(), secretName, password); err != nil {
				return engine.ServiceSpec{}, err
			}
		}
		secretIDs[secretName] = id
	}
	desired, err := renderService(inst, password, replicas)
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	swarmSpec, err := buildServiceSpec(desired, inst.QualifiedName(), secretIDs)
	if err != nil {
		return engine.ServiceSpec{}, err
	}
	if err := m.ensureService(ctx, desired.Name, swarmSpec, desired.DesiredHash(), replicas); err != nil {
		return engine.ServiceSpec{}, err
	}
	return desired, nil
}
