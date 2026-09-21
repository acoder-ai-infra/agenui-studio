package materializer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMaterializePassesFieldsAndAddsActionRuntimeReference(t *testing.T) {
	design := `[{"updateDataModel":{"value":{"title":"demo"}}},{"updateComponents":{"components":[{"id":"open","component":"Button"}]}}]`
	plan := `{"schema_version":"agenui.executable-binding/v1","field_mappings":[{"refKey":"/title","sourceKey":"$.title"}],"action_mappings":[{"componentId":"open","actionSourceType":"url","urlPath":"$.url"}]}`
	output, err := Materialize(design, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Plan, `"refKey":"/__actions/open/payload"`) || !strings.Contains(output.Plan, `"sourceKey":"$.url"`) {
		t.Fatalf("plan = %s", output.Plan)
	}
	var messages []map[string]any
	if json.Unmarshal([]byte(output.Result), &messages) != nil {
		t.Fatal("invalid result")
	}
	components := messages[1]["updateComponents"].(map[string]any)["components"].([]any)
	action := components[0].(map[string]any)["action"].(map[string]any)
	if action["functionCall"].(map[string]any)["call"] != "openUrl" {
		t.Fatalf("action = %#v", action)
	}
}

func TestMaterializeRejectsMissingActionComponent(t *testing.T) {
	_, err := Materialize(`[{"updateDataModel":{"value":{}}},{"updateComponents":{"components":[]}}]`,
		`{"schema_version":"agenui.executable-binding/v1","field_mappings":[],"action_mappings":[{"componentId":"missing","actionSourceType":"event","eventPath":"$.action"}]}`)
	if err == nil {
		t.Fatal("expected missing component error")
	}
}

func TestMaterializeUsesFrozenContractActionAsEventName(t *testing.T) {
	design := `[{"updateDataModel":{"value":{}}},{"updateComponents":{"components":[{"id":"confirm","component":"Button"}]}}]`
	plan := `{"schema_version":"agenui.executable-binding/v1","field_mappings":[],"action_mappings":[{"componentId":"confirm","actionSourceType":"event","eventPath":"$.actions.confirm","eventName":"confirm_submit"}]}`
	output, err := Materialize(design, plan)
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	if err := json.Unmarshal([]byte(output.Result), &messages); err != nil {
		t.Fatal(err)
	}
	components := messages[1]["updateComponents"].(map[string]any)["components"].([]any)
	event := components[0].(map[string]any)["action"].(map[string]any)["event"].(map[string]any)
	if event["name"] != "confirm_submit" {
		t.Fatalf("event = %#v", event)
	}
}
