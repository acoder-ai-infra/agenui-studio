package requirements

import "testing"

func TestParseSlotsProjectsWorkspaceDeclarations(t *testing.T) {
	required := true
	fields, actions, err := ParseSlots(
		[]map[string]any{
			{"slotId": "name.field.1234", "refKey": "/items[*]/name", "role": "title", "required": required, "contractItemId": "product.name", "valueType": "string"},
			{"slotId": "title.field.5678", "refKey": "/title"},
		},
		[]map[string]any{{"slotId": "detail.action.1234", "role": "primary_action", "contractActionId": "product.detail"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields[0].SlotID != "name.field.1234" || fields[0].RefKey != "/items[*]/name" ||
		fields[0].ContractItemID != "product.name" || fields[0].Required == nil || !*fields[0].Required {
		t.Fatalf("fields = %#v", fields)
	}
	if len(actions) != 1 || actions[0].SlotID != "detail.action.1234" || actions[0].ContractActionID != "product.detail" {
		t.Fatalf("actions = %#v", actions)
	}
}
