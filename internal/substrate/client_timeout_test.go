package substrate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// D2（S17 类 D：超时与取消闭环）挂起注入测试：假 Docker API 永不返回
// （handler 阻塞在门闸上）——非流式调用必须在注入预算（1s）内以
// DeadlineExceeded 终结，而非无限阻塞（dockerd 假死形态）。

// newHangingDockerAPI 起一个永不响应的假 Docker API（所有路径挂住；
// Cleanup 时放行并关闭，避免泄漏阻塞的 handler goroutine）。
func newHangingDockerAPI(t *testing.T) *Client {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // 挂住：不读不写不返回
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatalf("construct client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestNonStreamingCallDeadlineBound 非流式调用面（D2）：每个包装方法在
// 预算内限时失败。调用方 ctx 给 10s——若 per-call 预算缺失将挂满 10s
// （断言上限 3s 即拦住该回归）。
func TestNonStreamingCallDeadlineBound(t *testing.T) {
	c := newHangingDockerAPI(t)

	orig := defaultCallTimeout
	defaultCallTimeout = time.Second // 测试注入缩短预算
	t.Cleanup(func() { defaultCallTimeout = orig })

	cases := []struct {
		name string
		call func(ctx context.Context) error
	}{
		// client.go：Info / NodeList / NodeInspect / NodeUpdate
		{"SelfNodeID(Info)", func(ctx context.Context) error { _, err := c.SelfNodeID(ctx); return err }},
		{"ListNodeObservations(NodeList)", func(ctx context.Context) error { _, err := c.ListNodeObservations(ctx); return err }},
		{"UpdateNodeLabel(NodeInspect+NodeUpdate)", func(ctx context.Context) error {
			return c.UpdateNodeLabel(ctx, "n1", "k", "v", state.ObjectVersion{})
		}},
		{"ResolveObjectVersion(ServiceInspect)", func(ctx context.Context) error {
			_, err := c.ResolveObjectVersion(ctx, state.ObjectKindService, "web")
			return err
		}},
		// services.go：ServiceCreate/Update/Remove/Inspect/List + TaskList +
		// NetworkEnsure + SwarmReady + ImageDigest
		{"ServiceCreate", func(ctx context.Context) error { return c.ServiceCreate(ctx, engine.ServiceSpec{Name: "web"}) }},
		{"ServiceUpdate(Inspect+Update)", func(ctx context.Context) error {
			return c.ServiceUpdate(ctx, "web", engine.ServiceSpec{Name: "web"})
		}},
		{"ServiceRemove", func(ctx context.Context) error { return c.ServiceRemove(ctx, "web") }},
		{"ServiceInspect", func(ctx context.Context) error { _, err := c.ServiceInspect(ctx, "web"); return err }},
		{"ServiceList", func(ctx context.Context) error { _, err := c.ServiceList(ctx, nil); return err }},
		{"TaskList", func(ctx context.Context) error { _, err := c.TaskList(ctx, "web"); return err }},
		{"NetworkEnsure(Inspect+Create)", func(ctx context.Context) error { return c.NetworkEnsure(ctx, "net-x") }},
		{"SwarmReady(Info)", func(ctx context.Context) error { return c.SwarmReady(ctx) }},
		// networks.go（IMPL-T15-1 项目网对象面）：ensure/list/inspect/remove
		// 四原语同样逐调用独立预算（D2 覆盖面随新原语同步）。
		{"NetworkEnsureWithLabels(Inspect+Create)", func(ctx context.Context) error {
			return c.NetworkEnsureWithLabels(ctx, "net-y", map[string]string{"fleetly.managed": "true"})
		}},
		{"NetworkList", func(ctx context.Context) error { _, err := c.NetworkList(ctx, nil); return err }},
		{"NetworkInspect", func(ctx context.Context) error { _, err := c.NetworkInspect(ctx, "net-y"); return err }},
		{"NetworkRemove", func(ctx context.Context) error { return c.NetworkRemove(ctx, "net-y") }},
		// ImageDigest：IMPL-T1-2 起 tag 引用 registry-first（解析腿不经
		// daemon），本地 inspect 腿的预算断言用平台本地命名空间引用触达
		// （fleetly-local/… 跳过解析腿——v0.1 本地面语义）。
		{"ImageDigest(ImageInspect)", func(ctx context.Context) error {
			_, err := c.ImageDigest(ctx, "fleetly-local/web:web-01")
			return err
		}},
		// images.go：InspectImage / Volume / Container / Tag / Remove。
		// LoadImage 不在本表——H6 修正后属 D2 排除面（装载与 solve 共生命
		// 周期，预算由调用方 ctx 管理，见 TestLoadImageUsesCallerContext）。
		{"InspectImage(ImageInspect)", func(ctx context.Context) error { _, err := c.InspectImage(ctx, "nginx:1"); return err }},
		{"EnsureVolumePresent(Inspect+Create)", func(ctx context.Context) error { return c.EnsureVolumePresent(ctx, "vol") }},
		{"TagImage", func(ctx context.Context) error { return c.TagImage(ctx, "a:1", "b:2") }},
		{"RemoveImage", func(ctx context.Context) error { return c.RemoveImage(ctx, "a:1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			err := tc.call(ctx)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("call not bounded by per-call timeout: elapsed %v (want ≤3s)", elapsed)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("want context.DeadlineExceeded, got %v", err)
			}
		})
	}
}

// TestLoadImageUsesCallerContext D2 排除面（H6 修正）：LoadImage 不再自带
// per-call 30s 预算——装载流与 solve 共生命周期（导出器在 solve 末尾才产
// 流，per-call 预算会全程消耗在等 solve 上，>30s 冷构建在完成构建工作后
// 才失败），装载时长由调用方 ctx（构建路径 = per-build timeout_seconds）
// 治理。挂起 daemon + 即时 reader：POST 挂住，500ms 调用方 deadline 内
// 以 DeadlineExceeded 终结（既未被 30s 预算接管、也非无限挂起——与
// TestPingUsesCallerContext 同款口径）。
func TestLoadImageUsesCallerContext(t *testing.T) {
	c := newHangingDockerAPI(t)

	orig := defaultCallTimeout
	defaultCallTimeout = 30 * time.Second // 显式钉回缺省（防同包前序注入污染）
	t.Cleanup(func() { defaultCallTimeout = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.LoadImage(ctx, strings.NewReader("docker-archive-bytes"))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("image load not bounded by caller context: elapsed %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded (caller ctx), got %v", err)
	}
}

// TestPingUsesCallerContext D2 排除面钉死：Ping（健康探测）不走 per-call
// 预算——由调用方 ctx 管理（500ms 调用方 deadline 内终结，证明既未被
// 30s 预算接管、也非无限挂起）。
func TestPingUsesCallerContext(t *testing.T) {
	c := newHangingDockerAPI(t)

	orig := defaultCallTimeout
	defaultCallTimeout = 30 * time.Second // 显式钉回缺省（防同包前序注入污染）
	t.Cleanup(func() { defaultCallTimeout = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Ping(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ping not bounded by caller context: elapsed %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded (caller ctx), got %v", err)
	}
}
