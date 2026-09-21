package redisstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// QuotaLogger is the minimal logging interface used by TenantQuotaManager.
// It allows the composition root to wire any structured logger without
// creating an import cycle to observability.
type QuotaLogger interface {
	Warnf(ctx context.Context, format string, args ...interface{})
}

// TenantQuotaManager is a distributed modelgateway.TenantQuotaManager backed by
// Redis. Concurrency, QPS, token and cost accounting live in Redis so that a
// tenant's quota is enforced across every gateway process, not per-instance.
//
// Reserve runs a single Lua script so the concurrency/QPS checks-and-increments
// are atomic (no check-then-act race across processes). Commit uses one Lua
// transaction keyed by the reservation ID, making token/cost accounting atomic
// and idempotent across retries.
//
// When Redis is unavailable, behavior depends on Config.QuotaFailOpen:
//   - false (default): fail closed — return an error, blocking the request
//   - true: fail open — allow the request through (logged as degraded mode)
type TenantQuotaManager struct {
	client    Client
	prefix    string
	quotas    map[string]modelgateway.TenantQuota
	now       func() time.Time
	leaseTTL  time.Duration
	commitTTL time.Duration
	failOpen  bool
	logger    QuotaLogger
}

const (
	quotaLeaseCleanupMargin = 5 * time.Second
	defaultQuotaCommitTTL   = 5 * time.Minute
	quotaCommitAttempts     = 3
	minuteQuotaTTL          = 2 * time.Minute
	dayQuotaTTL             = 48 * time.Hour
)

// NewTenantQuotaManager builds a Redis-backed quota manager for the given static
// per-tenant quotas (same shape as the in-memory manager). Redis failure behavior
// is controlled by cfg.QuotaFailOpen (false = fail closed, true = fail open).
// logger may be nil; if non-nil, degraded-mode events are logged at Warn level.
func NewTenantQuotaManager(client Client, cfg Config, quotas map[string]modelgateway.TenantQuota, logger QuotaLogger) *TenantQuotaManager {
	leaseTTL := cfg.QuotaLeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = 2 * time.Minute
	}
	commitTTL := cfg.QuotaCommitTTL
	if commitTTL <= 0 {
		commitTTL = defaultQuotaCommitTTL
	}
	return &TenantQuotaManager{
		client:    client,
		prefix:    prefix(cfg) + "quota:",
		quotas:    quotas,
		now:       time.Now,
		leaseTTL:  leaseTTL,
		commitTTL: commitTTL,
		failOpen:  cfg.QuotaFailOpen,
		logger:    logger,
	}
}

// All quota keys wrap the tenant in a "{...}" hash tag so that on a Redis
// Cluster every key for a tenant hashes to the same slot. Redis only hashes the
// substring inside the first {...}, so this keeps the multi-key reserveScript
// EVAL (inflight + qps) single-slot and avoids CROSSSLOT errors.
func (m *TenantQuotaManager) inflightKey(t string) string {
	return m.prefix + "{" + t + "}:inflight"
}
func (m *TenantQuotaManager) qpsKey(t string, sec int64) string {
	return m.prefix + "{" + t + "}:qps:" + strconv.FormatInt(sec, 10)
}
func (m *TenantQuotaManager) tokensKey(t string) string { return m.prefix + "{" + t + "}:tokens" } // lifetime, legacy/observability
func (m *TenantQuotaManager) costKey(t string) string   { return m.prefix + "{" + t + "}:cost" }   // lifetime, legacy/observability
func (m *TenantQuotaManager) tokensMinuteKey(t string, minute int64) string {
	return m.prefix + "{" + t + "}:tokens:minute:" + strconv.FormatInt(minute, 10)
}
func (m *TenantQuotaManager) tokensDayKey(t, day string) string {
	return m.prefix + "{" + t + "}:tokens:day:" + day
}
func (m *TenantQuotaManager) costDayKey(t, day string) string {
	return m.prefix + "{" + t + "}:cost:day:" + day
}
func (m *TenantQuotaManager) commitKey(t, reservationID string) string {
	return m.prefix + "{" + t + "}:commit:" + reservationID
}

// reserveScript atomically enforces concurrency + QPS. Inflight calls are
// expiring ZSET leases, so process death cannot leave a permanent quota zombie.
// KEYS[1]=leases KEYS[2]=qps-window
// ARGV=maxConcurrent,qps,qpsTTL,nowMs,expiresMs,reservationID
// Returns: 0 ok, 1 concurrency exceeded, 2 qps exceeded.
var reserveScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[4])
local inflight = tonumber(redis.call('ZCARD', KEYS[1]) or '0')
local maxc = tonumber(ARGV[1])
if maxc > 0 and inflight >= maxc then
  return 1
end
local qps = tonumber(ARGV[2])
if qps > 0 then
  local cur = tonumber(redis.call('GET', KEYS[2]) or '0')
  if cur >= qps then
    return 2
  end
  redis.call('INCR', KEYS[2])
  redis.call('EXPIRE', KEYS[2], tonumber(ARGV[3]))
end
redis.call('ZADD', KEYS[1], ARGV[5], ARGV[6])
local latest = redis.call('ZRANGE', KEYS[1], -1, -1, 'WITHSCORES')
if latest[2] then
  redis.call('PEXPIREAT', KEYS[1], math.ceil(tonumber(latest[2])))
end
return 0
`)

// commitScript validates all counters before changing any of them, then stores
// updated lifetime observability counters, windowed quota counters, and the
// reservation marker in one Redis transaction. A repeated Commit with the same
// reservation and payload is a no-op; reusing the reservation with different
// usage is rejected.
// KEYS[1]=lifetime-tokens KEYS[2]=lifetime-cost KEYS[3]=minute-tokens
// KEYS[4]=day-tokens KEYS[5]=day-cost KEYS[6]=reservation-marker
// ARGV[1]=token-delta ARGV[2]=cost-delta ARGV[3]=payload ARGV[4]=marker-ttl
// ARGV[5]=minute-ttl ARGV[6]=day-ttl
var commitScript = redis.NewScript(`
local function finite_number(raw, label)
  local value = tonumber(raw)
  if not value or value ~= value or value == math.huge or value == -math.huge then
    return nil, redis.error_reply(label .. ' is not finite')
  end
  return value, nil
end
local function prepare_counter(key, delta, label, ttl)
  local current, current_err = finite_number(redis.call('GET', key) or '0', label)
  if current_err then
    return nil, current_err
  end
  local next_value, next_err = finite_number(current + delta, 'quota next ' .. label)
  if next_err then
    return nil, next_err
  end
  return {key, next_value, ttl}, nil
end
local previous = redis.call('GET', KEYS[6])
if previous then
  if previous == ARGV[3] then
    return 0
  end
  return redis.error_reply('quota reservation payload conflict')
end
local token_delta, token_delta_err = finite_number(ARGV[1], 'quota token delta')
if token_delta_err then
  return token_delta_err
end
local cost_delta, cost_delta_err = finite_number(ARGV[2], 'quota cost delta')
if cost_delta_err then
  return cost_delta_err
end
local updates = {}
for idx, spec in ipairs({
  {KEYS[1], token_delta, 'lifetime token counter', 0},
  {KEYS[2], cost_delta, 'lifetime cost counter', 0},
  {KEYS[3], token_delta, 'minute token counter', ARGV[5]},
  {KEYS[4], token_delta, 'day token counter', ARGV[6]},
  {KEYS[5], cost_delta, 'day cost counter', ARGV[6]},
}) do
  local update, prepare_err = prepare_counter(spec[1], spec[2], spec[3], spec[4])
  if prepare_err then
    return prepare_err
  end
  updates[idx] = update
end
for _, update in ipairs(updates) do
  redis.call('SET', update[1], update[2])
  local ttl_num = tonumber(update[3])
  if ttl_num and ttl_num > 0 then
    redis.call('EXPIRE', update[1], ttl_num)
  end
end
redis.call('SET', KEYS[6], ARGV[3], 'EX', tonumber(ARGV[4]))
return 1
`)

func (m *TenantQuotaManager) Reserve(ctx context.Context, req modelgateway.ModelRequest) (modelgateway.QuotaReservation, error) {
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
		return modelgateway.QuotaReservation{}, missingQuotaPolicyErr(tenant)
	}
	reservationID, err := newReservationID()
	if err != nil {
		return modelgateway.QuotaReservation{}, err
	}
	if (quota == modelgateway.TenantQuota{}) {
		return modelgateway.QuotaReservation{TenantID: tenant, ReservationID: reservationID}, nil
	}

	now := m.now()
	minute := now.Unix() / 60
	day := now.UTC().Format("20060102")
	if quota.TokensPerMinute > 0 {
		if err := m.checkIntCounter(ctx, tenant, m.tokensMinuteKey(tenant, minute), quota.TokensPerMinute, "tokens_per_minute", false); err != nil {
			return modelgateway.QuotaReservation{}, err
		}
	}
	if quota.DailyTokenBudget > 0 {
		if err := m.checkIntCounter(ctx, tenant, m.tokensDayKey(tenant, day), quota.DailyTokenBudget, "daily token budget", true); err != nil {
			return modelgateway.QuotaReservation{}, err
		}
	}
	if quota.DailyCostBudget > 0 {
		if err := m.checkFloatCounter(ctx, tenant, m.costDayKey(tenant, day), quota.DailyCostBudget, "daily cost budget", true); err != nil {
			return modelgateway.QuotaReservation{}, err
		}
	}
	// Deprecated lifetime budgets are still honored for backward compatibility.
	// Prefer windowed limits above for shared tenants.
	if quota.TokenBudget > 0 {
		if err := m.checkIntCounter(ctx, tenant, m.tokensKey(tenant), quota.TokenBudget, "token budget", true); err != nil {
			return modelgateway.QuotaReservation{}, err
		}
	}
	if quota.CostBudget > 0 {
		if err := m.checkFloatCounter(ctx, tenant, m.costKey(tenant), quota.CostBudget, "cost budget", true); err != nil {
			return modelgateway.QuotaReservation{}, err
		}
	}

	sec := now.Unix()
	leaseTTL := m.effectiveLeaseTTL(ctx, req, now)
	res, err := reserveScript.Run(ctx, m.client,
		[]string{m.inflightKey(tenant), m.qpsKey(tenant, sec)},
		quota.MaxConcurrent, quota.QPS, 2, now.UnixMilli(), now.Add(leaseTTL).UnixMilli(), reservationID,
	).Int()
	if err != nil {
		if m.failOpen {
			// Fail-open: allow the request through but the EventStore/usage ledger
			// still records truth. Log degraded mode so operators can detect
			// quota enforcement gaps.
			m.logWarn(ctx, "quota reserve failed (fail-open, request allowed)", err, tenant)
			return modelgateway.QuotaReservation{TenantID: tenant, ReservationID: reservationID}, nil
		}
		// Fail-closed (default): reject the request when quota cannot be verified.
		// This is the safe default for hard cost/security boundaries.
		return modelgateway.QuotaReservation{}, &modelgateway.AdapterError{
			Message:   fmt.Sprintf("tenant %s quota unavailable: %v", tenant, err),
			Class:     modelgateway.ErrorQuotaExceeded,
			Retryable: true,
		}
	}
	switch res {
	case 1:
		return modelgateway.QuotaReservation{}, concurrentErr(tenant)
	case 2:
		return modelgateway.QuotaReservation{}, qpsErr(tenant)
	}
	// Only this path saw the Lua script INCR inflight, so only it may be
	// decremented on Release.
	return modelgateway.QuotaReservation{TenantID: tenant, ReservationID: reservationID, Counted: true}, nil
}

func (m *TenantQuotaManager) Commit(ctx context.Context, res modelgateway.QuotaReservation, usage modelgateway.ModelUsage, cost modelgateway.ModelCost) {
	if res.TenantID == "" || res.ReservationID == "" {
		return
	}
	total := usage.PromptTokens + usage.CompletionTokens + usage.ReasoningTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	estimated := cost.Estimated
	if math.IsNaN(estimated) || math.IsInf(estimated, 0) {
		m.logWarn(ctx, "quota commit rejected non-finite cost", fmt.Errorf("estimated cost must be finite"), res.TenantID)
		return
	}
	if total < 0 {
		total = 0
	}
	if estimated < 0 {
		estimated = 0
	}
	payload := strconv.Itoa(total) + "|" + strconv.FormatFloat(estimated, 'g', -1, 64)
	now := m.now()
	minute := now.Unix() / 60
	day := now.UTC().Format("20060102")
	var err error
	for attempt := 0; attempt < quotaCommitAttempts; attempt++ {
		_, err = commitScript.Run(ctx, m.client,
			[]string{
				m.tokensKey(res.TenantID),
				m.costKey(res.TenantID),
				m.tokensMinuteKey(res.TenantID, minute),
				m.tokensDayKey(res.TenantID, day),
				m.costDayKey(res.TenantID, day),
				m.commitKey(res.TenantID, res.ReservationID),
			},
			total,
			strconv.FormatFloat(estimated, 'g', -1, 64),
			payload,
			max(int64(m.commitTTL/time.Second), 1),
			max(int64(minuteQuotaTTL/time.Second), 1),
			max(int64(dayQuotaTTL/time.Second), 1),
		).Int()
		if err == nil {
			return
		}
	}
	m.logWarn(ctx, "quota atomic commit failed after retries — Redis counters not updated", err, res.TenantID)
}

func missingQuotaPolicyErr(tenant string) error {
	return &modelgateway.AdapterError{
		Message:   fmt.Sprintf("tenant %s has no explicit quota policy", tenant),
		Class:     modelgateway.ErrorQuotaExceeded,
		Retryable: false,
	}
}

func (m *TenantQuotaManager) checkIntCounter(ctx context.Context, tenant, key string, limit int, label string, budget bool) error {
	used, getErr := m.client.Get(ctx, key).Int()
	if getErr != nil && getErr != redis.Nil {
		return m.counterUnavailable(ctx, tenant, label, getErr)
	}
	if getErr == nil && used >= limit {
		if budget {
			return budgetErr(tenant, label)
		}
		return quotaErr(tenant, label)
	}
	return nil
}

func (m *TenantQuotaManager) checkFloatCounter(ctx context.Context, tenant, key string, limit float64, label string, budget bool) error {
	used, getErr := m.client.Get(ctx, key).Float64()
	if getErr != nil && getErr != redis.Nil {
		return m.counterUnavailable(ctx, tenant, label, getErr)
	}
	if getErr == nil && used >= limit {
		if budget {
			return budgetErr(tenant, label)
		}
		return quotaErr(tenant, label)
	}
	return nil
}

func (m *TenantQuotaManager) counterUnavailable(ctx context.Context, tenant, label string, err error) error {
	if m.failOpen {
		m.logWarn(ctx, "quota "+label+" check failed (fail-open)", err, tenant)
		return nil
	}
	return &modelgateway.AdapterError{
		Message:   fmt.Sprintf("tenant %s %s unavailable: %v", tenant, label, err),
		Class:     modelgateway.ErrorQuotaExceeded,
		Retryable: true,
	}
}

func (m *TenantQuotaManager) effectiveLeaseTTL(ctx context.Context, req modelgateway.ModelRequest, now time.Time) time.Duration {
	leaseTTL := m.leaseTTL
	if req.TimeoutMS > 0 {
		leaseTTL = maxDuration(leaseTTL, time.Duration(req.TimeoutMS)*time.Millisecond+quotaLeaseCleanupMargin)
	}
	if deadline, ok := ctx.Deadline(); ok {
		leaseTTL = maxDuration(leaseTTL, deadline.Sub(now)+quotaLeaseCleanupMargin)
	}
	return leaseTTL
}

func maxDuration(left, right time.Duration) time.Duration {
	if right > left {
		return right
	}
	return left
}

func (m *TenantQuotaManager) Release(ctx context.Context, res modelgateway.QuotaReservation) {
	// Never decrement inflight for a reservation that did not increment it:
	// fail-open (Redis error), empty-quota short-circuit, and quota-exceeded
	// rejections all leave Counted false and must be Release no-ops. Only the
	// Reserve success path that ran the Lua INCR sets Counted.
	if res.TenantID == "" || !res.Counted {
		return
	}
	if err := m.client.ZRem(ctx, m.inflightKey(res.TenantID), res.ReservationID).Err(); err != nil {
		m.logWarn(ctx, "quota release failed — concurrency lease retained until expiry", err, res.TenantID)
	}
}

func newReservationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("quota reservation id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func concurrentErr(t string) error {
	return &modelgateway.AdapterError{Message: fmt.Sprintf("tenant %s concurrent quota exceeded", t), Class: modelgateway.ErrorQuotaExceeded, Retryable: false}
}
func qpsErr(t string) error {
	return &modelgateway.AdapterError{Message: fmt.Sprintf("tenant %s qps quota exceeded", t), Class: modelgateway.ErrorQuotaExceeded, Retryable: false}
}
func quotaErr(t, kind string) error {
	return &modelgateway.AdapterError{Message: fmt.Sprintf("tenant %s %s exceeded", t, kind), Class: modelgateway.ErrorQuotaExceeded, Retryable: false}
}
func budgetErr(t, kind string) error {
	return &modelgateway.AdapterError{Message: fmt.Sprintf("tenant %s %s exceeded", t, kind), Class: modelgateway.ErrorBudgetExceeded, Retryable: false}
}

var _ modelgateway.TenantQuotaManager = (*TenantQuotaManager)(nil)

// logWarn emits a structured warning via the optional QuotaLogger. It is used
// for degraded-mode events (fail-open, commit under-counts) that must be
// observable in production logs.
func (m *TenantQuotaManager) logWarn(ctx context.Context, msg string, err error, tenant string) {
	if m.logger != nil {
		m.logger.Warnf(ctx, "quota: tenant=%s msg=%q err=%v", tenant, msg, err)
	}
}
