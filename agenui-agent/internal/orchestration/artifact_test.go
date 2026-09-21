package orchestration

import (
	"encoding/json"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
)

func typedDesignArtifact(t *testing.T, messagesJSON, fieldsJSON, actionsJSON string) string {
	t.Helper()
	artifact := workspace.DesignArtifact{SchemaVersion: workspace.DesignArtifactSchemaVersion}
	if err := json.Unmarshal([]byte(messagesJSON), &artifact.Messages); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(fieldsJSON), &artifact.FieldSlots); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(actionsJSON), &artifact.ActionSlots); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
