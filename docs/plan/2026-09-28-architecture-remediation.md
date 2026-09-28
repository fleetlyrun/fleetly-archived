# 架构评审机制缺口整改批(IMPL-ARCH-A~M)

| 状态 | 日期 | 关联 |
|---|---|---|
| 已实现(十四票:A/B/C1/C2/D 2026-09-28;E~M 2026-09-29;含两项用户裁决 6b 搬家与候选 9 发布) | 2026-09-29 | 2026-09-28 架构评审(三路走查报告,临时 HTML 未入仓;候选编号本文沿用)→ 机制缺口排查 → rethink 复查(两修正五补充);还原点 **28ad020**(A)/ **1ead912**(B)/ **46dc3da**(C1)/ **dc88c00**(C2)/ **c266e91**(D)/ **211adf6**(E)/ **22915fa**(F)/ **5124337**(G)/ **148e689**(H)/ **f6ffef9**(I)/ **9aa71b1**(J)/ **fdb6004**(K)/ **a251e6c**(L)/ **ef41b64**(M);模块 tag **genproto/v0.1.0** 与 **sdk/go/v0.1.0**(本地已建,push 待用户);docs 记录 **45b05a1**(首批)/ **c7fc4db**(第二批) |

## 1. 背景与元类判定

2026-09-28 架构评审(三路 Explore 走查 + 两处现行缺陷人工复核)产出十张候选。机制缺口排查对其中缺陷簇回溯逃逸路径,判定**三个子类**(rethink 复查修正:不再笼统归为单一元类):

| 子类 | 证据 | 守卫方向 |
|---|---|---|
| ① 穷尽性靠人肉(契约更新需 N 处同步) | W2-S3(f154d49)漏 drift 读点;F1/T2-1 锚豁免虚报 | 枚举扫描守卫 + 谓词/过滤 module 单点 |
| ② 拷贝漂移(原语未共享,拷贝即丢细节) | 任务线哈希标戳丢失(收敛 switch 第二份);logs 第三 fork(未落) | 共享原语 + 调用点白名单 + 原语直接测试 |
| ③ 记录虚报(守卫表/实施记录声称的覆盖不存在) | T1-1 守卫表、T2-1 实施记录(F2 整改) | docs→code 交叉核对(挂账,见 §4) |

本批落地:缺陷簇全修 + 守卫五条(§3);评审候选映射:A=候选4、B=候选3、C1=候选5、C2=候选2(机制面)、D=候选6a(rethink 补充3 拆分的无裁决半张)。

## 2. 票据分述

### IMPL-ARCH-A(28ad020)漂移 extras 死腿 + 归属过滤 module 化

- **缺陷**:W2-S3 把 `fleetly.app` 值改三段限定形后,`computeAppDrift` extras 腿(drift.go:260-263)仍以裸 `app.Name` 精确过滤——真实集群恒空,且与 applyDesired 省略=删除作用域**矛盾**(漂移报干净、redeploy 却会删);drift_test 夹具同形裸名造数掩盖。
- **修法**:新增 `internal/engine/ownership.go` 归属单点(`qualifiedServiceFilter` 唯一字面量 / `appServiceFilter` / `oneShotJobService` 豁免谓词 / `scopeManagedServices`);computeAppDrift 改收 `state.App`;move/appdelete/engine 豁免段迁移;applyDesired 的 spec 自推导(W2-S3 重放安全决策)一字未动;substrate/logs.go 等语义不同站点盘点后留白名单。
- **守卫**:`TestNoHandWrittenAppLabelFilters` 源码扫描(CRLF 归一 + 白名单保鲜双向断言);`TestDriftExtrasBeyondDesiredSet` 三变体(修复前红)。

### IMPL-ARCH-B(1ead912)服务收敛原语单点化

- **缺陷**:收敛原语两份手写拷贝;任务线 `taskServiceSpec` 从不打 `LabelDesiredHash` ⇒ 读回哈希恒空 ⇒ 每拍无差别重申(ForceUpdate 恒不递增 ⇒ 零任务替换,代价=每拍 update 往返 + 辨别语义失效)。
- **修法**:新增 `internal/engine/converge.go`(`convergeServiceObserved` 批量入口 / `convergeService` 自查入口;标戳+缺失建+重申 switch 沉底;部署判据 force/deploymentID 经 opts);applyDesired 与任务线两路收编;升级兼容=存量任务服务首拍一次幂等重申,对账面零影响。
- **守卫**:`TestNoConvergenceOutsidePrimitive` 白名单扫描(scaleToZero/autoscaling/initjobs 三站点语义成立保留);幂等守卫 `TestTaskConvergenceRepeatBeatSkipsRewrite`(迁移前红)+ 原语直接测试 5 条。

### IMPL-ARCH-C1(46dc3da)归属锚谓词住进声明地

- **缺陷**:锚契约声明在 state/labels.go、兑现散在 reconNetworks 豁免链 + GC 分支 + 三处注释(F1 即「契约补了、兑现没跟上」)。
- **修法**:`ownershipAnchorLabels` 集合 + `IsOwnershipAnchor` 谓词 + `OwnershipAnchorLabels()` 枚举读口与锚常量同文件相邻;reconNetworks 豁免链合并为一次调用;**GC 分支行为保持**(回收门槛只认 `LabelProjectNetwork` 子集——task-group 网无 state 行,进回收环即误删;全集/子集关系写死谓词注释)。
- **守卫**:`TestOwnershipAnchorConstantsCoveredByPredicate` 源码解析双向钉死(加锚不进集合即红=F1 复发形态提交期拦截);recon 级 `TestNetworkReconExemptsEveryOwnershipAnchor`(新锚自动覆盖);F1 既有守卫双测重跑 PASS。

### IMPL-ARCH-C2(dc88c00)task 转移表 + 单写点

- **缺陷**:T2-1 把部署线「CAS+转移表+裸写拒收(M3-2)」复制成 6 个手搓 InTx 块,转移图只活在 WHERE 并集,无 `CanTransitionTask`。
- **修法**(行为保持):machine.go `taskTransitions` 表 + `CanTransitionTask`(自旧谓词忠实提取);tasks.go `updateTask` 单点内核(PrevStatus 缺即拒 / CAS 前查表 / 幂等语义保持),六函数塌缩为薄包装,导出签名零改动;RequestTaskDelete 顺带补上旧裸 UPDATE 缺失的 CAS(仅并发竞争路径更强);已识别微差异三条如实挂账(sqlite 不可达路径)。
- **守卫**:`TestNoBareTaskStatusWrites` SQL 片段扫描(白名单=单写点本体);`TestTaskTransitionMatrixExhaustive` 6×6 穷举(实施中曾抓出表提取错误=活性对照)。

### IMPL-ARCH-D(c266e91)RPC scope 登记完整性

- **缺陷**:methodScopes 漏登史(MoveApp/MoveDatabase)= fail-closed 静默降级 admin-only,不可见,无兜底;E2 MCP 将新增成打 RPC。
- **修法**:纯测试票零生产改动。`TestMethodScopeRegistryCoversDescriptor`:descriptor walk(28 文件/158 方法,零手抄清单)四向完备——缺登记红 / methodScopes 幻影红 / 豁免幻影红 / 豁免清单≡authExemptMethods 双向相等;豁免 4 条各带理由;盘点结论=现状零出入(154+4)。

## 2b. 第二批(E/F/G/H,2026-09-29,按「按顺序继续」推进;候选 6b 后半 J 见 §2c)

### IMPL-ARCH-E(211adf6)对账「披露一次」骨架收敛(候选 1,评审首推)

- **缺陷**:披露 idiom(只报一次/恢复清零/事件+审计同事务/节拍门)手写约十遍:8 个 xxxSeen 裸 map 各带逐字近同契约散文、六处散布 delete 清零环、report* 事务对 10 处跨 6 文件、6 个 xxxNextAt 门;T 线 tasks face 整套再抄,F1 整改被迫在两处各动一刀。
- **修法**:`internal/engine/disclosure.go` 包内单点——`disclosureSet`(reported/mark/clear/sweep,零值可用,契约散文全引擎一份)+ `writeDisclosure`/`discloseTx`/`discloseOnce`(事件先审计后同事务配对,事务失败不标记下拍重试)+ `scanGate`(到期判定+推进一份逻辑,5/6 门收编)。逐面裁决:recoveryNextAt **不并入**(M1-8 臂闸语义,arm 时推进读时不推进,与读时到期即推进相反);state/janitor.go staleSeen 跨包挂账。
- **证据**:事件码/审计 action 零变化(17 个点分字符串 HEAD↔工作树逐一计数相等);`TestDisclosureModuleContract` 五场景;既有对账测试断言零修改逐个点名重跑。

### IMPL-ARCH-F(22915fa)三小票(候选 10)

- **F1 errcode/eventcode 单一真源化**:Code/Event 加 `Source` 出处字段(逐字机械迁移);docCodes/docEvents 手抄 map(89+105 条)删除改注册表投影;硬编码计数断言删除;golden 再生且逐行核对 append-only;码值/只增零变化;第四份码清单盘点=不存在。
- **F2 rotate 命名归族**:rotate.go 三处手搓名改经既有 `naming.DBJobName`(fleetly-dbjob-* 族=一次性 DB 作业防 sweep/对账误伤的家;红→绿);`qualifiedOf` 删除改 `inst.QualifiedName()`;消费面盘点=旧形态零逻辑依赖。
- **F3 dbtemplate engineTools 表**:先核 managed-databases §2.6(钉接口逐字+行为/脚本语义,不钉内部分派形状⇒方案 a 相容);7 处引擎 switch 中 6 处转 `map[Engine]engineTools`(pgToolDir 保留,真轴=Distribution×Major);`TestEngineToolsTableConsistency`(表⇆注册表双向覆盖+四件套非空)。

### IMPL-ARCH-G(5124337)哨兵→信封映射表(候选 7)

- **缺陷**:errors.go 自称唯一登记点实装 2/94,tasks.go 对 ErrTaskNotFound 同哨兵映射三处;每调用点私有知识=哨兵→状态码→信封助手→文案。
- **修法**:`storeErrTable` 46 行登记(五形态 kind;contexts 三值源)+ 单点入口 `mapStoreErr(err, args...)`(签名偏差披露并批准:函数式映射点调用站本不传文案,format 留调用站则无法收敛);58 站点迁表、17 站点保留特例逐站注记(同哨兵异语义/E_APP_AMBIGUOUS 族/额外载荷/控制流);删局部映射函数 7 个;文案逐字由表测试硬编码期望钉住,既有 handler 测试断言零修改;红态演示(改错一行映射,既有测试与表测试同时红)。
- **遗留**:ErrTaskNotFound 无 handler 级查无 404 直打;E_APP_AMBIGUOUS 族 6 站文案三形统一需产品裁决。

### IMPL-ARCH-H(148e689)轮询循环骨架收编(候选 8)

- **缺陷**:internal/logs 三份循环;pollTaskStream 逐行重抄 pollStreamNamed(唯一真差异=sink 硬编循环体内没能成为参数)+ evict 孪生。
- **修法**:`pollStream(ctx, streamPoll)` 唯一骨架 + `streamSink` 三钩子(deliver/openFailed/watchdogFired);appSink(redact→ring→VL/盘)与 taskSink(VL 直推)两适配;**jsonl 不收任务日志的诚实边界留在 taskSink**,不外泄成循环参数;`evictCursors` 收编孪生;键构造不同构保留;行为保持对照十点逐字核对;唯一微差披露=task get-or-init 多一次纯读 clock()。
- **新发现挂账**:`evictStaleStreamState` 按 cur.app 对账,task 游标 app="" 永不在 active 集,连续运行超 5min 后存活任务游标被 app 级淘汰误收(下轮从零重读,重复优于丢失,幂等;建议后续小票豁免);第四号循环 pollAccess 游标为独立标量不同构,保留,接缝已预留。

## 2c. 候选 6b 收官(J,2026-09-28;前半 I 见 f6ffef9)

### IMPL-ARCH-J(候选 6b 后半;前半 I 服务登记表见 f6ffef9)scope 登记面搬家:proto option 唯一源

- **裁决**:用户批准搬家——原红线「scope.go fail-closed 登记制」的登记面改为 **proto option**(每 RPC `option (fleetly.annotations.v1.scope) = "<词>"`,定义在 proto/fleetly/annotations/v1/annotations.proto,MethodOptions 字段号 50000 组织保留区间);proto 成为登记的唯一人类编辑点。**搬的是「登记写在哪」,不是「怎么鉴权」**:fail-closed 兜底、豁免四条(住 auth.go authExemptMethods,不进 proto)、拦截器/RequiredScope 耦合零改动。
- **修法**:27 个 server proto 的 154 个方法逐条迁注解(一次性迁移工具 + descriptor 全集 ⇆ 旧表零 diff 校验,值零变化后工具即删);scope.go 手工表删除,init 期从 protoregistry 生成同形态 map,注解词 ∉ scopeWords 词表即 panic fail-fast(启动期爆,不静默成永不可达端点)。
- **守卫**:scope_completeness_test.go 四向适配——缺注解红 / 注解词词表外红 / authExemptMethods 幻影红 / 测试豁免清单 ≡ 生产豁免表双向;红态两方向演示过(摘注解点名方法;改词表外值先 init panic 再测试方向红)。rbac-teams 红线表述随票修订。

## 2d. 候选 9 收官(K,2026-09-28;跨仓票的仓内侧)

### IMPL-ARCH-K(候选 9)genproto/sdk 发布为版本化模块 + WaitBuild 收进 SDK

- **裁决**:用户批准。**模块发布面**:sdk/go/go.mod 对 genproto 的 require 从零伪版本(`v0.0.0-00010101000000-000000000000`,proxy 上不存在)改为真实版本号 `v0.1.0`;replace 行保留(GOWORK=off 场景的仓内密闭构建兜底,replace 仅主模块生效、对外无害)——零伪版本 + replace 被外部消费者忽略即 T2-3(torchwood)被迫 vendored fork 的机械根因。版本号在 tag push(`git tag genproto/v0.1.0` 与 `git tag sdk/go/v0.1.0`,子目录模块 tag 必带目录前缀)后对外可解析;仓内构建全走 go.work workspace,不依赖 tag 存在。
- **WaitBuild 收编**(候选 9 后半):SDK 新增 `Client.WaitBuild(ctx, buildIDs, ...WaitOption)`(轮询 GetBuild 至全部终态;`WithWaitTimeout/WithWaitPollInterval/WithWaitOnTerminal`;超时返回 `*WaitTimeoutError`(哨兵 `ErrWaitTimeout`,Pending 快照携带各构建最后已知状态))+ 终态谓词单点 `BuildTerminal(status string)`(queued/building 非终态,succeeded/failed 终态,词表外按终态——与收编前 CLI 环一致);错误语义:终态前 GetBuild 报错即中止原样上抛、终态后不再轮询、ctx 结束返回 `ctx.Err()` 本尊。CLI build 命令的等待环收编消费(既有逐构建行输出经终态回调保持、超时两分提示措辞逐字不变)。
- **服务端不共用**(裁决留痕):BuildFromUpload 的服务端等待(internal/api/builds.go `waitUploadBuild`)占 gRPC 流、直读 state.Store(非 gRPC GetBuild)、谓词消费类型化常量——机制不同构;且 internal 包不可反向 import 公共 SDK(依赖方向倒置)。SDK 侧 wait.go 注释指认此取舍;两侧词表同源 proto BuildView.status,漂移由 proto 契约面兜底。
- **根 go.mod 记录**(保持原样):对 genproto 同款 replace + 零伪版本(无外部消费者,离线 replace 兜底足够);对 sdk/go 挂真实伪版本 `v0.0.0-20260920151618-0ea0ebf6cf4a` 且**无 replace**(workspace 构建不受影响;GOWORK=off 场景仅 go generate 不解析导入,现状可用)——tag push 后可择机升 `v0.1.0`,挂账见 §4。
- **跨仓待办**(不属本仓改动):tag push 后 torchwood 侧改 import `github.com/fleetlyrun/fleetly/sdk/go@v0.1.0` 并删除 vendored fork;CI 核查结论:pr.yml 全部 Go job 为 workspace 形态或 sdk/go 目录内 GOWORK=off(replace 生效),无 `go mod tidy -diff` 门,本变更零 CI 风险。

## 2e. 裁决三/四落地(L/M,2026-09-29;6b 前半 I 见 f6ffef9)

### IMPL-ARCH-I(f6ffef9)服务登记表收敛(候选 6b 前半)

- **裁决**:用户批准搬家(2026-09-29)。
- **修法**:internal/runtime/registration.go 唯一登记表(27 条,泛型编译期对型 + gateway 注册器可空=Cron/Tasks 显式 gRPC-only;表序=原 grpc.go 注册序快照)被 grpc/gateway/apitest 三消费者共用;NewGRPCServer 签名零改动(wire/D20 不动);auth.go scopeWords 词表单点(词→蕴含集/排序,admin impliesAll 开放集语义逐字);五份清单盘点:harness_test 小清单保留(鉴权矩阵测试的范围选择,不同构)。
- **收编即抓真漂移**:apitest 原漏登 CronService/ExecService——表对账要求全集补齐为主会话追认的降级装配(零既有测试经 apitest 调这两面)。
- **守卫**:registration_test 三条(反射参数数≡表条目/AST 装配键集⇆表双向/表外 Register* 引用源码扫描)+ auth_scope_words_test 三条(表⇆常量双向/蕴含形状/排序投影);红态四向演示过。

### IMPL-ARCH-L(a251e6c)E_APP_AMBIGUOUS 六站文案统一(裁决三)

- **裁决**:按建议以 ownership.resolveApp 为基准统一。
- **修法**:errors.go 唯一构造函数 `ambiguousRefErr(kindName, qualifiedForm, ref, candidates)`,标准句取基准原文参数化(app/database 两族);kindName 兼任信封 context 键(六站既有键逐一不变);状态码/注册码/投影零变化;既有断言零改动(全仓唯一钉 detail 的 drift_test 子串断言原样保留)。
- **守卫**:`TestAmbiguousRefErrEnvelope` + `TestNoStrayAppAmbiguousLiterals`(AST 扫字面量,白名单=构造函数双向钉死)。
- **微差披露**:基准站候选列 context 恒投影,现构造空列不投影(可达路径候选恒非空,仅读故障边缘少一个空键);库族候选列现传 nil(state 无按名列库原语,挂账 §4)。

### IMPL-ARCH-M(ef41b64)记录虚报守卫:活跃 impl 档 TestXxx 存在性扫描(裁决四)

- **裁决**:按建议方案 a——只扫活跃波次,防误报优先(误报的代价是守卫被禁用)。
- **修法**:`TestActiveImplDocsReferenceExistingTests`(纯测试零生产改动):活跃判据=最新日期档锚回看 3 自然日窗(锚定目录状态不随日历空转;波次超窗滚出=有意漏报,注释写明);五条机械跳过规则(族形通配/-run 模式位/勘误除名披露行/外仓票据小节/围栏代码块)+防空转 Fatalf+白名单双向保鲜(「合法缺席」语义非豁免一切)。
- **首功**:上岗即抓到三处真坏引用——F1(22915fa)删除 TestRegisteredCountByKind 后,T1-4/T2-1/T2-2 实施记录仍以现行守卫口吻引用=③类记录虚报,就地加勘误标注(未入白名单);342 原始引用/跳过 104/现行主张 238 逐一核验全实存。
- **守卫自净预告**:活跃波次收口、新档超窗后当前档案滚出,白名单条目会被保鲜检查点名清除(机制设计使然)。

## 3. 守卫清单与验收句

| 守卫 | 层 | 验收句(下一个同类问题在哪被拦) |
|---|---|---|
| TestNoHandWrittenAppLabelFilters | 静态(源码扫描) | 手写 `fleetly.app` 归属过滤 → CI 红 |
| TestNoConvergenceOutsidePrimitive | 静态(源码扫描) | 绕开收敛原语直调底座写 → CI 红 |
| TestOwnershipAnchorConstantsCoveredByPredicate(+recon 级枚举) | 静态+测试 | 新锚声明不进谓词集 → 提交期红 |
| TestNoBareTaskStatusWrites | 静态(源码扫描) | tasks 表裸状态写 → CI 红 |
| TestMethodScopeRegistryCoversDescriptor | 测试(descriptor walk) | 新 RPC 漏登 scope → 第一次 go test 红且点名 |
| TestDisclosureModuleContract + TestScanGateDueAndAdvance | 测试(module 契约) | 披露 module 的 once/sweep/事务配对语义漂移 → 契约测试红 |
| TestEngineToolsTableConsistency | 测试(表一致性) | 新引擎条目缺件/幻影 → 表测试红 |
| TestStoreErrTable* 等六守卫 | 测试(表投影) | 映射行状态码/注册码改错 → 既有 handler 测试+表测试同时红 |
| TestPollStreamSharedSkeletonServesBothFamilies | 测试(共路) | task sink 误接 ring/循环分叉 → 共路测试红 |
| registration_test 三条 + auth_scope_words_test 三条 | 静态(AST/反射/源码扫描) | 表外 Register* 引用/装配键错名/词表漏行 → 提交期红 |
| TestNoStrayAppAmbiguousLiterals | 静态(AST) | E_APP_AMBIGUOUS 字面量逃出构造函数 → CI 红 |
| TestActiveImplDocsReferenceExistingTests | 测试(docs 扫描) | 实施记录引用不存在的测试名 → 提交期红(首日即抓到 F1 删除残留三处) |

共同效果:把「穷尽性」从提交者记性与评审者记忆搬进 CI 枚举守卫——同类问题从验收期(如 F1 于整线验收发现)前移到提交期。

## 4. 遗留挂账

| 候选/项 | 内容 | 状态 |
|---|---|---|
| 9 外发 | tag push(genproto/v0.1.0、sdk/go/v0.1.0,本地已建)+ torchwood 改 import 删 vendored + 根 go.mod 对 sdk/go 旧伪版本 pin 择机升 | 待用户执行/跨仓 pass |
| E 尾 | state/janitor.go 的 staleSeen/reportStale(披露骨架同款跨包拷贝) | 挂账,需跨包 helper 形态裁决 |
| F 尾 | eventcode 头注计数叙事停在 95、实注册 105(既有注释漂移) | 挂账,随下次事件增补重算 |
| F 尾 | database/move.go:67 手工复述三段限定形,疑似查询值与写入值不同形 | 挂账,需独立小票核查 |
| G 尾 | ErrTaskNotFound 无 handler 级查无 404 直打(仅表测试钉住) | 挂账,小票补测 |
| H 尾 | evictStaleStreamState 误收 task 游标(app="" 不在 active 集,幂等无害) | 挂账,小票豁免 |
| I 尾 | ownership.go:480 reachableScopeList 自持 scope 序清单 | 挂账,小票收编进 scopeWords 派生(注意是可达面子集,需过滤语义) |
| K 尾 | deploy.go 的部署等待环与旧 build wait 同构 | 挂账,可同样收编 SDK(另开小票) |
| L 尾 | 库族 E_APP_AMBIGUOUS 无候选列(state 无按名列库实例原语;errcode Suggestion 声称候选列在 error context) | 挂账,加 state 原语后一处接入 |
| L 尾 | resolveAppRefForMove/resolveDatabaseRefForMove 限定形解析同构 + resolveDatabaseRef 注释表述漂移 | 挂账,另票收编 |
| M 尾 | impldocscan 跳过规则是约定驱动:档案演化出新「非现行主张」写法需同步扩规则;勘误行上新幻影名会被放过(有意漏报,已声明) | 机制注记,非待办 |

## 5. 验证

十四道验收门均为一手取证:`go test ./... -count=1` 32 包全绿(每阶段独立跑);每张守卫/映射测试红态演示(白名单外注入/改错映射行/误接 sink/注入幻影测试名 → 红 → 撤除 → 绿);新测试独立 `-count=1` 重跑;diff 范围核对;全局一致性检查(applyDesired W2-S3 决策未动 / GC 分支未动 / 导出签名零改动 / F1 既有守卫全绿 / 事件码与 scope 词零变化 / 信封文案逐字保持 / methodScopes 运行时行为零变化=鉴权矩阵全绿)。
