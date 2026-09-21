package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestToolContextEmitsTypedEventsWithInheritedIdentity(t *testing.T) {
	sink := &recordingToolContextSink{}
	toolCtx, _, _, _ := newToolContextUnitFixture(t, sink, observability.NoopLogger{})
	percent := 40
	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{
		Stage: "search", Message: "working", Percent: &percent,
		Data: json.RawMessage(`{"batch":1}`),
	}); err != nil {
		t.Fatalf("emit progress: %v", err)
	}
	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{
		Message: "internal", Visibility: observability.VisibilityInternal,
	}); err != nil {
		t.Fatalf("emit explicit progress visibility: %v", err)
	}
	if err := toolCtx.EmitWarning(context.Background(), ToolWarningEvent{
		Code: "DEGRADED", Message: "using cache", Retryable: true,
	}); err != nil {
		t.Fatalf("emit warning: %v", err)
	}
	debugRef := "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_debug"
	if err := toolCtx.EmitDebug(context.Background(), ToolDebugEvent{
		Message: "wire trace", Data: json.RawMessage(`{"safe":true}`), RawRef: debugRef,
	}); err != nil {
		t.Fatalf("emit debug: %v", err)
	}
	artifactRef := "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_result"
	if err := toolCtx.EmitArtifact(context.Background(), ToolArtifactEvent{
		ArtifactRef: artifactRef, MimeType: "text/plain", SizeBytes: 12, Hash: "sha256:claimed",
		Preview: json.RawMessage(`{"text":"claimed"}`),
	}); err != nil {
		t.Fatalf("emit artifact: %v", err)
	}

	events := sink.Events()
	if len(events) != 5 {
		t.Fatalf("emitted events=%d, want 5", len(events))
	}
	for i := range events {
		assertInheritedToolEventIdentity(t, events[i])
	}
	if events[0].EventType != ToolEventProgress || events[0].Visibility != observability.VisibilityUserVisible {
		t.Fatalf("default progress event=%#v", events[0])
	}
	progress := decodeToolDomainPayload(t, events[0].Payload)
	if progress["stage"] != "search" || progress["message"] != "working" || progress["percent"] != float64(40) {
		t.Fatalf("progress payload=%#v", progress)
	}
	if data, ok := progress["data"].(map[string]any); !ok || data["batch"] != float64(1) {
		t.Fatalf("progress data=%#v", progress["data"])
	}
	if events[1].EventType != ToolEventProgress || events[1].Visibility != observability.VisibilityInternal {
		t.Fatalf("explicit progress event=%#v", events[1])
	}
	if events[2].EventType != ToolEventWarning || events[2].Visibility != observability.VisibilityUserVisible {
		t.Fatalf("warning event=%#v", events[2])
	}
	warning := decodeToolDomainPayload(t, events[2].Payload)
	if warning["code"] != "DEGRADED" || warning["message"] != "using cache" || warning["retryable"] != true {
		t.Fatalf("warning payload=%#v", warning)
	}
	if events[3].EventType != ToolEventDebug || events[3].Visibility != observability.VisibilityDebug || events[3].DebugRef != debugRef {
		t.Fatalf("debug event=%#v", events[3])
	}
	debugPayload := decodeToolDomainPayload(t, events[3].Payload)
	if debugPayload["raw_ref"] != debugRef || debugPayload["message"] != "wire trace" {
		t.Fatalf("debug payload=%#v", debugPayload)
	}
	if events[4].EventType != ToolEventArtifactCreated || events[4].Visibility != "" || events[4].PayloadRef != artifactRef {
		t.Fatalf("artifact event=%#v", events[4])
	}
	artifactPayload := decodeToolDomainPayload(t, events[4].Payload)
	if artifactPayload["artifact_ref"] != artifactRef || artifactPayload["mime_type"] != "text/plain" || artifactPayload["size_bytes"] != float64(12) || artifactPayload["hash"] != "sha256:claimed" {
		t.Fatalf("artifact payload=%#v", artifactPayload)
	}
}

func TestToolContextCancellationAndTrace(t *testing.T) {
	execCtx, cancel := context.WithCancel(testTraceContext())
	tc := observability.MustTraceContext(execCtx)
	tc.Baggage = map[string]string{"request_kind": "tool"}
	execCtx = observability.WithTraceContext(execCtx, tc)
	def := defaultSearchDefinition()
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	toolCtx := newDefaultToolContext(execCtx, tc, req, &def, &recordingToolContextSink{}, observability.NoopLogger{})

	if toolCtx.IsCancelled() {
		t.Fatalf("context unexpectedly cancelled")
	}
	trace := toolCtx.Trace()
	if !reflect.DeepEqual(trace, tc) {
		t.Fatalf("trace=%#v, want %#v", trace, tc)
	}
	trace.Baggage["request_kind"] = "mutated"
	if got := toolCtx.Trace().Baggage["request_kind"]; got != "tool" {
		t.Fatalf("trace baggage is not copy-safe: %q", got)
	}
	cancel()
	if !toolCtx.IsCancelled() {
		t.Fatalf("cancelled execution context not reflected")
	}
}

func TestToolContextProgressAndWarningSinkErrorsDegrade(t *testing.T) {
	sinkErr := errors.New("sink-private-error")
	logger := &recordingToolContextLogger{}
	toolCtx, _, _, _ := newToolContextUnitFixture(t, failingToolEventSink{err: sinkErr}, logger)

	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{Message: "payload-private"}); err != nil {
		t.Fatalf("progress should degrade: %v", err)
	}
	if err := toolCtx.EmitWarning(context.Background(), ToolWarningEvent{Code: "WARN", Message: "payload-private"}); err != nil {
		t.Fatalf("warning should degrade: %v", err)
	}
	entries := logger.Entries()
	if len(entries) != 2 || entries[0].level != "warn" || entries[1].level != "warn" {
		t.Fatalf("degradation logs=%#v", entries)
	}
}

func TestToolContextLogsStructuredProgressDegradation(t *testing.T) {
	logger := &recordingToolContextLogger{}
	toolCtx, _, _, _ := newToolContextUnitFixture(t, failingToolEventSink{err: errors.New("sink-secret")}, logger)
	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{Message: "payload-secret"}); err != nil {
		t.Fatalf("progress should degrade: %v", err)
	}

	entries := logger.Entries()
	if len(entries) != 1 {
		t.Fatalf("degradation logs=%#v", entries)
	}
	fields := toolContextLogFields(entries[0])
	want := map[string]string{
		"fallback_type": "tool_event_degraded",
		"tool_call_id":  "tc-1",
		"from":          "tool_event_sink",
		"to":            "structured_log",
		"reason":        "sink_error",
		"impact":        "process_event_replay_unavailable_p0_lifecycle_preserved",
	}
	for key, value := range want {
		if fields[key] != value {
			t.Fatalf("field %s=%q, want %q; fields=%#v", key, fields[key], value, fields)
		}
	}
	logText := toolContextLogText(entries[0])
	if strings.Contains(logText, "payload-secret") || strings.Contains(logText, "sink-secret") {
		t.Fatalf("unsafe degradation log: %s", logText)
	}
}

func TestToolContextMissingSinkDegradesProgressAndWarning(t *testing.T) {
	logger := &recordingToolContextLogger{}
	toolCtx, _, _, _ := newToolContextUnitFixture(t, nil, logger)
	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{Message: "working"}); err != nil {
		t.Fatalf("missing progress sink should degrade: %v", err)
	}
	if err := toolCtx.EmitWarning(context.Background(), ToolWarningEvent{Code: "WARN", Message: "warning"}); err != nil {
		t.Fatalf("missing warning sink should degrade: %v", err)
	}
	entries := logger.Entries()
	if len(entries) != 2 {
		t.Fatalf("missing-sink logs=%#v", entries)
	}
	for _, entry := range entries {
		if got := toolContextLogFields(entry)["reason"]; got != "sink_unavailable" {
			t.Fatalf("reason=%q", got)
		}
	}
}

func TestToolContextReturnsDebugPersistenceError(t *testing.T) {
	sinkErr := errors.New("debug persistence unavailable")
	toolCtx, _, _, _ := newToolContextUnitFixture(t, failingToolEventSink{err: sinkErr}, observability.NoopLogger{})
	if err := toolCtx.EmitDebug(context.Background(), ToolDebugEvent{Message: "debug"}); !errors.Is(err, sinkErr) {
		t.Fatalf("debug error=%v, want %v", err, sinkErr)
	}
}

func TestToolContextReturnsArtifactPersistenceError(t *testing.T) {
	sinkErr := errors.New("artifact persistence unavailable")
	toolCtx, _, _, _ := newToolContextUnitFixture(t, failingToolEventSink{err: sinkErr}, observability.NoopLogger{})
	if err := toolCtx.EmitArtifact(context.Background(), ToolArtifactEvent{ArtifactRef: "artifact://scoped"}); !errors.Is(err, sinkErr) {
		t.Fatalf("artifact error=%v, want %v", err, sinkErr)
	}
}

func TestToolContextCollectsPersistedEventsInOrderAndCopySafe(t *testing.T) {
	first := &observability.AgentEvent{EventID: "evt-process-1", Sequence: 2, EventType: observability.EventToolCallProgress, PayloadPreview: json.RawMessage(`{"message":"one"}`)}
	second := &observability.AgentEvent{EventID: "evt-process-2", Sequence: 3, EventType: observability.EventToolCallProgress, PayloadPreview: json.RawMessage(`{"message":"two"}`)}
	sink := &scriptedToolContextSink{results: []*ToolEventEmitResult{
		{PersistedEvent: first},
		{PersistedEvent: second},
		{Reason: "debug_artifact_only"},
	}}
	toolCtx, _, _, _ := newToolContextUnitFixture(t, sink, observability.NoopLogger{})
	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{Message: "one"}); err != nil {
		t.Fatalf("emit first: %v", err)
	}
	if err := toolCtx.EmitProgress(context.Background(), ToolProgressEvent{Message: "two"}); err != nil {
		t.Fatalf("emit second: %v", err)
	}
	if err := toolCtx.EmitDebug(context.Background(), ToolDebugEvent{Message: "debug"}); err != nil {
		t.Fatalf("emit debug: %v", err)
	}
	first.EventID = "mutated-sink-result"
	first.PayloadPreview[0] = '['

	events := toolCtx.persistedEvents()
	if len(events) != 2 || events[0].EventID != "evt-process-1" || events[1].EventID != "evt-process-2" || events[0].Sequence >= events[1].Sequence {
		t.Fatalf("persisted process events=%#v", events)
	}
	events[0].EventID = "mutated-snapshot"
	events[0].PayloadPreview[0] = '['
	again := toolCtx.persistedEvents()
	if again[0].EventID != "evt-process-1" || string(again[0].PayloadPreview) != `{"message":"one"}` {
		t.Fatalf("process event collection is not copy-safe: %#v", again)
	}
}

func TestFunctionExecutorDirectExecuteLeavesToolContextNil(t *testing.T) {
	def := defaultSearchDefinition()
	called := false
	executor := NewFunctionExecutor(map[string]FunctionTool{
		"search": func(_ context.Context, call FunctionCall) (*FunctionResult, error) {
			called = true
			if call.ToolContext != nil {
				t.Fatalf("direct Execute received ToolContext")
			}
			return &FunctionResult{Data: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	if _, err := executor.Execute(testTraceContext(), &def, baseToolRequest(json.RawMessage(`{"query":"hotel"}`))); err != nil {
		t.Fatalf("direct execute: %v", err)
	}
	if !called {
		t.Fatalf("function handler was not called")
	}
}

func newToolContextUnitFixture(
	t *testing.T,
	sink ToolEventSink,
	logger observability.StructuredLogger,
) (*defaultToolContext, context.Context, observability.TraceContext, ToolCallRequest) {
	t.Helper()
	execCtx := testTraceContext()
	tc := observability.MustTraceContext(execCtx)
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	def := defaultSearchDefinition()
	return newDefaultToolContext(execCtx, tc, req, &def, sink, logger), execCtx, tc, req
}

func assertInheritedToolEventIdentity(t *testing.T, event ToolEvent) {
	t.Helper()
	if event.TraceID != "trace-1" || event.TenantID != "tenant-a" || event.UserID != "user-a" || event.SessionID != "sess-1" || event.RunID != "run-1" || event.StepID != "step-1" || event.AgentID != "agent-a" {
		t.Fatalf("inherited execution identity=%#v", event)
	}
	if event.ToolCallID != "tc-1" || event.ToolName != "search_kb" || event.ToolVersion != "v1" || event.ToolType != ToolTypeFunction {
		t.Fatalf("inherited tool identity=%#v", event)
	}
}

func decodeToolDomainPayload(t *testing.T, payload json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode tool domain payload: %v; payload=%s", err, payload)
	}
	return decoded
}

type recordingToolContextSink struct {
	mu     sync.Mutex
	events []ToolEvent
}

func (s *recordingToolContextSink) Emit(_ context.Context, event ToolEvent) (*ToolEventEmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, cloneToolDomainEvent(event))
	return &ToolEventEmitResult{}, nil
}

func (s *recordingToolContextSink) Events() []ToolEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]ToolEvent, len(s.events))
	for i := range s.events {
		events[i] = cloneToolDomainEvent(s.events[i])
	}
	return events
}

type scriptedToolContextSink struct {
	mu      sync.Mutex
	results []*ToolEventEmitResult
	next    int
}

func (s *scriptedToolContextSink) Emit(context.Context, ToolEvent) (*ToolEventEmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.results) {
		return &ToolEventEmitResult{}, nil
	}
	result := s.results[s.next]
	s.next++
	return result, nil
}

func cloneToolDomainEvent(event ToolEvent) ToolEvent {
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	event.PayloadPreview = append(json.RawMessage(nil), event.PayloadPreview...)
	if event.Error != nil {
		eventError := *event.Error
		event.Error = &eventError
	}
	return event
}

type toolContextLogEntry struct {
	level   string
	message string
	fields  []observability.Field
}

type recordingToolContextLogger struct {
	mu      sync.Mutex
	entries []toolContextLogEntry
}

func (l *recordingToolContextLogger) Debug(_ context.Context, msg string, fields ...observability.Field) {
	l.record("debug", msg, fields)
}

func (l *recordingToolContextLogger) Info(_ context.Context, msg string, fields ...observability.Field) {
	l.record("info", msg, fields)
}

func (l *recordingToolContextLogger) Warn(_ context.Context, msg string, fields ...observability.Field) {
	l.record("warn", msg, fields)
}

func (l *recordingToolContextLogger) Error(_ context.Context, msg string, _ error, fields ...observability.Field) {
	l.record("error", msg, fields)
}

func (l *recordingToolContextLogger) With(...observability.Field) observability.StructuredLogger {
	return l
}

func (l *recordingToolContextLogger) Sync() {}

func (l *recordingToolContextLogger) record(level, message string, fields []observability.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, toolContextLogEntry{level: level, message: message, fields: append([]observability.Field(nil), fields...)})
}

func (l *recordingToolContextLogger) Entries() []toolContextLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := make([]toolContextLogEntry, len(l.entries))
	for i := range l.entries {
		entries[i] = toolContextLogEntry{
			level: l.entries[i].level, message: l.entries[i].message,
			fields: append([]observability.Field(nil), l.entries[i].fields...),
		}
	}
	return entries
}

func toolContextLogFields(entry toolContextLogEntry) map[string]string {
	fields := make(map[string]string, len(entry.fields))
	for _, field := range entry.fields {
		fields[field.Key] = field.String
	}
	return fields
}

func toolContextLogText(entry toolContextLogEntry) string {
	var text strings.Builder
	text.WriteString(entry.message)
	for _, field := range entry.fields {
		text.WriteString(field.Key)
		text.WriteString(field.String)
		text.WriteString(fmt.Sprint(field.Interface))
	}
	return text.String()
}
