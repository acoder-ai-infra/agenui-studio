package agentgateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
)

type pluginFailurePolicy string

const (
	pluginFailClosed pluginFailurePolicy = "fail_closed"
	pluginFailOpen   pluginFailurePolicy = "fail_open"
)

// pluginChain is a request-scoped execution view. Plugin implementations are
// shared by the Service, while all exactly-once and failure state stays here.
type pluginChain struct {
	plugins  []GatewayPlugin
	provider GatewayNext
}

func (s *Service) pluginChain(configured []gatewaycontract.GatewayPluginConfig) (pluginChain, error) {
	plugins := make([]GatewayPlugin, 0, len(fixedCorePluginIDs)+len(configured))
	for _, id := range fixedCorePluginIDs {
		plugin, ok := s.plugins[id]
		if !ok || plugin.invoke == nil {
			return pluginChain{}, newGatewayError(CodePluginUnavailable, "required plugin unavailable: "+id, nil)
		}
		plugin.config = json.RawMessage(`{}`)
		plugins = append(plugins, plugin)
	}

	seen := make(map[string]struct{}, len(configured))
	for _, selected := range configured {
		id := strings.TrimSpace(selected.PluginID)
		if id == "" {
			return pluginChain{}, newGatewayError(CodePluginConfigInvalid, "plugin id is empty", nil)
		}
		if isFixedCorePlugin(id) {
			return pluginChain{}, newGatewayError(CodePluginConfigInvalid, "fixed core plugin cannot be configured: "+id, nil)
		}
		if _, exists := seen[id]; exists {
			return pluginChain{}, newGatewayError(CodePluginConfigInvalid, "duplicate configured plugin: "+id, nil)
		}
		seen[id] = struct{}{}
		plugin, ok := s.plugins[id]
		if !ok || plugin.invoke == nil {
			return pluginChain{}, newGatewayError(CodePluginUnavailable, "configured plugin unavailable: "+id, nil)
		}
		config, err := normalizePluginConfig(selected.Config)
		if err != nil {
			return pluginChain{}, newGatewayError(CodePluginConfigInvalid, "invalid config for plugin "+id, err)
		}
		if plugin.validate != nil {
			if err := plugin.validate(config); err != nil {
				return pluginChain{}, newGatewayError(CodePluginConfigInvalid, "invalid config for plugin "+id, err)
			}
		}
		plugin.config = config
		plugins = append(plugins, plugin)
	}
	return pluginChain{plugins: plugins}, nil
}

func normalizePluginConfig(input json.RawMessage) (json.RawMessage, error) {
	return gatewaycontract.CanonicalizePluginConfig(input)
}

func cloneRawMessage(input json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), input...)
}

func (c pluginChain) Invoke(ctx context.Context, req AuthorizedInvocation, facts GatewayInvocationFacts) (agentruntime.SubAgentInvocationResult, error) {
	if c.provider == nil {
		return agentruntime.SubAgentInvocationResult{}, ErrProviderUnavailable
	}
	return c.invokeAt(ctx, 0, req, facts)
}

func (c pluginChain) invokeAt(ctx context.Context, index int, req AuthorizedInvocation, facts GatewayInvocationFacts) (agentruntime.SubAgentInvocationResult, error) {
	if index == len(c.plugins) {
		return c.provider(ctx)
	}
	plugin := c.plugins[index]
	if plugin.invoke == nil {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodePluginFailed, "nil plugin implementation", nil)
	}
	next := func(nextCtx context.Context) (agentruntime.SubAgentInvocationResult, error) {
		return c.invokeAt(nextCtx, index+1, req, facts)
	}
	return invokePlugin(ctx, plugin, req, facts, next)
}

type guardedNextState struct {
	mu sync.Mutex

	called    bool
	closed    bool
	inflight  bool
	violation bool
	done      chan struct{}

	result agentruntime.SubAgentInvocationResult
	err    error
}

func newGuardedNextState() *guardedNextState {
	return &guardedNextState{done: make(chan struct{})}
}

func (s *guardedNextState) invoke(ctx context.Context, next GatewayNext) (result agentruntime.SubAgentInvocationResult, err error) {
	s.mu.Lock()
	if s.closed || s.called {
		s.violation = true
		s.mu.Unlock()
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodePluginFailed, "plugin called next more than once or after returning", nil)
	}
	s.called = true
	s.inflight = true
	s.mu.Unlock()

	defer func() {
		recovered := recover()
		s.mu.Lock()
		s.result = result
		s.err = err
		s.inflight = false
		close(s.done)
		s.mu.Unlock()
		if recovered != nil {
			panic(recovered)
		}
	}()
	return next(ctx)
}

func (s *guardedNextState) closeAndSnapshot() (called, violation bool, result agentruntime.SubAgentInvocationResult, err error) {
	s.mu.Lock()
	s.closed = true
	called = s.called
	inflight := s.inflight
	done := s.done
	s.mu.Unlock()

	if inflight {
		<-done
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called, s.violation, s.result, s.err
}

func invokePlugin(
	ctx context.Context,
	plugin GatewayPlugin,
	req AuthorizedInvocation,
	facts GatewayInvocationFacts,
	next GatewayNext,
) (result agentruntime.SubAgentInvocationResult, err error) {
	state := newGuardedNextState()
	guardedNext := func(nextCtx context.Context) (agentruntime.SubAgentInvocationResult, error) {
		return state.invoke(nextCtx, next)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		result, err = plugin.Invoke(ctx, req, facts, guardedNext)
	}()

	called, violation, downstreamResult, downstreamErr := state.closeAndSnapshot()
	if violation {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(
			CodePluginFailed,
			"plugin "+plugin.id+" violated exactly-once next",
			nil,
		)
	}
	if called && downstreamErr != nil {
		// A downstream/provider error is authoritative. A plugin cannot hide it
		// or trigger a second provider call by returning a different error.
		return downstreamResult, downstreamErr
	}
	if recovered != nil {
		return recoverPluginFailure(ctx, plugin, next, called, downstreamResult)
	}
	if !called {
		if err != nil && plugin.policy == pluginFailClosed {
			return agentruntime.SubAgentInvocationResult{}, pluginFailure(plugin, false)
		}
		if plugin.policy == pluginFailOpen {
			return next(ctx)
		}
		return agentruntime.SubAgentInvocationResult{}, pluginFailure(plugin, false)
	}
	if err != nil {
		if plugin.policy == pluginFailOpen {
			return downstreamResult, nil
		}
		return agentruntime.SubAgentInvocationResult{}, pluginFailure(plugin, true)
	}
	return result, nil
}

func recoverPluginFailure(
	ctx context.Context,
	plugin GatewayPlugin,
	next GatewayNext,
	called bool,
	downstreamResult agentruntime.SubAgentInvocationResult,
) (agentruntime.SubAgentInvocationResult, error) {
	if !called && plugin.policy == pluginFailOpen {
		return next(ctx)
	}
	if called && plugin.policy == pluginFailOpen {
		return downstreamResult, nil
	}
	return agentruntime.SubAgentInvocationResult{}, pluginFailure(plugin, called)
}

func pluginFailure(plugin GatewayPlugin, postCall bool) error {
	code := CodePluginFailed
	phase := " failed"
	if postCall {
		code = CodePluginPostCallFailed
		phase = " failed after provider entry"
	}
	return newGatewayError(code, "plugin "+plugin.id+phase, nil)
}
