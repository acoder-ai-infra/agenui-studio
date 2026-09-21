package app

import (
	"context"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestUnknownTenantRoutedToDefaultFacadeUsesDefaultQuotaPolicy(t *testing.T) {
	policies := explicitTenantQuotaPolicy("default", modelgateway.TenantQuota{MaxConcurrent: 1})
	if len(policies) != 1 || policies["default"].MaxConcurrent != 1 {
		t.Fatalf("explicit quota policy=%#v", policies)
	}
	quota := modelgateway.NewMemoryTenantQuotaManager(policies)
	request := modelgateway.ModelRequest{Trace: observability.TraceContext{TenantID: "unknown"}}
	reservation, err := quota.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("unknown tenant did not fall back to default quota: %v", err)
	}
	if reservation.TenantID != "default" {
		t.Fatalf("reservation tenant=%q, want default", reservation.TenantID)
	}
	if _, err := quota.Reserve(context.Background(), request); err == nil {
		t.Fatal("unknown tenant bypassed default quota limit")
	}
}
