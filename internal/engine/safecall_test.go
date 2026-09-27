package engine

// MG-5/M1-7：tick 路径 duty panic 包壳的覆盖面测试（B4）。两层断言：
//  1. TestTickDutiesAllGoThroughSafeCall（结构断言，源扫描）：tick 与 Run 的
//     drift 分支里每个 duty 调用点必须经 e.safeCall(...) 收口——**新增 duty
//     不走 helper 本测试即红**（MG-5 的「覆盖面结构断言」契约：往 tick 里
//     加裸调用 e.xxx(ctx) 而不包 safeCall，或加了 duty 不更新清单，测试都
//     会失败，逼着新 duty 进入包壳与清单的统一管理）；
//  2. TestTickSingleDutyPanicIsolated（行为断言，表驱动）：以 duty 清单驱动
//     注入 panic，断言单 duty panic 不打死 tick、其余 duty 照常执行；
//  3. TestWatchPostWindowPanicDoesNotKillTick / TestDriftScanPanicDoesNotKill
//     RunLoop：经真实代码路径（假底座毒点，非注入钩子）分别命中
//     watchPostWindow 与 driftScan 两个原裸奔点。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// tickDutyManifest 是 tick 的 duty 清单（与 engine.go 的 tick 实现一一对应；
// 源扫描测试钉死两者一致——新增 duty 不进清单/不走 safeCall 即红）。
var tickDutyManifest = []string{"recoveryRetry", "pickQueued", "advanceActive", "watchPostWindow", "reapDeletingApps", "substrateRecon", "sweepInitJobs", "reconcileProjectNetworks", "dutyAutoscaling"}

// readEngineSource 读取 engine.go 源文本（同包直读；源扫描的输入）。
// 行尾归一到 LF 后再扫描：Windows 检出（core.autocrlf=true）会把磁盘上的
// .go 文件写成 CRLF——函数体结尾判据 `\n}\n` 是行尾敏感的字节序列，不归一
// 则测试结果随检出环境漂移（本机实录）。
func readEngineSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// funcBody 提取顶层方法的函数体（签名起始到首个列 0 的 "}"）。
func funcBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("signature %q not found in engine.go", signature)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("body end of %q not found", signature)
	}
	return src[start : start+end]
}

// TestTickDutiesAllGoThroughSafeCall 结构断言：tick 的每个 duty 恰好一次、
// 且只以 safeCall 形态出现（裸调用会使该 duty 的出现次数 >1 即红）；Run 的
// drift 分支同理。
func TestTickDutiesAllGoThroughSafeCall(t *testing.T) {
	src := readEngineSource(t)
	tickBody := funcBody(t, src, "func (e *Engine) tick(")
	runBody := funcBody(t, src, "func (e *Engine) Run(")

	// duty 总数与清单一致（多出的 safeCall 调用点也要进清单受管）。
	if got := strings.Count(tickBody, "e.safeCall("); got != len(tickDutyManifest) {
		t.Fatalf("safeCall call sites in tick = %d, manifest = %d (both sides must be kept in sync)", got, len(tickDutyManifest))
	}
	for _, duty := range tickDutyManifest {
		if !strings.Contains(tickBody, fmt.Sprintf("e.safeCall(%q, func() {", duty)) {
			t.Errorf("duty %q in tick does not go through safeCall (MG-5 contract: new duties must use the helper and join tickDutyManifest)", duty)
		}
	}
	// Run 的 drift 分支：driftScan 必须经 safeCall（tick 与 drift ticker 的
	// 全部 duty 调用点收口，M1-7）。
	if !strings.Contains(runBody, `e.safeCall("driftScan", func() { e.driftScan(ctx) })`) {
		t.Error(`Run's driftTicker branch does not go through safeCall("driftScan", ...)`)
	}
	if strings.Count(runBody, "e.driftScan(ctx)") != 1 {
		t.Error("e.driftScan(ctx) appears != 1 times in Run (it should only exist inside the safeCall wrapper)")
	}
}

// TestTickSingleDutyPanicIsolated 表驱动：单个 duty panic（注入钩子命中
// safeCall 入口）不打死 tick——同拍其余 duty 照常执行；panic 若未包壳将
// 直接打穿测试进程（Tick 调用即崩）。
func TestTickSingleDutyPanicIsolated(t *testing.T) {
	for _, duty := range tickDutyManifest {
		t.Run(duty, func(t *testing.T) {
			h := newHarness(t)
			h.eng.dutyCalls = map[string]int{}
			h.eng.dutyPanicOn = map[string]bool{duty: true}
			h.eng.Tick(context.Background()) // 未包壳即 panic → 测试失败
			for _, other := range tickDutyManifest {
				if other == duty {
					continue
				}
				if h.eng.dutyCallCount(other) == 0 {
					t.Fatalf("after duty %q panicked, other duty %q did not run (tick loop was killed)", duty, other)
				}
			}
		})
	}
}

// TestWatchPostWindowPanicDoesNotKillTick MG-5 回归（真实毒点：假底座
// TaskList panic——watchPostWindow 原是 tick 三 duty 中唯一无 recover 的
// 路径）：窗后巡检 panic → 同拍先行的 pickQueued 照常执行（入队部署被
// 拾取），清毒后链路继续收敛。第二条部署用独立 app（毒点只挂在 demo 的
// 服务名上，避免 advanceOne 的 per-record 兜底把该部署本身判死——那是
// A9 语义，不是本测试的对象）。
func TestWatchPostWindowPanicDoesNotKillTick(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.enqueue(h.writeCompose(composeV1))
	if final := h.runToTerminal(rec); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	// 窗末已过 + 巡检会触达 TaskList（毒点）。
	h.clk.Advance(30 * time.Second)
	h.sub.panicOnTaskList(h.svc("web"))
	// 同拍再入队一条（独立 app）：pickQueued 在毒点 duty（最后位）之前执行。
	composeOther := strings.Replace(composeV1, "name: demo", "name: other", 1)
	rec2 := h.enqueueApp(t, h.writeCompose(composeOther), "other")
	h.eng.Tick(ctx)
	row := mustGet(h, rec2.ID)
	if row.Status == state.DeployQueued {
		t.Fatal("pickQueued skipped: second deployment still queued (tick was killed by the watchPostWindow panic)")
	}
	if row.Status == state.DeployFailed {
		t.Fatalf("rec2 misjudged as failed %s (the poison point should only hit demo's patrol surface)", row.ErrorCode)
	}
	// 清毒：后续拍照常收敛（部署成功 + 巡检告警补上）。
	h.sub.panicOn = map[string]bool{}
	final := h.runToTerminal(rec2)
	if final.Status != state.DeploySucceeded {
		t.Fatalf("rec2 = %s (%s), want succeeded (tick survived)", final.Status, final.ErrorCode)
	}
}

// TestDriftScanPanicDoesNotKillRunLoop MG-5 回归（真实毒点：假底座
// ServiceList panic——driftScan 在 Run 栈上原无 recover）：drift 分支 panic
// → Run 循环存活，后续 drift 拍与 tick 拍照常执行、ctx 取消正常退出。
func TestDriftScanPanicDoesNotKillRunLoop(t *testing.T) {
	h := newHarness(t)
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("v1 = %s", final.Status)
	}
	h.sub.panicServiceList = true // driftScan → computeAppDrift → ServiceList

	// 独立引擎实例跑真实 Run 循环（毫秒级周期驱动两分支）。
	cfg := Config{PollInterval: 5 * time.Millisecond, DriftInterval: 5 * time.Millisecond}
	eng := NewEngine(cfg, h.store, h.sub, h.images, h.resolver, h.box,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithClock(h.clk)
	eng.dutyCalls = map[string]int{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	// 存活判据：毒点常驻的情况下 drift 与 tick 两个分支各跑满 2 拍
	//（第一拍即 panic——能到第二拍说明 panic 未打死循环）。
	deadline := time.Now().Add(5 * time.Second)
	for (eng.dutyCallCount("driftScan") < 2 || eng.dutyCallCount("tick") < 2) && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("Run exited early (drift panic broke through the loop): %v", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
	if got := eng.dutyCallCount("driftScan"); got < 2 {
		t.Fatalf("driftScan ran only %d beats (panic killed the Run loop)", got)
	}
	if got := eng.dutyCallCount("tick"); got < 2 {
		t.Fatalf("tick ran only %d beats (drift panic killed the Run loop)", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit on ctx cancellation")
	}
}
