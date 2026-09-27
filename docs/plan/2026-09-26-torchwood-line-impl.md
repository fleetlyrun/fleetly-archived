# T 线实施方案（票据表）——torchwood/messageloop 迁移与动态工作负载

| 状态 | 日期 | 关联 |
|---|---|---|
| **待实现（分发式：每票由实现 agent 按 prompt 执行，第 0 步强制方案可行性审查；用户逐票验收后统一提交）** | 2026-09-26 | [设计档](../design/2026-09-26-torchwood-line.md)（DT/OT 裁决）；[分发 prompt](2026-09-26-torchwood-line-prompts.md) |

## 0. 纪律（每票生效）

1. **第 0 步 = 方案可行性审查，先于任何代码**：核实票内「现状锚点」file:line 是否仍成立、逐项取证「必查项」；发现矛盾/前提不成立 → 停止并输出审查报告等人工裁决，不猜测着改方案。审查结论与实现中的偏离决定记入票尾「实施记录」。
2. 守卫与验收条款逐条落回归测试（测试名体现条款）；全量 `go test ./...` 绿；console 改动另跑 vitest/typecheck/lint/build。
3. 硬约束：AGENTS.md（Git Bash 正斜杠、导出标识符不缩写）；用户可见文案英文、注释中文；镜像引用 digest 钉定（check-image-pins 门禁）；错误码/事件只增；proto 破坏性变更先停手；Console 既有 data-testid 锚点不破坏；bash 过 `sh -n`、禁 `2>/dev/null`。
4. 完成不 commit/push——输出变更清单 + 测试清单 + 审查结论 + 偏离清单，等用户验收。

## 1. 依赖图与波次

```
IMPL-T1-1(域名资源) ─┐
IMPL-T1-2(镜像代拉) ─┼─→ IMPL-T1-5(messageloop 割接)          [T1 出口]
IMPL-T1-3(init job) ─┤      └──────────────→ (mlbridge 暂走公网)
IMPL-T1-4(Config)   ─┘
IMPL-T15-1(项目网+recon扩面) ─┐
IMPL-T2-0(spike×4) ──────────┼─→ IMPL-T2-1(Tasks API) ─┐
IMPL-DB-1(PG目录化,可先行) ──┼─→ (T2 割接前置)          ├─→ IMPL-T2-3(dispatcher改造)
IMPL-T2-2(build-upload) ─────┘                         ─┴─→ IMPL-T2-4(torchwood割接) [T2 出口]
IMPL-T1-6(SDK TLS,独立)   IMPL-T3-*(P2,后置)
```

| 波次 | 票 | 合计预估 |
|---|---|---|
| T1 | T1-1 / T1-2 / T1-3 / T1-4（可并行）→ T1-5、T1-6 | 10-18d |
| T1.5（可并行 T1） | T15-1 | 3-5d |
| 独立先行 | DB-1 | 2-3d |
| T2 | T2-0 → T2-1、T2-2 → T2-3 → T2-4 | 16-26d |
| T3（P2） | T3-1 / T3-2 / T3-3（简票，启动前再细化） | 3-5d |

## 2. 票据

### IMPL-T1-1（OT-2）Domains state 资源与协议面

- **目标**：per-domain `{host, service, port, protocol(http|h2c), cert_mode(http01|wildcard)}` state 资源；API + Console 可写；Traefik 后端按域名出 `scheme=h2c`；label 降级 bootstrap + 单一写点仲裁；80→443 重定向维持不做。
- **现状锚点**（先核实）：`internal/engine/routes.go:29`（Port=expose 首端口）、`routes.go:169-191`（firstExposePort）；`internal/state/domains.go:22-24`；`internal/compose/domains.go`（label 解析、≤5/服务 ≤10/app）；`internal/ingress/dynamic.go:285-300`（http 后端）、`:238-241`（不重定向）；`internal/ingress/manager.go:349-367`；Console `AppDomainsPage.tsx`（只读+verify）。
- **改动点**：proto 域名 CRUD（新字段向后兼容）；state schema 迁移；engine routes 改消费 state 行；ingress 渲染 h2c；compose label→bootstrap 种子；Console 页可写（host/port/protocol/cert_mode，复用原子组件）；CLI 同步。
- **守卫与验收**：①同服务两域名不同端口不同协议 → dynamic config 各自正确（回归钉）；②state 存在时 label 忽略 + 事件（仲裁验收）；③protocol=h2c → Traefik service `scheme=h2c`（配置快照断言）；④host 冲突/超限 → 4xx 点名；既有 5/10 上限不回退；⑤ACME HTTP-01 签发路径 staging 真机复验。
- **必查项**：现行 Domains proto/RPC 与 state 表真实形态；console verify 读哪张表（与写路径一致性）；域名删除的证书清理语义；label→seed 触发时点。
- **依赖**：无。**预估** 3-5d。

### IMPL-T1-2（DT-2）镜像代拉与凭证下发

- **目标**：部署时 tag→digest 经 registry API 解析钉定；swarm 逐节点按 digest 拉取；平台级 registry credentials（envelope 加密）；Redeploy 重解析可变 tag；airgap 本地镜像快路径不回归。
- **现状锚点**：`internal/engine/engine.go:720-735`（E_IMAGE_PULL_FAILED）、`:771-777`（digest 钉定）；`internal/substrate/services.go:52-90`（本机 inspect）；`internal/substrate/images.go:58-79`（ImagePull 仅 buildkitd 路径）。
- **改动点**：registry resolver（registry v2 token flow：ghcr 匿名 + 凭证两种）；platform settings 增 registry credentials；engine 部署路径改 resolved digest + auth 传递；错误语义（解析失败 → 点名 image+原因）。
- **守卫与验收**：①resolver 单测（mock registry：匿名/凭证/404/限流退避）；②staging 真机：ghcr 公共镜像零预拉部署成功；③digest 进 revision spec 且 drift 对账可见；④本地已有镜像走 inspect 快路径（airgap 回归）；⑤私有镜像 + 凭证逐节点拉取成功（依赖必查项结论）。
- **必查项**：**swarm service create/update 的 EncodedRegistryAuth 下发语义（update 不重携是否丢 auth——SwarmKit 行为实证）**；resolver 限流/重试；平台 zot 自建引用路径零回归。
- **依赖**：无。**预估** 2-4d。

### IMPL-T1-3（DT-4）部署期一次性作业（init job）

- **目标**：compose label `fleetly.job: init` 声明 init 服务；发布管线晋级前以新 spec 跑 one-shot job，失败即发布失败；超时看门狗；日志入 VL。
- **现状锚点**：`internal/compose/domains.go:118-164`（cron label 解析先例）；`internal/cron/manager.go`（one-shot job 机器）；`internal/compose/validate.go:47`（depends_on 拒绝注释——顺序由发布管线管的口径出处）。
- **改动点**：compose 解析+校验（job 服务禁 expose/与 cron label 互斥）；job runner 从 cron 机器抽出复用；release 管线挂钩（EnterPhase 单写点纪律内）；事件注册表增 release.job_*；naming 公式扩展。
- **守卫与验收**：①job 失败 → release 失败（回归）；②job 超时 → timed_out 事件 + release 失败；③多 init job 并行全过才晋级；④job 容器零残留（janitor 路径）；⑤无 init job 的 release 路径行为零变化（回归）。
- **必查项**：swarm one-shot job 约束的 label 公式（Docker29 坑记忆）；与 release 相位机挂钩点（不得绕开 EnterPhase）；job 的 env/secret/config 投影与 service 同源。
- **依赖**：无。**预估** 2-3d。

### IMPL-T1-4（OT-3）Config 资源

- **目标**：app 级明文配置资源（版本化、可回读、审计），compose `type: config` 任意只读挂载路径；面向少数运行时配置文件，**不映射目录级文件树**。
- **现状锚点**：`internal/api/secrets.go`（secrets 形态）；`internal/compose/validate.go:256-263`（secret target 固定 /run/secrets）、`:663-668`（volume type 白名单）；`internal/metrics/spec.go:35-36`（swarm config 内容寻址分发先例）。
- **改动点**：state（app_configs 表）；API CRUD（Get 回读明文，admin scope；配额沿 secrets 口径）；compose volume 校验（type: config，target 绝对路径 ro，禁撞 /run/secrets 前缀）；engine 渲染 swarm config（`fleetly-<app>-config-<name>-<sha8>` 内容寻址，服务 update 整体换引用）；Console AppConfigsPage（镜像 Secrets 页去加密）；CLI。
- **守卫与验收**：①未知 config source 解析期拒绝；②内容变更 → 新 config 对象 + 服务滚动 + 旧对象回收；③配额超限 4xx；④target 撞 /run/secrets 拒绝；⑤Get/List 权限矩阵与 secrets 同构（List 不出值？Config 明文可回读但 List 仍不带值，回读走 Get）。
- **必查项**：swarm config 引用更新的服务滚动语义（configs 列表整体替换先例）；旧 config 对象 GC 时机。
- **依赖**：无。**预估** 2-3d。

### IMPL-T1-5 messageloop 割接（messageloop 仓库 + staging 运维）

- **目标**：messageloop 栈（redis+messageloop+mlbridge）落 fleetly staging；dokploy 栈并行保留至验收。
- **内容**：messageloop 仓库新增 `docker/fleetly/`（受控子集 compose：删 ports/depends_on/external network/插值；`command` 保留 `--appendonly yes`；mlbridge.yaml → 平台 Config；MESSAGELOOP_* 全走平台 env）；三域名经 OT-2 API 声明（WS 9080 http / gRPC 9090 h2c / API 9091 h2c）；`MLBRIDGE_TORCHWOOD_BASE_URL` 临时公网形态；割接 runbook（BGSAVE 回灌或明示清零 + recover 探针，DT-8 验收）；docs/dokploy README 增 fleetly 章。
- **守卫与验收**：compose 过 fleetly validate 零告警；割接探针通过；WebSocket 域名真机连通；回滚 = dokploy 栈未拆。
- **依赖**：T1-1~4 全部。**预估** 1-2d（+真机窗口）。

### IMPL-T1-6 SDK DialGRPC TLS（messageloop 仓库）

- **目标**：`sdk.DialGRPC` 支持 TLS 凭据（现 insecure 硬编码，README §4.2 自认欠账）；insecure 本地路径保留。
- **守卫**：SDK 集成测试对 staging TLS 域名（h2c）握手 + 一次往返成功；本地 insecure 路径回归。
- **依赖**：T1-1（h2c 域名就绪后可验）。**预估** 1d。

### IMPL-T15-1（OT-1）Project 资源与项目网 + recon 扩面

- **目标**：Project = 网络共享作用域（身份轴仍归 Team，两轴正交，app ∈ 恰一 project 可选）；per-project overlay；成员服务双挂 + 项目网别名 `<app>-<service>`；substrateRecon 扩 networks。
- **现状锚点**：`internal/naming/naming.go`（`fleetly-<app>-net`、别名=服务名仅 app 网、用户自报 aliases 拒绝）；recon 现状（substrateRecon duty 30s，services only——T0-V2.2）。
- **改动点**：proto（projects CRUD + attach/detach）；state（projects 表 + app.project_id）；swarm overlay 生命周期（平台建 `fleetly-project-<id8>`）；服务双挂投影（engine/naming 扩展）；删除语义（project 空 app 才可删；detach 滚动）；recon 对 networks 的对账（孤儿网 → 派生修正+事件）；Console 最小面（app 归属显示 + 项目列表卡）；CLI。
- **守卫与验收**：①跨 app DNS 回归（exec 内 `ping <app>-<service>` 通）；②**孤儿网注入（state 外 `fleetly-` 前缀网）一个对账周期内暴露**（机制验收条款）；③project 删除非空拒绝；④短名跨 app 不混流（别名隔离断言）；⑤naming 契约：app 名保持全局唯一不变（表驱动测试不破）。
- **必查项**：**service update 增/摘网络的任务重启语义**（swarm 会重建任务——Console 与 runbook 诚实标注，或评估滚动窗口）；别名与 naming 契约评审记录；staging UDP 未放行期跨节点项目网的 placement 同节点指引。
- **依赖**：无（可并行 T1）。**预估** 3-5d。

### IMPL-DB-1（DT-9）PG 模板目录化（独立可先行）

- **目标**：dbtemplate 目录化；词表增 `postgres-18`（vanilla）与 `percona-postgresql-18`（含 pgvector）；发行版零新增适配器；大版本升级不做（创建钉死）。
- **现状锚点**：`internal/dbtemplate/dbtemplate.go:23-46`（注册表与 `E_DB_TEMPLATE_UNSUPPORTED`）、`internal/dbtemplate/render.go`、`internal/database/adapters.go`、E4 35/35 真机矩阵。
- **改动点**：注册表目录化重构（engine 通用 descriptor）；两新模板镜像 digest 钉定 + check-image-pins 台账登记；CreateDatabase 校验走既有路径；Console create-database-dialog 增模板选择器；文档（大版本升级 = dump/restore 新实例）。
- **守卫与验收**：**每个目录条目过 create→backup→restore 回归矩阵**（把「发行版不新增适配器」从断言钉成事实）；percona 镜像与官方镜像 env/entrypoint 兼容性实证（PGDATA/init 语义）；未知模板 4xx 既有错误码不破。
- **依赖**：无。**预估** 2-3d。

### IMPL-T2-0 Spikes（报告落 docs/reports/）

1. **Docker 29 internal overlay 出网实证**（DT-7）：对照 bridge internal 语义矩阵（NAT/DNS/跨节点）；不过 → 宿主 nft 兜底方案写进报告。
2. **digest registry 直拉真机**（T1-2 必查项的真机腿）：含 EncodedRegistryAuth update 语义。
3. **swarm service 生命周期时延基线**：spawn/healthy/stop 计时，对照 dispatcher 现役容器直操；常驻池模型余量结论。
4. **多服务编排顺序语义观测**：同 app 多服务发布时各服务启动时序与健康门行为，对照 depends_on 需求，诚实结论（顺序 or 并行+自愈窗口）。
- **预估** 1-2d。T2-1 前置 ①；T2-4 前置 ④。

### IMPL-T2-1（DT-5）Tasks API

- **目标**：程序化动态工作负载面——swarm service 承载、`restart: none`、owner/TTL label + janitor 回收、平台默认加固、机具令牌 scope + 配额、日志入 VL、三作用域网络（app|project|task-group + internal 变体）；MVP 不做 exec；池语义不进平台。
- **改动点**：proto `tasks.v1`（CreateTask/GetTask/ListTasks/StopTask/DeleteTask + EnsureNetwork）；state（tasks 表、配额表）；机具令牌新 scope `tasks`；engine 执行面（hardening 默认：CapDrop ALL/只读 rootfs/非 root/pids 限额——服务端强制；网络 = scope 引用，**API 面不存在 attach 入参**，task-group 网长活 + 控制面每网一次性挂靠）；janitor（TTL/到期/孤儿，recon 扩 tasks）；事件 + 审计；CLI `fleetly tasks run/ls/rm/logs`；Console 后置 backlog。
- **守卫与验收**：①hardening 默认落 service spec（spec 快照回归断言）；②配额超限（并发/资源）fail-closed 拒绝；③TTL 到期 janitor 回收 + 事件；④跨 scope 越权（他令牌的 task）拒绝；⑤API 面无 attach 入参（类型层不可表示——机制验收）；⑥networks 对账含 task（孤儿 task 暴露）。
- **依赖**：T15-1（作用域机器）、T2-0①。**预估** 5-8d。

### IMPL-T2-2（DT-6）build-from-upload API

- **目标**：通用「上下文 tar + Dockerfile 入口 → buildkitd → zot，返回 digest 引用」；机具令牌 scope `build`；大小/时长配额；与 git 构建同信任级（无特赦）。
- **改动点**：proto（流式上传 + BuildFromUpload）；buildkitd 管线复用（单平台构建）；zot push（平台侧，调用方无需 push 凭证）；配额与并发上限；临时上下文落盘清理。
- **守卫与验收**：①超限（大小/时长）拒绝；②产物 digest 返回且可被 CreateTask/部署引用（端到端回归）；③并发构建上限生效；④上下文临时文件零残留。
- **依赖**：zot（W2 已有）。**预估** 3-5d。

### IMPL-T2-3 dispatcher 改造（torchwood 仓库）

- **目标**：docker.sock 交互面清零——BuildImage→build API；SpawnInstance→CreateTask；回收→Stop/Delete；网络→EnsureNetwork + scope 引用；容量键/节点心跳/细胞模型删除；池语义（租约/保温/熔断/TW_MAX_REQUESTS）保留。
- **锚点**：`torchwood/dispatcher/daemon.go`（docker client 唯一入口）、`pool.go`（池语义，不动）、`nodes.go`/`capacity.go`（删除对象）；config `functions.docker.host` 段改 fleetly endpoint + 机具令牌（scope: tasks,build）。
- **守卫与验收**：①集成测试对 staging fleetly 端到端：spawn→health 握手→请求分发→TW_MAX_REQUESTS 自退→平台回收；②无任何手动 docker 网络操作（事故类回归——断言代码路径零 docker client）；③配额触顶时 dispatcher 语义（fail-open 改 fail-closed 上抛）。
- **依赖**：T2-1、T2-2、T15-1。**预估** 5-8d。

### IMPL-T2-4 torchwood 栈割接（torchwood 仓库 + staging 运维）

- **目标**：七常驻 + init job 栈落 fleetly；postgres 落托管 percona-postgresql-18；dokploy 栈并行保留至验收。
- **内容**：受控子集 compose（config.yaml → 平台 Config ×3 服务；migrate → `fleetly.job: init`；bootstrap SQL → Config；删 dokploy-network/ports/插值）；域名 OT-2 声明（9080 http + 9060 h2c）；postgres dump/restore 进托管实例；redis/minio 卷迁移或明示重置（DT-8 口径 + 割接记录）；mlbridge 切项目内网别名；dispatcher 挂 server 项目网（受控回访）。
- **守卫与验收**：全栈健康门过；函数端到端（部署 zip→执行→回收）；割接记录含数据决策；回滚 = dokploy 栈未拆。
- **依赖**：T2-3、DB-1、T2-0④。**预估** 2-3d（+真机窗口）。

### T3（P2 简票，启动前细化）

- **T3-1** 宿主回环端口 opt-in：label `fleetly.ports`（仅回环绑定，平台端口登记防冲突，UDP 供 QUIC/KCP）。
- **T3-2** 托管 redis AOF 选项：目录参数变体（同 DB-1 机器）。
- **T3-3** 通配证书沿 v0.3 W5 排期，不在本线重复立票。

## 3. 分发与验收流程

1. **分发**：按 §1 依赖图取票，prompt 见 [分发 prompt 文件](2026-09-26-torchwood-line-prompts.md)（每票一条，自包含，内置第 0 步可行性审查）。
2. **验收（用户）**：每票完成后核对——①变更文件清单与方案偏离清单；②测试清单与验收条款对应关系；③全量测试绿证据；④staging 真机项（T1-1⑤/T1-2②⑤/T1-5/T2-*）；⑤实施记录。验收通过后统一 commit（沿用仓库还原点惯例，一票一还原点）。
3. **阻塞上报**：实现 agent 第 0 步审查发现矛盾即停，方案修正走设计档裁决（回本档改票），不带病实现。

## 4. 实施记录

### IMPL-T1-1 方案可行性审查（2026-09-26，实现会话）

**结论：通过（无阻塞前置矛盾），进入实现。**

现状锚点核实（票内 file:line 逐条）：
- `internal/engine/routes.go:29`「Port 是后端端口（compose expose 首端口）」✓（现行 28-30 行；`firstExposePort` 现于 186-196，票内 169-191 为撰写时行号，语义未变）。
- `internal/state/domains.go` 现行表结构：`domains(id, app_id, service, domain UNIQUE, created_at)`（00001）+ `port/cert_sha256/cert_not_after/cert_updated_at`（00006，加法迁移）✓。
- `internal/compose/domains.go` label 解析与上限：`parseDomainsLabel`（≤5/服务，`maxDomainsPerService`）+ `checkDomainContracts`（≤10/app，`maxDomainsPerApp`，跨服务冲突 E_DOMAIN_CONFLICT）✓。
- `internal/ingress/dynamic.go:285-300`（http 后端合成）✓（现行 282-289）；`:238-241`（不重定向）✓；`internal/ingress/manager.go:349-367`（证书保障段）✓（现行 348-357）。
- Console `AppDomainsPage.tsx` 只读 + verify ✓。

必查项取证：
1. **Domains proto/RPC 与 state 表真实形态**：proto 只读（ListAppDomains GET、VerifyAppDomains POST），`DomainView{service, domain, port, cert_sha256, cert_not_after, created_at}`；写路径唯一 = `ingress.ReplaceAppDomains`（发布时按 engine 提取的 compose 声明对账，spec 不可读时走台账兜底）。
2. **console verify 读哪张表**：`VerifyAppDomains`（internal/api/read.go:280）经 `st.ListAppDomains` 读同一 `domains` 表，与写路径一致 ✓。
3. **域名删除的证书清理语义**：删行不触碰证书库；`ensureCertificate` 以「域名集变化即重签」判据（`needsRenewal`）收敛——被删域名的 SAN 会保留到下一次重签（续期窗口或下次域名集变化）。API 删除路径须显式触发重发布 + 证书保障，与发布路径同链。
4. **label→seed 触发时点**：`PublishRoutes` 由引擎在「首健康后」与「终态」两个挂点调用（sweep 只做全量重发布，不携带声明输入）。种子语义落位：state 无行且声明可用（spec_hash 匹配 + 有 expose 首端口）时播种；state 有行时 label 忽略 + 事件。

补充前提（本票实现依据）：
- 路由键 = Swarm 服务名（`fleetly.RouterName` 公式），access-log 经候选集反解（internal/logs/access.go）；多后端分组键需扩展后缀并使反解剥后缀（单后端服务公式零变化）。
- `internal/apitest` 以 `NewDomainsService(st, nil)` 注册（mgr 可空）——API 写面须容忍无 ingress 装配（跳过收敛）。
- 事件注册表在 `internal/eventcode`（注册 + golden 快照），新事件 `route.label_ignored` 按只增纪律登记。
- proto 生成：`mise exec -- buf generate`（remote 插件可用）；console 类型经 `pnpm gen:api` 从 genproto swagger 再生成。
- W5 已实现平台证书 DNS-01（`internal/ingress/dns01.go`），但 **app 级证书恒 HTTP-01**（W5 设计原文）；wildcard 主机（`*.`）本票仍拒绝（签发链未就绪，纳入 W5 app 级 DNS-01）；`cert_mode` 本票只落存储与校验，不改变现行签发路径。

### IMPL-T1-1 实施记录（2026-09-26，实现会话）

**状态：实现完成，待用户验收（未 commit）。**

变更文件清单（每文件一句）：
- `proto/fleetly/server/v1/domains.proto`：DomainsService 增 Create/Update/Remove 三 RPC；DomainView 增 `protocol/cert_mode`（新字段向后兼容）；Get/List 读面不变。
- `genproto/fleetly/server/v1/domains.{pb,pb.gw,swagger.json,grpc.pb}`：buf generate 生成物（swagger 供 console 类型再生成）。
- `internal/state/migrations/00023_domains_endpoint_columns.sql`：domains 增 `protocol/cert_mode` 两列（加法迁移 + Down 演练；既有行取默认 = 现行行为）。
- `internal/state/domains.go`：Domain 增两列；新增 `CreateAppDomain/UpdateAppDomain/RemoveAppDomain/GetAppDomain`（配额 ≤5/服务、≤10/app fail-closed；host 全局独占 `ErrDomainConflict`；`DomainLimitError` 带 scope/count）；`ReplaceAppDomains` 整组对账原语随 label 降级整体删除（死代码清理 + 语义退役），新增 `SeedAppDomainsIfEmpty`（单事务「检空才播种」原子仲裁——并发 API 写行不被覆盖/删除）。
- `internal/state/domains_test.go` / `internal/state/testdata/migrations.golden`：CRUD/默认值/配额/冲突回归 + 迁移 golden 再生成。
- `internal/compose/domains.go`：新增导出 `NormalizeDomain`（API 与 label 解析共用同一形态契约：trim/小写/IDN→punycode；通配主机拒）。
- `internal/compose/spec.go`、`internal/compose/validate.go`、`internal/state/labels.go`：注释口径更新（label = 首部署种子，state 行为真值）。
- `internal/engine/routes.go`：`RoutePublishInput.Services` → `Declared`（只承载 label 种子候选）；删除台账兜底直推（真值读取移到 ingress 发布点现读）。
- `internal/engine/routes_test.go`：种子提取回归（文件丢失无种子 / hash 匹配给候选）。
- `internal/ingress/manager.go`：`PublishInput.Declared`；`PublishRoutes` 单一写点仲裁（state 有行 → label 忽略 + `route.label_ignored` 事件；无行 → 播种）；新增 `ConvergeAppDomains`（API 写面收敛入口）；`routesFromLedger` 多后端分组（键 = app×service×port×protocol）。
- `internal/ingress/dynamic.go`：`Route` 增 `Protocol/KeySuffix`；合成按协议出后端 scheme（h2c:// 直出）、分组第 2..n 组键附 `~<port>[~h2c]` 后缀（后端 DNS 恒为 RouterName 本体）。
- `internal/ingress/manager_test.go` / `acme_test.go` / `s3public_test.go`：守卫①②③回归、省略=删除退役、API 收敛、ACME 代码链取证。
- `internal/logs/access.go`：access-log 路由键反解剥离分组后缀（候选集仍按 app×service 构造）。
- `internal/runtime/provides.go`：engine→ingress 适配器字段映射。
- `internal/api/read.go`：DomainsService 写面三方法与视图新字段、审计（domain.created/updated/removed）、写后收敛（best-effort 披露：失败落审计 error + `route.publish_failed` 事件，资源行保留）。
- `internal/api/scope.go`：三写 RPC 登记 deploy（读面 read 不变）。
- `internal/api/domains_test.go`：scope 登记、CRUD/归一化/默认值、守卫④、更新局部语义、删除与审计回归。
- `cmd/fleetly/cmd/domains.go`：`list` 增 protocol/cert_mode 投影；新增 `add/set/rm` 子命令（set 空 flag = 保持现值）。
- `cmd/fleetly/cmd/domains_test.go` / `cmd/fleetly/cmd/testdata/golden/domains_list.golden`：CLI 全链回归与 golden 再生成。
- `internal/eventcode/{events.go,eventcode_test.go,testdata/events.golden}`：`route.label_ignored` 登记（只增纪律）。
- `console/src/pages/AppDomainsPage.tsx`（+`.test.tsx`）：只读页升级为可写（新增/编辑/删除对话框；写控件走 deploy 门；平台管理员资源面只读说明行；既有 data-testid 锚点保留）。
- `console/src/api/{endpoints.ts,types.ts,schema.d.ts}`：三个写 RPC 封装、词表类型与 swagger 再生成。

测试清单与验收条款对应表：
| 条款 | 回归测试 |
|---|---|
| ① 同服务两域名不同端口不同协议各自正确 | `TestPublishMultiBackendServicePerDomainRouting` |
| ② state 存在时 label 忽略 + 事件（仲裁） | `TestPublishIgnoresLabelsWhenStateRowsExist`；种子里程 `TestPublishSeedsFromLabelsWhenStateEmpty`；「省略=删除」退役 `TestPublishEmptyDeclarationKeepsStateRows`；播种原子仲裁 `TestSeedAppDomainsIfEmptyArbitration`（并发 API 写行不被覆盖） |
| ③ protocol=h2c → service scheme=h2c | `TestPublishMultiBackendServicePerDomainRouting`（h2c:// 快照断言） |
| ④ host 冲突/超限 4xx 点名；5/10 上限不回退 | `TestDomainsServiceValidationAndLimits`（API 层）+ `TestCreateAppDomainConflictAndLimits`（state 层） |
| ⑤ ACME HTTP-01 路径不回退 | `TestConvergeAppDomainsIssuesHTTP01CertificateForAPICreatedDomain`（代码链）；**staging 真机复验待执行窗口** |
| API 写面完整性 | `TestDomainsServiceScopeRegistration` / `TestDomainsServiceCreateNormalizesAndDefaults` / `TestDomainsServiceUpdateKeepsOmittedFieldsAndRemove` |
| CLI 同步 | `TestDomainsCRUDSurface` + `domains_list` golden |
| Console 写面与角色门 | `AppDomainsPage.test.tsx`（4 测：viewer 无写控件 / admin 新增 / developer 编辑删除 / 平台管理员只读说明） |
| access-log 反解不回归 | `TestStripAccessRouterKey`（分组后缀用例） |
| 迁移纪律 | `TestPlatformSettingsMigrationUpDown`（00023 Up/Down）+ additive golden |

验证证据（一手）：`go test ./... -count=1` 全绿；变更包 `-race` 全绿（state/ingress/api/logs/engine/compose/eventcode/cmd）；`go vet ./...` 净；`golangci-lint run` 无新增问题（存量欠账不计）；console `pnpm test` 321 全绿（基线 313 + 新 4 测 + 既有计数）、`pnpm typecheck`/`pnpm lint`/`pnpm build` 净。

偏离清单（实现中的决策，均按票面「实施记录」纪律登记）：
1. **对账写入语义修正（含孤儿代码清理）**：「声明集对账（省略=删除）」随 label 降级整体退役——`ReplaceAppDomains` 原语删除，首部署播种改走 `SeedAppDomainsIfEmpty`（单事务检空才播种，消除「检空↔插入」竞态下并发 API 写行被对账删除的窗口）；API 删除是唯一撤销路径（与 OT-2 单一写点仲裁一致，`route.label_ignored` 事件披露）。退役语义的旧回归用例（`TestReplaceAppDomains*`）同步删除，夹具迁移到 `CreateAppDomain`/`SeedAppDomainsIfEmpty`。
2. **state 真值的读取位置**：票面「engine routes 改消费 state 行」落实为「发布链路在 ingress 发布点现读 state 行」；engine 只提取 label 种子候选。理由：消除「engine 读快照 ↔ 写点」竞态，且 domains 表的写与读消费点统一在 ingress。
3. **多后端分组键**：第 2..n 组路由键附 `~<port>[~h2c]` 后缀（'~' 不在服务名字符集内，结构防撞键；单后端服务公式零变化）；后端 DNS 名恒为 RouterName 本体；access-log 反解同步剥离后缀。
4. **API 收敛语义**：写面成功即返回资源行；入口收敛 best-effort（DNS 未就绪期的 ACME 失败不把资源写入报成错误；失败落审计 error + `route.publish_failed` 事件，下次部署/续期扫描恢复）。
5. **scope 解释**：写面取 deploy（与 SetEnv/SetScalingPolicy 同级的应用运行面写语义，非平台凭据面；票面「admin/deploy scope」按此落位）。
6. **golden 再生成**：事件注册表/迁移哈希/CLI 列表三处 golden 按各仓纪律显式再生成（非静默漂移）。

待真机项（本环境无 staging 访问权，未虚构结果）：
- ⑤ ACME HTTP-01 staging 真机复验（建议：加域名 `fleetly domains add` → 观察 80/443 签发与 Verify）；
- Traefik 对 `~` 后缀路由键的实机接受性复核（HTTP provider 为 JSON map 键，paerser 无字符集限制；staging 部署时一并确认）。

### IMPL-T1-2 方案可行性审查（2026-09-26，实现会话）

**结论：通过（无阻塞前置矛盾），进入实现。**

现状锚点核实（票内 file:line 逐条）：
- `internal/engine/engine.go:720-735`（E_IMAGE_PULL_FAILED 映射）、`:771-777`（`pinDigest`）✓ 现行 720-734 / 771-777。
- `internal/substrate/services.go:52-90`（本机 inspect、RepoDigests 取清单摘要；**平台 registry 腿**：zot 引用走 manifest HEAD）✓。
- `internal/substrate/images.go:58-79`（`EnsureImagePresent` 仅 buildkitd 容器路径调用，部署路径不拉取）✓。
- 附加现状（票面未列但关键）：`internal/substrate/registry.go` 已有 X-Registry-Auth 编码与 `registryAuthForImage`（仅平台 zot 引用命中），`ServiceCreate/ServiceUpdate` 已携 `EncodedRegistryAuth`（E1-5 多节点先例）。

**必查项取证（重点：swarm service create/update 的 EncodedRegistryAuth 下发与留存语义）**：
- 源码（moby master `daemon/cluster/services.go`，与本地 Docker 29.7.2 同线）：
  - Create：`if encodedAuth != "" { ctnr.PullOptions = &swarmapi.ContainerSpec_PullOptions{RegistryAuth: encodedAuth} }`——凭据写入 **service spec 的 `ContainerSpec.PullOptions.RegistryAuth`**（swarmkit `api/specs.proto` field 64），随 spec 持久化于 swarm raft，agent 拉取时消费。
  - Update：携带 auth → 覆盖 PullOptions；**不携带（空）→ 从 current spec（缺省，`RegistryAuthFrom=spec`）或 previous-spec 复制既有 PullOptions，不丢**（源码注释原文："this is needed because if the encodedAuth isn't being updated then we shouldn't lose it, and continue to use the one that was already present"）。moby client v0.6 `ServiceUpdateOptions` 提供 `RegistryAuthFrom`，与 29.x 服务端语义配套。
  - 推论（实现纪律）：a) 我们的 update 路径即使不重携也不会丢；本票实现仍按「凭证命中的镜像每次 create/update 恒携」下发（与 CLI `--with-registry-auth` 等价）；b) **空 auth 无法清场**——私有切公共镜像时旧凭据 blob 留在 spec（无 API 清场路径），运维文档诚实记录；c) 历史 issue moby#33929（17.06 "flag is lost"）是 CLI 侧 digest 解析丢失，现行为已由 PullOptions 持久化 + RegistryAuthFrom 收敛。
- 最小实证（本地 Docker 29.7.2，swarm active）：① create 携 auth → `docker service inspect` 的 **Docker API 类型不含 PullOptions（转换即丢）**——凭据不经 inspect API 泄露（披露面收敛）；② 宿主 swarm 数据目录为 `snap-v3-encrypted` / `wal-v3-encrypted`（raft 静态加密），明文 grep 不可得——密文态存储确认。逐节点拉取带 auth 的真机腿归 T2-0② spike（本票不重复）。

其余前提取证：
- moby client v0.6 `ServiceCreate/Update` 的 `QueryRegistry` 缺省 false：客户端**不**自行做 digest 解析——tag→digest 解析必须是平台自己的 resolver（本票）。
- 设置先例：`platform_settings` KV（无迁移）+ `internal/secrets.Box` envelope 密文（restic/rustfs 密码先例）；API 面 admin scope + `requirePlatformWriteFace`（S3/ACME 同款）；CLI `fleetly s3 ...` 同族。
- 代拉语义冻结（DT-2 两句话的落地次序）：**tag 引用 registry-first**（保证「Redeploy 重解析可变 tag = pull_policy: always 等价物」）；registry 不可达/404/401 时**回落本机 inspect**（airgap 不回归，失败原因入日志）；本机与 registry 皆不可得 → `E_IMAGE_PULL_FAILED` 点名 image+原因；digest 钉定引用直通（免解析，swarm 逐节点按 digest 拉取）。

设计冻结（实现者按此执行）：
1. 新包 `internal/imageregistry`：ref 解析（registry host 归一：显式 host / docker.io→registry-1.docker.io+library/；tag 与 digest 形态）+ registry v2 客户端（Bearer token flow 通用处理 `WWW-Authenticate`，匿名与凭证两种；404 → `ErrManifestNotFound`；429/5xx 退避重试；凭证只进 Authorization，绝不进日志/错误文本）。
2. `platform_settings` 增 `registry.host/username/password_cipher/password_fingerprint`（`state/registrysettings.go`；无迁移；password 只写只回指纹；空 = 保留现值，沿 ACME api_token 先例）+ API `GetRegistrySettings/UpdateRegistrySettings`（SystemService，admin + 平台管理员门）+ CLI `fleetly registry set/show/clear` + 事件 `registry.updated`（只带 host 与指纹，只增登记）。
3. substrate 装配：`WithImageRegistryCredentials(fn)`（runtime 注入：读设置 + Box 解密，惰性现读）；`ImageDigest` 新次序 = 平台 zot 前哨（不变）→ digest 引用直通 → tag 引用 registry 解析（命中设置 host 带凭证，否则匿名）→ 失败回落本机 inspect（RepoDigests 摘要；本地构建镜像返回空串沿旧语义）→ 双失败 `ErrImageMissing` 包原因；`registryAuthForImage` 扩展外部 registry 命中（设置 host 匹配即携 auth）。
4. engine：`resolveImage` 的 E_IMAGE_PULL_FAILED 文案改为点名 image + 底层原因（substrate 包装错误）；plan/spec 仍钉 digest（既有 planner 语义不变）。
5. 守卫测试：①mock registry resolver（匿名/凭证/404/429 退避）；④airgap 回落（registry 不可达 + 本机命中）；③digest 进 revision spec 回归；②⑤staging 真机项标注待执行（T2-0② 承接逐节点拉取实证）。
6. 硬约束：仓库内镜像引用仍过 `deploy/check-image-pins.sh`；错误码只增（如确需新码走注册表评审）；不 commit。

### IMPL-T1-2 实施记录（2026-09-26，实现会话）

**状态：实现完成，待用户验收（未 commit）。**

变更文件清单（每文件一句）：
- `internal/imageregistry/reference.go`（新包）：镜像引用解析（显式 host / docker.io → registry-1.docker.io + library/ 补齐 / tag 与 digest 形态；仓库路径字符集与大小写校验）与 host 归一单一事实源 `NormalizeHost`。
- `internal/imageregistry/client.go`：registry v2 最小客户端（WWW-Authenticate 挑战应答：Bearer token flow 通用 + Basic 直配；404 → `ErrManifestNotFound`；429/5xx 退避重试；凭证只进 Authorization/Basic Auth，错误文本零材料）。
- `internal/imageregistry/reference_test.go` / `client_test.go`：解析形态矩阵与 mock registry 全形态回归（匿名/凭证/404/429 退避/凭证零泄露）。
- `internal/imageregistry/client_manual_test.go`（新，默认不跑）：真实 registry 匿名解析探针（本机出网窗口手跑）。
- `internal/state/registrysettings.go`：`registry.host/username/password_cipher/password_fingerprint` KV 设置（无迁移；密文与指纹同事务落库，读面免解密；host 空 = 四键齐清；保存 + 审计 `registry.updated` + 事件 `registry.updated` 同事务）。
- `internal/state/registrysettings_test.go`：往返/归一/清除/密文-指纹同生共死/存储损坏 loud-fail/审计与事件脱敏。
- `proto/fleetly/server/v1/system.proto` + `genproto/fleetly/server/v1/system.{pb,pb.gw,grpc.pb,swagger.json}`：SystemService 增 `GetRegistrySettings/UpdateRegistrySettings`（GET/PUT `/v1/system/registry`；view 只回指纹）。
- `internal/api/registry.go`：两 RPC 实现（admin + `requirePlatformWriteFace` 双门；host 预校验 400；password 留空保留已存密文与指纹；清除形态四键齐清）。
- `internal/api/registry_test.go`：设置面主链（scope 门/加密落库/指纹/留空保留/清除/400/事件脱敏）、平台管理员门、指纹口径锚。
- `internal/api/scope.go`：两 RPC 登记 `ScopeAdmin`（注释同步平台凭据面口径）。
- `cmd/fleetly/cmd/registry.go` + `app.go` 注册：`fleetly registry show/set/clear`（密码只回指纹；set 留空保密码；clear = 清除）。
- `cmd/fleetly/cmd/registry_test.go`：CLI 全链与明文泄露负面扫描。
- `internal/substrate/client.go`：Client 增外部 registry 凭证读取缝 `WithImageRegistryCredentials`、回落留痕缝 `WithImageRegistryTrace`、解析/本机 inspect 注入缝（未导出字段，单测用）。
- `internal/substrate/registry.go`：`ExternalRegistrySettings` 类型、registry-first 解析（host 命中设置携凭证/否则匿名）、`registryAuthForImage` 外部 host 命中扩展（X-Registry-Auth 编码）、注入缝装配方法。
- `internal/substrate/services.go`：`ImageDigest` 新次序（平台 zot 前哨不变 → digest 直通 → tag registry-first → 本机 inspect 回落 → 双失败 `ErrImageMissing` 包装三因）。
- `internal/substrate/imagedigest_test.go`：registry-first/凭证命中两态/airgap 回落/双失败包装/直通/fleetly-local 跳过/设置读取失败显式/预算边界/平台腿优先级/外部 auth 编码。
- `internal/substrate/imagedigest_manual_test.go`（新，默认不跑）：本地单节点真机「零预拉 digest 部署」探针。
- `internal/substrate/client_timeout_test.go`：D2 超时用例的 `ImageDigest` 引用改 `fleetly-local/…`（tag 引用不再经 daemon 的语义变化；本地 inspect 腿预算断言强度不变）。
- `internal/substrate/registryauth_probe_manual_test.go`（审查会话遗留，本票范围）：EncodedRegistryAuth 披露面实机探针与语义结论注释。
- `internal/engine/engine.go`：`resolveImage` 的 `E_IMAGE_PULL_FAILED` 文案点名 image + 底层原因 + `fleetly registry set` 指引（顺带修正旧文案 params 反序）；digest 钉定/planner/drift 语义零变化。
- `internal/engine/resolve_image_registry_test.go`：E_IMAGE_PULL_FAILED 文案回归 + 守卫③（digest 进 ServiceSpec 与 revision overlay）。
- `internal/eventcode/events.go` / `eventcode_test.go` / `testdata/events.golden`：`registry.updated` 只增登记（docEvents + golden 再生成；两行 doc 注释 gofmt 归一）。
- `internal/runtime/provides.go` + `wire_gen.go`：`NewSubstrateClient` 装配外部凭证读数缝（state 现读 + Box 解密）与回落留痕（slog）；wire 再生成。
- `console/src/api/schema.d.ts`：`pnpm gen:api` 从新 swagger 再生成（只增 130 行，无 UI 改动）。
- `docs/plan/2026-09-26-torchwood-line-impl.md`：本实施记录。

测试清单与票面五条守卫逐条对应表：
| 守卫/验收条款 | 回归测试（新增，除注明外） |
|---|---|
| ① resolver 单测：mock registry 匿名/凭证/404/限流退避 | `TestResolveAnonymousBearerFlow`（匿名 token flow：挑战→token→Bearer 重试）`TestResolveCredentialBearerFlow`（Basic 凭证换 token）`TestResolveBasicChallenge`（无 token 服务自建 registry）`TestResolveNotFoundMapsSentinel`（404 哨兵）`TestResolveRetriesRateLimitAnd5xx`（429/5xx 退避与预算上限）`TestResolveDigestReferencePassesThrough` `TestResolveNeverLeaksCredentials` `TestResolveUnauthorizedWithoutChallengeIsExplicit` `TestParseForms` / `TestParseRejectsInvalidForms` / `TestNormalizeHost` / `TestParseChallengeForms` / `TestClientDefaults`；真实 registry 手跑：`TestManualResolvePublicRegistryImages`（Docker Hub alpine:3.19 与 ghcr sonarr:latest 均命中 digest） |
| ② staging 真机：ghcr 公共镜像零预拉部署成功 | **待真机**（无 staging 访问权，未虚构）。本地单节点同构实证：`TestManualPublicImageZeroPrePull`（清场本机镜像 → registry-first 解析 → digest 钉定 swarm 服务 → 任务 complete，真拉成功）；多节点 staging 归 T2-0② |
| ③ digest 进 revision spec | `TestDeployPinsResolvedDigestIntoServiceSpecAndRevisionOverlay`（底座服务实况镜像 = `alpine:3@sha256:…`；revision overlay 含同引用）；既有 `TestResolveImagePassesThroughRegistryPreflightEnvelope` 不回退 |
| ④ airgap 不回归：registry 不可达 + 本机命中 → 本机 digest | `TestImageDigestFallsBackToLocalInspectAirgap`（回落 + 留痕）`TestImageDigestLocalBuildImageKeepsEmptyDigest`（fleetly-local 零网络、空串旧语义）`TestImageDigestSettingsReadFailureFallsBackExplicitly` `TestImageDigestBothLegsFailWrapsImageMissing` `TestImageDigestRegistryLegBoundedByBudget`；`TestNonStreamingCallDeadlineBound` 的 ImageDigest 用例更新后仍钉住本机 inspect 腿预算 |
| ⑤ 私有镜像 + 凭证逐节点拉取成功 | **待真机**（T2-0② 承接；本地 Docker 29 单节点无逐节点腿）。凭证面单测：`TestImageDigestUsesSettingsCredentialsOnlyForMatchingHost`（命中/不命中两态 + docker.io 归一）`TestRegistryAuthForImageExternalHostMatching`（X-Registry-Auth 编码 ServerAddress 与零凭据面）；更新语义的 EncodedRegistryAuth 披露面实证见审查节 `registryauth_probe_manual_test.go` |
| 设置面 CRUD/指纹/权限/事件/CLI（本票新增面） | state：`TestRegistrySettingsRoundtrip` / `TestRegistrySettingsClear` / `TestRegistrySettingsValidation` / `TestRegistrySettingsStoredCorruptionFailsLoudly` / `TestRegistrySettingsAuditAndEventRedacted`；API：`TestRegistrySettingsFace`（含事件只带 host+指纹、密文为 age envelope）/ `TestRegistrySettingsPlatformAdminGate` / `TestRegistrySettingsFingerprintMatchesSHA256`；CLI：`TestRegistrySettingsCLI` |
| 事件注册表只增纪律 | `TestDocEventSetMatchesRegistry` / `TestGoldenSnapshot`（golden 显式再生成）/ `TestRegistryEventsReferencedInProduction`（新事件有生产发出来源） |
| 平台 zot 既有腿零回归 | `TestImageDigestRegistryBranchUsesHead` / `TestManifestHeadHitsV2PathWithBasicAuth` / `TestManifestHeadMissingMapsToImageNotFound` / `TestManifestHeadUnreachableStaysRawForEnvelopeMapping` / `TestRegistryAuthForImageGating` 全部不回退 + 新增 `TestImageDigestPlatformRegistryBranchKeepsManifestHeadPrecedence` |
| D2 超时闭环 | `TestNonStreamingCallDeadlineBound`（ImageDigest 用例改本地命名空间引用）/ `TestImageDigestRegistryLegBoundedByBudget` |

一手验证证据：
- 全量 `go test ./... -count=1` 全绿（32 包 ok，含新包 `internal/imageregistry`）；变更包 `go test -race` 全绿（imageregistry/state/substrate/engine/api/eventcode/runtime/cmd）；`go test ./sdk/go/...` 绿；`go vet ./...` 净。
- `golangci-lint run`（变更包）：新文件零问题；存量欠账未扩（gofmt 3 处均为未触文件 acmesettings.go/appgit.go/apps.go；errcheck/gosec/unused 为存量）。新引入的 QF1002 与自己的 gofmt 两处已修复。
- `sh deploy/check-image-pins.sh`：`OK — 28 image reference(s) digest-pinned, 0 exempt`。
- proto：`buf lint` 净；`buf generate` 再生成 `system.{pb,pb.gw,grpc.pb,swagger.json}`；console `pnpm gen:api` 再生成 `schema.d.ts`（+130 行）；console `pnpm typecheck` / `pnpm lint` / `pnpm test`（321 全绿）/ `pnpm build` 全净（无 UI 改动）。
- 本地真机探针（一手）：`FLEETLY_MANUAL_REGISTRY=1 go test -tags manual ./internal/imageregistry -run TestManualResolve -v` → `alpine:3.19 → sha256:6baf43584bcb78f2e5847d1de515f23499913acf12bdf834811a3145eb11ca1`；`ghcr.io/linuxserver/sonarr:latest → sha256:f247545d23ba8b233d6604575347e48a623fe6ad75dda02348bf81917f3b5c06`。`FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualPublicImageZeroPrePull -v`（本地 Docker 29.7.2 / swarm active）→ 清场本机镜像后 `ghcr.io/astral-sh/uv:latest → sha256:04d046b13e60d6bcec73cbc5e1cad25d680dea90c8573340950a0ac2d1aef424`，digest 钉定服务任务 `complete`（单节点真拉成功）。

偏离清单（实现中的决策与修正，均按纪律登记）：
1. **`fleetly-local/` 前缀跳过 registry 解析腿**：平台本机构建产物无 registry 身份，保持 v0.1 本地命名空间零网络语义（免每部署一次无谓远端查询）；其余 tag 引用一律 registry-first。回落/双失败语义不变。
2. **设置读取/解密失败不静默匿名**：装配缝返回错误时跳过该次 registry 解析（不降级成匿名尝试——配置了凭证却读不出属显式故障），原因进回落留痕；本机 inspect 亦失败时原因进最终 `E_IMAGE_PULL_FAILED` 文案。
3. **解析腿独立预算**：新增包级可配 `externalRegistryResolveTimeout`（10s，与平台 zot 前哨同口径），黑洞 registry 不挂满部署 tick；测试注入缩短预算钉住边界。
4. **digest 直通形态**：`@` 形态直通返回 digest（含 tag@digest 叠加与 `sha256:` 校验），非 sha256 算法按结构放行交由 registry 裁决；免网络免本机 inspect。
5. **设置 host 归一扩展**：`NormalizeHost` 同时服务引用解析与设置面（scheme 剥离、小写、尾斜杠、docker.io 家族 → registry-1.docker.io）；API 面非法 host（残留空白/路径段）以 400 点名拒绝——未新增错误码（避免注册表扩码，宿主校验在 API 层）。
6. **fingerprint 与密文同事务落库**（冻结设计键面）：读面免解密即回指纹；存储损坏（密文/指纹任一缺失）Load loud-fail。
7. **D2 超时用例引用调整**：`TestNonStreamingCallDeadlineBound` 的 `ImageDigest` 用例改 `fleetly-local/…`——tag 引用 registry-first 后不再触 daemon，用例意图（本机 inspect 腿预算）改用平台本地命名空间维持断言强度。
8. **E_IMAGE_PULL_FAILED 文案修正**：旧文案 `image %s of service %s` 实参反序（把服务名印进 image 位），本票改为点名 image + 底层原因 + 凭证设置指引（设计冻结第 4 条要求）。
9. **`NewSubstrateClient` 签名扩展 + wire 再生成**：装配层新增 state/box/app 依赖（惰性现读 + 解密 + 留痕）；wire_gen 显式再生成。
10. **事件 golden 与 doc 注释格式**：`registry.updated` 显式再生成 golden（只增登记），顺带把 `events.go` 两行 doc 注释（含 T1-1 遗留一行）gofmt 归一并修复一处存量 QF1002——格式面零功能变化。
11. **多两份手跑探针**（默认不跑，`-tags manual` + 环境变量门）：真实 registry 匿名解析与本地单节点零预拉部署，把真机腿钉成可复跑证据；不替代 staging/T2-0② 验收。
12. **留空保留语义对 host 变更同样生效**（ACME api_token 先例逐字落地）：改 host 而未给新密码时沿用已存密文/指纹——换 registry 请重录密码（proto 与 CLI 帮助文案已披露；`fleetly registry clear` 是清空路径）。

staging/真机待执行项（本环境无 staging 访问权，未虚构结果）：
- ② staging 多节点「ghcr 公共镜像零预拉部署」：T2-0②（本票已留本地单节点同构实证）。
- ⑤ 私有镜像 + 平台凭证逐节点拉取：T2-0②（本票已留 registryAuthForImage 编码与 EncodedRegistryAuth 更新/披露面证据；跨节点拉取需多节点窗口）。
- 建议 staging 窗口一并复核：registry 凭证设置保存后对下一次部署即刻生效（每次现读）；`fleetly registry set/clear` 后 `E_IMAGE_PULL_FAILED` 文案包含 registry 原因。

### IMPL-T1-3 方案可行性审查（2026-09-26，实现会话）

**结论：通过（无阻塞前置矛盾），进入实现。** 票面三处「由审查决定并说明」的裁量项（naming 前缀族、release 相位机挂钩点、看门狗预算取值来源）的裁决与理由见下；全部现状锚点成立。

现状锚点核实（票内 file:line 逐条）：
- `internal/compose/domains.go:118-164`（cron label 解析先例）✓：`parseCronSchedule` 表达式/时区/超时三键 fail-loud（E_LABEL_RESERVED + reason 上下文），label 白名单 `knownFleetlyLabels` 在 :53-61——init job 的 label 家族照此纪律扩展。
- `internal/cron/manager.go`（one-shot job 机器）✓：`jobSpecFrom` :694-715（模板克隆为 replicas=1 / restart-condition=none / Global=false + 一次性 label 集）、`trigger` :389-484（网络先行 → 建服务 → 台账/事件同事务）、`pollInFlight` :555-598（每拍任务判定 + 看门狗收口）、`closeRun` :603-651（删服务 + 终态事件）、`sweepOrphanJobs` :656-687（前缀清扫）——本票从中抽出可复用原语（见「挂钩点」第 1 条）。
- `internal/compose/validate.go:47`（depends_on 拒绝注释「orchestration order is managed by the platform release pipeline」）✓：作业顺序语义本就归发布管线，本票是它第一次真实承载。

必查项取证：

1. **swarm one-shot job 约束的 label 公式**（逐项核实）：
   - 底座翻译（`internal/substrate/services.go:253-264`）：`Job=true` → `ReplicatedJob{MaxConcurrent:1, TotalCompletions:1}`；job 模式**不写 UpdateConfig**（daemon 拒绝）；RestartPolicy 缺省 `condition=none`（失败即 failed 终态、不重试）；env/mounts/networks/secrets/constraints/resources/healthcheck 与长驻服务共用同一条 `buildSwarmSpec`——job 与 service 的投影同源由此成立。
   - 命名与识别面纪律（`internal/naming/naming.go:161-195` cron 族、`:294-330` dbjob 族注释）：一次性 job 服务的瞬时性必须**按前缀族可识别**，否则会被某一方的清扫误伤（在途 job 被删 = 静默丢失）。**裁决：新增独立前缀族 `fleetly-init-`**（不复用 cron 前缀——cron 孤儿清扫会把在途 init job 当 cron 残留误删；反之 init 清扫也会误伤在途 cron job）。公式 `fleetly-init-<team>-<prj>-<app>-<service>-<deploymentID8>`：尾缀取 **deploymentID 前 8 位**（cron 的 ulid8 是「每次触发一服务」，init 是「每次部署一服务」——确定性命名使创建幂等、重启续跑可寻址）。
   - **保留 slug 扩张**：team slug=`init` 时 `fleetly-init-<prj>-…` 命中 `IsInitJobName`（破坏性撞键：对账删除豁免/init 清扫误伤长驻服务）——`init` 进 `reservedTeamSlugReasons`（与 cron/db/dbjob 同证据链，E_TEAM_SLUG_RESERVED 受理层消费）。
   - label 集：job 服务 label = managed + app（三段限定形）+ process + team + project + `fleetly.init.run=<deploymentID>`（残留识别与日志归因锚；cron 的 `fleetly.cron.run` 同款）。

2. **与 release 相位机的挂钩点**（迁移安全论证 + 单写点纪律）：
   - 迁移安全：新代码不得先于迁移跑在旧库上 → init job 必须**先于 applyDesired（长驻服务 create/update）**完成；挂钩点 = releasing 相位内的子相位。
   - **裁决：复用 `deployments.phase` 子状态列**（blocked_waiting 先例），新增 `init_jobs` 值；`preparing/building → releasing` 的 **EnterPhase 同事务**携带 `Phase=init_jobs`（与 ReleaseStartedAt/WatchdogDeadlineAt/快照原子落位——不存在「已进 releasing 但 init 相位未记」的窗口）。相位推进（init 全过 → 清相位 + 重臂看门狗）走 `UpdateDeployment` 非转换就地更新通道（state/deployments.go:393-410 的既定口径：EnterPhase 管辖「转换 + 伴随字段」，子状态清位/时间锚正是 extra/就地更新面）——不绕 EnterPhase 单写点，也不越界用它。
   - `evaluateReleasing` 判定顺序：cancel 准入 → **init 相位分支（先于 watchBoundNode）** → watchBoundNode → 长驻判定。相位列单值约束下 init 相位优先于 blocked_waiting：节点不可用期由 job 看门狗兜底（超时失败，cancel 可用且收尾清 job），不引入第二子状态列（occam）。`classifyRecovering` 对 `phase==init_jobs` 直接放行回 tick 续跑（job 服务确定性命名 → ServiceInspect→缺失即创建 幂等；任务判定续跑），**不走** E_DEPLOY_INTERRUPTED 立即失败分类。
   - 失败/超时不绕道：统一走 `failUnswitchedOrSwitched`（D-REL-4 唯一入口）——失败分流/归位（replay 上一有效 revision）语义照旧；事件 `release.job_failed` / `release.job_timed_out` 先落（job 自身的失败记录），随后终态转移（deployment.failed payload 点名 job 与原因）。
   - 崩溃窗口诚实记录：init 全过 → 清相位提交 → 清场 → applyDesired；若在清相位后、applyDesired 前控制面崩溃，重启恢复按既有「无法判定」分类失败（E_DEPLOY_INTERRUPTED + 归位旧版本）——**安全失败**（迁移已执行、新代码未晋级、旧版本照常服务），不重跑迁移（重跑迁移的风险高于一次显式失败）。

3. **看门狗预算取值来源**（裁量项）：
   - **裁决：新增 label `fleetly.job.timeout`**（与 `fleetly.cron.timeout` 同形态同解析：正 Go duration，缺省平台预算 10m=沿 cron `DefaultJobTimeout`；孤儿 label（无 `fleetly.job`）拒绝——cron 的孤儿纪律同款）。理由：`fleetly.job` 与 `fleetly.cron` 互斥后 `fleetly.cron.timeout` 不可复用，而真实迁移（torchwood migrate）时长不可预设——per-service 预算是「复用 cron 看门狗语义」的完整兑现；engine.Config 新增 `InitJobTimeout` 注入位承载平台缺省（单测短预算）。
   - **per-job 预算语义**：每个 init job 独立计时（锚 = ReleaseStartedAt，job 创建紧随其后），任一 job 超其预算即 timeout（事件点名该 job + 预算）；相位级 WatchdogDeadlineAt = ReleaseStartedAt + max(各 job 预算) 作兜底（两处同值收敛，防时钟/边界漏判）。多 job 并行、全过才晋级；任一 failed/rejected/shutdown 立即失败（fail-fast；已完成的 job 不回滚——迁移前向语义）。

4. **env/secret/config 投影与 service 同源**：同一 `BuildPlan.buildServiceSpec`（planner.go:199-299）——env 三层合并/S3 system env/库网络/secret 挂载/卷/网络/资源限额/放置约束全部照编，init 模板只多 `Job=true` 标记（与 cron 的 :126-136 同路）；**快照（desired_spec 密文）保留 init 模板**（重启后执行形态来源）。secret 底座对象：`resolveSecretMounts`（secretinject.go:71-150）遍历全部服务已在 preparing 期 ensure（含 init 模板）；重启续跑路径在 evaluateInitJobs 建服务前再经 `ensureSnapshotSecrets` 幂等确保（SecretReference 必须携底座 ID——W3 真机教训）。网络先行（建服务前逐网 NetworkEnsure，cron trigger 同语义）。**Config 资源（OT-3/IMPL-T1-4）尚不在树**：当前投影链无 config 面；该票落地时 compose `type: config` 挂载在 buildServiceSpec 同一装配点，init 模板自动同源（本票不做预留代码，防悬空）。

5. **多 init job 并行 + 事件/错误码**：事件 `release.job_failed` / `release.job_timed_out`（eventcode 只增 + docEvents + golden 显式再生成）；错误码 `E_INIT_JOB_FAILED` / `E_INIT_JOB_TIMED_OUT`（HTTP 500，errcode 只增 + docCodes + count + golden）。payload 只带 app/app_id/service/job_service/budget/error（无敏感材料）。

6. **日志入 VL 并按 app/job 归因**：先例 = `internal/logs/manager.go:146-186`（发现面 → label 权威归属 → 零点全量回读 → ring/落盘/入湖同面合流）。**共享机制裁决**：日志发现面本就是「一次性 job 服务」族语义——把 `Port.CronJobServiceStates` 扩为 `JobServiceStates`（cron + init 两前缀族，基础实现 = `internal/substrate/logs.go` 的 managed+app 列举加 `IsInitJobName`），映射解析（前缀 + label 权威）、游标隔离（族标记 `\x00job\x00` 替代 `\x00cron\x00`，内存态无持久化影响）、暂态保留全部沿用；cron 采集行为（零点回读/增量/回收）零变化（既有测试机械改名后逐条语义不变）。

7. **审查新增的交互面**（不查即误伤）：
   - 引擎对账删除扫描（engine.go:909-922）与漂移「多余受管服务」判定（drift.go:252-270）都必须豁免 init 前缀：前者防 applyDesired 误删在途 init job（cron 同款豁免）；后者是同一豁免的配套——漂移 Extra 判定的自述前提「收敛原语会删」对这些瞬时族不成立（顺带把 cron 也纳入同一豁免，口径一致）。
   - MoveApp 摘旧名（move.go:181-185）必须豁免 init 前缀（在途 init job 让位跑完，由发布管线收口——cron 裁决同款）。
   - janitor 非终态超龄预算（state/janitor.go:70-72，装配 runtime/provides.go:228）= 2×(DeployTimeout+ObserveWindow)；含 init 相位的部署合法停留可到 InitJobTimeout+DeployTimeout → 装配改为 2×(InitJobTimeout+DeployTimeout+ObserveWindow)，防假告警（观测阈值面，非发布行为）。
   - 回滚重放**不执行** init job：`decodeSpecs` 过滤 Job 模板的既有口径（rollback.go:215-219 只重放长驻集）——迁移前向不回放，与 release-semantics §2.4「卷数据/DB 迁移不回滚」一致；本票不改。
   - 守卫⑤口径：无 init 模板的部署不携带 init 相位（phase 零写）、预算是 DeployTimeout、applyDesired 仍在计划拍内执行——专门回归测试钉死事件序列/相位推进/底座调用面，并在基线提交上复跑验证（见实施记录）。

结论：全部锚点成立、无票面矛盾；进入实现。实现中的决策与偏离记入实施记录。

### IMPL-T1-3 实施记录（2026-09-26，实现会话）

**状态：实现完成，待用户验收（未 commit）。** 引擎级守卫回归测试补齐后工作区全量 `go test ./...` 全绿（本会话仅新增 `internal/engine/initjobs_test.go`；实现文件零净改动——测试灵敏度对抗变异已逐一还原，`git diff --stat` 与本会话盘点时逐文件一致）。

变更文件清单（每文件一句）：
- `internal/engine/initjobs.go`（新）：init 相位评估——EnterPhase 同事务落相位/快照/看门狗锚；每拍 provision（网络先行 + 幂等确保 secret 底座对象 + 确定性命名建服务）+ 任务判定 + per-job 预算与相位兜底；失败/超时先落 `release.job_*` 事件再走 `failUnswitchedOrSwitched` 唯一入口；收尾清相位 + 重臂 DeployTimeout + 清场 + applyDesired 晋级；`sweepInitJobs` 孤儿清扫与 `SweepInitJobs` 测试/诊断入口。
- `internal/engine/jobrun.go`（新）：一次性 job 共享原语——`DefaultJobTimeout`（10m）、`JobSpecFrom`（单副本 / restart-condition=none / service label 收敛为受管件+归属+一次性运行锚）、`JobTaskVerdict`（complete/failed/rejected/shutdown 判定与单行归因）；抽取时 cron 行为逐字保持。
- `internal/engine/initjobs_test.go`（新，本会话补齐）：五条守卫逐条回归 + 执行形态/规划快照/env 同源/重启幂等补充（对应表见下）。
- `internal/engine/engine.go`：planAndRelease 挂 init 相位（同事务 patch.Phase + 预算取 max(各 job 预算)；无 init 模板相位零写）；applyDesired 删除扫描增 init 前缀豁免；tick 增 `sweepInitJobs` duty 与 `initScanNextAt` 频控字段。
- `internal/engine/planner.go`：`Plan` 增 `InitJobs`（Job=true 且 InitJob=true，不进长驻对账集）；BuildPlan 增 init job 模板路（timeout 归一、Kind 警告披露）。
- `internal/engine/ports.go`：ServiceSpec 增 `InitJob`/`InitJobTimeout`；`Config` 增 `InitJobTimeout`（Normalize 缺省 = DefaultJobTimeout）。
- `internal/engine/releasing.go`：evaluateReleasing 增 init 子相位分支（cancel 准入之后、watchBoundNode 之前）。
- `internal/engine/recovery.go`：classifyRecovering 对 `phase=init_jobs` 直接放行回 tick 续跑（不误分类 E_DEPLOY_INTERRUPTED）；cancelDeployment 先清场在途 init job。
- `internal/engine/drift.go`：漂移 Extra 判定豁免 cron/init 前缀（瞬时族的「收敛原语会删」前提不成立）。
- `internal/engine/move.go`：SweepMovedServices 摘旧名豁免 init 前缀（在途 job 让位跑完，由发布管线收口）。
- `internal/engine/safecall_test.go`：tick duty 清单增 `sweepInitJobs`（源扫描一致面）。
- `internal/compose/domains.go`：`LabelJob`/`LabelJobTimeout` 常量与 knownFleetlyLabels 扩面；`parseJobLabels`（值词表仅 init、与 fleetly.cron 互斥、孤儿超时拒绝、正 Go duration 归一）。
- `internal/compose/normalize.go`：job label 接入归一化；typed 层补 expose 禁令与 replicas 契约（>0 拒）。
- `internal/compose/spec.go`：Service 增 `InitJob`/`InitJobTimeout`（进归一化快照与 spec_hash）。
- `internal/compose/load.go`：`WarningKindInitJobDeclared`（只声明不部署服务的 Kind 披露）。
- `internal/compose/joblabel_test.go`（新）：label 值契约/互斥/孤儿超时/非法超时/expose/replicas/spec_hash 与 diff 九组回归。
- `internal/naming/naming.go`：`initJobNamePrefix`/`InitJobName`/`IsInitJobName`（独立前缀族 `fleetly-init-<team>-<prj>-<app>-<svc>-<deployid8>`）；team slug `init` 进保留字。
- `internal/naming/naming_test.go`：三段公式/跨族不误伤/参数校验 + 保留字清单扩面。
- `internal/cron/manager.go`：jobSpecFrom/jobTaskVerdict 内核抽到 engine（trigger/pollInFlight 换 `engine.JobSpecFrom`/`engine.JobTaskVerdict`；errOrText/singleLine 同步内化为 `jobErrOrText`/`jobSingleLine`；`DefaultJobTimeout = engine.DefaultJobTimeout` 别名）——cron 行为逐字保持。
- `internal/logs/logs.go`：`Port.CronJobServiceStates` → `JobServiceStates`；`CronJobRef` → `JobServiceRef`（cron/init 两前缀族共享发现面）。
- `internal/logs/manager.go`：pollCronJobs → pollJobServices；job 游标族标记 `\x00cron\x00` → `\x00job\x00`。
- `internal/logs/cron_test.go`：既有 cron 用例迁到共享面 + 新增 init job 零点回读/归因合流用例。
- `internal/logs/logs_test.go`：fakePort 发现面改名（jobStates/jobErr）。
- `internal/substrate/logs.go`：JobServiceStates 实现（cron + init 两前缀族列举）。
- `internal/apitest/apitest.go`：fakeLogPort 同名实现改名（API 测试装配）。
- `internal/state/deployments.go`：`PhaseInitJobs` 子相位常量。
- `internal/state/labels.go`：`LabelInitRun`（`fleetly.init.run` 归属锚）。
- `internal/state/janitor.go`：StaleDeploymentBudget 口径注释（含 init 相位）。
- `internal/runtime/provides.go`：janitor 非终态预算装配改 `2×(InitJobTimeout+DeployTimeout+ObserveWindow)`。
- `internal/errcode/codes.go` + `errcode_test.go` + `testdata/codes.golden`：`E_INIT_JOB_FAILED`/`E_INIT_JOB_TIMED_OUT` 只增登记（79 E + 5 W）与 golden 再生成。
- `internal/eventcode/events.go` + `eventcode_test.go` + `testdata/events.golden`：`release.job_failed`/`release.job_timed_out` 只增登记与 golden 再生成。
- `docs/plan/2026-09-26-torchwood-line-impl.md`：审查小节 + 本实施记录。

测试清单与票面五条守卫逐条对应表（本会话新增 = 行内全部 engine 测试；既有分层注明来源）：

| 守卫/补充面 | 回归测试 |
|---|---|
| ① job 失败 → release 失败（迁移安全） | `TestInitJobFailureFailsReleaseBeforePromotion`：failed 任务 → `release.job_failed`（payload 点名 job_service+原因）→ failed/`E_INIT_JOB_FAILED` + 首发 scale=0；job 在途期长驻服务零创建/零 update |
| ② job 超时 → timed_out + release 失败 | `TestInitJobTimeoutFailsReleaseWithTimedOutEvent`（`fleetly.job.timeout: 30s`，deadline=release+30s）；`TestInitJobPlatformDefaultBudgetTimesOut`（平台缺省 10m）；`TestInitJobInjectedPlatformBudgetTimesOut`（`Config.InitJobTimeout` 注入 15s）；`TestInitJobTimeoutIsPerJobBudget`（两 job 预算 30s vs 10m：小预算独立点火 + 事件点名该 job，不被 max 兜底掩盖） |
| ③ 多 init job 并行全过才晋级 | `TestMultipleInitJobsPromoteOnlyAfterAllPass`：两 job 交错完成；全过前无长驻 create/update、相位不清、零 healthy；全过后相位清位 + job 清场 + 新 spec 对账 + 健康门/观察窗照旧 |
| ④ job 零残留 | `TestInitJobServicesLeaveNoResidue`（成功/失败/超时三子测：fake 服务表 + ServiceRemove 调用双断言）；`TestSweepInitJobsClearsOrphansWithinOneScan`（孤儿三因——部署行缺失/终态/相位已清——一个扫描周期清除；在途 init job 与非 init 长驻不误伤）；`TestApplyDesiredSparesInitJobServices`（对账删除豁免） |
| ⑤ 无 init job 路径零变化 | `TestReleaseWithoutInitJobsIsUnchanged`：全程相位列零写、无 `release.job_*` 事件、首拍（计划拍内）即对账并切流 observing、看门狗 = release+DeployTimeout、唯一服务为长驻 web |
| 补充：执行形态快照 | 守卫①内联断言（Job=true / Global=false / replicas=1 / restart-condition=none / `fleetly.init.run=deploymentID` / 确定性命名 / 不继承 deployment 与 desired-hash label / 网络先行）；`TestBuildPlanInitJobTemplateSnapshot`（不进长驻集、进 plan.InitJobs 与快照、digest 钉定、45m 归一、Kind 警告） |
| 补充：env 投影同源 | `TestInitJobEnvironmentProjectionSharedSource`：init job 与长驻服务的 env 三层合并结果逐键同值（compose env + 平台层 pending env 同源注入） |
| 补充：重启续跑幂等 | `TestInitJobRestartRecoveryResumesIdempotently`：同部署连续两拍不重建 job 服务；控制面重启不被误分类、不写 error_code；续跑晋级至成功并清场 |
| 既有分层（审查会话，不回退） | compose `TestJobLabelValid`/`Defaults`/`InvalidValue`/`ExclusiveWithCron`/`TimeoutWithoutDeclaration`/`InvalidTimeout`/`ServiceExposeRejected`/`ReplicasContract`/`InSpecHashAndDiff`；naming `TestInitJobNameThreeSegment` + 保留字扩面；logs `TestInitJobLogsCollectedFromStart`、`TestJobServiceRefOfMapping`（两前缀族同面）；errcode/eventcode golden |

一手验证证据（本会话复验）：
- 全量 `go test ./... -count=1` 全绿（engine 13.5s，含新增 13 个守卫回归测试，其中 1 个三子测）。
- 变更包 `go test -race -count=1` 全绿：engine / compose / naming / logs / state / cron / errcode / eventcode。
- `go vet ./...` 净（exit 0）。
- golden：`internal/errcode`（79 E + 5 W）与 `internal/eventcode` golden 测试在列通过；本会话未再动 golden（只增登记与再生成已在审查会话完成）。
- 测试非空洞性对抗验证（临时变异实现 → 目标测试逐条转红 → 全部还原；还原后 `git diff --stat` 与变异前逐文件一致）：`len(unfinished)==0`→`<=1` 被守卫①③捕获；per-job 阈值改为 `2×budget` 被 `TestInitJobTimeoutIsPerJobBudget` 捕获（单 job 用例会被相位兜底同值收敛掩盖——这正是新增双预算用例的原因）；无 init 也写相位被守卫⑤捕获；applyDesired 摘除 init 豁免被 `TestApplyDesiredSparesInitJobServices` 捕获。

偏离清单（实现中的决策与修正，均按纪律登记）：
1. **本会话未发现实现 bug**：五条守卫回归首轮即绿；对抗变异仅用于验证测试灵敏度（非实现缺陷），变异已全部还原并以 `git diff --stat` 核对。
2. **新增 `SweepInitJobs` 导出入口**（测试/诊断直通频控闸）：孤儿清扫的生产驱动仍是 tick duty（30s 频控，重启即清零立即扫一拍）；导出单步入口与 `substrateRecon` 等 duty 的测试面惯例同型，审查裁决未涉及（测试接缝，非行为变化）。
3. **per-job 与相位兜底的判决覆盖**：实现与冻结裁决第 3 条一致（两处同值收敛：per-job 主路径 + deadline=max(各预算) 兜底）；单 job 用例无法区分两者，补双预算用例钉死 per-job 独立语义（未改实现）。
4. **防御分支登记**：`decodeInitJobTemplates` 为空时告警后清相位继续（理论不可达——相位与快照同事务落位，防静默卡死）；`initJobElapsed` 锚缺失回落 `created_at`（存量/异常行保持有界语义，同 H11 预算基线惯例）。
5. **日志族标记**：游标族标记由 `\x00cron\x00` 改 `\x00job\x00`（内存态键、无持久化影响；键内已含 job 服务名，cron/init 跨族不撞）——审查裁决第 6 条逐字落地。
6. **`internal/cron` 保留 jobSpecFrom 注释索引**：抽取后原实现删除，注释指向 engine 共享原语（防再次分叉出第二份实现）；`singleLine` 留在 cron（事件 payload 单行化，engine 侧 `jobSingleLine` 为 job 失败归因专用，两处文案契约同口径）。

staging/真机待执行项（本环境无 staging 访问权，未虚构结果；本票未跑真机探针）：
- 真实 daemon 上 `Job=true`（ReplicatedJob{MaxConcurrent:1,TotalCompletions:1}、不写 UpdateConfig、restart-condition=none）建服务与任务终态（complete/failed）端到端复核。
- 迁移时长校准：torchwood migrate 类长任务在真机上验证 `fleetly.job.timeout` 预算取值与超时事件的可行动性。
- init job 瞬态窗口（秒级到分钟级）内日志经 VL 的采集完整性（零点全量回读在真实 substrates 上的归属与保留）。
- 控制面真重启窗口的 `phase=init_jobs` 续跑（确定性命名服务寻址与任务判定续跑）；清相位后、applyDesired 前崩溃的安全失败路径（E_DEPLOY_INTERRUPTED + 归位旧版本，不重跑迁移）演练。

### IMPL-T1-4 方案可行性审查（2026-09-26，实现会话）

**结论：通过（无阻塞前置矛盾），进入实现。** 票面一处机制载体（compose `volumes: type: config`）被 compose-go schema 硬拒——**裁决改用标准 compose `configs:` 节（external 形态）承载同一语义**，理由与证据见下（这是设计字面的载体修正，不是语义降级；OT-3 的全部约束——任意绝对 target、只读、禁撞 /run/secrets、值不进仓库——逐条保留）；一处配额口径与设计字面不符（secrets 现无「每 app 条数」上限）——按「读 secrets 的现行上限照搬」指令照实现，登记为偏离项待设计档裁决。

现状锚点核实（票内 file:line 逐条）：
- `internal/api/secrets.go`（secrets 资源面形态）✓：`SetSecret/ListSecrets/RemoveSecret` 三 RPC、只写不回读（值与密文零出响应）、`maxSecretValueBytes = 64KiB`（handler 兜底，与 proto `max_len` 镜像）、`secretAudit`（secret.set/secret.removed，diff 只带 hash8）、`requireAppAccess` 角色门；scope 登记 `SetSecret/RemoveSecret = admin`、`ListSecrets = read`（scope.go:204-206）。
- `internal/compose/validate.go:256-263`（secret target 非空校验先例）✓ 现行 257-263（`/run/secrets/<target>` 固定根）；`:663-668`（volume type 白名单）✓ 现行 663-667（`type != "volume"` 即拒，bind/tmpfs 同款拒绝文案）。
- `internal/metrics/spec.go:35-36`（swarm config 内容寻址分发先例）✓：`fleetly-vm-scrape-<hash8>`/`fleetly-vmalert-rules-<hash8>` 内容寻址对象 + `ConfigEnsure`（返回 ID）+ `anchorSpec` 补 `ConfigID` + `gcScrapeConfigs`（按自描述 label 列族、除当前版外 best-effort 删除）+ mode 关闭全族清场——本票的内容寻址/GC 语义与其实证结论同族复用（`internal/metrics/docker.go:219-233` 的 ensure 幂等形态、`:277-300` 的 GC 读面）。

必查项取证：

1. **swarm config 引用形态（ConfigReference 必须携什么）**：本地 Docker 29.7.2 / swarm active 真机探针（`internal/substrate/configinject_manual_test.go`，`FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualSwarmConfigLifecycle -v`，2026-09-26 一手）结果矩阵：
   - `{ConfigID, ConfigName}`（缺 File）→ daemon 拒绝：`invalid Config: either File or Runtime should be set`；
   - `{ConfigID, File}`（缺 ConfigName）→ 拒绝：`malformed config reference`；
   - `{ConfigName, File}`（缺 ConfigID）→ 拒绝：`malformed config reference`；
   - `{ConfigID, ConfigName, File{Uid,Gid,Mode}}` → 接受，且服务实况（`ServiceInspect` 投影）可读回 name+target。
   ⇒ 三者全必填：**ID 与 Name 双写 + File 完整（Uid/Gid/Mode）**——W3 secret-ID 同族教训在 config 上逐条命中（比票面「必须携 ConfigID/File」更强：Name 也不可缺）。实现按此组装（engine 侧 ensure 取对象存在性，substrate 侧 `resolveConfigIDs` 按名 inspect 取 ID 再翻译）。
2. **引用整体替换 → 服务滚动**：同一探针，长驻服务（`sleep 600`，start-first）引用 config A（内容 `value-a`）→ `ServiceUpdate` 以 config B（`value-b`）整体换引用 → 新任务以新内容运行（任务容器内 `cat /etc/probe/cfg` = `value-b`，stdcopy 解复用实证），旧任务进入 `running/desired=shutdown`；服务滚动语义成立（task template 变更即触发滚动，与 metrics VM 滚动同链）。
3. **旧对象 GC 时机**：同一探针，引用在位时 `ConfigRemove(A)` 被 daemon 拒绝（`config '…-a' is in use by the following service: fleetly-probe-t1-4-config`）；**换引用后立即删除成功**——且删除时旧任务仍处 `running/desired=shutdown`（start-first 并存窗口）——⇒ 底座 in-use 判定基于**服务 spec 引用**而非任务态：引用换版即进入可清场态，已物化到任务内的文件不受对象删除影响。**GC 设计据此落位**：引擎在 `applyDesired` 服务收敛后按 app 归属 label 列举 config 对象、保留本次期望引用的集合、其余 best-effort 删除（daemon 拒绝 = 留待下一拍）；app 删除走 reap 全清（secrets 同款）。**不做**「创建时刻即删旧版」的激进路径（滚动窗口内旧 spec 仍可能引用，且并发部署语义应以收敛拍为界）。
4. **compose 载体（设计字面 vs schema 现实）**：compose-go v2.15.0 schema（module cache `schema/compose-spec.json` `$defs.service.properties.volumes.items.oneOf[1].properties.type.enum`）的卷 type 枚举 = bind/volume/tmpfs/cluster/npipe/image——**不含 config**；loader 默认开启 schema 校验（`internal/compose/load.go:46` 未设 `SkipValidation`），实测 `volumes: [{type: config, …}]` 直接被拒：`services.web.volumes.0.type value must be one of 'bind', 'volume', 'tmpfs', 'cluster', 'npipe', 'image'`。可行替代只有两条：① `SkipValidation = true` 放行整个 schema 层（把全部 compose 形状校验交给本仓 dict 白名单——回归面远超本票，**不可接受**）；② 用 **compose 标准 `configs:` 节**（顶层 `{name: {external: true}}` + 服务级 `[{source, target}]`），schema 原生支持、typed 解码 `ServiceConfigObjConfig{Source,Target,UID,GID,Mode}` 齐备（同探针实测通过）。裁决取 ②：语义与 secrets 面 1:1 同构（顶层 external 声明 = 值不进仓库 + 引用完整性哨兵在解析期、uid/gid/mode 平台受管、服务级 target 任意绝对路径、config 恒只读），且与 docker stack 原生 configs 形态一致（torchwood/messageloop 割接票的 compose 改写可直接写标准形态）。`/run/secrets` 前缀禁撞在解析期显式拒绝（secret 固定根不可撞）。
5. **source 存在性判定时机**（票面授权裁决项）：按 secrets 的 external 先例——compose 解析/校验期只做**声明面自洽**（服务引用必须在顶层 `configs` 声明且 external: true；source 形态校验），**值的存在性在引擎 preparing 期前哨**（app_configs 缺行 → `E_CONFIG_NOT_FOUND` 422 点名全部缺失名，一次暴露全量缺口；底座零 ensure/零服务写）。理由：解析层是纯函数（无 app 上下文，不读 state），secrets 先例同构；快照重放（回滚/归位/init 续跑）走同一存在性解析链（`ensureSnapshotConfigs`——config 名内嵌内容指纹，值轮换后旧名悬空 → E_CONFIG_NOT_FOUND 诚实失败，不静默改写）。
6. **配额口径**（读 secrets 现行上限）：secrets **现行只有值大小上限**（proto `max_len: 65536` + handler `maxSecretValueBytes = 64KiB` 镜像；名形态 `^[A-Za-z0-9][A-Za-z0-9._-]*$` ≤63），**无每 app 条数上限**（`internal/state/appsecrets.go` 全量通读：写通道无 count 门；`internal/api/secrets.go` 亦无）。config 照搬 = 64KiB 值上限 + 同一名形态 + 无条数上限（新增条数上限会与 secrets 口径分叉，且需新错误语义——登记为偏离项，设计档若要条数上限应两族同步加）。
7. **scope 裁决**（票面授权裁决项）：默认对齐 secrets 口径——`Set/Remove = admin`、`List = read`、`Get（明文回读）= admin`。理由：与 `SetSecret/RemoveSecret` 同级（明文配置是 app 行为的外置输入，可携带任意内容；写面与读面同门避免「可写不可读」的错位信任），票面默认即 admin，无足够硬理由降 deploy（若未来要降，应 env 先例同步评估，登记为设计档可选）。
8. **init job 投影同源**（T1-3 审查第 4 条的落地核对）：config 挂载在 `BuildPlan.buildServiceSpec`（planner.go:228-328）同一装配点——init job 模板经同函数编译（`svc.InitJob` 分支只加 `Job/InitJob` 标记），config 挂载自动同源；回归测试加显式断言（`TestInitJobConfigProjectionSharedSource` 形态）。
9. **对账/漂移/删除面**（不查即误伤，T1-3 同款纪律核对）：config 纯数据对象（非服务），不触 engine 对账删除扫描/漂移 Extra/MoveApp 豁免面；`ServiceState` 反解与 `driftSpec` 投影需补 `Configs`（外部篡改/对象漂移的判定面）——本票补齐。

审查新增的交互面（实现中必须接线/落测试）：
- `internal/substrate` 的 ensure/list/remove 三原语（engine `ConfigEnsurer/ConfigReaper` 端口实现）与 `buildSwarmSpec` 的 config 投影（ID 解析 + File 完整）；
- app 删除 reap 第二面（config 全清，secrets 同款 best-effort）；
- 快照重放面（`ensureSnapshotConfigs`，init 续跑/回滚/归位共用）；
- 错误码只增：`E_CONFIG_NOT_FOUND`（422，E_SECRET_NOT_FOUND 同族）——注册表 + docCodes + 计数 + golden 显式再生成；
- Console 新页/新路由/新页签 + `pnpm gen:api` 再生成；CLI `fleetly configs <set|get|ls|rm>`。

结论：全部锚点成立；进入实现。载体修正（标准 `configs:` 节）与配额偏离（无条数上限）按纪律登记，实现中的其余决策记入实施记录。

### IMPL-T1-4 实施记录（2026-09-26/27，实现会话）

**状态：实现完成，待用户验收（未 commit）。** 五条守卫与补充面回归全绿；审查期的真机探针（Docker 29.7.2 / swarm active）与全部静态门禁证据见下。

变更文件清单（每文件一句）：

- `internal/state/migrations/00024_app_configs.sql`（新）：app_configs 表（明文 value + hash8 内容指纹；UNIQUE (app_id, name) 承载覆盖即换版）；加法迁移 + Down 演练。
- `internal/state/appconfigs.go`（新）：`AppConfig` 与 `Upsert/Get/List/Remove` 的 Store/Tx 双面 CRUD（写通道校验、`ErrAppConfigNotFound` 哨兵）。
- `internal/state/appconfigs_test.go`（新）：明文往返/覆盖换版/列表序/删除哨兵/写通道校验 + 迁移 00024 Up/Down 演练。
- `internal/state/testdata/migrations.golden`：00024 行显式再生成（加性纪律）。
- `internal/compose/validate.go`：顶层 `configs` 入白名单、服务级 `configs` 拒条目解除；`validateConfigsDict`（external-only）与 `validateServiceConfigsDict`（显式 `{source, target}` 长语法；uid/gid/mode 拒；target 绝对单文件路径且 `path.Clean` 恒等〔拒 `//run/secrets/x`、`/etc/../run/secrets/x` 等非规范绕写〕；`/run/secrets` 前缀拒绝）；声明面引用完整性哨兵；`validComposeIdentifier` 抽公共字符集（secret/config 共用）。
- `internal/compose/spec.go`：`Spec.Configs`（顶层声明集合）与 `Service.Configs []ServiceConfig`（{source,target}，进快照与 spec_hash）。
- `internal/compose/normalize.go`：typed 层归一化（顶层声明排序 + 服务级挂载按 source 排序、target 原样）。
- `internal/compose/validate_test.go`：拒绝矩阵新增 configs 十一例（未声明 source/本地来源/无 external/null 定义/`/run/secrets` 两形态/非规范绕写两形态/相对路径/目录尾斜杠/缺 target/uid-gid-mode）+ `pos_configs_long` 对照正例；旧 `reject_configs` 整键拒绝用例改写为短语法拒绝。
- `internal/compose/configs_test.go`（新）：归一化形态/target 变更进 spec_hash 与 plan diff/名称形态校验。
- `internal/compose/testdata/whitelist.golden`：白名单键集显式再生成（顶层与服务级各增 `configs`）。
- `internal/naming/naming.go`：`ConfigName`（`fleetly-<team>-<prj>-<app>-config-<name>-<hash8>`）与包注公式表扩行。
- `internal/naming/naming_test.go`：表驱动公式行 + 换版换名 + 组件校验用例。
- `internal/engine/ports.go`：`ConfigMount`、`ServiceSpec.Configs`、`ServiceState.Configs`、`serviceSpecOf` 投影。
- `internal/engine/configinject.go`（新）：`ConfigEnsurer/ConfigReaper` 端口与 With 注入；`resolveConfigMounts`（preparing：存在性哨兵 E_CONFIG_NOT_FOUND + 内容寻址 ensure + 挂载装配）；`ensureSnapshotConfigs`（重放解析）；`gcAppConfigs` + `configKeepSet`（换版旧对象回收，长驻集 + 快照含 Job 模板）；`reapAppConfigs`（app 删除全清）；`configLabels`。
- `internal/engine/planner.go`：`PlanInput.ConfigMounts` 与 `buildServiceSpec` 的 Configs 装配（init job 模板自动同源）。
- `internal/engine/engine.go`：preparing 调 `resolveConfigMounts`；plan 传递 `ConfigMounts`；`decodeAllSpecs`（keep-set 汇总用）；applyDesired 头部 `ensureSnapshotConfigs` + 尾部 `gcAppConfigs`。
- `internal/engine/initjobs.go`：init job 建服务前 `ensureSnapshotConfigs`（与 secret 同拍）。
- `internal/engine/rollback.go`：回滚 preflight 增 config 存在性解析（悬空名 E_CONFIG_NOT_FOUND）。
- `internal/engine/appdelete.go`：tombstone 第二拍增 `reapAppConfigs`。
- `internal/engine/drift.go`：driftSpec/投影/diffDrift 补 `configs`（外部篡改判定面）。
- `internal/engine/move.go`：MoveApp 旧名 config 孤儿的口径注释扩行（诚实挂账，secrets 同款）。
- `internal/engine/fakes_test.go`：假底座 config 引用变更即任务替换（真机同语义）；`stateOf` 投影 Configs。
- `internal/engine/configinject_test.go`（新）：守卫①/②回归、快照重放、init job 同源、app 删除 reap、漂移篡改七测 + 三假实现。
- `internal/substrate/configs.go`（新）：`EnsureConfig/ConfigList/ConfigRemove` 三原语（幂等/归属 label 选择器/缺失即成功）。
- `internal/substrate/services.go`：`buildSwarmSpec` 增 configIDs 参数与 ConfigReference 投影（ID+Name+File{Uid,Gid,Mode}）；`resolveConfigIDs`；`serviceToState` 投影 Configs。
- `internal/substrate/services_test.go`：调用面签名随动。
- `internal/substrate/configs_test.go`（新）：ConfigReference 完整形态/缺 ID 显式报错/实况投影三测。
- `internal/substrate/configinject_manual_test.go`（新，默认不跑）：本地 swarm 真机探针（引用形态矩阵/换引用滚动/GC 时机）。
- `internal/api/configs.go`（新）：ConfigsService 四 RPC（Set 覆盖换版 + 审计 config.set；List 只投影名称/指纹；Get 明文回读；Remove 404 语义 + 审计 config.removed；`configAudit` target=`config:<app>/<name>`）。
- `internal/api/configs_test.go`（新）：CRUD/回读/审计脱敏/守卫③配额 4xx 点名/守卫⑤ scope 矩阵与真拦截链读 token 负面四测。
- `internal/api/scope.go`：ConfigsService 四方法登记（set/get/remove=admin、list=read）。
- `proto/fleetly/server/v1/configs.proto`（新）+ `genproto/fleetly/server/v1/configs.{pb,pb.gw,grpc.pb,swagger.json}`：ConfigsService 契约（buf generate 生成物）。
- `internal/runtime/grpc.go` / `gateway.go` / `provides.go` / `wire_gen.go`：ConfigsService 注册（gRPC + REST 两链）、wire provider、引擎 config 端口注入（`WithConfigEnsurer/WithConfigReaper`，wire 再生成）。
- `internal/apitest/apitest.go`：测试装配注册 ConfigsService（CLI/集成同路径）。
- `sdk/go/fleetly/client.go`：`Configs()` 访问器与字段。
- `cmd/fleetly/cmd/configs.go`（新）：`fleetly configs set`（`--value` | `--from-file`）、`get`（明文逐字输出）、`ls`、`rm`。
- `cmd/fleetly/cmd/configs_test.go`（新）+ `app.go` 注册：CLI 全链与用法错误回归。
- `internal/errcode/codes.go` + `errcode_test.go` + `testdata/codes.golden`：`E_CONFIG_NOT_FOUND`（422）只增登记（80 E + 5 W）与 golden 显式再生成。
- `console/src/api/{schema.d.ts,endpoints.ts,types.ts}`：`pnpm gen:api` 再生成（+228 行）+ 四端点封装与类型导出。
- `console/scripts/gen-api.mjs`：configs.swagger.json 进合并清单。
- `console/src/pages/AppConfigsPage.tsx` + `.test.tsx`（新）：列表/新增/编辑（同名锁定）/删除确认/明文查看对话框（admin 门）/空态/只读态七测；`data-testid` 沿用 `config-*` 前缀。
- `console/src/App.tsx` / `pages/AppDetailLayout.tsx` / `components/breadcrumbs.tsx`：路由、页签与面包屑接线。
- `docs/plan/2026-09-26-torchwood-line-impl.md`：审查小节 + 本实施记录。

测试清单与票面五条守卫逐条对应表（新增测试，除注明外）：

| 守卫/补充面 | 回归测试 |
|---|---|
| ① 未知 config source 解析/规划期拒绝 | 解析期：`TestValidationRejectMatrix/reject_config_source_undeclared`（服务引用未在顶层声明 → E_COMPOSE_UNSUPPORTED + 路径）；规划期：`TestDeployConfigMissingPreflight`（声明名不在 app_configs → `E_CONFIG_NOT_FOUND` 点名 app_conf，底座零 ensure/零服务写） |
| ② 内容变更 → 新对象 + 服务滚动 + 旧对象回收 | `TestConfigContentChangeCreatesNewObjectAndRollsService`（v1→v2：新内容寻址名 ensure、服务 spec 指向新名、假底座任务替换=滚动、旧对象清场且当前版保留）；命名侧 `TestHash8RotationIsNewName`（config 段）；真机探针 `TestManualSwarmConfigLifecycle`（换引用滚动 + 内容断言 + 换版后旧对象可删） |
| ③ 配额超限 4xx 点名 | `TestConfigsNameAndQuotaValidation`（空值/`>64KiB` → InvalidArgument 且文案点名 65536；名称形态 handler 兜底） |
| ④ target 撞 /run/secrets 拒绝 | `TestValidationRejectMatrix/reject_config_target_secret_root`、`/reject_config_target_secret_prefix`、`/reject_config_target_secret_nonclean`（`//run/secrets/...` 绕写拒绝）、`/reject_config_target_dotdot`（`/etc/../run/secrets/...` 拒绝）；另含相对路径/目录尾斜杠/缺 target 三例 |
| ⑤ 权限矩阵与 secrets 同构（List 不出值、Get 明文 admin、写面 admin） | `TestConfigsScopeMatrixMatchesSecrets`（登记面逐对与 SecretsService 对齐 + Get=admin）；`TestConfigsReadTokenRejectedOnWriteAndPlaintextRead`（真拦截链：read token set/get/remove 拒、list 放行）；`TestConfigsSetListGetRemove`（list 响应串零内容、Get 明文回读） |
| 补充：init job 投影同源（T1-3 审查第 4 条） | `TestInitJobConfigProjectionSharedSource`（job 服务与 web 同一内容寻址挂载、target 各自显式、跨模板 ensure 去重一次） |
| 补充：快照重放解析 | `TestSnapshotConfigResolution`（匹配 → 通过 + ensure；内容换版 → `E_CONFIG_NOT_FOUND` 悬空诚实失败） |
| 补充：app 删除 reap / 端口未接线不阻塞 | `TestReapDeletingAppsSweepsAppConfigs` / `TestReapDeletingAppsConfigSweepNotWired` |
| 补充：运行域漂移篡改可见 | `TestDriftDetectsConfigReferenceTamper`（平台形态零误报；外部改写 config 引用 → drift + `configs` 字段 diff） |
| 补充：compose 归一化/哈希/差分/白名单 | `TestConfigsNormalization` / `TestConfigsTargetChangeHashesAndDiffs` / `TestConfigsNameValidation` / `TestWhitelistGolden`（再生成） |
| 补充：底座投影形态 | `TestBuildSwarmSpecConfigFileTargetFullValues`（ID+Name+File 0:0/0444）/`TestBuildSwarmSpecConfigNotEnsuredFailsExplicitly`/`TestServiceToStateProjectsConfigs` |
| 补充：state/CLI/Console | `TestAppConfigsStore` / `TestAppConfigsMigrationUpDown`；`TestConfigsCRUDSurface`（CLI 全链 + 用法错误）；`AppConfigsPage.test.tsx`（7 测：列表/新增/编辑锁定/删除确认/明文查看/空态/viewer 只读） |
| 事件/错误码只增纪律 | `TestDocCodeSetMatchesRegistry` / `TestRegisteredCountByKind`（80 E + 5 W）/ `TestGoldenSnapshot`（显式再生成）/ `TestRegistryCodesReferencedInProduction`（E_CONFIG_NOT_FOUND 有生产发出来源） |

一手验证证据：

- 全量 `go test ./... -count=1` 全绿（含新包内全部新测）。
- 变更包 `go test -race -count=1` 全绿：state / compose / engine / substrate / api / runtime / errcode / naming / apitest / cmd/fleetly/cmd。
- `go test ./sdk/go/... -count=1` 绿；`go vet ./...` 净（exit 0）。
- `bash deploy/check-image-pins.sh`：`OK — 28 image reference(s) digest-pinned, 0 exempt`（本票零新增镜像引用）。
- `golangci-lint run --new --whole-files`：5 条全部为**未触行**的存量 gosec（`internal/engine/move.go:134` G115、`internal/errcode/errcode_test.go:16` G101、`internal/runtime/provides.go:983` G402、`internal/substrate/services_test.go:140/176` G101——对应行均不在本票 diff hunk 内，逐条核对）；新文件零问题；触文件 gofmt 干净（触前 3 处既有注释缩进随 gofmt 归一）。
- proto/console 再生成：`mise exec -- buf lint` 净、`buf generate` 生成 `configs.{pb,pb.gw,grpc.pb,swagger.json}`；`mise run console:gen-api` 再生成 `schema.d.ts`（+228 行）。
- Console 四脚本：`pnpm test` 328 全绿（54 文件；基线 321 + 新 7）、`pnpm typecheck` / `pnpm lint` / `pnpm build` 全净。
- 本地真机探针（一手；Docker 29.7.2 / swarm active / 单节点）：`FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualSwarmConfigLifecycle -v`：
  - 引用形态矩阵：`{ConfigID,ConfigName}`（缺 File）→ `invalid Config: either File or Runtime should be set`；`{ConfigID,File}` 与 `{ConfigName,File}` → `malformed config reference`；三写齐备 → 接受且 inspect 可读回 name+target；
  - 换引用滚动：start-first 下新任务运行且容器内 `cat /etc/probe/cfg` = 新内容，旧任务 `running/desired=shutdown`；
  - GC 时机：引用在位时 `ConfigRemove` 被拒（`config '…-a' is in use by the following service: …`）；换引用后立即删除成功（旧任务仍 running/desired=shutdown——底座 in-use 判定基于服务 spec 引用）。

偏离清单（实现中的决策与修正，均按纪律登记）：

1. **compose 载体：标准 `configs:` 节（设计字面 `volumes: type: config` 不可行）**：compose-go v2.15.0 schema 卷 type 枚举不含 config（实测拒绝；`SkipValidation` 会整体关闭 schema 层，不可接受）——审查记录第 4 条详载证据与替代裁决；平台语义逐条保留（显式 `target` 绝对单文件路径、恒只读、`/run/secrets` 前缀拒、external-only 值来源）。**这是用户可见 compose 契约的差异，请验收时明确追认**；后续割接票（T1-5/T2-4）按标准 configs 形态书写。
2. **配额：无每 app 条数上限**：secrets 现行只有值大小上限（64KiB）与名形态，无 count 门——config 照搬实际口径（审查记录第 6 条）；如设计档确需条数上限，建议两族同步加（新错误语义）。
3. **写面 scope = admin**（票面默认）：与 `SetSecret/RemoveSecret` 同门；明文读面（Get）同门，避免「可写不可读」的错位信任（审查记录第 7 条）。
4. **CLI `set` 增 `--from-file`**：配置内容多为多行文件体，`--value "$(cat …)"` 的 shell 转义/历史污染不可取；`--value` 与 `--from-file` 恰给其一（用法错误点名）。`get` 逐字输出明文（无附加换行，可管道）。
5. **GC 落点 = applyDesired 尾部（keep-set）+ app 删除 reap**：keep-set = 本次期望长驻集 ∪ 快照内全部模板（含 Job——init-only config 不因 Job 清场被误删/反复重建）；in-use 删除由底座拒绝 → 留待下一拍（真机实证：引用换版后即允许删除，判定基于服务 spec 而非任务态）。不做「变更时刻即时删旧版」（并发/滚动窗口语义以收敛拍为界）。
6. **`E_CONFIG_NOT_FOUND` 新码（422，只增）**：注册表 + docCodes + 计数（79→80 E）+ golden 显式再生成；生产引用点 = `internal/engine/configinject.go`（preparing 前哨 + 快照重放悬空名）。
7. **运行时接线面**：`NewEngine` 增 `WithConfigEnsurer/WithConfigReaper`（substrate.Client 双端口）；gRPC + REST 两链注册 ConfigsService；wire 显式再生成（`mise run generate:wire`）；apitest 与 SDK 访问器同步。
8. **漂移投影补 `Configs`**（`driftSpec`/`serviceSpecOf`/`ServiceState`/`diffDrift`）：外部 `docker service update --config-*` 篡改可见（回归 `TestDriftDetectsConfigReferenceTamper`）；无此补齐会成漂移盲区。
9. **MoveApp 旧名 config 孤儿**：与 secrets 同款诚实挂账（`SweepMovedServices` 注释扩行）——旧 label 值选择器扫不到，窗口 = MoveApp 与 DeleteApp 罕见叠加；不在本票扩面。
10. **假底座语义对齐真机**：`fakeSubstrate.ServiceUpdate` 把 config 引用变更视为任务替换（同镜像也滚动），使守卫②的「滚动」断言有实义；`stateOf` 同步投影 Configs（否则 config 挂载会永久假漂移）。
11. **gofmt 归一**：变更文件内两处既有注释缩进（`internal/runtime/provides.go`）随 gofmt 修正；不影响语义（`--new` 检查触文件 gofmt 干净）。

**验收追认（2026-09-27）**：用户以「提交推送」指示验收，偏离 1（标准 `configs:` 节替代设计字面的 `volumes: type: config`）与偏离 2（无每 app 条数上限——与 secrets 现行口径一致）视为追认；后续割接票（T1-5/T2-4）按标准 `configs:` 形态书写。

staging/真机待执行项（本环境无 staging 访问权，未虚构结果；本票已做单节点真机探针）：

- 多节点 config 分发与滚动（swarm config 逐节点物化；本票单节点实证 + 平台台账归 T2-0② 同窗口）。
- init job 挂 config 的真机端到端（`migrate` 类服务挂 bootstrap SQL；本票以假底座断言同源投影，真机归 T2-4 割接前的 compose 验证）。
- Console AppConfigsPage 真机走查（列表/新建/明文查看/删除；staging 窗口）。
- 与 IMPL-T1-5（messageloop：`mlbridge.yaml` → Config）与 IMPL-T2-4（torchwood：`config.yaml`×3 + `bootstrap-roles.sql` → Config）的 compose 改写联动（按标准 `configs:` 形态）。
