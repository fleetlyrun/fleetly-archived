//go:build manual

package api

// IMPL-T2-2/DT-6 本机真机端到端探针（默认不跑）：
//
//	FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/api -run TestManualBuildFromUpload -v
//
// 覆盖面（原始输出见实施记录）：
//  1. SDK client-streaming 上传 → 真实 build.Builder（自管 buildkitd 容器，
//     dockerfile.v0 前端）solve → 本机装载 → builds 行（本地模式引用 +
//     本机不可变 digest）；
//  2. 守卫②的引用链真机腿：产物引用 `ref@digest` 经 ImageDigest 解析腿
//     （CreateTask/部署共用）直通，并以 digest 钉定引用真实创建 swarm
//     service 且拉起 running 任务（部署等价面）；
//  3. 守卫④真机腿：上传会话目录终态后零残留。

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
	fleetlysdk "github.com/fleetlyrun/fleetly/sdk/go/fleetly"
)

// manualUploadTar 构造真机构建上下文（alpine 基底 + 标记文件）。
func manualUploadTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries := []struct{ name, body string }{
		{"Dockerfile", "FROM alpine:3.21\nRUN echo built-from-upload > /marker.txt\n"},
		{"app.js", "console.log('manual upload probe')\n"},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatalf("tar body %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// endlessReader 是无限 'z' 字节流（超限用例：不整段物化内存）。
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'z'
	}
	return len(p), nil
}

// hugeTarStream 构造「声明超大条目 + 无限数据」的 tar 流：tar 头部是合法
// 结构（服务器先读头再流式累计内容，超限在累计点触发），数据面无限——
// 测试不物化整段字节。
func hugeTarStream(t *testing.T, declaredSize int64) io.Reader {
	t.Helper()
	var hdr bytes.Buffer
	tw := tar.NewWriter(&hdr)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: declaredSize, Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write huge tar header: %v", err)
	}
	raw := hdr.Bytes()
	if len(raw) == 0 {
		t.Fatal("tar header was not flushed to the buffer")
	}
	return io.MultiReader(bytes.NewReader(raw), endlessReader{})
}

func TestManualBuildFromUpload(t *testing.T) {
	if os.Getenv("FLEETLY_MANUAL_SWARM") != "1" {
		t.Skip("set FLEETLY_MANUAL_SWARM=1 to probe the local daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	sc, err := substrate.NewClient("")
	if err != nil {
		t.Fatalf("construct substrate client: %v", err)
	}
	defer func() { _ = sc.Close() }()
	if err := sc.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}

	dir := t.TempDir()
	st, err := state.Open(ctx, filepath.Join(dir, "manual-upload.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	root := filepath.Join(dir, "build-uploads")
	cfg := build.Config{
		CacheDir:     filepath.Join(dir, "cache"),
		ArtifactsDir: filepath.Join(dir, "artifacts"),
		UploadsRoot:  root,
		ManageDaemon: true,
	}.Normalize()
	builder := build.NewBuilder(cfg, st, sc, sc, logger)
	queue := build.NewQueue(st, builder, 1, 200*time.Millisecond, 10*time.Minute, logger).WithUploadsRoot(root)
	go func() { _ = queue.Run(ctx) }()

	// API 面 + SDK 消费链（与 CLI/集成同路径）。
	tok := seedTokenPlain(t, st, ScopeBuild)
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterBuildsServiceServer(srv, NewBuildsService(st, queue, cfg.UploadConfig()))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	sdk, err := fleetlysdk.NewClient(
		fleetlysdk.WithAddr("passthrough:///bufnet"),
		fleetlysdk.WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
		fleetlysdk.WithToken(tok),
	)
	if err != nil {
		t.Fatalf("sdk client: %v", err)
	}
	defer func() { _ = sdk.Close() }()

	started := time.Now()
	resp, err := sdk.BuildFromUpload(ctx, "manual-upload", "Dockerfile", bytes.NewReader(manualUploadTar(t)))
	if err != nil {
		t.Fatalf("BuildFromUpload: %v", err)
	}
	rec := resp.GetBuild()
	t.Logf("upload build: id=%s status=%s driver=%s ref=%s digest=%s took=%.1fs",
		rec.GetId(), rec.GetStatus(), rec.GetDriver(), rec.GetImageRef(), rec.GetImageDigest(), time.Since(started).Seconds())
	if rec.GetStatus() != "succeeded" || rec.GetImageDigest() == "" || rec.GetImageRef() == "" {
		t.Fatalf("build not succeeded with identity: %+v", rec)
	}

	// 守卫②（引用链真机腿）：digest 钉定引用经解析腿直通（CreateTask 与
	// 部署共用的 ImageDigest 入口）。
	pinned := rec.GetImageRef() + "@" + rec.GetImageDigest()
	digest, err := sc.ImageDigest(ctx, pinned)
	if err != nil {
		t.Fatalf("ImageDigest(%s): %v", pinned, err)
	}
	if digest != rec.GetImageDigest() {
		t.Fatalf("ImageDigest pass-through = %q, want %q", digest, rec.GetImageDigest())
	}
	t.Logf("digest pass-through: %s -> %s", pinned, digest)

	// 守卫②（部署等价面）：按引擎的引用裁决复现部署引用——本地模式构建
	// 产物无清单摘要（RepoDigests 空），ImageDigest 返回空串、引擎原样使用
	// tag；digest 直通腿在上方已证（registry 模式的生产形态即 `ref@digest`，
	// 本地无 zot 不可复现）。随后以该引用创建真实 swarm 服务并等待 running
	// 任务（镜像本机可得，零拉取）。
	deployRef := rec.GetImageRef()
	if localDigest, derr := sc.ImageDigest(ctx, rec.GetImageRef()); derr != nil {
		t.Fatalf("ImageDigest(%s): %v", rec.GetImageRef(), derr)
	} else if localDigest != "" {
		deployRef = rec.GetImageRef() + "@" + localDigest
	}
	t.Logf("deploy reference (engine semantics, local mode): %s", deployRef)

	svcName := "fleetly-manual-upload-probe"
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		_ = sc.ServiceRemove(cctx, svcName)
	})
	spec := engine.ServiceSpec{
		Name:            svcName,
		Image:           deployRef,
		Args:            []string{"sleep", "60"},
		Replicas:        1,
		RestartPolicy:   &engine.RestartPolicySpec{Condition: "none"},
		StopGracePeriod: 2 * time.Second,
		Resources:       &engine.ResourcesSpec{NanoCPUs: 250_000_000, MemoryBytes: 64 << 20},
	}
	if err := sc.ServiceCreate(ctx, spec); err != nil {
		t.Fatalf("create digest-pinned service: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	var taskState string
	for time.Now().Before(deadline) {
		tasks, terr := sc.TaskList(ctx, svcName)
		if terr != nil {
			t.Fatalf("task list: %v", terr)
		}
		for _, task := range tasks {
			taskState = task.State
			if task.State == "running" {
				t.Logf("digest-pinned service task running: id=%s image=%s state=%s", task.ID, task.Image, task.State)
				goto running
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("digest-pinned service never reached running (last task state %q)", taskState)
running:

	// 守卫④（真机腿）：上传会话零残留。
	entries, rerr := os.ReadDir(root)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatalf("read uploads root: %v", rerr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("upload session residue: %v", names)
	}
	t.Logf("upload session root empty after terminal: %s", root)

	// 超限真机腿（同一面）：声明超大条目的合法 tar 头 + 无限数据流被
	// fail-closed 拒绝（流式累计到上限即拒；测试不物化整段字节）。
	if _, oerr := sdk.BuildFromUpload(ctx, "manual-oversize", "Dockerfile", hugeTarStream(t, cfg.MaxUploadBytes+1)); oerr == nil {
		t.Fatal("oversize upload succeeded; expected E_BUILD_UPLOAD_TOO_LARGE")
	} else if ae, ok := apperr.FromError(oerr); !ok || ae.Code() != "E_BUILD_UPLOAD_TOO_LARGE" {
		t.Fatalf("oversize error = %v, want E_BUILD_UPLOAD_TOO_LARGE envelope", oerr)
	} else {
		t.Logf("oversize upload rejected: %v", oerr)
	}
	entries, rerr = os.ReadDir(root)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatalf("read uploads root after oversize: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("residue after oversize rejection: %d entries", len(entries))
	}
}
