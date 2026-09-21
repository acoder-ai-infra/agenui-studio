package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	designcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type editAuthorizerStub struct {
	raw         string
	contractRaw string
}

type bindingCommitterStub struct{}

func (bindingCommitterStub) CommitBinding(
	_ context.Context, _ extension.Context, _ bindingcontract.Input, candidate string,
) (string, string, string, error) {
	submission, err := bindingcontract.ParseSubmission(candidate)
	if err != nil {
		return "", "", "", err
	}
	result, err := json.Marshal(submission.Result)
	if err != nil {
		return "", "", "", err
	}
	final := `{"protocol":[]}`
	if !bindingcontract.IsExecutableStatus(submission.Result.Status) {
		final = ""
	}
	return string(result), candidate, final, nil
}

func (s editAuthorizerStub) EditContractSnapshot(_, _, _ string) string { return s.raw }
func (s editAuthorizerStub) DesigningSnapshot(_, _, _ string) (string, string, bool) {
	return s.contractRaw, "", s.contractRaw != ""
}

func TestWorkspaceCommitCompilesFrozenContractAtomically(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	document := workspace.Document{
		SchemaVersion: workspace.SchemaVersion, SurfaceID: "default", CatalogID: "placeholder", RootID: "root",
		Components: []map[string]any{
			{"id": "root", "component": "Text", "text": map[string]any{"path": "/product_name"}},
		},
		DataModel: map[string]any{"product_name": "西湖国宾馆"},
		FieldSlots: []map[string]any{{
			"componentId": "root", "contractItemId": "product_name", "refKey": "/product_name",
			"role": "title", "valueType": "string", "scope": "surface", "required": true,
		}},
	}
	revision := buildWorkspace(t, provider.tool, document)
	committed := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "commit", "base_revision": revision,
	})
	if committed["committed"] != true || committed["requirements_hash"] == "" {
		t.Fatalf("commit = %#v", committed)
	}
	_, requirementsJSON, requirementsHash, ok := provider.CommittedDesignSnapshot("tenant", "user", "session", "run")
	if !ok || requirementsJSON == "" || requirementsHash != committed["requirements_hash"] {
		t.Fatalf("snapshot requirements were not frozen with commit: %#v", committed)
	}
}

func TestWorkspaceCommitRejectsComponentsUnreachableFromRoot(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	document := workspace.Document{
		SchemaVersion: workspace.SchemaVersion, SurfaceID: "default", CatalogID: "placeholder", RootID: "root",
		Components: []map[string]any{
			{"id": "root", "component": "Column"},
			{"id": "title", "component": "Text", "text": map[string]any{"path": "/title"}},
		},
		DataModel: map[string]any{"title": "孤立标题"},
		FieldSlots: []map[string]any{{
			"componentId": "title", "contractItemId": "title", "refKey": "/title",
			"role": "title", "valueType": "string", "scope": "surface", "required": true,
		}},
	}
	revision := buildWorkspace(t, provider.tool, document)
	rejected := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "commit", "base_revision": revision,
	})
	if rejected["committed"] != false || rejected["retryable"] != true {
		t.Fatalf("commit should reject an orphaned component graph: %#v", rejected)
	}
	if !strings.Contains(fmt.Sprint(rejected["error"]), "'children' is a required property") {
		t.Fatalf("commit error should identify the missing root topology: %#v", rejected["error"])
	}
	if !strings.Contains(fmt.Sprint(rejected["repair_feedback"]), "declared root") {
		t.Fatalf("commit should return a topology repair instruction: %#v", rejected)
	}
	if next, _ := rejected["next_actions"].([]any); len(next) != 2 || next[0] != "put_components" || next[1] != "commit" {
		t.Fatalf("commit recovery should repair before retrying: %#v", rejected["next_actions"])
	}
	if _, _, _, ok := provider.CommittedDesignSnapshot("tenant", "user", "session", "run"); ok {
		t.Fatal("unreachable component graph was exposed as a committed Design Artifact")
	}
}

func TestWorkspaceStateIsIsolatedByRootRun(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(runID, rootRunID, title string) map[string]any {
		arguments, _ := json.Marshal(map[string]any{
			"action": "begin", "surface_id": "default", "catalog_id": "placeholder",
			"root_id": "root", "data_model": map[string]any{"title": title},
		})
		result, invokeErr := provider.tool.Invoke(context.Background(), extension.FunctionCall{
			Ctx: extension.Context{
				TenantID: "tenant", UserID: "user", SessionID: "session",
				RunID: runID, RootRunID: rootRunID, AgentID: agenuiextensions.StyleAgent,
			},
			Name: WorkspaceToolName, Version: WorkspaceToolVersion, Arguments: arguments,
		})
		if invokeErr != nil {
			t.Fatal(invokeErr)
		}
		var decoded map[string]any
		if err := json.Unmarshal(result.Data, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	first := invoke("style-child-1", "root-1", "first")
	second := invoke("style-child-2", "root-2", "second")
	if first["revision"] == second["revision"] {
		t.Fatal("independent root Runs unexpectedly shared one Workspace revision")
	}
	if _, ok := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "root-1")); !ok {
		t.Fatal("first root Run workspace is missing")
	}
	if _, ok := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "root-2")); !ok {
		t.Fatal("second root Run workspace is missing")
	}
}

func TestWorkspaceBeginRejectsNonProtocolRootID(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.Marshal(map[string]any{
		"action": "begin", "surface_id": "default", "catalog_id": "placeholder",
		"root_id": "product-card", "data_model": map[string]any{"title": "商品"},
	})
	result, err := provider.tool.Invoke(context.Background(), extension.FunctionCall{
		Ctx: extension.Context{
			TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
			RootRunID: "run", AgentID: agenuiextensions.StyleAgent,
		},
		Name: WorkspaceToolName, Version: WorkspaceToolVersion, Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	var rejected map[string]any
	if err := json.Unmarshal(result.Data, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected["accepted"] != false || !strings.Contains(fmt.Sprint(rejected["error"]), `root_id must be "root"`) {
		t.Fatalf("begin result = %#v", rejected)
	}
	if _, exists := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "run")); exists {
		t.Fatal("rejected begin created a Workspace session")
	}
}

func TestWorkspaceCommitSupportsMultipleActionsOnOneComponentWithoutSlotIDs(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	document := workspace.Document{
		SchemaVersion: workspace.SchemaVersion, SurfaceID: "default", CatalogID: "placeholder", RootID: "root",
		Components: []map[string]any{
			{"id": "root", "component": "Column", "children": []any{"actionButton"}},
			{"id": "actionButton", "component": "Button", "action": map[string]any{"event": map[string]any{"name": "purchase"}}, "child": "buttonLabel"},
			{"id": "buttonLabel", "component": "Text", "text": map[string]any{"path": "/button_label"}},
		},
		DataModel: map[string]any{"button_label": "购票", "items": []any{map[string]any{"title": "联票"}}},
		FieldSlots: []map[string]any{{
			"componentId": "buttonLabel", "contractItemId": "button_label", "refKey": "/button_label",
			"role": "label", "valueType": "string", "scope": "surface", "required": true,
		}},
		ActionSlots: []map[string]any{
			{"componentId": "actionButton", "contractActionId": "purchase_ticket", "role": "action", "scope": "item", "scopeRefKey": "/items[*]"},
			{"componentId": "actionButton", "contractActionId": "reserve_ticket", "role": "action", "scope": "item", "scopeRefKey": "/items[*]"},
		},
	}
	revision := buildWorkspace(t, provider.tool, document)
	committed := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "commit", "base_revision": revision,
	})
	if committed["committed"] != true {
		t.Fatalf("commit = %#v", committed)
	}
	current, _ := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "run"))
	if len(current.Document.ActionSlots) != 2 ||
		current.Document.ActionSlots[0]["slotId"] == current.Document.ActionSlots[1]["slotId"] {
		t.Fatalf("action slot identities = %#v", current.Document.ActionSlots)
	}
}

func TestWorkspaceConflictReturnsStructuredRecoveryIdentity(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	result := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "begin", "surface_id": "default", "catalog_id": "placeholder",
		"root_id": "root", "data_model": map[string]any{"title": "商品"},
	})
	revision := result["revision"].(string)
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "put_components", "base_revision": revision,
		"components": []map[string]any{{"id": "root", "component": "Text", "text": map[string]any{"path": "/title"}}},
	})
	revision = result["revision"].(string)
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "set_slots", "base_revision": revision,
		"field_slots": []map[string]any{
			{"componentId": "root", "contractItemId": "title", "refKey": "/title", "role": "title", "description": "first"},
			{"componentId": "root", "contractItemId": "title", "refKey": "/title", "role": "title", "description": "second"},
		},
		"action_slots": []map[string]any{},
	})
	if result["accepted"] != false {
		t.Fatalf("conflicting set_slots = %#v", result)
	}
	identity, _ := result["conflict_identity"].(map[string]any)
	if identity["kind"] != "field" || identity["component_id"] != "root" ||
		identity["contract_id"] != "title" || identity["property_path"] != "/title" {
		t.Fatalf("conflict identity = %#v", identity)
	}
	if result["revision"] != revision {
		t.Fatalf("recovery revision = %#v, want %q", result["revision"], revision)
	}
}

func invokeWorkspace(t *testing.T, tool *WorkspaceTool, agentID string, input any) map[string]any {
	t.Helper()
	arguments, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", AgentID: agentID},
		Name: WorkspaceToolName, Version: WorkspaceToolVersion, Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(result.Data, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func buildWorkspace(t *testing.T, tool *WorkspaceTool, document workspace.Document) string {
	t.Helper()
	if document.FieldSlots == nil {
		document.FieldSlots = []map[string]any{}
	}
	if document.ActionSlots == nil {
		document.ActionSlots = []map[string]any{}
	}
	configureWorkspaceContract(t, tool.provider, document)
	result := invokeWorkspace(t, tool, "agenui_style", map[string]any{
		"action": "begin", "surface_id": document.SurfaceID,
		"catalog_id": document.CatalogID, "root_id": document.RootID,
		"data_model": document.DataModel,
	})
	revision := result["revision"].(string)
	result = invokeWorkspace(t, tool, "agenui_style", map[string]any{
		"action": "put_components", "base_revision": revision,
		"components": document.Components,
	})
	revision = result["revision"].(string)
	result = invokeWorkspace(t, tool, "agenui_style", map[string]any{
		"action": "set_slots", "base_revision": revision,
		"field_slots": document.FieldSlots, "action_slots": document.ActionSlots,
		"design_knowledge_receipt": document.DesignKnowledgeReceipt,
	})
	return result["revision"].(string)
}

func configureWorkspaceContract(t *testing.T, provider *WorkspaceProvider, document workspace.Document) {
	t.Helper()
	draft := designcontract.Draft{Goal: "workspace test", Type: "single"}
	seenContent := map[string]struct{}{}
	for _, slot := range document.FieldSlots {
		id := strings.TrimSpace(workspaceString(slot, "contractItemId", "contract_item_id"))
		if id == "" {
			continue
		}
		if _, exists := seenContent[id]; exists {
			continue
		}
		seenContent[id] = struct{}{}
		required, _ := slot["required"].(bool)
		draft.Contents = append(draft.Contents, designcontract.ContentItem{ID: id, Description: id, Required: required})
	}
	seenActions := map[string]struct{}{}
	for _, slot := range document.ActionSlots {
		id := strings.TrimSpace(workspaceString(slot, "contractActionId", "contract_action_id"))
		if id == "" {
			continue
		}
		if _, exists := seenActions[id]; exists {
			continue
		}
		seenActions[id] = struct{}{}
		draft.Actions = append(draft.Actions, designcontract.ActionItem{ID: id, Description: id})
	}
	revision := designcontract.Revision{SchemaVersion: designcontract.SchemaVersion, Draft: draft}
	raw, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	provider.authorizer = editAuthorizerStub{contractRaw: string(raw)}
}

func TestWorkspaceBuildInspectAndCommit(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	document := workspace.Document{
		SchemaVersion: workspace.SchemaVersion, SurfaceID: "default",
		CatalogID: "placeholder", RootID: "root",
		Components: []map[string]any{
			{"id": "root", "component": "Column", "children": []any{"title"}},
			{"id": "title", "component": "Text", "text": map[string]any{"path": "/title"}},
		},
		DataModel:  map[string]any{"title": "标题"},
		FieldSlots: []map[string]any{{"componentId": "title", "contractItemId": "title", "refKey": "/title"}},
	}
	revision := buildWorkspace(t, provider.tool, document)
	if revision == "" {
		t.Fatal("build returned no revision")
	}
	inspected := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "inspect", "component_ids": []string{"title"},
	})
	components, _ := inspected["components"].([]any)
	if len(components) != 1 {
		t.Fatalf("inspect should return only requested component: %#v", inspected)
	}
	committed := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{"action": "commit", "base_revision": revision})
	if committed["catalog_id"] == "" {
		t.Fatalf("commit = %#v", committed)
	}
	artifact, _, _, ok := provider.CommittedDesignSnapshot("tenant", "user", "session", "run")
	if !ok || artifact == "" {
		t.Fatal("committed design is unavailable")
	}
}

func TestWorkspaceNormalizesNestedAndJSONStringComponents(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	result := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "begin", "surface_id": "default", "catalog_id": "placeholder",
		"root_id": "root", "data_model": map[string]any{"title": "Readable preview"},
	})
	revision := result["revision"].(string)
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "put_components", "base_revision": revision,
		"components": []map[string]any{
			{"id": "root", "component": map[string]any{"component": "Column", "children": []any{"title"}}},
			{"id": "title", "component": `{"component":"Text","text":{"path":"/title"}}`},
		},
	})
	if result["accepted_count"] != float64(2) {
		t.Fatalf("nested components were not accepted: %#v", result)
	}
	current, _ := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "run"))
	if current.Document.Components[0]["component"] != "Column" ||
		current.Document.Components[1]["component"] != "Text" {
		t.Fatalf("components were not canonicalized: %#v", current.Document.Components)
	}
	if next, _ := result["next_actions"].([]any); len(next) != 1 || next[0] != "set_slots" {
		t.Fatalf("unexpected recovery path: %#v", result)
	}
}

func TestWorkspaceRejectedCallReturnsCurrentRecoveryState(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	result := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "begin", "surface_id": "default", "catalog_id": "placeholder",
		"root_id": "root", "data_model": map[string]any{"title": "Readable preview"},
	})
	revision := result["revision"].(string)
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "put_components", "base_revision": revision,
		"components": []map[string]any{{"id": "root", "component": 42}},
	})
	if result["accepted"] != false || result["revision"] != revision {
		t.Fatalf("rejection lost recovery state: %#v", result)
	}
	if next, _ := result["next_actions"].([]any); len(next) != 1 || next[0] != "put_components" {
		t.Fatalf("unexpected recovery path: %#v", result)
	}
}

func TestWorkspaceCommitRejectsSlotForUnknownComponent(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	revision := buildWorkspace(t, provider.tool, workspace.Document{
		SchemaVersion: workspace.SchemaVersion,
		SurfaceID:     "default", CatalogID: "placeholder", RootID: "root",
		Components: []map[string]any{{"id": "root", "component": "Text", "text": "示例"}},
		DataModel:  map[string]any{"title": "示例"},
		FieldSlots: []map[string]any{{"componentId": "missing", "contractItemId": "title", "refKey": "/title"}},
	})
	arguments, _ := json.Marshal(map[string]any{"action": "commit", "base_revision": revision})
	result, err := provider.tool.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", AgentID: "agenui_style"},
		Name: WorkspaceToolName, Version: WorkspaceToolVersion, Arguments: arguments,
	})
	if err != nil || !strings.Contains(string(result.Data), `references unknown component \"missing\"`) {
		t.Fatalf("commit result = %s error = %v", result.Data, err)
	}
}

func TestWorkspaceMutationInvalidatesCommittedRevision(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	revision := buildWorkspace(t, provider.tool, workspace.Document{
		SchemaVersion: workspace.SchemaVersion,
		SurfaceID:     "default", CatalogID: "placeholder", RootID: "root",
		Components: []map[string]any{{"id": "root", "component": "Text", "text": "示例"}},
		DataModel:  map[string]any{"title": "示例"},
	})
	committed := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "commit", "base_revision": revision,
	})
	revision = committed["revision"].(string)
	updated := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "set_slots", "base_revision": revision,
		"field_slots":  []map[string]any{{"componentId": "missing", "contractItemId": "title", "refKey": "/title"}},
		"action_slots": []map[string]any{},
	})
	if _, _, _, ok := provider.CommittedDesignSnapshot("tenant", "user", "session", "run"); ok {
		t.Fatal("mutated workspace remained committed")
	}
	arguments, _ := json.Marshal(map[string]any{"action": "commit", "base_revision": updated["revision"]})
	result, err := provider.tool.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run", AgentID: "agenui_style"},
		Name: WorkspaceToolName, Version: WorkspaceToolVersion, Arguments: arguments,
	})
	if err != nil || !strings.Contains(string(result.Data), `references unknown component \"missing\"`) {
		t.Fatalf("recommit result = %s error = %v", result.Data, err)
	}
}

func TestWorkspaceBuildsLargeDocumentThroughBoundedCallsAndReportsPreview(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	configureWorkspaceContract(t, provider, workspace.Document{FieldSlots: []map[string]any{{
		"componentId": "title", "contractItemId": "title", "refKey": "/title", "required": false,
	}}})
	result := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "begin", "surface_id": "default", "catalog_id": "placeholder",
		"root_id": "root", "data_model": map[string]any{"title": ""},
	})
	revision := result["revision"].(string)
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "put_components", "base_revision": revision,
		"components": []map[string]any{
			{"id": "root", "component": "Column", "children": []any{"title"}},
			{"id": "title", "component": "Text", "text": map[string]any{"path": "/title"}},
		},
	})
	revision = result["revision"].(string)
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "set_slots", "base_revision": revision,
		"field_slots":  []map[string]any{{"componentId": "title", "contractItemId": "title", "refKey": "/title"}},
		"action_slots": []map[string]any{},
	})
	revision = result["revision"].(string)
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "set_preview_data", "base_revision": revision,
		"data_model": map[string]any{"title": "Readable preview"},
	})
	revision = result["revision"].(string)
	if preview := result["ready"]; preview != true {
		t.Fatalf("preview should be readable: %#v", result)
	}
	result = invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "commit", "base_revision": revision,
	})
	preview, _ := result["preview"].(map[string]any)
	if result["committed"] != true || preview["ready"] != true {
		t.Fatalf("commit = %#v", result)
	}
}

func TestWorkspaceAppliesOnlyFrozenEditContract(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	base := typedDesignArtifact(t,
		`[{"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"catalog:test"}},{"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"Column","children":["title"]},{"id":"title","component":"Text","styles":{"color":"black"},"text":{"path":"/title"}}]}},{"version":"v0.9","updateDataModel":{"surfaceId":"default","path":"/","value":{"title":"标题"}}}]`,
		`[{"slotId":"title.text","componentId":"title","refKey":"/title"}]`, `[]`)
	if err := provider.RestoreDesign("tenant", "user", "session", "run", base); err != nil {
		t.Fatal(err)
	}
	current, _ := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "run"))
	authorization := edit.Contract{
		SchemaVersion: edit.SchemaVersion, Operation: "update",
		TargetSet:     []edit.Target{{Kind: "design_slot", ComponentID: "title", Path: "styles.color", Value: "red"}},
		Preconditions: edit.Preconditions{DesignHash: edit.HashText(base)},
	}
	raw, _ := json.Marshal(authorization)
	if err := provider.SetEditAuthorizer(editAuthorizerStub{raw: string(raw)}); err != nil {
		t.Fatal(err)
	}
	result := invokeWorkspace(t, provider.tool, "agenui_style", map[string]any{
		"action": "apply_edit_contract", "base_revision": current.Revision,
	})
	if result["revision"] == current.Revision {
		t.Fatalf("edit did not advance revision: %#v", result)
	}
	if _, leaked := result["document"]; leaked {
		t.Fatalf("edit result leaked the full document: %#v", result)
	}
	after, _ := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "run"))
	if after.Document.Components[1]["styles"].(map[string]any)["color"] != "red" || after.Document.DataModel["title"] != "标题" {
		t.Fatalf("edit escaped frozen target: %#v", after.Document)
	}
}

func TestWorkspaceBinderUsesProjectionAndSubmitsCandidate(t *testing.T) {
	provider, err := NewWorkspaceProvider(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetBindingCommitter(bindingCommitterStub{}); err != nil {
		t.Fatal(err)
	}
	document := workspace.Document{
		SchemaVersion: workspace.SchemaVersion, SurfaceID: "default", CatalogID: "placeholder", RootID: "root",
		Components: []map[string]any{
			{"id": "root", "component": "Column", "children": []any{"title"}},
			{"id": "title", "component": "Text", "text": map[string]any{"path": "/title"}},
		},
		DataModel:  map[string]any{"title": ""},
		FieldSlots: []map[string]any{{"componentId": "title", "contractItemId": "title", "refKey": "/title"}},
	}
	revision := buildWorkspace(t, provider.tool, document)
	current, _ := provider.snapshot(workspaceSessionKey("tenant", "user", "session", "run"))
	titleSlotID := workspaceString(current.Document.FieldSlots[0], "slotId")
	draft := designcontract.Draft{
		Goal: "show title", Type: "single",
		Contents: []designcontract.ContentItem{{ID: "title", Description: "title", Required: true}},
	}
	contractHash, err := designcontract.Hash(draft)
	if err != nil {
		t.Fatal(err)
	}
	bindingInput := bindingcontract.Input{
		SchemaVersion: bindingcontract.InputSchemaV1,
		Contract: designcontract.Revision{
			ContractID: "contract-title", Revision: 1, SchemaVersion: designcontract.SchemaVersion,
			Status: "confirmed", ChangeOrigin: "user", ContentHash: contractHash, Draft: draft,
		},
		Design: bindingcontract.DesignSnapshot{
			Ref: "artifact://design", ContentHash: "sha256:design",
			FieldHints:  mustJSON(t, []map[string]any{{"slotId": titleSlotID, "contractItemId": "title", "refKey": "/title"}}),
			ActionSlots: json.RawMessage(`[]`),
		},
		Requirements: requirements.Set{
			SchemaVersion: requirements.SchemaVersion, InputHash: "sha256:requirements",
			Data: []requirements.DataRequirement{{
				RequirementID: "data.title", ContractItemID: "title", Description: "title",
				TargetSlotIDs: []string{titleSlotID}, Level: "core", Type: "string",
				Shape: "scalar", WhenMissing: "block",
			}},
		},
		Sources: []bindingcontract.SourceSnapshot{{
			SourceID: "demo@v1", KnowledgeID: "demo@sha256:test", Primary: true,
			Paths: []string{"$.name"},
		}},
	}
	if err := provider.PrepareBindingInput(context.Background(), harness.Identity{
		TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
	}, bindingInput); err != nil {
		t.Fatal(err)
	}
	refreshedSources := []bindingcontract.SourceSnapshot{{
		SourceID: "demo-v2@v2", KnowledgeID: "demo-v2@sha256:next", Primary: true,
	}}
	if err := provider.RefreshBindingSources(context.Background(), harness.Identity{
		TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
	}, refreshedSources); err != nil {
		t.Fatal(err)
	}
	projection := invokeWorkspace(t, provider.tool, "agenui_binder", map[string]any{"action": "inspect_binding"})
	if projection["revision"] != revision {
		t.Fatalf("projection = %#v", projection)
	}
	projectedSources, _ := projection["sources"].([]any)
	if len(projectedSources) != 1 || projectedSources[0].(map[string]any)["source_id"] != "demo-v2@v2" {
		t.Fatalf("sources were not refreshed: %#v", projection)
	}
	// Restore the source used by the remainder of this candidate test.
	if err := provider.RefreshBindingSources(context.Background(), harness.Identity{
		TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
	}, bindingInput.Sources); err != nil {
		t.Fatal(err)
	}
	bindingResult := map[string]any{
		"schema_version": bindingcontract.ResultSchemaV1, "status": bindingcontract.StatusReady,
		"bindings": []map[string]any{{
			"requirement_id": "data.title", "target_slot_ids": []string{titleSlotID},
			"source_id": "demo@v1", "knowledge_id": "demo@sha256:test",
			"field_path": "$.name", "ref_key": "/title",
		}},
	}
	bindingResultJSON, _ := json.Marshal(bindingResult)
	result := invokeWorkspace(t, provider.tool, "agenui_binder", map[string]any{
		"action": "commit_binding", "base_revision": revision,
		"binding_result": string(bindingResultJSON),
	})
	if result["accepted"] != true {
		t.Fatalf("submit = %#v", result)
	}
	blockedResult := map[string]any{
		"schema_version": bindingcontract.ResultSchemaV1,
		"status":         bindingcontract.StatusBlocked,
		"bindings":       []map[string]any{},
		"issues": []map[string]any{{
			"requirement_id": "data.title", "code": "NO_MATCHING_SOURCE",
			"message": "No authorized source can provide the required title.",
		}},
	}
	blockedJSON, _ := json.Marshal(blockedResult)
	result = invokeWorkspace(t, provider.tool, "agenui_binder", map[string]any{
		"action": "commit_binding", "base_revision": revision,
		"binding_result": string(blockedJSON),
	})
	if result["accepted"] != true || result["status"] != bindingcontract.StatusBlocked || result["binding_count"] != float64(0) {
		t.Fatalf("fully blocked candidate = %#v", result)
	}
	_, plan, final, ok := provider.CommittedBindingSnapshot("tenant", "user", "session", "run")
	if !ok || !strings.Contains(plan, `"schema_version":"agenui.binding-submission/v1"`) || final != "" {
		t.Fatalf("committed plan = %q final=%q", plan, final)
	}
}
