package engine

// 应用运行实况投影（2026-09-29 Console IA 重设计，设计
// docs/design/2026-09-29-console-ia-redesign.md §5）：把观测底座已有的
// ServiceList/TaskList（ports.go Substrate 端口）按 app 上抛为只读报告
// ——此前该实况只被引擎观测循环内部消费（observing/drift/releasing），
// Console 无「实际运行的容器」可见面。只投影不判定：本文件不做漂移/健康
// 结论（那是 drift.go 与 observing.go 的域），只回答「现在跑的是什么」。
//
// 服务集 = 期望集（最近 succeeded 部署快照的长驻 spec——decodeSpecs 已滤
// Job 模板，声明副本经活覆盖钉入，与 drift 同款红线②）∪ 实况集
//（scopeManagedServices：归属过滤 + 一次性 job 豁免）。期望集有而实况无
// → Missing（absent，与 drift missing 同判）；实况多余（期望集之外）→
// 照列（不判 drift——多余服务的判定与收敛归 drift 面）。
//
// 任务 = TaskList 全量（含历史——swarm task 语义：滚动更新后旧任务仍在
// 列表，desired_state=shutdown/remove 标注下线；调用方按 state 过滤）。
// ActualReplicas = state=running 且 desired_state=running 的任务数——
// 「实际运行水位」的唯一定义点（与 docker service ps 的 running 判读一致）。
//
// 引擎调用方（internal/api/runtime.go）恒传三段限定形（app.QualifiedName()
// ——DriftService 同款纪律）：GetAppByName 只认限定形/唯一裸名。

import (
	"context"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// AppRuntimeService 是单服务的运行实况（期望∪实况并集投影）。
type AppRuntimeService struct {
	Name string
	// Service 是 compose 服务名（期望集内取 spec 的 fleetly.process
	// label；期望集外的实况多余服务无对应键，留空）——调用方按它与
	// 快照服务清单对位（Name 是 swarm 全名）。
	Service string
	// Image 取实况（服务在场时——swarm 会把 tag 归一为 digest 形态）或
	// 期望快照（服务 absent 时）。
	Image string
	// Global 是 global 模式（实况优先，absent 时取期望 spec）。
	Global bool
	// DeclaredReplicas 是期望副本（快照 spec，活覆盖钉入；global 恒 0
	// ——与 driftProjection 的 M1-3 归一同理，global 无受管副本语义）。
	DeclaredReplicas uint64
	// ActualReplicas 是 state=running 且 desired_state=running 的任务数。
	ActualReplicas uint64
	// UpdateState/UpdateMessage 逐字镜像底座 UpdateStatus
	//（''/updating/paused/completed；'' = 未投影/absent）。
	UpdateState   string
	UpdateMessage string
	// Missing = 期望集有而 Swarm 无此服务。
	Missing bool
	// Tasks 是全部任务（含历史，TaskList 原序）。
	Tasks []TaskState
}

// AppRuntimeReport 是应用运行实况报告。
type AppRuntimeReport struct {
	App string
	// DesiredDeployment 是期望态来源部署（空 = 无成功部署记录——服务集
	// 仅实况集）。
	DesiredDeployment string
	Services          []AppRuntimeService
}

// AppRuntime 即时投影应用运行实况（只读；不写事件不收敛）。
func (e *Engine) AppRuntime(ctx context.Context, appName string) (*AppRuntimeReport, error) {
	app, err := e.store.GetAppByName(ctx, appName)
	if err != nil {
		return nil, err
	}
	report := &AppRuntimeReport{App: app.Name}
	source, specs, err := e.lastSucceededSpecs(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		// 无成功部署：无期望态且对账域为空——与 drift show 同款早退（不触
		// 底座，nil 底座夹具/无部署 app 的调用零底座依赖）；残留服务本就
		// 无期望可对，平台对账（driftScan）同口径不枚举。
		return report, nil
	}
	report.DesiredDeployment = source.ID
	overrides, err := e.store.ListScalingReplicaOverrides(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	pinScalingReplicaOverrides(specs, overrides, source.ID)
	desired := make(map[string]*ServiceSpec, len(specs))
	for i := range specs {
		desired[specs[i].Name] = &specs[i]
	}
	existing, err := e.scopeManagedServices(ctx, app)
	if err != nil {
		return nil, err
	}
	actual := make(map[string]*ServiceState, len(existing))
	for i := range existing {
		actual[existing[i].Name] = &existing[i]
	}
	// 并集顺序：期望集序优先（快照序 = 用户 compose 服务序），实况多余殿后
	//（与 drift 的 report.Services 序惯例一致）。
	names := make([]string, 0, len(desired)+len(actual))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	for _, s := range existing {
		if _, ok := desired[s.Name]; !ok {
			names = append(names, s.Name)
		}
	}
	report.Services = make([]AppRuntimeService, 0, len(names))
	for _, name := range names {
		out := AppRuntimeService{Name: name}
		if spec, ok := desired[name]; ok {
			out.Service = spec.ServiceLabels[state.LabelProcess]
			out.DeclaredReplicas = spec.Replicas
			out.Global = spec.Global
			out.Image = spec.Image
		} else if st, ok := actual[name]; ok && st.Labels != nil {
			out.Service = st.Labels[state.LabelProcess]
		}
		st, ok := actual[name]
		if !ok {
			out.Missing = true
			report.Services = append(report.Services, out)
			continue
		}
		// 实况在场：镜像/形态/update 以实况为准（swarm 真值）。
		out.Image = st.Image
		out.Global = st.Global
		out.UpdateState = st.UpdateState
		out.UpdateMessage = st.UpdateMessage
		tasks, err := e.sub.TaskList(ctx, name)
		if err != nil {
			return nil, err
		}
		out.Tasks = tasks
		for _, t := range tasks {
			if t.State == "running" && t.DesiredState == "running" {
				out.ActualReplicas++
			}
		}
		report.Services = append(report.Services, out)
	}
	return report, nil
}
