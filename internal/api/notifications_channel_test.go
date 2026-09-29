package api

// NotificationsService 通道扩展测试（W4-S3，observability §8）：通道组合
// 形状 400 矩阵、email/slack 端点 CRUD 视图投影、SMTP 设置三 RPC（密码
// 只写不读——读面/存储往返只出指纹与密文；探针真 SMTP 会话对本地假
// sink——250 应答，零第三方）。

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// fakeSMTPSink 与 internal/notify 测试的 sink 同构（包间不共享 test 文件
// ——各包自足，dind e2e 用独立主程序 sink）。canned 应答 + 会话全文记录。
type apiFakeSMTPSink struct {
	ln         net.Listener
	mu         sync.Mutex
	transcript string
	addr       string
}

func newAPIFakeSMTPSink(t *testing.T) *apiFakeSMTPSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("smtp sink listen: %v", err)
	}
	s := &apiFakeSMTPSink{ln: ln, addr: ln.Addr().String()}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *apiFakeSMTPSink) write(conn net.Conn, line string) {
	_, _ = conn.Write([]byte(line + "\r\n"))
}

func (s *apiFakeSMTPSink) serve() {
	// 多连接接受循环（探针/试发各自建连——每次会话独立处理）。
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.handle(conn)
	}
}

func (s *apiFakeSMTPSink) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	s.write(conn, "220 sink.test ESMTP fake")
	sc := bufio.NewScanner(conn)
	inData := false
	for sc.Scan() {
		line := sc.Text()
		s.mu.Lock()
		s.transcript += line + "\n"
		s.mu.Unlock()
		upper := strings.ToUpper(line)
		switch {
		case inData:
			if line == "." {
				inData = false
				s.write(conn, "250 OK queued as sink-1")
			}
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			s.write(conn, "250-sink.test")
			s.write(conn, "250 8BITMIME")
		case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
			s.write(conn, "250 OK")
		case strings.HasPrefix(upper, "DATA"):
			inData = true
			s.write(conn, "354 end data with <CR><LF>.<CR><LF>")
		case strings.HasPrefix(upper, "QUIT"):
			s.write(conn, "221 bye")
			return
		default:
			s.write(conn, "250 OK")
		}
	}
}

func (s *apiFakeSMTPSink) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transcript
}

func TestChannelShapeValidationMatrix(t *testing.T) {
	_, _, cl, token := newNotificationsTestEnv(t)
	ctx := authCtx(context.Background(), token)

	cases := []struct {
		name   string
		typ    string
		url    string
		target string
		wantOK bool
	}{
		{"webhook-default", "", "http://127.0.0.1:1/hook", "", true},
		{"slack", "slack", "https://hooks.slack.test/T/B/x", "", true},
		{"email", "email", "", "ops@example.test", true},
		{"webhook-with-target", "webhook", "http://127.0.0.1:1/hook", "x@example.test", false},
		{"slack-with-target", "slack", "https://hooks.slack.test/x", "x@example.test", false},
		{"email-with-url", "email", "http://127.0.0.1:1/hook", "ops@example.test", false},
		{"email-without-target", "email", "", "", false},
		{"email-bad-target", "email", "", "not a mailbox", false},
		{"unknown-type", "irc", "http://127.0.0.1:1/hook", "", false},
	}
	for _, tc := range cases {
		resp, err := cl.CreateWebhookEndpoint(ctx, &serverv1.CreateWebhookEndpointRequest{
			Name: "ep-" + tc.name, Url: tc.url, Type: tc.typ, Target: tc.target,
			EventPatterns: []string{"deployment.*"},
		})
		if tc.wantOK && err != nil {
			t.Errorf("%s: create err = %v", tc.name, err)
			continue
		}
		if !tc.wantOK {
			if status.Convert(err).Code() != codes.InvalidArgument {
				t.Errorf("%s: err = %v, want InvalidArgument", tc.name, err)
			}
			continue
		}
		// 视图投影带通道语义（type 归一、target 随行）。
		wantType := tc.typ
		if wantType == "" {
			wantType = "webhook"
		}
		if got := resp.GetEndpoint().GetType(); got != wantType {
			t.Errorf("%s: view type = %q, want %q", tc.name, got, wantType)
		}
		if resp.GetEndpoint().GetTarget() != tc.target {
			t.Errorf("%s: view target = %q, want %q", tc.name, resp.GetEndpoint().GetTarget(), tc.target)
		}
	}
}

// TestChannelSwitchUpdate：webhook → email 换通道（url 显式清空 + target
// 设置同一次 update）；单独置空 url 被拒；email → slack 换回。
func TestChannelSwitchUpdate(t *testing.T) {
	_, _, cl, token := newNotificationsTestEnv(t)
	ctx := authCtx(context.Background(), token)
	created, err := cl.CreateWebhookEndpoint(ctx, &serverv1.CreateWebhookEndpointRequest{
		Name: "ops", Url: "http://127.0.0.1:1/hook", EventPatterns: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created.GetEndpoint().GetId()

	// 单独置空 url → 400（会失去投递地址）。
	empty := ""
	if _, err := cl.UpdateWebhookEndpoint(ctx, &serverv1.UpdateWebhookEndpointRequest{Id: id, Url: &empty}); status.Convert(err).Code() != codes.InvalidArgument {
		t.Fatalf("clearing url alone: err = %v, want InvalidArgument", err)
	}

	// 换通道（url="" + target 同次）→ 200；最终形态落库。
	target := "ops@example.test"
	email := "email"
	upd, err := cl.UpdateWebhookEndpoint(ctx, &serverv1.UpdateWebhookEndpointRequest{
		Id: id, Url: &empty, Target: &target, Type: &email,
	})
	if err != nil {
		t.Fatalf("switch to email: %v", err)
	}
	if upd.GetEndpoint().GetType() != "email" || upd.GetEndpoint().GetUrl() != "" || upd.GetEndpoint().GetTarget() != target {
		t.Fatalf("after switch: %+v", upd.GetEndpoint())
	}

	// 换回 slack（target 清空 + url 设置）。
	url := "https://hooks.slack.test/T/B/x"
	slack := "slack"
	emptyTarget := ""
	upd, err = cl.UpdateWebhookEndpoint(ctx, &serverv1.UpdateWebhookEndpointRequest{
		Id: id, Target: &emptyTarget, Url: &url, Type: &slack,
	})
	if err != nil {
		t.Fatalf("switch to slack: %v", err)
	}
	if upd.GetEndpoint().GetType() != "slack" || upd.GetEndpoint().GetTarget() != "" {
		t.Fatalf("after switch back: %+v", upd.GetEndpoint())
	}
}

// TestSmtpSettingsRPCRoundTrip：设置往返 + 密码只写不读（读面只出指纹）+
// 审计零泄漏（notify.smtp_changed 的 diff 不含密文/明文/用户名值）。
func TestSmtpSettingsRPCRoundTrip(t *testing.T) {
	st, box, cl, token := newNotificationsTestEnv(t)
	ctx := authCtx(context.Background(), token)

	// 缺省态读面：全空。
	got, err := cl.GetSmtpSettings(ctx, &serverv1.GetSmtpSettingsRequest{})
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if got.GetSettings().GetHost() != "" {
		t.Fatalf("empty settings: %+v", got.GetSettings())
	}

	// 保存（密码明文入站 → 库内必须是密文）。
	//nolint:gosec // G101：测试夹具口令（密文落库断言的入站材料），非真实凭据
	saved, err := cl.UpdateSmtpSettings(ctx, &serverv1.UpdateSmtpSettingsRequest{
		Host: "smtp.example.test", Port: 587,
		Username: "relay-user", Password: "PLAINTEXT-PW", From: "fleetly@example.test",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	view := saved.GetSettings()
	if view.GetPasswordFingerprint() == "" || len(view.GetPasswordFingerprint()) != 16 {
		t.Fatalf("read face must show the password fingerprint: %+v", view)
	}
	if strings.Contains(view.String(), "PLAINTEXT-PW") {
		t.Fatal("read face must never carry the plaintext password")
	}
	in, err := st.LoadSmtpSettings(ctx)
	if err != nil {
		t.Fatalf("load stored: %v", err)
	}
	if in.PasswordCipher == "" || strings.Contains(in.PasswordCipher, "PLAINTEXT-PW") {
		t.Fatalf("storage must hold envelope ciphertext, got %q", in.PasswordCipher)
	}
	plain, err := box.Decrypt([]byte(in.PasswordCipher))
	if err != nil || string(plain) != "PLAINTEXT-PW" {
		t.Fatalf("cipher must decrypt back to the plaintext: %v", err)
	}

	// 审计：notify.smtp_changed 落行；diff 零凭据材料。
	audits, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Action != "notify.smtp_changed" {
			continue
		}
		found = true
		for _, banned := range []string{"PLAINTEXT-PW", "relay-user"} {
			if strings.Contains(a.DiffSummary, banned) {
				t.Fatalf("audit diff leaks %q: %s", banned, a.DiffSummary)
			}
		}
	}
	if !found {
		t.Fatal("notify.smtp_changed audit row missing")
	}

	// PUT 语义：第二次保存不带密码 → 密码清除（指纹消失）。
	saved, err = cl.UpdateSmtpSettings(ctx, &serverv1.UpdateSmtpSettingsRequest{
		Host: "smtp2.example.test", Port: 25, From: "fleetly@example.test",
	})
	if err != nil {
		t.Fatalf("update 2: %v", err)
	}
	if saved.GetSettings().GetPasswordFingerprint() != "" {
		t.Fatalf("PUT semantics must clear the absent password: %+v", saved.GetSettings())
	}
}

// TestSmtpProbeAndTestEndpointAgainstSink：TestSmtp（候选配置）与
// TestEndpoint（email 端点试发，收件 = target）对本地假 sink 的真 SMTP
// 会话——ok + 250 + 会话内容断言。
func TestSmtpProbeAndTestEndpointAgainstSink(t *testing.T) {
	_, _, cl, token := newNotificationsTestEnv(t)
	ctx := authCtx(context.Background(), token)
	sink := newAPIFakeSMTPSink(t)
	_, portStr, err := net.SplitHostPort(sink.addr)
	if err != nil {
		t.Fatalf("sink addr %q: %v", sink.addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("sink port %q: %v", portStr, err)
	}

	// TestSmtp 对候选配置（未保存也能测——S3 探针同语义；候选字段
	// optional——指针置位才参与「是否给了候选」判定）。
	host := "127.0.0.1"
	port32 := int32(port) //nolint:gosec // G109：端口量级极小（1..65535）
	from := "fleetly@example.test"
	resp, err := cl.TestSmtp(ctx, &serverv1.TestSmtpRequest{
		To:   "probe@example.test",
		Host: &host, Port: &port32, From: &from,
	})
	if err != nil {
		t.Fatalf("test smtp: %v", err)
	}
	if !resp.GetOk() || resp.GetStatusCode() != 250 {
		t.Fatalf("probe: %+v (%s)", resp, resp.GetError())
	}
	if tr := sink.snapshot(); !strings.Contains(tr, "RCPT TO:<probe@example.test>") {
		t.Fatalf("probe transcript missing the rcpt:\n%s", tr)
	}

	// email 端点 + 已存设置 → TestEndpoint 试发（收件 = target 地址）。
	if _, err := cl.UpdateSmtpSettings(ctx, &serverv1.UpdateSmtpSettingsRequest{
		Host: "127.0.0.1", Port: port32, From: "fleetly@example.test",
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	created, err := cl.CreateWebhookEndpoint(ctx, &serverv1.CreateWebhookEndpointRequest{
		Name: "mail", Type: "email", Target: "ops@example.test",
		EventPatterns: []string{"cron.*"},
	})
	if err != nil {
		t.Fatalf("create email endpoint: %v", err)
	}
	test, err := cl.TestWebhook(ctx, &serverv1.TestWebhookRequest{Id: created.GetEndpoint().GetId()})
	if err != nil {
		t.Fatalf("test endpoint: %v", err)
	}
	if !test.GetOk() {
		t.Fatalf("email test endpoint: %+v (%s)", test, test.GetError())
	}
	if tr := sink.snapshot(); !strings.Contains(tr, "RCPT TO:<ops@example.test>") {
		t.Fatalf("endpoint test transcript missing the target:\n%s", tr)
	}

	// slack 端点试发：{"text"} 形态、无签名头。
	var slackBody string
	haveBody := make(chan struct{})
	httpSink := newSlackCaptureSink(t, &slackBody, haveBody)
	created, err = cl.CreateWebhookEndpoint(ctx, &serverv1.CreateWebhookEndpointRequest{
		Name: "relay", Type: "slack", Url: httpSink.URL, EventPatterns: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create slack endpoint: %v", err)
	}
	test, err = cl.TestWebhook(ctx, &serverv1.TestWebhookRequest{Id: created.GetEndpoint().GetId()})
	if err != nil {
		t.Fatalf("test slack endpoint: %v", err)
	}
	if !test.GetOk() {
		t.Fatalf("slack test endpoint: %+v (%s)", test, test.GetError())
	}
	<-haveBody
	if !strings.Contains(slackBody, `"text":"[fleetly] test`) {
		t.Fatalf("slack test payload form: %s", slackBody)
	}
}

// TestUnconfiguredSmtpTestEndpointFails：email 端点在设置未配置时试发 →
// RPC 200 + ok=false（诚实失败面，非 5xx）。
func TestUnconfiguredSmtpTestEndpointFails(t *testing.T) {
	_, _, cl, token := newNotificationsTestEnv(t)
	ctx := authCtx(context.Background(), token)
	created, err := cl.CreateWebhookEndpoint(ctx, &serverv1.CreateWebhookEndpointRequest{
		Name: "mail", Type: "email", Target: "ops@example.test",
		EventPatterns: []string{"cron.*"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := cl.TestWebhook(ctx, &serverv1.TestWebhookRequest{Id: created.GetEndpoint().GetId()})
	if err != nil {
		t.Fatalf("test endpoint rpc: %v", err)
	}
	// 投递结论是业务事实（RPC 200 + ok=false），不是传输层错误。
	if resp.GetOk() || !strings.Contains(resp.GetError(), "smtp") {
		t.Fatalf("unconfigured smtp must fail honestly: %+v", resp)
	}
}

// TestSmtpSettingsInvalidShapeRejected：UpdateSmtpSettings 的形状违约走
// protovalidate/服务端校验的 InvalidArgument 面。
func TestSmtpSettingsInvalidShapeRejected(t *testing.T) {
	_, _, cl, token := newNotificationsTestEnv(t)
	ctx := authCtx(context.Background(), token)
	if _, err := cl.UpdateSmtpSettings(ctx, &serverv1.UpdateSmtpSettingsRequest{
		Host: "smtp.example.test", Port: 587, From: "not a mailbox",
	}); status.Convert(err).Code() != codes.InvalidArgument {
		t.Fatalf("bad from: err = %v, want InvalidArgument", err)
	}
	if _, err := cl.TestSmtp(ctx, &serverv1.TestSmtpRequest{To: "nope"}); status.Convert(err).Code() != codes.InvalidArgument {
		t.Fatalf("bad to: err = %v, want InvalidArgument", err)
	}
}

// newSlackCaptureSink 起一个捕获 POST body 的本地 HTTP sink（slack 试发
// 断言面：{"text"} 单键形态、无签名头）。
func newSlackCaptureSink(t *testing.T, body *string, done chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		_, _ = io.ReadFull(r.Body, raw)
		*body = string(raw)
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	t.Cleanup(srv.Close)
	return srv
}
