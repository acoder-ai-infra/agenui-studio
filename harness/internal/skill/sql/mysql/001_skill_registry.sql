CREATE TABLE IF NOT EXISTS skill_versions (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(255) NOT NULL COMMENT 'Empty only for global skills',
  skill_id VARCHAR(128) NOT NULL,
  version VARCHAR(64) NOT NULL,
  definition_json JSON NOT NULL,
  content_hash VARCHAR(80) NOT NULL,
  content_size BIGINT NOT NULL,
  content MEDIUMBLOB NOT NULL,
  published_at_ms BIGINT NOT NULL,
  identity_digest BINARY(32) NOT NULL,
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_skill_version_identity (identity_digest),
  KEY idx_skill_version_tenant_id (tenant_id(64), skill_id(64), published_at_ms DESC),
  CONSTRAINT chk_skill_version_size CHECK (content_size > 0 AND content_size <= 1048576),
  CONSTRAINT chk_skill_version_published_at CHECK (published_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Immutable tenant and global Skill versions';
