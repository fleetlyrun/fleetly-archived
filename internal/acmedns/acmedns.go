// Package acmedns 是 DNS-01 challenge 服务商插件（B 线 W5 设计 §3.1，
// D-V3W5-3：DNSPod + Cloudflare 双插件首发）：TXT 记录的建删接口 + 注册表。
//
// 契约（设计 §3.1 原文 + 实现票定形）：
//   - Provider.Present(ctx, fqdn, value)：在 fqdn（形如
//     "_acme-challenge.<domain>"，**无尾点**——适配层裁剪）建 TXT 记录，
//     value 为 TXT 内容（ACME 库对 keyAuth 求形后的 digest——lego
//     dns01.GetChallengeInfo().Value，base64url 无填充 SHA256）；
//   - Provider.CleanUp(ctx, fqdn, value)：删 fqdn 上**值等于 value** 的
//     TXT 记录（值匹配而非只按名删——同名多条 TXT 时不错删他人记录，幂
//     等可重入：无匹配即 no-op）。票面 CleanUp(ctx, fqdn) 的定形自由度
//     （「按实际 ACME 库的 DNS-01 回调签名定形」）由 lego v4 的
//     CleanUp(domain, token, keyAuth) 签名承接——实现按值删是该签名的
//     忠实形态；
//   - 零第三方 SDK（设计明示）：两家的 REST 足够简单，标准库 HTTP + JSON
//     /表单直写。可注入的 httpClient 与 baseURL 只服务于 httptest 假端点
//     （生产零注入）。
//
// 凭证形态（S3 设置同款 envelope 加密的前置明文形态，内容按 provider 定形）：
//   - dnspod:      {"api_token":"<id>,<token>"}（DNSPod Token 的 ID,Token 形态）
//   - cloudflare:  {"api_token":"<token>"}（最小权限 Zone.DNS Edit）
//
// 明文只在写入请求（API 面）与解密后构造 Provider 的瞬时内存出现，绝不进
// 日志/错误文本（错误只回 provider 状态码与 message，不回显凭证）。
package acmedns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// provider 词表（platform_settings acme.dns.provider 同词表——state 层
// 是唯一登记点，本包以注册表形态对齐；两表一致性由 state 侧测试互钉）。
const (
	ProviderDNSPod     = "dnspod"
	ProviderCloudflare = "cloudflare"
)

// defaultTimeout 是单次 API 请求的缺省超时（无注入 httpClient 时构造）。
const defaultTimeout = 15 * time.Second

// Provider 是 DNS-01 challenge 的 TXT 记录读写面（接口见包注释）。
type Provider interface {
	Present(ctx context.Context, fqdn, value string) error
	CleanUp(ctx context.Context, fqdn, value string) error
}

// Option 是 Provider 构造选项（测试注入口；生产零注入）。
type Option func(*options)

type options struct {
	httpClient *http.Client
	// baseURL 覆盖 provider 的 API 基址（httptest 假端点；空 = 官方端点）。
	baseURL string
}

// WithBaseURL 覆盖 API 基址（httptest 假端点；生产禁用——错误形态诚实
// 面向官方端点）。
func WithBaseURL(u string) Option { return func(o *options) { o.baseURL = u } }

// New 按 provider 名构造实例（注册表——名字与本包词表一致；credentials
// 为该 provider 的 JSON 凭证明文，形态见包注释）。
func New(name string, credentials []byte, opts ...Option) (Provider, error) {
	switch name {
	case ProviderDNSPod:
		return newDNSPod(credentials, opts...)
	case ProviderCloudflare:
		return newCloudflare(credentials, opts...)
	default:
		return nil, fmt.Errorf("acmedns: unknown dns provider %q (supported: %s)",
			name, strings.Join(Providers(), ", "))
	}
}

// Providers 返回注册表内的 provider 名（字典序——诊断/错误文案出口）。
func Providers() []string {
	return []string{ProviderCloudflare, ProviderDNSPod}
}

// probeRecordPrefix 是探针 TXT 记录名前缀（实现票：建删真实 TXT
// `_acme-challenge-test.<base_domain>` 验证权限，删后清场）。
const probeRecordPrefix = "_acme-challenge-test."

// ProbeRecordName 返回平台域名的探针 TXT 记录名。
func ProbeRecordName(baseDomain string) string {
	return probeRecordPrefix + baseDomain
}

// ProbeStep 是探针单步结果（诚实契约：失败步可定位）。
type ProbeStep struct {
	Step   string
	OK     bool
	Cost   time.Duration
	ErrMsg string
}

// ProbeResult 是探针结构化结果：ok=false 时 FailedStep 指向首个失败步。
type ProbeResult struct {
	OK         bool
	FailedStep string
	Steps      []ProbeStep
}

// Probe 对真实 DNS API 执行一轮「建 TXT → 删 TXT」权限探针（两步真实往
// 返——通过 = 能认证/能写/能删；zone 解析在 Present 内部隐含执行，凭证
// 错误/无权限在 create 步即失败）。recordName = ProbeRecordName(base)。
// 失败步短路（create 失败 = 无记录可删，delete 不执行）；删除失败如实
// 报错（残留在调用方面前可见，不静默吞）。
func Probe(ctx context.Context, p Provider, recordName string) ProbeResult {
	res := ProbeResult{Steps: make([]ProbeStep, 0, 2)}
	run := func(step string, fn func() error) bool {
		start := now()
		err := fn()
		st := ProbeStep{Step: step, OK: err == nil, Cost: now().Sub(start)}
		if err != nil {
			st.ErrMsg = err.Error()
		}
		res.Steps = append(res.Steps, st)
		if err != nil {
			if res.FailedStep == "" {
				res.FailedStep = step
				res.OK = false
			}
			return false
		}
		return true
	}
	const probeValue = "fleetly-dns-probe"
	if run("create", func() error { return p.Present(ctx, recordName, probeValue) }) {
		run("delete", func() error { return p.CleanUp(ctx, recordName, probeValue) })
	}
	res.OK = res.FailedStep == ""
	return res
}

// credentials 是两家共享的凭证 JSON 形态（内容按 provider 定形——目前
// 都是单 api_token 字段；分字段定义保留词表只增的扩展位）。
type credentials struct {
	APIToken string `json:"api_token"`
}

// CredentialsEnvelope 把 API 面的裸 api_token 词形包装为插件层的凭证 JSON
// 信封（本包 credentials 形态的唯一构造出口——调用方不拼 JSON 字面量）。
// W5-S4 门上修复（e2e/control-plane-tls.sh AC-5 实证补获）：CLI/API 的
// 凭证口径是裸 token（dnspod = "<id>,<token>"、cloudflare = 单 token），
// 而插件解析面 parseCredentials 只认 JSON 信封——保存与探针两处入参若
// 不经包装，生产装配（零 factory 注入）必在 parseCredentials 处断裂。
// api_token 内含的双引号/反斜杠由 encoding/json 转义兜底（词形不变式：
// Envelope → parseCredentials 恒还原同一 token）。
func CredentialsEnvelope(apiToken string) ([]byte, error) {
	return json.Marshal(credentials{APIToken: apiToken}) //nolint:gosec // G117：序列化即凭证信封的存储形态（包唯一构造出口），非泄漏面
}

// parseCredentials 解析并校验凭证 JSON（api_token 必填非空）。
func parseCredentials(raw []byte) (credentials, error) {
	var c credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return credentials{}, fmt.Errorf("acmedns: parse credentials json: %w", err)
	}
	if strings.TrimSpace(c.APIToken) == "" {
		return credentials{}, fmt.Errorf("acmedns: credentials must carry a non-empty api_token")
	}
	return c, nil
}

// trimTrailingDot 去掉 DNS 协议形态 FQDN 的尾点（provider API 以无尾点
// 域名书写；lego 的 EffectiveFQDN 恒带尾点）。
func trimTrailingDot(name string) string {
	return strings.TrimSuffix(name, ".")
}

// now 是探针计时的时钟出口（单测注入确定性耗时；生产恒 time.Now）。
var now = time.Now

// candidateSuffixes 把记录名解析为 zone 候选集（最长优先）：剥去首标签
// （_acme-challenge. / _acme-challenge-test. 前缀族）后逐级去头——
// "_acme-challenge.console.example.com" → ["console.example.com",
// "example.com"]。保底两标签（TLD 单标签不是 zone）。
func candidateSuffixes(recordName string) []string {
	rest := recordName
	if i := strings.Index(rest, "."); i >= 0 {
		rest = rest[i+1:]
	}
	labels := strings.Split(rest, ".")
	var out []string
	for len(labels) >= 2 {
		out = append(out, strings.Join(labels, "."))
		labels = labels[1:]
	}
	return out
}

// subDomainOf 取记录名相对 zone 的主机记录段（DNSPod sub_domain 形态）：
// "_acme-challenge.console.example.com" 相对 "example.com" →
// "_acme-challenge.console"。
func subDomainOf(recordName, zone string) string {
	return strings.TrimSuffix(recordName, "."+zone)
}
