package database

// 受控升级编排（managed-databases §2.2「升级语义（受控重建 + 备份门）」，
// S5 验收步 7）：①pre_upgrade 备份门（verify 通过才继续——失败实例不动）
// ②digest 换新受控重建（有卷强制 stop-first——渲染层固定，停机窗口如实
// 累计）③健康门观察 ④失败 = digest 归位（回写旧值重建）+ db.upgrade_failed
// + 状态落 degraded——**不是 revision 重放**（库无 revision/部署记录，D-DB-1
// 终裁推论）。
//
// 状态机纪律：升级是**操作**不换主状态（§2.1）；paused 升级 = 仅换 spec 不
// 重启（convergePaused 渲染新 digest——resume 时以新版本重建，健康门推迟
// 到 resume 语义由既有收敛面承载）。失败终态按设计落 degraded：ready →
// degraded 走 EnterDbPhase（ready→degraded 合法边）；degraded 起步的失败
// 就地保持 degraded——digest 已归位，健康恢复由既有观察路径收口（观察恢
// 复到 ready 是系统诚实性的自然结果，degraded 是失败时刻的告警位）。
//
// 可升级检测（db.upgrade_available）：收敛拍尾部 duty——instance.digest ≠
// 模板当前 Image 即公告，per (实例, 目标 digest) 进程内去重（重启重公告
// 一次，诚实冗余优于静默）。
//
// 明文纪律：升级链无凭据面（备份门材料在 backup.go 内存链）；事件/审计
// 只带 digest 与阶段事实。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ErrUpgradeNotAvailable 表示实例已在模板当前版本（无可升级目标；api 映射
// E_STATE_VERSION_CONFLICT 族——「upgrade_available=false 是升级的非法前置
// 事实」，同族乐观冲突语义，零新增码）。
type ErrUpgradeNotAvailable struct {
	Instance string
	Current  string
	Template string
}

func (e ErrUpgradeNotAvailable) Error() string {
	return fmt.Sprintf("database %q is already at the template image (no upgrade available)", e.Instance)
}

// Upgrade 受理一次受控升级（API 面：快回 accepted；编排异步执行——备份门
// 与健康门是分钟级）。前置态（ready/degraded/paused，§2.3 操作表）/互斥/
// 可升级性在此同步裁决。返回 (旧 digest, 新 digest)（响应面/审计消费）。
func (m *Manager) Upgrade(ctx context.Context, name string) (string, string, error) {
	inst, err := m.store.GetDatabaseInstanceByName(ctx, name)
	if err != nil {
		return "", "", err
	}
	if inst.State == state.DatabaseDeleting || inst.State == state.DatabaseDeleted {
		return "", "", fmt.Errorf("%w: %s", state.ErrDatabaseNotFound, inst.Name)
	}
	switch inst.State {
	case state.DatabaseReady, state.DatabaseDegraded, state.DatabasePaused:
	default:
		return "", "", &OperationPrestateError{Operation: "upgrade", Current: string(inst.State), Legal: "ready, degraded, paused"}
	}
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		return "", "", fmt.Errorf("template %q is not in the platform registry", inst.Template)
	}
	if inst.ImageDigest == tpl.Image {
		return "", "", ErrUpgradeNotAvailable{Instance: inst.Name, Current: inst.ImageDigest, Template: tpl.Image}
	}
	if !m.beginOp(inst.ID, "upgrade") {
		return "", "", ErrOperationInFlight{Current: "backup/restore/upgrade"}
	}
	old := inst.ImageDigest
	m.opsWG.Add(1)
	//nolint:gosec // G118：异步链刻意脱离请求 ctx（受理已返回）——预算取升级 op 自身超时
	go func() {
		defer m.opsWG.Done()
		defer m.endOp(inst.ID)
		uctx, cancel := context.WithTimeout(context.Background(), upgradeOpTimeout)
		defer cancel()
		if err := m.runUpgrade(uctx, inst.ID, old, tpl.Image); err != nil {
			m.log.Warn("database: upgrade failed", "instance", inst.Name, "error", err.Error())
		}
	}()
	return old, tpl.Image, nil
}

// runUpgrade 是升级同步链（备份门 → digest 换新受控重建 → 健康门/归位）。
// 以 instanceID 重读权威态——受理到执行窗口内实例可能已被并发推进（删除/
// 暂停），每步重判。
func (m *Manager) runUpgrade(ctx context.Context, id, oldDigest, newDigest string) error {
	inst, err := m.store.GetDatabaseInstance(ctx, id)
	if err != nil {
		return err // 受理后实例消失/并发推进：无现场可升级，诚实退出
	}
	m.emitEvent(ctx, "db.upgrade_started", "database:"+inst.Name, map[string]string{
		"instance":   inst.Name,
		"old_digest": oldDigest,
		"new_digest": newDigest,
	})

	// ① 备份门（§2.2：pre_upgrade 备份且 verify 通过才继续；失败 → 实例
	// 不动，E_DB_BACKUP_FAILED 中止——runBackup 的 verify 失败路径已构造
	// 该信封）。
	//
	// paused 的引擎级边界（设计裁决边界，实现注记）：§2.3 操作表允许
	// paused 升级（spec-only），但 §2.2 备份门对停摆引擎不可执行（paused
	// 无任务，pg_dump 无从连接——备份前置态 ready/degraded 不含 paused，
	// D-DB-6 逻辑备份口径的构造性后果）。门的本意 = 动引擎前有可恢复快
	// 照；paused 的 spec-only 变更不触数据面，门降格为「台账内存在
	// verified 备份」的存在性检查——无任何 verified 备份的暂停实例如实
	// 拒绝（指引：resume → backup → suspend 后再升级），不做假门禁。
	if inst.State == state.DatabasePaused {
		if err := m.pausedGateVerifiedBackup(ctx, &inst); err != nil {
			m.emitEvent(ctx, "db.upgrade_failed", "database:"+inst.Name, map[string]string{
				"instance": inst.Name,
				"stage":    "backup_gate",
				"reason":   singleLine(err.Error()),
			})
			return err
		}
	} else if inst.State != state.DatabaseReady && inst.State != state.DatabaseDegraded {
		err := fmt.Errorf("upgrade backup gate requires state ready or degraded (current: %s)", inst.State)
		m.emitEvent(ctx, "db.upgrade_failed", "database:"+inst.Name, map[string]string{
			"instance": inst.Name,
			"stage":    "backup_gate",
			"reason":   singleLine(err.Error()),
		})
		return err
	} else if _, err := m.runBackup(ctx, &inst, dbtemplate.BackupPreUpgrade); err != nil {
		// 实例不动（digest/副本零写）；失败面 = backup_failed 事件（已落）
		// + upgrade_failed 事件 + 错误上抛（E_DB_BACKUP_FAILED 信封原样保
		// 留——门禁语义的诚实归因）。
		m.emitEvent(ctx, "db.upgrade_failed", "database:"+inst.Name, map[string]string{
			"instance": inst.Name,
			"stage":    "backup_gate",
			"reason":   singleLine(err.Error()),
		})
		return err
	}

	// ② digest 换新（权威态先行）+ 受控重建（渲染 + ensureService——
	// stop-first 顺序由渲染层固定；paused = 仅 spec 不重启）。
	if err := m.store.UpdateImageDigest(ctx, inst.ID, newDigest); err != nil {
		return m.upgradeFail(ctx, &inst, oldDigest, "persist", err)
	}
	inst.ImageDigest = newDigest
	desired, err := m.applyDesiredService(ctx, &inst, replicasOf(inst.State))
	if err != nil {
		return m.upgradeFail(ctx, &inst, oldDigest, "converge", err)
	}

	// ③ paused：spec-only（无任务可观察——升级在 resume 时以新版本重建；
	// 健康门语义由 resume 收敛面承载，此处 spec 落地即完成）。
	if inst.State == state.DatabasePaused {
		m.emitEvent(ctx, "db.upgrade_finished", "database:"+inst.Name, map[string]string{
			"instance":   inst.Name,
			"digest":     newDigest,
			"note":       "paused instance: spec updated only, engine rebuilds on resume",
		})
		m.log.Info("database: upgrade applied to paused instance (spec-only; rebuild deferred to resume)",
			"instance", inst.Name, "digest", newDigest)
		return nil
	}

	// ④ 健康门观察窗（watchHealthy 的判定真值源复用——目标镜像任务
	// running=healthy / failed|rejected=failing / 其余 pending 等待；窗口
	// 在 Manager 构造时取平台缺省，单测注入短窗）。
	deadline := time.Now().Add(m.upgradeWatch)
	for {
		verdict, _ := m.healthVerdict(ctx, desired.Name, newDigest)
		switch verdict {
		case healthHealthy:
			m.emitEvent(ctx, "db.upgrade_finished", "database:"+inst.Name, map[string]string{
				"instance": inst.Name,
				"digest":   newDigest,
			})
			m.log.Info("database: upgrade finished (new digest healthy)", "instance", inst.Name, "digest", newDigest)
			return nil
		case healthFailing:
			return m.upgradeFail(ctx, &inst, oldDigest, "health_gate",
				errors.New("new image task failed the health gate"))
		default: // healthPending：等待窗口
		}
		if !time.Now().Before(deadline) {
			return m.upgradeFail(ctx, &inst, oldDigest, "health_gate",
				fmt.Errorf("health gate timeout after %s (no healthy task on the new image)", m.upgradeWatch))
		}
		select {
		case <-ctx.Done():
			return m.upgradeFail(ctx, &inst, oldDigest, "health_gate", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// pausedGateVerifiedBackup 是 paused 升级的备份门降格形态（§2.2/§2.3 裁决
// 边界的实现注记见 runBackup 门块注释）：台账内存在 verified 备份 = 门通
// 过；空台账/全失败 = 如实拒绝。
func (m *Manager) pausedGateVerifiedBackup(ctx context.Context, inst *state.DatabaseInstance) error {
	rows, err := m.store.ListDatabaseBackups(ctx, inst.ID, 50)
	if err != nil {
		return fmt.Errorf("upgrade backup gate (paused): read ledger: %w", err)
	}
	for _, r := range rows {
		if r.VerifyStatus == state.DatabaseVerifyVerified {
			return nil
		}
	}
	return apperr.New("E_DB_BACKUP_FAILED",
		"upgrade of a paused instance requires a verified backup in the ledger (the pre_upgrade gate cannot run against a stopped engine); resume the instance, take a backup, then upgrade — or restore-and-upgrade after the next scheduled window").
		WithContext("instance", inst.Name).
		WithContext("stage", "backup_gate").
		WithContext("current_state", string(inst.State))
}

// upgradeFail 升级失败收口（§2.2：digest 归位 + db.upgrade_failed + 状态落
// degraded）：回写旧 digest → 重建（归位）→ last_error + degraded（ready→
// degraded 合法边；degraded 起步保持）→ 事件 + 系统审计（自动动作必入审
// 计）。归位失败不再有出边——错误如实上抛（人工收尾：retry/手动 digest 回
// 写），事件已披露现场。
func (m *Manager) upgradeFail(ctx context.Context, inst *state.DatabaseInstance, oldDigest, stage string, cause error) error {
	reason := singleLine(fmt.Sprintf("upgrade failed at stage %s: %v (digest rolled back to the previous value)", stage, cause))
	// 归位：digest 回写 + 重建（副本水位按当前态——paused 只在 spec-only
	// 路径出现，失败路径必为 ready/degraded → 副本 1）。
	if err := m.store.UpdateImageDigest(ctx, inst.ID, oldDigest); err != nil {
		m.log.Error("database: digest rollback write failed (manual recovery required: re-run upgrade or restore the digest)", "instance", inst.Name, "error", err)
		reason = singleLine(reason + "; digest rollback write also failed: " + err.Error())
	} else {
		inst.ImageDigest = oldDigest
		if _, err := m.applyDesiredService(ctx, inst, 1); err != nil {
			m.log.Error("database: digest rollback rebuild failed (converge duty will retry from the authoritative row)", "instance", inst.Name, "error", err)
		}
	}
	// 状态落 degraded（§2.2）：ready→degraded 合法边；degraded 保持；
	// CAS 落败（并发推进——删除等）静默让位，事件面仍披露。
	if inst.State == state.DatabaseReady {
		err := m.store.InTx(ctx, func(tx *state.Tx) error {
			if err := tx.SetDatabaseLastError(ctx, inst.ID, reason); err != nil {
				return err
			}
			return tx.EnterDbPhase(ctx, inst.ID, state.DatabaseReady, state.DatabaseDegraded)
		})
		if err != nil && !errIsNotFound(err) && !errors.Is(err, state.ErrDatabaseStateConflict) {
			m.log.Warn("database: enter degraded failed (rollback state disclosure)", "instance", inst.Name, "error", err)
		}
	} else {
		if err := m.store.SetDatabaseLastError(ctx, inst.ID, reason); err != nil {
			m.log.Warn("database: restore failure record failed", "instance", inst.Name, "error", err)
		}
	}
	m.emitEvent(ctx, "db.upgrade_failed", "database:"+inst.Name, map[string]string{
		"instance":   inst.Name,
		"stage":      stage,
		"reason":     reason,
		"old_digest": oldDigest,
	})
	// 系统审计（digest 归位是自动动作——§5.3「自动动作必入审计」）。
	if err := m.writeAudit(ctx, "db.upgrade", "database:"+inst.ID,
		state.DiffSummary("result", "rolled_back", "stage", stage, "digest", oldDigest)); err != nil {
		m.log.Warn("database: rollback audit write failed", "instance", inst.Name, "error", err)
	}
	return fmt.Errorf("%s (instance left on the previous image; state degraded)", reason)
}

// replicasOf 取实例当前态的期望副本水位（paused=0 其余=1——renderService
// 调用方的既有口径）。
func replicasOf(s state.DatabaseState) uint64 {
	if s == state.DatabasePaused {
		return 0
	}
	return 1
}

// dutyUpgradeAdvertisement 是收敛拍尾部的可升级公告 duty：instance.digest ≠
// 模板当前 Image → db.upgrade_available（per (实例, 目标 digest) 进程内去
// 重——重启重公告一次）。upgrade 的 opt-in 受理面不受此影响（重复公告不产
// 生重复受理）。
func (m *Manager) dutyUpgradeAdvertisement(ctx context.Context, rows []state.DatabaseInstance) {
	for i := range rows {
		inst := &rows[i]
		if inst.State == state.DatabaseDeleting || inst.State == state.DatabaseDeleted {
			continue
		}
		tpl, err := dbtemplate.Get(inst.Template)
		if err != nil {
			continue // 模板在册期理论不可达（注册表只增）
		}
		if inst.ImageDigest == tpl.Image {
			delete(m.upgradeAdvertised, inst.ID)
			continue
		}
		if m.upgradeAdvertised[inst.ID] == tpl.Image {
			continue // 已公告过该目标：不重复
		}
		m.emitEvent(ctx, "db.upgrade_available", "database:"+inst.Name, map[string]string{
			"instance":   inst.Name,
			"template":   inst.Template,
			"current":    inst.ImageDigest,
			"available":  tpl.Image,
		})
		if m.upgradeAdvertised == nil {
			m.upgradeAdvertised = map[string]string{}
		}
		m.upgradeAdvertised[inst.ID] = tpl.Image
		m.log.Info("database: upgrade available (template image digest moved; opt-in per instance)",
			"instance", inst.Name, "template", inst.Template)
	}
}
