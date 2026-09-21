package exportpackage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/harness/sdk"
)

func TestExpandSourcesAndBindingsProducesRuntimeLocalIDs(t *testing.T) {
	t.Parallel()
	exporter, err := New(fakeArtifacts{}, "http://127.0.0.1:18082", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := bindingcontract.Result{SchemaVersion: bindingcontract.ResultSchemaV1, Bindings: []bindingcontract.Binding{
		{RequirementID: "data.title", TargetSlotIDs: []string{"title"}, SourceID: "demo.products@v1", FieldPath: "$.title", RefKey: "/product/title",
			Transforms: []bindingcontract.Transform{{OperatorVersionID: 7, Params: map[string]any{"suffix": "!"}}}},
		{RequirementID: "action.details", TargetSlotIDs: []string{"button"}, SourceID: "demo.products@v1", ActionPath: "$.detail_url", ActionSourceType: "url", ComponentID: "button"},
	}}
	sources, index, err := exporter.expandSources(`{"results":[{"data_source_id":"demo.products","api_version":"v1","path":"/demo/products","method":"get","description":"Products","entity":{"entity_key":"id"},"binding_contract":{"primary_row_list":"$.items","headers":{"X-Demo":"yes"},"params":[{"name":"q","in":"query","type":"string","required":true,"template":"{{query}}"}]}}]}`, result)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].ID != "ds-1" || sources[0].Endpoint != "http://127.0.0.1:18082/demo/products" || sources[0].Method != "GET" {
		t.Fatalf("unexpected expanded sources: %#v", sources)
	}
	if sources[0].Headers["X-Demo"] != "yes" || len(sources[0].Params) != 1 || sources[0].Params[0].Name != "q" {
		t.Fatalf("execution profile was not expanded: %#v", sources[0])
	}
	required := requirements.Set{SchemaVersion: requirements.SchemaVersion, Data: []requirements.DataRequirement{
		{RequirementID: "data.title", WhenMissing: "block"},
	}}
	bindings, actions, operatorIDs, err := expandBindings(result, required, index)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].DataSourceID != "ds-1" || bindings[0].Target != "product.title" || bindings[0].MissingPolicy != "block" {
		t.Fatalf("unexpected expanded bindings: %#v", bindings)
	}
	if len(bindings[0].Transform) != 1 || bindings[0].Transform[0].Params["suffix"] != "!" || len(operatorIDs) != 1 || operatorIDs[0] != 7 {
		t.Fatalf("typed transform was not expanded: bindings=%#v ids=%v", bindings, operatorIDs)
	}
	if len(actions) != 1 || actions[0].Payload["dataSourceId"] != "ds-1" {
		t.Fatalf("unexpected expanded actions: %#v", actions)
	}
}

func TestExpandBindingsUsesEntityRelativeTargetForListItems(t *testing.T) {
	t.Parallel()
	result := bindingcontract.Result{SchemaVersion: bindingcontract.ResultSchemaV1, Bindings: []bindingcontract.Binding{{
		RequirementID: "data.title", TargetSlotIDs: []string{"title"}, SourceID: "demo.products@v1",
		FieldPath: "$.items[*].title", RefKey: "/items[*]/title",
	}}}
	required := requirements.Set{SchemaVersion: requirements.SchemaVersion, Data: []requirements.DataRequirement{{RequirementID: "data.title", WhenMissing: "block"}}}
	bindings, _, _, err := expandBindings(result, required, map[string]string{"demo.products@v1": "ds-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Scope != "list_item" || bindings[0].Target != "title" {
		t.Fatalf("list binding target must be entity-relative: %#v", bindings)
	}
}

func TestExpandOperatorsRequiresPublishedDetails(t *testing.T) {
	t.Parallel()
	exporter := &Exporter{}
	if _, err := exporter.expandOperators(context.Background(), []uint64{7}); err == nil {
		t.Fatal("expected missing operator resolver to fail")
	}
}

func TestDecodeBindingResultUsesTypedSubmission(t *testing.T) {
	t.Parallel()
	result := bindingcontract.Result{SchemaVersion: bindingcontract.ResultSchemaV1, Status: bindingcontract.StatusReady, Bindings: []bindingcontract.Binding{{RequirementID: "data.name"}}}
	plan, err := bindingcontract.EncodeExecutablePlan([]map[string]any{}, []map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bindingcontract.EncodeSubmission(result, plan)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeBindingResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != bindingcontract.ResultSchemaV1 || len(got.Bindings) != 1 || got.Bindings[0].RequirementID != "data.name" {
		t.Fatalf("unexpected decoded result: %#v", got)
	}
}

func TestDecodeBindingResultRejectsNonExecutableStatus(t *testing.T) {
	t.Parallel()
	plan, err := bindingcontract.EncodeExecutablePlan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{bindingcontract.StatusBlocked, bindingcontract.StatusUncertain} {
		raw, err := bindingcontract.EncodeSubmission(bindingcontract.Result{
			SchemaVersion: bindingcontract.ResultSchemaV1,
			Status:        status,
			Issues: []bindingcontract.Issue{{
				RequirementID: "data.name", Code: "NO_SOURCE", Message: "source unavailable",
			}},
		}, plan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeBindingResult(raw); err == nil || !strings.Contains(err.Error(), "is not executable") {
			t.Fatalf("status %s error = %v", status, err)
		}
	}
}

type timedArtifactStore struct {
	steps map[string]stepartifact.LatestStep
}

func (s timedArtifactStore) Load(context.Context, harness.Identity, string) (string, error) {
	return "", nil
}

func (s timedArtifactStore) LatestRunID(_ context.Context, _ harness.Identity, step string) (string, error) {
	return s.steps[step].RunID, nil
}

func (s timedArtifactStore) LatestStep(_ context.Context, _ harness.Identity, step string) (stepartifact.LatestStep, error) {
	return s.steps[step], nil
}

func TestLatestFinalIsCurrentRejectsStaleFinal(t *testing.T) {
	then := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	store := timedArtifactStore{steps: map[string]stepartifact.LatestStep{
		stepartifact.StepFinal:  {RunID: "final", CreatedAt: then},
		stepartifact.StepDesign: {RunID: "edit", CreatedAt: then.Add(time.Second)},
	}}
	err := latestFinalIsCurrent(context.Background(), store, harness.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"})
	if !errors.Is(err, ErrStaleFinal) {
		t.Fatalf("expected stale final rejection, got %v", err)
	}
}

// These fakes are only needed to satisfy New; helper tests do not call them.
type fakeArtifacts struct{ ArtifactStore }
