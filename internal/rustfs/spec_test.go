package rustfs

// 期望 spec 构造与幂等比对的单测（hermetic——零底座依赖）：形态表逐项
// 钉死（镜像钉版/卷/网络+alias/约束/限额/env/凭据 secret 引用）、比对面
// 正负路径、alias 与派生端点的不变量、凭据生成的字形与强度。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/state"
)

func testCreds() credentials {
	return credentials{AccessKey: "ACCESSKEY20CHARS1234", SecretKey: "0123456789abcdef0123456789abcdef01234567"}
}

// inspectOf 把期望 spec 投影为实况形态（dutydocker.snapshotOf 同构的消费
// 面子集——真实 ServiceInspect 的测试内镜像；投影口径一致由 TestSpecEqualPaths
// 的正路径钉住）。
func inspectOf(spec swarm.ServiceSpec) (dutydocker.ServiceSnapshot, error) {
	out := dutydocker.ServiceSnapshot{Exists: true, Version: 1}
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return out, errors.New("nil ContainerSpec")
	}
	out.Image = cs.Image
	out.Env = append([]string{}, cs.Env...)
	out.Mounts = append(out.Mounts, cs.Mounts...)
	for _, s := range cs.Secrets {
		out.SecretNames = append(out.SecretNames, s.SecretName)
	}
	for _, n := range spec.TaskTemplate.Networks {
		out.Networks = append(out.Networks, n.Target)
	}
	if pl := spec.TaskTemplate.Placement; pl != nil {
		out.Constraints = append([]string{}, pl.Constraints...)
	}
	if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil {
		out.Replicas = *spec.Mode.Replicated.Replicas
	}
	if res := spec.TaskTemplate.Resources; res != nil && res.Limits != nil {
		out.MemoryBytes = res.Limits.MemoryBytes
	}
	return out, nil
}

// TestBuildSpecShape 形态表逐项（设计 §2.5）：钉版镜像、数据卷、内部网络
// + alias、manager 约束、replicated-1、内存限额 256MB、官方 env 形态、
// 凭据 secret 文件挂载、零 host 端口（EndpointSpec 缺省无端口发布）。
func TestBuildSpecShape(t *testing.T) {
	c := testCreds()
	refs := []secretRef{
		{Name: accessSecretName(c), ID: "id-access", Target: accessKeyFileTarget},
		{Name: secretSecretName(c), ID: "id-secret", Target: secretKeyFileTarget},
	}
	spec := buildSpec("net-id-123", "n_TESTNODEID", c, refs)

	if spec.Name != ServiceName {
		t.Fatalf("service name = %s, want %s", spec.Name, ServiceName)
	}
	if spec.Labels[state.LabelManaged] != state.ManagedLabelValue ||
		spec.Labels[rustfsLabel] != "true" {
		t.Fatalf("service labels = %v, want managed+rustfs", spec.Labels)
	}
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		t.Fatal("nil ContainerSpec")
	}
	if cs.Image != DefaultRustFSImage {
		t.Fatalf("image = %s, want pinned %s", cs.Image, DefaultRustFSImage)
	}
	if !strings.HasPrefix(DefaultRustFSImage, "rustfs/rustfs:1.0.0@sha256:") {
		t.Fatalf("DefaultRustFSImage = %s, want tag+digest pinned form (D-S3-10)", DefaultRustFSImage)
	}
	if len(cs.Mounts) != 1 || cs.Mounts[0].Source != VolumeName || cs.Mounts[0].Target != dataMountPath {
		t.Fatalf("mounts = %+v, want %s → %s", cs.Mounts, VolumeName, dataMountPath)
	}
	// 官方 env 形态：S3 API 地址 + 控制台关闭（最小暴露）；凭据走 _FILE
	// 指向 secret 挂载（不进 env 值——负面：env 里不出现凭据材料）。
	env := map[string]bool{}
	for _, kv := range cs.Env {
		env[kv] = true
	}
	if !env["RUSTFS_ADDRESS=:"+backendPort] {
		t.Errorf("env missing RUSTFS_ADDRESS=:9000, got %v", cs.Env)
	}
	if !env["RUSTFS_CONSOLE_ENABLE=false"] {
		t.Errorf("env missing RUSTFS_CONSOLE_ENABLE=false (console off), got %v", cs.Env)
	}
	for _, kv := range cs.Env {
		if strings.Contains(kv, c.AccessKey) || strings.Contains(kv, c.SecretKey) {
			t.Errorf("env leaks credential material: %q", kv)
		}
	}
	if len(cs.Secrets) != 2 {
		t.Fatalf("secret refs = %d, want 2 (access+secret)", len(cs.Secrets))
	}
	byTarget := map[string]secretRef{}
	for _, s := range cs.Secrets {
		if s.File == nil {
			t.Fatalf("secret ref %s missing file target", s.SecretName)
		}
		byTarget[s.File.Name] = secretRef{Name: s.SecretName, ID: s.SecretID, Target: s.File.Name}
	}
	if byTarget[accessKeyFileTarget].Name != accessSecretName(c) || byTarget[accessKeyFileTarget].ID == "" {
		t.Errorf("access secret ref mismatch: %v", byTarget)
	}
	if byTarget[secretKeyFileTarget].Name != secretSecretName(c) || byTarget[secretKeyFileTarget].ID == "" {
		t.Errorf("secret ref mismatch: %v", byTarget)
	}
	// 网络：目标 = 内部 overlay ID；alias = 派生端点 host（不变量单独测）。
	if len(spec.TaskTemplate.Networks) != 1 {
		t.Fatalf("networks = %d, want 1", len(spec.TaskTemplate.Networks))
	}
	nats := spec.TaskTemplate.Networks[0]
	if nats.Target != "net-id-123" {
		t.Errorf("network target = %s, want net-id-123", nats.Target)
	}
	if len(nats.Aliases) != 1 || nats.Aliases[0] != serviceNetworkAlias {
		t.Errorf("aliases = %v, want [%s]", nats.Aliases, serviceNetworkAlias)
	}
	if pl := spec.TaskTemplate.Placement; pl == nil || len(pl.Constraints) != 1 ||
		pl.Constraints[0] != "node.labels."+state.LabelNodeID+" == n_TESTNODEID" {
		t.Fatalf("constraints = %+v, want manager pin on %s", spec.TaskTemplate.Placement, state.LabelNodeID)
	}
	if spec.TaskTemplate.Resources == nil || spec.TaskTemplate.Resources.Limits == nil ||
		spec.TaskTemplate.Resources.Limits.MemoryBytes != memoryLimitBytes {
		t.Fatalf("resources = %+v, want memory limit %d", spec.TaskTemplate.Resources, memoryLimitBytes)
	}
	if memoryLimitBytes != 256<<20 {
		t.Fatalf("memory limit = %d, want 256MB (zot parity)", memoryLimitBytes)
	}
	if spec.Mode.Replicated == nil || spec.Mode.Replicated.Replicas == nil || *spec.Mode.Replicated.Replicas != 1 {
		t.Fatalf("mode = %+v, want replicated 1", spec.Mode)
	}
	if spec.EndpointSpec != nil {
		t.Fatalf("endpoint spec = %+v, want nil (no host ports published)", spec.EndpointSpec)
	}
}

// TestAliasMatchesDerivedEndpoint 端点名不变量：服务的网络 alias 必须等于
// state.RustfsEndpointURL 的 host 段——应用注入面/上传轨都按该名字解析。
func TestAliasMatchesDerivedEndpoint(t *testing.T) {
	host := strings.TrimPrefix(strings.TrimPrefix(state.RustfsEndpointURL, "https://"), "http://")
	if h, _, ok := strings.Cut(host, ":"); ok && h != serviceNetworkAlias {
		t.Fatalf("derived endpoint host %q != network alias %q — injection/clients would resolve a wrong name", h, serviceNetworkAlias)
	}
}

// TestSpecEqualPaths 幂等比对正负路径：相同为真；镜像/env/挂载/网络/约束/
// 副本/限额/凭据 secret 引用任一漂移为假（凭据轮换 = secret 名变化必被捕获）。
func TestSpecEqualPaths(t *testing.T) {
	c := testCreds()
	refs := []secretRef{
		{Name: accessSecretName(c), ID: "id-access", Target: accessKeyFileTarget},
		{Name: secretSecretName(c), ID: "id-secret", Target: secretKeyFileTarget},
	}
	desired := buildSpec("net-1", "n_NODE", c, refs)
	cur, err := inspectOf(desired)
	if err != nil {
		t.Fatalf("inspectOf: %v", err)
	}
	if !specEqual(cur, desired) {
		t.Fatal("identical spec must compare equal")
	}

	drift := func(m func(s *dutydocker.ServiceSnapshot)) bool {
		mutated := cur
		m(&mutated)
		return specEqual(mutated, desired)
	}
	if drift(func(s *dutydocker.ServiceSnapshot) { s.Image = "rustfs/rustfs:other" }) {
		t.Error("image drift not detected")
	}
	if drift(func(s *dutydocker.ServiceSnapshot) { s.Env = []string{"RUSTFS_ADDRESS=:9999"} }) {
		t.Error("env drift not detected")
	}
	if drift(func(s *dutydocker.ServiceSnapshot) { s.Mounts = []mount.Mount{{Source: "other-volume"}} }) {
		t.Error("volume drift not detected")
	}
	if drift(func(s *dutydocker.ServiceSnapshot) { s.Networks = []string{"net-2"} }) {
		t.Error("network drift not detected")
	}
	if drift(func(s *dutydocker.ServiceSnapshot) {
		s.Constraints = []string{"node.labels.fleetly.node-id == n_OTHER"}
	}) {
		t.Error("constraint drift not detected")
	}
	if drift(func(s *dutydocker.ServiceSnapshot) { s.Replicas = 2 }) {
		t.Error("replica drift not detected")
	}
	if drift(func(s *dutydocker.ServiceSnapshot) { s.MemoryBytes = 128 << 20 }) {
		t.Error("memory limit drift not detected")
	}
	if drift(func(s *dutydocker.ServiceSnapshot) {
		s.SecretNames = []string{accessSecretName(c) + "old", secretSecretName(c)}
	}) {
		t.Error("credential rotation (secret name change) not detected")
	}
}

// probeSpecCommand 是测试辅助：构造探针 spec 并取其 restic 子命令（跳过
// 全局选项——与 fakeProbeRunner.probeCommand 同口径）。
func probeSpecCommand(t *testing.T, c credentials, args ...string) string {
	t.Helper()
	m := &Manager{probeRunner: &fakeProbeRunner{}}
	spec := m.probeSpec(context.Background(), c, args...)
	for i := 0; i < len(spec.Args); i++ {
		if strings.HasPrefix(spec.Args[i], "-") {
			if spec.Args[i] == "-o" {
				i++
			}
			continue
		}
		return spec.Args[i]
	}
	return ""
}

// TestGenerateCredentialsShape 凭据字形与强度：access 20 字符官方字形
// （大写字母+数字，无 `/`——SigV4 scope 兼容）；secret 40 hex（160bit）。
func TestGenerateCredentialsShape(t *testing.T) {
	c, err := generateCredentials()
	if err != nil {
		t.Fatalf("generateCredentials: %v", err)
	}
	if len(c.AccessKey) != 20 {
		t.Fatalf("access key length = %d, want 20", len(c.AccessKey))
	}
	if strings.ContainsAny(c.AccessKey, "/") {
		t.Fatalf("access key contains '/' (SigV4 scope unsafe): %q", c.AccessKey)
	}
	for _, r := range c.AccessKey {
		if !strings.ContainsRune(accessKeyAlphabet, r) {
			t.Fatalf("access key rune %q outside the official alphabet", r)
		}
	}
	if len(c.SecretKey) != 40 {
		t.Fatalf("secret key length = %d, want 40", len(c.SecretKey))
	}
	for _, r := range c.SecretKey {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("secret key rune %q not hex", r)
		}
	}
	// 两次生成不同（真随机的下限断言）。
	c2, err := generateCredentials()
	if err != nil {
		t.Fatalf("generateCredentials 2: %v", err)
	}
	if c == c2 {
		t.Fatal("two generations produced identical credentials")
	}
}

// TestProbeRepoURLAndEnv 探针容器视角的 repo 地址与 env：repo 用规范端点
// （容器内 DNS 解析服务 alias——不经宿主拨号），path-style 显式，口令由
// 托管 secret 派生且与凭据不同。
func TestProbeRepoURLAndEnv(t *testing.T) {
	c := testCreds()
	env := probeEnv(c, probeRepoURL(probeRepoPathSuffix))
	if env["RESTIC_REPOSITORY"] != "s3:"+state.RustfsEndpointURL+"/"+state.RustfsBucketName+"/"+probeRepoPathSuffix {
		t.Fatalf("repo = %q, want canonical in-network endpoint", env["RESTIC_REPOSITORY"])
	}
	if env["AWS_ACCESS_KEY_ID"] != c.AccessKey || env["AWS_SECRET_ACCESS_KEY"] != c.SecretKey {
		t.Fatal("probe env must carry the managed credentials")
	}
	if env["RESTIC_PASSWORD"] == "" || env["RESTIC_PASSWORD"] == c.SecretKey {
		t.Fatal("repo password must be derived (present and distinct from the secret key)")
	}
	if env["RESTIC_PASSWORD"] != probeRepoPassword(c) {
		t.Fatal("repo password must be the deterministic derivation of the managed secret key")
	}
	if probeSpecCommand(t, c, "backup", "/etc") != "backup" {
		t.Fatal("probe spec must append the restic subcommand after global options")
	}
}
