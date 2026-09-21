// Example: execute the embedded demo Card Execution Package with the
// runtime core library. It starts an in-process mock data source, so it
// runs fully offline:
//
//	go run ./runtime/example
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/AGenUI/agenui-studio/demo"
	"github.com/AGenUI/agenui-studio/runtime"
)

func main() {
	// 1. Load and validate the package.
	packageBytes, err := os.ReadFile("runtime/testdata/sample-package.json")
	if err != nil {
		log.Fatal(err)
	}
	pkg, err := runtime.Load(packageBytes)
	if err != nil {
		log.Fatal(err)
	}

	// 2. Serve the demo data sources in-process and route packaged
	//    endpoints at them. In production you would skip the resolver and
	//    let the runtime call the real endpoints.
	mock := startMock()
	defer mock.Close()
	fetcher := runtime.NewHTTPFetcher(0)
	fetcher.Resolver = func(endpoint string) string {
		if idx := strings.Index(endpoint, "/mock/"); idx >= 0 {
			return mock.URL + endpoint[idx:]
		}
		return endpoint
	}

	// 3. Execute. Orchestration (caching, retries, traffic policy) is up to
	//    the host application; the engine only runs the approved plan.
	engine := runtime.NewEngine(fetcher, nil)
	result, err := engine.Execute(context.Background(), pkg, map[string]any{"query": "city", "trace": "example"})
	if err != nil {
		log.Fatal(err)
	}

	out, _ := json.MarshalIndent(result.DataModel, "", "  ")
	fmt.Println(string(out))
	for _, d := range result.Diagnostics {
		fmt.Printf("[%s] %s\n", d.Level, d.Message)
	}
}

func startMock() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/mock/products", serve(demo.MustRead("mock/products.json")))
	return httptest.NewServer(mux)
}

func serve(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}
