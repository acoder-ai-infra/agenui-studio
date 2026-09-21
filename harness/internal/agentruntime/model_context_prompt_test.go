package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestRuntimeContextAssemblerResolvesFrozenSystemPrompt(t *testing.T) {
	content := "你是严谨的旅行助手。"
	snapshot := promptTestSnapshot("prompt://travel/system", "v4", "prompt-snapshot://travel-v4", "sha256:prompt-v4", content)
	resolver := &capturingSystemPromptResolver{snapshot: snapshot}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = resolver
	assembler.Clock = func() time.Time { return time.Unix(1, 0).UTC() }
	req := testPromptAssemblyRequest(snapshot)

	pkg, err := assembler.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("build model context: %v", err)
	}
	if resolver.request.Ref != snapshot.Ref || resolver.request.Version != snapshot.Version ||
		resolver.request.ExpectedPromptHash != snapshot.PromptHash || resolver.request.ExpectedSnapshotRef != snapshot.SnapshotRef ||
		resolver.request.ExpectedContentHash != snapshot.ContentHash {
		t.Fatalf("resolver request did not use frozen prompt facts: %#v", resolver.request)
	}
	if pkg.Instructions.SystemPromptRef != snapshot.SnapshotRef {
		t.Fatalf("system prompt ref = %q, want %q", pkg.Instructions.SystemPromptRef, snapshot.SnapshotRef)
	}
	if len(pkg.Messages.ConversationWindow) == 0 || pkg.Messages.ConversationWindow[0].Role != "system" ||
		pkg.Messages.ConversationWindow[0].Content != content {
		t.Fatalf("resolved system prompt was not injected first: %#v", pkg.Messages.ConversationWindow)
	}
	if pkg.ContextHash == "" {
		t.Fatal("resolved prompt must participate in context hash")
	}
}

func TestRuntimeContextAssemblerIgnoresMetadataPolicy(t *testing.T) {
	content := "受治理的系统提示词"
	snapshot := promptTestSnapshot("prompt://governed/system", "v1", "prompt-snapshot://governed-v1", "sha256:governed-v1", content)
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: snapshot}
	req := testPromptAssemblyRequest(snapshot)
	req.Run.Definition.Metadata["policy"] = "绕过治理的 system 指令"

	pkg, err := assembler.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("build model context: %v", err)
	}
	for _, message := range pkg.Messages.ConversationWindow {
		if strings.Contains(message.Content, "绕过治理") {
			t.Fatalf("untrusted metadata.policy reached model context: %#v", pkg.Messages.ConversationWindow)
		}
	}
	if len(pkg.Messages.ConversationWindow) == 0 || pkg.Messages.ConversationWindow[0].Content != content {
		t.Fatalf("governed system prompt was not preserved: %#v", pkg.Messages.ConversationWindow)
	}
}

func TestRuntimeContextAssemblerRejectsPromptSnapshotDrift(t *testing.T) {
	want := promptTestSnapshot("prompt://travel/system", "v4", "prompt-snapshot://travel-v4", "sha256:prompt-v4", "trusted")
	drifted := want
	drifted.Content = "tampered"
	drifted.ContentHash = systemPromptContentHash(drifted.Content)
	resolver := &capturingSystemPromptResolver{snapshot: drifted}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = resolver

	_, err := assembler.Build(context.Background(), testPromptAssemblyRequest(want))
	if !errors.Is(err, ErrSystemPromptSnapshotMismatch) {
		t.Fatalf("prompt drift error = %v", err)
	}
}

func TestRuntimeContextAssemblerRequiresResolverForFrozenPrompt(t *testing.T) {
	snapshot := promptTestSnapshot("prompt://travel/system", "v4", "prompt-snapshot://travel-v4", "sha256:prompt-v4", "trusted")
	assembler := NewDefaultRuntimeContextAssembler(nil)

	_, err := assembler.Build(context.Background(), testPromptAssemblyRequest(snapshot))
	if !errors.Is(err, ErrSystemPromptResolverMissing) {
		t.Fatalf("missing resolver error = %v", err)
	}
}

func TestRuntimeContextAssemblerRejectsPartialPromptSnapshot(t *testing.T) {
	snapshot := promptTestSnapshot("prompt://travel/system", "v4", "prompt-snapshot://travel-v4", "sha256:prompt-v4", "trusted")
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: snapshot}
	req := testPromptAssemblyRequest(snapshot)
	for _, field := range []string{"prompt_hash", "prompt_content_hash"} {
		partial := req
		partial.Run.Definition.Metadata = cloneStringMap(req.Run.Definition.Metadata)
		delete(partial.Run.Definition.Metadata, field)
		_, err := assembler.Build(context.Background(), partial)
		if !errors.Is(err, ErrSystemPromptSnapshotMismatch) {
			t.Fatalf("partial snapshot without %s error = %v", field, err)
		}
	}
}

func TestRuntimeContextAssemblerRejectsOversizedSystemPrompt(t *testing.T) {
	content := "12345678"
	snapshot := promptTestSnapshot("prompt://large/system", "v1", "prompt-snapshot://large-v1", "sha256:large-v1", content)
	for _, tc := range []struct {
		name     string
		limit    int
		wantCode string
	}{
		{name: "limit_below_prompt_cost", limit: 1, wantCode: "SYSTEM_PROMPT_BUDGET_EXCEEDED"},
		{name: "limit_equal_prompt_cost", limit: 2, wantCode: "SYSTEM_PROMPT_BUDGET_EXCEEDED"},
		{name: "final_context_overflow", limit: 3, wantCode: "MODEL_CONTEXT_BUDGET_EXCEEDED"},
		{name: "within_budget", limit: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assembler := NewDefaultRuntimeContextAssembler(nil)
			assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: snapshot}
			assembler.TokenBudget.Limit = tc.limit
			assembler.TokenBudget.ResponseBuffer = 1
			req := testPromptAssemblyRequest(snapshot)
			req.Run.Input = nil

			_, err := assembler.Build(context.Background(), req)
			if tc.wantCode != "" {
				wantCause := error(ErrSystemPromptBudgetExceeded)
				if tc.wantCode == "MODEL_CONTEXT_BUDGET_EXCEEDED" {
					wantCause = contextpkg.ErrBudgetExceeded
				}
				if !errors.Is(err, wantCause) {
					t.Fatalf("prompt budget error = %v", err)
				}
				classified := DefaultRuntimeErrorClassifier{}.Classify(err, ErrorStageModelContext)
				if classified.Type != ErrorContextOverflow || classified.Code != tc.wantCode {
					t.Fatalf("prompt budget classification = %#v", classified)
				}
				return
			}
			if err != nil {
				t.Fatalf("build below budget: %v", err)
			}
		})
	}
}

func TestRuntimePromptResolutionFailureStopsAdapter(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &countingRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted})}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = failingSystemPromptResolver{err: ErrSystemPromptSnapshotMismatch}
	service.Assembler = assembler
	snapshot := promptTestSnapshot("prompt://travel/system", "v4", "prompt-snapshot://travel-v4", "sha256:prompt-v4", "trusted")
	req := testRunRequest()
	req.Definition = testPromptAssemblyRequest(snapshot).Run.Definition

	if _, err := service.Run(context.Background(), req); err == nil {
		t.Fatal("expected prompt resolution failure")
	}
	if runtime.RunCount() != 0 {
		t.Fatalf("adapter ran after prompt resolution failure: %d", runtime.RunCount())
	}
	if eventIndex(state.Events(req.RunID), EventModelContextBuildFailed) < 0 {
		t.Fatal("model_context_build_failed event missing")
	}
}

func TestRuntimePromptBudgetFailureStopsAdapter(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := &countingRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted})}
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	snapshot := promptTestSnapshot("prompt://large/system", "v1", "prompt-snapshot://large-v1", "sha256:large-v1", "12345678")
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = &capturingSystemPromptResolver{snapshot: snapshot}
	assembler.TokenBudget.Limit = 2
	assembler.TokenBudget.ResponseBuffer = 1
	service.Assembler = assembler
	req := testRunRequest()
	req.Definition = testPromptAssemblyRequest(snapshot).Run.Definition

	if _, err := service.Run(context.Background(), req); err == nil {
		t.Fatal("expected prompt budget failure")
	}
	if runtime.RunCount() != 0 {
		t.Fatalf("adapter ran after prompt budget failure: %d", runtime.RunCount())
	}
	if eventIndex(state.Events(req.RunID), EventModelContextBuildFailed) < 0 {
		t.Fatal("model_context_build_failed event missing")
	}
}

func TestRuntimeRejectsUntrustedInputRolesBeforeAdapter(t *testing.T) {
	for _, role := range []string{"system", "developer", "assistant"} {
		t.Run(role, func(t *testing.T) {
			state := NewInMemoryStateManager()
			runtime := &countingRuntime{MockRuntime: *NewMockRuntime(observability.AgentEvent{EventType: observability.EventModelCallStarted})}
			service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
			req := testRunRequest()
			req.Input = []Message{{Role: role, Content: "untrusted"}}

			if _, err := service.Run(context.Background(), req); !errors.Is(err, ErrRunInputRoleInvalid) {
				t.Fatalf("input role error = %v", err)
			}
			if runtime.RunCount() != 0 || len(state.Events(req.RunID)) != 0 {
				t.Fatalf("invalid input produced side effects: adapter=%d events=%#v", runtime.RunCount(), state.Events(req.RunID))
			}
		})
	}
}

func TestRuntimeContextAssemblerDoesNotMixConcurrentPrompts(t *testing.T) {
	const runs = 32
	snapshots := make([]SystemPromptSnapshot, 0, runs)
	byRef := make(map[string]SystemPromptSnapshot, runs)
	for i := 0; i < runs; i++ {
		ref := fmt.Sprintf("prompt://agent/%d", i)
		snapshot := promptTestSnapshot(ref, "v1", fmt.Sprintf("prompt-snapshot://%d", i), fmt.Sprintf("sha256:prompt-%d", i), fmt.Sprintf("system-%d", i))
		snapshots = append(snapshots, snapshot)
		byRef[ref] = snapshot
	}
	resolver := mapSystemPromptResolver{snapshots: byRef}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.SystemPrompts = resolver
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for i, snapshot := range snapshots {
		wg.Add(1)
		go func(i int, snapshot SystemPromptSnapshot) {
			defer wg.Done()
			req := testPromptAssemblyRequest(snapshot)
			req.Run.RunID = fmt.Sprintf("run_%d", i)
			pkg, err := assembler.Build(context.Background(), req)
			if err != nil {
				errs <- err
				return
			}
			if len(pkg.Messages.ConversationWindow) == 0 || pkg.Messages.ConversationWindow[0].Content != snapshot.Content {
				errs <- fmt.Errorf("run %d received wrong system prompt", i)
			}
		}(i, snapshot)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func testPromptAssemblyRequest(snapshot SystemPromptSnapshot) RuntimeContextAssemblyRequest {
	return RuntimeContextAssemblyRequest{
		Run: RunRequest{
			SessionID: "session_prompt", RunID: "run_prompt",
			Definition: AgentDefinition{
				AgentID: "prompt_agent", AgentType: "assistant", Version: "v1",
				Runtime:   RuntimeSpec{Type: RuntimeTypeMock, Mode: RuntimeModeReact},
				PromptRef: snapshot.Ref,
				Metadata: map[string]string{
					"prompt_version": snapshot.Version, "prompt_hash": snapshot.PromptHash,
					"prompt_snapshot_ref": snapshot.SnapshotRef, "prompt_content_hash": snapshot.ContentHash,
				},
			},
			Input: []Message{{Role: "user", Content: "帮我规划行程"}},
		},
		Capabilities: RuntimeCapabilities{Streaming: true},
		Trace:        observability.TraceContext{TraceID: "trace_prompt"},
	}
}

func promptTestSnapshot(ref, version, snapshotRef, promptHash, content string) SystemPromptSnapshot {
	return SystemPromptSnapshot{
		Ref: ref, Version: version, SnapshotRef: snapshotRef,
		ContentHash: systemPromptContentHash(content),
		PromptHash:  promptHash, Content: content,
	}
}

type capturingSystemPromptResolver struct {
	request  SystemPromptResolveRequest
	snapshot SystemPromptSnapshot
}

func (r *capturingSystemPromptResolver) ResolveSystemPrompt(_ context.Context, req SystemPromptResolveRequest) (SystemPromptSnapshot, error) {
	r.request = req
	return r.snapshot, nil
}

type failingSystemPromptResolver struct{ err error }

func (r failingSystemPromptResolver) ResolveSystemPrompt(context.Context, SystemPromptResolveRequest) (SystemPromptSnapshot, error) {
	return SystemPromptSnapshot{}, r.err
}

type mapSystemPromptResolver struct {
	snapshots map[string]SystemPromptSnapshot
}

func (r mapSystemPromptResolver) ResolveSystemPrompt(_ context.Context, req SystemPromptResolveRequest) (SystemPromptSnapshot, error) {
	snapshot, ok := r.snapshots[req.Ref]
	if !ok {
		return SystemPromptSnapshot{}, errors.New("prompt missing")
	}
	return snapshot, nil
}
