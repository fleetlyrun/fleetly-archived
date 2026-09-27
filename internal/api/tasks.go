package api

// TasksService 实现 server.v1.TasksService（T 线 DT-5 / IMPL-T2-1）：程序化
// 动态工作负载面。API 面职责 = 受理/校验/投影/停止/删除 + 配额 fail-closed；
// 状态收敛（底座服务 create/remove、TTL 回收、孤儿对账）由引擎 tick duty
// 承载（唯一写点纪律）。
//
// 结构性禁令（DT-5）：**本服务的请求面不存在任何网络 attach 入参**——
// TaskScope 是网络引用（按名加入既有网），per-task 动态 attach 在类型层
// 不可表示（2026-09-14 attach 残留事故的事故类，机制验收条款）。
//
// 安全默认（服务端强制、不进用户表达面）：cap_drop ALL / 只读 rootfs /
// 非 root / pids 限额 / restart none 由引擎组装 spec 时固定下发——proto
// 没有对应字段（与 compose 拒 cap_drop 同立场）。
//
// 隔离与配额：owner_token_id = 调用令牌（跨令牌访问 404——不可见即不存在；
// 不以 403 泄漏存在性）；配额（并发/CPU/内存合计）在 CreateTask 同事务
// fail-closed 核对。scope 门 = 独立 `tasks` scope（read/deploy 不蕴含，
// admin 蕴含——terminal 同族先例）。

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/oklog/ulid/v2"
)

// TasksOrchestrator 是任务执行面的编排端口（engine 实现；方向纪律：api
// 定义端口、不感知实现类型——ProjectNetworkPort 同款）。
type TasksOrchestrator interface {
	// ResolveTaskScope 解析作用域引用为网络名并核验在位（失败即拒绝——
	// 任务只加入既有网）。
	ResolveTaskScope(ctx context.Context, kind, ref string, internal bool) (string, error)
	// ResolveTaskImage 把镜像引用解析为不可变 digest 形态（DT-2 解析腿）。
	ResolveTaskImage(ctx context.Context, ref string) (string, error)
	// EnsureTaskNetwork 幂等确保 task-group 网络在位并落控制面挂靠声明
	//（返回网络名与逐成员状态）。
	EnsureTaskNetwork(ctx context.Context, ref string, internal bool, members []engine.TaskNetworkMember) (string, []engine.TaskNetworkMemberStatus, error)
}

// TasksService 实现 server.v1.TasksService。
type TasksService struct {
	serverv1.UnimplementedTasksServiceServer
	st  *state.Store
	box *secrets.Box
	// port 是执行面编排端口（nil = RPC 如实报不可用，不静默退化）。
	port TasksOrchestrator
}

// NewTasksService 构造 TasksService。
func NewTasksService(st *state.Store, box *secrets.Box, port TasksOrchestrator) *TasksService {
	return &TasksService{st: st, box: box, port: port}
}

// EnsureTaskNetwork 幂等确保 task-group 网络（长活）+ 控制面挂靠声明。
func (s *TasksService) EnsureTaskNetwork(ctx context.Context, req *serverv1.EnsureTaskNetworkRequest) (*serverv1.EnsureTaskNetworkResponse, error) {
	if s.port == nil {
		return nil, statusEnvelope(codes.Unavailable, "task orchestration is not assembled in this build")
	}
	ref := strings.TrimSpace(req.GetRef())
	if err := naming.ValidateTaskGroupRef(ref); err != nil {
		return nil, apperr.New("E_TASK_UNSUPPORTED", "%v", err).WithStage("task.scope")
	}
	members := make([]engine.TaskNetworkMember, 0, len(req.GetMembers()))
	for _, m := range req.GetMembers() {
		appRef := strings.TrimSpace(m.GetApp())
		service := strings.TrimSpace(m.GetService())
		// 在途部署守卫（与项目网 attach 同款）：挂靠的生效腿是成员 app 的
		// 重部署，在途发布不得被旧快照覆盖（ConvergeApp 先例）。
		app, err := s.st.GetAppByName(ctx, appRef)
		if err != nil {
			return nil, apperr.New("E_TASK_UNSUPPORTED",
				"task network member app %q: %v", appRef, err).WithStage("task.network")
		}
		if has, err := s.st.AppHasNonTerminalDeployment(ctx, app.ID); err != nil {
			return nil, err
		} else if has {
			return nil, conflict("app " + app.Name + " has a deployment in flight: wait for it to reach a terminal state before attaching it to a task-group network")
		}
		members = append(members, engine.TaskNetworkMember{App: appRef, Service: service})
	}
	name, statuses, err := s.port.EnsureTaskNetwork(ctx, ref, req.GetInternal(), members)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.TaskNetworkMemberStatus, 0, len(statuses))
	for _, member := range statuses {
		out = append(out, &serverv1.TaskNetworkMemberStatus{
			App: member.App, Service: member.Service, Status: member.Status, DeploymentId: member.DeploymentID,
		})
	}
	return &serverv1.EnsureTaskNetworkResponse{Name: name, Internal: req.GetInternal(), Members: out}, nil
}

// CreateTask 受理任务（owner = 调用令牌；配额 fail-closed；返回 queued 视图）。
func (s *TasksService) CreateTask(ctx context.Context, req *serverv1.CreateTaskRequest) (*serverv1.CreateTaskResponse, error) {
	if s.port == nil {
		return nil, statusEnvelope(codes.Unavailable, "task orchestration is not assembled in this build")
	}
	owner := callerTokenID(ctx)
	if owner == "" {
		return nil, statusEnvelope(codes.Unauthenticated, "task ownership requires a token principal")
	}
	image := strings.TrimSpace(req.GetImage())
	if image == "" {
		return nil, apperr.New("E_TASK_UNSUPPORTED", "image is required").WithStage("task.create")
	}
	scope := req.GetScope()
	if scope == nil {
		return nil, apperr.New("E_TASK_UNSUPPORTED", "scope is required (app | project | task-group)").WithStage("task.create")
	}
	ttl, err := normalizeTaskTTL(req.GetTtlSeconds())
	if err != nil {
		return nil, err
	}
	cpuMillis, memoryBytes, err := normalizeTaskResources(req.GetCpuMillis(), req.GetMemoryBytes())
	if err != nil {
		return nil, err
	}
	envCipher, err := s.prepareTaskEnv(req.GetEnv())
	if err != nil {
		return nil, err
	}
	// 作用域解析（网络必须已在位——任务只按名加入既有网）。
	network, err := s.port.ResolveTaskScope(ctx, strings.TrimSpace(scope.GetKind()), strings.TrimSpace(scope.GetRef()), scope.GetInternal())
	if err != nil {
		return nil, err
	}
	// 镜像解析（tag → digest；平台 registry 凭证 + airgap 回落）。
	pinnedImage, err := s.port.ResolveTaskImage(ctx, image)
	if err != nil {
		return nil, err
	}
	taskID := ulid.Make().String()
	service, err := naming.TaskServiceName(taskID)
	if err != nil {
		return nil, statusEnvelope(codes.Internal, "task service naming failed: "+err.Error())
	}
	quota, err := s.st.LoadTaskQuota(ctx, owner)
	if err != nil {
		return nil, err
	}
	task, err := s.st.CreateTask(ctx, state.TaskWrite{
		ID:            taskID,
		Name:          strings.TrimSpace(req.GetName()),
		OwnerTokenID:  owner,
		Image:         pinnedImage,
		Command:       trimAll(req.GetCommand()),
		Args:          trimAll(req.GetArgs()),
		EnvCipher:     envCipher,
		ScopeKind:     strings.TrimSpace(scope.GetKind()),
		ScopeRef:      strings.TrimSpace(scope.GetRef()),
		ScopeInternal: scope.GetInternal(),
		Network:       network,
		TTLSeconds:    ttl,
		CPUMillis:     cpuMillis,
		MemoryBytes:   memoryBytes,
		Service:       service,
		ActorTokenID:  owner,
	}, quota)
	if err != nil {
		if errors.Is(err, state.ErrTaskQuotaExceeded) {
			return nil, apperr.New("E_TASK_QUOTA_EXCEEDED", "%v", err).WithStage("task.create")
		}
		return nil, err
	}
	return &serverv1.CreateTaskResponse{Task: taskView(task)}, nil
}

// GetTask 取单个任务视图（env 只在本面回显；他令牌的任务 404）。
func (s *TasksService) GetTask(ctx context.Context, req *serverv1.GetTaskRequest) (*serverv1.GetTaskResponse, error) {
	task, err := s.ownedTask(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	view, err := s.taskViewWithEnv(task)
	if err != nil {
		return nil, err
	}
	return &serverv1.GetTaskResponse{Task: view}, nil
}

// ListTasks 列本令牌的任务（缺省只出非终态；include_terminal=true 出台账）。
func (s *TasksService) ListTasks(ctx context.Context, req *serverv1.ListTasksRequest) (*serverv1.ListTasksResponse, error) {
	tasks, err := s.st.ListTasks(ctx, callerTokenID(ctx), req.GetIncludeTerminal(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.TaskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskView(t))
	}
	return &serverv1.ListTasksResponse{Tasks: out}, nil
}

// StopTask 停止任务（幂等：停止中/已停止成功返回；底座服务由引擎移除）。
func (s *TasksService) StopTask(ctx context.Context, req *serverv1.StopTaskRequest) (*serverv1.StopTaskResponse, error) {
	task, err := s.ownedTask(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	updated, _, err := s.st.RequestTaskStop(ctx, task.ID, "owner", callerTokenID(ctx))
	if err != nil {
		if errors.Is(err, state.ErrTaskNotFound) {
			return nil, notFound("task not found: " + req.GetId())
		}
		return nil, err
	}
	return &serverv1.StopTaskResponse{Task: taskView(updated)}, nil
}

// DeleteTask 删除任务（停止 + 移除底座服务 + 台账行删除；不存在 404）。
func (s *TasksService) DeleteTask(ctx context.Context, req *serverv1.DeleteTaskRequest) (*serverv1.DeleteTaskResponse, error) {
	task, err := s.ownedTask(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if _, _, err := s.st.RequestTaskDelete(ctx, task.ID, callerTokenID(ctx)); err != nil {
		if errors.Is(err, state.ErrTaskNotFound) {
			return nil, notFound("task not found: " + req.GetId())
		}
		return nil, err
	}
	return &serverv1.DeleteTaskResponse{Id: task.ID}, nil
}

// ownedTask 取任务行并核验属主（跨令牌访问恒 404——不可见即不存在，不以
// 403 泄漏他令牌任务的存在性）。
func (s *TasksService) ownedTask(ctx context.Context, id string) (state.Task, error) {
	if strings.TrimSpace(id) == "" {
		return state.Task{}, statusInvalidArgument("task id is required")
	}
	task, err := s.st.GetTask(ctx, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, state.ErrTaskNotFound) {
			return state.Task{}, notFound("task not found: " + id)
		}
		return state.Task{}, err
	}
	if task.OwnerTokenID != callerTokenID(ctx) {
		return state.Task{}, notFound("task not found: " + id)
	}
	return task, nil
}

// prepareTaskEnv 校验并加密任务 env（返回 envelope 密文；值明文只存活于
// 本次调用的内存链——落库恒密文，与 app env 同纪律）。
func (s *TasksService) prepareTaskEnv(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	if s.box == nil {
		return "", statusEnvelope(codes.Unavailable, "the platform key box is not assembled in this build: task env cannot be stored")
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if err := state.ValidateEnvKey(k); err != nil {
			return "", apperr.New("E_TASK_UNSUPPORTED", "task env key %q: %v", k, err).WithStage("task.create")
		}
		if strings.HasPrefix(k, state.ReservedEnvPrefix) {
			return "", apperr.New("E_ENV_KEY_RESERVED",
				"env key %q uses the reserved FLEETLY_ prefix (platform-managed namespace; user writes are rejected)", k).
				WithContext("key", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(env))
	for _, k := range keys {
		ordered[k] = env[k]
	}
	raw, err := json.Marshal(ordered)
	if err != nil {
		return "", statusEnvelope(codes.Internal, "task env encode failed: "+err.Error())
	}
	cipher, err := s.box.Encrypt(raw)
	if err != nil {
		return "", statusEnvelope(codes.Internal, "task env encryption failed: "+err.Error())
	}
	return string(cipher), nil
}

// normalizeTaskTTL 归一 TTL（0 = 平台默认；<1min / >24h fail-closed 拒绝——
// DT-5 参数建议沿 T2-0③：TTL 下限 ≥1min）。
func normalizeTaskTTL(seconds int64) (int64, error) {
	switch {
	case seconds == 0:
		return state.DefaultTaskTTLSeconds, nil
	case seconds < state.MinTaskTTLSeconds:
		return 0, apperr.New("E_TASK_UNSUPPORTED",
			"ttl_seconds %d is below the platform minimum %d (T2 baseline: a shorter TTL makes create-healthy-reclaim overhead dominate)",
			seconds, state.MinTaskTTLSeconds).WithStage("task.create")
	case seconds > state.MaxTaskTTLSeconds:
		return 0, apperr.New("E_TASK_UNSUPPORTED",
			"ttl_seconds %d is above the platform maximum %d", seconds, state.MaxTaskTTLSeconds).WithStage("task.create")
	}
	return seconds, nil
}

// normalizeTaskResources 归一资源请求（0 = 平台默认；上下限 fail-closed）。
func normalizeTaskResources(cpuMillis, memoryBytes int64) (int64, int64, error) {
	switch {
	case cpuMillis == 0:
		cpuMillis = state.DefaultTaskCPUMillis
	case cpuMillis < 1 || cpuMillis > state.MaxTaskCPUMillis:
		return 0, 0, apperr.New("E_TASK_UNSUPPORTED",
			"cpu_millis %d is outside [1, %d]", cpuMillis, state.MaxTaskCPUMillis).WithStage("task.create")
	}
	switch {
	case memoryBytes == 0:
		memoryBytes = state.DefaultTaskMemoryBytes
	case memoryBytes < 1<<20 || memoryBytes > state.MaxTaskMemoryBytes:
		return 0, 0, apperr.New("E_TASK_UNSUPPORTED",
			"memory_bytes %d is outside [%d, %d]", memoryBytes, 1<<20, state.MaxTaskMemoryBytes).WithStage("task.create")
	}
	return cpuMillis, memoryBytes, nil
}

// trimAll 复制并逐项 trim（命令/参数面的空白归一；保序）。
func trimAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

// taskView 组装任务视图（列表面：env 恒空——仓库级读面不把全部 env 值
// 铺开；单任务回读走 taskViewWithEnv）。
func taskView(t state.Task) *serverv1.TaskView {
	v := &serverv1.TaskView{
		Id:          t.ID,
		Name:        t.Name,
		Image:       t.Image,
		Command:     append([]string{}, t.Command...),
		Args:        append([]string{}, t.Args...),
		Status:      string(t.Status),
		Scope:       &serverv1.TaskScope{Kind: t.ScopeKind, Ref: t.ScopeRef, Internal: t.ScopeInternal},
		TtlSeconds:  t.TTLSeconds,
		CpuMillis:   t.CPUMillis,
		MemoryBytes: t.MemoryBytes,
		Service:     t.Service,
		DnsName:     t.Service,
		CreatedAt:   timestamppb.New(t.CreatedAt),
		ExpiresAt:   timestamppb.New(t.ExpiresAt),
		Error:       t.Error,
		StopReason:  t.StopReason,
	}
	if !t.StartedAt.IsZero() {
		v.StartedAt = timestamppb.New(t.StartedAt)
	}
	if !t.StoppedAt.IsZero() {
		v.StoppedAt = timestamppb.New(t.StoppedAt)
	}
	return v
}

// taskViewWithEnv 是 GetTask 面的带 env 投影（密文解密失败 = 显式 5xx——
// 回读面不伪造空 env）。
func (s *TasksService) taskViewWithEnv(t state.Task) (*serverv1.TaskView, error) {
	v := taskView(t)
	if t.EnvCipher == "" {
		return v, nil
	}
	if s.box == nil {
		return nil, statusEnvelope(codes.Unavailable, "the platform key box is not assembled in this build")
	}
	plain, err := s.box.Decrypt([]byte(t.EnvCipher))
	if err != nil {
		return nil, statusEnvelope(codes.Internal, "task env decryption failed")
	}
	var envMap map[string]string
	if err := json.Unmarshal(plain, &envMap); err != nil {
		return nil, statusEnvelope(codes.Internal, "task env payload is not a JSON object")
	}
	v.Env = envMap
	return v, nil
}
