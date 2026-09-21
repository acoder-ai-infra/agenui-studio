package context

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCurrentInputMarkerIsNotSnapshotFact(t *testing.T) {
	message := Message{ID: "m1", Role: RoleUser, Content: "hello"}
	withoutMarker := hashSnapshotFacts([]Message{message}, nil, nil)
	message.CurrentInput = true
	withMarker := hashSnapshotFacts([]Message{message}, nil, nil)
	if withMarker != withoutMarker {
		t.Fatalf("transient marker changed snapshot hash: without=%s with=%s", withoutMarker, withMarker)
	}
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "current_input") {
		t.Fatalf("transient marker leaked into persisted message: %s", data)
	}
}

func TestMaterializeDerivedResolvesEmptyFactsAcrossLedgerRepresentations(t *testing.T) {
	manager := SnapshotManager{
		Ledger: NewInMemoryMessageLedger(),
		Store:  NewInMemorySnapshotStore(),
		IDs:    func() string { return "snapshot-derived-empty" },
	}
	snapshot, err := manager.MaterializeDerived(context.Background(), "session", "child-run", "agent_gateway_child", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, messages, err := manager.Resolve(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatalf("Resolve() must treat nil and empty fact collections identically: %v", err)
	}
	if resolved.ID != snapshot.ID || len(messages) != 0 {
		t.Fatalf("unexpected derived snapshot: resolved=%+v messages=%+v", resolved, messages)
	}
}

func TestSnapshotRemainsImmutableAfterNewMessage(t *testing.T) {
	ledger := NewInMemoryMessageLedger()
	store := NewInMemorySnapshotStore()
	manager := SnapshotManager{
		Ledger: ledger,
		Store:  store,
		IDs:    func() string { return "snapshot-1" },
		Clock:  func() time.Time { return time.Unix(100, 0) },
	}
	_, _ = ledger.Append(context.Background(), "s1", Message{ID: "m1", Role: RoleUser, Content: "first"})
	snapshot, err := manager.Materialize(context.Background(), "s1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ledger.Append(context.Background(), "s1", Message{ID: "m2", Role: RoleAssistant, Content: "second"})

	resolved, messages, err := manager.Resolve(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContentHash != snapshot.ContentHash || len(messages) != 1 || messages[0].ID != "m1" {
		t.Fatalf("snapshot changed after append: %#v %#v", resolved, messages)
	}
}

func TestSnapshotPrefersHotContextAndFreezesSummary(t *testing.T) {
	ledger := NewInMemoryMessageLedger()
	stored, _ := ledger.Append(context.Background(), "s1", Message{ID: "m1", Role: RoleUser, Content: "durable"})
	manager := SnapshotManager{
		Ledger: ledger,
		Hot: hotContextReaderFunc(func(context.Context, string, int) (HotContextView, error) {
			return HotContextView{
				Messages:  []Message{stored},
				Fragments: []ContextFragment{{Slot: SlotSummary, Role: RoleSystem, Content: "rolling summary", TokenCost: 2}},
				Version:   7,
			}, nil
		}),
		Store: NewInMemorySnapshotStore(),
		IDs:   func() string { return "snapshot-hot" },
	}
	snapshot, err := manager.Materialize(context.Background(), "s1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != "hot_context_cache" || snapshot.HotVersion != 7 || len(snapshot.Fragments) != 1 {
		t.Fatalf("hot context was not frozen: %#v", snapshot)
	}
	snapshot.Fragments[0].Content = "mutated by caller"
	resolved, _, err := manager.Resolve(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Fragments[0].Content != "rolling summary" {
		t.Fatalf("stored snapshot was mutated: %#v", resolved.Fragments)
	}
}

func TestSnapshotFallsBackToLedgerOnlyOnCacheMiss(t *testing.T) {
	ledger := NewInMemoryMessageLedger()
	_, _ = ledger.Append(context.Background(), "s1", Message{ID: "m1", Role: RoleUser, Content: "durable"})
	manager := SnapshotManager{
		Ledger: ledger,
		Hot: hotContextReaderFunc(func(context.Context, string, int) (HotContextView, error) {
			return HotContextView{}, ErrHotContextMiss
		}),
		Store: NewInMemorySnapshotStore(), IDs: func() string { return "snapshot-fallback" },
	}
	snapshot, err := manager.Materialize(context.Background(), "s1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != "message_ledger_fallback" || len(snapshot.MessageIDs) != 1 {
		t.Fatalf("unexpected fallback snapshot: %#v", snapshot)
	}

	manager.Hot = hotContextReaderFunc(func(context.Context, string, int) (HotContextView, error) {
		return HotContextView{}, context.DeadlineExceeded
	})
	manager.IDs = func() string { return "snapshot-error" }
	if _, err := manager.Materialize(context.Background(), "s1", "r2"); err != context.DeadlineExceeded {
		t.Fatalf("cache outage must not silently become a ledger scan: %v", err)
	}
}

func TestDerivedSnapshotWithNoMessagesResolves(t *testing.T) {
	manager := SnapshotManager{
		Ledger: NewInMemoryMessageLedger(),
		Store:  NewInMemorySnapshotStore(),
		IDs:    func() string { return "snapshot-derived" },
		Clock:  func() time.Time { return time.Unix(100, 0) },
	}
	snapshot, err := manager.MaterializeDerived(context.Background(), "s1", "r1", "agent_gateway_child", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, messages, err := manager.Resolve(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContentHash != snapshot.ContentHash || len(messages) != 0 {
		t.Fatalf("derived snapshot changed: %#v messages=%#v", resolved, messages)
	}
}

type hotContextReaderFunc func(context.Context, string, int) (HotContextView, error)

func (f hotContextReaderFunc) Load(ctx context.Context, sessionID string, maxMessages int) (HotContextView, error) {
	return f(ctx, sessionID, maxMessages)
}
