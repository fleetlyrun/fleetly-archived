package engine

// 服务收敛原语（IMPL-ARCH-B）：「服务收敛原语——缺失→建；期望哈希不符→
// 全量重申——」在引擎里唯一存在于此。历史上部署线（applyDesired）与任务线
// （convergeQueuedTask / observeRunningTask 重建腿）各持一份手写 switch，
// 且任务线从不打 fleetly.desired-hash 标 ⇒ 读回哈希恒空 ⇒ 每拍无差别重申
//（同内容 ServiceUpdate 不触发任务重建，Spike B2，真实代价只是每拍无谓
// 往返 + 「篡改/残留才重申」的辨别语义从未可用）。
//
// 纪律：哈希标戳（LabelDesiredHash）在原语内完成——调用方不可能忘；引擎
// 包内不得再出现原语之外的收敛 switch（convergescan_test.go 白名单扫描守
// 卫强制）。底座写语义（ForceUpdate 恒不递增 = 同内容重申零任务替换）归
// internal/substrate 适配器，不在本原语管辖。

import (
	"context"
	"errors"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// convergeOptions 是收敛原语的调用方差异面。任务线恒零值；部署线的特有
// 条件（force 重放恒重申、LabelDeployment 换代即更新）经此传入——它们是
// switch 的第三、第四判据，若由调用方在原语之上自行组合，switch 就会在
// applyDesired 复制出第二份，违背单点纪律。
type convergeOptions struct {
	// force 为 true 时跳过哈希捷径恒重申（归位/回滚/漂移收敛的重放路径）：
	// label 是上次平台写的存根，外部改动不清理它——期望与 label 相等不代表
	// 实况未被篡改，重放必须以 swarm 侧 spec 重申为准。
	force bool
	// deploymentID 非空 = 部署线：建/重申前打 LabelDeployment 归属标，且
	// 实况归属与本次发布不符（换代）即重申——服务 label 以当前发布归属
	// 重写（任务零替换，Spike B2）。空 = 任务线：不带部署归属语义。
	deploymentID string
}

// convergeServiceObserved 以调用方已取得的实况投影收敛单只服务（批量对账
// 面的入口——applyDesired 一次 ServiceList 建全量 byName，逐服务复用，不
// 逐个 inspect）。exists = 该服务当前是否存在；不存在时 cur 零值即可。
func (e *Engine) convergeServiceObserved(ctx context.Context, spec ServiceSpec, cur ServiceState, exists bool, opts convergeOptions) error {
	// 哈希标戳单点：canonical JSON 不含服务 label（DesiredHash 纪律）——
	// 与部署归属标的写入先后无关，哈希输入恒为 spec 全字段。调用方（规划/
	// 快照路径）可能已带同值标，原语恒以现算值重写：语义是「调用方不可能
	// 忘」的机械保证，而非数据来源。
	if spec.ServiceLabels == nil {
		spec.ServiceLabels = map[string]string{}
	}
	if opts.deploymentID != "" {
		spec.ServiceLabels[state.LabelDeployment] = opts.deploymentID
	}
	hash := spec.DesiredHash()
	spec.ServiceLabels[state.LabelDesiredHash] = hash

	switch {
	case !exists:
		return e.sub.ServiceCreate(ctx, spec)
	case opts.force ||
		cur.DesiredHash != hash ||
		(opts.deploymentID != "" && cur.Labels[state.LabelDeployment] != opts.deploymentID):
		// 重申腿：force（重放恒重申）∨ 期望哈希不符（外部篡改/残留）∨
		// 部署换代（归属换 release）。全量 spec 重申；同内容更新零任务替换。
		return e.sub.ServiceUpdate(ctx, spec.Name, spec)
	}
	return nil // 哈希命中：零底座写（收敛幂等）
}

// convergeService 自查实况后收敛单只服务（无批量清单的调用方——任务线的
// 收敛与重建腿）。inspect 暂态错误原样上抛（调用方告警后下一拍重试，不得
// 据此下确定性结论）。
func (e *Engine) convergeService(ctx context.Context, spec ServiceSpec, opts convergeOptions) error {
	cur, err := e.sub.ServiceInspect(ctx, spec.Name)
	if err != nil {
		if !errors.Is(err, ErrServiceNotFound) {
			return err
		}
		return e.convergeServiceObserved(ctx, spec, ServiceState{}, false, opts)
	}
	return e.convergeServiceObserved(ctx, spec, cur, true, opts)
}
