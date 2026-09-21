package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

func TestNativeDirectRuntimeRunsOneShotModelStream(t *testing.T) {
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
		modelEvent(observability.EventModelCallStarted),
		modelTextEvent("你好"),
		modelEvent(observability.EventModelCallCompleted),
	}}}
	runtime := NewNativeDirectRuntime(models, nil, nil)
	pkg := directTestPackage("run_1", "pkg_1")
	ctx := WithModelContextPackage(context.Background(), pkg)

	events, err := runtime.Run(ctx, directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	assertEventTypes(t, collected,
		observability.EventModelContextBuilt,
		observability.EventModelCallStarted,
		observability.EventModelTokenDelta,
		EventAgentTextDelta,
		observability.EventModelCallCompleted,
	)
	if got := finalTextFromPayload(collected[3].Payload); got != "你好" {
		t.Fatalf("agent text delta = %q", got)
	}
	requests := models.Requests()
	if len(requests) != 1 || requests[0].Package.PackageID != "pkg_1" || requests[0].Round != 1 || requests[0].AllowTools {
		t.Fatalf("unexpected model request: %#v", requests)
	}
}

func TestNativeDirectRuntimeBlocksOversizedFinalInputBeforeInvoker(t *testing.T) {
	models := &scriptedModelInvoker{}
	runtime := NewNativeDirectRuntime(models, nil, nil)
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 1
	pkg.Messages.ConversationWindow = []ModelContextMessage{
		{Role: "system", Content: "required instruction"},
		{Role: "user", Content: "latest input"},
	}
	pkg = sealDirectTestPackage(pkg)
	events, err := runtime.Run(WithModelContextPackage(context.Background(), pkg), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, observability.EventGuardrailBlocked) < 0 {
		t.Fatalf("missing model input guardrail event: %#v", collected)
	}
	failed := eventByType(t, collected, EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "MODEL_INPUT_BUDGET_EXCEEDED" {
		t.Fatalf("unexpected failure: %#v", failed)
	}
	if len(models.Requests()) != 0 {
		t.Fatal("oversized request reached model invoker")
	}
}

func TestNativeDirectRuntimeDoesNotLeakChildGovernanceIntoInheritedEmitter(t *testing.T) {
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
		modelEvent(observability.EventModelCallStarted),
		modelTextEvent("child result"),
		modelEvent(observability.EventModelCallCompleted),
	}}}
	runtime := NewNativeDirectRuntime(models, nil, nil)
	pkg := directTestPackage("child_run", "child_pkg")
	parentEmitter := &capturingRuntimeEmitter{}
	ctx := WithRuntimeEventEmitter(WithModelContextPackage(context.Background(), pkg), parentEmitter)

	events, err := runtime.Run(ctx, directTestRunRequest("child_run"))
	if err != nil {
		t.Fatal(err)
	}
	collected := collect(events)
	if eventIndex(collected, observability.EventModelContextBuilt) < 0 {
		t.Fatalf("child governance event did not stay on child stream: %#v", collected)
	}
	if len(parentEmitter.events) != 0 {
		t.Fatalf("child governance leaked into inherited parent emitter: %#v", parentEmitter.events)
	}
}

func TestNativeDirectRuntimeExecutesAtMostOneToolRound(t *testing.T) {
	// 模型只返回 schema 中的工具名，版本必须从冻结的能力引用恢复。
	call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{"query":"map"}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{
		{
			modelEvent(observability.EventModelCallStarted),
			modelToolEvent(call),
			modelEvent(observability.EventModelCallCompleted),
		},
		{
			modelEvent(observability.EventModelCallStarted),
			modelTextEvent("result"),
			modelEvent(observability.EventModelCallCompleted),
		},
	}}
	tools := &capturingToolInvoker{result: ToolInvocationResult{Content: "tool result"}, events: toolLifecycleEvents()}
	rebuilder := &capturingModelContextRebuilder{pkg: directTestPackage("run_1", "pkg_2")}
	runtime := NewNativeDirectRuntime(models, tools, rebuilder)
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search@v1"}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, observability.EventToolCallStarted) < 0 || eventIndex(collected, observability.EventToolCallCompleted) < 0 {
		t.Fatalf("tool gateway events were not forwarded: %#v", collected)
	}
	if eventIndex(collected, EventAgentTextDelta) < eventIndex(collected, observability.EventToolCallCompleted) {
		t.Fatalf("final model response must follow tool completion: %#v", collected)
	}
	requests := models.Requests()
	if len(requests) != 2 || requests[0].Round != 1 || !requests[0].AllowTools || requests[1].Round != 2 || requests[1].AllowTools || requests[1].Package.PackageID != "pkg_2" {
		t.Fatalf("unexpected model rounds: %#v", requests)
	}
	toolRequests := tools.Requests()
	if len(toolRequests) != 1 || toolRequests[0].ToolCallID != "tc_1" || toolRequests[0].ToolName != "search" ||
		toolRequests[0].ToolVersion != "v1" || toolRequests[0].SourceRef != "search@v1" {
		t.Fatalf("unexpected tool request: %#v", toolRequests)
	}
	rebuilds := rebuilder.Requests()
	if len(rebuilds) != 1 || rebuilds[0].InitialPackage.PackageID != "pkg_1" || rebuilds[0].Result.Content != "tool result" {
		t.Fatalf("unexpected context rebuild: %#v", rebuilds)
	}
}

func TestNativeDirectRuntimeRunsPortablePreModelCompactorEveryRound(t *testing.T) {
	call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{"query":"map"}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{
		{modelEvent(observability.EventModelCallStarted), modelToolEvent(call), modelEvent(observability.EventModelCallCompleted)},
		{modelEvent(observability.EventModelCallStarted), modelTextEvent("result"), modelEvent(observability.EventModelCallCompleted)},
	}}
	tools := &capturingToolInvoker{result: ToolInvocationResult{Content: "tool result"}, events: toolLifecycleEvents()}
	runtime := NewNativeDirectRuntime(models, tools, &capturingModelContextRebuilder{pkg: directTestPackage("run_1", "pkg_2")})
	var mu sync.Mutex
	var rounds []int
	runtime.PreModel = RuntimePreModelCompactorFunc(func(_ context.Context, req RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error) {
		mu.Lock()
		rounds = append(rounds, req.Request.Round)
		mu.Unlock()
		return RuntimePreModelCompactionResult{Request: req.Request}, nil
	})
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search@v1"}
	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)
	mu.Lock()
	defer mu.Unlock()
	if len(rounds) != 2 || rounds[0] != 1 || rounds[1] != 2 {
		t.Fatalf("pre-model compactor rounds=%v", rounds)
	}
	for _, request := range models.Requests() {
		if request.PreserveManifest.ManifestHash == "" {
			t.Fatalf("final request missing preserve manifest: %#v", request)
		}
	}
}

func TestNativeDirectRuntimeRoutesMCPToolThroughGateway(t *testing.T) {
	call := ModelToolCall{ToolCallID: "tc_mcp", Name: "remote_search", Arguments: json.RawMessage(`{"query":"map"}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{
		{modelEvent(observability.EventModelCallStarted), modelToolEvent(call), modelEvent(observability.EventModelCallCompleted)},
		{modelEvent(observability.EventModelCallStarted), modelTextEvent("result"), modelEvent(observability.EventModelCallCompleted)},
	}}
	tools := &capturingToolInvoker{result: ToolInvocationResult{Content: "tool result"}, events: toolLifecycleEvents()}
	runtime := NewNativeDirectRuntime(models, tools, &capturingModelContextRebuilder{pkg: directTestPackage("run_1", "pkg_2")})
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.MCPSnapshots = []mcp.CapabilitySnapshot{{
		ID: "mcp_snapshot_1", ServerID: "search_server", Tools: []mcp.Tool{{Name: "remote_search"}},
	}}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)
	requests := tools.Requests()
	if len(requests) != 1 || requests[0].Source != ToolSourceMCP || requests[0].SourceRef != "search_server" || requests[0].SnapshotRef != "mcp_snapshot_1" {
		t.Fatalf("MCP route not preserved: %#v", requests)
	}
}

func TestNativeDirectRuntimeRejectsAmbiguousToolRoute(t *testing.T) {
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search@v1"}
	pkg.Capabilities.MCPSnapshots = []mcp.CapabilitySnapshot{{ID: "snapshot_1", ServerID: "server_1", Tools: []mcp.Tool{{Name: "search"}}}}
	_, err := directToolRoute(pkg, ModelToolCall{Name: "search"})
	if !errors.Is(err, ErrDirectToolRouteAmbiguous) {
		t.Fatalf("expected ambiguous route rejection, got %v", err)
	}
}

func TestNativeDirectRuntimeRejectsUnauthorizedTool(t *testing.T) {
	call := ModelToolCall{ToolCallID: "tc_1", Name: "write_data", Arguments: json.RawMessage(`{}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
		modelEvent(observability.EventModelCallStarted),
		modelToolEvent(call),
		modelEvent(observability.EventModelCallCompleted),
	}}}
	tools := &capturingToolInvoker{}
	runtime := NewNativeDirectRuntime(models, tools, &capturingModelContextRebuilder{})
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search"}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	failed := eventByType(t, collect(events), EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "DIRECT_TOOL_NOT_AUTHORIZED" || failed.Error.Type != observability.EventErrorPermissionDenied {
		t.Fatalf("unauthorized tool error changed: %#v", failed)
	}
	if len(tools.Requests()) != 0 {
		t.Fatal("unauthorized tool must not reach Tool Gateway")
	}
}

func TestNativeDirectRuntimeFailsWhenSecondRoundRequestsTool(t *testing.T) {
	first := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{}`)}
	second := ModelToolCall{ToolCallID: "tc_2", Name: "search", Arguments: json.RawMessage(`{}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{
		{modelEvent(observability.EventModelCallStarted), modelToolEvent(first), modelEvent(observability.EventModelCallCompleted)},
		{modelEvent(observability.EventModelCallStarted), modelToolEvent(second), modelEvent(observability.EventModelCallCompleted)},
	}}
	tools := &capturingToolInvoker{result: ToolInvocationResult{Content: "ok"}, events: toolLifecycleEvents()}
	rebuilder := &capturingModelContextRebuilder{pkg: directTestPackage("run_1", "pkg_2")}
	runtime := NewNativeDirectRuntime(models, tools, rebuilder)
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search"}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	failed := eventByType(t, collect(events), EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "DIRECT_RUNTIME_BOUNDARY_EXCEEDED" || !failed.Error.Retryable {
		t.Fatalf("boundary failure changed: %#v", failed)
	}
	if len(tools.Requests()) != 1 {
		t.Fatalf("second tool call must not execute: %#v", tools.Requests())
	}
}

func TestNativeDirectRuntimeFailsWhenContextRebuildFails(t *testing.T) {
	call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
		modelEvent(observability.EventModelCallStarted),
		modelToolEvent(call),
		modelEvent(observability.EventModelCallCompleted),
	}}}
	tools := &capturingToolInvoker{result: ToolInvocationResult{Content: "ok"}, events: toolLifecycleEvents()}
	rebuilder := &capturingModelContextRebuilder{err: errors.New("context unavailable")}
	runtime := NewNativeDirectRuntime(models, tools, rebuilder)
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search"}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	failed := eventByType(t, collect(events), EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "DIRECT_CONTEXT_REBUILD_FAILED" {
		t.Fatalf("context rebuild failure changed: %#v", failed)
	}
	if len(models.Requests()) != 1 {
		t.Fatalf("second model call must not start: %#v", models.Requests())
	}
}

func TestNativeDirectRuntimeRejectsStaleRebuiltPackage(t *testing.T) {
	call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
		modelEvent(observability.EventModelCallStarted),
		modelToolEvent(call),
		modelEvent(observability.EventModelCallCompleted),
	}}}
	tools := &capturingToolInvoker{result: ToolInvocationResult{Content: "ok"}, events: toolLifecycleEvents()}
	rebuilder := &capturingModelContextRebuilder{pkg: directTestPackage("run_1", "pkg_1")}
	runtime := NewNativeDirectRuntime(models, tools, rebuilder)
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search"}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	failed := eventByType(t, collect(events), EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "DIRECT_CONTEXT_PACKAGE_STALE" {
		t.Fatalf("stale context package failure changed: %#v", failed)
	}
}

func TestNativeDirectRuntimeFeedsStructuredToolFailureBackToModel(t *testing.T) {
	call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{
		{modelEvent(observability.EventModelCallStarted), modelToolEvent(call), modelEvent(observability.EventModelCallCompleted)},
		{modelEvent(observability.EventModelCallStarted), modelTextEvent("limited answer"), modelEvent(observability.EventModelCallCompleted)},
	}}
	tools := &capturingToolInvoker{result: ToolInvocationResult{
		Content: "search temporarily unavailable",
		IsError: true,
	}, events: []observability.AgentEvent{
		{EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityDebug},
		{EventType: observability.EventToolCallFailed, Visibility: observability.VisibilityDebug},
	}}
	rebuilder := &capturingModelContextRebuilder{pkg: directTestPackage("run_1", "pkg_2")}
	runtime := NewNativeDirectRuntime(models, tools, rebuilder)
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search"}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, observability.EventToolCallFailed) < 0 || eventIndex(collected, EventAgentTextDelta) < 0 || eventIndex(collected, EventRunFailed) >= 0 {
		t.Fatalf("structured tool failure should produce a controlled answer: %#v", collected)
	}
	rebuilds := rebuilder.Requests()
	if len(rebuilds) != 1 || !rebuilds[0].Result.IsError {
		t.Fatalf("structured tool failure was not passed to context rebuild: %#v", rebuilds)
	}
}

func TestNativeDirectRuntimeStreamsToolProgressBeforeToolReturns(t *testing.T) {
	call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{
		{modelEvent(observability.EventModelCallStarted), modelToolEvent(call), modelEvent(observability.EventModelCallCompleted)},
		{modelEvent(observability.EventModelCallStarted), modelTextEvent("done"), modelEvent(observability.EventModelCallCompleted)},
	}}
	tools := &streamingToolInvoker{release: make(chan struct{})}
	rebuilder := &capturingModelContextRebuilder{pkg: directTestPackage("run_1", "pkg_2")}
	runtime := NewNativeDirectRuntime(models, tools, rebuilder)
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search"}

	events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	var collected []observability.AgentEvent
	deadline := time.After(time.Second)
	for eventIndex(collected, observability.EventToolCallProgress) < 0 {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("runtime closed before tool progress: %#v", collected)
			}
			collected = append(collected, event)
		case <-deadline:
			t.Fatal("tool progress was buffered until tool completion")
		}
	}
	close(tools.release)
	collected = append(collected, collect(events)...)
	if eventIndex(collected, observability.EventToolCallProgress) > eventIndex(collected, observability.EventToolCallCompleted) || eventIndex(collected, EventAgentTextDelta) < 0 {
		t.Fatalf("streaming tool lifecycle changed: %#v", collected)
	}
}

func TestNativeDirectRuntimeRequiresModelContextPackage(t *testing.T) {
	runtime := NewNativeDirectRuntime(&scriptedModelInvoker{}, nil, nil)
	if _, err := runtime.Run(context.Background(), directTestRunRequest("run_1")); !errors.Is(err, ErrModelContextPackageMissing) {
		t.Fatalf("expected missing package error, got %v", err)
	}
}

func TestNativeDirectRuntimeValidatesSnapshotDependenciesBeforeModelExecution(t *testing.T) {
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"dynamic_tool"}

	t.Run("tool invoker", func(t *testing.T) {
		models := &scriptedModelInvoker{}
		runtime := NewNativeDirectRuntime(models, nil, &capturingModelContextRebuilder{})
		if _, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1")); !errors.Is(err, ErrDirectToolInvokerMissing) {
			t.Fatalf("expected missing tool invoker, got %v", err)
		}
		if len(models.Requests()) != 0 {
			t.Fatal("model must not run before snapshot dependencies are validated")
		}
	})

	t.Run("context rebuilder", func(t *testing.T) {
		models := &scriptedModelInvoker{}
		tools := &capturingToolInvoker{}
		runtime := NewNativeDirectRuntime(models, tools, nil)
		if _, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1")); !errors.Is(err, ErrDirectContextRebuildMissing) {
			t.Fatalf("expected missing context rebuilder, got %v", err)
		}
		if len(models.Requests()) != 0 || len(tools.Requests()) != 0 {
			t.Fatal("model and tool must not run before snapshot dependencies are validated")
		}
	})
}

func TestNativeDirectRuntimeRejectsAuthorizedToolWithoutModelSchema(t *testing.T) {
	runtime := NewNativeDirectRuntime(&scriptedModelInvoker{}, &capturingToolInvoker{}, &capturingModelContextRebuilder{})
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Capabilities.Tools = []string{"search@v1"}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	_, err := runtime.Run(WithModelContextPackage(context.Background(), pkg), directTestRunRequest("run_1"))
	if !errors.Is(err, ErrDirectToolSchemaMissing) {
		t.Fatalf("missing model-visible schema was accepted: %v", err)
	}
}

func TestNativeDirectRuntimeCancellationPropagatesToModelInvoker(t *testing.T) {
	invoker := &blockingModelInvoker{started: make(chan struct{}), cancelled: make(chan struct{})}
	runtime := NewNativeDirectRuntime(invoker, nil, nil)
	ctx, cancel := context.WithCancel(WithModelContextPackage(context.Background(), directTestPackage("run_1", "pkg_1")))
	events, err := runtime.Run(ctx, directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	select {
	case <-invoker.started:
	case <-time.After(time.Second):
		t.Fatal("model invoker did not start")
	}
	cancel()
	_ = collect(events)
	select {
	case <-invoker.cancelled:
	case <-time.After(time.Second):
		t.Fatal("model invoker did not observe cancellation")
	}
}

func TestNativeDirectRuntimePropagatesPreStartModelFailure(t *testing.T) {
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
		{Event: observability.AgentEvent{
			EventType:  observability.EventModelCallFailed,
			Visibility: observability.VisibilityDebug,
			Error:      &observability.EventError{Code: "MODEL_UNAVAILABLE", Type: observability.EventErrorType("model_unavailable"), Message: "requested model unavailable"},
		}},
	}}}
	runtime := NewNativeDirectRuntime(models, nil, nil)
	events, err := runtime.Run(WithModelContextPackage(context.Background(), directTestPackage("run_model_unavailable", "pkg_model_unavailable")), directTestRunRequest("run_model_unavailable"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	collected := collect(events)
	failed := eventByType(t, collected, EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "MODEL_UNAVAILABLE" {
		t.Fatalf("pre-start model failure was not propagated: %#v", collected)
	}
	if eventIndex(collected, observability.EventModelCallFailed) < 0 {
		t.Fatalf("model_call_failed event missing: %#v", collected)
	}
}

func TestNativeDirectRuntimeCancelsModelRoundAfterValidationFailure(t *testing.T) {
	invoker := &invalidCancellableModelInvoker{cancelled: make(chan struct{})}
	runtime := NewNativeDirectRuntime(invoker, nil, nil)
	events, err := runtime.Run(WithModelContextPackage(context.Background(), directTestPackage("run_1", "pkg_1")), directTestRunRequest("run_1"))
	if err != nil {
		t.Fatalf("run failed before stream: %v", err)
	}
	failed := eventByType(t, collect(events), EventRunFailed)
	if failed.Error == nil || failed.Error.Code != "DIRECT_MODEL_EVENT_INVALID" {
		t.Fatalf("validation failure changed: %#v", failed)
	}
	select {
	case <-invoker.cancelled:
	case <-time.After(time.Second):
		t.Fatal("invalid model stream was not cancelled")
	}
}

func TestNativeDirectRuntimeConcurrentRunsKeepPackagesIsolated(t *testing.T) {
	invoker := &packageEchoModelInvoker{}
	runtime := NewNativeDirectRuntime(invoker, nil, nil)
	const runs = 16
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("run_%d", i)
			pkg := directTestPackage(runID, "pkg_"+runID)
			events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest(runID))
			if err != nil {
				errs <- err
				return
			}
			collected := collect(events)
			text := eventByTypeConcurrent(collected, EventAgentTextDelta)
			if text == nil || finalTextFromPayload(text.Payload) != runID {
				errs <- fmt.Errorf("run package crossed: run=%s events=%#v", runID, collected)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeDirectRuntimeValidatesModeAndDependencies(t *testing.T) {
	def := directTestRunRequest("run_1").Definition
	def.Runtime.Mode = RuntimeModeReact
	runtime := NewNativeDirectRuntime(&scriptedModelInvoker{}, nil, nil)
	if err := runtime.ValidateConfig(context.Background(), def); !errors.Is(err, ErrDirectModeUnsupported) {
		t.Fatalf("expected unsupported mode, got %v", err)
	}

	def.Runtime.Mode = RuntimeModeDirect
	def.ToolRefs = []string{"search"}
	if err := runtime.ValidateConfig(context.Background(), def); !errors.Is(err, ErrDirectToolInvokerMissing) {
		t.Fatalf("expected missing tool invoker, got %v", err)
	}
}

func TestNativeDirectRuntimeInstallsRequiredSemanticCompactorByDefault(t *testing.T) {
	runtime := NewNativeDirectRuntime(&scriptedModelInvoker{}, nil, nil)
	def := directTestRunRequest("run_required_compaction").Definition
	policy := DefaultContextCompactionPolicy()
	policy.PolicyHash = ""
	policy.SemanticSummary = SemanticSummaryRequired
	def.ContextCompaction = policy
	if err := runtime.ValidateConfig(context.Background(), def); err != nil {
		t.Fatalf("default semantic compactor rejected: %v", err)
	}
	runtime.PreModel = nil
	if err := runtime.ValidateConfig(context.Background(), def); !errors.Is(err, ErrRuntimePreModelCompactorMissing) {
		t.Fatalf("explicitly removed required compactor error=%v", err)
	}
	runtime.PreModel = &DefaultRuntimePreModelCompactor{Strategies: []RuntimePreModelCompactionStrategy{semanticSummaryTestStrategy{}}}
	if err := runtime.ValidateConfig(context.Background(), def); err != nil {
		t.Fatalf("configured semantic compactor rejected: %v", err)
	}
}

func TestDirectToolAuthorizationIsVersionSafe(t *testing.T) {
	tests := []struct {
		name string
		refs []string
		call ModelToolCall
		want bool
	}{
		{name: "resolved name", refs: []string{"search@v1"}, call: ModelToolCall{Name: "search"}, want: true},
		{name: "exact encoded version", refs: []string{"search@v1"}, call: ModelToolCall{Name: "search@v1"}, want: true},
		{name: "exact separate version", refs: []string{"search@v1"}, call: ModelToolCall{Name: "search", Version: "v1"}, want: true},
		{name: "version mismatch", refs: []string{"search@v1"}, call: ModelToolCall{Name: "search", Version: "v2"}, want: false},
		{name: "encoded mismatch", refs: []string{"search@v1"}, call: ModelToolCall{Name: "search@v2"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := directToolAuthorized(tt.refs, tt.call); got != tt.want {
				t.Fatalf("authorization = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDirectToolEventSinkRejectsLateAndCancelledEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan observability.AgentEvent, 2)
	sink := newDirectToolEventSink(ctx, out)
	if err := sink.Emit(context.Background(), observability.AgentEvent{EventType: observability.EventToolCallStarted}); err != nil {
		t.Fatalf("emit start: %v", err)
	}
	if err := sink.Emit(context.Background(), observability.AgentEvent{EventType: observability.EventToolCallCompleted}); err != nil {
		t.Fatalf("emit terminal: %v", err)
	}
	sink.Close()
	if err := sink.Emit(context.Background(), observability.AgentEvent{EventType: observability.EventToolCallProgress}); !errors.Is(err, ErrDirectInvalidToolEvent) {
		t.Fatalf("late event error = %v", err)
	}

	cancelledSink := newDirectToolEventSink(ctx, make(chan observability.AgentEvent))
	cancel()
	if err := cancelledSink.Emit(context.Background(), observability.AgentEvent{EventType: observability.EventToolCallStarted}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sink error = %v", err)
	}
}

func TestNativeDirectRuntimeRejectsIncompleteGatewayLifecycles(t *testing.T) {
	t.Run("model lifecycle", func(t *testing.T) {
		models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{modelTextEvent("partial")}}}
		runtime := NewNativeDirectRuntime(models, nil, nil)
		events, err := runtime.Run(WithModelContextPackage(context.Background(), directTestPackage("run_1", "pkg_1")), directTestRunRequest("run_1"))
		if err != nil {
			t.Fatalf("run failed before stream: %v", err)
		}
		failed := eventByType(t, collect(events), EventRunFailed)
		if failed.Error == nil || failed.Error.Code != "DIRECT_MODEL_EVENT_INVALID" {
			t.Fatalf("incomplete model lifecycle changed: %#v", failed)
		}
	})

	t.Run("tool lifecycle", func(t *testing.T) {
		call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{}`)}
		models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
			modelEvent(observability.EventModelCallStarted),
			modelToolEvent(call),
			modelEvent(observability.EventModelCallCompleted),
		}}}
		tools := &capturingToolInvoker{result: ToolInvocationResult{Content: "orphan result"}}
		runtime := NewNativeDirectRuntime(models, tools, &capturingModelContextRebuilder{})
		pkg := directTestPackage("run_1", "pkg_1")
		pkg.Capabilities.Tools = []string{"search"}
		events, err := runtime.Run(WithModelContextPackage(context.Background(), sealDirectTestPackage(pkg)), directTestRunRequest("run_1"))
		if err != nil {
			t.Fatalf("run failed before stream: %v", err)
		}
		failed := eventByType(t, collect(events), EventRunFailed)
		if failed.Error == nil || failed.Error.Code != "DIRECT_TOOL_EVENT_INVALID" {
			t.Fatalf("incomplete tool lifecycle changed: %#v", failed)
		}
	})
}

func TestNativeDirectRuntimeCompletesThroughRuntimeService(t *testing.T) {
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{{
		modelEvent(observability.EventModelCallStarted),
		modelTextEvent("native answer"),
		modelEvent(observability.EventModelCallCompleted),
	}}}
	runtime := NewNativeDirectRuntime(models, nil, nil)
	state := NewInMemoryStateManager()
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := directTestRunRequest("run_native")
	req.Input = []Message{{Role: "user", Content: "hello"}}

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("runtime service failed: %v", err)
	}
	collected := collect(events)
	if eventIndex(collected, EventModelContextBuilt) < 0 || eventIndex(collected, EventAgentTextDelta) < 0 || eventIndex(collected, EventFinalResponse) < 0 || collected[len(collected)-1].EventType != EventRunCompleted {
		t.Fatalf("native direct lifecycle incomplete: %#v", collected)
	}
	run, ok := state.Run("run_native")
	if !ok || run.Status != RunStatusCompleted {
		t.Fatalf("native direct run did not complete: %#v", run)
	}
}

func TestNativeDirectRuntimeWaitsForModelCompletionCommitBeforeToolExecution(t *testing.T) {
	service, tools, req := newNativeToolCommitFixture()
	blocked := &blockingModelCompletionWriter{
		delegate: service.Writer,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	service.Writer = blocked

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("runtime service failed: %v", err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("model completion never reached the storage writer")
	}
	time.Sleep(25 * time.Millisecond)
	if got := len(tools.Requests()); got != 0 {
		t.Fatalf("tool executed before model completion commit: requests=%d", got)
	}
	close(blocked.release)
	collected := collect(events)
	if got := len(tools.Requests()); got != 1 {
		t.Fatalf("tool requests=%d events=%#v", got, collected)
	}
	modelCompleted := eventIndex(collected, observability.EventModelCallCompleted)
	toolStarted := eventIndex(collected, observability.EventToolCallStarted)
	if modelCompleted < 0 || toolStarted < 0 || modelCompleted >= toolStarted {
		t.Fatalf("causal event order changed: model_completed=%d tool_started=%d events=%#v", modelCompleted, toolStarted, collected)
	}
}

func TestNativeDirectRuntimeDoesNotExecuteToolWhenModelCompletionCommitFails(t *testing.T) {
	service, tools, req := newNativeToolCommitFixture()
	want := errors.New("model completion append failed")
	service.Writer = failingModelCompletionWriter{delegate: service.Writer, err: want}
	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("runtime service failed before stream: %v", err)
	}
	done := make(chan []observability.AgentEvent, 1)
	go func() { done <- collect(events) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime deadlocked after model completion append failure")
	}
	if got := len(tools.Requests()); got != 0 {
		t.Fatalf("tool executed after model completion append failure: requests=%d", got)
	}
}

func newNativeToolCommitFixture() (*RuntimeService, *capturingToolInvoker, RunRequest) {
	call := ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)}
	models := &scriptedModelInvoker{scripts: [][]ModelStreamItem{
		{
			modelEvent(observability.EventModelCallStarted),
			modelToolEvent(call),
			modelEvent(observability.EventModelCallCompleted),
		},
		{
			modelEvent(observability.EventModelCallStarted),
			modelTextEvent("done"),
			modelEvent(observability.EventModelCallCompleted),
		},
	}}
	tools := &capturingToolInvoker{
		result: ToolInvocationResult{Content: "result"},
		events: toolLifecycleEvents(),
	}
	second := directTestPackage("run_native_tool", "pkg_2")
	runtime := NewNativeDirectRuntime(models, tools, &capturingModelContextRebuilder{pkg: second})
	state := NewInMemoryStateManager()
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	first := directTestPackage("run_native_tool", "pkg_1")
	first.Capabilities.Tools = []string{"search"}
	first = sealDirectTestPackage(first)
	service.Assembler = fixedRuntimeContextAssembler{pkg: first}
	req := directTestRunRequest("run_native_tool")
	req.Input = []Message{{Role: "user", Content: "hello"}}
	return service, tools, req
}

type fixedRuntimeContextAssembler struct{ pkg ModelContextPackage }

func (a fixedRuntimeContextAssembler) Build(context.Context, RuntimeContextAssemblyRequest) (ModelContextPackage, error) {
	return a.pkg, nil
}

type blockingModelCompletionWriter struct {
	delegate StorageWriteExecutor
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

type failingModelCompletionWriter struct {
	delegate StorageWriteExecutor
	err      error
}

func (w failingModelCompletionWriter) Execute(ctx context.Context, plan storagewrite.Plan) (storagewrite.Result, error) {
	for _, write := range plan.RequiredWrites {
		event, ok := write.Payload.(observability.AgentEvent)
		if ok && event.EventType == observability.EventModelCallCompleted {
			return storagewrite.Result{}, w.err
		}
	}
	return w.delegate.Execute(ctx, plan)
}

func (w *blockingModelCompletionWriter) Execute(ctx context.Context, plan storagewrite.Plan) (storagewrite.Result, error) {
	for _, write := range plan.RequiredWrites {
		event, ok := write.Payload.(observability.AgentEvent)
		if !ok || event.EventType != observability.EventModelCallCompleted {
			continue
		}
		w.once.Do(func() { close(w.entered) })
		select {
		case <-ctx.Done():
			return storagewrite.Result{}, ctx.Err()
		case <-w.release:
		}
		break
	}
	return w.delegate.Execute(ctx, plan)
}

func modelEvent(eventType observability.EventType) ModelStreamItem {
	return ModelStreamItem{Event: observability.AgentEvent{EventType: eventType, Visibility: observability.VisibilityDebug}}
}

func modelTextEvent(text string) ModelStreamItem {
	return ModelStreamItem{
		Event:     observability.AgentEvent{EventType: observability.EventModelTokenDelta, Visibility: observability.VisibilityInternal, Payload: JSONPayload(map[string]string{"text": text})},
		TextDelta: text,
	}
}

func modelToolEvent(call ModelToolCall) ModelStreamItem {
	return ModelStreamItem{
		Event:    observability.AgentEvent{EventType: observability.EventModelToolCallDelta, Visibility: observability.VisibilityInternal},
		ToolCall: &call,
	}
}

func toolLifecycleEvents() []observability.AgentEvent {
	return []observability.AgentEvent{
		{EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityDebug},
		{EventType: observability.EventToolCallCompleted, Visibility: observability.VisibilityDebug},
	}
}

func directTestRunRequest(runID string) RunRequest {
	req := testRunRequest()
	req.RunID = runID
	req.SessionID = "session_" + runID
	req.Definition.Runtime = RuntimeSpec{Type: RuntimeTypeNative, Mode: RuntimeModeDirect}
	return req
}

func directTestPackage(runID, packageID string) ModelContextPackage {
	pkg := ModelContextPackage{
		SchemaVersion: ModelContextPackageSchemaVersion,
		PackageID:     packageID,
		Run:           ModelContextRun{RunID: runID, SessionID: "session_" + runID, AgentID: "agent_1", Runtime: string(RuntimeTypeNative), RuntimeMode: string(RuntimeModeDirect)},
	}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	return pkg
}

func sealDirectTestPackage(pkg ModelContextPackage) ModelContextPackage {
	if len(pkg.Capabilities.ToolDefinitions) == 0 {
		for _, ref := range pkg.Capabilities.Tools {
			pkg.Capabilities.ToolDefinitions = append(pkg.Capabilities.ToolDefinitions, ModelToolDefinition{Name: strings.SplitN(ref, "@", 2)[0], Schema: []byte(`{"type":"object"}`)})
		}
		for _, snapshot := range pkg.Capabilities.MCPSnapshots {
			for _, tool := range snapshot.Tools {
				pkg.Capabilities.ToolDefinitions = append(pkg.Capabilities.ToolDefinitions, ModelToolDefinition{Name: tool.Name, Description: tool.Description, Schema: append([]byte(nil), tool.InputSchema...)})
			}
		}
	}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	return pkg
}

func assertEventTypes(t *testing.T, events []observability.AgentEvent, want ...observability.EventType) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d: %#v", len(events), len(want), events)
	}
	for i := range want {
		if events[i].EventType != want[i] {
			t.Fatalf("event %d = %s, want %s: %#v", i, events[i].EventType, want[i], events)
		}
	}
}

type scriptedModelInvoker struct {
	mu       sync.Mutex
	scripts  [][]ModelStreamItem
	requests []ModelInvokeRequest
}

func (i *scriptedModelInvoker) Invoke(_ context.Context, req ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.requests = append(i.requests, req)
	index := len(i.requests) - 1
	if index >= len(i.scripts) {
		return nil, errors.New("model script exhausted")
	}
	out := make(chan ModelStreamItem, len(i.scripts[index]))
	for _, item := range i.scripts[index] {
		out <- item
	}
	close(out)
	return out, nil
}

func (i *scriptedModelInvoker) Requests() []ModelInvokeRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]ModelInvokeRequest, len(i.requests))
	copy(out, i.requests)
	return out
}

type capturingToolInvoker struct {
	mu       sync.Mutex
	result   ToolInvocationResult
	events   []observability.AgentEvent
	err      error
	requests []ToolInvocationRequest
}

func (i *capturingToolInvoker) Invoke(ctx context.Context, req ToolInvocationRequest, sink ToolEventSink) (ToolInvocationResult, error) {
	i.mu.Lock()
	i.requests = append(i.requests, req)
	result := i.result
	events := append([]observability.AgentEvent(nil), i.events...)
	err := i.err
	i.mu.Unlock()
	for _, event := range events {
		if emitErr := sink.Emit(ctx, event); emitErr != nil {
			return ToolInvocationResult{}, emitErr
		}
	}
	return result, err
}

func (i *capturingToolInvoker) Requests() []ToolInvocationRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]ToolInvocationRequest, len(i.requests))
	copy(out, i.requests)
	return out
}

type capturingModelContextRebuilder struct {
	mu       sync.Mutex
	pkg      ModelContextPackage
	err      error
	requests []ModelContextRebuildRequest
}

func (r *capturingModelContextRebuilder) Rebuild(_ context.Context, req ModelContextRebuildRequest) (ModelContextPackage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return r.pkg, r.err
}

func (r *capturingModelContextRebuilder) Requests() []ModelContextRebuildRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelContextRebuildRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

type blockingModelInvoker struct {
	started   chan struct{}
	cancelled chan struct{}
}

type invalidCancellableModelInvoker struct {
	cancelled chan struct{}
}

func (i *invalidCancellableModelInvoker) Invoke(ctx context.Context, _ ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	out := make(chan ModelStreamItem, 2)
	out <- modelEvent(observability.EventModelCallStarted)
	out <- ModelStreamItem{Event: observability.AgentEvent{EventType: EventRunCompleted}}
	go func() {
		<-ctx.Done()
		close(i.cancelled)
	}()
	return out, nil
}

func (i *blockingModelInvoker) Invoke(ctx context.Context, _ ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	out := make(chan ModelStreamItem)
	close(i.started)
	go func() {
		defer close(out)
		<-ctx.Done()
		close(i.cancelled)
	}()
	return out, nil
}

type packageEchoModelInvoker struct{}

func (*packageEchoModelInvoker) Invoke(_ context.Context, req ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	out := make(chan ModelStreamItem, 3)
	out <- modelEvent(observability.EventModelCallStarted)
	out <- modelTextEvent(req.Package.Run.RunID)
	out <- modelEvent(observability.EventModelCallCompleted)
	close(out)
	return out, nil
}

type streamingToolInvoker struct {
	release chan struct{}
}

func (i *streamingToolInvoker) Invoke(ctx context.Context, _ ToolInvocationRequest, sink ToolEventSink) (ToolInvocationResult, error) {
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallStarted, Visibility: observability.VisibilityDebug}); err != nil {
		return ToolInvocationResult{}, err
	}
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallProgress, Visibility: observability.VisibilityUserVisible}); err != nil {
		return ToolInvocationResult{}, err
	}
	select {
	case <-ctx.Done():
		return ToolInvocationResult{}, ctx.Err()
	case <-i.release:
	}
	if err := sink.Emit(ctx, observability.AgentEvent{EventType: observability.EventToolCallCompleted, Visibility: observability.VisibilityDebug}); err != nil {
		return ToolInvocationResult{}, err
	}
	return ToolInvocationResult{Content: "ok"}, nil
}
