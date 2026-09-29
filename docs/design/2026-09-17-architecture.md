# fleetly 平台架构设计

| 状态 | 日期 | 关联 |
|---|---|---|
| 已实现（v0.1 切面） | 2026-09-20 | **v0.1 收口**：§4.2 八项 + 横切硬指标验收全绿（[验收记录](../reports/2026-09-19-v0.1-acceptance.md)），票据还原点见 [切面冻结清单 §4/§5](../plan/2026-09-17-v0.1-scope-freeze.md)；评审整改 S1-S20 全落（[整改方案](2026-09-20-remediation-complete.md)）；§4.3 v0.2/v0.3 路线图仍为规划（[v0.2 规划](../plan/2026-09-20-v0.2-plan.md)）。决策来自项目启动讨论；[竞品调研](../research/2026-09-17-competitive-landscape.md) 13 条建议已应用（见调研 §10）；[Swarm 底座评估](../research/2026-09-17-swarm-substrate-assessment.md)已采纳（D2/D12 改写，V1-V7 为采纳门）；长线演进（§2.8、D13）已应用；**应用模型反转为 Compose 规范（D14 重写、§2.4 重写，自研 spec 废止）**；独立设计×交叉验证轮已合入（发布失败/回滚→[专项](2026-09-17-release-semantics.md)、stateful 放置→[专项](2026-09-17-stateful-placement.md)、控制面状态模型→[专项](2026-09-17-state-model.md)）；交付流水线见[交付流水线设计](2026-09-17-delivery-pipeline.md)；一致性审查轮已应用（A 类矛盾修正 + 6 项裁决：blocked_waiting 看门狗豁免、replicas v0.1 照用、app 状态机、placement label 冲突规则、cron 入 v0.2、state_backups 入 v0.1）；奥卡姆裁决轮已应用（F1/F2 悬空码删除、conformance 分档、S3 改外部端点优先〔D4 复议〕、v0.1 契约与节点表去噪、runtime_node_refs/卷身份/cAdvisor 记账补记）；cron 最小形态落档（§4.3 细则 + label 约定 + `cron_runs` + job 继承绑定 + §7 明确不做）；画像复核轮已应用（目标用户画像与设计输入入 §1.2，2 节点 HA 边界口径入 §2.6，v0.2 多节点提序入 §4.3）；定位复核续轮已应用（栈边界与对外口径入 §1.2：CI/CD 拆分、四库模板、S3 措辞、稳定性表述、AI 排序；TTFW 信任闭环验收入 §4.2；数据库模板与不做清单更新）；S3 卷被否方案落档（放置专项 §5/§7）；审核裁决轮已应用（升级双轨口径、执行中继 D19、每节点入口与集中证书模型、zot 平台域名方案、env 三层合并链、服务命名与网络别名、底座端口加固、cron 触发前哨与超时看门狗、域名列表契约、C 级一致性清理）；技术选型补充已应用（基础 Go 框架 = lynx + google/wire，D20，参考 messageloop；API 面 = gRPC + grpc-gateway，D21，参考 torchwood，huma 经用户裁决否决）；依赖复核轮已应用（age 加密 / coder-websocket 中继流 / shadcn-ui / minio-go 四项落定，git 许可证修正为 GPLv2 子进程调用，CI S3 代演用 RustFS〔初定 SeaweedFS，后经用户裁决改定〕）；T1 Spike 轮已完成并回写（2026-09-17：V1-V7 采纳门全过、B3/B2 关闭、V4 口径改优雅退出为主键、Traefik {} 纪律、DOWN 判定实测 13.5s，证据 spike/{a,b,c}/README.md） |

## 1. 现状与问题

### 1.1 术语约定（先读）

同一个英文词 "agent" 在本项目里曾指两个不同事物（基础设施的节点进程 与 AI 智能体）。裸词 "agent" 已作废，统一术语基线如下：

| 术语 | 定义 | 正例 | 反例 |
|---|---|---|---|
| **AI Agent** | 能自主决策并通过 API/MCP 操作平台的 AI 程序 | Claude Code 经 MCP 调 `deploy` 工具部署应用 | CI 脚本调 CLI 部署（属自动化客户端，非 AI Agent） |
| **节点（Swarm node）** | 加入 Swarm 集群的一台 Docker Engine 主机 | `docker node ls` 列出的机器；承接 service 任务 | 控制面进程本身；一个容器 |
| **actor** | 审计日志中的操作主体，取值 human / AI Agent | token 备注「CI 部署」 | — |

边界裁决：平台自动重启崩溃容器 = 确定性规则，不算 AI Agent；UI 点击部署 = human actor；在 AI 编辑器里说「帮我部署这个仓库」= AI Agent。产品承诺文案 **AI/Agent Native** 中的 Agent 指 AI Agent；本约定优先于历史用法与行业通用词。**「Web 终端」为固定功能名词**（浏览器内终端，经执行中继实现，D19），不随前端组件命名（console 端）变化。历史术语「节点守护进程（fleetlyd node）」已随 D12 采纳 Swarm 而退役（自研 node 协议降级为退出预案）。

### 1.2 项目定位

fleetly（曾用名 edgefleet，更早 edgesets；2026-09-18 更名）是极轻量级开源 PaaS：开箱即用、用户友好、面向 10 台以下的服务器集群、资源占用少、够用就好，对外承诺 AI/Agent Native。

一句话定位：**Dokku 的资源占用，Railway 的 API，AI Agent 优先的操作方式。**

**目标用户画像（2026-09-17 定位复核；行为定义，非人口统计）**：

| 维度 | 画像 | 依据 |
|---|---|---|
| 团队 | 无专职运维；专职开发 ≤5 人（其余为产品/设计/运营） | 用户裁决（访谈复核分布） |
| 基础设施 | 1~3 台 VPS/独服起步；**上线后为 HA 加至 ≥2 台**；10 台为设计余量 | 用户裁决 |
| 技能 | 会 Docker/Compose、SSH；不愿学 k8s | [竞品调研](../research/2026-09-17-competitive-landscape.md)（该人群明确拒绝 K8s） |
| 运维预算 | **1~2 小时/周**（≈4~8 小时/月，含升级/证书/备份/救火） | 用户裁决（最关键设计输入） |
| 应用形态 | web + worker + cron；依赖数据库/Redis/对象存储 | 行业共识 |
| 动机 | 控制/合规/成本（离开托管 PaaS、避免 k8s） | 竞品调研（Heroku 维持模式、需求空位） |
| 决策单元 | 1~2 名资深开发/CTO，无采购流程；GitHub/口碑传播 | 开源自托管采纳模式 |

反画像（明确不是谁）：有专职运维/平台团队（他们已有 k8s/云）；非技术用户（不承诺零技术门槛）；多租户平台商与 >10 台规模（商业版或退出预案）；要求强 HA、合规审计留存（不在 v0.x 承诺内）。

JTBD：让 ≤5 个开发，在不雇运维、不学 k8s 的前提下，把生产应用安全地从 1 台运营到 10 台——升级不出事、数据不丢、出事 AI 能帮着查。

画像导出的两条硬输入：**① 维护预算 1~2 小时/周** ⇒ 证书/备份/升级/巡检全自动且可验证（回读校验、演练）；静默失败零容忍；升级不得需要「研究」；故障诊断必须分钟级（错误信息即产品 + AI 排障是兑现方式，见 §2.1 原则 5）。**② 2 台为生产基线** ⇒ 多节点是投产必经而非扩展选项（zot 为投产组件、有状态放置立即相关、HA 边界必须说清，见 §2.6；v0.2 顺序见 §4.3）。

**栈边界与对外口径（定位复核续）**：

- **CI/CD**：CD（push/webhook → 构建 → 零停机上线 → 回滚）为平台闭环内置；CI（测试/lint）由 Git 托管方承担，平台以 webhook 状态门禁集成（CI 未过不发布）；**不自研 CI 引擎**（§7 明确不做）。
- **数据库**：产品是**托管数据服务机制**（模板 + 卷钉住 + 备份/恢复适配器 + 连接串注入），库清单是数据——目标模板 = Postgres/Redis/MySQL/MongoDB；PG/Redis 首发，MySQL/Mongo 紧随按需求排序（各引擎备份/恢复适配器是主要成本；v0.2 顺序见 §4.3）。
- **S3**：v0.2 只支持 S3 兼容**外部端点** + 本地路径兜底；内置 S3 打包为触发式（需求证据 → RustFS 评估，D4）。对外文案写「支持 S3 兼容端点」，**不得写「内置 S3」**。〔2026-09-20 V2-2：RustFS 1.0 GA 触发引入为 **opt-in 管理组件**——对外可写「可选自托管 S3 兼容存储（RustFS）」；仍不得表述为默认内置 S3，同节点备份不得表述为灾备，见 [v0.2 规划](../plan/2026-09-20-v0.2-plan.md)〕
- **稳定性表述**：不以「基于 Swarm」作为稳定性论据；对外口径 = 变化慢的底座 + 平台护栏（锁版本 + 升级回归矩阵、pause + 快照重放、升级原子化、备份回读校验）+ 诚实边界（无官方 LTS、Engine 破坏式升级风险及缓解，D12/§2.6）。
- **AI 排序**：AI Agent 是差异化的锋面（获客/传播），信任（升级可回退、备份可恢复、错误可诊断）是成交与留存的地板；对外演示必须走完一次真实「故障 → 诊断 → 修复」闭环；v0.1 不得宣传 v0.2 的 AI 能力（时间线见下）。

多节点语义：**统一管理与调度由 Docker Swarm 承担**——统一 API/UI/调度视图；无状态服务自动重调度（节点失联 15s 量级判定）；**有状态服务默认自动钉住**（平台 placement 绑定；节点消失不迁移、不换点；local 卷不跟随）；无跨节点共享存储（能力边界见 2.6 与[放置专项](2026-09-17-stateful-placement.md)）。

对外宣称时间线：v0.1 的 AI Agent 能力 = API/CLI/plan-apply/结构化错误；MCP 与完整 AI Agent 体验属 v0.2，对外材料不得把 v0.2 能力写进 v0.1 承诺。

### 1.3 市场窗口（2026-09 事实）

- Heroku 2026-02 宣布进入维持工程模式，不再增加新功能；"自托管 Heroku 替代品"需求处于高位。
- MinIO 社区版 2026-04 归档（2025-05 移除控制台功能，2025-12 转维护模式），S3 兼容存储出现空位。
- Coolify（约 61k stars）功能最全但自身资源占用以 GB 计，栈为 PHP/Laravel。
- Dokploy（约 37k stars）押注 Docker Swarm，`/proprietary` 目录采用 DSAL 非开源许可。
- Dokku（约 32k stars，MIT）持续活跃，但无 HTTP API、无 Web UI、单机设计。
- Docker Swarm 生态冻结，Docker Engine v29 出现影响 Swarm 的兼容性断裂（Portainer 2026 白皮书）。
- Coolify、Dokploy 均已提供 MCP 能力，但形态粗糙：Dokploy 官方 MCP 自动生成 546 个工具，tools/list 约 74k tokens（社区手工收敛版 27 工具约 8.6k tokens）。

结论：**轻 + 结构化 API + 统一集群 + AI Agent 原生**这一组合目前无人占据。

### 1.4 为什么不用 dokku 做核心

dokku 是单机设计：接口为 SSH + bash CLI 文本输出，扩展是 bash 插件。而本项目需要：

| 需求 | dokku 提供 | 缺口 |
|---|---|---|
| Console 端 / MCP / 结构化 CLI | `ssh dokku@host xxx` | 解析 stdout，无事务、无事件流、无错误码；dokku 升级即破坏解析层 |
| AI Agent 安全 | 单个 SSH 用户全权限 | 无 scope token、无审计、无 plan/dry-run |
| ≤10 台统一集群 | 单机设计 | 10 台 = 10 个独立盒子，中央控制面仍需自研 |
| Go 技术栈 | bash 插件 | 与项目取向相悖 |

dokku 保留两个用途：参考实现（发布流程、零停机、代理配置的既定做法）、潜在迁移兼容目标（远期）。

## 2. 目标设计

### 2.1 总体架构

```
CLI / Console 端 / MCP 客户端(v0.2) / REST / Webhook〔git push(SSH) 面已移除，ADR-0012〕
        │
        ▼
┌─ 控制面 fleetlyd server（Go 单二进制，运行于 Swarm manager）─┐
│  API 层        gRPC + grpc-gateway(REST/JSON/OpenAPI) + SSE    │
│  编排层        compose.yaml 解析（受控子集）→ 对账器 → 发布状态机 │
│  构建管线      Railpack / Dockerfile → BuildKit → 镜像        │
│  调度委托      Swarm 内置调度 + node label 约束（不自研）     │
│  状态层        SQLite(WAL) + 迁移 + 审计日志                  │
│  供应层        域名/TLS(Traefik)、env/密钥、S3、备份          │
└───────────────────────────────────────────────────────────────┘
        │  Docker API（本地 socket 管理全集群；成员管理与节点通信由 Swarm 承担）
        ▼
┌─ 节点：Docker Engine（Swarm mode）─────────────┐
│  Swarm services / overlay 网络 / 内置调度      │
│  Traefik（每节点）/ 执行中继 / BuildKit / 日志与指标 │
└────────────────────────────────────────────────┘
```

设计原则：

1. **API-first 铁律**：所有能力先有 REST/OpenAPI，UI/CLI/MCP 均为其客户端；没有 API 的功能不准进产品。
2. **对账式**：期望状态（spec + 数据库）与真实状态（Swarm service 状态、Traefik 配置）分离，对账器负责收敛与自愈。
3. **单二进制、多角色**：`fleetlyd standalone`（单机全功能，v0.1，安装时隐式初始化单节点 Swarm）/ `server`（运行于 Swarm manager，v0.2 多节点）；**无自研 node 协议**——成员管理、心跳与节点通信由 Swarm 承担（D2/D12）。
4. **基础设施只复用不自研**：构建、反向代理、**编排与调度（Docker Swarm）**、**应用模型（Compose 规范）**、指标存储、备份工具、对象存储全部用成熟组件。
5. **错误信息即产品**：错误码 + 原始上下文（stderr/事件）+ 修复建议 + 可订阅事件流，同时服务人类与 AI Agent（竞品差评最密集的类别，见调研报告第 6 节）。

### 2.2 组件与复用清单

| 领域 | 复用组件 | 语言/许可 | 自研部分 |
|---|---|---|---|
| 运行时 | Docker Engine API（moby/client） | Go, Apache-2.0 | 容器/服务生命周期封装、发布状态机 |
| 基础框架 | **lynx + google/wire**（`NewRunner` + `boot.Bootstrap` Wire 装配，D20；参考实现 messageloop 与 lynx-clean-template） | Go, Apache-2.0 / Apache-2.0 | 领域服务与端口适配器装配、生命周期（Start/Drain/Stop）、配置（lynx Config/Viper 适配）、平台日志（slog + zap contrib）、健康端点（healthz liveness/readiness）、进程内事件扇出（eventbus 仅作分发，events 落库仍走 SQLite Outbox） |
| 编排底座 | Docker Swarm（引擎内置，无需额外组件） | Go, Apache-2.0 | 只做集成——调度/成员管理/服务发现不自研（D12） |
| 应用模型 | Compose Specification（docker stack 语义） | Docker, Apache-2.0 | 受控子集校验、label 约定、平台覆盖层（digest/secret/路由/绑定）、stack 对账 |
| 构建 | Railpack + BuildKit（Dockerfile 兜底） | Go, MIT / Apache-2.0 | 构建队列、缓存、资源限制、镜像命名 |
| 入口 | Traefik（global，每节点）+ ACME（lego，控制面集中签发） | Go, MIT / MIT | 动态配置与证书集中下发、路由发布时机（health 门） |
| 状态 | SQLite（modernc 纯 Go）+ goose 迁移 | BSD-3 | schema、对账器、观测缓存与新鲜度契约、审计 |
| API | **gRPC + grpc-gateway/v2 + buf**（openapiv2 文档派生、protovalidate 校验；torchwood 范式，D21） | Go, Apache-2.0 / BSD-3 | proto 契约（`fleetly.{client,console,server}.vN` 分模块）、拦截器链（鉴权/限流）、自定义错误信封（ErrorResponse + snake_case + `disable_default_errors`）、genproto/SDK 生成 |
| CLI | lynx-go/commands + 平台 Go SDK（gRPC client，独立模块，torchwood 同型） | Go, MIT | 交互体验、输出格式（--json）；日志/事件长流走 gRPC streaming |
| git 拉源 | 系统 git | GPLv2（独立进程调用，不链接、不随发行物分发，无传染） | webhook 拉源 fetch 接线、bare 仓库对象库读取（git push(SSH) 收包面已移除，ADR-0012） |
| 日志 | 无 | — | 采集、落盘轮转、ring buffer、SSE |
| 指标(v0.2) | VictoriaMetrics（存储/查询）+ node_exporter（宿主）+ cAdvisor（逐节点容器指标：manager 无远端 Engine API） | Go, Apache-2.0 | 查询面、UI 图表、告警 |
| S3(v0.2) | 外部 S3 端点（**minio-go** 客户端〔2026-09-17 依赖复核选定〕；provider 抽象；打包 S3 延后到需求证据，见 D4） | Go, Apache-2.0 | 端点配置、凭证注入、备份策略、热备上传/回读 |
| 备份(v0.2) | restic | Go, BSD-2 | 调度、策略、恢复流程 |
| 镜像分发(v0.2) | zot registry（v0.1 免 registry：digest 引用本地镜像） | Go, Apache-2.0 | 构建推送、节点拉取（`--with-registry-auth`） |
| MCP(v0.2) | 官方 modelcontextprotocol/go-sdk | Go, MIT/Apache-2.0 | 精选工具面、scope 映射、审计 |
| Console 端（曾用名 Web UI，2026-09-17 更名） | React + Vite + shadcn/ui（SPA） | MIT | 全部界面 |

依赖许可证以实际锁定的版本为准；默认发行包不引入 AGPL 组件。

架构支持：v0.1 支持 amd64 与 arm64 的同架构构建与运行；一次构建多架构镜像列为 v0.2 评估项。

### 2.3 状态模型（分层原则）

状态分三层，完整判据、字段与契约见[控制面状态模型专项](2026-09-17-state-model.md)：

1. **权威态（SQLite）**：只存无法从真实态重算的知识——期望态（apps/compose 期望态/domains/env 密文/placement）、历史（deployments/events/audit）、凭证（tokens）、版本快照（revisions）、备份与恢复台账。
2. **派生缓存**：Docker/Swarm 观测快照（nodes/services/tasks/volumes），可整表重建，每行带 `observed_at/stale`，**禁止用于决策**。
3. **实时直读**：写操作先直读底座并以对象版本作乐观令牌（`E_STATE_VERSION_CONFLICT`）。

核心表（草案）：apps、deployments（含 `kind=deploy|rollback` 与 recovery 字段：归位不创建新记录，见[发布专项](2026-09-17-release-semantics.md)）、revisions（归一化 compose + 平台覆盖层快照，见[发布专项](2026-09-17-release-semantics.md)）、env_vars、domains、placements、volumes、nodes（v0.1 起即有〔单机同路径〕；观测缓存，**不承诺「最后心跳」**）、tokens、events、audit_log、state_backups、orphans。

密钥方案：envelope 加密（**age**〔filippo.io/age〕，2026-09-17 依赖复核选定——密钥即文件，与主密钥文件形态天然契合），主密钥存于控制面主机（文件权限保护）且与备份数据分离保存，运行时通过环境变量或 docker secrets 注入。**已知边界：Swarm service spec 中的 env 为明文，raft 备份会携带（诚实告知；v0.2 评估 secrets/tmpfs 注入）。**

数据保留（默认值，可配）：部署记录每 app 50 条、可重放版本 5 个、事件 30 天、审计 1 年、应用日志 7 天轮转；SQLite 定期归档/VACUUM，防止无限膨胀。

### 2.4 应用描述：Compose 规范（唯一应用模型）

应用定义 = 仓库内 `compose.yaml`（Compose Specification，docker stack 语义）；平台不定义自研 spec，只维护**受控子集 + 最小 label 约定**（D14）：

```yaml
name: my-api
services:
  web:
    build: { context: . }              # 无 dockerfile → Railpack 自动；有 build.dockerfile → Dockerfile；仅 image → 镜像模式
    expose: ["8080"]                   # 路由目标端口（取首个）
    labels:
      fleetly.domains: "api.example.com, www.api.example.com"   # 平台约定：域名列表（逗号分隔；TLS 自动）；有该 label 的服务即入口
    healthcheck:
      test: ["CMD", "/app/healthcheck"]   # 未写的子字段取平台默认（5s/3s/3/10s）；完全无 healthcheck → health_gate=none（警告）
      start_period: 10s
    environment: { NODE_ENV: production }
    deploy:
      replicas: 1
      update_config: { order: start-first, failure_action: pause }   # failure_action 必须为 pause（平台管理）
      resources: { limits: { cpus: "0.5", memory: 256M } }
  worker:
    build: { context: . }
    command: node worker.js
volumes:
  data:
```

> secrets v0.1 暂不接入：compose 声明 `secrets`（服务级或顶层）在校验层显式拒绝（`E_COMPOSE_UNSUPPORTED`，reason 注明 v0.2 平台密钥库接入后开放）——比放行到准备期晚期才失败诚实（2026-09-20 评审 C1 裁决）。

**平台约定（最小集，全部使用 compose 原生字段）**：

| 能力 | 承载 | 说明 |
|---|---|---|
| 域名/TLS | 服务 `labels: fleetly.domains`（**逗号分隔列表**，如 `"example.com, www.example.com"`） | 列表内域名同服务同路由，证书按 app 域名集合出一张多 SAN 证书；归一化（trim/小写/IDN→punycode）；每服务 ≤5、每 app ≤10；同域名出现在两个服务 → 409 `E_DOMAIN_CONFLICT`；通配符 v0.2 拒绝（需 DNS-01）→ `E_DOMAIN_UNSUPPORTED`；代理无关，核心契约不出现 Traefik 概念 |
| 路由目标端口 | `expose` 首个端口 | 未声明则不发布 |
| 放置（v0.2） | `labels: fleetly.placement.node`；有卷应用由平台自动绑定 | 用户 `deploy.placement.constraints` 仅允许 `node.labels.fleetly.*` 命名空间 |
| 定时任务（v0.2） | 服务 `labels: fleetly.cron`（+可选 `fleetly.cron.timezone`、`fleetly.cron.timeout`） | 带该 label 的服务不按长驻部署，由调度器创建一次性 Swarm job；`replicas` 必须 0/省略；违反 → `E_COMPOSE_UNSUPPORTED`（reason 细分） |
| 密钥 | ~~compose `secrets`~~ **v0.1 暂不接入，显式拒绝**（`E_COMPOSE_UNSUPPORTED`；2026-09-20 评审 C1：平台密钥库未接入前，放行只会在准备期晚期失败且错误码误导） | v0.2 平台密钥库接入后开放：映射为 Swarm secret（名 `fleetly-<team>-<prj>-<app>-<name>-<hash8>`，v0.3 三段命名，[rbac-teams §4.3](2026-09-23-rbac-teams.md)；file target 保持 compose 名），应用读 `/run/secrets`；`env_file` 允许但仅限非密钥 |
| 变量合并 | 三层优先链：`env_file` < `environment` < 平台 env_vars（2026-09-17 审核裁决） | 同键平台层覆盖；`desired-hash` 与 revision 快照按**合并结果**计算（`key:sha256` + 来源标注）；`fleetly env set` 创建 pending 变更、**随下次部署生效**（不立即改运行服务——env 变更经部署固化，与发布专项 D-REL-9 一致）；覆盖键在 plan/diff 告警 `W_ENV_PLATFORM_OVERRIDE`；模板自动连接串 = `source=system` 平台 env（只读展示） |
| 服务命名与网络 | Swarm 服务名 `fleetly-<team>-<prj>-<app>-<service>`（v0.3 三段命名，D-W0-4 二修，[rbac-teams §4.3](2026-09-23-rbac-teams.md)；适配器内；卷名族 `fleetly-<app>-<key>-<appid8>` 不变——appid8 天然全局防撞）；每 app 专属 overlay 网络（名 `fleetly-<team>-<prj>-<app>-net`）+ 服务别名 = compose 服务名 | 集群全局命名空间防撞名（app 名 project 内唯一，全局唯一由 team·prj 段承载——两个项目各有 `web`/`db` 不冲突）；app 内短名互访与 compose 语义一致、跨 app 网络隔离；平台命名不进归一化 compose；v0.2 跨 app 互访（数据库模板）由平台牵线共享网络，随模板设计裁决 |
| 变量插值 | 关闭 `${VAR}` 与 `.env` 插值 | 消除环境相关不确定性；归一化按字面处理 |
| 受管字段 | `deploy.update_config.failure_action` 必须 `pause`（或省略）；`monitor` 必须省略或 5s | 违反 → `E_COMPOSE_MANAGED_FIELD`，校验拒绝、不静默覆盖 |

**子集与拒绝清单**（显式报错 `E_COMPOSE_UNSUPPORTED`，不静默）：
- 支持：多服务（web/worker 等）、`build`/`image`、`healthcheck`、`environment`/`env_file`、命名卷与栈内网络、`deploy.*`（除受管字段）、`stop_signal`/`stop_grace_period`。**支持集以 `internal/compose/testdata/whitelist.golden` 为准**（顶层 + 服务级白名单键集的 golden 快照，与校验代码集合一致性由测试钉死——白名单增删忘改文档/golden 即测试红；2026-09-20 评审 C3）。
- v0.1 拒绝：`depends_on`、`extends`、`include`、`profiles`、`configs`、`secrets`（评审 C1：平台密钥库未接入，显式拒绝）、外部网络、`network_mode: host`；`deploy.mode: global` 亦在 Load 期显式拒绝（`E_COMPOSE_UNSUPPORTED`——2026-09-20 评审 M1-4：单节点下 global 的副本语义与失败停机语义（scale=0 对 global 无效）未实现，v0.2 多节点开放）；v0.3 受控扩展。
- 危险字段（`privileged`/`cap_add`/`pid`/`devices`/docker.sock 挂载/宿主路径 bind）默认拒绝，需 admin scope 显式开启并写审计（Coolify CVE-2025-34159 的根因即低权路径挂载宿主根）。

`fleetly init` 生成 `compose.yaml`（已有 compose 文件则直接接管）；`fleetly plan/apply/diff` 以**归一化 compose 差异**为核心；对账器持续检测漂移（检测默认开、收敛 per-app opt-in，见 D11）。

**期望态治理规则（缩水版）**：
- 单一真源：compose 文件为唯一期望态；平台管理字段（镜像 digest、secret 值、路由绑定、节点绑定、证书轮转）不进文件，UI 只读展示并标注来源，禁止静默双向合并（Fly 混乱与 ArgoCD self-heal 事故的教训）。
- 省略 = 删除（volumes 数据例外）：移除服务 → 删除对应 Swarm service；移除卷声明 → 解挂载，**数据不随声明删除**；删除仅显式 `--delete-volumes`（v0.2）或孤儿卷清理（见[放置专项](2026-09-17-stateful-placement.md)）。

**plan/apply 语义**（面向 AI Agent 的一等接口，照抄 Railway 已被生产验证的最小集）：
`--json` 输出；三态退出码（0=无变化 / 2=有变化 / 1=错误）；secrets 默认脱敏；plan 可落盘为 artifact，apply 前校验 etag 防 stale 并发；破坏性操作要求 `--confirm-destructive`。

### 2.5 发布流程（零停机）

```
push/webhook → 源获取 → 构建(Railpack/BuildKit，带缓存)
  → 镜像不可变 digest → Swarm service 更新（start-first + healthcheck + failure-action=pause）
  → health gate（Swarm executor 等待 healthy，平台复核）→ 路由发布 → 旧任务下线
  → 观察窗（默认 60s，只告警；rollback 为平台侧 opt-in）→ 状态落库/事件广播
失败任一步 → 不切流量（未切流=归位重放）；已切流失败 → 告警（或 opt-in 回滚）；结构化错误码与建议
```

**不变量（竞品事故教训 + Swarm 源码级验证；完整语义见[发布专项](2026-09-17-release-semantics.md)，Spike B 验收依据）**：

- 路由发布严格晚于 health gate：**Traefik 的 Swarm provider 不检查健康**（task 进 running 即注册，源码验证），因此关闭自动发现，由控制面在 health 通过后下发路由（规避 Dokku #8974 同类事故）。
- 入口配置写入原子且故障隔离：先校验后落盘，空 routers/services 不落盘，单应用坏配置不得影响其他应用路由（Dokploy #5189）。**Spike B provider 四态实测（2026-09-17）**：Traefik 只拒绝显式空 map（保留旧配置），**裸 `{}` 载荷会清空全部路由（实测 404）**，且坏 router 会被连同整份配置一起应用——因此「非空」校验的责任在控制面：routers/services 键必须存在、合成结果必须非空，校验不过不下发；配置服务不可达时 Traefik 保留上一份好配置继续服务（实测）。
- **失败 = 不切流量**：更新默认 `start-first`（有卷/固定端口强制 stop-first，见降级表）+ Swarm `failure-action=pause`（冻结更新、旧任务保留）；**不使用 Swarm 原生回滚**（自动回滚清空唯一 PreviousSpec、不覆盖 PENDING）——回滚由平台按完整版本快照单层重放（见 D15）。
- **观察窗默认只告警**；`rollback` 为平台侧 per-app opt-in（v0.1 无文件字段，见[发布专项](2026-09-17-release-semantics.md)）；窗口后崩溃只告警。**stop-first（有卷/固定端口）失败强制归位**，不可关闭（不回滚=永久宕机）。
- 失败分类以「新版本是否曾健康」为界：未切流=归位重放（start-first 下同内容重放通常零任务变动，Spike B 验证）；已切流=观察窗判定；首发失败（无版本可回）→ scale 0 保留现场。
- 回滚 = 单层实现：平台保留最近 5 个已验证版本（归一化 compose + 平台覆盖层快照，不只 digest），列表内任意重放；数据库迁移/持久数据/secret 值不承诺回滚（secret 取当前值）。
- 连接治理（2026-09-17 Spike B V4 实测改写）：keep-alive 连接池会复用已退出的任务（Dokploy #5281 同型）；用户可见失败的真实来源是 **in-flight 非幂等请求被 kill（必现 502）**——治理主键 = **应用侧 SIGTERM 优雅退出**（drain 在途请求后再退，v0.1 文档提供正确实现模板；注意 Go 默认 os.Exit 陷阱）；Traefik `serversTransport`（idleConnTimeout/maxIdleConnsPerHost）降为**辅助**（只治理空闲池，对 in-flight 无效）；幂等 GET 类请求 Go transport 自愈可观测到零失败。WS/SSE 明确「断线由客户端重连」语义。
- Webhook 安全：强制签名校验（GitHub/Gitea 等）+ 时间窗防重放 + 按 revision 幂等去重。
- 并发控制：同一 app 同时只允许一个进行中的部署（互斥 + 队列）。
- 运行期语义（观察窗之后）：容器退出由 Swarm `restart-condition=any`（delay 5s）重启；平台只告警一次并建议手动回滚（不做计数升级），不自动回滚。
- 超时与卡死：Swarm 更新在「新任务无法调度」时**没有超时**（源码 TODO）——平台以发布看门狗（`deployTimeout=300s`）兜底；确定性预检仅限平台自身对象（绑定节点状态、镜像/secret/端口），**不做 Swarm 调度约束预检**（见 §7）。

**默认参数（compose 字段原样映射 + 平台管理项，v0.1 除 compose 字段外不可配置）**：

| 参数 | 默认值 | 来源 |
|---|---|---|
| 更新顺序 | `deploy.update_config.order`（start-first；有卷自动 stop-first） | compose 字段（平台校验） |
| 更新并行度 / 间隔 | `parallelism` / `delay` 照用（默认 1 / 0s） | compose 字段 |
| 更新失败动作 | 固定 `pause`（其他值 → `E_COMPOSE_MANAGED_FIELD`） | 平台管理（D15） |
| 更新监控窗 | 固定 5s（只判定启动期失败） | 平台管理 |
| 发布看门狗 | 300s（含 PENDING/停滞） | 平台默认（v0.1 无文件配置） |
| 发布观察窗 | 60s | 平台默认（v0.1 无文件配置） |
| 观察窗动作 | `alert`（默认；`rollback` 平台侧 opt-in） | 平台默认（D15） |
| healthcheck | compose `healthcheck`（未写子字段取平台默认 5s/3s/3/10s；无 → `health_gate=none`） | compose 字段/平台默认 |
| stop signal / grace | `stop_signal` / `stop_grace_period`（默认 SIGTERM / 60s） | compose 字段/平台默认 |
| 版本保留数 | 最近 5 个已验证 compose revision | 平台默认 |

**零停机降级边界（必须显式告知，不静默降级）**：

| 场景 | 行为 |
|---|---|
| 持久卷（local volume） | 双任务并发挂同一卷有数据风险 → `stop-first`，存在停机窗口；同时按平台绑定钉节点（不迁移，见[放置专项](2026-09-17-stateful-placement.md)） |
| 固定 host 端口 / global 服务 | 同节点无法 start-first（端口冲突无校验）→ `stop-first` 或逐节点替换 |
| 数据库迁移类变更 | 部署不保证零停机，用户自担，文档提供迁移指引 |
| WS / 长连接在途请求 | 只保证新连接零失败；连接池治理见上文不变量 |
| 镜像从 registry 拉取时 | 拉取失败任务被拒 → 发布前确保推送完成；本地已有镜像容错依赖 Engine ≥29.7 |
| 观察窗内不稳定（start-first 正常路径） | 默认告警 + `app=degraded`；平台侧 opt-in `rollback` 才自动回滚（走同一健康门与观察窗） |
| health 通过后 5s 内崩溃 | Swarm monitor 窗不保护已切流流量（start-first 下旧任务已下线）——归入观察窗判定 |
| stop-first 失败恢复 | 强制归位重放，停机持续到恢复完成，`downtime_ms` 如实累计；恢复失败 = critical |
| 无 healthcheck | `health_gate=none` 显式降级（观察窗只看退出与副本水位）+ 警告事件（Spike B3c 实测：无门时容器 start +29ms 即进端点，6.1s 预热窗内 27 次连续失败——降级有真实暴露，警告文案必须醒目） |
| 回滚自身失败 | 不再二次自动回滚；critical + 对账对该 app 只检测不收敛，等人工/Agent |
| 回滚目标镜像不可得（v0.1 无 registry） | 回滚前 preflight + `E_IMAGE_UNAVAILABLE` + 警告，不静默失败 |

从 v0.1 起镜像一律以不可变 digest 引用：单节点免 registry（本地镜像零 pull），多节点走 zot（v0.2）。

### 2.6 多节点模型（v0.2，底座 = Docker Swarm）

- **底座**：Docker Swarm（引擎内置，无需额外组件，见 D12）；控制面运行在 manager 节点，通过本地 Docker API 管理全集群；成员管理、心跳、服务发现、调度、任务生命周期全部由 Swarm 承担。
- 单节点（v0.1）同样是单节点 Swarm（安装时隐式 `docker swarm init`，对用户透明）；从单机到多节点 = `docker swarm join`，无重构。
- **调度**：Swarm 内置（bin-pack/spread）+ node label 约束；不自研调度器或成员协议。
- **失联与故障语义**：对齐 Swarm 心跳（理论 5s × 3 ≈ 15–16.5s；**Spike C2 实测 2026-09-17：判定 DOWN 13.5s、窗口 12.4–13.5s、替代任务 Running ≤18.7s、节点回岗零回迁、瞬时副本过冲 3/2**——对外口径用「~13s 判定、~19s 完成重调度」）；节点 DOWN 后 **stateless 服务自动重调度**；**stateful 服务由平台绑定自动钉住**——节点消失时任务停留 PENDING、应用进入 blocked 可见态，不迁移、不自动换点（迁走会得到空卷）；数据安全由部署前哨兜底（目标节点 ≠ 卷数据节点 → 409）。节点恢复后 Swarm 不自动回迁；不做 rebalance（节点排布调整用 `docker node`，见[放置专项](2026-09-17-stateful-placement.md)）。
- **镜像分发**：v0.1 单节点 digest 引用免 registry；v0.2 引入 zot（manager 本地卷、平台绑定钉住），经 Traefik 暴露于平台域名 `registry.<base-domain>`、证书走集中 ACME（公信 CA）——**worker dockerd 零配置信任**，镜像引用形如 `registry.<base>/apps/<app>@sha256:…`，拉取经任意入口节点进 overlay 到 zot；自签 + 逐节点 `insecure-registries` 方案被否（需平台没有的远端 daemon 配置通道）。**前置条件：v0.2 多节点要求安装时提供平台基础域名**（面板与 registry 各占子域；单节点 v0.1 不需要）。服务创建时 `--with-registry-auth` 由 Swarm 原生分发凭据；zot 数据不进控制面备份（镜像可重建，文档注明）。
- **入口（每节点入口 + 集中证书；2026-09-17 审核裁决）**：Traefik = **global service，每节点（含 manager）一个**，host 模式发布 80/443；replicated-1 单入口形态被否（与 drain 语义矛盾、入口单点），「节点文件下发」不可行（无远端访问通道）——路由与证书一律由控制面经 **HTTP provider** 下发，不启用 Swarm/Docker provider 自动发现；取不到配置时 Traefik 保留上一份成功配置（控制面故障入口不坏）。
- **证书集中化**：控制面内嵌 ACME（lego）集中签发，证书存平台、随 HTTP provider 动态配置下发（Traefik `tls.certificates`）；v0.2 用 HTTP-01——各节点 Traefik 把 `/.well-known/acme-challenge/*` 反代到控制面，任意节点可解挑战，零 DNS 服务商集成；DNS-01/通配符留 v0.3 按服务商接入；**每节点独立 ACME 被否**（LE 重复证书限额 + 续期风暴 + N 份 acme.json 不可维护）。Traefik 轮询 manager 上控制面配置端点（Header token；跨公网走平台域名 HTTPS）。
- **DNS 契约与故障转移口径**：A 记录指向**全部**节点 IP（TTL ≤300s），UI/向导列出并校验（`fleetly domains verify`）；入口冗余 = **连接级**（节点不可达时客户端换下一 A 记录重试），非健康驱动故障转移、非 VIP；keepalived/VIP 与云 LB 只做文档配方不进产品，强入口可用性需求走 v0.3 Tunnel。
- **执行中继（Web 终端底座，D19）**：`fleetly-exec` global service，每节点一个任务，仅挂内部系统网络（不发布 host 端口、不挂应用网络）、挂载本节点 docker.sock；API 面收窄到 `healthz`/`exec` 且只对带 `fleetly.app` label 的容器（其余 403）；集群 token 经 Swarm secret 下发；控制面经 `tasks.fleetly-exec` DNS + task→NodeID 反查节点（成员发现零自研）；会话空闲 10 分钟/硬上限 30 分钟、`terminal` 独立 scope（默认仅 admin；MCP 工具面不暴露终端）、起止入审计；流式会话 = coder/websocket（torchwood 同款），SSE 用标准库。通用 Docker API 代理与卷/镜像/节点操作仍禁止（放置专项 §7 例外条款）。
- **能力边界（对外口径）**：统一管理 + Swarm 调度；自动迁移仅限无卷无状态服务；无跨节点共享存储（卷本地，CSI 实验性不采用）；**远端节点 local 卷不可经 manager 枚举/删除**——卷删除与校验由用户按文档在节点上执行（不建维护作业）。
- **HA 边界（对外口径）**：
  - 2 台**得到**：无状态服务进程级 HA（失联 15s 量级判定 + 自动重调度；重调度窗口内该 app 短暂不可用，如实口径）；节点可 drain，无状态负载维护新连接零失败（连接级重试语义，见入口与维护窗口口径）；控制面故障不影响应用运行（应用运行不依赖控制面）。
  - 2 台**得不到**：管理面 HA（**不做 2 manager**——quorum=2 时任一台失联管理即不可用；2 台正解 = 1 manager + 1 worker + 冷备；管理面 HA 需 3 manager，成本另计）；有状态 HA（local 卷不跟随；DB 所在节点失联 = 该库不可用，恢复走备份重放 + rebind）。
  - 口径纪律：**「2 台 ≠ 全面 HA」**——初始化向导/UI/文档必须显式说明，防止用户按「全面 HA」预期加第二台后把有状态单点当产品缺陷。
- **维护窗口语义（配合 1~2h/周 预算）**：
  - 无状态节点：drain → 升级 → 回岗，新连接零失败（连接级重试语义；在途连接可能中断一次，指引先摘 DNS——入口与故障转移口径见上文）。
  - 有状态节点：drain 期间该应用停机（绑定任务 PENDING、应用 `blocked`），回岗后自动回绑、数据在本地卷不丢；要缩短停机 = 备份恢复到另一台 + 人工 rebind（见[放置专项](2026-09-17-stateful-placement.md) §2.7）。
  - 节点操作仍用 `docker node` 原生命令（不建生命周期 API，D18）；v0.2 以文档 + UI 指引覆盖，验收含 2 节点演练（§4.3）。
- **控制面灾难恢复**：备份等序原则（SQLite 允许比 raft 新、绝不更旧）+ 固定恢复顺序（raft → SQLite → 控制面 → 只读观察 → 人工处理差异 → 收敛）+ 恢复阶梯 L1/L2；L3 场景（仅容器存活）runbook 化、不建机制；**单节点整机磁盘丢失不保应用与数据**（明确边界）。详见[状态模型专项](2026-09-17-state-model.md)。
- **引擎门禁**：Engine ≥ 29.8.1、iptables 后端（nftables 暂不支持 Swarm 节点）、升级前跑回归矩阵（服务名 DNS / ingress / secrets / 卷 / containerd 存储双模式）。

### 2.7 仓库结构（monorepo）

```
/                Go module 根（cmd/fleetlyd、cmd/fleetly、internal/、pkg/api）
/console         Console 端（React SPA，Vite）
/docs            本目录
/deploy          bootstrap 脚本、systemd unit、安装/升级
```

### 2.8 长期演进（核心-适配器边界，目标 5 年+ 不改核心）

**核心的定义**：领域语义（App / Deployment / Domain / Env / Volume / Node）+ 期望态对账 + 发布状态机 + 契约（Compose 子集 + label 约定 / API / 事件 / 错误码）。核心代码、数据库字段、UI 文案中不允许出现第三方概念（Traefik label、Docker 结构体、Railpack plan 等），外部表示只存在于适配器内部——这是 dokku「解析 CLI 输出」教训的一般化：外部表示一旦进入核心，上游升级就会变成我们的破坏性变更。

**端口清单（稳定 SPI，9 个）**：

| 端口 | 当前实现 | 退出预案 / 第二实现 | 半衰期风险 |
|---|---|---|---|
| Runtime | Docker Engine API（moby/client） | containerd / Wasm 运行时 | 中 |
| Builder | Railpack | Dockerfile + BuildKit（v0.1 即一等路径） | 高（pre-1.0） |
| Proxy | Traefik | Caddy | 中 |
| Registry | zot | distribution | 低 |
| ObjectStore | 外部 S3 端点（内置端点配置与凭证注入） | 打包 S3（RustFS 候选，触发式引入） | 高（组件最年轻，暂不打包） |
| Metrics | VictoriaMetrics | Prometheus / 任意 PromQL 兼容后端 | 中 |
| Secrets | 内置 envelope 加密 | 外部 KMS / Vault | 低 |
| Auth | 本地 token scope | OIDC / 反向代理头 | 低 |
| Orchestration | Docker Swarm（引擎内置） | k3s driver / 自研 node（退出预案，见 D12 / 3.1） | 中（依赖 Engine 演进，需版本门禁） |

**机制保障**：
- 适配器以 **lynx.Service** 注册（Name/Init/Start/Stop，D20）：生命周期交框架托管，实现 `Checker` 的组件自动进入平台健康检查；依赖装配统一走 **google/wire 编译期生成**（`boot.Bootstrap`），禁止运行时反射 DI。
- 每个端口定义 Go interface + **conformance 测试套件**；套件先覆盖已有双实现的端口（Builder、ObjectStore），其余端口随退出预案触发补齐——新适配器必须跑通套件才算可用，规则不变（k8s CSI/CRI 模式）；没有 conformance 的「可替换」只是愿望。
- 每个关键依赖维护**退出预案（exit plan）**：指认替代实现与迁移成本；Builder 与 ObjectStore 天然有两个实现，替换已被预演。

**契约版本化纪律**：
- Compose 子集与 label 约定：白名单/拒绝清单只增不减；`fleetly.*` label 契约化（版本化、只增不改语义）；不自研 schema（compose 官方 schema 校验 + 平台子集校验）。
- API：proto package 版本化（`fleetly.{client,console,server}.vN`）+ `buf breaking`（FILE 规则）为兼容门禁；REST 路径 `/v1` 由 proto `google.api.http` 注解派生；弃用窗口（N-2 支持），CLI 对弃用项给出迁移命令；OpenAPI（openapiv2）为派生物、禁止手改。
- 事件与错误码：注册表管理（**唯一真源为代码内注册表**，文档域清单为定义性说明），稳定字符串、永不复用、只新增。
- 状态库：只做加法迁移；回滚 = 恢复快照（不写 down migration）。
- 引擎门禁：Engine 版本下限与升级回归矩阵（见 2.6）；控制面与节点之间无自研协议——节点通信与成员管理由 Swarm 承担，平台只消费 Docker API。

**数据长寿**：期望态可从 DB + 仓库 compose 文件完整重建；实际态可从**服务/卷 label（最小集）**重建；一键导出为 tar（compose + SQLite 导出 + 卷，不含 token/主密钥/证书私钥，**不承诺免重建**，见[状态模型专项](2026-09-17-state-model.md)）。代码库整体替换后，用户仍可带走应用与数据。

**插件边界（L1 only）**：只做端口适配器（第三方经上游代码贡献），不做动态加载、不做通用插件 API、不开放对账器/状态机扩展点——每个插件都是永久兼容性约束，等于给核心上锁（Waypoint / MinIO 教训）。进程外集成仅通过稳定事件流 / Webhook，不触碰核心语义；L2 出现真实需求时再评估。

**AI 侧的长寿赌注**：不是赌 MCP 协议本身，而是「机器可读的能力面 + scope + 审计」；MCP 只是该能力面的一个适配器，协议换代时换适配器即可。

## 3. 关键决策及理由

| # | 决策 | 理由 | 被否方案及原因 |
|---|---|---|---|
| D1 | 自研 Go 控制面 | AI/Agent Native 与 UI/MCP 承诺需要结构化状态与 API-first 地基；统一集群需求已排除 dokku | 包装 dokku：无结构化状态、解析层长期维护税、单机；纯 k8s/Tsuru：过重，违背 10 台定位；Coolify 二次开发：PHP/Laravel + 自身过重 |
| D2 | 统一集群，底座 = Docker Swarm（引擎内置）；控制面运行于 manager，不自研分布式核心 | 三条硬约束（不自研分布式核心 / 轻量 / 五年可用）加权下 Swarm 得分最高：零新增组件、调度与成员管理全内置、health gate 与失败不切流原生可用（源码级验证）；专项评估见 [Swarm 评估报告](../research/2026-09-17-swarm-substrate-assessment.md) | 自研 node 协议：分布式核心工作量集中且最难测试（本轮推翻）；机队模式：无统一调度；Nomad：官方小集群 sizing 8-16GB 级 + BSL 许可；k3s：2C/2GB 起 + 用户拒绝 K8s（保留为可选 driver） |
| D3 | Console 端用 React SPA | 生态与组件库最丰富、人才与参考实现最多，长期维护与招人成本最低；MIT | Go 模板 + HTMX：更轻、单二进制，但交互上限低；Svelte / Solid：运行时更小、signals 模型更契合高频流式渲染，但生态规模小；本轮按团队选型定为 React，console 与 API 严格解耦故后续仍可换 |
| D4 | S3 做 provider 抽象，**v0.2 只支持外部端点**；不打包 S3 服务（RustFS 为将来候选，触发式引入）〔2026-09-17 D4 复议；**2026-09-20 V2-2：触发条件成立——RustFS 1.0 GA + 用户裁决引入，形态 opt-in 管理组件**（`s3.backend=rustfs`，默认装不捆绑；同节点备份诚实标注便捷层非灾备，DR 仍指向外部端点/跨节点），见 [v0.2 规划](../plan/2026-09-20-v0.2-plan.md)〕 | Dokploy 地板即外部 endpoints 形态（AWS/B2/R2/MinIO/Wasabi，Rclone 备份）；「MinIO 归档空位」是市场机会非约束；provider 抽象保留切换能力 | 打包 RustFS：许可与 UI 均优，但无需求证据前打包 = 过度设计（奥卡姆裁决 F5）；Garage：AGPL，仅外部端点；SeaweedFS：运维面大 |
| D5 | 开源核心 + 商业版，核心 Apache-2.0 | 专利授权、商业友好；核心保持完整可自用，不做 MinIO 式"先送后收" | MIT：缺少专利条款；AGPL：限制商业路径 |
| D6 | 构建用 Railpack + BuildKit，Dockerfile 兜底 | 自研构建系统是无底洞；Nixpacks 已转维护模式，Railpack 是其官方继任且为 Go 库可复用 | 自研 buildpack 体系；herokuish：bash + 与 dokku 生态绑定 |
| D7 | 状态用 SQLite | 零运维、单文件、支持 10 台规模足够；备份即复制文件 | etcd：为分布式一致性设计，杀鸡用牛刀；Postgres：平台自身变重 |
| D8 | MCP server 移至 v0.2（**2026-09-22 修订〔D-W5-1，用户直裁〕：暂缓至 v0.3；2026-09-23 再修订〔V3-2，用户直裁〕：继续降低优先级——退出 v0.3 主线，v0.3 先团队/RBAC 后生产深化；D10 纪律条款继续随迁挂账，重启时机随真实需求**，见 [v0.3 规划 §1](../plan/2026-09-23-v0.3-plan.md)） | v0.1 收敛范围；API-first 下 MCP 是服务层之上的薄适配层，晚一期不欠技术债 | v0.1 即交付 MCP：会挤占部署闭环的联调时间 |
| D9 | 镜像从 v0.1 起用不可变 digest | v0.2 多节点分发改造若发生在部署管线中途，代价远大于一开始就用 digest | tag 引用：早期省事，后期改管线 |
| D10 | 工具面精选（MCP ≤30 工具 + action 枚举 + scope） | Dokploy 546 工具/74k tokens 反面教材；精选是业界公开收敛（Dokploy 546→27、Railway 远程 7、Coolify 只读起步、Portainer 98→15）。配套机制：破坏性操作两段式（预览→确认），`confirm` 字段从 input schema 隐藏防模型自填；scope 在**执行层**强制（每次 tools/call，非仅 tools/list 过滤）；每次调用写审计；响应默认脱敏与截断；提供只读 token 模板 | 由 OpenAPI 自动生成全量工具：上下文灾难 + 坏 schema 可致客户端整体拒收（Anthropic 案例）；仅 tools/list 过滤做权限：可被 tools/call 绕过（CVE-2026-46519） |
| D11 | 漂移检测默认开、自动收敛 per-app opt-in | 检测是用户与 AI Agent 都需要的事实来源；PaaS 阵营无人做全（差异化空白区），K8s GitOps 证明需求同时暴露 self-heal 事故（ArgoCD #13598）；Terraform #35382 证明「检测」与「变更」应解耦 | 全自动收敛：事故中会被用户强制关闭且「Synced ≠ desired」；不做检测：与对账式原则矛盾，放弃差异化 |
| D12 | 采纳 Docker Swarm 作为多节点底座（v0.1 单节点即 Swarm，对用户透明）；退出预案 = k3s driver 或自研 node（见 3.1） | 专项验证结论：health gate / 失败不切流由 Swarm 原生兑现（源码级）；单版自动回滚原生存在但被否（清空唯一历史槽，见 D15），平台改用 pause + 快照重放；v0.1 单节点 digest 引用免 registry 实测可行；Engine 29.x 约 21 条 Swarm 修复、未 deprecated、Mirantis 支持至 2030；代价（Engine 破坏式升级 / 有状态弱 / 单 manager SPOF）均有具体缓解（锁版本+回归矩阵 / 状态外置+绑定钉住 / 冷备+演练） | 自研 node 协议：工作量与风险最高的分布式核心（推翻原 D2）；完全不采用集群底座：放弃统一调度与自动重调度；k3s：资源与产品身份不符（保留 driver）；Nomad：BSL + 重型 sizing |
| D13 | 核心-适配器分层（§2.8）：核心 = 语义 + 对账 + 契约；插件只做 L1 端口适配器；v1.0 冻结核心契约 | 5 年+「不动核心」的实现方式是收窄核心定义并把第三方概念全部赶入适配器；端口 + conformance 套件使替换成为工程事实而非愿望；动态插件会成为永久兼容性约束，冻结演进 | 通用插件 API / 动态加载：Waypoint、MinIO 的扩展点教训；把第三方语义留在核心：dokku CLI 解析教训，上游升级即破坏 |
| D14 | 应用定义与运行时模型 = Compose 规范（`compose.yaml`，docker stack 语义）；受控子集 + 最小 label 约定；不做自研 spec | 小团队无力维护自有规范；compose 的生态、官方 schema、AI Agent 训练覆盖与迁移入口现成；自研 spec 的每个字段都是永久兼容性负担（与 D13 同源）；Swarm 原生支持 stack 语义（`deploy.*` 映射、health gate、start-first） | 自研 `fleetly.yaml` + SchemaStore + 字段归属表：维护税与采用摩擦（本轮推翻）；compose 仅作迁移输入（原 D14，用户仍要学平台私有格式）；任意 compose 全量语义（depends_on/extends/profiles 等）：实现面不可控，v0.1 显式拒绝、v0.3 受控评估 |
| D15 | 发布失败动作固定 `pause`，回滚由平台按归一化 compose + 覆盖层快照单层重放；观察窗默认只告警、rollback 为平台侧 per-app opt-in（v0.1 无文件字段）；stop-first 失败强制归位 | Swarm 原生回滚清空唯一历史槽且不覆盖 PENDING，无法兑现「任意版本重放」；pause 保留旧任务与服务 spec，恢复动作幂等可审计；默认告警与 D11「检测开、收敛 opt-in」一致，避免对无效回滚的震荡 | Swarm `failure-action=rollback`（历史槽失效、首发卡死）；两层回滚（两套判定权与审计）；窗口后自动回滚（抖动）；观察窗默认自动回滚（静默改变运行版本，用户裁决改为 opt-in） |
| D16 | 有状态应用默认自动钉住：label `fleetly.placement.node` 可选，平台绑定（**平台节点 ID 为锚**）持久保持；节点消失不迁移不换点；跨点移动仅经备份恢复 + 显式数据处置确认 | 有卷应用被迁移会得到空卷（源码级验证），数据安全必须是默认行为；平台节点 ID 抗重名/重建（显示名仅供人/Agent 读写）；卷-节点归属前哨把空卷事故变成显式 409 | 要求显式 pin（首部署摩擦、AI Agent 易漏）；hostname/别名作身份（重名机器静默接管）；自动换点/迁移（空卷事故） |
| D17 | 控制面状态三层：权威（SQLite，意图/历史/凭证）/ 派生缓存（观测快照，带 observed_at/stale，禁入决策）/ 实时直读（写前校验）；`nodes` 降级为观测缓存；备份等序 + 恢复期禁止自动收敛 | 双状态源无法消灭只能明确属主；把运行态当权威是漂移与误删的唯一通路；恢复期自动收敛在 DB 较旧时会静默回滚部署 | 全量镜像 Swarm 状态入权威（双写者）；不落缓存（无降级读、打爆底座 API）；恢复即自动收敛（静默回滚）；声称「最后心跳」（Swarm 不暴露该时间戳） |
| D18 | 对标基线 = **Dokploy 体验（地板）+ Cloudflare 式体验（方向）**；复杂度纪律：Dokploy 没有且无硬承诺的机制一律不做，预算投向对标缺口（数据库托管提前、监控/通知、模板、Web 终端、Cron） | 小团队需求不极端；机制复杂度不构成 UX，对标缺口构成 UX（Dokploy 无熔断/rebalance/adopt/DR 阶梯/导出合同也做到头部体验）；我们保留的 pause+重放、plan/apply、漂移、错误透明、统一集群恰是 Dokploy 弱项 | 用内部机制做差异化（方向错误）；为「以后可能需要」预建机制（未来需求是猜测不是约束） |
| D19 | Web 终端经**执行中继** `fleetly-exec`（Swarm global service）实现：仅挂内部系统网络、不发布端口；API 面仅 `healthz`/`exec` 且只对带 `fleetly.app` label 的容器；集群 token 经 Swarm secret；成员发现复用 Swarm（task→NodeID 反查）；`terminal` 独立 scope + 会话限制（空闲 10m/上限 30m）+ 审计入档（2026-09-17 审核裁决；**2026-09-22 修订（D-W5-3）：通道机制改「反向常连」——exec 任务持集群 token 出站拨控制面，控制面按 task→NodeID 路由；原文字面「控制面经 `tasks.<name>` DNS 正向拨入」经实证宿主进程不可路由 overlay（E3-5 同源）不可实现，正向方案甩不掉 manager 常驻代理容器；安全面条款全部保留，详见 [web-terminal 专项 §2](2026-09-22-web-terminal.md)**） | Swarm 无 exec RPC，worker 容器终端在无远端 daemon 访问下不可达；Portainer Agent / Komodo Periphery 为同型先例；成员与分发仍归 Swarm，不违反 D12 | 通用 Docker API 代理（第二 docker.sock 面、安全事故面）；SSH 隧道（密钥分发 + NAT 脆弱，调研 §2 反模式）；per-container 终端 sidecar（侵入 compose 语义） |
| D20 | 基础 Go 框架 = **lynx + google/wire**（2026-09-17 技术选型）：`lynx.NewRunner` 承载进程生命周期，`boot.Bootstrap` + Wire 编译期装配依赖图；lynx 用法以 **messageloop**（github.com/messageloopio/messageloop，同域生产使用）为参考实现，Wire 装配形态以 lynx-clean-template 为模板；框架层只做装配与生命周期，领域代码不依赖框架类型（可替换性边界同 §2.8） | 统一生命周期（Drain/优雅关停语义现成，与排水和维护窗口契合）；`lynx.Service` 插件化天然承载端口-适配器；Wire 编译期 DI 无运行时反射、装配错误编译期暴露；轻量取向一致（非全家桶）；Apache-2.0 且上游同域可控 | 纯手工装配（messageloop 现状：装配逻辑淤积在 setup 函数，规模上升后不可读——fleetly 自第一天用 Wire）；fx/dig（运行时反射 DI，失败后移）；kratos/go-zero（全家桶过重，违背「基础设施只复用不自研」）；自研生命周期框架（重复造轮子） |
| D21 | API 定义 = **gRPC + grpc-gateway/v2，proto 为契约唯一真源**（2026-09-17 技术选型，**用户裁决：不引入 huma**）：buf 工具链生成 genproto 与 SDK（`fleetly.{client,console,server}.vN` 分模块，torchwood 同型）；REST/JSON + OpenAPI（openapiv2）由 gateway 派生；错误信封走自定义 ErrorResponse（`disable_default_errors`）+ snake_case JSON；鉴权/限流在 gRPC 拦截器链；SSE/长连接与 gateway mux 同进程共存（原生 handler，torchwood realtime 同型）——装配整体照抄 **torchwood**（github.com/torchwoodcloud/torchwood，同域生产使用） | 单一契约真源（proto）同时喂 CLI（SDK/gRPC）、Console（REST）、Agent/MCP（REST/gRPC）；`buf breaking` 即兼容门禁（对 oasdiff 类文本 diff 更强）；protovalidate 把校验写进 schema；与 D20 同栈（lynx 为壳）且参考项目代码可直接复制 | huma（**用户裁决否决**：多引入一层框架、偏离 lynx 生态参考栈）；oapi-codegen / spec 先行（spec 与代码双份维护）；纯 gRPC 无 gateway（Console/Agent 的 REST 生态面缺失）；手写 REST + 手维护 OpenAPI（漂移必然） |

### 3.1 底座再评估触发条件与退出预案

D12 的采纳带验证门（V1-V7，见 [Swarm 评估报告](../research/2026-09-17-swarm-substrate-assessment.md) 第 6 节）。以下任一条件出现时，切换到退出预案（k3s driver 或自研 node；方向已定，详细设计在触发条件出现时立项，触发前以 V1-V7 回归持续监测）：

| 触发条件 | 走向 |
|---|---|
| V1-V7 任一不通过且无缓解 | 启动退出预案：k3s driver（应用定义保持 Compose 兼容）或自研 node |
| Swarm 被官方废弃，或 Engine 破坏式升级无法用版本门禁缓解 | 同上 |
| 用户群真实要求跨节点共享存储 / 更强调度 | 评估 k3s driver（本地卷/CSI 是 Swarm 的硬边界） |
| 多节点规模远超设计（>10 台成为常态） | 重新评估底座选型 |
| 需要纳管用户已有的 k3s 集群 | 增加 k3s driver（参考 Dokku scheduler-k3s 先例） |

## 4. 分步实施计划

### 4.1 Spike 阶段（1-2 周，先验最高风险）

| Spike | 验证内容 | 通过标准 |
|---|---|---|
| A 构建 | BuildKit 容器 + Railpack 构建 Node/Go 应用；版本钉死与 plan JSON 归档；缓存三情形（本地层 / registry cache / secrets-hash 失效）；私有依赖 BuildKit secrets；rootless vs 特权选型与资源限额；**本地镜像 digest 引用免 registry（V2）** | 从源码到可运行镜像可复现（Railpack 钉版本 + `railpack-plan.json` 归档）；二次构建显著加速且 env 变化正确失效；私有依赖凭证不进入最终镜像；构建在 CPU/内存限额内且不污染宿主；rootless/特权方案明确选型；service 以 `app@sha256:` 创建时任务零 pull 尝试 |
| B 发布与路由 | Swarm 更新（start-first + healthcheck + `failure_action=pause`）+ 失败冻结与归位重放 + **stack 对账语义（服务增删、受管字段校验）** + Traefik 控制面下发（HTTP provider）+ **失败路径与配置隔离** + **连接池治理（V1/V3/V4）** + **归位零成本（B2）** | health 失败时新任务 FAILED、更新 paused、旧任务不中断（V1）；pause 后同内容 spec 重放**任务零替换**（B2）；stack apply 正确增删服务、`failure_action` 非 pause 显式报错（`E_COMPOSE_MANAGED_FIELD`）；更新窗口内持续探测零失败、VIP 更新前后不变（V3）；keep-alive 陈旧连接用 `serversTransport` 参数消除（V4）；**start-period 内任务是否已进 LB 端点集合（B3，最高优先级开放问题）**；「容器启动即崩」时旧版本持续服务且入口配置零污染；单应用坏配置不影响其他应用路由；失败矩阵（启动即崩/health 永不通过/拉取失败）逐条断言错误码；回滚（快照重放）一分钟内完成 |
| C 底座 | Swarm 初始化与 join、节点故障重调度、**卷与绑定语义（V6a）**、**绑定保持与基本漂移（V6b）**、**单 manager 故障恢复（V5/V5b）** | 单节点 `swarm init` 对既有容器无影响、用户视角透明；节点 DOWN（15s 量级）后 stateless 任务自动重建；有卷服务无约束时迁移得空卷（复现并文档化）、加绑定后任务钉住不迁移；绑定节点 down→blocked→恢复、drain→回岗、remove→人工重绑；`--force-new-cluster` 恢复演练成功、应用不中断；raft 回退后孤儿容器命运明确（V5b） |

A、B 通过则 v0.1 无未知数；C 通过则 v0.2 无悬念。V1-V7 为 Swarm 采纳门（详见 Swarm 评估报告第 6 节），任一不通过且无缓解则启动退出预案；全部失败才需要回到"包装 dokku"备选路线。交叉验证新增验证项（B2 归位零成本、B3 LB 端点时机、V5b/V6b）并入 B/C 的通过标准。

**Spike 结论（2026-09-17，A/B/C 全部完成）**：V1–V7 采纳门**全部通过**——V1（health 失败不切流，298 样本 0 失败）、V2（digest 引用零 pull，三层取证）、V3（更新窗口 395 样本 0 失败、VIP 不变）、V4（**条件成立**：治理主键改为应用侧优雅退出，serversTransport 降辅助——§2.5 已改写）、V5/V5b（停止态冷备可校验、恢复后 worker 任务 id 不变零中断；raft 回退孤儿命运已明）、V6a/V6b（空卷事故三段实证 + 钉住/drain/rm 全矩阵）。B3 头号开放问题关闭（带 healthcheck 端点晚于 healthy 45–87ms，失真=0，对外口径不降级）；B2 归位零成本成立。完整证据与 35 条意外发现见 `spike/a|b|c/README.md`；关键工程约束：构建本地镜像必须 `--provenance=false --sbom=false`（Spike A #9）、Traefik 裸 `{}` 会清空路由（Spike B）、Engine 29.8.1 的 docker events 无 task 事件——状态机观测走 service ps/API 轮询（Spike B #意外）。V1-V7 已转 nightly 永久回归（交付流水线 P3）。

并行非技术验证：Spike 期完成 5-10 个目标用户访谈，重点验证「声明式 / 漂移检测」与「AI Agent 直接操作平台」是否为其真实痛点；v0.1 发布后 4 周内设定外部试用与反馈目标（数量在 v0.1 启动时确定）。

### 4.2 v0.1（单机可用，8 项）

1. 部署闭环：Webhook（验签+拉源）/ API（CLI compose 上传）→ 构建 → 零停机上线 → 回滚（git push(SSH) 面已移除，ADR-0012）；支持 web + worker 双进程（`replicas` 字段 v0.1 即照用；cron 与多副本管理〔缩放 UI/指标水位/自动扩缩〕v0.2）；运行时 = 单节点 Swarm service（安装时隐式 `docker swarm init`）
2. gRPC + REST（gateway）API（OpenAPI 文档自动派生）+ CLI（全命令 `--json`）
3. 域名 + 自动 HTTPS（每节点 Traefik + 控制面集中 ACME；域名列表契约见 §2.4）
4. 环境变量/密钥（加密存储、注入、自动连接串）
5. 日志查看（实时 SSE 流 + 历史落盘检索）
6. 基础 Console 端（应用列表/详情、部署、日志、env、域名）
7. 发布语义全套：pause 固定、最近 5 版回滚（归一化 compose + 覆盖层）、观察窗默认告警、首发失败 scale 0 保留现场
8. Compose 子集校验与自动放置：白名单/受管字段校验、有卷应用自动钉住到本机（同一代码路径，见[放置专项](2026-09-17-stateful-placement.md)）

验收：一台干净 VPS 上执行一条安装命令，用已解析的域名，20 分钟内完成部署（API 上传或 webhook 投递）并拿到 HTTPS 访问（DNS 传播时间不计入），UI 可见日志与配置，可一键回滚；**信任闭环验收**：控制面状态完成一次备份 → 回读校验 → 按文档恢复演练（L1，首次配置预算 ≤10 分钟）。

横切硬指标（v0.1 即满足，评审门禁）：

- **平台自升级原子化（升级双轨口径，2026-09-17 审核裁决）**：**fleetlyd 升级 = 热备快照 + 预拉镜像 + 失败自动回退，不停 Engine、应用不停**；**主机/Engine 升级 = 冷备 + 维护窗口语义（§2.6；有状态应用停机如实告知）**——两类升级不得混淆，冷备不绑定 fleetlyd 升级；禁止 `--force-recreate` 式升级（Coolify/Dokploy 最高频信任事故模式）。
- **平台状态备份基线**：控制面状态（SQLite + 密钥）可备份、恢复步骤文档化且可人工执行（L1/L2 正式恢复演练属 v0.2）；备份密钥与元数据独立于备份数据保存；任何备份失败红色告警（禁止「绿色成功但实际没上传」）。
- **资源预算**：控制面 fleetlyd idle 内存 <200MB（对标 CapRover 实测值；含 dockerd + swarmkit，需自测校准；Traefik 单列约 50MB）；构建与应用资源隔离且有限额；v0.2 指标栈引入后另设平台组件总量预算（目标 idle <400MB，压测后定稿）。**实测校准（2026-09-19，[校准报告](../reports/2026-09-19-resource-calibration.md)）**：fleetlyd+dockerd(含 swarmkit) idle ≈160.7MB 达标；Traefik 实测 16-22MiB 远低于对标值；containerd 为独立进程（≈44-50MB）自本条起**单列**（并入 v0.2 平台组件总量预算口径，当前合计 ≈230MB 方向达标）；dockerd 内存随应用数线性增长（≈2.4MB/应用），v0.2 指标栈落地前按「110MB + 2.4MB×应用数」作水位提示。
- **引擎门禁**：Docker Engine ≥ 29.8.1、iptables 后端（nftables 暂不支持 Swarm 节点）；引擎升级前跑回归矩阵（见第 6 节）。
- **容量边界**：单节点建议 ≤50 apps / ≤200 域名 / 并发构建 2（压测后修正）；超限时显式提示而非静默降级。
- **安全默认基线**：数据库/内部服务默认不暴露公网；面板支持不裸奔运行（Tunnel/VPN/SSO 可选）；**后端强制鉴权**（Coolify CVE 群的共同模式：前端做了权限、后端没做）。
- **底座端口加固（2026-09-17 审核裁决）**：swarm 端口（2377/7946 TCP+UDP/4789 UDP）纳入安全默认——安装时 advertise-addr 优先选私网 IP（无则公网并在安装报告警示暴露面）；可选 `--harden-firewall` 自动加 iptables 规则（仅放行节点网段，不改写 ufw 以规避 docker-ufw 冲突）；v0.2 join 向导按指定 worker IP 生成精确放行规则。
- **状态诚实契约**：观测数据带 `observed_at/stale`；`nodes` 不提供「最后心跳」字段；备份含 manifest + sha256 回读校验（`state_backups` 台账）。

### 4.3 v0.2

**多节点（首项；2026-09-17 画像复核提序——目标画像的生产基线是 2 台，HA 与免停机维护依赖它，是 v0.1 用户投产后的第一个结构性缺口；zot 自动接管是「加第二台」一键化的前提；drain/remove 仍用 `docker node` + 文档/UI 指引，不建生命周期 API）**：`docker swarm join` + zot registry（平台自动部署 + `--with-registry-auth` 分发）+ placement 绑定 + 节点列表 + 卷位置前哨 + HA 边界口径落向导/UI；验收：2 节点拓扑上线、无状态节点 drain 新连接零失败（重试语义；在途连接可能中断一次，指引先摘 DNS）、有状态节点 drain→回岗自动回绑 → MCP server（薄适配层；**工具预算核算**：读/写操作（deployment/rollback/placement/nodes/volumes 等）须做 ≤30 预算核算，超出者并入 action 枚举或不暴露）→ S3（外部端点支持：端点配置/连通测试 + restic 备份目标 + 应用凭证注入）→ **数据库托管（Dokploy 对标项）**：托管数据服务机制（模板 + 卷钉住 + 备份/恢复适配器 + 连接串注入）；Postgres/Redis 首发，MySQL/MongoDB 模板紧随按需求排序（新增库 = 模板 + 备份适配器，各引擎备份/恢复是主要成本）→ **Cron/定时任务（Dokploy 对标缺口，排在数据库托管之后；细则见下）** → metrics（VictoriaMetrics + 图表 + 查询面）+ 通知（Webhook/Slack/Email）+ Web 终端（经执行中继 `fleetly-exec`，D19，覆盖 worker 节点容器；xterm.js UI）。

**Cron 细则（v0.2）**：
- **声明**：compose 服务 + label `fleetly.cron`（5 段表达式；可选 `fleetly.cron.timezone`，默认 UTC）；`replicas` 必须 0/省略；声明只在 compose（不建并行期望态）。
- **执行**：调度器按点创建一次性 Swarm job（`--mode replicated-job`，Engine ≥23 能力、门禁 29.8.1 覆盖），继承镜像/网络/secrets/placement/资源限额；有卷应用继承绑定节点；**触发前绑定节点前哨检查**——节点不 ready → 记 `skipped(node_unavailable)` + 事件、不创建 job（与控制面停机 skip 同型）；job 以 `--restart-condition=none` 创建（失败即 run 记 failed，不重试）；看门狗 `cronJobTimeout` 默认 10m（可选 label `fleetly.cron.timeout` 覆盖，如 `30m`），超时 → 删 job 服务 + `cron_runs` 记 timeout + 事件。
- **策略**：重叠 skip（max-concurrent 1）；控制面停机期间错过点 skip + 事件、不补跑；失败只记录 + 通知，不自动重试。
- **留存**：`cron_runs`（每 schedule 最近 20 条）+ 日志进现有采集；完成后删除 job 服务。
- **入口**：手动触发 API/CLI/UI（走同一路径，写审计）。
- **复用**：与平台热备、数据库备份共用同一调度核（由 v0.1 备份 ticker 演进）；调度核基于 lynx contrib/schedule（表达式 6 段含秒——**平台对外契约保持 5 段**，装配时秒位固定为 0，契约不随框架变）。（实现修正注记，2026-09-21，D-CR-1 已裁〔object-storage 设计 §8〕：lynx v1.11.0 无 contrib/schedule 包〔contrib 仅 zap，实测证伪〕，调度核改用 `robfig/cron/v3` + `cron.ParseStandard`——恰为五段标准式，无需「六段含秒钉零」转换，平台对外契约不变；tick 循环骨架仍沿用 v0.1 备份 ticker 模式。）
- **验收**：对齐 Dokploy 五项（表达式/时区/日志/手动触发/API）+ 本平台附加项（`replicas>0` 拒绝、不补跑、job 继承绑定）。
- **降级**：v0.2 预算破裂时第一个降级；用户逃生口 = 应用内 cron 容器或外部触发 API（文档写明）。

### 4.4 v0.3

PR 预览环境（AI Agent 开 PR → 自动 URL → 合并即销毁）、官方模板目录（先维护 10 个官方模板，`fleetly template add xxx`）、团队/RBAC/审计留存（商业版候选）、Compose 子集扩展（`depends_on`/外部网络/`configs` 按真实需求逐项开放）、Tunnel 接入（面板与应用不暴露公网，Cloudflare Tunnel 式）。

### 4.5 工作量估算（v0.1，AI 辅助开发）

约 31-42k LOC（含 UI），2-3 人 3-4 个月；单人全职 6-9 个月。多节点工作量因采纳 Swarm 显著下降（D12，分布式核心不再自研）；构建/发布与 UI 为最大变量。**估算基线说明**：本估算早于独立设计×交叉验证轮；三专项（发布/放置/状态模型）的 v0.1 实现切面须在 v0.1 启动前重算并冻结（契约冻结 ≠ v0.1 实现，超出切面的项按 §4.3 后置），避免范围蔓延（见 §5 风险表）。

## 5. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| 零停机状态机时序缺陷 | 发布中断、流量丢失 | Swarm 原生 health gate + `failure-action=pause`（源码级验证，V1 实测确认）；平台显式状态机 + 每步可观测 + 事件持久化；数据库迁移类变更明确文档化为"非零停机" |
| 构建确定性/缓存/私有依赖 | 用户构建失败 | BuildKit 独立容器 + cgroup 限额 + 构建队列；语言覆盖以 Railpack + Dockerfile 为界，不做全覆盖承诺 |
| 多节点镜像分发返工 | v0.2 改部署管线 | v0.1 起 digest 引用；zot 方案在 spike 阶段验证 |
| 底座依赖：Engine 破坏式升级（v29 类事件重演） | 集群不可用或功能断裂 | 锁版本 + hold + 升级回归矩阵（V7：服务名 DNS/ingress/secrets/卷/containerd 双模式）；`min-api-version` 兜底；版本门禁写入安装检查 |
| 有状态能力边界（local 卷不跟随、CSI 实验性） | 数据丢失风险 / 误迁移 | stateful 由平台绑定自动钉住并文档化；状态优先外置（外部 DB/S3）；CSI 不用于关键路径；V6a/V6b 演练验证 |
| keep-alive 陈旧连接 | 更新期间偶发 5xx | Traefik `serversTransport` 调小 idle 连接；应用侧 SIGTERM 后 `Connection: close`；发布观察窗；V4 实测复现与治理 |
| Swarm 单 manager SPOF | 管理面丢失（应用仍运行） | 冷备 `/var/lib/docker/swarm` + `--force-new-cluster` 演练（V5）；最小 label 集反向重建；Mirantis MKE 作为付费兜底选项 |
| 节点重调度语义误用 | 有卷服务迁移后空卷、误判 | stateful 由平台绑定自动钉住（见[放置专项](2026-09-17-stateful-placement.md)）；对齐 Swarm 心跳（15s 量级）；文档与 UI 明示「自动迁移仅限无状态」 |
| 2 台 HA 预期落差（有状态/管理面单点被当产品缺陷） | 信任受损 | §2.6 HA 边界口径入初始化向导/UI/文档；v0.2 验收含 2 节点演练 |
| AI Agent 权限滥用 | 安全事故，信任崩塌 | scope token（read/deploy/admin）+ plan/apply + 审计 + 破坏性操作显式确认 |
| 控制面单点丢失 | 无法管理（应用仍运行） | 备份等序与恢复阶梯 L1/L2（L3 runbook 化，见[状态模型专项](2026-09-17-state-model.md)）+ 最小 label 集 + DR runbook；应用运行不依赖控制面（既有架构性质） |
| 差异化需求未验证 | 做出来没人要 | 需求侧验证并行（4.1）；v0.1 后设反馈门禁；保留把资源转向已验证痛点的选项 |
| 平台自升级破坏（第一信任杀手） | 用户应用/数据不可控，信任崩塌 | 预拉镜像 + 升级前自动快照 + 失败自动回退；SQLite 迁移事务化 + 容器 label 为实际状态 + 对账自愈；发布前跑「上一版本 → 新版本」升级 E2E |
| 备份假成功 | 灾难日发现无可用备份 | 备份产物写入对象存储后回读校验；失败红色告警；密钥随备份独立保存；定期恢复演练（见 4.2 横切硬指标） |
| 错误信息黑盒 | 用户与 AI Agent 都无法排障；差评最密集处 | 结构化错误码 + 原始 stderr/上下文 + 修复建议 + 事件流可查（设计原则 5） |
| 构建与应用资源竞争 | 单机雪崩 | 构建并发队列 + 资源限额 + UI 资源水位展示 |
| S3 依赖外部端点（打包延后） | 备份目标需用户提供或本地 | 端点可配置（restic 目标）+ 本地路径兜底；打包 S3 延后到需求证据，引入时再评估 RustFS 单厂商风险；备份密钥与元数据独立保存 |
| 范围蔓延 | 工期失控 | 每期范围冻结；第 7 节"明确不做"清单为准 |
| 商业化与社区信任冲突 | 开源社区反弹 | 核心完整可自用；商业边界（团队/SSO/审计留存/HA/托管）提前定义并公开；不做功能回撤 |
| LB 端点可能早于 health 加入 | ~~start-first 重发布期「失败=不切流量」失真~~ **已于 2026-09-17 Spike B3 实测关闭**：带 healthcheck 时端点入集晚于 healthy 45–87ms、542 样本 0 失真——对外口径**不降级**；health_gate=none 的真实暴露（27 连败/6s 预热）归入降级表「无 healthcheck」行 | 见 spike/b/README.md §3；架构 §2.5 降级表同步更新 |
| 归位零成本依赖 task spec 深度相等 | ~~未验证~~ **已于 2026-09-17 Spike B2 实测关闭**：旧 task id 跨「失败 + 同内容重放」不变、零新增任务；字段脏检矩阵——container-label/env/restart-policy/`--force` 触发重建，service-label/update-config 不触发 | 平台纪律：归位重放**禁用 `--force`**；见 spike/b/README.md §2 |
| 冻结→归位竞态（窗口内旧任务节点 DOWN） | 按失败 spec 重建错误版本任务 | 归位 p95 <2s + 故障注入测试；错误任务不会通过 health 接管流量 |
| 缓存陈旧被误当事实 | 用户/AI Agent 误判 | 观测数据带 `observed_at/stale`；决策路径禁止读缓存（写前直读） |
| 恢复后孤儿误删 | 数据丢失 | 孤儿只登记不自动删；恢复后只读观察、人工处理差异 |
| env 明文随 raft 备份 | 「密钥独立于备份」边界被击穿 | 如实文档化 + 备份介质加密 + 密钥独立保存；v0.2 评估 secrets/tmpfs 注入 |
| 冷备维护窗口实际不执行 | 灾备假可用 | 升级前强制冷备（天然有窗口）；冷备占比监控与失败告警 |
| 系统性故障（registry/节点/构建器） | 多 app 同时失败叠加处理 | 部署失败率异常事件告警，人工判断；不做熔断（Dokploy 亦无此机制） |
| Compose 子集外的构造被拒绝（depends_on/extends/profiles 等） | 迁移摩擦与预期落差 | 拒绝时给替代建议与文档链接；真实需求驱动 v0.3 逐项开放；用户访谈验证 |
| stack apply 非事务（多服务部分失败） | 跨服务发布原子性缺失 | 观察窗按整体判定 + 失败归位重放整栈 revision；文档明示「按服务滚动」语义 |
| label 约定与 compose 生态习惯差异 | 用户误写 Traefik label 期望生效 | `fleetly.*` 为唯一一等约定；Traefik label 直写不保证（文档明示）；对账器忽略非 `fleetly.*` label |
| 基础框架依赖（lynx 小生态、pre-2.0 版本节奏，D20） | 框架破坏性升级波及控制面 | 锁版本（v1.11.x）+ 升级走依赖门禁附 changelog 评审；Wire 生成物 `go generate` 差异进 PR 门禁；上游同域可控，必要时可 fork 接管维护；框架只做装配与生命周期，领域代码零框架类型依赖（替换面收敛在 boot 层） |

## 6. 测试策略

（落地轨道与门禁流程见[交付流水线设计](2026-09-17-delivery-pipeline.md)；V1-V7 及扩展项已升级为永久回归测试，不再是一次性 Spike。）

- **单元**：发布状态机（穷举转换与失败分支）、compose 解析（子集校验/归一化）、放置解析与选点确定性、漂移 hash、加密。
- **集成/E2E**：真实 Docker/Swarm 环境跑 build → service 更新 → health gate → 路由 → rollback 全链路；每个 v0.1 验收项至少一条 E2E；必须覆盖失败矩阵（health 永不通过、容器启动即崩、拉取失败、观察窗崩溃循环、坏配置隔离、控制面中断恢复）与 V1/V3/V4/V6a 场景。
- **放置与状态**：绑定保持与基本漂移（down→blocked→recover / drain→回岗 / remove→人工重绑 / 数据不匹配 409）、写前直读、孤儿保护（只登记不删除）、备份等序与恢复顺序演练（L1/L2）、导出 tar 一致性（密钥不随包）。
- **契约**：proto 为 API 契约真源（buf lint + buf breaking 门禁；genproto/SDK 生成物编译即校验，生成物同步进 PR 门禁）；Compose 子集校验（白名单/拒绝清单/受管字段/label 约定）进 CI；错误码注册表只增校验、变更需显式评审。
- **引擎升级回归（V7）**：dind 矩阵（containerd 存储 + overlay2 两条腿，锁定版本号）跑服务名 DNS、ingress、secrets 挂载、卷语义子集；引擎版本升级前必跑。
- **适配器一致性（conformance）**：Builder 套件 v0.1 起常跑（双实现）；ObjectStore 套件随 S3 端点落地；其余端口（Proxy / Runtime 等）套件随退出预案触发补齐（§2.8）；新增或替换适配器必须跑通套件后才可合并。
- **升级**：上一版本数据 → 新版本迁移的前后对比测试；平台自升级的失败回退路径纳入每次发布的必测项。
- **恢复演练**：定期从备份恢复控制面与应用数据并验证（v0.2 起纳入发布检查单）。
- **AI Agent 面向**：MCP 工具 schema 快照测试（防上下文膨胀）、错误码稳定性测试、`--json` 输出 schema 测试。
- **手工验收**：干净 VPS 一键安装 + 示例应用部署（每次发布前执行）。

## 7. 明确不做的事

- 不做 k8s / Nomad 后端（底座为 Docker Swarm，见 D2/D12；k3s 保留为可选 driver，触发条件见 3.1）
- 不做内置 CI 引擎（测试/lint 归 Git 托管方；平台只做 webhook 状态门禁集成）
- v0.1 不做服务模板商店（Coolify 式 280+ 模板）
- v0.1 不做数据库托管（Postgres/Redis 托管自 v0.2 起，见 §4.3）
- 不做跨节点共享存储（卷本地；CSI cluster volumes 实验性，不采用；**S3/FUSE/CSI-S3 作为数据卷同样不做**，理由见[放置专项](2026-09-17-stateful-placement.md) §7）
- 不自研调度器 / 成员管理 / 节点协议（由 Swarm 承担；这是本方案的核心取舍）
- 不承诺构建语言全覆盖（以 Railpack + Dockerfile 能力为界）
- 不做多租户强隔离（预留为商业版）
- 默认发行包不引入 AGPL 组件
- 不做 stateful 自动迁移 / 自动换点 / 自动 failback；跨节点卷移动只经备份恢复（不做在线迁移）
- 不做 stateful 应用的零停机承诺（持久卷/固定端口场景显式降级 stop-first 并告知）
- 不做 MCP 全量自动生成工具面（精选 ≤30）
- 不做功能回撤：核心功能一旦发布不因商业版回收，许可证与功能承诺不可撤回（Dokploy DSAL / MinIO 教训）
- 不做遥测：默认不采集/不上报任何使用数据；版本更新检查为显式可选
- 不做通用插件系统：不开放动态加载、不做通用插件 API、不开放对账器/状态机扩展点（第三方集成走稳定事件流/Webhook；端口适配器经上游代码贡献，见 D13/§2.8）
- 不做自研应用描述规范：应用模型 = Compose 规范（见 D14）；v0.1 不支持白名单外的 compose 构造（`depends_on`/`extends`/`include`/`profiles`/`configs`/外部网络，显式拒绝不静默）
- 不使用 Swarm 原生回滚 / previous_spec；窗口后不自动回滚（观察窗默认告警、自动回滚 per-app opt-in）
- 不做标签/偏好式调度 DSL（placement 只做节点级硬钉住）；不做 rebalance 机制（节点排布调整用 `docker node` 原生命令）
- 不做对未登记对象的自动删除（孤儿只登记，处理走人工；不建 adopt/purge API）
- 不承诺节点心跳时间戳；不把运行态快照当权威
- 不承诺导出免重建；不承诺单节点整机丢失的应用与数据恢复
- 不做系统级熔断与崩溃计数升级（窗后异常只告警）
- 不做调度约束预检（等待 Swarm PENDING + 看门狗超时；只检查平台自己的绑定节点状态）
- 不做节点 adopt / 身份自动消解 / 卷内 marker / drain-remove 平台 API（节点变更用 `docker node`，重绑走一次性 CLI）
- 不做通用 docker.sock 远端作业（卷/镜像/节点操作仍走人工）；唯一例外 = 执行中继（仅 exec、仅平台标记容器，D19）；远端卷删除/校验由用户在节点上执行
- 不做 guarantees 机读块与新鲜度协商（warnings + observed_at/stale 已覆盖）
- 不做导出格式合同与自动导入（一键 tar 导出 + 文档化恢复流程）
- 不做宿主脚本类定时任务目标；不做秒级调度、任务依赖链、失败自动重试、错过点补跑（cron 细则见 §4.3）
- 不建与 compose 并行的 schedule 期望态（定时任务声明以 compose 为唯一真源）
