package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func TestRuntimeControlCreatorPublishesBoundTicketWithoutResumeToken(t *testing.T) {
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-1", UserID: "user-1", TraceID: "trace-1"})
	stores := memory.New().Stores()
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: "session-1", TenantID: "tenant-1", UserID: "user-1", Status: storage.SessionStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Runs.Create(ctx, &storage.Run{RunID: "run-1", SessionID: "session-1", TenantID: "tenant-1", Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{CheckpointID: "checkpoint-1", RunID: "run-1", TenantID: "tenant-1", StateRef: "artifact://checkpoint"}); err != nil {
		t.Fatal(err)
	}
	codec, err := controlticket.New([]byte("test-control-ticket-secret"))
	if err != nil {
		t.Fatal(err)
	}
	creator := runtimeControlCreator{service: control.New(stores, nil), tickets: codec}
	event, err := creator.CreateControlRequest(ctx, agentruntime.ControlRequestCreateRequest{
		TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", RunID: "run-1",
		RequestID: "control-1", CheckpointID: "checkpoint-1", ResumeToken: "resume-secret", Type: "ask_user",
		Event: observability.AgentEvent{EventType: observability.EventControlRequestCreated, Visibility: observability.VisibilityUserVisible, Payload: json.RawMessage(`{"request_id":"control-1","resume_token":"must-not-leak"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["resume_token"] != nil {
		t.Fatal("public control event leaked resume_token")
	}
	ticket, ok := payload["control_ticket"].(string)
	if !ok || ticket == "" {
		t.Fatalf("control_ticket missing from payload: %#v", payload)
	}
	var preview map[string]any
	if err := json.Unmarshal(event.PayloadPreview, &preview); err != nil {
		t.Fatal(err)
	}
	if preview["control_ticket"] != ticket || preview["resume_token"] != nil {
		t.Fatalf("SSE preview does not carry the sanitized ticket payload: %#v", preview)
	}
	projected := protocol.ProjectEvent(event, protocol.ClientTarget{}, nil)
	var clientPayload map[string]any
	if err := json.Unmarshal(projected.Payload, &clientPayload); err != nil {
		t.Fatal(err)
	}
	if clientPayload["control_ticket"] != ticket || clientPayload["resume_token"] != nil {
		t.Fatalf("protocol projection does not carry the authorized ticket: %#v", clientPayload)
	}
	claims, expiresAt, err := codec.Open(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if claims.TenantID != "tenant-1" || claims.UserID != "user-1" || claims.SessionID != "session-1" || claims.RunID != "run-1" || claims.RequestID != "control-1" || claims.ResumeToken != "resume-secret" {
		t.Fatalf("unexpected ticket claims: %#v", claims)
	}
	if time.Until(expiresAt) < defaultControlRequestTTL-time.Minute {
		t.Fatalf("ticket expiry %v is shorter than configured TTL", expiresAt)
	}
	persisted, err := stores.Controls.Get(ctx, "control-1")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ResumeTokenHash == "" || persisted.ExpiresAt.IsZero() {
		t.Fatalf("persisted control request missing hash/expiry: %#v", persisted)
	}
	events, err := stores.Events.Query(ctx, storage.EventQuery{RunID: "run-1", Limit: 10})
	if err != nil || len(events) != 1 || string(events[0].Payload) != string(event.Payload) || string(events[0].PayloadPreview) != string(event.PayloadPreview) {
		t.Fatalf("persisted event does not match public event: %#v %v", events, err)
	}
}
