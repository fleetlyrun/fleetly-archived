# ADR-0006: 观测栈 Victoria 系默认捆绑（VL+vmalert）；否决 OpenObserve

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.2/v0.3 落地） | 决策 2026-09-20（V2-1，用户直裁）、2026-09-25（V3W5-E1/D-V3W5-1）；收录 2026-09-29 | [observability](../design/2026-09-22-observability.md)、[B 线 W5](../design/2026-09-25-b-line-w5.md)、[v0.2 规划](../plan/2026-09-20-v0.2-plan.md)（V2-1）、[VL vs OpenObserve](../research/2026-09-25-openobserve-vs-victoria.md) |

## 背景

统一日志检索是易用性刚需（ADR-0008），但观测栈是「多容器重栈」的传统重灾区（fluentd+Loki+Grafana 三件套即红线对象）。

## 决策

1. **VictoriaLogs 默认捆绑**（`logs.backend` 缺省 victorialogs）：hub 脱敏后 ES bulk 直推、**无采集中间容器**；SSE 直读不动（VL 只承接检索面，VL 故障=检索降级、直播照常）；**VL 数据不进 state_backups**。
2. **降级条款**：实测不过 600MB 门则退 `jsonl`+FTS5 兜底（实测 12.7MB，未触发）。
3. **否决 OpenObserve 替换（V3W5-E1）**：实测 idle 340-346MB 对预算硬碰撞；opt-in 面组件数不净减；缺 resolved 语义。维持 Victoria 系。
4. **告警=vmalert 组件捆绑（D-V3W5-1，用户直裁）**：通知复用既有管线；**告警投递零事件**（防回环）。
5. metrics 捆绑 vmagent+vmalert；跨节点 metrics 走 VPC/LAN 直连不依赖 overlay（W3-F2 环境限制）。

## 后果

- VL/vmalert 是受管组件（managed component）：平台部署、卷钉住、配置注入——每个新受管组件都要走「部署+收敛+披露」的完整链条（这是架构评审的深化候选之一）。
- 600MB 门只管辖默认捆绑面；opt-in 组件不占顶（启用态如实记录不设顶）。

## 关联

ADR-0008（元裁决）、ADR-0007（RustFS 不作 VL/VM 主存储）、ADR-0013（host 网络回环约束）。
