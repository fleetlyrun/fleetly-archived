package api

// 项目网参与面（T 线 OT-1 / IMPL-T15-1）：ProjectsService 的 attach/detach 两
// RPC。归属（tenancy）与网络参与两面分离——apps.project_id 恒有（00019），
// 参与位（apps.project_network_attached）是**显式 opt-in**，缺省不参加
// （既有 app 私网隔离现状零变化），本文件是唯一的改变路径。
//
// 组合次序（在途守卫 → attach 前置 ensure → 状态落位 → 参与变更重部署）：
//  1. 在途部署拒绝（409）：重部署复用「最近 succeeded 部署」的 compose——
//     在途发布不得被旧快照覆盖（ConvergeApp 先例）。
//  2. attach：项目网幂等 ensure（失败即拒——状态不落位，重试安全；先建网
//     再置位，避免「已置位但网不存在」的窗口）。
//  3. 状态落位：state 原语（参与位 + 审计 + 事件同事务 fail-closed；幂等
//     重跑零重复披露）。
//  4. 参与变更重部署：入队正常发布管线（source=project_network）——新
//     revision 的成员服务双挂/摘除项目网（服务滚动由发布管收敛承载）。
//     无成功部署史 = ErrNoRedeploySource（无底座对象需重投影，状态已落位）；
//     入队失败如实返回错误——状态已落位，重跑同参数幂等收尾（changed=false
//     仍会重试入队，见 setAppProjectNetwork）。
//
// 权限：scope 登记 admin（网络姿态改变是隔离面的敏感写，与 app 删除/
// secrets 同级）+ 项目角色 admin 门（requireAppAccess；平台管理员只读不
// 代写，机具令牌 admin 等价照旧）。

import (
	"context"
	"errors"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
)

// ProjectNetworkPort 是项目网参与变更的编排腿（engine 实现；方向纪律：
// api 定义端口、不感知实现类型——AppMovePort 同款）。
type ProjectNetworkPort interface {
	// EnsureProjectNetwork 幂等确保项目网 overlay 在位（attach 前置）。
	EnsureProjectNetwork(ctx context.Context, projectID string) error
	// EnqueueNetworkRedeploy 入队参与变更重部署；无成功部署史返回
	// engine.ErrNoRedeploySource。
	EnqueueNetworkRedeploy(ctx context.Context, appID string) (string, error)
}

// WithNetworkPort 注入项目网参与编排端口（nil = RPC 如实报不可用，不静默
// 退化——WithMovePorts 同纪律）。
func (s *ProjectsService) WithNetworkPort(port ProjectNetworkPort) *ProjectsService {
	s.netPort = port
	return s
}

// AttachAppProjectNetwork 把 app 挂入其项目网（幂等；语义见 proto 注释）。
func (s *ProjectsService) AttachAppProjectNetwork(ctx context.Context, req *serverv1.AttachAppProjectNetworkRequest) (*serverv1.AttachAppProjectNetworkResponse, error) {
	membership, err := s.setAppProjectNetwork(ctx, req.GetApp(), true)
	if err != nil {
		return nil, err
	}
	return &serverv1.AttachAppProjectNetworkResponse{Membership: membership}, nil
}

// DetachAppProjectNetwork 从项目网摘除 app（幂等；语义见 proto 注释）。
func (s *ProjectsService) DetachAppProjectNetwork(ctx context.Context, req *serverv1.DetachAppProjectNetworkRequest) (*serverv1.DetachAppProjectNetworkResponse, error) {
	membership, err := s.setAppProjectNetwork(ctx, req.GetApp(), false)
	if err != nil {
		return nil, err
	}
	return &serverv1.DetachAppProjectNetworkResponse{Membership: membership}, nil
}

// setAppProjectNetwork 是 attach/detach 的共享组合（见文件头注）。
func (s *ProjectsService) setAppProjectNetwork(ctx context.Context, appRef string, attached bool) (*serverv1.AppProjectNetworkMembership, error) {
	app, err := resolveApp(ctx, s.st, appRef)
	if err != nil {
		return nil, err
	}
	// 角色门（第 2 门）：方法所需层级由拦截器注入的 scope 登记映射
	// （admin → 项目角色 admin+；平台管理员只读 → 403）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	if s.netPort == nil {
		return nil, statusEnvelope(codes.Unavailable, "project network orchestration is not assembled in this build")
	}
	if has, err := s.st.AppHasNonTerminalDeployment(ctx, app.ID); err != nil {
		return nil, err
	} else if has {
		return nil, conflict("app " + app.Name + " has a deployment in flight: wait for it to reach a terminal state before changing project-network participation (the participation redeploy replays the last succeeded revision)")
	}
	if attached {
		// 先建网再置位：确保失败时状态不动（重试安全）；已存在即幂等成功。
		if err := s.netPort.EnsureProjectNetwork(ctx, app.ProjectID); err != nil {
			return nil, err
		}
	}
	updated, changed, err := s.st.SetAppProjectNetworkAttached(ctx, app.ID, attached, principalOf(ctx).UserID, callerTokenID(ctx))
	if err != nil {
		return nil, mapAppErr(err, appRef)
	}
	network, err := naming.ProjectNetworkName(updated.ProjectID)
	if err != nil {
		return nil, statusEnvelope(codes.Internal, "project network naming failed: "+err.Error())
	}
	status := "attached"
	if !attached {
		status = "detached"
	}
	deploymentID := ""
	if id, rerr := s.netPort.EnqueueNetworkRedeploy(ctx, updated.ID); rerr == nil {
		deploymentID = id
		status = "rolling"
	} else if !errors.Is(rerr, engine.ErrNoRedeploySource) {
		return nil, rerr // 状态已落位：重跑同参数幂等收尾（changed=false 仍重试入队）
	}
	return &serverv1.AppProjectNetworkMembership{
		AppId:        updated.ID,
		App:          updated.Name,
		ProjectId:    updated.ProjectID,
		Project:      updated.TeamSlug + "/" + updated.ProjectSlug,
		Network:      network,
		Attached:     updated.ProjectNetworkAttached,
		Changed:      changed,
		DeploymentId: deploymentID,
		Status:       status,
	}, nil
}

// appProjectNetworkProjection 返回 app 的项目网投影字段（AppView 补全；
// 未参与 = 空名 + false——读面与参与状态单一来源）。
func appProjectNetworkProjection(app state.App) (string, bool) {
	if !app.ProjectNetworkAttached {
		return "", false
	}
	name, err := naming.ProjectNetworkName(app.ProjectID)
	if err != nil {
		return "", false // 形态违约（理论不可达）：读面如实回落未参与，不伪造名字
	}
	return name, true
}
