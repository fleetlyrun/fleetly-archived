# 架构决策记录（ADR）

> 承重决策的收拢索引：收录「未来架构评审不应重新论证」的已落锤决策。完整证据链在 [docs/design/](../design/)（按日期的设计文档，含 D 编号决策链）——ADR 引用其编号，不复制全文；冲突时以最新裁决为准。
> 评审流程约定见根目录 [CONTEXT.md](../../CONTEXT.md) §6：与 ADR 冲突的候选须明标「建议重开 ADR-xxxx」并给出新证据。

| # | 决策 | 状态 | 源 |
| --- | --- | --- | --- |
| [0001](0001-swarm-substrate-control-plane-on-manager.md) | Docker Swarm 为 substrate，控制面仅 manager、worker 零安装物 | 已接受（v0.1 落地） | 架构 D2/D12、multi-node D-MN-2 |
| [0002](0002-three-tier-state-model-write-points.md) | 三层状态模型与状态机单写点（CAS+Outbox+审计 fail-closed） | 已接受（v0.1 落地） | 架构 D17、state-model D-STM-1~3/10 |
| [0003](0003-rollback-snapshot-replay.md) | 回滚=快照单层重放，永不使用 Swarm 原生回滚 | 已接受（v0.1 落地） | release-semantics D15/D-REL-5/10/11 |
| [0004](0004-drift-detection-on-convergence-opt-in.md) | 漂移检测默认开、收敛 opt-in；挂起不得被静默撤销 | 已接受（v0.1 落地） | 架构 D11、release-semantics D-REL-6 |
| [0005](0005-ha-boundary-three-layers.md) | HA 边界三层裁决；入口=多 A 记录+连接级重试 | 已接受（落地）；管理面 HA 挂账 v0.3+ | 架构 §2.6、multi-node §2.9 |
| [0006](0006-victoria-stack-default-bundled.md) | 观测栈 Victoria 系默认捆绑（VL+vmalert）；否决 OpenObserve | 已接受（v0.2/v0.3 落地） | v0.2-plan V2-1、B 线 V3W5-E1/D-V3W5-1 |
| [0007](0007-rustfs-opt-in-backup-positioning.md) | RustFS=opt-in 管理组件；同节点备份=便捷层非灾备 | 已接受（落地）；跨节点互备挂账 | v0.2-plan V2-2、object-storage D-S3-2/9 |
| [0008](0008-usability-equals-lightweight.md) | 易用性与轻量同等重要（600MB 默认捆绑门）；对标纪律 D18 | 已接受（元裁决） | v0.2-plan 红线 1、架构 D18 |
| [0009](0009-naming-formula-label-contract.md) | naming 三段公式与 `fleetly.*` label 契约（只增不改语义） | 已接受（v0.1 落地） | state-model D-W0-4、naming 表驱动测试 |
| [0010](0010-team-project-orthogonal-networks.md) | Team/Project 正交；per-project overlay；per-task attach 明禁 | 已接受（T1.5 落地） | torchwood-line OT-1（推翻 RBAC「不做项目网」） |
| [0011](0011-registries-and-guard-idiom.md) | errcode/eventcode 只增不复用；穷尽性契约走枚举守卫 idiom；scope=proto option | 已接受（落地） | remediation F1/J、§3 守卫清单 |
| [0012](0012-git-deploy-cd-built-in.md) | git 部署：CD 内置、CI 不自研；双轨触发保留 | 已接受（v0.1 落地）；push 定位待裁决 | 架构 §1.2/§7、delivery-pipeline |
| [0013](0013-container-form-control-plane-reachability.md) | 容器形态三适配；控制面地址物化；`grpc://` scheme | 已接受（staging 落库） | staging 真机 0da7e82/2053e1b、observability D-W5-4 |
| [0014](0014-backup-ordering-recovery-semantics.md) | 备份等序原则；恢复期禁止自动收敛（只读观察） | 已接受（v0.1 落地） | state-model D-STM-6/7 |
| [0015](0015-compose-controlled-subset.md) | Compose 受控子集：白名单只增不减、受管字段拒绝不静默覆盖 | 已接受（v0.1 落地） | remediation C3、compose golden |
| [0016](0016-no-transition-write-point-constructor.md) | 状态机写点不抽统一构造器（四线零命中语义刻意相异） | 已接受（裁决：不抽象） | 2026-09-29 评审 C2 调查 |
| [0017](0017-move-orchestration-stays-in-api.md) | move 跨模块编排留在 api 服务面（事务/机制已各归其位） | 已接受（裁决：不迁移） | 2026-09-29 评审 C5 调查 |

## 状态语义

- **已接受（落地）**：代码已实现且有守卫/测试钉住。
- **已接受（挂账 vX.Y）**：决策已定，实现排期在后续版本。
- **待裁决**：方向有评估与建议口径，但用户尚未拍板——**不得当作已定决策引用**。

## 新增 ADR 的时机

- 用户在会话中以明确理由否决某架构候选，且该理由未来探索者还会需要 → 当场记录（Status: 已接受）。
- 完成一轮重大设计并落锤 → 从设计文档收拢（本批即此形态）。
- 翻案成功（如 OT-1 推翻 RBAC 裁决）→ 新 ADR 引用被推翻对象，旧 ADR 标注「被 ADR-xxxx 取代」。
