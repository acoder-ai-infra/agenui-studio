package orchestration

import (
	"context"
	"fmt"
	"testing"

	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	harness "github.com/AGenUI/agenui-studio/harness/sdk"
)

type lineageArtifactStore struct {
	values map[string]string
}

func lineageArtifactKey(identity harness.Identity, step string) string {
	return identity.TenantID + "/" + identity.UserID + "/" + identity.SessionID + "/" + identity.RunID + "/" + step
}

func (s *lineageArtifactStore) Save(_ context.Context, identity harness.Identity, step, content string) (stepartifact.Pointer, error) {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[lineageArtifactKey(identity, step)] = content
	return stepartifact.Pointer{Ref: "artifact://" + identity.RunID + "/" + step}, nil
}

func (s *lineageArtifactStore) Load(_ context.Context, identity harness.Identity, step string) (string, error) {
	value, ok := s.values[lineageArtifactKey(identity, step)]
	if !ok {
		return "", stepartifact.ErrNotFound
	}
	return value, nil
}

func (*lineageArtifactStore) LatestRunID(context.Context, harness.Identity, string) (string, error) {
	return "", fmt.Errorf("LatestRunID must not be used after the binding edit base is frozen")
}

func TestMaterializeBindingEditBaseUsesFrozenRunAndCurrentRoot(t *testing.T) {
	t.Parallel()
	base := harness.Identity{
		TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", RunID: "run-completed",
	}
	current := base
	current.RunID = "run-continuation"
	store := &lineageArtifactStore{values: make(map[string]string)}
	for _, step := range []string{
		stepartifact.StepContract,
		stepartifact.StepPreflight,
		stepartifact.StepTemplate,
		stepartifact.StepDesign,
		stepartifact.StepRequirements,
	} {
		store.values[lineageArtifactKey(base, step)] = step + "-from-completed-run"
	}
	// A newer incomplete Run in the same Session must never become the base.
	incomplete := base
	incomplete.RunID = "run-newer-incomplete"
	store.values[lineageArtifactKey(incomplete, stepartifact.StepDesign)] = "wrong-design"

	interceptor := NewTaskInterceptorWithStore(store)
	design, err := interceptor.materializeBindingEditBase(
		context.Background(), current, base.RunID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if design != stepartifact.StepDesign+"-from-completed-run" {
		t.Fatalf("design = %q", design)
	}
	for _, step := range []string{
		stepartifact.StepContract,
		stepartifact.StepPreflight,
		stepartifact.StepTemplate,
		stepartifact.StepDesign,
		stepartifact.StepRequirements,
	} {
		got, err := store.Load(context.Background(), current, step)
		if err != nil || got != step+"-from-completed-run" {
			t.Fatalf("current %s = %q, err = %v", step, got, err)
		}
	}
	if _, err := store.Load(context.Background(), current, stepartifact.StepFinal); err == nil {
		t.Fatal("previous Final must not be copied into a new binding edit Run")
	}
	if _, err := store.Load(context.Background(), current, stepartifact.StepBinding); err == nil {
		t.Fatal("previous Binding must not conflict with the new binding result")
	}
}
