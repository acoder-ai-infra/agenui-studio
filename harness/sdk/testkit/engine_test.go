package testkit

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

func TestMockEngineReplaysScenario(t *testing.T) {
	engine := NewMockEngine(Scenario{
		Events: []harness.Event{
			{EventType: harness.EventRunStarted, Visibility: harness.VisibilityDebug},
			TextDeltaEvent("hi"),
			FinalResponseEvent("done"),
			TerminalEvent(harness.EventRunCompleted),
		},
		Result: harness.ResultView{Content: "done"},
	})
	defer engine.Close(context.Background())

	exec, err := engine.Start(context.Background(), harness.StartRequest{
		Identity: harness.Identity{TenantID: "t"},
		Input:    harness.TextMessage("hello"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	stream := exec.Events()
	got := 0
	for {
		_, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("next: %v", err)
		}
		got++
	}
	if got != 4 {
		t.Fatalf("expected 4 events, got %d", got)
	}
}

func TestMockEngineHonoursBreakAfter(t *testing.T) {
	engine := NewMockEngine(Scenario{
		Events: []harness.Event{
			TextDeltaEvent("a"),
			TextDeltaEvent("b"),
			TextDeltaEvent("c"),
			TerminalEvent(harness.EventRunCompleted),
		},
		StreamPolicy: StreamPolicy{BreakAfter: 2},
	})
	defer engine.Close(context.Background())

	exec, err := engine.Start(context.Background(), harness.StartRequest{
		Identity: harness.Identity{TenantID: "t"},
		Input:    harness.TextMessage("hi"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	stream := exec.Events()
	seen := 0
	for {
		_, err := stream.Next(context.Background())
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("next: %v", err)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("break_after=2 should yield 2 events, got %d", seen)
	}
}

func TestMockEngineRejectsInvalidStart(t *testing.T) {
	engine := NewMockEngine(Scenario{})
	if _, err := engine.Start(context.Background(), harness.StartRequest{}); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
}

func TestMockEngineClosedAfterClose(t *testing.T) {
	engine := NewMockEngine(Scenario{})
	if err := engine.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := engine.Start(context.Background(), harness.StartRequest{
		Identity: harness.Identity{TenantID: "t"},
		Input:    harness.TextMessage("x"),
	}); !errors.Is(err, harness.ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}
