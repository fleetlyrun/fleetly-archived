package api

import (
	"context"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/execrelay"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ExecService 实现 server.v1.ExecService（E7，web-terminal §2.5，W5-S6）：
// Web 终端受理面——ticket 签发与状态视图。整个服务面 terminal scope
// （scope.go 登记；独立 scope，默认仅 admin）。WS 数据面不走 proto（帧协议
// 见 internal/execrelay/protocol.go，native 端点挂 runtime 的 root handler
// 分派——本服务只承接 REST 受理面）。
type ExecService struct {
	serverv1.UnimplementedExecServiceServer
	st *state.Store
	// hub 是终端 hub（ticket 表/连接表/会话表的持有者）。
	hub *execrelay.Hub
	// relay 是 relay 部署收敛管理器（status 面的部署态投影；nil = 未装配——
	// 如实报 not deployed）。
	relay *execrelay.Manager
}

// NewExecService 构造 ExecService。
func NewExecService(st *state.Store, hub *execrelay.Hub) *ExecService {
	return &ExecService{st: st, hub: hub}
}

// WithRelayManager 注入 relay 部署收敛管理器（链式装配；nil 合法）。
func (s *ExecService) WithRelayManager(m *execrelay.Manager) *ExecService {
	s.relay = m
	return s
}

// CreateTerminalTicket 签发一次性终端接入 ticket（60s、绑 token+app+
// service——设计 §2.5「浏览器无自定义 WS 头的诚实解」）：功能开关门
// （E_TERMINAL_DISABLED）→ app 存在性 → ticket 铸造。**不审计**——ticket
// 不授权任何执行，真正的安全事件是会话起止（terminal.opened/closed 在
// hub fail-closed 落审计）。
func (s *ExecService) CreateTerminalTicket(ctx context.Context, req *serverv1.CreateTerminalTicketRequest) (*serverv1.CreateTerminalTicketResponse, error) {
	if !s.hub.Enabled() {
		return nil, terminalDisabled()
	}
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：terminal scope 对人类角色按 §3.2 矩阵映射为
	// developer+（levelDeploy）——Web 终端对会话/用户 PAT 凭据的角色蕴含；
	// 机具令牌仍由独立 terminal scope 把门（第 1 门，逐字不动）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	var tokenID string
	if p, ok := PrincipalFromContext(ctx); ok {
		tokenID = p.TokenID
	}
	// 绑定携带三段限定形（v0.3 流标签口径）——hub 侧按限定形推导 Swarm
	// 服务名（fleetly-<team>-<prj>-<app>-<service>，rbac-teams §4.3）。
	b := s.hub.Tickets().CreateLabeled(tokenID, req.GetApp(), app.QualifiedName(), req.GetService())
	return &serverv1.CreateTerminalTicketResponse{
		Ticket:           b.Ticket,
		ExpiresAt:        timestamppb.New(b.Expires),
		WebsocketPath:    execrelay.TerminalWSPath + "?ticket=" + b.Ticket,
		ExpiresInSeconds: int32(execrelay.TicketTTLSeconds), //nolint:gosec // G115：常量秒数，量级极小
	}, nil
}

// GetTerminalStatus 终端功能状态视图（Console 面板状态行来源）：功能开关
// / relay 部署态 / 已连接节点数（node liveness = 连接存在）/ 活跃会话数。
func (s *ExecService) GetTerminalStatus(ctx context.Context, _ *serverv1.GetTerminalStatusRequest) (*serverv1.GetTerminalStatusResponse, error) {
	resp := &serverv1.GetTerminalStatusResponse{
		Enabled:        s.hub.Enabled(),
		NodesConnected: int32(s.hub.NodesConnected()), //nolint:gosec // G115：节点计数，量级极小
		ActiveSessions: int32(s.hub.ActiveSessions()), //nolint:gosec // G115：会话计数，量级极小
	}
	if s.relay != nil {
		dep, err := s.relay.DeploymentStatus(ctx)
		if err != nil {
			return nil, err
		}
		resp.RelayDeployed = dep.Exists
		resp.RelayImage = dep.Image
	}
	return resp, nil
}

// terminalDisabled 构造 E_TERMINAL_DISABLED 信封（409——功能开关关闭的
// 诚实拒绝；Console 面板按禁用态渲染）。
func terminalDisabled() error {
	return apperr.New("E_TERMINAL_DISABLED",
		"the web terminal is disabled (terminal.enabled=false): set it to true in the control plane config and restart fleetlyd").WithStage("terminal.ticket")
}
