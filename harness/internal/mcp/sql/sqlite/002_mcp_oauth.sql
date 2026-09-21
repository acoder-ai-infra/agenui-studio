CREATE TABLE IF NOT EXISTS mcp_oauth_grants (
  tenant_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  scope_hash TEXT NOT NULL,
  scopes_json TEXT NOT NULL CHECK (json_valid(scopes_json)),
  resource TEXT NOT NULL DEFAULT '',
  access_token_ciphertext TEXT NOT NULL,
  refresh_token_ciphertext TEXT NOT NULL DEFAULT '',
  access_expires_at_ms INTEGER NOT NULL DEFAULT 0 CHECK (access_expires_at_ms >= 0),
  refresh_expires_at_ms INTEGER NOT NULL DEFAULT 0 CHECK (refresh_expires_at_ms >= 0),
  status TEXT NOT NULL CHECK (status IN ('active', 'revoked')),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= 0),
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, user_id, server_id, provider, scope_hash),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS mcp_oauth_grants_user_updated_idx
  ON mcp_oauth_grants (tenant_id, user_id, updated_at_ms DESC);
