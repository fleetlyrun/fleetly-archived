// REST 面类型：与 proto 消息直接对应的接口一律引用 schema.d.ts 的生成类型
// （openapi-typescript 从 genproto/fleetly/server/v1/*.swagger.json 生成，
// `pnpm gen:api` 再生成，CI 以"再生成无 diff"门禁拦漂移——D4-②）。
//
// 生成类型的两个口径（与 gateway 实际输出一致，勿按直觉"纠正"）：
//   - 字段全部可选：gateway JSON marshaler EmitUnpopulated=false，proto3
//     零值字段不出现在 JSON 输出；
//   - int64 一律字符串（proto3 JSON 映射），Timestamp 是 RFC3339 字符串。
//
// 仅 gateway 传输形态投影（NDJSON result 包裹，stream-types.ts）与页面局部
// 视图模型保留手写——它们不在 proto 消息面内。

import type { components } from "./schema";

type Schemas = components["schemas"];

// ── auth（v0.3 RBAC W1 认证面；proto fleetly/server/v1/auth.proto）────────

export type UserView = Schemas["v1UserView"];
export type TeamMembership = Schemas["v1TeamMembership"];
export type MeResponse = Schemas["v1MeResponse"];
export type ProjectOverrideMembership = Schemas["v1ProjectOverrideMembership"];
export type RegisterResponse = Schemas["v1RegisterResponse"];
export type LoginResponse = Schemas["v1LoginResponse"];
export type LogoutResponse = Schemas["v1LogoutResponse"];
export type LogoutAllResponse = Schemas["v1LogoutAllResponse"];
export type AcceptInviteResponse = Schemas["v1AcceptInviteResponse"];
export type GetRegistrationStateResponse = Schemas["v1GetRegistrationStateResponse"];

// ── tokens / projects（v0.3 W2-S2 PAT 自服务页，rbac-teams 设计 §7）──────

export type TokenView = Schemas["v1TokenView"];
export type CreateTokenResponse = Schemas["v1CreateTokenResponse"];
export type ListTokensResponse = Schemas["v1ListTokensResponse"];
export type RevokeTokenResponse = Schemas["v1RevokeTokenResponse"];
export type ProjectView = Schemas["v1ProjectView"];
export type ListProjectsResponse = Schemas["v1ListProjectsResponse"];

// ── teams（v0.3 W2-S5 团队设置页：成员/角色/邀请管理，rbac-teams §3.1）──

export type TeamView = Schemas["v1TeamView"];
export type ListTeamsResponse = Schemas["v1ListTeamsResponse"];
export type TeamMemberView = Schemas["v1TeamMemberView"];
export type ListTeamMembersResponse = Schemas["v1ListTeamMembersResponse"];
export type SetTeamMemberRoleResponse = Schemas["v1SetTeamMemberRoleResponse"];
export type InviteView = Schemas["v1InviteView"];
export type CreateInviteResponse = Schemas["v1CreateInviteResponse"];
export type ListTeamInvitesResponse = Schemas["v1ListTeamInvitesResponse"];

// ── projects 覆写成员面（v0.3 W2-S5 项目设置 tab，rbac-teams §3.3）────────

export type ProjectMemberView = Schemas["v1ProjectMemberView"];
export type ListProjectMembersResponse = Schemas["v1ListProjectMembersResponse"];
export type SetProjectMemberRoleResponse = Schemas["v1SetProjectMemberRoleResponse"];
export type CreateProjectResponse = Schemas["v1CreateProjectResponse"];
export type GetProjectResponse = Schemas["v1GetProjectResponse"];
export type UpdateProjectResponse = Schemas["v1UpdateProjectResponse"];

// ── users（v0.3 W2-S5 平台管理员用户管理页，设计 §7「平台管理员」）───────
// 手写投影（非 schema.d.ts 生成——users.swagger.json 因 /v1/auth/registration
// 路径键与 auth.swagger.json 冲突留在 gen-api 清单外，S2 取舍沿用，见
// gen-api.mjs 头注）。UserView 复用 auth 段的生成类型（同一 proto 消息，
// Me 投影同源）；其余为 users.proto 消息的手写镜像：Timestamp 为 RFC3339
// 字符串、零值字段缺省（EmitUnpopulated=false）。

export type ListUsersResponse = { users?: UserView[] };

/** 创建用户应答：明文临时口令仅本次响应可见（服务端只存哈希）。 */
export type CreateUserResponse = { user?: UserView; temporary_password?: string };

/** 口令重置应答：明文临时口令仅本次可见；重置即该用户全端下线。 */
export type ResetUserPasswordResponse = { id?: string; temporary_password?: string };

export type DisableUserResponse = { user?: UserView };
export type EnableUserResponse = { user?: UserView };
export type GrantPlatformAdminResponse = { user?: UserView };
export type RevokePlatformAdminResponse = { user?: UserView };
export type SetRegistrationResponse = { open?: boolean };

/** 审计留存设置读面：set=false = 未显式设置（生效值走 config > 缺省 90）。 */
export type GetAuditRetentionResponse = {
  days?: number;
  set?: boolean;
  updated_at?: string;
};
export type SetAuditRetentionResponse = { days?: number };

// ── audit（v0.3 W3-S1 读面 + W3-S3 Console 浏览页，rbac-teams §6）─────────

export type AuditView = Schemas["v1AuditView"];
export type ListAuditResponse = Schemas["v1ListAuditResponse"];


// ── apps ────────────────────────────────────────────────────────────────

export type AppView = Schemas["v1AppView"];
export type ListAppsResponse = Schemas["v1ListAppsResponse"];

// ── placement ────────────────────────────────────────────────────────────

export type PlacementView = Schemas["v1PlacementView"];
export type VolumeView = Schemas["v1VolumeView"];

// ── join wizard（E1-8，multi-node §2.3）──────────────────────────────────

export type JoinGuideView = Schemas["v1JoinGuideView"];
export type GetJoinGuideResponse = Schemas["v1GetJoinGuideResponse"];
export type RotateJoinTokenResponse = Schemas["v1RotateJoinTokenResponse"];
export type FirewallRule = Schemas["v1FirewallRule"];

// ── deployments ──────────────────────────────────────────────────────────

export type DeploymentView = Schemas["v1DeploymentView"];
export type GetAppResponse = Schemas["v1GetAppResponse"];
export type ComposeWarning = Schemas["v1ComposeWarning"];
export type DeployResponse = Schemas["v1DeployResponse"];
export type ListDeploymentsResponse = Schemas["v1ListDeploymentsResponse"];
export type CancelDeploymentResponse = Schemas["v1CancelDeploymentResponse"];
export type RollbackDeploymentResponse = Schemas["v1RollbackDeploymentResponse"];

// ── builds（构建台账面，T2.18；proto fleetly/server/v1/builds.proto）──────
// BuildView 词表：status ∈ queued/building/succeeded/failed；driver ∈
// railpack/dockerfile/passthrough。无触发来源/commit sha/部署关联字段
// ——Console 不硬造对应列。TriggerBuild 类型随清单进来但无端点封装
//（CLI 构建入口，Console 不消费）。

export type BuildView = Schemas["v1BuildView"];
export type GetBuildResponse = Schemas["v1GetBuildResponse"];
export type ListBuildsResponse = Schemas["v1ListBuildsResponse"];

// ── git keys（T2.19 git push(SSH) 认证面；proto fleetly/server/v1/gitkeys.proto）──
// 无敏感投影：公钥本体为公开材料（指纹可复算），私钥永不经过平台。

export type GitKeyView = Schemas["v1GitKeyView"];
export type AddGitKeyRequest = Schemas["v1AddGitKeyRequest"];
export type AddGitKeyResponse = Schemas["v1AddGitKeyResponse"];
export type ListGitKeysResponse = Schemas["v1ListGitKeysResponse"];
export type RemoveGitKeyResponse = Schemas["v1RemoveGitKeyResponse"];

// ── apps webhook/git 触发面（T2.19；proto apps.proto，admin scope）────────
// secret 与 source 认证材料均为只写（响应只回 configured 位/回显非敏感字段）。

export type ShowAppWebhookResponse = Schemas["v1ShowAppWebhookResponse"];
// 请求体在 swagger 面是内联 body 形（openapiv2 不出 v1 消息 definition）——
// 投影随生成形态取 <Op>Body 名。
export type SetAppWebhookSecretRequest = Schemas["AppsServiceSetAppWebhookSecretBody"];
export type SetAppWebhookSecretResponse = Schemas["v1SetAppWebhookSecretResponse"];
export type SetAppSourceRequest = Schemas["AppsServiceSetAppSourceBody"];
export type SetAppSourceResponse = Schemas["v1SetAppSourceResponse"];

// ── drift（运行域漂移面，T2.18；proto fleetly/server/v1/drift.proto）──────

export type ShowDriftResponse = Schemas["v1ShowDriftResponse"];
export type ServiceDriftView = Schemas["v1ServiceDriftView"];
export type FieldDiffView = Schemas["v1FieldDiffView"];
export type ConvergeDriftResponse = Schemas["v1ConvergeDriftResponse"];
export type SetDriftConvergeResponse = Schemas["v1SetDriftConvergeResponse"];

// ── revisions ────────────────────────────────────────────────────────────

export type RevisionView = Schemas["v1RevisionView"];
export type ListRevisionsResponse = Schemas["v1ListRevisionsResponse"];
export type GetRevisionSpecResponse = Schemas["v1GetRevisionSpecResponse"];

// ── env ──────────────────────────────────────────────────────────────────

export type EnvVarView = Schemas["v1EnvVarView"];
export type ListEnvResponse = Schemas["v1ListEnvResponse"];
export type GetEnvResponse = Schemas["v1GetEnvResponse"];
export type SetEnvResponse = Schemas["v1SetEnvResponse"];
export type RemoveEnvResponse = Schemas["v1RemoveEnvResponse"];

// ── domains ──────────────────────────────────────────────────────────────

export type DomainView = Schemas["v1DomainView"];
export type ListAppDomainsResponse = Schemas["v1ListAppDomainsResponse"];
export type CreateAppDomainResponse = Schemas["v1CreateAppDomainResponse"];
export type UpdateAppDomainResponse = Schemas["v1UpdateAppDomainResponse"];
export type RemoveAppDomainResponse = Schemas["v1RemoveAppDomainResponse"];
export type DomainCheckView = Schemas["v1DomainCheckView"];
export type VerifyAppDomainsResponse = Schemas["v1VerifyAppDomainsResponse"];

/** 后端协议词表（proto CreateAppDomainRequest.protocol 消费侧词表）。 */
export type DomainProtocol = "http" | "h2c";
/** 证书模式词表（proto cert_mode；wildcard 的 DNS-01 签发链沿 W5）。 */
export type DomainCertMode = "http01" | "wildcard";

// ── logs ─────────────────────────────────────────────────────────────────

export type LogEntryView = Schemas["v1LogEntryView"];
export type ListHistoryLogsResponse = Schemas["v1ListHistoryLogsResponse"];
export type SearchLogRow = Schemas["v1SearchLogRow"];
export type SearchLogsResponse = Schemas["v1SearchLogsResponse"];

/** 检索来源过滤词表（proto sources repeated string 的消费侧词表）。 */
export type SearchSource = "container" | "build" | "access";

// ── metrics（E6 W5-S3，D-W5-2 opt-in）────────────────────────────────────

export type MetricsComponentView = Schemas["v1MetricsComponentView"];
export type MetricsPoint = Schemas["v1MetricsPoint"];
export type MetricsSeries = Schemas["v1MetricsSeries"];
export type SearchMetricsResponse = Schemas["v1SearchMetricsResponse"];
export type GetMetricsStatusResponse = Schemas["v1GetMetricsStatusResponse"];
export type SetMetricsModeResponse = Schemas["v1SetMetricsModeResponse"];

/** metrics.mode 词表（proto SetMetricsModeRequest.mode 消费侧词表）。 */
export type MetricsMode = "unset" | "on";

// ── alerting（B 线 W5-S2，D-V3W5-1 告警面）────────────────────────────────

export type AlertRuleView = Schemas["v1AlertRuleView"];
export type ListAlertRulesResponse = Schemas["v1ListAlertRulesResponse"];
export type CreateAlertRuleResponse = Schemas["v1CreateAlertRuleResponse"];
export type UpdateAlertRuleResponse = Schemas["v1UpdateAlertRuleResponse"];
export type DeleteAlertRuleResponse = Schemas["v1DeleteAlertRuleResponse"];
export type SetAlertsModeResponse = Schemas["v1SetAlertsModeResponse"];
export type GetAlertsStatusResponse = Schemas["v1GetAlertsStatusResponse"];
export type TestAlertRuleResponse = Schemas["v1TestAlertRuleResponse"];

/** alerts.mode 词表（proto SetAlertsModeRequest.mode 消费侧词表）。 */
export type AlertsMode = "unset" | "on";

// ── apps scaling（W5-S1，D-V3W5-2 自动扩缩策略面）────────────────────────

export type GetScalingPolicyResponse = Schemas["v1GetScalingPolicyResponse"];
export type SetScalingPolicyResponse = Schemas["v1SetScalingPolicyResponse"];
export type RemoveScalingPolicyResponse = Schemas["v1RemoveScalingPolicyResponse"];

// ── notifications（E6 W5-S4 通知 Webhook；observability §5 + §8 通道扩展）──

export type WebhookEndpointView = Schemas["v1WebhookEndpointView"];
export type ListWebhookEndpointsResponse = Schemas["v1ListWebhookEndpointsResponse"];
export type GetWebhookEndpointResponse = Schemas["v1GetWebhookEndpointResponse"];
export type CreateWebhookEndpointResponse = Schemas["v1CreateWebhookEndpointResponse"];
export type UpdateWebhookEndpointResponse = Schemas["v1UpdateWebhookEndpointResponse"];
export type DeleteWebhookEndpointResponse = Schemas["v1DeleteWebhookEndpointResponse"];
export type RotateWebhookSecretResponse = Schemas["v1RotateWebhookSecretResponse"];
// W4-S3：RPC 名沿契约版本化门禁保留 TestWebhook，语义扩为按端点通道类型
// 试发（webhook 签名 POST / slack {"text"} / email SMTP——observability §8.2）。
export type TestWebhookResponse = Schemas["v1TestWebhookResponse"];
export type WebhookDeliveryView = Schemas["v1WebhookDeliveryView"];
export type ListWebhookDeliveriesResponse = Schemas["v1ListWebhookDeliveriesResponse"];
// 平台级 SMTP 设置面（W4-S3，observability §8.3：密码只写不读——读面只出指纹）。
export type SmtpSettingsView = Schemas["v1SmtpSettingsView"];
export type GetSmtpSettingsResponse = Schemas["v1GetSmtpSettingsResponse"];
export type UpdateSmtpSettingsResponse = Schemas["v1UpdateSmtpSettingsResponse"];
export type TestSmtpResponse = Schemas["v1TestSmtpResponse"];

/** 通道类型词表（proto type 消费侧词表；缺省 webhook——存量端点升级即 webhook）。 */
export type WebhookChannelType = "webhook" | "slack" | "email";

/** 投递状态词表（proto status 过滤消费侧词表；failed = 终态）。 */
export type WebhookDeliveryStatus = "pending" | "ok" | "failed";

// ── terminal（E7 W5-S6 Web 终端；web-terminal §2.5）──────────────────────

export type CreateTerminalTicketResponse = Schemas["v1CreateTerminalTicketResponse"];
export type GetTerminalStatusResponse = Schemas["v1GetTerminalStatusResponse"];

// ── system ───────────────────────────────────────────────────────────────

export type ComponentHealth = Schemas["v1ComponentHealth"];
export type GetSystemStatusResponse = Schemas["v1GetSystemStatusResponse"];
export type NodeView = Schemas["v1NodeView"];
export type ListNodesResponse = Schemas["v1ListNodesResponse"];
export type TraefikView = Schemas["v1TraefikView"];
export type CertLedgerView = Schemas["v1CertLedgerView"];
export type GetIngressStatusResponse = Schemas["v1GetIngressStatusResponse"];

// ── backups（T2.22 台账 + E3-3 上传轨）───────────────────────────────────

export type BackupView = Schemas["v1BackupView"];
export type ListBackupsResponse = Schemas["v1ListBackupsResponse"];
export type TriggerBackupResponse = Schemas["v1TriggerBackupResponse"];

// ── S3 设置面（E3 对象存储 §5.1/E3-2）────────────────────────────────────

export type S3SettingsView = Schemas["v1S3SettingsView"];
export type GetS3SettingsResponse = Schemas["v1GetS3SettingsResponse"];
export type UpdateS3SettingsRequest = Schemas["v1UpdateS3SettingsRequest"];
export type UpdateS3SettingsResponse = Schemas["v1UpdateS3SettingsResponse"];
export type TestS3ConnectionRequest = Schemas["v1TestS3ConnectionRequest"];
export type TestS3ConnectionResponse = Schemas["v1TestS3ConnectionResponse"];
export type S3ConnectionTestResult = Schemas["v1S3ConnectionTestResult"];
export type S3ProbeStep = Schemas["v1S3ProbeStep"];

// ── ACME DNS-01 设置面（B 线 W5-S3，D-V3W5-3/D-V3W5-4）──────────────────

export type AcmeSettingsView = Schemas["v1AcmeSettingsView"];
export type GetAcmeSettingsResponse = Schemas["v1GetAcmeSettingsResponse"];
export type UpdateAcmeSettingsRequest = Schemas["v1UpdateAcmeSettingsRequest"];
export type UpdateAcmeSettingsResponse = Schemas["v1UpdateAcmeSettingsResponse"];
export type TestDnsProviderRequest = Schemas["v1TestDnsProviderRequest"];
export type TestDnsProviderResponse = Schemas["v1TestDnsProviderResponse"];
export type DnsProviderTestResult = Schemas["v1DnsProviderTestResult"];
export type DnsProbeStep = Schemas["v1DnsProbeStep"];

// ── cron（E5 Cron，架构 §4.3）────────────────────────────────────────────

export type CronRunView = Schemas["v1CronRunView"];
export type ListCronRunsResponse = Schemas["v1ListCronRunsResponse"];
export type TriggerCronRunResponse = Schemas["v1TriggerCronRunResponse"];

// ── databases（E4 数据库托管，managed-databases §5.1）────────────────────

export type DatabaseView = Schemas["v1DatabaseView"];
export type DatabaseVolumeView = Schemas["v1DatabaseVolumeView"];
export type DatabaseConnectionView = Schemas["v1DatabaseConnectionView"];
export type DatabaseLimits = Schemas["v1DatabaseLimits"];
export type DatabaseBackupPlan = Schemas["v1DatabaseBackupPlan"];
export type ListDatabasesResponse = Schemas["v1ListDatabasesResponse"];
export type GetDatabaseResponse = Schemas["v1GetDatabaseResponse"];
export type CreateDatabaseResponse = Schemas["v1CreateDatabaseResponse"];
export type DeleteDatabaseResponse = Schemas["v1DeleteDatabaseResponse"];
export type SuspendDatabaseResponse = Schemas["v1SuspendDatabaseResponse"];
export type ResumeDatabaseResponse = Schemas["v1ResumeDatabaseResponse"];
export type RetryDatabaseResponse = Schemas["v1RetryDatabaseResponse"];
export type RotateDatabaseCredentialsResponse = Schemas["v1RotateDatabaseCredentialsResponse"];
export type RevealDatabaseCredentialsResponse = Schemas["v1RevealDatabaseCredentialsResponse"];
export type DatabaseBackupView = Schemas["v1DatabaseBackupView"];
export type ListDatabaseBackupsResponse = Schemas["v1ListDatabaseBackupsResponse"];
export type TriggerDatabaseBackupResponse = Schemas["v1TriggerDatabaseBackupResponse"];
export type RestoreDatabaseBackupResponse = Schemas["v1RestoreDatabaseBackupResponse"];
export type UpgradeDatabaseResponse = Schemas["v1UpgradeDatabaseResponse"];

// ── platform secrets（E4 §2.7 平台密钥库；无值读回面）────────────────────

export type SecretView = Schemas["v1SecretView"];
export type ListSecretsResponse = Schemas["v1ListSecretsResponse"];
export type SetSecretResponse = Schemas["v1SetSecretResponse"];
export type RemoveSecretResponse = Schemas["v1RemoveSecretResponse"];

// ── app configs（T 线 OT-3 明文配置资源；Get 明文回读按 admin 门）─────────

export type ConfigView = Schemas["v1ConfigView"];
export type ListConfigsResponse = Schemas["v1ListConfigsResponse"];
export type SetConfigResponse = Schemas["v1SetConfigResponse"];
export type GetConfigResponse = Schemas["v1GetConfigResponse"];
export type RemoveConfigResponse = Schemas["v1RemoveConfigResponse"];

// ── events ───────────────────────────────────────────────────────────────

export type EventView = Schemas["v1EventView"];
export type CursorExpiredView = Schemas["v1CursorExpiredView"];

/** WatchEvents 的 oneof frame 投影（UseProtoNames → snake_case 成员名）。 */
export type WatchEventsFrame = Schemas["v1WatchEventsResponse"];
