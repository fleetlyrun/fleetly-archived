# VPS dogfooding runbook（T1-V2.1~T1-V2.3 实录，2026-09-20）

| 状态 | 日期 | 关联 |
|---|---|---|
| 进行中（核心链路全通；V5 演练与 Playwright 待做） | 2026-09-20 | [v0.2 规划 W1](../plan/2026-09-20-v0.2-plan.md)；[交付流水线 §2.6](../design/2026-09-17-delivery-pipeline.md)；基线产物 v0.1.0-rc1（公开 release） |

staging：`root@fleetly-dev.deeploop.net`（146.190.58.0，Debian 13 / 2C / 4G / 79G）；域名 `dev.fleetly.run` + 通配符 `*.dev.fleetly.run`（DNSPod，CNAME 到主机名）。

## 1. 环境准备（顺序敏感！）

```bash
# ① Docker 引擎（Debian 13）
curl -fsSL https://get.docker.com | sh
# ② 切 iptables legacy —— 必须在 dockerd 首启前或切换后重启 docker（见 §4-F3）
update-alternatives --set iptables /usr/sbin/iptables-legacy
systemctl restart docker          # 若 ① 时 daemon 已起
# ③ 环境体检（含 nft 双栈残留检测）
sh verify-vps.sh dev.fleetly.run
```

## 2. 安装与配置（公开 release 全真路径）

```bash
curl -fsSL -o /root/install.sh \
  https://github.com/fleetlyrun/fleetly/releases/download/v0.1.0-rc1/install.sh
sh /root/install.sh --version v0.1.0-rc1
```

安装报告全绿（引擎门禁/checksum/swarm init 私网 advertise/systemd/healthz）；bootstrap token 在 journalctl（rc1 形态，见 F1）。

配置增补（`/opt/fleetly/etc/config.yaml`）：`console.static_dir=/opt/fleetly/console`（main 构建 dist 经 scp 上传）；`acme.email`（`ca_dir_url` 缺省即 LE 生产目录，零 Pebble 痕迹）。

**平台镜像预拉**（v0.1 不代拉；W0 预拉台账的 digest 形态须补 tag——见 F4）：

```bash
docker pull alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
docker pull traefik:v3.5@sha256:16acb89c6db341182970d6fdafece31303b0a380a8ed7aa51682e225229bf1d2
docker tag alpine@sha256:d9e85… alpine:3.20        # 运行时 spec 按 tag 引用
docker tag traefik@sha256:16acb… traefik:v3.5
docker pull traefik/whoami:v1.10                    # demo 应用镜像
```

## 3. 已验证链路（全部真路径）

| 链路 | 结果 |
|---|---|
| 公开 release 安装（下载+checksum+门禁+systemd） | ✅（签名轨见 F1） |
| REST /v1（外网 Bearer）+ /ui/（console 托管） | ✅ http://146.190.58.0:8420 |
| CLI deploy（hello-web，域名变更 ×4 次部署） | ✅ running/succeeded/revision 链 |
| **git push 部署**（ssh://git@host:8424/<app>.git，TOFU+指纹审计+push 即 queue）〔2026-09-29 注记：该通道已随 ADR-0012 裁决移除——本行为历史实测记录，现役部署通道 = webhook 签名投递+拉源 / CLI compose 上传〕 | ✅ ×2（初推+空提交重触发） |
| rollback（快照重放，--to 指定 revision） | ✅（服务端异步完成，见 F6） |
| **LE 生产证书**（HTTP-01 经 Traefik 反代→集中签发→台账） | ✅ hello.dev + demo.dev 双证，issuer=Let's Encrypt CN=YE2，有效期至 2026-12-19 |
| HTTPS 服务 | ✅ 双域名 200（whoami 实答） |
| 平台热备 | ✅ verified snapshot 自动落账（daily） |

Console 入口：`http://dev.fleetly.run:8420/ui/`（8420 为明文 HTTP——平台自身 TLS 属 E1 平台子域范畴）。

## 4. 发现与挂账（F 编号，v0.1.x/v0.2 候选）

| # | 发现 | 处置建议 |
|---|---|---|
| F1 | **rc1 版本时间差**（tag 于 S13-S20 落地前）：安装器只有 cosign 轨（无 cosign 即 degraded skip，S14 openssl 双轨在 v0.1.0）；bootstrap token 仅 journal（B5 文件落盘在后）；空视图拒绝下发（S13 noop 兜底在后，首个应用部署前 sweep WARN） | v0.1.0 自然消除；无需代码动作 |
| F2 | **收敛/证书重试与续期扫描共用 12h 周期**：首装缺镜像/ACME 一次失败后，自然重试窗口过长（实测靠 redeploy 人为触发才签成） | v0.1.x 候选：收敛失败与 cert 失败用短退避（如 1min 起指数），续期扫描保持 12h |
| F3 | **iptables 后端切换时机**：daemon 以 nft 首启后再切 legacy，残留 nft 表挂内核钩子——宿主端口通、**DNAT 发布端口（80/443）全死**，云防火墙排查方向会被带偏（实测） | verify-vps.sh 已加检测；install.sh 可在 A8 门禁中加同款提示（v0.1.x 候选） |
| F4 | **digest 形态 pull 不落 tag**：运行时 spec 按 tag 引用镜像，digest pull 后须显式 `docker tag` | 预拉台账（image-prepull.md）已隐含；runbook 本节显式化 |
| F5 | CLI flag 顺序纪律（FZ-11）在真实使用中即踩（flag 必须先于位置参数） | 既有纪律，交互提示可随 v0.2 CLI 打磨 |
| F6 | CLI 等待期被杀（Ctrl-C/会话断开）服务端照常完成——异步语义正确，但操作者感知与实际状态可能不一致 | 观察项；Console/事件流已可核对状态 |
| F7 | **degraded 无周期自愈**：`refreshDerivedState` 仅部署路径触发（observing/recovery/releasing），swarm 混乱窗降级后要等下一次部署动作才翻回 running（实测 20 分钟不自愈，经重部署恢复） | v0.1.x/v0.2 候选：周期性派生刷新（可挂 drift tick 或复用 W0 substrate recon 模式扩展 degraded→running 方向） |

## 5. V5 真路径演练(T1-V2.4,2026-09-20 实录)

程序依据 Spike C §5(停止态冷备 → 毁 → 回填 → 自举/force-new-cluster);单 manager 真 VPS 首跑:

| 步骤 | 实录 | 结果 |
|---|---|---|
| 灾前 | `fleetly backups create`(manual verified)+ 停 fleetlyd/docker + 停止态 `cp -a /var/lib/docker/swarm`(sha256 记档) | ✅ 冷备 = raft/wal-v3-encrypted + certificates + state.json(Docker 29 布局) |
| 灾难+恢复① | `rm -rf swarm` → 回填 → start docker | ✅ Swarm active、**NodeID 与死前一致**、三服务 1/1、80/443 回监听 |
| force-new-cluster | 无 `--advertise-addr` 失败(eth0 双地址歧义);失败尝试把 manager 留在半死态(Is Manager=true 但 RPC 死);**二次冷备恢复收拾**;再从健康态带 `--advertise-addr 10.48.0.6` 执行 | ✅ rc=0、同 NodeID、**join token 轮换**(SWMTKN-1-2n92…≠旧);服务全回 |
| 控制面恢复 | start fleetlyd → 恢复分类 | ✅ healthz 200、应用行完好、台账完好 |
| 应用观测 | 混乱窗容器重启 → `deployment.warning → app.instability_detected → app.degraded`(如实);**degraded 无周期自愈**(见 F7),经 CLI 重部署 + git push 各恢复一应用,`app.recovered` 落事件 | ✅ 双应用 running |
| 外部验证 | hello/demo HTTPS 200(外部;VPS 侧 hairpin 探测 000 属本机 NAT 怪癖,外部不受影响) | ✅ |
| 计时 | 停机→恢复健康全验证 ≈ 4 分钟(预算 10 分钟,L1 口径) | ✅ PASS |

**runbook 固化(真 VPS 教训)**:①冷备必须停止态做;②单 manager 回填即自举,force-new-cluster 仅在需要轮换 join token 时执行,**必须带 --advertise-addr 且从健康态执行**(半死态执行先二次冷备恢复);③恢复后 join token 已变,加节点用新 token;④degraded 状态需部署动作驱动恢复(F7)。

## 7. W2 多节点实机演练(E1-9,2026-09-21 实录)

拓扑:manager = 本机(升级 main 构建 + `base_domain: dev.fleetly.run`);worker = fleetly-node2.deeploop.net(143.198.234.68 / VPC 10.124.0.5)。**组网实证**:两台共享 VPC 是 **eth1 的 10.124.0.0/20**(ARP REACHABLE);eth0 的 10.48.0.x 是两个 VPC 的同网段假象(ARP FAILED)——manager advertise 经 `swarm init --force-new-cluster --advertise-addr 10.124.0.3` 切换(V5 路径复用)。

| 断言/能力 | 实录 | 结果 |
|---|---|---|
| 平台证书 duty | LE 生产,`_fleetly-platform` 多 SAN(ctrl/registry/console) issuer=CN=YE2;8423 TLS 面服务 | ✅ |
| zot 部署器 | fleetly-registry 1/1(v2.1.21 钉版);registry.dev 443 → 401 Basic Auth 挑战 | ✅ |
| join 向导 | `nodes join-guide` 完整输出(join 命令/防火墙矩阵/DNS 步骤);node2 join 成功 | ✅ |
| 锚定 duty | node2 自动铸造平台 ID `n_01M30WZY…` + `node.joined` 事件 | ✅ |
| **auto-rotate(D-MN-1)** | 日志 `worker join token auto-rotated after new node anchoring, minted:1` | ✅ 真机首跑 |
| 断言 A(拓扑) | 双节点 Ready、节点观测缓存双行 | ✅ |
| 断言 C(有状态 drain) | drain→任务受阻(Pending);回岗→**自动回绑 node2**;**marker 数据完好**(真卷带部署后缀 `statedata-01M30X6G`) | ✅(事件面见 F11) |
| 断言 B(无状态 drain) | **PASS**:node2 连续 87 探测 × drain 窗口全 200(零新连接失败);诚实注记:断言 C 先行 drain 已把副本迁至 manager(无自动回迁,by design),B 验证入口面;「drain 下任务迁移」半面由 dind B2/B3 覆盖 | ✅ |

**预算实测(D-MN-12 口径,2026-09-21)**:manager 全栈 idle **≈424MB < 600MB** ✓(fleetlyd 64 + dockerd 202 + containerd 64 + Traefik 29 + zot 51 + buildkit 14);worker 侧单列 ≈201MB(dockerd 120 + containerd 65 + Traefik 16,零 fleetly 组件)。

### W2 真机发现(F8-F11)

| # | 发现 | 处置 |
|---|---|---|
| F8 | 平台证书签发落在启动发布之后,websecure/内联证书要等 12h sweep 才进视图(冷启动后平台子域 443 长期缺席) | **已修**(a70625c:证书就绪同拍触发重发布,指纹级断言) |
| F9 | worker Traefik 配置饥饿:动态配置含全平台 TLS 私钥,公网 8423 暴露应压到零 | **已修(修订二,bf9bf67)**:provider endpoint 直用 advertise VPC IP(`https://10.124.0.3:8423`)+ insecureSkipVerify(传输 TLS+token 保,服务器认证由 VPC 边界承担);extra_hosts 通道被真机证伪(**Docker 29.8.1 swarm 任务不应用 ContainerSpec.Hosts**,普通 --add-host 正常)——内部 CA + tls.ca 硬化挂 v0.2.x |
| F10 | **publish 无证书段形态会擦掉全平台 TLS**:无域名应用部署/签发竞态后下一拍,全量视图换入把既有 websecure+证书整体擦出(hello/demo 实测被擦) | **已修**(8c3a086:publish 统一带证书段;回归测试钉住) |
| F11 | settled 应用的 drain 无 `placement.blocked/recovered` 事件(发射点仅在 releasing 窗)——部署窗内 drain 有事件(dind C3/C6 证),settled 后只有任务层 Pending | v0.2.x 跟进票:周期性绑定节点可用性守护(事件流诚实面补齐) |
| 观察 | 残卷:node2 上存在无后缀 `statedata` 空卷(真卷带部署后缀)——卷命名/清理的巡检项 | 随 F11 票或孤儿卷清理指引核对 |

## 8. W3 对象存储+Cron 实机演练(2026-09-21 实录)

形态:manager 升级 main 构建(c0dddc5+bdd7665 前身,scp 二进制+console dist 替换重启);预拉钉版镜像(rustfs 1.0.0/restic 0.19.1,digest+tag 双记)。演练应用 w3rehearse2(web 有卷钉 manager + `fleetly.s3=true`;task `fleetly.cron: "* * * * *"`)。

| 断言/能力 | 实录 | 结果 |
|---|---|---|
| rustfs 启用→duty 收敛 | `s3 set --mode rustfs` 后 20s 服务 1/1;`s3.rustfs_deployed` 事件 | ✅ |
| 平台探针(rustfs 面) | `s3 test` 四步 init/backup/snapshots/forget 全绿(修 W3-F1 后二次探针亦绿) | ✅(修后) |
| 备份上传轨 | manual 备份 `upload=ok`(restic 仓库读回校验);二次上传幂等(W3-F1b 修后);`backup.upload_failed`→`backup.upload_recovered` 事件链真机闭环(见 W3-F3) | ✅ |
| 凭证注入+网络牵线 | web 容器 6 键 `S3_*` env 全注(endpoint/bucket/双键/path-style 值正确);`nslookup rustfs`→VIP 10.0.6.2;`wget`→HTTP 403(RustFS 应答匿名拒) | ✅ |
| cron 到点触发 | 每分钟火,`cron_runs` succeeded 行成串;job 服务完成即删(零残留) | ✅ |
| cron 日志入管线 | `logs history --service task` 连续 marker 行([task/out] 归属) | ✅ |
| 手动触发 | `cron trigger` 同路径行 started→succeeded + 审计 | ✅ |
| 公网子域开关 | 开:`s3.dev.fleetly.run` 443→403(TLS 真 LE,平台证书重签**四 SAN** 含 s3.dev);关:28s 路由摘除+证书重签回缩**三 SAN**+traefik 摘网(ID 形态断言) | ✅(真 LE 双向) |
| 诚实标注 | `s3 status` 常驻「便捷层非灾备」文案+部署态行 | ✅ |
| 禁用留卷 | `s3 set --mode unset`→服务移除+`fleetly-rustfs-data` 卷保留+`s3.rustfs_removed` 事件;此后备份 `upload=none`(合法停摆不红) | ✅ |
| **预算复测(rustfs 启用态)** | docker 重启取干净基线:全栈 idle **≈429MB < 600MB** ✓——fleetlyd 64.4 + dockerd 129.0 + containerd 66.5 + traefik 21.0 + zot 53.4 + **rustfs 75.5** + buildkit 15.4(dockerd 重启前 churn 漂移至 298MB,属 job churn 累积非 rustfs 归因) | ✅ |

### W3 真机发现(W3-F1~F3)

| # | 发现 | 处置 |
|---|---|---|
| W3-F1 | restic init 不幂等的两处同族:第二次探针(rustfs.RunProbe init 步)与第二次上传(statebackup 惰性 init)撞「repository master key and config already initialized」——restic 0.19.1 第二种文案,本地 fake 只建模了 `config file already exists` 故首跑未暴露 | **已修**(cdb76e9:统一 initAlreadyInitialized 双文案豁免;认证类错误不豁免;三组回归钉住;真机二次全绿) |
| W3-F2 | **跨节点 overlay 数据面整体不通(环境级)**:node2 上跨节点服务 DNS NXDOMAIN+VIP 不可达;两机 overlay netns 的 vxlan FDB 均无远端 VTEP;定向探测 **UDP 4789 VPC/公网两路均零到达(node2 eth1 tcpdump 0 包),同路径 TCP 7946/22 全通**——VPC/云防火墙滤 UDP。W2 断言未暴露因 ingress=宿主端口、provider=VPC TCP,从未压 overlay 数据面 | **环境侧待办**:VPC 放行节点对 UDP 4789/7946(用户操作);W3 演练改钉 manager 完成(代码面网络挂接已被容器网络 attachment 证明);跨节点 S3/服务互访在放行前不可用,诚实记录 |
| W3-F3 | docker 重启收敛窗内 daily 备份上传诚实红(rustfs 任务未就绪 DNS no such host)→下一份手动备份 ok + `backup.upload_recovered`——失败可见/恢复闭环符合设计 | 设计内行为;改进票:上传失败当日短退避重试(v0.2.x 候选) |

### W3 演练杂记(shell/CLI 坑,复用要点)

- 本仓 CLI 旗标必须在位置参数前(`logs history --service task <app>`,反序解析报错/空输出)。
- `docker ps` 双 label 值过滤在本 daemon 返回空(单滤正常)——容器定位用单滤+name 兜底。
- 无卷应用 `fleetly.placement.node` 不钉(设计冻结语义,W_PLACEMENT_STATELESS_PIN 只警告);演练钉 manager 需给应用命名卷。
- 平台证书开关往返 = 两次 LE 生产 order(配额注意);演练已用 2 枚。

## 9. W4 数据库托管实机演练(2026-09-22 实录)

形态:manager 升级 W4 main 构建(4298163,scp 二进制+console dist 替换重启);镜像预拉三枚——postgres:16/redis:7 公网按 tag 直拉(RepoDigest 落 index digest,与 `DefaultPostgresImage`/`DefaultRedisImage` 钉版逐字一致)、**dbtools 私有 ghcr 包经 SSH 管道喂 token 登录→按 tag 拉→登出(凭据零落盘)**;rustfs 沿用 W3 启用态(备份目标就绪)。演练实例 pgprod2(postgres-16)+引用应用 w4app(dbtools 当 psql 客户端)。

### 断言链(v3 全量实录)

| 断言 | 实录 | 结果 |
|---|---|---|
| A1 建库→健康门 | create 受理→**9s** ready(真 postgres 容器+pg_isready swarm 健康闭环) | ✅ |
| A2 卷/绑定/reveal | `fleetly-db-pgprod2-data-*` 卷登记;placement=manager 平台 ID;reveal 取得密码(审计留痕) | ✅ |
| A3 引用部署 | writer 应用(写循环)部署成功;服务 env 含 `FLEETLY_DB_PGPROD2_URL`(真密码);**15 行真实写入** | ✅ |
| A4 备份→verify | manual 备份受理→6s `verify_status=verified`(restic 真链路→rustfs,同 repo `db/<instance>/` 命名空间) | ✅ |
| A5 破坏清空→原地恢复 | truncate(基线 17)→count=0→restore confirm→**恢复至 15 行=备份时点状态**(写入器在备份后又写 2 行——**点时语义精确证明**,脚本断言按备份时点重判后 ✅);全程主状态 ready | ✅ |
| A6 轮换 | 密码变化;引用 app **自动重部署**且服务 env 换新密码;聚焦复测:rotate **同秒**新密码 psql 连通(t+0s),任务重建亚秒收敛 | ✅(复测洗清 v3 首测时序伪影) |
| A7 secrets | set→external 声明部署→`/run/secrets/mytoken` **逐字**;rm→悬空部署 `E_SECRET_NOT_FOUND` 诚实失败 | ✅ |
| A8 暂停/恢复 | suspend→服务 0/0;resume→ready(16s) | ✅ |
| A9 删除守卫/reap | 有引用删除→`E_DB_REFERENCED` 409;摘引用重部署→删除→deleted+服务移除+**数据卷 orphaned 保留** | ✅ |
| A10 预算 | fleetlyd idle 76.9MB(W3 64.4+12.5,database duty 增量);平台零新增常驻组件(D-DB-9:库/作业=用户负载);rustfs 演练后 116.8MiB(restic 负载后,限 256MiB 内) | ✅(轻量口径) |
| Console | /ui 前缀路由 `/ui/databases` 200,新 bundle 含 databases 面 | ✅ |

**v3 结果:35/35(含两处重判)**。v1/v2 的 19+35 处 FAIL 全数归因脚本伤(JSON 取值路径/heredoc 转义/sleep 60 观察窗/缺顶层 secrets 声明/rm 无 confirm 旗标/清场撞 deleted 名字保留期),平台面零缺陷。

### W4 真机发现与注记

| # | 发现 | 处置 |
|---|---|---|
| W4-N1 | **digest 直拉镜像经 save/load 丢 tag 引用**(只剩 untagged 条目,平台 tag@digest 引用无法解析):`docker pull name@sha256:` 后 save 的 tar 里 RepoTags 为空 | 预拉正道=**VPS 上按 tag 直拉**(RepoDigest 落 index digest);私有包经 SSH 管道喂 token 登录拉取后登出。已记 image-prepull.md |
| W4-N2 | PG 轮换触发库任务快速重建(Swarm secret 不可变→换值必换名→spec 变更):真机实测亚秒收敛、新密码同秒可用,连接无感 | 设计 §2.5 措辞已修正(「库不重启」准确口径=引擎数据面不停机,非任务零重建);非缺陷 |
| W4-N3 | 恢复点时语义:恢复回到**备份时刻**状态(备份后至清空前的写入不保留) | 设计内行为;演练断言应以备份时点为期望值(runbook 固化本行) |
| 观察 | app 删除 reap 后偶见一个 stray 任务容器 Up(service 已移除)——`docker rm -f` 收掉 | 巡检项,随 F11/孤儿清理票核对 |

### W4 演练杂记(复用要点)

- `fleetly databases get --json` 是**顶层视图**(status/placement/connection 直取,无 database 包裹);reveal --json 原生 protojson(`password` 直取);backups --json 键=`snapshot`/`verify_status`。
- 库实例名保留期:deleted 行占用名——**清场后重建须换名**(pgprod→pgprod2),或先行接受唯一冲突诚实拒绝。
- 演练应用退出码敏感:`sleep 60` 在部署观察窗内退出 0→`E_OBSERVE_UNHEALTHY`;常驻容器用 `sleep infinity`。
- 复杂远程操作一律脚本化(scp+sh)——cmd→ssh→sh 三层引号不可手工内联。

## 10. 控制面 TLS 开启与 Console 访问口径(W5-S5,2026-09-22)

控制面双面(8420 HTTP / 8421 gRPC)默认明文;开启 TLS 改 `control_plane.tls.mode`(web 终端专项设计 §3.1,键位注释见 config-example.yaml):

- **platform 模式**(推荐,`base_domain` 非空时):复用平台证书(LE 签发/续期由平台证书 duty 承接),证书续期落盘后约 60s 内热重载(新连接用新证书);证书就绪前 TLS 面照常监听、握手失败(日志有 warn/ready 锚点),等待签发的空窗属正常。
- **manual 模式**:自备证书对(`cert_file`/`key_file`),启动即校验可读,缺失报错拒绝启动;证书更换需重启生效。
- 最低协议版本 `control_plane.tls.min_version`(缺省 tls1.2)。不做 mTLS/客户端证书——Bearer token 仍是唯一认证。

**TLS on 后的访问口径**:

| 面 | 地址/用法 |
|---|---|
| Console | `https://<SAN 主机>:8420/ui/`(platform 模式即 `https://console.<base>:8420`——需 DNS A 记录指到 manager;IP 直连会有证书名不匹配,浏览器例外或换 SAN 主机名) |
| REST /v1 | 同上 `https://<SAN 主机>:8420/v1/**`(Bearer 不变) |
| CLI | `--tls`(校验,ServerName=所拨主机名)/`--tls-insecure`(显式跳过校验)/env `FLEETLY_TLS=true\|insecure`;两旗标互斥;缺省明文(存量兼容) |
| SDK | `WithTLS(*tls.Config)` / `WithTLSInsecure()`;TLS 拨号时 Bearer 凭据自动要求传输安全 |

## 11. W5 观测/通知/终端/TLS 实机演练(2026-09-22 实录)

形态:manager 升级 W5 构建(S1~S6 全量,scp 二进制+console dist 替换重启;镜像预拉 VL/VM/cAdvisor(gcr)/node_exporter + exec 镜像 CI 首推后直拉)。**staging 现保持态:TLS platform 模式开(8420/8421 双面,平台证书三 SAN console/ctrl/registry.dev)+VL 默认捆绑 on+终端 relay on(wss 反向常连)+metrics off(opt-in 缺省)+rustfs on(W3 起)**。

| 断言/能力 | 实录 | 结果 |
|---|---|---|
| VL 默认捆绑升级即生效 | 未显式设置的存量安装升级后 duty 自动部署 fleetly-victorialogs 1/1;`logs backend show` = victorialogs(default)/deployed/**ingest ok**/dropped 0;`logs.victorialogs_deployed` 事件 #348 | ✅ |
| 检索面 | 容器日志 `logs search --keyword` 命中(echo 循环 marker 3 行带时间戳);REST SearchLogs 同源 | ✅ |
| 访问日志归因(R4 载体=VL) | curl hello.dev 域名 → access 行带 method/status/host/path/route/client_ip/duration_ms/**deployment_id**(命中该 app 最近 succeeded 部署) | ✅ |
| Web 终端(D-W5-3 反向常连) | relay global 1/1;`nodes_connected:1`;termclient 真 PTY `echo` 回显 MATCH;terminal.opened/closed 事件;会话内容零泄漏(事件流 grep 0);打错 app 名(无运行任务)=诚实 500 非 5xx 假成功 | ✅ |
| TLS platform 真证书 | 8420/8421 双面 TLS(平台证书 LE 三 SAN);https+SAN+平台 CA 三端点 200(liveness/landing/`/ui/`);明文双面拒;CLI off-host(SSH 隧道)+`--tls-insecure` apps list 通——**token 全程加密**;exec relay 自动 wss 化(spec 漂移收敛 FLEETLY_CONTROL_TLS_NAME) | ✅ |
| 通知(V2-6) | 端点创建(patterns `deployment.*`,secret 一次性返回+指纹常驻);真实部署事件投递到达(python3 receiver 落盘请求原文);**HMAC 重算 5/5**(轮换 secret 后 python 独立复算);台账 ok 行 response_code 200;test 载荷到达;零 notify.* 事件(回环红线) | ✅ |
| metrics opt-in(D-W5-2) | `metrics mode set on` → 三件 3/1 收敛;VM 回环查询 up=1×2;`nodes_reporting: 1/2` 诚实(worker=Down node2);**S3 缺陷真机暴露并修复**:cadvisor 单值 containerd socket 参数在系统 containerd 宿主失效(dind=dockerd 私有 socket,本机=/run/containerd)→ 改 `/bin/sh -c` 自适选择一(双路径都在 /var/run 挂载内);修复后每容器序列恢复+image 标签在(swarm 归属标签仍缺=上游限制,共享镜像时 image 不足以归因 app,Console 降级口径维持) | ✅(含 1 修复) |
| **预算实测(steady-state 口径)** | 默认面(VL on+rustfs on+exec on,metrics off):平台容器 297MB+dockerd/containerd ≈195MB ≈ **492MB<600MB ✓**(VL 仅 12.7MB——V2-1 默认捆绑的成本实证);**metrics on 态 ≈671MB 超顶**(VM 113.9+cadvisor 36.2+node-exporter 18.6)——D-W5-2 opt-in 裁决被实测验证:启用即用户显式接受;VM allowedPercent 收紧挂账 | ✅(诚实) |
| **W5 门上真机修复二件** | ①cadvisor socket 自适应(见上行);②**gateway TLS 回拨缺陷**:TLS 形态下 gateway 对 8421 明文回拨使全量 REST /v1 断(e2e 只测 CLI 直连+native 端点未拦住,staging 实证「error reading server preface」)→ 回拨凭据跟随 TLS 形态(回环自拨+进程信任锚)+单测正负向+e2e TLS-9 断言;修复后 staging REST/terminal/status 200+relay wss 连接 | ✅ |

**W5 演练发现(W5-F1/F2)与用户动作项**:

| # | 发现/待办 | 处置 |
|---|---|---|
| W5-F1 | 云安全组未放行 8421 公网(仅 80/443/8420/22)——公网 CLI TLS 直连暂不可达,本次经 SSH 隧道验证;TLS 就绪后放行 8421 是安全的(token 已加密) | **用户动作项**:安全组放行 8421/TCP(可选——或维持 SSH 隧道形态) |
| W5-F2 | console.dev/ctrl.dev 无公网 DNS 记录(hosts 是 127.0.1.1 占位)——Console 经 `https://fleetly-dev.deeploop.net:8420/ui/`(浏览器证书例外)可达;域名化访问需 A 记录 | **用户动作项**:加 `console.dev.fleetly.run`(及可选 `ctrl.dev`)A 记录指 staging 公网 IP |
| 挂账 | metrics-on 超顶(671MB)的组件收紧票(VM `-memory.allowedPercent`/cadvisor 限额) | v0.2.x 候选 |
| 挂账 | exec 镜像 digest 已钉(CI 首推 run 35754500342,staging RepoDigest 一致);换版走 exec.yml 同款 dispatch | 已闭环 |
| 挂账 | 跨节点 metrics/终端 worker 面:node2 仍 Down+UDP 未放行(W3-F2 延续);relay 反向常连设计在 VPC TCP 上不受影响,worker 归队即可验 | 用户动作项(与 W3-F2 同源) |

## 12. v0.2.x 收尾波实机验证(2026-09-23 实录)

staging 升级收尾波构建(7dc5d2e 同源)后逐票验证:

| 断言/能力 | 实录 | 结果 |
|---|---|---|
| 保留字拒绝(收尾 S3) | `fleetly deploy` app 名 `rustfs` → **E_APP_NAME_RESERVED 结构化报错**(撞键证据+保留字全集+建议;命名审计证实 cron/db/dbjob 前缀族有破坏性撞键) | ✅ |
| F11 settled-drain 披露+自愈(收尾 S3) | 节点 `--availability drain` → 事件 **#393 app.degraded app:stateful-drain**(水位 payload)+派生态 running→degraded;恢复 active → **#397 app.recovered** 自愈;二次 drain → **#399 再披露**(可重触发)→ 回 running。判据=期望实例>0 且 running 任务=0;外部 `scale=0`(spec 副本=0)不误报(护栏实测) | ✅ |
| 验证脚本教训 | 首两轮 "MISSING" 系**脚本盲**——events watch 循环漏 `--tls-insecure`(明文拨 TLS 口 → stderr 被丢弃的盲管道),事件一直在;平台零缺陷 | 记档 |
| s3 status 实值渲染 | staging `s3.public_exposed=off`(W3 演练保持态)→ 无公网行=设计内(渲染面单测覆盖:开=实值/关=无行) | ✅(口径) |
| 收尾波整体回归 | logs e2e 26/26(引擎改动回归);dbtools digest 直拉 e2e 27/27(debian 基底+单 job 恢复真数据闭环);metrics e2e 23/23 | ✅ |

挂账(收尾波新增):8423 根治=内部 CA+swarm secret 分发+IP SAN 证书(设计级立项,依赖链钉在 traefik.go TODO(harden-8423));VM `-memory.allowedPercent` 可选优化;存量保留字 app 的重部署边界(新部署拒绝,存量视图不受影响)。

## 13. v0.3 W1+W2 真机演练(2026-09-24 实录,fresh 清空重建)

形态:**staging 清空重建为 fresh v0.3**(D-W0-5 修订口径——抹 /var/lib/fleetly 与全部 fleetly- swarm 对象,保留 swarm 集群与 config.yaml;scp main 构建 67c9dc4+双 fix 的 fleetlyd/fleetly/console dist)。冷启动即平台组件自动收敛(exec/ingress/registry/VL 各 1/1),平台证书 LE 重签(周内重复签发限额内)。

验证套件 `.w3out/staging-v03/verify.sh`(SV-1..SV-15)+ 定向复验,三轮迭代后 **32/32 全过**:

| 断言族 | 实录 | 结果 |
|---|---|---|
| fresh 认证链 | 注册窗口恒开+无用户→bootstrap 预注册可用→首用户注册=平台管理员+个人队 founder(owner)+默认项目 default→**bootstrap 即刻 401**(SV-1..5) | ✅ |
| 机具令牌+三段命名 | founder 会话铸 machine PAT→`FLEETLY_PROJECT=founder/default` deploy→**服务名 fleetly-founder-default-demo-web**(SV-6/7) | ✅ |
| 邀请链 | owner 建邀(developer)→临时开注册→mate 注册→accept(生效角色回读)→关注册(SV-8) | ✅(注记①) |
| 角色矩阵 | developer 可部署、**env 按键明文读 403**(写 200 对照);项目覆写 mate→viewer 后部署 403,恢复 developer 后复原(SV-9/10) | ✅ |
| 同名跨项目 | 同团队第二项目 staging 部署**同名 demo 成功**→两服务并存 fleetly-founder-{default,staging}-demo-web(SV-11) | ✅(注记②) |
| E4 跨项目守卫 | founder/default 建库 pgshared(postgres-16,就绪)→staging 项目 app 以 `fleetly.databases: pgshared` label 引用→**E_DB_PROJECT_MISMATCH 拒绝**(SV-12) | ✅ |
| 观测/事件/TLS | VL 默认捆绑 marker 检索命中;事件流 user.registered×2/team.created×2/project.created×3;Console /ui/ 200;明文 8420 拒(SV-13..15) | ✅ |

**演练产出两修一挂账**:
- 修①(7082385):**deploy 同名跨项目按设计新建**——S3 实现的「全局按名解析→409 指引 MoveApp」偏差与 D-W0-4 二修矛盾(§8 演练要点直判),ensureApp 改「目标项目内无此名即新建」+回归测试钉死;
- 修②(0b669d7):CLI deploy 成功后尾查 GetApp 裸名歧义→限定形化;
- 挂账①:**邀请未注册用户需临时开注册窗**(设计 §3.1「注册即自动 accept」的服务端 invite-token 注册通道未实现,Console 现靠 from-回跳+临时开窗;W3 候选票)。**已收口(W3-S4)**:RegisterRequest 增可选 invite_token——带有效邀请的注册豁免注册窗且同事务自动消费入队(受邀角色),无效 token 拒 E_INVITE_INVALID;关窗邀请注册全链已入 e2e/rbac.sh RB-0 断言。
脚本侧教训:SetRegistration 是 bool `open`(非字符串);库投影字段 `status`;env 明文=按键 GET `/env/{key}`;`logs search` 的 app 是位置参数;E4 引用声明=`fleetly.databases` label(非 env URL 探测)。

### §13.2 XFF 信任边界(W3-S4 收口,含上游可信代理放开指引)

W1-S2 披露的 XFF 伪造面已收口:gateway 层(newGatewayMuxWithTLS 的 sanitizeForwardedFor)删除入向 `X-Forwarded-For` 头——控制面自身即反代边界,grpc-gateway 随后以真实 TCP 对端地址回填 x-forwarded-for metadata;api 侧 clientIPFromContext 只在 gRPC 对端为环回(=gateway 回拨)时采信该 metadata,远程直连 gRPC 方自带的 XFF metadata 一律不采信(IP 键限流不可伪造;email 键不受影响)。

**上游可信代理形态(如外置 TLS 反代指到 gateway 端口)的放开指引**:当前实现会把代理注入的 XFF 一并清除,全量外部客户端将共享代理地址的注册/登录限流桶——email 键限流仍独立生效(防撞库主防线在),仅 per-IP 维度退化为 per-proxy。如需按代理放行(trusted_proxies 类配置:信任清单内代理注入的 XFF 取最后一个非清单项),属配置面扩展点——现票刻意不发明开关;有真实部署形态需求时按 errcode/config 注释惯例立项,勿在部署侧手工改头绕过(清洗层在进程内,改不了)。

### §13.3 W3 审计/留存/指纹/邀请注册 真机验证(2026-09-24 实录)

W3 构建(c9851e0)部署后 **17/17 全过**(`/tmp/v03/verify-w3.sh` 可复跑;两条脚本态——Me teams[0] 是个人队非受邀队/留存首轮已改——均定向查询证实):

| 断言族 | 实录 | 结果 |
|---|---|---|
| 审计读面 | `audit list`(action 过滤命中/result=error 过滤命中)/`audit export --csv`(RFC4180 表头+数据行) | ✅ |
| FZ-12 指纹 | `git fingerprint`=SHA256:gx2AO0hn…;GetSystemStatus.git_ssh_fingerprint 同值 | ✅ |
| invite-token 注册 | **关窗状态下**携有效邀请 token 注册 200→Me 双团队(个人队 newbie owner+受邀队 founder viewer——注册即自动 accept);无效 token 409 E_INVITE_INVALID | ✅ |
| 留存设置 | GET 未设置缺省态→PUT 30 读回 30→audit.retention_changed 落档→复位 90 | ✅ |

staging 附加态:newbie 用户;留存=90;W3 功能(审计页/覆写按钮/指纹卡/留存入口)Console 面可浏览器复核。

staging 现保持态:fresh v0.3(W5 构建 44b7fe8),TLS platform on(证书重签),founder/mate/newbie 三用户,demo×2 项目+matedemo+pgshared 库在役;metrics+alerts on(vmalert 第四件在役);node2 仍 Down(W3-F2 UDP 未放行)。

### §13.4 W5 自动扩缩+告警真机演练(2026-09-25 实录)

W5 构建部署后 verify-w5.sh(/tmp/w5bin,可 W5_SKIP_UPGRADE=1 复跑)+定向 ALM 复验:

| 断言族 | 实录 | 结果 |
|---|---|---|
| 自动扩缩 | metrics 栈收敛→stressapp 部署→策略(min1/max4/target20%)→**真扩容 2→4**(journal:"autoscaler adjusted replicas, before2 after4, dimension=cpu")→冷却窗护栏→**比例缩容 4→3**→dormant 披露 | ✅ |
| 告警链 | sink 端点→规则→vmalert 部署→**firing×10+resolved×8 载荷送达**(sink.log 计数;双向闭环) | ✅(两修后) |
| DNS-01 通配 | ACM 腿待 FLEETLY_DNSPOD_TOKEN(用户侧) | ⏳ |

**演练产出两修**(dind e2e 绿但 TLS 形态真机暴露——**控制面组件拨自身网关必须按 TLS 模式选 scheme,0d84e6a 同族教训第二次实证**):①b0c475a notifier scheme 跟随 tls.mode+insecureSkipVerify(明文拨 TLS 口=connection reset);②44b7fe8 flag 名驼峰 `-notifier.tlsInsecureSkipVerify`(点形 crash-loop,镜像 -help 实证)。
脚本侧教训(第三次!):events watch 流式命令必须有界(timeout 包装——F11 盲管道同族);webhook 通道告警载荷=status 小写 JSON(Slack/Email 才有 FIRING/RESOLVED 大写文案——grep 口径按通道类型)。

### §13.1 console 免端口直访(2026-09-24,用户实报易错点收口)

用户连续踩两坑(deeploop.net 主机名证书报警→console.dev.fleetly.run 缺端口缺路径打到 traefik 404)后落的产品修复(main 0921a1d+0d84e6a):

- **访问口径(新)**:`https://console.dev.fleetly.run/` ——根路径 302 到 /ui/,traefik 443 反代到控制面网关(advertise:8420,TLS+跳过服务器认证 transport——IP 端点无 SAN,F9 修订二同口径);证书绿锁。
- 旧口径仍有效:`https://console.dev.fleetly.run:8420/ui/`(控制面直连)。REST `/v1/**` 同享免端口形态。
- 实现要点:ingress Route 增 BackendURL/Transport/RootRedirect 三平台段字段;网关端口与 TLS 形态由 runtime 注入(ControlGatewayPort=addr 端口位,ControlGatewayTLS=tls.mode!=off)——**勿用 cfgPort(配置端点 8422/8423)当网关端口**(初版即犯此错,staging 500 'first record does not look like a TLS handshake' 揪出)。
- 前提:网关须绑定非回环(远程访问 Console 的安装形态天然满足;纯回环绑定下该路由 502,注释已诚实记录)。



