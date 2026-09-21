package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

const RuntimeBindingSchemaVersion = "harness.runtime_binding.v2"

var (
	ErrRuntimeBindingMissing  = errors.New("runtime binding missing")
	ErrRuntimeBindingInvalid  = errors.New("runtime binding invalid")
	ErrRuntimeBindingMismatch = errors.New("runtime binding mismatch")
	ErrRuntimeCapabilityDrift = errors.New("runtime capability snapshot drift")
)

// RuntimeBinding freezes every identity required to interpret a Run's native
// checkpoint. A new Run may select or fall back to another runtime; Resume may
// only use the exact binding persisted by that Run.
type RuntimeBinding struct {
	SchemaVersion       string                    `json:"schema_version"`
	Runtime             RuntimeType               `json:"runtime"`
	RuntimeVersion      string                    `json:"runtime_version"`
	AdapterVersion      string                    `json:"adapter_version"`
	Governance          RuntimeGovernanceMode     `json:"governance"`
	CheckpointFormat    string                    `json:"checkpoint_format,omitempty"`
	AgentDefinitionHash string                    `json:"agent_definition_hash"`
	ConfigSnapshotRef   string                    `json:"config_snapshot_ref,omitempty"`
	ConfigHash          string                    `json:"config_hash,omitempty"`
	AgentBindingID      string                    `json:"agent_binding_id,omitempty"`
	TenantID            string                    `json:"tenant_id,omitempty"`
	UserID              string                    `json:"user_id,omitempty"`
	Capabilities        *RuntimeCapabilityBinding `json:"capabilities,omitempty"`
}

// RuntimeCapabilityBinding freezes the exact model-visible capability set of
// the first ModelContextPackage. MCP refs let Resume reload the original
// snapshots; Hash rejects any schema, policy, Skill or Tool expansion.
type RuntimeCapabilityBinding struct {
	Hash         string                      `json:"hash"`
	MCPSnapshots []RuntimeMCPSnapshotBinding `json:"mcp_snapshots,omitempty"`
}

type RuntimeMCPSnapshotBinding struct {
	SnapshotID     string `json:"snapshot_id"`
	ServerID       string `json:"server_id"`
	ServerVersion  string `json:"server_version"`
	PrincipalHash  string `json:"principal_hash"`
	PolicyHash     string `json:"policy_hash"`
	CapabilityHash string `json:"capability_hash"`
}

// stableRuntimeCapabilities only contains capability facts that must remain
// identical across Resume. Resolution IDs, timestamps and token estimates are
// intentionally excluded because they are transient derivatives, not grants.
type stableRuntimeCapabilities struct {
	Tools           []string              `json:"tools,omitempty"`
	ToolSnapshot    *ToolSchemaSnapshot   `json:"tool_snapshot,omitempty"`
	ToolDefinitions []ModelToolDefinition `json:"tool_definitions,omitempty"`
	Skills          []string              `json:"skills,omitempty"`
	SubAgents       []string              `json:"sub_agents,omitempty"`
	MCPServers      []string              `json:"mcp_servers,omitempty"`
	SkillSnapshots  []stableSkillSnapshot `json:"skill_snapshots,omitempty"`
	MCPSnapshots    []stableMCPSnapshot   `json:"mcp_snapshots,omitempty"`
}

type stableSkillSnapshot struct {
	SkillID           string                  `json:"skill_id"`
	Version           string                  `json:"version"`
	TenantID          string                  `json:"tenant_id,omitempty"`
	ContentHash       string                  `json:"content_hash"`
	ContentSize       int64                   `json:"content_size"`
	InjectionStrategy skill.InjectionStrategy `json:"injection_strategy"`
	Dependencies      skill.Dependencies      `json:"dependencies,omitempty"`
}

type stableMCPSnapshot struct {
	ID             string     `json:"id"`
	ServerID       string     `json:"server_id"`
	ServerVersion  string     `json:"server_version"`
	PrincipalHash  string     `json:"principal_hash"`
	PolicyHash     string     `json:"policy_hash"`
	CapabilityHash string     `json:"capability_hash"`
	Tools          []mcp.Tool `json:"tools,omitempty"`
}

func (b RuntimeCapabilityBinding) Validate() error {
	if b.Hash == "" {
		return ErrRuntimeBindingInvalid
	}
	servers := make(map[string]struct{}, len(b.MCPSnapshots))
	snapshots := make(map[string]struct{}, len(b.MCPSnapshots))
	for _, snapshot := range b.MCPSnapshots {
		if snapshot.SnapshotID == "" || snapshot.ServerID == "" || snapshot.ServerVersion == "" || snapshot.PrincipalHash == "" ||
			snapshot.PolicyHash == "" || snapshot.CapabilityHash == "" {
			return ErrRuntimeBindingInvalid
		}
		if _, duplicate := servers[snapshot.ServerID]; duplicate {
			return ErrRuntimeBindingInvalid
		}
		if _, duplicate := snapshots[snapshot.SnapshotID]; duplicate {
			return ErrRuntimeBindingInvalid
		}
		servers[snapshot.ServerID] = struct{}{}
		snapshots[snapshot.SnapshotID] = struct{}{}
	}
	return nil
}

type runtimeBindingFacts struct {
	ConfigSnapshotRef string
	ConfigHash        string
	AgentBindingID    string
	TenantID          string
	UserID            string
}

func runtimeBindingFactsFromRun(req RunRequest) runtimeBindingFacts {
	return runtimeBindingFacts{
		ConfigSnapshotRef: req.ConfigSnapshotRef,
		ConfigHash:        req.ConfigHash,
		AgentBindingID:    req.AgentBindingID,
		TenantID:          req.TenantID,
		UserID:            req.UserID,
	}
}

func (b RuntimeBinding) Validate() error {
	if b.SchemaVersion != RuntimeBindingSchemaVersion || b.Runtime == "" || b.Runtime == RuntimeTypeAuto || b.RuntimeVersion == "" || b.AdapterVersion == "" || b.Governance == "" || b.AgentDefinitionHash == "" ||
		(b.ConfigSnapshotRef == "") != (b.ConfigHash == "") {
		return fmt.Errorf("%w: %#v", ErrRuntimeBindingInvalid, b)
	}
	if b.Capabilities != nil {
		if err := b.Capabilities.Validate(); err != nil {
			return fmt.Errorf("%w: capabilities: %v", ErrRuntimeBindingInvalid, err)
		}
	}
	return nil
}

func newRuntimeBinding(ctx context.Context, runtime AgentRuntime, def AgentDefinition, facts runtimeBindingFacts) (RuntimeBinding, error) {
	if runtime == nil {
		return RuntimeBinding{}, ErrRuntimeMissing
	}
	descriptor := runtime.Descriptor(ctx)
	runtimeType := descriptor.Name
	if runtimeType == "" || runtimeType == RuntimeTypeAuto {
		runtimeType = inferRuntimeType(runtime)
	}
	def.Runtime.Type = runtimeType
	hash, err := agentDefinitionHash(def)
	if err != nil {
		return RuntimeBinding{}, err
	}
	binding := RuntimeBinding{
		SchemaVersion:       RuntimeBindingSchemaVersion,
		Runtime:             runtimeType,
		RuntimeVersion:      descriptor.RuntimeVersion,
		AdapterVersion:      descriptor.AdapterVersion,
		Governance:          descriptor.Governance,
		CheckpointFormat:    descriptor.CheckpointFormat,
		AgentDefinitionHash: hash,
		ConfigSnapshotRef:   facts.ConfigSnapshotRef,
		ConfigHash:          facts.ConfigHash,
		AgentBindingID:      facts.AgentBindingID,
		TenantID:            facts.TenantID,
		UserID:              facts.UserID,
	}
	if err := binding.Validate(); err != nil {
		return RuntimeBinding{}, err
	}
	return binding, nil
}

func agentDefinitionHash(def AgentDefinition) (string, error) {
	data, err := json.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("hash agent definition: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateResumeRuntimeBinding(binding RuntimeBinding, req ResumeRequest) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if req.ConfigSnapshotRef != "" && req.ConfigSnapshotRef != binding.ConfigSnapshotRef {
		return runtimeBindingMismatch("config_snapshot_ref", binding.ConfigSnapshotRef, req.ConfigSnapshotRef)
	}
	if req.ConfigHash != "" && req.ConfigHash != binding.ConfigHash {
		return runtimeBindingMismatch("config_hash", binding.ConfigHash, req.ConfigHash)
	}
	if req.AgentBindingID != "" && req.AgentBindingID != binding.AgentBindingID {
		return runtimeBindingMismatch("agent_binding_id", binding.AgentBindingID, req.AgentBindingID)
	}
	if req.TenantID != "" && req.TenantID != binding.TenantID {
		return runtimeBindingMismatch("tenant_id", binding.TenantID, req.TenantID)
	}
	if req.UserID != "" && req.UserID != binding.UserID {
		return runtimeBindingMismatch("user_id", binding.UserID, req.UserID)
	}
	if req.Trace.TenantID != "" && req.Trace.TenantID != binding.TenantID {
		return runtimeBindingMismatch("trace.tenant_id", binding.TenantID, req.Trace.TenantID)
	}
	if req.Trace.UserID != "" && req.Trace.UserID != binding.UserID {
		return runtimeBindingMismatch("trace.user_id", binding.UserID, req.Trace.UserID)
	}
	def := req.Definition
	def.Runtime.Type = binding.Runtime
	hash, err := agentDefinitionHash(def)
	if err != nil {
		return err
	}
	if hash != binding.AgentDefinitionHash {
		return runtimeBindingMismatch("agent_definition_hash", binding.AgentDefinitionHash, hash)
	}
	return nil
}

func assertRuntimeBinding(expected, actual RuntimeBinding) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := actual.Validate(); err != nil {
		return err
	}
	checks := []struct {
		field string
		want  string
		got   string
	}{
		{field: "runtime", want: string(expected.Runtime), got: string(actual.Runtime)},
		{field: "runtime_version", want: expected.RuntimeVersion, got: actual.RuntimeVersion},
		{field: "adapter_version", want: expected.AdapterVersion, got: actual.AdapterVersion},
		{field: "governance", want: string(expected.Governance), got: string(actual.Governance)},
		{field: "checkpoint_format", want: expected.CheckpointFormat, got: actual.CheckpointFormat},
		{field: "agent_definition_hash", want: expected.AgentDefinitionHash, got: actual.AgentDefinitionHash},
		{field: "config_snapshot_ref", want: expected.ConfigSnapshotRef, got: actual.ConfigSnapshotRef},
		{field: "config_hash", want: expected.ConfigHash, got: actual.ConfigHash},
		{field: "agent_binding_id", want: expected.AgentBindingID, got: actual.AgentBindingID},
		{field: "tenant_id", want: expected.TenantID, got: actual.TenantID},
		{field: "user_id", want: expected.UserID, got: actual.UserID},
	}
	for _, check := range checks {
		if check.want != check.got {
			return runtimeBindingMismatch(check.field, check.want, check.got)
		}
	}
	return nil
}

func runtimeBindingMismatch(field, want, got string) error {
	return fmt.Errorf("%w: field=%s want=%q got=%q", ErrRuntimeBindingMismatch, field, want, got)
}

func newRuntimeCapabilityBinding(capabilities ModelContextCapabilities) (*RuntimeCapabilityBinding, error) {
	stable := canonicalRuntimeCapabilities(capabilities)
	data, err := json.Marshal(stable)
	if err != nil {
		return nil, fmt.Errorf("hash runtime capabilities: %w", err)
	}
	sum := sha256.Sum256(data)
	binding := &RuntimeCapabilityBinding{Hash: "sha256:" + hex.EncodeToString(sum[:])}
	for _, snapshot := range stable.MCPSnapshots {
		binding.MCPSnapshots = append(binding.MCPSnapshots, RuntimeMCPSnapshotBinding{
			SnapshotID: snapshot.ID, ServerID: snapshot.ServerID, ServerVersion: snapshot.ServerVersion,
			PrincipalHash: snapshot.PrincipalHash, PolicyHash: snapshot.PolicyHash, CapabilityHash: snapshot.CapabilityHash,
		})
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return binding, nil
}

func canonicalRuntimeCapabilities(capabilities ModelContextCapabilities) stableRuntimeCapabilities {
	stable := stableRuntimeCapabilities{
		Tools:      sortedStrings(capabilities.Tools),
		Skills:     sortedStrings(capabilities.Skills),
		SubAgents:  sortedStrings(capabilities.SubAgents),
		MCPServers: sortedStrings(capabilities.MCPServers),
	}
	if capabilities.ToolSnapshot != nil {
		snapshot := *capabilities.ToolSnapshot
		snapshot.ToolRefs = sortedStrings(snapshot.ToolRefs)
		stable.ToolSnapshot = &snapshot
	}
	for _, definition := range capabilities.ToolDefinitions {
		definition.Schema = append(json.RawMessage(nil), definition.Schema...)
		stable.ToolDefinitions = append(stable.ToolDefinitions, definition)
	}
	sort.SliceStable(stable.ToolDefinitions, func(i, j int) bool {
		left, right := stable.ToolDefinitions[i], stable.ToolDefinitions[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Description != right.Description {
			return left.Description < right.Description
		}
		return string(left.Schema) < string(right.Schema)
	})
	for _, snapshot := range capabilities.SkillSnapshots {
		stable.SkillSnapshots = append(stable.SkillSnapshots, stableSkillSnapshot{
			SkillID:           snapshot.SkillID,
			Version:           snapshot.Version,
			TenantID:          snapshot.TenantID,
			ContentHash:       snapshot.ContentHash,
			ContentSize:       snapshot.ContentSize,
			InjectionStrategy: snapshot.InjectionStrategy,
			Dependencies:      canonicalSkillDependencies(snapshot.Dependencies),
		})
	}
	sort.SliceStable(stable.SkillSnapshots, func(i, j int) bool {
		left, right := stable.SkillSnapshots[i], stable.SkillSnapshots[j]
		if left.SkillID != right.SkillID {
			return left.SkillID < right.SkillID
		}
		if left.Version != right.Version {
			return left.Version < right.Version
		}
		if left.TenantID != right.TenantID {
			return left.TenantID < right.TenantID
		}
		return left.ContentHash < right.ContentHash
	})
	for _, snapshot := range capabilities.MCPSnapshots {
		frozen := stableMCPSnapshot{
			ID:             snapshot.ID,
			ServerID:       snapshot.ServerID,
			ServerVersion:  snapshot.ServerVersion,
			PrincipalHash:  snapshot.PrincipalHash,
			PolicyHash:     snapshot.PolicyHash,
			CapabilityHash: snapshot.CapabilityHash,
		}
		for _, tool := range snapshot.Tools {
			tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
			frozen.Tools = append(frozen.Tools, tool)
		}
		sort.SliceStable(frozen.Tools, func(i, j int) bool {
			left, right := frozen.Tools[i], frozen.Tools[j]
			if left.Name != right.Name {
				return left.Name < right.Name
			}
			if left.Description != right.Description {
				return left.Description < right.Description
			}
			return string(left.InputSchema) < string(right.InputSchema)
		})
		stable.MCPSnapshots = append(stable.MCPSnapshots, frozen)
	}
	sort.SliceStable(stable.MCPSnapshots, func(i, j int) bool {
		left, right := stable.MCPSnapshots[i], stable.MCPSnapshots[j]
		if left.ServerID != right.ServerID {
			return left.ServerID < right.ServerID
		}
		return left.ID < right.ID
	})
	return stable
}

func canonicalSkillDependencies(dependencies skill.Dependencies) skill.Dependencies {
	stable := skill.Dependencies{
		Tools:      sortedStrings(dependencies.Tools),
		MCPServers: sortedStrings(dependencies.MCPServers),
		Skills:     append([]skill.Ref(nil), dependencies.Skills...),
	}
	sort.SliceStable(stable.Skills, func(i, j int) bool {
		if stable.Skills[i].ID != stable.Skills[j].ID {
			return stable.Skills[i].ID < stable.Skills[j].ID
		}
		return stable.Skills[i].Version < stable.Skills[j].Version
	})
	return stable
}

func sortedStrings(values []string) []string {
	stable := append([]string(nil), values...)
	sort.Strings(stable)
	return stable
}

func validateRuntimeCapabilityBinding(expected *RuntimeCapabilityBinding, capabilities ModelContextCapabilities) error {
	if expected == nil {
		return nil
	}
	actual, err := newRuntimeCapabilityBinding(capabilities)
	if err != nil {
		return err
	}
	if expected.Hash != actual.Hash {
		return fmt.Errorf("%w: want=%s got=%s", ErrRuntimeCapabilityDrift, expected.Hash, actual.Hash)
	}
	return nil
}

func assertRuntimeCapabilityBinding(expected, actual RuntimeCapabilityBinding) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := actual.Validate(); err != nil {
		return err
	}
	if expected.Hash != actual.Hash || len(expected.MCPSnapshots) != len(actual.MCPSnapshots) {
		return ErrRuntimeCapabilityDrift
	}
	for index := range expected.MCPSnapshots {
		if expected.MCPSnapshots[index] != actual.MCPSnapshots[index] {
			return ErrRuntimeCapabilityDrift
		}
	}
	return nil
}

func cloneRuntimeBinding(binding RuntimeBinding) RuntimeBinding {
	out := binding
	out.Capabilities = cloneRuntimeCapabilityBinding(binding.Capabilities)
	return out
}

func cloneRuntimeCapabilityBinding(binding *RuntimeCapabilityBinding) *RuntimeCapabilityBinding {
	if binding == nil {
		return nil
	}
	cloned := *binding
	cloned.MCPSnapshots = append([]RuntimeMCPSnapshotBinding(nil), binding.MCPSnapshots...)
	return &cloned
}

type runtimeCapabilityBindingContextKey struct{}

func withRuntimeCapabilityBinding(ctx context.Context, binding *RuntimeCapabilityBinding) context.Context {
	if binding == nil {
		// A fresh child Run must shadow any parent Run binding carried by the
		// invocation context. Otherwise its first capability resolution is
		// incorrectly validated against the parent's frozen capability set.
		return context.WithValue(ctx, runtimeCapabilityBindingContextKey{}, (*RuntimeCapabilityBinding)(nil))
	}
	return context.WithValue(ctx, runtimeCapabilityBindingContextKey{}, cloneRuntimeCapabilityBinding(binding))
}

func runtimeCapabilityBindingFromContext(ctx context.Context) (*RuntimeCapabilityBinding, bool) {
	binding, ok := ctx.Value(runtimeCapabilityBindingContextKey{}).(*RuntimeCapabilityBinding)
	if !ok || binding == nil {
		return nil, false
	}
	return cloneRuntimeCapabilityBinding(binding), true
}
