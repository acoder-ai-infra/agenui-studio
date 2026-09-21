package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type fakeFetcher struct {
	mu        sync.Mutex
	responses map[string]any
	fail      map[string]error
	calls     []string
}

func (f *fakeFetcher) Fetch(_ context.Context, source DataSource, _ map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, source.ID)
	if err := f.fail[source.ID]; err != nil {
		return nil, err
	}
	return f.responses[source.ID], nil
}

func schema(raw string) json.RawMessage { return json.RawMessage(raw) }

func testOperator(id uint64, key, source string, input, params, output json.RawMessage) Operator {
	return Operator{
		OperatorVersionID: id, OperatorKey: key, Version: 1,
		InputSchema: input, ParamsSchema: params, OutputSchema: output,
		SourceHash: hashContent(source), Language: "javascript", LanguageVersion: "ES2022",
		SourceCode: source, Entry: "run",
	}
}

func moneyOperator() Operator {
	return testOperator(101, "agenui.scalar.format_money",
		`function run(value, params) { return (params.currency || "¥") + (value / 100).toFixed(2); }`,
		schema(`{"type":"number"}`),
		schema(`{"type":"object","properties":{"currency":{"type":"string","maxLength":8}},"additionalProperties":false}`),
		schema(`{"type":"string"}`),
	)
}

func distanceOperator() Operator {
	return testOperator(102, "agenui.scalar.format_distance",
		`function run(value) { return value < 1000 ? value + "m" : (value / 1000).toFixed(1) + "km"; }`,
		schema(`{"type":"number"}`), schema(`{"type":"object","additionalProperties":false}`), schema(`{"type":"string"}`),
	)
}

func validPackage(t *testing.T) *Package {
	t.Helper()
	pkg := &Package{
		Version: PackageVersion, CardID: "card-demo", Name: "Product list",
		Protocol: []json.RawMessage{
			schema(`{"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"agenui"}}`),
			schema(`{"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"Column","children":["title","products","more"]},{"id":"title","component":"Text","text":{"path":"/title"}},{"id":"products","component":"List","children":{"componentId":"product","path":"/items"}},{"id":"product","component":"Column","children":["name","price","distance","rating"]},{"id":"name","component":"Text","text":{"path":"name"}},{"id":"price","component":"Text","text":{"path":"price"}},{"id":"distance","component":"Text","text":{"path":"distance"}},{"id":"rating","component":"Text","text":{"path":"rating"}},{"id":"more","component":"Button","child":"more-label"},{"id":"more-label","component":"Text","text":"More"}]}}`),
			schema(`{"version":"v0.9","updateDataModel":{"surfaceId":"default","path":"/","value":{"title":"Products","items":[],"__actions":{"more":{"payload":{}}}}}}`),
		},
		DataSources: []DataSource{
			{ID: "ds-products", Endpoint: "mock://products", Method: "GET", Role: RolePrimary, ItemsPath: "$.items", EntityKey: "id"},
			{ID: "ds-metadata", Endpoint: "mock://metadata", Method: "GET", Role: RoleSupplement, ItemsPath: "$.items", EntityKey: "id"},
		},
		Bindings: []Binding{
			{SlotID: "name.field", RequirementID: "product.name", DataSourceID: "ds-products", FieldPath: "$.items[*].title", RefKey: "/items[*]/name", MissingPolicy: MissingBlock},
			{SlotID: "price.field", RequirementID: "product.price", DataSourceID: "ds-products", FieldPath: "$.items[*].price_cents", RefKey: "/items[*]/price", MissingPolicy: MissingBlock, Transforms: []Invocation{{OperatorVersionID: 101, Params: map[string]any{"currency": "¥"}}}},
			{SlotID: "distance.field", RequirementID: "product.distance", DataSourceID: "ds-products", FieldPath: "$.items[*].distance_meters", RefKey: "/items[*]/distance", MissingPolicy: MissingHide, Transforms: []Invocation{{OperatorVersionID: 102}}},
			{SlotID: "rating.field", RequirementID: "product.rating", DataSourceID: "ds-metadata", FieldPath: "$.items[*].rating", RefKey: "/items[*]/rating", MissingPolicy: MissingHide},
		},
		Operators: []Operator{moneyOperator(), distanceOperator()},
		Actions:   []Action{{ID: "more", Name: "More", Type: "url", Payload: schema(`{"dataSourceId":"ds-products","path":"$.catalog_url","componentId":"more"}`)}},
		Meta: Meta{
			ContractHash: "sha256:" + strings.Repeat("1", 64), DesignHash: "sha256:" + strings.Repeat("2", 64),
			RequirementsHash: "sha256:" + strings.Repeat("3", 64), Generator: "test",
		},
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("valid package: %v", err)
	}
	return pkg
}

func productResponses() map[string]any {
	return map[string]any{
		"ds-products": map[string]any{
			"items": []any{
				map[string]any{"id": "p1", "title": "Museum", "price_cents": float64(6800), "distance_meters": float64(850)},
				map[string]any{"id": "p2", "title": "Lake", "price_cents": float64(12800), "distance_meters": float64(12400)},
			},
			"catalog_url": "https://example.com/products",
		},
		"ds-metadata": map[string]any{"items": []any{map[string]any{"id": "p1", "rating": float64(4.8)}}},
	}
}

func requireErrorCode(t *testing.T, err error, code string) *ExecutionError {
	t.Helper()
	typed, ok := err.(*ExecutionError)
	if !ok || typed.Code != code {
		t.Fatalf("error=%T %v, want code %s", err, err, code)
	}
	return typed
}

func jsonEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func mustJSON(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func failingFetcher(message string) *fakeFetcher {
	return &fakeFetcher{fail: map[string]error{"ds-products": fmt.Errorf("%s", message)}}
}
