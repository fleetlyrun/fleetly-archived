package state

// 发布状态机转移表的 state 侧穷举测试（S16-C5）：合法集合与引擎写点一一
// 对应；UpdateDeployment 的 CAS 分支按表拒写非法转移（含终态出边——
// ErrIllegalTransition 是 ErrDeploymentStateTransition 的特化，双哨兵皆真）。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestTransitionMatrixExhaustive(t *testing.T) {
	// 合法转移全集（release-semantics §2.3：building 直通由 preparing→
	// releasing 边承载；observing 不可取消；引擎写点枚举见 machine.go 头注）。
	legal := map[DeploymentStatus]map[DeploymentStatus]bool{
		DeployQueued:    {DeployPreparing: true, DeployFailed: true, DeployCancelled: true},
		DeployPreparing: {DeployBuilding: true, DeployReleasing: true, DeployFailed: true, DeployCancelled: true},
		DeployBuilding:  {DeployReleasing: true, DeployFailed: true, DeployCancelled: true},
		DeployReleasing: {DeployObserving: true, DeployFailed: true, DeployCancelled: true},
		DeployObserving: {DeploySucceeded: true, DeployFailed: true},
		DeploySucceeded: {},
		DeployFailed:    {},
		DeployCancelled: {},
	}
	for _, from := range AllDeploymentStatuses() {
		for _, to := range AllDeploymentStatuses() {
			if got := CanTransitionDeployment(from, to); got != legal[from][to] {
				t.Fatalf("CanTransitionDeployment(%s, %s) = %v, want %v", from, to, got, legal[from][to])
			}
		}
	}
	// 未知状态一律非法。
	if CanTransitionDeployment(DeploymentStatus("bogus"), DeployQueued) ||
		CanTransitionDeployment(DeployQueued, DeploymentStatus("bogus")) {
		t.Fatal("unknown status should be illegal")
	}
}

// TestUpdateDeploymentRejectsIllegalTransition S16-C5 机制验收：CAS 前按
// 转移表校验——表外组合（如 observing → queued）即使 from 谓词与当前行
// 一致也拒写（行未被改动），返回 ErrIllegalTransition（且属
// ErrDeploymentStateTransition 家族——引擎既有幂等收敛分支不变）。
func TestUpdateDeploymentRejectsIllegalTransition(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "machine.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app, err := seedAppE(t, st, "machine-app")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	rec, err := st.CreateDeployment(ctx, DeployRecord{AppID: app.ID, AppName: app.Name, Kind: "deploy"})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	// 推进到 observing（合法链：queued → preparing → releasing → observing）。
	for _, step := range [][2]DeploymentStatus{
		{DeployQueued, DeployPreparing},
		{DeployPreparing, DeployReleasing},
		{DeployReleasing, DeployObserving},
	} {
		from, to := step[0], step[1]
		if err := st.UpdateDeployment(ctx, rec.ID, DeploymentPatch{Status: &to, PrevStatus: &from}); err != nil {
			t.Fatalf("legal transition %s -> %s: %v", from, to, err)
		}
	}

	// 非法转移（表外组合：observing → queued；from 谓词与当前行一致）→ 拒写。
	queued := DeployQueued
	observing := DeployObserving
	err = st.UpdateDeployment(ctx, rec.ID, DeploymentPatch{Status: &queued, PrevStatus: &observing})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("observing -> queued err = %v, want ErrIllegalTransition", err)
	}
	if !errors.Is(err, ErrDeploymentStateTransition) {
		t.Fatalf("ErrIllegalTransition should belong to the ErrDeploymentStateTransition family: %v", err)
	}
	// 行未被改动（仍 observing）。
	row, err := st.GetDeployment(ctx, rec.ID)
	if err != nil || row.Status != DeployObserving {
		t.Fatalf("row after rejected write = %+v err=%v, want observing", row, err)
	}

	// 终态出边同样被表拒绝（succeeded → failed，from 谓词构造终态）。
	succ, failed := DeploySucceeded, DeployFailed
	if err := st.UpdateDeployment(ctx, rec.ID, DeploymentPatch{Status: &succ, PrevStatus: &observing}); err != nil {
		t.Fatalf("observing -> succeeded: %v", err)
	}
	if err := st.UpdateDeployment(ctx, rec.ID, DeploymentPatch{Status: &failed, PrevStatus: &succ}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("succeeded -> failed err = %v, want ErrIllegalTransition", err)
	}
	// 未知目标状态拒绝（词表外——不落库即拒，无需 CAS 谓词）。
	bogus := DeploymentStatus("bogus")
	if err := st.UpdateDeployment(ctx, rec.ID, DeploymentPatch{Status: &bogus}); err == nil {
		t.Fatal("unknown target status accepted")
	}
}

// TestEnterPhaseSingleWritePoint T0-V2.2 机制验收：EnterPhase 单写点同事务
// 完成转移校验 + 字段写 + 拾取锚点 + 事件——
//  1. 拾取边（queued→preparing）携带锚点与事件：状态/锚点/事件三者同拍
//     落库（H11/S9 预算基线与转换原子）；
//  2. 非拾取边不刷新锚点（preparing→building 共用拾取基线——写点若在
//     building 入口重摆锚点即私自续预算，违反 S9）；
//  3. 拾取边缺锚：拒写（锚点漏写因单写点不可发生），行与事件均未动；
//  4. 表外组合：拒写且**不产生事件**（事务回滚——事件披露与转换落库
//     原子，杜绝「转换被拒、事件照发」的假信号）；
//  5. CAS 竞争落败：ErrDeploymentStateTransition 且无事件（同上）。
func TestEnterPhaseSingleWritePoint(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "enterphase.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app, err := seedAppE(t, st, "enterphase-app")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	newRec := func() DeployRecord {
		t.Helper()
		rec, err := st.CreateDeployment(ctx, DeployRecord{AppID: app.ID, AppName: app.Name, Kind: "deploy"})
		if err != nil {
			t.Fatalf("create deployment: %v", err)
		}
		return rec
	}
	// 锚点由调用方时钟提供（引擎单测的假时钟下，写点自取墙钟会错位预算
	// 基准——契约见 Store.EnterPhase 注 3）。
	anchor := time.Unix(0, time.Now().UTC().UnixNano()).UTC()
	pickupEvent := func(id string) Event {
		return Event{Name: "deployment.release_started", Subject: "deployment:" + id}
	}

	// 1. 合法拾取边：锚点 + 状态 + 事件同一事务落库。
	rec := newRec()
	if err := st.EnterPhase(ctx, rec.ID, DeployQueued, DeployPreparing,
		DeploymentPatch{PhaseStartedAt: &anchor}, pickupEvent(rec.ID)); err != nil {
		t.Fatalf("enter preparing: %v", err)
	}
	row, err := st.GetDeployment(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if row.Status != DeployPreparing || !row.PhaseStartedAt.Equal(anchor) {
		t.Fatalf("row status=%s anchor=%v, want preparing/%v (same-transaction anchor write)", row.Status, row.PhaseStartedAt, anchor)
	}
	if names := eventNames(t, st); len(names) != 1 || names[0] != "deployment.release_started" {
		t.Fatalf("events = %v, want exactly [deployment.release_started]", names)
	}

	// 2. 非拾取边不刷新锚点：preparing→building 走写点、补丁不携带锚点，
	// 状态推进而基线保持拾取时刻。
	if err := st.EnterPhase(ctx, rec.ID, DeployPreparing, DeployBuilding, DeploymentPatch{}); err != nil {
		t.Fatalf("enter building: %v", err)
	}
	row, err = st.GetDeployment(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if row.Status != DeployBuilding || !row.PhaseStartedAt.Equal(anchor) {
		t.Fatalf("row status=%s anchor=%v, want building/%v (anchor must not be re-armed outside the pickup edge)",
			row.Status, row.PhaseStartedAt, anchor)
	}

	// 3. 拾取边缺锚：拒写（行保持 queued、无新事件）。
	rec2 := newRec()
	if err := st.EnterPhase(ctx, rec2.ID, DeployQueued, DeployPreparing, DeploymentPatch{}); err == nil {
		t.Fatal("queued -> preparing without phase_started_at anchor must be rejected (S9)")
	}
	if row2, err := st.GetDeployment(ctx, rec2.ID); err != nil || row2.Status != DeployQueued {
		t.Fatalf("row after rejected anchorless pickup = %+v err=%v, want queued untouched", row2, err)
	}
	if names := eventNames(t, st); len(names) != 1 {
		t.Fatalf("events after rejections = %v, want unchanged (no event without a committed transition)", names)
	}

	// 4. 表外组合（queued→queued 无边；事件入参随事务回滚）：拒写、无事件。
	if err := st.EnterPhase(ctx, rec2.ID, DeployQueued, DeployQueued, DeploymentPatch{},
		pickupEvent(rec2.ID)); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("queued -> queued err = %v, want ErrIllegalTransition", err)
	}
	if names := eventNames(t, st); len(names) != 1 {
		t.Fatalf("events after illegal transition = %v, want unchanged (same-transaction rollback)", names)
	}

	// 5. CAS 竞争落败（合法边但行已推进）：家族哨兵、无事件。
	if err := st.EnterPhase(ctx, rec2.ID, DeployQueued, DeployPreparing,
		DeploymentPatch{PhaseStartedAt: &anchor}, pickupEvent(rec2.ID)); err != nil {
		t.Fatalf("enter preparing rec2: %v", err)
	}
	if err := st.EnterPhase(ctx, rec2.ID, DeployQueued, DeployPreparing,
		DeploymentPatch{PhaseStartedAt: &anchor}, pickupEvent(rec2.ID)); !errors.Is(err, ErrDeploymentStateTransition) {
		t.Fatalf("duplicate pickup err = %v, want ErrDeploymentStateTransition", err)
	}
	if names := eventNames(t, st); len(names) != 2 {
		t.Fatalf("events = %v, want 2 (one release_started per committed pickup, none for the lost race)", names)
	}
}

// eventNames 返回库内事件名序列（断言辅助）。
func eventNames(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.EventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, e := range rows {
		out = append(out, e.Name)
	}
	return out
}

// ─── 任务状态机（IMPL-ARCH-C2）───

func TestTaskTransitionMatrixExhaustive(t *testing.T) {
	// 合法转移全集（与 T2-1 六个写点原语的旧 WHERE 谓词一一对应：终态行
	// 唯一出边是 → deleting 删除墓碑；deleting 无出边——出口是行删除）。
	legal := map[TaskStatus]map[TaskStatus]bool{
		TaskQueued:   {TaskRunning: true, TaskStopping: true, TaskFailed: true, TaskDeleting: true},
		TaskRunning:  {TaskStopping: true, TaskFailed: true, TaskDeleting: true},
		TaskStopping: {TaskStopped: true, TaskFailed: true, TaskDeleting: true},
		TaskStopped:  {TaskDeleting: true},
		TaskFailed:   {TaskDeleting: true},
		TaskDeleting: {},
	}
	for _, from := range AllTaskStatuses() {
		for _, to := range AllTaskStatuses() {
			if got := CanTransitionTask(from, to); got != legal[from][to] {
				t.Fatalf("CanTransitionTask(%s, %s) = %v, want %v", from, to, got, legal[from][to])
			}
		}
	}
	// 未知状态一律非法。
	if CanTransitionTask(TaskStatus("bogus"), TaskQueued) ||
		CanTransitionTask(TaskQueued, TaskStatus("bogus")) {
		t.Fatal("unknown status should be illegal")
	}
	// 表与谓词同源自检：LegalTaskTransitions 与 CanTransitionTask 双向一致。
	for _, from := range AllTaskStatuses() {
		edges := map[TaskStatus]bool{}
		for _, to := range LegalTaskTransitions(from) {
			edges[to] = true
			if !CanTransitionTask(from, to) {
				t.Fatalf("LegalTaskTransitions(%s) yields %s but CanTransitionTask denies it", from, to)
			}
		}
		for _, to := range AllTaskStatuses() {
			if !edges[to] && CanTransitionTask(from, to) {
				t.Fatalf("CanTransitionTask(%s, %s) = true but the edge is outside LegalTaskTransitions", from, to)
			}
		}
	}
}

// TestTaskWritePointRejectsIllegalTransition 写点机制验收（S16-C5 任务版）：
// updateTask 在 CAS 前按 taskTransitions 校验——表外组合（含自边缺失）即使
// 行恰为 from 也拒写（行未动、事务回滚无事件/审计）；裸状态写（缺
// PrevStatus）与词表外目标结构性拒绝。
func TestTaskWritePointRejectsIllegalTransition(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "taskmachine.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	owner := seedTaskToken(t, st, "tasks")
	id := ulid.Make().String()
	if _, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, id), DefaultTaskQuota()); err != nil {
		t.Fatalf("create task: %v", err)
	}
	baseline := len(eventNames(t, st)) // task.created

	tryWrite := func(p TaskPatch) error {
		t.Helper()
		return st.InTx(ctx, func(tx *Tx) error {
			_, err := updateTask(ctx, tx.Tx, id, p)
			return err
		})
	}

	// 表外组合（queued → queued：自边不存在）→ 拒写。
	if err := tryWrite(TaskPatch{Status: TaskQueued, PrevStatus: TaskQueued}); !errors.Is(err, ErrIllegalTaskTransition) {
		t.Fatalf("queued -> queued err = %v, want ErrIllegalTaskTransition", err)
	}
	// 表外组合（queued → stopped：跳过 running/stopping 的捷径）→ 拒写。
	if err := tryWrite(TaskPatch{Status: TaskStopped, PrevStatus: TaskQueued}); !errors.Is(err, ErrIllegalTaskTransition) {
		t.Fatalf("queued -> stopped err = %v, want ErrIllegalTaskTransition", err)
	}
	// 裸状态写（缺 PrevStatus）→ 结构性拒绝。
	if err := tryWrite(TaskPatch{Status: TaskRunning}); err == nil {
		t.Fatal("bare status write (no PrevStatus) must be rejected")
	}
	// 词表外目标 → 拒写。
	if err := tryWrite(TaskPatch{Status: TaskStatus("bogus"), PrevStatus: TaskQueued}); err == nil {
		t.Fatal("unknown target status accepted")
	}
	// 拒写零副作用：行仍在 queued，事件/审计未增长（事务回滚——无假信号）。
	row, err := st.GetTask(ctx, id)
	if err != nil || row.Status != TaskQueued {
		t.Fatalf("row after rejections = %+v err=%v, want queued untouched", row, err)
	}
	if got := len(eventNames(t, st)); got != baseline {
		t.Fatalf("events after rejections = %d, want %d (no event without a committed transition)", got, baseline)
	}
	auditBaseline, err := st.RecentAudits(ctx, 100)
	if err != nil {
		t.Fatalf("recent audits: %v", err)
	}
	_ = tryWrite(TaskPatch{Status: TaskQueued, PrevStatus: TaskQueued}) // 再拒一次
	if audits, err := st.RecentAudits(ctx, 100); err != nil || len(audits) != len(auditBaseline) {
		t.Fatalf("audits after rejections = %d, want %d (no audit without a committed transition)",
			len(audits), len(auditBaseline))
	}
}

// TestTaskIllegalTransitionsStayNoOps 意图原语端到端：表外组合经包装函数
// 保持既有幂等零变更语义（行为保持硬约束——changed=false、行不动、审计/
// 事件零增长）；n==0 幂等重入路径行为不变；行不在/越权的 ErrTaskNotFound
// 面不变。
func TestTaskIllegalTransitionsStayNoOps(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "tasknoop.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	owner := seedTaskToken(t, st, "tasks")
	newTask := func() string {
		t.Helper()
		id := ulid.Make().String()
		if _, err := st.CreateTask(ctx, taskWriteFixture(owner.ID, id), DefaultTaskQuota()); err != nil {
			t.Fatalf("create task: %v", err)
		}
		return id
	}

	// 1. stopped 行（终态）：除 →deleting 外全部表外 → 幂等零变更。
	stoppedID := newTask()
	if _, changed, err := st.MarkTaskRunning(ctx, stoppedID); err != nil || !changed {
		t.Fatalf("mark running: changed=%v err=%v", changed, err)
	}
	if _, changed, err := st.RequestTaskStop(ctx, stoppedID, "owner", ""); err != nil || !changed {
		t.Fatalf("request stop: changed=%v err=%v", changed, err)
	}
	if changed, err := st.MarkTaskStopped(ctx, stoppedID); err != nil || !changed {
		t.Fatalf("mark stopped: changed=%v err=%v", changed, err)
	}
	if _, changed, err := st.MarkTaskRunning(ctx, stoppedID); err != nil || changed {
		t.Fatalf("running CAS miss on stopped row: changed=%v err=%v", changed, err)
	}
	if changed, err := st.MarkTaskFailed(ctx, stoppedID, "late failure"); err != nil || changed {
		t.Fatalf("failed on terminal row: changed=%v err=%v (sticky, no error)", changed, err)
	}
	if _, changed, err := st.RequestTaskStop(ctx, stoppedID, "owner", ""); err != nil || changed {
		t.Fatalf("stop on terminal row: changed=%v err=%v (idempotent no-change)", changed, err)
	}
	if row, err := st.GetTask(ctx, stoppedID); err != nil || row.Status != TaskStopped {
		t.Fatalf("row after terminal no-ops = %+v err=%v, want stopped untouched", row, err)
	}
	// 终态行的唯一合法出边：→ deleting 墓碑（RequestTaskDelete）。
	if _, changed, err := st.RequestTaskDelete(ctx, stoppedID, owner.ID); err != nil || !changed {
		t.Fatalf("terminal row to deleting: changed=%v err=%v", changed, err)
	}
	if _, changed, err := st.RequestTaskDelete(ctx, stoppedID, owner.ID); err != nil || changed {
		t.Fatalf("delete reentry (n==0 path): changed=%v err=%v", changed, err)
	}
	if changed, err := st.DeleteTaskRow(ctx, stoppedID); err != nil || !changed {
		t.Fatalf("delete row: changed=%v err=%v", changed, err)
	}
	if changed, err := st.DeleteTaskRow(ctx, stoppedID); err != nil || changed {
		t.Fatalf("delete row reentry: changed=%v err=%v", changed, err)
	}

	// 2. stopping 行：重入 stop/删除受理零变更；running→stopped 捷径被 CAS 拦下。
	stoppingID := newTask()
	if _, changed, err := st.MarkTaskRunning(ctx, stoppingID); err != nil || !changed {
		t.Fatalf("mark running: changed=%v err=%v", changed, err)
	}
	if _, changed, err := st.RequestTaskStop(ctx, stoppingID, "expired", ""); err != nil || !changed {
		t.Fatalf("request stop expired: changed=%v err=%v", changed, err)
	}
	if _, changed, err := st.RequestTaskStop(ctx, stoppingID, "expired", ""); err != nil || changed {
		t.Fatalf("stop reentry (n==0 path): changed=%v err=%v", changed, err)
	}
	if changed, err := st.MarkTaskStopped(ctx, stoppingID); err != nil || !changed {
		t.Fatalf("mark stopped: changed=%v err=%v", changed, err)
	}

	// 3. 行不在：可识别错误面不变（stop/delete 显式 ErrTaskNotFound；
	// running 经事务外重读 ErrTaskNotFound；failed 幂等零变更）。
	missing := ulid.Make().String()
	if _, _, err := st.RequestTaskStop(ctx, missing, "owner", ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("stop missing row err = %v, want ErrTaskNotFound", err)
	}
	if _, _, err := st.RequestTaskDelete(ctx, missing, owner.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("delete missing row err = %v, want ErrTaskNotFound", err)
	}
	if changed, err := st.MarkTaskFailed(ctx, missing, "boom"); err != nil || changed {
		t.Fatalf("failed on missing row: changed=%v err=%v (idempotent no-row)", changed, err)
	}
	if _, _, err := st.MarkTaskRunning(ctx, missing); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("running missing row err = %v, want ErrTaskNotFound", err)
	}

	// 4. 事件披露面：只有真实落库的转移产生事件，幂等重入/表外组合零事件。
	var got []string
	events, err := st.EventsSince(ctx, 0, 100)
	if err != nil {
		t.Fatalf("events since: %v", err)
	}
	for _, e := range events {
		got = append(got, e.Name)
	}
	want := map[string]int{
		"task.created": 2, "task.started": 2, "task.expired": 1,
		"task.stopped": 2, "task.deleted": 1,
	}
	counts := map[string]int{}
	for _, name := range got {
		counts[name]++
	}
	for name, n := range want {
		if counts[name] != n {
			t.Fatalf("event %s count = %d, want %d (all=%v)", name, counts[name], n, counts)
		}
	}
}
