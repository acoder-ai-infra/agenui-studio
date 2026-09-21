package storage

import "context"

// Capability is a declared property of a storage backend. Each backend declares
// its capabilities at startup; wiring fails fast when a required capability is
// missing (see docs/simulation/harness-storage-portability.md §1-2, X-001).
type Capability string

const (
	CapCAS             Capability = "cas"
	CapTransaction     Capability = "transaction"
	CapOrderedAppend   Capability = "ordered_append" // required by EventStore
	CapIdempotency     Capability = "idempotency"
	CapTTL             Capability = "ttl"
	CapPrefixScan      Capability = "prefix_scan"
	CapPagination      Capability = "pagination"
	CapSecondaryIndex  Capability = "secondary_index"
	CapTenantIsolation Capability = "tenant_isolation" // required by all business stores
)

// Backend is implemented by every storage backend (memory, mysql, ...). It
// declares the capabilities it supports and exposes a health check.
type Backend interface {
	Name() string
	Capabilities() map[Capability]bool
	Health(ctx context.Context) error
}

// RequireCapabilities verifies that a backend declares all required
// capabilities, returning an ErrUnsupportedCapability error otherwise. Call this
// during wiring to fail fast instead of degrading silently.
func RequireCapabilities(b Backend, required ...Capability) error {
	if b == nil {
		return errorf(ErrInvalidArgument, "backend is nil")
	}
	caps := b.Capabilities()
	for _, c := range required {
		if !caps[c] {
			return errorf(ErrUnsupportedCapability, "backend %q missing required capability %q", b.Name(), c)
		}
	}
	return nil
}

// RequiredCapabilities returns the minimum capability set each store port needs,
// per docs/session-run-storage-landing-design.md §4.1.
var RequiredCapabilities = map[string][]Capability{
	"SessionStore":        {CapCAS, CapPagination, CapTenantIsolation},
	"RunStore":            {CapCAS, CapTransaction, CapSecondaryIndex, CapTenantIsolation},
	"EventStore":          {CapOrderedAppend, CapIdempotency, CapPagination, CapTenantIsolation},
	"CheckpointStore":     {CapIdempotency, CapTenantIsolation},
	"ControlRequestStore": {CapCAS, CapTTL, CapIdempotency, CapTenantIsolation},
	"ResumeStore":         {CapCAS, CapTransaction, CapOrderedAppend, CapTenantIsolation},
	"IdempotencyStore":    {CapIdempotency, CapTTL},
	"MessageStore":        {CapPagination, CapTenantIsolation},
	"ModelUsageStore":     {CapPagination, CapSecondaryIndex, CapTenantIsolation},
}
