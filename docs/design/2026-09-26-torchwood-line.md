# T 线：torchwood/messageloop 迁移与动态工作负载（v0.3 最高优先级线）

| 状态 | 日期 | 关联 |
|---|---|---|
| **草案（裁决轮进行中——DT/OT 编号待用户确认后冻结）** | 2026-09-26 | [实施方案与票据](../plan/2026-09-26-torchwood-line-impl.md)、[分发 prompt](../plan/2026-09-26-torchwood-line-prompts.md)；[v0.3 规划](../plan/2026-09-23-v0.3-plan.md)；[RBAC 设计](2026-09-23-rbac-teams.md)（OT-1 推翻对象）；[架构 §2.4](2026-09-17-architecture.md)（OT-2 推翻对象）；[数据库托管](2026-09-20-managed-databases.md)（V2-5 先例）；[观测设计](2026-09-22-observability.md)、[W5 B 线](2026-09-25-b-line-w5.md)（合流项） |

## 0. 裁决记录（用户直裁，2026-09-26）

| # | 议题 | 结论 |
|---|---|---|
| T-0 | 线路优先级 | **torchwood MessageLoop 部署是 fleetly 立项的原因，替代 dokploy 必须最高优先级支持**；本线为 v0.3 最高优先级输入 |
| T-1 | 兼容约束 | 自有项目 + 内测阶段，**不需要向后兼容**——过渡兼容层不做，直奔终态 |
| T-2 | 设计立场 | 从总体最优设计出发，**有足够硬的理由可推翻既有设计与约定**（本档 OT 编号即推翻项） |
| T-3 | 双部署形态（用户直裁 2026-09-28） | **torchwood/messageloop 必须继续支持原有 dokploy / docker 直接部署**（docker.sock 执行底座），与 fleetly 形态并存、按配置选择——部分推翻 T-1「无兼容」在部署维度的适用：T2-3 的「零 docker client」承诺改为「fleetly 驱动路径零 docker client，docker 驱动为受支持的一等底座」。触发证据：T2-3 重写了 dokploy 栈（必填 FLEETLY_* 插值 + 二进制仅 fleetly 后端）→ 现役 dokploy 部署报错。承接票 = IMPL-T2-5 |

## 1. 需求还原（证据基础）

### 1.1 messageloop 栈（现役 dokploy 部署）

仓库 `D:/Codes/qiulin/messageloop`，`docker/dokploy/docker-compose.yml`：单栈三服务（redis AOF + messageloop + mlbridge），GHCR 镜像只拉不编（`pull_policy: always`），Traefik label 声明**单服务三域名三端口**（WS 9080 / 客户端 gRPC 9090 h2c / Server API gRPC 9091 h2c）+ letsencrypt，配置文件 bind mount（`mlbridge.yaml`），宿主回环端口（9090/9091，SSH 隧道兜底），mlbridge 经 `dokploy-network` 内网直连同机 `torchwood-server:9080`。

### 1.2 torchwood 栈与 dispatcher（动态部署能力的真实形态）

仓库 `D:/Codes/qiulin/torchwood`，`docker/dokploy/docker-compose.yml`：七常驻服务（postgres/redis/minio/server/worker/dispatcher/packer）+ 三个一次性作业（migrate/db-grants/roles-sig），GHCR 拉取，三服务挂 `config.yaml`（:255/:289/:347），migrate 挂 `../../db/migrations`。

dispatcher **不是 per-request serverless，是常驻实例池**（`dispatcher/pool.go`）：

- 每部署构建镜像：zip → 宿主 docker sock `ImageBuild`，代码 COPY 进平台基座（node/go），`func-<fn>-<deploy>`；
- spawn 常驻容器（runner 监听 18080），Redis 租约认领复用，idle TTL 300s 回收、`TW_MAX_REQUESTS` 自退、reaper 收敛、`min_instances` 保温；
- 三层并发上限：每函数 4 / 每节点 16 / 全局 Redis 容量键；
- 网络：每项目 bridge `tw-func-<project>`，不可信函数挂 internal 无出网变体；dispatcher/server 动态 attach 进函数网回访；
- 加固：CapDrop ALL / 只读 rootfs / 非 root / pids 512；
- 多节点「细胞模型」：N 节点 × 本机 daemon + Redis 控制面 + registry 模式推镜像。

**sock 之痛自证**：dispatcher 须 `user: root`（compose :302-312 两处「等同宿主 root」警告）；执行硬绑定本机 daemon（`daemon.go:226-247`）；`tw-func-*` 网跨重建残留致函数队列卡死事故（2026-09-14，`daemon.go:296-305`）；node_id 随 hostname 漂移（`nodes.go:79-108`）；构建凭证复用宿主 `docker login`（`daemon.go:589-591`）。

**结论：fleetly 要建的是「程序化工作负载面」，不是函数运行时。** 池逻辑（租约/保温/熔断）留 torchwood，swarm 管调度，fleetly 管 API/网络/配额/审计/回收。

## 2. 推翻项（OT——有硬理由的既有设计变更）

### OT-1 恢复 per-project overlay（推翻 [RBAC 设计](2026-09-23-rbac-teams.md)「不做 per-project overlay」）

原裁决：隔离 = app 级专属网络，app 名全局唯一。**新硬理由（2026-09-23 时点不存在）：**

1. mlbridge→torchwood-server 内网直连是现役部署的既成事实需求，app 级网络 + 公网网关兜底浪费了自托管 PaaS「服务就近内网互通」的核心价值；
2. V2-5「平台牵线共享网络」（托管数据库）已是该模型的特例——project overlay 是推广而非新发明；
3. Tasks API（§3 DT-5）的隔离作用域需要共享网络原语：**三作用域模型（app / project / task-group）一套 overlay 机器全服务**；
4. 内测期真实 app = 2，网络拓扑变更成本处于现值最低点。

**形态**：Project = 网络共享作用域资源（归属真相在状态库，swarm label `fleetly.project` 仅为投影——沿 2026-09-21 蓝图）；成员服务双挂（app 私网 + 项目网）；项目网别名 = `<app>-<service>`（短名仅 app 私网，蓝图原条款）；**naming 公式不变**（app 保持全局唯一，蓝图标注的可行形态，项目段命名变更继续挂账）；swarm 无 NetworkPolicy 的口径不变，不对外承诺项目内 ACL。

**与 RBAC Team 轴的关系（防双真相）**：Team = 身份与权限轴（谁看得见、谁能操作，RBAC W0 既有）；Project = 网络作用域轴（哪些服务的流量可以互通）。app ∈ 恰一个 project（可选，缺省不参加任何项目网，维持 app 私网隔离现状）；project 归属变更走平台 API + 审计，与 team 成员变更互不感知。两轴正交，Console 分区呈现，不合并为单一「组织」概念。

### OT-2 Domains 升级为 state 资源（推翻「label 派生 + expose 首端口」）

**实锤**：`internal/engine/routes.go:29`——「Port 是后端端口（compose expose 首端口）」，一个服务的全部域名共享一个后端端口。messageloop 单服务三端口（9080/9090/9091）、torchwood 单服务双协议（9080 http + 9060 h2c）在现设计下**不可表达**。这不是功能增强，是表达力缺口。

**形态**：state 域名资源，per-domain `{host, service, port, protocol(http|h2c), cert_mode(http01|wildcard)}`；API + Console 可写（admin/deploy scope，现 Domains 页只读+verify 升级）；TLS 仍在平台边缘终结（ACME 沿用）；协议面 h2c 顺势并入（Traefik service `scheme=h2c` 直出，不引入 ServersTransport 资源）；与 v0.3 W5 通配证书/DNS-01 合流（`cert_mode` 字段就位，W5 只补 DNS-01 签发链）。**80→443 重定向维持不做**（机器客户端显式 TLS，v0.1 口径不变）。compose label 载体废除（T-1 免兼容，一次性迁移；实施票可留 label 作首部署种子）。**单一写点仲裁守卫**：state 存在域名行时 label 一律忽略并派事件（label 仅 bootstrap）——dokploy「Domains UI 与 label 规则相同并存时 Traefik 二选一不可控」的事故类在数据模型层消灭，不许以种子形态复活。

### OT-3 Config 资源（补 bind 禁令的真空）

bind mount 拒绝**维持**（宿主耦合、不可复现，立场正确）；但把一切文件配置挤进「Secret 固定 `/run/secrets` 路径」是约定过度——Secret 的固定路径是安全约束的产物（值不可回读），不该外溢到明文配置。**证据**：torchwood 三服务挂 `config.yaml` + migrate 挂 migrations/bootstrap SQL；messageloop 挂 `mlbridge.yaml`——两个真实栈共同阻塞。

**形态**：app 级 Config 资源（明文、版本化、审计、可回读），compose 卷声明新增 `type: config`（target 任意路径，只读）；与 Secrets 同族管理（Console/CLI/API），配额沿用 secrets 口径（每 app 条数/大小上限）。**Config 面向少数运行时配置文件，不映射目录级文件树**——torchwood 的 `migrations/`、`initdb/` 这类目录优先烘镜像（GHCR 镜像本就内含，现 compose bind 属源码运行覆盖），个别引导 SQL（如 `bootstrap-roles.sql`）才走 Config；防止 Config 沦为劣化的 bind mount。

## 3. 确认项（DT——经「总体最优」复审后维持）

| # | 结论 | 要点 |
|---|---|---|
| DT-2 | 部署时镜像代拉 | tag→digest 经 registry API 解析钉定 + swarm 逐节点按 digest 拉取；**凭证须随 service spec 下发（swarm `--with-registry-auth` 等价——现部署路径从不传 auth，私有镜像逐节点拉取会 404，此为实现票必查项）**；平台级 registry credentials 设置（envelope 加密，S3 先例）；Redeploy 重解析可变 tag = `pull_policy: always` 的干净等价物；本地已有镜像走 inspect 快路径（airgap 不回归）；`pull_policy` 字段继续不收 |
| DT-4 | 部署期一次性作业 | compose label `fleetly.job: init`；发布管线晋级前以新 spec 跑 one-shot job，失败即本次发布失败；复用 cron 的 one-shot job 机器 + 看门狗；日志入 VL |
| DT-5 | Tasks API（动态工作负载） | swarm service 承载（稳定 DNS 名，免 inspect）；`restart: none` 缺省（实例崩溃上抛 dispatcher 熔断重建，平台不静默自愈）；owner/TTL label + janitor 回收；平台侧默认加固（CapDrop ALL/只读 rootfs/非 root/pids 限额——服务端强制，不进用户表达面，与 compose 拒 `cap_drop` 同立场）；机具令牌新 scope + 每令牌并发/资源配额；日志入 VL；MVP 不做 exec；**池语义不进平台**；隔离作用域 = OT-1 三作用域网络。**task-group 网络生命周期 = torchwood 租户项目（长活），经幂等 EnsureNetwork 创建；控制面服务（dispatcher/server）每网一次性挂靠（一次 service update，摊销在项目创建时刻），task 只「按名加入」既有网——per-task 动态 attach 明令禁止（torchwood 2026-09-14 attach 残留事故的 swarm 级复刻路径，平台模型必须在结构上消灭该事故类）；internal 变体承不可信函数（DT-7）**。**对账兜底守卫**：substrateRecon 对账面从 services 扩到 networks 与 tasks（state↔swarm 双向，孤儿网/孤儿 task → 派生修正 + 事件）——结构禁令拦住产生点，对账拦住漏网者；验收 = 注入一个 state 外的 `fleetly-` 前缀网络，一个对账周期内被发现并出事件 |
| DT-6 | 函数镜像构建 = build-from-upload API | 通用「上下文 tar 包 + Dockerfile 入口 → buildkitd → zot，返回 digest 引用」；机具令牌 scope；大小/时长配额。dispatcher 只渲染构建上下文（runner/runtime 语义留 torchwood），从此不碰 docker。**无 socket 兼容层**（T-1；dispatcher 的 docker 交互收敛于 `daemon.go` 单文件，直接改造为 API 客户端是一次性成本）。zot push 凭证票被此吞并（平台构建时自推，dispatcher 不需要 push 凭证） |
| DT-7 | 不可信函数隔离 spike | overlay internal 变体承接前，先在 Docker 29 实证 internal overlay 真的封死出网（与现役 bridge internal「无 NAT 全 deny」语义对齐测试）；不过则宿主级 nft 按网段封出网兜底（运维规则，文档化）。这是全案唯一安全退化风险点，spike 是 dispatcher 迁移前置可信条件 |
| DT-8 | 数据面与备份口径 | **经 DT-9 修订：PG 转托管落位**——torchwood postgres 于 T2 割接时直接落托管 `percona-postgresql-18`（dump/restore 进新实例，即获 E4 备份/连接串注入/Console 管理，原「模板不匹配只能栈内自建」的前提被 DT-9 消解）；redis 留栈内（待 P2 AOF 模板选项落地再评估转托管），minio 留栈内（平台 rustfs 面向备份用途，无租户建桶模型）。残余栈内卷（redis/minio）仍无平台备份，内测期口径 = 手动/Tasks 跑 `BGSAVE` 入 S3，**栈内卷备份列 backlog**。割接数据面：messageloop redis AOF 回灌（BGSAVE→新卷恢复）再切 DNS，或明示接受历史清零——割接 runbook 必须写明选择，不许默认静默。**验收探针（机器可判）**：割接后自动跑客户端 recover 探针（携偏移重连被信任 + epoch 键存在 = 回灌成功）；若选择不回灌，须在割接记录写明明示重置——把「别忘了写明」变成「不写明则探针判死暴露」 |
| DT-9 | 托管 PG 版本 × 发行版可选（用户直裁 2026-09-26） | dbtemplate 注册表从固定四模板目录化：template 词表增 `postgres-18`（vanilla）与 `percona-postgresql-18`（含 pgvector，torchwood 现役发行版）；新镜像 digest 钉定入台账（check-image-pins 门禁）；**发行版不新增备份/健康门适配器**（线协议同 postgres，`pg_dump`/`pg_isready` 通用，目录化只动镜像与默认参数层；percona 镜像与官方镜像 env/entrypoint 兼容性为实施票实证项）；CreateDatabase 校验走既有注册表（`E_DB_TEMPLATE_UNSUPPORTED`），Console 创建对话框增模板选择器；**大版本升级不做**（创建时钉死，升 major = dump/restore 新实例，文档明示）；minor 由 digest 钉定、随平台发布以同卷重建演进。目录机制按 engine 通用设计（PG 首个消费者），redis/mysql/mongo 后续按需入目录。**验收 = 每个目录条目过 create→backup→restore 回归矩阵**（E4 35/35 真机模式的扩展）——「发行版不新增适配器」是断言，矩阵把它钉成事实 |
| DT-10 | 必填 env preflight（机制缺口排查新增） | compose label `fleetly.env.required: KEY_A,KEY_B` 声明必填环境变量；发布管线 preflight 对照**含 SetEnv pending 的生效视图**校验，缺任一 → 发布点名拒绝（任何容器启动之前）。拦截层 = 发布管线边界校验（平台侧最早层）；类别判定：一类问题（每个有状态应用都有必填配置；现靠应用自身 fail-closed 兜底 = crash-loop 噪音 + 不设默认值的应用会裸跑）。验收 = **下一个漏配 env 的部署在 preflight 被点名拒绝，而非 crash-loop 后人工读日志** |

## 4. 降级项

**宿主回环端口发布 P2⑧ → 降级**。正解 = **修 messageloop SDK 的 `DialGRPC` TLS 支持**（insecure 硬编码是 README §4.2 自认的已知欠账）——自有项目免兼容，修客户端而不长平台面，宿主端口发布退回 P2 仅服务 QUIC/KCP 与管理便利。这是「自有项目」约束对平台最小化的最典型应用。

## 5. 实施波次

| 波次 | 内容 | 出口 |
|---|---|---|
| **T1** | OT-2（域名资源+协议面）+ DT-2 + DT-4 + OT-3（Config） | **messageloop 整栈迁 fleetly**（mlbridge 暂走 torchwood 公网网关，单 env 改动，独立可回滚）；出口含 DT-8 割接数据步骤（redis 回灌或明示历史清零） |
| **T1.5** | OT-1（Project 资源 + 项目网 + 三作用域机器）；运维动作：staging VPC UDP 放行（跨节点 overlay 前置，未放行前 placement 钉同节点） | 共享网络作用域可用 |
| **T2** | DT-7 spike → DT-5 + DT-6 + DT-9（PG 模板目录化，E4 代码面独立可先行） | **torchwood 整栈迁 fleetly**（dispatcher 改 Tasks/build API 客户端，细胞模型/Redis 控制面/sock 依赖整体删除）；postgres 割接即落托管 `percona-postgresql-18`（DT-8/DT-9），redis/minio 卷迁移或明示重置；mlbridge 切项目内网 DNS |
| **T3** | P2：宿主回环端口 opt-in、托管 redis AOF 选项、通配证书（沿 W5 排期）、QUIC/KCP | 收尾 |

## 6. Spike 清单（T2 前置）

1. Docker 29 internal overlay 出网实证（DT-7；与 bridge internal 语义差测试矩阵）；
2. tag→digest registry 解析 + swarm 逐节点 digest 拉取（Docker29 digest 坑位回归：digest 不落 tag / save/load 丢 tag 均属离线预拉场景，需实证 registry 直拉不受累）；
3. swarm service 纳秒级生命周期压测（Tasks 高频 spawn/回收 vs dispatcher 现役容器直操的时延基线对齐——常驻池模型下单实例生命周期分钟级，预期余量充足，实测钉数）；
4. 多服务编排顺序语义验证：fleetly 发布管线对同 app 多服务的启动顺序/健康门行为，对照 messageloop（redis→messageloop→mlbridge）与 torchwood（postgres→server→worker/dispatcher）的 `depends_on: service_healthy` 真实需求——若为并行启动靠重启收敛，须文档诚实标注（自愈窗口内 crash-loop 噪音）并确认可接受。

## 7. 明确不做

socket 兼容层；`pull_policy` 字段；80→443 重定向；项目内 ACL / NetworkPolicy 承诺；池语义（租约/保温/熔断）进平台；per-request serverless 形态；Secrets 任意路径挂载；**per-task 动态网络 attach**（DT-5 结构性禁令）。
