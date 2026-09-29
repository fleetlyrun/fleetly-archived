package state

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/apperr"
)

// platform_settings 表读写（E3 对象存储专项设计 §2.2/D-S3-2）：S3 端点
// 配置的库内运行期设置（不落 config.yaml——Console/CLI 配置 → 测试连接
// → 保存即生效，与 domains/env/tokens 同为「用户运行期资源」类）。
//
// 词表只增（§2.2）：键名常量为本包唯一登记点。s3.secret_access_key 存
// envelope 密文（internal/secrets）——**state 层存密文不解释**：加密边界
// 在调用方（internal/api），与 env_vars 同型纪律；明文绝不进日志/事件/
// 审计/错误（state-model §2.9，负面测试钉死）。
//
// 变更路径（§2.2）：API 写入（本文件 SaveS3Settings，校验 + 审计 + 事件
// 同事务 fail-closed）→ 备份上传轨与新部署在每次装配时取当前值（读取点
// 不缓存长驻；消费者在 E3-3/E3-4）。

// s3.* 设置键词表（只增）。
const (
	S3KeyMode            = "s3.mode"
	S3KeyEndpointURL     = "s3.endpoint_url"
	S3KeyRegion          = "s3.region"
	S3KeyBucket          = "s3.bucket"
	S3KeyAccessKeyID     = "s3.access_key_id"
	S3KeySecretAccessKey = "s3.secret_access_key"
	S3KeyPathStyle       = "s3.path_style"
	S3KeyPublicExposed   = "s3.public_exposed"

	// S3KeyResticPassword 是 restic repo 口令的**内部键**（E3-3/D-S3-5，
	// 设计 §2.3）：存 envelope 密文（internal/secrets），上传轨惰性生成。
	// 内部键不属于八键 typed 设置面（SaveS3Settings 的 PUT 全量键集不含
	// 它、LoadS3Settings 的投影不读它）——它没有用户可见的读写面，只被
	// 上传轨消费（与 s3.* 用户设置同用 platform_settings 存储是物理复用，
	// 非语义同族）。词表只增纪律下登记于此（键名常量唯一登记点）。
	S3KeyResticPassword = "s3.restic_password" //nolint:gosec // G101：设置键名字面量，非凭据材料
)

// s3.mode 词表（§2.2）：缺省 unset。
const (
	S3ModeUnset    = "unset"
	S3ModeExternal = "external"
	S3ModeRustfs   = "rustfs"
)

// rustfs 模式的服务端派生端点（设计 §2.5：fleetlyd 所在网络内
// http://rustfs:9000，path-style；平台单桶）。S3 各消费面共用（api 探针
// storedS3Endpoint、备份上传轨 resticTarget——E3-3，凭证注入引擎面——E3-4），
// 单一事实源在本包。RustfsNetworkName 是托管 RustFS 的内部 overlay 网络
//（E3-5 管理器创建，attachable——restic 上传轨一次性容器经它入网）；
// 应用注入面（E3-4）与上传轨消费同名网络。端点 host 段 `rustfs` 是
// network alias（internal/rustfs 管理器的服务 alias），两者必须一致——
// internal/rustfs 的测试钉住该不变量。
const (
	RustfsEndpointURL = "http://rustfs:9000"
	RustfsBucketName  = "fleetly"
	RustfsNetworkName = "fleetly-rustfs-net"
)

// s3SettingsKeys 是保存时全量落库的键清单（PUT 语义：每次保存写全八键，
// 未提供的键回落空值——避免「改了 mode 残留旧凭证」的静默状态）。
var s3SettingsKeys = []string{
	S3KeyMode, S3KeyEndpointURL, S3KeyRegion, S3KeyBucket,
	S3KeyAccessKeyID, S3KeySecretAccessKey, S3KeyPathStyle, S3KeyPublicExposed,
}

// S3Settings 是 s3.* 设置的 typed 视图。SecretAccessKey 为**存储形态**
// （envelope 密文，由调用方加密/解密）；Mode 缺省 unset。
type S3Settings struct {
	Mode            string
	EndpointURL     string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string // 密文（存储形态）；空 = 未设置
	PathStyle       bool
	PublicExposed   bool
	// UpdatedAt 是各设置行 updated_at 的最大值（只读投影）。
	UpdatedAt time.Time
}

// NormalizeMode 把空 mode 归一为 unset（proto 请求缺省语义）。
func NormalizeMode(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return S3ModeUnset
	}
	return mode
}

// ValidateS3Settings 是保存前互斥校验（fail-fast，设计 §2.2）：
//   - mode=external：endpoint_url/bucket/access_key_id/secret_access_key 必填；
//   - mode=rustfs：这四项必须为空（托管与自管是两种互斥事实源，配了即
//     E_S3_CONFIG_CONFLICT，拒绝保存）；
//   - public_exposed=true 仅 mode=rustfs 允许，且需平台域名（baseDomain
//     非空），否则 E_S3_PUBLIC_REQUIRES_BASE_DOMAIN（§2.6）。
//
// mode=unset 不约束四项字段（设计未裁决 unset 的字段残留；PUT 语义下
// 保存即全量覆写，残留面由调用方形态决定——保守不做额外拒绝）。
func ValidateS3Settings(in S3Settings, baseDomain string) error {
	mode := NormalizeMode(in.Mode)
	externalRequired := []struct {
		name string
		val  string
	}{
		{"s3.endpoint_url", in.EndpointURL},
		{"s3.bucket", in.Bucket},
		{"s3.access_key_id", in.AccessKeyID},
		{"s3.secret_access_key", in.SecretAccessKey},
	}
	switch mode {
	case S3ModeUnset:
		// 缺省态：无互斥约束。
	case S3ModeExternal:
		for _, f := range externalRequired {
			if strings.TrimSpace(f.val) == "" {
				return apperr.New("E_S3_CONFIG_CONFLICT",
					"s3.mode=external requires %s to be set (all of endpoint_url/bucket/access_key_id/secret_access_key)", f.name)
			}
		}
	case S3ModeRustfs:
		for _, f := range externalRequired {
			if strings.TrimSpace(f.val) != "" {
				return apperr.New("E_S3_CONFIG_CONFLICT",
					"s3.mode=rustfs requires %s to be empty (managed RustFS owns its credentials; %s is set)", f.name, f.name)
			}
		}
	default:
		return fmt.Errorf("state: s3.mode %q not in {unset, external, rustfs}", in.Mode)
	}
	if in.PublicExposed {
		if mode != S3ModeRustfs {
			return apperr.New("E_S3_CONFIG_CONFLICT",
				"s3.public_exposed=true is only allowed with s3.mode=rustfs (external endpoints are already public)")
		}
		if strings.TrimSpace(baseDomain) == "" {
			return apperr.New("E_S3_PUBLIC_REQUIRES_BASE_DOMAIN",
				"s3.public_exposed=true requires a platform base domain (the public subdomain s3.<base> derives from it)")
		}
	}
	return nil
}

// LoadS3Settings 读取全部 s3.* 设置（读取点每次调用现读，不缓存长驻——
// §2.2 变更路径：保存即对下一次装配生效）。空库/无行 → Mode=unset 缺省态。
// 存储值畸形（布尔非 true/false）显式报错：设置损坏 loud-fail，不静默回落。
func (s *Store) LoadS3Settings(ctx context.Context) (S3Settings, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, updated_at FROM platform_settings WHERE key LIKE 's3.%'`)
	if err != nil {
		return S3Settings{}, fmt.Errorf("state: query s3 settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	vals := map[string]string{}
	var latest int64
	for rows.Next() {
		var key, value string
		var updatedAt int64
		if err := rows.Scan(&key, &value, &updatedAt); err != nil {
			return S3Settings{}, fmt.Errorf("state: scan s3 setting: %w", err)
		}
		vals[key] = value
		if updatedAt > latest {
			latest = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return S3Settings{}, fmt.Errorf("state: iterate s3 settings: %w", err)
	}
	out := S3Settings{
		Mode:            NormalizeMode(vals[S3KeyMode]),
		EndpointURL:     vals[S3KeyEndpointURL],
		Region:          vals[S3KeyRegion],
		Bucket:          vals[S3KeyBucket],
		AccessKeyID:     vals[S3KeyAccessKeyID],
		SecretAccessKey: vals[S3KeySecretAccessKey], // 密文原样（本层不解释）
	}
	if out.PathStyle, err = parseBoolSetting(S3KeyPathStyle, vals[S3KeyPathStyle]); err != nil {
		return S3Settings{}, err
	}
	if out.PublicExposed, err = parseBoolSetting(S3KeyPublicExposed, vals[S3KeyPublicExposed]); err != nil {
		return S3Settings{}, err
	}
	out.UpdatedAt = time.Unix(0, latest).UTC()
	return out, nil
}

// parseBoolSetting 解析布尔设置值（空 = false；值域 {true,false}，畸形
// 显式报错）。
func parseBoolSetting(key, value string) (bool, error) {
	switch value {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("state: setting %s has invalid boolean value %q", key, value)
	}
}

// S3SaveOptions 是保存的上下文选项：baseDomain 是公网开关门禁判定面
// （daemon 配置；空 = 单节点形态）；actor/actorTokenID 进审计（API 面传
// 调用者身份，同事务 fail-closed）。
type S3SaveOptions struct {
	BaseDomain   string
	Actor        string // "human"（API 面）——审计 actor 词表不扩
	ActorTokenID string
}

// SaveS3Settings 全量保存 s3.* 设置（PUT 语义）+ 审计 + 事件（同一事务，
// fail-closed）。SecretAccessKey 必须已由调用方 envelope 加密（密文入参
// ——本层不解释密文，也绝不把任何设置值拼进错误/审计/事件：审计 diff 与
// 事件 payload 只带 mode/public_exposed，凭证材料零出现）。
func (s *Store) SaveS3Settings(ctx context.Context, in S3Settings, opts S3SaveOptions) error {
	in.Mode = NormalizeMode(in.Mode)
	if err := ValidateS3Settings(in, opts.BaseDomain); err != nil {
		return err
	}
	values := map[string]string{
		S3KeyMode:            in.Mode,
		S3KeyEndpointURL:     in.EndpointURL,
		S3KeyRegion:          in.Region,
		S3KeyBucket:          in.Bucket,
		S3KeyAccessKeyID:     in.AccessKeyID,
		S3KeySecretAccessKey: in.SecretAccessKey,
		S3KeyPathStyle:       strconv.FormatBool(in.PathStyle),
		S3KeyPublicExposed:   strconv.FormatBool(in.PublicExposed),
	}
	err := s.InTx(ctx, func(tx *Tx) error {
		now := nowNano()
		for _, key := range s3SettingsKeys {
			const q = `INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
			if _, err := tx.ExecContext(ctx, q, key, values[key], now); err != nil {
				return fmt.Errorf("state: upsert s3 setting %s: %w", key, err)
			}
		}
		// 审计（§2.2 变更路径）：action = s3.updated；diff 摘要只含模式与
		// 公网开关——设置值（尤其 secret 明文/密文）零出现。
		if err := tx.WriteAudit(ctx, AuditEntry{
			Actor:        opts.Actor,
			ActorTokenID: opts.ActorTokenID,
			Action:       "s3.updated",
			Target:       "platform:s3",
			Result:       "ok",
			DiffSummary: DiffSummary("mode", in.Mode, "public_exposed", in.PublicExposed,
				"path_style", in.PathStyle),
		}); err != nil {
			return err
		}
		// 事件 s3.updated（eventcode 注册表内；payload 带模式不带走秘密，
		// §5.3）：与业务写同事务 = Outbox 模式。
		if _, err := tx.AppendEvent(ctx, Event{
			Name:    "s3.updated",
			Subject: "platform:s3",
			Payload: DiffSummary("mode", in.Mode, "public_exposed", in.PublicExposed),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("state: save s3 settings: %w", err)
	}
	return nil
}
