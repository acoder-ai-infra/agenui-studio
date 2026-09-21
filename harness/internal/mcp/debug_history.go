package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DebugOperationListTools = "list_tools"
	DebugOperationCallTool  = "call_tool"
)

type DebugRecord struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id"`
	ServerID     string          `json:"server_id"`
	OperatorID   string          `json:"operator_id"`
	Operation    string          `json:"operation"`
	ToolName     string          `json:"tool_name,omitempty"`
	SnapshotID   string          `json:"snapshot_id,omitempty"`
	RequestJSON  json.RawMessage `json:"request_json,omitempty"`
	ResponseJSON json.RawMessage `json:"response_json,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	LatencyMS    int64           `json:"latency_ms"`
	CreatedAt    time.Time       `json:"created_at"`
}

type DebugHistoryStore interface {
	AppendDebugRecord(ctx context.Context, record DebugRecord) (DebugRecord, error)
	ListDebugRecords(ctx context.Context, tenantID, serverID string, limit int) ([]DebugRecord, error)
}

func (r *SQLManagedRegistry) AppendDebugRecord(ctx context.Context, record DebugRecord) (DebugRecord, error) {
	if r == nil || r.db == nil {
		return DebugRecord{}, errorsConfiguration("database is required")
	}
	if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.TenantID) == "" || strings.TrimSpace(record.ServerID) == "" || strings.TrimSpace(record.OperatorID) == "" {
		return DebugRecord{}, errorsConfiguration("debug record requires id, tenant, server and operator")
	}
	if record.Operation != DebugOperationListTools && record.Operation != DebugOperationCallTool {
		return DebugRecord{}, errorsConfiguration("unsupported debug operation")
	}
	if len(record.RequestJSON) == 0 {
		record.RequestJSON = json.RawMessage(`{}`)
	}
	if len(record.ResponseJSON) == 0 {
		record.ResponseJSON = json.RawMessage(`null`)
	}
	if !json.Valid(record.RequestJSON) || !json.Valid(record.ResponseJSON) {
		return DebugRecord{}, errorsConfiguration("debug record JSON must be valid")
	}
	if record.LatencyMS < 0 {
		record.LatencyMS = 0
	}
	if record.CreatedAt.IsZero() {
		if r.now != nil {
			record.CreatedAt = r.now().UTC()
		} else {
			record.CreatedAt = time.Now().UTC()
		}
	} else {
		record.CreatedAt = record.CreatedAt.UTC()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO mcp_debug_records (
tenant_id, record_id, server_id, operator_id, operation, tool_name, snapshot_id,
request_json, response_json, error_code, error_message, latency_ms, created_at_ms, identity_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.TenantID, record.ID, record.ServerID, record.OperatorID, record.Operation, record.ToolName, record.SnapshotID,
		record.RequestJSON, record.ResponseJSON, record.ErrorCode, record.ErrorMessage, record.LatencyMS, record.CreatedAt.UnixMilli(),
		mcpIdentityDigest(record.TenantID, record.ID),
	)
	if err != nil {
		return DebugRecord{}, fmt.Errorf("append mcp debug record: %w", err)
	}
	return cloneDebugRecord(record), nil
}

func (r *SQLManagedRegistry) ListDebugRecords(ctx context.Context, tenantID, serverID string, limit int) ([]DebugRecord, error) {
	if r == nil || r.db == nil || tenantID == "" || serverID == "" {
		return nil, errorsConfiguration("database, tenant and server are required")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT record_id, tenant_id, server_id, operator_id, operation, tool_name, snapshot_id,
request_json, response_json, error_code, error_message, latency_ms, created_at_ms
FROM mcp_debug_records WHERE tenant_id=? AND server_id=?
ORDER BY created_at_ms DESC, record_id DESC LIMIT ?`, tenantID, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []DebugRecord
	for rows.Next() {
		record, err := scanDebugRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

type debugRecordScanner interface{ Scan(...any) error }

func scanDebugRecord(row debugRecordScanner) (DebugRecord, error) {
	var record DebugRecord
	var requestJSON, responseJSON []byte
	var createdAtMS int64
	if err := row.Scan(&record.ID, &record.TenantID, &record.ServerID, &record.OperatorID, &record.Operation, &record.ToolName, &record.SnapshotID,
		&requestJSON, &responseJSON, &record.ErrorCode, &record.ErrorMessage, &record.LatencyMS, &createdAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DebugRecord{}, ErrServerNotFound
		}
		return DebugRecord{}, err
	}
	record.RequestJSON = append(json.RawMessage(nil), requestJSON...)
	record.ResponseJSON = append(json.RawMessage(nil), responseJSON...)
	record.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	return record, nil
}

func cloneDebugRecord(record DebugRecord) DebugRecord {
	record.RequestJSON = append(json.RawMessage(nil), record.RequestJSON...)
	record.ResponseJSON = append(json.RawMessage(nil), record.ResponseJSON...)
	return record
}

var _ DebugHistoryStore = (*SQLManagedRegistry)(nil)
