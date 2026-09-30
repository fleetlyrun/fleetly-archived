# deploy — 安装器与引擎门禁（T2.1）

一条命令在干净 Linux VPS（amd64/arm64，root）上装出可运行平台：引擎门禁 →
获取二进制 → 隐式 `docker swarm init` → systemd 自启 → 安装报告。

设计依据（只读）：architecture §2.6（运行时 = 单节点 Swarm，安装时隐式
init）、§4.2（引擎门禁 / 安全默认基线 / 底座端口加固）、delivery-pipeline
§2.4 + remediation S14/H15（`curl | sh` 的完整性基线：checksum 必验 +
签名双轨必验其一——cosign bundle 轨优先，openssl 内嵌公钥轨兜底，降级
即死）。

## 文件

| 文件 | 作用 |
| --- | --- |
| `install.sh` | 安装器（POSIX sh；systemd unit 经内嵌同源 heredoc 生成） |
| `uninstall.sh` | 卸载器（应用数据默认保留，`--purge` 才删） |
| `upgrade.sh` | 平台自升级（T2.23；升级双轨的 fleetlyd 轨——热备快照 + 原子换件 + 失败自动回退，应用不停） |
| `fleetlyd.service` | systemd unit 参考模板（与 install.sh 内嵌 heredoc 逐字一致，test-install.sh 做 diff 防漂移） |
| `test-install.sh` | dind 内安装验收套件（A1-A9） |
| `run-dind-test.sh` | 宿主编排：交叉编译 → 起 dind → exec+stdin 注入 → 跑安装套件 → 清理 |
| `test-upgrade.sh` | dind 内升级验收套件（U/S 两段：正常升级零停应用 + 坏件自动回退 + 备份链 + S4 schema 感知回退） |
| `run-upgrade-test.sh` | 升级套件宿主编排（vA/vB 两份版本串二进制 + vC schema-skew 变体 + 探针应用交叉编译 → dind → 套件 → 清理） |
| `testdata/probeapp/` | 升级 E2E 探针应用（serve/hc/watch 三模式；scratch 镜像零 registry 依赖） |
| `testdata/release-test-key.pem` / `release-test.pub.pem` | openssl 轨**测试**签名密钥对（A11 断言用；仅测试——生产密钥另持，见下文「release 签名密钥（openssl 轨）」） |
| `Dockerfile.fleetlyd` | fleetlyd 容器形态（T2.24；多阶段构建 + alpine 运行层，入口 fleetlyd——可选运行形态，见下文） |

## 一条命令安装

```sh
# 最新 stable（缺省形态；release 制品链落地前 GitHub releases 尚无产物）
curl -fsSL https://fleetly.dev/install.sh | sudo sh -

# 钉定版本
curl -fsSL https://fleetly.dev/install.sh | sudo sh - --version v0.1.0

# 离线 / 开发形态：直接使用本地已备好的二进制（跳过下载，仍打版本信息）
sudo sh install.sh --bin-dir ./dist
```

### 获取形态与完整性

| 形态 | 参数 | 完整性 |
| --- | --- | --- |
| latest stable | （无） | checksums.txt sha256 必验；签名双轨必验其一（cosign bundle / openssl 内嵌公钥），降级即死 |
| 版本钉定 | `--version vX.Y.Z` | 同上 |
| 离线/开发 | `--bin-dir <dir>` | 本地直取（自备完整性） |

下载形态的 release 契约（T2.24 制品链 + S14 双轨签名已落地
`.github/workflows/release.yml`）：release 附 `fleetly_<tag>_linux_<arch>.tar.gz`
（tar 包根下有 `fleetlyd`、`fleetly`；安装脚本**独立制品**不打进 tar——
`install.sh`/`uninstall.sh`/`upgrade.sh` 在 release 根）、`checksums.txt`
（`<sha256>  <文件名>` 行式，覆盖 tar + 三个安装脚本）、`checksums.txt.sig`
（**Sigstore bundle JSON**——cosign v3 keyless `sign-blob --bundle` 的唯一
产物形态，内嵌签名 + 证书 + Rekor 条目；另附 `checksums.txt.cert` 供
`--certificate` 验证形态）、`checksums.txt.sig.pem`（**openssl 轨 detached
签名**——`openssl dgst -sha256` 的裸 DER 签名，`.pem` 为产物链约定名；
S14/H15 落地，`FLEETLY_RELEASE_KEY` secret 配置后附带）与 `*.spdx.json`
SBOM（syft）。cosign 轨签名验证约束（keyless，GitHub OIDC）：
`--certificate-oidc-issuer https://token.actions.githubusercontent.com`、
`--certificate-identity-regexp ^https://github.com/fleetlyrun/fleetly/`。

**验签语义（S14/H15 双轨，降级即死）**：install.sh/upgrade.sh 对
`checksums.txt` 双轨验签——① cosign 在且 `.sig`（bundle）在 → cosign
verify-blob（上述 identity 约束，失败即中止）；② 否则 `.sig.pem` 在 →
`openssl dgst -sha256 -verify <脚本内嵌公钥> -signature ...sig.pem
checksums.txt`（openssl 在目标发行版近乎必装——干净 VPS 无 cosign 的常态
走这条轨）；③ 双轨全部不可验（如 cosign 缺席且 release 只附 bundle）→
**拒绝安装**（checksums 与产物同 origin，防不了 release 侧自一致重打包
投毒——这正是 H15 的信任落差，不再 warn 降级）；④ 仅 S14 之前的历史
版本（release 未附任何签名产物）保留带显著警示的兼容路径（upgrade.sh
侧对应 `--allow-nightly` 门）。签名产物**下载失败不等于未附**：非 404
的下载失败直接拒绝（fail closed）；`--skip-signature-verify` 仅调试用。

### release 签名密钥（openssl 轨，S14/H15）

openssl 轨的信任根是 install.sh/upgrade.sh 内嵌的 `FLEETLY_RELEASE_PUBKEY`
（PEM 公钥 + `fingerprint-sha256` 指纹注释）。签名侧私钥**不落仓库**，由
repo secret `FLEETLY_RELEASE_KEY` 承载；release.yml 在 secret 可用时对
`checksums.txt` 追加 `openssl dgst -sha256 -sign` 签名产物
`checksums.txt.sig.pem`（secret 缺席时跳过并警示——过渡期口径）。

**算法裁决：RSA-2048 + SHA-256（`openssl dgst` 轨）**。ed25519 更现代，
但 `openssl dgst` 不支持 Ed 系签名（Ed 系必须 `pkeyutl -rawin`，OpenSSL
1.1.1/3.x 参数面分裂；实测 OpenSSL 4.0.2 的 `dgst -sign` 对 ed25519 仍报
unsupported），而 `dgst -sha256 -sign/-verify` 自 OpenSSL 1.0.x 起全版本
（含 LibreSSL）命令面一致——消费端兼容面优先（方案冻结口径）。

首次配置（正式发布前必做；当前脚本内嵌的是 `deploy/testdata/` 测试密钥，
其私钥在仓库内，不能作为生产信任根）：

```sh
# 1) 本地生成密钥对（离线保管私钥；建议入密码管理器/保险库）。
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out fleetly-release.key.pem          # 私钥（机密，永不入仓库）
openssl pkey -in fleetly-release.key.pem -pubout \
  -out fleetly-release.pub.pem          # 公钥（嵌入安装脚本）

# 2) 计算指纹（DER 公钥的 SHA-256，写入脚本指纹注释行）。
openssl pkey -pubin -in fleetly-release.pub.pem -outform DER \
  | openssl dgst -sha256

# 3) 配置 secret：GitHub 仓库 Settings → Secrets and variables → Actions →
#    New repository secret，名称 FLEETLY_RELEASE_KEY，值 = 私钥 PEM 全文
#    （-----BEGIN PRIVATE KEY----- 到 -----END PRIVATE KEY----- 含边界行）。

# 4) 替换 deploy/install.sh 与 deploy/upgrade.sh 两处的
#    FLEETLY_RELEASE_PUBKEY 块（逐字一致）与 fingerprint-sha256 注释行，
#    随仓库提交。
```

release.yml 的 **embedded-pubkey gate**（verify gate C）会在 secret 已配置
时强制校验：脚本内嵌公钥/指纹必须与 secret 派生公钥配对——「配了 secret
忘改脚本」的必炸 release（消费端全部验签失败）会在 CI 被拦下；仓库侧由
test-install.sh 的 A11 断言守三份材料（install.sh / upgrade.sh /
testdata 测试密钥派生公钥）一致。

**轮换**：生成新密钥对 → 重复步骤 3/4（换 secret、换内嵌公钥与指纹）→
之后的 release 用新钥签名。旧 release 的历史 `.sig.pem` 用旧公钥仍可验；
新 release 遇到旧安装器 → 验签失败的报错文案指引用户升级安装器
（install.sh/upgrade.sh 的 die 文案已内置该指引）。

**测试密钥**：`deploy/testdata/release-test-key.pem` /
`release-test.pub.pem` 仅供 dind 验收（test-install.sh A11 用它签出 staged
release），明确标注仅测试用——生产签名密钥另持。

## 引擎门禁（不满足即拒绝，exit 1）

| 检查 | 判定 | 依据 |
| --- | --- | --- |
| 平台 | Linux + x86_64/aarch64；root | 安装器目标形态 |
| Docker Engine | `docker version --format '{{.Server.Version}}'` ≥ **29.8.1** | architecture §4.2（Docker 29.x 破坏史，下限是硬门禁） |
| iptables 后端 | 默认 `iptables` 为 legacy，**或** `iptables-legacy` 可用；nftables-only 拒绝并给修复提示 | architecture §4.2（nftables 暂不支持 Swarm 节点） |
| 基础工具 | docker、curl/wget、tar、sha256sum | 下载与健康等待 |

## 安装动作

1. **目录约定**：`/opt/fleetly/bin`（二进制 0755）、`/opt/fleetly/etc`
   （config.yaml + unit 参考副本）、`/var/lib/fleetly`（数据根 0700——
   SQLite/主密钥/git bare 仓库/构建缓存/日志/证书全落这里）；
   `/usr/local/bin/fleetly{,d}` 符号链接入 PATH。
2. **配置生成**（已存在则保留不覆盖）：HTTP 面 `0.0.0.0:8420`（可
   `--http-addr` 覆盖）、gRPC 只绑 `127.0.0.1:8421`、git SSH `0.0.0.0:8424`
   （对外提供 git push；关闭改 `127.0.0.1:8424`——config-example.yaml 注释
   口径）、数据路径全部显式指向 `/var/lib/fleetly`；其余键回落
   `internal/*` 各 `Config.Normalize` 的平台默认。
3. **隐式 swarm init**（已初始化则跳过）：advertise-addr 优先私网 IP
   （RFC1918；排除 loopback/link-local 与 docker*/br-\*/\*gwbridge\* 网桥
   接口地址），无私网才取公网 IP 并在报告**警示暴露面**（§4.2 底座端口加固；
   `--harden-firewall` 为后续版本能力占位，本阶段不改写 iptables）。
4. **systemd**：`After=docker.service` + `Restart=on-failure` +
   `WorkingDirectory=/var/lib/fleetly` + 适度加固（`ProtectSystem=strict` +
   `ReadWritePaths` 放行数据根、`PrivateTmp`、`NoNewPrivileges` 等，逐项
   注释见模板）。enable + start + `/healthz/liveness` 轮询（60s，失败输出
   systemctl status/journalctl 诊断）。无 systemd 环境 → `--no-systemd`
   明示跳过并打印手动启动命令（不出 unit、不启服）。

## 端口暴露面（安装报告逐项输出）

| 端口 | 用途 | 缺省绑定 | 暴露判定 |
| --- | --- | --- | --- |
| 8420/tcp | HTTP REST/gateway + healthz + Console | `0.0.0.0:8420` | PUBLIC（配合防火墙/反代） |
| 8421/tcp | gRPC（CLI/SDK 直连） | `127.0.0.1:8421` | loopback only |
| 8422/tcp | Traefik HTTP provider 配置端点 | `0.0.0.0:8422`（平台默认） | bearer token 强制 |
| 8424/tcp | git push(SSH) | `0.0.0.0:8424` | PUBLIC（安装报告明示；不需要时改回 127.0.0.1） |
| 80,443/tcp | Traefik 入口（host 模式） | 宿主端口 | PUBLIC（应用入口，预期） |
| 2377/tcp, 7946/tcp+udp, 4789/udp | Swarm 成员/VXLAN | advertise-addr | 私网 → LAN only；公网 → 报告红色警示 |

## 首启 bootstrap token

控制面首启（库内无任何 token 时）生成 bootstrap admin token 并写入
`<数据根>/bootstrap-token`（缺省 `/var/lib/fleetly/bootstrap-token`，0600；
**不打印进日志**——systemd 形态 journald 不再持久留存明文凭据）。随后
`FLEETLY_ADDR=127.0.0.1:8421 FLEETLY_TOKEN=$(cat /var/lib/fleetly/bootstrap-token) fleetly apps list`
验通；首次成功登录后删除该文件。文件已存在时首启不重复生成（幂等）。

## 升级（T2.23，升级双轨）

**fleetlyd 轨**（`upgrade.sh`，不停 Engine、应用不停）——八步原子序列：
① 预下载并校验（先下后停）→ ② 升级前热备快照（`kind=pre_upgrade`，经
CLI 调 daemon RPC，`verify_status=verified` 才继续，否则在停 daemon 之前
中止）→ ③ 应用健康基线（apps derived_state + 可选 `--probe-url` 采样）→
④ 停 fleetlyd（Swarm service 与 Traefik 独立于 daemon——应用路由继续
服务）→ ⑤ 原子换二进制（旧件存 `fleetlyd.previous`；F6/S20——mv 序列带
原子性兜底，任一步失败就地归位旧件再 die）→ ⑥ start + liveness
门 → ⑦ 升级后验证（版本号 + derived_state 与基线一致 + probe 200）→
⑧ 任一步失败自动回退 previous 并再验证，仍失败停在最诚实状态打诊断
（F6：回退段 stop 失败即 die，不 warn continue）。F5/S20：拉起旧件前经
只读子命令 `fleetlyd schema-version` 比对 DB schema 与旧件支持上限——
高于上限默认 die 并给三步恢复指引；`--auto-restore` 从本运行的 verified
pre_upgrade 快照自动恢复状态库后再拉起旧件。

```sh
sudo sh upgrade.sh --version v0.1.1        # 或缺省 latest stable（stable 渠道只接受带签名版本）
sudo sh upgrade.sh --bin-dir /tmp/new-bin  # 离线/开发形态
# 可选：--probe-url <url> --probe-host <host>（入口可达性采样）
#       --allow-nightly（显式接受无签名 nightly）；--skip-backup（红色警告，破坏原子保证）
#       --auto-restore（F5：回退遇 schema 高于旧件时，自动恢复 pre_upgrade 快照再回退）
```

**Engine/主机轨** = 冷备 + 维护窗口（先备份 + 停应用；有状态应用停机
如实告知）——操作手册与双轨口径见 `docs/runbooks/upgrade.md`，**禁止用
本目录脚本执行 Engine/主机升级**；`--force-recreate` 式升级被双轨口径
明令禁止（升级脚本对应用服务零操作）。

dind 验收（含 probe 零失败断言与坏件回退）：

```sh
sh deploy/run-upgrade-test.sh
```

场景：S1 正常升级（vA→vB：probe 全程零失败 = 应用不停 E2E、版本正确、
derived_state 不变、pre_upgrade 备份 verified）；S2 坏 vB'（截断 ELF）→
自动回退 vA、daemon healthy、probe 仍零失败；S3 备份链台账
（kind/verify_status、manifest 密钥指纹、密钥不在备份目录）；S4（F5/S20）
回退 schema 感知——vC 变体（注入迁移 00099）制造「新版本已应用迁移后回退
旧版本」的错配现场：无 `--auto-restore` 时 die + 三步人肉指引（且指引照做
可恢复）；`--auto-restore` 时从 verified pre_upgrade 快照自动恢复状态库后
拉起旧件（ROLLED BACK + auto-restore 报告行）。

## 卸载

```sh
sudo sh uninstall.sh           # 停服 + 删 unit/二进制/符号链接/配置
                               # /var/lib/fleetly 保留并明示
sudo sh uninstall.sh --purge   # 连应用数据一起删（不可逆）
```

swarm 底座不默认拆除——提示 `docker swarm leave --force` 由操作者显式执行
（后果：单节点 Swarm 拆掉、运行中的应用服务失去调度）。

## dind 验收（本阶段核心验收，主会话可复跑）

Git Bash（Windows）或 Linux shell 上一条命令：

```sh
sh deploy/run-dind-test.sh
```

编排内容：交叉编译 linux/amd64（`CGO_ENABLED=0`，可 `TI_SKIP_BUILD=1 +
TI_BIN_DIR=<dir>` 复用现成二进制）→ 特权 `docker:29.8.1-dind` 容器 →
exec+stdin 注入脚本与二进制（**禁用 docker cp**——Engine 29.x 宿主向特权
dind 会 exit 0 但文件不落盘，e2e/README.md 已知问题；二进制不 sed 剥 CR，
否则 ELF 被破坏会 segfault）→ 容器内跑 `test-install.sh` → 失败 dump 日志
→ 清理。

`test-install.sh` 断言组（详细清单见脚本头注释）：A1 `sh -n` 语法；
A2 `--bin-dir --no-systemd` 正路径安装（目录/二进制/配置/unit 同源/
符号链接/报告字段）；A3 隐式 swarm init（active + advertise 私网）；
A4 daemon 手动启动 liveness 200 + 8424/8421/8420 监听面 + SIGTERM 退出
码 0；A5 bootstrap token 从 `<数据根>/bootstrap-token` 文件读取（B5：
不进日志）→ CLI `apps list`；A6 重装幂等
（swarm 跳过 init、config 保留）；A7 假 docker（28.3.2）版本门禁拒绝；
A8 nftables-only shim iptables 门禁拒绝；A9 卸载（数据保留/明示 +
`--purge`）；A10 健康探测 host 推导；A11 openssl 轨双轨验签（staged
release + 假 curl 端到端：正例安装成功 / 篡改 checksums 验签 die /
双产物缺历史兼容 / 仅 bundle 且无 cosign 拒绝 / skip 位保留 + 内嵌公钥
三方一致性）。

amd64 运行验证即覆盖门禁主路径；arm64 交叉编译产物存在性由构建证明
（release.yml build matrix；arm64 QEMU 运行 smoke 成本高，T2.24 明确列
遗留，见 release.yml 头注）。

## 容器形态（T2.24，可选运行形态）

`Dockerfile.fleetlyd`：多阶段（golang:1.26-alpine 构建层，`CGO_ENABLED=0`
+ `-trimpath` + `-X main.version=<tag>`）→ alpine:3.22 运行层，入口
`fleetlyd`，数据根 `/var/lib/fleetly`（**host bind，非命名卷**——三适配
之一，见下）。缺省不传配置文件（回落内置默认；F3/S20——镜像内并无
`/etc/fleetly/config.yaml`，固定 `-c` 会让容器开箱 crash-loop）；需要显式
配置时挂配置卷并覆写参数：

```sh
docker build -f deploy/Dockerfile.fleetlyd \
  --build-arg FLEETLY_VERSION=v0.1.0 -t ghcr.io/fleetlyrun/fleetlyd:v0.1.0 .
# 缺省形态（内置默认配置；三适配缺一即坏——staging 2026-09-29 实证）：
docker run -d --name fleetlyd \
  --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/fleetly:/var/lib/fleetly \
  ghcr.io/fleetlyrun/fleetlyd:v0.1.0
# 可选：挂配置卷（键集见仓库根 config-example.yaml）：
docker run -d --name fleetlyd \
  --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/fleetly:/var/lib/fleetly \
  -v /path/to/config.yaml:/etc/fleetly/config.yaml:ro \
  ghcr.io/fleetlyrun/fleetlyd:v0.1.0 -c /etc/fleetly/config.yaml
```

三适配（ADR-0013，staging 实证）：**① `--network host`**——VL/VM 消费面
拨宿主回环 127.0.0.1:9428/8428（桥接容器里 127.0.0.1 是自己），8420-8424
随 host 网络直听无需 `-p`；**② 数据根 host bind**——平台把数据根下的
文件 bind 挂载进 swarm 任务（任务在宿主解析路径），命名卷宿主路径不存在
→ zot 任务 Rejected；**③ docker.sock 挂载**——管理面（root 等价权限的
明示取舍）。已知缺口（挂账）：buildkit 构建链要 exec docker，镜像刻意
不带 docker CLI → 容器形态构建面不可用，带 CLI 的变体产物（`<tag>-staging`
后缀）未落；部署既有镜像/其余功能面不受影响。

边界：**主形态仍是宿主二进制 + systemd**（install.sh）；容器形态挂宿主
docker.socket（容器内进程获得宿主 dockerd root 等价权限——与主形态同权限
口径的明示取舍）；release 轨道按 digest cosign 签名
（`ghcr.io/fleetlyrun/fleetlyd:<tag>`，linux/amd64 单平台——arm64 运行
形态走 tarball/安装器，多平台镜像列后续）。

## 已知边界（后续阶段）

- release 制品链已落地（`.github/workflows/release.yml`，T2.24）：tar +
  checksums + SBOM + cosign keyless 签名 + ghcr 镜像 + GitHub Release。
  install.sh/upgrade.sh 的 cosign 验签为 `--bundle` 轨（`verify-blob
  --bundle` + keyless identity 约束，见上方「验签语义」），release.yml
  gate A 以同款命令面验证签名产物；gate B（detached signature +
  `--certificate`）是同一签名材料的补充验证形态。
- `--harden-firewall`（自动 iptables 放行规则）为 architecture §4.2
  预留，本阶段只提示不写规则。
- unit 以 root 运行（需要 docker.sock；专用用户 + docker 组收敛属后续
  加固项）。
- release 首发实跑（打 tag 全链）与 nightly 全绿门禁接入 release 轨道
  属 T2.26 验收面。
