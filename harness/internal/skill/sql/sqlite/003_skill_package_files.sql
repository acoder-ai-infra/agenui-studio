CREATE TABLE IF NOT EXISTS skill_package_files (
  tenant_id TEXT NOT NULL,
  skill_id TEXT NOT NULL,
  version TEXT NOT NULL,
  file_path TEXT NOT NULL,
  mime_type TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  content_size INTEGER NOT NULL CHECK (content_size >= 0 AND content_size <= 26214400),
  previewable INTEGER NOT NULL CHECK (previewable IN (0, 1)),
  artifact_ref TEXT NOT NULL,
  identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32),
  PRIMARY KEY (tenant_id, skill_id, version, file_path),
  FOREIGN KEY (tenant_id, skill_id, version) REFERENCES skill_versions (tenant_id, skill_id, version)
);

CREATE INDEX IF NOT EXISTS skill_package_files_identity_idx
  ON skill_package_files (identity_digest, file_path);
