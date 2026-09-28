package logs

// IMPL-ARCH-H 收编共路回归：app 家族（appSink：redact → ring → 入湖/落盘）
// 与任务家族（taskSink：task 标签直推入湖）在同一轮 scanOnce 内各走一遍
// 同一 pollStream 循环骨架——游标 get-or-init / 推进、看门狗、deliver
// select、last 时间戳语义为一份实现，家族差异只落在 sink（本测试逐点钉住
// 两族的 sink 差异面与共路的游标语义）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

func TestPollStreamSharedSkeletonServesBothFamilies(t *testing.T) {
	mg, port, st, box := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fb := &fakeBackend{}
	mg.WithIngestBackend(fb)
	if err := st.SaveLogsSettings(ctx, "victorialogs", state.LogsSaveOptions{Actor: "system"}); err != nil {
		t.Fatalf("save backend: %v", err)
	}
	mg.refreshBackendGate(ctx)
	if !mg.vlEnabled() {
		t.Fatal("vl gate should be on")
	}

	app, _ := testsupport.SeedAppE(t, st, "sharedloop")
	// 种一枚平台 env：app 家族行必须脱敏、任务家族行原样通过（任务无 app
	// env 脱敏面）——同一条 secret 在两个 sink 的分叉即共路证据的一半。
	const secret = "shared-loop-secret-4242"
	ciphertext, err := box.Encrypt([]byte(secret))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := st.SetAppEnv(ctx, app.ID, "API_KEY", string(ciphertext), "platform", "human"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}

	appService := "fleetly-" + app.TeamSlug + "-" + app.ProjectSlug + "-sharedloop-web"
	port.setApp(app.QualifiedName(), "web")
	base := time.Now().UTC().Add(-time.Hour)
	mg.WithClock(func() time.Time { return base })
	port.emit(appService, substrate.LogLine{At: base.Add(time.Millisecond), Line: "app boot token=" + secret})

	taskID := ulid.Make().String()
	taskService, err := naming.TaskServiceName(taskID)
	if err != nil {
		t.Fatalf("task service name: %v", err)
	}
	port.taskStates = []engine.ServiceState{{
		Name:   taskService,
		Labels: map[string]string{state.LabelTaskID: taskID},
	}}
	port.emit(taskService, substrate.LogLine{At: base.Add(time.Millisecond), Line: "task boot token=" + secret})

	mg.scanOnce(ctx) // 两族同轮：app 流与任务流各经一次同一循环骨架。
	mg.ing.flushTick(ctx)

	batches, _ := fb.snapshot()
	appRows, taskRows := ingestedRowsOf(batches, app.QualifiedName(), taskID)
	if len(appRows) != 1 || len(taskRows) != 1 {
		t.Fatalf("ingested rows: app=%d task=%d, want 1 each", len(appRows), len(taskRows))
	}
	// app 家族 sink：redact + (app, service) 标签 + source=container。
	if appRows[0].Service != "web" || appRows[0].Source != SourceContainer {
		t.Fatalf("app row attribution = %+v", appRows[0])
	}
	if strings.Contains(appRows[0].Line, secret) || !strings.Contains(appRows[0].Line, "***") {
		t.Fatalf("app row line not redacted by appSink: %q", appRows[0].Line)
	}
	// 任务家族 sink：task 标签、无脱敏面（行原样）、app/service 为空。
	if taskRows[0].App != "" || taskRows[0].Service != "" || taskRows[0].Source != SourceContainer {
		t.Fatalf("task row attribution = %+v", taskRows[0])
	}
	if want := "task boot token=" + secret; taskRows[0].Line != want {
		t.Fatalf("task row line altered (taskSink must not redact): got %q, want %q", taskRows[0].Line, want)
	}

	// 直播面差异同轮可查：app 行进 ring；任务行不进 ring（直播面无任务键
	// ——taskSink 若误接 hub，其 Entry 会落 streamKey("", "") 键）。
	ch, cancelSub := mg.hub.subscribe(app.QualifiedName(), "web")
	cancelSub()
	var ringRows []Entry
	for e := range ch {
		ringRows = append(ringRows, e)
	}
	if len(ringRows) != 1 || ringRows[0].Line != appRows[0].Line {
		t.Fatalf("ring rows = %+v, want the single redacted app row", ringRows)
	}
	if r := mg.hub.streams[streamKey("", "")]; r != nil {
		t.Fatal("task row leaked into the live ring (taskSink must not touch the hub)")
	}

	// 共路的游标语义：两族游标各自 get-or-init 并推进到末行可信时间戳。
	mg.mu.Lock()
	appCur, appOK := mg.streams[streamKey(app.QualifiedName(), "web")]
	taskCur, taskOK := mg.streams[taskCursorKey(taskID)]
	mg.mu.Unlock()
	if !appOK || !taskOK {
		t.Fatal("family cursors missing after the shared poll round")
	}
	if want := base.Add(time.Millisecond); !appCur.lastAt.Equal(want) || !taskCur.lastAt.Equal(want) {
		t.Fatalf("cursor advance: app=%v task=%v, want %v on both", appCur.lastAt, taskCur.lastAt, want)
	}

	// 续拍只增量（游标推进 = 下一轮 since 越过已投递行）——同一骨架的
	// 增量语义对两族一致。
	port.emit(appService, substrate.LogLine{At: base.Add(2 * time.Millisecond), Line: "app two"})
	port.emit(taskService, substrate.LogLine{At: base.Add(2 * time.Millisecond), Line: "task two"})
	mg.scanOnce(ctx)
	mg.ing.flushTick(ctx)

	batches, _ = fb.snapshot()
	appRows, taskRows = ingestedRowsOf(batches, app.QualifiedName(), taskID)
	if len(appRows) != 2 || len(taskRows) != 2 {
		t.Fatalf("rows after second round: app=%d task=%d, want 2 each (incremental only)", len(appRows), len(taskRows))
	}
	if appRows[1].Line != "app two" || taskRows[1].Line != "task two" {
		t.Fatalf("second round rows = %q / %q, want the newly emitted lines only", appRows[1].Line, taskRows[1].Line)
	}
}

// ingestedRowsOf 从入湖批次快照中筛出指定 app 行与任务行（保序）。
func ingestedRowsOf(batches [][]Entry, qualifiedApp, taskID string) (appRows, taskRows []Entry) {
	for _, batch := range batches {
		for _, e := range batch {
			switch {
			case e.App == qualifiedApp:
				appRows = append(appRows, e)
			case e.Task == taskID:
				taskRows = append(taskRows, e)
			}
		}
	}
	return appRows, taskRows
}
