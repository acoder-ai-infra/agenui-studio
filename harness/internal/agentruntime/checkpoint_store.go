package agentruntime

import (
	"context"
	"errors"
	"time"
)

const RuntimeCheckpointSchemaVersion = "harness.runtime_checkpoint.v1"

var (
	ErrCheckpointStoreMissing    = errors.New("runtime checkpoint store missing")
	ErrCheckpointInvalid         = errors.New("runtime checkpoint invalid")
	ErrCheckpointHashMismatch    = errors.New("runtime checkpoint hash mismatch")
	ErrCheckpointPayloadTooLarge = errors.New("runtime checkpoint payload too large")
)

// RuntimeCheckpointScope is the tenant and execution boundary for checkpoint
// storage. A backend must include the complete scope in every lookup key.
type RuntimeCheckpointScope struct {
	TenantID       string      `json:"tenant_id,omitempty"`
	SessionID      string      `json:"session_id"`
	RunID          string      `json:"run_id"`
	Runtime        RuntimeType `json:"runtime"`
	RuntimeVersion string      `json:"runtime_version,omitempty"`
	AdapterVersion string      `json:"adapter_version,omitempty"`
}

type RuntimeCheckpointKey struct {
	Scope        RuntimeCheckpointScope `json:"scope"`
	CheckpointID string                 `json:"checkpoint_id"`
}

// RuntimeCheckpointRecord is runtime-neutral. Payload contains the bytes needed
// by an execution adapter; durable backends may persist them in object storage
// and resolve the bytes when Load is called.
type RuntimeCheckpointRecord struct {
	SchemaVersion string                 `json:"schema_version"`
	Scope         RuntimeCheckpointScope `json:"scope"`
	CheckpointID  string                 `json:"checkpoint_id"`
	Payload       []byte                 `json:"-"`
	PayloadHash   string                 `json:"payload_hash"`
	SizeBytes     int64                  `json:"size_bytes"`
	BackendRef    string                 `json:"backend_ref,omitempty"`
	Revision      int64                  `json:"revision"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
	ExpiresAt     time.Time              `json:"expires_at,omitempty"`
}

type RuntimeCheckpointStore interface {
	Load(ctx context.Context, key RuntimeCheckpointKey) (RuntimeCheckpointRecord, bool, error)
	Save(ctx context.Context, record RuntimeCheckpointRecord) (RuntimeCheckpointRecord, error)
	Delete(ctx context.Context, key RuntimeCheckpointKey) error
}

type RuntimeCheckpointStoreResolver interface {
	Resolve(ctx context.Context, scope RuntimeCheckpointScope) (RuntimeCheckpointStore, error)
}

type RuntimeCheckpointStoreResolverFunc func(context.Context, RuntimeCheckpointScope) (RuntimeCheckpointStore, error)

func (f RuntimeCheckpointStoreResolverFunc) Resolve(ctx context.Context, scope RuntimeCheckpointScope) (RuntimeCheckpointStore, error) {
	return f(ctx, scope)
}

// StaticRuntimeCheckpointStore is intended for local/test use or for stores
// that already enforce tenant routing internally.
type StaticRuntimeCheckpointStore struct {
	Store RuntimeCheckpointStore
}

func (s StaticRuntimeCheckpointStore) Resolve(context.Context, RuntimeCheckpointScope) (RuntimeCheckpointStore, error) {
	if s.Store == nil {
		return nil, ErrCheckpointStoreMissing
	}
	return s.Store, nil
}
