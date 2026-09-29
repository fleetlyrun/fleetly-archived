// Package gitserver 是 webhook 部署触发入口的 daemon 侧实现（T2.19；
// git push(SSH) 收包半 2026-09-29 移除，ADR-0012——bare 仓库保留为拉源
// fetch 落点与 compose 读取源）：
//
//   - webhook（gateway 原生 HTTP handler 消费本包 Handler）：强制 HMAC-
//     SHA256 验签 + delivery ID TTL 防重放 + (app, sha) 幂等去重 + 拉源
//     （把 source fetch 进 bare 仓库）；
//   - DeployFromCommit：compose 真源在 git 对象库（`git show
//     <sha>:compose.{yaml,yml}`），不信任客户端传字节。
//
// 包纪律：不 import 任何框架类型（lynx.Service 壳在 cmd/fleetlyd）；git
// 操作全部 exec 系统 git（宿主 git 为前置条件，不引入 go-git 等重框架）。
package gitserver

import (
	"errors"
	"time"
)

// 安全默认基线：重放窗口默认 15 分钟。
const (
	// DefaultReplayTTL 是 webhook delivery ID 防重放缓存窗口（config
	// webhook.replay_ttl_seconds 缺省 900s = 15 分钟）。
	DefaultReplayTTL = 15 * time.Minute
	// TimestampWindow 是自定义投递方时间戳头（X-Fleetly-Timestamp）的
	// 接受窗口（±5 分钟）——绑定口径，不可配置。
	TimestampWindow = 5 * time.Minute
)

// Config 是 webhook 触发入口配置（config 键 git.* 残余与 webhook.* 归并
// 承载——webhook.* 只有重放窗口一键，随本配置节装配）。
type Config struct {
	// Root 是 bare 仓库根目录（git.root；缺省与 state 库同目录下 git/，
	// 由装配方计算回落——本包不感知 db 路径）。
	Root string
	// ReplayTTL 是 webhook delivery ID 防重放窗口
	//（webhook.replay_ttl_seconds；缺省 15 分钟）。
	ReplayTTL time.Duration
}

// Normalize 回落缺省值（装配期调用；Root 为空不在此兜底——它依赖 state
// 库路径，由 cmd/fleetlyd 装配点计算，此处仅在 Validate 校验非空）。
func (c Config) Normalize() Config {
	if c.ReplayTTL <= 0 {
		c.ReplayTTL = DefaultReplayTTL
	}
	return c
}

// Validate 配置完整性检查（装配期 fail-fast）。
func (c Config) Validate() error {
	if c.Root == "" {
		return errors.New("gitserver: git.root is empty")
	}
	return nil
}
