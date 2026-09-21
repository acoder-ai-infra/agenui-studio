package agentregistry

import (
	"context"
	"errors"
	"testing"
)

// countingControlStore counts GetVersion delegate calls. Only GetVersion is
// exercised; the embedded interface satisfies the rest (and would panic if the
// cache ever forwarded another method, which it must not for this test).
type countingControlStore struct {
	AgentConfigControlStore
	calls   map[string]int
	failFor map[string]bool
}

func (s *countingControlStore) GetVersion(_ context.Context, tenantID, agentID, version string) (AgentConfigVersion, error) {
	key := versionCacheKey(tenantID, agentID, version)
	s.calls[key]++
	if s.failFor[key] {
		return AgentConfigVersion{}, ErrAgentConfigControlNotFound
	}
	return AgentConfigVersion{TenantID: tenantID, AgentID: agentID, Version: version}, nil
}

func TestCachingControlStoreServesHitsWithoutDelegate(t *testing.T) {
	delegate := &countingControlStore{calls: map[string]int{}}
	cache := NewCachingAgentConfigControlStore(delegate, 8)

	for i := 0; i < 3; i++ {
		got, err := cache.GetVersion(context.Background(), "tenant-a", "agent-1", "v1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Version != "v1" {
			t.Fatalf("unexpected version: %#v", got)
		}
	}
	if n := delegate.calls[versionCacheKey("tenant-a", "agent-1", "v1")]; n != 1 {
		t.Fatalf("expected delegate hit once, got %d", n)
	}

	// A different version is a distinct immutable key -> a fresh delegate read.
	if _, err := cache.GetVersion(context.Background(), "tenant-a", "agent-1", "v2"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := delegate.calls[versionCacheKey("tenant-a", "agent-1", "v2")]; n != 1 {
		t.Fatalf("expected v2 delegate hit once, got %d", n)
	}
}

func TestCachingControlStoreDoesNotCacheMisses(t *testing.T) {
	key := versionCacheKey("tenant-a", "agent-1", "v-missing")
	delegate := &countingControlStore{
		calls:   map[string]int{},
		failFor: map[string]bool{key: true},
	}
	cache := NewCachingAgentConfigControlStore(delegate, 8)

	for i := 0; i < 2; i++ {
		_, err := cache.GetVersion(context.Background(), "tenant-a", "agent-1", "v-missing")
		if !errors.Is(err, ErrAgentConfigControlNotFound) {
			t.Fatalf("expected not-found, got %v", err)
		}
	}
	// A not-yet-published version must not be pinned: every miss re-reads.
	if n := delegate.calls[key]; n != 2 {
		t.Fatalf("expected miss to re-read delegate each time, got %d", n)
	}
}
