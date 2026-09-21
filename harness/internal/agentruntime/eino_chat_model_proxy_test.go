package agentruntime

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestEinoChatModelProxyStreamsThroughModelInvoker(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{
		modelEvent(observability.EventModelCallStarted),
		{Event: observability.AgentEvent{EventType: observability.EventModelThoughtDelta}, ReasoningDelta: "reasoning "},
		modelTextEvent("hello "),
		modelTextEvent("world"),
		modelEvent(observability.EventModelCallCompleted),
	}}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.ModelOptions = ModelCallOptions{ReasoningMode: ModelReasoningEnabled, ReasoningBudget: 256}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	proxy, err := NewEinoChatModelProxy(invoker, pkg)
	if err != nil {
		t.Fatal(err)
	}

	imageURL := "https://example.test/image.png"
	reader, err := proxy.Stream(einoModelTestContext(), []*schema.Message{{Role: schema.User, Content: "hi", UserInputMultiContent: []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &imageURL, MIMEType: "image/png"}}}}}}, model.WithTemperature(0.2), model.WithMaxTokens(128))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var content, reasoning string
	for {
		message, recvErr := reader.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		content += message.Content
		reasoning += message.ReasoningContent
	}
	if content != "hello world" {
		t.Fatalf("content=%q", content)
	}
	if reasoning != "reasoning " {
		t.Fatalf("reasoning content=%q", reasoning)
	}
	req := invoker.Requests()[0]
	if req.Package.PackageID != pkg.PackageID+".round.1" || len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("unexpected request: %#v", req)
	}
	if req.Package.ContextHash != ComputeModelContextHash(req.Package) {
		t.Fatalf("derived package hash mismatch: %#v", req.Package)
	}
	if len(req.Messages[0].ContentParts) != 1 || req.Messages[0].ContentParts[0].URL != imageURL {
		t.Fatalf("multimodal input not forwarded: %#v", req.Messages[0])
	}
	if req.Options.Temperature == nil || *req.Options.Temperature != 0.2 || req.Options.MaxTokens == nil || *req.Options.MaxTokens != 128 || req.Options.ReasoningMode != ModelReasoningEnabled || req.Options.ReasoningBudget != 256 {
		t.Fatalf("options not forwarded: %#v", req.Options)
	}
}

func TestEinoChatModelProxyDoesNotBlockOnModelDeltaBackpressure(t *testing.T) {
	items := []ModelStreamItem{modelEvent(observability.EventModelCallStarted)}
	for i := 0; i < 256; i++ {
		items = append(items, modelTextEvent("x"))
	}
	items = append(items,
		ModelStreamItem{Event: observability.AgentEvent{EventType: observability.EventModelThoughtDelta}, ReasoningDelta: "thinking"},
		modelEvent(observability.EventModelCallCompleted),
	)
	invoker := &capturingEinoModelInvoker{items: items}
	proxy, err := NewEinoChatModelProxy(invoker, validEinoModelPackage())
	if err != nil {
		t.Fatal(err)
	}
	emitter := &backpressuredModelDeltaEmitter{}
	ctx := WithRuntimeEventEmitter(context.Background(), emitter)
	reader, err := proxy.Stream(ctx, []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var content, reasoning string
	for {
		message, recvErr := reader.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		content += message.Content
		reasoning += message.ReasoningContent
	}
	if content != strings.Repeat("x", 256) {
		t.Fatalf("content length=%d", len(content))
	}
	if reasoning != "thinking" {
		t.Fatalf("reasoning=%q", reasoning)
	}
	if !eventTypesContain(emitter.events, observability.EventModelCallStarted) || !eventTypesContain(emitter.events, observability.EventModelCallCompleted) {
		t.Fatalf("critical model events not preserved: %#v", emitter.events)
	}
	if eventTypesContain(emitter.events, observability.EventModelTokenDelta) || eventTypesContain(emitter.events, observability.EventModelThoughtDelta) {
		t.Fatalf("backpressured model delta events leaked through: %#v", emitter.events)
	}
}

func TestEinoRuntimeDrainsBridgeWhileReadingStreamingMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bridge := newRuntimeEventBridge(1)
	out := make(chan observability.AgentEvent, 8)
	reader, writer := schema.Pipe[*schema.Message](1)
	done := make(chan bool, 1)
	go func() {
		done <- (&EinoRuntime{}).emitMessage(ctx, out, &adk.MessageVariant{IsStreaming: true, MessageStream: reader, Role: schema.Assistant}, "", bridge)
	}()
	go func() {
		if err := bridge.Emit(ctx, modelEvent(observability.EventModelCallStarted).Event); err != nil {
			writer.Send(nil, err)
			return
		}
		if writer.Send(&schema.Message{Role: schema.Assistant, Content: "ok"}, nil) {
			return
		}
		if err := bridge.Emit(ctx, modelEvent(observability.EventModelCallCompleted).Event); err != nil {
			writer.Send(nil, err)
			return
		}
		writer.Close()
	}()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("streaming message handling returned false")
		}
	case <-ctx.Done():
		t.Fatal("streaming message handling deadlocked while bridge contained terminal model event")
	}
	var events []observability.EventType
	for {
		select {
		case event := <-out:
			events = append(events, event.EventType)
		default:
			if !eventTypesContain(events, observability.EventModelCallStarted) || !eventTypesContain(events, observability.EventAgentTextDelta) || !eventTypesContain(events, observability.EventModelCallCompleted) {
				t.Fatalf("stream/bridge events missing: %#v", events)
			}
			return
		}
	}
}

func TestEinoChatModelProxyForwardsToolsAndToolMessages(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{
		modelEvent(observability.EventModelCallStarted),
		modelToolEvent(ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: []byte(`{"q":"x"}`)}),
		modelEvent(observability.EventModelCallCompleted),
	}}
	base, err := NewEinoChatModelProxy(invoker, validEinoModelPackage())
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := base.WithTools([]*schema.ToolInfo{{Name: "search", Desc: "search things"}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := withTools.Generate(einoModelTestContext(), []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "prior", Function: schema.FunctionCall{Name: "search", Arguments: `{}`}}}},
		{Role: schema.Tool, ToolCallID: "prior", ToolName: "search", Content: "old result"},
	}, model.WithToolChoice(schema.ToolChoiceAllowed, "search"))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "tc_1" || response.ToolCalls[0].Function.Name != "search" {
		t.Fatalf("tool call not converted: %#v", response)
	}
	req := invoker.Requests()[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != "search" || len(req.Tools[0].Schema) == 0 || !req.AllowTools {
		t.Fatalf("tools not forwarded: %#v", req)
	}
	if len(req.Messages) != 2 || req.Messages[1].ToolCallID != "prior" || req.Options.ToolChoice != "auto" {
		t.Fatalf("messages/options not forwarded: %#v", req)
	}
}

func TestEinoChatModelProxyDoesNotReleaseToolCallBeforeModelCompletionCommit(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{
		modelEvent(observability.EventModelCallStarted),
		modelToolEvent(ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: []byte(`{"q":"x"}`)}),
		modelEvent(observability.EventModelCallCompleted),
	}}
	base, err := NewEinoChatModelProxy(invoker, validEinoModelPackage())
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := base.WithTools([]*schema.ToolInfo{{Name: "search", Desc: "search things"}})
	if err != nil {
		t.Fatal(err)
	}
	emitter := newCompletionCommitGateEmitter()
	ctx, cancel := context.WithTimeout(WithRuntimeEventEmitter(context.Background(), emitter), time.Second)
	defer cancel()
	reader, err := withTools.Stream(ctx, []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	received := make(chan *schema.Message, 1)
	recvErr := make(chan error, 1)
	go func() {
		message, err := reader.Recv()
		if err != nil {
			recvErr <- err
			return
		}
		received <- message
	}()

	select {
	case <-emitter.completionEntered:
	case <-ctx.Done():
		t.Fatal("model completion never reached the durable commit barrier")
	}
	select {
	case message := <-received:
		t.Fatalf("tool call escaped before completion commit: %#v", message)
	case err := <-recvErr:
		t.Fatalf("stream failed before completion commit: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(emitter.releaseCompletion)
	select {
	case message := <-received:
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "tc_1" {
			t.Fatalf("unexpected committed tool call: %#v", message)
		}
	case err := <-recvErr:
		t.Fatalf("stream failed after completion commit: %v", err)
	case <-ctx.Done():
		t.Fatal("tool call was not released after completion commit")
	}
}

func TestEinoChatModelProxyWaitsForRuntimeServiceCommitBarrier(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{
		modelEvent(observability.EventModelCallStarted),
		modelToolEvent(ModelToolCall{ToolCallID: "tc_1", Name: "search", Arguments: []byte(`{"q":"x"}`)}),
		modelEvent(observability.EventModelCallCompleted),
	}}
	base, err := NewEinoChatModelProxy(invoker, validEinoModelPackage())
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := base.WithTools([]*schema.ToolInfo{{Name: "search", Desc: "search things"}})
	if err != nil {
		t.Fatal(err)
	}
	barrier := newRuntimeEventCommitBarrier(observability.NewULIDGenerator("test"))
	bridge := newRuntimeEventBridge(8)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = withRuntimeEventCommitBarrier(ctx, barrier)
	ctx = WithRuntimeEventEmitter(ctx, bridge)
	reader, err := withTools.Stream(ctx, []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	received := make(chan *schema.Message, 1)
	recvErr := make(chan error, 1)
	go func() {
		message, err := reader.Recv()
		if err != nil {
			recvErr <- err
			return
		}
		received <- message
	}()

	var types []observability.EventType
	var completed observability.AgentEvent
	for completed.EventID == "" {
		select {
		case event := <-bridge.events:
			types = append(types, event.EventType)
			if event.EventType == observability.EventModelCallCompleted {
				completed = event
			}
		case <-ctx.Done():
			t.Fatalf("completion did not reach RuntimeService bridge: events=%v", types)
		}
	}
	if !eventTypesContain(types, observability.EventModelToolCallDelta) {
		t.Fatalf("model tool-call fact was lost before completion: events=%v", types)
	}
	select {
	case message := <-received:
		t.Fatalf("tool call escaped before RuntimeService acknowledgement: %#v", message)
	case err := <-recvErr:
		t.Fatalf("stream failed before RuntimeService acknowledgement: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	barrier.acknowledge(completed.EventID, nil)
	select {
	case message := <-received:
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "tc_1" {
			t.Fatalf("unexpected committed tool call: %#v", message)
		}
	case err := <-recvErr:
		t.Fatalf("stream failed after RuntimeService acknowledgement: %v", err)
	case <-ctx.Done():
		t.Fatal("tool call was not released after RuntimeService acknowledgement")
	}
}

func TestEinoToolChoiceMapsToCanonicalGatewayValues(t *testing.T) {
	tests := []struct {
		choice schema.ToolChoice
		want   string
	}{
		{choice: schema.ToolChoiceAllowed, want: "auto"},
		{choice: schema.ToolChoiceForbidden, want: "none"},
		{choice: schema.ToolChoiceForced, want: "required"},
	}
	for _, test := range tests {
		options := &model.Options{ToolChoice: &test.choice}
		if got := toModelCallOptions(options).ToolChoice; got != test.want {
			t.Fatalf("tool choice %q mapped to %q, want %q", test.choice, got, test.want)
		}
	}
}

func TestEinoChatModelProxyPreservesReasoningSignatureForNextToolRound(t *testing.T) {
	message := modelStreamItemToEino(ModelStreamItem{ReasoningDelta: "inspect", ReasoningSignature: "sig-opaque"}, nil, "deep_agent")
	message.ToolCalls = []schema.ToolCall{{ID: "tool_1", Type: "function", Function: schema.FunctionCall{Name: "search", Arguments: `{}`}}}
	converted := toModelCallMessages([]*schema.Message{message})
	if len(converted) != 1 || converted[0].ReasoningContent != "inspect" || converted[0].ReasoningSignature != "sig-opaque" || len(converted[0].ToolCalls) != 1 {
		t.Fatalf("reasoning signature was not preserved for the next round: %#v", converted)
	}
}

func TestEinoChatModelProxyConvertsUnauthorizedToolCallToText(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{
		modelEvent(observability.EventModelCallStarted),
		modelToolEvent(ModelToolCall{ToolCallID: "tc_unauthorized", Name: "hybrid_search_projects", Arguments: []byte(`{"concepts":["成长阵地"]}`)}),
		modelEvent(observability.EventModelCallCompleted),
	}}
	base, err := NewEinoChatModelProxy(invoker, validEinoModelPackage())
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := base.WithTools([]*schema.ToolInfo{{Name: "write_todos", Desc: "runtime todo tracker"}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := withTools.Generate(einoModelTestContext(), []*schema.Message{{Role: schema.User, Content: "find projects"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) != 0 || !strings.Contains(response.Content, `hybrid_search_projects`) || !strings.Contains(response.Content, "not installed or authorized") {
		t.Fatalf("unauthorized tool call was not converted to controlled text: %#v", response)
	}
}

func TestEinoChatModelProxyUsesFrozenToolDefinition(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	pkg := validEinoModelPackage()
	pkg.Capabilities.Tools = []string{"search@v2"}
	pkg.Capabilities.ToolSnapshot = &ToolSchemaSnapshot{
		SnapshotID: "tool_snapshot", ToolRefs: []string{"search@v2"},
		CapabilityHash: "sha256:capability", PolicyHash: "sha256:policy",
	}
	pkg.Capabilities.ToolDefinitions = []ModelToolDefinition{{
		Name: "search", Description: "frozen description", Schema: []byte(`{"type":"object","required":["query"]}`),
	}}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	base, err := NewEinoChatModelProxy(invoker, pkg)
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := base.WithTools([]*schema.ToolInfo{{
		Name: "search", Desc: "untrusted live description",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"other": {Type: schema.String},
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withTools.Generate(einoModelTestContext(), []*schema.Message{{Role: schema.User, Content: "find"}}); err != nil {
		t.Fatal(err)
	}
	tools := invoker.Requests()[0].Tools
	if len(tools) != 1 || tools[0].Description != "frozen description" || string(tools[0].Schema) != `{"type":"object","required":["query"]}` {
		t.Fatalf("live Eino schema replaced frozen definition: %#v", tools)
	}
}

func TestEinoChatModelProxyUsesFrozenMCPOnlyDefinition(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	pkg := validEinoModelPackage()
	pkg.Capabilities.MCPSnapshots = []mcp.CapabilitySnapshot{{
		ID: "mcp_snapshot", ServerID: "maps", CapabilityHash: "sha256:mcp",
		Tools: []mcp.Tool{{Name: "maps_search", InputSchema: []byte(`{"type":"object","required":["query"]}`)}},
	}}
	pkg.Capabilities.ToolDefinitions = []ModelToolDefinition{{
		Name: "maps_search", Description: "frozen MCP description", Schema: []byte(`{"type":"object","required":["query"]}`),
	}}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	base, err := NewEinoChatModelProxy(invoker, pkg)
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := base.WithTools([]*schema.ToolInfo{{Name: "maps_search", Desc: "live description"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withTools.Generate(einoModelTestContext(), []*schema.Message{{Role: schema.User, Content: "find"}}); err != nil {
		t.Fatal(err)
	}
	tools := invoker.Requests()[0].Tools
	if len(tools) != 1 || tools[0].Description != "frozen MCP description" || string(tools[0].Schema) != `{"type":"object","required":["query"]}` {
		t.Fatalf("live Eino schema replaced frozen MCP definition: %#v", tools)
	}
}

func TestEinoChatModelProxyRejectsEmptyFrozenToolSchema(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("must not run")}}
	pkg := validEinoModelPackage()
	pkg.Capabilities.Tools = []string{"search@v2"}
	pkg.Capabilities.ToolSnapshot = &ToolSchemaSnapshot{
		SnapshotID: "tool_snapshot", ToolRefs: []string{"search@v2"},
		CapabilityHash: "sha256:capability", PolicyHash: "sha256:policy",
	}
	pkg.Capabilities.ToolDefinitions = []ModelToolDefinition{{Name: "search"}}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	base, err := NewEinoChatModelProxy(invoker, pkg)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = base.WithTools([]*schema.ToolInfo{{Name: "search"}}); !errors.Is(err, ErrEinoToolProxyInfoMissing) {
		t.Fatalf("WithTools error=%v, want %v", err, ErrEinoToolProxyInfoMissing)
	}
	if requests := invoker.Requests(); len(requests) != 0 {
		t.Fatalf("invalid frozen schema reached ModelInvoker: %#v", requests)
	}
}

func TestEinoChatModelProxyWithToolsIsConcurrentAndImmutable(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	base, err := NewEinoChatModelProxy(invoker, validEinoModelPackage())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			derived, deriveErr := base.WithTools([]*schema.ToolInfo{{Name: "search"}})
			if deriveErr != nil {
				t.Errorf("WithTools: %v", deriveErr)
				return
			}
			if _, generateErr := derived.Generate(einoModelTestContext(), []*schema.Message{{Role: schema.User, Content: "hi"}}); generateErr != nil {
				t.Errorf("Generate: %v", generateErr)
			}
		}()
	}
	wg.Wait()
	if len(invoker.Requests()) != workers {
		t.Fatalf("requests=%d", len(invoker.Requests()))
	}
}

func TestEinoChatModelProxyDerivesPackageForEveryModelRound(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("ok")}}
	basePackage := validEinoModelPackage()
	proxy, err := NewEinoChatModelProxy(invoker, basePackage)
	if err != nil {
		t.Fatal(err)
	}
	emitter := &capturingRuntimeEmitter{}
	ctx := WithRuntimeEventEmitter(context.Background(), emitter)
	if _, err = proxy.Generate(ctx, []*schema.Message{{Role: schema.User, Content: "find it"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = proxy.Generate(ctx, []*schema.Message{
		{Role: schema.User, Content: "find it"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "tc_1", Function: schema.FunctionCall{Name: "search", Arguments: `{"q":"x"}`}}}},
		{Role: schema.Tool, ToolCallID: "tc_1", ToolName: "search", Content: "result"},
	}); err != nil {
		t.Fatal(err)
	}
	requests := invoker.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests=%d", len(requests))
	}
	if requests[0].Package.PackageID == requests[1].Package.PackageID || requests[0].Package.ContextHash == requests[1].Package.ContextHash {
		t.Fatalf("model rounds must have distinct packages: %#v", requests)
	}
	second := requests[1].Package.Messages.ConversationWindow
	if len(second) != 3 || second[2].ToolResult == nil || second[2].ToolResult.CallID != "tc_1" {
		t.Fatalf("tool result missing from second model package: %#v", second)
	}
	var contextEvents int
	for _, event := range emitter.events {
		if event.EventType == observability.EventModelContextBuilt {
			contextEvents++
		}
	}
	if contextEvents != 2 {
		t.Fatalf("per-round context events missing: %#v", emitter.events)
	}
}

func TestEinoChatModelProxyRejectsTamperedPackage(t *testing.T) {
	pkg := validEinoModelPackage()
	pkg.Messages.ConversationWindow = append(pkg.Messages.ConversationWindow, ModelContextMessage{Role: "user", Content: "tampered"})
	if _, err := NewEinoChatModelProxy(&capturingEinoModelInvoker{}, pkg); !errors.Is(err, ErrEinoChatModelPackageInvalid) {
		t.Fatalf("expected tampered package rejection, got %v", err)
	}
}

func TestEinoChatModelProxyRejectsInvalidDependenciesAndStream(t *testing.T) {
	if _, err := NewEinoChatModelProxy(nil, validEinoModelPackage()); !errors.Is(err, ErrEinoChatModelInvokerMissing) {
		t.Fatalf("err=%v", err)
	}
	invoker := &capturingEinoModelInvoker{returnNil: true}
	proxy, err := NewEinoChatModelProxy(invoker, validEinoModelPackage())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = proxy.Generate(einoModelTestContext(), nil); !errors.Is(err, ErrEinoChatModelStreamInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestEinoChatModelProxyBlocksOversizedFinalInputBeforeInvoker(t *testing.T) {
	invoker := &capturingEinoModelInvoker{items: []ModelStreamItem{modelTextEvent("must not run")}}
	pkg := validEinoModelPackage()
	pkg.RuntimeConstraints.TokenBudget.MaxInputTokens = 1
	pkg.ContextHash = ComputeModelContextHash(pkg)
	proxy, err := NewEinoChatModelProxy(invoker, pkg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = proxy.Generate(einoModelTestContext(), []*schema.Message{
		{Role: schema.System, Content: "required instruction"},
		{Role: schema.User, Content: "latest input"},
	})
	if !errors.Is(err, ErrModelInputBudgetExceeded) {
		t.Fatalf("error=%v", err)
	}
	if len(invoker.Requests()) != 0 {
		t.Fatal("oversized request reached model invoker")
	}
}

func einoModelTestContext() context.Context {
	return WithRuntimeEventEmitter(context.Background(), &capturingRuntimeEmitter{})
}

func validEinoModelPackage() ModelContextPackage {
	pkg := ModelContextPackage{
		SchemaVersion: ModelContextPackageSchemaVersion,
		PackageID:     "pkg_1",
		Run:           ModelContextRun{SessionID: "session_1", RunID: "run_1", AgentID: "agent_1", AgentVersion: "v1"},
	}
	pkg.ContextHash = ComputeModelContextHash(pkg)
	return pkg
}

type capturingEinoModelInvoker struct {
	mu        sync.Mutex
	requests  []ModelInvokeRequest
	items     []ModelStreamItem
	returnNil bool
}

type backpressuredModelDeltaEmitter struct {
	events []observability.EventType
}

type completionCommitGateEmitter struct {
	completionEntered chan struct{}
	releaseCompletion chan struct{}
	once              sync.Once
}

func newCompletionCommitGateEmitter() *completionCommitGateEmitter {
	return &completionCommitGateEmitter{completionEntered: make(chan struct{}), releaseCompletion: make(chan struct{})}
}

func (e *completionCommitGateEmitter) Emit(ctx context.Context, event observability.AgentEvent) error {
	if event.EventType != observability.EventModelCallCompleted {
		return nil
	}
	e.once.Do(func() { close(e.completionEntered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.releaseCompletion:
		return nil
	}
}

func (e *backpressuredModelDeltaEmitter) Emit(_ context.Context, event observability.AgentEvent) error {
	if event.EventType == observability.EventModelTokenDelta || event.EventType == observability.EventModelThoughtDelta {
		return ErrRuntimeEventBridgeFull
	}
	e.events = append(e.events, event.EventType)
	return nil
}

func (e *backpressuredModelDeltaEmitter) TryEmit(_ context.Context, event observability.AgentEvent) error {
	if event.EventType == observability.EventModelTokenDelta || event.EventType == observability.EventModelThoughtDelta {
		return ErrRuntimeEventBridgeFull
	}
	return e.Emit(context.Background(), event)
}

func (i *capturingEinoModelInvoker) Invoke(_ context.Context, req ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	i.mu.Lock()
	i.requests = append(i.requests, req)
	items := append([]ModelStreamItem(nil), i.items...)
	returnNil := i.returnNil
	i.mu.Unlock()
	if returnNil {
		return nil, nil
	}
	out := make(chan ModelStreamItem, len(items))
	for _, item := range items {
		out <- item
	}
	close(out)
	return out, nil
}

func (i *capturingEinoModelInvoker) Requests() []ModelInvokeRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]ModelInvokeRequest(nil), i.requests...)
}

func eventTypesContain(events []observability.EventType, target observability.EventType) bool {
	for _, event := range events {
		if event == target {
			return true
		}
	}
	return false
}
