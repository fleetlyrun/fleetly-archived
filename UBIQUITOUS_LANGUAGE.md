# fleetly 词汇表（Ubiquitous Language）

> 2026-09-19 命名审查产物：核心概念的语族一致性审计 + 领域词汇钉死。
> 语族缩写：**拉**=拉丁/法语系 · **日**=日耳曼系 · **希**=希腊系 · **混**=混合词源。

## 发布生命周期

| 词条 | 定义 | 语族 | 别名（避免） |
| --- | --- | --- | --- |
| **deploy** | 一次部署：compose 入队到终态的全链 | 拉 | apply（CLI 面已废）、release（仅指切流阶段） |
| **release** | 发布阶段：任务替换到健康门的切流窗口 | 拉 | rollout |
| **rollback** | 平台按版本快照的单层重放回滚 | 混（roll 拉 + back 日） | revert、downgrade；中文统一「回滚」（「回退」留给升级/raft 语境） |
| **replay** | 同一快照内容的再次部署（幂等，通常零任务变动）；deployment 同记录归位值即 `recovery=replay` | 混（re 拉 + play 日） | redo |
| **restore** | 灾难恢复：从备份快照重建控制面状态（**仅此一义**；部署归位值不得再用 restore） | 拉 | recover |
| **revision** | 已验证的版本快照（归一化 compose + 覆盖层，保留 5 版；中文「版本快照」） | 拉 | version、snapshot（快照泛称，勿代指 revision；用户面文案不得用 snapshot/version/revert 命名它） |
| **verdict** | 部署终态的稳定性判定（healthy/unstable/degraded…） | 拉 | outcome、result |
| **drift** | 运行域与期望态的偏离（检测默认开） | 日 | divergence（保留 drift——GitOps 事实标准词） |
| **converge** | 漂移的显式收敛动作（per-app opt-in） | 拉 | sync、heal |
| **reconcile** | 对账器职责命名空间：stack 对账（底座实况增删改核对）+ 漂移检测事件前缀 `reconcile.*`；drift/converge 是检测与收敛动作词 | 拉 | 对账的英文正式词；sync |

## 时间性安全层（L1/L2/L3）

| 词条 | 定义 | 语族 | 别名（避免） |
| --- | --- | --- | --- |
| **health gate** | L1：任务健康才许切流（Traefik 不查健康，控制面补位）；同形名须带限定：healthcheck（compose 探针）/ grpc health（协议探针）/ CheckHealth（组件自检） | 日 | readiness（指探针，勿混用） |
| **watchdog** | L2：发布看门狗（`deployTimeout`，配置键 `engine.deploy_timeout_seconds`；2026-09-20 由 releaseTimeout 更名——它覆盖 preparing/building/releasing 全链预算，不专属切流阶段） | 日 | timeout 泛称 |
| **observe window** | L3：切流后观察窗（默认 60s 只告警） | 拉 | cooling、soak |

## 触发与数据流

| 词条 | 定义 | 语族 | 别名（避免） |
| --- | --- | --- | --- |
| **trigger** | 部署入口：webhook（投递触发拉源；git push(SSH) 收包面 2026-09-29 移除，ADR-0012） | 日（借拉丁 trigga） | — |
| **delivery** | 一次 webhook 投递（ID 是防重放键；代码用「投递」）；设计文档 delivery pipeline 是 CI/CD 交付流水线泛称（中文「交付」），非本词条 | 拉 | — |
| **fetch** | 从远端仓库拉对象到平台 bare 仓库 | 日 | pull（pull 专属镜像拉取） |
| **source**（git） | 应用的代码来源配置（URL/分支/认证） | 拉 | remote（指 git 语义的远端） |
| **source**（logs） | 日志行的产生端：`container` \| `build` | 拉 | producer、origin |
| **source**（env） | 环境变量的层来源：`env_file` \| `environment` \| `platform` | 拉 | layer 单独指层序，source 指来源标记 |
| **source_git**（deployments） | 部署产地的 git 溯源列（sha/ref） | 拉 | — |
| **follow** | 日志实时跟随流 | 日 | tail |
| **watch** | 事件流订阅（seq 游标） | 日 | subscribe |
| **history** | 日志/事件的落盘历史检索 | 希→拉 | archive |

## 状态与信任

| 词条 | 定义 | 语族 | 别名（避免） |
| --- | --- | --- | --- |
| **ledger** | 台账：append-only 的登记表（domains/backups/certs） | 拉 | 表名直译、record |
| **audit** | 审计：与业务写同事务的不可抵赖记录 | 拉 | log（泛指日志，勿混） |
| **event** | 事件流的单条广播（seq 单调，游标续读；元数据条目 `eventcode.Event` 与实例 `state.Event` 分属注册表/运行数据，不得互称） | 拉 | — |
| **snapshot**（泛义） | 通用快照泛称：desired_spec / DB 备份 / ingress 配置代次 / 日志 ring 快照各自带限定词；**用户面不得用 snapshot 命名 revision 或 rollback** | 日 | 代指 revision |
| **cursor** | 流式读取的位置令牌（410 过期） | 拉 | offset |
| **prune** | 按保留期的删除（janitor/backups）；孤儿目录回收 `prune_orphan`（24h 宽容期）是保留期删除的扩展——两者都不是 sweep | 拉 | purge |
| **sweep** | 周期扫描轮（**仅** ingress 收敛/续期扫描语义） | 日 | 见 flagged ①：缓存过期删除不得称 sweep |
| **backup** | 控制面状态快照（VACUUM INTO + 回读校验入台账） | 日 | dump |
| **envelope** | age 信封加密形态 | 拉 | — |
| **token** | Bearer 凭据（scope 三级）；基础设施同形名必须带限定词：bootstrap admin token / ingress config bearer / ACME challenge token（RFC 词）；hook token 随 git push 面 2026-09-29 退役（ADR-0012） | 日 | key（裸词在 secrets 语境专指 age 主密钥；env/volume 键必须带限定词：env key / volume key） |
| **principal** | 通过鉴权的调用方身份 | 拉 | — |

## 入口与放置

| 词条 | 定义 | 语族 | 别名（避免） |
| --- | --- | --- | --- |
| **ingress** | 平台自管的 Traefik 入口层 | 拉 | gateway（专指 grpc-gateway） |
| **route** | 下发给 Traefik 的单条路由 | 拉 | rule |
| **entrypoint** | Traefik 监听面（web/websecure；中文「监听面」，勿与 ingress 的「入口」混用） | 拉 | listener |
| **challenge** | ACME 验证挑战（HTTP-01 经反代） | 拉 | — |
| **placement** | 放置域：服务落哪个节点的决策 | 拉 | scheduling（K8s 语，勿借） |
| **binding** | 钉住的登记关系（placement binding） | 日 | constraint（专指调度约束编译产物） |
| **anchor** | 节点身份锚定（平台 ID → Swarm label；中文统一「锚定」，「锚写」废止） | 拉 | stamp |
| **pin** | 有卷应用钉住到本机节点（不迁移）；镜像 digest 钉定为内部实现义（`pinDigest`），不得简称 pin | 日 | — |

## 审查结论（2026-09-19）

**input/sink 型「同一概念对混族」反例：不存在。** 全库 `sink`/`upstream`/`downstream`/`producer`/`consumer` 零命中；`input` 仅 9 处且全部是 `placement.Input` 参数结构（与 `Decision` 构成代码层 in/out 对，自洽）；`output` 53 处全部是 CLI help 文案与局部变量，非领域概念。

**Flagged ambiguities（按重要性）：**

1. **sweep 双义**〔已改（2026-09-19）〕：`gitserver.deliveryCache.sweep` 已更名 **expire**（TTL 惰性过期删除）；sweep 专指周期扫描轮（ingress 收敛/续期）。
2. **source 四义**〔部分已改（2026-09-19）〕：git 来源（source_url）/日志来源（container|build）/env 层来源/部署溯源（source_git_*）四义仍并存——各自有 enum 与字段前缀限界，属可容忍多义；`gitserver.Source` 聚合门面已更名 **GitTriggers**（构造器 `NewGitTriggers`，装配 provider 同名），api 端口 `GitDeploySource` 同步更名 **GitDeployTriggers** 与实现词汇同族，名实相符。
3. **drift/converge 混族对**〔有意让位〕：反义对语族不对称（日/拉），对称替代 `diverge/converge`（双拉）被否——drift detection 是 GitOps 全行业词汇，行业词优先于词源对称。
4. **rollback 混合词源**〔接受〕：roll（拉）+back（日）。行业标准，不动。
5. 双语对照钉死：台账=ledger、对账=reconcile、切流=switch flow（switch traffic）、锚定=anchor（「锚写」废止）、钉住=pin；中文补充规则：回滚=rollback（「回退」留给升级/raft 语境）、监听面=entrypoint（「入口」归 ingress）、发布=deploy 泛称（release 阶段专称「切流」）、凭据=Bearer token（「令牌」专指乐观并发令牌）。

## 2026-09-20 全库复查落地（命名一致性）

复查范围：代码 / proto / DB 列 / CLI / console / 文档全库。关键裁决与证据如下（改名均单独成 commit）。

### 已落地改名与文案

| 项 | 前 | 后 | 证据 |
| --- | --- | --- | --- |
| 部署归位值撞车 DR | `recovery="restore"` | `recovery="replay"`（state.RecoveryReplay） | internal/state/deployments.go、internal/engine/{releasing,recovery}.go |
| 看门狗预算名实不符 | `ReleaseTimeout` / `engine.release_timeout_seconds` | `DeployTimeout` / `engine.deploy_timeout_seconds`（覆盖 preparing/building/releasing 全链） | internal/engine/ports.go、internal/runtime/config.go、config-example.yaml、deploy/*.sh |
| proto 请求/响应不同词族 | `SetAppSourceRequest{url,branch,auth_kind,auth_secret}` | `source_url/source_branch/source_auth_kind/source_auth_secret`；删除 `ShowAppWebhookResponse.branch`（与 source_branch 同值重复投影） | proto/fleetly/server/v1/apps.proto（reserved 6 + "branch"） |
| 错误信封 phase 与部署子状态 phase 撞车 | `ErrorResponse.phase` | `ErrorResponse.stage`（resolve/build/deploy/serve） | proto/fleetly/shared/v1/error.proto、internal/apperr |
| volume 节点列独名 | `VolumeView.node_id` | `platform_node_id`（与 PlacementView/DB 同族） | proto/fleetly/server/v1/placement.proto |
| CLI 列动词孤例 | `nodes ls` | `nodes list` | cmd/fleetly/cmd/state.go、golden 改 nodes_list*.golden |
| 用户面 revision 别名 | `snapshot replay` / `version snapshots` / `revert one version` | `revision replay` / `revisions` / `rollback to the previous revision` | cmd/fleetly/cmd/rollback.go、console/src/pages/AppDeploymentsPage.tsx、README(.zh) |
| README rollout | `zero-downtime rollout` | `zero-downtime release`（中文「零停机切流」） | README.md:37,109、README_ZH.md:35,107 |
| console reconcile 写成 sync | `synced` / `not synced` | `reconciled` / `not reconciled` | console/src/pages/AppDomainsPage.tsx |
| 事件页卡片词 | `Activity` | `Events` | console/src/pages/EventsPage.tsx |
| verdict 词族 | `E_OBSERVE_UNHEALTHY` 摘要「判定 unhealthy」 | 摘要「判定 unstable（未达 healthy）」（码 ID 只增不改） | internal/errcode/codes.go + codes.golden |
| 禁用词漏网 | `node.down` 摘要「Swarm 心跳语义」 | 「Swarm 失联判定」；wording_test 扫描面扩入 internal/eventcode | internal/eventcode/events.go、internal/state/wording_test.go |
| config 注释 sweep/对账越界 | 「drift 对账 sweep 节拍」 | 「漂移检测扫描节拍」 | config-example.yaml:91 |
| 私有 ingress 多义 | `view.revision`（配置代次） | `view.configRevision` | internal/ingress/view.go |
| 预算锚 vs 身份锚 | `prepareBudgetAnchor` /「预算锚点」 | `prepareBudgetBaseline` /「基线」 | internal/engine/engine.go、rollback.go、迁移 00009 注释 |
| proto 注释残留 apply | deployments.proto「plan/apply 语义」 | 「变更计划/确认语义」 | proto/fleetly/server/v1/deployments.proto:129 |

### 新增词条（2026-09-19 后落地的新概念）

| 词条 | 定义 | 备注 |
| --- | --- | --- |
| **confirm_destructive** | 破坏性变更确认门控（服务删除/卷解绑需显式置位放行） | deployments.proto、CLI `--confirm-destructive`、`E_DEPLOY_CONFIRM_REQUIRED` |
| **phase baseline** | 阶段预算基线（`phase_started_at`，拾取时刻；排队不计入预算） | 时间词，不得称 anchor/锚点 |
| **tombstone / reap** | app 删除第二拍（deleting → deleted + 受管服务移除）；`reap` 是 tombstone 回收 duty（非保留期删除，不与 prune 混用） | eventcode `app.deleted`、internal/engine/appdelete.go |
| **last-admin guard** | 吊销最后一枚 admin token 的守卫（409） | `E_TOKEN_LAST_ADMIN` |
| **context_roots** | 构建上下文信任边界扩根（显式配置） | build 配置键 |
| **route withdraw** | 路由撤销（app 删除/域名移除路径；与 publish 成对） | 当前无 `route.withdraw` 事件/审计，记为待补对端 |
| **stale_nonterminal** | 非终态超龄显性化告警（janitor，不自愈） | engine/build 事件 |
| **ingress configRevision** | ingress 配置代次（内部计数），与平台 revision 严格分开 | 仅 internal/ingress |

### 命名规则补遗

- **stage vs phase**：错误信封用 `stage`（管线阶段 resolve/build/deploy/serve）；部署子状态用 `phase`（blocked_waiting）——两词不得互换。
- **CLI 单条读**：资源单条用 `get`（apps/env），复合视图用 `show`（placement/drift/webhook/ingress status）；列动词统一 `list`。
- **CLI 增删动词**：资源内与 RPC 成对（tokens create/revoke、env set/rm、apps delete、builds trigger、backups create）；跨资源的 `add/create/set` 与 `rm/delete/revoke` 差异属语义选择，不算混族。（git keys add/rm 随 git push 面 2026-09-29 退役。）
- **DB 受控别名**：`git_branch` 列名与 API `source_branch` 并存（迁移只加法纪律）——读代码时以此映射为准，不视为漂移。
- **中文译名**：回滚/回退分域、切流/发布分层、锚定（锚写废止）、监听面（entrypoint）、凭据/令牌分指。
- **已知文档-实现差异**（非命名问题，记为遗留）：release-semantics §2.4 写 revision `status ∈ {candidate|active|superseded}`，v0.1 实现只有 active/superseded。
