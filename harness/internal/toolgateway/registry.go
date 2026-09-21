package toolgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// ToolRegistry 是不可变版本目录：已发布 name@version 禁止覆盖；相同主体与能力事实
// 必须生成稳定、内容寻址的 SnapshotID。生产实现用唯一键与 CAS 同时保证两条约束。
type ToolRegistry interface {
	Get(ctx context.Context, name string, version string) (*ToolDefinition, error)
	ResolveSnapshot(ctx context.Context, req ResolveToolSnapshotRequest) (*ToolSnapshot, error)
}

// ToolLister enumerates the catalog's enabled, well-formed tool definitions.
// It is a read surface for management/UI discovery and is intentionally
// separate from ToolRegistry so callers that only need to browse the catalog
// do not gain resolution capabilities.
type ToolLister interface {
	List(ctx context.Context) []ToolDefinition
}

type ResolveToolSnapshotRequest struct {
	TenantID string
	AgentID  string
	ToolRefs []ToolRef
	Trace    observability.TraceContext
}

type ToolRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type ToolSnapshot struct {
	SnapshotID        string
	ToolRefs          []ToolRef
	SchemaArtifactRef string
	CapabilityHash    string
	PolicyHash        string
	CreatedAt         time.Time
}

type StaticRegistry struct {
	mu               sync.RWMutex
	tools            map[string]ToolDefinition
	definitionErrors map[string]error
	toolNames        map[string]struct{}
}

func NewStaticRegistry(definitions []ToolDefinition) *StaticRegistry {
	registry := &StaticRegistry{
		tools:            make(map[string]ToolDefinition, len(definitions)),
		definitionErrors: make(map[string]error),
		toolNames:        make(map[string]struct{}),
	}
	for _, def := range definitions {
		key := toolKey(def.Name, def.Version)
		registry.toolNames[def.Name] = struct{}{}
		if _, exists := registry.tools[key]; exists {
			registry.definitionErrors[key] = NewToolError(
				ErrorTypeInternal,
				fmt.Sprintf("duplicate tool definition: %s@%s", def.Name, def.Version),
				false,
				nil,
			)
			continue
		}
		registry.tools[key] = cloneToolDefinition(def)
		if err := validateToolDefinition(def); err != nil {
			registry.definitionErrors[key] = err
		}
	}
	return registry
}

func (r *StaticRegistry) Get(_ context.Context, name string, version string) (*ToolDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := toolKey(name, version)
	if version == "" {
		key = ""
		for candidateKey, candidate := range r.tools {
			if candidate.Name != name {
				continue
			}
			if key != "" {
				return nil, NewToolError(ErrorTypeToolVersionNotFound, fmt.Sprintf("tool version is ambiguous: %s", name), false, nil)
			}
			key = candidateKey
		}
		if key == "" {
			return nil, NewToolError(ErrorTypeToolNotFound, fmt.Sprintf("tool not found: %s", name), false, nil)
		}
	}
	if err, invalid := r.definitionErrors[key]; invalid {
		return nil, err
	}
	def, ok := r.tools[key]
	if !ok {
		if _, nameExists := r.toolNames[name]; nameExists {
			return nil, NewToolError(ErrorTypeToolVersionNotFound, fmt.Sprintf("tool version not found: %s@%s", name, version), false, nil)
		}
		return nil, NewToolError(ErrorTypeToolNotFound, fmt.Sprintf("tool not found: %s", name), false, nil)
	}
	if def.Disabled {
		return nil, NewToolError(ErrorTypeToolDisabled, fmt.Sprintf("tool disabled: %s", name), false, nil)
	}
	cloned := cloneToolDefinition(def)
	return &cloned, nil
}

// List returns clones of every enabled, valid tool definition, sorted by
// name then version. Disabled tools and definitions that failed validation
// are omitted so the catalog only advertises tools that can actually resolve.
func (r *StaticRegistry) List(_ context.Context) []ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolDefinition, 0, len(r.tools))
	for key, def := range r.tools {
		if def.Disabled {
			continue
		}
		if _, invalid := r.definitionErrors[key]; invalid {
			continue
		}
		out = append(out, cloneToolDefinition(def))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Version < out[j].Version
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func validateToolDefinition(def ToolDefinition) error {
	invalid := func(message string) error {
		return NewToolError(ErrorTypeInternal, fmt.Sprintf("invalid tool definition %s@%s: %s", def.Name, def.Version, message), false, nil)
	}
	if strings.TrimSpace(def.Name) == "" {
		return invalid("name is required")
	}
	if strings.TrimSpace(def.Version) == "" {
		return invalid("version is required")
	}
	if len(strings.TrimSpace(string(def.InputSchema))) == 0 && strings.TrimSpace(def.InputSchemaRef) == "" {
		return invalid("input schema or input schema ref is required")
	}
	if len(def.InputSchema) > 0 && !json.Valid(def.InputSchema) {
		return invalid("input schema must be valid JSON")
	}
	if len(def.OutputSchema) > 0 && !json.Valid(def.OutputSchema) {
		return invalid("output schema must be valid JSON")
	}
	if def.ResultPolicy.RequireOutputSchema && len(strings.TrimSpace(string(def.OutputSchema))) == 0 && strings.TrimSpace(def.OutputSchemaRef) == "" {
		return invalid("output schema or output schema ref is required by result policy")
	}

	specCount := 0
	if def.Function != nil {
		specCount++
	}
	if def.HTTP != nil {
		specCount++
	}
	if def.MCP != nil {
		specCount++
	}
	if specCount != 1 {
		return invalid("exactly one executor spec is required")
	}

	switch def.Type {
	case ToolTypeFunction:
		if def.Function == nil || strings.TrimSpace(def.Function.HandlerName) == "" {
			return invalid("function handler name is required")
		}
	case ToolTypeHTTP:
		if def.HTTP == nil {
			return invalid("HTTP spec is required")
		}
		if strings.TrimSpace(def.HTTP.Method) == "" || strings.TrimSpace(def.HTTP.URL) == "" || strings.TrimSpace(def.HTTP.ResponseMode) == "" {
			return invalid("HTTP method, URL, and response mode are required")
		}
		method := strings.ToUpper(strings.TrimSpace(def.HTTP.Method))
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return invalid("HTTP method must be GET, POST, PUT, PATCH, or DELETE")
		}
		if def.HTTP.Write && def.RiskLevel != RiskHigh {
			return invalid("HTTP write operation requires high risk")
		}
		if def.HTTP.Write && method == http.MethodGet {
			return invalid("HTTP GET cannot be declared as a write operation")
		}
		if (method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete) && !def.HTTP.Write {
			return invalid("HTTP PUT, PATCH, and DELETE must be declared as write operations")
		}
		switch def.HTTP.ResponseMode {
		case "json", "text", "bytes":
		default:
			return invalid("HTTP response mode must be json, text, or bytes")
		}
	case ToolTypeMCP:
		if def.MCP == nil {
			return invalid("MCP spec is required")
		}
		if strings.TrimSpace(def.MCP.ServerID) == "" || strings.TrimSpace(def.MCP.SnapshotID) == "" || strings.TrimSpace(def.MCP.MCPToolName) == "" {
			return invalid("MCP server, snapshot, and tool names are required")
		}
	default:
		return invalid("unsupported tool type")
	}

	switch def.Visibility {
	case observability.VisibilityUserVisible,
		observability.VisibilityDebug,
		observability.VisibilityInternal,
		observability.VisibilityRestricted:
	default:
		return invalid("visibility must be canonical")
	}
	return nil
}

func (r *StaticRegistry) ResolveSnapshot(ctx context.Context, req ResolveToolSnapshotRequest) (*ToolSnapshot, error) {
	refs := append([]ToolRef(nil), req.ToolRefs...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name == refs[j].Name {
			return refs[i].Version < refs[j].Version
		}
		return refs[i].Name < refs[j].Name
	})
	defs := make([]ToolDefinition, 0, len(refs))
	for i, ref := range refs {
		def, err := r.Get(ctx, ref.Name, ref.Version)
		if err != nil {
			return nil, err
		}
		refs[i].Version = def.Version
		defs = append(defs, *def)
	}
	capabilityHash := stableHash(struct {
		TenantID string
		AgentID  string
		Defs     []ToolDefinition
	}{TenantID: req.TenantID, AgentID: req.AgentID, Defs: defs})
	policyHash := stableHash(struct {
		TenantID string
		AgentID  string
		Refs     []ToolRef
		Policies []toolPolicySnapshot
	}{TenantID: req.TenantID, AgentID: req.AgentID, Refs: refs, Policies: policies(defs)})
	return &ToolSnapshot{
		SnapshotID:     "tool_snapshot_" + capabilityHash[:24],
		ToolRefs:       refs,
		CapabilityHash: "sha256:" + capabilityHash,
		PolicyHash:     "sha256:" + policyHash,
		CreatedAt:      time.Now(),
	}, nil
}

func toolKey(name string, version string) string {
	return name + "@" + version
}

func stableHash(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type toolPolicySnapshot struct {
	Name         string
	Version      string
	RiskLevel    RiskLevel
	Timeout      time.Duration
	Retry        RetryPolicy
	Permissions  ToolPermissions
	ResultPolicy ToolOutputPolicy
	Visibility   observability.EventVisibility
}

func policies(defs []ToolDefinition) []toolPolicySnapshot {
	out := make([]toolPolicySnapshot, 0, len(defs))
	for _, def := range defs {
		out = append(out, toolPolicySnapshot{
			Name:         def.Name,
			Version:      def.Version,
			RiskLevel:    def.RiskLevel,
			Timeout:      def.Timeout,
			Retry:        def.Retry,
			Permissions:  def.Permissions,
			ResultPolicy: def.ResultPolicy,
			Visibility:   def.Visibility,
		})
	}
	return out
}

func cloneToolDefinition(def ToolDefinition) ToolDefinition {
	def.InputSchema = append(json.RawMessage(nil), def.InputSchema...)
	def.OutputSchema = append(json.RawMessage(nil), def.OutputSchema...)
	def.Permissions.AllowedAgents = append([]string(nil), def.Permissions.AllowedAgents...)
	def.Permissions.RequiredScopes = append([]string(nil), def.Permissions.RequiredScopes...)
	def.Metadata = cloneStringMap(def.Metadata)
	if def.HTTP != nil {
		httpSpec := *def.HTTP
		httpSpec.Headers = cloneStringMap(httpSpec.Headers)
		def.HTTP = &httpSpec
	}
	if def.Function != nil {
		functionSpec := *def.Function
		def.Function = &functionSpec
	}
	if def.MCP != nil {
		mcpSpec := *def.MCP
		def.MCP = &mcpSpec
	}
	return def
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
