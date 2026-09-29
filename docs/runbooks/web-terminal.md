# Web 终端运维手册（E7，W5-S6）

设计真源：docs/design/2026-09-22-web-terminal.md（§2 全部 + §4 验收表）。
本文记录运维路径与已知边界，不重复设计裁决。

## 组件与数据流

- `fleetly-exec`（global Swarm 服务，每节点一任务）：持集群 token 出站反拨
  控制面 `ws(s)://<advertise>:<http 端口>/internal/exec-relay`（零入站端
  口、零新增常驻组件——D-W5-3）；挂本节点 `/var/run/docker.sock`（只读
  bind）执行 label 卫兵（无 `fleetly.app` label 的容器一律 403）与 shell
  白名单探测（`/bin/bash`、`/bin/sh`）。
- 控制面 hub：连接表（每节点一连接、node liveness = 连接存在）、一次性
  ticket（60s、绑 token+app+service）、并发限额（per-token 2 / 全局 8）、
  会话时限（空闲 10min / 硬上限 30min，连接侧与 relay 侧双保险）、
  `terminal.opened` / `terminal.closed` 审计+事件（同事务；payload 只带
  元数据，会话内容零出现）。
- 接入路径：Console AppDetail → Terminal 页签；或 REST
  `POST /v1/terminal/tickets` 后自接 `GET /v1/terminal?ticket=...`
  （WS；长效 token 不进 URL）。

## 集群 token（fleetly-exec ↔ 控制面）

- 生成/分发：relay 部署收敛管理器首拍生成 48B crypto/rand，写入 Swarm secret
  `fleetly-exec-token`，sha256 哈希落 meta（`execrelay_cluster_token_hash`
  ——控制面永不持明文）。**用户面轮换不提供**（v0.2 裁决）。
- 运维轮换路径（需要时手动执行，之后收敛循环自动收敛）：
  1. `docker secret rm fleetly-exec-token`（等旧 secret 无人引用；
     必要时先 `terminal.enabled: false` 重启 fleetlyd 移除服务）；
  2. 清 meta 哈希（SQLite：`DELETE FROM meta WHERE key =
     'execrelay_cluster_token_hash'`，fleetlyd 停止态执行）；
  3. 重启 fleetlyd——收敛循环下一拍生成新 token、重建 secret、更新服务
     （relay 任务重启领新证；旧 relay 连接因 token 不符被拒）。
- Swarm 状态丢失（secret 不在但 meta 哈希在）：收敛循环自动重生成换哈希并
  更新服务，无需人工介入（日志锚点：`cluster token secret missing from
  the swarm state (regenerating ...)`）。

## 会话与限额

| 面 | 值 | 出处 |
|---|---|---|
| ticket 有效窗 | 60s、一次性 | 设计 §2.5 |
| 并发上限 | per-token 2 / 全局 8 | 设计 §2.4 |
| 空闲超时 | 10min（无数据帧） | 设计 §2.4 |
| 硬上限 | 30min | 设计 §2.4 |
| shell 白名单 | `/bin/bash`、`/bin/sh`（容器内探测择一） | 设计 §2.3 |

断线原因（超时/上限/服务重启/卫兵拒绝）以服务端 close 帧原文呈现在
Console 状态行与 `terminal.closed` 事件 payload。

## exec 镜像（供应链中间态）

`ghcr.io/fleetlyrun/fleetly-exec:<tag>` 尚未经 CI `exec.yml` 首推，Go 常量
`internal/execrelay.DefaultExecRelayImage` 暂为 tag 引用（豁免清单
deploy/image-pin-allowlist.txt 有据）。**CI 首推后的收紧票**须完成：
① Go 常量钉 digest；② 摘除豁免条目；③ docs/runbooks/image-prepull.md
台账增行。e2e（e2e/terminal.sh）在此之前以 dind 内本地构建同名 tag 承接。

## 功能开关

`terminal.enabled: false`（重启生效）：收敛管理器移除 fleetly-exec 服务；ticket
受理与 WS 接入报 `E_TERMINAL_DISABLED`；system status 组件 `execrelay`
恒绿（无所欠）。secret 与 meta 哈希保留——重新启用复用同一 token 身份。
