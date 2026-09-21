package agentruntime

import (
	"context"
	"sync"
)

type activeRun struct {
	cancel context.CancelFunc

	mu            sync.Mutex
	runtime       AgentRuntime
	cancelRequest *CancelRequest
}

func (r *activeRun) setRuntime(runtime AgentRuntime) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtime = runtime
}

func (r *activeRun) requestCancel(req CancelRequest) (AgentRuntime, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelRequest != nil {
		return nil, false
	}
	copy := req
	r.cancelRequest = &copy
	return r.runtime, true
}

func (r *activeRun) cancellation() *CancelRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelRequest == nil {
		return nil
	}
	copy := *r.cancelRequest
	return &copy
}

func (r *activeRun) runtimeValue() AgentRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runtime
}

type activeRunRegistry struct {
	mu   sync.Mutex
	runs map[string]*activeRun
}

func newActiveRunRegistry() *activeRunRegistry {
	return &activeRunRegistry{runs: make(map[string]*activeRun)}
}

func (r *activeRunRegistry) register(runID string, cancel context.CancelFunc) *activeRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := &activeRun{cancel: cancel}
	r.runs[runID] = active
	return active
}

func (r *activeRunRegistry) get(runID string) *activeRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[runID]
}

func (r *activeRunRegistry) remove(runID string, active *activeRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs[runID] == active {
		delete(r.runs, runID)
	}
}
