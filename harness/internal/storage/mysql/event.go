package mysql

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// eventStore is the MySQL EventStore: run-monotonic sequence + idempotency under
// real transactions. Sequence is allocated by locking the owning run row
// (SELECT ... FOR UPDATE) to serialize appends per run, then MAX(sequence)+1;
// the UNIQUE (run_id, sequence) constraint is the final backstop.
type eventStore struct {
	db     *sql.DB
	ids    observability.IDGenerator
	schema physicalSchema
}

func (e *eventStore) idgen() observability.IDGenerator {
	if e.ids == nil {
		e.ids = observability.NewULIDGenerator("")
	}
	return e.ids
}

func (e *eventStore) Append(ctx context.Context, ev observability.AgentEvent) (storage.AppendResult, error) {
	if err := validateMySQLEvent(ev); err != nil {
		return storage.AppendResult{}, err
	}
	if err := storage.ValidateTenantID(storage.ScopeFromLenient(ctx).TenantID); err != nil {
		return storage.AppendResult{}, err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.AppendResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := e.appendTx(ctx, tx, storage.ScopeFromLenient(ctx).TenantID, ev)
	if err != nil {
		return storage.AppendResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return storage.AppendResult{}, err
	}
	return result, nil
}

func validateMySQLEvent(ev observability.AgentEvent) error {
	if ev.RunID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "run_id required")
	}
	if err := storage.ValidateEventIdentifiers(ev); err != nil {
		return err
	}
	if !observability.IsRegisteredEventType(ev.EventType) {
		return storage.NewError(storage.ErrInvalidArgument, "unregistered event_type: "+string(ev.EventType))
	}
	if ev.SchemaVersion != "" && ev.SchemaVersion != observability.AgentEventSchemaVersion {
		return storage.NewError(storage.ErrInvalidArgument, "illegal schema_version: "+ev.SchemaVersion)
	}
	return nil
}

// appendTx lets ResumeStore update the claim and append its canonical event in
// the same InnoDB transaction.
func (e *eventStore) appendTx(ctx context.Context, tx *sql.Tx, tenantID string, ev observability.AgentEvent) (storage.AppendResult, error) {
	// Idempotency by event_id.
	if ev.EventID != "" {
		if existing, ok, err := e.getByEventID(ctx, tx, ev.EventID); err != nil {
			return storage.AppendResult{}, err
		} else if ok {
			return storage.AppendResult{Event: existing, Idempotent: true}, nil
		}
	}
	// Idempotency by (tenant, idempotency_key).
	if ev.IdempotencyKey != "" {
		if existing, ok, err := e.getByIdem(ctx, tx, tenantID, ev.IdempotencyKey); err != nil {
			return storage.AppendResult{}, err
		} else if ok {
			return storage.AppendResult{Event: existing, Idempotent: true}, nil
		}
	}

	// Serialize appends for this run.
	var locked int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM runs WHERE run_id=? FOR UPDATE", e.schema.id("runs", "run_id", ev.RunID)).Scan(&locked); err != nil {
		return storage.AppendResult{}, err
	}

	var next int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0)+1 FROM agent_events WHERE run_id=?", e.schema.id("agent_events", "run_id", ev.RunID)).Scan(&next); err != nil {
		return storage.AppendResult{}, err
	}

	if ev.EventID == "" {
		ev.EventID = e.idgen().NewEventID()
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
	ev.Sequence = next

	errJSON := jsonOrNil(ev.Error)
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_events
		(event_id, run_id, sequence, tenant_id, session_id, step_id, agent_id, trace_id, span_id, parent_span_id, agent_type, runtime,
		 event_type, visibility, schema_version, idempotency_key, payload, payload_preview, payload_ref,
		 `+e.usageColumn()+`, debug_ref, error, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.schema.id("agent_events", "event_id", ev.EventID), e.schema.id("agent_events", "run_id", ev.RunID), ev.Sequence, nullStr(tenantID), nullStr(ev.SessionID), nullStr(ev.StepID),
		nullStr(ev.AgentID), nullStr(ev.TraceID), nullStr(ev.SpanID), nullStr(ev.ParentSpanID), nullStr(ev.AgentType), nullStr(ev.Runtime),
		string(ev.EventType), string(ev.Visibility),
		ev.SchemaVersion, nullStr(ev.IdempotencyKey), rawOrNil(ev.Payload), rawOrNil(ev.PayloadPreview),
		nullStr(ev.PayloadRef), rawOrNil(ev.Usage), nullStr(ev.DebugRef), errJSON, ev.CreatedAt,
	); err != nil {
		return storage.AppendResult{}, err
	}
	return storage.AppendResult{Event: ev, Idempotent: false}, nil
}

func (e *eventStore) Query(ctx context.Context, q storage.EventQuery) ([]observability.AgentEvent, error) {
	if q.RunID == "" {
		return nil, storage.NewError(storage.ErrInvalidArgument, "run_id required")
	}
	var sb strings.Builder
	sb.WriteString(e.selectEventCols() + " FROM agent_events WHERE run_id=? AND sequence>?")
	args := []any{e.schema.id("agent_events", "run_id", q.RunID), q.AfterSequence}
	if len(q.Visibilities) > 0 {
		sb.WriteString(" AND visibility IN (" + placeholders(len(q.Visibilities)) + ")")
		for _, v := range q.Visibilities {
			args = append(args, string(v))
		}
	}
	if len(q.EventTypes) > 0 {
		sb.WriteString(" AND event_type IN (" + placeholders(len(q.EventTypes)) + ")")
		for _, t := range q.EventTypes {
			args = append(args, string(t))
		}
	}
	sb.WriteString(" ORDER BY sequence ASC")
	if q.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, q.Limit)
	}
	rows, err := e.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []observability.AgentEvent
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (e *eventStore) LastSequence(ctx context.Context, runID string) (int64, error) {
	var seq int64
	err := e.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM agent_events WHERE run_id=?", e.schema.id("agent_events", "run_id", runID)).Scan(&seq)
	return seq, err
}

func (e *eventStore) SequenceOf(ctx context.Context, runID, eventID string) (int64, error) {
	var seq int64
	err := e.db.QueryRowContext(ctx, "SELECT sequence FROM agent_events WHERE run_id=? AND event_id=?", e.schema.id("agent_events", "run_id", runID), e.schema.id("agent_events", "event_id", eventID)).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, storage.NewError(storage.ErrNotFound, "event not found: "+eventID)
	}
	return seq, err
}

func (e *eventStore) getByEventID(ctx context.Context, tx *sql.Tx, eventID string) (observability.AgentEvent, bool, error) {
	row := tx.QueryRowContext(ctx, e.selectEventCols()+" FROM agent_events WHERE event_id=?", e.schema.id("agent_events", "event_id", eventID))
	ev, err := scanEvent(row)
	if err == sql.ErrNoRows {
		return observability.AgentEvent{}, false, nil
	}
	if err == nil {
		ev.EventID = eventID
	}
	return ev, err == nil, err
}

func (e *eventStore) getByIdem(ctx context.Context, tx *sql.Tx, tenant, key string) (observability.AgentEvent, bool, error) {
	row := tx.QueryRowContext(ctx, e.selectEventCols()+" FROM agent_events WHERE tenant_id=? AND idempotency_key=?", tenant, key)
	ev, err := scanEvent(row)
	if err == sql.ErrNoRows {
		return observability.AgentEvent{}, false, nil
	}
	return ev, err == nil, err
}

func (e *eventStore) usageColumn() string {
	if e.schema.eventUsageColumn != "" {
		return e.schema.eventUsageColumn
	}
	return "usage"
}

func (e *eventStore) selectEventCols() string {
	return `SELECT event_id, run_id, sequence, tenant_id, session_id, step_id, agent_id,
	trace_id, span_id, parent_span_id, agent_type, runtime, event_type, visibility, schema_version, idempotency_key, payload,
	payload_preview, payload_ref, ` + e.usageColumn() + `, debug_ref, error, created_at`
}

var _ storage.EventStore = (*eventStore)(nil)
