package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/notify"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NotificationsService 实现 server.v1.NotificationsService（E6 观测专项
// 设计 §5 + §8 通道扩展，W5-S4 / W4-S3）：订阅端点 CRUD + 投递台账读面 +
// TestWebhook（TestEndpoint 语义——按通道类型试发）+ 平台级 SMTP 设置面。
//
// secret 纪律（§5.1 + state-model §2.9）：签名密钥平台生成（32B base64），
// envelope 加密落库（加密边界在本服务——box.Encrypt/Decrypt，与 s3 设置
// 同型）；明文只在 Create/Rotate 响应一次性出现；读面只出指纹（sha256
// 前 8 hex，s3 卡同口径）。SMTP 密码同理：明文只写不读，读面只出指纹。
// URL/name 形状校验在本面做 400 形状门（退化信封——形状违约不走注册表
// 码），patterns 白名单与通道组合形状校验在 state 层（422
// E_WEBHOOK_PATTERN_INVALID 原样透传；通道形状在 api 面先拦为 400）。投
// 递本身由 internal/notify 投递器承载——本服务是纯受理/投影面。
//
// scope：读 = read（台账与端点是事实面，指纹非凭据）；写 = admin（端点
// 是平台级凭据面——创建/轮换/删除/测试/SMTP 设置与 s3 设置同级）。
type NotificationsService struct {
	serverv1.UnimplementedNotificationsServiceServer
	st  *state.Store
	box *secrets.Box
}

// NewNotificationsService 构造 NotificationsService（box 供 secret 加解密
// ——nil 不可：无密钥面即无端点面）。
func NewNotificationsService(st *state.Store, box *secrets.Box) *NotificationsService {
	return &NotificationsService{st: st, box: box}
}

// ListWebhookEndpoints 端点清单（无敏感投影）。
func (s *NotificationsService) ListWebhookEndpoints(ctx context.Context, _ *serverv1.ListWebhookEndpointsRequest) (*serverv1.ListWebhookEndpointsResponse, error) {
	rows, err := s.st.ListWebhookEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.WebhookEndpointView, 0, len(rows))
	for _, e := range rows {
		out = append(out, webhookEndpointView(e))
	}
	return &serverv1.ListWebhookEndpointsResponse{Endpoints: out}, nil
}

// GetWebhookEndpoint 单端点视图（不存在 → E_WEBHOOK_NOT_FOUND 404）。
func (s *NotificationsService) GetWebhookEndpoint(ctx context.Context, req *serverv1.GetWebhookEndpointRequest) (*serverv1.GetWebhookEndpointResponse, error) {
	e, err := s.st.GetWebhookEndpoint(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.GetWebhookEndpointResponse{Endpoint: webhookEndpointView(e)}, nil
}

// CreateWebhookEndpoint 创建端点：密钥平台生成 + envelope 加密 + 指纹，
// **明文仅本次响应可见**。通道组合形状（W4-S3 设计 §8.1）在本面先拦为
// 400：webhook/slack → url 必填合法、target 必须为空；email → target 必
// 须合法邮箱、url 必须为空。
func (s *NotificationsService) CreateWebhookEndpoint(ctx context.Context, req *serverv1.CreateWebhookEndpointRequest) (*serverv1.CreateWebhookEndpointResponse, error) {
	// 平台面写门（v0.3 W3-S2 扩全，rbac-teams §3.2「通知 → 仅平台管理员」
	// ——端点是平台级凭据面，与 S3 设置同门）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if err := shapeWebhookName(req.GetName()); err != nil {
		return nil, err
	}
	if err := shapeChannel(req.GetType(), req.GetUrl(), req.GetTarget()); err != nil {
		return nil, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = req.GetEnabled()
	}
	plaintext, err := generateWebhookSecret()
	if err != nil {
		return nil, err
	}
	cipher, err := s.box.Encrypt([]byte(plaintext))
	if err != nil {
		return nil, errors.New("webhook secret encryption failed (platform key error): " + err.Error())
	}
	e, err := s.st.CreateWebhookEndpoint(ctx, state.WebhookEndpointWrite{
		Name:              req.GetName(),
		URL:               req.GetUrl(),
		Type:              req.GetType(),
		Target:            req.GetTarget(),
		SecretCipher:      string(cipher),
		SecretFingerprint: secretFingerprint([]byte(plaintext)),
		EventPatterns:     req.GetEventPatterns(),
		Enabled:           enabled,
		ActorTokenID:      callerTokenID(ctx),
	})
	if err != nil {
		return nil, mapStoreErr(err, req.GetName())
	}
	return &serverv1.CreateWebhookEndpointResponse{
		Endpoint: webhookEndpointView(e),
		Secret:   plaintext, // 仅此一次
	}, nil
}

// UpdateWebhookEndpoint 部分更新（optional 字段语义——未提供不变）。通道
// 字段（type/target/url）的组合形状对「更新后的最终形态」校验（本面 400
// 先拦，state 层第二道闸）。
func (s *NotificationsService) UpdateWebhookEndpoint(ctx context.Context, req *serverv1.UpdateWebhookEndpointRequest) (*serverv1.UpdateWebhookEndpointResponse, error) {
	// 平台面写门（v0.3 W3-S2 扩全——同 Create）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	u := state.WebhookEndpointUpdate{ActorTokenID: callerTokenID(ctx)}
	if req.Name != nil {
		if err := shapeWebhookName(req.GetName()); err != nil {
			return nil, err
		}
		name := req.GetName()
		u.Name = &name
	}
	// 通道字段形状：需要现值合成最终形态（换通道时 url/target 可以合法地
	// 显式清空——单独置空被拒）。
	if req.Type != nil || req.Url != nil || req.Target != nil {
		prev, err := s.st.GetWebhookEndpoint(ctx, req.GetId())
		if err != nil {
			return nil, mapStoreErr(err, req.GetId())
		}
		finalType, finalURL, finalTarget := prev.Type, prev.URL, prev.Target
		if req.Type != nil {
			finalType = req.GetType()
		}
		if req.Url != nil {
			finalURL = req.GetUrl()
		}
		if req.Target != nil {
			finalTarget = req.GetTarget()
		}
		if err := shapeChannel(finalType, finalURL, finalTarget); err != nil {
			return nil, err
		}
	}
	if req.Url != nil {
		url := req.GetUrl()
		u.URL = &url
	}
	if req.Type != nil {
		typ := req.GetType()
		u.Type = &typ
	}
	if req.Target != nil {
		target := req.GetTarget()
		u.Target = &target
	}
	if len(req.GetEventPatterns()) > 0 {
		u.EventPatterns = req.GetEventPatterns()
	}
	if req.Enabled != nil {
		enabled := req.GetEnabled()
		u.Enabled = &enabled
	}
	e, err := s.st.UpdateWebhookEndpoint(ctx, req.GetId(), u)
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.UpdateWebhookEndpointResponse{Endpoint: webhookEndpointView(e)}, nil
}

// DeleteWebhookEndpoint 删除端点（台账行同事务清理）。
func (s *NotificationsService) DeleteWebhookEndpoint(ctx context.Context, req *serverv1.DeleteWebhookEndpointRequest) (*serverv1.DeleteWebhookEndpointResponse, error) {
	// 平台面写门（v0.3 W3-S2 扩全——同 Create）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if err := s.st.DeleteWebhookEndpoint(ctx, req.GetId(), "human", callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.DeleteWebhookEndpointResponse{Id: req.GetId()}, nil
}

// RotateWebhookSecret 轮换签名密钥：新密钥**明文仅本次响应可见**。
func (s *NotificationsService) RotateWebhookSecret(ctx context.Context, req *serverv1.RotateWebhookSecretRequest) (*serverv1.RotateWebhookSecretResponse, error) {
	// 平台面写门（v0.3 W3-S2 扩全——同 Create；轮换即凭据材料重置）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	plaintext, err := generateWebhookSecret()
	if err != nil {
		return nil, err
	}
	cipherBytes, err := s.box.Encrypt([]byte(plaintext))
	if err != nil {
		return nil, errors.New("webhook secret encryption failed (platform key error): " + err.Error())
	}
	fp := secretFingerprint([]byte(plaintext))
	cipher := string(cipherBytes)
	if _, err := s.st.UpdateWebhookEndpoint(ctx, req.GetId(), state.WebhookEndpointUpdate{
		SecretCipher:      &cipher,
		SecretFingerprint: &fp,
		ActorTokenID:      callerTokenID(ctx),
	}); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.RotateWebhookSecretResponse{Secret: plaintext, SecretFingerprint: fp}, nil
}

// TestWebhook 发送 type=test 载荷（设计 §5.2/§8.2）：按端点类型真实试发
// ——webhook = 签名 POST / slack = {"text"} POST / email = SMTP 投递（收
// 件 = 端点 target 地址，凭据取平台级 SMTP 设置）。同步返回单次投递结论；
// 不落台账。（RPC 名沿契约版本化门禁保留——语义即 TestEndpoint。）
func (s *NotificationsService) TestWebhook(ctx context.Context, req *serverv1.TestWebhookRequest) (*serverv1.TestWebhookResponse, error) {
	// 平台面写门（v0.3 W3-S2 扩全——同 Create；测试消耗平台凭据发真实
	// 出站请求，与 S3 连接探针同门）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	e, err := s.st.GetWebhookEndpoint(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	ep := notify.Endpoint{Type: e.Type, URL: e.URL, Target: e.Target}
	var smtpCfg *notify.SmtpConfig
	if e.Type == state.WebhookChannelWebhook || e.Type == "" {
		secret, err := s.box.Decrypt([]byte(e.SecretCipher))
		if err != nil {
			return nil, apperr.New("E_WEBHOOK_NOT_FOUND",
				"webhook endpoint %s secret cannot be decrypted (platform key mismatch): rotate the secret to restore testability", req.GetId())
		}
		ep.Secret = secret
	}
	if e.Type == state.WebhookChannelEmail {
		// 宽松读取：设置未配置不构成传输错误——deliver 以 ok=false 诚实
		// 呈现「settings are not configured」（投递结论面，非 4xx）。
		in, err := s.st.LoadSmtpSettings(ctx)
		if err != nil {
			return nil, err
		}
		if in.Host != "" && in.Port != 0 && in.From != "" {
			smtpCfg = &notify.SmtpConfig{Host: in.Host, Port: in.Port, Username: in.Username, From: in.From}
			if in.PasswordCipher != "" {
				plain, derr := s.box.Decrypt([]byte(in.PasswordCipher))
				if derr != nil {
					return nil, errors.New("smtp password decrypt failed (platform key error)")
				}
				smtpCfg.Password = string(plain)
			}
		}
	}
	ok, code, errText := notify.SendTestEndpoint(ctx, ep, smtpCfg, e.ID)
	return &serverv1.TestWebhookResponse{Ok: ok, StatusCode: int32(code), Error: errText}, nil //nolint:gosec // G115：HTTP/SMTP 应答码量级极小
}

// GetSmtpSettings 平台级 SMTP 设置只读面：密码只回指纹（明文 sha256 前 8）。
func (s *NotificationsService) GetSmtpSettings(ctx context.Context, _ *serverv1.GetSmtpSettingsRequest) (*serverv1.GetSmtpSettingsResponse, error) {
	// 平台面写门（S3 设置同门——SMTP 凭据是平台凭据面，读面含指纹）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.box == nil {
		return nil, statusUnavailableBox()
	}
	in, err := s.st.LoadSmtpSettings(ctx)
	if err != nil {
		return nil, err
	}
	return &serverv1.GetSmtpSettingsResponse{Settings: s.smtpSettingsView(in)}, nil
}

// UpdateSmtpSettings 全量保存（PUT 语义：请求即新状态）。密码明文入站
//（TLS 传输面）→ envelope 加密落库；校验在 state 层 fail-fast（形状由
// protovalidate 先拦）；保存 + 审计 notify.smtp_changed 同事务（只落审计
// 不落事件——§8.3 红线延伸；diff 只带非凭据事实）。
func (s *NotificationsService) UpdateSmtpSettings(ctx context.Context, req *serverv1.UpdateSmtpSettingsRequest) (*serverv1.UpdateSmtpSettingsResponse, error) {
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.box == nil {
		return nil, statusUnavailableBox()
	}
	// 形状门（teams.go 同款复核：bufconn/直连装配面无 protovalidate，服务
	// 端先拦为 400——proto 注解在网关面生效，此处保证两面同语义）。
	if strings.TrimSpace(req.GetHost()) == "" {
		return nil, statusInvalidArgument("smtp host is required")
	}
	if req.GetPort() < 1 || req.GetPort() > 65535 {
		return nil, statusInvalidArgument(fmt.Sprintf("smtp port %d out of range (1..65535)", req.GetPort()))
	}
	if err := shapeEmailTarget(req.GetFrom()); err != nil {
		return nil, err
	}
	in := state.SmtpSettings{
		Host:     strings.TrimSpace(req.GetHost()),
		Port:     int(req.GetPort()), //nolint:gosec // G115：protovalidate 已限 1..65535
		Username: req.GetUsername(),
		From:     req.GetFrom(),
	}
	if req.GetPassword() != "" {
		ct, err := s.box.Encrypt([]byte(req.GetPassword()))
		if err != nil {
			return nil, errors.New("smtp password encryption failed (platform key error): " + err.Error())
		}
		in.PasswordCipher = string(ct)
	}
	opts := state.SmtpSaveOptions{Actor: "human", ActorTokenID: callerTokenID(ctx)}
	if err := s.st.SaveSmtpSettings(ctx, in, opts); err != nil {
		return nil, err
	}
	return &serverv1.UpdateSmtpSettingsResponse{Settings: s.smtpSettingsView(in)}, nil
}

// TestSmtp SMTP 探针（设计 §8.3）：对候选（未保存也能测）或已存配置发测
// 试邮件到指定收件地址——真实 SMTP 往返。候选密码只在本次探针内使用，
// 绝不落库。
func (s *NotificationsService) TestSmtp(ctx context.Context, req *serverv1.TestSmtpRequest) (*serverv1.TestSmtpResponse, error) {
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.box == nil {
		return nil, statusUnavailableBox()
	}
	// 形状门：收件地址（同 UpdateSmtpSettings 的两面同语义复核）。
	if err := shapeEmailTarget(req.GetTo()); err != nil {
		return nil, err
	}
	var cfg *notify.SmtpConfig
	if req.GetHost() != "" || req.GetPort() != 0 || req.GetUsername() != "" ||
		req.GetPassword() != "" || req.GetFrom() != "" {
		cfg = &notify.SmtpConfig{
			Host:     strings.TrimSpace(req.GetHost()),
			Port:     int(req.GetPort()),
			Username: req.GetUsername(),
			Password: req.GetPassword(),
			From:     req.GetFrom(),
		}
	} else {
		var err error
		if cfg, err = s.storedSmtpConfig(ctx); err != nil {
			return nil, err
		}
	}
	ok, code, errText := notify.SendTestEmail(ctx, *cfg, req.GetTo())
	return &serverv1.TestSmtpResponse{Ok: ok, StatusCode: int32(code), Error: errText}, nil //nolint:gosec // G115：SMTP 应答码量级极小
}

// storedSmtpConfig 读取已存 SMTP 设置并解密密码（email 投递与 TestSmtp/
// TestEndpoint 共用；未配置 → 400 形状信封——探针无可测之物）。
func (s *NotificationsService) storedSmtpConfig(ctx context.Context) (*notify.SmtpConfig, error) {
	in, err := s.st.LoadSmtpSettings(ctx)
	if err != nil {
		return nil, err
	}
	if in.Host == "" || in.Port == 0 || in.From == "" {
		return nil, statusInvalidArgument(
			"no SMTP settings saved yet: save them with 'notifications smtp set' (or pass a candidate configuration)")
	}
	cfg := &notify.SmtpConfig{Host: in.Host, Port: in.Port, Username: in.Username, From: in.From}
	if in.PasswordCipher != "" {
		plain, derr := s.box.Decrypt([]byte(in.PasswordCipher))
		if derr != nil {
			return nil, errors.New("smtp password decrypt failed (platform key error)")
		}
		cfg.Password = string(plain)
	}
	return cfg, nil
}

// smtpSettingsView 构造 SMTP 设置脱敏投影（密码解密出指纹，明文不出服务
// 端边界——s3SettingsView 同型）。
func (s *NotificationsService) smtpSettingsView(in state.SmtpSettings) *serverv1.SmtpSettingsView {
	v := &serverv1.SmtpSettingsView{
		Host:      in.Host,
		Port:      int32(in.Port), //nolint:gosec // G115：端口量级极小
		Username:  in.Username,
		From:      in.From,
		UpdatedAt: tstamp(in.UpdatedAt),
	}
	if in.PasswordCipher != "" {
		plain, err := s.box.Decrypt([]byte(in.PasswordCipher))
		if err == nil {
			v.PasswordFingerprint = secretFingerprint(plain)
		}
		// 解密失败：指纹留空（密钥面故障的诚实呈现——不谎报「已设置」）。
	}
	return v
}

// ListWebhookDeliveries 投递台账（按端点/状态过滤，最新在前）。
func (s *NotificationsService) ListWebhookDeliveries(ctx context.Context, req *serverv1.ListWebhookDeliveriesRequest) (*serverv1.ListWebhookDeliveriesResponse, error) {
	rows, err := s.st.ListWebhookDeliveries(ctx, req.GetEndpointId(), req.GetStatus(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.WebhookDeliveryView, 0, len(rows))
	for _, d := range rows {
		v := &serverv1.WebhookDeliveryView{
			Id:         d.ID,
			EventSeq:   d.EventSeq,
			EndpointId: d.EndpointID,
			Status:     d.Status,
			Attempts:   int32(d.Attempts), //nolint:gosec // G115：尝试次数量级极小
			LastError:  d.LastError,
		}
		if d.ResponseCode != nil {
			code := int32(*d.ResponseCode) //nolint:gosec // G115：HTTP 状态码量级极小
			v.ResponseCode = &code
		}
		if d.NextRetryAt != nil {
			v.NextRetryAt = tstamp(*d.NextRetryAt)
		}
		v.CreatedAt = tstamp(d.CreatedAt)
		v.UpdatedAt = tstamp(d.UpdatedAt)
		out = append(out, v)
	}
	return &serverv1.ListWebhookDeliveriesResponse{Deliveries: out}, nil
}

// webhookEndpointView 构造端点无敏感投影。
func webhookEndpointView(e state.WebhookEndpoint) *serverv1.WebhookEndpointView {
	typ := e.Type
	if typ == "" {
		typ = state.WebhookChannelWebhook
	}
	return &serverv1.WebhookEndpointView{
		Id:                e.ID,
		Name:              e.Name,
		Url:               e.URL,
		Type:              typ,
		Target:            e.Target,
		EventPatterns:     e.EventPatterns,
		Enabled:           e.Enabled,
		SecretFingerprint: e.SecretFingerprint,
		CreatedAt:         tstamp(e.CreatedAt),
		UpdatedAt:         tstamp(e.UpdatedAt),
	}
}

// state 哨兵（ErrWebhookNotFound/ErrWebhookNameConflict）→ 注册表码信封的
// 映射（E_WEBHOOK_NOT_FOUND 404 / E_WEBHOOK_NAME_CONFLICT 409）已收进
// errors.go 的哨兵登记表——本文件经 mapStoreErr 消费；E_WEBHOOK_PATTERN_INVALID
// apperr 原样透传（不在表内）。

// shapeWebhookName / shapeChannel 是 400 形状门（state 层白名单是防御性
// 第二道闸——本面先拦，形状违约走退化信封不走注册表码）。
func shapeWebhookName(name string) error {
	if err := state.ValidateWebhookName(name); err != nil {
		return statusInvalidArgument(err.Error())
	}
	return nil
}

// shapeChannel 校验通道组合形状（设计 §8.1：webhook/slack → url 有 target
// 无；email → target 有 url 无）。
func shapeChannel(typ, rawURL, target string) error {
	normalized, err := state.ValidateWebhookChannel(typ)
	if err != nil {
		return statusInvalidArgument(err.Error())
	}
	switch normalized {
	case state.WebhookChannelWebhook, state.WebhookChannelSlack:
		if strings.TrimSpace(target) != "" {
			return statusInvalidArgument(fmt.Sprintf(
				"%s endpoints must not carry a target (the receiver URL is the url field)", normalized))
		}
		return shapeWebhookURL(rawURL)
	case state.WebhookChannelEmail:
		if strings.TrimSpace(rawURL) != "" {
			return statusInvalidArgument(
				"email endpoints must not carry a url (delivery goes through the platform SMTP settings)")
		}
		return shapeEmailTarget(target)
	default:
		return statusInvalidArgument(fmt.Sprintf("unknown channel type %q", typ))
	}
}

// shapeWebhookURL 是 URL 形状门。
func shapeWebhookURL(raw string) error {
	if err := state.ValidateWebhookURL(raw); err != nil {
		return statusInvalidArgument(err.Error())
	}
	return nil
}

// shapeEmailTarget 是 email 收件地址形状门（net/mail 精校验 + 裸地址形态
// ——拒绝显示名/空格；state 层白名单是第二道闸）。
func shapeEmailTarget(target string) error {
	addr, err := mail.ParseAddress(strings.TrimSpace(target))
	if err != nil || addr.Address != strings.TrimSpace(target) {
		return statusInvalidArgument(fmt.Sprintf(
			"email target %q is not a bare mailbox address (local@domain, no display name, no spaces)", target))
	}
	return nil
}

// statusUnavailableBox 收敛 box 未装配的不可用信封（SMTP 三个 RPC 共用）。
func statusUnavailableBox() error {
	return status.Error(codes.Unavailable, "secrets box unavailable (not assembled)")
}

// generateWebhookSecret 生成签名密钥明文（crypto/rand 32 字节 → base64
// ——43 字符无填充；HMAC 密钥熵源，base64 只是传输形态）。
func generateWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("generate webhook secret: " + err.Error())
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

var _ serverv1.NotificationsServiceServer = (*NotificationsService)(nil)
