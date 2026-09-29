package runtime

// 控制面 TLS 基座（E7 同批 V2-8，设计 docs/design/2026-09-22-web-terminal.md
// §3.1/§3.2）：8420（HTTP：gateway + native 端点 + /ui）与 8421（gRPC）双面
// 同证书 TLS 化。off（缺省）= 今日明文行为逐字不变（NewControlPlaneTLS 返回
// nil，两个服务壳不加 TLS 选项）；platform = 复用平台证书（ingress 侧
// _fleetly-platform 多 SAN 单证书，LE 签发/续期由平台证书控制器既有机制承
// 接，D-MN-6）；manual = 显式证书文件对（重启生效——文档明示，设计原文）。
//
// 服务形态（关键裁决）：
//   - 证书供给 = tls.Config.GetCertificate 闭包读缓存证书（每握手零磁盘
//     IO）；platform 模式另有后台刷新循环（60s 周期重读落盘文件、按内容
//     指纹换缓存）——平台证书控制器续期落盘（tmp+rename 原子换入）后 60s
//     内生效，新握手即用新证书（在途连接不受影响——TLS 会话用旧证书直到
//     关闭，诚实语义）。
//   - platform 就绪次序 = 「listener 就绪、握手失败直到证书就绪」（8423
//     配置端点 TLS 面同款先例，ingress_service.go）：装配期不阻塞等签发，
//     证书未就绪时 GetCertificate 报错、握手失败；刷新循环检测到落盘证书
//     即装入缓存并打 info 日志。
//   - manual 就绪次序 = 装配期一次性加载（读不了/解析不了即 loud-fail 拒
//     绝启动）；运行期不重读——证书更换需重启生效（设计 §3.1 原文）。
//   - exec 反向通道的 TLS 参数面（设计 §3.3）由 ingress 侧导出的
//     PlatformCertPaths/PlatformTLSName 承接（S6 relay 消费，本阶段只导出）。

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/lynx-go/lynx"

	"github.com/fleetlyrun/fleetly/internal/ingress"
)

// tlsCertRefreshInterval 是 platform 模式证书缓存的后台刷新周期（设计 §3.1
// 「续期后热重载」的最小诚实形态——周期比对，事件回调留给未来）。测试经
// ControlPlaneTLS.refreshInterval 字段注入缩短。
const tlsCertRefreshInterval = 60 * time.Second

// ControlPlaneTLS 是控制面双面 TLS 的证书供给器：持有归一配置与证书缓存，
// 向 HTTP/gRPC 服务壳输出 *tls.Config（nil = off，不加 TLS 选项）。
// goroutine 安全（缓存有锁；TLSConfig 可在装配期一次性调用）。
type ControlPlaneTLS struct {
	// mode 是归一后的模式（off|platform|manual）。
	mode string
	// log 为空安全（缓存刷新日志的载体；测试可注入 io.Discard）。
	log *slog.Logger
	// cache 是证书缓存（manual/platform 共用；off 时 nil）。
	cache *tlsCertCache
	// refreshInterval 是 platform 模式刷新周期（0 = 不刷新——off/manual）。
	refreshInterval time.Duration
	// stop 停掉刷新循环（Wire cleanup 挂 OnPostStop；off/manual 为 nil）。
	stop func()
}

// NewControlPlaneTLS 构造控制面 TLS 供给器（Wire 装配点；返回 nil = off，
// 服务壳不加 TLS 选项）。校验失败（mode 未知 / platform 缺 base_domain /
// manual 文件缺或不可读 / min_version 未知）返回错误——装配期 loud-fail
// 拒绝启动（设计 §3.1 校验口径）。
func NewControlPlaneTLS(app lynx.App, cfg *AppConfig, ing *ingress.Manager) (*ControlPlaneTLS, func(), error) {
	log := app.Logger()
	if err := cfg.ValidateControlPlaneTLS(); err != nil {
		return nil, nil, err
	}
	switch cfg.TLSMode() {
	case ControlPlaneTLSOff:
		// cleanup 恒非 nil：wire 聚合清理闭包无条件调用（wire_gen.go 多处
		// cleanup10()），off 返回 nil 会让 SIGTERM 优雅停机在 runPostStopHooks
		// 里对 nil 函数值调用 panic（TLS-off 是缺省形态——smoke.sh 优雅停机
		// 断言实爆；manual 分支同病同修）。
		return nil, func() {}, nil
	case ControlPlaneTLSManual:
		cache := newTLSCertCache(cfg.ControlPlane.TLS.CertFile, cfg.ControlPlane.TLS.KeyFile).
			withMinVersion(cfg.TLSMinVersion())
		if err := cache.Load(); err != nil {
			return nil, nil, fmt.Errorf("control_plane.tls: %w", err)
		}
		log.Info("control plane TLS enabled (manual certificate; changes take effect on restart)",
			"cert_file", cfg.ControlPlane.TLS.CertFile)
		return &ControlPlaneTLS{
			mode:  ControlPlaneTLSManual,
			log:   log,
			cache: cache,
		}, func() {}, nil
	default: // platform——ValidateControlPlaneTLS 已保证 base_domain 非空。
		certFile, keyFile, err := ing.PlatformCertPaths()
		if err != nil {
			return nil, nil, fmt.Errorf("control_plane.tls: %w", err)
		}
		cache := newTLSCertCache(certFile, keyFile).withMinVersion(cfg.TLSMinVersion())
		// platform 模式初始加载允许失败（证书控制器可能尚未签发）——监听
		// 照常就绪，握手失败直到缓存装入（「listener 就绪、握手失败直到
		// 证书就绪」的诚实形态）；刷新循环每拍重试并日志告警。
		if err := cache.Load(); err != nil {
			log.Warn("control plane TLS enabled (platform certificate not yet on disk; TLS handshakes will fail until it is issued)",
				"error", err)
		}
		t := &ControlPlaneTLS{
			mode:            ControlPlaneTLSPlatform,
			log:             log,
			cache:           cache,
			refreshInterval: tlsCertRefreshInterval,
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go t.refreshLoop(ctx, done)
		t.stop = func() { cancel(); <-done }
		log.Info("control plane TLS enabled (platform certificate, hot-reloaded)",
			"cert_file", certFile)
		return t, t.stop, nil
	}
}

// TLSConfig 输出服务端 TLS 配置（8420/8421 双面共用一份）：MinVersion 按
// 配置（缺省 TLS 1.2）；证书经 GetCertificate 闭包读缓存（每握手零磁盘
// IO；未就绪返回错误 → 握手失败）。返回新实例（gRPC credentials.NewTLS 与
// http.Server 各自持有副本，不共享可变态）。
func (t *ControlPlaneTLS) TLSConfig() *tls.Config {
	return &tls.Config{ //nolint:gosec // G402：MinVersion 显式按配置装配（缺省 TLS 1.2）
		MinVersion: t.minVersion(),
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return t.cache.Get()
		},
	}
}

// minVersion 是配置化的协议下限（off 形态不会被调用——无 TLSConfig 消费方）。
func (t *ControlPlaneTLS) minVersion() uint16 { return t.cache.minVersion }

// refreshLoop 是 platform 模式的证书缓存刷新循环：每拍重读落盘文件，内容
// 指纹变化才换缓存（续期落盘 → ≤60s 生效）；读失败保留旧证书（可用性优先
// ——短暂读抖动不掐断服务面），首次装入缓存时打 info（「TLS 面自此可握
// 手」的可观测锚点）。ctx 取消即退出（done 收口）。
func (t *ControlPlaneTLS) refreshLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(t.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		wasReady := t.cache.Ready()
		changed, err := t.cache.Refresh()
		switch {
		case err != nil:
			if !wasReady {
				// 仍未就绪：握手持续失败中，按拍告警（运维可见的等待信号）。
				t.log.Warn("control plane TLS certificate still not ready (handshakes keep failing)", "error", err)
			} else {
				// 已就绪后的读失败：保留旧证书继续服务，仅告警。
				t.log.Warn("control plane TLS certificate refresh failed (serving the previously loaded certificate)", "error", err)
			}
		case changed && !wasReady:
			t.log.Info("control plane TLS certificate ready; the TLS faces now complete handshakes")
		case changed:
			t.log.Info("control plane TLS certificate reloaded (renewal picked up; new connections use the new certificate)")
		}
	}
}

// tlsCertCache 是单证书对（PEM 文件路径对）的内容指纹缓存：GetCertificate
// 闭包的取数出口。Load/Refresh 落盘重读 + sha256 指纹比对；Get 只读返回
// （未装入返回错误——platform 就绪前握手的诚实失败面）。
type tlsCertCache struct {
	certFile string
	keyFile  string
	// minVersion 透传进使用方 tls.Config（缓存随证书生命周期一起管理配置面）。
	minVersion uint16

	mu   sync.RWMutex
	cert *tls.Certificate
	fp   string
}

// newTLSCertCache 构造缓存（不读盘——Load/Refresh 才触文件系统）。
func newTLSCertCache(certFile, keyFile string) *tlsCertCache {
	return &tlsCertCache{certFile: certFile, keyFile: keyFile, minVersion: tls.VersionTLS12}
}

// withMinVersion 设置协议下限（装配点链式形态；NewControlPlaneTLS 消费）。
func (c *tlsCertCache) withMinVersion(v uint16) *tlsCertCache {
	c.minVersion = v
	return c
}

// Load 初始加载（manual 装配期 fail-fast / platform 装配期尽力而为）。
func (c *tlsCertCache) Load() error {
	_, err := c.Refresh()
	return err
}

// Refresh 重读落盘证书并按需换缓存：读/解析失败返回错误（缓存原样保留
// ——「保留旧证书继续服务」语义的承载点）；成功且内容指纹变化 → 换缓存并
// 返回 true。tmp+rename 原子写（ingress certStore.Save）保证读不到半写态。
func (c *tlsCertCache) Refresh() (bool, error) {
	certPEM, err := os.ReadFile(c.certFile)
	if err != nil {
		return false, fmt.Errorf("read certificate %s: %w", c.certFile, err)
	}
	keyPEM, err := os.ReadFile(c.keyFile)
	if err != nil {
		return false, fmt.Errorf("read key %s: %w", c.keyFile, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, fmt.Errorf("parse certificate pair %s/%s: %w", c.certFile, c.keyFile, err)
	}
	sum := sha256.Sum256(certPEM)
	fp := hex.EncodeToString(sum[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && c.fp == fp {
		return false, nil
	}
	c.cert = &cert
	c.fp = fp
	return true, nil
}

// Ready 报告缓存是否已装入证书（platform 模式刷新日志区分「尚未就绪」与
// 「就绪后读失败」）。
func (c *tlsCertCache) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cert != nil
}

// Get 返回缓存证书（GetCertificate 闭包出口）：未装入返回错误——握手失败
// 即「TLS 面尚不可用」（platform 等待签发的诚实形态；manual 装配期已保证
// 非空，本分支对 manual 不可达）。
func (c *tlsCertCache) Get() (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cert == nil {
		return nil, fmt.Errorf("control plane TLS certificate not ready (platform mode waits for the certificate controller to issue it)")
	}
	return c.cert, nil
}
