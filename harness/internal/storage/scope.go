package storage

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// Scope is the tenant/user execution scope derived from the request
// TraceContext. Tenant isolation is enforced from this scope, never from a
// client-supplied tenant_id (session-run-storage-design.md §13.1, SP-003).
type Scope struct {
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
}

// ScopeFrom derives the Scope from the observability.TraceContext in ctx.
// It requires a non-empty tenant_id; callers that legitimately run without a
// tenant (single-tenant dev) should use ScopeFromLenient.
func ScopeFrom(ctx context.Context) (Scope, error) {
	tc := observability.MustTraceContext(ctx)
	if tc.TenantID == "" {
		return Scope{}, errorf(ErrPermissionDenied, "missing tenant_id in trace context")
	}
	return Scope{
		TenantID:  tc.TenantID,
		UserID:    tc.UserID,
		SessionID: tc.SessionID,
		RunID:     tc.RunID,
	}, nil
}

// ScopeFromLenient derives the Scope but tolerates an empty tenant (dev/test).
// Backends still enforce tenant match when a tenant is present.
func ScopeFromLenient(ctx context.Context) Scope {
	tc := observability.MustTraceContext(ctx)
	return Scope{
		TenantID:  tc.TenantID,
		UserID:    tc.UserID,
		SessionID: tc.SessionID,
		RunID:     tc.RunID,
	}
}

// EnforceTenant returns an ErrTenantMismatch error when the scope carries a
// tenant that differs from the record's tenant. An empty scope tenant is
// tolerated (dev/test); an empty record tenant is treated as scope-owned.
func (s Scope) EnforceTenant(recordTenantID string) error {
	if s.TenantID == "" {
		return nil
	}
	if recordTenantID != "" && recordTenantID != s.TenantID {
		return errorf(ErrTenantMismatch, "tenant mismatch: scope=%s record=%s", s.TenantID, recordTenantID)
	}
	return nil
}
