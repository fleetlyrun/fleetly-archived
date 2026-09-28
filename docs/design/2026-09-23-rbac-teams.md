# 团队与多用户 RBAC（用户/团队/项目）设计（v0.3 C 线）

| 状态 | 日期 | 关联 |
|---|---|---|
| **已冻结（裁决轮完成 2026-09-23：内裁四项 + 用户直裁四票；评审补充 D-W0-9 同名项目解析、D-W0-2 修订项目角色队内覆写形、D-W0-4 二修 app/库名 project 内唯一〔三段命名〕、D-W0-5 修订 fresh-install 前提〔无迁移负担〕——D-W0-1~9 全落）** | 2026-09-23 | [v0.3 规划](../plan/2026-09-23-v0.3-plan.md) §2 W0 / §3 六问；[架构文档](2026-09-17-architecture.md) §4.2 安全基线 / D21；[v0.2 观测设计](2026-09-22-observability.md)（terminal scope 先例） |

## 0. 输入与定位

v0.3 主线 C 票的专项设计（V3-1：先团队/RBAC 后生产深化）。用户补充需求原文归纳（2026-09-23，逐条对齐落点）：

| # | 需求 | 落点 |
|---|---|---|
| R1 | 支持用户注册登录；**第一个注册用户为系统管理员** | §2.1/§3.2（无用户窗口恒开 + is_platform_admin） |
| R2 | 可配置是否开放注册；系统管理员也可以添加用户 | §2.1（auth.registration + UsersService） |
| R3 | 用户注册后默认创建自己的 Team；可再创建 Team | §3.1（个人默认队 + 数量不限） |
| R4 | 可以邀请别人加入自己的 Team | §3.1（邀请链接，一次性 token） |
| R5 | Team 内角色区分（owner/admin/developer/viewer 类，具体可重设计） | §3.2（四档重设计 + 平台管理员分离） |
| R6 | Team 包含 Project；Project 是隔离 App/Database 等资源的单位 | §3.3/§4（project_id + 准入 + 网络层既有事实） |
| R7 | 不同 Project 的应用间不能互相访问 | §4.1（**v0.2 底座已结构性具备**——设计发现） |
| R8 | Project 是否再做用户角色（GitLab 两层形态）——**委托设计方裁决** | §3.3（裁决 D-W0-2：不做，单层，理由与预留） |

**前提修订（2026-09-23 用户澄清）**：现役部署仅一处 staging 且可清空重建——v0.2→v0.3 **无升级迁移负担**，本文档按净新增面取最优设计（认领/收编/NULL 窗口等升级兼容机制一律不设，见 §8 与 §13 D-W0-5 修订）。

横切红线沿用：单写点（新表全走 InTx）、默认捆绑预算 600MB（本设计**零新增常驻组件**）、契约纪律（proto 唯一真源 + scope fail-closed 登记制——**登记面 = 每 RPC 的 proto scope option 注解**〔IMPL-ARCH-J 自 scope.go 手工表搬家，用户裁决批准〕，运行时 map 启动期从 descriptor 生成；未登记 fail-closed 按 admin 拒与「改登记 = 改测试」descriptor-walk 守卫不变）、文档先行。

## 1. 现状底座（设计输入的事实核对，2026-09-23 读码结论）

- **认证**：`Authorization: Bearer` → tokens 表哈希比对（常量时间二次校验）→ scope 判定（`read ⊂ deploy ⊂ admin ⊕ terminal`，admin 蕴含全部；`internal/api/auth.go`）。未登记方法 fail-closed 按 admin 拒（scope 登记 = proto scope option 注解，IMPL-ARCH-J 后；运行时 `internal/api/scope.go` 启动期从 descriptor 生成）。Principal = {TokenID, Scopes}，无用户概念。
- **审计**：`audit_log` 表已在 v0.1 落地（actor / actor_token_id / action / target / result / error_code / request_id / diff_summary；事务内写、CHECK 约束 fail-closed、janitor 365d 常量留存、**读面缺失**）。actor 词表 human/ai_agent/system 已预留。
- **网络隔离（关键发现）**：每 app 专属 overlay `fleetly-<app>-net`（`naming.NetworkName`），Traefik 按需逐网附着（`internal/ingress/traefik.go:11` 拓扑注释 + attachNetwork 幂等增挂），**app 间 L3 互不可见在 v0.2 底座已成立**。平台网（fleetly-system / rustfs / metrics / victorialogs 内部网）仅平台组件挂接。唯一跨 app 连通通道 = E4 库网络 `fleetly-db-<name>-net`（引用方 app 部署时平台附加挂载）——**项目隔离的准入守门点收敛为此一处**（§4.1）。
- **git SSH host key**：服务端文件持久化（`git.host_key_file`，ensureHostKey 装载/生成）；客户端拉源 TOFU（accept-new）+ `git.hostkey_first_seen` 审计。指纹无披露面（FZ-12 现状，§13 Q4）。
- **依赖**：`golang.org/x/crypto v0.56.0` 已在（argon2id 可用，零新依赖）；迁移下一号 00018。
- **保留字**：app 名全局唯一 + 8 保留字（`internal/naming/naming.go`），服务/卷/secret/网络公式全部以 app 名为参数。

## 2. 身份与认证（W1 落地）

### 2.1 用户与注册

users 表（00018）：

```sql
CREATE TABLE users (
    id                 TEXT PRIMARY KEY,      -- ULID
    email              TEXT NOT NULL UNIQUE,  -- 小写归一
    password_hash      TEXT NOT NULL,         -- argon2id PHC 串
    display_name       TEXT NOT NULL DEFAULT '',
    is_platform_admin  INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    disabled_at        INTEGER
);
```

- **口令哈希**：argon2id（m=64MiB, t=2, p=1, keyLen=32, salt 16B 随机，PHC 编码存储，常量时间校验）。登录/注册按 email+IP 双键限流（复用 rateLimiter 形态，如 10 次/分钟）——防撞库，无验证码依赖（自托管 + 默认关注册，滥用面有限）。
- **注册窗口规则（R1/R2）**：`users` 表为空 → 注册**恒开**（否则无人能登录；fresh 安装指引：暴露公网前完成首用户注册——首注册者即平台管理员）；首个注册用户 `is_platform_admin=1`，同事务建个人 Team + 默认 Project。users 非空后由 `platform_settings` 行 `auth.registration`（open|closed）管辖，**缺省 closed**；平台管理员可运行时切换（admin API + Console 开关，写审计）。
- **平台管理员添加用户（R2）**：`UsersService.CreateUser`（email + display_name + 临时口令，明文仅一次性返回——与 RevealDatabaseCredentials 同口径的 admin 面信任）；`ResetUserPassword` 同形（**v0.3 无 SMTP，找回口令 = 平台管理员重置**，诚实记录）；`DisableUser` 即吊销其全部会话与 PAT（认证路径联动拒认）。
- 无 email 验证（无邮件依赖；W4 Email 通道落地后挂账可选）。display_name 缺省取 email 本地部分。

### 2.2 会话（Console 浏览器面）

sessions 表（00018）：`id / token_hash / user_id / created_at / expires_at / last_seen_at`（last_seen 节流盖写，A2 同款）。

- Cookie `fleetly_session`：HttpOnly + SameSite=Lax + Path=/，Secure 随 TLS 模式；值随机 32B，库存 sha256。
- TTL：**7 天滑动 + 30 天绝对上限**（config `auth.session_ttl_hours` 可调）。注销 = 删行；「全部注销」= 删该用户全部行。
- **CSRF 口径**：SameSite=Lax 阻断跨站 POST 携带 cookie，v0.3 以此为防线（自托管单org 面）；double-submit token 挂账（§14）。
- 网关形态：gateway 把 Cookie 头透传 metadata，Authenticator 增加 session 分支（Bearer | Cookie 双凭据形态）；REST `/v1/auth/*` 进鉴权豁免名单（Register/Login/Ping 族），其余不变。

### 2.3 PAT 与既有 token 兼容

tokens 表加列：`user_id`（NULL = 平台机具令牌，**bootstrap token 及存量 token 语义不变**）、`project_id`（NULL = 不绑定）。

- **PAT 有效权限 = min(token scopes, 用户在目标 project 的角色蕴含)**——双门（§4.2），token 只能收缩不能放大。
- CreateToken 校验声明 scopes ⊆ 用户可达集（防呆非防险——角色门仍是硬边界）。
- **机具令牌（user NULL）**：v0.3 的**设计语义**（非兼容残留）——平台管理员显式创建的平台级凭据（CI/CD、基础设施自动化），全库 admin 等价，可带 team/project 绑定收缩。**bootstrap token 生命周期收敛**：零用户窗口的桥梁凭据（首启生成语义沿用），**首用户注册事务内自动吊销**——目的达成即死，不留常驻后门（fleetly-bootstrap-token 挂账项就此收口）。
- **TokensService 语义迁移（W2）**：CreateToken/ListTokens/RevokeToken 从「admin 全局面」改为「用户自服务面」——登录用户管自己的 PAT；平台管理员可看全部、可建平台级机具令牌。scope 登记随迁（纪律：改登记 = 改测试；登记面现为 proto scope option 注解，IMPL-ARCH-J）。
- **GitKeys 迁移用户化（W2）**：git 公钥表加 user_id；AddGitKey 自服务（登录用户加自己的 push key），SSH push 按署名用户入审计 actor。

### 2.4 CLI 登录与上下文

- `fleetly auth login`：指引在 Console 创建 PAT 后**粘贴**（device flow 挂账 §14）；`auth status / logout`。
- `~/.fleetly/config.yaml` 增加当前 team/project 上下文（apps list 等按上下文过滤；`--team/--project` 显式覆盖）。现有 `FLEETLY_TOKEN` 环境变量路径不变。

## 3. 团队、角色与项目（W2 落地）

### 3.1 Team 与成员/邀请（R3/R4）

```sql
CREATE TABLE teams (
    id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE,  -- [a-z0-9]{2,32} 单词制全局唯一（D-W0-4：slug 进底座命名公式,禁连字符=拼接无歧义;8 保留字守前缀族——E_TEAM_SLUG_RESERVED）;slug 不可变（UpdateTeam 仅改显示名）
    name TEXT NOT NULL, created_by TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE team_members (
    team_id TEXT NOT NULL, user_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('owner','admin','developer','viewer')),
    created_at INTEGER NOT NULL,
    UNIQUE (team_id, user_id));
CREATE TABLE team_invites (
    id TEXT PRIMARY KEY, team_id TEXT NOT NULL, email TEXT NOT NULL, role TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE, expires_at INTEGER NOT NULL,   -- 7d
    created_by TEXT NOT NULL, created_at INTEGER NOT NULL,
    accepted_at INTEGER, revoked_at INTEGER);
CREATE TABLE projects (
    id TEXT PRIMARY KEY, team_id TEXT NOT NULL,
    slug TEXT NOT NULL,  -- [a-z0-9]{2,32} 单词制、不可变（D-W0-4 二修：进底座命名公式三段第二位）;team 内唯一
    name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL, UNIQUE (team_id, slug));
CREATE TABLE project_members (          -- D-W0-2 修订：队内覆写形（§3.3）
    project_id TEXT NOT NULL, user_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin','developer','viewer')),
    created_at INTEGER NOT NULL,
    UNIQUE (project_id, user_id));      -- 仅限团队成员（写入校验+移出团队联动清理）
```

- **注册默认建队（R3）**：注册成功即在「个人 Team」下落位——slug 取 email 本地部分归一（剥离 `.`/`-`/`_` 等分隔符压成单词，冲突加 `2` 起序号），owner 一人；每个项目落进个人队默认 Project `default`（无项目参数的首次 Deploy 缺省进它，可显式指定）。Team 数量与项目数量 v0.3 不设限（商业分界见 §13 Q3）。
- **owner 恒在位**：最后一名 owner 不可被移除/退出/降级（操作拒绝，E_TEAM_LAST_OWNER）；team 删除 = owner 专属两段式 confirm + **需项目已空**（资源先迁走或删光，不做隐式级联——与 app 删除破坏性纪律对齐）。
- **邀请（R4）**：owner/admin 可邀（邀 owner 角色 = owner 专属；所邀角色不得高于邀请者自身）。链接 `console/auth/invite?token=…` 一次性（token sha256 入库，7 天过期，可 revoke）：已注册→登录后 accept；未注册→注册即自动 accept。**无 SMTP：邀请链接页面直出供复制**（W4 Email 通道后可选发送）。accept 前可调整角色。

### 3.2 角色（R5 重设计：四档 + 平台管理员分离）

**裁决 D-W0-3**：团队角色四档，语义直接对齐既有 scope 链（既有方法级 scope 映射不动〔登记面 = proto option，IMPL-ARCH-J 后〕，角色是 scope 之上的**归属解析层**）：

| 角色 | 蕴含资源权限 | 对齐 scope | 说明 |
|---|---|---|---|
| viewer | 全队项目只读：app 状态/日志检索与直播/事件/部署与构建台账/库投影（脱敏）/域名状态 | read | 观察者 |
| developer | viewer + 部署/回滚/取消、env 写、cron 触发、漂移收敛、DeployFromGit、**Web 终端** | deploy + terminal | 终端给 developer 的理由：deploy 能力 = 已可发布任意镜像（代码执行面早已持有），终端不新增信任面；v0.2 的 terminal **独立 token scope 保留**（机具最小权限），仅人类角色按角色蕴含放开 |
| admin | developer + 资源破坏性与敏感面：app 删除、env 明文读、secrets 写、构建触发（H14 宿主目录信任边界）、库全生命周期（create/delete/rotate/reveal/restore）、域名管理 | admin（资源面） | 团队资源全权 |
| owner | admin + 团队管理：成员/角色/邀请、项目创建与删除、team 改名与删除 | — | 队伍终极责任人 |
| 平台管理员（`is_platform_admin`，用户标志非角色） | 平台面：用户管理/注册开关/节点/S3/通知端点/TLS/日志后端与 metrics 模式等全局设置/平台备份/审计读（W3）/未认领资源认领。**全团队项目只读**（support 视角）+ 认领迁移权；**不代团队做写操作**（写操作仍需入队拿角色——职责分离，审计 actor 干净） | 平台面 | 首注册用户 + 显式授予 |

权限矩阵（方法级；既有 scope 映射〔proto scope option，IMPL-ARCH-J 后〕的资源面抽检）：

| 面 | viewer | developer | admin | owner | 平台管理员 |
|---|---|---|---|---|---|
| app 读/日志/事件/部署台账 | ✓ | ✓ | ✓ | ✓ | ✓（只读全域） |
| 部署/回滚/env 写/cron/终端 | — | ✓ | ✓ | ✓ | — |
| app 删除/env 明文/secrets/构建触发 | — | — | ✓ | ✓ | — |
| 库生命周期/凭据 reveal/备份恢复 | — | — | ✓ | ✓ | — |
| 项目创建/删除 | — | — | — | ✓ | — |
| 成员/角色/邀请 | — | — | — | ✓ | — |
| 用户管理/注册开关/节点/S3/通知/全局设置 | — | — | — | — | ✓ |
| GetSystemStatus/ListNodes | ✓（透明度例外：任何已认证 read） | ✓ | ✓ | ✓ | ✓ |
| 审计读（W3 落） | — | — | — | — | ✓ |
| SearchMetrics（PromQL 透传） | — | — | — | — | ✓（跨项目标签无法按 project 收口——诚实 fail-closed，放宽挂账 §14） |
| SearchLogs | app 约束强制（§4.2） | ✓ | ✓ | ✓ | ✓ |
| WatchEvents | app 主体按项目过滤；系统事件任何已认证 read | | | | |

### 3.3 Project 层角色（R8；D-W0-2 修订 2026-09-23：做队内覆写形）

**修订裁决 D-W0-2（用户直裁 2026-09-23，成本复盘后）**：Project 层角色随 W2 一起做，取**队内覆写形（B 形）**——GitLab 全量形（A 形：跨团队 outsiders + 可见性级联 + 项目级邀请）继续挂账。初裁「不做」的理由中否 A 的部分继续成立（成本全局、双来源权限难归因）；B 形以「可见性零级联」绕开成本大头，且 Console 项目页与 00018 迁移 W2 正在写，顺手成本 ≈ +3~4 人日，低于事后翻新（5~6 人日）。

**B 形语义**：

- `project_members(project_id, user_id, role, created_at, UNIQUE(project_id, user_id))`——**仅限已是团队成员的用户**（写入时校验；移出团队时联动清理其在该团队全部 project_members 行）。
- **有行则覆写、双向生效**（团队 developer 在某项目降为 viewer；团队 viewer 在某项目升为 developer）；**无行则用团队角色**；role ∈ {admin, developer, viewer}——**owner 不可覆写**（owner 是团队级概念，恒在全部项目保有 owner 权）。
- **可见性零级联**：可见项目集仍 = 团队归属（降权成员仍能只读团队全部项目——「对团队成员完全隐藏某项目」= A 形的私有项目，挂账 §12）。
- 管理：团队 admin/owner 经 ProjectsService 成员面增删改（**覆写角色不授予成员管理权**——权限怪圈防线：项目内被覆写为 admin 者也不能改任何覆写行）；审计与事件 `project.member_role_changed / project.member_removed`。
- 解析：ResolvePermission 先查 project_members、无行走 team_members（§4.2，仍是单点）。

### 3.4 资源归属与存量迁移

- `apps.project_id` / `databases.project_id` 与各自 `team_id` 列，自 00018 起 **NOT NULL**（净新增面，无存量行——fresh-install 前提见下条）。新建资源（首次 Deploy / CreateDatabase）必须携带 project（缺省 = 当前上下文个人队默认项目）。
- **无迁移负担前提（裁决 D-W0-5 修订，用户澄清 2026-09-23「现役仅一处 staging、可清空重建」）**：v0.3 为 **fresh-install 版本**——不设 v0.2→v0.3 升级路径，认领/收编/NULL 窗口等兼容机制全部不建（§8）；staging 演练按清空重建执行。资源改派（MoveApp / MoveDatabase，平台管理员）保留为日常整理面（改派只动归属，不触底座名，§4.3）。
- **同名项目跨团队允许（裁决 D-W0-9，2026-09-23 用户评审补充）**：projects 唯一性维持 `UNIQUE(team_id, slug)`——「每个团队各有 default/prod」是自然心智（GitLab group 命名空间同构），**不取全局唯一**（否则 `default` 即稀缺，与注册默认项目设计直接冲突）。引用解析规则：**限定形 `team-slug/project-slug` 恒可解析**；裸名仅在解析域内唯一时可用（解析域 = 调用方可见项目集；CLI 带上下文时 = 上下文团队内），歧义 → `E_PROJECT_AMBIGUOUS`（错误文案列出候选 `team/project` 供限定）；CLI 上下文存储、Console 路由、审计与事件 target 一律用 ID（`project:<id>`，免疫重名），展示层跨团队视图（平台管理员列表/审计页）显示限定形。
- **App/库名唯一性降为 project 内唯一（裁决 D-W0-4 二次修订 2026-09-23：初裁全局唯一 → 一修 team 内唯一 → 二修用户裁定 project 段拼进命名）**：apps/databases 增 `project_id`、`team_id`（冗余：角色解析与团队面列表加速）列 + `UNIQUE(project_id, name)`——**每个项目各有自己的 `web`/`api`/`db`**（GitLab 形态完成；prod 与 dev 项目同名应用天然合法）。底座命名三段 `team-prj-app` 承载全局唯一（公式表见 §4.3）。代价如实：**MoveApp/MoveDatabase 跨项目改派 = 换名重部署**（无卷服务建新删旧近零中断、有卷短暂停机窗口——罕见管理动作，GitLab 项目转移/GitHub 仓库转移换路径同惯例；跨团队改派另须过目标项目唯一性检查）。app/库名引用解析同 D-W0-9：限定形 `team/prj/app`、`team/prj/db` 恒可解析，裸名仅解析域内唯一（CLI 上下文钉 team+project 后裸名无歧义；歧义 `E_APP_AMBIGUOUS` 列候选）。
- 域名绑 app 不变（域名全局唯一性天然跨项目不撞）。

## 4. 隔离与准入（R6/R7 的执行面）

### 4.1 网络层（现状即隔离——设计发现）

- 每 app 专属 overlay + Traefik 逐网附着 + app 间 L3 互不可见：**跨 Project 应用互访在 v0.2 底座已被结构性阻断**，本项目零新增网络机制。
- **命名三段 team-prj-app（D-W0-4 二修）**：全局唯一由 team·prj 段承载（公式表见 §4.3）；同名项目（D-W0-9）在底座命名零冲突（project slug 单词制 + team 段限定）。隔离执行者仍是 **app 级**专属网络（比项目级更细：同项目 app 默认亦不互通）；**不做「项目网络」（fleetly-prj-* 形态）**——同项目默认互通会改变默认隔离 posture；同项目 app 互访若有需求，将来以显式 opt-in 挂账（§12）。
- **E4 库网络是唯一跨 app 连通通道**，准入守门（W2 增补，v0.2 单操作员无此校验）：部署受理时解析 app 引用的库实例，校验 `db.project_id == app.project_id`，跨项目 → 拒绝（E_DB_PROJECT_MISMATCH，部署失败带明确文案）。
- 平台网挂接面（traefik↔fleetly-system/rustfs 等）不随项目暴露，维持平台组件专属。

### 4.2 API 双门与解析单点

鉴权链扩展（auth.go 拦截器内，顺序不变）：

1. **token scope 门**（现有，逐字不动）：Bearer/session → Principal 扩展为 `{TokenID, UserID, SessionID, Scopes}`。
2. **角色门（新）**：仅资源方法（apps/databases/domains/env/secrets/builds/cron/logs/exec 资源面）触发——请求里的资源名 → 行 → project_id → `ResolvePermission`（先查 project_members 覆写行、无行走 team_members，§3.3）；机具令牌（user NULL）资源面维持全库（legacy 兼容）；user 会话/PAT 按角色蕴含判定方法所需层级（read/deploy/admin 映射自 §3.2 矩阵）。解析收口单点 `ResolvePermission`，fail-closed：解析不出（含 project/team 归属缺失）即拒。
3. **平台面方法**（System/Nodes/S3/Notifications 设置/Users/审计）：user principal 要求 is_platform_admin（§3.2 透明度例外除外）；机具令牌维持 scope 门现状。

- **列表过滤**：ListApps/ListDatabases/ListDeployments 等按「用户可见项目集」过滤 + `?project=` 收窄；机具令牌与平台管理员全库。
- **SearchLogs 强制 app 约束**：非平台管理员的 LogsQL 查询必须含 `app="team/prj/app"` 三段限定形流选择器（标签值同口径，§4.3；选择器抽取校验；无选择器/多 app 跨团队 → 拒绝并指引）。SearchMetrics 同能力做不到（任意标签选择器），v0.3 平台管理员专属（§3.2 矩阵注）。
- **WatchEvents 过滤**：app 主体事件按可见项目集服务端过滤；系统事件（节点/平台组件）任何已认证 read 可见。
- CLI/API/Console 三面同一拦截器与解析函数，无旁路。

### 4.3 底座命名公式修订（D-W0-4 修订的执行面）

**公式表（v0.3 起；team/prj = 团队与项目 slug，均单词制 [a-z0-9]{2,32} 且不可变）**：

| 对象 | v0.2 公式 | v0.3 公式 | 说明 |
|---|---|---|---|
| Swarm 服务名 | `fleetly-<app>-<service>` | `fleetly-<team>-<prj>-<app>-<service>` | 全局唯一由 team·prj 段保证（app 名 project 内唯一） |
| app 网络 | `fleetly-<app>-net` | `fleetly-<team>-<prj>-<app>-net` | 同上 |
| app secret | `fleetly-<app>-<name>-<hash8>` | `fleetly-<team>-<prj>-<app>-<name>-<hash8>` | 内容寻址，MoveApp 换名随重部署自然发生（新名创建+旧名清扫） |
| app 卷 | `fleetly-<app>-<key>-<appid8>` | **不变** | appid8（app ULID 前 8 位）已天然全局防撞——**零卷迁移/零数据搬移** |
| cron job | `fleetly-cron-<app>-<svc>-<ulid8>` | `fleetly-cron-<team>-<prj>-<app>-<svc>-<ulid8>` | 前缀族不变（IsCronJobName 沿用） |
| 库服务/网络 | `fleetly-db-<name>-<service>` / `fleetly-db-<name>-net` | `fleetly-db-<team>-<prj>-<name>-<service>` / `fleetly-db-<team>-<prj>-<name>-net` | 前缀族不变（IsDbServiceName 沿用） |
| 库卷 | `fleetly-db-<name>-<key>-<id8>` | **不变** | id8 同上天然防撞 |
| 路由键 | `fleetly-<app>-<svc>` | `fleetly-<team>-<prj>-<app>-<svc>` | traefik 动态配置键空间；access-log RouterName 反解为 spec 候选集匹配，公式变更内部消化 |

- **label 集 +`fleetly.team` / `fleetly.project`**（服务创建时写入；对账/清扫/管理查询的识别面）。架构与 state-model 的公式表、naming 表驱动测试随 W2 命名落地票同步修订（文档先行纪律；命名三段化属 W2 归属管道，见规划 §2）。
- **保留字迁移**：8 保留字（cron/db/dbjob/rustfs/registry/acme/metrics/victorialogs）从 app 名清单迁到 **team slug 清单**——app 名从此不再紧邻 `fleetly-` 前缀（结构性安全，E_APP_NAME_RESERVED 退役）；team slug 顶到该位置必须守前缀族与平台固定名（E_TEAM_SLUG_RESERVED）。project 段居第二位不邻前缀族（`fleetly-acme-db-…` 撞不上 `fleetly-db-`）——不新增保留约束。
- **已知段内歧义（v0.2 既有类，如实记录）**：同团队 `app=a, service=b-c` 与 `app=a-b, service=c` 推导同名服务——docker 创建失败显性暴露（非静默串线），admission 预检挂账（§12）；跨团队拼接歧义由 team slug 单词制结构性消灭。
- **流标签口径**：日志流标签 `app` 的值改为三段限定形 `team/prj/app`（ingest 写入与 SearchLogs 校验同口径，D-W0-9 解析规则复用）；metrics 容器/服务名随公式自然分段化。

## 5. API 面（proto 增量，W1/W2 分批）

| 服务 | 方法（读面默认 read、写面按 §3.2 归属） | 波次 |
|---|---|---|
| AuthService | Register / Login / Logout / LogoutAll / Me（含团队与角色投影）/ AcceptInvite / GetRegistrationState（登录页开关注册入口） | W1 |
| UsersService（平台） | ListUsers / CreateUser / DisableUser / EnableUser / ResetUserPassword / GrantPlatformAdmin / RevokePlatformAdmin / SetRegistration（open\|closed） | W1 |
| TeamsService | CreateTeam / ListTeams（我所在 + 平台全量）/ GetTeam / UpdateTeam / DeleteTeam；ListMembers / SetMemberRole / RemoveMember；CreateInvite / ListInvites / RevokeInvite | W2 |
| ProjectsService | CreateProject / ListProjects / GetProject / UpdateProject（slug 不可变）/ DeleteProject；ListProjectMembers / SetProjectMemberRole / RemoveProjectMember（队内覆写面，团队 admin/owner，§3.3）；MoveApp / MoveDatabase（资源改派面，平台管理员；**改派 = 换名重部署**〔D-W0-4 二修〕，跨团队改派须过目标项目唯一性检查） | W2 |
| AuditService | ListAudit（actor/action/时间/result 过滤 + 分页，平台管理员）；CLI `fleetly audit export --csv`（D-W0-6 读面） | W3 |
| SystemService 增量 | GetSystemStatus 增 git SSH host key SHA256 指纹字段（FZ-12 披露面，D-W0-8） | W3 |
| 既有资源 API | Deploy/CreateDatabase 请求增 project 字段（裸名或 `team/project` 限定形，D-W0-9 解析规则）；List* 增 project 过滤（同解析规则）；TokensService/GitKeysService 语义随迁（§2.3） | W2 |

纪律：新方法全部带 proto scope option 注解登记（fail-closed 兜底不变；登记面 = proto，IMPL-ARCH-J 自 scope.go 手工表搬家——唯一人类编辑点）；错误码按注册表现状（新增 E_TEAM_LAST_OWNER / E_INVITE_INVALID / E_DB_PROJECT_MISMATCH / E_PROJECT_AMBIGUOUS / E_APP_AMBIGUOUS / E_TEAM_SLUG_RESERVED 等进 errcode 注册表；E_APP_NAME_RESERVED 随保留字迁移退役〔§4.3〕）。

## 6. 审计与事件衔接（W3 铺垫）

- **actor 增维**：AuditEntry.Actor 对用户操作填 `user:<id>`（Console 读面解析 email 展示）；ActorTokenID 保留（PAT 面）；会话操作带 request_id 关联。登录失败入审计（result=error，不落口令）。
- **新动作**：`auth.registered / auth.login / auth.login_failed / auth.logout`、`user.created / disabled / password_reset / platform_admin_granted`、`team.created / deleted / member_added / member_role_changed / member_removed / invite_created / invite_accepted / invite_revoked`、`project.created / deleted / member_role_changed / member_removed / app_moved`、`auth.registration_changed`。
- **事件**（只增注册表）：`user.registered / team.created / team.member_changed / invite.accepted / project.created / project.deleted / project.member_changed`（metadata-only，零 secret——通知反环路红线沿用）。
- **留存与读面（裁决 D-W0-6）**：留存收敛为 platform_settings `audit.retention_days`（默认 90，现 365 常量改为缺省值，janitor 消费设置）；读面 = Console 审计页（平台管理员，过滤/分页）+ CLI 导出 CSV。
- **FZ-12（裁决 D-W0-8）**：host key 文件持久化现状保持；GetSystemStatus 披露 SHA256 指纹；host key 装载时对比存量指纹、变更（文件重建/换钥）发 `git.hostkey_changed` 事件 + 审计；CLI `fleetly git fingerprint` 直出指纹；known_hosts 钉定为客户端文档指引（不代管下发）。

## 7. Console 信息架构

- **登录页**：邮箱+口令主形态；开放注册时出注册入口（GetRegistrationState 驱动）；PAT 粘贴登录收进「命令行/高级」折叠区（运维直连保留）。邀请链接页：accept / 注册并 accept 两分支。
- **顶栏**：团队切换器 + 项目切换器（AppsPage 等按项目过滤）；用户菜单（PAT 页 / 退出）。
- **设置**：团队成员与角色、邀请管理（链接直出复制）；个人 PAT 页（创建时显式 scope + project 绑定 + 明文一次性展示）。
- **平台管理员**：用户管理、注册开关、（W3）审计页与 git SSH 指纹展示（system 页）。
- data-testid 锚点延续既有清单纪律（新增页面同规格登记）。

## 8. 迁移与兼容（fresh-install 版本，D-W0-5 修订）

- **前提**：现役部署仅一处 staging 且可清空重建（用户澄清 2026-09-23）——v0.2→v0.3 **不提供升级路径**，不为假想安装者保留兼容面；v0.2.x 对外为终点版，未来若出现真实外部安装者再评估升级专项。staging 演练按**清空重建**执行。
- **00018 一号迁移（净新增面）**：新表 users / sessions / teams / team_members / team_invites / projects / project_members（队内覆写，§3.3）；apps/databases 增 project_id、team_id（**终态 NOT NULL**——实现切分：00018 建列〔可空：W1 部署路径未接归属，SQLite UNIQUE 对 NULL 互异不阻塞〕、W2 命名/归属管道落地时 00019 表重建收紧）与 `UNIQUE(project_id, name)`（D-W0-4 二修，随 00018 建索引）；tokens 增 user_id、project_id；git_keys 增 user_id；索引：team_members(team_id/user_id)、projects(team_id)、project_members(project_id)、apps(project_id/team_id)、databases(project_id/team_id)、tokens(user_id)、sessions(user_id/expires_at)。外键关系应用层维护（与既有表一致，SQLite 不开硬约束）。迁移历史保持追加制（fresh 安装顺序跑全链，毫秒级——不做基线重写）。
- **命名公式（§4.3）自 v0.3 首次部署即新形**——无旧名对象、无收编 duty；CLI/Console 服务名展示随公式；**既有 e2e 脚本中 `fleetly-<app>-<svc>` 形服务名断言按 fixture 团队+项目前缀清扫**（W2 票内完成，验收 = 全量 e2e 绿）。
- **事件 golden** 只增不改（新事件 metadata-only）。
- **e2e 新增**：`auth.sh`（注册/首用户=平台管理员+个人队默认项目/注册开关/登录注销/限流/PAT 自服务/口令重置/**bootstrap token 首用户注册即吊销**）、`rbac.sh`（角色矩阵抽检：viewer 拒部署、developer 开终端拒 env 明文、跨项目库引用拒绝、**项目覆写抽检**〔团队 developer 某项目降 viewer 拒部署 / 团队 viewer 某项目升 developer 可部署 / owner 恒不覆写 / 移出团队联动清覆写〕、MoveApp 改派后归属与权限随迁（含换名重部署断言）、列表过滤、机具令牌全库）。
- **staging 演练要点**：清空重建（fresh v0.3 安装）→首用户注册（平台管理员+个人队；bootstrap token 即时吊销验证）→邀请第二用户→角色抽检→跨项目隔离验证（A 项目 app 引 B 项目库 = 拒绝）→**重名抽检（第二团队建同名 app 成功；同团队跨项目同名 app 亦成功）**。

## 9. 性能与预算

- **零新增常驻组件**：默认捆绑内存预算不动（≈492MB 现状维持）；argon2id 登录瞬时 ~64MiB + <100ms（fleetlyd 进程内，登录低频）；sessions 写频 = 登录/续期（SQLite 单写点内低频写，A2 同款节流）。
- 角色解析每资源请求一次索引读（单机 SQLite <µs 级）；v0.3 不做进程内缓存，profile 证需要时再挂账。

## 10. 安全口径汇录

- 口令 argon2id + 双键限流；会话 cookie HttpOnly/SameSite=Lax(+Secure under TLS)；secret 值永不入审计/事件/日志（红线沿用）；注册缺省关 + 无用户窗口恒开（fresh 安装指引：暴露公网前完成首用户注册）；bootstrap token 首用户注册即吊销（零残留后门）；禁用用户 = 会话+PAT 联动吊销；邀请一次性 token sha256 入库 7 天期；平台管理员只读全域不代写；fail-closed 双门（scope 未登记拒、项目解析不出拒）。

## 11. 波次映射

| 波次 | 本设计切面 |
|---|---|
| W1 身份与认证 | §2 全部 + §5 AuthService/UsersService + §7 登录/注册/PAT 页 + CLI auth/上下文 + 审计 actor 增维 + e2e auth.sh |
| W2 角色与授权 | §3/§4 全部（含 project_members 队内覆写，D-W0-2 修订） + §5 Teams/Projects 服务 + Tokens/GitKeys 语义迁移 + Console 切换器/成员/邀请/项目覆写成员 tab/资源改派 + e2e rbac.sh + 全量回归 |
| W3 审计与留存 | §6 留存设置化（audit.retention_days 默认 90）+ 双读面（Console 审计页 + CLI 导出 CSV）+ FZ-12（指纹披露 + git.hostkey_changed 变更事件 + CLI 核对） |

## 12. 挂账（v0.3 范围外，重启随需求）

项目级角色全量 A 形（跨团队 outsiders、可见性级联、项目级邀请、对团队成员隐藏私有项目）；同项目 app 互访的显式 opt-in（项目网络 fleetly-prj-* 形态，需求出现再启）；命名段内连字符歧义 admission 预检（同团队 a/b-c 与 a-b/c 推导同名服务——现由 docker 创建失败显性暴露，§4.3）；device flow 登录；email 验证/邮件邀请发送（随 W4 Email 通道）；找回口令自助（现为平台管理员重置）；per-project 命名空间（SaaS 化专项）；CSRF double-submit token；角色解析缓存；SearchMetrics 按项目收口（标签级 enforcement）；SSO/LDAP（商业企业件候选，随 §13 Q3 分界）；审计外发 SIEM。

## 13. 裁决记录

| # | 议题 | 状态 |
|---|---|---|
| D-W0-1 | 认证形态（规划 §3 Q1）：口令+服务端会话（Console）+ PAT（CLI/CI）；CLI 登录 = 粘贴 PAT，device flow 挂账 | **已裁**（本设计 §2，用户补充需求即选型输入） |
| D-W0-2 | Project 层角色：初裁不做（2026-09-23 用户委托）→ **修订为做队内覆写形 B**（2026-09-23 用户成本复盘后直裁：仅限团队成员、双向覆写、owner 不可覆写、可见性零级联，约 +3~4 人日进 W2）；A 形全量挂账 | **已裁（修订版，§3.3）** |
| D-W0-3 | 角色集（用户委托重设计）：viewer/developer/admin/owner 四档 + 平台管理员标志分离；terminal 归 developer+ | **已裁**（§3.2） |
| D-W0-4 | 命名唯一性：初裁 app 名全局唯一 → 一修 team 内唯一 → **二修 project 段拼进命名（2026-09-23 用户裁定）**：app/库名 **project 内唯一**（UNIQUE(project_id,name)，每项目各有 web/api/db），命名三段 `fleetly-<team>-<prj>-<app>-<service>`（两 slug 单词制+不可变），卷/库卷公式不变（ULID 尾缀防撞），保留字迁 team slug（project 段居第二位不邻前缀族），MoveApp=换名重部署（罕见管理动作，披露停机），`team/prj/app` 限定形引用 | **已裁（二次修订版，§3.4/§4.3）** |
| D-W0-5 | 存量单操作员迁移：初裁首用户自动全量认领 → **修订：无迁移负担**（用户澄清 2026-09-23「现役仅一处 staging、可清空重建」）——认领/收编/NULL 窗口全撤，v0.3 = fresh-install 版本（无 v0.2 升级路径），bootstrap token 首用户注册即自动吊销 | **已裁（修订版，§3.4/§8）** |
| D-W0-6 | 审计留存与读面（规划 §3 Q3）：90 天可调（platform_settings）+ Console 审计页 + CLI 导出 CSV | **已裁**（§6） |
| D-W0-7 | 商业分界（规划 §3 Q5）：**开源全功能**——自托管不设团队/项目/成员上限（红线直译）；付费 = 托管云 + 企业件（SSO/LDAP、审计外发 SIEM、合规报告、优先支持）；对 v0.3 实现零约束 | **已裁**（落点 = 发布口径，v0.3 发布时 README 明示） |
| D-W0-8 | FZ-12 SSH host key 钉位（规划 §3 Q4）：指纹披露 + git.hostkey_changed 变更事件 + CLI `fleetly git fingerprint` 核对；host key 持久化保持，known_hosts 钉定为客户端指引 | **已裁**（§6） |
| D-W0-9 | 同名项目跨团队（用户评审补充 2026-09-23）：per-team 唯一维持 + 限定形引用解析——`team/project` 恒可解析，裸名仅解析域内唯一（否则 E_PROJECT_AMBIGUOUS 列候选）；CLI 上下文/Console 路由/审计与事件 target 一律 ID | **已裁**（§3.4） |
