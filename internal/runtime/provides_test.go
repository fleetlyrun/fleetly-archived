package runtime

// MG-4（X-3，B6）：NewServices 停止顺序不变量的结构断言。背景：lynx 把
// 每个服务登记为 oklog/run actor，关停时 run.Group **按注册顺序**逐个
// interrupt，且 lynx 的 interrupt 包壳阻塞在服务 Stop 上——注册序即停止
// 序。旧形态把 HTTP/gRPC/git 排在切片最后 → SIGTERM 后入口面最后才停，
// 窗口期内 Deploy/TriggerBuild RPC 仍「假成功」入队而引擎/队列已死。
// 本测试直接断言返回切片的顺序：入口组在最前、写入者居中、store 类
// 资源殿后。lynx 集成级 SIGTERM 端到端测试成本过高不做（挂账见
// NewServices 注释）。

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	lynxhttp "github.com/lynx-go/lynx/server/http"

	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/cron"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/gitserver"
	"github.com/fleetlyrun/fleetly/internal/ingress"
	"github.com/fleetlyrun/fleetly/internal/logs"
	"github.com/fleetlyrun/fleetly/internal/notify"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/statebackup"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

// captureLynxApp 经真实 Runner 交接一个 lynx.App（NewServices 的参数面需要；
// app 只被读取 Logger——不注册服务、不进入 Run 主循环，交接后立即 Close）。
// os.Args/CWD 隔离同 TestStateServicesBlockUntilShutdown 的理由：Runner
// 构造期解析进程参数并做 CWD 配置发现，go test 参数会污染其行为。
func captureLynxApp(t *testing.T) lynx.App {
	t.Helper()
	savedArgs, savedWd := os.Args, ""
	if wd, err := os.Getwd(); err == nil {
		savedWd = wd
	}
	os.Args = []string{"fleetlyd-services-order-test"}
	_ = os.Chdir(t.TempDir())
	t.Cleanup(func() {
		os.Args = savedArgs
		if savedWd != "" {
			_ = os.Chdir(savedWd)
		}
	})
	appCh := make(chan lynx.App, 1)
	runner := lynx.NewRunner(func(a lynx.App) error {
		appCh <- a
		return nil
	}, lynx.WithName("fleetlyd-services-order-test"))
	runErr := make(chan error, 1)
	go func() { runErr <- runner.RunE() }()
	var app lynx.App
	select {
	case app = <-appCh:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not hand over app within 5s")
	}
	app.Close() // 立即收口：本测试不跑主循环
	select {
	case <-runErr:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not exit after Close")
	}
	return app
}

// TestNewServicesStopOrder 结构断言：返回切片的三段顺序 = 入口面（最前）
// → 写入者 → 资源层（store 最后）。注册序即停止序（oklog/run 按注册顺序
// interrupt + lynx interrupt 阻塞在 Stop——见 NewServices 注释），此断言
// 即停止不变量的可执行形态。
func TestNewServicesStopOrder(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := state.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "fleetly.key"))
	if err != nil {
		t.Fatalf("ensure key: %v", err)
	}
	sc, err := substrate.NewClient("tcp://127.0.0.1:1") // 惰性客户端：构造期不触底座
	if err != nil {
		t.Fatalf("substrate client: %v", err)
	}
	defer func() { _ = sc.Close() }()
	id := state.NewNodeIdentity(st, noopDocker{}, logger)
	ob := state.NewObserver(st, noopDocker{}, logger)
	jr := state.NewJanitor(st, state.JanitorConfig{}, logger)
	bm, err := statebackup.NewManager(statebackup.Config{},
		filepath.Join(dir, "backups"), box.Path(), "test", st, logger)
	if err != nil {
		t.Fatalf("backup manager: %v", err)
	}
	builder := build.NewBuilder(build.Config{}, st, sc, sc, logger)
	q := build.NewQueue(st, builder, 1, time.Second, time.Minute, logger)
	ing, _, err := ingress.NewManager(ingress.Config{}, st, logger)
	if err != nil {
		t.Fatalf("ingress manager: %v", err)
	}
	app := captureLynxApp(t)
	eng := engine.NewEngine(engine.Config{}, st, sc, sc,
		placement.NewResolver(st, sc), box, app.Logger())
	src := gitserver.NewGitTriggers(gitserver.Config{}, st, box, logger)
	lm := logs.NewManager(logs.Config{}, st, sc, box, logger)
	cm := cron.NewManager(cron.Config{}, st, box, sc, placement.NewResolver(st, sc), logger)
	nm := notify.NewManager(st, box, notify.Config{}, logger)
	cfg := &AppConfig{}
	hs := lynxhttp.NewServer(http.NotFoundHandler(), lynxhttp.WithAddr("127.0.0.1:0"))
	gs := lynxgrpc.NewServer(lynxgrpc.WithAddr("127.0.0.1:0"))

	services := NewServices(app, st, id, ob, jr, bm, box, q, builder, eng,
		ing, lm, cm, nm, src, cfg, nil, nil, nil, nil, nil, hs, gs)
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Name())
	}

	// 三段不变量（段内相对序同断言——入口组内 http→grpc→git，资源组内
	// store 恒最后、backup 晚于 engine）。
	want := []string{
		// 第一段：入口面（最先停——SIGTERM 后立即拒绝新工作）。
		// lynx 服务壳名：http/grpc（lynx 内建），git.webhook。
		"http", "grpc", "git.webhook",
		// 第二段：写入者（入口关后排空在途）。
		"build.queue", "engine.release", "ingress.traefik", "logs.collector", "notify.webhook",
		// 第三段：资源层（最后停；backup 晚于 engine 等 post-deploy
		// 在途快照，rustfs/victorialogs/metrics/database/execrelay 收敛
		// duty、cron 调度器、通知投递器同层，store 殿后）。
		"state.identity", "state.observer", "state.janitor",
		"state.backup", "objectstore.rustfs", "logs.victorialogs", "metrics.stack", "database.converge", "execrelay", "cron.schedule", "state.secrets", "state.store",
	}
	if len(names) != len(want) {
		t.Fatalf("services = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("service[%d] = %s, want %s（full order: %v）", i, names[i], want[i], names)
		}
	}
}
