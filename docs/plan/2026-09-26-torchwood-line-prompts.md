# T 线分发 Prompt（逐票自包含，复制即用）

| 状态 | 日期 | 关联 |
|---|---|---|
| 待分发（与实施方案票号一一对应；T3 简票启动前再补） | 2026-09-26 | [实施方案](2026-09-26-torchwood-line-impl.md)、[设计档](../design/2026-09-26-torchwood-line.md) |

使用说明：每条 prompt 自包含，整段复制给实现 agent 即可。分发顺序与并行关系见实施方案 §1 依赖图。**所有 prompt 的第 0 步都是方案可行性审查——审查不通过即停，这是特性不是缺陷，阻塞会回到设计档裁决而不是带病实现。**

---

## PROMPT · IMPL-T1-1（Domains state 资源与协议面）

```
【仓库与背景】你在 D:/Codes/qiulin/fleetly 工作：fleetly 是 Go 自托管 PaaS（swarm 底座 + SQLite 单写点 + React Console）。本票属于 torchwood 线（让 fleetly 承接 torchwood/messageloop 生产栈）。先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 OT-2 节、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 纪律与 IMPL-T1-1 票全文（其目标/改动点/守卫与验收/必查项为本票需求真值）。

【第 0 步：方案可行性审查（强制，先于任何代码）】逐条核实票内「现状锚点」的 file:line 是否仍成立（重点：internal/engine/routes.go:29 的「Port=expose 首端口」、internal/state/domains.go 现行表结构、internal/compose/domains.go 的 label 解析与上限、internal/ingress/dynamic.go 的后端渲染）；逐项取证票内「必查项」；检查步骤与现状是否矛盾。通过 → 审查结论（含每条必查项取证结果）记入票尾「实施记录」，进入实现；发现矛盾/前提不成立 → 停止，输出审查报告（点名矛盾 + 修正建议）等人工裁决，不要猜测着改方案。实现中的偏离决定记入「实施记录」并说明理由。

【硬约束】AGENTS.md（Git Bash 正斜杠；导出标识符不用缩写）；用户可见文案英文、注释中文；错误码/事件只增；proto 破坏性变更先停手报告；Console 既有 data-testid 锚点不破坏（AppDomainsPage 相关测试尤其注意）；bash 过 sh -n、禁 2>/dev/null。

【本票要点】per-domain {host, service, port, protocol(http|h2c), cert_mode(http01|wildcard)} state 资源；API+Console 可写（现 AppDomainsPage 只读+verify）；ingress 按域名出后端端口与 scheme=h2c（不引入 ServersTransport 资源）；compose label 降级 bootstrap 种子 + 单一写点仲裁（state 存在则 label 忽略并派事件）；80→443 重定向维持不做；ACME HTTP-01 路径不回退；cert_mode 字段本票只落存储与校验（DNS-01 签发在 W5 另票）。

【完成标准】票内五条守卫与验收逐条落回归测试（测试名体现条款）；全量 go test ./... 绿；console 改动按 package.json 实际脚本跑 test/typecheck/lint/build。输出：变更文件清单（每文件一句说明）、测试清单与验收条款对应表、审查结论、偏离清单（或「无」）。不执行 git commit/push（用户验收后统一提交）。
```

## PROMPT · IMPL-T1-2（镜像代拉与凭证下发）

```
【仓库与背景】（同上）先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 DT-2 节、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 纪律与 IMPL-T1-2 票全文（需求真值）。

【第 0 步：方案可行性审查（强制）】核实现状锚点（internal/engine/engine.go:720-735 的 E_IMAGE_PULL_FAILED 与 :771-777 的 digest 钉定、internal/substrate/services.go:52-90 本机 inspect、internal/substrate/images.go:58-79）；**重点必查项：swarm service create/update 携带 EncodedRegistryAuth 的下发与留存语义（update 不重携是否丢 auth）——用 swarmkit API 文档/源码或最小实证回答，结论写入实施记录**；其余必查项逐项取证。通过 → 记录后实现；矛盾 → 停，输出审查报告等人工裁决。偏离决定记入实施记录。

【硬约束】（同 T1-1）镜像引用一律 digest 钉定（check-image-pins 门禁过）。

【本票要点】registry v2 resolver（tag→digest；ghcr 匿名 token flow + 平台凭证两种；404/限流退避）；platform settings 增 registry credentials（envelope 加密，沿 S3 设置先例，含 CLI 设置面）；engine 部署路径：本地 inspect 快路径（airgap 不回归）→ resolver 解析钉定 → service create/update 携 auth；Redeploy 对可变 tag 重解析；错误信息点名 image+原因。

【完成标准】票内五条守卫逐条落回归测试（resolver 单测用 mock registry；staging 真机项「ghcr 公共镜像零预拉部署」若本环境无 staging 访问权则在报告中标注待真机项，不虚构结果）；全量 go test ./... 绿。输出/禁 commit 等同 T1-1。
```

## PROMPT · IMPL-T1-3（部署期一次性作业 init job）

```
【仓库与背景】（同上）先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 DT-4 节、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 与 IMPL-T1-3 票全文。

【第 0 步：方案可行性审查（强制）】核实现状锚点（internal/compose/domains.go:118-164 的 cron label 解析先例、internal/cron/manager.go 的 one-shot job 机器、internal/compose/validate.go:47）；重点必查项：swarm one-shot job 约束的 label 公式（参考 docs/ 与既有 cron 实现的坑位注释）、与 release 相位机的挂钩点（必须走 EnterPhase 单写点，不绕开）、init job 的 env/secret/config 投影是否与 service 同源。通过 → 记录后实现；矛盾 → 停。偏离记入实施记录。

【硬约束】（同 T1-1）事件注册表只增（release.job_* 新码按既有评审格式登记）。

【本票要点】compose label `fleetly.job: init`（job 服务禁 expose、与 cron label 互斥）；发布管线在晋级前以新 spec 跑 one-shot job（从 cron 机器抽出 job runner 复用）；失败/超时 → 发布失败（timed_out 事件沿 cron 先例）；多 init job 并行全过才晋级；日志入 VL 并按 app/job 归因；naming 公式扩展进既有表驱动测试。

【完成标准】票内五条守卫逐条落回归测试（尤其「无 init job 的 release 路径零变化」回归）；全量 go test ./... 绿。输出/禁 commit 等同 T1-1。
```

## PROMPT · IMPL-T1-4（Config 资源）

```
【仓库与背景】（同上）先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 OT-3 节、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 与 IMPL-T1-4 票全文。

【第 0 步：方案可行性审查（强制）】核实现状锚点（internal/api/secrets.go、internal/compose/validate.go:256-263 与 :663-668、internal/metrics/spec.go:35-36 的 swarm config 内容寻址分发先例）；重点必查项：swarm config 引用更新的服务滚动语义（configs 列表整体替换先例）与旧 config 对象 GC 时机。通过 → 记录后实现；矛盾 → 停。偏离记入实施记录。

【硬约束】（同 T1-1）错误码只增；Console 新页面锚点命名沿用 AppSecretsPage 同族风格。

【本票要点】app 级 Config 资源（明文、版本化、可回读、审计）；API CRUD（Get 回读明文 admin scope；List 不带值；配额沿 secrets 口径：每 app 条数/大小上限）；compose 卷 `type: config`（target 绝对路径只读；禁撞 /run/secrets 前缀；面向少数运行时配置文件，文档明示目录级文件树应烘镜像）；engine 渲染 swarm config（fleetly-<app>-config-<name>-<sha8> 内容寻址，变更即新对象+服务滚动+旧对象回收）；Console AppConfigsPage（镜像 Secrets 页去加密）+ CLI。

【完成标准】票内五条守卫逐条落回归测试；全量 go test ./... 绿；console 按实际脚本跑。输出/禁 commit 等同 T1-1。
```

## PROMPT · IMPL-T15-1（Project 资源与项目网 + recon 扩面）

```
【仓库与背景】（同上）先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 OT-1 节（含与 Team 轴正交的声明）、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 与 IMPL-T15-1 票全文。

【第 0 步：方案可行性审查（强制）】核实现状锚点（internal/naming/naming.go 的 app 网/别名/用户自报 aliases 拒绝；substrateRecon duty 的 services-only 现状与 30s 频控）；重点必查项：**service update 增/摘网络的任务重启语义（swarm 会重建任务——实证并在 Console/runbook 诚实标注）**、别名 <app>-<service> 与 naming 契约的评审记录、staging UDP 未放行期的 placement 同节点指引落文档。通过 → 记录后实现；矛盾 → 停。偏离记入实施记录。

【硬约束】（同 T1-1）naming 契约是文档钉死+表驱动测试的合同——app 名保持全局唯一，本项目不改 naming 公式主体，只扩项目网别名；Console 既有 data-testid 锚点不破坏。

【本票要点】Project = 网络共享作用域资源（身份权限轴仍归 Team，两轴正交；app ∈ 恰一 project 可选，缺省不参加维持 app 私网现状）；proto projects CRUD + attach/detach；state projects 表 + app.project_id；平台建 fleetly-project-<id8> overlay；成员服务双挂（app 私网+项目网），项目网别名 = <app>-<service>（短名仅 app 私网）；project 非空禁删；detach 走服务滚动；**substrateRecon 对账面扩 networks（state 外 fleetly- 前缀网 → 派生修正+事件）**；Console 最小面（app 归属显示 + 项目列表）+ CLI。

【完成标准】票内五条守卫逐条落回归测试——尤其「孤儿网注入一个对账周期内暴露」（机制验收）与「短名跨 app 不混流」；全量 go test ./... 绿；console 按实际脚本跑。输出/禁 commit 等同 T1-1。
```

## PROMPT · IMPL-DB-0（dbtools 多 PG 大版本工具面，DB-1 前置）

```
【仓库与背景】你在 D:/Codes/qiulin/fleetly 工作：fleetly 是 Go 自托管 PaaS（swarm 底座 + SQLite 单写点 + React Console）。本票是 IMPL-DB-1 的前置票（2026-09-27 用户裁决，阻塞证据见 docs/plan/2026-09-26-torchwood-line-impl.md §4「IMPL-DB-1 方案可行性审查」）：dbtools 备份/恢复执行体仍是 PG 16.15 单一大版本，PG 18 实例 backup（pg_dump 拒对更高大版本服务器）与 restore（16 的 postgres 起 18 数据目录即 incompat）结构性不可达。先读：AGENTS.md、DB-1 审查记录（§4 全文）、docs/design/2026-09-26-torchwood-line.md 的 DT-9 节、docs/design/2026-09-20-managed-databases.md §2.6（执行体/恢复契约）、deploy/Dockerfile.dbtools、internal/database/adapters.go、.github/workflows/dbtools.yml、docs/runbooks/image-prepull.md（台账与 #19/#20 先例）。

【第 0 步：方案可行性审查（强制）】核实现状锚点（Dockerfile.dbtools 基底与工具面、adapters.go 的 DefaultDatabaseToolsImage 与五处 ID switch、restorePostgresJobScript 的 PGDATA 硬编码、dbtools.yml 发布链——gh 已登录 fleetlyrun/fleetly 且具 workflow scope，dispatch 可用）；**机制必查项（实证裁决并给证据）**：①单镜像双工具面（versioned prefix + 每 job PATH/LD_LIBRARY_PATH；官方 postgres 产物为编译期 prefix，share 目录冲突面须实证）vs ②双镜像（Dockerfile.dbtools 双 final target/新文件，同构 COPY，job 按实例 PG major 选镜像）——选可行且改动面最小者，写清否定项证据；③pg_dump/pg_restore/postgres 的跨版本语义实证（哪些工具可共版、哪些必须与数据目录同 major）；④恢复临时实例启动形态（gosu 降权、PGDATA、share 可达性）在所选机制逐项可满足；⑤check-image-pins 对多 FROM/双文件扫描行为（全 FROM 必须 digest 钉定）。通过 → 记录后实现；矛盾 → 停，输出审查报告等人工裁决。偏离记入实施记录。

【硬约束】（同 T1-1）镜像纪律：纯 COPY 无 RUN（新 target 同守；机制确需 RUN 则停并报告）；全 FROM digest 钉定 + deploy/check-image-pins.sh 过；错误码/事件只增；bash 过 sh -n、禁 2>/dev/null；**不 commit/push**；全量 go test ./... 绿。

【本票要点】dbtools 支持 PG16+PG18 工具面（pg_dump/pg_restore/psql/postgres/pg_ctl/gosu；mysql/mongo/redis/restic 面两镜像同构）；`dbtemplate.Template` 增身份字段（Engine/Distribution/Major——DB-1 审查冻结的最小设计），既有四模板 ID/Image/字段值逐字不变；render/adapters 分派轴 ID→Engine 迁移（行为零变化）；job 镜像按实例 PG major 选择（新常量 digest 钉定）；恢复 PGDATA 自模板条目/`RestoreInput.VolumeTarget` 参数化（PG16 现值不变）；dbtools.yml 扩双镜像构建 + cosign 签名；新 digest 台账登记（顺延 #22/#23）；文档（工具面版本纪律：与实例数据目录同 major）。

【完成标准】①PG18 真机探针：真实 PG18 实例（官方镜像容器/swarm 服务均可；不依赖 DB-1 词表，走 adapter 原语级 job 脚本或等价命令）跑通 dump→verify（pg_restore --list）→restore 重放全链，原始输出入记录；②PG16 全链零回归（全量 go test + 本地等价探针）；③镜像发布：CI 新 digest + cosign 签名（不可行则如实挂账不虚构）；④redis/mysql/mongo 零回归。输出/禁 commit 等同 T1-1；审查与实施记录写入实施方案 §4「IMPL-DB-0 方案可行性审查/实施记录」。
```

## PROMPT · IMPL-DB-1（PG 模板目录化）

```
【仓库与背景】（同上）先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 DT-9 节、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 与 IMPL-DB-1 票全文。

【第 0 步：方案可行性审查（强制）】核实现状锚点（internal/dbtemplate/dbtemplate.go:23-46 注册表与 E_DB_TEMPLATE_UNSUPPORTED、render.go、internal/database/adapters.go、E4 回归矩阵的现行组织方式）；重点必查项：percona-distribution-postgresql:18 与官方 postgres 镜像的 env/entrypoint 兼容性（PGDATA/init 语义，取镜像文档+本地 docker 实证其一）；两个新镜像引用的 digest 取得与 check-image-pins.sh 台账登记流程。通过 → 记录后实现；矛盾 → 停。偏离记入实施记录。

【硬约束】（同 T1-1）镜像 digest 钉定 + deploy/check-image-pins.sh 过（负路径验证）。

【本票要点】dbtemplate 注册表目录化重构（engine 通用 descriptor：distribution/major 字段，PG 首个消费者，redis/mysql/mongo 结构预留不实现）；词表增 postgres-18 与 percona-postgresql-18；CreateDatabase 校验走既有 E_DB_TEMPLATE_UNSUPPORTED 路径；大版本升级不做（文档明示升 major = dump/restore 新实例）；Console create-database-dialog 增模板选择器；备份/健康门适配器零新增（percona 线协议同 postgres）。

【完成标准】**每个目录条目过 create→backup→restore 回归矩阵（把「发行版不新增适配器」从断言钉成事实）**；全量 go test ./... 绿；console 按实际脚本跑。输出/禁 commit 等同 T1-1。
```

## PROMPT · IMPL-T2-0（Spikes ×4）

```
【仓库与背景】你在 D:/Codes/qiulin/fleetly 工作。本票是 T2 的四个前置 spike，产出物是报告（docs/reports/2026-09-26-t2-spikes.md），不是产品代码。先读：AGENTS.md、docs/plan/2026-09-26-torchwood-line-impl.md 的 IMPL-T2-0 票、docs/design/2026-09-26-torchwood-line.md 的 DT-5/DT-7 节。

【第 0 步】确认四个 spike 各自的执行环境：①④可在本机 daemon 或 staging（凭据由使用者提供，不进对话/日志）；②需 staging 真机（无访问权则标注待真机并完成可离线部分）；③需要 Docker 29 环境。任何 spike 环境不可得 → 报告里标注阻塞，不虚构数据。

【四个 spike】①Docker 29 internal overlay 出网实证（对照 bridge internal：NAT/DNS/跨节点矩阵；不过 → 宿主 nft 兜底方案写入报告）；②digest registry 直拉（tag→digest 解析 + swarm 逐节点拉取 + EncodedRegistryAuth update 语义实证）；③swarm service 生命周期时延基线（spawn/healthy/stop 计时 ×N 次取分布，对照「常驻池分钟级生命周期」的余量结论）；④多服务编排顺序语义观测（同 app 多服务发布，记录各服务实际启动时序与健康门行为，回答：顺序 or 并行+自愈窗口，对 depends_on 需求的诚实结论）。

【硬约束】bash 过 sh -n、禁 2>/dev/null、远程复杂操作=写脚本→scp→sh；操作 staging 前读相关 runbook；探针/脚本随报告入库（可复跑）。

【完成标准】报告含：每 spike 方法+原始数据+结论+对设计的影响（确认/修正建议）；结论不确定处如实标注。不 commit。
```

## PROMPT · IMPL-T2-1（Tasks API）

```
【仓库与背景】（同上）先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 DT-5 节、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 与 IMPL-T2-1 票全文、docs/reports/ 的 T2 spike 报告（IMPL-T2-0 产物，尤其 internal overlay 结论）。

【第 0 步：方案可行性审查（强制）】前置依赖核查：IMPL-T15-1 已验收（projects/网络作用域机器存在）、IMPL-T2-0① 结论支持 overlay internal 变体；核实机具令牌/scope 现行机制、janitor 与 recon 的扩面挂点、事件注册表登记格式。任何前置不成立 → 停，输出审查报告。偏离记入实施记录。

【硬约束】（同 T1-1）安全默认是本票灵魂：hardening 由服务端强制、不进用户表达面（与 compose 拒 cap_drop 同立场）；池语义（租约/保温/熔断）不进平台——API 面不出现任何 attach 入参（类型层不可表示）。

【本票要点】proto tasks.v1（CreateTask/GetTask/ListTasks/StopTask/DeleteTask + EnsureNetwork）；state（tasks 表 + 每令牌配额）；机具令牌新 scope tasks；engine 执行面：swarm service 承载（稳定 DNS 名）、restart: none 缺省、owner/TTL label、hardening 默认（CapDrop ALL/只读 rootfs/非 root/pids 限额）、网络 = scope 引用（app|project|task-group + internal 变体；task-group 网长活，幂等 EnsureNetwork，控制面服务每网一次性挂靠）；janitor 回收（TTL/到期/孤儿，recon 扩 tasks）；日志入 VL（task label 归因）；事件+审计（机具令牌署名沿既有 actor 机制）；CLI fleetly tasks run/ls/rm/logs；Console 后置 backlog 不做。

【完成标准】票内六条守卫逐条落回归测试（hardening spec 快照断言、配额 fail-closed、TTL 回收、跨 scope 越权拒绝、API 无 attach 入参、孤儿 task 对账暴露）；全量 go test ./... 绿。输出/禁 commit 等同 T1-1。
```

## PROMPT · IMPL-T2-2（build-from-upload API）

```
【仓库与背景】（同上）先读：AGENTS.md、docs/design/2026-09-26-torchwood-line.md 的 DT-6 节、docs/plan/2026-09-26-torchwood-line-impl.md 的 §0 与 IMPL-T2-2 票全文。

【第 0 步：方案可行性审查（强制）】核实 buildkitd 现行构建管线（git 源如何进构建、产物如何推 zot、digest 如何回填）与机具令牌 scope 机制；重点必查项：上传上下文的临时落盘位置与清理钩子、构建并发上限的现行控制点。通过 → 记录后实现；矛盾 → 停。偏离记入实施记录。

【硬约束】（同 T1-1）**信任级红线：上传 Dockerfile 与 git Dockerfile 同信任级，不加特赦也不加歧视（无新增构建参数面）**；平台侧推 zot，调用方永不需要 push 凭证。

【本票要点】proto 流式上传（上下文 tar，大小上限）+ BuildFromUpload（Dockerfile 入口可指定）；buildkitd 复用（与 git 构建同基料、单平台构建）；产物 digest 钉定返回；scope build（机具令牌）；大小/时长/并发配额；临时上下文零残留（清理钩子 + 测试）。

【完成标准】票内四条守卫逐条落回归测试（端到端：上传→构建→digest 可被 CreateTask/部署引用）；全量 go test ./... 绿。输出/禁 commit 等同 T1-1。
```

## PROMPT · IMPL-T1-6（messageloop SDK DialGRPC TLS——messageloop 仓库）

```
【仓库与背景】你在 D:/Codes/qiulin/messageloop 工作（Go 实时消息服务）。本票给 Go SDK 的 DialGRPC 补 TLS 支持（现 insecure 硬编码，见 docker/dokploy/README.md §4.2 自述）。先读：仓库 AGENTS.md/CLAUDE.md、sdk/ 目录结构与现有 Dial 实现、README 对应章节。

【第 0 步：方案可行性审查（强制）】核实 DialGRPC 的 insecure 硬编码位置与调用方（SDK 内部/示例/文档引用面）；确定 API 兼容策略（内测免兼容，但 insecure 本地路径必须保留——新增 options 还是改签名，给出推荐并说明理由）；确认 TLS 凭据形态（系统根 + 可选自定 CA/cert）。通过 → 记录后实现；矛盾 → 停。偏离记入实施记录。

【硬约束】仓库命名约定；用户可见文案英文；导出标识符不用缩写。

【完成标准】SDK 集成测试：TLS 域名握手 + 一次发布/订阅往返（staging 域名可用则真跑，否则 local TLS 端到端）；insecure 路径回归；README §4.2 更新（TLS 用法 + 隧道降级说明）。全量 go test ./... 绿。输出变更清单/测试清单/审查结论/偏离清单；不 commit（用户验收后统一提交）。
```

## PROMPT · IMPL-T1-5（messageloop 栈割接——messageloop 仓库 + staging）

```
【仓库与背景】你在 D:/Codes/qiulin/messageloop 工作。本票产出 fleetly 部署形态与割接 runbook（真机执行需使用者提供 fleetly staging 凭据与域名，凭据不进对话/日志）。前置：fleetly 侧 IMPL-T1-1~T1-4 已验收（域名资源/镜像代拉/init job/Config 资源可用）。先读：仓库 docker/dokploy/ 全套（现役形态真值）、fleetly 仓库 docs/design/2026-09-26-torchwood-line.md 的 DT-2/DT-8 节、fleetly compose 受控子集文档（internal/compose/validate.go 白名单为准）。

【第 0 步：方案可行性审查（强制）】对照受控子集白名单逐行核对现有 dokploy compose，列出每一处需要改写的点（ports/depends_on/external network/${VAR:?}/label 域名路由→API 声明）；确认镜像引用形态（digest 钉定 vs 可变 tag 的取舍写入 runbook）；确认 mlbridge.yaml → 平台 Config 的挂载路径改写点。产出改写清单后实现。

【产出物】①docker/fleetly/docker-compose.yml（受控子集：redis 保留 command --appendonly yes + named volume；删 ports/depends_on/external 网络/插值；MLBRIDGE_TORCHWOOD_BASE_URL 临时公网形态并注释说明 T2 后切项目内网）；②docker/fleetly/README.md（域名三声明：WS 9080 http / gRPC 9090 h2c / API 9091 h2c，经 fleetly 域名资源 API；env 清单；割接步骤）；③割接 runbook（含 DT-8 验收探针：BGSAVE→新实例回灌 or 明示清零 + 客户端 recover 探针；回滚 = dokploy 栈未拆）；④dokploy README 增「fleetly 部署」指引章。

【完成标准】compose 过 fleetly validate（对照白名单逐行自查并在实施记录附核对表）；runbook 步骤可复跑；真机割接段标注「待使用者执行窗口」。输出变更清单/审查结论/偏离清单；不 commit。
```

## PROMPT · IMPL-T2-3（dispatcher 改造——torchwood 仓库）

```
【仓库与背景】你在 D:/Codes/qiulin/torchwood 工作（Go BaaS）。本票把 dispatcher 的 docker.sock 交互面清零，改为 fleetly Tasks/build API 客户端。前置：fleetly 侧 IMPL-T2-1（Tasks API）、IMPL-T2-2（build-from-upload）、IMPL-T15-1（项目网）已验收——先向使用者确认，或读 fleetly 仓库 docs/plan/2026-09-26-torchwood-line-impl.md 核对各票状态。先读：本仓库 dispatcher/daemon.go（docker client 唯一入口）、pool.go（池语义——本票不动）、nodes.go/capacity.go（删除对象）、internal/infra/functions/docker.go、docker/dokploy/config.yaml 的 functions 段、fleetly 仓库的 Tasks API proto/CLI 文档。

【第 0 步：方案可行性审查（强制）】画出 daemon.go 的 docker client 调用面完整清单（每个方法 → fleetly Tasks/build API 的映射表）；识别池语义依赖的容器级细节（inspect IP、health 探针、stop 宽限）在 swarm service 语义下的等价物（服务 DNS 名、平台健康门）；确认删 nodes.go/capacity.go 后配额语义改为平台配额 + 本地 fail-closed 上抛。映射表有缺口 → 停，报告缺口等裁决。

【硬约束】仓库命名约定；用户可见文案英文；池语义（租约/保温/熔断/TW_MAX_REQUESTS）一行不动——只换执行底座；禁保留任何 docker client 引用（连兜底都不留）。

【本票要点】BuildImage → fleetly build-from-upload API（渲染上下文逻辑保留：.tw-runner.js + Dockerfile → tar）；SpawnInstance/回收 → CreateTask/Stop/Delete（常驻 task，restart none，崩溃由本进程熔断重建——现有 TimeoutBudget 语义保留）；网络 → EnsureNetwork（task-group，per-torchwood-租户项目长活）+ scope 引用（internal 变体承不可信函数）；cells/Redis 控制面/节点心跳删除（多节点编排归 swarm）；config functions 段改 fleetly endpoint + 机具令牌（scope: tasks,build；令牌由部署注入，不进代码/对话）。

【完成标准】①集成测试对 fleetly staging 端到端：spawn→health 握手→请求分发→TW_MAX_REQUESTS 自退→平台回收；②断言代码路径零 docker client（机制回归：事故类在结构上不可复现）；③配额触顶 fail-closed 上抛测试。全量 go test ./... 绿（dispatcher 包 + 集成）。输出变更清单/映射表/审查结论/偏离清单；不 commit。
```

## PROMPT · IMPL-T2-4（torchwood 栈割接——torchwood 仓库 + staging）

```
【仓库与背景】你在 D:/Codes/qiulin/torchwood 工作。本票产出 fleetly 部署形态与割接 runbook（真机执行需使用者提供 fleetly staging 凭据、域名与机具令牌，凭据不进对话/日志）。前置：IMPL-T2-3、IMPL-DB-1（托管 percona-postgresql-18 模板）、IMPL-T1-1（域名 h2c）、IMPL-T15-1（项目网）全部验收。先读：docker/dokploy/ 全套（现役形态）、fleetly compose 白名单（internal/compose/validate.go）、设计档 DT-8/DT-9/OT-2 节。

【第 0 步：方案可行性审查（强制）】逐行对照 dokploy compose 列改写清单（config.yaml×3 服务 → 平台 Config；migrate → fleetly.job: init；bootstrap-roles.sql → Config；migrations/initdb 目录 → 确认 GHCR 镜像内含后删除 bind；删 external 网络/ports/插值）；postgres 割接路径核对（dump/restore 进托管实例的步骤与配额）；确认函数网络的受控回访形态（dispatcher/server 挂函数 task-group 项目网，一次性挂靠）。缺口 → 停，报告。

【产出物】①docker/fleetly/docker-compose.yml（受控子集全栈：postgres 若落托管则从栈内删除并改连接串注入；redis/minio 留栈内 + named volume；worker/packer/dispatcher 常驻；migrate 标 fleetly.job: init）；②docker/fleetly/README.md（域名声明：9080 http + 9060 h2c；机具令牌 scope: tasks,build 的铸造与注入；Config 清单）；③割接 runbook（DT-8 口径：postgres dump/restore 步骤、redis/minio 卷迁移或明示重置、割接记录模板含数据决策栏；mlbridge 切项目内网别名；回滚 = dokploy 栈未拆）；④dokploy README 增 fleetly 章。

【完成标准】compose 过 fleetly validate（核对表附实施记录）；函数端到端验收段（部署 zip→执行→回收）写入 runbook；真机段标注待执行窗口。输出变更清单/审查结论/偏离清单；不 commit。
```

---

## PROMPT · IMPL-T2-5（torchwood 双执行底座——T-3 裁决承接，2026-09-28 验收轮新增）

```
【仓库与背景】你在 D:/Codes/qiulin/torchwood 工作（Go BaaS）。背景：T2-3（commit 78ea1a4）把 dispatcher 的 docker.sock 交互面清零、docker/dokploy/ 栈改成 fleetly 客户端形态（必填 ${TORCHWOOD_FUNCTIONS_FLEETLY_*:?} 插值），导致现役 dokploy 部署报错。用户直裁（T-3，2026-09-28）：torchwood 必须继续支持原有 dokploy / docker 直接部署，与 fleetly 形态并存。先读：仓库 AGENTS.md/CLAUDE.md、fleetly 仓 docs/design/2026-09-26-torchwood-line.md 的 T-3 裁决行、docs/plan/2026-09-26-torchwood-line-impl.md 的 IMPL-T2-5 票全文（需求真值）、dispatcher/daemon.go（现行 Daemon 接口与 fleetlyDaemon）、git 历史 54b666b:dispatcher/daemon.go（docker 底座完整旧实现）。

【第 0 步：方案可行性审查（强制，先于任何代码）】①对比 54b666b 的 docker daemon 与现行 Daemon 接口的方法签名差异（尤其 InspectInstance 的 ip 语义、EnsureProjectNetwork 的网络/成员挂靠语义在 docker 底座下的对应物——docker 底座是自建 bridge 网络自 attach，无平台发布管线）；②确认 proto Functions 段 reserved 字段号清单（docker.host/network 旧号不可复用，新字段用新号）；③确认 bootkit 校验分发点；④列出 54b666b 旧实现中依赖已删除多节点模型的代码面（nodes/capacity/routing 钩子）并裁决裁剪方案（单机 local 模式为必达，registry 模式复归与否记录偏离）。差异/矛盾 → 停，输出审查报告等人工裁决；通过 → 记录后实现，偏离记入实施记录。

【硬约束】仓库命名约定；用户可见文案英文、注释中文；池语义（租约/保温/熔断/TW_MAX_REQUESTS）一行不动——只加执行底座；fleetly 驱动路径不得引入 docker client 依赖（机制测试按此口径改写）；导出标识符不用缩写。

【本票要点】①functions.driver: docker|fleetly 显式枚举，未设/未知 fail-closed 报错列出两选项与各自配置键；②docker 底座复活+适配现行接口（含 per-project 网络 tw-func-<project>[-int] 与 server attach 回访——旧实现语义）；③proto 新字段（docker 后端 host/network）；④bootkit 按驱动分发校验（driver=docker 时不要求 fleetly 段，反之亦然）；⑤docker/dokploy/ 三件（compose/config.yaml/README）恢复自包含 docker.sock 形态：sock 挂载、user: root、driver: docker、零 FLEETLY_* 变量、多节点细胞模型维持退役（历史注记保留）；README 保留 fleetly 指引章；⑥docker/fleetly/config.yaml 钉 driver: fleetly；⑦「零 docker client」三层机制断言改口径为 fleetly 驱动路径（源扫描排除 docker 底座文件、依赖图按构建标签或包边界裁量——给出可执行的口径并记录）。

【完成标准】票内五条守卫逐条落测试；全量 go test ./... 绿（含既有池测试零变化）；本地 dind E2E 两形态各跑一遍（docker 底座 spawn→health→分发→回收；fleetly 底座复跑既有 E2E）；docker compose -f docker/dokploy/docker-compose.yml config 渲染零缺变量。输出：变更清单/审查结论/偏离清单/两形态 E2E 原始输出；不 commit（用户验收后统一提交）。
```

## PROMPT · IMPL-F1（task-group 网络对账豁免——fleetly 仓整改小票）

```
【仓库与背景】你在 D:/Codes/qiulin/fleetly 工作。T 线验收发现：T2-1 实施记录声称 task-group 网有归属锚不被孤儿判定命中，实际 reconNetworks（internal/engine/projectnetwork.go:111-136）只豁免 LabelProjectNetwork——长活 task-group 网会被周期误披露 network.orphaned（披露噪声+记录虚报；不删网，安全性质在）。先读：docs/plan/2026-09-26-torchwood-line-impl.md 的 IMPL-F1 票、internal/engine/projectnetwork.go 与 internal/engine/tasks.go 的网络面、IMPL-T2-1 实施记录的对应句。

【第 0 步】核实缺陷（读代码确认 task-group 网确实落入孤儿判定）；确认豁免的正确锚点（任务网 label 形态以 internal/naming 与 EnsureTaskNetwork 实际落的 label 为准）。

【改动】reconNetworks 期望集并入任务网（或等价豁免），补回归测试：ensure 过的 task-group 网零 orphaned 披露、state 外注入的真正孤儿网仍被披露（机制验收不回退）；IMPL-T2-1 实施记录句勘误标注。

【完成标准】新测试+全量 go test ./... 绿；输出变更清单/审查结论；不 commit。
```

## PROMPT · IMPL-F2（00023 迁移 Down 腿测试——fleetly 仓整改小票）

```
【仓库与背景】你在 D:/Codes/qiulin/fleetly 工作。T 线验收发现：IMPL-T1-1 实施记录守卫表引用 TestPlatformSettingsMigrationUpDown（internal/state/s3settings_test.go:218）声称覆盖 00023（domains protocol/cert_mode 列）Up/Down，实际只覆盖 00011——00023 Down 腿零测试执行。先读：docs/plan/2026-09-26-torchwood-line-impl.md 的 IMPL-F2 票、internal/state/migrations/ 的 00023 迁移文件、既有迁移测试形态。

【第 0 步】核实 00023 迁移的 Up/Down SQL 内容与表结构变化（domains 行增列），确认 Down 测试的正确断言面（回滚后列消失/旧数据保全）。

【改动】补 00023 Down 腿迁移测试（建库→Up 全量→写域名行→Down 到 22→断言列消失与数据行为；或扩展既有 DownTo 用例）；IMPL-T1-1 实施记录该行勘误标注。

【完成标准】新测试+全量 go test ./... 绿；输出变更清单；不 commit。
```

---

## 未分发（启动前细化再补 prompt）

- IMPL-T3-1/T3-2/T3-3（P2 简票：宿主回环端口 opt-in、托管 redis AOF 选项、通配证书沿 W5）。
