package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// scriptedResolver captures inputs so the test can prove the resolver actually
// ran and its output flowed into the run's identity.
type scriptedResolver struct {
	mu       sync.Mutex
	calls    int
	lastReq  extension.IdentityRequest
	response extension.ResolvedIdentity
	err      error
}

func (s *scriptedResolver) Resolve(_ context.Context, req extension.IdentityRequest) (extension.ResolvedIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastReq = req
	if s.err != nil {
		return extension.ResolvedIdentity{}, s.err
	}
	return s.response, nil
}

type recordingObserver struct {
	events atomic.Int64
}

func (r *recordingObserver) Observe(_ context.Context, _ extension.ProtocolEvent) error {
	r.events.Add(1)
	return nil
}

// TestIdentityResolverExecutesAtStart proves that a registered IdentityResolver
// runs during engineImpl.Start and that its ResolvedIdentity is applied
// before the run's OpenTurn call, so downstream identity is the resolved one
// (not the caller's raw identity).
func TestIdentityResolverExecutesAtStart(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "identity-exec-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath:  configPath,
		Environment: "local",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	resolver := &scriptedResolver{
		response: extension.ResolvedIdentity{
			TenantID:  "resolver-tenant",
			UserID:    "resolver-user",
			SessionID: "resolver-session",
		},
	}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithIdentityResolver("test.identity", resolver),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	// Start with a raw identity; the resolver should override.
	exec, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "caller-tenant", UserID: "caller-user"},
		Input:    harness.TextMessage("resolver test"),
		Metadata: map[string]string{"source": "test"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer exec.Events().Close()

	// The resolver must have been called exactly once.
	resolver.mu.Lock()
	calls := resolver.calls
	lastReq := resolver.lastReq
	resolver.mu.Unlock()
	if calls != 1 {
		t.Fatalf("resolver.calls = %d; want 1", calls)
	}
	if lastReq.BusinessTenantID != "caller-tenant" {
		t.Fatalf("resolver saw BusinessTenantID = %q; want caller-tenant", lastReq.BusinessTenantID)
	}
	if lastReq.Metadata["source"] != "test" {
		t.Fatalf("resolver Metadata missing propagated caller Metadata: %+v", lastReq.Metadata)
	}

	// The run's handle should carry the RESOLVED identity, not the caller
	// identity (proves the resolver's output was applied to the run).
	handle := exec.Handle()
	if handle.Identity.TenantID != "resolver-tenant" {
		t.Fatalf("run TenantID = %q; want resolver-tenant", handle.Identity.TenantID)
	}
	if handle.Identity.SessionID != "resolver-session" {
		t.Fatalf("run SessionID = %q; want resolver-session", handle.Identity.SessionID)
	}
}

// TestEventObserverReceivesEvents proves that a registered EventObserver
// fires for events flowing through the SDK EventStream.
func TestEventObserverReceivesEvents(t *testing.T) {
	t.Setenv("HARNESS_MODEL_GATEWAY_TOKEN", "observer-fake-token")

	repoRoot := findRepoRoot(t)
	tempRoot := t.TempDir()
	configPath := filepath.Join(tempRoot, "harness.yaml")
	if err := os.WriteFile(configPath, []byte(renderIntegrationConfig(t, repoRoot, tempRoot)), 0o600); err != nil {
		t.Fatalf("write harness.yaml: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if _, err := harness.RunMigration(ctx, harness.MigrationOptions{
		ConfigPath:  configPath,
		Environment: "local",
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	observer := &recordingObserver{}
	engine, _, err := harness.Build(ctx,
		harness.WithConfigPath(configPath),
		harness.WithEnvironment("local"),
		harness.WithEventObserver("test.observer", observer),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Close(shutdownCtx)
	}()

	exec, err := engine.Start(ctx, harness.StartRequest{
		Identity: harness.Identity{TenantID: "public", UserID: "observer-tester", SessionID: "observer-session"},
		Input:    harness.TextMessage("observer test"),
	})
	if err != nil {
		// Certain composers can refuse without a real model connection; if
		// Start itself fails we still surface it here but do not fail the
		// observer contract test.
		t.Fatalf("start: %v", err)
	}

	// Drain some events; the local composer emits at least user_message_received,
	// run_created, agent_binding, etc. even without a live model call.
	streamCtx, cancelStream := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStream()
	for i := 0; i < 5; i++ {
		if _, err := exec.Events().Next(streamCtx); err != nil {
			// EOF or context deadline is fine; we just want to observe a few.
			break
		}
	}
	_ = exec.Events().Close()

	if observer.events.Load() == 0 {
		t.Fatalf("EventObserver received 0 events; expected at least one")
	}
}
