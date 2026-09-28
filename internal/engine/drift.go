package engine

// 运行域漂移检测（T2.13，state-model §2.5）：
//   - 期望态 = 最近一次 succeeded deployment 的 desired_spec 快照（平台
//     写入的目标形态）；实况 = ServiceInspect 反解（substrate 投影还原
//     ServiceSpec）。两侧各算 canonical 投影哈希逐服务比对——判定只用
//     hash（D-STM-5：廉价、稳定、不泄露 env）；
//   - 与 spec_hash 的分层关系：spec_hash 是**文件域**哈希（归一化 compose
//     的 canonical JSON，env 只有 key:sha256——入队后被改写的文件漂移由
//     preparing 的 spec_hash 复核承载）；本文件是**运行域**哈希（Swarm
//     服务实况 vs 平台期望投影——外部 docker service update 等运行期改动
//     的判定面）。两层各管一面，互不替代；
//   - 投影受控子集（§2.5 字段清单）：镜像/命令/env〔key:sha256(value)〕/
//     mounts/replicas/labels〔service 层剔除 fleetly.deployment 与
//     fleetly.desired-hash 两个平台簿记键，其余照比；container 层照比〕/
//     约束/resources/health/restart/stop/网络/secret 引用/global。env 只
//     报键名 + hash（值明文与长度都不出投影）；网络按别名集合投影（swarm
//     把 Target 归一为网络 ID，名称还原需额外观测面——别名集合形状仍可
//     检出 detach/attach，见遗留记录）；镜像按 digest 尾部比对（swarm 会
//     把 tag 归一为 repo@sha256:…，repo 字符串形态不参与）；
//   - 检测器：engine 服务内周期扫描（默认 30s，engine.drift_interval_seconds）
//     managed 服务集；漂移出现（no-drift → drift 迁移）→ 事件
//     reconcile.drift_detected（app/service/字段级 diff）+ 审计；
//   - 收敛 opt-in（D11：检测默认开、自动收敛默认关）：apps.drift_converge
//     位（00005 加法列，默认关）；开启时漂移 → 归位重放原语按当前期望态
//     收敛；回滚失败强制关闭直至人工重置；`fleetly drift converge` 为
//     人工一次性收敛（带审计，不经 opt-in 位）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// LabelBookkeeping 是漂移投影剔除的平台簿记 label 键：deployment 归属随
// 每次部署/回滚合法变化；desired-hash 本身就是被比对的量（写前才落 label）
// ——两者都不是用户可见期望态。
var LabelBookkeeping = map[string]bool{
	state.LabelDeployment:  true,
	state.LabelDesiredHash: true,
}

// driftEnvEntry 是投影后的 env 条目（值只以 sha256 hex 表示——判定与报告
// 都不携带明文，state-model §2.5「env 只报键名与 key:hash」）。
type driftEnvEntry struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}

// driftNetwork 是投影后的网络接入（别名集合形状；Target 由 swarm 归一为
// ID 不参与比对——见文件头注）。
type driftNetwork struct {
	Aliases []string `json:"aliases,omitempty"`
}

// driftSpec 是漂移哈希的 canonical 投影（期望与实况两侧同构；字段集 =
// state-model §2.5 受控子集 + S18-A8 补齐的 UpdateConfig 三字段）。
type driftSpec struct {
	Image             string             `json:"image"`
	Command           []string           `json:"command,omitempty"`
	Env               []driftEnvEntry    `json:"env,omitempty"`
	Replicas          uint64             `json:"replicas"`
	Global            bool               `json:"global,omitempty"`
	Mounts            []MountSpec        `json:"mounts,omitempty"`
	Networks          []driftNetwork     `json:"networks,omitempty"`
	ServiceLabels     map[string]string  `json:"service_labels,omitempty"`
	ContainerLabels   map[string]string  `json:"container_labels,omitempty"`
	Constraints       []string           `json:"constraints,omitempty"`
	Resources         *ResourcesSpec     `json:"resources,omitempty"`
	Healthcheck       *HealthcheckSpec   `json:"healthcheck,omitempty"`
	RestartPolicy     *RestartPolicySpec `json:"restart_policy,omitempty"`
	StopSignal        string             `json:"stop_signal,omitempty"`
	StopGracePeriod   time.Duration      `json:"stop_grace_period,omitempty"`
	Secrets           []SecretMount      `json:"secrets,omitempty"`
	Configs           []ConfigMount      `json:"configs,omitempty"`
	UpdateOrder       string             `json:"update_order"`
	UpdateParallelism uint64             `json:"update_parallelism"`
	UpdateDelay       time.Duration      `json:"update_delay,omitempty"`
}

// driftProjection 把 ServiceSpec 投影为漂移哈希输入（确定性：env 按 key
// 字典序、网络按别名串字典序、label 键字典序由 canonical JSON 保证）。
func driftProjection(s ServiceSpec) driftSpec {
	// M1-3：global 服务的副本数归一——Swarm global 模式无受管 Replicas
	// 语义（期望侧规划写副本缺省值、实况侧 Mode.Global 读回 0——适配器
	// serviceToState 对 global 恒 0），两侧若照抄会永久假阳性漂移。归一取
	// 1：与 desiredReplicasOf 的观察语义一致（单机 global 按一实例计），
	// 且 diffDrift 的 replicas 项两侧同值不产出（副本数对 global 非受管
	// 字段；v0.1 已在校验层拒绝新部署声明 mode: global，本归一防存量
	// 快照/回滚路径的漂移误报）。
	replicas := s.Replicas
	if s.Global {
		replicas = 1
	}
	out := driftSpec{
		Image:             driftImage(s.Image),
		Command:           append([]string{}, s.Command...),
		Replicas:          replicas,
		Global:            s.Global,
		Mounts:            append([]MountSpec{}, s.Mounts...),
		Constraints:       append([]string{}, s.Constraints...),
		Resources:         s.Resources,
		Healthcheck:       s.Healthcheck,
		RestartPolicy:     s.RestartPolicy,
		StopSignal:        s.StopSignal,
		StopGracePeriod:   s.StopGracePeriod,
		Secrets:           append([]SecretMount{}, s.Secrets...),
		Configs:           append([]ConfigMount{}, s.Configs...),
		ContainerLabels:   filterLabels(s.ContainerLabels, nil),
		ServiceLabels:     filterLabels(s.ServiceLabels, LabelBookkeeping),
		UpdateOrder:       s.UpdateOrder,
		UpdateParallelism: s.UpdateParallelism,
		UpdateDelay:       s.UpdateDelay,
	}
	for _, kv := range s.Env {
		k, v, _ := strings.Cut(kv, "=")
		sum := sha256.Sum256([]byte(v))
		out.Env = append(out.Env, driftEnvEntry{Key: k, Hash: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out.Env, func(i, j int) bool { return out.Env[i].Key < out.Env[j].Key })
	for _, n := range s.Networks {
		aliases := append([]string{}, n.Aliases...)
		sort.Strings(aliases)
		out.Networks = append(out.Networks, driftNetwork{Aliases: aliases})
	}
	sort.Slice(out.Networks, func(i, j int) bool {
		return strings.Join(out.Networks[i].Aliases, ",") < strings.Join(out.Networks[j].Aliases, ",")
	})
	return out
}

// driftHash 是运行域服务期望哈希（投影 canonical JSON 的 sha256）。
func driftHash(s ServiceSpec) string { return canonicalHash(driftProjection(s)) }

// driftImage 归一镜像引用：两侧都带 digest 时只比 digest 尾部（swarm 对
// tag 引用会补钉 repo@sha256:…，repo 字符串不归一化，比对字符串形态会
// 误报）；任一侧无 digest（本机构建镜像按 tag 部署）时整串比对。
func driftImage(ref string) string {
	if i := strings.Index(ref, "@sha256:"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// filterLabels 复制 label 集并剔除 exclude 键（nil = 全保留）。
func filterLabels(in map[string]string, exclude map[string]bool) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if exclude != nil && exclude[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// FieldDiff 是一个字段的差异条目（值均为投影形态：env 只到键名 + hash）。
type FieldDiff struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// ServiceDrift 是单个服务的漂移结果。
type ServiceDrift struct {
	Service string      `json:"service"`
	Drifted bool        `json:"drifted"`
	Missing bool        `json:"missing,omitempty"`
	Extra   bool        `json:"extra,omitempty"`
	Diff    []FieldDiff `json:"diff,omitempty"`
}

// DriftReport 是一次漂移检测报告。
type DriftReport struct {
	App string `json:"app"`
	// DesiredDeployment 是期望态来源部署（空 = 无成功部署记录，无从判定）。
	DesiredDeployment string         `json:"desired_deployment,omitempty"`
	Drifted           bool           `json:"drifted"`
	Services          []ServiceDrift `json:"services,omitempty"`
}

// DriftShow 按应用即时检测运行域漂移（CLI 读面；不写事件不收敛）。
func (e *Engine) DriftShow(ctx context.Context, appName string) (*DriftReport, error) {
	app, err := e.store.GetAppByName(ctx, appName)
	if err != nil {
		return nil, err
	}
	return e.computeAppDrift(ctx, app)
}

// computeAppDrift 是漂移判定的共享核心（检测器与 DriftShow 同源）。期望
// 副本先经 pinScalingReplicaOverrides 钉平台运行期覆盖（autoscaler 写通道
// ——D-V3W5-2：平台自己写的副本不被漂移误报，外部 scale 照常检出）。
func (e *Engine) computeAppDrift(ctx context.Context, app state.App) (*DriftReport, error) {
	report := &DriftReport{App: app.Name}
	source, specs, err := e.lastSucceededSpecs(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return report, nil // 无成功部署：无期望态，无从判定漂移
	}
	overrides, err := e.store.ListScalingReplicaOverrides(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	pinScalingReplicaOverrides(specs, overrides, source.ID)
	report.DesiredDeployment = source.ID

	desiredNames := map[string]bool{}
	for i := range specs {
		desiredNames[specs[i].Name] = true
		desired := driftProjection(specs[i])
		sd := ServiceDrift{Service: specs[i].Name}
		actualState, err := e.sub.ServiceInspect(ctx, specs[i].Name)
		if err != nil {
			if errors.Is(err, ErrServiceNotFound) {
				sd.Drifted = true
				sd.Missing = true
				sd.Diff = append(sd.Diff, FieldDiff{Field: "service", Expected: "present", Actual: "absent"})
				report.Services = append(report.Services, sd)
				report.Drifted = true
				continue
			}
			return nil, err
		}
		actual := driftProjection(serviceSpecOf(actualState))
		sd.Drifted = driftHash(specs[i]) != driftHash(serviceSpecOf(actualState))
		if sd.Drifted {
			sd.Diff = diffDrift(desired, actual)
		}
		// A8 专报：failure_action 是平台受管字段（适配器固定 pause，D-REL-1
		// ——平台是唯一回滚决策者），不进期望态哈希；实况读回非 pause 即
		// 受管字段被外部篡改（docker service update --update-failure-action
		// rollback 等）→ 专报漂移项（随 reconcile.drift_detected 事件披露）。
		// 空串 = 适配器未投影（旧形态/只读面），不误报。
		if fa := actualState.UpdateFailureAction; fa != "" && fa != "pause" {
			sd.Drifted = true
			sd.Diff = append(sd.Diff, FieldDiff{
				Field: "update_failure_action", Expected: "pause", Actual: fa})
		}
		if sd.Drifted {
			report.Drifted = true
		}
		report.Services = append(report.Services, sd)
	}
	// 期望集之外的多余受管服务（外部创建/未清理）也是漂移（省略=删除的
	// 期望语义；收敛原语会删）。归属过滤与一次性 job 豁免经 scopeManaged
	// Services 单点定义（IMPL-ARCH-A：此前此处以裸 app.Name 手写过滤——
	// W2-S3 起 fleetly.app 值为三段限定形，裸名精确匹配恒空，本腿死了；
	// job 豁免的原始理由见下）——例外：一次性 job 服务（fleetly-cron- /
	// fleetly-init- 前缀）是平台瞬时对象，收敛原语**不会**删（对账删除扫
	// 描豁免），标漂移与此前提自相矛盾；生命周期归 cron 调度器与 init 相
	// 位/sweepInitJobs。
	existing, err := e.scopeManagedServices(ctx, app)
	if err != nil {
		return nil, err
	}
	for _, s := range existing {
		if desiredNames[s.Name] {
			continue
		}
		report.Services = append(report.Services, ServiceDrift{
			Service: s.Name, Drifted: true, Extra: true,
			Diff: []FieldDiff{{Field: "service", Expected: "absent", Actual: "present"}},
		})
		report.Drifted = true
	}
	return report, nil
}

// diffDrift 产出字段级差异（输入为两侧投影；报告形态即事件/CLI 展示形态
// ——env 只到 key+hash）。
func diffDrift(expected, actual driftSpec) []FieldDiff {
	var out []FieldDiff
	add := func(field, want, got string) {
		out = append(out, FieldDiff{Field: field, Expected: want, Actual: got})
	}
	if expected.Image != actual.Image {
		add("image", expected.Image, actual.Image)
	}
	if jsonStr(expected.Command) != jsonStr(actual.Command) {
		add("command", jsonStr(expected.Command), jsonStr(actual.Command))
	}
	envDiff(expected.Env, actual.Env, add)
	if expected.Replicas != actual.Replicas {
		add("replicas", fmt.Sprint(expected.Replicas), fmt.Sprint(actual.Replicas))
	}
	if expected.Global != actual.Global {
		add("global", fmt.Sprint(expected.Global), fmt.Sprint(actual.Global))
	}
	if jsonStr(expected.Mounts) != jsonStr(actual.Mounts) {
		add("mounts", jsonStr(expected.Mounts), jsonStr(actual.Mounts))
	}
	if jsonStr(expected.Networks) != jsonStr(actual.Networks) {
		add("networks", jsonStr(expected.Networks), jsonStr(actual.Networks))
	}
	mapDiff("label", expected.ServiceLabels, actual.ServiceLabels, add)
	mapDiff("container_label", expected.ContainerLabels, actual.ContainerLabels, add)
	if jsonStr(expected.Constraints) != jsonStr(actual.Constraints) {
		add("constraints", jsonStr(expected.Constraints), jsonStr(actual.Constraints))
	}
	if jsonStr(expected.Resources) != jsonStr(actual.Resources) {
		add("resources", jsonStr(expected.Resources), jsonStr(actual.Resources))
	}
	if jsonStr(expected.Healthcheck) != jsonStr(actual.Healthcheck) {
		add("healthcheck", jsonStr(expected.Healthcheck), jsonStr(actual.Healthcheck))
	}
	if jsonStr(expected.RestartPolicy) != jsonStr(actual.RestartPolicy) {
		add("restart_policy", jsonStr(expected.RestartPolicy), jsonStr(actual.RestartPolicy))
	}
	if expected.StopSignal != actual.StopSignal {
		add("stop_signal", expected.StopSignal, actual.StopSignal)
	}
	if expected.StopGracePeriod != actual.StopGracePeriod {
		add("stop_grace_period", expected.StopGracePeriod.String(), actual.StopGracePeriod.String())
	}
	if jsonStr(expected.Secrets) != jsonStr(actual.Secrets) {
		add("secrets", jsonStr(expected.Secrets), jsonStr(actual.Secrets))
	}
	if jsonStr(expected.Configs) != jsonStr(actual.Configs) {
		add("configs", jsonStr(expected.Configs), jsonStr(actual.Configs))
	}
	// A8：UpdateConfig 三字段（外部 docker service update --update-* 篡改）。
	if expected.UpdateOrder != actual.UpdateOrder {
		add("update_order", expected.UpdateOrder, actual.UpdateOrder)
	}
	if expected.UpdateParallelism != actual.UpdateParallelism {
		add("update_parallelism", fmt.Sprint(expected.UpdateParallelism), fmt.Sprint(actual.UpdateParallelism))
	}
	if expected.UpdateDelay != actual.UpdateDelay {
		add("update_delay", expected.UpdateDelay.String(), actual.UpdateDelay.String())
	}
	return out
}

// envDiff 比对 env 键集：同键 hash 不同 / 单侧缺失（报告只出键名与 hash）。
func envDiff(expected, actual []driftEnvEntry, add func(field, want, got string)) {
	expByKey := map[string]string{}
	for _, e := range expected {
		expByKey[e.Key] = e.Hash
	}
	actByKey := map[string]string{}
	for _, a := range actual {
		actByKey[a.Key] = a.Hash
	}
	keys := map[string]bool{}
	for k := range expByKey {
		keys[k] = true
	}
	for k := range actByKey {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		want, hasWant := expByKey[k]
		got, hasGot := actByKey[k]
		switch {
		case hasWant && hasGot && want != got:
			add("env."+k, want, got)
		case hasWant && !hasGot:
			add("env."+k, want, "<absent>")
		case !hasWant && hasGot:
			add("env."+k, "<absent>", got)
		}
	}
}

// mapDiff 比对 label 集（同键不同值 / 单侧缺失）。
func mapDiff(prefix string, expected, actual map[string]string, add func(field, want, got string)) {
	keys := map[string]bool{}
	for k := range expected {
		keys[k] = true
	}
	for k := range actual {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		want, hasWant := expected[k]
		got, hasGot := actual[k]
		switch {
		case hasWant && hasGot && want != got:
			add(prefix+"."+k, want, got)
		case hasWant && !hasGot:
			add(prefix+"."+k, want, "<absent>")
		case !hasWant && hasGot:
			add(prefix+"."+k, "<absent>", got)
		}
	}
}

// jsonStr 是投影片段的确定性字符串化（diff 报告比较与渲染）。
func jsonStr(v any) string {
	raw, err := canonicalJSON(v)
	if err != nil {
		return "<unserializable>"
	}
	return string(raw)
}

// lastSucceededSpecs 取最近一次 succeeded deployment 及其期望态快照
// （无 → source=nil）。
func (e *Engine) lastSucceededSpecs(ctx context.Context, appID string) (*state.DeployRecord, []ServiceSpec, error) {
	rows, err := e.store.ListAppDeployments(ctx, appID, 50)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: list deployments for drift: %w", err)
	}
	for i := range rows {
		r := rows[i]
		if r.Status != state.DeploySucceeded || r.DesiredSpec == "" {
			continue
		}
		specs, err := e.decodeSpecs(r)
		if err != nil {
			return nil, nil, err
		}
		return &r, specs, nil
	}
	return nil, nil, nil
}

// ── 检测器（周期扫描 + 事件 + opt-in 收敛）─────────────────────────────────

// DriftScan 单步漂移扫描（引擎 Run 周期驱动；测试与诊断显式入口）。
func (e *Engine) DriftScan(ctx context.Context) { e.driftScan(ctx) }

// driftScan 周期扫描全部 active 应用：跳过有在途部署的应用（发布过程本身
// 就是期望态迁移，新旧并存不是漂移）；漂移出现（上拍无漂移 → 本拍有漂移）
// 发 reconcile.drift_detected 事件 + 审计；收敛 opt-in 开启时按当前期望态
// 收敛（归位重放原语）。重启后内存态清零：仍在漂移的应用会再报一次
// （事件重复优于漏报；收敛 opt-in 不受影响）。
func (e *Engine) driftScan(ctx context.Context) {
	apps, err := e.store.ListActiveApps(ctx)
	if err != nil {
		e.log.Warn("engine: drift scan list apps", "error", err)
		return
	}
	inFlight := map[string]bool{}
	if rows, err := e.store.ListNonTerminalDeployments(ctx); err != nil {
		e.log.Warn("engine: drift scan in-flight check", "error", err)
		return
	} else {
		for _, r := range rows {
			inFlight[r.AppID] = true
		}
	}
	for _, app := range apps {
		if inFlight[app.ID] {
			continue
		}
		report, err := e.computeAppDrift(ctx, app)
		if err != nil {
			e.log.Warn("engine: drift scan app", "app", app.Name, "error", err)
			continue
		}
		if report.DesiredDeployment == "" || !report.Drifted {
			e.driftSeen.clear(app.ID)
			continue
		}
		if e.driftSeen.reported(app.ID) {
			continue // 仍在上次报告的漂移中：不重复发事件
		}
		// 先标后报（既有语义，IMPL-ARCH-E 逐面核对保持）：标记在披露前
		// ——报告事务失败不重试本拍事件（漂移恢复清零后再次漂移才可再报）。
		e.driftSeen.mark(app.ID)
		if err := e.reportDrift(ctx, app.ID, app.Name, report); err != nil {
			e.log.Warn("engine: report drift", "app", app.Name, "error", err)
			continue
		}
		// 收敛 opt-in（默认关；回滚失败强制关闭直至人工重置）。
		on, err := e.store.GetAppDriftConverge(ctx, app.ID)
		if err != nil {
			e.log.Warn("engine: read drift converge flag", "app", app.Name, "error", err)
			continue
		}
		if !on {
			continue
		}
		if _, err := e.convergeApp(ctx, app.ID, app.Name, "system"); err != nil {
			e.log.Warn("engine: auto converge failed (keeps drifting silently)", "app", app.Name, "error", err)
			continue
		}
		e.driftSeen.clear(app.ID) // 收敛成功：下一拍重新基线
	}
}

// reportDrift 落 drift_detected 事件与审计（同事务 fail-closed；diff 为
// 投影形态 JSON——env 只到键名 + hash）。事件+审计配对经 disclosure.go
// 单点（discloseTx）。
func (e *Engine) reportDrift(ctx context.Context, appID, appName string, report *DriftReport) error {
	diffRaw, err := canonicalJSON(report.Services)
	if err != nil {
		diffRaw = []byte("[]")
	}
	e.log.Warn("engine: drift detected", "app", appName, "deployment", report.DesiredDeployment,
		"services", len(report.Services))
	return e.discloseTx(ctx, disclosure{
		eventCode:   "reconcile.drift_detected",
		subject:     "app:" + appName,
		payload:     []string{"app", appName, "app_id", appID, "desired_deployment", report.DesiredDeployment, "diff", string(diffRaw)},
		auditAction: "reconcile.drift_detected",
		auditTarget: "app:" + appName,
		auditDiff:   string(diffRaw),
	})
}

// ConvergeApp 人工一次性收敛（CLI 入口）：按当前期望态（最近 succeeded
// deployment 快照）执行归位重放原语并写审计。在途部署存在时拒绝
// （409 语义——发布本身就是收敛过程）。
func (e *Engine) ConvergeApp(ctx context.Context, appName, actor string) (state.DeployRecord, error) {
	app, err := e.store.GetAppByName(ctx, appName)
	if err != nil {
		return state.DeployRecord{}, err
	}
	has, err := e.store.AppHasNonTerminalDeployment(ctx, app.ID)
	if err != nil {
		return state.DeployRecord{}, err
	}
	if has {
		return state.DeployRecord{}, apperrConflict(
			"app %s has a deployment in flight: wait for it to reach a terminal state before converging", app.Name)
	}
	if actor == "" {
		actor = "human"
	}
	return e.convergeApp(ctx, app.ID, app.Name, actor)
}

// convergeApp 执行收敛（共享原语）：期望态快照 → restoreSnapshot（确定性、
// 幂等、禁 --force）→ 审计。重放前把活覆盖的副本数钉进期望（红线②：收敛
// 不回滚 autoscaler 的平台写——回滚旧 revision 的路径不经此处，覆盖不生效
// 是其正确语义）。
func (e *Engine) convergeApp(ctx context.Context, appID, appName, actor string) (state.DeployRecord, error) {
	source, specs, err := e.lastSucceededSpecs(ctx, appID)
	if err != nil {
		return state.DeployRecord{}, err
	}
	if source == nil {
		return state.DeployRecord{}, fmt.Errorf("engine: app %s has no succeeded deployment to converge to", appName)
	}
	overrides, oerr := e.store.ListScalingReplicaOverrides(ctx, appID)
	if oerr != nil {
		return state.DeployRecord{}, oerr
	}
	pinScalingReplicaOverrides(specs, overrides, source.ID)
	if err := e.restoreSnapshot(ctx, *source, specs); err != nil {
		return state.DeployRecord{}, err
	}
	if err := e.store.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       actor,
			Action:      "reconcile.converge",
			Target:      "app:" + appName,
			Result:      "ok",
			DiffSummary: state.DiffSummary("deployment", source.ID, "desired_hash", source.DesiredHash), // MG-6：构造器替换手拼 JSON
		})
	}); err != nil {
		return state.DeployRecord{}, err
	}
	return *source, nil
}

// SetDriftConverge 置位/清除收敛 opt-in（人工重置入口；带审计，actor 透传）。
func (e *Engine) SetDriftConverge(ctx context.Context, appName string, on bool, actor string) error {
	app, err := e.store.GetAppByName(ctx, appName)
	if err != nil {
		return err
	}
	if actor == "" {
		actor = "human"
	}
	return e.store.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.SetAppDriftConverge(ctx, app.ID, on); err != nil {
			return err
		}
		action := "reconcile.drift_converge_enabled"
		if !on {
			action = "reconcile.drift_converge_disabled"
		}
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:       actor,
			Action:      action,
			Target:      "app:" + app.Name,
			Result:      "ok",
			DiffSummary: state.DiffSummary("enabled", on), // MG-6：构造器替换手拼 JSON（布尔保持原生字面量）
		})
	})
}

// GetDriftConverge 读取收敛 opt-in 位（CLI show 面）。
func (e *Engine) GetDriftConverge(ctx context.Context, appName string) (bool, error) {
	app, err := e.store.GetAppByName(ctx, appName)
	if err != nil {
		return false, err
	}
	return e.store.GetAppDriftConverge(ctx, app.ID)
}

// apperrConflict 是 409 语义信封（在途部署拒绝收敛）。
func apperrConflict(format string, args ...any) error {
	return errorf("E_STATE_VERSION_CONFLICT", format, args...)
}
