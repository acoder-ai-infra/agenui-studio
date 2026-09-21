ALTER TABLE mcp_oauth_grants ADD COLUMN issuer TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN token_endpoint TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN client_secret_ciphertext TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN token_auth_method TEXT NOT NULL DEFAULT 'none';
