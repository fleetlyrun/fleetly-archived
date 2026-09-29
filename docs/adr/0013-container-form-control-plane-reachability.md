# ADR-0013: 容器形态三适配；控制面地址物化；`grpc://` scheme

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（staging 落库） | 决策/真机 2026-09-29；收录 2026-09-29 | [observability](../design/2026-09-22-observability.md)（D-W5-4）、[web-terminal](../design/2026-09-22-web-terminal.md)、git 0da7e82/2053e1b/f67845b/b4983dc |

## 背景

fleetlyd 以容器形态运行（与原生 systemd 双轨并存）后，host 网络与 overlay 的结构性约束集中暴露；任务回连控制面（torchwood dispatcher→Tasks API）需要地址与 TLS 语义。

## 决策

1. **容器形态三适配（缺一即坏）**：
   - `--network host` 必选：host 容器不可挂 overlay、swarm DNS 不服务宿主（D-W5-4）；VL/VM 消费面=daemon 拨宿主回环 127.0.0.1，桥接容器里 127.0.0.1 是自己；
   - 数据根必须 host bind（`/var/lib/fleetly`）：平台把该目录下文件 bind 挂载进 swarm 任务，任务在宿主解析路径——命名卷里文件宿主路径不存在→任务 Rejected；
   - 镜像必须带 `docker-cli`（buildkit 构建链要 exec docker；无内嵌 daemon）。
2. **D-W5-4 本体（结构解法族）**：受管服务宿主可达=host 网络任务+进程级回环监听——`PortConfig.HostIP=127.0.0.1` 会被 Engine API 静默丢弃；同族先例：rustfs 探针、exec 反向常连。
3. **控制面回连=引擎物化地址**：每个任务 spec 无条件物化 `FLEETLY_CONTROL_GRPC_ADDR=<advertise>:<gRPC端口>`+`FLEETLY_CONTROL_TLS_NAME`（advertise=swarm NodeAddr=VPC 内网流量不出集群）；dispatcher 端点空时回落。**为什么没有 DNS 服务名**：host 网络容器禁入 overlay、swarm 无 k8s Service 抽象、DO VPC 无内部 DNS——物化 env 是结构性解，exec relay 的 FLEETLY_CONTROL_ADDR 是同源先例。
4. **endpoint scheme=gRPC 社区约定**：`grpc://` 明文 / `grpcs://` TLS+系统 CA / `?insecure=true` 跳过校验 / `?server_name=` 覆盖 SNI；裸 host:port 明文兼容。**socat 无 ALPN 不可代 TLS gRPC**（须 HAProxy `alpn h2` 或客户端原生 TLS）——staging grpcbridge 是过渡兼容件非终态。
5. **console 进镜像（用户裁决）**：内置 `/opt/fleetly/console` 回落启用；`console.static_dir` 显式配置恒优先、宿主 bind 可遮蔽。
6. **代理形态每一跳都要 TLS 断言**（web-terminal 真机教训：gateway 回拨凭据跟随 TLS 形态）。

## 后果

- 升级换镜像零迁移（状态全在 bind），但镜像化更新仍有缺口（发布镜像不带 docker-cli、CI 仅 release tag 发镜像）——挂账票。
- 原生形态（systemd/二进制）与容器形态长期双轨并存。

## 关联

ADR-0001（Swarm 约束的源头）、ADR-0006（VL 回环消费面）、torchwood 线（dispatcher 回连需求）。
