package toolgateway

import "fmt"

func validateFrozenSourceBinding(req ToolCallRequest, def *ToolDefinition, required bool) error {
	source := req.Metadata[MetadataHarnessSource]
	sourceRef := req.Metadata[MetadataHarnessSourceRef]
	snapshotRef := req.Metadata[MetadataHarnessSnapshotRef]
	if source == "" && !required {
		return nil
	}
	deny := func(message string) error {
		return NewToolError(ErrorTypePermissionDenied, message, false, nil)
	}
	switch source {
	case "tool_registry":
		expected := def.Name
		if def.Version != "" {
			expected += "@" + def.Version
		}
		if sourceRef != expected || snapshotRef != "" {
			return deny("frozen registry tool binding does not match resolved definition")
		}
	case "mcp":
		if def.Type != ToolTypeMCP || def.MCP == nil {
			return deny("frozen MCP binding resolved to a non-MCP tool")
		}
		if sourceRef == "" || snapshotRef == "" || sourceRef != def.MCP.ServerID || (def.MCP.SnapshotID != "runtime-bound" && snapshotRef != def.MCP.SnapshotID) {
			return deny("frozen MCP server or snapshot does not match resolved definition")
		}
	case "http_tool":
		if def.Type != ToolTypeHTTP || def.HTTP == nil {
			return deny("frozen HTTP tool binding resolved to a non-HTTP tool")
		}
		if sourceRef == "" || snapshotRef == "" || sourceRef != def.Name || snapshotRef != def.Metadata[MetadataHarnessSnapshotRef] {
			return deny("frozen HTTP tool source or definition does not match resolved definition")
		}
	case "skill":
		return deny("skill tool execution is not supported by Tool Gateway P0")
	case "":
		return deny("frozen tool source binding is required")
	default:
		return deny(fmt.Sprintf("unsupported frozen tool source %q", source))
	}
	return nil
}
