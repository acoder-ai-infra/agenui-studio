package agentruntime

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

func einoToolInfoFromFrozenDefinition(definition ModelToolDefinition, extra map[string]any) (*schema.ToolInfo, error) {
	if definition.Name == "" || len(definition.Schema) == 0 || !json.Valid(definition.Schema) {
		return nil, fmt.Errorf("%w: invalid frozen tool=%q", ErrEinoToolProxyInfoMissing, definition.Name)
	}
	var inputSchema jsonschema.Schema
	if err := json.Unmarshal(definition.Schema, &inputSchema); err != nil {
		return nil, fmt.Errorf("%w: invalid frozen tool=%q: %v", ErrEinoToolProxyInfoMissing, definition.Name, err)
	}
	return &schema.ToolInfo{
		Name: definition.Name, Desc: definition.Description,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&inputSchema), Extra: extra,
	}, nil
}

func governedDefinitionsForRefs(refs []string, definitions []ModelToolDefinition) ([]ModelToolDefinition, error) {
	byName := make(map[string]ModelToolDefinition, len(definitions))
	for _, definition := range definitions {
		if _, duplicate := byName[definition.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate frozen tool=%q", ErrEinoToolProxyInfoMissing, definition.Name)
		}
		byName[definition.Name] = definition
	}
	selected := make([]ModelToolDefinition, 0, len(refs))
	for _, ref := range refs {
		name, version := splitVersionedRef(ref)
		if version == "" {
			return nil, fmt.Errorf("%w: exact tool ref required=%s", ErrEinoToolProxyInfoMissing, ref)
		}
		definition, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("%w: unresolved ref=%s", ErrEinoToolProxyInfoMissing, ref)
		}
		selected = append(selected, definition)
	}
	return selected, nil
}

func validatedLegacyEinoToolInfos(refs []string, resolved []EinoResolvedTool) (map[string]*schema.ToolInfo, error) {
	authorized := stringSet(refs)
	byRef := make(map[string]*schema.ToolInfo, len(resolved))
	for _, item := range resolved {
		if item.Ref == "" || item.Info == nil || item.Info.Name == "" {
			return nil, ErrEinoToolProxyInfoMissing
		}
		if _, ok := authorized[item.Ref]; !ok {
			return nil, fmt.Errorf("%w: unauthorized ref=%s", ErrEinoToolProxyInfoMissing, item.Ref)
		}
		if _, duplicate := byRef[item.Ref]; duplicate {
			return nil, fmt.Errorf("%w: duplicate ref=%s", ErrEinoToolProxyInfoMissing, item.Ref)
		}
		name, _ := splitVersionedRef(item.Ref)
		if item.Info.Name != name {
			return nil, fmt.Errorf("%w: ref=%s schema_name=%s", ErrEinoToolProxyInfoMissing, item.Ref, item.Info.Name)
		}
		byRef[item.Ref] = item.Info
	}
	return byRef, nil
}

// frozenModelToolDefinitions treats Eino ToolInfo as a selection only. Schema
// 与描述必须来自本轮已冻结的 ModelContextPackage，不能在模型调用前再次读取实时目录。
func frozenModelToolDefinitions(pkg ModelContextPackage, tools []*schema.ToolInfo) ([]ModelToolDefinition, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	if pkg.Capabilities.ToolSnapshot == nil && len(pkg.Capabilities.MCPSnapshots) == 0 {
		return modelToolDefinitionsFromEino(tools)
	}
	available := make(map[string]ModelToolDefinition, len(pkg.Capabilities.ToolDefinitions))
	for _, definition := range pkg.Capabilities.ToolDefinitions {
		if definition.Name == "" || len(definition.Schema) == 0 || !json.Valid(definition.Schema) {
			return nil, fmt.Errorf("%w: invalid frozen tool=%q", ErrEinoToolProxyInfoMissing, definition.Name)
		}
		if _, duplicate := available[definition.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate frozen tool=%q", ErrEinoToolProxyInfoMissing, definition.Name)
		}
		available[definition.Name] = definition
	}

	selected := make([]ModelToolDefinition, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, info := range tools {
		if info == nil || info.Name == "" {
			return nil, fmt.Errorf("%w: tool name required", ErrEinoToolProxyInfoMissing)
		}
		if _, duplicate := seen[info.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate selected tool=%q", ErrEinoToolProxyInfoMissing, info.Name)
		}
		definition, ok := available[info.Name]
		if !ok {
			if !einoRuntimeInternalTool(pkg.Run.RuntimeMode, info.Name) {
				return nil, fmt.Errorf("%w: tool=%q is absent from ModelContextPackage", ErrEinoToolProxyInfoMissing, info.Name)
			}
			var err error
			definition, err = modelToolDefinitionFromEino(info)
			if err != nil {
				return nil, err
			}
		}
		seen[info.Name] = struct{}{}
		selected = append(selected, ModelToolDefinition{
			Name: definition.Name, Description: definition.Description,
			Schema: append(json.RawMessage(nil), definition.Schema...),
		})
	}
	return selected, nil
}

func modelToolDefinitionsFromEino(tools []*schema.ToolInfo) ([]ModelToolDefinition, error) {
	converted := make([]ModelToolDefinition, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, info := range tools {
		if info == nil || info.Name == "" {
			return nil, fmt.Errorf("%w: tool name required", ErrEinoToolProxyInfoMissing)
		}
		if _, duplicate := seen[info.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate runtime tool=%q", ErrEinoToolProxyInfoMissing, info.Name)
		}
		definition, err := modelToolDefinitionFromEino(info)
		if err != nil {
			return nil, err
		}
		seen[info.Name] = struct{}{}
		converted = append(converted, definition)
	}
	return converted, nil
}

func modelToolDefinitionFromEino(info *schema.ToolInfo) (ModelToolDefinition, error) {
	definition := ModelToolDefinition{Name: info.Name, Description: info.Desc}
	if info.ParamsOneOf == nil {
		definition.Schema = json.RawMessage(`{"type":"object"}`)
		return definition, nil
	}
	inputSchema, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		return ModelToolDefinition{}, fmt.Errorf("%w: runtime tool=%q: %v", ErrEinoToolProxyInfoMissing, info.Name, err)
	}
	definition.Schema, err = json.Marshal(inputSchema)
	if err != nil {
		return ModelToolDefinition{}, fmt.Errorf("%w: runtime tool=%q: %v", ErrEinoToolProxyInfoMissing, info.Name, err)
	}
	return definition, nil
}

func einoRuntimeInternalTool(runtimeMode, name string) bool {
	switch RuntimeMode(runtimeMode) {
	case RuntimeModeDeepAgent:
		return name == "task" || name == "write_todos" || name == "exit" || name == "transfer_to_agent"
	case RuntimeModePlanExecute:
		return name == "plan" || name == "respond" || name == "exit" || name == "transfer_to_agent"
	default:
		return name == "exit" || name == "transfer_to_agent"
	}
}

// IsRuntimeReservedToolName reports model-visible names owned by a managed
// Runtime. Registry and capability assembly use it to reject collisions early.
func IsRuntimeReservedToolName(runtime RuntimeSpec, name string) bool {
	if runtime.Type != RuntimeTypeEino && runtime.Type != RuntimeTypeAuto && runtime.Preferred != RuntimeTypeEino {
		usesEino := false
		for _, candidate := range runtime.Candidates {
			if candidate == RuntimeTypeEino {
				usesEino = true
				break
			}
		}
		if !usesEino {
			return false
		}
	}
	return einoRuntimeInternalTool(string(runtime.Mode), name)
}
