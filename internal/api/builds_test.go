package api

// TriggerBuild 信任边界机制测试（H14 整改：封死构建上下文的宿主目录信任
// 边界——M3/M4 同根因）：
//   - scope 升级：deploy token → PermissionDenied（base_dir 可指向宿主
//     任意目录，与 env 明文读取同级信任）；admin token 放行；
//   - 包含性校验：build.context 的 `..` 逃逸（显式 base_dir 与临时回落
//     两种基准形态）→ E_COMPOSE_UNSUPPORTED 拒绝、不入队；合法相对
//     context → 入队通过且落库 context_dir 位于基准目录内。

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// triggerCompose 是构建触发的最小 compose（单 build 模式服务；context
// 由各用例注入）。
func triggerCompose(name, context string) []byte {
	return []byte("name: " + name + "\nservices:\n  web:\n    build:\n      context: " + context + "\n")
}

// TestTriggerBuildScopeAdminOnly scope 升级生效：deploy 拒、admin 放行。
func TestTriggerBuildScopeAdminOnly(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	builds := serverv1.NewBuildsServiceClient(env.conn)

	// 方法级 scope 表：TriggerBuild 登记 admin（fail-closed 的登记面）。
	if s, ok := RequiredScope("/fleetly.server.v1.BuildsService/TriggerBuild"); !ok || s != ScopeAdmin {
		t.Fatalf("TriggerBuild scope = %q ok=%v, want admin", s, ok)
	}

	// deploy scope token：PermissionDenied（H14——不随 deploy 下放）。
	_, err := builds.TriggerBuild(authCtx(ctx, env.depTok), &serverv1.TriggerBuildRequest{Project: env.projectRef(), 
		Compose: triggerCompose("scopeapp", "."),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("deploy token TriggerBuild code = %v, want PermissionDenied", status.Code(err))
	}
	// read scope token：同样拒绝（admin 严格于 deploy）。
	_, err = builds.TriggerBuild(authCtx(ctx, env.readTok), &serverv1.TriggerBuildRequest{Project: env.projectRef(), 
		Compose: triggerCompose("scopeapp", "."),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token TriggerBuild code = %v, want PermissionDenied", status.Code(err))
	}

	// admin token：放行（合法相对 context，基准回落服务端临时目录——
	// context "." 即基准本身，包含性满足）。
	resp, err := builds.TriggerBuild(authCtx(ctx, env.admTok), &serverv1.TriggerBuildRequest{Project: env.projectRef(), 
		Compose: triggerCompose("scopeapp", "."),
	})
	if err != nil {
		t.Fatalf("admin token TriggerBuild: %v", err)
	}
	if len(resp.GetBuilds()) != 1 || resp.GetBuilds()[0].GetStatus() != "queued" {
		t.Fatalf("admin TriggerBuild response = %+v, want one queued build", resp)
	}
}

// TestTriggerBuildContextContainment 包含性校验：context 逃逸基准目录
// → E_COMPOSE_UNSUPPORTED（compose 族拒绝码，errcode 零新增）；
// 合法相对 context → 通过且落库 context_dir 在基准内。
func TestTriggerBuildContextContainment(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	builds := serverv1.NewBuildsServiceClient(env.conn)
	base := t.TempDir()

	// 显式 base_dir + `..` 多级逃逸（直指宿主上层/根）：拒绝。
	_, err := builds.TriggerBuild(authCtx(ctx, env.admTok), &serverv1.TriggerBuildRequest{Project: env.projectRef(), 
		Compose: triggerCompose("escapeapp", "../../.."),
		BaseDir: base,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("escaping context code = %v (%v), want InvalidArgument (E_COMPOSE_UNSUPPORTED)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "E_COMPOSE_UNSUPPORTED") && !strings.Contains(err.Error(), "outside the base directory") {
		t.Fatalf("escaping context error = %v, want containment message", err)
	}
	// 不入队（拒绝发生在建行之前）。
	if rows, lerr := env.st.ListAppBuilds(ctx, mustAppID(t, env, "escapeapp"), 10); lerr != nil || len(rows) != 0 {
		t.Fatalf("rejected compose must not enqueue builds: rows=%d err=%v", len(rows), lerr)
	}

	// 显式 base_dir + 单级逃逸（context == 基准父目录）：拒绝。
	if _, err = builds.TriggerBuild(authCtx(ctx, env.admTok), &serverv1.TriggerBuildRequest{Project: env.projectRef(), 
		Compose: triggerCompose("escapeapp", ".."),
		BaseDir: base,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("parent-escape context code = %v, want InvalidArgument", status.Code(err))
	}

	// 基准回落临时目录（不携带 base_dir）+ 逃逸：同样拒绝（逃逸形态在
	// 临时基准下只有外带语义，无合法构建内容）。
	if _, err = builds.TriggerBuild(authCtx(ctx, env.admTok), &serverv1.TriggerBuildRequest{Project: env.projectRef(), 
		Compose: triggerCompose("escapeapp", "../.."),
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("temp-base escape code = %v, want InvalidArgument", status.Code(err))
	}

	// 合法相对 context（子目录词形，含折叠段）：通过。
	resp, err := builds.TriggerBuild(authCtx(ctx, env.admTok), &serverv1.TriggerBuildRequest{Project: env.projectRef(), 
		Compose: triggerCompose("okapp", "./web"),
		BaseDir: base,
	})
	if err != nil {
		t.Fatalf("legal context TriggerBuild: %v", err)
	}
	if len(resp.GetBuilds()) != 1 {
		t.Fatalf("legal context builds = %d, want 1", len(resp.GetBuilds()))
	}
	// 落库契约：request JSON 的 context_dir 位于基准目录内。
	rec, err := env.st.GetBuild(ctx, resp.GetBuilds()[0].GetId())
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	req, err := build.DecodeRequest(rec.Request)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if expected := filepath.Join(base, "web"); req.ContextDir != expected {
		t.Fatalf("context_dir = %q, want %q", req.ContextDir, expected)
	}
	if rel, rerr := filepath.Rel(base, req.ContextDir); rerr != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("stored context_dir %q escapes base %q (rel=%q err=%v)", req.ContextDir, base, rel, rerr)
	}
}

// mustAppID 取应用 ID（入队断言的对照读取；应用不存在即致命）。
func mustAppID(t *testing.T, env *testEnv, name string) string {
	t.Helper()
	app, err := env.st.GetAppByName(context.Background(), name)
	if err != nil {
		t.Fatalf("GetAppByName(%s): %v", name, err)
	}
	return app.ID
}

// ── S18-A11：TriggerBuild 走 Queue.Enqueue（同进程触发立即唤醒）───────

// recordingExecutor 记录被调时刻（A11 唤醒延迟断言的假执行器）。
type recordingExecutor struct {
	called chan time.Time
}

func (e *recordingExecutor) Execute(_ context.Context, rec state.BuildRecord) (state.BuildRecord, error) {
	select {
	case e.called <- time.Now():
	default:
	}
	return rec, nil
}

// TestTriggerBuildWakesQueueImmediately A11：TriggerBuild 经 Queue.Enqueue
// 入队（建行 + 唤醒）——同进程触发后认领延迟显著小于 poll interval
// （队列扫描周期 20s，唤醒路径应在秒级认领；唤醒通道闲置则要等满 20s）。
func TestTriggerBuildWakesQueueImmediately(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	exec := &recordingExecutor{called: make(chan time.Time, 4)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := build.NewQueue(st, exec, 1, 20*time.Second, time.Minute, logger)
	qctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = q.Run(qctx) }()

	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterBuildsServiceServer(srv, NewBuildsService(st, q, build.UploadConfig{}))
	conn := serveBufconn(t, srv)
	client := serverv1.NewBuildsServiceClient(conn)
	tok := tokenFor(t, st)
	seedFixtureProject(t, st) // 归属夹具（确定性 slug "fixture"）

	start := time.Now()
	if _, err := client.TriggerBuild(authCtx(context.Background(), tok), &serverv1.TriggerBuildRequest{Project: "fixture",
		Compose: triggerCompose("wakeapp", "."),
	}); err != nil {
		t.Fatalf("TriggerBuild: %v", err)
	}
	select {
	case at := <-exec.called:
		if elapsed := at.Sub(start); elapsed >= 20*time.Second {
			t.Fatalf("claim latency %v ≥ poll interval 20s (wakeup channel not effective)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor not called within 5s: same-process trigger was not woken (still waiting on the poll interval)")
	}
}
