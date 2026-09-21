package harness

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

func TestHTTPHandlerReturnsTheComposedSourceHandler(t *testing.T) {
	want := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/sessions" {
			t.Errorf("path = %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	engine := newEngine(&kernel.Kernel{Handler: want}, BuildReport{}, 0)
	handler, err := HTTPHandler(engine)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
}
