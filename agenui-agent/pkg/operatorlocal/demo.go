// Package operatorlocal serves the existing published Operator Detail HTTP
// contract from the local source data plane. It retains one public fixture for
// a blank database and otherwise reads only published admin versions.
package operatorlocal

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

const DetailPath = "/api/v1/agenui/platform/operator/published/detail"

// RegisterDemo mounts the source-compatible detail endpoint for the public
// demo operator advertised by knowragmcp.SeedDemo.
func RegisterDemo(mux *http.ServeMux) {
	Register(mux, nil)
}

// Register serves published versions from the local administration data plane.
// A nil DB preserves the tiny fixture for the standalone contract test.
func Register(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("POST "+DetailPath, func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			OperatorVersionID uint64 `json:"operatorVersionId"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input); err != nil || input.OperatorVersionID == 0 {
			write(w, map[string]any{"code": 40001, "message": "operatorVersionId is required", "result": false})
			return
		}
		if db != nil {
			var version int
			var key, hash, language, code, entry, inputSchema, paramsSchema, outputSchema string
			err := db.QueryRowContext(r.Context(), `SELECT operator_key,version_no,source_hash,language,source_code,entry_name,input_schema_json,params_schema_json,output_schema_json FROM local_operator_version WHERE runtime_id=? AND status=1`, input.OperatorVersionID).
				Scan(&key, &version, &hash, &language, &code, &entry, &inputSchema, &paramsSchema, &outputSchema)
			if err == nil {
				write(w, map[string]any{"code": 1, "message": "success", "result": true, "data": map[string]any{
					"operatorVersionId": input.OperatorVersionID, "operatorKey": key, "version": version,
					"inputSchema": json.RawMessage(inputSchema), "paramsSchema": json.RawMessage(paramsSchema), "outputSchema": json.RawMessage(outputSchema),
					"sourceHash": hash, "language": language, "languageVersion": "ES2022", "sourceCode": code, "entry": entry,
				}})
				return
			}
			if err != sql.ErrNoRows {
				write(w, map[string]any{"code": 50001, "message": "lookup failed", "result": false})
				return
			}
		}
		if input.OperatorVersionID != 205 {
			write(w, map[string]any{"code": 40415, "message": "not found", "result": false})
			return
		}
		write(w, map[string]any{"code": 1, "message": "success", "result": true, "data": map[string]any{
			"operatorVersionId": 205,
			"operatorKey":       "demo.identity",
			"version":           1,
			"inputSchema":       json.RawMessage(`true`),
			"paramsSchema":      json.RawMessage(`{"type":"object"}`),
			"outputSchema":      json.RawMessage(`true`),
			"sourceHash":        "sha256:demo.identity_transform.v1",
			"language":          "javascript",
			"languageVersion":   "ES2022",
			"sourceCode":        "function transform(value) { return value; }",
		}})
	})
}

func write(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
