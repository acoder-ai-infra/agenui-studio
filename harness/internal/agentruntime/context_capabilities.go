package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

var ErrSkillExecutionAdapterMissing = errors.New("skill execution adapter missing")

// SkillExecutionBindingProvider turns non-system skills into Tool Gateway refs.
// Runtime never executes a script or remote skill directly.
type SkillExecutionBindingProvider interface {
	Bind(ctx context.Context, run RunRequest, resolution skill.Resolution) ([]string, error)
}

// ToolDefinitionSnapshotProvider resolves immutable schemas for Tool Registry
// and executable Skill bindings. MCP schemas come from their own frozen
// CapabilitySnapshot and do not use this port.
type ToolDefinitionSnapshotProvider interface {
	ResolveToolDefinitions(ctx context.Context, run RunRequest, refs []string) ([]ModelToolDefinition, error)
}

// GovernedToolDefinitionSnapshotProvider is the production extension of
// ToolDefinitionSnapshotProvider. Besides model-visible schemas, it carries
// the immutable Tool Registry snapshot identity into ModelContextPackage.
//
// 保留旧接口是为了兼容本地测试和已有适配器；生产 Tool Gateway 适配器应实现本接口。
type GovernedToolDefinitionSnapshotProvider interface {
	ResolveGovernedToolDefinitions(ctx context.Context, run RunRequest, refs []string) (GovernedToolDefinitionSnapshot, error)
}

type GovernedToolDefinitionSnapshot struct {
	Snapshot    ToolSchemaSnapshot
	Definitions []ModelToolDefinition
}

type ToolDefinitionSnapshotProviderFunc func(context.Context, RunRequest, []string) ([]ModelToolDefinition, error)

func (f ToolDefinitionSnapshotProviderFunc) ResolveToolDefinitions(ctx context.Context, run RunRequest, refs []string) ([]ModelToolDefinition, error) {
	return f(ctx, run, refs)
}

// HTTPToolMirrorVersion derives the deterministic gateway-mirror tool version
// for a tenant HTTP tool from its frozen DefinitionHash. The model never
// supplies a version; this trusted mapping does. Both the runtime adapter (to
// resolve the definition) and the app registry fallback (to synthesize it) use
// this exact function so the version is stable and collision-resistant.
func HTTPToolMirrorVersion(definitionHash string) string {
	if len(definitionHash) < 16 {
		return "http-" + definitionHash
	}
	return "http-" + definitionHash[:16]
}

// HTTPToolProvider resolves a tenant-authored HTTP tool into a frozen,
// model-visible + executable snapshot, keyed strictly by tenant. It is a neutral
// port so agentruntime does not depend on the httptooldef store directly; the
// app layer supplies the implementation.
type HTTPToolProvider interface {
	ResolveHTTPToolSnapshot(ctx context.Context, tenantID, userID, agentID, name string) (HTTPToolSnapshot, error)
}

// GovernedCapabilitySnapshotProvider resolves immutable, principal-scoped MCP
// and Skill snapshots before the model package is built.
type GovernedCapabilitySnapshotProvider struct {
	MCP           *mcp.Service
	Skills        *skill.Service
	SkillBindings SkillExecutionBindingProvider
	ToolSchemas   ToolDefinitionSnapshotProvider
	HTTPTools     HTTPToolProvider
}

func (p GovernedCapabilitySnapshotProvider) Resolve(ctx context.Context, req RuntimeContextAssemblyRequest) (CapabilitySnapshot, error) {
	result := CapabilitySnapshot{
		Tools:     append([]string(nil), req.Run.Definition.ToolRefs...),
		SubAgents: append([]string(nil), req.Run.Definition.SubAgentRefs...),
	}
	expected, hasExpected := runtimeCapabilityBindingFromContext(ctx)
	expectedMCP := make(map[string]RuntimeMCPSnapshotBinding)
	if hasExpected {
		for _, snapshot := range expected.MCPSnapshots {
			expectedMCP[snapshot.ServerID] = snapshot
		}
	}
	resolvedMCP := make(map[string]struct{})
	resolveMCP := func(value string) error {
		if _, ok := resolvedMCP[value]; ok {
			return nil
		}
		if p.MCP == nil {
			return fmt.Errorf("mcp capability requested but service is unavailable: %s", value)
		}
		principal := mcp.Principal{
			TenantID: req.Run.TenantID,
			UserID:   req.Run.UserID,
			AgentID:  req.Run.Definition.AgentID,
		}
		var snapshot mcp.CapabilitySnapshot
		var err error
		if hasExpected {
			frozen, ok := expectedMCP[value]
			if !ok {
				return fmt.Errorf("%w: unexpected MCP server=%s", ErrRuntimeCapabilityDrift, value)
			}
			snapshot, err = p.MCP.ResolveFrozenSnapshot(ctx, principal, value, frozen.SnapshotID)
			if err == nil && (snapshot.ServerVersion != frozen.ServerVersion || snapshot.PrincipalHash != frozen.PrincipalHash ||
				snapshot.PolicyHash != frozen.PolicyHash || snapshot.CapabilityHash != frozen.CapabilityHash) {
				return fmt.Errorf("%w: MCP snapshot=%s", ErrRuntimeCapabilityDrift, frozen.SnapshotID)
			}
		} else {
			snapshot, err = p.MCP.ResolveSnapshot(ctx, principal, value)
		}
		if err != nil {
			return classifyMCPResolutionError(err)
		}
		resolvedMCP[value] = struct{}{}
		result.MCPServers = append(result.MCPServers, value)
		result.MCPSnapshots = append(result.MCPSnapshots, snapshot)
		return nil
	}
	for _, value := range uniqueStrings(capabilityValues(req.Run.Definition.Metadata, "mcp_servers")) {
		if err := resolveMCP(value); err != nil {
			return CapabilitySnapshot{}, err
		}
	}
	for _, value := range uniqueStrings(capabilityValues(req.Run.Definition.Metadata, "http_tools")) {
		if p.HTTPTools == nil {
			return CapabilitySnapshot{}, fmt.Errorf("http tool requested but provider is unavailable: %s", value)
		}
		snapshot, err := p.HTTPTools.ResolveHTTPToolSnapshot(ctx, req.Run.TenantID, req.Run.UserID, req.Run.Definition.AgentID, value)
		if err != nil {
			return CapabilitySnapshot{}, fmt.Errorf("resolve http tool %s: %w", value, err)
		}
		// The frozen DefinitionHash rides in the ModelContextPackage; execution
		// re-validates the live tenant definition against it and rejects drift, so
		// the model can never invoke a contract the executor won't honor.
		result.HTTPToolSnapshots = append(result.HTTPToolSnapshots, snapshot)
	}
	for _, value := range uniqueStrings(capabilityValues(req.Run.Definition.Metadata, "skill_refs")) {
		if p.Skills == nil {
			return CapabilitySnapshot{}, fmt.Errorf("skill requested but service is unavailable: %s", value)
		}
		ref, err := parseSkillRef(value)
		if err != nil {
			return CapabilitySnapshot{}, err
		}
		resolution, err := p.Skills.Resolve(ctx, skill.Principal{
			TenantID: req.Run.TenantID,
			AgentID:  req.Run.Definition.AgentID,
		}, ref)
		if err != nil {
			return CapabilitySnapshot{}, classifySkillResolutionError(err)
		}
		result.Skills = append(result.Skills, value)
		result.SkillResolutions = append(result.SkillResolutions, resolution)
		if resolution.Root.InjectionStrategy != skill.InjectSystem {
			if p.SkillBindings == nil {
				return CapabilitySnapshot{}, fmt.Errorf("%w: %s", ErrSkillExecutionAdapterMissing, value)
			}
			refs, err := p.SkillBindings.Bind(ctx, req.Run, resolution)
			if err != nil {
				return CapabilitySnapshot{}, classifySkillBindingError(err)
			}
			if len(refs) == 0 {
				return CapabilitySnapshot{}, fmt.Errorf("%w: empty binding for %s", ErrSkillExecutionAdapterMissing, value)
			}
			result.Tools = append(result.Tools, refs...)
		}
		resolvedSkills := []skill.Snapshot{resolution.Root}
		for _, dependency := range resolution.Dependencies {
			resolvedSkills = append(resolvedSkills, dependency.Snapshot)
		}
		for _, snapshot := range resolvedSkills {
			result.Tools = append(result.Tools, snapshot.Dependencies.Tools...)
			for _, serverID := range snapshot.Dependencies.MCPServers {
				if err := resolveMCP(serverID); err != nil {
					return CapabilitySnapshot{}, err
				}
			}
		}
		if resolution.Root.InjectionStrategy == skill.InjectSystem {
			result.ContextFragments = append(result.ContextFragments, contextpkg.ContextFragment{
				Slot:      contextpkg.SlotPolicies,
				Stability: contextpkg.StabilitySemi,
				Priority:  85,
				Pinned:    true,
				TokenCost: resolution.Root.EstimatedTokens,
				Source:    "skill",
				Role:      contextpkg.RoleSystem,
				Content:   resolution.Instructions,
			})
		}
		for _, dependency := range resolution.Dependencies {
			if dependency.Snapshot.InjectionStrategy != skill.InjectSystem {
				continue
			}
			result.ContextFragments = append(result.ContextFragments, contextpkg.ContextFragment{
				Slot:      contextpkg.SlotPolicies,
				Stability: contextpkg.StabilitySemi,
				Priority:  84,
				Pinned:    true,
				TokenCost: dependency.Snapshot.EstimatedTokens,
				Source:    "skill_dependency",
				Role:      contextpkg.RoleSystem,
				Content:   dependency.Instructions,
			})
		}
	}
	result.Tools = uniqueStrings(result.Tools)
	if hasExpected && len(resolvedMCP) != len(expectedMCP) {
		return CapabilitySnapshot{}, fmt.Errorf("%w: MCP server set changed", ErrRuntimeCapabilityDrift)
	}
	if len(result.Tools) > 0 && p.ToolSchemas != nil {
		if governed, ok := p.ToolSchemas.(GovernedToolDefinitionSnapshotProvider); ok {
			resolved, err := governed.ResolveGovernedToolDefinitions(ctx, req.Run, result.Tools)
			if err != nil {
				return CapabilitySnapshot{}, err
			}
			if err := validateToolSchemaSnapshot(resolved.Snapshot, result.Tools); err != nil {
				return CapabilitySnapshot{}, err
			}
			result.ToolSnapshot = cloneToolSchemaSnapshot(&resolved.Snapshot)
			result.ToolDefinitions = cloneModelToolDefinitions(resolved.Definitions)
		} else {
			definitions, err := p.ToolSchemas.ResolveToolDefinitions(ctx, req.Run, result.Tools)
			if err != nil {
				return CapabilitySnapshot{}, err
			}
			result.ToolDefinitions = cloneModelToolDefinitions(definitions)
		}
	}
	return result, nil
}

func classifyMCPResolutionError(err error) error {
	var authorizationRequired *mcp.AuthorizationRequiredError
	if errors.As(err, &authorizationRequired) {
		return err
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, mcp.ErrPrincipalRequired) || errors.Is(err, mcp.ErrPermissionDenied) ||
		errors.Is(err, mcp.ErrServerNotFound) || errors.Is(err, mcp.ErrToolNotAllowed) ||
		errors.Is(err, mcp.ErrSnapshotRequired) || errors.Is(err, mcp.ErrSnapshotNotFound) ||
		errors.Is(err, mcp.ErrSnapshotStale) {
		return err
	}
	return fmt.Errorf("%w: mcp: %w", ErrProductionCapabilityUnavailable, err)
}

func classifySkillResolutionError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, skill.ErrPrincipalRequired) || errors.Is(err, skill.ErrPermissionDenied) ||
		errors.Is(err, skill.ErrNotFound) || errors.Is(err, skill.ErrVersionConflict) ||
		errors.Is(err, skill.ErrInvalidDefinition) || errors.Is(err, skill.ErrDependencyCycle) ||
		errors.Is(err, skill.ErrContentIntegrity) {
		return err
	}
	return fmt.Errorf("%w: skill: %w", ErrProductionCapabilityUnavailable, err)
}

func classifySkillBindingError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrSkillExecutionAdapterMissing) || errors.Is(err, ErrRuntimeCapabilityDrift) ||
		errors.Is(err, skill.ErrPrincipalRequired) || errors.Is(err, skill.ErrPermissionDenied) ||
		errors.Is(err, skill.ErrNotFound) || errors.Is(err, skill.ErrVersionConflict) ||
		errors.Is(err, skill.ErrInvalidDefinition) || errors.Is(err, skill.ErrDependencyCycle) ||
		errors.Is(err, skill.ErrContentIntegrity) {
		return err
	}
	// Binding 适配器通常依赖 Tool Registry 或数据库。未知错误按临时依赖
	// 故障处理，避免一次短暂抖动永久消费 Resume token；结构性错误必须使用
	// 上述明确的 sentinel，继续 fail closed。
	return fmt.Errorf("%w: skill binding: %w", ErrProductionCapabilityUnavailable, err)
}

func validateToolSchemaSnapshot(snapshot ToolSchemaSnapshot, requested []string) error {
	if snapshot.SnapshotID == "" || snapshot.CapabilityHash == "" || snapshot.PolicyHash == "" || len(snapshot.ToolRefs) != len(requested) {
		return ErrProductionToolSnapshot
	}
	allowed := make(map[string]struct{}, len(requested))
	for _, ref := range requested {
		if ref == "" || ref != strings.TrimSpace(ref) || strings.Count(ref, "@") != 1 {
			return ErrProductionToolSnapshot
		}
		name, version, ok := strings.Cut(ref, "@")
		if !ok || name == "" || version == "" || name != strings.TrimSpace(name) || version != strings.TrimSpace(version) {
			return ErrProductionToolSnapshot
		}
		allowed[ref] = struct{}{}
	}
	seen := make(map[string]struct{}, len(snapshot.ToolRefs))
	for _, ref := range snapshot.ToolRefs {
		if _, ok := allowed[ref]; !ok {
			return ErrProductionToolSnapshot
		}
		if _, duplicate := seen[ref]; duplicate {
			return ErrProductionToolSnapshot
		}
		seen[ref] = struct{}{}
	}
	return nil
}

func validateGovernedToolDefinitions(snapshot ToolSchemaSnapshot, requested []string, definitions []ModelToolDefinition) error {
	if err := validateToolSchemaSnapshot(snapshot, requested); err != nil {
		return err
	}
	if len(definitions) != len(requested) {
		return fmt.Errorf("%w: governed tool definition count mismatch", ErrCapabilityToolSchemaInvalid)
	}

	requestedByName := make(map[string]string, len(requested))
	for _, ref := range requested {
		name, version, ok := strings.Cut(ref, "@")
		if !ok || name == "" || version == "" || strings.Contains(version, "@") {
			return fmt.Errorf("%w: invalid governed tool ref=%q", ErrCapabilityToolSchemaInvalid, ref)
		}
		if previous, duplicate := requestedByName[name]; duplicate {
			return fmt.Errorf("%w: model tool name %q maps to both %q and %q", ErrCapabilityToolSchemaInvalid, name, previous, ref)
		}
		requestedByName[name] = ref
	}

	seen := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if definition.Name == "" || len(definition.Schema) == 0 || !json.Valid(definition.Schema) {
			return fmt.Errorf("%w: governed tool=%q", ErrCapabilityToolSchemaInvalid, definition.Name)
		}
		if _, authorized := requestedByName[definition.Name]; !authorized {
			return fmt.Errorf("%w: extra governed tool=%q", ErrCapabilityToolSchemaInvalid, definition.Name)
		}
		if _, duplicate := seen[definition.Name]; duplicate {
			return fmt.Errorf("%w: duplicate governed tool=%q", ErrCapabilityToolSchemaInvalid, definition.Name)
		}
		seen[definition.Name] = struct{}{}
	}
	return nil
}

func validateGovernedMCPSnapshots(capabilities CapabilitySnapshot) error {
	if len(capabilities.MCPServers) != len(capabilities.MCPSnapshots) {
		return fmt.Errorf("%w: server/snapshot count mismatch", ErrProductionMCPSnapshot)
	}
	servers := make(map[string]struct{}, len(capabilities.MCPServers))
	for _, serverID := range capabilities.MCPServers {
		if serverID == "" || serverID != strings.TrimSpace(serverID) {
			return fmt.Errorf("%w: invalid server id", ErrProductionMCPSnapshot)
		}
		if _, duplicate := servers[serverID]; duplicate {
			return fmt.Errorf("%w: duplicate server=%s", ErrProductionMCPSnapshot, serverID)
		}
		servers[serverID] = struct{}{}
	}

	seenServers := make(map[string]struct{}, len(capabilities.MCPSnapshots))
	seenSnapshots := make(map[string]struct{}, len(capabilities.MCPSnapshots))
	for _, snapshot := range capabilities.MCPSnapshots {
		if snapshot.ID == "" || snapshot.ServerID == "" || snapshot.ServerVersion == "" || snapshot.PrincipalHash == "" ||
			snapshot.PolicyHash == "" || snapshot.CapabilityHash == "" {
			return fmt.Errorf("%w: incomplete snapshot identity for server=%s", ErrProductionMCPSnapshot, snapshot.ServerID)
		}
		if _, declared := servers[snapshot.ServerID]; !declared {
			return fmt.Errorf("%w: undeclared server=%s", ErrProductionMCPSnapshot, snapshot.ServerID)
		}
		if _, duplicate := seenServers[snapshot.ServerID]; duplicate {
			return fmt.Errorf("%w: duplicate snapshot for server=%s", ErrProductionMCPSnapshot, snapshot.ServerID)
		}
		if _, duplicate := seenSnapshots[snapshot.ID]; duplicate {
			return fmt.Errorf("%w: duplicate snapshot id=%s", ErrProductionMCPSnapshot, snapshot.ID)
		}
		seenServers[snapshot.ServerID] = struct{}{}
		seenSnapshots[snapshot.ID] = struct{}{}

		tools := make(map[string]struct{}, len(snapshot.Tools))
		for _, tool := range snapshot.Tools {
			if tool.Name == "" || tool.Name != strings.TrimSpace(tool.Name) || len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) {
				return fmt.Errorf("%w: invalid tool schema server=%s tool=%s", ErrProductionMCPSnapshot, snapshot.ServerID, tool.Name)
			}
			if _, duplicate := tools[tool.Name]; duplicate {
				return fmt.Errorf("%w: duplicate tool server=%s tool=%s", ErrProductionMCPSnapshot, snapshot.ServerID, tool.Name)
			}
			tools[tool.Name] = struct{}{}
		}
	}
	return nil
}

func parseSkillRef(value string) (skill.Ref, error) {
	var structured skill.Ref
	if strings.HasPrefix(strings.TrimSpace(value), "{") {
		if err := json.Unmarshal([]byte(value), &structured); err != nil {
			return skill.Ref{}, err
		}
		return structured, nil
	}
	parts := strings.Split(value, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return skill.Ref{}, fmt.Errorf("skill ref must use id@version: %q", value)
	}
	return skill.Ref{ID: parts[0], Version: parts[1]}, nil
}

// LedgerContextSnapshotProvider closes the Runtime/Context boundary over an
// immutable MessageLedger snapshot. Runtime 只解析 OpenTurn 已冻结的精确 ref，
// 不得在执行阶段重新扫描可变 Ledger 并生成另一份事实。
type LedgerContextSnapshotProvider struct {
	Manager contextpkg.SnapshotManager
}

func (p LedgerContextSnapshotProvider) Materialize(ctx context.Context, req RuntimeContextAssemblyRequest) (ContextSnapshot, error) {
	ref := req.Run.ContextSnapshotRef
	if ref == "" {
		return ContextSnapshot{}, fmt.Errorf("%w: context_snapshot_ref required", ErrProductionContextSnapshot)
	}
	snapshot, messages, err := p.Manager.Resolve(ctx, ref)
	if err != nil {
		return ContextSnapshot{}, fmt.Errorf("%w: %w: resolve ref=%s: %w", ErrProductionContextSnapshot, ErrProductionContextUnavailable, ref, err)
	}
	if snapshot.ID != ref || snapshot.SessionID != req.Run.SessionID || snapshot.RunID != req.Run.RunID || snapshot.ContentHash == "" {
		return ContextSnapshot{}, fmt.Errorf(
			"%w: ref=%s snapshot_ref=%s session=%s run=%s hash_present=%t",
			ErrProductionContextSnapshot, ref, snapshot.ID, snapshot.SessionID, snapshot.RunID, snapshot.ContentHash != "",
		)
	}
	for _, message := range messages {
		if message.SessionID != snapshot.SessionID || message.ID == "" || message.Sequence <= 0 || message.Sequence > snapshot.LastSequence {
			return ContextSnapshot{}, fmt.Errorf("%w: invalid snapshot message_id=%s", ErrProductionContextSnapshot, message.ID)
		}
	}
	// Attachments are immutable run facts stored beside the ledger messages.
	// Project them onto the exact current input after SnapshotManager has
	// verified the snapshot hash. This keeps the ledger normalized while making
	// the provider request genuinely multimodal.
	if len(snapshot.Attachments) > 0 {
		currentIDs := make(map[string]struct{}, len(req.Run.Input))
		for _, input := range req.Run.Input {
			if input.ID != "" {
				currentIDs[input.ID] = struct{}{}
			}
		}
		for index := range messages {
			if _, current := currentIDs[messages[index].ID]; !current || len(messages[index].Parts) > 0 {
				continue
			}
			messages[index].Parts = contextpkg.MessagePartsFromAttachments(messages[index].Content, snapshot.Attachments)
		}
	}
	return ContextSnapshot{
		Ref:          ref,
		ContentHash:  snapshot.ContentHash,
		LastSequence: snapshot.LastSequence,
		Messages:     messages,
		Fragments:    snapshot.Fragments,
		Attachments:  snapshot.Attachments,
	}, nil
}
