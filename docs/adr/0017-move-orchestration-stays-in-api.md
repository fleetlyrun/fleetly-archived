# ADR-0017: move 跨模块编排留在 api 服务面——事务与机制已各归其位

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（裁决：不迁移） | 2026-09-29（架构评审 C5 调查后） | [ADR-0002](0002-three-tier-state-model-write-points.md)、[ADR-0009](0009-naming-formula-label-contract.md)、api/projects.go MoveApp/MoveDatabase |

## 背景

2026-09-29 架构评审候选 C5 提议：MoveApp/MoveDatabase 的跨模块事务编排住在 api 服务面（ownership.go 定义三个编排端口、projects.go 五步序），「概念 home 错位」，应下沉为 engine 侧域模块。逐层取证后裁定**不迁移**。

## 证据（编排已分解到各归其位的家）

1. **事务性写入在 state 单写点**：`Store.MoveApp`/`Store.MoveDatabase`——归属切换 + 跨项目唯一性 409 + 同事务审计（app.moved），ADR-0002 写点纪律管辖。
2. **底座机制在各领域模块且直接可测**：换名重部署/等待交换/摘旧服务在 `engine/move.go`（`move_test.go` 直测，不拉 api harness）；库侧换名收敛在 `database/move.go`；摘旧网在 `ingress.Manager`（best-effort 语义）。
3. **api 面剩余 = 单调用方胶水序**：引用三形态解析（team/prj/name 限定形——proto 请求面关切）、`requireMoveAdmin` 鉴权、错误→信封映射（E_MOVE_SAME_TARGET/E_APP_EXISTS 族）、③④⑤ 调用序（约 25 行，五步序注释即契约：④超时保守放弃清扫、幂等收尾=人工重跑同参数）。
4. **deletion test 判负**：把 ③④⑤ 序抽成「move 事务域模块」——序只有一个调用方，删掉该模块序原样长回 handler 一处；域模块反而需要反向收编 ingress 端口（engine 不拥有摘网语义）。复杂度不浓缩，只加一层间接。

## 决策

move 编排保持现状三层归位：state 持事务写点、engine/database/ingress 持各域机制、api 持解析/鉴权/信封/单调用方调用序。编排端口的「接口在 api 定义」（方向纪律：消费方定义端口）不变。若未来出现第二个编排调用方（如 CLI 直连域层、cron 自动改派），再评估序的域化。

## 关联

ADR-0002（写点）、ADR-0009（MoveApp=换名重部署语义）、ADR-0016（同批「不抽象」裁决的姊妹条）。
