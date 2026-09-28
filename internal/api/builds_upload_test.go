package api

// BuildFromUpload（T 线 DT-6 / IMPL-T2-2）API 回归：四条守卫逐条落测试——
//   ①超限（大小/时长）fail-closed；②产物 digest 钉定引用返回（digest→ref
//   读通道可查）；③并发上限生效（同一队列 semaphore，无旁路）；④上下文
//   临时会话零残留（成功/失败/拒绝三态）。
// 另覆盖：scope 门（独立 build）、流协议违约族、终态失败码保真。

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// uploadEnv 是上传构建 API 测试环境（真实 state + 真实队列 + 注入假执行器
// + bufconn 鉴权链；上传根为独立临时目录）。
type uploadEnv struct {
	st       *state.Store
	conn     *grpc.ClientConn
	root     string
	buildTok string
	readTok  string
	exec     build.Executor
}

func newUploadEnv(t *testing.T, maxBytes int64, concurrency int, timeout time.Duration, exec build.Executor) *uploadEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := filepath.Join(dir, "build-uploads")
	uploads := build.UploadConfig{Root: root, MaxBytes: maxBytes, MaxChunkBytes: 4 << 10}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := build.NewQueue(st, exec, concurrency, 20*time.Millisecond, timeout, logger).WithUploadsRoot(root)
	qctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = q.Run(qctx) }()

	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterBuildsServiceServer(srv, NewBuildsService(st, q, uploads))
	conn := serveBufconn(t, srv)
	return &uploadEnv{
		st:       st,
		conn:     conn,
		root:     root,
		buildTok: seedTokenPlain(t, st, ScopeBuild),
		readTok:  seedTokenPlain(t, st, ScopeRead),
		exec:     exec,
	}
}

// uploadTarBytes 构造上下文 tar（Dockerfile + 可选附加条目）。
func uploadTarBytes(t *testing.T, dockerfile string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	write("Dockerfile", dockerfile)
	for name, body := range extra {
		write(name, body)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// streamUpload 以标准协议形态上传（首帧 metadata + tar 分片）。
func streamUpload(ctx context.Context, conn *grpc.ClientConn, name, dockerfile string, tarBytes []byte, chunk int) (*serverv1.BuildFromUploadResponse, error) {
	client := serverv1.NewBuildsServiceClient(conn)
	stream, err := client.BuildFromUpload(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&serverv1.BuildFromUploadRequest{
		Payload: &serverv1.BuildFromUploadRequest_Metadata{
			Metadata: &serverv1.BuildFromUploadMetadata{Name: name, Dockerfile: dockerfile},
		},
	}); err != nil {
		return nil, err
	}
	for off := 0; off < len(tarBytes); off += chunk {
		end := off + chunk
		if end > len(tarBytes) {
			end = len(tarBytes)
		}
		if err := stream.Send(&serverv1.BuildFromUploadRequest{
			Payload: &serverv1.BuildFromUploadRequest_Chunk{Chunk: tarBytes[off:end]},
		}); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

// streamRaw 以原始帧序上传（协议违约用例）。
func streamRaw(ctx context.Context, conn *grpc.ClientConn, msgs ...*serverv1.BuildFromUploadRequest) (*serverv1.BuildFromUploadResponse, error) {
	client := serverv1.NewBuildsServiceClient(conn)
	stream, err := client.BuildFromUpload(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if err := stream.Send(m); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

// uploadErrCode 提取错误信封码（非信封错误致命）。
func uploadErrCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	ae, ok := apperr.FromError(err)
	if !ok {
		t.Fatalf("error is not an apperr envelope: %v", err)
	}
	return ae.Code()
}

// mustNoResidue 断言上传根零残留（守卫④；根不存在或无子项都算零）。
func mustNoResidue(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read upload root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("upload root not empty after terminal/reject: %v", names)
	}
}

// ── 假执行器 ────────────────────────────────────────────────────────────────

// uploadFinishExecutor 立即成功收敛（产物引用/摘要给定），并按 builder 契
// 约清理上传会话目录（终态清理钩子的替身）。
type uploadFinishExecutor struct {
	store  *state.Store
	root   string
	ref    string
	digest string
}

func (e *uploadFinishExecutor) cleanup(rec state.BuildRecord) {
	req, err := build.DecodeRequest(rec.Request)
	if err != nil || req.EphemeralDir == "" {
		return
	}
	_ = build.CleanupUploadDir(req.EphemeralDir, e.root)
}

func (e *uploadFinishExecutor) Execute(ctx context.Context, rec state.BuildRecord) (state.BuildRecord, error) {
	if err := e.store.FinishBuildSucceeded(ctx, rec.ID, e.ref, e.digest, "", ""); err != nil {
		return rec, err
	}
	e.cleanup(rec)
	return e.store.GetBuild(ctx, rec.ID)
}

// uploadFailExecutor 失败收敛（注册码给定）并按 builder 契约清理会话目录。
type uploadFailExecutor struct {
	store *state.Store
	root  string
	code  string
}

func (e *uploadFailExecutor) Execute(ctx context.Context, rec state.BuildRecord) (state.BuildRecord, error) {
	if err := e.store.FinishBuildFailed(ctx, rec.ID, e.code); err != nil {
		return rec, err
	}
	req, _ := build.DecodeRequest(rec.Request)
	if req.EphemeralDir != "" {
		_ = build.CleanupUploadDir(req.EphemeralDir, e.root)
	}
	return e.store.GetBuild(ctx, rec.ID)
}

// uploadTimeoutExecutor 悬停至 ctx 取消（per-build 超时预算耗尽形态——不
// 收敛终态，由队列兜底收敛，测试断言队列侧清理钩子）。
type uploadTimeoutExecutor struct{}

func (uploadTimeoutExecutor) Execute(ctx context.Context, rec state.BuildRecord) (state.BuildRecord, error) {
	<-ctx.Done()
	return rec, ctx.Err()
}

// uploadGateExecutor 由测试闸门放行的执行器（并发上限断言）。
type uploadGateExecutor struct {
	store       *state.Store
	root        string
	gate        chan struct{}
	started     chan struct{}
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

func (e *uploadGateExecutor) Execute(ctx context.Context, rec state.BuildRecord) (state.BuildRecord, error) {
	cur := e.inFlight.Add(1)
	for {
		max := e.maxInFlight.Load()
		if cur <= max || e.maxInFlight.CompareAndSwap(max, cur) {
			break
		}
	}
	defer e.inFlight.Add(-1)
	e.started <- struct{}{}
	select {
	case <-e.gate:
	case <-ctx.Done():
		return rec, ctx.Err()
	}
	req, _ := build.DecodeRequest(rec.Request)
	if req.EphemeralDir != "" {
		defer func() { _ = build.CleanupUploadDir(req.EphemeralDir, e.root) }()
	}
	if err := e.store.FinishBuildSucceeded(ctx, rec.ID, "fleetly-local/demo:demo-"+rec.ID, "sha256:"+strings.Repeat("b", 64), "", ""); err != nil {
		return rec, err
	}
	return e.store.GetBuild(ctx, rec.ID)
}

// ── 用例 ────────────────────────────────────────────────────────────────────

const uploadTestDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestBuildFromUploadReturnsDigestPinnedReference 守卫②：上传→构建→返回
// builds 行（digest 钉定引用）；注册链路落库、无 app 归属、request 契约
// （Dockerfile 入口 + ephemeral_dir）齐备；digest→ref 读通道可查。
func TestBuildFromUploadReturnsDigestPinnedReference(t *testing.T) {
	ref := "registry.example.test/apps/demo@" + uploadTestDigest
	exec := &uploadFinishExecutor{ref: ref, digest: uploadTestDigest}
	exec.store = nil // 由 env 装配后回填
	env := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root

	ctx := context.Background()
	tarBytes := uploadTarBytes(t, "FROM scratch\nCOPY app.js /app.js\n", map[string]string{"app.js": "x"})
	resp, err := streamUpload(authCtx(ctx, env.buildTok), env.conn, "demo", "Dockerfile", tarBytes, 1024)
	if err != nil {
		t.Fatalf("BuildFromUpload: %v", err)
	}
	rec := resp.GetBuild()
	if rec.GetId() == "" || rec.GetStatus() != "succeeded" {
		t.Fatalf("build view = %+v, want succeeded row with id", rec)
	}
	if rec.GetApp() != "" {
		t.Fatalf("upload build app = %q, want empty (app-less)", rec.GetApp())
	}
	if rec.GetImageRef() != ref || rec.GetImageDigest() != uploadTestDigest {
		t.Fatalf("image ref/digest = %q/%q, want %q/%q", rec.GetImageRef(), rec.GetImageDigest(), ref, uploadTestDigest)
	}
	// digest→ref 读通道（部署引用登记的消费面）。
	rows, err := env.st.FindBuildsByDigest(ctx, uploadTestDigest)
	if err != nil || len(rows) != 1 || rows[0].ID != rec.GetId() {
		t.Fatalf("FindBuildsByDigest = %+v err=%v, want the upload build row", rows, err)
	}
	row, err := env.st.GetBuild(ctx, rec.GetId())
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	req, err := build.DecodeRequest(row.Request)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.AppID != "" || req.Driver != state.DriverDockerfile || req.Dockerfile != "Dockerfile" {
		t.Fatalf("request = %+v, want app-less dockerfile build", req)
	}
	if req.EphemeralDir != filepath.Join(env.root, rec.GetId()) {
		t.Fatalf("ephemeral_dir = %q, want %q", req.EphemeralDir, filepath.Join(env.root, rec.GetId()))
	}
	if req.ContextDir != filepath.Join(env.root, rec.GetId(), "context") {
		t.Fatalf("context_dir = %q", req.ContextDir)
	}
	mustNoResidue(t, env.root)
}

// TestBuildFromUploadNestedDockerfileEntry Dockerfile 入口可指定（仓内相对
// 路径，含折叠段），request 落规范化相对路径。
func TestBuildFromUploadNestedDockerfileEntry(t *testing.T) {
	exec := &uploadFinishExecutor{ref: "fleetly-local/nested:nested-1", digest: uploadTestDigest}
	env := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct{ name, body string }{
		{"Dockerfile", "FROM scratch\n"},
		{"deploy/Dockerfile", "FROM scratch\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := streamUpload(authCtx(context.Background(), env.buildTok), env.conn, "nested", "./deploy/Dockerfile", buf.Bytes(), 512)
	if err != nil {
		t.Fatalf("BuildFromUpload: %v", err)
	}
	row, err := env.st.GetBuild(context.Background(), resp.GetBuild().GetId())
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	req, err := build.DecodeRequest(row.Request)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Dockerfile != "deploy/Dockerfile" {
		t.Fatalf("dockerfile = %q, want deploy/Dockerfile", req.Dockerfile)
	}
}

// TestBuildFromUploadRejectsOversizeContext 守卫①（大小）：超限流式拒绝
// （E_BUILD_UPLOAD_TOO_LARGE），不建行、零残留。
func TestBuildFromUploadRejectsOversizeContext(t *testing.T) {
	exec := &uploadFinishExecutor{ref: "unused", digest: uploadTestDigest}
	env := newUploadEnv(t, 2048, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root

	tarBytes := uploadTarBytes(t, "FROM scratch\n", map[string]string{"big.bin": strings.Repeat("z", 8192)})
	_, err := streamUpload(authCtx(context.Background(), env.buildTok), env.conn, "demo", "Dockerfile", tarBytes, 1024)
	if got := uploadErrCode(t, err); got != "E_BUILD_UPLOAD_TOO_LARGE" {
		t.Fatalf("oversize code = %q, want E_BUILD_UPLOAD_TOO_LARGE", got)
	}
	rows, lerr := env.st.ListNonTerminalBuilds(context.Background())
	if lerr != nil || len(rows) != 0 {
		t.Fatalf("oversize upload must not enqueue builds: rows=%d err=%v", len(rows), lerr)
	}
	mustNoResidue(t, env.root)
}

// TestBuildFromUploadRejectsInvalidContext 形态违约族：首帧无 metadata（空
// 流/先 chunk）、metadata 重复、空帧、越界 tar、Dockerfile 入口缺失——统一
// E_BUILD_UPLOAD_INVALID，不建行、零残留。
func TestBuildFromUploadRejectsInvalidContext(t *testing.T) {
	exec := &uploadFinishExecutor{ref: "unused", digest: uploadTestDigest}
	env := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root
	ctx := authCtx(context.Background(), env.buildTok)
	meta := &serverv1.BuildFromUploadRequest{Payload: &serverv1.BuildFromUploadRequest_Metadata{
		Metadata: &serverv1.BuildFromUploadMetadata{Name: "demo"},
	}}
	validTar := uploadTarBytes(t, "FROM scratch\n", nil)

	cases := []struct {
		name string
		msgs []*serverv1.BuildFromUploadRequest
	}{
		{name: "empty stream"},
		{name: "chunk before metadata", msgs: []*serverv1.BuildFromUploadRequest{
			{Payload: &serverv1.BuildFromUploadRequest_Chunk{Chunk: validTar}},
		}},
		{name: "repeated metadata", msgs: []*serverv1.BuildFromUploadRequest{
			meta, meta,
		}},
		{name: "empty chunk", msgs: []*serverv1.BuildFromUploadRequest{
			meta,
			{Payload: &serverv1.BuildFromUploadRequest_Chunk{Chunk: nil}},
		}},
		{name: "oversize chunk", msgs: []*serverv1.BuildFromUploadRequest{
			meta,
			{Payload: &serverv1.BuildFromUploadRequest_Chunk{Chunk: bytes.Repeat([]byte("a"), (4<<10)+1)}},
		}},
		{name: "tampered tar", msgs: []*serverv1.BuildFromUploadRequest{
			meta,
			{Payload: &serverv1.BuildFromUploadRequest_Chunk{Chunk: []byte("not a tar at all")}},
		}},
		{name: "missing dockerfile", msgs: []*serverv1.BuildFromUploadRequest{
			{Payload: &serverv1.BuildFromUploadRequest_Metadata{
				Metadata: &serverv1.BuildFromUploadMetadata{Name: "demo", Dockerfile: "deploy/Dockerfile"},
			}},
			{Payload: &serverv1.BuildFromUploadRequest_Chunk{Chunk: validTar}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := streamRaw(ctx, env.conn, tc.msgs...)
			if got := uploadErrCode(t, err); got != "E_BUILD_UPLOAD_INVALID" {
				t.Fatalf("code = %q, want E_BUILD_UPLOAD_INVALID", got)
			}
		})
	}
	rows, err := env.st.ListNonTerminalBuilds(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid uploads must not enqueue builds: rows=%d err=%v", len(rows), err)
	}
	mustNoResidue(t, env.root)
}

// TestBuildFromUploadTimeoutFailsClosed 守卫①（时长）：构建执行超时（与 git
// 构建同一队列预算）→ 终态 failed（E_BUILD_FAILED 信封 + build_id 上下文），
// 队列侧清理钩子回收上下文（守卫④失败腿）。
func TestBuildFromUploadTimeoutFailsClosed(t *testing.T) {
	env := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, 150*time.Millisecond, uploadTimeoutExecutor{})

	tarBytes := uploadTarBytes(t, "FROM scratch\n", nil)
	_, err := streamUpload(authCtx(context.Background(), env.buildTok), env.conn, "demo", "Dockerfile", tarBytes, 512)
	ae, ok := apperr.FromError(err)
	if !ok {
		t.Fatalf("timeout error is not an envelope: %v", err)
	}
	if ae.Code() != "E_BUILD_FAILED" {
		t.Fatalf("timeout code = %q, want E_BUILD_FAILED (timeout lands in the shared build failure face)", ae.Code())
	}
	if ae.Context()["build_id"] == "" {
		t.Fatalf("timeout envelope must name the build id: %+v", ae.Context())
	}
	row, gerr := env.st.GetBuild(context.Background(), ae.Context()["build_id"])
	if gerr != nil {
		t.Fatalf("GetBuild after timeout: %v", gerr)
	}
	if row.Status != state.BuildFailed || row.ErrorCode != "E_BUILD_FAILED" {
		t.Fatalf("timed-out row = %s/%s, want failed/E_BUILD_FAILED", row.Status, row.ErrorCode)
	}
	mustNoResidue(t, env.root)
}

// TestBuildFromUploadFailureKeepsRegistryCode 失败码保真：推送类失败终态以
// 注册码 E_REGISTRY_PUSH_FAILED 回给调用方（不被 E_BUILD_FAILED 覆盖）。
func TestBuildFromUploadFailureKeepsRegistryCode(t *testing.T) {
	exec := &uploadFailExecutor{code: "E_REGISTRY_PUSH_FAILED"}
	env := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root

	tarBytes := uploadTarBytes(t, "FROM scratch\n", nil)
	_, err := streamUpload(authCtx(context.Background(), env.buildTok), env.conn, "demo", "Dockerfile", tarBytes, 512)
	if got := uploadErrCode(t, err); got != "E_REGISTRY_PUSH_FAILED" {
		t.Fatalf("failure code = %q, want E_REGISTRY_PUSH_FAILED", got)
	}
	mustNoResidue(t, env.root)
}

// TestBuildFromUploadRespectsQueueConcurrencyCap 守卫③：上传构建与 git 构建
// 共用同一队列 semaphore——并发 1 下第二条排队等待，在途峰值恒 ≤1（无旁路
// 第二执行通道）。
func TestBuildFromUploadRespectsQueueConcurrencyCap(t *testing.T) {
	exec := &uploadGateExecutor{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	env := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root

	ctx := context.Background()
	tarBytes := uploadTarBytes(t, "FROM scratch\n", nil)
	type result struct {
		resp *serverv1.BuildFromUploadResponse
		err  error
	}
	aCh := make(chan result, 1)
	bCh := make(chan result, 1)
	go func() {
		resp, err := streamUpload(authCtx(ctx, env.buildTok), env.conn, "first", "Dockerfile", tarBytes, 512)
		aCh <- result{resp, err}
	}()
	select {
	case <-exec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first upload never reached the executor")
	}
	go func() {
		resp, err := streamUpload(authCtx(ctx, env.buildTok), env.conn, "second", "Dockerfile", tarBytes, 512)
		bCh <- result{resp, err}
	}()
	// 第一执行器未放行期内：第二条不得开始执行（semaphore 满，排队可见）。
	select {
	case <-exec.started:
		t.Fatal("second upload executed while the concurrency slot was occupied")
	case <-time.After(500 * time.Millisecond):
	}
	if got := exec.maxInFlight.Load(); got != 1 {
		t.Fatalf("max in-flight = %d, want 1 (queue cap)", got)
	}
	close(exec.gate)
	for i, ch := range []chan result{aCh, bCh} {
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("upload %d: %v", i+1, r.err)
			}
			if r.resp.GetBuild().GetStatus() != "succeeded" {
				t.Fatalf("upload %d status = %s", i+1, r.resp.GetBuild().GetStatus())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("upload %d did not finish", i+1)
		}
	}
	if got := exec.maxInFlight.Load(); got != 1 {
		t.Fatalf("max in-flight = %d after both, want 1", got)
	}
	mustNoResidue(t, env.root)
}

// TestBuildFromUploadLeavesNoContextResidue 守卫④（显式）：成功/失败/拒绝
// 三态走完，上传根零条目。
func TestBuildFromUploadLeavesNoContextResidue(t *testing.T) {
	exec := &uploadFinishExecutor{ref: "fleetly-local/residue:residue-1", digest: uploadTestDigest}
	env := newUploadEnv(t, 2048, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root
	ctx := authCtx(context.Background(), env.buildTok)

	// 成功态。
	if _, err := streamUpload(ctx, env.conn, "ok", "Dockerfile", uploadTarBytes(t, "FROM scratch\n", nil), 256); err != nil {
		t.Fatalf("success upload: %v", err)
	}
	mustNoResidue(t, env.root)

	// 拒绝态（超限）。
	if _, err := streamUpload(ctx, env.conn, "big", "Dockerfile", uploadTarBytes(t, "FROM scratch\n", map[string]string{"big": strings.Repeat("q", 4096)}), 256); err == nil {
		t.Fatal("oversize upload succeeded")
	}
	mustNoResidue(t, env.root)

	// 失败态（执行器失败收敛并清理）。
	failExec := &uploadFailExecutor{code: "E_BUILD_FAILED"}
	failEnv := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, time.Minute, failExec)
	failExec.store = failEnv.st
	failExec.root = failEnv.root
	if _, err := streamUpload(authCtx(context.Background(), failEnv.buildTok), failEnv.conn, "fail", "Dockerfile", uploadTarBytes(t, "FROM scratch\n", nil), 256); err == nil {
		t.Fatal("failing upload returned success")
	}
	mustNoResidue(t, failEnv.root)
}

// TestBuildFromUploadScopeGate scope 门：BuildFromUpload 登记独立 build
// scope（read/deploy 不蕴含、admin 蕴含）；read 凭据拒绝（403）。
func TestBuildFromUploadScopeGate(t *testing.T) {
	method := "/fleetly.server.v1.BuildsService/BuildFromUpload"
	if got, ok := RequiredScope(method); !ok || got != ScopeBuild {
		t.Fatalf("RequiredScope(%s) = %q,%v; want %q,true", method, got, ok, ScopeBuild)
	}
	if containsScope(ScopeRead, ScopeBuild) || containsScope(ScopeDeploy, ScopeBuild) {
		t.Fatal("read/deploy must not imply build")
	}
	if !containsScope(ScopeAdmin, ScopeBuild) {
		t.Fatal("admin must imply build")
	}
	if !containsScope(ScopeBuild, ScopeBuild) {
		t.Fatal("build scope must satisfy itself")
	}

	exec := &uploadFinishExecutor{ref: "unused", digest: uploadTestDigest}
	env := newUploadEnv(t, build.DefaultMaxUploadBytes, 1, time.Minute, exec)
	exec.store = env.st
	exec.root = env.root
	_, err := streamUpload(authCtx(context.Background(), env.readTok), env.conn, "demo", "Dockerfile", uploadTarBytes(t, "FROM scratch\n", nil), 256)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("read token BuildFromUpload code = %v, want PermissionDenied", status.Code(err))
	}
	mustNoResidue(t, env.root)
}

// TestBuildFromUploadUnavailableWithoutAssembly 未装配形态如实报不可用
// （上传根空 / 队列缺位——不静默退化）。
func TestBuildFromUploadUnavailableWithoutAssembly(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tok := seedTokenPlain(t, st, ScopeBuild)
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterBuildsServiceServer(srv, NewBuildsService(st, nil, build.UploadConfig{}))
	conn := serveBufconn(t, srv)

	_, err = streamUpload(authCtx(context.Background(), tok), conn, "demo", "Dockerfile", uploadTarBytes(t, "FROM scratch\n", nil), 256)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("unassembled BuildFromUpload code = %v, want Unavailable (err=%v)", status.Code(err), err)
	}
}

// 编译期断言：夹具满足 build.Executor。
var _ build.Executor = (*uploadFinishExecutor)(nil)
var _ build.Executor = (*uploadGateExecutor)(nil)
var _ build.Executor = (*uploadFailExecutor)(nil)
var _ build.Executor = (*uploadTimeoutExecutor)(nil)
