package acmedns

// DNSPod 传统用户 API 插件（D-V3W5-3；staging 真机在用形态）。
//
// API 形态实证（官方文档 docs.dnspod.cn/api/，2026-09 核对——实现票「API
// api.dnspod.cn/infoversion 核实」的结论：login_token 表单形态的现行端点
// 基址是 https://dnsapi.cn/，api.dnspod.cn 为旧域名形态，官方文档现指
// dnsapi.cn）：
//   - 全部接口 POST 表单（application/x-www-form-urlencoded）：
//     login_token=<ID>,<Token> + format=json + 业务参数；
//   - 应答恒 JSON：{"status":{"code":..,"message":..}, ...}——code=="1"
//     （字符串或数字形态，宽容解析）为成功，其余为业务错误；
//   - Domain.Info：domain=<域名> → 域名归属与 domain.id（域名→domain_id
//     解析点；不存在的域名/非本账号域名返回错误码）；
//   - Record.Create：domain_id + sub_domain + record_type=TXT +
//     record_line=默认 + value + ttl → record.id；
//   - Record.List：domain_id + sub_domain + record_type 过滤，分页
//     offset/length（缺省首页 100、单页上限 3000）——本插件按 sub_domain
//     过滤后单页拉满（TXT 记录单名数量级远小于上限，仍以 record_total
//     驱动翻页，分页语义如实处理）；
//   - Record.Remove：domain_id + record_id。
//
// 错误形态：HTTP 非 2xx 或 status.code != 1 → 错误（文案 = 状态码 +
// message；凭证材料零出现）。User-Agent 恒置（DNSPod API 要求非空 UA）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// dnspodAPIBase 是 DNSPod 传统 API 的官方基址。
const dnspodAPIBase = "https://dnsapi.cn"

// dnspodRecordTTL 是挑战 TXT 的 TTL（秒；DNSPod 免费档最小 600——保守取下限，
// challenge 记录本就该短命）。
const dnspodRecordTTL = "600"

// dnspodUserAgent 是 API 要求的非空 UA。
const dnspodUserAgent = "fleetly-acmedns/1.0 (+https://fleetly.run)"

// dnspod 是 DNSPod Provider。
type dnspod struct {
	// loginToken 是 "ID,Token" 形态的登录令牌（构造期定形）。
	loginToken string
	opts       options
}

// newDNSPod 构造 DNSPod Provider（credentials = {"api_token":"<id>,<token>"}）。
func newDNSPod(credentials []byte, opts ...Option) (Provider, error) {
	cred, err := parseCredentials(credentials)
	if err != nil {
		return nil, err
	}
	id, token, found := strings.Cut(cred.APIToken, ",")
	if !found || strings.TrimSpace(id) == "" || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("acmedns: dnspod api_token must be the DNSPod token form \"<id>,<token>\"")
	}
	o := options{httpClient: http.DefaultClient}
	for _, fn := range opts {
		fn(&o)
	}
	if o.httpClient == http.DefaultClient {
		o.httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &dnspod{loginToken: cred.APIToken, opts: o}, nil
}

// dnspodStatus 是应答的 status 对象（code 宽容解析——字符串/数字同形）。
type dnspodStatus struct {
	Code    json.Number `json:"code"`
	Message string      `json:"message"`
}

// dnspodDomainInfo 是 Domain.Info 应答的 domain 段。
type dnspodDomainInfo struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
}

// dnspodRecord 是 Record.List 应答的 records 元素。
type dnspodRecord struct {
	ID    json.Number `json:"id"`
	Name  string      `json:"name"`
	Type  string      `json:"type"`
	Value string      `json:"value"`
}

// dnspodResponse 是共用应答信封（按需解码各段）。
type dnspodResponse struct {
	Status  dnspodStatus     `json:"status"`
	Domain  dnspodDomainInfo `json:"domain"`
	Records []dnspodRecord   `json:"records"`
	Info    dnspodListInfo   `json:"info"`
}

// dnspodListInfo 是 Record.List 应答的 info 段（record_total 驱动翻页）。
type dnspodListInfo struct {
	RecordTotal int `json:"record_total"`
}

// call 执行一次表单 POST 并解析信封（status.code==1 视为成功；HTTP 非 2xx
// 或业务错误码 → 错误，文案不含凭证）。
func (d *dnspod) call(ctx context.Context, action string, form url.Values) (*dnspodResponse, error) {
	form = cloneForm(form)
	form.Set("login_token", d.loginToken)
	form.Set("format", "json")
	body := strings.NewReader(form.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL()+"/"+action, body)
	if err != nil {
		return nil, fmt.Errorf("acmedns: dnspod build request %s: %w", action, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", dnspodUserAgent)
	resp, err := d.opts.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("acmedns: dnspod %s: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("acmedns: dnspod %s read response: %w", action, err)
	}
	var out dnspodResponse
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		return nil, fmt.Errorf("acmedns: dnspod %s: HTTP %s with non-JSON body: %w", action, resp.Status, jerr)
	}
	if out.Status.Code.String() != "1" {
		return nil, fmt.Errorf("acmedns: dnspod %s failed: status.code=%s status.message=%q (HTTP %s)",
			action, nonEmptyOrStr(out.Status.Code.String(), "?"), out.Status.Message, resp.Status)
	}
	return &out, nil
}

// baseURL 是 API 基址（注入覆盖优先，官方端点兜底）。
func (d *dnspod) baseURL() string {
	if d.opts.baseURL != "" {
		return d.opts.baseURL
	}
	return dnspodAPIBase
}

// zoneFor 把记录名解析为（domain_id, zone 名）：从剥去首标签的最长候选
// 逐个 Domain.Info 探测（_acme-challenge.console.example.com → 依次试
// console.example.com / example.com），命中即返回。全不命中 → 可重试错误
// （zone 不在本账号 = 权限面缺失，Present 失败即 challenge 失败）。
func (d *dnspod) zoneFor(ctx context.Context, recordName string) (string, string, error) {
	for _, candidate := range candidateSuffixes(recordName) {
		out, err := d.call(ctx, "Domain.Info", url.Values{"domain": {candidate}})
		if err != nil {
			continue // 该候选不是本账号 zone（或网络错误）：继续次优候选
		}
		return out.Domain.ID.String(), out.Domain.Name, nil
	}
	return "", "", fmt.Errorf("acmedns: dnspod: no zone owned by this account covers %q", recordName)
}

// Present 在 fqdn 建 TXT 记录 value（zone 解析 → Record.Create）。
func (d *dnspod) Present(ctx context.Context, fqdn, value string) error {
	name := trimTrailingDot(fqdn)
	domainID, zone, err := d.zoneFor(ctx, name)
	if err != nil {
		return err
	}
	_, err = d.call(ctx, "Record.Create", url.Values{
		"domain_id":   {domainID},
		"sub_domain":  {subDomainOf(name, zone)},
		"record_type": {"TXT"},
		"record_line": {"默认"},
		"value":       {value},
		"ttl":         {dnspodRecordTTL},
	})
	return err
}

// CleanUp 删 fqdn 上值等于 value 的 TXT 记录（Record.List 按 sub_domain+
// record_type 过滤分页**先全量收集**值匹配项、再逐条 Record.Remove——边扫
// 边删会使 offset 翻页位移漏删；无匹配 = 幂等 no-op）。
func (d *dnspod) CleanUp(ctx context.Context, fqdn, value string) error {
	name := trimTrailingDot(fqdn)
	domainID, zone, err := d.zoneFor(ctx, name)
	if err != nil {
		return err
	}
	sub := subDomainOf(name, zone)
	const pageSize = 3000 // 官方单页上限（sub_domain 过滤后的全量语义）
	var matched []string
	for offset := 0; ; offset += pageSize {
		out, err := d.call(ctx, "Record.List", url.Values{
			"domain_id":   {domainID},
			"sub_domain":  {sub},
			"record_type": {"TXT"},
			"offset":      {fmt.Sprint(offset)},
			"length":      {fmt.Sprint(pageSize)},
		})
		if err != nil {
			return err
		}
		for _, r := range out.Records {
			if r.Value == value {
				matched = append(matched, r.ID.String())
			}
		}
		// 翻页判据（offset/length 口径如实处理）：空页或扫满 record_total 即止。
		if len(out.Records) == 0 || offset+len(out.Records) >= out.Info.RecordTotal {
			break
		}
	}
	for _, recordID := range matched {
		if _, err := d.call(ctx, "Record.Remove", url.Values{
			"domain_id": {domainID},
			"record_id": {recordID},
		}); err != nil {
			return err
		}
	}
	return nil
}

// cloneForm 复制表单（call 追加公共参数不改调用方视图）。
func cloneForm(in url.Values) url.Values {
	out := url.Values{}
	for k, vs := range in {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// nonEmptyOrStr 空串回落缺省（错误文案占位用）。
func nonEmptyOrStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
