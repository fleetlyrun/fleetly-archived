package database

// 凭据轮换编排（managed-databases §2.5，D-DB-3：仅手动 + 引用 app 自动重
// 部署；编排在本包 Manager 而非 api——引擎侧动作（一次性 job / 收敛触发）
// 与底座紧邻，api 只做受理/守卫/审计）。
//
// 流程按引擎：
//   - PG：一次性容器 job（库共享网络内 psql -h <实例别名> 以**旧**密码认证，
//     执行 ALTER USER ... WITH PASSWORD '<新>'——密码字符集 [a-zA-Z0-9]
//     （GeneratePassword），单引号字面量内无引号/转义/注入风险，字面拼接
//     是该字符集下的安全形态）。ALTER 成功后才 CAS 落库新密文——顺序保证
//     「失败即未变」：job 失败时权威态与服务器侧都还是旧密码，重试天然
//     幂等。落库 CAS 落败（并发轮换）→ E_STATE_VERSION_CONFLICT 族哨兵。
//     **PG 暂停期拒绝**（诚实边界，§2.3 操作表「paused 合法前置态」的引擎
//     级限定）：initdb 只在首启读 POSTGRES_PASSWORD_FILE，暂停实例无法
//     ALTER USER——如实报 ErrPGRotationPaused 提示先 resume，不做「接受
//     受理、resume 后悄悄不生效」的假成功。
//   - Redis：无引擎侧动作（凭据 = spec 启动参数）——CAS 落库新密文后由收
//     敛 duty 下一拍按 desired-hash 差异重建任务（运行中）或仅更新 spec
//     （paused，resume 时以新密码重建）。
//   - MySQL（v0.3 W4，D-W4-3）：同 PG 原语——一次性容器 job 以旧密码认证
//     （MYSQL_PWD env）、`ALTER USER 'fleetly'@'%'`（官方镜像建 USER@'%'
//     的 host 段逐字）；暂停期同拒（mysqld 停摆无法 ALTER，ErrPGRotation
//     Paused 共用哨兵）。
//   - MongoDB（v0.3 W4，D-W4-3）：一次性容器 job 以旧密码认证
//     （MONGO_PASSWORD env 经 ${MONGO_PASSWORD} 引用展开进 URI——mongosh
//     无原生凭据 env）、`db.updateUser`（admin 库——官方入口 root 恒建于
//     admin，设计 §8.1 注记）；暂停期同拒。
//
// 引擎侧成功后（PG job exit 0 / Redis 密文落库）：
//  1. 重物化引用方 system env 行（FLEETLY_DB_<NAME>_*，值回 pending）——
//     与 engine dbinject 的部署期物化是**双写者同形**边界：行形状
//     （dbtemplate.ConnectionVars 键集 + age 密文 + source=system）两边
//     逐字一致，本包直连 state + dbtemplate 完成（不 import engine——
//     engine 不依赖 database，database 消费 engine 投影但物化逻辑只需
//     模板层）；部署期物化仍归 engine（S3 真源），本路径只覆盖「轮换到
//     重部署完成」的窗口。
//  2. 自动重部署全部引用 app（各自走正常部署队列——enqueue = 复用上一个
//     succeeded 部署的 compose 持久化路径与 spec_hash，引擎按常规发布语义
//     消费 pending 行；不重部署 = 旧密码失效即断连，自动化是唯一诚实选项，
//     D-DB-3）。
//  3. db.credentials_rotated 事件（载荷只带事实字段）。
//
// 中途失败的诚实口径：RotationStageError 携带已完成阶段上下文 → api 映射
// E_DB_ROTATE_FAILED（500）——人工收尾路径见各阶段注释（凭据永远可用
// 「databases reveal」对账权威态与服务器侧的一致性）。
//
// 明文纪律：新旧密码只存活于「生成/解密 → ALTER 载荷 / 加密落库 / 物化
// 加密」的内存链；日志/事件/审计/错误文本零出现。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ErrPGRotationPaused 表示库实例暂停期轮换被拒（PG/MySQL/Mongo 共用——
// 三引擎都需要运行中实例执行引擎侧改密语句；v0.3 W4 起 MySQL/Mongo 轮换
// 沿用同一哨兵，名字保留首发 PG 的历史锚；api 经 errors.Is 映射 409 族并
// 附「先 resume 再轮换」指引）。消息措辞引擎中立（哨兵单值，不给 MySQL/
// Mongo 回报 postgres 措辞的错位文本）。
var ErrPGRotationPaused = errors.New("database engine requires a running instance to rotate credentials (resume the database, then rotate)")

// ErrRotationConflict 表示轮换落库的乐观 CAS 落败（并发轮换已推进——
// 同族乐观冲突语义，D-DB-8；api 映射 E_STATE_VERSION_CONFLICT）。
var ErrRotationConflict = errors.New("database credentials were rotated concurrently (CAS mismatch)")

// RotationPrestateError 是轮换前置态违规（§2.3 操作表：ready/degraded/
// paused——api 映射 E_STATE_VERSION_CONFLICT 并附 current_state 与合法
// 前置态清单）。
type RotationPrestateError struct {
	Current string
}

func (e *RotationPrestateError) Error() string {
	return fmt.Sprintf("rotation requires state ready, degraded or paused (current: %s)", e.Current)
}

// RotationStageError 是轮换中途失败（E_DB_ROTATE_FAILED 的映射源）：Stage
// 是已完成阶段锚（engine/persist/materialize/redeploy），Err 不含凭据材料。
type RotationStageError struct {
	Stage string
	Err   error
}

func (e *RotationStageError) Error() string {
	if e.Err == nil {
		return "rotation failed at stage " + e.Stage
	}
	return fmt.Sprintf("rotation failed at stage %s: %v", e.Stage, e.Err)
}

func (e *RotationStageError) Unwrap() error { return e.Err }

// RotateCredentials 执行一次完整轮换编排（见文件头）。实例不存在 /
// deleting/deleted → state.ErrDatabaseNotFound（api 映射 E_DB_NOT_FOUND）。
// 返回重部署的引用 app 名单（响应面与审计 diff 消费）。
func (m *Manager) RotateCredentials(ctx context.Context, name string) ([]string, error) {
	inst, err := m.store.GetDatabaseInstanceByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if inst.State == state.DatabaseDeleting || inst.State == state.DatabaseDeleted {
		return nil, fmt.Errorf("%w: %s", state.ErrDatabaseNotFound, inst.Name)
	}
	// 前置态前哨（§2.3 操作表：ready/degraded/paused；provisioning 是收敛
	// 在途、failed 无健康引擎可改——表外组合显式 409 族拒绝）。
	switch inst.State {
	case state.DatabaseReady, state.DatabaseDegraded, state.DatabasePaused:
	default:
		return nil, &RotationPrestateError{Current: string(inst.State)}
	}
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		// 模板在册期被移除 = 平台降级装配错误（理论不可达）。
		return nil, &RotationStageError{Stage: "template", Err: fmt.Errorf("template %q is not in the platform registry", inst.Template)}
	}
	old, err := m.decryptCredential(&inst)
	if err != nil {
		return nil, &RotationStageError{Stage: "decrypt", Err: errors.New("stored credential is undecryptable (master key mismatch or corrupted ciphertext)")}
	}
	new, err := dbtemplate.GeneratePassword()
	if err != nil {
		return nil, &RotationStageError{Stage: "generate", Err: err}
	}
	cipher, err := m.box.Encrypt([]byte(new))
	if err != nil {
		return nil, &RotationStageError{Stage: "encrypt", Err: err}
	}

	// ── 引擎侧（查 engineTools 表分派——IMPL-DB-0 起轴 = tpl.Engine，同族
	// 发行版/大版本共享；IMPL-ARCH-F F-3 起 switch 收拢为表）──
	tools, err := engineToolsFor(tpl)
	if err != nil {
		return nil, &RotationStageError{Stage: "template", Err: err}
	}
	// 暂停拒绝（引擎级边界，§2.3 操作表「paused 合法前置态」的引擎级限定）：
	// 引擎侧改密语句（ALTER USER / updateUser）需要运行中实例——三个有引擎
	// 侧动作的引擎（PG/MySQL/Mongo）同谓词拒绝（initdb 只在首启读
	// POSTGRES_PASSWORD_FILE / mysqld 停摆 / mongod 停摆，机理见各自 rotate
	// 实现），诚实 409 族提示先 resume，不受理假轮换。Redis 无引擎侧动作
	//（rotate 为 nil），暂停期轮换合法（密文落库，resume 时以新密码重建）。
	if tools.rotate != nil {
		if inst.State == state.DatabasePaused {
			return nil, ErrPGRotationPaused
		}
		if err := tools.rotate(m, ctx, &inst, tpl.Image, old, new); err != nil {
			return nil, &RotationStageError{Stage: "engine", Err: err}
		}
	}

	// ── 权威态切换（CAS on credential_updated_at——并发轮换第二笔落败）──
	// PG 顺序注记：ALTER 已成功、本步失败 → 服务器侧已是新密码而权威态仍旧
	// ——E_DB_ROTATE_FAILED(stage=persist) 如实暴露；人工收尾 = 以新密码
	// 手动 ALTER 回旧值或修复存储后重试（reveal 可对账两侧一致性）。
	updated, err := m.store.RotateDatabaseCredentialCAS(ctx, inst.ID, inst.CredentialUpdatedAt, string(cipher))
	if err != nil {
		return nil, &RotationStageError{Stage: "persist", Err: err}
	}
	if !updated {
		return nil, ErrRotationConflict
	}

	// 收敛提前拍（Redis 任务重建 / PG 新凭据 secret ensure + 旧 secret 清场
	// ——kick 只是提前，正确性由幂等收敛拍兜底）。
	m.Kick()

	// ── 重物化引用方 system env 行（值回 pending；双写者同形边界见文件头）──
	appIDs, err := m.rematerializeReferences(ctx, &inst, inst.Template, new)
	if err != nil {
		return nil, &RotationStageError{Stage: "materialize", Err: fmt.Errorf("engine updated, referencing app materialization failed (apps reconnect with old credentials until the next successful rotation; manual recovery: re-run rotate after restoring storage consistency) %v", err)}
	}

	// ── 引用 app 自动重部署（正常部署队列；compose 持久化路径复用上一个
	// succeeded 部署——引擎 preparing 重载并按 spec_hash 自校验）──
	redeployed, missing, err := m.redeployReferencingApps(ctx, &inst, appIDs)
	if err != nil {
		return nil, &RotationStageError{Stage: "redeploy", Err: fmt.Errorf("credentials rotated and materialized, but auto-redeploy enqueue failed (manually redeploy %s): %v", strings.Join(missing, ", "), err)}
	}

	m.emitEvent(ctx, "db.credentials_rotated", "database:"+inst.Name, map[string]string{
		"instance":        inst.Name,
		"template":        inst.Template,
		"redeployed_apps": strings.Join(redeployed, ","),
	})
	return redeployed, nil
}

// rotatePostgresCredential 引擎侧热轮换（PG）：一次性容器 job 在库共享
// 网络内以旧密码认证、ALTER USER 换新（库不重启，§2.5）。密码字符集
// [a-zA-Z0-9]（GeneratePassword）→ 单引号字面量内无引号 hazard，字面拼接
// 安全。退出码非 0 = 认证/执行失败（诊断面只有退出码——容器输出不采集，
// psql 错误文本可能回显语句材料，明文纪律优先；`docker logs` 已随容器
// 移除，重试即重放诊断）。
func (m *Manager) rotatePostgresCredential(ctx context.Context, inst *state.DatabaseInstance, image, old, new string) error {
	netName, err := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
	if err != nil {
		return err
	}
	// 前置：共享网络在位（幂等——paused 恢复窗口/外部清理后的自愈）。
	if err := m.docker.NetworkEnsure(ctx, netName); err != nil {
		return fmt.Errorf("rotation network ensure: %w", err)
	}
	// 作业名走 dbjob 前缀族（fleetly-dbjob-<instance>-rotate-<ulid8>，
	// IMPL-ARCH-F F-2）：轮换是一次性瞬时容器，名必须可按族识别——误落
	// fleetly-db-* 库服务族会被库服务消费面（IsDbServiceName 识别面）当
	// 长驻服务（今天是一次性 container 无事；明天做成 service 即被误清）。
	jobName, err := naming.DBJobName(inst.Name, "rotate", ulid.Make().String())
	if err != nil {
		return err
	}
	exit, err := m.docker.ContainerRun(ctx, ContainerRunInput{
		Name: jobName,
		// 容器名不含凭据材料；env 只带旧密码（认证面），新密码进命令字面量
		// ——两者都只进容器创建载荷（dockerPort 实现零日志）。
		Image: image,
		// -d postgres 显式指库（W4-S6 e2e 实测修正）：psql 缺 -d 时按用户名
		// 连库（fleetly），实例只有模板库与实例库——连接即「database does
		// not exist」退败（exit 2）。ALTER USER 是集群级操作，维护库执行。
		Cmd:     []string{"psql", "-h", inst.Name, "-U", "fleetly", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "ALTER USER fleetly WITH PASSWORD '" + new + "'"},
		Env:     []string{"PGPASSWORD=" + old},
		Network: netName,
	})
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("credential rotation job exited %d (authentication with the stored password failed, or the instance is unhealthy; verify with databases reveal and retry)", exit)
	}
	return nil
}

// rotateMySQLCredential 引擎侧热轮换（MySQL，v0.3 W4）：一次性容器 job 在
// 库共享网络内以旧密码认证、ALTER USER 换新（官方镜像建 USER@'%'——host
// 段逐字；D-W4-3 轮换原语）。密码字符集 [a-zA-Z0-9]（GeneratePassword）→
// 单引号字面量内无引号 hazard，字面拼接安全；旧密码经 MYSQL_PWD env
// （mysql 客户端原生消费）。退出码非 0 = 认证/执行失败（诊断面只有退出码
// ——容器输出不采集，明文纪律优先；PG 同款口径）。
func (m *Manager) rotateMySQLCredential(ctx context.Context, inst *state.DatabaseInstance, image, old, new string) error {
	netName, err := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
	if err != nil {
		return err
	}
	// 前置：共享网络在位（幂等——paused 恢复窗口/外部清理后的自愈）。
	if err := m.docker.NetworkEnsure(ctx, netName); err != nil {
		return fmt.Errorf("rotation network ensure: %w", err)
	}
	jobName, err := naming.DBJobName(inst.Name, "rotate", ulid.Make().String())
	if err != nil {
		return err
	}
	exit, err := m.docker.ContainerRun(ctx, ContainerRunInput{
		Name:  jobName,
		Image: image,
		// ALTER USER 不依赖缺省库——mysql 客户端无库连接即集群级操作面。
		Cmd:     []string{"mysql", "-h", inst.Name, "-u", "fleetly", "-e", "ALTER USER 'fleetly'@'%' IDENTIFIED BY '" + new + "'"},
		Env:     []string{"MYSQL_PWD=" + old},
		Network: netName,
	})
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("credential rotation job exited %d (authentication with the stored password failed, or the instance is unhealthy; verify with databases reveal and retry)", exit)
	}
	return nil
}

// rotateMongoCredential 引擎侧热轮换（MongoDB，v0.3 W4）：一次性容器 job
// 在库共享网络内以旧密码认证、db.updateUser 换新（D-W4-3 轮换原语）。
// 认证与更新同在 admin 库（官方入口把 initdb root 恒建于 admin——设计
// managed-databases §8.1 实现注记）；旧密码经 MONGO_PASSWORD env、以
// ${MONGO_PASSWORD} 引用展开进 URI（字面量不进命令词表——mongosh 无原生
// 凭据 env，该形态与备份 job 的 Mongo 消费同款）；新密码进 eval 字面量
// （[a-zA-Z0-9] 无引号 hazard）。退出码非 0 = 认证/执行失败（诊断面只有
// 退出码——明文纪律优先）。
func (m *Manager) rotateMongoCredential(ctx context.Context, inst *state.DatabaseInstance, image, old, new string) error {
	netName, err := naming.DBNetworkName(inst.TeamSlug, inst.ProjectSlug, inst.Name)
	if err != nil {
		return err
	}
	// 前置：共享网络在位（幂等——paused 恢复窗口/外部清理后的自愈）。
	if err := m.docker.NetworkEnsure(ctx, netName); err != nil {
		return fmt.Errorf("rotation network ensure: %w", err)
	}
	jobName, err := naming.DBJobName(inst.Name, "rotate", ulid.Make().String())
	if err != nil {
		return err
	}
	cmd := `mongosh "mongodb://fleetly:${MONGO_PASSWORD}@` + inst.Name + `:27017/admin" --quiet --eval "db.getSiblingDB('admin').updateUser('fleetly', {pwd: '` + new + `'})"`
	exit, err := m.docker.ContainerRun(ctx, ContainerRunInput{
		Name:  jobName,
		Image: image,
		Cmd:   []string{"sh", "-c", cmd},
		Env:   []string{"MONGO_PASSWORD=" + old},
		// sh -c 展开需要 shell；ContainerRun 无 Cmd 包装——命令自带 sh -c
		// 形态（官方 mongo 镜像 ENTRYPOINT 透传）。
		Network: netName,
	})
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("credential rotation job exited %d (authentication with the stored password failed, or the instance is unhealthy; verify with databases reveal and retry)", exit)
	}
	return nil
}

// rematerializeReferences 重物化全部引用 app 的 system env 行（值回
// pending，随各自重部署消费；与 engine dbinject 部署期物化双写者同形——
// 行形状由 dbtemplate.ConnectionVars 唯一定义）。返回去重后的引用 app ID。
func (m *Manager) rematerializeReferences(ctx context.Context, inst *state.DatabaseInstance, templateID, password string) ([]string, error) {
	refs, err := m.store.ListDatabaseReferencesByDB(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	appIDs := make([]string, 0, len(refs))
	seen := map[string]bool{}
	for _, r := range refs {
		if seen[r.AppID] {
			continue
		}
		seen[r.AppID] = true
		appIDs = append(appIDs, r.AppID)
	}
	if len(appIDs) == 0 {
		return nil, nil
	}
	vars, err := dbtemplate.ConnectionVars(templateID, inst.Name, password)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	err = m.store.InTx(ctx, func(tx *state.Tx) error {
		for _, appID := range appIDs {
			for _, key := range keys {
				cipher, encErr := m.box.Encrypt([]byte(vars[key]))
				if encErr != nil {
					return encErr
				}
				if _, err := tx.SetAppEnv(ctx, appID, key, string(cipher), "system"); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return appIDs, nil
}

// redeployReferencingApps 为全部引用 app 入队重部署（各自走正常部署队列；
// compose 复用上一个 succeeded 部署的持久化路径与 spec_hash——引擎按常规
// 发布语义重载自校验，pending 物化行随本次部署消费）。返回已入队 app 名
// 与未入队清单（无成功部署史/持久化缺失 = 无法自重部署，如实暴露人工收尾
// 面）。审计与事件：db.rotate 审计（api 层）+ deployment.queued 事件
// （source=db_rotate）承载留痕；编排失败的缺口进 RotationStageError。
func (m *Manager) redeployReferencingApps(ctx context.Context, inst *state.DatabaseInstance, appIDs []string) (enqueued []string, missing []string, err error) {
	for _, appID := range appIDs {
		app, aerr := m.store.GetAppByID(ctx, appID)
		if aerr != nil {
			missing = append(missing, appID)
			continue
		}
		source, serr := m.lastSucceededDeployment(ctx, appID)
		if serr != nil {
			return nil, nil, serr
		}
		if source == nil {
			// 无成功部署史：引用关系在册但该 app 从未成功部署过（建库与
			// 引用并行窗口的孤儿登记）——重物化行已 pending，其下次常规
			// 部署自然消费；不在此代为首发。
			missing = append(missing, app.Name)
			continue
		}
		deployID := ulid.Make().String()
		var rec state.DeployRecord
		terr := m.store.InTx(ctx, func(tx *state.Tx) error {
			r, err := tx.CreateDeployment(ctx, state.DeployRecord{
				ID:          deployID,
				AppID:       appID,
				AppName:     app.Name,
				Kind:        "deploy",
				SpecHash:    source.SpecHash,
				ComposePath: source.ComposePath,
			})
			if err != nil {
				return err
			}
			rec = r
			if _, err := tx.AppendEvent(ctx, state.Event{
				Name:    "deployment.queued",
				Subject: "deployment:" + rec.ID,
				Payload: state.DiffSummary("deployment", rec.ID, "app", app.Name, "source", "db_rotate"),
			}); err != nil {
				return err
			}
			return tx.WriteAudit(ctx, state.AuditEntry{
				Actor:  "system",
				Action: "db.rotate",
				Target: "database:" + inst.Name,
				Result: "ok",
				DiffSummary: state.DiffSummary("redeployed_app", app.Name,
					"deployment", rec.ID),
			})
		})
		if terr != nil {
			return nil, nil, terr
		}
		m.log.Info("database: referencing app enqueued for redeploy (credential rotation)",
			"instance", inst.Name, "app", app.Name, "deployment", rec.ID)
		enqueued = append(enqueued, app.Name)
	}
	sort.Strings(enqueued)
	sort.Strings(missing)
	return enqueued, missing, nil
}

// lastSucceededDeployment 取该 app 最近一次 succeeded 部署的编排输入
// （compose 持久化路径 + spec_hash——重部署的期望态真源）。
func (m *Manager) lastSucceededDeployment(ctx context.Context, appID string) (*state.DeployRecord, error) {
	rows, err := m.store.ListAppDeployments(ctx, appID, 25)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Status != state.DeploySucceeded || rows[i].SpecHash == "" || rows[i].ComposePath == "" {
			continue
		}
		return &rows[i], nil
	}
	return nil, nil
}
