package ingress

// E4 验收测试（S19）：VerifyDomains 用传入端口探测——非默认端口部署下
// 探测命中真实入口（localhost 解析到回环，两个临时监听器分别承载配置
// HTTP/TLS 端口；旧实现硬编码 80/443 时两路探测均不可达，本用例必挂）。

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestVerifyDomainsUsesConfiguredPorts(t *testing.T) {
	// HTTP 侧：配置端口上的 200 应答。
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen http: %v", err)
	}
	httpPort := httpLn.Addr().(*net.TCPAddr).Port
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpSrv.Serve(httpLn) }()
	t.Cleanup(func() { _ = httpLn.Close() })

	// TLS 侧：配置端口上的自签证书握手（verify 的 InsecureSkipVerify
	// 「如实记录」语义下任何证书都可通过）。
	certPEM, keyPEM, _ := selfSignedTestCert(t, "localhost")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	tlsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	tlsPort := tlsLn.Addr().(*net.TCPAddr).Port
	tlsSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = tlsSrv.ServeTLS(tlsLn, "", "") }()
	t.Cleanup(func() { _ = tlsLn.Close() })

	// localhost 解析到回环（hosts 契约），两路探测都必须命中配置端口。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	checks := VerifyDomains(ctx, []string{"localhost"}, httpPort, tlsPort)
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(checks))
	}
	c := checks[0]
	if !c.Resolved || c.Err != "" {
		t.Fatalf("resolve failed (localhost must resolve to loopback): %+v", c)
	}
	if c.HTTP80 != "200 OK" {
		t.Fatalf("configured http port %d probe = %q, want 200 OK (E4: port must flow into the probe)", httpPort, c.HTTP80)
	}
	if !strings.HasPrefix(c.HTTPS443, "handshake ok") {
		t.Fatalf("configured https port %d probe = %q, want handshake ok", tlsPort, c.HTTPS443)
	}
}
