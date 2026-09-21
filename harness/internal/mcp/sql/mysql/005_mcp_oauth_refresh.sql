ALTER TABLE mcp_oauth_grants ADD COLUMN issuer VARCHAR(1024) NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN token_endpoint VARCHAR(2048) NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN client_id VARCHAR(512) NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN client_secret_ciphertext VARCHAR(4096) NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_grants ADD COLUMN token_auth_method VARCHAR(64) NOT NULL DEFAULT 'none';
