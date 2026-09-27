package engine

// MoveApp 换名重部署编排（rbac-teams §3.4/§4.3 D-W0-4 二修的执行面，W2-S3）：
// 底座命名三段 `fleetly-<team>-<prj>-<app>-*` 以归属为参数——改派（MoveApp）
// 即换名重部署。本文件提供编排原语（api 层 MoveApp RPC 组合消费）：
//
//	① EnqueueMoveRedeploy   归属切换后沿正常发布管线入队重部署（复用上个
//	                         succeeded 部署的 ComposePath+SpecHash——db_rotate
//	                         引用方重部署同款原语；新发布的规划/快照/revision
//	                         全链在新命名上下文生成，revisions/cron 模板/漂移
//	                         重放随之自然校正）；
//	② AwaitAppSwap          等待新命名上下文的长驻服务就位（每服务 running
//	                         任务数 ≥ 期望副本；deployTimeout 预算）；
//	③ SweepMovedServices    摘除旧命名上下文的长驻服务（在途 cron job 豁免
//	                         ——瞬时对象由调度器收口，见下）。
//
// 中断语义（设计 §3.4 披露原文）：无卷服务建新名→等 running→删旧名（近零
// 中断——新旧并存窗口内流量仍走旧名，切流由域名路由键随新发布自然翻转）；
// 有卷服务 stop-first（卷独占挂载，新旧不能并存——等新服务 running 的判定
// 在旧服务仍持卷时必然超时，故有卷 app 的 MoveApp 如实走「短暂停机窗口」：
// 发布管线对新服务以 stop-first 强制序重建，②的等待窗口即停机窗口）。
//
// cron 在途 job 处置（裁决：让在途 job 跑完）：一次性 job 服务名带 ulid8
// 尾缀且 fleetly-cron- 前缀族不变——在途 job 的收口删除由调度器按台账行
// 执行（与改派无关）；改派瞬间的在途 job 在旧名网络上继续跑到完成，其后
// 的触发由新快照模板（新归属 label）在新命名上下文生成。③按前缀豁免
// cron job 服务，不误删在途任务。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// moveSwapPollInterval 是换名交换等待的轮询周期（引擎 tick 2s 量级的子集
// ——等待面只读底座，短轮询换取响应性）。
const moveSwapPollInterval = 500 * time.Millisecond

// Store 是权威态读端口出口（runtime 装配的 api.AppMovePort 粘合层消费——
// 入队原语 EnqueueMoveRedeploy 与发布引擎共用 state.Store 单点）。
func (e *Engine) Store() *state.Store { return e.store }

// EnqueueMoveRedeploy 为改派后的 app 入队重部署（正常部署队列；compose
// 复用上一个 succeeded 部署的持久化路径与 spec_hash——引擎 preparing 重载
// 自校验）。无成功部署史返回 ErrNoRedeploySource（调用方语义：app 从未
// 发布 = 无底座对象随归属迁移，跳过编排）。deployment.queued 事件
// （source=app_move）与审计（app.move_redeploy）同事务落。
func EnqueueMoveRedeploy(ctx context.Context, st *state.Store, appID string) (string, error) {
	return enqueueRedeploy(ctx, st, appID, "app_move", "app.move_redeploy")
}

// enqueueRedeploy 是「按当前态重部署」的共享原语（MoveApp 换名重部署与
// IMPL-T15-1 项目网参与变更重部署同源）：以最近一次 succeeded 部署的
// ComposePath + SpecHash 入队——新发布的规划/快照/revision 全链在**当前**
// 归属与状态（含项目网参与位）下重建，服务滚动由发布管内收敛承载。
// source 进 deployment.queued 事件 payload（归因面），auditAction 进审计。
func enqueueRedeploy(ctx context.Context, st *state.Store, appID, source, auditAction string) (string, error) {
	app, err := st.GetAppByID(ctx, appID)
	if err != nil {
		return "", err
	}
	var from *state.DeployRecord
	rows, err := st.ListAppDeployments(ctx, appID, 25)
	if err != nil {
		return "", err
	}
	for i := range rows {
		if rows[i].Status == state.DeploySucceeded && rows[i].SpecHash != "" && rows[i].ComposePath != "" {
			from = &rows[i]
			break
		}
	}
	if from == nil {
		return "", ErrNoRedeploySource
	}
	deployID := ulid.Make().String()
	err = st.InTx(ctx, func(tx *state.Tx) error {
		if _, err := tx.CreateDeployment(ctx, state.DeployRecord{
			ID:          deployID,
			AppID:       appID,
			AppName:     app.Name,
			Kind:        "deploy",
			SpecHash:    from.SpecHash,
			ComposePath: from.ComposePath,
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(ctx, state.Event{
			Name:    "deployment.queued",
			Subject: "deployment:" + deployID,
			Payload: state.DiffSummary("deployment", deployID, "app", app.Name, "source", source),
		}); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:  "system",
			Action: auditAction,
			Target: "app:" + app.ID,
			Result: "ok",
			DiffSummary: state.DiffSummary("app", app.Name,
				"deployment", deployID, "project", app.ProjectID),
		})
	})
	if err != nil {
		return "", err
	}
	return deployID, nil
}

// ErrNoRedeploySource 表示改派目标无成功部署史（从未发布——无底座对象随
// 归属迁移，MoveApp 编排整体跳过）。
var ErrNoRedeploySource = errors.New("app has no succeeded deployment to redeploy from")

// AwaitAppSwap 等待 app（当前命名上下文）的长驻服务就位：受管服务按新
// 限定形 label 可发现，且每个副本>0 服务的 running 任务数 ≥ 期望副本。
// 预算耗尽返回超时错误（调用方据此决定是否保守放弃旧服务清扫——流量尚在
// 旧名上，删旧 = 主动停机）。从未发布（可发现集为空直至超时）同样超时。
func (e *Engine) AwaitAppSwap(ctx context.Context, appID string, timeout time.Duration) error {
	app, err := e.store.GetAppByID(ctx, appID)
	if err != nil {
		return err
	}
	label := map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelApp:     app.QualifiedName(),
	}
	deadline := e.now().Add(timeout)
	for {
		svcs, err := e.sub.ServiceList(ctx, label)
		if err == nil && len(svcs) > 0 {
			allUp := true
			for _, s := range svcs {
				if s.Replicas == 0 {
					continue // scale-0 服务无运行判据
				}
				tasks, terr := e.sub.TaskList(ctx, s.Name)
				if terr != nil || uint64(max(countRunningTasks(tasks), 0)) < s.Replicas {
					allUp = false
					break
				}
			}
			if allUp {
				return nil
			}
		}
		if e.now().After(deadline) {
			return fmt.Errorf("move swap wait timed out after %s (app %s: services in the new naming context are not running yet)", timeout, app.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(moveSwapPollInterval):
		}
	}
}

// countRunningTasks 统计 running 状态任务数（isNewVersionTask 的镜像判据
// ——MoveApp 的镜像跨新旧服务一致，版本比对退化为状态计数）。
func countRunningTasks(tasks []TaskState) int {
	n := 0
	for _, t := range tasks {
		if t.State == "running" {
			n++
		}
	}
	return n
}

// SweepMovedServices 摘除旧命名上下文的长驻服务（改派清扫收尾；幂等：
// 缺失视为成功）。在途 cron / init job 服务按前缀豁免（见文件头处置裁决
// ——DT-4：在途 init job 同样让位跑完，由发布管线收口）。
// 返回移除数。secret/config 清场不在此——旧名 secret/config（fleetly-<old>-*
// 族）无引用后成为无害孤儿，由 app 删除 reap 的 label 选择器兜底（app
// label 值已随改派切换，旧值选择器扫不到的窗口 = 一次 MoveApp 与一次
// DeleteApp 的罕见叠加，诚实挂账遗留记录；config 族同款）。
func (e *Engine) SweepMovedServices(ctx context.Context, oldQualified string) (int, error) {
	olds, err := e.sub.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelApp:     oldQualified,
	})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, s := range olds {
		if naming.IsCronJobName(s.Name) || naming.IsInitJobName(s.Name) {
			continue // 在途 job 让位：跑完由各自所有者收口
		}
		if err := e.sub.ServiceRemove(ctx, s.Name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
