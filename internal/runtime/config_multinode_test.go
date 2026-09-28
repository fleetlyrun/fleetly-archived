package runtime

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/build"
)

// E1-1 安装项与配置的多节点键位测试（E1 多节点设计 §2.2/§2.3/§2.4/§2.5）：
//   - join.token_rotate 缺省 auto（D-MN-1），manual 显式 opt-out，未知值
//     一律回落 auto（保守缺省 = 安全默认）；
//   - registry.auth_file 回落 <数据根>/fleetly-registry.auth（§2.5，与
//     ingress token 同形的 0600 凭据文件；数据根 = state 库同目录）；
//   - ingress.config_tls_addr 经 IngressSettings 透传给 ingress.Config
//     （缺省 0.0.0.0:8423 的回落单一事实源在 internal/ingress.Normalize；
//     装配期透传纪律与 ConfigAddr 同款——不经 Normalize 的空串会漂移）。
//   - ingress.base_domain 经 IngressSettings 透传（E1-3 接线：启用判定
//     在 Manager.ConfigTLSEnabled；本包不派生子域）。
//   - base_domain 是透传字符串（V2-7）：空 = 单节点 v0.1 形态。

func TestJoinTokenRotateDefaults(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty-defaults-to-auto", "", "auto"},
		{"auto-explicit", "auto", "auto"},
		{"manual-explicit", "manual", "manual"},
		{"manual-uppercase-falls-back", "MANUAL", "auto"},
		{"unknown-falls-back", "sometimes", "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := AppConfig{Join: JoinConfig{TokenRotate: tc.in}}
			if got := c.JoinTokenRotate(); got != tc.want {
				t.Errorf("JoinTokenRotate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRegistryAuthFileDefault(t *testing.T) {
	c := AppConfig{State: StateConfig{DBPath: "/var/lib/fleetly/fleetly.db"}}
	if got, want := c.RegistryAuthFile(), filepath.Join("/var/lib/fleetly", "fleetly-registry.auth"); got != want {
		t.Errorf("RegistryAuthFile() default = %q, want %q", got, want)
	}
}

func TestRegistryAuthFileExplicit(t *testing.T) {
	c := AppConfig{
		State:    StateConfig{DBPath: "/var/lib/fleetly/fleetly.db"},
		Registry: RegistryConfig{AuthFile: "/etc/fleetly/registry.auth"},
	}
	if got := c.RegistryAuthFile(); got != "/etc/fleetly/registry.auth" {
		t.Errorf("RegistryAuthFile() explicit = %q, want /etc/fleetly/registry.auth", got)
	}
}

func TestIngressSettingsPassesConfigTLSAddr(t *testing.T) {
	explicitCfg := AppConfig{Ingress: IngressConfig{ConfigTLSAddr: "10.0.0.5:18423"}}
	explicit := explicitCfg.IngressSettings()
	if explicit.ConfigTLSAddr != "10.0.0.5:18423" {
		t.Errorf("IngressSettings().ConfigTLSAddr explicit = %q, want 10.0.0.5:18423", explicit.ConfigTLSAddr)
	}

	emptyCfg := AppConfig{}
	empty := emptyCfg.IngressSettings()
	if empty.ConfigTLSAddr != "0.0.0.0:8423" {
		t.Errorf("IngressSettings().ConfigTLSAddr default = %q, want 0.0.0.0:8423 (ingress.Normalize fallback)", empty.ConfigTLSAddr)
	}
}

// TestIngressSettingsPassesBaseDomain base_domain 透传（E1-3）：非空原样
// 进入 ingress.Config（启用 8423 TLS 面 + 平台证书 duty 的判定输入）；空 =
// 单节点形态（ingress 侧零行为差异）。
func TestIngressSettingsPassesBaseDomain(t *testing.T) {
	setCfg := AppConfig{BaseDomain: "example.com"}
	set := setCfg.IngressSettings()
	if set.BaseDomain != "example.com" {
		t.Errorf("IngressSettings().BaseDomain = %q, want example.com", set.BaseDomain)
	}
	emptyCfg := AppConfig{}
	empty := emptyCfg.IngressSettings()
	if empty.BaseDomain != "" {
		t.Errorf("IngressSettings().BaseDomain empty = %q, want empty string", empty.BaseDomain)
	}
}

// E1-4/E1-5 registry 装配接线测试：base_domain 是 registry host 派生与
// 构建管线模式切换的唯一开关（空 = 本地模式 v0.1 逐字等价）。

func TestRegistryHostDerivedFromBaseDomain(t *testing.T) {
	set := AppConfig{BaseDomain: "example.com"}
	if got := set.RegistryHost(); got != "registry.example.com" {
		t.Errorf("RegistryHost() = %q, want registry.example.com", got)
	}
	empty := AppConfig{}
	if got := empty.RegistryHost(); got != "" {
		t.Errorf("RegistryHost() empty base = %q, want empty string", got)
	}
}

func TestBuildSettingsRegistryMode(t *testing.T) {
	// base_domain 非空：构建管线切 registry 模式（host 派生 + 凭据文件
	// 回落数据根形态——构建执行时点现读）。
	setCfg := AppConfig{
		BaseDomain: "example.com",
		State:      StateConfig{DBPath: "/var/lib/fleetly/fleetly.db"},
	}
	b := setCfg.BuildSettings()
	if b.RegistryHost != "registry.example.com" {
		t.Errorf("BuildSettings().RegistryHost = %q", b.RegistryHost)
	}
	if want := filepath.Join("/var/lib/fleetly", "fleetly-registry.auth"); b.RegistryAuthFile != want {
		t.Errorf("BuildSettings().RegistryAuthFile = %q, want %q", b.RegistryAuthFile, want)
	}
	// base_domain 空：本地模式（零 registry 字段——v0.1 管线逐字不变）。
	empty := AppConfig{State: StateConfig{DBPath: "/var/lib/fleetly/fleetly.db"}}
	eb := empty.BuildSettings()
	if eb.RegistryHost != "" || eb.RegistryAuthFile != "" {
		t.Errorf("local-mode build settings must not carry registry fields, got host=%q auth=%q",
			eb.RegistryHost, eb.RegistryAuthFile)
	}
}

// TestBuildSettingsUploadsRootAndLimit IMPL-T2-2：上传会话根从数据根派生
// 并恒并入受管根（builder 执行侧复核的锚点）；build.max_upload_mb 显式
// 配置生效、缺省回落 256MiB。
func TestBuildSettingsUploadsRootAndLimit(t *testing.T) {
	cfg := AppConfig{State: StateConfig{DBPath: filepath.Join("data", "fleetly.db")}}
	b := cfg.BuildSettings()
	if filepath.Base(b.UploadsRoot) != "build-uploads" || !filepath.IsAbs(b.UploadsRoot) {
		t.Fatalf("UploadsRoot = %q, want an absolute <data root>/build-uploads path", b.UploadsRoot)
	}
	if b.MaxUploadBytes != build.DefaultMaxUploadBytes {
		t.Fatalf("MaxUploadBytes = %d, want default %d", b.MaxUploadBytes, build.DefaultMaxUploadBytes)
	}
	found := false
	for _, root := range b.ContextRoots {
		if root == b.UploadsRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("UploadsRoot %q not merged into ContextRoots %v", b.UploadsRoot, b.ContextRoots)
	}

	cfg.Build.MaxUploadMB = 64
	if got := cfg.BuildSettings().MaxUploadBytes; got != 64<<20 {
		t.Fatalf("MaxUploadBytes with max_upload_mb=64 = %d, want %d", got, int64(64)<<20)
	}
}

func TestIngressSettingsRegistryFields(t *testing.T) {	setCfg := AppConfig{
		BaseDomain: "example.com",
		State:      StateConfig{DBPath: "/var/lib/fleetly/fleetly.db"},
	}
	ing := setCfg.IngressSettings()
	if want := filepath.Join("/var/lib/fleetly", "fleetly-registry.auth"); ing.RegistryAuthFile != want {
		t.Errorf("IngressSettings().RegistryAuthFile = %q, want %q", ing.RegistryAuthFile, want)
	}
	// registry.image 显式配置优先（钉版换版票的配置面）。
	setCfg.Registry.Image = "ghcr.io/project-zot/zot:v9.9.9@sha256:" + strings.Repeat("a", 64)
	if got := setCfg.IngressSettings().RegistryImage; got != setCfg.Registry.Image {
		t.Errorf("IngressSettings().RegistryImage = %q, want explicit override", got)
	}
	// 未配置 = ingress.Normalize 回落钉版缺省（digest 台账为真源）。
	if got := ing.RegistryImage; !strings.HasPrefix(got, "ghcr.io/project-zot/zot:") || !strings.Contains(got, "@sha256:") {
		t.Errorf("IngressSettings().RegistryImage default = %q, want pinned zot digest form", got)
	}
}
