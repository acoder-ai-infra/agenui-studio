package localadmin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	platformoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/knowragmcp"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/operatorlocal"
	_ "github.com/mattn/go-sqlite3"
)

func TestSystemOperatorsSeedIdempotentlyAndAreBinderDiscoverable(t *testing.T) {
	db := openSystemOperatorDB(t)
	handler, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	before := systemRuntimeIDs(t, db)
	if len(before) != len(systemOperatorSeeds) {
		t.Fatalf("system operators=%d want=%d", len(before), len(systemOperatorSeeds))
	}
	for _, seed := range systemOperatorSeeds {
		if before[seed.OperatorKey] != systemOperatorRuntimeID(seed.OperatorKey, 1) {
			t.Fatalf("system operator %q has unstable runtime ID %d", seed.OperatorKey, before[seed.OperatorKey])
		}
		if before[seed.OperatorKey] < 1 || before[seed.OperatorKey] > 1<<53-1 {
			t.Fatalf("system operator %q runtime ID is not JSON-safe: %d", seed.OperatorKey, before[seed.OperatorKey])
		}
	}
	if err := handler.SeedSystemOperators(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := systemRuntimeIDs(t, db); !reflect.DeepEqual(after, before) {
		t.Fatalf("idempotent seed changed IDs: before=%v after=%v", before, after)
	}

	server, err := knowragmcp.New(db)
	if err != nil {
		t.Fatal(err)
	}
	requestBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_developer_operators", "arguments": map[string]any{}},
	})
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(requestBody))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("operator discovery=%d %s", recorder.Code, recorder.Body.String())
	}
	var rpcResponse struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &rpcResponse); err != nil || len(rpcResponse.Result.Content) != 1 {
		t.Fatalf("decode operator discovery: %v body=%s", err, recorder.Body.String())
	}
	catalog := rpcResponse.Result.Content[0].Text
	for _, seed := range systemOperatorSeeds {
		if !strings.Contains(catalog, `"operator_key":"`+seed.OperatorKey+`"`) {
			t.Fatalf("Binder discovery is missing %q: %s", seed.OperatorKey, catalog)
		}
	}
}

func TestSystemOperatorDefinitionsExecuteThroughPublishedDetail(t *testing.T) {
	db := openSystemOperatorDB(t)
	if _, err := New(db); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	operatorlocal.Register(mux, db)
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := platformoperator.NewHTTPDetailClient(platformoperator.HTTPDetailClientConfig{
		Endpoint: server.URL + operatorlocal.DetailPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := platformoperator.NewService(client, platformoperator.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ids := systemRuntimeIDs(t, db)
	tests := []struct {
		name    string
		key     string
		value   any
		params  map[string]any
		want    any
		applied bool
	}{
		{"money", "agenui.scalar.format_money", float64(6800), map[string]any{"currency": "¥"}, "¥68.00", true},
		{"money rejects array", "agenui.scalar.format_money", []any{float64(6800)}, nil, nil, false},
		{"distance meters", "agenui.scalar.format_distance", float64(850), nil, "850m", true},
		{"distance kilometers", "agenui.scalar.format_distance", float64(12400), nil, "12.4km", true},
		{"distance rejects array", "agenui.scalar.format_distance", []any{float64(850)}, nil, nil, false},
		{"join scalars", "agenui.list.join", []any{"a", "b"}, map[string]any{"separator": ","}, "a,b", true},
		{"join field", "agenui.list.join", []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}}, map[string]any{"field": "name", "separator": " · "}, "a · b", true},
		{"join empty", "agenui.list.join", []any{}, nil, "", true},
		{"sum scalars", "agenui.list.sum", []any{float64(1), float64(2), float64(3)}, nil, int64(6), true},
		{"sum field", "agenui.list.sum", []any{map[string]any{"price": float64(2)}, map[string]any{"price": float64(3)}}, map[string]any{"field": "price"}, int64(5), true},
		{"sum empty", "agenui.list.sum", []any{}, nil, int64(0), true},
		{"sum rejects non-number", "agenui.list.sum", []any{"1"}, nil, nil, false},
		{"max scalars", "agenui.list.max", []any{float64(1), float64(9), float64(3)}, nil, int64(9), true},
		{"max field", "agenui.list.max", []any{map[string]any{"price": float64(2)}, map[string]any{"price": float64(4)}}, map[string]any{"field": "price"}, int64(4), true},
		{"max rejects empty", "agenui.list.max", []any{}, nil, nil, false},
		{"pick index", "agenui.list.pick", []any{"a", "b"}, map[string]any{"index": float64(1)}, "b", true},
		{"pick match and extract", "agenui.list.pick", []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}, map[string]any{"matchField": "id", "matchValue": "b", "extractField": "id"}, "b", true},
		{"pick rejects missing", "agenui.list.pick", []any{"a"}, map[string]any{"index": float64(9)}, nil, false},
		{"pick rejects duplicate", "agenui.list.pick", []any{map[string]any{"id": "a"}, map[string]any{"id": "a"}}, map[string]any{"matchField": "id", "matchValue": "a"}, nil, false},
		{"filter equals", "agenui.list.filter", []any{map[string]any{"status": "open"}, map[string]any{"status": "closed"}}, map[string]any{"field": "status", "operator": "eq", "value": "open"}, []any{map[string]any{"status": "open"}}, true},
		{"filter not equals", "agenui.list.filter", []any{map[string]any{"x": float64(1)}, map[string]any{"x": float64(2)}}, map[string]any{"field": "x", "operator": "neq", "value": float64(2)}, []any{map[string]any{"x": int64(1)}}, true},
		{"filter greater", "agenui.list.filter", []any{map[string]any{"x": float64(1)}, map[string]any{"x": float64(2)}}, map[string]any{"field": "x", "operator": "gt", "value": float64(1)}, []any{map[string]any{"x": int64(2)}}, true},
		{"filter greater or equal", "agenui.list.filter", []any{map[string]any{"x": float64(1)}, map[string]any{"x": float64(2)}}, map[string]any{"field": "x", "operator": "gte", "value": float64(2)}, []any{map[string]any{"x": int64(2)}}, true},
		{"filter less", "agenui.list.filter", []any{map[string]any{"x": float64(1)}, map[string]any{"x": float64(2)}}, map[string]any{"field": "x", "operator": "lt", "value": float64(2)}, []any{map[string]any{"x": int64(1)}}, true},
		{"filter less or equal", "agenui.list.filter", []any{map[string]any{"x": float64(1)}, map[string]any{"x": float64(2)}}, map[string]any{"field": "x", "operator": "lte", "value": float64(1)}, []any{map[string]any{"x": int64(1)}}, true},
		{"filter in", "agenui.list.filter", []any{map[string]any{"status": "open"}, map[string]any{"status": "closed"}}, map[string]any{"field": "status", "operator": "in", "value": []any{"open"}}, []any{map[string]any{"status": "open"}}, true},
		{"filter contains", "agenui.list.filter", []any{map[string]any{"name": "amap"}, map[string]any{"name": "other"}}, map[string]any{"field": "name", "operator": "contains", "value": "map"}, []any{map[string]any{"name": "amap"}}, true},
		{"filter empty", "agenui.list.filter", []any{}, map[string]any{"field": "x", "operator": "eq", "value": 1}, []any{}, true},
		{"map", "agenui.list.map", []any{map[string]any{"price": float64(2)}, map[string]any{"price": float64(3)}}, map[string]any{"field": "price"}, []any{int64(2), int64(3)}, true},
		{"map empty", "agenui.list.map", []any{}, map[string]any{"field": "price"}, []any{}, true},
		{"map rejects missing field", "agenui.list.map", []any{map[string]any{}}, map[string]any{"field": "price"}, nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := service.Execute(context.Background(), platformoperator.ExecuteRequest{
				OperatorID: uint64(ids[test.key]), Value: test.value, Context: test.params,
			})
			if result.Applied != test.applied {
				t.Fatalf("result=%+v applied=%v", result, test.applied)
			}
			if test.applied && !sameJSON(result.Value, test.want) {
				t.Fatalf("result=%+v want=%#v", result, test.want)
			}
			detail, err := client.GetOperator(context.Background(), uint64(ids[test.key]))
			if err != nil {
				t.Fatal(err)
			}
			if detail.OperatorKey != test.key || len(detail.InputSchema) == 0 || len(detail.ParamsSchema) == 0 || len(detail.OutputSchema) == 0 {
				t.Fatalf("published detail lacks contract: %+v", detail)
			}
		})
	}
}

func TestSystemOperatorSeedRejectsImmutableDefinitionDrift(t *testing.T) {
	db := openSystemOperatorDB(t)
	if _, err := New(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE local_operator_version SET source_hash=? WHERE operator_key='agenui.list.join' AND version_no=1`, "sha256:"+strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := New(db); err == nil || !strings.Contains(err.Error(), "conflicts with the bundled immutable definition") {
		t.Fatalf("expected immutable definition conflict, got %v", err)
	}
}

func TestSystemOperatorPublishedVersionRemainsImmutable(t *testing.T) {
	db := openSystemOperatorDB(t)
	mux := http.NewServeMux()
	if _, err := Register(mux, db); err != nil {
		t.Fatal(err)
	}
	seed := systemOperatorSeeds[0]
	originalHash := contentHash(seed.SourceCode)
	body, _ := json.Marshal(map[string]any{
		"operatorKey": seed.OperatorKey, "name": seed.Name, "description": "A user draft",
		"language": "typescript", "entry": "run", "sourceCode": "function run(value) { return value; }",
	})
	request := httptest.NewRequest(http.MethodPost, Prefix+"/operators/save", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save next version=%d %s", recorder.Code, recorder.Body.String())
	}
	var versionOneHash string
	if err := db.QueryRow(`SELECT source_hash FROM local_operator_version WHERE operator_key=? AND version_no=1 AND status=1`, seed.OperatorKey).Scan(&versionOneHash); err != nil {
		t.Fatal(err)
	}
	if versionOneHash != originalHash {
		t.Fatalf("published version changed: %s", versionOneHash)
	}
	var drafts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM local_operator_version WHERE operator_key=? AND version_no=2 AND status=0`, seed.OperatorKey).Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if drafts != 1 {
		t.Fatal("editing a system key must create a new draft version")
	}
	if _, err := db.Exec(`INSERT INTO local_operator_version(id,runtime_id,operator_key,version_no,status,name,language,entry_name,source_code,source_hash,gmt_create) VALUES('duplicate',999,?,1,1,'duplicate','typescript','run','x','x','now')`, seed.OperatorKey); err == nil {
		t.Fatal("database accepted a duplicate operator key and version")
	}
}

func TestOperatorSchemaMigrationAndValidation(t *testing.T) {
	db := openSystemOperatorDB(t)
	if _, err := db.Exec(`CREATE TABLE local_operator_version (
 id TEXT PRIMARY KEY, runtime_id INTEGER UNIQUE, operator_key TEXT NOT NULL,
 version_no INTEGER NOT NULL, status INTEGER NOT NULL, name TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '', usage_scenario TEXT NOT NULL DEFAULT '',
 language TEXT NOT NULL, entry_name TEXT NOT NULL, source_code TEXT NOT NULL,
 source_hash TEXT NOT NULL, gmt_create TEXT NOT NULL, gmt_published TEXT,
 UNIQUE(operator_key, version_no))`); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	if err := handler.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"input_schema_json", "params_schema_json", "output_schema_json"} {
		if varCount(t, db, `SELECT COUNT(*) FROM pragma_table_info('local_operator_version') WHERE name='`+column+`'`) != 1 {
			t.Fatalf("migration did not add %s", column)
		}
	}
	if _, err := normalizeOperatorSchema(json.RawMessage(`[]`), anySchema); err == nil {
		t.Fatal("array schema root must be rejected")
	}
}

func openSystemOperatorDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func systemRuntimeIDs(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT operator_key,runtime_id FROM local_operator_version WHERE operator_key LIKE 'agenui.%' AND status=1 ORDER BY operator_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]int64{}
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			t.Fatal(err)
		}
		result[key] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func sameJSON(left, right any) bool {
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}
