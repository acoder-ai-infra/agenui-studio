package localadmin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/examples"
)

const (
	anySchema             = `true`
	objectParamsSchema    = `{"type":"object","additionalProperties":false}`
	optionalFieldParams   = `{"type":"object","properties":{"field":{"type":"string","minLength":1}},"additionalProperties":false}`
	arraySchema           = `{"type":"array"}`
	numberSchema          = `{"type":"number"}`
	stringSchema          = `{"type":"string"}`
	arrayOutputSchema     = `{"type":"array"}`
	mapInputSchema        = `{"type":"array","items":{"type":"object"}}`
	moneyParamsSchema     = `{"type":"object","properties":{"currency":{"type":"string","maxLength":8},"suffix":{"type":"string","maxLength":16}},"additionalProperties":false}`
	distanceParamsSchema  = `{"type":"object","properties":{"unit":{"enum":["metric"]}},"additionalProperties":false}`
	joinParamsSchema      = `{"type":"object","properties":{"separator":{"type":"string","maxLength":32},"field":{"type":"string","minLength":1}},"additionalProperties":false}`
	pickParamsSchema      = `{"type":"object","properties":{"index":{"type":"integer","minimum":0},"matchField":{"type":"string","minLength":1},"matchValue":{},"extractField":{"type":"string","minLength":1}},"additionalProperties":false}`
	filterParamsSchema    = `{"type":"object","required":["field","operator","value"],"properties":{"field":{"type":"string","minLength":1},"operator":{"enum":["eq","neq","gt","gte","lt","lte","in","contains"]},"value":{}},"additionalProperties":false}`
	mapParamsSchema       = `{"type":"object","required":["field"],"properties":{"field":{"type":"string","minLength":1}},"additionalProperties":false}`
	operatorSelectColumns = `id,runtime_id,operator_key,version_no,status,name,description,usage_scenario,language,entry_name,source_code,source_hash,input_schema_json,params_schema_json,output_schema_json,gmt_create,COALESCE(gmt_published,'')`
)

var systemOperatorSeeds = []operatorInput{
	{
		OperatorKey: "agenui.scalar.format_money", Name: "format_money",
		Description:   "number -> string. Params: {currency?: string, suffix?: string}. Formats a finite amount in minor currency units.",
		UsageScenario: "Currency display formatting for one numeric field.", Language: "typescript", Entry: "run",
		SourceCode: examples.FormatMoneyOperator, InputSchema: json.RawMessage(numberSchema),
		ParamsSchema: json.RawMessage(moneyParamsSchema), OutputSchema: json.RawMessage(stringSchema),
	},
	{
		OperatorKey: "agenui.scalar.format_distance", Name: "format_distance",
		Description:   "number -> string. Params: {unit?: 'metric'}. Formats a finite distance in meters.",
		UsageScenario: "Metric distance formatting for one numeric field.", Language: "typescript", Entry: "run",
		SourceCode: examples.FormatDistanceOperator, InputSchema: json.RawMessage(numberSchema),
		ParamsSchema: json.RawMessage(distanceParamsSchema), OutputSchema: json.RawMessage(stringSchema),
	},
	{
		OperatorKey: "agenui.list.join", Name: "list_join",
		Description:   "array -> string. Params: {separator?: string, field?: string}. Joins values or one field from each object.",
		UsageScenario: "Collapse a list into display text.", Language: "typescript", Entry: "run",
		SourceCode: examples.ListJoinOperator, InputSchema: json.RawMessage(arraySchema),
		ParamsSchema: json.RawMessage(joinParamsSchema), OutputSchema: json.RawMessage(stringSchema),
	},
	{
		OperatorKey: "agenui.list.sum", Name: "list_sum",
		Description:   "array -> number. Params: {field?: string}. Sums finite numbers or one numeric field from each object.",
		UsageScenario: "Aggregate a numeric list into one total.", Language: "typescript", Entry: "run",
		SourceCode: examples.ListSumOperator, InputSchema: json.RawMessage(arraySchema),
		ParamsSchema: json.RawMessage(optionalFieldParams), OutputSchema: json.RawMessage(numberSchema),
	},
	{
		OperatorKey: "agenui.list.max", Name: "list_max",
		Description:   "non-empty array -> number. Params: {field?: string}. Returns the maximum finite numeric value.",
		UsageScenario: "Aggregate a numeric list into one maximum value.", Language: "typescript", Entry: "run",
		SourceCode: examples.ListMaxOperator, InputSchema: json.RawMessage(arraySchema),
		ParamsSchema: json.RawMessage(optionalFieldParams), OutputSchema: json.RawMessage(numberSchema),
	},
	{
		OperatorKey: "agenui.list.pick", Name: "list_pick",
		Description:   "array -> item or scalar. Params: {index} or {matchField, matchValue}, plus optional extractField. Requires exactly one match.",
		UsageScenario: "Select one deterministic value from a list.", Language: "typescript", Entry: "run",
		SourceCode: examples.ListPickOperator, InputSchema: json.RawMessage(arraySchema),
		ParamsSchema: json.RawMessage(pickParamsSchema), OutputSchema: json.RawMessage(anySchema),
	},
	{
		OperatorKey: "agenui.list.filter", Name: "list_filter",
		Description:   "array<object> -> array<object>. Params: {field, operator, value}; operator is eq, neq, gt, gte, lt, lte, in, or contains.",
		UsageScenario: "Retain list items that satisfy one explicit field predicate.", Language: "typescript", Entry: "run",
		SourceCode: examples.ListFilterOperator, InputSchema: json.RawMessage(mapInputSchema),
		ParamsSchema: json.RawMessage(filterParamsSchema), OutputSchema: json.RawMessage(arrayOutputSchema),
	},
	{
		OperatorKey: "agenui.list.map", Name: "list_map",
		Description:   "array<object> -> array<any>. Params: {field: string}. Projects one named field from every object.",
		UsageScenario: "Convert an object list into a list of field values.", Language: "typescript", Entry: "run",
		SourceCode: examples.ListMapOperator, InputSchema: json.RawMessage(mapInputSchema),
		ParamsSchema: json.RawMessage(mapParamsSchema), OutputSchema: json.RawMessage(arrayOutputSchema),
	},
}

func ensureOperatorColumn(ctx context.Context, db *sql.DB, column, definition string) error {
	switch column {
	case "input_schema_json", "params_schema_json", "output_schema_json":
	default:
		return fmt.Errorf("unsupported operator column %q", column)
	}
	rows, err := db.QueryContext(ctx, `SELECT `+column+` FROM local_operator_version LIMIT 0`)
	if err == nil {
		return rows.Close()
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE local_operator_version ADD COLUMN `+column+` `+definition)
	return err
}

func (h *Handler) SeedSystemOperators(ctx context.Context) error {
	if h == nil || h.db == nil {
		return fmt.Errorf("local admin: database is required")
	}
	if err := ensureFacts(ctx, h.db); err != nil {
		return err
	}
	for _, seed := range systemOperatorSeeds {
		if err := h.seedSystemOperator(ctx, seed); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) seedSystemOperator(ctx context.Context, seed operatorInput) error {
	inputSchema, err := normalizeOperatorSchema(seed.InputSchema, anySchema)
	if err != nil {
		return fmt.Errorf("system operator %q input schema: %w", seed.OperatorKey, err)
	}
	paramsSchema, err := normalizeOperatorSchema(seed.ParamsSchema, objectParamsSchema)
	if err != nil {
		return fmt.Errorf("system operator %q params schema: %w", seed.OperatorKey, err)
	}
	outputSchema, err := normalizeOperatorSchema(seed.OutputSchema, anySchema)
	if err != nil {
		return fmt.Errorf("system operator %q output schema: %w", seed.OperatorKey, err)
	}
	hash := contentHash(seed.SourceCode)
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	var runtimeID int64
	var version, status int
	var storedHash, language, entry, source, storedInput, storedParams, storedOutput string
	err = tx.QueryRowContext(ctx, `SELECT id,runtime_id,version_no,status,source_hash,language,entry_name,source_code,input_schema_json,params_schema_json,output_schema_json FROM local_operator_version WHERE operator_key=? AND version_no=1`, seed.OperatorKey).
		Scan(&id, &runtimeID, &version, &status, &storedHash, &language, &entry, &source, &storedInput, &storedParams, &storedOutput)
	if err == sql.ErrNoRows {
		now := time.Now().UTC().Format(time.RFC3339)
		id = "opv_" + shortHash(seed.OperatorKey+"/1")
		runtimeID = systemOperatorRuntimeID(seed.OperatorKey, 1)
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO local_operator_version(id,runtime_id,operator_key,version_no,status,name,description,usage_scenario,language,entry_name,source_code,source_hash,input_schema_json,params_schema_json,output_schema_json,gmt_create,gmt_published) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, runtimeID, seed.OperatorKey, 1, 1, seed.Name, seed.Description, seed.UsageScenario, seed.Language, seed.Entry,
			seed.SourceCode, hash, inputSchema, paramsSchema, outputSchema, now, now)
		if insertErr != nil {
			return insertErr
		}
	} else if err != nil {
		return err
	} else {
		storedInput, err = normalizeOperatorSchema(json.RawMessage(storedInput), anySchema)
		if err != nil {
			return fmt.Errorf("system operator %q stored input schema: %w", seed.OperatorKey, err)
		}
		storedParams, err = normalizeOperatorSchema(json.RawMessage(storedParams), objectParamsSchema)
		if err != nil {
			return fmt.Errorf("system operator %q stored params schema: %w", seed.OperatorKey, err)
		}
		storedOutput, err = normalizeOperatorSchema(json.RawMessage(storedOutput), anySchema)
		if err != nil {
			return fmt.Errorf("system operator %q stored output schema: %w", seed.OperatorKey, err)
		}
		if version != 1 || status != 1 || storedHash != hash || language != seed.Language || entry != seed.Entry ||
			source != seed.SourceCode || storedInput != inputSchema || storedParams != paramsSchema || storedOutput != outputSchema {
			return fmt.Errorf("system operator %q version 1 conflicts with the bundled immutable definition", seed.OperatorKey)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO knowrag_operator(operator_id,operator_key,summary,published) VALUES(?,?,?,1) ON CONFLICT(operator_id) DO UPDATE SET operator_key=excluded.operator_key,summary=excluded.summary,published=1`, runtimeID, seed.OperatorKey, seed.Description); err != nil {
		return err
	}
	return tx.Commit()
}

func systemOperatorRuntimeID(key string, version int) int64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("agenui-system-operator\x00%s\x00%d", key, version)))
	const maxJSONSafeInteger = uint64(1<<53 - 1)
	value := binary.BigEndian.Uint64(sum[:8]) & maxJSONSafeInteger
	if value == 0 {
		value = 1
	}
	return int64(value)
}

func normalizeOperatorSchema(raw json.RawMessage, fallback string) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		trimmed = fallback
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return "", fmt.Errorf("must be valid JSON: %w", err)
	}
	switch value.(type) {
	case bool, map[string]any:
	default:
		return "", fmt.Errorf("must be a JSON object or boolean")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

type operatorRow interface {
	Scan(dest ...any) error
}

func scanOperator(row operatorRow) (operator, error) {
	var value operator
	var inputSchema, paramsSchema, outputSchema string
	err := row.Scan(
		&value.OperatorVersionID, &value.RuntimeID, &value.OperatorKey, &value.VersionNo,
		&value.Status, &value.Name, &value.Description, &value.UsageScenario,
		&value.Language, &value.Entry, &value.SourceCode, &value.SourceHash,
		&inputSchema, &paramsSchema, &outputSchema, &value.CreatedAt, &value.PublishedAt,
	)
	if err != nil {
		return operator{}, err
	}
	value.InputSchema = json.RawMessage(inputSchema)
	value.ParamsSchema = json.RawMessage(paramsSchema)
	value.OutputSchema = json.RawMessage(outputSchema)
	return value, nil
}
