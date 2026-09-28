package build

// 构建输入请求与产出结果（构建队列的跨进程契约）：Request 以 JSON 落
// builds.request 列（CLI 入队 → daemon 队列 worker 执行的唯一通道）；
// Result 由 Builder.Execute 产出（succeeded 终态回写 builds 行）。

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ImageRepoPrefix 是本机构建镜像的仓前缀（fleetly-local/<app>；v0.1 免
// registry——镜像只落本机 daemon，部署以 `名称@sha256:` 引用，D9）。
const ImageRepoPrefix = "fleetly-local"

// tagComponentRe 是镜像 tag 合法字符（docker 分发规范限制小写；app 名与
// ULID 经 ToLower 后必然满足）。
var tagComponentRe = regexp.MustCompile(`^[a-z0-9._-]+$`)

// Request 是一次构建的执行输入（跨进程契约，JSON 落 builds.request）。
type Request struct {
	// BuildID 与 builds.id 一致（产物归档目录与镜像 tag 的可追溯成分）。
	BuildID string `json:"build_id"`
	// AppID / AppName 是平台应用标识与 compose 项目名（镜像 tag 成分）。
	AppID   string `json:"app_id"`
	AppName string `json:"app_name"`
	// Service 是 compose 服务名。
	Service string `json:"service"`
	// Driver 是构建驱动（railpack | dockerfile；由 compose 模式裁决）。
	Driver state.Driver `json:"driver"`
	// ContextDir 是构建上下文绝对路径（compose build.context 相对 compose
	// 文件目录解析后；受控子集内校验过的目录）。
	ContextDir string `json:"context_dir"`
	// Dockerfile 是相对 ContextDir 的 dockerfile 路径（仅 dockerfile 驱动）。
	Dockerfile string `json:"dockerfile,omitempty"`
	// EphemeralDir 是上传构建的临时会话目录（IMPL-T2-2/DT-6：上下文随
	// 构建终态清理；空 = git/compose 构建，不受清理钩子管辖）。清理前由
	// 钩子强制校验位于 build.Config.UploadsRoot 内（CleanupUploadDir）。
	EphemeralDir string `json:"ephemeral_dir,omitempty"`
	// Target 是可选构建目标 stage（v0.1 不开，字段预留 compose 受控子集）。
	Target string `json:"target,omitempty"`
	// SpecHash 是归一化 compose 的 spec_hash（缓存键与可追溯成分）。
	SpecHash string `json:"spec_hash,omitempty"`
	// Platform 是构建目标平台（空 = 宿主默认 linux/amd64）。
	Platform string `json:"platform,omitempty"`
	// Secrets 是构建期凭证（key=值；经 buildkit session secrets 挂载，永不
	// 进镜像层——Spike A E3）。v0.1 构建无凭证输入（compose secrets 是运行
	// 期 Swarm secret），字段存在即契约：加入缓存键 secrets-hash，未来接入
	// 时凭证变化自动正确失效。
	Secrets map[string]string `json:"secrets,omitempty"`
}

// Encode 序列化为 JSON（落 builds.request 列）。
func (r Request) Encode() (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("build: encode request %s: %w", r.BuildID, err)
	}
	return string(raw), nil
}

// DecodeRequest 从 builds.request 列还原请求。
func DecodeRequest(raw string) (Request, error) {
	var r Request
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Request{}, fmt.Errorf("build: decode request: %w", err)
	}
	if r.BuildID == "" || r.ContextDir == "" {
		return Request{}, fmt.Errorf("build: request missing build_id/context_dir")
	}
	return r, nil
}

// validateContextDir 校验 ContextDir 位于受管根内（H14 宿主目录信任边界
// 的执行侧纵深防御，Builder.Execute 在 DecodeRequest 之后调用）：
//  1. 必须是绝对路径且与 Clean 结果一致——API 层产物经 Abs(Join(…))
//     天然满足，直写 builds.request 的 `..` 逃逸词形在此拦截；
//  2. 必须位于 roots 中至少一个受管根之内（containsPath 判定；跨卷路径
//     对根不成立即继续比对下一根，全部不成立 = 越界）。
//
// H8（MG-1 同族：词法校验拦不住符号链接）：dir 与各受管 root 在比对前均
// 经 filepath.EvalSymlinks 解析——受管根内的 symlink 指向根外目录时，
// 解析后的真实路径对全部根做包含性判定即不成立（拒绝），且**已解析的
// dir 不做任何词法兜底比对**（词形命中会把 symlink 逃逸重新放进来）。
// dir 解析失败（目录不存在）保持原词法错误语义（用词法路径比对——存在
// 性校验不属本函数职责，缺失目录由构建执行期失败收敛；不存在路径上的
// 「词法在内」是惰性形态，无外带能力）。root 解析失败（根不存在）跳过
// 该根的解析态比对。与 fsutil.NewFS 对 root 的 EvalSymlinks 行为对齐
// （执行侧快照遍历的是解析后真源，校验面必须同一真源口径）。
//
// roots 须为归一化后的受管根（Config.Normalize 保证非空且含系统 temp
// 根）。校验失败不得静默放宽：调用方据此落 E_BUILD_FAILED 终态。
func validateContextDir(dir string, roots []string) error {
	if !filepath.IsAbs(dir) {
		return fmtErr("context directory outside managed roots: context_dir %q is not an absolute path (managed roots: %s)", dir, strings.Join(roots, ", "))
	}
	if filepath.Clean(dir) != dir {
		return fmtErr("context directory outside managed roots: context_dir %q contains unclean segments (.. escape form)", dir)
	}
	// H8：解析符号链后的真实路径参与比对；解析失败（目录不存在）回落
	// 词法路径（unresolved 标记——见函数头注记的语义边界）。
	resolved, resolveErr := filepath.EvalSymlinks(dir)
	if resolveErr != nil {
		resolved = dir
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		// 受管根 best-effort 解析：根不存在（EvalSymlinks 失败）跳过该根
		// 的解析态比对——真实存在的 dir 不可能位于不存在的根之内。
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr == nil && containsPath(resolvedRoot, resolved) {
			return nil
		}
		// 未解析（不存在）dir 的词法兜底：对词法根（与未解析根）比对。
		// macOS 形态（os.TempDir 的 /var 前缀解析为 /private/var）下，
		// 不存在路径的词形与解析后根恒不匹配，需要词法面收口；已解析
		// dir 不进此分支（防 symlink 逃逸经词形重新放行）。
		if resolveErr != nil && containsPath(root, dir) {
			return nil
		}
	}
	return fmtErr("context directory outside managed roots: context_dir %q is not within a managed root (%s) — the build context must live under platform-managed directories (system temp / git root / explicitly configured roots)", dir, strings.Join(roots, ", "))
}

// Result 是一次构建的产出（成功路径；失败经 E_BUILD_FAILED 错误信封表达）。
type Result struct {
	// ImageRef 是本机镜像引用 fleetly-local/<app>:<tag>。
	ImageRef string
	// ImageDigest 是不可变镜像 ID（`sha256:<hex>` 配置摘要；部署引用取
	// digest，D9）。
	ImageDigest string
	// PlanPath 是 railpack plan JSON 归档路径（dockerfile 驱动为空）。
	PlanPath string
	// LogPath 是构建日志路径。
	LogPath string
	// Duration 是构建净耗时（二次构建加速对照的取证口径）。
	DurationSeconds float64
}

// DriverFor 按 compose 服务声明裁决构建驱动（架构 §2.4：无 dockerfile →
// Railpack 自动；有 build.dockerfile → Dockerfile；仅 image → 镜像模式
// 直通，无构建）。第二返回值 false = 无构建（image 直通）。
func DriverFor(svc compose.Service) (state.Driver, bool, error) {
	switch {
	case svc.Build != nil && svc.Build.Dockerfile != "":
		return state.DriverDockerfile, true, nil
	case svc.Build != nil:
		return state.DriverRailpack, true, nil
	case svc.Image != "":
		return state.DriverPassthrough, false, nil
	default:
		return "", false, fmt.Errorf("build: service %s has neither build nor image (controlled-subset validation should have rejected this)", svc.Name)
	}
}

// ImageTag 返回构建产物 tag（可追溯：`<app>-<buildid>`，小写化——docker
// tag 字符集限制）。
func ImageTag(app, buildID string) (string, error) {
	tag := strings.ToLower(app + "-" + buildID)
	if !tagComponentRe.MatchString(tag) {
		return "", fmt.Errorf("build: image tag %q outside [a-z0-9._-]", tag)
	}
	return tag, nil
}

// ImageRef 返回本机镜像引用 `fleetly-local/<app>:<app>-<buildid>`。
func ImageRef(app, buildID string) (string, error) {
	tag, err := ImageTag(app, buildID)
	if err != nil {
		return "", err
	}
	repo := strings.ToLower(app)
	if !tagComponentRe.MatchString(repo) {
		return "", fmt.Errorf("build: app name %q not valid as image repo component", app)
	}
	return ImageRepoPrefix + "/" + repo + ":" + tag, nil
}

// ArtifactsDir 返回一次构建的产物目录（<artifacts>/<app>/<buildid>）。
func ArtifactsDir(base, app, buildID string) string {
	return filepath.Join(base, strings.ToLower(app), buildID)
}
