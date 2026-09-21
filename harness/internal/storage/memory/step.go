package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type stepStore struct {
	mu    sync.RWMutex
	byKey map[string]*storage.Step // key = runID + ":" + stepID
}

func newStepStore() *stepStore {
	return &stepStore{byKey: make(map[string]*storage.Step)}
}

func stepKey(runID, stepID string) string { return runID + ":" + stepID }

func (s *stepStore) Upsert(_ context.Context, step *storage.Step) error {
	if step == nil || step.StepID == "" || step.RunID == "" {
		return storageInvalid("step_id and run_id required")
	}
	if err := storage.ValidateStepIdentifiers(step); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := stepKey(step.RunID, step.StepID)
	clone := *step
	if existing, ok := s.byKey[key]; ok {
		// Field-level merge: lifecycle updates (Complete/Fail/Cancel) carry only
		// StepID/RunID/Status, so their empty descriptive fields must not clobber
		// the step_type/name/parent written at StartStep.
		if clone.StartedAt.IsZero() {
			clone.StartedAt = existing.StartedAt
		}
		if clone.StepType == "" {
			clone.StepType = existing.StepType
		}
		if clone.Name == "" {
			clone.Name = existing.Name
		}
		if clone.ParentStepID == "" {
			clone.ParentStepID = existing.ParentStepID
		}
		if clone.EndedAt.IsZero() {
			clone.EndedAt = existing.EndedAt
		}
	} else if clone.StartedAt.IsZero() {
		clone.StartedAt = time.Now()
	}
	s.byKey[key] = &clone
	return nil
}

func (s *stepStore) ListByRun(_ context.Context, runID string) ([]*storage.Step, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*storage.Step
	for _, step := range s.byKey {
		if step.RunID == runID {
			clone := *step
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}
