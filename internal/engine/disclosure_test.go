package engine

// 「披露一次」骨架（disclosure.go，IMPL-ARCH-E）的 module 直接契约测试：
// 一张表测试钉住——同一 key 首见报一次；sweep 移除 present 外 key 后可再报；
// 不同 key 相互独立；事务失败不标记（下次重试仍报）；事件 + 审计各恰一条
// 且审计 Action = 事件码。另以小测试钉住 scanGate 的「零值即刻到期 / 闸内
// 跳过 / force 直通且同样推进」语义。事件码沿用既有注册码（network.orphaned
// 族）——本文件不新增任何事件码/审计 action 字符串。

import (
	"context"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// disclosureProbe 构造契约测试的单键披露载荷（事件码与审计 action 取同串
// ——reportDrift / reportSubstrateMissing 族的既有形态，即「Action=事件码」
// 断言的被测面；subject/target 取网络面同款 network:<name> 形）。
func disclosureProbe(code, key string) disclosure {
	return disclosure{
		eventCode:   code,
		subject:     "network:" + key,
		payload:     []string{"network", key},
		auditAction: code,
		auditTarget: "network:" + key,
		auditDiff:   state.DiffSummary("network", key),
	}
}

// countAuditsByAction 统计至今某 action 的审计行数（事件侧的配对面）。
func countAuditsByAction(t *testing.T, h *harness, action string) int {
	t.Helper()
	rows, err := h.store.RecentAudits(context.Background(), 1000)
	if err != nil {
		t.Fatalf("recent audits: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Action == action {
			n++
		}
	}
	return n
}

func TestDisclosureModuleContract(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(t *testing.T, h *harness)
	}{
		{
			// 同一 key 首见报一次：第二次披露既不落库也不报错（节流，
			// 非错误路径）；事件与审计各恰一条。
			name: "first sighting reports exactly once",
			run: func(t *testing.T, h *harness) {
				ctx := context.Background()
				var set disclosureSet
				key := "fleetly-orphan-once"
				reported, err := h.eng.discloseOnce(ctx, &set, key, disclosureProbe("network.orphaned", key))
				if err != nil || !reported {
					t.Fatalf("first discloseOnce = (%t, %v), want (true, nil)", reported, err)
				}
				reported, err = h.eng.discloseOnce(ctx, &set, key, disclosureProbe("network.orphaned", key))
				if err != nil || reported {
					t.Fatalf("second discloseOnce = (%t, %v), want (false, nil) — sustained shape must not re-report", reported, err)
				}
				if n := countEventsByName(t, h, "network.orphaned"); n != 1 {
					t.Fatalf("network.orphaned events = %d, want exactly 1", n)
				}
				if n := countAuditsByAction(t, h, "network.orphaned"); n != 1 {
					t.Fatalf("audit rows = %d, want exactly 1", n)
				}
			},
		},
		{
			// sweep 恢复清零：present 外的 key 清零可再报，present 内的
			// key 保持节流（不在场即忘的循环单点契约）。
			name: "sweep clears absent keys so they report again",
			run: func(t *testing.T, h *harness) {
				ctx := context.Background()
				var set disclosureSet
				if _, err := h.eng.discloseOnce(ctx, &set, "fleetly-net-gone", disclosureProbe("network.orphaned", "fleetly-net-gone")); err != nil {
					t.Fatalf("discloseOnce gone: %v", err)
				}
				if _, err := h.eng.discloseOnce(ctx, &set, "fleetly-net-here", disclosureProbe("network.missing", "fleetly-net-here")); err != nil {
					t.Fatalf("discloseOnce here: %v", err)
				}
				set.sweep(map[string]bool{"fleetly-net-here": true})
				reported, err := h.eng.discloseOnce(ctx, &set, "fleetly-net-gone", disclosureProbe("network.orphaned", "fleetly-net-gone"))
				if err != nil || !reported {
					t.Fatalf("re-disclose after sweep = (%t, %v), want (true, nil) — recovery must re-arm", reported, err)
				}
				reported, err = h.eng.discloseOnce(ctx, &set, "fleetly-net-here", disclosureProbe("network.missing", "fleetly-net-here"))
				if err != nil || reported {
					t.Fatalf("re-disclose kept key = (%t, %v), want (false, nil) — present key stays throttled", reported, err)
				}
				if n := countEventsByName(t, h, "network.orphaned"); n != 2 {
					t.Fatalf("network.orphaned events = %d, want 2 (swept key re-reported)", n)
				}
				if n := countEventsByName(t, h, "network.missing"); n != 1 {
					t.Fatalf("network.missing events = %d, want 1 (swept-surviving key stays throttled)", n)
				}
			},
		},
		{
			// 不同 key 相互独立：一 key 已报不节流另一 key；互不清零。
			name: "different keys are independent",
			run: func(t *testing.T, h *harness) {
				ctx := context.Background()
				var set disclosureSet
				if _, err := h.eng.discloseOnce(ctx, &set, "fleetly-net-a", disclosureProbe("network.orphaned", "fleetly-net-a")); err != nil {
					t.Fatalf("discloseOnce a: %v", err)
				}
				reported, err := h.eng.discloseOnce(ctx, &set, "fleetly-net-b", disclosureProbe("network.orphaned", "fleetly-net-b"))
				if err != nil || !reported {
					t.Fatalf("discloseOnce b = (%t, %v), want (true, nil) — key b must not inherit key a's memory", reported, err)
				}
				reported, err = h.eng.discloseOnce(ctx, &set, "fleetly-net-a", disclosureProbe("network.orphaned", "fleetly-net-a"))
				if err != nil || reported {
					t.Fatalf("re-disclose a = (%t, %v), want (false, nil) — key b's report must not clear key a", reported, err)
				}
				if n := countEventsByName(t, h, "network.orphaned"); n != 2 {
					t.Fatalf("network.orphaned events = %d, want 2 (one per key)", n)
				}
			},
		},
		{
			// 事务失败不标记：披露整体回滚（事件、审计零残留），记忆为空
			// ——下一拍重试仍报（披露不丢）。
			name: "failed transaction is not marked and the next attempt reports",
			run: func(t *testing.T, h *harness) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // 事务注入面：已取消的 ctx 使 InTx 起始即失败
				var set disclosureSet
				key := "fleetly-orphan-txfail"
				reported, err := h.eng.discloseOnce(ctx, &set, key, disclosureProbe("network.orphaned", key))
				if err == nil {
					t.Fatal("discloseOnce on failed transaction = nil error, want failure")
				}
				if reported {
					t.Fatal("discloseOnce on failed transaction reports reported=true, want false")
				}
				if set.reported(key) {
					t.Fatal("failed transaction marked the memory — disclosure would be lost")
				}
				if n := countEventsByName(t, h, "network.orphaned"); n != 0 {
					t.Fatalf("network.orphaned events = %d after rollback, want 0", n)
				}
				if n := countAuditsByAction(t, h, "network.orphaned"); n != 0 {
					t.Fatalf("audit rows = %d after rollback, want 0 (fail-closed pairing)", n)
				}
				reported, err = h.eng.discloseOnce(context.Background(), &set, key, disclosureProbe("network.orphaned", key))
				if err != nil || !reported {
					t.Fatalf("retry discloseOnce = (%t, %v), want (true, nil) — unmarked failure must retry", reported, err)
				}
				if n := countEventsByName(t, h, "network.orphaned"); n != 1 {
					t.Fatalf("network.orphaned events = %d after retry, want 1", n)
				}
			},
		},
		{
			// 事件 + 审计各恰一条且 Action = 事件码；actor/result/target/
			// subject 形态随事件钉死（披露族的固定形态）。
			name: "event and audit are written exactly once each with action equal to the event code",
			run: func(t *testing.T, h *harness) {
				ctx := context.Background()
				var set disclosureSet
				key := "fleetly-orphan-pair"
				if _, err := h.eng.discloseOnce(ctx, &set, key, disclosureProbe("network.orphaned", key)); err != nil {
					t.Fatalf("discloseOnce: %v", err)
				}
				evs, err := h.store.EventsSince(ctx, 0, 100)
				if err != nil {
					t.Fatalf("events: %v", err)
				}
				if len(evs) != 1 {
					t.Fatalf("events = %d, want exactly 1 (%+v)", len(evs), evs)
				}
				if evs[0].Name != "network.orphaned" {
					t.Fatalf("event name = %s, want network.orphaned", evs[0].Name)
				}
				if evs[0].Subject != "network:"+key {
					t.Fatalf("event subject = %s, want network:%s", evs[0].Subject, key)
				}
				rows, err := h.store.RecentAudits(ctx, 100)
				if err != nil {
					t.Fatalf("audits: %v", err)
				}
				if len(rows) != 1 {
					t.Fatalf("audit rows = %d, want exactly 1 (%+v)", len(rows), rows)
				}
				if rows[0].Action != evs[0].Name {
					t.Fatalf("audit action = %s, want the event code %s", rows[0].Action, evs[0].Name)
				}
				if rows[0].Actor != "system" || rows[0].Result != "ok" {
					t.Fatalf("audit actor/result = %s/%s, want system/ok (disclosure family shape)", rows[0].Actor, rows[0].Result)
				}
				if rows[0].Target != "network:"+key {
					t.Fatalf("audit target = %s, want network:%s", rows[0].Target, key)
				}
			},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			h := newHarness(t)
			sc.run(t, h)
		})
	}
}

// TestScanGateDueAndAdvance 钉住节拍门语义：零值即刻到期（重启即清零 =
// 重启后立即扫一拍）；闸内拍跳过且不前移闸；过闸恢复扫描；force 直通且
// 同样推进闸（与收敛前各 duty 的形态一致）；推进锚 = 判定时刻 + interval。
func TestScanGateDueAndAdvance(t *testing.T) {
	var gate scanGate
	interval := 30 * time.Second
	base := time.Now()
	if !gate.due(base, false, interval) {
		t.Fatal("zero-value gate not due — restart must scan immediately")
	}
	if gate.due(base, false, interval) {
		t.Fatal("gated beat scanned (frequency control broken)")
	}
	if gate.due(base.Add(time.Second), false, interval) {
		t.Fatal("gated beat scanned within the interval")
	}
	if !gate.due(base.Add(interval+time.Second), false, interval) {
		t.Fatal("gate never reopened after the interval passed")
	}
	if gate.due(base.Add(interval+time.Second), false, interval) {
		t.Fatal("second beat after reopening scanned (gate did not advance)")
	}
	if !gate.due(base.Add(interval+time.Second), true, interval) {
		t.Fatal("force beat was gated (explicit entry must pass through)")
	}
	if gate.due(base.Add(interval+time.Second), false, interval) {
		t.Fatal("beat right after a force beat scanned (force must advance the gate too)")
	}
	if gate.due(base.Add(2*interval), false, interval) {
		t.Fatal("beat scanned one second before the gate anchored by the force beat")
	}
	if !gate.due(base.Add(2*interval+time.Second), false, interval) {
		t.Fatal("gate anchored by the force beat never reopened")
	}
}
