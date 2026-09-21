package toolgateway

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type toolTerminalBindingContextKey struct{}

func withToolTerminalBinding(ctx context.Context, binding ToolTerminalBinding) context.Context {
	binding.Keys = append([]string(nil), binding.Keys...)
	return context.WithValue(ctx, toolTerminalBindingContextKey{}, binding)
}

func toolTerminalBindingFrom(ctx context.Context) (ToolTerminalBinding, bool) {
	binding, ok := ctx.Value(toolTerminalBindingContextKey{}).(ToolTerminalBinding)
	return binding, ok && len(binding.Keys) > 0
}

func newToolTerminalBinding(keys []string, req ToolCallRequest, logicalKey, argumentsHash, definitionHash string) ToolTerminalBinding {
	return ToolTerminalBinding{
		Keys: append([]string(nil), keys...), ToolCallID: req.ToolCallID, ToolName: req.ToolName, ToolVersion: req.ToolVersion,
		ArgumentsHash: argumentsHash, DefinitionHash: definitionHash, LogicalKeyHash: hashString(logicalKey),
	}
}

func isTerminalToolEvent(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventToolCallCompleted, observability.EventToolCallFailed, observability.EventToolCallCancelled:
		return true
	default:
		return false
	}
}
