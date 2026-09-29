package ingress

// DNS-01 challenge 挂点（B 线 W5 设计 §3.2，D-V3W5-4；v0.3 W5-S3）：
// lego 的 DNS-01 走 SetDNS01Provider（challenge.Provider 同接口族——
// Present/CleanUp(domain, token, keyAuth)），本文件是 acmedns.Provider
// 插件与 lego 的适配层 + 平台证书签发路径的 DNS-01 分支决策。
//
// 适用面（设计 §3.2 口径）：**仅平台证书**（platformCertApp）在
// acme.wildcard=true 时走 DNS-01——通配 SAN 无法 HTTP-01 验证；app 级
// 证书路径零变化（per-app HTTP-01 保留给自定义域名场景；app 路由 443 由
// 通配证书经 Traefik tls.certificates 默认 store 的 SNI 匹配兜底覆盖，
// 无需路由改动）。
//
// 失败语义（诚实不静默）：wildcard=true 而设置不可读 / provider 未配 /
// 凭证解密失败 → 签发显式失败（收敛循环退避重试 + cert 审计 error 行）——
// **不回落 HTTP-01**：回落会按残缺方式打 CA 限额（wildcard SAN 的
// HTTP-01 必然失败），与平台证书控制器「不得按残缺集签发」同口径。

import (
	"context"
	"fmt"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"

	"github.com/fleetlyrun/fleetly/internal/acmedns"
)

// DNSProviderResolver 是 DNS-01 插件解析缝（生产实现 = 装配点闭包：读
// acme.* 设置 → envelope 解密凭证 → acmedns.New；nil = DNS-01 面未装配
// ——wildcard 开启时签发如实失败）。每签发调用一次（凭证现读不缓存——
// 轮换即生效），Provider 实例生命周期与单次签发一致。
type DNSProviderResolver func(ctx context.Context) (acmedns.Provider, error)

// WithDNSProviderResolver 注入 DNS-01 插件解析缝（链式装配，nil 合法——
// 单测/未装配形态）。
func (m *Manager) WithDNSProviderResolver(fn DNSProviderResolver) *Manager {
	m.dnsProviderFn = fn
	return m
}

// dns01ChallengeAdapter 是 acmedns.Provider → lego challenge.Provider 的
// 适配层：lego 回调 (domain, token, keyAuth)，TXT 记录名与值经
// dns01.GetChallengeInfo 求形（EffectiveFQDN 带尾点 → 适配层裁剪；Value
// = keyAuth 的 base64url 无填充 SHA256 digest）后转交插件。
type dns01ChallengeAdapter struct {
	provider acmedns.Provider
}

// Present 实现 challenge.Provider：在 _acme-challenge.<domain> 建 TXT。
func (a *dns01ChallengeAdapter) Present(domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	return a.provider.Present(context.Background(), trimFQDN(info.EffectiveFQDN), info.Value)
}

// CleanUp 实现 challenge.Provider：删值匹配的 TXT（幂等）。
func (a *dns01ChallengeAdapter) CleanUp(domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	return a.provider.CleanUp(context.Background(), trimFQDN(info.EffectiveFQDN), info.Value)
}

var _ challenge.Provider = (*dns01ChallengeAdapter)(nil)

// trimFQDN 裁剪 DNS 协议形态尾点（acmedns 插件契约：无尾点记录名）。
func trimFQDN(name string) string {
	if len(name) > 0 && name[len(name)-1] == '.' {
		return name[:len(name)-1]
	}
	return name
}

// dns01ForPlatform 报告平台证书本次签发是否走 DNS-01（分支决策单点）：
// base_domain 非空 + acme.wildcard=true。设置读取失败显式报错（不静默按
// false 走 HTTP-01——期望集里有通配 SAN 时 HTTP-01 必然失败，按残缺形态
// 消耗 CA 限额不如显式失败退避重试）。
func (m *Manager) dns01ForPlatform(ctx context.Context) (bool, error) {
	if !m.ConfigTLSEnabled() {
		return false, nil
	}
	in, err := m.store.LoadAcmeSettings(ctx)
	if err != nil {
		return false, fmt.Errorf("ingress: load acme settings: %w", err)
	}
	return in.Wildcard, nil
}

// resolveDNSProvider 把 DNS-01 插件解析缝求值（wildcard 开启时调用）：
// 未装配缝 = 显式错误（签发面缺件——诚实失败，收敛循环退避）。
func (m *Manager) resolveDNSProvider(ctx context.Context) (acmedns.Provider, error) {
	if m.dnsProviderFn == nil {
		return nil, fmt.Errorf("ingress: dns provider resolver not assembled (dns-01 unavailable; wildcard issuance cannot proceed)")
	}
	provider, err := m.dnsProviderFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingress: resolve dns provider: %w", err)
	}
	return provider, nil
}
