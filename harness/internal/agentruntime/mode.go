package agentruntime

func DefaultDataPassingMode(mode RuntimeMode) DataPassingMode {
	switch mode {
	case RuntimeModeWorkflow, RuntimeModeGraph:
		return DataPassingState
	case RuntimeModePlanExecute:
		return DataPassingPlanResult
	case RuntimeModeDeepAgent:
		return DataPassingTask
	case RuntimeModeDirect, RuntimeModeReact, "":
		return DataPassingMessages
	default:
		return DataPassingMessages
	}
}

func (d AgentDefinition) EffectiveDataPassingMode() DataPassingMode {
	if d.DataPassing.Mode != "" {
		return d.DataPassing.Mode
	}
	return DefaultDataPassingMode(d.Runtime.Mode)
}
