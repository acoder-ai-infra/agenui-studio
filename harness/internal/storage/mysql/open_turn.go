package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type openTurnStore struct {
	db     *sql.DB
	ids    observability.IDGenerator
	schema physicalSchema
}

// 幂等记录写入后不可变，读取不需要行锁。对尚不存在的 key 使用 FOR UPDATE
// 会在 MySQL 默认 RR 隔离级别创建 gap lock，并与并发 INSERT 形成死锁；唯一键
// 已经是最终 CAS，竞争失败者回滚后读取赢家即可。
const selectOpenTurnIdempotency = `SELECT request_hash,session_id,run_id,message_id
	FROM open_turn_idempotency
	WHERE tenant_id=? AND user_id=? AND session_scope=? AND idem_key=?`

func (s *openTurnStore) Commit(ctx context.Context, cmd storage.OpenTurnCommand) (storage.OpenTurnCommit, error) {
	if err := storage.ValidateOpenTurnCommandIdentifiers(cmd); err != nil {
		return storage.OpenTurnCommit{}, err
	}
	result, err := s.commitOnce(ctx, cmd)
	if err == nil || cmd.IdempotencyKey == "" || !isDuplicate(err) {
		return result, err
	}
	// A concurrent winner may have committed the same idempotency key after our
	// initial lookup. Our transaction rolled back; resolve the winner explicitly.
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return storage.OpenTurnCommit{}, beginErr
	}
	defer func() { _ = tx.Rollback() }()
	var hash, sid, rid, mid string
	if qerr := tx.QueryRowContext(ctx, selectOpenTurnIdempotency,
		s.schema.id("open_turn_idempotency", "tenant_id", cmd.Run.TenantID),
		s.schema.id("open_turn_idempotency", "user_id", cmd.Session.UserID),
		s.schema.id("open_turn_idempotency", "session_scope", cmd.IdempotencyScope),
		s.schema.id("open_turn_idempotency", "idem_key", cmd.IdempotencyKey),
	).Scan(&hash, &sid, &rid, &mid); qerr != nil {
		return storage.OpenTurnCommit{}, err
	}
	if hash != cmd.RequestHash {
		return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "idempotency key reused with a different request")
	}
	return s.replay(ctx, tx, sid, rid, mid)
}

func (s *openTurnStore) commitOnce(ctx context.Context, cmd storage.OpenTurnCommand) (storage.OpenTurnCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if cmd.IdempotencyKey != "" {
		var hash, sid, rid, mid string
		err = tx.QueryRowContext(ctx, selectOpenTurnIdempotency,
			s.schema.id("open_turn_idempotency", "tenant_id", cmd.Run.TenantID),
			s.schema.id("open_turn_idempotency", "user_id", cmd.Session.UserID),
			s.schema.id("open_turn_idempotency", "session_scope", cmd.IdempotencyScope),
			s.schema.id("open_turn_idempotency", "idem_key", cmd.IdempotencyKey),
		).Scan(&hash, &sid, &rid, &mid)
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
	physicalSessionID := s.schema.id("sessions", "id", cmd.Session.ID)
	sess, err := (&sessionStore{db: s.db, schema: s.schema}).get(ctx, tx, cmd.Session.ID)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,tenant_id,user_id,channel,agent_id,title,status,created_at,updated_at,version,metadata) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, physicalSessionID, sess.TenantID, sess.UserID, nullStr(sess.Channel), nullStr(sess.AgentID), nullStr(sess.Title), sess.Status, now, now, 1, jsonOrNil(sess.Metadata))
		if err != nil {
			return storage.OpenTurnCommit{}, err
		}
	}
	// Serialize top-level turn admission on the durable Session row. This keeps
	// the rule valid across instances without a distributed lock or lease table.
	var lockedSessionID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id=? FOR UPDATE`, physicalSessionID).Scan(&lockedSessionID); err != nil {
		return storage.OpenTurnCommit{}, err
	}
	// A same-key request may have missed the fast lookup and waited behind the
	// winner's Session lock. Replay it before applying the active-Run conflict.
	if cmd.IdempotencyKey != "" {
		var hash, sid, rid, mid string
		err = tx.QueryRowContext(ctx, selectOpenTurnIdempotency,
			s.schema.id("open_turn_idempotency", "tenant_id", cmd.Run.TenantID),
			s.schema.id("open_turn_idempotency", "user_id", cmd.Session.UserID),
			s.schema.id("open_turn_idempotency", "session_scope", cmd.IdempotencyScope),
			s.schema.id("open_turn_idempotency", "idem_key", cmd.IdempotencyKey),
		).Scan(&hash, &sid, &rid, &mid)
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
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(run_id,session_id,turn_id,parent_run_id,tenant_id,agent_id,runtime,status,trace_id,config_snapshot_ref,context_snapshot_ref,agent_binding_id,started_at,ended_at,version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.schema.id("runs", "run_id", run.RunID), run.SessionID, nullStr(run.TurnID), nullStr(run.ParentRunID), run.TenantID, nullStr(run.AgentID), nullStr(run.Runtime), string(run.Status), nullStr(run.TraceID), nullStr(run.ConfigSnapshotRef), nullStr(run.ContextSnapshotRef), nullStr(run.AgentBindingID), nil, nil, 1)
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
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,session_id,turn_id,run_id,tenant_id,role,visibility,content_ref,content_preview,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, s.schema.id("messages", "id", msg.ID), msg.SessionID, nullStr(msg.TurnID), nullStr(msg.RunID), msg.TenantID, msg.Role, string(msg.Visibility), nullStr(msg.ContentRef), nullStr(msg.ContentPreview), msg.CreatedAt)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_events(event_id,run_id,sequence,tenant_id,session_id,step_id,agent_id,trace_id,span_id,event_type,visibility,schema_version,idempotency_key,payload,payload_preview,payload_ref,`+s.usageColumn()+`,debug_ref,error,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.schema.id("agent_events", "event_id", ev.EventID), s.schema.id("agent_events", "run_id", ev.RunID), ev.Sequence, run.TenantID, nullStr(ev.SessionID), nullStr(ev.StepID), nullStr(ev.AgentID), nullStr(ev.TraceID), nullStr(ev.SpanID), string(ev.EventType), string(ev.Visibility), ev.SchemaVersion, nil, rawOrNil(ev.Payload), rawOrNil(ev.PayloadPreview), nullStr(ev.PayloadRef), rawOrNil(ev.Usage), nullStr(ev.DebugRef), jsonOrNil(ev.Error), ev.CreatedAt)
		if err != nil {
			return storage.OpenTurnCommit{}, err
		}
		events = append(events, ev)
	}
	if cmd.IdempotencyKey != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO open_turn_idempotency(tenant_id,user_id,session_scope,idem_key,request_hash,session_id,run_id,message_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			s.schema.id("open_turn_idempotency", "tenant_id", cmd.Run.TenantID),
			s.schema.id("open_turn_idempotency", "user_id", cmd.Session.UserID),
			s.schema.id("open_turn_idempotency", "session_scope", cmd.IdempotencyScope),
			s.schema.id("open_turn_idempotency", "idem_key", cmd.IdempotencyKey),
			cmd.RequestHash, sess.ID, run.RunID, msg.ID, now)
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
	sess, err := (&sessionStore{db: s.db, schema: s.schema}).get(ctx, tx, sid)
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	run, err := (&runStore{db: s.db, schema: s.schema}).get(ctx, tx, rid)
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	msg, err := scanMessage(tx.QueryRowContext(ctx, `SELECT id,session_id,turn_id,run_id,tenant_id,role,visibility,content_ref,content_preview,created_at FROM messages WHERE id=?`, s.schema.id("messages", "id", mid)))
	if err != nil {
		return storage.OpenTurnCommit{}, err
	}
	msg.ID = mid
	return storage.OpenTurnCommit{Session: sess, Run: run, Message: msg, Idempotent: true}, nil
}

func (s *openTurnStore) usageColumn() string {
	if s.schema.eventUsageColumn != "" {
		return s.schema.eventUsageColumn
	}
	return "usage"
}

var _ storage.OpenTurnStore = (*openTurnStore)(nil)
