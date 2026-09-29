# ADR-0005: HA 边界三层裁决；入口=多 A 记录+连接级重试

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（落地）；管理面 HA 挂账 v0.3+ | 决策 2026-09-20；收录 2026-09-29 | [架构](../design/2026-09-17-architecture.md) §2.6、[multi-node](../design/2026-09-20-multi-node.md) §2.9（D-MN-3/D-MN-4） |

## 背景

「2 台机器指向同一 base_domain，要不要 LB/keepalived？」——答案是分层的，且必须诚实（口径纪律进向导/UI）。

## 决策

1. **三层裁决（§2.9）**：
   - **`ctrl.<base>`（8423 配置端点）**：LB 无用甚至有害——仅 manager 存在，worker 拒连是刻意语义；控制面单点在 fleetlyd 单进程 SQLite 单写。
   - **console/registry/应用域名（每节点 Traefik global）**：数据面无节点单点，缺口只在 DNS 层=**多 A 记录（TTL≤300s）+连接级重试**；**明确排除**健康驱动故障转移/VIP/自建 keepalived+haproxy（VPS 无 L2 同网段+违背轻量裁决）。
   - **有状态服务**：LB 无关，local 卷不跟随节点，恢复走备份重放。
2. **「2 台 ≠ 全面 HA」**：得到无状态进程级 HA（~13s 判定/~19s 重调度）；**得不到管理面 HA——不做 2 manager**（quorum=2 任一失联管理即不可用，2 台正解=1 manager+1 worker+冷备）；得不到有状态 HA。
3. **管理面 HA=3 manager+状态复制/standby，挂账 v0.3+**。
4. **升级路径**：推荐云 NLB 或 DNS 健康检查（Route53/CF）L4 直通 80/443，fleetly 侧零改动。
5. **D-MN-3**：Traefik 配置通道=`https://ctrl.<base>:8423`（8422 留明文挑战面），A 记录仅指 manager、不经 Traefik（穿 443 反代有冷启动循环被否）；证书动态配置内联 PEM；v0.1 证书卷+seed 容器退役（D-MN-4 单轨）。
6. **入口形态**：每节点 Traefik global+集中 ACME+HTTP provider 下发——replicated-1 单入口被否（与 drain 矛盾）；每节点独立 ACME 被否（LE 限额+续期风暴）；关闭自动发现（Swarm provider 不检查健康）。

## 后果

- 「fleetly 是不是 HA」的回答必须是三层的——单一答案即口径违规。
- zot/配置通道为 SPOF（Traefik 冻结最后好配置=设计使然）。

## 关联

ADR-0001（控制面仅 manager）、ADR-0008（轻量裁决否决自建 LB 栈）、ADR-0007（有状态恢复走备份）。
