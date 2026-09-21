package skill

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type InMemoryRepository struct {
	mu      sync.RWMutex
	records map[string]Record
	content map[string][]byte
	files   map[string]map[string]FileRecord
	retired map[string]Retirement
}

var _ Repository = (*InMemoryRepository)(nil)

func NewInMemoryRepository() *InMemoryRepository {
	return &InMemoryRepository{records: make(map[string]Record), content: make(map[string][]byte), files: make(map[string]map[string]FileRecord), retired: make(map[string]Retirement)}
}

func (r *InMemoryRepository) PublishAtomic(_ context.Context, record Record, content []byte) (Record, bool, error) {
	return r.PublishAtomicWithFiles(context.Background(), record, content, defaultFileRecords(content))
}

func (r *InMemoryRepository) PublishAtomicWithFiles(_ context.Context, record Record, content []byte, files []FileRecord) (Record, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := recordKey(record.Definition.TenantID, record.Definition.ID, record.Definition.Version)
	if existing, ok := r.records[key]; ok {
		if existing.ContentHash != record.ContentHash || fileSetDigest(r.fileRecordsLocked(key)) != fileSetDigest(files) {
			return Record{}, false, fmt.Errorf("%w: %s@%s", ErrVersionConflict, record.Definition.ID, record.Definition.Version)
		}
		return cloneRecord(existing), false, nil
	}
	r.content[record.ContentHash] = append([]byte(nil), content...)
	r.records[key] = cloneRecord(record)
	r.files[key] = cloneFileMap(files)
	return cloneRecord(record), true, nil
}

func (r *InMemoryRepository) Get(_ context.Context, tenantID, skillID, version string) (Record, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if record, ok := r.records[recordKey(tenantID, skillID, version)]; ok {
		return cloneRecord(record), nil
	}
	if record, ok := r.records[recordKey("", skillID, version)]; ok && record.Definition.Policy.Scope == ScopeGlobal {
		return cloneRecord(record), nil
	}
	return Record{}, ErrNotFound
}

func (r *InMemoryRepository) LoadContent(_ context.Context, contentHash string) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	content, ok := r.content[contentHash]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), content...), nil
}

func (r *InMemoryRepository) ListFiles(_ context.Context, tenantID, skillID, version string) ([]FileRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, err := r.fileKeyLocked(tenantID, skillID, version)
	if err != nil {
		return nil, err
	}
	return r.fileRecordsLocked(key), nil
}

func (r *InMemoryRepository) LoadFile(_ context.Context, tenantID, skillID, version, filePath string) (FileRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, err := r.fileKeyLocked(tenantID, skillID, version)
	if err != nil {
		return FileRecord{}, err
	}
	files := r.files[key]
	if files == nil {
		return FileRecord{}, ErrNotFound
	}
	file, ok := files[filePath]
	if !ok {
		return FileRecord{}, ErrNotFound
	}
	return cloneFileRecord(file), nil
}

func (r *InMemoryRepository) List(_ context.Context, tenantID string) ([]Record, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Record, 0)
	for _, record := range r.records {
		if _, retired := r.retired[recordKey(record.Definition.TenantID, record.Definition.ID, record.Definition.Version)]; retired {
			continue
		}
		if record.Definition.TenantID == tenantID || (record.Definition.TenantID == "" && record.Definition.Policy.Scope == ScopeGlobal) {
			result = append(result, cloneRecord(record))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Definition.ID == result[j].Definition.ID {
			return result[i].Definition.Version > result[j].Definition.Version
		}
		return result[i].Definition.ID < result[j].Definition.ID
	})
	return result, nil
}

func (r *InMemoryRepository) Retire(_ context.Context, retirement Retirement) (Retirement, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := recordKey(retirement.TenantID, retirement.SkillID, retirement.Version)
	if _, exists := r.records[key]; !exists {
		return Retirement{}, false, ErrNotFound
	}
	if existing, exists := r.retired[key]; exists {
		return existing, false, nil
	}
	r.retired[key] = retirement
	return retirement, true, nil
}

func (r *InMemoryRepository) fileKeyLocked(tenantID, skillID, version string) (string, error) {
	key := recordKey(tenantID, skillID, version)
	if _, ok := r.records[key]; ok {
		return key, nil
	}
	globalKey := recordKey("", skillID, version)
	if record, ok := r.records[globalKey]; ok && record.Definition.Policy.Scope == ScopeGlobal {
		return globalKey, nil
	}
	return "", ErrNotFound
}

func (r *InMemoryRepository) fileRecordsLocked(key string) []FileRecord {
	files := r.files[key]
	result := make([]FileRecord, 0, len(files))
	for _, file := range files {
		result = append(result, cloneFileRecord(file))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path == "SKILL.md" {
			return true
		}
		if result[j].Path == "SKILL.md" {
			return false
		}
		return result[i].Path < result[j].Path
	})
	return result
}

func recordKey(tenantID, skillID, version string) string {
	return tenantID + "\x00" + skillID + "\x00" + version
}

func cloneFileMap(files []FileRecord) map[string]FileRecord {
	result := make(map[string]FileRecord, len(files))
	for _, file := range files {
		if strings.TrimSpace(file.Path) == "" {
			continue
		}
		result[file.Path] = cloneFileRecord(file)
	}
	return result
}

func cloneFileRecord(file FileRecord) FileRecord {
	file.Content = append([]byte(nil), file.Content...)
	return file
}

func cloneRecord(record Record) Record {
	record.Definition.Policy.AllowedAgents = append([]string(nil), record.Definition.Policy.AllowedAgents...)
	record.Definition.Dependencies.Tools = append([]string(nil), record.Definition.Dependencies.Tools...)
	record.Definition.Dependencies.MCPServers = append([]string(nil), record.Definition.Dependencies.MCPServers...)
	record.Definition.Dependencies.Skills = append([]Ref(nil), record.Definition.Dependencies.Skills...)
	return record
}

var _ RetirementRepository = (*InMemoryRepository)(nil)
var _ FileRepository = (*InMemoryRepository)(nil)
