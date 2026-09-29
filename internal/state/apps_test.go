package state

import (
	"context"
	"errors"
	"testing"
)

// tombstone 语义（state-model §2.6 + 冻结清单 §2.3「不复活测试」）：
// 删除 = deleting → deleted 状态位；恢复（DB 回填/同名重建尝试）不得把
// deleted 行翻回 active。
func TestAppTombstoneLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if app.Lifecycle != LifecycleActive {
		t.Fatalf("new app lifecycle = %s, want active", app.Lifecycle)
	}

	// 非法迁移：active 直达 deleted 被拒绝（必须经 deleting，防跳过清理）。
	if err := st.MarkAppDeleted(ctx, app.ID); !errors.Is(err, ErrInvalidLifecycleTransition) {
		t.Fatalf("active→deleted must be rejected, got: %v", err)
	}

	if err := st.MarkAppDeleting(ctx, app.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	got, err := st.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got.Lifecycle != LifecycleDeleting || got.DeletingAt.IsZero() {
		t.Fatalf("lifecycle = %s deleting_at zero=%v, want deleting with stamp", got.Lifecycle, got.DeletingAt.IsZero())
	}

	// 重复进入 deleting 被拒绝。
	if err := st.MarkAppDeleting(ctx, app.ID); !errors.Is(err, ErrInvalidLifecycleTransition) {
		t.Fatalf("deleting→deleting must be rejected, got: %v", err)
	}

	if err := st.MarkAppDeleted(ctx, app.ID); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	got, err = st.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app after delete: %v", err)
	}
	if got.Lifecycle != LifecycleDeleted || got.DeletedAt.IsZero() {
		t.Fatalf("lifecycle = %s deleted_at zero=%v, want deleted with stamp", got.Lifecycle, got.DeletedAt.IsZero())
	}
}

// TestAppTombstoneNotResurrected 「恢复不复活」：同名重建与状态位覆盖
// 均不得把 deleted 翻回 active——deleted 名字在保留期内仍被 tombstone
// 占用（恢复流程若按 DB 回填重放，也只能看到 deleted 行）。
func TestAppTombstoneNotResurrected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.MarkAppDeleting(ctx, app.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	if err := st.MarkAppDeleted(ctx, app.ID); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	// 同名 CreateApp（同项目）：拒绝（ErrAppExists），且绝不复用/改写
	// tombstone 行。（v0.3 名字占用语义 = project 内占用——同项目重名冲突。）
	if _, err := st.CreateApp(ctx, "", "demo", app.ProjectID, app.TeamID); !errors.Is(err, ErrAppExists) {
		t.Fatalf("recreate over tombstone must return ErrAppExists, got: %v", err)
	}
	got, err := st.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got.Lifecycle != LifecycleDeleted {
		t.Fatalf("tombstone resurrected: lifecycle = %s, want deleted", got.Lifecycle)
	}
	if got.ID != app.ID {
		t.Fatalf("row id changed: %s -> %s", app.ID, got.ID)
	}

	// 同 id 的 deleted 行重复进入 deleting/deleted 同样被拒绝（状态机
	// 单向：恢复路径不能通过生命周期接口翻回 active）。
	if err := st.MarkAppDeleting(ctx, app.ID); !errors.Is(err, ErrInvalidLifecycleTransition) {
		t.Fatalf("deleted→deleting must be rejected, got: %v", err)
	}
}

// TestAppNameOccupiedWhileDeleting deleting 名字同样占用。
func TestAppNameOccupiedWhileDeleting(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.MarkAppDeleting(ctx, app.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	if _, err := st.CreateApp(ctx, "", "demo", app.ProjectID, app.TeamID); !errors.Is(err, ErrAppExists) {
		t.Fatalf("create while deleting must be ErrAppExists, got: %v", err)
	}
}

// TestListAppsByLifecycle 按生命周期位查询（H10/MG-3：引擎 deleting 回收
// 回收步的候选集——只返回指定位的应用，空集返回空切片语义）。
func TestListAppsByLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	demo, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create demo: %v", err)
	}
	other, err := seedAppE(t, st, "other")
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	if err := st.MarkAppDeleting(ctx, demo.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}

	deleting, err := st.ListAppsByLifecycle(ctx, LifecycleDeleting)
	if err != nil {
		t.Fatalf("list deleting: %v", err)
	}
	if len(deleting) != 1 || deleting[0].ID != demo.ID {
		t.Fatalf("deleting set = %+v, want only demo", deleting)
	}
	active, err := st.ListAppsByLifecycle(ctx, LifecycleActive)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 || active[0].ID != other.ID {
		t.Fatalf("active set = %+v, want only other", active)
	}
	// deleted 位当前为空集（空切片非 nil 语义可接受，长度必须为 0）。
	deleted, err := st.ListAppsByLifecycle(ctx, LifecycleDeleted)
	if err != nil {
		t.Fatalf("list deleted: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted set = %+v, want empty", deleted)
	}
}

// TestTxMarkAppDeletedTransactional 事务内 tombstone 第二拍（H10/MG-3：
// 引擎收敛步与终局事件/审计同事务的组合原语）——非法迁移（active 直达
// deleted）在事务形态下同样被拒，事务整体回滚。
func TestTxMarkAppDeletedTransactional(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	app, err := seedAppE(t, st, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	// active 直达 deleted：拒绝（与 Store 形态同守卫）。
	if err := st.InTx(ctx, func(tx *Tx) error {
		return tx.MarkAppDeleted(ctx, app.ID)
	}); !errors.Is(err, ErrInvalidLifecycleTransition) {
		t.Fatalf("tx active→deleted must be rejected, got: %v", err)
	}
	// deleting → deleted：事务内成功，行迁移可见。
	if err := st.MarkAppDeleting(ctx, app.ID); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	if err := st.InTx(ctx, func(tx *Tx) error {
		if err := tx.MarkAppDeleted(ctx, app.ID); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, AuditEntry{
			Actor: "system", Action: "app.deleted", Target: "app:" + app.Name, Result: "ok",
		})
	}); err != nil {
		t.Fatalf("tx mark deleted: %v", err)
	}
	got, err := st.GetAppByName(ctx, "demo")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got.Lifecycle != LifecycleDeleted || got.DeletedAt.IsZero() {
		t.Fatalf("lifecycle = %s deleted_at zero=%v, want deleted with stamp", got.Lifecycle, got.DeletedAt.IsZero())
	}
	// 重复迁移（deleted → deleted）被拒：收敛步重放的幂等跳过依据。
	if err := st.InTx(ctx, func(tx *Tx) error {
		return tx.MarkAppDeleted(ctx, app.ID)
	}); !errors.Is(err, ErrInvalidLifecycleTransition) {
		t.Fatalf("tx deleted→deleted must be rejected, got: %v", err)
	}
}

// TestGetAppByNameQualifiedForm 三段限定形的精确解析（2026-09-26 staging
// 修复回归：跨项目同名 app〔「prod 与 dev 各有 demo」设计能力〕下，裸名
// 查询显性歧义、限定形逐行精确命中——按裸名重解析的引擎/日志共享面凭
// GetAppByName 的限定形分支免疫歧义）。
func TestGetAppByNameQualifiedForm(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	a := seedApp(t, st, "demo")
	b := seedApp(t, st, "demo")

	// 裸名：跨项目重名显性歧义（不静默取任意行）。
	if _, err := st.GetAppByName(ctx, "demo"); !errors.Is(err, ErrAppAmbiguous) {
		t.Fatalf("bare name over duplicate rows = %v, want ErrAppAmbiguous", err)
	}
	// 限定形：各自精确命中（回读归属 slug 与行 id 一致）。
	for _, want := range []App{a, b} {
		got, err := st.GetAppByName(ctx, want.QualifiedName())
		if err != nil {
			t.Fatalf("qualified %q: %v", want.QualifiedName(), err)
		}
		if got.ID != want.ID {
			t.Fatalf("qualified %q resolved id = %s, want %s", want.QualifiedName(), got.ID, want.ID)
		}
		if got.QualifiedName() != want.QualifiedName() {
			t.Fatalf("qualified roundtrip = %q, want %q", got.QualifiedName(), want.QualifiedName())
		}
	}
	// 限定形指向不存在的 app：ErrAppNotFound（不落歧义面）。
	if _, err := st.GetAppByName(ctx, a.TeamSlug+"/"+a.ProjectSlug+"/ghost"); !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("unknown qualified = %v, want ErrAppNotFound", err)
	}
}
