package state

// 状态机转移表（行驱动权威真源；S16-C5 自 internal/engine/machine.go 下沉
// ——state 是台账行的权威状态层，写路径在此校验 from→to 合法性，非法转移
// 拒写）：
//   - 部署（release-semantics §2.3；engine/machine.go 保留 Phase 词表与同名
//     兼容导出、委托本表，表不两份）；
//   - 任务（T 线 DT-5；IMPL-ARCH-C2 自 tasks.go 六个手搓 CAS 块显式化——
//     图与既有 WHERE 谓词逐条对应，不是重新设计，见 taskTransitions 注）。
//
// 转移纪律：
//   - 终态（succeeded/failed/cancelled）不可逆（表内无出边）；
//   - observing 不可取消（曾健康不可 cancel，409 语义）——表内无
//     observing → cancelled 边，cancel 准入由引擎按 first_healthy_at 预检
//     后走 releasing → cancelled；
//   - 引擎外的兜底失败（控制面重启无法判定）允许非终态直达 failed。
//
// 现存合法转换全集（与引擎写点一一对应，枚举见 machine_test）：
// queued→preparing（startQueued 拾取）、preparing→building（build 分路）、
// preparing/building→releasing（planAndRelease 与回滚 preflight 后）、
// releasing→observing（切流 enterObserving）、observing→succeeded（观察窗
// 通过）、非终态→failed（失败分流/兜底）、queued/preparing/building/releasing
// →cancelled（取消语义按切流与否分流）。

import (
	"errors"
	"fmt"
)

// ErrIllegalTransition 表示按转移表判定的非法状态迁移（from→to 无边——
// 确定性编码错误）。它是 ErrDeploymentStateTransition 的特化（包装链）：
// 既有调用方按「迁移非法家族」处理（errors.Is 双哨兵均真——引擎对竞争
// 落败的幂等收敛语义不变）；需要区分「表外组合」与「CAS 竞争落败」的
// 调用方（测试、诊断）精确匹配本哨兵——前者应修调用方，后者应重读状态
// 后裁决。
var ErrIllegalTransition = fmt.Errorf("%w: illegal per transition table", ErrDeploymentStateTransition)

// deploymentTransitions 是合法转移表（穷举定义；表外全部非法）。行 = from，
// 列集合 = 允许的 to。
var deploymentTransitions = map[DeploymentStatus][]DeploymentStatus{
	DeployQueued:    {DeployPreparing, DeployFailed, DeployCancelled},
	DeployPreparing: {DeployBuilding, DeployReleasing, DeployFailed, DeployCancelled},
	DeployBuilding:  {DeployReleasing, DeployFailed, DeployCancelled},
	DeployReleasing: {DeployObserving, DeployFailed, DeployCancelled},
	DeployObserving: {DeploySucceeded, DeployFailed},
	// 终态无出边。
}

// CanTransitionDeployment 报告 from → to 是否合法转移（未知状态、终态出边、
// 表外组合一律非法）。
func CanTransitionDeployment(from, to DeploymentStatus) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from.Terminal() {
		return false
	}
	for _, t := range deploymentTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// LegalDeploymentTransitions 返回 from 的合法目标集合（穷举测试断言用；
// from 为终态或未知状态返回空）。
func LegalDeploymentTransitions(from DeploymentStatus) []DeploymentStatus {
	out := make([]DeploymentStatus, 0, len(deploymentTransitions[from]))
	if !from.Valid() || from.Terminal() {
		return out
	}
	return append(out, deploymentTransitions[from]...)
}

// AllDeploymentStatuses 返回主状态全词表（穷举测试遍历用）。
func AllDeploymentStatuses() []DeploymentStatus {
	return []DeploymentStatus{DeployQueued, DeployPreparing, DeployBuilding, DeployReleasing,
		DeployObserving, DeploySucceeded, DeployFailed, DeployCancelled}
}

// ErrIllegalTaskTransition 表示按任务转移表判定的非法状态迁移（from→to
// 无边——确定性编码错误）。与部署侧 ErrIllegalTransition 同纪律：任务状态
// 单写点（updateTask）在 CAS 前按表校验，表外组合拒写——即使当前行恰为
// from 也不落库。它不服务「竞争落败」：任务线六个意图原语的既有幂等语义
// （n==0 零变更成功返回）不受影响，表校验针对的是编码错误。
var ErrIllegalTaskTransition = errors.New("task transition illegal per the transition table")

// taskTransitions 是任务合法转移表（穷举定义；表外全部非法）。行 = from，
// 列集合 = 允许的 to。图自 T2-1 六个写点原语的既有 WHERE 谓词忠实提取
// （IMPL-ARCH-C2 显式化，语义逐条对应）：
//
//	MarkTaskRunning    queued → running（旧谓词 AND status = queued）
//	RequestTaskStop    queued/running → stopping（旧幂等跳过集 = stopping/终态/deleting）
//	RequestTaskDelete  queued/running/stopping/stopped/failed → deleting（任意非 deleting——终态行也进删除墓碑）
//	MarkTaskStopped    stopping → stopped（旧谓词 AND status = stopping）
//	MarkTaskFailed     queued/running/stopping → failed（旧谓词 NOT IN 终态+deleting）
//
// 终态（stopped/failed）除 → deleting（删除墓碑受理任何非 deleting 行）
// 外无出边；deleting（墓碑）无出边——墓碑的出口是 DeleteTaskRow 的行删除
// （DELETE ... AND status = deleting），不是状态转移，不入本表。
var taskTransitions = map[TaskStatus][]TaskStatus{
	TaskQueued:   {TaskRunning, TaskStopping, TaskFailed, TaskDeleting},
	TaskRunning:  {TaskStopping, TaskFailed, TaskDeleting},
	TaskStopping: {TaskStopped, TaskFailed, TaskDeleting},
	TaskStopped:  {TaskDeleting},
	TaskFailed:   {TaskDeleting},
	// 删除墓碑（deleting）无出边。
}

// CanTransitionTask 报告 from → to 是否合法转移（未知状态、墓碑出边、表外
// 组合一律非法；终态保留 → deleting 的墓碑受理边，其余出边全无）。
func CanTransitionTask(from, to TaskStatus) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == TaskDeleting {
		return false
	}
	for _, t := range taskTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// LegalTaskTransitions 返回 from 的合法目标集合（穷举测试断言用；from 为
// 墓碑或未知状态返回空）。
func LegalTaskTransitions(from TaskStatus) []TaskStatus {
	out := make([]TaskStatus, 0, len(taskTransitions[from]))
	if !from.Valid() || from == TaskDeleting {
		return out
	}
	return append(out, taskTransitions[from]...)
}

// AllTaskStatuses 返回任务状态全词表（穷举测试遍历用）。
func AllTaskStatuses() []TaskStatus {
	return []TaskStatus{TaskQueued, TaskRunning, TaskStopping, TaskStopped, TaskFailed, TaskDeleting}
}
