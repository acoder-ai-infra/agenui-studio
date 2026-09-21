package agentregistry

import "context"

type PassThroughEvalGate struct {
	Pass   bool
	Reason string
}

func (g PassThroughEvalGate) Evaluate(context.Context, AgentConfig) (EvalGateResult, error) {
	pass := g.Pass
	if !pass && g.Reason == "" {
		pass = true
	}
	result := EvalGateResult{Passed: pass, Mock: true, Reason: g.Reason}
	if !result.Passed {
		return result, newError(CodeEvalGateFailed, "release.eval_gate", firstNonEmpty(g.Reason, "eval gate failed"))
	}
	if result.Reason == "" {
		result.Reason = "mock pass-through"
	}
	return result, nil
}

type PassThroughRuntimeValidator struct {
	Pass   bool
	Reason string
}

func (v PassThroughRuntimeValidator) DryRun(context.Context, AgentConfig) (RuntimeBuildResult, error) {
	pass := v.Pass
	if !pass && v.Reason == "" {
		pass = true
	}
	result := RuntimeBuildResult{Passed: pass, Mock: true, Reason: v.Reason}
	if !result.Passed {
		return result, newError(CodeRuntimeDryRunFailed, "runtime", firstNonEmpty(v.Reason, "runtime dry run failed"))
	}
	if result.Reason == "" {
		result.Reason = "dry run pass-through"
	}
	return result, nil
}
