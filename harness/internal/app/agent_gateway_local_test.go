package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/orchestrator"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagemem "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func TestLocalChildRunExecutorCreatesStandardChildRunAndDerivedSnapshot(t *testing.T) {
	backend := storagemem.New()
	stores := backend.Stores()
	snapshotStore := contextpkg.NewInMemorySnapshotStore()
	orch := &capturingChildOrchestrator{}
	if err := stores.Runs.Create(context.Background(), &storage.Run{
		RunID: "parent", SessionID: "session", TurnID: "turn-1", TenantID: "tenant", Status: storage.RunStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	executor := localChildRunExecutor{
		orchestrator: orch, runs: stores.Runs,
		snapshots: contextpkg.SnapshotManager{Store: snapshotStore, IDs: observability.NewULIDGenerator("snapshot").NewRequestID},
		ids:       observability.NewULIDGenerator("child"),
	}
	result, err := executor.ExecuteChild(context.Background(), agentgateway.LocalRunExecutionRequest{
		TaskID: "task-1", AttemptID: "attempt-1", TenantID: "tenant", SessionID: "session",
		ParentRunID: "parent", Description: "find a hotel", Depth: 1,
		InputParts: []contextpkg.ContentPart{{Kind: "image_ref", MIME: "image/png", Filename: "reference.png", ArtifactRef: "artifact-image-1"}},
		Target:     agentgateway.ResolvedTarget{Definition: agentruntime.AgentDefinition{AgentID: "hotel", Version: "v1", Runtime: agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect}}},
		ScopedData: agentruntime.ScopedData{Run: map[string]agentruntime.ScopedDataItem{"locale": {Value: json.RawMessage(`"zh-CN"`)}}},
		Trace:      observability.TraceContext{TraceID: "trace", TenantID: "tenant", UserID: "user"},
	})
	if err != nil {
		t.Fatalf("ExecuteChild: %v", err)
	}
	if result.ChildRunID == "" || result.Content != "child answer" || result.ContentRef != "message://child/content" {
		t.Fatalf("unexpected child result: %+v", result)
	}
	run, err := stores.Runs.Get(context.Background(), result.ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ParentRunID != "parent" || run.SessionID != "session" || run.TurnID != "turn-1" || run.AgentID != "hotel" || run.ContextSnapshotRef == "" {
		t.Fatalf("child Run facts incomplete: %+v", run)
	}
	snapshot, err := snapshotStore.Load(context.Background(), run.ContextSnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunID != result.ChildRunID || snapshot.Source != "agent_gateway_child" || len(snapshot.MessageIDs) != 0 {
		t.Fatalf("derived snapshot mismatch: %+v", snapshot)
	}
	if orch.request.BindingRequest.Request.AgentVersion != "v1" || orch.request.ParentRunID != "parent" || orch.request.ContextSnapshotRef != snapshot.ID ||
		string(orch.request.ScopedData.Run["locale"].Value) != `"zh-CN"` || orch.request.Metadata["agent_gateway.depth"] != "1" {
		t.Fatalf("orchestrator request drift: %+v", orch.request)
	}
	if len(orch.request.Input) != 1 || len(orch.request.Input[0].Parts) != 2 ||
		orch.request.Input[0].Parts[0].Kind != "text" || orch.request.Input[0].Parts[0].Text != "find a hotel" ||
		orch.request.Input[0].Parts[1].ArtifactRef != "artifact-image-1" {
		t.Fatalf("child input did not preserve the task text and reference image: %+v", orch.request.Input)
	}
}

func TestChildInputPartsLeavesTextOnlyChildInputUnchanged(t *testing.T) {
	if parts := childInputParts("describe a product", nil); parts != nil {
		t.Fatalf("text-only child input should use Message.Content, got %+v", parts)
	}
}

func TestOutputRepairFeedbackIsBoundedAndGeneric(t *testing.T) {
	feedback := strings.Repeat("x", 1300)
	repaired := appendOutputRepairFeedback("original task", feedback)
	if !strings.Contains(repaired, "<output-repair>") || !strings.Contains(repaired, "original task") {
		t.Fatalf("missing generic repair envelope: %q", repaired)
	}
	if len([]rune(repaired)) > len([]rune("original task\n\n<output-repair>\n\n</output-repair>"))+1201 {
		t.Fatalf("repair feedback must be bounded: %d", len([]rune(repaired)))
	}
	if got := appendOutputRepairFeedback("task", ""); !strings.Contains(got, "did not satisfy the output contract") {
		t.Fatalf("missing generic fallback: %q", got)
	}
}

func TestOutputRetryFromFailureEvent(t *testing.T) {
	event := observability.AgentEvent{
		EventType: observability.EventRunFailed,
		Error:     &observability.EventError{Code: "OUTPUT_VALIDATION_RETRY", Message: "output validation requires repair"},
		Payload:   json.RawMessage(`{"retry_budget":1,"repair_feedback":"return canonical output"}`),
	}
	retry := outputRetryFromFailureEvent(event)
	if retry == nil || retry.RetryBudget != 1 || retry.RepairFeedback != "return canonical output" {
		t.Fatalf("retry event was not decoded: %#v", retry)
	}
	if outputRetryFromFailureEvent(observability.AgentEvent{EventType: observability.EventRunFailed, Error: &observability.EventError{Code: "OTHER"}}) != nil {
		t.Fatal("unrelated failed run must not become an output retry")
	}
}

func TestLocalChildRunExecutorRetriesWithValidatorFeedback(t *testing.T) {
	backend := storagemem.New()
	stores := backend.Stores()
	if err := stores.Runs.Create(context.Background(), &storage.Run{
		RunID: "parent", SessionID: "session", TurnID: "turn-1", TenantID: "tenant", Status: storage.RunStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	orch := &retryingChildOrchestrator{}
	executor := localChildRunExecutor{
		orchestrator: orch, runs: stores.Runs,
		snapshots: contextpkg.SnapshotManager{Store: contextpkg.NewInMemorySnapshotStore(), IDs: observability.NewULIDGenerator("snapshot").NewRequestID},
		ids:       observability.NewULIDGenerator("child"),
	}
	result, err := executor.ExecuteChild(context.Background(), agentgateway.LocalRunExecutionRequest{
		TaskID: "task", AttemptID: "attempt", TenantID: "tenant", SessionID: "session", ParentRunID: "parent", Description: "make a card",
		Target: agentgateway.ResolvedTarget{Definition: agentruntime.AgentDefinition{AgentID: "child", Version: "v1", Runtime: agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect}}},
		Trace:  observability.TraceContext{TraceID: "trace", TenantID: "tenant", UserID: "user"},
	})
	if err != nil {
		t.Fatalf("ExecuteChild: %v", err)
	}
	if result.Content != "repaired answer" || orch.calls != 2 {
		t.Fatalf("unexpected retry result=%+v calls=%d", result, orch.calls)
	}
	if !strings.Contains(orch.descriptions[1], "<output-repair>") || !strings.Contains(orch.descriptions[1], "return canonical output") {
		t.Fatalf("second attempt did not receive validator feedback: %q", orch.descriptions[1])
	}
}

type capturingChildOrchestrator struct{ request orchestrator.RunRequest }

func (o *capturingChildOrchestrator) Run(_ context.Context, req orchestrator.RunRequest) (*dispatcher.Result, error) {
	o.request = req
	events := make(chan observability.AgentEvent, 3)
	events <- observability.AgentEvent{EventType: observability.EventAgentTextDelta, Payload: json.RawMessage(`{"text":"child answer"}`)}
	events <- observability.AgentEvent{EventType: observability.EventFinalResponse, Payload: json.RawMessage(`{"message_id":"msg","content_ref":"message://child/content","preview":"child answer","content_hash":"hash","size_bytes":12,"content_type":"text/plain"}`)}
	events <- observability.AgentEvent{EventType: observability.EventRunCompleted}
	close(events)
	return &dispatcher.Result{Mode: dispatcher.ModeInline, Events: events}, nil
}

func (*capturingChildOrchestrator) Resume(context.Context, agentruntime.ResumeRequest) (<-chan observability.AgentEvent, error) {
	return nil, nil
}

func (*capturingChildOrchestrator) Cancel(context.Context, agentruntime.CancelRequest) error {
	return nil
}

var _ orchestrator.Orchestrator = (*capturingChildOrchestrator)(nil)

type retryingChildOrchestrator struct {
	calls        int
	descriptions []string
}

func (o *retryingChildOrchestrator) Run(_ context.Context, req orchestrator.RunRequest) (*dispatcher.Result, error) {
	o.calls++
	o.descriptions = append(o.descriptions, req.Input[0].Content)
	events := make(chan observability.AgentEvent, 3)
	if o.calls == 1 {
		events <- observability.AgentEvent{
			EventType: observability.EventRunFailed,
			Error:     &observability.EventError{Code: "OUTPUT_VALIDATION_RETRY", Message: "output validation requires repair"},
			Payload:   json.RawMessage(`{"retry_budget":1,"repair_feedback":"return canonical output"}`),
		}
	} else {
		events <- observability.AgentEvent{EventType: observability.EventAgentTextDelta, Payload: json.RawMessage(`{"text":"repaired answer"}`)}
		events <- observability.AgentEvent{EventType: observability.EventFinalResponse, Payload: json.RawMessage(`{"message_id":"msg","content_ref":"message://child/content","preview":"repaired answer","content_hash":"hash","size_bytes":15,"content_type":"text/plain"}`)}
		events <- observability.AgentEvent{EventType: observability.EventRunCompleted}
	}
	close(events)
	return &dispatcher.Result{Mode: dispatcher.ModeInline, Events: events}, nil
}

func (*retryingChildOrchestrator) Resume(context.Context, agentruntime.ResumeRequest) (<-chan observability.AgentEvent, error) {
	return nil, nil
}

func (*retryingChildOrchestrator) Cancel(context.Context, agentruntime.CancelRequest) error {
	return nil
}

var _ orchestrator.Orchestrator = (*retryingChildOrchestrator)(nil)
