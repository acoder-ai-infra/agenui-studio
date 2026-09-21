CREATE TABLE IF NOT EXISTS artifact_metadata_key_lock (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'id',
    key_kind TINYINT NOT NULL COMMENT '1 idempotency key ；2 artifact ref ；3 artifact ID',
    key_hash BINARY(32) NOT NULL COMMENT '原始值的 SHA-256',
    PRIMARY KEY (id),
    UNIQUE KEY uk_kind_hash (key_kind, key_hash)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COMMENT='lock';
-- harness:artifact-mysql-schema-statement --
CREATE TABLE IF NOT EXISTS artifact_metadata (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '主键id',

    artifact_id LONGBLOB NOT NULL COMMENT 'artifact_id',
    artifact_id_hash BINARY(32) NOT NULL COMMENT 'artifact_id_hash',
    artifact_ref LONGBLOB NOT NULL COMMENT 'artifact_ref',
    artifact_ref_hash BINARY(32) NOT NULL COMMENT 'artifact_ref_hash',
    idempotency_key LONGBLOB NULL COMMENT 'idempotency_key',
    idempotency_hash BINARY(32) NULL COMMENT 'idempotency_hash',

    tenant_id LONGBLOB NOT NULL COMMENT 'tenant_id',
    tenant_id_hash BINARY(32) NOT NULL COMMENT 'tenant_id_hash',
    user_id LONGBLOB NOT NULL COMMENT 'user_id',
    session_id LONGBLOB NOT NULL COMMENT 'session_id',
    session_id_hash BINARY(32) NOT NULL COMMENT 'session_id_hash',
    run_id LONGBLOB NOT NULL COMMENT 'run_id',
    run_id_hash BINARY(32) NOT NULL COMMENT 'run_id_hash',
    step_id LONGBLOB NOT NULL COMMENT 'step_id',

    owner_module LONGBLOB NOT NULL COMMENT 'owner_module',
    owner_module_hash BINARY(32) NOT NULL COMMENT 'owner_module_hash',
    owner_id LONGBLOB NOT NULL COMMENT 'owner_id',
    owner_id_hash BINARY(32) NOT NULL COMMENT 'owner_id_hash',
    artifact_type LONGBLOB NOT NULL COMMENT 'artifact_type',
    artifact_type_hash BINARY(32) NOT NULL COMMENT 'artifact_type_hash',
    mime_type LONGBLOB NOT NULL COMMENT 'mime_type',
    name LONGBLOB NOT NULL COMMENT 'name',
    size_bytes BIGINT NOT NULL COMMENT 'size_bytes',
    artifact_hash LONGBLOB NOT NULL COMMENT 'artifact_hash',
    visibility_hash BINARY(32) NOT NULL COMMENT 'visibility_hash',
    visibility LONGBLOB NOT NULL COMMENT 'visibility',
    storage_backend LONGBLOB NOT NULL COMMENT 'storage_backend',
    storage_key LONGBLOB NOT NULL COMMENT 'storage_key',

    preview_payload LONGBLOB NOT NULL COMMENT 'preview_payload',
    retention_policy LONGBLOB NOT NULL COMMENT 'retention_policy',
    expires_at_sec BIGINT NULL COMMENT 'expires_at_sec',
    expires_at_nano INT UNSIGNED NULL COMMENT 'expires_at_nano',
    created_by LONGBLOB NOT NULL COMMENT 'created_by',
    created_at_sec BIGINT NOT NULL COMMENT 'created_at_sec',
    created_at_nano INT UNSIGNED NOT NULL COMMENT 'created_at_nano',
    status LONGBLOB NOT NULL COMMENT 'status',
    derived_from_payload LONGBLOB NULL COMMENT 'derived_from_payload',
    schema_version LONGBLOB NOT NULL COMMENT 'schema_version',
    deleted_at_sec BIGINT NULL COMMENT 'deleted_at_sec',
    deleted_at_nano INT UNSIGNED NULL COMMENT 'deleted_at_nano',
    delete_reason LONGBLOB NOT NULL COMMENT 'delete_reason',
    purge_status VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'purge_status',
    purged_at_sec BIGINT NULL COMMENT 'purged_at_sec',
    purged_at_nano INT UNSIGNED NULL COMMENT 'purged_at_nano',
    metadata_payload LONGBLOB NULL COMMENT 'metadata_payload',

    PRIMARY KEY (id),
    KEY idx_artifact_id_hash (artifact_id_hash),
    KEY idx_artifact_ref_hash (artifact_ref_hash),
    KEY idx_idempotency_hash (idempotency_hash),
    KEY idx_tenant_created (tenant_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_session_created (tenant_id_hash, session_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_run_created (tenant_id_hash, run_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_owner_created (tenant_id_hash, owner_module_hash, owner_id_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_type_created (tenant_id_hash, artifact_type_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_visibility_created (tenant_id_hash, visibility_hash, created_at_sec, created_at_nano),
    KEY idx_tenant_expiry (tenant_id_hash, expires_at_sec, expires_at_nano)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COMMENT='metadata';
-- harness:artifact-mysql-schema-statement --
CREATE TABLE IF NOT EXISTS artifact_purge_job (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'id',
    metadata_id BIGINT UNSIGNED NOT NULL COMMENT 'metadata id',
    status VARBINARY(16) NOT NULL COMMENT '状态',
    attempts BIGINT UNSIGNED NOT NULL COMMENT 'attempts',
    last_error LONGBLOB NOT NULL COMMENT 'errorinfo',
    created_at_sec BIGINT NOT NULL COMMENT 'create',
    created_at_nano INT UNSIGNED NOT NULL COMMENT 'create',
    updated_at_sec BIGINT NOT NULL COMMENT 'udpate',
    updated_at_nano INT UNSIGNED NOT NULL COMMENT 'update',
    lease_owner LONGBLOB NOT NULL COMMENT 'owner',
    lease_until_sec BIGINT NULL COMMENT 'lease',
    lease_until_nano INT UNSIGNED NULL COMMENT 'lease',
    lease_version BIGINT NOT NULL COMMENT 'lease',
    PRIMARY KEY (id),
    UNIQUE KEY uk_meta_id (metadata_id),
    KEY idx_artifact_purge_ready (
        status, lease_until_sec, lease_until_nano,
        created_at_sec, created_at_nano
    )
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COMMENT='异步删除';
