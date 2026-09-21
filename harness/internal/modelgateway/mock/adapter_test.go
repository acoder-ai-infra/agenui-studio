package mock_test

import (
	"context"
	"errors"
	"io"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
)

func TestMockReplay(t *testing.T) {
	m := mock.New("m", mock.Script{Chunks: []mg.NormalizedChunk{
		mock.TokenChunk("a"), mock.UsageChunk(1, 1),
	}})
	stream, err := m.InvokeChat(context.Background(), mg.AdapterRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for {
		_, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("want 2 chunks, got %d", n)
	}
	if m.Calls() != 1 {
		t.Fatalf("want 1 call, got %d", m.Calls())
	}
}

func TestMockInvokeErr(t *testing.T) {
	m := mock.New("m", mock.Script{InvokeErr: &mg.AdapterError{Message: "boom", Retryable: true}})
	if _, err := m.InvokeChat(context.Background(), mg.AdapterRequest{}); err == nil {
		t.Fatal("expected invoke error")
	}
}
