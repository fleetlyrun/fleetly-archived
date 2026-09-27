package cmd

// 任务命令 CLI 回归（T 线 DT-5 / IMPL-T2-1）：`fleetly tasks run/ls/stop/rm/
// logs/network ensure` 全链走 RPC（apitest 装配 TasksService + 确定性假编排
// 端口）；tasks logs 在无 VL 后端的装配形态诚实报错（E_LOGS_BACKEND_UNAVAILABLE）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTasksCLISurface(t *testing.T) {
	env := startCLI(t)
	env.CreateApp(t, "control")

	// network ensure：长活网 + 控制面挂靠声明（假端口的确定性应答）。
	code, out, errOut := runCLIConn(t, "tasks", "network", "ensure", "--internal", "--member", "control=server", "tenant1")
	if code != 0 {
		t.Fatalf("network ensure: code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"task-group network fleetly-taskgroup-apitest internal=true", "member control service server status=pending"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ensure output = %q, missing %q", out, want)
		}
	}

	// run：受理（queued 视图；digest 解析腿 = 假端口原样直通）。
	code, out, errOut = runCLIConn(t, "tasks", "run",
		"--image", "alpine:3.19", "--scope-kind", "task-group", "--scope-ref", "tenant1",
		"--name", "cli-task", "--ttl", "10m", "--env", "TOKEN=abc", "--json")
	if code != 0 {
		t.Fatalf("tasks run: code=%d stderr=%s", code, errOut)
	}
	var created struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		Service    string `json:"service"`
		DNSName    string `json:"dns_name"`
		TTLSeconds int64  `json:"ttl_seconds"`
		ScopeKind  string `json:"scope_kind"`
		ScopeRef   string `json:"scope_ref"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("tasks run --json: %v (out=%q)", err, out)
	}
	if created.ID == "" || created.Status != "queued" || created.ScopeKind != "task-group" || created.ScopeRef != "tenant1" {
		t.Fatalf("created task = %+v", created)
	}
	if created.TTLSeconds != 600 {
		t.Fatalf("ttl_seconds = %d, want 600", created.TTLSeconds)
	}
	if created.Service == "" || !strings.Contains(created.Service, created.ID) {
		t.Fatalf("service = %q must embed the task id %q (stable DNS name)", created.Service, created.ID)
	}
	if created.DNSName != created.Service {
		t.Fatalf("dns_name = %q, want the stable service name %q", created.DNSName, created.Service)
	}
	// 用法错误：缺 --image → 64。
	if code, _, _ := runCLIConn(t, "tasks", "run", "--scope-kind", "task-group", "--scope-ref", "tenant1"); code != exitUsage {
		t.Fatalf("missing --image code = %d, want %d", code, exitUsage)
	}

	// ls：缺省列在途；带上 id。
	code, out, errOut = runCLIConn(t, "tasks", "ls", "--json")
	if code != 0 {
		t.Fatalf("tasks ls: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, created.ID) || !strings.Contains(out, `"status": "queued"`) {
		t.Fatalf("ls --json = %q, want the created task", out)
	}

	// stop → stopping；rm → deleting。
	code, out, errOut = runCLIConn(t, "tasks", "stop", created.ID)
	if code != 0 || !strings.Contains(out, "status=stopping") {
		t.Fatalf("tasks stop: code=%d out=%q stderr=%s", code, out, errOut)
	}
	code, out, errOut = runCLIConn(t, "tasks", "rm", created.ID)
	if code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("tasks rm: code=%d out=%q stderr=%s", code, out, errOut)
	}

	// logs：本装配无 VL 后端 → 诚实报错（不返回空列表冒充）。
	code, _, errOut = runCLIConn(t, "tasks", "logs", created.ID)
	if code == 0 {
		t.Fatal("tasks logs without a log backend must fail honestly")
	}
	if !strings.Contains(errOut, "E_LOGS_BACKEND_UNAVAILABLE") {
		t.Fatalf("tasks logs stderr = %q, want E_LOGS_BACKEND_UNAVAILABLE", errOut)
	}
	// 用法错误：缺位置参数 → 64。
	if code, _, _ := runCLIConn(t, "tasks", "logs"); code != exitUsage {
		t.Fatalf("missing id code = %d, want %d", code, exitUsage)
	}
}
