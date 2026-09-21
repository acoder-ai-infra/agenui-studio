package agentruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

var ErrEinoPreserveManifestMissing = errors.New("eino preserve manifest missing")
var ErrEinoPreModelCompactionMissing = errors.New("eino pre-model compaction did not execute")

type einoPreserveManifestKey struct{}
type einoPreModelCompactionKey struct{}

func withPreserveManifest(ctx context.Context, manifest PreserveManifest) context.Context {
	return context.WithValue(ctx, einoPreserveManifestKey{}, manifest)
}

func preserveManifestFromContext(ctx context.Context) (PreserveManifest, bool) {
	manifest, ok := ctx.Value(einoPreserveManifestKey{}).(PreserveManifest)
	return manifest, ok
}

func withEinoPreModelCompaction(ctx context.Context, compaction ModelPreModelCompaction) context.Context {
	return context.WithValue(ctx, einoPreModelCompactionKey{}, compaction)
}

func einoPreModelCompactionFromContext(ctx context.Context) (ModelPreModelCompaction, bool) {
	compaction, ok := ctx.Value(einoPreModelCompactionKey{}).(ModelPreModelCompaction)
	return compaction, ok
}

// DefaultEinoPreModelHandlerResolver compiles the common Context compactor
// into Eino's native per-model-round middleware slot.
type DefaultEinoPreModelHandlerResolver struct{}

func (DefaultEinoPreModelHandlerResolver) Resolve(ctx context.Context, req EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error) {
	compactor, err := req.Environment.ResolvePreModelCompactor(ctx, req.Policy, RuntimeDescriptor{
		Name: RuntimeTypeEino, RuntimeVersion: EinoRuntimeVersion, AdapterVersion: EinoAdapterVersion,
		Governance: RuntimeGovernanceBridged,
	})
	if err != nil {
		return nil, err
	}
	if compactor == nil {
		return nil, nil
	}
	return []adk.ChatModelAgentMiddleware{&einoContextCompactionMiddleware{pkg: req.Package, policy: req.Policy, compactor: compactor}}, nil
}

type einoContextCompactionMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	pkg       ModelContextPackage
	policy    ContextCompactionPolicy
	compactor RuntimePreModelCompactor
}

func (m *einoContextCompactionMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	manifest, ok := preserveManifestFromContext(ctx)
	if !ok {
		return ctx, state, ErrEinoPreserveManifestMissing
	}
	request, err := einoStateModelRequest(m.pkg, state)
	if err != nil {
		return ctx, state, err
	}
	result, err := m.compactor.CompactBeforeModel(ctx, RuntimePreModelCompactionRequest{Request: request, Policy: m.policy, PreserveManifest: manifest})
	if err != nil {
		return ctx, state, err
	}
	state.Messages = retainEinoMessages(state.Messages, result.Request.Messages)
	compaction := ModelPreModelCompaction{
		Completed: true, PolicyHash: m.policy.PolicyHash,
		AppliedStrategies: append([]string(nil), result.AppliedStrategies...),
		SummaryRefs:       append([]string(nil), result.SummaryRefs...), TrimRecordRef: result.TrimRecordRef,
		Records:        append([]ContextCompactionRecord(nil), result.Records...),
		ObservedTokens: result.ObservedTokens, SoftTriggered: result.SoftTriggered,
	}
	return withEinoPreModelCompaction(ctx, compaction), state, nil
}

func retainEinoMessages(original []*schema.Message, compacted []ModelCallMessage) []*schema.Message {
	buckets := make(map[string][]*schema.Message)
	for _, message := range original {
		if message == nil {
			continue
		}
		converted := toModelCallMessages([]*schema.Message{message})[0]
		key := modelCallMessageFingerprint(converted)
		buckets[key] = append(buckets[key], message)
	}
	result := make([]*schema.Message, 0, len(compacted))
	for _, message := range compacted {
		key := modelCallMessageFingerprint(message)
		if candidates := buckets[key]; len(candidates) > 0 {
			result = append(result, candidates[0])
			buckets[key] = candidates[1:]
			continue
		}
		result = append(result, &schema.Message{Role: schema.RoleType(message.Role), Content: message.Content, Name: message.Name})
	}
	return result
}

type einoPreserveCaptureMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	pkg ModelContextPackage
}

func (m *einoPreserveCaptureMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	req, err := einoStateModelRequest(m.pkg, state)
	if err != nil {
		return ctx, state, err
	}
	manifest, err := BuildPreserveManifest(req)
	if err != nil {
		return ctx, state, err
	}
	return withPreserveManifest(ctx, manifest), state, nil
}

type einoPreserveValidateMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	pkg    ModelContextPackage
	policy ContextCompactionPolicy
}

type einoPreModelCompletionMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	policy ContextCompactionPolicy
}

func (m *einoPreModelCompletionMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if _, ok := einoPreModelCompactionFromContext(ctx); ok {
		return ctx, state, nil
	}
	return withEinoPreModelCompaction(ctx, ModelPreModelCompaction{
		Completed: true, PolicyHash: m.policy.PolicyHash,
		AppliedStrategies: []string{"eino.pre_model_handler_chain"},
	}), state, nil
}

func (m *einoPreserveValidateMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	manifest, ok := preserveManifestFromContext(ctx)
	if !ok {
		return ctx, state, ErrEinoPreserveManifestMissing
	}
	req, err := einoStateModelRequest(m.pkg, state)
	if err != nil {
		return ctx, state, err
	}
	if err := ValidatePreserveManifest(manifest, req); err != nil {
		return ctx, state, fmt.Errorf("eino pre-model preserve validation: %w", err)
	}
	compaction, completed := einoPreModelCompactionFromContext(ctx)
	if !completed {
		return ctx, state, ErrEinoPreModelCompactionMissing
	}
	if compaction.PolicyHash != m.policy.PolicyHash {
		return ctx, state, fmt.Errorf("%w: policy hash mismatch", ErrEinoPreModelCompactionMissing)
	}
	return withPreserveManifest(ctx, manifest), state, nil
}

// PreCapturePreModelHandler 是标记接口：实现它的 pre-model handler（如
// BeforeModelHook / ToolCallInterceptor 的投影）会被排在 preserve
// capture 之前——逐轮改写是授权行为，改写后的输入才是本轮冻结校验的
// 事实基线；否则合法改写会被 preserve 校验误判为输入篡改（fail closed）。
type PreCapturePreModelHandler interface {
	PreCapturePreModelHandler()
}

func wrapEinoPreModelHandlers(pkg ModelContextPackage, policy ContextCompactionPolicy, handlers []adk.ChatModelAgentMiddleware) []adk.ChatModelAgentMiddleware {
	var preCapture, governed []adk.ChatModelAgentMiddleware
	for _, handler := range handlers {
		if _, ok := handler.(PreCapturePreModelHandler); ok {
			preCapture = append(preCapture, handler)
			continue
		}
		governed = append(governed, handler)
	}
	return wrapEinoPreModelHandlersWithExtensions(pkg, policy, preCapture, governed)
}

// wrapEinoPreModelHandlersWithExtensions 把扩展钩子（BeforeModelHook /
// ToolCallInterceptor 投影）放在 preserve capture 之前：逐轮改写是授权行为，
// 改写后的输入才是本轮冻结校验的事实基线；若放在 capture 之后，合法
// 改写会被 preserve 校验误判为输入篡改（fail closed）。
func wrapEinoPreModelHandlersWithExtensions(pkg ModelContextPackage, policy ContextCompactionPolicy, extensions, handlers []adk.ChatModelAgentMiddleware) []adk.ChatModelAgentMiddleware {
	wrapped := make([]adk.ChatModelAgentMiddleware, 0, len(extensions)+len(handlers)+3)
	wrapped = append(wrapped, extensions...)
	wrapped = append(wrapped, &einoPreserveCaptureMiddleware{pkg: pkg})
	wrapped = append(wrapped, handlers...)
	wrapped = append(wrapped, &einoPreModelCompletionMiddleware{policy: policy})
	wrapped = append(wrapped, &einoPreserveValidateMiddleware{pkg: pkg, policy: policy})
	return wrapped
}

func einoStateModelRequest(pkg ModelContextPackage, state *adk.ChatModelAgentState) (ModelInvokeRequest, error) {
	if state == nil {
		return ModelInvokeRequest{}, errors.New("eino pre-model state is nil")
	}
	tools, err := frozenModelToolDefinitions(pkg, state.ToolInfos)
	if err != nil {
		return ModelInvokeRequest{}, err
	}
	return ModelInvokeRequest{
		Package:  pkg,
		Messages: toModelCallMessages(state.Messages),
		Tools:    tools,
	}, nil
}
