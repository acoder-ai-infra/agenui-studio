CREATE TABLE IF NOT EXISTS mcp_servers (
  tenant_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  definition_json TEXT NOT NULL CHECK (json_valid(definition_json)),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
  updated_by TEXT NOT NULL,
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, server_id),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS mcp_servers_tenant_updated_idx
  ON mcp_servers (tenant_id, updated_at_ms DESC, server_id);
