package app

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestGatewayMessagesPreservesAssistantToolCallPairing(t *testing.T) {
	messages := gatewayMessages([]agentruntime.ModelCallMessage{
		{Role: "assistant", ReasoningContent: "reason before tool", ReasoningSignature: "sig-pair", ToolCalls: []agentruntime.ModelToolCall{{ToolCallID: "call_1", Name: "harness.echo", Arguments: json.RawMessage(`{"text":"proof"}`)}}},
		{Role: "tool", ToolCallID: "call_1", Content: "proof"},
	})
	if len(messages) != 2 || messages[0].ReasoningContent != "reason before tool" || messages[0].ReasoningSignature != "sig-pair" || len(messages[0].ToolCalls) != 1 {
		t.Fatalf("assistant tool call was lost: %#v", messages)
	}
	call := messages[0].ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "harness.echo" || call.Function.Arguments != `{"text":"proof"}` {
		t.Fatalf("assistant tool call changed: %#v", call)
	}
	if messages[1].Role != "tool" || messages[1].ToolCallID != "call_1" || messages[1].Content != "proof" {
		t.Fatalf("tool result pairing changed: %#v", messages[1])
	}
}

func TestBridgeModelEventsPreservesReasoningSignatureBeforeToolCall(t *testing.T) {
	events := make(chan observability.AgentEvent, 1)
	events <- observability.AgentEvent{EventType: observability.EventModelCallCompleted}
	close(events)
	call := &modelgateway.ModelCall{Events: events, Await: func() (*modelgateway.ModelResponse, error) {
		return &modelgateway.ModelResponse{
			Status: modelgateway.StatusSuccess, ReasoningSignature: "sig-opaque",
			ToolCalls: []modelgateway.ModelToolCall{{ToolCallID: "tool_1", Name: "harness.echo", Arguments: `{"text":"hello"}`}},
		}, nil
	}}
	out := make(chan agentruntime.ModelStreamItem, 4)
	bridgeModelEvents(context.Background(), call, out)
	var items []agentruntime.ModelStreamItem
	for item := range out {
		items = append(items, item)
	}
	if len(items) != 3 || items[0].ReasoningSignature != "sig-opaque" || items[1].ToolCall == nil || items[1].ToolCall.ToolCallID != "tool_1" || items[2].Event.EventType != observability.EventModelCallCompleted {
		t.Fatalf("signature/tool completion order changed: %#v", items)
	}
}

func TestBridgeModelEventsPreservesReasoningDelta(t *testing.T) {
	events := make(chan observability.AgentEvent, 2)
	events <- observability.AgentEvent{EventType: observability.EventModelThoughtDelta, Payload: modelgatewayJSON(t, modelgateway.ModelThoughtDeltaPayload{Text: "reasoning"})}
	events <- observability.AgentEvent{EventType: observability.EventModelCallCompleted}
	close(events)
	call := &modelgateway.ModelCall{Events: events, Await: func() (*modelgateway.ModelResponse, error) {
		return &modelgateway.ModelResponse{Status: modelgateway.StatusSuccess}, nil
	}}
	out := make(chan agentruntime.ModelStreamItem, 4)
	bridgeModelEvents(context.Background(), call, out)
	var reasoning string
	for item := range out {
		reasoning += item.ReasoningDelta
	}
	if reasoning != "reasoning" {
		t.Fatalf("reasoning delta was lost at Runtime/Gateway bridge: %q", reasoning)
	}
}

func TestBridgeModelEventsPreservesInvalidToolIdentityAndDiagnostics(t *testing.T) {
	events := make(chan observability.AgentEvent, 1)
	events <- observability.AgentEvent{EventType: observability.EventModelCallCompleted}
	close(events)
	call := &modelgateway.ModelCall{Events: events, Await: func() (*modelgateway.ModelResponse, error) {
		return &modelgateway.ModelResponse{Status: modelgateway.StatusSuccess, ToolCalls: []modelgateway.ModelToolCall{{
			ToolCallID: "tool_invalid", Name: "list_developer_operators", Arguments: `{`,
		}}}, nil
	}}
	out := make(chan agentruntime.ModelStreamItem, 4)
	bridgeModelEvents(context.Background(), call, out)
	var toolItem *agentruntime.ModelStreamItem
	for item := range out {
		if item.ToolCall != nil {
			copy := item
			toolItem = &copy
		}
	}
	if toolItem == nil || toolItem.ToolCall.Name != "list_developer_operators" || len(toolItem.Event.Payload) == 0 || !json.Valid(toolItem.Event.Payload) {
		t.Fatalf("invalid tool call fact was lost: %#v", toolItem)
	}
	var payload struct {
		ToolCallID       string `json:"tool_call_id"`
		Name             string `json:"name"`
		ArgumentsInvalid bool   `json:"arguments_invalid"`
		ArgumentBytes    int    `json:"argument_bytes"`
		ArgumentsHash    string `json:"arguments_hash"`
		ValidationError  string `json:"validation_error"`
		RepairApplied    bool   `json:"repair_applied"`
		NormalizedTo     string `json:"normalized_to"`
	}
	if err := json.Unmarshal(toolItem.Event.Payload, &payload); err != nil || payload.ToolCallID != "tool_invalid" ||
		payload.Name != "list_developer_operators" || !payload.ArgumentsInvalid || payload.ArgumentBytes != 1 ||
		payload.ArgumentsHash == "" || payload.ValidationError == "" || !payload.RepairApplied || payload.NormalizedTo != "{}" ||
		string(toolItem.ToolCall.Arguments) != `{}` {
		t.Fatalf("invalid tool call payload=%s err=%v", toolItem.Event.Payload, err)
	}
}

func modelgatewayJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGatewayModelOptionsPreservesReasoningAndExplicitZeroTemperature(t *testing.T) {
	temperature := float32(0)
	topP := float32(0.7)
	maxTokens := 256
	converted := gatewayModelOptions(agentruntime.ModelCallOptions{
		Temperature: &temperature, TopP: &topP, MaxTokens: &maxTokens,
		Stop: []string{"END"}, ToolChoice: "auto",
		ReasoningMode: agentruntime.ModelReasoningEnabled, ReasoningBudget: 128, ReasoningEffort: "low",
	})
	if converted.Temperature == nil || *converted.Temperature != 0 || converted.TopP == nil || math.Abs(*converted.TopP-0.7) > 1e-6 || converted.MaxTokens == nil || *converted.MaxTokens != 256 || converted.ReasoningMode != "enabled" || converted.ReasoningBudget != 128 || converted.ReasoningEffort != "low" {
		t.Fatalf("model options changed at Runtime/Gateway boundary: %#v", converted)
	}
	if len(converted.Stop) != 1 || converted.Stop[0] != "END" || converted.ToolChoice != "auto" {
		t.Fatalf("generation options changed: %#v", converted)
	}
}
