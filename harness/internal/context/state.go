package context

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// StateStore manages structured state with path-scoped governance.
type StateStore interface {
	Get(ctx context.Context, sessionID string, path string) (any, error)
	Set(ctx context.Context, req WriteRequest) error
	Delete(ctx context.Context, sessionID string, path string) error
	Snapshot(ctx context.Context, sessionID string) (map[string]any, error)
}

// PathRegistry resolves a state path to its governance spec.
type PathRegistry interface {
	Lookup(path string) (*PathSpec, bool)
}

// ValidateGate validates agent-owned state writes.
type ValidateGate interface {
	Validate(path string, value any) error
}

// StaticPathRegistry is a simple registry backed by a fixed set of PathSpecs.
type StaticPathRegistry struct {
	specs []PathSpec
}

func NewStaticPathRegistry(specs ...PathSpec) *StaticPathRegistry {
	return &StaticPathRegistry{specs: specs}
}

func (r *StaticPathRegistry) Lookup(path string) (*PathSpec, bool) {
	for i := range r.specs {
		if matchPath(r.specs[i].Pattern, path) {
			return &r.specs[i], true
		}
	}
	return nil, false
}

// matchPath supports simple prefix matching with ".*" suffix.
func matchPath(pattern, path string) bool {
	if pattern == path {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, ".*")
		return strings.HasPrefix(path, prefix+".") || path == prefix
	}
	return false
}

// NoopValidateGate accepts all writes.
type NoopValidateGate struct{}

func (NoopValidateGate) Validate(_ string, _ any) error { return nil }

// InMemoryStateStore implements StateStore with path-scoped RBAC.
type InMemoryStateStore struct {
	mu       sync.RWMutex
	data     map[string]map[string]any // sessionID → path → value
	registry PathRegistry
	gate     ValidateGate
}

func NewInMemoryStateStore(registry PathRegistry, gate ValidateGate) *InMemoryStateStore {
	if registry == nil {
		registry = NewStaticPathRegistry()
	}
	if gate == nil {
		gate = NoopValidateGate{}
	}
	return &InMemoryStateStore{
		data:     make(map[string]map[string]any),
		registry: registry,
		gate:     gate,
	}
}

func (s *InMemoryStateStore) Get(_ context.Context, sessionID string, path string) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.data[sessionID]
	if !ok {
		return nil, nil
	}
	val, ok := session[path]
	if !ok {
		return nil, ErrStatePathNotFound
	}
	return val, nil
}

func (s *InMemoryStateStore) Set(_ context.Context, req WriteRequest) error {
	if req.SessionID == "" {
		return ErrSessionIDMissing
	}

	// Check path ownership if registry has the path registered.
	if spec, found := s.registry.Lookup(req.Path); found {
		if err := checkWritePermission(spec, req.Writer); err != nil {
			return err
		}
		// Agent-owned paths go through ValidateGate.
		if spec.Ownership == PathAgentOwned {
			if err := s.gate.Validate(req.Path, req.Value); err != nil {
				return fmt.Errorf("%w: %v", ErrStateValidation, err)
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[req.SessionID] == nil {
		s.data[req.SessionID] = make(map[string]any)
	}
	s.data[req.SessionID][req.Path] = req.Value
	return nil
}

func (s *InMemoryStateStore) Delete(_ context.Context, sessionID string, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.data[sessionID]; ok {
		delete(session, path)
	}
	return nil
}

func (s *InMemoryStateStore) Snapshot(_ context.Context, sessionID string) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.data[sessionID]
	if src == nil {
		return nil, nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

func checkWritePermission(spec *PathSpec, writer Writer) error {
	switch spec.Ownership {
	case PathRuleOwned:
		if writer.Role != "rule" && writer.Role != "harness" {
			return fmt.Errorf("%w: path is rule-owned, writer role=%q", ErrStatePathForbidden, writer.Role)
		}
	case PathAgentOwned:
		if writer.Role != "agent" && writer.Role != "harness" {
			return fmt.Errorf("%w: path is agent-owned, writer role=%q", ErrStatePathForbidden, writer.Role)
		}
	}
	// Shared: any writer allowed.
	return nil
}
