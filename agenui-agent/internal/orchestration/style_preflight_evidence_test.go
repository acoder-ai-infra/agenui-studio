package orchestration

import (
	"context"
	"encoding/json"
	"testing"

	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	harness "github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type recordingArtifactStore struct {
	steps map[string]string
}

func (s *recordingArtifactStore) Save(_ context.Context, _ harness.Identity, step, value string) (stepartifact.Pointer, error) {
	if s.steps == nil {
		s.steps = map[string]string{}
	}
	s.steps[step] = value
	return stepartifact.Pointer{}, nil
}

func (*recordingArtifactStore) Load(context.Context, harness.Identity, string) (string, error) {
	return "", stepartifact.ErrNotFound
}

func (*recordingArtifactStore) LatestRunID(context.Context, harness.Identity, string) (string, error) {
	return "", stepartifact.ErrNotFound
}

func TestStylePublicAPISearchPersistsCapabilityEvidence(t *testing.T) {
	t.Parallel()
	store := &recordingArtifactStore{}
	interceptor := NewTaskInterceptorWithStore(store)
	result := `{"total":1,"results":[{"id":"products","path":"/demo/products","method":"GET","description":"Product list","response_model":{"type":"object","properties":{"items":{"type":"array"}}}}]}`

	_, err := interceptor.Intercept(context.Background(), extension.ToolCallInfo{
		Ctx: extension.Context{
			TenantID: "public", UserID: "local", SessionID: "session-1",
			RunID: "child-1", RootRunID: "root-1", AgentID: agenuiextensions.StyleAgent,
		},
		Name:      "search_developer_apis",
		Arguments: json.RawMessage(`{"query":"products","top_k":3}`),
	}, func(context.Context, json.RawMessage) (extension.ToolCallOutcome, error) {
		return extension.ToolCallOutcome{Result: result}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.steps[stepartifact.StepCapabilityEvidence] == "" {
		t.Fatal("public Style API search did not persist capability evidence")
	}
}
