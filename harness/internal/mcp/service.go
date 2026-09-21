package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Service struct {
	registry  Registry
	clients   ClientProvider
	snapshots SnapshotStore
	ids       func() string
	clock     func() time.Time
	approvals ApprovalVerifier

	mu         sync.Mutex
	limiters   map[string]*callLimiter
	approvalMu sync.RWMutex
}

type callLimiter struct {
	semaphore chan struct{}
	refs      int
}

func (s *Service) SetApprovalVerifier(verifier ApprovalVerifier) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	s.approvals = verifier
}

func NewService(registry Registry, clients ClientProvider, snapshots SnapshotStore, ids func() string) (*Service, error) {
	if registry == nil || clients == nil || snapshots == nil || ids == nil {
		return nil, errorsConfiguration("registry, clients, snapshots, and ids are required")
	}
	return &Service{
		registry:  registry,
		clients:   clients,
		snapshots: snapshots,
		ids:       ids,
		clock:     time.Now,
		limiters:  make(map[string]*callLimiter),
	}, nil
}

func (s *Service) ResolveSnapshot(ctx context.Context, principal Principal, serverID string) (CapabilitySnapshot, error) {
	definition, err := s.authorize(ctx, principal, serverID)
	if err != nil {
		return CapabilitySnapshot{}, err
	}
	client, err := s.clients.Client(ctx, definition, principal)
	if err != nil {
		return CapabilitySnapshot{}, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return CapabilitySnapshot{}, err
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	for i, tool := range tools {
		if tool.Name == "" || (len(tool.InputSchema) > 0 && !json.Valid(tool.InputSchema)) {
			return CapabilitySnapshot{}, fmt.Errorf("%w: invalid tool schema at index=%d", ErrToolNotAllowed, i)
		}
		if i > 0 && tools[i-1].Name == tool.Name {
			return CapabilitySnapshot{}, fmt.Errorf("%w: duplicate tool=%s", ErrToolNotAllowed, tool.Name)
		}
	}
	snapshotID := s.ids()
	if snapshotID == "" {
		return CapabilitySnapshot{}, errorsConfiguration("snapshot id is required")
	}
	snapshot := CapabilitySnapshot{
		ID:            snapshotID,
		ServerID:      definition.ID,
		ServerVersion: definition.Version,
		PrincipalHash: hashJSON(principal),
		PolicyHash:    serverPolicyHash(definition),
		Tools:         cloneTools(tools),
		CreatedAt:     s.clock(),
	}
	snapshot.CapabilityHash = capabilityHash(snapshot)
	if err := s.snapshots.Save(ctx, snapshot); err != nil {
		return CapabilitySnapshot{}, err
	}
	return snapshot, nil
}

// ResolveFrozenSnapshot reloads the exact snapshot selected by an existing
// Run and revalidates it against the current principal and server policy.
// Resume must never turn a serverID back into a fresh, broader capability set.
func (s *Service) ResolveFrozenSnapshot(ctx context.Context, principal Principal, serverID, snapshotID string) (CapabilitySnapshot, error) {
	definition, err := s.authorize(ctx, principal, serverID)
	if err != nil {
		return CapabilitySnapshot{}, err
	}
	if snapshotID == "" {
		return CapabilitySnapshot{}, ErrSnapshotRequired
	}
	snapshot, err := s.snapshots.Load(ctx, snapshotID)
	if err != nil {
		return CapabilitySnapshot{}, err
	}
	if snapshot.ID != snapshotID || snapshot.ServerID != serverID || snapshot.PrincipalHash != hashJSON(principal) {
		return CapabilitySnapshot{}, ErrPermissionDenied
	}
	if snapshot.ServerVersion != definition.Version || snapshot.PolicyHash != serverPolicyHash(definition) || snapshot.CapabilityHash != capabilityHash(snapshot) {
		return CapabilitySnapshot{}, ErrSnapshotStale
	}
	return cloneCapabilitySnapshot(snapshot), nil
}

func (s *Service) CallTool(ctx context.Context, req ToolCallRequest) (ToolResult, error) {
	definition, err := s.authorize(ctx, req.Principal, req.ServerID)
	if err != nil {
		return ToolResult{}, err
	}
	if req.SnapshotID == "" {
		return ToolResult{}, ErrSnapshotRequired
	}
	snapshot, err := s.snapshots.Load(ctx, req.SnapshotID)
	if err != nil {
		return ToolResult{}, err
	}
	// SnapshotStore 是外部持久化边界，不能仅依赖 Load 的入参约定；
	// 返回值必须仍是调用方指定的那一份冻结快照。
	if snapshot.ID != req.SnapshotID || snapshot.ServerID != req.ServerID || snapshot.PrincipalHash != hashJSON(req.Principal) {
		return ToolResult{}, ErrPermissionDenied
	}
	if snapshot.ServerVersion != definition.Version || snapshot.PolicyHash != serverPolicyHash(definition) || snapshot.CapabilityHash != capabilityHash(snapshot) {
		return ToolResult{}, ErrSnapshotStale
	}
	if !snapshotAllowsTool(snapshot, req.ToolName) {
		return ToolResult{}, fmt.Errorf("%w: %s", ErrToolNotAllowed, req.ToolName)
	}
	if contains(definition.HITLTools, req.ToolName) {
		control := ControlRequest{
			Type:       "permission",
			ServerID:   req.ServerID,
			ToolName:   req.ToolName,
			ToolCallID: req.ToolCallID,
			SnapshotID: req.SnapshotID,
			Payload:    cloneRaw(req.Arguments),
		}
		s.approvalMu.RLock()
		approvals := s.approvals
		s.approvalMu.RUnlock()
		if approvals == nil || req.ApprovalRef == "" {
			return ToolResult{}, &ApprovalRequiredError{Request: control}
		}
		if err := approvals.Verify(ctx, req.Principal, control, req.ApprovalRef); err != nil {
			return ToolResult{}, ErrPermissionDenied
		}
	}
	client, err := s.clients.Client(ctx, definition, req.Principal)
	if err != nil {
		return ToolResult{}, err
	}
	release, err := s.acquire(ctx, definition)
	if err != nil {
		return ToolResult{}, err
	}
	defer release()
	result, err := client.CallTool(ctx, req.ToolName, cloneRaw(req.Arguments), CallOptions{MaxResultBytes: definition.MaxResultBytes})
	if err != nil {
		return ToolResult{}, err
	}
	if definition.MaxResultBytes > 0 && int64(len(result.Content)) > definition.MaxResultBytes {
		return ToolResult{}, fmt.Errorf("%w: got=%d max=%d", ErrResultTooLarge, len(result.Content), definition.MaxResultBytes)
	}
	result.Content = append([]byte(nil), result.Content...)
	return result, nil
}

func snapshotAllowsTool(snapshot CapabilitySnapshot, toolName string) bool {
	for _, tool := range snapshot.Tools {
		if tool.Name == toolName {
			return true
		}
	}
	return false
}

func (s *Service) authorize(ctx context.Context, principal Principal, serverID string) (ServerDefinition, error) {
	if err := principal.Validate(); err != nil {
		return ServerDefinition{}, err
	}
	var definition ServerDefinition
	var err error
	if scoped, ok := s.registry.(PrincipalRegistry); ok {
		definition, err = scoped.GetForPrincipal(ctx, principal, serverID)
	} else {
		definition, err = s.registry.Get(ctx, serverID)
	}
	if err != nil {
		return ServerDefinition{}, err
	}
	if principal.System {
		return definition, nil
	}
	if contains(definition.BlockedAgents, principal.AgentID) {
		return ServerDefinition{}, ErrPermissionDenied
	}
	if len(definition.AllowedAgents) > 0 && !contains(definition.AllowedAgents, principal.AgentID) {
		return ServerDefinition{}, ErrPermissionDenied
	}
	switch definition.Scope {
	case ScopeSystem:
		return definition, nil
	case ScopeTenant:
		if definition.TenantID == "" || definition.TenantID != principal.TenantID {
			return ServerDefinition{}, ErrPermissionDenied
		}
	case ScopeUser:
		if definition.TenantID == "" || definition.UserID == "" || definition.TenantID != principal.TenantID || definition.UserID != principal.UserID {
			return ServerDefinition{}, ErrPermissionDenied
		}
	default:
		return ServerDefinition{}, ErrPermissionDenied
	}
	return definition, nil
}

func (s *Service) acquire(ctx context.Context, definition ServerDefinition) (func(), error) {
	limit := definition.MaxConcurrentCalls
	if limit <= 0 {
		limit = 32
	}
	s.mu.Lock()
	limiterKey := definition.TenantID + "\x00" + definition.ID
	limiter, ok := s.limiters[limiterKey]
	if !ok {
		limiter = &callLimiter{semaphore: make(chan struct{}, limit)}
		s.limiters[limiterKey] = limiter
	}
	limiter.refs++
	s.mu.Unlock()
	select {
	case limiter.semaphore <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-limiter.semaphore
				s.releaseLimiter(limiterKey, limiter)
			})
		}, nil
	case <-ctx.Done():
		s.releaseLimiter(limiterKey, limiter)
		return nil, ctx.Err()
	}
}

func (s *Service) releaseLimiter(serverID string, limiter *callLimiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limiter.refs--
	if limiter.refs == 0 && s.limiters[serverID] == limiter {
		delete(s.limiters, serverID)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func hashJSON(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func serverPolicyHash(definition ServerDefinition) string {
	return hashJSON(struct {
		Scope              Scope
		TenantID           string
		UserID             string
		AllowedAgents      []string
		BlockedAgents      []string
		HITLTools          []string
		MaxConcurrentCalls int
		MaxResultBytes     int64
		Transport          string
		Endpoint           string
		ProtocolVersion    string
		HeaderEnv          map[string]string
		Auth               AuthConfig
	}{
		definition.Scope, definition.TenantID, definition.UserID,
		definition.AllowedAgents, definition.BlockedAgents, definition.HITLTools,
		definition.MaxConcurrentCalls, definition.MaxResultBytes,
		definition.Transport, definition.Endpoint, definition.ProtocolVersion, definition.HeaderEnv, definition.Auth,
	})
}

func capabilityHash(snapshot CapabilitySnapshot) string {
	return hashJSON(struct {
		ServerID      string
		ServerVersion string
		PrincipalHash string
		PolicyHash    string
		Tools         []Tool
	}{snapshot.ServerID, snapshot.ServerVersion, snapshot.PrincipalHash, snapshot.PolicyHash, snapshot.Tools})
}

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func cloneTools(tools []Tool) []Tool {
	out := make([]Tool, len(tools))
	copy(out, tools)
	for i := range out {
		out[i].InputSchema = cloneRaw(tools[i].InputSchema)
	}
	return out
}

func cloneCapabilitySnapshot(snapshot CapabilitySnapshot) CapabilitySnapshot {
	snapshot.Tools = cloneTools(snapshot.Tools)
	return snapshot
}

func errorsConfiguration(message string) error { return fmt.Errorf("mcp configuration: %s", message) }
