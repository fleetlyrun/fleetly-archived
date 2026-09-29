# ADR-0010: Team/Project 正交；per-project overlay；per-task attach 明禁

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（T1.5 落地；OT-1 推翻 RBAC「不做项目网」） | 决策 2026-09-26（OT-1）；收录 2026-09-29 | [torchwood-line](../design/2026-09-26-torchwood-line.md)（OT-1/DT-5）、[RBAC](../design/2026-09-23-rbac-teams.md)（被推翻对象）、[stateful-placement](../design/2026-09-17-stateful-placement.md) |

## 背景

2026-09-23 RBAC 设计曾裁决「不做项目网」；torchwood 线带来四条新硬理由（mlbridge→torchwood 内网直连既成需求 / V2-5 共享网络已是特例 / Tasks 三作用域网络复用 / 内测期拓扑变更成本最低），OT-1 重新裁决。

## 决策

1. **Team 与 Project 是正交两轴，不合并**：Team=身份权限轴（RBAC scope）；Project=网络作用域轴（app∈恰一 project）。
2. **per-project overlay**：成员服务双挂（app 网+项目网）；共享网上别名必须带 app 前缀 `<app>-<service>`（裸 compose 服务名跨 app DNS 混流）；短名只留在 app 私有网。
3. **归属真相在状态库，swarm label 只是投影**（`fleetly.project=<id>` 标 service/network/volume）。
4. **per-task 动态网络 attach 明令禁止**——torchwood 2026-09-14 attach 残留致队列卡死事故的结构性消灭；网络生命周期=租户项目长活、幂等 Ensure、控制面每网一次性挂靠（service update 摊销）。
5. **对账兜底守卫**：reconNetworks 扩到 networks+tasks；注入 state 外 `fleetly-` 前缀网必须一个周期内披露（LabelTaskGroup 豁免：task-group 网 state 无行不可枚举，选 label 豁免而非期望集并入；冒名网无 label 仍披露防伪回归）。
6. **E4 库网络是唯一跨 app 通道**+跨项目引用守卫 `E_DB_PROJECT_MISMATCH`；同项目 app 默认亦不互通（不做项目网默认互通）。
7. Swarm 无 NetworkPolicy：overlay 只能网内/网外二元隔离——**项目内 ACL 做不了，不对外承诺**；项目级配额 Swarm 不提供，需状态库自记。

## 后果

- 项目网是显式 opt-in 语义（`projects network attach`），不是默认互通。
- 「不同项目同名 app」仍不可（ADR-0009 公式不动）。

## 关联

ADR-0009（naming 公式不动）、ADR-0001（Swarm 能力边界）、torchwood 线（需求源）。
