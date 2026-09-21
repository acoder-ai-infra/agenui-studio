package memory

import (
	"context"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// controlStatusPending mirrors internal/control's pending status string without
// importing that package (control -> storage is the only allowed direction).
const controlStatusPending = "pending"
const controlStatusExpired = "expired"

type controlStore struct {
	mu   sync.Mutex
	byID map[string]*storage.ControlRequest
}

func newControlStore() *controlStore {
	return &controlStore{byID: make(map[string]*storage.ControlRequest)}
}

func (c *controlStore) Create(ctx context.Context, cr *storage.ControlRequest) error {
	if cr == nil || cr.RequestID == "" || cr.RunID == "" {
		return storageInvalid("request_id and run_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if cr.TenantID == "" {
		cr.TenantID = scope.TenantID
	}
	if err := storage.ValidateControlRequestIdentifiers(cr); err != nil {
		return err
	}
	if err := scope.EnforceTenant(cr.TenantID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byID[cr.RequestID]; ok {
		return storageConflict("control request already exists: " + cr.RequestID)
	}
	if cr.Status == "" {
		cr.Status = controlStatusPending
	}
	if cr.SchemaVersion == "" {
		cr.SchemaVersion = storage.ControlRequestSchemaVersion
	}
	if cr.CreatedAt.IsZero() {
		cr.CreatedAt = time.Now()
	}
	cr.Version = 1
	clone := *cr
	c.byID[cr.RequestID] = &clone
	return nil
}

func (c *controlStore) Get(ctx context.Context, requestID string) (*storage.ControlRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cr, ok := c.byID[requestID]
	if !ok {
		return nil, storageNotFound("control request not found: " + requestID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(cr.TenantID); err != nil {
		return nil, err
	}
	clone := *cr
	return &clone, nil
}

func (c *controlStore) ListByRun(ctx context.Context, runID string) ([]*storage.ControlRequest, error) {
	if runID == "" {
		return nil, storageInvalid("run_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*storage.ControlRequest
	for _, cr := range c.byID {
		if cr.RunID != runID {
			continue
		}
		if err := scope.EnforceTenant(cr.TenantID); err != nil {
			return nil, err
		}
		clone := *cr
		out = append(out, &clone)
	}
	return out, nil
}

// CompareAndAnswer moves a request from expectFrom to a terminal status,
// succeeding at most once (CAS). A second attempt returns ErrCASMismatch so the
// caller can fetch and return the first result idempotently.
func (c *controlStore) CompareAndAnswer(ctx context.Context, requestID, expectFrom, to, responseRef string) (*storage.ControlRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cr, ok := c.byID[requestID]
	if !ok {
		return nil, storageNotFound("control request not found: " + requestID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(cr.TenantID); err != nil {
		return nil, err
	}
	if cr.Status != expectFrom {
		return nil, storageCAS("control request status is " + cr.Status + ", expected " + expectFrom)
	}
	cr.Status = to
	if responseRef != "" {
		cr.ResponseRef = responseRef
	}
	cr.Version++
	clone := *cr
	return &clone, nil
}

func (c *controlStore) ExpirePending(ctx context.Context, now time.Time) ([]*storage.ControlRequest, error) {
	scope := storage.ScopeFromLenient(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	var expired []*storage.ControlRequest
	for _, cr := range c.byID {
		if scope.TenantID != "" && cr.TenantID != scope.TenantID {
			continue
		}
		if cr.Status != controlStatusPending {
			continue
		}
		if cr.ExpiresAt.IsZero() || cr.ExpiresAt.After(now) {
			continue
		}
		cr.Status = controlStatusExpired
		cr.Version++
		clone := *cr
		expired = append(expired, &clone)
	}
	return expired, nil
}
