package context

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInMemorySessionStoreCreateAndGet(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	sess := &Session{ID: "s1", UserID: "u1", TenantID: "t1", Status: "active"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "s1" || got.UserID != "u1" || got.TenantID != "t1" {
		t.Fatalf("unexpected session: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at should be set")
	}
}

func TestInMemorySessionStoreCreateMissingID(t *testing.T) {
	store := NewInMemorySessionStore()
	err := store.Create(context.Background(), &Session{UserID: "u1"})
	if !errors.Is(err, ErrSessionIDMissing) {
		t.Fatalf("expected ErrSessionIDMissing, got: %v", err)
	}
}

func TestInMemorySessionStoreGetNotFound(t *testing.T) {
	store := NewInMemorySessionStore()
	_, err := store.Get(context.Background(), "nonexistent")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got: %v", err)
	}
}

func TestInMemorySessionStoreUpdate(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	sess := &Session{ID: "s1", UserID: "u1", Status: "active"}
	_ = store.Create(ctx, sess)

	updated := &Session{ID: "s1", UserID: "u1", Status: "completed", CurrentTurn: 5}
	if err := store.Update(ctx, updated); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := store.Get(ctx, "s1")
	if got.Status != "completed" || got.CurrentTurn != 5 {
		t.Fatalf("update not applied: %+v", got)
	}
}

func TestInMemorySessionStoreUpdateNotFound(t *testing.T) {
	store := NewInMemorySessionStore()
	err := store.Update(context.Background(), &Session{ID: "nonexistent"})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got: %v", err)
	}
}

func TestInMemorySessionStoreDelete(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	_ = store.Create(ctx, &Session{ID: "s1", UserID: "u1"})
	if err := store.Delete(ctx, "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := store.Get(ctx, "s1")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound after delete, got: %v", err)
	}
}

func TestInMemorySessionStoreList(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	_ = store.Create(ctx, &Session{ID: "s1", UserID: "alice"})
	_ = store.Create(ctx, &Session{ID: "s2", UserID: "bob"})
	_ = store.Create(ctx, &Session{ID: "s3", UserID: "alice"})

	// List all.
	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(all))
	}

	// List by user.
	alice, err := store.List(ctx, "alice")
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(alice) != 2 {
		t.Fatalf("expected 2 alice sessions, got %d", len(alice))
	}
}

func TestInMemorySessionStoreCloneOnRead(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	_ = store.Create(ctx, &Session{ID: "s1", UserID: "u1", Status: "active"})

	got, _ := store.Get(ctx, "s1")
	got.Status = "mutated"

	got2, _ := store.Get(ctx, "s1")
	if got2.Status != "active" {
		t.Fatalf("store should clone on read, got mutated status: %s", got2.Status)
	}
}

func TestInMemorySessionStoreConcurrency(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			id := "concurrent"
			_ = store.Create(ctx, &Session{ID: id, UserID: "u1", Status: "active"})
			_, _ = store.Get(ctx, id)
			_ = store.Update(ctx, &Session{ID: id, UserID: "u1", Status: "updated"})
			_, _ = store.List(ctx, "u1")
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}

func TestSessionTimestamps(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	before := time.Now()
	_ = store.Create(ctx, &Session{ID: "s1", UserID: "u1"})
	after := time.Now()

	got, _ := store.Get(ctx, "s1")
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Fatalf("created_at out of range: %v", got.CreatedAt)
	}
}
