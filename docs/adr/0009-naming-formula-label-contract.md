# ADR-0009: naming 三段公式与 `fleetly.*` label 契约

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1 落地；二次修订 D-W0-4） | 决策 2026-09-17、修订 2026-09-23（用户裁定）；收录 2026-09-29 | [state-model](../design/2026-09-17-state-model.md)（D-W0-4、§2.4）、`internal/naming`（表驱动测试）、git 8bf821f（64 上限） |

## 背景

Swarm 对象命名是平台与 substrate 的接缝语言：公式即契约，改公式=文档级变更。

## 决策

1. **D-W0-4（二次修订，用户裁定）**：命名三段 `fleetly-<team>-<prj>-<app>-<service>`；app/库名 **project 内唯一**；保留字从 app 名迁 team slug。
2. **卷/库卷公式不变**（ULID 尾缀天然防撞=零卷迁移成本）。
3. **`fleetly.*` label 契约化：只增不改语义**；写者唯一=适配器 Marker 端口（state-model §2.4）。
4. **归属过滤单点**：三段限定形字面量唯一存在于 `internal/engine/ownership.go`（+`internal/naming` 常量）；手写 label 过滤被 `TestNoHandWrittenAppLabelFilters` 守卫禁止——原型事故：extras 死腿（漂移报干净、reploy 却会删）。
5. **swarm 64 字符上限三层策略**（git 8bf821f）：内容寻址族截断 name 段 / 瞬时 job 名截断 / 可寻址名显式报错。
6. **MoveApp=换名重部署**（披露停机）——不做原地改名魔术。
7. 公式在 `internal/naming` 由**表驱动测试逐字钉死**；项目化（ADR-0010）不动公式——app slug 全局唯一规避了加 project 段。

## 后果

- 新对象类型必须先在 naming 注册公式+label，再动 substrate。
- 真机撞过的相关坑：ConfigName 无总长守卫（`naming.go` ConfigName，64 上限溢出 bug 为挂账票）——修名族时同查 joinName 族总长守卫。

## 关联

ADR-0010（项目网别名 `<app>-<service>`）、ADR-0011（labelscan 守卫）、ADR-0001（moby 类型止步 substrate）。
