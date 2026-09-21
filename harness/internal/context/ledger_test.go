package context

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestMessageLedgerAllocatesStableIdempotentSequence(t *testing.T) {
	ledger := NewInMemoryMessageLedger()
	first, err := ledger.Append(context.Background(), "s1", Message{ID: "m1", Role: RoleUser, Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := ledger.Append(context.Background(), "s1", Message{ID: "m1", Role: RoleUser, Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || retry.Sequence != first.Sequence {
		t.Fatalf("sequence is not stable: first=%d retry=%d", first.Sequence, retry.Sequence)
	}
	_, err = ledger.Append(context.Background(), "s1", Message{ID: "m1", Role: RoleUser, Content: "changed"})
	if !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func TestMessageLedgerDoesNotPersistCurrentInputMarker(t *testing.T) {
	ledger := NewInMemoryMessageLedger()
	message := Message{ID: "m_current", IdempotencyKey: "idem_current", Role: RoleUser, Content: "hello", CurrentInput: true}
	stored, err := ledger.Append(context.Background(), "s1", message)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CurrentInput {
		t.Fatal("transient current-input marker was persisted")
	}
	message.CurrentInput = false
	if _, err := ledger.Append(context.Background(), "s1", message); err != nil {
		t.Fatalf("marker-only retry conflicted: %v", err)
	}
}

func TestMessageLedgerConcurrentAppendIsOrderedAndComplete(t *testing.T) {
	ledger := NewInMemoryMessageLedger()
	const count = 256
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ledger.Append(context.Background(), "s1", Message{
				ID:      fmt.Sprintf("m-%03d", i),
				Role:    RoleUser,
				Content: fmt.Sprintf("content-%03d", i),
			})
			if err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	messages, err := ledger.List(context.Background(), "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != count {
		t.Fatalf("got %d messages, want %d", len(messages), count)
	}
	for i, msg := range messages {
		if msg.Sequence != int64(i+1) {
			t.Fatalf("sequence[%d]=%d", i, msg.Sequence)
		}
	}
}

func TestMessageLedgerDeepClonesStructuredFacts(t *testing.T) {
	ledger := NewInMemoryMessageLedger()
	arguments := map[string]any{"filters": []any{map[string]any{"city": "hangzhou"}}}
	extra := map[string]any{"nested": map[string]any{"enabled": true}}
	stored, err := ledger.Append(context.Background(), "session", Message{
		ID: "message", Role: RoleAssistant,
		ToolCalls: []ToolCall{{ID: "call", Name: "search", Arguments: arguments}},
		Extra:     extra,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments["filters"].([]any)[0].(map[string]any)["city"] = "shanghai"
	extra["nested"].(map[string]any)["enabled"] = false
	stored.ToolCalls[0].Arguments["filters"].([]any)[0].(map[string]any)["city"] = "beijing"

	messages, err := ledger.List(context.Background(), "session", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if city := messages[0].ToolCalls[0].Arguments["filters"].([]any)[0].(map[string]any)["city"]; city != "hangzhou" {
		t.Fatalf("stored arguments were mutated: %v", city)
	}
	if enabled := messages[0].Extra["nested"].(map[string]any)["enabled"]; enabled != true {
		t.Fatalf("stored metadata was mutated: %v", enabled)
	}
}
