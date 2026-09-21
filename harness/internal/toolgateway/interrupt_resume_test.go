package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestGatewayAllowsOnlyOneConcurrentResumeOwner(t *testing.T) {
	var resumedCalls atomic.Int32
	resumeEntered := make(chan struct{})
	releaseResume := make(chan struct{})
	gateway := newGatewayForHardening(&recordingEventStore{}, nil, NewMemoryIdempotencyStore(), nil, resumableToolDefinition(), func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
		if call.Resume == nil {
			return nil, &ToolInterruptedError{Info: "continue?", State: json.RawMessage(`{"waiting":true}`)}
		}
		if resumedCalls.Add(1) == 1 {
			close(resumeEntered)
		}
		<-releaseResume
		return &FunctionResult{Data: json.RawMessage(`{"done":true}`)}, nil
	})
	req := resumableToolRequest()
	ctx := resumableToolContext(req)
	_, err := gateway.Invoke(ctx, req)
	var interrupted *ToolInterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("interrupt error = %v", err)
	}
	resume := req
	resume.Resume = &ToolCallResume{WasInterrupted: true, IsResumeTarget: true, State: interrupted.State, Payload: json.RawMessage(`{"answer":"yes"}`)}

	firstDone := make(chan error, 1)
	go func() {
		_, invokeErr := gateway.Invoke(ctx, resume)
		firstDone <- invokeErr
	}()
	<-resumeEntered
	if _, err := gateway.Invoke(ctx, resume); !IsErrorType(err, ErrorTypeDuplicateInflight) {
		t.Fatalf("concurrent resume error = %v", err)
	}
	close(releaseResume)
	if err := <-firstDone; err != nil {
		t.Fatalf("resume owner error = %v", err)
	}
	if resumedCalls.Load() != 1 {
		t.Fatalf("resume handler calls = %d, want 1", resumedCalls.Load())
	}
}

func TestGatewayFailsTerminalWhenSuspensionClaimCannotCommit(t *testing.T) {
	store := &failingAtomicSuspendStore{MemoryIdempotencyStore: NewMemoryIdempotencyStore()}
	events := &recordingEventStore{}
	gateway := newGatewayForHardening(events, nil, store, nil, resumableToolDefinition(), func(_ context.Context, _ FunctionCall) (*FunctionResult, error) {
		return nil, &ToolInterruptedError{Info: "continue?", State: json.RawMessage(`{"waiting":true}`)}
	})
	req := resumableToolRequest()
	result, err := gateway.Invoke(resumableToolContext(req), req)
	if err != nil || result == nil || result.Status != ToolCallFailed || result.Failure == nil || result.Failure.ErrorType != string(ErrorTypeInternal) {
		t.Fatalf("suspension failure = result %#v error %v", result, err)
	}
	if got := eventTypes(events.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("suspension failure lifecycle = %v", got)
	}
}

func TestMemoryIdempotencySuspendValidatesWholeKeySetBeforeMutation(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	record := IdempotencyRecord{Key: "call-key", ToolCallID: "call-1", Status: ToolCallRunning}
	if _, inserted, err := store.Claim(context.Background(), record); err != nil || !inserted {
		t.Fatalf("claim = inserted %v error %v", inserted, err)
	}
	if err := store.Suspend(context.Background(), []string{"call-key", "missing-key"}, "call-1"); err == nil {
		t.Fatal("partial key set suspension unexpectedly succeeded")
	}
	got, found, err := store.Get(context.Background(), "call-key")
	if err != nil || !found || got.Status != ToolCallRunning {
		t.Fatalf("validated claim was partially mutated: record %#v found %v error %v", got, found, err)
	}
}

func TestGatewayInterruptsAndResumesSameToolCall(t *testing.T) {
	var calls atomic.Int32
	var resumed ToolCallResume
	stepStore := &taskCStepStore{}
	events := &recordingEventStore{}
	idempotency := NewMemoryIdempotencyStore()
	gateway := newGatewayForHardening(events, nil, idempotency, stepStore, resumableToolDefinition(), func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		if call.Resume == nil {
			return nil, &ToolInterruptedError{
				Info:  map[string]any{"type": "ask_user", "question": "continue?"},
				State: json.RawMessage(`{"phase":"waiting"}`),
			}
		}
		resumed = *call.Resume
		return &FunctionResult{Data: json.RawMessage(`{"answer":"yes"}`), MimeType: "application/json"}, nil
	})
	req := resumableToolRequest()
	ctx := resumableToolContext(req)

	result, err := gateway.Invoke(ctx, req)
	var interrupted *ToolInterruptedError
	if result != nil || !errors.As(err, &interrupted) {
		t.Fatalf("initial invocation = result %#v error %v", result, err)
	}
	if got := eventTypes(events.events); len(got) != 1 || got[0] != observability.EventToolCallStarted {
		t.Fatalf("interrupt lifecycle = %v", got)
	}
	if stepStore.startCalls != 1 || stepStore.completeCalls != 0 || stepStore.failCalls != 0 {
		t.Fatalf("interrupted step lifecycle = %#v", stepStore)
	}
	assertTaskCIdempotencyStatus(t, gateway, req, req.Policy.IdempotencyKey, ToolCallSuspended, false)

	if _, err := gateway.Invoke(ctx, req); !IsErrorType(err, ErrorTypeDuplicateInflight) {
		t.Fatalf("duplicate initial invocation error = %v", err)
	}

	resume := req
	resume.Resume = &ToolCallResume{
		WasInterrupted: true,
		IsResumeTarget: true,
		State:          append(json.RawMessage(nil), interrupted.State...),
		Payload:        json.RawMessage(`{"answer":"yes"}`),
	}
	result, err = gateway.Invoke(ctx, resume)
	if err != nil || result == nil || result.Status != ToolCallSucceeded {
		t.Fatalf("resume invocation = result %#v error %v", result, err)
	}
	if calls.Load() != 2 || !resumed.WasInterrupted || !resumed.IsResumeTarget || string(resumed.Payload) != `{"answer":"yes"}` {
		t.Fatalf("resume was not delivered exactly once: calls=%d resume=%#v", calls.Load(), resumed)
	}
	if got := eventTypes(events.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallCompleted {
		t.Fatalf("resumed lifecycle = %v", got)
	}
	if stepStore.startCalls != 1 || stepStore.completeCalls != 1 || stepStore.failCalls != 0 {
		t.Fatalf("resumed step lifecycle = %#v", stepStore)
	}
	assertTaskCIdempotencyStatus(t, gateway, req, req.Policy.IdempotencyKey, ToolCallSucceeded, true)

	replayed, err := gateway.Invoke(ctx, resume)
	if err != nil || replayed == nil || replayed.Status != ToolCallSucceeded || calls.Load() != 2 {
		t.Fatalf("terminal replay = result %#v error %v calls=%d", replayed, err, calls.Load())
	}
}

func TestGatewayCanInterruptAgainAfterResume(t *testing.T) {
	var calls atomic.Int32
	gateway := newGatewayForHardening(&recordingEventStore{}, nil, NewMemoryIdempotencyStore(), nil, resumableToolDefinition(), func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
		attempt := calls.Add(1)
		if attempt < 3 {
			return nil, &ToolInterruptedError{
				Info:  map[string]any{"attempt": attempt},
				State: json.RawMessage(`{"waiting":true}`),
			}
		}
		return &FunctionResult{Data: json.RawMessage(`{"done":true}`)}, nil
	})
	req := resumableToolRequest()
	ctx := resumableToolContext(req)

	_, err := gateway.Invoke(ctx, req)
	var interrupted *ToolInterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("first interrupt error = %v", err)
	}
	resume := req
	resume.Resume = &ToolCallResume{WasInterrupted: true, IsResumeTarget: true, State: interrupted.State, Payload: json.RawMessage(`{"answer":1}`)}
	_, err = gateway.Invoke(ctx, resume)
	if !errors.As(err, &interrupted) {
		t.Fatalf("second interrupt error = %v", err)
	}
	resume.Resume.State = interrupted.State
	resume.Resume.Payload = json.RawMessage(`{"answer":2}`)
	result, err := gateway.Invoke(ctx, resume)
	if err != nil || result == nil || result.Status != ToolCallSucceeded || calls.Load() != 3 {
		t.Fatalf("second resume = result %#v error %v calls=%d", result, err, calls.Load())
	}
}

func TestGatewayResumeAfterIdempotencyStoreRebuildDoesNotRepeatStarted(t *testing.T) {
	var calls atomic.Int32
	events := &recordingEventStore{}
	def := resumableToolDefinition()
	handler := func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
		calls.Add(1)
		if call.Resume == nil {
			return nil, &ToolInterruptedError{Info: "continue?", State: json.RawMessage(`{"phase":1}`)}
		}
		return &FunctionResult{Data: json.RawMessage(`{"resumed":true}`)}, nil
	}
	first := newGatewayForHardening(events, nil, NewMemoryIdempotencyStore(), nil, def, handler)
	req := resumableToolRequest()
	ctx := resumableToolContext(req)
	_, err := first.Invoke(ctx, req)
	var interrupted *ToolInterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("interrupt error = %v", err)
	}

	second := newGatewayForHardening(events, nil, NewMemoryIdempotencyStore(), nil, def, handler)
	resume := req
	resume.Resume = &ToolCallResume{WasInterrupted: true, IsResumeTarget: true, State: interrupted.State, Payload: json.RawMessage(`{"answer":"yes"}`)}
	result, err := second.Invoke(ctx, resume)
	if err != nil || result == nil || result.Status != ToolCallSucceeded {
		t.Fatalf("rebuilt resume = result %#v error %v", result, err)
	}
	if got := eventTypes(events.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallCompleted {
		t.Fatalf("rebuilt lifecycle = %v", got)
	}
}

func resumableToolDefinition() ToolDefinition {
	def := defaultSearchDefinition()
	def.Name = "approval"
	def.Version = "v1"
	def.Function = &FunctionToolSpec{HandlerName: "search"}
	def.InputSchema = json.RawMessage(`{"type":"object","required":["question"],"properties":{"question":{"type":"string"}},"additionalProperties":false}`)
	def.OutputSchema = json.RawMessage(`{"type":"object"}`)
	def.Permissions.AllowedAgents = []string{"agent-resume"}
	def.Timeout = time.Second
	return def
}

func resumableToolRequest() ToolCallRequest {
	return ToolCallRequest{
		ToolCallID: "tool-call-resume", TenantID: "tenant-resume", UserID: "user-resume",
		SessionID: "session-resume", RunID: "run-resume", StepID: "step-resume", AgentID: "agent-resume",
		ToolName: "approval", ToolVersion: "v1", Arguments: json.RawMessage(`{"question":"continue?"}`),
		Caller: ToolCaller{Type: "agent", AgentID: "agent-resume"},
		Policy: ToolCallPolicy{RiskLevel: RiskLow, Timeout: time.Second, IdempotencyKey: "resume-once"},
	}
}

func resumableToolContext(req ToolCallRequest) context.Context {
	return observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace-resume", TenantID: req.TenantID, UserID: req.UserID,
		SessionID: req.SessionID, RunID: req.RunID, AgentID: req.AgentID,
	})
}

type failingAtomicSuspendStore struct {
	*MemoryIdempotencyStore
}

func (s *failingAtomicSuspendStore) Suspend(context.Context, []string, string) error {
	return errors.New("suspend store unavailable")
}
