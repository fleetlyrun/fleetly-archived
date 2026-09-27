package eventcode

// builtins 是文档域清单的全量录入（每事件注明文档出处）：
//   - release-semantics §2.7：deployment.* 16 个、app.* 3 个
//   - stateful-placement §2.8：placement.* 5 个、node.* 4 个、volume.* 4 个
//   - state-model §2.9/§2.10：reconcile.drift_detected、restore.completed
//   - architecture §4.3（cron 触发前哨「记 skipped + 事件」）：cron.skipped
//   - 实现期新增（单独列出）：route.* 2 个、app.webhook_fetch_failed、
//     engine.stale_nonterminal、build.stale_nonterminal（S18-A10）、
//     app.deleted（B6/H10，MG-3）、app.substrate_missing（T0-V2.2，R2）、
//     backup.upload_failed / backup.upload_recovered（E3-3，W3-S2）、
//     cron.triggered / cron.succeeded / cron.failed / cron.timed_out
//     （FZ-4 钉名，E5 Cron，W3-S5 接线）
//   - E4 数据库托管（managed-databases §5.3，D-DB-8 复核后统一自有族）：
//     db.* 18 个 = 转移事件 9（EnterDbPhase 单写点随转换同事务落）+
//     操作事件 9（操作不换主状态）。不复用 app.*/deployment.*——独立
//     资源无对应 subject。S1 注册；发出来源随 S2-S5 票据接线
//     （usage_test 豁免清单同步注记）。
//   - v0.3 W1 认证/用户面（rbac-teams §6 事件清单的注册态三类，注册表
//     只增）：发出来源 = 自助注册组合事务（internal/state/register.go，
//     与业务写同事务 = Outbox）。
//   - v0.3 W2-S1 团队/项目面（rbac-teams §6 事件清单余下四项，注册表
//     只增）：发出来源 = internal/state 的 teams.go/projects.go 原语
//     （AddMember/SetMemberRole/RemoveMember/ConsumeInvite/DeleteProject/
//     SetProjectMemberRole/RemoveProjectMember/CreateTeam/CreateProject，
//     与业务写同事务 = Outbox）。
//
// 计 52 + 18 = 70 + W1 增 3 = 73 + W2-S1 增 4 = 77 + W3-S2 增 1（FZ-12
// git.hostkey_changed）= 78 + W5-S1 增 3（scaling.adjusted / scaling.dormant
// / scaling.no_data，B 线 W5 D-V3W5-2）= 81 + IMPL-T1-1 增 1
// （route.label_ignored，OT-2 单一写点仲裁）= 82 + IMPL-T1-2 增 1
// （registry.updated，DT-2 平台 registry 凭证面）= 83 + IMPL-T1-3 增 2
// （release.job_failed / release.job_timed_out，DT-4 部署期 init job）=
// 85 + IMPL-T15-1 增 3（project.network_changed 项目网参与面 +
// network.orphaned / network.missing 对账兜底守卫，OT-1/DT-5）= 88 个事件名。
var builtins = []Event{
	// ── 发布（release-semantics §2.7）──
	{Name: "deployment.queued", Summary: "deploy queued (per-app mutually exclusive queueing)"},
	{Name: "deployment.release_started", Summary: "release started (build/update kicking off)"},
	{Name: "deployment.healthy", Summary: "new revision passed the health gate"},
	{Name: "deployment.switched", Summary: "traffic switch completed"},
	{Name: "deployment.observe_started", Summary: "observe window started (default 60s)"},
	{Name: "deployment.succeeded", Summary: "deploy succeeded (terminal state)"},
	{Name: "deployment.failed", Summary: "deploy failed (terminal state; reason=error code)"},
	{Name: "deployment.warning", Summary: "deploy warning (W_ code; observe window alert-only semantics)"},
	{Name: "deployment.cancelled", Summary: "deploy cancelled (replays the prior state first, then lands cancelled)"},
	{Name: "deployment.rollback_started", Summary: "rollback started (single-layer replay of the revision)"},
	{Name: "deployment.rollback_finished", Summary: "rollback finished"},
	{Name: "deployment.rollback_failed", Summary: "rollback failed"},
	// 预留：自动恢复排队事件随 v0.2 恢复器（v0.1 失败分流不建新
	// deployment，D-REL-6 默认只告警；usage_test 豁免清单同理由）。
	{Name: "deployment.recovery_scheduled", Summary: "recovery from systemic failure queued (automatic retry action)"},
	{Name: "deployment.recovery_blocked", Summary: "recovery blocked (e.g. bound node unavailable)"},
	{Name: "deployment.substrate_halted", Summary: "substrate failure halted the deploy (Swarm/node unavailable)"},
	// 预留：同 app 互斥排队下无「在途被新目标取代」路径，随 v0.2 并发策略
	//（usage_test 豁免清单同理由）。
	{Name: "deployment.superseded", Summary: "deploy superseded by a newer target (stale terminal state)"},

	// ── 应用状态机（release-semantics §2.7、state-model §2.10）──
	{Name: "app.degraded", Summary: "app entered degraded (observe window failure / post-window instability / W_DEPLOY_INSTABILITY)"},
	{Name: "app.instability_detected", Summary: "instability detected at runtime"},
	{Name: "app.recovered", Summary: "app left degraded, back to running"},
	// S17-D1 实现期新增（评审类 D；webhook 受理转异步后拉源失败只能走
	// 事件流披露——官方不重投 202，redeliver 靠人工）。
	{Name: "app.webhook_fetch_failed", Summary: "async source fetch failed after webhook acceptance (retry manually via redeliver on the same delivery)"},
	// B6/H10 实现期新增（MG-3 横切结构修复）：app 删除生命周期第二拍的
	// 终局事件——此前 api DeleteApp 只落第一拍（deleting），第二拍
	// （deleting → deleted + 受管服务移除）无执行者，服务永久运行。
	{Name: "app.deleted", Summary: "app deletion completed (tombstone second beat: managed services removed, name enters retention hold)"},
	// T0-V2.2 实现期新增（调研 R2 运行期 DB↔Swarm 对账，引擎周期 duty）：
	// 派生态声称 running 的 app 其期望服务在 substrate 整体缺失（外部
	// docker service rm）——只披露与修正派生态（running → down），不自动
	// 重建；判据是 service 存在性而非副本数，底座读错误不算缺失。
	{Name: "app.substrate_missing", Summary: "app claimed running but its managed service(s) are absent from the substrate (external removal); view corrected to down"},

	// ── 放置与节点/卷（stateful-placement §2.8）──
	{Name: "placement.bound", Summary: "app node binding completed (including automatic pinning)"},
	// v0.2 E1-7 接线：显式换点 Rebind（multi-node §2.6）发出——绑定换绑、
	// 卷行 prev 登记、事件与审计同事务。
	{Name: "placement.changed", Summary: "binding changed (via the explicitly confirmed migration path)"},
	{Name: "placement.blocked", Summary: "bound node unavailable/removed; app blocked"},
	{Name: "placement.recovered", Summary: "bound node recovered; binding re-established automatically"},
	// 预留：DR 后绑定判定要求显式放置——随 v0.2 恢复阶梯（单机无场景）。
	{Name: "placement.unresolved", Summary: "binding undecidable after DR; explicit placement required (no guessing)"},
	// v0.2 E1-6 接线：节点观测事件族由锚定 duty 差分发出（v0.1 的「不产生
	// 产品事件」注记解除，multi-node §2.7）。
	{Name: "node.joined", Summary: "node joined the cluster (observed)"},
	{Name: "node.down", Summary: "node judged DOWN (Swarm loss-of-contact verdict)"},
	{Name: "node.up", Summary: "node back to ready"},
	{Name: "node.removed", Summary: "node removed (observed docker node rm)"},
	// v0.2 多节点（multi-node §2.7/§5.3）实现期接入：availability 转移
	//（active↔drain/pause）是 drain 维护窗口叙事的事件载体（载荷 old/new）。
	{Name: "node.availability_changed", Summary: "node availability changed (active/drain/pause transition; drain maintenance-window narrative; payload carries old/new)"},
	{Name: "volume.created", Summary: "volume registered (data birthplace; pins the hosting node)"},
	// 预留：卷声明移除的显性化随 v0.2 卷生命周期票（usage_test 豁免清单
	// 同理由）。
	{Name: "volume.detached", Summary: "volume detached (volume declaration removed; data is not deleted with the declaration)"},
	{Name: "volume.orphaned", Summary: "volume orphaned (old volume kept by default after app deletion / after --discard)"},
	// v0.2 E1-7 接线：换点 discarded 处置声明（admin+confirm）由显式换点
	// 路径发出。
	{Name: "volume.discarded", Summary: "volume explicitly discarded (admin+confirm+audited)"},

	// ── 对账（state-model §2.9 对账/漂移）──
	{Name: "reconcile.drift_detected", Summary: "drift detected between desired and actual state (including manual docker operations); convergence is per-app opt-in"},

	// ── 备份恢复（state-model §2.7）──
	// 预留：恢复器未实现（同 E_BACKUP_KEY_MISSING 的预留裁决；usage_test
	// 豁免清单同理由）。
	{Name: "restore.completed", Summary: "control plane restore completed (after exiting read-only observation)"},

	// ── 入口路由（T2.15；架构 §2.5 不变量：路由发布严格晚于健康门——
	//    发布失败不回滚部署，route.publish_failed 单独告警 + 审计；证书
	//    签发/续期不设新事件名，走审计记录）──
	{Name: "route.published", Summary: "app ingress routes published (after the health gate passed, all dynamic config converged)"},
	{Name: "route.publish_failed", Summary: "app ingress route publish failed (deploy unaffected; alerts separately)"},
	// IMPL-T1-1 实现期新增（OT-2 单一写点仲裁）：state 已有域名行时
	// compose label 声明被忽略的披露（label 仅首部署 bootstrap 种子）。
	// 发出来源 = internal/ingress PublishRoutes 的仲裁点（best-effort
	// 披露，失败降级日志——不影响路由收敛）；payload 带 app 与声明/state
	// 计数，无敏感材料。
	{Name: "route.label_ignored", Summary: "compose fleetly.domains label declarations were ignored because state domain rows exist (labels are bootstrap-only; payload carries the declared/state counts)"},

	// ── 定时任务（E5 Cron，架构 §4.3 细则 + object-storage 设计 §8 事件面；
	//    W3-S5 接线。发出来源 = internal/cron 触发链与完成检测——事件与
	//    cron_runs 台账行同事务（Outbox）。FZ-4 钉名：cron.timed_out）──
	{Name: "cron.triggered", Summary: "cron schedule fired: one-shot swarm job created (payload carries app/service/run id/job service; scheduled_at is the hit cron point)"},
	{Name: "cron.succeeded", Summary: "cron run completed (task complete; job service removed)"},
	{Name: "cron.failed", Summary: "cron run failed (task failed/rejected; no retry — restart-condition=none; job service removed)"},
	{Name: "cron.timed_out", Summary: "cron run exceeded its watchdog budget (default 10m, fleetly.cron.timeout label overrides; job service removed)"},
	{Name: "cron.skipped", Summary: "cron trigger skipped (overlap / node unavailable / missed during downtime / interrupted by restart); no catch-up run"},

	// ── 部署期一次性作业（DT-4，torchwood 线；IMPL-T1-3 接线）──
	// 发出来源 = internal/engine 的 init 相位评估（release 管线在晋级前以
	// 新 spec 跑 fleetly.job: init 声明的一次性 job）：job 自身失败/超时的
	// 披露面；随后 deployment.failed（错误码 E_INIT_JOB_FAILED /
	// E_INIT_JOB_TIMED_OUT）点名 job 与原因。payload 只带事实字段
	//（deployment/app/service/job_service/budget/error），无敏感材料。
	{Name: "release.job_failed", Summary: "release-time init job failed (task failed/rejected/shut down; the deployment fails — no retry, restart-condition=none; job service removed)"},
	{Name: "release.job_timed_out", Summary: "release-time init job exceeded its watchdog budget (default 10m, fleetly.job.timeout label overrides; the deployment fails and the job service is removed)"},

	// S18-A10 实现期新增（评审类 A 运行时断言层，§9 裁决并入 janitor）：
	// 非终态行超龄停留的显性化告警（只告警不自愈——恢复路径已有 S8/S9 兜底，
	// 这层是未来新状态机漏洞的观测面）。
	{Name: "engine.stale_nonterminal", Summary: "deploy stayed non-terminal past 2x the (deploy watchdog + observe window) budget (surfaces state-machine bugs; no self-healing)"},
	{Name: "build.stale_nonterminal", Summary: "build stayed queued/building past 2x the build timeout budget (surfaces state-machine bugs; no self-healing)"},

	// ── 对象存储 S3 面（E3 对象存储专项设计 §5.3，2026-09-21 裁决轮落定，
	//    注册表只增；s3.updated 由 E3-2 接线，rustfs duty 差分事件由 E3-5
	//    接线——W3-S3）──
	// 发出来源：platform_settings 的 S3 设置保存事务（internal/state/
	// s3settings.go，与业务写同事务 = Outbox 模式）。payload 只带模式与
	// 布尔开关，绝不带凭证材料（state-model §2.9 secret 值禁止进事件）。
	{Name: "s3.updated", Summary: "object storage settings changed (payload carries the mode and toggles, never credentials)"},
	// IMPL-T1-2 实现期新增（DT-2 平台 registry 凭证面，注册表只增）：
	// 发出来源 = platform_settings 的 registry.* 设置保存事务
	//（internal/state/registrysettings.go，与业务写同事务 = Outbox 模式）。
	// payload 只带 host 与密码指纹（sha256 前 8），绝不带密码明文/密文。
	{Name: "registry.updated", Summary: "platform registry credentials changed (payload carries the host and the password fingerprint, never credentials)"},
	// E3-5 rustfs duty 差分事件（node.* 同型；发出来源 = internal/rustfs
	// 的收敛拍——服务缺失创建/spec 漂移更新发 deployed（payload 带 reason
	// created|updated），mode 离开 rustfs 服务移除发 removed（payload 带
	// volume_retained=true——数据卷保留语义的显性化面）。payload 不含任何
	// 凭据材料（凭据指纹只在日志面）。
	{Name: "s3.rustfs_deployed", Summary: "managed RustFS deployed or converged to the desired spec (payload carries service/image/reason, never credentials)"},
	{Name: "s3.rustfs_removed", Summary: "managed RustFS removed after s3.mode left rustfs (data volume retained; payload carries volume_retained=true)"},

	// ── 状态备份上传轨（E3-3，§2.3/D-S3-4；W3-S2 接线）──
	// 发出来源：备份 Manager 上传步（internal/statebackup/restic.go）。
	// 口径：restic backup 成功但回读校验失败/容器执行失败 → failed；本地
	// 份不受影响（verify_status 不回写）。payload 带 backup id 与错误摘要
	//（scrubText 擦除后），绝不带 restic env 凭证材料。
	{Name: "backup.upload_failed", Summary: "remote state backup upload failed (local snapshot unaffected; payload carries the backup id and a redacted error summary, never credentials)"},
	// 恢复绿：上一份上传 failed、本份 ok 时发出（配对 failed 形成红→绿
	// 闭环）。payload 带 backup id。
	{Name: "backup.upload_recovered", Summary: "remote state backup upload recovered (previous upload had failed; payload carries the backup id)"},

	// ── 数据库托管 E4（managed-databases §5.3，D-DB-8：统一自有 db.* 族，
	//    只增；S1 注册、发出来源随 S2-S5 票据接线）。转移事件随
	//    EnterDbPhase 单写点与转换同事务落库（Outbox）；操作事件不换主
	//    状态。payload 只带实例名/模板/状态/错误摘要等事实字段，凭据明文
	//    与连接串密码永不进事件（state-model §2.9 secret 纪律）──
	// 转移事件（§2.1 转移表逐行）：
	{Name: "db.provision_started", Summary: "database provisioning/convergence started (create, resume or retry acceptance; state -> provisioning)"},
	{Name: "db.ready", Summary: "database passed the health gate (provisioning -> ready)"},
	{Name: "db.provision_failed", Summary: "database convergence failed (health gate timeout / image unavailable / engine error; scene preserved, explicit retry required)"},
	{Name: "db.degraded", Summary: "database in service turned unhealthy (health probe failing / task crash loop; swarm self-healing observed)"},
	{Name: "db.recovered", Summary: "database recovered (health probe passing again; degraded -> ready)"},
	{Name: "db.suspended", Summary: "database suspended (scale 0, services and volumes retained)"},
	{Name: "db.resumed", Summary: "database resume accepted (paused -> provisioning reconvergence)"},
	{Name: "db.delete_started", Summary: "database deletion accepted (reference guard passed; tombstone first beat)"},
	{Name: "db.deleted", Summary: "database deletion completed (managed objects reaped; name enters retention hold; volumes orphaned by default)"},
	// 操作事件（不换主状态）：
	{Name: "db.upgrade_available", Summary: "newer template image digest available for the database template (opt-in upgrade advertised)"},
	{Name: "db.upgrade_started", Summary: "database upgrade started (pre_upgrade backup gate begins; controlled rebuild)"},
	{Name: "db.upgrade_finished", Summary: "database upgrade finished (new digest converged and healthy)"},
	{Name: "db.upgrade_failed", Summary: "database upgrade failed (digest rolled back to the previous value; state degraded)"},
	{Name: "db.backup_succeeded", Summary: "database backup succeeded (ledger row written; verify pending)"},
	{Name: "db.backup_failed", Summary: "database backup failed (failure summary in payload, never credentials)"},
	{Name: "db.restore_completed", Summary: "database restore completed in place (confirm-gated destructive operation)"},
	{Name: "db.restore_failed", Summary: "database restore failed (critical alert; in-place replay interrupted)"},
	{Name: "db.credentials_rotated", Summary: "database credentials rotated (manual two-phase confirm; referencing apps auto-redeploy follows)"},

	// ── 观测/日志库（E6 观测专项设计 §2/§7，W5-S1 接线；注册表只增。
	//    设计 §7「logs.* 5 项」的全集）──
	// 发出来源：SaveLogsSettings 保存事务（internal/state/logsettings.go，
	// 与业务写同事务 = Outbox；payload 只带后端值，词表内枚举）。
	{Name: "logs.backend_updated", Summary: "log backend setting changed (payload carries the backend value; switch triggers the duty to deploy or remove VictoriaLogs, the data volume is retained)"},
	// 发出来源：victorialogs duty 收敛拍差分（internal/victorialogs/
	// victorialogs.go——服务缺失创建/spec 漂移更新发 deployed（payload 带
	// reason created|updated），backend 离开 victorialogs 服务移除发
	// removed（payload 带 volume_retained=true）。VL 无凭据面，payload
	// 零敏感材料）。
	{Name: "logs.victorialogs_deployed", Summary: "managed VictoriaLogs deployed or converged to the desired spec (payload carries service/image/reason)"},
	{Name: "logs.victorialogs_removed", Summary: "managed VictoriaLogs removed after logs.backend left victorialogs (data volume retained; payload carries volume_retained=true)"},
	// 发出来源：hub 入湖批量器 streak 翻转沿（internal/logs/ingest.go——
	// 去抖：状态翻转才发，不逐行红；payload 带 dropped_total 与单行化
	// 错误摘要，无任何日志行内容/凭据材料）。
	{Name: "logs.ingest_degraded", Summary: "log ingestion to VictoriaLogs degraded (backend unreachable; live tail unaffected; payload carries dropped_total and a single-line error summary)"},
	{Name: "logs.ingest_recovered", Summary: "log ingestion to VictoriaLogs recovered (degradation streak exited)"},

	// ── 观测/metrics 三件套（E6 观测专项设计 §4/§7，W5-S3 接线；注册表
	//    只增。D-W5-2 opt-in——缺省零常驻，启用/停用是显式操作）──
	// 发出来源：SaveMetricsSettings 保存事务（internal/state/
	// metricssettings.go，与业务写同事务 = Outbox；payload 只带模式值，
	// 词表内枚举）。
	{Name: "metrics.mode_updated", Summary: "metrics mode setting changed (payload carries the mode value; switching on deploys the managed VictoriaMetrics/cAdvisor/node-exporter stack, switching off removes the services — the data volume is retained)"},
	// 发出来源：metrics duty 收敛拍差分（internal/metrics/metrics.go——服务
	// 缺失创建/spec 漂移更新各发一条 deployed（payload 带 service/image/
	// reason created|updated），mode 离开 on 三件移除发 removed（payload 带
	// volume_retained=true——数据卷保留语义的显性化面）。全链无凭据面，
	// payload 零敏感材料。
	{Name: "metrics.stack_deployed", Summary: "managed metrics stack service deployed or converged to the desired spec (payload carries service/image/reason)"},
	{Name: "metrics.stack_removed", Summary: "managed metrics stack removed after metrics.mode left on (data volume retained; payload carries volume_retained=true)"},

	// ── Web 终端（E7 设计 §2.4，W5-S6 接线；注册表只增）──
	// 发出来源：控制面 hub（internal/execrelay hub.go——会话受理与收尾，
	// 与审计 terminal.opened/closed 同事务 = Outbox；subject = app:<name>）。
	// payload 只带元数据（app/service/container/node/token/session/时长/
	// 关闭码/原因）——**会话内容（PTY 字节）零出现**（明文纪律，设计 §2.4
	// 「payload 不含会话内容」）。
	{Name: "terminal.opened", Summary: "web terminal session opened (payload carries app/service/container/node/token metadata only, never session content)"},
	{Name: "terminal.closed", Summary: "web terminal session closed (payload carries duration, close code and reason; metadata only, never session content)"},

	// ── 认证/用户面（v0.3 W1，RBAC 设计 §6 事件清单的注册态三类；注册表
	//    只增。metadata-only，零 secret——通知反环路红线沿用）：发出来源 =
	//    自助注册组合事务（internal/state/register.go，与用户/团队/项目
	//    写入同事务 = Outbox；auth.login/logout 族不落事件，仅审计）──
	{Name: "user.registered", Summary: "a user completed self-registration (payload carries email/is_platform_admin/team_id/team_slug, never credentials; first user is platform admin with a personal team + default project)"},
	{Name: "team.created", Summary: "a team was created (payload carries slug/name; W1 source = the personal team provisioned atomically with registration, W2-S1 source = TeamsService.CreateTeam)"},
	{Name: "project.created", Summary: "a project was created (payload carries team_id/slug; W1 source = the default project provisioned atomically with registration, W2-S1 source = ProjectsService.CreateProject)"},

	// ── 团队/项目面（v0.3 W2-S1，RBAC 设计 §6 事件清单余下四项；注册表
	//    只增。metadata-only，零 secret——通知反环路红线沿用）：发出来源 =
	//    internal/state 的 teams.go/projects.go 原语（与业务写同事务 =
	//    Outbox）。「成员/角色管理三动作在审计面分记 member_added/
	//    member_role_changed/member_removed，事件面收敛为单一
	//    team.member_changed（payload change 字段区分）」——设计 §6 的
	//    审计动作与事件清单是两张表，事件取清单原文 ──
	// 发出来源：AddMember / SetMemberRole / RemoveMember
	//（internal/state/teams.go，payload change=added|role_changed|removed；
	// RemoveMember 附带 project_overrides_removed 联动清理计数）。
	{Name: "team.member_changed", Summary: "team membership changed — a member added, role changed or removed (payload carries change/user_id/role or from/to; removal carries the purged project-override count)"},
	// 发出来源：ConsumeInvite（internal/state/teams.go，invite 一次性消费
	// 同事务；subject = invite:<id>，payload 带 team_id/user_id/role）。
	{Name: "invite.accepted", Summary: "a one-time team invite was accepted (payload carries team_id/user_id/role; the plaintext token never appears anywhere)"},
	// 发出来源：DeleteProject（internal/state/projects.go，非空守卫通过后的
	// 删除事务；payload 带 team_id/slug——行已删，slug 供事件读者定位）。
	{Name: "project.deleted", Summary: "an empty project was deleted (non-empty guard passed; payload carries team_id/slug)"},
	// 发出来源：SetProjectMemberRole / RemoveProjectMember
	//（internal/state/projects.go，队内覆写行 upsert/删除；payload
	// change=override_set 带 role、override_removed）。owner 恒不可覆写
	//（设计 §3.3），覆写事件只涉及三档项目角色。
	{Name: "project.member_changed", Summary: "a project role override was set or removed (payload carries change/user_id and role on override_set; no row means the team role applies again)"},
	// IMPL-T15-1 实现期新增（OT-1 项目网参与，注册表只增）：发出来源 =
	// internal/state/projectnetworks.go 的参与位置位/清位原语（与业务写同
	// 事务 = Outbox；API attach/detach 唯一消费点）。payload 带
	// change=attached|detached + app/app_id/project_id，零敏感材料；项目网
	// 名由 project_id 按 naming 公式现推（不重复落）。
	{Name: "project.network_changed", Summary: "an app was attached to or detached from its project network (payload carries change/app/app_id/project_id; participation is an explicit opt-in and defaults off)"},
	// IMPL-T15-1 实现期新增（OT-1/DT-5 对账兜底守卫，注册表只增）：发出来源 =
	// internal/engine 的 substrateRecon networks 面（自描述 label + 期望集差
	// 识别 state 外平台前缀网）。只披露不静默删（清理归各自归属路径/人工）；
	// 持续形态每进程只报一次（seen 记忆，恢复后清零可再报）。payload 带
	// 网络名与归因摘要，零敏感材料。
	{Name: "network.orphaned", Summary: "a platform-managed fleetly- network exists that no state fact attributes to a live resource (unattributed leak; disclosed only — never deleted silently)"},
	// 发出来源同上（state→swarm 方向）：成员项目的项目网在底座缺失（外部
	// 移除）；重建归 attach/发布链路的 NetworkEnsure（对账不自动重建，
	// 与 app.substrate_missing 同纪律）。
	{Name: "network.missing", Summary: "an expected project network is absent from the substrate (external removal); disclosed only — the next attach or member deploy re-ensures it"},

	// ── git SSH host key（v0.3 W3-S2，rbac-teams §6 裁决 D-W0-8 FZ-12；
	//    注册表只增）：发出来源 = host key 启动装载与指纹台账的比对事务
	//（internal/state/hostkeysettings.go，与台账更新同事务 = Outbox）。
	//    payload 只带新旧 SHA256 指纹——公钥指纹是公开材料（known_hosts
	//    核对值），私钥文件本体绝不出现（state-model §2.9 secret 纪律）。
	//    首启建账静默（零事件），装载指纹与台账不同才发。
	{Name: "git.hostkey_changed", Summary: "the git SSH host key changed since the previous load (file rebuilt or key replaced; payload carries the old and new SHA256 fingerprints — public key material only, the private key never appears)"},

	// ── 自动扩缩（B 线 W5 设计 §1，D-V3W5-2，v0.3 W5-S1 接线；注册表只增。
	//    发出来源 = engine 收敛拍尾部的扩缩 duty，internal/engine/
	//    autoscaling.go）。策略 CRUD 零事件（审计 scaling.policy_changed 承
	//    载——设计 §1.2 的事件面是运行期动作与披露）──
	// 副本调整动作：payload 带 service/dimension（cpu|mem|cpu+mem）/
	// replicas_before/replicas_after/实测水位百分数。
	{Name: "scaling.adjusted", Summary: "autoscaler adjusted a service's replica count (payload carries the triggering dimension, before/after replicas and measured utilization)"},
	// 披露面（每策略一次性；条件解除后可再披露）：
	{Name: "scaling.dormant", Summary: "a scaling policy is dormant because metrics.mode is not on (one-time disclosure per policy; re-armed when metrics turns on)"},
	{Name: "scaling.no_data", Summary: "a scaling policy had no metric series for its target dimension — no action, honest no-data (one-time disclosure per policy)"},
}
