# ADR-0002: 三层状态模型与状态机单写点

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1 落地；写点族持续扩充至 v0.2/T 线） | 决策 2026-09-17；SetAppSuspended 2026-09-29；收录 2026-09-29 | [架构](../design/2026-09-17-architecture.md)（D17）、[state-model](../design/2026-09-17-state-model.md)（D-STM-1~3/10、§2.6/§2.10）、[managed-databases](../design/2026-09-20-managed-databases.md)（D-DB-1） |

## 背景

控制面状态放哪里、谁有权写：把运行域实况全量镜像进状态库会引入双写者；完全不记则决策无据。

## 决策

1. **三层状态模型（D17/D-STM-1~3）**：
   - **权威态**（SQLite）：期望态、历史、凭证——单一写者；
   - **派生缓存**（观测快照，带 `observed_at/stale`）：**禁止用于决策**；
   - **实时直读**（写前直读 substrate）：冲突返回 `E_STATE_VERSION_CONFLICT` 409。
   - 三判据顺序：单一写者 ＞ 可重建性 ＞ 历史性。`nodes` 降级为观测缓存、不承诺「最后心跳」（D-STM-2，Swarm 不暴露该时间戳——文案纪律由 `TestNoHeartbeatWording` 守卫）。
2. **状态机单写点，四件一拍**：每条状态机线只有一个写点函数，同事务完成 ①转移表校验 ②CAS（`WHERE state=<from>`）③tombstone/时间锚 ④事件（Outbox）+审计。现有实例：部署线 `EnterPhase`、库线 `EnterDbPhase`、task 线 `updateTask`、应用线 `transitionApp`/`SetAppSuspended`。并发互斥由前置态前哨+CAS 结构性成立，**不建部署队列副本**。
3. **审计 fail-closed + 事件 Outbox 同事务（D-STM-10）**：审计失败即操作失败；`events.seq` 单调作 SSE 游标，断档显式 410；通知投递器经游标轮询，**不建进程内总线**；secret 值永不入事件/审计/日志。
4. **权威位模式（authoritative bit）**：用户意图用加法列表达——DB `paused`（D-DB-1 七态）→ app `suspended`（迁移 00028）。派生词表第一判短路：用户权威位直投影压倒观察态（`DeriveAppState` 裁决序 suspended>down>blocked>degraded>running）。
5. **tombstone-first 删除（state-model §2.6）**：`deleting→deleted`+保留期；恢复不复活；`deleting` 无失败出边，reap 幂等重试。
6. **app 状态机是派生视图（§2.10）**：`down>blocked>degraded>running`；`unstable` 仅是 deployment verdict 不入 app 词表。
7. **快照回滚 env 语义（D-REL-9）**：非密钥 env 随快照回滚；`source=system` 行（库凭据/连接串）取当前值——否则凭据轮换后回滚必回放旧密码断连。

## 后果

- 新状态机不是新文件+手抄形状，而是「转移表+写点函数」的新实例（构造器化是深化候选，见架构评审）。
- 零命中 CAS 需区分三态（NotFound/Tombstoned/Conflict）并各有注册码（如 `E_APP_SUSPEND_CONFLICT`）。
- 迁移纪律：权威位=加法列，lifecycle 词表不动。

## 关联

ADR-0001（单写者红线）、ADR-0003（写点承载的发布语义）、ADR-0011（事件/审计入 eventcode/errcode 注册表）、ADR-0014（备份恢复的等序约束）。
