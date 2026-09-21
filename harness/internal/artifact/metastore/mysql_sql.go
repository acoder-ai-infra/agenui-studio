package metastore

const mysqlMetadataSelectColumns = "artifact_id, artifact_ref, tenant_id, user_id, session_id, run_id, step_id, owner_module, owner_id, artifact_type, mime_type, name, size_bytes, artifact_hash, visibility, storage_backend, storage_key, preview_payload, retention_policy, expires_at_sec, expires_at_nano, created_by, created_at_sec, created_at_nano, status, derived_from_payload, schema_version, deleted_at_sec, deleted_at_nano, delete_reason, purge_status, purged_at_sec, purged_at_nano, metadata_payload"

const mysqlMetadataSelectByRef = "SELECT " + mysqlMetadataSelectColumns + " FROM artifact_metadata WHERE artifact_ref_hash = ? AND artifact_ref = ? LIMIT 2"

const mysqlMetadataSelectByID = "SELECT " + mysqlMetadataSelectColumns + " FROM artifact_metadata WHERE artifact_id_hash = ? AND artifact_id = ? LIMIT 2"

const mysqlMetadataSelectByIdempotencyKey = "SELECT " + mysqlMetadataSelectColumns + " FROM artifact_metadata WHERE idempotency_hash = ? AND idempotency_key = ? LIMIT 2"

const mysqlMetadataMarkDeletedSelect = "SELECT id, " + mysqlMetadataSelectColumns + " FROM artifact_metadata WHERE artifact_ref_hash = ? AND artifact_ref = ? LIMIT 2 FOR UPDATE"

const mysqlMetadataMarkDeletedUpdate = "UPDATE artifact_metadata SET status = ?, deleted_at_sec = ?, deleted_at_nano = ?, delete_reason = ?, purge_status = ?, purged_at_sec = NULL, purged_at_nano = NULL WHERE id = ?"

const mysqlMetadataListSelect = "SELECT " + mysqlMetadataSelectColumns + " FROM artifact_metadata"

const mysqlMetadataListOrder = " ORDER BY created_at_sec ASC, created_at_nano ASC, artifact_id ASC"

const mysqlMetadataKeyLockUpsert = "INSERT INTO artifact_metadata_key_lock (key_kind, key_hash) VALUES (?, ?) ON DUPLICATE KEY UPDATE key_hash = VALUES(key_hash)"

const mysqlMetadataKeyLockSelect = "SELECT key_hash FROM artifact_metadata_key_lock WHERE key_kind = ? AND key_hash = ? FOR UPDATE"

const mysqlMetadataInsertColumns = "artifact_id, artifact_id_hash, artifact_ref, artifact_ref_hash, idempotency_key, idempotency_hash, tenant_id, tenant_id_hash, user_id, session_id, session_id_hash, run_id, run_id_hash, step_id, owner_module, owner_module_hash, owner_id, owner_id_hash, artifact_type, artifact_type_hash, mime_type, name, size_bytes, artifact_hash, visibility, visibility_hash, storage_backend, storage_key, preview_payload, retention_policy, expires_at_sec, expires_at_nano, created_by, created_at_sec, created_at_nano, status, derived_from_payload, schema_version, deleted_at_sec, deleted_at_nano, delete_reason, purge_status, purged_at_sec, purged_at_nano, metadata_payload"

const mysqlMetadataInsert = "INSERT INTO artifact_metadata (" + mysqlMetadataInsertColumns + ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
