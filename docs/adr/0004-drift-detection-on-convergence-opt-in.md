# ADR-0004: 漂移检测默认开、收敛 opt-in；挂起不得被静默撤销

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1 落地；挂起豁免 2026-09-29） | 决策 2026-09-17（D11/D-REL-6）；收录 2026-09-29 | [架构](../design/2026-09-17-architecture.md)（D11）、[release-semantics](../design/2026-09-17-release-semantics.md)（D-REL-6）、git e105e67（suspend） |

## 背景

GitOps 世界两派之争：检测到漂移后要不要自动拉回。行业证据（ArgoCD #13598：全自动收敛会在事故中被用户强制关闭；Terraform #35382：检测与变更应解耦）。

## 决策

1. **D11：漂移检测默认开，收敛 per-app opt-in**。检测是用户与 AI Agent 共同的事实来源（drift 只读展示+事件）；收敛是显式变更动作（`ConvergeApp` 人工一次性、带审计）。
2. **D-REL-6（用户裁决）**：观察窗默认只告警；**自动回滚 per-app opt-in**；stop-first 失败的强制归位**不可关闭**（安全底线与便利分层）。
3. **挂起语义（e105e67）**：挂起期 0 副本**不是漂移**；drift 扫描/autoscaler/cron 调度按权威位豁免；**opt-in 也不得收敛回快照**——挂起不得被静默撤销（用户意图压倒一致性）。
4. 同族哲学：收敛原语只在 `converge.go`（白名单纪律见 ADR-0011）；「非收敛对账」站点（scaleToZero/autoscaling/initjobs/suspend 排水保持器）逐一登记白名单。

## 后果

- 「检测开、收敛关」的默认组合=平台不与用户抢方向盘。
- 豁免逻辑有成本：每加一种权威位/非收敛形态，drift/autoscaler/cron 三处豁免面都要同步——这是已付的设计税，新形态照抄 suspend 的登记路径。

## 关联

ADR-0002（权威位第一判）、ADR-0003（回滚原语）、ADR-0011（convergescan 白名单守卫）。
