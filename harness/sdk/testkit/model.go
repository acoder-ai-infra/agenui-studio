package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// ModelScript is a deterministic scripted response the MockModel replays for
// each Invoke call. Multi-turn tests use MockModel.Enqueue to append additional
// scripts; when the queue empties, Invoke returns ErrScriptExhausted.
type ModelScript struct {
	// TextDeltas is the ordered sequence of user-visible token fragments to
	// stream via agent_text_delta events. Delays applies BetweenTokens between
	// them.
	TextDeltas []string
	// FinalResponse, when non-empty, is emitted as the run's final_response.
	FinalResponse string
	// ToolCalls is the ordered list of tool calls the script issues. Each
	// tool call is emitted before the run continues.
	ToolCalls []ModelToolCall
	// Reasoning, when non-empty, is emitted as reasoning_summary events.
	Reasoning []string
	// Usage carries token / cost / duration attribution for the run.
	Usage json.RawMessage
	// FailWith, when non-nil, aborts the invocation before any delta with a
	// canonical model_call_failed event.
	FailWith error
	// BetweenTokens is the delay between successive TextDeltas.
	BetweenTokens time.Duration
}

// ModelToolCall is the scripted representation of a model-produced tool call.
type ModelToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// MockModel is a deterministic scripted stand-in for a Model Gateway. It is
// used by SDK consumers who want to test business orchestration without a
// live provider. The mock is safe for concurrent use.
type MockModel struct {
	mu      sync.Mutex
	scripts []ModelScript
}

// ErrScriptExhausted is returned when Invoke is called with no scripts left.
var ErrScriptExhausted = errors.New("testkit: model script queue exhausted")

// NewMockModel builds a MockModel loaded with an initial script queue.
func NewMockModel(initial ...ModelScript) *MockModel {
	return &MockModel{scripts: append([]ModelScript(nil), initial...)}
}

// Enqueue appends more scripts to the queue.
func (m *MockModel) Enqueue(scripts ...ModelScript) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts = append(m.scripts, scripts...)
}

// Invoke pops the next script and returns it. If the queue is empty it
// returns ErrScriptExhausted so tests fail loudly instead of silently
// producing empty responses.
func (m *MockModel) Invoke(ctx context.Context) (ModelScript, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.scripts) == 0 {
		return ModelScript{}, ErrScriptExhausted
	}
	script := m.scripts[0]
	m.scripts = m.scripts[1:]
	return script, nil
}

// ProjectEvents turns a ModelScript into the canonical event stream a
// Scenario replays through MockEngine. The projection covers text deltas,
// reasoning summaries, tool calls, final responses, usage, and failures.
func ProjectEvents(script ModelScript) []harness.Event {
	events := make([]harness.Event, 0, len(script.TextDeltas)+len(script.Reasoning)+len(script.ToolCalls)+2)
	if script.FailWith != nil {
		events = append(events, harness.Event{
			EventType:  harness.EventModelCallFailed,
			Visibility: harness.VisibilityDebug,
			Error: &harness.EventError{
				Code:    "MODEL_CALL_FAILED",
				Type:    harness.EventErrorUpstream,
				Message: script.FailWith.Error(),
			},
		})
		return events
	}
	events = append(events, harness.Event{
		EventType:  harness.EventModelCallStarted,
		Visibility: harness.VisibilityDebug,
	})
	for _, text := range script.TextDeltas {
		events = append(events, TextDeltaEvent(text))
	}
	for _, reason := range script.Reasoning {
		payload, _ := json.Marshal(map[string]string{"summary": reason})
		events = append(events, harness.Event{
			EventType:      harness.EventReasoningSummary,
			Visibility:     harness.VisibilityDebug,
			PayloadPreview: payload,
		})
	}
	for _, tc := range script.ToolCalls {
		payload, _ := json.Marshal(map[string]any{"id": tc.ID, "name": tc.Name, "arguments": string(tc.Arguments)})
		events = append(events, harness.Event{
			EventType:      harness.EventModelToolCallDelta,
			Visibility:     harness.VisibilityDebug,
			PayloadPreview: payload,
		})
	}
	events = append(events, harness.Event{
		EventType:  harness.EventModelCallCompleted,
		Visibility: harness.VisibilityDebug,
		Usage:      script.Usage,
	})
	if script.FinalResponse != "" {
		events = append(events, FinalResponseEvent(script.FinalResponse))
	}
	return events
}
