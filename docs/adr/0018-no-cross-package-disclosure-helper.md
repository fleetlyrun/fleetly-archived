# ADR-0018: 披露骨架不做跨包抽取——janitor 同款拷贝经取证为刻意分叉，重叠面仅约 10 行

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（裁决：不抽象） | 2026-09-30（架构评审 C8 / 整改批 E 尾调查后） | [ADR-0016](0016-no-transition-write-point-constructor.md)、[ADR-0017](0017-move-orchestration-stays-in-api.md)、engine/disclosure.go、state/janitor.go |

## 背景

整改批 IMPL-ARCH-E（211adf6）把披露骨架收拢进 `engine/disclosure.go` 时留下挂账（E 尾）：`state/janitor.go` 的 `staleSeen`/`reportStale` 是「披露骨架同款跨包拷贝」，需跨包 helper 形态裁决。2026-09-30 架构评审候选 C8 逐构件取证后裁定**不抽取**。

## 证据（重叠面远小于族形相似造成的印象）

1. **真实共享面 ≈10 行**：seen-set 记忆三方法（`reported`/`mark`/`clear` ↔ `staleSeen` 判定/标记）＋「事件进事务」一腿（`InTx`+`AppendEvent`）。仅此而已。
2. **骨架主体在 janitor 无对应物，差异面全部刻意**：审计配对（`writeDisclosure` 事件+审计同事务 fail-closed）、`discloseOnce` 先报后标/事务成功才标记、`scanGate` 节拍门——janitor 均无；反向 janitor 恒先标后写、错误吞掉、仅事件无审计腿、无 sweep/无 gate（`time.Ticker` 单频）、重启清零=唯一恢复。这是**留存守护**（超龄只告警不自愈、漏报劣于重复）与**对账披露**（fail-closed、恢复即清零）的领域策略差，非拷贝漂移；janitor.go:107 注释已自证同语义参照。
3. **方向约束下三式落位皆为一成共享面引入新形态**：state 不可 import engine（分层硬事实，state 全目录不引任何域包）。能同时覆盖 engine 诸 face 与 janitor 的落位只有：骨架下沉 state（janitor 差异面参数化进骨架=复杂度搬进参数管道，且 state 公画变宽）；state 之上新包（naming 同位，但 janitor 够不到，除非迁出 stale 上报面=动归属）；接口化下沉到 state 之下（EventSink/AuditSink，生产域无先例）。
4. **deletion test 判负**：删掉假设中的跨包 helper，两包各自长回现状——同 [ADR-0016](0016-no-transition-write-point-constructor.md) 形态。

## 决策

维持现状：`engine/disclosure.go` 是全引擎一份披露骨架（CONTEXT.md §3「披露」词条口径不变）；`state/janitor.go` 的 `staleSeen`/`reportStale` 保持包内自持，其与骨架的语义差异按本 ADR 记载为刻意分叉。E 尾挂账就此关账。

## 重开触发

出现**第三个**「once-披露」消费方（既非 engine 对账 face、亦非 janitor 留存守护的新形态）时，重评骨架下沉 state（方案 A）。

## 关联

ADR-0016/0017（同型「调查后裁决不抽象」姊妹条）、CONTEXT.md §3「披露」、整改批 IMPL-ARCH-E。
