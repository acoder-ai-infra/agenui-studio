package modelgateway

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type TenantQuota struct {
	MaxConcurrent    int     `json:"max_concurrent,omitempty"`
	QPS              int     `json:"qps,omitempty"`
	TokensPerMinute  int     `json:"tokens_per_minute,omitempty"`
	DailyTokenBudget int     `json:"daily_token_budget,omitempty"`
	DailyCostBudget  float64 `json:"daily_cost_budget,omitempty"`
	// Deprecated: token_budget and cost_budget are lifetime counters. Prefer
	// tokens_per_minute, daily_token_budget, and daily_cost_budget so shared
	// tenants do not lock permanently until an operator resets Redis.
	TokenBudget int     `json:"token_budget,omitempty"`
	CostBudget  float64 `json:"cost_budget,omitempty"`
}

type QuotaReservation struct {
	TenantID      string
	ReservationID string
	// Counted reports whether Reserve actually incremented a distributed
	// inflight counter for this reservation. Only such reservations may be
	// decremented on Release; fail-open, empty-quota, and rejected reservations
	// leave it false so Release is a no-op and cannot drive inflight negative.
	// (Exported because the distributed manager in package redisstore must set
	// and read it across the package boundary.)
	Counted bool
}

type TenantQuotaManager interface {
	Reserve(ctx context.Context, req ModelRequest) (QuotaReservation, error)
	Commit(ctx context.Context, res QuotaReservation, usage ModelUsage, cost ModelCost)
	Release(ctx context.Context, res QuotaReservation)
}

type MemoryTenantQuotaManager struct {
	mu      sync.Mutex
	quotas  map[string]TenantQuota
	records map[string]*quotaRecord
	now     func() time.Time
}

type quotaRecord struct {
	inflight     int
	window       time.Time
	qpsCount     int
	minuteWindow time.Time
	minuteTokens int
	day          string
	dayTokens    int
	dayCost      float64
	tokens       int
	cost         float64
}

func NewMemoryTenantQuotaManager(quotas map[string]TenantQuota) *MemoryTenantQuotaManager {
	return &MemoryTenantQuotaManager{quotas: quotas, records: map[string]*quotaRecord{}, now: time.Now}
}

func (m *MemoryTenantQuotaManager) Reserve(_ context.Context, req ModelRequest) (QuotaReservation, error) {
	tenant := req.Trace.TenantID
	if tenant == "" {
		tenant = "default"
	}
	quota, ok := m.quotas[tenant]
	if !ok {
		if defaultQuota, defaultOK := m.quotas["default"]; defaultOK {
			tenant = "default"
			quota = defaultQuota
			ok = true
		}
	}
	if !ok {
		return QuotaReservation{}, missingQuotaPolicyErr(tenant)
	}
	if (quota == TenantQuota{}) {
		return QuotaReservation{TenantID: tenant}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recordLocked(tenant)
	if quota.MaxConcurrent > 0 && rec.inflight >= quota.MaxConcurrent {
		return QuotaReservation{}, &AdapterError{Message: fmt.Sprintf("tenant %s concurrent quota exceeded", tenant), Class: ErrorQuotaExceeded, Retryable: false}
	}
	if quota.QPS > 0 {
		now := m.now().Truncate(time.Second)
		if rec.window.IsZero() || !rec.window.Equal(now) {
			rec.window = now
			rec.qpsCount = 0
		}
		if rec.qpsCount >= quota.QPS {
			return QuotaReservation{}, &AdapterError{Message: fmt.Sprintf("tenant %s qps quota exceeded", tenant), Class: ErrorQuotaExceeded, Retryable: false}
		}
		rec.qpsCount++
	}
	if quota.TokensPerMinute > 0 {
		now := m.now().Truncate(time.Minute)
		if rec.minuteWindow.IsZero() || !rec.minuteWindow.Equal(now) {
			rec.minuteWindow = now
			rec.minuteTokens = 0
		}
		if rec.minuteTokens >= quota.TokensPerMinute {
			return QuotaReservation{}, &AdapterError{Message: fmt.Sprintf("tenant %s tokens_per_minute exceeded", tenant), Class: ErrorQuotaExceeded, Retryable: false}
		}
	}
	if quota.DailyTokenBudget > 0 || quota.DailyCostBudget > 0 {
		day := m.now().UTC().Format("20060102")
		if rec.day != day {
			rec.day = day
			rec.dayTokens = 0
			rec.dayCost = 0
		}
		if quota.DailyTokenBudget > 0 && rec.dayTokens >= quota.DailyTokenBudget {
			return QuotaReservation{}, &AdapterError{Message: fmt.Sprintf("tenant %s daily token budget exceeded", tenant), Class: ErrorBudgetExceeded, Retryable: false}
		}
		if quota.DailyCostBudget > 0 && rec.dayCost >= quota.DailyCostBudget {
			return QuotaReservation{}, &AdapterError{Message: fmt.Sprintf("tenant %s daily cost budget exceeded", tenant), Class: ErrorBudgetExceeded, Retryable: false}
		}
	}
	if quota.TokenBudget > 0 && rec.tokens >= quota.TokenBudget {
		return QuotaReservation{}, &AdapterError{Message: fmt.Sprintf("tenant %s token budget exceeded", tenant), Class: ErrorBudgetExceeded, Retryable: false}
	}
	if quota.CostBudget > 0 && rec.cost >= quota.CostBudget {
		return QuotaReservation{}, &AdapterError{Message: fmt.Sprintf("tenant %s cost budget exceeded", tenant), Class: ErrorBudgetExceeded, Retryable: false}
	}
	rec.inflight++
	return QuotaReservation{TenantID: tenant}, nil
}

func missingQuotaPolicyErr(tenant string) error {
	return &AdapterError{
		Message:   "tenant " + tenant + " has no explicit quota policy",
		Class:     ErrorQuotaExceeded,
		Retryable: false,
	}
}

func (m *MemoryTenantQuotaManager) Commit(_ context.Context, res QuotaReservation, usage ModelUsage, cost ModelCost) {
	if res.TenantID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recordLocked(res.TenantID)
	total := usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	rec.tokens += total
	rec.cost += cost.Estimated
	now := m.now()
	minute := now.Truncate(time.Minute)
	if rec.minuteWindow.IsZero() || !rec.minuteWindow.Equal(minute) {
		rec.minuteWindow = minute
		rec.minuteTokens = 0
	}
	rec.minuteTokens += total
	day := now.UTC().Format("20060102")
	if rec.day != day {
		rec.day = day
		rec.dayTokens = 0
		rec.dayCost = 0
	}
	rec.dayTokens += total
	rec.dayCost += cost.Estimated
}

func (m *MemoryTenantQuotaManager) Release(_ context.Context, res QuotaReservation) {
	if res.TenantID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recordLocked(res.TenantID)
	if rec.inflight > 0 {
		rec.inflight--
	}
}

func (m *MemoryTenantQuotaManager) recordLocked(tenant string) *quotaRecord {
	rec := m.records[tenant]
	if rec == nil {
		rec = &quotaRecord{}
		m.records[tenant] = rec
	}
	return rec
}
