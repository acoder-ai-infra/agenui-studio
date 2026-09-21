package modelgateway

import (
	"context"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// TenantFacadeResolver 可选地在请求时按租户提供 Facade,支持运行时可编辑的租户
// 模型配置。返回 ok=false 表示该租户无托管配置,回退到启动时的 Tenants/Default。
type TenantFacadeResolver interface {
	Facade(ctx context.Context, tenantID string) (*Facade, bool, error)
}

// TenantGateway 按租户 ID 分发到对应的 Facade。
// 每个租户拥有独立的 provider 注册表和路由策略,实现租户级 provider 隔离。
// 未命中的租户回退到 Default Facade(若配置)。
type TenantGateway struct {
	Tenants  map[string]*Facade   // key = tenant_id;启动时从配置固化
	Default  *Facade              // 未命中租户时的兜底
	Resolver TenantFacadeResolver // 可选;先于静态 Tenants 咨询,命中则用托管配置
}

var _ ModelGateway = (*TenantGateway)(nil)

// lookup 解析某租户的 Facade:先咨询 Resolver(运行时托管配置),未命中回退到
// 启动时的静态 Tenants,再回退 Default。
func (g *TenantGateway) lookup(ctx context.Context, tenantID string) (*Facade, bool, error) {
	if g.Resolver != nil {
		f, ok, err := g.Resolver.Facade(ctx, tenantID)
		if err != nil {
			return nil, false, err
		}
		if ok != (f != nil) {
			return nil, false, fmt.Errorf("modelgateway: tenant facade resolver returned inconsistent hit state")
		}
		if ok {
			return f, true, nil
		}
	}
	if f, ok := g.Tenants[tenantID]; ok {
		return f, true, nil
	}
	if g.Default != nil {
		return g.Default, true, nil
	}
	return nil, false, nil
}

// Chat 按请求上下文中的 TraceContext.TenantID 选择对应的 Facade 执行模型调用。
func (g *TenantGateway) Chat(ctx context.Context, req ModelRequest) (*ModelCall, error) {
	trace, _, _, err := bindTrustedTrace(ctx, req.Trace)
	if err != nil {
		return nil, err
	}
	req.Trace = trace
	f, ok, err := g.lookup(ctx, trace.TenantID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("modelgateway: no provider configured for tenant %q", trace.TenantID)
	}
	return f.Chat(ctx, req)
}

func (g *TenantGateway) Embedding(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error) {
	f, trace, err := g.facade(ctx, req.Trace)
	if err != nil {
		return nil, err
	}
	req.Trace = trace
	return f.Embedding(ctx, req)
}

func (g *TenantGateway) Rerank(ctx context.Context, req RerankRequest) (*RerankResponse, error) {
	f, trace, err := g.facade(ctx, req.Trace)
	if err != nil {
		return nil, err
	}
	req.Trace = trace
	return f.Rerank(ctx, req)
}

func (g *TenantGateway) Judge(ctx context.Context, req JudgeRequest) (*JudgeResponse, error) {
	f, trace, err := g.facade(ctx, req.Trace)
	if err != nil {
		return nil, err
	}
	req.Trace = trace
	return f.Judge(ctx, req)
}

func (g *TenantGateway) facade(ctx context.Context, requestTrace observability.TraceContext) (*Facade, observability.TraceContext, error) {
	trace, _, _, err := bindTrustedTrace(ctx, requestTrace)
	if err != nil {
		return nil, observability.TraceContext{}, err
	}
	f, ok, err := g.lookup(ctx, trace.TenantID)
	if err != nil {
		return nil, observability.TraceContext{}, err
	}
	if ok {
		return f, trace, nil
	}
	return nil, observability.TraceContext{}, fmt.Errorf("modelgateway: no provider configured for tenant %q", trace.TenantID)
}
