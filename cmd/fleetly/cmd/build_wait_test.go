package cmd

// IMPL-ARCH-K（2026-09-28 架构评审候选 9 后半）build 等待环收编 SDK
// WaitBuild 后的 CLI 面守卫：超时提示两形态的措辞映射与 stayed-queued 端到
// 端路径。既有 golden/CLI 测试零修改是硬约束——本文件只新增，不动存量。

import (
	"strings"
	"testing"
	"time"
)

// cliBuildApp 是最小合法 compose（build 模式——构建行入队、等待环生效；
// 与 cliWebApp 的 image 模式互补）。context 指夹具所在目录，TriggerBuild
// 的包含性校验（context 不得越出 base_dir）天然满足。
const cliBuildApp = `
name: my-api
services:
  web:
    build:
      context: .
      dockerfile: Dockerfile
`

// TestBuildWaitTimeoutMessageMapping SDK WaitTimeoutError 快照 → CLI 两条既
// 有超时提示的分支映射（措辞与收编前 CLI 自持环逐字一致）。
func TestBuildWaitTimeoutMessageMapping(t *testing.T) {
	allQueued := buildTimeoutError(defaultBuildTimeout, map[string]string{
		"b1": "queued", "b2": "queued",
	})
	if allQueued.Error() != "builds stayed queued for 45m0s — is fleetlyd running? builds are executed by fleetlyd's build.queue service (this command only enqueues and waits)" {
		t.Fatalf("stayed-queued message drifted: %v", allQueued)
	}
	mixed := buildTimeoutError(time.Second, map[string]string{
		"b1": "queued", "b2": "building",
	})
	if mixed.Error() != "builds did not finish within 1s (in-flight builds keep running; check 'fleetly builds list')" {
		t.Fatalf("in-flight message drifted: %v", mixed)
	}
}

// TestBuildWaitStayedQueuedHint 端到端：无构建执行面的夹具（startCLI，
// queue=nil → 构建行恒 queued）下 build --timeout 触发 stayed-queued 提
// 示——SDK 收编轮询环后 CLI 对外文案逐字保持的证据。
func TestBuildWaitStayedQueuedHint(t *testing.T) {
	startCLI(t)
	composeFile := writeCLIFixture(t, cliBuildApp)

	code, out, errOut := runCLIConn(t, "build", "--timeout", "300ms", composeFile)
	if code != 1 {
		t.Fatalf("build timeout: code=%d out=%s", code, out)
	}
	if !strings.Contains(errOut, "builds stayed queued for 300ms") ||
		!strings.Contains(errOut, "is fleetlyd running?") {
		t.Fatalf("stayed-queued hint missing: stderr=%q", errOut)
	}
}
