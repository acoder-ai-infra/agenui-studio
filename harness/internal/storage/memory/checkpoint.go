package memory

import (
	"context"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type checkpointStore struct {
	mu    sync.RWMutex
	byID  map[string]*storage.CheckpointMeta
	byRun map[string][]string // run_id -> checkpoint_ids in creation order
}

func newCheckpointStore() *checkpointStore {
	return &checkpointStore{
		byID:  make(map[string]*storage.CheckpointMeta),
		byRun: make(map[string][]string),
	}
}

func (c *checkpointStore) Create(ctx context.Context, ck *storage.CheckpointMeta) error {
	if ck == nil || ck.CheckpointID == "" || ck.RunID == "" {
		return storageInvalid("checkpoint_id and run_id required")
	}
	if ck.StateRef == "" {
		return storageInvalid("state_ref required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if ck.TenantID == "" {
		ck.TenantID = scope.TenantID
	}
	if err := storage.ValidateCheckpointIdentifiers(ck); err != nil {
		return err
	}
	if err := scope.EnforceTenant(ck.TenantID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byID[ck.CheckpointID]; ok {
		return storageConflict("checkpoint already exists: " + ck.CheckpointID)
	}
	if ck.CreatedAt.IsZero() {
		ck.CreatedAt = time.Now()
	}
	clone := *ck
	c.byID[ck.CheckpointID] = &clone
	c.byRun[ck.RunID] = append(c.byRun[ck.RunID], ck.CheckpointID)
	return nil
}

func (c *checkpointStore) Get(ctx context.Context, checkpointID string) (*storage.CheckpointMeta, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ck, ok := c.byID[checkpointID]
	if !ok {
		return nil, storageNotFound("checkpoint not found: " + checkpointID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(ck.TenantID); err != nil {
		return nil, err
	}
	clone := *ck
	return &clone, nil
}

func (c *checkpointStore) LatestByRun(ctx context.Context, runID string) (*storage.CheckpointMeta, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := c.byRun[runID]
	if len(ids) == 0 {
		return nil, storageNotFound("no checkpoint for run: " + runID)
	}
	ck := c.byID[ids[len(ids)-1]]
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(ck.TenantID); err != nil {
		return nil, err
	}
	clone := *ck
	return &clone, nil
}

func (c *checkpointStore) CompareAndSwapState(ctx context.Context, checkpointID, expectedStateRef string, replacement *storage.CheckpointMeta) (bool, error) {
	if replacement == nil || replacement.StateRef == "" {
		return false, storageInvalid("replacement state_ref required")
	}
	if err := storage.ValidateCheckpointIdentifiers(replacement); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.byID[checkpointID]
	if !ok {
		return false, storageNotFound("checkpoint not found: " + checkpointID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(current.TenantID); err != nil {
		return false, err
	}
	if current.StateRef != expectedStateRef {
		return false, nil
	}
	if replacement.CheckpointID != checkpointID || replacement.RunID != current.RunID || replacement.TenantID != current.TenantID || replacement.Runtime != current.Runtime || replacement.Type != current.Type {
		return false, storageConflict("checkpoint scope is immutable")
	}
	clone := *replacement
	clone.CreatedAt = current.CreatedAt
	c.byID[checkpointID] = &clone
	return true, nil
}

func (c *checkpointStore) Delete(ctx context.Context, checkpointID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.byID[checkpointID]
	if !ok {
		return nil
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(current.TenantID); err != nil {
		return err
	}
	delete(c.byID, checkpointID)
	ids := c.byRun[current.RunID]
	for index, id := range ids {
		if id == checkpointID {
			c.byRun[current.RunID] = append(ids[:index], ids[index+1:]...)
			break
		}
	}
	return nil
}
