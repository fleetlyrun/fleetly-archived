package api

import (
	"context"
	"fmt"
	"strings"

	gossh "golang.org/x/crypto/ssh"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
)

// GitKeysService 实现 server.v1.GitKeysService（T2.19）：git 公钥（SSH
// push 认证）。公钥指纹入库（SHA256，ssh-keygen -lf 同格式）；私钥永不
// 经过平台。生命周期动作审计在 state 层与业务写同事务 fail-closed
// （gitkey.add / gitkey.remove）。
//
// v0.3 W2 用户化迁移（rbac-teams 设计 §2.3）：
//   - AddGitKey = 登录用户自服务（user principal 必需——机具令牌 403，
//     平台管理员用户亦然：公钥归属用户，push 审计 actor 随署名用户）；
//   - ListGitKeys = 自己的；平台管理员/机具令牌 = 全部（user_id 注记；
//     存量无主键〔user_id NULL〕只读展示归全列消费方）；
//   - RemoveGitKey = 自己的或平台管理员；机具令牌删除 = admin scope。
//
// scope 门登记 read（最小形状约束），真授权在本文件 handler 内。
type GitKeysService struct {
	serverv1.UnimplementedGitKeysServiceServer
	st *state.Store
}

// NewGitKeysService 构造 GitKeysService。
func NewGitKeysService(st *state.Store) *GitKeysService {
	return &GitKeysService{st: st}
}

// AddGitKey 解析 authorized_keys 单行并入库（指纹唯一——重复注册 409 退化
// 信封）。登录用户自服务：key 归属调用方用户，SSH push 认证按指纹命中后
// 以该用户入 push 审计 actor（设计 §2.3）。
func (s *GitKeysService) AddGitKey(ctx context.Context, req *serverv1.AddGitKeyRequest) (*serverv1.AddGitKeyResponse, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	if p.UserID == "" {
		return nil, statusEnvelope(codes.PermissionDenied,
			"git keys are user credentials: add keys from a logged-in account (machine tokens cannot own push keys)")
	}
	line := strings.TrimSpace(req.GetPublicKey())
	if strings.ContainsAny(line, "\n\r") {
		return nil, statusInvalidArgument("public key must be a single authorized_keys line")
	}
	pub, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, statusInvalidArgument("parse public key: not an authorized_keys line (" + err.Error() + ")")
	}
	keyType := pub.Type()
	// 备注缺省取 comment（authorized_keys 第三字段——常见即设备标识）。
	note := req.GetNote()
	if note == "" {
		note = strings.TrimSpace(comment)
	}
	if len(note) > 200 {
		note = note[:200]
	}
	// 入库存原始行（options/形态原样保留）；认证匹配只看指纹——原始行的
	// 解析形态与指纹由本处解析产出，存储即留痕。
	key, err := s.st.CreateGitKey(ctx, state.GitKeyWrite{
		Fingerprint:  gossh.FingerprintSHA256(pub),
		PublicKey:    line,
		KeyType:      keyType,
		Note:         note,
		ActorTokenID: callerTokenID(ctx),
		UserID:       p.UserID,
		ActorUserID:  p.UserID,
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return &serverv1.AddGitKeyResponse{
		Id:          key.ID,
		Fingerprint: key.Fingerprint,
		KeyType:     key.KeyType,
		Note:        key.Note,
		CreatedAt:   tstamp(key.CreatedAt),
	}, nil
}

// ListGitKeys 在册公钥列表（公钥为公开材料可回读）：用户 = 自己的；
// 平台管理员/机具令牌 = 全部（user_id 区分自服务 key 与存量无主键）。
func (s *GitKeysService) ListGitKeys(ctx context.Context, _ *serverv1.ListGitKeysRequest) (*serverv1.ListGitKeysResponse, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	var (
		rows []state.GitKey
		err  error
	)
	if p.UserID == "" || isPlatformAdminUser(ctx, s.st) {
		rows, err = s.st.ListGitKeys(ctx)
	} else {
		rows, err = s.st.ListGitKeysForUser(ctx, p.UserID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.GitKeyView, 0, len(rows))
	for _, k := range rows {
		out = append(out, &serverv1.GitKeyView{
			Id:          k.ID,
			Fingerprint: k.Fingerprint,
			KeyType:     k.KeyType,
			Note:        k.Note,
			CreatedAt:   tstamp(k.CreatedAt),
			UserId:      k.UserID,
		})
	}
	return &serverv1.ListGitKeysResponse{Keys: out}, nil
}

// RemoveGitKey 删除公钥（幂等语义：不存在 404 退化信封；删除即时生效
// ——在推连接不受影响，新握手即拒绝）。可见性先于动作：用户只能删自己
// 的 key（他人的/无主 key 对其 404——不可见即不存在，不泄漏注册表）；
// 平台管理员任意；机具令牌须 admin scope（写面语义）。
func (s *GitKeysService) RemoveGitKey(ctx context.Context, req *serverv1.RemoveGitKeyRequest) (*serverv1.RemoveGitKeyResponse, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, statusEnvelope(codes.Unauthenticated, "missing credential")
	}
	target, err := s.st.GetGitKey(ctx, req.GetId())
	if err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	actorUserID := p.UserID
	switch {
	case p.UserID == "":
		if !containsScope(strings.Join(p.Scopes, ","), ScopeAdmin) {
			return nil, statusEnvelope(codes.PermissionDenied,
				"machine token lacks the admin scope required to remove git keys")
		}
		actorUserID = "" // 机具动作：审计 actor 落 human 原口径
	case isPlatformAdminUser(ctx, s.st):
		// 平台管理员：任意 key。
	default:
		if target.UserID != p.UserID {
			return nil, notFound(fmt.Sprintf("git key not found: %s", req.GetId()))
		}
	}
	if err := s.st.RemoveGitKey(ctx, req.GetId(), actorUserID, callerTokenID(ctx)); err != nil {
		return nil, mapStoreErr(err, req.GetId())
	}
	return &serverv1.RemoveGitKeyResponse{Id: req.GetId()}, nil
}
