package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

var ErrCapabilityUnsupported = errors.New("modelgateway: capability unsupported")

// ErrEvaluationModelHintRequired：评估类调用（embedding/rerank）不设默认模型，
// 调用方必须显式声明 ModelHint；网关不虚构目标模型（fail closed）。
var ErrEvaluationModelHintRequired = errors.New("modelgateway: evaluation model hint is required")

type EmbeddingProvider interface {
	ProviderAdapter
	Embed(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error)
}

type RerankProvider interface {
	ProviderAdapter
	Rerank(ctx context.Context, req RerankRequest) (*RerankResponse, error)
}

type JudgeProvider interface {
	ProviderAdapter
	Judge(ctx context.Context, req JudgeRequest) (*JudgeResponse, error)
}

// sortedProviderNames 返回按名称排序的 provider 列表。评估类调用的 provider
// 选择必须可复现，不能依赖 map 迭代序。
func (f *Facade) sortedProviderNames() []string {
	names := make([]string, 0, len(f.Providers))
	for name := range f.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Embedding 选择首个（按名称序）实现 EmbeddingProvider 的 provider 执行调用。
// provider 内部对不服务的模型返回 4xx；网关不做跨 provider 降级（fail closed）。
func (f *Facade) Embedding(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error) {
	if f == nil || len(f.Providers) == 0 {
		return nil, ErrCapabilityUnsupported
	}
	if strings.TrimSpace(req.ModelHint) == "" {
		return nil, ErrEvaluationModelHintRequired
	}
	for _, name := range f.sortedProviderNames() {
		p, ok := f.Providers[name].(EmbeddingProvider)
		if !ok {
			continue
		}
		resp, err := p.Embed(ctx, req)
		if err != nil {
			return nil, err
		}
		f.recordEvaluationUsage(ctx, "embedding", req.RequestID, req.Trace, req.AgentID, resp.Provider, resp.Model, resp.Usage, resp.Cost)
		return resp, nil
	}
	return nil, ErrCapabilityUnsupported
}

// Rerank 与 Embedding 同构：确定性选择 + fail closed + usage 落账。
func (f *Facade) Rerank(ctx context.Context, req RerankRequest) (*RerankResponse, error) {
	if f == nil || len(f.Providers) == 0 {
		return nil, ErrCapabilityUnsupported
	}
	if strings.TrimSpace(req.ModelHint) == "" {
		return nil, ErrEvaluationModelHintRequired
	}
	for _, name := range f.sortedProviderNames() {
		p, ok := f.Providers[name].(RerankProvider)
		if !ok {
			continue
		}
		resp, err := p.Rerank(ctx, req)
		if err != nil {
			return nil, err
		}
		f.recordEvaluationUsage(ctx, "rerank", req.RequestID, req.Trace, req.AgentID, resp.Provider, resp.Model, resp.Usage, resp.Cost)
		return resp, nil
	}
	return nil, ErrCapabilityUnsupported
}

// Judge 本期无 provider 实现，保持 ErrCapabilityUnsupported（方案第 5 章）。
func (f *Facade) Judge(ctx context.Context, req JudgeRequest) (*JudgeResponse, error) {
	for _, name := range f.sortedProviderNames() {
		if p, ok := f.Providers[name].(JudgeProvider); ok {
			return p.Judge(ctx, req)
		}
	}
	return nil, ErrCapabilityUnsupported
}

// recordEvaluationUsage 把一次评估类调用写入用量账本。评估调用无 attempt 维度，
// 记录 ID 以调用类别区分；落账失败只告警，不影响调用结果。
func (f *Facade) recordEvaluationUsage(
	ctx context.Context, kind, requestID string, trace observability.TraceContext,
	agentID, provider, model string, usage ModelUsage, cost ModelCost,
) {
	if f.UsageLedger == nil {
		return
	}
	tenant := trace.TenantID
	if tenant == "" {
		tenant = "default"
	}
	err := f.UsageLedger.Record(ctx, &storage.ModelUsageRecord{
		ID:            fmt.Sprintf("%s:%s", requestID, kind),
		RequestID:     requestID,
		TraceID:       trace.TraceID,
		TenantID:      tenant,
		SessionID:     trace.SessionID,
		RunID:         trace.RunID,
		AgentID:       agentID,
		Provider:      provider,
		Model:         model,
		PromptTokens:  usage.PromptTokens,
		UsageSource:   string(usage.Source),
		Currency:      cost.Currency,
		EstimatedCost: cost.Estimated,
		CreatedAt:     time.Now(),
	})
	if err != nil {
		f.logger().Warn(ctx, "model gateway evaluation usage record failed",
			observability.String("request_id", requestID),
			observability.String("kind", kind),
			observability.String("error", err.Error()),
		)
	}
}
