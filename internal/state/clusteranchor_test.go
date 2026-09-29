package state

// 锚定循环（multi-node §2.7/D-MN-8，E1-6）单测：worker 身份铸造（label
// + ref + 审计 + node.joined）、从 label 反建 ref（L1/L2 恢复等序）、冲突
// 不自动消解（审计 + 警示，映射与卷不动）、node.* 差分事件（joined/down/
// up/removed/availability_changed 逐转移）。node.joined/up 等事件名为注册
// 表既有预留，本票接线发出——usage 豁免清单同步收窄。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// mustSnapshot 从 fake 底座取当前全量快照（观测拍同源入口）。
func mustSnapshot(t *testing.T, fd *fakeDocker) []SubstrateNode {
	t.Helper()
	nodes, err := fd.ListNodeObservations(context.Background())
	if err != nil {
		t.Fatalf("list node observations: %v", err)
	}
	return nodes
}

// auditHasAction 报告审计日志是否已有指定 action 的记录。
func auditHasAction(t *testing.T, st *Store, action string) bool {
	t.Helper()
	rows, err := st.RecentAudits(context.Background(), 100)
	if err != nil {
		t.Fatalf("read audits: %v", err)
	}
	for _, r := range rows {
		if r.Action == action {
			return true
		}
	}
	return false
}

// containsAll 报告 s 是否包含全部子串。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// toCached 把底座快照折算为缓存行形态（差分基线的测试构造器——生产路径
// 由 Observer 在 SyncNodeObservations 前直读缓存表取得）。
func toCached(nodes []SubstrateNode) []CachedNode {
	out := make([]CachedNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, CachedNode{
			SwarmNodeID: n.SwarmNodeID, Hostname: n.Hostname,
			State: n.State, Availability: n.Availability, Labels: n.Labels,
		})
	}
	return out
}

// newAnchorHarness 构造锚定循环测试环境（真实 store + fakeDocker）。
func newAnchorHarness(t *testing.T) (*ClusterAnchor, *Store, *fakeDocker) {
	t.Helper()
	st := newTestStore(t)
	fd := newFakeDocker()
	anchor := NewClusterAnchor(st, fd, testLogger())
	return anchor, st, fd
}

// seedSelf 让 fakeDocker 自省返回 manager 节点（本机 = 既有锚定路径）。
func seedSelf(fd *fakeDocker, swarmID string) {
	fd.addNode(swarmID, "manager-01", "ready", 1)
	fd.labels[swarmID] = map[string]string{LabelNodeID: "n_self0000000000000000000"}
	fd.setSelf(swarmID, nil)
}

// eventsByName 取事件表中指定名的全部 payload（差分断言用）。
func eventsByName(t *testing.T, st *Store, name string) []Event {
	t.Helper()
	evs, err := st.EventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var out []Event
	for _, e := range evs {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// setSnapshotLabel 把 label 写进快照节点的 Labels（fake 的 label 记账分
// 两处：nodes[id].Labels 是快照可见面，labels[id] 是 UpdateNodeLabel 写入面
// ——真实底座两者同源，fake 分离以分别断言读写）。
func setSnapshotLabel(fd *fakeDocker, swarmID, value string) {
	n := fd.nodes[swarmID]
	n.Labels = map[string]string{LabelNodeID: value}
	fd.nodes[swarmID] = n
}

// TestAnchorMintsWorkerIdentity：无 label 的 worker → 铸造 n_<ULID> →
// label 写入（fake 收到）→ ref 登记 → identity_created/anchored 审计；
// 本机节点不被收编（沿用既有锚定）。
func TestAnchorMintsWorkerIdentity(t *testing.T) {
	anchor, st, fd := newAnchorHarness(t)
	ctx := context.Background()
	seedSelf(fd, "swarm-self")
	fd.addNode("swarm-w1", "worker-01", "ready", 5)
	next := mustSnapshot(t, fd)

	res, err := anchor.Reconcile(ctx, nil, next)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Minted) != 1 || res.Minted[0] != "swarm-w1" {
		t.Fatalf("minted = %v, want [swarm-w1]", res.Minted)
	}
	// label 已写入底座（fake 记账），值为 n_<ULID> 形态。
	got := fd.labels["swarm-w1"][LabelNodeID]
	if len(got) != len(NodeIDPrefix)+26 {
		t.Fatalf("minted label = %q, want n_<ULID> shape", got)
	}
	// ref 已登记（label 值 → swarm node ID）。
	ref, err := st.GetRuntimeNodeRef(ctx, got)
	if err != nil || ref.SwarmNodeID != "swarm-w1" {
		t.Fatalf("ref = %+v err = %v, want swarm-w1", ref, err)
	}
	// 审计两拍（created + anchored）。
	for _, action := range []string{"node.identity_created", "node.identity_anchored"} {
		if !auditHasAction(t, st, action) {
			t.Fatalf("audit %s missing", action)
		}
	}
	// 本机节点未收编：refs 只有 worker 一行（self 的映射属 NodeIdentity）。
	if _, err := st.GetRuntimeNodeRefBySwarmID(ctx, "swarm-self"); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("self node must not be adopted by cluster anchor, err = %v", err)
	}
}

// TestAnchorRebuildsRefFromLabel：label 已存在而 ref 缺失（L1/L2 恢复后
// SQLite ref 可整表重建）→ 以 label 值登记 ref + identity_anchored 审计。
func TestAnchorRebuildsRefFromLabel(t *testing.T) {
	anchor, st, fd := newAnchorHarness(t)
	ctx := context.Background()
	seedSelf(fd, "swarm-self")
	fd.addNode("swarm-w1", "worker-01", "ready", 3)
	setSnapshotLabel(fd, "swarm-w1", "n_worker00000000000000000")
	next := mustSnapshot(t, fd)

	res, err := anchor.Reconcile(ctx, nil, next)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Rebuilt) != 1 || res.Rebuilt[0] != "n_worker00000000000000000" {
		t.Fatalf("rebuilt = %v", res.Rebuilt)
	}
	if _, err := st.GetRuntimeNodeRef(ctx, "n_worker00000000000000000"); err != nil {
		t.Fatalf("ref rebuild missing: %v", err)
	}
	if !auditHasAction(t, st, "node.identity_anchored") {
		t.Fatal("audit node.identity_anchored missing")
	}
	// 二次拍幂等：ref 已在 → 无新账目。
	res2, err := anchor.Reconcile(ctx, toCached(next), next)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if len(res2.Rebuilt) != 0 || len(res2.Minted) != 0 || len(res2.Conflicts) != 0 {
		t.Fatalf("second beat must be a no-op, got %+v", res2)
	}
}

// TestAnchorConflictNotAutoResolved：① label 值撞已有映射（同平台 ID 已
// 映射到另一 swarm 节点）；② label 缺失但该 swarm 节点已有映射（label 被
// 外部删改）——两者都披露冲突（identity_conflict 审计）且不动现有映射。
func TestAnchorConflictNotAutoResolved(t *testing.T) {
	anchor, st, fd := newAnchorHarness(t)
	ctx := context.Background()
	seedSelf(fd, "swarm-self")

	// ① label 撞已有映射：n_dup 已映射到 swarm-a，新节点 swarm-b 带同 label。
	if _, err := st.UpsertRuntimeNodeRef(ctx, "n_dup00000000000000000000", "swarm-a", time.Now().UTC()); err != nil {
		t.Fatalf("seed ref: %v", err)
	}
	fd.addNode("swarm-b", "worker-b", "ready", 2)
	setSnapshotLabel(fd, "swarm-b", "n_dup00000000000000000000")

	res, err := anchor.Reconcile(ctx, nil, mustSnapshot(t, fd))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want 1 disclosure", res.Conflicts)
	}
	// 现有映射不被改写（不自动消解）。
	ref, err := st.GetRuntimeNodeRef(ctx, "n_dup00000000000000000000")
	if err != nil || ref.SwarmNodeID != "swarm-a" {
		t.Fatalf("existing ref changed: %+v err = %v", ref, err)
	}
	if !auditHasAction(t, st, "node.identity_conflict") {
		t.Fatal("conflict audit missing")
	}

	// ② label 缺失但映射尚在：swarm-c 已映射 n_old，label 被删。
	if _, err := st.UpsertRuntimeNodeRef(ctx, "n_old0000000000000000000", "swarm-c", time.Now().UTC()); err != nil {
		t.Fatalf("seed ref: %v", err)
	}
	fd.addNode("swarm-c", "worker-c", "ready", 2)
	res2, err := anchor.Reconcile(ctx, nil, mustSnapshot(t, fd))
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	// 冲突逐拍披露（诚实：持续性冲突每拍都可见）：swarm-b 的 ① 冲突仍在
	// 快照中 → 再次披露；swarm-c 的 ② 冲突新披露；两者都不产生铸造。
	if len(res2.Minted) != 0 {
		t.Fatalf("conflicted beats must not mint, got %v", res2.Minted)
	}
	if len(res2.Conflicts) != 2 ||
		!containsAll(res2.Conflicts[0]+res2.Conflicts[1], "swarm-b", "swarm-c") {
		t.Fatalf("conflicts = %v, want swarm-b (re-disclosed) + swarm-c (missing label with ref)", res2.Conflicts)
	}
	if ref, err := st.GetRuntimeNodeRef(ctx, "n_old0000000000000000000"); err != nil || ref.SwarmNodeID != "swarm-c" {
		t.Fatalf("existing ref for swarm-c changed: %+v err = %v", ref, err)
	}
}

// TestNodeEventDiffTransitions：prev→next 差分——新现 = node.joined、
// ready→down = node.down、down→ready = node.up、active→drain =
// availability_changed（载荷 old/new）、消失 = node.removed。
func TestNodeEventDiffTransitions(t *testing.T) {
	anchor, st, fd := newAnchorHarness(t)
	ctx := context.Background()
	seedSelf(fd, "swarm-self")

	// 第一拍：self + worker-a 出现 → 两条 joined。
	fd.addNode("swarm-w1", "worker-01", "ready", 1)
	first := mustSnapshot(t, fd)
	if _, err := anchor.Reconcile(ctx, nil, first); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if got := eventsByName(t, st, "node.joined"); len(got) != 2 {
		t.Fatalf("joined events = %d, want 2 (self + worker)", len(got))
	}

	// 第二拍：worker-a ready→down 且 active→drain；worker-b 上线；无移除。
	fd.nodes["swarm-w1"] = SubstrateNode{
		SwarmNodeID: "swarm-w1", Hostname: "worker-01", State: "down",
		Availability: "drain", Version: ObjectVersion{Index: 2}, Labels: fd.nodes["swarm-w1"].Labels,
	}
	fd.addNode("swarm-w2", "worker-02", "ready", 1)
	second := mustSnapshot(t, fd)
	res, err := anchor.Reconcile(ctx, toCached(first), second)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res.Down != 1 || res.AvailabilityChanged != 1 || res.Joined != 1 || res.Up != 0 {
		t.Fatalf("second beat counts = %+v, want down=1 availability=1 joined=1", res)
	}

	// 第三拍：worker-a 恢复（down→ready、drain→active）、worker-b 移除。
	fd.nodes["swarm-w1"] = SubstrateNode{
		SwarmNodeID: "swarm-w1", Hostname: "worker-01", State: "ready",
		Availability: "active", Version: ObjectVersion{Index: 3}, Labels: fd.nodes["swarm-w1"].Labels,
	}
	fd.removeNode("swarm-w2")
	third := mustSnapshot(t, fd)
	res3, err := anchor.Reconcile(ctx, toCached(second), third)
	if err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if res3.Up != 1 || res3.AvailabilityChanged != 1 || res3.Removed != 1 {
		t.Fatalf("third beat counts = %+v, want up=1 availability=1 removed=1", res3)
	}

	// 载荷断言：availability_changed 携带 old/new（drain 维护窗口叙事）。
	upDown := eventsByName(t, st, "node.availability_changed")
	if len(upDown) != 2 {
		t.Fatalf("availability events = %d, want 2", len(upDown))
	}
	last := upDown[len(upDown)-1]
	if last.Payload == "" || !containsAll(last.Payload, `"old":"drain"`, `"new":"active"`) {
		t.Fatalf("availability payload = %s, want old/new", last.Payload)
	}
	// node.down 的文案纪律：subject/载荷均指 worker（Swarm 失联判定语义）。
	downs := eventsByName(t, st, "node.down")
	if len(downs) != 1 || !containsAll(downs[0].Payload, `"swarm_node_id":"swarm-w1"`) {
		t.Fatalf("down events = %+v", downs)
	}
}

// TestPostSyncSwallowsErrors：挂钩语义——底座故障（SelfNodeID 失败之外的
// 底座错误路径）只日志、不向观测同步传播。这里以 label 写失败模拟：铸造
// 路径 ResolveObjectVersion 报错 → Reconcile 出错 → PostSync 不 panic、
// 不返回（观测拍继续成功）。
func TestPostSyncSwallowsErrors(t *testing.T) {
	anchor, _, fd := newAnchorHarness(t)
	seedSelf(fd, "swarm-self")
	fd.addNode("swarm-w1", "worker-01", "ready", 1)
	// 移除版本令牌：ResolveObjectVersion → ErrObjectNotFound（铸造失败）。
	fd.versions["node:swarm-w1"] = 0
	delete(fd.versions, "node:swarm-w1")

	// 不得 panic（观测同步成功语义不受挂钩失败影响）。
	anchor.PostSync(context.Background(), nil, mustSnapshot(t, fd))
}
