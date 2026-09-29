package database

// MoveDatabase 换名重部署编排（rbac-teams §3.4 D-W0-4 二修的执行面，W2-S3）：
// 库族底座对象（服务/网络/secret）三段公式以归属为参数——改派（MoveDatabase）
// 即换名重部署。数据卷公式不变（id8 尾缀防撞）——**零卷迁移/零数据搬移**：
// 新名服务引用同一物理卷。
//
// 编排顺序（api 层 MoveDatabase RPC 在归属切换落库后调用）：
//   1. 旧名服务移除（卷独占挂载——新旧不能并存，短暂停机窗口如实披露；
//      服务移除即释放卷引用，幂等：缺失视为成功）；
//   2. 旧名凭据 secret 清场（fleetly.db label 值 = 旧限定形——best-effort，
//      in-use 由下一拍重试）；
//   3. 旧名共享网络移除（best-effort：仍有端点挂接由调用方文档/重试消化）；
//   4. 收敛拍触发（Kick）+ 等待新名服务在位（收敛链异步 ≤ 一拍——网络/
//      secret/卷登记/服务创建全链按新归属推导；健康门由 watchHealthy 观察
//      路径自然收口）。
//
// 引用面（db_references）随行不变：引用方 app 以**实例名**解析连接（env
// 前缀 FLEETLY_DB_<NAME>，名字与归属正交）；引用方下次重部署时由部署期
// 解析读到新归属的网络名（跨项目引用守卫 E_DB_PROJECT_MISMATCH 归 S4——
// S3 后跨项目既有引用在引用方重部署时诚实暴露网络不可达）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

const (
	// moveReapPollInterval 是旧名对象消失等待的轮询周期。
	moveReapPollInterval = 500 * time.Millisecond
	// moveReapTimeout 是旧名服务移除传播的预算（ServiceRemove 到对象消失
	// 有传播延迟——reap 同款「再 inspect 确认」纪律）。
	moveReapTimeout = 60 * time.Second
	// moveNewServiceTimeout 是新名服务在位的等待预算（收敛拍 ≤10s 量级，
	// 预算放宽覆盖底座抖动）。
	moveNewServiceTimeout = 90 * time.Second
)

// MoveDatabaseRedeploy 执行改派后的换名重部署（归属已在库中切换；inst =
// 切换后的行，oldTeamSlug/oldPrjSlug = 切换前 slug——旧名对象清理的参数）。
func (m *Manager) MoveDatabaseRedeploy(ctx context.Context, inst state.DatabaseInstance, oldTeamSlug, oldPrjSlug string) error {
	if oldTeamSlug == "" || oldPrjSlug == "" {
		return errors.New("database: move redeploy requires the previous team/project slugs")
	}
	// 从未收敛过（无旧名服务）= 无底座对象随迁移：直接触发新名收敛。
	oldSvcName, err := naming.DBServiceName(oldTeamSlug, oldPrjSlug, inst.Name, serviceSuffixOf(inst.Template))
	if err != nil {
		return fmt.Errorf("database: move naming (old service): %w", err)
	}
	if cur, ierr := m.docker.ServiceInspect(ctx, oldSvcName); ierr == nil && cur.Exists {
		if rerr := m.docker.ServiceRemove(ctx, oldSvcName); rerr != nil {
			return fmt.Errorf("database: move remove old service %s: %w", oldSvcName, rerr)
		}
		if werr := m.awaitServiceGone(ctx, oldSvcName); werr != nil {
			return werr
		}
		m.log.Info("database: move removed old-name service", "instance", inst.Name, "service", oldSvcName)
	}
	// 旧名凭据 secret 清场（fleetly.db label 选择器 = 旧限定形；best-effort
	// ——单条失败不阻塞改派：材料无引用后无害，删除 reap 兜底）。
	oldQualified := oldTeamSlug + "/" + oldPrjSlug + "/" + inst.Name
	if names, serr := m.docker.SecretList(ctx, map[string]string{state.LabelDatabase: oldQualified}); serr == nil {
		for _, name := range names {
			if rerr := m.docker.SecretRemove(ctx, name); rerr != nil {
				m.log.Warn("database: move stale credential secret cleanup deferred", "instance", inst.Name, "secret", name, "error", rerr)
				continue
			}
		}
	}
	// 旧名共享网络移除（best-effort——引用方 app 尚挂接时失败，属预期：
	// 引用方重部署后自然可清）。
	if oldNet, nerr := naming.DBNetworkName(oldTeamSlug, oldPrjSlug, inst.Name); nerr == nil {
		if rerr := m.docker.NetworkRemove(ctx, oldNet); rerr != nil {
			m.log.Warn("database: move old network remove deferred", "instance", inst.Name, "network", oldNet, "error", rerr)
		}
	}
	// 新名收敛：触发拍 + 等待服务在位（健康门异步收口——watchHealthy）。
	m.Kick()
	newSvcName, err := naming.DBServiceName(inst.TeamSlug, inst.ProjectSlug, inst.Name, serviceSuffixOf(inst.Template))
	if err != nil {
		return fmt.Errorf("database: move naming (new service): %w", err)
	}
	deadline := time.Now().Add(moveNewServiceTimeout)
	for {
		cur, ierr := m.docker.ServiceInspect(ctx, newSvcName)
		if ierr == nil && cur.Exists {
			m.log.Info("database: move converged new-name service", "instance", inst.Name, "service", newSvcName)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database: move new service %s did not converge within %s (convergence retried on next beats)", newSvcName, moveNewServiceTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(moveReapPollInterval):
		}
	}
}

// awaitServiceGone 等待服务对象消失（ServiceRemove 的传播延迟窗；reap 同
// 款「再 inspect 确认」纪律）。
func (m *Manager) awaitServiceGone(ctx context.Context, name string) error {
	deadline := time.Now().Add(moveReapTimeout)
	for {
		cur, err := m.docker.ServiceInspect(ctx, name)
		if err == nil && !cur.Exists {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database: move old service %s still present after %s (removal propagation window exceeded)", name, moveReapTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(moveReapPollInterval):
		}
	}
}
