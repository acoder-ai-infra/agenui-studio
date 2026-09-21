package agentregistry

import (
	"context"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

// ToolCatalogResolver 在 Agent 发布阶段校验作者声明的精确工具版本。
// 租户权限属于运行事实，必须由 Runtime 在 ModelContext 构建阶段使用
// 真实 TenantID 调 ResolveSnapshot；这里刻意只调用 Get，避免生成伪快照。
type ToolCatalogResolver struct {
	Registry toolgateway.ToolRegistry
}

func (r ToolCatalogResolver) Resolve(ctx context.Context, cfg AgentConfig) (ResolvedDependencies, error) {
	cfg = normalizeConfig(cfg)
	if len(cfg.Tools) == 0 {
		return ResolvedDependencies{}, nil
	}
	if r.Registry == nil {
		return ResolvedDependencies{}, newError(CodeDependencyMissing, "tools", "tool registry is required")
	}

	deps := ResolvedDependencies{Tools: make([]string, 0, len(cfg.Tools))}
	for _, ref := range cfg.Tools {
		if !validExactToolRef(ref) {
			return ResolvedDependencies{}, newError(
				CodeInvalidConfig,
				"tools",
				"tool reference must use exact name and version: "+versionedDependency(ref.Name, ref.Version),
			)
		}
		definition, err := r.Registry.Get(ctx, ref.Name, ref.Version)
		if err != nil {
			return ResolvedDependencies{}, wrapError(
				CodeDependencyMissing,
				"tools",
				"tool dependency is unavailable: "+versionedDependency(ref.Name, ref.Version),
				err,
			)
		}
		if definition == nil || definition.Name != ref.Name || definition.Version != ref.Version {
			return ResolvedDependencies{}, newError(
				CodeDependencyMissing,
				"tools",
				"tool registry returned a mismatched definition: "+versionedDependency(ref.Name, ref.Version),
			)
		}
		// Registry 实现可能只负责读取，发布校验必须自行拒绝已停用工具。
		if definition.Disabled {
			return ResolvedDependencies{}, newError(
				CodeDependencyMissing,
				"tools",
				"tool dependency is disabled: "+versionedDependency(ref.Name, ref.Version),
			)
		}
		if !toolAuthorizesAgent(definition.Permissions.AllowedAgents, cfg.AgentID) {
			return ResolvedDependencies{}, newError(
				CodePolicyViolation,
				"tools",
				"tool does not authorize this Agent: "+versionedDependency(ref.Name, ref.Version),
			)
		}
		risk, err := canonicalToolRisk(string(definition.RiskLevel))
		if err != nil || risk == "" {
			return ResolvedDependencies{}, wrapError(
				CodeDependencyMissing,
				"tools",
				"tool definition has an invalid risk level: "+versionedDependency(ref.Name, ref.Version),
				err,
			)
		}
		exactRef := versionedDependency(definition.Name, definition.Version)
		deps.Tools = append(deps.Tools, exactRef)
		deps.AuthoringMaxToolRisk = maxToolRisk(deps.AuthoringMaxToolRisk, string(risk))
		if risk == toolgateway.RiskHigh {
			deps.AuthoringHighRiskTools = append(deps.AuthoringHighRiskTools, exactRef)
		}
	}
	return normalizeDeps(deps), nil
}

// toolWildcardAgent in a tool's allowed_agents opens it to every Agent. It is
// intended for generic built-in tools (file/compute/etc.) that carry no
// per-agent authorization concern. Business tools keep explicit agent lists.
const toolWildcardAgent = "*"

func toolAuthorizesAgent(allowed []string, agentID string) bool {
	for _, entry := range allowed {
		if entry == toolWildcardAgent || entry == agentID {
			return true
		}
	}
	return false
}

func canonicalToolRisk(value string) (toolgateway.RiskLevel, error) {
	switch toolgateway.RiskLevel(value) {
	case "":
		return "", nil
	case toolgateway.RiskLow, toolgateway.RiskMedium, toolgateway.RiskHigh:
		return toolgateway.RiskLevel(value), nil
	default:
		return "", fmt.Errorf("unsupported tool risk level %q", value)
	}
}

func maxToolRisk(left, right string) string {
	if toolRiskRank(right) > toolRiskRank(left) {
		return right
	}
	return left
}

func toolRiskRank(value string) int {
	switch toolgateway.RiskLevel(value) {
	case toolgateway.RiskHigh:
		return 3
	case toolgateway.RiskMedium:
		return 2
	case toolgateway.RiskLow:
		return 1
	default:
		return 0
	}
}

func effectiveToolRisk(cfg AgentConfig, deps ResolvedDependencies) (string, error) {
	configured, err := canonicalToolRisk(cfg.ToolPolicy.RiskLevel)
	if err != nil {
		return "", err
	}
	authoring, err := canonicalToolRisk(deps.AuthoringMaxToolRisk)
	if err != nil {
		return "", fmt.Errorf("invalid authoring tool risk: %w", err)
	}
	if configured != "" && toolRiskRank(string(configured)) < toolRiskRank(string(authoring)) {
		return "", fmt.Errorf("configured tool risk %q is lower than catalog risk %q", configured, authoring)
	}
	return maxToolRisk(string(configured), string(authoring)), nil
}

var _ DependencyResolver = ToolCatalogResolver{}

func hasToolCatalogResolver(resolver DependencyResolver) bool {
	switch value := resolver.(type) {
	case ToolCatalogResolver:
		return value.Registry != nil
	case *ToolCatalogResolver:
		return value != nil && value.Registry != nil
	case CompositeDependencyResolver:
		for _, child := range value {
			if hasToolCatalogResolver(child) {
				return true
			}
		}
	}
	return false
}
