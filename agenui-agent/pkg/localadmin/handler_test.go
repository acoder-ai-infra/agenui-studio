package localadmin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/examples"
	bindingoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/operator"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/integration/operatorruntime"
	platformoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/ruleworker"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/operatorlocal"
	_ "github.com/mattn/go-sqlite3"
)

func TestLocalAdminUpgradesExistingKnowledgeTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE knowrag_api (id TEXT PRIMARY KEY, search_text TEXT NOT NULL, path TEXT NOT NULL, method TEXT NOT NULL DEFAULT 'GET', description TEXT NOT NULL, project_name TEXT NOT NULL DEFAULT '', score REAL NOT NULL DEFAULT 0, data_source_id TEXT NOT NULL, api_version TEXT NOT NULL, knowledge_entity_id TEXT NOT NULL, knowledge_revision TEXT NOT NULL, response_model TEXT NOT NULL, response_example TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(http.NewServeMux(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE knowrag_api SET binding_contract='{}'`); err != nil {
		t.Fatalf("upgraded column is unavailable: %v", err)
	}
}

func TestPublicationTargetRoundTrip(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	handler, err := Register(mux, db)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, Prefix+"/publication-target", bytes.NewBufferString(
		`{"kind":"callback","endpoint":"https://runtime.example.test/cards","enabled":true}`,
	))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save target=%d %s", recorder.Code, recorder.Body.String())
	}

	target, configured, err := handler.LoadPublicationTarget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || !target.Enabled || target.Kind != "callback" || target.Endpoint != "https://runtime.example.test/cards" {
		t.Fatalf("unexpected target: configured=%v target=%+v", configured, target)
	}

	request = httptest.NewRequest(http.MethodPost, Prefix+"/publication-target", bytes.NewBufferString(
		`{"kind":"callback","endpoint":"file:///tmp/cards","enabled":true}`,
	))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unsafe callback endpoint must be rejected, got=%d %s", recorder.Code, recorder.Body.String())
	}
}

func TestLocalAdminPublishesOnlyPublishedOperatorAndQueuesSourceRuleDoc(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	if _, err := Register(mux, db); err != nil {
		t.Fatal(err)
	}
	workerStore, err := ruleworker.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := workerStore.Check(context.Background()); err != nil {
		t.Fatalf("local schema must remain the source rule-worker schema: %v", err)
	}
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, Prefix+path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if got := post("/operators/save", `{"name":"trim","description":"Trim surrounding whitespace","sourceCode":"function run(v){return v.trim()}"}`); got.Code != http.StatusOK {
		t.Fatalf("save=%d %s", got.Code, got.Body.String())
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM local_operator_version WHERE name='trim'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if varCount(t, db, `SELECT COUNT(*) FROM knowrag_operator WHERE operator_id=(SELECT runtime_id FROM local_operator_version WHERE name='trim')`) != 0 {
		t.Fatal("draft must not be published")
	}
	if got := post("/operators/publish", `{"operatorVersionId":"`+id+`"}`); got.Code != http.StatusOK {
		t.Fatalf("publish=%d %s", got.Code, got.Body.String())
	}
	if varCount(t, db, `SELECT COUNT(*) FROM knowrag_operator WHERE published=1 AND operator_id=(SELECT runtime_id FROM local_operator_version WHERE name='trim')`) != 1 {
		t.Fatal("published operator absent from MCP facts")
	}
	if got := post("/rules", `{"fileName":"local.md","content":"# rule\n\n## Rules\n\n- Keep spacing stable."}`); got.Code != http.StatusOK {
		t.Fatalf("rule=%d %s", got.Code, got.Body.String())
	}
	if got := post("/rules", `{"fileName":"rule.json","content":"{}"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("rule JSON must be rejected, got=%d %s", got.Code, got.Body.String())
	}
	if got := post("/rules", `{"fileName":"book.md","content":"# A book\n\nLong-form prose without a rule section."}`); got.Code != http.StatusOK {
		t.Fatalf("Markdown semantics belong to the worker, got=%d %s", got.Code, got.Body.String())
	}
	if varCount(t, db, `SELECT COUNT(*) FROM agenui_rule_doc WHERE parse_status=0`) != 2 {
		t.Fatal("rule was not queued as source document")
	}
}

func TestPublishOperatorRejectsBlankDescription(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	if _, err := Register(mux, db); err != nil {
		t.Fatal(err)
	}
	post := func(path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, Prefix+path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}
	if got := post("/operators/save", `{"name":"legacy","sourceCode":"function run(v){return v}"}`); got.Code != http.StatusOK {
		t.Fatalf("save=%d %s", got.Code, got.Body.String())
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM local_operator_version WHERE name='legacy'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := post("/operators/publish", `{"operatorVersionId":"`+id+`"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("publish must reject a blank description, got=%d %s", got.Code, got.Body.String())
	}
	if varCount(t, db, `SELECT COUNT(*) FROM knowrag_operator WHERE operator_id=(SELECT runtime_id FROM local_operator_version WHERE name='legacy')`) != 0 {
		t.Fatal("invalid operator must not be exposed to binding")
	}
}

func TestTypeScriptOperatorSavePublishAdapterAndRuntimePackageExecution(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mux := http.NewServeMux()
	if _, err := Register(mux, db); err != nil {
		t.Fatal(err)
	}
	operatorlocal.Register(mux, db)
	server := httptest.NewServer(mux)
	defer server.Close()

	post := func(path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, Prefix+path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}
	source := `type Distance = { meters: number }; function run(value: Distance): string { return (value.meters / 1000).toFixed(1) + "km"; }`
	raw, err := json.Marshal(map[string]any{
		"name": "distance_formatter", "description": "Format a distance in kilometers", "language": "typescript", "entry": "run", "sourceCode": source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := post("/operators/save", string(raw)); got.Code != http.StatusOK {
		t.Fatalf("save=%d %s", got.Code, got.Body.String())
	}
	var versionID string
	var runtimeID int64
	if err := db.QueryRow(`SELECT id,runtime_id FROM local_operator_version WHERE name='distance_formatter'`).Scan(&versionID, &runtimeID); err != nil {
		t.Fatal(err)
	}
	if got := post("/operators/publish", `{"operatorVersionId":"`+versionID+`"}`); got.Code != http.StatusOK {
		t.Fatalf("publish=%d %s", got.Code, got.Body.String())
	}

	client, err := platformoperator.NewHTTPDetailClient(platformoperator.HTTPDetailClientConfig{
		Endpoint: server.URL + operatorlocal.DetailPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	detailResponse, err := server.Client().Post(
		server.URL+operatorlocal.DetailPath,
		"application/json",
		bytes.NewBufferString(fmt.Sprintf(`{"operatorVersionId":%d}`, runtimeID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer detailResponse.Body.Close()
	var published struct {
		Code   int            `json:"code"`
		Result bool           `json:"result"`
		Data   map[string]any `json:"data"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&published); err != nil {
		t.Fatal(err)
	}
	if detailResponse.StatusCode != http.StatusOK || !published.Result || published.Code != 1 {
		t.Fatalf("operator detail=%d %#v", detailResponse.StatusCode, published)
	}
	service, err := platformoperator.NewService(client, platformoperator.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := operatorruntime.New(service)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.RunOperator(context.Background(), bindingoperator.OperatorRuntimeRequest{
		Scope: bindingoperator.OperatorRuntimeScope{
			TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", AgentID: "binder", ToolCallID: "tool",
		},
		InvocationID:       "invocation",
		IdempotencyKey:     "idempotency",
		RequestFingerprint: "sha256:" + strings.Repeat("a", 64),
		OperatorID:         runtimeID,
		SampleValue:        json.RawMessage(`{"meters":1250}`),
		Params:             json.RawMessage(`{}`),
		MaxOutputBytes:     1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `"1.3km"` {
		t.Fatalf("adapter output = %s", result.Output)
	}

}

func TestLocalAdminModelEndpointEncryptsAndKeepsBlankKey(t *testing.T) {
	work := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	if _, err := Register(mux, db); err != nil {
		t.Fatal(err)
	}
	request := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, Prefix+"/model", bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if got := request(http.MethodPost, `{"baseUrl":"https://models.example.test/v1","model":"demo","apiKey":"sk-secret-value","protocol":"anthropic"}`); got.Code != http.StatusOK {
		t.Fatalf("save=%d %s", got.Code, got.Body.String())
	}
	var sealed string
	if err := db.QueryRow(`SELECT api_key_ciphertext FROM local_model_config WHERE id=1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if sealed == "" || strings.Contains(sealed, "secret-value") {
		t.Fatalf("unexpected stored credential %q", sealed)
	}
	if got := request(http.MethodPost, `{"baseUrl":"https://models.example.test/v1","model":"demo-next"}`); got.Code != http.StatusOK {
		t.Fatalf("blank key update=%d %s", got.Code, got.Body.String())
	}
	got := request(http.MethodGet, "")
	if got.Code != http.StatusOK || strings.Contains(got.Body.String(), "sk-secret-value") || !strings.Contains(got.Body.String(), "****alue") || !strings.Contains(got.Body.String(), `"protocol":"anthropic"`) {
		t.Fatalf("masked read=%d %s", got.Code, got.Body.String())
	}
	if _, err := os.Stat(filepath.Join(work, "var", "local-secrets", "model-config.key")); err != nil {
		t.Fatalf("local encryption key missing: %v", err)
	}
}

func TestLocalAdminRestartEndpointReportsCapabilityAndRequestsRestart(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	handler, err := Register(mux, db)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, Prefix+"/restart", nil))
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported restart=%d %s", recorder.Code, recorder.Body.String())
	}

	restartRequests := 0
	handler.SetRestartRequester("instance-before-restart", func() error {
		restartRequests++
		return nil
	})
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, Prefix+"/model", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"restartSupported":true`) || !strings.Contains(recorder.Body.String(), `"serviceInstanceId":"instance-before-restart"`) {
		t.Fatalf("model restart capability=%d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, Prefix+"/restart", nil))
	if recorder.Code != http.StatusAccepted || restartRequests != 1 || !strings.Contains(recorder.Body.String(), `"status":"restarting"`) {
		t.Fatalf("restart request=%d calls=%d body=%s", recorder.Code, restartRequests, recorder.Body.String())
	}
}

func TestLocalAdminConsoleAssetsUseSourceFactsAndMarkdownUploads(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	if _, err := Register(mux, db); err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, Prefix+path, bytes.NewReader(body))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	api := []byte(`{"id":"demo.weather","searchText":"weather temperature","path":"/demo/weather","method":"GET","description":"weather","responseModel":{},"responseExample":{},"responseFields":[{"path":"temperature","name":"Temperature","semantic":"weather temperature","valueType":"number","scope":"card"}]}`)
	if got := request(http.MethodPost, "/apis", api, "application/json"); got.Code != http.StatusOK {
		t.Fatalf("put api=%d %s", got.Code, got.Body.String())
	}
	got := request(http.MethodGet, "/apis", nil, "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"responseFields"`) || !strings.Contains(got.Body.String(), "Temperature") {
		t.Fatalf("list api=%d %s", got.Code, got.Body.String())
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("files", "design.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("# Console Rule\n\n## Rules\n\nKeep the summary clear."))
	book, err := writer.CreateFormFile("files", "book.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = book.Write([]byte("# A book\n\nLong-form prose without a rule section."))
	plain, err := writer.CreateFormFile("files", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = plain.Write([]byte("not a rule document"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := request(http.MethodPost, "/rules/upload", form.Bytes(), writer.FormDataContentType()); got.Code != http.StatusOK {
		t.Fatalf("upload=%d %s", got.Code, got.Body.String())
	} else if !strings.Contains(got.Body.String(), `"queued":2`) || !strings.Contains(got.Body.String(), `"skipped":1`) || !strings.Contains(got.Body.String(), `"rejected":[]`) {
		t.Fatalf("folder upload result must report queued, skipped, and rejected files: %s", got.Body.String())
	}
	var uploaded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agenui_rule_doc WHERE file_name IN ('design.md','book.md')`).Scan(&uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded != 2 {
		t.Fatalf("uploaded markdown documents = %d, want 2", uploaded)
	}
	if removed := request(http.MethodGet, "/rules/items", nil, ""); removed.Code != http.StatusNotFound {
		t.Fatalf("legacy rule projection route must remain removed: %d %s", removed.Code, removed.Body.String())
	}
}

func TestLocalAdminDemoSeedKeepsSystemOperatorsPublished(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	if _, err := Register(mux, db); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, Prefix+"/init-demo", nil)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("seed=%d %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
		Report struct {
			APIs      int `json:"apis"`
			Operators int `json:"operators"`
			Rules     int `json:"rules"`
		} `json:"report"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "seeded" || payload.Report.APIs != 3 || payload.Report.Operators != len(systemOperatorSeeds) || payload.Report.Rules != 1 {
		t.Fatalf("seed report = %#v", payload)
	}
	var demoContent string
	if err := db.QueryRow(`SELECT content FROM agenui_rule_doc WHERE file_name=?`, demoRuleFileName).Scan(&demoContent); err != nil {
		t.Fatalf("public demo rule was not queued: %v", err)
	}
	if !strings.Contains(demoContent, "公开设计规则") {
		t.Fatalf("unexpected public demo rule: %q", demoContent)
	}
	if varCount(t, db, `SELECT COUNT(*) FROM local_operator_version WHERE status=1 AND operator_key LIKE 'agenui.%'`) != len(systemOperatorSeeds) {
		t.Fatal("system operators must be published through the local admin lifecycle")
	}
	if varCount(t, db, `SELECT COUNT(*) FROM knowrag_operator WHERE operator_key LIKE 'agenui.%' AND published=1`) != len(systemOperatorSeeds) {
		t.Fatal("MCP must expose all system operators")
	}
	for operatorKey, wantSource := range map[string]string{
		"agenui.scalar.format_money":    examples.FormatMoneyOperator,
		"agenui.scalar.format_distance": examples.FormatDistanceOperator,
		"agenui.list.join":              examples.ListJoinOperator,
		"agenui.list.sum":               examples.ListSumOperator,
		"agenui.list.max":               examples.ListMaxOperator,
		"agenui.list.pick":              examples.ListPickOperator,
		"agenui.list.filter":            examples.ListFilterOperator,
		"agenui.list.map":               examples.ListMapOperator,
	} {
		var source, hash string
		if err := db.QueryRow(`SELECT source_code,source_hash FROM local_operator_version WHERE operator_key=? AND status=1`, operatorKey).Scan(&source, &hash); err != nil {
			t.Fatalf("published operator %s: %v", operatorKey, err)
		}
		if source != wantSource || hash != contentHash(wantSource) {
			t.Fatalf("published operator %s drifted from its bundled source", operatorKey)
		}
	}
	if varCount(t, db, `SELECT COUNT(*) FROM agenui_rule_doc WHERE file_name='demo.design-rules.md' AND parse_status=2 AND is_effective=1`) != 1 {
		t.Fatal("bundled demo must be published without a model-backed worker pass")
	}
	for _, table := range []string{"agenui_component", "agenui_layout", "agenui_rule_item"} {
		if varCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='`+table+`'`) != 0 {
			t.Fatalf("legacy management projection table %s must not be created", table)
		}
	}
}

func varCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRuleDocumentTitle(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		fallback string
		want     string
	}{
		{name: "first heading", content: "intro\n# 摘要信息卡\nbody", fallback: "layout.summary", want: "摘要信息卡"},
		{name: "ignores secondary heading", content: "## Details", fallback: "layout.summary", want: "layout.summary"},
		{name: "empty heading", content: "#   \ncontent", fallback: " rule.fallback ", want: "rule.fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ruleDocumentTitle(tt.content, tt.fallback); got != tt.want {
				t.Fatalf("ruleDocumentTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}
