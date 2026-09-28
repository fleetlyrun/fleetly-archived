package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
)

// TokensService 实现 server.v1.TokensService（T2.17；state-model §2.9）。
// 哈希存储：明文只在 CreateToken 响应出现一次；List 只出备注/scope/
// 哈希前缀。scope 集（admin 蕴含 deploy 蕴含 read ⊕ terminal）在
// containsScope 判定。
//
// v0.3 W2 语义迁移（rbac-teams 设计 §2.3）：从「admin 全局面」改为「用户
// 自服务面」——
//   - CreateToken：用户 principal 造自己的 PAT（user_id = 自己；声明
//     scopes ⊆ 用户可达集校验，防呆非防险——角色门仍是硬边界；项目绑定
//     可选）；机具令牌（user NULL）= 平台级凭据的显式创建语义：调用方须
//     为平台管理员用户（machine 旗标）或 admin scope 机具令牌，否则 403；
//   - ListTokens：用户 = 自己的；平台管理员 = 全部（user_id 注记机具）；
//     机具令牌按「平台管理员等价」放行全列（票面裁决）；
//   - RevokeToken：自己的或平台管理员；机具令牌吊销 = admin scope。
//
// scope 门登记 read（最小形状约束——会话凭据天然全集、用户 PAT 最小 read
// 可达），真授权在本文件 handler 内（teams/projects 同款切面；纪律：改
// 登记 = 改测试，TestAuthMatrix 的 token 行随迁）。
type TokensService struct {
	serverv1.UnimplementedTokensServiceServer
	st *state.Store
}

// NewTokensService 构造 TokensService。
func NewTokensService(st *state.Store) *TokensService {
	return &TokensService{st: st}
}

// tokenPrefix 是明文 token 前缀（可识别性；本体 = 24 随机字节 hex）。用户
// PAT 与机具令牌同格式——区分在库（user_id NULL 与否）不在串（设计 §2.3）。
const tokenPrefix = "flt_"

// CreateToken 生成并落库新 token：明文仅本次响应可见。语义分支见类型注释
// （用户自服务 PAT / 平台级机具令牌两态）。
func (s *TokensService) CreateToken(ctx context.Context, req *serverv1.CreateTokenRequest) (*serverv1.CreateTokenResponse, error) {
	plaintext, err := generateToken()
	if err != nil {
		return nil, err
	}
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	w := state.TokenWrite{
		Hash:         state.HashToken(plaintext),
		Name:         req.GetNote(),
		Scopes:       normalizeScopes(req.GetScopes()),
		ActorTokenID: callerTokenID(ctx),
		ProjectID:    req.GetProjectId(),
	}
	if p.UserID != "" && !req.GetMachine() {
		// 用户自服务：造自己的 PAT（声明 scopes ⊆ 可达集；项目须在册）。
		if err := s.validateProjectBinding(ctx, w.ProjectID); err != nil {
			return nil, err
		}
		if err := s.validateDeclaredScopes(ctx, p.UserID, w.Scopes); err != nil {
			return nil, err
		}
		w.UserID = p.UserID
		w.Actor = "user:" + p.UserID // 审计主体署名（设计 §6 口径）
	} else {
		// 机具令牌（user NULL）= 平台级凭据显式创建：用户调用方须为平台
		// 管理员；机具令牌调用方须持 admin scope（写面语义，与既有 CI
		// 形态兼容——scope 门 read 放行后在此收口）。
		if p.UserID != "" {
			if !isPlatformAdminUser(ctx, s.st) {
				return nil, statusEnvelope(codes.PermissionDenied,
					"only platform admins can create machine tokens (platform-level credentials)")
			}
		} else if !containsScope(strings.Join(p.Scopes, ","), ScopeAdmin) {
			return nil, statusEnvelope(codes.PermissionDenied,
				"machine token lacks the admin scope required to mint tokens")
		}
	}
	tok, err := s.st.CreateToken(ctx, w)
	if err != nil {
		return nil, err
	}
	return &serverv1.CreateTokenResponse{
		Id:        tok.ID,
		Token:     plaintext, // 仅此一次
		Note:      tok.Name,
		Scopes:    strings.Split(tok.Scopes, ","),
		CreatedAt: tstamp(tok.CreatedAt),
	}, nil
}

// ListTokens 在册 token 列表（无敏感投影；完整哈希与明文永不回读）：
// 用户 = 自己的 PAT；平台管理员/机具令牌 = 全部（user_id 区分用户 PAT 与
// 机具令牌——机具令牌该字段缺省，EmitUnpopulated=false 语义）。
func (s *TokensService) ListTokens(ctx context.Context, _ *serverv1.ListTokensRequest) (*serverv1.ListTokensResponse, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	var (
		rows []state.Token
		err  error
	)
	switch {
	case p.UserID == "" || isPlatformAdminUser(ctx, s.st):
		rows, err = s.st.ListTokens(ctx)
	default:
		rows, err = s.st.ListTokensForUser(ctx, p.UserID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.TokenView, 0, len(rows))
	for _, t := range rows {
		out = append(out, &serverv1.TokenView{
			Id:         t.ID,
			Note:       t.Name,
			Scopes:     strings.Split(t.Scopes, ","),
			HashPrefix: t.HashPrefix,
			CreatedAt:  tstamp(t.CreatedAt),
			LastUsedAt: tstamp(t.LastUsedAt),
			RevokedAt:  tstamp(t.RevokedAt),
			UserId:     t.UserID,
			ProjectId:  t.ProjectID,
		})
	}
	return &serverv1.ListTokensResponse{Tokens: out}, nil
}

// RevokeToken 吊销（幂等；不存在 404）。可见性先于动作：目标不在调用方
// 可见集（非自己的且非平台管理员/机具令牌）一律 404——token ID 不可枚举，
// 不泄漏存在性。M4-6 最后管理员守卫：吊销后平台必须仍存在 ≥1 枚未吊销
// admin token——依次吊销全部 admin 会使平台锁死（重启也不补种 bootstrap：
// HasAnyToken 已见 token 行，一次性语义），最后一枚的吊销被守卫拒绝
// （E_TOKEN_LAST_ADMIN 409，提示先创建新 token）。守卫判定与吊销在 state
// 层同一事务内闭合（RevokeTokenGuardLastAdmin）。
func (s *TokensService) RevokeToken(ctx context.Context, req *serverv1.RevokeTokenRequest) (*serverv1.RevokeTokenResponse, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	target, err := s.st.GetToken(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	switch {
	case p.UserID == "":
		// 机具令牌：吊销是写面语义，须 admin scope（平台级凭据管理）。
		if !containsScope(strings.Join(p.Scopes, ","), ScopeAdmin) {
			return nil, statusEnvelope(codes.PermissionDenied,
				"machine token lacks the admin scope required to revoke tokens")
		}
	case isPlatformAdminUser(ctx, s.st):
		// 平台管理员：可吊销任意 token（含机具令牌）。
	default:
		if target.UserID != p.UserID {
			// 非自己的（他人 PAT 或机具令牌）：可见集外，404 收口。
			return nil, notFound("token not found: " + req.GetId())
		}
	}
	err = s.st.RevokeTokenGuardLastAdmin(ctx, req.GetId(), callerTokenID(ctx), func(scopes string) bool {
		return containsScope(scopes, ScopeAdmin)
	})
	if err != nil {
		// ErrTokenNotFound → 404、ErrTokenLastAdmin → E_TOKEN_LAST_ADMIN
		// 409（errors.go 哨兵登记表）；表外错误原样透传。
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.RevokeTokenResponse{Id: req.GetId(), RevokedAt: "revoked"}, nil
}

// validateProjectBinding 校验可选项目绑定（设计 §2.3 project_id 收窄维度
// ——W2-S4 角色门消费；绑定是输入校验面：未知项目 400，不在本面做成员
// 资格判定——防呆非防险，越权绑定在角色门被硬性收缩）。
func (s *TokensService) validateProjectBinding(ctx context.Context, projectID string) error {
	if projectID == "" {
		return nil
	}
	if _, err := s.st.GetProject(ctx, projectID); err != nil {
		if errors.Is(err, state.ErrProjectNotFound) {
			return statusInvalidArgument("unknown project: " + projectID)
		}
		return err
	}
	return nil
}

// validateDeclaredScopes 校验声明 scopes ⊆ 用户可达集（设计 §2.3：防呆
// 非防险——viewer 用户造 admin PAT → 400 带指引；硬收缩在第 2 门角色门按
// min(scopes, 目标项目角色蕴含) 逐请求生效，本校验只把明显的越权声明挡在
// 创建时）。可达集单点 reachableScopesForUser（ownership.go）——v0.3 W2-S4
// 收口：S2 的「不做覆写精化」遗留在此闭环，覆写行（含升向）并入可达集。
func (s *TokensService) validateDeclaredScopes(ctx context.Context, userID, declared string) error {
	reachable := reachableScopesForUser(ctx, s.st, userID)
	for _, sc := range strings.Split(declared, ",") {
		if sc == "" {
			continue
		}
		if !reachable[sc] {
			return statusInvalidArgument(fmt.Sprintf(
				"scope %q exceeds your granted capabilities (reachable scopes: %s); ask a team owner for a higher role, or declare a narrower scope set",
				sc, reachableScopeList(reachable)))
		}
	}
	return nil
}

// generateToken 生成明文 token（crypto/rand 24 字节 → 48 hex）。
func generateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("api: generate token: %w", err)
	}
	return tokenPrefix + hex.EncodeToString(buf), nil
}

// GenerateBootstrapAdminToken 生成并落库 admin token（T2.17 安装引导；
// 明文返回一次，库内只存哈希）。审计 ActorTokenID 空 = 自举（无调用方
// token）。已存在任意 token 时调用方应跳过（HasAnyToken 谓词）。
func GenerateBootstrapAdminToken(ctx context.Context, st *state.Store, note string) (string, error) {
	plaintext, err := generateToken()
	if err != nil {
		return "", err
	}
	if _, err := st.CreateToken(ctx, state.TokenWrite{
		Hash:   state.HashToken(plaintext),
		Name:   note,
		Scopes: ScopeAdmin,
	}); err != nil {
		return "", err
	}
	return plaintext, nil
}

// normalizeScopes 归一 scope 集（去重、保序）。
func normalizeScopes(in []string) string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

// callerTokenID 取调用方 token ID（审计 actor_token_id；bootstrap 场景
// 可空）。
func callerTokenID(ctx context.Context) string {
	if p, ok := PrincipalFromContext(ctx); ok {
		return p.TokenID
	}
	return ""
}
