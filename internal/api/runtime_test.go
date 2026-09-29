package api

// RuntimeService 的引用寻址回归与投影形态（2026-09-29 Console IA 重设计）：
// resolveApp 过门禁后必须把**解析后的三段限定形**传引擎（AppRuntime 按
// GetAppByName 直查，不识别引用形态——DriftService 同款纪律，drift_test.go
// 头注）；底座桩回放「一服务 + running/failed 任务混合」的实况，断言水位
// 与任务投影逐字段对位、id/限定形/裸名三寻址同答。

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// runtimeSubstrateStub 回放确定性实况：一个受管 web 服务 + 两任务
// （1 running + 1 failed 历史）。ServiceInspect 缺失不触（AppRuntime 不用）。
type runtimeSubstrateStub struct {
	now time.Time
}

func (runtimeSubstrateStub) SwarmReady(context.Context) error { return nil }

func (runtimeSubstrateStub) NetworkEnsure(context.Context, string) error { return nil }

func (runtimeSubstrateStub) ServiceInspect(context.Context, string) (engine.ServiceState, error) {
	return engine.ServiceState{}, engine.ErrServiceNotFound
}

func (runtimeSubstrateStub) ServiceCreate(context.Context, engine.ServiceSpec) error { return nil }

func (runtimeSubstrateStub) ServiceUpdate(context.Context, string, engine.ServiceSpec) error {
	return nil
}

func (runtimeSubstrateStub) ServiceRemove(context.Context, string) error { return nil }

func (s runtimeSubstrateStub) ServiceList(_ context.Context, labels map[string]string) ([]engine.ServiceState, error) {
	if labels[state.LabelApp] != "tfixture/fixture/runtimeapp" {
		return nil, nil
	}
	return []engine.ServiceState{{
		Name:        "fleetly-tfixture-fixture-runtimeapp-web",
		Image:       "nginx:1.27@sha256:aaa",
		Replicas:    1,
		UpdateState: "completed",
	}}, nil
}

func (s runtimeSubstrateStub) TaskList(_ context.Context, serviceName string) ([]engine.TaskState, error) {
	if serviceName != "fleetly-tfixture-fixture-runtimeapp-web" {
		return nil, nil
	}
	return []engine.TaskState{
		{ID: "t1", Slot: 1, State: "running", DesiredState: "running",
			Image: "nginx:1.27@sha256:aaa", Timestamp: s.now},
		{ID: "t0", Slot: 1, State: "failed", DesiredState: "running", Err: "exit 1",
			Image: "nginx:1.26@sha256:bbb", Timestamp: s.now.Add(-time.Hour)},
	}, nil
}

// newRuntimeTestEnv 与 newDriftTestEnv 同构：bufconn + 生产鉴权链 + 种子
// succeeded 部署行（密文期望快照，web 声明 2 副本）。
func newRuntimeTestEnv(t *testing.T) (*testEnv, *grpc.ClientConn, state.App) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}

	env := &testEnv{st: st, box: box, fixtureProject: seedFixtureProject(t, st)}
	env.admTok = env.seedToken(t, "admin")

	app, err := st.CreateApp(context.Background(), "", "runtimeapp", env.fixtureProject.ID, env.fixtureProject.TeamID)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	spec := engine.ServiceSpec{
		Name:     "fleetly-tfixture-fixture-runtimeapp-web",
		Image:    "nginx:1.27",
		Replicas: 2,
		ServiceLabels: map[string]string{
			state.LabelManaged: state.ManagedLabelValue,
			state.LabelApp:     app.QualifiedName(),
			state.LabelProcess: "web",
		},
	}
	raw, err := json.Marshal([]engine.ServiceSpec{spec})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	ct, err := box.Encrypt(raw)
	if err != nil {
		t.Fatalf("encrypt snapshot: %v", err)
	}
	rec, err := st.CreateDeployment(context.Background(), state.DeployRecord{
		AppID: app.ID, AppName: app.Name, Kind: "deploy", DesiredSpec: string(ct),
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if err := st.InTx(context.Background(), func(tx *state.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`UPDATE deployments SET status = 'succeeded', desired_hash = ? WHERE id = ?`,
			spec.DesiredHash(), rec.ID)
		return err
	}); err != nil {
		t.Fatalf("seed succeeded row: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	eng := engine.NewEngine(engine.Config{}, st, runtimeSubstrateStub{now: time.Now().UTC()}, nil, nil, box, logger)
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterRuntimeServiceServer(srv, NewRuntimeService(st, eng))
	conn := serveBufconn(t, srv)
	return env, conn, app
}

func TestShowAppRuntimeResolvesReferenceAndProjectsTasks(t *testing.T) {
	env, conn, app := newRuntimeTestEnv(t)
	ctx := context.Background()
	client := serverv1.NewRuntimeServiceClient(conn)

	for label, ref := range map[string]string{
		"id":        app.ID,
		"qualified": app.QualifiedName(),
		"bare":      "runtimeapp",
	} {
		show, err := client.ShowAppRuntime(authCtx(ctx, env.admTok), &serverv1.ShowAppRuntimeRequest{App: ref})
		if err != nil {
			t.Fatalf("[%s] ShowAppRuntime(%q): %v", label, ref, err)
		}
		if show.GetApp() != "runtimeapp" {
			t.Fatalf("[%s] report app = %q, want runtimeapp", label, show.GetApp())
		}
		if show.GetDesiredDeployment() == "" {
			t.Fatalf("[%s] desired deployment empty, want seeded baseline", label)
		}
		if len(show.GetServices()) != 1 {
			t.Fatalf("[%s] services = %d, want 1", label, len(show.GetServices()))
		}
		svc := show.GetServices()[0]
		if svc.GetName() != "fleetly-tfixture-fixture-runtimeapp-web" {
			t.Fatalf("[%s] service name = %q", label, svc.GetName())
		}
		// compose 服务名（fleetly.process）随行投影——Console 按它与快照
		// 服务清单对位（swarm 全名不做第二份拼装公式）。
		if svc.GetService() != "web" {
			t.Fatalf("[%s] compose service = %q, want web", label, svc.GetService())
		}
		// 声明值来自期望快照（2），实况水位来自任务判定（1 running）——
		// 两侧各报各的（不相互冒充）。
		if svc.GetDeclaredReplicas() != 2 {
			t.Fatalf("[%s] declared replicas = %d, want 2 (snapshot)", label, svc.GetDeclaredReplicas())
		}
		if svc.GetActualReplicas() != 1 {
			t.Fatalf("[%s] actual replicas = %d, want 1 (running task)", label, svc.GetActualReplicas())
		}
		if svc.GetMode() != "replicated" {
			t.Fatalf("[%s] mode = %q, want replicated", label, svc.GetMode())
		}
		if svc.GetMissing() {
			t.Fatalf("[%s] missing = true, want false (service present)", label)
		}
		if len(svc.GetTasks()) != 2 {
			t.Fatalf("[%s] tasks = %d, want 2 (running + failed history)", label, len(svc.GetTasks()))
		}
		task := svc.GetTasks()[0]
		if task.GetState() != "running" || task.GetDesiredState() != "running" || task.GetSlot() != 1 {
			t.Fatalf("[%s] task[0] = %+v, want running/running slot 1", label, task)
		}
		if task.GetTimestamp() == nil {
			t.Fatalf("[%s] task[0] timestamp not projected", label)
		}
		history := svc.GetTasks()[1]
		if history.GetState() != "failed" || history.GetError() != "exit 1" {
			t.Fatalf("[%s] task[1] = %+v, want failed history with error", label, history)
		}
	}
}
