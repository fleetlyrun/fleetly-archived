// Package api 是 fleetly 控制面 v0.1 API 服务实现与拦截器链（T2.17；
// 架构 D21：proto 唯一真源，鉴权/限流在 gRPC 拦截器链）。
//
// 鉴权模型（architecture §4.2 安全基线）：Authorization: Bearer <token> →
// tokens 表哈希比对（常量时间二次校验）→ scope 判定（read ⊂ deploy ⊂
// admin，方法级映射在本包 scope.go）——**无 token / 错 token 全部 401**，
// scope 不足 403。错误码取舍：401/403 无专用稳定码，用信封退化形态
// （code 空 + grpc code 机械映射 HTTP），与 FZ-2 口径一致（本阶段
// internal/errcode 不在允许改动清单，不加码）。
package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// scope 词表（state-model §2.9 / architecture §4.2：read/deploy/admin；
// E7 W5-S6 增补 terminal——Web 终端独立 scope，词表见 scope.go 消费面）。
const (
	ScopeRead   = "read"
	ScopeDeploy = "deploy"
	ScopeAdmin  = "admin"
	// ScopeTerminal 是 Web 终端的独立 scope（E7 设计 §2.4：默认仅 admin
	// ——read/deploy **不**蕴含 terminal，独立 token 需显式 --scopes
	// terminal；admin 蕴含一切——下方 containsScope 的 admin 分支天然覆盖）。
	ScopeTerminal = "terminal"
	// ScopeTasks 是程序化动态工作负载面的独立 scope（T 线 DT-5 / IMPL-T2-1：
	// 默认仅 admin——read/deploy 不蕴含 tasks，独立 token 需显式 --scopes
	// tasks；admin 蕴含一切。机具令牌为典型持有者；任务写/读同门（跨令牌
	// 隔离在 handler 内以 owner_token_id 收口，与 scope 门正交））。
	ScopeTasks = "tasks"
	// ScopeBuild 是上传构建面的独立 scope（T 线 DT-6 / IMPL-T2-2：默认仅
	// admin——read/deploy 不蕴含 build，独立 token 需显式 --scopes build；
	// admin 蕴含一切。机具令牌为典型持有者（torchwood dispatcher 的构建
	// 通道）；与 TriggerBuild 的 admin 门差异见 scope.go 登记注释）。
	ScopeBuild = "build"
)

// containsScope 报告 scope 集（逗号分隔存储形态）是否蕴含所需 scope
//（admin ⊃ deploy ⊃ read ⊕ terminal ⊕ tasks ⊕ build——terminal/tasks/build
// 与 read/deploy 平行，仅 admin 蕴含它们：E7 设计 §2.4 / DT-5 / DT-6 的
// 执行点）。
func containsScope(scopes, need string) bool {
	for _, s := range strings.Split(scopes, ",") {
		switch strings.TrimSpace(s) {
		case ScopeAdmin:
			return true
		case ScopeDeploy:
			if need == ScopeRead || need == ScopeDeploy {
				return true
			}
		case ScopeRead:
			if need == ScopeRead {
				return true
			}
		case ScopeTerminal:
			if need == ScopeTerminal {
				return true
			}
		case ScopeTasks:
			if need == ScopeTasks {
				return true
			}
		case ScopeBuild:
			if need == ScopeBuild {
				return true
			}
		}
	}
	return false
}

// Principal 是通过鉴权的调用方身份（handler 侧审计 actor_token_id 消费）。
// v0.3 W1（RBAC 设计 §4.2 第 1 条）扩展用户维度：UserID/SessionID 仅有会话
// 或用户 PAT 凭据时非空；机具令牌（user NULL）二者恒空 = 平台级凭据。
type Principal struct {
	TokenID string
	Scopes  []string
	// UserID 是属主用户（会话凭据 = 会话属主；用户 PAT = token.user_id；
	// 机具令牌 = ""）。
	UserID string
	// SessionID 是会话凭据的会话行 ID（Cookie 认证分支非空；Bearer 恒空）。
	SessionID string
}

// sessionScopes（W1 临时口径：会话凭据视为 read/deploy/admin 全集）已于
// v0.3 W2-S4 废除——会话凭据的 scope 形状 = reachableScopesForUser（角色
// 蕴含的并集，ownership.go 单点；平台管理员 = 全集，developer+ 含 terminal），
// 资源面的真授权在第 2 门角色门（ResolvePermission，按目标资源逐请求解析
// ——rbac-teams §4.2；auth.go 旧注释的「W2 收口点」就此收口）。
// requiredScopeKey 是拦截器注入 ctx 的方法所需 scope 键（角色门把 scope.go
// 登记映射为资源层级的唯一输入——accessLevelForScope）。
type requiredScopeKey struct{}

// putRequiredScope / requiredScopeFromContext 是 ctx 携带方法所需 scope 的
// 读写对（拦截器写、资源 handler 读；缺失时角色门按 levelAdmin fail-closed）。
func putRequiredScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, requiredScopeKey{}, scope)
}

func requiredScopeFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(requiredScopeKey{}).(string); ok {
		return s
	}
	return ""
}

type principalKey struct{}

// PrincipalFromContext 取调用方身份（未鉴权路径不存在——拦截器链强制）。
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator 是 token 认证器（state 层哈希认证 + last_used_at 节流盖写
// + scope 判定 + per-token 限流——限流在认证成功后判定，身份先于限流成立）。
type Authenticator struct {
	st      *state.Store
	limiter *rateLimiter
	// A2（S18）：last_used_at 盖写节流——每请求同步 UPDATE tokens 是
	// SQLite 写放大，改为进程内 map[tokenID]上次成功写时刻，距上次成功
	// 写不足 touchEvery 跳过。map 无界增长无需防护：键空间 = 出现过的
	// token ID，活跃 token 数受 tokens 表约束（吊销/删除后残留的少量
	// 项是常数级观测误差，单机规模可忽略）。节流是尽力而为语义：并发
	// 首写可能重复落一次 UPDATE（无正确性影响）；写失败不记窗口、下次
	// 认证重试（last_used 是观测面，失败不拒绝已认证请求）。
	touchMu    sync.Mutex
	lastTouch  map[string]time.Time
	touchEvery time.Duration
	// touch 是盖写端口（缺省 st.TouchTokenUsed；测试注入计数假实现）。
	touch func(ctx context.Context, tokenID string) error
	// sessionTouch 是会话滑动续期端口（缺省 st.SlideSession；A2 同款节流经
	// lastTouch map 复用，键加 "sess:" 前缀隔离——滑动续期与 last_seen 盖写
	// 同一节流拍，60s 窗口内重复认证不重复落写）。
	sessionTouch func(ctx context.Context, sessionID string) error
	// sessionTTL 是会话滑动窗口时长（config auth.session_ttl_hours 的注入面；
	// 缺省 state.DefaultSessionTTL = 7 天。绝对上限 30 天在 state 层封顶）。
	sessionTTL time.Duration
	// now 是可注入时钟（测试用短窗口推进时间）。
	now func() time.Time
}

// A2 缺省节流窗口：60s 内的重复认证不重复盖 last_used_at（1 次/分钟/
// token 的观测精度对「最近使用」语义足够）。
const defaultTouchEvery = 60 * time.Second

// NewAuthenticator 构造认证器（内置缺省参数限流器：宽松防滥用）。
func NewAuthenticator(st *state.Store) *Authenticator {
	return &Authenticator{
		st:           st,
		limiter:      newRateLimiter(0, 0),
		lastTouch:    make(map[string]time.Time),
		touchEvery:   defaultTouchEvery,
		touch:        st.TouchTokenUsed,
		sessionTouch: func(ctx context.Context, id string) error { return st.SlideSession(ctx, id, state.DefaultSessionTTL) },
		sessionTTL:   state.DefaultSessionTTL,
		now:          time.Now,
	}
}

// WithSessionTTL 注入会话滑动窗口时长（config auth.session_ttl_hours 的装配
// 面；非正值忽略保持缺省——链式装配）。
func (a *Authenticator) WithSessionTTL(ttl time.Duration) *Authenticator {
	if ttl > 0 {
		a.sessionTTL = ttl
		s := a.st
		a.sessionTouch = func(ctx context.Context, id string) error { return s.SlideSession(ctx, id, ttl) }
	}
	return a
}

// Authenticate 校验 Bearer 凭据并返回身份；失败返回 grpc status（401/403
// 信封退化形态）。required scope 判定在 authorize（拦截器按方法映射调用）。
// 认证成功后经 A2 节流盖写 last_used_at（窗口内只写一次）。
func (a *Authenticator) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	token := bearerToken(authorization)
	if token == "" {
		return Principal{}, statusEnvelope(codes.Unauthenticated, "missing bearer token")
	}
	tok, err := a.st.AuthenticateToken(ctx, token)
	if err != nil {
		// ErrTokenInvalid / ErrTokenRevoked / 其他读取故障：统一 401（不
		// 泄漏存在性与内部状态；读取故障按拒绝处理属 fail-closed）。
		return Principal{}, statusEnvelope(codes.Unauthenticated, "invalid or revoked token")
	}
	if a.limiter != nil && !a.limiter.allow(tok.ID) {
		return Principal{}, statusEnvelope(codes.ResourceExhausted, "rate limit exceeded for token")
	}
	a.touchUsed(ctx, tok.ID)
	return Principal{TokenID: tok.ID, Scopes: strings.Split(tok.Scopes, ","), UserID: tok.UserID}, nil
}

// sessionCookieName 是 Console 浏览器会话 cookie 名（RBAC 设计 §2.2；
// gateway 透传 HTTP Cookie 头 → metadata "grpcgateway-cookie"，本分支解析）。
const sessionCookieName = "fleetly_session"

// AuthenticateSessionCookie 校验会话 cookie 凭据并返回身份：Cookie 头里的
// fleetly_session 明文 → state 哈希认证（属主禁用/过期一并拒认）→ Principal
// {SessionID, UserID, Scopes=可达集}。**W2-S4 硬收缩**：Scopes 不再是 W1 的
// 临时全集，而是 reachableScopesForUser（角色蕴含并集——viewer 只 read、
// developer 含 terminal、平台管理员全集；ownership.go 单点）；资源面的精确
// 授权在第 2 门角色门逐请求解析（rbac-teams §4.2）。认证成功后经 A2 节流
// 滑动续期（last_seen 盖写 + expires 滑动延长，绝对上限 state 层封顶）。
// 任何失败统一 401（不泄漏会话存在性）。
func (a *Authenticator) AuthenticateSessionCookie(ctx context.Context, cookieHeader string) (Principal, error) {
	plaintext := sessionCookieValue(cookieHeader)
	if plaintext == "" {
		return Principal{}, statusEnvelope(codes.Unauthenticated, "missing session cookie")
	}
	sess, err := a.st.AuthenticateSession(ctx, plaintext)
	if err != nil {
		// ErrSessionInvalid / ErrSessionExpired / 读取故障：统一 401
		//（fail-closed，不泄漏内部状态）。
		return Principal{}, statusEnvelope(codes.Unauthenticated, "invalid or expired session")
	}
	a.touchSessionUsed(ctx, sess.ID)
	scopes := reachableScopesForUser(ctx, a.st, sess.UserID)
	return Principal{SessionID: sess.ID, UserID: sess.UserID, Scopes: scopeSetToList(scopes)}, nil
}

// scopeSetToList 把可达集转为固定词表序的切片（Principal.Scopes 存储形态）。
func scopeSetToList(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for _, s := range []string{ScopeRead, ScopeDeploy, ScopeTerminal, ScopeTasks, ScopeBuild, ScopeAdmin} {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// touchSessionUsed 按 A2 节流盖写会话 last_seen_at（与 token 盖写共用节流
// map，键加 "sess:" 前缀隔离；窗口内跳过、失败不落窗口）。
func (a *Authenticator) touchSessionUsed(ctx context.Context, sessionID string) {
	key := "sess:" + sessionID
	now := a.now()
	a.touchMu.Lock()
	if last, ok := a.lastTouch[key]; ok && now.Sub(last) < a.touchEvery {
		a.touchMu.Unlock()
		return
	}
	a.touchMu.Unlock()
	if a.sessionTouch == nil {
		return
	}
	if err := a.sessionTouch(ctx, sessionID); err != nil {
		return
	}
	a.touchMu.Lock()
	a.lastTouch[key] = now
	a.touchMu.Unlock()
}

// touchUsed 按 A2 节流盖写 last_used_at：距上次**成功**写不足 touchEvery
// 跳过；写失败不拒绝请求、不记窗口（下次认证重试）。
func (a *Authenticator) touchUsed(ctx context.Context, tokenID string) {
	now := a.now()
	a.touchMu.Lock()
	if last, ok := a.lastTouch[tokenID]; ok && now.Sub(last) < a.touchEvery {
		a.touchMu.Unlock()
		return // 窗口内已有成功写：跳过（SQLite 写放大收口）
	}
	a.touchMu.Unlock()
	if err := a.touch(ctx, tokenID); err != nil {
		return // 尽力而为观测面：失败不落窗口
	}
	a.touchMu.Lock()
	a.lastTouch[tokenID] = now
	a.touchMu.Unlock()
}

// bearerToken 解析 "Bearer <token>" 头（大小写不敏感 scheme；其余形态
// 一律视为缺失——token 本体不含空格）。
func bearerToken(authorization string) string {
	parts := strings.SplitN(strings.TrimSpace(authorization), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// sessionCookieValue 从 Cookie 头原文解析 fleetly_session 的值（RFC 6265
// name=value 对的分号分隔形态；值不含引号——服务端下发的 64 hex，此处对
// 引号包裹形态做防御式剥离）。
func sessionCookieValue(cookieHeader string) string {
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		name, value, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(name) != sessionCookieName {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		return value
	}
	return ""
}

// authorize 判定 scope：不足 → 403（PermissionDenied，信封退化形态）。
func (a *Authenticator) authorize(p Principal, required string) error {
	if containsScope(strings.Join(p.Scopes, ","), required) {
		return nil
	}
	return statusEnvelope(codes.PermissionDenied,
		fmt.Sprintf("token scope insufficient (requires %s)", required))
}

// ── 拦截器 ───────────────────────────────────────────────────────────────────

// authExemptPrefixes 豁免前缀：框架服务（health/reflection）与显式公开
// 方法（SystemService.Ping——T2.17 契约：Ping 与 healthz 豁免鉴权）。
var authExemptPrefixes = []string{
	"/grpc.health.v1.",
	"/grpc.reflection.",
}

// authExemptMethods 显式豁免的完整方法名（v0.3 W1 增认证三面：Register/
// Login 无凭据可用；GetRegistrationState 驱动登录页注册入口——rbac-teams
// §2.2「REST /v1/auth/* 进鉴权豁免名单（Register/Login/Ping 族）」）。
var authExemptMethods = map[string]bool{
	"/fleetly.server.v1.SystemService/Ping":          true,
	"/fleetly.server.v1.AuthService/Register":        true,
	"/fleetly.server.v1.AuthService/Login":           true,
	"/fleetly.server.v1.AuthService/GetRegistrationState": true,
}

func authExempt(fullMethod string) bool {
	if authExemptMethods[fullMethod] {
		return true
	}
	for _, prefix := range authExemptPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return true
		}
	}
	return false
}

// UnaryAuthInterceptor 一元鉴权拦截器（链位：最外层，先于限流与
// protovalidate）。
func (a *Authenticator) UnaryAuthInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if authExempt(info.FullMethod) {
			return handler(ctx, req)
		}
		required, ok := RequiredScope(info.FullMethod)
		if !ok {
			// 未登记方法 fail-closed：按 admin 拒绝路径处理（新方法漏登记
			// 时宁拒勿放——拦截器链是安全边界）。
			required = ScopeAdmin
		}
		p, err := a.authenticateAndAuthorize(ctx, info.FullMethod, required)
		if err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, principalKey{}, p)
		ctx = context.WithValue(ctx, actionKey{}, methodAction(info.FullMethod))
		// 角色门的层级输入（scope.go 登记唯一来源——第 1 门与第 2 门共用）。
		ctx = putRequiredScope(ctx, required)
		return handler(ctx, req)
	}
}

// StreamAuthInterceptor 流式鉴权拦截器（Follow/Watch 双流同矩阵）。
func (a *Authenticator) StreamAuthInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if authExempt(info.FullMethod) {
			return handler(srv, ss)
		}
		required, ok := RequiredScope(info.FullMethod)
		if !ok {
			required = ScopeAdmin
		}
		p, err := a.authenticateAndAuthorize(ss.Context(), info.FullMethod, required)
		if err != nil {
			return err
		}
		ctx := context.WithValue(ss.Context(), principalKey{}, p)
		ctx = context.WithValue(ctx, actionKey{}, methodAction(info.FullMethod))
		ctx = putRequiredScope(ctx, required)
		return handler(srv, &authorizedStream{ServerStream: ss, ctx: ctx})
	}
}

// authenticateAndAuthorize 从 metadata 取凭据走认证 + scope 判定。双凭据
// 形态（RBAC 设计 §2.2）：Authorization Bearer 优先（CLI/机具面现状不变）；
// 无 Bearer 凭据时回落会话 cookie（gateway 透传形态 "grpcgateway-cookie"
// 优先，gRPC 直连手工携带的 "cookie" 键兜底）——Bearer 存在但不合法仍走
// Bearer 401（不静默回落 cookie，凭据来源唯一可判定）。
func (a *Authenticator) authenticateAndAuthorize(ctx context.Context, fullMethod, required string) (Principal, error) {
	authorization := authorizationFromMetadata(ctx)
	var (
		p   Principal
		err error
	)
	if bearerToken(authorization) != "" {
		p, err = a.Authenticate(ctx, authorization)
	} else if cookieHeader := cookieFromMetadata(ctx); cookieHeader != "" {
		p, err = a.AuthenticateSessionCookie(ctx, cookieHeader)
	} else {
		p, err = a.Authenticate(ctx, authorization)
	}
	if err != nil {
		return Principal{}, err
	}
	if err := a.authorize(p, required); err != nil {
		return Principal{}, err
	}
	return p, nil
}

// authorizationFromMetadata 取 Authorization 头（gateway 转发形态
// "authorization" 优先；grpcgateway- 前缀形态兜底）。
func authorizationFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, key := range []string{"authorization", "grpcgateway-authorization"} {
		if vals := md.Get(key); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// cookieFromMetadata 取 Cookie 头原文（grpc-gateway v2 缺省 HeaderMatcher
// 把永久头（IANA 永久清单，Cookie 在列）经 "grpcgateway-" 前缀转入 gRPC
// metadata——键全小写；直连 gRPC 客户端手工携带的 "cookie" 键兜底）。
func cookieFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, key := range []string{"grpcgateway-cookie", "cookie"} {
		if vals := md.Get(key); len(vals) > 0 {
			return strings.Join(vals, "; ")
		}
	}
	return ""
}

// authorizedStream 覆写 Context 的 ServerStream（把 Principal 注入流式
// handler 的 ctx——grpc.ServerStream.Context() 不可直接替换，经包装实现）。
type authorizedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authorizedStream) Context() context.Context { return s.ctx }

// statusEnvelope 构造退化信封错误：**不附加 ErrorResponse detail**——
// gateway 侧 EnvelopeFromGRPCStatus 对无 detail status 走退化路径（code
// 留空 + GRPCCodeToHTTP 机械映射 401/403/429），与 FZ-2 口径一致。X-5 起
// 空码 detail 信封（conflict() 的业务冲突通道）按 grpc code 机械映射——
// 但鉴权错误保持纯 status 形态（无 detail 即无多余字节，语义不变）。
func statusEnvelope(c codes.Code, message string) error {
	return status.Error(c, message)
}
