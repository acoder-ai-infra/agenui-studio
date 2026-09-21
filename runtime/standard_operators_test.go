package runtime

import (
	"context"
	"testing"
)

func listOperatorFixtures() map[uint64]Operator {
	objectParams := schema(`{"type":"object","additionalProperties":true}`)
	return map[uint64]Operator{
		401: testOperator(401, "agenui.list.join", `function run(values, p) { return values.map(function(v){ return p.field ? v[p.field] : v; }).join(p.separator || " | "); }`,
			schema(`{"type":"array"}`), objectParams, schema(`{"type":"string"}`)),
		402: testOperator(402, "agenui.list.sum", `function run(values, p) { return values.reduce(function(sum,v){ var n=Number(p.field ? v[p.field] : v); if(!Number.isFinite(n)) throw new Error("not number"); return sum+n; },0); }`,
			schema(`{"type":"array"}`), objectParams, schema(`{"type":"number"}`)),
		403: testOperator(403, "agenui.list.max", `function run(values, p) { if(!values.length) throw new Error("empty"); return Math.max.apply(null, values.map(function(v){ return Number(p.field ? v[p.field] : v); })); }`,
			schema(`{"type":"array"}`), objectParams, schema(`{"type":"number"}`)),
		404: testOperator(404, "agenui.list.pick", `function run(values, p) { var matches=p.matchField ? values.filter(function(v){ return v[p.matchField]===p.matchValue; }) : [values[p.index || 0]]; if(matches.length!==1 || matches[0]===undefined) throw new Error("pick failed"); return p.extractField ? matches[0][p.extractField] : matches[0]; }`,
			schema(`{"type":"array"}`), objectParams, schema(`true`)),
		405: testOperator(405, "agenui.list.filter", `function run(values, p) { return values.filter(function(v){ var x=v[p.field]; switch(p.operator){case "eq":return x===p.value;case "neq":return x!==p.value;case "gt":return x>p.value;case "gte":return x>=p.value;case "lt":return x<p.value;case "lte":return x<=p.value;case "in":return p.value.indexOf(x)>=0;case "contains":return x.indexOf(p.value)>=0;default:throw new Error("bad operator");} }); }`,
			schema(`{"type":"array"}`), objectParams, schema(`{"type":"array"}`)),
		406: testOperator(406, "agenui.list.map", `function run(values, p) { return values.map(function(v){ return v[p.field]; }); }`,
			schema(`{"type":"array","items":{"type":"object"}}`), objectParams, schema(`{"type":"array"}`)),
	}
}

func executeOperatorChain(t *testing.T, value any, operators []Operator, invocations []Invocation) any {
	t.Helper()
	pkg := validPackage(t)
	pkg.DataSources = []DataSource{{ID: "ds-products", Endpoint: "mock://value", Role: RolePrimary}}
	pkg.Bindings = []Binding{{
		SlotID: "result.field", DataSourceID: "ds-products", FieldPath: "$.value", RefKey: "/result",
		MissingPolicy: MissingBlock, Transforms: invocations,
	}}
	pkg.Operators = operators
	pkg.Actions = nil
	result, err := NewEngine(&fakeFetcher{responses: map[string]any{"ds-products": map[string]any{"value": value}}}, nil).
		Execute(context.Background(), pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.DataModel["result"]
}

func TestStandardListOperatorShapes(t *testing.T) {
	ops := listOperatorFixtures()
	tests := []struct {
		name   string
		value  any
		op     uint64
		params map[string]any
		want   any
	}{
		{"join scalar", []any{"a", "b"}, 401, map[string]any{"separator": " · "}, "a · b"},
		{"join field", []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}}, 401, map[string]any{"field": "name", "separator": ","}, "a,b"},
		{"sum scalar", []any{float64(1), float64(2)}, 402, nil, int64(3)},
		{"sum field", []any{map[string]any{"price": float64(2)}, map[string]any{"price": float64(3)}}, 402, map[string]any{"field": "price"}, int64(5)},
		{"max scalar", []any{float64(1), float64(3)}, 403, nil, int64(3)},
		{"max field", []any{map[string]any{"price": float64(2)}, map[string]any{"price": float64(4)}}, 403, map[string]any{"field": "price"}, int64(4)},
		{"pick index", []any{"a", "b"}, 404, map[string]any{"index": float64(1)}, "b"},
		{"pick match", []any{map[string]any{"id": "a", "price": float64(2)}, map[string]any{"id": "b", "price": float64(3)}}, 404, map[string]any{"matchField": "id", "matchValue": "b"}, map[string]any{"id": "b", "price": int64(3)}},
		{"pick extract", []any{map[string]any{"id": "a", "price": float64(2)}}, 404, map[string]any{"matchField": "id", "matchValue": "a", "extractField": "price"}, int64(2)},
		{"filter", []any{map[string]any{"status": "open"}, map[string]any{"status": "closed"}}, 405, map[string]any{"field": "status", "operator": "eq", "value": "open"}, []any{map[string]any{"status": "open"}}},
		{"map", []any{map[string]any{"price": float64(2)}, map[string]any{"price": float64(3)}}, 406, map[string]any{"field": "price"}, []any{int64(2), int64(3)}},
		{"empty join", []any{}, 401, nil, ""},
		{"empty sum", []any{}, 402, nil, int64(0)},
		{"empty filter", []any{}, 405, map[string]any{"field": "x", "operator": "eq", "value": 1}, []any{}},
		{"empty map", []any{}, 406, map[string]any{"field": "x"}, []any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := executeOperatorChain(t, test.value, []Operator{ops[test.op]}, []Invocation{{OperatorVersionID: test.op, Params: test.params}})
			if !jsonEqual(got, test.want) {
				t.Fatalf("got=%#v want=%#v", got, test.want)
			}
		})
	}
}

func TestStandardOperatorChains(t *testing.T) {
	ops := listOperatorFixtures()
	money := moneyOperator()
	tests := []struct {
		name        string
		value       any
		operators   []Operator
		invocations []Invocation
		want        any
	}{
		{
			name: "pick then format money", value: []any{float64(6800), float64(12800)}, operators: []Operator{ops[404], money},
			invocations: []Invocation{
				{OperatorVersionID: 404, Params: map[string]any{"index": 0}},
				{OperatorVersionID: 101, Params: map[string]any{"currency": "¥"}},
			}, want: "¥68.00",
		},
		{
			name: "filter map join", value: []any{map[string]any{"status": "open", "name": "a"}, map[string]any{"status": "closed", "name": "b"}, map[string]any{"status": "open", "name": "c"}},
			operators: []Operator{ops[405], ops[406], ops[401]}, invocations: []Invocation{
				{OperatorVersionID: 405, Params: map[string]any{"field": "status", "operator": "eq", "value": "open"}},
				{OperatorVersionID: 406, Params: map[string]any{"field": "name"}},
				{OperatorVersionID: 401, Params: map[string]any{"separator": ","}},
			}, want: "a,c",
		},
		{
			name: "filter sum", value: []any{map[string]any{"status": "open", "price": float64(2)}, map[string]any{"status": "closed", "price": float64(5)}},
			operators: []Operator{ops[405], ops[402]}, invocations: []Invocation{
				{OperatorVersionID: 405, Params: map[string]any{"field": "status", "operator": "eq", "value": "open"}},
				{OperatorVersionID: 402, Params: map[string]any{"field": "price"}},
			}, want: int64(2),
		},
		{
			name: "map max", value: []any{map[string]any{"price": float64(2)}, map[string]any{"price": float64(5)}},
			operators: []Operator{ops[406], ops[403]}, invocations: []Invocation{
				{OperatorVersionID: 406, Params: map[string]any{"field": "price"}},
				{OperatorVersionID: 403},
			}, want: int64(5),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := executeOperatorChain(t, test.value, test.operators, test.invocations)
			if !jsonEqual(got, test.want) {
				t.Fatalf("got=%#v want=%#v", got, test.want)
			}
		})
	}
}

func TestFilterComparisonOperators(t *testing.T) {
	op := listOperatorFixtures()[405]
	tests := []struct {
		name     string
		operator string
		value    any
		input    []any
		want     int
	}{
		{"eq", "eq", 2, []any{map[string]any{"x": 1}, map[string]any{"x": 2}}, 1},
		{"neq", "neq", 2, []any{map[string]any{"x": 1}, map[string]any{"x": 2}}, 1},
		{"gt", "gt", 1, []any{map[string]any{"x": 1}, map[string]any{"x": 2}}, 1},
		{"gte", "gte", 2, []any{map[string]any{"x": 1}, map[string]any{"x": 2}}, 1},
		{"lt", "lt", 2, []any{map[string]any{"x": 1}, map[string]any{"x": 2}}, 1},
		{"lte", "lte", 1, []any{map[string]any{"x": 1}, map[string]any{"x": 2}}, 1},
		{"in", "in", []any{1, 3}, []any{map[string]any{"x": 1}, map[string]any{"x": 2}}, 1},
		{"contains", "contains", "map", []any{map[string]any{"x": "amap"}, map[string]any{"x": "other"}}, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := executeOperatorChain(t, test.input, []Operator{op}, []Invocation{{
				OperatorVersionID: 405, Params: map[string]any{"field": "x", "operator": test.operator, "value": test.value},
			}})
			if values, ok := got.([]any); !ok || len(values) != test.want {
				t.Fatalf("got=%#v", got)
			}
		})
	}
}

func TestStandardOperatorFailures(t *testing.T) {
	ops := listOperatorFixtures()
	for _, test := range []struct {
		name   string
		value  any
		op     uint64
		params map[string]any
	}{
		{"sum invalid element", []any{"x"}, 402, nil},
		{"max empty", []any{}, 403, nil},
		{"pick missing", []any{"a"}, 404, map[string]any{"index": 9}},
		{"pick duplicate", []any{map[string]any{"id": "a"}, map[string]any{"id": "a"}}, 404, map[string]any{"matchField": "id", "matchValue": "a"}},
		{"filter operator", []any{map[string]any{"x": 1}}, 405, map[string]any{"field": "x", "operator": "bad", "value": 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			pkg.DataSources = []DataSource{{ID: "ds-products", Endpoint: "mock://value", Role: RolePrimary}}
			pkg.Bindings = []Binding{{SlotID: "result.field", DataSourceID: "ds-products", FieldPath: "$.value", RefKey: "/result", MissingPolicy: MissingBlock, Transforms: []Invocation{{OperatorVersionID: test.op, Params: test.params}}}}
			pkg.Operators, pkg.Actions = []Operator{ops[test.op]}, nil
			_, err := NewEngine(&fakeFetcher{responses: map[string]any{"ds-products": map[string]any{"value": test.value}}}, nil).Execute(context.Background(), pkg, nil)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
