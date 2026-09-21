package agentgateway

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestDefaultObservabilityUsesGatewayConfigHash(t *testing.T) {
	const configHash = "sha256:gateway-config"
	req := testInvocation()
	req.Binding.ConfigHash = configHash
	facts := newInvocationFacts()
	next := func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
		facts.setOutcome(GatewayProviderOutcomeKnown, true, "")
		return agentruntime.SubAgentInvocationResult{Content: "ok"}, nil
	}

	logger := &gatewayPluginTestLogger{}
	ctx := observability.WithLogger(context.Background(), logger)
	if _, err := invokeCanonicalObserver(ctx, json.RawMessage(`{}`), req, facts, next); err != nil {
		t.Fatalf("observer Invoke: %v", err)
	}
	assertGatewayConfigHashField(t, logger.snapshot(), configHash)

	tracer := &gatewayPluginTestTracer{}
	if _, err := tracePlugin(tracer)(context.Background(), json.RawMessage(`{}`), req, facts, next); err != nil {
		t.Fatalf("tracer Invoke: %v", err)
	}
	assertGatewayConfigHashField(t, tracer.snapshot(), configHash)
}

func assertGatewayConfigHashField(t *testing.T, fields map[string]string, want string) {
	t.Helper()
	if fields["gateway_config_hash"] != want {
		t.Fatalf("gateway_config_hash=%q want=%q fields=%v", fields["gateway_config_hash"], want, fields)
	}
	if _, exists := fields["gateway_profile_hash"]; exists {
		t.Fatalf("legacy gateway_profile_hash still emitted: %v", fields)
	}
}

type gatewayPluginTestLogger struct {
	mu     sync.Mutex
	fields map[string]string
}

func (l *gatewayPluginTestLogger) Debug(_ context.Context, _ string, fields ...observability.Field) {
	l.record(fields)
}
func (l *gatewayPluginTestLogger) Info(_ context.Context, _ string, fields ...observability.Field) {
	l.record(fields)
}
func (l *gatewayPluginTestLogger) Warn(_ context.Context, _ string, fields ...observability.Field) {
	l.record(fields)
}
func (l *gatewayPluginTestLogger) Error(_ context.Context, _ string, _ error, fields ...observability.Field) {
	l.record(fields)
}
func (l *gatewayPluginTestLogger) With(...observability.Field) observability.StructuredLogger {
	return l
}
func (l *gatewayPluginTestLogger) Sync() {}

func (l *gatewayPluginTestLogger) record(fields []observability.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fields = stringFields(fields)
}

func (l *gatewayPluginTestLogger) snapshot() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneTestFields(l.fields)
}

type gatewayPluginTestTracer struct {
	mu     sync.Mutex
	fields map[string]string
}

func (t *gatewayPluginTestTracer) Start(ctx context.Context, _ string, fields ...observability.Field) (context.Context, observability.Span) {
	t.mu.Lock()
	t.fields = stringFields(fields)
	t.mu.Unlock()
	return ctx, gatewayPluginTestSpan{}
}

func (t *gatewayPluginTestTracer) snapshot() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneTestFields(t.fields)
}

type gatewayPluginTestSpan struct{}

func (gatewayPluginTestSpan) End()                                      {}
func (gatewayPluginTestSpan) AddEvent(string, ...observability.Field)   {}
func (gatewayPluginTestSpan) RecordError(error, ...observability.Field) {}
func (gatewayPluginTestSpan) TraceContext() observability.TraceContext {
	return observability.TraceContext{}
}

func stringFields(fields []observability.Field) map[string]string {
	out := make(map[string]string, len(fields))
	for _, field := range fields {
		out[field.Key] = field.String
	}
	return out
}

func cloneTestFields(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
