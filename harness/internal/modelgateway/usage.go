package modelgateway

import (
	"context"
	"strings"
)

// UsageCostCollector normalizes usage and attaches best-effort cost attribution.
type UsageCostCollector interface {
	Collect(ctx context.Context, req UsageCostRequest) UsageCostResult
}

type UsageCostRequest struct {
	ModelRequest ModelRequest
	Target       ModelTarget
	OutputText   string
	Usage        ModelUsage
	UsagePresent bool
}

type UsageCostResult struct {
	Usage ModelUsage
	Cost  ModelCost
}

// DefaultUsageCostCollector trusts provider usage when present and falls back to
// a conservative local estimate when a streaming provider omits the final usage.
type DefaultUsageCostCollector struct{}

func (DefaultUsageCostCollector) Collect(_ context.Context, req UsageCostRequest) UsageCostResult {
	usage := req.Usage
	if req.UsagePresent {
		if usage.Source == "" {
			usage.Source = UsageSourceGateway
		}
	} else {
		usage = estimateUsage(req.ModelRequest.Messages, req.OutputText)
	}
	return UsageCostResult{
		Usage: usage,
		Cost:  estimateCost(usage, req.Target.Cost),
	}
}

func estimateUsage(messages []ChatMessage, output string) ModelUsage {
	_, promptTokens := estimatePromptInput(messages)
	return ModelUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: estimateTokensFromChars(len([]rune(output))),
		Source:           UsageSourceEstimated,
	}
}

func estimateTokensFromChars(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

func estimateCost(usage ModelUsage, table ModelCostTable) ModelCost {
	currency := strings.TrimSpace(table.Currency)
	if currency == "" || (table.InputPer1K <= 0 && table.OutputPer1K <= 0 && table.ReasoningPer1K <= 0 && table.CacheReadPer1K <= 0 && table.CacheWritePer1K <= 0) {
		return ModelCost{}
	}
	cost := per1K(usage.PromptTokens, table.InputPer1K) +
		per1K(usage.CompletionTokens, table.OutputPer1K) +
		per1K(usage.ReasoningTokens, table.ReasoningPer1K) +
		per1K(usage.CacheReadTokens, table.CacheReadPer1K) +
		per1K(usage.CacheWriteTokens, table.CacheWritePer1K)
	return ModelCost{Currency: currency, Estimated: cost}
}

func per1K(tokens int, price float64) float64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	return float64(tokens) * price / 1000
}
