package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errorReader) Close() error             { return nil }

type panicFetcher struct{}

func (panicFetcher) Fetch(context.Context, DataSource, map[string]any) (any, error) {
	panic("fetch panic")
}

func TestHTTPFetcherGETResolverQueryAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Query().Get("q") != "city" || request.URL.Query().Get("fixed") != "1" || request.Header.Get("X-Trace") != "trace-7" {
			t.Fatalf("request=%s %s headers=%v", request.Method, request.URL.String(), request.Header)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	fetcher := NewHTTPFetcher(time.Second)
	fetcher.Resolver = func(string) string { return server.URL + "?fixed=1" }
	value, err := fetcher.Fetch(context.Background(), DataSource{
		ID: "ds", Endpoint: "mock://source", Headers: map[string]string{"X-Trace": "trace-{{id}}"},
		Params: []Param{{Name: "q", Template: "{{query}}"}},
	}, map[string]any{"query": "city", "id": 7})
	if err != nil || value.(map[string]any)["ok"] != true {
		t.Fatalf("value=%#v error=%v", value, err)
	}
	if fetcher.Client.Timeout != time.Second || NewHTTPFetcher(0).Client.Timeout != 10*time.Second {
		t.Fatal("timeout defaults")
	}
}

func TestHTTPFetcherPOSTBodyAndParamValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" || string(body) != `{"count":2,"name":"museum"}` {
			t.Fatalf("method=%s contentType=%s body=%s", request.Method, request.Header.Get("Content-Type"), body)
		}
		_, _ = io.WriteString(w, `{"done":1}`)
	}))
	defer server.Close()
	value, err := NewHTTPFetcher(0).Fetch(context.Background(), DataSource{
		ID: "ds", Endpoint: server.URL, Method: http.MethodPost,
		Params: []Param{{Name: "name", In: "body"}, {Name: "count", In: "body", Value: 2}, {Name: "optional"}},
	}, map[string]any{"name": "museum"})
	if err != nil || value.(map[string]any)["done"] != float64(1) {
		t.Fatalf("value=%#v error=%v", value, err)
	}
}

func TestHTTPFetcherFailures(t *testing.T) {
	tests := []struct {
		name    string
		fetcher *HTTPFetcher
		source  DataSource
	}{
		{"required param", NewHTTPFetcher(0), DataSource{ID: "ds", Endpoint: "http://example.invalid", Params: []Param{{Name: "q", Required: true}}}},
		{"body marshal", NewHTTPFetcher(0), DataSource{ID: "ds", Endpoint: "http://example.invalid", Method: "POST", Params: []Param{{Name: "bad", In: "body", Value: make(chan int)}}}},
		{"invalid request", NewHTTPFetcher(0), DataSource{ID: "ds", Endpoint: "://bad"}},
		{"transport", &HTTPFetcher{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport") })}}, DataSource{ID: "ds", Endpoint: "http://example.test"}},
		{"body read", &HTTPFetcher{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: errorReader{}, Header: make(http.Header)}, nil
		})}}, DataSource{ID: "ds", Endpoint: "http://example.test"}},
		{"status", &HTTPFetcher{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":true}`)), Header: make(http.Header)}, nil
		})}}, DataSource{ID: "ds", Endpoint: "http://example.test"}},
		{"invalid json", &HTTPFetcher{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`bad`)), Header: make(http.Header)}, nil
		})}}, DataSource{ID: "ds", Endpoint: "http://example.test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.fetcher.Fetch(context.Background(), test.source, nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	var nilFetcher *HTTPFetcher
	if _, err := nilFetcher.Fetch(context.Background(), DataSource{}, nil); err == nil {
		t.Fatal("nil fetcher succeeded")
	}
	if _, err := (&HTTPFetcher{}).Fetch(context.Background(), DataSource{}, nil); err == nil {
		t.Fatal("fetcher without client succeeded")
	}
}

func TestFetchAllAndSupplementDiagnostics(t *testing.T) {
	pkg := validPackage(t)
	fetcher := &fakeFetcher{responses: productResponses(), fail: map[string]error{"ds-metadata": errors.New("boom")}}
	responses, failures := fetchAll(context.Background(), fetcher, pkg, nil)
	if len(responses) != 1 || len(failures) != 1 {
		t.Fatalf("responses=%#v failures=%#v", responses, failures)
	}
	entities := extractEntities(responses["ds-products"], "$.items")
	diagnostics := mergeSupplements(pkg, entities, responses, failures)
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
}

func TestFetchAllRecoversFetcherPanic(t *testing.T) {
	pkg := validPackage(t)
	_, failures := fetchAll(context.Background(), panicFetcher{}, pkg, nil)
	if len(failures) != len(pkg.DataSources) {
		t.Fatalf("failures=%#v", failures)
	}
}

func TestMergeSupplementEdgeCases(t *testing.T) {
	base := validPackage(t)
	primary := []map[string]any{{"id": "p1"}}
	tests := []struct {
		name     string
		mutate   func(*Package, map[string]any)
		wantDiag bool
	}{
		{"missing items", func(_ *Package, responses map[string]any) { responses["ds-metadata"] = map[string]any{} }, true},
		{"items not list", func(_ *Package, responses map[string]any) { responses["ds-metadata"] = map[string]any{"items": "bad"} }, true},
		{"no key", func(pkg *Package, _ map[string]any) {
			pkg.DataSources[0].EntityKey = ""
			pkg.DataSources[1].EntityKey = ""
		}, true},
		{"non object and missing key", func(_ *Package, responses map[string]any) {
			responses["ds-metadata"] = map[string]any{"items": []any{"bad", map[string]any{"rating": 1}}}
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := *base
			pkg.DataSources = append([]DataSource(nil), base.DataSources...)
			responses := productResponses()
			test.mutate(&pkg, responses)
			entities := []map[string]any{deepCopyMap(primary[0])}
			diagnostics := mergeSupplements(&pkg, entities, responses, nil)
			if (len(diagnostics) > 0) != test.wantDiag {
				t.Fatalf("diagnostics=%#v entities=%#v", diagnostics, entities)
			}
		})
	}
	t.Run("matched", func(t *testing.T) {
		entities := []map[string]any{{"id": "p1"}}
		if diagnostics := mergeSupplements(base, entities, productResponses(), nil); len(diagnostics) != 0 {
			t.Fatalf("diagnostics=%#v", diagnostics)
		}
		if _, ok := entities[0]["@supplement:ds-metadata"]; !ok {
			t.Fatalf("entities=%#v", entities)
		}
	})
	t.Run("primary entity missing key", func(t *testing.T) {
		entities := []map[string]any{{"name": "missing-id"}}
		if diagnostics := mergeSupplements(base, entities, productResponses(), nil); len(diagnostics) != 0 {
			t.Fatalf("diagnostics=%#v", diagnostics)
		}
	})
	t.Run("default items path", func(t *testing.T) {
		pkg := validPackage(t)
		pkg.DataSources[1].ItemsPath = ""
		entities := []map[string]any{{"id": "p1"}}
		if diagnostics := mergeSupplements(pkg, entities, productResponses(), nil); len(diagnostics) != 0 {
			t.Fatalf("diagnostics=%#v", diagnostics)
		}
	})
}
