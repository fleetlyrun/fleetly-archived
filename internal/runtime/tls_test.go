package runtime

// V2-8（E7 同批）控制面 TLS 基座测试：config 校验矩阵（三态 × base_domain
// × 文件存在性 × min_version）、证书缓存加载/轮换/损坏保留旧证书、
// GetCertificate 闭包的真握手供给（net.Pipe + tls.Client/server）与
// NewControlPlaneTLS 三态装配（off 返回 nil / manual fail-fast / platform
// 就绪前握手失败、落盘后可服务）。

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// writeTestCertPair 生成自签证书对落盘（dnsNames/IP 形态供客户端校验面），
// 返回 cert PEM 摘要（缓存指纹断言面）。
func writeTestCertPair(t *testing.T, certFile, keyFile string, dnsNames []string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "tls-test"},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	sum := sha256.Sum256(certPEM)
	return hex.EncodeToString(sum[:])
}

// TestValidateControlPlaneTLSMatrix 配置校验矩阵（设计 §3.1 校验口径）。
func TestValidateControlPlaneTLSMatrix(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	writeTestCertPair(t, certFile, keyFile, []string{"ctrl.example.test"})

	missing := filepath.Join(dir, "nope.pem")

	tests := []struct {
		name    string
		cfg     AppConfig
		wantErr bool
	}{
		{
			name: "off default (zero value) is valid and unchanged",
			cfg:  AppConfig{},
		},
		{
			name: "off ignores stray keys",
			cfg:  AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "off", CertFile: missing}}},
		},
		{
			name:    "unknown mode rejected",
			cfg:     AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "auto"}}},
			wantErr: true,
		},
		{
			name:    "platform requires base_domain (loud-fail)",
			cfg:     AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "platform"}}},
			wantErr: true,
		},
		{
			name: "platform with base_domain accepted",
			cfg:  AppConfig{BaseDomain: "example.test", ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "platform"}}},
		},
		{
			name:    "platform rejects unknown min_version",
			cfg:     AppConfig{BaseDomain: "example.test", ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "platform", MinVersion: "ssl3"}}},
			wantErr: true,
		},
		{
			name:    "manual requires both file paths",
			cfg:     AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "manual", CertFile: certFile}}},
			wantErr: true,
		},
		{
			name:    "manual rejects unreadable cert file",
			cfg:     AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "manual", CertFile: missing, KeyFile: keyFile}}},
			wantErr: true,
		},
		{
			name:    "manual rejects unreadable key file",
			cfg:     AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "manual", CertFile: certFile, KeyFile: missing}}},
			wantErr: true,
		},
		{
			name: "manual with readable pair accepted",
			cfg:  AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "manual", CertFile: certFile, KeyFile: keyFile}}},
		},
		{
			name: "manual accepts min_version tls1.3",
			cfg:  AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "manual", CertFile: certFile, KeyFile: keyFile, MinVersion: "tls1.3"}}},
		},
		{
			name:    "manual rejects unknown min_version",
			cfg:     AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{Mode: "manual", CertFile: certFile, KeyFile: keyFile, MinVersion: "tls1.1"}}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		err := tt.cfg.ValidateControlPlaneTLS()
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: ValidateControlPlaneTLS err=%v, wantErr=%v", tt.name, err, tt.wantErr)
		}
	}

	// min_version 归一：空串 = TLS 1.2（缺省基线）、tls1.2/tls1.3 直映射。
	var zero AppConfig
	if got := zero.TLSMinVersion(); got != tls.VersionTLS12 {
		t.Errorf("default min version = %x, want TLS 1.2", got)
	}
	var tls13 AppConfig
	tls13.ControlPlane.TLS.MinVersion = "tls1.3"
	if got := tls13.TLSMinVersion(); got != tls.VersionTLS13 {
		t.Errorf("tls1.3 min version = %x, want TLS 1.3", got)
	}
	if got := zero.TLSMode(); got != ControlPlaneTLSOff {
		t.Errorf("default mode = %q, want off", got)
	}
}

// TestTLSCertCacheRotation 假证书轮换：Load 就绪 → 换入新证书对 → Refresh
// 报告变化且 Get 供给新证书 → 内容未变时 Refresh 报告无变化（指纹幂等）。
func TestTLSCertCacheRotation(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	fpA := writeTestCertPair(t, certFile, keyFile, []string{"a.test"})

	cache := newTLSCertCache(certFile, keyFile).withMinVersion(tls.VersionTLS13)
	if err := cache.Load(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if !cache.Ready() {
		t.Fatal("cache not ready after successful Load")
	}
	gotA, err := cache.Get()
	if err != nil {
		t.Fatalf("get after load: %v", err)
	}
	if len(gotA.Certificate) == 0 || len(gotA.Certificate[0]) == 0 {
		t.Fatal("Get returned an empty certificate chain")
	}
	gotA2, err := cache.Get()
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if string(gotA.Certificate[0]) != string(gotA2.Certificate[0]) {
		t.Fatal("Get must serve a stable certificate between refreshes")
	}

	// 轮换：写入第二张证书 → Refresh 报告变化 → Get 供给新 DER。
	fpB := writeTestCertPair(t, certFile, keyFile, []string{"b.test"})
	if fpA == fpB {
		t.Fatal("test helper produced two identical certs — rotation is not exercised")
	}
	changed, err := cache.Refresh()
	if err != nil || !changed {
		t.Fatalf("refresh after rotation: changed=%v err=%v, want true/nil", changed, err)
	}
	gotB, err := cache.Get()
	if err != nil {
		t.Fatalf("get after rotation: %v", err)
	}
	if string(gotB.Certificate[0]) == string(gotA.Certificate[0]) {
		t.Fatal("Get still serves the pre-rotation certificate")
	}

	// 幂等：内容未变 → Refresh 报告无变化。
	changed, err = cache.Refresh()
	if err != nil || changed {
		t.Fatalf("idempotent refresh: changed=%v err=%v, want false/nil", changed, err)
	}
}

// TestTLSCertCacheCorruptKeepsServing 读/解析失败保留旧证书继续服务（刷新
// 失败的可访问性语义；错误原样上抛供刷新循环告警）。
func TestTLSCertCacheCorruptKeepsServing(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	writeTestCertPair(t, certFile, keyFile, []string{"a.test"})
	cache := newTLSCertCache(certFile, keyFile)
	if err := cache.Load(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	before, err := cache.Get()
	if err != nil {
		t.Fatalf("get before corruption: %v", err)
	}

	if err := os.WriteFile(certFile, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	if _, err := cache.Refresh(); err == nil {
		t.Fatal("refresh over a corrupt cert must fail")
	}
	after, err := cache.Get()
	if err != nil {
		t.Fatalf("get after failed refresh: %v", err)
	}
	if string(after.Certificate[0]) != string(before.Certificate[0]) {
		t.Fatal("failed refresh must keep serving the previously loaded certificate")
	}
}

// handshakeCertDER 做一次真 TLS 握手（本机回环 TCP 对——net.Pipe 的同步
// 无缓冲语义会让握手阻塞，回环 socket 才是真实握手时序），返回服务端出示
// 的叶证书 DER（GetCertificate 闭包供给面的端到端断言载体）。
func handshakeCertDER(t *testing.T, cfg *tls.Config, clientVerify bool) ([]byte, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srvErr := make(chan error, 1)
	go func() {
		conn, lerr := ln.Accept()
		if lerr != nil {
			srvErr <- lerr
			return
		}
		srv := tls.Server(conn, cfg)
		if err := srv.HandshakeContext(context.Background()); err != nil {
			srvErr <- err
			_ = conn.Close()
			return
		}
		srvErr <- nil
		// 服务端不主动关：客户端读完握手态后自行收口。
		buf := make([]byte, 1)
		for {
			if _, err := srv.Read(buf); err != nil {
				break
			}
		}
		_ = srv.Close()
	}()
	clientFD, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	clientCfg := &tls.Config{InsecureSkipVerify: !clientVerify} //nolint:gosec // G402：测试自签信任面
	client := tls.Client(clientFD, clientCfg)
	err = client.HandshakeContext(context.Background())
	_ = client.Close()
	srvHandshakeErr := <-srvErr
	if err != nil {
		return nil, err
	}
	if srvHandshakeErr != nil {
		return nil, srvHandshakeErr
	}
	peer := client.ConnectionState().PeerCertificates
	if len(peer) == 0 {
		return nil, errors.New("client saw no peer certificate")
	}
	return peer[0].Raw, nil
}

// TestControlPlaneTLSTLSConfigServesRotatedCert TLSConfig 闭包端到端：握手
// 供给证书 A → 轮换后新握手供给证书 B（热重载的服务面证据）；证书未就绪
// （platform 等待形态）握手失败。
func TestControlPlaneTLSTLSConfigServesRotatedCert(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	writeTestCertPair(t, certFile, keyFile, []string{"a.test"})
	cache := newTLSCertCache(certFile, keyFile).withMinVersion(tls.VersionTLS12)
	if err := cache.Load(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	ctl := &ControlPlaneTLS{mode: ControlPlaneTLSManual, log: slog.New(slog.NewTextHandler(io.Discard, nil)), cache: cache}
	cfg := ctl.TLSConfig()
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLSConfig MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}

	derA, err := handshakeCertDER(t, cfg, false)
	if err != nil {
		t.Fatalf("handshake with cert A: %v", err)
	}
	writeTestCertPair(t, certFile, keyFile, []string{"b.test"})
	if changed, rerr := cache.Refresh(); rerr != nil || !changed {
		t.Fatalf("rotation refresh: changed=%v err=%v", changed, rerr)
	}
	derB, err := handshakeCertDER(t, cfg, false)
	if err != nil {
		t.Fatalf("handshake with cert B: %v", err)
	}
	if string(derA) == string(derB) {
		t.Fatal("post-rotation handshake still presents the old certificate")
	}

	// 未就绪形态（platform 等待签发）：空缓存握手失败、错误诚实可读。
	empty := &ControlPlaneTLS{
		mode:  ControlPlaneTLSPlatform,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cache: newTLSCertCache(filepath.Join(dir, "absent.pem"), filepath.Join(dir, "absent.key")),
	}
	if _, err := empty.TLSConfig().GetCertificate(nil); err == nil {
		t.Fatal("GetCertificate on an empty cache must fail")
	}
	if _, err := handshakeCertDER(t, empty.TLSConfig(), false); err == nil {
		t.Fatal("handshake without a ready certificate must fail")
	}
}

// TestNewControlPlaneTLSAssembly 三态装配（Wire provider 形态）：off 返回
// nil（服务壳不加 TLS 选项——今日行为零变化的装配面保证）；manual 装配期
// fail-fast；platform 证书未落盘时缓存未就绪（握手失败形态）+ 路径取自
// 平台证书库，落盘后 Refresh 即可服务。
func TestNewControlPlaneTLSAssembly(t *testing.T) {
	app := captureLynxApp(t)
	dir := t.TempDir()
	certFile := filepath.Join(dir, "manual-cert.pem")
	keyFile := filepath.Join(dir, "manual-key.pem")

	// off：provider nil，但 cleanup 恒非 nil 且可安全调用——wire 聚合清理
	// 闭包无条件调用（nil 函数值 = SIGTERM 优雅停机 panic，smoke.sh 实爆；
	// manual 分支同病同修）。
	offCfg := &AppConfig{}
	off, cleanupOff, err := NewControlPlaneTLS(app, offCfg, nil)
	if err != nil || off != nil {
		t.Fatalf("off assembly: want nil-tls/nil-err (err=%v)", err)
	}
	if cleanupOff == nil {
		t.Fatal("off assembly cleanup must be non-nil (wire post-stop hooks call it unconditionally)")
	}
	cleanupOff()

	// manual：文件缺失 loud-fail；就绪后加载成功且 Get 供给。
	missingCfg := &AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{
		Mode: ControlPlaneTLSManual, CertFile: filepath.Join(dir, "absent.pem"), KeyFile: filepath.Join(dir, "absent.key"),
	}}}
	if _, _, err := NewControlPlaneTLS(app, missingCfg, nil); err == nil {
		t.Fatal("manual assembly with missing files must fail fast")
	}
	writeTestCertPair(t, certFile, keyFile, []string{"ctrl.example.test"})
	manualCfg := &AppConfig{ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{
		Mode: ControlPlaneTLSManual, CertFile: certFile, KeyFile: keyFile,
	}}}
	manual, cleanupManual, err := NewControlPlaneTLS(app, manualCfg, nil)
	if err != nil {
		t.Fatalf("manual assembly: %v", err)
	}
	if cleanupManual == nil {
		t.Fatal("manual assembly cleanup must be non-nil (wire post-stop hooks call it unconditionally)")
	}
	t.Cleanup(cleanupManual)
	if manual == nil || manual.mode != ControlPlaneTLSManual {
		t.Fatalf("manual assembly produced %+v", manual)
	}
	if _, err := manual.cache.Get(); err != nil {
		t.Fatalf("manual cache not loaded: %v", err)
	}

	// platform：依赖 ingress 管理器取平台证书路径（base_domain 非空门禁在
	// ValidateControlPlaneTLS 已单测）；证书未落盘 = 缓存未就绪（诚实等待
	// 形态），落盘后 Refresh 即服务。
	st, err := state.Open(context.Background(), filepath.Join(dir, "platform.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	platformCfg := &AppConfig{
		BaseDomain: "example.test",
		Ingress:    IngressConfig{CertDir: filepath.Join(dir, "certs"), TokenFile: filepath.Join(dir, "ingress.token")},
		ControlPlane: ControlPlaneConfig{TLS: ControlPlaneTLSConfig{
			Mode: ControlPlaneTLSPlatform,
		}},
	}
	ing, cleanupIng, err := NewIngressManager(app, platformCfg, st, nil)
	if err != nil {
		t.Fatalf("ingress manager: %v", err)
	}
	t.Cleanup(cleanupIng)
	platform, cleanupPlatform, err := NewControlPlaneTLS(app, platformCfg, ing)
	if err != nil {
		t.Fatalf("platform assembly: %v", err)
	}
	t.Cleanup(func() {
		if cleanupPlatform != nil {
			cleanupPlatform()
		}
	})
	if platform == nil || platform.mode != ControlPlaneTLSPlatform {
		t.Fatalf("platform assembly produced %+v", platform)
	}
	if _, err := platform.cache.Get(); err == nil {
		t.Fatal("platform cache must be unready before the certificate is issued")
	}
	// 平台证书落盘（控制器签发形态：ingress 证书库路径）→ 一拍 Refresh 装入。
	pCert, pKey, err := ing.PlatformCertPaths()
	if err != nil {
		t.Fatalf("platform cert paths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pCert), 0o750); err != nil {
		t.Fatalf("mkdir cert dir: %v", err)
	}
	writeTestCertPair(t, pCert, pKey, []string{"ctrl.example.test"})
	if changed, rerr := platform.cache.Refresh(); rerr != nil || !changed {
		t.Fatalf("platform refresh after issuance: changed=%v err=%v", changed, rerr)
	}
	if _, err := platform.cache.Get(); err != nil {
		t.Fatalf("platform cache not serving after issuance: %v", err)
	}
}

// TestControlPlaneTLSRefreshLoop 刷新循环的日志/缓存语义（短周期注入）：
// 未就绪时按拍告警 → 落盘后装入并报告 ready → 轮换后重载。
func TestControlPlaneTLSRefreshLoop(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	cache := newTLSCertCache(certFile, keyFile).withMinVersion(tls.VersionTLS12)
	logBuf := syncBuffer{}
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	ctl := &ControlPlaneTLS{
		mode:            ControlPlaneTLSPlatform,
		log:             log,
		cache:           cache,
		refreshInterval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go ctl.refreshLoop(ctx, done)

	awaitUntil := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", what)
	}

	// 未就绪：等待窗内握手失败，日志有 warn。
	awaitUntil("the not-ready warn", func() bool { return logBuf.contains("still not ready") })
	// 落盘（平台证书控制器的续期形态）：一拍内装入，日志 ready。
	writeTestCertPair(t, certFile, keyFile, []string{"a.test"})
	awaitUntil("the ready info", func() bool { return logBuf.contains("now complete handshakes") })
	awaitUntil("the cache becoming ready", cache.Ready)
	// 轮换：重载日志出现。
	writeTestCertPair(t, certFile, keyFile, []string{"b.test"})
	awaitUntil("the reload info", func() bool { return logBuf.contains("reloaded") })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh loop did not exit after ctx cancel")
	}
}

// syncBuffer 是并发安全的字节缓冲（slog handler 并发写、轮询线程读）。
type syncBuffer struct {
	mu   sync.RWMutex
	data []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *syncBuffer) contains(sub string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return strings.Contains(string(b.data), sub)
}
