# 架构评审机制缺口整改批(IMPL-ARCH-A/B/C1/C2/D)

| 状态 | 日期 | 关联 |
|---|---|---|
| 已实现(五票,2026-09-28) | 2026-09-28 | 2026-09-28 架构评审(三路走查报告,临时 HTML 未入仓;候选编号本文沿用)→ 机制缺口排查 → rethink 复查(两修正五补充);还原点 **28ad020**(A)/ **1ead912**(B)/ **46dc3da**(C1)/ **dc88c00**(C2)/ **c266e91**(D) |

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

## 3. 守卫清单与验收句

| 守卫 | 层 | 验收句(下一个同类问题在哪被拦) |
|---|---|---|
| TestNoHandWrittenAppLabelFilters | 静态(源码扫描) | 手写 `fleetly.app` 归属过滤 → CI 红 |
| TestNoConvergenceOutsidePrimitive | 静态(源码扫描) | 绕开收敛原语直调底座写 → CI 红 |
| TestOwnershipAnchorConstantsCoveredByPredicate(+recon 级枚举) | 静态+测试 | 新锚声明不进谓词集 → 提交期红 |
| TestNoBareTaskStatusWrites | 静态(源码扫描) | tasks 表裸状态写 → CI 红 |
| TestMethodScopeRegistryCoversDescriptor | 测试(descriptor walk) | 新 RPC 漏登 scope → 第一次 go test 红且点名 |

共同效果:把「穷尽性」从提交者记性与评审者记忆搬进 CI 枚举守卫——同类问题从验收期(如 F1 于整线验收发现)前移到提交期。

## 4. 遗留挂账(评审候选未落部分)

| 候选 | 内容 | 状态 |
|---|---|---|
| 1 | 对账「披露一次」骨架收敛为 disclosure module(评审**首推**;×10 站点/×8 seen 字段/×6 节拍门) | 未落,下一批首选 |
| 6b | 服务登记表收敛(scope proto option + ServiceRegistration 表) | 需 rbac-teams 红线显式裁决(fail-closed 登记制搬家) |
| 7 | 哨兵→信封映射表(errors.go 实装 2/95) | 未落 |
| 8 | logs 轮询 sink 参数化(第三 fork 收编) | 未落 |
| 9 | genproto/sdk/go 发布为版本化模块 + WaitBuild(torchwood vendored fork 收编) | 跨仓,Speculative,值得 ADR |
| 10 | 小票:errcode `Source` 字段 / rotate.go RotateJobName(前缀族落错,未修)/ dbtemplate engineTools | 未落 |
| ③类守卫 | docs 守卫表 TestXxx 名存在性扫描(限活跃 impl 档,防腐化需白名单) | 挂账 |

## 5. 验证

五道验收门均为一手取证:`go test ./... -count=1` 32 包全绿(每阶段独立跑);每张守卫测试红态演示(白名单外注入→红→撤除→绿);新测试独立 `-count=1` 重跑;diff 范围核对(阶段范围圈:engine → engine → state+engine → state → api 单测试文件);全局一致性检查(applyDesired W2-S3 决策未动 / GC 分支未动 / 导出签名零改动 / F1 既有守卫全绿)。
