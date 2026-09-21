CREATE TABLE IF NOT EXISTS http_tools (
  tenant_id TEXT NOT NULL,
  tool_name TEXT NOT NULL,
  definition_json TEXT NOT NULL CHECK (json_valid(definition_json)),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
  updated_by TEXT NOT NULL,
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, tool_name),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS http_tools_tenant_updated_idx
  ON http_tools (tenant_id, updated_at_ms DESC, tool_name);
