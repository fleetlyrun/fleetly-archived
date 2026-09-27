package naming

// 任务命名族回归（T 线 DT-5 / IMPL-T2-1）：任务服务名 fleetly-task-<ULID>
// 与 task-group 网名 fleetly-taskgroup-<ref> 的公式表、形态校验与前缀族
// 结构不相交（team slug 取 task / taskgroup 的最凶形态）。

import (
	"strings"
	"testing"
)

func TestTaskNamingFormulas(t *testing.T) {
	const taskID = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	service, err := TaskServiceName(taskID)
	if err != nil || service != "fleetly-task-"+taskID {
		t.Fatalf("TaskServiceName = %q (err=%v), want fleetly-task-%s", service, err, taskID)
	}
	if !IsTaskServiceName(service) {
		t.Fatalf("IsTaskServiceName(%q) = false", service)
	}
	network, err := TaskGroupNetworkName("tenant1")
	if err != nil || network != "fleetly-taskgroup-tenant1" {
		t.Fatalf("TaskGroupNetworkName = %q (err=%v)", network, err)
	}
	if !IsTaskGroupNetworkName(network) {
		t.Fatalf("IsTaskGroupNetworkName(%q) = false", network)
	}

	// 形态负路径：任务 ID 必须 26 位 Crockford；task-group ref 必须
	// [a-z0-9]{2,32} 且不含 '-'（结构不相交的前提）。
	if _, err := TaskServiceName("not-a-ulid"); err == nil {
		t.Fatal("non-ULID task id must be rejected")
	}
	if _, err := TaskServiceName(strings.ToLower(taskID)); err == nil {
		t.Fatal("lowercase task id must be rejected (Crockford is uppercase)")
	}
	for _, bad := range []string{"", "a", "-abc", "ab-c", "ABC", strings.Repeat("a", 33)} {
		if err := ValidateTaskGroupRef(bad); err == nil {
			t.Errorf("ValidateTaskGroupRef(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"ab", "tenant1", "t0123456789"} {
		if err := ValidateTaskGroupRef(good); err != nil {
			t.Errorf("ValidateTaskGroupRef(%q) = %v, want nil", good, err)
		}
	}
	// 前缀族识别不误伤：非任务族对象一律 false。
	for _, name := range []string{"fleetly-taskgroup-tenant1", "fleetly-task-", "fleetly-demo-web", "fleetly-project-01JABCDEFGHJKMNPQRSTVWXYZ0"} {
		if IsTaskServiceName(name) {
			t.Errorf("IsTaskServiceName(%q) = true, want false", name)
		}
	}
}

// TestTaskNamingFamiliesAreStructurallyDisjoint 钉死「task/taskgroup 两个
// 前缀族与三段 app 命名不相交」——team slug 取最凶形态 taskgroup/task：
// app 网名含 '-' 与小写，task-group ref 禁止 '-'、任务 ID 是 Crockford
// 大写——结构上不可能相撞。
func TestTaskNamingFamiliesAreStructurallyDisjoint(t *testing.T) {
	appNet, err := NetworkName("taskgroup", "default", "web")
	if err != nil {
		t.Fatalf("app network name: %v", err)
	}
	if IsTaskGroupNetworkName(appNet) {
		t.Fatalf("app network %q misidentified as a task-group network", appNet)
	}
	taskService, err := TaskServiceName("01JABCDEFGHJKMNPQRSTVWXYZ0")
	if err != nil {
		t.Fatalf("task service name: %v", err)
	}
	appService, err := ServiceName("task", "default", "web", "worker")
	if err != nil {
		t.Fatalf("app service name: %v", err)
	}
	if taskService == appService {
		t.Fatalf("task service %q collides with the app service name", taskService)
	}
	if IsTaskServiceName(appService) {
		t.Fatalf("app service %q misidentified as a task service", appService)
	}
	// 保留字纵深防御：taskgroup 进保留清单（ref 字符集放宽时的兜底）。
	if !IsReservedTeamSlug("taskgroup") {
		t.Fatal("taskgroup must stay in the reserved team slug set (defence in depth)")
	}
}
