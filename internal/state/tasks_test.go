package state

// 任务面 state 原语回归（T 线 DT-5 / IMPL-T2-1）：
//   - 守卫② 配额 fail-closed（并发/CPU/内存合计；同事务核对无竞态窗）；
//   - 状态机与事件/审计面（task.created / task.expired / task.stopped /
//     task.failed / task.deleted；env 密文落库）；
//   - 跨令牌隔离（列表按属主过滤）；
//   - task-group 挂靠声明幂等与投影读面；
//   - 终态台账保留期回收。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// seedTaskToken 落一枚在册机具令牌（配额/隔离夹具）。
func seedTaskToken(t *testing.T, st *Store, scopes string) Token {
	t.Helper()
	plaintext := "flt_" + ulid.Make().String()
	tok, err := st.CreateToken(context.Background(), TokenWrite{
		Hash: HashToken(plaintext), Name: "task fixture", Scopes: scopes, Actor: "human",
	})
	if err != nil {
		t.Fatalf("seed task token: %v", err)
	}
	return tok
}

// taskWriteFixture 构造一条最小任务写入（调用方覆盖关注字段）。
func taskWriteFixture(owner, id string) TaskWrite {
	return TaskWrite{
		ID:           id,
		Name:         "fixture task",
		OwnerTokenID: owner,
		Image:        "alpine:3.19@sha256:" + strings.Repeat("a", 64),
		ScopeKind:    "task-group",
		ScopeRef:     "tenant1",
		Network:      "fleetly-taskgroup-tenant1",
		TTLSeconds:   600,
		CPUMillis:    1000,
		MemoryBytes:  256 << 20,
		Service:      "fleetly-task-" + id,
	}
}

func TestCreateTaskQuotaFailClosed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	owner := seedTaskToken(t, st, "tasks")
	quota := TaskQuota{MaxConcurrent: 2, MaxCPUMillis: 2000, MaxMemoryBytes: 512 << 20}

	// 并发上限：第 3 条 fail-closed。
	for i := 0; i < 2; i++ {
		id := ulid.Make().String()
		if _, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, id), quota); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
	}
	if _, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, ulid.Make().String()), quota); !errors.Is(err, ErrTaskQuotaExceeded) {
		t.Fatalf("concurrent quota: err = %v, want ErrTaskQuotaExceeded", err)
	}

	// 资源合计上限：终止一条后并发有余量，但 CPU 合计仍会拦下超量请求。
	rows, err := st.ListTasks(ctx, owner.ID, false, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list tasks: rows=%d err=%v", len(rows), err)
	}
	if changed, err := st.MarkTaskFailed(ctx, rows[0].ID, "fixture done"); err != nil || !changed {
		t.Fatalf("mark failed: changed=%v err=%v", changed, err)
	}
	big := taskWriteFixture(owner.ID, ulid.Make().String())
	big.CPUMillis = 2000
	big.MemoryBytes = 256 << 20
	if _, err := st.CreateTask(ctx, big, quota); !errors.Is(err, ErrTaskQuotaExceeded) {
		t.Fatalf("cpu quota: err = %v, want ErrTaskQuotaExceeded", err)
	}
	// 并发有余量且资源在限内：放行。
	ok := taskWriteFixture(owner.ID, ulid.Make().String())
	if _, err := st.CreateTask(ctx, ok, quota); err != nil {
		t.Fatalf("within quota: %v", err)
	}
}

func TestTaskLifecycleEventsAndAudit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	owner := seedTaskToken(t, st, "tasks")
	id := ulid.Make().String()

	task, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, id), DefaultTaskQuota())
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.Status != TaskQueued || task.Service != "fleetly-task-"+id {
		t.Fatalf("task projection = %+v", task)
	}
	if task.ExpiresAt.Sub(task.CreatedAt) != 600*time.Second {
		t.Fatalf("expires_at - created_at = %s, want 600s", task.ExpiresAt.Sub(task.CreatedAt))
	}

	// queued → running（事件 task.started）。
	if _, changed, err := st.MarkTaskRunning(ctx, id); err != nil || !changed {
		t.Fatalf("mark running: changed=%v err=%v", changed, err)
	}
	if _, changed, err := st.MarkTaskRunning(ctx, id); err != nil || changed {
		t.Fatalf("mark running idempotent: changed=%v err=%v", changed, err)
	}

	// TTL 到期 → stopping + task.expired（reason=expired）。
	stopped, changed, err := st.RequestTaskStop(ctx, id, "expired", "")
	if err != nil || !changed {
		t.Fatalf("request stop expired: changed=%v err=%v", changed, err)
	}
	if stopped.Status != TaskStopping || stopped.StopReason != "expired" {
		t.Fatalf("stopping projection = %+v", stopped)
	}
	if _, changed, err := st.RequestTaskStop(ctx, id, "expired", ""); err != nil || changed {
		t.Fatalf("request stop idempotent: changed=%v err=%v", changed, err)
	}
	if changed, err := st.MarkTaskStopped(ctx, id); err != nil || !changed {
		t.Fatalf("mark stopped: changed=%v err=%v", changed, err)
	}

	// 终态行不再入选非终态工作集；删除墓碑 → 行删除 + task.deleted。
	if rows, err := st.ListNonTerminalTasks(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("non-terminal set = %d rows, err=%v", len(rows), err)
	}
	if _, changed, err := st.RequestTaskDelete(ctx, id, owner.ID); err != nil || !changed {
		t.Fatalf("request delete: changed=%v err=%v", changed, err)
	}
	if changed, err := st.DeleteTaskRow(ctx, id); err != nil || !changed {
		t.Fatalf("delete row: changed=%v err=%v", changed, err)
	}
	if _, err := st.GetTask(ctx, id); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("get deleted task: err = %v, want ErrTaskNotFound", err)
	}

	// 事件与审计面（Outbox 同事务）。
	events, err := st.EventsSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("events since: %v", err)
	}
	got := map[string]int{}
	for _, e := range events {
		got[e.Name]++
		if e.Subject != "task:"+id {
			t.Fatalf("event %s subject = %q, want task:%s", e.Name, e.Subject, id)
		}
	}
	for _, want := range []string{"task.created", "task.started", "task.expired", "task.stopped", "task.deleted"} {
		if got[want] != 1 {
			t.Fatalf("event %s count = %d, want 1 (all=%v)", want, got[want], got)
		}
	}
}

func TestTaskFailureAndCrossOwnerIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ownerA := seedTaskToken(t, st, "tasks")
	ownerB := seedTaskToken(t, st, "tasks")
	idA := ulid.Make().String()
	if _, err := st.CreateTask(ctx, taskWriteFixture(ownerA.ID, idA), DefaultTaskQuota()); err != nil {
		t.Fatalf("create task A: %v", err)
	}
	idB := ulid.Make().String()
	writeB := taskWriteFixture(ownerB.ID, idB)
	if _, err := st.CreateTask(ctx, writeB, DefaultTaskQuota()); err != nil {
		t.Fatalf("create task B: %v", err)
	}

	// 列表按属主过滤（跨令牌不可见——API 层把不可见映射为 404）。
	rowsA, err := st.ListTasks(ctx, ownerA.ID, true, 10)
	if err != nil || len(rowsA) != 1 || rowsA[0].ID != idA {
		t.Fatalf("owner A list = %+v (err=%v)", rowsA, err)
	}

	// 失败终态：单行有界化（多行输入压成单行）。
	multi := "boom\nsecond line\r\nthird"
	if changed, err := st.MarkTaskFailed(ctx, idA, multi); err != nil || !changed {
		t.Fatalf("mark failed: changed=%v err=%v", changed, err)
	}
	failed, err := st.GetTask(ctx, idA)
	if err != nil {
		t.Fatalf("get failed task: %v", err)
	}
	if strings.ContainsAny(failed.Error, "\n\r") {
		t.Fatalf("failure message not single-lined: %q", failed.Error)
	}
	if changed, err := st.MarkTaskFailed(ctx, idA, "again"); err != nil || changed {
		t.Fatalf("failed terminal is sticky: changed=%v err=%v", changed, err)
	}
}

func TestTaskNetworkMembersIdempotentAndProjection(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app := seedApp(t, st, "member-app")
	created, err := st.UpsertTaskNetworkMember(ctx, "tenant1", app.ID, "web")
	if err != nil || !created {
		t.Fatalf("upsert member: created=%v err=%v", created, err)
	}
	if created, err := st.UpsertTaskNetworkMember(ctx, "tenant1", app.ID, "web"); err != nil || created {
		t.Fatalf("upsert member idempotent: created=%v err=%v", created, err)
	}
	members, err := st.ListTaskNetworkMembersForApp(ctx, app.ID)
	if err != nil || len(members) != 1 {
		t.Fatalf("members for app = %+v (err=%v)", members, err)
	}
	if members[0].NetworkRef != "tenant1" || members[0].Service != "web" {
		t.Fatalf("member projection = %+v", members[0])
	}
	byRef, err := st.ListTaskNetworkMembersForRef(ctx, "tenant1")
	if err != nil || len(byRef) != 1 {
		t.Fatalf("members for ref = %+v (err=%v)", byRef, err)
	}
}

func TestTaskQuotaOverridesAndValidation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	owner := seedTaskToken(t, st, "tasks")

	got, err := st.LoadTaskQuota(ctx, owner.ID)
	if err != nil || got != DefaultTaskQuota() {
		t.Fatalf("default quota = %+v (err=%v)", got, err)
	}
	custom := TaskQuota{MaxConcurrent: 3, MaxCPUMillis: 6000, MaxMemoryBytes: 2 << 30}
	if err := st.SaveTaskQuota(ctx, owner.ID, custom); err != nil {
		t.Fatalf("save quota: %v", err)
	}
	if got, err := st.LoadTaskQuota(ctx, owner.ID); err != nil || got != custom {
		t.Fatalf("loaded quota = %+v (err=%v)", got, err)
	}
	if err := st.SaveTaskQuota(ctx, owner.ID, TaskQuota{}); err == nil {
		t.Fatal("save zero quota must be rejected (quota is a safety default, never silently disabled)")
	}

	// 覆盖生效：并发上限 3 时第 4 条拒绝。
	for i := 0; i < 3; i++ {
		if _, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, ulid.Make().String()), custom); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
	}
	if _, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, ulid.Make().String()), custom); !errors.Is(err, ErrTaskQuotaExceeded) {
		t.Fatalf("override quota: err = %v, want ErrTaskQuotaExceeded", err)
	}
}

func TestExpiredTasksAndTerminalPrune(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	owner := seedTaskToken(t, st, "tasks")
	id := ulid.Make().String()
	if _, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, id), DefaultTaskQuota()); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 未到期：不入选；到期后入选。
	rows, err := st.ExpiredTasks(ctx, time.Now().UTC().Add(time.Minute), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("expired before deadline = %d rows (err=%v)", len(rows), err)
	}
	rows, err = st.ExpiredTasks(ctx, time.Now().UTC().Add(48*time.Hour), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expired after deadline = %d rows (err=%v)", len(rows), err)
	}
	// 终态保留期回收。
	if changed, err := st.MarkTaskFailed(ctx, id, "fixture"); err != nil || !changed {
		t.Fatalf("mark failed: changed=%v err=%v", changed, err)
	}
	if n, err := st.PruneTerminalTasksOlderThan(ctx, time.Now().UTC().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune terminal tasks = %d (err=%v)", n, err)
	}
	if _, err := st.GetTask(ctx, id); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("task after prune: err = %v, want ErrTaskNotFound", err)
	}
}
