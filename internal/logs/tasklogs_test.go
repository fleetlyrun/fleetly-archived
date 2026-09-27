package logs

// 任务日志采集回归（T 线 DT-5 / IMPL-T2-1「日志入 VL，task 标签归因」）：
//   - 任务服务发现（TaskServiceStates）→ 流采集 → 入湖行以 task 归因、
//     app/service 为空；
//   - 纯 jsonl 形态不采集任务日志（无任务检索面——诚实边界）；
//   - 任务消失后游标回收（不悬挂）。

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

func TestTaskLogsIngestWithTaskAttribution(t *testing.T) {
	mg, port, st, _ := newTestManager(t)
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

	taskID := ulid.Make().String()
	swarmName, err := naming.TaskServiceName(taskID)
	if err != nil {
		t.Fatalf("task service name: %v", err)
	}
	port.taskStates = []engine.ServiceState{{
		Name:   swarmName,
		Labels: map[string]string{state.LabelTaskID: taskID},
	}}
	at := time.Now().UTC().Truncate(time.Second)
	port.emit(swarmName, substrate.LogLine{At: at, Line: "task says hello"})

	mg.scanOnce(ctx)
	mg.ing.flushTick(ctx) // 手工排水（批量器 ticker 之外的确定性测试驱动）

	batches, _ := fb.snapshot()
	var got []Entry
	for _, batch := range batches {
		for _, e := range batch {
			if e.Task == taskID {
				got = append(got, e)
			}
		}
	}
	if len(got) != 1 {
		t.Fatalf("task rows ingested = %d (batches=%v), want 1", len(got), batches)
	}
	if got[0].App != "" || got[0].Service != "" {
		t.Fatalf("task row app/service = %q/%q, want empty (task label is the attribution)", got[0].App, got[0].Service)
	}
	if got[0].Line != "task says hello" || got[0].Source != SourceContainer {
		t.Fatalf("task row = %+v", got[0])
	}

	// 任务消失：游标当轮回收（不悬挂）。
	port.taskStates = nil
	mg.scanOnce(ctx)
	mg.mu.Lock()
	_, alive := mg.streams[taskCursorKey(taskID)]
	mg.mu.Unlock()
	if alive {
		t.Fatal("cursor for a vanished task must be evicted on the next scan")
	}
}

func TestTaskLogsSkippedInJSONLMode(t *testing.T) {
	mg, port, _, _ := newTestManager(t)
	ctx := context.Background()
	// 未装配入湖后端（纯 jsonl 形态）：任务面整体跳过。
	taskID := ulid.Make().String()
	swarmName, err := naming.TaskServiceName(taskID)
	if err != nil {
		t.Fatalf("task service name: %v", err)
	}
	port.taskStates = []engine.ServiceState{{
		Name:   swarmName,
		Labels: map[string]string{state.LabelTaskID: taskID},
	}}
	port.emit(swarmName, substrate.LogLine{At: time.Now(), Line: "should not be collected"})
	mg.scanOnce(ctx)
	mg.mu.Lock()
	_, alive := mg.streams[taskCursorKey(taskID)]
	mg.mu.Unlock()
	if alive {
		t.Fatal("jsonl mode must not open task cursors (task logs live in the log backend only)")
	}
}
