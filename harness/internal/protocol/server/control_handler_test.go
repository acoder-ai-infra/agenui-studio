package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol/server"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestControlResponseAcceptsInlineTextAndPersistsArtifact(t *testing.T) {
	deps, stores := newDeps()
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	deps.Artifacts = artifacts
	router := server.NewRouter(deps)

	mustCreateOwnedSessionRun(t, stores, "s1", "run-control", "acme", "u1")
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "acme", UserID: "u1", TraceID: "trace_control"})
	if _, err := stores.Runs.CompareAndSetStatus(ctx, "run-control", storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Runs.CompareAndSetStatus(ctx, "run-control", storage.RunStatusRunning, storage.RunStatusWaitingControl, storage.RunMutation{}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Controls.Create(ctx, &storage.ControlRequest{
		RequestID: "ctrl_inline", RunID: "run-control", TenantID: "acme", CheckpointID: "ckpt_1",
		Type: "ask_user", Status: string(control.StatusPending), ResumeTokenHash: hashToken("tok"),
		PromptPreview: "Need input", SchemaVersion: storage.ControlRequestSchemaVersion,
	}); err != nil {
		t.Fatal(err)
	}

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/api/v1/control-requests/ctrl_inline/responses",
		strings.NewReader(`{"resume_token":"tok","decision":"answer","response_text":"Use the safer option"}`)), "acme", "u1")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("response status=%d body=%s", rec.Code, rec.Body.String())
	}

	controlRow, err := stores.Controls.Get(ctx, "ctrl_inline")
	if err != nil {
		t.Fatal(err)
	}
	if controlRow.Status != string(control.StatusAnswered) || controlRow.ResponseRef == "" {
		t.Fatalf("control not answered with response ref: %#v", controlRow)
	}
	obj, err := artifacts.Get(artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: "acme", UserID: "u1", SessionID: "s1", RunID: "run-control", Role: artifact.ActorRuntime,
	}), controlRow.ResponseRef, artifact.GetOptions{Purpose: artifact.PurposeView})
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Content.Close()
	data, err := io.ReadAll(obj.Content)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["decision"] != "answer" || payload["response_text"] != "Use the safer option" {
		t.Fatalf("unexpected response artifact: %s", data)
	}
}

func TestControlResponsePersistsTargetedAskUserAnswer(t *testing.T) {
	deps, stores := newDeps()
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory()})
	deps.Artifacts = artifacts
	router := server.NewRouter(deps)

	mustCreateOwnedSessionRun(t, stores, "s1", "run-targeted-control", "acme", "u1")
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "acme", UserID: "u1", TraceID: "trace_targeted_control"})
	if _, err := stores.Runs.CompareAndSetStatus(ctx, "run-targeted-control", storage.RunStatusCreated, storage.RunStatusRunning, storage.RunMutation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Runs.CompareAndSetStatus(ctx, "run-targeted-control", storage.RunStatusRunning, storage.RunStatusWaitingControl, storage.RunMutation{}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Controls.Create(ctx, &storage.ControlRequest{
		RequestID: "ctrl_targeted", RunID: "run-targeted-control", TenantID: "acme", CheckpointID: "ckpt_targeted",
		Type: "ask_user", Status: string(control.StatusPending), ResumeTokenHash: hashToken("tok"),
		PromptPreview: "Choose a rollout", SchemaVersion: storage.ControlRequestSchemaVersion,
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"resume_token":"tok","decision":"answer","response_text":"Use canary","targets":{"interrupt_1":{"answers":[{"question_index":0,"header":"Rollout","question":"Which rollout?","selected_option":{"label":"Canary","description":"Limit exposure"}}]}}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/api/v1/control-requests/ctrl_targeted/responses", strings.NewReader(body)), "acme", "u1")
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("response status=%d body=%s", rec.Code, rec.Body.String())
	}

	controlRow, err := stores.Controls.Get(ctx, "ctrl_targeted")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := artifacts.Get(artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: "acme", UserID: "u1", SessionID: "s1", RunID: "run-targeted-control", Role: artifact.ActorRuntime,
	}), controlRow.ResponseRef, artifact.GetOptions{Purpose: artifact.PurposeView})
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Content.Close()
	data, err := io.ReadAll(obj.Content)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Targets map[string]json.RawMessage `json:"targets"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Targets) != 1 || len(payload.Targets["interrupt_1"]) == 0 {
		t.Fatalf("targeted answer was not preserved: %s", data)
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}
