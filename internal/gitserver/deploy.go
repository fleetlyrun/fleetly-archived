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

// ErrBranchNotTracked 是分支过滤哨兵（H2：daemon 侧权威过滤）：投递 ref
// 非该 app 配置分支（空回落缺省 main）时 DeployFromCommit 不建部署、返回
// 它。webhook 受理侧 ServeHTTP 已同词形预过滤（ignored 回执），此处是
// 纵深防御——受理与执行分离（D1）后两步之间配置分支可能被改。
var ErrBranchNotTracked = errors.New("branch is not tracked")

// DeployFromCommit 是 webhook 入队点：compose 字节由服务端从 bare 仓库
// `git show <sha>:compose.{yaml,yml}` 自取（真源在 git 对象库，不信任客户
// 端传字节），落临时文件走受控子集校验（与 API Deploy 同路径）后入队部
// 署，deployments.source_git_* 记录来源。
//
// 幂等口径（绑定）：(app, sha) 去重恒开——受理侧预查在 ServeHTTP、竞态
// 兜底在本函数入队事务内（M3-4：COUNT 与 INSERT 同事务，命中返回
// state.ErrDuplicateGitDeployment）。分支过滤（投递到配置分支才触发部署）
// 在 daemon 侧权威承载——见 checkBranchTracked。事件流复用既有
// deployment.queued；审计 action 恒 git.webhook_deploy、actor 恒 system
// （机器动作）。
func (s *GitTriggers) DeployFromCommit(ctx context.Context, app, sha, ref string) (state.DeployRecord, []compose.Warning, error) {
	if !ValidAppName(app) {
		return state.DeployRecord{}, nil, fmt.Errorf("gitserver: invalid app name %q", app)
	}
	if !ValidSHA(sha) {
		return state.DeployRecord{}, nil, fmt.Errorf("gitserver: invalid sha %q", sha)
	}
	// 分支过滤（H2，daemon 侧权威——受理侧预查与执行间配置可变，纵深
	// 防御）。置于 EnsureBareRepo 之前——被跳过的投递不产生任何副作用
	// （不建仓库、不建部署行）。
	if err := s.checkBranchTracked(ctx, app, ref); err != nil {
		return state.DeployRecord{}, nil, err
	}
	if _, _, err := s.EnsureBareRepo(ctx, app); err != nil {
		return state.DeployRecord{}, nil, err
	}
	composeBytes, err := s.composeFromCommit(ctx, app, sha)
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
	if spec.Name != app {
		return state.DeployRecord{}, nil, apperr.New("E_COMPOSE_UNSUPPORTED",
			"compose name %q does not match the repo app name %q: the repo path is the app identity; align the compose name field",
			spec.Name, app)
	}

	// 应用行：webhook 是既有 app 的触发通道（签名密钥随 app 行配置，app
	// 不存在则验签已挡），此处仅取行——防御分支（受理与执行间 app 被删）
	// 按不存在如实拒绝。
	appRow, err := s.st.GetAppByName(ctx, app)
	if err != nil {
		if errors.Is(err, state.ErrAppNotFound) {
			return state.DeployRecord{}, nil, fmt.Errorf("gitserver: app %q does not exist", app)
		}
		return state.DeployRecord{}, nil, err
	}
	// 挂起门（app Stop/Start，00028 位）：挂起期排水到 0 是期望形态——
	// webhook 触发的部署不入队（排水分支会立即拉回 0）。恢复先行
	//（resume 清位 + 自带重部署）；拒绝经 rejected 审计如实披露。
	if appRow.Suspended {
		return state.DeployRecord{}, nil, apperr.New("E_APP_SUSPENDED",
			"app %s is suspended — resume it before deploying (Console: Start; resume redeploys the active revision)", appRow.Name).
			WithContext("app", appRow.Name)
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
	var rec state.DeployRecord
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		// M3-4：(app, sha) 幂等复查并入本事务（受理侧 ServeHTTP 的预查
		// 是快路径，此处闭合 check-then-insert 竞态——并发重投两路都过了
		// 预查时，写锁串行化下后到事务在此判重）。各写事务 BEGIN
		// IMMEDIATE 起手（见 state dsn），COUNT 与 INSERT 同事务即原子。
		n, err := tx.CountGitDeploymentsForSHA(ctx, appRow.ID, sha)
		if err != nil {
			return err
		}
		if n > 0 {
			return state.ErrDuplicateGitDeployment
		}
		r, err := tx.CreateDeployment(ctx, state.DeployRecord{
			ID:           deployID,
			AppID:        appRow.ID,
			AppName:      spec.Name,
			Kind:         "deploy",
			SpecHash:     spec.SpecHash,
			ComposePath:  persistPath,
			SourceGitSHA: sha,
			SourceGitRef: ref,
		})
		if err != nil {
			return err
		}
		rec = r
		if _, err := tx.AppendEvent(ctx, state.Event{
			Name:    "deployment.queued",
			Subject: "deployment:" + rec.ID,
			// B4：经 state.DiffSummary 构造（json.Marshal 转义）。
			Payload: state.DiffSummary("deployment", rec.ID, "app", spec.Name, "source", "webhook"),
		}); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			// actor 恒 system：webhook 是机器动作（署名用户语义随 git push
			// 面移除）。
			Actor:       "system",
			Action:      "git.webhook_deploy",
			Target:      "deployment:" + rec.ID,
			Result:      "ok",
			DiffSummary: state.DiffSummary("app", spec.Name, "sha", sha, "ref", ref, "via", "webhook"), // B4：构造器替换手拼 JSON
		})
	})
	if err != nil {
		return state.DeployRecord{}, nil, err
	}
	s.log.Info("gitserver: deployment enqueued from git webhook",
		"app", app, "sha", sha, "ref", ref, "deployment", rec.ID)
	return rec, warnings, nil
}

// checkBranchTracked 是分支过滤的权威谓词（MG-C1）：加载 app 的 git 配置
// 分支（与 webhook.go 同口径——GetAppGitConfig 对空分支回落
// state.DefaultGitBranch）；app 行不存在 = 首次配置前置形态，按缺省分支
// 处理。ref 命中 refs/heads/<branch> 才放行，其余（含 refs/tags/*、空
// 引用等非分支词形）返回 ErrBranchNotTracked。
func (s *GitTriggers) checkBranchTracked(ctx context.Context, app, ref string) error {
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
