# ADR-0015: Compose 受控子集契约

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1 落地；secrets 预留口 2026-09-20 解除） | 决策 2026-09-17；收录 2026-09-29 | [架构评审整改](../plan/2026-09-28-architecture-remediation.md)（C3）、`internal/compose`（validate/normalize/golden） |

## 背景

compose 是平台输入语言：全量支持=把自己变成 docker compose 的镜像兼容负担；任意子集=语义不可控。

## 决策

1. **受控子集**：validate/normalize 是唯一解析面；白名单/拒绝清单**只增不减**（golden 文件钉住，compose whitelist.golden）。
2. **受管字段拒绝、不静默覆盖**：用户 compose 里出现平台受管字段（labels/networks 等由 naming/ingress 管辖）→ `E_COMPOSE_MANAGED_FIELD` 显式报错，不悄悄改写。
3. **归一化快照是部署基线**：revision 存归一化 compose+覆盖层；快照 `services` 是**数组形态**（name 在元素上）——消费方一律走 `extractServiceNames` 单点解析，不按 map 取（Logs 页下拉曾因此显示「0/1」下标）。
4. **预留口纪律**：secrets 曾显式拒绝（评审 C1），v0.2 密钥库接入后按预留口显式解除——拒绝清单是「暂缓+预留」不是「永久否决」，解除必须走新能力面。
5. 受控子集只收 `deploy.resources.limits` 等**声明的受控字段**；compose 的其余 deploy 语义（placement 约束等）走平台 placement 域，不透传。

## 后果

- 「再加一个 compose 字段」=白名单+校验+归一化+golden 四处联动+受管字段冲突审查——减速带是刻意的。
- 用户可移植性口径：fleetly 受控子集内的 compose 可双向迁移；子集外字段不承诺。

## 关联

ADR-0009（受管 label 同族契约）、ADR-0011（golden 联动 idiom）、ADR-0003（快照=回滚基线）。
