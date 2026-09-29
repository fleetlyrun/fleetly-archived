package engine

// S3 注入面测试（E3-4，设计 §2.4）：system env 注入（键集/值/仅带 label
// 服务）、rustfs 网络牵线（external 不加）、快照/desired-hash 参与（脱敏
// 形态——source=system）、W_ENV_PLATFORM_OVERRIDE 同键警告、前置校验
// E_S3_NOT_CONFIGURED（unset 诚实拒绝）与托管凭据未备便的可重试哨兵。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/envlayer"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/state"
)

const s3LabelCompose = `name: s3app
services:
  web:
    image: repo/web:1
    labels:
      fleetly.s3: "true"
  worker:
    image: repo/worker:1
`

func planInputFor(t *testing.T, yaml string, mutate func(in *PlanInput)) (*Plan, error) {
	t.Helper()
	spec, _, err := compose.Load(context.Background(), writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("compose load: %v", err)
	}
	in := PlanInput{
		AppID:        "app1id",
		AppName:      "s3app",
		TeamSlug:     "acme",
		PrjSlug:      "prod",
		DeploymentID: "dep1",
		Spec:         spec,
		FileEnv:      map[string]map[string]string{},
		ComposeEnv:   map[string]map[string]string{},
		Images:       map[string]string{"web": "repo/web:1@sha256:abc", "worker": "repo/worker:1@sha256:def"},
		Decision:     placement.Decision{},
		Volumes:      nil,
	}
	if mutate != nil {
		mutate(&in)
	}
	return BuildPlan(in)
}

func envMapOf(spec ServiceSpec) map[string]string {
	out := map[string]string{}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// TestPlanInjectsS3SystemEnv 仅带 label 的服务获得六键 system env；未带
// label 的服务零注入；system 值参与合并（覆盖 compose 层同键）。
func TestPlanInjectsS3SystemEnv(t *testing.T) {
	sys := s3SystemVars("http://rustfs:9000", "", "fleetly", "AK-managed", "SK-plain", true)
	plan, err := planInputFor(t, s3LabelCompose, func(in *PlanInput) {
		in.SystemEnv = map[string][]envlayer.PlatformVar{"web": sys}
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(plan.Services))
	}
	for _, svc := range plan.Services {
		env := envMapOf(svc)
		if strings.HasSuffix(svc.Name, "-web") {
			for key, want := range map[string]string{
				"S3_ENDPOINT":         "http://rustfs:9000",
				"S3_REGION":           "",
				"S3_BUCKET":           "fleetly",
				"S3_ACCESS_KEY_ID":    "AK-managed",
				"S3_SECRET_ACCESS_KEY": "SK-plain",
				"S3_PATH_STYLE":       "true",
			} {
				if env[key] != want {
					t.Errorf("web env %s = %q, want %q", key, env[key], want)
				}
			}
		} else {
			for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_SECRET_ACCESS_KEY"} {
				if _, ok := env[key]; ok {
					t.Errorf("unlabeled service got %s=%q (injection must be label-scoped)", key, env[key])
				}
			}
		}
	}
}

// TestPlanNetworkWiring 网络牵线（仅 rustfs）：AttachRustfsNetwork=true →
// 带 label 服务附加 fleetly-rustfs-net；false（external 形态）不加。
func TestPlanNetworkWiring(t *testing.T) {
	sys := s3SystemVars("http://rustfs:9000", "", "fleetly", "AK", "SK", true)
	withNet, err := planInputFor(t, s3LabelCompose, func(in *PlanInput) {
		in.SystemEnv = map[string][]envlayer.PlatformVar{"web": sys}
		in.AttachRustfsNetwork = true
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	found := false
	for _, svc := range withNet.Services {
		if !strings.HasSuffix(svc.Name, "-web") {
			continue
		}
		for _, n := range svc.Networks {
			if n.Name == state.RustfsNetworkName {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("labeled service must attach the rustfs network in rustfs mode")
	}
	// unlabeled 服务不牵线。
	for _, svc := range withNet.Services {
		if strings.HasSuffix(svc.Name, "-worker") {
			for _, n := range svc.Networks {
				if n.Name == state.RustfsNetworkName {
					t.Error("unlabeled service must not attach the rustfs network")
				}
			}
		}
	}

	withoutNet, err := planInputFor(t, s3LabelCompose, func(in *PlanInput) {
		in.SystemEnv = map[string][]envlayer.PlatformVar{"web": sys}
	})
	if err != nil {
		t.Fatalf("BuildPlan external form: %v", err)
	}
	for _, svc := range withoutNet.Services {
		for _, n := range svc.Networks {
			if n.Name == state.RustfsNetworkName {
				t.Fatal("external mode must not attach the rustfs network")
			}
		}
	}
}

// TestPlanSystemEnvParticipatesInHash 注入 env 的脱敏参与：source=system 条
// 目进快照哈希输入；值变化（同键换值）→ EnvSnapshotHash 变化——desired-hash
// 据此感知注入面变化。
func TestPlanSystemEnvParticipatesInHash(t *testing.T) {
	sysA := s3SystemVars("http://rustfs:9000", "", "fleetly", "AK-1", "SK-1", true)
	sysB := s3SystemVars("http://rustfs:9000", "", "fleetly", "AK-2", "SK-2", true)
	mk := func(sys []envlayer.PlatformVar) string {
		plan, err := planInputFor(t, s3LabelCompose, func(in *PlanInput) {
			in.SystemEnv = map[string][]envlayer.PlatformVar{"web": sys}
		})
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		return plan.EnvSnapshotHash
	}
	if mk(sysA) == mk(sysB) {
		t.Fatal("injected credential change must change the env snapshot hash")
	}
}

// TestPlanOverrideWarningForS3Keys 同键覆盖警告：compose 层声明 S3_ENDPOINT
// → W_ENV_PLATFORM_OVERRIDE（system > 文件层既定序的披露面）。
func TestPlanOverrideWarningForS3Keys(t *testing.T) {
	yaml := `name: s3app
services:
  web:
    image: repo/web:1
    environment:
      S3_ENDPOINT: "http://user-override:9000"
    labels:
      fleetly.s3: "true"
`
	sys := s3SystemVars("http://rustfs:9000", "", "fleetly", "AK", "SK", true)
	plan, err := planInputFor(t, yaml, func(in *PlanInput) {
		in.SystemEnv = map[string][]envlayer.PlatformVar{"web": sys}
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	warned := false
	for _, w := range plan.Warnings {
		if w.Code == "W_ENV_PLATFORM_OVERRIDE" && strings.Contains(w.Message, "S3_ENDPOINT") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected W_ENV_PLATFORM_OVERRIDE for S3_ENDPOINT, warnings = %+v", plan.Warnings)
	}
	// 合并结论：system 值最终胜出。
	for _, svc := range plan.Services {
		if strings.HasSuffix(svc.Name, "-web") {
			if env := envMapOf(svc); env["S3_ENDPOINT"] != "http://rustfs:9000" {
				t.Fatalf("system layer must win the merge, got %q", env["S3_ENDPOINT"])
			}
		}
	}
}

// TestResolveS3InjectionSentinels resolveS3Injection 的裁决面：无 label 零
// 读取；unset → E_S3_NOT_CONFIGURED；rustfs 未备便 → 可重试哨兵；rustfs
// 已备便 → 派生端点 + 托管凭据解密。
func TestResolveS3InjectionSentinels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	spec, _, err := compose.Load(ctx, h.writeCompose(s3LabelCompose))
	if err != nil {
		t.Fatalf("compose load: %v", err)
	}

	// 无 label：零注入、不读设置（结构上以未配置也能通过证明）。
	vars, attach, err := h.eng.resolveS3Injection(ctx, mustStripS3(t, spec))
	if err != nil || vars != nil || attach {
		t.Fatalf("no-label: vars=%v attach=%v err=%v, want nil/false/nil", vars, attach, err)
	}

	// label 存在而 unset → E_S3_NOT_CONFIGURED（部署规划期诚实拒绝）。
	_, _, err = h.eng.resolveS3Injection(ctx, spec)
	if err == nil {
		t.Fatal("unset mode must be refused")
	}
	var target *apperr.Error
	if !errors.As(err, &target) || target.Code() != "E_S3_NOT_CONFIGURED" {
		t.Fatalf("err = %v, want E_S3_NOT_CONFIGURED", err)
	}

	// rustfs 且托管凭据未备便 → 可重试哨兵（生成拍未到的暂态）。
	if err := h.store.SaveS3Settings(ctx, state.S3Settings{Mode: state.S3ModeRustfs}, state.S3SaveOptions{Actor: "system"}); err != nil {
		t.Fatalf("save rustfs mode: %v", err)
	}
	_, _, err = h.eng.resolveS3Injection(ctx, spec)
	if !errors.Is(err, errS3WaitingRustfsCredentials) {
		t.Fatalf("rustfs without creds: err=%v, want retryable sentinel", err)
	}

	// 凭据备便（模拟 rustfs 管理器已生成）→ 派生端点 + 解密后的托管凭据。
	actCT, err := h.box.Encrypt([]byte("AK-plain-managed"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	secCT, err := h.box.Encrypt([]byte("SK-plain-managed"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := h.store.SaveRustfsCredentialsCiphertext(ctx, string(actCT), string(secCT)); err != nil {
		t.Fatalf("save creds: %v", err)
	}
	vars, attach, err = h.eng.resolveS3Injection(ctx, spec)
	if err != nil || !attach {
		t.Fatalf("rustfs ready: attach=%v err=%v, want wired", attach, err)
	}
	got := vars["web"]
	if got == nil || len(got) != 6 {
		t.Fatalf("vars = %+v, want six system keys", got)
	}
	byKey := map[string]string{}
	for _, v := range got {
		byKey[v.Key] = v.Value
		if v.Source != string(envlayer.SourceSystem) {
			t.Errorf("key %s source = %s, want system", v.Key, v.Source)
		}
	}
	if byKey["S3_ENDPOINT"] != state.RustfsEndpointURL || byKey["S3_BUCKET"] != state.RustfsBucketName {
		t.Errorf("derived endpoint/bucket = %s/%s, want managed constants", byKey["S3_ENDPOINT"], byKey["S3_BUCKET"])
	}
	if byKey["S3_ACCESS_KEY_ID"] != "AK-plain-managed" || byKey["S3_SECRET_ACCESS_KEY"] != "SK-plain-managed" {
		t.Error("managed credentials must be decrypted into the injection values")
	}
	if byKey["S3_PATH_STYLE"] != "true" {
		t.Error("managed endpoint must inject path-style=true")
	}
}

// mustStripS3 返回去掉 fleetly.s3 label 的同形 spec（无 label 对照面）。
func mustStripS3(t *testing.T, spec *compose.Spec) *compose.Spec {
	t.Helper()
	out := *spec
	out.Services = make([]compose.Service, len(spec.Services))
	for i := range spec.Services {
		out.Services[i] = spec.Services[i]
		out.Services[i].S3 = false
	}
	return &out
}

// TestS3ExternalInjectionValues external 形态：设置值直出 + secret 解密 +
// path-style 跟随设置；未带 label 服务零注入已由 rustfs 形态覆盖。
func TestS3ExternalInjectionValues(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	spec, _, err := compose.Load(ctx, h.writeCompose(s3LabelCompose))
	if err != nil {
		t.Fatalf("compose load: %v", err)
	}
	secCT, err := h.box.Encrypt([]byte("ext-sk-plain"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	in := state.S3Settings{
		Mode: state.S3ModeExternal, EndpointURL: "https://s3.example.com", Region: "us-east-1",
		Bucket: "fleetly-backup", AccessKeyID: "AKIDEXT", SecretAccessKey: string(secCT),
		PathStyle: true,
	}
	if err := h.store.SaveS3Settings(ctx, in, state.S3SaveOptions{Actor: "system"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	vars, attach, err := h.eng.resolveS3Injection(ctx, spec)
	if err != nil || attach {
		t.Fatalf("external: attach=%v err=%v, want no wiring", attach, err)
	}
	byKey := map[string]string{}
	for _, v := range vars["web"] {
		byKey[v.Key] = v.Value
	}
	want := map[string]string{ //nolint:gosec // G101：注入透传断言的期望值，非真实凭据
		"S3_ENDPOINT": "https://s3.example.com", "S3_REGION": "us-east-1", "S3_BUCKET": "fleetly-backup",
		"S3_ACCESS_KEY_ID": "AKIDEXT", "S3_SECRET_ACCESS_KEY": "ext-sk-plain", "S3_PATH_STYLE": "true",
	}
	for k, v := range want {
		if byKey[k] != v {
			t.Errorf("%s = %q, want %q", k, byKey[k], v)
		}
	}
}
