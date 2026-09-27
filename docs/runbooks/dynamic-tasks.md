# 动态任务（Tasks API）运维手册

| 状态 | 日期 | 关联 |
|---|---|---|
| 当前 | 2026-09-27 | [T 线设计 DT-5/DT-7](../design/2026-09-26-torchwood-line.md)；[实施方案 IMPL-T2-1](../plan/2026-09-26-torchwood-line-impl.md)（审查 + 实施记录）；[T2 spike 报告](../reports/2026-09-26-t2-spikes.md)（DT-7 internal 出网、TTL 参数建议） |

## 1. 语义（平台保证的与不保证的）

- **任务 = swarm service 承载**（`fleetly-task-<taskID>`，taskID 是任务平台 ID 全量
  ULID）：稳定 DNS 名 = 服务名，在作用域网内按名寻址，调用方无需 inspect。
- **平台管**：API/校验、网络作用域、每令牌配额、加固、审计与事件、TTL 回收、
  孤儿对账。**平台不管**：租约/保温/熔断/常驻池（调用方语义）、健康检查
  （任务没有 healthcheck，就绪由调用方轮询）、顺序编排（并行创建）。
- **安全默认（服务端强制、不可关闭）**：`cap_drop ALL`、只读 rootfs、非 root
  user（`65534:65534`）、pids 512、`restart-condition none`（容器崩溃上抛为
  `task.failed`，平台**不**静默自愈）、显式 `stop_grace_period 5s`。tasks.v1 的
  请求面没有对应字段。
- **网络 = 作用域引用（三作用域）**：
  | kind | ref | 加入的网络 | internal |
  |---|---|---|---|
  | `app` | app 名/限定形 | `fleetly-<team>-<prj>-<app>-net` | 拒绝 |
  | `project` | 项目 ID 或 `team/prj` | `fleetly-project-<projectID>` | 拒绝 |
  | `task-group` | `[a-z0-9]{2,32}`（不含 `-`） | `fleetly-taskgroup-<ref>`（长活） | 支持（DT-7 不可信隔离面） |

  任务**只按名加入既有网**（网络缺失 = `E_TASK_UNSUPPORTED`，不代建）；请求面
  **不存在**任何 attach 入参（per-task 动态 attach 在类型层不可表示——
  2026-09-14 attach 残留事故的事故类）。
- **TTL**：缺省 600s，下限 60s、上限 24h。到期由引擎每拍回收：`task.expired`
  事件 → 底座服务移除 → `task.stopped`（`stop_reason=expired`）。回收时延 ≈
  `stop_grace_period` + ~1.7s 控制面（T2-0③ 基线）。
- **配额**（每令牌；`task_quotas` 行覆盖，缺行 = 平台默认）：并发 16 / CPU
  8000m / 内存 8GiB；单片上限 CPU 4000m / 内存 4GiB。超限 = 429
  `E_TASK_QUOTA_EXCEEDED`（fail-closed，不排队）。
- **镜像**：tag 经平台 registry 解析腿钉定为 digest（平台凭证随 spec 下发、
  airgap 本机回落）；digest 引用直通。解析失败 = `E_IMAGE_PULL_FAILED`
  （不落行）。**任务日志**只入 VictoriaLogs（`task` 流标签；`fleetly tasks logs
  <id>`）；`logs.backend=jsonl` 形态无任务检索面（诚实报
  `E_LOGS_BACKEND_UNAVAILABLE`）。

## 2. 操作

```sh
# task-group 网络（长活；internal = 不可信函数隔离面）+ 控制面一次性挂靠
#（成员经其下一次发布双挂该网络；无部署史 = pending）
fleetly tasks network ensure --internal --member torchwood=server --member torchwood=dispatcher tenant1

# 受理任务（镜像 tag 服务端钉 digest；env 密文落库）
fleetly tasks run --image ghcr.io/acme/fn:1.2 --scope-kind task-group --scope-ref tenant1 \
  --name fn-abc --ttl 10m --cpu-millis 500 --memory-mb 256 --env MODE=prod

fleetly tasks ls [--all] [--json]     # 缺省只出在途；--all 含终态台账
fleetly tasks stop <id>               # 幂等：stopping → 引擎移除服务 → stopped
fleetly tasks rm <id>                 # 停止 + 移除底座服务 + 删除台账行
fleetly tasks logs <id> [--keyword K] # VL 检索（task 标签；需 backend=victorialogs）
```

凭据：整体 `tasks` 独立 scope（`fleetly tokens create --scopes tasks ...`；
read/deploy 不蕴含、admin 蕴含）。跨令牌隔离：他令牌的任务 Get/Stop/Delete 恒
404（不可见即不存在）。

## 3. 回收与对账

- TTL 到期：引擎每拍（2s）扫描，回收 + `task.expired`。
- 容器任务 failed/rejected：`task.failed`（错误摘要单行有界）；`complete`：
  `task.stopped`（reason=exited）。底座服务被外部移除：收敛拍幂等重建。
- 孤儿 task（底座带任务 label 的服务在 state 无对应非终态行）：
  `task.orphaned` 事件 + 审计 + 服务回收（归属明确的本平台对象）；持续形态按
  服务名节流。读错不结论（瞬态）。
- 终态台账行保留 30 天后由 state janitor 回收（事件/审计独立留存）。

## 4. 已知边界（如实标注）

- **task-group 网不可 attachable**：容器侧无法直接挂入（daemon 拒绝
  `not manually attachable`），任务只能作为 swarm service 按名加入——这是安全
  姿态，不是缺陷（本机真机实测）。
- **internal 变体**：无出网、无外部 DNS（应用需解析公网域名会失败，属预期）；
  跨节点数据面依赖 VPC UDP 4789/7946 放行（同项目网前置）。
- **控制面挂靠生效**依赖成员 app 的下一次发布（在途部署 409 拒绝）；成员服务
  的加固/其他字段不受影响（发布管线全量重投影）。
- **任务日志无 Follow/历史落盘面**：只有 VL 检索（任务无 app 归属，jsonl 落盘
  面按 app 分文件不适用）。
- 平台 `network inspect` 会把 attachment 的 Target 归一为网络 ID、`Driver`
  偶发为空（daemon 版本行为）——任务收敛判据用 desired-hash label，不依赖该
  字段回比。

## 5. 真机探针（可复跑）

```sh
# 本机 swarm（Docker 29+）：internal 网 + 加固回读 + 出网封死
FLEETLY_MANUAL_SWARM=1 go test -tags manual ./internal/substrate -run TestManualTaskHardening -v
```

staging 待执行项（本环境无 staging 访问权）：多节点任务 DNS 可达、TTL 回收
端到端时延、任务面 registry 解析腿跨节点复跑（见实施方案 §4 实施记录的
「staging/真机待执行项」）。
