package modelgateway

import (
	"context"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type cleanupContextQuota struct {
	commitErr error
}

func (*cleanupContextQuota) Reserve(context.Context, ModelRequest) (QuotaReservation, error) {
	return QuotaReservation{}, nil
}

func (q *cleanupContextQuota) Commit(ctx context.Context, _ QuotaReservation, _ ModelUsage, _ ModelCost) {
	q.commitErr = ctx.Err()
}

func (*cleanupContextQuota) Release(context.Context, QuotaReservation) {}

func TestQuotaCommitUsesBoundedCleanupContext(t *testing.T) {
	quota := &cleanupContextQuota{}
	facade := &Facade{Quota: quota}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	facade.commitQuota(ctx, QuotaReservation{TenantID: "tenant", ReservationID: "reservation"}, ModelUsage{}, ModelCost{})
	if quota.commitErr != nil {
		t.Fatalf("quota Commit inherited completed model context: %v", quota.commitErr)
	}
}

func TestMemoryQuotaUnknownTenantUsesDefaultPolicy(t *testing.T) {
	manager := NewMemoryTenantQuotaManager(map[string]TenantQuota{"default": {MaxConcurrent: 1}})
	request := ModelRequest{Trace: observability.TraceContext{TenantID: "unknown"}}
	reservation, err := manager.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("Reserve unknown tenant with default quota: %v", err)
	}
	if reservation.TenantID != "default" {
		t.Fatalf("reservation tenant=%q, want default", reservation.TenantID)
	}
	if _, err := manager.Reserve(context.Background(), request); err == nil {
		t.Fatal("second unknown tenant request bypassed default concurrent quota")
	} else if adapterErr, ok := err.(*AdapterError); !ok || adapterErr.Class != ErrorQuotaExceeded || adapterErr.Retryable {
		t.Fatalf("second unknown tenant error=%#v, want non-retryable quota_exceeded", err)
	}
}
