// Package componentloop 是受管组件 Manager 循环骨架的唯一一份
// （2026-09-30 架构评审 C7 收编：此前六件骨架在
// metrics/rustfs/victorialogs/execrelay/ingress/database 各持逐字拷贝
// ——retryOrScan/sleep 形/同序比对/设置读取预算/平台事件发射/网络挂载
// 回解析——execrelay 入族时整段复制 victorialogs（头注自证），复制链
// 仍活；新受管组件接入自本包取骨架，零拷贝）。
//
// 形态纪律（沿 C1/dockerapi 既立边界）：
//   - 本包只持「族内逐字同构」的循环骨架件；各包的收敛决策、spec 构造
//     与比对面（specEqual）、窄端口（消费方定义端口的 Go 惯例）、
//     ErrNotSwarmReady 哨兵（同语义不共享类型）与凭据链全部留属主；
//   - statebackup 的 emitEvent 变体（string 载荷、Subject 组装异形）刻意
//     不入场——非逐字同构，语义属备份上传轨。
package componentloop

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// SettingsLoadTimeout 是 status/健康检查面的设置读取预算（CheckHealth
// 无调用方 ctx 形态的统一预算；原四份同名常量收编）。
const SettingsLoadTimeout = 3 * time.Second

// SleepCtx 睡到 d 到期或 ctx 取消（false = ctx 已取消；d<=0 即时让出）。
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RetryOrScan 收敛失败/未收敛走短退避，已收敛走扫描周期（漂移复检节奏）。
func RetryOrScan(retry, scan time.Duration, converged bool) time.Duration {
	if converged && scan > 0 {
		return scan
	}
	return retry
}

// SameStrings 序列相等（顺序敏感——spec 各面以期望序权威表达）。
func SameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ResolveNetworkNames 把实况网络挂载目标（创建期被 engine 归一为网络
// ID——"host" 亦然）就地解析回名，供同锚比对；解析失败显式上返=调用方
// 退避重试，不误判漂移。resolve 通常是各包窄端口的 NetworkName 方法值。
func ResolveNetworkNames(ctx context.Context, targets []string, resolve func(context.Context, string) (string, error)) error {
	for i, t := range targets {
		n, err := resolve(ctx, t)
		if err != nil {
			return err
		}
		targets[i] = n
	}
	return nil
}

// EmitEvent 追加平台事件（Outbox 单写；失败只日志——事件披露不阻断
// 收敛）。component 仅作失败日志前缀（各 Manager 既有日志口径）；事件
// 名须为调用方在 eventcode 注册的码（state 只收注册码，本骨架不持码）。
func EmitEvent(ctx context.Context, st *state.Store, log *slog.Logger, component, name, subject string, payload map[string]string) {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	err = st.InTx(ctx, func(tx *state.Tx) error {
		_, err := tx.AppendEvent(ctx, state.Event{Name: name, Subject: subject, Payload: string(raw)})
		return err
	})
	if err != nil {
		log.Warn(component+": event append failed", "event", name, "error", err)
	}
}
