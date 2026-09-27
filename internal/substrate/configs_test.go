package substrate

// config 投影的翻译面测试（T 线 OT-3 / IMPL-T1-4）：ConfigReference 必须携
// ConfigID + ConfigName + 完整 File（Uid/Gid/Mode）——真机探针矩阵实证三者
// 全必填（缺任一 daemon 拒绝：no-file → "either File or Runtime should be
// set"；缺 ID 或名 → "malformed config reference"）；缺 ID = 引擎未先行
// EnsureConfig（对账层编码错误）必须显式报错。

import (
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/engine"
)

// TestBuildSwarmSpecConfigFileTargetFullValues 钉死 ConfigReference 的完整
// 形态（ID/Name/File 三写 + 0:0/0444），与 W3 secret 教训同族。
func TestBuildSwarmSpecConfigFileTargetFullValues(t *testing.T) {
	spec := engine.ServiceSpec{
		Name:     "fleetly-demo-web",
		Image:    "repo/app@sha256:abc",
		Replicas: 1,
		Configs: []engine.ConfigMount{
			{ConfigName: "fleetly-demo-config-app.yaml-abc12345", Target: "/etc/demo/app.yaml"},
		},
	}
	sw, err := buildSwarmSpec(spec, nil, map[string]string{"fleetly-demo-config-app.yaml-abc12345": "config-id-1"})
	if err != nil {
		t.Fatalf("buildSwarmSpec: %v", err)
	}
	cs := sw.TaskTemplate.ContainerSpec
	if cs == nil || len(cs.Configs) != 1 {
		t.Fatalf("container spec configs = %+v, want exactly 1 reference", cs)
	}
	ref := cs.Configs[0]
	if ref.ConfigID != "config-id-1" || ref.ConfigName != "fleetly-demo-config-app.yaml-abc12345" {
		t.Fatalf("config reference ids = %q/%q, want resolved id + name", ref.ConfigID, ref.ConfigName)
	}
	if ref.File == nil {
		t.Fatal("config File target missing (daemon rejects with 'either File or Runtime should be set')")
	}
	if ref.File.Name != "/etc/demo/app.yaml" {
		t.Fatalf("file target name = %q, want the explicit absolute path", ref.File.Name)
	}
	if ref.File.UID != "0" || ref.File.GID != "0" {
		t.Fatalf("file target UID/GID = %q/%q, want \"0\"/\"0\"", ref.File.UID, ref.File.GID)
	}
	if ref.File.Mode != 0o444 {
		t.Fatalf("file target mode = %o, want 0444 (read-only contract)", ref.File.Mode)
	}
}

// TestBuildSwarmSpecConfigNotEnsuredFailsExplicitly 反向锚：引擎未先行
// EnsureConfig（缺 ID）= 对账层编码错误，必须显式报错而不是静默丢引用。
func TestBuildSwarmSpecConfigNotEnsuredFailsExplicitly(t *testing.T) {
	_, err := buildSwarmSpec(engine.ServiceSpec{
		Name:     "s",
		Image:    "img",
		Replicas: 1,
		Configs:  []engine.ConfigMount{{ConfigName: "fleetly-s-config-k-abc12345", Target: "/etc/s/k.yaml"}},
	}, nil, nil)
	if err == nil {
		t.Fatal("missing config id must fail explicitly (engine ordering bug), got nil")
	}
}

// TestServiceToStateProjectsConfigs 实况投影（漂移反解的实况侧输入）：
// ConfigName+File.Name 出 engine.ConfigMount，与期望侧同构可比。
func TestServiceToStateProjectsConfigs(t *testing.T) {
	svc := swarm.Service{
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "fleetly-demo-web"},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
				Image: "repo/app@sha256:abc",
				Configs: []*swarm.ConfigReference{{
					ConfigID:   "cid-1",
					ConfigName: "fleetly-demo-config-app.yaml-abc12345",
					File:       &swarm.ConfigReferenceFileTarget{Name: "/etc/demo/app.yaml", UID: "0", GID: "0", Mode: 0o444},
				}},
			}},
		},
	}
	st := serviceToState(svc)
	if len(st.Configs) != 1 {
		t.Fatalf("projected configs = %+v, want one mount", st.Configs)
	}
	if st.Configs[0].ConfigName != "fleetly-demo-config-app.yaml-abc12345" || st.Configs[0].Target != "/etc/demo/app.yaml" {
		t.Fatalf("projected config = %+v, want name+target round-trip", st.Configs[0])
	}
}
