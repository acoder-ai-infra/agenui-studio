package mcp

import (
	"context"
	"sync"
)

type InMemoryRegistry struct {
	mu      sync.RWMutex
	servers map[string]ServerDefinition
}

var _ Registry = (*InMemoryRegistry)(nil)

func NewInMemoryRegistry(definitions ...ServerDefinition) (*InMemoryRegistry, error) {
	registry := &InMemoryRegistry{servers: make(map[string]ServerDefinition)}
	for _, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *InMemoryRegistry) Register(definition ServerDefinition) error {
	if definition.ID == "" || definition.Scope == "" {
		return errorsConfiguration("server id and scope are required")
	}
	if definition.Scope == ScopeTenant && definition.TenantID == "" {
		return errorsConfiguration("tenant scoped server requires tenant id")
	}
	if definition.Scope == ScopeUser && (definition.TenantID == "" || definition.UserID == "") {
		return errorsConfiguration("user scoped server requires tenant and user id")
	}
	if err := validateAuthConfig(definition.Auth, definition.HeaderEnv); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.servers[definition.ID]; exists {
		return errorsConfiguration("duplicate server id")
	}
	r.servers[definition.ID] = cloneDefinition(definition)
	return nil
}

func (r *InMemoryRegistry) Get(_ context.Context, serverID string) (ServerDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	definition, ok := r.servers[serverID]
	if !ok {
		return ServerDefinition{}, ErrServerNotFound
	}
	return cloneDefinition(definition), nil
}

func cloneDefinition(definition ServerDefinition) ServerDefinition {
	definition.AllowedAgents = append([]string(nil), definition.AllowedAgents...)
	definition.BlockedAgents = append([]string(nil), definition.BlockedAgents...)
	definition.HITLTools = append([]string(nil), definition.HITLTools...)
	definition.HeaderEnv = cloneStringMap(definition.HeaderEnv)
	definition.Auth.Scopes = append([]string(nil), definition.Auth.Scopes...)
	return definition
}

type StaticClientProvider struct{ Value Client }

var _ ClientProvider = StaticClientProvider{}

func (p StaticClientProvider) Client(context.Context, ServerDefinition, Principal) (Client, error) {
	return p.Value, nil
}

// InMemorySnapshotStore is test/development infrastructure. Production binds
// SnapshotStore to a shared TTL backend so execution can verify the exact
// capability set selected during context assembly.
type InMemorySnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]CapabilitySnapshot
}

var _ SnapshotStore = (*InMemorySnapshotStore)(nil)

func NewInMemorySnapshotStore() *InMemorySnapshotStore {
	return &InMemorySnapshotStore{snapshots: make(map[string]CapabilitySnapshot)}
}

func (s *InMemorySnapshotStore) Save(_ context.Context, snapshot CapabilitySnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.snapshots[snapshot.ID]; exists {
		return errorsConfiguration("duplicate snapshot id")
	}
	snapshot.Tools = cloneTools(snapshot.Tools)
	s.snapshots[snapshot.ID] = snapshot
	return nil
}

func (s *InMemorySnapshotStore) Load(_ context.Context, snapshotID string) (CapabilitySnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.snapshots[snapshotID]
	if !ok {
		return CapabilitySnapshot{}, ErrSnapshotNotFound
	}
	snapshot.Tools = cloneTools(snapshot.Tools)
	return snapshot, nil
}
