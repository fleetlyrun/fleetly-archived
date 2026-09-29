package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/cron"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// CronTriggers 是 TriggerCronRun RPC 的调度端口（实现方 = internal/cron
// Manager；方向纪律：api 定义端口、不感知实现类型）。
type CronTriggers interface {
	// TriggerRun 手动触发一次（与到点触发同链路；实现方在触发事务内写
	// cron.manual_triggered 审计，actorTokenID 承载归因）。重叠/节点不可用
	// 返回 skipped 行非错误。
	TriggerRun(ctx context.Context, appID, appName, service, actorTokenID string) (state.CronRun, error)
}

// CronService 实现 server.v1.CronService（E5 Cron 手动入口 + 台账读面）。
type CronService struct {
	serverv1.UnimplementedCronServiceServer
	st       *state.Store
	triggers CronTriggers
}

// NewCronService 构造 CronService（triggers 可 nil——调度器未装配的进程内
// 夹具形态，TriggerCronRun 显式不可用）。
func NewCronService(st *state.Store, triggers CronTriggers) *CronService {
	return &CronService{st: st, triggers: triggers}
}

// TriggerCronRun 手动触发一次：app 解析 → 调度端口触发（审计在实现方同
// 事务）→ 行投影。目标服务不构成 cron schedule（不存在/未声明 label）映射
// 404；重叠与节点不可用是 skipped 行（200 + 原因），与调度器处置一致。
func (s *CronService) TriggerCronRun(ctx context.Context, req *serverv1.TriggerCronRunRequest) (*serverv1.TriggerCronRunResponse, error) {
	if s.triggers == nil {
		return nil, statusEnvelope(codes.Internal, "cron scheduler not configured")
	}
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：触发=deploy、台账=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	// 挂起门（app Stop/Start，00028 位）：挂起 = 用户请求停止——手动触发
	// 的一次性 job 与停止语义互斥，显式拒绝（调度器的周期触发同门豁免）。
	if app.Suspended {
		return nil, apperr.New("E_APP_SUSPENDED",
			"app %s is suspended — resume it before triggering cron runs (Console: Start)", app.Name).
			WithContext("app", app.Name)
	}
	run, err := s.triggers.TriggerRun(ctx, app.ID, app.Name, req.GetService(), callerTokenID(ctx))
	if err != nil {
		if errors.Is(err, cron.ErrNoSchedule) {
			return nil, notFound(err.Error())
		}
		return nil, err
	}
	return &serverv1.TriggerCronRunResponse{Run: cronRunView(run)}, nil
}

// ListCronRuns 读运行台账（scheduled_at 倒序；service 收窄单 schedule）。
func (s *CronService) ListCronRuns(ctx context.Context, req *serverv1.ListCronRunsRequest) (*serverv1.ListCronRunsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：触发=deploy、台账=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = state.CronRunKeeper
	}
	rows, err := s.st.ListCronRuns(ctx, app.ID, req.GetService(), limit)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.CronRunView, 0, len(rows))
	for _, r := range rows {
		out = append(out, cronRunView(r))
	}
	return &serverv1.ListCronRunsResponse{App: app.Name, Runs: out}, nil
}

// cronRunView 构造台账投影（时间零值不输出——EmitUnpopulated=false 语义下
// skipped/在途行的 started_at/finished_at 缺省即「未发生」）。
func cronRunView(r state.CronRun) *serverv1.CronRunView {
	v := &serverv1.CronRunView{
		Id:          r.ID,
		Service:     r.Service,
		Expression:  r.Expression,
		Status:      r.Status,
		SkipReason:  r.SkipReason,
		JobService:  r.JobService,
		Error:       r.Error,
		ScheduledAt: timestamppb.New(r.ScheduledAt),
	}
	if !r.StartedAt.IsZero() {
		v.StartedAt = timestamppb.New(r.StartedAt)
	}
	if !r.FinishedAt.IsZero() {
		v.FinishedAt = timestamppb.New(r.FinishedAt)
	}
	return v
}
