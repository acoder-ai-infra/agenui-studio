package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// ModelGateway 是 LLM 调用的统一入口。方法以 ctx 为首参,作用域来自 TraceContext。
type ModelGateway interface {
	Chat(ctx context.Context, req ModelRequest) (*ModelCall, error)
	Embedding(ctx context.Context, req EmbeddingRequest) (*EmbeddingResponse, error)
	Rerank(ctx context.Context, req RerankRequest) (*RerankResponse, error)
	Judge(ctx context.Context, req JudgeRequest) (*JudgeResponse, error)
}

// ModelCall 是一次模型调用的句柄:归一化 model_* 事件流(Sequence=0)+ 终态响应。
// 调用方负责把事件 AppendEvent 落库(Phase 1 storage 分配 sequence)。
type ModelCall struct {
	Events <-chan observability.AgentEvent
	Await  func() (*ModelResponse, error)
}

// Facade 是精简后的 ModelGateway 实现。
type Facade struct {
	Router ModelRouter
	// Providers 是按 provider 名索引的多下游注册表。约束(B):target.Provider
	// 必须在此注册,否则该 attempt 失败(可回退到下一 target)。
	Providers   map[string]ChatProvider
	Usage       UsageCostCollector
	Classifier  ErrorClassifier
	Diagnostics InputDiagnostics
	Capability  CapabilityRegistry
	Budget      TokenBudgetChecker
	Cache       ModelCache
	Quota       TenantQuotaManager
	UsageLedger storage.ModelUsageStore
	Hooks       ModelHooks
	Output      OutputArtifactWriter
	Logger      observability.StructuredLogger
	IDs         observability.IDGenerator
	EventBuffer int // ModelCall.Events 缓冲,默认 64
}

const quotaCommitTimeout = 5 * time.Second

var _ ModelGateway = (*Facade)(nil)

// Chat 异步跑调用流水线,返回 ModelCall 句柄。
func (f *Facade) Chat(ctx context.Context, req ModelRequest) (*ModelCall, error) {
	if len(f.Providers) == 0 {
		return nil, errors.New("modelgateway: at least one provider is required")
	}
	boundTrace, trustedTrace, hasTrustedTrace, err := bindTrustedTrace(ctx, req.Trace)
	if err != nil {
		return nil, err
	}
	req.Trace = boundTrace
	if hasTrustedTrace && trustedTrace.AgentID != "" {
		if req.AgentID != "" && req.AgentID != trustedTrace.AgentID {
			return nil, fmt.Errorf("modelgateway: request agent_id conflicts with trusted context")
		}
		req.AgentID = trustedTrace.AgentID
	}
	// ModelRequest.RequestID is a model-call identity and is intentionally
	// distinct from the outer runtime TraceContext.RequestID. It is frozen below
	// against hooks, but must not be overwritten by the outer request ID.
	promptCacheAffinityKey := ""
	if hasTrustedTrace {
		promptCacheAffinityKey = DerivePromptCacheAffinityKey(trustedTrace)
	}
	buf := f.EventBuffer
	if buf <= 0 {
		buf = 64
	}
	events := make(chan observability.AgentEvent, buf)
	done := make(chan struct{})
	var resp *ModelResponse
	var rerr error
	go func() {
		defer close(events)
		defer close(done)
		resp, rerr = f.run(ctx, req, promptCacheAffinityKey, events)
	}()
	return &ModelCall{
		Events: events,
		Await: func() (*ModelResponse, error) {
			<-done
			return resp, rerr
		},
	}, nil
}

// resolveModelTarget returns the first configured target serving the named
// model. Agent-declared model names must match an already-configured (provider,
// model) target; the gateway never fabricates a target from a bare name.
func resolveModelTarget(targets []ModelTarget, model string) (ModelTarget, bool) {
	for _, t := range targets {
		if t.Model == model {
			return t, true
		}
	}
	return ModelTarget{}, false
}

func (f *Facade) run(ctx context.Context, req ModelRequest, promptCacheAffinityKey string, events chan<- observability.AgentEvent) (*ModelResponse, error) {
	// Freeze request/accounting identity before model hooks. Hooks may rewrite
	// prompts and options, but cannot redirect tenant/session accounting or the
	// trusted provider cache-affinity key derived by Chat.
	frozenTrace := cloneTraceContext(req.Trace)
	frozenRequestID := req.RequestID
	frozenAgentID := req.AgentID
	route, err := f.Router.Route(ctx, req)
	if err != nil {
		return f.failed(req, ModelTarget{}, 0, false, f.toAdapterErr(ctx, err)), nil
	}
	configured := append([]ModelTarget{route.Primary}, route.Fallback...)
	targets := configured

	// When the agent declares a specific primary model and/or fallback chain,
	// build the ordered target list explicitly from those declarations instead
	// of using the tenant default chain. Agents that declare nothing keep the
	// tenant default (primary + tenant fallback) unchanged.
	if req.ModelHint != "" || len(req.ModelFallbackHints) != 0 {
		// Fail closed on primary mismatch: a declared primary model the gateway
		// cannot serve is an error, never a silent downgrade to the tenant default.
		primary := route.Primary
		if req.ModelHint != "" {
			resolved, ok := resolveModelTarget(configured, req.ModelHint)
			if !ok {
				available := make([]string, 0, len(configured))
				for _, t := range configured {
					available = append(available, t.Model)
				}
				aerr := &AdapterError{
					Message:   fmt.Sprintf("requested model %q is not served by tenant %q (available: %s)", req.ModelHint, req.Trace.TenantID, strings.Join(available, ", ")),
					Class:     ErrorModelUnavailable,
					Retryable: false,
				}
				target := ModelTarget{Model: req.ModelHint}
				f.emitFailure(ctx, events, req, target, 0, aerr, nil)
				return f.failed(req, target, 0, false, aerr), nil
			}
			primary = resolved
		}
		ordered := []ModelTarget{primary}
		seen := map[string]bool{primary.Model: true}
		// Append agent-declared fallbacks in priority order. A fallback the tenant
		// does not serve is skipped (not fatal) so a missing backup never blocks a
		// working primary; the skip is logged for observability.
		for _, name := range req.ModelFallbackHints {
			if name == "" || seen[name] {
				continue
			}
			resolved, ok := resolveModelTarget(configured, name)
			if !ok {
				f.logger().Warn(ctx, "model gateway skipped unavailable fallback model",
					observability.String("request_id", req.RequestID),
					observability.String("agent_id", req.AgentID),
					observability.String("fallback_model", name),
				)
				continue
			}
			ordered = append(ordered, resolved)
			seen[name] = true
		}
		targets = ordered
	}

	var last string
	// committed becomes true once any semantic provider output (text, thought or
	// tool call) has been emitted. Falling back after that boundary would splice
	// two providers into one logical response.
	committed := false
	for attempt := 0; attempt < len(targets); attempt++ {
		target := targets[attempt]
		fallbackApplied := attempt > 0
		attemptReq, err := f.Hooks.RunBefore(ctx, req, target)
		attemptReq.Trace = cloneTraceContext(frozenTrace)
		attemptReq.RequestID = frozenRequestID
		attemptReq.AgentID = frozenAgentID
		if err != nil {
			aerr := f.toAdapterErr(ctx, err)
			last = aerr.Message
			f.emitFailure(ctx, events, attemptReq, target, attempt, aerr, nil)
			if !aerr.Retryable || attempt == len(targets)-1 {
				return f.failed(attemptReq, target, attempt, fallbackApplied, aerr), nil
			}
			f.emitFallback(ctx, events, attemptReq, target, targets[attempt+1], attempt+1, last)
			continue
		}
		diagnostics := f.diagnostics().Build(ctx, attemptReq)
		logDiagnostics(ctx, f.logger(), attemptReq, diagnostics)
		if err := f.capability().Validate(ctx, attemptReq, target); err != nil {
			aerr := f.toAdapterErr(ctx, err)
			last = aerr.Message
			f.emitFailure(ctx, events, attemptReq, target, attempt, aerr, nil)
			if !aerr.Retryable || attempt == len(targets)-1 {
				return f.failed(attemptReq, target, attempt, fallbackApplied, aerr), nil
			}
			f.emitFallback(ctx, events, attemptReq, target, targets[attempt+1], attempt+1, last)
			continue
		}
		if err := f.budget().Check(ctx, attemptReq, target, diagnostics); err != nil {
			aerr := f.toAdapterErr(ctx, err)
			last = aerr.Message
			f.emitFailure(ctx, events, attemptReq, target, attempt, aerr, nil)
			if !aerr.Retryable || attempt == len(targets)-1 {
				return f.failed(attemptReq, target, attempt, fallbackApplied, aerr), nil
			}
			f.emitFallback(ctx, events, attemptReq, target, targets[attempt+1], attempt+1, last)
			continue
		}

		provider, ok := f.Providers[target.Provider]
		var aerr *AdapterError
		var attemptResp *ModelResponse
		if !ok {
			aerr = &AdapterError{Message: fmt.Sprintf("provider %q not registered", target.Provider), Class: ErrorProvider4xx, Retryable: true}
		} else {
			if cached, ok := f.getCache(ctx, attemptReq, target); ok {
				cached.Attempt = attempt
				cached.FallbackApplied = fallbackApplied
				cached.CacheHit = true
				// An application cache hit is not a provider attempt. Replaying the
				// original usage/cost/latency would double bill and pollute TTFT/TPS.
				cached.Usage = ModelUsage{}
				cached.Cost = ModelCost{}
				cached.Latency = ModelLatency{}
				diagnostics.CacheHit = true
				cached.Diagnostics = diagnostics
				f.emitStarted(ctx, events, attemptReq, target, provider.ID(), attempt, diagnostics)
				if cached.Text != "" {
					f.emit(ctx, events, attemptReq, observability.EventModelTokenDelta, ModelTokenDeltaPayload{Text: cached.Text})
				}
				f.logCompleted(ctx, attemptReq, cached)
				f.emitCompleted(ctx, events, attemptReq, cached)
				return &cached, nil
			}
			f.emitStarted(ctx, events, attemptReq, target, provider.ID(), attempt, diagnostics)
			attemptResp, aerr = f.attempt(ctx, attemptReq, target, provider, attempt, fallbackApplied, diagnostics, promptCacheAffinityKey, events, &committed)
			if aerr == nil {
				return attemptResp, nil
			}
		}

		last = aerr.Message
		f.emitFailure(ctx, events, attemptReq, target, attempt, aerr, attemptResp)
		if committed {
			// Semantic output already left this attempt; do not mix providers.
			return withAttemptAccounting(f.failed(attemptReq, target, attempt, fallbackApplied, aerr), attemptResp), nil
		}
		if !aerr.Retryable || attempt == len(targets)-1 {
			return withAttemptAccounting(f.failed(attemptReq, target, attempt, fallbackApplied, aerr), attemptResp), nil
		}
		next := targets[attempt+1]
		f.emitFallback(ctx, events, attemptReq, target, next, attempt+1, last)
	}
	return f.failed(req, ModelTarget{}, 0, false, &AdapterError{Message: last, Class: ErrorUnknown}), nil
}

// attempt 执行一次 provider 往返:invoke -> 流循环归一为 model_* -> 组装输出。
func (f *Facade) attempt(ctx context.Context, req ModelRequest, target ModelTarget, provider ChatProvider, attempt int, fallbackApplied bool, diagnostics GatewayDiagnostics, promptCacheAffinityKey string, events chan<- observability.AgentEvent, committed *bool) (*ModelResponse, *AdapterError) {
	callCtx := ctx
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	reservation, err := f.reserveQuota(callCtx, req)
	if err != nil {
		return nil, f.toAdapterErr(ctx, err)
	}
	defer f.releaseQuota(callCtx, reservation)

	var (
		text               strings.Builder
		reasoningSignature string
		usage              ModelUsage
		usageResult        UsageCostResult
		usageSeen          bool
		attemptSettled     bool
		toolCalls          = map[int]*ModelToolCall{}
		toolOrder          []int
		completedAt        time.Time
	)
	latencyTracker := newModelLatencyTracker(time.Now())
	// Redis quota and the durable Usage Ledger consume the same immutable
	// attempt fact. The record ID (request_id:attempt) makes the ledger write
	// idempotent; the Redis reservation ID gives the quota side the same property.
	settleAttempt := func(resp *ModelResponse) error {
		if attemptSettled {
			return nil
		}
		attemptSettled = true
		f.commitQuota(callCtx, reservation, resp.Usage, resp.Cost)
		return f.recordUsageCleanup(callCtx, req, target, *resp)
	}
	failedAttempt := func(aerr *AdapterError, base *ModelResponse) (*ModelResponse, *AdapterError) {
		if base == nil {
			settlementCtx, cancel := context.WithTimeout(context.WithoutCancel(callCtx), quotaCommitTimeout)
			settlement := f.usage().Collect(settlementCtx, UsageCostRequest{
				ModelRequest: req, Target: target, OutputText: text.String(), Usage: usage, UsagePresent: usageSeen,
			})
			cancel()
			base = &ModelResponse{
				RequestID: req.RequestID, Status: StatusFailed, Provider: target.Provider, Model: target.Model,
				Text: text.String(), Usage: settlement.Usage, Cost: settlement.Cost,
				Latency: latencyTracker.snapshot(settlement.Usage, time.Now()), Diagnostics: diagnostics,
				FallbackApplied: fallbackApplied, Attempt: attempt,
			}
		} else {
			base.Status = StatusFailed
		}
		aerr = withModelLatency(aerr, base.Latency)
		if ledgerErr := settleAttempt(base); ledgerErr != nil {
			f.logger().Error(context.WithoutCancel(callCtx), "model attempt usage ledger write failed", ledgerErr,
				observability.String("request_id", req.RequestID),
				observability.String("provider", target.Provider),
				observability.String("model", target.Model),
				observability.Int("attempt", attempt),
			)
		}
		return base, aerr
	}

	options := cloneModelOptions(req.Options)
	if options.ReasoningMode == ReasoningDisabled && !target.Capability.Reasoning {
		// Disabled is already true for a non-reasoning model. Do not leak a
		// provider-specific thinking toggle to a model that does not support it.
		options.ReasoningMode = ReasoningAuto
		options.ReasoningBudget = 0
	}
	// A5：parallel_tool_calls 由模型能力声明驱动，路由到具体 target 后写入；
	// 请求未显式指定时采用模型声明值（nil 表示不下发该 provider 参数）。
	if options.ParallelToolCalls == nil && target.Capability.ParallelToolCalls != nil {
		value := *target.Capability.ParallelToolCalls
		options.ParallelToolCalls = &value
	}
	stream, err := provider.InvokeChat(callCtx, AdapterRequest{
		Model: target.Model, Messages: req.Messages, ToolsSchema: req.ToolsSchema,
		Streaming: req.Streaming, TimeoutMS: req.TimeoutMS, Options: options,
		PromptCacheAffinityKey: promptCacheAffinityKey,
	})
	if err != nil {
		return failedAttempt(f.toAdapterErr(ctx, err), nil)
	}
	defer stream.Close()

	for {
		chunk, err := stream.Next(callCtx)
		if errors.Is(err, io.EOF) {
			completedAt = time.Now()
			break
		}
		if err != nil {
			return failedAttempt(f.toAdapterErr(ctx, err), nil)
		}
		latencyTracker.observe(chunk, time.Now())
		switch chunk.Kind {
		case ChunkError:
			if chunk.Err != nil {
				return failedAttempt(chunk.Err, nil)
			}
		case ChunkToken:
			if committed != nil {
				*committed = true
			}
			text.WriteString(chunk.TextDelta)
			f.emit(ctx, events, req, observability.EventModelTokenDelta, ModelTokenDeltaPayload{Text: chunk.TextDelta})
		case ChunkThought:
			if chunk.ReasoningSignature != "" {
				reasoningSignature = chunk.ReasoningSignature
			}
			if committed != nil {
				*committed = *committed || chunk.ThoughtDelta != ""
			}
			if chunk.ThoughtDelta != "" {
				f.emit(ctx, events, req, observability.EventModelThoughtDelta, ModelThoughtDeltaPayload{Text: chunk.ThoughtDelta})
			}
		case ChunkToolCall:
			if d := chunk.ToolCallDelta; d != nil {
				if committed != nil {
					*committed = true
				}
				tc, ok := toolCalls[d.Index]
				if !ok {
					tc = &ModelToolCall{}
					toolCalls[d.Index] = tc
					toolOrder = append(toolOrder, d.Index)
				}
				if d.ToolCallID != "" {
					tc.ToolCallID = d.ToolCallID
				}
				if d.Name != "" {
					tc.Name = d.Name
				}
				tc.Arguments += d.ArgumentsDelta
				f.emit(ctx, events, req, observability.EventModelToolCallDelta, ModelToolCallDeltaPayload{
					Index: d.Index, ToolCallID: d.ToolCallID, Name: d.Name, ArgumentsDelta: d.ArgumentsDelta,
				})
			}
		case ChunkUsage:
			if chunk.Usage != nil {
				usage = *chunk.Usage
				usageSeen = true
				usageResult = f.usage().Collect(ctx, UsageCostRequest{
					ModelRequest: req, Target: target, OutputText: text.String(), Usage: usage, UsagePresent: true,
				})
				usage = usageResult.Usage
				f.emit(ctx, events, req, observability.EventModelUsageDelta, ModelUsageDeltaPayload{Usage: usageResult.Usage, Cost: usageResult.Cost})
			}
		case ChunkDone:
		}
	}
	if !usageSeen {
		usageResult = f.usage().Collect(ctx, UsageCostRequest{
			ModelRequest: req, Target: target, OutputText: text.String(), UsagePresent: false,
		})
		usage = usageResult.Usage
	} else {
		usageResult = f.usage().Collect(ctx, UsageCostRequest{
			ModelRequest: req, Target: target, OutputText: text.String(), Usage: usage, UsagePresent: true,
		})
		usage = usageResult.Usage
	}

	resp := &ModelResponse{
		RequestID: req.RequestID, Status: StatusSuccess, Provider: target.Provider, Model: target.Model,
		Text: text.String(), ReasoningSignature: reasoningSignature, Usage: usageResult.Usage, Cost: usageResult.Cost,
		Latency: latencyTracker.snapshot(usageResult.Usage, completedAt), Diagnostics: diagnostics,
		FallbackApplied: fallbackApplied, Attempt: attempt,
	}
	for _, idx := range toolOrder {
		resp.ToolCalls = append(resp.ToolCalls, *toolCalls[idx])
	}
	var hookErr error
	*resp, hookErr = f.Hooks.RunAfter(ctx, req, target, *resp)
	if hookErr != nil {
		return failedAttempt(f.toAdapterErr(ctx, hookErr), resp)
	}
	if ref, err := f.writeOutput(ctx, req, target, resp.Text); err != nil {
		return failedAttempt(f.toAdapterErr(ctx, err), resp)
	} else if ref != "" {
		resp.OutputRef = ref
		resp.Diagnostics.OutputRef = ref
	}
	if err := settleAttempt(resp); err != nil {
		return resp, withModelLatency(f.toAdapterErr(ctx, err), resp.Latency)
	}
	f.logCompleted(ctx, req, *resp)
	f.putCache(ctx, req, target, *resp)
	f.emitCompleted(ctx, events, req, *resp)
	return resp, nil
}

func (f *Facade) logCompleted(ctx context.Context, req ModelRequest, resp ModelResponse) {
	f.logger().Info(ctx, "model gateway call completed",
		observability.String("request_id", resp.RequestID),
		observability.String("agent_id", req.AgentID),
		observability.String("provider", resp.Provider),
		observability.String("model", resp.Model),
		observability.Int("attempt", resp.Attempt),
		observability.Bool("fallback_applied", resp.FallbackApplied),
		observability.Bool("cache_hit", resp.CacheHit),
		observability.String("usage_source", string(resp.Usage.Source)),
		observability.Int("prompt_tokens", resp.Usage.PromptTokens),
		observability.Int("completion_tokens", resp.Usage.CompletionTokens),
		observability.Int("reasoning_tokens", resp.Usage.ReasoningTokens),
		observability.Int("cache_read_tokens", resp.Usage.CacheReadTokens),
		observability.Int("cache_write_tokens", resp.Usage.CacheWriteTokens),
		observability.String("currency", resp.Cost.Currency),
		observability.Any("estimated_cost", resp.Cost.Estimated),
		observability.Bool("cost_available", resp.Cost.Currency != ""),
		observability.Int64("total_latency_ms", resp.Latency.TotalMS),
		observability.Bool("first_token_observed", resp.Latency.FirstTokenObserved),
		observability.Int64("first_token_ms", resp.Latency.FirstTokenMS),
		observability.Int64("generation_duration_ms", resp.Latency.GenerationDurationMS),
		observability.Any("output_tokens_per_second", resp.Latency.OutputTokensPerSecond),
	)
}

func withModelLatency(err *AdapterError, latency ModelLatency) *AdapterError {
	if err != nil {
		err.Latency = latency
	}
	return err
}

func (f *Facade) failed(req ModelRequest, target ModelTarget, attempt int, fallbackApplied bool, err *AdapterError) *ModelResponse {
	classified := f.classifier().Classify(context.Background(), err)
	return &ModelResponse{
		RequestID: req.RequestID, Status: StatusFailed, Provider: target.Provider, Model: target.Model,
		FallbackApplied: fallbackApplied, Attempt: attempt,
		Diagnostics: GatewayDiagnostics{Classification: string(classified.Class)},
		Error:       classified.EventError(), Latency: err.Latency,
	}
}

func withAttemptAccounting(failed, accounting *ModelResponse) *ModelResponse {
	if failed == nil || accounting == nil {
		return failed
	}
	failed.Usage = accounting.Usage
	failed.Cost = accounting.Cost
	failed.Latency = accounting.Latency
	return failed
}

func (f *Facade) emit(ctx context.Context, events chan<- observability.AgentEvent, req ModelRequest, t observability.EventType, payload any) {
	f.emitWithUsage(ctx, events, req, t, payload, nil)
}

func (f *Facade) emitWithUsage(ctx context.Context, events chan<- observability.AgentEvent, req ModelRequest, t observability.EventType, payload any, usage json.RawMessage) {
	ev := observability.AgentEvent{
		EventID:    f.ids().NewEventID(),
		RunID:      req.Trace.RunID,
		SessionID:  req.Trace.SessionID,
		AgentID:    req.AgentID,
		TraceID:    req.Trace.TraceID,
		EventType:  t,
		Visibility: observability.VisibilityDebug,
		Payload:    jsonPayload(payload),
		Usage:      usage,
		CreatedAt:  time.Now(),
	}
	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

func (f *Facade) emitStarted(ctx context.Context, events chan<- observability.AgentEvent, req ModelRequest, target ModelTarget, gateway string, attempt int, diagnostics GatewayDiagnostics) {
	f.emit(ctx, events, req, observability.EventModelCallStarted, ModelCallStartedPayload{
		RequestID: req.RequestID, Model: target.Model, Provider: target.Provider,
		Gateway: gateway, Attempt: attempt, Streaming: req.Streaming, Diagnostics: diagnostics,
	})
}

func (f *Facade) emitCompleted(ctx context.Context, events chan<- observability.AgentEvent, req ModelRequest, resp ModelResponse) {
	f.emitWithUsage(ctx, events, req, observability.EventModelCallCompleted, ModelCallCompletedPayload{
		RequestID: resp.RequestID, Model: resp.Model, Provider: resp.Provider,
		Usage: resp.Usage, Cost: resp.Cost, Latency: resp.Latency, OutputRef: resp.OutputRef, CacheHit: resp.CacheHit, Diagnostics: resp.Diagnostics,
	}, jsonPayload(resp.Usage))
}

func (f *Facade) emitFailure(ctx context.Context, events chan<- observability.AgentEvent, req ModelRequest, target ModelTarget, attempt int, err *AdapterError, accounting *ModelResponse) {
	classified := f.classifier().Classify(context.Background(), err)
	var usage ModelUsage
	var cost ModelCost
	if accounting != nil {
		usage = accounting.Usage
		cost = accounting.Cost
	}
	f.logger().Warn(ctx, "model gateway call failed",
		observability.String("request_id", req.RequestID),
		observability.String("agent_id", req.AgentID),
		observability.String("provider", target.Provider),
		observability.String("model", target.Model),
		observability.Int("attempt", attempt),
		observability.String("error_class", string(classified.Class)),
		observability.Bool("retryable", classified.Retryable),
		observability.Int64("total_latency_ms", err.Latency.TotalMS),
		observability.Bool("first_token_observed", err.Latency.FirstTokenObserved),
		observability.Int64("first_token_ms", err.Latency.FirstTokenMS),
		observability.String("usage_source", string(usage.Source)),
		observability.Int("prompt_tokens", usage.PromptTokens),
		observability.Int("completion_tokens", usage.CompletionTokens),
		observability.Int("reasoning_tokens", usage.ReasoningTokens),
		observability.Int("cache_read_tokens", usage.CacheReadTokens),
		observability.Int("cache_write_tokens", usage.CacheWriteTokens),
		observability.String("currency", cost.Currency),
		observability.Any("estimated_cost", cost.Estimated),
	)
	ev := observability.AgentEvent{
		EventID:    f.ids().NewEventID(),
		RunID:      req.Trace.RunID,
		SessionID:  req.Trace.SessionID,
		AgentID:    req.AgentID,
		TraceID:    req.Trace.TraceID,
		EventType:  observability.EventModelCallFailed,
		Visibility: observability.VisibilityDebug,
		Payload: jsonPayload(ModelCallFailedPayload{
			RequestID: req.RequestID, Model: target.Model, Provider: target.Provider, Attempt: attempt,
			FallbackApplied: accounting != nil && accounting.FallbackApplied,
			Class:           string(classified.Class), Message: classified.Message, Usage: usage, Cost: cost, Latency: err.Latency,
		}),
		Error:     classified.EventError(),
		CreatedAt: time.Now(),
	}
	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

func (f *Facade) emitFallback(ctx context.Context, events chan<- observability.AgentEvent, req ModelRequest, from, to ModelTarget, attempt int, reason string) {
	f.emit(ctx, events, req, observability.EventModelFallbackApplied, ModelFallbackAppliedPayload{
		RequestID: req.RequestID, From: from, To: to, Attempt: attempt, Reason: reason,
	})
}

func (f *Facade) ids() observability.IDGenerator {
	if f.IDs == nil {
		return observability.NewULIDGenerator("")
	}
	return f.IDs
}

func (f *Facade) usage() UsageCostCollector {
	if f.Usage == nil {
		return DefaultUsageCostCollector{}
	}
	return f.Usage
}

func (f *Facade) classifier() ErrorClassifier {
	if f.Classifier == nil {
		return DefaultErrorClassifier{}
	}
	return f.Classifier
}

func (f *Facade) diagnostics() InputDiagnostics {
	if f.Diagnostics == nil {
		return DefaultInputDiagnostics{}
	}
	return f.Diagnostics
}

func (f *Facade) capability() CapabilityRegistry {
	if f.Capability == nil {
		return DefaultCapabilityRegistry{}
	}
	return f.Capability
}

func (f *Facade) budget() TokenBudgetChecker {
	if f.Budget == nil {
		return DefaultTokenBudgetChecker{}
	}
	return f.Budget
}

func (f *Facade) logger() observability.StructuredLogger {
	if f.Logger == nil {
		return observability.NoopLogger{}
	}
	return f.Logger
}

func (f *Facade) getCache(ctx context.Context, req ModelRequest, target ModelTarget) (ModelResponse, bool) {
	if f.Cache == nil || !req.Cache.Enabled {
		return ModelResponse{}, false
	}
	return f.Cache.Get(ctx, cacheKey(req, target))
}

func (f *Facade) putCache(ctx context.Context, req ModelRequest, target ModelTarget, resp ModelResponse) {
	if f.Cache == nil || !req.Cache.Enabled || resp.Status != StatusSuccess {
		return
	}
	f.Cache.Put(ctx, cacheKey(req, target), resp)
}

func (f *Facade) reserveQuota(ctx context.Context, req ModelRequest) (QuotaReservation, error) {
	if f.Quota == nil {
		return QuotaReservation{}, nil
	}
	return f.Quota.Reserve(ctx, req)
}

func (f *Facade) commitQuota(ctx context.Context, res QuotaReservation, usage ModelUsage, cost ModelCost) {
	if f.Quota != nil {
		// Provider completion may coincide with the model-call deadline. Accounting
		// is a bounded cleanup action and must still run after that deadline.
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quotaCommitTimeout)
		defer cancel()
		f.Quota.Commit(commitCtx, res, usage, cost)
	}
}

func (f *Facade) recordUsage(ctx context.Context, req ModelRequest, target ModelTarget, resp ModelResponse) error {
	if f.UsageLedger == nil {
		return nil
	}
	tenant := req.Trace.TenantID
	if tenant == "" {
		tenant = "default"
	}
	return f.UsageLedger.Record(ctx, &storage.ModelUsageRecord{
		ID:                    fmt.Sprintf("%s:%d", resp.RequestID, resp.Attempt),
		RequestID:             resp.RequestID,
		TraceID:               req.Trace.TraceID,
		TenantID:              tenant,
		SessionID:             req.Trace.SessionID,
		RunID:                 req.Trace.RunID,
		AgentID:               req.AgentID,
		Provider:              target.Provider,
		Model:                 target.Model,
		Attempt:               resp.Attempt,
		FallbackApplied:       resp.FallbackApplied,
		PromptTokens:          resp.Usage.PromptTokens,
		CompletionTokens:      resp.Usage.CompletionTokens,
		ReasoningTokens:       resp.Usage.ReasoningTokens,
		CacheReadTokens:       resp.Usage.CacheReadTokens,
		CacheWriteTokens:      resp.Usage.CacheWriteTokens,
		UsageSource:           string(resp.Usage.Source),
		Currency:              resp.Cost.Currency,
		EstimatedCost:         resp.Cost.Estimated,
		TotalLatencyMS:        resp.Latency.TotalMS,
		FirstTokenObserved:    resp.Latency.FirstTokenObserved,
		FirstTokenMS:          resp.Latency.FirstTokenMS,
		GenerationDurationMS:  resp.Latency.GenerationDurationMS,
		OutputTokensPerSecond: resp.Latency.OutputTokensPerSecond,
		CacheHit:              resp.CacheHit,
		OutputRef:             resp.OutputRef,
		CreatedAt:             time.Now(),
	})
}

func (f *Facade) recordUsageCleanup(ctx context.Context, req ModelRequest, target ModelTarget, resp ModelResponse) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quotaCommitTimeout)
	defer cancel()
	return f.recordUsage(cleanupCtx, req, target, resp)
}

func (f *Facade) releaseQuota(ctx context.Context, res QuotaReservation) {
	if f.Quota != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		f.Quota.Release(cleanupCtx, res)
	}
}

func (f *Facade) writeOutput(ctx context.Context, req ModelRequest, target ModelTarget, text string) (string, error) {
	if f.Output == nil {
		return "", nil
	}
	return f.Output.WriteOutput(ctx, req, target, text)
}

func mergeTrace(req, ctx observability.TraceContext) observability.TraceContext {
	if req.TraceID == "" {
		req.TraceID = ctx.TraceID
	}
	if req.SpanID == "" {
		req.SpanID = ctx.SpanID
	}
	if req.RootSpanID == "" {
		req.RootSpanID = ctx.RootSpanID
	}
	if req.ParentSpanID == "" {
		req.ParentSpanID = ctx.ParentSpanID
	}
	if req.SessionID == "" {
		req.SessionID = ctx.SessionID
	}
	if req.RunID == "" {
		req.RunID = ctx.RunID
	}
	if req.ConversationID == "" {
		req.ConversationID = ctx.ConversationID
	}
	if req.RequestID == "" {
		req.RequestID = ctx.RequestID
	}
	if req.UserID == "" {
		req.UserID = ctx.UserID
	}
	if req.TenantID == "" {
		req.TenantID = ctx.TenantID
	}
	if req.AgentID == "" {
		req.AgentID = ctx.AgentID
	}
	if req.AgentType == "" {
		req.AgentType = ctx.AgentType
	}
	if req.AgentVersion == "" {
		req.AgentVersion = ctx.AgentVersion
	}
	if req.Runtime == "" {
		req.Runtime = ctx.Runtime
	}
	if req.Channel == "" {
		req.Channel = ctx.Channel
	}
	if req.Protocol == "" {
		req.Protocol = ctx.Protocol
	}
	if req.Source == "" {
		req.Source = ctx.Source
	}
	if len(req.Baggage) == 0 && len(ctx.Baggage) > 0 {
		req.Baggage = cloneStringMap(ctx.Baggage)
	} else if len(req.Baggage) > 0 {
		req.Baggage = cloneStringMap(req.Baggage)
	}
	return req
}

func cloneTraceContext(trace observability.TraceContext) observability.TraceContext {
	trace.Baggage = cloneStringMap(trace.Baggage)
	return trace
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

// bindTrustedTrace returns an immutable request trace rooted in the Harness
// context. Request fields may fill context omissions for backwards-compatible
// diagnostics, but cannot override non-empty trusted identity fields. The
// trusted return value is kept separate so provider affinity is never derived
// from caller-supplied ModelRequest.Trace values.
func bindTrustedTrace(ctx context.Context, request observability.TraceContext) (bound, trusted observability.TraceContext, hasTrusted bool, err error) {
	trusted, hasTrusted = observability.TraceContextFrom(ctx)
	if !hasTrusted {
		return request, observability.TraceContext{}, false, nil
	}
	if field := conflictingTraceIdentityField(request, trusted); field != "" {
		return observability.TraceContext{}, trusted, true, fmt.Errorf("modelgateway: request trace conflicts with trusted context field %s", field)
	}
	// mergeTrace preserves non-empty values from its first argument, so putting
	// trusted first makes context identity authoritative while retaining harmless
	// request-only detail when the runtime did not bind it.
	return mergeTrace(trusted, request), trusted, true, nil
}

func conflictingTraceIdentityField(request, trusted observability.TraceContext) string {
	fields := []struct {
		name             string
		request, trusted string
	}{
		{name: "trace_id", request: request.TraceID, trusted: trusted.TraceID},
		{name: "request_id", request: request.RequestID, trusted: trusted.RequestID},
		{name: "tenant_id", request: request.TenantID, trusted: trusted.TenantID},
		{name: "user_id", request: request.UserID, trusted: trusted.UserID},
		{name: "session_id", request: request.SessionID, trusted: trusted.SessionID},
		{name: "run_id", request: request.RunID, trusted: trusted.RunID},
		{name: "conversation_id", request: request.ConversationID, trusted: trusted.ConversationID},
		{name: "agent_id", request: request.AgentID, trusted: trusted.AgentID},
	}
	for _, field := range fields {
		if field.request != "" && field.trusted != "" && field.request != field.trusted {
			return field.name
		}
	}
	return ""
}

// toAdapterErr 归一错误:已是 *AdapterError 直接用,否则包成可重试错误(网络/超时类)。
func (f *Facade) toAdapterErr(ctx context.Context, err error) *AdapterError {
	var ae *AdapterError
	if errors.As(err, &ae) {
		if ae.Class == "" {
			classified := f.classifier().Classify(ctx, ae)
			ae.Class = classified.Class
			ae.HTTPStatus = classified.HTTPStatus
		}
		return ae
	}
	classified := f.classifier().Classify(ctx, err)
	return &AdapterError{Message: classified.Message, Class: classified.Class, Retryable: classified.Retryable, HTTPStatus: classified.HTTPStatus}
}
