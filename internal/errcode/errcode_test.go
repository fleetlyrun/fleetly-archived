package errcode

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// docCodes 是验收标准的内联文档清单（手抄自四份设计文档，逐条注明出处；
// 验收：与注册表数量与拼写完全一致）。抄录基准：docs/design/ 下
// 2026-09-17 四份文档当前版本。
var docCodes = map[string]string{ // code → 文档出处
	// release-semantics.md §2.7（17 E）
	"E_COMPOSE_UNSUPPORTED":         "release-semantics §2.7",
	"E_COMPOSE_MANAGED_FIELD":       "release-semantics §2.7",
	"E_COMPOSE_UNSAFE_STRATEGY":     "release-semantics §2.7",
	"E_BUILD_FAILED":                "release-semantics §2.7",
	"E_IMAGE_PULL_FAILED":           "release-semantics §2.7",
	"E_IMAGE_UNAVAILABLE":           "release-semantics §2.7",
	"E_SCHEDULER_PENDING_TIMEOUT":   "release-semantics §2.7",
	"E_TASK_START_FAILED":           "release-semantics §2.7",
	"E_HEALTH_TIMEOUT":              "release-semantics §2.7",
	"E_OBSERVE_CRASH_LOOP":          "release-semantics §2.7",
	"E_OBSERVE_UNHEALTHY":           "release-semantics §2.7",
	"E_DEPLOY_INTERRUPTED":          "release-semantics §2.7",
	"E_DEPLOY_POST_WINDOW_UNSTABLE": "release-semantics §2.7",
	"E_DEPLOY_DOWNTIME_FAILED":      "release-semantics §2.7",
	"E_ROLLBACK_FAILED":             "release-semantics §2.7",
	"E_ROLLBACK_NO_TARGET":          "release-semantics §2.7",
	"E_RUNTIME_UNAVAILABLE":         "release-semantics §2.7",

	// stateful-placement.md §2.8（8 E）
	"E_PLACEMENT_NODE_INVALID":      "stateful-placement §2.8",
	"E_PLACEMENT_NODE_NOT_FOUND":    "stateful-placement §2.8",
	"E_PLACEMENT_NODE_UNAVAILABLE":  "stateful-placement §2.8",
	"E_PLACEMENT_NODE_GONE":         "stateful-placement §2.8",
	"E_PLACEMENT_NO_ELIGIBLE_NODE":  "stateful-placement §2.8",
	"E_PLACEMENT_MOVE_REQUIRES_ACK": "stateful-placement §2.8",
	"E_PLACEMENT_LABEL_CONFLICT":    "stateful-placement §2.8",
	"E_VOLUME_NODE_MISMATCH":        "stateful-placement §2.8",

	// stateful-placement.md §2.9（1 E）
	"E_CAPABILITY_REQUIRES_MULTI_NODE": "stateful-placement §2.9",

	// state-model.md §2.7 / §2.9 / §2.4 / §2.2（4 E）
	"E_BACKUP_KEY_MISSING":     "state-model §2.7",
	"E_EVENT_CURSOR_EXPIRED":   "state-model §2.9",
	"E_LABEL_RESERVED":         "state-model §2.4",
	"E_STATE_VERSION_CONFLICT": "state-model §2.2 (same as architecture §2.3)",

	// architecture.md §2.4（2 E，E_COMPOSE_* 与 release-semantics 重复不另计）
	"E_DOMAIN_CONFLICT":    "architecture §2.4",
	"E_DOMAIN_UNSUPPORTED": "architecture §2.4",

	// T2.15 实现期新增（文档外码单独列出，待 T0.5 契约冻结确认）：架构 §2.5
	// 不变量「路由发布失败不回滚部署、单独告警」的审计错误码落点。
	"E_ROUTE_PUBLISH_FAILED": "T2.15 added during implementation (architecture §2.5 route-failure alert semantics; pending T0.5 freeze confirmation)",

	// DT-4 实现期新增（torchwood 线；注册表只增）：发布管线晋级前一次性
	// init job 的失败/超时归因码（消费点 = internal/engine init 相位评估；
	// job 名与原因进 deployment.failed detail 与 release.job_* 事件）。
	"E_INIT_JOB_FAILED":    "DT-4 added during implementation (torchwood line: release-time init job failed before promotion; no retry, restart-condition=none)",
	"E_INIT_JOB_TIMED_OUT": "DT-4 added during implementation (torchwood line: release-time init job watchdog budget exceeded; fleetly.job.timeout overrides the 10m default)",

	// MG-C3 实现期新增（文档外码单独列出，待 T0.5 契约冻结确认）：架构 §2.4
	// plan/apply 语义「破坏性操作要求 --confirm-destructive」的 deploy 入队
	// 门控错误码。
	"E_DEPLOY_CONFIRM_REQUIRED": "MG-C3 added during implementation (architecture §2.4 destructive-change confirmation gate; pending T0.5 freeze confirmation)",

	// M4-6 实现期新增（评审整改 B5，文档外码单独列出，待 T0.5 契约冻结
	// 确认）：RevokeToken 最后管理员守卫——吊销最后一枚未吊销 admin token
	// 会使平台锁死（重启不补种 bootstrap），409 拒绝。
	"E_TOKEN_LAST_ADMIN": "M4-6 added during implementation (review remediation B5: token last-admin guard; pending T0.5 freeze confirmation)",

	// multi-node.md §5.2（2 E，D-MN-11）：registry 模式部署前哨与推送的
	// 分层归因码（manifest 缺失复用 E_IMAGE_UNAVAILABLE，不另立新码）。
	"E_REGISTRY_UNAVAILABLE": "multi-node §5.2 (D-MN-11: registry-mode deploy preflight, registry unreachable)",
	"E_REGISTRY_PUSH_FAILED": "multi-node §5.2 (D-MN-11: push to the platform registry failed)",

	// multi-node.md §5.2（1 E，D-MN-13）：join 门禁——base_domain 缺失即
	// 多节点未启用，join 面显式拒绝（409）。
	"E_MULTI_NODE_REQUIRES_BASE_DOMAIN": "multi-node §5.2 (D-MN-13: join gate, base_domain missing)",

	// E3 对象存储 §5.2 实现期新增（2026-09-21 裁决轮落定，文档外码单独
	// 列出，待 T0.5 契约冻结确认）：E_S3_NOT_CONFIGURED = label 注入前哨
	//（E3-4/W3-S3 接线消费——s3.mode=unset 时 plan 阶段拒绝）；E_S3_TEST_
	// FAILED = 探针失败（detail/context 带失败步）；E_S3_PUBLIC_REQUIRES_
	// BASE_DOMAIN = 公网子域开关在无平台域名形态下拒绝。
	"E_S3_NOT_CONFIGURED":              "E3 object-storage §5.2 (label-injection sentinel; wired with E3-4)",
	"E_S3_CONFIG_CONFLICT":             "E3 object-storage §5.2 (mode vs explicit fields mutual exclusion)",
	"E_S3_TEST_FAILED":                 "E3 object-storage §5.2 (connection probe failed; failed step in the envelope context)",
	"E_S3_PUBLIC_REQUIRES_BASE_DOMAIN": "E3 object-storage §5.2 (public subdomain toggle without a base domain)",

	// E4 数据库托管（managed-databases 设计 §5.2，2026-09-20 冻结，注册表
	// 只增；D-DB-8 零新增码纪律 = 状态机冲突复用 E_STATE_VERSION_CONFLICT，
	// 本组 9 码是设计裁决的封闭清单）。E_ENV_KEY_RESERVED 已随 S1 守卫接线
	//（internal/state/env.go）；其余 8 码的生产引用随 S2-S5 票据落地
	//（usage_test 豁免清单同步注记）。
	"E_DB_NOT_FOUND":            "E4 managed-databases §5.2 (reference/operation target missing or deleting/deleted)",
	"E_DB_REFERENCED":           "E4 managed-databases §5.2 (delete refused while references exist)",
	"E_DB_TEMPLATE_UNSUPPORTED": "E4 managed-databases §5.2 (unknown template / settings violate the managed surface)",
	"E_DB_ENV_PREFIX_CONFLICT":  "E4 managed-databases §5.2 (same-app reference env prefix collision)",
	"E_DB_BACKUP_FAILED":        "E4 managed-databases §5.2 (backup job failed)",
	"E_DB_RESTORE_FAILED":       "E4 managed-databases §5.2 (restore failed)",
	"E_DB_ROTATE_FAILED":        "E4 managed-databases §5.2 (rotation failed mid-flight)",
	"E_SECRET_NOT_FOUND":        "E4 managed-databases §5.2 (compose-declared external secret missing; preflight)",
	"E_ENV_KEY_RESERVED":        "E4 managed-databases §5.2 (reserved FLEETLY_ env namespace; wired with the S1 SetAppEnv guard)",

	// T 线 OT-3/IMPL-T1-4（注册表只增）：明文配置资源的缺失哨兵。消费点 =
	// internal/engine/configinject.go（compose configs 声明的声明名不在
	// app_configs：preparing 期前哨 + 快照重放悬空名，与 E_SECRET_NOT_FOUND
	// 同分层）。
	"E_CONFIG_NOT_FOUND": "v0.3 T-line OT-3/IMPL-T1-4 (compose-declared external config missing from the platform config store; deploy preflight and snapshot replay)",

	// E6 观测（observability 设计 §3.1，W5-S1 接线，注册表只增）。
	"E_LOGS_BACKEND_UNAVAILABLE": "E6 observability §3.1 (search face unavailable: backend=jsonl or VictoriaLogs unreachable; live tail unaffected)",
	// E6 观测（observability 设计 §4.2，W5-S3 接线，注册表只增）：opt-in
	// 查询面的两处诚实分支。
	"E_METRICS_NOT_ENABLED":         "E6 observability §4.2 (metrics.mode unset — the query face is opt-in and has nothing to report)",
	"E_METRICS_BACKEND_UNAVAILABLE": "E6 observability §4.2 (VictoriaMetrics unreachable on the loopback face; scrape face unaffected)",
	// E6 观测（observability 设计 §5，W5-S4 接线，注册表只增）：通知
	// Webhook 面的族码——NotFound/NameConflict 为 state 哨兵的信封化投影
	//（internal/api/notifications.go），PatternInvalid 为订阅模式白名单校验
	//（internal/state/webhooks.go ValidateWebhookPatterns）。
	"E_WEBHOOK_NOT_FOUND":       "E6 observability §5 (W5-S4 webhook endpoint absent: envelope projection of the state sentinel, 404)",
	"E_WEBHOOK_NAME_CONFLICT":   "E6 observability §5 (W5-S4 endpoint names are unique: envelope projection of the state sentinel, 409)",
	"E_WEBHOOK_PATTERN_INVALID": "E6 observability §5 (W5-S4 subscription glob pattern rejected by the whitelist: non-empty [a-z0-9._-*], 422)",
	// B 线 W5 告警面（b-line-w5 设计 §2，D-V3W5-1，W5-S2 接线，注册表只增）：
	// alerts.mode 前置门 + alert_rules CRUD 的哨兵投影与形状校验族。消费点 =
	// internal/state/alertsettings.go 与 internal/state/alerting.go（409 前置
	// 门在 state 层构造）+ internal/api/alerting.go（哨兵投影）。
	"E_ALERTS_METRICS_REQUIRED":     "v0.3 W5 b-line-w5 §2.1 (alerts.mode=on gate: metrics.mode must be on first — vmalert has no datasource otherwise, 409)",
	"E_ALERT_RULE_NOT_FOUND":        "v0.3 W5 b-line-w5 §2.2 (alert rule absent: envelope projection of the state sentinel, 404)",
	"E_ALERT_RULE_NAME_CONFLICT":    "v0.3 W5 b-line-w5 §2.2 (rule names unique platform-wide: envelope projection of the state sentinel, 409)",
	"E_ALERT_RULE_EXPR_INVALID":     "v0.3 W5 b-line-w5 §2.2 (expr must be a non-empty PromQL of at most 2048 chars, 422)",
	"E_ALERT_RULE_FOR_INVALID":      "v0.3 W5 b-line-w5 §2.2 (for_duration must be >= 0 seconds, 422)",
	"E_ALERT_RULE_LABELS_INVALID":   "v0.3 W5 b-line-w5 §2.2 (labels must be a JSON object with non-empty string keys, 422)",
	"E_ALERT_RULE_CHANNELS_INVALID": "v0.3 W5 b-line-w5 §2.2/§2.3 (channels must be a list of non-empty endpoint ids; empty = all enabled endpoints, 422)",
	// E7 Web 终端（web-terminal 设计 §2.4/§2.5，W5-S6 接线，注册表只增）：
	// 功能开关门——terminal.enabled=false 时 ticket 受理与 WS 接入的诚实
	// 拒绝（internal/api/terminal.go / internal/execrelay hub.go）。
	"E_TERMINAL_DISABLED": "E7 web-terminal §2.4 (W5-S6 feature gate: terminal.enabled=false deploys no exec relay and refuses ticket issuance, 409)",

	// B 线 W5 ACME DNS-01 通配证书面（b-line-w5 设计 §3，D-V3W5-3/D-V3W5-4，
	// W5-S3 接线，注册表只增）：acme.* 设置联动校验门两码（消费点 =
	// internal/state/acmesettings.go ValidateAcmeSettings——422 语义违约 /
	// 409 配置缺失）+ DNS 服务商探针失败码（消费点 = internal/api/acme.go
	// TestDnsProvider——失败步进信封 context）。
	"E_ACME_WILDCARD_REQUIRES_PROVIDER":    "v0.3 W5 b-line-w5 §3 (acme.wildcard=true requires a DNS provider — a wildcard SAN can only be validated via DNS-01, 422)",
	"E_ACME_WILDCARD_REQUIRES_BASE_DOMAIN": "v0.3 W5 b-line-w5 §3 (acme.wildcard=true requires a platform base domain — the wildcard domain set derives from it, 409)",
	"E_ACME_DNS_TEST_FAILED":               "v0.3 W5 b-line-w5 §3 (DNS provider probe failed: create -> delete a real TXT record; failed step in the envelope context, 503)",

	// v0.3 W1 认证/用户面（rbac-teams 设计 §2.1/§10，注册表只增）：注册
	// 窗口关闭的稳定拒绝码（无用户窗口恒开不落本码）。消费点 =
	// internal/api/authservice.go Register（state 哨兵 ErrRegistrationClosed
	// 的 apperr 化投影，403）。
	"E_REGISTRATION_CLOSED": "v0.3 W1 rbac-teams §2.1 (registration window closed: auth.registration defaults to closed once any user exists, 403)",

	// v0.3 W2-S1 团队/项目面（rbac-teams 设计 §5 错误码清单，注册表只增）：
	// 消费点 = internal/api/teams.go（最后一名 owner 守卫、保留字 slug 守卫）
	// 与 internal/api/authservice.go AcceptInvite（一次性邀请四类不可消费
	// 形态统一同码）。
	"E_TEAM_LAST_OWNER":    "v0.3 W2-S1 rbac-teams §3.1/§5 (last-team-owner guard on member removal/demotion, 409)",
	"E_INVITE_INVALID":     "v0.3 W2-S1 rbac-teams §5 (one-time invite invalid/used/revoked/expired — one code, state not disclosed, 409)",
	"E_TEAM_SLUG_RESERVED": "v0.3 W2-S1 rbac-teams §4.3/§5 (team slug vs platform component namespaces — v0.3 naming formulas take the team slug as parameter, 422)",

	// v0.3 W2-S3 归属管道（rbac-teams §4.2/§5 + D-W0-9 解析规则）：裸名
	// 歧义两面（project 解析与 app/库资源解析）+ 归属一致性守卫。消费点 =
	// internal/api（deployments.go / databases.go / errors.go）。
	// E_APP_NAME_RESERVED 已随保留字迁移退役（v0.2.x 收尾波增、v0.3 W2-S3
	// 减——rbac-teams §4.3/§5 设计明示「E_APP_NAME_RESERVED 退役」；保留字
	// 清单整体迁 team slug 后 app 名不再紧邻 fleetly- 前缀，守卫消费点
	// internal/compose 受理层同步移除）。退役是设计明示的减码而非漂移。
	"E_PROJECT_AMBIGUOUS":    "v0.3 W2-S3 rbac-teams §4.2/§5 (bare project name matches multiple visible projects, D-W0-9; qualify as team/project, 400)",
	"E_APP_AMBIGUOUS":        "v0.3 W2-S3 rbac-teams §4.2/§5 (resource name matches rows across projects — per-project uniqueness, D-W0-4; qualified/id read face lands in S4, 400)",
	"E_APP_PROJECT_MISMATCH": "v0.3 W2-S3 rbac-teams §3.4 (deploy/database-create targets a project different from the row's ownership — check-consistency ruling; MoveApp guidance, 409)",
	"E_APP_PROJECT_REQUIRED": "v0.3 W2-S3 rbac-teams §3.4 (git-push first-deploy cannot derive project ownership: no signed user or no default project; deploy once via CLI/API, 400)",
	"E_DB_PROJECT_MISMATCH":  "v0.3 W2-S4 rbac-teams §4.1/§4.2 (referenced database instance belongs to a different project than the app — project isolation R6/R7; cross-project database attachment rejected at deploy admission via the engine preparing face, 409)",

	// W3 遗留撞键票收口（2026-09-21，实现期新增，文档外码单独列出）：app
	// 顶层名与平台组件命名空间的保留字校验（compose 受理层消费，
	// internal/naming 保留字表为证据链，422）。
	// E_APP_NAME_RESERVED 于 v0.3 W2-S3 退役（见上方归属管道分组的注记）。

	// 警告码（5 W）
	"W_DEPLOY_INSTABILITY":      "release-semantics §2.7",
	"W_DEPLOY_NO_HEALTHCHECK":   "release-semantics §2.7/§2.8",
	"W_ROLLBACK_IMAGE_RISK":     "release-semantics §2.7",
	"W_PLACEMENT_STATELESS_PIN": "stateful-placement §2.8",
	"W_ENV_PLATFORM_OVERRIDE":   "architecture §2.4",
}

// TestDocCodeSetMatchesRegistry 是验收标准 2：注册表码集与四份文档清单
// 逐一致（数量与拼写完全一致）。
func TestDocCodeSetMatchesRegistry(t *testing.T) {
	regIDs := Default().IDs()
	if len(regIDs) != len(docCodes) {
		t.Fatalf("registry has %d codes, doc list has %d", len(regIDs), len(docCodes))
	}
	for _, id := range regIDs {
		if _, ok := docCodes[id]; !ok {
			t.Errorf("registry code %q not in the doc list (out-of-doc codes must be listed separately and marked pending T0.5 freeze confirmation)", id)
		}
	}
	for id, source := range docCodes {
		if _, ok := Default().Get(id); !ok {
			t.Errorf("doc code %q (%s) missing from the registry: omission", id, source)
		}
	}
}

// TestRegisteredCountByKind 双保险：E + 5 W 计数钉死（T2.15 增
// E_ROUTE_PUBLISH_FAILED、MG-C3 增 E_DEPLOY_CONFIRM_REQUIRED、M4-6 增
// E_TOKEN_LAST_ADMIN、E1-5 增 E_REGISTRY_UNAVAILABLE/E_REGISTRY_PUSH_FAILED、
// E1-8 增 E_MULTI_NODE_REQUIRES_BASE_DOMAIN、E3-2 增 E_S3_* 四码、E4-S1 增
// managed-databases §5.2 九码、E6 W5-S1 增 E_LOGS_BACKEND_UNAVAILABLE、
// E6 W5-S3 增 E_METRICS_NOT_ENABLED/E_METRICS_BACKEND_UNAVAILABLE、
// E6 W5-S4 增 E_WEBHOOK_* 三码、E7 W5-S6 增 E_TERMINAL_DISABLED、v0.2.x
// 收尾波增 E_APP_NAME_RESERVED（W3 撞键票）、v0.3 W1 增
// E_REGISTRATION_CLOSED、v0.3 W2-S1 增 E_TEAM_LAST_OWNER / E_INVITE_INVALID /
// E_TEAM_SLUG_RESERVED。E7 S6 后 = 59 E + 5 W；W1 后 = 60 E + 5 W；
// W2-S1 后 = 63 E + 5 W；W2-S3 减 E_APP_NAME_RESERVED（rbac-teams §4.3
// 保留字迁移退役——设计明示的唯一减码）增 E_PROJECT_AMBIGUOUS /
// E_APP_AMBIGUOUS / E_APP_PROJECT_MISMATCH / E_APP_PROJECT_REQUIRED
// → 66 E + 5 W；W2-S4 增 E_DB_PROJECT_MISMATCH（rbac-teams §4.1 E4 跨项目
// 库引用守卫，部署受理面）→ 67 E + 5 W；W5-S2 增告警面七码（B 线
// b-line-w5 设计 §2，D-V3W5-1：E_ALERTS_METRICS_REQUIRED 前置门 +
// E_ALERT_RULE_* 六码）→ 74 E + 5 W；W5-S3 增 ACME DNS-01 通配证书面三码
// （b-line-w5 设计 §3，D-V3W5-3/D-V3W5-4：E_ACME_WILDCARD_REQUIRES_PROVIDER /
// E_ACME_WILDCARD_REQUIRES_BASE_DOMAIN 联动门 + E_ACME_DNS_TEST_FAILED
// 探针失败）→ 77 E + 5 W。DT-4（IMPL-T1-3）增 E_INIT_JOB_FAILED /
// E_INIT_JOB_TIMED_OUT（部署期 init job 失败/超时归因）→ 79 E + 5 W；
// IMPL-T1-4（OT-3）增 E_CONFIG_NOT_FOUND（明文配置资源缺失哨兵）→ 80 E + 5 W。
func TestRegisteredCountByKind(t *testing.T) {
	errCount, warnCount := 0, 0
	for _, c := range Default().All() {
		if strings.HasPrefix(c.ID, "E_") {
			errCount++
		} else {
			warnCount++
		}
	}
	if errCount != 80 || warnCount != 5 {
		t.Fatalf("E_ = %d (want 80), W_ = %d (want 5)", errCount, warnCount)
	}
}

// TestDuplicateRegistrationRejected 验收标准 3：重复注册 fail-fast。
func TestDuplicateRegistrationRejected(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Code{ID: "E_TEST_DUPLICATE", HTTP: 400, Summary: "s", Suggestion: "x"})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic (fail-fast)")
		}
	}()
	r.MustRegister(Code{ID: "E_TEST_DUPLICATE", HTTP: 409, Summary: "s2", Suggestion: "y"})
}

// TestInvalidFormatRejected 验收标准 3：非法格式 fail-fast（不以 E_/W_ 开头、
// 含小写、空串、空段）。
func TestInvalidFormatRejected(t *testing.T) {
	cases := []string{
		"E_lower",
		"e_UPPER",
		"X_NOT_REGISTRY",
		"NOTPREFIXED",
		"",
		"E_",
		"E__DOUBLE",
		"E_TRAILING_",
		"W_lower_case",
	}
	for _, id := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("invalid code %q must panic at registration", id)
				}
			}()
			NewRegistry().MustRegister(Code{ID: id, HTTP: 400, Summary: "s", Suggestion: "x"})
		}()
	}
}

// TestHTTPMappingInvariants：E_ 码须 4xx/5xx；W_ 码不得携带 HTTP 状态。
func TestHTTPMappingInvariants(t *testing.T) {
	for _, c := range Default().All() {
		if strings.HasPrefix(c.ID, "W_") {
			if c.HTTP != 0 {
				t.Errorf("warning %s carries HTTP %d, want 0", c.ID, c.HTTP)
			}
			continue
		}
		if c.HTTP < 400 || c.HTTP > 599 {
			t.Errorf("error %s HTTP = %d, want 4xx/5xx", c.ID, c.HTTP)
		}
		if c.Docs() != DocsURLPrefix+c.ID {
			t.Errorf("%s docs anchor = %q, want prefix+ID", c.ID, c.Docs())
		}
	}
}

// TestDocumentedHTTPMappings 文档显式给定的 HTTP 映射照文档。
func TestDocumentedHTTPMappings(t *testing.T) {
	want := map[string]int{
		"E_DOMAIN_CONFLICT":                 409, // architecture §2.4
		"E_STATE_VERSION_CONFLICT":          409, // state-model §2.2
		"E_VOLUME_NODE_MISMATCH":            409, // stateful-placement §2.8（前哨 409）
		"E_PLACEMENT_MOVE_REQUIRES_ACK":     409, // stateful-placement §2.2
		"E_EVENT_CURSOR_EXPIRED":            410, // state-model §2.9
		"E_LABEL_RESERVED":                  422, // state-model §2.4
		"E_PLACEMENT_LABEL_CONFLICT":        422, // stateful-placement §2.2
		"E_PLACEMENT_NODE_INVALID":          422, // stateful-placement §2.2（解析失败 422+候选）
		"E_PLACEMENT_NODE_NOT_FOUND":        422, // stateful-placement §2.5
		"E_REGISTRY_UNAVAILABLE":            503, // multi-node §5.2（D-MN-11 前哨快速失败）
		"E_REGISTRY_PUSH_FAILED":            500, // multi-node §5.2（D-MN-11 推送失败）
		"E_MULTI_NODE_REQUIRES_BASE_DOMAIN": 409, // multi-node §5.2（D-MN-13 join 门禁）
		// E4 managed-databases §5.2（9 码的 HTTP 映射为文档显式给定）。
		"E_DB_NOT_FOUND":            404,
		"E_DB_REFERENCED":           409,
		"E_DB_TEMPLATE_UNSUPPORTED": 400,
		"E_DB_ENV_PREFIX_CONFLICT":  422,
		"E_DB_BACKUP_FAILED":        500,
		"E_DB_RESTORE_FAILED":       500,
		"E_DB_ROTATE_FAILED":        500,
		"E_SECRET_NOT_FOUND":        422,
		"E_ENV_KEY_RESERVED":        422,
	}
	for id, httpStatus := range want {
		c, ok := Default().Get(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		if c.HTTP != httpStatus {
			t.Errorf("%s HTTP = %d, want %d (documented explicitly)", id, c.HTTP, httpStatus)
		}
	}
}

// TestGoldenSnapshot 码集 golden 快照：新增/改写码必须显式更新 golden
// （防静默变更；-update 重生成）。
func TestGoldenSnapshot(t *testing.T) {
	golden := filepath.Join("testdata", "codes.golden")
	got := Default().Snapshot()
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden) //nolint:gosec // golden 为 testdata 固定路径
	if err != nil {
		t.Fatalf("read golden (run go test -update to regenerate): %v", err)
	}
	if string(want) != got {
		t.Fatalf("code set drifted from golden:\n--- golden ---\n%s\n--- registry ---\n%s", want, got)
	}
}
