# deploy/staging — fleetly-in-docker dogfooding 环境(staging)定义

2026-09-29 建成的 staging 环境唯一真源:控制面 fleetlyd 以**容器形态**跑在
`fleetly-dev.deeploop.net`,其上并役 **torchwood + messageloop 双业务栈**,
数据库全部走平台托管实例。本目录 = 环境定义与重建配方;两业务仓的
`docker/fleetly/` 仍是各自**生产割接真源**,不因本目录改动。

## 拓扑

| 层 | 形态 |
|---|---|
| 控制面 | `fleetlyd` 容器(`--network host --restart unless-stopped`),数据根 host bind `/var/lib/fleetly`,config/console 从 `/opt/fleetly` 挂载;systemd unit 已 disable(回滚路径保留) |
| 业务栈 | app `torchwood`(6 常驻 + 3 init job)与 app `messageloop`(2 常驻),同项目 founder/default,经项目网互通(mlbridge → `torchwood-server:9080`) |
| 托管库 | `torchwood-pg`(percona-postgresql-18,pgvector 由迁移 000005 自建)、`twredis`(redis-7)、`mlredis`(redis-7) |
| 域名 | tw-app/tw-grpc/ml-ws/ml-grpc/ml-api `.dev.fleetly.run`(ACME HTTP-01) |

## 容器形态三适配(与 deploy/Dockerfile.fleetlyd 文档口径的差异,缺一不可)

1. **`--network host`**:VL/VM 消费面是 daemon 拨宿主回环 9428/8428(D-W5-4),桥接网络下 127.0.0.1 拨不到;
2. **数据根 host bind(非命名卷)**:平台把 `/var/lib/fleetly` 下的文件(zot htpasswd、ingress token)以 bind 挂载进 swarm 任务,任务在宿主解析路径——命名卷里宿主路径不存在,任务 Reject;
3. **镜像带 docker-cli**:buildx `docker-container://` driver 拨 buildkit 需 exec `docker`(原「docker CLI 不进容器」口径早于 buildkit 构建链)。

镜像构建(VPS 上,alpine 运行层 + 预编译二进制):

```sh
# 二进制:本地 env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/fleetlyd
cat > /tmp/rb/img/Dockerfile <<'EOF'
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache ca-certificates wget docker-cli
COPY fleetlyd /usr/local/bin/fleetlyd
RUN chmod +x /usr/local/bin/fleetlyd && mkdir -p /var/lib/fleetly /etc/fleetly
VOLUME ["/var/lib/fleetly"]
WORKDIR /var/lib/fleetly
ENTRYPOINT ["/usr/local/bin/fleetlyd"]
EOF
docker build -t ghcr.io/fleetlyrun/fleetlyd:<ver> /tmp/rb/img
docker run -d --name fleetlyd --network host --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/fleetly:/var/lib/fleetly \
  -v /opt/fleetly/etc/config.yaml:/opt/fleetly/etc/config.yaml:ro \
  -v /opt/fleetly/console:/opt/fleetly/console:ro \
  ghcr.io/fleetlyrun/fleetlyd:<ver> -c /opt/fleetly/etc/config.yaml
```

## 两栈变体与业务仓真源的差异(逐条有因)

### compose-messageloop.yml(源:messageloop `docker/fleetly/docker-compose.yml`)

| 差异 | 原因 |
|---|---|
| 删栈内 redis 服务 → 托管 `mlredis`(redis-7) | 用户裁决 staging 数据库全托管;E4 label `fleetly.databases` 牵线,地址=库网络别名,密码经 `databases reveal` + 平台 env 注入 |
| label 用 **map 形态** | compose list 形态 `- "k: v"` 不切分,整串成键名撞 `E_LABEL_RESERVED` |
| 配置键 `mlcfg`(短名) | swarm config 对象名 64 上限溢出(见 deploy 之外的平台票:internal/naming ConfigName 公式无长度守卫;A1 修复前的部署侧绕过) |

### compose-torchwood.yml(源:torchwood `docker/fleetly/docker-compose.yml`)

| 差异 | 原因 |
|---|---|
| 删栈内 redis → 托管 `twredis`;server/worker label 增补 `twredis` | 同上全托管裁决;地址/密码经 `TORCHWOOD_DATA_REDIS_ADDR/PASSWORD` env 覆盖 config.yaml 字面量 |
| minio(SILO)留栈内 | 对象存储非数据库,平台无托管模板(DT-8 口径) |
| 三个 Config 键改短名(config.yaml/runtime.sql/roles.sql),挂载目标路径不变 | 同 64 上限溢出(A1 修复前绕过) |
| 新增 `grpcbridge`(haproxy:3.1-alpine) | torchwood 的 fleetly 客户端纯明文 gRPC(`insecure.NewCredentials`),控制面 8421 是 TLS;桥在 app 网听明文 8421,TCP 中继 + 上游 `ssl verify none alpn h2 sni str(ctrl.dev.fleetly.run)` 连宿主 advertise:8421。**必须 HAProxy 不能 socat**:fleetlyd 的 grpc-go TLS 服务端要求 ALPN h2,socat OPENSSL 不支持 ALPN——TLS 握手成功后服务端即关连接,明文 gRPC(unary/流式皆然)读 server preface 得 EOF(2026-09-29 实证:CLI 经 socat 中继 unary 复现 EOF;换 HAProxy `alpn h2` 后 unary+BuildFromUpload 流式全通) |
| worker 健康探针 `kill -0 1`(原 `pgrep -x worker`) | fleetly `command` 覆盖 ENTRYPOINT 后 argv[0]=`/usr/local/bin/worker`,pgrep -x 恒 rc=1→健康门永不过;kill -0 1 = 零依赖 PID-1 存活探针(dokploy 形态 argv[0]=worker 不受影响) |

## 重建配方(scripts/ 为 2026-09-29 实录脚本,凭据全部 VPS 侧生成,不进仓库)

顺序敏感,概要:

1. `scripts/bootstrap.sh` — 首用户注册(REST `/v1/auth/register`,首用户=平台管理员,bootstrap 随注册吊销)→ 会话铸 machine 令牌 → CLI 就绪(`FLEETLY_ADDR=127.0.0.1:8421 FLEETLY_TLS=insecure`);
2. `scripts/tw-db-setup.sh` — 托管库创建(percona-18 + redis-7;messageloop 的 mlredis 在 `scripts/db-and-first-deploy.sh`),`databases reveal` 落 `/tmp/rb/secrets.sh`(0600);
3. 两栈各**两阶段部署**:`fleetly deploy`(首发预期失败:config 前哨 `E_CONFIG_NOT_FOUND`,app 由此创建)→ env/configs/domains → 再部署。torchwood 侧 env×14 + configs×3 + 域名×2 + 机具令牌(scope tasks,build)见 `scripts/torchwood-deploy.sh`;
4. **torchwood 首发竞态**:init job 与常驻服务并行,首窗角色/编码未就绪可能 `E_HEALTH_TIMEOUT`——状态收敛后重部署即绿(init job 全幂等);**首个函数的首次执行还有一个一次性挂靠竞态**:`FLEETLY_NETWORK_MEMBERS` 声明使平台在首个 task-group 网创建时排队 app 重部署(挂靠 dispatcher/server),会把在途的构建请求换掉(表现为 CLI 侧 `Post …/v1/dispatch/builds: EOF`,而构建本身在 fleetlyd 侧已成功)——挂靠一次完成后再重试即绿;
5. `scripts/tw-provision2.sh` — 供给链:sign-up(setup token)→ sign-in(**会话经 Set-Cookie TORCHWOOD_session_console,body 不投影 token**)→ `POST /v1/server/api-keys`(**必须带 `X-Torchwood-Project` 头**)→ `torchwood runbook up`(mlbridge 仓 runbooks/,供给专库/集合/索引)→ `fleetly projects network attach` 两 app(滚动入项目网)→ mlbridge env 换接 `http://torchwood-server:9080` + 项目 key → 重部署;
6. `scripts/verify.sh` — 验收:双 app derived_state/任务健康/redis PONG/E4 env 物化/域名 TLS+ALPN/VL 检索。

CLI 纪律:**flags 必须全部在位置参数前**(Go flag 包;两仓 cutover-runbook 的旧样例 `logs history <app> --service x` 解析失败,正确形 `logs history --service x --limit N <app>`)。

## 凭据与状态位置(VPS)

| 项 | 位置 |
|---|---|
| fleetly machine 令牌(日常 CLI) | `/tmp/rb/env.sh`(0600;丢失则 Console 登录重铸) |
| 两栈业务密钥(TW_SETUP_TOKEN/TW_ADMIN_PW/TW_ML_KEY/TWPW/TRPW/MLREDIS_PW) | `/tmp/rb/secrets.sh`(0600) |
| torchwood Console | `https://tw-app.dev.fleetly.run`,founder@torchwood.local(口令在 secrets.sh) |
| fleetly Console | `https://console.dev.fleetly.run`,founder@fleetly.run / founder-pass-1 |
| 回滚物 | `/root/fleetly-data-backup-<date>.tar.gz` + `/var/lib/fleetly.old-<date>`;systemd unit 保留可回原生形态 |
