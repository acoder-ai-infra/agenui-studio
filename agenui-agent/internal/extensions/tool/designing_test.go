package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

type designingArtifactStub struct {
	values     map[string]string
	pointer    stepartifact.Pointer
	saveErr    error
	latestStep string
}

func (s *designingArtifactStub) Save(_ context.Context, _ harness.Identity, step, content string) (stepartifact.Pointer, error) {
	if s.saveErr != nil {
		return stepartifact.Pointer{}, s.saveErr
	}
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[step] = content
	if s.pointer.Ref == "" {
		s.pointer = stepartifact.Pointer{Ref: "artifact://edit", Hash: "sha256:artifact", MIME: "application/json", Size: int64(len(content)), SchemaVersion: "agenui.step.v1"}
	}
	return s.pointer, nil
}

func TestSubmitContentContractDoesNotPublishFailedPersistence(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetPersistence(&designingArtifactStub{saveErr: errors.New("disk unavailable")}); err != nil {
		t.Fatal(err)
	}
	call := extension.FunctionCall{
		Ctx: extension.Context{
			TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
			RunID: "run-1", AgentID: agenuiextensions.MainAgent,
		},
		Name: SubmitContractName,
		Arguments: json.RawMessage(
			`{"goal":"compare products","type":"list","contents":[{"id":"name","description":"name","required":true}]}`,
		),
	}
	if _, err := provider.contract.Invoke(context.Background(), call); err == nil {
		t.Fatal("submit unexpectedly succeeded")
	}
	if _, _, ok := provider.DesigningSnapshot("tenant-1", "user-1", "session-1"); ok {
		t.Fatal("failed persistence leaked a confirmed in-memory contract")
	}
}

func (s *designingArtifactStub) Load(_ context.Context, _ harness.Identity, step string) (string, error) {
	return s.values[step], nil
}

func (s *designingArtifactStub) LatestRunID(_ context.Context, _ harness.Identity, step string) (string, error) {
	s.latestStep = step
	if strings.TrimSpace(s.values[stepartifact.StepDesign]) == "" {
		return "", stepartifact.ErrNotFound
	}
	return "run-base", nil
}

type preflightEvidenceStore struct {
	evidence string
}

func (s *preflightEvidenceStore) CapabilityPreflightEvidence(_, _, _ string) (string, bool) {
	return s.evidence, s.evidence != ""
}
func TestDesigningToolsSubmitContractThenFailClosedPreflight(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := extension.Context{RunID: "run-1", SessionID: "session-1", AgentID: agenuiextensions.MainAgent}
	if _, err := provider.contract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitContractName, Arguments: json.RawMessage(`{"goal":"比较商品","type":"list","contents":[{"id":"product.name","description":"名称","required":true}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	ctx.AgentID = agenuiextensions.StyleAgent
	result, err := provider.preflight.Invoke(context.Background(), extension.FunctionCall{Ctx: ctx, Name: PreflightCapabilitiesName, Arguments: json.RawMessage(`{"contract_source":"frozen"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Data) == "" {
		t.Fatal("preflight result is empty")
	}
}

func TestResolveDesignEditLetsModelSelectARealElement(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	design := typedDesignArtifact(t,
		`[{"updateComponents":{"components":[{"id":"root","component":"Column","children":["detail"]},{"id":"detail","component":"Button","child":"label"},{"id":"label","component":"Text","text":"查看详情"}]}}]`,
		`[]`, `[{"slotId":"product.detail.primary","componentId":"detail","role":"primary_action","description":"查看详情","contractActionId":"product.detail"}]`)
	if err := provider.RecordDesignSnapshot("tenant-1", "user-1", "session-1", design); err != nil {
		t.Fatal(err)
	}
	result, err := provider.edit.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", AgentID: agenuiextensions.MainAgent},
		Name: ResolveDesignEditName, Arguments: json.RawMessage(`{"query":"把查看详情按钮放大点"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Data), `"component_id":"detail"`) || !strings.Contains(string(result.Data), `"status":"candidates"`) || !strings.Contains(string(result.Data), `"parent_id":"root"`) || !strings.Contains(string(result.Data), `"has_more":false`) {
		t.Fatalf("result = %s", result.Data)
	}
	result, err = provider.edit.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", AgentID: agenuiextensions.MainAgent},
		Name: ResolveDesignEditName, Arguments: json.RawMessage(`{"query":"把查看详情按钮放大点","target_id":"title"}`),
	})
	if err != nil || !strings.Contains(string(result.Data), `"status":"not_found"`) ||
		!strings.Contains(string(result.Data), `"code":"TARGET_NOT_FOUND"`) ||
		!strings.Contains(string(result.Data), `"retryable":true`) {
		t.Fatalf("repairable invalid target result = %s err=%v", result.Data, err)
	}
	if snapshot := provider.ResolvedEditSnapshot("tenant-1", "user-1", "session-1"); snapshot != "" {
		t.Fatalf("invalid target must not persist a resolved selection: %s", snapshot)
	}
	result, err = provider.edit.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", AgentID: agenuiextensions.MainAgent},
		Name: ResolveDesignEditName, Arguments: json.RawMessage(`{"query":"把查看详情按钮放大点","target_id":"product.detail.primary"}`),
	})
	if err != nil || !strings.Contains(string(result.Data), `"component_id":"detail"`) || !strings.Contains(string(result.Data), `"status":"resolved"`) {
		t.Fatalf("selected result = %s err=%v", result.Data, err)
	}
}

func TestPrepareBindingEditCreatesSingleUseTypedRoute(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &designingArtifactStub{values: map[string]string{
		stepartifact.StepDesign: `{"schema_version":"agenui.design.v1"}`,
		stepartifact.StepFinal:  `{"status":"completed"}`,
	}}
	if err := provider.SetPersistence(artifacts); err != nil {
		t.Fatal(err)
	}
	if err := provider.RecordDesignSnapshot("tenant-1", "user-1", "session-1", typedDesignArtifact(t, `[{"updateComponents":{"components":[{"id":"root","component":"Column"}]}}]`, `[]`, `[]`)); err != nil {
		t.Fatal(err)
	}
	result, err := provider.bindingEdit.Invoke(context.Background(), extension.FunctionCall{
		Ctx: extension.Context{
			TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
			AgentID: agenuiextensions.MainAgent,
		},
		Name: PrepareBindingEditName, Arguments: json.RawMessage(`{"query":"把标题改为另一个接口字段"}`),
	})
	if err != nil || !strings.Contains(string(result.Data), `"route":"binding_edit"`) {
		t.Fatalf("result = %s, err = %v", result.Data, err)
	}
	if artifacts.latestStep != stepartifact.StepFinal {
		t.Fatalf("binding edit base selected from %q, want completed Final", artifacts.latestStep)
	}
	query, baseRunID, ok := provider.TakeBindingEditRequest("tenant-1", "user-1", "session-1")
	if !ok || query != "把标题改为另一个接口字段" || baseRunID != "run-base" {
		t.Fatalf("query = %q, base = %q, ok = %v", query, baseRunID, ok)
	}
	if _, _, ok := provider.TakeBindingEditRequest("tenant-1", "user-1", "session-1"); ok {
		t.Fatal("prepared route must be consumed exactly once")
	}
}

func TestResolveDesignEditFallsBackToNewGenerationWithoutBaseDesign(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetPersistence(&designingArtifactStub{}); err != nil {
		t.Fatal(err)
	}
	result, err := provider.edit.Invoke(context.Background(), extension.FunctionCall{
		Ctx: extension.Context{
			TenantID: "tenant-1", UserID: "user-1", SessionID: "session-without-design",
			AgentID: agenuiextensions.MainAgent,
		},
		Name: ResolveDesignEditName, Arguments: json.RawMessage(`{"query":"more-row组件的宽度改成占满整行"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"status":"no_base_design"`, `"next_action":"submit_content_contract"`,
		`"route":"new_generation"`,
	} {
		if !strings.Contains(string(result.Data), required) {
			t.Fatalf("result %s is missing %s", result.Data, required)
		}
	}
	if snapshot := provider.ResolvedEditSnapshot("tenant-1", "user-1", "session-without-design"); snapshot != "" {
		t.Fatalf("fallback must not persist a resolved edit target: %s", snapshot)
	}
}

func TestRestoreDesigningSnapshotRestoresPreflightWithMatchingContract(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	revision := mustContractRevision(t)
	contractJSON, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &designingArtifactStub{values: map[string]string{
		stepartifact.StepContract:  string(contractJSON),
		stepartifact.StepDesign:    typedDesignArtifact(t, `[{"updateComponents":{"components":[{"id":"root","component":"Column"}]}}]`, `[]`, `[]`),
		stepartifact.StepPreflight: `{"items":[],"overall":"ready"}`,
	}}
	if err := provider.SetPersistence(artifacts); err != nil {
		t.Fatal(err)
	}
	if err := provider.RestoreDesigningSnapshot(
		context.Background(), "tenant-1", "user-1", "session-1",
	); err != nil {
		t.Fatal(err)
	}
	_, preflight, ok := provider.DesigningSnapshot("tenant-1", "user-1", "session-1")
	if !ok || preflight != `{"items":[],"overall":"ready"}` {
		t.Fatalf("restored snapshot ok=%v preflight=%s", ok, preflight)
	}
}

func TestSubmitEditContractPersistsHostDerivedAuthorization(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &designingArtifactStub{}
	if err := provider.SetPersistence(artifacts); err != nil {
		t.Fatal(err)
	}
	provider.sessions.contracts[sessionKey("tenant-1", "user-1", "session-1")] = mustContractRevision(t)
	design := typedDesignArtifact(t,
		`[{"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"agenui.org_catalog_0_9"}},{"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"Column","children":["title"]},{"id":"title","component":"Text","text":{"path":"/title"},"styles":{"color":"#222222","font-size":"18px"}}]}},{"version":"v0.9","updateDataModel":{"surfaceId":"default","value":{"title":"标题"}}}]`,
		`[{"slotId":"food.title.primary","componentId":"title","role":"title","description":"标题","contractItemId":"food.title","refKey":"/title"}]`, `[]`)
	artifacts.values = map[string]string{stepartifact.StepDesign: design}
	if err := provider.RecordDesignSnapshot("tenant-1", "user-1", "session-1", design); err != nil {
		t.Fatal(err)
	}
	ctx := extension.Context{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", RunID: "run-edit", AgentID: agenuiextensions.MainAgent}
	if _, err := provider.edit.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: ResolveDesignEditName, Arguments: json.RawMessage(`{"query":"把标题改成红色","target_id":"food.title.primary"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if editContract, resolved, err := provider.EnsureDesignEditContract(context.Background(), harness.Identity{
		TenantID: ctx.TenantID, UserID: ctx.UserID, SessionID: ctx.SessionID, RunID: ctx.RunID,
	}, "把标题改成红色"); err == nil || !strings.Contains(err.Error(), "explicit requested_changes proposal") ||
		editContract != "" || !strings.Contains(resolved, `"status":"resolved"`) {
		t.Fatalf("generic fallback must preserve resolution and require an explicit proposal: contract=%s resolved=%s err=%v", editContract, resolved, err)
	}
	result, err := provider.editContract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitEditContractName,
		Arguments: json.RawMessage(`{"change_scope":"design_update","operation":"upsert","targets":[{"target_id":"food.title.primary","requested_changes":{"styles.color":"#FF0000"}}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifacts.values["edit_contract"] == "" ||
		!strings.Contains(string(result.Data), `"edit_contract_ref":"artifact://edit"`) ||
		!strings.Contains(provider.EditContractSnapshot("tenant-1", "user-1", "session-1"), `"component_id":"title"`) {
		t.Fatalf("result=%s artifact=%s snapshot=%s", result.Data, artifacts.values["edit_contract"], provider.EditContractSnapshot("tenant-1", "user-1", "session-1"))
	}
	firstSnapshot := provider.EditContractSnapshot("tenant-1", "user-1", "session-1")
	if _, err := provider.editContract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitEditContractName,
		Arguments: json.RawMessage(`{"change_scope":"design_update","operation":"upsert","targets":[{"target_id":"food.title.primary","requested_changes":{"styles.color":"#FF0000"}}]}`),
	}); err != nil || provider.EditContractSnapshot("tenant-1", "user-1", "session-1") != firstSnapshot {
		t.Fatalf("idempotent replay changed contract: err=%v before=%s after=%s", err, firstSnapshot, provider.EditContractSnapshot("tenant-1", "user-1", "session-1"))
	}
	ctx.RunID = "run-edit-relative"
	if _, err := provider.edit.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: ResolveDesignEditName, Arguments: json.RawMessage(`{"query":"标题调小点","target_id":"food.title.primary"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.editContract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitEditContractName,
		Arguments: json.RawMessage(`{"change_scope":"design_update","operation":"upsert","targets":[{"target_id":"food.title.primary","requested_changes":{"styles.font-size":"16px"}}]}`),
	}); err != nil {
		t.Fatalf("relative edit proposal was rejected: %v", err)
	}
	relativeSnapshot := provider.EditContractSnapshot("tenant-1", "user-1", "session-1")
	if !strings.Contains(relativeSnapshot, `"path":"styles.font-size"`) || !strings.Contains(relativeSnapshot, `"value":"16px"`) {
		t.Fatalf("relative edit contract = %s", relativeSnapshot)
	}
	ctx.AgentID = agenuiextensions.StyleAgent
	if _, err := provider.editContract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitEditContractName,
		Arguments: json.RawMessage(`{"change_scope":"design_update","operation":"upsert","targets":[{"target_id":"food.title.primary","requested_changes":{"styles.color":"#FF0000"}}]}`),
	}); err == nil || !strings.Contains(err.Error(), "caller is not allowed") {
		t.Fatalf("Style Agent ACL error = %v", err)
	}
}

func TestSubmitEditContractReturnsRepairableValidationErrorToModel(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &designingArtifactStub{}
	if err := provider.SetPersistence(artifacts); err != nil {
		t.Fatal(err)
	}
	key := sessionKey("tenant-1", "user-1", "session-1")
	provider.sessions.contracts[key] = mustContractRevision(t)
	design := typedDesignArtifact(t,
		`[{"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"agenui.org_catalog_0_9"}},{"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"Column","children":["title"]},{"id":"title","component":"Text","text":{"path":"/title"},"styles":{"color":"#222222"}}]}},{"version":"v0.9","updateDataModel":{"surfaceId":"default","value":{"title":"标题"}}}]`,
		`[{"slotId":"food.title.primary","componentId":"title","role":"title","description":"标题","contractItemId":"food.title","refKey":"/title"}]`, `[]`)
	artifacts.values = map[string]string{stepartifact.StepDesign: design}
	if err := provider.RecordDesignSnapshot("tenant-1", "user-1", "session-1", design); err != nil {
		t.Fatal(err)
	}
	result, err := provider.editContract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: extension.Context{
			TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
			RunID: "run-edit", AgentID: agenuiextensions.MainAgent,
		},
		Name:      SubmitEditContractName,
		Arguments: json.RawMessage(`{"change_scope":"design_update","operation":"upsert","targets":[{"target_id":"food.title.primary","requested_changes":{"styles.not-a-real-property":"value"}}]}`),
	})
	if err != nil {
		t.Fatalf("repairable proposal must remain in the model tool context: %v", err)
	}
	data := string(result.Data)
	if !strings.Contains(data, `"code":"EDIT_CONTRACT_REJECTED"`) ||
		!strings.Contains(data, `"retryable":true`) ||
		!strings.Contains(data, `styles.not-a-real-property`) {
		t.Fatalf("result=%s", data)
	}
	if artifacts.values[stepartifact.StepEditContract] != "" {
		t.Fatalf("rejected proposal persisted edit contract: %s", artifacts.values[stepartifact.StepEditContract])
	}
}

func TestSubmitEditContractClassifiesMixedDeclarativeUpsertAtomically(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &designingArtifactStub{}
	if err := provider.SetPersistence(artifacts); err != nil {
		t.Fatal(err)
	}
	key := sessionKey("tenant-1", "user-1", "session-1")
	provider.sessions.contracts[key] = mustContractRevision(t)
	design := typedDesignArtifact(t,
		`[{"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"agenui.org_catalog_0_9"}},{"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"Column","children":["title"]},{"id":"title","component":"Text","text":{"path":"/title"},"styles":{"color":"#222222"}}]}},{"version":"v0.9","updateDataModel":{"surfaceId":"default","value":{"title":"标题"}}}]`,
		`[{"slotId":"food.title.primary","componentId":"title","role":"title","description":"标题","contractItemId":"food.title","refKey":"/title"}]`, `[]`)
	artifacts.values = map[string]string{stepartifact.StepDesign: design}
	if err := provider.RecordDesignSnapshot("tenant-1", "user-1", "session-1", design); err != nil {
		t.Fatal(err)
	}
	result, err := provider.editContract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: extension.Context{
			TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1",
			RunID: "run-edit", AgentID: agenuiextensions.MainAgent,
		},
		Name: SubmitEditContractName,
		Arguments: json.RawMessage(`{
			"change_scope":"design_update","operation":"upsert","targets":[
				{"target_id":"food.title.primary","component":{"id":"title","component":"Text","styles":{"color":"#FF0000"}}},
				{"target_id":"badge","component":{"id":"badge","component":"Text","text":"新品"},"parent_id":"root","after_id":"title"}
			]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := string(result.Data)
	for _, expected := range []string{
		`"created_component_ids":["badge"]`, `"updated_component_ids":["title"]`,
		`"kind":"design_component_insert"`, `"parent_id":"root"`,
	} {
		if !strings.Contains(data, expected) {
			t.Fatalf("result %s is missing %s", data, expected)
		}
	}
	if artifacts.values[stepartifact.StepEditContract] == "" {
		t.Fatal("accepted mixed batch was not persisted")
	}
}

func TestSubmitEditContractRejectsMixedBatchWithInvalidInsertion(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &designingArtifactStub{}
	if err := provider.SetPersistence(artifacts); err != nil {
		t.Fatal(err)
	}
	key := sessionKey("tenant-1", "user-1", "session-1")
	provider.sessions.contracts[key] = mustContractRevision(t)
	design := typedDesignArtifact(t,
		`[{"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"agenui.org_catalog_0_9"}},{"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"Column","children":["title"]},{"id":"title","component":"Text","text":{"path":"/title"},"styles":{"color":"#222222"}}]}},{"version":"v0.9","updateDataModel":{"surfaceId":"default","value":{"title":"标题"}}}]`,
		`[{"slotId":"food.title.primary","componentId":"title","role":"title","description":"标题","contractItemId":"food.title","refKey":"/title"}]`, `[]`)
	artifacts.values = map[string]string{stepartifact.StepDesign: design}
	if err := provider.RecordDesignSnapshot("tenant-1", "user-1", "session-1", design); err != nil {
		t.Fatal(err)
	}
	result, err := provider.editContract.Invoke(context.Background(), extension.FunctionCall{
		Ctx:  extension.Context{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", RunID: "run-edit", AgentID: agenuiextensions.MainAgent},
		Name: SubmitEditContractName,
		Arguments: json.RawMessage(`{
			"change_scope":"design_update","operation":"upsert","targets":[
				{"target_id":"food.title.primary","requested_changes":{"styles.color":"#FF0000"}},
				{"component":{"id":"badge","component":"Text","text":"新品"},"parent_id":"missing"}
			]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Data), `"code":"EDIT_CONTRACT_REJECTED"`) ||
		!strings.Contains(string(result.Data), `parent \"missing\" does not exist`) {
		t.Fatalf("result=%s", result.Data)
	}
	if artifacts.values[stepartifact.StepEditContract] != "" {
		t.Fatalf("invalid mixed batch was partially persisted: %s", artifacts.values[stepartifact.StepEditContract])
	}
}

func mustContractRevision(t *testing.T) contract.Revision {
	t.Helper()
	draft, err := contract.Canonicalize(contract.Draft{Goal: "展示美食", Type: "single", Contents: []contract.ContentItem{{ID: "food.title", Description: "标题", Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := contract.Hash(draft)
	if err != nil {
		t.Fatal(err)
	}
	return contract.Revision{ContractID: "contract-1", Revision: 1, SchemaVersion: contract.SchemaVersion, Status: "confirmed", ContentHash: hash, Draft: draft}
}

func TestPreflightSupportsIndependentStyleEvaluationContract(t *testing.T) {
	provider, err := NewDesigningProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.preflight.Invoke(context.Background(), extension.FunctionCall{
		Ctx:       extension.Context{RunID: "run-eval", TenantID: "tenant-1", UserID: "user-1", SessionID: "session-eval", AgentID: agenuiextensions.StyleAgent},
		Name:      PreflightCapabilitiesName,
		Arguments: json.RawMessage(`{"contract_source":"provided","contract":{"goal":"查看商品","type":"single","contents":[{"id":"product.name","description":"商品名称","required":true}]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Data), `"contract_item_id":"product.name"`) || !strings.Contains(string(result.Data), `"status":"unknown"`) {
		t.Fatalf("result=%s", result.Data)
	}
}

func TestPreflightUsesHostCapturedKnowRAGEvidence(t *testing.T) {
	store := &preflightEvidenceStore{evidence: `{"total":1,"results":[{"path":"/product/detail","response_model":{"properties":{"name":{"type":"string"},"price":{"type":"number"}}}}]}`}
	provider, err := NewDesigningProvider(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := extension.Context{RunID: "run-1", TraceID: "trace-1", TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", AgentID: agenuiextensions.MainAgent}
	if _, err := provider.contract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitContractName,
		Arguments: json.RawMessage(`{"goal":"商品详情","type":"single","contents":[{"id":"product.name","description":"商品名称","required":true},{"id":"product.price","description":"价格","required":true}],"actions":[{"id":"product.detail","description":"查看详情"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	ctx.AgentID = agenuiextensions.StyleAgent
	result, err := provider.preflight.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: PreflightCapabilitiesName, Arguments: json.RawMessage(`{"contract_source":"frozen"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(result.Data)
	if !strings.Contains(text, `"source":"agenui-knowrag"`) ||
		strings.Count(text, `"status":"unknown"`) != 3 ||
		!strings.Contains(text, `"overall":"needs_attention"`) {
		t.Fatalf("preflight result = %s", text)
	}
}

func TestPreflightKeepsProviderFailureUnknown(t *testing.T) {
	store := &preflightEvidenceStore{evidence: `{"query":"目的地榜单","total":0,"results":[],"preflight_status":"unknown","preflight_reason":"knowledge_provider_invalid_response"}`}
	provider, err := NewDesigningProvider(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := extension.Context{RunID: "run-unknown", TraceID: "trace-unknown", TenantID: "tenant-1", UserID: "user-1", SessionID: "session-unknown", AgentID: agenuiextensions.MainAgent}
	if _, err := provider.contract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitContractName,
		Arguments: json.RawMessage(`{"goal":"目的地美食榜单","type":"single","contents":[{"id":"list_title","description":"榜单标题","required":true}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	ctx.AgentID = agenuiextensions.StyleAgent
	result, err := provider.preflight.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: PreflightCapabilitiesName, Arguments: json.RawMessage(`{"contract_source":"frozen"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(result.Data)
	if !strings.Contains(text, `"status":"unknown"`) ||
		!strings.Contains(text, `"overall":"needs_attention"`) ||
		strings.Contains(text, `"status":"unavailable"`) ||
		strings.Contains(text, `"overall":"blocked"`) {
		t.Fatalf("preflight result = %s", text)
	}
}

func TestPreflightRequiresKnowRAGInCurrentTrace(t *testing.T) {
	provider, err := NewDesigningProvider(&preflightEvidenceStore{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := extension.Context{RunID: "run-1", TraceID: "trace-1", TenantID: "tenant-1", UserID: "user-1", SessionID: "session-1", AgentID: agenuiextensions.MainAgent}
	if _, err := provider.contract.Invoke(context.Background(), extension.FunctionCall{
		Ctx: ctx, Name: SubmitContractName,
		Arguments: json.RawMessage(`{"goal":"商品","type":"single","contents":[{"id":"product.name","description":"名称","required":true}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	ctx.AgentID = agenuiextensions.StyleAgent
	_, err = provider.preflight.Invoke(context.Background(), extension.FunctionCall{Ctx: ctx, Name: PreflightCapabilitiesName, Arguments: json.RawMessage(`{"contract_source":"frozen"}`)})
	if err == nil || !strings.Contains(err.Error(), "KnowRAG search was not executed") {
		t.Fatalf("preflight error = %v", err)
	}
}
