package modelgateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	storagemem "github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
)

func collect(t *testing.T, gw *mg.Facade, req mg.ModelRequest) ([]observability.EventType, *mg.ModelResponse) {
	t.Helper()
	call, err := gw.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var types []observability.EventType
	for ev := range call.Events {
		if ev.Sequence != 0 {
			t.Fatalf("gateway must not allocate sequence, got %d for %s", ev.Sequence, ev.EventType)
		}
		types = append(types, ev.EventType)
	}
	resp, rerr := call.Await()
	if rerr != nil {
		t.Fatalf("Await: %v", rerr)
	}
	return types, resp
}

func collectEvents(t *testing.T, gw *mg.Facade, req mg.ModelRequest) ([]observability.AgentEvent, *mg.ModelResponse) {
	t.Helper()
	call, err := gw.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var events []observability.AgentEvent
	for ev := range call.Events {
		events = append(events, ev)
	}
	resp, rerr := call.Await()
	if rerr != nil {
		t.Fatalf("Await: %v", rerr)
	}
	return events, resp
}

func has(types []observability.EventType, want observability.EventType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func testArtifactStore() artifact.ArtifactStore {
	return artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objmem.NewMemory(),
		MetadataStore: metamem.NewMemory(),
	})
}

type recordingChatProvider struct {
	request  mg.AdapterRequest
	delegate mg.ChatProvider
}

func (p *recordingChatProvider) ID() string { return p.delegate.ID() }

func (p *recordingChatProvider) InvokeChat(ctx context.Context, req mg.AdapterRequest) (mg.AdapterStream, error) {
	p.request = req
	return p.delegate.InvokeChat(ctx, req)
}

func TestFacadeValidatesDynamicModelOptionsBeforeProviderCall(t *testing.T) {
	negative, aboveOne, zero, tooLarge := -0.1, 1.1, 0, 513
	tests := []struct {
		name    string
		options mg.ModelOptions
	}{
		{name: "negative temperature", options: mg.ModelOptions{Temperature: &negative}},
		{name: "top p above one", options: mg.ModelOptions{TopP: &aboveOne}},
		{name: "zero max tokens", options: mg.ModelOptions{MaxTokens: &zero}},
		{name: "reserved output exceeded", options: mg.ModelOptions{MaxTokens: &tooLarge}},
		{name: "empty stop", options: mg.ModelOptions{Stop: []string{""}}},
		{name: "invalid tool choice", options: mg.ModelOptions{ToolChoice: "sometimes"}},
		{name: "invalid reasoning mode", options: mg.ModelOptions{ReasoningMode: "deep"}},
		{name: "invalid reasoning effort", options: mg.ModelOptions{ReasoningMode: mg.ReasoningEnabled, ReasoningEffort: "extreme"}},
		{name: "reasoning effort without enabled mode", options: mg.ModelOptions{ReasoningMode: mg.ReasoningAuto, ReasoningEffort: "low"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &recordingChatProvider{delegate: mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("unexpected")}})}
			gw := &mg.Facade{
				Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "m", Provider: "mock", Capability: mg.ModelCapability{Model: "m", Provider: "mock", Chat: true}}}, nil),
				Providers: map[string]mg.ChatProvider{"mock": provider},
			}
			_, response := collect(t, gw, mg.ModelRequest{RequestID: "invalid-options", Options: test.options, MaxOutputTokens: 512})
			if response.Status != mg.StatusFailed || provider.request.Model != "" {
				t.Fatalf("invalid dynamic option reached provider: response=%#v request=%#v", response, provider.request)
			}
		})
	}
}

func TestFacadeDoesNotSendThinkingToggleToNonReasoningModel(t *testing.T) {
	provider := &recordingChatProvider{delegate: mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok"), mock.UsageChunk(1, 1)}})}
	gw := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "plain", Provider: "mock", Capability: mg.ModelCapability{Model: "plain", Provider: "mock", Chat: true}}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": provider},
	}
	_, response := collect(t, gw, mg.ModelRequest{RequestID: "plain-model", Options: mg.ModelOptions{ReasoningMode: mg.ReasoningDisabled}})
	if response.Status != mg.StatusSuccess || provider.request.Options.ReasoningMode != mg.ReasoningAuto {
		t.Fatalf("non-reasoning model received provider-specific thinking toggle: response=%#v request=%#v", response, provider.request)
	}
}

func TestFacadeChatHappy(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
			mock.TokenChunk("hi"), mock.TokenChunk(" there"), mock.UsageChunk(3, 2),
		}})},
	}
	types, resp := collect(t, gw, mg.ModelRequest{RequestID: "r1", Streaming: true})

	for _, want := range []observability.EventType{
		observability.EventModelCallStarted, observability.EventModelTokenDelta,
		observability.EventModelUsageDelta, observability.EventModelCallCompleted,
	} {
		if !has(types, want) {
			t.Fatalf("missing %s in %v", want, types)
		}
	}
	if resp.Status != mg.StatusSuccess || resp.Text != "hi there" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestFacadeUsageCostFromGateway(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
			Model: "mock-model", Provider: "mock",
			Cost: mg.ModelCostTable{Currency: "USD", InputPer1K: 0.10, OutputPer1K: 0.20, ReasoningPer1K: 0.30, CacheReadPer1K: 0.01},
		}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
			mock.TokenChunk("hi"), mock.UsageDetailChunk(1000, 2000, 100, 500, 0),
		}})},
	}
	events, resp := collectEvents(t, gw, mg.ModelRequest{RequestID: "r1", Streaming: true})

	if resp.Usage.Source != mg.UsageSourceGateway {
		t.Fatalf("usage source = %q", resp.Usage.Source)
	}
	if resp.Usage.CacheReadTokens != 500 || resp.Usage.ReasoningTokens != 100 {
		t.Fatalf("usage details = %+v", resp.Usage)
	}
	if resp.Cost.Currency != "USD" || math.Abs(resp.Cost.Estimated-0.535) > 0.0000001 {
		t.Fatalf("cost = %+v", resp.Cost)
	}
	var completed mg.ModelCallCompletedPayload
	var completedUsage mg.ModelUsage
	var completedEventFound bool
	for _, ev := range events {
		if ev.EventType == observability.EventModelCallCompleted {
			completedEventFound = true
			if err := json.Unmarshal(ev.Payload, &completed); err != nil {
				t.Fatalf("completed payload: %v", err)
			}
			if err := json.Unmarshal(ev.Usage, &completedUsage); err != nil {
				t.Fatalf("completed event usage: %v (%s)", err, ev.Usage)
			}
		}
	}
	if !completedEventFound {
		t.Fatal("model_call_completed event not found")
	}
	if completed.Usage.Source != mg.UsageSourceGateway || math.Abs(completed.Cost.Estimated-resp.Cost.Estimated) > 0.0000001 {
		t.Fatalf("completed payload = %+v", completed)
	}
	if completedUsage != resp.Usage {
		t.Fatalf("completed event usage = %+v, want %+v", completedUsage, resp.Usage)
	}
}

func TestFacadeRecordsUsageLedger(t *testing.T) {
	stores := storagemem.New().Stores()
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
			Model: "mock-model", Provider: "mock",
			Cost: mg.ModelCostTable{Currency: "USD", InputPer1K: 0.10, OutputPer1K: 0.20},
		}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
			mock.TokenChunk("hi"), mock.UsageChunk(1000, 2000),
		}})},
		UsageLedger: stores.Usage,
	}
	_, resp := collectEvents(t, gw, mg.ModelRequest{
		RequestID: "req_usage",
		Trace:     observability.TraceContext{TenantID: "t1", SessionID: "s1", RunID: "run1", TraceID: "trace1"},
		AgentID:   "agent1",
		Streaming: true,
	})
	if resp.Status != mg.StatusSuccess {
		t.Fatalf("resp = %+v", resp)
	}
	records, err := stores.Usage.List(context.Background(), storage.ModelUsageQuery{TenantID: "t1"})
	if err != nil {
		t.Fatalf("usage list: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("usage records = %d", len(records))
	}
	rec := records[0]
	if rec.RunID != "run1" || rec.SessionID != "s1" || rec.AgentID != "agent1" || rec.PromptTokens != 1000 || rec.CompletionTokens != 2000 {
		t.Fatalf("usage record = %+v", rec)
	}
	if !rec.FirstTokenObserved || rec.TotalLatencyMS < 0 || rec.FirstTokenMS < 0 || rec.GenerationDurationMS < 0 || rec.OutputTokensPerSecond <= 0 {
		t.Fatalf("usage record has no provider performance attribution = %+v", rec)
	}
	summary, err := stores.Usage.Summary(context.Background(), storage.ModelUsageQuery{TenantID: "t1"})
	if err != nil {
		t.Fatalf("usage summary: %v", err)
	}
	if summary.Records != 1 || summary.PromptTokens != 1000 || summary.CompletionTokens != 2000 || math.Abs(summary.EstimatedCost-0.5) > 0.0000001 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestFacadeUsageEstimatedWhenProviderOmitsUsage(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
			mock.TokenChunk("abcd"),
		}})},
	}
	_, resp := collectEvents(t, gw, mg.ModelRequest{
		RequestID: "r1",
		Messages:  []mg.ChatMessage{mg.TextMessage("user", "abcdefgh")},
		Streaming: true,
	})
	if resp.Usage.Source != mg.UsageSourceEstimated {
		t.Fatalf("usage source = %q", resp.Usage.Source)
	}
	if resp.Usage.PromptTokens != 3 || resp.Usage.CompletionTokens != 1 {
		t.Fatalf("estimated usage = %+v", resp.Usage)
	}
}

func TestFacadeFailedStreamCarriesFirstTokenTelemetry(t *testing.T) {
	logger := observability.NewRingLogger(observability.NoopLogger{}, 20)
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{
			mock.TokenChunk("partial"), mock.ErrorChunk("stream failed", false),
		}})},
		Logger: logger,
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TraceID: "trace-fail", RunID: "run-fail", TenantID: "t1"})
	call, err := gw.Chat(ctx, mg.ModelRequest{RequestID: "req-fail", Trace: observability.MustTraceContext(ctx), AgentID: "agent1", Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	var failed mg.ModelCallFailedPayload
	for event := range call.Events {
		if event.EventType == observability.EventModelCallFailed {
			if err := json.Unmarshal(event.Payload, &failed); err != nil {
				t.Fatal(err)
			}
		}
	}
	resp, err := call.Await()
	if err != nil || resp.Status != mg.StatusFailed || !resp.Latency.FirstTokenObserved || !failed.Latency.FirstTokenObserved {
		t.Fatalf("failed stream telemetry: response=%#v payload=%#v err=%v", resp, failed, err)
	}
	logs := logger.QueryLogs(observability.LogQuery{RunID: "run-fail", TenantID: "t1"})
	if len(logs) == 0 || logs[0].Message != "model gateway call failed" || logs[0].Fields["first_token_observed"] != true {
		t.Fatalf("failed stream log telemetry: %#v", logs)
	}
}

func TestFacadeClassifiesContextOverflowPreflight(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
			Model: "tiny", Provider: "mock",
			Capability: mg.ModelCapability{Model: "tiny", Provider: "mock", Chat: true, Streaming: true, Limits: mg.ModelLimits{MaxContextTokens: 2}},
		}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("never")}})},
	}
	events, resp := collectEvents(t, gw, mg.ModelRequest{
		RequestID: "r1",
		Messages:  []mg.ChatMessage{mg.TextMessage("user", strings.Repeat("x", 100))},
		Streaming: true,
	})
	if resp.Status != mg.StatusFailed || resp.Error == nil || string(resp.Error.Type) != string(mg.ErrorContextExceeded) {
		t.Fatalf("resp = %+v", resp)
	}
	for _, ev := range events {
		if ev.EventType == observability.EventModelCallStarted {
			t.Fatalf("model should not start after preflight overflow: %#v", events)
		}
	}
}

func TestDefaultErrorClassifierClasses(t *testing.T) {
	classifier := mg.DefaultErrorClassifier{}
	tests := []struct {
		name      string
		err       error
		wantClass mg.ModelErrorClass
		retryable bool
	}{
		{name: "http 429", err: &mg.AdapterError{Message: "too many requests", HTTPStatus: 429}, wantClass: mg.ErrorRateLimited, retryable: true},
		{name: "http 500", err: &mg.AdapterError{Message: "bad gateway", HTTPStatus: 502}, wantClass: mg.ErrorProvider5xx, retryable: true},
		{name: "http 400", err: &mg.AdapterError{Message: "bad request", HTTPStatus: 400}, wantClass: mg.ErrorProvider4xx, retryable: false},
		{name: "context overflow", err: errors.New("maximum context length exceeded"), wantClass: mg.ErrorContextExceeded, retryable: true},
		{name: "content filter", err: errors.New("content filter triggered"), wantClass: mg.ErrorContentFilter, retryable: false},
		{name: "schema error", err: errors.New("json schema validation failed"), wantClass: mg.ErrorSchema, retryable: true},
		{name: "quota", err: errors.New("tenant quota exceeded"), wantClass: mg.ErrorQuotaExceeded, retryable: false},
		{name: "budget", err: errors.New("budget exceeded"), wantClass: mg.ErrorBudgetExceeded, retryable: false},
		{name: "timeout", err: context.DeadlineExceeded, wantClass: mg.ErrorTimeout, retryable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifier.Classify(context.Background(), tt.err)
			if got.Class != tt.wantClass || got.Retryable != tt.retryable {
				t.Fatalf("classify(%v) = %+v, want class=%s retryable=%v", tt.err, got, tt.wantClass, tt.retryable)
			}
		})
	}
}

func TestFacadeCapabilityRejectsUnsupportedToolCalling(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{
			Model: "plain", Provider: "mock",
			Capability: mg.ModelCapability{Model: "plain", Provider: "mock", Chat: true, Streaming: true, ToolCalling: false},
		}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("never")}})},
	}
	events, resp := collectEvents(t, gw, mg.ModelRequest{
		RequestID:   "r1",
		Streaming:   true,
		ToolsSchema: json.RawMessage(`[]`),
	})
	if resp.Status != mg.StatusFailed || resp.Error == nil || string(resp.Error.Type) != string(mg.ErrorProvider4xx) {
		t.Fatalf("resp = %+v", resp)
	}
	for _, ev := range events {
		if ev.EventType == observability.EventModelCallStarted {
			t.Fatalf("model should not start when capability preflight fails: %#v", events)
		}
	}
}

func TestFacadeCapabilityValidatesReasoningModes(t *testing.T) {
	tests := []struct {
		name string
		cap  mg.ModelCapability
		mode mg.ReasoningMode
		want mg.CallStatus
	}{
		{name: "enabled supported", cap: mg.ModelCapability{Chat: true, Streaming: true, Reasoning: true, ReasoningToggle: true}, mode: mg.ReasoningEnabled, want: mg.StatusSuccess},
		{name: "enabled unsupported", cap: mg.ModelCapability{Chat: true, Streaming: true}, mode: mg.ReasoningEnabled, want: mg.StatusFailed},
		{name: "disabled hybrid", cap: mg.ModelCapability{Chat: true, Streaming: true, Reasoning: true, ReasoningToggle: true}, mode: mg.ReasoningDisabled, want: mg.StatusSuccess},
		{name: "disabled thinking only", cap: mg.ModelCapability{Chat: true, Streaming: true, Reasoning: true}, mode: mg.ReasoningDisabled, want: mg.StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cap.Model, tt.cap.Provider = "m", "mock"
			provider := mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok")}})
			gw := &mg.Facade{
				Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "m", Provider: "mock", Capability: tt.cap}}, nil),
				Providers: map[string]mg.ChatProvider{"mock": provider},
			}
			_, response := collectEvents(t, gw, mg.ModelRequest{RequestID: "reasoning", Streaming: true, Options: mg.ModelOptions{ReasoningMode: tt.mode}})
			if response.Status != tt.want {
				t.Fatalf("reasoning capability status=%s want=%s response=%#v", response.Status, tt.want, response)
			}
			if tt.want == mg.StatusFailed && provider.Calls() != 0 {
				t.Fatalf("unsupported reasoning reached provider: calls=%d", provider.Calls())
			}
		})
	}
}

func TestFacadeCacheIsTenantScoped(t *testing.T) {
	provider := mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("cached"), mock.UsageChunk(1, 1)}})
	gw := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": provider},
		Cache:     mg.NewMemoryModelCache(),
	}
	req := mg.ModelRequest{RequestID: "r1", Trace: observability.TraceContext{TenantID: "t1"}, Messages: []mg.ChatMessage{mg.TextMessage("user", "hi")}, Cache: mg.CachePolicy{Enabled: true}}
	_, first := collectEvents(t, gw, req)
	req.RequestID = "r2"
	_, second := collectEvents(t, gw, req)
	req.Trace.TenantID = "t2"
	req.RequestID = "r3"
	_, third := collectEvents(t, gw, req)

	if first.CacheHit || !second.CacheHit || third.CacheHit {
		t.Fatalf("cache hits: first=%v second=%v third=%v", first.CacheHit, second.CacheHit, third.CacheHit)
	}
	if second.Latency.FirstTokenObserved || second.Latency.TotalMS != 0 || second.Latency.OutputTokensPerSecond != 0 || second.Usage.PromptTokens != 0 || second.Usage.CompletionTokens != 0 || second.Cost.Estimated != 0 {
		t.Fatalf("cache hit replayed provider accounting/performance: %#v", second)
	}
	if provider.Calls() != 2 {
		t.Fatalf("tenant-scoped cache should invoke provider twice, got %d", provider.Calls())
	}
}

func TestFacadeQuotaExceeded(t *testing.T) {
	gw := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok")}})},
		Quota:     mg.NewMemoryTenantQuotaManager(map[string]mg.TenantQuota{"t1": {DailyTokenBudget: 1}}),
	}
	req := mg.ModelRequest{RequestID: "r1", Trace: observability.TraceContext{TenantID: "t1"}, Messages: []mg.ChatMessage{mg.TextMessage("user", "hi")}}
	_, first := collectEvents(t, gw, req)
	req.RequestID = "r2"
	_, second := collectEvents(t, gw, req)
	if first.Status != mg.StatusSuccess || second.Status != mg.StatusFailed || second.Error == nil || string(second.Error.Type) != string(mg.ErrorBudgetExceeded) {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestFacadeWritesOutputArtifact(t *testing.T) {
	store := testArtifactStore()
	gw := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("artifact")}})},
		Output:    mg.ArtifactOutputWriter{Store: store},
	}
	_, resp := collectEvents(t, gw, mg.ModelRequest{
		RequestID: "r1",
		Trace:     observability.TraceContext{TenantID: "t1", UserID: "u1", SessionID: "s1", RunID: "run1"},
	})
	if resp.OutputRef == "" {
		t.Fatalf("missing output ref: %+v", resp)
	}
}

func TestFacadeEvaluationMockCapabilities(t *testing.T) {
	gw := &mg.Facade{Providers: map[string]mg.ChatProvider{"mock": mock.New("mock")}}
	// 新契约：评估类调用不设默认模型，ModelHint 缺失时 fail closed。
	if _, err := gw.Embedding(context.Background(), mg.EmbeddingRequest{RequestID: "e0", Texts: []string{"abc"}}); err == nil {
		t.Fatal("embedding without model hint must fail closed")
	}
	emb, err := gw.Embedding(context.Background(), mg.EmbeddingRequest{RequestID: "e1", Texts: []string{"abc"}, ModelHint: "mock-embed"})
	if err != nil || len(emb.Embeddings) != 1 {
		t.Fatalf("embedding = %+v err=%v", emb, err)
	}
	rerank, err := gw.Rerank(context.Background(), mg.RerankRequest{RequestID: "r1", Query: "q", Documents: []string{"a", "b"}, ModelHint: "mock-rerank"})
	if err != nil || len(rerank.Results) != 2 {
		t.Fatalf("rerank = %+v err=%v", rerank, err)
	}
	judge, err := gw.Judge(context.Background(), mg.JudgeRequest{RequestID: "j1", Output: "ok"})
	if err != nil || judge.Score == 0 {
		t.Fatalf("judge = %+v err=%v", judge, err)
	}
}

func TestFacadeFallbackCrossProvider(t *testing.T) {
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary:  mg.ModelTarget{Model: "m1", Provider: "p1"},
			Fallback: []mg.ModelTarget{{Model: "m2", Provider: "p2"}},
		}, nil),
		Providers: map[string]mg.ChatProvider{
			"p1": mock.New("p1", mock.Script{InvokeErr: &mg.AdapterError{Message: "429", Retryable: true}}),
			"p2": mock.New("p2", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok")}}),
		},
	}
	types, resp := collect(t, gw, mg.ModelRequest{RequestID: "r1", Streaming: true})

	if !has(types, observability.EventModelCallFailed) || !has(types, observability.EventModelFallbackApplied) {
		t.Fatalf("expected failed+fallback_applied in %v", types)
	}
	if resp.Status != mg.StatusSuccess || resp.Provider != "p2" || resp.Text != "ok" || !resp.FallbackApplied {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestFacadeFallbackRechecksTokenBudgetPerTarget(t *testing.T) {
	provider := mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok")}})
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary: mg.ModelTarget{
				Model: "tiny", Provider: "mock",
				Capability: mg.ModelCapability{Model: "tiny", Provider: "mock", Chat: true, Streaming: true, Limits: mg.ModelLimits{MaxContextTokens: 2}},
			},
			Fallback: []mg.ModelTarget{{
				Model: "large", Provider: "mock",
				Capability: mg.ModelCapability{Model: "large", Provider: "mock", Chat: true, Streaming: true, Limits: mg.ModelLimits{MaxContextTokens: 1000}},
			}},
		}, nil),
		Providers: map[string]mg.ChatProvider{"mock": provider},
	}
	types, resp := collect(t, gw, mg.ModelRequest{
		RequestID:       "r1",
		Messages:        []mg.ChatMessage{mg.TextMessage("user", strings.Repeat("x", 100))},
		Streaming:       true,
		MaxPromptTokens: 1000,
	})
	if resp.Status != mg.StatusSuccess || resp.Model != "large" || resp.Text != "ok" || !resp.FallbackApplied {
		t.Fatalf("resp = %+v", resp)
	}
	if !has(types, observability.EventModelCallFailed) || !has(types, observability.EventModelFallbackApplied) {
		t.Fatalf("expected preflight failure then fallback in %v", types)
	}
	if provider.Calls() != 1 {
		t.Fatalf("provider should be called only for fallback target, got %d", provider.Calls())
	}
}

func TestFacadeBeforeAfterHooks(t *testing.T) {
	beforeCalled := false
	afterCalled := false
	gw := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "mock-model", Provider: "mock"}}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok")}})},
		Hooks: mg.ModelHooks{
			BeforeModel: []mg.BeforeModelHook{
				func(ctx context.Context, req mg.ModelRequest, target mg.ModelTarget) (mg.ModelRequest, error) {
					beforeCalled = true
					req.Messages = append(req.Messages, mg.TextMessage("system", "hooked"))
					return req, nil
				},
			},
			AfterModel: []mg.AfterModelHook{
				func(ctx context.Context, req mg.ModelRequest, target mg.ModelTarget, resp mg.ModelResponse) (mg.ModelResponse, error) {
					afterCalled = true
					resp.Text += "!"
					return resp, nil
				},
			},
		},
	}
	_, resp := collectEvents(t, gw, mg.ModelRequest{RequestID: "r1"})
	if !beforeCalled || !afterCalled {
		t.Fatalf("hook calls: before=%v after=%v", beforeCalled, afterCalled)
	}
	if resp.Text != "ok!" {
		t.Fatalf("resp text = %q", resp.Text)
	}
}

func TestFacadeFallbackExhausted(t *testing.T) {
	gw := &mg.Facade{
		Router:    mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "m1", Provider: "p1"}}, nil),
		Providers: map[string]mg.ChatProvider{"p1": mock.New("p1", mock.Script{InvokeErr: &mg.AdapterError{Message: "boom", Retryable: false}})},
	}
	_, resp := collect(t, gw, mg.ModelRequest{RequestID: "r1"})
	if resp.Status != mg.StatusFailed || resp.Error == nil {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestFacadeUnknownProviderFallsThrough(t *testing.T) {
	// primary provider not registered -> retryable -> fallback to registered one.
	gw := &mg.Facade{
		Router: mg.NewStaticRouter(mg.Route{
			Primary:  mg.ModelTarget{Model: "m", Provider: "missing"},
			Fallback: []mg.ModelTarget{{Model: "mock-model", Provider: "mock"}},
		}, nil),
		Providers: map[string]mg.ChatProvider{"mock": mock.New("mock", mock.Script{Chunks: []mg.NormalizedChunk{mock.TokenChunk("ok")}})},
	}
	_, resp := collect(t, gw, mg.ModelRequest{RequestID: "r1"})
	if resp.Status != mg.StatusSuccess || resp.Provider != "mock" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestFacadeNoProviders(t *testing.T) {
	gw := &mg.Facade{Router: mg.NewStaticRouter(mg.Route{Primary: mg.ModelTarget{Model: "m", Provider: "p"}}, nil)}
	if _, err := gw.Chat(context.Background(), mg.ModelRequest{}); err == nil {
		t.Fatal("expected error with no providers")
	}
}
