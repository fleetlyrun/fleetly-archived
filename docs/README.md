# fleetly 文档索引

> 更名链：**edgesets** →（2026-09-17）**edgefleet** →（2026-09-18）**fleetly**；守护进程 `fleetlyd`、CLI `fleetly`、label 命名空间 `fleetly.*`、模块 `github.com/fleetlyrun/fleetly`。当前名下的文档与代码已统一替换，历史名仅存于本注记与 git 历史。

## 设计方案（docs/design/）

| 日期 | 标题 | 状态 | 一句话说明 |
|---|---|---|---|
| 2026-09-17 | [平台架构设计](design/2026-09-17-architecture.md) | 已实现（v0.1 切面） | 自研 Go 控制面 + Swarm 底座的轻量 PaaS 总体架构（应用模型 = Compose 规范；含目标用户画像、栈边界与对外口径、2 节点 HA 边界口径、每节点入口与集中证书、执行中继 D19、基础框架 lynx+wire D20、API 面 gRPC+grpc-gateway D21）、关键决策、v0.1~v0.3 路线图 |
| 2026-09-17 | [交付流水线设计（CI/CD）](design/2026-09-17-delivery-pipeline.md) | 已实现（M1） | PR/nightly/release 三轨道、V1-V7 永久回归、引擎门禁、GitHub Actions 落地；M2（真 VPS dogfooding）后置 v0.2 E8 |
| 2026-09-17 | [发布失败与回滚语义](design/2026-09-17-release-semantics.md) | 已实现（v0.1 切面） | pause 冻结 + 快照单层重放、四层窗口、观察窗默认告警、失败场景矩阵与错误码 |
| 2026-09-17 | [stateful 放置（节点约束）](design/2026-09-17-stateful-placement.md) | 已实现（v0.1 单节点切面） | 意图/绑定/执行三层、有卷自动钉住、平台节点 ID 为锚、数据安全前哨、人工 rebind、drain 维护语义 |
| 2026-09-17 | [控制面状态模型](design/2026-09-17-state-model.md) | 已实现（v0.1 切面） | 权威/派生缓存/实时直读三层、最小 label 集、孤儿保护、备份等序与 L1/L2 恢复、一键导出 |
| 2026-09-20 | [评审遗留问题完整解决方案（S13-S20）](design/2026-09-20-remediation-complete.md) | 已实施 | [架构评审](reports/2026-09-19-architecture-review.md) 遗留项的决策完备方案：H9 路由撤销通道（noop@internal 兜底）、H15 双轨验签（openssl 兼容轨）、类 A-D 机制收口（出站出口/契约门禁/超时闭环）、S18-S20 中低严重度分波次方案与机制验收；2026-09-20 全部落地 |
| 2026-09-20 | [E1 多节点包专项设计](design/2026-09-20-multi-node.md) | 已裁决（方案冻结） | 拓扑与组件面（worker 零安装物）、平台三子域与安装项、join 向导（端口矩阵+token 自动 rotate）、8423 TLS 配置通道、证书内联统一、zot 部署器与镜像管线（base_domain 配置即部署）、placement 多节点三因子、节点锚定 duty、restic 迁移+rebind、HA 边界诚实口径；D-MN-1~14 裁决轮全落定；9 票据 + 三断言验收 |
| 2026-09-20 | [E4 数据库托管专项设计](design/2026-09-20-managed-databases.md) | 已实现+真机验证（W4） | 库实例=**独立一等资源**（D-DB-1 用户终裁：自有表/API/七态生命周期 + EnterDbPhase 单写点；组件级复用放置/卷/substrate/secret/审计/备份）、内置模板注册表、V2-5 共享网络、凭据 source=system 注入与轮换（FZ-1/R5）、备份恢复适配器（pg_dump/RDB + db_backups 台账）、secrets 全 app 开放（external-only）；D-DB-1~11 裁决轮全落定；含 D-REL-9 回滚 env 语义修正（D-DB-11，已回写发布专项） |
| 2026-09-22 | [E6 观测专项设计](design/2026-09-22-observability.md) | 已实现+真机验证（W5） | VictoriaLogs 默认捆绑（V2-1：duty 收敛=host 网络+进程回环监听〔D-W5-4 落地注记〕+hub ES bulk 直推+直播面分离）+ 统一检索（SearchLogs+注入安全负向+访问日志 RouterName 候选集反解+部署归因）+ metrics **opt-in**（D-W5-2：三件套 host 网络回环拓扑+uPlot 图表+多副本水位；预算实测默认面 492MB<600MB、metrics-on 671MB 超顶如实记录）+ 通知 Webhook 首发（V2-6：glob 订阅/HMAC 验签/3 次退避/台账/**通知零事件防回环红线**）；E2 MCP 暂缓出 v0.2（D-W5-1） |
| 2026-09-22 | [E7 Web 终端+控制面 TLS 专项设计](design/2026-09-22-web-terminal.md) | 已实现+真机验证（W5） | fleetly-exec global service 执行中继：**反向常连通道**（D-W5-3 修订 D19 字面——宿主不可路由 overlay 的结构性解法；安全面条款全保：API 收窄+label 卫兵+集群 token+时限+terminal scope+审计）+ 第二第一方镜像（CI 首推已钉 digest）+ xterm UI + ticket 流；控制面 TLS（V2-8：off/platform/manual 三态+双面同证书+CLI/SDK+relay wss）；**gateway TLS 回拨缺陷真机修复**（TLS 形态下明文回拨断全量 REST——e2e TLS-9 永固） |
| 2026-09-23 | [团队与多用户 RBAC（用户/团队/项目）设计](design/2026-09-23-rbac-teams.md) | 已裁决（方案冻结，W0 完成） | **fresh-install 版本**（D-W0-5 修订：无 v0.2 升级路径——用户澄清仅一处 staging 可清空重建）；用户注册登录（首用户=平台管理员+个人队，bootstrap token 注册即吊销）/Team+Project 多租户（注册默认个人队/邀请链接/四档角色 viewer-developer-admin-owner+平台管理员分离；**Project 层角色 = 队内覆写形**（D-W0-2 修订：仅限团队成员双向覆写、owner 不可覆写、可见性零级联；A 形全量挂账））/项目隔离（**v0.2 每 app 专属 overlay 底座已结构性具备**，准入守门收敛到 E4 库网络一处）/双门准入（token scope+角色，fail-closed）/app 库名 project 内唯一（每项目各有 web）+底座命名 team-prj-app 三段（D-W0-4 二修，fresh 首次部署即新形；MoveApp=换名重部署）/审计 90d 可调+双读面/FZ-12 指纹披露+变更事件；D-W0-1~9 全落（商业线 = **开源全功能，付费=云+企业件** D-W0-7） |

## 调研报告（docs/research/）

| 日期 | 标题 | 状态 | 一句话说明 |
|---|---|---|---|
| 2026-09-17 | [竞品调研：轻量自托管 PaaS 的六个关键问题](research/2026-09-17-competitive-landscape.md) | 已完成 | 零停机/多节点/声明式/MCP/构建/差评六主题；收敛点、死亡区、借鉴与避开清单（含对架构文档的 13 条修订建议） |
| 2026-09-17 | [Swarm 作为多节点底座的可行性评估](research/2026-09-17-swarm-substrate-assessment.md) | 已完成 | 源码级验证发布语义/路由/镜像存储/故障语义；结论：建议采纳（含 7 项不可退让的 Spike 验证门）；修正前报告 Swarm 表述 |
| 2026-09-20 | [对照调研：zane-ops（Python/Django + Temporal + Swarm）](research/2026-09-20-zane-ops-comparison.md) | 已完成 | 同底座不同重量级路线对照：部署编排多处独立收敛（印证）、Celery→Temporal 迁移史、10 容器 ≈7GB 控制面实证；R1-R8 借鉴 / A1-A6 避坑清单，输入 v0.2 规划 |
| 2026-09-25 | [评估：OpenObserve 替换 Victoria 系列](research/2026-09-25-openobserve-vs-victoria.md) | 已裁决（V3W5-E1 维持 Victoria） | 单二进制全栈观测平台（AGPL，22.1k stars）两角色评估：默认日志面否决（预算硬碰撞——实测 v1.0.4 idle RSS 340-346MiB vs VL 12.7MB）；opt-in 观测面可行但不值得（无 scrape 需另配采集器组件数不净减+PromQL v2.45 收敛期+resolved 通知缺口）；挂账「观测高级层」候选带重评三条件；顺带收获：OO 官方支持 RustFS 后端、摄入失败静默 200 与管道错误传播同族坑 |

## 实施规划（docs/plan/）

| 日期 | 标题 | 状态 | 一句话说明 |
|---|---|---|---|
| 2026-09-17 | [实施任务分解（Spike + v0.1）](plan/2026-09-17-task-breakdown.md) | 已完成（26/26 票，2026-09-19） | 35 张垂直切片票据（T0 骨架 5 / T1 Spike 4 / T2 v0.1 26），依赖图 + 验收锚定设计文档 + 105-146 人日估算；v0.2 已细化移至 [v0.2 规划](plan/2026-09-20-v0.2-plan.md) |
| 2026-09-17 | [v0.1 实现切面冻结清单](plan/2026-09-17-v0.1-scope-freeze.md) | 已收口（v0.1 关闭，2026-09-20） | T0.5 产出：三专项 v0.1 切面逐项裁决（FZ-1 连接串后置、FZ-2 退化信封、FZ-3 15 码 HTTP 缺省、FZ-4 cron.timed_out、FZ-5 校验信封）+ T0 完成回填 + 冻结轮确认（FZ-6~12） |
| 2026-09-20 | [v0.2 实施规划](plan/2026-09-20-v0.2-plan.md) | **已完成**（W0-W5 + v0.2.0 发布 + v0.2.x 收尾波，2026-09-23） | 波次 W0-W5（设计立项/引擎加固 → dogfooding → 多节点 → S3+Cron → 数据库托管 → 观测/通知/终端/TLS〔MCP 暂缓 v0.3〕）；裁决 V2-1~V2-8 全落；v0.2.0 已发布（2026-09-22） |
| 2026-09-23 | [v0.3 实施规划](plan/2026-09-23-v0.3-plan.md) | **范围已裁决**（四票 2026-09-23） | **主线先 C（团队/RBAC/审计留存）后 B（生产深化）**——用户直裁重排架构 §4.4 顺序；V3-2 MCP 继续降级（退出 v0.3 主线）、V3-3 DR 继续挂账、V3-4 挂账顺手收；波次 W0-W6 骨架 + W0 开放问题清单（人类认证形态/角色粒度/审计留存/FZ-12/商业分界/单操作员迁移） |
| 2026-09-28 | [架构评审机制缺口整改批](plan/2026-09-28-architecture-remediation.md) | 已实现（九票，A-D 2026-09-28 / E-H 2026-09-29） | 2026-09-28 架构评审缺陷簇的机制补齐：A=漂移 extras 死腿修复+归属过滤 module 化+LabelApp 扫描守卫（28ad020）/ B=服务收敛原语单点化+任务线无差别重申修复+调用点白名单（1ead912）/ C1=归属锚谓词住进声明地+锚枚举双向钉死（46dc3da）/ C2=task 转移表+单写点内核+裸写扫描（dc88c00）/ D=RPC scope 登记完整性四向测试（c266e91，纯测试票）/ E=对账「披露一次」骨架 disclosure module 单点（211adf6，评审首推）/ F=三小票（errcode Source 单一真源+rotate 命名归族+dbtemplate engineTools 表，22915fa）/ G=哨兵→信封映射表 46 行单点+表测试六件套（5124337）/ H=logs 轮询骨架 pollStream 收编+sink 参数化（148e689）；三类元型判定 + 守卫验收句九条 + 遗留挂账（6b 需 rbac-teams 裁决、9 跨仓需 ADR、E/F/G/H 尾部小项） |

## 验收报告（docs/reports/）

| 日期 | 标题 | 状态 | 一句话说明 |
|---|---|---|---|
| 2026-09-19 | [架构评审报告](reports/2026-09-19-architecture-review.md) | 归档 | 全仓对抗式评审：H 级高严重度 2 项 + 类 A-D 系统性发现，S1-S12 即时修复、S13-S20 见[整改方案](../design/2026-09-20-remediation-complete.md) |
| 2026-09-19 | [资源校准报告](reports/2026-09-19-resource-calibration.md) | 归档 | fleetlyd+dockerd idle ≈160.7MB（<200MB 达标）；Traefik/containerd 单列；容量边界实测（≤50 apps / ≤200 域名 / 并发构建 2） |
| 2026-09-19 | [v0.1 端到端验收记录](reports/2026-09-19-v0.1-acceptance.md) | 归档 | 八项能力 + 横切硬指标全绿；旅程 CRITICAL 61s（预算 20min）；v0.1 发布依据 |

状态维护：实现完成后将文档状态改为"已实现"并补 PR 链接；方案废弃时改为"已废弃"并指向替代文档。方案文档只追加关联，不删除。
