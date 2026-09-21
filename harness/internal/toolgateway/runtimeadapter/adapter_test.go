package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestAdapterIntegratesRealGatewayWithoutRebuildingLifecycle(t *testing.T) {
	const providerToolCallID = "call_01k20mksm0j0fmzx0y73x9tpxp"
	eventStore := toolgateway.NewMemoryEventStore()
	stepStore := &capturingRuntimeStepStore{}
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry: toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{{
			Name: "search", Version: "v1", Type: toolgateway.ToolTypeFunction,
			InputSchema: json.RawMessage(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}`),
			RiskLevel:   toolgateway.RiskLow, Timeout: time.Second,
			Visibility:  observability.VisibilityUserVisible,
			Function:    &toolgateway.FunctionToolSpec{HandlerName: "search"},
			Permissions: toolgateway.ToolPermissions{AllowedAgents: []string{"agent_1"}},
		}}),
		EventStore: eventStore,
		StepStore:  stepStore,
		Executors: []toolgateway.ToolExecutor{toolgateway.NewFunctionExecutor(map[string]toolgateway.FunctionTool{
			"search": func(context.Context, toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
				return &toolgateway.FunctionResult{Data: json.RawMessage(`{"answer":"ok"}`)}, nil
			},
		})},
	})
	adapter, err := New(gateway, InvocationResolverFunc(func(_ context.Context, req agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{
			StepID: composeToolStepID(req.RunID, req.ToolCallID), ParentStepID: "runtime_adapter_parent",
			Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
		}, nil
	}))
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	sink := &capturingSink{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_1", TenantID: "tenant_1", UserID: "user_1",
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
	})
	result, err := adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: providerToolCallID,
		ToolName: "search", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry,
		SourceRef: "search@v1", Arguments: json.RawMessage(`{"q":"west lake"}`),
	}, sink)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if result.IsError || result.Content != `{"answer":"ok"}` {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(sink.events) != 2 || sink.events[0].EventType != observability.EventToolCallStarted || sink.events[1].EventType != observability.EventToolCallCompleted {
		t.Fatalf("unexpected streamed lifecycle: %#v", sink.events)
	}
	persisted := eventStore.Events()
	if len(persisted) != len(sink.events) {
		t.Fatalf("persisted=%d streamed=%d", len(persisted), len(sink.events))
	}
	for index := range persisted {
		if persisted[index].EventID != sink.events[index].EventID || persisted[index].Sequence != sink.events[index].Sequence {
			t.Fatalf("event %d was rebuilt: persisted=%#v streamed=%#v", index, persisted[index], sink.events[index])
		}
	}
	if stepStore.startCalls != 1 || stepStore.completeCalls != 1 || stepStore.started.ParentStepID != "runtime_adapter_parent" ||
		stepStore.started.ToolCallID != providerToolCallID || stepStore.started.StepID != stepStore.completed.StepID || len(stepStore.started.StepID) > 64 {
		t.Fatalf("bounded tool step identity was not preserved: %#v", stepStore)
	}
	var startedPayload map[string]any
	if err := json.Unmarshal(sink.events[0].PayloadPreview, &startedPayload); err != nil || startedPayload["tool_call_id"] != providerToolCallID {
		t.Fatalf("provider tool_call_id was not preserved: payload=%#v err=%v", startedPayload, err)
	}
}

func TestAdapterPreservesModelContextWhenOnlySSEPreviewIsTruncated(t *testing.T) {
	const (
		previewLimit = 512
		modelLimit   = 16 << 10
	)
	fullResult := `{"items":"` + strings.Repeat("x", 8<<10) + `"}`
	artifactStore := artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry: toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{{
			Name:         "large_search",
			Version:      "v1",
			Type:         toolgateway.ToolTypeFunction,
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			RiskLevel:    toolgateway.RiskLow,
			Timeout:      time.Second,
			Visibility:   observability.VisibilityUserVisible,
			Function:     &toolgateway.FunctionToolSpec{HandlerName: "large_search"},
			Permissions:  toolgateway.ToolPermissions{AllowedAgents: []string{"agent_1"}},
			ResultPolicy: toolgateway.ToolOutputPolicy{
				MaxModelContextBytes:   modelLimit,
				MaxSSEPreviewBytes:     previewLimit,
				ArtifactThresholdBytes: 1024,
				RedactSensitiveFields:  true,
				RequireOutputSchema:    true,
				SummarizeWhenTruncated: true,
			},
		}}),
		EventStore:    toolgateway.NewMemoryEventStore(),
		ArtifactStore: artifactStore,
		Executors: []toolgateway.ToolExecutor{toolgateway.NewFunctionExecutor(map[string]toolgateway.FunctionTool{
			"large_search": func(context.Context, toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
				return &toolgateway.FunctionResult{Data: json.RawMessage(fullResult)}, nil
			},
		})},
	})
	adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{StepID: "step_1", ParentStepID: "parent_1", Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}}, nil
	}))
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_1", TenantID: "tenant_1", UserID: "user_1",
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
	})
	ctx = artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: "tenant_1", UserID: "user_1", SessionID: "session_1",
		RunID: "run_1", AgentID: "agent_1", Role: artifact.ActorRuntime,
	})

	result, err := adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "large_search", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry,
		SourceRef: "large_search@v1", Arguments: json.RawMessage(`{}`),
	}, &capturingSink{})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if result.Content != fullResult {
		t.Fatalf("runtime content bytes = %d, want full safe result bytes = %d", len(result.Content), len(fullResult))
	}
	if result.ContentRef == "" {
		t.Fatal("large result must retain its artifact reference")
	}
}

func TestAdapterMapsGatewayInterruptAndResumeWithoutRuntimeLeakage(t *testing.T) {
	var resumed *toolgateway.ToolCallResume
	stepStore := &capturingRuntimeStepStore{}
	gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
		Registry: toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{{
			Name: "approval", Version: "v1", Type: toolgateway.ToolTypeFunction,
			InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
			RiskLevel: toolgateway.RiskLow, Timeout: time.Second, Visibility: observability.VisibilityUserVisible,
			Function:    &toolgateway.FunctionToolSpec{HandlerName: "approval"},
			Permissions: toolgateway.ToolPermissions{AllowedAgents: []string{"agent_1"}},
		}}),
		EventStore: toolgateway.NewMemoryEventStore(),
		StepStore:  stepStore,
		Executors: []toolgateway.ToolExecutor{toolgateway.NewFunctionExecutor(map[string]toolgateway.FunctionTool{
			"approval": func(_ context.Context, call toolgateway.FunctionCall) (*toolgateway.FunctionResult, error) {
				if call.Resume == nil {
					return nil, &toolgateway.ToolInterruptedError{
						Info:  map[string]any{"type": "ask_user", "question": "continue?"},
						State: json.RawMessage(`{"phase":"waiting"}`),
					}
				}
				copy := *call.Resume
				resumed = &copy
				return &toolgateway.FunctionResult{Data: json.RawMessage(`{"answer":"yes"}`)}, nil
			},
		})},
	})
	adapter, err := New(gateway, InvocationResolverFunc(func(_ context.Context, req agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		parentStepID := "parent_initial"
		if req.Resume != nil {
			parentStepID = "parent_resume"
		}
		return ResolvedInvocation{
			StepID: composeToolStepID(req.RunID, req.ToolCallID), ParentStepID: parentStepID,
			Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_1", TenantID: "tenant_1", UserID: "user_1", SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
	})
	req := agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "approval", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry,
		SourceRef: "approval@v1", Arguments: json.RawMessage(`{}`),
	}
	firstSink := &capturingSink{}
	_, err = adapter.Invoke(ctx, req, firstSink)
	var interrupted *agentruntime.ToolInvocationInterruptedError
	if !errors.As(err, &interrupted) || string(interrupted.State) != `{"phase":"waiting"}` {
		t.Fatalf("adapter interrupt = %#v %v", interrupted, err)
	}
	if len(firstSink.events) != 1 || firstSink.events[0].EventType != observability.EventToolCallStarted {
		t.Fatalf("initial lifecycle = %#v", firstSink.events)
	}

	req.Resume = &agentruntime.ToolInvocationResume{
		WasInterrupted: true, IsResumeTarget: true,
		State: interrupted.State, Payload: json.RawMessage(`{"answer":"yes"}`),
	}
	resumeSink := &capturingSink{}
	result, err := adapter.Invoke(ctx, req, resumeSink)
	if err != nil || result.IsError || result.Content != `{"answer":"yes"}` {
		t.Fatalf("adapter resume = %#v %v", result, err)
	}
	if resumed == nil || !resumed.WasInterrupted || string(resumed.Payload) != `{"answer":"yes"}` {
		t.Fatalf("gateway resume = %#v", resumed)
	}
	if len(resumeSink.events) != 1 || resumeSink.events[0].EventType != observability.EventToolCallCompleted {
		t.Fatalf("resume lifecycle = %#v", resumeSink.events)
	}
	if stepStore.startCalls != 1 || stepStore.completeCalls != 1 || stepStore.started.ParentStepID != "parent_initial" ||
		stepStore.started.StepID != stepStore.completed.StepID {
		t.Fatalf("resume changed the durable tool step: %#v", stepStore)
	}
}

func TestAdapterPreservesCanonicalGatewayFailureClassification(t *testing.T) {
	tests := []struct {
		name          string
		allowedAgents []string
		arguments     json.RawMessage
		code          string
		errorType     observability.EventErrorType
		retryable     bool
	}{
		{
			name:          "permission denied",
			allowedAgents: []string{"another_agent"}, arguments: json.RawMessage(`{"q":"west lake"}`),
			code: "TOOL_PERMISSION_DENIED", errorType: observability.EventErrorPermissionDenied,
		},
		{
			name:          "schema rejected",
			allowedAgents: []string{"agent_1"}, arguments: json.RawMessage(`{}`),
			code: "TOOL_SCHEMA_VALIDATION_FAILED", errorType: observability.EventErrorSchemaValidation, retryable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gateway := toolgateway.NewGateway(toolgateway.GatewayConfig{
				Registry: toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{{
					Name: "search", Version: "v1", Type: toolgateway.ToolTypeFunction,
					InputSchema: json.RawMessage(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}`),
					RiskLevel:   toolgateway.RiskLow, Timeout: time.Second,
					Visibility:  observability.VisibilityDebug,
					Function:    &toolgateway.FunctionToolSpec{HandlerName: "search"},
					Permissions: toolgateway.ToolPermissions{AllowedAgents: tt.allowedAgents},
				}}),
				EventStore: toolgateway.NewMemoryEventStore(),
			})
			adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
				return ResolvedInvocation{StepID: "step_1", ParentStepID: "parent_1", Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			sink := &capturingSink{}
			ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
				TraceID: "trace_1", TenantID: "tenant_1", SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
			})
			result, err := adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
				SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
				ToolName: "search", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry,
				SourceRef: "search@v1", Arguments: tt.arguments,
			}, sink)
			if err != nil {
				t.Fatalf("controlled Gateway failure became an adapter error: %v", err)
			}
			if !result.IsError || len(sink.events) != 1 || sink.events[0].EventType != observability.EventToolCallFailed || sink.events[0].Error == nil {
				t.Fatalf("failure lifecycle=%#v result=%#v", sink.events, result)
			}
			if sink.events[0].Error.Code != tt.code || sink.events[0].Error.Type != tt.errorType || sink.events[0].Error.Retryable != tt.retryable {
				t.Fatalf("canonical failure error=%#v", sink.events[0].Error)
			}
		})
	}
}

func TestNativeDirectRuntimePassesFrozenRegistryVersionToAdapter(t *testing.T) {
	gateway := &directLifecycleGateway{}
	adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{StepID: "step_direct_1", ParentStepID: "parent_1", Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	model := &directNameOnlyModel{}
	runtime := agentruntime.NewNativeDirectRuntime(model, adapter, directContextRebuilder{})
	run := agentruntime.RunRequest{
		SessionID: "session_1", RunID: "run_1", TenantID: "tenant_1", UserID: "user_1",
		Definition: agentruntime.AgentDefinition{
			AgentID: "agent_1", AgentType: "assistant", Version: "v1",
			Runtime:  agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect},
			ToolRefs: []string{"search@v1"},
		},
	}
	pkg := directRegistryPackage("package_1")
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_1", TenantID: run.TenantID, UserID: run.UserID,
		SessionID: run.SessionID, RunID: run.RunID, AgentID: run.Definition.AgentID,
	})
	events, err := runtime.Run(agentruntime.WithModelContextPackage(ctx, pkg), run)
	if err != nil {
		t.Fatalf("run Native Direct: %v", err)
	}
	for range events {
	}

	if gateway.request.ToolName != "search" || gateway.request.ToolVersion != "v1" {
		t.Fatalf("Gateway identity=%q@%q, want search@v1", gateway.request.ToolName, gateway.request.ToolVersion)
	}
	if gateway.request.Metadata[toolgateway.MetadataHarnessSourceRef] != "search@v1" {
		t.Fatalf("frozen source ref missing: %#v", gateway.request.Metadata)
	}
}

func TestNativeDirectRuntimePreservesToolGatewayErrorSemantics(t *testing.T) {
	tests := []struct {
		name        string
		gatewayErr  error
		resolverErr error
		code        string
		errorType   observability.EventErrorType
		retryable   bool
		cancelled   bool
	}{
		{
			name:       "permission is permanent",
			gatewayErr: toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "sensitive detail", true, nil),
			code:       "TOOL_PERMISSION_DENIED", errorType: observability.EventErrorPermissionDenied,
		},
		{
			name:       "schema is permanent",
			gatewayErr: toolgateway.NewToolError(toolgateway.ErrorTypeSchemaValidationFailed, "sensitive detail", true, nil),
			code:       "TOOL_SCHEMA_VALIDATION_FAILED", errorType: observability.EventErrorSchemaValidation,
		},
		{
			name:       "business validation is not upstream",
			gatewayErr: toolgateway.NewToolError(toolgateway.ErrorTypeInvalidArgument, "sensitive detail", true, nil),
			code:       "TOOL_INVALID_ARGUMENT", errorType: observability.EventErrorSchemaValidation,
		},
		{
			name:       "upstream may retry",
			gatewayErr: toolgateway.NewToolError(toolgateway.ErrorTypeUpstreamError, "sensitive detail", true, nil),
			code:       "TOOL_UPSTREAM_ERROR", errorType: observability.EventErrorUpstream, retryable: true,
		},
		{
			name:       "rate limit retries",
			gatewayErr: toolgateway.NewToolError(toolgateway.ErrorTypeRateLimited, "sensitive detail", false, nil),
			code:       "TOOL_RATE_LIMITED", errorType: observability.EventErrorRateLimited, retryable: true,
		},
		{
			name:       "timeout retries",
			gatewayErr: context.DeadlineExceeded,
			code:       "TOOL_TIMEOUT", errorType: observability.EventErrorTimeout, retryable: true,
		},
		{
			name:       "cancellation terminates as cancelled",
			gatewayErr: toolgateway.NewToolError(toolgateway.ErrorTypeCancelled, "sensitive detail", true, nil),
			cancelled:  true,
		},
		{
			name:        "temporary capability failure retries",
			resolverErr: errors.Join(agentruntime.ErrProductionCapabilityUnavailable, errors.New("backend detail")),
			code:        "TOOL_CAPABILITY_UNAVAILABLE", errorType: observability.EventErrorUpstream, retryable: true,
		},
		{
			name:        "snapshot drift is permanent",
			resolverErr: ErrModelContextSnapshotMissing,
			code:        "TOOL_SNAPSHOT_DRIFT", errorType: observability.EventErrorSchemaValidation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gateway := &capturingGateway{err: tt.gatewayErr}
			adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
				if tt.resolverErr != nil {
					return ResolvedInvocation{}, tt.resolverErr
				}
				return ResolvedInvocation{StepID: "step_1", ParentStepID: "parent_1", Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			runtime := agentruntime.NewNativeDirectRuntime(&directNameOnlyModel{}, adapter, directContextRebuilder{})
			run := agentruntime.RunRequest{
				SessionID: "session_1", RunID: "run_1", TenantID: "tenant_1", UserID: "user_1",
				Definition: agentruntime.AgentDefinition{
					AgentID: "agent_1", AgentType: "assistant", Version: "v1",
					Runtime:  agentruntime.RuntimeSpec{Type: agentruntime.RuntimeTypeNative, Mode: agentruntime.RuntimeModeDirect},
					ToolRefs: []string{"search@v1"},
				},
			}
			ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
				TraceID: "trace_1", TenantID: run.TenantID, UserID: run.UserID,
				SessionID: run.SessionID, RunID: run.RunID, AgentID: run.Definition.AgentID,
			})
			events, err := runtime.Run(agentruntime.WithModelContextPackage(ctx, directRegistryPackage("package_1")), run)
			if err != nil {
				t.Fatalf("run Native Direct: %v", err)
			}
			var failed *observability.AgentEvent
			wasCancelled := false
			for event := range events {
				if event.EventType == observability.EventRunFailed {
					copy := event
					failed = &copy
				}
				wasCancelled = wasCancelled || event.EventType == observability.EventRunCancelled
			}
			if tt.cancelled {
				if !wasCancelled || failed != nil {
					t.Fatalf("cancellation lifecycle: cancelled=%v failed=%#v", wasCancelled, failed)
				}
				return
			}
			if failed == nil || failed.Error == nil {
				t.Fatalf("missing canonical failure event: %#v", failed)
			}
			if failed.Error.Code != tt.code || failed.Error.Type != tt.errorType || failed.Error.Retryable != tt.retryable {
				t.Fatalf("error=%#v, want code=%s type=%s retryable=%v", failed.Error, tt.code, tt.errorType, tt.retryable)
			}
			if failed.Error.Message == "sensitive detail" {
				t.Fatal("gateway detail leaked into canonical event")
			}
		})
	}
}

func TestAdapterMapsFrozenRuntimeInvocationAndPersistedEvent(t *testing.T) {
	event := observability.AgentEvent{EventID: "evt_1", Sequence: 7, EventType: observability.EventToolCallCompleted}
	gateway := &capturingGateway{
		event: event,
		result: &toolgateway.ToolCallResult{
			Status:             toolgateway.ToolCallSucceeded,
			ModelContextResult: []byte(`{"answer":"ok"}`),
			ResultRef:          "artifact://tenant_1/tool-result/result_1",
		},
	}
	resolver := InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{
			StepID: "step_1", ParentStepID: "parent_1",
			Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskHigh, Scopes: []string{"maps.read"}},
		}, nil
	})
	adapter, err := New(gateway, resolver)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	sink := &capturingSink{}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_1", SpanID: "span_1", TenantID: "tenant_1", UserID: "user_1",
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
	})
	result, err := adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "search", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry,
		SourceRef: "search@v1", Arguments: []byte(`{"q":"west lake"}`),
	}, sink)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if result.Content != `{"answer":"ok"}` || result.ContentRef == "" || result.IsError {
		t.Fatalf("unexpected runtime result: %#v", result)
	}
	if len(sink.events) != 1 || sink.events[0].EventID != event.EventID || sink.events[0].Sequence != event.Sequence {
		t.Fatalf("persisted event was rebuilt or lost: %#v", sink.events)
	}
	if gateway.request.StepID != "step_1" || gateway.request.ParentStepID != "parent_1" || gateway.request.TenantID != "tenant_1" || gateway.request.Policy.Scopes[0] != "maps.read" {
		t.Fatalf("gateway request was not resolved from trusted inputs: %#v", gateway.request)
	}
	if gateway.request.Metadata["harness.source_ref"] != "search@v1" {
		t.Fatalf("frozen source metadata missing: %#v", gateway.request.Metadata)
	}
	if gateway.request.ArgumentsPreview["q"] != "west lake" {
		t.Fatalf("arguments preview was not projected for Tool Gateway: %#v", gateway.request.ArgumentsPreview)
	}
}

func TestAdapterUsesTrustedGatewayIdentityForMCP(t *testing.T) {
	gateway := &capturingGateway{result: &toolgateway.ToolCallResult{Status: toolgateway.ToolCallSucceeded}}
	adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{
			StepID: "step_1", ParentStepID: "parent_1", ToolName: "lookup", ToolVersion: "mcp-v7",
			Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
		}, nil
	}))
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_1", TenantID: "tenant_1", SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
	})

	_, err = adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "lookup", Source: agentruntime.ToolSourceMCP,
		SourceRef: "maps", SnapshotRef: "mcp_snapshot_1", Arguments: json.RawMessage(`{"query":"west lake"}`),
	}, &capturingSink{})
	if err != nil {
		t.Fatalf("invoke MCP: %v", err)
	}
	if gateway.request.ToolName != "lookup" || gateway.request.ToolVersion != "mcp-v7" {
		t.Fatalf("Gateway mirror identity = %q@%q", gateway.request.ToolName, gateway.request.ToolVersion)
	}
	if gateway.request.Metadata[toolgateway.MetadataHarnessSourceRef] != "maps" ||
		gateway.request.Metadata[toolgateway.MetadataHarnessSnapshotRef] != "mcp_snapshot_1" {
		t.Fatalf("frozen MCP route lost: %#v", gateway.request.Metadata)
	}
}

func TestAdapterUsesTrustedGatewayIdentityForHTTPTool(t *testing.T) {
	gateway := &capturingGateway{result: &toolgateway.ToolCallResult{Status: toolgateway.ToolCallSucceeded}}
	adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{
			StepID: "step_1", ParentStepID: "parent_1", ToolName: "tenant_lookup", ToolVersion: "http-v3",
			Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
		}, nil
	}))
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TraceID: "trace_1", TenantID: "tenant_1", SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
	})

	_, err = adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "tenant_lookup", Source: agentruntime.ToolSourceHTTPTool,
		SourceRef: "tenant_lookup", SnapshotRef: "sha256:def", Arguments: json.RawMessage(`{"case_id":"ok"}`),
	}, &capturingSink{})
	if err != nil {
		t.Fatalf("invoke HTTP tool: %v", err)
	}
	if gateway.request.ToolName != "tenant_lookup" || gateway.request.ToolVersion != "http-v3" {
		t.Fatalf("Gateway mirror identity = %q@%q", gateway.request.ToolName, gateway.request.ToolVersion)
	}
	if gateway.request.Metadata[toolgateway.MetadataHarnessSourceRef] != "tenant_lookup" ||
		gateway.request.Metadata[toolgateway.MetadataHarnessSnapshotRef] != "sha256:def" {
		t.Fatalf("frozen HTTP tool route lost: %#v", gateway.request.Metadata)
	}
}

func TestAdapterRejectsMCPWithoutTrustedGatewayVersion(t *testing.T) {
	gateway := &capturingGateway{}
	adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{StepID: "step_1", ParentStepID: "parent_1", Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow}}, nil
	}))
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1", TenantID: "tenant_1"})

	_, err = adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "lookup", Source: agentruntime.ToolSourceMCP, SourceRef: "maps", SnapshotRef: "mcp_snapshot_1",
	}, &capturingSink{})
	if !errors.Is(err, ErrInvocationInvalid) || gateway.calls != 0 {
		t.Fatalf("missing trusted MCP version error=%v gateway_calls=%d", err, gateway.calls)
	}
}

func TestAdapterFailsClosedBeforeGatewayForInvalidFrozenRoute(t *testing.T) {
	gateway := &capturingGateway{}
	adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{StepID: "step_1", ParentStepID: "parent_1"}, nil
	}))
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1"})
	_, err = adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
		SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
		ToolName: "search", Source: agentruntime.ToolSourceMCP, SourceRef: "server_1",
	}, &capturingSink{})
	if !errors.Is(err, ErrInvocationInvalid) {
		t.Fatalf("invalid route error = %v", err)
	}
	if gateway.calls != 0 {
		t.Fatalf("gateway called for invalid frozen route: %d", gateway.calls)
	}
}

func TestAdapterFailsClosedBeforeGatewayForInvalidResolvedStepIdentity(t *testing.T) {
	tests := []struct {
		name     string
		resolved ResolvedInvocation
	}{
		{
			name: "oversized step id",
			resolved: ResolvedInvocation{
				StepID: strings.Repeat("s", 65), ParentStepID: "parent_1",
				Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
			},
		},
		{
			name: "oversized parent step id",
			resolved: ResolvedInvocation{
				StepID: "step_1", ParentStepID: strings.Repeat("p", 65),
				Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gateway := &capturingGateway{}
			adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
				return tt.resolved, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{
				TraceID: "trace_1", TenantID: "tenant_1", SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
			})
			_, err = adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
				SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
				ToolName: "search", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry, SourceRef: "search@v1",
			}, &capturingSink{})
			var runtimeErr *agentruntime.RuntimeError
			if !errors.As(err, &runtimeErr) || runtimeErr.Code != "TOOL_STEP_ID_INVALID" || runtimeErr.Retryable {
				t.Fatalf("invalid step identity error=%#v", err)
			}
			if !errors.Is(err, ErrToolStepIdentityInvalid) || gateway.calls != 0 {
				t.Fatalf("invalid step identity reached Gateway: err=%v calls=%d", err, gateway.calls)
			}
		})
	}
}

func TestAdapterIsSafeForConcurrentInvocations(t *testing.T) {
	gateway := &capturingGateway{result: &toolgateway.ToolCallResult{Status: toolgateway.ToolCallSucceeded}}
	adapter, err := New(gateway, InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (ResolvedInvocation, error) {
		return ResolvedInvocation{StepID: "step_1", ParentStepID: "parent_1", Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskHigh}}, nil
	}))
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace_1", TenantID: "tenant_1"})
	var wg sync.WaitGroup
	for index := 0; index < 32; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, invokeErr := adapter.Invoke(ctx, agentruntime.ToolInvocationRequest{
				SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", ToolCallID: "call_1",
				ToolName: "search", ToolVersion: "v1", Source: agentruntime.ToolSourceRegistry, SourceRef: "search@v1",
			}, &capturingSink{})
			if invokeErr != nil {
				t.Errorf("invoke: %v", invokeErr)
			}
		}()
	}
	wg.Wait()
}

type capturingGateway struct {
	mu      sync.Mutex
	calls   int
	request toolgateway.ToolCallRequest
	event   observability.AgentEvent
	result  *toolgateway.ToolCallResult
	err     error
}

type capturingRuntimeStepStore struct {
	startCalls    int
	completeCalls int
	failCalls     int
	started       toolgateway.StartToolStepRequest
	completed     toolgateway.CompleteToolStepRequest
}

func (s *capturingRuntimeStepStore) StartToolStep(_ context.Context, req toolgateway.StartToolStepRequest) (*toolgateway.StepSnapshot, error) {
	s.startCalls++
	s.started = req
	return &toolgateway.StepSnapshot{StepID: req.StepID, RunID: req.RunID, Status: "running"}, nil
}

func (s *capturingRuntimeStepStore) CompleteToolStep(_ context.Context, req toolgateway.CompleteToolStepRequest) error {
	s.completeCalls++
	s.completed = req
	return nil
}

func (s *capturingRuntimeStepStore) FailToolStep(context.Context, toolgateway.FailToolStepRequest) error {
	s.failCalls++
	return nil
}

func (g *capturingGateway) Invoke(ctx context.Context, req toolgateway.ToolCallRequest) (*toolgateway.ToolCallResult, error) {
	return g.InvokeWithEvents(ctx, req, nil)
}

func (g *capturingGateway) InvokeWithEvents(ctx context.Context, req toolgateway.ToolCallRequest, sink toolgateway.PersistedEventSink) (*toolgateway.ToolCallResult, error) {
	g.mu.Lock()
	g.calls++
	g.request = req
	g.mu.Unlock()
	if sink != nil && g.event.EventType != "" {
		if err := sink.Emit(ctx, g.event); err != nil {
			return nil, err
		}
	}
	return g.result, g.err
}

type capturingSink struct {
	mu     sync.Mutex
	events []observability.AgentEvent
}

func (s *capturingSink) Emit(_ context.Context, event observability.AgentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

type directNameOnlyModel struct {
	calls int
}

func (m *directNameOnlyModel) Invoke(_ context.Context, _ agentruntime.ModelInvokeRequest) (<-chan agentruntime.ModelStreamItem, error) {
	m.calls++
	items := []agentruntime.ModelStreamItem{
		{Event: observability.AgentEvent{EventType: observability.EventModelCallStarted}},
	}
	if m.calls == 1 {
		items = append(items, agentruntime.ModelStreamItem{
			Event: observability.AgentEvent{EventType: observability.EventModelToolCallDelta},
			ToolCall: &agentruntime.ModelToolCall{
				ToolCallID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"west lake"}`),
			},
		})
	}
	items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelCallCompleted}})
	out := make(chan agentruntime.ModelStreamItem, len(items))
	for _, item := range items {
		out <- item
	}
	close(out)
	return out, nil
}

type directContextRebuilder struct{}

func (directContextRebuilder) Rebuild(_ context.Context, _ agentruntime.ModelContextRebuildRequest) (agentruntime.ModelContextPackage, error) {
	return directRegistryPackage("package_2"), nil
}

func directRegistryPackage(packageID string) agentruntime.ModelContextPackage {
	pkg := agentruntime.ModelContextPackage{
		SchemaVersion: agentruntime.ModelContextPackageSchemaVersion,
		PackageID:     packageID,
		Run: agentruntime.ModelContextRun{
			SessionID: "session_1", RunID: "run_1", AgentID: "agent_1",
			Runtime: string(agentruntime.RuntimeTypeNative), RuntimeMode: string(agentruntime.RuntimeModeDirect),
		},
		Capabilities: agentruntime.ModelContextCapabilities{
			Tools: []string{"search@v1"},
			ToolDefinitions: []agentruntime.ModelToolDefinition{{
				Name: "search", Schema: json.RawMessage(`{"type":"object"}`),
			}},
		},
	}
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	return pkg
}

type directLifecycleGateway struct {
	request toolgateway.ToolCallRequest
}

func (g *directLifecycleGateway) Invoke(ctx context.Context, req toolgateway.ToolCallRequest) (*toolgateway.ToolCallResult, error) {
	return g.InvokeWithEvents(ctx, req, nil)
}

func (g *directLifecycleGateway) InvokeWithEvents(ctx context.Context, req toolgateway.ToolCallRequest, sink toolgateway.PersistedEventSink) (*toolgateway.ToolCallResult, error) {
	g.request = req
	if sink != nil {
		for _, eventType := range []observability.EventType{observability.EventToolCallStarted, observability.EventToolCallCompleted} {
			if err := sink.Emit(ctx, observability.AgentEvent{EventType: eventType}); err != nil {
				return nil, err
			}
		}
	}
	return &toolgateway.ToolCallResult{Status: toolgateway.ToolCallSucceeded, ModelContextResult: json.RawMessage(`{"answer":"ok"}`)}, nil
}
