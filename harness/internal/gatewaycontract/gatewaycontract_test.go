package gatewaycontract

import (
	"encoding/json"
	"testing"
)

func TestSubAgentProviderKindGatewayRouting(t *testing.T) {
	tests := map[SubAgentProviderKind]bool{
		SubAgentProviderLocalAgent: true,
		SubAgentProviderRemoteA2A:  true,
		"runtime_internal":         false,
		"":                         false,
	}
	for kind, want := range tests {
		if got := kind.IsGatewayRouted(); got != want {
			t.Fatalf("IsGatewayRouted(%q) = %t, want %t", kind, got, want)
		}
	}
}

func TestGatewayPluginConfigCloneDeepCopiesConfig(t *testing.T) {
	original := GatewayPluginConfig{PluginID: "basic_validator", Config: json.RawMessage(`{"mode":"strict"}`)}
	cloned := original.Clone()
	cloned.Config[2] = 'x'
	if string(original.Config) != `{"mode":"strict"}` {
		t.Fatalf("Clone() shared config bytes: %s", original.Config)
	}
}

func TestCanonicalizePluginConfigNormalizesNestedObjects(t *testing.T) {
	canonical, err := CanonicalizePluginConfig(json.RawMessage(`{"outer":{"z":2,"a":1},"enabled":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(canonical); got != `{"enabled":true,"outer":{"a":1,"z":2}}` {
		t.Fatalf("canonical config=%s", got)
	}
	for _, invalid := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`null`), json.RawMessage(`{} {}`)} {
		if _, err := CanonicalizePluginConfig(invalid); err == nil {
			t.Fatalf("invalid config accepted: %s", invalid)
		}
	}
}
