package substrate

// 平台 registry（zot）前哨与 swarm 凭据分发的 HTTP 适配（E1 多节点设计
// §2.5/D-MN-11，E1-4/E1-5）：
//
//   - ManifestHead 实现 build.RegistryClient 端口（manifest HEAD）——registry
//     模式部署前哨的底座腿：命中返回 Docker-Content-Digest；404 归一为
//     build.ErrImageNotFound（「manifest 缺失复用 E_IMAGE_UNAVAILABLE」的
//     归因来源）；其余失败原样返回（E_REGISTRY_UNAVAILABLE 信封归一在
//     build.PreflightRegistry）。HTTP 客户端属第三方适配面，不出本包。
//
//   - swarm 凭据分发（`--with-registry-auth` 语义，设计 §2.5「服务创建/
//     更新：registry 模式经 --with-registry-auth 携带 zot 凭据」）：平台
//     registry 的 Basic Auth 凭据以 X-Registry-Auth 编码头随 service
//     create/update 提交，Swarm 原生分发到拉取节点。引擎不感知凭据存在
//     ——适配器在镜像引用命中平台 registry（build.IsRegistryImageRef）时
//     自动附带，凭据经装配期注入的惰性读取函数现读（zot 部署 duty 可能
//     晚于装配期生成凭据文件；轮换后新部署即刻生效）。
//
//   - 外部 registry 解析与凭证（IMPL-T1-2/DT-2）：tag 引用经
//     imageregistry.Client 解析为 manifest digest（匿名或平台设置凭证
//     命中）；外部引用命中平台设置 host 时 service create/update 附带
//     X-Registry-Auth（私有镜像逐节点拉取的成功条件）。凭证只进
//     Authorization/编码头，绝不进日志与错误文本。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/registry"

	"github.com/fleetlyrun/fleetly/internal/build"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/imageregistry"
)

// 编译期断言：Client 隐式实现 build.RegistryClient 端口（manifest HEAD
// 前哨；第三方 HTTP 细节不出本包的结构性证明）。
var _ build.RegistryClient = (*Client)(nil)

// manifestHeadTimeout 是单次 manifest HEAD 的拨号预算（前哨是部署路径的
// 快速失败面——黑洞端点下挂满预算即判不可达，不排队）。
const manifestHeadTimeout = 10 * time.Second

// registryHeadAccept 是 registry v2 manifest 请求的 Accept 形态集（index 与
// 两种 manifest——digest 钉定引用的 HEAD 命中任意一层均有效）。
var registryHeadAccept = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// WithPlatformRegistry 装配平台 registry 前哨与凭据分发（E1-4/E1-5；
// base_domain 非空时由 runtime 装配调用，单节点不调——零行为差异）：
//   - host：registry.<base>（build.IsRegistryImageRef 的判定前缀；携带
//     "://" 前缀时按绝对基址原样使用——测试注入缝，生产形态无 scheme
//     即 https）；
//   - creds：凭据惰性读取函数（registry.auth_file 现读；轮换后新部署
//     即刻生效；文件未生成时的读取失败按调用点语义显式失败）。
func (c *Client) WithPlatformRegistry(host string, creds func() (build.RegistryCredentials, error)) *Client {
	c.registryHost = host
	c.registryCreds = creds
	return c
}

// platformRegistryEnabled 报告平台 registry 适配是否已装配（单节点 = false：
// ImageDigest 走本机 inspect、service 写不附凭据——v0.1 等价断言的谓词面）。
func (c *Client) platformRegistryEnabled() bool { return c != nil && c.registryHost != "" }

// registryBaseURL 返回 manifest HEAD 的基址：显式 scheme 原样（测试注入），
// 否则 https（生产形态：registry.<base> 经 Traefik 443 公信 CA 面）。
func registryBaseURL(host string) string {
	if strings.Contains(host, "://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + host
}

// splitRegistryRef 把 `<host>/apps/<app>@sha256:<hex>` 拆成 registry v2 的
// manifest 端点 URL（`<base>/v2/<name>/manifests/<reference>`）。非 registry
// 形态返回错误（调用点先经 IsRegistryImageRef 把门）。
func splitRegistryRef(ref, host string) (string, error) {
	if !build.IsRegistryImageRef(ref, host) {
		return "", fmt.Errorf("substrate: %s is not a platform registry reference (host %s)", ref, host)
	}
	at := strings.LastIndex(ref, "@sha256:")
	if at < 0 {
		return "", fmt.Errorf("substrate: %s carries no sha256 digest (not a digest-pinned registry reference)", ref)
	}
	name := strings.TrimPrefix(ref[:at], host+"/")
	reference := ref[at+1:]
	return registryBaseURL(host) + "/v2/" + name + "/manifests/" + reference, nil
}

// ManifestHead 实现 build.RegistryClient 端口：对 digest 钉定的平台 registry
// 引用做 manifest HEAD。D2 前哨预算：自 10s 拨号预算（黑洞端点快速判不可
// 达），不与 docker API 的 defaultCallTimeout 共用。
func (c *Client) ManifestHead(ctx context.Context, ref string) (string, error) {
	if !c.platformRegistryEnabled() {
		return "", fmt.Errorf("substrate: registry preflight requires platform registry wiring (base_domain)")
	}
	creds, err := c.registryCreds()
	if err != nil {
		return "", fmt.Errorf("substrate: registry credentials: %w", err)
	}
	url, err := splitRegistryRef(ref, c.registryHost)
	if err != nil {
		return "", err
	}
	hctx, cancel := context.WithTimeout(ctx, manifestHeadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodHead, url, nil)
	if err != nil {
		return "", fmt.Errorf("substrate: registry request %s: %w", ref, err)
	}
	for _, a := range registryHeadAccept {
		req.Header.Add("Accept", a)
	}
	req.SetBasicAuth(creds.User, creds.Password)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("substrate: registry head %s: %w", ref, err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body) // 连接复用要求排空（HEAD 通常无体）
	switch res.StatusCode {
	case http.StatusOK:
		digest := res.Header.Get("Docker-Content-Digest")
		if digest == "" {
			return "", fmt.Errorf("substrate: registry head %s: response carried no Docker-Content-Digest", ref)
		}
		return digest, nil
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: %s", build.ErrImageNotFound, ref)
	default:
		return "", fmt.Errorf("substrate: registry head %s: unexpected status %s", ref, res.Status)
	}
}

// encodedRegistryAuth 把凭据编码为 X-Registry-Auth 头形态（AuthConfig JSON
// 的 base64url——docker API 的 service create/update 原生格式）。
func encodedRegistryAuth(host string, creds build.RegistryCredentials) (string, error) {
	//nolint:gosec // G117：X-Registry-Auth 头的 Docker 原生形态就是 AuthConfig JSON+base64，序列化即协议要求
	raw, err := json.Marshal(registry.AuthConfig{
		Username:      creds.User,
		Password:      creds.Password,
		ServerAddress: host,
	})
	if err != nil {
		return "", fmt.Errorf("substrate: encode registry auth: %w", err)
	}
	return base64.URLEncoding.EncodeToString(raw), nil
}

// registryAuthForImage 计算镜像引用应附带的 X-Registry-Auth 头（E1-5 +
// IMPL-T1-2/DT-2）：
//   - 平台 registry 引用 → 平台 zot 凭据现读 + 编码（既有语义）；
//   - 外部 registry 引用命中平台设置 host 且凭证在位 → 设置凭证编码
//     （私有镜像逐节点拉取的成功条件）；其余外部镜像（本地 fleetly-local/…、
//     未配置 host、匿名命中）→ 空串（凭据只分发给需要的 service，不向全
//     集群广播）。
//
// 凭据读取失败显式报错——部署中途拉取失败比入队时失败更难定位
//（fail-closed）。
func (c *Client) registryAuthForImage(image string) (string, error) {
	if c.platformRegistryEnabled() && build.IsRegistryImageRef(image, c.registryHost) {
		creds, err := c.registryCreds()
		if err != nil {
			return "", fmt.Errorf("substrate: registry credentials for %s: %w", image, err)
		}
		return encodedRegistryAuth(c.registryHost, creds)
	}
	settings, err := c.externalRegistrySettings()
	if err != nil {
		return "", fmt.Errorf("substrate: registry credentials for %s: %w", image, err)
	}
	if settings.Host == "" || (settings.Username == "" && settings.Password == "") {
		return "", nil
	}
	parsed, err := imageregistry.Parse(image)
	if err != nil {
		// 形态不可解析（本机镜像 ID 等）：零凭据面（不猜测目标 registry）。
		return "", nil
	}
	if imageregistry.NormalizeHost(settings.Host) != parsed.Host {
		return "", nil
	}
	return encodedRegistryAuth(parsed.Host, build.RegistryCredentials{
		User: settings.Username, Password: settings.Password,
	})
}

// ExternalRegistrySettings 是平台 registry credentials 设置面的快照
//（registry.* 设置 + Box 解密后的明文密码；明文只在调用瞬间存活——本包
// 不做持久化读，装配层每次现读现解密）。
type ExternalRegistrySettings struct {
	Host     string
	Username string
	Password string
}

// WithImageRegistryCredentials 装配外部 registry 凭证设置读取缝
//（IMPL-T1-2/DT-2；runtime 注入：state registry.* 设置现读 + Box 解密；
// nil = 未装配——解析恒匿名、auth 恒空）。读取函数每次现读；失败显式
//（作为解析腿失败原因回落本机 inspect，不静默降级为匿名）。
func (c *Client) WithImageRegistryCredentials(fn func() (ExternalRegistrySettings, error)) *Client {
	c.externalRegistryCredentials = fn
	return c
}

// WithImageRegistryTrace 装配「registry 腿失败回落本机 inspect」的留痕缝
//（nil = 静默——测试形态；runtime 注入 slog.Warn）。
func (c *Client) WithImageRegistryTrace(trace func(msg string, args ...any)) *Client {
	c.imageRegistryTrace = trace
	return c
}

// externalRegistryResolveTimeout 是单次外部 registry 解析的预算（黑洞
// registry 下不挂满部署 tick；与平台 zot 前哨的 10s 拨号预算同口径）。
// 包级变量即可配（测试注入缩短预算，defaultCallTimeout 同款惯例）。
var externalRegistryResolveTimeout = 10 * time.Second

// defaultImageRegistryClient 是外部 registry 解析的生产实现（无状态，
// 包级复用；HTTP 细节在 internal/imageregistry）。
var defaultImageRegistryClient = imageregistry.NewClient()

// externalRegistrySettings 调用装配缝（未装配 = 零值快照——恒匿名）。
func (c *Client) externalRegistrySettings() (ExternalRegistrySettings, error) {
	if c.externalRegistryCredentials == nil {
		return ExternalRegistrySettings{}, nil
	}
	return c.externalRegistryCredentials()
}

// resolveTagDigestForImage 执行 registry-first 解析（DT-2 两句话的落地）：
// 宿主命中平台设置 host 且凭证在位 → 携凭证（私有镜像）；否则匿名（公共
// 镜像零预拉）。任何失败原样返回（调用点回落本机 inspect）。
func (c *Client) resolveTagDigestForImage(ctx context.Context, ref imageregistry.Reference) (string, error) {
	settings, err := c.externalRegistrySettings()
	if err != nil {
		return "", fmt.Errorf("platform registry credential settings unreadable: %w", err)
	}
	var creds *imageregistry.Credentials
	if settings.Host != "" && imageregistry.NormalizeHost(settings.Host) == ref.Host &&
		(settings.Username != "" || settings.Password != "") {
		creds = &imageregistry.Credentials{Username: settings.Username, Password: settings.Password}
	}
	resolve := c.resolveTagDigest
	if resolve == nil {
		resolve = defaultImageRegistryClient.Resolve
	}
	rctx, cancel := context.WithTimeout(ctx, externalRegistryResolveTimeout)
	defer cancel()
	digest, err := resolve(rctx, ref, creds)
	if err != nil {
		return "", fmt.Errorf("registry %s: %w", ref.Host, err)
	}
	if digest == "" {
		return "", fmt.Errorf("registry %s returned an empty manifest digest for %s", ref.Host, ref.String())
	}
	return digest, nil
}

// inspectLocalImageDigest 执行本机 inspect 腿（airgap 不回归的底座腿）：
// 优先取 RepoDigests 的清单摘要（swarm 分发解析只接受 manifest digest）；
// 本机构建镜像（无清单摘要）返回空串由引擎按 tag 直用（旧语义）。缺失
// 归一为 engine.ErrImageMissing。
func (c *Client) inspectLocalImageDigest(ctx context.Context, ref string) (string, error) {
	if c.inspectDigest != nil {
		return c.inspectDigest(ctx, ref)
	}
	ictx, cancel := withCallTimeout(ctx) // D2：非流式 per-call 超时
	defer cancel()
	res, err := c.cli.ImageInspect(ictx, ref)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", fmt.Errorf("%w: %s", engine.ErrImageMissing, ref)
		}
		return "", fmt.Errorf("substrate: image inspect %s: %w", ref, err)
	}
	for _, rd := range res.RepoDigests {
		if _, digest, ok := strings.Cut(rd, "@"); ok && strings.HasPrefix(digest, "sha256:") {
			return digest, nil
		}
	}
	return "", nil
}

// traceImageDigestFallback 在 registry 腿失败、本机 inspect 被启用时留痕
//（airgap/本地已有镜像的回落路径可观测；凭据材料零出现）。
func (c *Client) traceImageDigestFallback(ref string, registryErr error) {
	if c.imageRegistryTrace == nil || registryErr == nil {
		return
	}
	c.imageRegistryTrace("image digest: registry resolution failed, falling back to local inspect",
		"image", ref, "registry_error", registryErr.Error())
}
