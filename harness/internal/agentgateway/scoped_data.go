package agentgateway

import (
	"encoding/json"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// projectScopedData applies the target Agent's frozen data-passing allowlist.
// The default is deny: a child Agent never inherits arbitrary parent Run data.
func projectScopedData(input agentruntime.ScopedData, target agentruntime.AgentDefinition) agentruntime.ScopedData {
	allowed := make(map[string]struct{}, len(target.DataPassing.ScopedDataKeys))
	for _, key := range target.DataPassing.ScopedDataKeys {
		if key != "" {
			allowed[key] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return agentruntime.ScopedData{}
	}
	out := agentruntime.ScopedData{Run: make(map[string]agentruntime.ScopedDataItem)}
	copyAllowed := func(values map[string]agentruntime.ScopedDataItem) {
		for key, value := range values {
			if _, ok := allowed[key]; !ok {
				continue
			}
			value.Value = append(json.RawMessage(nil), value.Value...)
			out.Run[key] = value
		}
	}
	copyAllowed(input.Run)
	// Agent-scoped values are visible only to their exact target Agent. They
	// override same-key Run values because they are the narrower scope.
	copyAllowed(input.Agents[target.AgentID])
	if len(out.Run) == 0 {
		return agentruntime.ScopedData{}
	}
	return out
}
