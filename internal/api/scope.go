package api

import "strings"

// 方法级 scope 映射（T2.17 鉴权矩阵的唯一登记点）：
//
//	read   — 全部只读面（List/Get/Show/Watch/Follow/History/Status）
//	deploy — 部署/回滚/取消、env 写（env-set 影响下次部署）、漂移收敛与
//	         opt-in 置位（写运行域/影响下次部署语义）
//	admin  — token 管理、env 明文读、app 删除（破坏性）、构建触发（H14
//	         整改：TriggerBuild 的 base_dir 可指向宿主任意目录，构建会把
//	         整目录打进镜像——宿主文件系统读取面与 env 明文同级信任，
//	         不再随 deploy scope 下放）
//
// 纪律：新增 RPC 必须在此登记；未登记方法在拦截器按 admin 拒绝
// （fail-closed，见 auth.go）。
var methodScopes = map[string]string{
	// SystemService
	"/fleetly.server.v1.SystemService/GetSystemStatus":  ScopeRead,
	"/fleetly.server.v1.SystemService/ListNodes":        ScopeRead,
	"/fleetly.server.v1.SystemService/GetIngressStatus": ScopeRead,
	// join 向导面（E1-8，multi-node §2.3）：guide 响应含 join token 材料 =
	// admin；token 轮换 = admin（安全面写操作，D-MN-1）。
	"/fleetly.server.v1.SystemService/GetJoinGuide":    ScopeAdmin,
	"/fleetly.server.v1.SystemService/RotateJoinToken": ScopeAdmin,
	// 备份面（T2.22）：台账只读；手动触发 = 写面语义（与升级编排的
	// pre_upgrade 快照共用入口），取 deploy scope。
	"/fleetly.server.v1.SystemService/ListBackups":   ScopeRead,
	"/fleetly.server.v1.SystemService/TriggerBackup": ScopeDeploy,
	// W2-S4 平台面收口（rbac-teams §4.2 第 3 条）：TriggerBackup 属平台备份
	// 写面——用户 principal 另须 is_platform_admin（requirePlatformWriteFace，
	// internal/api/system.go）；机具令牌沿本登记的 scope 门。
	// S3 设置面（E3-2，对象存储 §5.1）：整体 admin——端点/桶/凭证指纹属
	// 平台敏感配置，secret 明文只写（Update）与凭证解密（Test）是平台
	// 信任面，与 env 明文读同级，不随 deploy/read 下放。
	"/fleetly.server.v1.SystemService/GetS3Settings":    ScopeAdmin,
	"/fleetly.server.v1.SystemService/UpdateS3Settings": ScopeAdmin,
	"/fleetly.server.v1.SystemService/TestS3Connection": ScopeAdmin,
	// ACME DNS-01 设置面（B 线 W5-S3，D-V3W5-3/4）：整体 admin——DNS 服务
	// 商凭证指纹属平台敏感配置，token 明文只写（Update）与凭证解密（探针
	// TestDnsProvider）是平台信任面，与 S3 设置三面同级，不随 deploy/read
	// 下放。用户 principal 另须 is_platform_admin（requirePlatformWriteFace
	// 挂全部三 handler——rbac-teams §3.2「全局设置 → 仅平台管理员」；机具
	// 令牌沿本登记的 scope 门）。
	"/fleetly.server.v1.SystemService/GetAcmeSettings":    ScopeAdmin,
	"/fleetly.server.v1.SystemService/UpdateAcmeSettings": ScopeAdmin,
	"/fleetly.server.v1.SystemService/TestDnsProvider":    ScopeAdmin,
	// 平台 registry 凭证设置面（IMPL-T1-2/DT-2）：整体 admin——host/用户名/
	// 密码指纹属平台敏感配置，密码明文只写（Update）是平台信任面，与 S3/
	// ACME 设置面同级，不随 deploy/read 下放。用户 principal 另须
	// is_platform_admin（requirePlatformWriteFace 挂两 handler）。
	"/fleetly.server.v1.SystemService/GetRegistrySettings":    ScopeAdmin,
	"/fleetly.server.v1.SystemService/UpdateRegistrySettings": ScopeAdmin,
	// AppsService
	"/fleetly.server.v1.AppsService/ListApps":  ScopeRead,
	"/fleetly.server.v1.AppsService/GetApp":    ScopeRead,
	"/fleetly.server.v1.AppsService/DeleteApp": ScopeAdmin,
	// webhook/git 触发配置面（T2.19；secret 与认证材料写面 = admin——
	// 验签是该端点的唯一认证，材料属平台敏感面）。
	"/fleetly.server.v1.AppsService/SetAppWebhookSecret": ScopeAdmin,
	"/fleetly.server.v1.AppsService/ShowAppWebhook":      ScopeAdmin,
	"/fleetly.server.v1.AppsService/SetAppSource":        ScopeAdmin,
	// 自动扩缩策略面（W5-S1，D-V3W5-2）：读 = read（策略是应用运行面的事
	// 实视图，与 GetApp 同级）；写 = deploy（资源面写语义——调整的是应用
	// 自身的运行参数，与 Deploy/SetEnv/SetDriftConverge 同级；无凭据材料，
	// 不到 admin。机具令牌 admin 等价照旧；用户 principal 另受第 2 门项目
	// 角色约束——requireAppAccess，developer+ 可写）。
	"/fleetly.server.v1.AppsService/GetScalingPolicy":    ScopeRead,
	"/fleetly.server.v1.AppsService/SetScalingPolicy":    ScopeDeploy,
	"/fleetly.server.v1.AppsService/RemoveScalingPolicy": ScopeDeploy,
	// DeploymentsService
	"/fleetly.server.v1.DeploymentsService/ListDeployments":    ScopeRead,
	"/fleetly.server.v1.DeploymentsService/GetDeployment":      ScopeRead,
	"/fleetly.server.v1.DeploymentsService/Deploy":             ScopeDeploy,
	"/fleetly.server.v1.DeploymentsService/CancelDeployment":   ScopeDeploy,
	"/fleetly.server.v1.DeploymentsService/RollbackDeployment": ScopeDeploy,
	// DeployFromGit（T2.19）：post-receive 钩子经 hook token（deploy
	// scope）回调——最小权限，与 Deploy 同级。
	"/fleetly.server.v1.DeploymentsService/DeployFromGit": ScopeDeploy,
	// RevisionsService
	"/fleetly.server.v1.RevisionsService/ListRevisions":   ScopeRead,
	"/fleetly.server.v1.RevisionsService/GetRevisionSpec": ScopeRead,
	// BuildsService
	// TriggerBuild = admin（H14 宿主目录信任边界）：base_dir 显式提供时可
	// 指向宿主任意目录（SQLite 库、age 密钥材料同位），构建把整目录打进
	// 镜像再经部署外带——比 deploy 多出宿主文件系统逃逸面，与 env 明文
	// 读取（GetEnv=admin）同级信任。读面（GetBuild/ListBuilds）不变。
	"/fleetly.server.v1.BuildsService/TriggerBuild": ScopeAdmin,
	"/fleetly.server.v1.BuildsService/GetBuild":     ScopeRead,
	"/fleetly.server.v1.BuildsService/ListBuilds":   ScopeRead,
	// DriftService
	"/fleetly.server.v1.DriftService/ShowDrift":        ScopeRead,
	"/fleetly.server.v1.DriftService/ConvergeDrift":    ScopeDeploy,
	"/fleetly.server.v1.DriftService/SetDriftConverge": ScopeDeploy,
	// DomainsService（T2.17 只读 + IMPL-T1-1 可写升级）：读面 read；写面
	// （域名资源 CRUD）= deploy——路由声明属应用运行面写语义（compose
	// label 同一信赖级：改可达面/暴露面但不含凭据材料，与 SetEnv/
	// SetScalingPolicy 同级；平台管理员资源面只读的双门不变）。机具令牌沿
	// 本登记的 scope 门；用户 principal 另受第 2 门项目角色约束
	//（requireAppAccess，developer+ 可写）。
	"/fleetly.server.v1.DomainsService/ListAppDomains":   ScopeRead,
	"/fleetly.server.v1.DomainsService/VerifyAppDomains": ScopeRead,
	"/fleetly.server.v1.DomainsService/CreateAppDomain":  ScopeDeploy,
	"/fleetly.server.v1.DomainsService/UpdateAppDomain":  ScopeDeploy,
	"/fleetly.server.v1.DomainsService/RemoveAppDomain":  ScopeDeploy,
	// EnvService
	"/fleetly.server.v1.EnvService/ListEnv":   ScopeRead,
	"/fleetly.server.v1.EnvService/SetEnv":    ScopeDeploy,
	"/fleetly.server.v1.EnvService/RemoveEnv": ScopeDeploy,
	// GetEnv（明文）= admin——取舍注记见 env.proto。
	"/fleetly.server.v1.EnvService/GetEnv": ScopeAdmin,
	// LogsService
	// SearchLogs（E6 W5-S1）：read——与 FollowLogs 同级（能看直播就能看
	// 检索，E6 设计 §3.1 权限原文）。
	"/fleetly.server.v1.LogsService/FollowLogs":      ScopeRead,
	"/fleetly.server.v1.LogsService/ListHistoryLogs": ScopeRead,
	"/fleetly.server.v1.LogsService/SearchLogs":      ScopeRead,
	// 日志后端面（E6 W5-S1）：show = read（运行视图）；set = deploy
	//（写运行域语义——切换触发 duty 收敛与采集路由翻转，与
	// SetDriftConverge 的 opt-in 置位同级；无凭据材料，不到 admin）。
	// W3-S2 起用户 principal 另须 is_platform_admin（requirePlatformWriteFace
	// 挂 handler——rbac-teams §3.2「全局设置 → 仅平台管理员」扩全；机具令牌
	// 沿本登记的 scope 门）。
	"/fleetly.server.v1.LogsService/GetLogsBackend": ScopeRead,
	"/fleetly.server.v1.LogsService/SetLogsBackend": ScopeDeploy,
	// MetricsService（E6 W5-S3，D-W5-2 opt-in）：查询与状态 = read
	//（PromQL 透传是操作员工具——设计 §4.2；能看日志检索就能查指标）；
	// 模式切换 = deploy（写运行域语义——opt-in 置位触发三件套部署/移除，
	// 与 SetLogsBackend 同级理由；无凭据材料，不到 admin）。W3-S2 起用户
	// principal 另须 is_platform_admin（SetLogsBackend 同款 handler 门）。
	"/fleetly.server.v1.MetricsService/SearchMetrics":    ScopeRead,
	"/fleetly.server.v1.MetricsService/GetMetricsStatus": ScopeRead,
	"/fleetly.server.v1.MetricsService/SetMetricsMode":   ScopeDeploy,
	// EventsService
	"/fleetly.server.v1.EventsService/WatchEvents": ScopeRead,
	// AlertingService（B 线 W5-S2，D-V3W5-1）：读面 = read（规则与栈态是
	// 事实面——任意已认证凭据）；规则 CRUD = admin（告警面向平台管理员，
	// 与 notifications 端点同域——平台级权威态，无 per-app 资源面语义）；
	// SetAlertsMode = deploy（写运行域语义——opt-in 置位触发 vmalert 部署/
	// 移除，与 SetMetricsMode 同级理由，无凭据材料）。TestAlertRule = admin
	//（规则编写伴随面且消耗 VM 查询资源——保守登记，与 CRUD 同门）。
	// W3-S2 起用户 principal 另须 is_platform_admin（requirePlatformWriteFace
	// 挂全部写面 handler——rbac-teams §3.2「全局设置 → 仅平台管理员」扩全；
	// 机具令牌沿本登记的 scope 门）。
	"/fleetly.server.v1.AlertingService/ListAlertRules":  ScopeRead,
	"/fleetly.server.v1.AlertingService/GetAlertsStatus": ScopeRead,
	"/fleetly.server.v1.AlertingService/CreateAlertRule": ScopeAdmin,
	"/fleetly.server.v1.AlertingService/UpdateAlertRule": ScopeAdmin,
	"/fleetly.server.v1.AlertingService/DeleteAlertRule": ScopeAdmin,
	"/fleetly.server.v1.AlertingService/SetAlertsMode":   ScopeDeploy,
	"/fleetly.server.v1.AlertingService/TestAlertRule":   ScopeAdmin,
	// PlacementService
	// ShowPlacement/ListVolumes/GetPlacementMigrationPlan = read（只读面，
	// E1-7）；UpdatePlacement = admin（破坏性确认路径，multi-node §2.6）。
	"/fleetly.server.v1.PlacementService/ShowPlacement":             ScopeRead,
	"/fleetly.server.v1.PlacementService/UpdatePlacement":           ScopeAdmin,
	"/fleetly.server.v1.PlacementService/ListVolumes":               ScopeRead,
	"/fleetly.server.v1.PlacementService/GetPlacementMigrationPlan": ScopeRead,
	// TokensService / GitKeysService（v0.3 W2 语义迁移，rbac-teams 设计
	// §2.3）：登记整体 read——真授权在 handler 内（用户自服务面：用户管
	// 自己的 PAT / push key；平台管理员全列 + 机具令牌显式创建；机具令牌
	// 按「平台管理员等价」读全列，写面须 admin scope，AddGitKey 恒要求
	// 用户 principal）。teams/projects 同款切面（scope 门只承担「凭据至少
	// 持有最小 read」的形状约束）；纪律：改登记 = 改测试。
	"/fleetly.server.v1.TokensService/CreateToken":   ScopeRead,
	"/fleetly.server.v1.TokensService/ListTokens":    ScopeRead,
	"/fleetly.server.v1.TokensService/RevokeToken":   ScopeRead,
	"/fleetly.server.v1.GitKeysService/AddGitKey":    ScopeRead,
	"/fleetly.server.v1.GitKeysService/ListGitKeys":  ScopeRead,
	"/fleetly.server.v1.GitKeysService/RemoveGitKey": ScopeRead,
	// CronService（E5 Cron）：手动触发 = 写面语义（与 Deploy 同级——触发
	// 的是应用自身的 compose 声明，不新增权限面）；台账读面 = read。
	"/fleetly.server.v1.CronService/TriggerCronRun": ScopeDeploy,
	"/fleetly.server.v1.CronService/ListCronRuns":   ScopeRead,
	// DatabaseService（E4 数据库托管，managed-databases §2.3）：get/list =
	// read（连接投影脱敏——明文 reveal 属 admin 更严面，随 S4/S6）；生命周期
	// 与设置写面（create/delete/suspend/resume/retry/settings）= admin
	// ——delete 是数据安全破坏性操作（引用守卫 + confirm 两段式），settings
	// 直改资源限额/备份计划，与 app 删除同级信任，不随 deploy 下放。
	"/fleetly.server.v1.DatabaseService/GetDatabase":            ScopeRead,
	"/fleetly.server.v1.DatabaseService/ListDatabases":          ScopeRead,
	"/fleetly.server.v1.DatabaseService/CreateDatabase":         ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/DeleteDatabase":         ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/SuspendDatabase":        ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/ResumeDatabase":         ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/RetryDatabase":          ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/UpdateDatabaseSettings": ScopeAdmin,
	// E4 W4-S4（managed-databases §2.5）：rotate = 破坏性两段式数据安全操作
	// （引用 app 被自动重部署），admin 与 delete 同级；reveal = 密码明文显式
	// 展开（admin 更严面——与 env GetEnv 同级信任）。
	"/fleetly.server.v1.DatabaseService/RotateDatabaseCredentials": ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/RevealDatabaseCredentials": ScopeAdmin,
	// E4 W4-S5（managed-databases §2.6）：备份列表 = read（台账只读事实面）；
	// 备份触发/恢复/升级 = admin（恢复与升级是破坏性两段式数据安全操作——
	// 原地重放覆盖数据卷、受控重建有停机窗口，与 delete 同级；备份触发直写
	// 远端 repo，写面语义与平台备份 TriggerBackup 同口径）。
	"/fleetly.server.v1.DatabaseService/ListDatabaseBackups":   ScopeRead,
	"/fleetly.server.v1.DatabaseService/TriggerDatabaseBackup": ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/RestoreDatabaseBackup": ScopeAdmin,
	"/fleetly.server.v1.DatabaseService/UpgradeDatabase":       ScopeAdmin,
	// SecretsService（E4 W4-S4，D-DB-7）：set/remove = admin（密钥写面与
	// webhook secret/env 明文同级信任）；list = read（只出名称/指纹——与
	// ListEnv 同口径，值零出现）。
	"/fleetly.server.v1.SecretsService/SetSecret":    ScopeAdmin,
	"/fleetly.server.v1.SecretsService/ListSecrets":  ScopeRead,
	"/fleetly.server.v1.SecretsService/RemoveSecret": ScopeAdmin,
	// ConfigsService（T 线 OT-3 / IMPL-T1-4）：set/get/remove = admin（明文
	// 配置的写面与回读面同门——与 SecretsService 写面同级信任；票面默认
	// 对齐 secrets 口径，实施记录第 7 条载裁决理由）；list = read（只出
	// 名称/指纹/时间锚，与 ListSecrets 同口径）。
	"/fleetly.server.v1.ConfigsService/SetConfig":    ScopeAdmin,
	"/fleetly.server.v1.ConfigsService/ListConfigs":  ScopeRead,
	"/fleetly.server.v1.ConfigsService/GetConfig":    ScopeAdmin,
	"/fleetly.server.v1.ConfigsService/RemoveConfig": ScopeAdmin,
	// NotificationsService（E6 W5-S4，observability §5；W4-S3 通道扩展 §8）：
	// 读面 = read（端点视图与投递台账是事实面——指纹非凭据，与 token 哈希
	// 前缀同口径）；写面 = admin（端点是平台级凭据面——创建/更新/删除/轮
	// 换/测试与 s3 设置同级；secret 明文只在创建/轮换响应一次性返回；SMTP
	// 设置面整体 admin——密码明文只写不读与 TestSmtp 探针消耗平台凭据，
	// GetSmtpSettings 的读面含凭据指纹）。W3-S2 起用户 principal 另须
	// is_platform_admin（requirePlatformWriteFace 挂全部写面 + SMTP 三 RPC
	// handler——rbac-teams §3.2「通知 → 仅平台管理员」扩全；机具令牌沿
	// scope 门）。
	"/fleetly.server.v1.NotificationsService/ListWebhookEndpoints":  ScopeRead,
	"/fleetly.server.v1.NotificationsService/GetWebhookEndpoint":    ScopeRead,
	"/fleetly.server.v1.NotificationsService/ListWebhookDeliveries": ScopeRead,
	"/fleetly.server.v1.NotificationsService/CreateWebhookEndpoint": ScopeAdmin,
	"/fleetly.server.v1.NotificationsService/UpdateWebhookEndpoint": ScopeAdmin,
	"/fleetly.server.v1.NotificationsService/DeleteWebhookEndpoint": ScopeAdmin,
	"/fleetly.server.v1.NotificationsService/RotateWebhookSecret":   ScopeAdmin,
	"/fleetly.server.v1.NotificationsService/TestWebhook":           ScopeAdmin,
	"/fleetly.server.v1.NotificationsService/GetSmtpSettings":       ScopeAdmin,
	"/fleetly.server.v1.NotificationsService/UpdateSmtpSettings":    ScopeAdmin,
	"/fleetly.server.v1.NotificationsService/TestSmtp":              ScopeAdmin,
	// ExecService（E7 W5-S6，web-terminal §2.4）：**整体 terminal scope**——
	// 独立 scope（默认仅 admin；read/deploy 不蕴含；admin 蕴含，auth.go
	// containsScope）。终端是任意命令执行面，权限与 deploy 的「发布自身声
	// 明」不同级，独立授予（D19 原文：terminal 独立 scope）。
	"/fleetly.server.v1.ExecService/CreateTerminalTicket": ScopeTerminal,
	"/fleetly.server.v1.ExecService/GetTerminalStatus":    ScopeTerminal,
	// AuthService（v0.3 W1，rbac-teams §2/§5）：Register/Login/
	// GetRegistrationState 三方法进 authExempt 名单（auth.go——登录页开关注
	// 册入口，无凭据可用）；Logout/LogoutAll/Me/AcceptInvite = 任意已认证
	// 凭据（取 read——会话凭据 scope = 角色可达集、最小机具令牌天然蕴含）。
	// 会话凭据的 scope 口径（reachableScopesForUser，W2-S4 硬收缩收口）见
	// auth.go。
	"/fleetly.server.v1.AuthService/Logout":       ScopeRead,
	"/fleetly.server.v1.AuthService/LogoutAll":    ScopeRead,
	"/fleetly.server.v1.AuthService/Me":           ScopeRead,
	"/fleetly.server.v1.AuthService/AcceptInvite": ScopeRead,
	// UsersService（v0.3 W1 平台用户管理面，rbac-teams §2.1/§3.2）：整体
	// admin scope（机具令牌面）+ handler 内平台管理员判定（用户 principal
	// 要求 is_platform_admin——requirePlatformAdmin，fail-closed）。
	"/fleetly.server.v1.UsersService/ListUsers":           ScopeAdmin,
	"/fleetly.server.v1.UsersService/CreateUser":          ScopeAdmin,
	"/fleetly.server.v1.UsersService/DisableUser":         ScopeAdmin,
	"/fleetly.server.v1.UsersService/EnableUser":          ScopeAdmin,
	"/fleetly.server.v1.UsersService/ResetUserPassword":   ScopeAdmin,
	"/fleetly.server.v1.UsersService/GrantPlatformAdmin":  ScopeAdmin,
	"/fleetly.server.v1.UsersService/RevokePlatformAdmin": ScopeAdmin,
	"/fleetly.server.v1.UsersService/SetRegistration":     ScopeAdmin,
	// 审计留存设置（v0.3 W3-S3，rbac-teams §6 D-W0-6 收口）：平台面写语义
	// 与 SetRegistration 同族——整体 admin scope + handler 内平台管理员判定
	//（requirePlatformAdmin 同门）。
	"/fleetly.server.v1.UsersService/GetAuditRetention": ScopeAdmin,
	"/fleetly.server.v1.UsersService/SetAuditRetention": ScopeAdmin,
	// TeamsService / ProjectsService（v0.3 W2-S1 团队/项目面，rbac-teams
	// §3.1/§3.3）：登记整体 read——这两面的真授权是 handler 内的**角色门**
	//（成员资格 + §3.2 矩阵/§3.3 覆写管理权，非 scope），scope 门只承担
	// 「凭据至少持有最小读」的形状约束（会话凭据 scope = 角色可达集；用户
	// PAT 最小 read 可达，实际权限由角色收敛）。机具令牌（user NULL）在
	// handler 内恒 403——无用户即无团队成员身份（含读面；平台级凭据的设计
	// 语义 §2.3），平台管理员用户只读放行、写面 403（不代写）。W2-S4 通用
	// 角色门（ResolvePermission 单点，ownership.go）已落地，本组登记维持
	// 形状门角色。
	"/fleetly.server.v1.TeamsService/CreateTeam":        ScopeRead,
	"/fleetly.server.v1.TeamsService/ListTeams":         ScopeRead,
	"/fleetly.server.v1.TeamsService/GetTeam":           ScopeRead,
	"/fleetly.server.v1.TeamsService/UpdateTeam":        ScopeRead,
	"/fleetly.server.v1.TeamsService/DeleteTeam":        ScopeRead,
	"/fleetly.server.v1.TeamsService/ListTeamMembers":   ScopeRead,
	"/fleetly.server.v1.TeamsService/SetTeamMemberRole": ScopeRead,
	"/fleetly.server.v1.TeamsService/RemoveTeamMember":  ScopeRead,
	"/fleetly.server.v1.TeamsService/CreateInvite":      ScopeRead,
	"/fleetly.server.v1.TeamsService/ListTeamInvites":   ScopeRead,
	"/fleetly.server.v1.TeamsService/RevokeInvite":      ScopeRead,
	// AuditService（v0.3 W3-S1 审计读面，rbac-teams §5/§6 D-W0-6）：整体
	// admin scope（机具令牌面——平台管理员等价，§2.3）+ handler 内平台管理
	// 员判定（requirePlatformAdminPrincipal 共享门——UsersService 同门）。
	// 读台账是平台敏感事实面（操作者全量动作流），不随 read/deploy 下放。
	"/fleetly.server.v1.AuditService/ListAudit":               ScopeAdmin,
	"/fleetly.server.v1.ProjectsService/CreateProject":        ScopeRead,
	"/fleetly.server.v1.ProjectsService/ListProjects":         ScopeRead,
	"/fleetly.server.v1.ProjectsService/GetProject":           ScopeRead,
	"/fleetly.server.v1.ProjectsService/UpdateProject":        ScopeRead,
	"/fleetly.server.v1.ProjectsService/DeleteProject":        ScopeRead,
	"/fleetly.server.v1.ProjectsService/ListProjectMembers":   ScopeRead,
	"/fleetly.server.v1.ProjectsService/SetProjectMemberRole": ScopeRead,
	"/fleetly.server.v1.ProjectsService/RemoveProjectMember":  ScopeRead,
	// 资源改派（backlog #16，MoveAppRequest/MoveDatabaseRequest 平台管理员
	// 专属）：admin scope——跨项目迁移是归属写面，与 app 删除同级；此前漏
	// 登记走拦截器 fail-closed（未登记按 admin 拒绝），本登记只把既有行为
	// 落回矩阵登记点，语义不变（scope.go 头注纪律：新增 RPC 必须登记）。
	"/fleetly.server.v1.ProjectsService/MoveApp":      ScopeAdmin,
	"/fleetly.server.v1.ProjectsService/MoveDatabase": ScopeAdmin,
}

// RequiredScope 返回方法所需 scope（未登记返回 false——调用方按 admin
// 拒绝路径处理）。
func RequiredScope(fullMethod string) (string, bool) {
	s, ok := methodScopes[fullMethod]
	return s, ok
}

// methodAction 是审计 action 的方法级词根（api.<Service>.<Method>——审计
// action 记 token/方法级，不发明新事件名）。
func methodAction(fullMethod string) string {
	trimmed := strings.TrimPrefix(fullMethod, "/fleetly.server.v1.")
	return "api." + strings.ReplaceAll(trimmed, "/", ".")
}
