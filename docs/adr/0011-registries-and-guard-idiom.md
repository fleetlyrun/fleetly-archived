# ADR-0011: 注册表只增不复用；穷尽性契约走枚举守卫 idiom；scope=proto option

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（落地） | 决策 2026-09-17 起、机制批 2026-09-28/29；收录 2026-09-29 | [架构评审整改](../plan/2026-09-28-architecture-remediation.md)（F1/J、§3 守卫清单）、`internal/errcode`、`internal/eventcode`、`internal/api/scope_completeness_test.go` |

## 背景

2026-09-28 架构评审把缺陷簇归为三个子类：①穷尽性靠人肉（契约更新需 N 处同步）②拷贝漂移（原语未共享）③记录虚报（守卫表声称的覆盖不存在）。承重结论：**穷尽性要从提交者记性搬进 CI 枚举守卫**。

## 决策

1. **errcode/eventcode 注册表只增、永不复用、只新增**：码 ID/事件名一旦发布即冻结；退役码保留占位（如 `E_CAPABILITY_REQUIRES_MULTI_NODE`）。
2. **三链咬合**：构造期 fail-fast（重复/格式/HTTP 映射不一致即 panic）→ golden 快照（`-update` 显式联动，Source 锚引用设计文档）→ usage 反向扫描（只注册不引用即红，豁免需持理由）。
3. **F1 单一真源化**：Code/Event 带 Source 字段，文档表改投影，手抄 map 删除。
4. **枚举守卫 idiom**（源码扫描+枚举+AST+docs 扫描）：新增穷尽性契约/登记面一律走这条 idiom。现有守卫：`TestNoConvergenceOutsidePrimitive`（收敛只许在原语）、`TestNoHandWrittenAppLabelFilters`、`TestNoBareTaskStatusWrites`、`TestOwnershipAnchorConstantsCoveredByPredicate`、`TestMethodScopeRegistryCoversDescriptor`、`TestActiveImplDocsReferenceExistingTests`（活跃 impl 档引用的测试必须存在——上岗首日即抓到三处记录虚报）；白名单条目**不再命中即红**（双向保鲜）。
5. **scope 登记面=proto option 唯一源**（IMPL-ARCH-J，用户批准搬家）：`fleetly.annotations.v1.scope` 字段号 50000；新 RPC 登记=proto 加一行注解；注解词∉词表启动期 panic fail-fast。
6. CI 级门禁：deadcode（豁免清单 `internal/testdata/deadcode-allow.txt` 持理由）、antipattern-grep、buf breaking、wire generate-sync、console schema.d.ts 漂移门。

## 后果

- 这是「好摩擦」的刻意设计：加一码动四处（codes+golden+Source+usage）是减速带，不是缺陷——评审不得以「减少摩擦」为由拆链。
- 动 engine/state/api/logs/runtime 面时对应白名单要保鲜（CONTEXT.md §5 清单）。
- 事件注册表外的 `Tx.AppendEvent` 直接 panic（fail-closed）。

## 关联

ADR-0002（Outbox 依赖注册表）、ADR-0004（convergescan 白名单）、ADR-0009（labelscan）。
