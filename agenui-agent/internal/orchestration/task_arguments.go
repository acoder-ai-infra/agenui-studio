package orchestration

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

func setTaskDescription(
	arguments json.RawMessage,
	description string,
) json.RawMessage {
	var decoded map[string]json.RawMessage
	if json.Unmarshal(arguments, &decoded) != nil {
		return arguments
	}
	encodedDescription, err := json.Marshal(description)
	if err != nil {
		return arguments
	}
	decoded["description"] = encodedDescription
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return arguments
	}
	return encoded
}

func compactSearchResult(value string) string {
	normalized := normalizedJSONText(value)
	var source struct {
		Query   string           `json:"query"`
		APIs    []map[string]any `json:"apis"`
		Results []map[string]any `json:"results"`
		Total   int              `json:"total"`
	}
	if json.NewDecoder(strings.NewReader(normalized)).Decode(&source) != nil ||
		(len(source.APIs) == 0 && len(source.Results) == 0) {
		return value
	}
	if len(source.APIs) > 0 {
		apis := make([]map[string]any, 0, len(source.APIs))
		for _, api := range source.APIs {
			apis = append(apis, searchSelectionAPI(api))
		}
		compacted := map[string]any{
			"query": source.Query,
			"total": source.Total,
			"apis":  apis,
		}
		encoded, err := json.Marshal(compacted)
		if err != nil {
			return value
		}
		return string(encoded)
	}
	result := source.Results[0]
	compacted := map[string]any{
		"query": source.Query,
		"total": source.Total,
		"apis":  []map[string]any{searchSelectionAPI(result)},
	}
	encoded, err := json.Marshal(compacted)
	if err != nil {
		return value
	}
	return string(encoded)
}

func searchSelectionAPI(source map[string]any) map[string]any {
	project := source["project"]
	if project == nil {
		project = source["project_name"]
	}
	return map[string]any{
		"path":               source["path"],
		"method":             source["method"],
		"description":        source["description"],
		"project":            project,
		"score":              source["score"],
		"result_id":          source["result_id"],
		"data_source_id":     source["data_source_id"],
		"api_version":        source["api_version"],
		"knowledge_revision": source["knowledge_revision"],
		"content_hash":       source["content_hash"],
	}
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func normalizedJSONText(value string) string {
	normalized := strings.TrimSpace(value)
	// SDK/MCP tool results may cross more than one string-valued envelope
	// before reaching the task interceptor. Unwrap a bounded number of layers
	// so the binder always receives the JSON object instead of an escaped JSON
	// string. The bound keeps malformed/adversarial input from looping forever.
	for range 8 {
		if len(normalized) == 0 || normalized[0] != '"' {
			break
		}
		var unwrapped string
		if json.NewDecoder(strings.NewReader(normalized)).Decode(&unwrapped) != nil {
			break
		}
		normalized = strings.TrimSpace(unwrapped)
	}
	return normalized
}

func searchTool(name string) bool {
	return name == "search_agenui_apis" || name == "search_developer_apis"
}

func stylePreflightUnknownReceipt(query string) (full string, compact string) {
	receipt := map[string]any{
		"query":            strings.TrimSpace(query),
		"total":            0,
		"results":          []any{},
		"preflight_status": "unknown",
		"preflight_reason": "knowledge_provider_invalid_response",
	}
	encoded, _ := json.Marshal(receipt)
	return string(encoded), string(encoded)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func taskStep(call extension.ToolCallInfo) (string, string, bool) {
	if call.Name != "task" {
		return "", "", false
	}
	var arguments struct {
		SubagentType string `json:"subagent_type"`
	}
	if json.Unmarshal(call.Arguments, &arguments) != nil {
		return "", "", false
	}
	switch arguments.SubagentType {
	case "agenui_style":
		return "design", "agenui_design", true
	case "agenui_binder":
		return "bind_apis", "binding_result", true
	default:
		return "", "", false
	}
}

func taskSubagent(call extension.ToolCallInfo) string {
	if call.Name != "task" {
		return ""
	}
	var payload struct {
		SubagentType string `json:"subagent_type"`
	}
	if json.Unmarshal(call.Arguments, &payload) != nil {
		return "<invalid>"
	}
	return strings.TrimSpace(payload.SubagentType)
}

func enrichDesignArguments(arguments json.RawMessage, contractJSON string) json.RawMessage {
	var payload map[string]any
	if json.Unmarshal(arguments, &payload) != nil {
		return arguments
	}
	description, _ := payload["description"].(string)
	payload["description"] = strings.TrimSpace(description) +
		"\n\n以下是 Host 已校验并冻结的 Content Contract。只读取，不得修改或重新生成：\n" +
		contractJSON
	encoded, err := json.Marshal(payload)
	if err != nil {
		return arguments
	}
	return encoded
}

func enrichDesignTargetArguments(arguments json.RawMessage, targetJSON string) json.RawMessage {
	var payload map[string]any
	if json.Unmarshal(arguments, &payload) != nil {
		return arguments
	}
	description, _ := payload["description"].(string)
	payload["description"] = strings.TrimSpace(description) +
		"\n\n以下是模型从 Host 提供的真实组件候选中选定、并由 Host 校验存在的编辑目标。只允许修改 target.component_id 上用户明确要求且出现在 editable_paths 中的属性；不得改写其它组件：\n" + targetJSON
	encoded, err := json.Marshal(payload)
	if err != nil {
		return arguments
	}
	return encoded
}

func enrichEditContractArguments(arguments json.RawMessage, editContract string) json.RawMessage {
	var payload map[string]any
	if json.Unmarshal(arguments, &payload) != nil {
		return arguments
	}
	description, _ := payload["description"].(string)
	payload["description"] = strings.TrimSpace(description) +
		"\n\n以下是 Host 已持久化的编辑契约。Workspace 会原子应用 target_set 中冻结的值；不要重写其它组件、槽位或数据：\n" + editContract
	encoded, err := json.Marshal(payload)
	if err != nil {
		return arguments
	}
	return encoded
}
