package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestDefaultRuntimeErrorClassifier(t *testing.T) {
	typed := NewRuntimeError(ErrorRateLimited, "MODEL_RATE_LIMITED", "model rate limited").WithRetryable(true).WithDependency("model_gateway")
	tests := []struct {
		name      string
		err       error
		stage     RuntimeErrorStage
		wantType  ErrorType
		wantCode  string
		retryable bool
	}{
		{name: "timeout", err: context.DeadlineExceeded, stage: ErrorStageRuntimeAdapter, wantType: ErrorTimeout, wantCode: "RUNTIME_TIMEOUT", retryable: true},
		{name: "cancelled", err: context.Canceled, stage: ErrorStageRuntimeAdapter, wantType: ErrorCancelled, wantCode: "RUN_CANCELLED"},
		{name: "runtime unavailable", err: ErrRuntimeUnavailable, stage: ErrorStageRuntimeSelection, wantType: ErrorDependencyUnavailable, wantCode: "RUNTIME_UNAVAILABLE", retryable: true},
		{name: "resume binding", err: ErrResumeBindingMismatch, stage: ErrorStageResume, wantType: ErrorCheckpoint, wantCode: "RESUME_BINDING_MISMATCH"},
		{name: "resume claim fenced", err: ErrResumeClaimLost, stage: ErrorStageState, wantType: ErrorCheckpoint, wantCode: "RESUME_CLAIM_LOST", retryable: true},
		{name: "typed", err: typed, stage: ErrorStageRuntimeAdapter, wantType: ErrorRateLimited, wantCode: "MODEL_RATE_LIMITED", retryable: true},
		{name: "mcp oauth authorization", err: &mcp.AuthorizationRequiredError{ServerID: "mcp_1", Provider: "oauth-provider"}, stage: ErrorStageModelContext, wantType: ErrorDependencyUnavailable, wantCode: "MCP_AUTHORIZATION_REQUIRED", retryable: true},
		{name: "unknown adapter", err: errors.New("provider stack with secret"), stage: ErrorStageRuntimeAdapter, wantType: ErrorRuntime, wantCode: "RUNTIME_ADAPTER_FAILED"},
	}
	classifier := DefaultRuntimeErrorClassifier{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifier.Classify(tt.err, tt.stage)
			if got.Type != tt.wantType || got.Code != tt.wantCode || got.Retryable != tt.retryable {
				t.Fatalf("classification mismatch: %#v", got)
			}
			if !errors.Is(got, tt.err) {
				t.Fatalf("classified error should preserve cause: %v", got)
			}
		})
	}
}

func TestRuntimeErrorSerializationExcludesCause(t *testing.T) {
	runtimeErr := NewRuntimeError(ErrorTool, "TOOL_FAILED", "tool failed").
		WithRetryable(true).
		WithDependency("tool_gateway").
		WithUserVisible(true).
		WithDegraded(true).
		WithDetailRef("artifact://error_detail").
		WithCause(errors.New("provider stack with secret"))
	encoded, err := json.Marshal(runtimeErr)
	if err != nil {
		t.Fatalf("marshal runtime error: %v", err)
	}
	if strings.Contains(string(encoded), "provider stack") || strings.Contains(string(encoded), "Cause") {
		t.Fatalf("runtime error cause leaked into json: %s", encoded)
	}
	payload := runtimeErr.Payload()
	if payload["dependency"] != "tool_gateway" || payload["detail_ref"] != "artifact://error_detail" || payload["user_visible"] != true || payload["degraded"] != true {
		t.Fatalf("runtime error payload incomplete: %#v", payload)
	}
	eventError := runtimeErr.EventError()
	if eventError.Code != "TOOL_FAILED" || eventError.Type != observability.EventErrorUpstream || !eventError.Retryable {
		t.Fatalf("event error mapping mismatch: %#v", eventError)
	}
}

func TestRuntimeErrorFromModelGatewayEventPreservesProviderCode(t *testing.T) {
	got := runtimeErrorFromEvent(&observability.EventError{
		Code:      "MODEL_PROVIDER_4XX",
		Type:      observability.EventErrorType("provider_4xx"),
		Message:   "provider rejected request",
		Retryable: false,
	})
	if got.Type != ErrorModel || got.Code != "MODEL_PROVIDER_4XX" || got.Message != "provider rejected request" || got.Retryable {
		t.Fatalf("runtime error from model event mismatch: %#v", got)
	}
}

func TestRuntimeErrorsMapToCanonicalEventErrorTypes(t *testing.T) {
	tests := []struct {
		runtimeType ErrorType
		want        observability.EventErrorType
	}{
		{ErrorRuntime, observability.EventErrorInternal},
		{ErrorModel, observability.EventErrorUpstream},
		{ErrorTool, observability.EventErrorUpstream},
		{ErrorProtocol, observability.EventErrorUpstream},
		{ErrorDependencyUnavailable, observability.EventErrorUpstream},
		{ErrorCheckpoint, observability.EventErrorInternal},
		{ErrorGuardrailBlock, observability.EventErrorGuardrailBlocked},
		{ErrorPermissionDenied, observability.EventErrorPermissionDenied},
		{ErrorContextOverflow, observability.EventErrorResourceExhausted},
		{ErrorSchemaValidation, observability.EventErrorSchemaValidation},
		{ErrorRateLimited, observability.EventErrorRateLimited},
		{ErrorTimeout, observability.EventErrorTimeout},
		{ErrorControlTimeout, observability.EventErrorTimeout},
		{ErrorCancelled, observability.EventErrorCancelled},
	}
	for _, test := range tests {
		t.Run(string(test.runtimeType), func(t *testing.T) {
			eventError := NewRuntimeError(test.runtimeType, "TEST", "safe").EventError()
			if eventError.Type != test.want {
				t.Fatalf("event error type = %q, want %q", eventError.Type, test.want)
			}
			restored := runtimeErrorFromEvent(eventError)
			if restored.Type == ErrorUnknown {
				t.Fatalf("canonical event error was not accepted: %#v", restored)
			}
		})
	}
}

func TestDefaultRuntimeErrorClassifierIsConcurrentSafe(t *testing.T) {
	classifier := DefaultRuntimeErrorClassifier{}
	const workers = 32
	var wg sync.WaitGroup
	results := make(chan *RuntimeError, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- classifier.Classify(ErrRuntimeUnavailable, ErrorStageRuntimeSelection)
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.Type != ErrorDependencyUnavailable || !result.Retryable {
			t.Fatalf("concurrent classification mismatch: %#v", result)
		}
	}
}
