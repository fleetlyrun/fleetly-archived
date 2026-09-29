package engine

// S3 应用凭证注入（E3-4，设计 §2.4/D-S3-6）：compose 服务 label
// fleetly.s3=true → 发布引擎为**该服务**注入一组 system env（env 三层合并
// 链预留 source=system 的第一个生产者——FZ-1 数据库连接串 W4 走同一机制）：
//
//	S3_ENDPOINT / S3_REGION / S3_BUCKET / S3_ACCESS_KEY_ID /
//	S3_SECRET_ACCESS_KEY / S3_PATH_STYLE（词表 §5.4，只增）
//
// 值来源：external = 设置值直出（secret 密文解密）；rustfs = 服务端派生
// 端点 + 平台托管凭据（E3-5 rustfs 管理器生成，envelope 解密）。仅带 label 的服务
// 注入（单桶单凭据共享——D-S3-10）；同键覆盖走 W_ENV_PLATFORM_OVERRIDE
// 既有警告（system > platform > 文件层既定序）。
//
// 前置校验（诚实拒绝，§2.4）：label 出现而 s3.mode=unset → 规划期
// E_S3_NOT_CONFIGURED（部署不触底座，不注入空 env 让应用谜之失败）。
//
// 网络牵线（rustfs mode 独有）：带 label 的服务 spec 附加
// fleetly-rustfs-net（应用→RustFS 单向可达）；external 模式不加（应用自
// 行出网到外部端点）。这是 V2-5「平台牵线共享网络」的 W3 窄面。
//
// 生效语义（与 `fleetly env set` 同型）：注入 env 参与规划快照与
// desired-hash（key+sha256+source=system 脱敏形态）；s3 设置变更后已部署
// 应用**不自动重部署**——引擎只在发布装配时取当前设置（prepareInputs 现
// 读），下一次部署自然生效。rustfs 凭据轮换（禁用→再启用再生成）同语义。
//
// 明文纪律：解密后的凭据只进 envlayer.MergeChain 的明文值注入路径与密文
// 快照，不进日志/事件/审计/错误。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/envlayer"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// errS3WaitingRustfsCredentials 是 rustfs 托管凭据尚未备便的可重试哨兵
//（mode=rustfs 刚保存、rustfs 管理器尚未跑完生成拍）：部署停留 preparing 下一拍
// 重试（预算由既有 preparing 看门狗守门——与 swarm 未就绪同型暂态）。
var errS3WaitingRustfsCredentials = errors.New(
	"s3 injection: managed rustfs credentials not provisioned yet (the rustfs manager provisions them within a minute of enabling rustfs mode)")

// s3SystemVars 是注入键的固定词表（§5.4：六键恒注入——键集确定 = 快照
// 与 desired-hash 确定；空值如实注入，消费方按值裁决）。
func s3SystemVars(endpoint, region, bucket, accessKey, secretKey string, pathStyle bool) []envlayer.PlatformVar {
	return []envlayer.PlatformVar{
		{Key: "S3_ENDPOINT", Value: endpoint, Source: string(envlayer.SourceSystem)},
		{Key: "S3_REGION", Value: region, Source: string(envlayer.SourceSystem)},
		{Key: "S3_BUCKET", Value: bucket, Source: string(envlayer.SourceSystem)},
		{Key: "S3_ACCESS_KEY_ID", Value: accessKey, Source: string(envlayer.SourceSystem)},
		{Key: "S3_SECRET_ACCESS_KEY", Value: secretKey, Source: string(envlayer.SourceSystem)},
		{Key: "S3_PATH_STYLE", Value: strconv.FormatBool(pathStyle), Source: string(envlayer.SourceSystem)},
	}
}

// resolveS3Injection 解析本次发布的 S3 注入面（preparing 现读设置，不缓存
// 长驻——保存即对下一次装配生效）。返回：
//   - varsByService：带 label 服务 → system env（无 label 服务零注入；
//     app 全体无 label 时为空、不读设置——未用 S3 的部署零额外读取）；
//   - attachRustfs：rustfs 模式网络牵线开关（仅 rustfs mode 为 true）；
//   - err：E_S3_NOT_CONFIGURED（unset 诚实拒绝）或凭据解密失败；托管凭据
//     未备便返回可重试哨兵（调用方停留 preparing 下一拍重试）。
func (e *Engine) resolveS3Injection(ctx context.Context, spec *compose.Spec) (varsByService map[string][]envlayer.PlatformVar, attachRustfs bool, err error) {
	labeled := 0
	for i := range spec.Services {
		if spec.Services[i].S3 {
			labeled++
		}
	}
	if labeled == 0 {
		return nil, false, nil
	}
	in, err := e.store.LoadS3Settings(ctx)
	if err != nil {
		return nil, false, errorf("E_RUNTIME_UNAVAILABLE", "failed to load s3 settings: %v", err)
	}
	var vars []envlayer.PlatformVar
	switch state.NormalizeMode(in.Mode) {
	case state.S3ModeUnset:
		// 前置校验（§2.4）：label 出现而未配置对象存储 → 规划期诚实拒绝
		//（S1 注册码 E_S3_NOT_CONFIGURED 的消费点）。
		return nil, false, apperr.New("E_S3_NOT_CONFIGURED",
			"%d service(s) declare label %q but object storage is not configured (s3.mode=unset): "+
				"configure an external endpoint or enable the managed rustfs first, then deploy again",
			labeled, "fleetly.s3")
	case state.S3ModeExternal:
		secret := ""
		if in.SecretAccessKey != "" {
			plain, derr := e.box.Decrypt([]byte(in.SecretAccessKey))
			if derr != nil {
				return nil, false, errorf("E_RUNTIME_UNAVAILABLE",
					"failed to decrypt s3 secret for injection (master key mismatch or corrupted ciphertext)")
			}
			secret = string(plain)
		}
		vars = s3SystemVars(strings.TrimSpace(in.EndpointURL), strings.TrimSpace(in.Region),
			strings.TrimSpace(in.Bucket), strings.TrimSpace(in.AccessKeyID), secret, in.PathStyle)
	case state.S3ModeRustfs:
		accessCT, secretCT, found, lerr := e.store.LoadRustfsCredentialsCiphertext(ctx)
		if lerr != nil {
			return nil, false, errorf("E_RUNTIME_UNAVAILABLE", "failed to load managed rustfs credentials: %v", lerr)
		}
		if !found {
			return nil, false, errS3WaitingRustfsCredentials // 可重试：生成拍未到
		}
		accessPlain, derr := e.box.Decrypt([]byte(accessCT))
		if derr != nil {
			return nil, false, errorf("E_RUNTIME_UNAVAILABLE",
				"failed to decrypt managed rustfs access key (master key mismatch or corrupted ciphertext)")
		}
		secretPlain, derr := e.box.Decrypt([]byte(secretCT))
		if derr != nil {
			return nil, false, errorf("E_RUNTIME_UNAVAILABLE",
				"failed to decrypt managed rustfs secret key (master key mismatch or corrupted ciphertext)")
		}
		// 派生端点 + 平台凭据 + path-style（托管端点固定内网形态）。
		vars = s3SystemVars(state.RustfsEndpointURL, "", state.RustfsBucketName,
			string(accessPlain), string(secretPlain), true)
		attachRustfs = true
	default:
		return nil, false, fmt.Errorf("engine: unknown s3.mode %q", in.Mode)
	}
	varsByService = make(map[string][]envlayer.PlatformVar, labeled)
	for i := range spec.Services {
		if spec.Services[i].S3 {
			varsByService[spec.Services[i].Name] = vars
		}
	}
	return varsByService, attachRustfs, nil
}
