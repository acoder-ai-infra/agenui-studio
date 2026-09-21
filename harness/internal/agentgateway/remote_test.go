package agentgateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func validRemoteAgent() RemoteAgent {
	return RemoteAgent{
		AgentRef:  "hotel_agent",
		Protocol:  A2AProtocol,
		BaseURL:   "https://hotel.example.com",
		Transport: A2AJSONRPCTransport,
		AuthRef:   "credential://hotel",
		Timeout:   30 * time.Second,
	}
}

func TestStaticRemoteAgentResolver(t *testing.T) {
	resolver, err := NewStaticRemoteAgentResolver(map[string]RemoteAgent{"hotel_agent": validRemoteAgent()})
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	agent, err := resolver.Resolve(context.Background(), "hotel_agent")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Mutating the returned slice must not affect the resolver's internal state.
	agent.AllowedEndpointOrigins = append(agent.AllowedEndpointOrigins, "https://evil.example.com")
	again, _ := resolver.Resolve(context.Background(), "hotel_agent")
	if len(again.AllowedEndpointOrigins) != 0 {
		t.Fatalf("resolver allowlist leaked mutation: %v", again.AllowedEndpointOrigins)
	}
	if _, err := resolver.Resolve(context.Background(), "unknown"); !errors.Is(err, ErrRemoteTargetNotFoundSentinel()) {
		t.Fatalf("unknown ref should not be found, got %v", err)
	}
}

func ErrRemoteTargetNotFoundSentinel() error {
	return &GatewayError{Code: CodeRemoteTargetNotFound}
}

func TestStaticRemoteAgentResolverRejectsInvalid(t *testing.T) {
	cases := map[string]func(*RemoteAgent){
		"bad protocol":  func(a *RemoteAgent) { a.Protocol = "rest" },
		"bad transport": func(a *RemoteAgent) { a.Transport = "grpc" },
		"bad url":       func(a *RemoteAgent) { a.BaseURL = "not-a-url" },
		"zero timeout":  func(a *RemoteAgent) { a.Timeout = 0 },
		"origin path":   func(a *RemoteAgent) { a.AllowedEndpointOrigins = []string{"https://x.example.com/path"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			agent := validRemoteAgent()
			mutate(&agent)
			if _, err := NewStaticRemoteAgentResolver(map[string]RemoteAgent{"hotel_agent": agent}); err == nil {
				t.Fatalf("expected construction failure for %s", name)
			}
		})
	}
}

func TestBoundedTransportEnforcesLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		for i := 0; i < 100; i++ {
			_, _ = w.Write(make([]byte, 1024))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	client := newBoundedA2AClient(server.URL, nil, 4096, 5*time.Second)
	resp, err := client.Get(server.URL)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("want errResponseTooLarge, got %v", err)
	}
}

func TestBoundedTransportCredentialOriginScoping(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	credential := http.Header{"Authorization": {"Bearer secret"}}
	// Same expected origin: credential injected.
	client := newBoundedA2AClient(server.URL, credential, jsonRPCMaxWireBytes, 5*time.Second)
	if resp, err := client.Get(server.URL); err == nil {
		resp.Body.Close()
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("credential not injected for expected origin, got %q", gotAuth)
	}

	// Different expected origin: credential stripped.
	gotAuth = ""
	crossClient := newBoundedA2AClient("https://other.example.com", credential, jsonRPCMaxWireBytes, 5*time.Second)
	if resp, err := crossClient.Get(server.URL); err == nil {
		resp.Body.Close()
	}
	if gotAuth != "" {
		t.Fatalf("credential leaked to non-expected origin: %q", gotAuth)
	}
}

func TestFilterCardForJSONRPC(t *testing.T) {
	target := RemoteAgent{BaseURL: "https://agent.example.com"}
	card := &a2a.AgentCard{
		SupportedInterfaces: []*a2a.AgentInterface{
			{URL: "https://agent.example.com/rest", ProtocolBinding: "REST", ProtocolVersion: a2a.Version},
			{URL: "https://agent.example.com/rpc", ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version},
		},
	}
	filtered, err := filterCardForJSONRPC(card, target)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(filtered.SupportedInterfaces) != 1 || filtered.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC {
		t.Fatalf("expected only the JSON-RPC interface, got %+v", filtered.SupportedInterfaces)
	}
}

func TestFilterCardRejectsNonJSONRPC(t *testing.T) {
	target := RemoteAgent{BaseURL: "https://agent.example.com"}
	card := &a2a.AgentCard{SupportedInterfaces: []*a2a.AgentInterface{
		{URL: "https://agent.example.com/rest", ProtocolBinding: "REST", ProtocolVersion: a2a.Version},
	}}
	if _, err := filterCardForJSONRPC(card, target); !errors.Is(err, &GatewayError{Code: CodeA2AProtocolIncompatible}) {
		t.Fatalf("want a2a_protocol_incompatible, got %v", err)
	}
}

func TestFilterCardRejectsUnknownRequiredExtension(t *testing.T) {
	target := RemoteAgent{BaseURL: "https://agent.example.com"}
	card := &a2a.AgentCard{
		Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{URI: "x://ext", Required: true}}},
		SupportedInterfaces: []*a2a.AgentInterface{
			{URL: "https://agent.example.com/rpc", ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version},
		},
	}
	if _, err := filterCardForJSONRPC(card, target); !errors.Is(err, &GatewayError{Code: CodeA2AProtocolIncompatible}) {
		t.Fatalf("want a2a_protocol_incompatible, got %v", err)
	}
}

func TestFilterCardRejectsCrossOriginEndpoint(t *testing.T) {
	target := RemoteAgent{BaseURL: "https://agent.example.com"}
	card := &a2a.AgentCard{SupportedInterfaces: []*a2a.AgentInterface{
		{URL: "https://evil.example.com/rpc", ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: a2a.Version},
	}}
	if _, err := filterCardForJSONRPC(card, target); !errors.Is(err, &GatewayError{Code: CodeA2AProtocolIncompatible}) {
		t.Fatalf("cross-origin endpoint should be rejected, got %v", err)
	}
}
