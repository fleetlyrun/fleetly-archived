# ADR-0012: git 部署：CD 内置、CI 不自研；webhook+拉源保留、push 收包移除

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1/v0.2 落地）；push 面移除已接受（2026-09-29 用户拍板，已落地） | 决策 2026-09-17；push 定位评估 2026-09-29（未裁决）；裁决 2026-09-29；收录 2026-09-29 | [架构](../design/2026-09-17-architecture.md)（§1.2/§7）、[delivery-pipeline](../design/2026-09-17-delivery-pipeline.md)、[console IA](../design/2026-09-29-console-ia-redesign.md)（Deploy 卡单方式 pills） |

## 背景

CI/CD 边界划在哪：自建 CI 是重栈红线；而「git 部署」在开源 PaaS 生态里两条路径处境相反（push-to-deploy 罕见、git-as-source+webhook 是主流主路径）。

## 决策

1. **CD 内置、CI 不自研**：CI 归 Git 托管方（GitHub Actions 等），平台仅做 webhook 状态门禁集成。
2. **触发轨（internal/gitserver）收敛为单轨**（2026-09-29 裁决前为双轨）：
   - **webhook+拉源（保留，主路径）**：强制签名+防重放（delivery ID）+幂等去重（v0.1），v0.2 异步化（202+有界队列）；拉源三认证 none/https_token/ssh_key；
   - ~~git push（SSH 收包）~~：**移除**——SSH git server、host key、git_keys 用户公钥、post-receive 钩子与 hook token 全链退场（迁移 00029 物理清理；proto 侧 DeployFromGit RPC/gitkeys 服务删除、git_remote_hint/git_ssh_fingerprint 字段 reserved）。dokploy/coolify/caprover 这一代 Docker-centric 开源 PaaS 均不做 push-to-deploy——它要求平台养一套与 Git 托管方重复的身份/凭证面，且 bare 仓库是管理节点上的可变状态。
3. **SSH 拉源 TOFU（accept-new）**+首连指纹落审计（D-W0-8）；v0.3 升级指纹披露+CLI 核对；多用户后仍不建钉位基础设施。（原 `git.hostkey_changed` 事件与 `git fingerprint` CLI 属 SSH 收包面的 host key 台账，随面退役；拉源的 known_hosts TOFU 是独立一套，保留。）
4. **砍线口径（2026-09-29 定案，修正本 ADR 初稿的表述）**：砍的是 **SSH 收包半**（SSH 服务/hostkey/git_keys/post-receive 钩子/hook token）；**bare 仓库不在砍线内**——它是 webhook 拉源的 fetch 落点与 compose 字节的唯一读取源（`git show <sha>`），属保留面的承重结构。

## 后果

- Console Deploy 卡仍呈现 Compose│Git│Webhook 单方式 pills；Git pill 语义收敛为「拉源配置」（push remote 行退场）。
- push 面唯一不可替代场景（无托管仓库/不想交仓库凭证的本地直推）不再覆盖；如未来回翻，按「建议重开 ADR-0012」走。
- per-app Autodeploy 开关（console-ia-redesign §8 挂账）落在 webhook 入队路径，不受本裁决影响。

## 关联

ADR-0008（对标纪律：webhook+拉源=dokploy 同款主路径）、torchwood 线（拉源=迁移主路径）。
