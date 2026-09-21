package skill

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type SQLRepository struct {
	db       *sql.DB
	packages PackageObjectStore
}

func NewSQLRepository(db *sql.DB, packages PackageObjectStore) *SQLRepository {
	return &SQLRepository{db: db, packages: packages}
}

func (r *SQLRepository) PublishAtomic(ctx context.Context, record Record, content []byte) (Record, bool, error) {
	return r.PublishAtomicWithFiles(ctx, record, content, defaultFileRecords(content))
}

func (r *SQLRepository) PublishAtomicWithFiles(ctx context.Context, record Record, content []byte, files []FileRecord) (Record, bool, error) {
	if r == nil || r.db == nil {
		return Record{}, false, errors.New("skill sql repository database is required")
	}
	if r.packages == nil {
		return Record{}, false, errors.New("skill package object store is required")
	}
	archive, err := buildPackageArchive(record.Definition.ID, files)
	if err != nil {
		return Record{}, false, err
	}
	packageRef, err := r.packages.PutPackage(ctx, PackageObject{
		TenantID: record.Definition.TenantID, SkillID: record.Definition.ID, Version: record.Definition.Version, Archive: archive,
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("persist skill package attachment: %w", err)
	}
	if packageRef == "" {
		return Record{}, false, errors.New("persist skill package attachment: empty artifact ref")
	}
	definition, err := json.Marshal(record.Definition)
	if err != nil {
		return Record{}, false, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.ExecContext(ctx, `INSERT INTO skill_versions (
tenant_id, skill_id, version, definition_json, content_hash, content_size, content, published_at_ms, identity_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Definition.TenantID, record.Definition.ID, record.Definition.Version,
		definition, record.ContentHash, record.ContentSize, []byte{}, record.PublishedAt.UnixMilli(), skillIdentityDigest(record.Definition.TenantID, record.Definition.ID, record.Definition.Version))
	if err == nil {
		if err := insertSkillFiles(ctx, tx, record, packageRef, files); err != nil {
			return Record{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return Record{}, false, err
		}
		committed = true
		return cloneRecord(record), true, nil
	}
	_ = tx.Rollback()
	committed = true
	existing, getErr := r.Get(ctx, record.Definition.TenantID, record.Definition.ID, record.Definition.Version)
	if getErr != nil {
		return Record{}, false, fmt.Errorf("publish skill version: %w", err)
	}
	existingFiles, filesErr := r.ListFiles(ctx, record.Definition.TenantID, record.Definition.ID, record.Definition.Version)
	if filesErr != nil {
		return Record{}, false, filesErr
	}
	if existing.Definition.TenantID != record.Definition.TenantID || existing.ContentHash != record.ContentHash || fileSetDigest(existingFiles) != fileSetDigest(files) {
		return Record{}, false, fmt.Errorf("%w: %s@%s", ErrVersionConflict, record.Definition.ID, record.Definition.Version)
	}
	return existing, false, nil
}

func (r *SQLRepository) Get(ctx context.Context, tenantID, skillID, version string) (Record, error) {
	if r == nil || r.db == nil {
		return Record{}, errors.New("skill sql repository database is required")
	}
	if record, err := r.getExact(ctx, tenantID, skillID, version); err == nil {
		return record, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Record{}, err
	}
	record, err := r.getExact(ctx, "", skillID, version)
	if err != nil || record.Definition.Policy.Scope != ScopeGlobal {
		return Record{}, ErrNotFound
	}
	return record, nil
}

func (r *SQLRepository) getExact(ctx context.Context, tenantID, skillID, version string) (Record, error) {
	row := r.db.QueryRowContext(ctx, `SELECT definition_json, content_hash, content_size, published_at_ms
FROM skill_versions WHERE identity_digest=? AND tenant_id=? AND skill_id=? AND version=?`,
		skillIdentityDigest(tenantID, skillID, version), tenantID, skillID, version)
	var record Record
	var definition []byte
	var publishedAtMS int64
	if err := row.Scan(&definition, &record.ContentHash, &record.ContentSize, &publishedAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, err
	}
	if err := json.Unmarshal(definition, &record.Definition); err != nil {
		return Record{}, fmt.Errorf("decode skill definition: %w", err)
	}
	record.PublishedAt = time.UnixMilli(publishedAtMS).UTC()
	return record, nil
}

func (r *SQLRepository) LoadContent(ctx context.Context, hash string) ([]byte, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("skill sql repository database is required")
	}
	var tenantID, artifactRef, filePath, fileHash string
	err := r.db.QueryRowContext(ctx, `SELECT tenant_id, artifact_ref, file_path, content_hash
FROM skill_package_files WHERE file_path='SKILL.md' AND content_hash=? LIMIT 1`, hash).Scan(&tenantID, &artifactRef, &filePath, &fileHash)
	if err == nil {
		return r.loadPackageFile(ctx, tenantID, artifactRef, filePath, fileHash)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var content []byte
	if err := r.db.QueryRowContext(ctx, `SELECT content FROM skill_versions WHERE content_hash=? AND length(content)>0 LIMIT 1`, hash).Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return append([]byte(nil), content...), nil
}

func (r *SQLRepository) ListFiles(ctx context.Context, tenantID, skillID, version string) ([]FileRecord, error) {
	record, err := r.Get(ctx, tenantID, skillID, version)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT file_path, mime_type, content_hash, content_size, previewable, artifact_ref
FROM skill_package_files WHERE identity_digest=? AND tenant_id=? AND skill_id=? AND version=?
ORDER BY CASE WHEN file_path='SKILL.md' THEN 0 ELSE 1 END, file_path`,
		skillIdentityDigest(record.Definition.TenantID, record.Definition.ID, record.Definition.Version), record.Definition.TenantID, record.Definition.ID, record.Definition.Version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FileRecord
	for rows.Next() {
		var file FileRecord
		if err := rows.Scan(&file.Path, &file.MimeType, &file.ContentHash, &file.SizeBytes, &file.Previewable, &file.ArtifactRef); err != nil {
			return nil, err
		}
		result = append(result, file)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return defaultFileRecordsFromRecord(record), nil
	}
	return result, nil
}

func (r *SQLRepository) LoadFile(ctx context.Context, tenantID, skillID, version, filePath string) (FileRecord, error) {
	record, err := r.Get(ctx, tenantID, skillID, version)
	if err != nil {
		return FileRecord{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT file_path, mime_type, content_hash, content_size, previewable, artifact_ref
FROM skill_package_files WHERE identity_digest=? AND tenant_id=? AND skill_id=? AND version=? AND file_path=?`,
		skillIdentityDigest(record.Definition.TenantID, record.Definition.ID, record.Definition.Version), record.Definition.TenantID, record.Definition.ID, record.Definition.Version, filePath)
	var file FileRecord
	if err := row.Scan(&file.Path, &file.MimeType, &file.ContentHash, &file.SizeBytes, &file.Previewable, &file.ArtifactRef); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if filePath == "SKILL.md" {
				content, err := r.LoadContent(ctx, record.ContentHash)
				if err != nil {
					return FileRecord{}, err
				}
				return defaultFileRecord("SKILL.md", content), nil
			}
			return FileRecord{}, ErrNotFound
		}
		return FileRecord{}, err
	}
	file.Content, err = r.loadPackageFile(ctx, record.Definition.TenantID, file.ArtifactRef, file.Path, file.ContentHash)
	if err != nil {
		return FileRecord{}, err
	}
	return file, nil
}

func (r *SQLRepository) loadPackageFile(ctx context.Context, tenantID, artifactRef, filePath, expectedHash string) ([]byte, error) {
	if r.packages == nil {
		return nil, errors.New("skill package object store is required")
	}
	archive, err := r.packages.GetPackage(ctx, tenantID, artifactRef)
	if err != nil {
		return nil, fmt.Errorf("load skill package attachment: %w", err)
	}
	return readPackageArchiveFile(archive, filePath, expectedHash)
}

func (r *SQLRepository) List(ctx context.Context, tenantID string) ([]Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("skill sql repository database is required")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT definition_json, content_hash, content_size, published_at_ms
FROM skill_versions AS versions
WHERE (tenant_id=? OR tenant_id='')
  AND NOT EXISTS (SELECT 1 FROM skill_retirements AS retired WHERE retired.identity_digest=versions.identity_digest)
ORDER BY skill_id, version DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Record
	for rows.Next() {
		var record Record
		var definition []byte
		var publishedAtMS int64
		if err := rows.Scan(&definition, &record.ContentHash, &record.ContentSize, &publishedAtMS); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(definition, &record.Definition); err != nil {
			return nil, err
		}
		if record.Definition.TenantID == "" && record.Definition.Policy.Scope != ScopeGlobal {
			continue
		}
		record.PublishedAt = time.UnixMilli(publishedAtMS).UTC()
		result = append(result, record)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Definition.ID < result[j].Definition.ID })
	return result, rows.Err()
}

func (r *SQLRepository) Retire(ctx context.Context, retirement Retirement) (Retirement, bool, error) {
	if r == nil || r.db == nil {
		return Retirement{}, false, errors.New("skill sql repository database is required")
	}
	if _, err := r.getExact(ctx, retirement.TenantID, retirement.SkillID, retirement.Version); err != nil {
		return Retirement{}, false, err
	}
	digest := skillIdentityDigest(retirement.TenantID, retirement.SkillID, retirement.Version)
	_, insertErr := r.db.ExecContext(ctx, `INSERT INTO skill_retirements (
tenant_id, skill_id, version, retired_at_ms, identity_digest
) VALUES (?, ?, ?, ?, ?)`, retirement.TenantID, retirement.SkillID, retirement.Version, retirement.RetiredAt.UnixMilli(), digest)
	if insertErr == nil {
		return retirement, true, nil
	}
	existing, getErr := r.getRetirement(ctx, retirement.TenantID, retirement.SkillID, retirement.Version)
	if getErr == nil {
		return existing, false, nil
	}
	return Retirement{}, false, fmt.Errorf("retire skill version: %w", insertErr)
}

func (r *SQLRepository) getRetirement(ctx context.Context, tenantID, skillID, version string) (Retirement, error) {
	row := r.db.QueryRowContext(ctx, `SELECT tenant_id, skill_id, version, retired_at_ms
FROM skill_retirements WHERE identity_digest=? AND tenant_id=? AND skill_id=? AND version=?`,
		skillIdentityDigest(tenantID, skillID, version), tenantID, skillID, version)
	var retirement Retirement
	var retiredAtMS int64
	if err := row.Scan(&retirement.TenantID, &retirement.SkillID, &retirement.Version, &retiredAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Retirement{}, ErrNotFound
		}
		return Retirement{}, err
	}
	retirement.RetiredAt = time.UnixMilli(retiredAtMS).UTC()
	return retirement, nil
}

func insertSkillFiles(ctx context.Context, tx *sql.Tx, record Record, artifactRef string, files []FileRecord) error {
	digest := skillIdentityDigest(record.Definition.TenantID, record.Definition.ID, record.Definition.Version)
	for _, file := range normalizeFileRecords(files, nil) {
		if file.Path == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_package_files (
tenant_id, skill_id, version, file_path, mime_type, content_hash, content_size, previewable, artifact_ref, identity_digest
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Definition.TenantID, record.Definition.ID, record.Definition.Version, file.Path, file.MimeType, file.ContentHash, file.SizeBytes, file.Previewable, artifactRef, digest); err != nil {
			return fmt.Errorf("insert skill package file %s: %w", file.Path, err)
		}
	}
	return nil
}

func skillIdentityDigest(values ...string) []byte {
	h := sha256.New()
	for _, value := range values {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(value))
	}
	return h.Sum(nil)
}

var _ Repository = (*SQLRepository)(nil)
var _ RetirementRepository = (*SQLRepository)(nil)
var _ FileRepository = (*SQLRepository)(nil)
