package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const embeddedEventBuffer = 256
const embeddedMaxInlineControlResponseBytes = 256 << 10

type embeddedRunDispatcher interface {
	Dispatch(context.Context, *storage.OpenTurnResult) error
	Cancel(context.Context, string, string, string) error
}

// EmbeddedRunRequest is the application-level input used by the public SDK.
// It intentionally follows the same fact-first OpenTurn path as HTTP.
type EmbeddedRunRequest struct {
	TenantID       string
	UserID         string
	SessionID      string
	AgentID        string
	Channel        string
	UserContent    string
	UserContentRef string
	IdempotencyKey string
}

type EmbeddedCancelRequest struct {
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
	Reason    string
}

// EmbeddedControlResponseRequest answers one suspended Run without exposing
// the Runtime resume credential. ControlTicket is the actor- and request-bound
// credential emitted with control_request_created.
type EmbeddedControlResponseRequest struct {
	TenantID      string
	UserID        string
	SessionID     string
	RunID         string
	RequestID     string
	ControlTicket string
	ClientEventID string
	Decision      string
	Value         json.RawMessage
	Targets       map[string]any
	ResponseText  string
	ResponseRef   string
}

type EmbeddedControlResponse struct {
	RequestID string
	Status    string
}

type EmbeddedRun struct {
	TenantID  string
	UserID    string
	SessionID string
	RunID     string
	TurnID    string
	Events    <-chan observability.AgentEvent

	closeOnce sync.Once
	close     func()
}

// Close stops this subscriber. It does not cancel the underlying Run; callers
// must use App.Cancel for that explicit state transition.
func (r *EmbeddedRun) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		if r.close != nil {
			r.close()
		}
	})
}

type embeddedEngine struct {
	runs       *storage.RunService
	stores     storage.Stores
	dispatcher embeddedRunDispatcher
	broker     protocol.EventBroker
	control    *control.Service
	tickets    *controlticket.Codec
	artifacts  artifact.ArtifactStore
	ids        observability.IDGenerator
	ctx        context.Context
	cancel     context.CancelFunc
	closeOnce  sync.Once
}

func newEmbeddedEngine(
	runs *storage.RunService,
	stores storage.Stores,
	dispatcher embeddedRunDispatcher,
	broker protocol.EventBroker,
	controlService *control.Service,
	tickets *controlticket.Codec,
	artifacts artifact.ArtifactStore,
) *embeddedEngine {
	ctx, cancel := context.WithCancel(context.Background())
	return &embeddedEngine{
		runs: runs, stores: stores, dispatcher: dispatcher, broker: broker,
		control: controlService, tickets: tickets, artifacts: artifacts,
		ids: observability.NewULIDGenerator("sdk"), ctx: ctx, cancel: cancel,
	}
}

func (e *embeddedEngine) Close() {
	if e == nil {
		return
	}
	e.closeOnce.Do(e.cancel)
}

func (a *App) Run(ctx context.Context, req EmbeddedRunRequest) (*EmbeddedRun, error) {
	if a == nil || a.embedded == nil {
		return nil, errors.New("embedded harness engine is unavailable")
	}
	return a.embedded.Run(ctx, req)
}

func (a *App) Cancel(ctx context.Context, req EmbeddedCancelRequest) error {
	if a == nil || a.embedded == nil {
		return errors.New("embedded harness engine is unavailable")
	}
	return a.embedded.Cancel(ctx, req)
}

func (a *App) AnswerControl(ctx context.Context, req EmbeddedControlResponseRequest) (*EmbeddedControlResponse, error) {
	if a == nil || a.embedded == nil {
		return nil, errors.New("embedded harness engine is unavailable")
	}
	return a.embedded.AnswerControl(ctx, req)
}

func (e *embeddedEngine) Run(ctx context.Context, req EmbeddedRunRequest) (*EmbeddedRun, error) {
	if err := validateEmbeddedRunRequest(req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	trace := observability.MustTraceContext(ctx)
	if trace.TraceID == "" {
		trace.TraceID = e.ids.NewTraceID()
	}
	trace.RequestID = e.ids.NewRequestID()
	trace.TenantID, trace.UserID = req.TenantID, req.UserID
	trace.SessionID, trace.AgentID = req.SessionID, req.AgentID
	trace.Channel, trace.Protocol, trace.Source = req.Channel, "sdk", "embedded"
	ctx = observability.WithTraceContext(ctx, trace)

	turn, err := e.runs.OpenTurn(ctx, storage.OpenTurnRequest{
		SessionID: req.SessionID, TenantID: req.TenantID, UserID: req.UserID,
		AgentID: req.AgentID, Channel: req.Channel,
		UserContentPreview: req.UserContent, UserContentRef: req.UserContentRef,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	stream, err := e.subscribe(turn)
	if err != nil {
		return nil, err
	}
	if turn.Idempotent {
		if turn.Run.Status == storage.RunStatusCreated {
			stream.Close()
			return nil, errors.New("idempotent embedded run remains created without a dispatch owner")
		}
		return stream, nil
	}
	if err := e.dispatcher.Dispatch(ctx, turn); err != nil {
		stream.Close()
		if terminal, terminalErr := e.runs.FailUndispatchedRun(ctx, turn.Run, err); terminalErr == nil && e.broker != nil {
			_ = e.broker.Publish(ctx, terminal.Event)
		}
		return nil, err
	}
	return stream, nil
}

func validateEmbeddedRunRequest(req EmbeddedRunRequest) error {
	if req.TenantID == "" || req.UserID == "" {
		return errors.New("embedded run requires tenant_id and user_id")
	}
	if req.UserContent == "" && req.UserContentRef == "" {
		return errors.New("embedded run requires user content or content_ref")
	}
	return identifiercontract.Validate(
		identifiercontract.TenantID(req.TenantID),
		identifiercontract.UserID(req.UserID),
		identifiercontract.SessionID(req.SessionID),
		identifiercontract.AgentID(req.AgentID),
		identifiercontract.IdempotencyKey(req.IdempotencyKey),
	)
}

func (e *embeddedEngine) subscribe(turn *storage.OpenTurnResult) (*EmbeddedRun, error) {
	if turn == nil || turn.Session == nil || turn.Run == nil || turn.Turn == nil {
		return nil, errors.New("embedded run has incomplete OpenTurn facts")
	}
	streamCtx, cancel := context.WithCancel(e.ctx)
	live, cancelLive, err := e.broker.Subscribe(streamCtx, turn.Run.RunID)
	if err != nil {
		cancel()
		return nil, err
	}
	replayed, err := e.stores.Events.Query(streamCtx, storage.EventQuery{RunID: turn.Run.RunID, Limit: 500})
	if err != nil {
		cancelLive()
		cancel()
		return nil, err
	}
	closeStream := func() {
		cancelLive()
		cancel()
	}
	out := make(chan observability.AgentEvent, embeddedEventBuffer)
	go e.replayThenSubscribe(streamCtx, turn.Run.RunID, replayed, live, out, closeStream)
	return &EmbeddedRun{
		TenantID: turn.Run.TenantID, UserID: turn.Session.UserID,
		SessionID: turn.Session.ID, RunID: turn.Run.RunID, TurnID: turn.Turn.ID,
		Events: out, close: closeStream,
	}, nil
}

func (e *embeddedEngine) replayThenSubscribe(
	ctx context.Context,
	runID string,
	replayed []observability.AgentEvent,
	live <-chan observability.AgentEvent,
	out chan<- observability.AgentEvent,
	closeStream func(),
) {
	defer close(out)
	defer closeStream()
	seen := make(map[string]struct{})
	var latency firstDeltaLatencyTracker
	for _, event := range replayed {
		seen[event.EventID] = struct{}{}
		latency.observe(ctx, event)
		if !sendEmbeddedEvent(ctx, out, event) || embeddedTerminal(event.EventType) {
			return
		}
	}
	caughtUp := false
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-live:
			if !ok {
				return
			}
			// Binding and other pre-runtime facts are persisted by the Orchestrator
			// before Runtime starts, but are not part of the Runtime event channel.
			// Catch them up immediately before the first live event so the SDK sees
			// the complete canonical stream without moving protocol logic into it.
			if !caughtUp {
				if err := e.catchUpBeforeLive(ctx, runID, event, seen, out); err != nil {
					return
				}
				caughtUp = true
			}
			if _, duplicate := seen[event.EventID]; duplicate {
				continue
			}
			seen[event.EventID] = struct{}{}
			latency.observe(ctx, event)
			if !sendEmbeddedEvent(ctx, out, event) || embeddedTerminal(event.EventType) {
				return
			}
		}
	}
}

func (e *embeddedEngine) catchUpBeforeLive(
	ctx context.Context,
	runID string,
	firstLive observability.AgentEvent,
	seen map[string]struct{},
	out chan<- observability.AgentEvent,
) error {
	if len(seen) == 0 {
		return nil
	}
	events, err := e.stores.Events.Query(ctx, storage.EventQuery{RunID: runID, Limit: 500})
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.EventID == firstLive.EventID {
			break
		}
		if !firstLive.CreatedAt.IsZero() && event.CreatedAt.After(firstLive.CreatedAt) {
			break
		}
		if _, duplicate := seen[event.EventID]; duplicate {
			continue
		}
		seen[event.EventID] = struct{}{}
		if !sendEmbeddedEvent(ctx, out, event) {
			return ctx.Err()
		}
	}
	return nil
}

func sendEmbeddedEvent(ctx context.Context, out chan<- observability.AgentEvent, event observability.AgentEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- event:
		return true
	}
}

func embeddedTerminal(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventRunCompleted, observability.EventRunFailed,
		observability.EventRunCancelled, observability.EventRunExpired:
		return true
	default:
		return false
	}
}

func (e *embeddedEngine) Cancel(ctx context.Context, req EmbeddedCancelRequest) error {
	if req.TenantID == "" || req.UserID == "" || req.SessionID == "" || req.RunID == "" {
		return errors.New("embedded cancel requires tenant_id, user_id, session_id and run_id")
	}
	if err := identifiercontract.Validate(
		identifiercontract.TenantID(req.TenantID),
		identifiercontract.UserID(req.UserID),
		identifiercontract.SessionID(req.SessionID),
		identifiercontract.RunID(req.RunID),
	); err != nil {
		return err
	}
	trace := observability.MustTraceContext(ctx)
	if trace.TraceID == "" {
		trace.TraceID = e.ids.NewTraceID()
	}
	trace.TenantID, trace.UserID = req.TenantID, req.UserID
	trace.SessionID, trace.RunID, trace.Protocol, trace.Source = req.SessionID, req.RunID, "sdk", "embedded"
	ctx = observability.WithTraceContext(ctx, trace)
	session, err := e.stores.Sessions.Get(ctx, req.SessionID)
	if err != nil {
		return err
	}
	if session.TenantID != "" && session.TenantID != req.TenantID {
		return storage.NewError(storage.ErrPermissionDenied, "session belongs to another tenant")
	}
	if session.UserID != "" && session.UserID != req.UserID {
		return storage.NewError(storage.ErrPermissionDenied, "session is owned by another user")
	}
	run, err := e.stores.Runs.Get(ctx, req.RunID)
	if err != nil {
		return err
	}
	if run.SessionID != req.SessionID {
		return storage.NewError(storage.ErrInvalidArgument, fmt.Sprintf("run %s does not belong to session %s", req.RunID, req.SessionID))
	}
	if run.TenantID != "" && run.TenantID != req.TenantID {
		return storage.NewError(storage.ErrPermissionDenied, "run belongs to another tenant")
	}
	// A6：记录 Cancel 受理到框架完成取消派发的延迟（run_cancel_latency）。
	cancelStart := time.Now()
	err = e.dispatcher.Cancel(ctx, req.SessionID, req.RunID, req.Reason)
	logRunCancelLatency(ctx, req.RunID, run.AgentID, cancelStart)
	return err
}

func (e *embeddedEngine) AnswerControl(ctx context.Context, req EmbeddedControlResponseRequest) (*EmbeddedControlResponse, error) {
	if err := validateEmbeddedControlResponseRequest(req); err != nil {
		return nil, err
	}
	if e.control == nil || e.tickets == nil {
		return nil, errors.New("embedded control response is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	trace := observability.MustTraceContext(ctx)
	if trace.TraceID == "" {
		trace.TraceID = e.ids.NewTraceID()
	}
	trace.RequestID = e.ids.NewRequestID()
	trace.TenantID, trace.UserID = req.TenantID, req.UserID
	trace.SessionID, trace.RunID, trace.Protocol, trace.Source = req.SessionID, req.RunID, "sdk", "embedded"
	ctx = observability.WithTraceContext(ctx, trace)

	session, err := e.stores.Sessions.Get(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	if session.TenantID != req.TenantID || session.UserID != req.UserID {
		return nil, storage.NewError(storage.ErrPermissionDenied, "session is owned by another actor")
	}
	run, err := e.stores.Runs.Get(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	if run.SessionID != req.SessionID {
		return nil, storage.NewError(storage.ErrInvalidArgument, "run does not belong to session")
	}
	if run.TenantID != req.TenantID {
		return nil, storage.NewError(storage.ErrPermissionDenied, "run belongs to another tenant")
	}
	request, err := e.stores.Controls.Get(ctx, req.RequestID)
	if err != nil {
		return nil, err
	}
	if request.RunID != req.RunID || request.TenantID != req.TenantID {
		return nil, storage.NewError(storage.ErrPermissionDenied, "control request belongs to another run")
	}
	claims, expiresAt, err := e.tickets.Open(req.ControlTicket)
	if err != nil || !time.Now().Before(expiresAt) ||
		claims.TenantID != req.TenantID || claims.UserID != req.UserID ||
		claims.SessionID != req.SessionID || claims.RunID != req.RunID || claims.RequestID != req.RequestID {
		return nil, storage.NewError(storage.ErrPermissionDenied, "control ticket is invalid or expired")
	}

	responseRef := request.ResponseRef
	if request.Status == string(control.StatusPending) {
		responseRef, err = e.persistEmbeddedControlResponse(ctx, run, req)
		if err != nil {
			return nil, err
		}
	}
	answered, err := e.control.Answer(ctx, control.AnswerRequest{
		RequestID: req.RequestID, ResumeToken: claims.ResumeToken, ResponseRef: responseRef,
	})
	if err != nil {
		return nil, err
	}
	return &EmbeddedControlResponse{RequestID: answered.RequestID, Status: answered.Status}, nil
}

func validateEmbeddedControlResponseRequest(req EmbeddedControlResponseRequest) error {
	if req.TenantID == "" || req.UserID == "" || req.SessionID == "" || req.RunID == "" || req.RequestID == "" {
		return errors.New("embedded control response requires tenant_id, user_id, session_id, run_id and request_id")
	}
	if req.ControlTicket == "" {
		return errors.New("embedded control response requires control_ticket")
	}
	return identifiercontract.Validate(
		identifiercontract.TenantID(req.TenantID),
		identifiercontract.UserID(req.UserID),
		identifiercontract.SessionID(req.SessionID),
		identifiercontract.RunID(req.RunID),
		identifiercontract.RequestID(req.RequestID),
		identifiercontract.ClientEventID(req.ClientEventID),
	)
}

func (e *embeddedEngine) persistEmbeddedControlResponse(
	ctx context.Context,
	run *storage.Run,
	req EmbeddedControlResponseRequest,
) (string, error) {
	if req.ResponseRef != "" {
		if req.Decision != "" || len(req.Value) > 0 || len(req.Targets) > 0 || strings.TrimSpace(req.ResponseText) != "" {
			return "", errors.New("response_ref cannot be combined with inline decision, value, targets, or response_text")
		}
		if e.artifacts == nil {
			return "", errors.New("artifact store is required for response_ref")
		}
		object, err := e.artifacts.Get(embeddedControlArtifactContext(ctx, run), req.ResponseRef, artifact.GetOptions{Purpose: artifact.PurposeReplay})
		if err != nil {
			return "", err
		}
		if closeErr := object.Content.Close(); closeErr != nil {
			return "", closeErr
		}
		if object.Meta.ArtifactType != artifact.ArtifactTypeControlResponse {
			return "", errors.New("response_ref must reference a control_response artifact")
		}
		return req.ResponseRef, nil
	}
	responseText := strings.TrimSpace(req.ResponseText)
	if req.Decision == "" && len(req.Value) == 0 && len(req.Targets) == 0 && responseText == "" {
		return "", nil
	}
	if e.artifacts == nil {
		return "", errors.New("artifact store is required for inline control response")
	}
	payload, err := json.Marshal(struct {
		Decision string          `json:"decision,omitempty"`
		Value    json.RawMessage `json:"value,omitempty"`
		Targets  map[string]any  `json:"targets,omitempty"`
		Text     string          `json:"response_text,omitempty"`
	}{Decision: req.Decision, Value: req.Value, Targets: req.Targets, Text: responseText})
	if err != nil {
		return "", fmt.Errorf("marshal control response: %w", err)
	}
	if len(payload) > embeddedMaxInlineControlResponseBytes {
		return "", errors.New("inline control response exceeds 256 KiB")
	}
	meta, err := e.artifacts.Put(embeddedControlArtifactContext(ctx, run), artifact.PutArtifactRequest{
		TenantID: run.TenantID, SessionID: run.SessionID, RunID: run.RunID,
		OwnerModule: artifact.OwnerModuleProtocol, OwnerID: req.RequestID,
		ArtifactType: artifact.ArtifactTypeControlResponse, MimeType: "application/json",
		Visibility: artifact.VisibilityInternal, Content: bytes.NewReader(payload),
		RetentionPolicy: artifact.RetentionRunTTL, CreatedBy: "control_response",
		IdempotencyKey: fmt.Sprintf("control-response:%s:%s", req.RequestID, req.ClientEventID),
		Metadata:       map[string]string{"request_id": req.RequestID, "schema_version": "harness.control_response.v1"},
	})
	if err != nil {
		return "", err
	}
	return meta.ArtifactRef, nil
}

func embeddedControlArtifactContext(ctx context.Context, run *storage.Run) context.Context {
	return artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: run.TenantID, SessionID: run.SessionID, RunID: run.RunID, Role: artifact.ActorRuntime,
	})
}
