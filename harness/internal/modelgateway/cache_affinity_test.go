package modelgateway_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagemem "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func TestDerivePromptCacheAffinityKeyRequiresTenantAndSession(t *testing.T) {
	tests := []observability.TraceContext{
		{},
		{TenantID: "tenant-a"},
		{SessionID: "session-a"},
	}
	for _, trace := range tests {
		if got := mg.DerivePromptCacheAffinityKey(trace); got != "" {
			t.Fatalf("affinity key for incomplete identity = %q", got)
		}
	}
}

func TestDerivePromptCacheAffinityKeyIsStableTenantIsolatedAndOpaque(t *testing.T) {
	base := observability.TraceContext{
		TenantID: "tenant-visible-sentinel", SessionID: "session-visible-sentinel",
		RunID: "run-1", RequestID: "request-1", TraceID: "trace-1",
	}
	first := mg.DerivePromptCacheAffinityKey(base)
	second := mg.DerivePromptCacheAffinityKey(observability.TraceContext{
		TenantID: base.TenantID, SessionID: base.SessionID,
		RunID: "run-2", RequestID: "request-2", TraceID: "trace-2",
	})
	otherTenant := mg.DerivePromptCacheAffinityKey(observability.TraceContext{
		TenantID: "tenant-b", SessionID: base.SessionID,
	})
	if first == "" || first != second {
		t.Fatalf("same tenant/session must be stable: first=%q second=%q", first, second)
	}
	if first == otherTenant {
		t.Fatalf("different tenants must not share affinity key: %q", first)
	}
	if strings.Contains(first, base.TenantID) || strings.Contains(first, base.SessionID) {
		t.Fatalf("affinity key exposes raw identity: %q", first)
	}
}

func TestDerivePromptCacheAffinityKeyIsConcurrentSafe(t *testing.T) {
	trace := observability.TraceContext{TenantID: "tenant-a", SessionID: "session-a"}
	want := mg.DerivePromptCacheAffinityKey(trace)
	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := mg.DerivePromptCacheAffinityKey(trace); got != want {
				errs <- got
			}
		}()
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Fatalf("concurrent derivation changed: got=%q want=%q", got, want)
	}
}

func TestPromptCacheSessionAffinityAllowedOnlyForSessionAffinityGateway(t *testing.T) {
	tests := []struct {
		name     string
		kind     mg.ProviderKind
		protocol string
		baseURL  string
		want     bool
	}{
		{name: "controlled loopback", kind: mg.ProviderKindSessionAffinity, protocol: "anthropic", baseURL: "http://127.0.0.1/open_api/anthropic", want: true},
		{name: "controlled localhost trailing slash", kind: mg.ProviderKindSessionAffinity, protocol: "anthropic", baseURL: "http://localhost/open_api/anthropic/", want: true},
		{name: "public anthropic mislabeled", kind: mg.ProviderKindSessionAffinity, protocol: "anthropic", baseURL: "https://api.anthropic.com"},
		{name: "arbitrary endpoint mislabeled", kind: mg.ProviderKindSessionAffinity, protocol: "anthropic", baseURL: "https://attacker.example.test/open_api/anthropic"},
		{name: "controlled host wrong path", kind: mg.ProviderKindSessionAffinity, protocol: "anthropic", baseURL: "http://localhost/v1"},
		{name: "controlled host query rejected", kind: mg.ProviderKindSessionAffinity, protocol: "anthropic", baseURL: "http://localhost/open_api/anthropic?redirect=1"},
		{name: "standard kind", protocol: "anthropic", baseURL: "https://model.example.test/open_api/anthropic"},
		{name: "wrong protocol", kind: mg.ProviderKindSessionAffinity, protocol: "openai_compatible", baseURL: "http://localhost/open_api/anthropic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mg.PromptCacheSessionAffinityAllowed(test.kind, test.protocol, test.baseURL); got != test.want {
				t.Fatalf("allowed=%v, want %v", got, test.want)
			}
		})
	}
}

type affinityRecordingProvider struct {
	mu       sync.Mutex
	requests []mg.AdapterRequest
	delegate mg.ChatProvider
}

func (p *affinityRecordingProvider) ID() string { return p.delegate.ID() }

func (p *affinityRecordingProvider) InvokeChat(ctx context.Context, req mg.AdapterRequest) (mg.AdapterStream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return p.delegate.InvokeChat(ctx, req)
}

func (p *affinityRecordingProvider) affinityKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, len(p.requests))
	for index, request := range p.requests {
		keys[index] = request.PromptCacheAffinityKey
	}
	return keys
}

func TestFacadePreservesPromptCacheAffinityAcrossHookAndFallback(t *testing.T) {
	primary := &affinityRecordingProvider{delegate: mock.New("primary", mock.Script{
		InvokeErr: &mg.AdapterError{Message: "retry elsewhere", Retryable: true, Class: mg.ErrorNetwork},
	})}
	backup := &affinityRecordingProvider{delegate: mock.New("backup", mock.Script{Chunks: []mg.NormalizedChunk{
		mock.TokenChunk("ok"), mock.UsageChunk(1, 1),
	}})}
	trustedTrace := observability.TraceContext{
		TraceID: "trace-a", TenantID: "tenant-a", SessionID: "session-a", RunID: "run-a", AgentID: "agent-a",
	}
	key := mg.DerivePromptCacheAffinityKey(trustedTrace)
	stores := storagemem.New().Stores()
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary:  mg.ModelTarget{Model: "primary-model", Provider: "primary"},
			Fallback: []mg.ModelTarget{{Model: "backup-model", Provider: "backup"}},
		}, nil),
		Providers:   map[string]mg.ChatProvider{"primary": primary, "backup": backup},
		UsageLedger: stores.Usage,
		Hooks: mg.ModelHooks{BeforeModel: []mg.BeforeModelHook{
			func(_ context.Context, req mg.ModelRequest, _ mg.ModelTarget) (mg.ModelRequest, error) {
				req.PromptVersion = "hooked"
				// A model hook may rewrite the provider-visible request, including its
				// Trace. It must not be able to redirect the provider cache affinity.
				req.Trace.TenantID = "attacker-tenant"
				req.Trace.SessionID = "attacker-session"
				req.Trace.RunID = "attacker-run"
				req.Trace.TraceID = "attacker-trace"
				req.Trace.Baggage = map[string]string{"identity": "attacker"}
				req.RequestID = "attacker-request"
				req.AgentID = "attacker-agent"
				return req, nil
			},
		}},
	}
	ctx := observability.WithTraceContext(context.Background(), trustedTrace)
	events, response := collectAffinityEvents(t, ctx, gw, mg.ModelRequest{
		RequestID: "request-a", Trace: trustedTrace, AgentID: "agent-a", Streaming: true,
		Messages: []mg.ChatMessage{mg.TextMessage("user", "hello")},
	})
	if response.Status != mg.StatusSuccess {
		t.Fatalf("fallback response = %#v", response)
	}
	for provider, keys := range map[string][]string{
		"primary": primary.affinityKeys(), "backup": backup.affinityKeys(),
	} {
		if len(keys) != 1 || keys[0] != key {
			t.Fatalf("%s affinity keys = %v, want [%q]", provider, keys, key)
		}
	}
	for _, event := range events {
		if event.SessionID != trustedTrace.SessionID || event.RunID != trustedTrace.RunID || event.TraceID != trustedTrace.TraceID || event.AgentID != trustedTrace.AgentID {
			t.Fatalf("hook split provider affinity from event identity: event=%#v", event)
		}
	}
	records, err := stores.Usage.List(context.Background(), storage.ModelUsageQuery{TenantID: trustedTrace.TenantID})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("trusted usage records=%d, want both primary and fallback attempts", len(records))
	}
	for _, record := range records {
		if record.RequestID != "request-a" || record.TenantID != trustedTrace.TenantID || record.SessionID != trustedTrace.SessionID || record.RunID != trustedTrace.RunID || record.TraceID != trustedTrace.TraceID || record.AgentID != trustedTrace.AgentID {
			t.Fatalf("hook split provider affinity from usage identity: record=%#v", record)
		}
	}
}

func TestFacadeRejectsRequestTraceThatConflictsWithTrustedContext(t *testing.T) {
	provider := mock.New("primary", mock.Script{Chunks: []mg.NormalizedChunk{
		mock.TokenChunk("must-not-run"), mock.UsageChunk(1, 1),
	}})
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary: mg.ModelTarget{Model: "primary-model", Provider: "primary"},
		}, nil),
		Providers: map[string]mg.ChatProvider{"primary": provider},
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TenantID: "trusted-tenant", SessionID: "trusted-session", RunID: "trusted-run", AgentID: "trusted-agent",
	})
	call, err := gw.Chat(ctx, mg.ModelRequest{
		RequestID: "request-spoof",
		Trace: observability.TraceContext{
			TenantID: "spoofed-tenant", SessionID: "spoofed-session", RunID: "spoofed-run",
		},
		Messages: []mg.ChatMessage{mg.TextMessage("user", "hello")},
	})
	if err == nil {
		for range call.Events {
		}
		_, _ = call.Await()
		t.Fatal("conflicting request trace must fail closed")
	}
	if provider.Calls() != 0 {
		t.Fatalf("spoofed request reached provider: calls=%d", provider.Calls())
	}
	call, err = gw.Chat(ctx, mg.ModelRequest{
		RequestID: "request-agent-spoof", AgentID: "spoofed-agent",
		Trace: observability.TraceContext{
			TenantID: "trusted-tenant", SessionID: "trusted-session", RunID: "trusted-run", AgentID: "trusted-agent",
		},
		Messages: []mg.ChatMessage{mg.TextMessage("user", "hello")},
	})
	if err == nil {
		for range call.Events {
		}
		_, _ = call.Await()
		t.Fatal("conflicting top-level agent id must fail closed")
	}
	if provider.Calls() != 0 {
		t.Fatalf("agent-spoofed request reached provider: calls=%d", provider.Calls())
	}
}

func collectAffinityEvents(t *testing.T, ctx context.Context, gw *mg.Facade, req mg.ModelRequest) ([]observability.AgentEvent, *mg.ModelResponse) {
	t.Helper()
	call, err := gw.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var events []observability.AgentEvent
	for event := range call.Events {
		events = append(events, event)
	}
	response, err := call.Await()
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	return events, response
}
