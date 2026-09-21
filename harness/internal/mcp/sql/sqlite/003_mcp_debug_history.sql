CREATE TABLE IF NOT EXISTS mcp_debug_records (
  tenant_id TEXT NOT NULL,
  record_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  operator_id TEXT NOT NULL,
  operation TEXT NOT NULL CHECK (operation IN ('list_tools', 'call_tool')),
  tool_name TEXT NOT NULL DEFAULT '',
  snapshot_id TEXT NOT NULL DEFAULT '',
  request_json TEXT NOT NULL CHECK (json_valid(request_json)),
  response_json TEXT NOT NULL CHECK (json_valid(response_json)),
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  latency_ms INTEGER NOT NULL CHECK (latency_ms >= 0),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, record_id),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS mcp_debug_records_server_created_idx
  ON mcp_debug_records (tenant_id, server_id, created_at_ms DESC, record_id DESC);
