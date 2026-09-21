// Package mysql is the MySQL backend (D3=MySQL) for the core ledger.
//
// It depends only on the standard library database/sql and wraps a connection
// pool created and owned by the caller.
// The live integration test lives behind the `mysql_integration` build tag.
// It implements the complete durable aggregate, including transactional event
// append, run CAS and fenced ResumeStore claims.
package mysql

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

//go:embed schema.sql
var Schema string

// Backend is the MySQL storage backend.
type Backend struct {
	db     *sql.DB
	schema physicalSchema
}

type columnInfo struct {
	dataType   string
	columnType string
}

type physicalSchema struct {
	eventUsageColumn string
}

func defaultPhysicalSchema() physicalSchema {
	return physicalSchema{eventUsageColumn: "usage"}
}

// id intentionally preserves canonical business IDs. Ledger identifiers are
// stored as strings; physical surrogate keys must use separate columns.
func (s physicalSchema) id(table, column, value string) any {
	return value
}

func (s physicalSchema) nullID(table, column, value string) any {
	if value == "" {
		return nil
	}
	return s.id(table, column, value)
}

// New wraps a caller-owned *sql.DB.
func New(db *sql.DB) *Backend { return &Backend{db: db, schema: defaultPhysicalSchema()} }

// Close closes the underlying connection pool.
func (b *Backend) Close() error { return b.db.Close() }

func (b *Backend) Name() string { return "mysql" }

// Capabilities reports MySQL's functional capabilities.
func (b *Backend) Capabilities() map[storage.Capability]bool {
	return map[storage.Capability]bool{
		storage.CapCAS:             true,
		storage.CapTransaction:     true,
		storage.CapOrderedAppend:   true,
		storage.CapIdempotency:     true,
		storage.CapTTL:             true,
		storage.CapPrefixScan:      true,
		storage.CapPagination:      true,
		storage.CapSecondaryIndex:  true,
		storage.CapTenantIsolation: true,
	}
}

// Health pings the database.
func (b *Backend) Health(ctx context.Context) error { return b.db.PingContext(ctx) }

// CheckReady verifies that the already-provisioned schema matches the storage
// contract. Runtime startup must not create or alter production tables.
func (b *Backend) CheckReady(ctx context.Context) error {
	schema := defaultPhysicalSchema()
	for _, table := range requiredMySQLTableOrder() {
		have, err := b.columns(ctx, table)
		if err != nil {
			return err
		}
		if len(have) == 0 {
			return fmt.Errorf("mysql schema not ready: table %s is missing; run storage migration before starting Runtime", table)
		}
		for _, column := range requiredMySQLColumns()[table] {
			if _, ok := have[column]; ok {
				continue
			}
			if table == "agent_events" && column == "usage" {
				if _, renamed := have["usage_data"]; renamed {
					schema.eventUsageColumn = "usage_data"
					continue
				}
			}
			return missingRequiredColumnError(table, column, have)
		}
		if err := schema.observeColumnTypes(table, have); err != nil {
			return err
		}
	}
	b.schema = schema
	return nil
}

func (b *Backend) columns(ctx context.Context, table string) (map[string]columnInfo, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT column_name, data_type, column_type
		FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name=?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]columnInfo)
	for rows.Next() {
		var name, dataType, columnType string
		if err := rows.Scan(&name, &dataType, &columnType); err != nil {
			return nil, err
		}
		out[name] = columnInfo{dataType: strings.ToLower(dataType), columnType: strings.ToLower(columnType)}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func missingRequiredColumnError(table, column string, have map[string]columnInfo) error {
	if table == "agent_events" && column == "usage" {
		if _, renamed := have["usage_data"]; renamed {
			return fmt.Errorf("mysql schema not ready: column agent_events.usage is missing; run storage migration before starting Runtime")
		}
	}
	return fmt.Errorf("mysql schema not ready: column %s.%s is missing; run storage migration before starting Runtime", table, column)
}

// Migrate applies schema.sql by splitting and executing statements individually.
func (b *Backend) Migrate(ctx context.Context) error {
	for _, statement := range mysqlSchemaStatements(Schema) {
		if _, err := b.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "runtime_binding", definition: "JSON NULL"},
		{name: "resume_attempt_id", definition: "VARCHAR(128) NULL"},
	} {
		if err := b.ensureColumn(ctx, "runs", column.name, column.definition); err != nil {
			return err
		}
	}
	// 旧库可能仍有 resume_lease_until；新代码不再读写，避免在线 DROP 带来额外风险。
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "parent_span_id", definition: "VARCHAR(64) NULL"},
		{name: "agent_type", definition: "VARCHAR(64) NULL"},
		{name: "runtime", definition: "VARCHAR(32) NULL"},
	} {
		if err := b.ensureColumn(ctx, "agent_events", column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "total_latency_ms", definition: "BIGINT NOT NULL DEFAULT 0 COMMENT '总耗时，单位毫秒'"},
		{name: "first_token_observed", definition: "TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否观测到首 token，0 否 1 是'"},
		{name: "first_token_ms", definition: "BIGINT NOT NULL DEFAULT 0 COMMENT '首 token 耗时，单位毫秒'"},
		{name: "generation_duration_ms", definition: "BIGINT NOT NULL DEFAULT 0 COMMENT '生成阶段耗时，单位毫秒'"},
		{name: "output_tokens_per_second", definition: "DECIMAL(18,6) NOT NULL DEFAULT 0 COMMENT '每秒输出 token 数'"},
	} {
		if err := b.ensureColumn(ctx, "model_usage_records", column.name, column.definition); err != nil {
			return err
		}
	}
	return b.CheckReady(ctx)
}

func mysqlSchemaStatements(schema string) []string {
	var executable strings.Builder
	for _, line := range strings.Split(schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		executable.WriteString(line)
		executable.WriteByte('\n')
	}

	statements := make([]string, 0)
	for _, statement := range strings.Split(executable.String(), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

func (b *Backend) ensureColumn(ctx context.Context, table, column, definition string) error {
	exists, err := b.columnExists(ctx, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, alterErr := b.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	if alterErr == nil {
		return nil
	}
	// 滚动启动时两个实例可能同时观察到缺列；后执行者若看到另一实例已补齐，
	// 将 duplicate-column 类错误收敛为幂等成功，其他 DDL 错误仍原样失败。
	exists, inspectErr := b.columnExists(ctx, table, column)
	if inspectErr == nil && exists {
		return nil
	}
	if inspectErr != nil {
		return errors.Join(alterErr, fmt.Errorf("recheck column after ALTER failure: %w", inspectErr))
	}
	return alterErr
}

func (b *Backend) columnExists(ctx context.Context, table, column string) (bool, error) {
	var count int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, table, column).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func requiredMySQLTableOrder() []string {
	return []string{
		"sessions",
		"messages",
		"model_usage_records",
		"runs",
		"steps",
		"agent_events",
		"checkpoint_meta",
		"control_requests",
		"idempotency_keys",
		"open_turn_idempotency",
	}
}

func requiredMySQLColumns() map[string][]string {
	return map[string][]string{
		"sessions": {
			"id", "tenant_id", "user_id", "channel", "agent_id", "title", "status",
			"created_at", "updated_at", "version", "metadata",
		},
		"messages": {
			"id", "session_id", "turn_id", "run_id", "tenant_id", "role", "visibility",
			"content_ref", "content_preview", "created_at",
		},
		"model_usage_records": {
			"usage_id", "request_id", "trace_id", "tenant_id", "session_id", "run_id",
			"agent_id", "provider", "model", "attempt", "fallback_applied", "prompt_tokens",
			"completion_tokens", "reasoning_tokens", "cache_read_tokens", "cache_write_tokens",
			"usage_source", "currency", "estimated_cost", "total_latency_ms",
			"first_token_observed", "first_token_ms", "generation_duration_ms",
			"output_tokens_per_second", "cache_hit", "output_ref", "created_at",
		},
		"runs": {
			"run_id", "session_id", "turn_id", "parent_run_id", "tenant_id", "agent_id",
			"runtime", "runtime_binding", "status", "trace_id", "config_snapshot_ref",
			"context_snapshot_ref", "agent_binding_id", "resume_attempt_id", "started_at",
			"ended_at", "error_code", "error_message", "version",
		},
		"steps": {
			"step_id", "run_id", "parent_step_id", "step_type", "name", "status",
			"started_at", "ended_at",
		},
		"agent_events": {
			"event_id", "run_id", "sequence", "tenant_id", "session_id", "step_id",
			"agent_id", "trace_id", "span_id", "parent_span_id", "agent_type", "runtime",
			"event_type", "visibility", "schema_version", "idempotency_key", "payload",
			"payload_preview", "payload_ref", "usage", "debug_ref", "error", "created_at",
		},
		"checkpoint_meta": {
			"checkpoint_id", "run_id", "tenant_id", "runtime", "type", "state_ref",
			"event_sequence", "created_reason", "created_at", "expires_at",
		},
		"control_requests": {
			"request_id", "run_id", "tenant_id", "checkpoint_id", "type", "status",
			"resume_token_hash", "tool_use_id", "prompt_preview", "response_ref",
			"schema_version", "created_at", "expires_at", "version",
		},
		"idempotency_keys": {
			"id", "expires_at", "created_at",
		},
		"open_turn_idempotency": {
			"tenant_id", "user_id", "session_scope", "idem_key", "request_hash",
			"session_id", "run_id", "message_id", "created_at",
		},
	}
}

func requiredColumns() map[string][]string {
	return requiredMySQLColumns()
}

func (s *physicalSchema) observeColumnTypes(table string, have map[string]columnInfo) error {
	for _, column := range requiredStringColumns()[table] {
		info, ok := have[column]
		if !ok {
			continue
		}
		if !mysqlStringCompatible(info.dataType) {
			return fmt.Errorf("mysql schema not ready: column %s.%s has type %s; expected string type", table, column, displayColumnType(info))
		}
	}
	for column, minimum := range requiredIdentifierColumnWidths()[table] {
		info, ok := have[column]
		if !ok {
			continue
		}
		capacity, ok := mysqlStringCapacity(info)
		if !ok || capacity < minimum {
			return fmt.Errorf("mysql schema not ready: column %s.%s has type %s; expected capacity of at least %d characters", table, column, displayColumnType(info), minimum)
		}
	}
	for _, column := range requiredJSONColumns()[table] {
		physicalColumn := column
		if table == "agent_events" && column == "usage" && s.eventUsageColumn != "" {
			physicalColumn = s.eventUsageColumn
		}
		info, ok := have[physicalColumn]
		if !ok {
			continue
		}
		if info.dataType != "json" {
			return fmt.Errorf("mysql schema not ready: column %s.%s has type %s, want json", table, physicalColumn, displayColumnType(info))
		}
	}
	return nil
}

func requiredIdentifierColumnWidths() map[string]map[string]int {
	id64 := identifiercontract.MaxRunIDCharacters
	agent128 := identifiercontract.MaxAgentIDCharacters
	return map[string]map[string]int{
		"sessions":              {"id": id64, "tenant_id": id64, "user_id": id64, "agent_id": agent128},
		"messages":              {"id": id64, "session_id": id64, "turn_id": id64, "run_id": id64, "tenant_id": id64},
		"model_usage_records":   {"usage_id": id64, "request_id": id64, "trace_id": id64, "tenant_id": id64, "session_id": id64, "run_id": id64, "agent_id": agent128},
		"runs":                  {"run_id": id64, "session_id": id64, "turn_id": id64, "parent_run_id": id64, "tenant_id": id64, "agent_id": agent128, "trace_id": id64, "agent_binding_id": identifiercontract.MaxAgentBindingIDCharacters, "resume_attempt_id": identifiercontract.MaxResumeAttemptIDCharacters},
		"steps":                 {"step_id": id64, "run_id": id64, "parent_step_id": id64},
		"agent_events":          {"event_id": id64, "run_id": id64, "tenant_id": id64, "session_id": id64, "step_id": id64, "agent_id": agent128, "trace_id": id64, "span_id": id64, "parent_span_id": id64, "agent_type": identifiercontract.MaxAgentTypeCharacters, "idempotency_key": identifiercontract.MaxIdempotencyKeyCharacters},
		"checkpoint_meta":       {"checkpoint_id": id64, "run_id": id64, "tenant_id": id64},
		"control_requests":      {"request_id": id64, "run_id": id64, "tenant_id": id64, "checkpoint_id": id64, "resume_token_hash": identifiercontract.MaxResumeTokenHashCharacters, "tool_use_id": id64},
		"idempotency_keys":      {"id": identifiercontract.MaxGenericIdempotencyIDCharacters},
		"open_turn_idempotency": {"tenant_id": id64, "user_id": id64, "session_scope": identifiercontract.MaxIdempotencyScopeCharacters, "idem_key": identifiercontract.MaxIdempotencyKeyCharacters, "request_hash": id64, "session_id": id64, "run_id": id64, "message_id": id64},
	}
}

func mysqlStringCapacity(info columnInfo) (int, bool) {
	switch strings.ToLower(info.dataType) {
	case "text", "mediumtext", "longtext":
		return int(^uint(0) >> 1), true
	case "char", "varchar":
		columnType := strings.ReplaceAll(strings.ToLower(info.columnType), " ", "")
		start, end := strings.IndexByte(columnType, '('), strings.IndexByte(columnType, ')')
		if start < 0 || end <= start+1 {
			return 0, false
		}
		capacity, err := strconv.Atoi(columnType[start+1 : end])
		return capacity, err == nil
	default:
		return 0, false
	}
}

func requiredStringColumns() map[string][]string {
	return map[string][]string{
		"sessions": {
			"id", "tenant_id", "user_id", "channel", "agent_id", "title", "status",
		},
		"messages": {
			"id", "session_id", "turn_id", "run_id", "tenant_id", "role", "visibility",
			"content_ref", "content_preview",
		},
		"model_usage_records": {
			"usage_id", "request_id", "trace_id", "tenant_id", "session_id", "run_id",
			"agent_id", "provider", "model", "usage_source", "currency", "output_ref",
		},
		"runs": {
			"run_id", "session_id", "turn_id", "parent_run_id", "tenant_id", "agent_id",
			"runtime", "status", "trace_id", "config_snapshot_ref", "context_snapshot_ref",
			"agent_binding_id", "resume_attempt_id", "error_code", "error_message",
		},
		"steps": {
			"step_id", "run_id", "parent_step_id", "step_type", "name", "status",
		},
		"agent_events": {
			"event_id", "run_id", "tenant_id", "session_id", "step_id", "agent_id",
			"trace_id", "span_id", "parent_span_id", "agent_type", "runtime", "event_type",
			"visibility", "schema_version", "idempotency_key", "payload_ref", "debug_ref",
		},
		"checkpoint_meta": {
			"checkpoint_id", "run_id", "tenant_id", "runtime", "type", "state_ref",
			"created_reason",
		},
		"control_requests": {
			"request_id", "run_id", "tenant_id", "checkpoint_id", "type", "status",
			"resume_token_hash", "tool_use_id", "prompt_preview", "response_ref",
			"schema_version",
		},
		"idempotency_keys": {
			"id",
		},
		"open_turn_idempotency": {
			"tenant_id", "user_id", "session_scope", "idem_key", "request_hash",
			"session_id", "run_id", "message_id",
		},
	}
}

func requiredJSONColumns() map[string][]string {
	return map[string][]string{
		"sessions":     {"metadata"},
		"runs":         {"runtime_binding"},
		"agent_events": {"payload", "payload_preview", "usage", "error"},
	}
}

func mysqlStringCompatible(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext":
		return true
	default:
		return false
	}
}

func displayColumnType(info columnInfo) string {
	if info.columnType != "" {
		return info.columnType
	}
	if info.dataType != "" {
		return info.dataType
	}
	return "<unknown>"
}

// EventStore returns the MySQL EventStore.
func (b *Backend) EventStore() storage.EventStore { return &eventStore{db: b.db, schema: b.schema} }

// RunStore returns the MySQL RunStore.
func (b *Backend) RunStore() storage.RunStore { return &runStore{db: b.db, schema: b.schema} }

// SessionStore returns the MySQL SessionStore.
func (b *Backend) SessionStore() storage.SessionStore {
	return &sessionStore{db: b.db, schema: b.schema}
}

// MessageStore returns the MySQL MessageStore.
func (b *Backend) MessageStore() storage.MessageStore {
	return &messageStore{db: b.db, schema: b.schema}
}

// ModelUsageStore returns the MySQL ModelUsageStore.
func (b *Backend) ModelUsageStore() storage.ModelUsageStore {
	return &usageStore{db: b.db, schema: b.schema}
}

// StepStore returns the MySQL StepStore.
func (b *Backend) StepStore() storage.StepStore { return &stepStore{db: b.db, schema: b.schema} }

// CheckpointStore returns the MySQL CheckpointStore.
func (b *Backend) CheckpointStore() storage.CheckpointStore {
	return &checkpointStore{db: b.db, schema: b.schema}
}

// ControlRequestStore returns the MySQL ControlRequestStore.
func (b *Backend) ControlRequestStore() storage.ControlRequestStore {
	return &controlStore{db: b.db, schema: b.schema}
}

// IdempotencyStore returns the MySQL IdempotencyStore.
func (b *Backend) IdempotencyStore() storage.IdempotencyStore {
	return &idempotencyStore{db: b.db, schema: b.schema}
}
func (b *Backend) ResumeStore() storage.ResumeStore {
	events := &eventStore{db: b.db, schema: b.schema}
	return &resumeStore{db: b.db, events: events, schema: b.schema}
}
func (b *Backend) OpenTurnStore() storage.OpenTurnStore {
	return &openTurnStore{db: b.db, ids: observability.NewULIDGenerator(""), schema: b.schema}
}

// Stores returns every store port backed by MySQL.
func (b *Backend) Stores() storage.Stores {
	return storage.Stores{
		Turns:       b.OpenTurnStore(),
		Sessions:    b.SessionStore(),
		Messages:    b.MessageStore(),
		Usage:       b.ModelUsageStore(),
		Runs:        b.RunStore(),
		Steps:       b.StepStore(),
		Events:      b.EventStore(),
		Checkpoints: b.CheckpointStore(),
		Controls:    b.ControlRequestStore(),
		Idem:        b.IdempotencyStore(),
		Resumes:     b.ResumeStore(),
	}
}

var _ storage.Backend = (*Backend)(nil)
