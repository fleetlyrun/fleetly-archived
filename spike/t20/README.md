# spike/t20 — IMPL-T2-0 前置 Spike ×4 探针（Docker 29 / swarm）

本目录是 [IMPL-T2-0 报告](../../docs/reports/2026-09-26-t2-spikes.md) 的可复跑探针与原始产物。
每条 spike 一个宿主侧编排脚本 + 必要的 dind 内脚本；产物落 `artifacts/<run-id>/`（随报告入库）。

环境基线（本轮）：宿主 Docker Desktop 29.7.2（Windows/WSL2，swarm active）；
dind 镜像 `docker:29.8.1-dind`（钉 digest，与 CI 门禁一致）；全部脚本在 Git Bash 下运行。

## 目录

| 路径 | 内容 |
| --- | --- |
| `scripts/lib.sh` | 公共库：dind 起停 / exec+stdin 暂存 / 日志与断言辅助 |
| `scripts/in-overlay-probe.sh` | 容器内探针（路由表 / 对等 DNS+ICMP+TCP / 公网 DNS+TCP+HTTP）|
| `scripts/in-cx-service-probe.sh` | 服务任务探针（`tasks.<svc>` 跨节点 DNS + 逐地址 ICMP）|
| `scripts/s1-host-overlay-matrix.sh` | Spike ① 单节点矩阵（bridge/overlay × internal/非 internal）|
| `scripts/s1-cross-node-overlay.sh` | Spike ① 跨节点腿（双 dind swarm；attachable 容器 + global 服务两层）|
| `scripts/s2-resolve-digests.sh` | Spike ② 解析腿（生产 resolver 真机解析 + 本地零预拉部署复核）|
| `scripts/s2-digest-auth-dind.sh` | Spike ② 跨节点+凭证腿（双 dind + registry:2/htpasswd）|
| `scripts/in-lifecycle-latency.sh` | Spike ③ 内层计时（swarm service 周期 + docker run/stop 基线）|
| `scripts/s3-lifecycle-latency.sh` | Spike ③ 宿主编排（单 dind）|
| `scripts/s3-stats.sh` | Spike ③ 分布统计（可对既有 run 目录独立复跑）|
| `scripts/s4-ordering-dind.sh` | Spike ④ 宿主编排（单 dind + fleetlyd 全链）|
| `scripts/in-s4-ordering.sh` | Spike ④ 内层观测（依赖链 / 永久失败 / depends_on 口径）|
| `cmd/t20resolve` | 解析探针（生产 `internal/imageregistry`；支持 Basic 凭证与 X-Registry-Auth 编码）|
| `fixtures/ord{1,2,3}-*.yaml` | Spike ④ compose fixture（依赖链 / 永久失败 / depends_on 负路径）|

## 复跑（Git Bash，仓库根目录）

```sh
# ① 单节点矩阵（~1 分钟；宿主 swarm 临时建网络/容器，脚本自清）
sh spike/t20/scripts/s1-host-overlay-matrix.sh

# ① 跨节点（~3 分钟；自建双 dind swarm，脚本自清）
sh spike/t20/scripts/s1-cross-node-overlay.sh

# ② 解析腿（需出网；~5 分钟，含一次公共镜像真拉）
sh spike/t20/scripts/s2-resolve-digests.sh

# ② 跨节点+凭证腿（~8 分钟；需外网拉 httpd 镜像与 alpine:3.20 基座）
sh spike/t20/scripts/s2-digest-auth-dind.sh

# ③ 生命周期时延（~11 分钟；N 默认 30，可用参数覆盖）
sh spike/t20/scripts/s3-lifecycle-latency.sh 30
sh spike/t20/scripts/s3-stats.sh spike/t20/artifacts/<run-id>   # 统计可独立复跑

# ④ 编排顺序语义（~4 分钟；宿主交叉编译 fleetlyd/fleetly）
sh spike/t20/scripts/s4-ordering-dind.sh
```

`sh -n` 全绿；脚本内不出现「stderr 丢弃到 /dev/null」写法。

## 已知环境事实（影响证据形态）

- 宿主 Git Bash 环境带 `HTTP_PROXY/HTTPS_PROXY`（本机事实）——宿主侧对私网
  registry 的 curl/Go 探针必须绕过（`--noproxy '*'` / 回环发布端口）。
- Windows/WSL2 宿主**不能**直达容器 bridge IP（实测 `curl 10.x` 返回 000）；
  需宿主访问的端口一律发布到 `127.0.0.1`。dind 之间互访走 bridge IP，正常。
- dind（29.8.1）为 containerd image store：swarm agent 拉取的镜像**不注册进
  docker image store**（`docker image ls` 不可见）；逐节点拉取证据以 dockerd
  `image pulled` 日志 + registry 访问日志 + `docker image inspect <digest 引用>`
  为准（本轮实测 digest 引用 inspect 可见）。
- 宿主 kill 不终止 dind 内进程（spike/a 已知问题#11 复现于 ③ 首轮）：重跑前
  必须重建 dind（本目录脚本的 `t20_teardown` 会做）。

## 产物索引

`artifacts/` 下每次运行的 `*-summary.txt` / `*-report.txt` 是人读汇总；其余
`*.log` 为原始证据。报告 `docs/reports/2026-09-26-t2-spikes.md` 的每节注明所引
run-id。
