# CONTEXT — fleetly 领域与架构上下文

> 架构评审（`/improve-codebase-architecture`、`/codebase-design` 等）的入口文件。
> 职责分工：**命名纪律的唯一真源是 [UBIQUITOUS_LANGUAGE.md](UBIQUITOUS_LANGUAGE.md)**（词条/别名/语族审查），本文件不重复其词条——只补架构层语义、模块地图与评审须知；**不可翻案的承重决策在 [docs/adr/](docs/adr/README.md)**。
>
> 收录：2026-09-29（源自 19 份设计/规划文档 + git 历史裁决记录的收拢）。

## 1. 产品形态

fleetly 是轻量级自托管 PaaS：**单二进制 `fleetlyd`（Go）+ SQLite 单写者权威态 + Docker Swarm substrate**，对标 Dokploy 的体验地板、Cloudflare 的方向（对标纪律见 ADR-0008）。console 是 React 前端（静态 dist，可进镜像可宿主遮蔽）。控制面有容器形态与原生形态双轨（ADR-0013）。

torchwood/messageloop 迁移线（T 线）是立项原因与最高优先级输入——fleetly 要补的核心能力是「程序化工作负载面」（Tasks API / build-from-upload / 项目网互通），不是函数运行时。

## 2. 领域词汇 → seam 的映射

命名词条（deploy/release/rollback/replay/revision/verdict/drift/converge/reconcile/ledger/audit/event/cursor/placement/binding/anchor/pin/ingress/route/entrypoint……）见 UBQUITOUS_LANGUAGE.md。这些词在代码里的落点：

| 词族 | 落点（seam 所在） |
| --- | --- |
| deploy/release/rollback/replay/verdict | `internal/engine`（发布管线）；转移表真源在 `internal/state/machine.go`，engine 只留词表并委托 |
| drift / converge / reconcile | `internal/engine`（drift.go 检测 / converge.go 收敛原语 / 对账扫描）；「对账」另含 substrateRecon（DB↔Swarm 周期对账） |
| ledger / audit / event / cursor | `internal/state`（Outbox 同事务 + `EventsSince` 游标；断档显式 410） |
| placement / binding / anchor / pin | `internal/placement`（Resolver）+ `internal/naming`（锚定 label） |
| ingress / route / entrypoint | `internal/ingress`（Traefik 路由面） |
| token / principal / scope | `internal/api`（proto option 唯一源，ADR-0011）+ `internal/secrets` |

## 3. 架构层词条（UBIQUITOUS_LANGUAGE 之外的架构语义）

| 词条 | 定义 | 落点 |
| --- | --- | --- |
| **substrate** | Swarm/Docker 适配层；moby 类型止步于此，`engine` 只见 `ServiceSpec/ServiceState` 投影 | `internal/substrate` |
| **权威态 / 派生缓存 / 实时直读** | 三层状态模型：SQLite 权威（期望态/历史/凭证）＋观测快照（禁用于决策）＋写前直读（冲突 409） | `internal/state`（ADR-0002） |
| **写点（write point）** | 状态机转移的唯一写入函数：转移表校验+CAS+tombstone 时间锚+事件/审计同事务，四件一拍 | 如 `EnterPhase`/`SetAppSuspended`（ADR-0002） |
| **权威位（authoritative bit）** | 用户意图的权威态列（`paused`/`suspended`）；派生词表第一判短路——用户权威位直投影压倒观察态 | `internal/state` + `engine/recovery.go` |
| **收敛原语（convergence primitive）** | 服务收敛的唯一实现：缺失建+desired-hash 标戳+重申 switch 全部在原语内完成（调用方不可能忘打标戳） | `internal/engine/converge.go` |
| **convergescan 白名单** | 「非收敛对账」站点的登记表（scaleToZero/autoscaling/initjobs/suspend 排水族）；白名单条目不再命中即红 | `internal/engine/convergescan_test.go` |
| **排水保持器（drain keeper）** | `drainSuspendedApp`：挂起期把受管长驻服务无条件压 0（服务对象保留），非收敛对账 | `internal/engine/suspend.go` |
| **对账（recon）** | substrateRecon：DB↔Swarm 周期核对，「错误≠缺失」；T 线扩到 networks/tasks 双向 | engine + `internal/substrate` |
| **披露（disclosure）** | 持续形态只报一次、恢复清零、事件+审计同事务、节拍门——全引擎一份骨架 | `internal/engine/disclosure.go` |
| **归属（ownership）** | 三段限定形 `fleetly-<team>-<prj>-<app>` 的过滤单点；手写 label 过滤被守卫禁止 | `internal/engine/ownership.go` + `internal/naming` |
| **受管组件（managed component）** | 平台自部署的平台级服务（ingress/zot/VictoriaLogs/vmalert/rustfs/metrics/exec-relay），各自有部署与收敛循环 | `internal/{ingress,imageregistry,victorialogs,rustfs,metrics,execrelay}` |
| **dutydocker** | 受管组件部署器/收敛 duty 对 Docker API 的共享消费面：`ServiceSnapshot` 投影超集 + 幂等 ensure/remove 原语的唯一实现（2026-09-29 架构评审 C1 收编——此前六包各持一份逐字拷贝的 realDockerClient；各包保留窄端口与哨兵，moby 止步于此与 substrate 两处） | `internal/dutydocker` |
| **守卫（guard test）** | 枚举/源码扫描/AST/docs 扫描型红线测试；「穷尽性从提交者记性搬进 CI 枚举守卫」 | 各包 `*_test.go`（清单见 §5） |
| **task-group 网** | Tasks 函数实例的租户项目长活网络；per-task 动态 attach 明令禁止 | `internal/engine/projectnetwork.go`（ADR-0010） |
| **Outbox** | 事件与业务写同事务落 `events` 表；通知投递器经游标轮询，不建进程内总线 | `internal/state/events.go` |

## 4. 模块地图（核心后端）

| 包 | 职责 | interface 所在 |
| --- | --- | --- |
| `state` | SQLite 权威态+观测缓存+事件/审计/迁移；四套状态机写点 | `Store`/`Tx` 门面（store.go，按实体分 50+ 文件） |
| `engine` | 发布引擎：状态机/收敛原语/健康门/观察窗/漂移/伸缩/回滚 | `Engine` 构造器+`With*` 可选端口（engine.go / ports.go） |
| `substrate` | Swarm/Docker 适配器 | 隐式实现 engine 端口（services.go / networks.go） |
| `dutydocker` | 受管组件 duty/部署器的 Docker API 共享消费面（六包适配拷贝的收编单点，2026-09-29 C1） | `Client` + `ServiceSnapshot`（dutydocker.go / service.go / objects.go） |
| `api` | gRPC + grpc-gateway 服务面：proto→state/engine 编排 | genproto 契约 + 包内消费端口 15 个 |
| `runtime` | composition root：Bootstrap + Wire 装配全部服务 | provides.go（api 端口绑定/跨模块桥的内联 adapter 属装配本职；engine 端口的适配已归属主包——substrate/ingress.RoutePublisher/dbtemplate.EngineTemplatePort/metrics，2026-09-29 C6） |
| `placement` | 有状态放置解析/绑定生命周期 | `Resolver.Resolve/Apply/Preflight` |
| `ingress` | Traefik 路由面（每节点 global+集中下发） | 自有 dockerClient 端口；实现 `engine.RoutePublisher` |
| `database` | 托管数据库「第二引擎」：自有收敛循环+`EnterDbPhase` | adapters.go |
| `compose` | compose 受控子集解析/校验/归一 | validate.go / normalize.go（ADR-0015） |
| `build` | buildkit 构建管线 | `Executor/RegistryClient/ImageSource` 端口 |
| `naming` | 对象命名与最小 label 集唯一定义点（纯函数） | naming.go（ADR-0009） |
| `errcode`/`eventcode`/`apperr` | 错误码/事件名注册表（只增）+错误信封 | codes.go / events.go（ADR-0011） |
| `gitserver` | webhook 部署触发 daemon（验签/防重放/去重/拉源；push 收包面已移除，ADR-0012） | config.go（ADR-0012） |
| `logs`+`victorialogs` | 日志管线（ingest+脱敏）+VL 入湖 adapter | `Port/IngestBackend/SecretValuesSource` |
| `execrelay`/`execrun` | Web 终端反向连接 hub/relay + 子进程生命周期 | `TaskSource`/`MessageConn` |
| `secrets`/`envlayer`/`dbtemplate`/`imageregistry`/`objectstore`/`statebackup`/`notify`/`acmedns`/`cron`/`metrics`/`rustfs` | 平台支撑件（加密 box/变量三层合并/DB 模板/registry 客户端/S3 客户端/热备/通知/DNS-01 双 adapter/定时任务/指标/S3 存储） | 各自有自有端口 |

入口：`fleetlyd`（daemon 薄入口，装配在 `internal/runtime.Bootstrap`）、`fleetly`（CLI，40+ 命令文件带 golden 测试）、`fleetly-exec`（终端 relay 独立 main）。

## 5. 核心不变量（详证与不可翻案理由见 ADR）

1. **三层状态模型**：派生缓存禁用于决策；nodes 不承诺「最后心跳」（ADR-0002）。
2. **写点四件一拍**：转移表校验+CAS+tombstone 锚+事件/审计同事务；审计 fail-closed（ADR-0002）。
3. **收敛只在原语**：`TestNoConvergenceOutsidePrimitive` 白名单纪律（ADR-0011）；检测默认开、收敛 opt-in、挂起不得被静默撤销（ADR-0004）。
4. **注册表只增不复用**：errcode/eventcode 构造期 panic + golden + usage 扫描三链咬合（ADR-0011）。
5. **Compose 白名单只增不减**；受管字段拒绝不静默覆盖（ADR-0015）。

**守卫测试清单**（动这些面时白名单要保鲜）：`TestNoConvergenceOutsidePrimitive`、`TestNoHandWrittenAppLabelFilters`、`TestNoBareTaskStatusWrites`、`TestOwnershipAnchorConstantsCoveredByPredicate`、`TestMethodScopeRegistryCoversDescriptor`、`TestActiveImplDocsReferenceExistingTests`（impldocscan）、`TestGoldenSnapshot`（errcode/eventcode）、usage 反向扫描、投影契约腿（`testsupport.ServiceProjectionPairs` 契约表——engine fake 投影面与 substrate 纯翻译层咬同一张表，spec/state 加字段不入表即红）；CI 级另有 deadcode 门禁（豁免需持理由入 `internal/testdata/deadcode-allow.txt`）、antipattern-grep、buf breaking、wire generate-sync、console schema.d.ts 漂移门。

## 6. 架构评审须知

- **红线**：docs/adr/ 收录的决策不再重新论证。若评审候选与 ADR 冲突且摩擦真实，须在候选卡上明标「建议重开 ADR-xxxx」并给出新证据——不悄悄翻案。
- **挂账区**：v0.3 计划已排项（见 docs/plan/2026-09-23-v0.3-plan.md 与各设计文档挂账表）不算新发现；评审前先对照。ADR 状态为「已接受（挂账）」的项同理。
- **命名**：任何新概念先查 UBQUITOUS_LANGUAGE.md 是否已有词条/别名禁令；架构评审产出的新模块名若不在词表，评审通过后补词条（发明词前先看 §3 是否已覆盖）。
- **两份真源文件**：`docs/design/`（按日期的设计文档，含 D 编号决策链）是历史决策的完整证据；ADR 是其收拢索引——ADR 引用设计文档编号，不复制全文。冲突时以最新裁决为准（T-3 部分推翻 T-1 即先例）。
