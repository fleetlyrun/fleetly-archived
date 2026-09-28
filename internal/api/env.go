package api

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// EnvService 实现 server.v1.EnvService（T2.17）：Set/Remove 走 pending
// 语义（state 层内置），Get 明文解密（admin scope 在拦截器链强制），
// List 恒不出值。值加密边界：入站明文 → box.Encrypt 落库；出站解密仅在
// GetEnv 显式路径。
type EnvService struct {
	serverv1.UnimplementedEnvServiceServer
	st  *state.Store
	box *secrets.Box
	// onEnvChanged 是 env 写路径成功后的联动回调（H9：即时失效日志脱敏
	// 值集——30s TTL 窗内新 secret 值会被明文采集并按天落盘保留 7 天，
	// 写点失效把暴露窗收敛到单次重建。nil = 未接（单测/降级形态）；api
	// 不直接依赖 logs.Manager，装配层注入闭包）。
	onEnvChanged func(appID string)
}

// NewEnvService 构造 EnvService。
func NewEnvService(st *state.Store, box *secrets.Box) *EnvService {
	return &EnvService{st: st, box: box}
}

// WithEnvChangedHook 注入 env 写路径联动回调（H9 装配点：fleetlyd 接到
// logs.Manager.InvalidateRedaction）。回调在写事务提交成功后同步执行，
// 实现必须非阻塞（实现方只做缓存删除）。
func (s *EnvService) WithEnvChangedHook(fn func(appID string)) *EnvService {
	s.onEnvChanged = fn
	return s
}

// SetEnv 设置平台 env（pending；值密文落库，审计在 state 层 fail-closed）。
func (s *EnvService) SetEnv(ctx context.Context, req *serverv1.SetEnvRequest) (*serverv1.SetEnvResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：SetEnv=deploy / GetEnv=admin / ListEnv=read
	// / RemoveEnv=deploy——层级由拦截器注入的 scope 登记映射（下同）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	ciphertext, err := s.box.Encrypt([]byte(req.GetValue()))
	if err != nil {
		return nil, err
	}
	// 审计主体署名（W2-12）：用户会话 "user:<id>"；机具令牌无用户身份，
	// 沿用终端会话的 "human" 约定——不落到 state 层默认值（签名强制显式）。
	actor := "human"
	if p, ok := PrincipalFromContext(ctx); ok && p.UserID != "" {
		actor = "user:" + p.UserID
	}
	if _, err := s.st.SetAppEnv(ctx, app.ID, req.GetKey(), string(ciphertext), "platform", actor); err != nil {
		return nil, err
	}
	// H9：值集已变（新 secret 已可随下次部署生效）→ 即时失效脱敏缓存。
	if s.onEnvChanged != nil {
		s.onEnvChanged(app.ID)
	}
	return &serverv1.SetEnvResponse{App: app.Name, Key: req.GetKey(), Status: string(state.EnvStatusPending)}, nil
}

// GetEnv 读回明文（admin scope 专用路径；密文解密失败按内部错误透出）。
func (s *EnvService) GetEnv(ctx context.Context, req *serverv1.GetEnvRequest) (*serverv1.GetEnvResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：SetEnv=deploy / GetEnv=admin / ListEnv=read
	// / RemoveEnv=deploy——层级由拦截器注入的 scope 登记映射（下同）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	row, err := s.st.GetAppEnv(ctx, app.ID, req.GetKey())
	if err != nil {
		return nil, mapStoreErr(err, req.GetKey())
	}
	plaintext, err := s.box.Decrypt([]byte(row.Value))
	if err != nil {
		return nil, err
	}
	return &serverv1.GetEnvResponse{
		App:    app.Name,
		Key:    row.Key,
		Value:  string(plaintext),
		Status: string(row.Status),
	}, nil
}

// ListEnv 键名/来源/状态位（值恒脱敏）。
func (s *EnvService) ListEnv(ctx context.Context, req *serverv1.ListEnvRequest) (*serverv1.ListEnvResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：SetEnv=deploy / GetEnv=admin / ListEnv=read
	// / RemoveEnv=deploy——层级由拦截器注入的 scope 登记映射（下同）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rows, err := s.st.ListAppEnv(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.EnvVarView, 0, len(rows))
	for _, row := range rows {
		out = append(out, &serverv1.EnvVarView{
			Key:       row.Key,
			Source:    row.Source,
			Status:    string(row.Status),
			CreatedAt: timestamppb.New(row.CreatedAt),
			UpdatedAt: timestamppb.New(row.UpdatedAt),
		})
	}
	return &serverv1.ListEnvResponse{EnvVars: out}, nil
}

// RemoveEnv 删除平台 env（幂等性不做：键不存在 404；台账立即删行，运行
// 实例 env 快照随下次部署更新）。
func (s *EnvService) RemoveEnv(ctx context.Context, req *serverv1.RemoveEnvRequest) (*serverv1.RemoveEnvResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：SetEnv=deploy / GetEnv=admin / ListEnv=read
	// / RemoveEnv=deploy——层级由拦截器注入的 scope 登记映射（下同）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	if err := s.st.DeleteAppEnv(ctx, app.ID, req.GetKey()); err != nil {
		return nil, mapStoreErr(err, req.GetKey())
	}
	// H9：删除同样改变值集（旧值不应继续被脱敏之外的语义影响——值集按
	// 当前 state 重建）→ 即时失效。
	if s.onEnvChanged != nil {
		s.onEnvChanged(app.ID)
	}
	return &serverv1.RemoveEnvResponse{App: app.Name, Key: req.GetKey(), Status: string(state.EnvStatusPending)}, nil
}
