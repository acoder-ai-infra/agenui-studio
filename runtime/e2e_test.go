package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/demo"
)

func TestCheckedInPackageRunsEndToEnd(t *testing.T) {
	raw, err := os.ReadFile("testdata/sample-package.json")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mock/products", func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("query") != "city" || request.Header.Get("X-Demo-Run") != "e2e" {
			t.Fatalf("query=%q header=%q", request.URL.Query().Get("query"), request.Header.Get("X-Demo-Run"))
		}
		_, _ = w.Write(demo.MustRead("mock/products.json"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	fetcher := NewHTTPFetcher(0)
	fetcher.Resolver = func(endpoint string) string {
		if index := strings.Index(endpoint, "/mock/"); index >= 0 {
			return server.URL + endpoint[index:]
		}
		return endpoint
	}
	result, err := NewEngine(fetcher, nil).Execute(context.Background(), pkg, map[string]any{"query": "city", "trace": "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	items := result.DataModel["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["price"] != "¥68.00" || items[1].(map[string]any)["distance"] != "12.4km" {
		t.Fatalf("items=%#v", items)
	}
	if len(result.Actions) != 1 || result.Actions[0].Value != "https://example.com/products" {
		t.Fatalf("actions=%#v", result.Actions)
	}
}
