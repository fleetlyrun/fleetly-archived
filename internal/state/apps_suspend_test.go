package state

// 挂起位写点测试（app Stop/Start，00028 加法列；2026-09-29 三轮）：CAS +
// tombstone 守卫 + 事件同事务 + 扫描回读。排水/派生/豁免的引擎侧语义在
// internal/engine/suspend_test.go。

import (
	"context"
	"errors"
	"testing"
)

func TestSetAppSuspended(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if app.Suspended {
		t.Fatal("new app must not be suspended")
	}

	// 置位：CAS 命中 + 事件同事务（app.suspended）+ 回读行带位。
	updated, err := st.SetAppSuspended(ctx, app.ID, true, Event{
		Name: "app.suspended", Subject: "app:" + app.ID, Payload: DiffSummary("app", app.Name),
	})
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !updated.Suspended {
		t.Fatal("returned row must carry suspended=true")
	}
	got, err := st.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if !got.Suspended {
		t.Fatal("re-read row must carry suspended=true")
	}

	// 重复置位：CAS 落败 → ErrAppSuspendedConflict（幂等面由读面投影消化，
	// 写面显式拒绝——不静默成功）。
	if _, err := st.SetAppSuspended(ctx, app.ID, true); !errors.Is(err, ErrAppSuspendedConflict) {
		t.Fatalf("re-suspend must return ErrAppSuspendedConflict, got: %v", err)
	}

	// 清位：CAS 命中（期望现值 true → false）。
	cleared, err := st.SetAppSuspended(ctx, app.ID, false, Event{
		Name: "app.resumed", Subject: "app:" + app.ID,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if cleared.Suspended {
		t.Fatal("returned row must carry suspended=false after resume")
	}

	// 清位后事件确实入流（Outbox 与位写同事务——位在事件必在）。
	rows, err := st.EventsSince(ctx, 0, 20)
	if err != nil {
		t.Fatalf("events since: %v", err)
	}
	var sawSuspend, sawResume bool
	for _, r := range rows {
		switch r.Name {
		case "app.suspended":
			sawSuspend = true
		case "app.resumed":
			sawResume = true
		}
	}
	if !sawSuspend || !sawResume {
		t.Fatalf("events missing: suspend=%t resume=%t (must be same-transaction with the bit)", sawSuspend, sawResume)
	}
}

func TestSetAppSuspendedGuards(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// 不存在的行：ErrAppNotFound。
	if _, err := st.SetAppSuspended(ctx, "01NOPE00000000000000000000", true); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("missing app must return ErrAppNotFound, got: %v", err)
	}

	// 墓碑：deleting 上不接受业务写（ErrAppTombstoned）。
	if err := st.MarkAppDeleting(ctx, app.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	if _, err := st.SetAppSuspended(ctx, app.ID, true); !errors.Is(err, ErrAppTombstoned) {
		t.Fatalf("tombstoned app must return ErrAppTombstoned, got: %v", err)
	}
}
