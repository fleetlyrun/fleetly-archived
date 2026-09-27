# Runbook：平台镜像预拉与 digest 台账（T0-V2.3）

适用：fleetly v0.2 W0（2026-09-20）。背景：zane-ops 的 CI 用维护者个人
fork 镜像 + `canary` 可变 tag——供应链反面教材（docs/research/
2026-09-20-zane-ops-comparison.md R7/A6）；本仓 S20-F1 已把 GitHub Actions
钉 commit SHA，本票把同一纪律扩展到**容器镜像引用**并补 arm64 冒烟
（release.yml `smoke-arm64` job）。

## 0. 纪律与形态

- 引用形态：`image:tag@sha256:<64hex>`——tag 保留作可读性，**digest 为准**。
- digest 取**多架构 index**（manifest list）摘要：amd64 / arm64 通吃，
  与 release 的 arm64 冒烟口径一致。
- 纪律范围：`deploy/**` 与 `.github/workflows/**` 中平台运行时/测试夹具
  引用的容器镜像（下表全集）。**不得**引用无 digest 的平台镜像。
- 门禁：`deploy/check-image-pins.sh`（已接入 pr.yml `deploy-scripts` job，
  无 digest 引用即红）。豁免走 `deploy/image-pin-allowlist.txt`（§5）。

## 1. 平台镜像台账（全集，2026-09-20 解析）

| # | 镜像（钉定形态） | digest（sha256 前缀） | 用途 | 引用位置 |
| --- | --- | --- | --- | --- |
| 1 | `docker:29.8.1-dind` | `3f3c01aa…283f0` | dind 测试底座（与引擎门禁下限一致） | pr.yml e2e；nightly.yml 顶层 env + V7 矩阵；deploy/run-dind-test.sh、run-calibration.sh、run-journey-test.sh、run-upgrade-test.sh（DIND_IMAGE 默认值）；deploy/cal-inner.sh、test-journey.sh（DIND_TAG 默认值） |
| 2 | `docker:29.7.2-dind` | `3ef33f2e…74cb6` | V7 引擎矩阵上一受支持 minor | nightly.yml engine-matrix |
| 3 | `alpine:3.20` | `d9e853e8…4b6bc` | 辅助镜像（预拉暖机 / fixture sidecar / cert 卷检查容器） | deploy/cal-inner.sh；test-journey.sh（预拉 + sidecar spec `image:` + cert 卷检查）；test-upgrade.sh |
| 4 | `alpine:3.22` | `5291449c…5e8fa8` | fleetlyd 容器运行层 | deploy/Dockerfile.fleetlyd |
| 5 | `golang:1.26-alpine` | `51a7c389…f59f1ae` | fleetlyd 容器构建层 | deploy/Dockerfile.fleetlyd |
| 6 | `traefik:v3.5` | `16acb89c…9bf1d2` | ingress 暖机（cert seed 与 Traefik 服务不自动拉镜像——T2.15 已知边界） | deploy/cal-inner.sh；test-journey.sh；test-upgrade.sh |
| 7 | `moby/buildkit:v0.32.2` | `28a89871…bb41d8` | fleetly-buildkit 构建器 warm 路径依赖 | deploy/cal-inner.sh |
| 8 | `ghcr.io/letsencrypt/pebble:latest` | `ddf23064…78199` | ACME 测试 CA（journey 链路代演） | deploy/test-journey.sh（`PEBBLE_IMG`） |
| 9 | `curlimages/curl:latest` | `58adaa4e…166777` | HTTPS 探针（journey J4） | deploy/test-journey.sh |
| 10 | `ghcr.io/project-zot/zot:v2.1.21` | `6b69512c…f48c8` | 平台 registry（zot，E1-4 部署器钉版缺省；多节点 manager 平台组件） | internal/ingress/registry.go `DefaultZotImage`（Go 常量字面，不在 `deploy/**`/`.github/**` 扫描口径内——钉版形态由本行与本常量双锚，改动须同步） |
| 11 | `restic/restic:0.19.1` | `136600b6…d510` | 状态备份远端上传轨（restic 钉版容器一次性执行，E3-3/D-S3-3；首次上传按需拉取，预拉可选） | internal/statebackup/restic.go `DefaultResticImage`（Go 常量字面，不在 `deploy/**`/`.github/**` 扫描口径内——钉版形态由本行与本常量双锚，改动须同步；2026-09-21 解析） |
| 12 | `rustfs/rustfs:1.0.0` | `8cc98017…d1ff` | 托管 RustFS（opt-in 管理组件，E3-5/D-S3-10；s3.mode=rustfs 时 duty 按需拉取，预拉可选；多架构 OCI index amd64/arm64） | internal/rustfs/spec.go `DefaultRustFSImage`（Go 常量字面，不在 `deploy/**`/`.github/**` 扫描口径内——钉版形态由本行与本常量双锚，改动须同步；2026-09-21 解析：1.0.0 为最新 1.0.x stable（2026-09-16 发布，与 latest tag 当前所指同 digest）） |
| 13 | `ghcr.io/fleetlyrun/dbtools:v0.3.0-dbtools.1` | `2b9288a9…bd96` | 库备份/恢复/校验一次性 job 的执行体（多引擎工具面 + restic，E4 D-DB-6；备份受理时按需拉取，预拉可选；多架构 amd64/arm64；**PRIVATE ghcr 包**）。**W4-S2 v0.3.0 多引擎工具面重发（CI 首推 2026-09-24 run 36063651480，digest 已钉）**：在 v0.2.1 debian 基底（postgres:16 同 digest 基底 + redis:7 拷入 redis-cli）上，从 mysql:8.4（#19 同 digest）与 mongo:8.0（#20 同 digest）引擎镜像**引擎同源 COPY** 工具面——mysqld+mysql/mysqldump/mysqladmin、mongod+mongosh/mongodump/mongorestore（恢复 = 镜像内临时引擎实例重放，D-W4-3；工具与引擎逐位同版；否决 debian default-mysql-client——MariaDB 客户端无 --source-data 无 mysqld；mongo 官方镜像自带全套，外置 deb 供应链面未引入）。基底缺库精确补集挂 /usr/local/dbtools-lib（LD_LIBRARY_PATH）；secure-file-priv 目录物化（mysqld 8.4 缺目录拒起实测）。镜像 ~400MB→~1.08GB（一次性 job 执行体非常驻，2026-09-23 预算裁决口径延伸）。e2e 无凭据本地构建腿随 digest 收紧退役为 fatal（本地构建解析不了 digest 引用——W4 实测教训） | internal/database/adapters.go `DefaultDatabaseToolsImage`（Go 常量字面，同上双锚口径，改动须同步 e2e/databases.sh `DBTOOLS_IMG`；发布 = .github/workflows/dbtools.yml，随平台 release 由 release.yml `dbtools-image` job 同版调用）。私有包预拉注意：须先认证（`docker login ghcr.io`；CI nightly databases-e2e 以 GITHUB_TOKEN + packages:read 在 dind 内直拉）。**不要用宿主 save\|load 拷贝替代直拉**——digest 钉定引用（tag@digest）的本地解析依赖真实 pull 落下的 RepoDigests，load 进来的镜像没有该记录，引用无法解析（docker 29.8.1 实测，见 e2e/databases.sh 头注）。旧载体留档：v0.2.1-dbtools.1@sha256:472e8a5d…866b（debian，PG/Redis 工具面）、v0.2.0-dbtools.1@sha256:64c367ff…99fe（alpine/musl，恢复双 job 时代） |
| 14 | `victoriametrics/victoria-logs:v1.52.0` | `47b82089…442e` | 托管 VictoriaLogs 日志库（默认捆绑管理组件，E6/V2-1；logs.backend=victorialogs〔缺省〕时 duty 按需拉取，预拉可选；多架构 OCI index amd64/arm64 等） | internal/victorialogs/spec.go `DefaultVictoriaLogsImage`（Go 常量字面，不在 `deploy/**`/`.github/**` 扫描口径内——钉版形态由本行与本常量双锚，改动须同步；e2e/logs-victorialogs.sh 以同 digest 引用拉取。2026-09-21 解析：v1.52.0 为实现时点最新 stable（GitHub releases 2026-07-16 发布，与 latest tag 当前所指同 digest——两侧 pull 解析一致）。注意官方 repo 是 `victoriametrics/victoria-logs`；设计文档字面 `victorialogs/victoria-logs` 在 Docker Hub 不存在 |
| 15 | `victoriametrics/victoria-metrics:v1.152.0` | `86ca5fdb…5cef` | 托管 VictoriaMetrics 单机版（metrics 三件套的存储/查询件，E6 W5-S3/D-W5-2 opt-in；metrics.mode=on 时 duty 按需拉取，预拉可选；多架构 OCI index amd64/arm64 等） | internal/metrics/spec.go `DefaultVictoriaMetricsImage`（Go 常量字面，同上双锚口径；e2e/metrics.sh 以同 digest 引用拉取。2026-09-22 解析：v1.152.0 为实现时点最新 stable（2026-09-14 发布；v1.151.0/v1.148.4 为老分支续版，rc/enterprise/scratch 变体不取）。注意单机版抓取配置 flag 是 `-promscrape.config`（文件路径/http URL），设计字面 `-prometheus.config` 在 v1.152 不存在——抓取配置经 swarm config 对象分发（internal/metrics/spec.go 头注记） |
| 16 | `prom/node-exporter:v1.12.1` | `1b4e4438…1be0` | 托管 node_exporter（metrics 三件套的节点指标采集件，global——每节点一任务；同上按需拉取；多架构 manifest list） | internal/metrics/spec.go `DefaultNodeExporterImage`（Go 常量字面，同上双锚口径；e2e/metrics.sh 同 digest。2026-09-22 解析：v1.12.1 为实现时点最新 stable，2026-07-14 发布） |
| 17 | `gcr.io/cadvisor/cadvisor:v0.55.1` | `3de2bd52…ec57` | 托管 cAdvisor（metrics 三件套的容器指标采集件，global——每节点一任务；同上按需拉取；多架构 manifest list） | internal/metrics/spec.go `DefaultCAdvisorImage`（Go 常量字面，同上双锚口径；e2e/metrics.sh 同 digest）。**repo 勘误**：设计字面「docker.io 系 google/cadvisor」在 Docker Hub 已标注 DEPRECATED（repo 描述原文「New images will NOT be pushed. Please use gcr.io/cadvisor/cadvisor instead」，2026-09-22 实测；google/cadvisor 最后镜像 v0.33.0 停在 2019 年）——官方多架构发布 repo 是 `gcr.io/cadvisor/cadvisor`，按实现时点核实取官方 repo。2026-09-22 解析：v0.55.1 为实现时点最新 stable（gcr.io tags/list 实测，v0.54.1 之上） |
| 18 | `ghcr.io/fleetlyrun/fleetly-exec:v0.2.0-exec.1` | `4de40017…0793b` | Web 终端执行中继（fleetly-exec global service，E7/D-W5-3；**PRIVATE ghcr 包**，预拉认证同 #13） | internal/execrelay/spec.go `DefaultExecRelayImage`（Go 常量字面，同上双锚口径；发布 = .github/workflows/exec.yml〔宿主侧 amd64+arm64 交叉编译 + buildx 双平台 + cosign，dbtools 同款〕，随平台 release 由 release.yml `exec-image` job 同版调用。CI 首推 2026-09-22 run 35754500342；staging 实拉 RepoDigest 一致；中间态 tag 豁免已摘除——见 deploy/image-pin-allowlist.txt 留档） |
| 19 | `mysql:8.4` | `0744ee5e…fb8d` | 库引擎镜像（模板 mysql-8.4，MySQL 8.4 LTS，v0.3 W4 D-W4-1；库服务收敛按需拉取，预拉可选；多架构 OCI index amd64/arm64） | internal/dbtemplate/dbtemplate.go `DefaultMySQLImage`（Go 常量字面，同上双锚口径，改动须同步；2026-09-24 解析：8.4 为当前 LTS 最新 minor 的 8.4.x 系 tag 所指；digest 双验 = tag 拉取 RepoDigest 一致 + 按 digest 以 arm64 平台独立拉取交付 arm64 镜像〔index 摘要判定〕）。备份/恢复工具面依赖 S2 的 dbtools 镜像扩展（设计 managed-databases §8 D-W4-3） |
| 20 | `mongo:8.0` | `4968f22d…5efc2` | 库引擎镜像（模板 mongodb-8.0，MongoDB 8.0 Community，v0.3 W4 D-W4-2；按需拉取同上；多架构 OCI index amd64/arm64） | internal/dbtemplate/dbtemplate.go `DefaultMongoImage`（同上双锚口径；2026-09-24 解析：8.0 为当前 LTS 最新 minor 的 8.0.x 系 tag 所指；digest 双验同 #19 方法）。凭据边界注记：官方入口把 initdb root 恒建于 admin 库 → 连接串投影带 `?authSource=admin`（设计 §8.1） |
| 21 | `victoriametrics/vmalert:v1.152.0` | `ba005663…96b2` | 托管 vmalert 规则评估器（metrics 栈第四组件，D-V3W5-1；alerts.mode=on 且 metrics.mode=on 时 duty 按需拉取，预拉可选；多架构 OCI index amd64/arm64 等） | internal/metrics/spec.go `DefaultVMAlertImage`（Go 常量字面，同上双锚口径；e2e/metrics.sh 同 digest 预拉行。2026-09-25 解析：组件与单机版同发同版号，v1.152.0 为实现时点最新 stable〔GitHub releases 2026-09-14，与 #15 同日核实，rc/enterprise 变体不取〕；digest = `docker buildx imagetools inspect` 多架构 index 实测）。flag 取证注记：notifier 认证 flag 实际形态为 `-notifier.basicAuth.username/-notifier.basicAuth.password`〔及 `*File` 变体〕，设计 §2.1 字面 `-notifier.basicAuthUsername/Password` 为笔误缩写——实现取 passwordFile 形态（凭据材料不进服务 spec，ingress token 文件复用） |
| 22 | `ghcr.io/fleetlyrun/dbtools:v0.3.1-dbtools.1`（**待 CI 发布，tag 建议值**） | **待回填**（多架构 index digest，`buildx --push` 输出） | 库备份/恢复/校验一次性 job 的执行体（**IMPL-DB-0 单镜像双 PG 大版本工具面**：postgres:16 基底 + postgres:18 版本分区 COPY〔`/usr/lib/postgresql/18` + `/usr/share/postgresql/18` + libnuma/liburing 补集〕；工具面版本纪律 = 与实例数据目录同 major，job 脚本按模板 `Major` 取 `/usr/lib/postgresql/<major>/bin` 显式绝对路径；mysql/mongo/redis/restic 面不变） | internal/database/adapters.go `DefaultDatabaseToolsImage`（Go 常量字面，同上双锚口径，改动须同步 e2e/databases.sh `DBTOOLS_IMG`；发布 = .github/workflows/dbtools.yml——单镜像形态不变，dispatch/随 release 调用）。**本票发布挂账（不 commit/push 纪律下 CI 无法构建新内容）**：发布命令 + digest 回填三锚清单见 docs/plan/2026-09-26-torchwood-line-impl.md §4「IMPL-DB-0 实施记录」；本地 amd64 构建实证可构建（`docker buildx build --load`，size 1.133GB = 现行 +48.8MB，机制/体积证据见同节审查记录）。**回填前 PG18 模板的 job 以「镜像缺该 major 工具面」显式失败**（fail-loud，不静默降级） |

台账与实际引用集的一致性以门禁扫描为准：

```sh
sh deploy/check-image-pins.sh -l    # 列出全部识别到的引用与钉定状态
```

（2026-09-20 实跑：23 处引用全部钉定，对应上表 9 个镜像；#10 zot 为
E1-4 起的 Go 常量钉版引用，不在门禁扫描口径内，改动须同步本表——
见该行引用位置注记。）

## 2. digest 解析与独立复验

解析（取输出首部 `Digest:` 行 = 多架构 index 摘要）：

```sh
docker buildx imagetools inspect traefik:v3.5
# Name:      docker.io/library/traefik:v3.5
# MediaType: application/vnd.oci.image.index.v1+json
# Digest:    sha256:16acb89c6db341182970d6fdafece31303b0a380a8ed7aa51682e225229bf1d2
```

独立复验（钉定形态可被解析且 digest 与台账一致；两命令任一即可）：

```sh
docker buildx imagetools inspect traefik:v3.5@sha256:16acb89c6db341182970d6fdafece31303b0a380a8ed7aa51682e225229bf1d2
docker manifest inspect traefik:v3.5@sha256:16acb89c6db341182970d6fdafece31303b0a380a8ed7aa51682e225229bf1d2
```

解析不到 digest（网络/仓库原因）的镜像**保持 tag 引用**并列遗留，
不得编造 digest——本票无此情况（9/9 解析成功）。

## 3. 预拉命令

### 3.1 在线主机（干净 VPS 跑平台测试前）

```sh
# 全集预拉（与台账一致；平台脚本 run-* 已内置各自所需子集的预热）
for ref in \
  docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0 \
  docker:29.7.2-dind@sha256:3ef33f2e220b79ed3ef3b99d81746f06f306cd6340e2cb7331d17ae996e74cb6 \
  alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc \
  alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 \
  golang:1.26-alpine@sha256:51a7c389a5ddaf82f527191a1e9bff9928655130a44e4975dd1d7e0acf59f1ae \
  traefik:v3.5@sha256:16acb89c6db341182970d6fdafece31303b0a380a8ed7aa51682e225229bf1d2 \
  moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8 \
  ghcr.io/project-zot/zot:v2.1.21@sha256:6b69512c00dceaad05b1144e6079aac6aa7309d7fd200f9947ecb1de09cf48c8 \
  ghcr.io/letsencrypt/pebble:latest@sha256:ddf230642b1a584f519f32e347de1b05a6e4c1f6c35c1863b33effeab5f78199 \
  curlimages/curl:latest@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777
do docker pull -q "$ref" || exit 1; done
```

### 3.2 离线环境（air-gapped VPS / dind）

```sh
# 在线机器导出（digest 钉定形态 save/load 后 digest 关系保持）
docker pull -q traefik:v3.5@sha256:16acb89c…9bf1d2
docker save traefik:v3.5@sha256:16acb89c…9bf1d2 | gzip > traefik-v3.5.tar.gz
# 离线机器导入
docker load < traefik-v3.5.tar.gz
```

dind 内预热同理：`docker exec <dind> docker pull -q <ref>`（nightly
run.sh 与 run-calibration.sh 已内置并行暖机，形态与 §3.1 一致）。

## 4. 换版流程（升级基镜像 / dind 引擎）

1. 选定新 tag，用 §2 命令解析其 digest；**禁止只换 tag 不换 digest**。
2. 全部引用同步（全局搜旧 tag：`grep -rn "<old-tag>" deploy/ .github/workflows/`）；
   dind 引擎换版须 V7 矩阵两腿（29.8.1 / 29.7.2）一起裁决。
3. `sh deploy/check-image-pins.sh` 必须绿；负路径抽检一条新引用未钉形态应红。
4. 跑受影响套件（deploy/run-dind-test.sh 起）后更新 §1 台账（digest 与日期）。

## 5. 门禁与豁免

```sh
sh deploy/check-image-pins.sh              # 扫描既定范围，无 digest 引用即非零退出
sh deploy/check-image-pins.sh -l           # 列表模式（台账一致性核对）
sh deploy/check-image-pins.sh FILE...      # 只扫指定文件（负路径自证用）
```

- 扫描口径与已知盲区（间接拼装引用、printf 动态 Dockerfile 等）见脚本
  头注释；识别不到 ≠ 允许——新增镜像引用优先用字面 `name:tag@sha256:…`。
- 负路径自证（2026-09-20 本机实跑）：构造含 `FROM alpine:3.22` 与
  `docker pull traefik:v3.5` 的临时文件 → 退出 1 并逐条标注 file:line；
  豁免清单（`-a` 换临时清单）命中一条后仍对未豁免引用退出 1；全部豁免
  则退出 0。
- **2026-09-27 IMPL-DB-0 盲区修复**：FROM 行不再套用「tag 非纯数字」噪声
  过滤——`postgres:16`/`redis:7` 这类纯数字 tag 是真实镜像引用（FROM 上下
  文不可能是时刻/端口映射噪声），修复前列表模式**零计数**、未钉也不报错。
  复验（本机实跑）：未钉 `FROM postgres:18` → 退出 1 逐条点名；仓库正路径
  计数 28 → **31**（补回 `Dockerfile.dbtools` 的 redis:7/postgres:16 + 新增
  postgres:18 FROM）。`COPY --from=<外部镜像>` 仍不在候选上下文——外部镜像
  引用请以 **FROM stage** 声明（IMPL-DB-0 先例：`AS postgres-engine-18`）。
- 豁免清单：`deploy/image-pin-allowlist.txt`，每行固定子串命中
  `<路径>:<引用>` 即豁免，**必须同行注释理由**（如故意验证「可变 tag
  被拒」的负路径用例）。当前条目：无。

## 6. 范围外与遗留

- ~~`e2e/nightly/*.sh`（infra-b.sh、resource-sample.sh、run.sh、
  conformance-builder.sh、n-stamp.sh）仍以 tag 形态引用 alpine:3.20 /
  traefik:v3.5 / `DIND_IMAGE` 默认 `docker:29.8.1-dind`~~——**已收口
  （2026-09-21，e2e/nightly 钉版票）**：run.sh / conformance-builder.sh /
  resource-sample.sh 的 `DIND_IMAGE` 默认值、infra-b.sh 的 alpine/traefik
  预拉（原「v3 tag 逐个试拉」可变 tag 面一并消灭）、run.sh v6 腿预拉、
  n-stamp.sh read-helper 容器、v6.sh 三处 fixture 服务，全部改为
  `name:tag@sha256:…` 形态，digest 引本台账 #1/#3/#6（`sh -n` 全过；
  脚本在 `deploy/check-image-pins.sh` 扫描口径外，钉定形态以脚本内
  `ALPINE_IMG`/`TRAEFIK_IMG` 变量与本节双锚）。v6.sh 的 c3/c4 服务引用
  alpine 属同票补漏（原 §6 清单未列）。
- `pebble:latest` / `curlimages/curl:latest` 浮动 tag 的漂移风险已被
  digest 钉定消除，但升级仍须按 §4 主动换版（digest 不会自更新）。
- `smoke-arm64`（release.yml）无容器镜像依赖（原生 go build + 单测），
  与本台账无交集；其 runner 口径（ubuntu-24.04-arm，公共仓库免费）见
  job 注释。
