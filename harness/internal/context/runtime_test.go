package context

import (
	"context"
	"testing"
)

func TestNewLiteRuntimeContext(t *testing.T) {
	rt := NewLiteRuntimeContext()
	if rt.Sessions == nil || rt.Timeline == nil || rt.States == nil || rt.Events == nil || rt.Memory == nil {
		t.Fatal("all subsystems should be initialized")
	}
}

func TestRuntimeContextEndToEnd(t *testing.T) {
	rt := NewLiteRuntimeContext()
	ctx := context.Background()

	// 1. Create session.
	sess := &Session{ID: "s1", UserID: "alice", Status: "active"}
	if err := rt.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 2. Append messages to timeline.
	if err := rt.Timeline.Append(ctx, "s1",
		Message{Role: RoleUser, Content: "Hello"},
		Message{Role: RoleAssistant, Content: "Hi Alice!"},
	); err != nil {
		t.Fatalf("append timeline: %v", err)
	}

	// 3. Write state.
	if err := rt.States.Set(ctx, WriteRequest{
		SessionID: "s1", Path: "task.status", Value: "in_progress",
		Writer: Writer{Role: "rule", Name: "rule:start"},
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	// 4. Publish event.
	if err := rt.Events.Publish(ctx, MutationEvent{
		SessionID: "s1", Path: "task.status",
		NewValue: "in_progress", Writer: Writer{Role: "rule", Name: "rule:start"},
	}); err != nil {
		t.Fatalf("publish event: %v", err)
	}

	// 5. Write memory.
	if err := rt.Memory.Put(ctx, &MemoryItem{
		Key: "pref", Value: map[string]any{"lang": "Go"},
		Namespace: []string{"user", "alice"}, Importance: 0.9,
	}); err != nil {
		t.Fatalf("put memory: %v", err)
	}

	// Verify all data.
	got, _ := rt.Sessions.Get(ctx, "s1")
	if got.UserID != "alice" {
		t.Fatalf("session mismatch: %+v", got)
	}
	msgs, _ := rt.Timeline.GetAll(ctx, "s1")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	val, _ := rt.States.Get(ctx, "s1", "task.status")
	if val != "in_progress" {
		t.Fatalf("state mismatch: %v", val)
	}
	history := rt.Events.History("s1")
	if len(history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(history))
	}
	mem, _ := rt.Memory.Get(ctx, []string{"user", "alice"}, "pref")
	if mem.Value["lang"] != "Go" {
		t.Fatalf("memory mismatch: %+v", mem)
	}
}

func TestRuntimeContextSourceBuilderIntegration(t *testing.T) {
	rt := NewLiteRuntimeContext()
	ctx := context.Background()

	// Set up session with conversation.
	_ = rt.Sessions.Create(ctx, &Session{ID: "s1", UserID: "bob", Status: "active"})
	_ = rt.Timeline.Append(ctx, "s1",
		Message{Role: RoleUser, Content: "What is Go?"},
		Message{Role: RoleAssistant, Content: "Go is a programming language."},
		Message{Role: RoleUser, Content: "Tell me more."},
	)

	// Set up Source + Builder.
	system := NewSystemPromptSource("You are a helpful coding assistant.", "HARNESS Agent", "Be concise.")
	conv := NewConversationSource(10)
	registry := NewSourceRegistry(system, conv)
	builder := NewDefaultBuilder(registry, EstimateCounter{}, BuilderConfig{
		MinPreserve: MinPreserveConfig{StablePrefix: true, CurrentInput: true, RecentTurns: 1},
	})

	// Read timeline → build prompt.
	msgs, _ := rt.Timeline.GetAll(ctx, "s1")
	ptrMsgs := make([]*Message, len(msgs))
	for i := range msgs {
		ptrMsgs[i] = &msgs[i]
	}

	result, err := builder.Build(ctx, BuildRequest{
		SessionID:   "s1",
		Generation:  1,
		Messages:    ptrMsgs,
		TokenBudget: ContextBudget{Limit: 10000},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Expect: 3 system messages (instruction+identity+policies) + 3 conversation messages = 6
	if len(result.Messages) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(result.Messages))
	}

	// First should be system prompt.
	if result.Messages[0].Role != RoleSystem {
		t.Fatalf("first message should be system, got %s", result.Messages[0].Role)
	}
	// Last should be user "Tell me more."
	last := result.Messages[len(result.Messages)-1]
	if last.Role != RoleUser || last.Content != "Tell me more." {
		t.Fatalf("last message should be user 'Tell me more.', got %+v", last)
	}
}
