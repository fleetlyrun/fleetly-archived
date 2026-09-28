package build

// Builder 是构建执行器（Executor 接口的实现）：一次构建的端到端编排——
// 请求解码 → 产物目录/日志 → 驱动分派（railpack LLB / dockerfile 前端）→
// buildkit solve + 本机装载 → inspect 取不可变 digest → builds 状态落库
// （终态迁移 + 审计同事务在 state 层完成）。失败归一为 E_BUILD_FAILED
// 信封（stderr 尾部与日志路径进 context——T2.8「失败带 stderr 与建议」）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// daemonEnsureInterval 是 buildkitd 就绪收敛的重试间隔（v0.1 常量值；包级
// 变量形态供测试注入缩短——与 substrate.defaultCallTimeout 同款接缝）。
var daemonEnsureInterval = 15 * time.Second

// daemonProbeInterval 是 buildkitd 就绪探测的重试间隔（M2-8；兼作单次探测
// 的拨号预算——黑洞端点下挂满一个间隔即判失败进下一轮）。包级变量形态
// 供测试注入缩短。
var daemonProbeInterval = 2 * time.Second

// Executor 是构建队列消费的执行契约（单测注入假执行器测并发上限）。
type Executor interface {
	// Execute 执行一条已 claim（building）的构建记录，终态落库后返回最新
	// 记录；返回的错误已归一为应用错误信封。
	Execute(ctx context.Context, rec state.BuildRecord) (state.BuildRecord, error)
}

// solveExecutor 是 solve 层的包内执行契约（builder 与 buildkit 连接细节
// 的隔离缝；单测注入假执行器断言 registry 模式的推送记账/失败归因，不触
// buildkit）。返回的 digest 是 solve 响应回填的 manifest digest（无 image
// 导出时为空串——本机模式由调用方 inspect 取本机 digest）。
type solveExecutor interface {
	solveLLB(ctx context.Context, opts bkclient.SolveOpt, def *llb.Definition, logw io.Writer) (string, error)
	solveFrontend(ctx context.Context, opts bkclient.SolveOpt, logw io.Writer) (string, error)
}

// Builder 依赖：配置 + 状态库 + 本机镜像端口 + buildkitd 编排端口 + solve
// 执行器 + 就绪探测。
type Builder struct {
	cfg    Config
	store  *state.Store
	images ImageSource
	daemon DaemonManager
	solver solveExecutor
	// probe 是 buildkitd 就绪探测（M2-8：EnsureContainerRunning 成功 ≠
	// gRPC 可服务，首建冷启动存在监听就绪窗口）。缺省对 buildkit_host 做
	// Info 拨号（defaultDaemonProbe）；包内测试注入假 probe 断言重试语义。
	probe func(ctx context.Context) error
	log   *slog.Logger
	// logSink 是构建日志行的下游分流目标（W5-S1；nil = 未装配——日志
	// 只写 build.log 文件，v0.1 行为逐字不变）。
	logSink BuildLogSink
}

// NewBuilder 构建构建执行器。daemon 可为 nil（装配测试干跑；自管容器形态
// 下执行前的就绪收敛由 ensureDaemonReady 承载）。
func NewBuilder(cfg Config, store *state.Store, images ImageSource, daemon DaemonManager, log *slog.Logger) *Builder {
	cfg = cfg.Normalize()
	return &Builder{
		cfg:    cfg,
		store:  store,
		images: images,
		daemon: daemon,
		solver: newSolveRunner(cfg.BuildkitHost, images),
		probe:  defaultDaemonProbe(cfg.BuildkitHost),
		log:    log,
	}
}

// Config 返回归一化后的配置（服务装配与诊断用）。
func (b *Builder) Config() Config { return b.cfg }

// WithBuildLogSink 注入构建日志行的下游分流目标（W5-S1；链式装配，nil
// 合法 = 不分流）。
func (b *Builder) WithBuildLogSink(s BuildLogSink) *Builder { b.logSink = s; return b }

// CacheLocalPath 返回 buildkit local cache 的宿主数据根（客户端侧路径）。
func (b *Builder) CacheLocalPath() string { return b.cfg.CacheDir }

// DaemonSpec 返回平台自管 buildkitd 容器的目标形态（ManageDaemon=false 时
// 返回 ok=false——外部端点形态不自管容器）。
func (b *Builder) DaemonSpec() (DaemonSpec, bool) {
	if !b.cfg.ManageDaemon {
		return DaemonSpec{}, false
	}
	return DaemonSpec{
		Name:        b.cfg.DaemonContainerName,
		Image:       BuildkitImage,
		MemoryBytes: b.cfg.MemoryBytes,
		NanoCPUs:    b.cfg.NanoCPUs,
		CacheVolume: b.cfg.CacheVolume,
	}, true
}

// executeBudget 是执行前 buildkitd 就绪收敛的重试预算（首建等钉版镜像
// 拉取——冷拉取受网络主导，Spike A 实测量级分钟；6 分钟覆盖慢网）。
// 包级变量形态供测试注入缩短（与 substrate.defaultCallTimeout 同款接缝）。
var executeEnsureBudget = 6 * time.Minute

// ensureDaemonReady 同步收敛平台自管 buildkitd（ManageDaemon=false 或
// daemon 端口为空 = no-op）。带重试预算的幂等收敛：镜像拉取中/容器重启
// 都在此等待而非终态失败——构建因基础设施暂未就绪而终态失败是把可重试
// 错误当永久错误（实测缺陷：warm-up 后台化后首建抢跑）。
func (b *Builder) ensureDaemonReady(ctx context.Context) error {
	spec, ok := b.DaemonSpec()
	if !ok || b.daemon == nil {
		return nil
	}
	deadline := time.Now().Add(executeEnsureBudget)
	var lastErr error
	for {
		// M2-4：每轮清零 lastErr——上一轮的瞬时错误（首建镜像拉取抖动等）
		// 不得毒化本轮。不清零时 `lastErr == nil` 的门条件永不成立：后续
		// 轮次 EnsureVolumePresent 成功也被跳过，空转整个预算后失败。
		lastErr = nil
		if spec.CacheVolume != "" {
			if err := b.daemon.EnsureVolumePresent(ctx, spec.CacheVolume); err != nil {
				lastErr = err
			}
		}
		if lastErr == nil {
			if err := b.daemon.EnsureContainerRunning(ctx, spec); err != nil {
				lastErr = err
			}
		}
		if lastErr == nil {
			if err := b.probeDaemonReady(ctx, deadline); err != nil {
				lastErr = err
			}
		}
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return lastErr
		}
		if !time.Now().Before(deadline) {
			return fmtErr("buildkitd not ready within %s: %w", executeEnsureBudget, lastErr)
		}
		b.log.Warn("buildkitd not ready, retrying", "container", spec.Name, "error", lastErr.Error())
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(daemonEnsureInterval):
		}
	}
}

// probeDaemonReady 就绪探测（M2-8）：容器 start 成功 ≠ buildkitd 可服务
// ——gRPC 监听就绪存在窗口（首建冷启动数秒），solve 前的 Info 连接门无
// 重试，冷窗口内的构建被误判 E_BUILD_FAILED。探测经 Builder.probe 注入
// （缺省 = 对 buildkit_host 做 Info 拨号），每 daemonProbeInterval 一次、
// 至剩余预算耗尽——未就绪保持等待而非失败（把可重试的就绪窗口当永久
// 错误是 M2-8 的缺陷形态）。
func (b *Builder) probeDaemonReady(ctx context.Context, deadline time.Time) error {
	var lastErr error
	for {
		// 单次探测自带拨号预算（daemonProbeInterval）：黑洞端点（防火墙
		// 静默丢包）下挂满一个间隔即判失败，不占住探测节奏。
		attemptCtx, cancel := context.WithTimeout(ctx, daemonProbeInterval)
		lastErr = b.probe(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return lastErr
		}
		if !time.Now().Before(deadline) {
			return fmtErr("buildkitd not ready within %s: %w", executeEnsureBudget, lastErr)
		}
		b.log.Warn("buildkitd probe not ready, waiting", "error", lastErr.Error())
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(daemonProbeInterval):
		}
	}
}

// EnsureDaemonReady 是 ensureDaemonReady 的公开形态（服务壳预热路径用；
// 幂等，可在构建执行前任意次调用）。
func (b *Builder) EnsureDaemonReady(ctx context.Context) error { return b.ensureDaemonReady(ctx) }

// Execute 实现执行器契约。
func (b *Builder) Execute(ctx context.Context, rec state.BuildRecord) (state.BuildRecord, error) {
	if rec.Status != state.BuildBuilding {
		return rec, fmtErr("build %s must be claimed (building) before execute, got %s", rec.ID, rec.Status)
	}
	// 终态读写用去取消化 ctx：执行 ctx 是队列服务生存期 ctx，优雅关停的
	// 取消可能落在 solve 完成后/失败处理中——终态落库不得随之丢失（否则
	// builds 行永久停留 building）。WithoutCancel 保留 trace/log 上下文、
	// 不继承取消与 deadline，正合「终态必达」语义；run/solve 本身仍用原
	// ctx（关停即取消，正确）。
	finCtx := context.WithoutCancel(ctx)
	req, err := DecodeRequest(rec.Request)
	if err != nil {
		// 请求损坏是入队方错误：终态 failed（E_BUILD_FAILED，无日志可附）。
		return state.BuildRecord{}, b.fail(finCtx, rec.ID, err)
	}
	// IMPL-T2-2：上传构建的上下文会话目录随终态清理（成功/失败/超时统一
	// 走 defer——清理钩子的 main 层；重启/收敛路径的兜底见 Queue 与
	// janitor）。CleanupUploadDir 只删上传根的直接子目录（越界形态 no-op）。
	if req.EphemeralDir != "" {
		defer func() {
			if err := CleanupUploadDir(req.EphemeralDir, b.cfg.UploadsRoot); err != nil {
				b.log.Warn("cleanup ephemeral build context", "build", rec.ID, "dir", req.EphemeralDir, "error", err.Error())
			}
		}()
	}
	if err := b.ensureDaemonReady(ctx); err != nil {
		return state.BuildRecord{}, b.fail(finCtx, rec.ID, err)
	}
	// H14 执行侧校验（纵深防御）：ContextDir 必须位于受管根内——
	// builds.request 是跨进程通道（CLI/API 入队之外的直写库形态同样可
	// 达），API 层包含性校验之外的最后一道门。越界即终态失败
	// （E_BUILD_FAILED，信息注明上下文目录越界），不静默放宽。
	if err := validateContextDir(req.ContextDir, b.cfg.ContextRoots); err != nil {
		return state.BuildRecord{}, b.fail(finCtx, rec.ID, err)
	}

	artDir := ArtifactsDir(b.cfg.ArtifactsDir, req.AppName, rec.ID)
	if err := os.MkdirAll(artDir, 0o750); err != nil {
		return state.BuildRecord{}, b.fail(finCtx, rec.ID, fmtErr("create artifacts dir %s: %w", artDir, err))
	}
	logPath := filepath.Join(artDir, "build.log")
	planPath := filepath.Join(artDir, "railpack-plan.json")
	// M2-5：产物目录就位即回填 log_path——builds.log_path 原先唯一写点在
	// FinishBuildSucceeded，失败终态行恒空 → fail() 的日志尾部取证与
	// log_path context 全部落空。回填后失败路径同样携带日志（尾部 + 路径）；
	// 写入 best-effort（观测面，失败仅告警不中断构建），终态谓词保证不
	// 触碰已终态行（成功终态的 log_path 仍由 FinishBuildSucceeded 权威写）。
	if err := b.store.SetBuildLogPath(finCtx, rec.ID, logPath); err != nil {
		b.log.Warn("record build log path", "build", rec.ID, "error", err.Error())
	}

	imageRef, err := ImageRef(req.AppName, rec.ID)
	if err != nil {
		return state.BuildRecord{}, b.fail(finCtx, rec.ID, err)
	}

	digest, took, buildErr := b.run(ctx, rec, req, imageRef, planPath, logPath)
	if buildErr != nil {
		// registry 模式失败归因（D-MN-11 分层建议）：推送类失败 →
		// E_REGISTRY_PUSH_FAILED（查 registry/网络/凭据）；其余（构建本体
		// 错误）维持 E_BUILD_FAILED（改代码）。本地模式不经分类器。
		if b.registryMode() {
			buildErr = ClassifyRegistryPushError(buildErr)
		}
		// 超时预算耗尽（队列以 WithTimeout 注入的 per-build deadline）：
		// 失败信息注明预算供定位（预算 ≈ deadline − 认领时间，claim 盖章）。
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && !rec.StartedAt.IsZero() {
			if dl, ok := ctx.Deadline(); ok && dl.After(rec.StartedAt) {
				buildErr = fmtErr("build timed out after ~%s budget (config build.timeout_seconds): %w",
					dl.Sub(rec.StartedAt).Round(time.Second), buildErr)
			}
		}
		return state.BuildRecord{}, b.fail(finCtx, rec.ID, buildErr)
	}

	// builds.image_ref 记账形态（设计 §2.5）：本地模式 = fleetly-local/<app>:
	// <tag>（v0.1 逐字不变）；registry 模式 = registry.<base>/apps/<app>@
	// sha256:<manifest digest>——引擎按引用直入 service spec（digest 钉定，
	// worker 经 --with-registry-auth 拉取），digest 列同记 manifest digest。
	recordedRef := imageRef
	if b.registryMode() {
		recordedRef, err = RegistryDigestRef(b.cfg.RegistryHost, req.AppName, digest)
		if err != nil {
			return state.BuildRecord{}, b.fail(finCtx, rec.ID, err)
		}
		digest = normalizeDigest(digest)
	}

	if err := b.store.FinishBuildSucceeded(finCtx, rec.ID, recordedRef, digest, planPathFor(rec, planPath), logPath); err != nil {
		return state.BuildRecord{}, fmtErr("record build %s succeeded: %w", rec.ID, err)
	}
	b.log.Info("build succeeded",
		"build", rec.ID, "app", req.AppName, "service", rec.Service,
		"driver", string(rec.Driver), "ref", recordedRef, "digest", digest,
		"seconds", fmt.Sprintf("%.2f", took))
	updated, err := b.store.GetBuild(finCtx, rec.ID)
	if err != nil {
		return state.BuildRecord{}, fmtErr("read build %s: %w", rec.ID, err)
	}
	return updated, nil
}

// registryMode 报告构建管线是否处于 registry 推送形态（RegistryHost 非空，
// base_domain 派生——E1-5；空 = 本地模式，v0.1 管线逐字不变）。
func (b *Builder) registryMode() bool { return b.cfg.RegistryHost != "" }

// applyRegistryMode 把 registry 模式叠加到 solve 选项上（推送引用 + buildkit
// session 凭据注入）。凭据文件读取在执行时点（凭据可能晚于 Builder 装配才
// 由 zot 部署 duty 生成）；缺失/损坏即推送失败（E_REGISTRY_PUSH_FAILED），
// 不静默空凭据。
func (b *Builder) applyRegistryMode(opts *bkclient.SolveOpt, req Request) error {
	if !b.registryMode() {
		return nil
	}
	creds, err := LoadRegistryCredentials(b.cfg.RegistryAuthFile)
	if err != nil {
		return registryPushFailed("registry credentials unavailable: %v", err)
	}
	pushRef, err := RegistryBuildRef(b.cfg.RegistryHost, req.AppName, req.BuildID)
	if err != nil {
		return err // 命名契约错误属构建输入问题：维持 E_BUILD_FAILED 归因
	}
	return applyRegistryPush(opts, pushRef, b.cfg.RegistryHost, creds.User, creds.Password)
}

// registryPushFailed 构造推送失败信封（E_REGISTRY_PUSH_FAILED，500——设计
// §5.2/D-MN-11：网络/凭据/registry 故障的分层归因）。
func registryPushFailed(format string, args ...any) error {
	return apperr.New("E_REGISTRY_PUSH_FAILED", "pushing build result to the platform registry failed: "+format, args...).
		WithStage("push")
}

// run 分派驱动执行构建，返回镜像 digest 与耗时：
//   - 本地模式：buildkit docker 导出 + 本机装载，digest = 本机镜像 ID
//     （inspect 取得，`sha256:<hex>` 配置摘要，v0.1 语义不变）；
//   - registry 模式：buildkit image 导出 push=true 直推平台 registry，
//     digest = solve 响应回填的 manifest digest（本机不装载——zot 是产物
//     唯一真源；空 digest = 推送未回填，按推送失败记账拒绝）。
func (b *Builder) run(ctx context.Context, rec state.BuildRecord, req Request, imageRef, planPath, logPath string) (string, float64, error) {
	started := time.Now()
	logf, err := os.Create(logPath) //nolint:gosec // G304：logPath 是平台配置产物目录下的受控派生路径（config build.artifacts_dir）
	if err != nil {
		return "", 0, fmtErr("create build log %s: %w", logPath, err)
	}
	defer func() { _ = logf.Close() }()
	// W5-S1：日志行分流（文件写入逐字不变；分流喂入湖批量器 source=build
	// ——设计 §2.3 同一咽喉点接入；sink 未装配 = 纯透传零差异）。v0.3：
	// app 标识以三段限定形传递（流标签口径，rbac-teams §4.3）——每次构建
	// 解析一次 app 行（slug 不可变），行级零开销。
	appLabel := req.AppName
	if appRow, aerr := b.store.GetAppByID(context.Background(), rec.AppID); aerr == nil {
		appLabel = appRow.QualifiedName()
	} else if !errors.Is(aerr, state.ErrAppNotFound) && b.log != nil {
		b.log.Warn("build: resolve app for log labels failed", "app_id", rec.AppID, "error", aerr.Error())
	}
	logw := newLogTee(logf, b.logSink, rec.AppID, appLabel, rec.Service)

	var pushDigest string
	switch rec.Driver {
	case state.DriverRailpack:
		def, imageConfig, planJSON, err := railpackPlan(ctx, req)
		if err != nil {
			return "", 0, err
		}
		if err := writePlanArchive(planPath, planJSON); err != nil {
			return "", 0, err
		}
		opts, err := railpackSolveOptions(req, imageRef, imageConfig, b.CacheLocalPath())
		if err != nil {
			return "", 0, err
		}
		if err := b.applyRegistryMode(&opts, req); err != nil {
			return "", 0, err
		}
		pushDigest, err = b.solver.solveLLB(ctx, opts, def, logw)
		if err != nil {
			return "", 0, err
		}
	case state.DriverDockerfile:
		opts, err := dockerfileSolveOptions(req, imageRef, b.CacheLocalPath())
		if err != nil {
			return "", 0, err
		}
		if err := b.applyRegistryMode(&opts, req); err != nil {
			return "", 0, err
		}
		pushDigest, err = b.solver.solveFrontend(ctx, opts, logw)
		if err != nil {
			return "", 0, err
		}
	default:
		return "", 0, fmtErr("unsupported build driver %q", rec.Driver)
	}

	if b.registryMode() {
		if pushDigest == "" {
			return "", 0, registryPushFailed("solve response carried no manifest digest (push accounting requires it)")
		}
		return pushDigest, time.Since(started).Seconds(), nil
	}

	info, err := b.images.InspectImage(ctx, imageRef)
	if err != nil {
		return "", 0, fmtErr("inspect built image %s: %w", imageRef, err)
	}
	return info.ID, time.Since(started).Seconds(), nil
}

// fail 归一失败终态：默认 E_BUILD_FAILED 信封（T2.8 契约：stderr 尾部 +
// 日志路径进 context）+ builds failed 落库。失败归因保留（D-MN-11 分层
// 建议）：registry 推送失败等已归一为专属注册码的错误信封原样保留——
// E_REGISTRY_PUSH_FAILED 的「查 registry/凭据」建议不得被 E_BUILD_FAILED
// 的「改代码」建议覆盖；信封即 cause（日志尾部经 cause 包装链随行），
// builds 行 error_code 落保留后的码。终态写一律去取消化（WithoutCancel）
// ——失败处理常发生在执行 ctx 已取消时（超时/关停），落库必达。
func (b *Builder) fail(ctx context.Context, buildID string, cause error) error {
	ctx = context.WithoutCancel(ctx)
	rec, err := b.store.GetBuild(ctx, buildID)
	if err == nil && rec.LogPath != "" {
		tail := tailFile(rec.LogPath)
		if tail != "" {
			cause = fmt.Errorf("%w\n--- build log tail ---\n%s", cause, tail)
		}
	}
	appErr := apperr.New("E_BUILD_FAILED", "build failed: %v", cause).
		WithStage("build").
		WithCause(cause)
	var coded *apperr.Error
	if errors.As(cause, &coded) && coded.Code() != "E_BUILD_FAILED" {
		appErr = coded.WithCause(cause)
	}
	if err == nil {
		if rec.LogPath != "" {
			appErr = appErr.WithContext("log_path", rec.LogPath)
		}
		if ferr := b.store.FinishBuildFailed(ctx, buildID, appErr.Code()); ferr != nil {
			b.log.Error("record build failure", "build", buildID, "error", ferr)
		}
	}
	b.log.Error("build failed", "build", buildID, "error", cause)
	return appErr
}

// planPathFor 仅 railpack 驱动回填 plan_path（dockerfile 无 plan 产物）。
func planPathFor(rec state.BuildRecord, planPath string) string {
	if rec.Driver == state.DriverRailpack {
		return planPath
	}
	return ""
}
