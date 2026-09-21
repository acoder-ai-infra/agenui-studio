package app

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func TestEmbeddedAnswerControlUsesTicketArtifactAndFormalResumer(t *testing.T) {
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace-sdk-control", TenantID: "tenant-sdk", UserID: "user-sdk",
	})
	stores := memory.New().Stores()
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: "session-sdk", TenantID: "tenant-sdk", UserID: "user-sdk", Status: storage.SessionStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Runs.Create(ctx, &storage.Run{RunID: "run-sdk", SessionID: "session-sdk", TenantID: "tenant-sdk", Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{CheckpointID: "checkpoint-sdk", RunID: "run-sdk", TenantID: "tenant-sdk", StateRef: "artifact://checkpoint"}); err != nil {
		t.Fatal(err)
	}
	codec, err := controlticket.New([]byte("sdk-control-ticket-secret"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := (runtimeControlCreator{service: control.New(stores, nil), tickets: codec}).CreateControlRequest(ctx, agentruntime.ControlRequestCreateRequest{
		TenantID: "tenant-sdk", UserID: "user-sdk", SessionID: "session-sdk", RunID: "run-sdk",
		RequestID: "control-sdk", CheckpointID: "checkpoint-sdk", ResumeToken: "resume-sdk-secret", Type: "ask_user",
		Event: observability.AgentEvent{
			EventType: observability.EventControlRequestCreated, Visibility: observability.VisibilityUserVisible,
			Payload: json.RawMessage(`{"request_id":"control-sdk","type":"ask_user"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var createdPayload struct {
		ControlTicket string `json:"control_ticket"`
	}
	if err := json.Unmarshal(created.Payload, &createdPayload); err != nil || createdPayload.ControlTicket == "" {
		t.Fatalf("control ticket payload = %s error %v", created.Payload, err)
	}

	artifacts := artifact.NewStore(artifact.StoreConfig{
		ObjectStore: objectstore.NewMemory(), MetadataStore: metastore.NewMemory(),
	})
	resumer := &capturingEmbeddedResumer{}
	engine := newEmbeddedEngine(nil, stores, nil, nil, control.New(stores, resumer), codec, artifacts)
	t.Cleanup(engine.Close)
	response, err := engine.AnswerControl(context.Background(), EmbeddedControlResponseRequest{
		TenantID: "tenant-sdk", UserID: "user-sdk", SessionID: "session-sdk", RunID: "run-sdk",
		RequestID: "control-sdk", ControlTicket: createdPayload.ControlTicket, ClientEventID: "answer-1",
		Decision: "submitted", Targets: map[string]any{"interrupt-1": map[string]any{"answer": "yes"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RequestID != "control-sdk" || response.Status != string(control.StatusAnswered) {
		t.Fatalf("control response = %#v", response)
	}
	if resumer.request.RequestID != "control-sdk" || resumer.request.ResumeToken != "resume-sdk-secret" || resumer.request.ResponseRef == "" {
		t.Fatalf("resume request = %#v", resumer.request)
	}

	artifactCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		TenantID: "tenant-sdk", SessionID: "session-sdk", RunID: "run-sdk", Role: artifact.ActorRuntime,
	})
	object, err := artifacts.Get(artifactCtx, resumer.request.ResponseRef, artifact.GetOptions{Purpose: artifact.PurposeReplay})
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(object.Content)
	closeErr := object.Content.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read response artifact: %v %v", readErr, closeErr)
	}
	var payload struct {
		Decision string         `json:"decision"`
		Targets  map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Decision != "submitted" || payload.Targets["interrupt-1"] == nil {
		t.Fatalf("response artifact = %s error %v", data, err)
	}
	expiredTicket, err := codec.Seal(controlticket.Claims{
		TenantID: "tenant-sdk", UserID: "user-sdk", SessionID: "session-sdk", RunID: "run-sdk",
		RequestID: "control-sdk", ResumeToken: "resume-sdk-secret",
	}, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	tamperedTicket := createdPayload.ControlTicket[:len(createdPayload.ControlTicket)-1] + "A"
	if tamperedTicket == createdPayload.ControlTicket {
		tamperedTicket = createdPayload.ControlTicket[:len(createdPayload.ControlTicket)-1] + "B"
	}
	for name, ticket := range map[string]string{
		"tampered": tamperedTicket,
		"expired":  expiredTicket,
	} {
		t.Run(name+" ticket", func(t *testing.T) {
			_, answerErr := engine.AnswerControl(context.Background(), EmbeddedControlResponseRequest{
				TenantID: "tenant-sdk", UserID: "user-sdk", SessionID: "session-sdk", RunID: "run-sdk",
				RequestID: "control-sdk", ControlTicket: ticket,
			})
			if answerErr == nil {
				t.Fatalf("%s control ticket unexpectedly succeeded", name)
			}
		})
	}
}

func TestEmbeddedAnswerControlRejectsWrongActorBeforeArtifactWrite(t *testing.T) {
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace", TenantID: "tenant-a", UserID: "user-a"})
	stores := memory.New().Stores()
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: "session-a", TenantID: "tenant-a", UserID: "user-a", Status: storage.SessionStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Runs.Create(ctx, &storage.Run{RunID: "run-a", SessionID: "session-a", TenantID: "tenant-a", Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Checkpoints.Create(ctx, &storage.CheckpointMeta{CheckpointID: "checkpoint-a", RunID: "run-a", TenantID: "tenant-a", StateRef: "artifact://checkpoint"}); err != nil {
		t.Fatal(err)
	}
	codec, err := controlticket.New([]byte("sdk-control-ticket-secret"))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := codec.Seal(controlticket.Claims{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", RunID: "run-a", RequestID: "control-a", ResumeToken: "resume-a",
	}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objectstore.NewMemory(), MetadataStore: metastore.NewMemory()})
	engine := newEmbeddedEngine(nil, stores, nil, nil, control.New(stores, nil), codec, artifacts)
	t.Cleanup(engine.Close)
	_, err = engine.AnswerControl(context.Background(), EmbeddedControlResponseRequest{
		TenantID: "tenant-a", UserID: "other-user", SessionID: "session-a", RunID: "run-a",
		RequestID: "control-a", ControlTicket: ticket, ResponseText: "yes",
	})
	if err == nil {
		t.Fatal("cross-user control response unexpectedly succeeded")
	}
}

type capturingEmbeddedResumer struct {
	request control.ResumeRequest
}

func (r *capturingEmbeddedResumer) Resume(_ context.Context, req control.ResumeRequest) error {
	r.request = req
	return nil
}
