package agentruntime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

type semanticSummaryTestStrategy struct{}

func (semanticSummaryTestStrategy) Name() string { return "semantic_summary_test" }

func (semanticSummaryTestStrategy) CompactBeforeModel(_ context.Context, req RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error) {
	result := req.Request
	result.Messages = []ModelCallMessage{
		req.Request.Messages[0],
		{Role: "assistant", Content: "summary of prior conversation"},
		req.Request.Messages[len(req.Request.Messages)-1],
	}
	return RuntimePreModelCompactionResult{
		Request: result, SummaryRefs: []string{"artifact://summary/1"}, TrimRecordRef: "artifact://trim/1",
	}, nil
}

func TestDefaultContextCompactorPreservesMCPAndSkillCapabilitySnapshots(t *testing.T) {
	pkg := directTestPackage("run_capabilities", "pkg_capabilities")
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 500
	pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
	pkg.Capabilities.ToolDefinitions = []ModelToolDefinition{{Name: "search", Schema: []byte(`{"type":"object"}`)}}
	pkg.Capabilities.MCPSnapshots = []mcp.CapabilitySnapshot{{ID: "mcp_snapshot", ServerID: "server", Tools: []mcp.Tool{{Name: "search"}}}}
	pkg.Capabilities.SkillSnapshots = []skill.Snapshot{{ID: "skill_snapshot", SkillID: "travel", Version: "v1", EstimatedTokens: 20}}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	originalCapabilities := pkg.Capabilities
	request := ModelInvokeRequest{Package: pkg, Messages: []ModelCallMessage{
		{Role: "system", Content: "system"},
		{Role: "user", Content: strings.Repeat("old question ", 200)},
		{Role: "assistant", Content: strings.Repeat("old answer ", 200)},
		{Role: "user", Content: "recent"},
		{Role: "assistant", Content: "recent answer"},
		{Role: "user", Content: "current"},
	}, Tools: cloneModelToolDefinitions(pkg.Capabilities.ToolDefinitions)}
	manifest, err := BuildPreserveManifest(request)
	if err != nil {
		t.Fatal(err)
	}
	compactor, err := NewDefaultContextCompactorProvider(nil).Resolve(context.Background(), pkg.RuntimeConstraints.CompactionPolicy, RuntimeDescriptor{Name: RuntimeTypeNative})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compactor.CompactBeforeModel(context.Background(), RuntimePreModelCompactionRequest{Request: request, Policy: pkg.RuntimeConstraints.CompactionPolicy, PreserveManifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Messages) >= len(request.Messages) || len(result.SummaryRefs) != 1 {
		t.Fatalf("default compaction did not run: %#v", result)
	}
	if !reflect.DeepEqual(result.Request.Package.Capabilities, originalCapabilities) || !reflect.DeepEqual(result.Request.Tools, request.Tools) {
		t.Fatalf("context compaction changed MCP/Skill/tool capabilities")
	}
}

func TestRuntimeContextAssemblerCompactsBeforeBuilderDropsOldHistory(t *testing.T) {
	req := testRunRequest()
	req.Input = []Message{{ID: "current", Role: "user", Content: "current question"}}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref: "ctxsnap_compaction", ContentHash: "sha256:compaction", LastSequence: 4,
		Messages: []contextpkg.Message{
			{ID: "old_user", Sequence: 1, Role: contextpkg.RoleUser, Content: strings.Repeat("old question ", 200)},
			{ID: "old_assistant", Sequence: 2, Role: contextpkg.RoleAssistant, Content: strings.Repeat("old answer ", 200)},
			{ID: "recent_user", Sequence: 3, Role: contextpkg.RoleUser, Content: "recent question"},
			{ID: "recent_assistant", Sequence: 4, Role: contextpkg.RoleAssistant, Content: "recent answer"},
		},
	}}
	assembler.TokenBudget.Limit = 500
	pkg, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if err != nil {
		t.Fatal(err)
	}
	var foundSummary, foundCurrent bool
	for _, message := range pkg.Messages.ConversationWindow {
		foundSummary = foundSummary || strings.Contains(message.Content, "Conversation summary")
		foundCurrent = foundCurrent || message.Content == "current question"
	}
	if !foundSummary || !foundCurrent {
		t.Fatalf("stage A compaction did not preserve summary/current input: %#v", pkg.Messages.ConversationWindow)
	}
	if len(pkg.Messages.CompactionRecords) == 0 || pkg.Messages.CompactionRecords[0].Phase != ContextCompactionPhaseAssembly ||
		!strings.HasPrefix(pkg.Messages.CompactionRecords[0].RecordRef, "context-trim-inline://sha256:") || pkg.Messages.TrimmedCount == 0 {
		t.Fatalf("stage A compaction provenance is incomplete: %#v", pkg.Messages)
	}
	if err := validateModelContextIntegrity(pkg); err != nil {
		t.Fatalf("compacted package integrity: %v", err)
	}
}

func TestRuntimeContextAssemblerFailsClosedForOversizedCurrentInput(t *testing.T) {
	req := testRunRequest()
	req.Input = []Message{{Role: "user", Content: strings.Repeat("current input ", 200)}}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.TokenBudget.Limit = 100
	_, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if !errors.Is(err, contextpkg.ErrBudgetExceeded) {
		t.Fatalf("oversized current input must fail closed: %v", err)
	}
	classified := DefaultRuntimeErrorClassifier{}.Classify(err, ErrorStageModelContext)
	if classified.Type != ErrorContextOverflow || classified.Code != "MODEL_CONTEXT_BUDGET_EXCEEDED" {
		t.Fatalf("wrong context overflow classification: %#v", classified)
	}
}

func TestRequiredSummaryFailsBeforeDestructiveFallbackWhenNoOldTurnIsRemovable(t *testing.T) {
	policy := DefaultContextCompactionPolicy()
	policy.PolicyHash = ""
	policy.SemanticSummary = SemanticSummaryRequired
	policy, err := NormalizeContextCompactionPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	req := testRunRequest()
	req.Definition.ContextCompaction = policy
	req.Input = []Message{{ID: "current", Role: "user", Content: strings.Repeat("current protected turn ", 40)}}
	assembler := NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = fixedContextSnapshotProvider{snapshot: ContextSnapshot{
		Ref: "ctxsnap_required", ContentHash: "sha256:required", LastSequence: 2,
		Messages: []contextpkg.Message{
			{ID: "first_user", Sequence: 1, Role: contextpkg.RoleUser, Content: strings.Repeat("first protected turn ", 40)},
			{ID: "first_assistant", Sequence: 2, Role: contextpkg.RoleAssistant, Content: strings.Repeat("protected response ", 40)},
		},
	}}
	assembler.TokenBudget.Limit = 100
	if _, err := assembler.Build(context.Background(), RuntimeContextAssemblyRequest{Run: req}); !errors.Is(err, ErrContextSummaryRequired) {
		t.Fatalf("stage A required summary error=%v", err)
	}

	pkg := directTestPackage("run_required", "pkg_required")
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 100
	pkg.RuntimeConstraints.CompactionPolicy = policy
	pkg.ContextHash = ComputeModelContextHash(pkg)
	modelReq := ModelInvokeRequest{Package: pkg, Messages: []ModelCallMessage{
		{Role: "system", Content: "required"},
		{Role: "user", Content: strings.Repeat("only protected turn ", 40)},
	}}
	manifest, err := BuildPreserveManifest(modelReq)
	if err != nil {
		t.Fatal(err)
	}
	compactor, err := NewDefaultContextCompactorProvider(nil).Resolve(context.Background(), policy, RuntimeDescriptor{Name: RuntimeTypeNative})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compactor.CompactBeforeModel(context.Background(), RuntimePreModelCompactionRequest{Request: modelReq, Policy: policy, PreserveManifest: manifest}); !errors.Is(err, ErrContextSummaryRequired) {
		t.Fatalf("stage B required summary error=%v", err)
	}
}

func TestContextCompactionServicesShareOneStrategyAcrossAssemblyAndRuntime(t *testing.T) {
	services := NewContextCompactionServices(nil)
	assembler := NewDefaultRuntimeContextAssembler(nil)
	services.ConfigureAssembler(assembler)
	environment := services.ConfigureEnvironment(RuntimeEnvironment{})
	provider, ok := environment.ContextCompactors.(*DefaultContextCompactorProvider)
	if !ok || assembler.ConversationCompactor != services.Conversation || provider.Compactor != services.Conversation {
		t.Fatalf("compaction services were not shared: assembler=%p provider=%p expected=%p", assembler.ConversationCompactor, provider.Compactor, services.Conversation)
	}
}

func TestContextCompactionPolicyBindingRejectsPackageDrift(t *testing.T) {
	req := directTestRunRequest("run_policy_drift")
	pkg := directTestPackage(req.RunID, "pkg_policy_drift")
	pkg.Run.SessionID = req.SessionID
	changed := DefaultContextCompactionPolicy()
	changed.PolicyHash = ""
	changed.TargetRatio = 0.60
	changed, err := NormalizeContextCompactionPolicy(changed)
	if err != nil {
		t.Fatal(err)
	}
	pkg.RuntimeConstraints.CompactionPolicy = changed
	pkg.ContextHash = ComputeModelContextHash(pkg)
	if err := validateDirectPackage(req, pkg); !errors.Is(err, ErrDirectPackageMismatch) || !strings.Contains(err.Error(), ErrContextCompactionPolicyDrift.Error()) {
		t.Fatalf("policy drift error=%v", err)
	}
}

func TestRuntimePreModelCompletionCannotClaimAnotherPolicy(t *testing.T) {
	pkg := directTestPackage("run_policy_claim", "pkg_policy_claim")
	pkg.Run.RuntimeMode = string(RuntimeModeDirect)
	pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
	pkg.ContextHash = ComputeModelContextHash(pkg)
	req := ModelInvokeRequest{
		Package: pkg, Messages: []ModelCallMessage{{Role: "user", Content: "current"}},
		PreModelCompaction: ModelPreModelCompaction{Completed: true, PolicyHash: "sha256:other"},
	}
	_, err := prepareRuntimePreModelRequest(context.Background(), req, nil)
	if !errors.Is(err, ErrContextCompactionPolicyDrift) {
		t.Fatalf("claimed policy drift error=%v", err)
	}
}

type unsafePreModelTestStrategy struct{}

func (unsafePreModelTestStrategy) Name() string { return "unsafe" }

func (unsafePreModelTestStrategy) CompactBeforeModel(_ context.Context, req RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error) {
	result := req.Request
	result.Messages = []ModelCallMessage{{Role: "assistant", Content: "lost everything"}}
	return RuntimePreModelCompactionResult{Request: result}, nil
}

func TestContextCompactionPolicyDefaultsAndRejectsTampering(t *testing.T) {
	first, err := NormalizeContextCompactionPolicy(ContextCompactionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NormalizeContextCompactionPolicy(ContextCompactionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if first.PolicyHash == "" || first.PolicyHash != second.PolicyHash || first.SemanticSummary != SemanticSummaryAuto {
		t.Fatalf("unstable default policy: first=%#v second=%#v", first, second)
	}
	tampered := first
	tampered.TargetRatio = 0.60
	if _, err := NormalizeContextCompactionPolicy(tampered); !errors.Is(err, ErrContextCompactionPolicyInvalid) {
		t.Fatalf("tampered policy must fail, got %v", err)
	}
}

func TestRuntimeContextAssemblerFreezesDefaultCompactionPolicy(t *testing.T) {
	req := testRunRequest()
	pkg, err := NewDefaultRuntimeContextAssembler(nil).Build(context.Background(), RuntimeContextAssemblyRequest{Run: req})
	if err != nil {
		t.Fatalf("build package: %v", err)
	}
	policy := pkg.RuntimeConstraints.CompactionPolicy
	if policy.PolicyHash == "" || policy.SemanticSummary != SemanticSummaryAuto {
		t.Fatalf("default compaction policy was not frozen: %#v", policy)
	}
	if err := validateModelContextIntegrity(pkg); err != nil {
		t.Fatalf("package integrity: %v", err)
	}
}

func TestPreserveManifestProtectsPinnedSemanticsAndAllowsCompleteToolTurnRemoval(t *testing.T) {
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.Instructions.SystemPromptRef = "prompt://system/v1"
	pkg.Security = ModelContextSecurity{TenantID: "tenant_1", UserID: "user_1"}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	original := ModelInvokeRequest{Package: pkg, Messages: []ModelCallMessage{
		{Role: "system", Content: "safety rules"},
		{Role: "user", Content: "old"},
		{Role: "assistant", ToolCalls: []ModelToolCall{{ToolCallID: "tc_1", Name: "search"}}},
		{Role: "tool", ToolCallID: "tc_1", Content: "old result"},
		{Role: "user", Content: "current"},
	}}
	manifest, err := BuildPreserveManifest(original)
	if err != nil {
		t.Fatal(err)
	}
	completeTurnRemoved := original
	completeTurnRemoved.Messages = []ModelCallMessage{original.Messages[0], original.Messages[4]}
	if err := ValidatePreserveManifest(manifest, completeTurnRemoved); err != nil {
		t.Fatalf("complete old tool turn should be removable: %v", err)
	}

	tests := []struct {
		name string
		edit func(ModelInvokeRequest) ModelInvokeRequest
	}{
		{"system_removed", func(req ModelInvokeRequest) ModelInvokeRequest { req.Messages = req.Messages[1:]; return req }},
		{"current_user_changed", func(req ModelInvokeRequest) ModelInvokeRequest {
			req.Messages[len(req.Messages)-1].Content = "changed"
			return req
		}},
		{"tool_pair_split", func(req ModelInvokeRequest) ModelInvokeRequest {
			req.Messages = append(req.Messages[:3], req.Messages[4:]...)
			return req
		}},
		{"security_changed", func(req ModelInvokeRequest) ModelInvokeRequest { req.Package.Security.UserID = "other"; return req }},
		{"budget_changed", func(req ModelInvokeRequest) ModelInvokeRequest {
			req.Package.RuntimeConstraints.TokenBudget.MaxInputTokens++
			return req
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := original
			input.Messages = cloneModelCallMessages(original.Messages)
			if err := ValidatePreserveManifest(manifest, tt.edit(input)); !errors.Is(err, ErrPreserveManifestInvalid) {
				t.Fatalf("expected preserve failure, got %v", err)
			}
		})
	}
}

func TestPortablePreModelCompactorRunsSemanticStrategyBeforeGovernor(t *testing.T) {
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 80
	pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
	pkg.ContextHash = ComputeModelContextHash(pkg)
	req := ModelInvokeRequest{Package: pkg, Messages: []ModelCallMessage{
		{Role: "system", Content: "required"},
		{Role: "user", Content: "a very long old question that should be summarized"},
		{Role: "assistant", Content: "a very long old answer that should be summarized"},
		{Role: "user", Content: "current"},
	}}
	manifest, err := BuildPreserveManifest(req)
	if err != nil {
		t.Fatal(err)
	}
	compactor := &DefaultRuntimePreModelCompactor{
		Counter: deterministicModelInputCounter{}, Strategies: []RuntimePreModelCompactionStrategy{semanticSummaryTestStrategy{}},
	}
	result, err := compactor.CompactBeforeModel(context.Background(), RuntimePreModelCompactionRequest{
		Request: req, Policy: pkg.RuntimeConstraints.CompactionPolicy, PreserveManifest: manifest,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(result.Request.Messages) != 3 || result.Request.Messages[1].Content != "summary of prior conversation" || result.TrimRecordRef == "" {
		t.Fatalf("semantic result not applied: %#v", result)
	}
	if err := ValidatePreserveManifest(manifest, result.Request); err != nil {
		t.Fatalf("semantic compaction broke preserve contract: %v", err)
	}
}

func TestPortablePreModelCompactorRejectsUnsafeStrategy(t *testing.T) {
	pkg := directTestPackage("run_1", "pkg_1")
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 10
	pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
	pkg.ContextHash = ComputeModelContextHash(pkg)
	req := ModelInvokeRequest{Package: pkg, Messages: []ModelCallMessage{{Role: "system", Content: "required"}, {Role: "user", Content: "current"}}}
	manifest, err := BuildPreserveManifest(req)
	if err != nil {
		t.Fatal(err)
	}
	compactor := &DefaultRuntimePreModelCompactor{Counter: deterministicModelInputCounter{}, Strategies: []RuntimePreModelCompactionStrategy{unsafePreModelTestStrategy{}}}
	_, err = compactor.CompactBeforeModel(context.Background(), RuntimePreModelCompactionRequest{Request: req, Policy: pkg.RuntimeConstraints.CompactionPolicy, PreserveManifest: manifest})
	if !errors.Is(err, ErrPreserveManifestInvalid) {
		t.Fatalf("unsafe strategy error=%v", err)
	}
}

func TestPortablePreModelCompactorIsConcurrentAndRequestIsolated(t *testing.T) {
	compactor := &DefaultRuntimePreModelCompactor{Counter: deterministicModelInputCounter{}}
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pkg := directTestPackage("run_concurrent", "pkg_concurrent")
			pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
			pkg.ContextHash = ComputeModelContextHash(pkg)
			req := ModelInvokeRequest{Package: pkg, Messages: []ModelCallMessage{{Role: "user", Content: "current"}}}
			manifest, err := BuildPreserveManifest(req)
			if err == nil {
				_, err = compactor.CompactBeforeModel(context.Background(), RuntimePreModelCompactionRequest{Request: req, Policy: pkg.RuntimeConstraints.CompactionPolicy, PreserveManifest: manifest})
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
