package build

// Config 是构建管线配置（daemon config build.* 节的核心形态）。缺省值即
// 保守基线：并发 2（架构 §4.2 容量边界「并发构建 2」）、限额 1GiB/1.5CPU
// （Spike A E4 实证可在该预算内完成真实构建）、双缓存持久化（见下）。
//
// 缓存机制（Spike A「缓存是默认能力不是优化」；实机勘误后的正确形态）：
// buildkit local cache 的导入/导出是客户端侧行为（经 solve session 流式
// 传输，路径解析在 fleetlyd 宿主而非 buildkitd 容器内）。持久化因此分两
// 层，各自承载一半语义：
//   - CacheVolume（命名卷）挂进 buildkitd 容器的内部工作缓存目录
//     /var/lib/buildkit——层缓存随容器重建不丢（moby/buildkit 镜像对该
//     路径的匿名卷在容器删除即失，命名卷是持久化手段）；
//   - CacheDir（fleetlyd 宿主目录）是 local cache 导入/导出的数据根——
//     跨 buildkitd 容器重建、跨实例可移植的缓存层（Spike A E2b 的
//     registry-cache 形态在 v0.1 单机上的等价物）。

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// buildkitd 资源与命名的平台缺省（Spike A 钉版与实证值）。
const (
	// DefaultBuildkitHost 是 buildkitd 连接端点缺省（docker-container://
	// connhelper 经 docker CLI exec buildctl dial-stdio）。
	DefaultBuildkitHost = "docker-container://" + DefaultDaemonContainerName
	// DefaultDaemonContainerName 是平台自管 buildkitd 容器名。
	DefaultDaemonContainerName = "fleetly-buildkit"
	// BuildkitImage 是 buildkitd 钉版镜像（与 Spike A 一致；升级 = 独立
	// 变更票，不跟随 railpack 传递依赖漂移）。
	BuildkitImage = "moby/buildkit:v0.32.2"
	// DefaultCacheVolume 是 buildkitd 内部工作缓存的持久化命名卷（挂载点
	// InternalCacheMountPath；容器重建不丢层缓存）。
	DefaultCacheVolume = "fleetly-buildkit-cache"
	// InternalCacheMountPath 是 buildkitd 容器内内部工作缓存目录（镜像
	// VOLUME 声明路径；命名卷持久化的挂载点）。
	InternalCacheMountPath = "/var/lib/buildkit"
	// DefaultCacheDir 是 local cache 导入/导出的宿主目录缺省（客户端侧
	// 数据根；相对 fleetlyd 工作目录，与 db_path/artifacts_dir 同约定）。
	DefaultCacheDir = "./build-cache"
	// DefaultConcurrency 是构建队列并发上限缺省（架构 §4.2：并发构建 2）。
	DefaultConcurrency = 2
	// MaxConcurrency 是并发上限配置天花板（防误配打穿宿主；超限取上限）。
	MaxConcurrency = 8
	// DefaultMemoryBytes / DefaultNanoCPUs 是 buildkitd 容器资源限额缺省
	// （Spike A E4 实证：1GiB/1.5CPU 下 node 冷构建可完成、峰值 59% 内存
	// 天花板）。
	DefaultMemoryBytes = int64(1) << 30
	DefaultNanoCPUs    = int64(1_500_000_000)
	// DefaultPollInterval 是队列扫描 builds queued 行的周期（CLI 入队为
	// 跨进程通道；wake 信号提供同进程加速路径）。
	DefaultPollInterval = 2 * time.Second
	// DefaultArtifactsDir 是产物归档缺省根目录（相对 daemon 工作目录）。
	DefaultArtifactsDir = "./build-artifacts"
	// DefaultTimeout 是单条构建执行的超时预算缺省（30min）。预算覆盖执行
	// 全程（buildkitd 就绪收敛（自身 ≤6min）+ solve + inspect）；到点未完
	// 成即终态 failed——挂起的执行（网络挂起/buildkitd 半死）不得永久占用
	// 并发槽（并发 2 时两个挂起即堵死全队列）。
	DefaultTimeout = 30 * time.Minute
)

// Config 是构建管线配置。零值经 Normalize 回落缺省。
type Config struct {
	// BuildkitHost 是 buildkit 端点（docker-container://<name> 或
	// tcp://…/unix://… 直连外部 buildkitd——隔离升级路径）。
	BuildkitHost string
	// ManageDaemon 报告是否由平台自管 buildkitd 容器（EnsureRunning 幂等
	// 拉起钉版容器 + 限额 + 内部缓存卷）。指向外部 buildkitd 时应置 false。
	ManageDaemon bool
	// DaemonContainerName 是自管 buildkitd 容器名。
	DaemonContainerName string
	// CacheVolume 是 buildkitd 内部工作缓存的持久化命名卷（缺省）。
	CacheVolume string
	// CacheDir 是 local cache 导入/导出的宿主目录（客户端侧数据根）。
	CacheDir string
	// ArtifactsDir 是 plan JSON / 构建日志归档根目录。
	ArtifactsDir string
	// Concurrency 是构建队列并发上限（≤0 取缺省 2，天花板 8）。
	Concurrency int
	// MemoryBytes / NanoCPUs 是 buildkitd 容器 cgroup 限额。
	MemoryBytes int64
	NanoCPUs    int64
	// PollInterval 是队列 DB 扫描周期。
	PollInterval time.Duration
	// Timeout 是单条构建执行的超时预算（≤0 取缺省 30min；超时 → 终态
	// failed（E_BUILD_FAILED），由队列在认领执行处包裹生效）。
	Timeout time.Duration
	// ContextRoots 是构建上下文的受管根集合（H14 宿主目录信任边界）：
	// 执行侧只接受位于受管根内的 context_dir——builds.request 是跨进程
	// 通道，直写库的越界请求不得把宿主任意目录整目录打进镜像。Normalize
	// 保证系统 temp 根恒在集合内（API 层 compose 暂存/回落基准的落点，
	// MkdirTemp("", …) 所在）；daemon 装配再并入 git 根（gitserver 裸
	// 仓库根，v0.2 worktree 物化路径的前缀形态）与显式配置根。越界构建
	// 终态失败（不静默放宽）。v0.2 挂账：CLI 同宿主构建目录（显式
	// base_dir）改为上传/物化到受管根后，该集合即对全部入队来源完备闭合。
	ContextRoots []string
	// RegistryHost 是平台 registry 主机（registry.<base>；E1-5，base_domain
	// 派生，装配期注入）。空 = 本地模式：v0.1 本地 digest 管线逐字不变
	//（无推送、本地 fleetly-local/<app> 引用）。非空 = registry 模式：构建
	// 产物 push 到 <host>/apps/<app>:b<buildid>，builds.image_ref 记
	// <host>/apps/<app>@sha256:<digest>（设计 §2.5/D-MN-11）。
	RegistryHost string
	// RegistryAuthFile 是平台 registry 凭据文件路径（registry.auth_file；
	// `<user>:<password>` 单行 0600，ingress 部署器生成）。registry 模式
	// 构建执行时读取（凭据可能晚于装配期才生成——zot 部署 duty 的产物），
	// 经 buildkit session 注入推送凭据；文件缺失/损坏 → 构建终态失败
	//（E_REGISTRY_PUSH_FAILED）。
	RegistryAuthFile string
	// UploadsRoot 是上传构建的会话根目录（<数据根>/build-uploads；
	// IMPL-T2-2/DT-6：上传上下文落盘、终态清理与受根约束 RemoveAll 的
	// 锚点）。空 = 上传面未装配（BuildFromUpload 如实报不可用）；非空经
	// Normalize 转绝对路径并恒并入 ContextRoots（H14 受管根）。
	UploadsRoot string
	// MaxUploadBytes 是单次上传 tar 字节上限（build.max_upload_mb 派生；
	// ≤0 回落 DefaultMaxUploadBytes）。
	MaxUploadBytes int64
}

// Normalize 回落全部缺省（config 缺省值单一事实源）。
func (c Config) Normalize() Config {
	if c.BuildkitHost == "" {
		c.BuildkitHost = DefaultBuildkitHost
	}
	if c.DaemonContainerName == "" {
		c.DaemonContainerName = DefaultDaemonContainerName
	}
	if c.CacheVolume == "" {
		c.CacheVolume = DefaultCacheVolume
	}
	if c.CacheDir == "" {
		c.CacheDir = DefaultCacheDir
	}
	if c.ArtifactsDir == "" {
		c.ArtifactsDir = DefaultArtifactsDir
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.Concurrency > MaxConcurrency {
		c.Concurrency = MaxConcurrency
	}
	if c.MemoryBytes <= 0 {
		c.MemoryBytes = DefaultMemoryBytes
	}
	if c.NanoCPUs <= 0 {
		c.NanoCPUs = DefaultNanoCPUs
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = DefaultMaxUploadBytes
	}
	if c.UploadsRoot != "" {
		if abs, err := filepath.Abs(c.UploadsRoot); err == nil {
			c.UploadsRoot = abs
		}
	}
	c.ContextRoots = normalizeContextRoots(append(c.ContextRoots, c.UploadsRoot))
	return c
}

// normalizeContextRoots 归一受管根集合（H14）：逐根 Abs+Clean、去重；
// 系统 temp 根（os.TempDir——API 层 compose 暂存目录与回落基准的落点）
// 恒在集合内（受管根的最小闭包，缺省集合即 [系统 temp 根]）。空串根
// 丢弃；不可解析根（Abs 失败，理论不可达）同样丢弃——fail-closed 方向
// 是收窄可用根，不中断装配。
func normalizeContextRoots(roots []string) []string {
	out := make([]string, 0, len(roots)+1)
	seen := map[string]bool{}
	add := func(raw string) {
		if raw == "" {
			return
		}
		abs, err := filepath.Abs(raw)
		if err != nil {
			return
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	add(os.TempDir())
	for _, r := range roots {
		add(r)
	}
	return out
}

// containsPath 报告 dir 是否位于 root 之内（词法判定：filepath.Rel 无错
// 且不以前导 .. 越出；dir == root 视为在内）。已知保真度边界（挂账
// v0.2）：Windows 卷内大小写差异走词法精确比对，形态不一致时判外——
// fail-closed（宁可误拒可见、不可误放）。
func containsPath(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false // 跨卷等不可比较形态：对该根不成立
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
