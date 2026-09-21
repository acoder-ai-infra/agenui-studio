package agentregistry

import (
	"regexp"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"golang.org/x/mod/semver"
)

var foundationSkillID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func validateFoundationCapabilityRefs(cfg AgentConfig) error {
	seenTools := make(map[string]struct{}, len(cfg.Tools))
	seenToolNames := make(map[string]string, len(cfg.Tools))
	for _, ref := range cfg.Tools {
		if !validExactToolRef(ref) {
			return newError(
				CodeInvalidConfig,
				"tools",
				"tool reference must use exact name and version: "+versionedDependency(ref.Name, ref.Version),
			)
		}
		if agentruntime.IsRuntimeReservedToolName(cfg.Runtime, ref.Name) {
			return newError(CodeInvalidConfig, "tools", "tool name is reserved by the configured Runtime: "+ref.Name)
		}
		key := versionedDependency(ref.Name, ref.Version)
		if _, exists := seenTools[key]; exists {
			return newError(CodeInvalidConfig, "tools", "duplicate tool reference: "+key)
		}
		if previous, exists := seenToolNames[ref.Name]; exists {
			return newError(
				CodeInvalidConfig,
				"tools",
				"one Agent cannot expose multiple versions of the same model tool: "+previous+", "+key,
			)
		}
		seenTools[key] = struct{}{}
		seenToolNames[ref.Name] = key
	}
	seenHITL := make(map[string]struct{}, len(cfg.ToolPolicy.HITLRequiredTools))
	for _, value := range cfg.ToolPolicy.HITLRequiredTools {
		if !validExactToolRefValue(value) {
			return newError(CodeInvalidConfig, "tool_policy.hitl_required_tools", "HITL tool must use exact name@version: "+value)
		}
		if _, declared := seenTools[value]; !declared {
			return newError(CodeInvalidConfig, "tool_policy.hitl_required_tools", "HITL tool is not declared by the Agent: "+value)
		}
		if _, duplicate := seenHITL[value]; duplicate {
			return newError(CodeInvalidConfig, "tool_policy.hitl_required_tools", "duplicate HITL tool: "+value)
		}
		seenHITL[value] = struct{}{}
	}
	if _, err := canonicalToolRisk(cfg.ToolPolicy.RiskLevel); err != nil {
		return newError(
			CodeInvalidConfig,
			"tool_policy.risk_level",
			"tool risk level must be low, medium, or high",
		)
	}
	if cfg.Skills.VersionRange != "" {
		return newError(
			CodeInvalidConfig,
			"skills.version_range",
			"foundation requires exact skill_id@semver references and does not resolve version ranges",
		)
	}
	for _, ref := range cfg.Skills.Allowlist {
		if !validExactSkillRef(ref) {
			return newError(
				CodeInvalidConfig,
				"skills.allowlist",
				"skill reference must use exact skill_id@semver: "+ref,
			)
		}
	}
	for _, serverID := range cfg.ToolPolicy.MCPServers {
		if serverID == "" || serverID != strings.TrimSpace(serverID) || strings.Contains(serverID, "://") {
			return newError(
				CodeInvalidConfig,
				"tool_policy.mcp_servers",
				"MCP reference must use a foundation Registry serverID: "+serverID,
			)
		}
	}
	for _, toolName := range cfg.ToolPolicy.HTTPTools {
		if toolName == "" || toolName != strings.TrimSpace(toolName) || strings.Contains(toolName, "://") {
			return newError(
				CodeInvalidConfig,
				"tool_policy.http_tools",
				"HTTP tool reference must use a bare tenant tool name: "+toolName,
			)
		}
	}
	return nil
}

func validExactToolRef(ref VersionedRef) bool {
	return ref.Name != "" && ref.Name == strings.TrimSpace(ref.Name) && !strings.Contains(ref.Name, "@") &&
		ref.Version != "" && ref.Version == strings.TrimSpace(ref.Version) && !strings.Contains(ref.Version, "@")
}

func validExactToolRefValue(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || strings.Count(value, "@") != 1 {
		return false
	}
	name, version, ok := strings.Cut(value, "@")
	return ok && validExactToolRef(VersionedRef{Name: name, Version: version})
}

func validExactSkillRef(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || strings.Count(value, "@") != 1 {
		return false
	}
	id, version, found := strings.Cut(value, "@")
	return found && foundationSkillID.MatchString(id) && semver.IsValid("v"+version)
}
