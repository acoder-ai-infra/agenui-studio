package toolgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestGatewayTruncationCreatesFullResultRefAndMetadata(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		ArtifactThresholdBytes: 4096,
		MaxSSEPreviewBytes:     48,
		MaxModelContextBytes:   64,
		RedactSensitiveFields:  true,
	}
	raw := &ToolRawResult{
		Data: json.RawMessage(`{"message":"abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz","password":"result-secret","nested":{"api_token":"nested-secret"}}`),
	}
	gateway := newRawResultGateway(eventStore, artifactStore, def, raw)

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke truncated result tool: %v", err)
	}
	if result.Status != ToolCallSucceeded || result.ResultRef == "" {
		t.Fatalf("truncated result must succeed with a full result ref: %#v", result)
	}
	if len(result.ResultPreview) > def.ResultPolicy.MaxSSEPreviewBytes || len(result.ModelContextResult) > def.ResultPolicy.MaxModelContextBytes {
		t.Fatalf("truncated layers exceed configured bounds: preview=%d/%d model=%d/%d", len(result.ResultPreview), def.ResultPolicy.MaxSSEPreviewBytes, len(result.ModelContextResult), def.ResultPolicy.MaxModelContextBytes)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallCompleted {
		t.Fatalf("truncated lifecycle = %#v", got)
	}
	meta, content := readArtifactForTest(t, ctx, artifactStore, result.ResultRef)
	if meta.ArtifactType != artifact.ArtifactTypeToolResult || meta.Visibility != artifact.VisibilityInternal || meta.RetentionPolicy != artifact.RetentionRunTTL {
		t.Fatalf("truncated full-result metadata = %#v", meta)
	}
	if !strings.Contains(string(content), "[REDACTED]") || containsAny(string(content), "result-secret", "nested-secret") {
		t.Fatalf("full result artifact is not safely redacted: %s", content)
	}
	if result.Usage.OutputBytes != int64(len(content)) {
		t.Fatalf("output bytes = %d, want full safe size %d", result.Usage.OutputBytes, len(content))
	}
	completed := decodeEventPayload(t, eventStore.events[2])
	if completed["truncated"] != true || completed["truncate_reason"] != "size_limit" {
		t.Fatalf("completed truncation metadata = %#v", completed)
	}
	if completed["original_size"] != float64(len(content)) || completed["result_ref"] != result.ResultRef {
		t.Fatalf("completed size/ref metadata = %#v, safe size=%d ref=%s", completed, len(content), result.ResultRef)
	}
	for layer, safe := range map[string][]byte{
		"preview":           result.ResultPreview,
		"model_context":     result.ModelContextResult,
		"completed_payload": eventStore.events[2].PayloadPreview,
		"artifact":          content,
	} {
		if !json.Valid(safe) || containsAny(string(safe), "result-secret", "nested-secret") {
			t.Fatalf("%s is invalid or leaked a secret: %s", layer, safe)
		}
	}
}

func TestGatewayPreservesBinaryResultBytesInArtifact(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.OutputSchema = nil
	def.ResultPolicy.RequireOutputSchema = false
	want := []byte{0x00, 0xff, 0x10, 0x80, 'A'}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Bytes: append([]byte(nil), want...), MimeType: "application/octet-stream",
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil || result.Status != ToolCallSucceeded || result.ResultRef == "" {
		t.Fatalf("Invoke() result = %#v, error = %v", result, err)
	}
	meta, got := readArtifactForTest(t, ctx, artifactStore, result.ResultRef)
	if !bytes.Equal(got, want) {
		t.Fatalf("binary artifact = %v, want %v", got, want)
	}
	if meta.MimeType != "application/octet-stream" || result.Usage.OutputBytes != int64(len(want)) {
		t.Fatalf("binary metadata = %#v, usage = %#v", meta, result.Usage)
	}
	if !json.Valid(result.ResultPreview) || bytes.Contains(result.ResultPreview, want) {
		t.Fatalf("binary preview must be safe structured metadata: %q", result.ResultPreview)
	}
}

func TestGatewayPreviewTruncationDoesNotSummarizeModelContext(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		ArtifactThresholdBytes: 4096,
		MaxSSEPreviewBytes:     48,
		MaxModelContextBytes:   4096,
		RedactSensitiveFields:  true,
		SummarizeWhenTruncated: true,
	}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data: json.RawMessage(`{"password":"summary-secret","text":"a long safe value that makes only the endpoint preview exceed its configured byte bound"}`),
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke summarized result tool: %v", err)
	}
	if result.Status != ToolCallSucceeded || result.ResultRef == "" {
		t.Fatalf("summarized truncation must retain a full result ref: %#v", result)
	}
	if bytes.Equal(result.ResultPreview, result.ModelContextResult) {
		t.Fatalf("preview truncation must not summarize model context: preview=%s model=%s", result.ResultPreview, result.ModelContextResult)
	}
	if len(result.ResultPreview) > def.ResultPolicy.MaxSSEPreviewBytes || len(result.ModelContextResult) > def.ResultPolicy.MaxModelContextBytes {
		t.Fatalf("safe layers exceed configured bounds: preview=%d/%d model=%d/%d", len(result.ResultPreview), def.ResultPolicy.MaxSSEPreviewBytes, len(result.ModelContextResult), def.ResultPolicy.MaxModelContextBytes)
	}
	if !strings.Contains(string(result.ModelContextResult), "configured byte bound") ||
		!strings.Contains(string(result.ModelContextResult), "[REDACTED]") ||
		containsAny(string(result.ModelContextResult), "summary-secret") {
		t.Fatalf("full model context leaked, lost redaction, or was truncated: %s", result.ModelContextResult)
	}
	completed := decodeEventPayload(t, eventStore.events[len(eventStore.events)-1])
	if completed["truncated"] != true || completed["truncate_reason"] != "size_limit" {
		t.Fatalf("preview truncation was not recorded: %#v", completed)
	}
	_, content := readArtifactForTest(t, ctx, artifactStore, result.ResultRef)
	if containsAny(string(content), "summary-secret") || !strings.Contains(string(content), "[REDACTED]") {
		t.Fatalf("full result artifact is unsafe: %s", content)
	}
}

func TestGatewayUsesSafePreviewAsSummaryWhenModelContextTruncated(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		ArtifactThresholdBytes: 4096,
		MaxSSEPreviewBytes:     48,
		MaxModelContextBytes:   64,
		RedactSensitiveFields:  true,
		SummarizeWhenTruncated: true,
	}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data: json.RawMessage(`{"password":"summary-secret","text":"a long safe value that exceeds both preview and model context byte bounds"}`),
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke summarized result tool: %v", err)
	}
	if result.Status != ToolCallSucceeded || result.ResultRef == "" {
		t.Fatalf("model-truncated result must retain a full result ref: %#v", result)
	}
	if !bytes.Equal(result.ResultPreview, result.ModelContextResult) {
		t.Fatalf("model summary = %s, want safe preview %s", result.ModelContextResult, result.ResultPreview)
	}
	if !strings.Contains(string(result.ResultPreview), "[REDACTED]") || containsAny(string(result.ResultPreview), "summary-secret") {
		t.Fatalf("safe summary leaked or lost redaction: %s", result.ResultPreview)
	}
}

func TestGatewayPersistsPartialResultArtifactBeforeFailedTerminal(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096, RedactSensitiveFields: true}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors: []ToolExecutor{rawResultExecutor{
			raw: &ToolRawResult{
				Data:    json.RawMessage(`{"items":[1,2],"password":"partial-secret"}`),
				Partial: true,
			},
			err: NewToolError(ErrorTypeUpstreamError, "upstream returned a partial result", true, nil),
		}},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("partial executor failure should return a durable failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeUpstreamError, true)
	if !result.Failure.Retryable || result.Failure.PartialResultRef == "" {
		t.Fatalf("partial failure metadata = %#v", result.Failure)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallFailed {
		t.Fatalf("partial lifecycle = %#v", got)
	}
	if eventStore.events[1].PayloadRef != result.Failure.PartialResultRef {
		t.Fatalf("partial artifact event ref = %q, failure ref = %q", eventStore.events[1].PayloadRef, result.Failure.PartialResultRef)
	}
	meta, content := readArtifactForTest(t, ctx, artifactStore, result.Failure.PartialResultRef)
	if meta.ArtifactType != artifact.ArtifactTypeToolResult || meta.Visibility != artifact.VisibilityInternal || meta.RetentionPolicy != artifact.RetentionRunTTL || meta.Metadata["partial"] != "true" {
		t.Fatalf("partial artifact metadata = %#v", meta)
	}
	if containsAny(string(content), "partial-secret") || !strings.Contains(string(content), "[REDACTED]") {
		t.Fatalf("partial artifact content is unsafe: %s", content)
	}
	failed := decodeEventPayload(t, eventStore.events[2])
	failure, ok := failed["tool_failure"].(map[string]any)
	if !ok || failure["partial_result_ref"] != result.Failure.PartialResultRef {
		t.Fatalf("failed payload lost partial result ref: %#v", failed)
	}
}

func TestGatewayPersistsFinalRetryPartialWithActualRetryCount(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 2, Idempotent: true}
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096}
	attempts := 0
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors: []ToolExecutor{rawResultExecutor{
			raw:      &ToolRawResult{Data: json.RawMessage(`{"items":[1,2]}`), Partial: true},
			err:      NewToolError(ErrorTypeUpstreamError, "retryable partial", true, nil),
			attempts: &attempts,
		}},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 1

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("final retry partial should return durable failure: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeUpstreamError, true)
	if attempts != 2 || result.Usage.RetryCount != 1 || result.Failure.PartialResultRef == "" {
		t.Fatalf("attempts=%d retry_count=%d failure=%#v", attempts, result.Usage.RetryCount, result.Failure)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallFailed {
		t.Fatalf("final retry partial lifecycle = %#v", got)
	}
	failed := decodeEventPayload(t, eventStore.events[2])
	if failed["retry_count"] != float64(1) {
		t.Fatalf("final retry partial payload retry_count=%#v", failed["retry_count"])
	}
}

func TestGatewayPersistsPartialCancellationAsNonRetryable(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.Retry = RetryPolicy{MaxAttempts: 3, Idempotent: true}
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096}
	attempts := 0
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors: []ToolExecutor{rawResultExecutor{
			raw:      &ToolRawResult{Data: json.RawMessage(`{"items":[1]}`), Partial: true},
			err:      context.Canceled,
			attempts: &attempts,
		}},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	req := baseToolRequest(json.RawMessage(`{"query":"hotel"}`))
	req.Policy.MaxRetries = 2

	result, err := gateway.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("partial cancellation should return durable failure: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeCancelled, true)
	if attempts != 1 || result.Usage.RetryCount != 0 || result.Failure.Retryable || result.Failure.PartialResultRef == "" {
		t.Fatalf("attempts=%d retry_count=%d failure=%#v", attempts, result.Usage.RetryCount, result.Failure)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallFailed {
		t.Fatalf("partial cancellation lifecycle = %#v", got)
	}
}

func TestGatewayNeverCompletesPartialResultWithoutError(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data:    json.RawMessage(`{"items":[1,2],"notice":"partial only"}`),
		Partial: true,
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("partial result without executor error should return a durable failure: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeNormalizationFailed, true)
	if result.Failure.Retryable || result.Failure.PartialResultRef == "" {
		t.Fatalf("unexpected partial normalization failure: %#v", result.Failure)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallFailed {
		t.Fatalf("partial-without-error lifecycle = %#v", got)
	}
	for _, event := range eventStore.events {
		if event.EventType == observability.EventToolCallCompleted {
			t.Fatalf("partial result persisted a successful terminal")
		}
	}
	meta, _ := readArtifactForTest(t, ctx, artifactStore, result.Failure.PartialResultRef)
	if meta.Metadata["partial"] != "true" {
		t.Fatalf("partial artifact marker = %#v", meta.Metadata)
	}
}

func TestGatewayFailsWhenPartialArtifactCannotPersist(t *testing.T) {
	eventStore := &recordingEventStore{}
	sentinel := errors.New("partial artifact unavailable")
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096}
	gateway := newRawResultGateway(eventStore, failingArtifactStore{err: sentinel}, def, &ToolRawResult{
		Data:    json.RawMessage(`{"items":[1,2]}`),
		Partial: true,
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("partial artifact failure should return a durable failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
	if result.Failure.PartialResultRef != "" {
		t.Fatalf("failed partial artifact exposed a ref: %#v", result.Failure)
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("partial artifact failure lifecycle = %#v", got)
	}
	if eventStore.events[1].Error == nil || eventStore.events[1].Error.Type != canonicalEventErrorType(ErrorTypeArtifactError) {
		t.Fatalf("partial artifact failure event error = %#v", eventStore.events[1].Error)
	}
}

func TestTruncateSafeResultHonorsFinalLimitAndUTF8(t *testing.T) {
	data := json.RawMessage(`{"text":"你好🙂世界🙂abcdefghijklmnopqrstuvwxyz"}`)
	for _, maxBytes := range []int{8, 10, 17, 32} {
		got, truncated := truncateSafeResult(data, maxBytes)
		if !truncated {
			t.Fatalf("limit %d did not truncate %d-byte input", maxBytes, len(data))
		}
		if len(got) > maxBytes {
			t.Fatalf("limit %d produced %d bytes: %s", maxBytes, len(got), got)
		}
		if !utf8.Valid(got) || !json.Valid(got) || bytes.Contains(got, []byte("�")) {
			t.Fatalf("limit %d produced invalid UTF-8/JSON: %q", maxBytes, got)
		}
	}
}

func TestGatewayRetainsPartialResultRefWhenDebugArtifactWriteFails(t *testing.T) {
	baseStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	sentinel := errors.New("partial debug artifact unavailable")
	store := failArtifactTypeStore{
		ArtifactStore: baseStore,
		artifactType:  artifact.ArtifactTypeDebugPayload,
		err:           sentinel,
	}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096, RedactSensitiveFields: true}
	gateway := newRawResultGateway(eventStore, store, def, &ToolRawResult{
		Data:    json.RawMessage(`{"items":[1,2],"password":"partial-debug-secret"}`),
		Debug:   json.RawMessage(`{"wire":"raw-debug"}`),
		Partial: true,
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("partial debug artifact failure should return durable failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
	if result.Failure.PartialResultRef == "" {
		t.Fatalf("partial result ref was lost after debug artifact failure: %#v", result.Failure)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallFailed {
		t.Fatalf("partial debug failure lifecycle = %#v", got)
	}
	if eventStore.events[1].PayloadRef != result.Failure.PartialResultRef {
		t.Fatalf("partial artifact event ref=%q failure ref=%q", eventStore.events[1].PayloadRef, result.Failure.PartialResultRef)
	}
	meta, content := readArtifactForTest(t, ctx, baseStore, result.Failure.PartialResultRef)
	if meta.Metadata["partial"] != "true" || containsAny(string(content), "partial-debug-secret") {
		t.Fatalf("partial artifact was not retained safely: meta=%#v content=%s", meta, content)
	}
}

func TestGatewayOffloadsLargeResultToArtifact(t *testing.T) {
	eventStore := &recordingEventStore{}
	artifactStore := artifact.NewStore(artifact.StoreConfig{
		ObjectStore:   objectstore.NewMemory(),
		MetadataStore: metastore.NewMemory(),
	})
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		ArtifactThresholdBytes: 24,
		MaxSSEPreviewBytes:     20,
		MaxModelContextBytes:   64,
	}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				return &FunctionResult{
					Data:     json.RawMessage(`{"text":"abcdefghijklmnopqrstuvwxyz","count":1}`),
					MimeType: "application/json",
				}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
	ctx := artifact.ContextWithActor(testTraceContext(), artifact.Actor{
		TenantID:  "tenant-a",
		UserID:    "runtime",
		SessionID: "sess-1",
		RunID:     "run-1",
		AgentID:   "agent-a",
		Role:      artifact.ActorRuntime,
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke large result tool: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if result.ResultRef == "" {
		t.Fatalf("large result should be offloaded to artifact")
	}
	if len(result.Usage.ArtifactRefs) != 1 || result.Usage.ArtifactRefs[0] != result.ResultRef {
		t.Fatalf("artifact refs not tracked in usage: %#v", result.Usage.ArtifactRefs)
	}
	if strings.Contains(string(result.ResultPreview), "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("preview should be truncated, got %s", result.ResultPreview)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallCompleted {
		t.Fatalf("expected started, artifact_created, completed; got %#v", got)
	}
	var completed map[string]any
	if err := json.Unmarshal(eventStore.events[2].PayloadPreview, &completed); err != nil {
		t.Fatalf("decode completed payload: %v", err)
	}
	if completed["result_ref"] != result.ResultRef {
		t.Fatalf("completed event should carry artifact ref, payload=%#v result_ref=%s", completed, result.ResultRef)
	}
	obj, err := artifactStore.Get(ctx, result.ResultRef, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		t.Fatalf("read result artifact for model context: %v", err)
	}
	defer obj.Content.Close()
	content, err := io.ReadAll(obj.Content)
	if err != nil {
		t.Fatalf("read artifact content: %v", err)
	}
	if string(content) != `{"text":"abcdefghijklmnopqrstuvwxyz","count":1}` {
		t.Fatalf("unexpected artifact content: %s", content)
	}
}

func TestGatewayFailsWhenLargeArtifactWriteFails(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 8}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   failingArtifactStore{err: errors.New("artifact store unavailable")},
		Executors: []ToolExecutor{NewFunctionExecutor(map[string]FunctionTool{
			"search": func(context.Context, FunctionCall) (*FunctionResult, error) {
				return &FunctionResult{
					Data:     json.RawMessage(`{"text":"abcdefghijklmnopqrstuvwxyz"}`),
					MimeType: "application/json",
				}, nil
			},
		})},
		IDGenerator: fixedIDGenerator{},
		Clock:       fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("artifact failure should be returned as ToolCallResult, got error: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("expected started then failed, got %#v", got)
	}
}

func TestGatewaySeparatesResultAndDebugArtifacts(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		ArtifactThresholdBytes: 32,
		MaxSSEPreviewBytes:     1024,
		MaxModelContextBytes:   1024,
		RedactSensitiveFields:  true,
	}
	safeResult := json.RawMessage(`{"message":"safe-result-large-enough-for-an-artifact","nested":{"password":"result-secret","items":[{"api_token":"array-secret"}]}}`)
	rawDebug := json.RawMessage(`{"wire":"raw-debug-secret","authorization":"Bearer debug-secret"}`)
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data:     safeResult,
		MimeType: "application/json",
		Debug:    rawDebug,
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke result/debug tool: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	if result.ResultRef == "" || result.DebugRef == "" || result.ResultRef == result.DebugRef {
		t.Fatalf("result/debug refs must be independent: result=%q debug=%q", result.ResultRef, result.DebugRef)
	}
	if len(result.Usage.ArtifactRefs) != 2 || !hasArtifactRef(result.Usage.ArtifactRefs, result.ResultRef) || !hasArtifactRef(result.Usage.ArtifactRefs, result.DebugRef) {
		t.Fatalf("usage refs = %#v, want result and debug refs", result.Usage.ArtifactRefs)
	}
	for layer, data := range map[string]json.RawMessage{
		"preview":       result.ResultPreview,
		"model_context": result.ModelContextResult,
	} {
		text := string(data)
		if containsAny(text, "result-secret", "array-secret", "raw-debug-secret", "Bearer debug-secret") {
			t.Fatalf("%s leaked secret data: %s", layer, text)
		}
		if !strings.Contains(text, "[REDACTED]") {
			t.Fatalf("%s did not contain recursive redaction markers: %s", layer, text)
		}
	}

	resultObject, err := artifactStore.Get(ctx, result.ResultRef, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		t.Fatalf("get safe result artifact: %v", err)
	}
	resultContent, err := io.ReadAll(resultObject.Content)
	resultObject.Content.Close()
	if err != nil {
		t.Fatalf("read safe result artifact: %v", err)
	}
	if resultObject.Meta.ArtifactType != artifact.ArtifactTypeToolResult || resultObject.Meta.Visibility != artifact.VisibilityInternal || resultObject.Meta.RetentionPolicy != artifact.RetentionRunTTL {
		t.Fatalf("unexpected result artifact metadata: %#v", resultObject.Meta)
	}
	if containsAny(string(resultContent), "result-secret", "array-secret", "raw-debug-secret") || !strings.Contains(string(resultContent), "[REDACTED]") {
		t.Fatalf("unsafe full result artifact: %s", resultContent)
	}

	debugObject, err := artifactStore.Get(ctx, result.DebugRef, artifact.GetOptions{Purpose: artifact.PurposeDebug})
	if err != nil {
		t.Fatalf("get raw debug artifact: %v", err)
	}
	debugContent, err := io.ReadAll(debugObject.Content)
	debugObject.Content.Close()
	if err != nil {
		t.Fatalf("read raw debug artifact: %v", err)
	}
	if debugObject.Meta.ArtifactType != artifact.ArtifactTypeDebugPayload || debugObject.Meta.Visibility != artifact.VisibilityDebug || debugObject.Meta.RetentionPolicy != artifact.RetentionDebugShortTTL {
		t.Fatalf("unexpected debug artifact metadata: %#v", debugObject.Meta)
	}
	if string(debugContent) != string(rawDebug) {
		t.Fatalf("debug artifact changed: got %s want %s", debugContent, rawDebug)
	}

	if got := eventTypes(eventStore.events); len(got) != 4 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolArtifactCreated || got[3] != observability.EventToolCallCompleted {
		t.Fatalf("unexpected result/debug event sequence: %#v", got)
	}
	artifactEvents := map[string]observability.AgentEvent{}
	for _, event := range eventStore.events {
		if event.Visibility == observability.VisibilityUserVisible && strings.Contains(string(event.PayloadPreview), "raw-debug-secret") {
			t.Fatalf("raw debug leaked into user-visible %s payload: %s", event.EventType, event.PayloadPreview)
		}
		if event.EventType == observability.EventToolArtifactCreated {
			artifactEvents[event.PayloadRef] = event
		}
	}
	if len(artifactEvents) != 2 {
		t.Fatalf("artifact events by outer ref = %#v", artifactEvents)
	}
	for _, meta := range []artifact.ArtifactMeta{resultObject.Meta, debugObject.Meta} {
		event, ok := artifactEvents[meta.ArtifactRef]
		if !ok {
			t.Fatalf("missing artifact event for %s", meta.ArtifactRef)
		}
		if event.IdempotencyKey != "run-1:tc-1:artifact:"+hashString(meta.ArtifactRef)[:24] {
			t.Fatalf("artifact event key = %q for %s", event.IdempotencyKey, meta.ArtifactRef)
		}
		var payload map[string]any
		if err := json.Unmarshal(event.PayloadPreview, &payload); err != nil {
			t.Fatalf("decode artifact payload: %v", err)
		}
		if payload["artifact_ref"] != meta.ArtifactRef || payload["artifact_type"] != string(meta.ArtifactType) || payload["mime_type"] != meta.MimeType || payload["hash"] != meta.Hash || payload["visibility"] != string(meta.Visibility) || int64(payload["size_bytes"].(float64)) != meta.SizeBytes {
			t.Fatalf("artifact payload is not authoritative: payload=%#v meta=%#v", payload, meta)
		}
	}
	completed := eventStore.events[len(eventStore.events)-1]
	if completed.PayloadRef != result.ResultRef || completed.DebugRef != result.DebugRef {
		t.Fatalf("completed refs = payload %q debug %q, want %q/%q", completed.PayloadRef, completed.DebugRef, result.ResultRef, result.DebugRef)
	}
}

func TestGatewayFailsWhenDebugArtifactWriteFails(t *testing.T) {
	baseStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 4096, RedactSensitiveFields: true}
	sentinel := errors.New("debug artifact unavailable")
	store := failArtifactTypeStore{
		ArtifactStore: baseStore,
		artifactType:  artifact.ArtifactTypeDebugPayload,
		err:           sentinel,
	}
	gateway := newRawResultGateway(eventStore, store, def, &ToolRawResult{
		Data:  json.RawMessage(`{"ok":true}`),
		Debug: json.RawMessage(`{"raw":"debug-secret"}`),
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("debug write failure should be returned as ToolCallResult: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("expected started then artifact_error failure, got %#v", got)
	}
	if eventStore.events[1].Error == nil || eventStore.events[1].Error.Type != canonicalEventErrorType(ErrorTypeArtifactError) {
		t.Fatalf("failed event error = %#v", eventStore.events[1].Error)
	}
	for _, event := range eventStore.events {
		if event.EventType == observability.EventToolCallCompleted {
			t.Fatalf("completed event persisted after debug artifact failure")
		}
	}
}

func TestGatewayEmitsDistinctArtifactEventsForArgumentsResultsAndDebug(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		MaxInlineBytes:         16,
		ArtifactThresholdBytes: 16,
		RedactSensitiveFields:  true,
	}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data:  json.RawMessage(`{"result":"large-result-requiring-artifact"}`),
		Debug: json.RawMessage(`{"wire":"raw-debug-payload"}`),
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"large-argument-requiring-artifact"}`)))
	if err != nil {
		t.Fatalf("invoke with argument/result/debug artifacts: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	artifactEvents := make([]observability.AgentEvent, 0, 3)
	for _, event := range result.Events {
		if event.EventType == observability.EventToolArtifactCreated {
			artifactEvents = append(artifactEvents, event)
		}
	}
	if len(artifactEvents) != 3 || len(result.Events) != 5 {
		t.Fatalf("result events = %#v, want started, three artifacts, completed", eventTypes(result.Events))
	}
	keys := map[string]struct{}{}
	refs := map[string]struct{}{}
	for _, event := range artifactEvents {
		if event.PayloadRef == "" {
			t.Fatalf("artifact event missing outer payload ref: %#v", event)
		}
		wantKey := "run-1:tc-1:artifact:" + hashString(event.PayloadRef)[:24]
		if event.IdempotencyKey != wantKey {
			t.Fatalf("artifact event key = %q, want %q", event.IdempotencyKey, wantKey)
		}
		keys[event.IdempotencyKey] = struct{}{}
		refs[event.PayloadRef] = struct{}{}
	}
	if len(keys) != 3 || len(refs) != 3 {
		t.Fatalf("artifact events collided: keys=%#v refs=%#v", keys, refs)
	}
}

func TestGatewayVerifiesExecutorArtifactRefBeforeEmission(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	meta, err := artifactStore.Put(ctx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", StepID: "step-1",
		OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tc-source",
		ArtifactType: artifact.ArtifactTypeFile, MimeType: "text/plain", Name: "report.txt",
		Visibility: artifact.VisibilityInternal, Content: strings.NewReader("authoritative artifact content"),
		RetentionPolicy: artifact.RetentionRunTTL, CreatedBy: "tool:source",
	})
	if err != nil {
		t.Fatalf("put executor artifact: %v", err)
	}
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data:         json.RawMessage(`{"ok":true}`),
		ArtifactRefs: []string{meta.ArtifactRef},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke with verified executor artifact: %v", err)
	}
	if result.Status != ToolCallSucceeded || len(result.Usage.ArtifactRefs) != 1 || result.Usage.ArtifactRefs[0] != meta.ArtifactRef {
		t.Fatalf("executor artifact was not exposed after verification: %#v", result)
	}
	if got := eventTypes(eventStore.events); len(got) != 3 || got[1] != observability.EventToolArtifactCreated {
		t.Fatalf("executor artifact event sequence = %#v", got)
	}
	event := eventStore.events[1]
	if event.PayloadRef != meta.ArtifactRef || event.Visibility != observability.VisibilityInternal {
		t.Fatalf("executor artifact outer metadata = %#v", event)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.PayloadPreview, &payload); err != nil {
		t.Fatalf("decode executor artifact event: %v", err)
	}
	if payload["artifact_type"] != string(meta.ArtifactType) || payload["mime_type"] != meta.MimeType || payload["hash"] != meta.Hash || int64(payload["size_bytes"].(float64)) != meta.SizeBytes {
		t.Fatalf("executor artifact event did not use Head metadata: %#v", payload)
	}
}

func TestGatewayRejectsUnknownExecutorArtifactRef(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	gateway := newRawResultGateway(eventStore, artifactStore, defaultSearchDefinition(), &ToolRawResult{
		Data:         json.RawMessage(`{"ok":true}`),
		ArtifactRefs: []string{"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_missing"},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("unknown executor artifact should return failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("unknown executor artifact events = %#v", got)
	}
}

func TestGatewayDoesNotWriteResultBeforeVerifyingExecutorArtifactRefs(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{ArtifactThresholdBytes: 8}
	gateway := newRawResultGateway(eventStore, artifactStore, def, &ToolRawResult{
		Data:         json.RawMessage(`{"result":"large-result-that-would-require-an-artifact"}`),
		ArtifactRefs: []string{"artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_missing"},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("unknown executor artifact should return failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
	artifacts, err := artifactStore.List(ctx, artifact.ListQuery{
		TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tc-1",
	})
	if err != nil {
		t.Fatalf("list tool-call artifacts: %v", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("unverified executor ref left result artifacts behind: %#v", artifacts)
	}
}

func TestGatewayRejectsCrossScopeExecutorArtifactRef(t *testing.T) {
	artifactStore, ctx := newScopedArtifactStore(t)
	otherRunCtx := artifact.ContextWithActor(testTraceContext(), artifact.Actor{
		TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-other", Role: artifact.ActorRuntime,
	})
	meta, err := artifactStore.Put(otherRunCtx, artifact.PutArtifactRequest{
		TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-other", StepID: "step-source",
		OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: "tc-source",
		ArtifactType: artifact.ArtifactTypeFile, MimeType: "text/plain",
		Visibility: artifact.VisibilityInternal, Content: strings.NewReader("cross-scope"),
		RetentionPolicy: artifact.RetentionRunTTL, CreatedBy: "tool:source",
	})
	if err != nil {
		t.Fatalf("put cross-scope artifact: %v", err)
	}
	eventStore := &recordingEventStore{}
	gateway := newRawResultGateway(eventStore, artifactStore, defaultSearchDefinition(), &ToolRawResult{
		Data:         json.RawMessage(`{"ok":true}`),
		ArtifactRefs: []string{meta.ArtifactRef},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("cross-scope executor artifact should return failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Fatalf("cross-scope executor artifact events = %#v", got)
	}
}

func TestGatewayRejectsExecutorArtifactWithoutTypeMetadata(t *testing.T) {
	baseStore, ctx := newScopedArtifactStore(t)
	store := headOverrideArtifactStore{
		ArtifactStore: baseStore,
		meta: &artifact.ArtifactMeta{
			ArtifactRef: "artifact://tenants/tenant-a/sessions/sess-1/runs/run-1/art_untyped",
			TenantID:    "tenant-a", SessionID: "sess-1", RunID: "run-1",
			MimeType: "application/octet-stream", SizeBytes: 1, Hash: "sha256:00",
			Visibility: artifact.VisibilityInternal,
		},
	}
	eventStore := &recordingEventStore{}
	gateway := newRawResultGateway(eventStore, store, defaultSearchDefinition(), &ToolRawResult{
		Data:         json.RawMessage(`{"ok":true}`),
		ArtifactRefs: []string{store.meta.ArtifactRef},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("untyped executor artifact should return failed result: %v", err)
	}
	assertFailedResult(t, result, ErrorTypeArtifactError, true)
}

func TestGatewayRedactsSensitiveResultFieldsWithDefaultOutputPolicy(t *testing.T) {
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	if def.ResultPolicy != (ToolOutputPolicy{}) {
		t.Fatalf("test requires zero/default output policy, got %#v", def.ResultPolicy)
	}
	gateway := newRawResultGateway(eventStore, nil, def, &ToolRawResult{
		Data: json.RawMessage(`{"username":"alice","nested":{"password":"default-secret","items":[{"api_token":"array-secret"}],"credential":"credential-secret"}}`),
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke with default output policy: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s", result.Status)
	}
	for layer, payload := range map[string]string{
		"result_preview":       string(result.ResultPreview),
		"model_context_result": string(result.ModelContextResult),
		"completed_payload":    string(eventStore.events[len(eventStore.events)-1].PayloadPreview),
	} {
		if containsAny(payload, "default-secret", "array-secret", "credential-secret") {
			t.Fatalf("%s leaked a sensitive default-policy value: %s", layer, payload)
		}
		if !strings.Contains(payload, "[REDACTED]") {
			t.Fatalf("%s did not contain redaction markers: %s", layer, payload)
		}
	}
}

func TestGatewayFailsClosedWhenResultCannotFormSafeLayers(t *testing.T) {
	const sentinel = "MALFORMED_RESULT_SECRET"
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	if len(def.OutputSchema) != 0 || def.ResultPolicy.RequireOutputSchema {
		t.Fatalf("test requires the documented output-schema opt-out, got schema=%s policy=%#v", def.OutputSchema, def.ResultPolicy)
	}
	gateway := newRawResultGateway(eventStore, nil, def, &ToolRawResult{
		Data: json.RawMessage(`{"password":"` + sentinel + `"`),
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("malformed result should become a canonical failed result: %v", err)
	}
	if result == nil {
		t.Fatal("malformed result returned nil ToolCallResult")
	}
	if result.Status != ToolCallFailed {
		t.Errorf("status = %s, want failed", result.Status)
	}
	if result.Failure == nil || result.Failure.ErrorType != string(ErrorTypeNormalizationFailed) || !result.Failure.Executed {
		t.Errorf("failure = %#v, want executed normalization_failed", result.Failure)
	}
	if len(result.ResultPreview) != 0 || len(result.ModelContextResult) != 0 || result.ResultRef != "" || result.DebugRef != "" {
		t.Errorf("malformed result populated safe layers: preview=%q model=%q result_ref=%q debug_ref=%q", result.ResultPreview, result.ModelContextResult, result.ResultRef, result.DebugRef)
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallFailed {
		t.Errorf("malformed result lifecycle = %#v, want started then failed", got)
	}

	observed := string(result.ResultPreview) + string(result.ModelContextResult) + result.ResultRef + result.DebugRef
	if failure, marshalErr := json.Marshal(result.Failure); marshalErr != nil {
		t.Fatalf("marshal failure: %v", marshalErr)
	} else {
		observed += string(failure)
	}
	for _, event := range append(append([]observability.AgentEvent(nil), result.Events...), eventStore.events...) {
		encoded, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatalf("marshal event %s: %v", event.EventType, marshalErr)
		}
		observed += string(encoded)
	}
	if strings.Contains(observed, sentinel) {
		t.Errorf("malformed raw result leaked into safe result/event layers: %s", observed)
	}
}

func TestGatewayRedactsSensitiveResultWithLargeJSONNumber(t *testing.T) {
	const sentinel = "LARGE_NUMBER_RESULT_SECRET"
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	gateway := newRawResultGateway(eventStore, nil, def, &ToolRawResult{
		Data: json.RawMessage(`{"password":"` + sentinel + `","value":1e1000}`),
	})

	result, err := gateway.Invoke(testTraceContext(), baseToolRequest(json.RawMessage(`{"query":"hotel"}`)))
	if err != nil {
		t.Fatalf("invoke valid large-number JSON: %v", err)
	}
	if result == nil || result.Status != ToolCallSucceeded {
		t.Fatalf("large-number result = %#v, want succeeded", result)
	}
	if got := eventTypes(eventStore.events); len(got) != 2 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolCallCompleted {
		t.Fatalf("large-number lifecycle = %#v", got)
	}
	for layer, payload := range map[string]json.RawMessage{
		"result_preview":       result.ResultPreview,
		"model_context_result": result.ModelContextResult,
		"completed_event":      eventStore.events[len(eventStore.events)-1].PayloadPreview,
	} {
		if !json.Valid(payload) {
			t.Errorf("%s is not valid JSON: %q", layer, payload)
		}
		if strings.Contains(string(payload), sentinel) {
			t.Errorf("%s leaked the sensitive value: %s", layer, payload)
		}
		if !strings.Contains(string(payload), "[REDACTED]") {
			t.Errorf("%s has no redaction marker: %s", layer, payload)
		}
	}
}

func TestGatewayDoesNotDuplicateEchoedArgumentArtifactEvent(t *testing.T) {
	artifactStore, _ := newScopedArtifactStore(t)
	ctx := artifact.ContextWithActor(testTraceContext(), artifact.Actor{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-1", RunID: "run-1", AgentID: "agent-a", Role: artifact.ActorDebug,
	})
	eventStore := &recordingEventStore{}
	def := defaultSearchDefinition()
	def.ResultPolicy = ToolOutputPolicy{
		MaxInlineBytes:         16,
		ArtifactThresholdBytes: 4096,
	}
	gateway := NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors:       []ToolExecutor{argumentRefEchoExecutor{}},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})

	result, err := gateway.Invoke(ctx, baseToolRequest(json.RawMessage(`{"query":"large-inline-argument-that-is-offloaded"}`)))
	if err != nil {
		t.Fatalf("invoke echoed argument artifact tool: %v", err)
	}
	if result.Status != ToolCallSucceeded {
		t.Fatalf("status = %s failure=%#v", result.Status, result.Failure)
	}
	artifactEvents := make([]observability.AgentEvent, 0, 2)
	refs := map[string]struct{}{}
	for _, event := range result.Events {
		if event.EventType == observability.EventToolArtifactCreated {
			artifactEvents = append(artifactEvents, event)
			refs[event.PayloadRef] = struct{}{}
		}
	}
	if len(artifactEvents) != 1 || len(refs) != 1 {
		t.Fatalf("echoed argument ref produced duplicate artifact events: events=%d refs=%#v sequence=%#v", len(artifactEvents), refs, eventTypes(result.Events))
	}
	if got := eventTypes(result.Events); len(got) != 3 || got[0] != observability.EventToolCallStarted || got[1] != observability.EventToolArtifactCreated || got[2] != observability.EventToolCallCompleted {
		t.Fatalf("unexpected deduplicated event sequence: %#v", got)
	}
}

type rawResultExecutor struct {
	raw      *ToolRawResult
	err      error
	attempts *int
}

func (e rawResultExecutor) Type() ToolType {
	return ToolTypeFunction
}

func (e rawResultExecutor) Execute(context.Context, *ToolDefinition, ToolCallRequest) (*ToolRawResult, error) {
	if e.attempts != nil {
		(*e.attempts)++
	}
	return e.raw, e.err
}

type argumentRefEchoExecutor struct{}

func (argumentRefEchoExecutor) Type() ToolType {
	return ToolTypeFunction
}

func (argumentRefEchoExecutor) Execute(_ context.Context, _ *ToolDefinition, req ToolCallRequest) (*ToolRawResult, error) {
	if req.ArgumentsRef == "" {
		return nil, NewToolError(ErrorTypeInternal, "expected offloaded arguments ref", false, nil)
	}
	return &ToolRawResult{
		Data:         json.RawMessage(`{"ok":true}`),
		ArtifactRefs: []string{req.ArgumentsRef},
	}, nil
}

func newRawResultGateway(eventStore EventStore, artifactStore artifact.ArtifactStore, def ToolDefinition, raw *ToolRawResult) *DefaultGateway {
	return NewGateway(GatewayConfig{
		Registry:        NewStaticRegistry([]ToolDefinition{def}),
		SchemaValidator: BasicSchemaValidator{},
		EventStore:      eventStore,
		ArtifactStore:   artifactStore,
		Executors:       []ToolExecutor{rawResultExecutor{raw: raw}},
		IDGenerator:     fixedIDGenerator{},
		Clock:           fixedClock{now: time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)},
	})
}

func hasArtifactRef(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

func decodeEventPayload(t *testing.T, event observability.AgentEvent) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(event.PayloadPreview, &payload); err != nil {
		t.Fatalf("decode %s payload: %v", event.EventType, err)
	}
	return payload
}

func readArtifactForTest(t *testing.T, ctx context.Context, store artifact.ArtifactStore, ref string) (artifact.ArtifactMeta, []byte) {
	t.Helper()
	object, err := store.Get(ctx, ref, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		t.Fatalf("get artifact %s: %v", ref, err)
	}
	content, readErr := io.ReadAll(object.Content)
	closeErr := object.Content.Close()
	if readErr != nil {
		t.Fatalf("read artifact %s: %v", ref, readErr)
	}
	if closeErr != nil {
		t.Fatalf("close artifact %s: %v", ref, closeErr)
	}
	return object.Meta, content
}

type failArtifactTypeStore struct {
	artifact.ArtifactStore
	artifactType artifact.ArtifactType
	err          error
}

func (s failArtifactTypeStore) Put(ctx context.Context, req artifact.PutArtifactRequest) (*artifact.ArtifactMeta, error) {
	if req.ArtifactType == s.artifactType {
		return nil, s.err
	}
	return s.ArtifactStore.Put(ctx, req)
}

type headOverrideArtifactStore struct {
	artifact.ArtifactStore
	meta *artifact.ArtifactMeta
	err  error
}

func (s headOverrideArtifactStore) Head(context.Context, string) (*artifact.ArtifactMeta, error) {
	return s.meta, s.err
}

type failingArtifactStore struct {
	err error
}

func (s failingArtifactStore) Put(context.Context, artifact.PutArtifactRequest) (*artifact.ArtifactMeta, error) {
	return nil, s.err
}

func (s failingArtifactStore) Get(context.Context, string, artifact.GetOptions) (*artifact.ArtifactObject, error) {
	return nil, s.err
}

func (s failingArtifactStore) Head(context.Context, string) (*artifact.ArtifactMeta, error) {
	return nil, s.err
}

func (s failingArtifactStore) Delete(context.Context, string, artifact.DeleteReason) error {
	return s.err
}

func (s failingArtifactStore) CreateDownloadURL(context.Context, string, artifact.DownloadURLOptions) (*artifact.DownloadURL, error) {
	return nil, s.err
}

func (s failingArtifactStore) List(context.Context, artifact.ListQuery) ([]artifact.ArtifactMeta, error) {
	return nil, s.err
}

func (s failingArtifactStore) CleanupExpired(context.Context, time.Time) (*artifact.CleanupResult, error) {
	return nil, s.err
}
