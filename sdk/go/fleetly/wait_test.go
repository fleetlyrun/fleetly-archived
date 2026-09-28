// WaitBuild / BuildTerminal 的直接测试（IMPL-ARCH-K）：跟随 client_test.go
// 的既有手法——进程内起最小 BuildsService gRPC 服务器（脚本化状态序列的
// stub），经真实 NewClient 客户端驱动等待环的全部分支（收敛/超时/错误/
// 取消/回调中止/空输入）。
package fleetly_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/sdk/go/fleetly"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubBuildsService 是脚本化构建服务 stub：每个 build id 挂一条状态序列，
// GetBuild 逐次消费、末态停留；failures 指定 id 的前 N 次问询强制返回
// NotFound（错误路径）；calls 记录问询次数（空输入零 RPC 的证据）。
type stubBuildsService struct {
	serverv1.UnimplementedBuildsServiceServer
	mu       sync.Mutex
	script   map[string][]string
	failures map[string]int
	calls    map[string]int
}

func (s *stubBuildsService) GetBuild(_ context.Context, req *serverv1.GetBuildRequest) (*serverv1.GetBuildResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := req.GetId()
	s.calls[id]++
	if n := s.failures[id]; n > 0 {
		s.failures[id] = n - 1
		return nil, status.Errorf(codes.NotFound, "build %s not found", id)
	}
	seq := s.script[id]
	if len(seq) == 0 {
		return nil, status.Errorf(codes.NotFound, "build %s not found", id)
	}
	st := seq[0]
	if len(seq) > 1 {
		s.script[id] = seq[1:]
	}
	return &serverv1.GetBuildResponse{Build: &serverv1.BuildView{
		Id: id, Service: "web", Status: st,
		ImageRef: "fleetly-local/web:v1", ImageDigest: "sha256:aa",
	}}, nil
}

// startWaitService 起 stub 构建服务并接好 SDK 客户端。
func startWaitService(t *testing.T, stub *stubBuildsService) *fleetly.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	serverv1.RegisterBuildsServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := fleetly.NewClient(fleetly.WithAddr(lis.Addr().String()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestBuildTerminalVocabulary 终态谓词与词表契约：queued/building 非终态，
// succeeded/failed 终态，词表外（含空串）按终态（未知状态暴露优先于挂死）。
func TestBuildTerminalVocabulary(t *testing.T) {
	cases := map[string]bool{
		"queued":     false,
		"building":   false,
		"succeeded":  true,
		"failed":     true,
		"":           true,
		"cancelled?": true,
	}
	for statusWord, want := range cases {
		if got := fleetly.BuildTerminal(statusWord); got != want {
			t.Errorf("BuildTerminal(%q) = %v, want %v", statusWord, got, want)
		}
	}
}

// TestWaitBuildCollectsTerminalViews 收敛主路：一构建已终态、一构建两拍后
// 终态——按完成序返回、回调同序触发；无超时上限（timeout=0 非限时形态）
// 时由收敛本身收口。
func TestWaitBuildCollectsTerminalViews(t *testing.T) {
	stub := &stubBuildsService{
		script: map[string][]string{
			"b-done": {"succeeded"},
			"b-late": {"queued", "building", "succeeded"},
		},
		calls: map[string]int{},
	}
	client := startWaitService(t, stub)

	var callbackOrder []string
	results, err := client.WaitBuild(context.Background(), []string{"b-late", "b-done"},
		fleetly.WithWaitTimeout(5*time.Second), // 安全网：断言失败而非挂死
		fleetly.WithWaitPollInterval(2*time.Millisecond),
		fleetly.WithWaitOnTerminal(func(rec *serverv1.BuildView) error {
			callbackOrder = append(callbackOrder, rec.GetId())
			return nil
		}))
	if err != nil {
		t.Fatalf("WaitBuild: %v", err)
	}
	if len(results) != 2 || results[0].GetId() != "b-done" || results[1].GetId() != "b-late" {
		t.Fatalf("results = %v, want completion order [b-done b-late]", results)
	}
	for _, rec := range results {
		if rec.GetStatus() != "succeeded" || rec.GetImageDigest() != "sha256:aa" {
			t.Fatalf("terminal row drifted: %+v", rec)
		}
	}
	if len(callbackOrder) != 2 || callbackOrder[0] != "b-done" || callbackOrder[1] != "b-late" {
		t.Fatalf("callback order = %v, want [b-done b-late]", callbackOrder)
	}
}

// TestWaitBuildTimeoutPendingSnapshot 超时主路：恒 queued → errors.Is 哨兵
// + errors.As 取回快照（Pending 状态、Timeout 上限）；部分结果为空。
func TestWaitBuildTimeoutPendingSnapshot(t *testing.T) {
	stub := &stubBuildsService{
		script: map[string][]string{"b-stuck": {"queued"}},
		calls:  map[string]int{},
	}
	client := startWaitService(t, stub)

	results, err := client.WaitBuild(context.Background(), []string{"b-stuck"},
		fleetly.WithWaitTimeout(40*time.Millisecond),
		fleetly.WithWaitPollInterval(5*time.Millisecond))
	if !errors.Is(err, fleetly.ErrWaitTimeout) {
		t.Fatalf("err = %v, want ErrWaitTimeout", err)
	}
	var timeoutErr *fleetly.WaitTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want *WaitTimeoutError via errors.As", err)
	}
	if timeoutErr.Timeout != 40*time.Millisecond {
		t.Fatalf("Timeout = %s, want 40ms", timeoutErr.Timeout)
	}
	if len(timeoutErr.Pending) != 1 || timeoutErr.Pending["b-stuck"] != "queued" {
		t.Fatalf("Pending = %v, want {b-stuck: queued}", timeoutErr.Pending)
	}
	if len(results) != 0 {
		t.Fatalf("results = %v, want empty", results)
	}
}

// TestWaitBuildTimeoutLastKnownStatuses 超时快照携带各构建最后已知状态
//（queued 与 building 混合——CLI「全部仍 queued vs 有 in-flight」分支判
// 决的输入面）。
func TestWaitBuildTimeoutLastKnownStatuses(t *testing.T) {
	stub := &stubBuildsService{
		script: map[string][]string{
			"b-queued":   {"queued"},
			"b-building": {"building"},
		},
		calls: map[string]int{},
	}
	client := startWaitService(t, stub)

	_, err := client.WaitBuild(context.Background(), []string{"b-queued", "b-building"},
		fleetly.WithWaitTimeout(40*time.Millisecond),
		fleetly.WithWaitPollInterval(5*time.Millisecond))
	var timeoutErr *fleetly.WaitTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want *WaitTimeoutError", err)
	}
	if timeoutErr.Pending["b-queued"] != "queued" || timeoutErr.Pending["b-building"] != "building" {
		t.Fatalf("Pending = %v, want last-known statuses per build", timeoutErr.Pending)
	}
}

// TestWaitBuildAbortsOnErrorBeforeTerminal 错误语义：终态前 GetBuild 报错
// 即中止（NotFound 原样上抛、非超时形态）；已到终态的另一构建不被反噬。
// 同轮多构建的问询序是 map 序——部分结果可能含或不含先行终态者，两者皆合
// 法（与收编前 CLI 环同性质）。两场景各用全新 stub（failures 计数不跨等
// 待共享，避免首个场景消费掉注入的失败）。
func TestWaitBuildAbortsOnErrorBeforeTerminal(t *testing.T) {
	client := startWaitService(t, &stubBuildsService{
		script:   map[string][]string{"b-alone": {"queued"}},
		failures: map[string]int{"b-alone": 1},
		calls:    map[string]int{},
	})
	results, err := client.WaitBuild(context.Background(), []string{"b-alone"},
		fleetly.WithWaitTimeout(5*time.Second),
		fleetly.WithWaitPollInterval(5*time.Millisecond))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want raw NotFound", err)
	}
	if errors.Is(err, fleetly.ErrWaitTimeout) {
		t.Fatalf("pre-terminal error must not surface as timeout: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %v, want empty", results)
	}

	client = startWaitService(t, &stubBuildsService{
		script: map[string][]string{
			"b-done": {"succeeded"},
			"b-gone": {"queued"},
		},
		failures: map[string]int{"b-gone": 1},
		calls:    map[string]int{},
	})
	results, err = client.WaitBuild(context.Background(), []string{"b-done", "b-gone"},
		fleetly.WithWaitTimeout(5*time.Second),
		fleetly.WithWaitPollInterval(5*time.Millisecond))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want raw NotFound", err)
	}
	if len(results) > 1 || (len(results) == 1 && results[0].GetId() != "b-done") {
		t.Fatalf("partial results = %v, want at most the terminal b-done row", results)
	}
}

// TestWaitBuildReturnsContextError ctx 结束：返回部分结果与 ctx.Err() 本尊
//（不包裹——调用方 errors.Is 判定取消/超时形态的通路；WithTimeout 的
// ctx.Err() 即 DeadlineExceeded）。
func TestWaitBuildReturnsContextError(t *testing.T) {
	stub := &stubBuildsService{
		script: map[string][]string{"b-stuck": {"queued"}},
		calls:  map[string]int{},
	}
	client := startWaitService(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	results, err := client.WaitBuild(ctx, []string{"b-stuck"},
		fleetly.WithWaitPollInterval(250*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded unwrapped", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %v, want empty", results)
	}
}

// TestWaitBuildEmptyBuildIDs 空输入零 RPC。
func TestWaitBuildEmptyBuildIDs(t *testing.T) {
	stub := &stubBuildsService{script: map[string][]string{}, calls: map[string]int{}}
	client := startWaitService(t, stub)

	results, err := client.WaitBuild(context.Background(), nil)
	if err != nil || results != nil {
		t.Fatalf("WaitBuild(nil) = %v, %v, want nil, nil", results, err)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("calls = %v, want zero RPC for empty input", stub.calls)
	}
}

// TestWaitBuildCallbackErrorAborts 回调报错即中止：错误原样上抛，已到终态
// 的行仍随返回值交还（追加发生在回调之前）。
func TestWaitBuildCallbackErrorAborts(t *testing.T) {
	stub := &stubBuildsService{
		script: map[string][]string{"b-done": {"succeeded"}},
		calls:  map[string]int{},
	}
	client := startWaitService(t, stub)

	callbackErr := status.Error(codes.FailedPrecondition, "sink closed")
	results, err := client.WaitBuild(context.Background(), []string{"b-done"},
		fleetly.WithWaitOnTerminal(func(*serverv1.BuildView) error { return callbackErr }))
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err = %v, want callback error", err)
	}
	if len(results) != 1 || results[0].GetId() != "b-done" {
		t.Fatalf("results = %v, want the terminal row kept", results)
	}
}
