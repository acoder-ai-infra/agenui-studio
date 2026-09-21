package artifact

import (
	"context"
	"time"
)

type MetadataStore interface {
	Create(ctx context.Context, meta ArtifactMeta, idempotencyKey string) (*ArtifactMeta, error)
	GetByRef(ctx context.Context, ref string) (*ArtifactMeta, error)
	GetByID(ctx context.Context, artifactID string) (*ArtifactMeta, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*ArtifactMeta, error)
	List(ctx context.Context, query ListQuery) ([]ArtifactMeta, error)
	MarkDeleted(ctx context.Context, ref string, reason DeleteReason, at time.Time) (*ArtifactMeta, error)
}

// PurgeStore atomically tombstones metadata and creates a recoverable purge
// job. Production implementations use a local transaction or an outbox.
type PurgeStore interface {
	RequestPurge(ctx context.Context, ref string, reason DeleteReason, at time.Time) (*ArtifactMeta, *PurgeJob, error)
	GetPurgeJob(ctx context.Context, ref string) (*PurgeJob, error)
	ListPendingPurges(ctx context.Context, query PurgeQuery) ([]PurgeJob, error)
	ClaimPurge(ctx context.Context, ref string, workerID string, now time.Time, leaseUntil time.Time) (*PurgeJob, bool, error)
	MarkPurgeSucceeded(ctx context.Context, ref string, workerID string, leaseVersion int64, at time.Time) (*ArtifactMeta, error)
	MarkPurgeFailed(ctx context.Context, ref string, workerID string, leaseVersion int64, message string, at time.Time) (*PurgeJob, error)
}
