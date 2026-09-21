package toolgateway

import "testing"

func TestValidateFrozenSourceBinding(t *testing.T) {
	registry := &ToolDefinition{Name: "search", Version: "v1", Type: ToolTypeFunction}
	mcp := &ToolDefinition{Name: "places", Version: "v2", Type: ToolTypeMCP, MCP: &MCPToolSpec{
		ServerID: "server_1", SnapshotID: "snapshot_1", MCPToolName: "remote_places_lookup",
	}}
	httpTool := &ToolDefinition{Name: "tenant_lookup", Version: "http-v1", Type: ToolTypeHTTP, HTTP: &HTTPToolSpec{
		Method: "GET", URL: "https://tools.example.test/lookup",
	}, Metadata: map[string]string{MetadataHarnessSnapshotRef: "sha256:def"}}
	tests := []struct {
		name     string
		req      ToolCallRequest
		def      *ToolDefinition
		required bool
		wantErr  bool
	}{
		{name: "legacy development call", req: ToolCallRequest{}, def: registry},
		{name: "production missing binding", req: ToolCallRequest{}, def: registry, required: true, wantErr: true},
		{name: "registry exact", req: sourceBoundRequest("search", "tool_registry", "search@v1", ""), def: registry, required: true},
		{name: "registry floating version", req: sourceBoundRequest("search", "tool_registry", "search", ""), def: registry, required: true, wantErr: true},
		{name: "mcp exact with registry alias", req: sourceBoundRequest("places", "mcp", "server_1", "snapshot_1"), def: mcp, required: true},
		{name: "mcp stale snapshot", req: sourceBoundRequest("places", "mcp", "server_1", "snapshot_old"), def: mcp, required: true, wantErr: true},
		{name: "http tool exact", req: sourceBoundRequest("tenant_lookup", "http_tool", "tenant_lookup", "sha256:def"), def: httpTool, required: true},
		{name: "http tool stale definition", req: sourceBoundRequest("tenant_lookup", "http_tool", "tenant_lookup", "sha256:old"), def: httpTool, required: true, wantErr: true},
		{name: "http tool resolved to registry definition", req: sourceBoundRequest("tenant_lookup", "http_tool", "tenant_lookup", "sha256:def"), def: registry, required: true, wantErr: true},
		{name: "skill unsupported", req: sourceBoundRequest("skill_tool", "skill", "skill_1", "snapshot_1"), def: registry, required: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFrozenSourceBinding(tt.req, tt.def, tt.required)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tt.wantErr)
			}
			if err != nil && !IsErrorType(err, ErrorTypePermissionDenied) {
				t.Fatalf("error type = %v", err)
			}
		})
	}
}

func sourceBoundRequest(toolName, source, sourceRef, snapshotRef string) ToolCallRequest {
	return ToolCallRequest{ToolName: toolName, Metadata: map[string]string{
		MetadataHarnessSource: source, MetadataHarnessSourceRef: sourceRef, MetadataHarnessSnapshotRef: snapshotRef,
	}}
}
