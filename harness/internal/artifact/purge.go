package artifact

import (
	"context"
	"errors"
	"time"
)

const defaultPurgeBatchSize = 100
const defaultPurgeLeaseTTL = 30 * time.Second

// PurgeWorker retries object deletion from durable metadata jobs. Delete on an
// ObjectStore must be idempotent: a retry after an unknown outcome is normal.
type PurgeWorker struct {
	Objects  ObjectStore
	Jobs     PurgeStore
	Clock    Clock
	WorkerID string
	LeaseTTL time.Duration
}

func (w *PurgeWorker) Process(ctx context.Context, artifactRef string) error {
	if w == nil || w.Objects == nil || w.Jobs == nil {
		return errorf(ErrInvalidArgument, "purge worker dependencies are required")
	}
	job, err := w.Jobs.GetPurgeJob(ctx, artifactRef)
	if err != nil {
		return err
	}
	if job.Status == PurgeStatusPurged {
		return nil
	}
	now := time.Now()
	if w.Clock != nil {
		now = w.Clock.Now()
	}
	workerID := w.WorkerID
	if workerID == "" {
		workerID = "artifact-purge-inline"
	}
	leaseTTL := w.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = defaultPurgeLeaseTTL
	}
	job, claimed, err := w.Jobs.ClaimPurge(ctx, artifactRef, workerID, now, now.Add(leaseTTL))
	if err != nil {
		return err
	}
	if !claimed {
		if job != nil && job.Status == PurgeStatusPurged {
			return nil
		}
		return errorf(ErrConflict, "artifact purge job is leased: %s", artifactRef)
	}
	if err := w.Objects.Delete(ctx, job.StorageKey); err != nil {
		_, markErr := w.Jobs.MarkPurgeFailed(ctx, artifactRef, workerID, job.LeaseVersion, "object_delete_failed", now)
		return errors.Join(err, markErr)
	}
	_, err = w.Jobs.MarkPurgeSucceeded(ctx, artifactRef, workerID, job.LeaseVersion, now)
	return err
}

func (w *PurgeWorker) RunOnce(ctx context.Context, query PurgeQuery) (int, error) {
	if w == nil || w.Jobs == nil {
		return 0, errorf(ErrInvalidArgument, "purge worker dependencies are required")
	}
	if query.Limit <= 0 {
		query.Limit = defaultPurgeBatchSize
	}
	if query.ReadyAt.IsZero() {
		query.ReadyAt = time.Now()
		if w.Clock != nil {
			query.ReadyAt = w.Clock.Now()
		}
	}
	jobs, err := w.Jobs.ListPendingPurges(ctx, query)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, job := range jobs {
		if err := w.Process(ctx, job.ArtifactRef); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}
