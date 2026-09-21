package operatorlocal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
)

func TestDemoImplementsSourceOperatorDetailContract(t *testing.T) {
	mux := http.NewServeMux()
	RegisterDemo(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := operator.NewHTTPDetailClient(operator.HTTPDetailClientConfig{Endpoint: server.URL + DetailPath})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := client.GetOperator(context.Background(), 205)
	if err != nil {
		t.Fatal(err)
	}
	if detail.OperatorVersionID != 205 || detail.OperatorKey != "demo.identity" || detail.Language != "javascript" || detail.Entry != "" || len(detail.InputSchema) == 0 || len(detail.ParamsSchema) == 0 || len(detail.OutputSchema) == 0 {
		t.Fatalf("detail=%+v", detail)
	}
}
