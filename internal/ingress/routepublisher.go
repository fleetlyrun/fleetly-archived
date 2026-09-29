package ingress

import (
	"context"

	"github.com/fleetlyrun/fleetly/internal/engine"
)

// RoutePublisher 以 Manager 承载引擎的路由发布端口（健康门后挂点，T2.15
// ——只在发布切流成功后发布域名路由，架构 §2.5 路由时机不变量）。
//
// 2026-09-29 架构评审 C6 归位：原 runtime/provides.go 的装配层内联 adapter
// 搬入属主包，装配文件回归纯接线；方向纪律同 substrate/dbtemplate/metrics
// ——域模块 import engine 实现其端口，engine 不 import ingress。载荷转换：
// 引擎侧 engine.RoutePublishInput（核心类型）→ PublishInput（适配器类型）
// ——核心不感知适配器类型，转换只在本包。
type RoutePublisher struct {
	m *Manager
}

// NewRoutePublisher 构造发布器（装配层注入 engine.WithRoutePublisher）。
func NewRoutePublisher(m *Manager) RoutePublisher { return RoutePublisher{m: m} }

// PublishRoutes 实现 engine.RoutePublisher。
func (p RoutePublisher) PublishRoutes(ctx context.Context, in engine.RoutePublishInput) error {
	out := PublishInput{AppID: in.AppID, AppName: in.AppName, TeamSlug: in.TeamSlug, PrjSlug: in.PrjSlug}
	for _, svc := range in.Declared {
		out.Declared = append(out.Declared, ServiceRoutes{
			Service: svc.Service, Port: svc.Port, Domains: svc.Domains,
		})
	}
	return p.m.PublishRoutes(ctx, out)
}

// 编译期断言：满足引擎端口（第三方载荷不出适配器的结构性证明）。
var _ engine.RoutePublisher = RoutePublisher{}
