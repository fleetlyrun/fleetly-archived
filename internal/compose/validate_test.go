package compose

import (
	"context"
	"strings"
	"testing"
)

// TestValidationRejectMatrix 验收 2：受控子集校验矩阵——每个拒绝路径至少
// 一例反例（期望码 + 违规路径），并抽样正例对照（同码族不误伤合法形态）。
func TestValidationRejectMatrix(t *testing.T) {
	cases := []struct {
		name       string
		compose    string
		wantCode   string
		wantPath   string // 错误 context.path 前缀（空则不断言）
		negativeOf string // 对照正例名（PositiveCases 中的键）
	}{
		// ── v0.1 拒绝清单（E_COMPOSE_UNSUPPORTED）──
		{"reject_depends_on", `
name: my-api
services:
  web: { image: nginx }
  api:
    image: my/api
    depends_on: [web]
`, "E_COMPOSE_UNSUPPORTED", "services.api.depends_on", "pos_multi_service"},

		{"reject_extends", `
name: my-api
services:
  base: { image: nginx }
  web:
    image: my/api
    extends: { service: base }
`, "E_COMPOSE_UNSUPPORTED", "services.web.extends", "pos_multi_service"},

		{"reject_include", `
include: [other.compose.yaml]
name: my-api
services:
  web: { image: nginx }
`, "E_COMPOSE_UNSUPPORTED", "include", "pos_multi_service"},

		{"reject_profiles", `
name: my-api
services:
  web:
    image: nginx
    profiles: [debug]
`, "E_COMPOSE_UNSUPPORTED", "services.web.profiles", "pos_multi_service"},

		// configs 的 v0.1 整键拒绝已随 T 线 OT-3/IMPL-T1-4 解除（配置资源
		// 开放）：短语法（canonical 化为 {source}，无 target）按受控形态
		// 拒绝——平台要求显式 {source, target} 长语法。
		{"reject_configs_short_syntax", `
name: my-api
services:
  web:
    image: nginx
    configs: [app_conf]
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0]", "pos_configs_long"},

		{"reject_external_network", `
name: my-api
services:
  web:
    image: nginx
    networks: [backnet]
networks:
  backnet:
    external: true
`, "E_COMPOSE_UNSUPPORTED", "networks.backnet.external", "pos_stack_network"},

		{"reject_network_mode_host", `
name: my-api
services:
  web:
    image: nginx
    network_mode: host
`, "E_COMPOSE_UNSUPPORTED", "services.web.network_mode", "pos_stack_network"},

		// ── 危险字段（E_COMPOSE_UNSUPPORTED，默认拒绝）──
		{"reject_privileged_false_still_rejected", `
name: my-api
services:
  web:
    image: nginx
    privileged: false
`, "E_COMPOSE_UNSUPPORTED", "services.web.privileged", ""},

		{"reject_cap_add", `
name: my-api
services:
  web:
    image: nginx
    cap_add: [NET_ADMIN]
`, "E_COMPOSE_UNSUPPORTED", "services.web.cap_add", ""},

		{"reject_pid_host", `
name: my-api
services:
  web:
    image: nginx
    pid: host
`, "E_COMPOSE_UNSUPPORTED", "services.web.pid", ""},

		{"reject_devices", `
name: my-api
services:
  web:
    image: nginx
    devices: [/dev/dri:/dev/dri]
`, "E_COMPOSE_UNSUPPORTED", "services.web.devices", ""},

		{"reject_host_bind", `
name: my-api
services:
  web:
    image: nginx
    volumes:
      - type: bind
        source: ./data
        target: /data
`, "E_COMPOSE_UNSUPPORTED", "services.web.volumes", "pos_named_volume"},

		{"reject_docker_sock_mount", `
name: my-api
services:
  web:
    image: nginx
    volumes:
      - docker_sock:/var/run/docker.sock
volumes:
  docker_sock:
`, "E_COMPOSE_UNSUPPORTED", "services.web.volumes", "pos_named_volume"},

		// ── 受管字段（E_COMPOSE_MANAGED_FIELD）──
		{"reject_failure_action_rollback", `
name: my-api
services:
  web:
    image: nginx
    deploy:
      update_config:
        failure_action: rollback
`, "E_COMPOSE_MANAGED_FIELD", "services.web.deploy.update_config.failure_action", "pos_managed_fields"},

		{"reject_monitor_10s", `
name: my-api
services:
  web:
    image: nginx
    deploy:
      update_config:
        monitor: 10s
`, "E_COMPOSE_MANAGED_FIELD", "services.web.deploy.update_config.monitor", "pos_managed_fields"},

		// ── 更新策略安全（E_COMPOSE_UNSAFE_STRATEGY）──
		{"reject_start_first_with_volume", `
name: my-api
services:
  web:
    image: nginx
    volumes: [data:/var/lib/data]
    deploy:
      update_config:
        order: start-first
volumes:
  data:
`, "E_COMPOSE_UNSAFE_STRATEGY", "services.web.deploy.update_config.order", "pos_stop_first_with_volume"},

		// ── global 模式显式拒绝（M1-4：v0.1 单节点不支持——副本/失败停机
		//    语义未实现；v0.2 开放，对齐 C1 secrets 先例）──
		{"reject_mode_global", `
name: my-api
services:
  agent:
    image: nginx
    deploy:
      mode: global
      update_config:
        order: stop-first
`, "E_COMPOSE_UNSUPPORTED", "services.agent.deploy.mode", ""},

		// ── 域名契约（E_DOMAIN_CONFLICT / E_DOMAIN_UNSUPPORTED）──
		{"reject_domain_wildcard", `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.domains: "*.example.com"
`, "E_DOMAIN_UNSUPPORTED", "services.web.labels.fleetly.domains", "pos_domains"},

		{"reject_domain_per_service_limit", `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.domains: "a.com, b.com, c.com, d.com, e.com, f.com"
`, "E_DOMAIN_UNSUPPORTED", "services.web.labels.fleetly.domains", "pos_domains"},

		{"reject_domain_conflict_across_services", `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.domains: "api.example.com"
  admin:
    image: nginx
    labels:
      fleetly.domains: "API.Example.com"
`, "E_DOMAIN_CONFLICT", "", "pos_domains_two_services"},

		{"reject_domain_per_app_limit", `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.domains: "a.com, b.com, c.com, d.com"
  extra:
    image: nginx
    labels:
      fleetly.domains: "e.com, f.com, g.com, h.com"
  third:
    image: nginx
    labels:
      fleetly.domains: "i.com, j.com, k.com, l.com"
`, "E_DOMAIN_UNSUPPORTED", "", "pos_domains_two_services"},

		// ── 保留 label 前缀（E_LABEL_RESERVED）──
		{"reject_reserved_label", `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.custom: mine
`, "E_LABEL_RESERVED", "services.web.labels.fleetly.custom", "pos_domains"},

		// ── 放置契约（E_PLACEMENT_NODE_INVALID / E_PLACEMENT_LABEL_CONFLICT）──
		{"reject_placement_node_invalid_ulid", `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.placement.node: "n_not-a-ulid"
`, "E_PLACEMENT_NODE_INVALID", "services.web.labels.fleetly.placement.node", "pos_placement"},

		{"reject_placement_label_conflict", `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.placement.node: srv-01
  worker:
    image: nginx
    labels:
      fleetly.placement.node: srv-02
`, "E_PLACEMENT_LABEL_CONFLICT", "", "pos_placement"},

		// ── 白名单缺省拒绝（E_COMPOSE_UNSUPPORTED）──
		// 注意：顶层未知键在 loader schema 校验即被拒（additionalProperties
		//=false），早于白名单——同一错误码，无 path 上下文。
		{"reject_unknown_top_level", `
name: my-api
x-custom: {}
services:
  web: { image: nginx }
runtime_config: {}
`, "E_COMPOSE_UNSUPPORTED", "", ""},

		{"reject_unknown_service_field", `
name: my-api
services:
  web:
    image: nginx
    container_name: my-web
`, "E_COMPOSE_UNSUPPORTED", "services.web.container_name", ""},

		{"reject_ports", `
name: my-api
services:
  web:
    image: nginx
    ports: ["8080:80"]
`, "E_COMPOSE_UNSUPPORTED", "services.web.ports", "pos_expose"},

		{"reject_build_unsupported_subkey", `
name: my-api
services:
  web:
    build:
      context: .
      args: { VER: "1" }
`, "E_COMPOSE_UNSUPPORTED", "services.web.build.args", "pos_build"},

		{"reject_healthcheck_disable", `
name: my-api
services:
  web:
    image: nginx
    healthcheck:
      disable: true
`, "E_COMPOSE_UNSUPPORTED", "services.web.healthcheck.disable", "pos_healthcheck"},

		{"reject_env_pass_through", `
name: my-api
services:
  web:
    image: nginx
    environment:
      - EMPTY_VAR
`, "E_COMPOSE_UNSUPPORTED", "services.web.environment", "pos_env_literal"},

		{"reject_env_null_value", `
name: my-api
services:
  web:
    image: nginx
    environment:
      EMPTY_VAR:
`, "E_COMPOSE_UNSUPPORTED", "services.web.environment.EMPTY_VAR", "pos_env_literal"},

		{"reject_no_image_no_build", `
name: my-api
services:
  web:
    expose: ["80"]
`, "E_COMPOSE_UNSUPPORTED", "services.web", "pos_build"},

		// ── secrets 受控开放（E4 W4-S4，managed-databases §2.7/D-DB-7：C1
		// 预授权的显式解除——external-only 形态放行，值进仓库的来源仍拒）──
		{"reject_secret_file_source", `
name: my-api
services:
  web: { image: nginx }
secrets:
  db_url:
    file: ./db_url.txt
`, "E_COMPOSE_UNSUPPORTED", "secrets.db_url.file", ""},

		{"reject_secret_environment_source", `
name: my-api
services:
  web: { image: nginx }
secrets:
  db_url:
    environment: DB_URL
`, "E_COMPOSE_UNSUPPORTED", "secrets.db_url.environment", ""},

		{"reject_secret_missing_external", `
name: my-api
services:
  web: { image: nginx }
secrets:
  db_url: {}
`, "E_COMPOSE_UNSUPPORTED", "", ""}, // loader 层即拒（one of file|environment must be set）——形态守卫在 parse 期，path 不经本包 context

		{"reject_secret_external_not_boolean", `
name: my-api
services:
  web: { image: nginx }
secrets:
  db_url: { external: "true" }
`, "E_COMPOSE_UNSUPPORTED", "secrets.db_url.external", ""},

		{"reject_secret_driver_source", `
name: my-api
services:
  web: { image: nginx }
secrets:
  db_url: { driver: secretfs }
`, "E_COMPOSE_UNSUPPORTED", "secrets.db_url.driver", ""},

		{"reject_service_secret_uid", `
name: my-api
services:
  web:
    image: nginx
    secrets: [{ source: db_url, uid: "1000" }]
secrets:
  db_url: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.secrets[0].uid", ""},

		{"reject_service_secret_mode", `
name: my-api
services:
  web:
    image: nginx
    secrets: [{ source: db_url, mode: 0400 }]
secrets:
  db_url: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.secrets[0].mode", ""},

		{"reject_service_secret_undeclared_reference", `
name: my-api
services:
  web:
    image: nginx
    secrets: [db_url]
`, "E_COMPOSE_UNSUPPORTED", "services.web.secrets[0]", ""},

		{"reject_secret_bad_name", `
name: my-api
services:
  web: { image: nginx }
secrets:
  "db/url": { external: true }
`, "E_COMPOSE_UNSUPPORTED", "", ""}, // compose schema 的键模式校验在 parse 期即拒（additionalProperties not allowed）

		{"reject_service_volume_tmpfs", `
name: my-api
services:
  web:
    image: nginx
    volumes:
      - type: tmpfs
        target: /cache
`, "E_COMPOSE_UNSUPPORTED", "services.web.volumes[0].type", "pos_named_volume"},

		{"reject_service_network_aliases", `
name: my-api
services:
  web:
    image: nginx
    networks:
      backnet:
        aliases: [alias-web]
networks:
  backnet:
`, "E_COMPOSE_UNSUPPORTED", "services.web.networks.backnet", "pos_stack_network"},

		{"reject_volume_name_override", `
name: my-api
services:
  web: { image: nginx }
volumes:
  data:
    name: pinned-name
`, "E_COMPOSE_UNSUPPORTED", "volumes.data.name", "pos_named_volume"},

		{"reject_replicas_with_volumes", `
name: my-api
services:
  web:
    image: nginx
    volumes: [data:/var/lib/data]
    deploy:
      replicas: 3
volumes:
  data:
`, "E_COMPOSE_UNSUPPORTED", "", "pos_named_volume"},

		{"reject_constraint_outside_namespace", `
name: my-api
services:
  web:
    image: nginx
    deploy:
      placement:
        constraints: [node.role == worker]
`, "E_COMPOSE_UNSUPPORTED", "services.web.deploy.placement.constraints[0]", "pos_placement_constraint"},

		// ── configs 契约（T 线 OT-3/IMPL-T1-4）──
		// 守卫①（解析期半边）：服务引用的 config 必须在顶层 configs 声明
		// （external-only）——声明面自洽哨兵；值的存在性归引擎 preflight。
		{"reject_config_source_undeclared", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /etc/app/config.yaml }]
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0]", "pos_configs_long"},

		// 值的本地来源一律拒绝（值不进仓库是硬边界）。注：compose-go 的
		// checkConsistency 要求 config 定义至少有 file|environment|content
		// 之一，「definition 存在但缺 external」的形态在 loader 期即被拒
		//（本层缺 external 分支为防御性兜底，见 validateConfigsDict）。
		{"reject_config_inline_content", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /etc/app/config.yaml }]
configs:
  app_conf:
    content: "inline"
`, "E_COMPOSE_UNSUPPORTED", "configs.app_conf.content", "pos_configs_long"},

		{"reject_config_null_definition", `
name: my-api
services:
  web: { image: nginx }
configs:
  app_conf:
`, "E_COMPOSE_UNSUPPORTED", "", ""}, // compose schema 的 config 定义必须是对象（null 在 parse 期即拒）

		{"reject_config_local_file_source", `
name: my-api
services:
  web: { image: nginx }
configs:
  app_conf:
    file: ./config.yaml
`, "E_COMPOSE_UNSUPPORTED", "configs.app_conf.file", ""},

		{"reject_config_external_false", `
name: my-api
services:
  web: { image: nginx }
configs:
  app_conf:
    external: false
`, "E_COMPOSE_UNSUPPORTED", "configs.app_conf.external", ""},

		// 守卫④：target 撞 /run/secrets 前缀（secret 固定根不可撞）。
		{"reject_config_target_secret_root", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /run/secrets/app_conf }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].target", "pos_configs_long"},

		{"reject_config_target_secret_prefix", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /run/secrets }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].target", "pos_configs_long"},

		{"reject_config_target_relative", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: etc/app/config.yaml }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].target", "pos_configs_long"},

		{"reject_config_target_directory", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /etc/app/ }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].target", "pos_configs_long"},

		{"reject_config_target_missing", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].target", "pos_configs_long"},

		// 非规范路径形态（path.Clean 恒等契约）——堵住 /run//secrets/x 这类
		// 绕过前缀禁撞的书写。
		{"reject_config_target_secret_nonclean", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: //run/secrets/app_conf }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].target", "pos_configs_long"},

		{"reject_config_target_dotdot", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /etc/../run/secrets/app_conf }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].target", "pos_configs_long"},

		{"reject_config_uid_gid_mode", `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /etc/app/config.yaml, mode: 0644 }]
configs:
  app_conf: { external: true }
`, "E_COMPOSE_UNSUPPORTED", "services.web.configs[0].mode", "pos_configs_long"},

		{"reject_empty_services", `
name: my-api
services: {}
`, "E_COMPOSE_UNSUPPORTED", "services", ""},

		{"reject_bad_spec_name", `
name: My API!
services:
  web: { image: nginx }
`, "E_COMPOSE_UNSUPPORTED", "name", ""},

		// 保留字撞键校验已随 v0.3 命名三段化退役（rbac-teams §4.3 保留字
		// 迁移）：app 名不再紧邻 fleetly- 前缀——旧保留名 cron/rustfs/
		// registry 作为 compose 顶层名现在**合法**（结构性安全，E_APP_NAME_
		// RESERVED 退役，8 词迁 team slug 清单由团队受理层消费）。负向断言
		// 就此移除；保留字的撞键证据链留在 internal/naming 保留字表及其测试。
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ae := loadErr(t, tc.compose, tc.wantCode)
			if tc.wantPath != "" {
				if path := ae.Context()["path"]; !strings.HasPrefix(path, tc.wantPath) {
					t.Errorf("context.path = %q, want prefix %q", path, tc.wantPath)
				}
			}
		})
	}
}

// PositiveCases 是拒绝矩阵的对照正例（同码族的合法形态必须通过——保证
// 校验没有把合法面一起拒掉）。
var PositiveCases = map[string]string{ //nolint:gosec // compose 夹具文本（含 secrets 字段名），非凭证
	"pos_multi_service": `
name: my-api
services:
  web: { image: nginx, expose: ["80"] }
  worker:
    image: my/worker
    command: run --queue
`,
	"pos_build": `
name: my-api
services:
  web:
    build: .
`,
	"pos_build_dockerfile": `
name: my-api
services:
  web:
    build: { context: ., dockerfile: deploy/Dockerfile }
`,
	"pos_expose": `
name: my-api
services:
  web: { image: nginx, expose: ["8080", "9090"] }
`,
	"pos_healthcheck": `
name: my-api
services:
  web:
    image: nginx
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost"]
      interval: 30s
      timeout: 5s
      retries: 5
      start_period: 15s
`,
	"pos_env_literal": `
name: my-api
services:
  web:
    image: nginx
    environment:
      FOO: bar
      BAZ: "quoted value"
`,
	"pos_managed_fields": `
name: my-api
services:
  web:
    image: nginx
    deploy:
      replicas: 2
      update_config:
        order: start-first
        failure_action: pause
        monitor: 5s
        parallelism: 2
        delay: 10s
`,
	"pos_stop_first_with_volume": `
name: my-api
services:
  web:
    image: nginx
    volumes: [data:/var/lib/data]
    deploy:
      update_config:
        order: stop-first
volumes:
  data:
`,
	"pos_named_volume": `
name: my-api
services:
  web:
    image: nginx
    volumes:
      - data:/var/lib/data
      - { type: volume, source: data, target: /other, read_only: true }
volumes:
  data: { driver: local }
`,
	"pos_stack_network": `
name: my-api
services:
  web:
    image: nginx
    networks: [frontnet, backnet]
  db:
    image: postgres
    networks: [backnet]
networks:
  frontnet:
  backnet:
    driver: overlay
`,
	"pos_domains": `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.domains: "Bücher.de, app.example.com"
`,
	"pos_domains_two_services": `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.domains: "api.example.com, web.example.com"
  admin:
    image: nginx
    labels:
      fleetly.domains: "admin.example.com"
`,
	"pos_placement": `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.placement.node: srv-01
  worker:
    image: nginx
    labels:
      fleetly.placement.node: srv-01
`,
	"pos_placement_constraint": `
name: my-api
services:
  web:
    image: nginx
    deploy:
      placement:
        constraints:
          - node.labels.fleetly.rack == r1
          - node.labels.fleetly.zone != z9
`,
	"pos_stop_signal": `
name: my-api
services:
  web:
    image: nginx
    stop_signal: SIGINT
    stop_grace_period: 30s
`,
	// secrets 受控开放正例（E4 W4-S4，D-DB-7）：external-only 声明 + 短/长
	// 语法服务引用必须通过——「接受面」与拒绝面对照。
	"pos_secret_external_short": `
name: my-api
services:
  web:
    image: nginx
    secrets: [db_url]
secrets:
  db_url: { external: true }
`,
	"pos_secret_external_long": `
name: my-api
services:
  web:
    image: nginx
    secrets: [{ source: db_url, target: db_url.txt }]
secrets:
  db_url: { name: whatever, external: true }
`,
	// configs 受控开放正例（T 线 OT-3/IMPL-T1-4）：顶层 external-only 声明 +
	// 服务级显式 {source, target} 长语法（绝对单文件路径）必须通过。
	"pos_configs_long": `
name: my-api
services:
  web:
    image: nginx
    configs: [{ source: app_conf, target: /etc/app/config.yaml }]
  worker:
    image: my/worker
    configs:
      - source: app_conf
        target: /etc/worker/config.yaml
configs:
  app_conf: { name: whatever, external: true }
`,
}

// TestValidationPositiveMatrix 跑全部对照正例（必须全通过）。
func TestValidationPositiveMatrix(t *testing.T) {
	for name, content := range PositiveCases {
		t.Run(name, func(t *testing.T) {
			loadOK(t, writeCompose(t, content))
		})
	}
}

// TestUserLabelNotPassedWarning S16-C2：非 fleetly.* 服务 label 产出 W 级
// 警告（Kind=user_label_not_passed，随 Load warnings 通道带出）；fleetly.*
// 平台约定 label 不触发。用户 label 仍被放行（不阻断），只是披露不透传。
func TestUserLabelNotPassedWarning(t *testing.T) {
	path := writeCompose(t, `
name: my-api
services:
  web:
    image: nginx
    labels:
      com.example.owner: platform-team
      com.example.version: "2"
      fleetly.domains: "api.example.com"
`)
	spec, warnings, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(spec.Services) != 1 {
		t.Fatalf("services = %d, want 1 (user labels are admitted without blocking)", len(spec.Services))
	}
	var hit bool
	for _, w := range warnings {
		if w.Kind == WarningKindUserLabelNotPassed {
			hit = true
			if w.Service != "web" {
				t.Errorf("warning service = %q, want web", w.Service)
			}
			if !strings.Contains(w.Message, "com.example.owner") || !strings.Contains(w.Message, "com.example.version") {
				t.Errorf("warning message does not list user label keys: %s", w.Message)
			}
			if !strings.Contains(w.Message, "does not pass through") {
				t.Errorf("warning message missing the pass-through disclaimer: %s", w.Message)
			}
		}
	}
	if !hit {
		t.Errorf("warnings missing %s: %+v", WarningKindUserLabelNotPassed, warnings)
	}
	// 对照：纯平台 label 不触发该警告（无 healthcheck 的 W_DEPLOY_NO_HEALTHCHECK
	// 属另一通道，不在断言面）。
	_, cleanWarnings, err := Load(context.Background(), writeCompose(t, `
name: my-api
services:
  web:
    image: nginx
    labels:
      fleetly.domains: "api.example.com"
`))
	if err != nil {
		t.Fatalf("Load clean: %v", err)
	}
	for _, w := range cleanWarnings {
		if w.Kind == WarningKindUserLabelNotPassed {
			t.Errorf("platform-only labels triggered the user label warning: %+v", w)
		}
	}
}
