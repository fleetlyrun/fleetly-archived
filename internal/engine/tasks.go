package engine

// 程序化动态工作负载执行面（T 线 DT-5 / IMPL-T2-1）：任务 = swarm service
// 承载（restart-condition none，实例崩溃上抛；平台不静默自愈），平台管
// API/网络/配额/审计/回收，池语义（租约/保温/熔断）留在调用方。
//
// 机制边：
//  1. 作用域解析（ResolveTaskScope）：scope {kind, ref, internal} → 网络名。
//     app → app 专属网；project → 项目网；task-group → fleetly-taskgroup-<ref>。
//     **internal 只对 task-group 支持**（DT-7 不可信隔离面）；app/project
//     internal 一律 fail-closed（不可静默降级）。任务只「按名加入既有网」：
//     网络必须已在位（缺失 = 确定性失败，不代建）——per-task 动态 attach
//     在类型层不存在。
//  2. 受理（API 层）：state 行（queued）+ 配额 fail-closed；env 密文入行。
//  3. 收敛（advanceTasks duty，每 tick）：queued → 底座服务 create → running；
//     running → 任务失败/自然退出检测（failed / stopped）；stopping/deleting
//     → 服务移除 → stopped / 行删除。幂等、瞬态读错重试、确定性错误立即
//     failed（不无限重试）。
//  4. TTL 回收（reapExpiredTasks duty）：到期 → stopping + task.expired 事件
//     （timeout 参数建议沿 T2-0③：TTL ≥1min、默认 10min、tick 2s/30s 窗口
//     可覆盖，stop_grace_period 显式钉 5s）。
//  5. 对账（reconTasks，substrateRecon 同拍）：底座带任务 label 的服务在
//     state 无对应非终态行 → 孤儿 task 披露 + 回收（归属明确的本平台对象，
//     派生修正）；读错不结论（services 面同纪律）。
//  6. task-group 网络（EnsureTaskNetwork）：长活、幂等、自描述 label；控制面
//     服务「每网一次性挂靠」= state 成员声明 + 发布管线重部署双挂（不是
//     per-task attach——事故类结构禁令）。
//
// 安全默认（服务端强制、不进用户表达面，与 compose 拒 cap_drop 同立场）：
// CapDrop ALL / 只读 rootfs / 非 root user(65534:65534) / pids 512 /
// restart none / 显式 stop_grace_period——tasks.v1 没有对应字段。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// 任务作用域 kind 词表（tasks.v1 TaskScope.kind 的服务端真值）。
const (
	TaskScopeApp       = "app"
	TaskScopeProject   = "project"
	TaskScopeTaskGroup = "task-group"
)

// 平台固定的任务加固与回收参数（服务端强制；不进用户表达面）。
const (
	// taskPlatformUser 是非 root 运行用户（nobody:nogroup——镜像是否可写
	// 不是平台承诺面；函数基座自带工作目录语义）。
	taskPlatformUser = "65534:65534"
	// taskPidsLimit 是 PID 数限额（DT-5 原文 pids 512）。
	taskPidsLimit = 512
	// taskStopGracePeriod 是显式钉定的停机宽限（T2-0③：别留 daemon 默认
	// 语义漂移；回收时延预算 = 宽限 + ~1.7s 控制面）。
	taskStopGracePeriod = 5 * time.Second
	// taskConvergeLimit 是单拍收敛/回收的工作集上限（读面永不无界）。
	taskConvergeLimit = 200
)

// taskCapDrop 是能力剥夺清单（服务端恒 ["ALL"]）。
var taskCapDrop = []string{"ALL"}

// TaskNetworkMember 是一条控制面挂靠声明（EnsureTaskNetwork 入参）。
type TaskNetworkMember struct {
	// App 是成员 app 引用（裸名/三段限定形——机器面解析）。
	App string
	// Service 是成员 compose 服务名。
	Service string
}

// TaskNetworkMemberStatus 是挂靠声明的结果投影（proto 同形）。
type TaskNetworkMemberStatus struct {
	App     string
	Service string
	// Status ∈ attached（已声明/已挂靠）| rolling（重部署入队）| pending
	// （无成功部署史，下次发布生效）。
	Status       string
	DeploymentID string
}

// ResolveTaskScope 把作用域引用解析为网络名并核验在位（CreateTask 前置；
// 失败即拒绝——任务只加入既有网）。internal 变体只对 task-group 支持，
// 且与底座实况必须一致（错变体显式失败，不静默接受）。
func (e *Engine) ResolveTaskScope(ctx context.Context, kind, ref string, internal bool) (string, error) {
	if e.netSub == nil {
		return "", errorf("E_RUNTIME_UNAVAILABLE",
			"network substrate is not assembled in this build: cannot resolve the task scope network")
	}
	var (
		name string
		err  error
	)
	switch kind {
	case TaskScopeApp:
		if internal {
			return "", errorf("E_TASK_UNSUPPORTED",
				"internal=true is only supported for the task-group scope (the app network carries the app's own egress)")
		}
		var app state.App
		app, err = e.store.GetAppByName(ctx, ref)
		if err != nil {
			return "", errorf("E_TASK_UNSUPPORTED", "task scope app %q: %v", ref, err)
		}
		name, err = naming.NetworkName(app.TeamSlug, app.ProjectSlug, app.Name)
		if err != nil {
			return "", errorf("E_RUNTIME_UNAVAILABLE", "app network name for %s: %v", app.Name, err)
		}
	case TaskScopeProject:
		if internal {
			return "", errorf("E_TASK_UNSUPPORTED",
				"internal=true is only supported for the task-group scope (the project network carries member services' egress)")
		}
		projectID, perr := e.resolveTaskProject(ctx, ref)
		if perr != nil {
			return "", perr
		}
		name, err = naming.ProjectNetworkName(projectID)
		if err != nil {
			return "", errorf("E_RUNTIME_UNAVAILABLE", "project network name for %s: %v", projectID, err)
		}
	case TaskScopeTaskGroup:
		name, err = naming.TaskGroupNetworkName(ref)
		if err != nil {
			return "", errorf("E_TASK_UNSUPPORTED", "task-group ref %q: %v", ref, err)
		}
	default:
		return "", errorf("E_TASK_UNSUPPORTED",
			"scope kind %q is not supported (app | project | task-group)", kind)
	}
	st, err := e.netSub.NetworkInspect(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNetworkNotFound) {
			return "", errorf("E_TASK_UNSUPPORTED",
				"scope network %s does not exist: tasks join existing networks only (create it via EnsureTaskNetwork or attach the owning app first)", name)
		}
		return "", errorf("E_RUNTIME_UNAVAILABLE", "scope network inspect %s: %v", name, err)
	}
	if st.Internal != internal {
		return "", errorf("E_TASK_UNSUPPORTED",
			"scope network %s exists with internal=%t but internal=%t was requested (variant mismatch)", name, st.Internal, internal)
	}
	return name, nil
}

// resolveTaskProject 解析项目引用（项目平台 ID 或 team/prj 限定形）。
func (e *Engine) resolveTaskProject(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", errorf("E_TASK_UNSUPPORTED", "project scope ref is empty")
	}
	if !strings.Contains(ref, "/") {
		project, err := e.store.GetProject(ctx, ref)
		if err != nil {
			return "", errorf("E_TASK_UNSUPPORTED", "task scope project %q: %v", ref, err)
		}
		return project.ID, nil
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errorf("E_TASK_UNSUPPORTED", "project scope ref %q must be a project id or team/prj form", ref)
	}
	team, err := e.store.GetTeamBySlug(ctx, parts[0])
	if err != nil {
		return "", errorf("E_TASK_UNSUPPORTED", "task scope project %q: team %q: %v", ref, parts[0], err)
	}
	projects, err := e.store.ListProjects(ctx)
	if err != nil {
		return "", errorf("E_RUNTIME_UNAVAILABLE", "list projects: %v", err)
	}
	for _, p := range projects {
		if p.TeamID == team.ID && p.Slug == parts[1] {
			return p.ID, nil
		}
	}
	return "", errorf("E_TASK_UNSUPPORTED", "task scope project %q not found", ref)
}

// ResolveTaskImage 解析任务镜像引用为不可变 digest 形态（IMPL-T1-2/DT-2
// 解析腿：digest 直通 / tag registry-first + 平台凭证 / airgap 本机回落）。
// 失败即 CreateTask fail-closed（E_IMAGE_PULL_FAILED 点名 image 与原因）。
func (e *Engine) ResolveTaskImage(ctx context.Context, ref string) (string, error) {
	if e.images == nil {
		return "", errorf("E_RUNTIME_UNAVAILABLE", "image checker is not assembled in this build")
	}
	digest, err := e.images.ImageDigest(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrImageMissing) {
			return "", errorf("E_IMAGE_PULL_FAILED",
				"task image %s is not available: %v (pull it on the nodes, or configure registry credentials via 'fleetly registry set')", ref, err)
		}
		if registryPreflightErr(err) {
			return "", err
		}
		return "", errorf("E_RUNTIME_UNAVAILABLE", "task image check failed %s: %v", ref, err)
	}
	return pinDigest(ref, digest), nil
}

// EnsureTaskNetwork 幂等确保 task-group 网络在位（长活；internal=true 是
// DT-7 不可信隔离面），并落控制面服务的挂靠声明（members 幂等：新声明
// 入队成员 app 的重部署，投影双挂随发布管线收敛）。
func (e *Engine) EnsureTaskNetwork(ctx context.Context, ref string, internal bool, members []TaskNetworkMember) (string, []TaskNetworkMemberStatus, error) {
	if e.netSub == nil {
		return "", nil, errorf("E_RUNTIME_UNAVAILABLE",
			"network substrate is not assembled in this build: cannot ensure the task-group network")
	}
	name, err := naming.TaskGroupNetworkName(ref)
	if err != nil {
		return "", nil, errorf("E_TASK_UNSUPPORTED", "task-group ref %q: %v", ref, err)
	}
	labels := map[string]string{
		state.LabelManaged:   state.ManagedLabelValue,
		state.LabelTaskGroup: ref,
	}
	if internal {
		labels[state.LabelNetworkInternal] = "true"
	}
	if err := e.netSub.NetworkEnsureWithOptions(ctx, name, labels, internal); err != nil {
		return "", nil, errorf("E_RUNTIME_UNAVAILABLE", "ensure task-group network %s: %v", name, err)
	}
	out := make([]TaskNetworkMemberStatus, 0, len(members))
	for _, m := range members {
		app, aerr := e.store.GetAppByName(ctx, m.App)
		if aerr != nil {
			return "", nil, errorf("E_TASK_UNSUPPORTED", "task network member app %q: %v", m.App, aerr)
		}
		if m.Service == "" {
			return "", nil, errorf("E_TASK_UNSUPPORTED", "task network member %s: service is empty", m.App)
		}
		created, uerr := e.store.UpsertTaskNetworkMember(ctx, ref, app.ID, m.Service)
		if uerr != nil {
			return "", nil, uerr
		}
		st := TaskNetworkMemberStatus{App: app.Name, Service: m.Service, Status: "attached"}
		if created {
			// 一次性挂靠的生效腿 = 发布管线重部署（新 revision 双挂网络；
			// per-task/直改 spec 的第二写通道不存在——见文件头注）。
			deploymentID, rerr := e.EnqueueNetworkRedeploy(ctx, app.ID)
			switch {
			case rerr == nil:
				st.Status = "rolling"
				st.DeploymentID = deploymentID
			case errors.Is(rerr, ErrNoRedeploySource):
				st.Status = "pending" // 无成功部署史：下次发布生效
			default:
				return "", nil, rerr
			}
		}
		out = append(out, st)
	}
	return name, out, nil
}

// AdvanceTasks 单步执行任务收敛（测试与诊断显式入口；生产由 tick 驱动）。
func (e *Engine) AdvanceTasks(ctx context.Context) { e.advanceTasks(ctx) }

// ReapExpiredTasks 单步执行 TTL 回收（测试与诊断显式入口）。
func (e *Engine) ReapExpiredTasks(ctx context.Context) { e.reapExpiredTasks(ctx) }

// ReconcileTasks 单步执行孤儿 task 对账（测试与诊断显式入口）。
func (e *Engine) ReconcileTasks(ctx context.Context) { e.reconTasks(ctx) }

// taskNetworkProjection 读取 app 的 task-group 挂靠声明并解析为「服务名 →
// 网络名」投影（planner 双挂输入；命名违约显式失败——不静默丢投影）。
// 空声明恒 nil（无挂靠的 app 发布路径零变化）。
func (e *Engine) taskNetworkProjection(ctx context.Context, appID string) (map[string][]string, error) {
	members, err := e.store.ListTaskNetworkMembersForApp(ctx, appID)
	if err != nil {
		return nil, errorf("E_RUNTIME_UNAVAILABLE", "task network memberships for app %s: %v", appID, err)
	}
	if len(members) == 0 {
		return nil, nil
	}
	out := map[string][]string{}
	for _, m := range members {
		name, nerr := naming.TaskGroupNetworkName(m.NetworkRef)
		if nerr != nil {
			return nil, errorf("E_RUNTIME_UNAVAILABLE", "task-group network name for ref %q: %v", m.NetworkRef, nerr)
		}
		out[m.Service] = append(out[m.Service], name)
	}
	for svc := range out {
		sort.Strings(out[svc]) // 网络集合序确定（desired-hash 稳定前提）
	}
	return out, nil
}

// reapExpiredTasks 回收到期任务：queued/running 且 expires_at ≤ now →
// stopping（reason=expired）+ task.expired 事件；随后的 advanceTasks 拍
// 移除底座服务并落 stopped。sub 未接线 = duty 空转（单测/精简装配）。
func (e *Engine) reapExpiredTasks(ctx context.Context) {
	if e.sub == nil {
		return
	}
	rows, err := e.store.ExpiredTasks(ctx, e.now(), taskConvergeLimit)
	if err != nil {
		e.log.Warn("engine: task TTL scan failed", "error", err)
		return
	}
	for _, t := range rows {
		task, changed, rerr := e.store.RequestTaskStop(ctx, t.ID, "expired", "")
		if rerr != nil {
			e.log.Warn("engine: task TTL reclaim failed", "task", t.ID, "error", rerr)
			continue
		}
		if changed {
			e.log.Info("engine: task expired (reclaiming the substrate service)",
				"task", task.ID, "ttl_seconds", task.TTLSeconds)
		}
	}
}

// advanceTasks 推进全部非终态任务一拍（幂等；单行失败不阻塞其余行）。
func (e *Engine) advanceTasks(ctx context.Context) {
	if e.sub == nil {
		return
	}
	rows, err := e.store.ListNonTerminalTasks(ctx, taskConvergeLimit)
	if err != nil {
		e.log.Warn("engine: task scan failed", "error", err)
		return
	}
	for _, t := range rows {
		e.convergeTask(ctx, t)
	}
}

// convergeTask 按状态分发单条任务的收敛动作。
func (e *Engine) convergeTask(ctx context.Context, t state.Task) {
	switch t.Status {
	case state.TaskQueued:
		e.convergeQueuedTask(ctx, t)
	case state.TaskRunning:
		e.observeRunningTask(ctx, t)
	case state.TaskStopping, state.TaskDeleting:
		e.removeTaskService(ctx, t)
	}
}

// convergeQueuedTask 把 queued 任务收敛到 running：作用域网复核 → 底座服务
// create/update（缺失创建；期望哈希不符 = 外部篡改/残留 → 重申）→
// running + task.started。确定性错误（网络缺失/镜像不可得）立即 failed。
func (e *Engine) convergeQueuedTask(ctx context.Context, t state.Task) {
	if e.netSub == nil {
		e.log.Warn("engine: network substrate is not assembled; task convergence retries next tick", "task", t.ID)
		return
	}
	if err := e.sub.SwarmReady(ctx); err != nil {
		if errors.Is(err, ErrNotSwarmReady) {
			e.log.Warn("engine: swarm not ready, task convergence retries next tick", "task", t.ID)
			return
		}
		e.log.Warn("engine: task substrate check failed", "task", t.ID, "error", err)
		return
	}
	if _, err := e.netSub.NetworkInspect(ctx, t.Network); err != nil {
		if errors.Is(err, ErrNetworkNotFound) {
			e.failTask(ctx, t, fmt.Sprintf("scope network %s is absent from the substrate (tasks join existing networks only)", t.Network))
			return
		}
		e.log.Warn("engine: task scope network inspect failed (transient)", "task", t.ID, "error", err)
		return
	}
	spec, err := e.taskServiceSpec(ctx, t)
	if err != nil {
		e.failTask(ctx, t, err.Error())
		return
	}
	// 收敛原语单点（IMPL-ARCH-B）：inspect→缺失建→哈希不符重申 + 哈希标戳
	// 全在 converge.go。修复前任务 spec 从不打 LabelDesiredHash ⇒ 读回哈希
	// 恒空 ⇒ 每拍无差别重申；修复后首拍落标、后续拍哈希命中即零底座写。
	// 暂态读错原样上抛 → 告警后下一拍重试（不据此下确定性结论）。
	if err := e.convergeService(ctx, spec, convergeOptions{}); err != nil {
		e.log.Warn("engine: task service convergence failed (retrying next tick)", "task", t.ID, "error", err)
		return
	}
	if _, changed, err := e.store.MarkTaskRunning(ctx, t.ID); err != nil {
		e.log.Warn("engine: mark task running failed", "task", t.ID, "error", err)
		return
	} else if changed {
		e.log.Info("engine: task converged", "task", t.ID, "service", spec.Name, "network", t.Network)
	}
}

// observeRunningTask 观测 running 任务：底座服务缺失 → 重建（幂等收敛）；
// 最新任务 failed/rejected → failed 终态；complete → stopped（reason=exited，
// restart-condition none 的自然后果）。瞬态读错重试，不误判。
func (e *Engine) observeRunningTask(ctx context.Context, t state.Task) {
	if _, err := e.sub.ServiceInspect(ctx, t.Service); err != nil {
		if errors.Is(err, ErrServiceNotFound) {
			spec, serr := e.taskServiceSpec(ctx, t)
			if serr != nil {
				e.failTask(ctx, t, serr.Error())
				return
			}
			// 重建腿走收敛原语（IMPL-ARCH-B）：哈希标戳在原语内落，重建后的
			// 服务与首拍收敛同构（原语自查实况——缺失即建；此处 inspect 与
			// 原语 inspect 的重复只在重建这条罕见路径上，换取 switch 单点）。
			if cerr := e.convergeService(ctx, spec, convergeOptions{}); cerr != nil {
				e.log.Warn("engine: task service recreate failed (retrying next tick)", "task", t.ID, "error", cerr)
			} else {
				e.log.Warn("engine: task service was absent from the substrate (recreated by convergence)", "task", t.ID)
			}
			return
		}
		e.log.Warn("engine: task service inspect failed (transient)", "task", t.ID, "error", err)
		return
	}
	tasks, err := e.sub.TaskList(ctx, t.Service)
	if err != nil {
		e.log.Warn("engine: task task-list failed (transient)", "task", t.ID, "error", err)
		return
	}
	newest := newestTask(tasks)
	if newest == nil {
		return // 任务尚未落位（pending/new）：下一拍再看
	}
	switch newest.State {
	case "failed", "rejected":
		reason := strings.TrimSpace(newest.Err)
		if reason == "" {
			reason = "container task " + newest.State + " (restart-condition none: the platform does not self-heal task instances)"
		}
		e.failTask(ctx, t, reason)
	case "complete":
		if _, changed, rerr := e.store.RequestTaskStop(ctx, t.ID, "exited", ""); rerr != nil {
			e.log.Warn("engine: task natural-exit transition failed", "task", t.ID, "error", rerr)
		} else if changed {
			e.log.Info("engine: task container completed (restart-condition none: landing stopped)", "task", t.ID)
		}
	}
}

// removeTaskService 移除 stopping/deleting 任务的底座服务并落终态
// （stopped / 台账行删除）；幂等（服务缺失视为成功）。
func (e *Engine) removeTaskService(ctx context.Context, t state.Task) {
	if err := e.sub.ServiceRemove(ctx, t.Service); err != nil {
		e.log.Warn("engine: task service remove failed (retrying next tick)", "task", t.ID, "error", err)
		return
	}
	if t.Status == state.TaskDeleting {
		changed, err := e.store.DeleteTaskRow(ctx, t.ID)
		if err != nil {
			e.log.Warn("engine: task row delete failed", "task", t.ID, "error", err)
			return
		}
		if changed {
			e.log.Info("engine: task deleted", "task", t.ID)
		}
		return
	}
	changed, err := e.store.MarkTaskStopped(ctx, t.ID)
	if err != nil {
		e.log.Warn("engine: mark task stopped failed", "task", t.ID, "error", err)
		return
	}
	if changed {
		e.log.Info("engine: task stopped", "task", t.ID, "reason", t.StopReason)
	}
}

// failTask 把任务落 failed（确定性错误：网络缺失/镜像不可得/容器任务失败）。
func (e *Engine) failTask(ctx context.Context, t state.Task, message string) {
	changed, err := e.store.MarkTaskFailed(ctx, t.ID, message)
	if err != nil {
		e.log.Warn("engine: mark task failed", "task", t.ID, "error", err)
		return
	}
	if changed {
		e.log.Warn("engine: task failed", "task", t.ID, "error", message)
	}
}

// newestTask 返回任务列表中最新状态的一条（Timestamp 最大；空集 nil）。
func newestTask(tasks []TaskState) *TaskState {
	var newest *TaskState
	for i := range tasks {
		if newest == nil || tasks[i].Timestamp.After(newest.Timestamp) {
			newest = &tasks[i]
		}
	}
	return newest
}

// taskServiceSpec 组装任务承载服务的期望 spec（平台加固 + owner/TTL label +
// 作用域网按名加入；env 密文解密进 spec——明文只存活于内存链）。
func (e *Engine) taskServiceSpec(ctx context.Context, t state.Task) (ServiceSpec, error) {
	env, err := e.decryptTaskEnv(t)
	if err != nil {
		return ServiceSpec{}, err
	}
	return ServiceSpec{
		Name:           t.Service,
		Image:          t.Image,
		Command:        append([]string{}, t.Command...),
		Args:           append([]string{}, t.Args...),
		User:           taskPlatformUser,
		ReadOnlyRootfs: true,
		CapDrop:        append([]string{}, taskCapDrop...),
		PidsLimit:      taskPidsLimit,
		Env:            env,
		ServiceLabels: map[string]string{
			state.LabelManaged:   state.ManagedLabelValue,
			state.LabelTasks:     "true",
			state.LabelTaskID:    t.ID,
			state.LabelTaskOwner: t.OwnerTokenID,
			state.LabelTaskTTL:   strconv.FormatInt(t.TTLSeconds, 10),
		},
		ContainerLabels: map[string]string{state.LabelTaskID: t.ID},
		Replicas:        1,
		Networks:        []NetworkAttach{{Name: t.Network}},
		UpdateOrder:     "start-first",
		RestartPolicy:   &RestartPolicySpec{Condition: "none"},
		StopGracePeriod: taskStopGracePeriod,
		Resources: &ResourcesSpec{
			NanoCPUs:    t.CPUMillis * 1_000_000,
			MemoryBytes: t.MemoryBytes,
		},
	}, nil
}

// decryptTaskEnv 解密任务 env（JSON map → 字典序 KEY=VALUE 列表；密文损坏
// 显式失败——不静默丢注入面）。
func (e *Engine) decryptTaskEnv(t state.Task) ([]string, error) {
	if t.EnvCipher == "" {
		return nil, nil
	}
	if e.box == nil {
		return nil, fmt.Errorf("task %s carries env but the platform key box is not assembled", t.ID)
	}
	plain, err := e.box.Decrypt([]byte(t.EnvCipher))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt task env (master key mismatch or corrupted ciphertext): %w", err)
	}
	var envMap map[string]string
	if err := json.Unmarshal(plain, &envMap); err != nil {
		return nil, fmt.Errorf("task env payload is not a JSON object: %w", err)
	}
	out := make([]string, 0, len(envMap))
	for k, v := range envMap {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

// reconTasks 是存在性对账的 tasks 面（substrateRecon 同拍调用；DT-5 对账
// 兜底守卫）：底座带任务 label 的服务在 state 无对应非终态行 → 孤儿 task
// 披露（event + 审计，按服务名节流）+ 回收（归属明确的本平台对象，派生
// 修正——行已终态或不存在 = 该服务不应存在）。读错不结论（不事件、不动作、
// 不清记忆——services/networks 面同纪律）。
func (e *Engine) reconTasks(ctx context.Context) {
	if e.sub == nil {
		return
	}
	rows, err := e.sub.ServiceList(ctx, map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelTasks:   "true",
	})
	if err != nil {
		e.log.Warn("engine: task recon list failed (transient, no conclusions)", "error", err)
		return
	}
	orphanSet := map[string]bool{}
	for _, svc := range rows {
		if !naming.IsTaskServiceName(svc.Name) {
			continue // label 漂移的异物：不越权处置（披露面归 drift/人工）
		}
		taskID := svc.Labels[state.LabelTaskID]
		if taskID != "" {
			if row, gerr := e.store.GetTask(ctx, taskID); gerr == nil && !row.Status.Terminal() {
				continue // 在途：收敛归 advanceTasks，非孤儿
			} else if gerr != nil && !errors.Is(gerr, state.ErrTaskNotFound) {
				e.log.Warn("engine: task recon row read failed (transient, no conclusions)", "service", svc.Name, "error", gerr)
				continue
			}
		}
		orphanSet[svc.Name] = true
		// 持续形态只报一次（回收成功/条件解除清零可再报）；事件+审计同
		// 事务，事务失败不标记——下一拍重试（disclosure.go 单点）。
		reported, rerr := e.discloseOnce(ctx, &e.taskOrphanSeen, svc.Name, taskOrphanDisclosure(svc))
		if rerr != nil {
			e.log.Warn("engine: report orphan task", "service", svc.Name, "error", rerr)
			continue
		}
		if reported {
			e.log.Warn("engine: task service without a non-terminal state row (disclosed and reclaimed)",
				"service", svc.Name, "task", taskID)
		}
		if rerr := e.sub.ServiceRemove(ctx, svc.Name); rerr != nil {
			e.log.Warn("engine: orphan task reclaim failed (next beat retries)", "service", svc.Name, "error", rerr)
			continue
		}
		e.taskOrphanSeen.clear(svc.Name) // 回收成功：记忆清零可再报
	}
	e.taskOrphanSeen.sweep(orphanSet) // 恢复/回收成功：记忆清零可再报
}

// taskOrphanDisclosure 构造孤儿 task 披露载荷（事件 task.orphaned + 审计
// reconcile.task_orphaned，同事务 fail-closed）。payload 只带事实字段
// （服务名/task 归属/属主），零敏感材料。
func taskOrphanDisclosure(svc ServiceState) disclosure {
	taskID := svc.Labels[state.LabelTaskID]
	owner := svc.Labels[state.LabelTaskOwner]
	return disclosure{
		eventCode:   "task.orphaned",
		subject:     "task:" + taskID,
		payload:     []string{"service", svc.Name, "task", taskID, "owner_token", owner},
		auditAction: "reconcile.task_orphaned",
		auditTarget: "task:" + taskID,
		auditDiff:   state.DiffSummary("service", svc.Name, "task", taskID, "owner_token", owner),
	}
}
