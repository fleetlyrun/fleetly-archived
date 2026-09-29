package runtime

// gateway 回拨 TLS 回归（W5-S5 缺陷修复，2026-09-22 staging 门上实证）：
// control_plane.tls 开启时 gRPC 面对 TLS，gateway 回拨必须用 TLS 凭据——
// 原明文 insecure 回拨使全量 REST /v1 断（「error reading server preface」）。
// 本测试以自签 TLS gRPC 服务 + TLS 回拨 mux 走通一条 Ping 往返（对照组：
// 明文回拨对 TLS gRPC 面必败）。

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// pingOnlySystem 是 Ping 成功应答的最小 SystemService 测试实现（错误面
// 用 errors_test.go 的 failingSystemService——本测试走通路径）。
type pingOnlySystem struct {
	serverv1.UnimplementedSystemServiceServer
}

func (s *pingOnlySystem) Ping(ctx context.Context, req *serverv1.PingRequest) (*serverv1.PingResponse, error) {
	return &serverv1.PingResponse{}, nil
}

// selfSignedCert 生成测试用自签证书（SAN 127.0.0.1——回环自拨形态）。
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gateway-tls-test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestGatewayDialbackTLS TLS gRPC 面 + TLS 回拨 mux → Ping 往返通。
func TestGatewayDialbackTLS(t *testing.T) {
	cert := selfSignedCert(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
	serverv1.RegisterSystemServiceServer(gs, &pingOnlySystem{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dialTLS := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true} //nolint:gosec // G402：自签测试证书，验证的是回拨链路非对端身份
	mux, err := newGatewayMuxWithTLS(lis.Addr().String(), dialTLS)
	if err != nil {
		t.Fatalf("newGatewayMuxWithTLS: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/v1/system/ping")
	if err != nil {
		t.Fatalf("ping via TLS dialback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ping status = %d, want 200", resp.StatusCode)
	}
}

// TestGatewayDialbackPlaintextAgainstTLSFails 对照组：明文回拨（newGatewayMux
// 缺省 insecure）对 TLS gRPC 面必败——缺陷形态钉死，防回归到明文回拨。
func TestGatewayDialbackPlaintextAgainstTLSFails(t *testing.T) {
	cert := selfSignedCert(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
	serverv1.RegisterSystemServiceServer(gs, &pingOnlySystem{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	mux, err := newGatewayMux(lis.Addr().String())
	if err != nil {
		t.Fatalf("newGatewayMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(srv.URL + "/v1/system/ping")
	// 缺陷形态：gRPC 拨号失败 → gateway 500/503 信封（不是 200）。
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("plaintext dialback unexpectedly succeeded against a TLS gRPC face (regression: insecure dial must not work here)")
		}
	}
}
