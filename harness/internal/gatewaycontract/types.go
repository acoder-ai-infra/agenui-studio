// Package gatewaycontract holds the minimal protocol-neutral Agent Gateway
// types shared by Registry, Runtime adapters and Agent Gateway. Plugin
// implementations and execution policy remain owned by agentgateway.
package gatewaycontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// SubAgentProviderKind identifies how a gateway-routed sub-agent target is
// executed. Runtime-internal agents never reach this contract.
type SubAgentProviderKind string

const (
	// SubAgentProviderLocalAgent runs a registered platform Agent as an
	// independent scheduled child Run through the local provider.
	SubAgentProviderLocalAgent SubAgentProviderKind = "local_agent"
	// SubAgentProviderRemoteA2A calls a remote Agent over the A2A protocol.
	SubAgentProviderRemoteA2A SubAgentProviderKind = "remote_a2a"
)

// IsGatewayRouted reports whether the kind is routed through Agent Gateway.
func (k SubAgentProviderKind) IsGatewayRouted() bool {
	return k == SubAgentProviderLocalAgent || k == SubAgentProviderRemoteA2A
}

// GatewayPluginConfig is one author-configured plugin appended after the fixed
// core chain. Config is an object when present; Registry freezes it as
// canonical JSON and Agent Gateway validates it against the trusted plugin.
type GatewayPluginConfig struct {
	PluginID string          `json:"plugin_id"`
	Config   json.RawMessage `json:"config,omitempty"`
}

// Clone returns a deep copy so request-scoped plugin code cannot mutate the
// Registry-owned configuration snapshot.
func (c GatewayPluginConfig) Clone() GatewayPluginConfig {
	c.Config = append(json.RawMessage(nil), c.Config...)
	return c
}

// CanonicalizePluginConfig validates an object config and recursively
// canonicalizes it. Registry and Gateway share this implementation so the
// config snapshot hash and the runtime binding cannot interpret bytes
// differently.
func CanonicalizePluginConfig(input json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return json.RawMessage(`{}`), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("plugin config contains trailing JSON")
		}
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, errors.New("plugin config must be an object")
	}
	return json.Marshal(object)
}
