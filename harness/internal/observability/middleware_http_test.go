package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capturingAccessLogger struct {
	entries []AccessEntry
}

func (l *capturingAccessLogger) Log(_ context.Context, entry AccessEntry) {
	l.entries = append(l.entries, entry)
}

func TestHTTPMiddlewareInjectsTrace(t *testing.T) {
	handler := HTTPMiddleware(HTTPMiddlewareConfig{
		ServiceName: "test",
		Logger:      NoopLogger{},
		Tracer:      NewNoopTracer("test"),
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, ok := TraceContextFrom(r.Context())
		if !ok {
			t.Fatal("trace context missing")
		}
		if tc.TraceID == "" || tc.SpanID == "" {
			t.Fatalf("trace identifiers missing: %#v", tc)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", rr.Code)
	}
	if rr.Header().Get(HeaderTraceID) == "" || rr.Header().Get(HeaderSpanID) == "" {
		t.Fatalf("trace headers missing: %#v", rr.Header())
	}
}

func TestHTTPMiddlewareAcceptsStandardTraceHeaders(t *testing.T) {
	handler := HTTPMiddleware(HTTPMiddlewareConfig{
		ServiceName: "test",
		Logger:      NoopLogger{},
		Tracer:      NewNoopTracer("test"),
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, ok := TraceContextFrom(r.Context())
		if !ok || tc.TraceID != "trace_1" || tc.ConversationID != "conversation_1" {
			t.Fatalf("trace context missing: %#v", tc)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(HeaderTraceID, "trace_1")
	req.Header.Set("x-conversation-id", "conversation_1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get(HeaderTraceID) != "trace_1" {
		t.Fatalf("trace header should reuse supplied trace: %#v", rr.Header())
	}
	if rr.Header().Get("x-conversation-id") != "conversation_1" {
		t.Fatalf("conversation response header missing: %#v", rr.Header())
	}
}

func TestHTTPMiddlewareRedactsArtifactDownloadBearerFromAccessPath(t *testing.T) {
	const token = "secret-bearer-token-must-not-be-logged"
	access := &capturingAccessLogger{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/artifacts/download/{token}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := HTTPMiddleware(HTTPMiddlewareConfig{
		ServiceName: "test",
		Logger:      NoopLogger{},
		AccessLog:   access,
		Tracer:      NewNoopTracer("test"),
	})(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/download/"+token, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(access.entries) != 1 {
		t.Fatalf("access entries = %d, want 1", len(access.entries))
	}
	if got := access.entries[0].Path; got != artifactDownloadPathPattern || strings.Contains(got, token) {
		t.Fatalf("access path = %q, want redacted route pattern", got)
	}
}
