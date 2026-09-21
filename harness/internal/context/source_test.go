package context

import (
	"context"
	"errors"
	"testing"
	"time"
)

func ptrMsg(m Message) *Message { return &m }

func TestSystemPromptSourceCollect(t *testing.T) {
	src := NewSystemPromptSource("You are a coding assistant.", "HARNESS Agent", "Be safe.")
	ctx := context.Background()

	frags, err := src.Collect(ctx, CollectRequest{Generation: 1})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(frags) != 3 {
		t.Fatalf("expected 3 fragments, got %d", len(frags))
	}

	// Check ordering by priority.
	if frags[0].Priority != 100 || frags[1].Priority != 90 || frags[2].Priority != 80 {
		t.Fatalf("unexpected priorities: %d, %d, %d", frags[0].Priority, frags[1].Priority, frags[2].Priority)
	}

	for _, f := range frags {
		if f.Stability != StabilityStable {
			t.Fatalf("system prompt should be stable: %+v", f)
		}
		if !f.Pinned {
			t.Fatalf("system prompt should be pinned: %+v", f)
		}
		if f.Role != RoleSystem {
			t.Fatalf("system prompt role should be system: %+v", f)
		}
		if f.TokenCost <= 0 {
			t.Fatalf("token cost should be > 0: %+v", f)
		}
	}
}

func TestSystemPromptSourceEmptyFields(t *testing.T) {
	src := NewSystemPromptSource("Instruction only.", "", "")
	frags, _ := src.Collect(context.Background(), CollectRequest{})
	if len(frags) != 1 {
		t.Fatalf("expected 1 fragment for non-empty field, got %d", len(frags))
	}
}

func TestConversationSourceCollect(t *testing.T) {
	src := NewConversationSource(0) // no limit
	ctx := context.Background()

	msgs := []*Message{
		ptrMsg(Message{Role: RoleUser, Content: "hello"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "hi"}),
		ptrMsg(Message{Role: RoleUser, Content: "how are you?"}),
	}

	frags, err := src.Collect(ctx, CollectRequest{Messages: msgs, Generation: 1})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(frags) != 3 {
		t.Fatalf("expected 3 fragments, got %d", len(frags))
	}
	for _, f := range frags {
		if f.Stability != StabilityDynamic {
			t.Fatalf("conversation should be dynamic: %+v", f)
		}
		if f.Slot != SlotConversation {
			t.Fatalf("conversation slot expected, got %s", f.Slot)
		}
	}
}

func TestConversationSourcePreservesToolMessages(t *testing.T) {
	src := NewConversationSource(0)
	msgs := []*Message{
		ptrMsg(Message{Role: RoleUser, Content: "do it"}),
		ptrMsg(Message{Role: RoleTool, Content: "tool output"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "done"}),
	}
	frags, _ := src.Collect(context.Background(), CollectRequest{Messages: msgs})
	if len(frags) != 3 || frags[1].Slot != SlotToolResult {
		t.Fatalf("expected ordered tool message, got %#v", frags)
	}
}

func TestConversationSourceMaxTurns(t *testing.T) {
	src := NewConversationSource(2) // keep last 2 turns

	msgs := []*Message{
		ptrMsg(Message{Role: RoleUser, Content: "turn 1"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "resp 1"}),
		ptrMsg(Message{Role: RoleUser, Content: "turn 2"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "resp 2"}),
		ptrMsg(Message{Role: RoleUser, Content: "turn 3"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "resp 3"}),
	}

	frags, _ := src.Collect(context.Background(), CollectRequest{Messages: msgs})
	// 2 turns = user turn 2 + resp 2 + user turn 3 + resp 3 = 4 messages
	if len(frags) != 4 {
		t.Fatalf("expected 4 fragments (2 turns), got %d", len(frags))
	}
	if frags[0].Messages[0].Content != "turn 2" {
		t.Fatalf("expected first message to be 'turn 2', got %s", frags[0].Messages[0].Content)
	}
}

func TestToolResultSourceCollect(t *testing.T) {
	src := NewToolResultSource(0, 0) // no limits
	msgs := []*Message{
		ptrMsg(Message{Role: RoleUser, Content: "do"}),
		ptrMsg(Message{Role: RoleTool, Content: "result 1"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "ok"}),
		ptrMsg(Message{Role: RoleTool, Content: "result 2"}),
	}

	frags, err := src.Collect(context.Background(), CollectRequest{Messages: msgs})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(frags) != 2 {
		t.Fatalf("expected 2 tool fragments, got %d", len(frags))
	}
	for _, f := range frags {
		if f.Slot != SlotToolResult {
			t.Fatalf("expected tool_result slot, got %s", f.Slot)
		}
	}
}

func TestToolResultSourceMaxResults(t *testing.T) {
	src := NewToolResultSource(2, 0) // keep last 2

	var msgs []*Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, ptrMsg(Message{Role: RoleTool, Content: "result"}))
	}

	frags, _ := src.Collect(context.Background(), CollectRequest{Messages: msgs})
	if len(frags) != 2 {
		t.Fatalf("expected 2 (maxResults), got %d", len(frags))
	}
}

func TestToolResultSourceMaxChars(t *testing.T) {
	src := NewToolResultSource(0, 10)
	longContent := "this is a very long tool result that exceeds the limit"
	msgs := []*Message{
		ptrMsg(Message{Role: RoleTool, Content: longContent}),
	}

	frags, _ := src.Collect(context.Background(), CollectRequest{Messages: msgs})
	if len(frags) != 1 {
		t.Fatalf("expected 1 fragment, got %d", len(frags))
	}
	if len(frags[0].Content) >= len(longContent) {
		t.Fatalf("content should be truncated")
	}
}

func TestSourceRegistryCollectAll(t *testing.T) {
	system := NewSystemPromptSource("Be helpful.", "", "")
	conv := NewConversationSource(0)
	registry := NewSourceRegistry(system, conv)

	msgs := []*Message{
		ptrMsg(Message{Role: RoleUser, Content: "hi"}),
	}

	frags, err := registry.CollectAll(context.Background(), CollectRequest{
		Messages: msgs, Generation: 1,
	})
	if err != nil {
		t.Fatalf("collect all: %v", err)
	}
	// 1 system prompt + 1 conversation = 2
	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments, got %d", len(frags))
	}
	// System prompt should come first (order=0 vs order=3).
	if frags[0].Source != string(SourceSystemPrompt) {
		t.Fatalf("first fragment should be system_prompt, got %s", frags[0].Source)
	}
}

func TestSourceRegistryDegradesOnFailure(t *testing.T) {
	registry := NewSourceRegistry(&failSource{}, NewSystemPromptSource("ok", "", ""))

	frags, err := registry.CollectAll(context.Background(), CollectRequest{})
	if err != nil {
		t.Fatalf("collect all should not fail: %v", err)
	}
	// failSource skipped, system prompt still collected.
	if len(frags) != 1 {
		t.Fatalf("expected 1 fragment (degraded), got %d", len(frags))
	}
}

type failSource struct{}

func (failSource) Kind() SourceKind { return SourceWorkspace }
func (failSource) Collect(_ context.Context, _ CollectRequest) ([]ContextFragment, error) {
	return nil, errors.New("workspace unavailable")
}

func TestTrimToRecentTurns(t *testing.T) {
	msgs := []*Message{
		ptrMsg(Message{Role: RoleUser, Content: "u1"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "a1"}),
		ptrMsg(Message{Role: RoleUser, Content: "u2"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "a2"}),
		ptrMsg(Message{Role: RoleUser, Content: "u3"}),
		ptrMsg(Message{Role: RoleAssistant, Content: "a3"}),
	}

	// Keep 1 turn.
	result := trimToRecentTurns(msgs, 1)
	if len(result) != 2 {
		t.Fatalf("expected 2 (1 turn), got %d", len(result))
	}

	// Keep 3 turns = all.
	result = trimToRecentTurns(msgs, 3)
	if len(result) != 6 {
		t.Fatalf("expected 6 (3 turns), got %d", len(result))
	}

	// Keep 0 = no limit.
	result = trimToRecentTurns(msgs, 0)
	if len(result) != 6 {
		t.Fatalf("expected 6 (no limit), got %d", len(result))
	}

	// More turns than available = all.
	result = trimToRecentTurns(msgs, 10)
	if len(result) != 6 {
		t.Fatalf("expected 6 (exceeds turns), got %d", len(result))
	}
}

func TestEstimateCounter(t *testing.T) {
	c := EstimateCounter{}

	if c.Count("") != 0 {
		t.Fatalf("empty string should be 0 tokens")
	}
	if c.Count("hi") != 1 {
		t.Fatalf("'hi' should be 1 token, got %d", c.Count("hi"))
	}
	if c.Count("hello world") < 2 {
		t.Fatalf("'hello world' should be >= 2 tokens")
	}

	m := &Message{Role: RoleUser, Content: "hello world"}
	mc := c.CountMessage(m)
	if mc <= c.Count("hello world") {
		t.Fatalf("message count should include role overhead")
	}

	if c.CountMessage(nil) != 0 {
		t.Fatalf("nil message should be 0")
	}
}

// Suppress unused import warnings.
var _ = time.Now
