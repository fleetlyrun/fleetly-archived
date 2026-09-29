# ADR-0012: git 部署：CD 内置、CI 不自研；双轨触发保留

| 状态 | 日期 | 关联 |
|---|---|---|
| 已接受（v0.1/v0.2 落地）；**push 定位=待裁决** | 决策 2026-09-17；push 定位评估 2026-09-29（未裁决）；收录 2026-09-29 | [架构](../design/2026-09-17-architecture.md)（§1.2/§7）、[delivery-pipeline](../design/2026-09-17-delivery-pipeline.md)、[console IA](../design/2026-09-29-console-ia-redesign.md)（Deploy 卡单方式 pills） |

## 背景

CI/CD 边界划在哪：自建 CI 是重栈红线；而「git 部署」在开源 PaaS 生态里两条路径处境相反（push-to-deploy 罕见、git-as-source+webhook 是主流主路径）。

## 决策

1. **CD 内置、CI 不自研**：CI 归 Git 托管方（GitHub Actions 等），平台仅做 webhook 状态门禁集成。
2. **双轨触发都保留（internal/gitserver）**：
   - **git push（SSH 收包）**：平台自养 SSH git server、bare 仓库、post-receive→DeployFromCommit；
   - **webhook+拉源**：强制签名+防重放（delivery ID）+幂等去重（v0.1），v0.2 异步化（202+有界队列）；拉源三认证 none/https_token/ssh_key。
3. **SSH 拉源 TOFU（accept-new）**+首连指纹落审计（D-W0-8）；v0.3 升级指纹披露+`git.hostkey_changed` 事件+CLI 核对；多用户后仍不建钉位基础设施。
4. **待裁决（不得当作已定引用）**：2026-09-29 评估建议「push-to-deploy=便利层不再扩（唯一不可替代场景=无托管仓库/不想交仓库凭证，dokploy 覆盖不了的差异化）；若减负砍线画在 SSH 收包半（bare 仓库+hostkey+git_keys），webhook+拉源完整保留」——用户未拍板。

## 后果

- Console Deploy 卡呈现 Compose│Git│Webhook 单方式 pills，git pane 同时承载两轨配置。
- push 面的持续投入应待裁决结论，避免与 webhook+拉源平权排期。

## 关联

ADR-0008（对标纪律：push-to-deploy 是 dokploy 无而 fleetly 有的差异化）、torchwood 线（拉源=迁移主路径）。
