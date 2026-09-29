package database

// 按态收敛分支（managed-databases §2.1 转移表 / §2.3 操作表的收敛器侧）：
//
//	convergeProvisioning — 网络 → 选点钉住 → 卷登记 → 引擎凭据 secret →
//	                       服务幂等收敛 → 健康门（ready / failed）
//	watchHealthy        — 在役漂移复检 + 健康观察（ready↔degraded）
//	convergePaused      — scale-0 保全（suspend 的收敛面，resume 由 API
//	                       转回 provisioning 后走 convergeProvisioning）
//	reapDeleting        — 幂等清理（服务/secret/卷按选择/网络）→ deleted
//
// 失败纪律：单实例单步失败 = 本拍让位、下一拍重走（幂等）；转移失败
// （CAS 冲突 = 并发已推进）静默让位——下一拍按新状态分派。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// serviceSpec 是 swarm.ServiceSpec 的本包别名（端口消费面类型不出包——
// 翻译点在 spec.go，本文件只传递）。
type serviceSpec = swarm.ServiceSpec

// databaseEventPayloadMarshal 是事件载荷的序列化出口（JSON；失败退 "{}"）。
func databaseEventPayloadMarshal(payload map[string]string) []byte {
	raw, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// pgSecretKey 是 PG 引擎凭据 secret 的模板内声明名（dbtemplate 渲染的
// secret 名成分——DBSecretName(name, key, hash8) 的 key 位）。
const pgSecretKey = "password"

// convergeProvisioning 走完整收敛链（创建/resume/retry 共用——provisioning
// 兼作重收敛态，§2.1）。链上任一步失败：记录 last_error（诊断面）但**不**
// 转移——瞬态错误（底座抖动）由下一拍重走；只有健康门级失败（任务硬失败/
// 超时，failProvisioning）才落 failed。last_error 在每次进入收敛时先清空
// 前值再前进——列语义 = 「当前失败现场」，收敛中 = 无失败现场。
func (m *Manager) convergeProvisioning(ctx context.Context, inst *state.DatabaseInstance) {
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		// 模板在注册表消失 = 平台降级装配错误（模板只增纪律下理论不可达
		// ——实例名占用期模板被移除的回退窗）。如实落 failed：唯一可自查
		// 的诚实出口。
		m.failProvisioning(ctx, inst, fmt.Sprintf("template %q is not in the platform registry (platform regression)", inst.Template))
		return
	}
	password, err := m.decryptCredential(inst)
	if err != nil {
		m.log.Warn("database: provision deferred (credential undecryptable)", "instance", inst.Name, "error", err)
		return
	}

	// ① 共享网络（幂等创建，managed label；库服务与引用方 app 都挂它）。
	netName, err := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
	if err != nil {
		m.log.Warn("database: provision deferred", "instance", inst.Name, "error", err)
		return
	}
	if err := m.docker.NetworkEnsure(ctx, netName, true); err != nil {
		m.log.Warn("database: provision deferred (network)", "instance", inst.Name, "error", err)
		return
	}

	// ② 放置：既有绑定保持；未绑定选点 + 落绑定（数据诞生点钉住同款）。
	if err := m.ensurePlacement(ctx, inst); err != nil {
		m.log.Warn("database: provision deferred (placement)", "instance", inst.Name, "error", err)
		return
	}

	// ③ 数据卷登记 + 底座卷确保（数据诞生点：登记即事实——volume.created
	// 事件随首次登记发出）。
	if _, err := m.ensureVolume(ctx, inst, tpl.VolumeKey, tpl.VolumeMountPath); err != nil {
		m.log.Warn("database: provision deferred (volume)", "instance", inst.Name, "error", err)
		return
	}

	// ④ 引擎凭据 secret（PG：secret 文件投递——服务创建前在位；Redis 无
	// secret，spec 启动参数投递）。
	secretIDs := map[string]string{}
	if tpl.CredentialDelivery == dbtemplate.CredentialSecretFile {
		secretName, err := naming.DBSecretName(inst.TeamSlug, inst.ProjectSlug, inst.Name, pgSecretKey, naming.Hash8(password))
		if err != nil {
			m.log.Warn("database: provision deferred", "instance", inst.Name, "error", err)
			return
		}
		id, err := m.ensureCredentialSecret(ctx, inst.QualifiedName(), secretName, password)
		if err != nil {
			m.log.Warn("database: provision deferred (credential secret)", "instance", inst.Name, "error", err)
			return
		}
		secretIDs[secretName] = id
		// 旧值 secret 清场（轮换后名字更替——best-effort：in-use 由下一拍
		// 消化；材料不残留）。
		if err := m.removeStaleCredentialSecrets(ctx, inst.QualifiedName(), secretName); err != nil {
			m.log.Warn("database: stale credential secret cleanup deferred", "instance", inst.Name, "error", err)
		}
	}

	// ⑤ 服务幂等收敛（渲染 → 翻译 → 缺失创建/哈希漂移更新）。
	desired, err := renderService(inst, password, 1)
	if err != nil {
		m.failProvisioning(ctx, inst, "template render failed: "+err.Error())
		return
	}
	swarmSpec, err := buildServiceSpec(desired, inst.QualifiedName(), secretIDs)
	if err != nil {
		m.failProvisioning(ctx, inst, "service spec build failed: "+err.Error())
		return
	}
	if err := m.ensureService(ctx, desired.Name, swarmSpec, desired.DesiredHash(), 1); err != nil {
		m.log.Warn("database: provision deferred (service)", "instance", inst.Name, "error", err)
		return
	}

	// ⑥ 健康门：healthy → ready；任务硬失败 → failed；超预算 → failed。
	switch verdict, reason := m.healthVerdict(ctx, desired.Name, desired.Image); verdict {
	case healthHealthy:
		m.markReady(ctx, inst)
	case healthFailing:
		m.failProvisioning(ctx, inst, reason)
	default: // healthPending：探测窗内/启动中——留在 provisioning 等待
		if reason := m.provisionTimedOut(inst); reason != "" {
			m.failProvisioning(ctx, inst, reason)
		}
	}
}

// ensurePlacement 选点与绑定落库（未绑定时：选点 → SetDatabaseNodeBinding；
// 幂等——绑定非空即保持，v0.2 无任何换点路径）。
func (m *Manager) ensurePlacement(ctx context.Context, inst *state.DatabaseInstance) error {
	if m.placement == nil {
		return errors.New("database: placement selector not assembled")
	}
	d, err := m.placement.ResolveDatabase(ctx, DatabaseInput{
		InstanceID:      inst.ID,
		InstanceName:    inst.Name,
		ExistingBinding: inst.PlatformNodeID,
	})
	if err != nil {
		return err
	}
	if !d.KeptExisting {
		if err := m.store.SetDatabaseNodeBinding(ctx, inst.ID, d.PlatformNodeID); err != nil {
			return fmt.Errorf("database: persist node binding of %s: %w", inst.Name, err)
		}
		inst.PlatformNodeID = d.PlatformNodeID
		m.log.Info("database: node binding pinned (data gravity placement)",
			"instance", inst.Name, "node", d.PlatformNodeID)
	}
	return nil
}

// ensureVolume 数据卷登记（owner=database 的卷注册表泛化行）+ 底座卷确保。
// 首次登记发 volume.created 事件（数据诞生点）。幂等：重复登记以最新事实
// 覆盖（placement 行同款）。返回卷 docker 名（reap 的按名删除路径消费）。
func (m *Manager) ensureVolume(ctx context.Context, inst *state.DatabaseInstance, key, mountPath string) (string, error) {
	volName, err := naming.DBVolumeName(inst.Name, key, inst.ID)
	if err != nil {
		return "", err
	}
	if err := m.docker.VolumeEnsure(ctx, volName); err != nil {
		return "", err
	}
	vol, isNew, err := m.store.RegisterVolume(ctx, state.VolumeWrite{
		OwnerKind:      state.VolumeOwnerDatabase,
		OwnerID:        inst.ID,
		Key:            key,
		Name:           volName,
		Kind:           state.VolumeKindNamed,
		PlatformNodeID: inst.PlatformNodeID,
		MountPath:      mountPath,
	})
	if err != nil {
		return "", fmt.Errorf("database: register data volume of %s: %w", inst.Name, err)
	}
	if isNew {
		m.emitEvent(ctx, "volume.created", "volume:"+vol.Name, map[string]string{
			"owner_kind": string(state.VolumeOwnerDatabase),
			"owner_id":   inst.ID,
			"key":        key,
			"node":       inst.PlatformNodeID,
		})
	}
	return vol.Name, nil
}

// ensureCredentialSecret 幂等创建引擎凭据 swarm secret（名内嵌值指纹 → 值
// 变化即新名，「存在性」判据即幂等；rustfs 同口径）。明文只进创建载荷；
// secret 带 fleetly.db 归属 label（reap 清场与旧值清场的选择器）。
func (m *Manager) ensureCredentialSecret(ctx context.Context, instanceName, secretName, password string) (string, error) {
	id, exists, err := m.docker.SecretInspect(ctx, secretName)
	if err != nil {
		return "", err
	}
	if exists {
		return id, nil
	}
	return m.docker.SecretCreate(ctx, secretSpecOf(instanceName, secretName, password))
}

// removeStaleCredentialSecrets 清场本实例的全部凭据 secret（fleetly.db
// label 选择），保留 keep（当前期望引用）。轮换后旧值 secret 的 best-effort
// 清理——in-use（服务引用未释放）如实报错由调用方降级日志。
func (m *Manager) removeStaleCredentialSecrets(ctx context.Context, instanceName, keep string) error {
	names, err := m.docker.SecretList(ctx, map[string]string{state.LabelDatabase: instanceName})
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == keep {
			continue
		}
		if err := m.docker.SecretRemove(ctx, name); err != nil {
			return err
		}
		m.log.Info("database: stale credential secret removed (credential rotation)", "instance", instanceName)
	}
	return nil
}

// ensureService 服务幂等收敛：缺失创建；期望投影哈希（label 判据）或副本
// 水位与实况不一致时更新。哈希判据覆盖限额变更/升级/凭据轮换/暂停伸缩；
// 副本判据补齐 out-of-band `docker service scale` 的漂移（spec 突变不改
// label——paused 的 scale-0 语义必须由收敛器保持，这是唯一 externally
// mutable 的水位面。其余字段的漂移投影面 v0.2 S2 不设，挂账遗留）。
func (m *Manager) ensureService(ctx context.Context, name string, spec serviceSpec, desiredHash string, desiredReplicas uint64) error {
	cur, err := m.docker.ServiceInspect(ctx, name)
	if err != nil {
		return err
	}
	if !cur.Exists {
		if err := m.docker.ServiceCreate(ctx, spec); err != nil {
			return err
		}
		m.log.Info("database: managed service created",
			"service", name, "desired_hash", desiredHash)
		return nil
	}
	if cur.Labels[state.LabelDesiredHash] != desiredHash || cur.Replicas != desiredReplicas {
		if err := m.docker.ServiceUpdate(ctx, name, cur.Version, spec); err != nil {
			return err
		}
		m.log.Info("database: managed service updated to desired spec (settings change, upgrade, rotation or drift)",
			"service", name, "desired_hash", desiredHash)
	}
	return nil
}

// taskRunning 报告任务是否期望 running（当前代——旧代任务 desired=shutdown
// 不计；共享观测 dutydocker.TaskObservation 无方法面，判定以小函数内聚——
// 原本包 TaskObservation.running 的语义逐字迁址）。
func taskRunning(t dutydocker.TaskObservation) bool {
	return t.DesiredState == string(swarm.TaskStateRunning)
}

// healthVerdict 是任务健康判定（健康门与在役观察共用的唯一真值源）。判定
// 信号 = 目标镜像的任务状态——本 API 代的 swarm 任务对象不携带容器健康位，
// 模板 healthcheck 的引擎级判定经 swarm 原生闭环落到任务：探测连续失败耗
// 尽 retries → 任务 failed + Err（与引擎应用健康门同源的观察纪律）。
//
//	healthHealthy — 存在 desired=running 且 running 且镜像匹配的任务
//	healthFailing — 存在 desired=running 且 failed/rejected 的目标版本任务
//	                （引擎级硬失败——镜像不可得/启动退出/健康门耗尽；reason
//	                取任务 Err）
//	healthPending — 其余（启动中/准备中/无任务——等待，不抢跑）
func (m *Manager) healthVerdict(ctx context.Context, service, image string) (healthState, string) {
	tasks, err := m.docker.TaskList(ctx, service)
	if err != nil {
		return healthPending, ""
	}
	for _, t := range tasks {
		if !taskRunning(t) || (image != "" && t.Image != image) {
			continue
		}
		switch t.State {
		case "rejected", "failed":
			reason := strings.TrimSpace(t.Err)
			if reason == "" {
				reason = "task state " + t.State
			}
			return healthFailing, singleLine(reason)
		case "running":
			return healthHealthy, ""
		}
	}
	return healthPending, ""
}

// healthState 是健康判定词表。
type healthState int

const (
	healthPending healthState = iota
	healthHealthy
	healthFailing
)

// provisionTimedOut 报告 provisioning 是否已超健康门预算（返回非空 = 超时
// 原因；首见时刻在进程内记账——重启后重记，只损失一次起点，不产生错误
// 转移：超时判定顺延）。
func (m *Manager) provisionTimedOut(inst *state.DatabaseInstance) string {
	since, ok := m.provisioningSince[inst.ID]
	if !ok {
		m.provisioningSince[inst.ID] = m.now()
		return ""
	}
	if elapsed := m.now().Sub(since); elapsed > m.cfg.ProvisionTimeout {
		return fmt.Sprintf("health gate timeout after %s (no healthy task; scene preserved — retry to reconverge)", elapsed.Truncate(time.Second))
	}
	return ""
}

// markReady 健康门通过：provisioning → ready（db.ready）+ 清失败现场，
// 同事务原子。
func (m *Manager) markReady(ctx context.Context, inst *state.DatabaseInstance) {
	err := m.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.SetDatabaseLastError(ctx, inst.ID, ""); err != nil {
			return err
		}
		return tx.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseReady,
			databaseEvent("db.ready", inst))
	})
	switch {
	case err == nil:
		m.log.Info("database: instance ready (health gate passed)", "instance", inst.Name)
		inst.State = state.DatabaseReady
	case errIsNotFound(err) || errors.Is(err, state.ErrDatabaseStateConflict):
		// 并发已推进（retry/delete 竞态）：静默让位，下一拍按新状态分派。
	default:
		m.log.Warn("database: enter ready failed", "instance", inst.Name, "error", err)
	}
}

// failProvisioning 收敛彻底失败：last_error 落诊断 + provisioning → failed
// （db.provision_failed），同事务原子——「保留现场（服务与卷不删）」由
// 收敛链只增不删的构造满足。
func (m *Manager) failProvisioning(ctx context.Context, inst *state.DatabaseInstance, reason string) {
	err := m.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.SetDatabaseLastError(ctx, inst.ID, reason); err != nil {
			return err
		}
		return tx.EnterDbPhase(ctx, inst.ID, state.DatabaseProvisioning, state.DatabaseFailed,
			databaseEvent("db.provision_failed", inst, "reason", reason))
	})
	switch {
	case err == nil:
		m.log.Warn("database: provisioning failed (scene preserved; retry to reconverge)",
			"instance", inst.Name, "reason", reason)
		inst.State = state.DatabaseFailed
	case errIsNotFound(err) || errors.Is(err, state.ErrDatabaseStateConflict):
		// 并发已推进：静默让位。
	default:
		m.log.Warn("database: enter failed failed", "instance", inst.Name, "error", err)
	}
}

// watchHealthy 在役观察（ready/degraded）：服务幂等收敛（漂移修复/限额
// 生效）+ 健康观察——unhealthy 持续证据（swarm retries 耗尽的判定产物）
// → degraded；healthy → recovered。ready 期任务硬失败（failed/rejected）
// 也是 degraded 证据（swarm restart-policy 自愈中；若彻底失败，健康位终
// 将 unhealthy，观察不抢跑）。
func (m *Manager) watchHealthy(ctx context.Context, inst *state.DatabaseInstance) {
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		m.log.Warn("database: watch deferred (template missing)", "instance", inst.Name, "error", err)
		return
	}
	password, err := m.decryptCredential(inst)
	if err != nil {
		m.log.Warn("database: watch deferred (credential undecryptable)", "instance", inst.Name, "error", err)
		return
	}
	// 服务收敛（轮换感知，S4：在役期凭据可经 RotateCredentials 变更——
	// secret 引用随 desired-hash 比对自然换挂；换名后旧值 secret 由
	// removeStaleCredentialSecrets best-effort 清场，材料不残留）。
	secretIDs := map[string]string{}
	if tpl.CredentialDelivery == dbtemplate.CredentialSecretFile {
		secretName, err := naming.DBSecretName(inst.TeamSlug, inst.ProjectSlug, inst.Name, pgSecretKey, naming.Hash8(password))
		if err != nil {
			m.log.Warn("database: watch deferred", "instance", inst.Name, "error", err)
			return
		}
		id, exists, err := m.docker.SecretInspect(ctx, secretName)
		if err != nil {
			m.log.Warn("database: watch deferred (credential secret)", "instance", inst.Name, "error", err)
			return
		}
		if !exists {
			// secret 缺失（外部清理）：先补齐再收敛服务——服务引用不能悬空。
			if id, err = m.ensureCredentialSecret(ctx, inst.QualifiedName(), secretName, password); err != nil {
				m.log.Warn("database: watch deferred (credential secret recreate)", "instance", inst.Name, "error", err)
				return
			}
		}
		secretIDs[secretName] = id
		// 旧值 secret 清场（轮换换名后——best-effort：in-use 如实报错降级
		// 日志，下一拍重走；与 provisioning 路径同款纪律）。
		if err := m.removeStaleCredentialSecrets(ctx, inst.QualifiedName(), secretName); err != nil {
			m.log.Warn("database: stale credential secret cleanup deferred", "instance", inst.Name, "error", err)
		}
	}
	desired, err := renderService(inst, password, 1)
	if err != nil {
		m.log.Warn("database: watch deferred (render)", "instance", inst.Name, "error", err)
		return
	}
	swarmSpec, err := buildServiceSpec(desired, inst.QualifiedName(), secretIDs)
	if err != nil {
		m.log.Warn("database: watch deferred (spec build)", "instance", inst.Name, "error", err)
		return
	}
	if err := m.ensureService(ctx, desired.Name, swarmSpec, desired.DesiredHash(), 1); err != nil {
		m.log.Warn("database: watch deferred (service)", "instance", inst.Name, "error", err)
		return
	}

	verdict, _ := m.healthVerdict(ctx, desired.Name, desired.Image)
	switch {
	case verdict == healthFailing && inst.State == state.DatabaseReady:
		m.enterDegraded(ctx, inst)
	case verdict == healthHealthy && inst.State == state.DatabaseDegraded:
		m.enterRecovered(ctx, inst)
	}
}

// enterDegraded 在役不健康：ready → degraded（db.degraded——swarm 自愈观
// 察中，无平台收敛动作失败）。
func (m *Manager) enterDegraded(ctx context.Context, inst *state.DatabaseInstance) {
	err := m.store.EnterDbPhase(ctx, inst.ID, state.DatabaseReady, state.DatabaseDegraded,
		databaseEvent("db.degraded", inst))
	switch {
	case err == nil:
		m.log.Warn("database: instance degraded (health probe failing; swarm self-healing observed)", "instance", inst.Name)
		inst.State = state.DatabaseDegraded
	case errIsNotFound(err) || errors.Is(err, state.ErrDatabaseStateConflict):
	default:
		m.log.Warn("database: enter degraded failed", "instance", inst.Name, "error", err)
	}
}

// enterRecovered 在役恢复：degraded → ready（db.recovered）+ 清失败现场
// （degraded 期 last_error 已是空——保持空写无害，语义对齐「恢复即无现场」）。
func (m *Manager) enterRecovered(ctx context.Context, inst *state.DatabaseInstance) {
	err := m.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.SetDatabaseLastError(ctx, inst.ID, ""); err != nil {
			return err
		}
		return tx.EnterDbPhase(ctx, inst.ID, state.DatabaseDegraded, state.DatabaseReady,
			databaseEvent("db.recovered", inst))
	})
	switch {
	case err == nil:
		m.log.Info("database: instance recovered (health probe passing again)", "instance", inst.Name)
		inst.State = state.DatabaseReady
	case errIsNotFound(err) || errors.Is(err, state.ErrDatabaseStateConflict):
	default:
		m.log.Warn("database: enter recovered failed", "instance", inst.Name, "error", err)
	}
}

// convergePaused 暂停保全：渲染副本 0 的期望投影幂等收敛（suspend 的收敛
// 面——API 只转状态，scale-0 由本分支落地与保持；外部 scale 回 1 的漂移
// 被哈希判据纠回）。健康观察不做（无任务可观察）。
func (m *Manager) convergePaused(ctx context.Context, inst *state.DatabaseInstance) {
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		m.log.Warn("database: pause converge deferred (template missing)", "instance", inst.Name, "error", err)
		return
	}
	password, err := m.decryptCredential(inst)
	if err != nil {
		m.log.Warn("database: pause converge deferred (credential undecryptable)", "instance", inst.Name, "error", err)
		return
	}
	secretIDs := map[string]string{}
	if tpl.CredentialDelivery == dbtemplate.CredentialSecretFile {
		secretName, err := naming.DBSecretName(inst.TeamSlug, inst.ProjectSlug, inst.Name, pgSecretKey, naming.Hash8(password))
		if err != nil {
			return
		}
		id, exists, err := m.docker.SecretInspect(ctx, secretName)
		if err != nil {
			m.log.Warn("database: pause converge deferred (credential secret)", "instance", inst.Name, "error", err)
			return
		}
		if !exists {
			if id, err = m.ensureCredentialSecret(ctx, inst.QualifiedName(), secretName, password); err != nil {
				m.log.Warn("database: pause converge deferred (credential secret recreate)", "instance", inst.Name, "error", err)
				return
			}
		}
		secretIDs[secretName] = id
	}
	desired, err := renderService(inst, password, 0)
	if err != nil {
		m.log.Warn("database: pause converge deferred (render)", "instance", inst.Name, "error", err)
		return
	}
	swarmSpec, err := buildServiceSpec(desired, inst.QualifiedName(), secretIDs)
	if err != nil {
		m.log.Warn("database: pause converge deferred (spec build)", "instance", inst.Name, "error", err)
		return
	}
	if err := m.ensureService(ctx, desired.Name, swarmSpec, desired.DesiredHash(), 0); err != nil {
		m.log.Warn("database: pause converge deferred (service)", "instance", inst.Name, "error", err)
	}
}

// reapDeleting tombstone 第二拍（幂等重试直至完成；deleting 无失败出边）：
// ① 服务移除并确认消失 → ② 凭据 secret 清场 → ③ 卷按选择处置（默认保留
// 转 orphaned；显式 delete_volumes 删卷并记 discarded）→ ④ 共享网络移除
// → ⑤ deleting → deleted（db.deleted）+ 系统审计。任一步失败保持 deleting
// ——下一拍从该步重走（各步幂等）。
func (m *Manager) reapDeleting(ctx context.Context, inst *state.DatabaseInstance) {
	// ① 服务移除（幂等）+ 消失确认（删除有传播延迟——未消失本拍收场）。
	svcName, err := naming.DBServiceName(inst.TeamSlug, inst.ProjectSlug, inst.Name, serviceSuffixOf(inst.Template))
	if err != nil {
		m.log.Warn("database: reap deferred", "instance", inst.Name, "error", err)
		return
	}
	if err := m.docker.ServiceRemove(ctx, svcName); err != nil {
		m.log.Warn("database: reap deferred (service remove)", "instance", inst.Name, "error", err)
		return
	}
	if cur, err := m.docker.ServiceInspect(ctx, svcName); err != nil {
		m.log.Warn("database: reap deferred (service confirm)", "instance", inst.Name, "error", err)
		return
	} else if cur.Exists {
		return // 删除传播中：下一拍复认
	}

	// ② 凭据 secret 清场（label 选择——全部历史值；in-use 已随服务消失
	// 解除，残余竞态下一拍重走）。
	if err := m.removeStaleCredentialSecrets(ctx, inst.QualifiedName(), ""); err != nil {
		m.log.Warn("database: reap deferred (secret cleanup)", "instance", inst.Name, "error", err)
		return
	}

	// ③ 卷按选择处置（数据安全默认：保留转 orphaned——平台永不随状态机
	// 自动删数据；显式 delete_volumes 才删）。
	if err := m.disposeVolumes(ctx, inst); err != nil {
		m.log.Warn("database: reap deferred (volumes)", "instance", inst.Name, "error", err)
		return
	}

	// ④ 共享网络移除（引用方端点未释放时 in-use——下一拍重试直至清场；
	// 引用守卫保证受理时无引用行，挂接残余只来自在途部署窗口）。
	netName, err := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
	if err != nil {
		m.log.Warn("database: reap deferred", "instance", inst.Name, "error", err)
		return
	}
	if err := m.docker.NetworkRemove(ctx, netName); err != nil {
		m.log.Warn("database: reap deferred (network)", "instance", inst.Name, "error", err)
		return
	}

	// ⑤ 终态：deleting → deleted + 系统审计（自动动作必入审计）。
	err = m.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.EnterDbPhase(ctx, inst.ID, state.DatabaseDeleting, state.DatabaseDeleted,
			databaseEvent("db.deleted", inst)); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       "system",
			Action:      "db.delete",
			Target:      "database:" + inst.Name,
			Result:      "ok",
			DiffSummary: state.DiffSummary("phase", "deleted", "reaped", true),
		})
	})
	switch {
	case err == nil:
		m.log.Info("database: instance deleted (managed objects reaped)", "instance", inst.Name)
		inst.State = state.DatabaseDeleted
	case errIsNotFound(err) || errors.Is(err, state.ErrDatabaseStateConflict):
		// 并发已推进：终态已落，幂等收敛。
	default:
		m.log.Warn("database: mark deleted failed", "instance", inst.Name, "error", err)
	}
}

// disposeVolumes 卷处置（reap 第③步）：delete_volumes=false（默认）→ 全部
// active 卷置 orphaned（保留可发现）；true → 底座卷移除 + 注册表记
// discarded（显式丢弃；数据不可逆删除是用户显式选择）。
func (m *Manager) disposeVolumes(ctx context.Context, inst *state.DatabaseInstance) error {
	if inst.DeleteVolumes {
		vols, err := m.store.ListOwnerVolumes(ctx, state.VolumeOwnerDatabase, inst.ID)
		if err != nil {
			return err
		}
		for _, v := range vols {
			if v.Status == state.VolumeDiscarded {
				continue
			}
			if err := m.docker.VolumeRemove(ctx, v.Name); err != nil {
				return err
			}
		}
		return m.markVolumesDiscarded(ctx, inst.ID, inst.Name)
	}
	_, err := m.store.MarkOwnerVolumesOrphaned(ctx, state.VolumeOwnerDatabase, inst.ID)
	return err
}

// serviceSuffixOf 取模板服务名（reap 的服务名重构——与渲染层同一公式尾段；
// 模板缺失时回退极小概率误名 → 服务 inspect 确认存在性兜底）。模板 ID 与
// 服务名在注册表内一一对应；此处内表化避免 reap 依赖模板渲染全链。
func serviceSuffixOf(templateID string) string {
	if tpl, err := dbtemplate.Get(templateID); err == nil {
		return tpl.ServiceName
	}
	return templateID
}

// singleLine 归一任务错误文本为单行（last_error 诊断列的单行化纪律；
// 长度截断防任务错误文本失控膨胀——列是摘要不是日志）。
func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// databaseEvent 构造库生命周期事件的载荷（subject = database:<名>；payload 只
// 带事实字段——reason 是引擎/任务层文本，凭据材料零出现）。
func databaseEvent(name string, inst *state.DatabaseInstance, extra ...string) state.Event {
	payload := map[string]string{
		"instance": inst.Name,
		"template": inst.Template,
	}
	for i := 0; i+1 < len(extra); i += 2 {
		payload[extra[i]] = extra[i+1]
	}
	return state.Event{Name: name, Subject: "database:" + inst.Name, Payload: string(databaseEventPayloadMarshal(payload))}
}

// markVolumesDiscarded 把实例全部卷记 discarded（显式 delete_volumes 的
// 台账面——底座卷已在 disposeVolumes 移除）。系统审计随写（显式丢弃 =
// 数据安全动作）。
func (m *Manager) markVolumesDiscarded(ctx context.Context, ownerID, instanceName string) error {
	err := m.store.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.MarkOwnerVolumesDiscarded(ctx, state.VolumeOwnerDatabase, ownerID)
		return err
	})
	if err != nil {
		return err
	}
	return m.writeAudit(ctx, "volume.discarded", "database:"+instanceName,
		string(state.DiffSummary("owner_kind", string(state.VolumeOwnerDatabase), "owner_id", ownerID)))
}
