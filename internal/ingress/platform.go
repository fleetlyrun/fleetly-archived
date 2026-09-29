package ingress

// 平台证书控制器（E1-3；E1 多节点设计 §2.4 Bootstrap 次序 + D-MN-6）：
// base_domain 非空时，启动即确保平台证书签发（多 SAN 一张：ctrl/registry/
// console.<base>，一次签发覆盖三子域、续期同批），重试直至成功——证书就
// 续前 provider endpoint 维持明文 8422（Traefik 容忍期只有挑战面在用），
// 就绪后翻转 https://<advertise>:8423（见 traefik.go providerEndpoint，F9
// 修订二：VPC IP 直连形态）并
// 由 runtime 装配壳在 8423 起 TLS 监听（证书未就绪时握手失败 = 「8423 尚
// 不可用」，次序③④）。
//
// 与 app 证书的关系：共用证书库（cert_dir 落盘 0600 真源）、签发互斥
//（issueMu）、obtainFn 注入缝与账号失效自愈；不入域名台账（平台证书不属
// 任何 app，domains 行是 app 域名面）——审计留痕即事实记录（§5.3「证书
// 签发与续期 → 审计记录，不设事件」）。

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// platformCertApp 是平台证书在证书库中的保留名（cert_dir 文件名前缀）。
// 前导下划线不可能出现在 compose project 名的首字符（project 名必须以字
// 母数字开头），用户 app 名与之相撞在命名契约层面被排除。
const platformCertApp = "_fleetly-platform"

// platformCertRetryInterval 是平台证书签发的重试退避（设计 §2.4 次序③：
// 「启动重试，退避 ~30s 直到成功」）。Manager.platformRetryInterval 可注入
// 覆盖（单测）。
const platformCertRetryInterval = 30 * time.Second

// PlatformDomains 返回平台证书的**基础** SAN 域名集（D-MN-6：ctrl/registry/
// console.<base>）。base_domain 为空时无意义（控制器不启动）。公网子域开关
// （E3-6，s3.public_exposed）开启时实际签发集条件增第四 SAN s3.<base>——
// 生产签发路径走 platformDomainsWithS3（期望态函数，现读 s3 设置）；本函数
// 是基础集（诊断/测试出口），不读设置。
func (m *Manager) PlatformDomains() []string {
	return []string{
		"ctrl." + m.cfg.BaseDomain,
		"registry." + m.cfg.BaseDomain,
		"console." + m.cfg.BaseDomain,
	}
}

// ConfigTLSEnabled 报告 8423 配置端点 TLS 面是否启用（base_domain 非空，
// 设计 §2.4 端口分面表；空 = 单节点 v0.1 形态逐字等价——零新监听、零
// endpoint 变化）。
func (m *Manager) ConfigTLSEnabled() bool { return m.cfg.BaseDomain != "" }

// platformCertOnDisk 报告平台证书是否已落盘（provider endpoint 翻转的就绪
// 判定；sticky——一经签发持续存在，续期同路径换入，endpoint 不回摆）。
func (m *Manager) platformCertOnDisk() bool {
	_, err := m.certs.Load(platformCertApp)
	return err == nil
}

// PlatformTLSCertificate 加载平台证书供 8423 TLS 面握手（tls.Config.
// GetCertificate 的取数出口；证书未就绪返回错误——握手失败即「8423 尚不
// 可用」，Traefik 对 provider 不可达的原生容忍承接空窗）。
func (m *Manager) PlatformTLSCertificate() (*tls.Certificate, error) {
	pair, err := m.certs.Load(platformCertApp)
	if err != nil {
		return nil, fmt.Errorf("ingress: platform certificate not ready: %w", err)
	}
	cert, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("ingress: parse platform certificate: %w", err)
	}
	return &cert, nil
}

// PlatformCertPaths 返回平台证书的落盘文件路径（cert/key PEM，cert_dir
// 真源——路径公式单源在 certStore）。控制面 TLS platform 模式（V2-8）的
// 证书读取入口；E7 §3.3 的 exec relay 管理器（S6）同样经此取路径下发给
// relay 任务做拨号参数。base_domain 为空返回错误（无平台证书可言——调用
// 方先以 ConfigTLSEnabled/自身配置门禁把关）。
func (m *Manager) PlatformCertPaths() (certFile, keyFile string, err error) {
	if !m.ConfigTLSEnabled() {
		return "", "", fmt.Errorf("ingress: platform certificate paths unavailable (base_domain is not configured)")
	}
	return m.certs.certPath(platformCertApp), m.certs.keyPath(platformCertApp), nil
}

// PlatformTLSName 返回平台证书 SAN 中承载控制面的主机名（ctrl.<base>）：
// exec relay（S6，设计 §3.3）拨 advertise IP 但按此名做 TLS 校验的
// ServerName 下发源。base_domain 为空返回错误（同 PlatformCertPaths 口径）。
func (m *Manager) PlatformTLSName() (string, error) {
	if !m.ConfigTLSEnabled() {
		return "", fmt.Errorf("ingress: platform TLS name unavailable (base_domain is not configured)")
	}
	return "ctrl." + m.cfg.BaseDomain, nil
}

// ensurePlatformCertificate 确保平台证书就绪：缺/进续期窗口才签发，已健
// 康直接返回既有证书对。签发共用 issueMu 互斥 + obtainFn 注入缝 + 账号
// 自愈（obtainPEMWithHeal）；失败写 cert 审计 error 行（renewing 区分
// issued/renewed 文案）。
func (m *Manager) ensurePlatformCertificate(ctx context.Context, renewing bool) (*CertificatePair, error) {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	// SAN 期望态集（E3-6：公网开关条件增第四 SAN）——设置读取失败显式失败
	// 退避重试（不得回落基础集：签发是不可逆消耗 LE 限额的动作，按残缺集
	// 签发会先缩 SAN 再补签，两次真实签发换一次抖动）。
	domains, err := m.platformDomainsWithS3(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := m.certs.Load(platformCertApp)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		existing = nil
	default:
		return nil, fmt.Errorf("ingress: load platform cert: %w", err)
	}
	if !needsRenewal(existing, domains, m.nowFunc(), m.cfg.RenewBefore) {
		return existing, nil
	}
	certPEM, keyPEM, err := m.obtainPEMWithHeal(ctx, platformCertApp, domains)
	if err != nil {
		m.writeCertAudit(ctx, renewing, platformCertApp, existing, err)
		return nil, err
	}
	pair, err := ParsePair(platformCertApp, domains, certPEM, keyPEM)
	if err != nil {
		m.writeCertAudit(ctx, renewing, platformCertApp, existing, err)
		return nil, err
	}
	if err := m.certs.Save(pair); err != nil {
		m.writeCertAudit(ctx, renewing, platformCertApp, existing, err)
		return nil, err
	}
	m.writeCertAudit(ctx, renewing, platformCertApp, pair, nil)
	return pair, nil
}

// runPlatformCertController 是平台证书控制器 的常驻循环（Manager.Run 启动的独立
// goroutine；ctx 取消返回）：
//
//	未就绪/进窗 → 签发（重试退避 platformRetryInterval，默认 ~30s）；
//	就绪且健康 → 确保 Traefik 静态参数已切 TLS endpoint（一次收敛成功后
//	不再重复；失败按同节奏重试），随后按续期扫描周期守望。
//
// 次序保证（设计 §2.4）：本循环不阻塞 sweep——Traefik 部署与挑战面在证书
// 就绪前照常收敛（容忍期），签发成功后 endpoint 才翻转。
func (m *Manager) runPlatformCertController(ctx context.Context) {
	if !m.ConfigTLSEnabled() {
		return
	}
	if !m.cfg.ACME.ACMEEnabled() {
		m.log.Warn("ingress: platform certificate controller inert (acme disabled; the 8423 TLS config face will not become available)")
		return
	}
	retry := m.platformRetryInterval
	if retry <= 0 {
		retry = platformCertRetryInterval
	}
	converged := false
	for {
		// SAN 期望态集现读（E3-6：公网开关条件增第四 SAN；读取失败按退避
		// 重试——不得按残缺集做续期判定，理由见 ensurePlatformCertificate）。
		domains, err := m.platformDomainsWithS3(ctx)
		if err != nil {
			m.log.Warn("ingress: platform certificate domain set unreadable (retrying)",
				"error", err, "retry_in", retry.String())
			if !sleepCtx(ctx, retry) {
				return
			}
			continue
		}
		pair, err := m.certs.Load(platformCertApp)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist):
			pair = nil
		default:
			m.log.Warn("ingress: platform certificate load failed", "error", err)
			if !sleepCtx(ctx, retry) {
				return
			}
			continue
		}
		if pair == nil || needsRenewal(pair, domains, m.nowFunc(), m.cfg.RenewBefore) {
			if _, err := m.ensurePlatformCertificate(ctx, pair != nil); err != nil {
				m.log.Warn("ingress: platform certificate issue deferred (retrying)",
					"error", err, "retry_in", retry.String())
				if !sleepCtx(ctx, retry) {
					return
				}
				continue
			}
			// 落盘即真源就绪（8423 TLS 面自此可握手）；endpoint 收敛在
			// 下一拍立即执行（不再等待退避）。
			converged = false
			continue
		}
		if !converged {
			// 证书在盘：翻转 provider endpoint 至 https://<advertise>:8423
			//（幂等；swarm 未就绪/版本冲突按退避重试——与 sweep 的降级
			// 语义同构）。8423 监听本身由 runtime 服务壳装配。
			if err := m.EnsureTraefik(ctx); err != nil {
				m.log.Warn("ingress: traefik endpoint TLS convergence deferred (retrying)",
					"error", err, "retry_in", retry.String())
				if !sleepCtx(ctx, retry) {
					return
				}
				continue
			}
			converged = true
			// 证书就绪后立即重发布视图（F8 修复，2026-09-21 真机发现）：签发/
			// 落盘常发生在启动发布之后，websecure 路由与内联证书段若等 12h
			// sweep 才进视图，平台子域 443 在冷启动后长时间缺席（registry 404/
			// 默认自签证书）。翻转 endpoint 的同一拍同步刷视图，幂等。
			if err := m.republishAll(ctx); err != nil {
				m.log.Warn("ingress: post-cert view republish deferred (picked up by next sweep)",
					"error", err)
			}
			m.log.Info("ingress: platform certificate ready; traefik provider endpoint switched to the TLS config face",
				"base_domain", m.cfg.BaseDomain,
				"endpoint", m.providerEndpoint(m.advertiseIP)+"/configs")
		}
		// 健康守望：睡一个续期扫描周期后复查续期窗口。
		if !sleepCtx(ctx, m.cfg.RenewScanInterval) {
			return
		}
	}
}

// obtainPEMWithHeal 是「Obtain + 账号失效自愈重试一次」的共用段：app 证书
// （ensureCertificate→obtainAndRegister）与平台证书两条签发路径共用。CA
// 侧账号丢失（pebble 重启/账号库重置）时 Obtain 报 accountDoesNotExist
// ——清除注册 sidecar 重新注册并重试一次；其余错误原样返回。
func (m *Manager) obtainPEMWithHeal(ctx context.Context, app string, domains []string) ([]byte, []byte, error) {
	certPEM, keyPEM, err := m.obtainOnce(ctx, app, domains)
	if err == nil {
		return certPEM, keyPEM, nil
	}
	if !strings.Contains(err.Error(), "accountDoesNotExist") {
		return nil, nil, err
	}
	m.log.Warn("ingress: acme account missing on CA (re-registering)", "app", app)
	m.invalidateAccount()
	return m.obtainOnce(ctx, app, domains)
}

// obtainOnce 是单次「账号 → 注入缝 Obtain」（自愈重试的最小步）。
func (m *Manager) obtainOnce(ctx context.Context, app string, domains []string) ([]byte, []byte, error) {
	user, err := m.ensureAccount(ctx)
	if err != nil {
		return nil, nil, err
	}
	return m.obtainFn(ctx, app, user, domains)
}

// sleepCtx 可取消休眠（控制器循环的退避/守望载体；ctx 取消返回 false）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
