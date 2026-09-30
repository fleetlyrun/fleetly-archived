package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/logs"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/victorialogs"
)

// LogsService 实现 server.v1.LogsService（T2.20）：Follow 接管线实时面
// （ring 回放 + 实时扇出），History 接落盘/构建产物检索。脱敏已在采集端
// （internal/logs），本面不二次处理。
//
// E6 W5-S1 扩展：SearchLogs 统一检索（VL LogsQL 后端——backend=jsonl 或
// VL 不可达都以 E_LOGS_BACKEND_UNAVAILABLE 诚实报错，不返回空列表冒充）；
// GetLogsBackend/SetLogsBackend 日志后端视图与切换（set 即生效——后台收敛
// 由 victorialogs.Manager 常驻循环承载，本面只落设置）。vl/vm 可为 nil
//（测试/精简装配形态——SearchLogs 如实报后端不可用，backend 面部署态
// 如实报 unknown）。
type LogsService struct {
	serverv1.UnimplementedLogsServiceServer
	st *state.Store
	mg *logs.Manager
	// vl 是 VL 查询/入湖消费端（nil = 未装配——SearchLogs 如实报不可用）。
	vl *victorialogs.Backend
	// vm 是 VL 收敛管理器（nil = 未装配——backend 视图部署态 unknown）。
	vm *victorialogs.Manager
}

// NewLogsService 构造 LogsService。
func NewLogsService(st *state.Store, mg *logs.Manager) *LogsService {
	return &LogsService{st: st, mg: mg}
}

// WithVictorialogs 注入 VL 消费端与收敛管理器（W5-S1；链式装配，nil
// 合法）。
func (s *LogsService) WithVictorialogs(vl *victorialogs.Backend, vm *victorialogs.Manager) *LogsService {
	s.vl = vl
	s.vm = vm
	return s
}

// FollowLogs 实时跟随（server-streaming；ctx 取消即断流，重连 = 重新
// Follow）。订阅键 = app 的三段限定形（v0.3 流标签口径——采集端 ring 以
// 限定形记账，rbac-teams §4.3）。app 参数即引用（裸名/限定形，可见域解析
// + 角色门——W2-S4；FollowLogs 的强制约束由此承载）。
func (s *LogsService) FollowLogs(req *serverv1.FollowLogsRequest, stream serverv1.LogsService_FollowLogsServer) error {
	ctx := stream.Context()
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return err
	}
	ch, cancel := s.mg.Follow(ctx, app.QualifiedName(), req.GetService())
	defer cancel()
	for entry := range ch {
		if err := stream.Send(&serverv1.FollowLogsResponse{Entry: logEntryView(entry)}); err != nil {
			return err
		}
	}
	return nil
}

// ListHistoryLogs 历史检索（时间窗/服务/来源过滤；可见域解析 + 角色门）。
func (s *LogsService) ListHistoryLogs(ctx context.Context, req *serverv1.ListHistoryLogsRequest) (*serverv1.ListHistoryLogsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	// 诚实边界（observability 设计 §2.3「JSONL 边界」：victorialogs 模式
	// 停 JSONL 落盘、检索不跨界——CLI 与 Console 文案同口径，与 SearchLogs
	// 的 jsonl 分支对称）：container 来源的历史面在 victorialogs 模式为空
	//（落盘从未发生），不返回空列表冒充有数——指路 search；build 来源仍
	// 可检索（builds 表产物与 backend 无关）。
	if src := req.GetSource(); src == "" || src == logs.SourceContainer {
		in, err := s.st.LoadLogsSettings(ctx)
		if err != nil {
			return nil, err
		}
		if in.Backend == state.LogsBackendVictorialogs {
			return nil, apperrLogsBackendUnavailable(
				"log history is unavailable: logs.backend=victorialogs does not write the JSONL store (use 'fleetly logs search' or the Console search face, which covers the VictoriaLogs window); build logs remain queryable with --source build")
		}
	}
	q := logs.HistoryQuery{
		// 解析后的三段限定形下发（logs.Manager.History 内部按 GetAppByName
		// 重解析并换算三段限定形流键；本面的可见域解析 + 角色门已先行收口
		// ——W2-S4）。传限定形而非原始引用：路由参数 = 平台 id 时 Manager
		// 内部 GetAppByName 恒 miss（裸 app not found 未映射错误码 → 裸
		// 500，2026-09-26 staging 多应用复现）；裸名在跨项目同名时撞
		// ErrAppAmbiguous 同病——限定形经 GetAppByQualifiedName 精确命中。
		App:     app.QualifiedName(),
		Service: req.GetService(),
		Source:  req.GetSource(),
		Limit:   int(req.GetLimit()),
	}
	if req.GetSince() != nil {
		q.Since = req.GetSince().AsTime()
	}
	if req.GetUntil() != nil {
		q.Until = req.GetUntil().AsTime()
	}
	rows, err := s.mg.History(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.LogEntryView, 0, len(rows))
	for _, e := range rows {
		out = append(out, logEntryView(e))
	}
	return &serverv1.ListHistoryLogsResponse{Entries: out}, nil
}

// Search limit 缺省与天花板（与 ListHistoryLogs/proto 契约同口径：
// buf.validate lte:1000 与 internal/logs History 上限同值）。
const (
	defaultSearchLimit = 200
	maxSearchLimit     = 1000
)

// SearchLogs 统一检索（E6 设计 §3.1，W5-S1）：VL LogsQL 后端。诚实边界
//（同码 E_LOGS_BACKEND_UNAVAILABLE 两分支）：① 当前 backend=jsonl（检索
// 面只在日志库——不返回空列表冒充）；② VL 不可达（检索降级，直播面不受
// 影响）。keyword 由 victorialogs.BuildLogsQL 转义为字面量短语（注入安全
// 硬性条款；白名单外的服务名/来源以 InvalidArgument 拒绝）。
//
// v0.3 W2-S4 强制 app 约束（rbac-teams §4.2）：非平台管理员的用户查询必须
// 含 `app="team/prj/app"` 三段限定形流选择器（请求 apps 字段即选择器面）；
// 无选择器 / 选择器非限定形 / 引用不可见 app → 拒绝带指引。命中校验后按
// 解析出的规范限定形下发查询（与 ingest 写入的标签值同口径）。机具令牌与
// 平台管理员不受约束（全库）。
func (s *LogsService) SearchLogs(ctx context.Context, req *serverv1.SearchLogsRequest) (*serverv1.SearchLogsResponse, error) {
	tasks := req.GetTasks()
	apps, err := s.constrainSearchApps(ctx, req.GetApp(), req.GetApps(), tasks)
	if err != nil {
		return nil, err
	}
	if s.vl == nil {
		return nil, apperrLogsBackendUnavailable(
			"log search is unavailable: the log backend face is not assembled in this build")
	}
	in, err := s.st.LoadLogsSettings(ctx)
	if err != nil {
		return nil, err
	}
	if in.Backend != state.LogsBackendVictorialogs {
		return nil, apperrLogsBackendUnavailable(
			"log search is unavailable: logs.backend=jsonl has no search face (search covers only the VictoriaLogs window; the JSONL history stays on disk)")
	}
	offset, err := decodeSearchCursor(req.GetCursor())
	if err != nil {
		return nil, statusInvalidArgument("invalid search cursor: "+err.Error())
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	q := victorialogs.SearchQuery{
		Apps:     apps,
		Keyword:  req.GetKeyword(),
		Services: req.GetServices(),
		Sources:  req.GetSources(),
		Tasks:    tasks,
		Limit:    limit,
		Offset:   offset,
	}
	if req.GetTimeStart() != nil {
		q.Start = req.GetTimeStart().AsTime()
	}
	if req.GetTimeEnd() != nil {
		q.End = req.GetTimeEnd().AsTime()
	}
	rows, err := s.vl.Search(ctx, q)
	if err != nil {
		if errors.Is(err, victorialogs.ErrBadQuery) {
			return nil, statusInvalidArgument(err.Error())
		}
		return nil, apperrLogsBackendUnavailable(
			"log search is unavailable: the VictoriaLogs backend did not answer (search degraded; live tail is unaffected)")
	}
	hasMore := false
	if len(rows) > limit {
		rows = rows[:limit]
		hasMore = true
	}
	out := make([]*serverv1.SearchLogRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, &serverv1.SearchLogRow{
			At:      timestamppb.New(r.At),
			App:     r.App,
			Service: r.Service,
			Source:  r.Source,
			Task:    r.Task,
			Stderr:  r.Stderr,
			Msg:     r.Msg,
			Fields:  searchRowFields(r.Fields),
		})
	}
	resp := &serverv1.SearchLogsResponse{Rows: out}
	if hasMore {
		resp.NextCursor = encodeSearchCursor(offset + len(out))
	}
	return resp, nil
}

// constrainSearchApps 校验并归一 SearchLogs 的 app 流选择器（v0.3 W2-S4
// 强制约束，rbac-teams §4.2「SearchLogs 强制 app 约束」的执行点）：
//   - 选择器必填（proto 形状已约束，此处双保险——无选择器 400 带指引）；
//   - **DT-5 例外**：tasks 选择器非空且调用方为全局凭据（机具令牌/平台
//     管理员）时 app 可空——任务行无 app 归属，task 流标签即约束面；用户
//     凭据不得以 tasks 绕过 app 约束（仍要求限定形 app 选择器）；
//   - 非全局调用方（用户且非平台管理员）：选择器必须是三段限定形
//     team/prj/app（选择器即约束面——裸名无从判定「查的是哪个项目的流」），
//     解析后过角色门（不可见/越权随门拒绝）；
//   - 全局调用方（机具令牌/平台管理员）：任意引用形态，按解析出的规范限
//     定形下发；
//   - 返回查询过滤集（规范限定形——与 ingest 写入的流标签值同口径）。
//     预留的 apps 重复字段与归一结果不符即 400（单 app 诚实边界，不静默
//     忽略额外值）。
func (s *LogsService) constrainSearchApps(ctx context.Context, appRef string, extraApps []string, tasks []string) ([]string, error) {
	appRef = strings.TrimSpace(appRef)
	if appRef == "" {
		if len(tasks) > 0 && callerIsGlobal(ctx, s.st) {
			return nil, nil // 任务日志面：task 选择器承载约束
		}
		return nil, statusInvalidArgument(
			`search requires an app stream selector: pass app="team/prj/app" (the qualified form is mandatory for user credentials)`)
	}
	global := callerIsGlobal(ctx, s.st)
	if !global && strings.Count(appRef, "/") != 2 {
		return nil, statusInvalidArgument(fmt.Sprintf(
			"app stream selector %q must use the qualified form team/prj/app for user credentials (search is enforced per app; resolve the bare name with 'fleetly apps list' first)", appRef))
	}
	app, err := resolveApp(ctx, s.st, appRef)
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	canonical := app.QualifiedName()
	if len(extraApps) > 0 {
		normalized := make([]string, 0, len(extraApps))
		for _, a := range extraApps {
			row, rerr := resolveApp(ctx, s.st, a)
			if rerr != nil {
				return nil, rerr
			}
			if rerr := requireAppAccess(ctx, s.st, row); rerr != nil {
				return nil, rerr
			}
			normalized = append(normalized, row.QualifiedName())
		}
		return append([]string{canonical}, normalized...), nil
	}
	return []string{canonical}, nil
}

// searchRowFields 投影访问行的结构化字段（白名单键双保险——入湖侧已过滤，
// 读侧再过一次同一词表，未知键不透传给消费者）。
func searchRowFields(fields map[string]string) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		if logs.AllowedAccessFieldKey(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// encodeSearchCursor 签发分页游标（偏移量的 base64url 不透明形态——
// 客户端只回传服务端签发的值）。
func encodeSearchCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// decodeSearchCursor 解析游标（空 = 第一页；非服务端签发形态以
// InvalidArgument 拒绝——不猜不将就）。
func decodeSearchCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 {
		return 0, errors.New("cursor payload is not a page offset")
	}
	return n, nil
}

// GetLogsBackend 日志后端视图（CLI logs backend show / Console 卡共面）。
func (s *LogsService) GetLogsBackend(ctx context.Context, _ *serverv1.GetLogsBackendRequest) (*serverv1.GetLogsBackendResponse, error) {
	in, err := s.st.LoadLogsSettings(ctx)
	if err != nil {
		return nil, err
	}
	return &serverv1.GetLogsBackendResponse{View: s.backendView(ctx, in)}, nil
}

// SetLogsBackend 切换日志后端（victorialogs | jsonl）：设置保存 + 审计 +
// 事件同事务（state 层 fail-closed）；收敛循环下一拍按新值收敛（部署或移除，
// 卷保留）。返回保存后的视图。
func (s *LogsService) SetLogsBackend(ctx context.Context, req *serverv1.SetLogsBackendRequest) (*serverv1.SetLogsBackendResponse, error) {
	// 平台面写门（v0.3 W3-S2 扩全，rbac-teams §3.2「全局设置 → 仅平台管理
	// 员」）：日志后端切换触发后台收敛与采集路由翻转，属平台全局设置写
	// 面——用户 principal 须平台管理员（机具令牌沿 scope 门现状）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	opts := state.LogsSaveOptions{Actor: "human"}
	if p, ok := PrincipalFromContext(ctx); ok {
		opts.ActorTokenID = p.TokenID
	}
	if err := s.st.SaveLogsSettings(ctx, req.GetBackend(), opts); err != nil {
		return nil, err
	}
	in, err := s.st.LoadLogsSettings(ctx)
	if err != nil {
		return nil, err
	}
	return &serverv1.SetLogsBackendResponse{View: s.backendView(ctx, in)}, nil
}

// backendView 组装后端视图（部署态 + ingest streak + 丢弃计数——设计 §2.3
//「丢弃计数常驻」的诚实口径；mg 为 nil 的测试/精简形态 = 计数恒 0、
// streak 恒未降级）。
func (s *LogsService) backendView(ctx context.Context, in state.LogsSettings) *serverv1.LogsBackendView {
	v := &serverv1.LogsBackendView{
		Backend:      in.Backend,
		BackendSet:   in.Set,
		Deployment:   logsBackendDeploymentUnknown,
		DroppedTotal: s.mg.IngestDroppedTotal(),
	}
	switch {
	case in.Backend != state.LogsBackendVictorialogs:
		v.Deployment = logsBackendDeploymentRemoved
	case s.vm == nil:
		v.Deployment = logsBackendDeploymentUnknown
	default:
		st, err := s.vm.DeploymentStatus(ctx)
		switch {
		case err != nil:
			v.Deployment = logsBackendDeploymentUnknown
		case st.Exists:
			v.Deployment = logsBackendDeploymentDeployed
		default:
			v.Deployment = logsBackendDeploymentPending
		}
	}
	v.IngestDegraded = s.mg.IngestDegraded()
	if since := s.mg.IngestStreakSince(); !since.IsZero() {
		v.IngestDegradedSince = timestamppb.New(since)
	}
	return v
}

// 部署态词表（proto LogsBackendView.deployment 注释同步维护）。
const (
	logsBackendDeploymentDeployed = "deployed"
	logsBackendDeploymentPending  = "pending"
	logsBackendDeploymentRemoved  = "removed"
	logsBackendDeploymentUnknown  = "unknown"
)

// apperrLogsBackendUnavailable 构造 E_LOGS_BACKEND_UNAVAILABLE 信封
//（503——检索面降级的诚实报错，设计 §3.1；不返回空列表冒充）。
func apperrLogsBackendUnavailable(msg string) error {
	return apperr.New("E_LOGS_BACKEND_UNAVAILABLE", "%s", msg).WithStage("logs.search")
}

func logEntryView(e logs.Entry) *serverv1.LogEntryView {
	return &serverv1.LogEntryView{
		App:     e.App,
		Service: e.Service,
		At:      timestamppb.New(e.At),
		Stderr:  e.Stderr,
		Line:    e.Line,
		Source:  e.Source,
	}
}
