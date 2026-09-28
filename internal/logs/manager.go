package logs

import (
	"bufio"
	"context"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

// Manager 主体：采集循环 / Follow / History / 清理（类型与构造在 logs.go）。

// stream 是采集循环内的 per (app, service) 轮询游标。
type stream struct {
	app     string
	appID   string
	service string
	lastAt  time.Time // 轮询起点游标（首轮 = 发现时刻；此后 = 末行时间）
}

// Run 是采集主循环（lynx 服务壳 Start 消费）：周期扫描 active apps 的
// 受管服务 → 逐流轮询 docker service logs（follow=false + since 游标）→
// 脱敏 → ring + 入湖/落盘 + 扇出。ctx 取消即返回。首轮前做一次落盘清理
//（读/prune 路径与后端无关——切换前的旧文件继续按保留窗老化，设计 §2.3
//「检索不跨界」的诚实边界）。W5-S1：入湖批量器 flush 循环随本循环拉起
//（同 ctx 退出）；logs.backend 门每扫描拍现读刷新。
func (m *Manager) Run(ctx context.Context) error {
	if m.ing != nil {
		go func() {
			if err := m.ing.Run(ctx); err != nil {
				m.log.Warn("logs: ingest flush loop exited", "error", err.Error())
			}
		}()
	}
	if _, err := m.dsk.prune(ctx, time.Duration(m.cfg.RetentionDays)*24*time.Hour); err != nil {
		m.log.Warn("logs: initial prune failed", "error", err.Error())
	}
	pruneTicker := time.NewTicker(time.Hour)
	defer pruneTicker.Stop()
	ticker := time.NewTicker(m.cfg.ScanInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pruneTicker.C:
			if n, err := m.dsk.prune(ctx, time.Duration(m.cfg.RetentionDays)*24*time.Hour); err != nil {
				m.log.Warn("logs: prune failed", "error", err.Error())
			} else if n > 0 {
				m.log.Info("logs: pruned expired log files", "files", n)
			}
		case <-ticker.C:
			m.refreshBackendGate(ctx)
			m.scanOnce(ctx)
		}
	}
}

// scanOnce 执行一轮扫描：发现服务集并逐流轮询拉取增量。轮内顺路构造访问
// 日志的路由键候选集（active apps × 各自受管服务按 ingress 公式），尾部
// 以同一候选集采集平台入口的访问流（W5-S2；backend=victorialogs 时生效）。
func (m *Manager) scanOnce(ctx context.Context) {
	apps, err := m.st.ListActiveApps(ctx)
	if err != nil {
		m.log.Warn("logs: list apps failed", "error", err.Error())
		return
	}
	active := make(map[string]struct{}, len(apps))
	for _, app := range apps {
		// 采集面的 app 键 = 三段限定形（v0.3 流标签口径，rbac-teams §4.3
		// ——游标/ring/淘汰记账与入湖 label 同键，Follow 消费面同值）。
		active[qualifiedAppOf(app)] = struct{}{}
	}
	// M7-6：先对账已消失 app 的延迟淘汰（连续 miss 超窗 → 游标 + ring
	// 回收），再推进活跃面采集。
	m.evictStaleStreamState(active)
	candidates := make(map[string]accessTarget)
	for _, app := range apps {
		services, err := m.port.ManagedServiceProcesses(ctx, qualifiedAppOf(app))
		if err != nil {
			// 底座暂态（swarm 未就绪等）：跳过本轮，下轮重试。
			m.log.Debug("logs: service discovery failed", "app", app.Name, "error", err.Error())
			continue
		}
		red := m.red.forApp(ctx, app.ID)
		for _, svc := range services {
			candidates[accessRouterName(app.TeamSlug, app.ProjectSlug, app.Name, svc)] = accessTarget{app: app, service: svc}
			m.pollAppStream(ctx, app, svc, red)
		}
		// E5 Cron / DT-4：一次性 job 服务（fleetly-cron- / fleetly-init- 前缀，
		// 服务名不进命名公式发现面）的日志同管线采集，归属 (app, compose
		// 服务)。
		m.pollJobServices(ctx, app, red)
	}
	// DT-5：任务服务日志（无 app 归属的独立发现面；仅 VL 后端采集——
	// 纯 jsonl 形态无任务检索面，采集面整体跳过，诚实边界见 runbook）。
	m.pollTaskServices(ctx)
	m.pollAccess(ctx, candidates)
}

// qualifiedAppOf 是 app 三段限定形的本地出口（state.App.QualifiedName 同
// 式；logs 包统一经此处取值，便于口径单点）。
func qualifiedAppOf(app state.App) string { return app.QualifiedName() }

// pollAppStream 拉取长驻服务的单条流（compose 服务名 → 命名公式 swarm 名；
// v0.3 三段公式——team/prj 段随 app 行 slug）。
func (m *Manager) pollAppStream(ctx context.Context, app state.App, service string, red *redactor) {
	swarmName, err := naming.ServiceName(app.TeamSlug, app.ProjectSlug, app.Name, service)
	if err != nil {
		m.log.Warn("logs: swarm service name resolve failed", "app", app.Name, "service", service, "error", err.Error())
		return
	}
	m.pollStream(ctx, streamPoll{
		swarmName:     swarmName,
		cursorKey:     streamKey(qualifiedAppOf(app), service),
		cursorApp:     qualifiedAppOf(app),
		cursorAppID:   app.ID,
		cursorService: service,
		sink:          m.appSink(app, service, red),
	})
}

// jobCursorKey 是一次性 job 采集游标键（与长驻 (app, service) 键隔离——
// 同一 (app, service) 的长驻流与历次触发的 job 流各自独立记账；键保留 app
// 前缀，app 级延迟淘汰〔M7-6〕自然覆盖）。族标记 \x00job\x00 对 cron 与
// init 共用（键内已含 job 服务名，跨族不撞）。
func jobCursorKey(app, jobService string) string {
	return app + "\x00job\x00" + jobService
}

// taskCursorKey / taskCursorPrefix 是任务日志采集的游标键（DT-5：与 app
// 游标面完全隔离——任务无 app 归属；键 = \x00task\x00 + 任务平台 ID）。
func taskCursorKey(taskID string) string { return "\x00task\x00" + taskID }

func taskCursorPrefix() string { return "\x00task\x00" }

// pollTaskServices 采集平台任务服务的日志（DT-5 任务日志面）：任务行以
// task 标签归因入湖（VL）；发现即从零全量回读（任务生命周期秒级/分钟级，
// 历史即全部），服务消失后游标当轮回收。仅 VL 后端采集——纯 jsonl 形态
// 无任务检索面（不落盘：落盘面按 app 分文件，任务无 app 归属），诚实边界
// 记录在 proto 与 runbook（承载点 = taskSink）。
func (m *Manager) pollTaskServices(ctx context.Context) {
	if !m.vlEnabled() {
		m.evictCursors(taskCursorPrefix(), map[string]bool{})
		return
	}
	states, err := m.port.TaskServiceStates(ctx)
	if err != nil {
		// 底座暂态：跳过本轮，游标保留（下一轮发现集为空时回收）。
		m.log.Debug("logs: task service discovery failed", "error", err.Error())
		return
	}
	seen := make(map[string]bool, len(states))
	for _, s := range states {
		taskID := s.Labels[state.LabelTaskID]
		if taskID == "" {
			continue
		}
		key := taskCursorKey(taskID)
		seen[key] = true
		m.pollStream(ctx, streamPoll{
			swarmName: s.Name,
			cursorKey: key,
			// 任务无 app 归属：游标归属三值皆空（app 级延迟淘汰不适用，
			// 回收由发现集对账〔evictCursors〕承载）。
			fromStart: true, // 发现即零点全量回读（一次性/短生命周期语义）。
			sink:      m.taskSink(taskID),
		})
	}
	m.evictCursors(taskCursorPrefix(), seen)
}

// evictCursors 回收前缀族内发现集之外的采集游标（job 与 task 两族共用的
// 一份实现；族 = 游标键前缀）：本轮发现集之外的游标 = 客体已消失（job 服
// 务被调度器/发布管线删除〔完成收口〕或残留清扫；任务被任务平台回收）——
// 游标失去客体，立即回收不悬挂。hub ring 不在此清理范围（job 行与长驻服
// 务同面合流共享 (app,service) 键；任务行不进 ring）；job 游标另受 app 级
// 延迟淘汰（M7-6）整体兜底。
func (m *Manager) evictCursors(prefix string, seen map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.streams {
		if strings.HasPrefix(key, prefix) && !seen[key] {
			delete(m.streams, key)
		}
	}
}

// taskSink 返回任务流家族的入库面（IMPL-ARCH-H 收编后，原 pollTaskStream
// 与 app 循环的唯一真差异所在）：task 标签归因直推入湖批量器。「jsonl 后端
// ⇒ 不收任务日志」的诚实边界由本 sink 与族入口门承载、不是循环骨架的参数：
// 仅 VL 后端采集（门在 pollTaskServices，纯 jsonl 形态无任务检索面，采集面
// 整体跳过）；不 redact（任务无 app env 脱敏面，值由调用方自持）；不进
// ring（直播面按 (app,service) 寻址，任务无归属）；不落盘（落盘面按 app
// 分文件）。边界记录另见 proto 注释与 docs/runbooks/dynamic-tasks.md。
func (m *Manager) taskSink(taskID string) streamSink {
	return streamSink{
		deliver: func(_ context.Context, line substrate.LogLine) {
			m.ing.Add(Entry{
				Task:   taskID,
				At:     line.At,
				Stderr: line.Stderr,
				Line:   line.Line, // 任务无 app env 脱敏面（值由调用方自持）
				Source: SourceContainer,
			})
		},
		openFailed: func(err error) {
			m.log.Debug("logs: task stream open failed", "task", taskID, "error", err.Error())
		},
		watchdogFired: func() {
			m.log.Error("logs: task poll round exceeded watchdog deadline, abandoning this round", "task", taskID)
		},
	}
}

func jobCursorPrefix(app string) string { return app + "\x00job\x00" }

// jobServiceRefOf 把一次性 job 服务的实况投影解析为采集归属（映射解析的
// 单测面）：swarm 服务名必须带 fleetly-cron- 或 fleetly-init- 前缀（两个
// 瞬时族共享采集面），归属 app/process 以受管 label 为权威——job 服务名的
// 字符串反解在 app/service 含 '-' 时有歧义，label 是唯一权威；app 谓词
// 拦下 label 漂移的误归属。
func jobServiceRefOf(app string, s engine.ServiceState) (JobServiceRef, bool) {
	if !naming.IsCronJobName(s.Name) && !naming.IsInitJobName(s.Name) {
		return JobServiceRef{}, false
	}
	if s.Labels[state.LabelApp] != app {
		return JobServiceRef{}, false
	}
	service := s.Labels[state.LabelProcess]
	if service == "" {
		return JobServiceRef{}, false
	}
	return JobServiceRef{JobService: s.Name, Service: service}, true
}

// pollJobServices 采集该 app 的一次性 job 服务日志（E5 Cron / DT-4 的留存
// 兑现：cron 触发的一次性 job 与发布期 init job 共享同一采集面）：job 服务
// 存活期（触发 → 完成删除，秒级到拍级）内尽力采集——发现即从零全量回读
// （一次性作业的历史即全部，长驻服务「发现时刻起采、不回灌历史」的口径不
// 适用），行按 (app, compose 服务) 归属与长驻同面（ring/落盘/History/Follow
// 同键合流）；服务删除后流自然断、游标当轮回收，无悬挂 goroutine
//（pollStream 同步排空 + MG-1 看门狗兜底）。
func (m *Manager) pollJobServices(ctx context.Context, app state.App, red *redactor) {
	states, err := m.port.JobServiceStates(ctx, qualifiedAppOf(app))
	if err != nil {
		// 底座暂态：跳过本轮，游标保留（服务若已删，下一轮发现集为空时回收）。
		m.log.Debug("logs: job service discovery failed", "app", app.Name, "error", err.Error())
		return
	}
	seen := make(map[string]bool, len(states))
	for _, s := range states {
		ref, ok := jobServiceRefOf(qualifiedAppOf(app), s)
		if !ok {
			continue
		}
		key := jobCursorKey(qualifiedAppOf(app), ref.JobService)
		seen[key] = true
		m.pollStream(ctx, streamPoll{
			swarmName:     ref.JobService,
			cursorKey:     key,
			cursorApp:     qualifiedAppOf(app),
			cursorAppID:   app.ID,
			cursorService: ref.Service,
			fromStart:     true, // 一次性作业历史即全部：零点全量回读。
			sink:          m.appSink(app, ref.Service, red),
		})
	}
	m.evictCursors(jobCursorPrefix(qualifiedAppOf(app)), seen)
}

// streamSink 是流家族的入库面（轮询循环骨架的家族差异收敛点）：deliver
// 消费一条已投递的底座行（Entry 构造与入库路由的族内单点）；openFailed /
// watchdogFired 承载族专属诊断日志（消息与字段各族自持，骨架不感知家族
// 身份）。
type streamSink struct {
	deliver       func(ctx context.Context, line substrate.LogLine)
	openFailed    func(err error)
	watchdogFired func()
}

// streamPoll 是单条流的一轮拉取参数（循环骨架的全部家族差异）。
type streamPoll struct {
	// swarmName 是底座流名（StreamServiceLogs 的打开目标）。
	swarmName string
	// cursorKey 是采集游标键（m.streams 表键；三族互不撞键：长驻 =
	// streamKey(app, service)，job = jobCursorKey，任务 = taskCursorKey）。
	cursorKey string
	// cursorApp / cursorAppID / cursorService 是游标首拍归属初值（仅
	// get-or-init 的 miss 分支读取；app 级延迟淘汰〔M7-6〕按 cur.app 对账
	// ——任务族三值皆空，任务无 app 归属，游标回收由发现集对账承载）。
	cursorApp     string
	cursorAppID   string
	cursorService string
	// fromStart 报告首拍锚点是否从零起算：job/任务 = true（一次性/短生命
	// 周期作业的历史即全部），长驻服务 = false（M7「发现时刻起采」口径）。
	fromStart bool
	// sink 是入库面（Entry 构造 + 路由 + 族专属诊断日志）。
	sink streamSink
}

// pollStream 拉取单条流（swarm 服务名 p.swarmName）的本轮增量并交 p.sink
// 入库——全部流家族共用的唯一轮询循环骨架（IMPL-ARCH-H 第三次 fork 收编：
// 原 pollStreamNamed 与逐行重抄的 pollTaskStream 只剩这一份）：cursor 取/
// 置（m.mu 下）/推进、StreamServiceLogs 调用、MG-1 看门狗、deliver select、
// last 时间戳跟踪各一份实现；家族差异全部收敛为 streamPoll（游标键/首拍
// 锚点口径/归属初值）与 streamSink。访问日志面（pollAccess）游标为独立
// 标量、行级路由带归属反解，不在本骨架（见 access.go）。
func (m *Manager) pollStream(ctx context.Context, p streamPoll) {
	m.mu.Lock()
	cur, ok := m.streams[p.cursorKey]
	if !ok {
		anchor := m.clock()
		if p.fromStart {
			// 首拍零点全量回读：since 零值 = 底座不设过滤起点（job/任务从
			// 服务创建起的全部输出；行短量小，一次性语义天然有界）。
			anchor = time.Time{}
		}
		cur = &stream{app: p.cursorApp, appID: p.cursorAppID, service: p.cursorService, lastAt: anchor}
		m.streams[p.cursorKey] = cur
	}
	since := cur.lastAt
	m.mu.Unlock()

	lines, err := m.port.StreamServiceLogs(ctx, p.swarmName, since, false)
	if err != nil {
		p.sink.openFailed(err)
		return
	}
	// MG-1 纵深防御：单轮看门狗。scanLines 修复后底座流恒会排水结束，此处
	// 只兜「底座/管线再出同款永不返回缺陷」的极端面——超时记 Error 并放弃
	// 本轮（下一轮重开流重试），采集循环/落盘/prune 不再被单条流拖死。权衡：
	// 放弃后底座侧流 goroutine 可能挂在无读者的 channel 发送上，泄漏到 ctx
	// 取消（进程停机）——跳过一轮的代价优于全管线停摆，且正常路径（流必
	// 结束）不会触发，故选最小实现：只告警 + 弃轮，不主动断流。
	watchdog := time.NewTimer(m.pollWatchdog())
	defer watchdog.Stop()
	var last time.Time
deliver:
	for {
		select {
		case <-ctx.Done():
			return
		case <-watchdog.C:
			p.sink.watchdogFired()
			return
		case line, ok := <-lines:
			if !ok {
				break deliver // 流自然结束。
			}
			at := line.At
			if at.IsZero() {
				// M7-4：无时间戳续行不推进游标——旧实现以墙钟代投，会把
				// cur.lastAt 拨到 now，下一轮 since 越过同批未投递行（跳批
				// 丢行）。行本身以零值 At 投递（视图层容忍 e.at 可空，console
				// 端 `e.at ?? ""`；落盘侧落入零日文件 00010101.jsonl，检索
				// 与保留期清理语义均覆盖）。取舍：若全批皆零 At，last 保持
				// 不动，下轮重复拉取该批——重复优于丢失，行内容相同幂等。
			} else if at.After(last) {
				last = at // 游标只随可信时间戳行推进。
			}
			p.sink.deliver(ctx, line)
		}
	}
	if !last.IsZero() {
		m.mu.Lock()
		if last.After(cur.lastAt) {
			cur.lastAt = last
		}
		m.mu.Unlock()
	}
}

// appSink 返回 app 家族（长驻服务与一次性 job 共用）的入库面：redact →
// 直播面（ring + 扇出）→ 入湖/落盘路由。
func (m *Manager) appSink(app state.App, service string, red *redactor) streamSink {
	return streamSink{
		deliver: func(ctx context.Context, line substrate.LogLine) {
			e := Entry{
				// app = 三段限定形（v0.3 流标签口径——入湖 label / ring 键 /
				// 落盘文件名同值；消费面 Follow/History 按限定形寻址）。
				App:     qualifiedAppOf(app),
				Service: service,
				At:      line.At,
				Stderr:  line.Stderr,
				Line:    red.redact(line.Line),
				Source:  SourceContainer,
			}
			// 直播面（ring + 扇出）零改动——A3 直读不动条款：入湖/落盘的
			// 路由选择发生在直播面之后（E6 设计 §2.4：VL 故障 = 检索降级，
			// 直播照常）。
			m.hub.ingest(e)
			if m.vlEnabled() {
				// victorialogs 模式：入湖（行已脱敏——redact 之后接入），
				// JSONL 落盘停止（设计 §2.3「双写不留，磁盘不翻倍」）。
				m.ing.Add(e)
			} else if err := m.dsk.append(ctx, e); err != nil {
				m.log.Warn("logs: disk append failed", "app", app.Name, "error", err.Error())
			}
		},
		openFailed: func(err error) {
			// 服务不存在（未部署/已删）安静跳过；其余底座暂态 debug 级。
			m.log.Debug("logs: stream open failed", "app", app.Name, "service", service, "error", err.Error())
		},
		watchdogFired: func() {
			m.log.Error("logs: poll round exceeded watchdog deadline, abandoning this round",
				"app", app.Name, "service", service)
		},
	}
}

// minPollWatchdog 是单轮拉取看门狗的期限下限（MG-1 纵深防御）。
const minPollWatchdog = 30 * time.Second

// pollWatchdog 返回本轮看门狗期限：max(3×扫描周期, 30s)——既覆盖慢轮询
// 周期下的正常整轮时长，又有绝对下限防扫描周期配置过小时看门狗过敏。
// pollWatchdogOverride 是测试注入位（同包私有，缩短真实等待）。
func (m *Manager) pollWatchdog() time.Duration {
	if m.pollWatchdogOverride > 0 {
		return m.pollWatchdogOverride
	}
	d := 3 * m.cfg.ScanInterval()
	if d < minPollWatchdog {
		d = minPollWatchdog
	}
	return d
}

// streamEvictAfter 是 app 消失后采集状态的延迟淘汰窗（M7-6）：窗内 app
// 回归则撤销计时保留状态（采集连续性优先）；超窗删除该 app 全部
// per-service 游标与 ring——app 回归等价首启语义（游标自发现时刻重建，
// 历史由落盘承载，代价可接受）。不淘汰则 hub.streams/m.streams 只增不删，
// 随 app 创建/删除更迭无限累积（ring 每流 ringSize 条）。
const streamEvictAfter = 5 * time.Minute

// evictStaleStreamState 执行一轮淘汰对账（scanOnce 头部调用）：非 active
// app 记 miss 首见时刻；连续 miss 超过 streamEvictAfter 的 app 删除其全部
// 游标（m.streams 按 app 前缀）并同步清 hub 侧 ring。active 判定以本轮
// ListActiveApps 快照为准。
func (m *Manager) evictStaleStreamState(active map[string]struct{}) {
	now := m.clock()
	m.mu.Lock()
	// 计时更新：活跃 app 撤销 miss 计时；非活跃 app 记首见 miss 时刻。
	for _, cur := range m.streams {
		if _, ok := active[cur.app]; ok {
			delete(m.appMiss, cur.app)
			continue
		}
		if _, seen := m.appMiss[cur.app]; !seen {
			m.appMiss[cur.app] = now
		}
	}
	// 淘汰：连续 miss 超窗 → 删该 app 全部游标（各服务键在迭代中一并命中）。
	var evicted []string
	for key, cur := range m.streams {
		if _, ok := active[cur.app]; ok {
			continue
		}
		if first, miss := m.appMiss[cur.app]; miss && now.Sub(first) >= streamEvictAfter {
			delete(m.streams, key)
			if !slices.Contains(evicted, cur.app) {
				evicted = append(evicted, cur.app)
			}
		}
	}
	// 删尽的 app 清计时（app 再消失等价首次发现，重新起算）。
	for _, app := range evicted {
		delete(m.appMiss, app)
	}
	m.mu.Unlock()
	if len(evicted) > 0 {
		m.hub.evictApps(evicted)
		m.log.Info("logs: evicted stream state for inactive apps", "apps", strings.Join(evicted, ","))
	}
}

// Follow 订阅实时日志（T2.20：实时流；gRPC Follow RPC 的管线入口）。
// 先回放 ring（限深回放），后实时扇出；ctx 取消自动注销（断线重连 =
// 客户端重新 Follow）。
func (m *Manager) Follow(ctx context.Context, app, service string) (<-chan Entry, func()) {
	return m.hub.follow(ctx, app, service)
}

// redactBudget 是 IngestBuildLine 的脱敏值集读取预算（redactor 缓存命中
// 时无 IO；未命中重建的兜底期限）。
const redactBudget = 3 * time.Second

// IngestBuildLine 把一条构建日志行接入入湖批量器（W5-S1，设计 §2.3
//「build 日志与写入咽喉点接入，source=build」；实现 build.BuildLogSink
// 端口——方向纪律：build 定义端口、logs 实现，build 不反向感知 logs）。
// 咽喉点 = build.log 文件的行级写入（internal/build logTee 分流），本方法
// 在消费侧行同款脱敏（red.forApp——构建日志同可能回显 secret 值，与
// buildLogEntries 读侧脱敏同一纪律），只进批量器：不进 ring（直播面不加
// 噪——FollowLogs 零改动），不落 JSONL（build 行从未落盘，jsonl 模式下
// 构建日志仍以 builds 表 log_path 产物为检索面）。backend 非 victorialogs
// 时静默 no-op。
func (m *Manager) IngestBuildLine(appID, app, service string, at time.Time, line string) {
	if m.ing == nil || !m.vlEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redactBudget)
	defer cancel()
	red := m.red.forApp(ctx, appID)
	m.ing.Add(Entry{
		App:     app,
		Service: service,
		At:      at,
		Line:    red.redact(line),
		Source:  SourceBuild,
	})
}

// HistoryQuery 是历史检索参数（app 必填；service/source 空 = 不过滤）。
type HistoryQuery struct {
	App     string
	Service string
	Source  string // "" | container | build
	Since   time.Time
	Until   time.Time
	Limit   int
}

// History limit 缺省与天花板。
const (
	defaultHistoryLimit = 200
	maxHistoryLimit     = 1000
	maxBuildsPerHistory = 50
)

// History 检索历史日志：container 来源走落盘扫描；build 来源读 builds 表
// log_path 产物（行级无时间戳，行时间取构建开始时刻——诚实缺失，不伪造
// 逐行时间）。结果按时间升序，超 limit 取最新 limit 条。
func (m *Manager) History(ctx context.Context, q HistoryQuery) ([]Entry, error) {
	if q.Limit <= 0 {
		q.Limit = defaultHistoryLimit
	}
	if q.Limit > maxHistoryLimit {
		q.Limit = maxHistoryLimit
	}
	app, err := m.st.GetAppByName(ctx, q.App)
	if err != nil {
		return nil, err
	}
	// 落盘/入湖文件与行均以三段限定形为 app 键（v0.3 流标签口径）——检索
	// 键随解析后的行归属换算，请求侧继续收裸名/限定形（GetAppByName 兼容）。
	q.App = qualifiedAppOf(app)
	var out []Entry
	if q.Source == "" || q.Source == SourceContainer {
		rows, err := m.dsk.query(ctx, q.App, q.Service, SourceContainer, q.Since, q.Until, q.Limit)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if q.Source == "" || q.Source == SourceBuild {
		rows, err := m.buildLogEntries(ctx, app, q)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}

// buildLogEntries 从 builds 表产物读构建日志（source=build）。B3（出站
// 字节出口收口）：出口与容器日志同管线过该 app 的 redactor——构建日志
// 同样可能回显 secret 值（拉源 token、env 值），不得因来源不同绕过脱敏。
func (m *Manager) buildLogEntries(ctx context.Context, app state.App, q HistoryQuery) ([]Entry, error) {
	builds, err := m.st.ListAppBuilds(ctx, app.ID, maxBuildsPerHistory)
	if err != nil {
		return nil, err
	}
	red := m.red.forApp(ctx, app.ID)
	var out []Entry
	for _, b := range builds {
		if q.Service != "" && b.Service != q.Service {
			continue
		}
		if b.LogPath == "" {
			continue
		}
		at := b.StartedAt
		if at.IsZero() {
			at = b.CreatedAt
		}
		if !q.Since.IsZero() && at.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && at.After(q.Until) {
			continue
		}
		lines, err := readPlainLines(b.LogPath, q.Limit)
		if err != nil {
			m.log.Warn("logs: read build log failed", "build", b.ID, "error", err.Error())
			continue
		}
		for _, line := range lines {
			out = append(out, Entry{
				App:     qualifiedAppOf(app),
				Service: b.Service,
				At:      at,
				Line:    red.redact(line),
				Source:  SourceBuild,
			})
		}
	}
	return out, nil
}

// readPlainLines 读取纯文本日志文件的**最后** limit 行（M7-5：构建失败的
// 关键信息——错误摘要/退出原因——在文件尾部，头部截断使诊断面不可达，且
// 与容器源「超 limit 取最新 limit 条」语义相反）。滑窗实现：总驻留 ≤ limit
// 行，文件多大都不全量进内存；单行仍受 1MiB 扫描上限（builds 产物行不会
// 接近该量级，维持原语义不在本轮扩面）。
func readPlainLines(path string, limit int) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // G304：路径来自 builds 表登记（写侧平台受管）
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var win []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if limit > 0 && len(win) == limit {
			copy(win, win[1:]) // 挤掉最旧（滑窗）
			win[limit-1] = line
			continue
		}
		win = append(win, line)
	}
	return win, sc.Err()
}
