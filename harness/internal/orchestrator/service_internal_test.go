package orchestrator

import (
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestBindingFailureErrorUsesCanonicalTypes(t *testing.T) {
	tests := []struct {
		code agentbinding.ErrorCode
		want observability.EventErrorType
	}{
		{agentbinding.CodeAskUserRequired, observability.EventErrorSchemaValidation},
		{agentbinding.CodeDefinitionInvalid, observability.EventErrorSchemaValidation},
		{agentbinding.CodeControlDenied, observability.EventErrorPermissionDenied},
		{agentbinding.CodeAgentDisabled, observability.EventErrorPermissionDenied},
		{agentbinding.CodeConfigResolveFailed, observability.EventErrorUpstream},
		{agentbinding.CodeCapabilitySnapshotFailed, observability.EventErrorUpstream},
		{agentbinding.CodeAgentNotFound, observability.EventErrorInternal},
	}
	for _, test := range tests {
		t.Run(string(test.code), func(t *testing.T) {
			got := bindingFailureError(agentbinding.FailurePayload{Code: test.code})
			if got.Type != test.want {
				t.Fatalf("type = %q, want %q", got.Type, test.want)
			}
		})
	}
}
