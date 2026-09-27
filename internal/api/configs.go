package api

// ConfigsService 实现 server.v1.ConfigsService（T 线 OT-3 / IMPL-T1-4）：
// app 级明文配置资源面——compose `configs:` 声明（external: true）的唯一
// 写入口。与 SecretsService 的差异 = 明文可回读：
//   - Set 覆盖即换版（hash8 重盖），明文落 app_configs（不是凭据材料——
//     设计 OT-3 明确「明文、版本化、审计、可回读」）；
//   - List 只投影名称/指纹/时间锚（与 ListSecrets 同口径——大内容不进列表；
//     值走 GetConfig 的显式 admin 回读）；
//   - Get 明文回读 = admin scope（与 GetEnv/RevealDatabaseCredentials 同级
//     信任面）；
//   - Remove 删除行；引用方下次部署 preflight E_CONFIG_NOT_FOUND 诚实失败。
//
// scope（scope.go 登记处）：set/get/remove = admin（写面与明文读面同门——
// 避免「可写不可读」的错位信任；票面默认对齐 secrets 口径）；list = read
// （只出名称/指纹，与 ListSecrets/ListEnv 同口径）。
//
// 审计纪律：config.set / config.removed 的 diff 只带名称与 hash8 指纹——
// 内容明文只存活于「入站明文 → app_configs 落库」的内存链，不进审计/事件/
// 错误文本。

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// maxConfigValueBytes 是值的 sanity 上限（64KiB——与 secrets 同一口径：
// proto 形状层 max_len 同值；Swarm config 单对象上界 500KB 的宽松内档。
// 形状层已拒，本层兜底防绕过 buf.validate 的调用面）。
const maxConfigValueBytes = 64 << 10

// ConfigsService 实现 server.v1.ConfigsService。
type ConfigsService struct {
	serverv1.UnimplementedConfigsServiceServer
	st *state.Store
}

// NewConfigsService 构造 ConfigsService。
func NewConfigsService(st *state.Store) *ConfigsService {
	return &ConfigsService{st: st}
}

// SetConfig 写入（覆盖即换版：hash8 同拍重盖；Swarm config 换名由发布
// 引擎按新 hash8 承载——引用进 desired-hash，内容变化随下次部署换挂并
// 触发服务滚动）。
func (s *ConfigsService) SetConfig(ctx context.Context, req *serverv1.SetConfigRequest) (*serverv1.SetConfigResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：set/get/remove=admin、list=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	if err := validateConfigName(req.GetName()); err != nil {
		return nil, statusInvalidArgument(err.Error())
	}
	if len(req.GetValue()) == 0 || len(req.GetValue()) > maxConfigValueBytes {
		return nil, statusInvalidArgument("config value length must be within 1..65536 bytes")
	}
	hash8 := naming.Hash8(req.GetValue())
	row, err := s.st.UpsertAppConfig(ctx, state.AppConfig{
		AppID: app.ID,
		Name:  req.GetName(),
		Value: req.GetValue(),
		Hash8: hash8,
	})
	if err != nil {
		return nil, err
	}
	// 审计 config.set（diff 只带名称与 hash8 指纹——内容零出现）。同事务
	// fail-closed。
	if err := s.st.InTx(ctx, func(tx *state.Tx) error {
		return tx.WriteAudit(ctx, configAudit(ctx, "config.set", app.Name, row.Name,
			state.DiffSummary("hash8", row.Hash8)))
	}); err != nil {
		return nil, err
	}
	return &serverv1.SetConfigResponse{
		App:       app.Name,
		Name:      row.Name,
		Hash8:     row.Hash8,
		CreatedAt: timestamppb.New(row.CreatedAt),
		UpdatedAt: timestamppb.New(row.UpdatedAt),
	}, nil
}

// ListConfigs 该 app 全部 config（只投影名称/指纹/时间锚；值零出现）。
func (s *ConfigsService) ListConfigs(ctx context.Context, req *serverv1.ListConfigsRequest) (*serverv1.ListConfigsResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：set/get/remove=admin、list=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	rows, err := s.st.ListAppConfigs(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.ConfigView, 0, len(rows))
	for _, row := range rows {
		out = append(out, &serverv1.ConfigView{
			Name:      row.Name,
			Hash8:     row.Hash8,
			CreatedAt: timestamppb.New(row.CreatedAt),
			UpdatedAt: timestamppb.New(row.UpdatedAt),
		})
	}
	return &serverv1.ListConfigsResponse{Configs: out}, nil
}

// GetConfig 明文回读（admin scope 专用路径——与 GetEnv 同级信任面）。
func (s *ConfigsService) GetConfig(ctx context.Context, req *serverv1.GetConfigRequest) (*serverv1.GetConfigResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：set/get/remove=admin、list=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	row, err := s.st.GetAppConfig(ctx, app.ID, req.GetName())
	if err != nil {
		if errors.Is(err, state.ErrAppConfigNotFound) {
			return nil, notFound("config not found: " + req.GetName())
		}
		return nil, err
	}
	return &serverv1.GetConfigResponse{
		App:       app.Name,
		Name:      row.Name,
		Value:     row.Value,
		Hash8:     row.Hash8,
		UpdatedAt: timestamppb.New(row.UpdatedAt),
	}, nil
}

// RemoveConfig 删除单条（幂等不做：不存在 404——与 RemoveSecret 同口径）。
// 已被运行中服务引用的 removal 不追写部署——引用方下次部署 preflight
// E_CONFIG_NOT_FOUND 诚实失败（移除声明再部署的既有语义）。
func (s *ConfigsService) RemoveConfig(ctx context.Context, req *serverv1.RemoveConfigRequest) (*serverv1.RemoveConfigResponse, error) {
	app, err := resolveApp(ctx, s.st, req.GetApp())
	if err != nil {
		return nil, err
	}
	// 角色门（W2-S4 第 2 门）：set/get/remove=admin、list=read（scope 登记映射）。
	if err := requireAppAccess(ctx, s.st, app); err != nil {
		return nil, err
	}
	err = s.st.InTx(ctx, func(tx *state.Tx) error {
		if err := tx.RemoveAppConfig(ctx, app.ID, req.GetName()); err != nil {
			return err
		}
		return tx.WriteAudit(ctx, configAudit(ctx, "config.removed", app.Name, req.GetName(),
			state.DiffSummary("hash8", "")))
	})
	if err != nil {
		if errors.Is(err, state.ErrAppConfigNotFound) {
			return nil, notFound("config not found: " + req.GetName())
		}
		return nil, err
	}
	return &serverv1.RemoveConfigResponse{App: app.Name, Name: req.GetName()}, nil
}

// configAudit 构造配置资源审计条目（T 线 OT-3：动作词表 config.set/
// config.removed；target = config:<app>/<名>——与 secret:<app>/<名> 同型；
// actor 归因同款：API 无法区分人类/AI 代理，token 承载可追溯性）。
func configAudit(ctx context.Context, action, app, name, diff string) state.AuditEntry {
	return databaseAudit(ctx, action, "config:"+app+"/"+name, diff)
}

// validateConfigName 是 config 声明名的 handler 层校验（proto 形状层
// pattern 的兜底镜像；与 internal/compose 的 validConfigName 同规则——
// naming 组件字符集 [A-Za-z0-9._-]、首字符字母数字、≤63，两端注释互指）。
func validateConfigName(name string) error {
	if name == "" || len(name) > 63 {
		return statusInvalidArgument("config name must match ^[A-Za-z0-9][A-Za-z0-9._-]*$ (max 63 chars)")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '_' || r == '-' || r == '.'):
		default:
			return statusInvalidArgument("config name must match ^[A-Za-z0-9][A-Za-z0-9._-]*$ (max 63 chars)")
		}
	}
	return nil
}
