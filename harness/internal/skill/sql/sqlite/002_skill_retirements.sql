CREATE TABLE IF NOT EXISTS skill_retirements (
  tenant_id TEXT NOT NULL CHECK (tenant_id <> ''),
  skill_id TEXT NOT NULL,
  version TEXT NOT NULL,
  retired_at_ms INTEGER NOT NULL CHECK (retired_at_ms >= 0),
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, skill_id, version),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS skill_retirements_tenant_id_idx
  ON skill_retirements (tenant_id, skill_id, retired_at_ms DESC);
