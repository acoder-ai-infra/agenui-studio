package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// Resumer triggers Runtime.Resume for an answered control request. It is a port
// injected by the dispatcher/runtime layer so control does not import
// agentruntime. May be nil in P0 skeletons that only persist the interaction.
type Resumer interface {
	Resume(ctx context.Context, req ResumeRequest) error
}

// ResumeRequest carries the identifiers needed to resume the paused run.
type ResumeRequest struct {
	RunID        string
	CheckpointID string
	RequestID    string
	ResumeToken  string
	ResponseRef  string
	// Response 是内联答复 payload（小 JSON）；ResponseRef 非空时以 ref 为准。
	Response []byte
}

// Service coordinates the ControlRequest lifecycle over storage.
type Service struct {
	Stores  storage.Stores
	Resumer Resumer // may be nil
	IDs     observability.IDGenerator
	// Hash hashes a resume token before storage; defaults to SHA-256.
	Hash func(token string) string
}

// New builds a control Service with default hashing and id generation.
func New(stores storage.Stores, resumer Resumer) *Service {
	return &Service{Stores: stores, Resumer: resumer, IDs: observability.NewULIDGenerator(""), Hash: sha256Hex}
}

func sha256Hex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) hash(token string) string {
	if s.Hash != nil {
		return s.Hash(token)
	}
	return sha256Hex(token)
}

// CreateRequest describes a new control request. CheckpointID must reference an
// already-persisted checkpoint (fail closed otherwise, execution §3 / T-002).
type CreateRequest struct {
	RequestID     string
	RunID         string
	Type          string // ask_user|permission_request|hitl_review|mcp_elicitation
	CheckpointID  string
	ToolUseID     string
	PromptPreview string
	ResumeToken   string // plaintext; only the hash is stored
	TTL           time.Duration
	Event         observability.AgentEvent
}

// Create persists a pending control request, emits control_request_created, and
// moves the run running->waiting_control. If the checkpoint is not persisted, it
// fails closed and does not enter waiting_control.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*storage.ControlRequest, error) {
	if req.RunID == "" || req.Type == "" {
		return nil, errors.New("run_id and type required")
	}
	if req.CheckpointID == "" {
		return nil, errors.New("checkpoint_id required before waiting_control")
	}
	if req.ResumeToken == "" {
		return nil, errors.New("resume_token required before waiting_control")
	}
	run, err := s.Stores.Runs.Get(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	// platform child run 与父 Run 共用同一条 control 创建链路：child 的
	// control 事实建在 child run 上，仅被 Agent Gateway 内部消费，最终由
	// 父 Run 以 proposal 形式向用户呈现（方案 §6.5）。
	checkpoint, err := s.Stores.Checkpoints.Get(ctx, req.CheckpointID)
	if err != nil {
		return nil, err // checkpoint must be persisted first (fail closed)
	}
	if checkpoint.RunID != req.RunID {
		return nil, storage.NewError(storage.ErrConflict, "checkpoint does not belong to run")
	}

	requestID := req.RequestID
	if requestID == "" {
		requestID = "ctrl_" + ulid.Make().String()
	}
	cr := &storage.ControlRequest{
		RequestID:     requestID,
		RunID:         req.RunID,
		TenantID:      run.TenantID,
		CheckpointID:  req.CheckpointID,
		Type:          req.Type,
		Status:        string(StatusPending),
		ToolUseID:     req.ToolUseID,
		PromptPreview: req.PromptPreview,
		SchemaVersion: storage.ControlRequestSchemaVersion,
	}
	cr.ResumeTokenHash = s.hash(req.ResumeToken)
	if req.TTL != 0 {
		cr.ExpiresAt = time.Now().Add(req.TTL)
	}
	event := s.controlRequestCreatedEvent(ctx, req.Event, cr)
	if s.Stores.Resumes != nil {
		if err := s.Stores.Resumes.Wait(ctx, storage.ResumeWaitCommand{Control: cr, Event: event}); err != nil {
			return nil, err
		}
		return cr, nil
	}
	if err := s.Stores.Controls.Create(ctx, cr); err != nil {
		return nil, err
	}
	if _, err := s.Stores.Events.Append(ctx, event); err != nil {
		return nil, err
	}
	// running -> waiting_control (must be bound to pending request + checkpoint).
	if _, err := s.Stores.Runs.CompareAndSetStatus(ctx, req.RunID, storage.RunStatusRunning, storage.RunStatusWaitingControl, storage.RunMutation{}); err != nil {
		return nil, err
	}
	return cr, nil
}

func (s *Service) controlRequestCreatedEvent(ctx context.Context, event observability.AgentEvent, cr *storage.ControlRequest) observability.AgentEvent {
	if event.EventID == "" {
		event.EventID = s.ids().NewEventID()
	}
	if event.RunID == "" {
		event.RunID = cr.RunID
	}
	if event.EventType == "" {
		event.EventType = observability.EventControlRequestCreated
	}
	if event.Visibility == "" {
		event.Visibility = observability.VisibilityUserVisible
	}
	if event.TraceID == "" {
		event.TraceID = observability.MustTraceContext(ctx).TraceID
	}
	if event.SchemaVersion == "" {
		event.SchemaVersion = observability.AgentEventSchemaVersion
	}
	if event.IdempotencyKey == "" {
		event.IdempotencyKey = cr.RequestID + ":created"
	}
	return event
}

func (s *Service) ids() observability.IDGenerator {
	if s.IDs != nil {
		return s.IDs
	}
	s.IDs = observability.NewULIDGenerator("")
	return s.IDs
}

// AnswerRequest submits a control response.
type AnswerRequest struct {
	RequestID   string
	ResumeToken string
	ResponseRef string
}

// Answer records the response idempotently and asks Runtime to claim the
// resumable run. Control 不预写 resuming：owner/event 必须由 Runtime 的
// ResumeStore 在同一事务中领取，避免进程在“改状态、调 Runtime”之间崩溃后卡死。
func (s *Service) Answer(ctx context.Context, req AnswerRequest) (*storage.ControlRequest, error) {
	cr, err := s.Stores.Controls.Get(ctx, req.RequestID)
	if err != nil {
		return nil, err
	}
	// run 必须存在（fail-closed）；platform child run 的 control 答复由
	// Agent Gateway 凭 child 票据经 canonical Resume 链发起（方案 §6.5），
	// 与父 Run 走同一条 Answer 路径。
	if _, err := s.Stores.Runs.Get(ctx, cr.RunID); err != nil {
		return nil, err
	}
	if cr.Status != string(StatusPending) && cr.Status != string(StatusAnswered) {
		return nil, storage.NewError(storage.ErrConflict, "control request not pending: "+cr.Status)
	}
	if cr.Status == string(StatusPending) && !cr.ExpiresAt.IsZero() && !time.Now().Before(cr.ExpiresAt) {
		return nil, storage.NewError(storage.ErrConflict, "control request expired")
	}
	if cr.ResumeTokenHash != "" && s.hash(req.ResumeToken) != cr.ResumeTokenHash {
		return nil, storage.NewError(storage.ErrPermissionDenied, "invalid resume token")
	}

	answered := cr
	if cr.Status == string(StatusPending) {
		answered, err = s.Stores.Controls.CompareAndAnswer(ctx, req.RequestID, string(StatusPending), string(StatusAnswered), req.ResponseRef)
		if err != nil {
			if storage.IsErrorCode(err, storage.ErrCASMismatch) {
				// 并发回答的 CAS loser 继续承担 Resume 投递：winner 可能已在
				// 回答落库后崩溃，不能在这里提前返回并留下 waiting_control。
				answered, err = s.Stores.Controls.Get(ctx, req.RequestID)
				if err == nil && answered.Status != string(StatusAnswered) {
					err = storage.NewError(storage.ErrConflict, "control request is not answered: "+answered.Status)
				}
				if err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
	}

	if _, err := s.emitOnce(ctx, cr.RunID, observability.EventControlResponseReceived, cr.RequestID+":response"); err != nil {
		return nil, err
	}
	run, err := s.Stores.Runs.Get(ctx, cr.RunID)
	if err != nil {
		return nil, err
	}
	if run.Status != storage.RunStatusWaitingControl && run.Status != storage.RunStatusResuming {
		// 已经恢复成功的重复回答按幂等成功处理；终态也不能倒退回 resuming。
		if run.Status == storage.RunStatusRunning || run.Status.IsTerminal() {
			return answered, nil
		}
		return nil, storage.NewError(storage.ErrConflict, "run is not resumable: "+string(run.Status))
	}
	// Answered 请求会在每次重试时重新触发幂等 Resume：waiting_control 可首次
	// claim，resuming 则返回已被 owner 领取。真正只执行一次由 Runtime
	// ResumeStore 的 attempt fencing 保证，而不是 Control 的易丢本地标记。
	if s.Resumer != nil {
		if err := s.Resumer.Resume(ctx, ResumeRequest{RunID: cr.RunID, CheckpointID: cr.CheckpointID, RequestID: cr.RequestID, ResumeToken: req.ResumeToken, ResponseRef: answered.ResponseRef}); err != nil {
			return answered, err
		}
	}
	return answered, nil
}

// Expire moves all pending requests past their deadline to expired, emits
// control_request_expired, and drives each owning run to expired (C-002).
func (s *Service) Expire(ctx context.Context, now time.Time) (int, error) {
	expired, err := s.Stores.Controls.ExpirePending(ctx, now)
	if err != nil {
		return 0, err
	}
	for _, cr := range expired {
		if _, err := s.emit(ctx, cr.RunID, observability.EventControlRequestExpired); err != nil {
			return len(expired), err
		}
		// waiting_control -> expired; ignore CAS mismatch if run already moved.
		_, _ = s.Stores.Runs.CompareAndSetStatus(ctx, cr.RunID, storage.RunStatusWaitingControl, storage.RunStatusExpired, storage.RunMutation{})
	}
	return len(expired), nil
}

func (s *Service) emit(ctx context.Context, runID string, t observability.EventType) (storage.AppendResult, error) {
	return s.emitOnce(ctx, runID, t, "")
}

func (s *Service) emitOnce(ctx context.Context, runID string, t observability.EventType, key string) (storage.AppendResult, error) {
	return s.Stores.Events.Append(ctx, observability.AgentEvent{
		RunID:          runID,
		TraceID:        observability.MustTraceContext(ctx).TraceID,
		EventType:      t,
		Visibility:     observability.VisibilityUserVisible,
		IdempotencyKey: key,
	})
}
