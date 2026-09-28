package api

// SecretsService 实现 server.v1.SecretsService（E4 managed-databases §2.7，
// D-DB-7：平台密钥库资源面——compose external secret 的唯一来源，开放面 =
// 所有 app）。无值读回面（忘记即轮换——比 env GetEnv 的 admin 明文路径更
// 严一档）：Set 加密落库、List 只投影名称/指纹/时间锚、Remove 删除行——
// 值与密文零出现在任何响应。
//
// 明文纪律：值只存活于「入站明文 → box.Encrypt」的内存链；不进日志/事件/
// 审计/错误（审计 diff 只带名称与 hash8 指纹——负面测试钉死）。
//
// scope（scope.go 登记处）：set/remove = admin（密钥写面与 app 删除同级
// 信任）；list = read（只出名称/指纹——与 env ListEnv 同口径）。

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// maxSecretValueBytes 是值的 sanity 上限（64KiB——proto 形状层
// max_len 同值；Swarm secret 单对象上界 500KB 的宽松内档。形状层已拒，
// 本层兜底防绕过 buf.validate 的调用面）。
const maxSecretValueBytes = 64 << 10

// SecretsService 实现 server.v1.SecretsService。
type SecretsService struct {
	serverv1.UnimplementedSecretsServiceServer
	st  *state.Store
	box *secrets.Box
}

// NewSecretsService 构造 SecretsService。
func NewSecretsService(st *state.Store, box *secrets.Box) *SecretsService {
	return &SecretsService{st: st, box: box}
}

// SetSecret 写入（覆盖即轮换：cipher 与 hash8 同拍重盖；Swarm secret 换名
// 由发布引擎按新 hash8 承载——引用进 desired-hash，值变化随下次部署换挂）。
func (s *SecretsService) SetSecret(ctx context.Context, req *serverv1.SetSecretRequest) (*serverv1.SetSecretResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：set/remove=admin、list=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	if err := validateSecretName(req.GetName()); err != nil {
		return nil, statusInvalidArgument(err.Error())
	}
	if len(req.GetValue()) == 0 || len(req.GetValue()) > maxSecretValueBytes {
		return nil, statusInvalidArgument("secret value length must be within 1..65536 bytes")
	}
	cipher, err := s.box.Encrypt([]byte(req.GetValue()))
	if err != nil {
		return nil, err
	}
	hash8 := naming.Hash8(req.GetValue())
	row, err := s.st.UpsertAppSecret(ctx, state.AppSecret{
		AppID:       app.ID,
		Name:        req.GetName(),
		ValueCipher: string(cipher),
		Hash8:       hash8,
	})
	if err != nil {
		return nil, err
	}
	// 审计 secret.set（§5.3 动作词表；diff 只带名称与 hash8 指纹——值零
	// 出现）。同事务 fail-closed。
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, secretAudit(ctx, "secret.set", app.Name, row.Name,
			state.DiffSummary("hash8", row.Hash8)))
	}); err != nil {
		return nil, err
	}
	return &serverv1.SetSecretResponse{
		App:       app.Name,
		Name:      row.Name,
		Hash8:     row.Hash8,
		CreatedAt: timestamppb.New(row.CreatedAt),
		UpdatedAt: timestamppb.New(row.UpdatedAt),
	}, nil
}

// ListSecrets 该 app 全部 secret（只投影名称/指纹/时间锚；值与密文零出现
// ——D-DB-7 无值读回）。
func (s *SecretsService) ListSecrets(ctx context.Context, req *serverv1.ListSecretsRequest) (*serverv1.ListSecretsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：set/remove=admin、list=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rows, err := s.st.ListAppSecrets(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.SecretView, 0, len(rows))
	for _, row := range rows {
		out = append(out, &serverv1.SecretView{
			Name:      row.Name,
			Hash8:     row.Hash8,
			CreatedAt: timestamppb.New(row.CreatedAt),
			UpdatedAt: timestamppb.New(row.UpdatedAt),
		})
	}
	return &serverv1.ListSecretsResponse{Secrets: out}, nil
}

// RemoveSecret 删除单条（幂等不做：不存在 404——与 RemoveEnv 同口径）。
// 已被运行中服务引用的 removal 不追写部署——引用方下次部署 preflight
// E_SECRET_NOT_FOUND 诚实失败（移除声明再部署的既有语义）。
func (s *SecretsService) RemoveSecret(ctx context.Context, req *serverv1.RemoveSecretRequest) (*serverv1.RemoveSecretResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：set/remove=admin、list=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.RemoveAppSecret(ctx, app.ID, req.GetName()); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, secretAudit(ctx, "secret.removed", app.Name, req.GetName(),
			state.DiffSummary("hash8", "")))
	})
	if err != nil {
		return nil, mapStoreErr(err, req.GetName())
	}
	return &serverv1.RemoveSecretResponse{App: app.Name, Name: req.GetName()}, nil
}

// secretAudit 构造密钥库审计条目（设计 §5.3 动作词表 secret.set/removed；
// target = secret:<app>/<名>——与 db.* 审计的 database:<id> 同型；actor
// 归因同款：API 无法区分人类/AI 代理，token 承载可追溯性）。
func secretAudit(ctx context.Context, action, app, name, diff string) state.AuditEntry {
	entry := databaseAudit(ctx, action, "secret:"+app+"/"+name, diff)
	return entry
}

// validateSecretName 是 secret 声明名的 handler 层校验（proto 形状层
// pattern 的兜底镜像；与 internal/compose 的 validSecretName 同规则——
// 合法的 /run/secrets/<name> 文件名，两端注释互指）。
func validateSecretName(name string) error {
	if name == "" || len(name) > 63 {
		return statusInvalidArgument("secret name must match ^[A-Za-z0-9][A-Za-z0-9._-]*$ (max 63 chars)")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '_' || r == '-' || r == '.'):
		default:
			return statusInvalidArgument("secret name must match ^[A-Za-z0-9][A-Za-z0-9._-]*$ (max 63 chars)")
		}
	}
	return nil
}
