// fleetly CLI：fleetly 平台命令行（lynx-go/commands 动词注册，D21）。
//
// T2.18 CLI-over-SDK 改造：CLI 与 API 同源——全部平台动词只经 SDK（gRPC）
// 消费 fleetlyd（连接参数 --addr/--token，env 覆盖 FLEETLY_ADDR/
// FLEETLY_TOKEN；W1-S4 起 token 另有 config 回落尾环——auth login 落盘的
// ~/.fleetly/config.yaml，读序 flag > env > config，见 resolve.go），不再
// 有任何直开 DB / 直连 docker / 直读密钥的路径。
// 纯本地解析保留在 validate/plan--baseline/diff（internal/compose 纯库）。
// 全动词支持 --json；退出码四态（S17-D3）：0=成功/无变化、1=错误、
// 2=有变化（仅 plan/diff）、64=用法错误。
package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/lynx-go/commands"
)

// NewApp 组装 CLI（测试同路径复用：直接对 App.Run 传参断言退出码，
// 不起子进程、不触碰 os.Args——lynx-go/commands 不解析进程参数，无
// fleetlyd Runner 的 os.Args/CWD 隔离问题）。version 为 CLI 自身版本
// （main 进程经 -ldflags "-X main.version=..." 注入后传入，未注入为 dev）。
func NewApp(version string) *commands.App {
	app := commands.New()
	app.Register(
		&versionCmd{version: version},
		&validateCmd{},
		&planCmd{},
		&diffCmd{},
		newAppsCmd(),
		&buildCmd{},
		newBuildsCmd(),
		&deployCmd{},
		newDeploymentsCmd(),
		&rollbackCmd{},
		newRevisionsCmd(),
		newDriftCmd(),
		newEnvCmd(),
		newLogsCmd(),
		newMetricsCmd(),
		newAlertsCmd(),
		newNotificationsCmd(),
		newEventsCmd(),
		newTokensCmd(),
		newAuthCmd(),
		newAuditCmd(),
		newPlacementCmd(),
		newNodesCmd(),
		newVolumesCmd(),
		newDomainsCmd(),
		newIngressCmd(),
		newAcmeCmd(),
		newS3Cmd(),
		newRegistryCmd(),
		newBackupsCmd(),
		newCronCmd(),
		newDatabasesCmd(),
		newSecretsCmd(),
		newConfigsCmd(),
		newProjectsCmd(),
		newTasksCmd(),
	)
	// 退出码四态（架构 §2.4 plan/apply 语义 + S17-D3）：0=无变化/成功、
	// 2=有变化（仅 plan/diff）、1=错误、64=用法错误（EX_USAGE 惯例）。
	app.ExitCode = exitCodeFor
	app.RenderError = renderCLIError
	app.HelpHeader = "fleetly — fleetly platform CLI (" + version + ")"
	app.VerbTitle = "verbs:"
	return app
}

// exitUsage 是用法类错误（未知动词/flag 解析失败/位置参数违规）的退出码
// （S17-D3）：64 = sysexits.h 的 EX_USAGE 惯例。与 plan/diff「检测到变化」
// 的 2 分离——Agent/脚本据此区分"有漂移"与"调用姿势错误"。
const exitUsage = 64

// exitCodeFor 是退出码裁决钩子：errChanges（有变化）→ 2；用法类 → 64；
// 其余 → 1。
func exitCodeFor(err error) int {
	if errors.Is(err, errChanges) {
		return 2
	}
	var unknown *commands.UnknownVerbError
	var usage *commands.UsageError
	if errors.As(err, &unknown) || errors.As(err, &usage) {
		return exitUsage
	}
	return commands.ExitError
}

// subDispatchUsage 是外层动词的内层分派收口（H12）：不直接用框架的
// SubDispatch——它会把内层 UnknownVerbError 重写成 plain error（丢失
// 类型），顶层 exitCodeFor 的类型断言因此判不中，嵌套未知子命令退化为
// exit 1，违背 README 契约（嵌套 miss 与顶层 miss 同为 64）。这里直接
// 走内层 Dispatch 保留错误类型，再按类型识别内层 miss 并包成 UsageError
// （携带外层动词的 usage）：退出码回到 64、"usage:" 提示行照常输出
// （与 missing subcommand 同形态），错误文案保持 "unknown subcommand %q"
// 原语义。全部外层动词统一走此收口——分派语义单点可审计。
func subDispatchUsage(c commands.Command, sub *commands.App, ctx context.Context, env *commands.Environment, args []string) error {
	err := sub.Dispatch(ctx, env, args)
	var unknown *commands.UnknownVerbError
	if errors.As(err, &unknown) {
		return &commands.UsageError{Usage: c.Usage(), Err: fmt.Errorf("unknown subcommand %q", unknown.Name)}
	}
	return err
}
