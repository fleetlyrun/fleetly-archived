package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/ingress"
	"github.com/fleetlyrun/fleetly/internal/objectstore"
	"github.com/fleetlyrun/fleetly/internal/rustfs"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/statebackup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SystemService 实现 server.v1.SystemService：进程级系统信息（Ping/Status，
// T0.3/T2.17）+ 集群级观察面（ListNodes/GetIngressStatus，T2.18）+ join
// 向导面（E1-8，multi-node §2.3）。
//
// ingress 依赖以 IngressStatusSource 端口注入（实现 = *ingress.Manager）：
// 入口探测在服务端执行——CLI 不再直连 docker / 读本地 token 文件 / 探测
// 配置端点（CLI-over-SDK 单一通道纪律）；nil 端口 = 入口面未装配（测试
// 形态），Traefik 视图如实报告不可用而非谎报。
type SystemService struct {
	serverv1.UnimplementedSystemServiceServer
	version string
	// components 是命名健康组件集（复用各服务 CheckHealth 实现——lynx
	// Checker 接口无名，命名清单在装配点显式维护）。
	components func() []SystemComponent
	st         *state.Store
	ing        IngressStatusSource
	// backup 是状态备份管理器（T2.22；nil = 备份面未装配——ListBackups/
	// GetSystemStatus 的备份视图走台账仍可用，TriggerBackup 如实报不可用）。
	backup *statebackup.Manager
	// baseDomain 是平台域名（multi-node §2.2；E1-8 join 门禁 D-MN-13 的
	// 判定面：空 = 多节点未启用）。
	baseDomain string
	// join 是 swarm join-token 面端口（实现 = *substrate.Client；nil =
	// 未装配——GetJoinGuide 在 base_domain 缺失时先以 409 拒绝、不触底座，
	// RotateJoinToken 如实报不可用）。
	join JoinTokenPort
	// box 是 envelope 加解密器（E3-2 S3 设置面：secret 密文落库/读面解密
	// 出指纹/探针解密已存凭证；nil = 未装配——S3 三面如实报不可用）。
	box *secrets.Box
	// rustfs 是托管 RustFS 收敛管理器（E3-5：TestConnection 的 rustfs
	// 分支经它解析派生端点与托管凭据；nil = 未装配——rustfs 探针如实报
	// 不可用）。
	rustfs *rustfs.Manager
	// dnsProviderFactory 是 DNS-01 插件构造缝（W5-S3，acme.go；nil = 回落
	// acmedns.New 官方端点形态——测试注入 httptest 假端点用）。
	dnsProviderFactory DNSProviderFactory
}

// JoinTokenPort 是 swarm join-token 面端口（multi-node §2.3；*substrate.
// Client 隐式实现——端口在 api 定义、适配在 substrate，方向纪律同
// IngressStatusSource）。
type JoinTokenPort interface {
	// SwarmJoinInfo 返回 manager advertise addr 与 worker join token。
	SwarmJoinInfo(ctx context.Context) (string, string, error)
	// SwarmRotateJoinToken 轮换指定角色（worker|manager）的 join token
	// 并返回新 token（rotate 后旧 token 立即失效）。
	SwarmRotateJoinToken(ctx context.Context, role string) (string, error)
}

// SystemComponent 是带名的健康组件（CheckHealth 复用面）。
type SystemComponent struct {
	Name  string
	Check func() error
}

// IngressStatusSource 是入口状态消费端口（*ingress.Manager 隐式实现）。
type IngressStatusSource interface {
	Status(ctx context.Context) (ingress.Status, error)
	Config() ingress.Config
}

// NewSystemService 构造 SystemService（version 由构建 -ldflags 注入；
// components 为装配点命名的健康组件集；ing 可为 nil——入口面未装配形态；
// rustfsMgr 可为 nil——托管 RustFS 面未装配形态）。
func NewSystemService(version string, st *state.Store, components func() []SystemComponent, ing IngressStatusSource, rustfsMgr *rustfs.Manager) *SystemService {
	return &SystemService{version: version, st: st, components: components, ing: ing, rustfs: rustfsMgr}
}

// WithJoinGuide 注入 join 向导面（E1-8；链式装配）。baseDomain 为空 =
// 单节点形态（GetJoinGuide 以 E_MULTI_NODE_REQUIRES_BASE_DOMAIN 409 拒绝
// ——D-MN-13）；jp 可为 nil（join 底座面未装配）。
func (s *SystemService) WithJoinGuide(baseDomain string, jp JoinTokenPort) *SystemService {
	s.baseDomain = baseDomain
	s.join = jp
	return s
}

// WithBackupManager 注入状态备份管理器（T2.22；链式装配，nil 合法——
// 测试/精简形态的备份面缺省）。
func (s *SystemService) WithBackupManager(m *statebackup.Manager) *SystemService {
	s.backup = m
	return s
}

// WithSecretsBox 注入 envelope 加解密器（E3-2 S3 设置面；链式装配，nil
// 合法——S3 三面如实报不可用）。secret 明文只在服务端内存存活：写入即
// envelope 加密落库，读面只出指纹，探针解密已存凭证在服务端完成。
func (s *SystemService) WithSecretsBox(box *secrets.Box) *SystemService {
	s.box = box
	return s
}

// requirePlatformWriteFace 是平台面写/敏感面的用户凭据门（v0.3 W2-S4 收口，
// rbac-teams §4.2 第 3 条 + §3.2 矩阵「用户管理/注册开关/节点/S3/通知/全局
// 设置 → 仅平台管理员」）：机具令牌（UserID 空）沿 scope 门现状放行（平台
// 级凭据 §2.3 设计语义）；用户 principal 要求 is_platform_admin（403——写
// 操作需入队拿角色，平台管理员身份不代平台外写）。透明度例外（GetSystemStatus/
// ListNodes/GetIngressStatus 任意已认证 read）不经本门。
func requirePlatformWriteFace(ctx context.Context, st *state.Store) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	if p.UserID == "" {
		return nil // 机具令牌：scope 门已判定（admin/deploy 级登记不变）
	}
	if !isPlatformAdminUser(ctx, st) {
		return statusEnvelope(codes.PermissionDenied,
			"platform administrator privileges required for this platform face (users keep it read-only; join flows and node/backup administration need a platform admin or a machine token)")
	}
	return nil
}

// Ping 回应 service / version。proto 字段上的 buf.validate 最小约束由拦截
// 器统一校验；Ping 豁免鉴权（T2.17 契约：与 healthz 同为存活面）。
func (s *SystemService) Ping(ctx context.Context, req *serverv1.PingRequest) (*serverv1.PingResponse, error) {
	return &serverv1.PingResponse{Service: "fleetlyd", Version: s.version}, nil
}

// GetSystemStatus 健康汇总（引擎/Traefik/状态层/备份——复用 CheckHealth 面）：
// 逐组件如实上报，不聚合单一布尔，判断权在消费方。备份明细（最近一次
// 台账行的时间与 verify_status）随 backup 字段带出——组件布尔之外让
// 「备份上次何时成功」直接可见（T2.22 状态诚实契约）。
func (s *SystemService) GetSystemStatus(ctx context.Context, req *serverv1.GetSystemStatusRequest) (*serverv1.GetSystemStatusResponse, error) {
	resp := &serverv1.GetSystemStatusResponse{Service: "fleetlyd", Version: s.version}
	for _, c := range s.components() {
		ch := &serverv1.ComponentHealth{Name: c.Name}
		if err := c.Check(); err != nil {
			ch.Ok = false
			ch.Error = err.Error()
		} else {
			ch.Ok = true
		}
		resp.Components = append(resp.Components, ch)
	}
	if latest, err := s.st.LatestStateBackup(ctx); err == nil && latest != nil {
		resp.Backup = backupHealth(latest)
	}
	return resp, nil
}

// backupHealth 把最近一次台账行投影为备份健康视图。
func backupHealth(latest *state.StateBackup) *serverv1.BackupHealth {
	return &serverv1.BackupHealth{
		LastBackupId:     latest.ID,
		LastKind:         latest.Kind,
		LastBackupAt:     tstamp(latest.CreatedAt),
		LastVerifyStatus: latest.VerifyStatus,
		LastError:        latest.Error,
	}
}

// ListBackups 状态备份台账只读列表（T2.22；n 缺省 50——台账量级受保留
// 份数约束，50 已覆盖全部现行行 + 近期失败行）。
func (s *SystemService) ListBackups(ctx context.Context, req *serverv1.ListBackupsRequest) (*serverv1.ListBackupsResponse, error) {
	rows, err := s.st.ListStateBackups(ctx, 50)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.BackupView, 0, len(rows))
	for _, r := range rows {
		out = append(out, backupView(r))
	}
	return &serverv1.ListBackupsResponse{Backups: out}, nil
}

// TriggerBackup 手动触发一次状态备份（同步：响应即落账后的台账行；
// verify_status=failed 时以 FailedPrecondition 返回且台账行保留失败事实
// ——调用方看得见失败，绝不渲染成成功）。备份本体与审计运行在脱钩
// 客户端取消的 ctx 上（WithoutCancel——备份预算由 Manager 自身的
// TriggerTimeout 守门）：慢后端（rustfs 首传含 4 段 restic 容器执行）下
// CLI 缺省 30s deadline 不再掐死备份本体——客户端只失去本次同步响应，
// 台账照常落账（ListBackups 复核）。
func (s *SystemService) TriggerBackup(ctx context.Context, req *serverv1.TriggerBackupRequest) (*serverv1.TriggerBackupResponse, error) {
	// 平台面写门（W2-S4）：用户 principal 须平台管理员（机具令牌沿 scope 门）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.backup == nil {
		return nil, status.Error(codes.Unavailable, "backup manager unavailable (not assembled)")
	}
	kind := req.GetKind()
	if kind == "" {
		kind = state.BackupKindManual
	}
	detached := context.WithoutCancel(ctx)
	rec, err := s.backup.Trigger(detached, kind)
	if err != nil {
		// 失败行已落台账（backup.failed 审计随行）——错误原文回传，调用方
		// 可经 ListBackups 复核失败事实。
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err := s.st.InTx(detached, func(tx *state.Tx) error {
		return tx.WriteAudit(detached, auditEntry(detached, "backup:"+rec.ID,
			state.DiffSummary("kind", rec.Kind, "verify", rec.VerifyStatus))) // MG-6：构造器替换手拼 JSON
	}); err != nil {
		return nil, err
	}
	return &serverv1.TriggerBackupResponse{Backup: backupView(rec)}, nil
}

// backupView 把台账行投影为只读视图（E3-3：上传结论三面随行——
// upload_status/uploaded_at/upload_error；uploaded_at 零值不输出）。
func backupView(r state.StateBackup) *serverv1.BackupView {
	v := &serverv1.BackupView{
		Id:           r.ID,
		Kind:         r.Kind,
		Path:         r.Path,
		Sha256:       r.SHA256,
		SizeBytes:    r.SizeBytes,
		VerifyStatus: r.VerifyStatus,
		Error:        r.Error,
		CreatedAt:    tstamp(r.CreatedAt),
		UploadStatus: r.UploadStatus,
		UploadError:  r.UploadError,
	}
	if !r.UploadedAt.IsZero() {
		v.UploadedAt = tstamp(r.UploadedAt)
	}
	return v
}

// ListNodes 节点观测缓存只读列表（state-model §2.2：缓存禁止用于决策，
// 展示/诊断专用；节点变更用 docker node 原生命令）。NodeView 增补
// pinned_app_ids（E1-8，multi-node §2.7/D-MN-9：读时 join placements
// 权威表，UI「已钉应用」交叉引用——无迁移）。
func (s *SystemService) ListNodes(ctx context.Context, req *serverv1.ListNodesRequest) (*serverv1.ListNodesResponse, error) {
	nodes, err := s.st.ListCachedNodes(ctx)
	if err != nil {
		return nil, err
	}
	pinned, err := s.st.PlacementAppsByNode(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.NodeView, 0, len(nodes))
	for _, n := range nodes {
		v := &serverv1.NodeView{
			SwarmNodeId:  n.SwarmNodeID,
			Hostname:     n.Hostname,
			State:        n.State,
			Availability: n.Availability,
			IsManager:    n.IsManager,
			ObservedAt:   tstamp(n.ObservedAt),
			Stale:        n.Stale,
			Labels:       n.Labels,
			PinnedAppIds: []string{},
		}
		if id := n.Labels[state.LabelNodeID]; id != "" {
			v.PlatformId = id
			v.PinnedAppIds = pinned[id]
		}
		out = append(out, v)
	}
	return &serverv1.ListNodesResponse{Nodes: out}, nil
}

// GetJoinGuide join 向导（E1-8，multi-node §2.3）：join 命令 + 按 worker_ip
// 的精确放行规则（只生成不自动应用）+ worker 前置门禁命令 + DNS 步骤 +
// 完成判据。前哨：base_domain 为空 → E_MULTI_NODE_REQUIRES_BASE_DOMAIN
// （409，D-MN-13——多节点未启用显式拒绝，不静默降级）。admin scope（响应
// 含 token 材料）由拦截器链把门。
func (s *SystemService) GetJoinGuide(ctx context.Context, req *serverv1.GetJoinGuideRequest) (*serverv1.GetJoinGuideResponse, error) {
	// 平台面敏感读门（W2-S4）：响应含 join token 材料 = 平台管理员。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.baseDomain == "" {
		// D-MN-13：配置缺失显式拒绝——provider 通道/zot 均不可用，join 后
		// 入口残缺；隐式猜测即事故面。
		return nil, apperr.New("E_MULTI_NODE_REQUIRES_BASE_DOMAIN",
			"multi-node is not enabled: base_domain is not configured (the config endpoint, platform subdomains and the registry all derive from it)")
	}
	if s.join == nil {
		return nil, status.Error(codes.Unavailable, "swarm join face unavailable (not assembled)")
	}
	addr, workerToken, err := s.join.SwarmJoinInfo(ctx)
	if err != nil {
		if errors.Is(err, state.ErrNotSwarmManager) {
			return nil, status.Error(codes.FailedPrecondition,
				"swarm mode not active: join guide requires an initialized swarm (run docker swarm init on the manager)")
		}
		return nil, err
	}
	if req.GetManagerAddr() != "" {
		// 跨公网场景覆盖（advertise 为私网时，安装报告已警示的暴露面口径）。
		addr = req.GetManagerAddr()
	}
	workerIP := req.GetWorkerIp()
	if workerIP == "" {
		workerIP = "<worker-ip>"
	}
	managerIP := hostOfAddr(addr)
	if managerIP == "" {
		managerIP = "<manager-ip>"
	}
	return &serverv1.GetJoinGuideResponse{Guide: buildJoinGuide(s.baseDomain, addr, workerToken, workerIP, managerIP)}, nil
}

// buildJoinGuide 生成 JoinGuideView（规则/步骤文本为英文文案纪律；平台
// 只生成规则文本、不自动应用——--harden-firewall 自动应用维持 reserved）。
func buildJoinGuide(baseDomain, addr, workerToken, workerIP, managerIP string) *serverv1.JoinGuideView {
	g := &serverv1.JoinGuideView{
		JoinCommand: "docker swarm join --token " + workerToken + " " + addr + ":2377",
		ManagerAddr: addr,
		WorkerToken: workerToken,
		BaseDomain:  baseDomain,
		ManagerFirewallRules: []*serverv1.FirewallRule{
			{Direction: "worker_to_manager", Port: "2377/tcp", Purpose: "cluster management (swarm join)",
				Side: "manager", Rule: "iptables -A INPUT -p tcp -s " + workerIP + " --dport 2377 -j ACCEPT"},
			{Direction: "worker_to_manager", Port: "7946/tcp", Purpose: "gossip",
				Side: "manager", Rule: "iptables -A INPUT -p tcp -s " + workerIP + " --dport 7946 -j ACCEPT"},
			{Direction: "worker_to_manager", Port: "7946/udp", Purpose: "gossip",
				Side: "manager", Rule: "iptables -A INPUT -p udp -s " + workerIP + " --dport 7946 -j ACCEPT"},
			{Direction: "bidirectional", Port: "4789/udp", Purpose: "overlay VXLAN",
				Side: "manager", Rule: "iptables -A INPUT -p udp -s " + workerIP + " --dport 4789 -j ACCEPT"},
			{Direction: "worker_to_manager", Port: "8423/tcp", Purpose: "Traefik config endpoint TLS face (token-authenticated)",
				Side: "manager", Rule: "iptables -A INPUT -p tcp -s " + workerIP + " --dport 8423 -j ACCEPT"},
			{Direction: "public_to_all", Port: "80,443/tcp", Purpose: "app ingress (Traefik host ports)",
				Side: "manager", Rule: "existing baseline: already open on every node — no change"},
		},
		WorkerFirewallRules: []*serverv1.FirewallRule{
			{Direction: "bidirectional", Port: "7946/tcp", Purpose: "gossip",
				Side: "worker", Rule: "iptables -A INPUT -p tcp -s " + managerIP + " --dport 7946 -j ACCEPT"},
			{Direction: "bidirectional", Port: "7946/udp", Purpose: "gossip",
				Side: "worker", Rule: "iptables -A INPUT -p udp -s " + managerIP + " --dport 7946 -j ACCEPT"},
			{Direction: "bidirectional", Port: "4789/udp", Purpose: "overlay VXLAN",
				Side: "worker", Rule: "iptables -A INPUT -p udp -s " + managerIP + " --dport 4789 -j ACCEPT"},
			{Direction: "public_to_all", Port: "80,443/tcp", Purpose: "app ingress (Traefik host ports)",
				Side: "worker", Rule: "existing baseline: already open on every node — no change"},
		},
		WorkerPreflightCommands: []string{
			"docker version --format '{{.Server.Version}}'   # must be >= 29.8.1",
			"iptables --version   # legacy iptables required (nftables-only hosts are not supported by the installer gate)",
		},
		DnsSteps: []string{
			"Add A records for the application domains and the platform subdomains to include the worker IP " + workerIP + " (TTL <= 300s): registry." + baseDomain + ", console." + baseDomain + ", and every app domain.",
			"ctrl." + baseDomain + " keeps pointing at the manager only — do NOT add the worker IP to it.",
			"Run fleetly domains verify after DNS propagation.",
		},
		CompletionChecks: []string{
			"The observation beat lists the new node: fleetly nodes list",
			"Anchoring completes automatically: node.joined event with a non-empty platform_id",
			"The node reports state=ready availability=active",
			"The Traefik (fleetly-ingress) task is running on the node",
			"Once anchoring completes, the worker join token rotates automatically (join.token_rotate=auto, the default) — the token you joined with stops working; in manual mode rotate it yourself with fleetly nodes rotate-token after all nodes have joined",
		},
	}
	return g
}

// hostOfAddr 取地址的 host 段（manager-addr 覆盖值可能是 host:port 形态；
// 无 port 段原样返回）。
func hostOfAddr(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// RotateJoinToken 轮换 swarm join token（E1-8，D-MN-1）：role 缺省 worker；
// rotate 后旧 token 立即失效。审计动作 node.join_token_rotated（§5.3：
// join-token rotate 记审计、不设事件）。admin scope 由拦截器链把门。
func (s *SystemService) RotateJoinToken(ctx context.Context, req *serverv1.RotateJoinTokenRequest) (*serverv1.RotateJoinTokenResponse, error) {
	// 平台面写门（W2-S4）：join token 轮换 = 平台管理员（机具令牌沿 scope 门）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.join == nil {
		return nil, status.Error(codes.Unavailable, "swarm join face unavailable (not assembled)")
	}
	role := req.GetRole()
	if role == "" {
		role = "worker"
	}
	token, err := s.join.SwarmRotateJoinToken(ctx, role)
	if err != nil {
		if errors.Is(err, state.ErrNotSwarmManager) {
			return nil, status.Error(codes.FailedPrecondition,
				"swarm mode not active: token rotation requires an initialized swarm")
		}
		return nil, err
	}
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		actorTokenID := ""
		if p, ok := PrincipalFromContext(ctx); ok {
			actorTokenID = p.TokenID
		}
		// 审计动作 = §5.3 登记的 node.join_token_rotated（join-token rotate
		// 记审计、不设事件；auditEntry 的默认 action 是 api.<Service>.<Method>
		// ——此处显式覆盖为产品语义词根）。
		return tx.WriteAudit(ctx, state.AuditEntry{
			Actor:        "human",
			ActorTokenID: actorTokenID,
			Action:       "node.join_token_rotated",
			Target:       "node:swarm",
			Result:       "ok",
			DiffSummary:  state.DiffSummary("role", role),
		})
	}); err != nil {
		return nil, err
	}
	return &serverv1.RotateJoinTokenResponse{Role: role, Token: token}, nil
}

// GetIngressStatus 入口链三面状态（T2.18）：① Traefik 服务实况（Swarm
// inspect，不可达如实标注）；② 配置端点两面探测（/healthz 无鉴权 +
// /configs 带 token 鉴权核验——环回执行，401 = token 缺失/错误，如实报告）；
// ③ 证书台账（domains 表 cert 列）与证书存储目录对照。
func (s *SystemService) GetIngressStatus(ctx context.Context, req *serverv1.GetIngressStatusRequest) (*serverv1.GetIngressStatusResponse, error) {
	resp := &serverv1.GetIngressStatusResponse{
		Certificates: []*serverv1.CertLedgerView{},
		CertDirApps:  []string{},
		Healthz:      "unreachable",
		Auth:         "unreachable",
	}
	if s.ing == nil {
		// 入口面未装配（测试/精简形态）：如实报告不可用，不谎报健康。
		resp.Traefik = &serverv1.TraefikView{Error: "ingress manager unavailable"}
		return resp, nil
	}
	cfg := s.ing.Config()
	resp.ConfigAddr = cfg.ConfigAddr

	// ① Traefik 服务实况（底座不可达 → error 原文）。
	status, err := s.ing.Status(ctx)
	resp.AdvertiseIp = status.AdvertiseIP
	resp.Responder = status.Responder
	if err != nil {
		resp.Traefik = &serverv1.TraefikView{Error: err.Error()}
	} else {
		resp.Traefik = &serverv1.TraefikView{
			Exists: status.Traefik.Exists,
			Image:  status.Traefik.Image,
			// 静态参数条数（len 收窄 int32——服务实况的计数永不大）。
			StaticArgs: int32(len(status.Traefik.Args)), //nolint:gosec // G115：参数条数计数
		}
	}

	// ② 配置端点环回探测（3s 预算；token 读自配置文件——服务端本机语义）。
	s.probeConfigEndpoint(ctx, cfg, resp)

	// ③ 证书台账对照 + 证书目录 app 清单（目录缺失 = 空清单非错误）。
	s.certLedger(ctx, resp)
	apps, dirErr := listCertDirApps(cfg.CertDir)
	resp.CertDir = cfg.CertDir
	resp.CertDirApps = apps
	if dirErr != nil {
		resp.CertDirError = dirErr.Error()
	}
	return resp, nil
}

// probeConfigEndpoint 环回探测配置端点两面（与旧 CLI 探测同口径：健康面
// 无鉴权；/configs 带 token 鉴权核验并校验 JSON 可解析）。
func (s *SystemService) probeConfigEndpoint(ctx context.Context, cfg ingress.Config, resp *serverv1.GetIngressStatusResponse) {
	base := "http://" + loopbackAddr(cfg.ConfigAddr)
	client := &http.Client{Timeout: 3 * time.Second}
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil); err == nil {
		if respd, err := client.Do(req); err == nil {
			_ = respd.Body.Close()
			resp.Healthz = respd.Status
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/configs", nil)
	if err != nil {
		return
	}
	if tokenRaw, tokErr := os.ReadFile(cfg.TokenFile); tokErr == nil && strings.TrimSpace(string(tokenRaw)) != "" { //nolint:gosec // G304：token 文件路径来自服务端 ingress 配置
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenRaw)))
	} else {
		resp.Auth = "token file unreadable: " + cfg.TokenFile
	}
	respd, err := client.Do(req)
	if err != nil {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(respd.Body, 4096))
	_ = respd.Body.Close()
	switch respd.StatusCode {
	case http.StatusOK:
		var probe map[string]any
		if json.Unmarshal(body, &probe) == nil {
			resp.Auth = "200 (authorized)"
		} else {
			resp.Auth = "200 (non-JSON body?)"
		}
	default:
		resp.Auth = respd.Status
	}
}

// certLedger 构造证书台账投影（app 为显示名；已删除应用回退显示 ID）。
func (s *SystemService) certLedger(ctx context.Context, resp *serverv1.GetIngressStatusResponse) {
	rows, err := s.st.ListAllDomains(ctx)
	if err != nil {
		return
	}
	nameByAppID := map[string]string{}
	for _, r := range rows {
		if r.CertSHA256 == "" {
			continue
		}
		if _, ok := nameByAppID[r.AppID]; !ok {
			name := r.AppID
			if appRow, err := s.st.GetAppByID(ctx, r.AppID); err == nil {
				name = appRow.Name
			}
			nameByAppID[r.AppID] = name
		}
		resp.Certificates = append(resp.Certificates, &serverv1.CertLedgerView{
			App:          nameByAppID[r.AppID],
			Domain:       r.Domain,
			CertSha256:   r.CertSHA256,
			CertNotAfter: tstamp(r.CertNotAfter),
		})
	}
}

// loopbackAddr 把监听地址的 host 段替换为 127.0.0.1（配置端点绑 0.0.0.0
// ——环回探测用回环地址；无 port 段时原样返回，探测按不可达处理）。
func loopbackAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return addr
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// listCertDirApps 读证书目录 app 清单（meta 索引；目录缺失 = 空清单非错误
// ——与证书目录的生命周期语义一致：未部署入口 = 无目录）。
func listCertDirApps(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("read cert dir %s: %w", dir, err)
	}
	apps := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".meta.json") {
			apps = append(apps, strings.TrimSuffix(e.Name(), ".meta.json"))
		}
	}
	return apps, nil
}

// ── 对象存储 S3 设置面（E3 对象存储 §5.1/E3-2，admin scope）──────────────
// 设置落库内运行期设置（platform_settings，D-S3-2）；secret 明文只写不读
//（读面 fingerprint），持久层 envelope 加密；探针语义 = 能认证/能写/能读
// 回（§2.1 诚实契约）。

// rustfsEndpointURL / rustfsBucketName 已收口为 state 包常量
//（state.RustfsEndpointURL / state.RustfsBucketName——备份上传轨 E3-3
// 与探针共用，单一事实源在 internal/state）。

// GetS3Settings 对象存储设置只读面：secret 只回 fingerprint（明文 sha256
// 前 8），绝不回明文。s3.mode=unset 时其余字段为空。
func (s *SystemService) GetS3Settings(ctx context.Context, req *serverv1.GetS3SettingsRequest) (*serverv1.GetS3SettingsResponse, error) {
	// 平台面敏感读门（W2-S4）：S3 设置 = 平台凭据面（§3.2 矩阵）。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.box == nil {
		return nil, status.Error(codes.Unavailable, "secrets box unavailable (not assembled)")
	}
	in, err := s.st.LoadS3Settings(ctx)
	if err != nil {
		return nil, err
	}
	view, err := s.s3SettingsView(in)
	if err != nil {
		return nil, err
	}
	return &serverv1.GetS3SettingsResponse{Settings: view}, nil
}

// UpdateS3Settings 全量保存（PUT 语义：请求即新状态）。secret_access_key
// 明文入站（TLS 传输面）→ envelope 加密落库；互斥校验 fail-fast 在 state
// 层（E_S3_CONFIG_CONFLICT / E_S3_PUBLIC_REQUIRES_BASE_DOMAIN，信封原样
// 透传）；保存 + 审计 + 事件 s3.updated 同事务（payload 带模式不带走秘密）。
func (s *SystemService) UpdateS3Settings(ctx context.Context, req *serverv1.UpdateS3SettingsRequest) (*serverv1.UpdateS3SettingsResponse, error) {
	// 平台面写门（W2-S4）：S3 设置保存 = 平台管理员。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.box == nil {
		return nil, status.Error(codes.Unavailable, "secrets box unavailable (not assembled)")
	}
	secretCT := ""
	if req.GetSecretAccessKey() != "" {
		ct, err := s.box.Encrypt([]byte(req.GetSecretAccessKey()))
		if err != nil {
			return nil, err
		}
		secretCT = string(ct)
	}
	in := state.S3Settings{
		Mode:            state.NormalizeMode(req.GetMode()),
		EndpointURL:     strings.TrimSpace(req.GetEndpointUrl()),
		Region:          strings.TrimSpace(req.GetRegion()),
		Bucket:          strings.TrimSpace(req.GetBucket()),
		AccessKeyID:     strings.TrimSpace(req.GetAccessKeyId()),
		SecretAccessKey: secretCT,
		PathStyle:       req.GetPathStyle(),
		PublicExposed:   req.GetPublicExposed(),
	}
	opts := state.S3SaveOptions{BaseDomain: s.baseDomain, Actor: "human"}
	if p, ok := PrincipalFromContext(ctx); ok {
		opts.ActorTokenID = p.TokenID
	}
	if err := s.st.SaveS3Settings(ctx, in, opts); err != nil {
		return nil, err
	}
	view, err := s.s3SettingsView(in)
	if err != nil {
		return nil, err
	}
	return &serverv1.UpdateS3SettingsResponse{Settings: view}, nil
}

// TestS3Connection S3 连接探针：候选配置（未保存也能测）或已存配置
// （全部候选字段为空时）。探针失败以 E_S3_TEST_FAILED 报错——失败步与
// 底层错误摘要进信封 context（secret 材料不进错误文本：minio-go 错误不
// 回显凭证，信封 context 只带 endpoint/bucket/失败步）。rustfs 已存配置
// 的探针 = 探测容器形态（E3-5：一次性 restic 容器 attach 托管网络执行
// 真实往返——宿主进程不可达 overlay，容器内 DNS 才可达服务）。
func (s *SystemService) TestS3Connection(ctx context.Context, req *serverv1.TestS3ConnectionRequest) (*serverv1.TestS3ConnectionResponse, error) {
	// 平台面写门（W2-S4）：连接探针消耗平台凭据 = admin 级信任面。
	if err := requirePlatformWriteFace(ctx, s.st); err != nil {
		return nil, err
	}
	if s.box == nil {
		return nil, status.Error(codes.Unavailable, "secrets box unavailable (not assembled)")
	}
	candidate := objectstore.Endpoint{
		URL:       strings.TrimSpace(req.GetEndpointUrl()),
		Region:    strings.TrimSpace(req.GetRegion()),
		Bucket:    strings.TrimSpace(req.GetBucket()),
		AccessKey: strings.TrimSpace(req.GetAccessKeyId()),
		SecretKey: req.GetSecretAccessKey(),
		PathStyle: req.GetPathStyle(),
	}
	ep := candidate
	if candidate.URL == "" && candidate.Region == "" && candidate.Bucket == "" &&
		candidate.AccessKey == "" && candidate.SecretKey == "" && !candidate.PathStyle {
		// 无候选配置 → 测已存配置（每次现读，不缓存长驻）。
		in, err := s.st.LoadS3Settings(ctx)
		if err != nil {
			return nil, err
		}
		if state.NormalizeMode(in.Mode) == state.S3ModeRustfs {
			// 托管面探针：探测容器内执行（E3-5）。
			return s.runRustfsProbe(ctx)
		}
		stored, found, err := s.storedS3Endpoint(ctx)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, statusInvalidArgument(
				"no S3 settings saved yet (s3.mode=unset): pass a candidate configuration to test")
		}
		ep = stored
	}
	res := objectstore.TestConnection(ctx, ep)
	if !res.OK {
		lastErr := ""
		for _, st := range res.Steps {
			if !st.OK {
				lastErr = st.Err
			}
		}
		return nil, apperr.New("E_S3_TEST_FAILED",
			"s3 connection test failed at step %s: %s", res.FailedStep, lastErr).
			WithContext("endpoint", res.EndpointURL).
			WithContext("bucket", res.Bucket).
			WithContext("failed_step", res.FailedStep)
	}
	return &serverv1.TestS3ConnectionResponse{Result: s3ProbeView(res)}, nil
}

// runRustfsProbe 执行托管 RustFS 探针（E3-5）：一次性 restic 容器 attach
// fleetly-rustfs-net，经托管凭据做认证/写/读回/删除四步真实往返。探针
// 失败以 E_S3_TEST_FAILED 报错（失败步 + 擦除凭据后的错误摘要进 context
// ——诚实契约与 in-process 探针同构）。
func (s *SystemService) runRustfsProbe(ctx context.Context) (*serverv1.TestS3ConnectionResponse, error) {
	if s.rustfs == nil {
		return nil, status.Error(codes.Unavailable,
			"managed rustfs face not assembled (probe unavailable in this build)")
	}
	pr, err := s.rustfs.RunProbe(ctx)
	if err != nil {
		return nil, err
	}
	res := &serverv1.S3ConnectionTestResult{
		Ok:          pr.OK,
		EndpointUrl: state.RustfsEndpointURL,
		Bucket:      state.RustfsBucketName,
		PathStyle:   true,
		FailedStep:  pr.FailedStep,
		Steps:       make([]*serverv1.S3ProbeStep, 0, len(pr.Steps)),
	}
	for _, st := range pr.Steps {
		res.Steps = append(res.Steps, &serverv1.S3ProbeStep{
			Step:       st.Step,
			Ok:         st.OK,
			DurationMs: st.Duration.Milliseconds(),
			Error:      st.Err,
		})
	}
	if !pr.OK {
		lastErr := ""
		for _, st := range pr.Steps {
			if !st.OK {
				lastErr = st.Err
			}
		}
		return nil, apperr.New("E_S3_TEST_FAILED",
			"s3 connection test failed at step %s: %s", pr.FailedStep, lastErr).
			WithContext("endpoint", state.RustfsEndpointURL).
			WithContext("bucket", state.RustfsBucketName).
			WithContext("failed_step", pr.FailedStep)
	}
	return &serverv1.TestS3ConnectionResponse{Result: res}, nil
}

// storedS3Endpoint 把已存设置解析为探针端点（found=false = unset 无可测
// 配置）。external：设置值直出（secret 解密）。rustfs 不经本函数——托管
// 面探针由 runRustfsProbe 以探测容器执行（internal/rustfs.RunProbe，
// E3-5；宿主进程不可达 overlay，容器内才可达服务）。
func (s *SystemService) storedS3Endpoint(ctx context.Context) (objectstore.Endpoint, bool, error) {
	in, err := s.st.LoadS3Settings(ctx)
	if err != nil {
		return objectstore.Endpoint{}, false, err
	}
	switch in.Mode {
	case state.S3ModeExternal:
		ep := objectstore.Endpoint{
			URL:       in.EndpointURL,
			Region:    in.Region,
			Bucket:    in.Bucket,
			AccessKey: in.AccessKeyID,
			PathStyle: in.PathStyle,
		}
		if in.SecretAccessKey != "" {
			plain, err := s.box.Decrypt([]byte(in.SecretAccessKey))
			if err != nil {
				return objectstore.Endpoint{}, false, fmt.Errorf("decrypt stored s3 secret: %w", err)
			}
			ep.SecretKey = string(plain)
		}
		return ep, true, nil
	default:
		return objectstore.Endpoint{}, false, nil
	}
}

// s3SettingsView 把设置构造为脱敏读面投影（secret 解密出指纹，明文不出
// 服务端边界）。公网域名实值随读面下发（v0.2.x 收尾票）：开关开且
// base_domain 非空时派生 s3.<base>——CLI/Console 渲染真实域名而非字面
// 形态；其余形态留空（无公网面可指）。
func (s *SystemService) s3SettingsView(in state.S3Settings) (*serverv1.S3SettingsView, error) {
	v := &serverv1.S3SettingsView{
		Mode:          in.Mode,
		EndpointUrl:   in.EndpointURL,
		Region:        in.Region,
		Bucket:        in.Bucket,
		AccessKeyId:   in.AccessKeyID,
		PathStyle:     in.PathStyle,
		PublicExposed: in.PublicExposed,
		UpdatedAt:     tstamp(in.UpdatedAt),
	}
	if in.PublicExposed && s.baseDomain != "" {
		v.PublicDomain = "s3." + s.baseDomain
	}
	if in.SecretAccessKey != "" {
		plain, err := s.box.Decrypt([]byte(in.SecretAccessKey))
		if err != nil {
			return nil, fmt.Errorf("decrypt stored s3 secret: %w", err)
		}
		v.SecretFingerprint = secretFingerprint(plain)
	}
	return v, nil
}

// s3ProbeView 把探针结果投影为契约面。
func s3ProbeView(res objectstore.ProbeResult) *serverv1.S3ConnectionTestResult {
	out := &serverv1.S3ConnectionTestResult{
		Ok:          res.OK,
		EndpointUrl: res.EndpointURL,
		Region:      res.Region,
		Bucket:      res.Bucket,
		PathStyle:   res.PathStyle,
		FailedStep:  res.FailedStep,
		Steps:       make([]*serverv1.S3ProbeStep, 0, len(res.Steps)),
	}
	for _, st := range res.Steps {
		out.Steps = append(out.Steps, &serverv1.S3ProbeStep{
			Step:       st.Step,
			Ok:         st.OK,
			DurationMs: st.Duration.Milliseconds(),
			Error:      st.Err,
		})
	}
	return out
}

// secretFingerprint 是 secret 的展示指纹（明文 sha256 前 8 hex——与 swarm
// secret 引用 hash8 同口径；只判「是不是那个 secret」，不回传材料）。
func secretFingerprint(plain []byte) string {
	sum := sha256.Sum256(plain)
	return hex.EncodeToString(sum[:8])
}
