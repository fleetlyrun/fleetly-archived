package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/grpc/codes"

	"github.com/oklog/ulid/v2"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/gitserver"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// GitDeployTriggers 是 DeployFromGit RPC 的 git 触发端口（实现方在
// internal/gitserver 的 GitTriggers——compose 真源在其 bare 仓库对象库；
// 方向纪律：api 定义端口、不感知实现类型——唯一例外是跨端口的错误哨兵
// 契约 gitserver.ErrBranchNotTracked（值依赖，非实现类型依赖）；命名口径
// UBIQUITOUS_LANGUAGE §flagged-2：与实现类型同族词汇，弃旧名
// GitDeploySource）。
type GitDeployTriggers interface {
	// DeployFromGitPush 以 git push 语义入队部署：每次调用建部署记录
	// （显式用户动作，不去重——幂等口径绑定在票面）；返回记录与校验期
	// 警告。actorTokenID 记录钩子回调 token（可空）；pushUser 是 SSH 公
	// 钥认证回调解析的署名用户（空 = 存量无主 key——审计 actor 落原口
	// 径）。ref 非该 app 配置分支（空回落 main）时返回
	// gitserver.ErrBranchNotTracked——分支过滤的权威点在实现侧
	//（daemon），本 handler 把哨兵映射为 skipped 回执而非 gRPC 错误。
	DeployFromGitPush(ctx context.Context, app, sha, ref, actorTokenID, pushUser string) (state.DeployRecord, []compose.Warning, error)
}

// DeploymentsService 实现 server.v1.DeploymentsService（T2.17）。
//
// Deploy 契约（proto 取舍注记）：compose 内容字节 + app 名入队，返回
// deployment id 后立即返回；终态由客户端轮询 GetDeployment 或消费事件流。
// compose 落临时文件后走受控子集校验（compose.Load）；A7（S18）起入队时
// 字节持久化 <数据根>/deployments/<id>/compose.yaml 并以该路径随行落库
// ——引擎 preparing 阶段重载复核不再依赖 OS 临时目录（tmpfiles 清理/容器
// 形态丢失免疫），终态后由 janitor 按 30 天窗清理；temp 仅解析中转。
type DeploymentsService struct {
	serverv1.UnimplementedDeploymentsServiceServer
	st  *state.Store
	git GitDeployTriggers
}

// NewDeploymentsService 构造 DeploymentsService（git 触发端口可 nil——
// DeployFromGit 届时显式不可用，进程内夹具形态）。
func NewDeploymentsService(st *state.Store, git GitDeployTriggers) *DeploymentsService {
	return &DeploymentsService{st: st, git: git}
}

// ListDeployments 按应用列部署（created_at 倒序）。app 引用经可见域解析
//（W2-S4）+ 角色门。
func (s *DeploymentsService) ListDeployments(ctx context.Context, req *serverv1.ListDeploymentsRequest) (*serverv1.ListDeploymentsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.st.ListAppDeployments(ctx, app.ID, limit)
	if err != nil {
		return nil, err
	}
	return &serverv1.ListDeploymentsResponse{Deployments: deploymentViews(rows)}, nil
}

// GetDeployment 单条部署。
func (s *DeploymentsService) GetDeployment(ctx context.Context, req *serverv1.GetDeploymentRequest) (*serverv1.GetDeploymentResponse, error) {
	rec, err := s.st.GetDeployment(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.GetDeploymentResponse{Deployment: deploymentView(rec)}, nil
}

// Deploy 入队部署（同 CLI：入队前受控子集校验——compose 违约不动底座、
// 不入队；deployment.queued 事件与审计同事务 fail-closed）。校验期警告随
// 响应带出（T2.18：CLI 改经 RPC 入队后保留警告的人读呈现）。
func (s *DeploymentsService) Deploy(ctx context.Context, req *serverv1.DeployRequest) (*serverv1.DeployResponse, error) {
	// 入队前受控子集校验：内容落临时文件（A7 起 temp 仅解析中转，持久化
	// 副本见下方 PersistDeploymentCompose）。
	dir, err := os.MkdirTemp("", "fleetly-compose-")
	if err != nil {
		return nil, fmt.Errorf("create compose temp dir: %w", err)
	}
	// MG-6：解析中转目录随请求回收——持久化副本在 <数据根>/deployments/
	// <id>/compose.yaml，本目录不存活到函数外，不留孤儿 tmp。
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, req.GetCompose(), 0o600); err != nil { //nolint:gosec // G306：compose 内容非密钥，0600 保守
		return nil, fmt.Errorf("write compose temp file: %w", err)
	}
	spec, warnings, err := compose.Load(ctx, path)
	if err != nil {
		return nil, err // apperr（E_COMPOSE_*）原样透传
	}

	// A1（S18）：compose 应用名与请求 app 必须一致。REST 路径 {app}
	//（gRPC 面 = 请求字段 app）是调用方对目标资源的显式声明，与 compose
	// name 静默错位时部署会落到声明之外的资源上（REST 路径与实际资源
	// 错位 + 误建 app）；拒绝发生在 ensureApp 之前——不误建 app、不入队。
	// 复用 compose 族拒绝码 E_COMPOSE_UNSUPPORTED（errcode 零新增，与
	// H14 同裁决）。
	//
	// id 寻址（5e593d9 起 Console 详情导航以平台 id 寻址）：请求 app 能解
	// 析到既有应用（26 字符 ULID 按 id 短路 / team/prj/app 限定形 / 解析域
	// 内唯一裸名）时，与**解析后的业务名**比对——裸 ULID 与 compose name
	// 必然错位，原样比对会使 Console 部署既有应用恒拒。解析不到既有应用
	// 时保持原语义：原始参数与 spec.Name 严格一致（NotFound = 首次部署的
	// 创建场景，防 app 参数声明 X 却静默建出 Y；裸名歧义等其余解析失败一
	// 并按创建语义兜底——目标归属由下方 ensureApp 的项目内精确查询裁决，
	// 歧义面不在此提前收紧，CLI 按名部署行为不变）。mismatch 报错的
	// 「requested target app」展示解析后的业务名（无业务名可解时回落原始
	// 参数），不再裸显平台 id。
	targetApp := req.GetApp()
	if targetApp != "" {
		if app, resolveErr := resolveApp(ctx, s.st, targetApp); resolveErr == nil {
			targetApp = app.Name
		}
	}
	if req.GetApp() != "" && spec.Name != targetApp {
		return nil, apperr.New("E_COMPOSE_UNSUPPORTED",
			"compose application name %q does not match the requested target app %q (the compose application name and the requested app must match)",
			spec.Name, targetApp).
			WithContext("expected", targetApp).
			WithContext("actual", spec.Name)
	}

	// 归属解析与一致性（v0.3 W2-S3 归属管道，rbac-teams §3.4/§4.2 D-W0-9）：
	// project 引用（裸名/限定形）解析为项目行；应用行已存在时校验行上归属
	// 与请求一致（**校验一致**而非静默沿用——不一致 409 指引 MoveApp）；
	// 首次部署把归属写上 app 行。compose 应用名与请求 app 一致性已在上
	// （A1）——此处 spec.Name 即目标 app 名。
	proj, err := resolveProjectRef(ctx, s.st, req.GetProject())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：部署 = developer+（levelDeploy；平台管理员
	// 不代写在此收口）。首署前先门——不得为无权调用方建 app 行。
	if err := requireResourceAccess(ctx, s.st, proj.ID); err != nil {
		return nil, err
	}
	app, err := ensureApp(ctx, s.st, spec.Name, proj)
	if err != nil {
		return nil, err
	}
	// 破坏性变更门控（MG-C3，架构 §2.4 plan/apply 语义）：destructive 且
	// 未携带 confirm_destructive → E_DEPLOY_CONFIRM_REQUIRED、不入队。
	// 边界（v0.1）：DeployFromGit（git push 路径）不加此闸——admin scope +
	// 公钥认证的受信操作流，push 即显式确认动作。
	if err := s.guardDestructiveDeploy(ctx, app, spec, req.GetConfirmDestructive()); err != nil {
		return nil, err
	}
	// fail-closed 单事务（H13 修复，state-model §2.9）：部署行、
	// deployment.queued 事件与审计在同一个 InTx 内写入——回调内任一失败
	//（含事件/审计写失败）整体回滚、部署行不落库，杜绝「引擎照常执行但
	// 事件与审计缺失」的审计黑洞。回调内拿到的 rec（含生成的 ID/时间戳）
	// 供事件 payload 与审计使用。
	//
	// A7（S18）：compose 字节持久化 <数据根>/deployments/<id>/compose.yaml
	//（先写文件后建行——行不指向缺失文件；临时文件自此仅解析中转，
	// tmpfiles 清理/容器形态丢失不再影响引擎 preparing 与成功固化的重载；
	// 终态后由 janitor 按 30 天窗清理）。
	deployID := ulid.Make().String()
	persistPath, err := state.PersistDeploymentCompose(
		state.DeploymentsRoot(s.st.Path()), deployID, req.GetCompose())
	if err != nil {
		return nil, err
	}
	var rec state.DeployRecord
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		r, err := tx.CreateDeployment(ctx, state.DeployRecord{
			ID:          deployID,
			AppID:       app.ID,
			AppName:     spec.Name,
			Kind:        "deploy",
			SpecHash:    spec.SpecHash,
			ComposePath: persistPath,
		})
		if err != nil {
			return err
		}
		rec = r
		if _, err := tx.AppendEvent(ctx, state.Event{
			Name:    "deployment.queued",
			Subject: "deployment:" + rec.ID,
			Payload: state.DiffSummary("deployment", rec.ID, "app", spec.Name), // B4：构造器替换手拼 JSON
		}); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, auditEntry(ctx, "deployment:"+rec.ID,
			state.DiffSummary("app", spec.Name, "spec_hash", spec.SpecHash, "source", "api"))) // B4：构造器替换手拼 JSON
	}); err != nil {
		return nil, err
	}
	return &serverv1.DeployResponse{
		DeploymentId: rec.ID,
		App:          app.Name,
		Status:       string(state.DeployQueued),
		Warnings:     composeWarnings(warnings),
	}, nil
}

// guardDestructiveDeploy 是 Deploy 的破坏性变更门控（MG-C3，架构 §2.4
// plan/apply 语义「破坏性操作要求 --confirm-destructive」）：取该 app 最新
// revision（保留窗 seq 最高）的归一化 compose 为基线，与本次目标 spec 走
// 判定单源 compose.DestructiveChanges——与 plan artifact 的
// requires_confirm_destructive 同一口径，杜绝 plan 侧与入队侧判定漂移。
// destructive 且请求未置位 confirm_destructive 时返回
// E_DEPLOY_CONFIRM_REQUIRED 信封（修复建议走注册表默认文案：加
// --confirm-destructive 重试），部署不入队。无历史 revision（首发）无基线
// 可比，恒放行；已置位则短路放行。
func (s *DeploymentsService) guardDestructiveDeploy(ctx context.Context, app state.App, target *compose.Spec, confirmed bool) error {
	if confirmed {
		return nil
	}
	revs, err := s.st.ListRevisions(ctx, app.ID)
	if err != nil {
		return err
	}
	if len(revs) == 0 {
		return nil // 首发：无历史版本，无可删除对象
	}
	var base compose.Spec
	if err := json.Unmarshal([]byte(revs[0].ComposeNormalized), &base); err != nil {
		return fmt.Errorf("decode baseline revision %s compose: %w", revs[0].ID, err)
	}
	if !compose.DestructiveChanges(&base, target) {
		return nil
	}
	return apperr.New("E_DEPLOY_CONFIRM_REQUIRED",
		"app %s: this deploy introduces destructive changes relative to the latest revision (service removal / volume unbinding); confirm_destructive was not set, rejecting enqueue", app.Name).
		WithContext("app", app.Name).
		WithContext("baseline_revision_id", revs[0].ID)
}

// DeployFromGit 是 git push(SSH) 触发入口的入队面（T2.19）：post-receive
// 钩子经 loopback REST 携带 hook token（deploy scope）调用。compose 字节
// 由服务端从 bare 仓库自取（真源在 git 对象库，不信任客户端传字节）；sha
// 为 40 位 commit（protovalidate 形状 + 服务端十六进制复核）。审计在源端
// 实现（git.push_deploy，actor=system + 钩子 token id）。
func (s *DeploymentsService) DeployFromGit(ctx context.Context, req *serverv1.DeployFromGitRequest) (*serverv1.DeployFromGitResponse, error) {
	if s.git == nil {
		return nil, statusEnvelope(codes.Internal, "git deploy source not configured")
	}
	if !gitSHAValid(req.GetSha()) {
		return nil, statusInvalidArgument("sha must be 40 hex chars")
	}
	// 角色门（W2-S4 第 2 门）：app 行在册时按行上归属门 deploy 层级（git
	// push 钩子经 PAT 回调的用户面）；行不在册（首发）由 gitserver 的建行
	// 路径承载归属解析（E_APP_PROJECT_REQUIRED 指引）——角色门在行落位后
	// 的后续部署自然生效。
	if app, aerr := resolveApp(ctx, s.st, req.GetApp()); aerr == nil {
		if err := requireAppAccess(ctx, s.st, app); err != nil {
			return nil, err
		}
	} else if !isNotFoundErr(aerr) {
		return nil, aerr
	}
	rec, warnings, err := s.git.DeployFromGitPush(ctx, req.GetApp(), req.GetSha(), req.GetRef(), callerTokenID(ctx), req.GetPushUser())
	if err != nil {
		// 分支未跟踪（H2）：非错误终局——skipped 回执（deployment_id 留空；
		// status 是自由字符串字段，钩子脚本把响应 JSON 打到 pusher stderr，
		// 用户可读「推送被接受但不触发部署」），并写处置审计。
		if errors.Is(err, gitserver.ErrBranchNotTracked) {
			s.auditGitIgnoredBranch(ctx, req.GetApp(), req.GetRef())
			return &serverv1.DeployFromGitResponse{App: req.GetApp(), Status: "skipped"}, nil
		}
		return nil, err // apperr（E_COMPOSE_*）原样透传；其余按信封退化
	}
	return &serverv1.DeployFromGitResponse{
		DeploymentId: rec.ID,
		App:          rec.AppName,
		Status:       string(rec.Status),
		Warnings:     composeWarnings(warnings),
	}, nil
}

// auditGitIgnoredBranch 写 SSH push 分支未跟踪的处置审计（action 词根
// git.ignored_branch——与 webhook 侧 ignored_branch outcome 同语义；机器
// 动作 actor=system，ActorTokenID 记钩子回调 token）。target 优先 app 行
// id；app 行不存在（首次 push 即非配置分支）退化为名形态。审计失败不阻断
// skipped 回执——跳过决策已定，审计是观测面（webhook 侧审计失败同口径
// 降级；本服务无 logger，静默吞掉）。
func (s *DeploymentsService) auditGitIgnoredBranch(ctx context.Context, appName, ref string) {
	target := "app:" + appName
	if appRow, err := s.st.GetAppByName(ctx, appName); err == nil {
		target = "app:" + appRow.ID
	}
	_ = s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:        "system",
			ActorTokenID: callerTokenID(ctx),
			Action:       "git.ignored_branch",
			Target:       target,
			Result:       "ok",
			DiffSummary:  state.DiffSummary("app", appName, "ref", ref, "outcome", "ignored_branch"), // B4：构造器替换手拼 JSON
		})
	})
}

// gitSHAValid 复核 commit 形态（40 位小写十六进制；protovalidate 只约束
// 长度）。
func gitSHAValid(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		if !isDigit && !isLowerHex {
			return false
		}
	}
	return true
}

// CancelDeployment 置位取消（受限语义：未切流可取消；曾健康/终态 409
// ——引擎侧二次校验为权威）。
func (s *DeploymentsService) CancelDeployment(ctx context.Context, req *serverv1.CancelDeploymentRequest) (*serverv1.CancelDeploymentResponse, error) {
	rec, err := s.st.GetDeployment(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	if !rec.FirstHealthyAt.IsZero() || rec.Status == state.DeployObserving || rec.Status.Terminal() {
		return nil, apperrConflict(rec)
	}
	set := true
	if err := s.st.UpdateDeployment(ctx, rec.ID, state.DeploymentPatch{CancelRequested: &set}); err != nil {
		return nil, err
	}
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, auditEntry(ctx, "deployment:"+rec.ID, state.DiffSummary("cancel_requested", true, "source", "api"))) // B4：构造器替换手拼 JSON
	}); err != nil {
		return nil, err
	}
	return &serverv1.CancelDeploymentResponse{Id: rec.ID, Status: string(rec.Status)}, nil
}

// RollbackDeployment 回滚入队（engine.EnqueueRollback：目标解析失败
// E_ROLLBACK_NO_TARGET；事件/审计在引擎入队路径内置）。
func (s *DeploymentsService) RollbackDeployment(ctx context.Context, req *serverv1.RollbackDeploymentRequest) (*serverv1.RollbackDeploymentResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4）：回滚 = developer+。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rec, err := engine.EnqueueRollback(ctx, s.st, engine.RollbackInput{
		// 引擎按 GetAppByName 重解析（不识别引用形态）：传解析后的三段限
		// 定形——原始引用（路由参数 = 平台 id）透传使既有应用的回滚恒
		// E_ROLLBACK_NO_TARGET 且文案裸显平台 id（2026-09-25 teamadmin 走
		// 查实录「app <ULID> does not exist」）；业务裸名在跨项目同名时撞
		// ErrAppAmbiguous（E_RUNTIME_UNAVAILABLE 500）。限定形精确命中，
		// 目标解析失败的文案亦不复现平台 id（与 Deploy A1 修法同源，取
		// 限定形而非裸业务名的理由见 drift.go 头注）。
		AppName:          app.QualifiedName(),
		TargetRevisionID: req.GetTargetRevisionId(),
		Actor:            "human",
	})
	if err != nil {
		return nil, err // apperr（E_ROLLBACK_NO_TARGET 等）原样透传
	}
	return &serverv1.RollbackDeploymentResponse{
		DeploymentId: rec.ID,
		App:          req.GetApp(),
		Status:       string(rec.Status),
	}, nil
}

// apperrConflict 构造「已切流不可 cancel」的 409（与 CLI 同文案口径，
// 走 E_STATE_VERSION_CONFLICT 注册码——语义匹配，不新增码）。
func apperrConflict(rec state.DeployRecord) error {
	return apperr.New("E_STATE_VERSION_CONFLICT",
		"deployment %s has already switched flow or reached a terminal state (%s) and cannot be cancelled: use rollback instead if it was healthy", rec.ID, rec.Status).
		WithContext("deployment", rec.ID).
		WithContext("reason", "already_switched")
}

// ensureApp 取应用行；不存在则创建（部署常是应用的第一个平台动作，与
// CLI 同语义：应用随首次部署自动创建）。v0.3 W2-S3 归属管道（rbac-teams
// §3.4 D-W0-4 二修）：
//   - 按（解析项目，名字）精确查行——命中即归属一致，沿用行；
//   - 目标项目内无此名 → **在目标项目新建**。名字在其他项目存在 ≠ 冲突
//     （project 内唯一 = 同名跨项目是两个不同应用——「prod 与 dev 各有
//     demo」是设计能力）。staging 真机演练（2026-09-24 SV-11b）抓出 S3
//     的 409 E_APP_PROJECT_MISMATCH 偏差裁决与此对齐：调用方上下文漂移
//     的防呆由 deploy 请求的显式 project 字段承载（机具必须显式；用户
//     缺省=个人队 default——显式传错项目属调用方错误，产品不越权拦截）。
func ensureApp(ctx context.Context, st *state.Store, name string, proj state.Project) (state.App, error) {
	app, err := st.GetAppByNameInProject(ctx, proj.ID, name)
	if err == nil {
		return app, nil
	}
	if !errors.Is(err, state.ErrAppNotFound) {
		return state.App{}, mapStoreErr(err, name)
	}
	created, err := st.CreateApp(ctx, "", name, proj.ID, proj.TeamID)
	if err != nil {
		return state.App{}, err
	}
	return created, nil
}
