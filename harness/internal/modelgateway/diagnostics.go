package modelgateway

import (
	"context"
	"sort"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type InputDiagnostics interface {
	Build(ctx context.Context, req ModelRequest) GatewayDiagnostics
}

type DefaultInputDiagnostics struct{}

func (DefaultInputDiagnostics) Build(_ context.Context, req ModelRequest) GatewayDiagnostics {
	roleCounts := map[string]int{}
	for _, msg := range req.Messages {
		roleCounts[msg.Role]++
	}
	chars, tokens := estimatePromptInput(req.Messages)
	return GatewayDiagnostics{
		MessageCount:    len(req.Messages),
		RoleCounts:      roleCounts,
		PromptChars:     chars,
		EstimatedTokens: tokens,
	}
}

func logDiagnostics(ctx context.Context, logger observability.StructuredLogger, req ModelRequest, d GatewayDiagnostics) {
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	roles := make([]string, 0, len(d.RoleCounts))
	for role := range d.RoleCounts {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	fields := []observability.Field{
		observability.String("request_id", req.RequestID),
		observability.String("agent_id", req.AgentID),
		observability.String("model_hint", req.ModelHint),
		observability.String("context_hash", req.ContextHash),
		observability.Int("message_count", d.MessageCount),
		observability.Int("prompt_chars", d.PromptChars),
		observability.Int("estimated_tokens", d.EstimatedTokens),
	}
	for _, role := range roles {
		fields = append(fields, observability.Int("role_"+role, d.RoleCounts[role]))
	}
	logger.Info(ctx, "model gateway input diagnostics", fields...)
}
