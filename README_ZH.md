# fleetly

[English](README.md) | [简体中文](README_ZH.md)

> Dokku 的资源占用，Railway 的 API，AI Agent 优先的操作方式。

fleetly 是面向小团队的极轻量级开源 PaaS：把 `compose.yaml` 应用部署到 1~10 台服务器的集群上，获得零停机发布、版本回滚、漂移检测，以及一套同时为人类与 AI Agent 设计的 API 面——不需要 Kubernetes。

**当前状态：v0.3 已发布（v0.1.0 / v0.2.0 / v0.3.0）。** 设计已定稿并通过评审；每一波实现均带 dind E2E 与真实 VPS 演练记录。单机形态（v0.1）、生产基线（v0.2：多节点、托管数据库、统一日志检索、通知、Web 终端、控制面 TLS）与团队版（v0.3：用户账号、团队与项目隔离、角色授权、审计追踪、MySQL/MongoDB 模板、Slack/Email 通道、自动扩缩、vmalert 告警、DNS-01 通配证书）均已交付——见[路线图](#路线图)。曾用名 *edgesets* 与 *edgefleet*。

## 安装

干净 Linux VPS（amd64/arm64，root）上一条命令装出可运行平台——引擎门禁（Docker ≥ 29.8.1 + iptables 后端）、隐式 `docker swarm init`、systemd 开机自启，安装报告含端口暴露面提示：

```sh
curl -fsSL https://fleetly.dev/install.sh | sudo sh -            # 最新 stable
curl -fsSL https://fleetly.dev/install.sh | sudo sh - --version v0.3.0
sudo sh install.sh --bin-dir ./dist                              # 离线 / 开发形态
```

首启开放自助注册直到首个用户注册（该用户即平台管理员，并自动创建个人团队与默认项目）；写入 `<数据根>/bootstrap-token` 的初始 admin token 在此刻自动吊销——装机后请尽快注册、再暴露端口。卸载默认保留应用数据（`--purge` 才删）。控制面升级一条命令、自带升级前快照与失败自动回退（`sudo sh upgrade.sh --version vX.Y.Z`）；Engine/主机升级是另一条冷备轨——见 [`docs/runbooks/upgrade.md`](docs/runbooks/upgrade.md)。三形态、门禁清单、端口面表与 dind 验收见 [`deploy/README.md`](deploy/README.md)。（release 制品链随发布流水线落地；在那之前离线 `--bin-dir` 形态是可用路径。）

## 为什么是 fleetly

- **为没有运维的团队而建。** ≤5 名开发、无专职运维、1~3 台服务器起步、每周 1~2 小时的维护预算。一切可自动化的（证书、备份、升级、巡检）都自动化且可验证。
- **Compose 是唯一应用模型。** 没有私有 spec。受控的 Compose 规范子集 + 最小 `fleetly.*` label 约定；子集之外一律结构化报错拒绝，绝不静默忽略。
- **Docker Swarm 作底座。** 成员管理、调度、健康门更新由引擎内置——不自研分布式核心。v0.1 单节点本身就是（对用户透明的）单节点 Swarm，加第二台是 `docker swarm join`，不是重构。
- **API 优先，proto 即契约。** gRPC + REST（grpc-gateway）由同一份 protobuf 派生；CLI、Console 与未来的 Agent 集成都是同一契约的消费者。没有 API 的功能不准进产品。
- **信任是地板。** 原子化自升级（预拉镜像 + 快照 + 失败自动回退）、带回读校验的备份、错误信息即产品（稳定错误码 + 上下文 + 修复建议）——同时服务人类与 AI Agent。

## 能力地图（规划）

| 领域 | 行为 | 版本 |
|---|---|---|
| 部署 | webhook（GitHub/Gitea）/ API → Railpack 或 Dockerfile 构建 → 零停机切流 → 观察窗 | v0.1 |
| 发布安全 | Swarm `failure-action=pause` + 平台版本重放（保留最近 5 个已验证版本）；不用 Swarm 原生回滚 | v0.1 |
| 路由 / TLS | 每节点 Traefik，路由与证书由控制面下发；集中 ACME（HTTP-01）、多 SAN 域名列表 | v0.1 |
| 状态 | SQLite 控制面状态，三层模型（权威 / 观测缓存 / 实时直读） | v0.1 |
| 漂移检测 | 期望态 hash 对现实；检测默认开、自动收敛 per-app opt-in | v0.1 |
| 多节点 | `docker swarm join`、镜像仓库（zot）、有状态钉住、诚实的 HA 边界 | v0.2 |
| 数据服务 | 托管 Postgres/Redis 模板 + 备份/恢复/升级 + 连接串注入 + 平台密钥库 | v0.2 |
| 可观测 | VictoriaLogs 默认捆绑 + 统一日志检索（运行/构建/访问日志同库）+ opt-in 指标（VictoriaMetrics/cAdvisor/node_exporter） | v0.2 |
| 通知 | Webhook 端点 + 事件模式订阅 + HMAC 签名投递 + 重试台账 | v0.2 |
| Web 终端 | 执行中继（`fleetly-exec` 反向常连）+ shell 白名单 + 会话限额 + terminal scope + 审计 | v0.2 |
| 控制面 TLS | off / 平台证书 / 手工三态，双面（gRPC + HTTP）同证书，CLI/SDK TLS | v0.2 |
| S3 备份 | 外部端点或 opt-in 托管 RustFS（同节点 = 便捷层，非灾备） | v0.2 |
| Cron | Swarm job 形态 cron + 台账 + 看门狗 | v0.2 |
| 团队与项目 | 用户账号（首用户=平台管理员）、团队邀请链接、项目级 app/库隔离、四档角色（viewer/developer/admin/owner + 项目内覆写）、CI 机具令牌 | v0.3 |
| 审计 | 可检索审计追踪（操作者/动作/对象/结果/diff）、留存可调、Console 浏览 + CLI 导出 CSV | v0.3 |
| 数据服务 | 托管 Postgres/Redis/MySQL/MongoDB 模板 + 备份/恢复/升级 + 连接串注入 + 平台密钥库 | v0.2–v0.3 |
| 通知 | Webhook 端点 + 事件模式订阅 + HMAC 签名投递 + 重试台账；Slack 与 Email（SMTP）通道 | v0.2–v0.3 |
| 自动扩缩 | CPU/内存水位策略（冷却窗）；平台持有的副本覆盖层，与漂移对账不打架 | v0.3 |
| 告警 | vmalert（随 metrics opt-in）+ 规则 API + 渲染 Prometheus 规则文件 + 平台内建 Alertmanager 兼容接收器路由到通知通道（firing+resolved） | v0.3 |
| 通配 TLS | 可选平台通配证书（`*.base_domain`）走 DNS-01（DNSPod/Cloudflare），覆盖全部应用域名 | v0.3 |
| AI Agent | MCP server，精选工具面（≤30 工具）、scope token、破坏性操作两段式确认 | 暂缓 |

## 诚实的边界

我们明确说清楚不做什么：不做跨节点共享存储（卷本地；有状态服务钉住节点、永不自动迁移——数据移动只走备份恢复）；**2 台 ≠ 全面 HA**（你得到的是无状态进程级 HA，不是管理面或有状态 HA——安装器会明说）；不做 CI 引擎（测试归 Git 托管方，fleetly 以 webhook 状态做发布门禁）；不做 Kubernetes 后端（k3s 是退出预案，不是功能）。

## 架构

```
CLI (fleetly) / Console / gRPC / REST / Webhook
                 │
   fleetlyd —— 运行于 Swarm manager 的 Go 单二进制
     API：gRPC + grpc-gateway（proto = 唯一契约真源）
     发布状态机 · 对账器 · 构建管线（Railpack/BuildKit）
     状态：SQLite (WAL) · 密钥：envelope 加密（age）· TLS：集中 ACME
                 │  Docker API（本地 socket 管理全集群）
   Docker Engine（Swarm mode）—— 服务 · overlay 网络 · 调度
   Traefik（global，每节点）—— 路由与证书由控制面下发
```

基础栈：[lynx](https://github.com/lynx-go/lynx) + [google/wire](https://github.com/google/wire)（D20），buf + [grpc-gateway](https://github.com/grpc-ecosystem/grpc-gateway/v2)（D21）。领域代码零框架类型依赖——核心/适配器边界是硬纪律（D13）。

## 仓库结构

```
cmd/fleetlyd/   控制面守护进程
cmd/fleetly/    CLI
proto/            API 契约（fleetly.{server,client,console,shared}.v1）
genproto/         生成代码 + OpenAPI（openapiv2）——已提交
sdk/go/           Go SDK（gRPC client）
internal/         errcode / eventcode 注册表、应用错误信封
e2e/              dind 冒烟骨架（CI 与 Spike 复用）
docs/             设计文档、调研报告、实施规划
console/          Console 前端（React + Vite + shadcn/ui，随 T2.21 落地）
deploy/           安装器与 systemd unit（随 T2.1 落地）
```

## CLI

CLI 只经 gRPC（SDK）与守护进程通信——没有任何直开数据库或直连 Docker 的路径。所有触达平台的动词都带 `--addr`（默认 `127.0.0.1:8421`，env `FLEETLY_ADDR`）、`--token`（env `FLEETLY_TOKEN`）与 team/project 上下文 flag `--team`/`--project`（env `FLEETLY_TEAM`/`FLEETLY_PROJECT`）；token 与上下文的读取序为 flag > env > 本地配置 `~/.fleetly/config.yaml`。`fleetly auth login` 验证粘贴的 PAT（经 `Me`）后连同当前 team/project 上下文落盘该文件；`fleetly auth status` 展示身份与上下文，`fleetly auth logout` 只清本地副本（服务端吊销仍走 `fleetly tokens revoke`）。bootstrap admin token 在首启时**一次性写入** `<数据根>/bootstrap-token` 文件（不进日志；首登后删除），后续 token 由 `fleetly tokens create` 签发。全部动词支持 `--json`；退出码 `0` 成功/无变化、`1` 错误、`2` 有变化（仅 `plan`/`diff`）、`64` 用法错误（未知动词/flag 或参数违规，EX_USAGE 惯例）。flags 需置于位置参数之前（Go std `flag` 语义）。一元 RPC 带缺省 30s deadline；流式动词（`logs follow`、`events watch`）与等待动词（`deploy`、`build`、`rollback`）上 Ctrl-C 干净退出（退出码 0）。

```bash
fleetlyd &                                  # 控制面（gRPC :8421，HTTP :8420）
export FLEETLY_ADDR=127.0.0.1:8421

fleetly auth login                          # 粘贴一次 PAT；落盘 ~/.fleetly/config.yaml
fleetly auth status                         # 身份（Me）、团队×角色、team/project 上下文

fleetly validate compose.yaml               # 受控子集校验（本地）
fleetly plan compose.yaml                   # 经 API 与最近版本快照比对；退出 2 = 有变化
fleetly deploy compose.yaml                 # 入队并等待终态
fleetly apps list && fleetly deployments list my-api
fleetly logs follow --service web my-api    # 实时流（--json 为 JSONL）
fleetly env set my-api KEY value            # 随下次部署生效
fleetly rollback my-api                     # 版本重放（最近 5 版）
fleetly drift show my-api                   # 期望态 vs 实况
fleetly tokens create --scopes deploy --note CI   # 明文仅此一次显示
```

### 通过 Webhook（GitHub / Gitea）部署

先配置 per-app 签名密钥（设置后不再回显），再在 Git 托管方把 webhook 指向控制面（`POST /v1/apps/<app>/webhooks/github` 或 `/gitea`）。守护进程强制校验 HMAC-SHA256 签名、按 delivery ID 防重放（15 分钟窗口）、按 commit 幂等去重，随后拉源并入队部署。

```bash
fleetly apps webhook set-secret my-api <secret>            # ≥16 字符；admin scope
fleetly apps webhook set-source --branch main --auth-kind none \
    my-api https://github.com/acme/web.git                  # 或 https_token / ssh_key
fleetly apps webhook show my-api                           # 无敏感投影
```

完整 flag 列表见 `fleetly help <动词>`。

### Console 端（Web UI）

React SPA（Vite + Tailwind + shadcn/ui），只经带鉴权的 REST API 消费平台。构建后把产物目录配给 daemon 即可在 `/ui/` 前缀访问（静态资源不要求 token；数据仍全部走 Bearer 鉴权的 `/v1`）：

```bash
cd console && pnpm install && pnpm build      # → console/dist
fleetlyd -c config.yaml                       # 配置 console.static_dir: "./console/dist"
# 打开 http://127.0.0.1:8420/ui/  → 粘贴 API token 登录
```

覆盖：应用列表/详情（派生状态徽章）、部署（跟踪到终态）与回滚、实时日志（NDJSON 跟随 + 历史检索）、env 管理（pending 变更独立分组「待下次部署生效」）、域名管理与 verify、系统健康、平台事件流。详见 [console/README.md](console/README.md)。

### 对象存储备份（S3）

控制面状态备份可上传到 S3 兼容端点（恢复流程见[备份恢复 runbook](docs/runbooks/backup-restore.md)）。设置为运行期配置（无需重启）；保存即全量替换（PUT 语义——请求即新配置整体），secret 只写：读面只见指纹，永不回明文。

```bash
fleetly s3 set --mode external --endpoint-url https://s3.example.test \
    --bucket fleetly-backups --region us-east-1 \
    --access-key-id AKIDEXAMPLE --secret-access-key <secret> --path-style
fleetly s3 test        # 真实探针：put → get → delete，分步 ok/耗时（候选配置可先测后存）
fleetly s3 show        # 脱敏投影
fleetly s3 status      # 模式、端点、托管服务部署态
```

`--mode rustfs` 启用平台托管 RustFS（内部网络单桶；可经 `--public-exposed` 开公网子域 `s3.<base_domain>`）。诚实口径常驻：本机 RustFS 是**便捷层**（防误删/防单文件损坏），**不是灾备**——主机整体损毁时这些备份随主机一同丢失。

应用按服务粒度以 `fleetly.s3` label 选择接入；下次部署时平台注入 S3 system env（`S3_ENDPOINT`、`S3_BUCKET`、`S3_ACCESS_KEY_ID`、`S3_SECRET_ACCESS_KEY`、`S3_PATH_STYLE`），rustfs 模式同时挂接内部网络：

```yaml
services:
  worker:
    image: ghcr.io/acme/worker:1
    labels:
      fleetly.s3: "true"   # s3.mode=unset 时部署被诚实拒绝（E_S3_NOT_CONFIGURED）
```

### 定时任务（cron）

以 `fleetly.cron` label 家族声明定时服务。cron 服务是一次性 job，不是长驻服务——不得设置 `deploy.replicas`，不计入应用 running 态，Console 中如实标注 `scheduled`：

```yaml
services:
  cleanup:
    image: ghcr.io/acme/cleanup:1
    labels:
      fleetly.cron: "*/5 * * * *"              # 恰为五段标准 crontab 式
      fleetly.cron.timezone: "Asia/Shanghai"   # 可选；缺省 UTC
      fleetly.cron.timeout: "30m"              # 可选；看门狗预算，缺省 10m
```

```bash
fleetly cron trigger my-api cleanup   # 手动触发——与到点触发同链路；写审计；重叠/节点不可用 → skipped 并带原因
fleetly cron runs my-api              # 运行台账：status / scheduled / started / finished / skip_reason / error
```

## 文档

全部文档在 [`docs/`](docs/README.md)（中文，设计先行的工作流）：

- [架构设计](docs/design/2026-09-17-architecture.md)——定位、技术栈、21 项关键决策（D1–D21）、路线图
- 专项设计：[发布语义](docs/design/2026-09-17-release-semantics.md) · [stateful 放置](docs/design/2026-09-17-stateful-placement.md) · [控制面状态模型](docs/design/2026-09-17-state-model.md) · [交付流水线](docs/design/2026-09-17-delivery-pipeline.md)
- 调研：[竞品全景](docs/research/2026-09-17-competitive-landscape.md) · [Swarm 底座评估](docs/research/2026-09-17-swarm-substrate-assessment.md)
- 实施：[任务分解](docs/plan/2026-09-17-task-breakdown.md) · [v0.1 切面冻结清单](docs/plan/2026-09-17-v0.1-scope-freeze.md)

## 路线图

| 阶段 | 范围 | 状态 |
|---|---|---|
| T0 地基 | 仓库、CI 门禁、proto 契约链、错误/事件注册表、dind E2E 骨架 | ✅ 完成 |
| Spike A/B/C | 构建、发布与路由、底座风险验证（V1–V7） | ✅ 完成 |
| v0.1 | 单节点 8 项范围 GA（部署闭环、TLS、回滚、信任闭环演练） | ✅ 完成（v0.1.0） |
| v0.2 | 多节点、托管数据库（Postgres/Redis）、统一日志检索、通知、cron、Web 终端、控制面 TLS、指标（opt-in） | ✅ 完成（v0.2.0） |
| v0.3 | 团队与项目（用户/角色/审计）、MySQL/MongoDB 模板、Slack/Email 通道、自动扩缩、vmalert 告警、DNS-01 通配证书 | ✅ 完成（v0.3.0） |
| v0.4+ | MCP 重启评估、OpenObserve 观测高级层（带重评门槛）、生产深化后续 | 规划中 |

**商业线（刻意保持简单）：** 自托管核心全功能且永远如此——不做功能阉割。付费面是托管云与企业件（SSO/LDAP、审计外发 SIEM、合规报告、优先支持）。

## 开发

前置：[mise](https://mise.jdx.dev/)——`mise install` 按 `mise.toml` 钉版装齐 Go、Node、pnpm、buf 与 golangci-lint（版本与 CI 门禁对齐）；Docker 仅 e2e（dind）与本地完整闭环需要。不 mise 的环境按原前置自行安装（Go ≥ 1.26.6，`GOTOOLCHAIN=auto` 可用、buf CLI）。

```bash
go build ./...
go test ./... ./sdk/go/... -race   # mise run test
buf lint && buf generate          # 生成物已提交，不得漂移；mise run generate:proto
golangci-lint run                 # mise run lint
```

冒烟 E2E（在 `docker:29.8.1-dind` 内运行 fleetlyd）：见 [`e2e/README.md`](e2e/README.md)。

贡献纪律：本项目设计先行——行为变更先落文档（走评审轮），再按任务分解的垂直切片落地。错误码与事件是只增注册表。整改的机制验收必须包含关联文档/注释回写核对：对改动关键词在 `docs/`、`deploy/` 与代码注释里做 grep，确认 runbook、脚本与帮助文案不再描述修复前的行为（漂移即缺陷，不是风格问题）。

## 许可证

Apache-2.0——见 [LICENSE](LICENSE)。默认发行包不含 AGPL/DSAL 组件。
