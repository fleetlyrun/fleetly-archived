package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 通知 Webhook 状态面测试（E6 W5-S4，observability §5.1/§5.2）：CRUD +
// secret 纪律（密文/指纹入库、明文零出现）+ 订阅模式白名单 + 游标语义 +
// 投递台账状态机 + janitor prune。事件注册表零新增（设计红线）——本文件
// 的写路径只落审计行。

func createTestEndpoint(t *testing.T, st *Store, name string) (WebhookEndpoint, string) {
	t.Helper()
	e, err := st.CreateWebhookEndpoint(context.Background(), WebhookEndpointWrite{
		Name:              name,
		URL:               "https://ops.example.test/hook",
		SecretCipher:      "cipher:" + name,
		SecretFingerprint: "0123456789abcdef",
		EventPatterns:     []string{"deployment.*"},
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create endpoint %s: %v", name, err)
	}
	return e, "plain-" + name
}

func TestWebhookEndpointCRUDLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	e, _ := createTestEndpoint(t, st, "ops")
	if e.ID == "" || e.Name != "ops" || !e.Enabled {
		t.Fatalf("created endpoint unexpected: %+v", e)
	}
	if e.SecretCipher != "cipher:ops" || e.SecretFingerprint != "0123456789abcdef" {
		t.Fatalf("secret fields must round-trip verbatim: %+v", e)
	}

	// List / Get。
	list, err := st.ListWebhookEndpoints(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v (n=%d)", err, len(list))
	}
	got, err := st.GetWebhookEndpoint(ctx, e.ID)
	if err != nil || got.Name != "ops" {
		t.Fatalf("get: %v", err)
	}
	if _, err := st.GetWebhookEndpoint(ctx, "01NOPE"); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("get missing: err = %v, want ErrWebhookNotFound", err)
	}

	// 名字唯一。
	_, err = st.CreateWebhookEndpoint(ctx, WebhookEndpointWrite{
		Name: "ops", URL: "https://x.example.test", SecretCipher: "c",
		SecretFingerprint: "0123456789abcdef", EventPatterns: []string{"*"}, Enabled: true,
	})
	if !errors.Is(err, ErrWebhookNameConflict) {
		t.Fatalf("duplicate name: err = %v, want ErrWebhookNameConflict", err)
	}

	// Update：开关翻转 + 改名 + 改 URL + patterns 替换。
	disabled := false
	updated, err := st.UpdateWebhookEndpoint(ctx, e.ID, WebhookEndpointUpdate{
		Enabled: &disabled,
	})
	if err != nil || updated.Enabled {
		t.Fatalf("disable: %v (%+v)", err, updated)
	}
	newName := "ops-2"
	updated, err = st.UpdateWebhookEndpoint(ctx, e.ID, WebhookEndpointUpdate{
		Name:          &newName,
		EventPatterns: []string{"cron.failed", "*"},
	})
	if err != nil {
		t.Fatalf("rename/patterns: %v", err)
	}
	if updated.Name != "ops-2" || len(updated.EventPatterns) != 2 {
		t.Fatalf("update result unexpected: %+v", updated)
	}

	// 全空更新显式拒绝。
	if _, err := st.UpdateWebhookEndpoint(ctx, e.ID, WebhookEndpointUpdate{}); err == nil {
		t.Fatal("empty update must be rejected")
	}

	// Delete：台账行同事务清理（先落一行台账再删端点）。
	if _, err := st.CreateWebhookDeliveriesAndAdvance(ctx, 7, []string{e.ID}); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	if err := st.DeleteWebhookEndpoint(ctx, e.ID, "human", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetWebhookEndpoint(ctx, e.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("deleted endpoint: err = %v, want ErrWebhookNotFound", err)
	}
	rows, err := st.ListWebhookDeliveries(ctx, e.ID, "", 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("deliveries must be cleaned with the endpoint: %v (n=%d)", err, len(rows))
	}
	// 审计面（created/updated/deleted）。
	audits, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	actions := map[string]bool{}
	for _, a := range audits {
		actions[a.Action] = true
	}
	for _, want := range []string{"webhook.created", "webhook.updated", "webhook.deleted"} {
		if !actions[want] {
			t.Fatalf("audit %s missing (got %v)", want, actions)
		}
	}
}

func TestWebhookValidationGuards(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 名字形状。
	if err := ValidateWebhookName(""); err == nil {
		t.Fatal("empty name must be rejected")
	}
	if err := ValidateWebhookName("has space"); err == nil {
		t.Fatal("name with space must be rejected")
	}
	if err := ValidateWebhookName("Ops_1.ok"); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}

	// URL：scheme 限定 http/https、host 必填、userinfo 拒绝。
	if err := ValidateWebhookURL("ftp://x"); err == nil {
		t.Fatal("ftp scheme must be rejected")
	}
	if err := ValidateWebhookURL("http://"); err == nil {
		t.Fatal("hostless URL must be rejected")
	}
	if err := ValidateWebhookURL("https://user:pw@example.test/hook"); err == nil {
		t.Fatal("userinfo in URL must be rejected")
	}
	if err := ValidateWebhookURL("http://127.0.0.1:8899/hook"); err != nil {
		t.Fatalf("plain http (intranet receiver) must be allowed: %v", err)
	}

	// 模式白名单：非空、字符集、`*` 通配；去重保序。
	if _, err := ValidateWebhookPatterns(nil); err == nil {
		t.Fatal("empty pattern list must be rejected")
	}
	if _, err := ValidateWebhookPatterns([]string{"Bad_*"}); err == nil {
		t.Fatal("uppercase pattern must be rejected")
	}
	if _, err := ValidateWebhookPatterns([]string{"a; DROP TABLE x"}); err == nil {
		t.Fatal("pattern with metacharacters must be rejected")
	}
	// 白名单允许无点单段模式（对某事件名的精确匹配形态——匹配器语义，
	// 白名单只管字符集）。
	if _, err := ValidateWebhookPatterns([]string{"cron"}); err != nil {
		t.Fatalf("single-token exact pattern must be allowed: %v", err)
	}
	got, err := ValidateWebhookPatterns([]string{"b.*", "a.*", "b.*"})
	if err != nil {
		t.Fatalf("valid patterns rejected: %v", err)
	}
	if len(got) != 2 || got[0] != "b.*" || got[1] != "a.*" {
		t.Fatalf("dedupe/order: %v", got)
	}

	// 端点级防御：secret 密文空/指纹形状非法拒绝（防旁路写入）。
	if _, err := st.CreateWebhookEndpoint(ctx, WebhookEndpointWrite{
		Name: "n1", URL: "https://x", SecretCipher: "",
		SecretFingerprint: "0123456789abcdef", EventPatterns: []string{"*"},
	}); err == nil {
		t.Fatal("empty cipher must be rejected")
	}
	if _, err := st.CreateWebhookEndpoint(ctx, WebhookEndpointWrite{
		Name: "n2", URL: "https://x", SecretCipher: "c",
		SecretFingerprint: "short", EventPatterns: []string{"*"},
	}); err == nil {
		t.Fatal("malformed fingerprint must be rejected")
	}
}

// TestWebhookSecretNeverInAudit：密文与指纹零出现在审计 diff（state-model
// §2.9 secret 纪律；URL 亦不进审计——最小事实集）。
func TestWebhookSecretNeverInAudit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	//nolint:gosec // G101：测试夹具密文/指纹标记，非真实凭据
	_, err := st.CreateWebhookEndpoint(ctx, WebhookEndpointWrite{
		Name:              "ops",
		URL:               "https://secret-host.example.test/hook",
		SecretCipher:      "SUPER-CIPHER-MATERIAL",
		SecretFingerprint: "0123456789abcdef",
		EventPatterns:     []string{"deployment.*"},
		Enabled:           true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT diff_summary FROM audit_log`)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var diff string
		if err := rows.Scan(&diff); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		for _, banned := range []string{"SUPER-CIPHER", "0123456789abcdef", "secret-host"} {
			if strings.Contains(diff, banned) {
				t.Fatalf("audit diff leaks %q: %s", banned, diff)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit: %v", err)
	}
}

// TestWebhookCursorSemantics：游标只前进；创建台账行与推进同事务；首启
// 初始化只发生一次且对齐当前最大 seq（订阅从现在开始，不回放历史）。
func TestWebhookCursorSemantics(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 预置事件两条。
	for _, name := range []string{"app.degraded", "app.recovered"} {
		if err := st.InTx(ctx, func(tx *Tx) error {
			_, err := tx.AppendEvent(ctx, Event{Name: name, Subject: "app:x"})
			return err
		}); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
	maxSeq, has, err := st.MaxEventSeq(ctx)
	if err != nil || !has || maxSeq != 2 {
		t.Fatalf("max seq: %v has=%v seq=%d", err, has, maxSeq)
	}

	// 首启初始化：对齐 maxSeq；二次调用 no-op。
	initialized, err := st.InitializeWebhookCursorIfEmpty(ctx, maxSeq)
	if err != nil || !initialized {
		t.Fatalf("first init: %v (%v)", err, initialized)
	}
	initialized, err = st.InitializeWebhookCursorIfEmpty(ctx, 99)
	if err != nil || initialized {
		t.Fatalf("second init must be a no-op: %v (%v)", err, initialized)
	}
	cursor, err := st.GetWebhookCursor(ctx)
	if err != nil || cursor != 2 {
		t.Fatalf("cursor after init: %v (%d)", err, cursor)
	}

	// 消费原子：matched 落 pending 行 + 游标推进到 event_seq。
	ep, _ := createTestEndpoint(t, st, "ops")
	rows, err := st.CreateWebhookDeliveriesAndAdvance(ctx, 4, []string{ep.ID})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != WebhookDeliveryPending || rows[0].Attempts != 0 {
		t.Fatalf("pending rows: %+v", rows)
	}
	cursor, err = st.GetWebhookCursor(ctx)
	if err != nil || cursor != 4 {
		t.Fatalf("cursor after consume: %v (%d)", err, cursor)
	}

	// 游标只前进（SetWebhookCursor 回退被拒）。
	if err := st.SetWebhookCursor(ctx, 3); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	cursor, _ = st.GetWebhookCursor(ctx)
	if cursor != 4 {
		t.Fatalf("cursor regressed to %d, want 4", cursor)
	}
}

// TestWebhookDeliveryStateMachine：pending → 尝试记账 → 退避 → 终态。
// 重试语义（§5.2）：非 2xx/传输错误 = 失败（记响应码/错误摘要）；未达
// 预算 = pending + next_retry_at；预算耗尽 = 终态 failed；成功 = ok。
func TestWebhookDeliveryStateMachine(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ep, _ := createTestEndpoint(t, st, "ops")
	rows, err := st.CreateWebhookDeliveriesAndAdvance(ctx, 1, []string{ep.ID})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	d := rows[0]

	// 失败 #1：有退避 → pending。
	retry := time.Now().UTC().Add(time.Minute)
	if err := st.RecordWebhookAttempt(ctx, d.ID, WebhookAttemptResult{
		OK: false, ResponseCode: 500, ErrText: "boom 500", NextRetryAt: retry,
	}); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	got, err := st.GetWebhookDelivery(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != WebhookDeliveryPending || got.Attempts != 1 {
		t.Fatalf("after attempt 1: %+v", got)
	}
	if got.ResponseCode == nil || *got.ResponseCode != 500 {
		t.Fatalf("response code: %+v", got.ResponseCode)
	}
	if got.NextRetryAt == nil || !got.NextRetryAt.Equal(retry) {
		t.Fatalf("next retry: %+v want %v", got.NextRetryAt, retry)
	}
	if got.LastError != "boom 500" {
		t.Fatalf("last error: %q", got.LastError)
	}

	// 到期扫描：未来重试行不被扫出；now 推进后被扫出。
	due, err := st.DueWebhookDeliveries(ctx, time.Now().UTC(), 10, nil)
	if err != nil || len(due) != 0 {
		t.Fatalf("future row must not be due: %v (n=%d)", err, len(due))
	}
	due, err = st.DueWebhookDeliveries(ctx, retry.Add(time.Second), 10, nil)
	if err != nil || len(due) != 1 {
		t.Fatalf("due after the retry point: %v (n=%d)", err, len(due))
	}

	// 失败 #2（无退避时刻）→ 终态 failed（响应码 0 = 传输失败语义）。
	if err := st.RecordWebhookAttempt(ctx, d.ID, WebhookAttemptResult{
		OK: false, ResponseCode: 0, ErrText: "dial refused",
	}); err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	got, _ = st.GetWebhookDelivery(ctx, d.ID)
	if got.Status != WebhookDeliveryFailed || got.Attempts != 2 {
		t.Fatalf("terminal: %+v", got)
	}
	if got.NextRetryAt != nil {
		t.Fatalf("terminal rows carry no retry time: %+v", got.NextRetryAt)
	}

	// 终态后的成功尝试仍记账（幂等事实面——重启恢复扫描不误伤）。
	if err := st.RecordWebhookAttempt(ctx, d.ID, WebhookAttemptResult{
		OK: true, ResponseCode: 200,
	}); err != nil {
		t.Fatalf("late ok: %v", err)
	}
	got, _ = st.GetWebhookDelivery(ctx, d.ID)
	if got.Status != WebhookDeliveryOK || got.Attempts != 3 || got.LastError != "" {
		t.Fatalf("after late ok: %+v", got)
	}

	// 不存在的行 → ErrWebhookNotFound（端点删除竞态的投递器放弃路径）。
	if err := st.RecordWebhookAttempt(ctx, "01NOPE", WebhookAttemptResult{OK: true}); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("missing row: err = %v, want ErrWebhookNotFound", err)
	}
}

// TestWebhookLatestTerminalDeliveries：组件判红数据源——每端点最近一条
// 终态行；终态后的 pending 行不掩盖最近终态。
func TestWebhookLatestTerminalDeliveries(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ep, _ := createTestEndpoint(t, st, "ops")
	rows, _ := st.CreateWebhookDeliveriesAndAdvance(ctx, 1, []string{ep.ID})
	// 失败 → 终态。
	if err := st.RecordWebhookAttempt(ctx, rows[0].ID, WebhookAttemptResult{OK: false, ErrText: "down"}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	latest, err := st.LatestWebhookTerminalDeliveries(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	d, ok := latest[ep.ID]
	if !ok || d.Status != WebhookDeliveryFailed {
		t.Fatalf("failed endpoint must surface: %+v", d)
	}
	// 新事件 → 新 pending 行（成功前最近终态仍是 failed——红面持续）。
	rows2, _ := st.CreateWebhookDeliveriesAndAdvance(ctx, 2, []string{ep.ID})
	latest, _ = st.LatestWebhookTerminalDeliveries(ctx)
	if latest[ep.ID].Status != WebhookDeliveryFailed || latest[ep.ID].ID == rows2[0].ID {
		t.Fatalf("pending row must not mask the failed terminal: %+v", latest[ep.ID])
	}
	// 成功 → 绿。
	if err := st.RecordWebhookAttempt(ctx, rows2[0].ID, WebhookAttemptResult{OK: true, ResponseCode: 200}); err != nil {
		t.Fatalf("ok: %v", err)
	}
	latest, _ = st.LatestWebhookTerminalDeliveries(ctx)
	if latest[ep.ID].Status != WebhookDeliveryOK {
		t.Fatalf("after success the endpoint is green: %+v", latest[ep.ID])
	}
}

// TestWebhookPruneExpiredDeliveries：janitor 7d 窗（分批删除复用）。
func TestWebhookPruneExpiredDeliveries(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ep, _ := createTestEndpoint(t, st, "ops")
	if _, err := st.CreateWebhookDeliveriesAndAdvance(ctx, 1, []string{ep.ID}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 1 天前的 cutoff → 新鲜行存活；8 天后的 cutoff → 过窗行清理。
	n, err := st.PruneExpiredWebhookDeliveries(ctx, time.Now().UTC().AddDate(0, 0, -1))
	if err != nil || n != 0 {
		t.Fatalf("fresh rows must survive: %v (n=%d)", err, n)
	}
	n, err = st.PruneExpiredWebhookDeliveries(ctx, time.Now().UTC().AddDate(0, 0, 8))
	if err != nil || n != 1 {
		t.Fatalf("stale rows must be pruned: %v (n=%d)", err, n)
	}
	rows, _ := st.ListWebhookDeliveries(ctx, ep.ID, "", 100)
	if len(rows) != 0 {
		t.Fatalf("rows remain after prune: %d", len(rows))
	}
}
