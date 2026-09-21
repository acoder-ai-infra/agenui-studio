package agentregistry

import "testing"

func TestRegistryEventDurabilityClassification(t *testing.T) {
	durable := []string{
		EventConfigCommitted,
		EventEvalGatePassed,
		EventEvalGateSkipped,
		EventAgentRegistered,
		EventAgentEnabled,
		EventAgentDisabled,
		EventGrayPercentChanged,
		EventRollbackTriggered,
	}
	for _, eventType := range durable {
		if !requiresDurableRegistryAudit(eventType) {
			t.Errorf("event %q must be durable", eventType)
		}
	}

	appLogs := []string{
		EventSchemaValidated,
		EventDependencyResolved,
		EventPolicyValidated,
		EventRuntimeDryRunPassed,
		EventRuntimeDryRunSkipped,
		EventEffectiveConfigResolved,
		EventConfigDriftDetected,
	}
	for _, eventType := range appLogs {
		if requiresDurableRegistryAudit(eventType) {
			t.Errorf("event %q must remain an app log", eventType)
		}
	}
}
