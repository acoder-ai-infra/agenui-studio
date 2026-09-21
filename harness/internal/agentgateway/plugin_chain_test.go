package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
)

type testPluginInvoke func(context.Context, json.RawMessage, AuthorizedInvocation, GatewayInvocationFacts, GatewayNext) (agentruntime.SubAgentInvocationResult, error)

func testGatewayPlugin(id string, policy pluginFailurePolicy, invoke testPluginInvoke) GatewayPlugin {
	return GatewayPlugin{id: id, policy: policy, invoke: invoke}
}

func testInvocation() AuthorizedInvocation {
	return AuthorizedInvocation{
		TenantID: "tenant-1", SessionID: "session-1", ParentRunID: "run-parent",
		SubAgentRef: "child-agent", ProviderKind: gatewaycontract.SubAgentProviderLocalAgent,
		InvocationID: "task-1", Description: "do work",
	}
}

func TestDefaultPluginsUseFixedCoreOrder(t *testing.T) {
	chain, err := (&Service{plugins: defaultPluginCatalog(nil)}).pluginChain(nil)
	if err != nil {
		t.Fatal(err)
	}
	plugins := chain.plugins
	want := []string{defaultTracingPluginID, defaultAuditPluginID, defaultObservabilityPluginID}
	if len(plugins) != len(want) {
		t.Fatalf("plugins=%d want=%d", len(plugins), len(want))
	}
	for i := range want {
		if plugins[i].id != want[i] {
			t.Fatalf("plugin order=%v want=%v", pluginIDs(plugins), want)
		}
	}
}

func TestServicePluginChainCopiesStartupSlicePerRequest(t *testing.T) {
	service := &Service{plugins: defaultPluginCatalog(nil)}
	first, err := service.pluginChain(nil)
	if err != nil {
		t.Fatal(err)
	}
	first.plugins[0].id = "mutated"
	second, err := service.pluginChain(nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.plugins[0].id != defaultTracingPluginID {
		t.Fatalf("request-scoped chain mutated startup plugins: %v", pluginIDs(second.plugins))
	}
}

func TestPluginChainRunsFrozenOrder(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(value string) {
		mu.Lock()
		calls = append(calls, value)
		mu.Unlock()
	}
	plugin := func(id string) GatewayPlugin {
		return testGatewayPlugin(id, pluginFailClosed, func(ctx context.Context, _ json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
			record("enter:" + id)
			result, err := next(ctx)
			record("exit:" + id)
			return result, err
		})
	}
	chain := pluginChain{
		plugins: []GatewayPlugin{plugin("first"), plugin("second")},
		provider: func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
			record("provider")
			return agentruntime.SubAgentInvocationResult{Content: "ok"}, nil
		},
	}
	result, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
	if err != nil || result.Content != "ok" {
		t.Fatalf("Invoke() result=%+v err=%v", result, err)
	}
	want := []string{"enter:first", "enter:second", "provider", "exit:second", "exit:first"}
	if len(calls) != len(want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls=%v want=%v", calls, want)
		}
	}
}

func TestPluginChainReturnsSuccessfulPluginResult(t *testing.T) {
	bound := testGatewayPlugin("transform", pluginFailClosed, func(ctx context.Context, _ json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
		result, err := next(ctx)
		if err != nil {
			return result, err
		}
		result.Content = "plugin:" + result.Content
		return result, nil
	})
	chain := pluginChain{
		plugins: []GatewayPlugin{bound},
		provider: func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
			return agentruntime.SubAgentInvocationResult{Content: "provider"}, nil
		},
	}
	result, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
	if err != nil || result.Content != "plugin:provider" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPluginChainAppliesFailurePolicyBeforeProvider(t *testing.T) {
	pluginErr := errors.New("plugin failed")
	for _, tc := range []struct {
		name       string
		policy     pluginFailurePolicy
		panicValue any
		wantCalls  int32
		wantErr    error
	}{
		{name: "fail open error", policy: pluginFailOpen, wantCalls: 1},
		{name: "fail closed error", policy: pluginFailClosed, wantErr: ErrPluginFailed},
		{name: "fail open panic", policy: pluginFailOpen, panicValue: "boom", wantCalls: 1},
		{name: "fail closed panic", policy: pluginFailClosed, panicValue: "boom", wantErr: ErrPluginFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var providerCalls atomic.Int32
			bound := testGatewayPlugin("policy", tc.policy, func(context.Context, json.RawMessage, AuthorizedInvocation, GatewayInvocationFacts, GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
				if tc.panicValue != nil {
					panic(tc.panicValue)
				}
				return agentruntime.SubAgentInvocationResult{}, pluginErr
			})
			chain := pluginChain{plugins: []GatewayPlugin{bound}, provider: func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
				providerCalls.Add(1)
				return agentruntime.SubAgentInvocationResult{Content: "ok"}, nil
			}}
			result, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error=%v want=%v", err, tc.wantErr)
				}
			} else if err != nil || result.Content != "ok" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if providerCalls.Load() != tc.wantCalls {
				t.Fatalf("provider calls=%d want=%d", providerCalls.Load(), tc.wantCalls)
			}
		})
	}
}

func TestPluginChainNeverReplaysOrHidesProviderFailure(t *testing.T) {
	providerErr := errors.New("provider failed")
	for _, policy := range []pluginFailurePolicy{pluginFailOpen, pluginFailClosed} {
		t.Run(string(policy), func(t *testing.T) {
			var providerCalls atomic.Int32
			bound := testGatewayPlugin("post", policy, func(ctx context.Context, _ json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
				_, _ = next(ctx)
				return agentruntime.SubAgentInvocationResult{}, errors.New("plugin replacement")
			})
			chain := pluginChain{plugins: []GatewayPlugin{bound}, provider: func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
				providerCalls.Add(1)
				return agentruntime.SubAgentInvocationResult{}, providerErr
			}}
			_, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
			if !errors.Is(err, providerErr) {
				t.Fatalf("provider error was hidden: %v", err)
			}
			if providerCalls.Load() != 1 {
				t.Fatalf("provider calls=%d want=1", providerCalls.Load())
			}
		})
	}
}

func TestPluginChainPostCallFailurePolicyDoesNotReplayProvider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		policy    pluginFailurePolicy
		panicPost bool
		wantErr   error
	}{
		{name: "fail open error", policy: pluginFailOpen},
		{name: "fail closed error", policy: pluginFailClosed, wantErr: ErrPluginPostCallFailed},
		{name: "fail open panic", policy: pluginFailOpen, panicPost: true},
		{name: "fail closed panic", policy: pluginFailClosed, panicPost: true, wantErr: ErrPluginPostCallFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var providerCalls atomic.Int32
			bound := testGatewayPlugin("post", tc.policy, func(ctx context.Context, _ json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
				result, err := next(ctx)
				if err != nil {
					return result, err
				}
				if tc.panicPost {
					panic("post")
				}
				return agentruntime.SubAgentInvocationResult{}, errors.New("post failed")
			})
			chain := pluginChain{plugins: []GatewayPlugin{bound}, provider: func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
				providerCalls.Add(1)
				return agentruntime.SubAgentInvocationResult{Content: "authoritative"}, nil
			}}
			result, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error=%v want=%v", err, tc.wantErr)
				}
			} else if err != nil || result.Content != "authoritative" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if providerCalls.Load() != 1 {
				t.Fatalf("provider calls=%d want=1", providerCalls.Load())
			}
		})
	}
}

func TestPluginChainRejectsSerialDoubleNextEvenWhenPluginIgnoresError(t *testing.T) {
	var providerCalls atomic.Int32
	bound := testGatewayPlugin("double", pluginFailOpen, func(ctx context.Context, _ json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
		result, _ := next(ctx)
		_, _ = next(ctx)
		return result, nil
	})
	chain := pluginChain{plugins: []GatewayPlugin{bound}, provider: func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
		providerCalls.Add(1)
		return agentruntime.SubAgentInvocationResult{Content: "ok"}, nil
	}}
	_, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
	if !errors.Is(err, ErrPluginFailed) {
		t.Fatalf("error=%v want ErrPluginFailed", err)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider calls=%d want=1", providerCalls.Load())
	}
}

func TestPluginChainRejectsConcurrentDoubleNext(t *testing.T) {
	var providerCalls atomic.Int32
	bound := testGatewayPlugin("double", pluginFailClosed, func(ctx context.Context, _ json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				<-start
				_, _ = next(ctx)
			}()
		}
		close(start)
		wg.Wait()
		return agentruntime.SubAgentInvocationResult{Content: "ignored"}, nil
	})
	chain := pluginChain{plugins: []GatewayPlugin{bound}, provider: func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
		providerCalls.Add(1)
		return agentruntime.SubAgentInvocationResult{Content: "ok"}, nil
	}}
	_, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
	if !errors.Is(err, ErrPluginFailed) {
		t.Fatalf("error=%v want ErrPluginFailed", err)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider calls=%d want=1", providerCalls.Load())
	}
}

func TestServicePluginChainPrependsFixedCoreThenConfiguredOrder(t *testing.T) {
	service := &Service{plugins: defaultPluginCatalog(nil)}
	service.plugins["configured_first"] = testGatewayPlugin("configured_first", pluginFailClosed, func(ctx context.Context, config json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
		if string(config) != `{"position":1}` {
			t.Fatalf("first config=%s", config)
		}
		return next(ctx)
	})
	service.plugins["configured_second"] = testGatewayPlugin("configured_second", pluginFailClosed, func(ctx context.Context, config json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
		if string(config) != `{}` {
			t.Fatalf("second default config=%s", config)
		}
		return next(ctx)
	})
	chain, err := service.pluginChain([]gatewaycontract.GatewayPluginConfig{
		{PluginID: "configured_first", Config: json.RawMessage(`{"position":1}`)},
		{PluginID: "configured_second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		defaultTracingPluginID,
		defaultAuditPluginID,
		defaultObservabilityPluginID,
		"configured_first",
		"configured_second",
	}
	if got := pluginIDs(chain.plugins); !equalStrings(got, want) {
		t.Fatalf("plugin order=%v want=%v", got, want)
	}
	chain.provider = func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
		return agentruntime.SubAgentInvocationResult{Content: "ok"}, nil
	}
	if _, err := chain.Invoke(context.Background(), testInvocation(), newInvocationFacts()); err != nil {
		t.Fatal(err)
	}
}

func TestServicePluginChainRejectsReservedUnknownDuplicateAndInvalidConfig(t *testing.T) {
	service := &Service{plugins: defaultPluginCatalog(nil)}
	tests := []struct {
		name    string
		plugins []gatewaycontract.GatewayPluginConfig
		wantErr error
	}{
		{name: "reserved core", plugins: []gatewaycontract.GatewayPluginConfig{{PluginID: defaultTracingPluginID}}, wantErr: ErrPluginConfigInvalid},
		{name: "unknown", plugins: []gatewaycontract.GatewayPluginConfig{{PluginID: "not_registered"}}, wantErr: ErrPluginUnavailable},
		{name: "duplicate", plugins: []gatewaycontract.GatewayPluginConfig{{PluginID: basicValidatorPluginID}, {PluginID: basicValidatorPluginID}}, wantErr: ErrPluginConfigInvalid},
		{name: "non object", plugins: []gatewaycontract.GatewayPluginConfig{{PluginID: basicValidatorPluginID, Config: json.RawMessage(`[]`)}}, wantErr: ErrPluginConfigInvalid},
		{name: "unsupported properties", plugins: []gatewaycontract.GatewayPluginConfig{{PluginID: basicValidatorPluginID, Config: json.RawMessage(`{"mode":"strict"}`)}}, wantErr: ErrPluginConfigInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.pluginChain(tc.plugins); !errors.Is(err, tc.wantErr) {
				t.Fatalf("error=%v want=%v", err, tc.wantErr)
			}
		})
	}
}

func TestConfiguredPluginConfigIsIsolatedAcrossConcurrentRequests(t *testing.T) {
	service := &Service{plugins: defaultPluginCatalog(nil)}
	service.plugins["config_reader"] = testGatewayPlugin("config_reader", pluginFailClosed, func(ctx context.Context, config json.RawMessage, _ AuthorizedInvocation, _ GatewayInvocationFacts, next GatewayNext) (agentruntime.SubAgentInvocationResult, error) {
		if string(config) != `{"value":1}` {
			return agentruntime.SubAgentInvocationResult{}, errors.New("shared config mutated")
		}
		config[9] = '9'
		return next(ctx)
	})
	configured := []gatewaycontract.GatewayPluginConfig{{PluginID: "config_reader", Config: json.RawMessage(`{"value":1}`)}}
	const requests = 64
	errCh := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chain, err := service.pluginChain(configured)
			if err != nil {
				errCh <- err
				return
			}
			chain.provider = func(context.Context) (agentruntime.SubAgentInvocationResult, error) {
				return agentruntime.SubAgentInvocationResult{Content: "ok"}, nil
			}
			_, err = chain.Invoke(context.Background(), testInvocation(), newInvocationFacts())
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := string(configured[0].Config); got != `{"value":1}` {
		t.Fatalf("source config mutated: %s", got)
	}
}

func pluginIDs(plugins []GatewayPlugin) []string {
	ids := make([]string, len(plugins))
	for i := range plugins {
		ids[i] = plugins[i].id
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
