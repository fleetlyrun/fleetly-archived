package cmd

// S17-D3 机制测试：一元 RPC 缺省 deadline（拦截器层——fake conn 挂起 +
// 短父 deadline → 限时失败 + 超时引导渲染）、流式/等待动词的取消干净退
// 出（exit 0、不渲染错误）、Unavailable 引导渲染。

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lynx-go/commands"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestUnaryTimeoutInterceptor 缺省 deadline 拦截器：每次一元调用挂 30s
// deadline；父 ctx 自带更早 deadline 时不放宽（min 语义）——fake conn 挂
// 起（invoker 永不主动返回，即 daemon 假死的调用面投影）在父 deadline
// 处限时失败，错误为 DeadlineExceeded 且渲染带可行动提示。
func TestUnaryTimeoutInterceptor(t *testing.T) {
	// 常规调用：deadline 已挂载。
	var sawDeadline bool
	probe := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		_, sawDeadline = ctx.Deadline()
		return nil
	}
	if err := defaultUnaryTimeout(context.Background(), "/x", nil, nil, nil, probe); err != nil || !sawDeadline {
		t.Fatalf("default deadline not attached: err=%v saw=%v", err, sawDeadline)
	}

	// 挂起调用 + 更早的父 deadline → 限时失败（不等待 30s 缺省值）。
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	hang := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err() // gRPC 面同型投影
	}
	start := time.Now()
	err := defaultUnaryTimeout(parent, "/x", nil, nil, nil, hang)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("hanging call did not fail on the parent deadline: %v", elapsed)
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("hanging call error is not DeadlineExceeded: %v", err)
	}
	for _, want := range []string{"request timed out", "--timeout"} {
		if !strings.Contains(renderCLIError(err), want) {
			t.Errorf("DeadlineExceeded render missing %q:\n%s", want, renderCLIError(err))
		}
	}
}

// TestUnaryTimeoutLongBudgetOverride 长预算覆盖（W3 遗留票收口）：同步
// TriggerBackup（响应含远端上传结论，服务端合法耗时 ≈ TriggerTimeout 5m
// + uploadTimeout 10m）的客户端 deadline 取 16min 覆盖值——长上传不再被
// 30s 缺省掐死；其余方法（含 DatabaseService 的异步受理 TriggerBackup
// ——快回）仍走 30s 缺省；父 ctx 更早 deadline 时不放宽（min 语义同缺省）。
func TestUnaryTimeoutLongBudgetOverride(t *testing.T) {
	if got := unaryTimeoutFor(triggerBackupMethod); got != 16*time.Minute {
		t.Fatalf("TriggerBackup unary timeout = %s, want 16m (5m snapshot + 10m upload + margin)", got)
	}
	saw := time.Duration(0)
	probe := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		dl, ok := ctx.Deadline()
		if ok {
			saw = time.Until(dl)
		}
		return nil
	}
	if err := defaultUnaryTimeout(context.Background(), triggerBackupMethod, nil, nil, nil, probe); err != nil {
		t.Fatalf("TriggerBackup call: %v", err)
	}
	if saw < 15*time.Minute {
		t.Fatalf("TriggerBackup deadline ≈ %s, want ≈ 16m (long-upload relief)", saw)
	}
	// 异步受理的同名动词（databases backup → DatabaseService）不吃覆盖。
	if got := unaryTimeoutFor("/fleetly.server.v1.DatabaseService/TriggerBackup"); got != defaultRPCTimeout {
		t.Fatalf("DatabaseService/TriggerBackup timeout = %s, want the 30s default (async accept)", got)
	}
	// min 语义：父 ctx 更早 deadline 时不放宽。
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = defaultUnaryTimeout(parent, triggerBackupMethod, nil, nil, nil, func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("override ignored the earlier parent deadline: %v", elapsed)
	}
}

// TestIsCleanCancel 干净取消判定的边界：Canceled 本尊与 gRPC 投影（根 ctx
// 已取消）为干净；根 ctx 未取消的服务端 Canceled、超时、无错误均不干净。
func TestIsCleanCancel(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if !isCleanCancel(canceled, context.Canceled) {
		t.Error("context.Canceled itself should count as clean")
	}
	if !isCleanCancel(canceled, status.FromContextError(context.Canceled).Err()) {
		t.Error("gRPC projection (codes.Canceled) should count as clean")
	}
	if isCleanCancel(context.Background(), status.Error(codes.Canceled, "server canceled")) {
		t.Error("server-side cancel with a live root ctx must not count as clean")
	}
	if isCleanCancel(canceled, context.DeadlineExceeded) {
		t.Error("DeadlineExceeded must not count as clean")
	}
	if isCleanCancel(canceled, nil) {
		t.Error("nil error must not count as clean")
	}
}

// TestStreamCancelCleanExit 流式动词（logs follow / events watch）在 ctx
// 取消（Ctrl-C 的信号投影）时干净退出：exit 0、stderr 无错误渲染。预先
// 取消的 ctx 使首个流式 RPC 立即失败——无需真实服务面，不起子进程。
func TestStreamCancelCleanExit(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "follow", "my-api"},
		{"events", "watch"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		env := &commands.Environment{Stdout: &stdout, Stderr: &stderr}
		code := NewApp(testAppVersion).Run(ctx, env, args)
		if code != 0 {
			t.Fatalf("%v: code=%d, want 0 (clean exit)\nstderr=%s", args, code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("%v: clean exit must not render an error: %q", args, stderr.String())
		}
	}
}

// TestWaitVerbCancelCleanExit 轮询等待动词（rollback / deployments cancel
// ——deploy/build 同骨架）在 ctx 取消时同样干净退出（exit 0）。
func TestWaitVerbCancelCleanExit(t *testing.T) {
	for _, args := range [][]string{
		{"rollback", "my-api"},
		{"deployments", "cancel", "01TEST"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		env := &commands.Environment{Stdout: &stdout, Stderr: &stderr}
		code := NewApp(testAppVersion).Run(ctx, env, args)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("%v: code=%d stderr=%q, want 0/empty (clean exit)", args, code, stderr.String())
		}
	}
}

// TestTokenFlagHelpNoEnvEcho H1 回归：--token 的 flag 默认值必须为空串
// ——std flag 的 -h/help 会把非空默认值（PrintDefaults 的 "(default …)"）
// 明文打进 stdout，帮助输出常被贴进工单/CI 日志/AI 会话。设 FLEETLY_
// TOKEN 后打印动词帮助（动词级 -h 与顶层 help <verb> 两个帮助面），断言
// 输出不含 token 本体——env 回落挪到消费点 dial()（golden 测试的 env
// 链路覆盖行为等价）。同时钉住帮助文案口径：指路 env 与 bootstrap-token
// 文件（B5 后 token 不进日志，旧"首启日志"指引自 B1 起废除）。
func TestTokenFlagHelpNoEnvEcho(t *testing.T) {
	const secret = "flt_h1_no_env_echo_regression" //nolint:gosec // G101：回显回归钉测标记，非真实凭据
	t.Setenv("FLEETLY_TOKEN", secret)
	// 帮助面 ①：动词级 -h（apps list 带 conn flags）。
	code, out, _ := runCLI(t, "apps", "list", "-h")
	if code != 0 {
		t.Fatalf("apps list -h: code=%d, want 0 (-h is a help exit)", code)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("verb-level -h echoed the FLEETLY_TOKEN value:\n%s", out)
	}
	// 帮助面 ②：顶层 help <verb>（deploy 带 conn flags）。
	code, out, _ = runCLI(t, "help", "deploy")
	if code != 0 {
		t.Fatalf("help deploy: code=%d, want 0", code)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("help <verb> echoed the FLEETLY_TOKEN value:\n%s", out)
	}
	// 文案口径：指路 env 与 bootstrap-token 文件，不出现已废除的日志通道。
	for _, want := range []string{"FLEETLY_TOKEN", "bootstrap-token"} {
		if !strings.Contains(out, want) {
			t.Errorf("token help text missing %q guidance:\n%s", want, out)
		}
	}
	if strings.Contains(out, "first-start log") {
		t.Errorf("token help text still points to the retired log channel:\n%s", out)
	}
}

// TestRenderUnavailableHint Unavailable（fleetlyd 未起/addr 错）的引导
// 渲染：与 401 hint 同风格的可行动提示（地址/环境变量/守护进程状态）。
func TestRenderUnavailableHint(t *testing.T) {
	got := renderCLIError(status.Error(codes.Unavailable, "connection error: dial 127.0.0.1:8421: connect: connection refused"))
	for _, want := range []string{"fleetlyd unreachable", "--addr", "127.0.0.1:8421", "FLEETLY_ADDR", "systemctl status fleetlyd"} {
		if !strings.Contains(got, want) {
			t.Errorf("Unavailable render missing %q:\n%s", want, got)
		}
	}
}
