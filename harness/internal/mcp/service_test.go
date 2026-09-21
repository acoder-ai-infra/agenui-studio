package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPFailsClosedWithoutPrincipal(t *testing.T) {
	service := newTestService(t, &fakeClient{tools: []Tool{{Name: "search"}}})
	_, err := service.ResolveSnapshot(context.Background(), Principal{}, "maps")
	if !errors.Is(err, ErrPrincipalRequired) {
		t.Fatalf("expected principal error, got %v", err)
	}
}

func TestManagementDebugPrincipalRequiresTenantUserAndNeverSystemOrAgent(t *testing.T) {
	principal := ManagementDebugPrincipal("tenant", "operator")
	if err := principal.Validate(); err != nil {
		t.Fatal(err)
	}
	if principal.System || principal.AgentID != "" || principal.Purpose != PrincipalPurposeManagementDebug {
		t.Fatalf("management debug principal = %#v", principal)
	}
	for _, invalid := range []Principal{
		ManagementDebugPrincipal("", "operator"),
		ManagementDebugPrincipal("tenant", ""),
		{TenantID: "tenant", UserID: "operator", AgentID: "agent", Purpose: PrincipalPurposeManagementDebug},
		{System: true, Purpose: PrincipalPurposeManagementDebug},
	} {
		if err := invalid.Validate(); !errors.Is(err, ErrPrincipalRequired) {
			t.Fatalf("expected invalid management debug principal %#v, got %v", invalid, err)
		}
	}
}

func TestMCPTenantScopeAndHITL(t *testing.T) {
	service := newTestService(t, &fakeClient{tools: []Tool{{Name: "write"}}})
	_, err := service.CallTool(context.Background(), ToolCallRequest{
		Principal: Principal{TenantID: "other", AgentID: "agent"}, ServerID: "maps", ToolName: "write",
	})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected tenant denial, got %v", err)
	}
	_, err = service.CallTool(context.Background(), ToolCallRequest{
		Principal: Principal{TenantID: "tenant", AgentID: "agent"}, ServerID: "maps", SnapshotID: resolveSnapshotID(t, service), ToolName: "write", ToolCallID: "call-1",
	})
	var approval *ApprovalRequiredError
	if !errors.As(err, &approval) || approval.Request.ToolCallID != "call-1" {
		t.Fatalf("expected typed approval request, got %v", err)
	}
}

func TestMCPApprovedCallStillUsesFrozenCapabilitySnapshot(t *testing.T) {
	client := &fakeClient{tools: []Tool{{Name: "write"}}}
	service := newTestService(t, client)
	service.SetApprovalVerifier(approvalVerifierFunc(func(_ context.Context, _ Principal, request ControlRequest, approvalRef string) error {
		if request.ToolName != "write" || approvalRef != "approval-1" {
			return ErrPermissionDenied
		}
		return nil
	}))
	snapshotID := resolveSnapshotID(t, service)
	result, err := service.CallTool(context.Background(), ToolCallRequest{
		Principal: Principal{TenantID: "tenant", AgentID: "agent"}, ServerID: "maps",
		SnapshotID: snapshotID, ToolName: "write", ToolCallID: "call-1", ApprovalRef: "approval-1",
	})
	if err != nil || string(result.Content) != "ok" || client.calls.Load() != 1 {
		t.Fatalf("approved call failed: result=%q calls=%d err=%v", result.Content, client.calls.Load(), err)
	}
	_, err = service.CallTool(context.Background(), ToolCallRequest{
		Principal: Principal{TenantID: "tenant", AgentID: "agent"}, ServerID: "maps",
		SnapshotID: snapshotID, ToolName: "not-in-snapshot", ApprovalRef: "approval-1",
	})
	if !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("snapshot allowlist was bypassed: %v", err)
	}
}

func TestMCPRejectsTamperedCapabilitySnapshot(t *testing.T) {
	registry, err := NewInMemoryRegistry(ServerDefinition{ID: "maps", Version: "1", Scope: ScopeTenant, TenantID: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	store := NewInMemorySnapshotStore()
	service, err := NewService(registry, StaticClientProvider{Value: &fakeClient{}}, store, func() string { return "unused" })
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{TenantID: "tenant", AgentID: "agent"}
	definition, _ := registry.Get(context.Background(), "maps")
	tampered := CapabilitySnapshot{
		ID: "tampered", ServerID: "maps", ServerVersion: "1", PrincipalHash: hashJSON(principal),
		PolicyHash: serverPolicyHash(definition), CapabilityHash: "sha256:forged", Tools: []Tool{{Name: "search"}},
	}
	if err := store.Save(context.Background(), tampered); err != nil {
		t.Fatal(err)
	}
	_, err = service.CallTool(context.Background(), ToolCallRequest{
		Principal: principal, ServerID: "maps", SnapshotID: "tampered", ToolName: "search",
	})
	if !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("tampered snapshot was accepted: %v", err)
	}
}

func TestMCPCallToolRejectsSnapshotStoreIdentityMismatch(t *testing.T) {
	definition := ServerDefinition{ID: "maps", Version: "v1", Scope: ScopeTenant, TenantID: "tenant"}
	registry, err := NewInMemoryRegistry(definition)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{TenantID: "tenant", AgentID: "agent"}
	snapshot := CapabilitySnapshot{
		ID: "snapshot-other", ServerID: definition.ID, ServerVersion: definition.Version,
		PrincipalHash: hashJSON(principal), PolicyHash: serverPolicyHash(definition),
		Tools: []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	snapshot.CapabilityHash = capabilityHash(snapshot)
	client := &fakeClient{}
	service, err := NewService(
		registry,
		StaticClientProvider{Value: client},
		mismatchedSnapshotStore{snapshot: snapshot},
		func() string { return "unused" },
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.CallTool(context.Background(), ToolCallRequest{
		Principal: principal, ServerID: "maps", SnapshotID: "snapshot-requested", ToolName: "search",
	})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("mismatched snapshot identity was accepted: %v", err)
	}
	if client.calls.Load() != 0 {
		t.Fatal("mismatched snapshot reached MCP client")
	}
}

func TestMCPResolveFrozenSnapshotRejectsDirectoryDrift(t *testing.T) {
	registry := &mutableMCPRegistry{definition: ServerDefinition{
		ID: "maps", Version: "v1", Scope: ScopeTenant, TenantID: "tenant", AllowedAgents: []string{"agent"},
	}}
	store := NewInMemorySnapshotStore()
	service, err := NewService(
		registry,
		StaticClientProvider{Value: &fakeClient{tools: []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}}},
		store,
		func() string { return "snapshot-frozen" },
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{TenantID: "tenant", AgentID: "agent"}
	snapshot, err := service.ResolveSnapshot(context.Background(), principal, "maps")
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := service.ResolveFrozenSnapshot(context.Background(), principal, "maps", snapshot.ID)
	if err != nil || frozen.ID != snapshot.ID || frozen.CapabilityHash != snapshot.CapabilityHash {
		t.Fatalf("reload unchanged frozen snapshot: snapshot=%#v err=%v", frozen, err)
	}

	registry.definition.Version = "v2"
	if _, err := service.ResolveFrozenSnapshot(context.Background(), principal, "maps", snapshot.ID); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("server version drift was accepted: %v", err)
	}
	registry.definition.Version = "v1"
	registry.definition.HITLTools = []string{"search"}
	if _, err := service.ResolveFrozenSnapshot(context.Background(), principal, "maps", snapshot.ID); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("server policy drift was accepted: %v", err)
	}
}

func TestMCPConcurrencyLimitHonorsContext(t *testing.T) {
	client := &fakeClient{tools: []Tool{{Name: "search"}}, block: make(chan struct{})}
	service := newTestService(t, client)
	request := ToolCallRequest{Principal: Principal{TenantID: "tenant", AgentID: "agent"}, ServerID: "maps", SnapshotID: resolveSnapshotID(t, service), ToolName: "search"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = service.CallTool(context.Background(), request)
	}()
	for client.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := service.CallTool(ctx, request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected backpressure timeout, got %v", err)
	}
	close(client.block)
	<-done
	service.mu.Lock()
	remaining := len(service.limiters)
	service.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("idle limiter leaked: %d", remaining)
	}
}

func newTestService(t *testing.T, client Client) *Service {
	t.Helper()
	registry, err := NewInMemoryRegistry(ServerDefinition{
		ID: "maps", Version: "1", Scope: ScopeTenant, TenantID: "tenant", HITLTools: []string{"write"}, MaxConcurrentCalls: 1, MaxResultBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(registry, StaticClientProvider{Value: client}, NewInMemorySnapshotStore(), func() string { return "snapshot-1" })
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func resolveSnapshotID(t *testing.T, service *Service) string {
	t.Helper()
	snapshot, err := service.ResolveSnapshot(context.Background(), Principal{TenantID: "tenant", AgentID: "agent"}, "maps")
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.ID
}

type fakeClient struct {
	tools []Tool
	block chan struct{}
	calls atomic.Int64
}

type mutableMCPRegistry struct {
	definition ServerDefinition
}

type mismatchedSnapshotStore struct {
	snapshot CapabilitySnapshot
}

func (s mismatchedSnapshotStore) Save(context.Context, CapabilitySnapshot) error { return nil }

func (s mismatchedSnapshotStore) Load(context.Context, string) (CapabilitySnapshot, error) {
	return cloneCapabilitySnapshot(s.snapshot), nil
}

func (r *mutableMCPRegistry) Get(_ context.Context, serverID string) (ServerDefinition, error) {
	if r.definition.ID != serverID {
		return ServerDefinition{}, ErrServerNotFound
	}
	return r.definition, nil
}

type approvalVerifierFunc func(context.Context, Principal, ControlRequest, string) error

func (f approvalVerifierFunc) Verify(ctx context.Context, principal Principal, request ControlRequest, approvalRef string) error {
	return f(ctx, principal, request, approvalRef)
}

func (c *fakeClient) ListTools(context.Context) ([]Tool, error) { return cloneTools(c.tools), nil }
func (c *fakeClient) CallTool(ctx context.Context, _ string, _ json.RawMessage, _ CallOptions) (ToolResult, error) {
	c.calls.Add(1)
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		}
	}
	return ToolResult{Content: []byte("ok")}, nil
}
