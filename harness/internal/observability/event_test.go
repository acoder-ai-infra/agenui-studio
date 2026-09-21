package observability

import "testing"

func TestIsEphemeralDelta(t *testing.T) {
	// 高频逐字增量:走实时通道不落库。
	ephemeral := []EventType{EventModelTokenDelta, EventAgentTextDelta}
	for _, et := range ephemeral {
		if !IsEphemeralDelta(et) {
			t.Fatalf("%s should be ephemeral", et)
		}
	}
	// 事实类事件:必须落库,不得被判为 ephemeral。
	facts := []EventType{
		EventModelThoughtDelta, EventModelToolCallDelta, EventModelUsageDelta,
		EventModelCallCompleted, EventFinalResponse, EventRunCompleted,
		EventRunStarted, EventModelCallStarted, EventModelFallbackApplied,
	}
	for _, et := range facts {
		if IsEphemeralDelta(et) {
			t.Fatalf("%s must NOT be ephemeral (it is a persisted fact)", et)
		}
	}
}

func TestEphemeralDeltaTypesAreRegistered(t *testing.T) {
	// ephemeral 事件仍是合法的 canonical 类型(只是不落库),故仍应在注册表中,
	// 以便在需要时(如 debug 通道显式落库)不被 EventStore 拒绝。
	for et := range ephemeralDeltaEventTypes {
		if !IsRegisteredEventType(et) {
			t.Fatalf("ephemeral type %s should still be a registered event type", et)
		}
	}
}

func TestRuntimeStepEventsAreRegistered(t *testing.T) {
	for _, eventType := range []EventType{
		EventRuntimeStepStarted,
		EventRuntimeStepCompleted,
		EventRuntimeStepFailed,
		EventRuntimeStepCancelled,
	} {
		if !IsRegisteredEventType(eventType) {
			t.Fatalf("runtime step type %s must be persistable", eventType)
		}
	}
}
