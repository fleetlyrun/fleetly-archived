package engine

// 「披露一次」骨架单点（IMPL-ARCH-E，2026-09-28 架构评审首推候选）。
//
// 此前该 idiom 在引擎里手写约十遍：8 个 xxxSeen 裸 map 字段各带一份逐字
// 近同的契约散文、六处散布的 delete 恢复清零环、report* 的 InTx→事件→
// 审计重复事务形态、6 个 xxxNextAt 节拍门字段各带一份「tick goroutine
// 专用」注释。T 线加 tasks face 时整套又抄了一遍（IMPL-F1 修复时还被迫
// 在两处散布块里各动一刀）——拷贝漂移的实证成本。本文件把骨架收成一份
// 逻辑，各对账面（networks/tasks/substrate/drift/autoscaling）全部消费
// 此处，不再持有第二份：
//
//  1. disclosureSet —— seen 记忆 + once 语义 + 恢复清零。契约（全引擎
//     仅此一份）：持续异常形态只报一次；恢复/条件解除清零可再报；进程
//     重启清零 = 重报一次——重复优于漏报。
//  2. disclosure / writeDisclosure / discloseTx / discloseOnce —— 事件 +
//     审计同事务配对（fail-closed：任一失败整体回滚，不产生只有事件没有
//     审计的半程披露）；discloseOnce 叠加「首见才落库、事务成功才标记」。
//  3. scanGate —— tick 步的频控节拍门（到期判定 + 推进一份逻辑）。
//
// 事件码只增纪律：本文件不持有任何事件码/审计 action 字符串字面量——
// 全部由各 face 的披露载荷构造点沿用既有注册码。读错不结论的瞬态守卫
// 不属于本 module——留在各对账调用面。internal/state/janitor.go 的
// staleSeen/reportStale 是 state 包同款拷贝，跨包不在本票范围（挂账）。

import (
	"context"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// ── seen 记忆（once 语义 + 恢复清零）────────────────────────────────────────

// disclosureSet 是「披露一次」的进程内记忆（键 = 对账对象：网络名/服务名/
// appID/策略键）。零值即可用（记忆为空）。非并发安全：只在对账步 的
// 调用栈上使用（tick goroutine 专用，与收敛前的裸 map 同纪律）。
type disclosureSet struct {
	seen map[string]bool
}

// reported 报告 key 是否已在记忆中（持续形态的节流判定：在 = 已报过，跳过）。
func (s *disclosureSet) reported(key string) bool { return s.seen[key] }

// mark 在披露落库成功后标记（先标后报的 face 在披露前调用——时机契约
// 归各 face，见 drift.go driftScan 的逐面核对注）。
func (s *disclosureSet) mark(key string) {
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[key] = true
}

// clear 单键清零（恢复/回收/解除——可再报）。
func (s *disclosureSet) clear(key string) { delete(s.seen, key) }

// sweep 恢复清零：present（本拍对账实际在场的键集）之外的 key 全部清零
// ——孤儿/缺失面每拍尾部的「不在场即忘」循环单点。
func (s *disclosureSet) sweep(present map[string]bool) {
	for key := range s.seen {
		if !present[key] {
			delete(s.seen, key)
		}
	}
}

// ── 事件 + 审计同事务配对 ──────────────────────────────────────────────────

// disclosure 是一次披露的落库载荷：事件（码/subject/kv 载荷）+ 同事务
// 审计（action/target/diff）。auditAction 非空时同事务补审计，恒
// Actor=system、Result=ok（披露族的固定形态；ErrorCode/RequestID/
// ActorTokenID 恒空）。auditAction 空 = 仅事件披露（scaling.dormant /
// scaling.no_data 族无审计面）。
type disclosure struct {
	eventCode   string
	subject     string
	payload     []string
	auditAction string
	auditTarget string
	auditDiff   string
}

// writeDisclosure 在既有事务内落一次披露（事件先、审计后——与收敛前各
// report* 的形态逐字段一致）。调用方持有事务边界：substrate 两面的派生
// 态 CAS 与披露同事务，经此单点组合。
func writeDisclosure(ctx context.Context, tx *state.Tx, d disclosure) error {
	if err := appendEvents(ctx, tx, eventOf(d.eventCode, d.subject, d.payload...)); err != nil {
		return err
	}
	if d.auditAction == "" {
		return nil
	}
	return tx.WriteAudit(ctx, state.AuditEntry{
		Actor:       "system",
		Action:      d.auditAction,
		Target:      d.auditTarget,
		Result:      "ok",
		DiffSummary: d.auditDiff,
	})
}

// discloseTx 在独立事务内落一次披露（report* 事务对的 InTx 单点）。
// seen 判定与标记时机归调用面——drift 面「先标后报」的既有语义由此保留。
func (e *Engine) discloseTx(ctx context.Context, d disclosure) error {
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		return writeDisclosure(ctx, tx, d)
	})
}

// discloseOnce 是「首见才报」单点：记忆命中即跳过（reported=false，零事务）；
// 首见落库（事件 + 审计同事务），事务成功才标记——事务失败不标记，下一拍
// 重试（披露不丢）。返回本拍是否落库。
func (e *Engine) discloseOnce(ctx context.Context, set *disclosureSet, key string, d disclosure) (bool, error) {
	if set.reported(key) {
		return false, nil
	}
	if err := e.discloseTx(ctx, d); err != nil {
		return false, err
	}
	set.mark(key)
	return true, nil
}

// ── 30s 节拍门 ─────────────────────────────────────────────────────────────

// scanGate 是 tick 步的频控节拍门（此前 6 个 xxxNextAt time.Time 字段的
// 单点：到期判定 + 推进一份逻辑）。零值即刻到期——重启即清零 = 重启后
// 立即扫一拍的既有语义。tick goroutine 专用（Run 单 goroutine 驱动，无
// 并发访问）。恢复重试门（recoveryNextAt）不在此列：它是「登记待重试时
// 臂闸」语义——arm 时推进、读时不推进，与本类型「读时到期即推进」相反，
// 且与 recoveryPending/recoveryStuck 同属 M1-8 一个机制——保留 recovery.go
// 原样（IMPL-ARCH-E 汇总有记）。
type scanGate struct {
	nextAt time.Time
}

// due 报告本拍是否执行：force 直通（测试与诊断显式入口）；非 force 形态
// 未到期返回 false（闸不推进）。到期即推进到 now+interval（force 拍同样
// 推进——与收敛前各步的形态一致）。
func (g *scanGate) due(now time.Time, force bool, interval time.Duration) bool {
	if !force && now.Before(g.nextAt) {
		return false
	}
	g.nextAt = now.Add(interval)
	return true
}
