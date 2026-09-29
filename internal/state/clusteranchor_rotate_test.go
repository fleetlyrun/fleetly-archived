package state

// auto-rotate 触发链（multi-node §2.3/D-MN-1，阶段5任务A）单测：三态语义
//   1. 新铸造锚定 → rotate 被调一次（异步等待断言 + 不阻塞观测拍）；
//   2. 无新锚定的稳态拍 → 零调用（不空转）；
//   3. manual 模式 → 零调用（显式 opt-out 关闭位）；
//   4. rotate 失败 → 不伤观测拍、不落审计、不重试（下一拍稳态零再触发）。
// 分层断言：轮换端口为 state 定义的 JoinTokenRotator（测试替身实现）——
// state 不依赖 substrate 的纪律由本文件即证（替身不需要底座）。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRotator 是 JoinTokenRotator 端口的测试替身：gate 关闭时调用阻塞
// （证明触发链不阻塞观测拍——PostSync 先返回、后放行）；记录每次调用的
// role；rotated 在成功调用记账后发信号（审计写完才发——信号即全链完成）。
type fakeRotator struct {
	mu      sync.Mutex
	gate    chan struct{}
	roles   []string
	err     error
	rotated chan struct{}
}

func newFakeRotator() *fakeRotator {
	return &fakeRotator{gate: make(chan struct{}), rotated: make(chan struct{}, 8)}
}

func (f *fakeRotator) SwarmRotateJoinToken(_ context.Context, role string) (string, error) {
	<-f.gate // 触发链必须异步：放行前阻塞即证明 PostSync 未被拖住
	f.mu.Lock()
	f.roles = append(f.roles, role)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	select {
	case f.rotated <- struct{}{}:
	default:
	}
	return "tok-rotated", nil
}

func (f *fakeRotator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.roles)
}

func (f *fakeRotator) release() { close(f.gate) }

// newRotateHarness 构造带轮换接线的锚定循环测试环境（真实 store +
// fakeDocker + fakeRotator）。
func newRotateHarness(t *testing.T, mode string, rotErr error) (*ClusterAnchor, *Store, *fakeDocker, *fakeRotator) {
	t.Helper()
	st := newTestStore(t)
	fd := newFakeDocker()
	fr := newFakeRotator()
	fr.err = rotErr
	anchor := NewClusterAnchor(st, fd, testLogger()).WithTokenRotate(mode, fr)
	return anchor, st, fd, fr
}

// waitRotate 等待一次成功轮换全链（rotate + 审计）完成。轮换成功信号
//（rotated）先于审计落库——生产触发链在 rotator 调用返回后才写审计
//（clusteranchor.go maybeRotateToken 的 goroutine 序），全量跑测试时的并行
// 负载会把该窗口放大成偶发红：此处轮询审计行至多 5s，等待「信号 + 审计」
// 两段都落地。
func waitRotate(t *testing.T, st *Store, fr *fakeRotator) {
	t.Helper()
	select {
	case <-fr.rotated:
	case <-time.After(5 * time.Second):
		t.Fatal("join token rotate did not complete within 5s")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !auditHasAction(t, st, "node.join_token_rotated") {
		if time.Now().After(deadline) {
			t.Fatal("audit node.join_token_rotated did not land within 5s after rotate")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAutoRotateFiresOnMintedAnchor：mode=auto + 新铸造 worker → rotate 恰
// 被调一次（role=worker），审计 node.join_token_rotated（actor=system）落
// 库；PostSync 在轮换放行前已返回（不阻塞观测拍）。
func TestAutoRotateFiresOnMintedAnchor(t *testing.T) {
	anchor, st, fd, fr := newRotateHarness(t, "auto", nil)
	ctx := context.Background()
	seedSelf(fd, "swarm-self")
	fd.addNode("swarm-w1", "worker-01", "ready", 5)
	next := mustSnapshot(t, fd)

	// PostSync 返回时轮换必然仍在 gate 后阻塞——能走到 release() 即证明
	// 观测拍未被轮换拖住（异步触发链的结构证明）。
	anchor.PostSync(ctx, nil, next)
	fr.release()
	waitRotate(t, st, fr)

	if got := fr.callCount(); got != 1 {
		t.Fatalf("rotate calls = %d, want 1", got)
	}
	// 审计：node.join_token_rotated，actor=system，trigger=auto（与人工
	// 路径 RotateJoinToken 的 actor=human 区分）。
	rows, err := st.RecentAudits(ctx, 100)
	if err != nil {
		t.Fatalf("read audits: %v", err)
	}
	for _, r := range rows {
		if r.Action == "node.join_token_rotated" {
			if r.Actor != "system" {
				t.Fatalf("audit actor = %q, want system", r.Actor)
			}
			return
		}
	}
	t.Fatal("audit node.join_token_rotated missing after successful auto-rotate")
}

// TestAutoRotateIdleWithoutNewAnchor：首拍铸造触发一次后，稳态拍（快照无
// 变化）零再触发——无新锚定不空转（D-MN-1 不重试风暴的另一面：稳态不轮换）。
func TestAutoRotateIdleWithoutNewAnchor(t *testing.T) {
	anchor, st2, fd, fr := newRotateHarness(t, "auto", nil)
	ctx := context.Background()
	seedSelf(fd, "swarm-self")
	fd.addNode("swarm-w1", "worker-01", "ready", 5)
	first := mustSnapshot(t, fd)

	anchor.PostSync(ctx, nil, first)
	fr.release()
	waitRotate(t, st2, fr)

	// 稳态第二拍：prev == next，无铸造 → 不触发。
	anchor.PostSync(ctx, toCached(first), first)
	select {
	case <-fr.rotated:
		t.Fatal("steady-state beat must not rotate")
	case <-time.After(300 * time.Millisecond):
	}
	if got := fr.callCount(); got != 1 {
		t.Fatalf("rotate calls after steady beat = %d, want 1", got)
	}
}

// TestManualModeNeverRotates：manual（与未知值）即使出现新铸造也零触发
// ——manual 是显式 opt-out（批量加节点场景，全部完成后人工 rotate-token）。
func TestManualModeNeverRotates(t *testing.T) {
	for _, mode := range []string{"manual", "unknown-value"} {
		t.Run(mode, func(t *testing.T) {
			anchor, _, fd, fr := newRotateHarness(t, mode, nil)
			seedSelf(fd, "swarm-self")
			fd.addNode("swarm-w1", "worker-01", "ready", 5)

			anchor.PostSync(context.Background(), nil, mustSnapshot(t, fd))
			// 铸造确已发生（锚定语义不受关闭位影响）——rotate 仍零调用。
			select {
			case <-fr.rotated:
				t.Fatalf("mode %s must not rotate", mode)
			case <-time.After(300 * time.Millisecond):
			}
			if got := fr.callCount(); got != 0 {
				t.Fatalf("mode %s rotate calls = %d, want 0", mode, got)
			}
		})
	}
}

// TestRotateFailureDoesNotHurtBeat：rotate 失败 → 观测拍照常完成（PostSync
// 无返回值可炸、Reconcile 账目不受影响）、不落 rotate 审计；稳态下一拍零
// 再触发（无重试风暴——下一拍有新锚定才会再武装）。
func TestRotateFailureDoesNotHurtBeat(t *testing.T) {
	anchor, st, fd, fr := newRotateHarness(t, "auto", errors.New("swarm update: boom"))
	ctx := context.Background()
	seedSelf(fd, "swarm-self")
	fd.addNode("swarm-w1", "worker-01", "ready", 5)
	first := mustSnapshot(t, fd)

	// 不得 panic；轮换被尝试（gate 后的失败路径跑完）。
	anchor.PostSync(ctx, nil, first)
	fr.release()
	// 失败不发 rotated 信号——以 callCount + 审计双面确认走到了失败分支。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && fr.callCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fr.callCount(); got != 1 {
		t.Fatalf("rotate attempts = %d, want 1", got)
	}
	// 铸造本体不受轮换失败影响（收编账目完整）。
	ref, err := st.GetRuntimeNodeRefBySwarmID(ctx, "swarm-w1")
	if err != nil || ref.PlatformID == "" {
		t.Fatalf("anchoring hurt by rotate failure: ref=%+v err=%v", ref, err)
	}
	if auditHasAction(t, st, "node.join_token_rotated") {
		t.Fatal("failed rotate must not write a success audit")
	}
	// 稳态下一拍：零再触发（无重试风暴）。
	anchor.PostSync(ctx, toCached(first), first)
	time.Sleep(300 * time.Millisecond)
	if got := fr.callCount(); got != 1 {
		t.Fatalf("rotate calls after steady beat = %d, want 1 (no retry storm)", got)
	}
}
