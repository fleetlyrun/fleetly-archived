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
IMPL-DB-0(dbtools多PG版本) ──→ IMPL-DB-1(PG目录化) ─────┼─→ (T2 割接前置)          ├─→ IMPL-T2-3(dispatcher改造)
IMPL-T2-2(build-upload) ─────┘                         ─┴─→ IMPL-T2-4(torchwood割接) [T2 出口]
IMPL-T1-6(SDK TLS,独立)   IMPL-T3-*(P2,后置)
```

| 波次 | 票 | 合计预估 |
|---|---|---|
| T1 | T1-1 / T1-2 / T1-3 / T1-4（可并行）→ T1-5、T1-6 | 10-18d |
| T1.5（可并行 T1） | T15-1 | 3-5d |
| 独立先行 | DB-0（DB-1 前置）→ DB-1 | 2-4d + 2-3d |
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

### IMPL-DB-0（DB-1 前置）dbtools 多 PG 大版本工具面

- **目标**：dbtools 备份/恢复执行体支持 PG 16 与 PG 18 两代工具面；job 按实例模板的 PG major 选执行镜像/工具；恢复 PGDATA 参数化。解除 DB-1「每个目录条目过 create→backup→restore」的结构性阻塞（DB-1 审查记录 option A，2026-09-27 用户裁决）。
- **现状锚点**：`deploy/Dockerfile.dbtools:81`（`FROM postgres:16@sha256:a3b7…`，工具面 16.15——W4 「与引擎逐位同版」原则）；`internal/database/adapters.go:46`（`DefaultDatabaseToolsImage`）与五处 ID switch、`restorePostgresJobScript` 硬编码 `PGDATA=/var/lib/postgresql/data/pgdata`；`.github/workflows/dbtools.yml`（单镜像 build-push + cosign）；台账 `docs/runbooks/image-prepull.md`（末行 #21）。
- **改动点**：Dockerfile.dbtools 双 PG 工具面（双 final target 或新文件，**纯 COPY、全 FROM digest 钉定**）；`dbtemplate.Template` 增身份字段（Engine/Distribution/Major，既有四条目 ID/Image/字段值逐字不变）；render/adapters 分派轴 ID→Engine 迁移（行为零变化）；job 镜像按 PG major 选择（新常量钉定）；恢复 PGDATA 自模板条目/`RestoreInput.VolumeTarget` 参数化（PG16 现值不变）；dbtools.yml 扩双镜像构建 + cosign；新 digest 台账（顺延 #22/#23）+ Go 常量 + check-image-pins 三锚一致；文档（工具面版本纪律：与实例数据目录同 major）。
- **守卫与验收**：①PG18 真机探针：真实 PG18 实例跑通 dump→verify（`pg_restore --list`）→restore 重放全链（原始输出；不依赖 DB-1 词表，adapter 原语级）；②PG16 全链零回归（全量 go test + 本地等价探针）；③镜像发布：CI 新 digest + cosign 签名（dispatch 可用；不可用则如实挂账不虚构）；④redis/mysql/mongo 零回归。
- **依赖**：无（DB-1 前置）。**预估** 2-4d。

### IMPL-DB-1（DT-9）PG 模板目录化（DB-0 后行）

- **目标**：dbtemplate 目录化；词表增 `postgres-18`（vanilla）与 `percona-postgresql-18`（含 pgvector）；发行版零新增适配器；大版本升级不做（创建钉死）。
- **现状锚点**：`internal/dbtemplate/dbtemplate.go:23-46`（注册表与 `E_DB_TEMPLATE_UNSUPPORTED`）、`internal/dbtemplate/render.go`、`internal/database/adapters.go`、E4 35/35 真机矩阵。
- **改动点**：注册表目录化重构（engine 通用 descriptor）；两新模板镜像 digest 钉定 + check-image-pins 台账登记；CreateDatabase 校验走既有路径；Console create-database-dialog 增模板选择器；文档（大版本升级 = dump/restore 新实例）。
- **守卫与验收**：**每个目录条目过 create→backup→restore 回归矩阵**（把「发行版不新增适配器」从断言钉成事实）；percona 镜像与官方镜像 env/entrypoint 兼容性实证（PGDATA/init 语义）；未知模板 4xx 既有错误码不破。
- **依赖**：IMPL-DB-0（dbtools 多 PG 大版本工具面）。**预估** 2-3d。
- **2026-09-27 阻塞裁决注**：本票第 0 步审查发现 dbtools 单大版本工具面阻塞（证据与裁决选项见 §4「IMPL-DB-1 方案可行性审查」）；目录化身份字段（Engine/Distribution/Major）与分派轴迁移、job 工具按 major 选择、恢复 PGDATA 参数化移入 DB-0；本票实装面 = 两新条目（含 percona 原生 `/data/db` 路径与 pgvector 说明）+ Console 选择器 + 文档 + 全目录矩阵跑通 + 台账/词表收尾。

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

### IMPL-T15-1 方案可行性审查（2026-09-26/27，实现会话）

**结论：通过（无阻塞前置矛盾），进入实现。** 参与模型按「归属（tenancy）与网络参与两面分离、网络参与 = 显式 attach/detach opt-in、缺省不参加」落（与 OT-1 原文与 RBAC §12 挂账行逐字一致，反证搜索见下）；票面「proto projects CRUD / state projects 表 / app.project_id / project 非空禁删」经核实**大部分已由 RBAC W0（W2-S1/S3）落地**，本票真正的新增面收敛为「项目网生命周期 + 成员服务双挂投影 + attach/detach + recon networks 扩面 + Console 最小面 + CLI」（差集表见下）。

#### A. 票内「现状锚点」逐条取证

- `internal/naming/naming.go`：`NetworkName` = `fleetly-<team>-<prj>-<app>-net`（:165-176，三段公式）✓；`NetworkAlias` = compose 服务名仅 app 网（:181-186）✓；用户自报 aliases 拒绝 = `internal/compose/validate.go:864-887`（`validateServiceNetworksDict`：每网络配置非空即拒，文案「service aliases are managed by the platform from compose service names」）✓；包注公式表在 :27-39（无项目网行——本票扩行）。
- `internal/engine/substrate.go`：`substrateRecon` duty（:52-94）30s 频控（`substrateReconInterval` :46）、services-only（候选 = `ListActiveApps` 派生态 running/degraded；逐服务 `ServiceInspect`）、missing/drained 进程内记忆与派生态修正模式 ✓；测试骨架 `internal/engine/substrate_test.go`（缺失/在岗/瞬态错误/时间闸/drain 五族）✓。
- `internal/engine/planner.go:294-305`（networks 装配点：app 网 + rustfs + 库网三源）✓ 现行 294-305；`buildServiceSpec` :234-340。
- `internal/engine/ports.go`：`NetworkAttach`（:94-99，Name+Aliases）✓；`Substrate` 端口 :248-267（NetworkEnsure/Service*/TaskList）。
- `internal/substrate/services.go`：`buildSwarmSpec`（:166-298）投影 `swarm.NetworkAttachmentConfig{Target, Aliases}`（:224-231）✓；`NetworkEnsure`（:129-157）恒带 `fleetly.managed=true`（:141-143）。
- 附加现状（票面未列、实现必需）：`NetworkList/NetworkInspect/NetworkRemove` 在 substrate 端口**不存在**——moby client v0.6 具备 API（`client/network_list.go`、`network_remove.go`，`network.Summary` 含 Name/Labels/Containers）；`ingress/traefik.go` 与 `database/docker.go` 各有自己的 NetworkRemove/NetworkID 实现（不复用）。网络对象 label 下发能力：`NetworkCreate` 已带 Labels（substrate/database/rustfs/ingress 四处同款）✓。

#### B. 票面 vs 现状差集表（W0 既有 vs 本票新增）

| 票面改动点 | 现状核验 | 判定 |
|---|---|---|
| proto projects CRUD | `projects.proto` 已有 Create/List/Get/Update/Delete + 成员三 RPC + MoveApp/MoveDatabase（:42-106） | **已存在**（本票零改动该面） |
| state projects 表 + app.project_id | 00018 建表/加列 + 00019 收紧 NOT NULL + `UNIQUE(project_id,name)`（apps_new :27-47） | **已存在** |
| project 非空禁删（空才可删） | `state/projects.go:299-349` `DeleteProject` 存活资源计数拒（`ErrProjectNotEmpty`）+ api 409 映射（projects.go:84-85/223-238） | **已存在**（本票只加网络面交叉测试，不改语义） |
| team/project label 集 | `state/labels.go:20-25` `fleetly.team`/`fleetly.project`；`naming/labels.go` 服务建立即写 | **已存在** |
| Console 项目页 | `ProjectsPage.tsx` / `ProjectDetailPage.tsx` 已有一级页 + 详情（成员/应用/库卡） | **已存在**（本票加网络卡/列） |
| 项目网 overlay 生命周期 | 无任何项目网概念（`fleetly-project-` 前缀不存在） | **新增** |
| 成员服务双挂 + 项目网别名 | 单挂 app 网（planner :299） | **新增** |
| attach/detach RPC + state 参与位 | 无 | **新增** |
| state 参与成员资格/审计/迁移 | 无 | **新增**（app 列 + 审计 + 事件 + 00025） |
| recon networks 扩面 | services-only | **新增** |
| 项目网 GC（成员清空/项目删除） | 无 | **新增**（独立 duty：空网 + 零端点才回收） |
| CLI projects | `cmd/fleetly/cmd` 无 projects 命令（`newProjectsCmd` 不存在）；SDK 无 `Projects()` 访问器 | **新增**（最小集 attach/detach/show） |

#### C. 参与模型裁决（按证据）

票面/设计原文链条：OT-1「**app ∈ 恰一 project（可选，缺省不参加任何项目网，维持 app 私网隔离现状）**」（设计档 :51）；RBAC §12「同项目 app 互访的**显式 opt-in**（项目网络形态，需求出现再启）」（rbac-teams :265）；票面「proto（projects CRUD + **attach/detach**）」「detach 走服务滚动」。现状证据亦一致：`apps.project_id` NOT NULL 是唯一项目归属（00019），网络投影唯一入口是 planner 的 app 网单挂。

**裁决：归属与网络参与两面分离。**
- 归属（tenancy）= `apps.project_id`（恒有，RBAC W0 已落地）；
- 网络参与 = 新 app 级布尔位（迁移 00025 `apps.project_network_attached`，**加法默认 0**）——语义 = 「本 app 的成员服务在该 app 当前项目网内可互访」，**唯一改变路径 = 显式 attach/detach RPC**；缺省不参加 ⇒ 既有 app 行为零变化（投影仅在位时追加一段网络）。
- attach 生效 = 项目网幂等 ensure（带自描述 label）+ 入队「参与变更重部署」（沿 MoveApp 换名重部署先例：正常发布管线产出新 revision/快照，服务滚动由 applyDesired 的 ServiceUpdate 承载，不新造绕过发布链路的直改 spec 通路）；detach 生效 = 摘网随下一次重部署的 task template 变更滚动。
- 反证搜索（「某个守卫只能由缺省自动双挂满足？」）：①跨 app DNS 守卫（exec 内 `ping <app>-<service>`）在显式 attach 下同样成立（测试按 attach 后断言）；④别名隔离与该模型同构；DT-5 的 task-group 网是 T2-1 另网，不依赖缺省双挂；T1-5/T2-4 割接票（mlbridge 内网切法）显式声明为项目内网别名消费方，未预设缺省。**未发现反证，按默认读法实现。**

**MoveApp 交叉语义裁决**：参与位是 **app 级**属性，MoveApp 改派**保留**该位；项目网投影随 app 的**当前**项目推导——新 revision 的成员服务双挂新项目的项目网（`fleetly-project-<newProjectID>`），旧项目网在失去最后一名成员后由 GC duty 回收（零端点判据）。理由：参与位表达的是「本 app 愿意与所在项目同伴共享网络」这一意图，归属变更不改变该意图；改派已强制换名重部署（全量重投影），旧网不留悬挂（GC 拍内回收）。测试钉死（`TestMoveAppPreservesProjectNetworkParticipation`）。

#### D. 必查项取证

**1. service update 增/摘网络的滚动语义（本地 Docker 29.7.2 / swarm active，一手探针 2026-09-27）**：

原始观测（`docker service create/update`，`docker service ps` 逐次快照）：
- 基线服务（alpine sleep 600，挂 net-a）：任务 `91ozad8g0m2z Running`。
- `--network-add net-b`（CLI 缺省 stop-first）→ **任务整体替换**：新任务 `npp4kbg244h0 Running`，旧任务 `91ozad8g0m2z Shutdown/Shutdown`（停旧起新，无并存）。
- 单独改写 `--update-order start-first`（非 task template 变更）→ **零任务替换**（同任务 `npp4kbg244h0` 持续 Running）——与 Spike B2「label/非模板字段不触发重建」口径一致。
- start-first 下 `--network-add net-c` → **新旧并存滚动窗口**：新任务 `7j1nujuzhseu Running/DesiredState=Running` 与旧任务 `npp4kbg244h0 Running/DesiredState=Shutdown` 并存（随后旧任务下线）。
- `--network-rm net-b` → 同款任务替换（`wuznaa35wekk` 新、`7j1nujuzhseu` 转 Shutdown），网络集合 = {net-a, net-c} 读回正确。
- 平台侧结论：**网络集合是 task template 的一部分，增/摘网必然触发任务重建**；平台缺省更新序 start-first（planner `composeOrder`）⇒ 滚动窗口有新旧并存、不中断；有卷/global 服务强制 stop-first ⇒ 有停机窗口。Console/runbook 按此诚实标注。
- 附带 GC 安全性实证：`docker network rm net-c`（仍被服务引用）→ daemon 拒绝：`FailedPrecondition: network <id> is in use by service <id>`——**in-use 移除由底座兜底拒绝**（GC 即使判据竞态也不会拆掉在役网络），零端点判据 + 拒绝兜底双层安全。

（探针复跑面：本票落 `internal/substrate/projectnetwork_manual_test.go`，`FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualProjectNetwork -v`，见实施记录。）

**2. 别名 `<app>-<service>` 与 naming 契约评审（不混流论证）**：
- DNS 解析域 = 容器所属网络（swarm 内嵌 DNS 按 netns 网络逐个解析）——短名别名 `<service>` 只写在 app 私网（`naming.NetworkAlias` = compose 服务名，planner 只为 app 网写），项目网只写 `<app>-<service>`（新公式）；跨 app 容器不在对方 app 私网内 ⇒ 短名必然解析不到，混流需要「同名短名与服务落在同一网络」这一结构条件，平台投影不存在该形态。
- 用户自报 aliases 在校验层拒绝（A 项证据）⇒ 不存在用户制造的第三形态别名。
- 命名扩展方式：新公式行进 `naming.go` 包注公式表（与 ConfigName 行同格）+ 表驱动测试（`ProjectNetworkName` / `ProjectNetworkAlias` 参数校验与逐字公式）；**不改**任何既有公式（票面「不改命名公式主体」）。
- 撞键审计：项目网名 `fleetly-project-<projectID>`（项目平台 ULID **全量**——**实现期修正**：票面建议的「id8 = 前 8」在测试中即实证撞名（ULID 前 8 只承载 40 位时间戳成分，256ms 窗口内创建的两个夹具项目前 8 位相同）；截断还会把「第二个项目的项目网 ensure」静默并入第一个网络（NetworkEnsure 已存在即成功）——网络静默合并是安全级缺陷，故取全量 ID：结构性唯一（主键）、给 42 字符网络名无解析面约束）vs app 网 `fleetly-<team>-<prj>-<app>-net`（三段 slug + app 名全部 `[a-z0-9._-]`，含小写与 '-'）——即便 team slug = `project`（合法单词制），候选串 `fleetly-project-<prj>-<app>-net` 含 `-` 与小写字符，**结构上不可能**等于 `fleetly-project-` + 纯 Crockford 尾段（'-' 不在 Crockford 字符集）；service/secret/config/volume 与网络分别是底座不同命名空间，且公式不同。测试 `TestProjectNetworkNameCannotCollideWithAppNetwork` 钉死（含 team=project 形态 + 同窗口双 ID 反证）。

**3. 项目网命名与前缀族全局撞键审计**（`fleetly-` 前缀族全列）：
| 前缀/固定名 | 对象 | 与 `fleetly-project-<projectID>` 关系 |
|---|---|---|
| `fleetly-<team>-<prj>-<app>-net` | app 网 | 结构不相交（见上） |
| `fleetly-db-<team>-<prj>-<name>-net` | 库网 | 前缀不同段（`db-`），不相交 |
| `fleetly-cron-…` / `fleetly-init-…` / `fleetly-dbjob-…` | 一次性 job 服务 | 服务命名空间，不同对象类 |
| `fleetly-rustfs-net` | 组件网（固定名） | 不同串；入 recon 期望白名单（组件 duty 生命周期） |
| `fleetly-system` | 组件网（ingress registry 固定名） | 同上 |
| `fleetly-victorialogs`/`fleetly-victoriametrics`/`fleetly-cadvisor`/`fleetly-node-exporter`/`fleetly-vmalert`/`fleetly-exec`/`fleetly-ingress`/`fleetly-registry`/`fleetly-buildkit` 等 | 服务/卷/容器名 | 服务与容器命名空间，不同对象类；`fleetly-rustfs-net` 唯一平台网络固定名已列 |
| 保留 team slug 清单（cron/init/db/dbjob/rustfs/registry/acme/metrics/victorialogs） | team slug 守则 | **`project` 不入保留清单**：`fleetly-project-…` 与上表全串不相交，且无团队 slug 顶到 `fleetly-<team>-…` 首段时会撞项目网名（结构性证明同上）；project slug 居第二段不邻前缀族（rbac-teams §4.3 原判维持） |
- **孤儿网识别用自描述 label**：新 label `fleetly.project-network=<projectID>`（网络对象；值取项目 ULID——slug 是 team 内局部量，ID 全局唯一）；识别面 = `managed=true` + `fleetly-` 前缀 + **期望集差**（期望集由 state 推导：active/deleting app 网、非 deleted 库网、成员项目网、组件固定名白名单），不靠前缀猜测（前缀族太多，票面要求）；带项目网 label 的漏网对象走 GC 分支（零端点才回收），不带 label 的无法归因孤儿只披露不删。

**对账两方向与「派生修正」的最终定义**（票面授权裁决项）：
- **state→swarm（应存在而缺失的项目网）**：`substrateRecon` networks 面同周期**暴露**（`network.missing` 事件 + 审计，按网络名节流）**且**项目网 duty 幂等重 ensure（`NetworkEnsureWithLabels`）——「派生修正」= **恢复平台自建、归属明确的对象**（平台是项目网的所有者，重建不涉他人物件；与 services 面「重建是部署链路职责」的差别在于网络没有发布链路归属）。事件先于修正可观察（tick 序：substrateRecon → reconcileProjectNetworks），披露不因自愈而消失。
- **swarm→state（state 外 `fleetly-` 前缀平台网）**：`network.orphaned` 事件 + 审计（节流），**不静默删**（无法归因 ⇒ 可能伤及他人物件；清理归人工/后续票）。**唯一例外** = 带 `fleetly.project-network` label 且**无成员 + 零端点**的项目网：它是归属明确的本平台对象（成员清空后的回收残留），由项目网 duty 回收（零端点判据 + 底座 in-use 拒绝兜底双层安全）。
- 「派生修正」不含视图字段改写（网络无派生态行），落地形态 = 事件 + 审计 + 上述幂等动作。

**4. staging UDP 未放行期 placement 同节点指引落文档**：
- 证据：`docs/design/2026-09-20-multi-node.md:66-67`（7946/tcp+udp gossip、4789/udp VXLAN 双向放行表）+ `docs/runbooks/vps-dogfooding.md:136`（W3-F2 真机实测：node2 跨节点 DNS NXDOMAIN/VIP 不可达、tcpdump 0 包、TCP 7946/22 全通 ⇒ VPC/云防火墙滤 UDP；跨节点 overlay 数据面整体不通）。
- 落点：`docs/runbooks/project-networks.md`（新）——UDP 未放行期的**同节点 placement 指引**（走平台既有放置机制：compose `deploy.placement.constraints` 的 `node.labels.fleetly.*` 命名空间 + 绑定钉住，**不新造机制**，操作方式按 `internal/placement` 与 runbook 现状写准确）+ attach/detach 滚动语义诚实标注 + 本地/单节点可用性说明（Docker Desktop/单节点 overlay 数据面不走 VXLAN 跨宿主，功能可验）+ 跨节点验收前置（UDP 放行后复验）。

#### E. 审查新增的交互面（实现必须接线/落回归）

1. **desired-hash 与快照**：项目网进 `ServiceSpec.Networks` ⇒ 进 `DesiredHash`（`ports.go:88-92` 全字段 canonical JSON）与快照/漂移投影（`driftProjection` 网络按别名集合，:129-136）——attach/detach 必须走重部署产出新 revision，才有哈希一致（绕过发布链路直改 spec 会与快照比对成漂移，故不取）。
2. **applyDesired 网络前置**（engine.go:911-924）：已对期望 spec 引用的全部网络做 `NetworkEnsure` 幂等确认 ⇒ 项目网在成员部署时自动确保（现有循环天然覆盖，无需新增写通路）。
3. **in-flight 部署拒绝**：attach/detach 触发重部署复用「最近 succeeded 部署」的 compose/spec_hash（`EnqueueMoveRedeploy` 同源原语）——若目标 app 有非终态部署，重部署会以**旧**快照覆盖在途发布，必须 409 拒绝（ConvergeApp 先例「wait for it to reach a terminal state」）。机具令牌/角色门与错误码沿既有（复用 `E_STATE_VERSION_CONFLICT` 409 语义，不新造码）。
4. **app 删除**：tombstone 第二拍清参与位（`MarkAppDeleted` 同步清 0——删后不参加任何项目网，防成员计数悬挂）；项目网随成员清空由 GC 回收。
5. **MoveApp 摘旧网**：`MoveApp` 编排的 `DetachAppNetwork`（ingress）只摘 app 私网，项目网不在其域；项目网由 GC 按成员计数回收（本票覆盖测试）。
6. **recon 瞬态纪律**：networks 面读错（List/Inspect 失败）不下任何结论（不事件、不 GC、不清记忆），与 services 面逐字同款。
7. **范围纪律**：本票不新增 `NetworkList` 到既有 `engine.Substrate` 端口（会波及全部测试替身），按仓库「新能力 = 新小端口 + With 注入」惯例新增 `NetworkSubstrate` 端口（ensureWithLabels/list/inspect/remove 四原语），runtime 装配 substrate.Client。
8. **已知泄漏类（本票如实披露、不扩面修）**：app 删除路径不回收 app 私网（`reapDeletingApp` 只摘服务/secret/config；traefik 亦未摘挂）——recon networks 面会把 deleted app 的 `fleetly-<team>-<prj>-<app>-net` 报为孤儿（持续形态每进程只报一次）。这是**既有真实泄漏**（非本票引入），本票的机制价值正在于让它可见；回收路径（含 traefik 摘挂）挂账后续票。

结论：全部锚点成立；参与模型无阻塞矛盾；进入实现。实现中的决策与偏离记入实施记录。

### IMPL-T15-1 实施记录（2026-09-26/27，实现会话）

**状态：实现完成，待用户验收（未 commit）。** 五条守卫 + 新增交叉语义逐条回归全绿；审查期/实施期的本地真机探针（Docker 29.7.2 / swarm active / 单节点）与全部静态门禁证据见下。

变更文件清单（每文件一句）：

- `internal/state/migrations/00025_project_network_membership.sql`（新）：`apps.project_network_attached` 加法列（默认 0）+ 扫描索引；Down 演练；迁移 golden 显式再生成。
- `internal/state/projectnetworks.go`（新）：参与位置位/清位幂等原语（审计 `app.project_network_attached`/`_detached` + 事件 `project.network_changed` 同事务 Outbox）+ 成员计数批量读面（`ProjectNetworkMemberCounts`/`ProjectNetworkMembers`，单分组查询免 N+1）。
- `internal/state/projectnetworks_test.go`（新）：置位生命周期（幂等零重复披露）、成员计数口径（deleting/deleted 不计）、项目删除非空守卫交叉、迁移 00025 Up/Down 演练。
- `internal/state/apps.go`：`App.ProjectNetworkAttached` 投影（appScanCols/scanApp）+ `MarkAppDeleted` tombstone 第二拍清参与位（无悬挂成员）；顺带 gofmt 归一一行既有注释。
- `internal/state/labels.go`：`LabelProjectNetwork`（`fleetly.project-network`，网络对象自描述归属锚；值 = 项目 ID）。
- `internal/state/testdata/migrations.golden`：00025 行显式再生成。
- `internal/naming/naming.go`：`ProjectNetworkName`（`fleetly-project-<projectID>` **全量 ID**，含 `IsProjectNetworkName` 识别谓词）/`ProjectNetworkAlias`（`<app>-<service>`）/`PlatformPrefix` 导出；包注公式表与保留字审计注释扩行。
- `internal/naming/naming_test.go`：表驱动公式行（项目网名/别名）、撞键审计（team=project 最凶形态 + 同 256ms 窗口双 ID 反证）、别名隔离契约、保留 slug 负路径扩 `project`。
- `internal/engine/ports.go`：`NetworkState` 实况投影（Labels/Driver/Containers/Services）+ `NetworkSubstrate` 独立端口（ensureWithLabels/list/inspect/remove）+ `ErrNetworkNotFound` 哨兵。
- `internal/engine/planner.go`：`PlanInput.ProjectNetwork` 与 `buildServiceSpec` 项目网双挂投影（别名 = `<app>-<service>`；投影进 desired-hash/快照 ⇒ attach/detach 由重部署滚动收敛）。
- `internal/engine/engine.go`：`netSub`/`platformNetworks` 字段与 `WithNetworkSubstrate`/`WithPlatformNetworks` 注入；tick 增 `reconcileProjectNetworks` duty；planAndRelease 传项目网投影（命名违约显式失败）。
- `internal/engine/projectnetwork.go`（新）：`EnsureProjectNetwork`（attach 前置幂等 ensure）/`EnqueueNetworkRedeploy`（参与变更重部署）/`reconNetworks`（孤儿网/缺失项目网披露，按名节流、瞬态读错不结论）/`reconcileProjectNetworks`（缺失项目网自愈 + 零成员零端点项目网回收）/期望集推导（app 网/库网/项目网/组件白名单）。
- `internal/engine/projectnetwork_test.go`（新）：守卫①②④的引擎级回归 + attach/detach 滚动 + GC 安全性 + 瞬态纪律 + MoveApp 交叉 + 漂移可见 + 回滚点时重放 + 删除泄漏披露（明细见对应表）。
- `internal/engine/substrate.go`：`substrateRecon` 同拍先跑 `reconNetworks`（读面披露与 services 面共用频控闸）。
- `internal/engine/move.go`：`EnqueueMoveRedeploy` 抽出共享核心 `enqueueRedeploy`（source/auditAction 参数化，项目网参与变更复用）；顺带 G115 钳制（`uint64(max(...))`）。
- `internal/engine/fakes_test.go`：假底座 ServiceUpdate 的模板变更模型扩网络集合（网络变化 = 任务替换，真机同构）；`fakeNetworkSubstrate`（ensure/list/inspect/remove + 孤儿/端点注入面）。
- `internal/engine/engine_test.go`：harness 装配假网络面 + 组件网白名单（生产装配同形）。
- `internal/engine/safecall_test.go`：tick duty 清单增 `reconcileProjectNetworks`（源扫描一致面）。
- `internal/substrate/networks.go`（新）：`NetworkSubstrate` 端口的 moby/client 实现（四原语 + 投影；D2 per-call 预算）。
- `internal/substrate/projectnetwork_manual_test.go`（新，默认不跑）：本地真机探针（label 往返/别名隔离双向/增摘网滚动/order-only 零替换/in-use 拒绝/零端点回收）。
- `internal/substrate/client_timeout_test.go`：D2 预算覆盖面扩四原语。
- `internal/api/projectnetwork.go`（新）：`ProjectNetworkPort` 端口 + attach/detach 组合（在途 409 → attach 前置 ensure → 状态落位 → 重部署入队；幂等重跑安全）+ AppView 项目网投影 helper。
- `internal/api/projectnetwork_test.go`（新）：全链/幂等/无部署史/在途守卫/端口未装配/404 双门矩阵/ProjectView 投影七族回归。
- `internal/api/projects.go`：`netPort` 字段与 `WithNetworkPort`；`projectView` 增 network_name/network_members（列表单查询共享计数；G115 钳制）。
- `internal/api/apps.go`：ListApps/GetApp 增 `project_id`/`project_network_attached`/`project_network` 三字段投影。
- `internal/api/scope.go`：两 RPC 登记 `ScopeAdmin`（隔离面敏感写；用户 principal 另受项目角色 admin 门）。
- `internal/apitest/apitest.go`：ProjectsService 装配确定性假编排端口（CLI 测试同路径）+ `fakeProjectNetworkPort`。
- `internal/runtime/provides.go`：`NewProjectsService` 注入 `appNetworkPort`（engine 实现）；`NewEngine` 注入 `WithNetworkSubstrate(sc)` 与 `WithPlatformNetworks(state.RustfsNetworkName, ingress.RegistryNetworkName)`（组件网白名单装配层注入，engine 不 import ingress）。
- `proto/fleetly/server/v1/apps.proto` + `genproto/…/apps.{pb.go,swagger.json}`：AppView/GetAppResponse 增三字段（加法、向后兼容）。
- `proto/fleetly/server/v1/projects.proto` + `genproto/…/projects.{pb.go,pb.gw.go,grpc.pb.go,swagger.json}`：Attach/DetachAppProjectNetwork 两 RPC（`POST|DELETE /v1/apps/{app}/project-network`）+ 请求/应答/成员投影消息 + ProjectView 网络两字段。
- `sdk/go/fleetly/client.go`：`Projects()` 访问器（CLI 消费面）。
- `cmd/fleetly/cmd/projects.go`（新）：`fleetly projects network <attach|detach|show>`（show 支持 `--json`；滚动语义文案）。
- `cmd/fleetly/cmd/projects_test.go`（新）+ `app.go` 注册 + `testdata/golden/apps_get.golden` 显式再生成：CLI 全链与用法错误（64）。
- `internal/eventcode/events.go` / `eventcode_test.go` / `testdata/events.golden`：`project.network_changed` / `network.orphaned` / `network.missing` 只增登记（85→88；docEvents 与 golden 显式再生成）。
- `console/src/api/{schema.d.ts,endpoints.ts,types.ts}`：`pnpm gen:api` 再生成（+157 行）+ attach/detach 端点封装与三类型导出。
- `console/src/components/app-project-network-card.tsx` + `.test.tsx`（新）：App 概览项目网卡（归属项目链接 + 参与徽标 + 网络名 + attach/detach + detach 确认对话框 + 角色/平台管理员只读说明）；4 测。
- `console/src/pages/AppOverviewPage.tsx`：接入项目网卡（Application 卡后）。
- `console/src/pages/ProjectDetailPage.tsx` + `.test.tsx`：信息卡增项目网行（overlay 名 + 成员数）、应用表增参与徽标列；测试夹具与断言随行。
- `docs/runbooks/project-networks.md`（新）：项目网运维手册（语义/操作/滚动语义/UDP 未放行期同节点 placement 指引/对账披露/回收/真机探针原始观测/已知边界）。
- `docs/plan/2026-09-26-torchwood-line-impl.md`：审查小节（已前置写入）+ 本实施记录。

测试清单与票面五条守卫 + 新增交叉语义逐条对应表：

| 守卫/条款 | 回归测试 |
|---|---|
| ① 跨 app DNS（exec 内 `ping <app>-<service>` 通） | 真机探针 `TestManualProjectNetwork`（app2 → `app1-web` exit=0，10.0.1.2；反向对称通）；spec 级断言 `TestProjectNetworkAttachmentProjectsMembersServices`（双挂：app 网 + 项目网，别名逐字） |
| ② **孤儿网注入一个对账周期内暴露**（机制验收） | `TestProjectNetworkReconDisclosesInjectedOrphanWithinOneScan`（注入 fleetly- 前缀受管网 → force recon → `network.orphaned` 事件 + `reconcile.network_orphaned` 审计各 1；组件网白名单与项目网 label 对象零误报；持续形态节流；消失清零后再注入可再报）；`TestNetworkReconSubstrateReadErrorDisclosesNothing`（list 瞬态错误零事件/零动作，恢复后照常检出） |
| ③ project 删除非空拒绝（既有语义不回退 + 网络面交叉） | `TestProjectDeleteGuardWithAttachmentCross`（attach 不改变拒绝；tombstone 两拍后项目可删、参与位已清） |
| ④ 短名跨 app 不混流（别名隔离断言） | spec 级：`TestProjectNetworkAttachmentProjectsMembersServices`（项目网别名恰 `[demo-web]`，app 网别名恰 `[web]`）；真机：`TestManualProjectNetwork`（`ping only1` 自 app2 失败「bad address」、反向失败）；命名契约 `TestProjectNetworkAliasIsolationContract` |
| ⑤ naming 契约表驱动测试不破（三段公式/保留字/公式表） | `TestNamesMatchDesignDocs`（项目网/别名两新行 + 既有行零改）、`TestProjectNetworkNameCannotCollideWithAppNetwork`（team=project 最凶形态 + 同窗口双 ID 反证 + 近名负路径）、`TestReservedTeamSlugs`（`project` 不入保留清单）；OT-1「app 保持全局唯一」旧文与现行三段契约的解释见审查节与偏离 8 |
| attach/detach 滚动语义（真机实证 + 假底座同构） | 真机 `TestManualProjectNetwork`（增/摘网任务重建、start-first 并存窗口 `running/desired=shutdown`、order-only 零替换）；引擎 `TestAttachDetachRollMemberTasks`（重部署后任务 ID 替换） |
| 成员回收 GC（成员清空/项目删除） | `TestReconcileProjectNetworksReclaimsDetachedEmptyNetwork`（有成员保留 / 端点未排空保留 / 零成员零端点回收恰一次）；真机 in-use 拒绝 + 清场后回收成功；`TestMoveAppPreservesProjectNetworkParticipation`（改派后旧项目网回收） |
| 缺失项目网（state→swarm）披露 + 自愈 | `TestProjectNetworkReconDisclosesMissingMemberNetwork`（一周期暴露 + 节流 + recon 不自动建 + 收敛 duty 重 ensure） |
| MoveApp 交叉语义（参与位保留、投影随当前项目） | `TestMoveAppPreservesProjectNetworkParticipation`（新命名上下文双挂新项目网、别名不变、旧网回收） |
| 漂移投影补网络面（外部篡改可见） | `TestDriftDetectsProjectNetworkDetach`（平台形态零漂移；外部摘网 → `networks` 字段 diff） |
| app 删除交叉（清位 + 泄漏可见） | 清位：`TestProjectNetworkMemberCounts`（MarkAppDeleted 后 flag=0）；泄漏披露：`TestNetworkReconDisclosesDeletedAppNetworkLeak`（active 网零误报；deleted 网一事件——既有泄漏可见化，回收挂账） |
| 回滚点时重放边界（网络面随快照） | `TestRollbackReplaysSnapshotNetworkFace`（回滚到 attach 过的 revision → 项目网重放挂回；参与位不被回滚翻转；下次发布收敛） |
| API 面（组合/幂等/双门/守卫） | `TestProjectNetworkAttachDetachFlow`（ensure 前置 + 状态 + 审计/事件 + rolling + GetApp 投影 + 幂等重跑 + detach 对称）、`TestProjectNetworkNoRedeployHistoryIsAccepted`、`TestProjectNetworkAttachInFlightGuard`（409 + 端口零调用 + 状态零变更）、`TestProjectNetworkScopeAndRoleGates`（登记 admin；read 拒 / developer 拒 / 平台管理员拒 / 团队 owner 放行）、`TestProjectNetworkPortNotAssembledAndNotFound`（Unavailable/404）、`TestProjectNetworkProjectViewProjection`（Get/List 网络面） |
| CLI / Console / SDK | `TestProjectsNetworkSurface`（show 缺省 detached → attach → show/--json → detach → 用法错误 64）；`app-project-network-card.test.tsx`（detached+admin attach POST / attached+admin detach 确认 DELETE / developer 角色说明 / 平台管理员只读，4 测）；`ProjectDetailPage.test.tsx`（网络行 + 应用参与徽标，既有 10 测不回退）；SDK `Projects()` 访问器随全量 sdk 测试 |
| 迁移/事件只增纪律 | `TestAppProjectNetworkAttachedLifecycle`（审计/事件词表与幂等）、`TestProjectNetworkMembershipMigrationUpDown`（00025 Up/Down + 默认 0）、migrations.golden 显式再生成；eventcode `TestDocEventSetMatchesRegistry`/`TestGoldenSnapshot`/`TestRegistryEventsReferencedInProduction`（三新事件有生产发出来源） |
| D2 超时闭环 | `TestNonStreamingCallDeadlineBound` 扩 `NetworkEnsureWithLabels`/`NetworkList`/`NetworkInspect`/`NetworkRemove` 四例 |

一手验证证据（本会话复验）：

- 全量 `go test ./... -count=1` 全绿（32 包 ok，含新测 20 个 + 改写若干）。
- 变更包 `go test -race -count=1` 全绿：engine / api / state / substrate / naming / eventcode / cmd/fleetly/cmd / apitest / runtime。
- `go test ./sdk/...` 绿；`go vet ./...` 净（exit 0）。
- `golangci-lint run --new --whole-files`：新代码零新增问题；仅余两处**未触行**存量（`internal/eventcode/eventcode_test.go:14` G101（`docEvents` 声明行，改动前同位置同报）、`internal/runtime/provides.go:1005` G402（gateway 回拨 TLS 形态，T1-4 记录同源）——逐条核对 diff hunk 不在本票改动内；本票顺带修复了的 `state/apps.go` 一处既有 doc 注释 gofmt 与 `engine/move.go` 一处 G115 钳制（均触文件归一）。
- `sh deploy/check-image-pins.sh`：`OK — 28 image reference(s) digest-pinned, 0 exempt`（本票零新增镜像引用）。
- proto/生成物幂等：`mise run generate:proto`（buf lint + generate）二次运行前后六文件 sha256 一致；`mise run generate:wire` 零漂移（`internal/runtime/wire_gen.go` 未变）；`mise run console:gen-api` 二次运行 `schema.d.ts` 哈希一致。
- Console 四脚本：`pnpm test` **332 全绿**（55 文件；基线 328 + 新卡 4 测 + ProjectDetail 既有 10 测扩断言）、`pnpm typecheck` / `pnpm lint` / `pnpm build` 全净。
- **本地真机探针（一手；Docker 29.7.2 / swarm active / 单节点）**：`FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualProjectNetwork -v`（2026-09-27，PASS，51.5s）原始观测：
  - 项目网 ensure：`fleetly-project-01JMANUALPROJECTNET0000000`，labels `map[fleetly.managed:true fleetly.project-network:01JMANUALPROJECTNET0000000]`，driver=overlay；二次 ensure 幂等。
  - **守卫①**：app2 → `ping app1-web` exit=0（10.0.1.2，56 bytes/0.243ms）；反向 app1 → `app2-web` 通。
  - **守卫④**：app2 → `ping only1`（app1 私网短别名）exit=1 `ping: bad address 'only1'`；反向同。
  - 滚动：`--network-add` 等价 update → 任务 `mz0x…` → `rfax…`（旧任务 `running/desired=shutdown`——start-first 并存窗口）；摘网 → 再滚动；order-only update → `running/desired=running`（零替换）。
  - GC 安全：inspect 端点 `containers=4 services=0`；`NetworkRemove`（在役）被拒 `FailedPrecondition: network … is in use by service …`；服务清场 + 端点排空后移除成功。
  - 探针期附加观测：`network inspect` 的 `Services` 字段在本机受管 overlay 上恒 0（未填充）——GC 的零引用判据实际由 Containers + daemon in-use 拒绝双层承载（runbook 与端口注释已如实标注）。
  - 审查期已跑的同族探针（`docker server update` 增/摘网）结论与 runbook 滚动语义表一致。

偏离清单（实现中的决策与修正，均按纪律登记）：

1. **项目网名 = 全量项目 ID（`fleetly-project-<projectID>`），非票面建议的 `<id8>`**：审查记录 D.3 原文已载「实现期修正」——测试首轮即实证两个夹具项目 ULID 前 8 位撞名（前 8 只承载 40 位时间戳成分、256ms 窗口同值），且截断会把第二个项目的项目网 ensure 静默并入第一个网络（`NetworkEnsure` 已存在即成功）= 网络静默合并的安全级缺陷。全量 ID 结构性唯一（主键）、42 字符网络名无解析面约束、grep 友好。
2. **attach/detach 生效腿 = 发布管线重部署（非绕过发布链路直改 service spec）**：新 revision 的规划投影按当前参与位重建（快照/desired-hash/漂移三面自动一致）；直改 spec 会与快照比对成漂移且需第二写通道。语义代价如实披露：在途部署 **409 拒绝**（重部署以最近 succeeded 为基，不得覆盖在途发布）；无成功部署史 = 仅状态落位（status=attached/detached），有史 = status=rolling。
3. **「派生修正」的定义（票面授权裁决项，审查记录已冻结）**：缺失项目网 = 披露 + 收敛 duty 幂等重 ensure（平台自建对象，恢复不涉他人物件）；无法归因孤儿 = **只披露不删**（不静默删他人物件）；唯一回收例外 = 带自描述 label 且零成员零端点的项目网（归属明确的本平台对象）。回滚的点时重放（网络面随快照）登记为已知边界（与 env 快照同心智，runbook 载明）。
4. **项目网参与权限 = admin scope + 项目角色 admin 门**：网络姿态改变是隔离面敏感写（把成员暴露给项目内其他 app 的可达集），与 app 删除/secrets 同级；平台管理员只读不代写、机具令牌 admin 等价照旧。CLI attach/detach 因此需要 admin 层凭据（README 未列动词清单，无需连带更新）。
5. **新独立端口 `NetworkSubstrate`**（不扩既有 `Substrate` 接口）：四原语（ensureWithLabels/list/inspect/remove）单独 With 注入，既有测试替身零波及；实现 = substrate.Client。
6. **GC 的双层安全判据**：零成员 + 零挂接容器才收；in-use 由 daemon `FailedPrecondition` 拒绝兜底（真机实证）；`Services` 字段未填充的实测注记见证据节。
7. **recon 披露节流与瞬态纪律**：孤儿/缺失按网络名进程内 seen 记忆（持续只报一次、恢复清零可再报、重启重报一次——substrateMissingSeen 同款）；读错（List/Inspect）不结论、不动作。
8. **OT-1「app 保持全局唯一」旧文的口径**（票面守卫⑤要求解释）：该字样是 RBAC D-W0-4 二修前的旧文；现行契约 = 三段命名 `fleetly-<team>-<prj>-<app>-*` + app 名 **project 内唯一**（`UNIQUE(project_id,name)`）。本票**零改动**命名公式主体（新增对象族只加行），表驱动测试全量不回退。
9. **CLI 形态**：`fleetly projects network <attach|detach|show>`（嵌套子命令组沿 alerts/notifications 惯例；attach/detach 幂等重跑安全、show 支持 `--json`）；SDK 补 `Projects()` 访问器（此前 CLI 无项目动词面）。
10. **Console 最小面**：app 归属 + 参与状态与 attach/detach 控件收在 App 概览「Project network」卡（admin 角色；平台管理员只读说明原位渲染）；项目详情信息卡显示网络名 + 成员数、应用列增参与徽标；项目列表页不加网络列（避免列表 N+1 查询；审查记录已声明最小面取舍）。
11. **app_get golden 显式再生成**（`project_id` 加法字段进 `--json` 输出——只增契约变更，按 CLI golden 纪律 `-update` 再生成）。
12. **已知泄漏类如实披露不扩面修**：app 删除路径不回收 app 私网（含 Traefik 摘挂缺失）——recon 会披露（`TestNetworkReconDisclosesDeletedAppNetworkLeak` 钉住机制）；回收路径挂账后续票（审查记录 E.8 已登记）。
13. **别名段内歧义挂账**：项目网别名 `<app>-<service>` 是跨两层拼接——同项目 app「a」+服务「b-c」与 app「a-b」+服务「c」都 attach 时别名 `a-b-c` 撞名（swarm DNS 同时返回两地址）。与 rbac-teams §4.3 记载的服务名段内歧义同族（罕见命名组合、需两个特定命名同时 attach），本票照该先例**如实记录 + admission 预检挂账**（不新造检查机制）；runbook 已知边界载明。

**验收追认（2026-09-27）**：用户于提交前指示「提交，然后下一项」验收，追认七项裁决：①项目网名全量 ID 修正（票面 `<id8>` 弃用）；②网络参与 = 显式 opt-in（缺省不参加）；③attach/detach 生效腿 = 发布管线重部署（在途 409）；④参与权限 = admin scope + 项目角色 admin；⑤对账口径（孤儿只披露 / 缺失自愈 / 回收仅自描述项目网零成员零端点）；⑥app 私网泄漏披露挂账（本票只可见化）；⑦别名段内歧义挂账。T1-5/T2-4 割接票按「显式 attach 后才有项目内网可达」书写。

staging/真机待执行项（本环境无 staging 访问权，未虚构结果；本票已做单节点真机探针）：

- **跨节点项目网数据面**：依赖 VPC/云防火墙放行 UDP 4789/7946（W3-F2 未放行）；放行前成员服务按 runbook 钉同节点 placement。放行后复验：两节点各一成员 app，互相 `ping <app>-<service>` 通、短名不通、attach/detach 跨节点滚动。
- 多节点 overlay 上 `network inspect` 的 `Containers`/`Services` 字段分布复核（GC 判据在多节点形态的实测）。
- Console 网络卡 staging 走查（attach → 重部署滚动 → 徽标翻转；平台管理员/developer 只读态）。
- 项目网 + E4 库网/rustfs 牵线多网络叠加形态的真机复核（本票单节点探针为两网叠加，库网/rustfs 组合沿既有投影测试链）。
- attach/detach 与域名路由收敛的交互走查（网络切换期间 Traefik 路由不依赖项目网——设计上入口只挂 app 网，无需改动；staging 一次真机确认）。

### IMPL-DB-1 方案可行性审查（2026-09-27，实现会话）

**结论：不通过——阻塞前置矛盾，停止实现，等人工裁决。本票未动任何代码/生成物（唯一落盘 = 本节）。**

一句话：**percona 镜像的兼容性前提成立**（env/entrypoint/PGDATA/凭据投递/pgvector 逐项实证通过），但**平台自身的 dbtools 工具面仍是 PG 16.15 单一大版本**——PG 18 实例的 backup 与 restore 都在 job 首步硬失败（一手复现：`pg_dump` 16 对 18 服务器 `aborting because of server version mismatch`；dbtools 内 postgres 16 起在 PG 18 数据目录 `database files are incompatible with server`）。DT-9 的验收「每个目录条目过 create→backup→restore」在本票范围内**结构性不可达**：修复面（新 dbtools 镜像多 PG 大版本工具面 + 按模板 major 选工具 + 恢复脚本路径参数化）需要 CI 发布私有镜像新 digest，超出票面「目录化只动镜像与默认参数层」。另有一处票面字面与实证相左：percona 的 `VolumeMountPath` **不可**与 postgres-16 同款（uid 26 + 空卷 root-owned → `mkdir: Permission denied`），须按条目携带原生路径（见必查项 1.4）。

现状锚点核实（票内 file:line 逐条；全部成立，语义与票面一致）：

- `internal/dbtemplate/dbtemplate.go`：镜像常量（现行 :22-35，四个）、模板 ID 词表（:38-47）、`ErrUnknownTemplate`（:49-51）、`Template` 结构（:85-108）✓。`render.go` 的**替换轴**在 ID：`renderEnv`（:145-180）与 `ConnectionVars`（:240-261）均 `switch tpl.ID`——目录化要换成 engine 轴。
- `internal/database/adapters.go`：`backupFilename`（:87-100）、`backupJobScript`（:130-161）、`verifyJobScript`（:170-202）、`Restore`（:359-406）、`RotateCredential`（:532-557）全部按 `dbtemplate.TemplateXxx` 常量 switch；`restorePostgresJobScript` **硬编码** `PGDATA=/var/lib/postgresql/data/pgdata`（:240）——percona 路径适配的必改点（`RestoreInput.VolumeTarget` 已承载模板挂载点，:104-115）。
- `internal/api/databases.go:106-112`：未知模板 → `E_DB_TEMPLATE_UNSUPPORTED` 400 + `templateIDList()`（由 `dbtemplate.List()` 派生，词表自动扩；:792-799）✓；引擎侧防御映射在 `internal/engine/dbinject.go:229-235`（E_RUNTIME_UNAVAILABLE，注释声明理论不可达）✓；既有回归 `internal/api/databases_test.go:122-124`（未知模板 400）✓。
- E4 矩阵现行组织（运行入口/环境/覆盖点）：`e2e/databases.sh` 单 dind 自足脚本（现行 **1252 行**；票面写 1181 行——撰写后 W4-S4 追加了 M/G 腿，语义覆盖与票面描述一致）；D 腿 = postgres-16 全生命周期（D1-D27：建库/卷放置/rustfs 目标/引用 app 注入/备份/清空/恢复/轮换/密钥库/暂停/删除留卷）、M 腿 = mysql-8.4（M1-M20）、G 腿 = mongodb-8.0（G1-G15）；断言行 97 处（成败成对）。CI 入口 = `.github/workflows/nightly.yml` `databases-e2e` job 直接 `sh e2e/databases.sh`（:505-506），dbtools 私有包在 dind 内登录直拉（:497-499）；本地复跑 = 同一脚本（W4 记录 dind 27/27，v0.3 记录 62/62）。E4 真机面 = v0.2 W4「staging 35/35」（runbook §9）。
- Console：`create-database-dialog.tsx:32-37` 硬编码四模板、`:53` 缺省 postgres-16、testid `database-template-select`（:136）；`DatabasesPage.tsx:223` 空态 hint 例句；`DatabaseDetailPage` 的 template 展示面（测试夹具 `.test.tsx:21`）✓。

必查项取证：

1. **percona 与官方 postgres:18 兼容性（本地 Docker 29.7.2 + Docker Hub API + 运行实证，2026-09-27）**：
   1.1 真实仓库路径 = `percona/percona-distribution-postgresql:18`（设计字面 `percona-distribution-postgresql:18` 缺 org 前缀；Docker Hub tags 670 个，`:18` / `:18.6` / `:18-ubi8` / `:18-ubi10` 均在；`:18` 解析 index digest `sha256:dae47360e8137cafc1e8d66f9a1be348f1405e3cf51daa383b94e6c277e6b256`，内含 amd64 `d8741ab9…c904` + arm64 `f4136fb0…a7c1`；镜像 `FULL_PERCONA_VERSION=18.6-1.el9`，`User=26`）。
   1.2 入口 = docker-library postgres 入口的 fork（`/entrypoint.sh` 源码逐段核对）：`file_env` 支持 `POSTGRES_PASSWORD_FILE`/`POSTGRES_USER_FILE`/`POSTGRES_DB_FILE`；`docker-entrypoint-initdb.d` 处理逻辑与官方同形；`gosu` 在位；`PGDATA` 缺省 `/data/db` 但**显式设置时被尊重**（`if [[ -z "$PGDATA" ]]; then export PGDATA=/data/db; fi`）；`pg_isready`/`pg_dump`/`pg_restore`/`psql`/`initdb` 在 `/usr/pgsql-18/bin`（PATH 已含）。
   1.3 运行实证（swarm service + 平台投递形态 `File{Name:"/run/secrets/…", UID 0, GID 0, Mode 0444}`）：官方 `postgres:18` 与 percona 均 `running`，`pg_isready -U fleetly` 应答（健康门命令可执行），`POSTGRES_PASSWORD_FILE` 指向的 Swarm secret 密码**可用**（TCP 登录 `select current_user` = fleetly；错密码被拒 `password authentication failed`）；percona 的 `docker-entrypoint-initdb.d` 经 swarm config 挂载实测执行（探针表值 42 在库）；`SELECT pg_available_extensions` → **`vector 0.8.6`**，`CREATE EXTENSION vector` → 0.8.6 ✓（「含 pgvector」成立）。官方镜像同法走平台约定（`PGDATA=/var/lib/postgresql/data/pgdata` + 挂载 `/var/lib/postgresql/data`）工作正常（PG 18.6；卷内 `pgdata/` 子目录；镜像声明 `VOLUME /var/lib/postgresql`，匿名卷无害）。
   1.4 **票面偏差（实证）**：percona 用 postgres-16 同款挂载路径直接失败——`mkdir: cannot create directory '/var/lib/postgresql/data/pgdata': Permission denied`（进程 uid 26，卷根 root-owned；percona 预启动无 root 分支，不做 chown）。percona 原生路径成立且可沿用子目录约定：挂 `/data/db` + `PGDATA=/data/db/pgdata` → ready、卷内 `pgdata/`。⇒ volume 路径/PGDATA 必须**按条目**（descriptor 字段）承载；恢复脚本的硬编码 PGDATA 同步参数化。
   1.5 健康门/凭据/线协议面：percona 与官方同为 PG 18 wire protocol；`pg_isready`/`psql` 工具齐备；平台 `CredentialSecretFile`（`POSTGRES_PASSWORD_FILE=/run/secrets/password`）语义两镜像同构——**「发行版不新增适配器」在 18 系内部成立**（前提是工具面问题先解决）。

2. **两个新镜像的 digest 与台账/门禁流程**：
   2.1 digest 取得（多架构 index）：`postgres:18` → `sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722`（amd64 `0377e72c…`、arm64/v8 `f4fcd2b9…`；PG 18.6 debian trixie）；`percona/percona-distribution-postgresql:18` → `sha256:dae47360…`（见 1.1）。amd64 实拉 `RepoDigests` 与上述 index digest 逐字一致；arm64 存在性经 registry 侧 `docker buildx imagetools inspect` 逐平台 manifest 核对（本地 classic image store 对同 tag 换平台报 `cannot overwrite digest`——这是本地存储限制，非远端缺失，如实记录；CI/dind 与本机 containerd store 可复跑 W4「按 digest 以 arm64 平台独立拉取」形态）。
   2.2 台账：#19/#20 是 mysql/mongo 行，**现行台账末行已是 #21**（vmalert）——新行顺延为 **#22/#23**；登记位置与「Go 常量字面 + 台账行双锚」口径照 #19/#20 行。
   2.3 `deploy/check-image-pins.sh` 扫描范围 = `deploy/*.sh`、`deploy/Dockerfile*`、`deploy/testdata/*/Dockerfile*`、`.github/workflows/*.yml`（脚本头 :21-26）；**Go 常量（`internal/dbtemplate`）与 `e2e/**` 不在扫描口径内**——兜底机制 = 台账双锚（runbook 各行「Go 常量字面……改动须同步」），不得虚构门禁覆盖。现状实跑 `OK — 28 image reference(s) digest-pinned, 0 exempt`；负路径自证（临时文件两条未钉引用）rc=1 并逐条点名 file:line；正路径 rc=0。若实现：新常量 + 台账两行 + e2e 新镜像常量（三锚）；若未来 dbtools 扩 PG18 工具面，其 `FROM` 新镜像会进扫描口径（那是工具面票的门禁面）。

3. **dbtools 工具面跨大版本（本审查的阻塞证据，一手）**：
   3.1 现行 dbtools（`ghcr.io/fleetlyrun/dbtools:v0.3.0-dbtools.1@sha256:2b9288a9…bd96`，匿名可拉）自报 `pg_dump/postgres/pg_restore = 16.15 (Debian)`——基底 = `postgres:16`（`deploy/Dockerfile.dbtools:81`），与 `DefaultPostgresImage` 同一 digest，恢复单 job 的 glibc 同源前提即建立在此。
   3.2 **backup 腿**（平台命令词表原样）：`pg_dump -h <instance> -U fleetly -d <db> -Fc` → 对 percona 18 与官方 18 均 `pg_dump: error: aborting because of server version mismatch`（`detail: server version: 18.6 …; pg_dump version: 16.15`）。pg_dump 拒对更高大版本服务器导出（硬约束，非告警）。
   3.3 **restore 腿**（脚本原样降权后起临时服务器）：dbtools 内 `gosu <卷属主 uid> postgres -D <datadir>` 于 PG 18 数据目录 → `FATAL: database files are incompatible with server`（`detail: The data directory was initialized by PostgreSQL version 18, which is not compatible with this version 16.15`）；官方 18 卷与 percona 18 卷同结论（percona 需先按 1.4 修正路径，修正后同结论）。另：PG16 客户端读 PG18 的 `postgresql.conf` 先撞 `unrecognized configuration parameter "autovacuum_worker_slots"`——即使绕开配置解析，数据目录版本检查仍是硬失败。
   3.4 结论：**PG 18 模板的 backup/verify/restore 全链在当前平台工具面下不可用**；DT-9 验收在当前仓库状态下不可达。修复需要新工具面设计（下述待裁决选项）。

4. **目录化最小设计（供裁决后直接开工的边界，非实现）**：
   - descriptor 增 engine 通用身份字段：`Engine`（词表 `postgres|redis|mysql|mongo`）+ `Distribution`（`vanilla|percona`，既有四模板留空/vanilla）+ `Major`（int）；`Template` 其余字段与既有四个条目的 `ID/Image/字段值`**逐字不变**（golden/spec 测试零回归）。分发轴替换点 = `render.go` 两处 `switch tpl.ID` + `adapters.go` 五处 `switch`（改按 `tpl.Engine` 分派；percona/vanilla 同属 `postgres` 家族）。
   - 入目录条目 = 既有四 + `postgres-18`（vanilla）+ `percona-postgresql-18`（Distribution=percona；`VolumeMountPath=/data/db`、PGDATA 子目录、`EnginePort 5432`、`CredentialSecretFile`、`pg_isready` 健康门、1C/1Gi 缺省限额——除 volume 路径外与 postgres-16 同款）；redis/mysql/mongo 目录结构预留、不实现新条目。
   - 适配器仍单实现（PG 家族一份）；两个必改参数化：(a) 恢复脚本 PGDATA 取 `VolumeTarget`+子目录（去硬编码）；(b) 工具面按 `Major` 选 pg_dump/pg_restore/postgres 二进制（阻塞解除后的工具面票交付物）。
   - 大版本升级不做（落点：dbtemplate 包注 + `proto/fleetly/server/v1/database.proto` 模板注释 + managed-databases §2.2/§7 一行；不新造机制）。
   - CreateDatabase 校验/错误码零变化（`E_DB_TEMPLATE_UNSUPPORTED` 词表经 `List()` 自动扩；注意 `internal/errcode/testdata/codes.golden:28` 的 suggestion 文本列了四个模板 ID——词表增须显式再生成该 golden）；Console `TEMPLATES` 增两行 + 测试（`database-template-select` 锚点不破）；e2e 增设参数化 P 段（每新条目 create→backup→restore，D/M/G 不回归）。

**待人工裁决（阻塞解除路径，按代价升序）**：

- **A（建议）**：先立「dbtools 多 PG 大版本工具面」前置票——扩展 `deploy/Dockerfile.dbtools`（PG18 工具链与 16 并存，二进制目录/LD_LIBRARY_PATH 按 major 选择；恢复临时实例须与实例数据目录同 major）+ job 脚本按模板 major 选工具 + 新 digest 经 CI 发布（私有 ghcr）+ 台账 #22/#23；发布后 DB-1 恢复全量实现并跑通矩阵。本票阻塞点即此票的验收面。
- **B**：DB-1 缩面为「创建/健康门可用，backup/restore 对 18 系显式拒绝并点名工具面缺口」——与 DT-9 验收字面冲突（矩阵钉不死「发行版不新增适配器」），且 T2-4 割接的托管备份前提落空；不建议。
- **C**：本票只做目录化机制重构（既有四模板零变化），两新条目挂账到工具面就绪——不满足 DT-9 的用户可见目标（torchwood 现役 percona 发行版落位顺延）。
- **D**：只加 vanilla `postgres-18` 不加 percona——同样被工具面阻塞（3.2/3.3 对官方镜像同样成立），不成立。

**裁决落定（2026-09-27，用户选定 A）**：新增 `IMPL-DB-0` 前置票（dbtools 多 PG 大版本工具面；票文见 §2、prompt 见分发档），DB-1 依赖改为 DB-0 后行（本票实装面相应收敛：两新条目 + Console + 文档 + 矩阵；身份字段与分派轴迁移、工具选择、PGDATA 参数化移入 DB-0）。

**未执行项（停手纪律）**：本票未改任何 Go/Console/proto/脚本/台账文件，未跑 `go test`/console 四脚本/e2e 矩阵（零改动，无回归面）；上述全部结论均有本节记录的一手观测支撑，无推断性「已通过」。

### IMPL-DB-0 方案可行性审查（2026-09-27，实现会话）

**结论：通过（机制裁决 = 单镜像双工具面：`postgres:16` 基底 + `postgres:18` 工具链版本分区 COPY + 缺失 soname 补集；job 工具按实例 major 以显式绝对路径选择）。无阻塞前置矛盾，进入实现。** 两处与票面字面的偏离（无新镜像常量；门禁盲区修复）见文末「审查期裁决与偏离」。

现状锚点核实（票内 file:line 逐条）：

- `deploy/Dockerfile.dbtools:81`（`FROM postgres:16@sha256:a3b7…`）✓；工具面实证修正一处票面记忆：官方镜像为 **pgdg Debian 布局**——二进制在 `/usr/lib/postgresql/16/bin`（`/usr/bin/pg_dump` 等是 postgresql-common `pg_wrapper` 的符号链接）、share 在 `/usr/share/postgresql/16`、pkglib 在 `/usr/lib/postgresql/16/lib`，**不在 `/usr/local/bin`**；`pg_dump/postgres/pg_restore = 16.15 (Debian)` ✓。
- `internal/database/adapters.go:46`（`DefaultDatabaseToolsImage`）✓ 现行 46 行；五处 ID switch（`backupFilename`/`backupJobScript`/`verifyJobScript`/`Restore`/`RotateCredential`）✓；**第六处** `internal/database/rotate.go:140`（`rotateCredential`）同轴，一并迁移（票面未列，语义同类）。
- `restorePostgresJobScript` 硬编码 `PGDATA=/var/lib/postgresql/data/pgdata` ✓（现行 :240）；`RestoreInput.VolumeTarget` 已承载模板挂载点 ✓。
- `.github/workflows/dbtools.yml` 发布链（单镜像 build-push + cosign sign/verify + 飞书；`release.yml` 以 `workflow_call` 同版调用）✓。
- `gh auth status` = `qiulin@github.com`，scopes 含 `workflow` ✓；`git ls-remote origin` 仅 `refs/heads/main`，本票改动未推送（`不 commit/push` 约束下 dispatch 新内容的可行性见实施记录）。

机制必查项实证（本机 Docker 29.7.2；探针镜像均为临时产物，非仓库文件）：

**1. 单镜像双工具面（选定）——官方镜像的 Debian 布局本就按 major 分区，无 share/prefix 冲突面**

- 布局实测：两代镜像的 `pg_config --bindir/--sharedir/--pkglibdir` = `/usr/lib/postgresql/<major>/bin`、`/usr/share/postgresql/<major>`、`/usr/lib/postgresql/<major>/lib`（pgdg 编译期绝对路径，带 major 段）。票面担心的「share 目录/编译期 prefix 冲突」**不存在**：无需 relocation，两代目录天然并存。
- 候选镜像（`postgres:16` 基底 + `COPY --from=postgres:18` 两目录 + 两个缺失 soname）实测：
  - PG18 全部二进制 `ldd` 零 `not found`（缺口仅 `libnuma.so.1`/`liburing.so.2`——基底 `ls` 实测缺；`libpq.so.5` 两镜像逐字节同源 `sha256:9cce9bfa…`）；
  - `initdb` + 临时服务器在 PG18 数据目录起动成功、`psql`/`pg_dump`/`pg_restore --list` 全可用；
  - PG18 二进制对 PG16 数据目录 `FATAL: database files are incompatible with server`（major 硬约束复现）。
- **pg_wrapper 陷阱（关键实证）**：两代并存后，裸名 `pg_dump/psql/pg_restore/pg_isready` 经 `/usr/bin` → `pg_wrapper` 解析为**最新版 18**（wrapper 尾部「if we have no version yet, use the latest version」；psql/pg_isready 恒取最新）；裸名 `postgres/pg_ctl` 仍走 PATH 尾的 16 目录（不在 `/usr/bin`）。⇒ 工具选择必须显式：PG job 脚本一律用 `/usr/lib/postgresql/<major>/bin/<tool>` 绝对路径。PG16 侧该路径**即今日 pg_wrapper 的解析结果**（同一二进制；wrapper 在显式 `-h` 下不注入 cluster 缺省，脚本恒带 `-h`）——行为逐字不变。
- 体积实测（`docker inspect .Size`，amd64 未压缩）：现行 dbtools `1,084,149,767` B → 候选单镜像 `1,132,959,955` B（**+48.8MB / +4.5%**）。

**2. 双镜像（否定项）——同构第二镜像实测 1.089GB**

- 以 `postgres:18` 为基底、同构 COPY 全工具面（其余四个 stage 不变）的候选实测 `1,089,486,184` B；两镜像合计 ~2.22GB vs 单镜像 1.13GB。
- 结构性代价：`deploy/Dockerfile.dbtools` 双份维护（或新增文件+同步纪律）、双 tag/digest/cosign/台账行/e2e 锚、每 release 双发；**无能力增益**（单镜像已实证满足全部所需语义）。裁决：否定。

**3. 跨版本工具语义（一手原始输出）**

- `pg_dump` 必须 ≥ 服务器 major：PG18 客户端导 PG16 服务器 `rc=0`（成功）；PG16 客户端导 PG18 服务器 `pg_dump: error: aborting because of server version mismatch`（复现 DB-1 审查）。
- `pg_restore` 必须 ≥ dump 产出 major：PG18 产 custom 归档 `Dump Version: 1.16-0`，PG16 `pg_restore --list` 报 `pg_restore: error: unsupported version (1.16) in file header`（rc=1）；PG18 `pg_restore --list` 同归档 rc=0（18 条 TOC）。
- `postgres`（临时恢复实例）必须 = 数据目录 major（双向：18 起 16 目录 `database files are incompatible`；16 起 18 目录同）。
- ⇒ **无「可共版」工具**：pg_dump/pg_restore/postgres 及消费链 psql/pg_isready/pg_ctl 全部随实例数据目录 major 选取（psql/pg_isready 线协议虽兼容，同 major 选取零成本且免混版面）。

**4. 恢复临时实例启动形态在所选机制下逐项满足（候选镜像内实测）**

- gosu 降权（基底自带 `/usr/local/bin/gosu`）✓；PGDATA 由 `RestoreInput.VolumeTarget + "/pgdata"` 参数化后 **PG16 现值逐字不变**（`/var/lib/postgresql/data/pgdata`）、PG18/percona 形态可承载 ✓；share 可达（绝对路径随 COPY 落位，initdb/起动实测）✓；socket `/var/run/postgresql`（pgdg 编译期缺省；**percona 数据目录实测**：conf 内该行是注释样例、percona 引擎的 `/run/postgresql, /tmp` 来自入口旗标，vanilla 临时服务器 `-C` 解析为 `/var/run/postgresql`——恢复脚本 socket 参数**无需**改）✓；端口 5432 缺省 ✓。
- **新发现（DB-1 交接项，非本票阻塞）**：vanilla PG18 工具链对 **percona 数据目录的 pgvector 重放不可用**——percona 的 `vector.so/vector.control` 在 `/usr/pgsql-18`（不在 vanilla share/pkglib）。原地重放实测 `pg_restore rc=1`：`ERROR: extension "vector" is not available` → 依赖表 `CREATE TABLE ... public.vector(3)` 与 `COPY` 连锁失败（重放后空库）。本票机制（major 面）不解分布差异；DB-1 需按发行版决策（建议：dbtools PG18 面补 percona vector 控制/库文件，或恢复临时实例改用引擎镜像）。

**5. check-image-pins 扫描行为（一手，正/负路径）**

- 扫描域 `deploy/Dockerfile*` 覆盖新增文件/新增 FROM ✓（通配）。
- **实测盲区（本票新增引用形态正好命中）**：纯数字 tag 被噪声过滤整条丢弃——`FROM postgres:16@sha256:…`、`FROM redis:7@sha256:…` 现行列表模式**零计数**；未钉的 `FROM postgres:18` 亦不报错（rc=0）。`COPY --from=<外部镜像>` 行不在候选上下文（脚本头注已知盲区）。
- 处置：①Dockerfile 新增 PG18 引用以 **FROM stage 形态**声明（`FROM postgres:18@sha256:… AS postgres-engine-18`），引用落进扫描口径；②**修门禁**：FROM 行不再套用「tag 非纯数字」噪声过滤（FROM 上下文不可能是时刻/端口噪声），负路径自证 = 未钉 `FROM postgres:18` 必须 rc=1 点名。

**审查期裁决与偏离（相对票面字面）**：

1. **无新镜像常量 / 无 dbtools.yml 双镜像扩展**：票面「新常量如 `DatabaseToolsImagePostgres18`」是双镜像假设下的写法；机制裁决为单镜像后，job 镜像恒为 `DefaultDatabaseToolsImage`（重建后的新 digest），「按 major 选择」落在**工具二进制路径**（`/usr/lib/postgresql/<major>/bin`）而非镜像选择。dbtools.yml 无需改构建形态（仍单镜像 build-push + cosign），仅头注随工具面更新。
2. **工具面路径显式化**（pg_wrapper 陷阱所致，见必查项 1）：PG16 脚本同步从裸名改绝对路径——同一二进制，行为不变；不显式化则 PG16 会在两代并存后静默切到 18 工具。
3. **门禁盲区修复**（见必查项 5）：最小改动 + 头注/runbook 记录；负路径自证改为 `FROM postgres:18`。
4. **PGDATA 参数化扩到渲染面**：除恢复脚本外，`renderEnv` 的 `PGDATA` 一并由模板 `VolumeMountPath` 派生（PG16 输出逐字不变），使 DB-1 的 percona 条目无需再改渲染器（DB-1 审查冻结设计「PGDATA 子目录约定」的落点）。
5. **发布面挂账**：`不 commit/push` 硬约束使「CI dispatch 构建**新内容**」客观不可行（dispatch 只能跑远端 ref 上的既有内容；新 Dockerfile 不在任何远端 ref）。发布按票面兜底「如实挂账」处理，命令与回填清单见实施记录。

### IMPL-DB-0 实施记录（2026-09-27，实现会话）

**状态：实现完成，待用户验收（未 commit）。机制 = 单镜像双工具面（审查裁决）；镜像发布按票面兜底挂账（阻塞证据与回填清单见文末）。**

变更文件清单（每文件一句）：

- `deploy/Dockerfile.dbtools`：新增 `FROM postgres:18@sha256:5a5a… AS postgres-engine-18`（FROM 形态——引用落进 check-image-pins 扫描口径）+ COPY 18 工具链到版本分区路径（`/usr/lib/postgresql/18`、`/usr/share/postgresql/18`）+ libnuma/liburing 补集；头注增「PG 双大版本工具面」纪律段，工具面清单/多架构行同步；纯 COPY 无 RUN、全 FROM digest 钉定不变。
- `deploy/check-image-pins.sh`：FROM 行豁免「tag 非纯数字」噪声过滤（修复 `postgres:16`/`redis:7` 等真实引用零计数的盲区）；头注识别口径/盲区同步。
- `internal/dbtemplate/dbtemplate.go`：`Engine`/`Distribution` 词表常量 + `Template` 身份字段（Engine/Distribution/Major）；既有四条目填身份值——ID/Image/其余字段值逐字不变。
- `internal/dbtemplate/render.go`：`renderEnv`/`ConnectionVars` 分派轴 ID→Engine；PGDATA 改由 `VolumeMountPath + "/pgdata"` 派生（PG16 现值逐字不变；percona 原生挂载点自然成立）。
- `internal/dbtemplate/dbtemplate_test.go`：新增身份字段齐备/逐字钉、PGDATA 参数化（合成 percona 条目）、PG URL 投影回归。
- `internal/database/adapters.go`：五处 switch ID→Engine；新增 `pgToolDir`（major → `/usr/lib/postgresql/<major>/bin`；未知 major/非 PG 引擎诚实报错）；PG 三段脚本工具二进制全改显式绝对路径；恢复 PGDATA 参数化 + 工具面缺面前置（exit 66）；`DefaultDatabaseToolsImage` 注释更新（双工具面 + 待发布回填说明）。
- `internal/database/rotate.go`：`rotateCredential` 分派轴 ID→Engine（票面未列的第六处同轴补全）。
- `internal/database/backup.go`：`pruneBackups` 前置模板读取（`pruneJobScript` 改签传模板）。
- `internal/database/adapters_test.go`（新）：major 选工具面（16/18/percona 路径）+ 反钉别代路径 + PG16 现值钉 + 全注册表脚本可构建 + 未知引擎 fail-loud。
- `internal/database/dbtools_manual_test.go`（新，`-tags manual` + env 门）：PG16/PG18 真机 dump→verify→restore 全链探针（adapter 原语脚本逐字 + docker run 承载 + 重放断言）。
- `.github/workflows/dbtools.yml`：头注更新（镜像内容 = 单镜像双 PG 工具面）；构建/签名形态零变化（机制裁决结果）。
- `docs/design/2026-09-20-managed-databases.md`：§2.6 增「工具面版本纪律」段（与实例数据目录同 major）。
- `docs/runbooks/image-prepull.md`：§5 门禁盲区修复记录与复验；台账新增 #22 行（**待 CI 发布**形态，digest 待回填）。
- `docs/plan/2026-09-26-torchwood-line-impl.md`：本审查/实施记录。

测试清单与票面四项验收逐条对应：

| 验收条款 | 证据（新增测试 + 一手观测） |
|---|---|
| ① PG18 真机探针（dump→verify→restore 全链，原始输出） | `TestManualDbtoolsPostgresMultiMajor/major18`（manual 探针；见下方原始输出）：官方 `postgres:18` 真机实例，adapter 原语脚本（`backupJobScript`/`verifyJobScript`/`restorePostgresJobScript` 对合成 PG18 模板的逐字产物）跑通，重放断言 = 备份后新增行消失、备份时刻行在场 |
| ② PG16 全链零回归 | 全量 `go test ./... -count=1` 全绿（32 包 ok）；变更包 `-race` 绿；`go vet ./...` 净；探针 `major16` 腿（同一批脚本对真实 `postgres:16` 实例全链）；`TestPostgres16ToolFaceValuesUnchanged`（PGDATA/命令现值逐字钉）；既有 `backup_test`/`restore_test`/`w4_mysql_mongo_test`/`rotate_test`/`upgrade_test` 零改动全绿（脚本断言为子串匹配，绝对路径化后语义不变） |
| ③ 镜像发布（CI 新 digest + cosign） | **挂账**（`不 commit/push` ⇒ CI 无法构建新内容；阻塞证据与命令见文末「发布挂账」）。本地实证：`docker buildx build --load -f deploy/Dockerfile.dbtools deploy` 成功，镜像 `1,132,959,955 B`（现行 `1,084,149,767 B`，+48.8MB/+4.5%）；`sh deploy/check-image-pins.sh` = `OK — 31 image reference(s) digest-pinned, 0 exempt`（rc=0），负路径 `FROM postgres:18` 未钉 → rc=1 点名 file:line |
| ④ redis/mysql/mongo 零回归 | 全量测试绿（M/G 腿适配器/恢复/轮换回归全在）；`TestEngineDispatchCoversEveryRegistryTemplate`（四引擎脚本全可构建）；`TestEngineDispatchUnknownEngineAndMissingMajorFailLoud` |

一手验证证据（原始输出摘要）：

```
$ go test ./... -count=1
ok  github.com/fleetlyrun/fleetly/internal/database    2.865s
ok  github.com/fleetlyrun/fleetly/internal/dbtemplate  6.246s
（其余 30 包 ok；含 api/state/engine/substrate/ingress/runtime/cmd 全链）

$ go vet ./...
（零输出，rc=0）

$ go test -race -count=1 ./internal/dbtemplate/... ./internal/database/...
ok  github.com/fleetlyrun/fleetly/internal/dbtemplate  6.445s
ok  github.com/fleetlyrun/fleetly/internal/database   71.657s

$ sh deploy/check-image-pins.sh            # 正路径（修复后计数 28→31）
check-image-pins: OK — 31 image reference(s) digest-pinned, 0 exempt      # rc=0
$ sh deploy/check-image-pins.sh deploy/Dockerfile.dbtools
check-image-pins: OK — 6 image reference(s) digest-pinned, 0 exempt       # 六个 FROM 全数
$ sh deploy/check-image-pins.sh <tmp>/Dockerfile.neg     # 负路径：FROM postgres:18 未钉
::error file=…/Dockerfile.neg line=2::container image reference without digest: postgres:18
check-image-pins: FAILED — 1 unpinned reference(s) (1 pinned, 0 exempt)   # rc=1
$ sh -n deploy/check-image-pins.sh         # OK

$ docker buildx build --load -t fleetly-dbtools:local -f deploy/Dockerfile.dbtools deploy
#36 DONE … naming to docker.io/library/fleetly-dbtools:local
$ docker inspect --format '{{.Size}}' fleetly-dbtools:local
1132959955        # 现行 v0.3.0-dbtools.1 = 1084149767（+48.8MB）；双镜像否定项候选 = 1089486184（第二枚全镜像）
$ docker run --rm fleetly-dbtools:local /usr/lib/postgresql/16/bin/pg_dump --version
pg_dump (PostgreSQL) 16.15 (Debian 16.15-1.pgdg13+2)
$ docker run --rm fleetly-dbtools:local /usr/lib/postgresql/18/bin/pg_dump --version
pg_dump (PostgreSQL) 18.6 (Debian 18.6-1.pgdg13+2)

$ FLEETLY_MANUAL_DBTOOLS=1 FLEETLY_MANUAL_DBTOOLS_IMAGE=fleetly-dbtools:local \
    go test -tags manual ./internal/database -run TestManualDbtoolsPostgresMultiMajor -v
=== RUN   TestManualDbtoolsPostgresMultiMajor
    dbtools_manual_test.go:72: tools face pg_dump 16.: pg_dump (PostgreSQL) 16.15 (Debian 16.15-1.pgdg13+2)
    dbtools_manual_test.go:72: tools face pg_dump 18.: pg_dump (PostgreSQL) 18.6 (Debian 18.6-1.pgdg13+2)
=== RUN   TestManualDbtoolsPostgresMultiMajor/major16
    dbtools_manual_test.go:82: leg major16 server: PostgreSQL 16.15 (Debian 16.15-1.pgdg13+2) …
    dbtools_manual_test.go:82: leg major16 backup snapshot=1eb02bf1b9755eb52718fa3bc4fa52a6e4f3b9a038fa37e174c0f64e9aaa5dcb size=0
    dbtools_manual_test.go:82: leg major16 verify ok:
    dbtools_manual_test.go:82: leg major16 restore ok: … starting PostgreSQL 16.15 … database system is ready to accept connections … DROP DATABASE / CREATE DATABASE … server stopped
    dbtools_manual_test.go:82: leg major16 replayed rows: 1:at-backup-time
=== RUN   TestManualDbtoolsPostgresMultiMajor/major18
    dbtools_manual_test.go:82: leg major18 server: PostgreSQL 18.6 (Debian 18.6-1.pgdg13+2) …
    dbtools_manual_test.go:82: leg major18 backup snapshot=44bb90bc623192947f220f9c43651094e19095ace29a402321182731e857dd8b size=0
    dbtools_manual_test.go:82: leg major18 verify ok:
    dbtools_manual_test.go:82: leg major18 restore ok: … starting PostgreSQL 18.6 … database system is ready to accept connections … DROP DATABASE / CREATE DATABASE … server stopped
    dbtools_manual_test.go:82: leg major18 replayed rows: 1:at-backup-time
--- PASS: TestManualDbtoolsPostgresMultiMajor (28.58s)      # 两条腿均 PASS
```

探针环境注记：本机 Docker 29.7.2 / swarm 无关（`docker run` 等价承载 adapter 的 `["sh","-c",script]` Cmd 形态）；restic repo = 本地卷（免 S3 依赖）；实例容器挂平台同款卷路径（`PGDATA=<挂载点>/pgdata`）。**「size=0」是存量观测**：restic 0.19 `--json` summary 无 `total_bytes` 字段（真实字段 `total_bytes_processed`/`data_added`——本机实测 schema 原文见审查节），`parseResticSummary` 的 tag 从未命中 ⇒ `db_backups.size_bytes` 生产恒 0（既有测试夹具的假 JSON 用了同名字段，故单测不可见）。**本票不修**（出票面范围；建议另立小票：tag 改 `total_bytes_processed` + 夹具同步 + 台账断言更新）。

偏离清单（实现中的决策，均按纪律登记）：

1. **无新镜像常量、dbtools.yml 构建形态零变化**：单镜像机制裁决的直接结果（票面「新常量如 `DatabaseToolsImagePostgres18`」的双镜像假设不成立）；「按 major 选择」落在工具二进制路径（`pgToolDir`），job 镜像恒 `DefaultDatabaseToolsImage`。
2. **PG16 脚本同步显式绝对路径**：pg_wrapper 陷阱（两代并存后裸名解析为 18）使「裸名 = 现值」不再成立；`/usr/lib/postgresql/16/bin/*` 即今日 pg_wrapper 的解析结果（同一二进制；wrapper 在显式 `-h` 下不注入 cluster 缺省），行为逐字不变——既有脚本断言为子串匹配故零改动全绿，新测试另钉绝对路径。
3. **门禁修复**：`check-image-pins.sh` 的 FROM 行豁免纯数字 tag 噪声过滤（审查必查项 5 的盲区；修复后仓库计数 28→31，负路径自证改为本票真实形态 `FROM postgres:18`）。`COPY --from=<外部镜像>` 盲区不修（头注明示改走 FROM stage 形态）。
4. **PGDATA 参数化扩到渲染面**：除恢复脚本外，`renderEnv` 的 PGDATA 一并由 `VolumeMountPath` 派生（PG16 输出逐字不变）——DB-1 的 percona 条目无需再改渲染器。
5. **第六处分派轴迁移**：`rotate.go:rotateCredential`（票面只列 adapters.go 五处 + render.go 两处）；不迁移则 percona/新 major 条目会在轮换面落 `no rotation adapter`。
6. **`pruneJobScript`/`backupFilename` 签名改传模板**：脚本构建器统一「调用方解析模板 → 纯函数拼装」（避免脚本构建器内重复 `dbtemplate.Get`；探针/单测可对合成模板钉行为）。
7. **探针不依赖 DB-1 词表**：PG18 侧用合成模板（`syntheticPostgresTemplate(18, …)`，只进测试）——DB-1 落 `postgres-18` 条目后可换读注册表；引擎镜像字面 = `postgres:18@sha256:5a5a…`（与 Dockerfile 的 FROM 同 digest）。
8. **`db_backups.size_bytes` 存量缺陷仅记录不修**（见上「size=0」注记）。
9. **新发现（DB-1 交接项，审查节已列）**：percona 数据目录 + pgvector 的原地重放对 vanilla PG18 工具链不可用（`extension "vector" is not available` → 依赖表/数据整链失败）——DB-1 需按发行版决策（建议：dbtools PG18 面补 percona vector 控制/库文件，或恢复临时实例改用引擎镜像）。本票机制（major 面）不覆盖分布差异，如实交接。

**验收追认（2026-09-27）**：用户以「提交推送」指示验收，追认五项裁决与两项交接：①单镜像双工具面（无新镜像常量，工具按 major 显式绝对路径选择；否定双镜像）；②PG16 脚本同步绝对路径化（pg_wrapper 裸名解析陷阱，行为逐字不变）；③check-image-pins FROM 行盲区修复（未钉真实引用原可蒙混）；④PGDATA 参数化扩到渲染面；⑤发布挂账（推送后 CI 发布 + 三锚回填）；交接一 = percona+pgvector 恢复归 DB-1；交接二 = `db_backups.size_bytes` 存量缺陷另立小票。

**发布挂账（票面兜底「如实挂账并给阻塞证据」；不虚构 CI 结果）**：

- 阻塞证据：`gh auth status` = `qiulin@github.com`，scopes 含 `workflow`（dispatch 能力在）；`git ls-remote origin` 仅 `refs/heads/main`，本票改动全部未提交/未推送。GitHub Actions `workflow_dispatch` 只能跑**远端 ref** 上的既有内容——新 Dockerfile 不在任何远端 ref，dispatch 只会重建旧内容（无意义）；而「不 commit/push」是本票硬约束，故 CI 构建新内容客观不可行。本机未做任何 ghcr 推送（供应链纪律：发布只走 CI + cosign）。
- 解除步骤（用户验收后执行，三条命令 + 三锚回填）：
  1. `git add deploy/Dockerfile.dbtools deploy/check-image-pins.sh …`（本票全部变更）→ commit → push（或仅推送承载 Dockerfile 的提交）；
  2. `gh workflow run dbtools.yml --repo fleetlyrun/fleetly --ref <branch> -f tag=v0.3.1-dbtools.1` → `gh run watch` 等完 → `gh run view --log | grep -i digest`（或 `docker buildx imagetools inspect ghcr.io/fleetlyrun/dbtools:v0.3.1-dbtools.1` 取多架构 index digest）；
  3. 三锚回填：`internal/database/adapters.go` 的 `DefaultDatabaseToolsImage`、`e2e/databases.sh` 的 `DBTOOLS_IMG`、`docs/runbooks/image-prepull.md` 台账 #22 的 digest 列（cosign 签名由 workflow 自带 verify 门确认）。
- 回填前语义：PG16 模板的 job 在旧镜像上仍全功能（显式 16 路径在旧镜像同样存在）；PG18 模板（DB-1 落条目后）的 job 会以「镜像缺该 major 工具面」显式失败（fail-loud）。

**发布完成（2026-09-27，本会话执行——挂账解除）**：本票提交推送（`1574121`）后 dispatch `dbtools.yml`（run **36303530093**，tag `v0.3.1-dbtools.1`，conclusion=success，cosign 签名 + 验签门随工作流）；多架构 index digest = `sha256:c6cafbc3…20382`（`docker buildx imagetools inspect` 实测；amd64 manifest `729a7f42…`、arm64 `13a0c1e7…`）；发布产物按 digest 拉取实证双工具链（pg_dump 16.15/18.6）+ 真机探针两腿 PASS（本会话复跑）。三锚已回填：`DefaultDatabaseToolsImage`、e2e `DBTOOLS_IMG`、台账 #22。arm64 运行腿仍为 staging 待验项（本机存储限制，见下）。

staging/真机待执行项（本环境不可得者，未虚构）：

- **多架构 arm64 运行腿**：本机 classic image store 对同 digest 换平台报 `cannot overwrite digest`（DB-1 审查同款限制），arm64 本地不可运行验证；缺口库闭包由 `COPY` 同源 + `/usr/lib/*-linux-gnu/` 通配按构造覆盖，**CI buildx 双平台构建**为第一道门（amd64 已实证；arm64 建议 staging/dind 按 digest 独立拉取复验）。
- staging 真机复验建议：发布后对 `postgres-18` 实例跑一次 backup→verify→restore（DB-1 的矩阵承接），并观察 `db_backups.size_bytes` 是否为 0（存量缺陷现场确认）。
- e2e `databases.sh`：DBTOOLS_IMG 随发布回填（三锚之一）；D 腿（PG16）预期零回归，DB-1 增设的 P 段（PG18 矩阵）承接新条目全链。
