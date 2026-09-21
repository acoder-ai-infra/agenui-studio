package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type openTurnStore struct {
	db  *sql.DB
	ids observability.IDGenerator
}

func (s *openTurnStore) Commit(ctx context.Context, cmd storage.OpenTurnCommand) (storage.OpenTurnCommit, error) {
	if err := storage.ValidateOpenTurnCommandIdentifiers(cmd); err != nil {
		return storage.OpenTurnCommit{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if cmd.IdempotencyKey != "" {
		var hash, sid, rid, mid string
		err = tx.QueryRowContext(ctx, `SELECT request_hash,session_id,run_id,message_id FROM open_turn_idempotency
			WHERE tenant_id=? AND user_id=? AND session_scope=? AND idem_key=?`, cmd.Run.TenantID, cmd.Session.UserID, cmd.IdempotencyScope, cmd.IdempotencyKey).
			Scan(&hash, &sid, &rid, &mid)
		if err == nil {
			if hash != cmd.RequestHash {
				return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "idempotency key reused with a different request")
			}
			return s.replay(ctx, tx, sid, rid, mid)
		}
		if err != sql.ErrNoRows {
			return storage.OpenTurnCommit{}, err
		}
	}

	now := time.Now()
	sess, err := (&sessionStore{db: s.db}).get(ctx, tx, cmd.Session.ID)
	if err != nil && !storage.IsErrorCode(err, storage.ErrNotFound) {
		return storage.OpenTurnCommit{}, err
	}
	if sess != nil {
		if sess.TenantID != cmd.Session.TenantID || sess.UserID != cmd.Session.UserID {
			return storage.OpenTurnCommit{}, storage.NewError(storage.ErrPermissionDenied, "session owner mismatch")
		}
		if sess.AgentID != "" && cmd.Session.AgentID != "" && sess.AgentID != cmd.Session.AgentID {
			return storage.OpenTurnCommit{}, storage.NewError(storage.ErrPermissionDenied, "session agent mismatch")
		}
	} else {
		sess = &cmd.Session
		sess.Status = storage.SessionStatusActive
		sess.CreatedAt = now
		sess.UpdatedAt = now
		sess.Version = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions(session_id,tenant_id,user_id,channel,agent_id,title,status,created_at,updated_at,version,metadata) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			sess.ID, sess.TenantID, nullStr(sess.UserID), nullStr(sess.Channel), nullStr(sess.AgentID), nullStr(sess.Title), sess.Status, tsVal(now), tsVal(now), 1, mapToJSON(sess.Metadata))
		if err != nil {
			return storage.OpenTurnCommit{}, err
		}
	}
	var activeRuns int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs
		WHERE session_id=? AND (parent_run_id IS NULL OR parent_run_id='')
		AND status IN ('created','running','waiting_control','resuming')`, sess.ID).Scan(&activeRuns); err != nil {
		return storage.OpenTurnCommit{}, err
	}
	if activeRuns != 0 {
		return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "session already has an active top-level run")
	}
	run := cmd.Run
	run.Status = storage.RunStatusCreated
	run.Version = 1
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(run_id,session_id,turn_id,parent_run_id,tenant_id,agent_id,runtime,status,trace_id,config_snapshot_ref,context_snapshot_ref,agent_binding_id,started_at,ended_at,version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.RunID, run.SessionID, nullStr(run.TurnID), nullStr(run.ParentRunID), run.TenantID, nullStr(run.AgentID), nullStr(run.Runtime), string(run.Status), nullStr(run.TraceID), nullStr(run.ConfigSnapshotRef), nullStr(run.ContextSnapshotRef), nullStr(run.AgentBindingID), int64(0), int64(0), 1)
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	msg := cmd.Message
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
	}
	if msg.Visibility == "" {
		msg.Visibility = observability.VisibilityUserVisible
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(message_id,session_id,turn_id,run_id,tenant_id,role,visibility,content_ref,content_preview,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		msg.ID, msg.SessionID, nullStr(msg.TurnID), nullStr(msg.RunID), msg.TenantID, msg.Role, string(msg.Visibility), nullStr(msg.ContentRef), nullStr(msg.ContentPreview), tsVal(msg.CreatedAt))
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	events := make([]observability.AgentEvent, 0, len(cmd.Events))
	for i, ev := range cmd.Events {
		if !observability.IsRegisteredEventType(ev.EventType) {
			return storage.OpenTurnCommit{}, storage.NewError(storage.ErrInvalidArgument, "unregistered event type")
		}
		if ev.EventID == "" {
			ev.EventID = s.ids.NewEventID()
		}
		ev.Sequence = int64(i + 1)
		ev.SchemaVersion = observability.AgentEventSchemaVersion
		if ev.CreatedAt.IsZero() {
			ev.CreatedAt = now
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_events(event_id,run_id,sequence,tenant_id,session_id,step_id,agent_id,trace_id,span_id,event_type,visibility,schema_version,idempotency_key,payload,payload_preview,payload_ref,usage,debug_ref,error,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ev.EventID, ev.RunID, ev.Sequence, nullStr(run.TenantID), nullStr(ev.SessionID), nullStr(ev.StepID), nullStr(ev.AgentID), nullStr(ev.TraceID), nullStr(ev.SpanID), string(ev.EventType), string(ev.Visibility), ev.SchemaVersion, nil, rawStr(ev.Payload), rawStr(ev.PayloadPreview), nullStr(ev.PayloadRef), rawStr(ev.Usage), nullStr(ev.DebugRef), errStr(ev.Error), tsVal(ev.CreatedAt))
		if err != nil {
			return storage.OpenTurnCommit{}, err
		}
		events = append(events, ev)
	}
	if cmd.IdempotencyKey != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO open_turn_idempotency(tenant_id,user_id,session_scope,idem_key,request_hash,session_id,run_id,message_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, cmd.Run.TenantID, cmd.Session.UserID, cmd.IdempotencyScope, cmd.IdempotencyKey, cmd.RequestHash, sess.ID, run.RunID, msg.ID, tsVal(now))
		if err != nil {
			return storage.OpenTurnCommit{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return storage.OpenTurnCommit{}, err
	}
	return storage.OpenTurnCommit{Session: sess, Run: &run, Message: &msg, Events: events}, nil
}

func (s *openTurnStore) replay(ctx context.Context, tx *sql.Tx, sid, rid, mid string) (storage.OpenTurnCommit, error) {
	sess, err := (&sessionStore{db: s.db}).get(ctx, tx, sid)
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	run, err := (&runStore{db: s.db}).get(ctx, tx, rid)
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	msg, err := scanMessage(tx.QueryRowContext(ctx, `SELECT message_id,session_id,turn_id,run_id,tenant_id,role,visibility,content_ref,content_preview,created_at FROM messages WHERE message_id=?`, mid))
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	return storage.OpenTurnCommit{Session: sess, Run: run, Message: msg, Idempotent: true}, nil
}

var _ storage.OpenTurnStore = (*openTurnStore)(nil)
