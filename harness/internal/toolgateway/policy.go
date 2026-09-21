package toolgateway

import (
	"context"
	"fmt"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type PolicyEngine interface {
	EvaluateToolCall(ctx context.Context, def *ToolDefinition, req ToolCallRequest) (*PolicyDecision, error)
}

type PolicyDecision struct {
	Decision            Decision `json:"decision"`
	ReasonCode          string   `json:"reason_code,omitempty"`
	SafeMessage         string   `json:"safe_message,omitempty"`
	RequiredControlType string   `json:"required_control_type,omitempty"`
}

type Decision string

const (
	DecisionAllow          Decision = "allow"
	DecisionBlock          Decision = "block"
	DecisionRequireControl Decision = "require_control"
)

type DefaultPolicyEngine struct{}

func (DefaultPolicyEngine) EvaluateToolCall(ctx context.Context, def *ToolDefinition, req ToolCallRequest) (*PolicyDecision, error) {
	if len(def.Permissions.AllowedAgents) == 0 {
		return &PolicyDecision{
			Decision:    DecisionBlock,
			ReasonCode:  string(ErrorTypePermissionDenied),
			SafeMessage: "工具未绑定到当前 Agent。",
		}, nil
	}
	if !containsString(def.Permissions.AllowedAgents, req.AgentID) {
		return &PolicyDecision{
			Decision:    DecisionBlock,
			ReasonCode:  string(ErrorTypePermissionDenied),
			SafeMessage: "当前 Agent 无权调用该工具。",
		}, nil
	}
	for _, required := range def.Permissions.RequiredScopes {
		if !containsString(req.Policy.Scopes, required) {
			return &PolicyDecision{
				Decision:    DecisionBlock,
				ReasonCode:  string(ErrorTypePermissionDenied),
				SafeMessage: fmt.Sprintf("缺少工具调用权限: %s", required),
			}, nil
		}
	}
	if decision := evaluateTenantScope(ctx, def.Permissions.TenantScope, req); decision != nil {
		return decision, nil
	}
	if req.Policy.RequireApproval || def.RiskLevel == RiskHigh {
		return &PolicyDecision{
			Decision:            DecisionRequireControl,
			ReasonCode:          string(ErrorTypeControlRequired),
			SafeMessage:         "工具调用需要人工确认。",
			RequiredControlType: "approval",
		}, nil
	}
	if riskRank(req.Policy.RiskLevel) < riskRank(def.RiskLevel) {
		return &PolicyDecision{
			Decision:    DecisionBlock,
			ReasonCode:  string(ErrorTypePermissionDenied),
			SafeMessage: "工具风险等级超过当前调用策略。",
		}, nil
	}
	return &PolicyDecision{Decision: DecisionAllow}, nil
}

func evaluateTenantScope(ctx context.Context, scope string, req ToolCallRequest) *PolicyDecision {
	deny := func(message string) *PolicyDecision {
		return &PolicyDecision{
			Decision:    DecisionBlock,
			ReasonCode:  string(ErrorTypePermissionDenied),
			SafeMessage: message,
		}
	}

	switch scope {
	case "", "tenant", "system":
		if req.TenantID == "" {
			return deny("工具调用缺少租户身份。")
		}
	case "user":
		if req.TenantID == "" || req.UserID == "" {
			return deny("工具调用缺少租户或用户身份。")
		}
	default:
		return deny("工具租户权限配置无效。")
	}

	trusted, ok := observability.TraceContextFrom(ctx)
	if !ok {
		return nil
	}
	if trusted.TenantID != "" && trusted.TenantID != req.TenantID {
		return deny("工具调用租户身份不匹配。")
	}
	if scope == "user" && trusted.UserID != "" && trusted.UserID != req.UserID {
		return deny("工具调用用户身份不匹配。")
	}
	return nil
}

func riskRank(risk RiskLevel) int {
	switch risk {
	case RiskHigh:
		return 3
	case RiskMedium:
		return 2
	case RiskLow:
		return 1
	default:
		return 0
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == "*" || value == target {
			return true
		}
	}
	return false
}
