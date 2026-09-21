-- Tenant-scoped Agent configuration control plane for MySQL 8.
-- Business identities are stored in full and protected by application-written
-- SHA-256 tuple digests so unique indexes remain below the 767-byte precheck.

CREATE TABLE IF NOT EXISTS agent_config_drafts (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'InnoDB clustered primary key',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'Trusted tenant identity',
  agent_id VARCHAR(255) NOT NULL COMMENT 'Tenant-local Agent identity',
  config_json JSON NOT NULL COMMENT 'Mutable AgentConfig draft',
  content_hash VARCHAR(80) NOT NULL COMMENT 'Canonical draft JSON SHA-256',
  revision BIGINT NOT NULL COMMENT 'Optimistic-lock revision',
  created_at_ms BIGINT NOT NULL COMMENT 'Creation time in Unix milliseconds',
  updated_at_ms BIGINT NOT NULL COMMENT 'Last update time in Unix milliseconds',
  created_by VARCHAR(255) NOT NULL COMMENT 'Creator principal',
  updated_by VARCHAR(255) NOT NULL COMMENT 'Last editor principal',
  identity_digest BINARY(32) NOT NULL COMMENT 'tenant_id and agent_id tuple digest',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_agent_config_draft_identity (identity_digest),
  KEY idx_agent_config_draft_tenant_time (tenant_id(64), updated_at_ms DESC, agent_id(64)),
  CONSTRAINT chk_agent_config_draft_revision CHECK (revision >= 1),
  CONSTRAINT chk_agent_config_draft_created_at CHECK (created_at_ms >= 0),
  CONSTRAINT chk_agent_config_draft_updated_at CHECK (updated_at_ms >= created_at_ms)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Tenant Agent configuration drafts';

CREATE TABLE IF NOT EXISTS agent_config_versions (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'InnoDB clustered primary key',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'Trusted tenant identity',
  agent_id VARCHAR(255) NOT NULL COMMENT 'Tenant-local Agent identity',
  version VARCHAR(255) NOT NULL COMMENT 'Immutable tenant Agent version',
  config_json JSON NOT NULL COMMENT 'Immutable AgentConfig snapshot',
  content_hash VARCHAR(80) NOT NULL COMMENT 'Canonical version JSON SHA-256',
  source_draft_revision BIGINT NOT NULL COMMENT 'Draft revision copied at publish',
  created_at_ms BIGINT NOT NULL COMMENT 'Publish time in Unix milliseconds',
  created_by VARCHAR(255) NOT NULL COMMENT 'Publishing principal',
  prepared_json LONGTEXT NULL COMMENT 'Compiled immutable runtime projections',
  identity_digest BINARY(32) NOT NULL COMMENT 'tenant_id, agent_id and version tuple digest',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_agent_config_version_identity (identity_digest),
  KEY idx_agent_config_version_tenant_agent_time (tenant_id(64), agent_id(64), created_at_ms DESC, version(48)),
  CONSTRAINT chk_agent_config_version_revision CHECK (source_draft_revision >= 1),
  CONSTRAINT chk_agent_config_version_created_at CHECK (created_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Immutable tenant Agent configuration versions';

CREATE TABLE IF NOT EXISTS agent_config_releases (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'InnoDB clustered primary key',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'Trusted tenant identity',
  environment VARCHAR(32) NOT NULL COMMENT 'Release environment',
  agent_id VARCHAR(255) NOT NULL COMMENT 'Tenant-local Agent identity',
  version VARCHAR(255) NOT NULL COMMENT 'Active immutable version',
  content_hash VARCHAR(80) NOT NULL COMMENT 'Active version content hash',
  revision BIGINT NOT NULL COMMENT 'Release pointer optimistic-lock revision',
  created_at_ms BIGINT NOT NULL COMMENT 'Pointer creation time in Unix milliseconds',
  updated_at_ms BIGINT NOT NULL COMMENT 'Pointer update time in Unix milliseconds',
  updated_by VARCHAR(255) NOT NULL COMMENT 'Last promoting principal',
  identity_digest BINARY(32) NOT NULL COMMENT 'tenant_id, environment and agent_id tuple digest',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_agent_config_release_identity (identity_digest),
  KEY idx_agent_config_release_tenant_environment (tenant_id(64), environment, updated_at_ms DESC, agent_id(64)),
  CONSTRAINT chk_agent_config_release_environment CHECK (environment IN ('local', 'testing', 'staging', 'production')),
  CONSTRAINT chk_agent_config_release_revision CHECK (revision >= 1),
  CONSTRAINT chk_agent_config_release_created_at CHECK (created_at_ms >= 0),
  CONSTRAINT chk_agent_config_release_updated_at CHECK (updated_at_ms >= created_at_ms)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Tenant Agent environment release pointers';

CREATE TABLE IF NOT EXISTS agent_config_release_events (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'InnoDB clustered primary key',
  event_id VARCHAR(255) NOT NULL COMMENT 'Release audit event identity',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'Trusted tenant identity',
  environment VARCHAR(32) NOT NULL COMMENT 'Release environment',
  agent_id VARCHAR(255) NOT NULL COMMENT 'Tenant-local Agent identity',
  from_version VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'Previous active version',
  to_version VARCHAR(255) NOT NULL COMMENT 'New active version',
  content_hash VARCHAR(80) NOT NULL COMMENT 'New version content hash',
  release_revision BIGINT NOT NULL COMMENT 'Resulting release revision',
  actor VARCHAR(255) NOT NULL COMMENT 'Promoting principal',
  reason VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'Operator-supplied release reason',
  occurred_at_ms BIGINT NOT NULL COMMENT 'Event time in Unix milliseconds',
  event_id_digest BINARY(32) NOT NULL COMMENT 'event_id digest',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_agent_config_release_event_id (event_id_digest),
  KEY idx_agent_config_release_event_scope_time (tenant_id(48), environment, agent_id(48), occurred_at_ms DESC, event_id(48)),
  CONSTRAINT chk_agent_config_release_event_environment CHECK (environment IN ('local', 'testing', 'staging', 'production')),
  CONSTRAINT chk_agent_config_release_event_revision CHECK (release_revision >= 1),
  CONSTRAINT chk_agent_config_release_event_occurred_at CHECK (occurred_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Tenant Agent release audit events';
