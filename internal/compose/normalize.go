package compose

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/fleetlyrun/fleetly/internal/apperr"
)

// normalize 把 compose-go typed 工程转换为归一化 Spec，并执行需要 typed
// 信息的第二层校验：
//   - 平台 label 契约（domains/placement/cron/s3/fleetly.* 保留前缀）；
//   - 危险挂载语义（宿主 bind、docker.sock——dict 层白名单之外的补刀）；
//   - 有卷服务 replicas 校验（本地卷不能多副本共享，stateful-placement
//     §2.3）与 cron 服务 replicas=0/省略契约（E5 Cron，架构 §4.3）；
//   - env_file 合并（来源标注 env_file < environment）。
//
// 同时产出非阻断警告（无 healthcheck、无卷显式钉节点的计划警告在 plan
// 层产出）。
func normalize(abs string, project *types.Project) (*Spec, []Warning, error) {
	workDir := filepath.Dir(abs)
	ws := &warnings{}

	// ── 平台 label 契约（跨服务判定先收集，逐服务判定即查即报）──
	serviceDomains := map[string][]string{}
	placementRefs := map[string]string{}

	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	services := make([]Service, 0, len(names))
	for _, name := range names {
		svc := project.Services[name]
		prefix := "services." + name
		// fleetly.* 保留前缀：约定键之外的占用 → E_LABEL_RESERVED（422）。
		// 非 fleetly.* 用户 label：平台不透传（v0.1 只消费平台约定 label）——
		// S16-C2：静默丢弃改为 W 级警告披露（每服务一条，列出键集）。
		var userLabels []string
		for _, k := range sortedStringKeys(svc.Labels) {
			if !strings.HasPrefix(k, LabelNamespace) {
				userLabels = append(userLabels, k)
				continue
			}
			if !knownFleetlyLabels[k] {
				return nil, nil, apperr.New("E_LABEL_RESERVED",
					"service %q uses the reserved platform label %q (reserved namespace %s*, convention keys such as %s)",
					name, k, LabelNamespace, strings.Join(sortedStringKeys(knownFleetlyLabels), ", ")).
					WithContext("path", prefix+".labels."+k)
			}
		}
		if len(userLabels) > 0 {
			ws.add(Warning{
				Kind:    WarningKindUserLabelNotPassed,
				Service: name,
				Message: "service " + name + " declares non-platform labels (" + strings.Join(userLabels, ", ") + "): the platform does not pass through user labels (v0.1) and only consumes fleetly.* platform convention keys",
			})
		}

		// domains label（有该 label 的服务即入口）。
		var domains []string
		if v, ok := svc.Labels[LabelDomains]; ok {
			parsed, err := parseDomainsLabel(name, v)
			if err != nil {
				return nil, nil, err
			}
			domains = parsed
		}
		serviceDomains[name] = domains

		// s3 label（E3-4 凭证注入开关）：值契约 = 字面 "true"；非法值在
		// 解析期即拒（fail-loud，不静默当未启用）。
		s3 := false
		if v, ok := svc.Labels[LabelS3]; ok {
			if err := parseS3Label(name, strings.TrimSpace(v)); err != nil {
				return nil, nil, err
			}
			s3 = true
		}

		// databases label（E4 托管数据库引用声明，managed-databases §2.4/
		// D-DB-4）：逗号分隔实例名 → trim/排序归一化；形态违规（空条目/
		// 字符集/重复）在解析期即拒（fail-loud，同 parseS3Label 纪律）。
		var databases []string
		if v, ok := svc.Labels[LabelDatabases]; ok {
			parsed, err := parseDatabasesLabel(name, v)
			if err != nil {
				return nil, nil, err
			}
			databases = parsed
		}

		// placement label：语法 + 跨服务一致性。
		if v, ok := svc.Labels[LabelPlacementNode]; ok {
			placementRefs[name] = strings.TrimSpace(v)
		}

		// cron label 家族（E5 Cron，架构 §4.3 声明行）：值契约在归一化期
		// 校验（表达式五段标准式/时区/超时；fail-loud 即拒），主 label 存在
		// 时 service.Cron 非空——发布引擎跳过其长驻装配，调度器按点建一次性
		// job。孤儿时区/超时 label（无 fleetly.cron）同样拒绝：静默无机会
		// 生效的声明是「以为配了」的悬案，与 parseS3Label 同纪律。
		var cronSchedule *CronSchedule
		if _, ok := svc.Labels[LabelCron]; ok {
			cs, err := parseCronSchedule(name, svc.Labels)
			if err != nil {
				return nil, nil, err
			}
			cronSchedule = cs
			// replicas 契约（架构 §4.3）：cron 服务是一次性 job，replicas
			// 必须 0/省略——>0 声明的是平台无法兑现的期望（job 模式无长驻
			// 副本语义），E_COMPOSE_UNSUPPORTED reason 细分在 Load 期即拒。
			if svc.Deploy != nil && svc.Deploy.Replicas != nil && *svc.Deploy.Replicas > 0 {
				return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED",
					"service %q declares deploy.replicas=%d together with the %q label (cron services run as one-shot jobs: replicas must be 0 or omitted)",
					name, *svc.Deploy.Replicas, LabelCron).
					WithContext("path", prefix+".deploy.replicas").
					WithContext("reason", "cron_replicas")
			}
		} else {
			for _, k := range []string{LabelCronTimezone, LabelCronTimeout} {
				if _, ok := svc.Labels[k]; ok {
					return nil, nil, apperr.New("E_LABEL_RESERVED",
						"service %q declares label %q without %q (timezone/timeout refine a cron schedule; without the schedule label they would never take effect)",
						name, k, LabelCron).
						WithContext("path", prefix+".labels."+k).
						WithContext("reason", "cron_without_schedule")
				}
			}
		}

		// job label 家族（DT-4 部署期一次性作业）：值词表仅 init（fail-loud
		// 解析期拒绝，见 parseJobLabels），与 fleetly.cron 互斥；typed 层补
		// 两条契约——expose 禁令（expose 是路由目标端口声明，job 不产出长驻
		// 监听面）与 replicas 契约（一次性 job 无长驻副本语义，>0 声明的是
		// 平台无法兑现的期望；cron 同型）。
		initJob, initJobTimeout, err := parseJobLabels(name, svc.Labels)
		if err != nil {
			return nil, nil, err
		}
		if initJob {
			if len(svc.Expose) > 0 {
				return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED",
					"service %q declares both %q and expose (an init job is a one-shot process with no long-running listener to route to; expose is the routing target port declaration)",
					name, LabelJob).
					WithContext("path", prefix+".expose").
					WithContext("reason", "init_job_expose")
			}
			if svc.Deploy != nil && svc.Deploy.Replicas != nil && *svc.Deploy.Replicas > 0 {
				return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED",
					"service %q declares deploy.replicas=%d together with the %q label (init jobs run as one-shot jobs: replicas must be 0 or omitted)",
					name, *svc.Deploy.Replicas, LabelJob).
					WithContext("path", prefix+".deploy.replicas").
					WithContext("reason", "init_job_replicas")
			}
		}

		normalized, err := normalizeService(workDir, name, &svc, ws)
		if err != nil {
			return nil, nil, err
		}
		normalized.Domains = domains
		normalized.PlacementNode = placementRefs[name]
		normalized.S3 = s3
		normalized.Cron = cronSchedule
		normalized.InitJob = initJob
		normalized.InitJobTimeout = initJobTimeout
		normalized.Databases = databases
		services = append(services, normalized)
	}

	if err := checkDomainContracts(serviceDomains); err != nil {
		return nil, nil, err
	}
	if err := checkPlacementLabel(placementRefs); err != nil {
		return nil, nil, err
	}

	// ── 顶层卷/网络/secret 归一化 ──
	var volumes []Volume
	for key, def := range project.Volumes {
		volumes = append(volumes, Volume{Key: key, Driver: def.Driver})
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Key < volumes[j].Key })

	var networks []string
	for net := range project.Networks {
		networks = append(networks, net)
	}
	sort.Strings(networks)

	var secrets []string
	for name := range project.Secrets {
		secrets = append(secrets, name)
	}
	sort.Strings(secrets)

	var configs []string
	for name := range project.Configs {
		configs = append(configs, name)
	}
	sort.Strings(configs)

	// 服务级 secret/config 引用的完整性哨兵在 dict 层（validateDict：声明面
	// 自洽先于 typed 解析——compose-go 的引用校验报错不带平台路径上下文）。

	// ── 无 healthcheck 警告（health_gate=none 显式降级，release-semantics
	// §2.8 健康门解析）──
	for i := range services {
		if services[i].Healthcheck == nil {
			ws.add(Warning{
				Code:    "W_DEPLOY_NO_HEALTHCHECK",
				Service: services[i].Name,
				Message: "service " + services[i].Name + " declares no healthcheck: health_gate=none (the health gate degrades to exit/replica-count checks, so failures surface later)",
			})
		}
	}

	spec := &Spec{
		Name:     project.Name,
		Services: services,
		Volumes:  volumes,
		Networks: networks,
		Secrets:  secrets,
		Configs:  configs,
	}
	return spec, ws.items, nil
}

// normalizeService 归一化单个服务（不含 domains/placement——由调用方回填）。
func normalizeService(workDir, name string, svc *types.ServiceConfig, ws *warnings) (Service, error) {
	out := Service{Name: name}

	if svc.Build != nil {
		out.Build = &Build{Context: svc.Build.Context, Dockerfile: svc.Build.Dockerfile}
	}
	out.Image = svc.Image
	out.Command = append([]string{}, svc.Command...)
	if len(svc.Expose) > 0 {
		out.Expose = append([]string{}, svc.Expose...)
	}
	if svc.HealthCheck != nil {
		hc := &Healthcheck{Test: append([]string{}, svc.HealthCheck.Test...)}
		if svc.HealthCheck.Interval != nil {
			hc.Interval = svc.HealthCheck.Interval.String()
		}
		if svc.HealthCheck.Timeout != nil {
			hc.Timeout = svc.HealthCheck.Timeout.String()
		}
		if svc.HealthCheck.Retries != nil {
			hc.Retries = *svc.HealthCheck.Retries
		}
		if svc.HealthCheck.StartPeriod != nil {
			hc.StartPeriod = svc.HealthCheck.StartPeriod.String()
		}
		out.Healthcheck = hc
	}

	// env 合并链：env_file < environment（架构 §2.4 变量合并行；平台层随
	// env 票接入）。值只以 sha256 表示。
	env, err := mergeEnvironment(workDir, name, svc)
	if err != nil {
		return out, err
	}
	out.Environment = env

	for _, s := range svc.Secrets {
		target := s.Target
		if target == "" {
			target = s.Source
		}
		out.Secrets = append(out.Secrets, ServiceSecret{Source: s.Source, Target: target})
	}
	sort.Slice(out.Secrets, func(i, j int) bool { return out.Secrets[i].Source < out.Secrets[j].Source })

	// config 挂载（T 线 OT-3）：target 必填（dict 层已拒短语法与缺失
	// target——平台不默认 /<source>）；只读与 uid/gid/mode 平台受管。
	for _, cfg := range svc.Configs {
		out.Configs = append(out.Configs, ServiceConfig{Source: cfg.Source, Target: cfg.Target})
	}
	sort.Slice(out.Configs, func(i, j int) bool { return out.Configs[i].Source < out.Configs[j].Source })

	for _, v := range svc.Volumes {
		// 危险挂载语义（Coolify CVE-2025-34159 根因类）：宿主 bind 与
		// docker.sock 一律拒绝（admin 显式开启 + 审计为后续票 TODO）。
		if v.Type == "bind" {
			return out, errCompose("volume mount %s:%s of service %q is a host-path bind (dangerous fields are denied by default; use named volumes for data persistence)", name, v.Source, v.Target).
				WithContext("path", "services."+name+".volumes").
				WithContext("reason", "host_bind")
		}
		if isDockerSockTarget(v.Target) {
			return out, errCompose("volume mount target %s of service %q hits the Docker daemon socket (dangerous fields are denied by default)", name, v.Target).
				WithContext("path", "services."+name+".volumes").
				WithContext("reason", "docker_sock")
		}
		out.Volumes = append(out.Volumes, Mount{Volume: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
	}

	for net := range svc.Networks {
		out.Networks = append(out.Networks, net)
	}
	sort.Strings(out.Networks)

	if svc.Deploy != nil {
		// 有命名卷服务 replicas 上限（stateful-placement §2.3：本地卷不能
		// 多副本共享——双任务并发挂同一卷有数据风险；replicas 0/1 合法，
		// 上限校验收敛在 >1，省略 = 平台按 1 处理）。
		if r := svc.Deploy.Replicas; r != nil && *r > 1 && len(svc.Volumes) > 0 {
			return out, errCompose("service %q mounts named volumes with replicas=%d (local volumes cannot be shared across replicas; replicas must be ≤1)", name, *r).
				WithContext("path", "services."+name+".deploy.replicas")
		}
		out.Deploy = normalizeDeploy(svc.Deploy)
	}
	out.StopSignal = svc.StopSignal
	if svc.StopGracePeriod != nil {
		out.StopGracePeriod = svc.StopGracePeriod.String()
	}
	return out, nil
}

// normalizeDeploy 归一化 deploy 段（受管字段 failure_action/monitor 校验已
// 通过，不进快照——平台常量；order 及其余字段照用）。
func normalizeDeploy(d *types.DeployConfig) *Deploy {
	out := &Deploy{}
	if d.Mode != "" {
		out.Mode = d.Mode
	}
	if d.Replicas != nil {
		out.Replicas = int64(*d.Replicas)
	}
	if uc := d.UpdateConfig; uc != nil {
		u := &UpdateConfig{Order: uc.Order, Parallelism: valueOrZero(uc.Parallelism)}
		if uc.Delay != 0 {
			u.Delay = uc.Delay.String()
		}
		out.UpdateConfig = u
	}
	if rp := d.RestartPolicy; rp != nil {
		out.RestartPolicy = &RestartPolicy{
			Condition:   rp.Condition,
			MaxAttempts: valueOrZero(rp.MaxAttempts),
		}
		if rp.Delay != nil && *rp.Delay != 0 {
			out.RestartPolicy.Delay = rp.Delay.String()
		}
		if rp.Window != nil && *rp.Window != 0 {
			out.RestartPolicy.Window = rp.Window.String()
		}
	}
	if d.Resources.Limits != nil {
		out.Resources = &Resources{Limits: &ResourceLimits{
			CPUS:        float64(d.Resources.Limits.NanoCPUs),
			MemoryBytes: int64(d.Resources.Limits.MemoryBytes),
		}}
	}
	if len(d.Placement.Constraints) > 0 {
		out.Placement = &Placement{Constraints: append([]string{}, d.Placement.Constraints...)}
	}
	return out
}

// mergeEnvironment 按 env_file < environment 合并并产出排序后的
// key+hash+source 列表。
func mergeEnvironment(workDir, name string, svc *types.ServiceConfig) ([]EnvVar, error) {
	merged := map[string]EnvVar{}
	for _, ef := range svc.EnvFiles {
		p := ef.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		content, err := os.ReadFile(p) //nolint:gosec // env_file 为用户 compose 显式声明的读取对象（校验已定路径形态）
		if err != nil {
			if !bool(ef.Required) {
				continue // required: false 显式缺省容忍
			}
			return nil, errCompose("env_file %s of service %q is not readable: %v", name, ef.Path, err).
				WithContext("path", "services."+name+".env_file")
		}
		kvs, err := parseEnvFile(name, ef.Path, content)
		if err != nil {
			return nil, err
		}
		for k, v := range kvs {
			merged[k] = EnvVar{Key: k, Hash: sha256Hex(v), Source: EnvSourceEnvFile}
		}
	}
	for k, v := range svc.Environment {
		if v == nil {
			// dict 层已拒绝裸键；typed 兜底（防御性，理论不可达）。
			return nil, errCompose("environment entry %q of service %q has no literal value", name, k).
				WithContext("path", "services."+name+".environment."+k)
		}
		merged[k] = EnvVar{Key: k, Hash: sha256Hex(*v), Source: EnvSourceEnvironment}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, merged[k])
	}
	return out, nil
}

// isDockerSockTarget 判定挂载目标是否命中 Docker 守护进程套接字（含其
// 父目录挂载）。
func isDockerSockTarget(target string) bool {
	t := filepath.ToSlash(target)
	return t == "/var/run/docker.sock" || strings.HasPrefix(t, "/var/run/docker.sock/")
}

// valueOrZero 解 nil 指针为零值（归一化输出不允许指针）。
func valueOrZero[T any](p *T) T {
	var zero T
	if p != nil {
		return *p
	}
	return zero
}

// sortedStringKeys 返回映射键排序切片（泛型工具）。
func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedKeys 是 sortedStringKeys 的别名（validate.go 用）。
func sortedKeys[V any](m map[string]V) []string { return sortedStringKeys(m) }
