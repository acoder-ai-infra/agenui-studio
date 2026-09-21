package context

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBuilderPreservesConversationAndToolOrder(t *testing.T) {
	builder := NewDefaultBuilder(NewSourceRegistry(
		NewConversationSource(0),
	), EstimateCounter{}, BuilderConfig{})
	messages := []*Message{
		{ID: "u1", Sequence: 1, Role: RoleUser, Content: "search"},
		{ID: "a1", Sequence: 2, Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "search"}}},
		{ID: "t1", Sequence: 3, Role: RoleTool, ToolResult: &ToolResult{CallID: "c1", Name: "search", Content: "result"}, Content: "result"},
		{ID: "a2", Sequence: 4, Role: RoleAssistant, Content: "done"},
	}
	result, err := builder.Build(context.Background(), BuildRequest{Messages: messages, TokenBudget: ContextBudget{Limit: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("got %d messages", len(result.Messages))
	}
	for i, message := range result.Messages {
		if message.Sequence != int64(i+1) {
			t.Fatalf("message order changed at %d: sequence=%d", i, message.Sequence)
		}
	}
}

func TestBuilderDropsParallelToolInteractionAtomically(t *testing.T) {
	builder := NewDefaultBuilder(NewSourceRegistry(
		NewConversationSource(0),
		&largeOptionalSource{},
	), EstimateCounter{}, BuilderConfig{})
	messages := []*Message{
		{ID: "u1", Sequence: 1, Role: RoleUser, Content: "run both"},
		{ID: "a1", Sequence: 2, Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "one"}, {ID: "c2", Name: "two"}}},
		{ID: "t1", Sequence: 3, Role: RoleTool, Content: "one result", ToolResult: &ToolResult{CallID: "c1"}},
		{ID: "t2", Sequence: 4, Role: RoleTool, Content: "two result", ToolResult: &ToolResult{CallID: "c2"}},
		{ID: "u2", Sequence: 5, Role: RoleUser, Content: "latest input"},
	}
	result, err := builder.Build(context.Background(), BuildRequest{Messages: messages, TokenBudget: ContextBudget{Limit: 25}})
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, message := range result.Messages {
		present[message.ID] = true
	}
	interactionCount := 0
	for _, id := range []string{"a1", "t1", "t2"} {
		if present[id] {
			interactionCount++
		}
	}
	if interactionCount != 0 && interactionCount != 3 {
		t.Fatalf("parallel tool interaction was split: %#v", present)
	}
}

func TestBuilderFailsClosedWhenPinnedContextExceedsBudget(t *testing.T) {
	builder := NewDefaultBuilder(NewSourceRegistry(
		NewSystemPromptSource("a required system instruction that cannot be removed", "", ""),
	), EstimateCounter{}, BuilderConfig{})
	_, err := builder.Build(context.Background(), BuildRequest{TokenBudget: ContextBudget{Limit: 1}})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected budget error, got %v", err)
	}
}

func TestBuilderPinsEveryCurrentInputAcrossHistoryTrim(t *testing.T) {
	builder := NewDefaultBuilder(
		NewSourceRegistry(NewConversationSource(0)),
		EstimateCounter{},
		BuilderConfig{MinPreserve: MinPreserveConfig{CurrentInput: true, RecentTurns: 1}},
	)
	messages := []*Message{
		{ID: "old", Sequence: 1, Role: RoleUser, Content: strings.Repeat("old ", 40)},
		{ID: "current-1", Sequence: 2, Role: RoleUser, Content: "first", CurrentInput: true},
		{ID: "current-2", Sequence: 3, Role: RoleUser, Content: "second", CurrentInput: true},
	}
	result, err := builder.Build(context.Background(), BuildRequest{Messages: messages, TokenBudget: ContextBudget{Limit: 12}})
	if err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, message := range result.Messages {
		present[message.ID] = true
	}
	if present["old"] || !present["current-1"] || !present["current-2"] {
		t.Fatalf("current inputs were not preserved across trim: %#v", present)
	}
	for _, fragment := range result.Fragments {
		if fragment.Messages[0].CurrentInput && !fragment.Pinned {
			t.Fatalf("current input fragment is not pinned: %#v", fragment)
		}
	}
}

func TestBuilderFailsClosedInsteadOfDroppingOversizedCurrentInput(t *testing.T) {
	builder := NewDefaultBuilder(
		NewSourceRegistry(NewConversationSource(0)),
		EstimateCounter{},
		BuilderConfig{MinPreserve: MinPreserveConfig{CurrentInput: true}},
	)
	_, err := builder.Build(context.Background(), BuildRequest{
		Messages: []*Message{{
			ID: "current", Role: RoleUser, Content: strings.Repeat("required ", 40), CurrentInput: true,
		}},
		TokenBudget: ContextBudget{Limit: 1},
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("oversized current input was silently dropped: %v", err)
	}
}

func TestBuilderRejectsOrphanStructuredToolResult(t *testing.T) {
	builder := NewDefaultBuilder(NewSourceRegistry(NewConversationSource(0)), nil, BuilderConfig{})
	_, err := builder.Build(context.Background(), BuildRequest{
		SessionID: "session", TokenBudget: ContextBudget{Limit: 100},
		Messages: []*Message{{ID: "result", Role: RoleTool, ToolResult: &ToolResult{CallID: "missing", Content: "orphan"}}},
	})
	if !errors.Is(err, ErrBuilderBuild) || !errors.Is(err, ErrToolPairingInvalid) {
		t.Fatalf("orphan tool result was accepted: %v", err)
	}
}

func TestAtomicPairingClosesOverParallelToolCalls(t *testing.T) {
	assistant := &Message{ID: "assistant", Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "one"}, {ID: "call-2", Name: "two"}}}
	result1 := &Message{ID: "result-1", Role: RoleTool, ToolResult: &ToolResult{CallID: "call-1"}}
	result2 := &Message{ID: "result-2", Role: RoleTool, ToolResult: &ToolResult{CallID: "call-2"}}
	all := []ContextFragment{
		{Messages: []*Message{assistant}, PairingIDs: []string{"call-1", "call-2"}},
		{Messages: []*Message{result1}, PairingIDs: []string{"call-1"}},
		{Messages: []*Message{result2}, PairingIDs: []string{"call-2"}},
	}
	kept, dropped := enforceAtomicPairing(all, all[1:])
	if len(kept) != 0 || len(dropped) != 2 {
		t.Fatalf("parallel pair closure left an orphan: kept=%#v dropped=%#v", kept, dropped)
	}
}

type largeOptionalSource struct{}

func (*largeOptionalSource) Kind() SourceKind { return SourceRetrieval }
func (*largeOptionalSource) Collect(context.Context, CollectRequest) ([]ContextFragment, error) {
	return []ContextFragment{{Slot: SlotRAG, Stability: StabilitySemi, Priority: 1, TokenCost: 100, Content: "optional"}}, nil
}
