package context

import (
	"context"
	"testing"
)

func TestDefaultBuilderBuildBasic(t *testing.T) {
	system := NewSystemPromptSource("You are helpful.", "Agent", "")
	conv := NewConversationSource(0)
	registry := NewSourceRegistry(system, conv)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{
		MinPreserve: MinPreserveConfig{StablePrefix: true, CurrentInput: true, RecentTurns: 1},
	})

	msgs := []*Message{
		{Role: RoleUser, Content: "hello"},
	}

	result, err := builder.Build(context.Background(), BuildRequest{
		SessionID:   "s1",
		Generation:  1,
		Messages:    msgs,
		TokenBudget: ContextBudget{Limit: 10000},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// 2 system prompt frags (instruction + identity) + 1 conversation = 3 messages
	if len(result.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result.Messages))
	}
	if result.TokenCount <= 0 {
		t.Fatalf("token count should be > 0")
	}
	if result.CacheBreak < 0 {
		t.Fatalf("cache break should be set for stable fragments")
	}
	// First messages should be system role.
	if result.Messages[0].Role != RoleSystem {
		t.Fatalf("first message should be system, got %s", result.Messages[0].Role)
	}
}

func TestDefaultBuilderSortStabilityFirst(t *testing.T) {
	system := NewSystemPromptSource("Sys instruction.", "", "")
	conv := NewConversationSource(0)
	registry := NewSourceRegistry(conv, system) // intentionally reversed order
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})

	msgs := []*Message{
		{Role: RoleUser, Content: "user msg"},
	}

	result, _ := builder.Build(context.Background(), BuildRequest{
		Messages: msgs, TokenBudget: ContextBudget{Limit: 10000},
	})

	// Stable should come before dynamic regardless of registration order.
	if result.Messages[0].Role != RoleSystem {
		t.Fatalf("stable system prompt should be first, got role=%s", result.Messages[0].Role)
	}
}

func TestDefaultBuilderTrimEphemeral(t *testing.T) {
	// Create a builder with a custom registry that produces ephemeral fragments.
	system := NewSystemPromptSource("Sys.", "", "")
	registry := NewSourceRegistry(system, &ephemeralSource{})
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})

	// Tight budget to force trimming.
	result, err := builder.Build(context.Background(), BuildRequest{
		Messages:    nil,
		TokenBudget: ContextBudget{Limit: 5}, // very tight
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Ephemeral fragments should be trimmed first.
	for _, f := range result.Fragments {
		if f.Stability == StabilityEphemeral {
			t.Fatalf("ephemeral fragments should be trimmed first")
		}
	}
}

func TestDefaultBuilderTrimOldTools(t *testing.T) {
	system := NewSystemPromptSource("Sys.", "", "")
	tool := NewToolResultSource(0, 0)
	registry := NewSourceRegistry(system, tool)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})

	var msgs []*Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, &Message{Role: RoleTool, Content: "tool result output data"})
	}

	result, _ := builder.Build(context.Background(), BuildRequest{
		Messages:    msgs,
		TokenBudget: ContextBudget{Limit: 50}, // force trimming
	})

	// Count remaining tool results.
	toolCount := 0
	for _, f := range result.Fragments {
		if f.Slot == SlotToolResult {
			toolCount++
		}
	}
	// Should keep at most 3 old tools after trim.
	if toolCount > 3 {
		t.Fatalf("expected at most 3 tool results after trim, got %d", toolCount)
	}
}

func TestDefaultBuilderTrimRecords(t *testing.T) {
	system := NewSystemPromptSource("Sys.", "", "")
	tool := NewToolResultSource(0, 0)
	registry := NewSourceRegistry(system, tool)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})

	var msgs []*Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, &Message{Role: RoleTool, Content: "tool result output data"})
	}

	result, _ := builder.Build(context.Background(), BuildRequest{
		Messages:    msgs,
		TokenBudget: ContextBudget{Limit: 50},
	})

	if len(result.Trimmed) == 0 {
		t.Fatal("expected trim records when over budget")
	}
}

func TestDefaultBuilderNoTrimWhenWithinBudget(t *testing.T) {
	system := NewSystemPromptSource("Hi.", "", "")
	registry := NewSourceRegistry(system)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})

	result, _ := builder.Build(context.Background(), BuildRequest{
		Messages:    nil,
		TokenBudget: ContextBudget{Limit: 100000},
	})

	if len(result.Trimmed) != 0 {
		t.Fatalf("expected no trim records when within budget, got %d", len(result.Trimmed))
	}
}

func TestDefaultBuilderAssembleFragmentMetadata(t *testing.T) {
	system := NewSystemPromptSource("Instruction.", "", "")
	registry := NewSourceRegistry(system)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})

	result, _ := builder.Build(context.Background(), BuildRequest{
		TokenBudget: ContextBudget{Limit: 10000},
	})

	for _, msg := range result.Messages {
		if msg.Extra == nil {
			t.Fatal("assembled messages should have Extra metadata")
		}
		frag, ok := msg.Extra["cm_fragment"]
		if !ok {
			t.Fatal("cm_fragment metadata missing")
		}
		fragMap := frag.(map[string]any)
		if fragMap["stability"] != string(StabilityStable) {
			t.Fatalf("expected stable, got %v", fragMap["stability"])
		}
	}
}

func TestDefaultBuilderAssemblesTruncatedToolResult(t *testing.T) {
	tool := NewToolResultSource(0, 10)
	registry := NewSourceRegistry(tool)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})
	original := &Message{Role: RoleTool, Content: "this is a very long tool result that exceeds the limit"}

	result, err := builder.Build(context.Background(), BuildRequest{
		Messages:    []*Message{original},
		TokenBudget: ContextBudget{Limit: 10000},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	if result.Messages[0].Content == original.Content {
		t.Fatalf("expected assembled tool result to use truncated fragment content")
	}
	if len(result.Messages[0].Content) >= len(original.Content) {
		t.Fatalf("assembled tool result should be shorter than original: %q", result.Messages[0].Content)
	}
	if original.Extra != nil {
		t.Fatalf("assemble should not mutate original message metadata: %#v", original.Extra)
	}
}

func TestDefaultBuilderCacheBreakpoint(t *testing.T) {
	system := NewSystemPromptSource("A.", "B.", "")
	conv := NewConversationSource(0)
	registry := NewSourceRegistry(system, conv)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{})

	result, _ := builder.Build(context.Background(), BuildRequest{
		Messages:    []*Message{{Role: RoleUser, Content: "hi"}},
		TokenBudget: ContextBudget{Limit: 10000},
	})

	// CacheBreak should be the index of the last stable fragment.
	if result.CacheBreak < 0 {
		t.Fatal("cache break should be set")
	}
	// All messages before cache break should be system role.
	for i := 0; i <= result.CacheBreak; i++ {
		if result.Messages[i].Role != RoleSystem {
			t.Fatalf("message %d before cache break should be system, got %s", i, result.Messages[i].Role)
		}
	}
}

func TestDefaultBuilderEmergencyTrim(t *testing.T) {
	// Create fragments that are all non-stable and non-pinned, except one stable.
	system := NewSystemPromptSource("Keep this.", "", "")
	registry := NewSourceRegistry(system, &hugeSource{})
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{
		MinPreserve: MinPreserveConfig{RecentTurns: 1},
	})

	result, err := builder.Build(context.Background(), BuildRequest{
		Messages:    []*Message{{Role: RoleUser, Content: "current input"}},
		TokenBudget: ContextBudget{Limit: 3}, // extremely tight
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].Content != "Keep this." {
		t.Fatalf("emergency trim should retain only stable content: %#v", result.Messages)
	}
}

// --- test helpers ---

type ephemeralSource struct{}

func (ephemeralSource) Kind() SourceKind { return SourceWorkspace }
func (ephemeralSource) Collect(_ context.Context, req CollectRequest) ([]ContextFragment, error) {
	return []ContextFragment{
		{
			Slot: SlotRuntime, Stability: StabilityEphemeral,
			Priority: 10, Source: "workspace", Role: RoleSystem,
			Content:   "debug info that should be trimmed",
			TokenCost: 100,
		},
	}, nil
}

type hugeSource struct{}

func (hugeSource) Kind() SourceKind { return SourceConversation }
func (hugeSource) Collect(_ context.Context, req CollectRequest) ([]ContextFragment, error) {
	return []ContextFragment{
		{
			Slot: SlotConversation, Stability: StabilityDynamic,
			Priority: 50, Source: "conversation", Role: RoleUser,
			Content:   "a very long conversation message that takes lots of tokens",
			TokenCost: 500,
		},
		{
			Slot: SlotConversation, Stability: StabilityDynamic,
			Priority: 50, Source: "conversation", Role: RoleAssistant,
			Content:   "another very long conversation response",
			TokenCost: 500,
		},
	}, nil
}
