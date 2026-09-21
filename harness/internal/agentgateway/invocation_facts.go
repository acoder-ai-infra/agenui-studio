package agentgateway

import "sync"

type invocationFacts struct {
	mu      sync.Mutex
	sealed  bool
	fields  map[string]string
	outcome GatewayProviderOutcome
}

func newInvocationFacts() *invocationFacts {
	return &invocationFacts{fields: make(map[string]string), outcome: GatewayProviderOutcome{Phase: GatewayProviderNotStarted}}
}

func (f *invocationFacts) AddAuditField(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sealed {
		return newGatewayError(CodePluginFailed, "audit fields are sealed", nil)
	}
	if key == "" {
		return newGatewayError(CodePluginFailed, "audit field key is empty", nil)
	}
	f.fields[key] = value
	return nil
}

func (f *invocationFacts) ProviderOutcome() GatewayProviderOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outcome
}

func (f *invocationFacts) seal() {
	f.mu.Lock()
	f.sealed = true
	f.mu.Unlock()
}

func (f *invocationFacts) markEntered() {
	f.mu.Lock()
	f.sealed = true
	if f.outcome.Phase == GatewayProviderNotStarted {
		f.outcome.Phase = GatewayProviderEntered
	}
	f.mu.Unlock()
}

func (f *invocationFacts) setPhase(phase GatewayProviderPhase) {
	f.mu.Lock()
	f.outcome.Phase = phase
	f.mu.Unlock()
}

func (f *invocationFacts) setOutcome(phase GatewayProviderPhase, succeeded bool, code string) {
	f.mu.Lock()
	f.outcome = GatewayProviderOutcome{Phase: phase, Succeeded: succeeded, ErrorCode: code}
	f.mu.Unlock()
}

func (f *invocationFacts) snapshotFields() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.fields))
	for key, value := range f.fields {
		out[key] = value
	}
	return out
}
