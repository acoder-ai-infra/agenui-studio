package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/cloudwego/eino/schema"
)

func TestEmitEinoMessagePublishesNonEmptyPreview(t *testing.T) {
	out := make(chan observability.AgentEvent, 1)
	if !emitEinoMessage(context.Background(), out, &schema.Message{Role: schema.Assistant, Content: "hello"}, schema.Assistant, "deep") {
		t.Fatal("emitEinoMessage returned false")
	}
	event := <-out
	if got := previewTextFromEvent(event); got != "hello" {
		t.Fatalf("Eino delta preview=%q event=%#v", got, event)
	}
}

func TestEmitEinoMessageIgnoresEmptyAssistantChunk(t *testing.T) {
	out := make(chan observability.AgentEvent, 1)
	if !emitEinoMessage(context.Background(), out, &schema.Message{Role: schema.Assistant}, schema.Assistant, "deep") {
		t.Fatal("emitEinoMessage returned false")
	}
	select {
	case event := <-out:
		t.Fatalf("empty Eino chunk produced an event: %#v", event)
	default:
	}
}

func TestEmitEinoMessagePublishesUserVisibleReasoningSummary(t *testing.T) {
	out := make(chan observability.AgentEvent, 1)
	message := &schema.Message{Role: schema.Assistant, AssistantGenMultiContent: []schema.MessageOutputPart{{
		Type:      schema.ChatMessagePartTypeReasoning,
		Reasoning: &schema.MessageOutputReasoning{Text: "已检查用户目标和可用工具"},
	}}}
	if !emitEinoMessage(context.Background(), out, message, schema.Assistant, "deep") {
		t.Fatal("emitEinoMessage returned false")
	}
	event := <-out
	if event.EventType != observability.EventReasoningSummary || event.Visibility != observability.VisibilityUserVisible {
		t.Fatalf("reasoning summary event=%#v", event)
	}
	if got := previewTextFromEvent(event); got != "已检查用户目标和可用工具" {
		t.Fatalf("reasoning summary preview=%q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.PayloadPreview, &payload); err != nil || payload["reasoning_scope"] != reasoningScopeAnswerSummary {
		t.Fatalf("reasoning summary scope=%#v err=%v", payload, err)
	}
}

func TestEmitEinoMessageDoesNotPublishToolRoundReasoning(t *testing.T) {
	out := make(chan observability.AgentEvent, 1)
	message := &schema.Message{
		Role:      schema.Assistant,
		ToolCalls: []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "search"}}},
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type:      schema.ChatMessagePartTypeReasoning,
			Reasoning: &schema.MessageOutputReasoning{Text: "先分析参数，再调用搜索工具"},
		}},
	}
	if !emitEinoMessage(context.Background(), out, message, schema.Assistant, "deep") {
		t.Fatal("emitEinoMessage returned false")
	}
	select {
	case event := <-out:
		t.Fatalf("tool round reasoning became user-visible: %#v", event)
	default:
	}
}

func TestEmitEinoMessageBoundsOneReasoningSummary(t *testing.T) {
	out := make(chan observability.AgentEvent, 1)
	message := &schema.Message{Role: schema.Assistant, AssistantGenMultiContent: []schema.MessageOutputPart{{
		Type:      schema.ChatMessagePartTypeReasoning,
		Reasoning: &schema.MessageOutputReasoning{Text: strings.Repeat("思", maxPublicReasoningRunes+100)},
	}}}
	if !emitEinoMessage(context.Background(), out, message, schema.Assistant, "deep") {
		t.Fatal("emitEinoMessage returned false")
	}
	event := <-out
	var payload struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(event.PayloadPreview, &payload); err != nil {
		t.Fatalf("decode reasoning summary: %v", err)
	}
	if got := len([]rune(payload.Text)); got != maxPublicReasoningRunes || !payload.Truncated {
		t.Fatalf("bounded reasoning runes=%d truncated=%v", got, payload.Truncated)
	}
}

func TestEinoUserOutputProjectionPublishesOneRootAcknowledgement(t *testing.T) {
	projection := newEinoUserOutputProjection("agenui_agent")

	first := newEinoMessageObservation()
	first.Observe(&schema.Message{
		Role: schema.Assistant, Content: "已收到，我会先梳理内容，再生成界面。",
		ToolCalls: []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "prepare"}}},
	})
	projection.Observe(first, schema.Assistant, "agenui_agent")

	later := newEinoMessageObservation()
	later.Observe(&schema.Message{
		Role: schema.Assistant, Content: "接下来绑定字段。",
		ToolCalls: []schema.ToolCall{{ID: "call-2", Type: "function", Function: schema.FunctionCall{Name: "bind"}}},
	})
	projection.Observe(later, schema.Assistant, "agenui_agent")

	event := projection.Decorate(observability.AgentEvent{
		EventType:      observability.EventToolCallStarted,
		PayloadPreview: json.RawMessage(`{"tool_name":"prepare"}`),
	})
	var payload map[string]any
	if err := json.Unmarshal(event.PayloadPreview, &payload); err != nil {
		t.Fatalf("decode acknowledgement: %v", err)
	}
	if payload["task_acknowledgement"] != "已收到，我会先梳理内容，再生成界面。" || payload["tool_name"] != "prepare" {
		t.Fatalf("acknowledgement payload=%#v", payload)
	}
	duplicate := projection.Decorate(observability.AgentEvent{
		EventType:      observability.EventToolCallStarted,
		PayloadPreview: json.RawMessage(`{"tool_name":"bind"}`),
	})
	var duplicatePayload map[string]any
	if err := json.Unmarshal(duplicate.PayloadPreview, &duplicatePayload); err != nil {
		t.Fatalf("decode later tool event: %v", err)
	}
	if _, exists := duplicatePayload["task_acknowledgement"]; exists {
		t.Fatalf("later tool turn carried duplicate acknowledgement: %#v", duplicatePayload)
	}
}

func TestEinoUserOutputProjectionIgnoresSubagentTextAndEmitsExplicitFinal(t *testing.T) {
	out := make(chan observability.AgentEvent, 4)
	projection := newEinoUserOutputProjection("agenui_agent")

	subagent := newEinoMessageObservation()
	subagent.Observe(&schema.Message{
		Role: schema.Assistant, Content: "子 Agent 内部说明",
		ToolCalls: []schema.ToolCall{{ID: "call-child", Function: schema.FunctionCall{Name: "inspect"}}},
	})
	projection.Observe(subagent, schema.Assistant, "agenui_style")

	final := newEinoMessageObservation()
	final.Observe(&schema.Message{Role: schema.Assistant, Content: "界面已经生成并完成数据校验。"})
	projection.Observe(final, schema.Assistant, "agenui_agent")
	if !projection.EmitFinal(context.Background(), out) {
		t.Fatal("explicit final response was not emitted")
	}

	event := <-out
	if event.EventType != observability.EventFinalResponse {
		t.Fatalf("final event=%#v", event)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["content"] != "界面已经生成并完成数据校验。" {
		t.Fatalf("final payload=%#v err=%v", payload, err)
	}
	select {
	case unexpected := <-out:
		t.Fatalf("subagent text became public output: %#v", unexpected)
	default:
	}
}

func TestEinoUserOutputProjectionFallsBackAndBoundsAcknowledgement(t *testing.T) {
	projection := newEinoUserOutputProjection("agenui_agent")
	observation := newEinoMessageObservation()
	observation.hasToolCall = true
	observation.role = schema.Assistant
	observation.text.WriteString(strings.Repeat("说明", maxTaskAcknowledgementRunes))

	projection.Observe(observation, schema.Assistant, "agenui_agent")
	bounded := projection.Decorate(observability.AgentEvent{
		EventType: observability.EventToolCallStarted,
		Payload:   json.RawMessage(`{"arguments":{"secret":"must-not-enter-preview"}}`),
	})
	var payload map[string]any
	if err := json.Unmarshal(bounded.PayloadPreview, &payload); err != nil {
		t.Fatalf("decode acknowledgement: %v", err)
	}
	text, _ := payload["task_acknowledgement"].(string)
	if len([]rune(text)) != maxTaskAcknowledgementRunes || payload["task_acknowledgement_truncated"] != true {
		t.Fatalf("bounded acknowledgement runes=%d payload=%#v", len([]rune(text)), payload)
	}
	if _, leaked := payload["arguments"]; leaked {
		t.Fatalf("raw tool payload leaked into acknowledgement preview: %#v", payload)
	}

	empty := newEinoUserOutputProjection("agenui_agent")
	emptyObservation := newEinoMessageObservation()
	emptyObservation.hasToolCall = true
	emptyObservation.role = schema.Assistant
	empty.Observe(emptyObservation, schema.Assistant, "agenui_agent")
	fallback := empty.Decorate(observability.AgentEvent{EventType: observability.EventToolCallStarted})
	var fallbackPayload map[string]any
	if err := json.Unmarshal(fallback.PayloadPreview, &fallbackPayload); err != nil {
		t.Fatalf("decode fallback acknowledgement: %v", err)
	}
	if got, _ := fallbackPayload["task_acknowledgement"].(string); got != defaultTaskAcknowledgement {
		t.Fatalf("fallback acknowledgement=%q", got)
	}
}

func previewTextFromEvent(event observability.AgentEvent) string {
	var payload struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(event.PayloadPreview, &payload)
	return payload.Text
}
