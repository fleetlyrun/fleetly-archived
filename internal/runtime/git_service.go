package runtime

// git webhook 入口的服务装配壳（T2.19）：Init 阶段做配置完整性 fail-fast
// （Validate），Stop 等待 webhook 后台 worker 排空。webhook 的 HTTP 入口
// 不走本服务——它是 gateway 原生 handler（见 gateway.go 的原生端点例外清
// 单），随 HTTP 面启停；受理后的后台 worker（D1，S17 类 D）挂本服务生命
// 周期（见 Start/Stop）。（git push(SSH) 收包面 2026-09-29 移除，ADR-0012
// ——本壳由 SSH 监听壳收敛为 worker 生命周期壳。）

import (
	"context"

	"github.com/lynx-go/lynx"

	"github.com/fleetlyrun/fleetly/internal/gitserver"
)

// gitService 是 git webhook 面服务壳；D1（S17 类 D）起承载 webhook 后台
// worker 的生命周期（Start 启动、Stop 排空退出）。
type gitService struct {
	src     *gitserver.GitTriggers
	started chan struct{}
}

func newGitService(src *gitserver.GitTriggers) lynx.Service {
	return &gitService{
		src:     src,
		started: make(chan struct{}),
	}
}

func (s *gitService) Name() string { return "git.webhook" }

// Init 配置完整性 fail-fast（Root 非空）。
func (s *gitService) Init(_ lynx.AppContext) error {
	return s.src.Config().Validate()
}

// Start 启动 webhook 后台 worker 后驻留至 ctx 取消。
func (s *gitService) Start(ctx context.Context) error {
	// D1：webhook 后台 worker 随本服务生命周期启动（幂等；ctx 取消进入
	// 排空，StopWebhookWorker 是等待点）。
	s.src.StartWebhookWorker(ctx)
	close(s.started)
	<-ctx.Done()
	return nil
}

// Stop 等待 Start 收口与 webhook worker 排空退出（在处理项的 per-item
// 预算独立于取消，排空不打断；等待本身受 Stop ctx 时限约束，超时让位给
// lynx 停机时限，进程退出兜底）。X-6/MG-4：排空预算尽（webhookDrainBudget，
// 3s < lynx StopTimeout 5s）时队列剩余项在 worker 侧落披露审计
// （app.webhook_interrupted）后丢弃——停机丢失可对账、可手动 redeliver。
func (s *gitService) Stop(ctx context.Context) error {
	<-s.started
	return s.src.StopWebhookWorker(ctx)
}

// CheckHealth 委托核心（配置完整性 + state 可达）。
func (s *gitService) CheckHealth() error {
	return s.src.CheckHealth()
}
