package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type recoveryArtifactLoader struct{ values map[string]string }

func (l recoveryArtifactLoader) Load(_ context.Context, _ harness.Identity, step string) (string, error) {
	value := l.values[step]
	if value == "" {
		return "", stepartifact.ErrNotFound
	}
	return value, nil
}
func (l recoveryArtifactLoader) Save(_ context.Context, _ harness.Identity, step, value string) (stepartifact.Pointer, error) {
	l.values[step] = value
	return stepartifact.Pointer{Ref: "artifact://" + step}, nil
}
func (l recoveryArtifactLoader) LatestRunID(context.Context, harness.Identity, string) (string, error) {
	return "run-old", nil
}

func TestResolveDesignEditRestoresContractAndDesignAfterRestart(t *testing.T) {
	draft := contract.Draft{Goal: "查看商品", Type: "single", Contents: []contract.ContentItem{{ID: "product.name", Description: "名称", Required: true}}, Actions: []contract.ActionItem{{ID: "product.detail", Description: "查看详情"}}}
	hash, _ := contract.Hash(draft)
	revision, _ := json.Marshal(contract.Revision{ContractID: "contract-1", Revision: 1, SchemaVersion: contract.SchemaVersion, Status: "confirmed", ChangeOrigin: "user", ContentHash: hash, Draft: draft})
	design := typedDesignArtifact(t,
		`[{"updateComponents":{"components":[{"id":"root","component":"Column","children":["detail"]},{"id":"detail","component":"Button","child":"label"},{"id":"label","component":"Text","text":"查看详情"}]}}]`,
		`[]`, `[{"slotId":"product.detail.primary","componentId":"detail","role":"primary_action","description":"查看详情","contractActionId":"product.detail"}]`)
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetPersistence(recoveryArtifactLoader{values: map[string]string{stepartifact.StepContract: string(revision), stepartifact.StepDesign: design}}); err != nil {
		t.Fatal(err)
	}
	result, err := provider.edit.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", AgentID: agenuiextensions.MainAgent},
		Name: ResolveDesignEditName, Arguments: json.RawMessage(`{"query":"把查看详情按钮放大","target_id":"product.detail.primary"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result.Data, []byte(`"component_id":"detail"`)) {
		t.Fatalf("result=%s", result.Data)
	}
}
