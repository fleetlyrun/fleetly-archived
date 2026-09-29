package state

// logs.* 设置面单测（E6 观测专项设计 §2.2，W5-S1）：缺省语义（未显式
// 设置 = victorialogs 生效、Set=false——V2-1 默认捆绑）、保存校验、
// 审计+事件同事务、存储值畸形 loud-fail。

import (
	"context"
	"strings"
	"testing"
)

// TestLoadLogsSettingsDefault 空库/未设置 → 缺省态（Backend=victorialogs，
// Set=false）——未显式设置过的存量安装升级后收敛管理器即部署 VL 的语义锚。
func TestLoadLogsSettingsDefault(t *testing.T) {
	st := newSettingsStore(t)
	in, err := st.LoadLogsSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadLogsSettings: %v", err)
	}
	if in.Backend != LogsBackendVictorialogs || in.Set {
		t.Fatalf("default = %+v, want {victorialogs, Set=false}", in)
	}
}

// TestSaveLogsSettingsRoundTrip 保存回读：显式 jsonl → Backend=jsonl 且
// Set=true（「未设置」与「显式 jsonl」的区分——CLI/Console 诚实展示面）；
// 显式 victorialogs 同样 Set=true；UpdatedAt 随保存推进。
func TestSaveLogsSettingsRoundTrip(t *testing.T) {
	st := newSettingsStore(t)
	ctx := context.Background()

	if err := st.SaveLogsSettings(ctx, LogsBackendJSONL, LogsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save jsonl: %v", err)
	}
	in, err := st.LoadLogsSettings(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if in.Backend != LogsBackendJSONL || !in.Set || in.UpdatedAt.IsZero() {
		t.Fatalf("settings = %+v, want {jsonl, Set=true, UpdatedAt set}", in)
	}

	if err := st.SaveLogsSettings(ctx, LogsBackendVictorialogs, LogsSaveOptions{Actor: "human"}); err != nil {
		t.Fatalf("save victorialogs: %v", err)
	}
	in, err = st.LoadLogsSettings(ctx)
	if err != nil {
		t.Fatalf("load 2: %v", err)
	}
	if in.Backend != LogsBackendVictorialogs || !in.Set {
		t.Fatalf("settings = %+v, want {victorialogs, Set=true}", in)
	}
}

// TestSaveLogsSettingsAuditAndEvent 保存同事务落审计（action=
// logs.backend_updated，diff 只带 backend）与事件（logs.backend_updated，
// Outbox）——s3settings 同型。
func TestSaveLogsSettingsAuditAndEvent(t *testing.T) {
	st := newSettingsStore(t)
	ctx := context.Background()
	if err := st.SaveLogsSettings(ctx, LogsBackendJSONL, LogsSaveOptions{Actor: "human", ActorTokenID: "flt_test"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	audit, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	foundAudit := false
	for _, a := range audit {
		if a.Action == "logs.backend_updated" {
			foundAudit = true
			if !strings.Contains(a.DiffSummary, "jsonl") {
				t.Fatalf("audit diff = %q, want backend value", a.DiffSummary)
			}
			if a.Actor != "human" {
				t.Fatalf("audit actor = %q", a.Actor)
			}
		}
	}
	if !foundAudit {
		t.Fatal("audit row logs.backend_updated not written")
	}
	evs, err := st.EventsSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	foundEvent := false
	for _, ev := range evs {
		if ev.Name == "logs.backend_updated" {
			foundEvent = true
			if !strings.Contains(ev.Payload, "jsonl") {
				t.Fatalf("event payload = %q, want backend value", ev.Payload)
			}
		}
	}
	if !foundEvent {
		t.Fatal("event logs.backend_updated not appended")
	}
}

// TestSaveLogsSettingsValidation 值域校验：非法值显式拒绝（不落库、不落
// 审计/事件——fail-closed）。
func TestSaveLogsSettingsValidation(t *testing.T) {
	st := newSettingsStore(t)
	ctx := context.Background()
	if err := st.SaveLogsSettings(ctx, "loki", LogsSaveOptions{Actor: "human"}); err == nil {
		t.Fatal("save loki must fail")
	}
	if err := st.SaveLogsSettings(ctx, "", LogsSaveOptions{Actor: "human"}); err == nil {
		t.Fatal("save empty must fail")
	}
	evs, err := st.EventsSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	for _, ev := range evs {
		if ev.Name == "logs.backend_updated" {
			t.Fatal("rejected save must not emit the event (fail-closed)")
		}
	}
	// 库内仍是缺省态。
	in, err := st.LoadLogsSettings(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if in.Set {
		t.Fatalf("rejected save leaked a row: %+v", in)
	}
}

// TestLoadLogsSettingsMalformedStored 存储值畸形（越词表）→ loud-fail
//（设置损坏显式报错，不静默回落缺省——与 s3 布尔同口径）。
func TestLoadLogsSettingsMalformedStored(t *testing.T) {
	st := newSettingsStore(t)
	ctx := context.Background()
	if err := st.InTx(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)`,
			LogsKeyBackend, "graylog", 1)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.LoadLogsSettings(ctx); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("LoadLogsSettings err = %v, want invalid-value loud fail", err)
	}
}
