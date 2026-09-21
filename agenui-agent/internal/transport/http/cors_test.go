package httptransport

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// sentinelHandler is a comparable (pointer) http.Handler so tests can assert
// that CORS returns the next handler unchanged. http.HandlerFunc values are not
// comparable and panic under ==.
type sentinelHandler struct{}

func (*sentinelHandler) ServeHTTP(response http.ResponseWriter, _ *http.Request) {
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("ok"))
}

func okHandler() http.Handler { return &sentinelHandler{} }

func TestCORS_EmptyAllowListReturnsHandlerUnchanged(t *testing.T) {
	next := okHandler()
	if got := CORS(nil, next); got != next {
		t.Fatalf("empty allow list must return the next handler unchanged")
	}
	if got := CORS([]string{"", "   "}, next); got != next {
		t.Fatalf("blank-only allow list must return the next handler unchanged")
	}
}

func TestCORS_AllowedOriginReflected(t *testing.T) {
	handler := CORS([]string{"https://studio.example.test"}, okHandler())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agenui_main/chat", nil)
	request.Header.Set("Origin", "https://studio.example.test")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected passthrough 200, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://studio.example.test" {
		t.Fatalf("expected reflected origin, got %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("expected credentials true, got %q", got)
	}
	if got := recorder.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("expected Vary: Origin, got %q", got)
	}
}

func TestCORS_DisallowedOriginNoHeadersButServed(t *testing.T) {
	handler := CORS([]string{"https://studio.example.test"}, okHandler())
	request := httptest.NewRequest(http.MethodPost, "/x", nil)
	request.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("non-preflight request must still be served, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin must not receive ACAO, got %q", got)
	}
}

func TestCORS_PreflightAllowed(t *testing.T) {
	handler := CORS([]string{"https://studio.example.test"}, okHandler())
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/agents/agenui_main/chat", nil)
	request.Header.Set("Origin", "https://studio.example.test")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "content-type, idempotency-key")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for preflight, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Methods"); got != corsAllowedMethods {
		t.Fatalf("expected allow-methods %q, got %q", corsAllowedMethods, got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Headers"); got != "content-type, idempotency-key" {
		t.Fatalf("expected reflected request headers, got %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Max-Age"); got != corsMaxAgeSeconds {
		t.Fatalf("expected max-age %q, got %q", corsMaxAgeSeconds, got)
	}
}

func TestCORS_PreflightDisallowedOriginShortCircuits(t *testing.T) {
	served := false
	next := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		served = true
		response.WriteHeader(http.StatusOK)
	})
	handler := CORS([]string{"https://studio.example.test"}, next)
	request := httptest.NewRequest(http.MethodOptions, "/x", nil)
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Access-Control-Request-Method", "POST")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if served {
		t.Fatalf("preflight from disallowed origin must not reach the mux")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed preflight must not receive ACAO, got %q", got)
	}
}

func TestCORS_WildcardReflectsAnyOrigin(t *testing.T) {
	handler := CORS([]string{"*"}, okHandler())
	request := httptest.NewRequest(http.MethodPost, "/x", nil)
	request.Header.Set("Origin", "https://anything.example")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://anything.example" {
		t.Fatalf("wildcard must reflect the request origin, got %q", got)
	}
}
