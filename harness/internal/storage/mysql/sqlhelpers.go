package mysql

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(s scanner) (observability.AgentEvent, error) {
	var (
		ev                                        observability.AgentEvent
		tenant, session, step, agent, trace, span sql.NullString
		parentSpan, agentType, runtime            sql.NullString
		idem, payloadRef, debugRef                sql.NullString
		eventType, visibility                     string
		payload, preview, usage, errBytes         []byte
		created                                   time.Time
	)
	if err := s.Scan(
		&ev.EventID, &ev.RunID, &ev.Sequence, &tenant, &session, &step, &agent,
		&trace, &span, &parentSpan, &agentType, &runtime, &eventType, &visibility, &ev.SchemaVersion, &idem, &payload,
		&preview, &payloadRef, &usage, &debugRef, &errBytes, &created,
	); err != nil {
		return observability.AgentEvent{}, err
	}
	_ = tenant // tenant column is scope-derived; AgentEvent carries no tenant field
	ev.SessionID = session.String
	ev.StepID = step.String
	ev.AgentID = agent.String
	ev.TraceID = trace.String
	ev.SpanID = span.String
	ev.ParentSpanID = parentSpan.String
	ev.AgentType = agentType.String
	ev.Runtime = runtime.String
	ev.EventType = observability.EventType(eventType)
	ev.Visibility = observability.EventVisibility(visibility)
	ev.IdempotencyKey = idem.String
	ev.PayloadRef = payloadRef.String
	ev.DebugRef = debugRef.String
	ev.Payload = rawFromBytes(payload)
	ev.PayloadPreview = rawFromBytes(preview)
	ev.Usage = rawFromBytes(usage)
	ev.CreatedAt = created
	if len(errBytes) > 0 {
		var ee observability.EventError
		if err := json.Unmarshal(errBytes, &ee); err == nil {
			ev.Error = &ee
		}
	}
	return ev, nil
}

func rawFromBytes(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

func rawOrNil(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return []byte(r)
}

func jsonOrNil(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// isDuplicate reports whether err is a MySQL unique/primary-key violation
// (driver error 1062 "Duplicate entry"). Kept string-based so the mysql package
// need not depend on the concrete driver's error type.
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Duplicate entry") || strings.Contains(err.Error(), "1062")
}

// metaToJSON marshals a metadata map for a JSON column (nil for empty).
func metaToJSON(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// jsonToMeta unmarshals a JSON column back into a metadata map.
func jsonToMeta(b []byte) map[string]string {
	if len(b) == 0 {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}
