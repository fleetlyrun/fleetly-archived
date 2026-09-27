package eventcode

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// docEvents 是验收标准的内联文档清单（手抄自设计文档，逐条注明出处）。
var docEvents = map[string]string{ // event → 文档出处
	// release-semantics.md §2.7：deployment.* 16 个
	"deployment.queued":             "release-semantics §2.7",
	"deployment.release_started":    "release-semantics §2.7",
	"deployment.healthy":            "release-semantics §2.7",
	"deployment.switched":           "release-semantics §2.7",
	"deployment.observe_started":    "release-semantics §2.7",
	"deployment.succeeded":          "release-semantics §2.7",
	"deployment.failed":             "release-semantics §2.7",
	"deployment.warning":            "release-semantics §2.7",
	"deployment.cancelled":          "release-semantics §2.7",
	"deployment.rollback_started":   "release-semantics §2.7",
	"deployment.rollback_finished":  "release-semantics §2.7",
	"deployment.rollback_failed":    "release-semantics §2.7",
	"deployment.recovery_scheduled": "release-semantics §2.7",
	"deployment.recovery_blocked":   "release-semantics §2.7",
	"deployment.substrate_halted":   "release-semantics §2.7",
	"deployment.superseded":         "release-semantics §2.7",

	// release-semantics.md §2.7：app.* 3 个
	"app.degraded":             "release-semantics §2.7",
	"app.instability_detected": "release-semantics §2.7",
	"app.recovered":            "release-semantics §2.7",

	// stateful-placement.md §2.8：placement.* 5 / node.* 4 / volume.* 4
	"placement.bound":      "stateful-placement §2.8",
	"placement.changed":    "stateful-placement §2.8",
	"placement.blocked":    "stateful-placement §2.8",
	"placement.recovered":  "stateful-placement §2.8",
	"placement.unresolved": "stateful-placement §2.8",
	"node.joined":          "stateful-placement §2.8",
	"node.down":            "stateful-placement §2.8",
	"node.up":              "stateful-placement §2.8",
	"node.removed":         "stateful-placement §2.8",
	// multi-node.md §2.7/§5.3：availability 转移事件（v0.2 新增码）。
	"node.availability_changed": "multi-node §2.7/§5.3 (v0.2: active/drain/pause transition, drain maintenance-window narrative)",
	"volume.created":            "stateful-placement §2.8",
	"volume.detached":           "stateful-placement §2.8",
	"volume.orphaned":           "stateful-placement §2.8",
	"volume.discarded":          "stateful-placement §2.8",

	// state-model.md §2.9（对账漂移）/ §2.7（恢复完成）
	"reconcile.drift_detected": "state-model §2.9",
	"restore.completed":        "state-model §2.7",

	// architecture.md §4.3（cron 触发前哨「记 skipped + 事件」，v0.2）
	"cron.skipped": "architecture §4.3",

	// E5 Cron 实现期新增（object-storage 设计 §8 事件面 + FZ-4 钉名，W3-S5
	// 接线）：发出来源 = internal/cron 触发链与完成检测——事件与 cron_runs
	// 台账行同事务（Outbox）。cron.timed_out 为 FZ-4 钉名。
	"cron.triggered": "E5 Cron added during implementation (object-storage §8; W3-S5; one-shot job created on schedule or manual trigger)",
	"cron.succeeded": "E5 Cron added during implementation (object-storage §8; W3-S5; run completed, job service removed)",
	"cron.failed":    "E5 Cron added during implementation (object-storage §8; W3-S5; run failed, no retry)",
	"cron.timed_out": "E5 Cron added during implementation (object-storage §8; FZ-4 pinned name; watchdog budget exceeded, job service removed)",

	// DT-4 实现期新增（torchwood 线；注册表只增）：发布管线晋级前一次性
	// init job 的失败/超时披露——发出来源 = internal/engine init 相位评估，
	// 随后 deployment.failed（E_INIT_JOB_FAILED / E_INIT_JOB_TIMED_OUT）
	// 点名 job 与原因；payload 只带事实字段，无敏感材料。
	"release.job_failed":    "DT-4 added during implementation (torchwood line: release-time init job failed; the deployment fails, job service removed)",
	"release.job_timed_out": "DT-4 added during implementation (torchwood line: release-time init job watchdog budget exceeded; fleetly.job.timeout overrides the 10m default)",

	// T2.15 实现期新增（文档外事件名单独列出，待 T0.5 契约冻结确认）：架构
	// §2.5 不变量「路由发布严格晚于健康门；发布失败不回滚部署、单独告警 +
	// 审计」。证书签发/续期不设新事件名（走审计记录）。
	"route.published":      "T2.15 added during implementation (architecture §2.5 route publish timing; pending T0.5 freeze confirmation)",
	"route.publish_failed": "T2.15 added during implementation (architecture §2.5 route failure alerts separately; pending T0.5 freeze confirmation)",
	// IMPL-T1-1 实现期新增（OT-2 单一写点仲裁，T 线设计档；注册表只增）：
	// state 域名行存在时 compose label 声明被忽略的披露——label 仅首部署
	// bootstrap 种子；发出来源 = internal/ingress PublishRoutes 仲裁点。
	"route.label_ignored": "IMPL-T1-1 added during implementation (OT-2 single-writer arbitration: labels are bootstrap-only once state domain rows exist)",

	// S17-D1 实现期新增（评审类 D 超时与取消闭环）：webhook 受理转异步后
	// 拉源失败只能走事件流披露（官方不重投）。
	"app.webhook_fetch_failed": "S17-D1 added during implementation (review class D; disclosure of webhook async source-fetch failure)",

	// S18-A10 实现期新增（评审类 A 运行时断言层，§9 裁决并入 janitor）：
	// 非终态行超龄停留的显性化告警。
	"engine.stale_nonterminal": "S18-A10 added during implementation (review class A; over-age alert for non-terminal deploys)",
	"build.stale_nonterminal":  "S18-A10 added during implementation (review class A; over-age alert for non-terminal builds)",

	// B6/H10 实现期新增（MG-3 横切结构修复）：app 删除生命周期第二拍的
	// 终局事件（引擎 deleting 回收 duty 发出）。
	"app.deleted": "B6/H10 added during implementation (MG-3; app deletion second-beat terminal event)",

	// T0-V2.2 实现期新增（调研 R2 运行期 DB↔Swarm 对账，引擎周期 duty）：
	// 声称 running 的 app 其期望服务在 substrate 整体缺失的披露与派生态
	// 修正（running → down；只披露不重建）。
	"app.substrate_missing": "T0-V2.2 added during implementation (R2; runtime existence reconciliation: running app whose substrate service vanished — disclosed and the derived view corrected)",

	// E3 对象存储 §5.3 实现期新增（2026-09-21 裁决轮落定，文档外事件名
	// 单独列出，待 T0.5 契约冻结确认）：设置变更事件——payload 带模式与
	// 布尔开关不带走秘密；发出来源 = platform_settings 的 S3 设置保存事务
	//（internal/state/s3settings.go，与业务写同事务 = Outbox 模式）。
	"s3.updated": "E3 object-storage §5.3 added during implementation (settings change; payload carries mode/toggles, never credentials)",
	// IMPL-T1-2 实现期新增（DT-2 平台 registry 凭证面；注册表只增）：
	// 发出来源 = platform_settings 的 registry.* 设置保存事务
	//（internal/state/registrysettings.go，与业务写同事务 = Outbox）。
	"registry.updated": "IMPL-T1-2 added during implementation (DT-2 platform registry credentials change; payload carries the host and the password fingerprint, never credentials)",

	// E3 对象存储 §5.3 rustfs duty 差分事件（W3-S3/E3-5 接线）：发出来源 =
	// internal/rustfs 收敛拍（创建/漂移更新 → deployed，mode 离开 → removed
	// 且卷保留）；payload 不含任何凭据材料。
	"s3.rustfs_deployed": "E3 object-storage §5.3 added during implementation (W3-S3 rustfs duty converge diff; payload carries service/image/reason, never credentials)",
	"s3.rustfs_removed":  "E3 object-storage §5.3 added during implementation (W3-S3 rustfs duty converge diff; data volume retained)",

	// E3 对象存储 §2.3/D-S3-4 实现期新增（W3-S2/E3-3 上传轨接线）：上传
	// 失败与恢复绿配对事件——发出来源 = 备份 Manager 上传步
	//（internal/statebackup/restic.go）；payload 带 backup id 与擦除后的
	// 错误摘要，绝不带 restic env 凭证材料。
	"backup.upload_failed":    "E3 object-storage §2.3/D-S3-4 added during implementation (W3-S2 upload track; local snapshot unaffected, payload carries backup id + redacted error summary)",
	"backup.upload_recovered": "E3 object-storage §2.3/D-S3-4 added during implementation (W3-S2 upload track; red-to-green closure after a failed upload)",

	// E4 数据库托管（managed-databases §5.3，D-DB-8 复核后统一自有 db.*
	// 族，注册表只增）：转移事件 9（EnterDbPhase 单写点随转换同事务落）+
	// 操作事件 9（不换主状态）。S1 注册；发出来源随 S2-S5 票据接线。
	"db.provision_started":   "E4 managed-databases §5.3 (transition event; provisioning acceptance for create/resume/retry)",
	"db.ready":               "E4 managed-databases §5.3 (transition event; health gate passed)",
	"db.provision_failed":    "E4 managed-databases §5.3 (transition event; convergence failed, scene preserved)",
	"db.degraded":            "E4 managed-databases §5.3 (transition event; in-service unhealthy)",
	"db.recovered":           "E4 managed-databases §5.3 (transition event; degraded -> ready)",
	"db.suspended":           "E4 managed-databases §5.3 (transition event; ready/degraded -> paused)",
	"db.resumed":             "E4 managed-databases §5.3 (transition event; paused -> provisioning)",
	"db.delete_started":      "E4 managed-databases §5.3 (transition event; reference guard passed, tombstone first beat)",
	"db.deleted":             "E4 managed-databases §5.3 (transition event; reap completed, name enters retention hold)",
	"db.upgrade_available":   "E4 managed-databases §5.3 (operation event; opt-in upgrade advertised)",
	"db.upgrade_started":     "E4 managed-databases §5.3 (operation event; controlled rebuild with backup gate)",
	"db.upgrade_finished":    "E4 managed-databases §5.3 (operation event; upgrade converged and healthy)",
	"db.upgrade_failed":      "E4 managed-databases §5.3 (operation event; digest rolled back, state degraded)",
	"db.backup_succeeded":    "E4 managed-databases §5.3 (operation event; ledger row written)",
	"db.backup_failed":       "E4 managed-databases §5.3 (operation event; never credentials in payload)",
	"db.restore_completed":   "E4 managed-databases §5.3 (operation event; in-place restore finished)",
	"db.restore_failed":      "E4 managed-databases §5.3 (operation event; interrupted in-place restore alerts critically)",
	"db.credentials_rotated": "E4 managed-databases §5.3 (operation event; referencing apps auto-redeploy follows)",
	// E6 观测（observability §2/§7，W5-S1 接线）：logs.* 5 项——设计 §7
	//「事件码只增（logs.* 5 项）」的全集。
	"logs.backend_updated":       "E6 observability §2.2 (settings save transaction; switch triggers the duty deploy/remove)",
	"logs.victorialogs_deployed": "E6 observability §2.1 (duty converge diff; payload carries service/image/reason)",
	"logs.victorialogs_removed":  "E6 observability §2.2 (backend left victorialogs; data volume retained)",
	"logs.ingest_degraded":       "E6 observability §2.3 (ingest streak enter edge, debounced; live tail unaffected)",
	"logs.ingest_recovered":      "E6 observability §2.3 (ingest streak exit edge, debounced)",
	// E6 观测（observability §4/§7，W5-S3 接线）：metrics.* 3 项——D-W5-2
	// opt-in 的设置/收敛/清场事件面。
	"metrics.mode_updated":   "E6 observability §4.1 (settings save transaction; opt-in switch deploys or removes the managed stack, volume retained)",
	"metrics.stack_deployed": "E6 observability §4.1 (duty converge diff; payload carries service/image/reason)",
	"metrics.stack_removed":  "E6 observability §4.1 (mode left on; data volume retained)",
	// E7 Web 终端（web-terminal §2.4，W5-S6 接线）：terminal.* 2 项——会话
	// 起止的审计/事件双落面；payload 只带元数据，会话内容零出现。
	"terminal.opened": "E7 web-terminal §2.4 (hub session acceptance, same-transaction with the audit row; metadata only, never session content)",
	"terminal.closed": "E7 web-terminal §2.4 (hub session close, same-transaction with the audit row; duration/reason metadata, never session content)",

	// v0.3 W1 认证/用户面（rbac-teams 设计 §6 事件清单的注册态三类，注册表
	// 只增）：发出来源 = 自助注册组合事务（internal/state/register.go，与
	// 用户/团队/项目写入同事务 = Outbox；payload metadata-only 零 secret）。
	// auth.login/logout 族不落事件（设计 §6 仅审计）。
	"user.registered": "v0.3 W1 rbac-teams §6 (registration composite transaction; metadata-only, never credentials)",
	"team.created":    "v0.3 W1 rbac-teams §6 (registration composite transaction: personal team; W2-S1 also emits it from TeamsService.CreateTeam; metadata-only)",
	"project.created": "v0.3 W1 rbac-teams §6 (registration composite transaction: default project; W2-S1 also emits it from ProjectsService.CreateProject; metadata-only)",

	// v0.3 W2-S1 团队/项目面（rbac-teams 设计 §6 事件清单余下四项，注册表
	// 只增）：发出来源 = internal/state 的 teams.go/projects.go 原语（与
	// 业务写同事务 = Outbox；payload metadata-only 零 secret）。
	"team.member_changed":    "v0.3 W2-S1 rbac-teams §6 (member add/role-change/remove converge on one event, change field discriminates; state teams.go, same transaction)",
	"invite.accepted":        "v0.3 W2-S1 rbac-teams §6 (one-time invite consumed; state teams.go ConsumeInvite, same transaction)",
	"project.deleted":        "v0.3 W2-S1 rbac-teams §6 (empty-project deletion; state projects.go DeleteProject, same transaction)",
	"project.member_changed": "v0.3 W2-S1 rbac-teams §6 (project role override set/removed; state projects.go, same transaction)",

	// T 线 OT-1 / IMPL-T15-1 项目网（设计档 OT-1 + 票面；注册表只增）：
	// 发出来源 = internal/state/projectnetworks.go 的参与位置位/清位原语
	//（与业务写同事务 = Outbox；API attach/detach 唯一消费点）+ engine
	// substrateRecon 的 networks 面（孤儿/缺失披露；只披露不自动删建）。
	"project.network_changed": "T-line OT-1/IMPL-T15-1 (explicit opt-in attach/detach of an app to its project network; state projectnetworks.go, same transaction; defaults off)",
	"network.orphaned":        "T-line OT-1/IMPL-T15-1 DT-5 recon guard (platform-managed fleetly- network with no state attribution; disclosed only, never deleted silently)",
	"network.missing":         "T-line OT-1/IMPL-T15-1 DT-5 recon guard (expected project network absent from the substrate; re-ensured by attach/deploy, never auto-recreated by recon)",

	// v0.3 W3-S2 git SSH host key（rbac-teams §6 裁决 D-W0-8 FZ-12，注册表
	// 只增）：发出来源 = host key 启动装载与指纹台账的比对事务
	//（internal/state/hostkeysettings.go，与台账更新同事务 = Outbox；
	// payload 只带新旧 SHA256 指纹——公开材料，私钥零出现）。
	"git.hostkey_changed": "v0.3 W3-S2 rbac-teams §6 D-W0-8 (host key startup load vs fingerprint ledger; state hostkeysettings.go, same transaction; public key material only)",

	// B 线 W5 自动扩缩（设计 §1，D-V3W5-2，v0.3 W5-S1 接线，注册表只增）：
	// 发出来源 = engine 收敛拍尾部的扩缩 duty（internal/engine/
	// autoscaling.go）。策略 CRUD 零事件（审计 scaling.policy_changed 承载
	// ——事件面是运行期动作与披露）。
	"scaling.adjusted": "B-line W5 §1 D-V3W5-2 (autoscaler adjusted a service's replica count; engine autoscaling duty; payload carries dimension/before-after replicas/utilization)",
	"scaling.dormant":  "B-line W5 §1 D-V3W5-2 (policy dormant while metrics.mode is off; one-time disclosure per policy, re-armed when metrics turns on)",
	"scaling.no_data":  "B-line W5 §1 D-V3W5-2 (no metric series for the target dimension — no action, honest no-data; one-time disclosure per policy)",
}

// TestDocEventSetMatchesRegistry：注册表事件集与文档清单逐一致。
func TestDocEventSetMatchesRegistry(t *testing.T) {
	regNames := Default().Names()
	if len(regNames) != len(docEvents) {
		t.Fatalf("registry has %d events, doc list has %d", len(regNames), len(docEvents))
	}
	for _, name := range regNames {
		if _, ok := docEvents[name]; !ok {
			t.Errorf("registry event %q not in the doc list (out-of-doc event names must be listed separately and marked pending T0.5 freeze confirmation)", name)
		}
	}
	for name, source := range docEvents {
		if _, ok := Default().Get(name); !ok {
			t.Errorf("doc event %q (%s) missing from the registry: omission", name, source)
		}
	}
}

// TestDuplicateRegistrationRejected：重复注册 fail-fast。
func TestDuplicateRegistrationRejected(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Event{Name: "test.duplicate", Summary: "s"})
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate event registration must panic (fail-fast)")
		}
	}()
	r.MustRegister(Event{Name: "test.duplicate", Summary: "s2"})
}

// TestInvalidFormatRejected：非法格式 fail-fast（大写、缺 namespace 段、
// 多段、空串、数字开头）。
func TestInvalidFormatRejected(t *testing.T) {
	cases := []string{
		"",
		"UPPER.case",
		"noDot",
		"two.dots.here",
		".leading",
		"trailing.",
		"1number.start",
		"deployment.failed_extra_dash-", // 非法字符
	}
	for _, name := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("invalid event name %q must panic at registration", name)
				}
			}()
			NewRegistry().MustRegister(Event{Name: name, Summary: "s"})
		}()
	}
}

// TestGoldenSnapshot 事件集 golden 快照（防静默变更）。
func TestGoldenSnapshot(t *testing.T) {
	golden := filepath.Join("testdata", "events.golden")
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
		t.Fatalf("event set drifted from golden:\n--- golden ---\n%s\n--- registry ---\n%s", want, got)
	}
}

// TestNamespaces 命名空间覆盖核对：deployment/app/placement/node/volume/
// reconcile/restore/cron 八个域均非空。
func TestNamespaces(t *testing.T) {
	want := []string{"deployment", "app", "placement", "node", "volume", "reconcile", "restore", "cron"}
	seen := make(map[string]int)
	for _, name := range Default().Names() {
		seen[strings.SplitN(name, ".", 2)[0]]++
	}
	for _, ns := range want {
		if seen[ns] == 0 {
			t.Errorf("namespace %q has no registered events", ns)
		}
	}
}
