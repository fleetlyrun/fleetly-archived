package errcode

// builtins 是文档域清单的全量录入（唯一真源为注册表；此处每码注明文档
// 出处）。分布核对：release-semantics §2.7（17 E + 3 W）、stateful-placement
// §2.8（8 E + 1 W）与 §2.9（1 E）、state-model §2.7/§2.9/§2.4/§2.2（4 E）、
// architecture §2.4（3 码）+ §2.3（E_STATE_VERSION_CONFLICT）；v0.3 W1 增
// E_REGISTRATION_CLOSED（rbac-teams §2.1）；v0.3 W2-S1 增 E_TEAM_LAST_OWNER /
// E_INVITE_INVALID / E_TEAM_SLUG_RESERVED（rbac-teams §5）。
// 计 84 个 E_ + 5 个 W_ = 89 码（逐波注记见各分节与 errcode_test 的计数
// 注释；v0.3 W2-S3 随保留字迁移退役 E_APP_NAME_RESERVED——设计明示的
// 唯一减码；T 线 DT-6/IMPL-T2-2 增 E_BUILD_UPLOAD_TOO_LARGE /
// E_BUILD_UPLOAD_INVALID——上传构建面流式超限与形态违约）。
//
// HTTP 默认映射：文档显式给定的照文档（E_DOMAIN_CONFLICT/E_STATE_VERSION_
// CONFLICT/E_VOLUME_NODE_MISMATCH/E_PLACEMENT_MOVE_REQUIRES_ACK→409、
// E_EVENT_CURSOR_EXPIRED→410、E_LABEL_RESERVED/E_PLACEMENT_LABEL_CONFLICT/
// E_PLACEMENT_NODE_INVALID/E_PLACEMENT_NODE_NOT_FOUND→422）；文档未给定的
// 取保守缺省（请求契约违约→400、请求与状态冲突→409、底座/管线故障→500/
// 503），待 T0.5 契约冻结复核。
var builtins = []Code{
	// ── compose 契约（architecture §2.4、release-semantics §2.7/§2.8）──
	{ID: "E_COMPOSE_UNSUPPORTED", HTTP: 400,
		Summary:    "compose declaration falls outside the controlled subset/deny list; validation rejects it explicitly",
		Suggestion: "Remove or rewrite the unsupported compose fields and retry; see the error code docs for the controlled subset and deny list."},
	{ID: "E_COMPOSE_MANAGED_FIELD", HTTP: 400,
		Summary:    "compose managed-field value violates platform governance (failure_action/monitor)",
		Suggestion: "Set deploy.update_config.failure_action to pause (or omit it) and monitor to 5s (or omit it), then retry."},
	{ID: "E_COMPOSE_UNSAFE_STRATEGY", HTTP: 400,
		Summary:    "explicit start-first conflicts with platform-enforced stop-first (volumes/fixed host ports/global)",
		Suggestion: "Set deploy.update_config.order to start-first or omit it; services with volumes or fixed host ports are forced to stop-first by the platform."},

	// ── 域名/TLS（architecture §2.4）──
	{ID: "E_DOMAIN_CONFLICT", HTTP: 409,
		Summary:    "the same domain appears on two services of the same app",
		Suggestion: "The domain is already claimed by another service of this app: remove it from one of them before declaring it."},
	{ID: "E_DOMAIN_UNSUPPORTED", HTTP: 400,
		Summary:    "domain form unsupported (e.g. wildcards require DNS-01 from v0.2)",
		Suggestion: "Use a concrete domain; see the error code docs for wildcard and certificate support scope."},

	// ── 发布管线（release-semantics §2.7 场景矩阵）──
	{ID: "E_BUILD_FAILED", HTTP: 500,
		Summary:    "build stage failed (Railpack/BuildKit)",
		Suggestion: "Check the build logs to locate the compile/dependency error, fix it, and deploy again."},
	{ID: "E_IMAGE_PULL_FAILED", HTTP: 500,
		Summary:    "image pull failed (registry unreachable/invalid credentials/network failure)",
		Suggestion: "Verify the image reference and registry credentials are valid and the node can reach the registry, then retry."},
	{ID: "E_IMAGE_UNAVAILABLE", HTTP: 500,
		Summary:    "rollback target image is no longer available (digest stale/pruned)",
		Suggestion: "The rollback target image is no longer available: replay the most recent usable revision instead, or rebuild and deploy."},
	{ID: "E_SCHEDULER_PENDING_TIMEOUT", HTTP: 500,
		Summary:    "task stuck in PENDING past the deploy watchdog (placement constraints/insufficient resources/node unavailable)",
		Suggestion: "Check node resources and placement constraints (whether the bound node is ready); free resources or adjust constraints and retry."},
	{ID: "E_TASK_START_FAILED", HTTP: 500,
		Summary:    "new task failed to start (image entrypoint/resource limits/runtime error)",
		Suggestion: "Check the task logs to locate the start failure, fix it, and retry."},
	// ── 部署期一次性作业（DT-4，torchwood 线；IMPL-T1-3 只增）──
	// 消费点 = internal/engine init 相位评估：release 管线在晋级前以新 spec
	// 跑 fleetly.job: init 声明的一次性 job。失败与超时分别落码（与
	// E_TASK_START_FAILED / E_HEALTH_TIMEOUT 同一分层归因习惯）；job 名与
	// 原因进 deployment.failed detail 与 release.job_* 事件。
	{ID: "E_INIT_JOB_FAILED", HTTP: 500,
		Summary:    "release-time init job failed (task failed/rejected/shut down): the deployment fails before the new revision is promoted",
		Suggestion: "Check the init job logs (attributed to the declaring compose service) to locate the failure, fix the job, and deploy again; completed jobs are not rolled back (migrations are forward-only)."},
	{ID: "E_INIT_JOB_TIMED_OUT", HTTP: 500,
		Summary:    "release-time init job exceeded its watchdog budget (default 10m, fleetly.job.timeout label overrides): the deployment fails before promotion",
		Suggestion: "Raise fleetly.job.timeout on the init service (a positive Go duration such as 30m) or speed up the migration, then deploy again."},
	{ID: "E_HEALTH_TIMEOUT", HTTP: 500,
		Summary:    "health gate timeout: new task did not pass healthcheck within budget",
		Suggestion: "Verify the healthcheck command and port are correct and the app can pass the health check within budget."},
	{ID: "E_OBSERVE_CRASH_LOOP", HTTP: 500,
		Summary:    "observe window verdict: crash loop",
		Suggestion: "The process exited repeatedly during the observe window: check app logs to locate the crash cause and roll back manually if needed."},
	{ID: "E_OBSERVE_UNHEALTHY", HTTP: 500,
		Summary:    "observe window verdict: unstable (never reached healthy)",
		Suggestion: "The health verdict failed during the observe window: check app logs and health endpoints, and roll back manually if needed."},
	{ID: "E_DEPLOY_INTERRUPTED", HTTP: 500,
		Summary:    "deploy interrupted (control plane stopped/node failure), state machine not completed",
		Suggestion: "Start a new deploy to recover; if the traffic switch had not happened yet, traffic was unaffected."},
	{ID: "E_DEPLOY_POST_WINDOW_UNSTABLE", HTTP: 500,
		Summary:    "unstable verdict after the observe window passed (platform only alerts, no automatic rollback)",
		Suggestion: "Check app logs to assess the impact; manual rollback is advised (no automatic rollback after the observe window)."},
	{ID: "E_DEPLOY_DOWNTIME_FAILED", HTTP: 500,
		Summary:    "stop-first downtime switch failed and forced recovery failed (permanent outage risk path)",
		Suggestion: "Fix the start failure and deploy again; this path forces recovery and cannot roll back."},
	{ID: "E_ROLLBACK_FAILED", HTTP: 500,
		Summary:    "rollback failed (single-layer replay of the revision)",
		Suggestion: "Check the deployment record and logs to locate the replay failure; rollback can be started again."},
	{ID: "E_ROLLBACK_NO_TARGET", HTTP: 409,
		Summary:    "no replayable rollback target (outside the 5 most recent verified revisions/first deploy failed)",
		Suggestion: "No replayable revision in history: deploy a new revision instead (default retention is 5 revisions)."},
	{ID: "E_RUNTIME_UNAVAILABLE", HTTP: 503,
		Summary:    "Swarm/node substrate unreachable or operation failed",
		Suggestion: "Check Docker Engine and node status and retry after the substrate recovers; the reconciler converges the observation cache automatically."},

	// ── 破坏性变更门控（MG-C3 实现期新增，architecture §2.4 plan/apply
	//    语义「破坏性操作要求 --confirm-destructive」；文档外码单独列出，
	//    待 T0.5 契约冻结确认）──
	{ID: "E_DEPLOY_CONFIRM_REQUIRED", HTTP: 409,
		Summary:    "deploy contains destructive changes (service removal/volume unbind) without the confirmation flag; rejected from queueing",
		Suggestion: "This deploy will remove services or unbind volumes relative to the latest revision: confirm the intent and retry with --confirm-destructive."},

	// ── 有状态放置（stateful-placement §2.8/§2.5/§2.6）──
	{ID: "E_PLACEMENT_NODE_INVALID", HTTP: 422,
		Summary:    "failed to resolve node name/ID in the placement label (422 + candidate list)",
		Suggestion: "Check the fleetly.placement.node value (a node name or platform node ID); see context for the candidate node list."},
	{ID: "E_PLACEMENT_NODE_NOT_FOUND", HTTP: 422,
		Summary:    "node referenced by the placement label does not exist or was removed",
		Suggestion: "Pick a valid node from the node list and retry."},
	{ID: "E_PLACEMENT_NODE_UNAVAILABLE", HTTP: 503,
		Summary:    "bound node is currently not ready (deploy preflight fails fast, no queueing)",
		Suggestion: "Wait for the bound node to become ready, or change the binding via rebind after confirming data safety."},
	{ID: "E_PLACEMENT_NODE_GONE", HTTP: 409,
		Summary:    "bound node was removed (blocked(node_gone)); in-flight deploys fail with this code",
		Suggestion: "rebind --data-restored after data recovery, or rebind --discard after confirming discard (admin+confirm+audited)."},
	{ID: "E_PLACEMENT_NO_ELIGIBLE_NODE", HTTP: 503,
		Summary:    "no candidate for automatic placement (no ready node satisfies the constraints)",
		Suggestion: "Check node readiness and placement constraints; retry after a candidate node recovers."},
	// 预留→退役面（E1-7 追认）：v0.1 单机无第二候选（MoveBinding 守卫
	// 拒绝）；v0.2 多节点落地后守卫路径退役，码保留注册表、永不复用，
	// summary 已由「v0.1 single-node」措辞维护为拓扑/配置语义（multi-node
	// §5.2 退役面 + 保留码，注册表文案维护、非码变更）。
	{ID: "E_PLACEMENT_MOVE_REQUIRES_ACK", HTTP: 409,
		Summary:    "cross-node placement moves require explicit confirmation (data disposition and/or destructive-confirm missing)",
		Suggestion: "A cross-node move is only valid with an explicit data disposition: --data-restored after the restore flow, or --discard with --confirm-destructive (admin+confirm+audited)."},
	{ID: "E_PLACEMENT_LABEL_CONFLICT", HTTP: 422,
		Summary:    "placement labels of multiple services in one app point to different nodes",
		Suggestion: "Set fleetly.placement.node to the same node for all services of the app."},
	{ID: "E_VOLUME_NODE_MISMATCH", HTTP: 409,
		Summary:    "volume data node ≠ target deploy node (data-safety sentinel that turns the empty-volume incident into a 409)",
		Suggestion: "Declare a data disposition and retry: --data-restored (rebuilt by the restore flow) or --discard (admin+confirm; the old volume becomes orphaned)."},
	// 退役面 + 保留码（E1-7 追认，multi-node §5.2）：v0.1 的多节点操作
	// 守卫路径已随 E1-7 删除；码保留注册表（永不复用、永不删码），summary
	// 由「v0.1 single-node」措辞维护为拓扑/配置语义（注册表文案维护、
	// 非码变更，契约轮追认口径——阶段 3 E1-5 同款）。
	{ID: "E_CAPABILITY_REQUIRES_MULTI_NODE", HTTP: 400,
		Summary:    "multi-node operations are unavailable on the current topology or configuration (retired guard path; the code is retained and never reused)",
		Suggestion: "This operation requires a multi-node topology with the platform base domain configured; multi-node support ships with v0.2 (see the join guide: fleetly nodes join-guide)."},

	// ── 备份与恢复（state-model §2.7）──
	// 预留：恢复器未实现（横切评审确认；usage_test 的豁免清单同理由）。
	{ID: "E_BACKUP_KEY_MISSING", HTTP: 500,
		Summary:    "restore verification failed: checksum/master key fingerprint mismatch; half-restored state is rejected",
		Suggestion: "Provide the correct backup set and master key; the restore flow rejects half-restored state — do not hand-assemble state."},

	// ── 事件流（state-model §2.9）──
	{ID: "E_EVENT_CURSOR_EXPIRED", HTTP: 410,
		Summary:    "event cursor predates the retention window (30 days); explicit gap",
		Suggestion: "Re-fetch all events starting from the oldest_seq in the response."},

	// ── 多节点 registry（E1 多节点设计 §5.2/D-MN-11，2026-09-20 冻结）：
	//    registry 模式部署前哨与推送的分层归因——registry 错 → 查 zot/网络/
	//    凭据，镜像缺 → 重建；快速失败不排队。manifest 缺失复用
	//    E_IMAGE_UNAVAILABLE（语义同「回滚目标不可得」），不另立新码 ──
	{ID: "E_REGISTRY_UNAVAILABLE", HTTP: 503,
		Summary:    "registry-mode deploy preflight: the platform registry is unreachable (fail fast, no queueing)",
		Suggestion: "The platform registry did not answer: check the fleetly-registry service on the manager and the registry.<base> route (Traefik); deploys fail fast until the registry responds."},
	{ID: "E_REGISTRY_PUSH_FAILED", HTTP: 500,
		Summary:    "pushing the build result to the platform registry failed (network/credentials/registry fault)",
		Suggestion: "The push to registry.<base> failed: check that the fleetly-registry service is healthy and the platform registry credentials (registry.auth_file) are current, then run the build again."},

	// ── 命名保留字（W3 遗留撞键票收口，2026-09-21；v0.3 W2-S3 退役）：
	//    E_APP_NAME_RESERVED 随保留字迁移退役（rbac-teams §4.3：v0.3 命名
	//    三段化后 app 名不再紧邻 fleetly- 前缀——结构性安全，保留字清单
	//    整体迁到 team slug〔E_TEAM_SLUG_RESERVED 消费〕，app 名守卫的
	//    消费点 internal/compose 受理层同步移除）。退役是设计明示的减码
	//    （§5「E_APP_NAME_RESERVED 随保留字迁移退役」），非注册表语义
	//    漂移；v0.2.x 终点版文档保留该码的档案记载。──

	// ── 多节点 join 门禁（E1 多节点设计 §5.2/D-MN-13，2026-09-20 冻结，
	//    E1-8 接线）：base_domain 缺失即多节点未启用——join 面显式拒绝、
	//    不静默降级（provider 通道/zot 均不可用，join 后入口残缺）──
	{ID: "E_MULTI_NODE_REQUIRES_BASE_DOMAIN", HTTP: 409,
		Summary:    "multi-node is not enabled: the platform base domain is not configured (join face refuses instead of silently degrading)",
		Suggestion: "Configure base_domain (installer --base-domain) before joining nodes: the config endpoint (8423 TLS), platform subdomains and the zot registry all derive from it."},

	// ── 入口路由（T2.15；架构 §2.5/§2.6：路由发布严格晚于健康门，发布
	//    失败不回滚部署、单独告警——deployment 仍可成功，错误落审计与本码）──
	{ID: "E_ROUTE_PUBLISH_FAILED", HTTP: 503,
		Summary:    "ingress route publish failed (deploy unaffected; route alerts separately)",
		Suggestion: "Check the route.publish_failed event and fleetly ingress status; once the substrate/ingress recovers, it converges automatically with the next deploy or republish."},

	// ── label 契约（state-model §2.4）──
	{ID: "E_LABEL_RESERVED", HTTP: 422,
		Summary:    "user occupied the reserved namespace fleetly.*",
		Suggestion: "fleetly.* is a platform-reserved namespace: use a different label prefix."},

	// ── 对象存储 S3 面（E3 对象存储专项设计 §5.2，2026-09-21 裁决轮落定；
	//    注册表只增）──
	// E_S3_NOT_CONFIGURED 已随 E3-4 接线（label fleetly.s3=true 注入前哨
	// ——s3.mode=unset 时 plan 阶段诚实拒绝，设计 §2.4；消费点 =
	// internal/engine/s3inject.go resolveS3Injection）。
	{ID: "E_S3_NOT_CONFIGURED", HTTP: 409,
		Summary:    "a service declares label fleetly.s3=true but object storage is not configured (s3.mode=unset); deploy plan refuses instead of injecting empty env",
		Suggestion: "Configure object storage first: run 'fleetly s3 set' (or the Console S3 settings card) to set s3.mode=external or rustfs, then deploy again."},
	{ID: "E_S3_CONFIG_CONFLICT", HTTP: 409,
		Summary:    "s3 settings conflict: mode=external requires endpoint/bucket/keys while mode=rustfs requires them empty (two mutually exclusive fact sources; silent overwrite is rejected)",
		Suggestion: "s3.mode=external and s3.mode=rustfs are mutually exclusive: clear the external endpoint fields before switching to rustfs, or fill them all before switching to external."},
	{ID: "E_S3_TEST_FAILED", HTTP: 503,
		Summary:    "S3 connection test failed (probe put→get→delete; the failed step and the underlying error travel in the envelope context; this is a reachable-endpoint fault, not a TCP probe)",
		Suggestion: "Fix the endpoint/credentials per the failed probe step (put=auth/write, get=readback, delete=cleanup) and test again; the honest contract is: test passed = can authenticate, can write, can read back."},
	{ID: "E_S3_PUBLIC_REQUIRES_BASE_DOMAIN", HTTP: 409,
		Summary:    "s3.public_exposed requires a platform base domain (public subdomain s3.<base> derivation); refused in the single-node form without one",
		Suggestion: "Configure base_domain (installer --base-domain) before exposing RustFS on the public subdomain s3.<base>; keep public_exposed=false for internal-only access."},

	// ── 状态与并发（state-model §2.2、architecture §2.3）──
	{ID: "E_STATE_VERSION_CONFLICT", HTTP: 409,
		Summary:    "read-before-write optimistic concurrency conflict (object version token invalid)",
		Suggestion: "The object was modified concurrently: re-read the latest state and retry with the new version token."},

	// ── 数据库托管 E4（managed-databases 设计 §5.2，2026-09-20 冻结；注册表
	//    只增。D-DB-8：状态机前置态/CAS 冲突复用 E_STATE_VERSION_CONFLICT
	//    不另立码。S1 阶段注册 + E_ENV_KEY_RESERVED 随守卫接线；其余码的
	//    生产引用随 S2-S5 票据落地，usage_test 豁免清单同步注记）──
	{ID: "E_DB_NOT_FOUND", HTTP: 404,
		Summary:    "the referenced/operated database instance does not exist or has entered deleting/deleted (candidate list attached)",
		Suggestion: "Check the database instance name against the database list; references are validated for existence only (not readiness — a not-ready database yields a plan warning instead)."},
	{ID: "E_DB_REFERENCED", HTTP: 409,
		Summary:    "database deletion refused while app references exist (reference list attached; data-safety sentinel)",
		Suggestion: "Remove the fleetly.databases labels from the referencing apps and redeploy them first (the reference list in the error context names every blocking app/service)."},
	{ID: "E_DB_TEMPLATE_UNSUPPORTED", HTTP: 400,
		Summary:    "unknown template id or a settings change violates the template-managed surface (image/engine parameters are not user-editable)",
		Suggestion: "Use one of the built-in template ids (postgres-16, postgres-18, percona-postgresql-18, redis-7, mysql-8.4, mongodb-8.0) and limit settings changes to resource limits and the backup plan."},
	{ID: "E_DB_ENV_PREFIX_CONFLICT", HTTP: 422,
		Summary:    "two databases referenced by the same app derive the same env prefix (e.g. pg-prod vs pg_prod)",
		Suggestion: "Rename one of the database instances (or reference only one per colliding pair in this app): env prefixes are derived from instance names by uppercasing with '-' mapped to '_'."},
	{ID: "E_DB_BACKUP_FAILED", HTTP: 500,
		Summary:    "database backup job failed (resource-terminal class; surfaced primarily in the backup ledger and events)",
		Suggestion: "Check the db_backups ledger error column and the db.backup_failed event for the failure summary; fix the cause and trigger the backup again."},
	{ID: "E_DB_RESTORE_FAILED", HTTP: 500,
		Summary:    "database restore failed (an interrupted in-place restore is a critical alert, not a retry-quietly path)",
		Suggestion: "Follow the restore runbook: verify the instance state and volume data, then re-run the restore from the same snapshot after fixing the cause."},
	{ID: "E_DB_ROTATE_FAILED", HTTP: 500,
		Summary:    "credential rotation failed mid-flight (the completed stages are attached; manual completion may be required)",
		Suggestion: "Check the attached completed stages and the instance state, finish the remaining stage manually or retry the rotation, then verify the referencing apps redeploy."},
	{ID: "E_SECRET_NOT_FOUND", HTTP: 422,
		Summary:    "a compose-declared external secret is not in the platform secret store (deploy preflight)",
		Suggestion: "Create the secret first (fleetly secrets set) with the exact declared name, then deploy again; compose secrets must declare external: true."},
	// T 线 OT-3/IMPL-T1-4（注册表只增）：明文配置资源的缺失哨兵（compose
	// configs 声明的声明名不在平台 app_configs）。消费点 =
	// internal/engine/configinject.go preparing 期前哨与快照重放的悬空名
	// （内容换版后旧对象名不再解析——诚实失败不静默改写）。
	{ID: "E_CONFIG_NOT_FOUND", HTTP: 422,
		Summary:    "a compose-declared external config is not in the platform config store (deploy preflight)",
		Suggestion: "Create the config first (fleetly configs set) with the exact declared name, then deploy again; compose configs must declare external: true."},
	{ID: "E_ENV_KEY_RESERVED", HTTP: 422,
		Summary:    "user wrote into the reserved FLEETLY_ env namespace (template connection-string materialization keys)",
		Suggestion: "FLEETLY_* env keys are platform-reserved (database connection materialization): rename the key without the FLEETLY_ prefix; system-source platform writes are unaffected."},

	// ── token 管理（M4-6 实现期新增，评审整改 B5；文档外码单独列出，
	//    待 T0.5 契约冻结确认）──
	{ID: "E_TOKEN_LAST_ADMIN", HTTP: 409,
		Summary:    "last-admin guard: revoking would leave no unrevoked admin token on the platform; self-lockout rejected",
		Suggestion: "This is the last unrevoked admin token: create a new admin token before revoking this one (otherwise the platform becomes unmanageable, and the bootstrap token is not reseeded on restart)."},

	// ── 观测/日志库（E6 观测专项设计 §3.1，W5-S1；注册表只增）──
	// 消费点：SearchLogs 的两处诚实分支（internal/api/logs.go）——① 当前
	// backend=jsonl（检索面只在日志库，不返回空列表冒充）；② VictoriaLogs
	// 不可达（检索降级，直播面不受影响——A3 直读不动条款）。
	{ID: "E_LOGS_BACKEND_UNAVAILABLE", HTTP: 503,
		Summary:    "the unified log search face is unavailable (logs.backend=jsonl has no search face, or VictoriaLogs did not answer; live log tail is unaffected)",
		Suggestion: "Check the current backend with 'fleetly logs backend show'. If it is jsonl, switch to victorialogs to enable search; if VictoriaLogs is unreachable, check the fleetly-victorialogs service — search recovers automatically once it answers (the degradation streak and dropped counter are surfaced in the same view)."},

	// ── 观测/metrics 查询面（E6 观测专项设计 §4.2，W5-S3；注册表只增）──
	// 消费点：SearchMetrics 的两处诚实分支（internal/api/metrics.go）——
	// ① metrics.mode 未开（opt-in 默认关——不返回空序列冒充有数）；
	// ② VictoriaMetrics 不可达（查询面降级，采集面不受影响）。PromQL 透传
	// 是操作员工具：坏表达式由 VM 拒绝，走退化信封 InvalidArgument（不走
	// 本码族——详见 api 层）。
	{ID: "E_METRICS_NOT_ENABLED", HTTP: 409,
		Summary:    "metrics collection is not enabled (metrics.mode=unset): the query face is opt-in and has nothing to report",
		Suggestion: "Enable the metrics stack with 'fleetly metrics mode set on' (or the Console metrics card). Once the three managed services converge, SearchMetrics starts serving; the data volume survives disabling, so history resumes from where it stopped."},
	{ID: "E_METRICS_BACKEND_UNAVAILABLE", HTTP: 503,
		Summary:    "the metrics query face is unavailable (VictoriaMetrics did not answer on the loopback face; the scrape face is unaffected)",
		Suggestion: "Check the managed services with 'fleetly metrics status'. If the stack is still converging, wait for the services to appear; if VictoriaMetrics is running, verify the 127.0.0.1 loopback probe — the query face recovers automatically once it answers."},

	// ── 通知 Webhook 面（E6 观测专项设计 §5，W5-S4；注册表只增，族按需
	//    注册）──
	// 消费点：internal/api/notifications.go 的端点读取/更新/删除/轮换/测试
	// 路径（state 层哨兵 ErrWebhookNotFound 的信封化投影，404）。
	{ID: "E_WEBHOOK_NOT_FOUND", HTTP: 404,
		Summary:    "the webhook endpoint referenced by the operation does not exist (or was already deleted)",
		Suggestion: "List the endpoints with 'fleetly notifications endpoint list' and retry with an existing name or id. Deleting an endpoint also removes its delivery ledger rows."},
	// 消费点：CreateWebhookEndpoint / UpdateWebhookEndpoint 的重名守卫
	//（state 层哨兵 ErrWebhookNameConflict 的信封化投影，409）。
	{ID: "E_WEBHOOK_NAME_CONFLICT", HTTP: 409,
		Summary:    "a webhook endpoint with the same name already exists (endpoint names are unique)",
		Suggestion: "Pick another endpoint name (or delete the old endpoint first); names are the operator-facing handle used by the CLI and Console."},
	// 消费点：订阅模式集校验（internal/state/webhooks.go
	// ValidateWebhookPatterns——非空数组 + 白名单字符 + `*` 通配；422 语义
	// 违约与 E_ENV_KEY_RESERVED 同级）。
	{ID: "E_WEBHOOK_PATTERN_INVALID", HTTP: 422,
		Summary:    "a webhook event pattern is invalid (patterns must be a non-empty list of 1..128 chars from [a-z0-9._-*]; '*' is the wildcard)",
		Suggestion: "Use event-name glob patterns like \"deployment.*\", \"cron.failed\" or \"*\" (matches everything); see 'fleetly events watch' for the event vocabulary the patterns match against."},

	// ── 告警面（B 线 W5 设计 §2，D-V3W5-1，W5-S2；注册表只增）──
	// 消费点：E_ALERTS_METRICS_REQUIRED = alerts.mode=on 的前置门
	//（internal/state/alertsettings.go SaveAlertsSettings——设计 §2.1「前置
	// 门 = metrics.mode=on，否则 409 带指引」）；E_ALERT_RULE_NOT_FOUND /
	// E_ALERT_RULE_NAME_CONFLICT = alert_rules CRUD 的 state 哨兵信封化投影
	//（internal/api/alerting.go，404/409）；E_ALERT_RULE_EXPR_INVALID /
	// E_ALERT_RULE_FOR_INVALID / E_ALERT_RULE_LABELS_INVALID /
	// E_ALERT_RULE_CHANNELS_INVALID = 规则形状校验（internal/state/
	// alerting.go——422 语义违约，与 E_WEBHOOK_PATTERN_INVALID 同级）。
	{ID: "E_ALERTS_METRICS_REQUIRED", HTTP: 409,
		Summary:    "alerts.mode=on requires metrics.mode=on first (vmalert has no datasource without the managed VictoriaMetrics)",
		Suggestion: "Enable the metrics stack first ('fleetly metrics mode set on' or the Console metrics card), wait for the three managed services to converge, then switch alerts on ('fleetly alerts mode on')."},
	{ID: "E_ALERT_RULE_NOT_FOUND", HTTP: 404,
		Summary:    "the alert rule referenced by the operation does not exist (or was already deleted)",
		Suggestion: "List the rules with 'fleetly alerts rules ls' and retry with an existing id or name."},
	{ID: "E_ALERT_RULE_NAME_CONFLICT", HTTP: 409,
		Summary:    "an alert rule with the same name already exists (rule names are unique platform-wide — alerting is a platform-admin domain)",
		Suggestion: "Pick another rule name (or delete the old rule first); names are the operator-facing handle used by the CLI and Console."},
	{ID: "E_ALERT_RULE_EXPR_INVALID", HTTP: 422,
		Summary:    "an alert rule expr is invalid (must be a non-empty PromQL expression of at most 2048 chars)",
		Suggestion: "Write the PromQL condition that should evaluate to a series when the alert should fire (e.g. 'up == 0'); syntax is validated by vmalert/VictoriaMetrics — use 'fleetly alerts rules test' to evaluate it before saving."},
	{ID: "E_ALERT_RULE_FOR_INVALID", HTTP: 422,
		Summary:    "an alert rule for_duration is invalid (must be >= 0 seconds; 0 = no for clause)",
		Suggestion: "Pass a non-negative number of seconds (e.g. --for 300 for a five-minute sustained condition before firing)."},
	{ID: "E_ALERT_RULE_LABELS_INVALID", HTTP: 422,
		Summary:    "an alert rule labels object is invalid (keys must be non-empty; values are strings; the severity label drives the notification text)",
		Suggestion: "Provide a JSON object of string to string, e.g. {\"severity\":\"critical\"}; an empty object is valid."},
	{ID: "E_ALERT_RULE_CHANNELS_INVALID", HTTP: 422,
		Summary:    "an alert rule channels list is invalid (must be a list of non-empty notification endpoint ids; an empty list means deliver to all enabled endpoints)",
		Suggestion: "List the notification endpoints with 'fleetly notifications endpoint list' and pass their ids (comma-separated on the CLI); leave it empty to fan out to every enabled endpoint."},

	// ── ACME DNS-01 通配证书面（B 线 W5 设计 §3，D-V3W5-3/D-V3W5-4，
	//    W5-S3；注册表只增）──
	// 消费点：E_ACME_WILDCARD_REQUIRES_PROVIDER / E_ACME_WILDCARD_REQUIRES_
	// BASE_DOMAIN = acme.* 设置保存的联动校验门（internal/state/
	// acmesettings.go ValidateAcmeSettings——422 语义违约 / 409 配置缺失，
	// 与 E_ALERTS_METRICS_REQUIRED、E_S3_PUBLIC_REQUIRES_BASE_DOMAIN 同构）；
	// E_ACME_DNS_TEST_FAILED = TestDnsProvider 探针失败（internal/api
	// ——失败步与底层错误摘要进信封 context，凭证材料零出现）。
	{ID: "E_ACME_WILDCARD_REQUIRES_PROVIDER", HTTP: 422,
		Summary:    "acme.wildcard=true requires a DNS provider (dnspod or cloudflare): a wildcard SAN can only be validated via a DNS-01 challenge",
		Suggestion: "Configure the DNS provider credentials first ('fleetly acme dns set --provider dnspod --token …' or the Console ACME card), then switch the wildcard on ('fleetly acme wildcard on')."},
	{ID: "E_ACME_WILDCARD_REQUIRES_BASE_DOMAIN", HTTP: 409,
		Summary:    "acme.wildcard=true requires a platform base domain (the wildcard domain set *.base with console/ctrl/registry.<base> derives from it)",
		Suggestion: "Configure base_domain (installer --base-domain) before switching the wildcard on; the wildcard certificate covers every platform subdomain and app domain under it."},
	{ID: "E_ACME_DNS_TEST_FAILED", HTTP: 503,
		Summary:    "the DNS provider probe failed (create -> delete a real TXT record at _acme-challenge-test.<base_domain>; the failed step and the underlying provider error travel in the envelope context)",
		Suggestion: "Fix the provider credentials per the failed probe step (create=auth/write incl. zone resolution, delete=cleanup) and test again; the honest contract is: test passed = the provider credentials can authenticate, write and delete TXT records under the platform domain."},

	// ── Web 终端面（E7 设计 §2.4/§2.5，W5-S6；注册表只增）──
	// 消费点：ExecService.CreateTerminalTicket 与 native WS 端点的功能开关
	// 门（config terminal.enabled=false——duty 移除 relay 服务、API 拒绝
	// 签发，Console 面板显示禁用态）。
	{ID: "E_TERMINAL_DISABLED", HTTP: 409,
		Summary:    "the web terminal feature is disabled (terminal.enabled=false): no exec relay is deployed and no terminal tickets are issued",
		Suggestion: "Enable the feature by setting terminal.enabled: true in the control plane config and restarting fleetlyd; the exec relay duty converges the fleetly-exec service on every node automatically."},

	// ── 程序化动态工作负载（T 线 DT-5 / IMPL-T2-1，注册表只增）：任务面的
	// 请求契约违约（scope 形态/内部变体支持矩阵/TTL 与资源上下限）走 400；
	// 每令牌配额 fail-closed 拒绝走 429（可重试语义：等非终态任务回收后再试
	// ——不是请求错误）。消费点 = internal/api/tasks.go 与 internal/engine
	// tasks.go（ResolveTaskScope 的 fail-closed 面）。
	{ID: "E_TASK_UNSUPPORTED", HTTP: 400,
		Summary:    "task request falls outside the dynamic-workload contract (scope kind/ref invalid, internal variant unsupported for the scope kind, scope network absent, ttl/resource bounds violated)",
		Suggestion: "Fix the named field: internal=true is only supported for task-group networks; tasks join networks that already exist (create the task-group network via EnsureTaskNetwork first); ttl_seconds must be within [60, 86400] and resources within the platform bounds."},
	{ID: "E_TASK_QUOTA_EXCEEDED", HTTP: 429,
		Summary:    "the token's task quota is exhausted (concurrent tasks or total CPU/memory of non-terminal tasks at the limit; fail-closed)",
		Suggestion: "Wait for running tasks to reach a terminal state (TTL reclaim or explicit stop) before creating more, or raise the per-token quota."},

	// ── 上传构建面（T 线 DT-6 / IMPL-T2-2，注册表只增）：client-streaming
	// 上下文 tar 上传的 fail-closed 拒绝族。消费点 = internal/api/builds.go
	// BuildFromUpload（流式累计超限 / 流协议与 tar 形态违约 / Dockerfile
	// 入口越界）。时长超限不落新码——构建执行超时沿用 E_BUILD_FAILED
	//（与 git 构建同面，不歧视）。
	{ID: "E_BUILD_UPLOAD_TOO_LARGE", HTTP: 413,
		Summary:    "the uploaded build context exceeds the platform size limit (tar bytes; enforced while streaming)",
		Suggestion: "Shrink the build context (exclude vendor/cache directories) or raise build.max_upload_mb in the control plane config."},
	{ID: "E_BUILD_UPLOAD_INVALID", HTTP: 400,
		Summary:    "the uploaded build context is invalid (stream protocol violation, malformed or unsafe tar, or an invalid Dockerfile entry)",
		Suggestion: "Send a single tar stream: the first message carries metadata (name, dockerfile), the rest are non-empty tar chunks; the Dockerfile entry must be a relative path inside the context."},

	// ── 认证/用户面（v0.3 W1，RBAC 设计 §2.1/§10；注册表只增）：注册窗口
	//    关闭的稳定拒绝码（无用户窗口恒开不落本码；users 非空后
	//    auth.registration 缺省 closed 管辖）。消费点：internal/api/
	//    authservice.go Register（state 哨兵 ErrRegistrationClosed 的
	//    apperr 化投影，403）。──
	{ID: "E_REGISTRATION_CLOSED", HTTP: 403,
		Summary:    "self-service registration is closed (auth.registration defaults to closed once the platform has any user; the zero-user window is always open)",
		Suggestion: "Ask a platform administrator to create the account via 'POST /v1/users' (or flip the switch with 'PUT /v1/auth/registration' open); the first user of a fresh install can always register."},

	// ── 团队/项目面（v0.3 W2-S1，RBAC 设计 §5 错误码清单，注册表只增）：
	//    消费点 = internal/api/teams.go 与 internal/api/authservice.go
	//    （state 哨兵的信封化投影）。E_DB_PROJECT_MISMATCH 同清单余项随
	//    W2-S4 跨项目守卫票登记（本票无消费点，不提前造码）。──
	// 消费点：RemoveTeamMember / SetTeamMemberRole 降级路径的最后一名 owner
	// 守卫（state 哨兵 ErrTeamLastOwner，409——操作与守卫同事务）。
	{ID: "E_TEAM_LAST_OWNER", HTTP: 409,
		Summary:    "last-team-owner guard: removing or demoting the only owner would leave the team without a responsible party",
		Suggestion: "Grant the owner role to another member first (POST /v1/teams/{team_id}/members:set-role), then retry; a team always keeps at least one owner."},
	// 消费点：AuthService.AcceptInvite 的四类不可消费形态（查无此 token /
	// 已接受 / 已吊销 / 已过期——state 哨兵 ErrInviteInvalid 统一同码，
	// 409；一次性凭据不泄漏具体状态）。
	{ID: "E_INVITE_INVALID", HTTP: 409,
		Summary:    "the invite token is invalid, already used, revoked, or expired (one-time credential; the specific state is not disclosed)",
		Suggestion: "Ask the team owner or admin for a fresh invite link; invite links are valid for 7 days and can be used exactly once."},
	// 消费点：CreateTeam 的保留字守卫（8 个平台组件保留字守前缀族——命名
	// 公式 v0.3 起以 team slug 为参数，撞上即在受理层拒绝；state 层注释
	// 「保留字校验在上层」的落点。422 与 E_LABEL_RESERVED 同级语义违约）。
	{ID: "E_TEAM_SLUG_RESERVED", HTTP: 422,
		Summary:    "the team slug collides with a platform-reserved component name (v0.3 naming formulas derive fleetly-<team>-* objects from the team slug; the reserved set guards the prefix families)",
		Suggestion: "Pick another slug outside the reserved set (listed in the error message); slugs are lowercase word-form identifiers [a-z0-9]{2,32} and immutable once created."},

	// ── 归属管道（v0.3 W2-S3，rbac-teams §4.2/§5 + D-W0-9 解析规则）──
	// 消费点：Deploy/CreateDatabase 的 project 解析（裸名解析域内命中多个
	// 同名项目 → 列候选；409 无——请求侧可修正）。
	{ID: "E_PROJECT_AMBIGUOUS", HTTP: 400,
		Summary:    "the bare project name matches more than one visible project (same-name projects across teams are legal, D-W0-9)",
		Suggestion: "Qualify the reference as team/project (e.g. acme/prod) and retry; the matching candidates are listed in the error context."},
	// 消费点：按裸名解析 app/库资源的多行命中面（读面限定形支持归 S4——
	// 创建面与命名面先行，state.ErrAppAmbiguous / ErrDatabaseAmbiguous 的
	// 信封化投影）。
	{ID: "E_APP_AMBIGUOUS", HTTP: 400,
		Summary:    "the resource name matches more than one row across projects (app/database names are unique per project, D-W0-4); bare-name references must be unique in the resolution scope",
		Suggestion: "Qualify the reference as team/project/<name> or reference the resource by its platform id; e2e/CLI fixtures should avoid same-named resources until the qualified read face lands (S4)."},
	// 消费点：Deploy/CreateDatabase 的归属一致性守卫——行上归属已定，重复
	// 请求指向其他项目即拒绝（校验一致而非静默沿用；指引 MoveApp）。
	{ID: "E_APP_PROJECT_MISMATCH", HTTP: 409,
		Summary:    "the request targets a different project than the one the app row already belongs to (ownership on the row wins once assigned)",
		Suggestion: "Deploy without the project field (it resolves to the row's own project) or move the app first: a platform administrator can reassign ownership with MoveApp (rename redeploy)."},
	// 消费点：git push 首发路径（internal/gitserver ensureAppRow）——首次
	// 建行的归属解析失败（push 无署名用户，或署名用户无缺省项目）：先经
	// CLI/API 携带 project 首发建行，再 push。
	{ID: "E_APP_PROJECT_REQUIRED", HTTP: 400,
		Summary:    "the pushed app does not exist yet and no project ownership can be derived from the push (no signed user or no default project)",
		Suggestion: "Deploy once via CLI/API passing project \"team/project\" to create the app with ownership, then push; subsequent pushes deploy to the row's own project."},
	// 消费点：部署受理的跨项目库引用守卫（v0.3 W2-S4，rbac-teams §4.1/
	// §4.2 E4——引用实例与 app 不同项目 → 拒绝入队；执行点 = 引擎 preparing
	// 期解析 fleetly.databases label 的单点，internal/engine/dbinject.go，
	// 覆盖 API Deploy / git push / webhook 全部入队路径）。
	{ID: "E_DB_PROJECT_MISMATCH", HTTP: 409,
		Summary:    "the referenced database instance belongs to a different project than the app (project isolation, R6/R7): cross-project database attachment is rejected at deploy admission",
		Suggestion: "Move the database into the app's project first (a platform administrator can reassign it with MoveDatabase), or reference an instance created in the same project; the app's project is listed in the error context."},

	// ── 警告码（W_：资源/计划上的标注，不作为 HTTP 错误返回，HTTP=0）──
	{ID: "W_DEPLOY_INSTABILITY",
		Summary:    "post-observe-window instability alert (one source of app degraded)",
		Suggestion: "Instability detected at runtime: check app logs to assess; the platform alerts once and advises manual rollback (no escalation counter)."},
	{ID: "W_DEPLOY_NO_HEALTHCHECK",
		Summary:    "service declares no healthcheck, health_gate=none (health gate skipped)",
		Suggestion: "Add a healthcheck to the service; without one the health gate is skipped and the deploy passes straight through."},
	{ID: "W_ROLLBACK_IMAGE_RISK",
		Summary:    "rollback target declared by tag; its content may have changed (not an immutable digest)",
		Suggestion: "The rollback target image is declared by tag and its content may have changed: confirm the image content before replaying."},
	{ID: "W_PLACEMENT_STATELESS_PIN",
		Summary:    "stateless app auto-pinned because a new volume was added (data birthplace)",
		Suggestion: "The stateless app was pinned because of the new volume: remove the volume declaration if free scheduling is needed."},
	{ID: "W_ENV_PLATFORM_OVERRIDE",
		Summary:    "platform-layer env_vars overrides same-named keys from files (three-layer merge chain)",
		Suggestion: "Platform-layer env overrides same-named compose keys: the merge result follows the platform layer; check the platform env_vars."},
}
