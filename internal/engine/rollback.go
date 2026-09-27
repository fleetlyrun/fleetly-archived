package engine

// 回滚（T2.12，release-semantics §2.4 单层快照重放 + §2.3 kind=rollback）：
//   - 回滚目标 = revisions 保留窗内最近 5 次成功部署（列表即选项）；越界 /
//     不存在 / 归属不符 → E_ROLLBACK_NO_TARGET；
//   - 重放语义：compose 字段与合并 env 按快照（执行形态 = 目标 succeeded
//     deployment 行的 desired_spec 密文——合并 env 明文只在密文里）；
//     **source=system 行除外**（E4 托管数据库 D-DB-11 修正）：连接串等平台
//     物化行不随快照回放——重放时按 key 从 env_vars 现读当前解密值（凭据
//     轮换后回滚不再复活旧密码，D-REL-9 同族）；键已不在 env_vars 的保留
//     快照值（物化归一发生在下一次常规部署）；治理
//     参数（看门狗/观察窗）取当前引擎配置；secret 值取当前（v0.1 平台
//     密钥库未接入，快照不含 secret——结构性地满足「回滚不撤销密钥轮换」，
//     密钥票落地后此处语义不变）；卷数据/DB 迁移/DNS 不回滚；
//   - preflight 四项（镜像可得/约束可满足/secret 存在/compose 合法）在动
//     底座之前执行，失败按原因码落 failed（不动 app、不关收敛 opt-in）；
//   - 已切流回滚 = 新 deployment（kind=rollback，recovery_of 指向被回退的
//     源部署），复用 T2-5a 全链：对账/健康门/观察窗；
//   - 回滚执行后失败（已动底座）：E_ROLLBACK_FAILED（critical，不再二次
//     自动恢复，D-REL-10）+ 该 app 漂移收敛 opt-in 强制关闭，直至人工重置
//    （`fleetly drift enable`，带审计）；
//   - 事件 deployment.rollback_started / rollback_finished / rollback_failed
//    （release-semantics §2.7）+ 审计（actor 透传）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/envlayer"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// kindRollback 是 deployments.kind 的回滚取值（00001 CHECK 词表）。
const kindRollback = "rollback"

// RollbackInput 是一次回滚入队的请求。
type RollbackInput struct {
	// AppName 是 compose 应用名。
	AppName string
	// TargetRevisionID 是回滚目标版本快照；空 = 最近一次成功部署的版本
	//（`fleetly rollback <app>` 缺省回退一版）。
	TargetRevisionID string
	// Actor 是审计操作主体（human/ai_agent；CLI 直连为 human）。空按
	// human 兜底（审计列非空约束）。
	Actor string
}

// EnqueueRollback 解析回滚目标并入队 kind=rollback 部署（CLI 入口；与
// deploy 同拓扑——入队与执行跨进程解耦，执行由 fleetlyd 引擎扫描推进）。
// 目标解析失败（app 不存在 / revision 越界或被保留窗淘汰 / 源部署快照
// 缺失）→ E_ROLLBACK_NO_TARGET。
func EnqueueRollback(ctx context.Context, st *state.Store, in RollbackInput) (state.DeployRecord, error) {
	app, err := st.GetAppByName(ctx, in.AppName)
	if err != nil {
		if errors.Is(err, state.ErrAppNotFound) {
			return state.DeployRecord{}, errorf("E_ROLLBACK_NO_TARGET",
				"app %s does not exist: nothing to roll back to (rollback targets = the last 5 successful deployments kept in the revisions retention window)", in.AppName).
				WithContext("app", in.AppName)
		}
		return state.DeployRecord{}, errorf("E_RUNTIME_UNAVAILABLE", "failed to read app: %v", err)
	}

	rev, err := resolveRollbackTarget(ctx, st, app.ID, in.TargetRevisionID)
	if err != nil {
		return state.DeployRecord{}, err
	}

	// 重放执行形态与回退对象：replay source = 创建目标 revision 的
	// succeeded 部署行（密文快照，含合并 env 明文——§2.4「env 随快照
	// 回滚」的载体）；origin = 最近一次 succeeded 部署（被回退的现行
	// 版本），recovery_of 指向它（D-REL-7：回滚记录指向的原 deployment）。
	origin, source, err := rollbackDeployments(ctx, st, app.ID, rev.ID)
	if err != nil {
		return state.DeployRecord{}, err
	}

	actor := in.Actor
	if actor == "" {
		actor = "human"
	}
	// fail-closed 单事务（H13 修复，state-model §2.9）：回滚部署行、
	// deployment.rollback_started 事件与审计在同一个 InTx 内写入——回调内
	// 任一失败（含事件/审计写失败）整体回滚、部署行不落库，杜绝「引擎照常
	// 执行但事件与审计缺失」的审计黑洞。回调内拿到的 rec（含生成的 ID/
	// 时间戳）供事件 payload 与审计使用；建行失败仍映射
	// E_RUNTIME_UNAVAILABLE（错误形态与合并前一致）。
	var rec state.DeployRecord
	if err := st.InTx(ctx, func(tx *state.Tx) error {
		r, err := tx.CreateDeployment(ctx, state.DeployRecord{
			AppID:           app.ID,
			AppName:         app.Name,
			Kind:            kindRollback,
			RecoveryOf:      origin.ID,
			SpecHash:        source.SpecHash,
			EnvSnapshotHash: source.EnvSnapshotHash,
			DesiredHash:     source.DesiredHash,
			DesiredSpec:     source.DesiredSpec,
			ComposePath:     source.ComposePath,
		})
		if err != nil {
			return errorf("E_RUNTIME_UNAVAILABLE", "failed to create rollback deployment: %v", err)
		}
		rec = r
		if err := appendEvents(ctx, tx, eventOf("deployment.rollback_started", "deployment:"+rec.ID,
			"deployment", rec.ID, "app", app.Name,
			"target_revision", rev.ID, "source_deployment", source.ID,
			"recovery_of", origin.ID)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:  actor,
			Action: "deployment.rollback",
			Target: "deployment:" + rec.ID,
			Result: "ok",
			// MG-6：构造器替换手拼 JSON。
			DiffSummary: state.DiffSummary("app", app.Name, "target_revision", rev.ID,
				"source_deployment", source.ID, "recovery_of", origin.ID),
		})
	}); err != nil {
		return state.DeployRecord{}, err
	}
	return rec, nil
}

// resolveRollbackTarget 解析回滚目标 revision（保留窗 active 谓词 + 缺省
// 取最新；越界/不存在 → E_ROLLBACK_NO_TARGET——列表即选项，无额外状态机）。
func resolveRollbackTarget(ctx context.Context, st *state.Store, appID, revisionID string) (state.Revision, error) {
	if revisionID == "" {
		rows, err := st.ListRevisions(ctx, appID)
		if err != nil {
			return state.Revision{}, errorf("E_RUNTIME_UNAVAILABLE", "failed to list revisions: %v", err)
		}
		if len(rows) == 0 {
			return state.Revision{}, errorf("E_ROLLBACK_NO_TARGET",
				"app has no revisions to roll back to (revisions retention window is empty — no successful deployments yet)").WithContext("app_id", appID)
		}
		return rows[0], nil
	}
	rev, err := st.GetAppRevision(ctx, appID, revisionID)
	if err != nil {
		if errors.Is(err, state.ErrRevisionNotFound) {
			return state.Revision{}, errorf("E_ROLLBACK_NO_TARGET",
				"revision %s is not in the rollback set (the retention window lists only the last %d successful deployments; see fleetly revisions list)",
				revisionID, state.RevisionKeepVersions).
				WithContext("revision", revisionID)
		}
		return state.Revision{}, errorf("E_RUNTIME_UNAVAILABLE", "failed to read revision: %v", err)
	}
	return rev, nil
}

// rollbackDeployments 反查回滚涉及的两个部署行：origin（最近一次
// succeeded——被回退的现行版本）与 source（创建目标 revision 的成功部署
// ——重放快照载体）。source 缺失（理论不可达：revision 仅在成功终态固化）
// → E_ROLLBACK_NO_TARGET（目标不可重放即不可回滚）。
func rollbackDeployments(ctx context.Context, st *state.Store, appID, revisionID string) (state.DeployRecord, state.DeployRecord, error) {
	rows, err := st.ListAppDeployments(ctx, appID, 50)
	if err != nil {
		return state.DeployRecord{}, state.DeployRecord{}, errorf("E_RUNTIME_UNAVAILABLE", "failed to read deployment history: %v", err)
	}
	var origin, source state.DeployRecord
	for _, r := range rows {
		if r.Status != state.DeploySucceeded {
			continue
		}
		if origin.ID == "" {
			origin = r // created_at 倒序：首个 succeeded 即最新
		}
		if r.RevisionID == revisionID && r.DesiredSpec != "" {
			source = r
			break // origin 已就位后即可停
		}
	}
	if source.ID == "" {
		return state.DeployRecord{}, state.DeployRecord{}, errorf("E_ROLLBACK_NO_TARGET",
			"revision %s has no replayable deployment snapshot (history record missing or corrupted)", revisionID).
			WithContext("revision", revisionID)
	}
	if origin.ID == "" {
		origin = source
	}
	return origin, source, nil
}

// runRollbackPreparing 执行 kind=rollback 部署的准备阶段：跳过 compose
// 重载/构建核对（期望态已随入队固化），preflight 四项通过后直接进
// releasing 并按快照对账（§2.4：preflight 任一失败在动底座之前失败）。
func (e *Engine) runRollbackPreparing(ctx context.Context, rec state.DeployRecord) error {
	// 未触底座：cancel 直接落 cancelled（无归位动作）。
	if rec.CancelRequested {
		return e.cancelTerminal(ctx, rec)
	}
	// 回滚准备预算同 deploying 路径：基线（拾取时刻）起算，排队等待不计入
	//（H11）；存量行基线为 0 回落 created_at 保持旧语义。
	if anchor := prepareBudgetBaseline(rec); !anchor.IsZero() && e.now().Sub(anchor) > e.cfg.DeployTimeout {
		return e.failRollbackPreflight(ctx, rec, errorf("E_RUNTIME_UNAVAILABLE",
			"rollback preparing exceeded the deploy watchdog budget (substrate unavailable or environment error)"))
	}
	if err := e.sub.SwarmReady(ctx); err != nil {
		if errors.Is(err, ErrNotSwarmReady) {
			e.log.Warn("engine: swarm not ready, retrying rollback next tick", "deployment", rec.ID)
			return nil // 暂态：下一 tick 重试（预算由上守）
		}
		return e.failRollbackPreflight(ctx, rec, errorf("E_RUNTIME_UNAVAILABLE", "substrate check failed: %v", err))
	}

	// D-DB-11（E4 托管数据库）：快照的合并 env 对 source=system 行按 key
	// 取当前值——代换已下沉 restoreSnapshot 共享原语（归位/回滚/漂移收敛
	// 全路径统一），此处不再单独修正。
	specs, err := e.decodeSpecs(rec)
	if err != nil || len(specs) == 0 {
		return e.failRollbackPreflight(ctx, rec, errorf("E_ROLLBACK_FAILED",
			"rollback desired-state snapshot unreadable (%v): target revision is not replayable", err))
	}

	// preflight 四项（§2.4）——任一失败不动底座。
	if err := e.preflightRollback(ctx, rec, specs); err != nil {
		return e.failRollbackPreflight(ctx, rec, err)
	}

	// releasing 迁移（哈希/快照已在入队时落行；看门狗按当前平台配置起算
	// ——治理参数取当前，§2.4）。经单写点（T0-V2.2）同事务生效。
	releaseAt := e.now()
	deadline := releaseAt.Add(e.cfg.DeployTimeout)
	if err := e.store.EnterPhase(ctx, rec.ID, state.DeployPreparing, state.DeployReleasing, state.DeploymentPatch{
		ReleaseStartedAt:   &releaseAt,
		WatchdogDeadlineAt: &deadline,
	}); err != nil {
		return err
	}
	rec.Status = state.DeployReleasing

	// 快照对账（归位/回滚共用原语：确定性、幂等、禁 --force）。
	if err := e.restoreSnapshot(ctx, rec, specs); err != nil {
		ae := appErrOf(err, rec.ID)
		return e.failRollbackDeployment(ctx, rec, ae.Code(), ae.Message())
	}
	return nil
}

// replaySystemEnvCurrent 是 D-DB-11 的重放修正（E4 托管数据库 §6 冲突 ①，
// 对发布专项 D-REL-9 的兑现）：快照的合并 env 中，凡 key 当前存在于
// env_vars 且 source=system 的条目，值替换为当前解密值——凭据轮换（S4
// rotate 更新 system 物化行）后重放不复活旧密码。边界（诚实口径）：键已
// 不在 env_vars（引用移除后的清理拍等）的条目保留快照原值——重物化归一
// 发生在下一次常规部署（prepareInputs 重物化 + 合并），重放路径不做物化。
// 调用点 = restoreSnapshot 共享原语头部：kind=rollback 回滚、失败归位
// （recovery=replay）、release 失败回退、漂移收敛四条重放路径统一过此
// 修正（验收裁决：D-REL-9 是全路径纪律，不设单路径豁免）。
//
// 解密失败 = 密钥/密文损坏：显式失败（E_RUNTIME_UNAVAILABLE）不静默降级
// ——带着错值重放比失败更危险。
func (e *Engine) replaySystemEnvCurrent(ctx context.Context, rec state.DeployRecord, specs []ServiceSpec) error {
	rows, err := e.store.ListAppEnv(ctx, rec.AppID)
	if err != nil {
		return errorf("E_RUNTIME_UNAVAILABLE", "failed to read env rows for rollback replay: %v", err)
	}
	current := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.Source != string(envlayer.SourceSystem) {
			continue
		}
		plain, derr := e.box.Decrypt([]byte(row.Value))
		if derr != nil {
			return errorf("E_RUNTIME_UNAVAILABLE",
				"failed to decrypt system env %s for rollback replay (master key mismatch or corrupted ciphertext)", row.Key)
		}
		current[row.Key] = string(plain)
	}
	if len(current) == 0 {
		return nil
	}
	for i := range specs {
		for j, kv := range specs[i].Env {
			key, _, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if v, hit := current[key]; hit {
				specs[i].Env[j] = key + "=" + v
			}
		}
	}
	return nil
}

// preflightRollback 执行回滚 preflight 四项（release-semantics §2.4）：
//  1. 镜像可得：build.PreflightImage（E_IMAGE_UNAVAILABLE 信封自带
//     W_ROLLBACK_IMAGE_RISK——v0.1 无 registry，镜像被清理时提示保留或重建）；
//  2. 约束可满足：放置前哨（绑定节点 ready / 卷归属一致——取当前平台
//     绑定状态，不放快照）；
//  3. secret 存在：快照可携带 SecretMount（E4 managed-databases §2.7 起，
//     名内嵌值指纹）——逐名对 app_secrets 现值解析（名 = fleetly-<app>-
//     <name>-<hash8>，值轮换即换名），可解析 → applyDesired 幂等确保在位；
//     悬空（轮换/移除已发生）→ E_SECRET_NOT_FOUND 诚实失败（快照不可变
//     纪律，不做静默改写——D-REL-9 同族）；
//  4. compose 合法：取舍——信任归一化快照（目标 compose 在原部署入队时
//     已过受控子集校验，快照是其 canonical 投影），只复核关键执行面
//     （服务集非空、服务名在平台命名空间内、镜像引用非空）。不做反解析：
//     compose_normalized 是 canonical JSON 而非 YAML 源文件，重走
//     compose.Load 无输入可用；且回滚的执行依据是 desired_spec 本身。
func (e *Engine) preflightRollback(ctx context.Context, rec state.DeployRecord, specs []ServiceSpec) error {
	images := checkerImageSource{images: e.images}
	for i := range specs {
		spec := specs[i]
		// 4. compose 合法（关键执行面复核）。
		if !strings.HasPrefix(spec.Name, "fleetly-") || spec.Image == "" {
			return errorf("E_ROLLBACK_FAILED",
				"snapshot service %s fails key-surface validation (namespace/image reference missing): target revision is not replayable", spec.Name)
		}
		// 1. 镜像可得（v0.1 本机 inspect；缺失 → E_IMAGE_UNAVAILABLE +
		//    W_ROLLBACK_IMAGE_RISK）。
		if _, err := build.PreflightImage(ctx, images, spec.Image); err != nil {
			var ae *apperr.Error
			if asAppErr(err, &ae) && ae != nil {
				return ae
			}
			return errorf("E_RUNTIME_UNAVAILABLE", "image check failed %s: %v", spec.Image, err)
		}
	}
	// 3. secret 存在（挂载名 → app_secrets 现值解析；快照携带 secret 为
	//    E4 起的合法形态——验证替代原 v0.1 整体拒绝）。
	if err := e.ensureSnapshotSecrets(ctx, rec, specs); err != nil {
		return err
	}
	// 3b. config 存在（挂载名 → app_configs 现值解析；内容换版 → 名悬空 →
	//    E_CONFIG_NOT_FOUND——快照不可变纪律，OT-3 同款）。
	if err := e.ensureSnapshotConfigs(ctx, rec, specs); err != nil {
		return err
	}
	// 2. 约束可满足（放置前哨：绑定节点 ready / 卷归属一致；取当前绑定）。
	if err := e.resolver.Preflight(ctx, rec.AppID); err != nil {
		return err
	}
	return nil
}

// checkerImageSource 把引擎镜像可见性端口适配为 build.ImageSource
// （PreflightImage 只消费 InspectImage；LoadImage 属构建管线，回滚路径
// 不可达——桩实现显式报错防误用）。
type checkerImageSource struct {
	images ImageChecker
}

func (c checkerImageSource) InspectImage(ctx context.Context, ref string) (build.ImageInfo, error) {
	digest, err := c.images.ImageDigest(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrImageMissing) {
			return build.ImageInfo{}, fmt.Errorf("%w: %s", build.ErrImageNotFound, ref)
		}
		return build.ImageInfo{}, err
	}
	return build.ImageInfo{ID: digest}, nil
}

func (c checkerImageSource) LoadImage(context.Context, io.Reader) error {
	return errors.New("engine: LoadImage is not available on the rollback preflight path")
}

// failRollbackPreflight 回滚 preflight 失败（未触底座）：按原因码落
// failed + rollback_failed 事件（reason=preflight）。app 运行现场未被
// 改动——不判 critical、不关漂移收敛 opt-in。
func (e *Engine) failRollbackPreflight(ctx context.Context, rec state.DeployRecord, err error) error {
	ae := appErrOf(err, rec.ID)
	if ferr := e.failTransition(ctx, rec, ae.Code(), ae.Message()); ferr != nil {
		return ferr
	}
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		return appendEvents(ctx, tx, deploymentEvent("deployment.rollback_failed", rec.ID,
			"reason", "preflight", "code", ae.Code()))
	})
}

// failRollbackDeployment 回滚执行后失败（已动底座，D-REL-10 critical）：
// E_ROLLBACK_FAILED 终态（切流过则 verdict=unstable）+ 不再二次自动恢复
// （不重放、不归位——现场留人工）+ 该 app 漂移收敛 opt-in 强制关闭直至
// 人工重置 + rollback_failed 事件与审计（cause 码随 payload）。
func (e *Engine) failRollbackDeployment(ctx context.Context, rec state.DeployRecord, causeCode, detail string) error {
	code := "E_ROLLBACK_FAILED"
	msg := fmt.Sprintf("%s; rollback failed (critical, no second automatic recovery): %s", detail, causeCode)
	patch := state.DeploymentPatch{ErrorCode: &code}
	if !rec.FirstHealthyAt.IsZero() {
		verdict := state.VerdictUnstable
		patch.Verdict = &verdict // 切流过：app=degraded（旧版本未接住）
	}
	// 终态写与 failed/rollback_failed 事件同一事务（单写点，T0-V2.2；CAS
	// 落败整体回滚——事件与审计不先行）。
	err := e.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.EnterPhase(ctx, rec.ID, rec.Status, state.DeployFailed, patch,
			deploymentEvent("deployment.failed", rec.ID, "code", code, "detail", msg),
			deploymentEvent("deployment.rollback_failed", rec.ID,
				"reason", "replay", "code", causeCode)); err != nil {
			return err // 含 CAS 落败（ErrDeploymentStateTransition 家族）——整体回滚
		}
		rec.Status = state.DeployFailed
		if err := tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      "deployment.auto_abort",
			Target:      "deployment:" + rec.ID,
			Result:      "ok",
			ErrorCode:   code,
			DiffSummary: state.DiffSummary("app", rec.AppName, "kind", rec.Kind, "cause", causeCode), // MG-6：构造器替换手拼 JSON
		}); err != nil {
			return err
		}
		if err := auditDeployment(ctx, tx, "system", "deployment.rollback", rec.ID,
			"error", code, state.DiffSummary("cause", causeCode)); err != nil { // MG-6：构造器替换手拼 JSON
			return err
		}
		// 收敛 opt-in 强制关闭（critical 后只检测不收敛；人工经
		// `fleetly drift enable` 重置）。已关或 app 缺失则无副作用。
		on, gerr := tx.GetAppDriftConverge(ctx, rec.AppID)
		if gerr != nil && !errors.Is(gerr, state.ErrAppNotFound) {
			return gerr
		}
		if on {
			if err := tx.SetAppDriftConverge(ctx, rec.AppID, false); err != nil {
				return err
			}
			return tx.WriteAudit(ctx, state.AuditEntry{
				Actor:       "system",
				Action:      "reconcile.drift_converge_disabled",
				Target:      "app:" + rec.AppName,
				Result:      "ok",
				ErrorCode:   code,
				DiffSummary: state.DiffSummary("reason", "rollback_failed", "deployment", rec.ID), // MG-6：构造器替换手拼 JSON
			})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, state.ErrDeploymentStateTransition) {
			return nil // 已被并发推进（重启恢复/CLI 竞争）：终态不可逆（事务回滚，无审计/事件残留）
		}
		return err
	}
	return e.refreshDerivedState(ctx, rec.AppID, rec.AppName)
}
