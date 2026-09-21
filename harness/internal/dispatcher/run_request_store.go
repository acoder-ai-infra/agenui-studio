package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrRunRequestStoreMissing = errors.New("run request store missing")
	ErrRunRequestNotFound     = errors.New("run request not found")
	ErrRunRequestConflict     = errors.New("run request conflicts with frozen request")
	ErrRunRequestRefInvalid   = errors.New("run request ref invalid")
	ErrRunRequestIntegrity    = errors.New("run request integrity check failed")
)

const runRequestRefPrefix = "run-request://"

// RunRequestStore freezes the complete Runtime.Run input before a dispatch is
// made visible. Production adapters persist this value in the canonical Run
// store; queues only carry the returned immutable reference.
type RunRequestStore interface {
	Put(ctx context.Context, req agentruntime.RunRequest) (string, error)
	Get(ctx context.Context, ref string) (agentruntime.RunRequest, error)
}

type frozenRunRequest struct {
	hash string
	data []byte
}

// InMemoryRunRequestStore is the P0 single-process adapter. It is intentionally
// explicit so production composition can reject it in favor of a durable store.
type InMemoryRunRequestStore struct {
	mu      sync.RWMutex
	byRef   map[string]frozenRunRequest
	byRunID map[string]string
}

func NewInMemoryRunRequestStore() *InMemoryRunRequestStore {
	return &InMemoryRunRequestStore{
		byRef:   make(map[string]frozenRunRequest),
		byRunID: make(map[string]string),
	}
}

func (s *InMemoryRunRequestStore) Put(ctx context.Context, req agentruntime.RunRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if req.RunID == "" || req.SessionID == "" || req.Definition.AgentID == "" {
		return "", ErrRunRequestRefInvalid
	}
	frozen, err := freezeRunRequest(req)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(frozen)
	if err != nil {
		return "", fmt.Errorf("marshal frozen run request: %w", err)
	}
	hash := hashRunRequestData(data)
	ref := runRequestRefPrefix + req.RunID

	s.mu.Lock()
	defer s.mu.Unlock()
	if existingRef := s.byRunID[req.RunID]; existingRef != "" {
		existing := s.byRef[existingRef]
		if existing.hash != hash {
			return "", ErrRunRequestConflict
		}
		return existingRef, nil
	}
	s.byRunID[req.RunID] = ref
	s.byRef[ref] = frozenRunRequest{hash: hash, data: append([]byte(nil), data...)}
	return ref, nil
}

func (s *InMemoryRunRequestStore) Get(ctx context.Context, ref string) (agentruntime.RunRequest, error) {
	if err := ctx.Err(); err != nil {
		return agentruntime.RunRequest{}, err
	}
	if len(ref) <= len(runRequestRefPrefix) || ref[:len(runRequestRefPrefix)] != runRequestRefPrefix {
		return agentruntime.RunRequest{}, ErrRunRequestRefInvalid
	}
	s.mu.RLock()
	stored, ok := s.byRef[ref]
	s.mu.RUnlock()
	if !ok {
		return agentruntime.RunRequest{}, ErrRunRequestNotFound
	}
	data := append([]byte(nil), stored.data...)
	if hashRunRequestData(data) != stored.hash {
		return agentruntime.RunRequest{}, ErrRunRequestIntegrity
	}
	var req agentruntime.RunRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return agentruntime.RunRequest{}, fmt.Errorf("decode frozen run request: %w", err)
	}
	if runRequestRefPrefix+req.RunID != ref {
		return agentruntime.RunRequest{}, ErrRunRequestIntegrity
	}
	return req, nil
}

func (*InMemoryRunRequestStore) InMemory() bool { return true }

func freezeRunRequest(req agentruntime.RunRequest) (agentruntime.RunRequest, error) {
	frozen, err := cloneRunRequest(req)
	if err != nil {
		return agentruntime.RunRequest{}, err
	}
	// 队列重试会产生新的 span、request_id 和 baggage。这里只冻结 Run 级稳定身份，
	// 投递级 Trace 在 worker 执行副本中恢复，避免同一 Run 的安全重试被误判为冲突。
	frozen.Trace = stableRunTrace(frozen)
	return frozen, nil
}

func cloneRunRequest(req agentruntime.RunRequest) (agentruntime.RunRequest, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return agentruntime.RunRequest{}, fmt.Errorf("marshal run request clone: %w", err)
	}
	var cloned agentruntime.RunRequest
	if err := json.Unmarshal(data, &cloned); err != nil {
		return agentruntime.RunRequest{}, fmt.Errorf("decode run request clone: %w", err)
	}
	return cloned, nil
}

func stableRunTrace(req agentruntime.RunRequest) observability.TraceContext {
	return observability.TraceContext{
		TraceID:      req.Trace.TraceID,
		SessionID:    req.SessionID,
		RunID:        req.RunID,
		UserID:       req.UserID,
		TenantID:     req.TenantID,
		AgentID:      req.Definition.AgentID,
		AgentType:    req.Definition.AgentType,
		AgentVersion: req.Definition.Version,
		Runtime:      string(req.Definition.Runtime.Type),
	}
}

func hashRunRequestData(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
