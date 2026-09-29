// 本文件由 openapi-typescript 从 genproto/fleetly/server/v1/*.swagger.json 生成
// （console/scripts/gen-api.mjs，`pnpm gen:api`）——不要手改；proto 变更后
// 重新生成并提交。CI（pr.yml console job）以"再生成无 diff"门禁拦截漂移。
// 字段名/类型语义：UseProtoNames（snake_case 声明名）+ proto3 JSON 映射
// （int64 → 字符串；gateway EmitUnpopulated=true → 零值字段显式输出〔标量
// 零值/null〔unset message 与 Timestamp〕/[]〕，全部属性可选）。

export interface paths {
    "/v1/auth/invite:accept": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * AcceptInvite 消费一次性邀请（设计 §3.1：W1 落面、合法消费 store 原语
         *     ——邀请的产生面（TeamsService.CreateInvite）随 W2 落地）。
         */
        post: operations["AuthService_AcceptInvite"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/auth/login": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post: operations["AuthService_Login"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/auth/logout": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** Logout 注销当前会话（需会话 cookie 凭据；删除会话行 + 清除 cookie）。 */
        post: operations["AuthService_Logout"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/auth/logout-all": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** LogoutAll 全部注销：删除当前用户全部会话（含本会话）。 */
        post: operations["AuthService_LogoutAll"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/auth/me": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Me 当前身份投影：user + is_platform_admin + 所属团队与角色（团队面
         *     W2 落地前的只读投影）。
         */
        get: operations["AuthService_Me"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/auth/register": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Register 自助注册：无用户窗口恒开（首注册者即平台管理员，同事务建
         *     个人 Team + 默认 Project `default`，并吊销 bootstrap token——零残留
         *     后门）；users 非空后由注册开关管辖（关闭 → E_REGISTRATION_CLOSED）。
         */
        post: operations["AuthService_Register"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/auth/registration": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetRegistrationState 登录页开关注册入口（设计 §5/§7）：无用户窗口恒
         *     返回 open=true（has_users=false）。
         */
        get: operations["AuthService_GetRegistrationState"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/tokens": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["TokensService_ListTokens"];
        put?: never;
        post: operations["TokensService_CreateToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/tokens/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        delete: operations["TokensService_RevokeToken"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/teams": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** ListTeams 我所在团队；平台管理员 = 全部（§3.2 只读全域）。 */
        get: operations["TeamsService_ListTeams"];
        put?: never;
        /**
         * CreateTeam 建队：调用方成为 owner（§3.1）；slug 冲突 409、保留字
         *     422（E_TEAM_SLUG_RESERVED）。
         */
        post: operations["TeamsService_CreateTeam"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/teams/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["TeamsService_GetTeam"];
        put?: never;
        post?: never;
        /**
         * DeleteTeam owner 专属两段式（confirm = slug）+ 项目须空（资源先迁走
         *     或删光，不做隐式级联，§3.1）。
         */
        delete: operations["TeamsService_DeleteTeam"];
        options?: never;
        head?: never;
        /** UpdateTeam 仅改显示名（slug 不可变——请求无 slug 字段）。 */
        patch: operations["TeamsService_UpdateTeam"];
        trace?: never;
    };
    "/v1/teams/{team_id}/invites": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["TeamsService_ListTeamInvites"];
        put?: never;
        /**
         * CreateInvite owner/admin 可邀（§3.1）：所邀角色不得高于邀请者自身；
         *     邀 owner 角色 = owner 专属。明文 token 仅本次响应一次性返回（无 SMTP：
         *     邀请链接页面直出供复制）。
         */
        post: operations["TeamsService_CreateInvite"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/teams/{team_id}/invites/{invite_id}:revoke": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** RevokeInvite 吊销未消费邀请（已消费/已吊销按幂等成功）。 */
        post: operations["TeamsService_RevokeInvite"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/teams/{team_id}/members": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["TeamsService_ListTeamMembers"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/teams/{team_id}/members/{user_id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * RemoveTeamMember owner 专属：移出成员（联动清该团队全部项目覆写行，
         *     D-W0-2；最后一名 owner → E_TEAM_LAST_OWNER）。
         */
        delete: operations["TeamsService_RemoveTeamMember"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/teams/{team_id}/members:set-role": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * SetTeamMemberRole owner 专属：调整成员角色（最后一名 owner 降级 →
         *     E_TEAM_LAST_OWNER）。
         */
        post: operations["TeamsService_SetTeamMemberRole"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/project-network": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * AttachAppProjectNetwork 把 app 挂入其项目网（T 线 OT-1 / IMPL-T15-1 的
         *     显式 opt-in；项目由 app 行归属解析——请求不携带项目，避免「指向别的
         *     项目网」的歧义面）。语义（全部幂等）：
         *       1. 确保项目网 overlay 在位（平台建 `fleetly-project-<project_id>`，
         *          带自描述 label；缺失创建、已存在不覆盖）；
         *       2. 参与位置位（状态库唯一改变路径；审计 app.project_network_attached
         *          + 事件 project.network_changed 同事务）；
         *       3. 入队「参与变更重部署」（复用最近 succeeded 部署的 compose——新
         *          revision 的成员服务双挂项目网，滚动收敛；无部署史 = 仅置位）。
         *     权限：admin scope + 项目角色 admin（网络姿态改变是隔离面的敏感写，
         *     与 app 删除/secrets 同级；平台管理员只读不代写）。在途部署存在时 409
         *     拒绝（重部署会以旧快照覆盖在途发布）。
         */
        post: operations["ProjectsService_AttachAppProjectNetwork"];
        /**
         * DetachAppProjectNetwork 从项目网摘除 app（摘网随下一次重部署的服务
         *     滚动收敛；项目网在失去最后一名成员且零端点后由平台回收）。权限与
         *     在途守卫同 Attach。
         */
        delete: operations["ProjectsService_DetachAppProjectNetwork"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/projects": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListProjects 我可见项目（团队归属集）；带 team_id 收窄到单队（须为
         *     该队成员或平台管理员）；平台管理员不带过滤 = 全部。
         */
        get: operations["ProjectsService_ListProjects"];
        put?: never;
        /** CreateProject 在团队下建项目（owner 专属）；team 内 slug 冲突 409。 */
        post: operations["ProjectsService_CreateProject"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/projects/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["ProjectsService_GetProject"];
        put?: never;
        post?: never;
        /**
         * DeleteProject 删除空项目：项目内有存活资源（apps 非 tombstone /
         *     db_instances 非 deleted 终态）即拒绝（409——资源先迁走或删光，不做
         *     隐式级联，§3.1）。
         */
        delete: operations["ProjectsService_DeleteProject"];
        options?: never;
        head?: never;
        /** UpdateProject 仅改 name/description（slug 不可变——请求无 slug 字段）。 */
        patch: operations["ProjectsService_UpdateProject"];
        trace?: never;
    };
    "/v1/projects/{project_id}/members": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["ProjectsService_ListProjectMembers"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/projects/{project_id}/members/{user_id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /** RemoveProjectMember 删除覆写行（该成员回退用团队角色）。 */
        delete: operations["ProjectsService_RemoveProjectMember"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/projects/{project_id}/members:set-role": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * SetProjectMemberRole 队内覆写 upsert（团队 admin/owner，D-W0-2 B 形）：
         *     仅限团队成员；owner 不可覆写（409）；有行则改角色、无行则插入。
         */
        post: operations["ProjectsService_SetProjectMemberRole"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/projects/{to_project_id}:move-app": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * MoveApp 资源改派（v0.3 W2-S3，rbac-teams §3.4/§5；平台管理员专属）：
         *     写归属 + 换名重部署（D-W0-4 二修——底座命名三段以归属为参数）。无卷
         *     服务近零中断（建新名→等 running→删旧名）；有卷短暂停机窗口。跨团队/
         *     跨项目改派先过目标项目唯一性（409）。app 引用 = 裸名（解析域内唯一）
         *     或 `team/prj/app` 限定形或平台 ID；目标项目 = 平台 ID（D-W0-9：管理面
         *     用 ID，免疫重名）。
         */
        post: operations["ProjectsService_MoveApp"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/projects/{to_project_id}:move-database": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * MoveDatabase 资源改派（同 MoveApp 语义；库卷公式不变——零卷迁移/
         *     零数据搬移，换名重部署引用同一物理卷）。
         */
        post: operations["ProjectsService_MoveDatabase"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/audit": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["AuditService_ListAudit"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["AppsService_ListApps"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["AppsService_GetApp"];
        put?: never;
        post?: never;
        delete: operations["AppsService_DeleteApp"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{name}/resume": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * ResumeApp 恢复应用（app Start；admin 门）：清挂起位并入队 active
         *     revision 的重部署（重放快照走正常发布管线恢复副本——响应带
         *     deployment_id 供跟踪；应用从无成功部署时无物可恢复，deployment_id
         *     为空且清位照常生效）。
         */
        post: operations["AppsService_ResumeApp"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{name}/scaling/{service}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetScalingPolicy 读取服务的自动扩缩策略（W5-S1，D-V3W5-2；read 门——
         *     策略是应用运行面的事实视图）。未设置返回 404（未配置即无策略）。
         */
        get: operations["AppsService_GetScalingPolicy"];
        /**
         * SetScalingPolicy 写入（整行替换 upsert）服务的自动扩缩策略（deploy 门
         *     ——资源面写语义，与 Deploy/SetEnv 同级；用户 principal 另受项目角色门
         *     约束，机具令牌 admin 等价照旧）。约束：min ≥1、max ≤16、target ∈
         *     [20,90]（0 = 该维度不设目标，至少一维必设）、cooldown ∈ [60,3600]s
         *     缺省 180。生效前置：metrics.mode=on 且服务 running（metrics off 时
         *     策略休眠——scaling.dormant 事件一次性披露）；有卷服务只扩不缩。
         */
        put: operations["AppsService_SetScalingPolicy"];
        post?: never;
        /**
         * RemoveScalingPolicy 删除服务的自动扩缩策略（deploy 门）。同键运行期
         *     副本覆盖一并清除——期望副本回落 compose 快照（外部改动照常走漂移判据）。
         */
        delete: operations["AppsService_RemoveScalingPolicy"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{name}/source": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * SetAppSource 设置 webhook 拉源配置（remote url + 分支 + 认证形态；
         *     admin）。source_branch 同时是 webhook 投递的触发分支（app 配置分支，
         *     默认 main）。
         *     认证材料（https_token/ssh_key）经平台 envelope 加密落库，引用不落明文。
         */
        put: operations["AppsService_SetAppSource"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{name}/suspend": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * SuspendApp 挂起应用（app Stop；admin 门，与 SuspendDatabase 同级——
         *     整应用停摆是大爆炸半径动作）。权威位翻转（apps.suspended，API 只转
         *     位）——引擎周期对账随后把受管长驻服务排水到副本 0（服务对象保留，
         *     引用方连不上是诚实暴露）；cron 调度/手动触发、部署入队、drift、
         *     autoscaler 挂起期按位豁免。重复挂起 409（幂等面由读面投影消化）。
         */
        post: operations["AppsService_SuspendApp"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{name}/webhook": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ShowAppWebhook 回读 webhook/git 触发配置（无敏感投影：secret 只回
         *     configured 位；admin scope——source URL 与分支拓扑属运维面）。
         */
        get: operations["AppsService_ShowAppWebhook"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{name}/webhook-secret": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * SetAppWebhookSecret 设置 per-app webhook 签名密钥（T2.19；admin）。
         *     值经平台 envelope 加密落库（明文不落），show 面只回 configured 位。
         *     未配置 = webhook 端点未启用（404 语义）。
         */
        put: operations["AppsService_SetAppWebhookSecret"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/deployments": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["DeploymentsService_ListDeployments"];
        put?: never;
        post: operations["DeploymentsService_Deploy"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/rollbacks": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post: operations["DeploymentsService_RollbackDeployment"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/deployments/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["DeploymentsService_GetDeployment"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/deployments/{id}/cancel": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post: operations["DeploymentsService_CancelDeployment"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/builds": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["BuildsService_ListBuilds"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/builds": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post: operations["BuildsService_TriggerBuild"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/builds/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["BuildsService_GetBuild"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/drift": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["DriftService_ShowDrift"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/drift/converge": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post: operations["DriftService_ConvergeDrift"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/drift/convergence": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put: operations["DriftService_SetDriftConverge"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/runtime": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["RuntimeService_ShowAppRuntime"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/revisions": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["RevisionsService_ListRevisions"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/revisions/{revision_id}/spec": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["RevisionsService_GetRevisionSpec"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/env": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["EnvService_ListEnv"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/env/{key}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["EnvService_GetEnv"];
        put: operations["EnvService_SetEnv"];
        post?: never;
        delete: operations["EnvService_RemoveEnv"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/domains": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["DomainsService_ListAppDomains"];
        put?: never;
        /**
         * CreateAppDomain 新建域名资源（host 全局独占：冲突 409 E_DOMAIN_CONFLICT；
         *     每服务 ≤5、每 app ≤10 超限 4xx E_DOMAIN_UNSUPPORTED）。写入成功后同步
         *     触发入口收敛（路由发布 + 证书保障；失败以事件/审计披露，资源行保留）。
         */
        post: operations["DomainsService_CreateAppDomain"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/domains/verify": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post: operations["DomainsService_VerifyAppDomains"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/domains/{domain}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * UpdateAppDomain 更新既有域名行（{domain} 是寻址键 = host，不改名——
         *     改名 = 删除 + 重建，与 env key 同口径）。空字段 = 保持现值（CLI 局部
         *     更新形态；Console 恒发全量）。
         */
        put: operations["DomainsService_UpdateAppDomain"];
        post?: never;
        /**
         * RemoveAppDomain 删除域名行（幂等不做：不存在 404）。删除即触发入口
         *     收敛（路由撤销；证书 SAN 集变化随下次签发收敛）。
         */
        delete: operations["DomainsService_RemoveAppDomain"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/logs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["LogsService_ListHistoryLogs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/logs/search": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * SearchLogs 统一检索（E6 观测专项设计 §3.1，W5-S1）：日志库
         *     （VictoriaLogs）LogsQL 检索面。keyword 构造为转义后的字面量短语
         *     （用户输入永不裸拼进查询串）；VL 不可达或当前 backend=jsonl 时以
         *     E_LOGS_BACKEND_UNAVAILABLE 诚实报错（不返回空列表冒充）。检索面只
         *     覆盖入湖窗口内的日志（切换前的 JSONL 历史不在检索面——设计 §2.3
         *     「检索不跨界」的诚实边界）。
         */
        get: operations["LogsService_SearchLogs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/logs/stream": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["LogsService_FollowLogs"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/logs-backend": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetLogsBackend 日志后端视图（W5-S1：mode / 是否显式设置 / 部署态 /
         *     ingest streak / 丢弃计数——CLI `logs backend show` 与 Console 卡共面）。
         */
        get: operations["LogsService_GetLogsBackend"];
        /**
         * SetLogsBackend 切换日志后端（victorialogs | jsonl）：保存即生效——
         *     duty 收敛部署/移除（卷保留），采集路由下拍切换。
         */
        put: operations["LogsService_SetLogsBackend"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/metrics/mode": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * SetMetricsMode 切换 metrics 模式（unset | on）：保存即生效——duty 收敛
         *     部署/移除（数据卷保留），deploy scope（同 SetLogsBackend 分级理由）。
         */
        put: operations["MetricsService_SetMetricsMode"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/metrics/search": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * SearchMetrics 执行 PromQL 区间查询（read scope）：127.0.0.1:8428
         *     /api/v1/query_range 的规范化投影（label 集 + {t,v} 点集）。
         */
        get: operations["MetricsService_SearchMetrics"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/metrics/status": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetMetricsStatus 托管 metrics 栈状态视图（read scope）：模式 / 三件
         *     部署态 / 节点上报比 / retention——「N/M nodes reporting」的诚实口径。
         */
        get: operations["MetricsService_GetMetricsStatus"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/alerting/mode": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * SetAlertsMode 切换 alerts.mode（unset | on；前置门 metrics.mode=on，
         *     违反 → 409 E_ALERTS_METRICS_REQUIRED 带指引）。
         */
        put: operations["AlertingService_SetAlertsMode"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/alerting/rules": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** ListAlertRules 规则清单（name 字典序——渲染序的稳定性锚）。 */
        get: operations["AlertingService_ListAlertRules"];
        put?: never;
        /** CreateAlertRule 创建规则（平台级唯一名）。 */
        post: operations["AlertingService_CreateAlertRule"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/alerting/rules/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /** UpdateAlertRule 部分更新（optional 字段语义——未提供不变）。 */
        put: operations["AlertingService_UpdateAlertRule"];
        post?: never;
        /** DeleteAlertRule 删除规则（duty 下一拍重渲染规则文件）。 */
        delete: operations["AlertingService_DeleteAlertRule"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/alerting/rules:test": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * TestAlertRule 单次求值试跑（经 VM /api/v1/query 直接执行 expr 返回
         *     样本——不落库不部署，纯校验面）。
         */
        post: operations["AlertingService_TestAlertRule"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/alerting/status": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** GetAlertsStatus 告警栈状态视图（mode + vmalert 部署态 + 规则数）。 */
        get: operations["AlertingService_GetAlertsStatus"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/events/stream": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["EventsService_WatchEvents"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/acme": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetAcmeSettings ACME DNS-01 设置只读面（B 线 W5 设计 §3，D-V3W5-3/4；
         *     admin scope——DNS 服务商凭据指纹属平台敏感配置，与 S3 设置同门）。
         *     凭证只回 fingerprint（sha256 前 8），绝不回明文；同时下发服务端派生的
         *     通配期望域名集（wildcard=true 时非空）——CLI/Console 展示当前证书
         *     域名集形态，派生公式与签发面同源（服务端单点）。
         */
        get: operations["SystemService_GetAcmeSettings"];
        /**
         * UpdateAcmeSettings 保存 ACME DNS-01 设置（provider + 凭证 + wildcard
         *     开关）。api_token 为明文只写字段（TLS 传输面承载机密性，持久层
         *     envelope 加密）：**留空 = 保留已存凭证**（SMTP 密码同款先例——wildcard
         *     开关切换不要求重录凭证）；dns_provider=none 恒清空凭证。联动校验
         *     fail-fast：wildcard=true 须 provider ∈ {dnspod, cloudflare}（422
         *     E_ACME_WILDCARD_REQUIRES_PROVIDER——通配 SAN 只能 DNS-01 验证）且须
         *     base_domain（409 E_ACME_WILDCARD_REQUIRES_BASE_DOMAIN）。保存落审计
         *     acme.settings_changed（provider/wildcard/凭证有无——零明文；本设置族
         *     零事件，审计承载）。
         */
        put: operations["SystemService_UpdateAcmeSettings"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/acme/dns:test": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * TestDnsProvider DNS 服务商探针（诚实契约：在 _acme-challenge-test.
         *     <base_domain> 建 TXT → 删 TXT 两步真实往返——通过 = 能认证/能写/能删，
         *     zone 解析在 create 步隐含执行；不是"能列域名"级别的浅探测）。可带候选
         *     凭证（未保存也能测）；两字段全空 = 测已存凭证。探针失败以
         *     E_ACME_DNS_TEST_FAILED 报错，失败步与底层 provider 错误摘要进信封
         *     context（凭证材料零出现）。
         */
        post: operations["SystemService_TestDnsProvider"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/backups": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListBackups 状态备份台账（T2.22，只读）：热备快照的诚实账（kind/路径/
         *     sha256/verify_status）。verify_status=failed 的行是红色告警面的一部分
         *     ——台账如实保留失败行，消费方据此判断备份可用性。
         */
        get: operations["SystemService_ListBackups"];
        put?: never;
        /**
         * TriggerBackup 手动触发一次状态备份（T2.22；deploy scope——写面语义，
         *     与升级编排 pre_upgrade 快照共用同一同步入口；响应即落账后的台账行，
         *     verify_status=failed 时调用方必须视为备份失败而非请求失败歧义态）。
         */
        post: operations["SystemService_TriggerBackup"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/ingress": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["SystemService_GetIngressStatus"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/nodes": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["SystemService_ListNodes"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/nodes/join-guide": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetJoinGuide join 向导（E1-8，multi-node §2.3/D-MN-13；admin scope
         *     ——响应含 join token 材料）。base_domain 为空 → E_MULTI_NODE_REQUIRES_
         *     BASE_DOMAIN（409，D-MN-13：多节点未启用显式拒绝）。服务端生成 join
         *     命令、按 worker_ip 的精确防火墙放行规则（只生成不自动应用）、DNS
         *     步骤与完成判据；防火墙规则文本附「--harden-firewall 自动应用维持
         *     reserved」口径（平台不静默改用户防火墙）。
         */
        get: operations["SystemService_GetJoinGuide"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/nodes/join-token:rotate": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * RotateJoinToken 轮换 swarm join token（E1-8，D-MN-1：锚定完成后自动
         *     rotate 把泄露窗口收敛到分钟级；批量场景 manual 后手动执行）。role
         *     缺省 worker；rotate 后旧 token 立即失效。经底座 swarm 面执行，轮换
         *     记审计（node.join_token_rotated，§5.3——审计不设事件）。
         */
        post: operations["SystemService_RotateJoinToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/ping": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["SystemService_Ping"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/registry": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetRegistrySettings 平台 registry 凭证设置只读面（IMPL-T1-2/DT-2；
         *     admin scope——registry host/用户名/密码指纹属平台敏感配置，与 S3/ACME
         *     设置同门）。密码只回 fingerprint（sha256 前 8），绝不回明文；未配置
         *     时 host 为空。
         */
        get: operations["SystemService_GetRegistrySettings"];
        /**
         * UpdateRegistrySettings 保存平台 registry 凭证设置（host + 用户名 +
         *     密码）。语义：host 空 = 清除全部设置；password 留空 = 保留已存密码
         *     （ACME api_token 同款先例——改主机/用户名不强制重录）。保存落审计
         *     registry.updated + 事件 registry.updated（payload 带 host 与指纹，
         *     凭据材料零出现）。解析失败时部署路径回落本机 inspect（airgap 不回归）。
         */
        put: operations["SystemService_UpdateRegistrySettings"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/s3": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetS3Settings 对象存储设置只读面（E3 对象存储 §5.1/E3-2；admin scope
         *     ——端点/桶/凭证指纹属平台敏感配置）。secret 只回 fingerprint（sha256
         *     前 8），绝不回明文；s3.mode=unset 时其余字段为空。
         */
        get: operations["SystemService_GetS3Settings"];
        /**
         * UpdateS3Settings 全量保存对象存储设置（PUT 语义：请求即新状态，空字段
         *     即清空——避免「改 mode 残留旧凭证」的静默状态；secret_access_key 为
         *     明文字段，只写不读，传输面 TLS 承载机密性，持久层 envelope 加密）。
         *     互斥校验 fail-fast：mode=external 必填四项；mode=rustfs 四项必须为空；
         *     public_exposed=true 仅 rustfs 且需 base_domain（E_S3_PUBLIC_REQUIRES_
         *     BASE_DOMAIN）。保存落审计 + 事件 s3.updated（payload 带模式不带走秘密）。
         */
        put: operations["SystemService_UpdateS3Settings"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/s3:test": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * TestS3Connection S3 连接探针（E3 对象存储 §2.1 诚实契约：put→get→
         *     delete 一枚探针对象并逐字节比对——通过 = 能认证/能写/能读回，不是 TCP
         *     探活）。可带候选配置（未保存也能测）；全部候选字段为空 = 测已存配置。
         *     探针失败以 E_S3_TEST_FAILED 报错，失败步与底层错误摘要进信封 context。
         */
        post: operations["SystemService_TestS3Connection"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/system/status": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["SystemService_GetSystemStatus"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/placement": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get: operations["PlacementService_ShowPlacement"];
        /**
         * UpdatePlacement 显式换点（E1-7，multi-node §2.6；admin scope——破坏性
         *     确认路径）。目标节点存在且 ready（直读）；有卷应用 data_ack 必填
         *     （restored|discarded，缺省 → E_VOLUME_NODE_MISMATCH——前哨语义前置）；
         *     discarded 需 confirm（→ E_PLACEMENT_MOVE_REQUIRES_ACK）。落库同事务
         *     （绑定换绑 + 卷行 prev 登记 + placement.changed + 审计）；换点不自动
         *     部署，由用户发起部署收敛。
         */
        put: operations["PlacementService_UpdatePlacement"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/placement/migrate-plan": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetPlacementMigrationPlan restic 迁移 runbook（E1-7，multi-node §2.8/
         *     D-MN-10；read scope）：服务端生成步骤文档（真实卷名/节点名填充），
         *     restic 备份/恢复由用户在两节点执行——平台不编排远端数据移动。
         */
        get: operations["PlacementService_GetPlacementMigrationPlan"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/volumes": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListVolumes 跨 app 卷清单（E1-7，multi-node §2.8；read scope）：active
         *     在册行、orphaned 孤儿行（删除应用保留）与残留指引（prev_platform_
         *     node_id 非空的 active 行派生 residual 标记）。status 过滤缺省输出全部；
         *     不建生命周期 API、不做远端删除（D18）。
         */
        get: operations["PlacementService_ListVolumes"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/cron-runs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListCronRuns 读运行台账（scheduled_at 倒序；service 空 = 该 app 全部
         *     schedule 的行；保留窗每 schedule 最近 20 条，janitor 清理）。
         */
        get: operations["CronService_ListCronRuns"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/services/{service}/trigger": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * TriggerCronRun 手动触发一次（走与到点触发完全相同的链路：重叠 skip、
         *     绑定节点前哨、一次性 job 创建、cron_runs 行与事件；本路径追加审计
         *     cron.manual_triggered）。重叠/节点不可用不报错——响应携带 skipped 行
         *     与原因（与调度器处置一致）。
         */
        post: operations["CronService_TriggerCronRun"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListDatabases 库实例列表（name 字典序；deleted tombstone 不进默认列表
         *     ——与 apps 列表同口径）。
         */
        get: operations["DatabaseService_ListDatabases"];
        put?: never;
        /**
         * CreateDatabase 创建库实例（受理即 provisioning——无 created 态；凭据
         *     生成一次、age 密文落库；收敛器异步建现场过健康门 → ready）。
         */
        post: operations["DatabaseService_CreateDatabase"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetDatabase 库实例详情（连接信息脱敏投影——密码明文零离开存储；显式
         *     reveal 面随 S4/S6 轮换与 Console 票据）。
         */
        get: operations["DatabaseService_GetDatabase"];
        put?: never;
        post?: never;
        /**
         * DeleteDatabase 删除受理（引用守卫通过后 → deleting tombstone 第一拍；
         *     reap duty 幂等清理受管对象。confirm = 实例名——数据安全两段式确认；
         *     delete_volumes 默认 false = 卷保留转 orphaned，true = 删数据卷不可逆）。
         */
        delete: operations["DatabaseService_DeleteDatabase"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/backups": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListDatabaseBackups 备份台账列表（created_at 降序——恢复目标选择与
         *     Console 备份列表的数据源；kind/snapshot/size/verify_status/error 全量
         *     事实面）。
         */
        get: operations["DatabaseService_ListDatabaseBackups"];
        put?: never;
        /**
         * TriggerDatabaseBackup 手动备份受理（E4 S5，managed-databases §2.6）：
         *     异步受理（job 分钟级——响应即 accepted，结论经台账与 db.backup_* 事件
         *     披露；在途备份无台账行）。合法前置态 ready/degraded（§2.3 操作表）；
         *     per 实例操作互斥（备份/恢复/升级并发第二笔 → 409）。s3.mode=unset →
         *     E_S3_NOT_CONFIGURED（409——诚实拒绝，与注入前哨同码同语义）。
         */
        post: operations["DatabaseService_TriggerDatabaseBackup"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/credentials": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * RevealDatabaseCredentials 连接信息显式展开（admin scope；§2.5「Console
         *     库详情页展示连接信息，密码默认脱敏、显式展开」的 API 面——含密码明文
         *     与完整 URL；审计 db.reveal 承载敏感访问留痕，不产生事件）。
         */
        get: operations["DatabaseService_RevealDatabaseCredentials"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/restore": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * RestoreDatabaseBackup 原地恢复受理（破坏性两段式 confirm + 快照归属
         *     守卫：只重放本实例台账内的快照）。停库重放：实例 scale 0 → job 挂卷
         *     rw 重放 → 重部署（异步——结论经 db.restore_* 事件披露；恢复中断 =
         *     实例保持停止 + E_DB_RESTORE_FAILED 事件的 critical 口径，§2.6）。
         */
        post: operations["DatabaseService_RestoreDatabaseBackup"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/resume": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** ResumeDatabase 恢复（paused → provisioning 重收敛 → ready/degraded）。 */
        post: operations["DatabaseService_ResumeDatabase"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/retry": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * RetryDatabase 显式重试（failed → provisioning 重收敛；现场保留语义下
         *     失败的唯一出边）。
         */
        post: operations["DatabaseService_RetryDatabase"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/rotate": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * RotateDatabaseCredentials 凭据轮换（E4 managed-databases §2.5，S4）：
         *     破坏性两段式（confirm = 实例名）。合法前置态 ready/degraded/paused
         *     （§2.3 操作表；PG 暂停期拒绝——postgres 需运行中实例才能 ALTER USER，
         *     如实报 409 并提示先 resume）。引擎侧成功后：引用方 system 物化行回
         *     pending + 平台自动触发全部引用 app 重部署（各自走正常部署队列）。
         */
        post: operations["DatabaseService_RotateDatabaseCredentials"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/settings": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        /**
         * UpdateDatabaseSettings 设置变更（限额 + 备份计划；任意非终态准入、主
         *     状态不变；限额变更 = spec 重建由收敛器在下一拍承载）。镜像/引擎参数
         *     受管（违规模板字段不存在于请求——E_DB_TEMPLATE_UNSUPPORTED 面）。
         */
        put: operations["DatabaseService_UpdateDatabaseSettings"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/suspend": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /** SuspendDatabase 暂停（scale 0 保留服务与卷；引用方连不上是诚实暴露）。 */
        post: operations["DatabaseService_SuspendDatabase"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/databases/{name}/upgrade": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * UpgradeDatabase 受控升级受理（E4 S5，managed-databases §2.2）：①
         *     pre_upgrade 备份门（verify 通过才继续——失败实例不动）②digest 换新
         *     受控重建 ③健康门 ④失败 = digest 归位 + db.upgrade_failed + 状态落
         *     degraded。异步受理（备份门与健康门是分钟级）；paused = 仅换 spec 不
         *     重启（resume 时以新版本重建）。合法前置态 ready/degraded/paused。
         */
        post: operations["DatabaseService_UpgradeDatabase"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/secrets": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListSecrets 该 app 全部 secret（按 name 字典序；只投影名称/指纹/时间锚
         *     ——值与密文零出现）。
         */
        get: operations["SecretsService_ListSecrets"];
        put?: never;
        /**
         * SetSecret 写入（覆盖即轮换）：值经 age envelope 加密落 app_secrets；
         *     审计 secret.set（diff 只带名称与 hash8 指纹——值零出现）。
         */
        post: operations["SecretsService_SetSecret"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/secrets/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * RemoveSecret 删除单条（幂等不做：不存在 404；审计 secret.removed）。
         *     已被运行中服务引用的 removal 不追写部署——引用方下次部署 preflight
         *     E_SECRET_NOT_FOUND 诚实失败（移除声明再部署的既有语义）。
         */
        delete: operations["SecretsService_RemoveSecret"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/configs": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListConfigs 该 app 全部 config（按 name 字典序；只投影名称/指纹/时间锚
         *     ——值只在 GetConfig 的 admin 回读路径出现）。
         */
        get: operations["ConfigsService_ListConfigs"];
        put?: never;
        /**
         * SetConfig 写入（覆盖即换版）：值明文落 app_configs；审计 config.set
         *     （diff 只带名称与 hash8 指纹）。
         */
        post: operations["ConfigsService_SetConfig"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/apps/{app}/configs/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** GetConfig 明文回读（admin scope——与 GetEnv 同级信任面）。 */
        get: operations["ConfigsService_GetConfig"];
        put?: never;
        post?: never;
        /**
         * RemoveConfig 删除单条（幂等不做：不存在 404）。已被运行中服务引用的
         *     removal 不追写部署——引用方下次部署 preflight E_CONFIG_NOT_FOUND 诚实
         *     失败（移除声明再部署的既有语义，与 RemoveSecret 同口径）。
         */
        delete: operations["ConfigsService_RemoveConfig"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/notifications/deliveries": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * ListWebhookDeliveries 投递台账（read scope）：按端点/状态过滤，最新
         *     在前——重试路径与终败的诚实可见面。
         */
        get: operations["NotificationsService_ListWebhookDeliveries"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/notifications/endpoints": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** ListWebhookEndpoints 端点清单（read scope；无敏感投影）。 */
        get: operations["NotificationsService_ListWebhookEndpoints"];
        put?: never;
        /**
         * CreateWebhookEndpoint 创建端点（admin scope）：密钥平台生成，**明文
         *     仅本次响应可见**——丢失只能 rotate-secret 重置。
         */
        post: operations["NotificationsService_CreateWebhookEndpoint"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/notifications/endpoints/{id}": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** GetWebhookEndpoint 单端点视图（read scope）。 */
        get: operations["NotificationsService_GetWebhookEndpoint"];
        /**
         * UpdateWebhookEndpoint 部分更新（admin scope）：可选字段语义——未提供
         *     的字段不变（event_patterns 空 = 不变，非空 = 整体替换）。
         */
        put: operations["NotificationsService_UpdateWebhookEndpoint"];
        post?: never;
        /**
         * DeleteWebhookEndpoint 删除端点（admin scope）：投递台账行随之清理
         *     （同事务）；游标不动（订阅面变化不影响消费位）。
         */
        delete: operations["NotificationsService_DeleteWebhookEndpoint"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/notifications/endpoints/{id}/rotate-secret": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * RotateWebhookSecret 轮换签名密钥（admin scope）：新密钥**明文仅本次
         *     响应可见**；在途投递行按新密钥继续（接收方需同步换验签密钥）。
         */
        post: operations["NotificationsService_RotateWebhookSecret"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/notifications/endpoints/{id}/test": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * TestWebhook 发送 type=test 载荷（admin scope；设计 §5.2）：结构同真实
         *     事件、验签同链路——验证连通与验签配置。同步等待单次投递结果（10s
         *     预算）；不落台账（连通性检查不是投递事实）。
         * @description W4-S3 语义扩为按端点类型试发（TestEndpoint 语义——email 试发收件 =
         *     端点 target 地址；slack 载荷 = {"text"} 形态）。RPC 名保留 TestWebhook
         *     ——buf breaking FILE 门禁禁 RPC/消息删除（契约版本化纪律 §2.8），
         *     线格式重命名随下一契约版本评估；CLI `notifications test` 即本语义。
         */
        post: operations["NotificationsService_TestWebhook"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/notifications/smtp": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetSmtpSettings 平台级 SMTP 设置只读面（admin scope；设计 §8.3）：
         *     密码只回指纹（明文 sha256 前 8 hex），绝不回明文。全部 email 端点共
         *     用这一份设置（通道设置与端点解耦）。
         */
        get: operations["NotificationsService_GetSmtpSettings"];
        /**
         * UpdateSmtpSettings 全量保存平台级 SMTP 设置（PUT 语义：请求即新状态，
         *     空 password/username 回落空值；设计 §8.3）。密码明文入站（TLS 传输面）
         *     → envelope 加密落库；审计 notify.smtp_changed（只落审计不落事件）。
         */
        put: operations["NotificationsService_UpdateSmtpSettings"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/notifications/smtp/test": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * TestSmtp SMTP 探针（admin scope；设计 §8.3）：对候选（未保存也能测）
         *     或已存配置发测试邮件到指定收件地址——真实 SMTP 往返。不落台账（连通
         *     性检查不是投递事实）。
         */
        post: operations["NotificationsService_TestSmtp"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/terminal/status": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * GetTerminalStatus 终端功能状态视图（terminal scope）：功能开关 / relay
         *     部署态 / 已连接节点数 / 活跃会话数——Console 面板的常驻状态行来源。
         */
        get: operations["ExecService_GetTerminalStatus"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/v1/terminal/tickets": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * CreateTerminalTicket 签发 Web 终端接入 ticket（terminal scope）：一次性
         *     60s，绑定调用 token 与 app/service 对。terminal 功能关闭（terminal.
         *     enabled=false）→ E_TERMINAL_DISABLED。
         */
        post: operations["ExecService_CreateTerminalTicket"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
}
export type webhooks = Record<string, never>;
export interface components {
    schemas: {
        v1AcceptInviteRequest: {
            /** 邀请链接携带的一次性明文 token（服务端只存 sha256）。 */
            token?: string;
        };
        v1AcceptInviteResponse: {
            team_id?: string;
            team_slug?: string;
            team_name?: string;
            /** 受邀团队角色（accept 时落定的成员角色）。 */
            role?: string;
        };
        /**
         * ErrorResponse 是 fleetly 对外错误信封的唯一契约定义（发布专项 §2.7、
         *     架构 D21）：gateway HTTPErrorHandler（阶段 3 落地）将 gRPC 错误统一渲染
         *     为本结构，snake_case JSON 输出；code 与 T0.2 错误码注册表（唯一真源，
         *     只增不复用）对齐。
         */
        v1ErrorResponse: {
            /** 稳定错误码字符串（如 "E_COMPOSE_INVALID"），注册表校验只增。 */
            code?: string;
            /** 人读错误信息（面向运维/集成方，不承诺文案稳定）。 */
            message?: string;
            /**
             * 失败所处管线阶段（如 resolve / build / deploy / serve；勿与部署子状态
             *     phase 混用——该字段 2026-09-20 命名审查由 phase 更名 stage）。
             */
            stage?: string;
            /** 关联的部署 ID（无关联时为空）。 */
            deployment_id?: string;
            /** 可执行的修复建议（面向用户展示）。 */
            suggestion?: string;
            /** 结构化附加上下文（machine-readable 键值对）。 */
            context?: {
                [key: string]: string;
            };
            /** 相关文档 URL（错误码文档锚点）。 */
            docs?: string;
        };
        v1GetRegistrationStateResponse: {
            /** 注册是否开放（无用户窗口恒 true；否则 = auth.registration，缺省 closed）。 */
            open?: boolean;
            /** 平台是否已有用户（首用户引导/演练面事实披露）。 */
            has_users?: boolean;
        };
        v1LoginRequest: {
            email?: string;
            password?: string;
        };
        v1LoginResponse: {
            user?: components["schemas"]["v1UserView"];
        };
        v1LogoutAllRequest: Record<string, never>;
        v1LogoutAllResponse: {
            /**
             * 本次删除的会话行数（含当前会话）。
             * Format: int64
             */
            sessions_revoked?: string;
        };
        v1LogoutRequest: Record<string, never>;
        v1LogoutResponse: Record<string, never>;
        v1MeResponse: {
            user?: components["schemas"]["v1UserView"];
            /** 所属团队与角色投影（viewer/developer/admin/owner 四档，设计 §3.2）。 */
            teams?: components["schemas"]["v1TeamMembership"][];
            /**
             * 队内覆写行投影（v0.3 W3-S2，rbac-teams §3.3 B 形：有行则覆写、双向
             *     生效；无行则团队角色生效——本投影只列**有覆写行**的项目）。Console
             *     消费在 S3。
             */
            project_overrides?: components["schemas"]["v1ProjectOverrideMembership"][];
        };
        /**
         * ProjectOverrideMembership 是「我的项目角色覆写行」只读投影（§3.3 队内
         *     覆写形：仅限团队成员；owner 恒不可覆写——本投影角色词表只有三档）。
         */
        v1ProjectOverrideMembership: {
            project_id?: string;
            team_id?: string;
            /** 项目 slug（队内唯一、不可变）。 */
            prj_slug?: string;
            /** 覆写角色（admin/developer/viewer）。 */
            role?: string;
        };
        v1RegisterRequest: {
            email?: string;
            /** 明文口令（8..128 字符；argon2id 落库，明文不入库/审计/日志）。 */
            password?: string;
            /** 人读显示名（空 = 缺省取 email 本地部分）。 */
            display_name?: string;
            /**
             * 邀请 token（可空，v0.3 W3-S4 设计 §3.1「未注册→注册即自动 accept」）：
             *     携带有效邀请 token 的注册豁免注册窗（窗口关闭也可注册），注册与邀请
             *     消费同事务——注册成功即以受邀角色入队。无效 token → E_INVITE_INVALID
             *     （一次性凭据不泄漏存在性细节）。缺省（空）时行为与无邀请注册逐字一致。
             */
            invite_token?: string;
        };
        v1RegisterResponse: {
            user?: components["schemas"]["v1UserView"];
        };
        /** TeamMembership 是「我所在团队」的只读投影。 */
        v1TeamMembership: {
            team_id?: string;
            team_slug?: string;
            team_name?: string;
            /** 团队角色（owner/admin/developer/viewer）。 */
            role?: string;
        };
        /**
         * UserView 是用户行的无敏感投影：口令哈希不存在于任何通道（state 层投影
         *     纪律）；disabled_at 为空 = 在册。
         */
        v1UserView: {
            id?: string;
            email?: string;
            display_name?: string;
            /** 平台管理员标志（用户标志非角色，设计 §3.2）。 */
            is_platform_admin?: boolean;
            /** Format: date-time */
            created_at?: string;
            /**
             * 已禁用时输出禁用时刻；在册时不输出。
             * Format: date-time
             */
            disabled_at?: string;
        };
        v1CreateTokenRequest: {
            /**
             * scope 集（read / deploy / terminal / tasks / build / admin；admin 蕴含
             *     deploy 蕴含 read ⊕ terminal ⊕ tasks ⊕ build——terminal 为 Web 终端独立
             *     scope、tasks 为程序化动态工作负载面独立 scope（DT-5）、build 为上传
             *     构建面独立 scope（DT-6），三者 read/deploy 均不蕴含（E7 W5-S6 /
             *     IMPL-T2-1 / IMPL-T2-2）；重复项服务端归一去重）。
             */
            scopes?: string[];
            /** 备注（人读；如 "CI 部署"）。字段名 note（name 列承载，兼容既有表结构）。 */
            note?: string;
            /**
             * 绑定项目（可选；设计 §2.3 project_id 收窄维度——W2-S4 角色门消费）。
             *     提供时须为在册项目（未知 → 400）。
             */
            project_id?: string;
            /**
             * 机具令牌旗标（设计 §2.3：平台级凭据 = 平台管理员显式创建）。true 时
             *     产出 user NULL 的机具令牌：调用方须为平台管理员用户或 admin scope
             *     机具令牌，否则 403。缺省 false = 用户自服务 PAT（机具令牌调用方无
             *     用户身份，恒产出机具令牌——与既有 CI 形态兼容）。
             */
            machine?: boolean;
        };
        v1CreateTokenResponse: {
            id?: string;
            /** 明文 token（flt_<随机 48 hex>），仅本次响应可见。 */
            token?: string;
            note?: string;
            scopes?: string[];
            /** Format: date-time */
            created_at?: string;
        };
        v1ListTokensResponse: {
            tokens?: components["schemas"]["v1TokenView"][];
        };
        v1RevokeTokenResponse: {
            id?: string;
            revoked_at?: string;
        };
        /**
         * TokenView 是 token 行的无敏感投影：备注/scope/哈希前缀——明文与完整
         *     哈希永不回读（state-model §2.9）。
         */
        v1TokenView: {
            id?: string;
            note?: string;
            scopes?: string[];
            /** token_hash 前 12 hex（识别用，非凭据）。 */
            hash_prefix?: string;
            /** Format: date-time */
            created_at?: string;
            /**
             * 最近使用；从未使用时不输出。
             * Format: date-time
             */
            last_used_at?: string;
            /**
             * 已吊销时不输出（列表默认只出在册 token）。
             * Format: date-time
             */
            revoked_at?: string;
            /**
             * 属主用户（W2 §2.3 用户化注记）：非空 = 用户 PAT 的属主 id；空 = 平台
             *     机具令牌（gateway EmitUnpopulated=true 下机具令牌显式输出空串）。
             */
            user_id?: string;
            /** 绑定项目（空 = 不绑定；EmitUnpopulated=true 下显式输出空串）。 */
            project_id?: string;
        };
        TeamsServiceCreateInviteBody: {
            email?: string;
            role?: string;
        };
        TeamsServiceRevokeInviteBody: Record<string, never>;
        TeamsServiceSetTeamMemberRoleBody: {
            user_id?: string;
            role?: string;
        };
        TeamsServiceUpdateTeamBody: {
            /** 新显示名（slug 不可变——请求无 slug 字段，改 slug 只能重建团队）。 */
            name?: string;
        };
        v1CreateInviteResponse: {
            invite?: components["schemas"]["v1InviteView"];
            /**
             * 明文一次性 token（sha256 入库）：仅本次响应可见，用于拼邀请链接
             *     console/auth/invite?token=…（无 SMTP 直出供复制，设计 §3.1）。
             */
            token?: string;
        };
        v1CreateTeamRequest: {
            slug?: string;
            name?: string;
        };
        v1CreateTeamResponse: {
            team?: components["schemas"]["v1TeamView"];
        };
        v1DeleteTeamResponse: Record<string, never>;
        v1GetTeamResponse: {
            team?: components["schemas"]["v1TeamView"];
        };
        v1InviteView: {
            id?: string;
            team_id?: string;
            /** 被邀邮箱（小写归一）。 */
            email?: string;
            role?: string;
            /**
             * 过期时刻（创建 + 7 天；一次性窗口）。
             * Format: date-time
             */
            expires_at?: string;
            /** Format: date-time */
            created_at?: string;
            /**
             * 已接受/已吊销时输出对应时刻；未消费时不输出。
             * Format: date-time
             */
            accepted_at?: string;
            /** Format: date-time */
            revoked_at?: string;
        };
        v1ListTeamInvitesResponse: {
            invites?: components["schemas"]["v1InviteView"][];
        };
        v1ListTeamMembersResponse: {
            members?: components["schemas"]["v1TeamMemberView"][];
        };
        v1ListTeamsResponse: {
            teams?: components["schemas"]["v1TeamView"][];
        };
        v1RemoveTeamMemberResponse: Record<string, never>;
        v1RevokeInviteResponse: Record<string, never>;
        v1SetTeamMemberRoleResponse: {
            member?: components["schemas"]["v1TeamMemberView"];
        };
        v1TeamMemberView: {
            team_id?: string;
            user_id?: string;
            /** 团队角色（owner/admin/developer/viewer 四档，设计 §3.2）。 */
            role?: string;
            /** Format: date-time */
            created_at?: string;
            /** 属主投影（成员列表的可用性面——email 与显示名；无敏感材料）。 */
            email?: string;
            display_name?: string;
        };
        /** TeamView 是团队行的无敏感投影。 */
        v1TeamView: {
            id?: string;
            /** 单词制标识（[a-z0-9]{2,32}、全局唯一、不可变；底座命名公式段）。 */
            slug?: string;
            /** 人读显示名。 */
            name?: string;
            created_by?: string;
            /** Format: date-time */
            created_at?: string;
        };
        v1UpdateTeamResponse: {
            team?: components["schemas"]["v1TeamView"];
        };
        /** MoveAppRequest 是资源改派请求（平台管理员专属）。 */
        ProjectsServiceMoveAppBody: {
            /** app 引用：裸名（解析域内唯一）、`team/prj/app` 限定形或平台 ID。 */
            app?: string;
        };
        /** MoveDatabaseRequest 是库实例改派请求（平台管理员专属）。 */
        ProjectsServiceMoveDatabaseBody: {
            /** 库实例引用：裸名（解析域内唯一）、`team/prj/<name>` 限定形或平台 ID。 */
            database?: string;
        };
        ProjectsServiceSetProjectMemberRoleBody: {
            user_id?: string;
            role?: string;
        };
        ProjectsServiceUpdateProjectBody: {
            name?: string;
            description?: string;
        };
        /** AppProjectNetworkMembership 是参与变更的结论投影。 */
        v1AppProjectNetworkMembership: {
            app_id?: string;
            app?: string;
            project_id?: string;
            /** 项目限定形展示（team/project）。 */
            project?: string;
            /** 项目网 overlay 名（`fleetly-project-<project_id>`）。 */
            network?: string;
            /** 变更后的参与状态（true = 已挂入）。 */
            attached?: boolean;
            /** 本次是否发生状态变更（false = 已是目标值，幂等重跑；后置编排仍执行）。 */
            changed?: boolean;
            /** 参与变更重部署的部署 ID（无成功部署史 = 空）。 */
            deployment_id?: string;
            /**
             * rolling | attached | detached（rolling = 重部署已入队，服务随发布
             *     滚动切换网络；attached/detached = 无部署史，仅状态落位）。
             */
            status?: string;
        };
        /**
         * AttachAppProjectNetworkResponse 是挂入应答（两条 RPC 共享同一
         *     membership 投影——buf lint 要求 response 名与 RPC 同名，投影复用）。
         */
        v1AttachAppProjectNetworkResponse: {
            membership?: components["schemas"]["v1AppProjectNetworkMembership"];
        };
        v1CreateProjectRequest: {
            team_id?: string;
            slug?: string;
            name?: string;
            description?: string;
        };
        v1CreateProjectResponse: {
            project?: components["schemas"]["v1ProjectView"];
        };
        v1DeleteProjectResponse: Record<string, never>;
        /** DetachAppProjectNetworkResponse 是摘除应答。 */
        v1DetachAppProjectNetworkResponse: {
            membership?: components["schemas"]["v1AppProjectNetworkMembership"];
        };
        v1GetProjectResponse: {
            project?: components["schemas"]["v1ProjectView"];
        };
        v1ListProjectMembersResponse: {
            members?: components["schemas"]["v1ProjectMemberView"][];
        };
        v1ListProjectsResponse: {
            projects?: components["schemas"]["v1ProjectView"][];
        };
        /**
         * MoveAppResponse 是改派应答：换名重部署的结论投影（swapped = 新命名服务
         *     已就位并摘除旧名服务；accepted = 无运行中服务，仅归属切换）。
         */
        v1MoveAppResponse: {
            app?: string;
            from_project_id?: string;
            to_project_id?: string;
            to_project?: string;
            /** 换名重部署的部署 ID（无部署史 = 空）。 */
            deployment_id?: string;
            /** moved|accepted（accepted = 无底座对象随迁的纯归属切换）。 */
            status?: string;
        };
        /** MoveDatabaseResponse 是库改派应答。 */
        v1MoveDatabaseResponse: {
            database?: string;
            from_project_id?: string;
            to_project_id?: string;
            to_project?: string;
            status?: string;
        };
        v1ProjectMemberView: {
            project_id?: string;
            user_id?: string;
            /** 覆写角色（admin/developer/viewer 三档；owner 不可覆写——设计 §3.3）。 */
            role?: string;
            /** Format: date-time */
            created_at?: string;
            /** 属主投影（成员列表的可用性面；无敏感材料）。 */
            email?: string;
            display_name?: string;
        };
        /** ProjectView 是项目行的无敏感投影。 */
        v1ProjectView: {
            id?: string;
            team_id?: string;
            /** 归属团队 slug（限定形 team/project 展示面，D-W0-9）。 */
            team_slug?: string;
            /** 单词制标识（[a-z0-9]{2,32}、team 内唯一、不可变；命名公式三段第二位）。 */
            slug?: string;
            name?: string;
            description?: string;
            /** Format: date-time */
            created_at?: string;
            /**
             * 项目网投影（IMPL-T15-1/OT-1）：network_name = 项目网 overlay 名
             *     （`fleetly-project-<id>`，平台建；无成员时对象可不存在）；network_members
             *     = 参与位在位的 active app 数（成员计数，Console 网络面展示）。
             */
            network_name?: string;
            /** Format: int32 */
            network_members?: number;
        };
        v1RemoveProjectMemberResponse: Record<string, never>;
        v1SetProjectMemberRoleResponse: {
            member?: components["schemas"]["v1ProjectMemberView"];
        };
        v1UpdateProjectResponse: {
            project?: components["schemas"]["v1ProjectView"];
        };
        /**
         * AuditView 是审计行的读面投影：AuditRecord 全字段（无敏感材料——secret
         *     值禁止进入审计，state-model §2.9 红线在写侧强制，读面零脱敏负担）。
         */
        v1AuditView: {
            id?: string;
            /** Format: date-time */
            at?: string;
            /** human / system / user:<id>（设计 §6 actor 增维）。 */
            actor?: string;
            action?: string;
            target?: string;
            /** ok | error。 */
            result?: string;
            /** 失败路径的注册表错误码（'' = 成功行）。 */
            error_code?: string;
            /** 关联请求 ID（'' = 无）。 */
            request_id?: string;
            /** 脱敏后的 diff 摘要（'' = 无）。 */
            diff_summary?: string;
        };
        v1ListAuditResponse: {
            audits?: components["schemas"]["v1AuditView"][];
            /**
             * 全量命中计数（过滤生效、分页生效前）——读面分页器的总数投影。
             * Format: int32
             */
            total?: number;
        };
        AppsServiceResumeAppBody: Record<string, never>;
        AppsServiceSetAppSourceBody: {
            /** 拉源 remote URL（file:// 与 https://、ssh:// 形态）。 */
            source_url?: string;
            /** app 配置分支（默认 main）：webhook 投递触发与拉取共用此分支。 */
            source_branch?: string;
            /** 认证形态：none | https_token | ssh_key。 */
            source_auth_kind?: string;
            /**
             * 认证材料（https_token = token 原文；ssh_key = PEM 私钥）。source_auth_kind =
             *     none 时必须为空；服务端 envelope 加密落库，明文不落、永不回读。
             *     protovalidate 形状约束在服务端用例层按 source_auth_kind 交叉校验（跨字段
             *     规则—— CEL 交叉字段此处不引入，保持 proto 面最小）。
             */
            source_auth_secret?: string;
        };
        AppsServiceSetAppWebhookSecretBody: {
            /**
             * webhook 签名密钥（HMAC-SHA256 原料，GitHub/Gitea 同形态）。≥16 字符
             *     ——弱密钥显式拒绝（验签是该端点的唯一认证）。
             */
            secret?: string;
        };
        AppsServiceSetScalingPolicyBody: {
            /**
             * 副本下限（≥1；必填）。
             * Format: int32
             */
            min_replicas?: number;
            /**
             * 副本上限（≤16 且 ≥ min；必填）。
             * Format: int32
             */
            max_replicas?: number;
            /**
             * CPU 目标水位（0 = 不设该维度；至少一维非 0）。
             * Format: int32
             */
            target_cpu_pct?: number;
            /**
             * 内存目标水位（0 = 不设该维度；至少一维非 0）。
             * Format: int32
             */
            target_mem_pct?: number;
            /**
             * 冷却窗秒数（0 = 缺省 180）。
             * Format: int32
             */
            cooldown_seconds?: number;
        };
        AppsServiceSuspendAppBody: Record<string, never>;
        v1AppView: {
            id?: string;
            name?: string;
            /** 生命周期状态位：active / deleting / deleted。 */
            lifecycle?: string;
            /**
             * 派生状态：running / degraded / blocked / down / suspended（读面即时
             *     推导；suspended = 用户挂起位的直投影，不是对底座的观察结论）。
             */
            derived_state?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
            /**
             * 归属 slug（v0.3 W2-S3 归属模型；Console 限定形展示与团队级收窄过滤
             *     的数据源——此前无归属投影，同名 app 在列表面不可分）。
             */
            team_slug?: string;
            project_slug?: string;
            /**
             * 归属项目平台 ID（IMPL-T15-1：项目详情链接与项目网操作的目标锚；
             *     管理面用 ID，免疫跨团队同名项目歧义）。
             */
            project_id?: string;
            /**
             * 项目网参与状态（IMPL-T15-1/OT-1 显式 opt-in）：attached=true 时
             *     project_network = 项目网 overlay 名（`fleetly-project-<project_id>`），
             *     成员服务在 app 私网之外双挂该网（项目网别名 `<app>-<service>`）；
             *     缺省 false = 不参加（既有 app 私网隔离现状）。
             */
            project_network_attached?: boolean;
            project_network?: string;
            /**
             * 挂起位（app Stop/Start）：true = 用户请求停止——受管长驻服务副本被
             *     引擎排水到 0（服务对象保留），部署入队/drift/autoscaler/cron 挂起期
             *     按位豁免。恢复 = resume（清位 + active revision 重部署）。
             */
            suspended?: boolean;
        };
        v1DeleteAppResponse: {
            name?: string;
            /** 删除推进后的生命周期位（active → deleting）。 */
            lifecycle?: string;
        };
        /**
         * DeploymentView 是部署状态机行的只读投影（词表与 state 层一致：
         *     queued/preparing/building/releasing/observing/succeeded/failed/cancelled）。
         */
        v1DeploymentView: {
            id?: string;
            app?: string;
            /** deploy | rollback。 */
            kind?: string;
            status?: string;
            /** 子状态（blocked_waiting 或空）。 */
            phase?: string;
            revision_id?: string;
            error_code?: string;
            verdict?: string;
            /** 同记录恢复记录（replay | blocked；空 = 无）。 */
            recovery?: string;
            /** 首发失败 scale=0 保留现场。 */
            substrate_halted?: boolean;
            /** Format: date-time */
            first_healthy_at?: string;
            /** Format: int64 */
            downtime_ms?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
            /**
             * git 触发来源（T2.19）：仅经 git webhook 入队的部署非空——sha 为 40 位
             *     commit、ref 为 refs/heads/<branch>；API/CLI 直传 compose 的部署为空
             *     （gateway EmitUnpopulated=true 语义下显式输出空串，空串即「非 git 来
             *     源」）。
             *     webhook 入口的 (app, sha) 幂等去重即以此字段为判据，读面回显供
             *     AI-Agent/运营核对「这次部署来自哪个 commit」。
             */
            source_git_sha?: string;
            source_git_ref?: string;
        };
        v1GetAppResponse: {
            id?: string;
            name?: string;
            lifecycle?: string;
            derived_state?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
            /** 归属 slug（AppView 同款；详情头与列表行的限定形展示同源）。 */
            team_slug?: string;
            project_slug?: string;
            /**
             * 归属项目平台 ID 与项目网参与投影（IMPL-T15-1；AppView 同款字段，
             *     详情页的项目网卡数据源）。
             */
            project_id?: string;
            project_network_attached?: boolean;
            project_network?: string;
            /** 挂起位投影（AppView.suspended 同款字段；详情头 Stop/Start 按钮的数据源）。 */
            suspended?: boolean;
            placement?: components["schemas"]["v1PlacementView"];
            /** 最近部署（created_at 倒序，至多 5 条；派生状态的正交细节）。 */
            recent_deployments?: components["schemas"]["v1DeploymentView"][];
        };
        v1GetScalingPolicyResponse: {
            name?: string;
            service?: string;
            /**
             * 副本下限（≥1）。
             * Format: int32
             */
            min_replicas?: number;
            /**
             * 副本上限（≤16——swarm 单服务上限口径）。
             * Format: int32
             */
            max_replicas?: number;
            /**
             * CPU 目标水位百分数（[20,90]；0 = 未设该维度目标）。
             * Format: int32
             */
            target_cpu_pct?: number;
            /**
             * 内存目标水位百分数（[20,90]；0 = 未设该维度目标）。
             * Format: int32
             */
            target_mem_pct?: number;
            /**
             * 动作冷却窗秒数（[60,3600]，缺省 180）。
             * Format: int32
             */
            cooldown_seconds?: number;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1ListAppsResponse: {
            apps?: components["schemas"]["v1AppView"][];
        };
        /**
         * PlacementView 是 placements 行投影（state ∈ bound/blocked/unresolved，
         *     派生语义见 state-model §2.10；blocked/unresolved 是 app 派生状态
         *     blocked 的来源）。
         */
        v1PlacementView: {
            platform_node_id?: string;
            /** 用户 label 书写原值（显示名或 n_<ULID>；空 = 未声明）。 */
            label_ref?: string;
            state?: string;
            /** 进入当前状态的派生原因（node_down / node_gone 等；可空）。 */
            reason?: string;
            /** 绑定来源（placement 源词表：显式/自动钉住等）。 */
            source?: string;
            /** Format: date-time */
            pinned_at?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1RemoveScalingPolicyResponse: {
            name?: string;
            service?: string;
            /** 恒 true（删除成功即无策略）。 */
            removed?: boolean;
        };
        v1ResumeAppResponse: {
            app?: components["schemas"]["v1AppView"];
            /**
             * 恢复重部署的部署行 ID（active revision 重放入队时非空；应用从无成功
             *     部署时为空——无物可恢复，清位照常生效）。
             */
            deployment_id?: string;
        };
        v1SetAppSourceResponse: {
            name?: string;
            source_url?: string;
            source_branch?: string;
            source_auth_kind?: string;
        };
        v1SetAppWebhookSecretResponse: {
            name?: string;
            /** 恒 true（设置成功即已配置）。 */
            configured?: boolean;
        };
        v1SetScalingPolicyResponse: {
            policy?: components["schemas"]["v1GetScalingPolicyResponse"];
        };
        v1ShowAppWebhookResponse: {
            name?: string;
            /** webhook 签名密钥已配置（secret 值永不回读）。 */
            secret_configured?: boolean;
            /** 拉源配置（未设置时 url/branch 为空串、auth_kind = none）。 */
            source_url?: string;
            /** app 配置分支（默认 main）：webhook 投递触发与拉取共用此分支。 */
            source_branch?: string;
            /** none | https_token | ssh_key。 */
            source_auth_kind?: string;
        };
        v1SuspendAppResponse: {
            app?: components["schemas"]["v1AppView"];
        };
        DeploymentsServiceCancelDeploymentBody: Record<string, never>;
        DeploymentsServiceDeployBody: {
            /**
             * compose 文件内容字节（JSON/YAML 原文；服务端落临时文件走受控子集
             *     校验——compose 违约不动底座、不入队）。
             * Format: byte
             */
            compose?: string;
            /**
             * 破坏性变更确认门控（架构 §2.4 变更计划/确认语义，MG-C3）：本次部署相对
             *     最新 revision 的变更集含破坏性操作（服务删除/卷解绑——判定单源在
             *     compose 包，与 plan artifact 的 requires_confirm_destructive 同口径）时，
             *     必须显式置位才放行入队；未置位返回 E_DEPLOY_CONFIRM_REQUIRED、不入队。
             *     首发（无历史 revision）恒非破坏性，置位与否均放行。
             */
            confirm_destructive?: boolean;
            /**
             * 目标项目（v0.3 W2-S3 归属管道，D-W0-9 解析规则）：裸名或 `team/project`
             *     限定形。裸名仅解析域内唯一时可用（用户 = 可见项目集；机具令牌/平台
             *     管理员 = 全库），多命中 400 E_PROJECT_AMBIGUOUS 列候选。缺省：用户 =
             *     个人队 default 项目；机具令牌无缺省（必须显式，否则 400 带指引）。
             *     首次部署写入 apps.project_id/team_id；行上归属已定时必须一致（409）。
             */
            project?: string;
        };
        DeploymentsServiceRollbackDeploymentBody: {
            /** 回滚目标版本快照 ID；空 = 最近一次成功部署的版本（回退一版）。 */
            target_revision_id?: string;
        };
        v1CancelDeploymentResponse: {
            id?: string;
            /**
             * 置位 cancel_requested 时的状态（取消是异步语义：引擎先归位再落
             *     cancelled；曾健康 409）。
             */
            status?: string;
        };
        /**
         * ComposeWarning 是 compose 校验期非阻断标注的跨面投影（deploy/build 共
         *     用；code = 注册表 W_ 码，无注册码提示以 kind 承载稳定标识）。
         */
        v1ComposeWarning: {
            kind?: string;
            code?: string;
            service?: string;
            message?: string;
        };
        v1DeployResponse: {
            deployment_id?: string;
            app?: string;
            /** 入队即返回，恒 "queued"。 */
            status?: string;
            /**
             * compose 校验期非阻断标注（服务端受控子集校验的警告随响应带出——
             *     T2.18：CLI 改经 RPC 入队后仍保留校验警告的人读呈现）。
             */
            warnings?: components["schemas"]["v1ComposeWarning"][];
        };
        v1GetDeploymentResponse: {
            deployment?: components["schemas"]["v1DeploymentView"];
        };
        v1ListDeploymentsResponse: {
            deployments?: components["schemas"]["v1DeploymentView"][];
        };
        v1RollbackDeploymentResponse: {
            deployment_id?: string;
            app?: string;
            /** 恒 "queued"（入队即返回）。 */
            status?: string;
        };
        /**
         * BuildFromUploadMetadata 是上传构建的请求头（传输面元数据）。
         * @description 信任级红线（IMPL-T2-2）：与 git 构建**同信任级**——本消息不含 target/
         *     build-args/secrets/platform 等任何额外构建参数（无特赦亦无歧视）；解包
         *     后的 Dockerfile 走与 git 构建逐字相同的 dockerfile.v0 前端。
         */
        v1BuildFromUploadMetadata: {
            /**
             * 镜像仓库组件名（平台 registry 路径 <host>/apps/<name>；服务端转小写，
             *     首末位须为字母数字，可含 . _ -，≤100——tag 以 <name>-<buildid> 派生）。
             */
            name?: string;
            /**
             * Dockerfile 入口（相对上下文根的仓内路径；缺省 Dockerfile）。服务端
             *     校验：clean 相对路径、不越出上下文根、解析后为常规文件。
             */
            dockerfile?: string;
        };
        /**
         * BuildFromUploadResponse 返回终态构建行（succeeded；失败经错误信封点名
         *     error_code 与 build id）。image_ref/image_digest 是 digest 钉定引用——
         *     registry 模式 `<host>/apps/<name>@sha256:<manifest digest>`；本地模式
         *     `fleetly-local/<name>:<tag>` + 本机不可变 ID。
         */
        v1BuildFromUploadResponse: {
            build?: components["schemas"]["v1BuildView"];
        };
        /**
         * BuildView 是构建行的只读投影（词表与 state 层一致：queued/building/
         *     succeeded/failed；driver ∈ railpack/dockerfile/passthrough）。
         */
        v1BuildView: {
            id?: string;
            app?: string;
            service?: string;
            driver?: string;
            status?: string;
            /** 成功终态必非空（image_ref ↔ image_digest，D9 digest 引用）。 */
            image_ref?: string;
            image_digest?: string;
            /** 产物归档路径（railpack plan JSON / 构建日志）。 */
            plan_path?: string;
            log_path?: string;
            /** 失败终态的注册表错误码。 */
            error_code?: string;
            /** Format: date-time */
            started_at?: string;
            /** Format: date-time */
            finished_at?: string;
        };
        v1GetBuildResponse: {
            build?: components["schemas"]["v1BuildView"];
        };
        v1ListBuildsResponse: {
            builds?: components["schemas"]["v1BuildView"][];
        };
        /** PassthroughService 是镜像模式服务的直通报告（无构建行）。 */
        v1PassthroughService: {
            service?: string;
            image?: string;
        };
        v1TriggerBuildRequest: {
            /**
             * compose 文件内容字节（JSON/YAML 原文；应用名取自 compose name——请求
             *     无 app 字段，单源无错位面（A1 注记；若后续加 app 字段须与 Deploy 同款
             *     一致性校验）；服务端解析受控子集——compose 违约不动底座、不入队）。
             * Format: byte
             */
            compose?: string;
            /** 只构建该 compose 服务；空 = 全部 build 模式服务。 */
            service?: string;
            /**
             * 构建上下文解析基准目录（context 相对路径的宿主基准；空 = 服务端临时
             *     目录）。v0.1 单机同宿主语义，见 service 注释（H14：admin scope，且
             *     context 解析后不得越出该基准目录）。
             */
            base_dir?: string;
            /**
             * 目标项目（v0.3 W2-S3 归属管道）：仅在该 app 尚不存在、构建自动建行时
             *     消费——解析与缺省语义同 DeployRequest.project（D-W0-9；机具令牌必须
             *     显式）。app 行已在时沿用行上归属，本字段忽略。
             */
            project?: string;
        };
        v1TriggerBuildResponse: {
            /** 应用名（compose name；应用不存在时自动创建——与 deploy 同语义）。 */
            app?: string;
            /** 入队的构建行（status 恒 "queued"）。 */
            builds?: components["schemas"]["v1BuildView"][];
            /** compose 校验期非阻断标注（与 Deploy 同口径透传）。 */
            warnings?: components["schemas"]["v1ComposeWarning"][];
            /** 直通服务（仅 image 无 build——不建行不构建，附镜像引用）。 */
            passthrough?: components["schemas"]["v1PassthroughService"][];
        };
        DriftServiceConvergeDriftBody: Record<string, never>;
        DriftServiceSetDriftConvergeBody: {
            enabled?: boolean;
        };
        v1ConvergeDriftResponse: {
            app?: string;
            /** 收敛基准部署（期望态快照来源；收敛入队为准入重放，状态经部署面跟踪）。 */
            deployment_id?: string;
            desired_hash?: string;
        };
        /**
         * FieldDiffView 是单字段差异（值均为投影形态：env 只到键名 + key:hash，
         *     值明文与长度都不出投影）。
         */
        v1FieldDiffView: {
            field?: string;
            expected?: string;
            actual?: string;
        };
        /**
         * ServiceDriftView 是单服务的漂移结果（missing = 期望服务不存在；extra =
         *     期望集之外的多余受管服务）。
         */
        v1ServiceDriftView: {
            service?: string;
            drifted?: boolean;
            missing?: boolean;
            extra?: boolean;
            diff?: components["schemas"]["v1FieldDiffView"][];
        };
        v1SetDriftConvergeResponse: {
            app?: string;
            enabled?: boolean;
        };
        v1ShowDriftResponse: {
            app?: string;
            /** 期望态来源部署（空 = 无成功部署记录，无从判定）。 */
            desired_deployment?: string;
            drifted?: boolean;
            services?: components["schemas"]["v1ServiceDriftView"][];
        };
        /**
         * ServiceRuntimeView 是单服务的运行实况投影：
         *       - mode: replicated | global（cron/init 一次性 job 服务是快照模板，
         *         不物化长驻服务——decodeSpecs 期望集恒为长驻集，任务台账在 cron-runs
         *         读面，本面不混入）；
         *       - declared_replicas：compose deploy.replicas（global 恒 0）；
         *       - actual_replicas：state=running 且 desired_state=running 的任务数；
         *       - missing：期望集有而 Swarm 无此服务（absent——与 drift missing 同判）。
         */
        v1ServiceRuntimeView: {
            name?: string;
            image?: string;
            mode?: string;
            /** Format: uint64 */
            declared_replicas?: string;
            /** Format: uint64 */
            actual_replicas?: string;
            /** 底座 UpdateStatus 投影（''/updating/paused/completed + 消息）。 */
            update_state?: string;
            update_message?: string;
            missing?: boolean;
            tasks?: components["schemas"]["v1ServiceTaskView"][];
            /**
             * compose 服务名（期望集内取 spec 的 fleetly.process label；期望集外
             *     的实况多余服务无对应键，留空）——Console 按它与快照服务清单对位
             *     （name 是 swarm 全名，跨端拼装公式不做第二份）。
             */
            service?: string;
        };
        /**
         * ServiceTaskView 是单任务实况投影（engine TaskState 的契约形态；State/
         *     DesiredState 逐字镜像底座任务状态词表 new/pending/running/failed/
         *     complete/shutdown/rejected/...——running 且 desired_state=running 的任务
         *     即「实际运行的容器」。命名带 Service 前缀：包内 tasks.proto 的 TaskView
         *     是程序化动态工作载荷面（DT-5）的既有消息，两者域不同不可混用）。
         */
        v1ServiceTaskView: {
            id?: string;
            /** Format: int32 */
            slot?: number;
            state?: string;
            desired_state?: string;
            error?: string;
            /**
             * 任务 spec 镜像引用（新旧版本判据；与服务的 image 比对可见滚动中的
             *     新旧并存）。
             */
            image?: string;
            /** Format: date-time */
            timestamp?: string;
        };
        v1ShowAppRuntimeResponse: {
            app?: string;
            /** 期望态来源部署（空 = 无成功部署记录——服务集仅实况集）。 */
            desired_deployment?: string;
            services?: components["schemas"]["v1ServiceRuntimeView"][];
        };
        v1GetRevisionSpecResponse: {
            revision_id?: string;
            /** Format: int64 */
            seq?: string;
            /**
             * 归一化 compose 快照（canonical JSON 文本；compose.Spec 同构——env 为
             *     key:sha256，值明文结构性不在快照中）。
             */
            compose?: string;
        };
        v1ListRevisionsResponse: {
            revisions?: components["schemas"]["v1RevisionView"][];
        };
        v1RevisionView: {
            id?: string;
            /** Format: int64 */
            seq?: string;
            desired_hash?: string;
            /** active = 可回滚选项；superseded = 被保留窗淘汰（存档）。 */
            status?: string;
            verified?: boolean;
            /** Format: date-time */
            created_at?: string;
        };
        EnvServiceSetEnvBody: {
            /** 明文入参；服务端 envelope 加密落库（值不进审计/事件/日志）。 */
            value?: string;
        };
        /** EnvVarView 是 env 行的无值投影（值恒脱敏——读值走 GetEnv 显式路径）。 */
        v1EnvVarView: {
            key?: string;
            /** platform | system。 */
            source?: string;
            /** pending | effective。 */
            status?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1GetEnvResponse: {
            app?: string;
            key?: string;
            /** 明文（admin scope 专用路径）。 */
            value?: string;
            /** pending | effective。 */
            status?: string;
        };
        v1ListEnvResponse: {
            env_vars?: components["schemas"]["v1EnvVarView"][];
        };
        v1RemoveEnvResponse: {
            app?: string;
            key?: string;
            /**
             * 平台层台账立即删行；运行实例的 env 快照随下次部署更新（与 pending
             *     同链路语义，取值恒 "pending"——移除不是即时生效面）。
             */
            status?: string;
        };
        v1SetEnvResponse: {
            app?: string;
            key?: string;
            /** 恒 "pending"（随下次部署生效）。 */
            status?: string;
        };
        DomainsServiceCreateAppDomainBody: {
            /**
             * host（入参原文；服务端归一化：trim/小写/IDN→punycode；通配主机本票
             *     拒绝——app 级 DNS-01 签发链沿 W5）。
             */
            domain?: string;
            /**
             * compose 服务名（存在性由部署期底座事实兜底：服务未运行 → 502 诚实
             *     暴露，不在此层猜服务清单）。
             */
            service?: string;
            port?: string;
            /** http | h2c（空 = http）。 */
            protocol?: string;
            /** http01 | wildcard（空 = http01）。 */
            cert_mode?: string;
        };
        DomainsServiceUpdateAppDomainBody: {
            /** 空 = 保持现值（全空 = 无操作更新，幂等返回当前行）。 */
            service?: string;
            port?: string;
            protocol?: string;
            cert_mode?: string;
        };
        DomainsServiceVerifyAppDomainsBody: Record<string, never>;
        v1CreateAppDomainResponse: {
            domain?: components["schemas"]["v1DomainView"];
        };
        v1DomainCheckView: {
            domain?: string;
            ips?: string[];
            resolved?: boolean;
            /** 80 端口探测结果（空 = 不可达；否则记录响应状态行）。 */
            http_80?: string;
            /** 443 端口 TLS 握手结果（空 = 不可达）。 */
            https_443?: string;
            /** 实收证书的诚实记录（不经信任判定）。 */
            cert_subject?: string;
            cert_dns_names?: string[];
            /** Format: date-time */
            cert_not_after?: string;
            /** 单域名探测失败原文（resolve 失败等）。 */
            error?: string;
        };
        v1DomainView: {
            service?: string;
            domain?: string;
            /** 路由目标端口（'' = 未同步）。 */
            port?: string;
            /** 证书 PEM 内容 sha256 hex；'' = 尚无证书。 */
            cert_sha256?: string;
            /**
             * 叶证书 NotAfter；未签发时不输出。
             * Format: date-time
             */
            cert_not_after?: string;
            /** Format: date-time */
            created_at?: string;
            /** 后端协议（http | h2c；h2c = Traefik 后端 scheme=h2c 直出）。 */
            protocol?: string;
            /** 证书模式（http01 | wildcard；wildcard 的 DNS-01 app 级签发链沿 W5）。 */
            cert_mode?: string;
        };
        v1ListAppDomainsResponse: {
            domains?: components["schemas"]["v1DomainView"][];
        };
        v1RemoveAppDomainResponse: {
            app?: string;
            domain?: string;
        };
        v1UpdateAppDomainResponse: {
            domain?: components["schemas"]["v1DomainView"];
        };
        v1VerifyAppDomainsResponse: {
            checks?: components["schemas"]["v1DomainCheckView"][];
        };
        v1FollowLogsResponse: {
            entry?: components["schemas"]["v1LogEntryView"];
        };
        v1GetLogsBackendResponse: {
            view?: components["schemas"]["v1LogsBackendView"];
        };
        v1ListHistoryLogsResponse: {
            entries?: components["schemas"]["v1LogEntryView"][];
        };
        /** LogEntryView 是单条日志投影。source ∈ container | build。 */
        v1LogEntryView: {
            app?: string;
            service?: string;
            /** Format: date-time */
            at?: string;
            stderr?: boolean;
            line?: string;
            source?: string;
        };
        /**
         * LogsBackendView 是日志后端视图（E6 设计 §2.2/§2.3 诚实口径：模式、
         *     是否显式设置、部署态、ingest streak、丢弃计数常驻可见）。
         */
        v1LogsBackendView: {
            /**
             * 生效模式：victorialogs | jsonl（缺省 victorialogs——V2-1 默认捆绑；
             *     未显式设置时 mode 已投影为缺省值，set 标志区分「缺省生效」）。
             */
            backend?: string;
            /** 该键是否被显式保存过（false = 缺省态生效）。 */
            backend_set?: boolean;
            /**
             * 部署态（backend=victorialogs 时）：deployed（服务在位）| pending
             *     （duty 收敛中）| removed（backend=jsonl 或服务已移除）；面未装配
             *     （测试形态）= unknown。
             */
            deployment?: string;
            /** 入湖 streak 是否降级中（VL 不可达——检索降级，直播不受影响）。 */
            ingest_degraded?: boolean;
            /**
             * 降级 streak 起点（未降级不输出）。
             * Format: date-time
             */
            ingest_degraded_since?: string;
            /**
             * 进程启动以来溢出丢弃的累计行数。
             * Format: uint64
             */
            dropped_total?: string;
        };
        /**
         * SearchLogRow 是检索命中的单行（字段与入湖行对齐：_time/_msg/app/
         *     service/source/stderr——E6 设计 §3.1 行集契约；DT-5 增 task）。
         */
        v1SearchLogRow: {
            /** Format: date-time */
            at?: string;
            app?: string;
            service?: string;
            source?: string;
            stderr?: boolean;
            msg?: string;
            /**
             * 访问行（source=access）的结构化字段透传（W5-S2 设计 §3.2：method/
             *     status/host/path/route/duration_ms/client_ip/deployment_id——入湖
             *     白名单词表内回读；deployment_id 为滚动窗内**近似**归因，多副本滚动
             *     窗内外流量可能分属新旧两代部署）。container/build 行为空。
             */
            fields?: {
                [key: string]: string;
            };
            /** 任务归因（DT-5：任务行 = 任务平台 ID；其余行空）。 */
            task?: string;
        };
        v1SearchLogsResponse: {
            rows?: components["schemas"]["v1SearchLogRow"][];
            /** 下一页游标（空 = 没有更多命中）。 */
            next_cursor?: string;
        };
        v1SetLogsBackendRequest: {
            backend?: string;
        };
        v1SetLogsBackendResponse: {
            view?: components["schemas"]["v1LogsBackendView"];
        };
        /**
         * GetMetricsStatusResponse 是状态视图（设计 §4.1/§4.2 诚实口径）：mode /
         *     三件部署态 / 节点上报比（以实际抓到的 cAdvisor 目标数计——跨节点采集
         *     依赖 overlay 数据面，worker 节点缺席时不谎报 M/M）/ retention。
         */
        v1GetMetricsStatusResponse: {
            /**
             * 生效模式：unset | on（缺省 unset；未显式设置时 mode 已投影为缺省值，
             *     set 标志区分「缺省生效」）。
             */
            mode?: string;
            /** 该键是否被显式保存过（false = 缺省态生效）。 */
            mode_set?: boolean;
            /** 三件部署态（cAdvisor / node_exporter / VictoriaMetrics 固定序）。 */
            components?: components["schemas"]["v1MetricsComponentView"][];
            /**
             * 正在向 VM 上报的节点数（up{job="fleetly-cadvisor"} 计数；VM 不可达
             *     = 0——配合 components 部署态解读，不单独谎报）。
             * Format: int32
             */
            nodes_reporting?: number;
            /**
             * 集群节点总数（观测缓存投影）。
             * Format: int32
             */
            nodes_total?: number;
            /**
             * VM -retentionPeriod 对齐天数（config 键 metrics.retention_days）。
             * Format: int32
             */
            retention_days?: number;
        };
        /** MetricsComponentView 是单件托管服务的部署态投影。 */
        v1MetricsComponentView: {
            name?: string;
            /** 服务是否在位（mode=on 且 false = duty 收敛中——过渡态红面）。 */
            exists?: boolean;
            /** 实况镜像引用（不在位为空）。 */
            image?: string;
        };
        /** MetricsPoint 是单个采样点（t = unix 秒；v = 采样值）。 */
        v1MetricsPoint: {
            /** Format: int64 */
            t?: string;
            /** Format: double */
            v?: number;
        };
        /**
         * MetricsSeries 是一条时序（label 集原样透传——含 __name__ 原始指标名；
         *     函数产出的序列无该 label，透传不补造）。
         */
        v1MetricsSeries: {
            metric?: {
                [key: string]: string;
            };
            points?: components["schemas"]["v1MetricsPoint"][];
        };
        v1SearchMetricsResponse: {
            series?: components["schemas"]["v1MetricsSeries"][];
        };
        v1SetMetricsModeRequest: {
            mode?: string;
        };
        v1SetMetricsModeResponse: {
            status?: components["schemas"]["v1GetMetricsStatusResponse"];
        };
        AlertingServiceUpdateAlertRuleBody: {
            name?: string;
            expr?: string;
            /** Format: int64 */
            for_duration_seconds?: string;
            /** 非 null = 整体替换（null = 不变）。 */
            labels?: {
                [key: string]: string;
            };
            /**
             * 非空 = 整体替换（全量替换语义用空集表达需经 Delete+Create——与
             *     notifications patterns 更新同口径）。
             */
            channels?: string[];
        };
        /** AlertRuleView 是一条规则的读面投影。 */
        v1AlertRuleView: {
            id?: string;
            name?: string;
            expr?: string;
            /**
             * for 子句秒数（0 = 无 for 子句）。
             * Format: int64
             */
            for_duration_seconds?: string;
            /** 规则 label 集（含 severity——severity 进通知文案的源头）。 */
            labels?: {
                [key: string]: string;
            };
            /** 通知端点 id 集（空 = 缺省投全部启用端点）。 */
            channels?: string[];
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1CreateAlertRuleRequest: {
            name?: string;
            expr?: string;
            /** Format: int64 */
            for_duration_seconds?: string;
            labels?: {
                [key: string]: string;
            };
            channels?: string[];
        };
        v1CreateAlertRuleResponse: {
            rule?: components["schemas"]["v1AlertRuleView"];
        };
        v1DeleteAlertRuleResponse: Record<string, never>;
        v1GetAlertsStatusResponse: {
            /** 生效模式：unset | on（缺省 unset；set 区分「缺省生效」）。 */
            mode?: string;
            mode_set?: boolean;
            /** vmalert 服务部署态（mode=on 且 exists=false = duty 收敛中——过渡态）。 */
            vmalert_exists?: boolean;
            vmalert_image?: string;
            /**
             * 平台规则数。
             * Format: int32
             */
            rule_count?: number;
            /**
             * metrics.mode 当前值（前置门的可见面——alerts on 而 metrics off =
             *     vmalert 被移除的休眠形态，如实并列让消费方自判）。
             */
            metrics_mode?: string;
        };
        v1ListAlertRulesResponse: {
            rules?: components["schemas"]["v1AlertRuleView"][];
        };
        v1SetAlertsModeRequest: {
            mode?: string;
        };
        v1SetAlertsModeResponse: {
            status?: components["schemas"]["v1GetAlertsStatusResponse"];
        };
        v1TestAlertRuleRequest: {
            expr?: string;
        };
        /**
         * TestAlertRuleResponse 是 instant query 的样本回（序列 label 集 + 单点；
         *     空集 = 表达式合法但当前无匹配序列——同样是「试跑成功」的事实）。
         */
        v1TestAlertRuleResponse: {
            series?: components["schemas"]["v1MetricsSeries"][];
        };
        v1UpdateAlertRuleResponse: {
            rule?: components["schemas"]["v1AlertRuleView"];
        };
        /**
         * CursorExpiredView 是游标过期断档帧：seq ≤ (oldest_seq - 1) 的事件已被
         *     保留策略清理，消费方应以 oldest_seq 重新拉全量。
         */
        v1CursorExpiredView: {
            /** Format: int64 */
            oldest_seq?: string;
            message?: string;
        };
        /**
         * EventView 是事件行投影（payload 为脱敏 JSON 文本；secret 值禁止进入
         *     事件——state-model §2.9，采集端已保证）。
         */
        v1EventView: {
            /** Format: int64 */
            seq?: string;
            /** Format: date-time */
            at?: string;
            /** 注册表内事件名（deployment.succeeded 等）。 */
            name?: string;
            /** 主题（deployment:<id> / app:<name> 等）。 */
            subject?: string;
            payload?: string;
        };
        v1WatchEventsResponse: {
            event?: components["schemas"]["v1EventView"];
            cursor_expired?: components["schemas"]["v1CursorExpiredView"];
        };
        /**
         * AcmeSettingsView 是 acme.* 设置的只读投影。凭证只回 fingerprint（明文
         *     sha256 前 8 hex；空 = 未设置）——读面永无明文（写面 UpdateAcmeSettings
         *     承载明文，TLS 传输面 + envelope 持久层）。
         */
        v1AcmeSettingsView: {
            /** DNS-01 服务商词表：none（缺省，未配置）| dnspod | cloudflare。 */
            dns_provider?: string;
            /** 凭证指纹（sha256 前 8 hex），非凭证本体；空 = 未设置。 */
            credentials_fingerprint?: string;
            /**
             * 通配证书 opt-in 开关（true 时平台证书签
             *     [*.base, console/ctrl/registry.<base>]，DNS-01 验证）。
             */
            wildcard?: boolean;
            /**
             * 最近一次保存时刻（从未保存 → 不输出）。
             * Format: date-time
             */
            updated_at?: string;
            /**
             * 通配期望域名集的服务端派生实值（wildcard=true 且 base_domain 非空时
             *     非空；派生公式与签发面同源）。CLI/Console 的「当前证书域集」展示源。
             */
            wildcard_domains?: string[];
            /** 平台域名（空 = 单节点形态——通配/平台证书面均不可用）。 */
            base_domain?: string;
        };
        /**
         * BackupHealth 是系统状态里备份面的明细视图（组件布尔健康的展开：最近
         *     一次备份的时间与校验结论——「绿色成功但实际没备份」的对立面是让
         *     verify_status 与时间直接可见）。
         */
        v1BackupHealth: {
            /** 最近一次备份的台账 ID（= 备份目录名）。 */
            last_backup_id?: string;
            /** 最近一次备份的触发类别（daily/pre_upgrade/post_deploy/manual）。 */
            last_kind?: string;
            /**
             * 最近一次备份的台账落账时刻。
             * Format: date-time
             */
            last_backup_at?: string;
            /** 回读校验结论（verified/failed）。 */
            last_verify_status?: string;
            /** 失败原因原文（verified 行为空）。 */
            last_error?: string;
        };
        /**
         * BackupView 是状态备份台账行的只读投影（state_backups 表）。path 指向
         *     备份目录内的快照文件；manifest.json 与其同目录（含 sha256/密钥指纹/
         *     schema 版本——恢复核对材料，密钥本体绝不入备份目录）。
         */
        v1BackupView: {
            id?: string;
            /**
             * 触发类别：daily / pre_upgrade / post_deploy / manual（历史行可为
             *     hot/cold）。
             */
            kind?: string;
            /** 快照文件路径。 */
            path?: string;
            /** 快照文件 sha256（hex；回读校验对象）。 */
            sha256?: string;
            /** Format: int64 */
            size_bytes?: string;
            /** 回读校验结论：verified / failed（失败行保留——红色告警面的一部分）。 */
            verify_status?: string;
            /** 校验失败原因原文（verified 行为空）。 */
            error?: string;
            /**
             * 台账落账时刻。
             * Format: date-time
             */
            created_at?: string;
            /**
             * 远端上传结论（E3-3 上传轨）：none（未上传——s3.mode=unset 合法态或
             *     上传步未执行）/ ok / failed。本地 verify 语义不变（上传失败不回写
             *     verify_status）。
             */
            upload_status?: string;
            /**
             * 最近一次上传尝试的完成时刻（ok/failed 都记；从未尝试不输出）。
             * Format: date-time
             */
            uploaded_at?: string;
            /**
             * 上传失败原因摘要（截断上界在存储层；不含 secret——restic env 凭证
             *     材料禁止进台账/事件/读面）。
             */
            upload_error?: string;
        };
        /**
         * CertLedgerView 是证书台账行投影（domains 表 cert 列对照；app 为显示名，
         *     已删除应用回退显示 app id）。
         */
        v1CertLedgerView: {
            app?: string;
            domain?: string;
            cert_sha256?: string;
            /** Format: date-time */
            cert_not_after?: string;
        };
        /**
         * ComponentHealth 是单组件健康如实上报（name = lynx 服务名，如
         *     state.store / state.observer / ingress.traefik）。ok=false 时 error 为
         *     检查器返回原文。
         */
        v1ComponentHealth: {
            name?: string;
            ok?: boolean;
            error?: string;
        };
        /** DnsProbeStep 是探针单步结果（create/delete；诚实契约：失败步可定位）。 */
        v1DnsProbeStep: {
            /** 步骤名：create | delete。 */
            step?: string;
            ok?: boolean;
            /**
             * 该步耗时（毫秒）。
             * Format: int64
             */
            duration_ms?: string;
            /** 失败时的底层 provider 错误摘要（不含凭证材料）。 */
            error?: string;
        };
        /**
         * DnsProviderTestResult 是探针结构化结果：provider 回显、探针 TXT 名、
         *     各步耗时、失败步。ok=false 时 failed_step 指向首个失败步（create 失败
         *     短路——无记录可删，delete 不执行）。
         */
        v1DnsProviderTestResult: {
            ok?: boolean;
            dns_provider?: string;
            /** 探针 TXT 记录名（_acme-challenge-test.<base_domain>）。 */
            record_name?: string;
            steps?: components["schemas"]["v1DnsProbeStep"][];
            failed_step?: string;
        };
        /**
         * FirewallRule 是一条防火墙放行规则文本（方向 + 端口/协议 + 用途 + 可
         *     复制命令；只生成不自动应用——平台不静默改用户防火墙，multi-node §2.3）。
         */
        v1FirewallRule: {
            /** 方向词表：worker_to_manager / bidirectional / public_to_all。 */
            direction?: string;
            /** 端口/协议（如 2377/tcp、7946/tcp+udp、4789/udp、8423/tcp、80,443/tcp）。 */
            port?: string;
            /** 用途（集群管理 / gossip / overlay VXLAN / Traefik 配置端点 TLS / 应用入口）。 */
            purpose?: string;
            /** 规则文本（manager 侧或 worker 侧可复制的 iptables 命令/说明）。 */
            rule?: string;
            /** 规则应用在哪一侧（manager / worker / both）。 */
            side?: string;
        };
        v1GetAcmeSettingsResponse: {
            settings?: components["schemas"]["v1AcmeSettingsView"];
        };
        v1GetIngressStatusResponse: {
            traefik?: components["schemas"]["v1TraefikView"];
            /** 控制面配置端点监听地址（ingress.config_addr 配置原值）。 */
            config_addr?: string;
            /** 下发给 Traefik 的控制面可达 IP（空 = 自动探测）。 */
            advertise_ip?: string;
            /** ACME challenge 应答器基址（空 = 未接入集中签发）。 */
            responder?: string;
            /**
             * 配置端点两面探测结果（服务端环回执行）：/healthz 无鉴权状态行；
             *     /configs 带 token 鉴权核验（401 = token 缺失/错误，如实报告；200 =
             *     鉴权通过且返回合法 JSON）。不可达 = "unreachable"。
             */
            healthz?: string;
            auth?: string;
            /** 证书台账（有证书的域名行）。 */
            certificates?: components["schemas"]["v1CertLedgerView"][];
            /**
             * 证书存储目录（控制面侧）与其中的 app 清单（meta 索引；目录缺失 =
             *     空清单非错误，cert_dir_error 承载读取故障原文）。
             */
            cert_dir?: string;
            cert_dir_apps?: string[];
            cert_dir_error?: string;
        };
        v1GetJoinGuideResponse: {
            guide?: components["schemas"]["v1JoinGuideView"];
        };
        v1GetRegistrySettingsResponse: {
            settings?: components["schemas"]["v1RegistrySettingsView"];
        };
        v1GetS3SettingsResponse: {
            settings?: components["schemas"]["v1S3SettingsView"];
        };
        v1GetSystemStatusResponse: {
            service?: string;
            version?: string;
            components?: components["schemas"]["v1ComponentHealth"][];
            backup?: components["schemas"]["v1BackupHealth"];
        };
        /** JoinGuideView 是 join 向导输出（服务端生成，multi-node §2.3）。 */
        v1JoinGuideView: {
            /** 完整 join 命令（docker swarm join --token SWMTKN-… <manager-addr>:2377）。 */
            join_command?: string;
            /** manager 可达地址（swarm advertise addr 或请求覆盖值）。 */
            manager_addr?: string;
            /**
             * worker join token（admin scope 的 token 材料；向导后按 join.token_
             *     rotate=auto 自动轮换的口径见 D-MN-1）。
             */
            worker_token?: string;
            /** 平台域名（DNS 步骤的主体）。 */
            base_domain?: string;
            /** manager 侧放行规则（按 worker_ip 生成）。 */
            manager_firewall_rules?: components["schemas"]["v1FirewallRule"][];
            /** worker 侧放行规则。 */
            worker_firewall_rules?: components["schemas"]["v1FirewallRule"][];
            /**
             * worker 前置门禁命令（docker version ≥29.8.1 + iptables legacy 判定
             *     ——与 install.sh 同判据的命令形态；复制到 worker 执行）。
             */
            worker_preflight_commands?: string[];
            /**
             * DNS 步骤（既有应用/平台子域 A 记录追加 worker IP；ctrl.<base> 保持
             *     仅 manager；fleetly domains verify 复核）。
             */
            dns_steps?: string[];
            /**
             * 完成判据（向导自动推进面：节点观测拍出现 → 锚定 node.joined →
             *     ready + Traefik 任务 running）。
             */
            completion_checks?: string[];
        };
        v1ListBackupsResponse: {
            backups?: components["schemas"]["v1BackupView"][];
        };
        v1ListNodesResponse: {
            nodes?: components["schemas"]["v1NodeView"][];
        };
        /**
         * NodeView 是节点观测缓存行的只读投影（state-model §2.2：缓存禁止用于
         *     决策，展示/诊断专用；节点变更用 docker node 原生命令）。观测数据带
         *     observed_at/stale——状态诚实契约（architecture §4.2 横切硬指标）；
         *     nodes 不提供「最后心跳」字段。
         */
        v1NodeView: {
            swarm_node_id?: string;
            /** 平台节点 ID（fleetly.placement.node-id label；未锚定时为空）。 */
            platform_id?: string;
            hostname?: string;
            state?: string;
            availability?: string;
            is_manager?: boolean;
            /** Format: date-time */
            observed_at?: string;
            stale?: boolean;
            labels?: {
                [key: string]: string;
            };
            /**
             * 绑定其上的应用 ID 清单（E1-8，multi-node §2.7/D-MN-9：读时 join
             *     placements 权威表，无迁移——UI「已钉应用」交叉引用面）。
             */
            pinned_app_ids?: string[];
        };
        v1PingResponse: {
            /** 应答服务名（恒 "fleetlyd"）。 */
            service?: string;
            /** 服务版本（构建 -ldflags 注入，未注入时为 "dev"）。 */
            version?: string;
        };
        /**
         * RegistrySettingsView 是 registry.* 设置的只读投影。密码只回 fingerprint
         *     （明文 sha256 前 8 hex；空 = 未设置）——读面永无明文（写面
         *     UpdateRegistrySettings 承载明文，TLS 传输面 + envelope 持久层）。
         */
        v1RegistrySettingsView: {
            /**
             * 外部 registry host（归一形态：小写、无 scheme；docker.io 家族归一为
             *     registry-1.docker.io）。空 = 未配置（解析腿恒匿名）。
             */
            host?: string;
            username?: string;
            /** 密码指纹（sha256 前 8 hex），非密码本体；空 = 未设置密码。 */
            password_fingerprint?: string;
            /**
             * 最近一次保存时刻（从未保存 → 不输出）。
             * Format: date-time
             */
            updated_at?: string;
        };
        v1RotateJoinTokenRequest: {
            /** 轮换目标 token 的角色：worker（缺省）| manager。 */
            role?: string;
        };
        v1RotateJoinTokenResponse: {
            /** 轮换后的角色与新 token（旧 token 立即失效；admin scope 材料）。 */
            role?: string;
            token?: string;
        };
        /**
         * S3ConnectionTestResult 是探针结构化结果：endpoint 回显脱敏（secret 不
         *     回显）、各步耗时、失败步。ok=false 时 failed_step 指向首个失败步。
         */
        v1S3ConnectionTestResult: {
            ok?: boolean;
            endpoint_url?: string;
            region?: string;
            bucket?: string;
            path_style?: boolean;
            steps?: components["schemas"]["v1S3ProbeStep"][];
            failed_step?: string;
        };
        /** S3ProbeStep 是探针单步结果（put/get/delete；诚实契约：失败步可定位）。 */
        v1S3ProbeStep: {
            /** 步骤名：put | get | delete。 */
            step?: string;
            ok?: boolean;
            /**
             * 该步耗时（毫秒）。
             * Format: int64
             */
            duration_ms?: string;
            /** 失败时的底层错误摘要（不含 secret 材料）。 */
            error?: string;
        };
        /**
         * S3SettingsView 是 s3.* 设置的只读投影。secret 只回 fingerprint（明文
         *     sha256 前 8 hex；空 = 未设置）——读面永无明文（写面 UpdateS3Settings
         *     承载 secret 明文，TLS 传输面 + envelope 持久层）。
         */
        v1S3SettingsView: {
            /**
             * 模式词表：unset（缺省，未配置）| external（外部 S3 端点）| rustfs
             *     （托管 RustFS，opt-in）。
             */
            mode?: string;
            /**
             * S3 端点 URL（含 scheme，如 https://s3.amazonaws.com；rustfs 模式下
             *     服务端派生 http://rustfs:9000）。
             */
            endpoint_url?: string;
            region?: string;
            bucket?: string;
            access_key_id?: string;
            /** secret 指纹（sha256 前 8 hex），非 secret 本体。 */
            secret_fingerprint?: string;
            /** path-style 寻址（RustFS/MinIO 类自建端点 true，AWS 虚拟主机式 false）。 */
            path_style?: boolean;
            /** 公网子域开关（仅 rustfs 模式可开；开启后 s3.<base> 公网可达）。 */
            public_exposed?: boolean;
            /**
             * 最近一次保存时刻（从未保存 → 不输出）。
             * Format: date-time
             */
            updated_at?: string;
            /**
             * 公网访问域名的服务端派生实值（v0.2.x 收尾票：CLI 公网行字面 s3.<base>
             *     的收口——读面此前不含 base_domain，CLI 只能显示字面形态）。派生公式
             *     = "s3." + base_domain；仅 public_exposed=true 且 base_domain 非空时填
             *     充，其余形态为空串（读面永不含凭据材料）。
             */
            public_domain?: string;
        };
        v1TestDnsProviderRequest: {
            /**
             * 候选配置（未保存也能测）：任一字段非空即视为候选；两字段全空 = 测
             *     已存凭证（provider 未配置时拒绝）。
             */
            dns_provider?: string;
            api_token?: string;
        };
        v1TestDnsProviderResponse: {
            result?: components["schemas"]["v1DnsProviderTestResult"];
        };
        v1TestS3ConnectionRequest: {
            /**
             * 候选配置（未保存也能测）：任一字段非零即视为候选配置；全空 = 测已存
             *     配置（s3.mode=unset 时已存配置不存在，拒绝）。
             */
            endpoint_url?: string;
            region?: string;
            bucket?: string;
            access_key_id?: string;
            secret_access_key?: string;
            path_style?: boolean;
        };
        v1TestS3ConnectionResponse: {
            result?: components["schemas"]["v1S3ConnectionTestResult"];
        };
        /**
         * TraefikView 是入口服务实况投影（Swarm service inspect；不可达时 exists
         *     = false 且 error 为探测原文）。
         */
        v1TraefikView: {
            exists?: boolean;
            image?: string;
            /** Format: int32 */
            static_args?: number;
            error?: string;
        };
        v1TriggerBackupRequest: {
            /**
             * 触发类别（缺省 manual；升级编排传 pre_upgrade）。manual/daily/
             *     pre_upgrade/post_deploy 之外取值被请求校验拒绝。
             */
            kind?: string;
        };
        v1TriggerBackupResponse: {
            backup?: components["schemas"]["v1BackupView"];
        };
        v1UpdateAcmeSettingsRequest: {
            /** 服务商词表（空 = none）。联动校验见 rpc 注记。 */
            dns_provider?: string;
            /**
             * 凭证明文（只写字段；读面只见 fingerprint）。**留空 = 保留已存凭证**
             *     （SMTP 密码同款先例——wildcard 开关切换不要求重录）；dns_provider=none
             *     时恒清空。dnspod 形态 "<id>,<token>"；cloudflare 为单 token。
             */
            api_token?: string;
            /** 通配证书 opt-in 开关。 */
            wildcard?: boolean;
        };
        v1UpdateAcmeSettingsResponse: {
            settings?: components["schemas"]["v1AcmeSettingsView"];
        };
        v1UpdateRegistrySettingsRequest: {
            /**
             * 外部 registry host（ghcr.io 形态；scheme 会被归一剥离）。空 = 清除
             *     全部设置（host/用户名/密码与指纹）。
             */
            host?: string;
            username?: string;
            /**
             * 密码明文（只写字段；读面只见 fingerprint）。**留空 = 保留已存密码**
             *     （ACME api_token 同款先例）；清除走 host 留空。
             */
            password?: string;
        };
        v1UpdateRegistrySettingsResponse: {
            settings?: components["schemas"]["v1RegistrySettingsView"];
        };
        v1UpdateS3SettingsRequest: {
            /** 模式词表（空 = unset）。external↔rustfs 互斥校验见 rpc 注记。 */
            mode?: string;
            endpoint_url?: string;
            region?: string;
            bucket?: string;
            access_key_id?: string;
            /**
             * secret 明文（只写字段；读面只见 fingerprint）。PUT 语义：留空 = 无
             *     secret（切换到 rustfs/unset 时随全量覆写自然清空外部凭证）。
             */
            secret_access_key?: string;
            path_style?: boolean;
            public_exposed?: boolean;
        };
        v1UpdateS3SettingsResponse: {
            settings?: components["schemas"]["v1S3SettingsView"];
        };
        /** UpdatePlacementRequest 是显式换点请求（admin scope；破坏性确认路径）。 */
        PlacementServiceUpdatePlacementBody: {
            /** 目标节点（唯一显示名或平台 ID）。 */
            node?: string;
            /** 数据处置声明：""（无卷应用）| restored | discarded。 */
            data_ack?: string;
            /** 破坏性确认（data_ack=discarded 时必填——源节点数据成为残留）。 */
            confirm?: boolean;
        };
        v1GetPlacementMigrationPlanResponse: {
            app?: string;
            /** 源/目标节点人读形态（hostname (platform ID)）。 */
            from_node?: string;
            to_node?: string;
            /** 涉及的 active 卷。 */
            volumes?: components["schemas"]["v1VolumeView"][];
            /** 顺序步骤（停写 → restic 备份/恢复 → rebind → deploy 收敛 → 残留清理）。 */
            steps?: components["schemas"]["v1MigrationStep"][];
            /** 计划级警示（如目标节点当前非 ready）。 */
            warnings?: string[];
        };
        v1ListVolumesResponse: {
            /** 应用显示名（已删除应用回退显示 app id——与证书台账同口径）。 */
            volumes?: components["schemas"]["v1VolumeView"][];
        };
        /** MigrationStep 是迁移 runbook 的一步（title 短语 + 可复制 detail）。 */
        v1MigrationStep: {
            title?: string;
            detail?: string;
        };
        v1ShowPlacementResponse: {
            app?: string;
            placement?: components["schemas"]["v1PlacementView"];
            /** 卷注册表（无卷应用为空集）。 */
            volumes?: components["schemas"]["v1VolumeView"][];
        };
        v1UpdatePlacementResponse: {
            app?: string;
            placement?: components["schemas"]["v1PlacementView"];
            volumes?: components["schemas"]["v1VolumeView"][];
        };
        /**
         * VolumeView 是卷注册表行投影（volumes 表；orphaned 状态位经 status 透出
         *     ——删除应用保留卷）。
         */
        v1VolumeView: {
            key?: string;
            name?: string;
            kind?: string;
            /** 平台节点 ID（与 PlacementView.platform_node_id 同词族）。 */
            platform_node_id?: string;
            mount_path?: string;
            status?: string;
            /**
             * 数据原在节点（E1-7 迁移 00010：显式换点登记的源节点；空 = 从未跨
             *     节点迁移）。指向源节点的残留副本清理指引。
             */
            prev_platform_node_id?: string;
            /**
             * 残留标记（prev_platform_node_id 非空的 active 行派生 = 源节点有
             *     待清理副本，docker volume rm 后平台对账消失；只指引不代删——D18）。
             */
            residual?: boolean;
        };
        CronServiceTriggerCronRunBody: Record<string, never>;
        /**
         * CronRunView 是一次 cron 触发的台账投影（状态词表 started | succeeded |
         *     failed | timeout | skipped；skipped 行带 skip_reason：
         *     overlap | node_unavailable | missed_downtime | interrupted）。
         */
        v1CronRunView: {
            id?: string;
            service?: string;
            expression?: string;
            /**
             * 命中的 cron 点（手动触发 = 触发时刻）。
             * Format: date-time
             */
            scheduled_at?: string;
            /**
             * job 启动时刻（skipped 行不输出）。
             * Format: date-time
             */
            started_at?: string;
            /**
             * 终态收口时刻（在途/skipped 行不输出）。
             * Format: date-time
             */
            finished_at?: string;
            status?: string;
            skip_reason?: string;
            /** 一次性 job 服务名（skipped 行不输出）。 */
            job_service?: string;
            /** 失败/超时原因摘要。 */
            error?: string;
        };
        v1ListCronRunsResponse: {
            app?: string;
            runs?: components["schemas"]["v1CronRunView"][];
        };
        v1TriggerCronRunResponse: {
            run?: components["schemas"]["v1CronRunView"];
        };
        DatabaseServiceRestoreDatabaseBackupBody: {
            /** 恢复目标（restic snapshot 标识——必须在本实例台账内，跨实例误指 422）。 */
            snapshot?: string;
            /**
             * 破坏性确认 = 实例名原样回传（mismatch → 400——原地重放覆盖数据卷上
             *     的现库，与 DeleteDatabase 同型的数据安全面）。
             */
            confirm?: string;
        };
        DatabaseServiceResumeDatabaseBody: Record<string, never>;
        DatabaseServiceRetryDatabaseBody: Record<string, never>;
        /**
         * RotateDatabaseCredentialsRequest 是凭据轮换受理（破坏性两段式：confirm =
         *     实例名原样回传，mismatch → 400——与 DeleteDatabase 同型的数据安全面）。
         */
        DatabaseServiceRotateDatabaseCredentialsBody: {
            confirm?: string;
        };
        DatabaseServiceSuspendDatabaseBody: Record<string, never>;
        DatabaseServiceTriggerDatabaseBackupBody: {
            /**
             * 备份类别（缺省 manual；API 面只受理 manual——daily/pre_upgrade 是平台
             *     调度与升级门的内部类别）。
             */
            kind?: string;
        };
        DatabaseServiceUpdateDatabaseSettingsBody: {
            limits?: components["schemas"]["v1DatabaseLimits"];
            backup_plan?: components["schemas"]["v1DatabaseBackupPlan"];
        };
        DatabaseServiceUpgradeDatabaseBody: {
            /**
             * 破坏性确认 = 实例名原样回传（mismatch → 400——受控重建有停机窗口，
             *     且失败路径触发 digest 归位重建）。
             */
            confirm?: string;
        };
        v1CreateDatabaseRequest: {
            /**
             * 库实例名（^[a-z0-9][a-z0-9_-]*$——与 app 名同字符集规则；对象前缀族
             *     fleetly-db-* 与 app 名族解耦，app 与库实例可重名）。project 内唯一
             *     （D-W0-4 二修——跨项目同名实例合法）。
             */
            name?: string;
            /**
             * 模板 ID（平台内置注册表：postgres-16 / postgres-18 /
             *     percona-postgresql-18 / redis-7 / mysql-8.4 / mongodb-8.0；未知 → 400）。
             *     **大版本升级不做**（创建时钉死）：升 major = dump/restore 到新实例；
             *     minor 由镜像 digest 钉定、随平台 release 以同卷受控重建演进
             *     （UpgradeDatabase）。
             */
            template?: string;
            limits?: components["schemas"]["v1DatabaseLimits"];
            backup_plan?: components["schemas"]["v1DatabaseBackupPlan"];
            /**
             * 目标项目（v0.3 W2-S3 归属管道，D-W0-9 解析规则）：裸名或
             *     `team/project` 限定形；解析规则与缺省语义同 DeployRequest.project
             *     （机具令牌必须显式）。首次创建写入 db_instances.project_id/team_id；
             *     行上归属已定时必须一致（409 E_APP_PROJECT_MISMATCH 指引 MoveDatabase）。
             */
            project?: string;
        };
        v1CreateDatabaseResponse: {
            database?: components["schemas"]["v1DatabaseView"];
        };
        /**
         * DatabaseBackupPlan 是备份计划（§5.4 配置键 databases.backup_* 的 per
         *     实例覆盖；0 值字段 = 平台缺省）。
         */
        v1DatabaseBackupPlan: {
            /** Format: int32 */
            interval_hours?: number;
            /** Format: int32 */
            keep?: number;
            /** Format: int32 */
            hour_utc?: number;
        };
        /** DatabaseBackupView 是一行备份台账投影（§2.6 台账裁决的全量事实面）。 */
        v1DatabaseBackupView: {
            id?: string;
            /** 备份类别（daily|manual|pre_upgrade）。 */
            kind?: string;
            /** restic repo 内 snapshot 标识（db/<instance>/ 命名空间寻址，非文件路径）。 */
            snapshot?: string;
            /**
             * 导出流字节量（0 = 未记录）。
             * Format: int64
             */
            size_bytes?: string;
            /** 回读校验状态（unverified|verified|failed——「备份假成功」零容忍）。 */
            verify_status?: string;
            /** 失败/校验失败原因摘要（单行；凭据材料零出现）。 */
            error?: string;
            /** Format: date-time */
            created_at?: string;
        };
        /**
         * DatabaseConnectionView 是连接信息脱敏投影（§2.5 键集的只读面）。url 中
         *     密码段恒为固定掩码（********）——明文零离开存储；host = 实例名 DNS 别名
         *     （引用方 app 内即以此可达）。
         */
        v1DatabaseConnectionView: {
            host?: string;
            /** Format: int32 */
            port?: number;
            /**
             * PG 有 user/database；Redis 为空串/0（gateway EmitUnpopulated=true 下
             *     显式输出——消费方按空值跳过渲染）。
             */
            user?: string;
            database?: string;
            /** 掩码 URL（postgres://fleetly:********@<host>:5432/<db> / redis://:********@<host>:6379/0）。 */
            url?: string;
            /** 密码指纹（sha256 前 8 hex——只判「是不是那个值」，材料零出现）。 */
            password_fingerprint?: string;
        };
        /** DatabaseLimits 是资源限额（仅 limits——镜像/引擎参数受管）。 */
        v1DatabaseLimits: {
            /**
             * CPU 限额（核数；0 = 模板缺省）。
             * Format: double
             */
            cpu_seconds?: number;
            /**
             * 内存限额（字节；0 = 模板缺省）。
             * Format: int64
             */
            memory_bytes?: string;
        };
        /**
         * DatabaseView 是库实例投影（状态 = 生命周期态；连接信息脱敏——密码明文
         *     零离开存储，url 已掩码、password_fingerprint 供「是不是那个 secret」比
         *     对；显式 reveal 面随 S4/S6）。last_error 是最近一次收敛失败原因（failed
         *     诊断面；'' = 无失败现场）。
         */
        v1DatabaseView: {
            id?: string;
            name?: string;
            template?: string;
            image_digest?: string;
            /** 生命周期状态位（provisioning|ready|failed|degraded|paused|deleting|deleted）。 */
            status?: string;
            /** 放置绑定（平台节点 ID；空 = 未绑定——provisioning 首拍前）。 */
            placement?: string;
            volume?: components["schemas"]["v1DatabaseVolumeView"];
            connection?: components["schemas"]["v1DatabaseConnectionView"];
            backup_plan?: components["schemas"]["v1DatabaseBackupPlan"];
            limits?: components["schemas"]["v1DatabaseLimits"];
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
            /** Format: date-time */
            credential_updated_at?: string;
            /** 最近一次收敛失败原因（failed/deleting 前的现场快照；空 = 无失败现场）。 */
            last_error?: string;
            /**
             * 可升级位（E4 S5，§2.2 升级语义）：instance.image_digest ≠ 模板当前钉
             *     定镜像 = true（升级逐实例 opt-in——既有实例不自动变）。
             */
            upgrade_available?: boolean;
        };
        v1DatabaseVolumeView: {
            name?: string;
            status?: string;
            /** 数据节点（卷钉住语义——与 placement 一致）。 */
            platform_node_id?: string;
        };
        v1DeleteDatabaseResponse: {
            name?: string;
            status?: string;
        };
        v1GetDatabaseResponse: {
            database?: components["schemas"]["v1DatabaseView"];
        };
        v1ListDatabaseBackupsResponse: {
            backups?: components["schemas"]["v1DatabaseBackupView"][];
        };
        v1ListDatabasesResponse: {
            databases?: components["schemas"]["v1DatabaseView"][];
        };
        /**
         * 异步受理响应：停库重放分钟级——结论经 db.restore_completed /
         *     db.restore_failed 事件与实例 last_error 披露；恢复中断 = 实例保持停止
         *     （人工 runbook 随事件/错误文本）。
         */
        v1RestoreDatabaseBackupResponse: {
            name?: string;
            snapshot?: string;
            /** 恒为 "accepted"。 */
            status?: string;
        };
        v1ResumeDatabaseResponse: {
            database?: components["schemas"]["v1DatabaseView"];
        };
        v1RetryDatabaseResponse: {
            database?: components["schemas"]["v1DatabaseView"];
        };
        /**
         * RevealDatabaseCredentialsResponse 是连接信息的显式展开投影（§2.5 键集
         *     全量 + 密码明文——admin scope 专用面；值只出现在本响应，不进日志/事件/
         *     审计，审计 db.reveal 只记访问事实）。
         */
        v1RevealDatabaseCredentialsResponse: {
            name?: string;
            template?: string;
            host?: string;
            /** Format: int32 */
            port?: number;
            /** PG 有 user/database；Redis 不输出（空串）。 */
            user?: string;
            database?: string;
            /** 密码明文（显式展开面的设计内例外；凭据字符集 [a-zA-Z0-9]）。 */
            password?: string;
            /** 标准 URI（密码段为明文——与 GetEnv 明文读同级的 admin 面）。 */
            url?: string;
        };
        v1RotateDatabaseCredentialsResponse: {
            database?: components["schemas"]["v1DatabaseView"];
            /**
             * 平台自动重部署的引用 app 名单（各自走正常部署队列；credential_updated_at
             *     展示随 database.credential_updated_at 刷新）。
             */
            redeployed_apps?: string[];
        };
        v1SuspendDatabaseResponse: {
            database?: components["schemas"]["v1DatabaseView"];
        };
        /**
         * 异步受理响应：job 分钟级——结论经台账（ListDatabaseBackups）与
         *     db.backup_succeeded / db.backup_failed 事件披露；在途备份无台账行。
         */
        v1TriggerDatabaseBackupResponse: {
            name?: string;
            kind?: string;
            /** 恒为 "accepted"。 */
            status?: string;
        };
        v1UpdateDatabaseSettingsResponse: {
            database?: components["schemas"]["v1DatabaseView"];
        };
        /**
         * 异步受理响应：备份门与健康门是分钟级——结论经 db.upgrade_started/
         *     finished/failed 事件披露。view = 受理时刻投影（upgrade_available 尚为
         *     true；digest 切换随编排推进）。
         */
        v1UpgradeDatabaseResponse: {
            database?: components["schemas"]["v1DatabaseView"];
            /** 恒为 "accepted"。 */
            status?: string;
        };
        SecretsServiceSetSecretBody: {
            /**
             * secret 声明名（compose 服务级 secrets 引用的短名；合法 /run/secrets/<name>
             *     文件名字符集 ^[A-Za-z0-9][A-Za-z0-9._-]*$——与 internal/compose 的
             *     secret 名校验同规则，注释锚互指）。
             */
            name?: string;
            /**
             * 值（明文；age 加密在服务端。上限 64KiB sanity——Swarm secret 单对象
             *     上界 500KB 的宽松内档；长度进形状层即拒，不落日志）。
             */
            value?: string;
        };
        v1ListSecretsResponse: {
            secrets?: components["schemas"]["v1SecretView"][];
        };
        v1RemoveSecretResponse: {
            app?: string;
            name?: string;
        };
        /**
         * SecretView 是平台密钥库条目的只读投影（值/密文/明文零出现——D-DB-7
         *     无值读回；hash8 是唯一的值比对面）。
         */
        v1SecretView: {
            name?: string;
            hash8?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1SetSecretResponse: {
            app?: string;
            name?: string;
            /** 值指纹（sha256 前 8 hex——「是不是那个值」比对面；值材料零出现）。 */
            hash8?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        ConfigsServiceSetConfigBody: {
            /**
             * config 声明名（compose 服务级 configs 引用的短名；合法标识符字符集
             *     ^[A-Za-z0-9][A-Za-z0-9._-]*$——与 internal/compose 的 config 名校验同
             *     规则，注释锚互指）。
             */
            name?: string;
            /**
             * 值（明文；上限 64KiB sanity——与 secrets 口径一致：swarm config 单对象
             *     上界 500KB 的宽松内档；长度进形状层即拒）。
             */
            value?: string;
        };
        /**
         * ConfigView 是配置资源的只读投影（值零出现——值只在 GetConfig 的显式
         *     admin 回读路径；hash8 是内容寻址与「是不是那一版」比对面）。
         */
        v1ConfigView: {
            name?: string;
            hash8?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1GetConfigResponse: {
            app?: string;
            name?: string;
            /** 配置内容明文（admin scope；与 GetEnv 的回读信任同级）。 */
            value?: string;
            hash8?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1ListConfigsResponse: {
            configs?: components["schemas"]["v1ConfigView"][];
        };
        v1RemoveConfigResponse: {
            app?: string;
            name?: string;
        };
        v1SetConfigResponse: {
            app?: string;
            name?: string;
            /** 内容指纹（sha256 前 8 hex——swarm config 对象名的内容寻址尾缀）。 */
            hash8?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        NotificationsServiceRotateWebhookSecretBody: Record<string, never>;
        NotificationsServiceTestWebhookBody: Record<string, never>;
        NotificationsServiceUpdateWebhookEndpointBody: {
            /** 改名（可选）。 */
            name?: string;
            /**
             * 改 URL（可选；webhook/slack 非空——形状在服务端校验；显式空串仅允许
             *     伴随换通道到 email，单独置空被服务端拒绝）。
             */
            url?: string;
            /**
             * 改订阅模式集（可选——repeated 无 optional 语义：空 = 不变，非空 =
             *     整体替换且过白名单校验）。
             */
            event_patterns?: string[];
            /** 启停（可选——enabled 订阅开关；停用端点暂停投递不删台账）。 */
            enabled?: boolean;
            /**
             * 通道类型（可选；词表同 Create——组合形状在服务端对「更新后的最终
             *     形态」校验）。
             */
            type?: string;
            /**
             * email 通道收件地址（可选；非空必须合法邮箱形状，显式空串仅允许伴随
             *     换通道离开 email）。
             */
            target?: string;
        };
        v1CreateWebhookEndpointRequest: {
            /** 端点名（唯一；1..64，字母数字开头）。 */
            name?: string;
            /**
             * 接收端 URL（webhook/slack 通道必填；http/https；host 必填；不带
             *     userinfo。email 通道必须为空——投递走平台 SMTP 设置）。
             */
            url?: string;
            /** 订阅模式集（非空；每项 1..128 chars of [a-z0-9._-*]；服务端去重保序）。 */
            event_patterns?: string[];
            /** 创建即启用（缺省 true）。 */
            enabled?: boolean;
            /**
             * 通道类型（webhook | slack | email；空串 = webhook 缺省。组合形状
             *     [url/target 互斥、email target 必须合法邮箱] 在服务端校验——跨字段
             *     条件不做 protovalidate CEL，400 形状门 + state 层白名单双闸）。
             */
            type?: string;
            /** email 通道收件地址（to；非 email 通道必须为空）。 */
            target?: string;
        };
        v1CreateWebhookEndpointResponse: {
            endpoint?: components["schemas"]["v1WebhookEndpointView"];
            /** 签名密钥明文（32B base64）——**仅本次响应可见**。 */
            secret?: string;
        };
        v1DeleteWebhookEndpointResponse: {
            id?: string;
        };
        v1GetSmtpSettingsResponse: {
            settings?: components["schemas"]["v1SmtpSettingsView"];
        };
        v1GetWebhookEndpointResponse: {
            endpoint?: components["schemas"]["v1WebhookEndpointView"];
        };
        v1ListWebhookDeliveriesResponse: {
            deliveries?: components["schemas"]["v1WebhookDeliveryView"][];
        };
        v1ListWebhookEndpointsResponse: {
            endpoints?: components["schemas"]["v1WebhookEndpointView"][];
        };
        v1RotateWebhookSecretResponse: {
            /** 新签名密钥明文（32B base64）——**仅本次响应可见**。 */
            secret?: string;
            /** 新密钥指纹（读面对齐）。 */
            secret_fingerprint?: string;
        };
        /**
         * SmtpSettingsView 是 notify.smtp.* 设置的只读投影：密码只出指纹（明文与
         *     密文永不回读——S3 secret 卡同口径）。
         */
        v1SmtpSettingsView: {
            /** SMTP 中继主机（空 = 未设置）。 */
            host?: string;
            /**
             * SMTP 端口（1..65535；未设置 = 0）。
             * Format: int32
             */
            port?: number;
            /** 认证用户名（可选——无认证中继为空）。 */
            username?: string;
            /** 密码指纹（明文 sha256 前 8 hex；空 = 未设置）。 */
            password_fingerprint?: string;
            /** 信封发件地址（From）。 */
            from?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        v1TestSmtpRequest: {
            /** 测试邮件收件地址（必填；合法邮箱形状）。 */
            to?: string;
            /**
             * 候选配置（未保存也能测——S3 探针同语义）：全部候选字段为空时测已存
             *     配置。候选密码明文只在本请求内使用，绝不落库。字段全部 optional——
             *     未设置的形状规则跳过（零值不是「空邮箱」违约），服务端按「全空 =
             *     测已存」收敛。
             */
            host?: string;
            /** Format: int32 */
            port?: number;
            username?: string;
            password?: string;
            from?: string;
        };
        /**
         * TestSmtpResponse 是 SMTP 探针的同步结论（ok = 邮件被中继接受；error =
         *     失败步单行摘要，密码材料零出现）。
         */
        v1TestSmtpResponse: {
            ok?: boolean;
            /**
             * SMTP 会话完成码（250 语义；未达 DATA 应答 = 0）。
             * Format: int32
             */
            status_code?: number;
            /** 失败摘要（单行化；成功为空）。 */
            error?: string;
        };
        /**
         * TestWebhookResponse 是 type=test 载荷按通道类型试发的同步结论（设计
         *     §5.2/§8.2：webhook/slack = HTTP 语义；email = SMTP 会话语义）。
         */
        v1TestWebhookResponse: {
            /** 投递被接收方接受（webhook/slack 2xx；email SMTP 会话完成）。 */
            ok?: boolean;
            /**
             * HTTP 响应码（webhook/slack；传输失败 = 0。email 通道不出码）。
             * Format: int32
             */
            status_code?: number;
            /** 失败摘要（单行化；成功为空）。 */
            error?: string;
        };
        v1UpdateSmtpSettingsRequest: {
            /** SMTP 中继主机（必填）。 */
            host?: string;
            /**
             * SMTP 端口（1..65535）。
             * Format: int32
             */
            port?: number;
            /** 认证用户名（可选；空 = 无认证中继）。 */
            username?: string;
            /**
             * 密码明文（**只写不读**——空 = 清除已存密码；envelope 加密落库，读面
             *     只出指纹）。
             */
            password?: string;
            /** 信封发件地址（From；必须合法邮箱形状）。 */
            from?: string;
        };
        v1UpdateSmtpSettingsResponse: {
            settings?: components["schemas"]["v1SmtpSettingsView"];
        };
        v1UpdateWebhookEndpointResponse: {
            endpoint?: components["schemas"]["v1WebhookEndpointView"];
        };
        /**
         * WebhookDeliveryView 是投递台账行的投影：状态/尝试数/响应码/下次重试——
         *     重试路径与终败的诚实可见面（投递失败不产生事件，台账即事实源）。
         */
        v1WebhookDeliveryView: {
            id?: string;
            /**
             * 触发投递的事件 seq。
             * Format: int64
             */
            event_seq?: string;
            endpoint_id?: string;
            /** pending | ok | failed（failed = 终态：3 次尝试耗尽）。 */
            status?: string;
            /** Format: int32 */
            attempts?: number;
            /**
             * 最近一次 HTTP 响应码（传输失败不出现在 JSON 中——proto3 零值语义）。
             * Format: int32
             */
            response_code?: number;
            /** 最近一次失败摘要（成功为空）。 */
            last_error?: string;
            /**
             * 下次重试时刻（终态/等待首发的行不输出）。
             * Format: date-time
             */
            next_retry_at?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
        };
        /**
         * WebhookEndpointView 是端点行的无敏感投影：密钥只出指纹（明文与密文
         *     永不回读）。
         */
        v1WebhookEndpointView: {
            id?: string;
            name?: string;
            /**
             * 接收端 URL（webhook/slack 通道；操作员自配——单操作员信任模型，无
             *     SSRF 过滤，设计 §5.1 诚实口径；https 建议但不强制。email 通道恒空）。
             */
            url?: string;
            /** 订阅模式集（事件名 glob；`*` = 全订）。 */
            event_patterns?: string[];
            enabled?: boolean;
            /** 签名密钥指纹（明文 sha256 前 8 hex——只判「是不是那个 secret」）。 */
            secret_fingerprint?: string;
            /** Format: date-time */
            created_at?: string;
            /** Format: date-time */
            updated_at?: string;
            /**
             * 通道类型（webhook | slack | email；缺省 webhook——存量端点升级即
             *     webhook，行为逐字不变，设计 §8.1）。
             */
            type?: string;
            /**
             * email 通道收件地址（to；webhook/slack 恒空——设计 §8.1 通道与端点
             *     解耦：投递凭据在平台级 SMTP 设置面，端点只带收件地址）。
             */
            target?: string;
        };
        v1CreateTerminalTicketRequest: {
            /** 目标应用名。 */
            app?: string;
            /**
             * 目标服务名（app 内 compose 服务名；多副本时由控制面择 running 任务，
             *     副本细节对操作员透明——设计 §2.3）。
             */
            service?: string;
        };
        v1CreateTerminalTicketResponse: {
            /** 一次性 ticket（随机 128bit URL-safe；60s 过期、单次使用）。 */
            ticket?: string;
            /**
             * 过期时刻（UTC）。
             * Format: date-time
             */
            expires_at?: string;
            /**
             * WS 接入路径（相对 gateway 同源；Console 拼协议与 host）：
             *     `/v1/terminal?ticket=<ticket>`。
             */
            websocket_path?: string;
            /**
             * 有效秒数（60——与 expires_at 冗余的诚实倒计时面）。
             * Format: int32
             */
            expires_in_seconds?: number;
        };
        v1GetTerminalStatusResponse: {
            /**
             * 终端功能是否启用（config terminal.enabled；false = duty 不部署 relay，
             *     CreateTerminalTicket 报 E_TERMINAL_DISABLED）。
             */
            enabled?: boolean;
            /** fleetly-exec relay 服务部署态（global 服务在位与否）。 */
            relay_deployed?: boolean;
            /** relay 实况镜像引用（不在位为空）。 */
            relay_image?: string;
            /**
             * 已连接 relay 节点数（连接表大小——node liveness = 连接存在）。
             * Format: int32
             */
            nodes_connected?: number;
            /**
             * 活跃终端会话数（全局——上限 8，per-token 上限 2）。
             * Format: int32
             */
            active_sessions?: number;
        };
    };
    responses: never;
    parameters: never;
    requestBodies: never;
    headers: never;
    pathItems: never;
}
export type $defs = Record<string, never>;
export interface operations {
    AuthService_AcceptInvite: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1AcceptInviteRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1AcceptInviteResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AuthService_Login: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1LoginRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1LoginResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AuthService_Logout: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1LogoutRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1LogoutResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AuthService_LogoutAll: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1LogoutAllRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1LogoutAllResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AuthService_Me: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1MeResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AuthService_Register: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1RegisterRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RegisterResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AuthService_GetRegistrationState: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetRegistrationStateResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TokensService_ListTokens: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListTokensResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TokensService_CreateToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1CreateTokenRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateTokenResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TokensService_RevokeToken: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RevokeTokenResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_ListTeams: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListTeamsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_CreateTeam: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1CreateTeamRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateTeamResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_GetTeam: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetTeamResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_DeleteTeam: {
        parameters: {
            query?: {
                /**
                 * @description 两段式确认：confirm 必须等于团队 slug（数据/归属安全同 DeleteDatabase
                 *     口径）。
                 */
                confirm?: string;
            };
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DeleteTeamResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_UpdateTeam: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["TeamsServiceUpdateTeamBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateTeamResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_ListTeamInvites: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                team_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListTeamInvitesResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_CreateInvite: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                team_id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["TeamsServiceCreateInviteBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateInviteResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_RevokeInvite: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                team_id: string;
                invite_id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["TeamsServiceRevokeInviteBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RevokeInviteResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_ListTeamMembers: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                team_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListTeamMembersResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_RemoveTeamMember: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                team_id: string;
                user_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RemoveTeamMemberResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    TeamsService_SetTeamMemberRole: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                team_id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["TeamsServiceSetTeamMemberRoleBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetTeamMemberRoleResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_AttachAppProjectNetwork: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1AttachAppProjectNetworkResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_DetachAppProjectNetwork: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DetachAppProjectNetworkResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_ListProjects: {
        parameters: {
            query?: {
                /** @description 可选：收窄到单队（空 = 我可见全部——跨我所在团队；平台管理员 = 全部）。 */
                team_id?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListProjectsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_CreateProject: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1CreateProjectRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateProjectResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_GetProject: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetProjectResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_DeleteProject: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DeleteProjectResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_UpdateProject: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProjectsServiceUpdateProjectBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateProjectResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_ListProjectMembers: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                project_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListProjectMembersResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_RemoveProjectMember: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                project_id: string;
                user_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RemoveProjectMemberResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_SetProjectMemberRole: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                project_id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProjectsServiceSetProjectMemberRoleBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetProjectMemberRoleResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_MoveApp: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 目标项目平台 ID（管理面用 ID，D-W0-9——免疫同名项目歧义）。 */
                to_project_id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProjectsServiceMoveAppBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1MoveAppResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ProjectsService_MoveDatabase: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 目标项目平台 ID。 */
                to_project_id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ProjectsServiceMoveDatabaseBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1MoveDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AuditService_ListAudit: {
        parameters: {
            query?: {
                actor?: string;
                action?: string;
                /** @description ok | error（其他值恒空集——精确匹配语义，服务端不再另行校验词表）。 */
                result?: string;
                target?: string;
                since?: string;
                until?: string;
                /** @description 分页（非正/缺省 = 服务端页大小 100，天花板 1000；offset 非负）。 */
                limit?: number;
                offset?: number;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListAuditResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_ListApps: {
        parameters: {
            query?: {
                /** @description 列表上限（缺省 100；v0.1 单机规模不做分页游标）。 */
                limit?: number;
                /**
                 * @description 项目收窄（v0.3 W2-S4 可见性过滤，rbac-teams §4.2）：裸名或 `team/project`
                 *     限定形（D-W0-9 解析规则；解析域 = 调用方可见项目集，机具令牌/平台管理
                 *     员 = 全库）。空 = 不收窄（用户面仍按可见项目集过滤）。
                 */
                project?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListAppsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_GetApp: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 应用名（compose 应用名，权威态唯一键）。 */
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetAppResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_DeleteApp: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DeleteAppResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_ResumeApp: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 应用名（裸名/限定形，resolveApp 单点解析）。 */
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["AppsServiceResumeAppBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ResumeAppResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_GetScalingPolicy: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 应用名（裸名/限定形，resolveApp 单点解析）。 */
                name: string;
                /** @description compose 服务名。 */
                service: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetScalingPolicyResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_SetScalingPolicy: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
                service: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["AppsServiceSetScalingPolicyBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetScalingPolicyResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_RemoveScalingPolicy: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
                service: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RemoveScalingPolicyResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_SetAppSource: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["AppsServiceSetAppSourceBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetAppSourceResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_SuspendApp: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 应用名（裸名/限定形，resolveApp 单点解析）。 */
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["AppsServiceSuspendAppBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SuspendAppResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_ShowAppWebhook: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ShowAppWebhookResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AppsService_SetAppWebhookSecret: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 应用名。 */
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["AppsServiceSetAppWebhookSecretBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetAppWebhookSecretResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DeploymentsService_ListDeployments: {
        parameters: {
            query?: {
                limit?: number;
            };
            header?: never;
            path: {
                /** @description 应用名（列表必选）。 */
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListDeploymentsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DeploymentsService_Deploy: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /**
                 * @description 应用名（不存在时自动创建——与 CLI deploy 同语义：应用随首次部署创建）。
                 *     compose 应用名与请求 app 必须一致（A1：不一致 → E_COMPOSE_UNSUPPORTED，
                 *     不误建 app、不入队）。app 名 project 内唯一（D-W0-4 二修）——同一项目
                 *     内重复部署沿用行上归属（请求 project 必须一致，不一致 409
                 *     E_APP_PROJECT_MISMATCH 指引 MoveApp）。
                 */
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DeploymentsServiceDeployBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DeployResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DeploymentsService_RollbackDeployment: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DeploymentsServiceRollbackDeploymentBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RollbackDeploymentResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DeploymentsService_GetDeployment: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetDeploymentResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DeploymentsService_CancelDeployment: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DeploymentsServiceCancelDeploymentBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CancelDeploymentResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    BuildsService_ListBuilds: {
        parameters: {
            query?: {
                /** @description 返回上限（缺省 20，天花板 100）。 */
                limit?: number;
            };
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListBuildsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    BuildsService_TriggerBuild: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1TriggerBuildRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TriggerBuildResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    BuildsService_GetBuild: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetBuildResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DriftService_ShowDrift: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ShowDriftResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DriftService_ConvergeDrift: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DriftServiceConvergeDriftBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ConvergeDriftResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DriftService_SetDriftConverge: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DriftServiceSetDriftConvergeBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetDriftConvergeResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    RuntimeService_ShowAppRuntime: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ShowAppRuntimeResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    RevisionsService_ListRevisions: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListRevisionsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    RevisionsService_GetRevisionSpec: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                /** @description 目标快照 ID（active 集内；superseded → 404，与回滚选项面同口径）。 */
                revision_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetRevisionSpecResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    EnvService_ListEnv: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListEnvResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    EnvService_GetEnv: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                key: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetEnvResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    EnvService_SetEnv: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                key: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["EnvServiceSetEnvBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetEnvResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    EnvService_RemoveEnv: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                key: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RemoveEnvResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DomainsService_ListAppDomains: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListAppDomainsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DomainsService_CreateAppDomain: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DomainsServiceCreateAppDomainBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateAppDomainResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DomainsService_VerifyAppDomains: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DomainsServiceVerifyAppDomainsBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1VerifyAppDomainsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DomainsService_UpdateAppDomain: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                /** @description 寻址键（host，不改名）。 */
                domain: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DomainsServiceUpdateAppDomainBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateAppDomainResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DomainsService_RemoveAppDomain: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                domain: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RemoveAppDomainResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    LogsService_ListHistoryLogs: {
        parameters: {
            query?: {
                /** @description compose 服务名；空 = 全部服务。 */
                service?: string;
                /** @description 时间窗下界；缺省 = 保留窗起点。 */
                since?: string;
                /** @description 时间窗上界；缺省 = 现在。 */
                until?: string;
                /** @description 返回上限（缺省 200，天花板 1000；超过取最新 limit 条）。 */
                limit?: number;
                /** @description 来源过滤：container | build；空 = 全部。 */
                source?: string;
            };
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListHistoryLogsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    LogsService_SearchLogs: {
        parameters: {
            query?: {
                /**
                 * @description 可选的 app 过滤集（预留跨应用语义；当前检索面为单 app，额外值不
                 *     放行——诚实边界）。
                 */
                apps?: string[];
                /** @description 全文关键词（构造为转义后的 LogsQL 字面量短语——注入安全硬性条款）。 */
                keyword?: string;
                /** @description 时间窗下界；缺省 = 不设下界（保留窗即 VL -retentionPeriod）。 */
                time_start?: string;
                /** @description 时间窗上界；缺省 = 现在。 */
                time_end?: string;
                /** @description compose 服务名过滤集。 */
                services?: string[];
                /**
                 * @description 来源过滤集：container | build | access（access 随 W5-S2 访问日志
                 *     采集进入词表）。
                 */
                sources?: string[];
                /** @description 返回上限（缺省 200，天花板 1000）。 */
                limit?: number;
                /**
                 * @description 分页游标（服务端签发的下一页凭证；空 = 第一页）。游标分页自最新
                 *     命中向后走（VL limit/offset 语义）。
                 */
                cursor?: string;
                /**
                 * @description 任务流选择器（DT-5 任务日志面；值 = 任务平台 ID——入湖 task 流标签）。
                 *     非空时 app 可空（任务行无 app 归属）。
                 */
                tasks?: string[];
            };
            header?: never;
            path: {
                /**
                 * @description app 流选择器（三段限定形 team/prj/app 或裸名；用户凭据强制限定形）。
                 *     空 = 仅按 tasks 选择器查询（任务日志；全局调用方限定）。
                 */
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SearchLogsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    LogsService_FollowLogs: {
        parameters: {
            query?: {
                /** @description compose 服务名；空 = 该 app 全部服务。 */
                service?: string;
            };
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response.(streaming responses) */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        result?: components["schemas"]["v1FollowLogsResponse"];
                    };
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    LogsService_GetLogsBackend: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetLogsBackendResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    LogsService_SetLogsBackend: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1SetLogsBackendRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetLogsBackendResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    MetricsService_SetMetricsMode: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1SetMetricsModeRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetMetricsModeResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    MetricsService_SearchMetrics: {
        parameters: {
            query?: {
                /** @description PromQL 表达式——**透传**（不转义、不校验语法、无沙箱；VM 裁决）。 */
                query?: string;
                /** @description 时间窗下界；缺省 = 现在回望 1 小时。 */
                time_start?: string;
                /** @description 时间窗上界；缺省 = 现在。 */
                time_end?: string;
                /** @description 步长秒数（缺省 60）。 */
                step_seconds?: number;
                /** @description 返回序列数上限（缺省 200，天花板 1000——规范化面截断）。 */
                limit?: number;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SearchMetricsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    MetricsService_GetMetricsStatus: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetMetricsStatusResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AlertingService_SetAlertsMode: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1SetAlertsModeRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetAlertsModeResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AlertingService_ListAlertRules: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListAlertRulesResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AlertingService_CreateAlertRule: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1CreateAlertRuleRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateAlertRuleResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AlertingService_UpdateAlertRule: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["AlertingServiceUpdateAlertRuleBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateAlertRuleResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AlertingService_DeleteAlertRule: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DeleteAlertRuleResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AlertingService_TestAlertRule: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1TestAlertRuleRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TestAlertRuleResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    AlertingService_GetAlertsStatus: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetAlertsStatusResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    EventsService_WatchEvents: {
        parameters: {
            query?: {
                /** @description 游标：返回 seq > since_seq 的事件（升序）；0 = 从保留窗起点。 */
                since_seq?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response.(streaming responses) */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        result?: components["schemas"]["v1WatchEventsResponse"];
                    };
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_GetAcmeSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetAcmeSettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_UpdateAcmeSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1UpdateAcmeSettingsRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateAcmeSettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_TestDnsProvider: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1TestDnsProviderRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TestDnsProviderResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_ListBackups: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListBackupsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_TriggerBackup: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1TriggerBackupRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TriggerBackupResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_GetIngressStatus: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetIngressStatusResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_ListNodes: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListNodesResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_GetJoinGuide: {
        parameters: {
            query?: {
                /**
                 * @description worker 节点的公网 IP（防火墙规则按它生成精确放行文本；空 = 输出
                 *     规则模板、IP 位以 <worker-ip> 占位）。
                 */
                worker_ip?: string;
                /**
                 * @description manager 可达地址覆盖（advertise 为私网而 worker 跨公网的场景；空 =
                 *     取 swarm advertise addr）。
                 */
                manager_addr?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetJoinGuideResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_RotateJoinToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1RotateJoinTokenRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RotateJoinTokenResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_Ping: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1PingResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_GetRegistrySettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetRegistrySettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_UpdateRegistrySettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1UpdateRegistrySettingsRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateRegistrySettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_GetS3Settings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetS3SettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_UpdateS3Settings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1UpdateS3SettingsRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateS3SettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_TestS3Connection: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1TestS3ConnectionRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TestS3ConnectionResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SystemService_GetSystemStatus: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetSystemStatusResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    PlacementService_ShowPlacement: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ShowPlacementResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    PlacementService_UpdatePlacement: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["PlacementServiceUpdatePlacementBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdatePlacementResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    PlacementService_GetPlacementMigrationPlan: {
        parameters: {
            query?: {
                /** @description 目标节点（唯一显示名或平台 ID）。 */
                to?: string;
            };
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetPlacementMigrationPlanResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    PlacementService_ListVolumes: {
        parameters: {
            query?: {
                /** @description 状态过滤（active|orphaned|discarded；空 = 输出全部）。 */
                status?: string;
                /** @description 残留过滤（true = 只输出 residual 行）。 */
                residual?: boolean;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListVolumesResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    CronService_ListCronRuns: {
        parameters: {
            query?: {
                /** @description 收窄到单 schedule（空 = 全部）。 */
                service?: string;
                /** @description 行数上限（缺省 20；≤20 的量级面，无分页游标）。 */
                limit?: number;
            };
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListCronRunsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    CronService_TriggerCronRun: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 应用名（compose 应用名）。 */
                app: string;
                /** @description compose 服务名（必须声明 fleetly.cron，否则 404 语义）。 */
                service: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["CronServiceTriggerCronRunBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TriggerCronRunResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_ListDatabases: {
        parameters: {
            query?: {
                /** @description 行数上限（缺省 100）。 */
                limit?: number;
                /**
                 * @description 项目收窄（v0.3 W2-S4 可见性过滤，rbac-teams §4.2）：裸名或 `team/project`
                 *     限定形（D-W0-9 解析规则；解析域 = 调用方可见项目集，机具令牌/平台管理
                 *     员 = 全库）。空 = 不收窄（用户面仍按可见项目集过滤）。
                 */
                project?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListDatabasesResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_CreateDatabase: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1CreateDatabaseRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_GetDatabase: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_DeleteDatabase: {
        parameters: {
            query?: {
                /**
                 * @description 破坏性确认 = 实例名原样回传（mismatch → 400——与卷-节点 409 前哨同
                 *     型的数据安全面；引用 app 在册 → 409 E_DB_REFERENCED 先行）。
                 */
                confirm?: string;
                /** @description 删除数据卷（默认 false = 保留转 orphaned；true = 不可逆删除）。 */
                delete_volumes?: boolean;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DeleteDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_ListDatabaseBackups: {
        parameters: {
            query?: {
                /** @description 行数上限（缺省 20）。 */
                limit?: number;
            };
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListDatabaseBackupsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_TriggerDatabaseBackup: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceTriggerDatabaseBackupBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TriggerDatabaseBackupResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_RevealDatabaseCredentials: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RevealDatabaseCredentialsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_RestoreDatabaseBackup: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceRestoreDatabaseBackupBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RestoreDatabaseBackupResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_ResumeDatabase: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceResumeDatabaseBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ResumeDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_RetryDatabase: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceRetryDatabaseBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RetryDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_RotateDatabaseCredentials: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceRotateDatabaseCredentialsBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RotateDatabaseCredentialsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_UpdateDatabaseSettings: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceUpdateDatabaseSettingsBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateDatabaseSettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_SuspendDatabase: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceSuspendDatabaseBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SuspendDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    DatabaseService_UpgradeDatabase: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                name: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["DatabaseServiceUpgradeDatabaseBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpgradeDatabaseResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SecretsService_ListSecrets: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListSecretsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SecretsService_SetSecret: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 归属 app 名。 */
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["SecretsServiceSetSecretBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetSecretResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    SecretsService_RemoveSecret: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RemoveSecretResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ConfigsService_ListConfigs: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListConfigsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ConfigsService_SetConfig: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description 归属 app 名（或限定形/id）。 */
                app: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ConfigsServiceSetConfigBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1SetConfigResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ConfigsService_GetConfig: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetConfigResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ConfigsService_RemoveConfig: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app: string;
                name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RemoveConfigResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_ListWebhookDeliveries: {
        parameters: {
            query?: {
                /** @description 按端点过滤（空 = 全部端点）。 */
                endpoint_id?: string;
                /** @description 按状态过滤（pending | ok | failed；空 = 全部）。 */
                status?: string;
                /** @description 返回行数上限（缺省 50，天花板 500）。 */
                limit?: number;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListWebhookDeliveriesResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_ListWebhookEndpoints: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ListWebhookEndpointsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_CreateWebhookEndpoint: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1CreateWebhookEndpointRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateWebhookEndpointResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_GetWebhookEndpoint: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetWebhookEndpointResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_UpdateWebhookEndpoint: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationsServiceUpdateWebhookEndpointBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateWebhookEndpointResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_DeleteWebhookEndpoint: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1DeleteWebhookEndpointResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_RotateWebhookSecret: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationsServiceRotateWebhookSecretBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1RotateWebhookSecretResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_TestWebhook: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                id: string;
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationsServiceTestWebhookBody"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TestWebhookResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_GetSmtpSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetSmtpSettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_UpdateSmtpSettings: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1UpdateSmtpSettingsRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1UpdateSmtpSettingsResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    NotificationsService_TestSmtp: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1TestSmtpRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1TestSmtpResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ExecService_GetTerminalStatus: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1GetTerminalStatusResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
    ExecService_CreateTerminalTicket: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["v1CreateTerminalTicketRequest"];
            };
        };
        responses: {
            /** @description A successful response. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1CreateTerminalTicketResponse"];
                };
            };
            /** @description An unexpected error response. */
            default: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["v1ErrorResponse"];
                };
            };
        };
    };
}

