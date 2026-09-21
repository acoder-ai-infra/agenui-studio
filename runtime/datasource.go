package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Fetcher abstracts one data source call. The embedded default speaks plain
// HTTP; hosts can plug custom transports (service mesh, signed requests,
// mocks) without touching the engine.
type Fetcher interface {
	Fetch(ctx context.Context, ds DataSource, params map[string]any) (any, error)
}

// EndpointResolver optionally rewrites a data source endpoint before fetch.
// Demo tooling uses it to route packaged endpoints at an in-process mock
// server.
type EndpointResolver func(endpoint string) string

// HTTPFetcher is the embedded default Fetcher.
type HTTPFetcher struct {
	Client   *http.Client
	Resolver EndpointResolver
}

func NewHTTPFetcher(timeout time.Duration) *HTTPFetcher {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPFetcher{Client: &http.Client{Timeout: timeout}}
}

func (f *HTTPFetcher) Fetch(ctx context.Context, ds DataSource, params map[string]any) (any, error) {
	if f == nil || f.Client == nil {
		return nil, fmt.Errorf("runtime: HTTP fetcher is unavailable")
	}
	endpoint := ds.Endpoint
	if f.Resolver != nil {
		endpoint = f.Resolver(endpoint)
	}
	method := ds.Method
	if method == "" {
		method = http.MethodGet
	}
	query := url.Values{}
	var body map[string]any
	for _, param := range ds.Params {
		var rendered any
		switch {
		case param.Template != "":
			rendered = renderParam(param.Template, params)
		case param.Value != nil:
			rendered = param.Value
		default:
			if v, ok := params[param.Name]; ok {
				rendered = v
			} else if param.Required {
				return nil, fmt.Errorf("runtime: data source %s: missing required param %q", ds.ID, param.Name)
			} else {
				continue
			}
		}
		switch param.In {
		case "body":
			if body == nil {
				body = map[string]any{}
			}
			body[param.Name] = rendered
		default:
			query.Set(param.Name, fmt.Sprintf("%v", rendered))
		}
	}
	target := endpoint
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range ds.Headers {
		req.Header.Set(k, fmt.Sprintf("%v", renderParam(v, params)))
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runtime: data source %s: %w", ds.ID, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("runtime: data source %s: status %d", ds.ID, resp.StatusCode)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("runtime: data source %s: invalid JSON response", ds.ID)
	}
	return decoded, nil
}

// fetchAll calls all data sources concurrently. Per the bounded-composition
// rule, sources are one-level and parallel: no source consumes another
// source's output. It returns decoded responses plus per-source errors so the
// engine can apply the degrade semantics (primary fails the run, supplements
// degrade their bound slots).
func fetchAll(ctx context.Context, fetcher Fetcher, pkg *Package, params map[string]any) (map[string]any, map[string]error) {
	results := make(map[string]any, len(pkg.DataSources))
	errs := make(map[string]error, len(pkg.DataSources))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ds := range pkg.DataSources {
		wg.Add(1)
		go func(ds DataSource) {
			defer wg.Done()
			var data any
			var err error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						err = fmt.Errorf("runtime: data source %s panicked: %v", ds.ID, recovered)
					}
				}()
				data, err = fetcher.Fetch(ctx, ds, params)
			}()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[ds.ID] = err
				return
			}
			results[ds.ID] = data
		}(ds)
	}
	wg.Wait()
	return results, errs
}

// mergeSupplements applies one-level, parallel, same-entity supplementation:
// each supplement response is indexed by its entity key and its bound fields
// are copied into the matching primary entity. No chaining, no cycles.
func mergeSupplements(pkg *Package, entities []map[string]any, responses map[string]any, failed map[string]error) []Diagnostic {
	var diagnostics []Diagnostic
	primary, _ := pkg.PrimaryDataSource()
	for _, ds := range pkg.DataSources {
		if ds.Role != RoleSupplement {
			continue
		}
		if _, failedFetch := failed[ds.ID]; failedFetch {
			diagnostics = append(diagnostics, Diagnostic{Level: "warn", Source: ds.ID, Message: "supplement failed; bound optional slots degrade"})
			continue
		}
		resp := responses[ds.ID]
		itemsPath := ds.ItemsPath
		if itemsPath == "" {
			itemsPath = "items"
		}
		rawItems, ok := Extract(resp, itemsPath)
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{Level: "warn", Source: ds.ID, Message: fmt.Sprintf("items path %q not found", itemsPath)})
			continue
		}
		list, ok := rawItems.([]any)
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{Level: "warn", Source: ds.ID, Message: "items path is not a list"})
			continue
		}
		key := ds.EntityKey
		if key == "" {
			key = primary.EntityKey
		}
		if key == "" {
			diagnostics = append(diagnostics, Diagnostic{Level: "warn", Source: ds.ID, Message: "no entity key configured; skipped"})
			continue
		}
		index := make(map[string]map[string]any, len(list))
		for _, item := range list {
			entity, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if kv, ok := Extract(entity, key); ok {
				index[fmt.Sprintf("%v", kv)] = entity
			}
		}
		for _, entity := range entities {
			kv, ok := Extract(entity, key)
			if !ok {
				continue
			}
			if match, found := index[fmt.Sprintf("%v", kv)]; found {
				entity["@supplement:"+ds.ID] = match
			}
		}
	}
	return diagnostics
}
