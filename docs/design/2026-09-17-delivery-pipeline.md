# fleetly 交付流水线设计（CI/CD）

| 状态 | 日期 | 关联 |
|---|---|---|
| 已实现（M1） | 2026-09-20 | **v0.1 收口**：PR/nightly/release 三轨道就绪（还原点 683709e），证据见 [v0.1 验收记录 §5](../reports/2026-09-19-v0.1-acceptance.md)；CI 门禁经评审整改加固（action 钉 SHA 等，[整改方案 S20](2026-09-20-remediation-complete.md)）；§2.6 M2（真 VPS dogfooding/TLS 真路径/资源趋势）后置至 v0.2 E8（[v0.2 规划](../plan/2026-09-20-v0.2-plan.md)）。[平台架构设计](2026-09-17-architecture.md) §4.1/§6；[Swarm 评估报告](../research/2026-09-17-swarm-substrate-assessment.md) V1-V7；执行层：GitHub Actions |

## 1. 现状与问题

- 项目自身的构建/测试/发版尚无设计；Swarm 底座引入了必须自证的行为（health gate 等结论为**源码级而非文档承诺**）、引擎版本矩阵（Docker 29.x 破坏史）与升级 E2E 需求。
- 平台对用户的 CI/CD 边界未写明，容易产生「fleetly 会跑我的测试」的预期错位。
- 约束条件：open-core（公开核心 / 私有商业）、默认发行集禁 AGPL/DSAL（许可证守卫）、控制面资源预算需回归监控、`curl | sh` 安装方式需要完整性保障。

## 2. 目标设计

### 2.1 平台对用户的边界（产品侧，需写入用户文档）

fleetly **不做 CI**：不跑用户测试、不校验用户产物。CI 由用户自带的 GitHub Actions / GitLab CI 承担。

fleetly 做 CD：Webhook（签名投递+拉源）/ API / CLI 触发的构建 → 发布 → 路由 → 回滚 → 观察窗（git push(SSH) 收包面已移除，ADR-0012）。演进方向：

- v0.2+：**CI 门禁**——webhook 只接受 CI 已通过的事件（避免「测试挂了还自动上线」）；
- v0.3：预览环境消费 PR 事件（合并即销毁）。

### 2.2 项目自身的三条轨道

**PR 轨道（目标 ≤10 分钟，必需检查）**

1. 静态：golangci-lint、gofmt、staticcheck、gosec、govulncheck；Wire 生成物同步检查（`go generate ./...` 后 git diff 为空，D20）
2. 单元 + race：发布状态机、对账器、spec、加密、配置解析
3. 契约：buf lint + buf breaking（对 main）+ 生成物（genproto/SDK）编译即校验与同步检查（D21）；MCP 工具 schema 快照；错误码注册表校验（只增、不复用）；Compose 子集校验回归（白名单/拒绝清单/受管字段/label 约定）
4. 许可证守卫：默认发行组件清单不得出现 AGPL/DSAL（白名单机制，见 D5）
5. 集成 E2E（单节点 dind）：`docker:29.8.1-dind` 内 install → 部署 fixture 应用（compose）→ stack 对账（增/删服务、受管字段拒绝）→ health gate → 路由 → 回滚 → 平台自升级
6. Console 端：typecheck + build（Playwright smoke 可选）

**nightly 轨道（完整矩阵，红则阻断发版）**

1. 引擎矩阵：dind 29.8.1 × { containerd 存储（默认）、overlay2 } + 上一受支持 minor
2. **V1-V7 全套 + 扩展项**（映射见 §6；含 B2/B3、绑定保持、raft 回退孤儿观察与恢复演练）；多节点场景 = 同一 runner 上两个 dind 容器 `swarm join`
3. 升级 E2E：v_n → v_{n+1}（SQLite 迁移前后数据对比 + 应用不中断）
4. conformance 套件对真实组件（Traefik / zot / S3 端点，CI 内以 RustFS 容器代演〔2026-09-17 裁决：与 D4 打包候选同栈，未来若打包零切换〕——MinIO 已归档且为 AGPL 不用；SeaweedFS 运维面大）
5. 资源基线采样（控制面 idle 内存、构建峰值；趋势告警，不阻断）

**release 轨道（tag 触发 + 人工确认）**

1. V1-V7 + 扩展项 + nightly 全绿
2. **VPS 验证（脚本化）**：通过云 API 起一次性机器 → 干净安装 → 示例应用 → 自升级 → 回滚 → 卸载；**TLS / ACME 真路径只在这里测**
3. 制品：多平台二进制（amd64/arm64 交叉编译，arm64 release smoke）+ 安装脚本 + ghcr 镜像；SBOM（syft）+ 签名（cosign 或 GitHub attestation）+ checksum
4. 兼容承诺检查（Compose 子集契约 / proto buf breaking / N-2 升级路径）+ changelog
5. 文档更新检查（含升级说明）

### 2.3 引擎门禁流程（把 Docker 升级当特殊变更类管理）

- Docker 新 minor 发布 → 开「引擎升级」PR → CI 跑 V7 矩阵 + V1/V3/V4/V6 → 通过后更新受支持版本矩阵与安装脚本下限 → release note 公告。
- 版本基线：Engine ≥ 29.8.1；**iptables 后端**（nftables 暂不支持 Swarm 节点）；dind 与本地开发环境固定到 patch 版本。
- 依赖升级（Renovate 类自动 PR）：普通依赖自动合；**引擎类必须附 V7 回归证据**。

### 2.4 版本与渠道

- SemVer；trunk-based + tag 发版；破坏性变更只在大版本且带迁移工具。
- 平台自升级通道：`stable`（默认）/ `nightly`；用户环境自动升级只走 stable，且遵守「自升级原子化」硬指标（预拉镜像 + 快照 + 失败回退）。

### 2.5 open-core 与安全

- 核心仓库（Apache-2.0）公开，使用 GitHub 免费 runner；商业部分独立私有仓库，Go module **单向依赖**核心；核心 CI 保证独立可构建、不反向依赖。
- Secrets：优先 GitHub OIDC 换取云短时凭据（VPS 验证），不落长期密钥；release 签名密钥最小权限。
- 供应链：锁定 dind 与镜像 digest；制品签名 + 校验和 + SBOM 随 release 附出。

### 2.6 dogfooding

v0.1 发布后：在 staging VPS 上用 fleetly 部署 fleetly 自身（console 端 + 文档 + demo）。staging 验收进入发布检查单——这是真实用户路径的最强验证。

## 3. 关键决策及理由

| # | 决策 | 理由 | 被否方案 |
|---|---|---|---|
| P1 | PR / nightly / release 三轨道分离 | PR 门禁快且稳定；多节点与矩阵测试的 flaky 隔离在 nightly；release 才做昂贵验证 | 单一大流程：慢、flaky 绑架合并、昂贵步骤拖累日常 |
| P2 | E2E 宿主用 dind（平台整体跑在 dind 内） | 单机可重复、无外部依赖、可进 PR 轨道；TLS/DNS 真路径移至 release | 每次起真 VM：慢且贵；纯 mock：验证不到真实行为 |
| P3 | **V1-V7 及扩展项从一次性 Spike 升级为永久回归测试** | 我们依赖的 Swarm 行为是源码级结论（尤其 health gate 无官方文档），必须自证，防上游无声漂移 | 一次性验证：回归无法发现，风险变成信仰 |
| P4 | Docker 引擎升级走独立门禁 | v29 类破坏是已知会重演的风险类别，需要矩阵回归承载 | 与普通依赖同流程：无法携带回归证据 |
| P5 | GitHub Actions + 免费 runner；self-hosted 只在必要场景引入 | 成本与维护最优；多节点/VPS 类验证必要时再上 | 初期自建 runner 农场：过重 |
| P6 | 安装脚本 + 二进制签名与校验和 | `curl \| sh` 的安全基线；Open-Core 项目需要可验证的发布链路 | 仅靠 HTTPS：无完整性保障 |

## 4. 分步实施计划

| 里程碑 | 内容 |
|---|---|
| M0（与 Spike 同步） | 单节点 dind E2E 骨架；PR 基础门禁（lint/单元/契约） |
| M1（v0.1 前） | 三轨道齐备；V1-V7 及扩展项全部进 nightly（含绑定保持与恢复演练）；安装脚本 + 制品签名 + 校验和；许可证守卫；引擎矩阵 |
| M2（v0.1 后） | dogfood staging；VPS 验证脚本化；资源基线趋势 |
| M3（v0.2） | 多节点 nightly 稳定化（必要时 self-hosted runner）；conformance 套件真实组件化 |

## 5. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| 多节点 dind 在共享 runner 上 flaky | 门禁可信度下降 | 只放 nightly；连续失败开 issue 而非重试掩盖；必要时迁 self-hosted |
| CI 成本失控 | 维护负担 | PR 门禁精简 + 并发取消 + Go/BuildKit 缓存；nightly 定时 |
| 引擎升级回归漏检 | 用户环境故障 | 强制 V7 证据；受支持矩阵显式维护并在安装检查中校验 |
| 发布链路被篡改 | 供应链攻击 | 签名 + checksum + SBOM；tag 触发 + 人工确认；OIDC 短时凭据 |
| 门禁被绕过 | 质量与兼容承诺失守 | 分支保护 + 必需检查；错误码/spec 变更需显式评审 |

## 6. V1-V7 与轨道的映射（与架构文档 §6 对齐）

| 验证 | 内容 | 轨道 |
|---|---|---|
| V1 | health 失败时更新 paused、旧任务不中断（含 B1） | PR（子集）→ nightly（全） |
| V2 | 本地 digest 镜像免 pull | PR E2E 内 |
| V3 | 更新期间路由零失败、VIP 稳定 | nightly |
| V4 | keep-alive 陈旧连接复现与治理 | nightly |
| V5 | 单 manager 故障 + `--force-new-cluster` 恢复 | nightly（多节点） |
| V5b | raft 回退后孤儿容器命运（0/5/30min 观察） | nightly（多节点） |
| V6a | 卷与绑定语义（无约束迁移得空卷、加绑定后钉住） | nightly（多节点） |
| V6b | 绑定保持与基本漂移（down→blocked→恢复；remove→人工 rebind；数据不匹配 409） | nightly（多节点） |
| V7 | 引擎升级回归矩阵（dind 双存储模式） | 引擎升级 PR + nightly |

补充：B2「归位零成本」与 B3「LB 端点时机」并入 PR E2E 与 nightly（见架构 §4.1）；状态回归项（写前直读冲突、孤儿只登记不删除、备份顺序与密钥指纹、导出 tar 不含密钥、审计 fail-closed、事件游标）按同轨道分布（见[控制面状态模型](2026-09-17-state-model.md) §6）。

## 7. 明确不做的事

- 不做 CI 执行引擎：不用 fleetly 跑用户测试（边界见 2.1）
- PR 门禁不跑多节点 / VPS / TLS / 大矩阵（防 flaky 与慢）
- 不引入第三方 CI 服务（CircleCI/Jenkins）——触发条件：GitHub Actions 无法满足并发或成本
- 不做自动持续部署到用户环境（发布需人工确认）
- 初期不自建 runner 农场（触发条件：多节点 nightly 长期不稳定且云 runner 无法缓解）
