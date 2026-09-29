package gitserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ErrBranchNotTracked 是分支过滤哨兵（H2：daemon 侧权威过滤）：push 的
// ref 非该 app 配置分支（空回落缺省 main）时 DeployFromCommit 不建部署、
// 返回它；api 面据此映射 skipped 回执，与 webhook 入口的 ignored 语义
// 对齐（webhook 路径在上游已同词形预过滤，不会命中）。
var ErrBranchNotTracked = errors.New("branch is not tracked")

// DeployFromCommit 是两条 git 触发入口的汇合点：compose 字节由服务端从
// bare 仓库 `git show <sha>:compose.{yaml,yml}` 自取（真源在 git 对象库，
// 不信任客户端传字节），落临时文件走受控子集校验（与 API Deploy 同路径）
// 后入队部署，deployments.source_git_* 记录来源。
//
// 幂等口径（绑定）：git push 是显式用户动作——每次调用都建部署记录（引擎
// 对同 spec 重放安全，Spike B2）；(app, sha) 去重仅属 webhook 入口，受理侧
// 预查在 ServeHTTP、竞态兜底在本函数入队事务内（M3-4：DedupeSHA 置位时
// COUNT 与 INSERT 同事务，命中返回 state.ErrDuplicateGitDeployment）。分支
// 过滤（push 到配置分支才触发部署）在 daemon 侧权威承载——见
// checkBranchTracked。事件流复用既有 deployment.queued；审计 action
// 由入参区分（git.push_deploy / git.webhook_deploy），actor 恒 system
// （机器动作）。
type DeployInput struct {
	// App 是仓库对应的 app 名（compose name 与它不一致 → 拒绝——仓库
	// 路径即应用身份，跨名部署会被静默错置）。
	App string
	// SHA 是 40 位 commit。
	SHA string
	// Ref 是来源引用（refs/heads/<branch>；非配置分支在入口被过滤——
	// ErrBranchNotTracked，落库值恒为配置分支）。
	Ref string
	// AuditAction 是部署入队审计 action 词根。
	AuditAction string
	// ActorTokenID 是入队调用方 token（钩子 token id；webhook 进程内路径
	// 为空）。
	ActorTokenID string
	// DedupeSHA 置位时入队事务内做 (app, sha) 幂等复查（M3-4）：该 sha
	// 已有 enqueued/active/succeeded 部署 → 整笔回滚返回
	// state.ErrDuplicateGitDeployment。仅 webhook 入口置位（受理侧的
	// check-then-insert 竞态在此闭合）；SSH push 路径恒不置位——git push
	// 是显式用户动作，每次调用都建部署（幂等口径绑定，见上）。
	DedupeSHA bool
	// PushUser 是 push 署名用户（W2 §2.3 审计 actor 联动）：SSH 公钥认证
	// 回调按指纹命中的 git_keys.user_id 经钩子环境变量透传而来；空 =
	// 存量无主键/缺省（审计 actor 落 system 原口径）。非空时审计 actor =
	// user:<id>（设计 §6 用户操作署名）。webhook 入口恒空（机器动作）。
	PushUser string
}

// DeployFromCommit 执行读源 → 校验 → 入队；返回 queued 部署记录与校验
// 期警告（非阻断标注，与 API Deploy 同面）。compose 违约（受控子集之外）
// 经 compose.Load 原样透传 E_COMPOSE_* 信封。
func (s *GitTriggers) DeployFromCommit(ctx context.Context, in DeployInput) (state.DeployRecord, []compose.Warning, error) {
	if !ValidAppName(in.App) {
		return state.DeployRecord{}, nil, fmt.Errorf("gitserver: invalid app name %q", in.App)
	}
	if !ValidSHA(in.SHA) {
		return state.DeployRecord{}, nil, fmt.Errorf("gitserver: invalid sha %q", in.SHA)
	}
	// 分支过滤（H2，daemon 侧权威——钩子注释承诺的过滤点在此）：SSH push
	// 路径由此获得与 webhook 一致的语义；webhook 路径上游已同词形预过滤，
	// 此处恒放行（现有行为不变）。置于 EnsureBareRepo 之前——被跳过的
	// push 不产生任何副作用（不建仓库、不建部署行）。
	if err := s.checkBranchTracked(ctx, in.App, in.Ref); err != nil {
		return state.DeployRecord{}, nil, err
	}
	if _, _, err := s.EnsureBareRepo(ctx, in.App); err != nil {
		return state.DeployRecord{}, nil, err
	}
	composeBytes, err := s.composeFromCommit(ctx, in.App, in.SHA)
	if err != nil {
		return state.DeployRecord{}, nil, composeReject(err)
	}

	// compose 落临时文件走受控子集校验（A7 起 temp 仅解析中转——持久化
	// 副本在 <数据根>/deployments/<id>/compose.yaml，见下）。
	dir, err := os.MkdirTemp("", "fleetly-compose-")
	if err != nil {
		return state.DeployRecord{}, nil, fmt.Errorf("gitserver: create compose temp dir: %w", err)
	}
	// MG-6：解析中转目录随请求回收——持久化副本已另落 <数据根>/deployments/
	// <id>/compose.yaml，本目录不存活到函数外，不留孤儿 tmp。
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, composeBytes, 0o600); err != nil { //nolint:gosec // G306：compose 内容非密钥，0600 保守
		return state.DeployRecord{}, nil, fmt.Errorf("gitserver: write compose temp file: %w", err)
	}
	spec, warnings, err := compose.Load(ctx, path)
	if err != nil {
		return state.DeployRecord{}, nil, err // apperr（E_COMPOSE_*）原样透传
	}
	if spec.Name != in.App {
		return state.DeployRecord{}, nil, apperr.New("E_COMPOSE_UNSUPPORTED",
			"compose name %q does not match the repo app name %q: the repo path is the app identity; align the compose name field",
			spec.Name, in.App)
	}

	// 应用行（首次 push = 应用的第一个平台动作，随部署自动创建）。v0.3
	// 归属管道：首建行以 push 署名用户解析缺省项目（ensureAppRow 注）。
	app, err := ensureAppRow(ctx, s.st, spec.Name, in.PushUser)
	if err != nil {
		return state.DeployRecord{}, nil, err
	}
	// 挂起门（app Stop/Start，00028 位）：挂起期排水到 0 是期望形态——
	// push/webhook 触发的部署不入队（排水分支会立即拉回 0）。恢复先行
	//（resume 清位 + 自带重部署）；拒绝随 push 报错如实上抛给推送方。
	if app.Suspended {
		return state.DeployRecord{}, nil, apperr.New("E_APP_SUSPENDED",
			"app %s is suspended — resume it before deploying (Console: Start; resume redeploys the active revision)", app.Name).
			WithContext("app", app.Name)
	}
	// A7（S18）：compose 字节持久化 <数据根>/deployments/<id>/compose.yaml
	//（先写文件后建行；临时文件自此仅解析中转——tmpfiles 清理不再影响
	// 引擎 preparing 与成功固化的重载；终态后由 janitor 按 30 天窗清理）。
	deployID := ulid.Make().String()
	persistPath, err := state.PersistDeploymentCompose(
		state.DeploymentsRoot(s.st.Path()), deployID, composeBytes)
	if err != nil {
		return state.DeployRecord{}, nil, err
	}
	// fail-closed 单事务（H13 修复，state-model §2.9）：部署行、
	// deployment.queued 事件与审计在同一个 InTx 内写入——回调内任一失败
	//（含事件/审计写失败）整体回滚、部署行不落库，杜绝「引擎照常执行但
	// 事件与审计缺失」的审计黑洞。回调内拿到的 rec（含生成的 ID/时间戳）
	// 供事件 payload 与审计使用。
	via := "git_push"
	if in.AuditAction == "git.webhook_deploy" {
		via = "webhook"
	}
	var rec state.DeployRecord
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		// M3-4：webhook 入口的 (app, sha) 幂等复查并入本事务（受理侧
		// ServeHTTP 的预查是快路径，此处闭合 check-then-insert 竞态——
		// 并发重投两路都过了预查时，写锁串行化下后到事务在此判重）。
		// 各写事务 BEGIN IMMEDIATE 起手（见 state dsn），COUNT 与 INSERT
		// 同事务即原子。SSH push 路径不置位，行为不变。
		if in.DedupeSHA {
			n, err := tx.CountGitDeploymentsForSHA(ctx, app.ID, in.SHA)
			if err != nil {
				return err
			}
			if n > 0 {
				return state.ErrDuplicateGitDeployment
			}
		}
		r, err := tx.CreateDeployment(ctx, state.DeployRecord{
			ID:           deployID,
			AppID:        app.ID,
			AppName:      spec.Name,
			Kind:         "deploy",
			SpecHash:     spec.SpecHash,
			ComposePath:  persistPath,
			SourceGitSHA: in.SHA,
			SourceGitRef: in.Ref,
		})
		if err != nil {
			return err
		}
		rec = r
		if _, err := tx.AppendEvent(ctx, state.Event{
			Name:    "deployment.queued",
			Subject: "deployment:" + rec.ID,
			// B4：经 state.DiffSummary 构造（json.Marshal 转义）。
			Payload: state.DiffSummary("deployment", rec.ID, "app", spec.Name, "source", via),
		}); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			// push 审计 actor（W2 §2.3）：署名用户非空 = user:<id>（公钥
			// 认证回调解析的属主）；空 = system 原口径（机器动作——webhook
			// 入口与存量无主 key）。
			Actor:        pushAuditActor(in.PushUser),
			ActorTokenID: in.ActorTokenID,
			Action:       in.AuditAction,
			Target:       "deployment:" + rec.ID,
			Result:       "ok",
			DiffSummary:  state.DiffSummary("app", spec.Name, "sha", in.SHA, "ref", in.Ref, "via", via), // B4：构造器替换手拼 JSON
		})
	})
	if err != nil {
		return state.DeployRecord{}, nil, err
	}
	s.log.Info("gitserver: deployment enqueued from git",
		"app", in.App, "sha", in.SHA, "ref", in.Ref, "deployment", rec.ID, "via", via)
	return rec, warnings, nil
}

// DeployFromGitPush 是 DeployFromGit RPC 的端口实现（git push 路径；每次
// push 都建部署记录）。actorTokenID 记录钩子回调 token（可追溯）；pushUser
// 是 SSH 公钥认证回调解析的署名用户（空 = 存量无主 key，审计落原口径）。
func (s *GitTriggers) DeployFromGitPush(ctx context.Context, app, sha, ref, actorTokenID, pushUser string) (state.DeployRecord, []compose.Warning, error) {
	return s.DeployFromCommit(ctx, DeployInput{
		App:          app,
		SHA:          sha,
		Ref:          ref,
		AuditAction:  "git.push_deploy",
		ActorTokenID: actorTokenID,
		PushUser:     pushUser,
	})
}

// pushAuditActor 是 push 审计主体归一（W2 §2.3）：署名用户非空 = user:<id>
// （设计 §6 用户操作署名）；空 = system 原口径（机器动作）。
func pushAuditActor(pushUser string) string {
	if pushUser == "" {
		return "system"
	}
	return "user:" + pushUser
}

// checkBranchTracked 是分支过滤的权威谓词（MG-C1）：加载 app 的 git 配置
// 分支（与 webhook.go 同口径——GetAppGitConfig 对空分支回落
// state.DefaultGitBranch）；app 行不存在 = 首次 push 前置形态（应用行随
// 首次部署创建），按缺省分支处理。ref 为空 = 端口调用方未携带引用形态，
// 不做比对（兼容进程内直连夹具）；ref 命中 refs/heads/<branch> 才放行，
// 其余（含 refs/tags/* 等非分支引用）返回 ErrBranchNotTracked。
func (s *GitTriggers) checkBranchTracked(ctx context.Context, app, ref string) error {
	if ref == "" {
		return nil
	}
	branch := state.DefaultGitBranch
	if appRow, err := s.st.GetAppByName(ctx, app); err == nil {
		cfg, err := s.st.GetAppGitConfig(ctx, appRow.ID)
		if err != nil {
			return err
		}
		branch = cfg.Branch // GetAppGitConfig 已做空分支回落
	} else if !errors.Is(err, state.ErrAppNotFound) {
		return err
	}
	if ref != "refs/heads/"+branch {
		return ErrBranchNotTracked
	}
	return nil
}

// composeReject 把 composeFromCommit 的哨兵错误映射为 E_COMPOSE_UNSUPPORTED
// 信封（errcode 零新增——compose 族复用；message 携带 git 侧原文）。
func composeReject(err error) error {
	if errors.Is(err, errComposeRejected) {
		return apperr.New("E_COMPOSE_UNSUPPORTED", "%s", err.Error())
	}
	return err
}

// ensureAppRow 取应用行；不存在则创建（与 api 包 ensureApp 同语义——本包
// 自持一份，避免反向依赖 api）。v0.3 W2-S3 归属管道：首次 push 建行时以
// push 署名用户的个人队默认项目落归属（与 API Deploy 的用户缺省同口径，
// state.ResolveUserDefaultProject 单点）；无署名用户（存量无主键/机器
// push）→ 拒绝并指引：先经 CLI/API 携带 project 首发建行，再 push。
func ensureAppRow(ctx context.Context, st *state.Store, name, pushUserID string) (state.App, error) {
	app, err := st.GetAppByName(ctx, name)
	if err == nil {
		return app, nil
	}
	if !errors.Is(err, state.ErrAppNotFound) {
		return state.App{}, err
	}
	if pushUserID == "" {
		return state.App{}, apperr.New("E_APP_PROJECT_REQUIRED",
			"app %q does not exist yet and the push carries no signed user; deploy once via CLI/API with a project to create it, then push", name).
			WithContext("app", name)
	}
	proj, err := st.ResolveUserDefaultProject(ctx, pushUserID)
	if err != nil {
		return state.App{}, apperr.New("E_APP_PROJECT_REQUIRED",
			"app %q does not exist yet and no default project is resolvable for the push user; deploy once via CLI/API with a project to create it, then push", name).
			WithContext("app", name)
	}
	created, err := st.CreateApp(ctx, "", name, proj.ID, proj.TeamID)
	if err != nil {
		return state.App{}, err
	}
	return created, nil
}
