# fleetly E4 数据库托管专项设计

| 状态 | 日期 | 说明 |
|---|---|---|
| **已实现+真机验证（2026-09-22，W4 收官）**：六阶段落地（状态层/模板层 → 收敛器+生命周期 → 引用注入+D-DB-11 → secrets+轮换 → 备份恢复升级 → Console+e2e）；dind e2e 27/27（逼出修复 5 处）+ staging 真机 35/35（runbook §9）；dbtools 首个第一方平台镜像已发布签名。落地注记已回写：PG 暂停轮换拒绝（§2.5）、恢复凭据边界（§2.2）、轮换任务重建措辞（§2.5） | 2026-09-22 | 原裁决轮 2026-09-20：[v0.2 规划 §2 W4 行 / §5 E4 行 / §3 V2-5、V2-2](../plan/2026-09-20-v0.2-plan.md)；[架构 §2.3/§2.4 应用模型数据库行 / §4.3 v0.2 路线](2026-09-17-architecture.md)；[发布专项 §2.4 快照与回滚](2026-09-17-release-semantics.md)（D-DB-11 限定已回写）；[放置专项（卷钉住复用）](2026-09-17-stateful-placement.md)；[状态模型专项（表/台账纪律）](2026-09-17-state-model.md)；[zane-ops 调研 R5（generate_* 先例）](../research/2026-09-20-zane-ops-comparison.md)；[v0.1 冻结清单 FZ-1（source=system）](../plan/2026-09-17-v0.1-scope-freeze.md) |

## 1. 现状与问题

v0.1 收口后，「托管数据服务机制」（架构 §1.2/§2.4：模板 + 卷钉住 + 备份/恢复适配器 + 连接串注入）全部缺位，用户自建数据库只能以普通 compose app 形态部署，承担四类现存的硬缺口：

| # | 现状 | 缺口 |
|---|---|---|
| 1 | 应用模型只有 git/image 两种来源（apps 表 + git 触发列，`internal/state/appgit.go`） | 无库实例资源：创建/暂停/升级/删除无生命周期面；无模板 |
| 2 | 跨 app 网络严格隔离（每 app 专属 overlay + 别名 = compose 服务名，`internal/naming.NetworkName`） | app 无法连另一个 app 的库——用户被迫把库塞进同一 compose，卷钉住与库运维语义混入业务 app |
| 3 | env 三层合并链已留位 `source=system`（envlayer 词表 + `env_vars.source` 列，v0.1 无生产者——代码注释明示「模板连接串留位」） | 无凭据生成/轮换/注入实现；FZ-1 挂账 W4 |
| 4 | compose `secrets` 在 Load 期显式拒绝（`E_COMPOSE_UNSUPPORTED`，评审 C1：平台密钥库未接入前拒绝比晚期失败诚实） | 无平台密钥库；Swarm secret 管道（`engine.SecretMount`/`substrate` SecretReference、`naming.SecretName` hash8 换名）已就绪但无来源 |
| 5 | 备份台账只有控制面状态（`state_backups`：SQLite 快照 + 本地路径，S3 上传随 E3） | 库数据无备份/恢复；E3（restic + S3/RustFS）是本设计的前置 |

实现侧已核实的三个关键事实（设计以此为锚，不重造）：

- **env 管道就绪度高于预期**：`env_vars(app_id, key, value 密文, source ∈ {platform,system}, status)` 唯一键 (app_id,key)；`envlayer.MergeChain` 已实现平台层内部 `system > platform`；pending 参与合并、部署成功后统一提升（S16-C4）。连接串注入 = 写 source=system 行，**合并链零改动**。
- **Swarm secret 管道就绪**：`engine.ServiceSpec.Secrets`（SecretName + Target=/run/secrets/\<名\>）与 `substrate` 适配已实现并有漂移反解；缺的只是存储（app_secrets 表）、compose 校验开放与 planner 接线。
- **回滚 env 语义有暗礁**：`internal/engine/rollback.go` 头注——重放「compose 字段与合并 env 按快照（desired_spec 密文，含合并 env 明文）」。连接串（含密码）一旦进快照，回滚会回放**旧密码**，与发布专项 D-REL-9「secret 值永远取当前」冲突。v0.1 因密钥库未接入而「结构性满足」；E4 落地时此路径必被击穿，须显式修正（D-DB-11，§6）。

## 2. 目标设计

### 2.1 资源模型：库实例 = 独立一等资源（D-DB-1 用户终裁）

**库实例（database instance，待入术语表）是独立一等资源**：自有表 `db_instances`、自有 API（`/v1/databases`）、自有生命周期状态机——不复用 apps 表、不产生 deployments/revisions、不走部署管线。模板渲染产物 = 平台受管的 Swarm 服务形态（ServiceSpec 投影），由库专属收敛器（provisioner）落地，不进 compose 期望态。

```
用户/API                                平台内部                              复用组件（包级，不依赖 app 形态）
CreateDatabase ──→ db_instances 行(state=provisioning) + 凭据生成(密文列)
                 ──→ 模板渲染 → ServiceSpec 投影 ──→ 放置选点/前哨(internal/placement)
                                                 ──→ 卷登记(volumes, owner 泛化)
                                                 ──→ substrate 服务原语(NetworkEnsure/Create/Update)
                                                 ──→ 健康门(pg_isready/redis-cli ping) → ready
引用 app 部署 ──→ label 解析 → db_references 倒排 + system env 物化 + 网络挂载（app 侧既有部署管线）
```

**生命周期状态集（7 态）与转移表**（主状态承载可用性与生命周期；升级/备份/恢复/轮换是操作，不换主状态，见 §2.3）：

| from | to | 触发 | 事件 |
|---|---|---|---|
| — | provisioning | CreateDatabase 受理（渲染模板、建绑定/卷/服务） | db.provision_started |
| provisioning | ready | 健康门通过 | db.ready |
| provisioning | failed | 收敛失败（健康门超时/镜像不可得/引擎错误）；保留现场（服务与卷不删） | db.provision_failed |
| failed | provisioning | 显式重试（retry） | db.provision_started |
| ready | degraded | 就绪后不健康（健康探测失败/任务崩溃循环，Swarm 自愈观测中） | db.degraded |
| degraded | ready | 恢复（健康探测通过） | db.recovered |
| ready / degraded | paused | Suspend（scale 0，保留服务与卷） | db.suspended |
| paused | provisioning | Resume（重收敛） | db.resumed |
| ready / degraded / paused / failed | deleting | Delete 受理（引用守卫 `E_DB_REFERENCED` 通过后；tombstone 第一拍） | db.delete_started |
| deleting | deleted | reap 完成（受管对象移除；卷按选择保留转 orphaned 或显式删除） | db.deleted |

- `provisioning` 兼作「重收敛态」：创建、resume、retry 共用（进入即重走服务收敛 + 健康门）；「created」不单设——受理即 provisioning。
- `degraded` 与 `failed` 的分界：degraded = 在役不健康、自愈可期（无平台收敛动作失败）；failed = 平台收敛彻底失败、需显式 retry/人工。
- `deleting` 无失败出边：reap duty 幂等重试直至完成（对齐 app tombstone 纪律）；卷数据永不随状态机自动删除。
- 同实例操作互斥 = 状态机前置态前哨 + CAS（§2.3 单写点纪律），不建部署队列副本。

**组件级复用清单**（包级组件，与 app 形态解耦，独立资源下照用）：

| 组件 | 复用方式 |
|---|---|
| 放置绑定（internal/placement 选点决策/前哨 + runtime_node_refs 节点锚，D16） | 绑定内嵌 `db_instances.platform_node_id`（**不写 placements 表**——该表以 app_id 为主键）；有卷自动钉住、节点失联 → degraded/blocked 可见态同款语义 |
| 卷注册表（volumes + 登记语义） | 表归属泛化（owner_kind ∈ {app, database}，§5.4 迁移①）后照用：数据诞生点钉住/orphaned/discarded/命名防代际全复用 |
| substrate 服务原语（NetworkEnsure/ServiceCreate/Update/Inspect/TaskList） | 库服务即受管 Swarm service，desired-hash label 纪律照用；漂移检测默认开、收敛 per-instance opt-in（D11 口径沿用） |
| secret 管道（age Box + SwarmSecretRef + SecretMount 注入） | 库引擎凭据经同管道投递（D-DB-10）；用户 app secrets 开放同管道（§2.7） |
| 审计（fail-closed 同事务） | db.* 动作词表照用既有审计面 |
| 备份基础设施（E3 restic/S3/RustFS + E5 调度核） | §2.6 全节 |
| naming（唯一定义点纪律） | 新增库族公式（`fleetly-db-<name>-*` 前缀族，§2.4/§5.4）——新增函数非改既有公式 |

**明确不复制的东西**（领域分界，D-DB-1 终裁的必然推论）：

| 不复制 | 替代 |
|---|---|
| 部署队列（per-app 互斥 + queued 扫描） | 状态机前置态前哨 + CAS 天然互斥（操作各自声明合法前置态，并发第二笔冲突 409，§2.3） |
| EnterPhase / deployments 表 | 库自有单写点 `EnterDbPhase`（同款纪律：同事务转移校验 + CAS + 事件，§2.3） |
| 观察窗 / revision 快照重放回滚 | 库升级 = **受控重建 + 备份门**（§2.2）：升级前强制 pre_upgrade 备份且 verify 通过才继续；失败 = digest 归位（回写旧值重建），非版本重放 |
| 构建管线 / compose 解析 | 模板渲染器（平台受管 ServiceSpec 投影，无用户 compose） |

**名字空间独立**：库对象命名走 `fleetly-db-<name>-*` 前缀族（服务/网络/卷/secret，§2.4/§5.4），与 app 的 `fleetly-<app>-*` 对象空间解耦——app 与库实例**可重名**（对象不撞、API 路径分立 `/v1/apps` 与 `/v1/databases`）；引用方 env 前缀 `FLEETLY_DB_<NAME>` 的唯一性由 `E_DB_ENV_PREFIX_CONFLICT` 守卫（同 app 引用撞前缀才冲突）。独立前缀使跨资源名冲突不存在，零新增「名字冲突」错误码。

- **UI/CLI 面**：`/v1/databases` 独立资源面（create/get/list/delete/suspend/resume/settings/upgrade/rotate/backup/restore）；Console 库实例为一等页面。CLI `fleetly databases <verb>`。API-first 铁律：REST 先行，Console/CLI 为客户端。
- **审计面**：库操作全部入审计（`db.*` 动作词表见 §5），与业务写同事务 fail-closed 照用。
- **术语对齐**：库实例不是「服务」不是「容器」更不是 app；模板（template）= 平台内置的引擎定义；引用（reference）= 引用方 app 服务对库实例的声明关系；provisioning = 库收敛（创建/恢复/重试共用的收敛态）。均标注待入 UBIQUITOUS_LANGUAGE。

### 2.2 模板机制

模板 = **平台内置 Go 注册表条目**（`internal/dbtemplate` 新包），随平台版本发布；不是文件、不是数据、不可热更。每个模板定义：

| 字段 | postgres-16（首发） | redis-7（首发） |
|---|---|---|
| 镜像 | `postgres:16-x` digest 钉定（随平台 release 锁定，R7 门禁同批） | `redis:7-x` digest 钉定 |
| 引擎内部端口 | 5432 | 6379 |
| 卷 | key=`data`，挂 `/var/lib/postgresql/data`（PGDATA 子目录约定由模板处理 initdb lost+found 问题） | key=`data`，挂 `/data` |
| 凭据规格 | 固定 `POSTGRES_USER=fleetly`；`POSTGRES_DB=<实例名，'-'→'_'>`；生成 32 位 [a-zA-Z0-9] 密码 | 仅生成 32 位密码（requirepass） |
| 凭据投递 | **Swarm secret 文件**（`POSTGRES_PASSWORD_FILE=/run/secrets/password`，官方镜像原生支持） | 启动参数 `--requirepass`（Swarm spec 参数明文——与 env 同暴露类，文档明示，见 §6 已知边界） |
| 健康门 | `pg_isready -U fleetty`（interval 5s/timeout 3s/retries 3/start_period 30s） | `redis-cli ping`（同缺省） |
| 默认限额 | cpus 1.0 / memory 1Gi（创建时可覆盖） | cpus 0.5 / memory 256Mi |
| 连接串渲染 | `postgres://fleetly:<pw>@<实例名>:5432/<dbname>` | `redis://:<pw>@<实例名>:6379/0` |

- **升级语义（受控重建 + 备份门）**：平台 release 携带新 digest（minor/patch）→ 既有实例**不自动变**；`databases upgrade` 逐实例 opt-in，流程 = ①自动 `pre_upgrade` 备份且 verify 通过才继续（失败 → `E_DB_BACKUP_FAILED` 中止，实例不动）②spec 换新 digest 受控重建（有卷强制 stop-first，停机窗口如实累计）③健康门 ④失败 = digest 归位（回写旧值重建）+ `db.upgrade_failed` + 状态落 degraded——**不是 revision 重放**（库无 revision/部署记录，D-DB-1 终裁推论）。平台检测到可升级 → `db.upgrade_available` 事件。主版本升级（16→17）与引擎切换不做（§7）。**暂停实例的备份门（W4-S5 落地注记）**：停摆引擎构造性无法执行 pg_dump/RDB 导出，降格为「台账内存在本实例 verified 备份」的存在性检查——无已验证备份 → 如实拒绝并指引先恢复再升级（不做假备份门）。**恢复的凭据语义边界（W4-S5 落地注记）**：原地恢复重放的是备份时刻的库内密码——若备份后轮换过凭据，恢复后库内密码与权威态密文错位（`databases reveal` 可对账），runbook 记人工收尾（恢复后再 rotate 一次即对齐）。
- **镜像受管**：用户不可改库镜像/引擎参数（设置面只有限额与备份计划）；违规 → `E_DB_TEMPLATE_UNSUPPORTED`。
- 新引擎接入成本 = 一个模板条目 + 一个 `EngineAdapter`（§2.6）+ 备份镜像工具，与架构 §4.3「各引擎备份/恢复适配器是主要成本」一致；MySQL/Mongo 后置按需求排序。

### 2.3 生命周期与操作

**单写点纪律（与 EnterPhase 同款，T0-V2.2 纪律延伸）**：库状态机全部转换收敛 `state.EnterDbPhase` 单写点——同一事务内完成转移表校验（§2.1 转移表为唯一真源，穷举测试钉死）+ CAS（`WHERE state = <from>`）+ 字段写 + 事件；收敛器/引擎侧裸 Status 写清零。**并发操作互斥由前置态前哨 + CAS 结构性成立**：每笔操作声明合法前置态（下表），非法前置态或 CAS 失败 → 409 复用 `E_STATE_VERSION_CONFLICT`（context 带 current_state 与合法前置态清单——同族乐观冲突语义，零新增码，D-DB-8）；不建部署队列副本。

| 操作 | 合法前置态 | 语义 |
|---|---|---|
| 创建 | — | db_instances 行（provisioning）+ 凭据生成（密文列）+ 收敛器：模板渲染 → 选点/卷登记/服务创建 → 健康门 |
| 重试 retry | failed | → provisioning 重收敛（现场保留：服务与卷不删） |
| 设置变更 | 任意非终态 | 限额/备份计划 → settings 更新；限额变更 = spec 重建（无备份门——无数据面变更）；主状态不变 |
| 暂停 | ready / degraded | → paused（scale 0，保留服务与卷）；引用方连不上是诚实暴露（错误信息即产品） |
| 恢复 | paused | → provisioning（重收敛）→ ready / degraded |
| 升级 | ready / degraded / paused | 受控重建 + 备份门（§2.2）；主状态不变（事件承载）；paused 下仅换 spec 不重启，收敛推迟到 resume |
| 轮换 | ready / degraded / paused | §2.5；破坏性两段式确认 |
| 备份 | ready / degraded | §2.6 |
| 恢复 | ready / degraded | 原地重放（confirm 破坏性确认；§2.6） |
| 删除 | ready / degraded / paused / failed | 引用守卫：`db_references` 非空 → `E_DB_REFERENCED`（409，context 列出引用 app/服务清单）——与放置前哨（卷-节点 409）同型的数据安全前哨；通过后 → deleting（tombstone 第一拍）→ reap duty 幂等清理受管服务与共享网络 → deleted（名字保留期占用）；卷默认保留转 orphaned，显式 `--delete-volumes` 才删数据（卷语义复用） |

### 2.4 跨 app 网络：平台牵线共享网络（V2-5 落地细则）

**库实例专属共享网络**：`fleetly-db-<name>-net`（naming 新增库族公式 `DBNetworkName`——独立资源独立前缀，新增函数非改既有公式；库对象名族 `fleetly-db-<name>-*` 与 app 的 `fleetly-<app>-*` 解耦，见 §2.1 名字空间独立）。库服务（`fleetly-db-<name>-<模板服务名>`）挂该网络，**别名 = 库实例名**（而非模板服务名——防两个 PG 实例的通用别名 `postgres` 在共享网络互撞 DNS；模板渲染层供给）。引用方服务由平台在**其部署时**附加挂载到该网络（无附加别名，以 Swarm 服务名可达）。

| 时序点 | 行为 |
|---|---|
| 库收敛（provisioning） | `NetworkEnsure(fleetly-db-<name>-net)`（既有幂等创建，managed label） |
| 引用 app 部署（plan 期） | 解析服务 label `fleetly.databases: "pg-prod"`（逗号分隔，多库可引）→ 逐名校验：`db_instances` 存在且 state ∉ {deleting, deleted}，否则 `E_DB_NOT_FOUND`（404，附候选清单）；**不要求 ready**（建库与引用部署可并行；未就绪/已暂停/failed → 计划警告 `W_DB_REFERENCE_NOT_READY`，不阻塞） |
| 引用 app 部署（plan 期） | env 前缀计算：实例名 `-`→`_` 大写；同 app 引用两个前缀撞名（`pg-prod` vs `pg_prod`）→ `E_DB_ENV_PREFIX_CONFLICT`（422） |
| 引用 app 部署（releasing 前） | `NetworkEnsure` 全部附加网络（既有调用面扩展为多网络）→ ServiceSpec.Networks += 共享网络（desired-hash 含网络，后续手动摘挂进漂移面） |
| 引用关系登记 | planner 同事务维护 `db_references(db_id, app_id, service, env_prefix)` 倒排索引——**可从各 app 当前 revision 重建的派生登记**（compose 仍是引用方唯一期望态真源）；label 移除并部署 = 行删除；引用 app 删除 = 行级联清理 |
| 库删除（前置哨兵） | `db_references` 非空 → `E_DB_REFERENCED`（409，context 列出引用 app/服务清单）——与放置前哨（卷-节点 409）同型的数据安全前哨 |
| 库删除（reap） | 引用清零后删除受管 service 与共享网络 |

不开放任何用户自由跨网语法（compose 外部网络仍在拒绝清单，V2-5 裁决）；网络内流量为 Swarm overlay 缺省**不加密**（性能与兼容取舍，已知边界，§6）。

### 2.5 凭据与连接串注入（FZ-1）

- **生成时机**：创建库实例时一次（模板凭据规格），落 `db_instances.credential_cipher`（age 密文列——独立资源自有存储；USER/DATABASE 由实例名与模板确定性推导，不落库）。引用方的物化行仍是 app 的 `env_vars`（source=system）——app 侧存储形态不变。
- **注入形态**：引用方 app 部署时，planner 把连接信息**物化**为引用 app 的 `env_vars` 行（source=system，走既有 upsert → pending → 本次部署合并消费 → 成功后提升的完整链路；值变化 → 行回 pending → 随下次部署生效——与 S16-C4 天然一致）。键集（模板渲染定义）：

| 引擎 | 物化键（`<NAME>` = 实例名大写下划线形） |
|---|---|
| PG | `FLEETLY_DB_<NAME>_URL`、`_HOST`（= 实例名 DNS 别名）、`_PORT`（5432）、`_USER`、`_PASSWORD`、`_DATABASE` |
| Redis | `FLEETLY_DB_<NAME>_URL`、`_HOST`、`_PORT`（6379）、`_PASSWORD` |

  `FLEETLY_*` 前缀为平台保留名字空间：用户 `SetEnv` 撞前缀 → `E_ENV_KEY_RESERVED`（422；同时防 system 行被用户 upsert 劫持 source——现状 `SetAppEnv` 会改写 source，此守卫是必要补丁）。
- **可见性**：只读展示照既有 env 投影（`EnvVarView.source=system` 已支持；Console 库详情页展示连接信息，密码默认脱敏、显式展开；`fleetly databases show` 同级 admin 面）——对齐 R5 先例（生成、存平台层、对用户只读）。
- **轮换**：**仅手动**（`databases rotate`，破坏性操作两段式确认）。流程按引擎经适配器钩子：PG = 一次性 job 执行 `ALTER USER fleetly WITH PASSWORD`（引擎级热换——真机实测 rotate 同秒新密码即连通；**W4 真机注记：Swarm secret 不可变 → 换值必换名 → spec 变更触发库任务快速重建（实测亚秒收敛，连接无感窗口），「库不重启」的准确口径 = 引擎数据面不停机、非任务零重建**；**引擎级限定（W4-S4 落地注记）：PG 暂停态拒绝轮换**——initdb 只在首启读密码文件，暂停实例无法 ALTER，如实 409 提示先 resume，不受理假轮换）；Redis = 密文列更新 + 库实例受控重启（任务重建，requirepass 重读；暂停态允许——spec 更新，resume 时以新密码重建任务）。随后：逐引用 app 的 system 物化行更新为 pending → **平台自动触发全部引用 app 重部署**（各自走正常部署队列；不重部署 = 旧密码失效即断连，无更诚实选项）→ `db.credentials_rotated` 事件 + 审计。定期自动轮换不做（§7）。
- **连接串格式**：引擎标准 URI（§2.2 表）；密码字符集 [a-zA-Z0-9] 免 percent-encode。

### 2.6 备份与恢复适配器（对接 E3）

每引擎一个 `EngineAdapter`（Go interface，conformance 套件随 MySQL/Mongo 接入时补齐——首两引擎单实现，先钉接口不建套件）：

```go
type EngineAdapter interface {
    // Backup 逻辑备份：一次性 Swarm job（钉实例绑定节点、挂实例卷）内
    // 流式导出 → restic 入库（repo 内路径 db/<instance>/<snapshot>）。
    Backup(ctx context.Context, in BackupInput) (BackupOutcome, error)
    // Restore 原地恢复：实例 scale 0 → job 挂卷 rw 重放 → 重部署。
    Restore(ctx context.Context, in RestoreInput) error
    // Verify 回读校验：restic 读回 + 引擎级头部校验（pg_restore --list /
    // RDB magic），台账 verify_status 置位——「备份假成功」零容忍。
    Verify(ctx context.Context, in BackupOutcome) error
    // RotateCredential 引擎侧热轮换（§2.5）。
    RotateCredential(ctx context.Context, in RotateInput) error
}
```

| 裁定点 | 结论 | 被否方案 |
|---|---|---|
| PG 备份形态 | **pg_dump -Fc 逻辑备份**（运行中一致性、引擎 minor 版本可移植、体积小） | restic 卷快照 PG 数据目录：WAL 在途写入非崩溃一致（需停库才稳）；恢复跨版本脆弱；被否 |
| Redis 备份 | RDB 流式导出（`redis-cli --rdb -` 管道） | AOF 复制（体积与恢复复杂度）；卷快照（同上） |
| 备份目标 | 复用 **E3 restic 基础设施**（外部 S3 端点或 opt-in RustFS），repo 内独立命名空间 `db/<instance>/` | 独立 S3 桶/独立 repo（配置面翻倍）；直传 S3 不过 restic（失去去重/加密/完整性） |
| 台账 | **独立 `db_backups` 表**（restic snapshot ID 寻址，非文件路径；kind ∈ daily/manual/pre_upgrade；verify_status 三态） | 并入 `state_backups`：控制面快照与库数据备份的保留策略/恢复语义/schema 全不同，CHECK 重建还牵连；被否 |
| 恢复 | **原地恢复**（同实例，停库重放，confirm 破坏性确认） | 一键恢复到新实例：跨节点 DR 场景走「建新库 + 手动重放 + rebind」文档化 runbook（E1 rebind CLI 组合），不进 v0.2 API |
| 执行体 | 一次性 Swarm job（同放置约束钉节点——**远端节点 local 卷不可经 manager 读**是既有硬约束）、镜像 = 平台 dbtools 镜像（引擎工具 + restic，digest 钉定随平台发布）、库凭据与 S3 目标经 Swarm secret 注入 | 控制面中转流（受远端卷不可读约束 + 单点带宽）；常驻 agent（违背轻单核） |
| 调度 | 复用 E5 调度核（备份 ticker 演进，同一调度器），per 实例计划（缺省每日 03:00 UTC，保留 7 份，prune 沿用台账保留期删除语义） | 自建定时器 |
| 诚实口径 | 同节点 RustFS 目标上的库备份 = **便捷层非灾备**（V2-2 口径延伸），Console 与文档同标注 | — |

**工具面版本纪律（IMPL-DB-0 增补，2026-09-27）**：dbtools 执行体与实例数据目录**同 major**——PG 的 `pg_dump`/`pg_restore`/`postgres`（及消费链 `psql`/`pg_isready`/`pg_ctl`）跨大版本有硬语义边界（`pg_dump` 拒更高 major 服务器、`pg_restore` 拒更高 major 归档、临时恢复实例拒异 major 数据目录），job 脚本按模板 `Major` 取 `/usr/lib/postgresql/<major>/bin` 显式绝对路径（两代工具面并存后裸名会被 postgresql-common `pg_wrapper` 解析为最新 major）。单镜像双工具面的机制裁决与实证见 `docs/plan/2026-09-26-torchwood-line-impl.md` §4「IMPL-DB-0 方案可行性审查」；大版本升级不做，升 major = dump/restore 新实例（§7）。

### 2.7 平台密钥库与 compose secrets 开放

**开放面 = 所有 app**（含库实例自身），机制统一；仅对库实例开放会造出第二套 secret 语义。

- **唯一来源 = 平台密钥库**：新表 `app_secrets(app_id, name, value_cipher age 密文, hash8, UNIQUE(app_id,name))`；`fleetly secrets set/list/rm` + proto SecretsService（Set/List/Remove）。**不提供值读回**（忘记即轮换——比 env GetEnv 的 admin 明文路径更严一档：secret 是更敏感类；hash8 供引用比对）。轮换 = 换 hash8 换 Swarm secret 名（`naming.SecretName` 既有设计）→ 引用进 desired-hash → 随下次部署换挂——与 `secrets.SwarmSecretRef` 注释「值轮换即换名换引用，轮换天然触发重部署」逐字兑现。
- **compose 形态收窄开放**（受控子集只增方向）：顶层 `secrets:` 仅接受 `external: true` 形态；`file:`/`environment:` 来源拒绝（`E_COMPOSE_UNSUPPORTED`——**值不进 git 仓库**是硬边界）；服务级 `secrets:` 仅短语法或 `{source, target}`（uid/gid/mode 拒绝）。声明名在库中不存在 → 部署 preflight `E_SECRET_NOT_FOUND`（422）。`validate.go` 拒绝清单移除 secrets 两项 = C1 裁决预留口的显式解除（评审门禁，§6）。
- **注入**：planner 经 `app_secrets` 构造 `SecretMount`（SecretName=`fleetly-<app>-<name>-<hash8>`、Target=`/run/secrets/<compose 名>`）——`engine.ServiceSpec.Secrets` 与 substrate 适配**零改动**（管道已就绪，§1 事实 2）。
- 库实例引擎凭据存 `db_instances.credential_cipher`（§2.5），经同一 Swarm secret 管道投递（D-DB-10），与用户 secret 同一加密与轮换纪律。

## 3. 关键裁决表（D-DB-*）

| # | 裁决 | 理由 | 被否方案 | 终裁 |
|---|---|---|---|---|
| D-DB-1 | 库实例 = **独立资源类型**：自有表 `db_instances`、自有 API `/v1/databases`、自有生命周期状态机（§2.1 七态 + 转移表）；组件级复用（放置/卷/substrate/secret/审计/备份/naming），不复制部署管线/EnterPhase/观察窗/回滚 | **用户终裁（2026-09-20）：领域清晰度优先**——库运维语义（provision/暂停/升级/备份门）与 app 发布语义（构建/滚动发布/观察窗/回滚）本质不同，独立状态机远简于部署管线，UI/CLI/API 面以库为一等公民 | app 形态（apps.kind + db_instances 明细）：模型复用最大化（EnterPhase/队列/放置/卷/审计/tombstone 全复用，12-20 人日预算最省），但领域混杂：库运维语义混入 app 生命周期，UI/CLI/API 面被迫走 app 抽象 | 已裁（用户终裁 2026-09-20） |
| D-DB-2 | 模板 = **平台内置 Go 注册表**，版本与镜像 digest 随平台 release 受管；实例升级逐个 opt-in、失败 digest 归位回退（§2.2 受控重建 + 备份门） | 模板耦合备份适配器与凭据规格，是代码不是数据；热更模板 = 动态插件面（D13 禁）；版本偏斜产生未测组合；供应链受控（digest 门禁 R7） | compose 文件模板库热更（插件面 + 任意镜像引用的供应链面）；用户可控镜像（备份/健康门契约失守，Dokploy 式全开放被否） | 已裁（用户确认 2026-09-20，按原案：内置 Go 模板注册表、镜像受管） |
| D-DB-3 | 凭据：创建时生成、age 存 env_vars(source=system)、引用方**物化** `FLEETLY_DB_<NAME>_{URL,HOST,PORT,USER,PASSWORD,DATABASE}` 行（Redis 无 USER/DATABASE）、只读展示、**仅手动轮换**（自动触发引用 app 重部署） | 物化使 ListEnv/revision/desired-hash/pending 语义零改动兑现（envlayer 留位即为此设计）；R5 先例同构；轮换后不重部署引用方 = 必断连，自动化是唯一诚实选项 | 运行时合成不物化（投影 API 与快照链路全要另造）；定期轮换（无告警面配套前是定时炸弹） | 已裁（用户确认 2026-09-20，键名 FLEETLY_DB_<NAME>_* 定案） |
| D-DB-4 | 引用声明 = compose 服务 label `fleetly.databases`（逗号分隔） | 单一真源 = compose（期望态治理规则）；与 fleetly.domains 同载体同风格；plan/diff 天然可见 | API 旁路注册（并行期望态，违反「不建与 compose 并行的期望态」纪律——cron 同款裁决） | 已裁 |
| D-DB-5 | 网络时序：库网络即共享网络、别名 = 实例名；引用前哨只查存在性（`E_DB_NOT_FOUND`）不查就绪（`W_DB_REFERENCE_NOT_READY` 警告）；**有引用禁删**（`E_DB_REFERENCED` 409） | 建库/引用可并行（bootstrap 顺序是用户事务，警告比阻塞诚实）；数据安全前哨与卷-节点 409 同型 | 就绪门禁（部署耦合库健康，级联阻塞）；允许删库留悬空引用（数据事故面） | 已裁 |
| D-DB-6 | 备份：每引擎逻辑备份（pg_dump -Fc / RDB 流式）→ restic 同基础设施独立命名空间；**独立 `db_backups` 台账** + 回读校验；恢复 = 原地 + confirm；跨节点 DR 走 runbook | 见 §2.6 表（各行被否方案已列） | restic 卷快照、并入 state_backups、控制面中转、一键跨实例恢复 | 已裁 |
| D-DB-7 | secrets 开放面 = **所有 app**，external-only 形态，平台密钥库为唯一来源，无值读回 | 机制 app 无关；file: 来源把密钥耦合进仓库；读回面扩大敏感面（忘记即轮换更安全） | 仅库实例开放（第二套语义）；file: 来源；GetSecret 明文读回 | 已裁 |
| D-DB-8 | 错误码/事件：**错误码维持 9 码 + 1 警告（零新增）**；事件统一走自有 `db.*` 族（18 个，§5.3）= 生命周期转移事件 + 操作事件；状态机前置态/CAS 冲突复用 `E_STATE_VERSION_CONFLICT`（409，context 带 current_state 与合法前置态清单——同族乐观冲突语义） | D-DB-1 终裁（独立资源）后库无 app/deployment subject 可挂，生命周期事件必须自有族（2026-09-20 复核修订原裁「复用既有族」）；原案 7 个 db.* 事件全保留并补齐转移事件 | 复用 app.*/deployment.*（独立资源下 subject 错位、无载体）；为状态冲突新造码（E_STATE_VERSION_CONFLICT 已覆盖同族语义）；全部复用 E_COMPOSE_*（域语义错位） | 已裁（2026-09-20 复核修订） |
| D-DB-9 | 库实例 = **用户 app 资源**，不计入 600MB 平台组件 idle 预算；默认限额 PG 1C/1Gi、Redis 0.5C/256Mi（创建可调） | 600MB 红线约束的是平台常驻组件（fleetlyd/Traefik/containerd/zot/VL/RustFS）；库是用户负载，与用户 app 同资源隔离与容量边界口径（§4.2 既有「构建与应用资源隔离」）；备份 job 为瞬态非常驻 | 库计入平台预算（把用户负载算进平台成本，口径失真）；无默认限额（单机雪邦面） | 已裁（口径确认，红线 1 复核通过：E4 零新增常驻组件） |
| D-DB-10 | 库引擎凭据投递：Swarm secret 文件优先（PG `POSTGRES_PASSWORD_FILE` 原生支持）；Redis 走启动参数（spec 明文，与 env 同暴露类，文档明示） | 原生支持 _FILE 的引擎零成本硬化；Redis 无文件形态，硬造 wrapper 是过度设计 | 全 env（PG 有更好形态不用）；全 secret（Redis 需自造 wrapper） | 已裁 |
| D-DB-11 | 回滚对 **source=system 行取当前值**（重放时按 key 从 env_vars 重读），不随 desired_spec 快照回放 | rollback.go 现状按密文快照整体回放合并 env——连接串含密码进快照后，轮换+回滚组合必回放旧密码断连，违反 D-REL-9；v0.1 靠「无密钥库」结构性豁免，E4 落地即失效 | 快照回放（断连事故）；轮换时改写历史快照（快照不可变纪律） | 已裁（对既有实现是**修正项**，随 E4 票据落地并回写发布专项 §2.4 一句限定） |

## 4. v0.2 切面与验收

**主验收闭环**（v0.2 规划 E4 行原文口径）：建库 → app 引用连接串部署 → 备份恢复闭环。E4 对 E3 的依赖仅在备份步（第 6 步）；模板/生命周期/注入/secrets（1-5、7-8 步）不依赖 E3，W3 若滑动可先行。

| 步 | 内容 | 验证方式 | 回滚路径 |
|---|---|---|---|
| 1 | 状态层：volumes 归属泛化重建（owner_kind ∈ {app,database} + owner_id，00008 先例同型：建新→搬行→删旧→改名）+ `db_instances`（含状态机列与转移表）/`db_references`/`app_secrets`/`db_backups` 四新表 + `EnterDbPhase` 单写点 + `E_ENV_KEY_RESERVED` 守卫（SetAppEnv 拒 `FLEETLY_*` 用户写） | 迁移前后快照测试（既有 app 卷行零变化）+ 转移表穷举 + 非法转移拒写负面测试 + env 保留前缀负面测试 | 恢复快照（无 down migration，既有纪律） |
| 2 | 模板注册表 + 渲染器（两模板全字段）+ EngineAdapter 接口 | 渲染确定性 golden 测试（同输入同 ServiceSpec 投影同 desired-hash） | 纯新增包，移除即回零 |
| 3 | 生命周期 API/CLI + provision 收敛器（placement 选点/前哨、卷登记、substrate 服务创建、健康门 → ready；suspend/resume/retry；删除守卫 + reap duty） | dind E2E：建 PG/Redis → ready → 卷登记 + 钉住；suspend → paused → resume → ready；provision 失败注入 → failed → retry → ready | 删实例（卷默认保留）/恢复快照 |
| 4 | 引用与注入：label 解析（查 db_instances）、前缀冲突哨兵、db_references 维护、system env 物化、多网络 NetworkEnsure、`W_DB_REFERENCE_NOT_READY` | E2E：app 以 label 引用 → 部署 → 容器内以 URL 真连接读写 PG；删除守卫 409；**引用方 app 回滚后仍连通（D-DB-11 断言）** | label 移除 + 部署（引用与物化行随之清除） |
| 5 | 轮换：rotate API + 适配器钩子（凭据源 = credential_cipher）+ 引用 app 自动重部署编排 | E2E：写入数据 → rotate → 引用 app 自动重部署 → 旧数据仍在且新连接成功 | 不可逆（审计 + 事件留痕；两段式确认） |
| 6 | 备份/恢复：dbtools 镜像（digest 钉定）+ 备份 job + db_backups 台账 + 调度核接入 + verify + 恢复 API（confirm） | E2E：写入行 → 备份 → verify=verified → 破坏性清空卷数据 → 恢复 → 行断言一致；失败路径红色告警；Redis 同型 | 恢复即逆操作；备份保留窗 prune |
| 7 | 升级：upgrade API（pre_upgrade 备份门 → digest 受控重建 → 健康门 → 失败 digest 归位）+ `db.upgrade_*` 事件 | E2E：升级成功路径；坏 digest 注入 → 归位旧版 → 状态回 degraded + `db.upgrade_failed`；备份门失败 → 实例不动 | digest 归位即内建回退 |
| 8 | secrets 开放：存储 + SecretsService + compose 校验收窄开放 + planner 注入 | E2E：external secret 声明 → /run/secrets 读到值；缺库 `E_SECRET_NOT_FOUND`；file: 拒绝；golden 白名单更新 | 拒绝清单还原（C1 形态） |
| 9 | Console：库实例列表/详情（连接信息脱敏默认、备份列表、升级入口）+ secrets 页 | data-testid 锚点只增 + Playwright 冒烟扩展（沿用 W1 冒烟轨道） | 前端面独立，可单独回退 |

**横切验收**：600MB 复测确认平台组件无新增常驻（dbtools/restic job 为瞬态）；错误码/事件注册表 golden 快照更新过评审（只增门禁）；`buf breaking` 零破坏（新增文件与字段）；MCP 工具面预算核算（E4 相关读写工具 ≤30 预算内计入，E2 时点统一核算）。

## 5. 契约面

### 5.1 proto 加法（`fleetly.server.v1`，全为新增文件/RPC，零 breaking）

```proto
service DatabaseService {
  rpc CreateDatabase / GetDatabase / ListDatabases / DeleteDatabase   // 删除带 confirm + delete_volumes
  rpc SuspendDatabase / ResumeDatabase / UpdateDatabaseSettings       // 限额/备份计划
  rpc UpgradeDatabase                                                 // digest 升级（opt-in）
  rpc RotateDatabaseCredentials                                       // 破坏性两段式
  rpc TriggerDatabaseBackup / ListDatabaseBackups / RestoreDatabaseBackup  // 恢复带 confirm
}
service SecretsService { rpc SetSecret / ListSecrets / RemoveSecret }   // 无值读回
```

`DatabaseView`：name、template、image_digest、status（= 生命周期态，§2.1 状态集）、placement、volume、connection（脱敏投影 + 显式 reveal）、backup_plan、upgrade_available。apps 面零改动（独立资源，无 kind 字段耦合）。

### 5.2 错误码（注册表只增，9 码）

| 码 | HTTP | 语义 |
|---|---|---|
| E_DB_NOT_FOUND | 404 | 引用/操作的目标库实例不存在或已进入 deleting/deleted（附候选清单） |
| E_DB_REFERENCED | 409 | 有引用 app 时禁删（附引用清单） |
| E_DB_TEMPLATE_UNSUPPORTED | 400 | 模板 ID 未知 / 设置违反模板受管面 |
| E_DB_ENV_PREFIX_CONFLICT | 422 | 同 app 引用的多库 env 前缀撞名 |
| E_DB_BACKUP_FAILED | 500 | 备份 job 失败（资源终态类，主呈现于台账与事件） |
| E_DB_RESTORE_FAILED | 500 | 恢复失败（原地恢复中断即 critical 告警） |
| E_DB_ROTATE_FAILED | 500 | 轮换中途失败（附已完成阶段，人工收尾） |
| E_SECRET_NOT_FOUND | 422 | compose 声明的 external secret 不在库（preflight） |
| E_ENV_KEY_RESERVED | 422 | 用户写 `FLEETLY_*` 保留名字空间 |

警告码：`W_DB_REFERENCE_NOT_READY`（引用的库未就绪/已暂停/failed，计划警告不阻塞）。状态机前置态/CAS 冲突复用既有 `E_STATE_VERSION_CONFLICT`（409，context 带 current_state 与合法前置态清单——同族乐观冲突语义；D-DB-8 复核后零新增码）。

### 5.3 事件（18 个，只增；D-DB-8 复核后统一自有族）与审计

转移事件（`EnterDbPhase` 单写点随转换同事务落）：`db.provision_started`、`db.ready`、`db.provision_failed`、`db.degraded`、`db.recovered`、`db.suspended`、`db.resumed`、`db.delete_started`、`db.deleted`。操作事件（不换主状态）：`db.upgrade_available`、`db.upgrade_started`、`db.upgrade_finished`、`db.upgrade_failed`、`db.backup_succeeded`、`db.backup_failed`、`db.restore_completed`、`db.restore_failed`、`db.credentials_rotated`。不复用 `app.*`/`deployment.*`（独立资源无对应 subject）。审计动作：`db.create/retry/suspend/resume/upgrade/rotate/backup_trigger/restore/delete`、`secret.set/removed`（human/ai_agent；reap、digest 归位等 system 自动动作照「自动动作必入审计」纪律）。

### 5.4 表、命名与配置

迁移两条（只加法纪律；重建表走 00008 先例「建新→搬行→删旧→改名」，无外键引用者无连带）：① `volumes` 归属泛化重建——`owner_kind TEXT NOT NULL DEFAULT 'app' CHECK (owner_kind IN ('app','database'))` + `owner_id`，唯一键 (owner_kind, owner_id, key)；既有 app 卷行零语义变化（卷登记/孤儿/丢弃/命名防代际全复用，§2.1 复用清单）。② 新表四张：

- `db_instances`：id、name UNIQUE、template、image_digest、settings（限额/备份计划 JSON）、credential_cipher（age 密文）、credential_updated_at、platform_node_id（绑定内嵌，不写 placements 表）、state（§2.1 状态集）、created_at/updated_at/deleting_at/deleted_at（tombstone 时间戳）。
- `db_references`：db_id REFERENCES db_instances、app_id REFERENCES apps、service、env_prefix，PRIMARY KEY (db_id, app_id, service)——引用方 compose 派生的倒排登记（可重建）。
- `app_secrets`：id、app_id REFERENCES apps、name、value_cipher（age 密文）、hash8、created_at/updated_at，UNIQUE (app_id, name)。
- `db_backups`：id、db_id REFERENCES db_instances、kind ∈ {daily, manual, pre_upgrade}、restic_snapshot（repo 内寻址，非文件路径）、size_bytes、verify_status 三态、error、created_at。

naming 新增库族公式（新增函数非改既有公式，本文档为文档锚）：`DBServiceName = fleetly-db-<name>-<service>`、`DBNetworkName = fleetly-db-<name>-net`、`DBVolumeName = fleetly-db-<name>-<key>-<id8>`、`DBSecretName = fleetly-db-<name>-<secret>-<hash8>`——与 app 名族 `fleetly-<app>-*` 解耦（§2.1 名字空间独立）。配置键：`databases.backup_interval_hours=24`、`databases.backup_keep=7`、`databases.backup_hour_utc=3`（平台缺省，实例可覆盖）。CLI：`fleetly databases <create|get|list|delete|suspend|resume|upgrade|rotate|backup|restore|show>`、`fleetly secrets <set|list|rm>`。

## 6. 与既有文档一致性

| 面 | 一致性核对 |
|---|---|
| 单写点/轻单核 | 库状态机自有单写点 `EnterDbPhase`（EnterPhase 同款纪律：同事务转移校验 + CAS + 事件，§2.3）；独立资源**不复制**部署队列/EnterPhase/观察窗/回滚（D-DB-1 终裁推论）；零新增常驻组件（红线 2 与 600MB 复核通过，D-DB-9）；模板为代码内注册表非动态插件（D13） |
| 只增纪律 | 错误码/事件/迁移/白名单全为加法；**拒绝清单移除 secrets 两项 = C1 裁决预留口的显式解除**（validate.go 注释原文「v0.2 平台密钥库接入后解除」预授权；走显式评审 + golden 同步，非静默减项） |
| 快照回滚语义 | 引用方 app 的 revision 含三层合并结果（system 物化行 hash 已自然入快照）；**D-DB-11 修正**：回放对 source=system 行按 key 取当前值——本设计对发布专项 §2.4 的限定补写（「合并 env 按快照」增加「source=system 行除外，取当前」），实现载体 rollback.go 重放路径随 E4 票据改；库实例自身无 revision/无回滚（升级失败 = digest 归位，D-DB-1 终裁推论）；卷数据/库内容不回滚（既有） |
| 诚实契约 | 备份 verify 三态 + 失败红色告警；同节点 RustFS 库备份标注「便捷层非灾备」（V2-2 延伸）；suspend/未就绪引用如实警告；Redis 凭据 spec 明文与 overlay 网内不加密为已知边界（对齐架构 §2.3「Swarm spec env 明文」边界族，文档明示） |
| 安全默认 | 库不发布 host 端口、无 fleetly.domains 即不进路由（「数据库默认不暴露公网」由构造满足）；凭据 age 密文；值不进事件/审计/日志（负面测试随票） |
| 术语 | 新词条待入 UBIQUITOUS_LANGUAGE：**库实例 database instance / 模板 template / 引用 reference（数据库语境）/ 库备份 database backup（与 backup=控制面快照分立）/ 库恢复 database restore（restore 裸词仍仅指控制面 DR——本设计用带限定词第二义，需术语表裁决收录）/ 平台密钥库 platform secret store / provisioning 收敛（库实例状态机的重收敛态）** |
| 依赖链 | E3（restic/S3 目标）为备份步前置（W4 排序已含）；E5 调度核共享（cron 细则同款「与平台热备、数据库备份共用同一调度核」）；放置/卷/rebind 全复用放置专项 |

**发现的既有冲突（如实记录）**：① rollback.go 现状「合并 env 按密文快照回放」与发布专项 D-RL-9「secret 值取当前」在密钥库接入后冲突（v0.1 靠无密钥库结构性豁免）——D-DB-11 修正；② env.proto:27 注释「合并链只消费 effective 行」与 env.go/envlayer 实现（S16-C4：pending 参与合并）不一致——既有文档注释层小冲突，与本设计无直接关系，随 E4 票据顺手订正；③ 词汇表 backup/restore 词条与本设计的库备份/库恢复撞词——以带限定词新词条化解，待术语表裁决。

## 7. 明确不做

- ~~MySQL/MongoDB 模板~~（紧随按需求排序——各引擎备份适配器是主要成本，架构 §4.3；**v0.3 W4 兑现：见 §8**）
- 主版本升级（PG 16→17）与引擎切换迁移路径
- 读写分离/副本/库层 HA（有状态 HA 边界口径不变：库所在节点失联 = 该库不可用，恢复走备份重放 + rebind）
- 库内多 database/多用户管理、RBAC、计量计费、多租户（v0.2 规划 §8）
- 定期自动轮换凭据（仅手动）
- 一键恢复到新实例/跨节点自动 DR（runbook 化：建新库 + 手动重放 + rebind CLI）
- 库暴露公网/TLS 入口（网内不加密为缺省口径）
- 连接池/代理组件（pgbouncer 类）
- 纳管用户自建外部库（adopt）
- compose `secrets` 的 `file:`/`environment:` 来源与 uid/gid/mode 子键；secret 值读回 API
- 库实例自动换点/自动迁移（钉住纪律，D16）；用户自由跨 app 网络（V2-5）

## 8. v0.3 W4 模板扩展：MySQL 8.4 / MongoDB 8.0（2026-09-24 增补）

**背景**：§7 首行「MySQL/MongoDB 模板（紧随按需求排序）」在本轮排期兑现（v0.3 W4）。裁决参数由主会话给定（2026-09-24），照录为 D-W4-* 行——实现严格沿既有模板/适配器模式（§2.2/§2.5/§2.6），零新增机制面。分票口径：S1 = 模板 + 适配器脚本与断言（本节）；S2 = dbtools 镜像扩展（加装 mysqldump/mysql/mongosh/mongodump/mongorestore）；S4 = e2e 真跑腿。

### 8.1 裁决行（D-W4-*）

| # | 裁决 | 落地注记 |
|---|---|---|
| D-W4-1 | 模板 `mysql-8.4`（MySQL 8.4 LTS）；引擎镜像 `mysql:8.4` 按实现时点解析钉 digest，台账登记（runbook image-prepull §1） | digest = `mysql:8.4@sha256:0744ee5ef89ce6ccfa13de3e579fe6b9e27f93dd70da9c06d2c908b1b193fb8d`（多架构 OCI index，amd64/arm64 双平台按同一 digest 独立拉取复核通过，2026-09-24；tag@digest 双锚与 postgres-16 同款） |
| D-W4-2 | 模板 `mongodb-8.0`（MongoDB 8.0 Community）；引擎镜像 `mongo:8.0` 同上钉定 | digest = `mongo:8.0@sha256:4968f22d0c6c10ef29952f3e807f62872ba22b3312f25803564fbfc08255efc2`（同上双验，2026-09-24） |
| D-W4-3 | 备份适配器：mysql = `mysqldump --single-transaction --source-data=2`（InnoDB 一致性快照）恢复 `mysql < dump.sql`；mongo = `mongodump --archive --gzip` 恢复 `mongorestore --archive --gzip --drop`。工具由 dbtools 镜像承载（**S2 扩展镜像**；S1 只写适配器脚本与断言，e2e 真跑依赖 S2 新镜像——S1 的 e2e 范围 = 既有 postgres/redis 腿不回归破坏） | 单 job 形态沿 PG 先例（恢复 = restic 取回 + 临时引擎实例重放同 job）：临时 mysqld/mongod 须在 dbtools 镜像内——S2 的镜像基底扩为多引擎工具面（PG/Redis 现有工具面保留） |

连接串投影（§2.5 键集表扩两行；`<NAME>` = 实例名大写下划线形，USER/DATABASE 与 PG 同构——USER=fleetly 固定、DATABASE=实例名 `'-'→'_'`）：

| 引擎 | 物化键 | URL 形态 |
|---|---|---|
| MySQL | `FLEETLY_DB_<NAME>_{URL,HOST,PORT,USER,PASSWORD,DATABASE}`（PG 全键同构） | `mysql://fleetly:<pw>@<实例名>:3306/<db>` |
| MongoDB | 同上全键 | `mongodb://fleetly:<pw>@<实例名>:27017/<db>?authSource=admin` |

**MongoDB authSource 实现注记（对裁决格式的必要补充）**：裁决给定的投影骨架为 `mongodb://user:pass@<name>:27017/<db>`；官方 mongo 镜像入口脚本把 initdb root 用户**恒建于 `admin` 库**（docker-entrypoint.sh: `rootAuthDatabase='admin'` 硬编码，8.0 无 `MONGO_INITDB_ROOT_DATABASE` 支持），驱动的 authSource 缺省取 URI path 库 → 无 `?authSource=admin` 时认证必败。故投影以追加 `?authSource=admin` 的形态兑现裁决（最小改动：不动入口脚本、不自造 init script 造第二用户——两者都是新机制面）。密码字符集 [a-zA-Z0-9]（GeneratePassword 共用），URI 免 percent-encode；`/`、`?`、`@`、`:` 均不出现，query 参数位置无注入面。

### 8.2 模板规格表（§2.2 表的扩两列）

| 字段 | mysql-8.4 | mongodb-8.0 |
|---|---|---|
| 镜像 | `mysql:8.4@sha256:0744ee5e…3fb8d`（D-W4-1） | `mongo:8.0@sha256:4968f22d…5efc2`（D-W4-2） |
| 引擎内部端口 | 3306 | 27017 |
| 卷 | key=`data`，挂 `/var/lib/mysql` | key=`data`，挂 `/data/db` |
| 凭据规格 | USER=fleetly、DATABASE=实例名 `'-'→'_'`、密码 32 位 [a-zA-Z0-9]（与 PG 同构——凭据只存一份）；root 密码 = 同一凭据值（`MYSQL_ROOT_PASSWORD_FILE` 指向同一 secret 文件——root 不在投影/用户面暴露，仅满足官方镜像 initdb 必填；轮换只动 `fleetly@'%'`，root 恒为 initdb 值且恢复路径无需它，见 8.3） | 同左（`MONGO_INITDB_ROOT_USERNAME=fleetly`；`MONGO_INITDB_DATABASE=<db>` 声明 initdb 缺省库；库本体由首写惰性创建——MongoDB 语义） |
| 凭据投递 | **Swarm secret 文件**（`MYSQL_PASSWORD_FILE`/`MYSQL_ROOT_PASSWORD_FILE`——官方入口 `file_env` 原生支持 _FILE 变体，镜像内 grep 实证；CredentialDelivery=secret-file，与 PG 同管） | **Swarm secret 文件**（`MONGO_INITDB_ROOT_PASSWORD_FILE`——官方入口 `file_env` 原生支持，同上实证） |
| 健康门（D-W4 裁决逐字） | `mysqladmin ping`（CMD 形态；服务应答即存活——官方镜像缺省探针；`mysqladmin` 对「应答但拒认证」的活服务器返回 0，无假阴性，与 §2.2 Redis 行的 NOAUTH 假成功不同类：Redis 是每命令全 NOAUTH，此处 ping 本身可达即判真） | `mongosh --quiet --eval db.adminCommand('ping')`（CMD 形态；`ping` 在 MongoDB 认证开启时属免认证命令白名单，连通即真值） |
| 节奏 | 5s/3s/3/30s（与 PG/Redis 缺省同） | 同左 |
| 默认限额 | 1.0 CPU / 1GiB（沿用 D-DB-9 PG 档——W4 裁决未单列，创建时可覆盖；MySQL 8.4 与 WiredTiger 在 1GiB 内以缺省配置可运行） | 同左（实现者保守取值，见遗留） |
| 连接串渲染 | 见 §8.1 投影表 | 同左（含 `?authSource=admin`） |

模板注册表键与 proto 注释/Console 选项/CLI help 同步（CreateDatabase 的 template 词表 = postgres-16 / redis-7 / mysql-8.4 / mongodb-8.0；`E_DB_TEMPLATE_UNSUPPORTED` 词表面零改动）。

### 8.3 备份适配器命令面（§2.6 的引擎实现扩充）

repo 内路径：`db/<instance>/db.sql`（mysqldump 文本）与 `db/<instance>/db.archive`（mongodump 归档）；restic 管道/summary 解析/路径过滤 prune/verify 三态与既有引擎逐字共用。

| 步 | MySQL | MongoDB |
|---|---|---|
| Backup | `mysqldump -h <实例> -u fleetly --single-transaction --source-data=2 <db> \| restic backup --stdin --stdin-filename db/<实例>/db.sql --json` | `mongodump --host <实例> --db <db> --archive --gzip \| restic backup --stdin --stdin-filename db/<实例>/db.archive --json` |
| Verify | restic dump 回读头部 `grep -q "MySQL dump"`（mysqldump 文件头魔术串） | restic dump 回读头部 gzip 魔术 `1f 8b`（`od -An -tx1` 十六进制比对） |
| Restore（停库重放，单 dbtools job 挂卷 rw） | 快照落卷根暂存 → 临时 mysqld 起于数据卷（`--skip-grant-tables --skip-networking`〔后者由前者自动蕴含〕+ socket-only + gosu 降权——与 PG「socket trust + 降权」同暴露类：无网络监听、容器内瞬态）→ `DROP DATABASE IF EXISTS` + `mysql --socket=… < dump.sql` → `mysqladmin shutdown` → 暂存清场 | 快照落卷根暂存 → 临时 mongod 起于数据卷（缺省无 `--auth` = 本进程内免认证，`--bind_ip 127.0.0.1` 锁回环——与 PG socket trust 同类：授权是进程旗标非卷内持久态）→ `mongorestore --host 127.0.0.1 --archive=<暂存> --gzip --drop` → `db.adminCommand({shutdown:1})` → 暂存清场 |
| RotateCredential | 一次性容器（引擎镜像）`mysql -h <实例> -u fleetly -e "ALTER USER 'fleetly'@'%' IDENTIFIED BY '<new>'"`，旧密码经 `MYSQL_PWD` env（沿 §2.5 PG 轮换原语形态：引擎级热换、失败即未变、CAS 落库在后；官方镜像建 `USER@'%'`） | 一次性容器（引擎镜像）`mongosh "mongodb://fleetly:${MONGO_PASSWORD}@<实例>:27017/admin" --quiet --eval "db.getSiblingDB('admin').updateUser('fleetly', {pwd: '<new>'})"`——认证与 updateUser 同在 admin（root 恒建于 admin，§8.1 注记）；旧密码经 job env、新密码进 eval 字面量（[a-zA-Z0-9] 无引号 hazard，PG 同款） |

明文纪律沿既有负面测试面：密码只进 job env（新增 `MYSQL_PWD`/`MONGO_PASSWORD` 键）与运行时 shell 展开（`${MONGO_PASSWORD}` 引用形态——字面量不进 job spec/命令词表，与 Redis 健康门 env 引用同暴露类）；命令词表零明文。恢复的凭据语义边界沿 §2.2 W4-S5 落地注记（重放备份时刻的库内密码，错位以 `reveal` 对账 + 再轮换收口——skip-grant/免认证临时实例不读取卷内凭据，边界不受影响）。

**挂账（跨票）**：S2 dbtools 镜像扩展（多引擎工具面 + digest 钉定 + 台账行更新）；S4 e2e mysql/mongo 真跑腿。本票登记的镜像台账两行只覆盖引擎镜像；dbtools 行的 digest 更新归 S2。
