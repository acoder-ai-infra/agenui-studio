CREATE TABLE IF NOT EXISTS model_providers (
  tenant_id TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  definition_json TEXT NOT NULL CHECK (json_valid(definition_json)),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
  updated_by TEXT NOT NULL,
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, provider_id),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS model_providers_tenant_updated_idx
  ON model_providers (tenant_id, updated_at_ms DESC, provider_id);
