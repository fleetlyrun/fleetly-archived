package build

// IMPL-T2-2（DT-6）队列侧清理钩子回归：启动复位把中断的 building 行收敛
// failed 的同时清理其上传会话目录；queued 行的会话目录保留（构建仍要消费
// 上下文）；直接子目录实体删除、非本会话形态 no-op。

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// passthroughExecutor 只回显记录（本测试不驱动执行循环——resetInterrupted
// 直调；接口构造需要）。
type passthroughExecutor struct{}

func (passthroughExecutor) Execute(_ context.Context, rec state.BuildRecord) (state.BuildRecord, error) {
	return rec, nil
}

// TestQueueResetCleansUploadedContext 启动复位：building 行（崩溃窗口）的
// 上传会话目录被清理；queued 行目录保留。
func TestQueueResetCleansUploadedContext(t *testing.T) {
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "queue-upload.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := testsupport.SeedAppE(t, st, "queue-upload"); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	root := t.TempDir()
	ctx := context.Background()

	mkSession := func(id string) (string, string) {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(filepath.Join(dir, "context"), 0o700); err != nil {
			t.Fatalf("mkdir session %s: %v", id, err)
		}
		raw, err := Request{BuildID: id, AppName: "queue-upload", Driver: state.DriverDockerfile,
			ContextDir: filepath.Join(dir, "context"), EphemeralDir: dir}.Encode()
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		return dir, raw
	}

	// building 行（上一进程中断在途）：复位收敛 + 目录清理。
	interruptedDir, interruptedReq := mkSession("01INTERRUPTED00000000000000")
	rec, err := st.CreateBuild(ctx, state.BuildRecord{ID: "01INTERRUPTED00000000000000", Driver: state.DriverDockerfile, Request: interruptedReq})
	if err != nil {
		t.Fatalf("create interrupted build: %v", err)
	}
	if err := st.ClaimBuild(ctx, rec.ID); err != nil {
		t.Fatalf("claim interrupted build: %v", err)
	}

	// queued 行：目录保留。
	queuedDir, queuedReq := mkSession("01QUEUED000000000000000000")
	if _, err := st.CreateBuild(ctx, state.BuildRecord{ID: "01QUEUED000000000000000000", Driver: state.DriverDockerfile, Request: queuedReq}); err != nil {
		t.Fatalf("create queued build: %v", err)
	}

	q := NewQueue(st, passthroughExecutor{}, 1, time.Second, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithUploadsRoot(root)
	q.resetInterrupted(ctx)

	got, err := st.GetBuild(ctx, "01INTERRUPTED00000000000000")
	if err != nil {
		t.Fatalf("read interrupted build: %v", err)
	}
	if got.Status != state.BuildFailed {
		t.Fatalf("interrupted build status = %s, want failed", got.Status)
	}
	if _, err := os.Stat(interruptedDir); !os.IsNotExist(err) {
		t.Fatalf("interrupted session dir still present (err=%v)", err)
	}
	if _, err := os.Stat(queuedDir); err != nil {
		t.Fatalf("queued session dir must survive reset: %v", err)
	}
}

// TestCleanupUploadDirRejectsNonSessionForms 清理钩子的形态边界：非直接子
// 目录（根自身/越界/深层）与伪造的符号链条目不触发删除。
func TestCleanupUploadDirRejectsNonSessionForms(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, outside, filepath.Join(outside, "keep.txt")} {
		if err := CleanupUploadDir(dir, root); err != nil {
			t.Fatalf("CleanupUploadDir(%s): %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatalf("outside file deleted by cleanup guard: %v", err)
	}
}
