CREATE TABLE IF NOT EXISTS mcp_oauth_pending (
  state_digest BLOB NOT NULL CHECK (length(state_digest) = 32),
  tenant_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  scope_hash TEXT NOT NULL,
  scopes_json TEXT NOT NULL CHECK (json_valid(scopes_json)),
  resource TEXT NOT NULL DEFAULT '',
  issuer TEXT NOT NULL,
  token_endpoint TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  client_id TEXT NOT NULL,
  client_secret_ciphertext TEXT NOT NULL DEFAULT '',
  token_auth_method TEXT NOT NULL DEFAULT 'none',
  code_verifier_ciphertext TEXT NOT NULL,
  expires_at_ms INTEGER NOT NULL CHECK (expires_at_ms > 0),
  consumed_at_ms INTEGER NOT NULL DEFAULT 0 CHECK (consumed_at_ms >= 0),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms > 0),
  PRIMARY KEY (state_digest)
);

CREATE INDEX IF NOT EXISTS mcp_oauth_pending_expiry_idx
  ON mcp_oauth_pending (expires_at_ms, consumed_at_ms);
