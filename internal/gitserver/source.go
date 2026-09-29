package gitserver

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// D1（S17 类 D）：webhook 受理与执行分离——GitTriggers 额外承载 webhook
// 异步执行面（带界队列 + 单 worker，webhook_worker.go），生命周期挂
// git 服务壳（StartWebhookWorker/StopWebhookWorker）。

// GitTriggers 是 git 触发入口的门面（命名口径 UBIQUITOUS_LANGUAGE §flagged-2：
// 承载触发面+拉源+部署入队，不只是一个「来源」，故弃 Source 旧名）：
// bare 仓库管理、DeployFromCommit 入队、webhook 验签/防重放/去重/拉源。
// 它聚合 state 层（apps/deployments）与平台密钥盒（webhook secret / 拉源
// 认证材料的 envelope 加解密），对上暴露：
//
//   - HTTP：WebhookHandler（gateway 原生端点例外清单挂载）
type GitTriggers struct {
	cfg    Config
	st     *state.Store
	box    *secrets.Box
	log    *slog.Logger
	replay *deliveryCache
	// fetchFn 是拉源执行步骤的注入缝（NewGitTriggers 恒设为 runFetch；测试替换后可
	// 不真实触网驱动 TOFU 审计链——与 statebackup.Manager.verifyFn 同款
	// 接缝形态）。
	fetchFn func(ctx context.Context, plan fetchPlan) error
	// hooks 是 webhook 异步执行面（D1，S17 类 D）：受理队列 + 单 worker，
	// 见 webhook_worker.go。
	hooks webhookRunner
	// repoMu 保护 repoLocks；repoLocks 是 per-app 的仓库写入互斥
	// （E7③，S19：EnsureBareRepo 的 stat→init 存在 TOCTOU——并发同 app
	// 的 webhook 拉源交错可产生半初始化仓库）。
	repoMu    sync.Mutex
	repoLocks map[string]*sync.Mutex
}

// NewGitTriggers 构造 GitTriggers（cfg 先经 Normalize；完整性 Validate 由
// 装配点在 Init 阶段 fail-fast）。
func NewGitTriggers(cfg Config, st *state.Store, box *secrets.Box, log *slog.Logger) *GitTriggers {
	norm := cfg.Normalize()
	if log == nil {
		log = slog.New(discardHandler{})
	}
	src := &GitTriggers{
		cfg:       norm,
		st:        st,
		box:       box,
		log:       log,
		replay:    newDeliveryCache(norm.ReplayTTL, time.Now),
		fetchFn:   runFetch,
		repoLocks: map[string]*sync.Mutex{}, // E7③：per-app 分段锁
	}
	src.hooks.init() // D1：webhook 受理队列（worker 由服务壳 Start 启动）
	return src
}

// Config 返回归一后的配置（只读投影）。
func (s *GitTriggers) Config() Config { return s.cfg }

// CheckHealth 报告 git 入口层健康：配置完整性 + state 可达。
func (s *GitTriggers) CheckHealth() error {
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	return s.st.CheckHealth()
}

// discardHandler 兜底空日志（理论不可达：装配点恒注入）。
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
