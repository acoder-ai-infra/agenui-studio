package toolgateway

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

func TestBuildToolStartedToolEventThenNormalizeAgentEvent(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)
	tc := observability.TraceContext{
		TraceID:      "trace-1",
		SpanID:       "span-tool",
		ParentSpanID: "span-parent",
		SessionID:    "sess-1",
		RunID:        "run-1",
		AgentID:      "agent-a",
		AgentType:    "planner",
		Runtime:      "go-test",
	}
	def := defaultSearchDefinition()
	def.DisplayName = "检索知识"
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.ProcessStage = processpresentation.Stage{ID: "research", Label: "理解内容要求", Order: 10}

	toolEvent := buildToolStartedToolEvent(fixedIDGenerator{}, now, tc, req, &def)
	if toolEvent.SchemaVersion != ToolEventSchemaVersion {
		t.Fatalf("tool event schema version = %q", toolEvent.SchemaVersion)
	}
	if toolEvent.ToolEventID != "evt-generated" {
		t.Fatalf("tool event id = %q", toolEvent.ToolEventID)
	}
	if toolEvent.EventType != toolEventStarted {
		t.Fatalf("tool event type = %q", toolEvent.EventType)
	}
	if toolEvent.ToolCallID != "tc-1" || toolEvent.ToolName != "search_kb" || toolEvent.ToolVersion != "v1" || toolEvent.ToolType != ToolTypeFunction {
		t.Fatalf("tool identity not populated: %#v", toolEvent)
	}
	if toolEvent.TraceID != "trace-1" || toolEvent.SpanID != "span-tool" || toolEvent.ParentSpanID != "span-parent" {
		t.Fatalf("trace context not populated: %#v", toolEvent)
	}

	agentEvent, err := NormalizeToolEvent(fixedIDGenerator{}, toolEvent)
	if err != nil {
		t.Fatalf("normalize started tool event: %v", err)
	}
	if agentEvent.SchemaVersion != observability.AgentEventSchemaVersion {
		t.Fatalf("agent event schema version = %q", agentEvent.SchemaVersion)
	}
	if agentEvent.EventID != toolEvent.ToolEventID || agentEvent.EventType != observability.EventToolCallStarted {
		t.Fatalf("agent event identity mismatch: %#v", agentEvent)
	}
	if agentEvent.TraceID != toolEvent.TraceID || agentEvent.RunID != toolEvent.RunID || agentEvent.StepID != toolEvent.StepID || agentEvent.AgentID != toolEvent.AgentID {
		t.Fatalf("agent event context mismatch: %#v", agentEvent)
	}
	if string(agentEvent.PayloadPreview) != string(toolEvent.PayloadPreview) {
		t.Fatalf("agent payload preview = %s, want %s", agentEvent.PayloadPreview, toolEvent.PayloadPreview)
	}

	var payload map[string]any
	if err := json.Unmarshal(agentEvent.PayloadPreview, &payload); err != nil {
		t.Fatalf("decode normalized payload: %v", err)
	}
	if payload["tool_call_id"] != "tc-1" || payload["tool_name"] != "search_kb" || payload["tool_type"] != "function" {
		t.Fatalf("unexpected normalized payload: %#v", payload)
	}
	presentation, _ := payload["process_presentation"].(map[string]any)
	stage, _ := presentation["stage"].(map[string]any)
	activity, _ := presentation["activity"].(map[string]any)
	if stage["id"] != "research" || stage["label"] != "理解内容要求" || activity["key"] != "search_kb" || activity["label"] != "检索知识" {
		t.Fatalf("process presentation missing: %#v", presentation)
	}
}

func TestNormalizeToolEventAppliesP0Defaults(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)
	agentEvent, err := NormalizeToolEvent(fixedIDGenerator{}, ToolEvent{
		EventType:      ToolEventType(observability.EventToolCallCompleted),
		TraceID:        "trace-1",
		PayloadPreview: json.RawMessage(`{"tool_call_id":"tc-1","success":true}`),
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("normalize completed tool event: %v", err)
	}

	if agentEvent.EventID != "evt-generated" {
		t.Fatalf("default event id = %q", agentEvent.EventID)
	}
	if agentEvent.SchemaVersion != observability.AgentEventSchemaVersion {
		t.Fatalf("schema version = %q", agentEvent.SchemaVersion)
	}
	if agentEvent.Visibility != observability.VisibilityDebug {
		t.Fatalf("default visibility = %q", agentEvent.Visibility)
	}
	if !agentEvent.CreatedAt.Equal(now) {
		t.Fatalf("created_at = %s", agentEvent.CreatedAt)
	}
}

func TestCompletedEventUsesCanonicalRefs(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)
	tc := observability.TraceContext{
		TraceID: "trace-1", SpanID: "span-tool", SessionID: "sess-1", RunID: "run-1", AgentID: "agent-a",
	}
	def := defaultSearchDefinition()
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	normalized := normalizedResult{
		preview:   json.RawMessage(`{"ok":true}`),
		resultRef: "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_result",
		debugRef:  "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_debug",
	}

	toolEvent := buildToolCompletedToolEvent(fixedIDGenerator{}, now, tc, req, &def, normalized, time.Second, 0)
	event, err := NormalizeToolEvent(fixedIDGenerator{}, toolEvent)
	if err != nil {
		t.Fatalf("normalize completed tool event: %v", err)
	}
	if event.PayloadRef != normalized.resultRef {
		t.Fatalf("completed outer payload ref = %q, want %q", event.PayloadRef, normalized.resultRef)
	}
	if event.DebugRef != normalized.debugRef {
		t.Fatalf("completed outer debug ref = %q, want %q", event.DebugRef, normalized.debugRef)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.PayloadPreview, &payload); err != nil {
		t.Fatalf("decode completed preview: %v", err)
	}
	if payload["result_ref"] != normalized.resultRef {
		t.Fatalf("safe completed payload lost documented result_ref: %#v", payload)
	}
}

func TestToolProcessPresentationUsesCallerStageForSharedTool(t *testing.T) {
	def := defaultSearchDefinition()
	def.Name = "agenui_workspace"
	def.DisplayName = "生成界面结构"
	req := baseToolRequest(json.RawMessage(`{}`))
	req.ToolName = def.Name

	req.ProcessStage = processpresentation.Stage{ID: "ui_generation", Label: "生成界面", Order: 20}
	style := toolProcessPresentation(req, &def)
	req.ProcessStage = processpresentation.Stage{ID: "data_binding", Label: "配置数据与动作", Order: 30}
	binder := toolProcessPresentation(req, &def)

	if style.Stage.ID != "ui_generation" || binder.Stage.ID != "data_binding" {
		t.Fatalf("shared tool stages style=%#v binder=%#v", style.Stage, binder.Stage)
	}
	if style.Activity != binder.Activity || style.Activity.Key != "agenui_workspace" {
		t.Fatalf("shared tool activity drifted style=%#v binder=%#v", style.Activity, binder.Activity)
	}
}

func TestCompletedEventCarriesBoundedPresentationWithoutExtraEvent(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.DisplayName = "知识检索"
	gateway := newRawResultGateway(eventStore, nil, def, &ToolRawResult{
		Data: json.RawMessage(`{"items":3}`),
		Presentation: &ResultPresentation{
			Title:   "检索完成",
			Summary: "已找到 3 项候选能力",
			Details: []ResultPresentationDetail{
				{Label: "候选数量", Value: "3"},
				{Label: "API Key", Value: "must-not-leak"},
			},
		},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke presentation tool: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("result status = %s", result.Status)
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallCompleted {
		t.Fatalf("presentation added events: %#v", got)
	}
	payload := decodeEventPayload(t, eventStore.events[1])
	if payload["display_name"] != "知识检索" {
		t.Fatalf("display name missing: %#v", payload)
	}
	presentation, ok := payload["presentation"].(map[string]any)
	if !ok || presentation["title"] != "检索完成" || presentation["summary"] != "已找到 3 项候选能力" {
		t.Fatalf("presentation missing: %#v", payload)
	}
	details, ok := presentation["details"].([]any)
	if !ok || len(details) != 2 {
		t.Fatalf("presentation details = %#v", presentation["details"])
	}
	sensitive, _ := details[1].(map[string]any)
	if sensitive["value"] != "[REDACTED]" {
		t.Fatalf("sensitive presentation detail leaked: %#v", sensitive)
	}
}

func TestCompletedEventUsesAuthoritativeResultArtifactMetadata(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		ArtifactThresholdBytes: 8,
		MaxSSEPreviewBytes:     4096,
		MaxModelContextBytes:   4096,
	}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data:     json.RawMessage(`{"message":"authoritative artifact metadata"}`),
		MimeType: "application/vnd.harness.result+json",
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke result artifact tool: %v", err)
	}
	if result.Status != ToolCallSucceeded || result.ResultRef == "" {
		t.Fatalf("result artifact invocation = %#v", result)
	}
	meta, err := artifactStore.Head(ctx, result.ResultRef)
	if err != nil {
		t.Fatalf("head result artifact: %v", err)
	}
	completed := eventStore.events[len(eventStore.events)-1]
	if completed.EventType != observability.EventToolCallCompleted || completed.PayloadRef != meta.ArtifactRef {
		t.Fatalf("completed outer metadata = %#v, artifact=%#v", completed, meta)
	}
	payload := decodeEventPayload(t, completed)
	if payload["result_ref"] != meta.ArtifactRef || payload["hash"] != meta.Hash || payload["mime_type"] != meta.MimeType || payload["size_bytes"] != float64(meta.SizeBytes) {
		t.Fatalf("completed payload is not based on authoritative artifact metadata: payload=%#v meta=%#v", payload, meta)
	}
}
