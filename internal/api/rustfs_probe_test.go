package api

// TestConnection rustfs 分支的接线测试（E3-5）：TestS3Connection 的已存
// 配置路径在 mode=rustfs 时经 rustfs.Manager.RunProbe 以探测容器执行
//（认证/写/读回/删除四步真实往返，E_S3_TEST_FAILED 信封 context 带失败步
// ——诚实契约与 in-process 探针同构）。runner 假件回放四步成功/失败。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/rustfs"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/statebackup"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRustfsDocker 是 rustfs.dockerPort 的 api 侧最小假件（unexported 接口
// 由方法集隐式满足）：swarm 在位、凭据 secret 在位（探针只消费凭据面）。
type fakeRustfsDocker struct{}

func (fakeRustfsDocker) Info(context.Context) (dutydocker.InfoSnapshot, error) {
	return dutydocker.InfoSnapshot{SwarmActive: true}, nil
}

func (fakeRustfsDocker) ServiceInspect(_ context.Context, _ string) (dutydocker.ServiceSnapshot, error) {
	return dutydocker.ServiceSnapshot{Exists: true, Version: 1}, nil
}

func (fakeRustfsDocker) ServiceCreate(context.Context, swarm.ServiceSpec) error { return nil }
func (fakeRustfsDocker) ServiceRemove(context.Context, string) error            { return nil }

func (fakeRustfsDocker) ServiceUpdate(_ context.Context, _ string, _ uint64, _ swarm.ServiceSpec) error {
	return nil
}

func (fakeRustfsDocker) VolumeEnsure(context.Context, string) error { return nil }
func (fakeRustfsDocker) NetworkEnsure(context.Context, string, bool) error {
	return nil
}
func (fakeRustfsDocker) SecretRemove(context.Context, string) error { return nil }

func (fakeRustfsDocker) NetworkID(_ context.Context, _ string) (string, error) {
	return "net-fake", nil
}

func (fakeRustfsDocker) SecretInspect(context.Context, string) (string, bool, error) {
	return "id-fake", true, nil
}

func (fakeRustfsDocker) SecretCreate(context.Context, swarm.SecretSpec) (string, error) {
	return "id-fake", nil
}

func (fakeRustfsDocker) SecretList(context.Context, map[string]string) ([]string, error) {
	return nil, nil
}

func (fakeRustfsDocker) TaskAddress(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

// fakeProbeRunner 是探针容器执行器假件：init 幂等容忍、backup 出固定快照
// id、snapshots 读回在列、forget 可注入失败。
type fakeProbeRunner struct {
	forgetErr error
}

func (f *fakeProbeRunner) RunRestic(_ context.Context, spec statebackup.ResticSpec) (string, error) {
	switch probeCommandOf(spec.Args) {
	case "init":
		return "", nil
	case "backup":
		return `{"message_type":"summary","snapshot_id":"abc123"}`, nil
	case "snapshots":
		return `[{"id":"abc123"}]`, nil
	case "forget":
		return "", f.forgetErr
	default:
		return "", errors.New("fakeProbeRunner: unexpected args")
	}
}

// probeCommandOf 取 restic 子命令（跳过 `-o`/`--flag` 全局选项对）。
func probeCommandOf(args []string) string {
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			if args[i] == "-o" {
				i++
			}
			continue
		}
		return args[i]
	}
	return ""
}

func TestS3RustfsProbeWiring(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(context.Background(), dir+"/s3rustfs.db")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(dir + "/s3rustfs.key")
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	admin := seedTokenPlain(t, st, "admin")

	// rustfs duty 管理器（fake 底座 + fake 探针执行器）：托管凭据已备便。
	rmgr := rustfs.NewManagerWithDocker(st, box, fakeRustfsDocker{}, discardLogger()).
		WithProbeRunner(&fakeProbeRunner{})
	if err := st.SaveS3Settings(context.Background(), state.S3Settings{Mode: state.S3ModeRustfs},
		state.S3SaveOptions{Actor: "system"}); err != nil {
		t.Fatalf("SaveS3Settings: %v", err)
	}
	actCT, err := box.Encrypt([]byte("loopback-AK"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	secCT, err := box.Encrypt([]byte("loopback-SK"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.SaveRustfsCredentialsCiphertext(context.Background(), string(actCT), string(secCT)); err != nil {
		t.Fatalf("SaveRustfsCredentialsCiphertext: %v", err)
	}

	svc := NewSystemService("dev", st, nil, nil, rmgr).WithSecretsBox(box)
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srv, svc)
	conn := serveBufconn(t, srv)
	cl := serverv1.NewSystemServiceClient(conn)

	// 已存配置路径（无候选参数）→ rustfs 分支 → 探测容器四步 → ok。
	res, err := cl.TestS3Connection(authCtx(context.Background(), admin), &serverv1.TestS3ConnectionRequest{})
	if err != nil {
		t.Fatalf("rustfs stored-config probe: %v", err)
	}
	got := res.GetResult()
	if !got.GetOk() || got.GetEndpointUrl() != "http://rustfs:9000" || got.GetBucket() != "fleetly" || !got.GetPathStyle() {
		t.Fatalf("probe result = %+v, want ok over the managed endpoint", got)
	}
	if len(got.GetSteps()) != 4 {
		t.Fatalf("steps = %d, want 4 (init/backup/snapshots/forget)", len(got.GetSteps()))
	}

	// rustfs 面未装配的形态：探针分支如实报不可用（不谎报 TCP 失败）。
	svcNil := NewSystemService("dev", st, nil, nil, nil).WithSecretsBox(box)
	srvNil := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srvNil, svcNil)
	connNil := serveBufconn(t, srvNil)
	clNil := serverv1.NewSystemServiceClient(connNil)
	_, err = clNil.TestS3Connection(authCtx(context.Background(), admin), &serverv1.TestS3ConnectionRequest{})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("rustfs face missing: err = %v, want Unavailable with explicit note", err)
	}
}
