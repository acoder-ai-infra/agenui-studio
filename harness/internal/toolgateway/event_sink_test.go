package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestDefaultToolEventSinkPersistsProgressAsCanonicalAgentEvent(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"stage":"search","message":"working","percent":40}`))

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit progress: %v", err)
	}
	if result == nil || result.PersistedEvent == nil {
		t.Fatalf("progress result = %#v", result)
	}
	persisted := result.PersistedEvent
	if persisted.EventType != observability.EventToolCallProgress || persisted.Sequence != 1 || persisted.SchemaVersion != observability.AgentEventSchemaVersion {
		t.Fatalf("canonical progress = %#v", persisted)
	}
	if persisted.Visibility != observability.VisibilityUserVisible || persisted.TraceID != "trace-1" || persisted.SessionID != "sess-1" || persisted.RunID != "run-1" || persisted.StepID != "step-1" || persisted.AgentID != "agent-a" {
		t.Fatalf("progress outer identity = %#v", persisted)
	}
	payload := decodeEventPayload(t, *persisted)
	assertCanonicalToolPayload(t, payload)
	if payload["stage"] != "search" || payload["message"] != "working" || payload["percent"] != float64(40) {
		t.Fatalf("progress payload = %#v", payload)
	}
}

func TestDefaultToolEventSinkMapsWarningToToolCallProgress(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventWarning, json.RawMessage(`{"code":"DEGRADED","message":"using cached data","retryable":true}`))

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit warning: %v", err)
	}
	if result == nil || result.PersistedEvent == nil || result.PersistedEvent.EventType != observability.EventToolCallProgress {
		t.Fatalf("warning result = %#v", result)
	}
	payload := decodeEventPayload(t, *result.PersistedEvent)
	assertCanonicalToolPayload(t, payload)
	if payload["kind"] != "warning" || payload["code"] != "DEGRADED" || payload["message"] != "using cached data" || payload["retryable"] != true {
		t.Fatalf("warning payload = %#v", payload)
	}
}

func TestDefaultToolEventSinkPersistsArtifactAsToolArtifactCreated(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	meta := putToolEventArtifact(t, fixture.artifactStore, fixture.ctx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", StepID: "artifact-step",
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: "context-owner",
		ArtifactType: artifact.ArtifactTypeFile, MimeType: "text/plain", Name: "report.txt",
		Visibility: artifact.VisibilityUserVisible, Content: strings.NewReader("authoritative report"),
		RetentionPolicy: artifact.RetentionRunTTL, CreatedBy: "tool:search_kb",
	})
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+meta.ArtifactRef+`","mime_type":"forged/type","size_bytes":9999,"hash":"forged"}`))
	event.PayloadRef = meta.ArtifactRef
	event.Visibility = observability.VisibilityUserVisible

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit artifact: %v", err)
	}
	if result == nil || result.PersistedEvent == nil || result.PersistedEvent.EventType != observability.EventToolArtifactCreated {
		t.Fatalf("artifact result = %#v", result)
	}
	persisted := result.PersistedEvent
	if persisted.PayloadRef != meta.ArtifactRef || persisted.Visibility != observability.VisibilityUserVisible {
		t.Fatalf("artifact outer metadata = %#v", persisted)
	}
	payload := decodeEventPayload(t, *persisted)
	assertCanonicalToolPayload(t, payload)
	assertAuthoritativeArtifactPayload(t, payload, meta)
}

func TestDefaultToolEventSinkStoresDebugAsArtifactWithoutToolDebugEvent(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventDebug, json.RawMessage(`{"message":"wire trace","data":{"password":"debug-secret","safe":"value"}}`))
	event.Visibility = observability.VisibilityDebug

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit debug: %v", err)
	}
	if result == nil || result.PersistedEvent != nil || result.Reason != "debug_artifact_only" {
		t.Fatalf("debug result = %#v", result)
	}
	if len(fixture.eventStore.events) != 0 {
		t.Fatalf("debug added AgentEvent: %#v", eventTypes(fixture.eventStore.events))
	}
	metas := listToolCallArtifacts(t, fixture.ctx, fixture.artifactStore)
	if len(metas) != 1 || metas[0].ArtifactType != artifact.ArtifactTypeDebugPayload || metas[0].Visibility != artifact.VisibilityDebug {
		t.Fatalf("debug artifacts = %#v", metas)
	}
	content := readDebugArtifact(t, fixture.ctx, fixture.artifactStore, metas[0].ArtifactRef)
	if strings.Contains(string(content), "debug-secret") || !strings.Contains(string(content), "[REDACTED]") {
		t.Fatalf("unsafe debug artifact: %s", content)
	}
}

func TestDefaultToolEventSinkAddsCanonicalToolIdentity(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"tool_call_id":"forged","tool_name":"forged","tool_version":"forged","tool_type":"mcp","message":"working"}`))

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit progress identity: %v", err)
	}
	payload := decodeEventPayload(t, *result.PersistedEvent)
	assertCanonicalToolPayload(t, payload)
}

func TestNormalizeToolEventRejectsUnknownMappingAndLeavesSequenceZero(t *testing.T) {
	known, err := NormalizeToolEvent(fixedIDGenerator{}, ToolEvent{
		ToolEventID: "te-known", EventType: ToolEventProgress, Sequence: 77,
		RunID: "run-1", ToolCallID: "tc-1", ToolName: "search_kb", ToolVersion: "v1", ToolType: ToolTypeFunction,
		Visibility:     observability.VisibilityDebug,
		PayloadPreview: json.RawMessage(`{"tool_call_id":"tc-1","tool_name":"search_kb","tool_version":"v1","tool_type":"function"}`),
		CreatedAt:      time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("normalize known mapping: %v", err)
	}
	if known.EventType != observability.EventToolCallProgress || known.Sequence != 0 {
		t.Fatalf("known normalization = %#v", known)
	}
	unknown, err := NormalizeToolEvent(fixedIDGenerator{}, ToolEvent{EventType: ToolEventType("tool_private_event"), Sequence: 88})
	if err == nil || !reflect.DeepEqual(unknown, observability.AgentEvent{}) {
		t.Fatalf("unknown normalization = %#v err=%v", unknown, err)
	}
}

func TestNormalizeToolEventRejectsMissingOrInvalidSanitizedPreview(t *testing.T) {
	for _, tt := range []struct {
		name    string
		preview json.RawMessage
	}{
		{name: "missing"},
		{name: "invalid", preview: json.RawMessage(`{"tool_call_id":`)},
		{name: "scalar", preview: json.RawMessage(`"bounded"`)},
		{name: "identity mismatch", preview: json.RawMessage(`{"tool_call_id":"forged","tool_name":"search_kb","tool_version":"v1","tool_type":"function"}`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			event, err := NormalizeToolEvent(fixedIDGenerator{}, ToolEvent{
				ToolEventID: "te-unsafe", EventType: ToolEventProgress,
				RunID: "run-1", ToolCallID: "tc-1", ToolName: "search_kb", ToolVersion: "v1", ToolType: ToolTypeFunction,
				Payload:        json.RawMessage(`{"password":"raw-secret"}`),
				PayloadPreview: tt.preview,
			})
			if err == nil || !reflect.DeepEqual(event, observability.AgentEvent{}) {
				t.Fatalf("preview=%s event=%#v err=%v", tt.preview, event, err)
			}
		})
	}
}

func TestDefaultToolEventSinkRejectsCrossScopeArtifactRefs(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	otherCtx := artifact.ContextWithActor(testTraceContext(), artifact.Actor{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-other", AgentID: "agent-a", Role: artifact.ActorRuntime,
	})
	meta := putToolEventArtifact(t, fixture.artifactStore, otherCtx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-other", StepID: "source-step",
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: "source", ArtifactType: artifact.ArtifactTypeFile,
		MimeType: "text/plain", Visibility: artifact.VisibilityInternal, Content: strings.NewReader("cross-run"), RetentionPolicy: artifact.RetentionRunTTL,
	})
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+meta.ArtifactRef+`"}`))
	event.PayloadRef = meta.ArtifactRef

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
		t.Fatalf("cross-scope result=%#v err=%v events=%#v", result, err, eventTypes(fixture.eventStore.events))
	}
}

func TestDefaultToolEventSinkUsesAuthoritativeArtifactMetadata(t *testing.T) {
	meta := validToolEventArtifactMeta()
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{
		EventStore:    eventStore,
		ArtifactStore: headOverrideArtifactStore{meta: meta},
		IDGenerator:   fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+meta.ArtifactRef+`","mime_type":"forged/type","size_bytes":9999,"hash":"forged"}`))
	event.PayloadRef = meta.ArtifactRef
	event.Visibility = observability.VisibilityInternal

	result, err := sink.Emit(testTraceContext(), event)
	if err != nil {
		t.Fatalf("emit authoritative artifact: %v", err)
	}
	payload := decodeEventPayload(t, *result.PersistedEvent)
	assertAuthoritativeArtifactPayload(t, payload, meta)
}

func TestDefaultToolEventSinkRejectsArtifactVisibilityWidening(t *testing.T) {
	meta := validToolEventArtifactMeta()
	meta.Visibility = artifact.VisibilityDebug
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, ArtifactStore: headOverrideArtifactStore{meta: meta}})
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+meta.ArtifactRef+`"}`))
	event.PayloadRef = meta.ArtifactRef
	event.Visibility = observability.VisibilityUserVisible

	result, err := sink.Emit(testTraceContext(), event)
	if result != nil || err == nil || len(eventStore.events) != 0 {
		t.Fatalf("visibility widening result=%#v err=%v events=%#v", result, err, eventTypes(eventStore.events))
	}
}

func TestDefaultToolEventSinkValidatesDebugRawRef(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	meta := putToolEventArtifact(t, fixture.artifactStore, fixture.ctx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", StepID: "debug-step",
		OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tc-1", ArtifactType: artifact.ArtifactTypeDebugPayload,
		MimeType: "application/json", Visibility: artifact.VisibilityDebug, Content: strings.NewReader(`{"wire":"raw"}`), RetentionPolicy: artifact.RetentionDebugShortTTL,
	})
	event := baseProcessToolEvent(ToolEventDebug, json.RawMessage(`{"message":"existing debug","raw_ref":"`+meta.ArtifactRef+`"}`))
	event.Visibility = observability.VisibilityDebug

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit existing debug ref: %v", err)
	}
	if result == nil || result.Reason != "debug_artifact_only" || result.PersistedEvent != nil {
		t.Fatalf("debug raw-ref result = %#v", result)
	}
	if got := listToolCallArtifacts(t, fixture.ctx, fixture.artifactStore); len(got) != 1 {
		t.Fatalf("debug raw ref should be reused, artifacts=%#v", got)
	}
	if len(fixture.eventStore.events) != 0 {
		t.Fatalf("debug raw ref persisted AgentEvent")
	}
}

func TestDefaultToolEventSinkRejectsMissingCanonicalIdentity(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	tests := []struct {
		name   string
		ctx    context.Context
		mutate func(*ToolEvent)
	}{
		{name: "missing trace without trusted context", ctx: context.Background(), mutate: func(event *ToolEvent) { event.TraceID = "" }},
		{name: "missing tenant", ctx: context.Background(), mutate: func(event *ToolEvent) { event.TenantID = "" }},
		{name: "missing session", ctx: context.Background(), mutate: func(event *ToolEvent) { event.SessionID = "" }},
		{name: "missing run", ctx: context.Background(), mutate: func(event *ToolEvent) { event.RunID = "" }},
		{name: "missing agent", ctx: context.Background(), mutate: func(event *ToolEvent) { event.AgentID = "" }},
		{name: "missing step", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.StepID = "" }},
		{name: "missing tool call", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.ToolCallID = "" }},
		{name: "missing tool name", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.ToolName = "" }},
		{name: "missing tool version", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.ToolVersion = "" }},
		{name: "missing tool type", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.ToolType = "" }},
		{name: "trusted trace conflict", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.TraceID = "trace-forged" }},
		{name: "trusted tenant conflict", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.TenantID = "tenant-forged" }},
		{name: "trusted user conflict", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.UserID = "user-forged" }},
		{name: "trusted session conflict", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.SessionID = "session-forged" }},
		{name: "trusted run conflict", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.RunID = "run-forged" }},
		{name: "trusted agent conflict", ctx: fixture.ctx, mutate: func(event *ToolEvent) { event.AgentID = "agent-forged" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
			tt.mutate(&event)
			result, err := fixture.sink.Emit(tt.ctx, event)
			if result != nil || err == nil {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
	if len(fixture.eventStore.events) != 0 {
		t.Fatalf("invalid identity persisted events=%#v", eventTypes(fixture.eventStore.events))
	}
}

func TestDefaultToolEventSinkRejectsOutOfRangeProgressPercent(t *testing.T) {
	for _, percent := range []string{"-1", "101", "1.5"} {
		t.Run(percent, func(t *testing.T) {
			fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
			event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working","percent":`+percent+`}`))
			result, err := fixture.sink.Emit(fixture.ctx, event)
			if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
				t.Fatalf("percent=%s result=%#v err=%v events=%#v", percent, result, err, eventTypes(fixture.eventStore.events))
			}
		})
	}
}

func TestDefaultToolEventSinkRejectsEmptyWarningCodeOrMessage(t *testing.T) {
	for name, payload := range map[string]json.RawMessage{
		"code":    json.RawMessage(`{"message":"warning"}`),
		"message": json.RawMessage(`{"code":"WARN"}`),
		"blank":   json.RawMessage(`{"code":" ","message":" "}`),
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
			event := baseProcessToolEvent(ToolEventWarning, payload)
			result, err := fixture.sink.Emit(fixture.ctx, event)
			if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
				t.Fatalf("result=%#v err=%v events=%#v", result, err, eventTypes(fixture.eventStore.events))
			}
		})
	}
}

func TestDefaultToolEventSinkRejectsUnknownArtifactRef(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	ref := "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_missing"
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+ref+`"}`))
	event.PayloadRef = ref
	result, err := fixture.sink.Emit(fixture.ctx, event)
	if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
		t.Fatalf("result=%#v err=%v events=%#v", result, err, eventTypes(fixture.eventStore.events))
	}
}

func TestDefaultToolEventSinkRejectsIllegalVisibility(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
	event.Visibility = observability.EventVisibility("public")
	result, err := fixture.sink.Emit(fixture.ctx, event)
	if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
		t.Fatalf("result=%#v err=%v events=%#v", result, err, eventTypes(fixture.eventStore.events))
	}
}

func TestDefaultToolEventSinkRejectsIllegalArtifactVisibilityBeforeLookup(t *testing.T) {
	store := &countingHeadArtifactStore{err: errors.New("artifact lookup should not run")}
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, ArtifactStore: store})
	ref := "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_private"
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+ref+`"}`))
	event.PayloadRef = ref
	event.Visibility = observability.EventVisibility("public")

	result, err := sink.Emit(testTraceContext(), event)
	if result != nil || err == nil || store.headCalls != 0 || len(eventStore.events) != 0 {
		t.Fatalf("result=%#v err=%v head_calls=%d events=%#v", result, err, store.headCalls, eventTypes(eventStore.events))
	}
}

func TestDefaultToolEventSinkDefaultsEmptyVisibilityToDebug(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
	event.Visibility = ""
	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit default visibility: %v", err)
	}
	if result.PersistedEvent.Visibility != observability.VisibilityDebug {
		t.Fatalf("visibility=%q", result.PersistedEvent.Visibility)
	}
}

func TestDefaultToolEventSinkOffloadsOversizedPayloadWithSafePreview(t *testing.T) {
	const maxPreviewBytes = 192
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{MaxPayloadBytes: 64, MaxPreviewBytes: maxPreviewBytes})
	event := baseProcessToolEvent(ToolEventProgress, mustJSONRaw(map[string]any{
		"message": strings.Repeat("large-safe-message", 20),
		"nested":  map[string]any{"password": "oversized-secret"},
	}))

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit oversized progress: %v", err)
	}
	persisted := result.PersistedEvent
	if persisted == nil || persisted.PayloadRef == "" || len(persisted.PayloadPreview) > maxPreviewBytes || !json.Valid(persisted.PayloadPreview) {
		t.Fatalf("oversized persisted event = %#v", persisted)
	}
	previewPayload := decodeEventPayload(t, *persisted)
	assertCanonicalToolPayload(t, previewPayload)
	if previewPayload["truncated"] != true {
		t.Fatalf("oversized preview lacks truncation marker: %#v", previewPayload)
	}
	if strings.Contains(string(persisted.PayloadPreview), "oversized-secret") {
		t.Fatalf("oversized preview leaked secret: %s", persisted.PayloadPreview)
	}
	metas := listToolCallArtifacts(t, fixture.ctx, fixture.artifactStore)
	if len(metas) != 1 || metas[0].ArtifactRef != persisted.PayloadRef || metas[0].ArtifactType != artifact.ArtifactTypeDebugPayload || metas[0].Visibility != artifact.VisibilityInternal {
		t.Fatalf("oversized payload artifact = %#v event=%#v", metas, persisted)
	}
	content := readInternalArtifact(t, fixture.ctx, fixture.artifactStore, persisted.PayloadRef)
	if strings.Contains(string(content), "oversized-secret") || !strings.Contains(string(content), "[REDACTED]") {
		t.Fatalf("oversized artifact content is unsafe: %s", content)
	}
}

func TestDefaultToolEventSinkRejectsPreviewBoundTooSmallForCanonicalIdentity(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{MaxPayloadBytes: 4096, MaxPreviewBytes: 32})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
		t.Fatalf("result=%#v err=%v events=%#v", result, err, eventTypes(fixture.eventStore.events))
	}
}

func TestDefaultToolEventSinkRedactsNestedSecrets(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working","nested":{"password":"secret","items":[{"api_token":"array-secret"}]}}`))
	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit nested secrets: %v", err)
	}
	preview := string(result.PersistedEvent.PayloadPreview)
	if containsAny(preview, "secret", "array-secret") || !strings.Contains(preview, "[REDACTED]") {
		t.Fatalf("unsafe preview: %s", preview)
	}
}

func TestDefaultToolEventSinkRejectsUnknownToolEventType(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventType("tool_private_event"), json.RawMessage(`{"message":"private"}`))
	result, err := fixture.sink.Emit(fixture.ctx, event)
	if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
		t.Fatalf("result=%#v err=%v events=%#v", result, err, eventTypes(fixture.eventStore.events))
	}
}

func TestDefaultToolEventSinkRejectsGatewayOwnedLifecycleEvent(t *testing.T) {
	for _, eventType := range []ToolEventType{
		ToolEventType(observability.EventToolCallStarted),
		ToolEventType(observability.EventToolCallCompleted),
		ToolEventType(observability.EventToolCallFailed),
	} {
		t.Run(string(eventType), func(t *testing.T) {
			fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
			event := baseProcessToolEvent(eventType, json.RawMessage(`{"message":"forged lifecycle"}`))
			result, err := fixture.sink.Emit(fixture.ctx, event)
			if result != nil || err == nil || len(fixture.eventStore.events) != 0 {
				t.Fatalf("result=%#v err=%v events=%#v", result, err, eventTypes(fixture.eventStore.events))
			}
		})
	}
}

func TestDefaultToolEventSinkRejectsInvalidArtifactMetadata(t *testing.T) {
	valid := validToolEventArtifactMeta()
	tests := []struct {
		name      string
		requested string
		mutate    func(*artifact.ArtifactMeta)
	}{
		{name: "unknown type", mutate: func(meta *artifact.ArtifactMeta) { meta.ArtifactType = artifact.ArtifactType("private_type") }},
		{name: "unknown visibility", mutate: func(meta *artifact.ArtifactMeta) { meta.Visibility = artifact.Visibility("private") }},
		{name: "empty mime", mutate: func(meta *artifact.ArtifactMeta) { meta.MimeType = "" }},
		{name: "empty hash", mutate: func(meta *artifact.ArtifactMeta) { meta.Hash = "" }},
		{name: "negative size", mutate: func(meta *artifact.ArtifactMeta) { meta.SizeBytes = -1 }},
		{name: "requested meta ref mismatch", requested: "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_other"},
		{name: "parsed ref scope mismatch", mutate: func(meta *artifact.ArtifactMeta) {
			meta.ArtifactRef = "artifact://tenants/tenant-b/sessions/sess-1/runs/run-1/art_authoritative"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := *valid
			if tt.mutate != nil {
				tt.mutate(&meta)
			}
			requested := tt.requested
			if requested == "" {
				requested = meta.ArtifactRef
			}
			eventStore := &recordingEventStore{}
			sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, ArtifactStore: headOverrideArtifactStore{meta: &meta}})
			event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+requested+`"}`))
			event.PayloadRef = requested
			result, err := sink.Emit(testTraceContext(), event)
			if result != nil || err == nil || len(eventStore.events) != 0 {
				t.Fatalf("result=%#v err=%v events=%#v meta=%#v", result, err, eventTypes(eventStore.events), meta)
			}
		})
	}
}

func TestDefaultToolEventSinkPreservesAuthoritativeArtifactOwnerMetadata(t *testing.T) {
	meta := validToolEventArtifactMeta()
	meta.StepID = "source-step"
	meta.OwnerModule = artifact.OwnerModuleContextEngine
	meta.OwnerID = "context-snapshot-owner"
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, ArtifactStore: headOverrideArtifactStore{meta: meta}})
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+meta.ArtifactRef+`"}`))
	event.PayloadRef = meta.ArtifactRef
	event.Visibility = observability.VisibilityInternal

	result, err := sink.Emit(testTraceContext(), event)
	if err != nil {
		t.Fatalf("emit artifact owner metadata: %v", err)
	}
	payload := decodeEventPayload(t, *result.PersistedEvent)
	if payload["step_id"] != meta.StepID || payload["owner_module"] != string(meta.OwnerModule) || payload["owner_id"] != meta.OwnerID {
		t.Fatalf("artifact owner metadata = %#v, meta=%#v", payload, meta)
	}
}

func TestDefaultToolEventSinkIgnoresUntrustedPayloadPreview(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"tool_call_id":"forged-payload","message":"working","password":"payload-secret"}`))
	event.PayloadPreview = json.RawMessage(`{"tool_call_id":"forged-preview","message":"forged","password":"preview-secret"}`)

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit untrusted preview event: %v", err)
	}
	preview := string(result.PersistedEvent.PayloadPreview)
	if containsAny(preview, "forged-payload", "forged-preview", "payload-secret", "preview-secret") || !strings.Contains(preview, "[REDACTED]") {
		t.Fatalf("untrusted preview bypassed sanitizer: %s", preview)
	}
	payload := decodeEventPayload(t, *result.PersistedEvent)
	assertCanonicalToolPayload(t, payload)
}

func TestDefaultToolEventSinkStripsGatewayPrivateOuterFields(t *testing.T) {
	for _, tt := range []struct {
		name      string
		eventType ToolEventType
		payload   json.RawMessage
	}{
		{name: "progress", eventType: ToolEventProgress, payload: json.RawMessage(`{"message":"working"}`)},
		{name: "warning", eventType: ToolEventWarning, payload: json.RawMessage(`{"code":"DEGRADED","message":"using cache"}`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
			event := baseProcessToolEvent(tt.eventType, tt.payload)
			event.PayloadRef = "artifact://tenants/tenant-other/sessions/sess-other/runs/run-other/art_payload"
			event.DebugRef = "artifact://tenants/tenant-other/sessions/sess-other/runs/run-other/art_debug"
			event.IdempotencyKey = "run-1:tc-1:completed"
			event.Error = &observability.EventError{Code: "FORGED", Type: "forged", Message: "outer-error-secret"}

			result, err := fixture.sink.Emit(fixture.ctx, event)
			if err != nil {
				t.Fatalf("emit %s: %v", tt.name, err)
			}
			persisted := result.PersistedEvent
			if persisted == nil || persisted.PayloadRef != "" || persisted.DebugRef != "" || persisted.Error != nil {
				t.Fatalf("unsafe outer fields persisted: %#v", persisted)
			}
			if persisted.IdempotencyKey == "" || persisted.IdempotencyKey == event.IdempotencyKey {
				t.Fatalf("untrusted idempotency key persisted: %#v", persisted)
			}
			if strings.Contains(string(persisted.PayloadPreview), "outer-error-secret") {
				t.Fatalf("outer error leaked into preview: %s", persisted.PayloadPreview)
			}
		})
	}

	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	meta := putToolEventArtifact(t, fixture.artifactStore, fixture.ctx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", StepID: "source-step",
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: "source-owner", ArtifactType: artifact.ArtifactTypeFile,
		MimeType: "text/plain", Visibility: artifact.VisibilityUserVisible, Content: strings.NewReader("safe artifact"), RetentionPolicy: artifact.RetentionRunTTL,
	})
	event := baseProcessToolEvent(ToolEventArtifactCreated, json.RawMessage(`{"artifact_ref":"`+meta.ArtifactRef+`"}`))
	event.PayloadRef = meta.ArtifactRef
	event.DebugRef = "artifact://tenants/tenant-other/sessions/sess-other/runs/run-other/art_debug"
	event.IdempotencyKey = "run-1:tc-1:completed"
	event.Error = &observability.EventError{Code: "FORGED", Type: "forged", Message: "artifact-error-secret"}

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit artifact: %v", err)
	}
	persisted := result.PersistedEvent
	if persisted == nil || persisted.PayloadRef != meta.ArtifactRef || persisted.DebugRef != "" || persisted.Error != nil {
		t.Fatalf("unsafe artifact outer fields persisted: %#v", persisted)
	}
	if persisted.IdempotencyKey == "" || persisted.IdempotencyKey == event.IdempotencyKey {
		t.Fatalf("untrusted artifact idempotency key persisted: %#v", persisted)
	}
}

func TestDefaultToolEventSinkAcceptsDomainDebugRefWithoutPayloadRawRef(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	meta := putToolEventArtifact(t, fixture.artifactStore, fixture.ctx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", StepID: "debug-step",
		OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tc-1", ArtifactType: artifact.ArtifactTypeDebugPayload,
		MimeType: "application/json", Visibility: artifact.VisibilityDebug, Content: strings.NewReader(`{"wire":"raw"}`), RetentionPolicy: artifact.RetentionDebugShortTTL,
	})
	event := baseProcessToolEvent(ToolEventDebug, nil)
	event.Visibility = observability.VisibilityDebug
	event.DebugRef = meta.ArtifactRef

	result, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit domain debug ref: %v", err)
	}
	if result == nil || result.Reason != "debug_artifact_only" || result.PersistedEvent != nil {
		t.Fatalf("domain debug-ref result = %#v", result)
	}
	if got := listToolCallArtifacts(t, fixture.ctx, fixture.artifactStore); len(got) != 1 {
		t.Fatalf("domain debug ref should be reused, artifacts=%#v", got)
	}
	if len(fixture.eventStore.events) != 0 {
		t.Fatalf("domain debug ref persisted AgentEvent")
	}
}

func TestDefaultToolEventSinkRejectsGeneratedArtifactOwnershipMismatch(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*artifact.ArtifactMeta)
	}{
		{name: "step", mutate: func(meta *artifact.ArtifactMeta) { meta.StepID = "wrong-step" }},
		{name: "owner module", mutate: func(meta *artifact.ArtifactMeta) { meta.OwnerModule = artifact.OwnerModuleRuntime }},
		{name: "owner id", mutate: func(meta *artifact.ArtifactMeta) { meta.OwnerID = "wrong-owner" }},
	}
	for _, path := range []string{"debug", "offload"} {
		for _, mutation := range mutations {
			t.Run(path+"/"+mutation.name, func(t *testing.T) {
				store, ctx := newScopedArtifactStore(t)
				eventStore := &recordingEventStore{}
				sink := NewDefaultToolEventSink(ToolEventSinkConfig{
					EventStore: eventStore,
					ArtifactStore: putArtifactMetaOverrideStore{
						ArtifactStore: store,
						mutate:        mutation.mutate,
					},
					IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
					Policy: ToolEventPolicy{MaxPayloadBytes: 64, MaxPreviewBytes: 48},
				})
				var event ToolEvent
				if path == "debug" {
					event = baseProcessToolEvent(ToolEventDebug, json.RawMessage(`{"message":"wire trace"}`))
					event.Visibility = observability.VisibilityDebug
				} else {
					event = baseProcessToolEvent(ToolEventProgress, mustJSONRaw(map[string]any{"message": strings.Repeat("large event", 40)}))
				}

				result, err := sink.Emit(ctx, event)
				if result != nil || err == nil || len(eventStore.events) != 0 {
					t.Fatalf("path=%s mutation=%s result=%#v err=%v events=%#v", path, mutation.name, result, err, eventTypes(eventStore.events))
				}
			})
		}
	}
}

func TestDefaultToolEventSinkDeduplicatesExternalEventID(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
	event.ExternalEventID = "external-progress-1"

	first, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit first external event: %v", err)
	}
	second, err := fixture.sink.Emit(fixture.ctx, event)
	if err != nil {
		t.Fatalf("emit duplicate external event: %v", err)
	}
	if first == second || first.PersistedEvent == second.PersistedEvent {
		t.Fatalf("dedupe result must be copy-safe: first=%p/%p second=%p/%p", first, first.PersistedEvent, second, second.PersistedEvent)
	}
	if first.PersistedEvent.EventID != second.PersistedEvent.EventID || len(fixture.eventStore.events) != 1 {
		t.Fatalf("dedupe first=%#v second=%#v events=%#v", first, second, fixture.eventStore.events)
	}
}

func TestDefaultToolEventSinkDoesNotCacheFailedExternalEventID(t *testing.T) {
	eventStore := &failOnceToolEventStore{err: errors.New("temporary append failure")}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, IDGenerator: fixedIDGenerator{}})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
	event.ExternalEventID = "external-retry-1"

	if result, err := sink.Emit(testTraceContext(), event); result != nil || err == nil {
		t.Fatalf("first result=%#v err=%v", result, err)
	}
	result, err := sink.Emit(testTraceContext(), event)
	if err != nil || result == nil || result.PersistedEvent == nil {
		t.Fatalf("retry result=%#v err=%v", result, err)
	}
	if eventStore.calls != 2 || len(eventStore.events) != 1 {
		t.Fatalf("calls=%d events=%#v", eventStore.calls, eventTypes(eventStore.events))
	}
}

func TestDefaultToolEventSinkScopesExternalEventIDDeduplication(t *testing.T) {
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: eventStore, IDGenerator: fixedIDGenerator{}})
	eventA := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"tenant a"}`))
	eventA.ExternalEventID = "shared-external-id"
	if _, err := sink.Emit(testTraceContext(), eventA); err != nil {
		t.Fatalf("emit scope a: %v", err)
	}

	eventB := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"tenant b"}`))
	eventB.ExternalEventID = eventA.ExternalEventID
	eventB.TenantID = "tenant-b"
	eventB.UserID = "user-b"
	eventB.TraceID = "trace-b"
	eventB.SessionID = "sess-b"
	eventB.RunID = "run-b"
	eventB.StepID = "step-b"
	eventB.AgentID = "agent-b"
	eventB.ToolCallID = "tc-b"
	ctxB := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace-b", TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b", RunID: "run-b", AgentID: "agent-b",
	})
	if _, err := sink.Emit(ctxB, eventB); err != nil {
		t.Fatalf("emit scope b: %v", err)
	}
	if len(eventStore.events) != 2 {
		t.Fatalf("cross-scope external ID deduped events=%#v", eventTypes(eventStore.events))
	}
}

func TestDefaultToolEventSinkDuplicateExternalEventIDDoesNotRepeatArtifactWrite(t *testing.T) {
	for _, eventType := range []ToolEventType{ToolEventDebug, ToolEventProgress} {
		t.Run(string(eventType), func(t *testing.T) {
			baseStore, ctx := newScopedArtifactStore(t)
			store := &countingPutArtifactStore{ArtifactStore: baseStore}
			eventStore := &recordingEventStore{}
			sink := NewDefaultToolEventSink(ToolEventSinkConfig{
				EventStore: eventStore, ArtifactStore: store, IDGenerator: fixedIDGenerator{},
				Policy: ToolEventPolicy{MaxPayloadBytes: 64, MaxPreviewBytes: 192},
			})
			var event ToolEvent
			if eventType == ToolEventDebug {
				event = baseProcessToolEvent(ToolEventDebug, json.RawMessage(`{"message":"wire trace"}`))
				event.Visibility = observability.VisibilityDebug
			} else {
				event = baseProcessToolEvent(ToolEventProgress, mustJSONRaw(map[string]any{"message": strings.Repeat("large event", 40)}))
			}
			event.ExternalEventID = "external-artifact-1"
			if _, err := sink.Emit(ctx, event); err != nil {
				t.Fatalf("first emit: %v", err)
			}
			if _, err := sink.Emit(ctx, event); err != nil {
				t.Fatalf("duplicate emit: %v", err)
			}
			if calls := store.PutCalls(); calls != 1 {
				t.Fatalf("artifact puts=%d, want 1", calls)
			}
		})
	}
}

func TestDefaultToolEventSinkRejectsInvalidPersistedEventAcknowledgement(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*observability.AgentEvent)
	}{
		{name: "sequence", mutate: func(event *observability.AgentEvent) { event.Sequence = 0 }},
		{name: "schema", mutate: func(event *observability.AgentEvent) { event.SchemaVersion = "private.event.v1" }},
		{name: "type", mutate: func(event *observability.AgentEvent) { event.EventType = observability.EventToolCallCompleted }},
		{name: "scope", mutate: func(event *observability.AgentEvent) { event.RunID = "run-other" }},
		{name: "visibility", mutate: func(event *observability.AgentEvent) { event.Visibility = observability.VisibilityRestricted }},
		{name: "payload ref", mutate: func(event *observability.AgentEvent) { event.PayloadRef = "artifact://forged" }},
		{name: "idempotency", mutate: func(event *observability.AgentEvent) { event.IdempotencyKey = "forged" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mutatingAckToolEventStore{mutate: tt.mutate}
			sink := NewDefaultToolEventSink(ToolEventSinkConfig{EventStore: store, IDGenerator: fixedIDGenerator{}})
			event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
			result, err := sink.Emit(testTraceContext(), event)
			if result != nil || err == nil || store.calls != 1 {
				t.Fatalf("result=%#v err=%v calls=%d", result, err, store.calls)
			}
		})
	}
}

func TestDefaultToolEventSinkConcurrentDuplicateExternalEventID(t *testing.T) {
	fixture := newToolEventSinkFixture(t, ToolEventPolicy{})
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
	event.ExternalEventID = "external-concurrent-1"
	const goroutines = 16
	results := make(chan *ToolEventEmitResult, goroutines)
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := fixture.sink.Emit(fixture.ctx, event)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent duplicate: %v", err)
		}
	}
	eventID := ""
	for result := range results {
		if result == nil || result.PersistedEvent == nil {
			t.Fatalf("concurrent result = %#v", result)
		}
		if eventID == "" {
			eventID = result.PersistedEvent.EventID
		} else if result.PersistedEvent.EventID != eventID {
			t.Fatalf("concurrent event ID = %q, want %q", result.PersistedEvent.EventID, eventID)
		}
	}
	if len(fixture.eventStore.events) != 1 {
		t.Fatalf("concurrent duplicate appended %d events", len(fixture.eventStore.events))
	}
}

func TestDefaultToolEventSinkBoundsCompletedDedupeEntries(t *testing.T) {
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{
		EventStore: eventStore, IDGenerator: fixedIDGenerator{}, MaxDedupeEntries: 2,
	})
	for _, externalID := range []string{"external-1", "external-2", "external-3"} {
		event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
		event.ExternalEventID = externalID
		if _, err := sink.Emit(testTraceContext(), event); err != nil {
			t.Fatalf("emit %s: %v", externalID, err)
		}
	}
	sink.dedupeMu.Lock()
	entries := len(sink.deduped)
	order := len(sink.dedupeOrder)
	sink.dedupeMu.Unlock()
	if entries != 2 || order != 2 {
		t.Fatalf("bounded cache entries=%d order=%d", entries, order)
	}
	event := baseProcessToolEvent(ToolEventProgress, json.RawMessage(`{"message":"working"}`))
	event.ExternalEventID = "external-1"
	if _, err := sink.Emit(testTraceContext(), event); err != nil {
		t.Fatalf("emit evicted key: %v", err)
	}
	if len(eventStore.events) != 4 {
		t.Fatalf("evicted key did not execute again, events=%d", len(eventStore.events))
	}
}

type toolEventSinkFixture struct {
	ctx           context.Context
	eventStore    *recordingEventStore
	artifactStore artifact.ArtifactStore
	sink          *DefaultToolEventSink
}

func newToolEventSinkFixture(t *testing.T, policy ToolEventPolicy) toolEventSinkFixture {
	t.Helper()
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	sink := NewDefaultToolEventSink(ToolEventSinkConfig{
		EventStore: eventStore, ArtifactStore: artifactStore,
		IDGenerator: fixedIDGenerator{}, Clock: fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
		Logger: observability.NoopLogger{}, Policy: policy,
	})
	return toolEventSinkFixture{ctx: ctx, eventStore: eventStore, artifactStore: artifactStore, sink: sink}
}

func baseProcessToolEvent(eventType ToolEventType, payload json.RawMessage) ToolEvent {
	return ToolEvent{
		ToolEventID: "te-1", EventType: eventType,
		TenantID: "tenant-a", UserID: "user-a", TraceID: "trace-1", SpanID: "span-tool",
		SessionID: "sess-1", RunID: "run-1", StepID: "step-1", AgentID: "agent-a",
		ToolCallID: "tc-1", ToolName: "search_kb", ToolVersion: "v1", ToolType: ToolTypeFunction,
		Visibility: observability.VisibilityUserVisible, Payload: payload,
		CreatedAt: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC),
	}
}

func assertCanonicalToolPayload(t *testing.T, payload map[string]any) {
	t.Helper()
	if payload["tool_call_id"] != "tc-1" || payload["tool_name"] != "search_kb" || payload["tool_version"] != "v1" || payload["tool_type"] != "function" {
		t.Fatalf("canonical tool identity = %#v", payload)
	}
}

func assertAuthoritativeArtifactPayload(t *testing.T, payload map[string]any, meta *artifact.ArtifactMeta) {
	t.Helper()
	if payload["artifact_ref"] != meta.ArtifactRef || payload["artifact_type"] != string(meta.ArtifactType) || payload["mime_type"] != meta.MimeType || payload["hash"] != meta.Hash || payload["size_bytes"] != float64(meta.SizeBytes) || payload["visibility"] != string(meta.Visibility) {
		t.Fatalf("artifact payload=%#v meta=%#v", payload, meta)
	}
}

func validToolEventArtifactMeta() *artifact.ArtifactMeta {
	return &artifact.ArtifactMeta{
		ArtifactRef: "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_authoritative",
		TenantID:    "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", StepID: "source-step",
		OwnerModule: artifact.OwnerModuleContextEngine, OwnerID: "source-owner",
		ArtifactType: artifact.ArtifactTypeFile, MimeType: "application/vnd.authoritative+json", SizeBytes: 123,
		Hash: "sha256:authoritative", Visibility: artifact.VisibilityInternal, RetentionPolicy: artifact.RetentionRunTTL,
	}
}

func putToolEventArtifact(t *testing.T, store artifact.ArtifactStore, ctx context.Context, req artifact.PutArtifactRequest) *artifact.ArtifactMeta {
	t.Helper()
	meta, err := store.Put(ctx, req)
	if err != nil {
		t.Fatalf("put tool event artifact: %v", err)
	}
	return meta
}

func listToolCallArtifacts(t *testing.T, ctx context.Context, store artifact.ArtifactStore) []artifact.ArtifactMeta {
	t.Helper()
	inspectionCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", Role: artifact.ActorDebug,
	})
	metas, err := store.List(inspectionCtx, artifact.ListQuery{
		TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tc-1",
	})
	if err != nil {
		t.Fatalf("list tool-call artifacts: %v", err)
	}
	return metas
}

func readDebugArtifact(t *testing.T, ctx context.Context, store artifact.ArtifactStore, ref string) []byte {
	t.Helper()
	object, err := store.Get(ctx, ref, artifact.GetOptions{Purpose: artifact.PurposeDebug})
	if err != nil {
		t.Fatalf("get debug artifact: %v", err)
	}
	return readAndCloseArtifact(t, object)
}

func readInternalArtifact(t *testing.T, ctx context.Context, store artifact.ArtifactStore, ref string) []byte {
	t.Helper()
	object, err := store.Get(ctx, ref, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		t.Fatalf("get internal artifact: %v", err)
	}
	return readAndCloseArtifact(t, object)
}

func readAndCloseArtifact(t *testing.T, object *artifact.ArtifactObject) []byte {
	t.Helper()
	if object == nil || object.Content == nil {
		t.Fatalf("artifact object is empty: %#v", object)
	}
	content, readErr := io.ReadAll(object.Content)
	closeErr := object.Content.Close()
	if readErr != nil {
		t.Fatalf("read artifact: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close artifact: %v", closeErr)
	}
	return content
}

func mustJSONRaw(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

type failingToolEventSink struct {
	err error
}

func (s failingToolEventSink) Emit(context.Context, ToolEvent) (*ToolEventEmitResult, error) {
	return nil, s.err
}

var _ ToolEventSink = failingToolEventSink{err: errors.New("sink unavailable")}

type putArtifactMetaOverrideStore struct {
	artifact.ArtifactStore
	mutate func(*artifact.ArtifactMeta)
}

type countingHeadArtifactStore struct {
	artifact.ArtifactStore
	headCalls int
	err       error
}

func (s *countingHeadArtifactStore) Head(context.Context, string) (*artifact.ArtifactMeta, error) {
	s.headCalls++
	return nil, s.err
}

type countingPutArtifactStore struct {
	artifact.ArtifactStore
	mu    sync.Mutex
	calls int
}

func (s *countingPutArtifactStore) Put(ctx context.Context, req artifact.PutArtifactRequest) (*artifact.ArtifactMeta, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.ArtifactStore.Put(ctx, req)
}

func (s *countingPutArtifactStore) PutCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type failOnceToolEventStore struct {
	err    error
	calls  int
	events []observability.AgentEvent
}

func (s *failOnceToolEventStore) AppendEvent(_ context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	s.calls++
	if s.calls == 1 {
		return nil, s.err
	}
	event.Sequence = int64(len(s.events) + 1)
	s.events = append(s.events, cloneAgentEvent(event))
	return &EventAppendResult{Event: cloneAgentEvent(event)}, nil
}

type mutatingAckToolEventStore struct {
	mutate func(*observability.AgentEvent)
	calls  int
}

func (s *mutatingAckToolEventStore) AppendEvent(_ context.Context, event observability.AgentEvent) (*EventAppendResult, error) {
	s.calls++
	event.Sequence = 1
	if s.mutate != nil {
		s.mutate(&event)
	}
	return &EventAppendResult{Event: event}, nil
}

func (s putArtifactMetaOverrideStore) Put(ctx context.Context, req artifact.PutArtifactRequest) (*artifact.ArtifactMeta, error) {
	meta, err := s.ArtifactStore.Put(ctx, req)
	if err != nil || meta == nil {
		return meta, err
	}
	cloned := *meta
	if s.mutate != nil {
		s.mutate(&cloned)
	}
	return &cloned, nil
}
