package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// resultLookupMessageLimit 是 GetResult 回查本 Run 最终答复时的单页上限。
// 一个 Run 在会话尾部只会产生少量 user_visible 消息（user 输入 +
// assistant 答复），取最近一页即可命中；不命中时字段留空。
const resultLookupMessageLimit = 50

// engineImpl is the private implementation of Engine. Callers only see the
// interface; every state field lives here.
type engineImpl struct {
	kernel          *kernel.Kernel
	report          BuildReport
	runDrainTimeout time.Duration

	// turns 是入口层预回合扩展管线（IdentityResolver/RunInitializer/
	// ContextContributor/InputNormalizer）的共享执行器，与 HTTP 入口复用
	// 同一份 kernel 实现；SDK 不再自带副本。
	turns *kernel.TurnPipeline

	closed atomic.Bool

	// activeRuns bounds how many concurrently-running executions the Engine
	// knows about; batch A only tracks them for Close draining.
	mu         sync.Mutex
	active     map[string]struct{}
	inProgress sync.WaitGroup
}

func newEngine(k *kernel.Kernel, report BuildReport, drainTimeout time.Duration) *engineImpl {
	var turns *kernel.TurnPipeline
	if k != nil {
		turns = kernel.NewTurnPipeline(k.Extensions, kernel.TurnEnvironment{
			Environment:    report.Environment,
			SDKVersion:     Version,
			SchemaVersions: report.SchemaVersions,
		})
	}
	return &engineImpl{
		kernel:          k,
		report:          report,
		runDrainTimeout: drainTimeout,
		turns:           turns,
		active:          make(map[string]struct{}),
	}
}

// Assert engineImpl satisfies Engine at compile time.
var _ Engine = (*engineImpl)(nil)

// Report returns the frozen BuildReport captured at Build time.
func (e *engineImpl) Report() BuildReport {
	return e.report
}

// Start opens a new Run and returns an Execution. Subscription is installed
// BEFORE dispatch so no early event is lost.
func (e *engineImpl) Start(ctx context.Context, req StartRequest) (Execution, error) {
	if e.closed.Load() {
		return nil, closedErrorf("Start", nil)
	}
	if err := validateStartRequest(req); err != nil {
		return nil, err
	}
	if e.kernel == nil || e.kernel.Runs == nil || e.kernel.RunEntry == nil || e.kernel.Broker == nil {
		return nil, fmt.Errorf("%w: kernel is missing runtime dependencies", ErrNotReady)
	}

	trace, _ := observability.TraceContextFrom(ctx)
	if trace.TraceID == "" {
		trace = observability.TraceContext{}
	}

	// 预回合扩展管线（§7.1 阶段 1-4）：IdentityResolver → RunInitializer →
	// ContextContributor → InputNormalizer。执行机制位于 kernel.TurnPipeline
	//（与 HTTP 入口共享同一治理链）；全部阶段 fail-closed：任一扩展失败
	// 则 Start 不得继续，因为下游 ACL / ScopedData / 输入确定性都依赖它们。
	req, contribFragments, err := e.prepareTurn(ctx, req)
	if err != nil {
		return nil, err
	}

	trace.TenantID = req.Identity.TenantID
	trace.UserID = req.Identity.UserID
	trace.SessionID = req.Identity.SessionID
	trace.AgentID = req.Identity.AgentID
	ctx = observability.WithTraceContext(ctx, trace)

	openReq := storage.OpenTurnRequest{
		SessionID:          req.Identity.SessionID,
		TenantID:           req.Identity.TenantID,
		UserID:             req.Identity.UserID,
		AgentID:            firstNonEmpty(req.Identity.AgentID, e.kernel.DefaultAgentID),
		Channel:            "sdk",
		Runtime:            "",
		UserContentPreview: renderUserContent(req.Input),
		IdempotencyKey:     req.IdempotencyKey,
		// ContextContributor 产出的 fragment 随 Turn 透传，最终进入 ModelContext
		// 装配（取代旧的 metadata digest 占位消费方式）。
		ContextFragments: kernel.ContextFragmentsForTurn(contribFragments),
		// prepareTurn 合并后的 Run 级 ScopedData（调用方入参 + RunInitializer
		// 产出）随 Turn 透传，由 dispatcher 填入 RunRequest.ScopedData，最终
		// 以只读冻结视图暴露给 BeforeModelHook 等运行期扩展。
		ScopedData: scopedDataToStorage(req.ScopedData.Run),
	}
	manifest := BuildInputManifest(req.Input)
	turn, err := e.kernel.Runs.OpenTurn(ctx, openReq)
	if err != nil {
		return nil, classifyStartError(err)
	}
	if turn == nil || turn.Run == nil {
		return nil, fmt.Errorf("%w: open turn returned empty result", ErrNotReady)
	}

	sub, err := e.kernel.SubscribeRun(ctx, turn.Run.RunID)
	if err != nil {
		return nil, fmt.Errorf("harness: subscribe run: %w", err)
	}

	// Register the run for Close drain BEFORE dispatch so a racing Close waits
	// for it. Dispatch itself is asynchronous; the goroutine inside
	// formalRunDispatcher.drain closes the subscription once the run reaches
	// a terminal event.
	e.registerActive(turn.Run.RunID)

	if err := e.kernel.RunEntry.Dispatch(ctx, turn); err != nil {
		sub.Close()
		e.releaseActive(turn.Run.RunID)
		return nil, fmt.Errorf("harness: dispatch: %w", err)
	}

	stream := newEventStream(sub, turn.Run.RunID, func() { e.releaseActive(turn.Run.RunID) })
	stream.validators = e.eventValidators()
	stream.projectors = e.eventProjectors()
	validation := &executionOutputValidation{}
	stream.validation = validation
	frames := &executionFrames{}
	stream.frames = frames
	handle := RunHandle{
		Identity: Identity{
			TenantID:  turn.Run.TenantID,
			UserID:    req.Identity.UserID,
			SessionID: turn.Session.ID,
			RunID:     turn.Run.RunID,
			AgentID:   turn.Run.AgentID,
		},
		AgentBindingID:    turn.Run.AgentBindingID,
		ConfigSnapshotRef: turn.Run.ConfigSnapshotRef,
		TraceID:           trace.TraceID,
		InputManifest:     manifest,
	}
	return &executionImpl{handle: handle, events: stream, validation: validation, frames: frames}, nil
}

// prepareTurn 执行 kernel 预回合扩展管线并把产出折回 StartRequest：harness
// 只做 DTO 转换，不再自己执行任何扩展阶段。返回的 fragment 列表由 Start
// 透传给 OpenTurn。
func (e *engineImpl) prepareTurn(ctx context.Context, req StartRequest) (StartRequest, []extension.ContextFragment, error) {
	if e.turns == nil || !e.turns.HasStages() {
		return req, nil, nil
	}
	prepared, err := e.turns.PrepareTurn(ctx, kernel.PrepareTurnRequest{
		Identity: kernel.TurnIdentity{
			TenantID:     req.Identity.TenantID,
			UserID:       req.Identity.UserID,
			SessionID:    req.Identity.SessionID,
			AgentID:      req.Identity.AgentID,
			AgentVersion: req.Identity.AgentVersion,
		},
		Metadata:   req.Metadata,
		Input:      messageToExtension(req.Input),
		ScopedData: scopedDataToExtension(req.ScopedData.Run),
	})
	if err != nil {
		if errors.Is(err, kernel.ErrTurnPipelineInvalid) {
			// 注册无效（类型不匹配 / panic / 空产出）映射为 SDK 的
			// ErrInvalidRequest 语义；扩展自身的业务错误保留原链透传。
			return StartRequest{}, nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		return StartRequest{}, nil, err
	}
	req.Identity.TenantID = prepared.Identity.TenantID
	req.Identity.UserID = prepared.Identity.UserID
	req.Identity.SessionID = prepared.Identity.SessionID
	if err := validateStartIdentifiers(req); err != nil {
		return StartRequest{}, nil, err
	}
	req.Input = extensionToMessage(prepared.Input)
	if len(prepared.ScopedData) > 0 {
		if req.ScopedData.Run == nil {
			req.ScopedData.Run = make(map[string]ScopedDataItem, len(prepared.ScopedData))
		}
		if err := mergeScopedDataFromExtension(req.ScopedData.Run, prepared.ScopedData); err != nil {
			return StartRequest{}, nil, err
		}
	}
	return req, prepared.Fragments, nil
}

// Resume continues a run in waiting_control.
func (e *engineImpl) Resume(ctx context.Context, req ResumeRequest) (Execution, error) {
	if e.closed.Load() {
		return nil, closedErrorf("Resume", nil)
	}
	if req.Identity.RunID == "" {
		return nil, wrapInvalidRequest("Resume requires Identity.RunID")
	}
	if req.ControlRequestID == "" || req.CheckpointID == "" || req.ControlTicket == "" {
		return nil, wrapInvalidRequest("Resume requires ControlRequestID, CheckpointID and ControlTicket")
	}
	if e.kernel == nil || e.kernel.RunEntry == nil || e.kernel.Broker == nil {
		return nil, fmt.Errorf("%w: kernel is missing runtime dependencies", ErrNotReady)
	}

	sub, err := e.kernel.SubscribeRun(ctx, req.Identity.RunID)
	if err != nil {
		return nil, fmt.Errorf("harness: subscribe run: %w", err)
	}
	e.registerActive(req.Identity.RunID)

	kernelReq := control.ResumeRequest{
		RunID:        req.Identity.RunID,
		RequestID:    req.ControlRequestID,
		CheckpointID: req.CheckpointID,
		ResumeToken:  req.ControlTicket,
		ResponseRef:  req.ResponseArtifactRef,
		// 内联答复（小 JSON）直接透传；ResponseRef 非空时 kernel 以 ref 为准。
		Response: cloneRaw(req.Response),
	}
	if err := e.kernel.RunEntry.Resume(ctx, kernelReq); err != nil {
		sub.Close()
		e.releaseActive(req.Identity.RunID)
		return nil, fmt.Errorf("harness: resume: %w", err)
	}

	// Reconstruct RunHandle from the durable run row. GetRun surfaces the
	// server-assigned values (SessionID etc.).
	handle := RunHandle{
		Identity: Identity{
			RunID: req.Identity.RunID,
		},
	}
	if run, lookupErr := e.kernel.Stores.Runs.Get(ctx, req.Identity.RunID); lookupErr == nil && run != nil {
		handle.Identity.TenantID = run.TenantID
		handle.Identity.SessionID = run.SessionID
		handle.Identity.AgentID = run.AgentID
		handle.AgentBindingID = run.AgentBindingID
		handle.ConfigSnapshotRef = run.ConfigSnapshotRef
		handle.TraceID = run.TraceID
	}
	stream := newEventStream(sub, req.Identity.RunID, func() { e.releaseActive(req.Identity.RunID) })
	// 与 Start 保持一致：Resume 流同样挂载 validator 视图填充与 projector
	// 投影（旧实现遗漏，导致 Resume 返回的 Execution 拿不到校验结果与 frame）。
	stream.validators = e.eventValidators()
	stream.projectors = e.eventProjectors()
	validation := &executionOutputValidation{}
	stream.validation = validation
	frames := &executionFrames{}
	stream.frames = frames
	return &executionImpl{handle: handle, events: stream, validation: validation, frames: frames}, nil
}

// Cancel is idempotent for terminal runs.
func (e *engineImpl) Cancel(ctx context.Context, req CancelRequest) error {
	if e.closed.Load() {
		return closedErrorf("Cancel", nil)
	}
	if req.Identity.RunID == "" {
		return wrapInvalidRequest("Cancel requires Identity.RunID")
	}
	if e.kernel == nil || e.kernel.RunEntry == nil {
		return fmt.Errorf("%w: kernel is missing runtime dependencies", ErrNotReady)
	}
	return e.kernel.RunEntry.Cancel(ctx, req.Identity.SessionID, req.Identity.RunID, req.Reason)
}

// Subscribe attaches a fresh EventStream to an existing run.
func (e *engineImpl) Subscribe(ctx context.Context, req SubscribeRequest) (EventStream, error) {
	if e.closed.Load() {
		return nil, closedErrorf("Subscribe", nil)
	}
	if req.Identity.RunID == "" {
		return nil, wrapInvalidRequest("Subscribe requires Identity.RunID")
	}
	if e.kernel == nil || e.kernel.Broker == nil {
		return nil, fmt.Errorf("%w: kernel is missing runtime dependencies", ErrNotReady)
	}
	sub, err := e.kernel.SubscribeRun(ctx, req.Identity.RunID)
	if err != nil {
		return nil, fmt.Errorf("harness: subscribe: %w", err)
	}
	after := req.AfterSequence
	if req.Cursor.AfterSequence > after {
		after = req.Cursor.AfterSequence
	}
	stream := newEventStream(sub, req.Identity.RunID, nil)
	if after > 0 {
		stream.setSkipUntil(after)
	}
	// 与 Start/Resume 保持一致（R2c/R2d）：Subscribe 流同样挂载 validator 与
	// projector。Subscribe 不返回 Execution，结果单元不对外暴露，但带状态的
	// 业务 projector/validator 实现（在自身闭包里累积 frame / 结果）依赖
	// 重连流上的同样触发。
	stream.validators = e.eventValidators()
	stream.projectors = e.eventProjectors()
	stream.validation = &executionOutputValidation{}
	stream.frames = &executionFrames{}
	return stream, nil
}

// GetRun returns the durable RunView.
func (e *engineImpl) GetRun(ctx context.Context, req GetRunRequest) (RunView, error) {
	if e.closed.Load() {
		return RunView{}, closedErrorf("GetRun", nil)
	}
	if req.Identity.RunID == "" {
		return RunView{}, wrapInvalidRequest("GetRun requires Identity.RunID")
	}
	if e.kernel == nil || e.kernel.Stores.Runs == nil {
		return RunView{}, fmt.Errorf("%w: run store missing", ErrNotReady)
	}
	run, err := e.kernel.Stores.Runs.Get(ctx, req.Identity.RunID)
	if err != nil {
		if isNotFound(err) {
			return RunView{}, errRunNotFound
		}
		return RunView{}, fmt.Errorf("harness: get run: %w", err)
	}
	if run == nil {
		return RunView{}, errRunNotFound
	}
	return RunView{
		Identity: Identity{
			TenantID:  run.TenantID,
			SessionID: run.SessionID,
			RunID:     run.RunID,
			AgentID:   run.AgentID,
		},
		Status:            RunStatus(run.Status),
		ConfigSnapshotRef: run.ConfigSnapshotRef,
		AgentBindingID:    run.AgentBindingID,
		StartedAt:         run.StartedAt,
		CompletedAt:       run.EndedAt,
		ErrorCode:         run.ErrorCode,
		ErrorMessage:      run.ErrorMessage,
	}, nil
}

// GetResult returns the run's final ResultView.
func (e *engineImpl) GetResult(ctx context.Context, req GetResultRequest) (ResultView, error) {
	if e.closed.Load() {
		return ResultView{}, closedErrorf("GetResult", nil)
	}
	if req.Identity.RunID == "" {
		return ResultView{}, wrapInvalidRequest("GetResult requires Identity.RunID")
	}
	view, err := e.GetRun(ctx, GetRunRequest{Identity: req.Identity})
	if err != nil {
		return ResultView{}, err
	}
	if !view.Status.IsTerminal() {
		return ResultView{}, fmt.Errorf("%w: run %s is not terminal", ErrNotReady, req.Identity.RunID)
	}
	result := ResultView{
		RunID:     view.Identity.RunID,
		SessionID: view.Identity.SessionID,
	}
	// 从 Message Store 读本 Run 的 assistant 最终答复（权威事实）填充
	// Content / ContentRef / MessageID：事件流的 payload preview 会在大
	// 结果溢出时被截断，不能作为结果来源。失败终态的 Run 无产物，
	// 字段留空不报错。
	if err := e.fillResultContent(ctx, &result, view.Identity.SessionID); err != nil {
		return ResultView{}, err
	}
	return result, nil
}

// fillResultContent 把本 Run 最后一条 assistant 消息投影进 ResultView。
// 按 session 倒序翻页定位（存储层默认只返回 user_visible，与
// ListMessages 同语义），命中本 RunID 的第一条 assistant 即最终答复。
func (e *engineImpl) fillResultContent(ctx context.Context, result *ResultView, sessionID string) error {
	if e.kernel == nil || e.kernel.Stores.Messages == nil || sessionID == "" {
		return nil
	}
	page, err := e.kernel.Stores.Messages.List(ctx, storage.MessageListQuery{
		SessionID: sessionID,
		Limit:     resultLookupMessageLimit,
	})
	if err != nil {
		return fmt.Errorf("harness: get result: %w", err)
	}
	for i := len(page.Items) - 1; i >= 0; i-- {
		msg := page.Items[i]
		if msg == nil || msg.RunID != result.RunID || msg.Role != "assistant" {
			continue
		}
		result.MessageID = msg.ID
		result.Content = msg.ContentPreview
		result.ContentRef = msg.ContentRef
		result.CreatedAt = msg.CreatedAt
		return nil
	}
	return nil
}

// Readiness returns a live ReadinessReport by probing subsystems where cheap.
func (e *engineImpl) Readiness(ctx context.Context) (ReadinessReport, error) {
	if e.closed.Load() {
		return ReadinessReport{}, closedErrorf("Readiness", nil)
	}
	report := ReadinessReport{
		Ready:          true,
		SchemaVersions: append([]string(nil), e.report.SchemaVersions...),
		Runtimes:       append([]RuntimeInfo(nil), e.report.Runtimes...),
		Extensions:     append([]ExtensionInfo(nil), e.report.Extensions...),
		CheckedAt:      time.Now(),
	}
	if e.kernel == nil || e.kernel.Runs == nil || e.kernel.RunEntry == nil || e.kernel.Broker == nil {
		report.Ready = false
		report.Reasons = append(report.Reasons, "kernel runtime dependencies missing")
	}
	return report, nil
}

// Close first marks the engine closed, drains active runs bounded by
// runDrainTimeout / ctx, then delegates to app.Close for managed-resource
// release.
func (e *engineImpl) Close(ctx context.Context) error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}

	// drain: wait for active executions to release, bounded by ctx and the
	// configured runDrainTimeout.
	drainCtx := ctx
	if drainCtx == nil {
		drainCtx = context.Background()
	}
	deadline := time.Now().Add(e.runDrainTimeout)
	if d, ok := drainCtx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	drained := make(chan struct{})
	go func() {
		e.inProgress.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Until(deadline)):
		// timeout: proceed to resource release even if some runs are still
		// held, so hosts do not hang on shutdown.
	case <-drainCtx.Done():
	}

	if e.kernel == nil || e.kernel.Close == nil {
		return nil
	}
	return e.kernel.Close(drainCtx)
}

func (e *engineImpl) registerActive(runID string) {
	e.mu.Lock()
	if _, exists := e.active[runID]; !exists {
		e.active[runID] = struct{}{}
		e.inProgress.Add(1)
	}
	e.mu.Unlock()
}

func (e *engineImpl) releaseActive(runID string) {
	e.mu.Lock()
	if _, exists := e.active[runID]; exists {
		delete(e.active, runID)
		e.inProgress.Done()
	}
	e.mu.Unlock()
}

// executionImpl is the concrete Execution returned by Start / Resume.
type executionImpl struct {
	handle     RunHandle
	events     EventStream
	validation *executionOutputValidation
	frames     *executionFrames
}

func (x *executionImpl) Handle() RunHandle   { return x.handle }
func (x *executionImpl) Events() EventStream { return x.events }

func (x *executionImpl) OutputValidation() extension.OutputValidateResult {
	if x == nil || x.validation == nil {
		return extension.OutputValidateResult{}
	}
	return x.validation.get()
}

func (x *executionImpl) ProjectedFrames() []extension.Frame {
	if x == nil || x.frames == nil {
		return nil
	}
	return x.frames.snapshot()
}

// eventStream translates kernel.Subscription -> harness.EventStream.
type eventStream struct {
	sub        *kernel.Subscription
	runID      string
	onClose    func()
	closeOnce  sync.Once
	closed     atomic.Bool
	skipUntil  int64
	cursor     atomic.Int64
	terminalRC atomic.Bool
	validators []extensionValidatorBinding
	projectors []extensionProjectorBinding
	validation *executionOutputValidation
	frames     *executionFrames
}

func newEventStream(sub *kernel.Subscription, runID string, onClose func()) *eventStream {
	return &eventStream{sub: sub, runID: runID, onClose: onClose}
}

func (s *eventStream) setSkipUntil(seq int64) { s.skipUntil = seq }

func (s *eventStream) Next(ctx context.Context) (Event, error) {
	if s == nil || s.sub == nil {
		return Event{}, io.EOF
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ch := s.sub.Events()
	for {
		if s.closed.Load() {
			return Event{}, ErrClosed
		}
		select {
		case ev, ok := <-ch:
			if !ok {
				// Channel closed by broker: terminal reached or upstream cancelled.
				return Event{}, io.EOF
			}
			if s.skipUntil > 0 && ev.Sequence <= s.skipUntil {
				continue
			}
			s.cursor.Store(ev.Sequence)
			if isTerminalEventType(ev.EventType) {
				s.terminalRC.Store(true)
			}
			converted := convertEvent(ev)
			maybeRunValidators(ctx, s.validators, converted, s.validation)
			fanEventToProjectors(ctx, s.projectors, converted, s.frames)
			return converted, nil
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

func (s *eventStream) Close() error {
	if s == nil {
		return nil
	}
	s.closed.Store(true)
	s.closeOnce.Do(func() {
		if s.sub != nil {
			s.sub.Close()
		}
		if s.onClose != nil {
			s.onClose()
		}
	})
	return nil
}

func (s *eventStream) Cursor() Cursor {
	if s == nil {
		return Cursor{}
	}
	return Cursor{AfterSequence: s.cursor.Load(), RunID: s.runID}
}

func convertEvent(ev observability.AgentEvent) Event {
	out := Event{
		EventID:        ev.EventID,
		SchemaVersion:  ev.SchemaVersion,
		Sequence:       ev.Sequence,
		IdempotencyKey: ev.IdempotencyKey,
		TraceID:        ev.TraceID,
		SpanID:         ev.SpanID,
		ParentSpanID:   ev.ParentSpanID,
		SessionID:      ev.SessionID,
		RunID:          ev.RunID,
		ParentRunID:    ev.ParentRunID,
		StepID:         ev.StepID,
		AgentID:        ev.AgentID,
		AgentType:      ev.AgentType,
		Runtime:        ev.Runtime,
		EventType:      EventType(ev.EventType),
		Visibility:     Visibility(ev.Visibility),
		PayloadPreview: cloneRaw(ev.PayloadPreview),
		PayloadRef:     ev.PayloadRef,
		Usage:          cloneRaw(ev.Usage),
		DebugRef:       ev.DebugRef,
		CreatedAt:      ev.CreatedAt,
	}
	if ev.Error != nil {
		out.Error = &EventError{
			Code:      ev.Error.Code,
			Type:      EventErrorType(ev.Error.Type),
			Message:   ev.Error.Message,
			Retryable: ev.Error.Retryable,
		}
	}
	return out
}

func cloneRaw(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}

// isTerminalEventType 复用 canonical 终态定义（observability 包是唯一事实源），
// 避免 SDK 侧维护终态集合副本产生漂移。
func isTerminalEventType(t observability.EventType) bool {
	return observability.IsTerminalRunEventType(t)
}

// validateStartRequest enforces the DTO-level preconditions Start requires.
func validateStartRequest(req StartRequest) error {
	if req.Identity.TenantID == "" {
		return wrapInvalidRequest("Start requires Identity.TenantID")
	}
	if err := validateStartIdentifiers(req); err != nil {
		return err
	}
	if len(req.Input.Parts) == 0 {
		return wrapInvalidRequest("Start requires non-empty Input.Parts")
	}
	for i, part := range req.Input.Parts {
		if err := validatePart(part); err != nil {
			return wrapInvalidRequest(fmt.Sprintf("Input.Parts[%d]: %v", i, err))
		}
	}
	if req.Input.Role != "" && req.Input.Role != RoleUser {
		return wrapInvalidRequest("Start requires Input.Role to be user or empty")
	}
	return nil
}

func validateStartIdentifiers(req StartRequest) error {
	if err := identifiercontract.Validate(
		identifiercontract.TenantID(req.Identity.TenantID),
		identifiercontract.UserID(req.Identity.UserID),
		identifiercontract.SessionID(req.Identity.SessionID),
		identifiercontract.AgentID(req.Identity.AgentID),
		identifiercontract.AgentVersion(req.Identity.AgentVersion),
		identifiercontract.IdempotencyKey(req.IdempotencyKey),
	); err != nil {
		return wrapInvalidRequest(err.Error())
	}
	return nil
}

// validatePart enforces the Kind vs payload invariant for one Part.
func validatePart(p MessagePart) error {
	switch p.Kind {
	case PartKindText:
		if strings.TrimSpace(p.Text) == "" {
			return errors.New("text part must have Text")
		}
	case PartKindJSON:
		if len(p.JSON) == 0 {
			return errors.New("json part must have JSON payload")
		}
	case PartKindImageRef, PartKindFileRef, PartKindArtifactRef:
		if p.Ref.ID == "" {
			return errors.New("ref part must have Ref.ID")
		}
	case PartKindInlineBinary:
		if len(p.Inline) == 0 {
			return errors.New("inline_binary part must have Inline bytes")
		}
		if len(p.Inline) > MaxInlineBinaryBytes {
			return fmt.Errorf("inline_binary part exceeds %d bytes (upload large payloads as an Artifact ref)", MaxInlineBinaryBytes)
		}
	default:
		return fmt.Errorf("unknown part kind %q", p.Kind)
	}
	return nil
}

// renderUserContent 产出 OpenTurn 的用户消息正文：纯单文本输入保持裸文本
// （历史行为零变化）；含非文本 Part / 多 Part 的输入编码为 canonical parts
// envelope，由读取侧（storage 标题/事件、Context Ledger）统一解码。
func renderUserContent(msg Message) string {
	parts := contextPartsFromMessage(msg)
	if len(parts) == 0 {
		return renderPreview(msg)
	}
	return contextpkg.EncodePartsEnvelope(parts, renderPreview(msg))
}

// contextPartsFromMessage 把 SDK MessagePart 映射为 Ledger 的 ContentPart。
// 单 text Part 返回 nil（裸文本路径）；InlineBinary 承载受上限约束的小二进制
// （如视频帧，ADR-013），随 parts envelope 持久化以支持冻结输入完整性校验。
func contextPartsFromMessage(msg Message) []contextpkg.ContentPart {
	if len(msg.Parts) == 1 && msg.Parts[0].Kind == PartKindText {
		return nil
	}
	out := make([]contextpkg.ContentPart, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		converted := contextpkg.ContentPart{MIME: part.MIME, Filename: part.Filename, Hash: part.Hash}
		switch part.Kind {
		case PartKindText:
			converted.Kind, converted.Text = "text", part.Text
		case PartKindJSON:
			converted.Kind, converted.JSON = "json", cloneRaw(part.JSON)
		case PartKindImageRef:
			converted.Kind, converted.ArtifactRef = "image_ref", part.Ref.ID
		case PartKindFileRef:
			converted.Kind, converted.ArtifactRef = "file_ref", part.Ref.ID
		case PartKindArtifactRef:
			converted.Kind, converted.ArtifactRef = "artifact_ref", part.Ref.ID
		case PartKindInlineBinary:
			converted.Kind, converted.Inline = "inline_binary", append([]byte(nil), part.Inline...)
		default:
			continue
		}
		out = append(out, converted)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// renderPreview flattens ordered Parts into the stable UTF-8 preview stored by
// storage.OpenTurn. Binary content remains referenced through artifacts.
func renderPreview(msg Message) string {
	var b strings.Builder
	for _, part := range msg.Parts {
		switch part.Kind {
		case PartKindText:
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		case PartKindJSON:
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.Write(part.JSON)
		case PartKindImageRef, PartKindFileRef, PartKindArtifactRef:
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("[artifact:")
			b.WriteString(part.Ref.ID)
			b.WriteByte(']')
		case PartKindInlineBinary:
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(fmt.Sprintf("[inline:%d bytes]", len(part.Inline)))
		}
	}
	return b.String()
}

// classifyStartError converts internal storage errors into SDK-level ones so
// callers can rely on errors.Is without importing internal packages.
func classifyStartError(err error) error {
	if err == nil {
		return nil
	}
	if storage.IsErrorCode(err, storage.ErrConflict) || storage.IsErrorCode(err, storage.ErrCASMismatch) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if storage.IsErrorCode(err, storage.ErrInvalidArgument) {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if storage.IsErrorCode(err, storage.ErrUnsupportedCapability) {
		return fmt.Errorf("%w: %v", ErrUnsupportedCapability, err)
	}
	if storage.IsErrorCode(err, storage.ErrPermissionDenied) {
		return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
	}
	return err
}

func isNotFound(err error) bool {
	return storage.IsErrorCode(err, storage.ErrNotFound)
}
