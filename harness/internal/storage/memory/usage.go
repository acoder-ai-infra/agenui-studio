package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type usageStore struct {
	mu      sync.Mutex
	records map[string]*storage.ModelUsageRecord
}

func newUsageStore() *usageStore {
	return &usageStore{records: map[string]*storage.ModelUsageRecord{}}
}

func (s *usageStore) Record(ctx context.Context, r *storage.ModelUsageRecord) error {
	if r == nil || r.ID == "" {
		return storageInvalid("usage id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if r.TenantID == "" {
		r.TenantID = scope.TenantID
	}
	if err := storage.ValidateUsageIdentifiers(r); err != nil {
		return err
	}
	if err := scope.EnforceTenant(r.TenantID); err != nil {
		return err
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	cp := *r
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ID] = &cp
	return nil
}

func (s *usageStore) List(ctx context.Context, q storage.ModelUsageQuery) ([]*storage.ModelUsageRecord, error) {
	scope := storage.ScopeFromLenient(ctx)
	s.mu.Lock()
	items := make([]*storage.ModelUsageRecord, 0, len(s.records))
	for _, r := range s.records {
		if !usageMatches(scope, q, r) {
			continue
		}
		cp := *r
		items = append(items, &cp)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	if q.Limit > 0 && len(items) > q.Limit {
		items = items[:q.Limit]
	}
	return items, nil
}

func (s *usageStore) Summary(ctx context.Context, q storage.ModelUsageQuery) (storage.ModelUsageSummary, error) {
	items, err := s.List(ctx, q)
	if err != nil {
		return storage.ModelUsageSummary{}, err
	}
	var out storage.ModelUsageSummary
	for _, r := range items {
		accumulateUsage(&out, r)
	}
	if out.TenantID == "" {
		out.TenantID = q.TenantID
	}
	return out, nil
}

func usageMatches(scope storage.Scope, q storage.ModelUsageQuery, r *storage.ModelUsageRecord) bool {
	if scope.TenantID != "" && r.TenantID != scope.TenantID {
		return false
	}
	if q.TenantID != "" && r.TenantID != q.TenantID {
		return false
	}
	if q.SessionID != "" && r.SessionID != q.SessionID {
		return false
	}
	if q.RunID != "" && r.RunID != q.RunID {
		return false
	}
	if q.AgentID != "" && r.AgentID != q.AgentID {
		return false
	}
	if q.Provider != "" && r.Provider != q.Provider {
		return false
	}
	if q.Model != "" && r.Model != q.Model {
		return false
	}
	if !q.From.IsZero() && r.CreatedAt.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !r.CreatedAt.Before(q.To) {
		return false
	}
	return true
}

func accumulateUsage(out *storage.ModelUsageSummary, r *storage.ModelUsageRecord) {
	out.Records++
	if out.TenantID == "" {
		out.TenantID = r.TenantID
	}
	if out.Currency == "" {
		out.Currency = r.Currency
	}
	out.PromptTokens += r.PromptTokens
	out.CompletionTokens += r.CompletionTokens
	out.ReasoningTokens += r.ReasoningTokens
	out.CacheReadTokens += r.CacheReadTokens
	out.CacheWriteTokens += r.CacheWriteTokens
	out.EstimatedCost += r.EstimatedCost
}

var _ storage.ModelUsageStore = (*usageStore)(nil)
