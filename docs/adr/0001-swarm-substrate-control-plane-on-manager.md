# ADR-0001: Docker Swarm 为 substrate，控制面仅 manager、worker 零安装物

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1 落地） | 决策 2026-09-17（D12 修订 2026-09-20）；收录 2026-09-29 | [架构](../design/2026-09-17-architecture.md)（D2）、[multi-node](../design/2026-09-20-multi-node.md)（D12/D-MN-2/D-MN-3）、[swarm 评估](../research/2026-09-17-swarm-substrate-assessment.md) |

## 背景

PaaS 底座选型：K8s 过重（与轻量元裁决冲突），裸 Docker 单机（无调度/成员/滚动更新）。Swarm 已内置于 Docker daemon，但自研 node 协议曾是备选（原 D2）。

## 决策

1. **Docker Swarm 是唯一 substrate**：成员管理、心跳、调度、滚动更新全部委托 Swarm；moby 类型止步于 `internal/substrate`，engine 只见 `ServiceSpec/ServiceState` 投影。
2. **D12 推翻原 D2**：不自研 node 协议——Swarm 已提供等价物，自研=重复造轮子且必然更差。
3. **控制面仅 manager（D-MN-2）**：`fleetlyd` 只在 manager 运行（8423 配置端点仅 manager 存在，worker 拒连——刻意直连语义）；**worker 零 fleetly 安装物**。
4. **worker 跑 fleetlyd 副本被否决**：多写点红线（SQLite 单写者权威态，见 ADR-0002）。
5. **退出预案 = k3s driver**（架构 §3.1）：V1-V7 为采纳门，已转 nightly 永久回归；这是保险丝不是路线图。

## 后果

- 控制面单点：故障爆炸半径=不能部署/管理，**不影响运行中应用**——这是 HA 边界三层裁决的输入（ADR-0005）。
- 环境限制如实暴露而非掩盖：云防火墙滤 VXLAN（UDP 4789/7946）时跨节点 overlay 数据面不可用，跨节点 metrics 已改 VPC/LAN 直连、placement 未放行前钉同节点（W3-F2）。
- 受 Swam 语义约束的已知坑集中记录于 Docker29 多节点备忘（digest-pull、nft 杀 DNAT、job 约束须 label 公式等）。

## 关联

ADR-0002（单写者）、ADR-0005（HA 边界）、ADR-0013（容器形态）。
