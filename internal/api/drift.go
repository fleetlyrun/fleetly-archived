package api

import (
	"context"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// DriftService 实现 server.v1.DriftService（T2.18）：漂移判定/收敛/opt-in
// 置位复用 engine 既有逻辑（DriftShow/ConvergeApp/SetDriftConverge），本面
// 只做契约投影——不重写对账语义。actor 恒 "human"（API 无法区分人类/AI
// 代理，可追溯性由审计 actor_token_id 承载，与 rollback 面同口径）。
//
// 引擎调用恒传**解析后的三段限定形**（app.QualifiedName()）：engine 侧
// 三方法按 GetAppByName 直查，不识别引用形态——把原始引用（Console 主路径
// 的路由参数 = 平台 id）透传会使 id 寻址恒 404（app not found: <ULID>）；
// 而业务裸名在跨项目同名时（「prod 与 dev 各有 demo」是设计能力）撞
// state.ErrAppAmbiguous 裸抛 500（2026-09-26 staging 实证：demo 双项目
// 重名，GET /v1/apps/<id>/drift → 500）。限定形经
// GetAppByQualifiedName 两段 slug 精确命中，与 handler 已过的 resolveApp
// 门禁指向同一行。
type DriftService struct {
	serverv1.UnimplementedDriftServiceServer
	st  *state.Store
	eng *engine.Engine
}

// NewDriftService 构造 DriftService。
func NewDriftService(st *state.Store, eng *engine.Engine) *DriftService {
	return &DriftService{st: st, eng: eng}
}

// ShowDrift 即时判定运行域漂移（不写事件不收敛——与 CLI 读面同源）。
func (s *DriftService) ShowDrift(ctx context.Context, req *serverv1.ShowDriftRequest) (*serverv1.ShowDriftResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；Converge/Set 同批——show=read、写面=deploy）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	report, err := s.eng.DriftShow(ctx, app.QualifiedName())
	if err != nil {
		return nil, mapStoreErr(err, req.GetApp())
	}
	out := &serverv1.ShowDriftResponse{
		App:               report.App,
		DesiredDeployment: report.DesiredDeployment,
		Drifted:           report.Drifted,
		Services:          make([]*serverv1.ServiceDriftView, 0, len(report.Services)),
	}
	for _, svc := range report.Services {
		view := &serverv1.ServiceDriftView{
			Service: svc.Service,
			Drifted: svc.Drifted,
			Missing: svc.Missing,
			Extra:   svc.Extra,
		}
		for _, d := range svc.Diff {
			view.Diff = append(view.Diff, &serverv1.FieldDiffView{
				Field: d.Field, Expected: d.Expected, Actual: d.Actual,
			})
		}
		out.Services = append(out.Services, view)
	}
	return out, nil
}

// ConvergeDrift 人工一次性收敛（归位重放原语，带审计；在途部署存在时
// 409 语义由引擎承载）。
func (s *DriftService) ConvergeDrift(ctx context.Context, req *serverv1.ConvergeDriftRequest) (*serverv1.ConvergeDriftResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rec, err := s.eng.ConvergeApp(ctx, app.QualifiedName(), "human")
	if err != nil {
		return nil, mapStoreErr(err, req.GetApp())
	}
	return &serverv1.ConvergeDriftResponse{
		App:          req.GetApp(),
		DeploymentId: rec.ID,
		DesiredHash:  rec.DesiredHash,
	}, nil
}

// SetDriftConverge 收敛 opt-in 的人工置位/重置（回滚失败强制关闭后的
// 唯一恢复入口；带审计）。
func (s *DriftService) SetDriftConverge(ctx context.Context, req *serverv1.SetDriftConvergeRequest) (*serverv1.SetDriftConvergeResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	if err := s.eng.SetDriftConverge(ctx, app.QualifiedName(), req.GetEnabled(), "human"); err != nil {
		return nil, mapStoreErr(err, req.GetApp())
	}
	return &serverv1.SetDriftConvergeResponse{
		App: req.GetApp(), Enabled: req.GetEnabled(),
	}, nil
}
