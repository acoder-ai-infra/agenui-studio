package toolgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestStaticRegistryGetByNameAndVersion(t *testing.T) {
	registry := NewStaticRegistry([]ToolDefinition{{
		Name:        "search_kb",
		Version:     "v1",
		Type:        ToolTypeFunction,
		Description: "search knowledge base",
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}}}`),
		RiskLevel:   RiskLow,
		Timeout:     2 * time.Second,
		Visibility:  observability.VisibilityUserVisible,
		Function:    &FunctionToolSpec{HandlerName: "search"},
	}})

	def, err := registry.Get(context.Background(), "search_kb", "v1")
	if err != nil {
		t.Fatalf("get registered tool: %v", err)
	}
	if def.Name != "search_kb" || def.Version != "v1" || def.Type != ToolTypeFunction {
		t.Fatalf("unexpected definition: %#v", def)
	}
}

func TestStaticRegistryListEnabledSortedSkipsDisabledAndInvalid(t *testing.T) {
	valid := func(name, version string) ToolDefinition {
		return ToolDefinition{
			Name: name, Version: version, Type: ToolTypeFunction,
			InputSchema: json.RawMessage(`{"type":"object"}`),
			RiskLevel:   RiskLow, Visibility: observability.VisibilityUserVisible,
			Function: &FunctionToolSpec{HandlerName: "h"},
		}
	}
	disabled := valid("beta_tool", "v1")
	disabled.Disabled = true
	invalid := valid("broken_tool", "v1")
	invalid.Function = nil // no executor spec -> validation error, must be skipped

	registry := NewStaticRegistry([]ToolDefinition{
		valid("search_kb", "v2"),
		valid("search_kb", "v1"),
		valid("calculator", "v1"),
		disabled,
		invalid,
	})

	got := registry.List(context.Background())
	type nv struct{ name, version string }
	var refs []nv
	for _, def := range got {
		refs = append(refs, nv{def.Name, def.Version})
	}
	want := []nv{
		{"calculator", "v1"},
		{"search_kb", "v1"},
		{"search_kb", "v2"},
	}
	if len(refs) != len(want) {
		t.Fatalf("unexpected list length: got %d (%#v), want %d", len(refs), refs, len(want))
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("unexpected order at %d: got %#v, want %#v", i, refs[i], want[i])
		}
	}
}

func TestStaticRegistryListReturnsClones(t *testing.T) {
	registry := NewStaticRegistry([]ToolDefinition{{
		Name: "search_kb", Version: "v1", Type: ToolTypeFunction,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		RiskLevel:   RiskLow, Visibility: observability.VisibilityUserVisible,
		Function: &FunctionToolSpec{HandlerName: "h"},
	}})
	got := registry.List(context.Background())
	if len(got) != 1 {
		t.Fatalf("expected one tool, got %d", len(got))
	}
	got[0].Name = "mutated"
	again := registry.List(context.Background())
	if again[0].Name != "search_kb" {
		t.Fatalf("List must return clones; registry mutated to %q", again[0].Name)
	}
}

func TestStaticRegistryReturnsToolNotFound(t *testing.T) {
	registry := NewStaticRegistry(nil)

	_, err := registry.Get(context.Background(), "missing", "v1")
	if !IsErrorType(err, ErrorTypeToolNotFound) {
		t.Fatalf("expected tool_not_found, got %v", err)
	}
}

func TestStaticRegistryResolveSnapshotIsStable(t *testing.T) {
	registry := NewStaticRegistry([]ToolDefinition{{
		Name:        "search_kb",
		Version:     "v1",
		Type:        ToolTypeFunction,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		RiskLevel:   RiskLow,
		Visibility:  observability.VisibilityUserVisible,
		Function:    &FunctionToolSpec{HandlerName: "search"},
	}})

	req := ResolveToolSnapshotRequest{
		TenantID: "tenant-a",
		AgentID:  "agent-a",
		ToolRefs: []ToolRef{{Name: "search_kb", Version: "v1"}},
		Trace: observability.TraceContext{
			TraceID: "trace-1",
		},
	}
	first, err := registry.ResolveSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("resolve first snapshot: %v", err)
	}
	second, err := registry.ResolveSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("resolve second snapshot: %v", err)
	}
	if first.CapabilityHash == "" || first.PolicyHash == "" || first.SnapshotID == "" {
		t.Fatalf("snapshot should contain ids and hashes: %#v", first)
	}
	if first.CapabilityHash != second.CapabilityHash || first.PolicyHash != second.PolicyHash || first.SnapshotID != second.SnapshotID {
		t.Fatalf("snapshot should be stable, first=%#v second=%#v", first, second)
	}
}

func TestStaticRegistryRejectsInvalidDefinitions(t *testing.T) {
	validSchema := json.RawMessage(`{"type":"object"}`)
	cases := []struct {
		name string
		def  ToolDefinition
	}{
		{
			name: "missing name",
			def: ToolDefinition{
				Version: "v1", Type: ToolTypeFunction, InputSchema: validSchema,
				Visibility: observability.VisibilityUserVisible, Function: &FunctionToolSpec{HandlerName: "missing"},
			},
		},
		{
			name: "missing version",
			def: ToolDefinition{
				Name: "missing", Type: ToolTypeFunction, InputSchema: validSchema,
				Visibility: observability.VisibilityUserVisible, Function: &FunctionToolSpec{HandlerName: "missing"},
			},
		},
		{
			name: "missing schema",
			def: ToolDefinition{
				Name: "missing", Version: "v1", Type: ToolTypeFunction,
				Visibility: observability.VisibilityUserVisible, Function: &FunctionToolSpec{HandlerName: "missing"},
			},
		},
		{
			name: "incomplete function",
			def: ToolDefinition{
				Name: "function", Version: "v1", Type: ToolTypeFunction, InputSchema: validSchema,
				Visibility: observability.VisibilityUserVisible, Function: &FunctionToolSpec{},
			},
		},
		{
			name: "mismatched spec",
			def: ToolDefinition{
				Name: "wrong", Version: "v1", Type: ToolTypeFunction, InputSchema: validSchema,
				Visibility: observability.VisibilityUserVisible,
				HTTP:       &HTTPToolSpec{Method: http.MethodPost, URL: "https://invalid.example", ResponseMode: "json"},
			},
		},
		{
			name: "multiple specs",
			def: ToolDefinition{
				Name: "multiple", Version: "v1", Type: ToolTypeFunction, InputSchema: validSchema,
				Visibility: observability.VisibilityUserVisible,
				Function:   &FunctionToolSpec{HandlerName: "multiple"},
				HTTP:       &HTTPToolSpec{Method: http.MethodPost, URL: "https://invalid.example", ResponseMode: "json"},
			},
		},
		{
			name: "incomplete http",
			def: ToolDefinition{
				Name: "http", Version: "v1", Type: ToolTypeHTTP, InputSchema: validSchema,
				Visibility: observability.VisibilityUserVisible, HTTP: &HTTPToolSpec{Method: http.MethodPost},
			},
		},
		{
			name: "http write below high risk",
			def: ToolDefinition{
				Name: "http-write", Version: "v1", Type: ToolTypeHTTP, InputSchema: validSchema,
				RiskLevel: RiskMedium, Visibility: observability.VisibilityUserVisible,
				HTTP: &HTTPToolSpec{Method: http.MethodPost, URL: "https://invalid.example", ResponseMode: "json", Write: true},
			},
		},
		{
			name: "http mutating method without write declaration",
			def: ToolDefinition{
				Name: "http-put", Version: "v1", Type: ToolTypeHTTP, InputSchema: validSchema,
				RiskLevel: RiskLow, Visibility: observability.VisibilityUserVisible,
				HTTP: &HTTPToolSpec{Method: http.MethodPut, URL: "https://invalid.example", ResponseMode: "json"},
			},
		},
		{
			name: "incomplete mcp",
			def: ToolDefinition{
				Name: "mcp", Version: "v1", Type: ToolTypeMCP, InputSchema: validSchema,
				Visibility: observability.VisibilityUserVisible, MCP: &MCPToolSpec{ServerID: "server-1", MCPToolName: "lookup"},
			},
		},
		{
			name: "invalid visibility",
			def: ToolDefinition{
				Name: "visibility", Version: "v1", Type: ToolTypeFunction, InputSchema: validSchema,
				Visibility: observability.EventVisibility("private"), Function: &FunctionToolSpec{HandlerName: "visibility"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewStaticRegistry([]ToolDefinition{tc.def})
			_, err := registry.Get(context.Background(), tc.def.Name, tc.def.Version)
			if !IsErrorType(err, ErrorTypeInternal) {
				t.Fatalf("definition error = %v", err)
			}
		})
	}
}

func TestStaticRegistryRejectsMCPWithoutSnapshot(t *testing.T) {
	def := ToolDefinition{
		Name:        "mcp_lookup",
		Version:     "v1",
		Type:        ToolTypeMCP,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		RiskLevel:   RiskLow,
		Visibility:  observability.VisibilityUserVisible,
		MCP: &MCPToolSpec{
			ServerID:    "server-a",
			MCPToolName: "lookup",
		},
	}

	_, err := NewStaticRegistry([]ToolDefinition{def}).Get(context.Background(), def.Name, def.Version)
	if !IsErrorType(err, ErrorTypeInternal) {
		t.Fatalf("missing MCP snapshot definition error=%v", err)
	}
}

func TestStaticRegistryAcceptsSchemaRefDefinition(t *testing.T) {
	def := defaultSearchDefinition()
	def.InputSchema = nil
	def.InputSchemaRef = "artifact://tenant-a/schemas/search-v1"
	registry := NewStaticRegistry([]ToolDefinition{def})

	got, err := registry.Get(context.Background(), def.Name, def.Version)
	if err != nil || got.InputSchemaRef != def.InputSchemaRef {
		t.Fatalf("definition=%#v error=%v", got, err)
	}
}

func TestStaticRegistryRejectsDuplicateDefinitions(t *testing.T) {
	def := defaultSearchDefinition()
	registry := NewStaticRegistry([]ToolDefinition{def, def})

	_, err := registry.Get(context.Background(), def.Name, def.Version)
	if !IsErrorType(err, ErrorTypeInternal) {
		t.Fatalf("duplicate definition error = %v", err)
	}
}

func TestStaticRegistryReturnsVersionNotFoundForExistingTool(t *testing.T) {
	registry := NewStaticRegistry([]ToolDefinition{defaultSearchDefinition()})

	_, err := registry.Get(context.Background(), "search_kb", "v2")
	if !IsErrorType(err, ErrorTypeToolVersionNotFound) {
		t.Fatalf("version error = %v", err)
	}
}

func TestDefaultToolOutputPolicyMatchesDocumentedP0Defaults(t *testing.T) {
	got := DefaultToolOutputPolicy()
	if got.MaxInlineBytes != 4096 || got.MaxModelContextBytes != 12000 || got.MaxSSEPreviewBytes != 1024 || got.ArtifactThresholdBytes != 4096 {
		t.Fatalf("numeric defaults = %#v", got)
	}
	if !got.RedactSensitiveFields || !got.RequireOutputSchema || !got.SummarizeWhenTruncated {
		t.Fatalf("boolean defaults = %#v", got)
	}
}

func TestStaticRegistryRejectsRequiredOutputSchemaWhenMissing(t *testing.T) {
	def := defaultSearchDefinition()
	def.ResultPolicy.RequireOutputSchema = true
	registry := NewStaticRegistry([]ToolDefinition{def})

	_, err := registry.Get(context.Background(), def.Name, def.Version)
	if !IsErrorType(err, ErrorTypeInternal) {
		t.Fatalf("missing required output schema error = %v", err)
	}
}

func TestStaticRegistryResolvesSingleVersionWhenRequestVersionIsEmpty(t *testing.T) {
	def := defaultSearchDefinition()
	registry := NewStaticRegistry([]ToolDefinition{def})

	got, err := registry.Get(context.Background(), def.Name, "")
	if err != nil {
		t.Fatalf("resolve unique version: %v", err)
	}
	if got.Version != def.Version {
		t.Fatalf("resolved version = %q, want %q", got.Version, def.Version)
	}

	other := def
	other.Version = "v2"
	registry = NewStaticRegistry([]ToolDefinition{def, other})
	_, err = registry.Get(context.Background(), def.Name, "")
	if !IsErrorType(err, ErrorTypeToolVersionNotFound) {
		t.Fatalf("ambiguous empty version error = %v", err)
	}
}

func TestStaticRegistryPolicyHashCoversRiskRetryResultAndVisibility(t *testing.T) {
	base := defaultSearchDefinition()
	base.Timeout = time.Second
	base.Retry = RetryPolicy{MaxAttempts: 1, Idempotent: true}
	base.ResultPolicy = ToolOutputPolicy{MaxInlineBytes: 4096}
	baseline := resolvePolicyHash(t, base)

	tests := []struct {
		name   string
		mutate func(*ToolDefinition)
	}{
		{name: "risk", mutate: func(def *ToolDefinition) { def.RiskLevel = RiskMedium }},
		{name: "timeout", mutate: func(def *ToolDefinition) { def.Timeout = 2 * time.Second }},
		{name: "retry", mutate: func(def *ToolDefinition) { def.Retry.MaxAttempts = 2 }},
		{name: "permissions", mutate: func(def *ToolDefinition) { def.Permissions.RequiredScopes = []string{"tool:read"} }},
		{name: "result policy", mutate: func(def *ToolDefinition) { def.ResultPolicy.MaxInlineBytes = 8192 }},
		{name: "visibility", mutate: func(def *ToolDefinition) { def.Visibility = observability.VisibilityInternal }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := cloneToolDefinition(base)
			tt.mutate(&changed)
			if got := resolvePolicyHash(t, changed); got == baseline {
				t.Fatalf("policy hash did not change for %s: %s", tt.name, got)
			}
		})
	}
}

func TestBasicSchemaValidatorRejectsUnknownTypeAndUnsupportedKeyword(t *testing.T) {
	validator := BasicSchemaValidator{}
	for _, tt := range []struct {
		name   string
		schema json.RawMessage
		data   json.RawMessage
	}{
		{name: "unknown type", schema: json.RawMessage(`{"type":"uuid"}`), data: json.RawMessage(`"abc"`)},
		{name: "unsupported constraint", schema: json.RawMessage(`{"type":"string","minLength":3}`), data: json.RawMessage(`"x"`)},
		{name: "unsupported dialect", schema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"}`), data: json.RawMessage(`"x"`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validator.Validate(context.Background(), tt.schema, tt.data); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
				t.Fatalf("unsupported schema error = %v", err)
			}
		})
	}

	allowedAnnotations := json.RawMessage(`{"type":"string","title":"Name","description":"display name","default":"a","examples":["a"],"deprecated":false,"readOnly":true,"writeOnly":false,"$comment":"safe annotation"}`)
	if err := validator.Validate(context.Background(), allowedAnnotations, json.RawMessage(`"alice"`)); err != nil {
		t.Fatalf("safe annotations should be accepted: %v", err)
	}
}

func TestJSONSchemaValidatorSupportsProductionKeywords(t *testing.T) {
	validator := NewJSONSchemaValidator(2)
	schema := json.RawMessage(`{
		"type":"object",
		"required":["mode","items"],
		"properties":{
			"mode":{"enum":["fast","safe"]},
			"items":{"type":"array","minItems":1,"items":{"type":"integer","minimum":1}}
		},
		"additionalProperties":false
	}`)
	if err := validator.Validate(context.Background(), schema, json.RawMessage(`{"mode":"safe","items":[1,2]}`)); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"mode":"other","items":[1]}`),
		json.RawMessage(`{"mode":"safe","items":[0]}`),
		json.RawMessage(`{"mode":"safe","items":[]}`),
	} {
		if err := validator.Validate(context.Background(), schema, invalid); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
			t.Fatalf("invalid payload %s error = %v", invalid, err)
		}
	}
}

func TestJSONSchemaValidatorCacheIsConcurrentAndBounded(t *testing.T) {
	validator := NewJSONSchemaValidator(2)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := validator.Validate(context.Background(), json.RawMessage(`{"type":"string","minLength":1}`), json.RawMessage(`"ok"`)); err != nil {
				t.Errorf("Validate() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := len(validator.compiled); got > 2 {
		t.Fatalf("compiled cache size = %d, want <= 2", got)
	}
}

func TestBasicSchemaValidatorEnforcesStandaloneAdditionalProperties(t *testing.T) {
	validator := BasicSchemaValidator{}
	for _, tt := range []struct {
		name   string
		schema json.RawMessage
		data   json.RawMessage
	}{
		{
			name:   "top level",
			schema: json.RawMessage(`{"additionalProperties":false}`),
			data:   json.RawMessage(`{"unexpected":1}`),
		},
		{
			name:   "nested",
			schema: json.RawMessage(`{"type":"object","properties":{"nested":{"additionalProperties":false}}}`),
			data:   json.RawMessage(`{"nested":{"unexpected":1}}`),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validator.Validate(context.Background(), tt.schema, tt.data); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
				t.Fatalf("standalone additionalProperties error = %v", err)
			}
		})
	}
}

func resolvePolicyHash(t *testing.T, def ToolDefinition) string {
	t.Helper()
	snapshot, err := NewStaticRegistry([]ToolDefinition{def}).ResolveSnapshot(context.Background(), ResolveToolSnapshotRequest{
		TenantID: "tenant-a",
		AgentID:  "agent-a",
		ToolRefs: []ToolRef{{Name: def.Name, Version: def.Version}},
	})
	if err != nil {
		t.Fatalf("resolve snapshot: %v", err)
	}
	return snapshot.PolicyHash
}

func TestBasicSchemaValidatorValidatesRequiredScalarFields(t *testing.T) {
	validator := BasicSchemaValidator{}
	schema := json.RawMessage(`{
		"type":"object",
		"required":["query","limit"],
		"properties":{
			"query":{"type":"string"},
			"limit":{"type":"integer"},
			"fresh":{"type":"boolean"}
		},
		"additionalProperties": false
	}`)

	if err := validator.Validate(context.Background(), schema, json.RawMessage(`{"query":"hotel","limit":3,"fresh":true}`)); err != nil {
		t.Fatalf("valid arguments should pass: %v", err)
	}
	if err := validator.Validate(context.Background(), schema, json.RawMessage(`{"query":"hotel"}`)); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
		t.Fatalf("missing required field should fail schema validation, got %v", err)
	}
	if err := validator.Validate(context.Background(), schema, json.RawMessage(`{"query":"hotel","limit":"3"}`)); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
		t.Fatalf("wrong scalar type should fail schema validation, got %v", err)
	}
	if err := validator.Validate(context.Background(), schema, json.RawMessage(`{"query":"hotel","limit":3,"extra":"nope"}`)); !IsErrorType(err, ErrorTypeSchemaValidationFailed) {
		t.Fatalf("additional property should fail schema validation, got %v", err)
	}
}

func TestToolErrorHelpers(t *testing.T) {
	err := NewToolError(ErrorTypePermissionDenied, "agent is not allowed", false, nil)
	if !IsErrorType(err, ErrorTypePermissionDenied) {
		t.Fatalf("expected permission_denied, got %v", err)
	}
	var toolErr *ToolError
	if !AsToolError(err, &toolErr) {
		t.Fatalf("expected AsToolError to unwrap ToolError")
	}
	if toolErr.Retryable {
		t.Fatalf("permission errors should not be retryable")
	}
}
