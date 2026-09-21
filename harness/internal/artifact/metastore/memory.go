package metastore

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

type MemoryMetadataStore struct {
	mu            sync.RWMutex
	byRef         map[string]artifact.ArtifactMeta
	byID          map[string]string
	byIdempotency map[string]string
	purgeJobs     map[string]artifact.PurgeJob
}

func NewMemoryMetadataStore() *MemoryMetadataStore {
	return &MemoryMetadataStore{
		byRef:         make(map[string]artifact.ArtifactMeta),
		byID:          make(map[string]string),
		byIdempotency: make(map[string]string),
		purgeJobs:     make(map[string]artifact.PurgeJob),
	}
}

func NewMemory() *MemoryMetadataStore {
	return NewMemoryMetadataStore()
}

func (*MemoryMetadataStore) ProductionReady() bool { return false }

func (s *MemoryMetadataStore) Create(_ context.Context, meta artifact.ArtifactMeta, idempotencyKey string) (*artifact.ArtifactMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idempotencyKey != "" {
		if ref, ok := s.byIdempotency[idempotencyKey]; ok {
			existing := s.byRef[ref]
			return cloneMeta(existing), nil
		}
	}
	if _, ok := s.byRef[meta.ArtifactRef]; ok {
		return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("artifact ref already exists: %s", meta.ArtifactRef)}
	}
	if ref, ok := s.byID[meta.ArtifactID]; ok && ref != meta.ArtifactRef {
		return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("artifact id already exists: %s", meta.ArtifactID)}
	}
	owned := cloneMeta(meta)
	s.byRef[owned.ArtifactRef] = *owned
	s.byID[owned.ArtifactID] = owned.ArtifactRef
	if idempotencyKey != "" {
		s.byIdempotency[idempotencyKey] = owned.ArtifactRef
	}
	return cloneMeta(*owned), nil
}

func (s *MemoryMetadataStore) GetByRef(_ context.Context, ref string) (*artifact.ArtifactMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.byRef[ref]
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", ref)}
	}
	return cloneMeta(meta), nil
}

func (s *MemoryMetadataStore) GetByID(ctx context.Context, artifactID string) (*artifact.ArtifactMeta, error) {
	s.mu.RLock()
	ref, ok := s.byID[artifactID]
	s.mu.RUnlock()
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", artifactID)}
	}
	return s.GetByRef(ctx, ref)
}

func (s *MemoryMetadataStore) GetByIdempotencyKey(ctx context.Context, key string) (*artifact.ArtifactMeta, error) {
	s.mu.RLock()
	ref, ok := s.byIdempotency[key]
	s.mu.RUnlock()
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "idempotency key not found"}
	}
	return s.GetByRef(ctx, ref)
}

func (s *MemoryMetadataStore) List(_ context.Context, query artifact.ListQuery) ([]artifact.ArtifactMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]artifact.ArtifactMeta, 0)
	for _, meta := range s.byRef {
		if !query.IncludeDeleted && meta.Status == artifact.ArtifactStatusDeleted {
			continue
		}
		if query.TenantID != "" && meta.TenantID != query.TenantID {
			continue
		}
		if query.SessionID != "" && meta.SessionID != query.SessionID {
			continue
		}
		if query.RunID != "" && meta.RunID != query.RunID {
			continue
		}
		if query.OwnerModule != "" && meta.OwnerModule != query.OwnerModule {
			continue
		}
		if query.OwnerID != "" && meta.OwnerID != query.OwnerID {
			continue
		}
		if query.Type != "" && meta.ArtifactType != query.Type {
			continue
		}
		if query.Visibility != "" && meta.Visibility != query.Visibility {
			continue
		}
		if !query.ExpiredAtOrBefore.IsZero() && (meta.ExpiresAt.IsZero() || meta.ExpiresAt.After(query.ExpiredAtOrBefore)) {
			continue
		}
		out = append(out, *cloneMeta(meta))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ArtifactID < out[j].ArtifactID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryMetadataStore) MarkDeleted(_ context.Context, ref string, reason artifact.DeleteReason, at time.Time) (*artifact.ArtifactMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.byRef[ref]
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", ref)}
	}
	if meta.Status == artifact.ArtifactStatusDeleted {
		return cloneMeta(meta), nil
	}
	meta.Status = artifact.ArtifactStatusDeleted
	meta.DeletedAt = at
	meta.DeleteReason = reason
	meta.PurgeStatus = artifact.PurgeStatusPending
	s.byRef[ref] = meta
	return cloneMeta(meta), nil
}

func (s *MemoryMetadataStore) RequestPurge(_ context.Context, ref string, reason artifact.DeleteReason, at time.Time) (*artifact.ArtifactMeta, *artifact.PurgeJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.byRef[ref]
	if !ok {
		return nil, nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("artifact not found: %s", ref)}
	}
	if existing, ok := s.purgeJobs[ref]; ok {
		metaCopy := cloneMeta(meta)
		jobCopy := existing
		return metaCopy, &jobCopy, nil
	}
	if meta.Status != artifact.ArtifactStatusDeleted {
		meta.Status = artifact.ArtifactStatusDeleted
		meta.DeletedAt = at
		meta.DeleteReason = reason
	}
	meta.PurgeStatus = artifact.PurgeStatusPending
	meta.PurgedAt = time.Time{}
	s.byRef[ref] = meta
	job := artifact.PurgeJob{
		ArtifactRef: ref, TenantID: meta.TenantID, SessionID: meta.SessionID, RunID: meta.RunID,
		StorageKey: meta.StorageKey, Status: artifact.PurgeStatusPending, CreatedAt: at, UpdatedAt: at,
	}
	s.purgeJobs[ref] = job
	metaCopy := cloneMeta(meta)
	jobCopy := job
	return metaCopy, &jobCopy, nil
}

func (s *MemoryMetadataStore) GetPurgeJob(_ context.Context, ref string) (*artifact.PurgeJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.purgeJobs[ref]
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge job not found: %s", ref)}
	}
	copyJob := job
	return &copyJob, nil
}

func (s *MemoryMetadataStore) ListPendingPurges(_ context.Context, query artifact.PurgeQuery) ([]artifact.PurgeJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	readyAt := query.ReadyAt
	if readyAt.IsZero() {
		readyAt = time.Now()
	}
	jobs := make([]artifact.PurgeJob, 0, limit)
	for _, job := range s.purgeJobs {
		if job.Status != artifact.PurgeStatusPending && job.Status != artifact.PurgeStatusRetrying && !(job.Status == artifact.PurgeStatusLeased && !job.LeaseUntil.After(readyAt)) {
			continue
		}
		if query.TenantID != "" && job.TenantID != query.TenantID || query.SessionID != "" && job.SessionID != query.SessionID || query.RunID != "" && job.RunID != query.RunID {
			continue
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ArtifactRef < jobs[j].ArtifactRef
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

func (s *MemoryMetadataStore) ClaimPurge(_ context.Context, ref string, workerID string, now time.Time, leaseUntil time.Time) (*artifact.PurgeJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.purgeJobs[ref]
	if !ok {
		return nil, false, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge job not found: %s", ref)}
	}
	if job.Status == artifact.PurgeStatusPurged {
		copyJob := job
		return &copyJob, false, nil
	}
	if job.Status == artifact.PurgeStatusLeased && job.LeaseOwner != workerID && job.LeaseUntil.After(now) {
		copyJob := job
		return &copyJob, false, nil
	}
	if job.Status != artifact.PurgeStatusPending && job.Status != artifact.PurgeStatusRetrying && job.Status != artifact.PurgeStatusLeased {
		copyJob := job
		return &copyJob, false, nil
	}
	job.Status = artifact.PurgeStatusLeased
	job.LeaseOwner = workerID
	job.LeaseUntil = leaseUntil
	job.LeaseVersion++
	job.UpdatedAt = now
	s.purgeJobs[ref] = job
	meta := s.byRef[ref]
	meta.PurgeStatus = artifact.PurgeStatusLeased
	s.byRef[ref] = meta
	copyJob := job
	return &copyJob, true, nil
}

func (s *MemoryMetadataStore) MarkPurgeSucceeded(_ context.Context, ref string, workerID string, leaseVersion int64, at time.Time) (*artifact.ArtifactMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, metaOK := s.byRef[ref]
	job, jobOK := s.purgeJobs[ref]
	if !metaOK || !jobOK {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge state not found: %s", ref)}
	}
	if job.Status == artifact.PurgeStatusPurged {
		return cloneMeta(meta), nil
	}
	if job.Status != artifact.PurgeStatusLeased || job.LeaseOwner != workerID || job.LeaseVersion != leaseVersion {
		return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("stale purge lease: %s", ref)}
	}
	job.Status = artifact.PurgeStatusPurged
	job.LastError = ""
	job.LeaseOwner = ""
	job.LeaseUntil = time.Time{}
	job.UpdatedAt = at
	s.purgeJobs[ref] = job
	meta.PurgeStatus = artifact.PurgeStatusPurged
	meta.PurgedAt = at
	s.byRef[ref] = meta
	return cloneMeta(meta), nil
}

func (s *MemoryMetadataStore) MarkPurgeFailed(_ context.Context, ref string, workerID string, leaseVersion int64, message string, at time.Time) (*artifact.PurgeJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.purgeJobs[ref]
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: fmt.Sprintf("purge job not found: %s", ref)}
	}
	if job.Status == artifact.PurgeStatusPurged {
		copyJob := job
		return &copyJob, nil
	}
	if job.Status != artifact.PurgeStatusLeased || job.LeaseOwner != workerID || job.LeaseVersion != leaseVersion {
		return nil, &artifact.Error{Code: artifact.ErrConflict, Message: fmt.Sprintf("stale purge lease: %s", ref)}
	}
	job.Status = artifact.PurgeStatusRetrying
	job.Attempts++
	job.LastError = message
	job.LeaseOwner = ""
	job.LeaseUntil = time.Time{}
	job.UpdatedAt = at
	s.purgeJobs[ref] = job
	meta := s.byRef[ref]
	meta.PurgeStatus = artifact.PurgeStatusRetrying
	s.byRef[ref] = meta
	copyJob := job
	return &copyJob, nil
}

func cloneMeta(meta artifact.ArtifactMeta) *artifact.ArtifactMeta {
	copyMeta := meta
	memo := make(map[cloneContainerID]reflect.Value)
	copyMeta.DerivedFrom = cloneContainerValue(reflect.ValueOf(meta.DerivedFrom), memo).Interface().([]artifact.ArtifactLineage)
	copyMeta.Preview.Fields = cloneContainerValue(reflect.ValueOf(meta.Preview.Fields), memo).Interface().(map[string]any)
	copyMeta.Metadata = cloneContainerValue(reflect.ValueOf(meta.Metadata), memo).Interface().(map[string]string)
	return &copyMeta
}

type cloneContainerID struct {
	kind     reflect.Kind
	typeOf   reflect.Type
	pointer  uintptr
	length   int
	capacity int
}

func cloneContainerValue(value reflect.Value, memo map[cloneContainerID]reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneContainerValue(value.Elem(), memo)
		wrapped := reflect.New(value.Type()).Elem()
		wrapped.Set(cloned)
		return wrapped
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		id := cloneContainerIdentity(value)
		if cloned, ok := memo[id]; ok {
			return cloned
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		memo[id] = cloned
		iterator := value.MapRange()
		for iterator.Next() {
			key := cloneContainerValue(iterator.Key(), memo)
			item := cloneContainerValue(iterator.Value(), memo)
			cloned.SetMapIndex(key, item)
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		id := cloneContainerIdentity(value)
		if cloned, ok := memo[id]; ok {
			return cloned
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Cap())
		memo[id] = cloned
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneContainerValue(value.Index(i), memo))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneContainerValue(value.Index(i), memo))
		}
		return cloned
	default:
		return value
	}
}

func cloneContainerIdentity(value reflect.Value) cloneContainerID {
	id := cloneContainerID{
		kind:    value.Kind(),
		typeOf:  value.Type(),
		pointer: value.Pointer(),
	}
	if value.Kind() == reflect.Slice {
		id.length = value.Len()
		id.capacity = value.Cap()
	}
	return id
}
