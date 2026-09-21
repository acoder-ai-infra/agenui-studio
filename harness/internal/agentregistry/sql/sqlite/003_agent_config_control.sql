-- Tenant-scoped Agent configuration control plane for local development and CI.
-- Runtime Registry tables remain unchanged; these tables manage authoring and
-- release facts without changing how an existing Run resolves configuration.

CREATE TABLE IF NOT EXISTS agent_config_drafts (
  tenant_id TEXT NOT NULL CHECK (length(trim(tenant_id)) BETWEEN 1 AND 255),
  agent_id TEXT NOT NULL CHECK (length(trim(agent_id)) BETWEEN 1 AND 255),
  config_json TEXT NOT NULL CHECK (json_valid(config_json) AND json_type(config_json) = 'object'),
  content_hash TEXT NOT NULL CHECK (length(trim(content_hash)) > 0),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
  created_by TEXT NOT NULL CHECK (length(trim(created_by)) > 0),
  updated_by TEXT NOT NULL CHECK (length(trim(updated_by)) > 0),
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, agent_id),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS agent_config_drafts_tenant_time_idx
  ON agent_config_drafts (tenant_id, updated_at_ms DESC, agent_id);

CREATE TABLE IF NOT EXISTS agent_config_versions (
  tenant_id TEXT NOT NULL CHECK (length(trim(tenant_id)) BETWEEN 1 AND 255),
  agent_id TEXT NOT NULL CHECK (length(trim(agent_id)) BETWEEN 1 AND 255),
  version TEXT NOT NULL CHECK (length(trim(version)) BETWEEN 1 AND 255),
  config_json TEXT NOT NULL CHECK (json_valid(config_json) AND json_type(config_json) = 'object'),
  content_hash TEXT NOT NULL CHECK (length(trim(content_hash)) > 0),
  source_draft_revision INTEGER NOT NULL CHECK (source_draft_revision >= 1),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  created_by TEXT NOT NULL CHECK (length(trim(created_by)) > 0),
  prepared_json BLOB,
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, agent_id, version),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS agent_config_versions_tenant_agent_time_idx
  ON agent_config_versions (tenant_id, agent_id, created_at_ms DESC, version DESC);

CREATE TABLE IF NOT EXISTS agent_config_releases (
  tenant_id TEXT NOT NULL CHECK (length(trim(tenant_id)) BETWEEN 1 AND 255),
  environment TEXT NOT NULL CHECK (environment IN ('local', 'testing', 'staging', 'production')),
  agent_id TEXT NOT NULL CHECK (length(trim(agent_id)) BETWEEN 1 AND 255),
  version TEXT NOT NULL CHECK (length(trim(version)) BETWEEN 1 AND 255),
  content_hash TEXT NOT NULL CHECK (length(trim(content_hash)) > 0),
  revision INTEGER NOT NULL CHECK (revision >= 1),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms),
  updated_by TEXT NOT NULL CHECK (length(trim(updated_by)) > 0),
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, environment, agent_id),
  UNIQUE (identity_digest)
);

CREATE INDEX IF NOT EXISTS agent_config_releases_tenant_environment_idx
  ON agent_config_releases (tenant_id, environment, updated_at_ms DESC, agent_id);

CREATE TABLE IF NOT EXISTS agent_config_release_events (
  event_id TEXT NOT NULL PRIMARY KEY CHECK (length(trim(event_id)) > 0),
  tenant_id TEXT NOT NULL CHECK (length(trim(tenant_id)) BETWEEN 1 AND 255),
  environment TEXT NOT NULL CHECK (environment IN ('local', 'testing', 'staging', 'production')),
  agent_id TEXT NOT NULL CHECK (length(trim(agent_id)) BETWEEN 1 AND 255),
  from_version TEXT NOT NULL DEFAULT '',
  to_version TEXT NOT NULL CHECK (length(trim(to_version)) BETWEEN 1 AND 255),
  content_hash TEXT NOT NULL CHECK (length(trim(content_hash)) > 0),
  release_revision INTEGER NOT NULL CHECK (release_revision >= 1),
  actor TEXT NOT NULL CHECK (length(trim(actor)) > 0),
  reason TEXT NOT NULL DEFAULT '',
  occurred_at_ms INTEGER NOT NULL CHECK (occurred_at_ms >= 0),
  event_id_digest BLOB NOT NULL CHECK (length(event_id_digest) = 32),
  UNIQUE (event_id_digest)
);

CREATE INDEX IF NOT EXISTS agent_config_release_events_scope_time_idx
  ON agent_config_release_events (
    tenant_id,
    environment,
    agent_id,
    occurred_at_ms DESC,
    event_id DESC
  );
