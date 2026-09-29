package state

// ListAudits 查询原语单测（v0.3 W3-S1，rbac-teams §6 D-W0-6 读面）：过滤
// 语义表（actor/target 子串包含、action 前缀、result 精确、since/until 闭
// 区间）+ 分页（limit/offset/total）+ 投影全字段（含 W3-S1 补披露的
// request_id）。RecentAudits 既有消费面不动（零值 RequestID 行为不变）。

import (
	"context"
	"testing"
	"time"
)

// seedAudits 播种一组可区分的审计行（At 显式给定——过滤断言的确定性锚）。
func seedAudits(t *testing.T, st *Store, rows []AuditEntry) {
	t.Helper()
	ctx := context.Background()
	for _, r := range rows {
		if err := st.InTx(ctx, func(tx *Tx) error {
			return tx.WriteAudit(ctx, r)
		}); err != nil {
			t.Fatalf("write audit %s: %v", r.Action, err)
		}
	}
}

func auditFixtureRows(base time.Time) []AuditEntry {
	return []AuditEntry{
		{
			ID: "01AUDITLOGINFAIL0000000000", At: base.Add(-3 * time.Hour),
			Actor: "human", Action: "auth.login_failed", Target: "login:ada@example.com",
			Result: "error", ErrorCode: "E_AUTH_INVALID_CREDENTIALS",
		},
		{
			ID: "01AUDITAPIDELETE00000000000", At: base.Add(-2 * time.Hour),
			Actor: "user:01USERADA00000000000000", ActorTokenID: "01TOK000000000000000000000",
			Action: "api.AppsService.DeleteApp", Target: "app:01APP00000000000000000000",
			Result: "ok", RequestID: "req-42",
		},
		{
			ID: "01AUDITSYSTEMPRUNE000000000", At: base.Add(-1 * time.Hour),
			Actor: "system", Action: "audit.retention_changed", Target: "platform:audit",
			Result: "ok", DiffSummary: DiffSummary("retention_days", 90),
		},
	}
}

// TestListAuditsUnorderedAll 无过滤全查：行序 = at 倒序；total = 全量；
// 分页缺省页大小生效。
func TestListAuditsUnorderedAll(t *testing.T) {
	st := newTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	seedAudits(t, st, auditFixtureRows(base))

	rows, total, err := st.ListAudits(context.Background(), AuditQuery{})
	if err != nil {
		t.Fatalf("ListAudits: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// at 倒序：最新（system 行）在前。
	if rows[0].Action != "audit.retention_changed" || rows[2].Action != "auth.login_failed" {
		t.Fatalf("order = %s..%s, want newest-first", rows[0].Action, rows[2].Action)
	}
	// 投影全字段：RequestID/ErrorCode/DiffSummary 原样往返。
	if rows[1].RequestID != "req-42" || rows[1].Actor != "user:01USERADA00000000000000" {
		t.Fatalf("request_id projection broken: %+v", rows[1])
	}
	if rows[0].DiffSummary != `{"retention_days":90}` {
		t.Fatalf("diff_summary projection broken: %+v", rows[0])
	}
	if rows[2].ErrorCode != "E_AUTH_INVALID_CREDENTIALS" {
		t.Fatalf("error_code projection broken: %+v", rows[2])
	}
}

// TestListAuditsFilterMatrix 过滤语义表（票面裁决逐维钉死）。
func TestListAuditsFilterMatrix(t *testing.T) {
	st := newTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	seedAudits(t, st, auditFixtureRows(base))
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		query AuditQuery
		wantN int
	}{
		{"actor substring hits user id", AuditQuery{Actor: "01USERADA"}, 1},
		{"actor substring exact form", AuditQuery{Actor: "user:01USERADA00000000000000"}, 1},
		{"actor substring system", AuditQuery{Actor: "system"}, 1},
		{"action prefix family", AuditQuery{Action: "auth."}, 1},
		{"action prefix api family", AuditQuery{Action: "api."}, 1},
		{"action prefix full", AuditQuery{Action: "audit.retention"}, 1},
		{"result exact error", AuditQuery{Result: "error"}, 1},
		{"result exact ok", AuditQuery{Result: "ok"}, 2},
		{"target substring app", AuditQuery{Target: "app:"}, 1},
		{"target substring platform", AuditQuery{Target: "platform:audit"}, 1},
		{"since inclusive", AuditQuery{Since: base.Add(-2 * time.Hour)}, 2},
		{"until inclusive", AuditQuery{Until: base.Add(-2 * time.Hour)}, 2},
		{"window both ends", AuditQuery{Since: base.Add(-2 * time.Hour), Until: base.Add(-1 * time.Hour)}, 2},
		{"combined action+result", AuditQuery{Action: "auth.", Result: "error"}, 1},
		{"no match empty page", AuditQuery{Action: "no.such"}, 0},
	} {
		rows, total, err := st.ListAudits(ctx, tc.query)
		if err != nil {
			t.Fatalf("%s: ListAudits: %v", tc.name, err)
		}
		if len(rows) != tc.wantN || total != tc.wantN {
			t.Fatalf("%s: rows=%d total=%d, want %d/%d", tc.name, len(rows), total, tc.wantN, tc.wantN)
		}
	}

	// action 前缀中的 LIKE 元字符按字面匹配（外部输入不当通配符）。
	rows, total, err := st.ListAudits(ctx, AuditQuery{Action: "auth._fail"})
	if err != nil || len(rows) != 0 || total != 0 {
		t.Fatalf("LIKE metachars must match literally: rows=%d total=%d err=%v", len(rows), total, err)
	}
}

// TestListAuditsPagination 分页：limit 截断、offset 跳行、total 恒为全量
// 命中数（过滤生效、分页生效前）。
func TestListAuditsPagination(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	seedAudits(t, st, auditFixtureRows(base))

	rows, total, err := st.ListAudits(ctx, AuditQuery{Limit: 2})
	if err != nil || total != 3 || len(rows) != 2 {
		t.Fatalf("limit 2: rows=%d total=%d err=%v, want 2/3/nil", len(rows), total, err)
	}
	rows2, total2, err := st.ListAudits(ctx, AuditQuery{Limit: 2, Offset: 2})
	if err != nil || total2 != 3 || len(rows2) != 1 {
		t.Fatalf("offset 2: rows=%d total=%d err=%v, want 1/3/nil", len(rows2), total2, err)
	}
	// 页接续：第二页首行 = 全量序的第三行（同序锚——与无分页全查对齐）。
	full, _, err := st.ListAudits(ctx, AuditQuery{})
	if err != nil {
		t.Fatalf("full list: %v", err)
	}
	if rows2[0].ID != full[2].ID {
		t.Fatalf("page continuation broken: page2 head %s, want %s", rows2[0].ID, full[2].ID)
	}
	// 越界 offset：空页、total 不变。
	rows3, total3, err := st.ListAudits(ctx, AuditQuery{Offset: 10})
	if err != nil || total3 != 3 || len(rows3) != 0 {
		t.Fatalf("offset beyond end: rows=%d total=%d err=%v, want 0/3/nil", len(rows3), total3, err)
	}
}

// TestRecentAuditsRequestIDProjection RecentAudits 保留面加法扩展的回归：
// 既有字段行为不变，RequestID 就位（不破既有消费方）。
func TestRecentAuditsRequestIDProjection(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedAudits(t, st, auditFixtureRows(time.Now().UTC()))
	rows, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	found := false
	for _, r := range rows {
		if r.ID == "01AUDITAPIDELETE00000000000" {
			found = true
			if r.RequestID != "req-42" {
				t.Fatalf("RecentAudits request_id = %q, want req-42", r.RequestID)
			}
		}
	}
	if !found {
		t.Fatal("seeded api row missing from RecentAudits")
	}
}
