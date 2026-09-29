package compose

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/sirupsen/logrus"

	"github.com/fleetlyrun/fleetly/internal/apperr"
)

// Load 读取并解析 compose 文件，执行受控子集校验后返回归一化 Spec 与
// 警告标注（警告不阻断校验：W_DEPLOY_NO_HEALTHCHECK 等）。任何失败都经
// apperr 携带注册表错误码（suggestion/docs 由注册表默认带出）。
func Load(ctx context.Context, path string) (*Spec, []Warning, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED", "failed to resolve compose path %s: %v", path, err).WithCause(err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED", "cannot read compose file %s: %v", path, err).WithCause(err)
	}

	details := types.ConfigDetails{
		WorkingDir:  filepath.Dir(abs),
		Environment: map[string]string{}, // 插值关闭：环境查找恒空（字面值纪律）
		ConfigFiles: []types.ConfigFile{{Filename: abs}},
	}

	// compose-go 会经 logrus 输出运行时告警（如 version obsolete）——CLI
	// 的 stdout/stderr 契约（--json 纯净度）由调用方持有，库内不外溢：
	// 解析期间静默 logrus（其信息价值已由 schema 校验与白名单覆盖）。
	quiet := logrus.StandardLogger()
	savedOut, savedLevel := quiet.Out, quiet.GetLevel()
	quiet.SetOutput(io.Discard)
	quiet.SetLevel(logrus.PanicLevel)
	defer func() {
		quiet.SetOutput(savedOut)
		quiet.SetLevel(savedLevel)
	}()

	dict, err := loader.LoadModelWithContext(ctx, details,
		withSubsetOptions(),
	)
	if err != nil {
		return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED", "failed to parse compose (%s): %v", filepath.Base(abs), err).WithCause(err)
	}

	if err := validateDict(abs, dict); err != nil {
		return nil, nil, err
	}

	// ModelToProject 会 delete(dict, "name") 且其内部 projectName 不可注入
	// （无导出选项）——应用标识由本包自行提取与校验。
	specName, _ := dict["name"].(string)

	opts := loader.ToOptions(&details, []func(*loader.Options){withSubsetOptions()})
	project, err := loader.ModelToProject(dict, opts, details)
	if err != nil {
		return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED", "failed to parse compose (%s): %v", filepath.Base(abs), err).WithCause(err)
	}

	spec, warnings, err := normalize(abs, project)
	if err != nil {
		return nil, nil, err
	}
	spec.Name = specName
	if err := spec.hashSpec(); err != nil {
		return nil, nil, apperr.New("E_COMPOSE_UNSUPPORTED", "failed to compute normalized hash: %v", err).WithCause(err)
	}
	return spec, warnings, nil
}

// withSubsetOptions 是本包对 compose-go loader 的固定策略（理由见包注释
// 「解析器选型」）。
func withSubsetOptions() func(*loader.Options) {
	return func(o *loader.Options) {
		o.SkipInterpolation = true      // ${VAR}/.env 插值关闭，归一化按字面（§2.4）
		o.SkipInclude = true            // include 在拒绝清单：保留原始键交由白名单显式拒绝
		o.SkipExtends = true            // extends 同上（loader 默认会先消解 extends 键）
		o.ResolvePaths = false          // 路径保持书写形态，spec_hash 跨机稳定
		o.SkipResolveEnvironment = true // env_file 合并由本包做（保留来源标注）
		o.SkipResolveLabels = true      // label_file 不在支持清单（白名单拒绝）
		// SkipNormalization：compose 的 Normalize 会注入默认网络
		//（networks.default.name = <project>_default）与卷/secret 资源名——
		// 平台命名纪律（服务/网络/卷名由平台按 app 命名）不容 parse 层预写；
		// 代价是顶层 name 成为必填（loader 在 schema 校验开启时对空项目名
		// 报错），与平台「compose name = 应用标识」契约一致。
		o.SkipNormalization = true
		// SkipDefaultValues：缺省值注入会把 build.dockerfile 补成
		// "Dockerfile"——而 §2.4 的构建模式裁决是「无 dockerfile → Railpack，
		// 有 → Dockerfile」，注入会误判全部服务为 Dockerfile 模式。
		o.SkipDefaultValues = true
	}
}

// LoadEmpty 返回空基线 Spec（首部署语义：一切服务视为新增）。plan 在未
// 提供 --baseline 时使用；DB 基线（上一 revision 快照）随引擎票接入。
func LoadEmpty(name string) *Spec {
	return &Spec{Name: name}
}

// warnings 是校验期产生的非阻断标注集合（保持出现顺序）。
type warnings struct{ items []Warning }

func (w *warnings) add(item Warning) { w.items = append(w.items, item) }

// Warning 是校验/计划产物上的非阻断标注。Code 为注册表 W_ 码（如
// W_DEPLOY_NO_HEALTHCHECK）；无注册码的提示（如 cron label 的 v0.2 生效
// 提示——v0.1 冻结注册表不含对应码，不发明新码）Code 留空、以 Kind 给出
// 稳定标识。
type Warning struct {
	Code    string `json:"code,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Service string `json:"service,omitempty"`
	Message string `json:"message"`
}

// 警告 Kind 常量（无注册码提示的稳定标识）。
const (
	// WarningKindUserLabelNotPassed：非 fleetly.* 的服务 label 平台不透传
	//（v0.1 受控子集只消费平台约定 label；S16-C2——静默丢弃改警告披露）。
	WarningKindUserLabelNotPassed = "user_label_not_passed" //nolint:gosec // G101：警告 Kind 枚举值，非凭据
	// WarningKindCronServiceScheduled：服务带 fleetly.cron 声明（E5 Cron）
	// ——只声明不部署长驻服务的 plan 披露口径（调度器按点建一次性 job；
	// 无注册 W 码，Kind 标识）。
	WarningKindCronServiceScheduled = "cron_service_scheduled"
	// WarningKindInitJobDeclared：服务带 fleetly.job 声明（DT-4）——只声明
	// 不部署长驻服务的 plan 披露口径（发布管线在晋级前建一次性 job 跑
	// init 语义；无注册 W 码，Kind 标识）。
	WarningKindInitJobDeclared = "init_job_declared"
)

// errCompose 构造带路径上下文的 E_COMPOSE_UNSUPPORTED。
func errCompose(format string, args ...any) *apperr.Error {
	return apperr.New("E_COMPOSE_UNSUPPORTED", format, args...)
}
