package state

// IMPL-T2-2（DT-6）上传构建面 state 回归：builds.app_id 可空（上传构建无
// app 归属；00027 表重建）的落库/回读/FK 语义，与 janitor 上传会话孤儿
// 目录清扫（终态/无行按 mtime 回收，在途行保留，读故障不结论）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBuildAppLessRoundtrip 上传构建行（AppID 空）落库为 NULL、回读为空串；
// 非空 AppID 的 FK 约束不回退（伪造引用显性失败）。
func TestBuildAppLessRoundtrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	rec, err := st.CreateBuild(ctx, BuildRecord{
		Service: "", Driver: DriverDockerfile, Request: `{"build_id":"upload-1"}`,
	})
	if err != nil {
		t.Fatalf("create app-less build: %v", err)
	}
	if rec.AppID != "" {
		t.Fatalf("created AppID = %q, want empty", rec.AppID)
	}
	got, err := st.GetBuild(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get app-less build: %v", err)
	}
	if got.AppID != "" {
		t.Fatalf("round-tripped AppID = %q, want empty (NULL)", got.AppID)
	}

	// 非空 AppID 仍受 FK 约束（00027 保留 REFERENCES apps）。
	if _, err := st.CreateBuild(ctx, BuildRecord{
		AppID: "no-such-app", Service: "web", Driver: DriverDockerfile,
	}); err == nil {
		t.Fatal("create build with unknown app_id succeeded; FK enforcement regressed")
	}
}

// TestJanitorPrunesOrphanUploadSessions 上传会话清扫：
//   - 无行对应的孤儿目录过窗回收（解包后建行前崩溃的兜底形态）；
//   - 在途行（queued/building）的目录恒保留（构建仍要消费上下文）；
//   - 终态行目录过窗回收、窗内保留。
func TestJanitorPrunesOrphanUploadSessions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	root := t.TempDir()

	mkDir := func(name string, at time.Time) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "context"), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		return dir
	}

	orphanOld := mkDir("01ORPHANOLD0000000000000000", old)
	orphanFresh := mkDir("01ORPHANFRESH00000000000000", now)

	queued := mkDir("01QUEUED000000000000000000", old)
	if _, err := st.CreateBuild(ctx, BuildRecord{
		ID: "01QUEUED000000000000000000", Driver: DriverDockerfile,
	}); err != nil {
		t.Fatalf("seed queued build: %v", err)
	}

	terminal := mkDir("01TERMINAL0000000000000000", old)
	terminalFresh := mkDir("01TERMINALFRESH0000000000", now)
	for _, id := range []string{"01TERMINAL0000000000000000", "01TERMINALFRESH0000000000"} {
		rec, err := st.CreateBuild(ctx, BuildRecord{ID: id, Driver: DriverDockerfile})
		if err != nil {
			t.Fatalf("seed terminal build %s: %v", id, err)
		}
		if err := st.ClaimBuild(ctx, rec.ID); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		if err := st.FinishBuildSucceeded(ctx, rec.ID, "ref", "sha256:aa", "", ""); err != nil {
			t.Fatalf("finish %s: %v", id, err)
		}
	}

	jr := NewJanitor(st, JanitorConfig{
		EventRetentionDays: DefaultEventRetentionDays,
		AuditRetentionDays: DefaultAuditRetentionDays,
		UploadsRoot:        root,
	}, testLogger())
	if _, _, err := jr.PruneOnce(ctx, now); err != nil {
		t.Fatalf("prune once: %v", err)
	}

	mustExist := func(dir string) {
		t.Helper()
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s should exist: %v", dir, err)
		}
	}
	mustGone := func(dir string) {
		t.Helper()
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone (err=%v)", dir, err)
		}
	}
	mustGone(orphanOld)      // 无行 + 过窗
	mustExist(orphanFresh)   // 无行 + 窗内
	mustExist(queued)        // 在途行：保留
	mustGone(terminal)       // 终态行 + 过窗
	mustExist(terminalFresh) // 终态行 + 窗内
}
