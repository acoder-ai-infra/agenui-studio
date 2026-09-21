package storagewrite

import "context"

type StoreName string

const (
	StoreRun        StoreName = "run"
	StoreStep       StoreName = "step"
	StoreFallback   StoreName = "fallback"
	StoreMessage    StoreName = "message"
	StoreEvent      StoreName = "event"
	StoreArtifact   StoreName = "artifact"
	StoreHotSession StoreName = "hot_session"
	StoreHotContext StoreName = "hot_context"
	StoreHotStream  StoreName = "hot_stream"
)

type Operation string

const (
	OperationInsert Operation = "insert"
	OperationUpdate Operation = "update"
	OperationUpsert Operation = "upsert"
	OperationAppend Operation = "append"
	OperationPut    Operation = "put"
	OperationDelete Operation = "delete"
)

type CommitPolicy string

const (
	CommitPolicyRequiredFirst CommitPolicy = "required_first"
)

type Plan struct {
	TraceID        string
	TenantID       string
	SessionID      string
	RunID          string
	IdempotencyKey string
	Reason         string
	RequiredWrites []Write
	OptionalWrites []Write
	Projections    []Projection
	CommitPolicy   CommitPolicy
}

type Write struct {
	TraceID        string
	TenantID       string
	SessionID      string
	RunID          string
	IdempotencyKey string
	Reason         string
	Store          StoreName
	Operation      Operation
	Ref            string
	PayloadRef     string
	Payload        any
	Blocking       bool
}

type Projection struct {
	Store      StoreName
	Operation  Operation
	Ref        string
	PayloadRef string
	Payload    any
}

type WriteResult struct {
	Write   Write
	Skipped bool
	Err     error
	Receipt WriteReceipt
}

type ProjectionResult struct {
	Projection Projection
	Skipped    bool
	Err        error
}

type Result struct {
	Required    []WriteResult
	Optional    []WriteResult
	Projections []ProjectionResult
}

type StorePort interface {
	Write(ctx context.Context, write Write) (WriteReceipt, error)
}

type WriteReceipt struct {
	Store StoreName
	Ref   string
	// Sequence is the run-monotonic sequence the store allocated for an appended
	// event (0 when not applicable). It is propagated back so the realtime channel
	// can carry the same sequence the persisted/replayed copy has (P1-8).
	Sequence int64
}
