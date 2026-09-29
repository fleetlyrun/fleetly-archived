# fleetly 实施任务分解（Spike + v0.1）

| 状态 | 日期 | 关联 |
|---|---|---|
| 草案 | 2026-09-17 | 输入：[平台架构设计](../design/2026-09-17-architecture.md) §4/§6、三份专项设计、[交付流水线](../design/2026-09-17-delivery-pipeline.md) M0-M3、[Swarm 评估](../research/2026-09-17-swarm-substrate-assessment.md) V1-V7；本文件是架构 §4.5「v0.1 切面重算并冻结」的载体；实现后按票据勾验收并补 PR |

## 1. 分解原则与读法

- **垂直切片（tracer bullet）**：每张票据打通「schema → 逻辑 → API/CLI → 测试」的完整窄路径，完成即可独立演示或验证；不做横切层票据（「先写完所有表」「先写完所有 API」不允许）。
- **验收锚定设计文档**：每张票据的验收清单引用设计文档章节、V1-V7 验证项或错误码——设计文档是验收真源，票据不复述语义。
- **粒度**：票据 = 2~5 人日（AI 辅助口径）；验收清单内的条目即天然 PR 切分点，执行时可再切、不可扩大。带 ★ 的票据偏大（5 人日+），已标注建议切分点。
- **依赖表示**：`Blocked by` 只列真实闸门（genuinely gate），不列「最好先做」；无阻塞的票据构成 frontier，可立即开工。
- **范围冻结纪律**：v0.1 范围以架构 §4.2 八项 + 横切硬指标为硬边界；本分解新增任何条目须经显式裁决并记入架构文档，不允许从票据侧悄悄扩scope。

## 2. 里程碑总览与依赖图

```
T0 骨架与契约（~1 周）
 ├─ T0.1 仓库/Go module/CI ──┬─→ T0.4 dind E2E 骨架 ─┐
 ├─ T0.2 错误码/事件注册表 ──┼─→ T2.5 Compose 解析    │
 ├─ T0.3 OpenAPI+CLI 生成链 ─┘                        │
 └─ T0.5 三专项 v0.1 切面冻结（文档，独立并行）

T1 Spike（1-2 周，可并行；单人顺序 A→B→C）
 T1.1 A 构建 ─┬─→ T1.4 结论落档+V1-V7 回归种子 ─→ T2.24
 T1.2 B 发布 ─┤
 T1.3 C 底座 ─┘

T2 v0.1（核心；分层依赖见 §5）
 基座层：T2.1 安装器   T2.2 状态层 → T2.3 直读缓存 / T2.4 标记命名
 管线层：T2.5 Compose 解析 → T2.6 plan/apply      T2.7 密钥与 env
 构建层：T2.8 构建管线 → T2.9 digest 引用
 引擎层：T2.10 状态机对账 ★ → T2.11 窗口失败 ★ / T2.12 快照回滚 / T2.13 漂移
 放置层：T2.14 单机切面
 入口层：T2.15 Traefik 下发 → T2.16 ACME 域名
 面向层：T2.17 API → T2.18 CLI / T2.21 Console 端 ★   T2.19 git/webhook   T2.20 日志
 信任层：T2.22 备份基线 → T2.23 自升级   T2.24 流水线 M1   T2.25 资源校准
 收口：T2.26 v0.1 端到端验收（依赖多数）
```

关键路径（单人视角）：T0.1 → T2.2 → T2.5 → T2.10 → T2.12 → T2.15 → T2.16 → T2.17 → T2.21 → T2.26。

## 3. T0 骨架与契约（M0，约 1 周）

**T0.1 仓库与 CI 骨架** ｜ Blocked by: 无 ｜ 2-3 人日 ｜ ✅ 完成 2026-09-17（还原点 d8328d5）
- 交付：monorepo 骨架可克隆即跑——`cmd/fleetlyd`、`cmd/fleetly`、`internal/`、`pkg/api`、`/console`、`/deploy` 就位（架构 §2.7）；fleetlyd 以 **lynx `NewRunner` + Wire（`boot.Bootstrap`）引导**（D20，装配形态参考 lynx-clean-template，lynx 用法参考 messageloop）；PR 门禁（lint/单测/并发取消）绿。
- 验收：golangci-lint + gofmt + staticcheck + gosec + govulncheck 进 PR 轨道并阻断；`go generate ./...` 后无差异（wire_gen 同步）进 PR 门禁；空跑的单测任务绿；`go build ./...` 与 `fleetly --help` 可执行。

**T0.2 错误码与事件注册表** ｜ Blocked by: T0.1 ｜ 2 人日 ｜ ✅ 完成 2026-09-17（还原点 421a991；遗留裁决见冻结清单 FZ-2/3/4/5）
- 交付：代码内注册表为唯一真源（架构 §2.8），错误信封 `{code,message,stage,deployment_id,suggestion,context,docs}` 以 proto `ErrorResponse` 定义、经 gateway `HTTPErrorHandler` 输出（发布专项 §2.7、D21）。
- 验收：注册表只增/不复用的 CI 校验测试；首发错误码（`E_COMPOSE_*`、`E_STATE_VERSION_CONFLICT` 等 T2 首批）入表；信封序列化有 golden 测试。

**T0.3 proto 契约与生成链** ｜ Blocked by: T0.1 ｜ 2-3 人日 ｜ ✅ 完成 2026-09-17（还原点 0a25e25）
- 交付：buf 工具链 + proto 骨架（`fleetly.{client,console,server}.vN` 分模块）+ gRPC 服务骨架挂 lynx + grpc-gateway 挂载（torchwood 范式，D21）+ 平台 SDK（gRPC client，独立模块）。
- 验收：buf lint 通过、buf breaking 对基线跑通；hello RPC 从同一 proto 生成 gRPC client（SDK）与 REST 端点（gateway）双面可用；错误信封以 proto `ErrorResponse` 定义、gateway `HTTPErrorHandler` 输出 snake_case（torchwood 同款 `disable_default_errors`）；生成物同步检查（`buf generate` 后 diff 为空）进 PR 门禁；`/healthz/*` 来自 lynx 框架。

**T0.4 dind E2E 骨架** ｜ Blocked by: T0.1 ｜ 2-3 人日 ｜ ✅ 完成 2026-09-17（还原点 bfa0f48）
- 交付：`docker:29.8.1-dind` 内起平台的 E2E harness（交付 P2、M0）。
- 验收：CI 中 dind 容器内拉起 fleetlyd 冒烟（启动/健康检查/关闭）；可复用为 Spike B/C 与 nightly 的底座。

**T0.5 三专项 v0.1 切面冻结**（文档任务）｜ Blocked by: 无 ｜ 1-2 人日 ｜ ✅ 完成 2026-09-17（[冻结清单](2026-09-17-v0.1-scope-freeze.md)，含 FZ-1~FZ-5 裁决）
- 交付：按架构 §4.5 对发布/放置/状态模型三专项的 v0.1 条目逐项核对，产出冻结清单（进 v0.1 的表、状态、错误码、事件白名单；超出者后置）。
- 验收：清单经用户裁决合入；已知待裁项有结论——例：§4.2 第 4 项「自动连接串」在 v0.1 无数据服务时可为何种形态（预留机制 or 明确后置）。

## 4. T1 Spike（1-2 周，V1-V7 为采纳门）

**T1.1 Spike A：构建与镜像** ｜ Blocked by: T0.1（可与他票并行）｜ 3-4 人日 ｜ ✅ 完成 2026-09-17（还原点 7042317；[spike/a/README.md](../../spike/a/README.md)）
- 交付：构建链路风险清零的验证报告 + 可复现脚本。
- 验收（架构 §4.1 A 行全项）：Railpack 钉版本 + `railpack-plan.json` 归档可复现；缓存三情形（本地层/registry cache/secrets-hash 失效）断言；私有依赖凭证不进最终镜像；构建在 CPU/内存限额内且不污染宿主；rootless vs 特权选型结论；**V2**——service 以 `app@sha256:` 创建零 pull 尝试。

**T1.2 Spike B：发布与路由** ｜ Blocked by: T0.1；建议接 T0.4 加速 ｜ 4-6 人日 ｜ ✅ 完成 2026-09-17（还原点 9b722c2；[spike/b/README.md](../../spike/b/README.md)）
- 交付：发布语义全链路验证 + 失败矩阵断言脚本（即 nightly 回归的雏形）。
- 验收（架构 §4.1 B 行全项）：**V1** health 失败 → FAILED/paused/旧任务不中断；**B2** 同内容重放任务零替换（task id 不变）；**B3** start-period 内 LB 端点时机结论（最高优先级开放问题，结论回写架构 §5 风险行与对外口径）；stack apply 增删服务 + 受管字段 `E_COMPOSE_MANAGED_FIELD`；**V3** 更新窗口探测零失败/VIP 不变；**V4** keep-alive 陈旧连接以 `serversTransport` 消除；启动即崩时旧版本持续服务且入口零污染；单应用坏配置不影响他应用；失败矩阵逐条错误码断言；快照重放回滚 1 分钟内完成。

**T1.3 Spike C：底座** ｜ Blocked by: T0.1 ｜ 3-4 人日 ｜ ✅ 完成 2026-09-17（还原点 35fc2ad；[spike/c/README.md](../../spike/c/README.md)）
- 交付：Swarm 底座行为验证记录（卷/绑定/恢复语义）。
- 验收（架构 §4.1 C 行全项）：`swarm init` 对既有容器无影响、用户视角透明（副作用清单落档）；节点 DOWN 15s 量级 stateless 自动重建；**V6a** 有卷无约束迁移得空卷（复现+文档化）、加绑定钉住；**V6b** down→blocked→恢复、drain→回岗、remove→人工重绑路径走通；**V5** `--force-new-cluster` 恢复演练应用不中断；**V5b** raft 回退后孤儿容器命运（0/5/30min 观察）。

**T1.4 Spike 结论落档 + 回归种子** ｜ Blocked by: T1.1、T1.2、T1.3 ｜ 2-3 人日 ｜ ✅ 完成 2026-09-17（回写 db5d70e + 种子 4a4c97c；nightly 5 suite 本机 GREEN，V5 占位）
- 交付：Spike 结论写回设计文档（含 B3 对外口径裁决）；V1-V7 转为可进 nightly 的回归测试骨架（交付 P3）。
- 验收：任一 V 项不通过且有缓解/无缓解结论落档（触发 §3.1 退出预案流程）；回归骨架在 CI 按交付 §6 轨道分布可运行。

## 5. T2 v0.1（8 项范围 + 横切硬指标）

### 基座层

**T2.1 安装器与引擎门禁** ｜ Blocked by: T1.3 ｜ 3-4 人日
- 交付：一条命令安装 → 单机可运行平台（隐式 `docker swarm init`，架构 §2.6/§4.2）。
- 验收：干净 VPS（amd64/arm64）一条命令完成安装，安装报告含底座端口暴露面提示（私网/公网 advertise-addr 判定，§4.2 底座端口加固）；Engine ≥29.8.1 + iptables 后端检查不满足即拒绝；systemd unit 开机自启；卸载脚本带走平台不留孤儿（应用数据除外，明示）。

**T2.2 状态层基座** ｜ Blocked by: T0.2 ｜ 4-5 人日
- 交付：SQLite(WAL) + goose 迁移 + 核心表 + 审计/事件自存（状态模型 §2.1/§2.9、§4 v0.1 清单）。
- 验收：v0.1 冻结清单内的表全部建齐（以 T0.5 结论为准）；审计与业务写同事务、审计失败即操作失败（fail-closed 有测试）；事件 `seq` 单调 + SSE 游标 + `E_EVENT_CURSOR_EXPIRED`(410) 断档测试；tombstone 删除不复活有单测。

**T2.3 观测缓存与写前直读** ｜ Blocked by: T2.2 ｜ 2-3 人日
- 交付：`nodes` 观测缓存（单机同路径）+ 读契约（状态模型 §2.2）。
- 验收：每行 `observed_at/stale`；底座不可达 → 全部置 stale + 指数退避；写前直读冲突 → `E_STATE_VERSION_CONFLICT`(409) 有集成测试；不出现「心跳」字样（文案断言）。

**T2.4 对象标记与命名** ｜ Blocked by: T2.2 ｜ 2-3 人日
- 交付：最小 label 集 + 平台命名约定（状态模型 §2.4、架构 §2.4 服务命名与网络行）。
- 验收：service/container/node label 最小集下发与对账重放（label 被删改 → 自动重放 + 事件）；`fleetly-<app>-<service>` 命名 + per-app 网络 + 别名 = compose 服务名（两个 app 同名服务互不冲突、app 内短名互通有集成测试）；secret 命名空间化 + file target 保持 compose 名；卷命名约定 `fleetly-<app>-<key>-<appid8>`；用户占用 `fleetly.*` 前缀 → 422 `E_LABEL_RESERVED`。

### 管线层

**T2.5 Compose 解析与校验** ｜ Blocked by: T0.2 ｜ 4-6 人日
- 交付：compose 文件 → 归一化期望态 + 校验错误（架构 §2.4）。
- 验收：白名单/拒绝清单（`depends_on`/`extends`/`include`/`profiles`/`configs`/外部网络/host 模式 → `E_COMPOSE_UNSUPPORTED`）；受管字段（`failure_action`≠pause、`monitor`≠5s → `E_COMPOSE_MANAGED_FIELD`）；危险字段默认拒绝、admin scope + 审计开启；`${VAR}`/`.env` 插值关闭（字面值）；label 契约——domains 列表解析（逗号分隔、≤5/服务、`E_DOMAIN_CONFLICT`/`E_DOMAIN_UNSUPPORTED`）、placement label 语法、cron label 仅 v0.2 校验提示；归一化输出稳定（golden 测试，后续 `desired-hash` 的地基）。

**T2.6 plan / apply / diff CLI** ｜ Blocked by: T2.5、T0.3 ｜ 3-4 人日
- 交付：面向 AI Agent 的一等接口（架构 §2.4 plan/apply 语义）。
- 验收：`--json` 全覆盖；三态退出码（0/2/1）；secrets 脱敏；plan artifact 落盘 + apply 前 etag 校验（stale → 拒绝）；破坏性操作要求 `--confirm-destructive`；覆盖键 env → `W_ENV_PLATFORM_OVERRIDE` 出现在 plan 输出。

**T2.7 密钥与 env** ｜ Blocked by: T2.2、T2.4 ｜ 3-4 人日
- 交付：加密存储 → 注入运行的完整链路（架构 §2.3 密钥方案、§2.4 变量合并/密钥行）。
- 验收：envelope 加密（age）落库，主密钥文件权限保护且与备份分离；compose secrets → Swarm secret 映射（`/run/secrets/<name>` 可读）；env 三层合并（`env_file` < `environment` < 平台 env_vars）+ `fleetly env set` 创建 pending、随下次部署生效；密钥值不进事件/审计/日志（负面断言）。

### 构建层

**T2.8 构建管线产品化** ｜ Blocked by: T1.1、T2.2 ｜ 5-7 人日 ★（切分建议：a 队列与限额 / b Railpack 集成 / c Dockerfile 路径与日志）
- 交付：`git push`/webhook 触发到镜像 digest 的生产构建链（架构 §2.2 构建行、§4.2 第 1 项）。
- 验收：构建队列并发 ≤2、cgroup 限额生效、排队可见；Railpack 版本钉死 + plan JSON/构建日志按 app 留存；Dockerfile 与仅 image 两模式一等支持；失败 → `E_BUILD_FAILED` 带 stderr 与建议；构建资源峰值不击穿宿主（压测记录）。

**T2.9 镜像身份（v0.1 免 registry）** ｜ Blocked by: T2.8 ｜ 2-3 人日
- 交付：本地 digest 引用体系（架构 §2.5 末条、发布专项 §2.4 preflight）。
- 验收：部署一律以 `app@sha256:` 创建（V2 断言进回归）；镜像不自动清理（策略落档）；回滚 preflight 镜像缺失 → `E_IMAGE_UNAVAILABLE` + `W_ROLLBACK_IMAGE_RISK`。

### 引擎层

**T2.10 发布状态机与对账核心** ｜ Blocked by: T2.3、T2.5、T1.2 ｜ 6-8 人日 ★（切分建议：a 状态机骨架 / b stack 对账 / c 重启恢复与互斥）
- 交付：发布主链路 `queued→…→succeeded/failed/cancelled`（发布专项 §2.3）。
- 验收：状态机转换穷举单测（含 `blocked_waiting` 子状态）；同 app 部署互斥 + 队列；控制面重启扫描非终态 deployment（健康→重开观察窗 / paused·failed→分类归位 / 不可判→人工）；stack 对账服务增删、受管字段校验、`deployments.kind=recovery` 字段落库；cancel 语义（未健康可 cancel、曾健康 409）。

**T2.11 窗口与失败语义** ｜ Blocked by: T2.10 ｜ 5-7 人日 ★（切分建议：a L2 看门狗 / b L3 观察窗与 degraded / c 场景矩阵测试）
- 交付：四层窗口与失败分流（发布专项 §2.2/§2.5 场景矩阵 1-14）。
- 验收：L1 固定 5s 不暴露；L2 `deployTimeout=300s`（blocked_waiting 暂停计时/恢复重算）；L3 观察窗 60s（崩溃循环 ≥2、窗末未恢复、副本水位按 desired；默认告警 + `app=degraded`）；失败分流按 `first_healthy_at`（未切流同记录 `recovery=replay` / 已切流 `verdict=unstable`）；首发失败 scale=0 + `substrate_halted`；stop-first 强制归位 + `downtime_ms` 如实累计；场景矩阵 1-14 每条错误码断言进 nightly。

**T2.12 快照与回滚** ｜ Blocked by: T2.10、T2.9 ｜ 4-5 人日
- 交付：最近 5 版 + 单层重放（发布专项 §2.4）。
- 验收：`revisions` 含归一化 compose + 平台覆盖层 + **env 合并结果快照**；成功部署才入保留集，越界 → `E_ROLLBACK_NO_TARGET`；重放：compose/env 按快照、治理参数与 secret 取当前；preflight 四项在动底座前执行；回滚 1 分钟内完成（回归断言）。

**T2.13 漂移检测** ｜ Blocked by: T2.10、T2.7 ｜ 3-4 人日
- 交付：检测默认开、收敛 opt-in（架构 D11、状态模型 §2.5）。
- 验收：`desired-hash` 按合并结果计算、稳定性单测（env 只参与 `key:sha256`）；外部改动（含手动 `--rollback`）→ `reconcile.drift_detected`；收敛 per-app opt-in 默认关；字段级 diff 报告不含 env 值。

### 放置层（单机切面）

**T2.14 placement 单机切面** ｜ Blocked by: T2.4、T2.5 ｜ 2-3 人日
- 交付：同一代码路径的单节点放置（放置专项 §2.9、§4 v0.1）。
- 验收：label 解析（名/ID，失败 → `E_PLACEMENT_NODE_INVALID/NOT_FOUND` + 候选清单）；有卷自动绑定本机、绑定优先规则；卷注册表（volumes 表）；多节点操作 → `E_CAPABILITY_REQUIRES_MULTI_NODE` 不静默成功；删除应用保留卷（orphaned 可见）。

### 入口层

**T2.15 Traefik 部署与路由下发** ｜ Blocked by: T2.10、T1.2 ｜ 4-5 人日
- 交付：控制面全量下发的入口（架构 §2.5 不变量、§2.6 入口模型）。
- 验收：Traefik global service 部署（v0.1 单实例同路径）；HTTP provider 配置端点（Header token）；**路由发布严格晚于 health gate**（集成断言）；空 routers/services 不落盘、单应用坏配置零污染（回归）；`serversTransport` 连接治理参数下发（V4 断言）。

**T2.16 集中 ACME 与域名** ｜ Blocked by: T2.15 ｜ 3-5 人日
- 交付：域名列表 → HTTPS 全自动（架构 §2.4 域名行、§2.6 证书集中化）。
- 验收：lego 集中签发 + 多 SAN；HTTP-01 挑战经各节点 Traefik 反代到控制面；证书存平台、随动态配置下发、独立备份；`fleetly domains verify`（解析 + 证书 + 全节点入口可达）；证书材料不进应用备份。

### 面向层

**T2.17 API 面（gRPC + gateway）** ｜ Blocked by: T2.10、T2.6 ｜ 5-7 人日 ★（切分建议：按资源域 apps/deployments/domains/env/logs 各一片）
- 交付：v0.1 全资源 API——proto 服务实现 + gateway REST 面 + SSE（gRPC server-streaming 或同进程原生 handler，torchwood realtime 同型）（架构 §4.2 第 2 项、D21）。
- 验收：apps/deployments/rollback/domains/env/logs/placement 服务齐，SDK（gRPC）与 REST（gateway）双面由同一 proto 派生且一致；SSE 事件流带游标；拦截器链强制鉴权（token scope，read/deploy/admin）——无 token 全部 401（安全基线）；gRPC-only 不挂 gateway 的服务清单显式维护（torchwood 同纪律）。

**T2.18 CLI 全命令** ｜ Blocked by: T2.17 ｜ 3-4 人日
- 交付：`fleetly` CLI（lynx-go/commands，torchwood 同款）直接消费平台 SDK（gRPC client，与 API 同源；架构 §4.2 第 2 项、D21）。
- 验收：deploy/logs/env/domains/rollback/plan/apply/diff 全命令 `--json`；日志/事件长流经 gRPC streaming 输出；输出 schema 快照测试（防漂移）；三态退出码贯穿。

**T2.19 git push(SSH) 与 webhook** ｜ Blocked by: T2.17 ｜ 3-4 人日
〔2026-09-29 注记：SSH push 半随 ADR-0012 裁决移除；webhook+拉源保留为唯一 git 触发轨。本卡为 v0.1 历史票面。〕
- 交付：两条触发入口（架构 §2.5 webhook 不变量、§4.2 第 1 项）。
- 验收：SSH git push → post-receive 触发部署；webhook 强制验签（GitHub/Gitea）+ 时间窗防重放 + 按 revision 幂等去重（重复投递不重复部署，测试断言）。

**T2.20 日志管线** ｜ Blocked by: T2.10 ｜ 3-4 人日
- 交付：实时 + 历史日志（架构 §4.2 第 5 项）。
- 验收：SSE 实时流（断线游标续读）；落盘轮转 7 天；ring buffer 限深；历史检索按 app/时间窗；日志中 secret 值脱敏（负面断言）。

**T2.21 Console 端基础** ｜ Blocked by: T2.17 ｜ 8-12 人日 ★（切分建议：a 骨架与鉴权 / b 应用列表·详情·部署 / c 日志与 env·域名）
- 交付：console 端基础界面（React + Vite + shadcn/ui；架构 §4.2 第 6 项）。
- 验收：应用列表/详情、部署历史与状态（degraded/blocked 一等展示）、日志（SSE）、env 管理（pending 变更可见）、域名管理；console 仅经 REST API（无旁路调用）；Playwright smoke 进 PR 可选轨。

### 信任层

**T2.22 备份基线与信任闭环** ｜ Blocked by: T2.2、T2.12 ｜ 3-4 人日
- 交付：控制面状态可备份可恢复可演练（架构 §4.2 横切备份项、状态模型 §2.7、TTFW 验收）。
- 验收：热备 `VACUUM INTO` + sha256 回读校验（每次成功部署后 + 每日）进 `state_backups` 台账（verify_status）；失败红色告警（无「绿色假成功」路径，负面测试）；备份密钥/元数据与数据分离保存；按文档人工执行 L1 恢复演练一次成功，首次配置 ≤10 分钟（演练记录落档）。

**T2.23 平台自升级（升级双轨）** ｜ Blocked by: T2.22、T2.1 ｜ 3-4 人日
- 交付：fleetlyd 升级原子化 + 双轨口径落地（架构 §4.2 横切、交付 §2.4）。
- 验收：预拉镜像 + 升级前热备快照 + 失败自动回退（注入失败测试）；升级全程应用不停（E2E 断言）；Engine/主机升级的冷备路径文档化并指向维护窗口语义（不停 Engine 的口径写入升级文档）；stable 通道只发签名版本。

**T2.24 交付流水线 M1 完备** ｜ Blocked by: T1.4、T2.17 ｜ 4-6 人日 ★
- 交付：三轨道齐备（交付 §4 M1）。
- 验收：nightly = 引擎矩阵（29.8.1 × containerd/overlay2 + 上一 minor）+ V1-V7 全套 + 升级 E2E（v_n→v_{n+1} 迁移前后对比 + 应用不中断）+ conformance（Builder；ObjectStore 以 MinIO 容器代演）+ 资源基线采样；release 轨道制品链（多平台二进制 + 安装脚本 + ghcr 镜像 + SBOM + cosign 签名 + checksum）；许可证守卫（AGPL/DSAL 白名单）阻断。

**T2.25 资源预算与容量校准** ｜ Blocked by: T2.8、T2.24 ｜ 2-3 人日
- 交付：硬指标实测定稿（架构 §4.2 横切）。
- 验收：控制面 idle（含 dockerd+swarmkit，Traefik 单列）实测报告，>200MB 则优化或修订口径（落档）；≤50 apps / ≤200 域名 / 并发构建 2 压测并按结果修正建议值。

**T2.26 v0.1 端到端验收** ｜ Blocked by: T2.16、T2.18、T2.19、T2.21、T2.22、T2.23、T2.24 ｜ 2-3 人日
- 交付：发布检查单 + 验收记录（架构 §4.2 验收段）。
- 验收：干净 VPS 一条命令安装 → 已解析域名 20 分钟内 git push 部署拿到 HTTPS（DNS 传播不计入）→ UI 可见日志与配置 → 一键回滚成功；信任闭环：备份 → 回读校验 → L1 恢复演练（≤10 分钟配置）；发布检查单全部勾选并归档为 v0.1 发布依据。

## 6. 估算汇总

| 阶段 | 人日（AI 辅助） | 说明 |
|---|---|---|
| T0 骨架与契约 | 9-13 | M0，约 1 周墙钟 |
| T1 Spike | 12-17 | 三线可并行 = 1-2 周墙钟；单人串行 3-4 周 |
| T2 v0.1 | 84-116 | 26 张票据；console 端（T2.21）与引擎层（T2.10-12）为最大变量 |
| 合计 | **105-146 人日** | 对齐架构 §4.5（31-42k LOC；2-3 人 3-4 个月 / 单人 6-9 个月），本表即「切面重算」结果，随 burn-down 修正 |

排班建议：2 人时 A=引擎层+基座（T2.1-2.4、T2.10-14）、B=管线+面向层（T2.5-9、T2.15-21），信任层共同收口；3 人时 console 与流水线独立成线。

## 7. v0.2 粗粒度 Epics（2026-09-20 起已细化，移至 [v0.2 规划](2026-09-20-v0.2-plan.md)，下表为原始占位存档）

| # | Epic | 前置设计/裁决 |
|---|---|---|
| E1 | 多节点包：join 向导（含端口精确放行）+ zot（平台域名/信任方案已定）+ placement 多节点 + 卷位置前哨 + HA 边界向导 + 2 节点验收演练 | 架构 §2.6；前置 = 平台基础域名安装项 |
| E2 | MCP server（薄适配层）+ 工具预算 ≤30 核算 | 架构 D8/D10；CVE-2026-46519 类执行层 scope 测试 |
| E3 | S3 外部端点（配置/连通测试 + restic 备份目标 + 凭证注入） | conformance 套件随落地 |
| E4 | 数据库托管（PG/Redis 首发） | **跨 app 网络互访裁决**（架构 §2.4 遗留开放点）+ 备份适配器设计 |
| E5 | Cron（细则已定，§4.3） | 与备份 ticker 共核；调度核用 lynx contrib/schedule（6 段含秒 → 平台契约 5 段，秒位固定 0） |
| E6 | Metrics（VictoriaMetrics/cAdvisor）+ 通知 | 资源预算修订（idle <400MB 目标） |
| E7 | Web 终端（固定名词；执行中继 `fleetly-exec`，D19） | 中继安全面测试套件 |
| E8 | dogfooding：staging 用 fleetly 部署自身 | 交付 §2.6，进发布检查单 |

## 8. 非工程并行项

- Spike 期完成 5-10 个目标用户访谈（验证「声明式/漂移检测」「AI Agent 直接操作」为真实痛点，架构 §4.1）。
- v0.1 发布时设定外部试用与反馈目标（数量在 v0.1 启动时定）。
- v0.1 发布前：对外口径素材按 §1.2 纪律审查（不得写 v0.2 能力、不得写「内置 S3」、稳定性表述按既定三条）。

## 9. 执行纪律

- 只从 frontier 取票（全部 blocker 完成的票据）；票据完成定义 = 验收全勾 + 设计文档引用章节未偏离。
- 发现设计缺口 → 停下补设计文档（走裁决轮），不允许票据内私改语义。
- 每完成一个阶段（T0/T1/T2 各层）跑一次文档↔实现一致性抽查；v0.1 收口时文档状态改「已实现」并补 PR 链接（docs/README 约定）。

## 10. 实施状态（2026-09-19 回填：26/26 票完成）

实施方式：顺序子代理分段实现 + 段间验收门 + 还原点提交（subagent-sequential-implement 纪律）。逐票还原点与冻结轮确认见 [v0.1-scope-freeze §4/§5](2026-09-17-v0.1-scope-freeze.md)；端到端证据见 [v0.1 验收记录](../reports/2026-09-19-v0.1-acceptance.md) 与 [资源校准报告](../reports/2026-09-19-resource-calibration.md)。

| 票据 | 完成日 | 还原点/证据 |
|---|---|---|
| T0.1-T0.5（骨架/注册表/proto/E2E/冻结） | 2026-09-17 | d8328d5 / 421a991 / 0a25e25 / bfa0f48 |
| T1.1-T1.4（Spike A/B/C + 回归种子） | 2026-09-17 | spike/*/FINDINGS；e2e/nightly（V5 见 FZ-6） |
| T2.2-T2.4（状态层/观测缓存/标记命名） | 2026-09-18 | T2-1/T2-2 阶段还原点 |
| T2.5-T2.9（compose 管线/secrets/env/构建） | 2026-09-18 | T2-3/T2-4 阶段还原点 |
| T2.10-T2.14（引擎状态机/窗口/回滚/漂移/放置） | 2026-09-18 | 8a0e6c5 / 2e2312b |
| T2.15-T2.16（Traefik 入口/ACME） | 2026-09-18 | 57a5c98 |
| T2.17+T2.20（API 面/日志管线） | 2026-09-18 | f60e52a |
| T2.18（CLI-over-SDK） | 2026-09-19 | 4ecf97d |
| T2.19（git push + webhook） | 2026-09-19 | 30cdc26 |
| T2.21（Console 端） | 2026-09-19 | 1d2166a |
| T2.1（安装器与引擎门禁——排期补票，FZ-10） | 2026-09-19 | 62697a3 |
| T2.22+T2.23（备份基线/自升级双轨） | 2026-09-19 | 69299f0；runbooks 三份 |
| T2.24（交付流水线 M1） | 2026-09-19 | 683709e + c204b48 + 00c0e8c |
| T2.25+T2.26（资源校准/端到端验收） | 2026-09-19 | docs/reports/ 两份；旅程 CRITICAL 61s≤20min |

已知边界与后置项（v0.2）：自动连接串（FZ-1）、ObjectStore conformance（FZ-7）、多节点 V5 自动化（FZ-6）、Playwright smoke（可选轨）、真 VPS dogfooding（M2，delivery §2.6）、arm64 运行级 release smoke。
