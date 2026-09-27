package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// TestNamesMatchDesignDocs 验收 5：命名函数输出与设计文档字符串逐一相等
// （表驱动，文档原文钉死——state-model §2.4 对象命名 + architecture §2.4
// 服务命名与网络行 + rbac-teams §4.3 公式表 v0.3 三段形）。任何一侧改动
// 都必须先改文档再改此处。
func TestNamesMatchDesignDocs(t *testing.T) {
	const hashInput = "super-secret-value"
	sum := sha256.Sum256([]byte(hashInput))
	wantHash8 := hex.EncodeToString(sum[:])[:8]

	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			// rbac-teams §4.3 服务行：`fleetly-<team>-<prj>-<app>-<service>`。
			name: "service name",
			got:  must(t, func() (string, error) { return ServiceName("acme", "prod", "my-api", "web") }),
			want: "fleetly-acme-prod-my-api-web",
		},
		{
			// rbac-teams §4.3 secret 行：`fleetly-<team>-<prj>-<app>-<name>-<hash8>`。
			name: "secret name",
			got:  must(t, func() (string, error) { return SecretName("acme", "prod", "my-api", "database_url", wantHash8) }),
			want: "fleetly-acme-prod-my-api-database_url-" + wantHash8,
		},
		{
			// T 线 OT-3 config 行：`fleetly-<team>-<prj>-<app>-config-<name>-<hash8>`
			// （内容寻址；hash8 与 secret 同口径 = 值 sha256 前 8）。
			name: "config name",
			got:  must(t, func() (string, error) { return ConfigName("acme", "prod", "my-api", "app.yaml", wantHash8) }),
			want: "fleetly-acme-prod-my-api-config-app.yaml-" + wantHash8,
		},
		{
			// rbac-teams §4.3 app 卷行「不变」：`fleetly-<app>-<key>-<appid8>`。
			name: "volume name (v0.3 formula unchanged)",
			got:  must(t, func() (string, error) { return VolumeName("my-api", "data", "01JABCDEFGH") }),
			want: "fleetly-my-api-data-01JABCDE",
		},
		{
			// rbac-teams §4.3 app 网络行：`fleetly-<team>-<prj>-<app>-net`。
			name: "network name",
			got:  must(t, func() (string, error) { return NetworkName("acme", "prod", "my-api") }),
			want: "fleetly-acme-prod-my-api-net",
		},
		{
			// 服务别名 = compose 服务名（app 内短名互访；v0.3 不变）。
			name: "network alias",
			got:  must(t, func() (string, error) { return NetworkAlias("web") }),
			want: "web",
		},
		{
			// 流标签口径（rbac-teams §4.3 流标签行）：三段限定形。
			name: "qualified name",
			got:  must(t, func() (string, error) { return QualifiedName("acme", "prod", "my-api") }),
			want: "acme/prod/my-api",
		},
		{
			name: "hash8 = sha256(content)[:8]",
			got:  Hash8(hashInput),
			want: wantHash8,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q (design doc verbatim)", tc.got, tc.want)
			}
		})
	}
}

// TestCronJobNameThreeSegment E5 Cron × rbac-teams §4.3 cron 行：一次性 job
// 服务名 `fleetly-cron-<team>-<prj>-<app>-<svc>-<ulid8>`；前缀族不变
// （IsCronJobName 对新旧前缀族成员的识别面一致）。
func TestCronJobNameThreeSegment(t *testing.T) {
	name, err := CronJobName("acme", "prod", "my-api", "cleanup", "01JABCDEFGHJKMNPQRSTVWX")
	if err != nil {
		t.Fatalf("CronJobName: %v", err)
	}
	if name != "fleetly-cron-acme-prod-my-api-cleanup-01JABCDE" {
		t.Fatalf("CronJobName = %q, want the three-segment cron family form", name)
	}
	if !IsCronJobName(name) {
		t.Fatalf("IsCronJobName(%q) = false, want true", name)
	}
	if !IsCronJobName("fleetly-cron-app-svc-01JABCDE") {
		t.Fatal("IsCronJobName must keep recognizing the bare prefix family")
	}
	if IsCronJobName("fleetly-acme-prod-my-api-cleanup") {
		t.Fatal("long-running service name must not match the cron job prefix")
	}
}

// TestInitJobNameThreeSegment DT-4：部署期 init job 服务名
// `fleetly-init-<team>-<prj>-<app>-<svc>-<deployid8>`；前缀族独立于 cron
// （互不误伤是两族清扫/对账豁免的正确性前提）。
func TestInitJobNameThreeSegment(t *testing.T) {
	name, err := InitJobName("acme", "prod", "my-api", "migrate", "01JABCDEFGHJKMNPQRSTVWX")
	if err != nil {
		t.Fatalf("InitJobName: %v", err)
	}
	if name != "fleetly-init-acme-prod-my-api-migrate-01JABCDE" {
		t.Fatalf("InitJobName = %q, want the three-segment init family form", name)
	}
	if !IsInitJobName(name) {
		t.Fatalf("IsInitJobName(%q) = false, want true", name)
	}
	if IsCronJobName(name) || IsInitJobName("fleetly-cron-acme-prod-my-api-migrate-01JABCDE") {
		t.Fatal("init and cron prefix families must not cross-match (independent sweeps)")
	}
	if IsInitJobName("fleetly-acme-prod-my-api-migrate") {
		t.Fatal("long-running service name must not match the init job prefix")
	}
	for _, tc := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"empty team", func() (string, error) { return InitJobName("", "prj", "app", "migrate", "01JABCDEFGH") }},
		{"empty service", func() (string, error) { return InitJobName("t", "p", "app", "", "01JABCDEFGH") }},
		{"short deployment id", func() (string, error) { return InitJobName("t", "p", "app", "migrate", "01JA") }},
	} {
		if _, err := tc.fn(); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

// TestHash8RotationIsNewName 验收 2 的命名侧：值轮换即换名换引用（架构
// §2.4 密钥行——引用参与 desired-hash，轮换天然触发重部署）。
func TestHash8RotationIsNewName(t *testing.T) {
	old := must(t, func() (string, error) { return SecretName("t", "p", "app", "k", Hash8("value-v1")) })
	rotated := must(t, func() (string, error) { return SecretName("t", "p", "app", "k", Hash8("value-v2")) })
	if old == rotated {
		t.Fatalf("rotated secret name unchanged: %s", old)
	}
	if !strings.HasPrefix(old, "fleetly-t-p-app-k-") || !strings.HasPrefix(rotated, "fleetly-t-p-app-k-") {
		t.Fatalf("name prefix broken: %s / %s", old, rotated)
	}
	// OT-3 config 同律：内容变更即换名换引用（服务滚动的内容寻址面）。
	oldCfg := must(t, func() (string, error) { return ConfigName("t", "p", "app", "k", Hash8("content-v1")) })
	newCfg := must(t, func() (string, error) { return ConfigName("t", "p", "app", "k", Hash8("content-v2")) })
	if oldCfg == newCfg {
		t.Fatalf("rotated config name unchanged: %s", oldCfg)
	}
	if !strings.HasPrefix(oldCfg, "fleetly-t-p-app-config-k-") {
		t.Fatalf("config name prefix broken: %s", oldCfg)
	}
}

// TestSameNameAcrossProjects D-W0-4 二修的核心性质：两个项目各有同名 app，
// 底座命名零冲突（team·prj 段承载全局唯一）；卷名族不含 team/prj 段、
// appid8 尾缀防撞（零卷迁移的前提）。
func TestSameNameAcrossProjects(t *testing.T) {
	a, err := ServiceName("alpha", "prod", "web", "web")
	if err != nil {
		t.Fatalf("service name alpha/prod: %v", err)
	}
	b, err := ServiceName("beta", "prod", "web", "web")
	if err != nil {
		t.Fatalf("service name beta/prod: %v", err)
	}
	if a == b {
		t.Fatalf("same-named apps in different projects collide: %s", a)
	}
	va, _ := VolumeName("web", "data", "01JAAAAAAAAA")
	vb, _ := VolumeName("web", "data", "01JBBBBBBBBB")
	if va == vb {
		t.Fatalf("volume names collide across apps: %s", va)
	}
	if strings.Contains(va, "alpha") {
		t.Fatalf("volume formula must stay team/prj-free: %s", va)
	}
}

// TestServiceLabelsMinimalSet 验收 1 的 label 构造器：最小集六键
// （state-model §2.4 Service 行 + rbac-teams §4.3 增 fleetly.team /
// fleetly.project），值逐一对照；fleetly.app 值 = 三段限定形。
func TestServiceLabelsMinimalSet(t *testing.T) {
	labels, err := ServiceLabels("my-team", "my-prj", "my-api", "web", "dep_01")
	if err != nil {
		t.Fatalf("ServiceLabels: %v", err)
	}
	want := map[string]string{
		state.LabelManaged:    state.ManagedLabelValue,
		state.LabelApp:        "my-team/my-prj/my-api",
		state.LabelProcess:    "web",
		state.LabelDeployment: "dep_01",
		state.LabelTeam:       "my-team",
		state.LabelProject:    "my-prj",
	}
	if len(labels) != len(want) {
		t.Fatalf("label count = %d, want %d (minimal set)", len(labels), len(want))
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, labels[k], v)
		}
	}

	// 稳定序列化：SortedServiceLabels 键字典序。
	sorted := SortedServiceLabels(labels)
	if len(sorted) != len(want) {
		t.Fatalf("sorted labels length = %d, want %d", len(sorted), len(want))
	}
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1][0] > sorted[i][0] {
			t.Fatalf("sorted labels not ordered: %v", sorted)
		}
	}

	// 容器 label 仅 fleetly.app（值 = 三段限定形）。
	cl, err := ContainerLabels("my-team", "my-prj", "my-api")
	if err != nil || len(cl) != 1 || cl[state.LabelApp] != "my-team/my-prj/my-api" {
		t.Fatalf("container labels = %v err=%v", cl, err)
	}
}

// TestNameValidation 非法成分拒绝（空串、越界字符、分隔层级注入）。
func TestNameValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"empty team", func() (string, error) { return ServiceName("", "prj", "app", "web") }},
		{"empty prj", func() (string, error) { return NetworkName("team", "", "app") }},
		{"empty app", func() (string, error) { return ServiceName("team", "prj", "", "web") }},
		{"empty service", func() (string, error) { return ServiceName("team", "prj", "app", "") }},
		{"slash injection", func() (string, error) { return ServiceName("a/b", "prj", "app", "web") }},
		{"colon injection", func() (string, error) { return ServiceName("team", "prj", "app", "we:b") }},
		{"space", func() (string, error) { return VolumeName("app", "da ta", "01JABCDEFGH") }},
		{"short appid", func() (string, error) { return VolumeName("app", "data", "01JA") }},
		{"empty hash8", func() (string, error) { return SecretName("team", "prj", "app", "k", "") }},
		{"uppercase hash8", func() (string, error) { return SecretName("team", "prj", "app", "k", "ABCDEF12") }},
		{"empty config hash8", func() (string, error) { return ConfigName("team", "prj", "app", "k", "") }},
		{"short config hash8", func() (string, error) { return ConfigName("team", "prj", "app", "k", "abc123") }},
		{"empty config name", func() (string, error) { return ConfigName("team", "prj", "app", "", "abc12345") }},
		{"empty deployment", func() (string, error) { _, err := ServiceLabels("team", "prj", "app", "web", ""); return "", err }},
		{"qualified empty name", func() (string, error) { return QualifiedName("team", "prj", "") }},
	} {
		if _, err := tc.fn(); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

// must 是表驱动夹具的构造 helper（失败即测试失败）。
func must(t *testing.T, fn func() (string, error)) string {
	t.Helper()
	got, err := fn()
	if err != nil {
		t.Fatalf("construct name: %v", err)
	}
	return got
}

// TestReservedTeamSlugs 保留字清单钉死（rbac-teams §4.3 保留字迁移：v0.2
// 的 8 个 app 名保留字迁到 team slug；D-W0-4 迁移裁决 + W3 撞键票审计证据
// 链随迁 + DT-4 增 init）：每个保留 slug 各带撞键证据；清单字典序稳定
// （错误文案可重放）；非保留名（近名形态与普通名）不误伤；app 名清单已随
// 迁移退役（结构性安全——app 名不再紧邻 fleetly- 前缀，E_APP_NAME_RESERVED
// 退役）。
func TestReservedTeamSlugs(t *testing.T) {
	want := []string{"acme", "cron", "db", "dbjob", "init", "metrics", "registry", "rustfs", "victorialogs"}
	got := ReservedTeamSlugs()
	if len(got) != len(want) {
		t.Fatalf("reserved set = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reserved set = %v, want sorted %v", got, want)
		}
	}
	for _, name := range want {
		if !IsReservedTeamSlug(name) {
			t.Errorf("IsReservedTeamSlug(%q) = false, want true", name)
		}
		if ReservedTeamSlugReason(name) == "" {
			t.Errorf("ReservedTeamSlugReason(%q) empty: every reserved slug must carry its collision evidence", name)
		}
	}
	for _, name := range []string{"demo", "ingress", "exec", "system", "console", "victoriametrics", "cadvisor", "node-exporter", "cronapp", "dbapp", "initapp"} {
		if IsReservedTeamSlug(name) {
			t.Errorf("IsReservedTeamSlug(%q) = true, audit proved no collision (reserved set must stay minimal)", name)
		}
	}
}
