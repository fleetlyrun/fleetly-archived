// 端点封装：页面只经此消费平台 REST 面（端点清单 = proto google.api.http
// 派生，见 proto/fleetly/server/v1/*.proto）。Console 不新增/绕过端点。

import { api, utf8ToBase64 } from "./client";
import type {
  AcceptInviteResponse,
  AttachAppProjectNetworkResponse,
  CancelDeploymentResponse,
  CreateDatabaseResponse,
  CreateInviteResponse,
  CreateProjectResponse,
  CreateTerminalTicketResponse,
  CreateTokenResponse,
  CreateWebhookEndpointResponse,
  CreateUserResponse,
  ConvergeDriftResponse,
  DeleteDatabaseResponse,
  DeleteWebhookEndpointResponse,
  DeployResponse,
  DeploymentView,
  DetachAppProjectNetworkResponse,
  DisableUserResponse,
  EnableUserResponse,
  GetAlertsStatusResponse,
  GetAppResponse,
  GetAuditRetentionResponse,
  GetDatabaseResponse,
  GetEnvResponse,
  GetAcmeSettingsResponse,
  GetIngressStatusResponse,
  GetJoinGuideResponse,
  GetMetricsStatusResponse,
  GetRegistrationStateResponse,
  GetRevisionSpecResponse,
  GetS3SettingsResponse,
  GetScalingPolicyResponse,
  GetProjectResponse,
  GetSmtpSettingsResponse,
  GetSystemStatusResponse,
  GetTerminalStatusResponse,
  GetWebhookEndpointResponse,
  GrantPlatformAdminResponse,
  SetAppSourceRequest,
  SetAppSourceResponse,
  SetAppWebhookSecretResponse,
  ShowAppWebhookResponse,
  ShowAppRuntimeResponse,
  ListAppDomainsResponse,
  CreateAppDomainResponse,
  UpdateAppDomainResponse,
  RemoveAppDomainResponse,
  DomainProtocol,
  DomainCertMode,
  ListAlertRulesResponse,
  AlertRuleView,
  CreateAlertRuleResponse,
  UpdateAlertRuleResponse,
  DeleteAlertRuleResponse,
  SetAlertsModeResponse,
  TestAlertRuleResponse,
  AlertsMode,
  ListAuditResponse,
  ListBackupsResponse,
  ListBuildsResponse,
  ListCronRunsResponse,
  ListDatabaseBackupsResponse,
  ListDeploymentsResponse,
  ListDatabasesResponse,
  ListEnvResponse,
  ListHistoryLogsResponse,
  ListNodesResponse,
  ListRevisionsResponse,
  ListSecretsResponse,
  ListTokensResponse,
  ListProjectsResponse,
  ListProjectMembersResponse,
  UpdateProjectResponse,
  ListTeamInvitesResponse,
  ListTeamMembersResponse,
  ListTeamsResponse,
  ListUsersResponse,
  ListWebhookDeliveriesResponse,
  ListWebhookEndpointsResponse,
  ListAppsResponse,
  LoginResponse,
  LogoutAllResponse,
  LogoutResponse,
  MeResponse,
  MetricsMode,
  PlacementView,
  RegisterResponse,
  RemoveEnvResponse,
  RemoveScalingPolicyResponse,
  RemoveSecretResponse,
  ResetUserPasswordResponse,
  RestoreDatabaseBackupResponse,
  ResumeDatabaseResponse,
  RetryDatabaseResponse,
  RevealDatabaseCredentialsResponse,
  RevokePlatformAdminResponse,
  RevokeTokenResponse,
  RollbackDeploymentResponse,
  RotateDatabaseCredentialsResponse,
  RotateJoinTokenResponse,
  RotateWebhookSecretResponse,
  SearchLogsResponse,
  SearchMetricsResponse,
  SearchSource,
  SetConfigResponse,
  GetConfigResponse,
  ListConfigsResponse,
  RemoveConfigResponse,
  SetDriftConvergeResponse,
  SetEnvResponse,
  SetMetricsModeResponse,
  SetScalingPolicyResponse,
  SetProjectMemberRoleResponse,
  SetAuditRetentionResponse,
  SetRegistrationResponse,
  SetSecretResponse,
  SetTeamMemberRoleResponse,
  ShowDriftResponse,
  ResumeAppResponse,
  SuspendAppResponse,
  SuspendDatabaseResponse,
  TeamView,
  TestWebhookResponse,
  TestS3ConnectionRequest,
  TestS3ConnectionResponse,
  TestDnsProviderRequest,
  TestDnsProviderResponse,
  TestSmtpResponse,
  TriggerBackupResponse,
  TriggerCronRunResponse,
  TriggerDatabaseBackupResponse,
  UpgradeDatabaseResponse,
  UpdateAcmeSettingsRequest,
  UpdateAcmeSettingsResponse,
  UpdateS3SettingsRequest,
  UpdateS3SettingsResponse,
  UpdateSmtpSettingsResponse,
  UpdateWebhookEndpointResponse,
  VerifyAppDomainsResponse,
  VolumeView,
  WebhookDeliveryStatus,
} from "./types";

// ── auth（v0.3 RBAC W1 认证面；proto fleetly/server/v1/auth.proto）──────
// 会话 = HttpOnly cookie fleetly_session（Register/Login 经 Set-Cookie 下
// 发，fetch credentials:"include" 携带——见 client.ts）；API token 路径仍
// 走 Bearer（双凭据：有 token 用 Bearer，否则 cookie）。认证面自身的 401
// 是业务结果（错口令/未登录探测），一律 optionalAuth 豁免全局未授权处置。

/** 自助注册（无用户窗口恒开；成功即下发会话 cookie——自动登录）。 */
export function register(input: {
  email: string;
  password: string;
  display_name?: string;
  /**
   * 可选一次性邀请 token（W3-S4 受邀注册通道，P1-1 前端接线）：服务端
   * 同事务现查 team_invites 豁免注册窗并在注册落位后消费（受邀角色入队
   * ——internal/state/register.go RegisterWrite.InviteToken 语义）；无效
   * token → 409 E_INVITE_INVALID。
   */
  invite_token?: string;
}) {
  return api<RegisterResponse>("/auth/register", {
    method: "POST",
    json: input,
    optionalAuth: true,
  });
}

/** 口令登录（成功即下发会话 cookie）。 */
export function login(email: string, password: string) {
  return api<LoginResponse>("/auth/login", {
    method: "POST",
    json: { email, password },
    optionalAuth: true,
  });
}

/** 注销当前会话（服务端删行 + 清 cookie；会话已失效时静默成功语义）。 */
export function logout() {
  return api<LogoutResponse>("/auth/logout", {
    method: "POST",
    json: {},
    optionalAuth: true,
  });
}

/** 全部注销（删除当前用户全部会话；响应携带吊销行数）。 */
export function logoutAll() {
  return api<LogoutAllResponse>("/auth/logout-all", {
    method: "POST",
    json: {},
    optionalAuth: true,
  });
}

/**
 * 当前身份投影（user + is_platform_admin + 所属团队与角色）。optionalAuth
 * 仅供启动探测使用——会话面内消费（用户菜单）保持默认：会话失效走全局
 * 登出。
 */
export function me(opts: { optionalAuth?: boolean } = {}) {
  return api<MeResponse>("/auth/me", { optionalAuth: opts.optionalAuth });
}

/** 消费一次性邀请（无效/过期/已消费 → 409 信封，调用方如实展示）。 */
export function acceptInvite(token: string) {
  return api<AcceptInviteResponse>("/auth/invite:accept", {
    method: "POST",
    json: { token },
  });
}

/** 注册窗口状态（登录页注册入口开关：open=true 或 has_users=false 显示）。 */
export function getRegistrationState() {
  return api<GetRegistrationStateResponse>("/auth/registration", {
    optionalAuth: true,
  });
}

// ── tokens / projects（v0.3 W2-S2 PAT 自服务页，rbac-teams 设计 §7）──────
// TokensService 用户化语义（§2.3）：本组端点是用户自服务面——列表只回自
// 己的 PAT；创建明文仅响应一次；吊销限自己的（平台管理员语义不经 Console
// 本页暴露）。

/** 当前用户的 PAT 列表（无敏感投影：备注/scope/绑定项目/哈希前缀）。 */
export function listTokens() {
  return api<ListTokensResponse>("/tokens");
}

/**
 * 创建 PAT：明文只在本次响应出现一次（服务端只存 sha256 哈希）；scopes
 * 声明不得超出角色可达集（越集 400 带指引）；project_id 可选绑定。
 */
export function createToken(input: {
  scopes: string[];
  note: string;
  project_id?: string;
}) {
  return api<CreateTokenResponse>("/tokens", { method: "POST", json: input });
}

/** 吊销自己的 PAT（幂等；可见集外/不存在一律 404 信封）。 */
export function revokeToken(id: string) {
  return api<RevokeTokenResponse>(`/tokens/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

/** 可见项目集（所在团队的项目；平台管理员全量——PAT 绑定下拉数据源）。 */
export function listProjects(opts: { team_id?: string } = {}) {
  const qs = opts.team_id ? `?team_id=${encodeURIComponent(opts.team_id)}` : "";
  return api<ListProjectsResponse>(`/projects${qs}`);
}

// ── teams（v0.3 W2-S5 团队设置页，rbac-teams §3.1/§7）────────────────────
// 团队面权限（teams.proto 头注）：读面 = 团队成员（平台管理员只读放行）；
// 写面 = 成员角色增删改 owner、邀请 owner/admin（所邀角色 ≤ 邀请者自身）。
// 机具令牌两面恒 403；服务端硬门，Console 按角色渲染只是体验门。

/** 建队：调用方成为 owner；slug 冲突 409、保留字 422。 */
export function createTeam(input: { slug: string; name: string }) {
  return api<{ team?: TeamView }>("/teams", { method: "POST", json: input });
}

/** 我所在团队；平台管理员 = 全部（只读 support 视角）。 */
export function listTeams() {
  return api<ListTeamsResponse>("/teams");
}

/**
 * 两段式删除（confirm = slug；项目须空——不做隐式级联）。confirm 走 query
 * 参数（teams.proto DeleteTeam 的 DELETE 无 body 绑定——与 deleteDatabase
 * 同款传输形态；本端点尚无 Console 消费方，形状先钉对，2026-09-25 同型排查）。
 */
export function deleteTeam(id: string, confirm: string) {
  return api<Record<string, never>>(
    `/teams/${encodeURIComponent(id)}?confirm=${encodeURIComponent(confirm)}`,
    { method: "DELETE" },
  );
}

export function listTeamMembers(teamId: string) {
  return api<ListTeamMembersResponse>(
    `/teams/${encodeURIComponent(teamId)}/members`,
  );
}

/** 调整成员角色（owner 专属；最后一名 owner 降级 → E_TEAM_LAST_OWNER）。 */
export function setTeamMemberRole(teamId: string, userId: string, role: string) {
  return api<SetTeamMemberRoleResponse>(
    `/teams/${encodeURIComponent(teamId)}/members:set-role`,
    { method: "POST", json: { user_id: userId, role } },
  );
}

/** 移出成员（owner 专属；联动清该团队全部项目覆写行）。 */
export function removeTeamMember(teamId: string, userId: string) {
  return api<Record<string, never>>(
    `/teams/${encodeURIComponent(teamId)}/members/${encodeURIComponent(userId)}`,
    { method: "DELETE" },
  );
}

/**
 * 创建邀请：明文 token 仅本次响应一次性返回（无 SMTP——链接直出供复制，
 * 链接形态 <origin>/ui/auth/invite?token=…）。
 */
export function createInvite(teamId: string, input: { email: string; role: string }) {
  return api<CreateInviteResponse>(
    `/teams/${encodeURIComponent(teamId)}/invites`,
    { method: "POST", json: input },
  );
}

export function listTeamInvites(teamId: string) {
  return api<ListTeamInvitesResponse>(
    `/teams/${encodeURIComponent(teamId)}/invites`,
  );
}

/** 吊销未消费邀请（已消费/已吊销按幂等成功）。 */
export function revokeInvite(teamId: string, inviteId: string) {
  return api<Record<string, never>>(
    `/teams/${encodeURIComponent(teamId)}/invites/${encodeURIComponent(inviteId)}:revoke`,
    { method: "POST", json: {} },
  );
}

// ── 项目网参与面（T 线 OT-1 / IMPL-T15-1）───────────────────────────────
// attach/detach 是显式 opt-in（缺省不参加——app 私网隔离现状）；成员服务
// 在下一次「参与变更重部署」中滚动切换网络。服务端权限 = admin scope +
// 项目角色 admin（团队 owner/admin；平台管理员只读不代写）。app 用平台 ID
// 寻址（REST 单段路径；管理面惯例，免疫跨项目同名歧义）。

/** 挂入项目网（幂等；重部署入队后成员服务滚动双挂）。 */
export function attachAppProjectNetwork(app: string) {
  return api<AttachAppProjectNetworkResponse>(
    `/apps/${encodeURIComponent(app)}/project-network`,
    { method: "POST" },
  );
}

/** 摘除项目网（幂等；成员服务在重部署中离开项目网）。 */
export function detachAppProjectNetwork(app: string) {
  return api<DetachAppProjectNetworkResponse>(
    `/apps/${encodeURIComponent(app)}/project-network`,
    { method: "DELETE" },
  );
}

// ── projects 覆写成员面（v0.3 W2-S5 项目设置 tab，rbac-teams §3.3）────────
// 项目生命周期写面（创建/删除）= 团队 owner（§3.2 矩阵）；覆写成员面 =
// 团队 admin/owner；覆写角色 ∈ admin/developer/viewer（owner 不可覆写）。

export function createProject(input: {
  team_id: string;
  slug: string;
  name: string;
  description?: string;
}) {
  return api<CreateProjectResponse>("/projects", { method: "POST", json: input });
}

export function deleteProject(id: string) {
  return api<Record<string, never>>(`/projects/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

/** 项目详情（读面门：成员/平台管理员只读）。 */
export function getProject(id: string) {
  return api<GetProjectResponse>(`/projects/${encodeURIComponent(id)}`);
}

/** 编辑 name/description（slug 不可变；服务端 owner 硬门——团队设置同规）。 */
export function updateProject(id: string, input: { name: string; description?: string }) {
  return api<UpdateProjectResponse>(`/projects/${encodeURIComponent(id)}`, {
    method: "PATCH",
    json: input,
  });
}

export function listProjectMembers(projectId: string) {
  return api<ListProjectMembersResponse>(
    `/projects/${encodeURIComponent(projectId)}/members`,
  );
}

/** 覆写 upsert（团队 admin/owner；目标须为团队成员；owner 不可覆写）。 */
export function setProjectMemberRole(projectId: string, userId: string, role: string) {
  return api<SetProjectMemberRoleResponse>(
    `/projects/${encodeURIComponent(projectId)}/members:set-role`,
    { method: "POST", json: { user_id: userId, role } },
  );
}

/** 摘除覆写行（回退团队角色）。 */
export function removeProjectMember(projectId: string, userId: string) {
  return api<Record<string, never>>(
    `/projects/${encodeURIComponent(projectId)}/members/${encodeURIComponent(userId)}`,
    { method: "DELETE" },
  );
}

// ── users（v0.3 W2-S5 平台管理员用户管理页，设计 §7）─────────────────────
// 全部方法 is_platform_admin 硬门（机具令牌不符格——users.proto 语义）；
// 临时口令/重置口令明文仅本次响应可见，页面一次性展示（无找回）。

export function listUsers() {
  return api<ListUsersResponse>("/users");
}

export function createUser(input: { email: string; display_name?: string }) {
  return api<CreateUserResponse>("/users", { method: "POST", json: input });
}

export function disableUser(id: string) {
  return api<DisableUserResponse>(`/users/${encodeURIComponent(id)}/disable`, {
    method: "POST",
    json: {},
  });
}

export function enableUser(id: string) {
  return api<EnableUserResponse>(`/users/${encodeURIComponent(id)}/enable`, {
    method: "POST",
    json: {},
  });
}

/** 重置口令：旧口令即死 + 该用户全部会话同事务吊销（重置即全端下线）。 */
export function resetUserPassword(id: string) {
  return api<ResetUserPasswordResponse>(
    `/users/${encodeURIComponent(id)}/password:reset`,
    { method: "POST", json: {} },
  );
}

export function grantPlatformAdmin(id: string) {
  return api<GrantPlatformAdminResponse>(
    `/users/${encodeURIComponent(id)}/platform-admin:grant`,
    { method: "POST", json: {} },
  );
}

export function revokePlatformAdmin(id: string) {
  return api<RevokePlatformAdminResponse>(
    `/users/${encodeURIComponent(id)}/platform-admin:revoke`,
    { method: "POST", json: {} },
  );
}

/** 注册窗口开关（PUT /v1/auth/registration；与 GET 同路径不同方法）。 */
export function setRegistration(open: boolean) {
  return api<SetRegistrationResponse>("/auth/registration", {
    method: "PUT",
    json: { open },
  });
}

// ── audit（v0.3 W3-S1 读面 + W3-S3 留存设置与浏览页，rbac-teams §6）───────
// 全部方法平台管理员硬门（用户 principal 须 is_platform_admin；机具令牌沿
// admin scope 门）。导出（CSV）不经 Console——诚实口径指向 CLI
// `fleetly audit export --csv`，Console 只做浏览。

/** 审计台账分页检索（at 倒序；total = 过滤生效、分页生效前的全量命中数）。 */
export function listAudit(
  opts: {
    actor?: string;
    action?: string;
    result?: "ok" | "error" | "";
    target?: string;
    since?: string;
    until?: string;
    limit?: number;
    offset?: number;
  } = {},
) {
  const query: Record<string, string> = {};
  if (opts.actor) query.actor = opts.actor;
  if (opts.action) query.action = opts.action;
  if (opts.result) query.result = opts.result;
  if (opts.target) query.target = opts.target;
  if (opts.since) query.since = opts.since;
  if (opts.until) query.until = opts.until;
  if (opts.limit !== undefined) query.limit = String(opts.limit);
  if (opts.offset !== undefined) query.offset = String(opts.offset);
  const qs = new URLSearchParams(query).toString();
  return api<ListAuditResponse>(`/audit${qs ? `?${qs}` : ""}`);
}

/** 审计留存设置读面（set=false = 未显式设置——Console 显示缺省口径）。 */
export function getAuditRetention() {
  return api<GetAuditRetentionResponse>("/audit/retention");
}

/** 审计留存天数设置（≥1；保存即生效——janitor 每拍现读）。 */
export function setAuditRetention(days: number) {
  return api<SetAuditRetentionResponse>("/audit/retention", {
    method: "PUT",
    json: { days },
  });
}

// ── apps ────────────────────────────────────────────────────────────────

/**
 * 列表按「调用方可见项目集」过滤（服务端）；opts.project 为可选收窄
 * （裸名或 `team/prj` 限定形——W2-S4，apps.proto ListAppsRequest.project）。
 */
export function listApps(opts: { project?: string } = {}) {
  const qs = opts.project ? `?project=${encodeURIComponent(opts.project)}` : "";
  return api<ListAppsResponse>(`/apps${qs}`);
}

export function getApp(name: string) {
  return api<GetAppResponse>(`/apps/${encodeURIComponent(name)}`);
}

export function deleteApp(name: string) {
  return api<{ name: string; lifecycle: string }>(
    `/apps/${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
}

// ── deployments ─────────────────────────────────────────────────────────

export function listDeployments(app: string, limit = 20) {
  return api<ListDeploymentsResponse>(
    `/apps/${encodeURIComponent(app)}/deployments?limit=${limit}`,
  );
}

export function getDeployment(id: string) {
  return api<{ deployment: DeploymentView }>(
    `/deployments/${encodeURIComponent(id)}`,
  );
}

/**
 * Deploy：compose 内容字节按 proto bytes 契约 base64 上行。opts.project
 * 透传 DeployRequest.project（裸名或 team/prj 限定形，proto deployments.proto
 * body: "*" 面字段——REST 无 query 形态）：新应用首次部署的归属声明（服务
 * 端 ensureApp 随首署建行）；缺省 = 服务端缺省（调用者个人队 default 项目）。
 * 既有消费方（应用详情页 DeployCard 部署既有应用）不传即载荷形态不变。
 */
export function deploy(app: string, composeText: string, opts: { project?: string } = {}) {
  return api<DeployResponse>(`/apps/${encodeURIComponent(app)}/deployments`, {
    method: "POST",
    rawBody: {
      compose: utf8ToBase64(composeText),
      ...(opts.project ? { project: opts.project } : {}),
    },
  });
}

export function cancelDeployment(id: string) {
  return api<CancelDeploymentResponse>(
    `/deployments/${encodeURIComponent(id)}/cancel`,
    { method: "POST", json: {} },
  );
}

export function rollbackDeployment(app: string, targetRevisionId?: string) {
  return api<RollbackDeploymentResponse>(
    `/apps/${encodeURIComponent(app)}/rollbacks`,
    {
      method: "POST",
      json: targetRevisionId ? { target_revision_id: targetRevisionId } : {},
    },
  );
}

/** SuspendApp 挂起应用（app Stop；admin）：API 只翻权威位——副本排水到 0 由
 * 引擎周期对账执行与保持（DB paused 同型「状态驱动渲染」）；挂起期部署入队/
 * drift/autoscaler/cron 按位豁免。重复挂起 409 E_APP_SUSPEND_CONFLICT。 */
export function suspendApp(name: string) {
  return api<SuspendAppResponse>(
    `/apps/${encodeURIComponent(name)}/suspend`,
    { method: "POST", json: {} },
  );
}

/** ResumeApp 恢复应用（app Start；admin）：清挂起位并入队 active revision
 * 的重部署（正常发布管线恢复副本）。应用从无成功部署时无物可恢复——
 * deployment_id 为空且清位照常生效。 */
export function resumeApp(name: string) {
  return api<ResumeAppResponse>(
    `/apps/${encodeURIComponent(name)}/resume`,
    { method: "POST", json: {} },
  );
}

// ── builds（构建台账面，T2.18；proto fleetly/server/v1/builds.proto）──────
// 服务端角色门（internal/api/scope.go 登记）：GetBuild / ListBuilds = read
//（所有角色可读——本组只封装读面）。TriggerBuild（POST /v1/builds）=
// admin scope 且载荷是 compose 内容字节、app 可自动建行——语义是 CLI
// `fleetly build <compose>` 的构建入口，不是「对既有应用的手动重建」，
// Console 不封装、Builds 页只读。单条构建 GetBuild 不封装：台账页的刷新
// 由 listBuilds 轮询覆盖，零消费的封装面不下沉（2026-09-25 复核）。

/** 构建台账（ListBuilds：路径参数即 app 过滤；created_at 倒序；天花板 100）。 */
export function listBuilds(app: string, limit = 20) {
  return api<ListBuildsResponse>(
    `/apps/${encodeURIComponent(app)}/builds?limit=${limit}`,
  );
}

// ── drift（运行域漂移面，T2.18；proto fleetly/server/v1/drift.proto）──────
// 服务端角色门（internal/api/scope.go 登记）：ShowDrift = read（所有角色
// 可读）；ConvergeDrift / SetDriftConverge = deploy（developer+，资源面写
// ——Console 侧体验门再按卡内设计收口）。收敛入队为准入重放，状态经部署
// 面跟踪（ConvergeDriftResponse.deployment_id 是收敛基准部署，非新在途行
// ——drift.proto 注释原文）；opt-in 位（apps.drift_converge）无读取面
// （ShowDrift/AppView 均不带）——Console 开关只做显式置位、回显响应。

/** 即时漂移判定（不写事件不收敛——与 CLI `fleetly drift show` 同源）。 */
export function showDrift(app: string) {
  return api<ShowDriftResponse>(`/apps/${encodeURIComponent(app)}/drift`);
}

/**
 * 人工一次性收敛（带审计，不经 opt-in 位；在途部署存在时 409
 * E_STATE_VERSION_CONFLICT）。proto 载荷只有 app（路径参数，body:"*" 下
 * 空 JSON 对象即全量请求）——CLI --confirm-destructive 是 deploy 动词的
 * flag，收敛契约无确认参数，本端点不携带。
 */
export function convergeDrift(app: string) {
  return api<ConvergeDriftResponse>(
    `/apps/${encodeURIComponent(app)}/drift/converge`,
    { method: "POST", json: {} },
  );
}

/** 收敛 opt-in 置位/重置（回滚失败强制关闭后的恢复路径；带审计）。 */
export function setDriftConvergence(app: string, enabled: boolean) {
  return api<SetDriftConvergeResponse>(
    `/apps/${encodeURIComponent(app)}/drift/convergence`,
    { method: "PUT", json: { enabled } },
  );
}

// ── runtime（应用运行实况面，2026-09-29 Console IA 重设计；
// proto fleetly/server/v1/runtime.proto）────────────────────────────────
// scope 登记 read（所有角色可读）。即时投影：服务集 = 期望集（最近
// succeeded 部署快照）∪ 实况集（受管长驻服务），每服务附全量任务（含
// 历史——desired_state=remove/shutdown 的旧任务仍在列表）。cron/init 一次
// 性 job 服务不物化长驻服务，本面不混入（任务台账在 cron-runs 读面）。

/** 应用运行实况（实际运行的任务/容器水位——Containers 页签数据源）。 */
export function getAppRuntime(app: string) {
  return api<ShowAppRuntimeResponse>(
    `/apps/${encodeURIComponent(app)}/runtime`,
  );
}

// ── apps webhook/git 触发面（T2.19；proto apps.proto）────────────────────
// scope.go 登记：ShowAppWebhook / SetAppWebhookSecret / SetAppSource 三 RPC
// 均 admin scope（secret 与认证材料写面 = admin——验签是 webhook 端点的唯一
// 认证；读面亦 admin——source URL 与分支拓扑属运维面）。用户 principal 另受
// 项目角色门（requireAppAccess → admin+），Console 侧 Deploy triggers 卡按
// canAdminResources 同口径收口读面（viewer/developer 见说明态）。

/** 回读 webhook/git 触发配置（无敏感投影：secret 只回 configured 位）。 */
export function showAppWebhook(app: string) {
  return api<ShowAppWebhookResponse>(
    `/apps/${encodeURIComponent(app)}/webhook`,
  );
}

/**
 * 设置 webhook 签名密钥（≥16 字符，服务端弱密钥显式拒绝；envelope 加密落
 * 库、永不回读）。轮换即时生效：验签按库存值单点判定，旧密钥签名的投递
 * 立即失败。proto min_len=16——REST 面无「清除」路径（空值 400）。
 */
export function setAppWebhookSecret(app: string, secret: string) {
  return api<SetAppWebhookSecretResponse>(
    `/apps/${encodeURIComponent(app)}/webhook-secret`,
    { method: "PUT", json: { secret } },
  );
}

/**
 * 设置 webhook 拉源（整体替换语义——认证材料加密落库无法「留旧」，每次
 * 调用写全量字段）。source_auth_kind ∈ none|https_token|ssh_key；kind 非
 * none 时 source_auth_secret 必带（≥16 字符，https_token 强制 https:// 源
 * ——服务端用例层交叉校验）。source_url 空 + kind none = 清除拉源配置
 *（internal/state AppSourceWrite.URL 空 = 清除，auth 字段一并清空）。
 */
export function setAppSource(app: string, input: SetAppSourceRequest) {
  return api<SetAppSourceResponse>(`/apps/${encodeURIComponent(app)}/source`, {
    method: "PUT",
    json: input,
  });
}

// ── revisions ───────────────────────────────────────────────────────────

export function listRevisions(app: string) {
  return api<ListRevisionsResponse>(
    `/apps/${encodeURIComponent(app)}/revisions`,
  );
}

export function getRevisionSpec(app: string, revisionId: string) {
  return api<GetRevisionSpecResponse>(
    `/apps/${encodeURIComponent(app)}/revisions/${encodeURIComponent(revisionId)}/spec`,
  );
}

// ── env ─────────────────────────────────────────────────────────────────

export function listEnv(app: string) {
  return api<ListEnvResponse>(`/apps/${encodeURIComponent(app)}/env`);
}

export function getEnv(app: string, key: string) {
  return api<GetEnvResponse>(
    `/apps/${encodeURIComponent(app)}/env/${encodeURIComponent(key)}`,
  );
}

export function setEnv(app: string, key: string, value: string) {
  return api<SetEnvResponse>(
    `/apps/${encodeURIComponent(app)}/env/${encodeURIComponent(key)}`,
    { method: "PUT", json: { value } },
  );
}

export function removeEnv(app: string, key: string) {
  return api<RemoveEnvResponse>(
    `/apps/${encodeURIComponent(app)}/env/${encodeURIComponent(key)}`,
    { method: "DELETE" },
  );
}

// ── domains（IMPL-T1-1：域名资源 CRUD + verify；label 仅首部署种子）─────

export function listDomains(app: string) {
  return api<ListAppDomainsResponse>(
    `/apps/${encodeURIComponent(app)}/domains`,
  );
}

export function createDomain(
  app: string,
  body: {
    domain: string;
    service: string;
    port: string;
    protocol: DomainProtocol;
    cert_mode: DomainCertMode;
  },
) {
  return api<CreateAppDomainResponse>(
    `/apps/${encodeURIComponent(app)}/domains`,
    { method: "POST", json: body },
  );
}

export function updateDomain(
  app: string,
  domain: string,
  body: {
    service: string;
    port: string;
    protocol: DomainProtocol;
    cert_mode: DomainCertMode;
  },
) {
  return api<UpdateAppDomainResponse>(
    `/apps/${encodeURIComponent(app)}/domains/${encodeURIComponent(domain)}`,
    { method: "PUT", json: body },
  );
}

export function removeDomain(app: string, domain: string) {
  return api<RemoveAppDomainResponse>(
    `/apps/${encodeURIComponent(app)}/domains/${encodeURIComponent(domain)}`,
    { method: "DELETE" },
  );
}

export function verifyDomains(app: string) {
  return api<VerifyAppDomainsResponse>(
    `/apps/${encodeURIComponent(app)}/domains/verify`,
    { method: "POST", json: {} },
  );
}

// ── logs（历史检索走 /logs；实时跟随走 stream.ts 的 NDJSON 流；日志库
// 统一检索走 /logs/search——W5-S2）──────────────────────────────────────

export function listHistoryLogs(
  app: string,
  opts: {
    service?: string;
    since?: string;
    until?: string;
    limit?: number;
    source?: "container" | "build" | "";
  } = {},
) {
  const query: Record<string, string> = {};
  if (opts.service) query.service = opts.service;
  if (opts.since) query.since = opts.since;
  if (opts.until) query.until = opts.until;
  if (opts.limit !== undefined) query.limit = String(opts.limit);
  if (opts.source) query.source = opts.source;
  const qs = new URLSearchParams(query).toString();
  return api<ListHistoryLogsResponse>(
    `/apps/${encodeURIComponent(app)}/logs${qs ? `?${qs}` : ""}`,
  );
}

/**
 * 日志库统一检索（VictoriaLogs LogsQL 后端）：时间倒序 + 游标分页（服务端
 * 签发 next_cursor）。sources/services 为可选过滤集（重复 query key 形态
 * ——gateway 对 repeated 字段同时接受重复键与 CSV）。VL 不可达 / jsonl 模
 * 式 → E_LOGS_BACKEND_UNAVAILABLE 信封（诚实报错，不返回空列表冒充）。
 */
export function searchLogs(
  app: string,
  opts: {
    keyword?: string;
    services?: string[];
    sources?: SearchSource[];
    since?: string;
    until?: string;
    limit?: number;
    cursor?: string;
  } = {},
) {
  const query: Record<string, string> = {};
  if (opts.keyword) query.keyword = opts.keyword;
  if (opts.since) query.time_start = opts.since;
  if (opts.until) query.time_end = opts.until;
  if (opts.limit !== undefined) query.limit = String(opts.limit);
  if (opts.cursor) query.cursor = opts.cursor;
  const params = new URLSearchParams(query);
  for (const s of opts.services ?? []) params.append("services", s);
  for (const s of opts.sources ?? []) params.append("sources", s);
  const qs = params.toString();
  return api<SearchLogsResponse>(
    `/apps/${encodeURIComponent(app)}/logs/search${qs ? `?${qs}` : ""}`,
  );
}

// ── metrics（E6 W5-S3，D-W5-2 opt-in）────────────────────────────────────

/**
 * 托管 metrics 栈状态视图：模式 / 三件部署态 / 「N/M nodes reporting」
 * 诚实口径 / retention。
 */
export function getMetricsStatus() {
  return api<GetMetricsStatusResponse>("/metrics/status");
}

/**
 * PromQL 区间查询（透传——操作员工具，无查询沙箱；设计 §4.2 诚实口径）。
 * query 为 PromQL 原文；step_seconds 缺省 60；limit 缺省 200。VM 不可达 /
 * metrics.mode=unset → 服务端信封诚实报错（E_METRICS_*）。
 */
export function searchMetrics(
  query: string,
  opts: { since?: string; until?: string; step_seconds?: number; limit?: number } = {},
) {
  const p: Record<string, string> = { query };
  if (opts.since) p.time_start = opts.since;
  if (opts.until) p.time_end = opts.until;
  if (opts.step_seconds !== undefined) p.step_seconds = String(opts.step_seconds);
  if (opts.limit !== undefined) p.limit = String(opts.limit);
  const qs = new URLSearchParams(p).toString();
  return api<SearchMetricsResponse>(`/metrics/search?${qs}`);
}

/** 模式切换（deploy scope）：保存即生效——后台收敛部署/移除，卷保留。 */
export function setMetricsMode(mode: MetricsMode) {
  return api<SetMetricsModeResponse>("/metrics/mode", { method: "PUT", json: { mode } });
}

// ── alerting（B 线 W5-S2，D-V3W5-1 告警面）────────────────────────────────

/** 规则清单（read scope；name 字典序——规则文件渲染同序）。 */
export function listAlertRules() {
  return api<ListAlertRulesResponse>("/alerting/rules");
}

/** 创建规则（admin scope + 平台写面；平台级唯一名）。 */
export function createAlertRule(input: {
  name: string;
  expr: string;
  for_duration_seconds?: number;
  labels?: Record<string, string>;
  channels?: string[];
}) {
  return api<CreateAlertRuleResponse>("/alerting/rules", { method: "POST", json: input });
}

/** 部分更新规则（admin scope + 平台写面；未提供字段不变）。 */
export function updateAlertRule(
  id: string,
  input: {
    name?: string;
    expr?: string;
    for_duration_seconds?: number;
    labels?: Record<string, string>;
    channels?: string[];
  },
) {
  return api<UpdateAlertRuleResponse>(
    `/alerting/rules/${encodeURIComponent(id)}`,
    { method: "PUT", json: input },
  );
}

/** 删除规则（admin scope + 平台写面；收敛管理器下一拍重渲染规则文件）。 */
export function deleteAlertRule(id: string) {
  return api<DeleteAlertRuleResponse>(
    `/alerting/rules/${encodeURIComponent(id)}`,
    { method: "DELETE" },
  );
}

/** alerts.mode 切换（deploy scope + 平台写面；前置门 metrics.mode=on）。 */
export function setAlertsMode(mode: AlertsMode) {
  return api<SetAlertsModeResponse>("/alerting/mode", { method: "PUT", json: { mode } });
}

/** 告警栈状态视图（read scope）：mode/vmalert 部署态/规则数/metrics.mode。 */
export function getAlertsStatus() {
  return api<GetAlertsStatusResponse>("/alerting/status");
}

/** TestAlertRule：expr 经 VM instant query 单次求值（规则编写即时校验面）。 */
export function testAlertRule(expr: string) {
  return api<TestAlertRuleResponse>("/alerting/rules:test", { method: "POST", json: { expr } });
}

/** AlertRuleView 重导出（表单投影消费）。 */
export type { AlertRuleView };

// ── apps scaling（W5-S1，D-V3W5-2 自动扩缩策略面）────────────────────────

/**
 * 读取服务的自动扩缩策略（read scope）。未设置 = 404（未配置即无策略
 ——服务端信封，调用面按 404 归一「无策略」态）。
 */
export function getScalingPolicy(app: string, service: string) {
  return api<GetScalingPolicyResponse>(
    `/apps/${encodeURIComponent(app)}/scaling/${encodeURIComponent(service)}`,
  );
}

/**
 * 写入（整行替换 upsert）服务的自动扩缩策略（deploy scope；Console 卡按
 * 设计 §1.1「admin 可写」再收紧前端门）。约束：min ≥1、max ≤16、target ∈
 * [20,90]（0 = 该维度不设目标，至少一维必设）、cooldown ∈ [60,3600]s。
 */
export function setScalingPolicy(
  app: string,
  service: string,
  input: {
    min_replicas: number;
    max_replicas: number;
    target_cpu_pct: number;
    target_mem_pct: number;
    cooldown_seconds: number;
  },
) {
  return api<SetScalingPolicyResponse>(
    `/apps/${encodeURIComponent(app)}/scaling/${encodeURIComponent(service)}`,
    { method: "PUT", json: input },
  );
}

/** 删除服务的自动扩缩策略（deploy scope；运行期副本覆盖随删）。 */
export function removeScalingPolicy(app: string, service: string) {
  return api<RemoveScalingPolicyResponse>(
    `/apps/${encodeURIComponent(app)}/scaling/${encodeURIComponent(service)}`,
    { method: "DELETE" },
  );
}

// ── notifications（E6 W5-S4 通知 Webhook；observability §5 + §8 通道扩展）──

/** 端点清单（无敏感投影——secret 只出指纹）。 */
export function listWebhookEndpoints() {
  return api<ListWebhookEndpointsResponse>("/notifications/endpoints");
}

/** 单端点视图。 */
export function getWebhookEndpoint(id: string) {
  return api<GetWebhookEndpointResponse>(
    `/notifications/endpoints/${encodeURIComponent(id)}`,
  );
}

/**
 * 创建端点（admin scope）：签名密钥明文仅在响应出现一次，读面只出指纹。
 * 通道类型 webhook|slack|email（W4-S3）：webhook/slack 带 url（内网
 * receiver 允许 http——Console 出警示文案）；email 带 target 收件地址，
 * 投递凭据走平台级 SMTP 设置面。
 */
export function createWebhookEndpoint(input: {
  name: string;
  url: string;
  event_patterns: string[];
  enabled?: boolean;
  type?: string;
  target?: string;
}) {
  return api<CreateWebhookEndpointResponse>("/notifications/endpoints", {
    method: "POST",
    json: input,
  });
}

/**
 * 部分更新（admin scope）：未提供的字段不变；event_patterns 空 = 不变、
 * 非空 = 整体替换。type/target/url 组合形状在服务端对最终形态校验
 * （换通道时 url/target 可显式清空）。
 */
export function updateWebhookEndpoint(
  id: string,
  patch: {
    name?: string;
    url?: string;
    event_patterns?: string[];
    enabled?: boolean;
    type?: string;
    target?: string;
  },
) {
  return api<UpdateWebhookEndpointResponse>(
    `/notifications/endpoints/${encodeURIComponent(id)}`,
    { method: "PUT", json: patch },
  );
}

/** 删除端点（admin scope）：投递台账行随之清理。 */
export function deleteWebhookEndpoint(id: string) {
  return api<DeleteWebhookEndpointResponse>(
    `/notifications/endpoints/${encodeURIComponent(id)}`,
    { method: "DELETE" },
  );
}

/** 轮换签名密钥（admin scope）：新明文仅本次响应可见。 */
export function rotateWebhookSecret(id: string) {
  return api<RotateWebhookSecretResponse>(
    `/notifications/endpoints/${encodeURIComponent(id)}/rotate-secret`,
    { method: "POST", json: {} },
  );
}

/** 发送 type=test 载荷（admin scope；按端点通道类型试发，同步结论——RPC
 * 名沿契约门禁保留 TestWebhook，语义即 TestEndpoint）。 */
export function testWebhook(id: string) {
  return api<TestWebhookResponse>(
    `/notifications/endpoints/${encodeURIComponent(id)}/test`,
    { method: "POST", json: {} },
  );
}

/** 投递台账（按端点/状态过滤，最新在前——重试与终败的诚实可见面）。 */
export function listWebhookDeliveries(
  opts: { endpoint_id?: string; status?: WebhookDeliveryStatus; limit?: number } = {},
) {
  const p: Record<string, string> = {};
  if (opts.endpoint_id) p.endpoint_id = opts.endpoint_id;
  if (opts.status) p.status = opts.status;
  if (opts.limit !== undefined) p.limit = String(opts.limit);
  const qs = new URLSearchParams(p).toString();
  return api<ListWebhookDeliveriesResponse>(`/notifications/deliveries${qs ? `?${qs}` : ""}`);
}

/** 平台级 SMTP 设置只读面（密码只出指纹；email 端点共用一份——W4-S3）。 */
export function getSmtpSettings() {
  return api<GetSmtpSettingsResponse>("/notifications/smtp");
}

/** 保存平台级 SMTP 设置（PUT 语义；密码明文只写不读，空 = 清除）。 */
export function updateSmtpSettings(input: {
  host: string;
  port: number;
  username?: string;
  password?: string;
  from: string;
}) {
  return api<UpdateSmtpSettingsResponse>("/notifications/smtp", {
    method: "PUT",
    json: input,
  });
}

/** SMTP 探针（候选或已存配置发测试邮件到指定 to——真实 SMTP 往返）。 */
export function testSmtp(input: {
  to: string;
  host?: string;
  port?: number;
  username?: string;
  password?: string;
  from?: string;
}) {
  return api<TestSmtpResponse>("/notifications/smtp/test", {
    method: "POST",
    json: input,
  });
}

// ── system ──────────────────────────────────────────────────────────────

export function getSystemStatus() {
  return api<GetSystemStatusResponse>("/system/status");
}

export function listNodes() {
  return api<ListNodesResponse>("/system/nodes");
}

/** join 向导（E1-8；admin scope——响应含 join token 材料）。 */
export function getJoinGuide(workerIp?: string, managerAddr?: string) {
  const query: Record<string, string> = {};
  if (workerIp) query.worker_ip = workerIp;
  if (managerAddr) query.manager_addr = managerAddr;
  const qs = new URLSearchParams(query).toString();
  return api<GetJoinGuideResponse>(
    `/system/nodes/join-guide${qs ? `?${qs}` : ""}`,
  );
}

/** 轮换 swarm join token（E1-8，D-MN-1；旧 token 即刻失效）。 */
export function rotateJoinToken(role: "worker" | "manager" = "worker") {
  return api<RotateJoinTokenResponse>("/system/nodes/join-token:rotate", {
    method: "POST",
    json: { role },
  });
}

export function getIngressStatus() {
  return api<GetIngressStatusResponse>("/system/ingress");
}

// ── backups（状态备份台账，T2.22/E3-3）──────────────────────────────────

/** 台账只读面（created_at 倒序）。 */
export function listBackups() {
  return api<ListBackupsResponse>("/system/backups");
}

/** 手动触发一次状态备份（响应即落账后的台账行）。 */
export function triggerBackup() {
  return api<TriggerBackupResponse>("/system/backups", {
    method: "POST",
    json: { kind: "manual" },
  });
}

// ── S3 设置面（E3 对象存储 §5.1/E3-2，admin scope）──────────────────────

/** 设置只读投影：secret 只回 fingerprint，读面永无明文。 */
export function getS3Settings() {
  return api<GetS3SettingsResponse>("/system/s3");
}

/** 全量保存（PUT 语义：请求即新状态；secret 明文只写）。 */
export function updateS3Settings(req: UpdateS3SettingsRequest) {
  return api<UpdateS3SettingsResponse>("/system/s3", { method: "PUT", json: req });
}

/**
 * 连接探针（put→get→delete 单轮真实读写）：传候选配置即「先测后存」；
 * 全空 = 测已存配置。
 */
export function testS3Connection(req: TestS3ConnectionRequest) {
  return api<TestS3ConnectionResponse>("/system/s3:test", {
    method: "POST",
    json: req,
  });
}

// ── ACME DNS-01 设置面（B 线 W5 设计 §3，D-V3W5-3/D-V3W5-4，admin scope）──

/** ACME 设置只读投影：凭证只回 fingerprint，读面永无明文。 */
export function getAcmeSettings() {
  return api<GetAcmeSettingsResponse>("/system/acme");
}

/** 保存 provider/凭证/wildcard（api_token 明文只写；留空 = 保留已存凭证）。 */
export function updateAcmeSettings(req: UpdateAcmeSettingsRequest) {
  return api<UpdateAcmeSettingsResponse>("/system/acme", { method: "PUT", json: req });
}

/**
 * DNS 服务商探针（create→delete 真实 TXT _acme-challenge-test.<base>）：
 * 传候选凭证即「先测后存」；全空 = 测已存凭证。
 */
export function testDnsProvider(req: TestDnsProviderRequest) {
  return api<TestDnsProviderResponse>("/system/acme/dns:test", {
    method: "POST",
    json: req,
  });
}

// ── cron（E5 Cron，架构 §4.3）───────────────────────────────────────────

/**
 * 手动触发一次（与到点触发同链路：重叠/节点不可用不报错——响应携带
 * skipped 行与原因）。
 */
export function triggerCronRun(app: string, service: string) {
  return api<TriggerCronRunResponse>(
    `/apps/${encodeURIComponent(app)}/services/${encodeURIComponent(service)}/trigger`,
    { method: "POST", json: {} },
  );
}

/** 运行台账（scheduled_at 倒序；service 空 = 该 app 全部 schedule 的行）。 */
export function listCronRuns(app: string, service?: string, limit = 20) {
  const query: Record<string, string> = {};
  if (service) query.service = service;
  if (limit !== undefined) query.limit = String(limit);
  const qs = new URLSearchParams(query).toString();
  return api<ListCronRunsResponse>(
    `/apps/${encodeURIComponent(app)}/cron-runs${qs ? `?${qs}` : ""}`,
  );
}

export function getPlacement(app: string) {
  return api<{
    app: string;
    placement?: PlacementView;
    volumes?: VolumeView[];
  }>(`/apps/${encodeURIComponent(app)}/placement`);
}

// ── databases（E4 数据库托管，managed-databases §5.1/§5.2）───────────────

/** 列表可见性过滤同 apps；opts.project 可选收窄（裸名或 team/prj 限定形）。 */
export function listDatabases(opts: { project?: string } = {}) {
  const qs = opts.project ? `?project=${encodeURIComponent(opts.project)}` : "";
  return api<ListDatabasesResponse>(`/databases${qs}`);
}

export function getDatabase(name: string) {
  return api<GetDatabaseResponse>(`/databases/${encodeURIComponent(name)}`);
}

export function createDatabase(req: {
  name: string;
  template: string;
  /** 目标项目（裸名或 team/prj 限定形）；缺省 = 调用者个人队 default。 */
  project?: string;
  limits?: { cpu_seconds?: number; memory_bytes?: string };
}) {
  return api<CreateDatabaseResponse>("/databases", { method: "POST", json: req });
}

/**
 * 删除受理（破坏性两段式）：confirm/delete_volumes 走 **query 参数**——
 * proto http rule 对 DELETE 只声明路径段（databases.proto DeleteDatabase，
 * 无 body 绑定），grpc-gateway 把非路径字段从 query 读取；此前以 JSON body
 * 上送会被网关整体忽略，confirm 恒空 → 400 "destructive operation: pass
 * confirm=…"（2026-09-25 走查实录：确认框输名了但载荷从未到达服务端）。
 * delete_volumes 序列化为 proto3 bool 的 JSON 形态（true/false 字面量）。
 */
export function deleteDatabase(name: string, opts: { confirm: string; delete_volumes: boolean }) {
  const qs = `confirm=${encodeURIComponent(opts.confirm)}&delete_volumes=${opts.delete_volumes}`;
  return api<DeleteDatabaseResponse>(`/databases/${encodeURIComponent(name)}?${qs}`, {
    method: "DELETE",
  });
}

export function suspendDatabase(name: string) {
  return api<SuspendDatabaseResponse>(`/databases/${encodeURIComponent(name)}/suspend`, {
    method: "POST",
    json: {},
  });
}

export function resumeDatabase(name: string) {
  return api<ResumeDatabaseResponse>(`/databases/${encodeURIComponent(name)}/resume`, {
    method: "POST",
    json: {},
  });
}

export function retryDatabase(name: string) {
  return api<RetryDatabaseResponse>(`/databases/${encodeURIComponent(name)}/retry`, {
    method: "POST",
    json: {},
  });
}

export function rotateDatabaseCredentials(name: string, confirm: string) {
  return api<RotateDatabaseCredentialsResponse>(`/databases/${encodeURIComponent(name)}/rotate`, {
    method: "POST",
    json: { confirm },
  });
}

/**
 * 连接信息显式展开（admin 面动作；密码明文只出现在本响应——显式 reveal
 * 才取，取到即前台展示、隐藏即弃，不做任何持久化）。
 */
export function revealDatabaseCredentials(name: string) {
  return api<RevealDatabaseCredentialsResponse>(
    `/databases/${encodeURIComponent(name)}/credentials`,
  );
}

export function listDatabaseBackups(name: string, limit = 20) {
  return api<ListDatabaseBackupsResponse>(
    `/databases/${encodeURIComponent(name)}/backups?limit=${limit}`,
  );
}

export function triggerDatabaseBackup(name: string) {
  return api<TriggerDatabaseBackupResponse>(`/databases/${encodeURIComponent(name)}/backups`, {
    method: "POST",
    json: { kind: "manual" },
  });
}

export function restoreDatabaseBackup(name: string, snapshot: string, confirm: string) {
  return api<RestoreDatabaseBackupResponse>(`/databases/${encodeURIComponent(name)}/restore`, {
    method: "POST",
    json: { snapshot, confirm },
  });
}

export function upgradeDatabase(name: string, confirm: string) {
  return api<UpgradeDatabaseResponse>(`/databases/${encodeURIComponent(name)}/upgrade`, {
    method: "POST",
    json: { confirm },
  });
}

// ── platform secrets（E4 §2.7 平台密钥库；无值读回）─────────────────────

export function listSecrets(app: string) {
  return api<ListSecretsResponse>(`/apps/${encodeURIComponent(app)}/secrets`);
}

export function setSecret(app: string, name: string, value: string) {
  return api<SetSecretResponse>(`/apps/${encodeURIComponent(app)}/secrets`, {
    method: "POST",
    json: { app, name, value },
  });
}

export function removeSecret(app: string, name: string) {
  return api<RemoveSecretResponse>(
    `/apps/${encodeURIComponent(app)}/secrets/${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
}

// ── app configs（T 线 OT-3 明文配置资源；Get 明文回读按 admin 门）─────────

export function listConfigs(app: string) {
  return api<ListConfigsResponse>(`/apps/${encodeURIComponent(app)}/configs`);
}

/** Get 明文回读（admin scope；值逐字返回——调用方直接呈现/复制）。 */
export function getConfig(app: string, name: string) {
  return api<GetConfigResponse>(
    `/apps/${encodeURIComponent(app)}/configs/${encodeURIComponent(name)}`,
  );
}

export function setConfig(app: string, name: string, value: string) {
  return api<SetConfigResponse>(`/apps/${encodeURIComponent(app)}/configs`, {
    method: "POST",
    json: { app, name, value },
  });
}

export function removeConfig(app: string, name: string) {
  return api<RemoveConfigResponse>(
    `/apps/${encodeURIComponent(app)}/configs/${encodeURIComponent(name)}`,
    { method: "DELETE" },
  );
}

// ── terminal（E7 W5-S6 Web 终端；web-terminal §2.5）──────────────────────
// WS 数据面不走 REST（native 端点 /v1/terminal?ticket=...，帧协议在
// execrelay）——本文件只承接 ticket 受理与状态两个 proto 面 RPC。

export function createTerminalTicket(app: string, service: string) {
  return api<CreateTerminalTicketResponse>("/terminal/tickets", {
    method: "POST",
    json: { app, service },
  });
}

export function getTerminalStatus() {
  return api<GetTerminalStatusResponse>("/terminal/status");
}
