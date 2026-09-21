CREATE TABLE IF NOT EXISTS skill_versions (
  tenant_id TEXT NOT NULL,
  skill_id TEXT NOT NULL,
  version TEXT NOT NULL,
  definition_json TEXT NOT NULL CHECK (json_valid(definition_json)),
  content_hash TEXT NOT NULL,
  content_size INTEGER NOT NULL CHECK (content_size > 0 AND content_size <= 1048576),
  content BLOB NOT NULL,
  published_at_ms INTEGER NOT NULL CHECK (published_at_ms >= 0),
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, skill_id, version),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS skill_versions_tenant_id_idx
  ON skill_versions (tenant_id, skill_id, published_at_ms DESC);
