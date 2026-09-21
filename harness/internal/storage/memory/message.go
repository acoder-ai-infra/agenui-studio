package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type messageStore struct {
	mu        sync.RWMutex
	bySession map[string][]*storage.Message
}

func newMessageStore() *messageStore {
	return &messageStore{bySession: make(map[string][]*storage.Message)}
}

func (m *messageStore) Append(ctx context.Context, msg *storage.Message) error {
	if msg == nil || msg.ID == "" || msg.SessionID == "" {
		return storageInvalid("message id and session_id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if msg.TenantID == "" {
		msg.TenantID = scope.TenantID
	}
	if err := storage.ValidateMessageIdentifiers(msg); err != nil {
		return err
	}
	if err := scope.EnforceTenant(msg.TenantID); err != nil {
		return err
	}
	if msg.Visibility == "" {
		msg.Visibility = observability.VisibilityUserVisible
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, messages := range m.bySession {
		for _, existing := range messages {
			if existing.ID == msg.ID {
				return storageConflict("message already exists: " + msg.ID)
			}
		}
	}
	clone := *msg
	m.bySession[msg.SessionID] = append(m.bySession[msg.SessionID], &clone)
	return nil
}

func (m *messageStore) Get(ctx context.Context, id string) (*storage.Message, error) {
	scope := storage.ScopeFromLenient(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, messages := range m.bySession {
		for _, message := range messages {
			if message.ID != id {
				continue
			}
			if err := scope.EnforceTenant(message.TenantID); err != nil {
				return nil, err
			}
			clone := *message
			return &clone, nil
		}
	}
	return nil, storageNotFound("message not found: " + id)
}

func (m *messageStore) List(ctx context.Context, q storage.MessageListQuery) (storage.MessagePage, error) {
	scope := storage.ScopeFromLenient(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()

	src := m.bySession[q.SessionID]
	vis := q.Visibilities
	if len(vis) == 0 {
		vis = []observability.EventVisibility{observability.VisibilityUserVisible}
	}
	visSet := make(map[observability.EventVisibility]bool, len(vis))
	for _, v := range vis {
		visSet[v] = true
	}

	var items []*storage.Message
	for _, msg := range src {
		if scope.TenantID != "" && msg.TenantID != scope.TenantID {
			continue
		}
		if !visSet[msg.Visibility] {
			continue
		}
		clone := *msg
		items = append(items, &clone)
	}
	// order by created_at asc, id asc
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})

	// cursor: before_message_id (older, take the window ending before it) or
	// after_message_id (newer, take the window starting after it).
	if q.AfterMessageID != "" {
		items = sliceAfter(items, q.AfterMessageID)
	}
	if q.BeforeMessageID != "" {
		items = sliceBefore(items, q.BeforeMessageID)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	page := storage.MessagePage{}
	if q.BeforeMessageID != "" && len(items) > limit {
		// keep the newest `limit` older messages
		items = items[len(items)-limit:]
		page.HasMore = true
	} else if q.BeforeMessageID == "" && q.AfterMessageID == "" && len(items) > limit {
		// Initial history loads the newest window, returned in chronological
		// order so the client can render directly and page upward with before.
		items = items[len(items)-limit:]
		page.HasMore = true
	} else if len(items) > limit {
		items = items[:limit]
		page.HasMore = true
	}
	page.Items = items
	if n := len(items); n > 0 {
		page.NextBeforeMessageID = items[0].ID
		page.NextAfterMessageID = items[n-1].ID
	}
	return page, nil
}

func (m *messageStore) ListRecent(ctx context.Context, sessionID string, limit int) ([]*storage.Message, error) {
	page, err := m.List(ctx, storage.MessageListQuery{SessionID: sessionID, Limit: int(^uint(0) >> 1)})
	if err != nil {
		return nil, err
	}
	start := 0
	if limit > 0 && len(page.Items) > limit {
		start = len(page.Items) - limit
	}
	return page.Items[start:], nil
}

func (m *messageStore) GetByIDs(ctx context.Context, sessionID string, ids []string) ([]*storage.Message, error) {
	out := make([]*storage.Message, 0, len(ids))
	for _, id := range ids {
		message, err := m.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if message.SessionID != sessionID {
			return nil, storage.NewError(storage.ErrNotFound, "message does not belong to session: "+id)
		}
		out = append(out, message)
	}
	return out, nil
}

func sliceAfter(items []*storage.Message, id string) []*storage.Message {
	for i, it := range items {
		if it.ID == id {
			return items[i+1:]
		}
	}
	return items
}

func sliceBefore(items []*storage.Message, id string) []*storage.Message {
	for i, it := range items {
		if it.ID == id {
			return items[:i]
		}
	}
	return items
}
