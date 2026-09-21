package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type sessionStore struct {
	mu   sync.RWMutex
	byID map[string]*storage.Session
}

func newSessionStore() *sessionStore {
	return &sessionStore{byID: make(map[string]*storage.Session)}
}

func (s *sessionStore) Create(ctx context.Context, sess *storage.Session) error {
	if sess == nil || sess.ID == "" {
		return storageInvalid("session id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if sess.TenantID == "" {
		sess.TenantID = scope.TenantID
	}
	if err := storage.ValidateSessionIdentifiers(sess); err != nil {
		return err
	}
	if err := scope.EnforceTenant(sess.TenantID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[sess.ID]; ok {
		return storageConflict("session already exists: " + sess.ID)
	}
	now := time.Now()
	clone := *sess
	if clone.Status == "" {
		clone.Status = storage.SessionStatusActive
	}
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	clone.Version = 1
	s.byID[sess.ID] = &clone
	*sess = clone
	return nil
}

func (s *sessionStore) Get(ctx context.Context, id string) (*storage.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil, storageNotFound("session not found: " + id)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(sess.TenantID); err != nil {
		return nil, err
	}
	clone := *sess
	return &clone, nil
}

func (s *sessionStore) Update(ctx context.Context, sess *storage.Session) error {
	if sess == nil || sess.ID == "" {
		return storageInvalid("session id required")
	}
	if err := storage.ValidateSessionIdentifiers(sess); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[sess.ID]
	if !ok {
		return storageNotFound("session not found: " + sess.ID)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(cur.TenantID); err != nil {
		return err
	}
	if sess.Version != 0 && sess.Version != cur.Version {
		return storageCAS("session version mismatch")
	}
	clone := *sess
	clone.TenantID = cur.TenantID
	clone.CreatedAt = cur.CreatedAt
	clone.UpdatedAt = time.Now()
	clone.Version = cur.Version + 1
	s.byID[sess.ID] = &clone
	*sess = clone
	return nil
}

func (s *sessionStore) Archive(ctx context.Context, id string) error {
	return s.setStatus(ctx, id, storage.SessionStatusArchived)
}

func (s *sessionStore) SoftDelete(ctx context.Context, id string) error {
	return s.setStatus(ctx, id, storage.SessionStatusDeleted)
}

func (s *sessionStore) setStatus(ctx context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[id]
	if !ok {
		return storageNotFound("session not found: " + id)
	}
	if err := storage.ScopeFromLenient(ctx).EnforceTenant(cur.TenantID); err != nil {
		return err
	}
	cur.Status = status
	cur.UpdatedAt = time.Now()
	cur.Version++
	return nil
}

func (s *sessionStore) List(ctx context.Context, q storage.SessionListQuery) (storage.SessionPage, error) {
	scope := storage.ScopeFromLenient(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()

	var items []*storage.Session
	for _, sess := range s.byID {
		if scope.TenantID != "" && sess.TenantID != scope.TenantID {
			continue
		}
		if q.UserID != "" && sess.UserID != q.UserID {
			continue
		}
		if q.AgentID != "" && sess.AgentID != q.AgentID {
			continue
		}
		if !q.IncludeArchived && sess.Status != storage.SessionStatusActive {
			continue
		}
		clone := *sess
		items = append(items, &clone)
	}
	// order by updated_at desc, id desc
	sort.Slice(items, func(i, j int) bool {
		if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		return items[i].ID > items[j].ID
	})
	// apply cursor
	if !q.BeforeUpdatedAt.IsZero() || q.BeforeSessionID != "" {
		filtered := items[:0]
		for _, it := range items {
			if beforeCursor(it, q.BeforeUpdatedAt, q.BeforeSessionID) {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	page := storage.SessionPage{}
	if len(items) > limit {
		items = items[:limit]
		page.HasMore = true
	}
	page.Items = items
	if n := len(items); n > 0 {
		page.NextBeforeUpdatedAt = items[n-1].UpdatedAt
		page.NextBeforeSessionID = items[n-1].ID
	}
	return page, nil
}

// beforeCursor reports whether it sorts strictly after the cursor position in
// (updated_at desc, id desc) order.
func beforeCursor(it *storage.Session, beforeUpdatedAt time.Time, beforeID string) bool {
	if beforeUpdatedAt.IsZero() {
		return true
	}
	if it.UpdatedAt.Before(beforeUpdatedAt) {
		return true
	}
	if it.UpdatedAt.Equal(beforeUpdatedAt) {
		return it.ID < beforeID
	}
	return false
}
