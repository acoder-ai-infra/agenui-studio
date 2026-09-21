CREATE TABLE IF NOT EXISTS tenants (
  tenant_id     TEXT NOT NULL PRIMARY KEY,
  name          TEXT NOT NULL,
  owner         TEXT NOT NULL DEFAULT '',
  plan          TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT 'active',
  secret_key    TEXT NOT NULL,
  revision      INTEGER NOT NULL CHECK (revision >= 1),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
  updated_by    TEXT NOT NULL,
  UNIQUE (secret_key)
);

CREATE INDEX IF NOT EXISTS tenants_updated_idx
  ON tenants (updated_at_ms DESC, tenant_id);
