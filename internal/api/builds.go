package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/errcode"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// BuildsService 实现 server.v1.BuildsService（T2.18）。TriggerBuild 与
// Deploy 同型契约：compose 内容字节入队前受控子集校验（compose 违约不动
// 底座、不入队），构建目标裁决（railpack/dockerfile + image 直通）在服务
// 端——旧 CLI 直连入库时在客户端做的 selectBuildTargets 随 CLI-over-SDK
// 改造收编到服务端，目标裁决单一事实源随行。builds.request 跨进程执行
// 契约不变（daemon 的 build.queue worker 消费同一 JSON 形态）。
//
// BuildFromUpload（T 线 DT-6 / IMPL-T2-2）是上传构建面：client-streaming
// 上传上下文 tar（首帧 metadata），服务端落临时会话 → 同一构建队列执行
// → 等待终态返回 builds 行（含 digest 钉定引用）。信任级红线：上传上下文
// 与 git 构建同信任级（同一 dockerfile.v0 前端、无新增构建参数面）；产物
// 平台侧推 zot（调用方零 push 凭证）。
//
// S18-A11：服务持 build.Queue，入队走 Queue.Enqueue（建行 + 唤醒）——同
// 进程触发立即唤醒扫描（不等 poll interval），Queue.Enqueue 自死代码转
// 正；queue 为 nil（进程内夹具形态）时回落直连建行，行为与旧路径一致。
type BuildsService struct {
	serverv1.UnimplementedBuildsServiceServer
	st    *state.Store
	queue *build.Queue
	// uploads 是上传构建面配置（Root 空 = 未装配，BuildFromUpload 如实
	// 报不可用；runtime 经 BuildSettings().UploadConfig() 注入）。
	uploads build.UploadConfig
}

// NewBuildsService 构造 BuildsService（queue 可 nil——未接队列面的进程内
// 夹具形态，入队回落直连建行；uploads.Root 空 = 上传面未装配）。
func NewBuildsService(st *state.Store, queue *build.Queue, uploads build.UploadConfig) *BuildsService {
	return &BuildsService{st: st, queue: queue, uploads: uploads}
}

// TriggerBuild 解析 compose、裁决构建目标并逐服务入队（应用不存在时自动
// 创建——构建常是应用的第一个平台动作，与 deploy 同语义）。
func (s *BuildsService) TriggerBuild(ctx context.Context, req *serverv1.TriggerBuildRequest) (*serverv1.TriggerBuildResponse, error) {
	// compose 内容落临时文件（受控子集校验同 Deploy：临时文件生命周期 =
	// 本次解析，构建执行消费的是 request 里的 context 目录而非该文件）。
	// tmpdir: owned-by build worker（终态后回收，见 janitor/构建归档）——
	// MG-6：base_dir 空回落本目录时，context 目录需存活到异步 worker 执行，
	// 不能随请求 RemoveAll；回收归构建终态清理轨。
	dir, err := os.MkdirTemp("", "fleetly-compose-")
	if err != nil {
		return nil, fmt.Errorf("create compose temp dir: %w", err)
	}
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, req.GetCompose(), 0o600); err != nil { //nolint:gosec // G306：compose 内容非密钥，0600 保守
		return nil, fmt.Errorf("write compose temp file: %w", err)
	}
	spec, warnings, err := compose.Load(ctx, path)
	if err != nil {
		return nil, err // apperr（E_COMPOSE_*）原样透传
	}
	// A1（S18）：TriggerBuildRequest 无 app 字段（REST 面 POST /v1/builds
	// 也不含路径段），应用名单源 = compose name——不存在「请求 app 与
	// compose 应用名错位」的面（Deploy 侧的 A1 守卫在此无对应物）；若
	// 后续版本给请求加 app 字段，必须与 Deploy 同款一致性校验。

	// 上下文解析基准：显式 base_dir（单机同宿主语义）回落临时目录。
	// 基准本身先解析为绝对归一形态（H14：显式 base_dir 是调用方输入，
	// 包含性校验需要确定的锚点；临时回落形态天然绝对）。
	base := req.GetBaseDir()
	if base == "" {
		base = dir
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve base_dir %s: %w", base, err)
	}

	// 归属（v0.3 W2-S3）：app 行已在 → 沿用行上归属（project 字段忽略）；
	// 不存在 → 解析 project（显式引用或用户缺省；机具令牌必须显式）建行。
	// 角色门（v0.3 W2-S4 第 2 门）：TriggerBuild=admin 层级（H14 宿主目录
	// 信任边界）——行在册按行上归属门；首建按解析项目门（先门后建行）。
	app, err := s.st.GetAppByName(ctx, spec.Name)
	if errors.Is(err, state.ErrAppNotFound) {
		proj, rerr := resolveProjectRef(ctx, s.st, req.GetProject())
		if rerr != nil {
			return nil, rerr
		}
		if gerr := requireResourceAccess(ctx, s.st, proj.ID); gerr != nil {
			return nil, gerr
		}
		app, err = ensureApp(ctx, s.st, spec.Name, proj)
	} else if errors.Is(err, state.ErrAppAmbiguous) {
		return nil, apperr.New("E_APP_AMBIGUOUS",
			"app %q resolves to multiple rows across projects; reference it by id or use the qualified read face", spec.Name).
			WithContext("app", spec.Name)
	}
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}

	out := &serverv1.TriggerBuildResponse{App: app.Name, Builds: []*serverv1.BuildView{}}
	for _, svc := range spec.Services {
		if filter := req.GetService(); filter != "" && svc.Name != filter {
			continue
		}
		driver, buildable, err := build.DriverFor(svc)
		if err != nil {
			return nil, err
		}
		if !buildable {
			out.Passthrough = append(out.Passthrough, &serverv1.PassthroughService{
				Service: svc.Name, Image: svc.Image,
			})
			continue
		}
		contextDir, err := resolveBuildContext(base, svc)
		if err != nil {
			return nil, err
		}
		// 构建 ID 在入队侧分配：request JSON 与 builds 行共用同一 ID（一次
		// 建行原子落库；DecodeRequest 校验 build_id 非空——旧 CLI 入队同
		// 一形态，T2.18 实机扫描发现的缺 ID 缺陷在此修正）。
		id := ulid.Make().String()
		raw, err := build.Request{
			BuildID:    id,
			AppID:      app.ID,
			AppName:    spec.Name,
			Service:    svc.Name,
			Driver:     driver,
			ContextDir: contextDir,
			Dockerfile: svc.Build.Dockerfile,
			SpecHash:   spec.SpecHash,
		}.Encode()
		if err != nil {
			return nil, err
		}
		// A11：入队走 Queue.Enqueue（建行 + 唤醒，单一写点——同进程触发
		// 不等 poll interval 即被认领）；无队列面（进程内夹具）回落直连
		// 建行，行为与旧路径一致。M4-8：入队审计带调用方归因（caller
		// token + actor=human，与 Deploy 的 auditEntry 模式对齐——H14
		// 敏感写面上行为人必须可追溯，不再恒 actor=system）。
		audit := state.AuditEntry{
			Actor:        "human",
			ActorTokenID: callerTokenID(ctx),
			Action:       "build.create",
			Target:       "app:" + app.ID,
			Result:       "ok",
			DiffSummary:  state.DiffSummary("build", id, "service", svc.Name, "driver", string(driver)), // B4：构造器替换手拼 JSON
		}
		if s.queue != nil {
			rec, err := s.queue.Enqueue(ctx, state.BuildRecord{
				ID:      id,
				AppID:   app.ID,
				Service: svc.Name,
				Driver:  driver,
				Request: raw,
			}, audit)
			if err != nil {
				return nil, err
			}
			out.Builds = append(out.Builds, buildView(rec, app.Name))
			continue
		}
		rec, err := s.st.CreateBuildAs(ctx, state.BuildRecord{
			ID:      id,
			AppID:   app.ID,
			Service: svc.Name,
			Driver:  driver,
			Request: raw,
		}, &audit)
		if err != nil {
			return nil, err
		}
		out.Builds = append(out.Builds, buildView(rec, app.Name))
	}
	if req.GetService() != "" && len(out.Builds) == 0 && len(out.Passthrough) == 0 {
		return nil, statusInvalidArgument(
			fmt.Sprintf("service %q not found in compose %q", req.GetService(), spec.Name))
	}
	out.Warnings = composeWarnings(warnings)
	return out, nil
}

// ── BuildFromUpload（T 线 DT-6 / IMPL-T2-2）────────────────────────────────

// uploadWaitInterval 是上传构建等待终态的轮询周期（构建秒级起，500ms 兼顾
// 时延与库读开销；与 CLI 的 1s 轮询同族、更密）。
const uploadWaitInterval = 500 * time.Millisecond

// uploadNameRe 是上传镜像仓组件名形态（小写化后校验；首末位字母数字，
// 可含 . _ -；与 docker 分发规范的仓库组件字符集一致）。
var uploadNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// BuildFromUpload 受理上传构建（client-streaming）：首帧 metadata（镜像仓
// 名 + Dockerfile 入口），后续帧 tar 分片；解包 → 同一队列入队 → 等待终态
// → 返回 succeeded 构建行（image_ref/image_digest 为 digest 钉定引用）。
//
// 契约与信任边界见 proto 注释与服务类型注释；传输面裁决（gRPC-only，无
// HTTP 注解）见 §4 审查记录。
func (s *BuildsService) BuildFromUpload(stream serverv1.BuildsService_BuildFromUploadServer) error {
	if s.uploads.Root == "" {
		return statusEnvelope(codes.Unavailable, "build uploads are not assembled in this build (build uploads root is unset)")
	}
	if s.queue == nil {
		return statusEnvelope(codes.Unavailable, "the build queue is not assembled in this build: uploads cannot be executed")
	}
	ctx := stream.Context()
	meta, err := recvUploadMetadata(stream)
	if err != nil {
		return err
	}
	name, err := normalizeUploadName(meta.GetName())
	if err != nil {
		return err
	}
	buildID := ulid.Make().String()
	session, err := build.NewUploadSession(s.uploads.Root, buildID, s.uploads.MaxBytes)
	if err != nil {
		return statusEnvelope(codes.Internal, err.Error())
	}
	// 交接前失败（解包/入口校验/入队失败）由本 handler 清理会话目录；
	// 入队成功后清理归构建终态钩子（成功/失败/超时统一，含进程重启的
	// 收敛路径）——此处不得提前删（等待超时时构建仍在消费上下文）。
	enqueued := false
	defer func() {
		if !enqueued {
			_ = session.Cleanup()
		}
	}()
	reader := &uploadTarReader{stream: stream, maxChunk: s.uploads.MaxChunkBytes}
	if _, err := session.ExtractTar(reader); err != nil {
		return uploadReject(err)
	}
	dockerfile, err := build.ValidateUploadDockerfile(session.ContextDir(), meta.GetDockerfile())
	if err != nil {
		return uploadReject(err)
	}
	raw, err := build.Request{
		BuildID:    buildID,
		AppID:      "", // 上传构建无 app 归属（builds.app_id 可空，00027）
		AppName:    name,
		Service:    "",
		Driver:     state.DriverDockerfile,
		ContextDir: session.ContextDir(),
		Dockerfile: dockerfile,
		// 会话目录随构建终态清理（builder/queue 钩子；受上传根约束）。
		EphemeralDir: session.Dir(),
	}.Encode()
	if err != nil {
		return statusEnvelope(codes.Internal, err.Error())
	}
	audit := state.AuditEntry{
		Actor:        "human",
		ActorTokenID: callerTokenID(ctx),
		Action:       "build.create",
		Target:       "build:" + buildID,
		Result:       "ok",
		DiffSummary:  state.DiffSummary("build", buildID, "name", name, "dockerfile", dockerfile, "source", "upload"),
	}
	if _, err := s.queue.Enqueue(ctx, state.BuildRecord{
		ID: buildID, AppID: "", Service: "", Driver: state.DriverDockerfile, Request: raw,
	}, audit); err != nil {
		return err
	}
	enqueued = true
	rec, err := s.waitUploadBuild(ctx, buildID)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&serverv1.BuildFromUploadResponse{Build: buildView(rec, "")})
}

// recvUploadMetadata 读取上传流首帧并取 metadata（首帧形态违约 →
// E_BUILD_UPLOAD_INVALID；传输错误原样上抛）。
func recvUploadMetadata(stream serverv1.BuildsService_BuildFromUploadServer) (*serverv1.BuildFromUploadMetadata, error) {
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return nil, apperr.New("E_BUILD_UPLOAD_INVALID", "upload stream is empty: the first message must carry build metadata").WithStage("build.upload")
	}
	if err != nil {
		return nil, err
	}
	meta := first.GetMetadata()
	if meta == nil {
		return nil, apperr.New("E_BUILD_UPLOAD_INVALID", "upload stream protocol violation: the first message must carry metadata").WithStage("build.upload")
	}
	return meta, nil
}

// normalizeUploadName 归一上传镜像仓名（trim + 小写 + 形态校验）。
func normalizeUploadName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" || len(n) > 100 || !uploadNameRe.MatchString(n) {
		return "", apperr.New("E_BUILD_UPLOAD_INVALID",
			"upload name %q is not a valid image repository component (want [a-z0-9] with . _ - inside, 1-100 chars; it is lowercased)", name).
			WithStage("build.upload").
			WithContext("name", name)
	}
	return n, nil
}

// uploadReject 把解包/入口校验错误映射为注册码信封（超限/形态非法各有
// 稳定码；其余宿主 IO 故障原样上抛——B1 出口收口）。
func uploadReject(err error) error {
	switch {
	case errors.Is(err, build.ErrUploadTooLarge):
		return apperr.New("E_BUILD_UPLOAD_TOO_LARGE", "%v", err).WithStage("build.upload")
	case errors.Is(err, build.ErrUploadInvalid):
		return apperr.New("E_BUILD_UPLOAD_INVALID", "%v", err).WithStage("build.upload")
	default:
		return err
	}
}

// waitUploadBuild 轮询至构建终态：succeeded → 记录；failed → 注册码信封
// （点名 build id 与日志路径，便于调用方归因）；等待期 ctx 结束 → 构建仍
// 在队列中执行（不回调、不清理），返回等待中止信封点名 build id。
func (s *BuildsService) waitUploadBuild(ctx context.Context, buildID string) (state.BuildRecord, error) {
	for {
		rec, err := s.st.GetBuild(ctx, buildID)
		if err != nil {
			return state.BuildRecord{}, err
		}
		switch rec.Status {
		case state.BuildSucceeded:
			return rec, nil
		case state.BuildFailed:
			return state.BuildRecord{}, buildTerminalError(rec)
		}
		select {
		case <-ctx.Done():
			code := status.FromContextError(ctx.Err()).Code()
			return state.BuildRecord{}, status.Errorf(code,
				"build %s is still running: upload accepted and the build continues in the queue; re-check its terminal state via GetBuild (the uploaded context is cleaned up when the build finishes)", buildID)
		case <-time.After(uploadWaitInterval):
		}
	}
}

// buildTerminalError 构造构建失败终态的错误信封：沿用 builds 行登记的
// error_code（注册表内码；未知/缺失回落 E_BUILD_FAILED），build id 与
// 日志路径进 context。
func buildTerminalError(rec state.BuildRecord) error {
	code := rec.ErrorCode
	recorded := rec.ErrorCode
	if _, ok := errcode.Get(code); !ok {
		code = "E_BUILD_FAILED"
	}
	if recorded == "" {
		recorded = "no error code recorded"
	}
	e := apperr.New(code, "build %s ended as failed (%s)", rec.ID, recorded).
		WithStage("build").
		WithContext("build_id", rec.ID)
	if rec.LogPath != "" {
		e = e.WithContext("log_path", rec.LogPath)
	}
	return e
}

// uploadTarReader 把上传流帧适配为 tar 读取器：metadata 只允许首帧携带，
// 分片帧非空且不超单帧上限；流结束以 io.EOF 表达（tar 层据此判截断）。
type uploadTarReader struct {
	stream   serverv1.BuildsService_BuildFromUploadServer
	pending  []byte
	maxChunk int64
}

func (r *uploadTarReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		msg, err := r.stream.Recv()
		if err != nil {
			return 0, err // io.EOF = 客户端正常结束
		}
		if msg.GetMetadata() != nil {
			return 0, fmt.Errorf("%w: metadata is only allowed in the first message", build.ErrUploadInvalid)
		}
		chunk := msg.GetChunk()
		if len(chunk) == 0 {
			return 0, fmt.Errorf("%w: chunk frames must be non-empty", build.ErrUploadInvalid)
		}
		maxChunk := r.maxChunk
		if maxChunk <= 0 {
			maxChunk = build.MaxUploadChunkBytes
		}
		if int64(len(chunk)) > maxChunk {
			return 0, fmt.Errorf("%w: chunk of %d bytes exceeds the %d byte frame limit", build.ErrUploadInvalid, len(chunk), maxChunk)
		}
		r.pending = chunk
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// resolveBuildContext 解析服务的构建上下文目录并执行包含性校验（H14
// 宿主目录信任边界）：base 为已归一的绝对基准目录，build.context 解析后
// 必须位于 base 之内——`..` 逃逸形态（如 ../../.. 直指宿主根、SQLite 库
// 或密钥材料所在目录）会把宿主任意目录整目录打进镜像再经部署外带，
// 在入队前拒绝（基准为临时回落目录时同样适用：逃逸形态不可能承载合法
// 构建内容，只有外带语义）。复用 compose 族拒绝码 E_COMPOSE_UNSUPPORTED
// （errcode 零新增，H14 裁决），错误信息点名服务与逃逸取值。
func resolveBuildContext(base string, svc compose.Service) (string, error) {
	contextDir := filepath.Join(base, svc.Build.Context) // Join 已 Clean，越界逃逸折叠为前导 ..
	rel, err := filepath.Rel(base, contextDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", apperr.New("E_COMPOSE_UNSUPPORTED",
			"service %s: build.context %q resolves outside the base directory %s: the build context must stay within base_dir (defaults to the server-side temp directory) (host directory trust boundary, H14)",
			svc.Name, svc.Build.Context, base)
	}
	return contextDir, nil
}

// GetBuild 单条构建（CLI build 的等待轮询源）。
func (s *BuildsService) GetBuild(ctx context.Context, req *serverv1.GetBuildRequest) (*serverv1.GetBuildResponse, error) {
	rec, err := s.st.GetBuild(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.GetBuildResponse{Build: buildView(rec, s.appNameByID(ctx, rec.AppID))}, nil
}

// ListBuilds 按应用列构建（created_at 倒序；可见域解析 + 角色门，W2-S4）。
func (s *BuildsService) ListBuilds(ctx context.Context, req *serverv1.ListBuildsRequest) (*serverv1.ListBuildsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.st.ListAppBuilds(ctx, app.ID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.BuildView, 0, len(rows))
	for _, rec := range rows {
		out = append(out, buildView(rec, app.Name))
	}
	return &serverv1.ListBuildsResponse{Builds: out}, nil
}

// appNameByID 取应用显示名（已删除应用回退显示 ID——台账对照语义）。
func (s *BuildsService) appNameByID(ctx context.Context, appID string) string {
	if app, err := s.st.GetAppByID(ctx, appID); err == nil {
		return app.Name
	}
	return appID
}

// buildView 构造构建投影。
func buildView(rec state.BuildRecord, appName string) *serverv1.BuildView {
	return &serverv1.BuildView{
		Id:          rec.ID,
		App:         appName,
		Service:     rec.Service,
		Driver:      string(rec.Driver),
		Status:      string(rec.Status),
		ImageRef:    rec.ImageRef,
		ImageDigest: rec.ImageDigest,
		PlanPath:    rec.PlanPath,
		LogPath:     rec.LogPath,
		ErrorCode:   rec.ErrorCode,
		StartedAt:   tstamp(rec.StartedAt),
		FinishedAt:  tstamp(rec.FinishedAt),
	}
}

// composeWarnings 构造 compose 校验警告的跨面投影。
func composeWarnings(ws []compose.Warning) []*serverv1.ComposeWarning {
	out := make([]*serverv1.ComposeWarning, 0, len(ws))
	for _, w := range ws {
		out = append(out, &serverv1.ComposeWarning{
			Kind: w.Kind, Code: w.Code, Service: w.Service, Message: w.Message,
		})
	}
	return out
}
