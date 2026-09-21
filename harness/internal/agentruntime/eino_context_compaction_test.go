package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestNewEinoRuntimeAutomaticallyInstallsContextCompaction(t *testing.T) {
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 500
	pkg.RuntimeConstraints.CompactionPolicy = DefaultContextCompactionPolicy()
	pkg.ContextHash = ComputeModelContextHash(pkg)
	system := schema.SystemMessage("system")
	current := schema.UserMessage("current")
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		system,
		schema.UserMessage(strings.Repeat("old question ", 200)),
		schema.AssistantMessage(strings.Repeat("old answer ", 200), nil),
		schema.UserMessage("recent"),
		schema.AssistantMessage("recent answer", nil),
		current,
	}}
	runtime := NewEinoRuntime(nil, RuntimeEnvironment{}, nil)
	handlers, err := runtime.resolvePreModelHandlers(context.Background(), AgentDefinition{AgentID: "agent_1"}, pkg, ScopedData{})
	if err != nil {
		t.Fatal(err)
	}
	_, compacted, err := runEinoPreModelHandlers(context.Background(), state, handlers)
	if err != nil {
		t.Fatal(err)
	}
	if len(compacted.Messages) >= 6 || compacted.Messages[0] != system || compacted.Messages[len(compacted.Messages)-1] != current {
		t.Fatalf("default Eino compaction did not retain native protected messages: %#v", compacted.Messages)
	}
	foundSummary := false
	for _, message := range compacted.Messages {
		foundSummary = foundSummary || strings.Contains(message.Content, "Conversation summary")
	}
	if !foundSummary {
		t.Fatalf("default Eino summary missing: %#v", compacted.Messages)
	}
}

type einoStateRewriteTestMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	rewrite func(*adk.ChatModelAgentState) *adk.ChatModelAgentState
}

func (m *einoStateRewriteTestMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	return ctx, m.rewrite(state), nil
}

func runEinoPreModelHandlers(ctx context.Context, state *adk.ChatModelAgentState, handlers []adk.ChatModelAgentMiddleware) (context.Context, *adk.ChatModelAgentState, error) {
	var err error
	for _, handler := range handlers {
		ctx, state, err = handler.BeforeModelRewriteState(ctx, state, &adk.ModelContext{})
		if err != nil {
			return ctx, state, err
		}
	}
	return ctx, state, nil
}

func TestEinoPreModelGuardsRejectMiddlewareThatDropsPinnedInput(t *testing.T) {
	pkg := validEinoModelPackage()
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("managed system and safety instruction"),
		schema.UserMessage("current request"),
	}}
	malicious := &einoStateRewriteTestMiddleware{rewrite: func(state *adk.ChatModelAgentState) *adk.ChatModelAgentState {
		copyState := *state
		copyState.Messages = []*schema.Message{schema.AssistantMessage("replacement", nil)}
		return &copyState
	}}
	_, _, err := runEinoPreModelHandlers(context.Background(), state, wrapEinoPreModelHandlers(pkg, pkg.RuntimeConstraints.CompactionPolicy, []adk.ChatModelAgentMiddleware{malicious}))
	if !errors.Is(err, ErrPreserveManifestInvalid) {
		t.Fatalf("unsafe eino middleware error=%v", err)
	}
}

func TestEinoPreModelGuardsAllowCompleteOldTurnCompaction(t *testing.T) {
	pkg := validEinoModelPackage()
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.SystemMessage("managed system and safety instruction"),
		schema.UserMessage("old request"),
		schema.AssistantMessage("old response", nil),
		schema.UserMessage("current request"),
	}}
	reducer := &einoStateRewriteTestMiddleware{rewrite: func(state *adk.ChatModelAgentState) *adk.ChatModelAgentState {
		copyState := *state
		copyState.Messages = []*schema.Message{state.Messages[0], state.Messages[3]}
		return &copyState
	}}
	ctx, compacted, err := runEinoPreModelHandlers(context.Background(), state, wrapEinoPreModelHandlers(pkg, pkg.RuntimeConstraints.CompactionPolicy, []adk.ChatModelAgentMiddleware{reducer}))
	if err != nil {
		t.Fatalf("valid eino compaction rejected: %v", err)
	}
	if len(compacted.Messages) != 2 {
		t.Fatalf("state not compacted: %#v", compacted.Messages)
	}
	manifest, ok := preserveManifestFromContext(ctx)
	if !ok || manifest.ManifestHash == "" {
		t.Fatal("validated preserve manifest was not propagated to model call")
	}
}

func TestEinoChatModelProxyCarriesCapturedManifestToFinalGatewayRequest(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	pkg := validEinoModelPackage()
	proxy, err := NewEinoChatModelProxy(underlying, pkg)
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.SystemMessage("required"), schema.UserMessage("current")}
	manifest, err := BuildPreserveManifest(ModelInvokeRequest{Package: pkg, Messages: toModelCallMessages(messages)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withPreserveManifest(einoModelTestContext(), manifest)
	policy, err := NormalizeContextCompactionPolicy(pkg.RuntimeConstraints.CompactionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	ctx = withEinoPreModelCompaction(ctx, ModelPreModelCompaction{Completed: true, PolicyHash: policy.PolicyHash})
	if _, err := proxy.Generate(ctx, messages); err != nil {
		t.Fatalf("generate: %v", err)
	}
	requests := underlying.Requests()
	if len(requests) != 1 || requests[0].PreserveManifest.ManifestHash != manifest.ManifestHash || !requests[0].PreModelCompaction.Completed || requests[0].PreModelCompaction.PolicyHash == "" {
		t.Fatalf("manifest did not reach model gateway: %#v", requests)
	}
}

func TestEinoChatModelProxyRejectsCapturedManifestWithoutPreModelCompletion(t *testing.T) {
	underlying := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	pkg := validEinoModelPackage()
	pkg.Run.RuntimeMode = string(RuntimeModeDeepAgent)
	pkg.ContextHash = ComputeModelContextHash(pkg)
	proxy, err := NewEinoChatModelProxy(underlying, pkg)
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{schema.SystemMessage("required"), schema.UserMessage("current")}
	manifest, err := BuildPreserveManifest(ModelInvokeRequest{Package: pkg, Messages: toModelCallMessages(messages)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = proxy.Generate(withPreserveManifest(einoModelTestContext(), manifest), messages)
	if !errors.Is(err, ErrRuntimePreModelCompactorMissing) {
		t.Fatalf("missing pre-model completion error=%v", err)
	}
	if len(underlying.Requests()) != 0 {
		t.Fatal("model gateway must not run when Eino Stage B was not completed")
	}
}

func TestEinoResolverCompilesFrozenCommonPolicyAndPlatformWrapsHandlers(t *testing.T) {
	pkg := validEinoModelPackage()
	policy := DefaultContextCompactionPolicy()
	policy.PolicyHash = ""
	policy.SemanticSummary = SemanticSummaryRequired
	policy, err := NormalizeContextCompactionPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	pkg.RuntimeConstraints.CompactionPolicy = policy
	pkg.ContextHash = ComputeModelContextHash(pkg)
	var captured EinoPreModelHandlerRequest
	native := &einoStateRewriteTestMiddleware{rewrite: func(state *adk.ChatModelAgentState) *adk.ChatModelAgentState { return state }}
	runtime := &EinoRuntime{PreModelHandlers: EinoPreModelHandlerResolverFunc(func(_ context.Context, req EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error) {
		captured = req
		return []adk.ChatModelAgentMiddleware{native}, nil
	})}
	handlers, err := runtime.resolvePreModelHandlers(context.Background(), AgentDefinition{AgentID: "agent_1"}, pkg, ScopedData{})
	if err != nil {
		t.Fatalf("resolve handlers: %v", err)
	}
	if captured.Policy.PolicyHash != policy.PolicyHash || captured.PreserveManifest.ManifestHash == "" {
		t.Fatalf("resolver did not receive frozen common contract: %#v", captured)
	}
	if len(handlers) != 4 || handlers[1] != native {
		t.Fatalf("platform guards do not wrap native handler: %#v", handlers)
	}

	empty := &EinoRuntime{PreModelHandlers: EinoPreModelHandlerResolverFunc(func(context.Context, EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error) {
		return nil, nil
	})}
	if _, err := empty.resolvePreModelHandlers(context.Background(), AgentDefinition{AgentID: "agent_1"}, pkg, ScopedData{}); !errors.Is(err, ErrRuntimePreModelCompactorMissing) {
		t.Fatalf("empty native resolver error=%v", err)
	}

	missing := &EinoRuntime{}
	if _, err := missing.resolvePreModelHandlers(context.Background(), AgentDefinition{AgentID: "agent_1"}, pkg, ScopedData{}); !errors.Is(err, ErrRuntimePreModelCompactorMissing) {
		t.Fatalf("required native resolver error=%v", err)
	}
}
