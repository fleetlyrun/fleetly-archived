# ADR-0016: 状态机写点不抽统一构造器——四线零命中语义刻意相异

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（裁决：不抽象） | 2026-09-29（架构评审 C2 调查后） | [ADR-0002](0002-three-tier-state-model-write-points.md)、[state-model](../design/2026-09-17-state-model.md) §2.3、[架构评审整改](../plan/2026-09-28-architecture-remediation.md)（IMPL-ARCH-C2） |

## 背景

2026-09-29 架构评审候选 C2 提议：state 包四条状态机线（deployments `EnterPhase` / db_instances `EnterDbPhase` / apps `transitionApp`+`SetAppSuspended` / tasks `updateTask`）各持一份「转移表校验+CAS+tombstone 锚+Outbox+审计」形状，抽一个 table-driven 转移写点构造器收编。逐线取证后的结论是**不立**。

## 证据（为什么不抽象）

1. **真正逐字同构的部分极小**：事件追加循环（约 5 行 ×4）与「ExecContext → RowsAffected → 零命中分派」骨架（约 8 行 ×4）。合计约 50 行；为此在台账写路径上加一层间接不值得——写点注释即契约（ADR-0002），显式展开正是可读性所在。
2. **零命中语义刻意三相异，是领域策略不是重复**：
   - tasks：`n==0` = 幂等跳过成功（六个意图原语的既有语义，`return nil // 幂等/并发翻转`）；
   - db_instances：`n==0` = 三态区分报错（行缺失 → `ErrDatabaseNotFound`；行在已推进 → `ErrDatabaseStateConflict` 携当前态——api 层映射 `E_STATE_VERSION_CONFLICT` 附 current_state，D-DB-8）；
   - apps 挂起位：`n==0` = 三态不同哨兵（`ErrAppNotFound` / `ErrAppTombstoned` / `ErrAppSuspendedConflict`——注册表只增不复用，ADR-0011）；
   - deployments：经 `updateDeployment` 共享内核，其复杂度是 25 列动态 patch 构造（与**非转换写** `UpdateDeployment` 共用——patch 机制不是转换形状）。
3. **SET 构造四线根本不同**：动态 patch 列表（deployments）/固定列+墓碑锚 switch（db）/固定列+附带列（apps 删终态清 project_network_attached）/逐意图 WHERE 谓词（tasks）。
4. **deletion test 判负**：删掉假想构造器，四线各自长回接近现状——复杂度不浓缩、只搬进参数管道；构造器接口≈四份实现之和=浅模块（interface 与 implementation 一样复杂）。
5. **既有守卫已钉住该钉的**：`TestNoBareTaskStatusWrites`（裸状态写拒写）、转移表穷举测试、CAS/墓碑/事件同事务的行为测试——「写点纪律」由测试与注释承载，不需要类型收编。

## 决策

不抽统一状态机写点构造器。新状态机线的模板 = 既有同族写点的显式展开（转移表 + CAS + 锚 + Outbox + 各线零命中策略），不进公共参数化框架。若未来第五条线出现且与某既有线**零命中语义完全一致**，允许两线共享私有小助手（如事件追加循环），三线以上同语义才重新评估。

## 关联

ADR-0002（写点四件一拍纪律——本 ADR 是其实现形状的裁决）、ADR-0011（哨兵只增不复用——零命中分派差异的根因）。
