// Package knowragmcp exposes the source AGenUI KnowRAG tool contract over
// standard MCP Streamable HTTP, backed by a user-owned SQLite database.
package knowragmcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"unicode"
)

const ProtocolVersion = "2025-03-26"

type Server struct{ db *sql.DB }

type retrievalDocument struct {
	apiIndex int
	tokens   []string
}

func New(db *sql.DB) (*Server, error) {
	if db == nil {
		return nil, fmt.Errorf("knowrag mcp: database is required")
	}
	return &Server{db: db}, nil
}

// EnsureSchema creates only the public knowledge tables. API documents are
// stored as frozen JSON facts and are returned unchanged to the AGenUI governor.
func (s *Server) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("knowrag mcp: database is required")
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS knowrag_api (
 id TEXT PRIMARY KEY, search_text TEXT NOT NULL, path TEXT NOT NULL,
 method TEXT NOT NULL DEFAULT 'GET', description TEXT NOT NULL,
 project_name TEXT NOT NULL DEFAULT '', score REAL NOT NULL DEFAULT 0,
 data_source_id TEXT NOT NULL, api_version TEXT NOT NULL,
 knowledge_entity_id TEXT NOT NULL, knowledge_revision TEXT NOT NULL,
 response_model TEXT NOT NULL, response_example TEXT NOT NULL,
 response_fields TEXT, entity TEXT, binding_contract TEXT
);
CREATE INDEX IF NOT EXISTS knowrag_api_search ON knowrag_api(search_text);
CREATE TABLE IF NOT EXISTS knowrag_operator (
 operator_id INTEGER PRIMARY KEY, operator_key TEXT NOT NULL,
 summary TEXT NOT NULL, published INTEGER NOT NULL DEFAULT 1
);`); err != nil {
		return err
	}
	for _, column := range []string{"response_fields", "entity", "binding_contract"} {
		if err := ensureKnowledgeColumn(ctx, s.db, column); err != nil {
			return err
		}
	}
	return nil
}

func ensureKnowledgeColumn(ctx context.Context, db *sql.DB, column string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(knowrag_api)`)
	if err != nil {
		return fmt.Errorf("inspect knowrag_api schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		found = found || name == column
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE knowrag_api ADD COLUMN `+column+` TEXT`); err != nil {
		return fmt.Errorf("add knowrag_api.%s: %w", column, err)
	}
	return nil
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// Search exposes the same canonical developer-API search used by the MCP
// tools/call implementation. It lets the local management projection render
// source search evidence without carrying a second retrieval implementation.
func (s *Server) Search(ctx context.Context, query string, topK int) (any, error) {
	arguments, err := json.Marshal(map[string]any{"query": query, "top_k": topK})
	if err != nil {
		return nil, err
	}
	return s.search(ctx, arguments)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.write(w, response{JSONRPC: "2.0", Error: &rpcError{-32700, "invalid json-rpc request"}})
		return
	}
	if req.JSONRPC != "2.0" {
		s.write(w, response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32600, "jsonrpc must be 2.0"}})
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	var err error
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "agenui-knowrag")
		result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "agenui-knowrag-sqlite", "version": "1.0"}}
	case "tools/list":
		result = map[string]any{"tools": tools()}
	case "tools/call":
		result, err = s.call(r.Context(), req.Params)
	default:
		s.write(w, response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32601, "method not found"}})
		return
	}
	if err != nil {
		s.write(w, response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32602, err.Error()}})
		return
	}
	s.write(w, response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) write(w http.ResponseWriter, value response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func tools() []map[string]any {
	searchSchema := map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{"query": map[string]any{"type": "string", "minLength": 1}, "top_k": map[string]any{"type": "integer", "minimum": 1, "maximum": 50}}, "additionalProperties": false}
	operatorSchema := map[string]any{"type": "object", "properties": map[string]any{
		"page_no":   map[string]any{"type": "integer", "minimum": 1, "maximum": 5, "default": 1},
		"page_size": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 100},
	}, "additionalProperties": false}
	return []map[string]any{
		{"name": "search_developer_apis", "description": "Search published developer API knowledge.", "inputSchema": searchSchema},
		{"name": "list_developer_operators", "description": "List a bounded page of published developer operators.", "inputSchema": operatorSchema},
	}
}

func (s *Server) call(ctx context.Context, raw json.RawMessage) (any, error) {
	var input struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("invalid tools/call params")
	}
	var data any
	var err error
	switch input.Name {
	case "search_developer_apis":
		data, err = s.search(ctx, input.Arguments)
	case "list_developer_operators":
		data, err = s.operators(ctx, input.Arguments)
	default:
		return nil, fmt.Errorf("unknown tool %q", input.Name)
	}
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(encoded)}}, "isError": false}, nil
}

func (s *Server) search(ctx context.Context, raw json.RawMessage) (any, error) {
	var input struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := json.Unmarshal(raw, &input); err != nil || strings.TrimSpace(input.Query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	if input.TopK <= 0 {
		input.TopK = 1
	}
	if input.TopK > 50 {
		return nil, fmt.Errorf("top_k must be at most 50")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,search_text,path,method,description,project_name,score,data_source_id,api_version,knowledge_entity_id,knowledge_revision,response_model,response_example,response_fields,entity,binding_contract FROM knowrag_api`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		entry map[string]any
		score float64
		id    string
	}
	all := make([]candidate, 0)
	documents := make([]retrievalDocument, 0)
	for rows.Next() {
		var id, searchText, path, method, description, project, source, apiVersion, knowledgeID, knowledgeRevision, responseModel, responseExample string
		var score float64
		var fields, entity, profile sql.NullString
		if err := rows.Scan(&id, &searchText, &path, &method, &description, &project, &score, &source, &apiVersion, &knowledgeID, &knowledgeRevision, &responseModel, &responseExample, &fields, &entity, &profile); err != nil {
			return nil, err
		}
		entry := map[string]any{"result_id": id, "path": path, "method": method, "description": description, "project_name": project, "score": score, "data_source_id": source, "api_version": apiVersion, "knowledge_entity_id": knowledgeID, "knowledge_revision": knowledgeRevision, "response_model": jsonValue(responseModel), "response_example": jsonValue(responseExample)}
		if fields.Valid {
			entry["response_fields"] = jsonValue(fields.String)
		}
		if entity.Valid {
			entry["entity"] = jsonValue(entity.String)
		}
		if profile.Valid {
			entry["binding_contract"] = jsonValue(profile.String)
		}
		apiIndex := len(all)
		all = append(all, candidate{entry: entry, score: score, id: id})
		fieldTexts := []string{strings.Join([]string{searchText, description, project}, " ")}
		if fields.Valid {
			var values []map[string]any
			if json.Unmarshal([]byte(fields.String), &values) == nil && len(values) > 0 {
				fieldTexts = fieldTexts[:0]
				for _, value := range values {
					raw, _ := json.Marshal(value)
					fieldTexts = append(fieldTexts, strings.Join([]string{searchText, description, string(raw)}, " "))
				}
			}
		}
		for _, text := range fieldTexts {
			documents = append(documents, retrievalDocument{apiIndex: apiIndex, tokens: tokenize(text)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	scores := bm25(documents, tokenize(input.Query))
	matched := make(map[int]float64)
	for index, score := range scores {
		if score > 0 {
			matched[documents[index].apiIndex] += score
		}
	}
	candidates := make([]candidate, 0, len(matched))
	for apiIndex, score := range matched {
		candidate := all[apiIndex]
		candidate.score = score
		candidate.entry["score"] = math.Round(score*1000) / 1000
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > input.TopK {
		candidates = candidates[:input.TopK]
	}
	results := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		results = append(results, candidate.entry)
	}
	return map[string]any{"query": input.Query, "total": len(results), "results": results}, nil
}

// tokenize and bm25 are the offline local retrieval algorithm previously used
// by the project: mixed CJK/Latin tokens (including CJK unigrams and bigrams)
// plus classic BM25. This is only a knowledge-service replacement; MCP facts,
// source authorization and downstream binding validation remain unchanged.
func tokenize(text string) []string {
	var tokens []string
	var latin strings.Builder
	var cjk []rune
	flushLatin := func() {
		if latin.Len() > 0 {
			tokens = append(tokens, strings.ToLower(latin.String()))
			latin.Reset()
		}
	}
	flushCJK := func() {
		for index, value := range cjk {
			tokens = append(tokens, string(value))
			if index+1 < len(cjk) {
				tokens = append(tokens, string(value)+string(cjk[index+1]))
			}
		}
		cjk = cjk[:0]
	}
	for _, value := range text {
		switch {
		case unicode.Is(unicode.Han, value):
			flushLatin()
			cjk = append(cjk, value)
		case unicode.IsLetter(value) || unicode.IsDigit(value):
			flushCJK()
			latin.WriteRune(unicode.ToLower(value))
		default:
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return tokens
}

func bm25(docs []retrievalDocument, queryTokens []string) []float64 {
	const k1, b = 1.5, 0.75
	if len(docs) == 0 {
		return nil
	}
	averageLength := 0.0
	documentFrequency := make(map[string]int)
	for _, doc := range docs {
		averageLength += float64(len(doc.tokens))
		seen := make(map[string]bool)
		for _, token := range doc.tokens {
			if !seen[token] {
				documentFrequency[token]++
				seen[token] = true
			}
		}
	}
	averageLength /= float64(len(docs))
	scores := make([]float64, len(docs))
	for index, doc := range docs {
		termFrequency := make(map[string]int)
		for _, token := range doc.tokens {
			termFrequency[token]++
		}
		for _, token := range queryTokens {
			frequency := float64(termFrequency[token])
			if frequency == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(docs))-float64(documentFrequency[token])+0.5)/(float64(documentFrequency[token])+0.5))
			scores[index] += idf * frequency * (k1 + 1) / (frequency + k1*(1-b+b*float64(len(doc.tokens))/averageLength))
		}
	}
	return scores
}

func (s *Server) operators(ctx context.Context, raw json.RawMessage) (any, error) {
	var input struct {
		PageNo   int `json:"page_no"`
		PageSize int `json:"page_size"`
	}
	if len(raw) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return nil, fmt.Errorf("invalid operator page arguments")
		}
	}
	if input.PageNo == 0 {
		input.PageNo = 1
	}
	if input.PageSize == 0 {
		input.PageSize = 100
	}
	if input.PageNo < 1 || input.PageNo > 5 || input.PageSize < 1 || input.PageSize > 100 {
		return nil, fmt.Errorf("operator page is outside the published bounds")
	}
	var total int
	// Published rows created before description validation was enforced may
	// contain a blank summary. Do not let one legacy row invalidate the entire
	// tool result at the Harness output-schema gate.
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowrag_operator WHERE published=1 AND TRIM(summary) <> ''`).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT operator_id,operator_key,summary FROM knowrag_operator WHERE published=1 AND TRIM(summary) <> '' ORDER BY operator_id LIMIT ? OFFSET ?`, input.PageSize, (input.PageNo-1)*input.PageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []map[string]any{}
	for rows.Next() {
		var id int64
		var key, summary string
		if err := rows.Scan(&id, &key, &summary); err != nil {
			return nil, err
		}
		values = append(values, map[string]any{"operator_id": id, "operator_key": key, "summary": summary})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(values, func(i, j int) bool { return values[i]["operator_id"].(int64) < values[j]["operator_id"].(int64) })
	return map[string]any{
		"items": values, "total": total,
		"page_no": input.PageNo, "page_size": input.PageSize,
	}, nil
}

func jsonValue(raw string) any {
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		return value
	}
	return raw
}
