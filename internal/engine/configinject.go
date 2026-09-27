package engine

// config 挂载解析与装载（T 线 OT-3 / IMPL-T1-4 Config 资源）：compose 服务级
// configs 引用（source ∈ 顶层 external 声明，校验层已收窄）→ 发布引擎
// preparing 期按 app_configs 装载三件事——
//
//  1. 存在性哨兵（plan-time fail-fast）：声明名在 app_configs 缺失 →
//     E_CONFIG_NOT_FOUND（422，点名全部缺失名——校验层只保证声明面自洽，
//     值的存在性在引擎侧权威，与 E_SECRET_NOT_FOUND 同分层）；
//  2. Swarm config 确保：naming.ConfigName(team,prj,app,name,hash8(内容))
//     ——内容变更即换名换引用（引用进 desired-hash）；底座对象缺失时以
//     明文内容创建（ConfigEnsurer 端口，实现 = substrate.Client；在服务
//     create/update 之前在位——ConfigReference 需要底座对象 ID，W3 真机
//     教训同族；IMPL-T1-4 真机探针矩阵实证 ID/Name/File 三必填）；
//  3. 注入：ConfigMount{ConfigName, Target} 进 ServiceSpec.Configs——值
//     零进规划产物（快照只带对象名）；target 是显式绝对路径（校验层已收窄，
//     恒只读）。
//
// 生效语义（OT-3）：config 值变更 = 换 hash8 换名 → 引用与 desired-hash
// 变化 → 随**下次部署**换挂并触发服务滚动；旧对象回收 = applyDesired 的
// keep-set GC（同一收敛拍 best-effort；真机实证：引用换版后 daemon 即允许
// 删除，in-use 拒绝 = 留待下一拍），app 删除 reap 全清（secrets 同款）。
//
// 重放路径（回滚/归位/init 续跑）经 restoreSnapshot → applyDesired：快照的
// ConfigMount 按名对 app_configs 现值解析（ensureSnapshotConfigs）——名不
// 匹配（内容已换版）→ E_CONFIG_NOT_FOUND 诚实失败，不做静默改写（D-REL-9
// 快照不可变纪律，与 secret 重放路径同款）。

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ConfigEnsurer 是引擎对 Swarm config 对象的确保端口（OT-3 注入链的底座
// 原语；实现 = internal/substrate.Client，单测注入假实现）。幂等语义由实现
// 保证（inspect → 缺失才 create——名内嵌内容指纹，「存在性」判据即幂等）；
// data 只进创建载荷，实现方绝不记日志/拼错误。
type ConfigEnsurer interface {
	// EnsureConfig 确认 Swarm config 在位并返回其对象 ID；缺失时以 data
	// 创建（labels 为归属标注——清场/识别的选择器锚）。
	EnsureConfig(ctx context.Context, name string, data []byte, labels map[string]string) (string, error)
}

// ConfigReaper 是引擎对 Swarm config 对象的清场端口（换版旧对象 GC 与 app
// 删除 reap 的扫尾面；实现 = substrate.Client，WithConfigReaper 注入）。
// nil = 未接线——清场如实跳过并告警，不阻塞部署/删除收敛。
type ConfigReaper interface {
	// ConfigList 按 label 选择器返回 config 对象名（清场选择面）。
	ConfigList(ctx context.Context, labels map[string]string) ([]string, error)
	// ConfigRemove 删除 config（幂等：缺失视为成功；in-use 返回错误——
	// best-effort 清场留待下一拍重试）。
	ConfigRemove(ctx context.Context, name string) error
}

// WithConfigEnsurer 注入 Swarm config 对象的确保端口（实现 = substrate.Client，
// 装配层接线）。
func (e *Engine) WithConfigEnsurer(c ConfigEnsurer) *Engine { e.configEnsure = c; return e }

// WithConfigReaper 注入 Swarm config 对象的清场端口（实现 = substrate.Client，
// 装配层接线）。
func (e *Engine) WithConfigReaper(r ConfigReaper) *Engine { e.configReap = r; return e }

// resolveConfigMounts 解析本次发布的 config 挂载面（preparing 现读 app_configs，
// 不缓存长驻）。返回：服务 → ConfigMount 列表（按 target 字典序——desired-hash
// 确定性）；无任何声明的 app 返回 nil（零写入零底座副作用）。声明缺失 →
// E_CONFIG_NOT_FOUND（点名全部缺失名，一次暴露全量缺口）。
func (e *Engine) resolveConfigMounts(ctx context.Context, app state.App, spec *compose.Spec) (map[string][]ConfigMount, error) {
	// 声明收集（跨服务去重：同一 source 多服务引用 = 一次读取一次 ensure）。
	var sources []string
	for i := range spec.Services {
		for _, ref := range spec.Services[i].Configs {
			sources = append(sources, ref.Source)
		}
	}
	if len(sources) == 0 {
		return nil, nil
	}
	sort.Strings(sources)
	sources = dedupeStrings(sources)

	// 确保端口未接线（装配缺失）：带声明的部署规划期显式失败——静默跳过
	// 会产出「部署成功但目标路径缺文件」的悬案（secret 同款纪律）。
	if e.configEnsure == nil {
		return nil, errorf("E_RUNTIME_UNAVAILABLE",
			"service(s) declare compose configs but the config ensurer port is not wired (assembly bug: the engine requires a ConfigEnsurer for config declarations)")
	}
	appLabel := app.QualifiedName()

	// 存在性哨兵：全量缺口一次点名（可行动面——逐个修比逐轮部署撞墙诚实）。
	var missing []string
	valueBySource := make(map[string]string, len(sources))
	for _, source := range sources {
		row, err := e.store.GetAppConfig(ctx, app.ID, source)
		if err != nil {
			if !errors.Is(err, state.ErrAppConfigNotFound) {
				return nil, errorf("E_RUNTIME_UNAVAILABLE", "failed to read config %s from the platform config store: %v", source, err)
			}
			missing = append(missing, source)
			continue
		}
		valueBySource[source] = row.Value
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, apperr.New("E_CONFIG_NOT_FOUND",
			"config(s) %s declared by the compose file are not in the platform config store (set them with: fleetly configs set %s <name> --value <value>)",
			strings.Join(missing, ", "), app.Name).
			WithContext("missing", strings.Join(missing, ",")).
			WithContext("app", app.Name)
	}

	mounts := make(map[string][]ConfigMount)
	ensured := make(map[string]string, len(sources)) // source → 底座对象 ID
	for i := range spec.Services {
		svc := &spec.Services[i]
		if len(svc.Configs) == 0 {
			continue
		}
		list := make([]ConfigMount, 0, len(svc.Configs))
		for _, ref := range svc.Configs {
			value := valueBySource[ref.Source]
			configName, err := configNameFor(app.TeamSlug, app.ProjectSlug, app.Name, ref.Source, value)
			if err != nil {
				return nil, err
			}
			if _, ok := ensured[ref.Source]; !ok {
				id, eerr := e.configEnsure.EnsureConfig(ctx, configName, []byte(value), configLabels(appLabel))
				if eerr != nil {
					return nil, errorf("E_RUNTIME_UNAVAILABLE", "failed to ensure swarm config for %s: %v", ref.Source, eerr)
				}
				ensured[ref.Source] = id
			}
			list = append(list, ConfigMount{ConfigName: configName, Target: ref.Target})
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Target < list[b].Target })
		mounts[svc.Name] = list
	}
	return mounts, nil
}

// ensureSnapshotConfigs 为快照重放路径确保 config 底座对象在位（applyDesired
// 头部调用——回滚/归位/漂移收敛共用；发布路径的 ensure 已在 resolveConfigMounts
// 完成，此处幂等重入无害）。快照的挂载名必须能对 app_configs 现值解析（名 =
// fleetly-<team>-<prj>-<app>-config-<name>-<hash8>，内容换版即换名——不匹配
// = 内容已变更，挂载名悬空）：缺失解析 → E_CONFIG_NOT_FOUND。
func (e *Engine) ensureSnapshotConfigs(ctx context.Context, rec state.DeployRecord, specs []ServiceSpec) error {
	names := map[string]bool{}
	for i := range specs {
		for _, m := range specs[i].Configs {
			names[m.ConfigName] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	if e.configEnsure == nil {
		return errorf("E_RUNTIME_UNAVAILABLE",
			"snapshot carries config mounts but the config ensurer port is not wired (assembly bug)")
	}
	app, err := e.store.GetAppByID(ctx, rec.AppID)
	if err != nil {
		return errorf("E_RUNTIME_UNAVAILABLE", "failed to load app %s for config replay: %v", rec.AppID, err)
	}
	appLabel := app.QualifiedName()
	rows, err := e.store.ListAppConfigs(ctx, rec.AppID)
	if err != nil {
		return errorf("E_RUNTIME_UNAVAILABLE", "failed to read the platform config store for replay: %v", err)
	}
	valueByName := make(map[string]string, len(rows))
	for _, row := range rows {
		configName, serr := configNameFor(app.TeamSlug, app.ProjectSlug, app.Name, row.Name, row.Value)
		if serr != nil {
			return serr
		}
		valueByName[configName] = row.Value
	}
	for name := range names {
		value, ok := valueByName[name]
		if !ok {
			return apperr.New("E_CONFIG_NOT_FOUND",
				"snapshot config mount %s no longer resolves against the platform config store (the config value changed or was removed since this revision; redeploy to refresh mounts)", name).
				WithContext("config", name).
				WithContext("app", rec.AppName)
		}
		if _, err := e.configEnsure.EnsureConfig(ctx, name, []byte(value), configLabels(appLabel)); err != nil {
			return errorf("E_RUNTIME_UNAVAILABLE", "failed to ensure swarm config for replay: %v", err)
		}
	}
	return nil
}

// gcAppConfigs 清场本应用失引用的 config 对象（内容寻址换版后的旧对象；
// best-effort——服务在用的对象删除会被底座拒绝〔真机实证：in-use 按服务
// spec 引用判定〕，此处只清真正无引用的旧版，失败留待下一拍）。keep 为空
// 表示本次期望集不引用任何 config——全族清场（服务删光后的收敛形态）。
func (e *Engine) gcAppConfigs(ctx context.Context, appLabel string, keep map[string]bool) {
	if e.configReap == nil {
		e.log.Warn("engine: app config gc skipped (config reaper port not wired)", "app", appLabel)
		return
	}
	names, err := e.configReap.ConfigList(ctx, configLabels(appLabel))
	if err != nil {
		e.log.Warn("engine: app config gc list", "app", appLabel, "error", err)
		return
	}
	for _, name := range names {
		if keep[name] {
			continue
		}
		if err := e.configReap.ConfigRemove(ctx, name); err != nil {
			e.log.Warn("engine: app config gc remove", "app", appLabel, "config", name, "error", err)
			continue
		}
		e.log.Info("engine: stale app config removed (content-addressed rotation)", "app", appLabel, "config", name)
	}
}

// reapAppConfigs 收敛单个 deleting 应用的 Swarm config 残留（best-effort，
// reapAppSecrets 同族口径）：按归属 label 扫描 → 逐条移除；端口未接线/
// 扫描失败/单条移除失败都只落 warn 日志，绝不阻塞删除收敛。幂等：重复扫描
// 对已删除名零操作。
func (e *Engine) reapAppConfigs(ctx context.Context, appName string) {
	if e.configReap == nil {
		e.log.Warn("engine: deleting-app config sweep skipped (config reaper port not wired)",
			"app", appName)
		return
	}
	names, err := e.configReap.ConfigList(ctx, configLabels(appName))
	if err != nil {
		e.log.Warn("engine: deleting-app config scan", "app", appName, "error", err)
		return
	}
	for _, name := range names {
		if err := e.configReap.ConfigRemove(ctx, name); err != nil {
			e.log.Warn("engine: deleting-app config remove", "app", appName,
				"config", name, "error", err)
			continue
		}
		e.log.Info("engine: app swarm config removed (app delete reap)",
			"app", appName, "config", name)
	}
}

// configNameFor 由配置资源名与内容构造 Swarm config 名（内容变更即换名换
// 引用，OT-3 逐字兑现 naming.ConfigName；v0.3 三段形——team/prj 段进公式）。
func configNameFor(team, prj, app, name, value string) (string, error) {
	configName, err := naming.ConfigName(team, prj, app, name, naming.Hash8(value))
	if err != nil {
		return "", errorf("E_RUNTIME_UNAVAILABLE", "naming failed for config %s: %v", name, err)
	}
	return configName, nil
}

// configKeepSet 汇总本次收敛应保留的 config 对象名：期望长驻服务集 + 快照
// 内的全部模板（含 Job——init job 引用的 config 对象在其服务清场后仍属
// 本 revision 的期望引用面，避免每次部署的无谓「删了再建」；快照解码失败
// 只退化为长驻集——多余对象下一拍仍会被清，方向安全）。
func (e *Engine) configKeepSet(rec state.DeployRecord, desired []ServiceSpec) map[string]bool {
	keep := map[string]bool{}
	for i := range desired {
		for _, m := range desired[i].Configs {
			keep[m.ConfigName] = true
		}
	}
	all, err := e.decodeAllSpecs(rec)
	if err != nil {
		e.log.Warn("engine: config keep-set snapshot decode failed (using long-running set only)",
			"deployment", rec.ID, "error", err)
		return keep
	}
	for i := range all {
		for _, m := range all[i].Configs {
			keep[m.ConfigName] = true
		}
	}
	return keep
}

// configLabels 构造 app config 的归属 label 集（清场/识别的选择器锚——内容
// 换版换名后旧对象的 best-effort 清场按 fleetly.app 选择器扫描；值 = 三段
// 限定形）。
func configLabels(qualifiedApp string) map[string]string {
	return map[string]string{
		state.LabelManaged: state.ManagedLabelValue,
		state.LabelApp:     qualifiedApp,
	}
}
