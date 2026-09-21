package agentbinding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

type bindingHashPayload struct {
	SchemaVersion       string             `json:"schema_version"`
	AgentID             string             `json:"agent_id"`
	AgentVersion        string             `json:"agent_version"`
	ExecutionMode       executionmode.Mode `json:"execution_mode"`
	Target              Target             `json:"target"`
	Source              Source             `json:"source"`
	ControlRuleRef      string             `json:"control_rule_ref,omitempty"`
	ControlRuleRevision string             `json:"control_rule_revision,omitempty"`
	Fallback            FallbackFact       `json:"fallback"`
}

// ComputeBindingHash 只摘要执行选择语义；Run 身份、时间和配置快照由独立事实约束。
func ComputeBindingHash(binding EffectiveBinding) (string, error) {
	payload := bindingHashPayload{
		SchemaVersion:       binding.SchemaVersion,
		AgentID:             binding.AgentID,
		AgentVersion:        binding.AgentVersion,
		ExecutionMode:       binding.ExecutionMode,
		Target:              binding.Target,
		Source:              binding.Source,
		ControlRuleRef:      binding.ControlRuleRef,
		ControlRuleRevision: binding.ControlRuleRevision,
		Fallback:            binding.Fallback,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", NewError(StageFinalize, CodeInvalidRequest, false, err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (binding EffectiveBinding) Validate() error {
	if binding.SchemaVersion != BindingSchemaVersion || binding.BindingID == "" || binding.SessionID == "" || binding.RunID == "" || binding.AgentID == "" || binding.AgentVersion == "" || binding.CreatedAt.IsZero() {
		return NewError(StageStaticValidate, CodeInvalidRequest, false, nil)
	}
	if err := modeError(binding.ExecutionMode); err != nil {
		return err
	}
	if err := binding.Source.Validate(); err != nil {
		return err
	}
	if (binding.ControlRuleRef == "") != (binding.ControlRuleRevision == "") {
		return NewError(StageStaticValidate, CodeControlRuleInvalid, false, nil)
	}
	if binding.ConfigSnapshotRef == "" {
		return NewError(StageStaticValidate, CodeConfigSnapshotMissing, false, nil)
	}
	if binding.ConfigHash == "" {
		return NewError(StageStaticValidate, CodeConfigHashMissing, false, nil)
	}
	if len(binding.CapabilitySnapshotRefs) != 1 || binding.CapabilitySnapshotRefs[0] != binding.ConfigSnapshotRef+"#capabilities" {
		return NewError(StageStaticValidate, CodeCapabilitySnapshotFailed, false, nil)
	}
	if err := validateEffectiveTarget(binding.ExecutionMode, binding.AgentID, binding.AgentVersion, binding.Target); err != nil {
		return err
	}
	if err := validateFallbackFact(binding.Fallback); err != nil {
		return err
	}
	want, err := ComputeBindingHash(binding)
	if err != nil {
		return err
	}
	if binding.BindingHash == "" || binding.BindingHash != want {
		return NewError(StageStaticValidate, CodeBindingHashMismatch, false, nil)
	}
	return nil
}

func validateEffectiveTarget(mode executionmode.Mode, agentID, agentVersion string, target Target) error {
	if err := validateTargetBasics(target); err != nil {
		return err
	}
	switch mode {
	case executionmode.DirectAction, executionmode.SingleAgent, executionmode.DeepAgent:
		if target.Kind != TargetAgent || target.Ref != agentID || target.Version != agentVersion || target.StateSchemaRef != "" {
			return NewError(StageStaticValidate, CodeTargetInvalid, false, nil)
		}
	case executionmode.Workflow:
		if target.Kind != TargetWorkflow || target.Version == "" || target.Hash == "" || target.StateSchemaRef != "" {
			return NewError(StageStaticValidate, CodeTargetInvalid, false, nil)
		}
	case executionmode.Graph:
		if target.Kind != TargetGraph || target.Version == "" || target.Hash == "" || target.StateSchemaRef == "" {
			return NewError(StageStaticValidate, CodeTargetInvalid, false, nil)
		}
	default:
		return NewError(StageStaticValidate, CodeExecutionModeUnsupported, false, nil)
	}
	return nil
}

func validateFallbackFact(fallback FallbackFact) error {
	if !fallback.Applied {
		if fallback.From != (Selection{}) || fallback.To != (Selection{}) || fallback.ReasonCode != "" {
			return NewError(StageStaticValidate, CodeInvalidRequest, false, nil)
		}
		return nil
	}
	if fallback.From.AgentID == "" || fallback.To.AgentID == "" || !isUnavailableCode(fallback.ReasonCode) {
		return NewError(StageStaticValidate, CodeInvalidRequest, false, nil)
	}
	return nil
}

func isUnavailableCode(code ErrorCode) bool {
	switch code {
	case CodeAgentNotFound, CodeAgentDisabled, CodeAgentVersionUnavailable, CodeAgentUnavailable:
		return true
	default:
		return false
	}
}
