package execrelay

// relay duty 单测：收敛幂等（缺失创建/漂移更新/开关关闭移除）、spec 形态
// 钉死（global + host 网络 + docker sock RO 挂载 + secret 引用 + advertise
// env + wss/wss 退化）、集群 token 供给（首拍生成落哈希、secret 丢失重生
// 成——meta 哈希随之更换）。

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// fakeDutyDocker 是 dutyDocker 假实现。
type fakeDutyDocker struct {
	mu       sync.Mutex
	active   bool
	nodeAddr string
	services map[string]dutydocker.ServiceSnapshot
	specs    map[string]swarm.ServiceSpec
	created  []string
	updated  []string
	removed  []string
	secrets  map[string]string // name → ID（缺项 = 不在位）
	// secretData 录制 secret 创建载荷（token 形态钉定面）。
	secretData map[string][]byte
}

func newFakeDutyDocker(active bool) *fakeDutyDocker {
	return &fakeDutyDocker{
		active:     active,
		nodeAddr:   "10.99.0.10",
		services:   map[string]dutydocker.ServiceSnapshot{},
		specs:      map[string]swarm.ServiceSpec{},
		secrets:    map[string]string{},
		secretData: map[string][]byte{},
	}
}

func (d *fakeDutyDocker) Info(_ context.Context) (dutydocker.InfoSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return dutydocker.InfoSnapshot{SwarmActive: d.active, NodeAddr: d.nodeAddr}, nil
}

func (d *fakeDutyDocker) ServiceInspect(_ context.Context, name string) (dutydocker.ServiceSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.services[name], nil
}

func (d *fakeDutyDocker) ServiceCreate(_ context.Context, spec swarm.ServiceSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.created = append(d.created, spec.Name)
	d.storeSpecLocked(spec)
	return nil
}

func (d *fakeDutyDocker) ServiceUpdate(_ context.Context, name string, _ uint64, spec swarm.ServiceSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updated = append(d.updated, name)
	d.storeSpecLocked(spec)
	return nil
}

// storeSpecLocked 把 spec 转成实况投影（模拟底座存储形态——host 网络目标
// 归一为 "net-host-id" 以验证比对前的 NetworkName 反解路径）。
func (d *fakeDutyDocker) storeSpecLocked(spec swarm.ServiceSpec) {
	st := dutydocker.ServiceSnapshot{Exists: true, Version: 7}
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		st.Image = cs.Image
		st.Env = append([]string{}, cs.Env...)
		st.Mounts = append(st.Mounts, cs.Mounts...)
		for _, ref := range cs.Secrets {
			st.SecretIDs = append(st.SecretIDs, ref.SecretID)
			st.SecretNames = append(st.SecretNames, ref.SecretName)
		}
	}
	for _, n := range spec.TaskTemplate.Networks {
		target := n.Target
		if target == "host" {
			target = "net-host-id" // 创建期被归一为 ID 的形态
		}
		st.Networks = append(st.Networks, target)
	}
	st.Global = spec.Mode.Global != nil
	if task := spec.TaskTemplate; task.Resources != nil && task.Resources.Limits != nil {
		st.MemoryBytes = task.Resources.Limits.MemoryBytes
	}
	d.services[spec.Name] = st
	d.specs[spec.Name] = spec
}

func (d *fakeDutyDocker) ServiceRemove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.services, name)
	d.removed = append(d.removed, name)
	return nil
}

func (d *fakeDutyDocker) SecretInspect(_ context.Context, name string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, ok := d.secrets[name]
	return id, ok, nil
}

func (d *fakeDutyDocker) SecretEnsure(_ context.Context, name string, data []byte, _ map[string]string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := "sec-" + name
	d.secrets[name] = id
	d.secretData[name] = append([]byte(nil), data...)
	return id, nil
}

// NetworkName 把 ID 反解回名（比对同锚路径的模拟）。
func (d *fakeDutyDocker) NetworkName(_ context.Context, target string) (string, error) {
	if target == "net-host-id" {
		return "host", nil
	}
	return target, nil
}

func newDutyFixture(t *testing.T, enabled bool, d *fakeDutyDocker) (*Manager, *state.Store) {
	t.Helper()
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := NewManagerWithDocker(st, enabled, "8420", "", d, testLogger())
	return mgr, st
}

func TestDutyEnsureCreatesService(t *testing.T) {
	d := newFakeDutyDocker(true)
	mgr, st := newDutyFixture(t, true, d)
	ctx := context.Background()
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// 服务创建 + secret 生成 + 哈希落 meta。
	if len(d.created) != 1 || d.created[0] != ExecRelayServiceName {
		t.Fatalf("created = %v", d.created)
	}
	hash, err := st.GetMeta(ctx, MetaKeyClusterTokenHash)
	if err != nil || hash == "" {
		t.Fatalf("cluster token hash not stored (err=%v hash=%q)", err, hash)
	}
	if len(d.secretData[ExecRelaySecretName]) != 96 { // 48B hex
		t.Fatalf("cluster token length = %d, want 96 hex chars", len(d.secretData[ExecRelaySecretName]))
	}
	// spec 形态钉死（设计 §2.1：global + host 网络 + sock RO + secret env）。
	spec := d.specs[ExecRelayServiceName]
	if spec.Mode.Global == nil {
		t.Fatal("relay service must be global")
	}
	cs := spec.TaskTemplate.ContainerSpec
	if cs.Image != DefaultExecRelayImage {
		t.Fatalf("image = %q", cs.Image)
	}
	envOK, tlsOK := false, true
	for _, e := range cs.Env {
		if e == EnvControlAddr+"=10.99.0.10:8420" {
			envOK = true
		}
		if strings.HasPrefix(e, EnvControlTLSName) {
			tlsOK = false // TLSName 空（off 形态）不得注入
		}
	}
	if !envOK || !tlsOK {
		t.Fatalf("env mismatch: addrOK=%v tlsNameInjected=%v env=%v", envOK, !tlsOK, cs.Env)
	}
	foundSock := false
	for _, m := range cs.Mounts {
		if m.Source == "/var/run/docker.sock" && m.Target == "/var/run/docker.sock" && m.ReadOnly {
			foundSock = true
		}
	}
	if !foundSock {
		t.Fatal("read-only docker.sock bind mount missing")
	}
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretName != ExecRelaySecretName || cs.Secrets[0].File == nil || cs.Secrets[0].File.Mode != 0o444 {
		t.Fatalf("secret reference mismatch: %+v", cs.Secrets)
	}
	// 幂等：二拍不再更新。
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if len(d.updated) != 0 {
		t.Fatalf("unexpected update on steady state: %v", d.updated)
	}
}

func TestDutyEnsureDrift(t *testing.T) {
	d := newFakeDutyDocker(true)
	mgr, _ := newDutyFixture(t, true, d)
	ctx := context.Background()
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// 外部改动实况（env 漂移）→ 下一拍更新。
	d.mu.Lock()
	cur := d.services[ExecRelayServiceName]
	cur.Env = []string{EnvControlAddr + "=wrong:1"}
	d.services[ExecRelayServiceName] = cur
	d.mu.Unlock()
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure after drift: %v", err)
	}
	if len(d.updated) != 1 || d.updated[0] != ExecRelayServiceName {
		t.Fatalf("updates = %v, want one drift update", d.updated)
	}
}

func TestDutyDisabledRemoves(t *testing.T) {
	d := newFakeDutyDocker(true)
	enabled := true
	mgr, _ := newDutyFixture(t, enabled, d)
	ctx := context.Background()
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// 开关翻 false（静态配置重启形态——fixture 直接改字段）。
	mgr.enabled = false
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure disabled: %v", err)
	}
	if len(d.removed) != 1 {
		t.Fatalf("removed = %v, want service removal", d.removed)
	}
	// secret 与 meta 哈希保留（重启用同一 token 身份）。
	if _, ok := d.secrets[ExecRelaySecretName]; !ok {
		t.Fatal("cluster token secret must be retained across disable")
	}
}

func TestDutyTokenRegeneration(t *testing.T) {
	d := newFakeDutyDocker(true)
	mgr, st := newDutyFixture(t, true, d)
	ctx := context.Background()
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	oldHash, _ := st.GetMeta(ctx, MetaKeyClusterTokenHash)
	// swarm 状态丢失：secret 消失 → 重生成 + 哈希更换。
	d.mu.Lock()
	delete(d.secrets, ExecRelaySecretName)
	d.mu.Unlock()
	if err := mgr.Ensure(ctx); err != nil {
		t.Fatalf("Ensure after secret loss: %v", err)
	}
	newHash, _ := st.GetMeta(ctx, MetaKeyClusterTokenHash)
	if oldHash == newHash {
		t.Fatal("cluster token hash must rotate after secret loss")
	}
	if _, ok := d.secrets[ExecRelaySecretName]; !ok {
		t.Fatal("cluster token secret must be recreated")
	}
}

// TestDutyTLSNameInjection platform 模式：tlsName 注入 wss 校验名；off/manual
// 诚实降级（不注入 = ws://）。
func TestDutyTLSNameInjection(t *testing.T) {
	d := newFakeDutyDocker(true)
	st, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := NewManagerWithDocker(st, true, "8420", "ctrl.example.com", d, testLogger())
	if err := mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	spec := d.specs[ExecRelayServiceName]
	found := false
	for _, e := range spec.TaskTemplate.ContainerSpec.Env {
		if e == EnvControlTLSName+"=ctrl.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("wss ServerName env missing in %v", spec.TaskTemplate.ContainerSpec.Env)
	}
}

// TestDutySpecEqual 单元钉死幂等比对的敏感面（env/挂载/secret/网络/global
// ——任何执行面漂移都必须被捕获）。
func TestDutySpecEqual(t *testing.T) {
	desired := buildSpec("10.0.0.1:8420", "ctrl.example.com", "sec-1")
	base := dutydocker.ServiceSnapshot{Exists: true, Image: DefaultExecRelayImage,
		Env:       []string{EnvControlAddr + "=10.0.0.1:8420", EnvControlTLSName + "=ctrl.example.com"},
		Networks:  []string{"host"},
		Mounts:    []mount.Mount{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}},
		SecretIDs: []string{"sec-1"}, SecretNames: []string{ExecRelaySecretName},
		Global: true, MemoryBytes: relayMemoryLimitBytes}
	if !specEqual(base, desired) {
		t.Fatal("identical spec must compare equal")
	}
	cases := map[string]func(*dutydocker.ServiceSnapshot){
		"image drift":   func(s *dutydocker.ServiceSnapshot) { s.Image = "other:1" },
		"env drift":     func(s *dutydocker.ServiceSnapshot) { s.Env = []string{EnvControlAddr + "=10.0.0.2:8420"} },
		"mount drift":   func(s *dutydocker.ServiceSnapshot) { s.Mounts[0].Source = "/other.sock" },
		"network drift": func(s *dutydocker.ServiceSnapshot) { s.Networks = []string{"bridge"} },
		"secret drift":  func(s *dutydocker.ServiceSnapshot) { s.SecretIDs[0] = "sec-2" },
		"mode drift":    func(s *dutydocker.ServiceSnapshot) { s.Global = false },
		"limit drift":   func(s *dutydocker.ServiceSnapshot) { s.MemoryBytes = 1 },
	}
	for name, mutate := range cases {
		cur := base
		mutate(&cur)
		if specEqual(cur, desired) {
			t.Fatalf("%s must be detected as drift", name)
		}
	}
}

// TestDutyNotSwarmReady swarm 未就绪 → 哨兵错误（duty 退避重试态）。
func TestDutyNotSwarmReady(t *testing.T) {
	d := newFakeDutyDocker(false)
	mgr, _ := newDutyFixture(t, true, d)
	if err := mgr.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure must fail when swarm is not active")
	}
}
