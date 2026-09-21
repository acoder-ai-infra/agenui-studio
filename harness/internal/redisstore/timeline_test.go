package redisstore

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

func newTestTimeline(t *testing.T) (*Timeline, Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })
	return NewTimeline(client, Config{}), client
}

// TestTimelineAppendGetAll: basic round-trip.
func TestTimelineAppendGetAll(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)

	msgs := []ctxpkg.Message{
		{ID: "m1", Content: "hello"},
		{ID: "m2", Content: "world"},
	}
	if err := tl.Append(ctx, "sess-1", msgs...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := tl.GetAll(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetAll len: got %d, want 2", len(got))
	}
	if got[0].ID != "m1" || got[1].ID != "m2" {
		t.Fatalf("order mismatch: got %+v", got)
	}
}

// TestTimelineEmptySession: GetAll on empty session returns nil.
func TestTimelineEmptySession(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)
	got, err := tl.GetAll(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if got != nil {
		t.Fatalf("GetAll empty: got %v, want nil", got)
	}
}

// TestTimelineLen: Len returns the correct count.
func TestTimelineLen(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)

	n, err := tl.Len(ctx, "sess-2")
	if err != nil {
		t.Fatalf("Len empty: %v", err)
	}
	if n != 0 {
		t.Fatalf("Len empty: got %d, want 0", n)
	}

	if err := tl.Append(ctx, "sess-2", ctxpkg.Message{ID: "m1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := tl.Append(ctx, "sess-2", ctxpkg.Message{ID: "m2"}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	n, err = tl.Len(ctx, "sess-2")
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != 2 {
		t.Fatalf("Len: got %d, want 2", n)
	}
}

// TestTimelineTruncate: keeps only the last N messages.
func TestTimelineTruncate(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)

	msgs := []ctxpkg.Message{
		{ID: "m1"}, {ID: "m2"}, {ID: "m3"}, {ID: "m4"}, {ID: "m5"},
	}
	if err := tl.Append(ctx, "sess-3", msgs...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Truncate to last 2.
	if err := tl.Truncate(ctx, "sess-3", 2); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	got, err := tl.GetAll(ctx, "sess-3")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after Truncate: got %d messages, want 2", len(got))
	}
	if got[0].ID != "m4" || got[1].ID != "m5" {
		t.Fatalf("Truncate kept wrong messages: got %+v", got)
	}
}

// TestTimelineTruncateNoOp: keepLast >= len is a no-op.
func TestTimelineTruncateNoOp(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)

	if err := tl.Append(ctx, "sess-4", ctxpkg.Message{ID: "m1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// keepLast > len
	if err := tl.Truncate(ctx, "sess-4", 10); err != nil {
		t.Fatalf("Truncate no-op: %v", err)
	}
	n, _ := tl.Len(ctx, "sess-4")
	if n != 1 {
		t.Fatalf("after no-op Truncate: got %d, want 1", n)
	}
	// keepLast = 0
	if err := tl.Truncate(ctx, "sess-4", 0); err != nil {
		t.Fatalf("Truncate 0: %v", err)
	}
	n, _ = tl.Len(ctx, "sess-4")
	if n != 1 {
		t.Fatalf("after Truncate 0: got %d, want 1", n)
	}
}

// TestTimelineEmptySessionID: Append with empty session ID returns error.
func TestTimelineEmptySessionID(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)
	err := tl.Append(ctx, "", ctxpkg.Message{ID: "m1"})
	if err != ctxpkg.ErrSessionIDMissing {
		t.Fatalf("Append empty: got %v, want ErrSessionIDMissing", err)
	}
}

// TestTimelineSessionIDSet: Append sets SessionID on messages.
func TestTimelineSessionIDSet(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)

	if err := tl.Append(ctx, "sess-5", ctxpkg.Message{ID: "m1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := tl.GetAll(ctx, "sess-5")
	if len(got) != 1 || got[0].SessionID != "sess-5" {
		t.Fatalf("SessionID not set: got %+v", got)
	}
}

// TestTimelineAppendOrder: multiple appends preserve global order.
func TestTimelineAppendOrder(t *testing.T) {
	ctx := context.Background()
	tl, _ := newTestTimeline(t)

	if err := tl.Append(ctx, "sess-6", ctxpkg.Message{ID: "a"}); err != nil {
		t.Fatalf("Append a: %v", err)
	}
	if err := tl.Append(ctx, "sess-6", ctxpkg.Message{ID: "b"}, ctxpkg.Message{ID: "c"}); err != nil {
		t.Fatalf("Append b,c: %v", err)
	}
	got, _ := tl.GetAll(ctx, "sess-6")
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].ID != want {
			t.Fatalf("order[%d]: got %q, want %q", i, got[i].ID, want)
		}
	}
}
