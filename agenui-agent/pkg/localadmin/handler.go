// Package localadmin provides local-first Studio asset management. It owns
// only local administration metadata;
// published API/operator records remain in the source MCP fact tables and rule
// edits remain source agenui_rule_doc inputs for the source rule worker.
package localadmin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/exportpackage"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/ruleworker"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/knowragmcp"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/localmodel"
)

const Prefix = "/api/v1/agenui/admin/local"

const maxRuleDocumentBytes = 256 << 10

const demoRuleFileName = "demo.design-rules.md"

// demoRuleMarkdown is the public, human-editable source for the bundled
// revision. Uploaded Markdown remains an independent source document handled
// by ruleworker.
//
//go:embed demo_rule.md
var demoRuleMarkdown string

type Handler struct {
	db                      *sql.DB
	designKnowledgeRoot     string
	designKnowledgeRevision string
	generatedRevisionRoot   string
	revisionPointerPath     string
	serviceInstanceID       string
	restartRequester        func() error
}

func New(db *sql.DB) (*Handler, error) {
	if db == nil {
		return nil, fmt.Errorf("local admin: database is required")
	}
	h := &Handler{db: db}
	if err := h.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	if err := h.SeedSystemOperators(context.Background()); err != nil {
		return nil, fmt.Errorf("local admin: seed system operators: %w", err)
	}
	if err := h.SeedBundledDesignRevision(context.Background()); err != nil {
		return nil, err
	}
	if _, err := localmodel.New(db); err != nil {
		return nil, err
	}
	return h, nil
}

// SetRestartRequester enables the local console to request a graceful Agent
// rebuild. The callback must be configured before the HTTP server starts.
func (h *Handler) SetRestartRequester(serviceInstanceID string, requester func() error) {
	h.serviceInstanceID = strings.TrimSpace(serviceInstanceID)
	h.restartRequester = requester
}

func (h *Handler) EnsureSchema(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS local_operator_version (
 id TEXT PRIMARY KEY, runtime_id INTEGER UNIQUE, operator_key TEXT NOT NULL,
 version_no INTEGER NOT NULL, status INTEGER NOT NULL, name TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '', usage_scenario TEXT NOT NULL DEFAULT '',
 language TEXT NOT NULL, entry_name TEXT NOT NULL, source_code TEXT NOT NULL,
 source_hash TEXT NOT NULL,
 input_schema_json TEXT NOT NULL DEFAULT 'true',
 params_schema_json TEXT NOT NULL DEFAULT '{"type":"object","additionalProperties":false}',
 output_schema_json TEXT NOT NULL DEFAULT 'true',
 gmt_create TEXT NOT NULL, gmt_published TEXT,
 UNIQUE(operator_key, version_no));
CREATE INDEX IF NOT EXISTS idx_local_operator_key ON local_operator_version(operator_key, version_no DESC);
CREATE TABLE IF NOT EXISTS agenui_rule_doc (
 id INTEGER PRIMARY KEY AUTOINCREMENT, file_name TEXT NOT NULL DEFAULT '', content TEXT NOT NULL,
 content_md5 TEXT NOT NULL, status INTEGER NOT NULL DEFAULT 2, is_current INTEGER NOT NULL DEFAULT 1,
 is_effective INTEGER NOT NULL DEFAULT 0, parse_status INTEGER NOT NULL DEFAULT 0,
 parser_version TEXT NOT NULL DEFAULT '', parse_error TEXT NOT NULL DEFAULT '', parse_report TEXT NOT NULL DEFAULT '',
 parse_worker TEXT NOT NULL DEFAULT '', retry_count INTEGER NOT NULL DEFAULT 0,
 parse_started_at TEXT, parse_finished_at TEXT, lease_expire_at TEXT,
 change_description TEXT NOT NULL DEFAULT '', create_user TEXT NOT NULL DEFAULT '',
 gmt_create TEXT NOT NULL DEFAULT (datetime('now')), gmt_modified TEXT NOT NULL DEFAULT (datetime('now')));
CREATE INDEX IF NOT EXISTS idx_rule_doc_parse_task ON agenui_rule_doc(parse_status,lease_expire_at,id);`); err != nil {
		return fmt.Errorf("local admin schema: %w", err)
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "input_schema_json", definition: "TEXT NOT NULL DEFAULT 'true'"},
		{name: "params_schema_json", definition: `TEXT NOT NULL DEFAULT '{"type":"object","additionalProperties":false}'`},
		{name: "output_schema_json", definition: "TEXT NOT NULL DEFAULT 'true'"},
	} {
		if err := ensureOperatorColumn(ctx, h.db, column.name, column.definition); err != nil {
			return fmt.Errorf("local admin operator schema: %w", err)
		}
	}
	if _, err := h.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS local_package_publication_target (
 id INTEGER PRIMARY KEY CHECK (id = 1), kind TEXT NOT NULL,
 endpoint TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
 gmt_modified TEXT NOT NULL DEFAULT (datetime('now')));`); err != nil {
		return fmt.Errorf("local admin publication schema: %w", err)
	}
	return nil
}

// Register mounts the local source-data management API.
func Register(mux *http.ServeMux, db *sql.DB) (*Handler, error) {
	return RegisterWithDesignKnowledge(mux, db, "", "")
}

// RegisterWithDesignKnowledge mounts the local console with a published
// design-knowledge revision as the source of truth for rules.
func RegisterWithDesignKnowledge(mux *http.ServeMux, db *sql.DB, root, revision string) (*Handler, error) {
	return RegisterWithDesignKnowledgeSource(mux, db, root, revision, "", "")
}

// RegisterWithDesignKnowledgeSource also exposes revisions published by the
// local Worker. The pointer is resolved per request so the UI hot-reloads with
// the same active revision used by generation.
func RegisterWithDesignKnowledgeSource(mux *http.ServeMux, db *sql.DB, root, revision, generatedRoot, pointerPath string) (*Handler, error) {
	h, err := New(db)
	if err != nil {
		return nil, err
	}
	h.designKnowledgeRoot = strings.TrimSpace(root)
	h.designKnowledgeRevision = strings.TrimSpace(revision)
	h.generatedRevisionRoot = strings.TrimSpace(generatedRoot)
	h.revisionPointerPath = strings.TrimSpace(pointerPath)
	// knowrag's schema is shared by the local setup; execute it on this DB.
	if err := ensureFacts(context.Background(), db); err != nil {
		return nil, err
	}
	mux.HandleFunc("GET "+Prefix+"/apis", h.listAPIs)
	mux.HandleFunc("POST "+Prefix+"/apis", h.putAPI)
	mux.HandleFunc("GET "+Prefix+"/knowledge/search", h.searchKnowledge)
	mux.HandleFunc("GET "+Prefix+"/operators", h.listOperators)
	mux.HandleFunc("POST "+Prefix+"/operators/save", h.saveOperator)
	mux.HandleFunc("POST "+Prefix+"/operators/publish", h.publishOperator)
	mux.HandleFunc("POST "+Prefix+"/operators/delete-draft", h.deleteDraft)
	mux.HandleFunc("GET "+Prefix+"/operators/{key}/versions", h.versions)
	mux.HandleFunc("GET "+Prefix+"/rules", h.listRules)
	mux.HandleFunc("POST "+Prefix+"/rules", h.putRule)
	mux.HandleFunc("POST "+Prefix+"/rules/upload", h.uploadRules)
	mux.HandleFunc("GET "+Prefix+"/rules/jobs", h.ruleJobs)
	mux.HandleFunc("GET "+Prefix+"/rules/library", h.ruleLibrary)
	mux.HandleFunc("POST "+Prefix+"/init-demo", h.seed)
	mux.HandleFunc("GET "+Prefix+"/model", h.getModel)
	mux.HandleFunc("POST "+Prefix+"/model", h.putModel)
	mux.HandleFunc("POST "+Prefix+"/restart", h.restartAgent)
	mux.HandleFunc("GET "+Prefix+"/publication-target", h.getPublicationTarget)
	mux.HandleFunc("POST "+Prefix+"/publication-target", h.putPublicationTarget)
	return h, nil
}

func (h *Handler) LoadPublicationTarget(ctx context.Context) (exportpackage.PublicationTarget, bool, error) {
	var target exportpackage.PublicationTarget
	var enabled int
	err := h.db.QueryRowContext(ctx, `SELECT kind,endpoint,enabled FROM local_package_publication_target WHERE id=1`).Scan(
		&target.Kind, &target.Endpoint, &enabled,
	)
	if err == sql.ErrNoRows {
		return exportpackage.PublicationTarget{}, false, nil
	}
	if err != nil {
		return exportpackage.PublicationTarget{}, false, err
	}
	target.Enabled = enabled != 0
	return target, true, nil
}

func (h *Handler) getPublicationTarget(w http.ResponseWriter, r *http.Request) {
	target, configured, err := h.LoadPublicationTarget(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{
		"configured": configured, "kind": target.Kind,
		"endpoint": target.Endpoint, "enabled": target.Enabled,
	})
}

func (h *Handler) putPublicationTarget(w http.ResponseWriter, r *http.Request) {
	var target exportpackage.PublicationTarget
	if err := decode(w, r, &target); err != nil {
		return
	}
	target.Kind = strings.TrimSpace(target.Kind)
	target.Endpoint = strings.TrimSpace(target.Endpoint)
	if target.Kind != "callback" && target.Kind != "mq" {
		fail(w, http.StatusBadRequest, errors.New("publication target kind must be callback or mq"))
		return
	}
	endpoint, err := url.Parse(target.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		fail(w, http.StatusBadRequest, errors.New("publication target endpoint must be an absolute URL"))
		return
	}
	if target.Kind == "callback" && endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		fail(w, http.StatusBadRequest, errors.New("callback endpoint must use http or https"))
		return
	}
	enabled := 0
	if target.Enabled {
		enabled = 1
	}
	_, err = h.db.ExecContext(r.Context(), `
INSERT INTO local_package_publication_target(id,kind,endpoint,enabled,gmt_modified)
VALUES(1,?,?,?,datetime('now'))
ON CONFLICT(id) DO UPDATE SET kind=excluded.kind,endpoint=excluded.endpoint,
 enabled=excluded.enabled,gmt_modified=datetime('now')`, target.Kind, target.Endpoint, enabled)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"status": "saved"})
}

func (h *Handler) getModel(w http.ResponseWriter, r *http.Request) {
	store, err := localmodel.New(h.db)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	cfg, configured, err := store.Load(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	masked := ""
	if configured && len(cfg.APIKey) > 4 {
		masked = "****" + cfg.APIKey[len(cfg.APIKey)-4:]
	}
	protocol := cfg.Protocol
	if protocol == "" {
		protocol = localmodel.ProtocolOpenAICompatible
	}
	respond(w, 200, map[string]any{
		"baseUrl": cfg.BaseURL, "model": cfg.Model, "protocol": protocol,
		"apiKeyMasked": masked, "configured": configured,
		"restartSupported":  h.restartRequester != nil,
		"serviceInstanceId": h.serviceInstanceID,
	})
}
func (h *Handler) putModel(w http.ResponseWriter, r *http.Request) {
	var x struct {
		BaseURL  string `json:"baseUrl"`
		Model    string `json:"model"`
		APIKey   string `json:"apiKey"`
		Protocol string `json:"protocol"`
	}
	if err := decode(w, r, &x); err != nil {
		return
	}
	store, err := localmodel.New(h.db)
	if err == nil {
		err = store.Save(r.Context(), localmodel.Config{BaseURL: x.BaseURL, Model: x.Model, APIKey: x.APIKey, Protocol: x.Protocol})
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	respond(w, 200, map[string]any{"status": "saved", "applied": true})
}

func (h *Handler) restartAgent(w http.ResponseWriter, _ *http.Request) {
	if h.restartRequester == nil {
		fail(w, http.StatusNotImplemented, errors.New("Agent restart is unavailable for this process"))
		return
	}
	if err := h.restartRequester(); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{
		"status": "restarting", "service": "agenui-agent",
		"serviceInstanceId": h.serviceInstanceID,
	})
}

func ensureFacts(ctx context.Context, db *sql.DB) error {
	server, err := knowragmcp.New(db)
	if err != nil {
		return err
	}
	return server.EnsureSchema(ctx)
}

type apiFact struct {
	ID                string          `json:"id"`
	SearchText        string          `json:"searchText"`
	Path              string          `json:"path"`
	Method            string          `json:"method"`
	Description       string          `json:"description"`
	ProjectName       string          `json:"projectName"`
	Score             float64         `json:"score"`
	DataSourceID      string          `json:"dataSourceId"`
	APIVersion        string          `json:"apiVersion"`
	KnowledgeEntityID string          `json:"knowledgeEntityId"`
	KnowledgeRevision string          `json:"knowledgeRevision"`
	ResponseModel     json.RawMessage `json:"responseModel"`
	ResponseExample   json.RawMessage `json:"responseExample"`
	ResponseFields    json.RawMessage `json:"responseFields,omitempty"`
	Entity            json.RawMessage `json:"entity,omitempty"`
	BindingContract   json.RawMessage `json:"bindingContract,omitempty"`
}

func (h *Handler) listAPIs(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id,search_text,path,method,description,project_name,score,data_source_id,api_version,knowledge_entity_id,knowledge_revision,response_model,response_example,COALESCE(response_fields,''),COALESCE(entity,''),COALESCE(binding_contract,'') FROM knowrag_api ORDER BY id`)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	out := []apiFact{}
	for rows.Next() {
		var x apiFact
		// SQLite returns TEXT for the frozen JSON columns. Scan into strings first:
		// database/sql deliberately does not scan a string straight into RawMessage.
		var responseModel, responseExample, rf, e, p string
		if err = rows.Scan(&x.ID, &x.SearchText, &x.Path, &x.Method, &x.Description, &x.ProjectName, &x.Score, &x.DataSourceID, &x.APIVersion, &x.KnowledgeEntityID, &x.KnowledgeRevision, &responseModel, &responseExample, &rf, &e, &p); err != nil {
			fail(w, 500, err)
			return
		}
		x.ResponseModel = json.RawMessage(responseModel)
		x.ResponseExample = json.RawMessage(responseExample)
		x.ResponseFields = json.RawMessage(rf)
		x.Entity = json.RawMessage(e)
		x.BindingContract = json.RawMessage(p)
		out = append(out, x)
	}
	respond(w, 200, map[string]any{"items": out})
}
func (h *Handler) putAPI(w http.ResponseWriter, r *http.Request) {
	var x apiFact
	if err := decode(w, r, &x); err != nil {
		return
	}
	x.ID = strings.TrimSpace(x.ID)
	x.Path = strings.TrimSpace(x.Path)
	if x.ID == "" || x.Path == "" || strings.TrimSpace(x.Description) == "" {
		fail(w, 400, fmt.Errorf("id, path and description are required"))
		return
	}
	if x.SearchText == "" {
		x.SearchText = x.ID + " " + x.Description
	}
	if x.Method == "" {
		x.Method = "GET"
	}
	if x.DataSourceID == "" {
		x.DataSourceID = "local"
	}
	if x.APIVersion == "" {
		x.APIVersion = "v1"
	}
	if x.KnowledgeEntityID == "" {
		x.KnowledgeEntityID = x.ID
	}
	if x.KnowledgeRevision == "" {
		x.KnowledgeRevision = "local"
	}
	if len(x.ResponseModel) == 0 {
		x.ResponseModel = []byte(`{}`)
	}
	if len(x.ResponseExample) == 0 {
		x.ResponseExample = []byte(`{}`)
	}
	_, err := h.db.ExecContext(r.Context(), `INSERT INTO knowrag_api (id,search_text,path,method,description,project_name,score,data_source_id,api_version,knowledge_entity_id,knowledge_revision,response_model,response_example,response_fields,entity,binding_contract) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET search_text=excluded.search_text,path=excluded.path,method=excluded.method,description=excluded.description,project_name=excluded.project_name,score=excluded.score,data_source_id=excluded.data_source_id,api_version=excluded.api_version,knowledge_entity_id=excluded.knowledge_entity_id,knowledge_revision=excluded.knowledge_revision,response_model=excluded.response_model,response_example=excluded.response_example,response_fields=excluded.response_fields,entity=excluded.entity,binding_contract=excluded.binding_contract`, x.ID, x.SearchText, x.Path, x.Method, x.Description, x.ProjectName, x.Score, x.DataSourceID, x.APIVersion, x.KnowledgeEntityID, x.KnowledgeRevision, string(x.ResponseModel), string(x.ResponseExample), nullableJSON(x.ResponseFields), nullableJSON(x.Entity), nullableJSON(x.BindingContract))
	if err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]any{"status": "saved", "api": x})
}

// searchKnowledge is a UI-only projection of the existing KnowRAG source
// result. The source MCP owns filtering/ranking; this handler only expands a
// selected API fact into one old-console row per declared field.
func (h *Handler) searchKnowledge(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		fail(w, http.StatusBadRequest, fmt.Errorf("q is required"))
		return
	}
	server, err := knowragmcp.New(h.db)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	result, err := server.Search(r.Context(), query, 8)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	var canonical struct {
		Results []struct {
			ResultID       string            `json:"result_id"`
			ProjectName    string            `json:"project_name"`
			Score          float64           `json:"score"`
			ResponseFields []json.RawMessage `json:"response_fields"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &canonical); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	items := []map[string]any{}
	for _, entry := range canonical.Results {
		for _, field := range entry.ResponseFields {
			items = append(items, map[string]any{"apiId": entry.ResultID, "apiName": entry.ProjectName, "score": entry.Score, "provider": "agenui-knowrag-sqlite", "field": field})
		}
	}
	respond(w, http.StatusOK, map[string]any{"items": items})
}

type operator struct {
	OperatorVersionID string          `json:"operatorVersionId"`
	RuntimeID         int64           `json:"runtimeId"`
	OperatorKey       string          `json:"operatorKey"`
	VersionNo         int             `json:"versionNo"`
	Status            int             `json:"status"`
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	UsageScenario     string          `json:"usageScenario"`
	Language          string          `json:"language"`
	Entry             string          `json:"entry"`
	SourceCode        string          `json:"sourceCode"`
	SourceHash        string          `json:"sourceHash"`
	InputSchema       json.RawMessage `json:"inputSchema"`
	ParamsSchema      json.RawMessage `json:"paramsSchema"`
	OutputSchema      json.RawMessage `json:"outputSchema"`
	CreatedAt         string          `json:"gmtCreate"`
	PublishedAt       string          `json:"gmtPublished,omitempty"`
}
type operatorInput struct {
	OperatorVersionID string          `json:"operatorVersionId,omitempty"`
	OperatorKey       string          `json:"operatorKey,omitempty"`
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	UsageScenario     string          `json:"usageScenario"`
	Language          string          `json:"language,omitempty"`
	Entry             string          `json:"entry,omitempty"`
	SourceCode        string          `json:"sourceCode"`
	InputSchema       json.RawMessage `json:"inputSchema,omitempty"`
	ParamsSchema      json.RawMessage `json:"paramsSchema,omitempty"`
	OutputSchema      json.RawMessage `json:"outputSchema,omitempty"`
}

func (h *Handler) listOperators(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT `+operatorSelectColumns+` FROM local_operator_version ORDER BY operator_key,version_no DESC`)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	out := []operator{}
	for rows.Next() {
		x, scanErr := scanOperator(rows)
		if scanErr != nil {
			fail(w, 500, scanErr)
			return
		}
		out = append(out, x)
	}
	respond(w, 200, map[string]any{"items": out})
}
func (h *Handler) saveOperator(w http.ResponseWriter, r *http.Request) {
	var p operatorInput
	if err := decode(w, r, &p); err != nil {
		return
	}
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.SourceCode) == "" {
		fail(w, 400, fmt.Errorf("name and sourceCode are required"))
		return
	}
	if p.Language == "" {
		p.Language = "typescript"
	}
	if p.Entry == "" {
		p.Entry = "run"
	}
	inputSchema, err := normalizeOperatorSchema(p.InputSchema, anySchema)
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("inputSchema: %w", err))
		return
	}
	paramsSchema, err := normalizeOperatorSchema(p.ParamsSchema, objectParamsSchema)
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("paramsSchema: %w", err))
		return
	}
	outputSchema, err := normalizeOperatorSchema(p.OutputSchema, anySchema)
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("outputSchema: %w", err))
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	key := strings.TrimSpace(p.OperatorKey)
	if key == "" {
		key = "op_" + shortHash(p.Name+now)
	}
	var existing operator
	existing, err = scanOperator(h.db.QueryRowContext(r.Context(), `SELECT `+operatorSelectColumns+` FROM local_operator_version WHERE operator_key=? AND status=0`, key))
	hash := contentHash(p.SourceCode)
	if err == nil {
		_, err = h.db.ExecContext(r.Context(), `UPDATE local_operator_version SET name=?,description=?,usage_scenario=?,language=?,entry_name=?,source_code=?,source_hash=?,input_schema_json=?,params_schema_json=?,output_schema_json=? WHERE id=?`, p.Name, p.Description, p.UsageScenario, p.Language, p.Entry, p.SourceCode, hash, inputSchema, paramsSchema, outputSchema, existing.OperatorVersionID)
		existing.Name = p.Name
		existing.Description = p.Description
		existing.UsageScenario = p.UsageScenario
		existing.Language = p.Language
		existing.Entry = p.Entry
		existing.SourceCode = p.SourceCode
		existing.SourceHash = hash
		existing.InputSchema = json.RawMessage(inputSchema)
		existing.ParamsSchema = json.RawMessage(paramsSchema)
		existing.OutputSchema = json.RawMessage(outputSchema)
	} else if err == sql.ErrNoRows {
		var n int
		_ = h.db.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(version_no),0)+1 FROM local_operator_version WHERE operator_key=?`, key).Scan(&n)
		id := "opv_" + shortHash(key+fmt.Sprint(n)+now)
		res, insertErr := h.db.ExecContext(r.Context(), `INSERT INTO local_operator_version(id,operator_key,version_no,status,name,description,usage_scenario,language,entry_name,source_code,source_hash,input_schema_json,params_schema_json,output_schema_json,gmt_create) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, key, n, 0, p.Name, p.Description, p.UsageScenario, p.Language, p.Entry, p.SourceCode, hash, inputSchema, paramsSchema, outputSchema, now)
		err = insertErr
		if err != nil {
			fail(w, 500, err)
			return
		}
		rid, _ := res.LastInsertId()
		if _, err = h.db.ExecContext(r.Context(), `UPDATE local_operator_version SET runtime_id=? WHERE id=?`, rid, id); err != nil {
			fail(w, 500, err)
			return
		}
		existing = operator{OperatorVersionID: id, RuntimeID: rid, OperatorKey: key, VersionNo: n, Status: 0, Name: p.Name, Description: p.Description, UsageScenario: p.UsageScenario, Language: p.Language, Entry: p.Entry, SourceCode: p.SourceCode, SourceHash: hash, InputSchema: json.RawMessage(inputSchema), ParamsSchema: json.RawMessage(paramsSchema), OutputSchema: json.RawMessage(outputSchema), CreatedAt: now}
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]any{"status": "saved", "operator": existing})
}
func (h *Handler) publishOperator(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OperatorVersionID string `json:"operatorVersionId"`
	}
	if err := decode(w, r, &in); err != nil {
		return
	}
	x, err := scanOperator(h.db.QueryRowContext(r.Context(), `SELECT `+operatorSelectColumns+` FROM local_operator_version WHERE id=?`, in.OperatorVersionID))
	if err == sql.ErrNoRows {
		fail(w, 404, fmt.Errorf("operator version not found"))
		return
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	if strings.TrimSpace(x.Description) == "" {
		fail(w, 400, fmt.Errorf("description is required before publishing an operator"))
		return
	}
	if x.Status == 0 {
		x.Status = 1
		x.PublishedAt = time.Now().UTC().Format(time.RFC3339)
		if _, err = h.db.ExecContext(r.Context(), `UPDATE local_operator_version SET status=1,gmt_published=? WHERE id=?`, x.PublishedAt, x.OperatorVersionID); err != nil {
			fail(w, 500, err)
			return
		}
	}
	if _, err = h.db.ExecContext(r.Context(), `INSERT INTO knowrag_operator(operator_id,operator_key,summary,published,input_schema_json,params_schema_json,output_schema_json) VALUES(?,?,?,1,?,?,?) ON CONFLICT(operator_id) DO UPDATE SET operator_key=excluded.operator_key,summary=excluded.summary,published=1,input_schema_json=excluded.input_schema_json,params_schema_json=excluded.params_schema_json,output_schema_json=excluded.output_schema_json`, x.RuntimeID, x.OperatorKey, x.Description, string(x.InputSchema), string(x.ParamsSchema), string(x.OutputSchema)); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]any{"status": "published", "operator": x})
}
func (h *Handler) deleteDraft(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OperatorVersionID string `json:"operatorVersionId"`
	}
	if err := decode(w, r, &in); err != nil {
		return
	}
	res, err := h.db.ExecContext(r.Context(), `DELETE FROM local_operator_version WHERE id=? AND status=0`, in.OperatorVersionID)
	if err != nil {
		fail(w, 500, err)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, 409, fmt.Errorf("only an existing draft can be deleted"))
		return
	}
	respond(w, 200, map[string]string{"status": "deleted"})
}
func (h *Handler) versions(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	rows, err := h.db.QueryContext(r.Context(), `SELECT `+operatorSelectColumns+` FROM local_operator_version WHERE operator_key=? ORDER BY version_no DESC`, key)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	out := []operator{}
	for rows.Next() {
		x, scanErr := scanOperator(rows)
		if scanErr != nil {
			fail(w, 500, scanErr)
			return
		}
		out = append(out, x)
	}
	respond(w, 200, map[string]any{"items": out})
}

type ruleDoc struct {
	ID                int64  `json:"id,omitempty"`
	FileName          string `json:"fileName"`
	Content           string `json:"content"`
	ChangeDescription string `json:"changeDescription,omitempty"`
	CreateUser        string `json:"createUser,omitempty"`
	ParseStatus       int    `json:"parseStatus"`
	ParseError        string `json:"parseError,omitempty"`
	CreatedAt         string `json:"gmtCreate"`
}

func (h *Handler) listRules(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id,file_name,content,change_description,create_user,parse_status,parse_error,gmt_create FROM agenui_rule_doc ORDER BY id DESC`)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	out := []ruleDoc{}
	for rows.Next() {
		var x ruleDoc
		if err = rows.Scan(&x.ID, &x.FileName, &x.Content, &x.ChangeDescription, &x.CreateUser, &x.ParseStatus, &x.ParseError, &x.CreatedAt); err != nil {
			fail(w, 500, err)
			return
		}
		out = append(out, x)
	}
	respond(w, 200, map[string]any{"items": out})
}
func (h *Handler) putRule(w http.ResponseWriter, r *http.Request) {
	var x ruleDoc
	if err := decode(w, r, &x); err != nil {
		return
	}
	if strings.TrimSpace(x.FileName) == "" || strings.TrimSpace(x.Content) == "" {
		fail(w, 400, fmt.Errorf("fileName and content are required"))
		return
	}
	if err := validateRuleMarkdown(x.FileName, x.Content); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := h.queueRuleDoc(r.Context(), &x); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]any{"status": "queued", "ruleDocument": x})
}

func (h *Handler) queueRuleDoc(ctx context.Context, x *ruleDoc) error {
	if err := validateRuleMarkdown(x.FileName, x.Content); err != nil {
		return err
	}
	if x.CreateUser == "" {
		x.CreateUser = "local-admin"
	}
	sum := sha256.Sum256([]byte(x.Content))
	res, err := h.db.ExecContext(ctx, `INSERT INTO agenui_rule_doc(file_name,content,content_md5,status,is_current,is_effective,parse_status,change_description,create_user) VALUES(?,?,?,2,1,0,0,?,?)`, x.FileName, x.Content, hex.EncodeToString(sum[:]), x.ChangeDescription, x.CreateUser)
	if err != nil {
		return err
	}
	x.ID, _ = res.LastInsertId()
	x.ParseStatus = 0
	return nil
}

// SeedBundledDesignRevision records the checked-in public Markdown as the
// source of the already-active bundled revision. No model call or SQL rule
// projection is needed during first-run initialization.
func (h *Handler) SeedBundledDesignRevision(ctx context.Context) error {
	content := strings.TrimSpace(demoRuleMarkdown)
	sum := sha256.Sum256([]byte(content))
	contentHash := hex.EncodeToString(sum[:])
	var effectiveFile, effectiveHash string
	err := h.db.QueryRowContext(ctx, `SELECT file_name,content_md5 FROM agenui_rule_doc WHERE is_effective=1 LIMIT 1`).Scan(&effectiveFile, &effectiveHash)
	if err == nil {
		// A user- or integration-published revision owns the workspace. Only a
		// newer version of our own bundled revision may replace itself.
		if effectiveFile != demoRuleFileName || effectiveHash == contentHash {
			return nil
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	var docID int64
	err = h.db.QueryRowContext(ctx, `SELECT id FROM agenui_rule_doc WHERE file_name=? AND content_md5=? ORDER BY id DESC LIMIT 1`, demoRuleFileName, contentHash).Scan(&docID)
	if err == sql.ErrNoRows {
		doc := &ruleDoc{FileName: demoRuleFileName, Content: content, ChangeDescription: "public bundled design revision", CreateUser: "agenui-studio"}
		if err := h.queueRuleDoc(ctx, doc); err != nil {
			return err
		}
		docID = doc.ID
	} else if err != nil {
		return err
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE agenui_rule_doc SET is_effective=0 WHERE is_effective=1`); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE agenui_rule_doc SET is_effective=1,parse_status=2,parse_error='',parse_finished_at=?,gmt_modified=? WHERE id=?`, now, now, docID); err != nil {
		return err
	}
	return tx.Commit()
}

// validateRuleMarkdown keeps every management entry point aligned with the
// authoring contract: people submit Markdown; JSON is generated internally by
// the parser and governance pipeline, never uploaded as a rule document.
func validateRuleMarkdown(name, content string) error {
	name = filepath.Base(strings.TrimSpace(name))
	lower := strings.ToLower(name)
	if name == "" || (!strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".markdown")) {
		return fmt.Errorf("仅支持 Markdown 文档（.md/.markdown）")
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("Markdown 文档不能为空")
	}
	if len(content) > maxRuleDocumentBytes {
		return fmt.Errorf("规则 Markdown 不能超过 256 KiB；请拆分为布局、元素和原子规则文档")
	}
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "# ") {
		return fmt.Errorf("规则 Markdown 必须以一级标题开始；请下载页面提供的模板")
	}
	return nil
}

// uploadRules preserves the existing console's multi-file/folder Markdown
// upload while queuing the exact source agenui_rule_doc inputs consumed by the
// in-service source rule worker. It does not introduce a second parser.
func (h *Handler) uploadRules(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("multipart form required: %w", err))
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	queued, skipped := 0, 0
	type rejectedFile struct {
		FileName string `json:"fileName"`
		Reason   string `json:"reason"`
	}
	rejected := make([]rejectedFile, 0)
	for _, headers := range r.MultipartForm.File {
		for _, header := range headers {
			name := filepath.Base(header.Filename)
			lower := strings.ToLower(name)
			if !strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".markdown") {
				skipped++
				continue
			}
			file, err := header.Open()
			if err != nil {
				rejected = append(rejected, rejectedFile{FileName: name, Reason: err.Error()})
				continue
			}
			content, readErr := io.ReadAll(io.LimitReader(file, maxRuleDocumentBytes+1))
			_ = file.Close()
			if readErr != nil {
				rejected = append(rejected, rejectedFile{FileName: name, Reason: readErr.Error()})
				continue
			}
			if err := h.queueRuleDoc(r.Context(), &ruleDoc{FileName: name, Content: string(content), ChangeDescription: "console upload"}); err != nil {
				rejected = append(rejected, rejectedFile{FileName: name, Reason: err.Error()})
				continue
			}
			queued++
		}
	}
	if queued == 0 {
		fail(w, http.StatusBadRequest, fmt.Errorf("文件夹中没有可解析的规则 Markdown：%d 个非 Markdown 已跳过，%d 个 Markdown 未通过校验", skipped, len(rejected)))
		return
	}
	respond(w, http.StatusOK, map[string]any{"status": "queued", "queued": queued, "skipped": skipped, "rejected": rejected})
}

func (h *Handler) ruleJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `SELECT id,file_name,parse_status,parse_error,gmt_create FROM agenui_rule_doc ORDER BY id DESC LIMIT 100`)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var file, parseError, created string
		var state int
		if err := rows.Scan(&id, &file, &state, &parseError, &created); err != nil {
			fail(w, 500, err)
			return
		}
		status := map[int]string{0: "pending", 1: "processing", 2: "done", 3: "failed"}[state]
		if status == "" {
			status = "pending"
		}
		parsed := 0
		if state == 2 {
			parsed = 1
		}
		items = append(items, map[string]any{"id": fmt.Sprintf("rule-doc-%d", id), "status": status, "files": []string{file}, "parsed": parsed, "error": parseError, "createdAt": created})
	}
	respond(w, 200, map[string]any{"items": items})
}

// ruleLibrary exposes the active published typed revision. It returns document
// metadata and raw Markdown so the browser can show Layout -> Element / Rule
// relationships without a second management projection.
func (h *Handler) ruleLibrary(w http.ResponseWriter, r *http.Request) {
	root, revision, err := h.activeRuleRevision()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if root == "" || revision == "" {
		fail(w, http.StatusServiceUnavailable, fmt.Errorf("active design knowledge revision is not configured"))
		return
	}
	repository, err := designknowledge.Load(r.Context(), os.DirFS(root), revision)
	if err != nil {
		fail(w, http.StatusInternalServerError, fmt.Errorf("load active design knowledge: %w", err))
		return
	}
	items := make([]map[string]any, 0, len(repository.Index().Documents))
	for _, metadata := range repository.Index().Documents {
		document, ok := repository.Document(metadata.ID)
		if !ok {
			continue
		}
		items = append(items, map[string]any{
			"id":            metadata.ID,
			"title":         ruleDocumentTitle(document.Content, metadata.ID),
			"kind":          metadata.Kind,
			"version":       metadata.Version,
			"summary":       metadata.Summary,
			"appliesTo":     metadata.AppliesTo,
			"notFor":        metadata.NotFor,
			"requires":      metadata.Requires,
			"conflictsWith": metadata.ConflictsWith,
			"references":    metadata.References,
			"content":       document.Content,
		})
	}
	respond(w, http.StatusOK, map[string]any{
		"revisionId":   repository.RevisionID(),
		"revisionHash": repository.RevisionHash(),
		"items":        items,
	})
}

func ruleDocumentTitle(content, fallback string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "# ") {
			continue
		}
		if title := strings.TrimSpace(strings.TrimPrefix(line, "# ")); title != "" {
			return title
		}
	}
	return strings.TrimSpace(fallback)
}

func (h *Handler) activeRuleRevision() (string, string, error) {
	if h.revisionPointerPath != "" && h.generatedRevisionRoot != "" {
		pointer, found, err := ruleworker.ReadLocalRevisionPointer(h.revisionPointerPath)
		if err != nil {
			return "", "", fmt.Errorf("read active rule revision: %w", err)
		}
		if found {
			return h.generatedRevisionRoot, pointer.RevisionID, nil
		}
	}
	return h.designKnowledgeRoot, h.designKnowledgeRevision, nil
}

func (h *Handler) seed(w http.ResponseWriter, r *http.Request) {
	err := h.SeedDemo(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	// Return counts so the console can refresh the initialized asset tables.
	var apis, operators, rules int
	if err := h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM knowrag_api WHERE id LIKE 'demo.%'`).Scan(&apis); err != nil {
		fail(w, 500, err)
		return
	}
	if err := h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM local_operator_version WHERE operator_key LIKE 'agenui.%' AND status=1`).Scan(&operators); err != nil {
		fail(w, 500, err)
		return
	}
	if err := h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agenui_rule_doc WHERE file_name LIKE 'demo.%'`).Scan(&rules); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]any{"status": "seeded", "report": map[string]int{"apis": apis, "operators": operators, "rules": rules}})
}

// SeedDemo publishes the same generic local APIs, operators and bundled rules
// used by the one-click console action. It is safe to call repeatedly.
func (h *Handler) SeedDemo(ctx context.Context) error {
	s, err := knowragmcp.New(h.db)
	if err == nil {
		err = s.SeedDemo(ctx)
	}
	if err == nil {
		err = h.SeedSystemOperators(ctx)
	}
	if err == nil {
		err = h.SeedBundledDesignRevision(ctx)
	}
	return err
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		fail(w, 400, err)
		return err
	}
	return nil
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	respond(w, status, map[string]string{"error": err.Error()})
}
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func contentHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
