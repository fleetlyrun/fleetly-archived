# 评审遗留问题完整解决方案（S13-S20 波次任务源）

| 状态 | 日期 | 关联 |
|---|---|---|
| **已实施**（S13-S20 全部落地，2026-09-20；验证基线：`go test ./... ./sdk/go/...` 21 包全绿、`buf lint`、`gofmt -l` 干净、console vitest 36 用例 + `tsc --noEmit`、全部 deploy/e2e 脚本 `sh -n` + 无 BOM、三 workflow YAML 校验、F1 全部 13 个 action 钉 SHA） | 2026-09-20 | [架构评审报告](../reports/2026-09-19-architecture-review.md)（任务源 H 编号沿用）；[架构](2026-09-17-architecture.md)；[发布语义](2026-09-17-release-semantics.md)；[状态模型](2026-09-17-state-model.md) |

本文档给出评审确认、S1-S12 之外全部遗留问题的**完整解决方案**：每项含问题回顾、方案裁决（含被否备选）、改动面、机制验收（"下一个同类问题在哪一层被拦住"）。S1-S12 已修复项见评审报告 §5，不重复。

## 0. 波次总览

| 波次 | 主题 | 严重度 | 预估 | 状态 |
|---|---|---|---|---|
| S13 | H9 路由撤销合法通道 | 高 | 0.5 天 | ✅ 已实施（noop@internal 兜底 + WithdrawAppRoutes 接入 DeleteApp；TestPublishEmptyViewWithdrawsToFallback 等 8 测试） |
| S14 | H15 供应链双轨验签 | 高 | 0.5-1 天 | ✅ 已实施（RSA-2048 openssl 轨 + gate C + test-install A11 17 断言；生产密钥按 deploy/README 配置后生效） |
| S15 | 类 B 出站字节出口收口（6 项） | 中（系统性） | 1-1.5 天 | ✅ 已实施（B1-B6 全落；泄漏扫描 4 测试） |
| S16 | 类 C 契约可校验化（6 项） | 中（系统性） | 1 天 | ✅ 已实施（转移表下沉 state 并接线 UpdateDeployment；deadcode CI 门禁 + 豁免清单） |
| S17 | 类 D 超时与取消闭环（4 组） | 中（系统性） | 1-1.5 天 | ✅ 已实施（webhook 202/worker、substrate 30s、CLI 0/1/2/64、Console 五项） |
| S18 | API/状态层细节修正（11 项） | 中低 | 1 天 | ✅ 已实施（A1-A11 全落；非终态超龄扫描事件并入 janitor） |
| S19 | 入口/gitserver 细节修正（7 项） | 中低 | 0.5-1 天 | ✅ 已实施（E1-E7 全落；时间戳防线边界如实入注释） |
| S20 | 交付/CI 修正（7 项） | 中低 | 0.5 天 | ✅ 已实施（F1-F7 全落；13 action 钉 SHA、schema-version 子命令 + --auto-restore） |

依赖关系：S13/S14 独立可先行；S15 的泄漏扫描测试是 S19 个别项的验收依赖（先 S15 更顺）；其余无硬依赖，可按人力并行。

## 1. S13 — H9：路由撤销合法通道（空视图 + 删除管线接通）

**问题**：删除 app 最后一个域名后 `Validate` 拒绝空合成 → Traefik 永久保留旧路由（502 而非撤销）、台账与边缘分歧、sweep 每 12h 永久 warn；`state.DeleteAppDomains` 无生产调用方（app 删除后路由继续发布）。根因：Spike B 模型下（显式空 map 被拒、裸 `{}` 危险）"合法空态"无载体。

**方案裁决**：**常驻兜底 router（`noop@internal`）**。
1. `internal/ingress/dynamic.go` 的 `Synthesize`：路由集为空时合成单条兜底路由——`routers: {"fleetly-fallback": {rule: Host(\`fleetly.invalid\`), service: "noop@internal"}}`，services 键保持存在（Traefik v3 内置 noop 服务，无外部端点依赖、零流量命中）。`Validate` 规则不变（键存在 + 合成非空——兜底使空集恒过）。挑战路由合并（ACME 期间）与空视图共存时正常合并。
2. **删除管线接通**：app 删除（api/apps.go deleting 流程）在终态清理时调 `DeleteAppDomains` + 触发 ingress 重发布（装配层已有 ingressPublisher 通道；apps 服务需要拿到重发布入口——经既有 `RoutePublisher` 端口空载荷调用或 manager 幂等 `republishAll`）。`DeleteAppDomains` 从死代码转为删除路径一环。
3. `TestPublishEmptyViewKeepsPreviousConfig` 语义反转：空视图 = 兜底形态真实下发（改断言而非删测试）。
- **被否**：显式空 map（Traefik 拒绝、保留旧配置——Spike B 实测）；死端点 service（多一个假依赖，noop@internal 是 Traefik 官方为此场景提供的原语）；"拒绝清空"维持现状（撤销链路缺失，评审已定级高）。

**改动面**：internal/ingress/{dynamic,manager}.go、internal/api/apps.go（删除清理）、装配层接线、internal/ingress 测试。

**机制验收**：删除唯一域名/app 后，provider 视图断言为兜底形态（测试层拦截"撤销不生效"回归）；`deadcode` 门禁（S16）保证 DeleteAppDomains 不再无名腐烂。

## 2. S14 — H15：安装/升级链双轨验签

**问题**：cosign 缺席（干净 VPS 接近 100%）或 .sig 下载失败时 install.sh/upgrade.sh 只 warn 降级；checksums 强制但来自同一 origin，不防 release 侧投毒——"生产强（release.yml 完整 sigstore 链）、消费弱"。

**方案裁决**：**openssl 兼容轨 + 降级即死**。
1. release.yml：新增 repo secret `FLEETLY_RELEASE_KEY`（openssl 私钥）；产物步骤追加 `openssl dgst -sha256 -sign "$KEY" checksums.txt > checksums.txt.sig.pem`，与既有 cosign bundle 并行发布（双轨并存）。
2. install.sh / upgrade.sh：内嵌对应**公钥 PEM**（约 450 字节，附指纹注释与轮换说明：轮换 = 换脚本公钥 + 版本号，旧产物验签失败提示升级安装器）。验签顺序：有 cosign → cosign verify-blob（现逻辑不变）；无 cosign → `openssl dgst -sha256 -verify <公钥> -signature checksums.txt.sig.pem checksums.txt`（openssl 在目标发行版近乎必装）。
3. **降级语义收紧**：`checksums.txt.sig` 与 `.sig.pem` 任一存在但下载失败、或双轨全部不可验 → `die`（不再 warn）；仅当 release 明确未附签名（< 引入双轨的历史版本）时保留带显著警示的兼容路径。
- **被否**：vendored cosign（自举问题——验证器自身无根）；仅内嵌指纹（openssl 验签需完整公钥）；维持 warn（评审定级高的信任落差）。

**改动面**：.github/workflows/release.yml、deploy/install.sh、deploy/upgrade.sh、deploy/test-install.sh（断言双轨路径 + 伪造 checksums 必 die）。

**机制验收**：dind 安装测试注入篡改 checksums → 安装 die（test-install.sh 新断言）；无 cosign 容器内 openssl 轨端到端通过。

## 3. S15 — 类 B：出站字节出口收口

| # | 问题 | 方案裁决 | 改动面 | 机制验收 |
|---|---|---|---|---|
| B1 | 退化信封透传内部错误原文（apperr.go:210） | 无信封 detail 且 code∈{Unknown,Internal,FailedPrecondition} → 固定文案"internal error（详见服务端日志，request 侧凭 code/X-Request-ID 关联）"；原文仅进 slog | internal/apperr/apperr.go、cmd/fleetlyd/errors.go | 泄漏扫描测试 B6：构造 state 层错误直传 → REST/gRPC message 为固定文案 |
| B2 | git stderr 全文进响应/审计/日志（fetch.go:246、webhook.go:308-312） | 对外信封固定文案（E_RUNTIME_UNAVAILABLE 不带 `%v` 原文）；原文仅 slog（Warn 级、含路径）；审计 detail 只记错误码+阶段 | internal/gitserver/{fetch,webhook}.go | B6 扫描断言 message/审计不含 `git fetch`/路径模式 |
| B3 | build 日志绕过 redact（manager.go:215-227） | `buildLogEntries` 出口过该 app redactor（与容器日志同一管线）；redact 值集扩面：webhook secret、拉源 https_token、hook token（≥8 字节规则同 env 值） | internal/logs/manager.go、redact.go、internal/gitserver（值集供给） | 单测：构建日志含 secret 值 → 输出脱敏 |
| B4 | 审计 DiffSummary 手工拼 JSON（6 处） | `state.DiffSummary(kvs ...any)` 构造器（内部 json.Marshal），替换 env/builds/appgit/gitkeys/webhook/deploy 全部手拼点 | internal/state + 调用点 | 单测：键值含 `"`/`\` 的输入产出合法 JSON |
| B5 | bootstrap token 明文进 INFO 日志/journald | 改为写 `<数据根>/bootstrap-token` 文件（0600，fsync），日志只报路径 + 首次成功登录提示删除；install 报告同步指引 | cmd/fleetlyd/bootstrap_token.go、deploy/install.sh | 测试：日志全文 grep 不到 `flt_` 前缀 |
| B6 | （机制）泄漏扫描负路径 | 一条遍历全部 proto 错误路径的测试：断言 message 不含绝对路径、`stderr`、`git fetch`、SQL 片段、`flt_` 前缀等内部模式 | internal/api 或 apitest | 该测试即验收本体，CI 层拦截 |

## 4. S16 — 类 C：契约可校验化

| # | 问题 | 方案裁决 | 改动面 | 机制验收 |
|---|---|---|---|---|
| C1 | secrets 白名单放行但规划层必然拒绝、文档示例照抄即失败 | v0.1 裁决**校验层显式拒绝**：compose 含 `secrets` → `E_COMPOSE_UNSUPPORTED`（reason: v0.1 平台密钥库未接入，v0.2 开放）——比"放行到 preparing 晚期才炸 + 错误码误导"诚实；架构文档 §2.4 示例删除 secrets 用法并加注 | internal/compose/validate.go、docs/design/2026-09-17-architecture.md | 校验测试：secrets 在 Load 期即拒 + 错误码正确 |
| C2 | 用户 label 静默丢弃 | 非 `fleetly.*` 服务 label 产出 W 级校验警告（"平台不透传用户 label"），随 plan/deploy warnings 带出 | internal/compose/{validate,load}.go | golden 更新 + 警告断言 |
| C3 | 文档清单与校验白名单无人 diff | 白名单键集落 golden（`internal/compose/testdata/whitelist.golden`，经 `go test -update` 机制）；测试断言 golden ↔ validate.go 集合一致；架构 §2.4 加"以 golden 为准"注 | internal/compose + golden | 白名单增删忘改文档/golden → 测试红 |
| C4 | env pending 两套相反契约（引擎行为正确、state/envlayer 注释与测试钉死错误侧） | 以引擎现行为准（pending 参与合并、成功提升=架构文档语义）：修订 state/env.go、envlayer.go 注释与 envlayer_test 描述；`EffectiveAppEnv` 删除（deadcode 首批） | internal/state/env.go、internal/envlayer | 注释/测试语义与架构文档一致；deadcode 门禁守 |
| C5 | machine.go 转移表不在写路径（纯说明书） | `state.UpdateDeployment` 的 CAS 分支前校验 `CanTransition(from,to)`，非法转移拒写（引擎唯一写点纪律不变） | internal/state/deployments.go、internal/engine/machine.go | 单测：observing→queued 类非法转移被 store 拒 |
| C6 | 死代码无门禁（SetAppServicePorts/Queue.Enqueue/EffectiveAppEnv/EmptyConfig；DeleteAppDomains 由 S13 接活） | CI（pr.yml）加 `go run golang.org/x/tools/cmd/deadcode@<钉版> ./...` + 豁免清单文件（SDK 消费面等，`// deadcode: exempt` 约定或清单文件）；首批清理上列死代码 | .github/workflows/pr.yml、internal/{state,build,ingress} | 新增无人调用导出符号 → deadcode job 红 |

## 5. S17 — 类 D：超时与取消闭环

| # | 问题 | 方案裁决 | 改动面 | 机制验收 |
|---|---|---|---|---|
| D1 | webhook 同步拉源绑 `r.Context()`（GitHub 10s 投递超时杀 fetch，大仓库永久失败循环） | **最小异步化**：验签+去重+占坑后即回 `202 {status:"accepted"}`；入带界内存队列（chan 32，满则 503+warn）；后台 worker（挂 GitTriggers 服务生命周期，随 lynx Start/Stop）执行 fetch+DeployFromCommit，per-item `context.WithTimeout(Background, 30min)`；失败 → 事件 `app.webhook_fetch_failed` + 审计 + Unmark（支持手动 redeliver 幂等重试）。结果披露走事件流/UI（官方不会重投 202） | internal/gitserver/webhook.go、装配 | 测试：慢 fetch（fake 30s）不阻塞响应；worker 失败产生事件+Unmark |
| D2 | substrate 全部 Docker API 调用无 per-call 超时 | client 内统一 helper：非流式调用（Info/ServiceInspect/ServiceUpdate/ServiceCreate/NetworkEnsure/ImageLoad/ImageInspect…）包 `WithTimeout(30s)`（config 可调）；流式（ContainerLogs/Events/Ping）保持调用方 ctx | internal/substrate | 挂起注入测试（fake 永不返回、1s 预算）断言限时失败 |
| D3 | CLI 无信号处理、无 RPC 超时、用法错误与 plan/diff 共用退出码 2、Unavailable 无引导 | ① `signal.NotifyContext(os.Interrupt, SIGTERM)`，`context.Canceled` 对流式动词判干净退出（exit 0）；② `withClient` 非流式动词缺省 30s deadline（deploy/build 的等待语义保持自有 `--timeout`）；③ 用法类错误退出码改 **64（EX_USAGE）**，`2` 专属 plan/diff"检测到变化"——README 契约同步 `0/1/2/64`；④ renderCLIError 加 Unavailable 分支（"fleetlyd 不可达——检查 --addr 与守护进程状态"） | cmd/fleetly/{main,app,conn,render}.go、README | app_test 退出码断言更新（usage→64）；Ctrl+C 流式 exit 0 测试 |
| D4 | Console：流式 401 不接全局登出、types 手写漂移、重连无退避、无 CSP、无请求超时 | ① stream.ts consume 对 401 复用 `clearToken()+unauthorizedListener`（页面仅展示）；② types 生成化：`openapi-typescript` 从 `genproto/*.swagger.json` 生成 `src/api/schema.d.ts`，手写 types 仅保留视图模型，CI 加"再生成无 diff"门禁；③ 重连指数退避 1.5s→30s 上限+抖动，服务端立即正常关流连续 N 次后降频提示；④ fleetlyd 静态托管响应加 `Content-Security-Policy: default-src 'self'; connect-src 'self'`（console_static.go）；⑤ api() 加 `AbortSignal.any([init.signal, AbortSignal.timeout(30_000)])` | console/src/api/*、cmd/fleetlyd/console_static.go、pr.yml | 401 登出测试；schema.d.ts diff 门禁；CSP 响应头断言（gateway_console_test） |

## 6. S18 — API/状态层细节修正

| # | 问题 | 方案 | 验收 |
|---|---|---|---|
| A1 | Deploy/TriggerBuild 忽略 `req.GetApp()`（REST 路径与实际资源静默错位） | `spec.Name != req.GetApp()` → 400 `E_COMPOSE_UNSUPPORTED`（reason: compose 应用名与请求目标不一致）；proto 注释同步"两处必须一致" | api 测试：不一致即拒、不误建 app |
| A2 | 每请求同步写 `last_used_at`（SQLite 写放大） | 进程内节流：map[tokenID]lastTouch，间隔 <60s 跳过写 | 高频调用下 UPDATE 次数断言（fake store 计数） |
| A3 | 匿名请求无限流（DB 查询风暴面） | REST gateway 面（gRPC 面拿不到对端 IP，维持 401 熵防线）按 RemoteAddr host 小容量失败桶（10/min，超限 429）——挂在根 handler 鉴权失败路径 | apitest：匿名 11 连击第 11 次 429 |
| A4 | ListApps limit=0 无截断 + derivedState N+1 | limit=0 套 100 缺省（proto 注释本就如此）；derivedState 批量化（placements/最新部署一次 IN 查询） | 1000 app 语义测试（fake）+ 查询计数断言 |
| A5 | WatchEvents 无 per-token 并发流上限 | 每 token 5 条常驻流上限，超限 `ResourceExhausted` | api 测试 |
| A6 | revision 固化与终态 CAS 两事务（重启重复版本行） | CreateRevision 并入终态 CAS 同一 InTx；`(app_id, desired_hash)` 幂等（存在即复用，seq 不递增） | crashpoint 类测试：两事务间崩溃重放不产生重复行 |
| A7 | 部署 compose 只存 OS 临时目录（tmpfiles 清理/容器形态丢失） | 入队时 compose 字节持久化 `<数据根>/deployments/<id>/compose.yaml`（DeployRecord 存路径），终态后由 janitor 按 30 天窗清理；temp 仅解析中转 | 重启后 preparing 重载成功（测试：删 temp 后引擎仍可推进） |
| A8 | 漂移投影遗漏 update 配置（order/parallelism/delay/failure_action 篡改不告警） | substrate `serviceToState` 补抄 `Spec.UpdateConfig` 三字段；driftSpec 纳入；`failure_action != pause` 专报事件（受管字段被篡改） | 漂移测试：外部改 update-order → 漂移项+事件 |
| A9 | tick 无 panic 隔离（单条毒记录 crash-loop 控制面） | `advanceActive` 每条部署包 `defer recover()` → `failTransition(E_RUNTIME_UNAVAILABLE, "内部错误已捕获")` + Error 日志含栈 | 注入 panic 的 fake → 单条失败、tick 存活 |
| A10 | janitor 删除单语句无分批 + artifacts/builds 台账无界 | `DELETE ... LIMIT 500` 循环；janitor 扩保留窗：build artifacts 目录 30 天、builds 终态行 90 天（config 可配） | janitor 测试：大批量分批删；目录清理断言 |
| A11 | TriggerBuild 直写行不走 Queue.Enqueue（唤醒通道闲置 2s） | BuildsService 持 Queue 经 Enqueue 入队（Wake 生效）；Queue.Enqueue 从死代码转正 | 触发后认领延迟 <poll 间隔断言 |

## 7. S19 — 入口/gitserver 细节修正

> 2026-09-29 注记：git push(SSH) 收包面移除（ADR-0012）——E6 整行与 E7③
> 随 SSH 收包/钩子 token 面退役；E7①②④⑤（webhook 签名/方法门/拉源认证/
> https 强制）属保留面，继续有效。

| # | 问题 | 方案 | 验收 |
|---|---|---|---|
| E1 | acme.go `m.user` 无锁读写竞态 + 并发双重签发（LE 限额） | `m.user` 读写统一走 `m.mu`；`ensureCertificate` per-app 互斥（map[app]mutex 或单签发队列串行——v0.1 单签发队列即可） | -race 并发发布测试 |
| E2 | `markServed` 记录 revision 可能高于实际服务（挑战收敛门虚假通过） | `snapshot()` 返回 `(cfg, rev)`；`markServed(rev)` 记录该值 | 并发 addChallenge+snapshot 竞态测试 |
| E3 | `renewDue` appID 解析失败空串下传（签了不记） | 解析失败跳过该 app + warn | 单测 |
| E4 | `VerifyDomains` 硬编码 80/443 与可配端口脱节 | 端口经 Manager 配置传入 | 非默认端口 verify 测试 |
| E5 | certs.Save 非原子且注释承诺的回读校验不存在 | tmp+rename 三元组 + 写后回读 sha256 比对（补齐注释承诺） | 损坏注入测试 |
| E6 | SSH git 子进程无超时、断开不回收 | per-connection `WithCancel`（channel 关闭即 cancel）+ `cmd.WaitDelay=30s` + 总超时 10min | 客户端中途断开 → 进程限时回收测试 |
| E7 | X-Fleetly-Timestamp 不参与签名可省略；webhook 不校验方法；hook token TOCTOU；`SetAppSource` auth_secret 无最小长度；http:// 明文拉源 | ① 自定义投递方强制携带时间戳且 HMAC 覆盖 `ts+"."+body`（GitHub/Gitea 官方无此头的现实写入文档：时间窗防线不存在，靠 delivery TTL+sha 去重）；② webhook handler 非 POST → 405；③ `EnsureBareRepo` per-app 互斥（token 轮换 CAS 化）；④ `auth_secret` 非 none 时 ≥16 字符（与 webhook secret 同标）；⑤ https_token 认证强制 https:// scheme | 各一单测；TOCTOU 用并发 goroutine 断言钩子 token 始终有效 |

## 8. S20 — 交付/CI 修正

| # | 问题 | 方案 | 验收 |
|---|---|---|---|
| F1 | 敏感 workflow 的 actions 全 tag 引用（tag 劫持进 id-token: write 链） | release.yml/nightly.yml 全部 action 钉 commit SHA（注释保留版本号）；govulncheck 钉版本（与 go-licenses 同待遇） | CI 评审清单（无 @vN 引用残留） |
| F2 | nightly 失败取证截断且不上传 | 失败路径 `actions/upload-artifact@v4`（钉 SHA）上传 `$TMP`（日志+artifacts）；`tail -60` 双重截断去一层 | 人审 + YAML 校验 |
| F3 | Dockerfile CMD 指向不存在配置（开箱 crash-loop） | CMD 去 `-c`（回落内置默认）；README 容器形态示例补挂配置卷说明 | 容器冒烟（dind） |
| F4 | config-example 键漂移（缺 logs.* 整节、engine.drift_interval_seconds；S8/S12 新键需核对） | 补齐 + 加一条测试：example 键集 ⊆ AppConfig mapstructure 键集（防继续漂移） | 该测试即验收 |
| F5 | 升级回退不感知 schema（迁移已应用时回退必然 DEGRADED） | 回退前读 DB `schema_version`（goose 表直查）对比旧二进制支持上限（fleetlyd 暴露 `fleetlyd version --schema` 或安装报告记录）；高于旧件 → 提示并支持 `--auto-restore` 从 pre_upgrade 快照恢复状态库再拉起（快照已 verified，材料齐备） | test-upgrade.sh 增场景：新版本应用迁移后验证失败 → auto-restore 回退成功 |
| F6 | 换件 mv 序列无原子性兜底 | ⑤ 步包 ERR trap 子函数：任一 mv 失败立即执行回退；回退前强制确认旧进程已退出（stop 失败即 die，不 continue） | 注入 mv 失败 → 回退路径走通（shell 测试） |
| F7 | Traefik 创建日志 provider_endpoint 字段错位（args[3] 是 pollInterval） | 直接引用 endpoint 变量 | 人审 |

## 9. 明确不做（本轮裁决后挂账到 v0.2 或接受现状）

- **EnterPhase 全量单写点**（类 A 演进项）：S9 锚点列已落、S18-A6/S8 复位已闭合已知漏洞；全量重构触全部转换调用点，收益是防未来误用——由非终态超龄扫描兜底风险，列为 v0.2 前置重构。
- **非终态超龄扫描**（类 A 运行时断言层）：并入 S18-A10 janitor 扩展实现（deployments/builds 非终态且超 2×最大预算 → 事件+组件红），不单独立项。
- **证书/ACME 私钥 age 封装**：v0.1 维持明文 0600 + 文档明示边界（控制面主机文件系统即信任边界）；v0.2 多节点证书同步时再评估。
- **CLI TLS 通道 / SDK insecure**：v0.1 回环默认可辩护；S17-D3 的 Unavailable/非回环告警先落，TLS 配置面列 v0.2（与执行中继同批）。
- **naming `-` 歧义碰撞**：Swarm 服务名创建时天然报冲突；planner 提前友好报错列为低优先（v0.2 多 app 规模化时一并处理）。
- **手拼 JSON 其余低频点 / 各 <低> 级文案类项**：随所属文件被触碰时顺手收敛（S15-B4 构造器已覆盖审计摘要主路径）。

## 10. 实施与回归纪律

- 每项修复携带本文档对应机制验收（测试/门禁/运行时断言），禁止"改完即关闭"。
- 全局回归门：`go build ./...`、`go test ./...`（含 sdk 模块）、`buf lint`、`gofmt -l`、console `vitest + tsc`；涉及 proto 的走 `buf generate` + breaking 门禁；涉及 deploy/ 的走 S6 已落的 deploy-scripts 门禁。
- dind 全链（journey/upgrade/nightly V1-V7）由 CI nightly 轨道回归，本地方案不重复跑。
- 完成后按评审报告 §5 惯例回填状态（S13-S20 → ✅ + 测试名），本文档状态改"已实施"。
