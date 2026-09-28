package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/ingress"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 只读资源面：Placement / Revisions / Domains（T2.17）。E1-7 起 Placement
// 扩容显式换点（UpdatePlacement）与跨 app 卷清单（ListVolumes）、迁移
// runbook（GetPlacementMigrationPlan）——multi-node §2.6/§2.8。

// PlacementService 实现 server.v1.PlacementService。res 是放置解析器
// （E1-7：换点裁决在 internal/placement——目标校验/数据处置门/同事务落库；
// api 面只做应用解析与投影；nil resolver = 测试形态的只读降级面——写面
// RPC 如实报不可用）。
type PlacementService struct {
	serverv1.UnimplementedPlacementServiceServer
	st  *state.Store
	res *placement.Resolver
}

// NewPlacementService 构造 PlacementService。
func NewPlacementService(st *state.Store, res *placement.Resolver) *PlacementService {
	return &PlacementService{st: st, res: res}
}

// ShowPlacement 放置绑定视图（未绑定时 placement 不输出）+ 卷注册表
// （T2.18：卷的钉住语义由放置绑定决定，与绑定同面展示）。
func (s *PlacementService) ShowPlacement(ctx context.Context, req *serverv1.ShowPlacementRequest) (*serverv1.ShowPlacementResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；层级随方法 scope 登记映射——读面 read、
	// UpdatePlacement=admin）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	resp := &serverv1.ShowPlacementResponse{App: app.Name, Volumes: []*serverv1.VolumeView{}}
	if p, err := s.st.GetPlacement(ctx, app.ID); err == nil {
		resp.Placement = placementView(p)
	} else if !errors.Is(err, state.ErrPlacementNotFound) {
		return nil, err
	}
	volumes, err := s.st.ListAppVolumes(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range volumes {
		resp.Volumes = append(resp.Volumes, volumeView(v))
	}
	return resp, nil
}

// UpdatePlacement 显式换点（E1-7，multi-node §2.6；admin scope——破坏性
// 确认路径）：裁决在 internal/placement.Rebind（目标校验 + data_ack 门 +
// 同事务落库/事件/审计），换点不自动部署。
func (s *PlacementService) UpdatePlacement(ctx context.Context, req *serverv1.UpdatePlacementRequest) (*serverv1.UpdatePlacementResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；层级随方法 scope 登记映射——读面 read、
	// UpdatePlacement=admin）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	if s.res == nil {
		return nil, status.Error(codes.Unavailable, "placement resolver unavailable (not assembled)")
	}
	res, err := s.res.Rebind(ctx, placement.RebindInput{
		AppID:   app.ID,
		Node:    req.GetNode(),
		DataAck: req.GetDataAck(),
		Confirm: req.GetConfirm(),
		Actor:   "human",
	})
	if err != nil {
		return nil, err
	}
	out := &serverv1.UpdatePlacementResponse{
		App:        app.Name,
		Placement:  placementView(res.Placement),
		Volumes:    []*serverv1.VolumeView{},
	}
	for _, v := range res.Volumes {
		out.Volumes = append(out.Volumes, volumeView(v))
	}
	return out, nil
}

// ListVolumes 跨 app 卷清单（E1-7，multi-node §2.8；read scope）：active/
// orphaned/discarded 行 + residual 派生标记（prev_platform_node_id 非空的
// active 行 = 源节点有待清理副本）。status 过滤缺省输出全部；不建生命
// 周期 API、不做远端删除（D18）。卷归属可从命名约定名读取
//（fleetly-<app>-<key>-<appid8>，state-model §2.4）。
func (s *PlacementService) ListVolumes(ctx context.Context, req *serverv1.ListVolumesRequest) (*serverv1.ListVolumesResponse, error) {
	rows, err := s.st.ListAllVolumes(ctx)
	if err != nil {
		return nil, err
	}
	out := &serverv1.ListVolumesResponse{Volumes: []*serverv1.VolumeView{}}
	for _, v := range rows {
		if filter := req.GetStatus(); filter != "" && string(v.Status) != filter {
			continue
		}
		residual := v.Status == state.VolumeActive && v.PrevPlatformNodeID != ""
		if req.GetResidual() && !residual {
			continue
		}
		out.Volumes = append(out.Volumes, volumeView(v))
	}
	return out, nil
}

// GetPlacementMigrationPlan restic 迁移 runbook（E1-7，multi-node §2.8/
// D-MN-10）：服务端生成步骤文档（真实卷名/节点名填充）；只读面，不触发
// 任何状态变更。
func (s *PlacementService) GetPlacementMigrationPlan(ctx context.Context, req *serverv1.GetPlacementMigrationPlanRequest) (*serverv1.GetPlacementMigrationPlanResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；层级随方法 scope 登记映射——读面 read、
	// UpdatePlacement=admin）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	if s.res == nil {
		return nil, status.Error(codes.Unavailable, "placement resolver unavailable (not assembled)")
	}
	plan, err := s.res.MigrationPlan(ctx, app.ID, app.Name, req.GetTo())
	if err != nil {
		return nil, err
	}
	out := &serverv1.GetPlacementMigrationPlanResponse{
		App:      app.Name,
		FromNode: plan.FromNode,
		ToNode:   plan.ToNode,
		Volumes:  []*serverv1.VolumeView{},
		Steps:    []*serverv1.MigrationStep{},
		Warnings: plan.Warnings,
	}
	for _, v := range plan.Volumes {
		out.Volumes = append(out.Volumes, volumeView(v))
	}
	for _, st := range plan.Steps {
		out.Steps = append(out.Steps, &serverv1.MigrationStep{Title: st.Title, Detail: st.Detail})
	}
	return out, nil
}

// RevisionsService 实现 server.v1.RevisionsService。
type RevisionsService struct {
	serverv1.UnimplementedRevisionsServiceServer
	st *state.Store
}

// NewRevisionsService 构造 RevisionsService。
func NewRevisionsService(st *state.Store) *RevisionsService {
	return &RevisionsService{st: st}
}

// ListRevisions 版本快照列表（保留窗；列表即回滚选项集）。
func (s *RevisionsService) ListRevisions(ctx context.Context, req *serverv1.ListRevisionsRequest) (*serverv1.ListRevisionsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；层级随方法 scope 登记映射——读面 read、
	// UpdatePlacement=admin）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rows, err := s.st.ListRevisions(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.RevisionView, 0, len(rows))
	for _, r := range rows {
		out = append(out, &serverv1.RevisionView{
			Id:          r.ID,
			Seq:         r.Seq,
			DesiredHash: r.DesiredHash,
			Status:      r.Status,
			Verified:    r.Verified,
			CreatedAt:   tstamp(r.CreatedAt),
		})
	}
	return &serverv1.ListRevisionsResponse{Revisions: out}, nil
}

// GetRevisionSpec 单条快照的归一化 compose 正文（canonical JSON；T2.18
// plan 的 RPC 基线消费——env 为 key:sha256，值明文结构性不在快照中）。
// superseded → 404（与回滚选项面同口径：列不出来的不可消费）。
func (s *RevisionsService) GetRevisionSpec(ctx context.Context, req *serverv1.GetRevisionSpecRequest) (*serverv1.GetRevisionSpecResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；层级随方法 scope 登记映射——读面 read、
	// UpdatePlacement=admin）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rev, err := s.st.GetAppRevision(ctx, app.ID, req.GetRevisionId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetRevisionId())
	}
	return &serverv1.GetRevisionSpecResponse{
		RevisionId: rev.ID,
		Seq:        rev.Seq,
		Compose:    rev.ComposeNormalized,
	}, nil
}

// DomainsService 实现 server.v1.DomainsService（T2.17 只读面 + IMPL-T1-1
// 可写升级）：域名资源 CRUD + 本机视角验证。域名资源是路由声明的唯一真值
// （compose label 仅首部署 bootstrap 种子）；写入成功后同步触发入口收敛
// （mgr 可空——测试/无 ingress 装配形态跳过收敛），收敛失败不撤销资源行：
// 落审计 error 行 + route.publish_failed 事件（下次部署/续期扫描恢复）。
type DomainsService struct {
	serverv1.UnimplementedDomainsServiceServer
	st  *state.Store
	mgr *ingress.Manager
}

// NewDomainsService 构造 DomainsService。
func NewDomainsService(st *state.Store, mgr *ingress.Manager) *DomainsService {
	return &DomainsService{st: st, mgr: mgr}
}

// domainViewOf 是域名行的视图投影（列表与写面回执共用的单点）。
func domainViewOf(d state.Domain) *serverv1.DomainView {
	return &serverv1.DomainView{
		Service:      d.Service,
		Domain:       d.Domain,
		Port:         d.Port,
		Protocol:     d.Protocol,
		CertMode:     d.CertMode,
		CertSha256:   d.CertSHA256,
		CertNotAfter: tstamp(d.CertNotAfter),
		CreatedAt:    tstamp(d.CreatedAt),
	}
}

// ListAppDomains 域名资源列表（排序：域名字典序）。
func (s *DomainsService) ListAppDomains(ctx context.Context, req *serverv1.ListAppDomainsRequest) (*serverv1.ListAppDomainsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；层级随方法 scope 登记映射——读面 read、
	// 写面 deploy）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rows, err := s.st.ListAppDomains(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.DomainView, 0, len(rows))
	for _, d := range rows {
		out = append(out, domainViewOf(d))
	}
	return &serverv1.ListAppDomainsResponse{Domains: out}, nil
}

// CreateAppDomain 新建域名资源（守卫④：host 冲突 409、超限 400 均点名；
// 上限与 compose label 契约同值 ≤5/服务、≤10/app）。
func (s *DomainsService) CreateAppDomain(ctx context.Context, req *serverv1.CreateAppDomainRequest) (*serverv1.CreateAppDomainResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	domain, err := compose.NormalizeDomain(req.GetDomain())
	if err != nil {
		return nil, err
	}
	input, err := domainInputOf(req.GetService(), req.GetPort(), req.GetProtocol(), req.GetCertMode(), domain,
		state.Domain{}) // 创建：空字段取平台缺省（http/http01）
	if err != nil {
		return nil, err
	}
	row, err := s.st.CreateAppDomain(ctx, app.ID, input)
	if err != nil {
		return nil, domainWriteError(err, domain)
	}
	if err := s.writeDomainAudit(ctx, "domain.created", app, row, "ok", ""); err != nil {
		return nil, err
	}
	s.convergeAfterDomainWrite(ctx, app, row.Domain)
	return &serverv1.CreateAppDomainResponse{Domain: domainViewOf(row)}, nil
}

// UpdateAppDomain 更新域名资源（{domain} 是寻址键 = host 不改名；空字段 =
// 保持现值——CLI 局部更新形态，Console 恒发全量）。
func (s *DomainsService) UpdateAppDomain(ctx context.Context, req *serverv1.UpdateAppDomainRequest) (*serverv1.UpdateAppDomainResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	domain, err := compose.NormalizeDomain(req.GetDomain())
	if err != nil {
		return nil, err
	}
	current, err := s.st.GetAppDomain(ctx, app.ID, domain)
	if err != nil {
		return nil, mapStoreErr(err, domain)
	}
	input, err := domainInputOf(req.GetService(), req.GetPort(), req.GetProtocol(), req.GetCertMode(), domain, current)
	if err != nil {
		return nil, err
	}
	row, err := s.st.UpdateAppDomain(ctx, app.ID, domain, input)
	if err != nil {
		return nil, domainWriteError(err, domain)
	}
	if err := s.writeDomainAudit(ctx, "domain.updated", app, row, "ok", ""); err != nil {
		return nil, err
	}
	s.convergeAfterDomainWrite(ctx, app, row.Domain)
	return &serverv1.UpdateAppDomainResponse{Domain: domainViewOf(row)}, nil
}

// RemoveAppDomain 删除域名资源（不存在 404；删除即触发入口收敛——路由
// 撤销 + 证书 SAN 集随下次签发收敛）。
func (s *DomainsService) RemoveAppDomain(ctx context.Context, req *serverv1.RemoveAppDomainRequest) (*serverv1.RemoveAppDomainResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	domain, err := compose.NormalizeDomain(req.GetDomain())
	if err != nil {
		return nil, err
	}
	current, err := s.st.GetAppDomain(ctx, app.ID, domain)
	if err != nil {
		return nil, mapStoreErr(err, domain)
	}
	if err := s.st.RemoveAppDomain(ctx, app.ID, domain); err != nil {
		return nil, mapStoreErr(err, domain)
	}
	if err := s.writeDomainAudit(ctx, "domain.removed", app, current, "ok", ""); err != nil {
		return nil, err
	}
	s.convergeAfterDomainWrite(ctx, app, domain)
	return &serverv1.RemoveAppDomainResponse{App: app.Name, Domain: domain}, nil
}

// domainInputOf 组装域名资源写入载荷：空字段的语义由 fallback 承载——
// 创建（fallback 零值）空 = 平台缺省 http/http01；更新空 = 保持现值
// （fallback = 当前行）。非空字段一律显式校验（形态违规 400 点名）。
func domainInputOf(service, port, protocol, certMode, domain string, fallback state.Domain) (state.DomainInput, error) {
	in := state.DomainInput{
		Domain:   domain,
		Service:  pickNonEmpty(service, fallback.Service),
		Port:     pickNonEmpty(port, fallback.Port),
		Protocol: pickNonEmpty(protocol, fallback.Protocol),
		CertMode: pickNonEmpty(certMode, fallback.CertMode),
	}
	if in.Protocol == "" {
		in.Protocol = "http"
	}
	if in.CertMode == "" {
		in.CertMode = "http01"
	}
	if err := validateDomainService(in.Service); err != nil {
		return state.DomainInput{}, err
	}
	if err := validateDomainPort(in.Port); err != nil {
		return state.DomainInput{}, err
	}
	switch in.Protocol {
	case "http", "h2c":
	default:
		return state.DomainInput{}, statusInvalidArgument(
			"protocol must be one of http|h2c (got " + in.Protocol + ")")
	}
	switch in.CertMode {
	case "http01", "wildcard":
	default:
		return state.DomainInput{}, statusInvalidArgument(
			"cert_mode must be one of http01|wildcard (got " + in.CertMode + ")")
	}
	return in, nil
}

// pickNonEmpty 空串取回退值（更新面的「空 = 保持现值」语义）。
func pickNonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// validateDomainService 校验服务引用形态（compose 服务名字符集
// [A-Za-z0-9._-]，首字符字母数字——与 compose 标识符同族的宽松门；服务
// 是否存在属底座事实：未运行服务 → 502 诚实暴露，不在解析层猜清单）。
func validateDomainService(service string) error {
	if service == "" {
		return statusInvalidArgument("service is required")
	}
	if len(service) > 128 {
		return statusInvalidArgument("service must be at most 128 chars")
	}
	for i, r := range service {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return statusInvalidArgument("service must match ^[A-Za-z0-9][A-Za-z0-9._-]*$")
		}
	}
	return nil
}

// validateDomainPort 校验后端端口（1..65535 的十进制数字形态）。
func validateDomainPort(port string) error {
	if port == "" {
		return statusInvalidArgument("port is required")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return statusInvalidArgument("port must be an integer within 1..65535 (got " + port + ")")
	}
	return nil
}

// domainWriteError 把 state 写面错误映射为 API 语义。哨兵行
// （ErrDomainConflict → E_DOMAIN_CONFLICT 409、ErrDomainNotFound → 404）
// 在 errors.go 哨兵登记表；DomainLimitError 是携带 count/limit 载荷的
// 类型化投影（E_DOMAIN_UNSUPPORTED 400）——登记表外的调用面特例（类型化
// 载荷无法以哨兵行表达）。
func domainWriteError(err error, domain string) error {
	var limitErr *state.DomainLimitError
	if errors.As(err, &limitErr) {
		scope := "app"
		if limitErr.Scope == "service" {
			scope = "service"
		}
		return apperr.New("E_DOMAIN_UNSUPPORTED",
			"domain limit reached (%s scope: %d domains already declared, limit %d)", scope, limitErr.Count, limitErr.Limit).
			WithContext("reason", "per_"+scope+"_limit")
	}
	return mapStoreErr(err, domain)
}

// writeDomainAudit 写域名资源审计行（动作 domain.created/updated/removed；
// target = domain:<app>/<host>，与 secret:<app>/<name> 同型）。
func (s *DomainsService) writeDomainAudit(ctx context.Context, action string, app state.App, row state.Domain, result, errorCode string) error {
	entry := domainAudit(ctx, action, app.Name, row.Domain,
		state.DiffSummary("service", row.Service, "port", row.Port, "protocol", row.Protocol, "cert_mode", row.CertMode))
	entry.Result = result
	entry.ErrorCode = errorCode
	return s.st.InTx(ctx, func(tx *state.Tx) error { return tx.WriteAudit(ctx, entry) })
}

// domainAudit 构造域名资源审计条目（actor 归因沿 databaseAudit：API 无法
// 区分人类/AI 代理，token 承载可追溯性）。
func domainAudit(ctx context.Context, action, app, domain, diff string) state.AuditEntry {
	return databaseAudit(ctx, action, "domain:"+app+"/"+domain, diff)
}

// convergeAfterDomainWrite 是写面后的入口收敛（best-effort：资源行已落库，
// 收敛失败不撤销写入——落审计 error 行 + route.publish_failed 事件供运维面
// 观察，下次部署/续期扫描恢复；mgr 为空 = 无 ingress 装配形态跳过）。
func (s *DomainsService) convergeAfterDomainWrite(ctx context.Context, app state.App, domain string) {
	if s.mgr == nil {
		return
	}
	if err := s.mgr.ConvergeAppDomains(ctx, app.ID); err != nil {
		summary := domainErrorSummary(err)
		entry := domainAudit(ctx, "route.publish", app.Name, domain, state.DiffSummary("error", summary))
		entry.Result = "error"
		entry.ErrorCode = "E_ROUTE_PUBLISH_FAILED"
		if werr := s.st.InTx(ctx, func(tx *state.Tx) error {
			if aerr := tx.WriteAudit(ctx, entry); aerr != nil {
				return aerr
			}
			_, eerr := tx.AppendEvent(ctx, state.Event{
				Name:    "route.publish_failed",
				Subject: "app:" + app.Name,
				Payload: fmt.Sprintf(`{"app":%q,"source":"domains_api","error":%q}`, app.Name, summary),
			})
			return eerr
		}); werr != nil {
			// 披露失败不回滚资源事实（与证书审计同纪律）。
			return
		}
	}
}

// domainErrorSummary 是收敛错误的事件/审计单行化（禁换行、限长——事件
// payload 脱敏契约）。
func domainErrorSummary(err error) string {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// VerifyAppDomains 本机视角域名验证（ingress.VerifyDomains；探测材料
// 如实记录，判断权在操作者）。
func (s *DomainsService) VerifyAppDomains(ctx context.Context, req *serverv1.VerifyAppDomainsRequest) (*serverv1.VerifyAppDomainsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门；层级随方法 scope 登记映射——读面 read、
	// UpdatePlacement=admin）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rows, err := s.st.ListAppDomains(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &serverv1.VerifyAppDomainsResponse{}, nil
	}
	domains := make([]string, 0, len(rows))
	for _, d := range rows {
		domains = append(domains, d.Domain)
	}
	// E4（S19）：探测端口与入口发布端口同源（Manager 配置）——非默认
	// 端口部署不再探测 80/443 假目标；无 ingress 装配的测试面回落缺省。
	httpPort, httpsPort := 80, 443
	if s.mgr != nil {
		httpPort, httpsPort = s.mgr.Config().HTTPPort, s.mgr.Config().HTTPSPort
	}
	checks := ingress.VerifyDomains(ctx, domains, httpPort, httpsPort)
	out := make([]*serverv1.DomainCheckView, 0, len(checks))
	for _, c := range checks {
		out = append(out, &serverv1.DomainCheckView{
			Domain:       c.Domain,
			Ips:          c.IPs,
			Resolved:     c.Resolved,
			Http_80:      c.HTTP80,
			Https_443:    c.HTTPS443,
			CertSubject:  c.CertSubject,
			CertDnsNames: c.CertDNSNames,
			Error:        c.Err,
		})
	}
	return &serverv1.VerifyAppDomainsResponse{Checks: out}, nil
}
