package redisstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type quotaCommandFailureHook struct {
	commands    map[string]error
	keyContains string
}

func (h quotaCommandFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h quotaCommandFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err, ok := h.commands[strings.ToLower(cmd.Name())]; ok {
			if h.keyContains == "" || commandContains(cmd, h.keyContains) {
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func (h quotaCommandFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func commandContains(cmd redis.Cmder, fragment string) bool {
	for _, arg := range cmd.Args() {
		if strings.Contains(fmt.Sprint(arg), fragment) {
			return true
		}
	}
	return false
}

type quotaLogRecorder struct {
	mu      sync.Mutex
	entries []string
}

func (l *quotaLogRecorder) Warnf(_ context.Context, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fmt.Sprintf(format, args...))
}

func (l *quotaLogRecorder) contains(fragment string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if strings.Contains(entry, fragment) {
			return true
		}
	}
	return false
}

func newQuotaManagerWithHook(t *testing.T, cfg Config, quotas map[string]modelgateway.TenantQuota, logger QuotaLogger, hook redis.Hook) (*TenantQuotaManager, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	if hook != nil {
		client.AddHook(hook)
	}
	t.Cleanup(func() { _ = client.Close() })
	return NewTenantQuotaManager(client, cfg, quotas, logger), client
}

func newTestManager(t *testing.T, quotas map[string]modelgateway.TenantQuota) (*TenantQuotaManager, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewTenantQuotaManager(client, Config{}, quotas, nil), client
}

func reqFor(tenant string) modelgateway.ModelRequest {
	return modelgateway.ModelRequest{Trace: observability.TraceContext{TenantID: tenant}}
}

// inflight reads the number of active concurrency leases.
func inflight(t *testing.T, client Client, m *TenantQuotaManager, tenant string) int {
	t.Helper()
	n, err := client.ZCard(context.Background(), m.inflightKey(tenant)).Result()
	if err != nil {
		t.Fatalf("read inflight: %v", err)
	}
	return int(n)
}

// TestReserveReleaseInflightZero: a single Reserve then Release must leave the
// inflight counter at exactly 0, not -1.
func TestReserveReleaseInflightZero(t *testing.T) {
	ctx := context.Background()
	m, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: 5},
	})

	res, err := m.Reserve(ctx, reqFor("t1"))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !res.Counted {
		t.Fatal("successful Reserve must set Counted=true")
	}
	if got := inflight(t, client, m, "t1"); got != 1 {
		t.Fatalf("inflight after Reserve = %d, want 1", got)
	}
	if ttl := client.TTL(ctx, m.inflightKey("t1")).Val(); ttl <= 0 {
		t.Fatalf("concurrency lease key must expire, ttl=%v", ttl)
	}

	m.Release(ctx, res)
	if got := inflight(t, client, m, "t1"); got != 0 {
		t.Fatalf("inflight after Release = %d, want 0", got)
	}
}

// TestConcurrencyLimit: with MaxConcurrent=N, N Reserves succeed and the N+1th
// is rejected; after Releasing one, a new Reserve succeeds. The inflight counter
// stays exact and never goes negative.
func TestConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	const N = 3
	m, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: N},
	})

	var held []modelgateway.QuotaReservation
	for i := 0; i < N; i++ {
		res, err := m.Reserve(ctx, reqFor("t1"))
		if err != nil {
			t.Fatalf("Reserve %d: unexpected err %v", i, err)
		}
		held = append(held, res)
	}
	if got := inflight(t, client, m, "t1"); got != N {
		t.Fatalf("inflight = %d, want %d", got, N)
	}

	// N+1th must be rejected and must NOT have incremented inflight.
	rejected, err := m.Reserve(ctx, reqFor("t1"))
	if err == nil {
		t.Fatal("Reserve past MaxConcurrent should fail")
	}
	if rejected.Counted {
		t.Fatal("rejected Reserve must not be Counted")
	}
	if got := inflight(t, client, m, "t1"); got != N {
		t.Fatalf("inflight after rejection = %d, want %d", got, N)
	}

	// Release one slot, then a fresh Reserve should succeed.
	m.Release(ctx, held[0])
	if got := inflight(t, client, m, "t1"); got != N-1 {
		t.Fatalf("inflight after Release = %d, want %d", got, N-1)
	}
	res, err := m.Reserve(ctx, reqFor("t1"))
	if err != nil {
		t.Fatalf("Reserve after Release: %v", err)
	}
	held = append(held[1:], res)
	if got := inflight(t, client, m, "t1"); got != N {
		t.Fatalf("inflight after re-Reserve = %d, want %d", got, N)
	}

	// Release everything; counter must land exactly on 0.
	for _, r := range held {
		m.Release(ctx, r)
	}
	if got := inflight(t, client, m, "t1"); got != 0 {
		t.Fatalf("final inflight = %d, want 0", got)
	}
}

// TestReleaseRejectedIsNoOp: Releasing a reservation that was rejected for quota
// exceeded must not push inflight below the correct value (BUG B).
func TestReleaseRejectedIsNoOp(t *testing.T) {
	ctx := context.Background()
	m, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: 1},
	})

	ok, err := m.Reserve(ctx, reqFor("t1"))
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	rejected, err := m.Reserve(ctx, reqFor("t1"))
	if err == nil {
		t.Fatal("second Reserve should be rejected")
	}

	// Deferred Release of the rejected reservation must be a no-op.
	m.Release(ctx, rejected)
	if got := inflight(t, client, m, "t1"); got != 1 {
		t.Fatalf("inflight after releasing rejected reservation = %d, want 1", got)
	}

	// The agenuinely held reservation still releases cleanly to 0.
	m.Release(ctx, ok)
	if got := inflight(t, client, m, "t1"); got != 0 {
		t.Fatalf("inflight after real Release = %d, want 0", got)
	}
}

// TestEmptyQuotaReleaseIsNoOp: an explicitly configured tenant with a zero
// quota short-circuits (no accounting), and Release must not touch inflight.
func TestEmptyQuotaReleaseIsNoOp(t *testing.T) {
	ctx := context.Background()
	m, client := newTestManager(t, map[string]modelgateway.TenantQuota{"free": {}})

	res, err := m.Reserve(ctx, reqFor("free"))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Counted {
		t.Fatal("empty-quota reservation must not be Counted")
	}
	m.Release(ctx, res)
	if got := inflight(t, client, m, "free"); got != 0 {
		t.Fatalf("inflight for empty-quota tenant = %d, want 0", got)
	}
}

// TestKeyHashTag: every quota key must wrap the tenant in a "{tenant}" hash tag
// so that on a Redis Cluster all of a tenant's keys hash to one slot (BUG A).
func TestKeyHashTag(t *testing.T) {
	ctx := context.Background()
	m, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: 5, QPS: 5},
	})

	for name, key := range map[string]string{
		"inflight":      m.inflightKey("t1"),
		"qps":           m.qpsKey("t1", 100),
		"tokens":        m.tokensKey("t1"),
		"cost":          m.costKey("t1"),
		"tokens_minute": m.tokensMinuteKey("t1", 123),
		"tokens_day":    m.tokensDayKey("t1", "20260717"),
		"cost_day":      m.costDayKey("t1", "20260717"),
		"commit":        m.commitKey("t1", "reservation"),
	} {
		if !strings.Contains(key, "{t1}") {
			t.Fatalf("%s key %q missing {t1} hash tag", name, key)
		}
	}

	// A real Reserve must actually write keys carrying the hash tag.
	if _, err := m.Reserve(ctx, reqFor("t1")); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	keys, err := client.Keys(ctx, "*").Result()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("expected Reserve to write at least one key")
	}
	for _, k := range keys {
		if !strings.Contains(k, "{t1}") {
			t.Fatalf("written key %q missing {t1} hash tag", k)
		}
	}
}

// TestConcurrentReserveRace hammers Reserve from many goroutines to shake out
// races (run with -race). With MaxConcurrent=limit and no releases during the
// burst, exactly `limit` Reserves succeed and inflight equals `limit` exactly —
// never over, never negative. Releasing all winners returns it to 0.
func TestConcurrentReserveRace(t *testing.T) {
	ctx := context.Background()
	const limit = 10
	const goroutines = 60
	m, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: limit},
	})

	var (
		mu      sync.Mutex
		winners []modelgateway.QuotaReservation
		wg      sync.WaitGroup
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			res, err := m.Reserve(ctx, reqFor("t1"))
			if err != nil {
				return
			}
			mu.Lock()
			winners = append(winners, res)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(winners) != limit {
		t.Fatalf("successful Reserves = %d, want exactly %d", len(winners), limit)
	}
	if got := inflight(t, client, m, "t1"); got != limit {
		t.Fatalf("inflight after burst = %d, want %d", got, limit)
	}

	var rwg sync.WaitGroup
	rwg.Add(len(winners))
	for _, r := range winners {
		go func(r modelgateway.QuotaReservation) {
			defer rwg.Done()
			m.Release(ctx, r)
		}(r)
	}
	rwg.Wait()

	if got := inflight(t, client, m, "t1"); got != 0 {
		t.Fatalf("inflight after releasing all = %d, want 0", got)
	}
}

func TestConcurrencyLeaseCoversModelRequestTimeout(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	manager := NewTenantQuotaManager(client, Config{QuotaLeaseTTL: 2 * time.Minute}, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: 1},
	}, nil)
	now := time.Unix(1_700_000_000, 0)
	mr.SetTime(now)
	manager.now = func() time.Time { return now }
	req := reqFor("t1")
	req.TimeoutMS = int((5 * time.Minute) / time.Millisecond)

	first, err := manager.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	score, err := client.ZScore(context.Background(), manager.inflightKey("t1"), first.ReservationID).Result()
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry := now.Add(5*time.Minute + quotaLeaseCleanupMargin).UnixMilli()
	if got := int64(score); got != wantExpiry {
		t.Fatalf("lease expiry=%d, want %d", got, wantExpiry)
	}

	now = now.Add(121 * time.Second)
	mr.FastForward(121 * time.Second)
	if _, err := manager.Reserve(context.Background(), req); err == nil {
		t.Fatal("lease expired at the configured 120s before the model timeout")
	}

	now = now.Add(185 * time.Second)
	mr.FastForward(185 * time.Second)
	if _, err := manager.Reserve(context.Background(), req); err != nil {
		t.Fatalf("Reserve after request-aligned lease expiry: %v", err)
	}
}

func TestShortRequestDoesNotShortenExistingLongLease(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	manager := NewTenantQuotaManager(client, Config{QuotaLeaseTTL: 2 * time.Minute}, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: 2},
	}, nil)
	now := time.Unix(1_700_000_000, 0)
	mr.SetTime(now)
	manager.now = func() time.Time { return now }

	longReq := reqFor("t1")
	longReq.TimeoutMS = int((10 * time.Minute) / time.Millisecond)
	longReservation, err := manager.Reserve(context.Background(), longReq)
	if err != nil {
		t.Fatalf("reserve long request: %v", err)
	}
	shortReq := reqFor("t1")
	shortReq.TimeoutMS = int((2 * time.Minute) / time.Millisecond)
	if _, err := manager.Reserve(context.Background(), shortReq); err != nil {
		t.Fatalf("reserve short request: %v", err)
	}

	wantExpiry := now.Add(10*time.Minute + quotaLeaseCleanupMargin)
	if got := client.PTTL(context.Background(), manager.inflightKey("t1")).Val(); got < 10*time.Minute || got > 10*time.Minute+quotaLeaseCleanupMargin {
		t.Fatalf("short request changed lease key TTL to %v, want expiry at %v", got, wantExpiry)
	}

	now = now.Add(2*time.Minute + quotaLeaseCleanupMargin + time.Second)
	mr.FastForward(2*time.Minute + quotaLeaseCleanupMargin + time.Second)
	third, err := manager.Reserve(context.Background(), shortReq)
	if err != nil {
		t.Fatalf("reserve after short lease expiry: %v", err)
	}
	if _, err := client.ZScore(context.Background(), manager.inflightKey("t1"), longReservation.ReservationID).Result(); err != nil {
		t.Fatalf("long lease disappeared after short lease expiry: %v", err)
	}
	manager.Release(context.Background(), third)
}

func TestEffectiveLeaseTTLCoversRuntimeDeadline(t *testing.T) {
	now := time.Now()
	manager := &TenantQuotaManager{leaseTTL: 2 * time.Minute}
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(5*time.Minute))
	defer cancel()
	if got, want := manager.effectiveLeaseTTL(ctx, modelgateway.ModelRequest{}, now), 5*time.Minute+quotaLeaseCleanupMargin; got != want {
		t.Fatalf("effective lease TTL=%v, want %v", got, want)
	}
}

func TestQuotaCommitIsAtomicAndIdempotent(t *testing.T) {
	manager, client := newTestManager(t, map[string]modelgateway.TenantQuota{"t1": {MaxConcurrent: 1}})
	now := time.Date(2026, 7, 17, 16, 45, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	reservation, err := manager.Reserve(context.Background(), reqFor("t1"))
	if err != nil {
		t.Fatal(err)
	}
	usage := modelgateway.ModelUsage{PromptTokens: 10, CompletionTokens: 4, ReasoningTokens: 2}
	cost := modelgateway.ModelCost{Estimated: 0.75}

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			manager.Commit(context.Background(), reservation, usage, cost)
		}()
	}
	wg.Wait()

	if got := client.Get(context.Background(), manager.tokensKey("t1")).Val(); got != "16" {
		t.Fatalf("tokens after duplicate Commit=%q, want 16", got)
	}
	if got, err := client.Get(context.Background(), manager.costKey("t1")).Float64(); err != nil || got != 0.75 {
		t.Fatalf("cost after duplicate Commit=(%v, %v), want 0.75", got, err)
	}
	if got := client.Get(context.Background(), manager.tokensMinuteKey("t1", now.Unix()/60)).Val(); got != "16" {
		t.Fatalf("minute tokens after duplicate Commit=%q, want 16", got)
	}
	if ttl := client.TTL(context.Background(), manager.tokensMinuteKey("t1", now.Unix()/60)).Val(); ttl <= 0 || ttl > minuteQuotaTTL {
		t.Fatalf("minute token TTL=%v, want (0,%v]", ttl, minuteQuotaTTL)
	}
	if got := client.Get(context.Background(), manager.tokensDayKey("t1", "20260717")).Val(); got != "16" {
		t.Fatalf("daily tokens after duplicate Commit=%q, want 16", got)
	}
	if got, err := client.Get(context.Background(), manager.costDayKey("t1", "20260717")).Float64(); err != nil || got != 0.75 {
		t.Fatalf("daily cost after duplicate Commit=(%v, %v), want 0.75", got, err)
	}
	if ttl := client.TTL(context.Background(), manager.commitKey("t1", reservation.ReservationID)).Val(); ttl <= 0 {
		t.Fatalf("idempotency marker must expire, ttl=%v", ttl)
	}
}

func TestWindowedTokenQuotaResetsByMinuteAndDay(t *testing.T) {
	ctx := context.Background()
	manager, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: 1, TokensPerMinute: 10, DailyTokenBudget: 20, DailyCostBudget: 10},
	})
	now := time.Date(2026, 7, 17, 16, 45, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }

	first, err := manager.Reserve(ctx, reqFor("t1"))
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	manager.Commit(ctx, first, modelgateway.ModelUsage{PromptTokens: 10}, modelgateway.ModelCost{Estimated: 1})
	manager.Release(ctx, first)
	if _, err := manager.Reserve(ctx, reqFor("t1")); err == nil {
		t.Fatal("tokens_per_minute should reject the same minute after 10 tokens")
	}

	now = now.Add(time.Minute)
	second, err := manager.Reserve(ctx, reqFor("t1"))
	if err != nil {
		t.Fatalf("Reserve after minute rollover: %v", err)
	}
	manager.Commit(ctx, second, modelgateway.ModelUsage{PromptTokens: 10}, modelgateway.ModelCost{Estimated: 1})
	manager.Release(ctx, second)

	now = now.Add(time.Minute)
	if _, err := manager.Reserve(ctx, reqFor("t1")); err == nil {
		t.Fatal("daily_token_budget should reject after 20 tokens in one day")
	}
	if got := client.Get(ctx, manager.tokensDayKey("t1", "20260717")).Val(); got != "20" {
		t.Fatalf("daily token key=%q, want 20", got)
	}

	now = time.Date(2026, 7, 18, 0, 0, 1, 0, time.UTC)
	if _, err := manager.Reserve(ctx, reqFor("t1")); err != nil {
		t.Fatalf("Reserve after day rollover: %v", err)
	}
}

func TestQuotaCommitMarkerWindowIsConfigurableAndCapacityBounded(t *testing.T) {
	const configured = 45 * time.Second
	manager, client := newQuotaManagerWithHook(t, Config{QuotaCommitTTL: configured}, map[string]modelgateway.TenantQuota{
		"t1": {MaxConcurrent: 1},
	}, nil, nil)
	reservation, err := manager.Reserve(context.Background(), reqFor("t1"))
	if err != nil {
		t.Fatal(err)
	}
	manager.Commit(context.Background(), reservation, modelgateway.ModelUsage{PromptTokens: 1}, modelgateway.ModelCost{})
	if got := client.TTL(context.Background(), manager.commitKey("t1", reservation.ReservationID)).Val(); got != configured {
		t.Fatalf("commit marker TTL=%v, want %v", got, configured)
	}

	const steadyQPS = 4
	if markers := steadyQPS * int(defaultQuotaCommitTTL/time.Second); markers > 1200 {
		t.Fatalf("default commit marker window retains %d keys at QPS=%d, want <=1200", markers, steadyQPS)
	}
}

func TestQuotaCommitRejectsPartialUpdate(t *testing.T) {
	logs := &quotaLogRecorder{}
	manager, client := newQuotaManagerWithHook(t, Config{}, map[string]modelgateway.TenantQuota{"t1": {MaxConcurrent: 1}}, logs, nil)
	now := time.Date(2026, 7, 17, 16, 45, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	reservation, err := manager.Reserve(context.Background(), reqFor("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), manager.tokensKey("t1"), "7", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(context.Background(), manager.costDayKey("t1", "20260717"), "invalid", 0).Err(); err != nil {
		t.Fatal(err)
	}

	manager.Commit(context.Background(), reservation, modelgateway.ModelUsage{PromptTokens: 10}, modelgateway.ModelCost{Estimated: 0.5})
	if got := client.Get(context.Background(), manager.tokensKey("t1")).Val(); got != "7" {
		t.Fatalf("token counter changed despite cost validation failure: %q", got)
	}
	if !logs.contains("atomic commit failed after retries") {
		t.Fatalf("missing atomic commit warning: %#v", logs.entries)
	}
}

func TestQuotaCommitRejectsNonFiniteCostBeforeWritingCounters(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprintf("cost_%v", value), func(t *testing.T) {
			logs := &quotaLogRecorder{}
			manager, client := newQuotaManagerWithHook(t, Config{}, map[string]modelgateway.TenantQuota{"t1": {MaxConcurrent: 1}}, logs, nil)
			reservation, err := manager.Reserve(context.Background(), reqFor("t1"))
			if err != nil {
				t.Fatal(err)
			}

			manager.Commit(context.Background(), reservation, modelgateway.ModelUsage{PromptTokens: 10}, modelgateway.ModelCost{Estimated: value})
			if client.Exists(context.Background(), manager.tokensKey("t1"), manager.costKey("t1"), manager.commitKey("t1", reservation.ReservationID)).Val() != 0 {
				t.Fatal("non-finite cost wrote quota counters or idempotency marker")
			}
			if !logs.contains("non-finite") {
				t.Fatalf("missing non-finite quota warning: %#v", logs.entries)
			}
		})
	}
}

func TestQuotaCommitScriptRejectsNonFiniteBeforeWritingCounters(t *testing.T) {
	for _, raw := range []string{"nan", "inf", "-inf"} {
		t.Run(raw, func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
			t.Cleanup(func() { _ = client.Close() })
			keys := []string{"quota:{t1}:tokens", "quota:{t1}:cost", "quota:{t1}:tokens:minute:1", "quota:{t1}:tokens:day:20260717", "quota:{t1}:cost:day:20260717", "quota:{t1}:commit:r1"}

			if _, err := commitScript.Run(context.Background(), client, keys, 10, raw, "10|"+raw, 60, 120, 172800).Int(); err == nil {
				t.Fatalf("commit script accepted non-finite cost %q", raw)
			}
			if got := client.Exists(context.Background(), keys...).Val(); got != 0 {
				t.Fatalf("commit script wrote %d keys before rejecting %q", got, raw)
			}
		})
	}
}

func TestUnknownTenantUsesDefaultQuotaPolicy(t *testing.T) {
	manager, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"default": {MaxConcurrent: 1},
	})
	reservation, err := manager.Reserve(context.Background(), reqFor("unknown"))
	if err != nil {
		t.Fatalf("Reserve unknown tenant with default quota: %v", err)
	}
	if reservation.TenantID != "default" || !reservation.Counted {
		t.Fatalf("reservation=%#v, want counted default reservation", reservation)
	}
	if _, err := manager.Reserve(context.Background(), reqFor("unknown")); err == nil {
		t.Fatal("second unknown tenant request bypassed default concurrent quota")
	} else if adapterErr, ok := err.(*modelgateway.AdapterError); !ok || adapterErr.Class != modelgateway.ErrorQuotaExceeded || adapterErr.Retryable {
		t.Fatalf("second unknown tenant error=%#v, want non-retryable quota_exceeded", err)
	}
	if got := inflight(t, client, manager, "unknown"); got != 0 {
		t.Fatalf("unknown tenant inflight=%d, want 0", got)
	}
	if got := inflight(t, client, manager, "default"); got != 1 {
		t.Fatalf("default tenant inflight=%d, want 1", got)
	}
}

func TestUnknownTenantFailsClosedWithoutDefaultQuotaPolicy(t *testing.T) {
	manager, client := newTestManager(t, map[string]modelgateway.TenantQuota{
		"public": {MaxConcurrent: 1},
	})
	if _, err := manager.Reserve(context.Background(), reqFor("unknown")); err == nil {
		t.Fatal("unknown tenant without default quota policy was allowed")
	} else if adapterErr, ok := err.(*modelgateway.AdapterError); !ok || adapterErr.Class != modelgateway.ErrorQuotaExceeded || adapterErr.Retryable {
		t.Fatalf("unknown tenant error=%#v, want non-retryable quota_exceeded", err)
	}
	if got := inflight(t, client, manager, "unknown"); got != 0 {
		t.Fatalf("unknown tenant inflight=%d, want 0", got)
	}
}

func TestQuotaBudgetReadFailureHonorsPolicy(t *testing.T) {
	forced := errors.New("forced budget read failure")
	for _, budget := range []struct {
		name        string
		quota       modelgateway.TenantQuota
		keyFragment string
	}{
		{name: "tpm", quota: modelgateway.TenantQuota{TokensPerMinute: 100, MaxConcurrent: 1}, keyFragment: ":tokens:minute:"},
		{name: "daily_token", quota: modelgateway.TenantQuota{DailyTokenBudget: 100, MaxConcurrent: 1}, keyFragment: ":tokens:day:"},
		{name: "daily_cost", quota: modelgateway.TenantQuota{DailyCostBudget: 1, MaxConcurrent: 1}, keyFragment: ":cost:day:"},
	} {
		for _, failOpen := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail_open_%t", budget.name, failOpen), func(t *testing.T) {
				logs := &quotaLogRecorder{}
				manager, _ := newQuotaManagerWithHook(t, Config{QuotaFailOpen: failOpen}, map[string]modelgateway.TenantQuota{"t1": budget.quota}, logs,
					quotaCommandFailureHook{commands: map[string]error{"get": forced}, keyContains: budget.keyFragment})

				reservation, err := manager.Reserve(context.Background(), reqFor("t1"))
				if failOpen {
					if err != nil || !reservation.Counted {
						t.Fatalf("fail-open Reserve = (%#v, %v), want counted success", reservation, err)
					}
					if !logs.contains("check failed (fail-open)") {
						t.Fatalf("missing fail-open warning: %#v", logs.entries)
					}
					return
				}
				if err == nil || reservation.Counted {
					t.Fatalf("fail-closed Reserve = (%#v, %v), want rejection", reservation, err)
				}
			})
		}
	}
}

func TestQuotaReserveFailureHonorsPolicy(t *testing.T) {
	forced := errors.New("forced reserve failure")
	for _, failOpen := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail_open_%t", failOpen), func(t *testing.T) {
			logs := &quotaLogRecorder{}
			manager, _ := newQuotaManagerWithHook(t, Config{QuotaFailOpen: failOpen}, map[string]modelgateway.TenantQuota{"t1": {MaxConcurrent: 1}}, logs,
				quotaCommandFailureHook{commands: map[string]error{"eval": forced}})

			reservation, err := manager.Reserve(context.Background(), reqFor("t1"))
			if failOpen {
				if err != nil || reservation.Counted {
					t.Fatalf("fail-open Reserve = (%#v, %v), want uncounted success", reservation, err)
				}
				if !logs.contains("request allowed") {
					t.Fatalf("missing degraded-mode warning: %#v", logs.entries)
				}
				return
			}
			if err == nil || reservation.Counted {
				t.Fatalf("fail-closed Reserve = (%#v, %v), want rejection", reservation, err)
			}
		})
	}
}

func TestQuotaCommitAndReleaseFailuresAreObservable(t *testing.T) {
	forced := errors.New("forced quota write failure")
	logs := &quotaLogRecorder{}
	manager, client := newQuotaManagerWithHook(t, Config{}, map[string]modelgateway.TenantQuota{"t1": {MaxConcurrent: 1}}, logs, nil)

	reservation, err := manager.Reserve(context.Background(), reqFor("t1"))
	if err != nil || !reservation.Counted {
		t.Fatalf("Reserve = (%#v, %v), want counted success", reservation, err)
	}
	client.AddHook(quotaCommandFailureHook{commands: map[string]error{"evalsha": forced, "eval": forced, "zrem": forced}})
	manager.Commit(context.Background(), reservation, modelgateway.ModelUsage{PromptTokens: 10}, modelgateway.ModelCost{Estimated: 0.5})
	manager.Release(context.Background(), reservation)

	for _, fragment := range []string{"atomic commit failed after retries", "release failed"} {
		if !logs.contains(fragment) {
			t.Fatalf("missing %q warning: %#v", fragment, logs.entries)
		}
	}
}
