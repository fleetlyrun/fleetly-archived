package engine

// 服务归属过滤与一次性 job 豁免的单点定义（IMPL-ARCH-A）。
//
// 「一个 app 的受管服务如何按 label 圈定」在此唯一产出：fleetly.managed=
// true + fleetly.app=<三段限定形 team/prj/app>（W2-S3 起 planner 经
// naming.ServiceLabels 写入该形态）。历史上散落多处的手写过滤 map 中，
// drift.go 的 extras 腿仍用裸 app.Name 构造——label 精确匹配恒空，漂移判
// 定的「期望集之外的多余受管服务也是漂移」腿自 W2-S3 起失效（本文件收敛
// 后由 ownership_test.go 红→绿钉死）。新增按归属圈定受管服务的读面一律经
// 本文件原语，不得再手写过滤 map（源码扫描守卫 TestNoHandWrittenAppLabel
// Filters 强制，白名单见该测试注）。

import (
	"context"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// qualifiedServiceFilter 产出按限定形 app 值圈定受管服务的过滤 map（唯一
// 手写构造点）。入参是三段限定形 `team/prj/app` 字符串——供只持旧限定形
// 的清扫面（SweepMovedServices 摘除改派前的旧命名上下文）消费；持有应用
// 行的调用方用 appServiceFilter。
func qualifiedServiceFilter(qualifiedApp string) map[string]string {
	return map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelApp:     qualifiedApp,
	}
}

// appServiceFilter 产出 app 受管服务的归属过滤 map（值 = QualifiedName 三
// 段限定形——与 planner 的 label 写入同源同形）。
func appServiceFilter(app state.App) map[string]string {
	return qualifiedServiceFilter(app.QualifiedName())
}

// oneShotJobService 报告服务名是否为一次性 job 服务（cron / init 两个前缀
// 族）。豁免谓词的唯一落点：这类服务是平台瞬时对象（生命周期归 cron 调度
// 器与 init 相位/孤儿清扫），不属于「期望集之外的多余服务」——发布对账的
// 省略=删除、漂移 extras 判定、改派清扫对它们让位（误删在途 job = 运行/
// 迁移静默丢失）。
func oneShotJobService(name string) bool {
	return naming.IsCronJobName(name) || naming.IsInitJobName(name)
}

// scopeManagedServices 列出 app 名下的受管**长驻**服务：归属过滤 + 一次性
// job 服务豁免（漂移 extras 腿等「多余服务」判定的对账域原语）。任务承载
// 服务天然不在结果内——它们不携带 fleetly.app（state.LabelTasks 契约），
// 归属过滤即可排除。
func (e *Engine) scopeManagedServices(ctx context.Context, app state.App) ([]ServiceState, error) {
	existing, err := e.sub.ServiceList(ctx, appServiceFilter(app))
	if err != nil {
		return nil, err
	}
	out := make([]ServiceState, 0, len(existing))
	for _, s := range existing {
		if oneShotJobService(s.Name) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
