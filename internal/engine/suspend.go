package engine

// 应用挂起（app Stop/Start，00028 位；2026-09-29 Console dokploy 对齐）：
// suspended 是行上权威位——本文件是「状态驱动渲染」的保持器（DB paused 的
// renderService(inst, pw, 0) 同型）：API 只翻位，副本排水由周期对账按位
// 落地与保持。恢复走 resume 清位 + active revision 重部署管线（rollback.go
// 入队；入队面带挂起门）。派生投影（suspended 第一判）在 recovery.go；
// drift/autoscaler/cron 豁免在各消费点。

import (
	"context"
	"errors"
)

// drainSuspendedApp 把挂起应用的受管长驻服务副本排水到 0（app Stop 的执行
// 腿）：副本 >0 的 replicated 服务逐个 ServiceUpdate 压 0；已为 0 / global
// （无副本标量，排水不达——挂起前请改 replicated，文档口径）/ 缺失（不补
// 建——挂起不是部署）的服务零动作；多余服务不回收（归各自既有路径）。无
// 成功部署 = 无期望集，位面语义照旧（无物可排）。整应用遇底座瞬态错误放弃
// 本拍（与 reconAppSubstrate 同纪律：部分服务未核实就继续写不是安全形态）。
// 排水拍尾刷新派生基线（suspended 直投影进 apps.derived_state——事件比较
// 基准随权威位走）。
func (e *Engine) drainSuspendedApp(ctx context.Context, appID, appName string) {
	_, specs, err := e.lastSucceededSpecs(ctx, appID)
	if err != nil {
		e.log.Warn("engine: suspended app drain read expectations", "app", appName, "error", err)
		return
	}
	for i := range specs {
		st, err := e.sub.ServiceInspect(ctx, specs[i].Name)
		if err != nil {
			if errors.Is(err, ErrServiceNotFound) {
				continue // 缺失不补建：挂起不是部署（恢复走 resume 重部署管线）
			}
			e.log.Warn("engine: suspended app drain inspect failed (transient, retry next pass)",
				"app", appName, "service", specs[i].Name, "error", err)
			return
		}
		if st.Global || st.Replicas == 0 {
			continue // global 无副本标量（排水不达，如实口径）；已为 0 零动作
		}
		spec := specs[i]
		spec.Replicas = 0
		if err := e.sub.ServiceUpdate(ctx, spec.Name, spec); err != nil {
			e.log.Warn("engine: suspended app drain scale-to-zero failed (transient, retry next pass)",
				"app", appName, "service", spec.Name, "error", err)
			return
		}
		e.log.Info("engine: suspended app service drained to zero",
			"app", appName, "service", spec.Name)
	}
	if err := e.refreshDerivedState(ctx, appID, appName); err != nil {
		e.log.Warn("engine: suspended app drain refresh derived state", "app", appName, "error", err)
	}
}
