package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/envlayer"
)

// TestResolveControlPlaneInjection 控制面地址注入（ctrlinject.go）：
// ControlGRPCAddr 装配 → 每个服务同一组 system env（GRPC_ADDR + TLS_NAME）；
// 未装配（空）→ nil（零行为变更）；键在保留名字空间（system source）。
func TestResolveControlPlaneInjection(t *testing.T) {
	t.Parallel()

	spec := &compose.Spec{Services: []compose.Service{
		{Name: "web"},
		{Name: "worker"},
	}}

	t.Run("未装配 = 不注入", func(t *testing.T) {
		t.Parallel()
		e := &Engine{cfg: Config{}}
		require.Nil(t, e.resolveControlPlaneInjection(spec))
	})

	t.Run("装配后每服务恒注入两键", func(t *testing.T) {
		t.Parallel()
		e := &Engine{cfg: Config{ControlGRPCAddr: "10.124.0.3:8421", ControlTLSName: "ctrl.dev.fleetly.run"}}
		got := e.resolveControlPlaneInjection(spec)
		require.Len(t, got, 2)
		want := []envlayer.PlatformVar{
			{Key: "FLEETLY_CONTROL_GRPC_ADDR", Value: "10.124.0.3:8421", Source: "system"},
			{Key: "FLEETLY_CONTROL_TLS_NAME", Value: "ctrl.dev.fleetly.run", Source: "system"},
		}
		for _, svc := range []string{"web", "worker"} {
			require.Equal(t, want, got[svc])
		}
	})

	t.Run("TLS off = TLS_NAME 空值如实注入", func(t *testing.T) {
		t.Parallel()
		e := &Engine{cfg: Config{ControlGRPCAddr: "10.124.0.3:8421"}}
		got := e.resolveControlPlaneInjection(spec)
		require.Equal(t, "", got["web"][1].Value)
		require.Equal(t, "system", got["web"][1].Source)
	})
}
