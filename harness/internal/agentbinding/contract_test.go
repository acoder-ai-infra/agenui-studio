package agentbinding

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

func TestResolverRejectsInvalidRequestEnvelope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		mutate      func(*BindingRequest)
		nilResolver bool
		nilConfigs  bool
	}{
		{name: "nil resolver", nilResolver: true},
		{name: "nil config resolver", nilConfigs: true},
		{name: "binding id missing", mutate: func(req *BindingRequest) { req.BindingID = "" }},
		{name: "session id missing", mutate: func(req *BindingRequest) { req.SessionID = "" }},
		{name: "run id missing", mutate: func(req *BindingRequest) { req.RunID = "" }},
		{name: "schema version unsupported", mutate: func(req *BindingRequest) { req.SchemaVersion = "harness.agent_binding.v2" }},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := baseRequest(Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent})
			if tt.mutate != nil {
				tt.mutate(&req)
			}

			calls := 0
			var resolver *Resolver
			switch {
			case tt.nilResolver:
			case tt.nilConfigs:
				resolver = NewResolver(nil)
			default:
				resolver = NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
					calls++
					return resolvedAgent("agent-a", "v1", executionmode.SingleAgent), nil
				}))
			}

			_, err := resolver.Resolve(context.Background(), req)
			var bindingErr *Error
			if !errors.As(err, &bindingErr) {
				t.Fatalf("Resolve() error type = %T, want *Error", err)
			}
			if CodeOf(err) != CodeInvalidRequest || StageOf(err) != StageSourceSelection || RetryableOf(err) {
				t.Fatalf("Resolve() error = %v, code=%q stage=%q retryable=%t", err, CodeOf(err), StageOf(err), RetryableOf(err))
			}
			if SafeMessageOf(err) != SafeMessage(CodeInvalidRequest) {
				t.Fatalf("safe message = %q", SafeMessageOf(err))
			}
			if calls != 0 {
				t.Fatalf("invalid envelope reached ConfigResolver %d times", calls)
			}
		})
	}
}

func TestResolverUsesRequestedExecutionModeWhenConfigOmitsIt(t *testing.T) {
	t.Parallel()
	resolved := resolvedAgent("agent-a", "v1", executionmode.SingleAgent)
	resolved.ExecutionMode = ""
	resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		return resolved, nil
	}))

	result, err := resolver.Resolve(context.Background(), baseRequest(Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent}))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if result.Binding.ExecutionMode != executionmode.SingleAgent {
		t.Fatalf("execution mode = %q", result.Binding.ExecutionMode)
	}
	if err := result.Binding.Validate(); err != nil {
		t.Fatalf("effective binding invalid: %v", err)
	}
}

func TestResolverRequiresEffectiveExecutionMode(t *testing.T) {
	t.Parallel()
	resolved := resolvedAgent("agent-a", "v1", "")
	resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		return resolved, nil
	}))

	_, err := resolver.Resolve(context.Background(), baseRequest(Selection{AgentID: "agent-a"}))
	if !errors.Is(err, ErrAskUserRequired) || CodeOf(err) != CodeAskUserRequired || StageOf(err) != StageFinalize {
		t.Fatalf("Resolve() error = %v, code=%q stage=%q", err, CodeOf(err), StageOf(err))
	}
	var ask *AskUserError
	if !errors.As(err, &ask) || len(ask.Clarification.MissingFields) != 1 || ask.Clarification.MissingFields[0] != "execution_mode" {
		t.Fatalf("unexpected AskUser error: %#v", err)
	}
}

func TestResolverPreservesCreatedAtAsUTC(t *testing.T) {
	t.Parallel()
	resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
		return resolvedAgent("agent-a", "v1", executionmode.SingleAgent), nil
	}))
	resolver.Clock = func() time.Time {
		t.Fatal("Clock must not replace a caller-provided created_at")
		return time.Time{}
	}
	req := baseRequest(Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent})
	req.CreatedAt = time.Date(2026, 7, 13, 18, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))

	result, err := resolver.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if result.Binding.CreatedAt != req.CreatedAt.UTC() || result.Binding.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at = %v, want %v", result.Binding.CreatedAt, req.CreatedAt.UTC())
	}
}

func TestResolverReturnsTypedConfigErrors(t *testing.T) {
	t.Parallel()
	rawErr := errors.New("registry unavailable")
	typedErr := NewError(StageStaticValidate, CodeConfigInvalid, false, errors.New("invalid dependency"))
	tests := []struct {
		name      string
		cause     error
		wantCode  ErrorCode
		retryable bool
	}{
		{name: "raw backend error", cause: rawErr, wantCode: CodeConfigResolveFailed, retryable: true},
		{name: "typed config error", cause: typedErr, wantCode: CodeConfigInvalid},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolver := NewResolver(configResolverFunc(func(context.Context, ConfigResolveRequest) (ResolvedConfig, error) {
				return ResolvedConfig{}, tt.cause
			}))

			_, err := resolver.Resolve(context.Background(), baseRequest(Selection{AgentID: "agent-a", Mode: executionmode.SingleAgent}))
			var bindingErr *Error
			if !errors.As(err, &bindingErr) || !errors.Is(err, tt.cause) {
				t.Fatalf("Resolve() error = %#v, want typed wrapper preserving cause", err)
			}
			if CodeOf(err) != tt.wantCode || StageOf(err) != StageConfigResolve || RetryableOf(err) != tt.retryable {
				t.Fatalf("code=%q stage=%q retryable=%t", CodeOf(err), StageOf(err), RetryableOf(err))
			}
		})
	}
}

func TestEffectiveBindingRequiresCanonicalFacts(t *testing.T) {
	t.Parallel()
	base := validEffectiveBinding()
	base.BindingHash, _ = ComputeBindingHash(base)
	tests := []struct {
		name   string
		mutate func(*EffectiveBinding)
		want   ErrorCode
	}{
		{name: "schema version", mutate: func(binding *EffectiveBinding) { binding.SchemaVersion = "" }, want: CodeInvalidRequest},
		{name: "binding id", mutate: func(binding *EffectiveBinding) { binding.BindingID = "" }, want: CodeInvalidRequest},
		{name: "session id", mutate: func(binding *EffectiveBinding) { binding.SessionID = "" }, want: CodeInvalidRequest},
		{name: "run id", mutate: func(binding *EffectiveBinding) { binding.RunID = "" }, want: CodeInvalidRequest},
		{name: "agent id", mutate: func(binding *EffectiveBinding) { binding.AgentID = "" }, want: CodeInvalidRequest},
		{name: "agent version", mutate: func(binding *EffectiveBinding) { binding.AgentVersion = "" }, want: CodeInvalidRequest},
		{name: "execution mode", mutate: func(binding *EffectiveBinding) { binding.ExecutionMode = "" }, want: CodeExecutionModeUnsupported},
		{name: "created at", mutate: func(binding *EffectiveBinding) { binding.CreatedAt = time.Time{} }, want: CodeInvalidRequest},
		{name: "config snapshot", mutate: func(binding *EffectiveBinding) { binding.ConfigSnapshotRef = "" }, want: CodeConfigSnapshotMissing},
		{name: "config hash", mutate: func(binding *EffectiveBinding) { binding.ConfigHash = "" }, want: CodeConfigHashMissing},
		{name: "binding hash", mutate: func(binding *EffectiveBinding) { binding.BindingHash = "" }, want: CodeBindingHashMismatch},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			binding := base
			tt.mutate(&binding)
			if err := binding.Validate(); CodeOf(err) != tt.want {
				t.Fatalf("Validate() code = %q, want %q (err=%v)", CodeOf(err), tt.want, err)
			}
		})
	}
}
