package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type runStore struct {
	mu   sync.RWMutex
	byID map[string]*storage.Run
}

func newRunStore() *runStore {
	return &runStore{byID: make(map[string]*storage.Run)}
}

func (r *runStore) Create(ctx context.Context, run *storage.Run) error {
	if run == nil || run.RunID == "" || run.SessionID == "" {
		return storageInvalid("run_id and session_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if run.TenantID == "" {
		run.TenantID = scope.TenantID
	}
	if err := storage.ValidateRunIdentifiers(run); err != nil {
		return err
	}
	if err := scope.EnforceTenant(run.TenantID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[run.RunID]; ok {
		return storageConflict("run already exists: " + run.RunID)
	}
	clone := *run
	clone.RuntimeBinding = append(json.RawMessage(nil), run.RuntimeBinding...)
	if clone.Status == "" {
		clone.Status = storage.RunStatusCreated
	}
	clone.Version = 1
	r.byID[run.RunID] = &clone
	*run = clone
	return nil
}

func (r *runStore) Get(ctx context.Context, runID string) (*storage.Run, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.byID[runID]
	if !ok {
		return nil, storageNotFound("run not found: " + runID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return nil, err
	}
	clone := *run
	clone.RuntimeBinding = append(json.RawMessage(nil), run.RuntimeBinding...)
	return &clone, nil
}

func (r *runStore) CompareAndSetStatus(ctx context.Context, runID string, from, to storage.RunStatus, mut storage.RunMutation) (*storage.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.byID[runID]
	if !ok {
		return nil, storageNotFound("run not found: " + runID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return nil, err
	}
	if run.Status != from {
		return nil, storageCAS("run status is " + string(run.Status) + ", expected " + string(from))
	}
	if err := storage.ValidateRunTransition(from, to); err != nil {
		return nil, err
	}
	run.Status = to
	if !mut.EndedAt.IsZero() {
		run.EndedAt = mut.EndedAt
	} else if to.IsTerminal() {
		run.EndedAt = time.Now()
	}
	if to == storage.RunStatusRunning && run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	if mut.ErrorCode != "" {
		run.ErrorCode = mut.ErrorCode
	}
	if mut.ErrorMessage != "" {
		run.ErrorMessage = mut.ErrorMessage
	}
	if to != storage.RunStatusResuming {
		run.ResumeAttemptID = ""
	}
	run.Version++
	clone := *run
	clone.RuntimeBinding = append(json.RawMessage(nil), run.RuntimeBinding...)
	return &clone, nil
}

func (r *runStore) BindContextSnapshot(ctx context.Context, runID, ref string) error {
	return r.bindString(ctx, runID, "context snapshot", ref,
		func(run *storage.Run) string { return run.ContextSnapshotRef },
		func(run *storage.Run, value string) { run.ContextSnapshotRef = value })
}

func (r *runStore) BindAgentBinding(ctx context.Context, runID, bindingID string) error {
	if err := storage.ValidateAgentBindingIdentifiers(runID, bindingID); err != nil {
		return err
	}
	return r.bindString(ctx, runID, "agent binding", bindingID,
		func(run *storage.Run) string { return run.AgentBindingID },
		func(run *storage.Run, value string) { run.AgentBindingID = value })
}

func (r *runStore) BindAgentConfig(ctx context.Context, runID, bindingID, configSnapshotRef string) error {
	if err := storage.ValidateAgentBindingIdentifiers(runID, bindingID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, err := r.runForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if run.AgentBindingID != "" && run.AgentBindingID != bindingID {
		return storage.NewError(storage.ErrCASMismatch, "agent binding is already frozen")
	}
	if run.ConfigSnapshotRef != "" && run.ConfigSnapshotRef != configSnapshotRef {
		return storage.NewError(storage.ErrCASMismatch, "config snapshot is already frozen")
	}
	if run.AgentBindingID == bindingID && run.ConfigSnapshotRef == configSnapshotRef {
		return nil
	}
	run.AgentBindingID = bindingID
	run.ConfigSnapshotRef = configSnapshotRef
	run.Version++
	return nil
}

func (r *runStore) BindRuntimeBinding(ctx context.Context, runID string, binding json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, err := r.runForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if len(run.RuntimeBinding) != 0 {
		if bytes.Equal(run.RuntimeBinding, binding) {
			return nil
		}
		return storage.NewError(storage.ErrCASMismatch, "runtime binding is already frozen")
	}
	run.RuntimeBinding = append(json.RawMessage(nil), binding...)
	run.Version++
	return nil
}

func (r *runStore) CompareAndSetRuntimeBinding(ctx context.Context, runID string, expected, replacement json.RawMessage) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.byID[runID]
	if !ok {
		return false, storageNotFound("run not found: " + runID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return false, err
	}
	if run.Status != storage.RunStatusRunning {
		return false, storage.NewError(storage.ErrIllegalTransition, "runtime binding can only be changed while running")
	}
	if !bytes.Equal(run.RuntimeBinding, expected) {
		return false, nil
	}
	run.RuntimeBinding = append(json.RawMessage(nil), replacement...)
	run.Version++
	return true, nil
}

func (r *runStore) bindString(
	ctx context.Context,
	runID, name, desired string,
	current func(*storage.Run) string,
	set func(*storage.Run, string),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, err := r.runForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if existing := current(run); existing != "" {
		if existing == desired {
			return nil
		}
		return storage.NewError(storage.ErrCASMismatch, name+" is already frozen")
	}
	set(run, desired)
	run.Version++
	return nil
}

func (r *runStore) runForBinding(ctx context.Context, runID string) (*storage.Run, error) {
	run, ok := r.byID[runID]
	if !ok {
		return nil, storageNotFound("run not found: " + runID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(run.TenantID); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *runStore) ListBySession(ctx context.Context, sessionID string) ([]*storage.Run, error) {
	scope := storage.ScopeFromLenient(ctx)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*storage.Run
	for _, run := range r.byID {
		if run.SessionID != sessionID {
			continue
		}
		if scope.TenantID != "" && run.TenantID != scope.TenantID {
			continue
		}
		clone := *run
		clone.RuntimeBinding = append(json.RawMessage(nil), run.RuntimeBinding...)
		out = append(out, &clone)
	}
	return out, nil
}
