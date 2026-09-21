package app

import (
	"context"
	"errors"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

// subAgentInvokerProxy breaks the intentional Composition Root cycle:
// Runtime -> Agent Gateway -> Orchestrator -> Runtime. It is wired once during
// startup and fails closed if execution somehow begins before installation.
type subAgentInvokerProxy struct {
	mu     sync.RWMutex
	target agentruntime.SubAgentInvoker
}

func (p *subAgentInvokerProxy) Set(target agentruntime.SubAgentInvoker) error {
	if target == nil {
		return errors.New("sub-agent invoker target is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.target != nil {
		return errors.New("sub-agent invoker already installed")
	}
	p.target = target
	return nil
}

func (p *subAgentInvokerProxy) Invoke(ctx context.Context, req agentruntime.SubAgentInvocationRequest, sink agentruntime.SubAgentEventSink) (agentruntime.SubAgentInvocationResult, error) {
	p.mu.RLock()
	target := p.target
	p.mu.RUnlock()
	if target == nil {
		return agentruntime.SubAgentInvocationResult{}, errors.New("Agent Gateway is not installed")
	}
	return target.Invoke(ctx, req, sink)
}

// ResumeChild 透传子 Run 恢复请求；目标不支持恢复端口时 fail-closed。
func (p *subAgentInvokerProxy) ResumeChild(ctx context.Context, req agentruntime.SubAgentResumeRequest, sink agentruntime.SubAgentEventSink) (agentruntime.SubAgentInvocationResult, error) {
	p.mu.RLock()
	target := p.target
	p.mu.RUnlock()
	if target == nil {
		return agentruntime.SubAgentInvocationResult{}, errors.New("Agent Gateway is not installed")
	}
	resumer, ok := target.(agentruntime.SubAgentResumer)
	if !ok {
		return agentruntime.SubAgentInvocationResult{}, errors.New("Agent Gateway does not support child resume")
	}
	return resumer.ResumeChild(ctx, req, sink)
}

var _ agentruntime.SubAgentInvoker = (*subAgentInvokerProxy)(nil)
var _ agentruntime.SubAgentResumer = (*subAgentInvokerProxy)(nil)
