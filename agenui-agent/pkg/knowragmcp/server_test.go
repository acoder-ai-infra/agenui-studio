package knowragmcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

func TestSQLiteMCPServesSourceToolContracts(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO knowrag_api (id,search_text,path,description,data_source_id,api_version,knowledge_entity_id,knowledge_revision,response_model,response_example) VALUES ('product','product price','/product','product prices','public','v1','product-knowledge','r1','{"type":"object"}','{"data":{}}')`)
	if err != nil {
		t.Fatal(err)
	}
	result := rpcCall(t, server.Handler(), "tools/call", map[string]any{"name": "search_developer_apis", "arguments": map[string]any{"query": "product price", "top_k": 1}})
	content := result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var search struct {
		Query   string           `json:"query"`
		Total   int              `json:"total"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(content), &search); err != nil {
		t.Fatal(err)
	}
	if search.Query != "product price" || search.Total != 1 || search.Results[0]["knowledge_revision"] != "r1" {
		t.Fatalf("source contract result = %#v", search)
	}

	toolsResult := rpcCall(t, server.Handler(), "tools/list", nil).(map[string]any)
	tools := toolsResult["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tool count = %d", len(tools))
	}
}

func TestEnsureSchemaUpgradesExistingKnowledgeTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE knowrag_api (id TEXT PRIMARY KEY, search_text TEXT NOT NULL, path TEXT NOT NULL, method TEXT NOT NULL DEFAULT 'GET', description TEXT NOT NULL, project_name TEXT NOT NULL DEFAULT '', score REAL NOT NULL DEFAULT 0, data_source_id TEXT NOT NULL, api_version TEXT NOT NULL, knowledge_entity_id TEXT NOT NULL, knowledge_revision TEXT NOT NULL, response_model TEXT NOT NULL, response_example TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	server, _ := New(db)
	if err := server.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO knowrag_api (id,search_text,path,description,data_source_id,api_version,knowledge_entity_id,knowledge_revision,response_model,response_example,binding_contract) VALUES ('existing','existing','/existing','existing','existing','v1','existing','r1','{}','{}','{}')`); err != nil {
		t.Fatalf("upgraded column is unavailable: %v", err)
	}
}

func TestSeedDemoUsesExistingMCPFactTablesWithoutOverwrite(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SeedDemo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.SeedDemo(context.Background()); err != nil {
		t.Fatal(err)
	}
	var apiCount, operatorCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM knowrag_api`).Scan(&apiCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM knowrag_operator`).Scan(&operatorCount); err != nil {
		t.Fatal(err)
	}
	if apiCount != 3 || operatorCount != 0 {
		t.Fatalf("demo facts api=%d operators=%d", apiCount, operatorCount)
	}
	result := rpcCall(t, server.Handler(), "tools/call", map[string]any{"name": "search_developer_apis", "arguments": map[string]any{"query": "product title price distance", "top_k": 1}})
	content := result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !bytes.Contains([]byte(content), []byte("demo.product.list")) {
		t.Fatalf("demo search result = %s", content)
	}
	if !bytes.Contains([]byte(content), []byte(`"price_cents":6800`)) || !bytes.Contains([]byte(content), []byte(`"valueType":"integer"`)) {
		t.Fatalf("demo must retain typed source values for operator transforms: %s", content)
	}
	if !bytes.Contains([]byte(content), []byte(`"path":"$.items[*].title"`)) ||
		!bytes.Contains([]byte(content), []byte(`"primary_row_list":"$.items"`)) {
		t.Fatalf("product demo must expose a typed list binding contract: %s", content)
	}
	if !bytes.Contains([]byte(content), []byte(`"path":"$.items[*].detail_url"`)) ||
		!bytes.Contains([]byte(content), []byte(`"path":"$.catalog_url"`)) {
		t.Fatalf("product demo must include item and card action paths: %s", content)
	}
}

func TestSearchUsesCJKBM25RatherThanSubstringLike(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
INSERT INTO knowrag_api (id,search_text,path,description,data_source_id,api_version,knowledge_entity_id,knowledge_revision,response_model,response_example,response_fields) VALUES
('product-detail', '商品详情 商品名称 规格 价格 评分', '/products/{id}', '查询单个商品详情', 'public', 'v1', 'product', 'r1', '{"type":"object"}', '{"data":{}}', '[{"name":"price","semantic":"商品价格"}]'),
('route-price', '路线价格 出行费用', '/routes', '查询路线规划价格', 'public', 'v1', 'route', 'r1', '{"type":"object"}', '{"data":{}}', '[{"name":"price","semantic":"出行费用"}]')`)
	if err != nil {
		t.Fatal(err)
	}
	value, err := server.Search(context.Background(), "帮我看商品名称、规格、价格和评分", 1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0]["result_id"] != "product-detail" {
		t.Fatalf("BM25 ranking = %s", encoded)
	}
}

func TestListDeveloperOperatorsMatchesPublishedPagedContract(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO knowrag_operator(operator_id,operator_key,summary,published) VALUES
(1,'first','First operator',1),(2,'second','Second operator',1),(3,'third','Third operator',1),(4,'draft','Hidden operator',0),(5,'legacy-blank','',1)`); err != nil {
		t.Fatal(err)
	}
	result := rpcCall(t, server.Handler(), "tools/call", map[string]any{
		"name": "list_developer_operators", "arguments": map[string]any{"page_no": 2, "page_size": 2},
	})
	content := result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var page struct {
		Items    []map[string]any `json:"items"`
		Total    int              `json:"total"`
		PageNo   int              `json:"page_no"`
		PageSize int              `json:"page_size"`
	}
	if err := json.Unmarshal([]byte(content), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || page.PageNo != 2 || page.PageSize != 2 || len(page.Items) != 1 || page.Items[0]["operator_key"] != "third" {
		t.Fatalf("operator page = %s", content)
	}
	for _, key := range []string{"input_schema", "params_schema", "output_schema"} {
		if _, ok := page.Items[0][key]; !ok {
			t.Fatalf("operator page omits %s: %s", key, content)
		}
	}
	assertOperatorCatalogConformance(t, json.RawMessage(content))
}

func assertOperatorCatalogConformance(t *testing.T, output json.RawMessage) {
	t.Helper()
	var toolsCatalog struct {
		Definitions []struct {
			Name         string `yaml:"name"`
			InputSchema  any    `yaml:"input_schema"`
			OutputSchema any    `yaml:"output_schema"`
		} `yaml:"definitions"`
	}
	readYAML(t, filepath.Join("..", "..", "configs", "harness", "catalogs", "tools.yaml"), &toolsCatalog)
	var publishedInput, publishedOutput any
	for _, definition := range toolsCatalog.Definitions {
		if definition.Name == "list_developer_operators" {
			publishedInput, publishedOutput = definition.InputSchema, definition.OutputSchema
			break
		}
	}
	if publishedInput == nil || publishedOutput == nil {
		t.Fatal("published operator tool contract is missing")
	}
	var mcpCatalog struct {
		Definitions []struct {
			Tools []struct {
				Name        string `yaml:"name"`
				InputSchema any    `yaml:"input_schema"`
			} `yaml:"tools"`
		} `yaml:"definitions"`
	}
	readYAML(t, filepath.Join("..", "..", "configs", "harness", "catalogs", "mcp.local.yaml"), &mcpCatalog)
	var transportInput any
	for _, definition := range mcpCatalog.Definitions {
		for _, tool := range definition.Tools {
			if tool.Name == "list_developer_operators" {
				transportInput = tool.InputSchema
			}
		}
	}
	serverInput := tools()[1]["inputSchema"]
	if canonicalJSON(t, serverInput) != canonicalJSON(t, publishedInput) ||
		canonicalJSON(t, serverInput) != canonicalJSON(t, transportInput) {
		t.Fatalf("operator input schemas drifted: server=%s published=%s transport=%s",
			canonicalJSON(t, serverInput), canonicalJSON(t, publishedInput), canonicalJSON(t, transportInput))
	}
	schemaDocument := jsonValue(canonicalJSON(t, publishedOutput))
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("operator-output.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("operator-output.json")
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(output, &instance); err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("operator output violates published schema: %v; output=%s", err, output)
	}
}

func readYAML(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func canonicalJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func rpcCall(t *testing.T, handler http.Handler, method string, params any) any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var value struct {
		Result any `json:"result"`
		Error  any `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Error != nil {
		t.Fatalf("rpc error=%#v", value.Error)
	}
	return value.Result
}
