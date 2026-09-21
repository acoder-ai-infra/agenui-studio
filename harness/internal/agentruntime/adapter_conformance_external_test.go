package agentruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntimetest"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway/runtimeadapter"
)

func TestNativeDirectRuntimeAdapterConformance(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeNative, agentruntime.RuntimeModeDirect)
	agentruntimetest.Run(t, agentruntimetest.Contract{
		Name:       agentruntime.RuntimeTypeNative,
		Definition: definition,
		New: func(*testing.T) agentruntime.AgentRuntime {
			return agentruntime.NewNativeDirectRuntime(conformanceModelInvoker{}, nil, nil)
		},
		Context: conformanceContext,
	})
}

func TestEinoRuntimeAdapterConformance(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeReact)
	store := newMemoryCheckpointStore()
	agent := &scriptedEinoAgent{
		run: func(context.Context, *adk.AgentInput) *adk.AsyncIterator[*adk.AgentEvent] {
			return einoIterator(adk.EventFromMessage(schema.AssistantMessage("ok", nil), nil, schema.Assistant, ""))
		},
	}
	resumeStore := newMemoryCheckpointStore()
	resumeAgent := &scriptedEinoAgent{
		run: func(ctx context.Context, _ *adk.AgentInput) *adk.AsyncIterator[*adk.AgentEvent] {
			return einoIterator(adk.Interrupt(ctx, "resume conformance"))
		},
		resume: func(context.Context, *adk.ResumeInfo) *adk.AsyncIterator[*adk.AgentEvent] {
			return einoIterator(adk.EventFromMessage(schema.AssistantMessage("resumed", nil), nil, schema.Assistant, ""))
		},
	}
	agentruntimetest.Run(t, agentruntimetest.Contract{
		Name:       agentruntime.RuntimeTypeEino,
		Definition: definition,
		New: func(*testing.T) agentruntime.AgentRuntime {
			return newTestEinoRuntime(agent, store, agentruntime.DefaultEinoControlRequestFactory{})
		},
		Context: conformanceContext,
		Resume: &agentruntimetest.ResumeContract{
			New: func(*testing.T) agentruntime.AgentRuntime {
				return newTestEinoRuntime(resumeAgent, resumeStore, agentruntime.DefaultEinoControlRequestFactory{})
			},
			Prepare: func(t *testing.T, runtime agentruntime.AgentRuntime) (context.Context, agentruntime.ResumeRequest) {
				req := conformanceRequest(definition, "adapter_resume")
				events, err := runtime.Run(conformanceContext(req), req)
				if err != nil {
					t.Fatalf("prepare interrupted run: %v", err)
				}
				checkpointID := ""
				for _, event := range collectEvents(events) {
					if event.EventType != observability.EventCheckpointCreated {
						continue
					}
					var payload struct {
						CheckpointID string `json:"checkpoint_id"`
					}
					_ = json.Unmarshal(event.Payload, &payload)
					checkpointID = payload.CheckpointID
				}
				if checkpointID == "" {
					t.Fatal("prepare interrupted run emitted no checkpoint")
				}
				return conformanceContext(req), agentruntime.ResumeRequest{
					SessionID:        req.SessionID,
					RunID:            req.RunID,
					Definition:       definition,
					CheckpointID:     checkpointID,
					ControlRequestID: "conformance_control",
					ResumeToken:      "conformance_token",
					Trace:            req.Trace,
				}
			},
		},
	})
}

func TestEinoRuntimeCancellationUnblocksIterator(t *testing.T) {
	exited := make(chan struct{})
	agent := &scriptedEinoAgent{
		run: func(ctx context.Context, _ *adk.AgentInput) *adk.AsyncIterator[*adk.AgentEvent] {
			iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
			go func() {
				defer close(exited)
				defer generator.Close()
				<-ctx.Done()
			}()
			return iterator
		},
	}
	runtime := newTestEinoRuntime(agent, newMemoryCheckpointStore(), agentruntime.DefaultEinoControlRequestFactory{})
	req := conformanceRequest(conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeReact), "cancel_blocked_iterator")
	ctx, cancel := context.WithCancel(conformanceContext(req))
	events, err := runtime.Run(ctx, req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	cancel()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("Eino agent producer did not exit after context cancellation")
	}
	select {
	case _, ok := <-events:
		if ok {
			for range events {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("Eino runtime stream did not close after iterator cancellation")
	}
}

func TestEinoManagedDeepAgentRunsThroughRealADK(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent)
	runtime := agentruntime.NewEinoRuntime(
		agentruntime.NewEinoDeepAgentFactory(),
		agentruntime.RuntimeEnvironment{
			Models:      conformanceModelInvoker{},
			Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: newMemoryCheckpointStore()},
		},
		agentruntime.DefaultEinoControlRequestFactory{},
	)
	req := conformanceRequest(definition, "real_deep")
	events, err := runtime.Run(conformanceContext(req), req)
	if err != nil {
		t.Fatalf("run real DeepAgent: %v", err)
	}
	collected := collectEvents(events)
	for _, eventType := range []observability.EventType{
		observability.EventAgentStarted,
		observability.EventModelCallStarted,
		observability.EventModelTokenDelta,
		observability.EventModelCallCompleted,
		observability.EventAgentTextDelta,
		observability.EventAgentCompleted,
		observability.EventRunCompleted,
	} {
		if indexEvent(collected, eventType) < 0 {
			t.Fatalf("real DeepAgent did not emit %s: %#v", eventType, collected)
		}
	}
	if err := agentruntimetest.ValidateStream(collected); err != nil {
		t.Fatalf("real DeepAgent lifecycle invalid: %v", err)
	}
}

func TestEinoManagedDeepAgentDelegatesTaskThroughGatewayProxy(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent)
	definition.SubAgentRefs = []string{"researcher"}
	modelInvoker := &deepTaskModelInvoker{}
	subAgents := &deepTaskSubAgentInvoker{}
	runtime := agentruntime.NewEinoRuntime(
		agentruntime.NewEinoDeepAgentFactory(),
		agentruntime.RuntimeEnvironment{
			Models:      modelInvoker,
			SubAgents:   subAgents,
			Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: newMemoryCheckpointStore()},
		},
		agentruntime.DefaultEinoControlRequestFactory{},
	)
	req := conformanceRequest(definition, "real_deep_task")
	events, err := runtime.Run(conformanceContext(req), req)
	if err != nil {
		t.Fatalf("run DeepAgent task: %v", err)
	}
	collected := collectEvents(events)
	if subAgents.calls.Load() != 1 {
		t.Fatalf("sub-agent gateway calls = %d", subAgents.calls.Load())
	}
	start := indexEvent(collected, observability.EventSubAgentStarted)
	completed := indexEvent(collected, observability.EventSubAgentCompleted)
	finalText := indexEvent(collected, observability.EventAgentTextDelta)
	if start < 0 || completed < start || finalText < completed {
		t.Fatalf("DeepAgent task lifecycle invalid: %#v", collected)
	}
	if err := agentruntimetest.ValidateStream(collected); err != nil {
		t.Fatalf("DeepAgent task stream invalid: %v", err)
	}
}

func TestEinoManagedDeepAgentExecutesAuthorizedToolThroughGatewayProxy(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent)
	definition.ToolRefs = []string{"search@v2"}
	modelInvoker := &deepToolModelInvoker{}
	tools := &deepToolInvoker{}
	nativeErrors := &capturingEinoErrors{}
	runtime := agentruntime.NewEinoRuntime(
		agentruntime.NewEinoDeepAgentFactory(),
		agentruntime.RuntimeEnvironment{
			Models:      modelInvoker,
			Tools:       tools,
			Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: newMemoryCheckpointStore()},
		},
		agentruntime.DefaultEinoControlRequestFactory{},
	)
	runtime.ToolInfos = agentruntime.EinoToolInfoResolverFunc(func(_ context.Context, refs []string) ([]agentruntime.EinoResolvedTool, error) {
		if len(refs) != 1 || refs[0] != "search@v2" {
			return nil, errors.New("unexpected tool refs")
		}
		return []agentruntime.EinoResolvedTool{{Ref: "search@v2", Info: &schema.ToolInfo{
			Name: "search",
			Desc: "search documents",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"query": {Type: schema.String, Required: true},
			}),
		}}}, nil
	})
	runtime.Executions = capturingEinoExecutionFactory{errors: nativeErrors}
	req := conformanceRequest(definition, "real_deep_tool")
	events, err := runtime.Run(conformanceContext(req), req)
	if err != nil {
		t.Fatalf("run DeepAgent tool: %v", err)
	}
	collected := collectEvents(events)
	if tools.calls.Load() != 1 {
		var runErr *observability.EventError
		if len(collected) > 0 {
			runErr = collected[len(collected)-1].Error
		}
		t.Fatalf("tool gateway calls = %d error=%+v native_errors=%v events=%#v", tools.calls.Load(), runErr, nativeErrors.snapshot(), collected)
	}
	start := indexEvent(collected, observability.EventToolCallStarted)
	completed := indexEvent(collected, observability.EventToolCallCompleted)
	finalText := indexEvent(collected, observability.EventAgentTextDelta)
	if start < 0 || completed < start || finalText < completed {
		t.Fatalf("DeepAgent tool lifecycle invalid: %#v", collected)
	}
	if err := agentruntimetest.ValidateStream(collected); err != nil {
		t.Fatalf("DeepAgent tool stream invalid: %v", err)
	}
}

// TestEinoRuntimeServiceCommitsModelCompletionBeforeToolSideEffect exercises the
// real Eino DeepAgent/ADK loop through RuntimeService. The State Manager gate
// makes the persistence race deterministic: an executable tool call must not be
// released until Event Store has durably assigned model_call_completed.sequence.
func TestEinoRuntimeServiceCommitsModelCompletionBeforeToolSideEffect(t *testing.T) {
	t.Run("forced append delay preserves durable order and live replay parity", func(t *testing.T) {
		state := newCompletionGateState(nil)
		runtime, tools := newSequenceBarrierEinoRuntime()
		service := agentruntime.NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
		req := conformanceRequest(conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent), "eino_commit_delay")
		req.Definition.ToolRefs = []string{"search@v2"}

		ctx, cancel := context.WithTimeout(conformanceContext(req), 5*time.Second)
		defer cancel()
		events, err := service.Run(ctx, req)
		if err != nil {
			t.Fatalf("run Eino RuntimeService: %v", err)
		}
		collected := make(chan []observability.AgentEvent, 1)
		go func() { collected <- collectEvents(events) }()
		state.waitUntilCompletionAppend(t)
		assertToolNotCalled(t, tools)
		close(state.release)
		live := waitForCollectedEvents(t, collected)

		if got := tools.calls.Load(); got != 1 {
			t.Fatalf("tool calls=%d, want 1 after durable completion", got)
		}
		stored := state.Events(req.RunID)
		assertLiveStoreExactParity(t, live, stored)
		completion := firstEvent(stored, observability.EventModelCallCompleted)
		toolStarted := firstEvent(stored, observability.EventToolCallStarted)
		if completion == nil || toolStarted == nil {
			t.Fatalf("missing completion/tool start: %#v", stored)
		}
		if toolStarted.Sequence != completion.Sequence+1 {
			t.Fatalf("durable causal order violated: model completion seq=%d tool start seq=%d", completion.Sequence, toolStarted.Sequence)
		}
	})

	t.Run("append failure releases waiter without executing tool", func(t *testing.T) {
		appendErr := errors.New("forced model completion append failure")
		state := newCompletionGateState(appendErr)
		runtime, tools := newSequenceBarrierEinoRuntime()
		service := agentruntime.NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
		req := conformanceRequest(conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent), "eino_commit_failure")
		req.Definition.ToolRefs = []string{"search@v2"}

		ctx, cancel := context.WithTimeout(conformanceContext(req), 5*time.Second)
		defer cancel()
		events, err := service.Run(ctx, req)
		if err != nil {
			t.Fatalf("run Eino RuntimeService: %v", err)
		}
		collected := make(chan []observability.AgentEvent, 1)
		go func() { collected <- collectEvents(events) }()
		state.waitUntilCompletionAppend(t)
		assertToolNotCalled(t, tools)
		close(state.release)
		_ = waitForCollectedEvents(t, collected)
		assertToolNotCalled(t, tools)
		assertNoToolEvents(t, state.Events(req.RunID))
	})

	t.Run("cancellation releases waiter without executing tool", func(t *testing.T) {
		state := newCompletionGateState(nil)
		runtime, tools := newSequenceBarrierEinoRuntime()
		service := agentruntime.NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
		req := conformanceRequest(conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent), "eino_commit_cancel")
		req.Definition.ToolRefs = []string{"search@v2"}

		ctx, cancel := context.WithTimeout(conformanceContext(req), 5*time.Second)
		defer cancel()
		events, err := service.Run(ctx, req)
		if err != nil {
			t.Fatalf("run Eino RuntimeService: %v", err)
		}
		collected := make(chan []observability.AgentEvent, 1)
		go func() { collected <- collectEvents(events) }()
		state.waitUntilCompletionAppend(t)
		assertToolNotCalled(t, tools)
		if err := service.Cancel(context.Background(), agentruntime.CancelRequest{SessionID: req.SessionID, RunID: req.RunID, Reason: "test cancellation"}); err != nil {
			t.Fatalf("cancel Eino RuntimeService: %v", err)
		}
		_ = waitForCollectedEvents(t, collected)
		assertToolNotCalled(t, tools)
		assertNoToolEvents(t, state.Events(req.RunID))
	})
}

func TestEinoManagedDeepAgentPreservesToolGatewayErrorSemantics(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      string
		errorType observability.EventErrorType
		retryable bool
		cancelled bool
	}{
		{
			name: "permission is permanent",
			err:  toolgateway.NewToolError(toolgateway.ErrorTypePermissionDenied, "private gateway detail", true, nil),
			code: "TOOL_PERMISSION_DENIED", errorType: observability.EventErrorPermissionDenied,
		},
		{
			name: "upstream may retry",
			err:  toolgateway.NewToolError(toolgateway.ErrorTypeUpstreamError, "private gateway detail", true, nil),
			code: "TOOL_UPSTREAM_ERROR", errorType: observability.EventErrorUpstream, retryable: true,
		},
		{
			name:      "cancellation terminates as cancelled",
			err:       toolgateway.NewToolError(toolgateway.ErrorTypeCancelled, "private gateway detail", true, nil),
			cancelled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, err := runtimeadapter.New(
				failingStreamingToolGateway{err: tt.err},
				runtimeadapter.InvocationResolverFunc(func(context.Context, agentruntime.ToolInvocationRequest) (runtimeadapter.ResolvedInvocation, error) {
					return runtimeadapter.ResolvedInvocation{
						StepID: "step_deep_tool_1", ParentStepID: "parent_deep_tool_1",
						Policy: toolgateway.ToolCallPolicy{RiskLevel: toolgateway.RiskLow},
					}, nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent)
			definition.ToolRefs = []string{"search@v2"}
			runtime := agentruntime.NewEinoRuntime(
				agentruntime.NewEinoDeepAgentFactory(),
				agentruntime.RuntimeEnvironment{
					Models:      &deepToolModelInvoker{},
					Tools:       adapter,
					Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: newMemoryCheckpointStore()},
				},
				agentruntime.DefaultEinoControlRequestFactory{},
			)
			req := conformanceRequest(definition, "deep_tool_error")
			events, err := runtime.Run(conformanceContext(req), req)
			if err != nil {
				t.Fatalf("run DeepAgent tool: %v", err)
			}
			collected := collectEvents(events)
			if tt.cancelled {
				if indexEvent(collected, observability.EventRunCancelled) < 0 || indexEvent(collected, observability.EventRunFailed) >= 0 {
					t.Fatalf("cancellation lifecycle invalid: %#v", collected)
				}
				return
			}
			failedIndex := indexEvent(collected, observability.EventRunFailed)
			if failedIndex < 0 || collected[failedIndex].Error == nil {
				t.Fatalf("missing canonical failure: %#v", collected)
			}
			failed := collected[failedIndex].Error
			if failed.Code != tt.code || failed.Type != tt.errorType || failed.Retryable != tt.retryable {
				t.Fatalf("error=%#v, want code=%s type=%s retryable=%v", failed, tt.code, tt.errorType, tt.retryable)
			}
			if failed.Message == "private gateway detail" {
				t.Fatal("gateway detail leaked into canonical event")
			}
		})
	}
}

func TestEinoManagedDeepAgentResumesInterruptedToolAfterRuntimeRebuild(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeDeepAgent)
	definition.ToolRefs = []string{"approval@v1"}
	checkpointStore := newMemoryCheckpointStore()
	state := agentruntime.NewInMemoryStateManager()
	modelInvoker := &deepInterruptModelInvoker{}
	toolInvoker := &deepInterruptToolInvoker{}
	nativeErrors := &capturingEinoErrors{}
	const (
		controlID   = "control_deep_tool"
		resumeToken = "resume_deep_tool_secret"
	)
	controls := agentruntime.EinoControlRequestFactoryFunc(func(_ context.Context, req agentruntime.EinoControlRequestFactoryRequest) (agentruntime.EinoControlRequest, error) {
		return agentruntime.EinoControlRequest{
			Binding: agentruntime.WaitingControlRequest{ControlRequestID: controlID, ResumeToken: resumeToken},
			Type:    "ask_user",
			Payload: mustJSON(map[string]any{"question": "approve?", "checkpoint_id": req.CheckpointID}),
		}, nil
	})
	newRuntime := func() *agentruntime.EinoRuntime {
		runtime := agentruntime.NewEinoRuntime(
			agentruntime.NewEinoDeepAgentFactory(),
			agentruntime.RuntimeEnvironment{
				Models:      modelInvoker,
				Tools:       toolInvoker,
				Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: checkpointStore},
			},
			controls,
		)
		runtime.ToolInfos = approvalToolResolver()
		runtime.Executions = capturingEinoExecutionFactory{errors: nativeErrors}
		return runtime
	}

	service := agentruntime.NewRuntimeService(newRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.ControlRequests = stateControlCreator{state: state}
	req := conformanceRequest(definition, "deep_tool_resume")
	firstStream, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run interrupted DeepAgent: %v", err)
	}
	first := collectEvents(firstStream)
	run, ok := state.Run(req.RunID)
	if !ok || run.Status != agentruntime.RunStatusWaitingControl || run.CheckpointID == "" || run.PendingControlRequestID != controlID {
		t.Fatalf("interrupted run state = %#v native_errors=%v events=%#v", run, nativeErrors.snapshot(), first)
	}
	interruptContextID := firstInterruptContextID(t, first)
	if toolInvoker.initialCalls.Load() != 1 || toolInvoker.resumeCalls.Load() != 0 {
		t.Fatalf("tool calls before resume: initial=%d resume=%d", toolInvoker.initialCalls.Load(), toolInvoker.resumeCalls.Load())
	}

	// Simulate process/runtime reconstruction while retaining durable state and checkpoint ports.
	service = agentruntime.NewRuntimeService(newRuntime(), state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.ControlRequests = stateControlCreator{state: state}
	resumedStream, err := service.Resume(context.Background(), agentruntime.ResumeRequest{
		SessionID: req.SessionID, RunID: req.RunID, Definition: definition,
		CheckpointID: run.CheckpointID, ControlRequestID: controlID, ResumeToken: resumeToken,
		ControlPayload: mustJSON(map[string]any{"targets": map[string]any{interruptContextID: map[string]any{"approved": true}}}),
		Trace:          req.Trace,
	})
	if err != nil {
		t.Fatalf("resume rebuilt DeepAgent: %v", err)
	}
	resumed := collectEvents(resumedStream)
	run, _ = state.Run(req.RunID)
	if run.Status != agentruntime.RunStatusCompleted {
		t.Fatalf("resumed run state = %#v events=%#v", run, resumed)
	}
	if toolInvoker.initialCalls.Load() != 1 || toolInvoker.resumeCalls.Load() != 1 {
		t.Fatalf("tool side effects were replayed: initial=%d resume=%d", toolInvoker.initialCalls.Load(), toolInvoker.resumeCalls.Load())
	}
	if toolInvoker.distinctToolCallIDs() != 1 {
		t.Fatalf("resume changed tool_call_id: %#v", toolInvoker.toolCallIDs)
	}
	if indexEvent(resumed, observability.EventResumeAccepted) < 0 || indexEvent(resumed, observability.EventToolCallCompleted) < 0 || indexEvent(resumed, observability.EventAgentTextDelta) < 0 || indexEvent(resumed, observability.EventRunCompleted) < 0 {
		t.Fatalf("resume lifecycle incomplete: %#v", resumed)
	}
}

func approvalToolResolver() agentruntime.EinoToolInfoResolver {
	return agentruntime.EinoToolInfoResolverFunc(func(context.Context, []string) ([]agentruntime.EinoResolvedTool, error) {
		return []agentruntime.EinoResolvedTool{{Ref: "approval@v1", Info: &schema.ToolInfo{
			Name: "approval", Desc: "request approval",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"action": {Type: schema.String, Required: true}}),
		}}}, nil
	})
}

func firstInterruptContextID(t *testing.T, events []observability.AgentEvent) string {
	t.Helper()
	for _, event := range events {
		if event.EventType != observability.EventControlRequestCreated {
			continue
		}
		var payload struct {
			InterruptContexts []struct {
				ID string `json:"id"`
			} `json:"interrupt_contexts"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode control request: %v", err)
		}
		if len(payload.InterruptContexts) > 0 && payload.InterruptContexts[0].ID != "" {
			return payload.InterruptContexts[0].ID
		}
	}
	t.Fatal("control request has no interrupt context id")
	return ""
}

func TestEinoRuntimeRejectsUngovernedCapabilities(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*agentruntime.ModelContextPackage)
	}{
		{name: "tool", configure: func(pkg *agentruntime.ModelContextPackage) { pkg.Capabilities.Tools = []string{"search@v1"} }},
		{name: "mcp", configure: func(pkg *agentruntime.ModelContextPackage) { pkg.Capabilities.MCPServers = []string{"maps"} }},
		{name: "skill", configure: func(pkg *agentruntime.ModelContextPackage) { pkg.Capabilities.Skills = []string{"route_plan"} }},
		{name: "sub_agent", configure: func(pkg *agentruntime.ModelContextPackage) { pkg.Capabilities.SubAgents = []string{"planner"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeReact)
			agent := &scriptedEinoAgent{}
			runtime := agentruntime.NewEinoRuntime(
				agentruntime.EinoManagedAgentFactoryFunc(func(context.Context, agentruntime.EinoManagedAgentBuildRequest) (adk.ResumableAgent, error) {
					return agent, nil
				}),
				agentruntime.RuntimeEnvironment{
					Models:      conformanceModelInvoker{},
					Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: newMemoryCheckpointStore()},
				},
				agentruntime.DefaultEinoControlRequestFactory{},
			)
			req := conformanceRequest(definition, "governance_"+tt.name)
			ctx := conformanceContextWith(req, tt.configure)
			if _, err := runtime.Run(ctx, req); !errors.Is(err, agentruntime.ErrRuntimeEnvironmentDependencyMissing) {
				t.Fatalf("expected managed dependency rejection, got %v", err)
			}
		})
	}
}

func TestEinoRuntimeBridgesGatewayEventsIntoCanonicalStream(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeReact)
	agent := &scriptedEinoAgent{
		run: func(ctx context.Context, _ *adk.AgentInput) *adk.AsyncIterator[*adk.AgentEvent] {
			emitter, ok := agentruntime.RuntimeEventEmitterFrom(ctx)
			if !ok {
				return einoIterator(&adk.AgentEvent{Err: agentruntime.ErrRuntimeEventBridgeClosed})
			}
			if err := emitter.Emit(ctx, observability.AgentEvent{
				EventType:  observability.EventToolCallStarted,
				Visibility: observability.VisibilityDebug,
				Payload:    mustJSON(map[string]string{"tool_call_id": "call_1", "tool_name": "search"}),
			}); err != nil {
				return einoIterator(&adk.AgentEvent{Err: err})
			}
			if err := emitter.Emit(ctx, observability.AgentEvent{
				EventType:  observability.EventToolCallCompleted,
				Visibility: observability.VisibilityDebug,
				Payload:    mustJSON(map[string]string{"tool_call_id": "call_1", "tool_name": "search"}),
			}); err != nil {
				return einoIterator(&adk.AgentEvent{Err: err})
			}
			return einoIterator(adk.EventFromMessage(schema.AssistantMessage("done", nil), nil, schema.Assistant, ""))
		},
	}
	runtime := newTestEinoRuntime(agent, newMemoryCheckpointStore(), agentruntime.DefaultEinoControlRequestFactory{})
	req := conformanceRequest(definition, "bridge")
	events, err := runtime.Run(conformanceContext(req), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collectEvents(events)
	toolStartIndex := indexEvent(collected, observability.EventToolCallStarted)
	toolCompletedIndex := indexEvent(collected, observability.EventToolCallCompleted)
	textIndex := indexEvent(collected, observability.EventAgentTextDelta)
	if toolStartIndex < 0 || toolCompletedIndex < 0 || textIndex < 0 || toolStartIndex > toolCompletedIndex || toolCompletedIndex > textIndex {
		t.Fatalf("gateway event was not bridged before agent output: %#v", collected)
	}
}

func TestEinoRuntimeMapsContextMessagesAndStreamsChunks(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeReact)
	var captured []*schema.Message
	agent := &scriptedEinoAgent{
		run: func(_ context.Context, input *adk.AgentInput) *adk.AsyncIterator[*adk.AgentEvent] {
			for _, message := range input.Messages {
				copy := *message
				captured = append(captured, &copy)
			}
			stream := schema.StreamReaderFromArray([]*schema.Message{
				{Role: schema.Assistant, Content: "hello "},
				{Role: schema.Assistant, Content: "world"},
			})
			event := adk.EventFromMessage(nil, stream, schema.Assistant, "")
			event.AgentName = "runtime_planner"
			return einoIterator(event)
		},
	}
	runtime := newTestEinoRuntime(agent, newMemoryCheckpointStore(), agentruntime.DefaultEinoControlRequestFactory{})
	req := conformanceRequest(definition, "stream")
	pkg := agentruntime.ModelContextPackage{
		SchemaVersion: agentruntime.ModelContextPackageSchemaVersion,
		PackageID:     "pkg_stream",
		Run:           agentruntime.ModelContextRun{SessionID: req.SessionID, RunID: req.RunID, AgentID: definition.AgentID, Runtime: string(agentruntime.RuntimeTypeEino)},
		Messages: agentruntime.ModelContextMessages{ConversationWindow: []agentruntime.ModelContextMessage{
			{ID: "system_1", Role: "system", Content: "be concise"},
			{ID: "user_1", Role: "user", Content: "greet me"},
		}},
	}
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	ctx := observability.WithTraceContext(context.Background(), req.Trace)
	ctx = agentruntime.WithModelContextPackage(ctx, pkg)
	events, err := runtime.Run(ctx, req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collectEvents(events)
	if len(captured) != 2 || captured[0].Role != schema.System || captured[0].Content != "be concise" || captured[1].Role != schema.User || captured[1].Content != "greet me" {
		t.Fatalf("model context messages were not mapped exactly: %#v", captured)
	}
	var deltas []string
	for _, event := range collected {
		if event.EventType != observability.EventAgentTextDelta {
			continue
		}
		var payload struct {
			Text             string `json:"text"`
			RuntimeAgentName string `json:"runtime_agent_name"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		if event.AgentID != "" || payload.RuntimeAgentName != "scripted_eino" {
			t.Fatalf("runtime-internal identity leaked into platform agent_id: event=%#v payload=%#v", event, payload)
		}
		deltas = append(deltas, payload.Text)
	}
	if len(deltas) != 2 || deltas[0] != "hello " || deltas[1] != "world" {
		t.Fatalf("stream chunks were not preserved: %#v", deltas)
	}
}

func TestEinoRuntimeRejectsInvalidContextMessageRole(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeReact)
	runtime := newTestEinoRuntime(&scriptedEinoAgent{}, newMemoryCheckpointStore(), agentruntime.DefaultEinoControlRequestFactory{})
	req := conformanceRequest(definition, "invalid_role")
	ctx := conformanceContextWith(req, func(pkg *agentruntime.ModelContextPackage) {
		pkg.Messages.ConversationWindow = []agentruntime.ModelContextMessage{{Role: "developer", Content: "must not pass through"}}
	})
	if _, err := runtime.Run(ctx, req); !errors.Is(err, agentruntime.ErrEinoMessageRoleInvalid) {
		t.Fatalf("expected invalid role rejection, got %v", err)
	}
}

func TestEinoRuntimeInterruptAndResumeThroughRuntimeService(t *testing.T) {
	definition := conformanceDefinition(agentruntime.RuntimeTypeEino, agentruntime.RuntimeModeReact)
	store := newMemoryCheckpointStore()
	agent := &scriptedEinoAgent{
		run: func(ctx context.Context, _ *adk.AgentInput) *adk.AsyncIterator[*adk.AgentEvent] {
			return einoIterator(adk.Interrupt(ctx, map[string]any{"question": "continue?"}))
		},
		resume: func(context.Context, *adk.ResumeInfo) *adk.AsyncIterator[*adk.AgentEvent] {
			return einoIterator(adk.EventFromMessage(schema.AssistantMessage("resumed", nil), nil, schema.Assistant, ""))
		},
	}
	const (
		controlID   = "control_eino_1"
		resumeToken = "resume_secret_1"
	)
	controls := agentruntime.EinoControlRequestFactoryFunc(func(_ context.Context, req agentruntime.EinoControlRequestFactoryRequest) (agentruntime.EinoControlRequest, error) {
		return agentruntime.EinoControlRequest{
			Binding: agentruntime.WaitingControlRequest{ControlRequestID: controlID, ResumeToken: resumeToken},
			Type:    "ask_user",
			Payload: mustJSON(map[string]any{
				"request_id": controlID, "type": "ask_user", "checkpoint_id": req.CheckpointID,
				"required": true, "resume_token": "must_not_leak",
				"interrupt_contexts": []map[string]any{{"id": "forged_target"}},
			}),
		}, nil
	})
	runtime := newTestEinoRuntime(agent, store, controls)
	state := agentruntime.NewInMemoryStateManager()
	service := agentruntime.NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.ControlRequests = stateControlCreator{state: state}
	req := conformanceRequest(definition, "interrupt")

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	first := collectEvents(events)
	run, ok := state.Run(req.RunID)
	if !ok || run.Status != agentruntime.RunStatusWaitingControl || run.PendingControlRequestID != controlID || run.CheckpointID == "" {
		t.Fatalf("run did not enter waiting_control: %#v", run)
	}
	if indexEvent(first, observability.EventControlRequestCreated) < 0 || indexEvent(first, observability.EventRunCompleted) >= 0 {
		t.Fatalf("invalid interrupt event sequence: %#v", first)
	}
	for _, event := range first {
		if event.EventType == observability.EventControlRequestCreated && string(event.Payload) != "" {
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("invalid control payload: %v", err)
			}
			if _, leaked := payload["resume_token"]; leaked {
				t.Fatalf("resume token leaked into canonical event: %s", event.Payload)
			}
			contexts, ok := payload["interrupt_contexts"].([]any)
			if !ok || len(contexts) != 1 {
				t.Fatalf("canonical interrupt contexts missing: %s", event.Payload)
			}
			interrupt, ok := contexts[0].(map[string]any)
			if !ok || interrupt["id"] == "" || interrupt["id"] == "forged_target" {
				t.Fatalf("canonical interrupt target is not engine-owned: %s", event.Payload)
			}
		}
	}
	if _, ok, err := store.Load(context.Background(), agentruntime.RuntimeCheckpointKey{
		Scope: agentruntime.RuntimeCheckpointScope{
			SessionID:      req.SessionID,
			RunID:          req.RunID,
			Runtime:        agentruntime.RuntimeTypeEino,
			RuntimeVersion: agentruntime.EinoRuntimeVersion,
			AdapterVersion: agentruntime.EinoAdapterVersion,
		},
		CheckpointID: run.CheckpointID,
	}); err != nil || !ok {
		t.Fatalf("eino checkpoint was not persisted: ok=%v err=%v", ok, err)
	}

	resumed, err := service.Resume(context.Background(), agentruntime.ResumeRequest{
		SessionID:        req.SessionID,
		RunID:            req.RunID,
		Definition:       definition,
		CheckpointID:     run.CheckpointID,
		ControlRequestID: controlID,
		ResumeToken:      resumeToken,
		Trace:            req.Trace,
	})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	second := collectEvents(resumed)
	run, _ = state.Run(req.RunID)
	if run.Status != agentruntime.RunStatusCompleted {
		t.Fatalf("resumed run not completed: %#v", run)
	}
	if indexEvent(second, observability.EventResumeAccepted) < 0 || indexEvent(second, observability.EventAgentTextDelta) < 0 || indexEvent(second, observability.EventRunCompleted) < 0 {
		t.Fatalf("invalid resume event sequence: %#v", second)
	}
}

func newTestEinoRuntime(agent adk.ResumableAgent, store agentruntime.RuntimeCheckpointStore, controls agentruntime.EinoControlRequestFactory) *agentruntime.EinoRuntime {
	return agentruntime.NewEinoRuntime(
		agentruntime.EinoManagedAgentFactoryFunc(func(_ context.Context, req agentruntime.EinoManagedAgentBuildRequest) (adk.ResumableAgent, error) {
			if req.Environment.Models == nil || req.Environment.Checkpoints == nil || req.ChatModel == nil || req.InternalAgents == nil {
				return nil, errors.New("managed environment was not injected")
			}
			return agent, nil
		}),
		agentruntime.RuntimeEnvironment{
			Models:      conformanceModelInvoker{},
			Tools:       conformanceToolInvoker{},
			SubAgents:   conformanceSubAgentInvoker{},
			Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: store},
		},
		controls,
	)
}

func conformanceDefinition(runtimeType agentruntime.RuntimeType, mode agentruntime.RuntimeMode) agentruntime.AgentDefinition {
	return agentruntime.AgentDefinition{
		AgentID:   "agent_conformance",
		AgentType: "test",
		Version:   "v1",
		Runtime:   agentruntime.RuntimeSpec{Type: runtimeType, Mode: mode},
	}
}

func conformanceRequest(definition agentruntime.AgentDefinition, suffix string) agentruntime.RunRequest {
	return agentruntime.RunRequest{
		SessionID:  "session_" + suffix,
		RunID:      "run_" + suffix,
		Definition: definition,
		Input:      []agentruntime.Message{{Role: "user", Content: "hello"}},
		Trace:      observability.TraceContext{TraceID: "trace_" + suffix},
	}
}

func conformanceContext(req agentruntime.RunRequest) context.Context {
	return conformanceContextWith(req, nil)
}

func conformanceContextWith(req agentruntime.RunRequest, configure func(*agentruntime.ModelContextPackage)) context.Context {
	pkg := agentruntime.ModelContextPackage{
		SchemaVersion: agentruntime.ModelContextPackageSchemaVersion,
		PackageID:     "pkg_" + req.RunID,
		CreatedAt:     time.Now(),
		Run: agentruntime.ModelContextRun{
			SessionID:   req.SessionID,
			RunID:       req.RunID,
			AgentID:     req.Definition.AgentID,
			Runtime:     string(req.Definition.Runtime.Type),
			RuntimeMode: string(req.Definition.Runtime.Mode),
		},
		Messages: agentruntime.ModelContextMessages{
			ConversationWindow: []agentruntime.ModelContextMessage{{ID: "msg_1", Role: "user", Content: "hello"}},
		},
		Capabilities: agentruntime.ModelContextCapabilities{
			Tools:     append([]string(nil), req.Definition.ToolRefs...),
			SubAgents: append([]string(nil), req.Definition.SubAgentRefs...),
		},
	}
	if len(req.Definition.ToolRefs) > 0 {
		pkg.Capabilities.ToolSnapshot = &agentruntime.ToolSchemaSnapshot{
			SnapshotID:     "tool_snapshot_" + req.RunID,
			ToolRefs:       append([]string(nil), req.Definition.ToolRefs...),
			CapabilityHash: "sha256:conformance-capability",
			PolicyHash:     "sha256:conformance-policy",
		}
		for _, ref := range req.Definition.ToolRefs {
			name, _, ok := strings.Cut(ref, "@")
			if !ok {
				name = ref
			}
			pkg.Capabilities.ToolDefinitions = append(pkg.Capabilities.ToolDefinitions, agentruntime.ModelToolDefinition{
				Name: name, Description: "frozen conformance tool",
				Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"action":{"type":"string"}}}`),
			})
		}
	}
	if configure != nil {
		configure(&pkg)
	}
	pkg.ContextHash = agentruntime.ComputeModelContextHash(pkg)
	ctx := observability.WithTraceContext(context.Background(), req.Trace)
	return agentruntime.WithModelContextPackage(ctx, pkg)
}

type conformanceModelInvoker struct{}

func (conformanceModelInvoker) Invoke(ctx context.Context, _ agentruntime.ModelInvokeRequest) (<-chan agentruntime.ModelStreamItem, error) {
	out := make(chan agentruntime.ModelStreamItem, 3)
	go func() {
		defer close(out)
		items := []agentruntime.ModelStreamItem{
			{Event: observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug}},
			{Event: observability.AgentEvent{EventType: observability.EventModelTokenDelta, Visibility: observability.VisibilityUserVisible, Payload: mustJSON(map[string]string{"text": "ok"})}, TextDelta: "ok"},
			{Event: observability.AgentEvent{EventType: observability.EventModelCallCompleted, Visibility: observability.VisibilityDebug}},
		}
		for _, item := range items {
			select {
			case <-ctx.Done():
				return
			case out <- item:
			}
		}
	}()
	return out, nil
}

type conformanceToolInvoker struct{}

func (conformanceToolInvoker) Invoke(context.Context, agentruntime.ToolInvocationRequest, agentruntime.ToolEventSink) (agentruntime.ToolInvocationResult, error) {
	return agentruntime.ToolInvocationResult{Content: "ok"}, nil
}

type conformanceSubAgentInvoker struct{}

func (conformanceSubAgentInvoker) Invoke(context.Context, agentruntime.SubAgentInvocationRequest, agentruntime.SubAgentEventSink) (agentruntime.SubAgentInvocationResult, error) {
	return agentruntime.SubAgentInvocationResult{ChildRunID: "child_run", Content: "ok"}, nil
}

type deepTaskModelInvoker struct {
	calls atomic.Int32
}

func (i *deepTaskModelInvoker) Invoke(ctx context.Context, _ agentruntime.ModelInvokeRequest) (<-chan agentruntime.ModelStreamItem, error) {
	round := i.calls.Add(1)
	out := make(chan agentruntime.ModelStreamItem, 3)
	go func() {
		defer close(out)
		items := []agentruntime.ModelStreamItem{{Event: observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug}}}
		if round == 1 {
			call := &agentruntime.ModelToolCall{ToolCallID: "task_call_1", Name: "task", Arguments: mustJSON(map[string]string{"subagent_type": "researcher", "description": "inspect runtime"})}
			items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelToolCallDelta, Visibility: observability.VisibilityDebug}, ToolCall: call})
		} else {
			items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelTokenDelta, Visibility: observability.VisibilityUserVisible}, TextDelta: "synthesized"})
		}
		items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelCallCompleted, Visibility: observability.VisibilityDebug}})
		for _, item := range items {
			select {
			case <-ctx.Done():
				return
			case out <- item:
			}
		}
	}()
	return out, nil
}

type deepTaskSubAgentInvoker struct {
	calls atomic.Int32
}

type deepToolModelInvoker struct {
	calls atomic.Int32
}

func (i *deepToolModelInvoker) Invoke(ctx context.Context, _ agentruntime.ModelInvokeRequest) (<-chan agentruntime.ModelStreamItem, error) {
	round := i.calls.Add(1)
	out := make(chan agentruntime.ModelStreamItem, 3)
	go func() {
		defer close(out)
		items := []agentruntime.ModelStreamItem{{Event: observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug}}}
		if round == 1 {
			call := &agentruntime.ModelToolCall{ToolCallID: "search_call_1", Name: "search", Version: "v2", Arguments: mustJSON(map[string]string{"query": "runtime"})}
			items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelToolCallDelta, Visibility: observability.VisibilityDebug}, ToolCall: call})
		} else {
			items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelTokenDelta, Visibility: observability.VisibilityUserVisible}, TextDelta: "tool synthesized"})
		}
		items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelCallCompleted, Visibility: observability.VisibilityDebug}})
		for _, item := range items {
			select {
			case <-ctx.Done():
				return
			case out <- item:
			}
		}
	}()
	return out, nil
}

type deepToolInvoker struct {
	calls atomic.Int32
}

type deepInterruptModelInvoker struct{}

func (*deepInterruptModelInvoker) Invoke(ctx context.Context, req agentruntime.ModelInvokeRequest) (<-chan agentruntime.ModelStreamItem, error) {
	hasToolResult := false
	for _, message := range req.Messages {
		if message.Role == string(schema.Tool) {
			hasToolResult = true
			break
		}
	}
	out := make(chan agentruntime.ModelStreamItem, 3)
	go func() {
		defer close(out)
		items := []agentruntime.ModelStreamItem{{Event: observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug}}}
		if !hasToolResult {
			call := &agentruntime.ModelToolCall{ToolCallID: "approval_call_1", Name: "approval", Version: "v1", Arguments: mustJSON(map[string]string{"action": "publish"})}
			items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelToolCallDelta, Visibility: observability.VisibilityDebug}, ToolCall: call})
		} else {
			items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelTokenDelta, Visibility: observability.VisibilityUserVisible}, TextDelta: "approved"})
		}
		items = append(items, agentruntime.ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelCallCompleted, Visibility: observability.VisibilityDebug}})
		for _, item := range items {
			select {
			case <-ctx.Done():
				return
			case out <- item:
			}
		}
	}()
	return out, nil
}

type deepInterruptToolInvoker struct {
	initialCalls atomic.Int32
	resumeCalls  atomic.Int32
	mu           sync.Mutex
	toolCallIDs  []string
}

func (i *deepInterruptToolInvoker) Invoke(ctx context.Context, req agentruntime.ToolInvocationRequest, sink agentruntime.ToolEventSink) (agentruntime.ToolInvocationResult, error) {
	i.mu.Lock()
	i.toolCallIDs = append(i.toolCallIDs, req.ToolCallID)
	i.mu.Unlock()
	if req.Resume == nil {
		i.initialCalls.Add(1)
		if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityDebug}); err != nil {
			return agentruntime.ToolInvocationResult{}, err
		}
		return agentruntime.ToolInvocationResult{}, &agentruntime.ToolInvocationInterruptedError{
			Info: map[string]any{
				"type": "ask_user",
				"questions": []map[string]any{{
					"header": "scope", "question": "approve?",
					"options": []map[string]any{{"label": "yes", "description": "continue"}, {"label": "no", "description": "stop"}},
				}},
			},
			State: mustJSON(map[string]int{"attempt": 1}),
		}
	}
	i.resumeCalls.Add(1)
	if !req.Resume.WasInterrupted || !req.Resume.IsResumeTarget || string(req.Resume.State) != `{"attempt":1}` {
		return agentruntime.ToolInvocationResult{}, errors.New("invalid tool resume state")
	}
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallCompleted, Visibility: observability.VisibilityDebug}); err != nil {
		return agentruntime.ToolInvocationResult{}, err
	}
	return agentruntime.ToolInvocationResult{Content: "approved"}, nil
}

func (i *deepInterruptToolInvoker) distinctToolCallIDs() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	seen := make(map[string]struct{}, len(i.toolCallIDs))
	for _, id := range i.toolCallIDs {
		seen[id] = struct{}{}
	}
	return len(seen)
}

type capturingEinoErrors struct {
	mu     sync.Mutex
	errors []error
}

func (c *capturingEinoErrors) add(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.errors = append(c.errors, err)
	c.mu.Unlock()
}

func (c *capturingEinoErrors) snapshot() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.errors...)
}

type capturingEinoExecutionFactory struct {
	errors *capturingEinoErrors
}

func (f capturingEinoExecutionFactory) New(ctx context.Context, agent adk.ResumableAgent, checkpoints adk.CheckPointStore) (agentruntime.EinoExecution, error) {
	execution, err := (agentruntime.ADKEinoExecutionFactory{}).New(ctx, agent, checkpoints)
	if err != nil {
		return nil, err
	}
	return &capturingEinoExecution{EinoExecution: execution, errors: f.errors}, nil
}

type capturingEinoExecution struct {
	agentruntime.EinoExecution
	errors *capturingEinoErrors
}

func (e *capturingEinoExecution) Run(ctx context.Context, messages []*schema.Message, checkpointID string) (agentruntime.EinoEventIterator, adk.AgentCancelFunc, error) {
	iterator, cancel, err := e.EinoExecution.Run(ctx, messages, checkpointID)
	return &capturingEinoIterator{EinoEventIterator: iterator, errors: e.errors}, cancel, err
}

func (e *capturingEinoExecution) Resume(ctx context.Context, checkpointID string, params *adk.ResumeParams) (agentruntime.EinoEventIterator, adk.AgentCancelFunc, error) {
	iterator, cancel, err := e.EinoExecution.Resume(ctx, checkpointID, params)
	return &capturingEinoIterator{EinoEventIterator: iterator, errors: e.errors}, cancel, err
}

type capturingEinoIterator struct {
	agentruntime.EinoEventIterator
	errors *capturingEinoErrors
}

func (i *capturingEinoIterator) Next() (*adk.AgentEvent, bool) {
	event, ok := i.EinoEventIterator.Next()
	if ok && event != nil {
		i.errors.add(event.Err)
	}
	return event, ok
}

func (i *deepToolInvoker) Invoke(ctx context.Context, req agentruntime.ToolInvocationRequest, sink agentruntime.ToolEventSink) (agentruntime.ToolInvocationResult, error) {
	i.calls.Add(1)
	if req.ToolName != "search" || req.ToolVersion != "v2" || req.ToolCallID != "search_call_1" {
		return agentruntime.ToolInvocationResult{}, errors.New("invalid tool request")
	}
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityDebug}); err != nil {
		return agentruntime.ToolInvocationResult{}, err
	}
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallCompleted, Visibility: observability.VisibilityDebug}); err != nil {
		return agentruntime.ToolInvocationResult{}, err
	}
	return agentruntime.ToolInvocationResult{Content: "search result"}, nil
}

type failingStreamingToolGateway struct {
	err error
}

func (g failingStreamingToolGateway) Invoke(context.Context, toolgateway.ToolCallRequest) (*toolgateway.ToolCallResult, error) {
	return nil, g.err
}

func (g failingStreamingToolGateway) InvokeWithEvents(context.Context, toolgateway.ToolCallRequest, toolgateway.PersistedEventSink) (*toolgateway.ToolCallResult, error) {
	return nil, g.err
}

func (i *deepTaskSubAgentInvoker) Invoke(ctx context.Context, req agentruntime.SubAgentInvocationRequest, sink agentruntime.SubAgentEventSink) (agentruntime.SubAgentInvocationResult, error) {
	i.calls.Add(1)
	if req.SubAgentRef != "researcher" || req.Scope != agentruntime.SubAgentScopePlatformChildRun || req.Description != "inspect runtime" {
		return agentruntime.SubAgentInvocationResult{}, errors.New("invalid sub-agent request")
	}
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventSubAgentStarted, Visibility: observability.VisibilityDebug}); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventSubAgentCompleted, Visibility: observability.VisibilityDebug}); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	return agentruntime.SubAgentInvocationResult{ChildRunID: "child_run_1", Content: "research result"}, nil
}

type completionGateState struct {
	*agentruntime.InMemoryStateManager
	entered chan struct{}
	release chan struct{}
	fail    error
	once    sync.Once
}

func newCompletionGateState(fail error) *completionGateState {
	return &completionGateState{
		InMemoryStateManager: agentruntime.NewInMemoryStateManager(),
		entered:              make(chan struct{}),
		release:              make(chan struct{}),
		fail:                 fail,
	}
}

func (s *completionGateState) AppendEvent(ctx context.Context, event observability.AgentEvent) (*agentruntime.EventAppendResult, error) {
	if event.EventType == observability.EventModelCallCompleted {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.release:
		}
		if s.fail != nil {
			return nil, s.fail
		}
	}
	return s.InMemoryStateManager.AppendEvent(ctx, event)
}

func (s *completionGateState) waitUntilCompletionAppend(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("RuntimeService did not reach model_call_completed append")
	}
}

func newSequenceBarrierEinoRuntime() (*agentruntime.EinoRuntime, *deepToolInvoker) {
	tools := &deepToolInvoker{}
	runtime := agentruntime.NewEinoRuntime(
		agentruntime.NewEinoDeepAgentFactory(),
		agentruntime.RuntimeEnvironment{
			Models:      &deepToolModelInvoker{},
			Tools:       tools,
			Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: newMemoryCheckpointStore()},
		},
		agentruntime.DefaultEinoControlRequestFactory{},
	)
	runtime.ToolInfos = agentruntime.EinoToolInfoResolverFunc(func(_ context.Context, refs []string) ([]agentruntime.EinoResolvedTool, error) {
		if len(refs) != 1 || refs[0] != "search@v2" {
			return nil, errors.New("unexpected tool refs")
		}
		return []agentruntime.EinoResolvedTool{{Ref: "search@v2", Info: &schema.ToolInfo{
			Name: "search",
			Desc: "search documents",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"query": {Type: schema.String, Required: true},
			}),
		}}}, nil
	})
	return runtime, tools
}

func assertToolNotCalled(t *testing.T, tools *deepToolInvoker) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	if got := tools.calls.Load(); got != 0 {
		t.Fatalf("tool executed before durable model completion: calls=%d", got)
	}
}

func waitForCollectedEvents(t *testing.T, collected <-chan []observability.AgentEvent) []observability.AgentEvent {
	t.Helper()
	select {
	case events := <-collected:
		return events
	case <-time.After(5 * time.Second):
		t.Fatal("RuntimeService event stream did not close; commit waiter may be leaked")
		return nil
	}
}

func assertLiveStoreExactParity(t *testing.T, live, stored []observability.AgentEvent) {
	t.Helper()
	durableLive := make([]observability.AgentEvent, 0, len(live))
	for _, event := range live {
		if !observability.IsEphemeralDelta(event.EventType) {
			durableLive = append(durableLive, event)
		}
	}
	if len(durableLive) != len(stored) {
		t.Fatalf("live/store event count differs: live=%d stored=%d\nlive=%#v\nstored=%#v", len(durableLive), len(stored), durableLive, stored)
	}
	for i := range stored {
		got, want := durableLive[i], stored[i]
		if got.EventID != want.EventID || got.EventType != want.EventType || got.Sequence != want.Sequence {
			t.Fatalf("live/store mismatch at %d: live=(%s,%s,%d) stored=(%s,%s,%d)", i, got.EventID, got.EventType, got.Sequence, want.EventID, want.EventType, want.Sequence)
		}
	}
}

func firstEvent(events []observability.AgentEvent, eventType observability.EventType) *observability.AgentEvent {
	for i := range events {
		if events[i].EventType == eventType {
			return &events[i]
		}
	}
	return nil
}

func assertNoToolEvents(t *testing.T, events []observability.AgentEvent) {
	t.Helper()
	for _, event := range events {
		if event.EventType == observability.EventToolCallStarted || event.EventType == observability.EventToolCallCompleted || event.EventType == observability.EventToolCallFailed {
			t.Fatalf("tool lifecycle event persisted after blocked completion: %#v", event)
		}
	}
}

type scriptedEinoAgent struct {
	run    func(context.Context, *adk.AgentInput) *adk.AsyncIterator[*adk.AgentEvent]
	resume func(context.Context, *adk.ResumeInfo) *adk.AsyncIterator[*adk.AgentEvent]
}

func (*scriptedEinoAgent) Name(context.Context) string        { return "scripted_eino" }
func (*scriptedEinoAgent) Description(context.Context) string { return "runtime adapter test agent" }

func (a *scriptedEinoAgent) Run(ctx context.Context, input *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	if a.run != nil {
		return a.run(ctx, input)
	}
	return einoIterator()
}

func (a *scriptedEinoAgent) Resume(ctx context.Context, info *adk.ResumeInfo, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	if a.resume != nil {
		return a.resume(ctx, info)
	}
	return einoIterator()
}

func einoIterator(events ...*adk.AgentEvent) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	for _, event := range events {
		generator.Send(event)
	}
	generator.Close()
	return iterator
}

func newMemoryCheckpointStore() *agentruntime.InMemoryRuntimeCheckpointStore {
	return agentruntime.NewInMemoryRuntimeCheckpointStore()
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func collectEvents(events <-chan observability.AgentEvent) []observability.AgentEvent {
	var out []observability.AgentEvent
	for event := range events {
		out = append(out, event)
	}
	return out
}

type stateControlCreator struct {
	state agentruntime.RuntimeStateManager
}

func (c stateControlCreator) CreateControlRequest(ctx context.Context, req agentruntime.ControlRequestCreateRequest) (observability.AgentEvent, error) {
	err := c.state.EnterWaitingControl(ctx, agentruntime.WaitingControlRequest{
		SessionID:        req.SessionID,
		RunID:            req.RunID,
		CheckpointID:     req.CheckpointID,
		ControlRequestID: req.RequestID,
		ResumeToken:      req.ResumeToken,
		Type:             req.Type,
		Event:            req.Event,
	})
	return req.Event, err
}

func indexEvent(events []observability.AgentEvent, eventType observability.EventType) int {
	for i, event := range events {
		if event.EventType == eventType {
			return i
		}
	}
	return -1
}
