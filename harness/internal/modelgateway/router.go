package modelgateway

import (
	"context"
	"fmt"
)

// ModelTarget 指定一次调用的模型 + 下游 provider。
type ModelTarget struct {
	Model      string
	Provider   string
	Cost       ModelCostTable
	Capability ModelCapability
}

// Route 是一次请求解析出的执行计划:首选 + 降级链(可跨 provider)。
type Route struct {
	Primary  ModelTarget
	Fallback []ModelTarget
}

// ModelRouter 把请求解析成 Route(极简策略层,替代原来的打分/能力清单引擎)。
type ModelRouter interface {
	Route(ctx context.Context, req ModelRequest) (Route, error)
}

// StaticRouter 是配置驱动的最简 ModelRouter:按 agent_id 命中,否则回退 Default。
type StaticRouter struct {
	Routes  map[string]Route // key = agent_id
	Default Route
}

// NewStaticRouter 构建一个静态路由器。
func NewStaticRouter(def Route, routes map[string]Route) *StaticRouter {
	return &StaticRouter{Routes: routes, Default: def}
}

func (r *StaticRouter) Route(_ context.Context, req ModelRequest) (Route, error) {
	if rt, ok := r.Routes[req.AgentID]; ok {
		return rt, nil
	}
	if r.Default.Primary.Model != "" {
		return r.Default, nil
	}
	return Route{}, fmt.Errorf("modelgateway: no route for agent %q", req.AgentID)
}
