package runtime

// API token 种子（T2.17 安装引导语义）：首次启动无任何 token（含已吊销
// ——「存在过」即不算首次）时生成 bootstrap admin token 并入审计
// （token.create，ActorTokenID 空 = bootstrap 自举）。
//
// B5（出站字节出口收口）：token 本体不再打印进启动日志/journald（systemd
// 形态下 journald 持久留存明文凭据）——改为写 <数据根>/bootstrap-token
// 文件（0600 + fsync），日志只报路径与操作指引（首次成功登录后删除）。
// 文件已存在则不重复生成（幂等）；库内仍只存 sha256 哈希。

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/lynx-go/lynx"

	"github.com/fleetlyrun/fleetly/internal/api"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// NewAuthenticator 构造 API 认证器并承载 bootstrap 种子（装配期执行——
// 失败 fail-fast 拒绝启动：鉴权面就绪是 API 面可用的前置）。cfg 提供
// bootstrap token 文件路径（B5：<数据根>/bootstrap-token，数据根与 state
// 库同目录）与会话滑动窗口 TTL（auth.session_ttl_hours，W2-S4 注入面——
// 认证拍的滑动续期与登录面同参数）。
func NewAuthenticator(app lynx.App, cfg *AppConfig, st *state.Store) (*api.Authenticator, error) {
	if err := bootstrapAdminToken(app.Logger(), cfg.BootstrapTokenPath(), st); err != nil {
		return nil, err
	}
	return api.NewAuthenticator(st).WithSessionTTL(cfg.SessionTTL()), nil
}

// bootstrapAdminToken 首启种子（B5：token 写文件不进日志——测试断言日志
// 全文无 flt_ 前缀、文件本体可读）。
func bootstrapAdminToken(log *slog.Logger, path string, st *state.Store) error {
	any, err := st.HasAnyToken(context.Background())
	if err != nil {
		return err
	}
	if any {
		return nil // 已有 token（含历史吊销）：不重复引导
	}
	// 幂等：token 文件在（上一次首启已落盘）则不重复生成——库内无 token
	// 但文件存在的形态（用户手动清库）复用文件内凭据的哈希语义由登录面
	// 裁决，这里只保证不二次签发。
	if _, err := os.Stat(path); err == nil {
		log.Info("bootstrap token file already exists, skipping generation (delete it after the first successful login)", "path", path)
		return nil
	}
	plaintext, err := api.GenerateBootstrapAdminToken(context.Background(), st, state.BootstrapTokenName)
	if err != nil {
		return err
	}
	if err := writeBootstrapTokenFile(path, plaintext); err != nil {
		return fmt.Errorf("write bootstrap token file %s: %w", path, err)
	}
	log.Info("bootstrap admin token generated and written to file (secret not printed; delete the file after the first successful login)", "path", path)
	return nil
}

// writeBootstrapTokenFile 把 token 明文写 0600 文件并 fsync（B5：崩溃窗口
// 内不落半行；O_EXCL 防并发双写）。
func writeBootstrapTokenFile(path, plaintext string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304：路径来自平台配置的数据根，0600 独占创建
	if err != nil {
		return err
	}
	if _, err := f.WriteString(plaintext + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
