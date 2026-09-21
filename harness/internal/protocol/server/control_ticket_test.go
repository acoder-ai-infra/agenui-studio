package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestResolveControlResumeTokenBindsAuthenticatedActorAndExecution(t *testing.T) {
	codec, err := controlticket.New([]byte("test-control-ticket-secret"))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := codec.Seal(controlticket.Claims{
		TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
		RunID: "run-1", RequestID: "control-1", ResumeToken: "resume-secret",
	}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{ControlTickets: codec}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-1", UserID: "user-1"})
	req := newRequestWithContext(ctx)
	run := &storage.Run{RunID: "run-1", SessionID: "session-1", TenantID: "tenant-1"}
	got, err := deps.resolveControlResumeToken(req, run, "control-1", controlResponseRequest{ControlTicket: ticket})
	if err != nil || got != "resume-secret" {
		t.Fatalf("resolve = %q, %v", got, err)
	}

	wrongUser := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: "tenant-1", UserID: "user-2"})
	if _, err := deps.resolveControlResumeToken(newRequestWithContext(wrongUser), run, "control-1", controlResponseRequest{ControlTicket: ticket}); err == nil {
		t.Fatal("ticket was accepted for a different user")
	}
	if _, err := deps.resolveControlResumeToken(req, run, "control-2", controlResponseRequest{ControlTicket: ticket}); err == nil {
		t.Fatal("ticket was accepted for a different control request")
	}
	index := len(ticket) / 2
	replacement := byte('A')
	if ticket[index] == replacement {
		replacement = 'B'
	}
	tampered := ticket[:index] + string(replacement) + ticket[index+1:]
	if _, err := deps.resolveControlResumeToken(req, run, "control-1", controlResponseRequest{ControlTicket: tampered}); err == nil {
		t.Fatal("tampered ticket was accepted")
	}
}

func newRequestWithContext(ctx context.Context) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
}
