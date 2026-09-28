package fleetly

// 构建等待面（IMPL-ARCH-K，2026-09-28 架构评审候选 9 后半）：把「等构建
// 终态」的轮询环收编进 SDK——此前 CLI build 命令自持一份（1s GetBuild 轮
// 询 + 自有超时文案），外部消费者被迫各自重写。终态判定谓词 BuildTerminal
// 是消费侧单点；状态词表以 proto BuildView.status 注释为唯一契约源
//（queued/building/succeeded/failed，与服务端 internal/state builds.go 的
// 常量同词表）。
//
// 服务端流式变体不共用本环：BuildFromUpload 的服务端等待（internal/api/
// builds.go waitUploadBuild）占着 gRPC 流、直读 state.Store（非 gRPC
// GetBuild）、且谓词消费类型化常量——机制不同构；服务端 internal 包也不可
// 反向 import 公共 SDK（依赖方向倒置）。两侧取舍理由以注释互相指认，词表
// 漂移由 proto 契约测试兜底。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
)

// DefaultWaitPollInterval 是 WaitBuild 的缺省轮询周期（与 CLI build 命令
// 收编前的 1s 对齐；服务端流内变体用 500ms——占流等待要更低时延，理由见
// waitUploadBuild 注释）。
const DefaultWaitPollInterval = time.Second

// BuildTerminal 报告构建状态是否终态。词表 = proto BuildView.status（封闭
// 四词：queued/building = 非终态，succeeded/failed = 终态）；词表外取值
//（含空串）按终态处理——与 CLI 收编前的等待环一致，宁可立刻暴露未知状态
// 也不静默挂死调用方（服务端加新终态词时旧 SDK 自动跟随，不为等待环强制
// 升级）。
func BuildTerminal(status string) bool {
	switch status {
	case "queued", "building":
		return false
	default:
		// succeeded/failed 及一切词表外取值。
		return true
	}
}

// ErrWaitTimeout 是 WaitBuild 超时的哨兵（errors.Is 匹配；场景细节经
// *WaitTimeoutError 用 errors.As 取回）。
var ErrWaitTimeout = errors.New("build wait timed out")

// WaitTimeoutError 携带 WaitBuild 超时时刻的场景：Timeout 是生效的等待上
// 限；Pending 是仍处非终态的构建的最后已知状态（build id → 状态词表）——
// 调用方据此区分「全部仍 queued（执行侧没在跑）」与「有 in-flight（执行
// 卡住）」两类可行动提示（CLI build 命令的消费形态）。
type WaitTimeoutError struct {
	Timeout time.Duration
	Pending map[string]string
}

func (e *WaitTimeoutError) Error() string {
	ids := make([]string, 0, len(e.Pending))
	for id := range e.Pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+"="+e.Pending[id])
	}
	return fmt.Sprintf("build wait timed out after %s: %d build(s) still non-terminal (%s)",
		e.Timeout, len(e.Pending), strings.Join(parts, ", "))
}

// Unwrap 暴露哨兵（errors.Is(ErrWaitTimeout) 的通路）。
func (e *WaitTimeoutError) Unwrap() error { return ErrWaitTimeout }

// WaitOption 修改 WaitBuild 的等待参数。
type WaitOption func(*waitConfig)

type waitConfig struct {
	timeout      time.Duration
	pollInterval time.Duration
	onTerminal   func(*serverv1.BuildView) error
}

// WithWaitTimeout 设置总等待上限；非正值（含零值）= 不限时（仅受 ctx 收
// 口）。超时返回部分结果与 *WaitTimeoutError。
func WithWaitTimeout(timeout time.Duration) WaitOption {
	return func(cfg *waitConfig) { cfg.timeout = timeout }
}

// WithWaitPollInterval 覆盖轮询周期；非正值回落 DefaultWaitPollInterval。
func WithWaitPollInterval(interval time.Duration) WaitOption {
	return func(cfg *waitConfig) {
		if interval > 0 {
			cfg.pollInterval = interval
		}
	}
}

// WithWaitOnTerminal 注册终态回调：每条构建到终态时按完成序逐条调用（CLI
// 借此保持「一条构建一行」的流式输出——等全部结束再打印会丢进度可见性）。
// 回调返回非 nil 即中止等待（已到终态的结果仍随返回值交还）。
func WithWaitOnTerminal(onTerminal func(*serverv1.BuildView) error) WaitOption {
	return func(cfg *waitConfig) { cfg.onTerminal = onTerminal }
}

// WaitBuild 轮询 GetBuild 直至 buildIDs 全部到终态，按完成序返回终态行。
// 重复 id 去重；空输入返回 (nil, nil)，不发起任何 RPC。首个轮询即刻发生
// （先问再等——全部已终态时零延迟返回）。
//
// 错误语义（本票裁决，单点注释）：
//   - 任一构建未到终态时 GetBuild 报错：中止整个等待，原样返回已收集的终
//     态结果与该错误——瞬时网络故障不被静默空转吞掉，是否重试由调用方裁；
//   - 已到终态的构建不再参与轮询，后续错误不可能反噬已记录的终态结果；
//   - 超时：返回部分结果与 *WaitTimeoutError（errors.Is ErrWaitTimeout）；
//   - ctx 结束：返回部分结果与 ctx.Err() 本尊（不再包裹，调用方
//     errors.Is 判定取消形态的通路干净）。
func (c *Client) WaitBuild(ctx context.Context, buildIDs []string, options ...WaitOption) ([]*serverv1.BuildView, error) {
	if len(buildIDs) == 0 {
		return nil, nil
	}
	cfg := waitConfig{pollInterval: DefaultWaitPollInterval}
	for _, opt := range options {
		opt(&cfg)
	}
	var deadline <-chan time.Time
	if cfg.timeout > 0 {
		timer := time.NewTimer(cfg.timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	pending := make(map[string]bool, len(buildIDs))
	// lastStatus 记录各 id 最近一次问询到的状态（超时快照的原料；初值空串
	// 不可达——超时分支只在至少一整轮问询之后才可能触发）。
	lastStatus := make(map[string]string, len(buildIDs))
	for _, id := range buildIDs {
		pending[id] = true
		lastStatus[id] = ""
	}
	done := make([]*serverv1.BuildView, 0, len(buildIDs))
	for {
		for id := range pending {
			resp, err := c.builds.GetBuild(ctx, &serverv1.GetBuildRequest{Id: id})
			if err != nil {
				return done, err
			}
			rec := resp.GetBuild()
			lastStatus[id] = rec.GetStatus()
			if !BuildTerminal(rec.GetStatus()) {
				continue
			}
			delete(pending, id)
			done = append(done, rec)
			if cfg.onTerminal != nil {
				if err := cfg.onTerminal(rec); err != nil {
					return done, err
				}
			}
		}
		if len(pending) == 0 {
			return done, nil
		}
		select {
		case <-deadline:
			stillPending := make(map[string]string, len(pending))
			for id := range pending {
				stillPending[id] = lastStatus[id]
			}
			return done, &WaitTimeoutError{Timeout: cfg.timeout, Pending: stillPending}
		case <-ctx.Done():
			return done, ctx.Err()
		case <-time.After(cfg.pollInterval):
		}
	}
}
