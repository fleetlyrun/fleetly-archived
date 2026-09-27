package cmd

// 项目网参与 CLI 测试（T 线 OT-1 / IMPL-T15-1）：`fleetly projects network
// <show|attach|detach>` 全链经 RPC（apitest 装配 ProjectsService + 确定性
// 假编排端口：无部署史 → status=attached/detached，roll 语义在 engine 测试
// 覆盖）；用法错误退出码 64。

import (
	"strings"
	"testing"
)

func TestProjectsNetworkSurface(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "my-api")

	// show：缺省不参加（OT-1 缺省隔离现状）——项目限定形 + detached 文案。
	code, out, errOut := runCLIConn(t, "projects", "network", "show", "my-api")
	if code != 0 {
		t.Fatalf("network show: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "app my-api") || !strings.Contains(out, "detached") {
		t.Fatalf("show output = %q, want the detached default state", out)
	}

	// attach：项目网名（fleetly-project-<id>）+ 状态 + changed 投影。
	code, out, errOut = runCLIConn(t, "projects", "network", "attach", "my-api")
	if code != 0 {
		t.Fatalf("network attach: code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"attached to project network fleetly-project-", "changed=true", "status=attached"} {
		if !strings.Contains(out, want) {
			t.Fatalf("attach output = %q, missing %q", out, want)
		}
	}

	// show 随行（attached + 项目网名）。
	code, out, errOut = runCLIConn(t, "projects", "network", "show", "my-api")
	if code != 0 {
		t.Fatalf("network show after attach: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "attached (fleetly-project-") {
		t.Fatalf("show after attach = %q, want attached + network name", out)
	}
	code, out, errOut = runCLIConn(t, "projects", "network", "show", "--json", "my-api")
	if code != 0 {
		t.Fatalf("network show --json: code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{`"project_network_attached": true`, `"project_network": "fleetly-project-`} {
		if !strings.Contains(out, want) {
			t.Fatalf("show --json = %q, missing %q", out, want)
		}
	}

	// detach：changed=true + status=detached。
	code, out, errOut = runCLIConn(t, "projects", "network", "detach", "my-api")
	if code != 0 {
		t.Fatalf("network detach: code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"detached from project network fleetly-project-", "changed=true", "status=detached"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detach output = %q, missing %q", out, want)
		}
	}

	// 用法错误：缺 app / 缺子命令 → 64。
	if code, _, errOut := runCLIConn(t, "projects", "network", "attach"); code != 64 {
		t.Fatalf("attach without app: code=%d stderr=%s, want usage (64)", code, errOut)
	}
	if code, _, errOut := runCLIConn(t, "projects"); code != 64 {
		t.Fatalf("projects without subcommand: code=%d stderr=%s, want usage (64)", code, errOut)
	}
	if code, _, errOut := runCLIConn(t, "projects", "network"); code != 64 {
		t.Fatalf("network without subcommand: code=%d stderr=%s, want usage (64)", code, errOut)
	}
}
