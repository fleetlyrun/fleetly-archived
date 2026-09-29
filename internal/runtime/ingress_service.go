package runtime

// ingress 服务的 lynx.Service 装配壳（T2.15/T2.16；E1-3 增 8423 TLS 面）：
// 配置端点（独立内部端口——取舍见 internal/ingress/provider.go）+ Traefik
// 收敛/续期扫描周期任务（Run，后台降级语义——swarm 未就绪/镜像拉取失败
// 只日志告警，下轮 sweep 重试；入口故障不拖垮控制面 readiness）。
//
// Start 顺序：先起配置端点监听（Traefik 首次拉取的前置），base_domain 非
// 空时再起 8423 TLS 面（同 /configs 载荷、平台证书服务——证书未就绪时
// GetCertificate 报错、握手失败，Traefik 容忍期语义承接），后台跑 Run
// （收敛 + 续期 + 平台证书控制器）。Stop 关停 HTTP server（明文 + TLS）。

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/lynx-go/lynx"

	"github.com/fleetlyrun/fleetly/internal/ingress"
)

// ingressService 是入口服务壳。
type ingressService struct {
	mgr  *ingress.Manager
	log  *slog.Logger
	srv  *http.Server
	addr string
	// tlsAddr 是 8423 TLS 面监听地址（ingress.config_tls_addr；仅
	// ConfigTLSEnabled 时装配，单节点零新监听）。
	tlsAddr string
	// tlsSrv 是 TLS 面 server（Stop 与明文面同预算关停）。
	tlsSrv *http.Server
}

func newIngressService(m *ingress.Manager, app lynx.App, addr, tlsAddr string) lynx.Service {
	return &ingressService{mgr: m, log: app.Logger(), addr: addr, tlsAddr: tlsAddr}
}

func (s *ingressService) Name() string { return "ingress.traefik" }

// Init 无动作（token/handler 构造延迟到 Start 的监听路径内——装配期不碰
// 文件系统生成的 token）。
func (s *ingressService) Init(_ lynx.AppContext) error { return nil }

// tlsConfig 构造 8423 面的 TLS 配置：证书经 GetCertificate 动态取自平台
// 证书（未就绪时返回错误 → 握手失败 = 「8423 尚不可用」，设计 §2.4 次序
// ③④）；TLS 1.2 起步（gosec 基线 + Traefik 客户端能力富余）。
func (s *ingressService) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.mgr.PlatformTLSCertificate()
		},
	}
}

func (s *ingressService) Start(ctx context.Context) error {
	handler, err := s.mgr.Handler(ctx)
	if err != nil {
		return fmt.Errorf("ingress: construct provider handler: %w", err)
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("ingress: listen config endpoint %s: %w", s.addr, err)
	}
	// 回填端口（ConfigAddr 端口位为 0 的动态端口形态；挑战应答 URL 依赖）。
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		s.mgr.SetConfigPort(tcp.Port)
	}
	s.srv = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 2)
	go func() {
		if serr := s.srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			serveErr <- serr
		}
	}()
	s.log.Info("ingress: config endpoint listening", "addr", ln.Addr().String())

	// 8423 TLS 面（E1-3）：base_domain 非空才装配（单节点零行为差异）。
	// 监听失败显式失败——显式启用的面装配不成即多节点形态残缺，不静默
	// 降级；证书未就绪不阻塞监听（握手期失败，Traefik 原生容忍）。
	if s.mgr.ConfigTLSEnabled() {
		tlsHandler, terr := s.mgr.TLSHandler(ctx)
		if terr != nil {
			return fmt.Errorf("ingress: construct TLS provider handler: %w", terr)
		}
		tln, lerr := net.Listen("tcp", s.tlsAddr)
		if lerr != nil {
			return fmt.Errorf("ingress: listen config TLS endpoint %s: %w", s.tlsAddr, lerr)
		}
		s.tlsSrv = &http.Server{Handler: tlsHandler, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if serr := s.tlsSrv.Serve(tls.NewListener(tln, s.tlsConfig())); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				serveErr <- serr
			}
		}()
		s.log.Info("ingress: config TLS endpoint listening (platform certificate served once issued)",
			"addr", tln.Addr().String())
	}

	// 后台收敛与续期（Run 随 ctx 取消返回；sweep 内部降级——不阻塞
	// 服务 Start，Traefik 未就绪不影响控制面 readiness）。
	go func() { _ = s.mgr.Run(ctx) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("ingress: config endpoint serve: %w", err)
		}
		<-ctx.Done()
		return nil
	}
}

// Stop 关停配置端点（明文 + TLS 面，各预算 5s；Traefik 侧「配置服务不可
// 达保留旧配置」语义保证入口不坏，Spike B d 态实测）。
func (s *ingressService) Stop(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var firstErr error
	if s.srv != nil {
		if err := s.srv.Shutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.tlsSrv != nil {
		if err := s.tlsSrv.Shutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CheckHealth 恒健康（配置端点启动失败 = Start 阶段显式失败，readiness
// 不需要二次上报；Traefik 收敛态由 sweep 日志与 fleetly ingress status
// 呈现，不进 readiness——入口是降级设计）。
func (s *ingressService) CheckHealth() error { return nil }
