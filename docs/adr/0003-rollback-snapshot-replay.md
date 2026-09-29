# ADR-0003: 回滚=快照单层重放，永不使用 Swarm 原生回滚

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1 落地） | 决策 2026-09-17；收录 2026-09-29 | [release-semantics](../design/2026-09-17-release-semantics.md)（D15、D-REL-5/9/10/11、Spike B2）、[架构](../design/2026-09-17-architecture.md) |

## 背景

Swarm 自带 `--rollback`，看起来免费；但语义与平台需要的「按已验证快照回滚」不同。

## 决策

1. **D15：`failure_action=pause` 固定，永不使用 Swarm 原生回滚**——原生回滚清空唯一 `PreviousSpec`、不覆盖 PENDING，语义不可控。
2. **回滚=按归一化 compose+覆盖层的版本快照（revision，保留 5 版）单层重放**；自动回滚与手动回滚走同一原语（EnqueueRollback）。resume=清权威位后空目标重放 active revision，同样走正常发布管线。
3. **失败分流唯一判据=`first_healthy_at`**；首发失败 scale=0 保留现场（D-REL-5）——不销毁证据。
4. **首发/恢复失败不二次自动回滚**（D-REL-10）——避免回滚环。
5. **不做熔断**（D-REL-11）——Dokploy 无熔断也是头部体验（对标纪律 ADR-0008）。
6. **归位重放禁 `--force`**（Spike B2 实测）：同内容重放任务零替换是归位零成本的前提，`--force` 必重建。
7. **词表**：rollback=平台快照重放（「回滚」）；restore 专属灾难恢复（UL 词条）。

## 后果

- 回滚历史如实落 `kind=rollback` 部署行（审计与回滚基线合一）。
- 「单层重放」= rollback 不递归：目标 revision 的内容就是终态，不再向前追溯。

## 关联

ADR-0002（写点与快照 env 语义）、ADR-0004（自动回滚是 opt-in）。
