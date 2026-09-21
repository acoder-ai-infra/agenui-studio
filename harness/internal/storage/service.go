package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/oklog/ulid/v2"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// SessionService creates and loads durable sessions.
type SessionService struct {
	Stores Stores
}

// GetOrCreate loads an existing session or creates a new active one. When id is
// empty a new session id is generated.
func (s *SessionService) GetOrCreate(ctx context.Context, id string, seed Session) (*Session, error) {
	if id != "" {
		if existing, err := s.Stores.Sessions.Get(ctx, id); err == nil {
			return existing, nil
		} else if !IsErrorCode(err, ErrNotFound) {
			return nil, err
		}
	}
	scope := ScopeFromLenient(ctx)
	seed.ID = firstNonEmpty(id, seed.ID, "sess_"+newULID())
	seed.Status = SessionStatusActive
	if seed.TenantID == "" {
		seed.TenantID = scope.TenantID
	}
	if seed.UserID == "" {
		seed.UserID = scope.UserID
	}
	if err := s.Stores.Sessions.Create(ctx, &seed); err != nil {
		return nil, err
	}
	return &seed, nil
}

// RunService orchestrates the fact-first write ordering for a turn
// (session-run-storage-design.md §6, execution §1). It optionally invokes the
// ContextSnapshotBuilder before the run is dispatched (D5).
type RunService struct {
	Stores   Stores
	Sessions *SessionService
	// Snapshot generates the context snapshot before dispatch; may be nil, in
	// which case snapshot binding is skipped.
	Snapshot ContextSnapshotBuilder
	IDs      observability.IDGenerator
}

// NewRunService wires a RunService with a default ULID generator.
func NewRunService(stores Stores, snapshot ContextSnapshotBuilder) *RunService {
	return &RunService{
		Stores:   stores,
		Sessions: &SessionService{Stores: stores},
		Snapshot: snapshot,
		IDs:      observability.NewULIDGenerator(""),
	}
}

// OpenTurnRequest describes a new user turn.
type OpenTurnRequest struct {
	SessionID         string
	TenantID          string
	UserID            string
	AgentID           string
	Channel           string
	Runtime           string
	ConfigSnapshotRef string
	AgentBindingID    string // produced by upstream binding/business chain

	UserContentPreview string
	UserContentRef     string

	// IdempotencyKey, when set (e.g. from an HTTP Idempotency-Key header), makes
	// the turn's session/turn/run/message ids deterministic from (tenant, key) so
	// a retried request re-resolves the same records instead of creating
	// duplicate Turn/Run/Message rows.
	IdempotencyKey       string
	DeferContextSnapshot bool
	ContextAttachments   []ContextAttachmentSnapshot

	// ContextFragments 是入口层预回合管线（kernel.TurnPipeline 的
	// ContextContributor 阶段）产出的业务上下文片段。存储层不解释语义，
	// 仅原样回传到 OpenTurnResult，由 dispatcher 透传进 ModelContext 装配。
	ContextFragments []contextpkg.ContextFragment

	// ScopedData 是入口层（SDK Start / HTTP 入口）随本 Turn 附带的 per-Run
	// 状态包（已合并 RunInitializer 产出的冻结快照）。与 ContextFragments
	// 同为透传字段：存储层不解释语义、不持久化，仅原样回传到
	// OpenTurnResult，由 dispatcher 透传进 Runtime 的 RunRequest.ScopedData。
	ScopedData []ScopedDataItem
}

// ScopedDataItem 是随 Turn 透传的 per-Run 状态条目（字段与
// harness.ScopedDataItem 对齐；storage 不反向依赖 SDK / runtime 包，故
// 本地定义同形 DTO，边界上由调用方互拷贝）。
type ScopedDataItem struct {
	Key        string
	Source     string
	Visibility string
	Value      json.RawMessage
	Ref        string
	Hash       string
}

// OpenTurnResult is the outcome of OpenTurn. Run is left in status=created; the
// runtimestore.Bridge emits run_started (created->running) on dispatch.
type OpenTurnResult struct {
	Session            *Session
	Run                *Run
	Turn               *Turn
	UserMessage        *Message
	ContextSnapshotRef string
	Idempotent         bool
	// ContextFragments 原样回传 OpenTurnRequest.ContextFragments，供
	// dispatcher 在构建 RunRequest 时携带。
	ContextFragments []contextpkg.ContextFragment
	// ScopedData 原样回传 OpenTurnRequest.ScopedData，供 dispatcher 在构建
	// RunRequest 时携带（冻结快照，Runtime 与 BeforeModelHook 只读消费）。
	ScopedData []ScopedDataItem
}

// OpenTurn executes the standard write ordering:
//
//	session -> user message (+user_message_received)
//	-> run created (+run_created)
//	-> context snapshot build (D5) -> bind (+context_snapshot_created | context_build_failed)
//	-> agent binding (+agent_binding)
//
// Facts (session/message/run + user_message_received/run_created) are persisted
// before the derived context snapshot, so the turn is queryable even if snapshot
// building fails (fact-first).
func (s *RunService) OpenTurn(ctx context.Context, req OpenTurnRequest) (*OpenTurnResult, error) {
	if s.Stores.Turns == nil {
		return nil, NewError(ErrUnsupportedCapability, "atomic open turn store is required")
	}
	scope := ScopeFromLenient(ctx)
	trace := observability.MustTraceContext(ctx)
	tenant := firstNonEmpty(req.TenantID, scope.TenantID)
	userID := firstNonEmpty(req.UserID, scope.UserID)
	if err := ValidateOpenTurnRequestIdentifiers(req, tenant, userID); err != nil {
		return nil, err
	}
	title := titleFromUserInput(req.UserContentPreview, req.UserContentRef)
	sessionScope := req.SessionID
	if sessionScope == "" {
		sessionScope = "<new>"
	}
	sessionID := firstNonEmpty(req.SessionID, "sess_"+newULID())
	turnID, runID, msgID := "turn_"+newULID(), s.IDs.NewRunID(), "msg_"+newULID()

	turn := &Turn{ID: turnID, SessionID: sessionID, CreatedAt: time.Now()}
	run := &Run{
		RunID: runID, SessionID: sessionID, TurnID: turn.ID, TenantID: tenant,
		AgentID: req.AgentID, Runtime: req.Runtime, Status: RunStatusCreated,
		TraceID: trace.TraceID, ConfigSnapshotRef: req.ConfigSnapshotRef,
	}
	msg := &Message{
		ID: msgID, SessionID: sessionID, TurnID: turn.ID, RunID: run.RunID,
		TenantID: tenant, Role: "user", Visibility: observability.VisibilityUserVisible,
		ContentRef: req.UserContentRef, ContentPreview: req.UserContentPreview,
	}

	hashInput, _ := json.Marshal(struct {
		Tenant, User, Session, Agent, Channel, Runtime, Config, Binding, ContentRef, Content string
		DeferContextSnapshot                                                                 bool
		Attachments                                                                          []ContextAttachmentSnapshot
	}{tenant, userID, sessionScope, req.AgentID, req.Channel, req.Runtime, req.ConfigSnapshotRef, req.AgentBindingID, req.UserContentRef, req.UserContentPreview, req.DeferContextSnapshot, req.ContextAttachments})
	sum := sha256.Sum256(hashInput)
	commit, err := s.Stores.Turns.Commit(ctx, OpenTurnCommand{
		Session: Session{ID: sessionID, TenantID: tenant, UserID: userID, AgentID: req.AgentID, Channel: req.Channel, Title: title},
		Run:     *run, Message: *msg,
		Events: []observability.AgentEvent{
			{RunID: runID, SessionID: sessionID, AgentID: req.AgentID, TraceID: run.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID, EventType: observability.EventUserMessageReceived, Visibility: observability.VisibilityUserVisible, PayloadPreview: mustJSON(messagePayload(msg))},
			{RunID: runID, SessionID: sessionID, AgentID: req.AgentID, TraceID: run.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID, EventType: observability.EventRunCreated, Visibility: observability.VisibilityDebug},
		},
		IdempotencyKey: req.IdempotencyKey, IdempotencyScope: sessionScope, RequestHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return nil, err
	}
	turn = &Turn{ID: commit.Run.TurnID, SessionID: commit.Session.ID, CreatedAt: commit.Message.CreatedAt}
	result := &OpenTurnResult{Session: commit.Session, Run: commit.Run, Turn: turn, UserMessage: commit.Message, Idempotent: commit.Idempotent, ContextFragments: req.ContextFragments, ScopedData: req.ScopedData}
	if commit.Idempotent {
		result.ContextSnapshotRef = commit.Run.ContextSnapshotRef
		return result, nil
	}
	run, msg = commit.Run, commit.Message

	// 4-5. context snapshot (D5): generated before dispatch.
	if s.Snapshot != nil && !req.DeferContextSnapshot {
		if err := s.BuildContextSnapshot(ctx, result, req.ContextAttachments); err != nil {
			return result, err
		}
	}

	// 6. agent binding
	if req.AgentBindingID != "" {
		if err := s.Stores.Runs.BindAgentBinding(ctx, run.RunID, req.AgentBindingID); err != nil {
			return result, err
		}
		run.AgentBindingID = req.AgentBindingID
		if _, err := s.append(ctx, run, observability.EventAgentBinding, observability.VisibilityDebug, nil); err != nil {
			return result, err
		}
	}

	return result, nil
}

// BuildContextSnapshot freezes and binds the model context for a created run.
// Agent Chat uses this after persisting attachments so the snapshot can carry
// artifact_refs as first-class facts while ordinary OpenTurn keeps the classic
// build-before-dispatch ordering.
func (s *RunService) BuildContextSnapshot(ctx context.Context, result *OpenTurnResult, attachments []ContextAttachmentSnapshot) error {
	if s == nil || s.Snapshot == nil || result == nil || result.Run == nil || result.Session == nil || result.Turn == nil || result.UserMessage == nil {
		return nil
	}
	run := result.Run
	if result.ContextSnapshotRef != "" || run.ContextSnapshotRef != "" {
		return nil
	}
	ref, err := s.Snapshot.Build(ctx, SnapshotBuildRequest{
		TenantID: run.TenantID, SessionID: result.Session.ID, TurnID: result.Turn.ID, RunID: run.RunID,
		MessageID: result.UserMessage.ID, AgentID: run.AgentID, TraceID: run.TraceID,
		Attachments: attachments,
	})
	if err != nil {
		// fact-first: run record already queryable; fail closed on build error.
		_, _ = s.appendErr(ctx, run, observability.EventContextBuildFailed, err)
		_, _ = s.Stores.Runs.CompareAndSetStatus(ctx, run.RunID, RunStatusCreated, RunStatusFailed, RunMutation{
			ErrorCode: string(observability.EventContextBuildFailed), ErrorMessage: err.Error(),
		})
		return err
	}
	if err := s.Stores.Runs.BindContextSnapshot(ctx, run.RunID, ref); err != nil {
		return err
	}
	run.ContextSnapshotRef = ref
	result.ContextSnapshotRef = ref
	_, err = s.append(ctx, run, observability.EventContextSnapshotCreated, observability.VisibilityDebug, nil)
	return err
}

// FailUndispatchedRun 收敛“事实已创建但执行入口未取得所有权”的失败。
// 先用 created->failed CAS 确认 Run 仍未开始，再追加唯一终态事件；不启动补偿协程。
func (s *RunService) FailUndispatchedRun(ctx context.Context, run *Run, cause error) (AppendResult, error) {
	if run == nil || run.RunID == "" || cause == nil {
		return AppendResult{}, NewError(ErrInvalidArgument, "run and dispatch failure are required")
	}
	failed, err := s.Stores.Runs.CompareAndSetStatus(ctx, run.RunID, RunStatusCreated, RunStatusFailed, RunMutation{
		ErrorCode: "dispatch_failed", ErrorMessage: cause.Error(),
	})
	if err != nil {
		return AppendResult{}, err
	}
	trace := observability.MustTraceContext(ctx)
	return s.Stores.Events.Append(ctx, observability.AgentEvent{
		SessionID: failed.SessionID, RunID: failed.RunID, AgentID: failed.AgentID,
		TraceID: failed.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID,
		EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug,
		IdempotencyKey: failed.RunID + ":dispatch_failed",
		Error: &observability.EventError{
			Code: "dispatch_failed", Type: "internal_error", Message: cause.Error(),
		},
	})
}

func (s *RunService) append(ctx context.Context, run *Run, t observability.EventType, vis observability.EventVisibility, payload any) (AppendResult, error) {
	var raw json.RawMessage
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	trace := observability.MustTraceContext(ctx)
	return s.Stores.Events.Append(ctx, observability.AgentEvent{
		SessionID: run.SessionID, RunID: run.RunID, AgentID: run.AgentID,
		TraceID: run.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID,
		EventType: t, Visibility: vis, PayloadPreview: raw,
	})
}

func (s *RunService) appendErr(ctx context.Context, run *Run, t observability.EventType, cause error) (AppendResult, error) {
	trace := observability.MustTraceContext(ctx)
	return s.Stores.Events.Append(ctx, observability.AgentEvent{
		SessionID: run.SessionID, RunID: run.RunID, AgentID: run.AgentID,
		TraceID: run.TraceID, SpanID: trace.SpanID, ParentSpanID: trace.ParentSpanID,
		EventType: t, Visibility: observability.VisibilityDebug,
		Error: &observability.EventError{Code: string(t), Type: "internal_error", Message: cause.Error()},
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func newULID() string { return ulid.Make().String() }

func titleFromUserInput(preview, ref string) string {
	// 多 Part envelope 正文先解码取压扁预览，避免会话标题出现 JSON。
	if _, flattened, ok := contextpkg.DecodePartsEnvelope(preview); ok {
		preview = flattened
	}
	title := firstNonEmpty(preview, ref)
	const maxTitleRunes = 60
	runes := []rune(title)
	if len(runes) > maxTitleRunes {
		return string(runes[:maxTitleRunes])
	}
	return title
}

func messagePayload(msg *Message) map[string]string {
	payload := map[string]string{
		"message_id": msg.ID,
		"role":       msg.Role,
	}
	if msg.ContentPreview != "" {
		previewText := msg.ContentPreview
		// 事件 text 字段面向终端用户，envelope 正文解码为压扁预览。
		if _, flattened, ok := contextpkg.DecodePartsEnvelope(previewText); ok {
			previewText = flattened
		}
		payload["content_preview"] = previewText
		payload["text"] = previewText
	}
	if msg.ContentRef != "" {
		payload["content_ref"] = msg.ContentRef
	}
	return payload
}
