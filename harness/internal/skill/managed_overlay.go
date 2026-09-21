package skill

import (
	"context"
	"errors"
	"sort"
)

type overlayRepository struct {
	managed  Repository
	fallback Repository
}

func (r overlayRepository) PublishAtomic(ctx context.Context, record Record, content []byte) (Record, bool, error) {
	return r.managed.PublishAtomic(ctx, record, content)
}

func (r overlayRepository) PublishAtomicWithFiles(ctx context.Context, record Record, content []byte, files []FileRecord) (Record, bool, error) {
	if repository, ok := r.managed.(FileRepository); ok {
		return repository.PublishAtomicWithFiles(ctx, record, content, files)
	}
	return r.managed.PublishAtomic(ctx, record, content)
}

func (r overlayRepository) Get(ctx context.Context, tenantID, skillID, version string) (Record, error) {
	record, err := r.managed.Get(ctx, tenantID, skillID, version)
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Record{}, err
	}
	return r.fallback.Get(ctx, tenantID, skillID, version)
}

func (r overlayRepository) LoadContent(ctx context.Context, contentHash string) ([]byte, error) {
	content, err := r.managed.LoadContent(ctx, contentHash)
	if err == nil {
		return content, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return r.fallback.LoadContent(ctx, contentHash)
}

func (r overlayRepository) ListFiles(ctx context.Context, tenantID, skillID, version string) ([]FileRecord, error) {
	if repository, ok := r.managed.(FileRepository); ok {
		files, err := repository.ListFiles(ctx, tenantID, skillID, version)
		if err == nil {
			return files, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if repository, ok := r.fallback.(FileRepository); ok {
		return repository.ListFiles(ctx, tenantID, skillID, version)
	}
	record, err := r.fallback.Get(ctx, tenantID, skillID, version)
	if err != nil {
		return nil, err
	}
	return defaultFileRecordsFromRecord(record), nil
}

func (r overlayRepository) LoadFile(ctx context.Context, tenantID, skillID, version, filePath string) (FileRecord, error) {
	if repository, ok := r.managed.(FileRepository); ok {
		file, err := repository.LoadFile(ctx, tenantID, skillID, version, filePath)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return FileRecord{}, err
		}
	}
	if repository, ok := r.fallback.(FileRepository); ok {
		return repository.LoadFile(ctx, tenantID, skillID, version, filePath)
	}
	if filePath != "SKILL.md" {
		return FileRecord{}, ErrNotFound
	}
	record, err := r.fallback.Get(ctx, tenantID, skillID, version)
	if err != nil {
		return FileRecord{}, err
	}
	content, err := r.fallback.LoadContent(ctx, record.ContentHash)
	if err != nil {
		return FileRecord{}, err
	}
	return defaultFileRecord("SKILL.md", content), nil
}

func (r overlayRepository) List(ctx context.Context, tenantID string) ([]Record, error) {
	managed, err := r.managed.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	fallback, err := r.fallback.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(managed))
	result := append([]Record(nil), managed...)
	for _, record := range managed {
		seen[recordKey(record.Definition.TenantID, record.Definition.ID, record.Definition.Version)] = struct{}{}
	}
	for _, record := range fallback {
		key := recordKey(record.Definition.TenantID, record.Definition.ID, record.Definition.Version)
		if _, exists := seen[key]; !exists {
			result = append(result, record)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Definition.ID < result[j].Definition.ID })
	return result, nil
}

func (r overlayRepository) Retire(ctx context.Context, retirement Retirement) (Retirement, bool, error) {
	repository, ok := r.managed.(RetirementRepository)
	if !ok {
		return Retirement{}, false, ErrLifecycleUnsupported
	}
	return repository.Retire(ctx, retirement)
}

func NewManagedOverlay(base *Service, managed Repository) (*Service, error) {
	if base == nil || managed == nil {
		return nil, errors.New("skill managed overlay requires base service and repository")
	}
	return &Service{
		repository: overlayRepository{managed: managed, fallback: base.repository},
		estimator:  base.estimator, ids: base.ids, clock: base.clock, maxContentBytes: base.maxContentBytes,
	}, nil
}

var _ Repository = overlayRepository{}
var _ RetirementRepository = overlayRepository{}
var _ FileRepository = overlayRepository{}
