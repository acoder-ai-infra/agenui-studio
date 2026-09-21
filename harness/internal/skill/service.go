package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

const DefaultMaxContentBytes int64 = 1 << 20

type ServiceOptions struct {
	MaxContentBytes int64
	Clock           func() time.Time
}

type Service struct {
	repository      Repository
	estimator       TokenEstimator
	ids             func() string
	clock           func() time.Time
	maxContentBytes int64
}

func NewService(repository Repository, estimator TokenEstimator, ids func() string) (*Service, error) {
	return NewServiceWithOptions(repository, estimator, ids, ServiceOptions{})
}

func NewServiceWithOptions(repository Repository, estimator TokenEstimator, ids func() string, options ServiceOptions) (*Service, error) {
	if repository == nil || ids == nil {
		return nil, fmt.Errorf("skill configuration: repository and ids are required")
	}
	if estimator == nil {
		estimator = ConservativeEstimator{}
	}
	if options.MaxContentBytes <= 0 {
		options.MaxContentBytes = DefaultMaxContentBytes
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Service{
		repository: repository, estimator: estimator, ids: ids,
		clock: options.Clock, maxContentBytes: options.MaxContentBytes,
	}, nil
}

func (s *Service) Publish(ctx context.Context, principal Principal, definition Definition, content []byte) (Record, bool, error) {
	return s.publish(ctx, principal, definition, content, defaultFileRecords(content))
}

func (s *Service) PublishPackage(ctx context.Context, principal Principal, definition Definition, content []byte, files []FileRecord) (Record, bool, error) {
	return s.publish(ctx, principal, definition, content, normalizeFileRecords(files, content))
}

func (s *Service) publish(ctx context.Context, principal Principal, definition Definition, content []byte, files []FileRecord) (Record, bool, error) {
	if err := principal.Validate(); err != nil {
		return Record{}, false, err
	}
	if err := validateDefinition(definition); err != nil {
		return Record{}, false, err
	}
	if len(content) == 0 {
		return Record{}, false, fmt.Errorf("%w: empty content", ErrInvalidDefinition)
	}
	if int64(len(content)) > s.maxContentBytes {
		return Record{}, false, fmt.Errorf("%w: content bytes=%d max=%d", ErrInvalidDefinition, len(content), s.maxContentBytes)
	}
	if definition.Policy.Scope == ScopeGlobal {
		if !principal.System || definition.TenantID != "" {
			return Record{}, false, ErrPermissionDenied
		}
	} else if principal.System {
		if definition.TenantID == "" {
			return Record{}, false, ErrPermissionDenied
		}
	} else if definition.TenantID != principal.TenantID {
		return Record{}, false, ErrPermissionDenied
	}
	tokens, err := s.estimator.Estimate(ctx, string(content))
	if err != nil {
		return Record{}, false, err
	}
	if definition.Policy.MaxInputTokens > 0 && tokens > definition.Policy.MaxInputTokens {
		return Record{}, false, fmt.Errorf("%w: content tokens=%d max=%d", ErrInvalidDefinition, tokens, definition.Policy.MaxInputTokens)
	}
	record := Record{
		Definition:  cloneRecord(Record{Definition: definition}).Definition,
		ContentHash: contentHash(content),
		ContentSize: int64(len(content)),
		PublishedAt: s.clock(),
	}
	if repository, ok := s.repository.(FileRepository); ok {
		return repository.PublishAtomicWithFiles(ctx, record, content, files)
	}
	return s.repository.PublishAtomic(ctx, record, content)
}

func (s *Service) Resolve(ctx context.Context, principal Principal, ref Ref) (Resolution, error) {
	if err := principal.Validate(); err != nil {
		return Resolution{}, err
	}
	seen := make(map[string]bool)
	resolved := make(map[string]ResolvedDependency)
	var dependencies []ResolvedDependency
	root, instructions, err := s.resolve(ctx, principal, ref, seen, resolved, &dependencies, true)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Root: root, Dependencies: dependencies, Instructions: instructions}, nil
}

func (s *Service) List(ctx context.Context, principal Principal) ([]Record, error) {
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	return s.repository.List(ctx, principal.TenantID)
}

func (s *Service) ListFiles(ctx context.Context, principal Principal, ref Ref) (FileList, error) {
	record, err := s.previewRecord(ctx, principal, ref)
	if err != nil {
		return FileList{}, err
	}
	var files []FileRecord
	if repository, ok := s.repository.(FileRepository); ok {
		files, err = repository.ListFiles(ctx, record.Definition.TenantID, record.Definition.ID, record.Definition.Version)
		if err != nil {
			return FileList{}, err
		}
	} else {
		files = defaultFileRecordsFromRecord(record)
	}
	for index := range files {
		files[index].Content = nil
	}
	return FileList{SkillID: record.Definition.ID, Version: record.Definition.Version, Files: files}, nil
}

func (s *Service) PreviewFile(ctx context.Context, principal Principal, ref Ref, requestedPath string) (FilePreview, error) {
	record, err := s.previewRecord(ctx, principal, ref)
	if err != nil {
		return FilePreview{}, err
	}
	filePath, err := normalizePreviewPath(requestedPath)
	if err != nil {
		return FilePreview{}, err
	}
	var file FileRecord
	if repository, ok := s.repository.(FileRepository); ok {
		file, err = repository.LoadFile(ctx, record.Definition.TenantID, record.Definition.ID, record.Definition.Version, filePath)
		if err != nil {
			return FilePreview{}, err
		}
	} else if filePath == "SKILL.md" {
		content, err := s.repository.LoadContent(ctx, record.ContentHash)
		if err != nil {
			return FilePreview{}, err
		}
		file = defaultFileRecord("SKILL.md", content)
	} else {
		return FilePreview{}, fmt.Errorf("%w: skill package asset is not persisted: %s", ErrNotFound, filePath)
	}
	if !file.Previewable {
		return FilePreview{}, fmt.Errorf("%w: skill file is not previewable: %s", ErrInvalidDefinition, filePath)
	}
	if contentHash(file.Content) != file.ContentHash {
		return FilePreview{}, fmt.Errorf("%w: %s@%s/%s", ErrContentIntegrity, ref.ID, ref.Version, filePath)
	}
	return FilePreview{
		SkillID:     record.Definition.ID,
		Version:     record.Definition.Version,
		Path:        file.Path,
		MimeType:    file.MimeType,
		SizeBytes:   file.SizeBytes,
		ContentHash: file.ContentHash,
		Content:     string(file.Content),
	}, nil
}

func (s *Service) previewRecord(ctx context.Context, principal Principal, ref Ref) (Record, error) {
	if err := principal.Validate(); err != nil {
		return Record{}, err
	}
	if !safeID.MatchString(ref.ID) || !semver.IsValid("v"+ref.Version) {
		return Record{}, fmt.Errorf("%w: invalid skill reference", ErrInvalidDefinition)
	}
	record, err := s.repository.Get(ctx, principal.TenantID, ref.ID, ref.Version)
	if err != nil {
		return Record{}, err
	}
	if err := authorize(principal, record.Definition); err != nil {
		return Record{}, err
	}
	return record, nil
}

func ValidateUpdate(base Ref, definition Definition) error {
	if !safeID.MatchString(base.ID) || !semver.IsValid("v"+base.Version) {
		return fmt.Errorf("%w: invalid base skill reference", ErrInvalidDefinition)
	}
	if definition.ID != base.ID {
		return fmt.Errorf("%w: update skill id must remain %s", ErrInvalidDefinition, base.ID)
	}
	if !semver.IsValid("v"+definition.Version) || semver.Compare("v"+definition.Version, "v"+base.Version) <= 0 {
		return fmt.Errorf("%w: update version must be newer than %s", ErrInvalidDefinition, base.Version)
	}
	return nil
}

func (s *Service) PublishUpdate(ctx context.Context, principal Principal, base Ref, definition Definition, content []byte) (Record, bool, error) {
	return s.publishUpdate(ctx, principal, base, definition, content, defaultFileRecords(content))
}

func (s *Service) PublishPackageUpdate(ctx context.Context, principal Principal, base Ref, definition Definition, content []byte, files []FileRecord) (Record, bool, error) {
	return s.publishUpdate(ctx, principal, base, definition, content, normalizeFileRecords(files, content))
}

func (s *Service) publishUpdate(ctx context.Context, principal Principal, base Ref, definition Definition, content []byte, files []FileRecord) (Record, bool, error) {
	if err := principal.Validate(); err != nil {
		return Record{}, false, err
	}
	if err := ValidateUpdate(base, definition); err != nil {
		return Record{}, false, err
	}
	baseRecord, err := s.repository.Get(ctx, principal.TenantID, base.ID, base.Version)
	if err != nil {
		return Record{}, false, err
	}
	if baseRecord.Definition.Policy.Scope != ScopeTenant || baseRecord.Definition.TenantID != principal.TenantID {
		return Record{}, false, ErrPermissionDenied
	}
	return s.publish(ctx, principal, definition, content, files)
}

func (s *Service) Retire(ctx context.Context, principal Principal, ref Ref) (Retirement, bool, error) {
	if err := principal.Validate(); err != nil {
		return Retirement{}, false, err
	}
	if !safeID.MatchString(ref.ID) || !semver.IsValid("v"+ref.Version) {
		return Retirement{}, false, fmt.Errorf("%w: invalid skill reference", ErrInvalidDefinition)
	}
	record, err := s.repository.Get(ctx, principal.TenantID, ref.ID, ref.Version)
	if err != nil {
		return Retirement{}, false, err
	}
	if record.Definition.Policy.Scope != ScopeTenant || record.Definition.TenantID != principal.TenantID {
		return Retirement{}, false, ErrPermissionDenied
	}
	repository, ok := s.repository.(RetirementRepository)
	if !ok {
		return Retirement{}, false, ErrLifecycleUnsupported
	}
	return repository.Retire(ctx, Retirement{
		TenantID: principal.TenantID, SkillID: ref.ID, Version: ref.Version, RetiredAt: s.clock(),
	})
}

func (s *Service) resolve(ctx context.Context, principal Principal, ref Ref, path map[string]bool, resolved map[string]ResolvedDependency, dependencies *[]ResolvedDependency, root bool) (Snapshot, string, error) {
	key := ref.ID + "@" + ref.Version
	if path[key] {
		return Snapshot{}, "", fmt.Errorf("%w: %s", ErrDependencyCycle, key)
	}
	if !root {
		if dependency, ok := resolved[key]; ok {
			return dependency.Snapshot, dependency.Instructions, nil
		}
	}
	path[key] = true
	defer delete(path, key)
	record, err := s.repository.Get(ctx, principal.TenantID, ref.ID, ref.Version)
	if err != nil {
		return Snapshot{}, "", err
	}
	if err := authorize(principal, record.Definition); err != nil {
		return Snapshot{}, "", err
	}
	content, err := s.repository.LoadContent(ctx, record.ContentHash)
	if err != nil {
		return Snapshot{}, "", err
	}
	if contentHash(content) != record.ContentHash {
		return Snapshot{}, "", fmt.Errorf("%w: %s", ErrContentIntegrity, key)
	}
	tokens, err := s.estimator.Estimate(ctx, string(content))
	if err != nil {
		return Snapshot{}, "", err
	}
	snapshotID := s.ids()
	if snapshotID == "" {
		return Snapshot{}, "", fmt.Errorf("skill configuration: snapshot id is required")
	}
	snapshot := Snapshot{
		ID:                snapshotID,
		SkillID:           record.Definition.ID,
		Version:           record.Definition.Version,
		TenantID:          record.Definition.TenantID,
		ContentHash:       record.ContentHash,
		ContentSize:       record.ContentSize,
		EstimatedTokens:   tokens,
		InjectionStrategy: record.Definition.InjectionStrategy,
		Dependencies:      cloneRecord(record).Definition.Dependencies,
		ResolvedAt:        s.clock(),
	}
	for _, dependency := range record.Definition.Dependencies.Skills {
		_, _, err := s.resolve(ctx, principal, dependency, path, resolved, dependencies, false)
		if err != nil {
			return Snapshot{}, "", err
		}
	}
	if !root {
		dependency := ResolvedDependency{Snapshot: snapshot, Instructions: string(content)}
		resolved[key] = dependency
		*dependencies = append(*dependencies, dependency)
	}
	return snapshot, string(content), nil
}

func authorize(principal Principal, definition Definition) error {
	if principal.System {
		return nil
	}
	if definition.Policy.Scope == ScopeTenant && definition.TenantID != principal.TenantID {
		return ErrPermissionDenied
	}
	if len(definition.Policy.AllowedAgents) > 0 && !contains(definition.Policy.AllowedAgents, principal.AgentID) {
		return ErrPermissionDenied
	}
	return nil
}

func validateDefinition(definition Definition) error {
	if !safeID.MatchString(definition.ID) || !semver.IsValid("v"+definition.Version) {
		return fmt.Errorf("%w: invalid id or semantic version", ErrInvalidDefinition)
	}
	if definition.Policy.Scope != ScopeTenant && definition.Policy.Scope != ScopeGlobal {
		return fmt.Errorf("%w: invalid scope", ErrInvalidDefinition)
	}
	if definition.Policy.Scope == ScopeTenant && definition.TenantID == "" {
		return fmt.Errorf("%w: tenant scope requires tenant id", ErrInvalidDefinition)
	}
	switch definition.InjectionStrategy {
	case InjectSystem, InjectOnDemand, InjectToolOnly:
	default:
		return fmt.Errorf("%w: invalid injection strategy", ErrInvalidDefinition)
	}
	for _, dependency := range definition.Dependencies.Skills {
		if !safeID.MatchString(dependency.ID) || !semver.IsValid("v"+dependency.Version) {
			return fmt.Errorf("%w: invalid skill dependency", ErrInvalidDefinition)
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func normalizePreviewPath(requestedPath string) (string, error) {
	filePath := strings.TrimSpace(requestedPath)
	if filePath == "" {
		filePath = "SKILL.md"
	}
	if strings.Contains(filePath, "\x00") || strings.Contains(filePath, "\\") || strings.HasPrefix(filePath, "/") {
		return "", fmt.Errorf("%w: invalid skill file path", ErrInvalidDefinition)
	}
	for _, segment := range strings.Split(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: invalid skill file path", ErrInvalidDefinition)
		}
	}
	return filePath, nil
}

func normalizeFileRecords(files []FileRecord, content []byte) []FileRecord {
	if len(files) == 0 {
		return defaultFileRecords(content)
	}
	result := make([]FileRecord, 0, len(files))
	for _, file := range files {
		if strings.TrimSpace(file.Path) == "" {
			continue
		}
		copied := cloneFileRecord(file)
		if copied.MimeType == "" {
			copied.MimeType = packageFileMimeType(copied.Path)
		}
		copied.SizeBytes = int64(len(copied.Content))
		copied.ContentHash = contentHash(copied.Content)
		result = append(result, copied)
	}
	if !hasFileRecord(result, "SKILL.md") {
		result = append([]FileRecord{defaultFileRecord("SKILL.md", content)}, result...)
	}
	return result
}

func defaultFileRecords(content []byte) []FileRecord {
	return []FileRecord{defaultFileRecord("SKILL.md", content)}
}

func defaultFileRecordsFromRecord(record Record) []FileRecord {
	return []FileRecord{{
		Path:        "SKILL.md",
		MimeType:    "text/markdown; charset=utf-8",
		SizeBytes:   record.ContentSize,
		ContentHash: record.ContentHash,
		Previewable: true,
	}}
}

func defaultFileRecord(filePath string, content []byte) FileRecord {
	return FileRecord{
		Path:        filePath,
		MimeType:    packageFileMimeType(filePath),
		SizeBytes:   int64(len(content)),
		ContentHash: contentHash(content),
		Previewable: true,
		Content:     append([]byte(nil), content...),
	}
}

func hasFileRecord(files []FileRecord, filePath string) bool {
	for _, file := range files {
		if file.Path == filePath {
			return true
		}
	}
	return false
}

func fileSetDigest(files []FileRecord) string {
	normalized := make([]FileRecord, 0, len(files))
	for _, file := range files {
		if strings.TrimSpace(file.Path) == "" {
			continue
		}
		normalized = append(normalized, FileRecord{Path: file.Path, ContentHash: file.ContentHash, SizeBytes: file.SizeBytes, MimeType: file.MimeType, Previewable: file.Previewable})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Path < normalized[j].Path })
	hash := sha256.New()
	for _, file := range normalized {
		_, _ = hash.Write([]byte(file.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(file.ContentHash))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func contentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type ConservativeEstimator struct{}

func (ConservativeEstimator) Estimate(_ context.Context, content string) (int, error) {
	runes := len([]rune(content))
	if runes == 0 {
		return 0, nil
	}
	return (runes + 2) / 3, nil
}
