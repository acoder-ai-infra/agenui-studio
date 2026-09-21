package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestBuildFacadeRegistersAnthropicProvider(t *testing.T) {
	facade, names := buildFacade("tenant-a", TenantConfig{Providers: []ProviderConfig{{
		Name: "claude", Protocol: "anthropic", BaseURL: "https://anthropic.example.test", APIKey: "test-key",
		Models: []string{"claude-sonnet"}, Capability: modelgateway.ModelCapability{
			Chat: true, Streaming: true, ToolCalling: true,
			Limits: modelgateway.ModelLimits{MaxContextTokens: 200000, MaxOutputTokens: 8192},
		},
	}}, DefaultProvider: "claude", DefaultModel: "claude-sonnet"})

	if len(names) != 1 || names[0] != "claude" {
		t.Fatalf("registered provider names = %v", names)
	}
	provider, ok := facade.Providers["claude"]
	if !ok || provider.ID() != "claude" {
		t.Fatalf("anthropic provider was not registered: %#v", facade.Providers)
	}
}

func TestBuildFacadeDoesNotEnableAnthropicPromptCacheAffinityOutsideControlledGateway(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		kind    modelgateway.ProviderKind
	}{
		{name: "default off"},
		{name: "mislabeled arbitrary endpoint", enabled: true, kind: modelgateway.ProviderKindSessionAffinity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Session-Id")
				_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()

			facade, _ := buildFacade("tenant-a", TenantConfig{Providers: []ProviderConfig{{
				Name: "claude", Protocol: "anthropic", BaseURL: server.URL, APIKey: "test-key",
				Models: []string{"claude-sonnet"}, ProviderKind: test.kind, PromptCacheSessionAffinity: test.enabled,
			}}, DefaultProvider: "claude", DefaultModel: "claude-sonnet"})
			ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a", SessionID: "session-a"})
			call, err := facade.Chat(ctx, modelgateway.ModelRequest{
				RequestID: "request-a",
				Messages:  []modelgateway.ChatMessage{{Role: "user", Content: "hello"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range call.Events {
			}
			response, err := call.Await()
			if err != nil || response.Status != modelgateway.StatusSuccess {
				t.Fatalf("response=%#v err=%v", response, err)
			}
			if got != "" {
				t.Fatalf("untrusted endpoint received X-Session-Id: %q", got)
			}
		})
	}
}

func TestValidateModelProviderOptionsRejectsAffinityOnNonAnthropicProvider(t *testing.T) {
	config := ModelConfig{Default: TenantConfig{Providers: []ProviderConfig{{
		Name: "qwen", Protocol: "openai_compatible", PromptCacheSessionAffinity: true,
	}}}}
	if err := validateModelProviderOptions(config); err == nil {
		t.Fatal("openai_compatible provider with prompt cache affinity must fail closed")
	}
	config.Default.Providers[0].Protocol = "anthropic"
	if err := validateModelProviderOptions(config); err == nil {
		t.Fatal("ordinary anthropic provider without controlled company kind must be rejected")
	}
}

func TestValidateModelProviderOptionsRequiresSessionAffinityAnthropicEndpoint(t *testing.T) {
	base := ProviderConfig{
		Name: "company-claude", Protocol: "anthropic",
		ProviderKind:               modelgateway.ProviderKindSessionAffinity,
		PromptCacheSessionAffinity: true,
	}
	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{name: "controlled loopback gateway", baseURL: "http://localhost/open_api/anthropic"},
		{name: "public Anthropic even if mislabeled", baseURL: "https://api.anthropic.com", wantErr: true},
		{name: "arbitrary endpoint even if mislabeled", baseURL: "https://attacker.example.test/anthropic", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := base
			provider.BaseURL = test.baseURL
			err := validateModelProviderOptions(ModelConfig{Default: TenantConfig{Providers: []ProviderConfig{provider}}})
			if (err != nil) != test.wantErr {
				t.Fatalf("validation error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestBuildFacadeDoesNotSendAffinityToUntrustedAnthropicEndpoint(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Session-Id")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	facade, _ := buildFacade("tenant-a", TenantConfig{Providers: []ProviderConfig{{
		Name: "mislabeled-public", Protocol: "anthropic", BaseURL: server.URL, APIKey: "test-key",
		Models: []string{"claude-sonnet"}, ProviderKind: modelgateway.ProviderKindSessionAffinity,
		PromptCacheSessionAffinity: true,
	}}, DefaultProvider: "mislabeled-public", DefaultModel: "claude-sonnet"})
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-a", SessionID: "session-a"})
	call, err := facade.Chat(ctx, modelgateway.ModelRequest{
		RequestID: "request-a", Messages: []modelgateway.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range call.Events {
	}
	if _, err := call.Await(); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("untrusted Anthropic endpoint received X-Session-Id: %q", got)
	}
}

func TestBuildFacadeSendsAffinityForSessionAffinityGateway(t *testing.T) {
	var gotHeader, gotHost, gotPath string
	originalTransport := http.DefaultTransport
	http.DefaultTransport = appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		gotHeader = request.Header.Get("X-Session-Id")
		gotHost = request.URL.Host
		gotPath = request.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			Request:    request,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	facade, names := buildFacade("tenant-a", TenantConfig{Providers: []ProviderConfig{{
		Name: "company-claude", Protocol: "anthropic",
		ProviderKind: modelgateway.ProviderKindSessionAffinity,
		BaseURL:      "http://localhost/open_api/anthropic", APIKey: "test-key",
		Models: []string{"claude-sonnet"}, PromptCacheSessionAffinity: true,
	}}, DefaultProvider: "company-claude", DefaultModel: "claude-sonnet"})
	if len(names) != 1 {
		t.Fatalf("provider names=%v", names)
	}
	trusted := observability.TraceContext{TenantID: "tenant-a", SessionID: "session-a", AgentID: "agent-a"}
	ctx := observability.WithTraceContext(context.Background(), trusted)
	call, err := facade.Chat(ctx, modelgateway.ModelRequest{
		RequestID: "model-call-a", AgentID: trusted.AgentID,
		Messages: []modelgateway.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range call.Events {
	}
	if response, err := call.Await(); err != nil || response.Status != modelgateway.StatusSuccess {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	want := modelgateway.DerivePromptCacheAffinityKey(trusted)
	if gotHeader != want || gotHost != "localhost" || gotPath != "/open_api/anthropic/v1/messages" {
		t.Fatalf("controlled provider request host=%q path=%q header=%q want=%q", gotHost, gotPath, gotHeader, want)
	}
}

type appRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn appRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
