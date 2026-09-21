package memory

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type openTurnStore struct {
	sessions *sessionStore
	runs     *runStore
	messages *messageStore
	events   *eventStore
	idem     map[string]openTurnRecord
}

type openTurnRecord struct {
	hash      string
	sessionID string
	runID     string
	messageID string
}

func newOpenTurnStore(s *sessionStore, r *runStore, m *messageStore, e *eventStore) *openTurnStore {
	return &openTurnStore{sessions: s, runs: r, messages: m, events: e, idem: map[string]openTurnRecord{}}
}

func (s *openTurnStore) Commit(ctx context.Context, cmd storage.OpenTurnCommand) (storage.OpenTurnCommit, error) {
	if cmd.Session.ID == "" || cmd.Run.RunID == "" || cmd.Message.ID == "" || len(cmd.Events) == 0 {
		return storage.OpenTurnCommit{}, storage.NewError(storage.ErrInvalidArgument, "incomplete open turn command")
	}
	if err := storage.ValidateOpenTurnCommandIdentifiers(cmd); err != nil {
		return storage.OpenTurnCommit{}, err
	}
	for _, ev := range cmd.Events {
		if !observability.IsRegisteredEventType(ev.EventType) {
			return storage.OpenTurnCommit{}, storage.NewError(storage.ErrInvalidArgument, "unregistered event type")
		}
	}
	scope := storage.ScopeFromLenient(ctx)
	if err := scope.EnforceTenant(cmd.Run.TenantID); err != nil {
		return storage.OpenTurnCommit{}, err
	}

	// One fixed lock order makes the whole command atomic to every memory store.
	s.sessions.mu.Lock()
	s.runs.mu.Lock()
	s.messages.mu.Lock()
	s.events.mu.Lock()
	defer s.sessions.mu.Unlock()
	defer s.runs.mu.Unlock()
	defer s.messages.mu.Unlock()
	defer s.events.mu.Unlock()

	idemKey := cmd.Run.TenantID + "|" + cmd.Session.UserID + "|" + cmd.IdempotencyScope + "|" + cmd.IdempotencyKey
	if cmd.IdempotencyKey != "" {
		if rec, ok := s.idem[idemKey]; ok {
			if rec.hash != cmd.RequestHash {
				return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "idempotency key reused with a different request")
			}
			return s.replay(rec)
		}
	}

	now := time.Now()
	sess, exists := s.sessions.byID[cmd.Session.ID]
	if exists {
		if sess.TenantID != cmd.Session.TenantID || sess.UserID != cmd.Session.UserID {
			return storage.OpenTurnCommit{}, storage.NewError(storage.ErrPermissionDenied, "session owner mismatch")
		}
		if sess.AgentID != "" && cmd.Session.AgentID != "" && sess.AgentID != cmd.Session.AgentID {
			return storage.OpenTurnCommit{}, storage.NewError(storage.ErrPermissionDenied, "session agent mismatch")
		}
	} else {
		clone := cmd.Session
		clone.Status = storage.SessionStatusActive
		clone.CreatedAt, clone.UpdatedAt, clone.Version = now, now, 1
		s.sessions.byID[clone.ID] = &clone
		sess = &clone
	}
	for _, existing := range s.runs.byID {
		if existing.SessionID == sess.ID && existing.ParentRunID == "" && existing.Status.BlocksNewTopLevelTurn() {
			return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "session already has an active top-level run")
		}
	}
	if _, ok := s.runs.byID[cmd.Run.RunID]; ok {
		return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "run already exists")
	}
	run := cmd.Run
	run.Status, run.Version = storage.RunStatusCreated, 1
	s.runs.byID[run.RunID] = &run
	msg := cmd.Message
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
	}
	if msg.Visibility == "" {
		msg.Visibility = observability.VisibilityUserVisible
	}
	s.messages.bySession[msg.SessionID] = append(s.messages.bySession[msg.SessionID], &msg)
	events := make([]observability.AgentEvent, 0, len(cmd.Events))
	for _, ev := range cmd.Events {
		if ev.EventID == "" {
			ev.EventID = s.events.ids.NewEventID()
		}
		ev.SchemaVersion = observability.AgentEventSchemaVersion
		if ev.CreatedAt.IsZero() {
			ev.CreatedAt = now
		}
		s.events.seq[run.RunID]++
		ev.Sequence = s.events.seq[run.RunID]
		s.events.byRun[run.RunID] = append(s.events.byRun[run.RunID], ev)
		s.events.byEvent[ev.EventID] = run.RunID
		events = append(events, ev)
	}
	if cmd.IdempotencyKey != "" {
		s.idem[idemKey] = openTurnRecord{hash: cmd.RequestHash, sessionID: sess.ID, runID: run.RunID, messageID: msg.ID}
	}
	sessCopy, runCopy, msgCopy := *sess, run, msg
	return storage.OpenTurnCommit{Session: &sessCopy, Run: &runCopy, Message: &msgCopy, Events: events}, nil
}

func (s *openTurnStore) replay(rec openTurnRecord) (storage.OpenTurnCommit, error) {
	sess, sok := s.sessions.byID[rec.sessionID]
	run, rok := s.runs.byID[rec.runID]
	if !sok || !rok {
		return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "idempotency record references incomplete turn")
	}
	var msg *storage.Message
	for _, candidate := range s.messages.bySession[rec.sessionID] {
		if candidate.ID == rec.messageID {
			clone := *candidate
			msg = &clone
			break
		}
	}
	if msg == nil {
		return storage.OpenTurnCommit{}, storage.NewError(storage.ErrConflict, "idempotency record references missing message")
	}
	sessCopy, runCopy := *sess, *run
	events := append([]observability.AgentEvent(nil), s.events.byRun[run.RunID]...)
	return storage.OpenTurnCommit{Session: &sessCopy, Run: &runCopy, Message: msg, Events: events, Idempotent: true}, nil
}

var _ storage.OpenTurnStore = (*openTurnStore)(nil)
