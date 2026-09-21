package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

func TestRuntimeConformanceMissingAssemblerDoesNotExecuteAdapter(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &countingRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted})}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = nil

	if _, err := service.Run(context.Background(), testRunRequest()); !errors.Is(err, ErrModelContextPackageMissing) {
		t.Fatalf("expected ErrModelContextPackageMissing, got %v", err)
	}
	if runtime.RunCount() != 0 {
		t.Fatalf("adapter should not run without assembler, count=%d", runtime.RunCount())
	}
	if _, ok := state.Run("run_1"); ok {
		t.Fatal("run should not be created when assembler is missing")
	}
}

func TestRuntimeConformanceModelContextBuiltBeforeAdapterEvents(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	collected := collect(events)

	modelContextIdx := eventIndex(collected, EventModelContextBuilt)
	modelCallIdx := eventIndex(collected, observability.EventModelCallStarted)
	if modelContextIdx < 0 || modelCallIdx < 0 {
		t.Fatalf("expected model_context_built and model_call_started, got %#v", collected)
	}
	if modelContextIdx > modelCallIdx {
		t.Fatalf("model_context_built must precede adapter events: context=%d model_call=%d", modelContextIdx, modelCallIdx)
	}
}

func TestProductionRuntimeContextAssemblerFailsClosed(t *testing.T) {
	if _, err := NewProductionRuntimeContextAssembler(nil, nil, nil, nil, nil); !errors.Is(err, ErrProductionContextDependency) {
		t.Fatalf("expected missing dependency error, got %v", err)
	}
	assembler, err := NewProductionRuntimeContextAssembler(
		nil,
		fixedContextSnapshotProvider{snapshot: ContextSnapshot{}},
		fixedCapabilitySnapshotProvider{},
		fixedTokenBudgetAllocator{budget: ModelTokenBudget{MaxInputTokens: 4096, ReservedOutputTokens: 512}},
		packageGuardrailFunc(func(context.Context, ModelContextPackage) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(assembler.ValidateProduction(), ErrProductionContextDependency) {
		t.Fatal("production assembler accepted a missing system prompt resolver")
	}
	prompt := promptTestSnapshot("prompt://production/system", "v1", "prompt-snapshot://production-v1", "sha256:production-v1", "trusted")
	assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: prompt}
	if err := assembler.ValidateProduction(); err != nil {
		t.Fatalf("validate production assembler: %v", err)
	}
	req := testRunRequest()
	req.Definition = testPromptAssemblyRequest(prompt).Run.Definition
	_, err = assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, ErrProductionRunBinding) {
		t.Fatalf("expected missing run binding rejection, got %v", err)
	}
	req.AgentBindingID = "binding_production"
	req.ConfigSnapshotRef = "agent-config://production/v1/sha256:config"
	req.ConfigHash = "sha256:config"
	_, err = assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, ErrProductionContextSnapshot) {
		t.Fatalf("expected invalid snapshot rejection, got %v", err)
	}

	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{Ref: "ctx://production", ContentHash: "sha256:ctx"}}
	req.Definition = AgentDefinition{AgentID: "missing_prompt", Version: "v1", Runtime: RuntimeSpec{Type: RuntimeTypeMock, Mode: RuntimeModeReact}}
	_, err = assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, ErrSystemPromptSnapshotMismatch) {
		t.Fatalf("expected missing frozen prompt rejection, got %v", err)
	}

	req.Definition = testPromptAssemblyRequest(prompt).Run.Definition
	assembler.CapabilitySnapshots = fixedCapabilitySnapshotProvider{snapshot: CapabilitySnapshot{Tools: []string{"search@v1"}}}
	_, err = assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req, Trace: req.Trace})
	if !errors.Is(err, ErrProductionToolSnapshot) {
		t.Fatalf("expected missing governed tool snapshot rejection, got %v", err)
	}
}

func TestProductionRuntimeServiceRejectsInMemoryState(t *testing.T) {
	_, err := NewProductionRuntimeService(
		NewMockRuntime(),
		NewInMemoryStateManager(),
		storagewrite.NewExecutor(map[storagewrite.StoreName]storagewrite.StorePort{}),
		NewDefaultRuntimeContextAssembler(nil),
		observability.NoopLogger{},
		observability.NewNoopTracer("test"),
	)
	if !errors.Is(err, ErrProductionMemoryBackend) {
		t.Fatalf("expected in-memory state rejection, got %v", err)
	}
}

func TestRuntimeConformanceContextBuildFailedStopsAdapterAndRunStarted(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &countingRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted})}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = failingAssembler{err: errors.New("context snapshot unavailable")}

	if _, err := service.Run(context.Background(), testRunRequest()); err == nil {
		t.Fatal("expected context build failure")
	}
	if runtime.RunCount() != 0 {
		t.Fatalf("adapter should not run after context build failure, count=%d", runtime.RunCount())
	}
	events := state.Events("run_1")
	if eventIndex(events, EventModelContextBuildFailed) < 0 {
		t.Fatalf("model_context_build_failed missing: %#v", events)
	}
	if eventIndex(events, EventRunStarted) >= 0 {
		t.Fatalf("run_started must not be emitted after context build failure: %#v", events)
	}
	if eventIndex(events, observability.EventModelCallStarted) >= 0 {
		t.Fatalf("model_call_started must not be emitted after context build failure: %#v", events)
	}
}

func TestRuntimeConformanceAssemblerExtensionPointsFeedPackage(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &modelContextCapturingRuntime{MockRuntime: *NewMockRuntime()}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{Ref: "ctxsnap_1"}}
	assembler.CapabilitySnapshots = fixedCapabilitySnapshotProvider{snapshot: CapabilitySnapshot{
		Tools:      []string{"tool.snapshot"},
		Skills:     []string{"skill.snapshot"},
		SubAgents:  []string{"agent.snapshot"},
		MCPServers: []string{"mcp.snapshot"},
	}}
	assembler.TokenBudgets = fixedTokenBudgetAllocator{budget: ModelTokenBudget{MaxInputTokens: 32000, ReservedOutputTokens: 4096}}
	assembler.Guardrail = packageGuardrailFunc(func(_ context.Context, pkg ModelContextPackage) error {
		if pkg.Run.ContextSnapshotRef == "" {
			return errors.New("context snapshot ref missing")
		}
		return nil
	})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = assembler

	events, err := service.Run(context.Background(), testRunRequest())
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)

	pkg := runtime.pkg
	if pkg.Run.ContextSnapshotRef != "ctxsnap_1" {
		t.Fatalf("context snapshot provider not applied: %#v", pkg.Run)
	}
	if got := pkg.Capabilities.Tools; len(got) != 1 || got[0] != "tool.snapshot" {
		t.Fatalf("capability provider not applied: %#v", pkg.Capabilities)
	}
	if pkg.RuntimeConstraints.TokenBudget.MaxInputTokens != 32000 || pkg.RuntimeConstraints.TokenBudget.ReservedOutputTokens != 4096 {
		t.Fatalf("token budget provider not applied: %#v", pkg.RuntimeConstraints.TokenBudget)
	}
}

func TestRuntimeContextAssemblerBuildsFromImmutableSnapshotMessages(t *testing.T) {
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref:          "ctxsnap_1",
		ContentHash:  "sha256:snapshot",
		LastSequence: 2,
		Messages: []contextpkg.Message{
			{ID: "history_1", SessionID: "session_1", Sequence: 1, Role: contextpkg.RoleUser, Content: "historical input"},
			{ID: "history_2", SessionID: "session_1", Sequence: 2, Role: contextpkg.RoleAssistant, Content: "historical reply"},
		},
	}}
	req := RuntimeContextAssemblyRequest{Run: testRunRequest(), Trace: observability.TraceContext{TraceID: "trace_1"}}
	pkg, err := assembler.Build(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var historyIDs []string
	for _, message := range pkg.Messages.ConversationWindow {
		if message.ID != "" {
			historyIDs = append(historyIDs, message.ID)
		}
	}
	if len(historyIDs) != 2 || historyIDs[0] != "history_1" || historyIDs[1] != "history_2" {
		t.Fatalf("assembler ignored immutable snapshot: %#v", pkg.Messages.ConversationWindow)
	}
}

func TestRuntimeConformanceConcurrentRunsKeepPackageAndTraceIsolated(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &multiPackageCapturingRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	const runs = 16
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := testRunRequest()
			req.RunID = fmt.Sprintf("run_%d", i)
			req.SessionID = fmt.Sprintf("session_%d", i)
			req.Trace = observability.TraceContext{TraceID: fmt.Sprintf("trace_%d", i)}
			events, err := service.Run(context.Background(), req)
			if err != nil {
				t.Errorf("run %d failed: %v", i, err)
				return
			}
			_ = collect(events)
		}(i)
	}
	wg.Wait()

	packages := runtime.Packages()
	if len(packages) != runs {
		t.Fatalf("captured package count mismatch: got %d want %d", len(packages), runs)
	}
	for i := 0; i < runs; i++ {
		runID := fmt.Sprintf("run_%d", i)
		pkg, ok := packages[runID]
		if !ok {
			t.Fatalf("package missing for %s", runID)
		}
		if pkg.Run.RunID != runID {
			t.Fatalf("package run id crossed: key=%s pkg=%#v", runID, pkg.Run)
		}
		if pkg.Observability.TraceID != fmt.Sprintf("trace_%d", i) {
			t.Fatalf("package trace crossed: key=%s pkg=%#v", runID, pkg.Observability)
		}
	}
}

func TestRuntimeConformanceResumeBuildsModelContextBeforeAdapter(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	runtime := &resumePackageCapturingRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug})}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	events, err := service.Resume(context.Background(), ResumeRequest{
		SessionID:        runReq.SessionID,
		RunID:            runReq.RunID,
		Definition:       runReq.Definition,
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_token_1",
		Trace:            runReq.Trace,
	})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	collected := collect(events)

	if len(collected) == 0 || collected[0].EventType != EventResumeAccepted {
		t.Fatalf("resume_accepted should be first resume event: %#v", collected)
	}
	if eventIndex(collected, EventRunStarted) >= 0 {
		t.Fatalf("resume must not emit a second run_started: %#v", collected)
	}
	if runtime.pkg.PackageID == "" {
		t.Fatal("resume adapter did not receive model context package")
	}
	modelContextIdx := eventIndex(collected, EventModelContextBuilt)
	modelCallIdx := eventIndex(collected, observability.EventModelCallStarted)
	if modelContextIdx < 0 || modelCallIdx < 0 || modelContextIdx > modelCallIdx {
		t.Fatalf("resume model_context_built must precede model_call_started: %#v", collected)
	}
}

func TestRuntimeConformanceResumeRestoresGovernedRunFacts(t *testing.T) {
	prompt := promptTestSnapshot("prompt://resume/system", "v1", "prompt-snapshot://resume-v1", "sha256:resume-v1", "trusted resume prompt")
	runReq := runtimeBindingRunRequest(RuntimeTypeMock)
	runReq.Definition = testPromptAssemblyRequest(prompt).Run.Definition
	runReq.Definition.ToolRefs = []string{"search@v1"}
	runReq.Definition.Metadata["mcp_servers"] = `["maps"]`
	runReq.ContextSnapshotRef = "ctx://run/original"

	runtime := &resumePackageCapturingRuntime{MockRuntime: *NewMockRuntime(
		observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug},
	)}
	state := NewInMemoryStateManager()

	const resumeContextRef = "ctx://run/resume-latest"
	assembler, err := NewProductionRuntimeContextAssembler(
		nil,
		validatingResumeContextProvider{
			contextRef: resumeContextRef,
			tenantID:   runReq.TenantID,
			userID:     runReq.UserID,
		},
		validatingResumeCapabilityProvider{
			tenantID: runReq.TenantID,
			userID:   runReq.UserID,
		},
		fixedTokenBudgetAllocator{budget: ModelTokenBudget{MaxInputTokens: 4096, ReservedOutputTokens: 512}},
		packageGuardrailFunc(func(context.Context, ModelContextPackage) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: prompt}
	initialReq := runReq
	initialReq.ContextSnapshotRef = resumeContextRef
	initialPackage, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{
		Run: initialReq, Handle: AgentHandle{Runtime: RuntimeTypeMock}, Trace: initialReq.Trace,
	})
	if err != nil {
		t.Fatalf("build initial governed package: %v", err)
	}
	capabilityBinding, err := newRuntimeCapabilityBinding(initialPackage.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	prepareWaitingBoundRunWithCapabilities(t, state, runtime, runReq, capabilityBinding)
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = assembler

	resume := runtimeBindingResumeRequest(runReq)
	resume.ContextSnapshotRef = resumeContextRef
	resumed, err := service.Resume(context.Background(), resume)
	if err != nil {
		t.Fatalf("resume governed run: %v", err)
	}
	_ = collect(resumed)

	pkg := runtime.pkg
	if pkg.Run.ContextSnapshotRef != resumeContextRef || pkg.Security.TenantID != runReq.TenantID || pkg.Security.UserID != runReq.UserID {
		t.Fatalf("resume facts were not restored: run=%#v security=%#v", pkg.Run, pkg.Security)
	}
	if pkg.Capabilities.ToolSnapshot == nil || pkg.Capabilities.ToolSnapshot.SnapshotID != "tool-snapshot-resume" || len(pkg.Capabilities.MCPSnapshots) != 1 {
		t.Fatalf("governed tool/MCP snapshots did not reach resume package: %#v", pkg.Capabilities)
	}
}

func TestRuntimeConformanceResumeCapabilityUnavailableReturnsClaimForRetry(t *testing.T) {
	prompt := promptTestSnapshot("prompt://resume/retry", "v1", "prompt-snapshot://resume-retry", "sha256:resume-retry", "trusted retry prompt")
	runReq := runtimeBindingRunRequest(RuntimeTypeMock)
	runReq.Definition = testPromptAssemblyRequest(prompt).Run.Definition
	runReq.ContextSnapshotRef = "ctx://resume/retry"
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime(
		observability.AgentEvent{EventType: observability.EventModelCallStarted, Visibility: observability.VisibilityDebug},
	)}
	state := NewInMemoryStateManager()
	capabilityBinding, err := newRuntimeCapabilityBinding(ModelContextCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	prepareWaitingBoundRunWithCapabilities(t, state, runtime, runReq, capabilityBinding)
	capabilities := &failOnceCapabilitySnapshotProvider{err: fmt.Errorf("%w: tool registry", ErrProductionCapabilityUnavailable)}
	assembler, err := NewProductionRuntimeContextAssembler(
		nil,
		fixedContextSnapshotProvider{snapshot: ContextSnapshot{Ref: runReq.ContextSnapshotRef, ContentHash: "sha256:context"}},
		capabilities,
		fixedTokenBudgetAllocator{budget: ModelTokenBudget{MaxInputTokens: 4096, ReservedOutputTokens: 512}},
		packageGuardrailFunc(func(context.Context, ModelContextPackage) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: prompt}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = assembler
	resume := runtimeBindingResumeRequest(runReq)

	_, err = service.Resume(context.Background(), resume)
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != "CAPABILITY_SNAPSHOT_UNAVAILABLE" || !runtimeErr.Retryable || runtimeErr.Dependency != "capability_registry" {
		t.Fatalf("capability dependency error was not retryable: %#v, err=%v", runtimeErr, err)
	}
	if snapshot, ok := state.Run(runReq.RunID); !ok || snapshot.Status != RunStatusWaitingControl {
		t.Fatalf("retryable capability failure consumed resume claim: %#v", snapshot)
	}
	if runtime.ResumeCount() != 0 {
		t.Fatal("runtime executed while capability registry was unavailable")
	}

	events, err := service.Resume(context.Background(), resume)
	if err != nil {
		t.Fatalf("retry same resume token: %v", err)
	}
	_ = collect(events)
	if runtime.ResumeCount() != 1 {
		t.Fatalf("runtime resume calls=%d, want 1", runtime.ResumeCount())
	}
}

func TestRuntimeConformanceCancelledResumePersistsRetryableFailure(t *testing.T) {
	prompt := promptTestSnapshot("prompt://resume/timeout", "v1", "prompt-snapshot://resume-timeout", "sha256:resume-timeout", "trusted timeout prompt")
	runReq := runtimeBindingRunRequest(RuntimeTypeMock)
	runReq.Definition = testPromptAssemblyRequest(prompt).Run.Definition
	runReq.ContextSnapshotRef = "ctx://resume/timeout"
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime()}
	state := NewInMemoryStateManager()
	capabilityBinding, err := newRuntimeCapabilityBinding(ModelContextCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	prepareWaitingBoundRunWithCapabilities(t, state, runtime, runReq, capabilityBinding)
	checkedState := &contextCheckingStateManager{RuntimeStateManager: state}
	assembler, err := NewProductionRuntimeContextAssembler(
		nil,
		fixedContextSnapshotProvider{snapshot: ContextSnapshot{Ref: runReq.ContextSnapshotRef, ContentHash: "sha256:context"}},
		blockingCapabilitySnapshotProvider{},
		fixedTokenBudgetAllocator{budget: ModelTokenBudget{MaxInputTokens: 4096, ReservedOutputTokens: 512}},
		packageGuardrailFunc(func(context.Context, ModelContextPackage) error { return nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: prompt}
	service := NewRuntimeService(runtime, checkedState, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = assembler
	resume := runtimeBindingResumeRequest(runReq)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = service.Resume(ctx, resume)
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || !runtimeErr.Retryable || runtimeErr.Code != "RUNTIME_TIMEOUT" {
		t.Fatalf("cancelled capability build error=%v runtime_error=%#v", err, runtimeErr)
	}
	if checkedState.CancelledFailResumeCalls() != 0 {
		t.Fatal("fail-resume write reused the cancelled request context")
	}
	if snapshot, ok := state.Run(runReq.RunID); !ok || snapshot.Status != RunStatusWaitingControl {
		t.Fatalf("cancelled resume remained claimed: %#v", snapshot)
	}

	assembler.CapabilitySnapshots = fixedCapabilitySnapshotProvider{}
	events, err := service.Resume(context.Background(), resume)
	if err != nil {
		t.Fatalf("retry after timeout: %v", err)
	}
	_ = collect(events)
}

func TestRuntimeConformanceResumeContextBuildFailedStopsAdapter(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted})}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = failingAssembler{err: errors.New("resume context unavailable")}

	if _, err := service.Resume(context.Background(), ResumeRequest{
		SessionID:        runReq.SessionID,
		RunID:            runReq.RunID,
		Definition:       runReq.Definition,
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_token_1",
		Trace:            runReq.Trace,
	}); err == nil {
		t.Fatal("expected resume context build failure")
	}
	if runtime.ResumeCount() != 0 {
		t.Fatalf("resume adapter should not run after context build failure, count=%d", runtime.ResumeCount())
	}
	events := state.Events(runReq.RunID)
	if eventIndex(events, EventModelContextBuildFailed) < 0 {
		t.Fatalf("model_context_build_failed missing: %#v", events)
	}
	if eventIndex(events, observability.EventModelCallStarted) >= 0 {
		t.Fatalf("model_call_started must not be emitted after resume context build failure: %#v", events)
	}
}

func TestRuntimeConformanceCancelledResumePreparationReturnsClaim(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.Assembler = failingAssembler{err: context.Canceled}

	_, err := service.Resume(context.Background(), ResumeRequest{
		SessionID: runReq.SessionID, RunID: runReq.RunID, Definition: runReq.Definition,
		CheckpointID: "checkpoint_1", ControlRequestID: "control_1", ResumeToken: "resume_token_1", Trace: runReq.Trace,
	})
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != "RESUME_PREPARATION_CANCELLED" || !runtimeErr.Retryable {
		t.Fatalf("cancelled resume preparation error=%v runtime_error=%#v", err, runtimeErr)
	}
	if snapshot, ok := state.Run(runReq.RunID); !ok || snapshot.Status != RunStatusWaitingControl {
		t.Fatalf("client cancellation consumed the resume claim: %#v", snapshot)
	}
	if runtime.ResumeCount() != 0 {
		t.Fatal("runtime executed after cancelled resume preparation")
	}
}

func TestRuntimeConformanceResumeAdapterErrorFailsRun(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	runtime := &MockRuntime{RunError: errors.New("resume upstream unavailable")}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	if _, err := service.Resume(context.Background(), ResumeRequest{
		SessionID:        runReq.SessionID,
		RunID:            runReq.RunID,
		Definition:       runReq.Definition,
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_token_1",
		Trace:            runReq.Trace,
	}); err == nil {
		t.Fatal("expected resume adapter error")
	}
	run, ok := state.Run(runReq.RunID)
	if !ok {
		t.Fatal("run state missing")
	}
	if run.Status != RunStatusFailed {
		t.Fatalf("resume adapter error should fail run: %#v", run)
	}
	events := state.Events(runReq.RunID)
	if eventIndex(events, EventModelContextBuilt) < 0 || eventIndex(events, EventRunFailed) < 0 {
		t.Fatalf("expected model_context_built and run_failed events: %#v", events)
	}
}

func TestRuntimeConformanceResumeRejectsMissingBinding(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := testRunRequest()

	_, err := service.Resume(context.Background(), ResumeRequest{SessionID: req.SessionID, RunID: req.RunID, Definition: req.Definition})
	if !errors.Is(err, ErrCheckpointIDMissing) {
		t.Fatalf("expected checkpoint validation error, got %v", err)
	}
	if runtime.ResumeCount() != 0 {
		t.Fatalf("adapter should not run for invalid resume, count=%d", runtime.ResumeCount())
	}
}

func TestRuntimeConformanceResumeRejectsBindingMismatch(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	runtime := &countingResumeRuntime{MockRuntime: *NewMockRuntime()}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	_, err := service.Resume(context.Background(), ResumeRequest{
		SessionID:        runReq.SessionID,
		RunID:            runReq.RunID,
		Definition:       runReq.Definition,
		CheckpointID:     "checkpoint_wrong",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_token_1",
		Trace:            runReq.Trace,
	})
	if !errors.Is(err, ErrResumeBindingMismatch) {
		t.Fatalf("expected resume binding mismatch, got %v", err)
	}
	if runtime.ResumeCount() != 0 {
		t.Fatalf("adapter should not run for mismatched resume, count=%d", runtime.ResumeCount())
	}
	if eventIndex(state.Events(runReq.RunID), EventResumeAccepted) >= 0 {
		t.Fatal("rejected resume must not persist resume_accepted")
	}
}

func TestRuntimeConformanceConcurrentResumeClaimsOnce(t *testing.T) {
	state := NewInMemoryStateManager()
	runReq := testRunRequest()
	prepareWaitingRun(t, state, runReq)
	req := ResumeRequest{
		SessionID:        runReq.SessionID,
		RunID:            runReq.RunID,
		Definition:       runReq.Definition,
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_token_1",
		Trace:            runReq.Trace,
	}

	const callers = 16
	var wg sync.WaitGroup
	results := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func(attempt int) {
			defer wg.Done()
			attemptID := fmt.Sprintf("resume_attempt_%d", attempt)
			results <- state.BeginResume(context.Background(), req, attemptID, observability.AgentEvent{EventID: "resume_event_" + attemptID, EventType: EventResumeAccepted, RunID: req.RunID})
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, ErrResumeAlreadyClaimed) {
			t.Fatalf("unexpected concurrent resume error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("resume claim successes = %d, want 1", successes)
	}
	acceptedEvents := 0
	for _, event := range state.Events(runReq.RunID) {
		if event.EventType == EventResumeAccepted {
			acceptedEvents++
		}
	}
	if acceptedEvents != 1 {
		t.Fatalf("resume_accepted events = %d, want 1", acceptedEvents)
	}
}

func prepareWaitingRun(t *testing.T, state *InMemoryStateManager, req RunRequest) {
	t.Helper()
	if _, err := state.StartRun(context.Background(), req); err != nil {
		t.Fatalf("start run failed: %v", err)
	}
	binding, err := newRuntimeBinding(context.Background(), NewMockRuntime(), req.Definition, runtimeBindingFactsFromRun(req))
	if err != nil {
		t.Fatalf("build runtime binding failed: %v", err)
	}
	if err := state.BindRuntime(context.Background(), req.RunID, binding); err != nil {
		t.Fatalf("bind runtime failed: %v", err)
	}
	if err := state.EnterWaitingControl(context.Background(), WaitingControlRequest{
		SessionID:        req.SessionID,
		RunID:            req.RunID,
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_token_1",
	}); err != nil {
		t.Fatalf("enter waiting control failed: %v", err)
	}
}

type countingRuntime struct {
	MockRuntime
	mu    sync.Mutex
	count int
}

func (r *countingRuntime) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	return r.MockRuntime.Run(ctx, req)
}

func (r *countingRuntime) RunCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

type countingResumeRuntime struct {
	MockRuntime
	mu    sync.Mutex
	count int
}

func (r *countingResumeRuntime) Resume(ctx context.Context, req ResumeRequest) (<-chan observability.AgentEvent, error) {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
	return r.MockRuntime.Resume(ctx, req)
}

func (r *countingResumeRuntime) ResumeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

type multiPackageCapturingRuntime struct {
	MockRuntime
	mu       sync.Mutex
	packages map[string]ModelContextPackage
}

func (r *multiPackageCapturingRuntime) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	pkg, ok := ModelContextPackageFrom(ctx)
	if !ok {
		return nil, ErrModelContextPackageMissing
	}
	r.mu.Lock()
	if r.packages == nil {
		r.packages = make(map[string]ModelContextPackage)
	}
	r.packages[req.RunID] = pkg
	r.mu.Unlock()
	return r.MockRuntime.Run(ctx, req)
}

func (r *multiPackageCapturingRuntime) Packages() map[string]ModelContextPackage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]ModelContextPackage, len(r.packages))
	for key, pkg := range r.packages {
		out[key] = pkg
	}
	return out
}

type resumePackageCapturingRuntime struct {
	MockRuntime
	pkg ModelContextPackage
}

func (r *resumePackageCapturingRuntime) Resume(ctx context.Context, req ResumeRequest) (<-chan observability.AgentEvent, error) {
	pkg, ok := ModelContextPackageFrom(ctx)
	if !ok {
		return nil, ErrModelContextPackageMissing
	}
	r.pkg = pkg
	return r.MockRuntime.Resume(ctx, req)
}

type fixedContextSnapshotProvider struct {
	snapshot ContextSnapshot
	err      error
}

type validatingResumeContextProvider struct {
	contextRef string
	tenantID   string
	userID     string
}

func (p validatingResumeContextProvider) Materialize(_ context.Context, req RuntimeContextAssemblyRequest) (ContextSnapshot, error) {
	if req.Run.ContextSnapshotRef != p.contextRef || req.Run.TenantID != p.tenantID || req.Run.UserID != p.userID ||
		req.Run.Trace.TenantID != p.tenantID || req.Run.Trace.UserID != p.userID {
		return ContextSnapshot{}, fmt.Errorf("resume context facts mismatch: %#v", req.Run)
	}
	return ContextSnapshot{Ref: p.contextRef, ContentHash: "sha256:resume-context"}, nil
}

type validatingResumeCapabilityProvider struct {
	tenantID string
	userID   string
}

func (p validatingResumeCapabilityProvider) Resolve(_ context.Context, req RuntimeContextAssemblyRequest) (CapabilitySnapshot, error) {
	if req.Run.TenantID != p.tenantID || req.Run.UserID != p.userID || req.Run.Trace.TenantID != p.tenantID || req.Run.Trace.UserID != p.userID {
		return CapabilitySnapshot{}, fmt.Errorf("resume capability principal mismatch: %#v", req.Run)
	}
	return CapabilitySnapshot{
		Tools: []string{"search@v1"},
		ToolSnapshot: &ToolSchemaSnapshot{
			SnapshotID: "tool-snapshot-resume", ToolRefs: []string{"search@v1"},
			CapabilityHash: "sha256:tool-capability", PolicyHash: "sha256:tool-policy",
		},
		ToolDefinitions: []ModelToolDefinition{{
			Name: "search", Description: "frozen search", Schema: json.RawMessage(`{"type":"object"}`),
		}},
		MCPServers: []string{"maps"},
		MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID: "mcp-snapshot-resume", ServerID: "maps", ServerVersion: "v1",
			PrincipalHash: "sha256:principal", PolicyHash: "sha256:mcp-policy", CapabilityHash: "sha256:mcp-capability",
			Tools: []mcp.Tool{{Name: "maps_search", Description: "frozen maps", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		}},
	}, nil
}

func (p fixedContextSnapshotProvider) Materialize(context.Context, RuntimeContextAssemblyRequest) (ContextSnapshot, error) {
	return p.snapshot, p.err
}

type fixedCapabilitySnapshotProvider struct {
	snapshot CapabilitySnapshot
	err      error
}

func (p fixedCapabilitySnapshotProvider) Resolve(context.Context, RuntimeContextAssemblyRequest) (CapabilitySnapshot, error) {
	return p.snapshot, p.err
}

type failOnceCapabilitySnapshotProvider struct {
	mu  sync.Mutex
	err error
}

func (p *failOnceCapabilitySnapshotProvider) Resolve(context.Context, RuntimeContextAssemblyRequest) (CapabilitySnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		err := p.err
		p.err = nil
		return CapabilitySnapshot{}, err
	}
	return CapabilitySnapshot{}, nil
}

type blockingCapabilitySnapshotProvider struct{}

func (blockingCapabilitySnapshotProvider) Resolve(ctx context.Context, _ RuntimeContextAssemblyRequest) (CapabilitySnapshot, error) {
	<-ctx.Done()
	return CapabilitySnapshot{}, ctx.Err()
}

type contextCheckingStateManager struct {
	RuntimeStateManager
	mu                       sync.Mutex
	cancelledFailResumeCalls int
}

func (m *contextCheckingStateManager) FailResumeAttempt(
	ctx context.Context,
	runID, attemptID string,
	event observability.AgentEvent,
	err error,
	retryable bool,
) error {
	if ctx.Err() != nil {
		m.mu.Lock()
		m.cancelledFailResumeCalls++
		m.mu.Unlock()
		return ctx.Err()
	}
	return m.RuntimeStateManager.FailResumeAttempt(ctx, runID, attemptID, event, err, retryable)
}

func (m *contextCheckingStateManager) CancelledFailResumeCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelledFailResumeCalls
}

type fixedTokenBudgetAllocator struct {
	budget ModelTokenBudget
	err    error
}

func (a fixedTokenBudgetAllocator) Allocate(context.Context, RuntimeContextAssemblyRequest, ContextSnapshot, CapabilitySnapshot) (ModelTokenBudget, error) {
	return a.budget, a.err
}

type packageGuardrailFunc func(context.Context, ModelContextPackage) error

func (f packageGuardrailFunc) Validate(ctx context.Context, pkg ModelContextPackage) error {
	return f(ctx, pkg)
}

func eventIndex(events []observability.AgentEvent, eventType observability.EventType) int {
	for i, event := range events {
		if event.EventType == eventType {
			return i
		}
	}
	return -1
}
