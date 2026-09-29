package engine

// secret 挂载面测试（E4 managed-databases §2.7，D-DB-7，W4-S4 验收 3/4/6）：
// preflight E_SECRET_NOT_FOUND（缺失点名 + plan-time fail-fast）、
// SecretMount 进服务 spec（/run/secrets/<target> + Swarm secret 名内嵌
// 值指纹）、底座 ensure 收到解密后的值（装载链面）、回滚 preflight 对
// 快照挂载名的现值解析（轮换悬空 → E_SECRET_NOT_FOUND）、值零出现在
// 事件/错误。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// fakeSecretEnsure 是 SecretEnsurer 端口的测试替身（记录 ensure 调用的
// 名字与值——值只在本替身内存，断言后即弃）。
type fakeSecretEnsure struct {
	ensured map[string][]byte
	labels  map[string]map[string]string
	failOn  string
}

func newFakeSecretEnsure() *fakeSecretEnsure {
	return &fakeSecretEnsure{ensured: map[string][]byte{}, labels: map[string]map[string]string{}}
}

func (f *fakeSecretEnsure) EnsureSecret(_ context.Context, name string, data []byte, labels map[string]string) (string, error) {
	if f.failOn == name {
		return "", errors.New("simulated substrate failure")
	}
	f.ensured[name] = data
	f.labels[name] = labels
	return "sid-" + name, nil
}

// secretCompose 是 external secret 部署 fixture：web 短语法 + worker 长语法
// （同 source 双服务引用 = 一次解密一次 ensure 的去重对照）。
//
//nolint:gosec // G101：external secret 部署 fixture，无真实凭据
const secretCompose = `name: demo
services:
  web:
    image: alpine:3
    command: ["sleep", "infinity"]
    secrets: [dbpass]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
  worker:
    image: alpine:3
    command: ["sleep", "infinity"]
    secrets: [{ source: dbpass, target: dbpass.txt }]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
secrets:
  dbpass: { external: true }
`

// mustSetAppSecret 落一条平台密钥库条目（密文 + hash8 由调用面构造）。
func mustSetAppSecret(t *testing.T, h *harness, appID, name, value string) string {
	t.Helper()
	cipher, err := h.box.Encrypt([]byte(value))
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	if _, err := h.store.UpsertAppSecret(context.Background(), state.AppSecret{
		AppID:       appID,
		Name:        name,
		ValueCipher: string(cipher),
		Hash8:       naming.Hash8(value),
	}); err != nil {
		t.Fatalf("upsert app secret: %v", err)
	}
	return naming.Hash8(value)
}

// TestDeployWithExternalSecret 主链路：external secret 声明 → spec 挂载
// （名内嵌指纹 + /run/secrets/<target>）→ 底座 ensure 收到解密值 → 双服务
// 去重（单次 ensure）。
func TestDeployWithExternalSecret(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeSecretEnsure()
	h.eng.WithSecretEnsurer(ensure)
	ctx := context.Background()

	const secretValue = "s3cr3t-Value-42"
	app, err := h.store.GetAppByName(ctx, "demo")
	if err != nil {
		if !errors.Is(err, state.ErrAppNotFound) {
			t.Fatalf("get app: %v", err)
		}
		app, err = testsupport.SeedAppE(t, h.store, "demo")
		if err != nil {
			t.Fatalf("create app: %v", err)
		}
	}
	hash8 := mustSetAppSecret(t, h, app.ID, "dbpass", secretValue)

	final := h.runToTerminal(h.enqueue(h.writeCompose(secretCompose)))
	if final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s), want succeeded", final.Status, final.ErrorCode)
	}

	// spec 挂载面：短语法 target 缺省 = source；长语法 target 显式。
	wantName := h.demoSecretPrefix("dbpass") + hash8
	web := h.sub.services[h.svc("web")].spec
	if len(web.Secrets) != 1 {
		t.Fatalf("web secrets = %+v, want exactly one mount", web.Secrets)
	}
	if web.Secrets[0].SecretName != wantName {
		t.Errorf("web secret name = %q, want %q (rotation-renames-reference contract)", web.Secrets[0].SecretName, wantName)
	}
	if web.Secrets[0].Target != "/run/secrets/dbpass" {
		t.Errorf("web secret target = %q, want /run/secrets/dbpass", web.Secrets[0].Target)
	}
	worker := h.sub.services[h.svc("worker")].spec
	if len(worker.Secrets) != 1 || worker.Secrets[0].Target != "/run/secrets/dbpass.txt" {
		t.Fatalf("worker secret mount = %+v, want long-syntax target /run/secrets/dbpass.txt", worker.Secrets)
	}

	// 底座 ensure：值 = 解密后的明文（装载链面）；双服务引用去重一次。
	if data, ok := ensure.ensured[wantName]; !ok {
		t.Fatalf("swarm secret %s was not ensured", wantName)
	} else if string(data) != secretValue {
		t.Errorf("ensured data mismatch (len %d vs %d)", len(data), len(secretValue))
	}
	if len(ensure.ensured) != 1 {
		t.Errorf("ensure calls = %d, want 1 (same source deduped across services)", len(ensure.ensured))
	}
	demo := h.demoApp()
	if lbl := ensure.labels[wantName]; lbl[state.LabelApp] != demo.QualifiedName() || lbl[state.LabelManaged] != state.ManagedLabelValue {
		t.Errorf("ensure labels = %+v, want managed + app=%s ownership anchors", lbl, demo.QualifiedName())
	}

	// 值零出现：事件载荷（快照只带名字；desired_spec 密文由引擎加密落库，
	// 不在断言面——与 env 值同纪律）。
	for name, payload := range eventPayloads(t, h) {
		if strings.Contains(payload, secretValue) {
			t.Errorf("event %s payload contains the secret value", name)
		}
	}
}

// TestDeploySecretMissingPreflight 缺失 preflight（验收 3）：声明名不在
// 密钥库 → E_SECRET_NOT_FOUND（422）点名缺失名；底座零 ensure。
func TestDeploySecretMissingPreflight(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeSecretEnsure()
	h.eng.WithSecretEnsurer(ensure)
	testsupport.SeedApp(t, h.store, "demo")

	final := h.runToTerminal(h.enqueue(h.writeCompose(secretCompose)))
	if final.Status != state.DeployFailed {
		t.Fatalf("deploy = %s, want failed (preflight fail-fast)", final.Status)
	}
	if final.ErrorCode != "E_SECRET_NOT_FOUND" {
		t.Fatalf("error code = %s, want E_SECRET_NOT_FOUND", final.ErrorCode)
	}
	payload := eventPayloads(t, h)["deployment.failed"]
	if !strings.Contains(payload, "dbpass") {
		t.Fatalf("failure disclosure must name the missing secret: %s", payload)
	}
	if len(ensure.ensured) != 0 {
		t.Errorf("substrate must not be touched on preflight failure, saw %d ensures", len(ensure.ensured))
	}
}

// TestSnapshotSecretResolution 回滚 preflight（验收 3 后半）：快照挂载名对
// app_secrets 现值解析——值匹配 → ensure 且通过；值轮换（换 hash8 换名）→
// E_SECRET_NOT_FOUND 悬空诚实失败。
func TestSnapshotSecretResolution(t *testing.T) {
	h := newHarness(t)
	ensure := newFakeSecretEnsure()
	h.eng.WithSecretEnsurer(ensure)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, h.store, "demo")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	hash8 := mustSetAppSecret(t, h, app.ID, "dbpass", "value-v1-AAA")

	mounts := []SecretMount{{SecretName: h.demoSecretPrefix("dbpass") + hash8, Target: "/run/secrets/dbpass"}}
	specs := []ServiceSpec{{
		Name:    h.svc("web"),
		Image:   "alpine:3@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Secrets: mounts,
	}}
	rec := state.DeployRecord{ID: "dep-1", AppID: app.ID, AppName: "demo"}

	// 匹配：preflight 通过 + ensure 在位。
	if err := h.eng.preflightRollback(ctx, rec, specs); err != nil {
		t.Fatalf("preflight with matching secret: %v", err)
	}
	if _, ok := ensure.ensured[h.demoSecretPrefix("dbpass")+hash8]; !ok {
		t.Fatal("replay path did not ensure the swarm secret")
	}

	// 轮换：值变化 → 名变 → 快照挂载名悬空 → E_SECRET_NOT_FOUND。
	mustSetAppSecret(t, h, app.ID, "dbpass", "value-v2-BBB")
	err = h.eng.preflightRollback(ctx, rec, specs)
	if err == nil {
		t.Fatal("preflight with rotated-away secret must fail")
	}
	ae, ok := asAppErrEnvelope(err)
	if !ok || ae.Code() != "E_SECRET_NOT_FOUND" {
		t.Fatalf("error = %v, want E_SECRET_NOT_FOUND envelope", err)
	}
}

// asAppErrEnvelope 是 errors.As 的包内薄封装（apperr 信封断言用）。
func asAppErrEnvelope(err error) (interface{ Code() string }, bool) {
	var ae interface{ Code() string }
	for err != nil {
		if e, ok := err.(interface{ Code() string }); ok {
			ae = e
			return ae, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
