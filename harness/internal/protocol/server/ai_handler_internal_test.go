package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagemem "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

type aiSDKPipeResponseWriter struct {
	header http.Header
	writer io.Writer
}

func (w *aiSDKPipeResponseWriter) Header() http.Header         { return w.header }
func (w *aiSDKPipeResponseWriter) WriteHeader(_ int)           {}
func (w *aiSDKPipeResponseWriter) Write(p []byte) (int, error) { return w.writer.Write(p) }
func (w *aiSDKPipeResponseWriter) Flush()                      {}

func TestEventTextFallsBackToPayloadAndRejectsEmptyDelta(t *testing.T) {
	if got := eventText(observability.AgentEvent{Payload: []byte(`{"text":"from-payload"}`)}); got != "from-payload" {
		t.Fatalf("payload fallback=%q", got)
	}
	if got := eventText(observability.AgentEvent{PayloadPreview: []byte(`{"preview":"from-preview"}`)}); got != "from-preview" {
		t.Fatalf("preview field fallback=%q", got)
	}
	if got := eventText(observability.AgentEvent{Payload: []byte(`{"content":"from-content"}`)}); got != "from-content" {
		t.Fatalf("content field fallback=%q", got)
	}
	if got := eventText(observability.AgentEvent{Payload: []byte(`{"message":"from-message"}`)}); got != "from-message" {
		t.Fatalf("message field fallback=%q", got)
	}
	if got := eventText(observability.AgentEvent{Payload: []byte(`{"text":""}`)}); got != "" {
		t.Fatalf("empty delta=%q", got)
	}
	if got := eventText(observability.AgentEvent{
		Payload: []byte(`{"text":"payload"}`), PayloadPreview: []byte(`{"text":"preview"}`),
	}); got != "preview" {
		t.Fatalf("preview precedence=%q", got)
	}
}

func TestStreamAISDKProjectsInteractiveEvents(t *testing.T) {
	stores := storagemem.New().Stores()
	deps := Deps{Stores: stores}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_ai_sdk"})
	runID := "run_ai_sdk_projection"
	appendEvent := func(eventType observability.EventType, payload string, payloadRef string) {
		t.Helper()
		_, err := stores.Events.Append(ctx, observability.AgentEvent{
			RunID: runID, EventType: eventType, Visibility: observability.VisibilityUserVisible,
			PayloadPreview: json.RawMessage(payload), PayloadRef: payloadRef,
		})
		if err != nil {
			t.Fatalf("append %s: %v", eventType, err)
		}
	}
	appendEvent(observability.EventToolCallStarted, `{"tool_call_id":"tool-1","tool_name":"lookup","arguments_preview":{"q":"roadmap"}}`, "")
	appendEvent(observability.EventToolCallProgress, `{"tool_call_id":"tool-1","tool_name":"lookup","status":"running","message":"searching"}`, "")
	appendEvent(observability.EventToolArtifactCreated, `{"tool_call_id":"tool-1","tool_name":"lookup","artifact_ref":"artifact://tool/result","artifact_type":"document"}`, "artifact://tool/result")
	appendEvent(observability.EventToolCallCompleted, `{"tool_call_id":"tool-1","tool_name":"lookup","result_preview":{"answer":"done"},"result_ref":"artifact://tool/result"}`, "artifact://tool/result")
	appendEvent(observability.EventControlRequestCreated, `{"request_id":"ctrl-1","type":"ask_user","control_ticket":"ticket","resume_token":"must-not-leak","interrupt_contexts":[{"id":"interrupt-1"}]}`, "")
	appendEvent(observability.EventControlResponseReceived, `{"request_id":"ctrl-1","status":"answered"}`, "")
	appendEvent(observability.EventSubAgentStarted, `{"child_run_id":"child-1","agent_id":"researcher"}`, "")
	appendEvent(observability.EventArtifactCreated, `{"artifact_ref":"artifact://run/final","artifact_type":"report"}`, "artifact://run/final")
	appendEvent(observability.EventRunCompleted, `{}`, "")

	rec := httptest.NewRecorder()
	writer, ok := protocol.NewAISDKWriter(rec)
	if !ok {
		t.Fatal("response recorder should support flushing")
	}
	deps.streamAISDK(ctx, writer, runID, nil)
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"tool-input-start"`,
		`"type":"tool-input-available"`,
		`"type":"data-tool-progress"`,
		`"type":"data-artifact"`,
		`"type":"tool-output-available"`,
		`"type":"data-control-request"`,
		`"type":"data-control-response"`,
		`"type":"data-sub-agent"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("AI SDK stream missing %s; body=%s", want, body)
		}
	}
	if strings.Contains(body, "resume_token") {
		t.Fatalf("AI SDK stream leaked resume_token: %s", body)
	}
}

func TestStreamAgentChatRecoversCanonicalFinalBeforeLiveTerminal(t *testing.T) {
	stores := storagemem.New().Stores()
	deps := Deps{Stores: stores}
	ctx, cancel := context.WithTimeout(
		observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_agent_chat_final"}),
		3*time.Second,
	)
	defer cancel()
	const sessionID = "session_agent_chat_final"
	const runID = "run_agent_chat_final"
	if err := stores.Sessions.Create(ctx, &storage.Session{ID: sessionID, Status: storage.SessionStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Runs.Create(ctx, &storage.Run{RunID: runID, SessionID: sessionID, Status: storage.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	appendEvent := func(eventType observability.EventType, payload string) observability.AgentEvent {
		t.Helper()
		result, err := stores.Events.Append(ctx, observability.AgentEvent{
			EventID: "event_" + string(eventType), SessionID: sessionID, RunID: runID,
			EventType: eventType, Visibility: observability.VisibilityUserVisible,
			Payload: json.RawMessage(payload), PayloadPreview: json.RawMessage(payload),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Event
	}
	appendEvent(observability.EventAgentStarted, `{}`)

	reader, writer := io.Pipe()
	response := &aiSDKPipeResponseWriter{header: make(http.Header), writer: writer}
	aiWriter, ok := protocol.NewAISDKWriter(response)
	if !ok {
		t.Fatal("pipe response writer should support flushing")
	}
	live := make(chan observability.AgentEvent, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		deps.streamAgentChat(ctx, aiWriter, runID, live, nil, nil)
		_ = writer.Close()
	}()

	var body strings.Builder
	buffer := make([]byte, 256)
	for !strings.Contains(body.String(), `"type":"data-cursor"`) {
		count, err := reader.Read(buffer)
		if count > 0 {
			body.Write(buffer[:count])
		}
		if err != nil {
			t.Fatalf("read initial stream: %v; body=%s", err, body.String())
		}
	}

	appendEvent(observability.EventAgentTextDelta, `{"text":"intermediate tool-round prose"}`)
	appendEvent(observability.EventFinalResponse, `{"text":"canonical final response"}`)
	terminal := appendEvent(observability.EventRunCompleted, `{}`)
	live <- terminal
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	body.Write(remainder)
	<-done

	if !strings.Contains(body.String(), `"delta":"canonical final response"`) {
		t.Fatalf("live terminal skipped persisted canonical final response: %s", body.String())
	}
	if strings.Contains(body.String(), "intermediate tool-round prose") {
		t.Fatalf("intermediate assistant prose leaked into Agent Chat: %s", body.String())
	}
}
