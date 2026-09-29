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
			// OT-1/IMPL-T15-1 项目网行：`fleetly-project-<projectID>`（组件网
			// 固定前缀 + 项目平台 ID **全量**——ULID 前 8 只有 40 位时间戳
			// 成分、256ms 窗口内撞名，截断即网络静默合并风险；设计档 OT-1
			// 命名公式本体不变、项目段变更继续挂账——本行是新增对象族）。
			name: "project network name",
			got:  must(t, func() (string, error) { return ProjectNetworkName("01JABCDE9Z8Y7X6W5V4T3S2R1Q") }),
			want: "fleetly-project-01JABCDE9Z8Y7X6W5V4T3S2R1Q",
		},
		{
			// OT-1 蓝图原条款：项目网别名 = `<app>-<service>`（短名仅 app
			// 私网——别名隔离守卫④）。
			name: "project network alias",
			got:  must(t, func() (string, error) { return ProjectNetworkAlias("my-api", "web") }),
			want: "my-api-web",
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

// TestProjectNetworkNameCannotCollideWithAppNetwork OT-1：项目网前缀族
// `fleetly-project-<projectID>`（ID 全量 = ULID Crockford：大写字母+数字）
// 与三段 slug 网络名 `fleetly-<team>-<prj>-<app>-net`（[a-z0-9._-] 字符集）
// 结构不相交——即便 team slug 取 `project`（合法单词制），产物含小写/'-'/
// 'net' 尾，不可能等于前缀 + 纯 Crockford 尾段。保留字清单因此无需扩
// `project`。截断撞名的反证：两个 256ms 窗口内创建的项目 ID 前 8 位相同
// （时间戳高位相同）——本测试以同前缀双 ID 钉死「全量 ID 才唯一」。
func TestProjectNetworkNameCannotCollideWithAppNetwork(t *testing.T) {
	const projectID = "01JABCDE9Z8Y7X6W5V4T3S2R1Q"
	projectNet := must(t, func() (string, error) { return ProjectNetworkName(projectID) })
	if projectNet != "fleetly-project-"+projectID {
		t.Fatalf("project network name = %q, want fleetly-project-<full project id>", projectNet)
	}
	if !IsProjectNetworkName(projectNet) {
		t.Fatalf("IsProjectNetworkName(%q) = false, want true", projectNet)
	}
	// 同 256ms 窗口内创建的两个项目：ULID 前 8 位相同（时间戳高位），
	// 随机尾段不同——项目网名必须仍互异（截断方案会撞名的反证）。
	const sameWindowID = "01JABCDE9Z8Y7X6W5V4T3S2R1R"
	if sameWindowID[:8] != projectID[:8] {
		t.Fatalf("fixture error: %q and %q must share the first 8 chars", projectID, sameWindowID)
	}
	otherNet := must(t, func() (string, error) { return ProjectNetworkName(sameWindowID) })
	if otherNet == projectNet {
		t.Fatalf("project networks of same-window projects collide: %s (full id required)", projectNet)
	}
	// team slug = project 的最凶形态（同为 fleetly-project- 前缀），遍历
	// slug/app 组合断言不误判为项目网、也不等值。
	for _, prj := range []string{"ab", "prod", "abcdefgh"} {
		for _, app := range []string{"c", "web", "backend-1"} {
			appNet := must(t, func() (string, error) { return NetworkName("project", prj, app) })
			if appNet == projectNet {
				t.Fatalf("app network %q equals the project network name (collision)", appNet)
			}
			if IsProjectNetworkName(appNet) {
				t.Fatalf("IsProjectNetworkName(%q) = true for an app network (cross-family misjudgment)", appNet)
			}
		}
	}
	// 近名负路径：lowercase/含 '-'/空尾/非 Crockford 一律不判项目网。
	for _, name := range []string{
		"fleetly-project-01jabcde9z8y7x6w5v4t3s2r1q", // 小写
		"fleetly-project-01JABCDE",                   // 截断形态（非全量 ID）
		"fleetly-project-01JABCD-",                   // 非 Crockford
		"fleetly-project-",                           // 空尾
		"fleetly-project-web",                        // 非 ID 形态
	} {
		if IsProjectNetworkName(name) {
			t.Errorf("IsProjectNetworkName(%q) = true, want false", name)
		}
	}
	// 组件网/库网/既有族不误判。
	for _, name := range []string{"fleetly-system", "fleetly-rustfs-net", "fleetly-acme-prod-web-net", "fleetly-db-acme-prod-pg-net", "fleetly-cron-acme-prod-web-x-01JABCDE"} {
		if IsProjectNetworkName(name) {
			t.Errorf("IsProjectNetworkName(%q) = true, want false", name)
		}
	}
}

// TestProjectNetworkAliasIsolationContract 别名隔离的命名侧契约（守卫④）：
// app 私网别名 = 短名（仅服务名）；项目网别名 = <app>-<service>；两者在
// 同名服务上必不相等。非法成分拒绝同 NetworkAlias 纪律。
func TestProjectNetworkAliasIsolationContract(t *testing.T) {
	short := must(t, func() (string, error) { return NetworkAlias("web") })
	long := must(t, func() (string, error) { return ProjectNetworkAlias("my-api", "web") })
	if short != "web" || long != "my-api-web" || short == long {
		t.Fatalf("alias contract broken: short=%q long=%q", short, long)
	}
	for _, tc := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"empty app", func() (string, error) { return ProjectNetworkAlias("", "web") }},
		{"empty service", func() (string, error) { return ProjectNetworkAlias("app", "") }},
		{"slash app", func() (string, error) { return ProjectNetworkAlias("a/b", "web") }},
		{"colon service", func() (string, error) { return ProjectNetworkAlias("app", "we:b") }},
		{"empty project id", func() (string, error) { return ProjectNetworkName("") }},
		{"short project id", func() (string, error) { return ProjectNetworkName("01JA") }},
		{"lowercase project id", func() (string, error) { return ProjectNetworkName("01jabcde9z8y7x6w5v4t3s2r1q") }},
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
	want := []string{"acme", "cron", "db", "dbjob", "init", "metrics", "registry", "rustfs", "taskgroup", "victorialogs"}
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
	for _, name := range []string{"demo", "ingress", "exec", "system", "console", "victoriametrics", "cadvisor", "node-exporter", "cronapp", "dbapp", "initapp", "project"} {
		if IsReservedTeamSlug(name) {
			t.Errorf("IsReservedTeamSlug(%q) = true, audit proved no collision (reserved set must stay minimal)", name)
		}
	}
}

// TestConfigNameCapsAtSwarmLimit 内容寻址名族的 64 上限守卫(2026-09-29
// staging 真机回归:两个真实撞例——messageloop/mlbridge.yaml 原始拼接 65 字
// 符、torchwood/bootstrap-runtime.sql 73 字符,swarm config create 直接
// InvalidArgument)。断言:①两撞例产物 ≤64 且以 hash8 尾段收尾、固定段完
// 整;②确定性(同输入同输出);③内容寻址语义不变——同名不同 hash8 仍互异;
//④短名产物与既有公式逐字一致(截断只在超预算时发生)。
func TestConfigNameCapsAtSwarmLimit(t *testing.T) {
	const h = "0123abcd"
	cases := []struct {
		team, prj, app, name string
	}{
		{"founder", "default", "messageloop", "mlbridge.yaml"},
		{"founder", "default", "torchwood", "bootstrap-runtime.sql"},
	}
	for _, tc := range cases {
		got, err := ConfigName(tc.team, tc.prj, tc.app, tc.name, h)
		if err != nil {
			t.Fatalf("ConfigName(%q,%q,%q,%q): %v", tc.team, tc.prj, tc.app, tc.name, err)
		}
		if len(got) > 64 {
			t.Fatalf("ConfigName(%q) = %q at %d chars, exceeds the 64-char swarm limit", tc.name, got, len(got))
		}
		if !strings.HasPrefix(got, "fleetly-"+tc.team+"-"+tc.prj+"-"+tc.app+"-config-") {
			t.Fatalf("ConfigName(%q) = %q, fixed prefix segments damaged", tc.name, got)
		}
		if !strings.HasSuffix(got, "-"+h) {
			t.Fatalf("ConfigName(%q) = %q, hash8 tail lost (content addressing broken)", tc.name, got)
		}
		again, err := ConfigName(tc.team, tc.prj, tc.app, tc.name, h)
		if err != nil || again != got {
			t.Fatalf("ConfigName not deterministic: %q vs %q (err %v)", got, again, err)
		}
	}
	other, err := ConfigName("founder", "default", "messageloop", "mlbridge.yaml", "4567bcde")
	if err != nil {
		t.Fatalf("ConfigName(other hash): %v", err)
	}
	same, err := ConfigName("founder", "default", "messageloop", "mlbridge.yaml", h)
	if err != nil {
		t.Fatalf("ConfigName(same hash): %v", err)
	}
	if other == same {
		t.Fatalf("distinct content hashes produced the same config name %q: content addressing broken by capping", other)
	}
	short, err := ConfigName("a", "b", "c", "d", h)
	if err != nil {
		t.Fatalf("ConfigName(short): %v", err)
	}
	if want := "fleetly-a-b-c-config-d-" + h; short != want {
		t.Fatalf("ConfigName(short) = %q, want %q (short names must keep the exact formula)", short, want)
	}
}

// TestSecretNameCapsAtSwarmLimit 内容寻址 secret 名族同款守卫(app 与库两系)。
func TestSecretNameCapsAtSwarmLimit(t *testing.T) {
	const h = "0123abcd"
	long := strings.Repeat("s", 60)
	got, err := SecretName("team", "prj", "app", long, h)
	if err != nil {
		t.Fatalf("SecretName(long): %v", err)
	}
	if len(got) > 64 || !strings.HasSuffix(got, "-"+h) {
		t.Fatalf("SecretName(long) = %q: must be ≤64 with the hash8 tail intact", got)
	}
	dbGot, err := DBSecretName("team", "prj", "dbinst", long, h)
	if err != nil {
		t.Fatalf("DBSecretName(long): %v", err)
	}
	if len(dbGot) > 64 || !strings.HasSuffix(dbGot, "-"+h) {
		t.Fatalf("DBSecretName(long) = %q: must be ≤64 with the hash8 tail intact", dbGot)
	}
	short, err := SecretName("a", "b", "c", "d", h)
	if err != nil {
		t.Fatalf("SecretName(short): %v", err)
	}
	if want := "fleetly-a-b-c-d-" + h; short != want {
		t.Fatalf("SecretName(short) = %q, want %q", short, want)
	}
}

// TestAddressableNameLengthGuards 可寻址名族(被调用方/CLI/对账按名引用)
// 超长显式报错——不截断(截断会切断引用链),错误点名公式与 64 上限;短名
// 不受影响(既有 golden 测试覆盖)。瞬时 job 名(cron/init/dbjob)是第三类:
// 前缀识别 + label 归属、名字不参与反解——service/purpose 段截断( ≤64),
// 单独断言。
func TestAddressableNameLengthGuards(t *testing.T) {
	longSlug := strings.Repeat("n", 50)
	longApp := strings.Repeat("a", 50)
	cases := []struct {
		name string
		call func() (string, error)
	}{
		{"ServiceName", func() (string, error) { return ServiceName("team", "prj", longApp, "web") }},
		{"NetworkName", func() (string, error) { return NetworkName("team", "prj", longApp) }},
		{"VolumeName", func() (string, error) { return VolumeName("app", longSlug, "01M3N588") }},
		{"DBServiceName", func() (string, error) { return DBServiceName("team", "prj", longSlug, "postgres") }},
		{"DBNetworkName", func() (string, error) { return DBNetworkName("team", "prj", longSlug) }},
		{"DBVolumeName", func() (string, error) { return DBVolumeName(longSlug, "data", "01M3N588") }},
	}
	for _, tc := range cases {
		got, err := tc.call()
		if err == nil {
			t.Fatalf("%s returned %q (len %d): overflow past the 64-char swarm limit must error", tc.name, got, len(got))
		}
		if !strings.Contains(err.Error(), "swarm object name limit") {
			t.Fatalf("%s error = %v: must name the swarm object name limit", tc.name, err)
		}
	}
	ok, err := ServiceName("team", "prj", "app", "web")
	if err != nil || ok != "fleetly-team-prj-app-web" {
		t.Fatalf("ServiceName(short) = %q, err %v: short names must keep the exact formula", ok, err)
	}
}

// TestJobNameCapsAtSwarmLimit 瞬时 job 名族截断:超预算时 service/purpose 段
// 收短、id8 尾段与前缀族完整;固定段本身顶满(预算 <1)显式报错;短名逐字
// 不变。三公式固定开销不同(cron/init 四段、dbjob 三段),夹具按各自预算取。
func TestJobNameCapsAtSwarmLimit(t *testing.T) {
	cron, err := CronJobName("team", "prj", strings.Repeat("a", 30), "web", "0123456789")
	if err != nil {
		t.Fatalf("CronJobName(long): %v", err)
	}
	if len(cron) > 64 || !strings.HasPrefix(cron, "fleetly-cron-team-prj-") || !strings.HasSuffix(cron, "-01234567") {
		t.Fatalf("CronJobName(long) = %q: must be ≤64 with prefix family and id8 tail intact", cron)
	}
	init, err := InitJobName("team", "prj", strings.Repeat("a", 30), "web", "0123456789")
	if err != nil {
		t.Fatalf("InitJobName(long): %v", err)
	}
	if len(init) > 64 || !strings.HasPrefix(init, "fleetly-init-team-prj-") || !strings.HasSuffix(init, "-01234567") {
		t.Fatalf("InitJobName(long) = %q: must be ≤64 with prefix family and id8 tail intact", init)
	}
	job, err := DBJobName(strings.Repeat("n", 39), "backup", "0123456789")
	if err != nil {
		t.Fatalf("DBJobName(long): %v", err)
	}
	if len(job) > 64 || !strings.HasPrefix(job, "fleetly-dbjob-") || !strings.HasSuffix(job, "-01234567") {
		t.Fatalf("DBJobName(long) = %q: must be ≤64 with prefix family and id8 tail intact", job)
	}
	if _, err := CronJobName("team", "prj", strings.Repeat("a", 60), "web", "0123456789"); err == nil ||
		!strings.Contains(err.Error(), "no room for the name segment") {
		t.Fatalf("CronJobName(pathological) error = %v: fixed segments filling the limit must error explicitly", err)
	}
	short, err := InitJobName("acme", "prod", "cronapp", "migrate", "01JABCDEFGHJKMNPQRSTVWX")
	if err != nil {
		t.Fatalf("InitJobName(short): %v", err)
	}
	if want := "fleetly-init-acme-prod-cronapp-migrate-01JABCDE"; short != want {
		t.Fatalf("InitJobName(short) = %q, want %q", short, want)
	}
}
