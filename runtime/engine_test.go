package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEngineProjectsBindingsOperatorsSupplementsAndAction(t *testing.T) {
	pkg := validPackage(t)
	result, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := result.DataModel["items"].([]any)
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["name"] != "Museum" || first["price"] != "¥68.00" || first["distance"] != "850m" || first["rating"] != float64(4.8) {
		t.Fatalf("first=%#v", first)
	}
	if second["name"] != "Lake" || second["price"] != "¥128.00" || second["distance"] != "12.4km" {
		t.Fatalf("second=%#v", second)
	}
	if _, exists := second["rating"]; exists {
		t.Fatalf("unmatched supplement leaked: %#v", second)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Binding != "rating.field" {
		t.Fatalf("diagnostics=%#v", result.Diagnostics)
	}
	if len(result.Actions) != 1 || result.Actions[0].Value != "https://example.com/products" {
		t.Fatalf("actions=%#v", result.Actions)
	}
	if result.DataModel["__actions"].(map[string]any)["more"].(map[string]any)["url"] != "https://example.com/products" {
		t.Fatalf("dataModel=%#v", result.DataModel)
	}
	if len(result.Entities) != 2 {
		t.Fatalf("entities=%#v", result.Entities)
	}
	if _, leaked := result.Entities[0]["@supplement:ds-metadata"]; leaked {
		t.Fatal("supplement implementation detail leaked")
	}
}

func TestEngineNoWildcardPassesCompleteListToOperator(t *testing.T) {
	join := testOperator(201, "agenui.list.join",
		`function run(values, params) { return values.join(params.separator); }`,
		schema(`{"type":"array","items":{"type":"string"}}`),
		schema(`{"type":"object","required":["separator"],"properties":{"separator":{"type":"string"}},"additionalProperties":false}`),
		schema(`{"type":"string"}`),
	)
	pkg := validPackage(t)
	pkg.DataSources = pkg.DataSources[:1]
	pkg.Operators = []Operator{join}
	pkg.Bindings = []Binding{{
		SlotID: "tags.field", DataSourceID: "ds-products", FieldPath: "$.tags[*]", RefKey: "/tag_summary",
		MissingPolicy: MissingBlock, Transforms: []Invocation{{OperatorVersionID: 201, Params: map[string]any{"separator": " · "}}},
	}}
	pkg.Actions = nil
	responses := map[string]any{"ds-products": map[string]any{"items": []any{}, "tags": []any{"亲子", "室内", "免费"}}}
	result, err := NewEngine(&fakeFetcher{responses: responses}, nil).Execute(context.Background(), pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.DataModel["tag_summary"] != "亲子 · 室内 · 免费" {
		t.Fatalf("dataModel=%#v", result.DataModel)
	}
}

func TestEngineWildcardCoordinatesForZeroOneManyAndNested(t *testing.T) {
	tests := []struct {
		name     string
		response any
		field    string
		ref      string
		want     any
	}{
		{name: "zero", response: mustJSON(t, `{"items":[]}`), field: "$.items[*].value", ref: "/items[*]/value", want: []any{}},
		{name: "one", response: mustJSON(t, `{"items":[{"value":"a"}]}`), field: "$.items[*].value", ref: "/items[*]/value", want: []any{map[string]any{"value": "a"}}},
		{name: "many", response: mustJSON(t, `{"items":[{"value":"a"},{"value":"b"}]}`), field: "$.items[*].value", ref: "/items[*]/value", want: []any{map[string]any{"value": "a"}, map[string]any{"value": "b"}}},
		{name: "nested", response: mustJSON(t, `{"groups":[{"items":[{"value":"a"},{"value":"b"}]},{"items":[{"value":"c"}]}]}`), field: "$.groups[*].items[*].value", ref: "/groups[*]/items[*]/value", want: []any{
			map[string]any{"items": []any{map[string]any{"value": "a"}, map[string]any{"value": "b"}}},
			map[string]any{"items": []any{map[string]any{"value": "c"}}},
		}},
		{name: "nested empty", response: mustJSON(t, `{"groups":[{"items":[]}]}`), field: "$.groups[*].items[*].value", ref: "/groups[*]/items[*]/value", want: []any{map[string]any{"items": []any{}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg := validPackage(t)
			pkg.DataSources = []DataSource{{ID: "ds-products", Endpoint: "mock://values", Role: RolePrimary}}
			pkg.Bindings = []Binding{{SlotID: "value.field", DataSourceID: "ds-products", FieldPath: tt.field, RefKey: tt.ref, MissingPolicy: MissingBlock}}
			pkg.Operators, pkg.Actions = nil, nil
			result, err := NewEngine(&fakeFetcher{responses: map[string]any{"ds-products": tt.response}}, nil).Execute(context.Background(), pkg, nil)
			if err != nil {
				t.Fatal(err)
			}
			key := strings.TrimSuffix(strings.Split(strings.TrimPrefix(tt.ref, "/"), "/")[0], "[*]")
			if !jsonEqual(result.DataModel[key], tt.want) {
				t.Fatalf("got=%#v want=%#v", result.DataModel[key], tt.want)
			}
		})
	}
}

func TestEngineFixedIndexAndMapKey(t *testing.T) {
	pkg := validPackage(t)
	pkg.DataSources = []DataSource{{ID: "ds-products", Endpoint: "mock://root", Role: RolePrimary}}
	pkg.Bindings = []Binding{
		{SlotID: "fixed.field", DataSourceID: "ds-products", FieldPath: "$.items[1].name", RefKey: "/fixed", MissingPolicy: MissingBlock},
		{SlotID: "map.field", DataSourceID: "ds-products", FieldPath: `$.price_map['vip-member']`, RefKey: "/vip", MissingPolicy: MissingBlock},
	}
	pkg.Operators, pkg.Actions = nil, nil
	response := mustJSON(t, `{"items":[{"name":"a"},{"name":"b"}],"price_map":{"vip-member":99}}`)
	result, err := NewEngine(&fakeFetcher{responses: map[string]any{"ds-products": response}}, nil).Execute(context.Background(), pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.DataModel["fixed"] != "b" || result.DataModel["vip"] != float64(99) {
		t.Fatalf("dataModel=%#v", result.DataModel)
	}
}

func TestEngineMissingPolicies(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy string
		want   any
		code   string
	}{
		{name: "block", policy: MissingBlock, code: CodeMapKeyNotFound},
		{name: "hide", policy: MissingHide},
		{name: "fallback", policy: MissingFallback, want: "fallback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			pkg.DataSources = pkg.DataSources[:1]
			pkg.Bindings = []Binding{{SlotID: "missing.field", DataSourceID: "ds-products", FieldPath: "$.missing", RefKey: "/missing", MissingPolicy: test.policy, FallbackValue: test.want}}
			pkg.Operators, pkg.Actions = nil, nil
			result, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
			if test.code != "" {
				requireErrorCode(t, err, test.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.policy == MissingHide {
				if _, exists := result.DataModel["missing"]; exists || len(result.Diagnostics) != 1 {
					t.Fatalf("result=%#v", result)
				}
			} else if result.DataModel["missing"] != test.want {
				t.Fatalf("dataModel=%#v", result.DataModel)
			}
		})
	}
}

func TestEngineRejectsRealProblemInputShape(t *testing.T) {
	pkg := validPackage(t)
	pkg.DataSources = pkg.DataSources[:1]
	pkg.Bindings = []Binding{{
		SlotID: "product-price.field", DataSourceID: "ds-products", FieldPath: "$.items[*].price_cents", RefKey: "/product_price",
		MissingPolicy: MissingBlock, Transforms: []Invocation{{OperatorVersionID: 101, Params: map[string]any{"currency": "¥"}}},
	}}
	pkg.Operators = pkg.Operators[:1]
	pkg.Actions = nil
	_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	typed := requireErrorCode(t, err, CodeOperatorInputValidationFailed)
	if typed.Executed || typed.OperatorVersionID != 101 || typed.BindingID != "product-price.field" {
		t.Fatalf("error=%#v", typed)
	}
}

func TestEngineOperatorChainValidatesEveryStep(t *testing.T) {
	pick := testOperator(301, "agenui.list.pick",
		`function run(values) { return values[0]; }`, schema(`{"type":"array","minItems":1}`),
		schema(`{"type":"object","additionalProperties":false}`), schema(`{"type":"number"}`))
	pkg := validPackage(t)
	pkg.DataSources = pkg.DataSources[:1]
	pkg.Bindings = []Binding{{
		SlotID: "price.field", DataSourceID: "ds-products", FieldPath: "$.items[*].price_cents", RefKey: "/product_price",
		MissingPolicy: MissingBlock, Transforms: []Invocation{{OperatorVersionID: 301}, {OperatorVersionID: 101, Params: map[string]any{"currency": "¥"}}},
	}}
	pkg.Operators = []Operator{pick, moneyOperator()}
	pkg.Actions = nil
	result, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.DataModel["product_price"] != "¥68.00" {
		t.Fatalf("dataModel=%#v", result.DataModel)
	}
}

func TestEngineOperatorChainIntermediateFailures(t *testing.T) {
	first := testOperator(310, "first", `function run() { return "not-number"; }`,
		schema(`true`), schema(`{"type":"object"}`), schema(`{"type":"string"}`))
	second := testOperator(311, "second", `function run(value) { return value; }`,
		schema(`{"type":"number"}`), schema(`{"type":"object"}`), schema(`{"type":"number"}`))
	pkg := validPackage(t)
	pkg.DataSources = pkg.DataSources[:1]
	pkg.Bindings = []Binding{{
		SlotID: "chain.field", DataSourceID: "ds-products", FieldPath: "$.catalog_url", RefKey: "/value", MissingPolicy: MissingBlock,
		Transforms: []Invocation{{OperatorVersionID: 310}, {OperatorVersionID: 311}},
	}}
	pkg.Operators, pkg.Actions = []Operator{first, second}, nil
	_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	typed := requireErrorCode(t, err, CodeOperatorInputValidationFailed)
	if typed.OperatorVersionID != 311 || typed.Executed {
		t.Fatalf("error=%#v", typed)
	}

	first.OutputSchema = schema(`{"type":"number"}`)
	pkg.Operators = []Operator{first, second}
	_, err = NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	typed = requireErrorCode(t, err, CodeOperatorOutputValidationFailed)
	if typed.OperatorVersionID != 310 || !typed.Executed {
		t.Fatalf("error=%#v", typed)
	}
}

func TestEnginePropagatesOperatorLimitAndReportedCodes(t *testing.T) {
	for _, test := range []struct {
		name     string
		source   string
		config   ExecutorConfig
		wantCode string
	}{
		{"output limit", `function run(){return "long-output";}`, ExecutorConfig{MaxOutputBytes: 3}, CodeOperatorOutputTooLarge},
		{"reported pick", `function run(){throw {code:"PICK_NOT_FOUND"};}`, ExecutorConfig{}, CodePickNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := testOperator(320, "operator", test.source, schema(`true`), schema(`{"type":"object"}`), schema(`true`))
			pkg := validPackage(t)
			pkg.DataSources = pkg.DataSources[:1]
			pkg.Bindings = []Binding{{SlotID: "operator.field", DataSourceID: "ds-products", FieldPath: "$.catalog_url", RefKey: "/value", MissingPolicy: MissingBlock, Transforms: []Invocation{{OperatorVersionID: 320}}}}
			pkg.Operators, pkg.Actions = []Operator{op}, nil
			_, err := NewEngine(&fakeFetcher{responses: productResponses()}, NewJSExecutor(test.config)).Execute(context.Background(), pkg, nil)
			requireErrorCode(t, err, test.wantCode)
		})
	}
}

func TestEngineFailuresAreStrict(t *testing.T) {
	t.Run("missing capabilities", func(t *testing.T) {
		var nilEngine *Engine
		if _, err := nilEngine.Execute(context.Background(), validPackage(t), nil); err == nil {
			t.Fatal("nil engine succeeded")
		}
		if _, err := (&Engine{}).Execute(context.Background(), validPackage(t), nil); err == nil {
			t.Fatal("empty engine succeeded")
		}
	})
	t.Run("nil context", func(t *testing.T) {
		if _, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(nil, validPackage(t), nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("nil package", func(t *testing.T) {
		if _, err := NewEngine(nil, nil).Execute(context.Background(), nil, nil); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("primary fetch", func(t *testing.T) {
		_, err := NewEngine(failingFetcher("boom"), nil).Execute(context.Background(), validPackage(t), nil)
		if err == nil || !strings.Contains(err.Error(), "primary data source") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("supplement fetch hide", func(t *testing.T) {
		fetcher := &fakeFetcher{responses: productResponses(), fail: map[string]error{"ds-metadata": errors.New("boom")}}
		result, err := NewEngine(fetcher, nil).Execute(context.Background(), validPackage(t), nil)
		if err != nil || len(result.Diagnostics) < 2 {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})
	t.Run("join not proven", func(t *testing.T) {
		pkg := validPackage(t)
		pkg.DataSources[1].EntityKey = "other_id"
		_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
		requireErrorCode(t, err, CodeSourceJoinNotProven)
	})
}

func TestEngineSupplementFetchFallbackCoordinates(t *testing.T) {
	pkg := validPackage(t)
	pkg.Bindings = pkg.Bindings[3:4]
	pkg.Bindings[0].MissingPolicy = MissingFallback
	pkg.Bindings[0].FallbackValue = float64(0)
	pkg.Bindings[0].Transforms = []Invocation{{OperatorVersionID: 101, Params: map[string]any{"currency": "¥"}}}
	pkg.Actions = nil
	fetcher := &fakeFetcher{responses: productResponses(), fail: map[string]error{"ds-metadata": errors.New("down")}}
	result, err := NewEngine(fetcher, nil).Execute(context.Background(), pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := result.DataModel["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["rating"] != "¥0.00" || items[1].(map[string]any)["rating"] != "¥0.00" {
		t.Fatalf("items=%#v", items)
	}

	pkg.Bindings[0].RefKey = "/groups[*]/items[*]/rating"
	pkg.Bindings[0].FieldPath = "$.groups[*].items[*].rating"
	_, err = NewEngine(fetcher, nil).Execute(context.Background(), pkg, nil)
	requireErrorCode(t, err, CodeSourceJoinNotProven)

	pkg.Bindings[0].RefKey = "/items[*]/rating"
	pkg.Bindings[0].FieldPath = "$.items[*].rating"
	pkg.Bindings[0].FallbackValue = "bad"
	_, err = NewEngine(fetcher, nil).Execute(context.Background(), pkg, nil)
	requireErrorCode(t, err, CodeOperatorInputValidationFailed)
}

func TestEngineWildcardMissingPolicies(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy string
		code   string
	}{
		{"block", MissingBlock, CodeMapKeyNotFound},
		{"hide", MissingHide, ""},
		{"fallback", MissingFallback, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			pkg.DataSources = pkg.DataSources[:1]
			pkg.Bindings = []Binding{{
				SlotID: "label.field", DataSourceID: "ds-products", FieldPath: "$.items[*].label", RefKey: "/items[*]/label",
				MissingPolicy: test.policy, FallbackValue: "fallback",
			}}
			pkg.Operators, pkg.Actions = nil, nil
			responses := map[string]any{"ds-products": mustJSON(t, `{"items":[{"id":"p1"},{"id":"p2","label":"ok"}]}`)}
			result, err := NewEngine(&fakeFetcher{responses: responses}, nil).Execute(context.Background(), pkg, nil)
			if test.code != "" {
				requireErrorCode(t, err, test.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			items := result.DataModel["items"].([]any)
			if test.policy == MissingFallback && items[0].(map[string]any)["label"] != "fallback" {
				t.Fatalf("items=%#v", items)
			}
			if items[1].(map[string]any)["label"] != "ok" {
				t.Fatalf("items=%#v", items)
			}
		})
	}
}

func TestEngineSupplementPathMustBeUnderItemsPath(t *testing.T) {
	pkg := validPackage(t)
	pkg.Bindings = []Binding{{
		SlotID: "rating.field", DataSourceID: "ds-metadata", FieldPath: "$.other[*].rating", RefKey: "/items[*]/rating", MissingPolicy: MissingHide,
	}}
	pkg.Actions = nil
	_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	requireErrorCode(t, err, CodeBindingWildcardMismatch)
}

func TestEngineExtractionEdges(t *testing.T) {
	if entities := extractEntities(map[string]any{}, ""); entities != nil {
		t.Fatalf("entities=%#v", entities)
	}
	if entities := extractEntities(map[string]any{}, "$.items"); entities != nil {
		t.Fatalf("entities=%#v", entities)
	}
	if entities := extractEntities(map[string]any{"items": "bad"}, "$.items"); entities != nil {
		t.Fatalf("entities=%#v", entities)
	}
	entities := extractEntities(map[string]any{"items": []any{"bad", map[string]any{"id": "ok"}}}, "$.items")
	if len(entities) != 1 || entities[0]["id"] != "ok" {
		t.Fatalf("entities=%#v", entities)
	}
}

func TestEngineOperatorErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Package)
		code   string
	}{
		{name: "params", code: CodeOperatorParamsValidationFailed, mutate: func(pkg *Package) {
			pkg.Bindings[1].Transforms[0].Params["unknown"] = true
		}},
		{name: "output", code: CodeOperatorOutputValidationFailed, mutate: func(pkg *Package) {
			pkg.Operators[0].SourceCode = `function run() { return 1; }`
			pkg.Operators[0].SourceHash = hashContent(pkg.Operators[0].SourceCode)
		}},
		{name: "execute", code: CodeOperatorExecutionFailed, mutate: func(pkg *Package) {
			pkg.Operators[0].SourceCode = `function run() { throw new Error("boom"); }`
			pkg.Operators[0].SourceHash = hashContent(pkg.Operators[0].SourceCode)
		}},
		{name: "timeout", code: CodeOperatorTimeout, mutate: func(pkg *Package) {
			pkg.Operators[0].SourceCode = `function run() { while (true) {} }`
			pkg.Operators[0].SourceHash = hashContent(pkg.Operators[0].SourceCode)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			test.mutate(pkg)
			pkg.DataSources = pkg.DataSources[:1]
			pkg.Bindings = pkg.Bindings[1:2]
			pkg.Operators = pkg.Operators[:1]
			pkg.Actions = nil
			executor := NewJSExecutor(ExecutorConfig{Timeout: time.Millisecond})
			_, err := NewEngine(&fakeFetcher{responses: productResponses()}, executor).Execute(context.Background(), pkg, nil)
			requireErrorCode(t, err, test.code)
		})
	}
}

func TestEngineTargetAssignmentFailure(t *testing.T) {
	pkg := validPackage(t)
	pkg.DataSources = pkg.DataSources[:1]
	pkg.Bindings = []Binding{{SlotID: "bad.field", DataSourceID: "ds-products", FieldPath: "$.catalog_url", RefKey: "/title/value", MissingPolicy: MissingBlock}}
	pkg.Operators, pkg.Actions = nil, nil
	_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	requireErrorCode(t, err, CodeBindingTargetAssignFailed)
}

func TestEngineWildcardTargetAssignmentFailure(t *testing.T) {
	pkg := validPackage(t)
	pkg.DataSources = pkg.DataSources[:1]
	pkg.Bindings = pkg.Bindings[:1]
	pkg.Operators, pkg.Actions = nil, nil
	pkg.Protocol[2] = schema(`{"updateDataModel":{"path":"/","value":{"items":"blocked"}}}`)
	_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
	requireErrorCode(t, err, CodeBindingTargetAssignFailed)

	pkg = validPackage(t)
	pkg.DataSources = []DataSource{{ID: "ds-products", Endpoint: "mock://groups", Role: RolePrimary}}
	pkg.Bindings = []Binding{{
		SlotID: "nested.field", DataSourceID: "ds-products", FieldPath: "$.groups[*].items[*].value", RefKey: "/groups[*]/items[*]/value", MissingPolicy: MissingBlock,
	}}
	pkg.Operators, pkg.Actions = nil, nil
	pkg.Protocol[2] = schema(`{"updateDataModel":{"path":"/","value":{"groups":"blocked"}}}`)
	responses := map[string]any{"ds-products": mustJSON(t, `{"groups":[{"items":[]}]}`)}
	_, err = NewEngine(&fakeFetcher{responses: responses}, nil).Execute(context.Background(), pkg, nil)
	requireErrorCode(t, err, CodeBindingTargetAssignFailed)
}

func TestEngineActionErrorsRemainIndependent(t *testing.T) {
	t.Run("invalid payload", func(t *testing.T) {
		pkg := validPackage(t)
		pkg.Bindings = nil
		pkg.Actions[0].Payload = json.RawMessage(`{`)
		_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing value", func(t *testing.T) {
		pkg := validPackage(t)
		pkg.Bindings = nil
		pkg.Actions[0].Payload = schema(`{"dataSourceId":"ds-products","path":"$.missing","componentId":"more"}`)
		_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("assignment collision", func(t *testing.T) {
		pkg := validPackage(t)
		pkg.Bindings = nil
		pkg.Protocol[2] = schema(`{"updateDataModel":{"path":"/","value":{"__actions":"blocked"}}}`)
		_, err := NewEngine(&fakeFetcher{responses: productResponses()}, nil).Execute(context.Background(), pkg, nil)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestEngineInternalFailureBranches(t *testing.T) {
	engine := NewEngine(nil, nil)
	pkg := validPackage(t)
	result := &Result{DataModel: map[string]any{}}

	t.Run("invalid package before fetch", func(t *testing.T) {
		invalid := validPackage(t)
		invalid.Version = "bad"
		if _, err := engine.Execute(context.Background(), invalid, nil); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("failed scalar source policies", func(t *testing.T) {
		binding := Binding{SlotID: "x", DataSourceID: "ds-metadata", FieldPath: "$.x", RefKey: "/x", MissingPolicy: MissingFallback, FallbackValue: "fallback"}
		if err := engine.applyBinding(context.Background(), pkg, binding, nil, map[string]error{"ds-metadata": errors.New("down")}, nil, result); err != nil {
			t.Fatal(err)
		}
		if result.DataModel["x"] != "fallback" {
			t.Fatalf("dataModel=%#v", result.DataModel)
		}
		binding.MissingPolicy = MissingHide
		if err := engine.applyBinding(context.Background(), pkg, binding, nil, map[string]error{"ds-metadata": errors.New("down")}, nil, result); err != nil {
			t.Fatal(err)
		}
		binding.MissingPolicy = MissingBlock
		if err := engine.applyBinding(context.Background(), pkg, binding, nil, map[string]error{"ds-metadata": errors.New("down")}, nil, result); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("failed source fallback transform and assignment", func(t *testing.T) {
		binding := Binding{SlotID: "x", DataSourceID: "ds-metadata", FieldPath: "$.x", RefKey: "/x", MissingPolicy: MissingFallback, FallbackValue: "bad", Transforms: []Invocation{{OperatorVersionID: 101}}}
		if err := engine.applyBinding(context.Background(), pkg, binding, nil, map[string]error{"ds-metadata": errors.New("down")}, nil, &Result{DataModel: map[string]any{}}); err == nil {
			t.Fatal("expected transform error")
		}
		binding.Transforms = nil
		collision := &Result{DataModel: map[string]any{"x": "blocked"}}
		binding.RefKey = "/x/value"
		if err := engine.applyBinding(context.Background(), pkg, binding, nil, map[string]error{"ds-metadata": errors.New("down")}, nil, collision); err == nil {
			t.Fatal("expected assignment error")
		}
	})

	t.Run("failed wildcard source block and assignment", func(t *testing.T) {
		binding := pkg.Bindings[3]
		binding.MissingPolicy = MissingBlock
		if err := engine.applyBinding(context.Background(), pkg, binding, nil, map[string]error{"ds-metadata": errors.New("down")}, []map[string]any{{"id": "p1"}}, &Result{DataModel: map[string]any{}}); err == nil {
			t.Fatal("expected missing error")
		}
		binding.MissingPolicy = MissingFallback
		binding.FallbackValue = float64(1)
		collision := &Result{DataModel: map[string]any{"items": "blocked"}}
		if err := engine.applyBinding(context.Background(), pkg, binding, nil, map[string]error{"ds-metadata": errors.New("down")}, []map[string]any{{"id": "p1"}}, collision); err == nil {
			t.Fatal("expected assignment error")
		}
	})

	t.Run("relative item token failures", func(t *testing.T) {
		valid, _ := parseFieldPath("$.items[*].rating")
		for _, test := range []struct {
			tokens []pathToken
			path   string
		}{
			{valid, "$bad"},
			{[]pathToken{{kind: pathTokenField, field: "items"}}, "$.items"},
			{valid, "$.other"},
			{[]pathToken{{kind: pathTokenField, field: "items"}, {kind: pathTokenField, field: "rating"}}, "$.items"},
		} {
			if _, ok := relativeItemTokens(test.tokens, test.path); ok {
				t.Fatalf("relativeItemTokens(%#v,%q) succeeded", test.tokens, test.path)
			}
		}
	})

	t.Run("binding match rejects unproven join", func(t *testing.T) {
		binding := pkg.Bindings[3]
		unproven := *pkg
		unproven.DataSources = append([]DataSource(nil), pkg.DataSources...)
		unproven.DataSources[1].EntityKey = "other"
		tokens, _ := parseFieldPath(binding.FieldPath)
		if _, err := bindingMatches(&unproven, binding, nil, tokens, nil); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("runtime params validation", func(t *testing.T) {
		binding := pkg.Bindings[1]
		binding.Transforms[0].Params["extra"] = true
		if _, err := engine.applyTransforms(context.Background(), pkg, binding, float64(1)); err == nil {
			t.Fatal("expected params error")
		}
	})

	t.Run("default missing code", func(t *testing.T) {
		if _, _, err := resolveMissing(Binding{SlotID: "x", MissingPolicy: MissingBlock}, "", nil, result); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid action payload direct", func(t *testing.T) {
		invalid := &Package{Actions: []Action{{ID: "x", Payload: json.RawMessage(`{`)}}}
		if err := applyActions(invalid, nil, map[string]any{}, &Result{}); err == nil {
			t.Fatal("expected error")
		}
	})
}
