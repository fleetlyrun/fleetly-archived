package api

import (
	"context"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// RuntimeService 实现 server.v1.RuntimeService（2026-09-29 Console IA 重
// 设计，设计 docs/design/2026-09-29-console-ia-redesign.md §5）：应用运行
// 实况只读投影——复用引擎 AppRuntime（ServiceList/TaskList 的观测端口上
// 抛），本面只做契约投影。只读不判定：漂移结论在 DriftService，健康判定
// 在引擎观测循环。
//
// 引擎调用恒传**解析后的三段限定形**（app.QualifiedName()）：engine 侧
// AppRuntime 按 GetAppByName 直查，不识别引用形态——把原始引用（Console
// 主路径的路由参数 = 平台 id）透传会使 id 寻址恒 404（DriftService 同款
// 纪律，见 drift.go 头注）。
type RuntimeService struct {
	serverv1.UnimplementedRuntimeServiceServer
	st  *state.Store
	eng *engine.Engine
}

// NewRuntimeService 构造 RuntimeService。
func NewRuntimeService(st *state.Store, eng *engine.Engine) *RuntimeService {
	return &RuntimeService{st: st, eng: eng}
}

// ShowAppRuntime 即时投影应用运行实况（服务并集 + 每服务全量任务）。
func (s *RuntimeService) ShowAppRuntime(ctx context.Context, req *serverv1.ShowAppRuntimeRequest) (*serverv1.ShowAppRuntimeResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；读面 read——与 drift show/placement show 同层）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	report, err := s.eng.AppRuntime(ctx, app.QualifiedName())
	if err != nil {
		return nil, mapStoreErr(err, req.GetApp())
	}
	out := &serverv1.ShowAppRuntimeResponse{
		App:               report.App,
		DesiredDeployment: report.DesiredDeployment,
		Services:          make([]*serverv1.ServiceRuntimeView, 0, len(report.Services)),
	}
	for _, svc := range report.Services {
		view := &serverv1.ServiceRuntimeView{
			Name:             svc.Name,
			Service:          svc.Service,
			Image:            svc.Image,
			Mode:             runtimeModeOf(svc),
			DeclaredReplicas: svc.DeclaredReplicas,
			ActualReplicas:   svc.ActualReplicas,
			UpdateState:      svc.UpdateState,
			UpdateMessage:    svc.UpdateMessage,
			Missing:          svc.Missing,
			Tasks:            make([]*serverv1.ServiceTaskView, 0, len(svc.Tasks)),
		}
		for _, t := range svc.Tasks {
			view.Tasks = append(view.Tasks, &serverv1.ServiceTaskView{
				Id:           t.ID,
				Slot:         int32(t.Slot),
				State:        t.State,
				DesiredState: t.DesiredState,
				Error:        t.Err,
				Image:        t.Image,
				Timestamp:    tstamp(t.Timestamp),
			})
		}
		out.Services = append(out.Services, view)
	}
	return out, nil
}

// runtimeModeOf 是 mode 词投影（replicated|global；词表见 runtime.proto）。
func runtimeModeOf(svc engine.AppRuntimeService) string {
	if svc.Global {
		return "global"
	}
	return "replicated"
}
