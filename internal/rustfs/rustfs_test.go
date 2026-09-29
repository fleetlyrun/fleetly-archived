package rustfs

// duty 收敛流程的 hermetic 单测（fake dockerPort——zot 部署器测试同型）：
// 部署幂等（缺失创建/在位稳态/漂移更新）、凭据惰性生成与再启用重生成、
// 移除路径（服务删、卷保留、secret 清场、凭据键删除）、差分事件、负面
//（swarm 未就绪、平台 ID 未铸、secret 材料零落事件/日志面）。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/statebackup"
)

// contains 线性包含判定（测试断言辅助）。
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// probeCommand 取 args 中的 restic 子命令（跳过 `-o`/`--flag` 全局选项对
// ——与 statebackup.resticCommand 同口径的探针内测试实现）。
func probeCommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			if args[i] == "-o" || args[i] == "--option" {
				i++
			}
			continue
		}
		return args[i]
	}
	return ""
}

// fakeProbeRunner 是探针/建桶容器执行器假件（statebackup.ResticRunner
// 端口）：记录每次调用规格，按子命令回放注入的输出与错误。
type fakeProbeRunner struct {
	specs []statebackup.ResticSpec

	initErr         error
	backupOutput    string
	snapshotsOutput string
	forgetErr       error
}

func (f *fakeProbeRunner) RunRestic(_ context.Context, spec statebackup.ResticSpec) (string, error) {
	f.specs = append(f.specs, spec)
	switch probeCommand(spec.Args) {
	case "init":
		return "", f.initErr
	case "backup":
		return f.backupOutput, nil
	case "snapshots":
		return f.snapshotsOutput, nil
	case "forget":
		return "", f.forgetErr
	default:
		return "", errors.New("fakeProbeRunner: unexpected args")
	}
}

// fakeDocker 记录服务/卷/网络/secret 的写调用（幂等收敛断言面）。
type fakeDocker struct {
	mu sync.Mutex

	swarmActive bool

	services map[string]dutydocker.ServiceSnapshot
	created  []string
	updated  []string
	removed  []string

	volumesEnsured []string
	networks       map[string]string // name → id
	secrets        map[string]bool
	secretsCreated []string
	secretsRemoved []string

	// runningTaskIP 非 = 服务有 running 任务（EnsureBucket/探针解析面）。
	runningTaskIP string

	inspectErr error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		services:      map[string]dutydocker.ServiceSnapshot{},
		networks:      map[string]string{},
		secrets:       map[string]bool{},
		runningTaskIP: "10.66.0.9",
	}
}

func (f *fakeDocker) Info(_ context.Context) (dutydocker.InfoSnapshot, error) {
	return dutydocker.InfoSnapshot{SwarmActive: f.swarmActive}, nil
}

func (f *fakeDocker) ServiceInspect(_ context.Context, name string) (dutydocker.ServiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return dutydocker.ServiceSnapshot{}, f.inspectErr
	}
	return f.services[name], nil
}

func (f *fakeDocker) ServiceCreate(_ context.Context, spec swarm.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, spec.Name)
	cur := dutydocker.ServiceSnapshot{Exists: true, Version: 1}
	fillSnapshotFromSpec(&cur, spec)
	f.services[spec.Name] = cur
	return nil
}

func (f *fakeDocker) ServiceUpdate(_ context.Context, name string, _ uint64, spec swarm.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, name)
	cur := f.services[name]
	cur.Version++
	fillSnapshotFromSpec(&cur, spec)
	f.services[name] = cur
	return nil
}

func (f *fakeDocker) ServiceRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	delete(f.services, name)
	return nil
}

func (f *fakeDocker) VolumeEnsure(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumesEnsured = append(f.volumesEnsured, name)
	return nil
}

func (f *fakeDocker) NetworkEnsure(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[name]; !ok {
		f.networks[name] = "net-" + name
	}
	return nil
}

func (f *fakeDocker) NetworkID(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.networks[name]
	if !ok {
		return "", errors.New("fakeDocker: network missing")
	}
	return id, nil
}

func (f *fakeDocker) SecretInspect(_ context.Context, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.secrets[name] {
		return "id-" + name, true, nil
	}
	return "", false, nil
}

func (f *fakeDocker) SecretCreate(_ context.Context, spec swarm.SecretSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[spec.Name] = true
	f.secretsCreated = append(f.secretsCreated, spec.Name)
	return "id-" + spec.Name, nil
}

func (f *fakeDocker) SecretList(_ context.Context, labels map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name := range f.secrets {
		out = append(out, name)
	}
	return out, nil
}

func (f *fakeDocker) SecretRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.secrets[name] {
		return nil
	}
	delete(f.secrets, name)
	f.secretsRemoved = append(f.secretsRemoved, name)
	return nil
}

func (f *fakeDocker) TaskAddress(_ context.Context, service, _ string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.services[service].Exists && f.runningTaskIP != "" {
		return f.runningTaskIP, true, nil
	}
	return "", false, nil
}

// fillSnapshotFromSpec 把期望 spec 投影为实况形态（dutydocker.snapshotOf
// 同构的消费面子集——ServiceInspect 的 fake 侧镜像；收敛后 specEqual 必须
// 为真，否则幂等收敛会死循环）。切片全量重置——update 路径不得残留旧
// spec 字段（否则收敛比对永远不等）。
func fillSnapshotFromSpec(cur *dutydocker.ServiceSnapshot, spec swarm.ServiceSpec) {
	cs := spec.TaskTemplate.ContainerSpec
	cur.Image = cs.Image
	cur.Env = append([]string{}, cs.Env...)
	cur.Mounts = nil
	cur.Mounts = append(cur.Mounts, cs.Mounts...)
	cur.SecretNames = nil
	for _, s := range cs.Secrets {
		cur.SecretNames = append(cur.SecretNames, s.SecretName)
	}
	cur.Networks = nil
	for _, n := range spec.TaskTemplate.Networks {
		cur.Networks = append(cur.Networks, n.Target)
	}
	cur.Constraints = nil
	if pl := spec.TaskTemplate.Placement; pl != nil {
		cur.Constraints = append(cur.Constraints, pl.Constraints...)
	}
	if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil {
		cur.Replicas = *spec.Mode.Replicated.Replicas
	}
	if res := spec.TaskTemplate.Resources; res != nil && res.Limits != nil {
		cur.MemoryBytes = res.Limits.MemoryBytes
	}
}

// testHarness 是 duty 测试环境（真实 store + 真实 envelope + fake 底座）。
type testHarness struct {
	t      *testing.T
	st     *state.Store
	box    *secrets.Box
	docker *fakeDocker
	mgr    *Manager

	bucketEnsures []string // 注入缝的调用账
}

// 确定性托管凭据（注入 generateCredentials —— 断言 env 注入值时需要）。
const (
	probeAK = "TESTACCESSKEY1234567"
	probeSK = "0123456789abcdef0123456789abcdef01234567"
)

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	orig := generateCredentials
	generateCredentials = func() (credentials, error) {
		return credentials{AccessKey: probeAK, SecretKey: probeSK}, nil
	}
	t.Cleanup(func() { generateCredentials = orig })
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "fleetly.key"))
	if err != nil {
		t.Fatalf("ensure key: %v", err)
	}
	if err := st.InTx(context.Background(), func(tx *state.Tx) error {
		return tx.SetMeta(context.Background(), state.MetaKeyPlatformNodeID, "n_TESTNODEID01")
	}); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	fd := newFakeDocker()
	fd.swarmActive = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManagerWithDocker(st, box, fd, logger)
	h := &testHarness{t: t, st: st, box: box, docker: fd, mgr: mgr}
	// 建桶注入缝：记录调用并即时成功（容器探针协议另有专项测试）。
	mgr.ensureBucketFn = func(_ context.Context) error {
		h.bucketEnsures = append(h.bucketEnsures, "ensured")
		return nil
	}
	return h
}

// setMode 保存 s3.mode（SaveS3Settings 的 PUT 语义；只动 mode 位）。
func (h *testHarness) setMode(mode string) {
	h.t.Helper()
	if err := h.st.SaveS3Settings(context.Background(),
		state.S3Settings{Mode: mode}, state.S3SaveOptions{Actor: "system"}); err != nil {
		h.t.Fatalf("save s3 mode %s: %v", mode, err)
	}
}

// events 抓取全量事件流。
func (h *testHarness) events() []state.Event {
	h.t.Helper()
	evs, err := h.st.EventsSince(context.Background(), 0, 100)
	if err != nil {
		h.t.Fatalf("EventsSince: %v", err)
	}
	return evs
}

// TestConvergeCreatesService 首拍收敛：缺失创建（期望 spec 已由形态表测试
// 钉死，这里钉账面）+ 差分事件 + 凭据惰性生成落库 + 建桶以任务 IP 拨号；
// 第二拍稳态零写（幂等——无 churn）。
func TestConvergeCreatesService(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)

	oc, err := h.mgr.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	if oc != outcomeDeployed {
		t.Fatalf("outcome = %v, want deployed", oc)
	}
	if len(h.docker.created) != 1 || h.docker.created[0] != ServiceName {
		t.Fatalf("created = %v, want [%s]", h.docker.created, ServiceName)
	}
	// 前置物自证：数据卷 + 内部网络收敛在列。
	seenVol, seenNet := false, false
	for _, v := range h.docker.volumesEnsured {
		if v == VolumeName {
			seenVol = true
		}
	}
	if _, ok := h.docker.networks[state.RustfsNetworkName]; ok {
		seenNet = true
	}
	if !seenVol || !seenNet {
		t.Fatalf("volume/network not ensured (vol=%v net=%v)", h.docker.volumesEnsured, h.docker.networks)
	}
	// 凭据：生成 + envelope 落库（内部键）。
	accessCT, secretCT, found, err := h.st.LoadRustfsCredentialsCiphertext(context.Background())
	if err != nil || !found || accessCT == "" || secretCT == "" {
		t.Fatalf("credentials stored found=%v err=%v, want generated+stored", found, err)
	}
	if strings.Contains(accessCT, "ACCESSKEY") {
		t.Fatal("credentials must be stored encrypted (ciphertext), not plaintext")
	}
	// 建桶：经注入缝执行一次。
	if len(h.bucketEnsures) != 1 || h.bucketEnsures[0] != "ensured" {
		t.Fatalf("bucket ensures = %v, want one ensure via probe container", h.bucketEnsures)
	}
	// 差分事件 s3.rustfs_deployed（reason=created）。
	deployed := 0
	for _, ev := range h.events() {
		if ev.Name == "s3.rustfs_deployed" {
			deployed++
			if !strings.Contains(ev.Payload, `"created"`) {
				t.Errorf("deployed payload %q missing reason=created", ev.Payload)
			}
		}
	}
	if deployed != 1 {
		t.Fatalf("s3.rustfs_deployed count = %d, want 1", deployed)
	}

	// 第二拍：稳态零写（幂等——服务/secret/事件零新增）。
	before := len(h.events())
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if len(h.docker.created) != 1 || len(h.docker.updated) != 0 {
		t.Fatalf("second beat churn: created=%v updated=%v, want zero writes", h.docker.created, h.docker.updated)
	}
	if n := len(h.events()); n != before {
		t.Fatalf("second beat emitted %d events, want 0", n-before)
	}
}

// TestConvergeUpdatesOnDrift 漂移收敛：外部篡改（模拟 spec drift）→ 下一拍
// 更新 + 事件 reason=updated。
func TestConvergeUpdatesOnDrift(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	// 外部篡改：内存限额被改。
	cur := h.docker.services[ServiceName]
	cur.MemoryBytes = 64 << 20
	h.docker.services[ServiceName] = cur

	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if len(h.docker.updated) != 1 || h.docker.updated[0] != ServiceName {
		t.Fatalf("updated = %v, want one drift update", h.docker.updated)
	}
	updated := 0
	for _, ev := range h.events() {
		if ev.Name == "s3.rustfs_deployed" && strings.Contains(ev.Payload, `"updated"`) {
			updated++
		}
	}
	if updated != 1 {
		t.Fatalf("s3.rustfs_deployed(updated) count = %d, want 1", updated)
	}
}

// TestRemoveKeepsVolume 清场路径（设计 §2.5）：mode 离开 rustfs → 服务移除
// + 事件 s3.rustfs_removed（volume_retained=true）+ secret 清场 + 凭据键
// 删除；数据卷零删除动作（端口无卷删除能力——结构性保证）；再启用凭据
// 重生成（运行时配置不烙进数据）。
func TestRemoveKeepsVolume(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure deploy: %v", err)
	}
	firstAccess, firstSecret, _, err := h.st.LoadRustfsCredentialsCiphertext(context.Background())
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}

	h.setMode(state.S3ModeUnset)
	for i := 0; i < 3; i++ { // 分步收敛（服务→secret→凭据键），多拍内完成
		if _, err := h.mgr.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure remove beat %d: %v", i+1, err)
		}
	}
	if len(h.docker.removed) != 1 || h.docker.removed[0] != ServiceName {
		t.Fatalf("removed = %v, want [%s]", h.docker.removed, ServiceName)
	}
	if len(h.docker.secretsRemoved) == 0 {
		t.Fatal("credential swarm secrets not cleaned up on disable")
	}
	if _, _, found, err := h.st.LoadRustfsCredentialsCiphertext(context.Background()); err != nil || found {
		t.Fatalf("credential keys found=%v err=%v after disable, want deleted", found, err)
	}
	// 事件：removed 一条，payload 明示卷保留。
	removedEvents := 0
	for _, ev := range h.events() {
		if ev.Name == "s3.rustfs_removed" {
			removedEvents++
			if !strings.Contains(ev.Payload, `"true"`) {
				t.Errorf("removed payload %q missing volume_retained flag", ev.Payload)
			}
		}
	}
	if removedEvents != 1 {
		t.Fatalf("s3.rustfs_removed count = %d, want 1", removedEvents)
	}

	// 再启用：凭据重生成（≠ 旧密文），数据卷仍在（fake 从未收到卷删除——
	// 卷删除能力在端口上结构性不存在）。
	h.setMode(state.S3ModeRustfs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure re-enable: %v", err)
	}
	accessCT, secretCT, found, err := h.st.LoadRustfsCredentialsCiphertext(context.Background())
	if err != nil || !found {
		t.Fatalf("credentials after re-enable found=%v err=%v, want regenerated", found, err)
	}
	if accessCT == firstAccess || secretCT == firstSecret {
		t.Fatal("credentials must be regenerated on re-enable (runtime config, not baked into the volume)")
	}
	if len(h.docker.created) != 2 {
		t.Fatalf("created = %v, want redeploy after re-enable", h.docker.created)
	}
}

// TestCredentialRotationUpdatesService 凭据指纹变化 → secret 名变化 → 服务
// spec 漂移 → 收敛更新 + 旧 secret 清场（凭据材料不残留）。
func TestCredentialRotationUpdatesService(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	oldSecretNames := h.docker.services[ServiceName].SecretNames

	// 模拟凭据轮换：库内密文换新（显式异值——不走注入的确定性生成器）。
	newC := credentials{AccessKey: "ROTATEDACCESSKEY123", SecretKey: "fedcba98765432100123456789abcdef01234567"}
	actCT, err := h.box.Encrypt([]byte(newC.AccessKey))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	secCT, err := h.box.Encrypt([]byte(newC.SecretKey))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := h.st.SaveRustfsCredentialsCiphertext(context.Background(), string(actCT), string(secCT)); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if len(h.docker.updated) == 0 {
		t.Fatal("credential rotation must trigger a service update (secret refs changed)")
	}
	cur := h.docker.services[ServiceName]
	for _, name := range cur.SecretNames {
		for _, old := range oldSecretNames {
			if name == old {
				t.Fatalf("stale secret %s still referenced after rotation", old)
			}
		}
	}
	for _, name := range oldSecretNames {
		if h.docker.secrets[name] {
			t.Fatalf("stale secret %s not removed after rotation", name)
		}
	}
}

// TestEnsureNegativePaths 负面路径：swarm 未就绪 / 平台 ID 未铸 → 可重试
// 错误（不部分写入）；unset 且无服务 = idle 稳态（零动作零事件）。
func TestEnsureNegativePaths(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)

	// swarm 未就绪。
	h.docker.swarmActive = false
	if _, err := h.mgr.Ensure(context.Background()); !errors.Is(err, ErrNotSwarmReady) {
		t.Fatalf("swarm inactive: err=%v, want ErrNotSwarmReady", err)
	}
	h.docker.swarmActive = true

	// 平台 ID 未铸（identity duty 尚未跑）。
	if err := h.st.InTx(context.Background(), func(tx *state.Tx) error {
		return tx.SetMeta(context.Background(), state.MetaKeyPlatformNodeID, "")
	}); err != nil {
		t.Fatalf("clear meta: %v", err)
	}
	if _, err := h.mgr.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "platform node id") {
		t.Fatalf("missing platform id: err=%v, want explicit retryable error", err)
	}
	// 部分写入负面：上面两拍失败后服务/凭据零落。
	if len(h.docker.created) != 0 {
		t.Fatalf("services created = %v, want none on negative path", h.docker.created)
	}
	if _, _, found, _ := h.st.LoadRustfsCredentialsCiphertext(context.Background()); found {
		t.Fatal("credentials must not be generated on a negative path")
	}

	// unset + 无服务 = idle 稳态。
	h.setMode(state.S3ModeUnset)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure idle: %v", err)
	}
	for _, ev := range h.events() {
		if ev.Name == "s3.rustfs_removed" {
			t.Fatal("removed event must not fire when nothing was deployed")
		}
	}
}

// TestSecretMaterialNeverInEvents 负面测试（state-model §2.9）：差分事件
// payload 零凭据材料（明文与指纹都不进事件——指纹只在日志面）。
func TestSecretMaterialNeverInEvents(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	accessCT, secretCT, _, err := h.st.LoadRustfsCredentialsCiphertext(context.Background())
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}
	accessPlain, err := h.box.Decrypt([]byte(accessCT))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	secretPlain, err := h.box.Decrypt([]byte(secretCT))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	for _, ev := range h.events() {
		for _, material := range []string{string(accessPlain), string(secretPlain), fingerprint(string(accessPlain)), fingerprint(string(secretPlain))} {
			if strings.Contains(ev.Payload, material) {
				t.Fatalf("event %s payload leaks credential material: %q", ev.Name, ev.Payload)
			}
		}
	}
}

// TestProbeAndBucketEnsure 探针与建桶（探测容器形态，设计 §2.5）：
// 建桶 = restic init（attach fleetly-rustfs-net + 托管凭据 env；镜像钉版
// 沿用上传轨）；RunProbe 四步（init/backup/snapshots/forget）全绿 = OK，
// 任一步失败 = 失败步可见且凭据已擦除。
func TestProbeAndBucketEnsure(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// 探针 runner 假件：按子命令回放（记录 env/spec 断言面）。
	fr := &fakeProbeRunner{}
	h.mgr.WithProbeRunner(fr)

	// EnsureBucketViaProbe：init 容器一次；镜像钉版 + 网络 + 托管凭据 env。
	if err := h.mgr.EnsureBucketViaProbe(context.Background()); err != nil {
		t.Fatalf("EnsureBucketViaProbe (fresh): %v", err)
	}
	if len(fr.specs) != 1 {
		t.Fatalf("probe runs = %d, want 1", len(fr.specs))
	}
	spec := fr.specs[0]
	if spec.Image != statebackup.DefaultResticImage {
		t.Errorf("probe image = %s, want pinned upload-track image", spec.Image)
	}
	for _, want := range []string{state.RustfsNetworkName} {
		found := false
		for _, n := range spec.Networks {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("probe networks %v missing %s", spec.Networks, want)
		}
	}
	if spec.Env["AWS_ACCESS_KEY_ID"] != probeAK || spec.Env["AWS_SECRET_ACCESS_KEY"] != probeSK {
		t.Error("probe env missing decrypted managed credentials")
	}
	if !contains(spec.Args, "-o") || !contains(spec.Args, "s3.bucket-lookup=path") {
		t.Errorf("probe args %v missing path-style option", spec.Args)
	}
	// 幂等：init 报「仓库已存在」→ 通过。
	fr.initErr = errors.New("substrate: restic init failed (exit 1): config file already exists")
	if err := h.mgr.EnsureBucketViaProbe(context.Background()); err != nil {
		t.Fatalf("EnsureBucketViaProbe (idempotent): %v", err)
	}
	// 其他 init 错误如实上抛（凭据擦除后）。
	fr.initErr = errors.New("substrate: restic init failed (exit 1): boom " + probeAK)
	if err := h.mgr.EnsureBucketViaProbe(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "probe bucket ensure failed") ||
		strings.Contains(err.Error(), probeAK) {
		t.Fatalf("init failure surfacing = %v, want scrubbed honest error", err)
	}
}

// TestRunProbeSteps RunProbe 四步协议：全绿 = OK；backup 无快照 id = 失败
// 步 backup；snapshots 读回缺 id = 失败步 snapshots（诚实读回）；forget
// 失败 = 失败步 forget。
func TestRunProbeSteps(t *testing.T) {
	h := newTestHarness(t)
	h.setMode(state.S3ModeRustfs)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	t.Run("all-green", func(t *testing.T) {
		fr := &fakeProbeRunner{backupOutput: `{"message_type":"summary","snapshot_id":"abc123"}`,
			snapshotsOutput: `[{"id":"abc123"}]`}
		h.mgr.WithProbeRunner(fr)
		pr, err := h.mgr.RunProbe(context.Background())
		if err != nil {
			t.Fatalf("RunProbe: %v", err)
		}
		if !pr.OK || len(pr.Steps) != 4 {
			t.Fatalf("probe = %+v, want ok with 4 steps", pr)
		}
	})
	// W3-F1 回归（真机发现）：第二次探针的 init 撞「repository already
	// initialized」——幂等改判本步 OK，四步全绿（auth 由 backup 步证明）。
	t.Run("init-already-initialized-idempotent", func(t *testing.T) {
		for _, msg := range []string{
			"substrate: restic init failed (exit 1): Fatal: Fatal: create key in repository at s3:http://rustfs:9000/fleetly/fleetly-probe failed: repository master key and config already initialized",
			"substrate: restic init failed (exit 1): config file already exists",
		} {
			fr := &fakeProbeRunner{initErr: errors.New(msg),
				backupOutput:    `{"message_type":"summary","snapshot_id":"abc123"}`,
				snapshotsOutput: `[{"id":"abc123"}]`}
			h.mgr.WithProbeRunner(fr)
			pr, err := h.mgr.RunProbe(context.Background())
			if err != nil {
				t.Fatalf("RunProbe (already initialized): %v", err)
			}
			if !pr.OK || pr.FailedStep != "" || len(pr.Steps) != 4 || !pr.Steps[0].OK {
				t.Fatalf("probe = %+v, want ok with idempotent init step (%s)", pr, msg)
			}
		}
	})
	// init 的其他错误（如认证失败）不被幂等豁免误吞。
	t.Run("init-auth-failure-not-exempt", func(t *testing.T) {
		fr := &fakeProbeRunner{initErr: errors.New("substrate: restic init failed (exit 1): Fatal: unable to open config file: Stat: The Access Key Id you provided does not exist"),
			backupOutput:    `{"message_type":"summary","snapshot_id":"abc123"}`,
			snapshotsOutput: `[{"id":"abc123"}]`}
		h.mgr.WithProbeRunner(fr)
		pr, err := h.mgr.RunProbe(context.Background())
		if err != nil || pr.OK || pr.FailedStep != "init" {
			t.Fatalf("probe = %+v err=%v, want failed at init (auth)", pr, err)
		}
	})
	t.Run("backup-no-snapshot-id", func(t *testing.T) {
		fr := &fakeProbeRunner{backupOutput: `{"message_type":"summary"}`}
		h.mgr.WithProbeRunner(fr)
		pr, err := h.mgr.RunProbe(context.Background())
		if err != nil || pr.OK || pr.FailedStep != "backup" {
			t.Fatalf("probe = %+v err=%v, want failed at backup", pr, err)
		}
	})
	t.Run("snapshots-missing-id", func(t *testing.T) {
		fr := &fakeProbeRunner{backupOutput: `{"message_type":"summary","snapshot_id":"abc123"}`,
			snapshotsOutput: `[{"id":"other"}]`}
		h.mgr.WithProbeRunner(fr)
		pr, err := h.mgr.RunProbe(context.Background())
		if err != nil || pr.OK || pr.FailedStep != "snapshots" {
			t.Fatalf("probe = %+v err=%v, want failed at snapshots (honest readback)", pr, err)
		}
	})
	t.Run("forget-failure", func(t *testing.T) {
		fr := &fakeProbeRunner{backupOutput: `{"message_type":"summary","snapshot_id":"abc123"}`,
			snapshotsOutput: `[{"id":"abc123"}]`, forgetErr: errors.New("prune boom: " + probeAK)}
		h.mgr.WithProbeRunner(fr)
		pr, err := h.mgr.RunProbe(context.Background())
		if err != nil || pr.OK || pr.FailedStep != "forget" {
			t.Fatalf("probe = %+v err=%v, want failed at forget", pr, err)
		}
		if strings.Contains(pr.Steps[3].Err, probeAK) {
			t.Fatalf("forget error = %q leaks credential material", pr.Steps[3].Err)
		}
	})
	t.Run("credentials-not-provisioned", func(t *testing.T) {
		h2 := newTestHarness(t)
		h2.setMode(state.S3ModeRustfs)
		h2.mgr.WithProbeRunner(&fakeProbeRunner{})
		if _, err := h2.mgr.RunProbe(context.Background()); err == nil {
			t.Fatal("probe without credentials must fail (not provisioned)")
		}
	})
}

// TestCheckHealthStates 健康面：mode 非 rustfs 恒绿（无所欠）；rustfs 模式
// 服务未在位 → 红（收敛过渡态如实可见）；服务在位 → 绿。
func TestCheckHealthStates(t *testing.T) {
	h := newTestHarness(t)
	if err := h.mgr.CheckHealth(); err != nil {
		t.Fatalf("unset mode health: %v, want green (nothing owed)", err)
	}
	h.setMode(state.S3ModeRustfs)
	if err := h.mgr.CheckHealth(); err == nil {
		t.Fatal("rustfs mode without the service must be red")
	}
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := h.mgr.CheckHealth(); err != nil {
		t.Fatalf("health after converge: %v, want green", err)
	}
}
