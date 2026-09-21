package app

import (
	"context"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/orchestrator"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type dispatchIdentityOrchestrator struct {
	request orchestrator.RunRequest
}

func (o *dispatchIdentityOrchestrator) Run(_ context.Context, req orchestrator.RunRequest) (*dispatcher.Result, error) {
	o.request = req
	events := make(chan observability.AgentEvent)
	close(events)
	return &dispatcher.Result{Mode: dispatcher.ModeInline, Events: events}, nil
}

func (*dispatchIdentityOrchestrator) Resume(context.Context, agentruntime.ResumeRequest) (<-chan observability.AgentEvent, error) {
	return nil, nil
}

func (*dispatchIdentityOrchestrator) Cancel(context.Context, agentruntime.CancelRequest) error {
	return nil
}

func TestFormalRunDispatcherPreservesUserMessageIdentity(t *testing.T) {
	orchestratorStub := &dispatchIdentityOrchestrator{}
	ledger := contextpkg.NewInMemoryMessageLedger()
	snapshots := contextpkg.NewInMemorySnapshotStore()
	stored, err := ledger.Append(context.Background(), "session_identity", contextpkg.Message{
		ID: "message_identity", IdempotencyKey: "idempotency_identity", Role: contextpkg.RoleUser,
		Content: "full body from the frozen ledger",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshots.Save(context.Background(), contextpkg.Snapshot{
		SchemaVersion: contextpkg.SnapshotSchemaVersion, ID: "context_identity", SessionID: "session_identity",
		RunID: "run_identity", MessageIDs: []string{stored.ID},
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := &formalRunDispatcher{
		orchestrator: orchestratorStub,
		ledger:       ledger,
		snapshots:    snapshots,
		slots:        make(chan struct{}, 1),
		ids:          observability.NewULIDGenerator("binding"),
	}
	turn := &storage.OpenTurnResult{
		Run: &storage.Run{
			RunID: "run_identity", SessionID: "session_identity", TenantID: "tenant_identity", AgentID: "agent_identity",
		},
		UserMessage: &storage.Message{
			ID: "message_identity", ContentPreview: "same content as frozen snapshot",
		},
		ContextSnapshotRef: "context_identity",
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_identity", TenantID: "tenant_identity", UserID: "user_identity",
	})

	if err := dispatcher.Dispatch(ctx, turn); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(orchestratorStub.request.Input) != 1 {
		t.Fatalf("runtime input count=%d, want 1", len(orchestratorStub.request.Input))
	}
	input := orchestratorStub.request.Input[0]
	if input.ID != stored.ID || input.IdempotencyKey != stored.IdempotencyKey {
		t.Fatalf("runtime input identity=(%q, %q), want frozen identity=(%q, %q)", input.ID, input.IdempotencyKey, stored.ID, stored.IdempotencyKey)
	}
	if input.Role != "user" || input.Content != stored.Content {
		t.Fatalf("runtime input content=%#v", input)
	}
}
