package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// eventStore 是 SQLite EventStore。sequence 分配在事务内做 MAX(sequence)+1;
// 由于连接数固定为 1,写事务被串行化,UNIQUE(run_id,sequence) 兜底。
type eventStore struct {
	db  *sql.DB
	ids observability.IDGenerator
}

func (e *eventStore) idgen() observability.IDGenerator {
	if e.ids == nil {
		e.ids = observability.NewULIDGenerator("")
	}
	return e.ids
}

const selectEventCols = `SELECT event_id, run_id, sequence, session_id, step_id, agent_id,
	trace_id, span_id, parent_span_id, agent_type, runtime, event_type, visibility, schema_version, idempotency_key, payload,
	payload_preview, payload_ref, usage, debug_ref, error, created_at`

func (e *eventStore) Append(ctx context.Context, ev observability.AgentEvent) (storage.AppendResult, error) {
	if err := validateSQLiteEvent(ev); err != nil {
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

func validateSQLiteEvent(ev observability.AgentEvent) error {
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

// appendTx keeps sequence allocation and event idempotency inside the caller's
// transaction. ResumeStore uses it to avoid a non-atomic nested transaction.
func (e *eventStore) appendTx(ctx context.Context, tx *sql.Tx, tenantID string, ev observability.AgentEvent) (storage.AppendResult, error) {
	if ev.EventID != "" {
		if existing, ok, err := e.getBy(ctx, tx, "event_id=?", ev.EventID); err != nil {
			return storage.AppendResult{}, err
		} else if ok {
			return storage.AppendResult{Event: existing, Idempotent: true}, nil
		}
	}
	if ev.IdempotencyKey != "" {
		if existing, ok, err := e.getBy(ctx, tx, "tenant_id=? AND idempotency_key=?", tenantID, ev.IdempotencyKey); err != nil {
			return storage.AppendResult{}, err
		} else if ok {
			return storage.AppendResult{Event: existing, Idempotent: true}, nil
		}
	}

	var next int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0)+1 FROM agent_events WHERE run_id=?", ev.RunID).Scan(&next); err != nil {
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

	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_events
		(event_id, run_id, sequence, tenant_id, session_id, step_id, agent_id, trace_id, span_id, parent_span_id, agent_type, runtime,
		 event_type, visibility, schema_version, idempotency_key, payload, payload_preview, payload_ref,
		 usage, debug_ref, error, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.EventID, ev.RunID, ev.Sequence, nullStr(tenantID), nullStr(ev.SessionID), nullStr(ev.StepID),
		nullStr(ev.AgentID), nullStr(ev.TraceID), nullStr(ev.SpanID), nullStr(ev.ParentSpanID), nullStr(ev.AgentType), nullStr(ev.Runtime),
		string(ev.EventType), string(ev.Visibility),
		ev.SchemaVersion, nullStr(ev.IdempotencyKey), rawStr(ev.Payload), rawStr(ev.PayloadPreview),
		nullStr(ev.PayloadRef), rawStr(ev.Usage), nullStr(ev.DebugRef), errStr(ev.Error), tsVal(ev.CreatedAt),
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
	sb.WriteString(selectEventCols + " FROM agent_events WHERE run_id=? AND sequence>?")
	args := []any{q.RunID, q.AfterSequence}
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
	err := e.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM agent_events WHERE run_id=?", runID).Scan(&seq)
	return seq, err
}

func (e *eventStore) SequenceOf(ctx context.Context, runID, eventID string) (int64, error) {
	var seq int64
	err := e.db.QueryRowContext(ctx, "SELECT sequence FROM agent_events WHERE run_id=? AND event_id=?", runID, eventID).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, storage.NewError(storage.ErrNotFound, "event not found: "+eventID)
	}
	return seq, err
}

type rowScanner interface{ Scan(dest ...any) error }

func (e *eventStore) getBy(ctx context.Context, tx *sql.Tx, where string, args ...any) (observability.AgentEvent, bool, error) {
	row := tx.QueryRowContext(ctx, selectEventCols+" FROM agent_events WHERE "+where, args...)
	ev, err := scanEvent(row)
	if err == sql.ErrNoRows {
		return observability.AgentEvent{}, false, nil
	}
	return ev, err == nil, err
}

func scanEvent(s rowScanner) (observability.AgentEvent, error) {
	var (
		ev                                observability.AgentEvent
		session, step, agent, trace, span sql.NullString
		parentSpan, agentType, runtime    sql.NullString
		idem, payloadRef, debugRef        sql.NullString
		eventType, visibility             string
		payload, preview, usage, errText  sql.NullString
		created                           int64
	)
	if err := s.Scan(
		&ev.EventID, &ev.RunID, &ev.Sequence, &session, &step, &agent,
		&trace, &span, &parentSpan, &agentType, &runtime, &eventType, &visibility, &ev.SchemaVersion, &idem, &payload,
		&preview, &payloadRef, &usage, &debugRef, &errText, &created,
	); err != nil {
		return observability.AgentEvent{}, err
	}
	ev.SessionID, ev.StepID, ev.AgentID = session.String, step.String, agent.String
	ev.TraceID, ev.SpanID = trace.String, span.String
	ev.ParentSpanID, ev.AgentType, ev.Runtime = parentSpan.String, agentType.String, runtime.String
	ev.EventType = observability.EventType(eventType)
	ev.Visibility = observability.EventVisibility(visibility)
	ev.IdempotencyKey, ev.PayloadRef, ev.DebugRef = idem.String, payloadRef.String, debugRef.String
	ev.Payload = jsonRaw(payload.String)
	ev.PayloadPreview = jsonRaw(preview.String)
	ev.Usage = jsonRaw(usage.String)
	ev.CreatedAt = tsTime(created)
	if errText.String != "" {
		var ee observability.EventError
		if json.Unmarshal([]byte(errText.String), &ee) == nil {
			ev.Error = &ee
		}
	}
	return ev, nil
}

func jsonRaw(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func rawStr(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

func errStr(e *observability.EventError) any {
	if e == nil {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	return string(b)
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

var _ storage.EventStore = (*eventStore)(nil)
