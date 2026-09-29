# Console UI 重设计:信息架构 + 设计语言(2026-09-29)

状态:已裁决并随本提交落地(部分条目挂账,见 §8)。
参考物:`D:/tmp/dokploy-snapshots`(dokploy v0.30.6/v0.30.7,22 张截图,2026-09-29 用户截取);
前序:`docs/reports/2026-09-25-console-ui-review.md`(18 项对照矩阵与 backlog 大部分已落地)。

## 1. 背景与问题

用户反馈(2026-09-29):「Console UI 感觉很乱,而且很多必要的信息并没有显示,比如实际生效的
compose 文件、没有显示实际运行的 container 等等」。

代码事实核对(两个缺口都属实):

1. **实际生效的 compose 文件不可见**。`GetRevisionSpec`(`GET /v1/apps/{app}/revisions/{id}/spec`)
   返回归一化 compose 快照(canonical JSON,env 为 key:sha256),但 Console 只把它解析成
   服务/cron 清单与字段级 diff,**从不渲染原文**——用户部署的 compose 在平台里长什么样,
   UI 没有任何入口能看到。
2. **实际运行的容器/任务不可见**。引擎底座端口 `Substrate.TaskList(ctx, service)` 与
   `ServiceList` 只在引擎观测循环内部消费,**没有上抛 REST**。应用详情里最近似的是
   drift 判定(受管字段投影)与 opt-in metrics 曲线;Services 卡的 Replicas 是
   **声明值**(compose `deploy.replicas`),不是实际副本水位。

「乱」的定性:应用详情 Overview 堆了 10 张卡(Application/项目网/Placement/Drift/Services/
Volumes/Cron/Metrics/Scaling/Danger),运营真相(实际在跑什么)与配置面(扩缩/项目网/危险区)
混在一屏;页签顺序把 Deployments 放在第二位而运行态无处可看。

## 2. dokploy 参考物要点(截图归纳)

**应用详情(compose 服务页)**:面包屑(项目>环境>服务,可切换)+ 标题行(图标+名+服务 id,
右上服务器徽章/编辑/删除)+ **12 页签**:General / Environment / Domains / Deployments /
**Containers** / Backups / Schedules / Volume Backups / Logs / Patches / Monitoring / Advanced。

- **General**:Deploy Settings 卡(主行动 Deploy+Stop+Open Terminal+Autodeploy 开关)
  + Provider 卡(拉源)+ Preview Compose 入口。
- **Containers**(用户点名要的能力):实况表——Name / State(running 徽章)/
  Status("Up 20 hours (healthy)")/ Container ID / 行动菜单;描述句
  "Inspect each container in this compose and run basic lifecycle actions"。
- **Logs**:先选容器(下拉含状态摘要),再 Limit/Time range/Log type + Pause/Copy/Download。
- **Monitoring**:同样先选容器,CPU/Memory/Block IO/Network IO 四卡。
- **Deployments**:编号列表(1. Done 圆点)+ commit 标题 + 描述展开 + 时长 + View/Delete。
- **平台级 Docker 页**:8 页签 Containers/Swarm/Images/Volumes/Networks/Events/Disk Usage/Health
  ——把底座实况(全量容器表/按节点分组/镜像台账/daemon 事件/磁盘占用/诊断)做成一等页面。

**设计语言**:单一卡片范式(圆角卡+图标+标题+一句描述+右上操作区)、空态范式(居中大图标
+一句结论+主 CTA)、状态徽章(彩色 pill)、表格为主的信息密度、top-level 行动用黑色主按钮。

## 3. 域对照(dokploy 形态 → fleetly 形态)

| dokploy | fleetly 对应 | 裁决 |
| --- | --- | --- |
| container(per-compose) | swarm task(per-service;running task ≈ 容器实例) | UI 面向用户仍叫 **Containers**,行=任务;表列出 Service/Slot/State/Desired/Image/Updated/Error |
| Preview Compose(源码 compose) | 归一化快照(canonical JSON) | 渲染**实际生效快照**,文案如实注明 env 只有 key:hash;Overview 卡 + Deployments 每行 |
| General 页签 | Overview | 概览=运营摘要(状态/服务水位/放置/卷/cron/metrics) |
| Backups/Schedules/Volume Backups/Patches | — | 不引入(fleetly 的备份在 System/DB 面,cron 已有台账;Patches 无对应域) |
| 平台级 Docker 页 | System 页已有 nodes/ingress | 全平台容器/镜像台账**不引入**(v0.4 挂账,§8) |

## 4. 信息架构裁决

### 4.1 应用详情页签(重排 + 新增 Containers)

```
Overview │ Containers │ Deployments │ Builds │ Logs │ Env │ Secrets │ Configs │ Domains │ Terminal
```

- **Containers 紧随 Overview**(运行真相第二优先,对齐 dokploy 把 Containers 放中部显眼位);
  Deployments/Builds 是变更史,顺延。
- Env/Secrets/Configs 是配置三族,保持相邻;Terminal 殿后(工具位)。
- 路由:`/apps/:ref/containers`(新增);其余不变。

### 4.2 Overview 运营摘要化(去「乱」的主刀)

上部三张 StatCard:**Derived state**(徽章)/ **Services**(running x / declared y,来自
runtime 面)/ **Tasks**(running n)。Application 卡收编 ID/lifecycle/times;以下分区保留但
按「运营真相 → 配置面 → 危险面」排序:Application(+项目网)/ Placement / Services(声明
清单,叠加实际水位)/ Volumes / **Compose 卡(新)** / Cron / Metrics / Scaling / Danger Zone。

### 4.3 Containers 页 = 运行真相页

- **Runtime 卡**:per-service 小结(服务名+running/declared 徽章+update state)+ 全量任务表
  (Service/Slot/State/Desired/Image/Updated/Error),5s 轮询(与详情头 getApp 同拍)。
- **Drift 卡从 Overview 迁入本页**:「实况 vs 期望」的 reconciliation 语义与任务实况同域
  ——Containers 页成为回答「现在到底跑的是什么、对不对」的唯一入口。
- 空态:无任务时 EmptyState(Container 图标+「No tasks」+指引)。

### 4.4 Compose 可视化(实际生效的 compose 文件)

- **Overview「Compose」卡**:active revision 的归一化快照渲染为代码块(等宽、可折叠、
  Copy 按钮),标题注明 revision seq;无 active revision 时 EmptyState 指引去 Deployments。
  如实文案:env 值以 SHA-256 哈希存储(值明文结构性不在快照中,平台无明文)。
- **Deployments 每行新增「Compose」按钮**:打开对话框渲染该部署 revision 的快照
  ——历史每一版部署的实际 compose 都可回看(与 What changed 并列)。

### 4.5 全站导航(不动)

侧边栏五组(Home/工作区/Workloads/Platform/Administration)是 2026-09-25 走查裁决形态,
本次不动。平台级 Docker 实况页(全量容器/镜像台账)挂账 v0.4(§8)。

### 4.6 Deployments 页单方式部署源(2026-09-29 二次裁决)

用户复核裁决:「一般情况下,一个应用只需要一种部署方式,而不是所有的部署方式都列出来」。
此前页面把手动 compose(Deploy 卡)、回滚(Rollback 卡)、git push + webhook + 拉源
(Deploy triggers 卡)三卡平铺——全部方式同时列出,信息架构把「方式选择」的负担推给了
用户。重设计为 **单一 Deploy 卡 + 方式切换**:

- **方式 pills**(CardHeader 右侧,SectionCard actions 槽):`Compose │ Git │ Webhook`,
  一次只呈现当前方式的 pane;选择按应用持久化(localStorage
  `fleetly.console.deploy-method.<app>`),跨会话记忆该应用的工作方式。
- **可用方式 = 角色门投影**:Compose=deploy scope(developer+);Git/Webhook=admin
  scope(admin+,与 ShowAppWebhook 三 RPC 同门)——不可用的方式不出 pill;developer
  只见 Compose pill + 卡内说明行(triggers-admin-note 语义内移);平台管理员整卡换
  platform-readonly-note(P0-3 双门不变)。
- **pane 内容即原三卡内嵌**:Compose pane=粘贴/上传+Deploy+跟踪器(锚点
  `deployment-tracker`/`deploy-project-context-hint` 不变);Git pane=push 远端+触发
  分支+拉源表单;Webhook pane=接收端 URL+签名密钥+名字词形披露——全部触发面
  testid 原样保留(内移进 pane),webhook 读面查询只在 admin+ 且非 compose 方式时
  发起(developer 挂载不发注定 403 的请求)。
- **回滚收编为历史行内操作**:独立 Rollback 卡删除;带 revision 的行出 Rollback 钮
  (deploy scope),确认框承载语义(重放该行 revision 快照=一条新部署走正常发布
  管线),POST /rollbacks 载荷携带该行 `target_revision_id`。
- **页面终态 = Deploy 卡(单方式)+ Deployment history 表**——与 dokploy 的
  Deployments 页签(触发 URL+历史列表)同构,方式选择负担归零。

### 4.7 三轮:Overview/Deployments 对齐 dokploy General/Deployments(2026-09-29)

用户以 dokploy 真机截图(General 页签=Deploy Settings 快捷动作+Provider 源配置;
Deployments 页签=webhook URL+历史列表)提出「基本功能要对齐」。对齐面:

- **Overview = 部署入口**(对齐 dokploy General 的「打开应用即见怎么部署」):
  新增 **Deploy settings 快捷动作卡**(SectionCard,锚点 `deploy-settings`)——
  **Redeploy**=重放当前 active revision(POST /rollbacks 带
  `target_revision_id`,与历史行内回滚同管线;服务端把重放统一建模为
  kind=rollback 部署,历史如实呈现);**Open terminal**=Terminal 页签直达;
  无 active revision 时禁用+卡内指路 Deploy 卡;平台管理员 P0-3 只读说明由
  本卡承载。**Deploy 卡(单方式+方式 pills)自 Deployments 页上移随迁**为独立
  组件 `components/app-deploy-card.tsx`(全部触发面 testid 原样保留;TERMINAL
  词表/webhookReceiverUrl/CopyValueRow 单点导出复用)。
- **Deployments = 纯历史面**(对齐 dokploy Deployments):历史卡换 SectionCard;
  顶部 **Redeploy webhook URL 行**(admin+,锚点 `history-webhook-url`,值=
  接收端 URL 按业务名寻址——与 Webhook pane 同一单点拼装;读失败呈中性说明
  非 alert);**时长列**(dokploy 每行耗时徽章同构,锚点 `deployment-duration`)
  ——终态行=created_at→updated_at(state 层自注 updated_at 恒重盖、是「终态
  写入时刻的最近似代理」,internal/state/deployments.go;含排队等待,title
  如实说明口径),进行中行=已流逝时间(随轮询节拍刷新);**Cancel queued**
  (dokploy Cancel Queues 同构,锚点 `deployments-cancel-queued`)——非终态
  行计数入钮,确认框列明,逐条调 CancelDeployment,部分 409(已切流/终态,
  服务端取消受限语义)如实汇报成败计数不静默关框。
- **webhook 读面的 Me 就绪门**:挂载即拉的面只看 canAdminResources 会在能力
  门 fail-open 窗口(Me 未达)内发出注定 403 的请求——`useMeQuery` 导出,
  enabled/渲染双叠 `isSuccess` 门(useTeamCapabilities 头注口径的补丁)。
- **测试布局**:Deploy 卡 11 用例随迁至新文件 `AppOverviewPage.deploy.test.tsx`
  (路由 /apps/:name)+ Deploy settings 卡 5 新用例(Redeploy 载荷/无 revision
  禁用/失败信封/P0-3/viewer 退场);AppDeploymentsPage.test 重写(时长列/
  批量取消/webhook 行/developer 不发 webhook 读请求);AppOverviewPage.test
  P0-3 断言联动(readonly-note ≥4、装配守卫)。

**对齐差距挂账(后端无面,不做假)**:应用级 Stop/Start(仅 DB 有
Suspend/Resume、Task 有 Stop——引擎需要 suspended 语义,收敛循环须绕行);
per-app Autodeploy 开关(gitserver push/webhook 入队路径需要布尔位+RPC);
部署行 commit message(gitserver 需读 commit 对象落列);单条部署的构建日志
查看(BuildView 与 Deployment 无反向关联键——Builds 页头注已认);Clear
deployments/删单行(部署行兼审计与回滚基线,revisions 保留窗 5 次成功);
Fresh Volumes/Watch Paths/Submodules(dokploy 构建域概念,fleetly 拉源模型
无对应物)。

### 4.8 四轮:app Stop/Start/Redeploy + Applications/Databases 统一(2026-09-29)

用户裁决「增加 Stop/Start/Redeploy application 操作,UI/UX 参考 databases 的
标题栏按钮的方式,同时支持新建 application;统一 Applications 和 Databases 的
UI/UX」。此前 §4.7 挂账的「应用级 Stop/Start」本轮落地:

- **后端(app Stop/Start 原语,照抄 DB paused 的「权威态列 + 状态机单写点 +
  收敛按态渲染」形态)**:apps 行加法列 `suspended`(00028;ifecycle 词表
  三态不动);state 写点 `SetAppSuspended`(CAS + tombstone 守卫 +
  app.suspended/app.resumed 事件同事务 Outbox;并发翻转哨兵
  ErrAppSuspendedConflict → 注册码 E_APP_SUSPEND_CONFLICT 409);引擎排水
  保持器 `drainSuspendedApp`(suspend.go,convergescan 白名单行——scaleToZero
  同族的无条件副本清零,非收敛对账):周期对账把受管长驻 replicated 服务
  副本压 0(服务对象保留,缺失不补建、多余不回收、global 无副本标量如实
  不达),拍尾刷新派生基线;派生词表增 `suspended`(第一判短路,用户权威位
  直投影,压倒观察态——DB paused 同型);drift 扫描/autoscaler/cron 调度
  挂起期按位豁免(0 副本不是漂移,opt-in 也不得把副本收敛回快照——否则
  挂起被静默撤销);**部署入队门 E_APP_SUSPENDED**(EnqueueRollback/api
  Deploy/gitserver push/webhook 单点各一道——挂起期排水会立即拉回 0,新
  部署不入队;resume 在清位后走入队不被拦);cron 手动触发同门 409。
- **ResumeApp = 清位 + active revision 重部署**(EnqueueRollback 空目标 =
  保留窗最新成功版本,正常发布管线恢复副本——与应用内 Redeploy/行内回滚
  同管线;无成功部署 = 无物可恢复,deployment_id 留空且清位照常生效)。
  RPC:SuspendApp/ResumeApp(POST /apps/{name}/suspend|resume,admin scope
  ——与 SuspendDatabase 同级,整应用停摆是大爆炸半径动作)。
- **前端:详情标题栏动作行**(AppHeaderActions,databases 标题栏同款——
  右对齐 outline 小按钮排 + 状态相宜启停 + title 提示 + 即发即走):
  Redeploy(deploy 面)/Stop|Start(admin 面,挂起位翻转 Stop↔Start,
  挂起期 Redeploy 禁用)/动作错误信封原位;角色门外按钮不投影(平台管理
  员 P0-3 全隐,说明态在 Deploy settings 卡);§4.7 的 Deploy settings 卡
  收敛为次级入口(Open terminal + 挂起说明 + P0-3 说明,Redeploy 移交标题
  栏);StateBadge 增 suspended 琥珀档。
- **列表页统一**:两页共享同一骨架(PageHeader〔title+desc+Refresh+Create〕
  → 统计卡行 → 工具栏 → 表格卡)。Applications 页补齐:统计卡(总数/运行/
  挂起/需注意)+ Create application 钮(deploy 面;CreateAppDialog 复用,
  目标项目 = 顶栏选中项目限定形)+ P0-3 说明卡(与 Databases 页同款)。
  Databases 页补齐:搜索/状态筛选/排序工具栏(客户端投影,Apps 页同款)。

**§4.7 挂账项更新**:应用级 Stop/Start 本轮落地(上列);其余挂账
(Autodeploy 开关/commit message/单条部署日志/Clear deployments)不变。

## 5. 后端新读面:RuntimeService

引擎端口 `Substrate.TaskList/ServiceList` 已存在(`internal/engine/ports.go:298-317`),
缺的是上抛。新增面沿 DriftService 先例(独立 service:运行实况是跨 deployments 快照与
swarm 实况的对账域,不属于任何资源行 CRUD):

- **proto** `fleetly/server/v1/runtime.proto`:`RuntimeService.ShowAppRuntime`
  → `GET /v1/apps/{app}/runtime`,scope=read。响应:
  `AppRuntimeView{app, desired_deployment, services[] ServiceRuntimeView{name, image, mode(replicated|global|cron), declared_replicas, actual_replicas, update_state, update_message, missing, tasks[] TaskView{id, slot, state, desired_state, error, image, timestamp}}}`。
- **engine** `AppRuntime(ctx, appName)`(internal/engine/runtime.go):`lastSucceededSpecs`
  (期望集+声明副本,活覆盖钉入)∪ `scopeManagedServices`(实况集,一次性 job 豁免);
  每服务 `TaskList`;期望集有而实况无 → `missing=true`(与 drift missing 同判);
  mode=cron 的期望服务不查任务(一次性 job,任务语义在 cron-runs 台账)。
- **api** `internal/api/runtime.go`:`resolveApp` + `requireAppAccess` + 引擎调用恒传
  `app.QualifiedName()`(DriftService 同款纪律,id 寻址语境不透传原始引用)。
- **登记**:registration.go 挂双面;grpc.go 装配集、provides.go 构造器、wire、apitest
  装配集四处同步(登记表双向对账缺一即红)。

**为什么不在 GetApp 里内联**:详情头 5s 轮询已承载 GetApp;任务表是独立轮询面
(5s),分开才能各按各的节拍,且 GetApp 响应形状不被拖重。

## 6. 设计语言规范(以本提交为基准)

1. **卡片范式**:统一 `SectionCard`(新组件)——Card + 图标+标题+可选一句描述+右上
   actions 槽 + border-b 头部;新面一律用它,存量卡片渐进收编(不一次性翻新)。
2. **状态色彩**:state-badge 的 stateTone 语义(绿 running/黄 intermediate/红 failed)
   是唯一状态色来源;新增面不得另造色。
3. **数据形态**:ID/镜像/哈希=font-mono text-xs;时间=相对时间+title 绝对值;
   声明值 vs 实况值必须成对出现(declared x · running y),不单报声明值冒充实况。
4. **空态**:EmptyState(图标+一句结论+可选 CTA);加载态骨架或中性占位,禁止把
   未加载渲染成确认关闭/空清单(W2-3 教训)。
5. **文案**:英文(报错/日志/展示),注释中文;如实披露(env 哈希/任务含历史)。

## 7. 测试锚点兼容

保留全部既有 data-testid(`state-badge`/`services-list`/`service-replicas`/
`service-scheduled-badge`/`deployment-row`/`deployment-tracker`/`cron-runs-list`/
`danger-zone`/`platform-readonly-note`/`app-row` 等)。新增锚点:`runtime-services`/
`runtime-task-row`/`runtime-service-card`/`compose-viewer`/`deployment-compose`。
Overview 测试的 fetch stub 对未知 URL 走 GetApp 兜底——Overview 上新增的 runtime 查询
必须容错缺 `services` 字段的响应(降级为声明值显示,不崩)。

## 8. 落地范围与挂账

**本提交落地**:RuntimeService 后端面 + Containers 页 + Overview 摘要化/Compose 卡 +
Deployments 每行 Compose + SectionCard 组件 + 页签重排;三轮(§4.7):Deploy settings
卡 + Deploy 卡上移 Overview + Deployments 历史化(时长列/批量取消/webhook URL 行)。

**挂账(不做)**:平台级 Docker 实况页(全量容器/镜像/daemon 事件,dokploy Docker 页
形态,v0.4 候选);Containers 页的行级生命周期动作(restart/scale——扩缩已有 Scaling 卡,
重启语义需引擎新原语);logs 按任务选择器(现 logs 面按 app,任务级过滤挂 E6 后续);
DatabaseDetailPage 的容器实况(库实例状态已有读面);§4.7 尾列的 dokploy 对齐差距
(app Stop/Start、Autodeploy 开关、部署行 commit message、单条部署日志、Clear
deployments——各有后端缺口注记)。
