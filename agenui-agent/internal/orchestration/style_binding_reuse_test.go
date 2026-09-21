package orchestration

import "testing"

func TestTypedBindingSubmissionDrivesReuseChecks(t *testing.T) {
	binding := `{"schema_version":"agenui.binding-submission/v1","result":{"schema_version":"agenui.bind-result/v1","status":"ready","bindings":[{"requirement_id":"data.title","target_slot_ids":["title"],"source_id":"demo@v1","knowledge_id":"demo@sha256:test","field_path":"$.name","ref_key":"/title"}]},"plan":{"schema_version":"agenui.executable-binding/v1","field_mappings":[{"refKey":"/title","sourceKey":"$.name"}],"action_mappings":[]}}`
	if !hasBindingDecisions(binding) {
		t.Fatal("typed binding submission was not recognized")
	}
	if bindingUsesTurnLocalSources(binding) {
		t.Fatal("ordinary typed binding was treated as turn-local")
	}
}

func TestEditContractPreservesBinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "style edit", raw: `{"schema_version":"edit_contract.v2","change_scope":"design_update"}`, want: true},
		{name: "binding edit", raw: `{"schema_version":"edit_contract.v2","change_scope":"binding_update"}`, want: false},
		{name: "invalid", raw: `{`, want: false},
		{name: "missing", raw: ``, want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := editContractPreservesBinding(test.raw); got != test.want {
				t.Fatalf("editContractPreservesBinding() = %v, want %v", got, test.want)
			}
		})
	}
}
