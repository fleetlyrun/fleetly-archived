package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// 载荷与签名（E6 观测专项设计 §5.2）：POST JSON（事件全字段 + type 标位）
// + `X-Fleetly-Timestamp`（Unix 秒）+ `X-Fleetly-Signature: sha256=hex(
// HMAC-SHA256(secret, timestamp + "." + body))`。接收方可验签 + 5min 窗
// 防重放；secret 平台生成、可轮换。验签算法与头名是**对外契约**——改动
// 必须走设计修订（e2e 用宿主侧独立实现重算 HMAC 钉住该契约）。

// HeaderSignature / HeaderTimestamp / HeaderContentType 是投递请求的固定头。
const (
	HeaderSignature   = "X-Fleetly-Signature"
	HeaderTimestamp   = "X-Fleetly-Timestamp"
	HeaderContentType = "application/json"
)

// PayloadTypeEvent / PayloadTypeTest 是载荷 type 标位（TestEndpoint 发送
// type=test 载荷——结构同真实事件，设计 §5.2）。
const (
	PayloadTypeEvent = "event"
	PayloadTypeTest  = "test"
)

// Payload 是投递 body 的顶层结构：事件全字段（seq/at/name/subject/payload
// ——与 events 表行一一对应）+ type 标位。payload 原样透传（事件写入时已
// 脱敏——secret 值禁止进事件是 state-model §2.9 既有纪律，投递器不加噪
// 也不再过滤）。at 为 Unix 秒。
type Payload struct {
	Type    string          `json:"type"`
	Seq     int64           `json:"seq"`
	At      int64           `json:"at"`
	Name    string          `json:"name"`
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload"`
}

// NewEventPayload 把事件行装配为投递载荷（payload 非法 JSON 时回落
// `"{}"`——事件面保证 payload 恒为 JSON 文本，此处是防御性兜底）。
func NewEventPayload(seq int64, atUnix int64, name, subject, rawPayload string) Payload {
	if rawPayload == "" {
		rawPayload = "{}"
	}
	if !json.Valid([]byte(rawPayload)) {
		rawPayload = "{}"
	}
	return Payload{
		Type:    PayloadTypeEvent,
		Seq:     seq,
		At:      atUnix,
		Name:    name,
		Subject: subject,
		Payload: json.RawMessage(rawPayload),
	}
}

// NewTestPayload 构造 type=test 载荷（结构同真实事件：seq=0、name=test、
// subject 指向被测端点、payload 空——设计 §5.2「验证连通与验签配置」）。
func NewTestPayload(endpointID string, atUnix int64) Payload {
	return Payload{
		Type:    PayloadTypeTest,
		Seq:     0,
		At:      atUnix,
		Name:    "test",
		Subject: "webhook:" + endpointID,
		Payload: json.RawMessage("{}"),
	}
}

// MarshalPayload 序列化载荷（json.Marshal 结构体字段序确定——同一载荷的
// body 字节恒定，签发与复算不漂移）。
func MarshalPayload(p Payload) ([]byte, error) {
	return json.Marshal(p)
}

// SignatureHeaderValue 返回 `sha256=<hex>` 形态的签名头值：
// HMAC-SHA256(secret, timestamp + "." + body)。
func SignatureHeaderValue(secret []byte, timestampUnix int64, body []byte) string {
	return "sha256=" + hex.EncodeToString(hmacSHA256(secret, timestampUnix, body))
}

// VerifySignature 是签名算法的参考实现（正向单测与运维侧复核用——生产
// 投递只签不发验）：常量时间比对，防时序侧信道。
func VerifySignature(secret []byte, timestampUnix int64, body []byte, headerValue string) bool {
	expected := SignatureHeaderValue(secret, timestampUnix, body)
	return hmac.Equal([]byte(expected), []byte(headerValue))
}

// hmacSHA256 计算 HMAC-SHA256(secret, "<ts>.<body>") 的摘要字节。
func hmacSHA256(secret []byte, timestampUnix int64, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(timestampUnix, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}

// sendPayload 是签名 POST 的执行体（Manager webhook 投递与 TestEndpoint
// 共链路——同一签名/头/超时/Close 语义，验签配置的验证才有意义）。
func sendPayload(ctx context.Context, client *http.Client, rawURL string, secret []byte, body []byte, timeout time.Duration) (bool, int, string) {
	ts := time.Now().UTC().Unix()
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return false, 0, "request build failed: " + err.Error()
	}
	req.Header.Set("Content-Type", HeaderContentType)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, SignatureHeaderValue(secret, ts, body))
	req.Close = true // receiver-friendly：投递完即断，不占接收方连接
	resp, err := client.Do(req)
	if err != nil {
		return false, 0, "post failed: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, resp.StatusCode, fmt.Sprintf("receiver answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return true, resp.StatusCode, ""
}
