package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	storagesqlite "github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
)

// runStepMergeContract asserts that a lifecycle update carrying only
// StepID/RunID/Status does NOT clobber the step_type/name/parent recorded at
// StartStep. This is the same contract for memory/sqlite/mysql.
func runStepMergeContract(t *testing.T, steps storage.StepStore) {
	t.Helper()
	ctx := context.Background()
	const runID, stepID = "run-1", "step-1"

	// StartStep: full descriptive record.
	if err := steps.Upsert(ctx, &storage.Step{
		StepID: stepID, RunID: runID, ParentStepID: "root",
		StepType: "model_call", Name: "planner", Status: "running",
	}); err != nil {
		t.Fatalf("start upsert: %v", err)
	}
	// CompleteStep: partial record (mirrors runtimestore bridge) — status only.
	if err := steps.Upsert(ctx, &storage.Step{
		StepID: stepID, RunID: runID, Status: "completed",
	}); err != nil {
		t.Fatalf("complete upsert: %v", err)
	}

	list, err := steps.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 step, got %d", len(list))
	}
	got := list[0]
	if got.Status != "completed" {
		t.Errorf("status not updated: %q", got.Status)
	}
	if got.StepType != "model_call" {
		t.Errorf("step_type clobbered: %q (want model_call)", got.StepType)
	}
	if got.Name != "planner" {
		t.Errorf("name clobbered: %q (want planner)", got.Name)
	}
	if got.ParentStepID != "root" {
		t.Errorf("parent_step_id clobbered: %q (want root)", got.ParentStepID)
	}
	if got.StartedAt.IsZero() {
		t.Errorf("started_at lost")
	}
}

func TestStepMergeContract_Memory(t *testing.T) {
	runStepMergeContract(t, memory.New().Stores().Steps)
}

func TestStepMergeContract_SQLite(t *testing.T) {
	sq, err := storagesqlite.Open(filepath.Join(t.TempDir(), "steps.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := sq.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	runStepMergeContract(t, sq.Stores().Steps)
}
