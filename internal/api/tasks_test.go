package api

// 任务面 API 回归（T 线 DT-5 / IMPL-T2-1）：
//   - 守卫② 配额 fail-closed（E_TASK_QUOTA_EXCEEDED 429 信封）；
//   - 守卫④ 跨令牌越权拒绝（他令牌的任务 Get/Stop/Delete 恒 404——不可见
//     即不存在，不以 403 泄漏存在性）；
//   - 守卫⑤ API 面无 attach 入参（类型层不可表示——描述符扫描 + 网络面
//     请求面字段集断言）；
//   - scope 门登记（整体 tasks 独立 scope）+ TTL/资源上下限 + env 密文落库
//     与列表面零 env。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// fakeTasksOrchestrator 是执行面编排端口的确定性替身（网络名固定；成员
// 声明全记录）。
type fakeTasksOrchestrator struct {
	network    string
	resolveErr error
	imageErr   error
	pinned     string

	ensureCalls int
	ensureRef   string
	ensureInt   bool
	ensureMem   []engine.TaskNetworkMember
}

func (f *fakeTasksOrchestrator) ResolveTaskScope(_ context.Context, _, _ string, _ bool) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return f.network, nil
}

func (f *fakeTasksOrchestrator) ResolveTaskImage(_ context.Context, ref string) (string, error) {
	if f.imageErr != nil {
		return "", f.imageErr
	}
	if f.pinned != "" {
		return f.pinned, nil
	}
	return ref, nil
}

func (f *fakeTasksOrchestrator) EnsureTaskNetwork(_ context.Context, ref string, internal bool, members []engine.TaskNetworkMember) (string, []engine.TaskNetworkMemberStatus, error) {
	f.ensureCalls++
	f.ensureRef = ref
	f.ensureInt = internal
	f.ensureMem = members
	out := make([]engine.TaskNetworkMemberStatus, 0, len(members))
	for _, m := range members {
		out = append(out, engine.TaskNetworkMemberStatus{App: m.App, Service: m.Service, Status: "pending"})
	}
	return f.network, out, nil
}

type tasksAPIEnv struct {
	st   *state.Store
	box  *secrets.Box
	port *fakeTasksOrchestrator
	svc  serverv1.TasksServiceClient
}

func newTasksAPIEnv(t *testing.T) *tasksAPIEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "fleetly.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	port := &fakeTasksOrchestrator{network: "fleetly-taskgroup-tenant1"}
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterTasksServiceServer(srv, NewTasksService(st, box, port))
	conn := serveBufconn(t, srv)
	return &tasksAPIEnv{st: st, box: box, port: port, svc: serverv1.NewTasksServiceClient(conn)}
}

// seedTasksAPIToken 落一枚机具令牌并返回（明文, token id）。
func seedTasksAPIToken(t *testing.T, st *state.Store, scopes string) (string, string) {
	t.Helper()
	plaintext, err := generateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	tok, err := st.CreateToken(context.Background(), state.TokenWrite{
		Hash: state.HashToken(plaintext), Name: "tasks api fixture " + scopes, Scopes: scopes, Actor: "human",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return plaintext, tok.ID
}

func taskCreateRequest() *serverv1.CreateTaskRequest {
	return &serverv1.CreateTaskRequest{
		Name:  "api fixture",
		Image: "alpine:3.19",
		Scope: &serverv1.TaskScope{Kind: "task-group", Ref: "tenant1"},
	}
}

func TestTasksServiceScopeRegistration(t *testing.T) {
	for _, method := range []string{
		"/fleetly.server.v1.TasksService/EnsureTaskNetwork",
		"/fleetly.server.v1.TasksService/CreateTask",
		"/fleetly.server.v1.TasksService/GetTask",
		"/fleetly.server.v1.TasksService/ListTasks",
		"/fleetly.server.v1.TasksService/StopTask",
		"/fleetly.server.v1.TasksService/DeleteTask",
	} {
		got, ok := RequiredScope(method)
		if !ok || got != ScopeTasks {
			t.Fatalf("RequiredScope(%s) = %q,%v; want %q,true", method, got, ok, ScopeTasks)
		}
	}
	// scope 语义：tasks 独立（read/deploy 不蕴含；admin 蕴含）。
	if containsScope(ScopeRead, ScopeTasks) || containsScope(ScopeDeploy, ScopeTasks) {
		t.Fatal("tasks must not be implied by read/deploy (independent scope)")
	}
	if !containsScope(ScopeAdmin, ScopeTasks) {
		t.Fatal("admin must imply tasks")
	}
	if !containsScope(ScopeTasks, ScopeTasks) {
		t.Fatal("tasks token must satisfy the tasks scope gate")
	}
}

func TestTasksCreateListStopDeleteFlowAndCrossTokenIsolation(t *testing.T) {
	env := newTasksAPIEnv(t)
	ctx := context.Background()
	tokenA, _ := seedTasksAPIToken(t, env.st, "tasks")
	tokenB, _ := seedTasksAPIToken(t, env.st, "tasks")

	// 守卫④：read-only scope 令牌在 scope 门被拒（连面都进不来）。
	readToken, _ := seedTasksAPIToken(t, env.st, "read")
	if _, err := env.svc.ListTasks(authCtx(ctx, readToken), &serverv1.ListTasksRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token on tasks face: code = %v, want PermissionDenied", status.Code(err))
	}

	created, err := env.svc.CreateTask(authCtx(ctx, tokenA), taskCreateRequest())
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	task := created.GetTask()
	if task.GetStatus() != "queued" || task.GetId() == "" || task.GetService() != "fleetly-task-"+task.GetId() {
		t.Fatalf("created task = %+v", task)
	}
	if task.GetTtlSeconds() != state.DefaultTaskTTLSeconds {
		t.Fatalf("default ttl = %d, want %d", task.GetTtlSeconds(), state.DefaultTaskTTLSeconds)
	}
	if task.GetDnsName() != task.GetService() {
		t.Fatalf("dns_name = %q, want the stable service name %q", task.GetDnsName(), task.GetService())
	}

	// 守卫④：B 令牌看不到、停不了、删不了 A 的任务（恒 404）。
	if _, err := env.svc.GetTask(authCtx(ctx, tokenB), &serverv1.GetTaskRequest{Id: task.GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-token get: code = %v, want NotFound", status.Code(err))
	}
	if _, err := env.svc.StopTask(authCtx(ctx, tokenB), &serverv1.StopTaskRequest{Id: task.GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-token stop: code = %v, want NotFound", status.Code(err))
	}
	if _, err := env.svc.DeleteTask(authCtx(ctx, tokenB), &serverv1.DeleteTaskRequest{Id: task.GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-token delete: code = %v, want NotFound", status.Code(err))
	}
	listB, err := env.svc.ListTasks(authCtx(ctx, tokenB), &serverv1.ListTasksRequest{})
	if err != nil || len(listB.GetTasks()) != 0 {
		t.Fatalf("cross-token list = %d rows (err=%v), want 0", len(listB.GetTasks()), err)
	}

	// 属主面：列表可见；stop（stopping）→ rm（deleting）幂等。
	listA, err := env.svc.ListTasks(authCtx(ctx, tokenA), &serverv1.ListTasksRequest{})
	if err != nil || len(listA.GetTasks()) != 1 {
		t.Fatalf("owner list = %d rows (err=%v), want 1", len(listA.GetTasks()), err)
	}
	stopped, err := env.svc.StopTask(authCtx(ctx, tokenA), &serverv1.StopTaskRequest{Id: task.GetId()})
	if err != nil || stopped.GetTask().GetStatus() != "stopping" {
		t.Fatalf("stop = %+v (err=%v)", stopped.GetTask(), err)
	}
	if again, err := env.svc.StopTask(authCtx(ctx, tokenA), &serverv1.StopTaskRequest{Id: task.GetId()}); err != nil || again.GetTask().GetStatus() != "stopping" {
		t.Fatalf("stop idempotent = %+v (err=%v)", again.GetTask(), err)
	}
	if _, err := env.svc.DeleteTask(authCtx(ctx, tokenA), &serverv1.DeleteTaskRequest{Id: task.GetId()}); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	got, err := env.svc.GetTask(authCtx(ctx, tokenA), &serverv1.GetTaskRequest{Id: task.GetId()})
	if err != nil || got.GetTask().GetStatus() != "deleting" {
		t.Fatalf("after delete = %+v (err=%v), want deleting", got.GetTask(), err)
	}
	// 默认列表隐藏终态/墓碑之外的在途行——deleting 仍在途，可见；终态行只在
	// include_terminal 面出现（此处由 state 层终态语义承载）。
}

func TestTasksQuotaFailClosed(t *testing.T) {
	env := newTasksAPIEnv(t)
	ctx := context.Background()
	token, tokenID := seedTasksAPIToken(t, env.st, "tasks")
	if err := env.st.SaveTaskQuota(ctx, tokenID, state.TaskQuota{MaxConcurrent: 1, MaxCPUMillis: 8000, MaxMemoryBytes: 8 << 30}); err != nil {
		t.Fatalf("save quota: %v", err)
	}
	if _, err := env.svc.CreateTask(authCtx(ctx, token), taskCreateRequest()); err != nil {
		t.Fatalf("first task: %v", err)
	}
	_, err := env.svc.CreateTask(authCtx(ctx, token), taskCreateRequest())
	if err == nil {
		t.Fatal("second task must be rejected by the per-token quota")
	}
	assertWireApperrCode(t, err, "E_TASK_QUOTA_EXCEEDED")
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("quota error code = %v, want ResourceExhausted (429)", status.Code(err))
	}
}

// TestTasksRequestSurfaceHasNoAttachOrHardeningInputs 是守卫⑤的机制验收：
// 请求类型层不可表示 per-task 动态 attach 与加固关闭开关（与 compose 拒
// cap_drop 同立场）。
func TestTasksRequestSurfaceHasNoAttachOrHardeningInputs(t *testing.T) {
	banned := []string{"attach", "network", "interface", "cap", "privileged", "read_only", "readonly", "pids", "user", "rootfs", "restart"}
	assertFields := func(message string, descriptors []string) {
		for _, field := range descriptors {
			lower := strings.ToLower(field)
			for _, bad := range banned {
				if strings.Contains(lower, bad) {
					t.Errorf("%s carries field %q matching banned token %q (attach/hardening must be structurally unrepresentable)", message, field, bad)
				}
			}
		}
	}
	var createFields []string
	md := (&serverv1.CreateTaskRequest{}).ProtoReflect().Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		createFields = append(createFields, string(md.Fields().Get(i).Name()))
	}
	assertFields("CreateTaskRequest", createFields)
	var scopeFields []string
	sd := (&serverv1.TaskScope{}).ProtoReflect().Descriptor()
	for i := 0; i < sd.Fields().Len(); i++ {
		scopeFields = append(scopeFields, string(sd.Fields().Get(i).Name()))
	}
	assertFields("TaskScope", scopeFields)
	// 网络面请求恰好是「ref/internal/members」——没有 per-task 成员或任务
	// 级 attach 字段。
	var ensureFields []string
	ed := (&serverv1.EnsureTaskNetworkRequest{}).ProtoReflect().Descriptor()
	for i := 0; i < ed.Fields().Len(); i++ {
		ensureFields = append(ensureFields, string(ed.Fields().Get(i).Name()))
	}
	if strings.Join(ensureFields, ",") != "ref,internal,members" {
		t.Fatalf("EnsureTaskNetworkRequest fields = %v, want [ref internal members] (no per-task attach surface)", ensureFields)
	}
}

func TestTasksValidationBoundsAndEnvHandling(t *testing.T) {
	env := newTasksAPIEnv(t)
	ctx := context.Background()
	token, _ := seedTasksAPIToken(t, env.st, "tasks")

	// TTL 下限/上限与资源上下限 fail-closed（E_TASK_UNSUPPORTED 400）。
	short := taskCreateRequest()
	short.TtlSeconds = 30
	if _, err := env.svc.CreateTask(authCtx(ctx, token), short); err == nil {
		t.Fatal("ttl below the minimum must be rejected")
	} else {
		assertWireApperrCode(t, err, "E_TASK_UNSUPPORTED")
	}
	long := taskCreateRequest()
	long.TtlSeconds = 24*60*60 + 1
	if _, err := env.svc.CreateTask(authCtx(ctx, token), long); err == nil {
		t.Fatal("ttl above the maximum must be rejected")
	}
	big := taskCreateRequest()
	big.CpuMillis = 5000
	if _, err := env.svc.CreateTask(authCtx(ctx, token), big); err == nil {
		t.Fatal("cpu above the maximum must be rejected")
	}
	// 保留前缀 env 拒绝（平台命名空间）。
	reserved := taskCreateRequest()
	reserved.Env = map[string]string{"FLEETLY_SECRET": "x"}
	if _, err := env.svc.CreateTask(authCtx(ctx, token), reserved); err == nil {
		t.Fatal("reserved FLEETLY_ env prefix must be rejected")
	} else {
		assertWireApperrCode(t, err, "E_ENV_KEY_RESERVED")
	}

	// env 值密文落库；GetTask 回显 env；列表面零 env。
	withEnv := taskCreateRequest()
	withEnv.Env = map[string]string{"TOKEN": "super-secret-value"}
	created, err := env.svc.CreateTask(authCtx(ctx, token), withEnv)
	if err != nil {
		t.Fatalf("create task with env: %v", err)
	}
	row, err := env.st.GetTask(ctx, created.GetTask().GetId())
	if err != nil {
		t.Fatalf("read task row: %v", err)
	}
	if row.EnvCipher == "" || strings.Contains(row.EnvCipher, "super-secret-value") {
		t.Fatalf("env must be stored encrypted, got cipher %q", row.EnvCipher)
	}
	got, err := env.svc.GetTask(authCtx(ctx, token), &serverv1.GetTaskRequest{Id: created.GetTask().GetId()})
	if err != nil || got.GetTask().GetEnv()["TOKEN"] != "super-secret-value" {
		t.Fatalf("GetTask env echo = %v (err=%v)", got.GetTask().GetEnv(), err)
	}
	list, err := env.svc.ListTasks(authCtx(ctx, token), &serverv1.ListTasksRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range list.GetTasks() {
		if len(row.GetEnv()) != 0 {
			t.Fatalf("ListTasks must not echo env values, got %v", row.GetEnv())
		}
	}
	// 镜像解析失败 fail-closed（E_IMAGE_PULL_FAILED 透传）。
	env.port.imageErr = engine.ErrImageMissing
	if _, err := env.svc.CreateTask(authCtx(ctx, token), taskCreateRequest()); err == nil {
		t.Fatal("image resolution failure must fail closed")
	}
}

func TestTasksEnsureNetworkMembersAndInFlightGuard(t *testing.T) {
	env := newTasksAPIEnv(t)
	ctx := context.Background()
	token, _ := seedTasksAPIToken(t, env.st, "tasks")

	team, err := env.st.CreateTeam(ctx, state.TeamWrite{Slug: "taskcp", Name: "Task CP", CreatedBy: "fixture"})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	proj, err := env.st.CreateProject(ctx, state.ProjectWrite{TeamID: team.ID, Slug: "default", Name: "Default"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app, err := env.st.CreateApp(ctx, "", "control-plane", proj.ID, team.ID)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	resp, err := env.svc.EnsureTaskNetwork(authCtx(ctx, token), &serverv1.EnsureTaskNetworkRequest{
		Ref: "tenant1", Internal: true,
		Members: []*serverv1.TaskNetworkMember{{App: "control-plane", Service: "server"}},
	})
	if err != nil {
		t.Fatalf("ensure task network: %v", err)
	}
	if resp.GetName() != "fleetly-taskgroup-tenant1" || !resp.GetInternal() {
		t.Fatalf("ensure response = %+v", resp)
	}
	if env.port.ensureCalls != 1 || len(env.port.ensureMem) != 1 || env.port.ensureMem[0].Service != "server" {
		t.Fatalf("orchestrator calls = %d members=%+v", env.port.ensureCalls, env.port.ensureMem)
	}
	if len(resp.GetMembers()) != 1 || resp.GetMembers()[0].GetStatus() != "pending" {
		t.Fatalf("member statuses = %+v", resp.GetMembers())
	}
	// ref 形态违约：显式拒绝（命名族纪律）。
	if _, err := env.svc.EnsureTaskNetwork(authCtx(ctx, token), &serverv1.EnsureTaskNetworkRequest{Ref: "Bad-Ref!"}); err == nil {
		t.Fatal("invalid task-group ref must be rejected")
	}
	// 在途部署守卫：成员 app 有非终态部署 → 409（挂靠生效腿是重部署）。
	rec, err := env.st.CreateDeployment(ctx, state.DeployRecord{
		AppID: app.ID, AppName: app.Name, Kind: "deploy", SpecHash: "h", ComposePath: "/tmp/compose.yaml",
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	_ = rec
	_, err = env.svc.EnsureTaskNetwork(authCtx(ctx, token), &serverv1.EnsureTaskNetworkRequest{
		Ref: "tenant1", Members: []*serverv1.TaskNetworkMember{{App: "control-plane", Service: "server"}},
	})
	if err == nil || status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("in-flight member deploy: err = %v (code %v), want 409 conflict", err, status.Code(err))
	}
	// 未知成员 app：E_TASK_UNSUPPORTED。
	if _, err := env.svc.EnsureTaskNetwork(authCtx(ctx, token), &serverv1.EnsureTaskNetworkRequest{
		Ref: "tenant1", Members: []*serverv1.TaskNetworkMember{{App: "ghost", Service: "server"}},
	}); err == nil {
		t.Fatal("unknown member app must be rejected")
	}
}
