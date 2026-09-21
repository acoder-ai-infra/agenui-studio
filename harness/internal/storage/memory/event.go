package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// eventStore is the in-memory EventStore: the owner of run-monotonic sequence
// allocation and idempotency (canonical §12.2, conformance E-001..E-004).
type eventStore struct {
	mu      sync.Mutex
	ids     observability.IDGenerator
	byRun   map[string][]observability.AgentEvent
	seq     map[string]int64  // run_id -> last sequence
	byEvent map[string]string // event_id -> run_id (dedup)
	byIdem  map[string]string // tenant|idempotency_key -> event_id (dedup)
}

func newEventStore() *eventStore {
	return &eventStore{
		ids:     observability.NewULIDGenerator(""),
		byRun:   make(map[string][]observability.AgentEvent),
		seq:     make(map[string]int64),
		byEvent: make(map[string]string),
		byIdem:  make(map[string]string),
	}
}

func (e *eventStore) Append(ctx context.Context, ev observability.AgentEvent) (storage.AppendResult, error) {
	if err := validateEvent(ev); err != nil {
		return storage.AppendResult{}, err
	}
	if err := storage.ValidateTenantID(storage.ScopeFromLenient(ctx).TenantID); err != nil {
		return storage.AppendResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.appendLocked(ctx, ev)
}

func validateEvent(ev observability.AgentEvent) error {
	if ev.RunID == "" {
		return storageInvalid("run_id required for event append")
	}
	if err := storage.ValidateEventIdentifiers(ev); err != nil {
		return err
	}
	// E-002: reject unregistered event types and non-canonical schema versions.
	if !observability.IsRegisteredEventType(ev.EventType) {
		return storageInvalid("unregistered event_type: " + string(ev.EventType))
	}
	if ev.SchemaVersion != "" && ev.SchemaVersion != observability.AgentEventSchemaVersion {
		return storageInvalid("illegal schema_version: " + ev.SchemaVersion)
	}
	return nil
}

// appendLocked is shared by ResumeStore so event persistence and claim changes
// live under one critical section. The caller must hold e.mu.
func (e *eventStore) appendLocked(ctx context.Context, ev observability.AgentEvent) (storage.AppendResult, error) {
	return e.appendLockedForTenant(ev, storage.ScopeFromLenient(ctx).TenantID)
}

func (e *eventStore) appendLockedForTenant(ev observability.AgentEvent, tenantID string) (storage.AppendResult, error) {
	// Idempotency by event_id.
	if ev.EventID != "" {
		if runID, ok := e.byEvent[ev.EventID]; ok {
			return e.replay(runID, ev.EventID)
		}
	}
	// Idempotency by idempotency_key (namespaced by tenant to avoid cross-tenant collision).
	idemLookup := ""
	if ev.IdempotencyKey != "" {
		idemLookup = tenantID + "|" + ev.IdempotencyKey
		if eventID, ok := e.byIdem[idemLookup]; ok {
			return e.replay(e.byEvent[eventID], eventID)
		}
	}

	// Normalize.
	if ev.EventID == "" {
		ev.EventID = e.ids.NewEventID()
	}
	if ev.SchemaVersion == "" {
		ev.SchemaVersion = observability.AgentEventSchemaVersion
	}
	if ev.Visibility == "" {
		ev.Visibility = observability.VisibilityDebug
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	// Allocate run-monotonic sequence.
	e.seq[ev.RunID]++
	ev.Sequence = e.seq[ev.RunID]

	e.byRun[ev.RunID] = append(e.byRun[ev.RunID], ev)
	e.byEvent[ev.EventID] = ev.RunID
	if idemLookup != "" {
		e.byIdem[idemLookup] = ev.EventID
	}
	return storage.AppendResult{Event: ev, Idempotent: false}, nil
}

func (e *eventStore) replay(runID, eventID string) (storage.AppendResult, error) {
	for _, ev := range e.byRun[runID] {
		if ev.EventID == eventID {
			return storage.AppendResult{Event: ev, Idempotent: true}, nil
		}
	}
	return storage.AppendResult{}, storageNotFound("event not found on replay: " + eventID)
}

func (e *eventStore) Query(ctx context.Context, q storage.EventQuery) ([]observability.AgentEvent, error) {
	if q.RunID == "" {
		return nil, storageInvalid("run_id required for event query")
	}
	visSet := map[observability.EventVisibility]bool{}
	for _, v := range q.Visibilities {
		visSet[v] = true
	}
	typeSet := map[observability.EventType]bool{}
	for _, t := range q.EventTypes {
		typeSet[t] = true
	}

	e.mu.Lock()
	src := append([]observability.AgentEvent(nil), e.byRun[q.RunID]...)
	e.mu.Unlock()

	sort.Slice(src, func(i, j int) bool { return src[i].Sequence < src[j].Sequence })

	var out []observability.AgentEvent
	for _, ev := range src {
		if ev.Sequence <= q.AfterSequence {
			continue
		}
		if len(visSet) > 0 && !visSet[ev.Visibility] {
			continue
		}
		if len(typeSet) > 0 && !typeSet[ev.EventType] {
			continue
		}
		out = append(out, ev)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

func (e *eventStore) LastSequence(_ context.Context, runID string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq[runID], nil
}

func (e *eventStore) SequenceOf(_ context.Context, runID, eventID string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.byRun[runID] {
		if ev.EventID == eventID {
			return ev.Sequence, nil
		}
	}
	return 0, storageNotFound("event not found: " + eventID)
}
