package context

import (
	"context"
	"errors"
	"testing"
	"time"
)

func msg(role RoleType, content string) Message {
	return Message{Role: role, Content: content, Timestamp: time.Now()}
}

func TestInMemoryTimelineAppendAndGetAll(t *testing.T) {
	tl := NewInMemoryTimeline()
	ctx := context.Background()

	err := tl.Append(ctx, "s1", msg(RoleUser, "hello"), msg(RoleAssistant, "hi"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	msgs, err := tl.GetAll(ctx, "s1")
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" || msgs[1].Content != "hi" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

func TestInMemoryTimelineAppendMissingSessionID(t *testing.T) {
	tl := NewInMemoryTimeline()
	err := tl.Append(context.Background(), "", msg(RoleUser, "x"))
	if !errors.Is(err, ErrSessionIDMissing) {
		t.Fatalf("expected ErrSessionIDMissing, got: %v", err)
	}
}

func TestInMemoryTimelineGetAllEmpty(t *testing.T) {
	tl := NewInMemoryTimeline()
	msgs, err := tl.GetAll(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if msgs != nil {
		t.Fatalf("expected nil for empty session, got %d", len(msgs))
	}
}

func TestInMemoryTimelineTruncate(t *testing.T) {
	tl := NewInMemoryTimeline()
	ctx := context.Background()

	_ = tl.Append(ctx, "s1",
		msg(RoleUser, "m1"),
		msg(RoleAssistant, "m2"),
		msg(RoleUser, "m3"),
		msg(RoleAssistant, "m4"),
		msg(RoleUser, "m5"),
	)

	err := tl.Truncate(ctx, "s1", 3)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}

	msgs, _ := tl.GetAll(ctx, "s1")
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after truncate, got %d", len(msgs))
	}
	if msgs[0].Content != "m3" {
		t.Fatalf("expected first message to be m3, got %s", msgs[0].Content)
	}
}

func TestInMemoryTimelineTruncateNoOp(t *testing.T) {
	tl := NewInMemoryTimeline()
	ctx := context.Background()

	_ = tl.Append(ctx, "s1", msg(RoleUser, "m1"))

	// keepLast > len → no-op
	_ = tl.Truncate(ctx, "s1", 100)
	msgs, _ := tl.GetAll(ctx, "s1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	// keepLast = 0 → no-op
	_ = tl.Truncate(ctx, "s1", 0)
	msgs, _ = tl.GetAll(ctx, "s1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

func TestInMemoryTimelineLen(t *testing.T) {
	tl := NewInMemoryTimeline()
	ctx := context.Background()

	n, err := tl.Len(ctx, "s1")
	if err != nil {
		t.Fatalf("len: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}

	_ = tl.Append(ctx, "s1", msg(RoleUser, "a"), msg(RoleUser, "b"))
	n, _ = tl.Len(ctx, "s1")
	if n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}
}

func TestInMemoryTimelineCloneOnRead(t *testing.T) {
	tl := NewInMemoryTimeline()
	ctx := context.Background()

	_ = tl.Append(ctx, "s1", msg(RoleUser, "original"))

	msgs, _ := tl.GetAll(ctx, "s1")
	msgs[0].Content = "mutated"

	msgs2, _ := tl.GetAll(ctx, "s1")
	if msgs2[0].Content != "original" {
		t.Fatalf("timeline should clone on read, got: %s", msgs2[0].Content)
	}
}

func TestInMemoryTimelineConcurrency(t *testing.T) {
	tl := NewInMemoryTimeline()
	ctx := context.Background()

	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = tl.Append(ctx, "s1", msg(RoleUser, "x"))
			_, _ = tl.GetAll(ctx, "s1")
			_, _ = tl.Len(ctx, "s1")
		}()
	}
	for i := 0; i < 50; i++ {
		<-done
	}

	n, _ := tl.Len(ctx, "s1")
	if n != 50 {
		t.Fatalf("expected 50 messages, got %d", n)
	}
}
