package compose

// configs 受控形态的归一化/哈希/差分回归（T 线 OT-3 / IMPL-T1-4）：顶层
// external 声明进入归一化集合、服务级 {source,target} 落位（按 source
// 排序）、target 变更进 spec_hash 与 plan diff、名称形态校验（compose 标识
// 符字符集）。

import (
	"strings"
	"testing"
)

const configsCompose = `
name: my-api
services:
  worker:
    image: my/worker
    configs:
      - source: b_conf
        target: /etc/worker/b.yaml
      - source: a_conf
        target: /etc/worker/a.yaml
  web:
    image: nginx
    configs: [{ source: a_conf, target: /etc/app/a.yaml }]
configs:
  a_conf: { external: true }
  b_conf: { external: true }
`

// TestConfigsNormalization 归一化形态：顶层声明集合排序、服务级挂载按
// source 排序、target 原样保留（平台不默认 /<source>）。
func TestConfigsNormalization(t *testing.T) {
	spec := loadOK(t, writeCompose(t, configsCompose))
	if len(spec.Configs) != 2 || spec.Configs[0] != "a_conf" || spec.Configs[1] != "b_conf" {
		t.Fatalf("spec.Configs = %v, want sorted [a_conf b_conf]", spec.Configs)
	}
	var worker, web *Service
	for i := range spec.Services {
		switch spec.Services[i].Name {
		case "worker":
			worker = &spec.Services[i]
		case "web":
			web = &spec.Services[i]
		}
	}
	if worker == nil || web == nil {
		t.Fatalf("services = %+v, want worker+web", spec.Services)
	}
	if len(worker.Configs) != 2 || worker.Configs[0].Source != "a_conf" || worker.Configs[1].Source != "b_conf" {
		t.Fatalf("worker configs = %+v, want source-sorted mounts", worker.Configs)
	}
	if worker.Configs[1].Target != "/etc/worker/b.yaml" {
		t.Fatalf("config target = %q, want the explicit absolute path", worker.Configs[1].Target)
	}
	if len(web.Configs) != 1 || web.Configs[0].Target != "/etc/app/a.yaml" {
		t.Fatalf("web configs = %+v", web.Configs)
	}
}

// TestConfigsTargetChangeHashesAndDiffs 内容声明变更（target 改路径）进
// spec_hash 与 plan diff——「声明变更即期望态变更」在配置面上成立。
func TestConfigsTargetChangeHashesAndDiffs(t *testing.T) {
	base := loadOK(t, writeCompose(t, configsCompose))
	target := loadOK(t, writeCompose(t, strings.Replace(configsCompose, "/etc/app/a.yaml", "/etc/app/renamed.yaml", 1)))
	if base.SpecHash == target.SpecHash {
		t.Fatal("target path change did not move spec_hash")
	}
	plan, err := Diff(base, target)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	found := false
	for _, upd := range plan.Services.Updated {
		if upd.Name != "web" {
			continue
		}
		for _, f := range upd.Fields {
			if f.Path == "configs.0.target" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("plan diff missing configs.0.target change: %+v", plan.Services.Updated)
	}
}

// TestConfigsNameValidation 顶层 config 名称形态（compose 标识符字符集 +
// ≤63）：非法名在解析期拒绝。
func TestConfigsNameValidation(t *testing.T) {
	for _, name := range []string{"-lead", "a/b", ".dot", strings.Repeat("x", 64)} {
		content := `
name: my-api
services:
  web: { image: nginx }
configs:
  "` + name + `": { external: true }
`
		if _, _, err := Load(t.Context(), writeCompose(t, content)); err == nil {
			t.Fatalf("config name %q accepted, want rejection", name)
		}
	}
	// 合法名（字母数字开头 + ._-）。
	spec := loadOK(t, writeCompose(t, `
name: my-api
services:
  web: { image: nginx }
configs:
  a1-B.c: { external: true }
`))
	if len(spec.Configs) != 1 || spec.Configs[0] != "a1-B.c" {
		t.Fatalf("spec.Configs = %v", spec.Configs)
	}
}
