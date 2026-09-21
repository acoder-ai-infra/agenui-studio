-- MCP OAuth Authorization Code + PKCE 临时状态。state 只保存 SHA-256 摘要，
-- code_verifier 与动态注册 client_secret 使用应用 AEAD 密文保存。
CREATE TABLE IF NOT EXISTS mcp_oauth_pending (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  state_digest BINARY(32) NOT NULL,
  tenant_id VARCHAR(255) NOT NULL,
  user_id VARCHAR(255) NOT NULL,
  server_id VARCHAR(128) NOT NULL,
  provider VARCHAR(128) NOT NULL,
  scope_hash VARCHAR(96) NOT NULL,
  scopes_json JSON NOT NULL,
  resource VARCHAR(1024) NOT NULL DEFAULT '',
  issuer VARCHAR(1024) NOT NULL,
  token_endpoint VARCHAR(2048) NOT NULL,
  redirect_uri VARCHAR(2048) NOT NULL,
  client_id VARCHAR(512) NOT NULL,
  client_secret_ciphertext TEXT NOT NULL,
  token_auth_method VARCHAR(64) NOT NULL DEFAULT 'none',
  code_verifier_ciphertext TEXT NOT NULL,
  expires_at_ms BIGINT NOT NULL,
  consumed_at_ms BIGINT NOT NULL DEFAULT 0,
  created_at_ms BIGINT NOT NULL,
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_mcp_oauth_pending_state (state_digest),
  KEY idx_mcp_oauth_pending_expiry (expires_at_ms, consumed_at_ms),
  CONSTRAINT chk_mcp_oauth_pending_expiry CHECK (expires_at_ms > 0),
  CONSTRAINT chk_mcp_oauth_pending_consumed CHECK (consumed_at_ms >= 0),
  CONSTRAINT chk_mcp_oauth_pending_created CHECK (created_at_ms > 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='MCP OAuth 一次性授权临时状态';
