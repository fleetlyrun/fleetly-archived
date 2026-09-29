# 项目网运维手册（T 线 OT-1 / IMPL-T15-1）

设计真源：`docs/design/2026-09-26-torchwood-line.md` OT-1 节（Project = 网络
共享作用域；app ∈ 恰一 project，**网络参与 = 显式 opt-in，缺省不参加**）+
`docs/plan/2026-09-26-torchwood-line-impl.md` §4「IMPL-T15-1 方案可行性审查/
实施记录」。本文记录运维路径、真机边界与已知泄漏，不重复设计裁决。

## 是什么

- **项目网** = 平台建的 Swarm overlay `fleetly-project-<projectID>`
  （项目主键全量；带自描述 label `fleetly.managed=true` +
  `fleetly.project-network=<projectID>`）。
- **参与** = app 级显式 opt-in（状态库 `apps.project_network_attached`）。
  参与的 app 其成员服务**双挂**：app 私网（别名 = 短服务名，仅此网可见）+
  项目网（别名 `<app>-<service>`）。同项目、都参与的 app 之间可按
  `<app>-<service>` 互访；**未参与 = 维持既有 app 私网隔离**。
- **生效路径** = 「参与变更重部署」：attach 前置 ensure 项目网 → 参与位落位
  （审计 `app.project_network_attached` / `..._detached` + 事件
  `project.network_changed`）→ 入队重部署（复用最近一次 succeeded 部署的
  compose；无部署史 = 仅落位）。新 revision 的成员服务在网络集合上滚动。
- **回收**：项目网在失去最后一名成员（app 删除/改派/逐出）且**零挂接容器**
  后由平台收敛步（30s 频控）回收；仍被服务引用时保留、下拍重试（底座对
  in-use 移除另有 `FailedPrecondition` 拒绝兜底——真机实证）。
  ⚠️ 实测注记（Docker 29.7.2 本机 swarm）：`network inspect` 的 `Services`
  字段在受管 overlay 上**未填充**（恒 0）——零引用判据实际由 `Containers`
  计数与 daemon 的 in-use 拒绝双层承载；「零容器但服务 spec 仍引用」的窗口
  由 daemon 拒绝兜底，不会拆掉在役网络（移除失败留下拍重试）。

## 操作面

CLI（`fleetly projects network ...`，全部经 RPC；需要项目角色 admin+）：

```sh
fleetly projects network show <app>     # 项目 + 参与状态 + 项目网名（--json 可机读）
fleetly projects network attach <app>   # 显式 opt-in（幂等重跑安全）
fleetly projects network detach <app>   # 摘除（项目内同伴失去 <app>-<service> 可达）
```

Console：App 概览页 **Project network** 卡（状态 + attach/detach；
viewer/developer 只读 + 角色说明；平台管理员只读说明——职责分离）；
项目详情页信息卡显示 overlay 名与参与成员数，应用行带参与徽标。

在途部署存在时 attach/detach 返回 **409**（重部署以最近成功部署为基，不得
覆盖在途发布）——等部署终态后重试。

## ⚠️ 滚动语义（诚实标注，真机实证）

网络集合是 Swarm **task template 的一部分**：attach/detach 生效时成员服务
**必然重建任务**（Docker 29.7.2 / swarm active 实机探针，2026-09-27）：

| 观测 | 结果 |
|---|---|
| `service update --network-add`（缺省 stop-first） | 旧任务 stop → 新任务起（**有停机窗口**） |
| `--network-add` + `start-first`（平台缺省更新序） | 新旧任务**并存滚动窗口**后旧任务下线（不中断） |
| `--network-rm` | 同款任务重建 |
| 仅改 update-order（非模板字段） | **零任务替换**（与 Spike B2 一致） |
| 摘网时若服务仍引用（有卷/stop-first 强制序） | 旧任务先停 → **有停机窗口**（有卷/global 服务按平台既有 stop-first 纪律） |

因此：**无卷 start-first 服务近零中断；有卷/global 服务如实有短暂停机窗口**
（与 MoveApp 换名重部署同口径披露）。

## 跨节点前置：UDP 4789/7946（staging 未放行期的同节点指引）

overlay 数据面跨节点走 **VXLAN（UDP 4789）** 与 **gossip（UDP 7946）**；
端口矩阵见 `docs/design/2026-09-20-multi-node.md` §端口表，真机证据见
`docs/runbooks/vps-dogfooding.md` W3-F2（node2 跨节点 DNS NXDOMAIN/VIP 不可
达/tcpdump 0 包，同路径 TCP 通路 ⇒ VPC/云防火墙滤 UDP）。**未放行前，跨节点
项目网不可用**（节点上的任务拿不到跨节点 DNS/VIP）。

未放行期的运维动作 = **把参与项目网的成员服务钉在同一节点**，走平台既有
放置机制（不新造机制）：

1. 参与者/操作者先确认目标节点：`fleetly nodes ls`（或 Console System 页）
   取节点显示名（hostname，集群内唯一）或平台节点 ID（`n_<ULID>`）。
2. compose 服务声明放置意图 label（v0.2 多节点既定机制，含解析/校验/事件
   `placement.bound`）：

   ```yaml
   services:
     worker:
       labels:
         fleetly.placement.node: "node-1"      # 或 n_<ULID>（ID 恒可解析）
   ```

   有卷 app 由平台自动绑定数据诞生节点（同机制），无需手写。
3. 低层兜底（用户显式约束面，仅 `node.labels.fleetly.*` 命名空间）：
   `deploy.placement.constraints: ["node.labels.fleetly.node-id == n_<ULID>"]`。
4. UDP 放行后：移除同节点钉位并按常规发布（`placement.bound`/pre-mail 语义
   见多节点 runbook），再复验跨节点 `ping <app>-<service>`（见下节探针）。

单节点/本机（Docker Desktop、单机 dind）**不依赖 VXLAN**：项目网数据面在
本机容器网络内即通，功能可全量验证（本票探针即在此形态跑通）。

## 对账与披露（事件词表）

| 事件 | 含义 | 处置 |
|---|---|---|
| `project.network_changed` | 参与位变更（attach/detach） | 通知/审计面事实 |
| `network.missing` | 有成员项目的项目网在底座缺失（外部 `docker network rm` 等） | 披露 + 收敛步幂等重 ensure（自愈）；反复出现查外部脚本 |
| `network.orphaned` | `fleetly-` 前缀受管网无 state 归因 | **只披露不自动删**（无法归因 ⇒ 不静默删他人物件）；人工确认后清理 |

孤儿/缺失持续形态**每进程只报一次**（恢复后清零可再报）。
**已知泄漏类（本票如实披露、不扩面修）**：app 删除路径不回收 app 私网
（`reapDeletingApp` 只摘服务/secret/config；Traefik 亦未摘挂），因此 deleted
app 的 `fleetly-<team>-<prj>-<app>-net` 会被 `network.orphaned` 披露——真实
泄漏可见化即本机制的价值；回收路径（含 Traefik 摘挂）挂账后续票。

## 验收探针（可复跑）

```sh
# 1) CLI 全链（admin 角色凭据）
fleetly projects network show <app>            # 缺省 detached
fleetly projects network attach <app>          # status=rolling（有部署史）
fleetly projects network show <app>            # attached + fleetly-project-...

# 2) 跨 app DNS（同一项目、两个都 attach 的 app；exec 终端内）
ping -c 1 <peer-app>-<peer-service>            # 通
nslookup <peer-service>                        # 短名必须解析不到（别名隔离）

# 3) 底座对象与归属 label
docker network inspect fleetly-project-<projectID> \
  --format '{{.Labels}} {{len .Containers}} {{len .Services}}'
docker network ls --filter label=fleetly.managed=true
```

本票单节点真机探针脚本（Docker 29 环境，默认不跑）：

```sh
FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate \
  -run TestManualProjectNetwork -v
```

实测原始观测（2026-09-27，Docker 29.7.2 / swarm active / 单节点）：

```
project network ensured: fleetly-project-01JMANUALPROJECTNET0000000
  labels=map[fleetly.managed:true fleetly.project-network:01JMANUALPROJECTNET0000000] driver=overlay
app2 -> app1-web: exit=0（10.0.1.2，跨 app 项目网别名通）
app2 -> only1（app1 私网短别名）: exit=1 "ping: bad address 'only1'"（短名不跨网）
network add: task mz0x… -> rfax…（旧任务 running/desired=shutdown——start-first 并存滚动）
network remove: task rfax… -> ijuy…（再次滚动）
order-only update: running/desired=running（零任务替换）
in-use removal rejected: FailedPrecondition network … is in use by service …
zero endpoints: containers=0（services 字段恒 0——见上注记）→ 移除成功
```

## 已知边界

- 项目网**不提供项目内 ACL**（swarm 无 NetworkPolicy——设计明确不承诺）；
  attach 即「同项目参与者可达」的全量授予。
- **回滚 = 该 revision 期望态的点时重放**（网络面随快照，与 env 快照同口径）：
  回滚到一个「当时已 attach」的 revision 会把项目网重新挂上（即使当前参与位
  已 detach）——参与位本身不变，下一次发布收敛回当前参与位。漂移基线随回滚
  记录（无假阳性）。把参与位变更当发布事实看即可（与 env 回滚同心智）。
- 参与位是**跨项目网名的唯一来源**：MoveApp 改派后参与位保留、投影随新项目
  （新项目网），旧项目网在零成员零引用后回收。
- **别名段内歧义（已知类，挂账 admission 预检）**：项目网别名 = `<app>-<service>`
  为跨两层拼接——同项目内 app「a」+ 服务「b-c」与 app「a-b」+ 服务「c」会产生
  同一别名 `a-b-c`（两个都 attach 时 DNS 同时返回两地址，静默混流）。与
  rbac-teams §4.3 记载的服务名段内歧义同族（罕见命名组合）；当前靠命名纪律
  规避，预检挂账后续票。
- 项目网名 = 项目 ID 全量（非前 8 截断）：ULID 前 8 只承载 40 位时间戳成分，
  256ms 窗口内创建的项目会撞名并静默并网——命名审计记录见实施方案 §4。
