package agentruntimetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// Contract is the reusable acceptance surface for every AgentRuntime adapter.
// Adapter packages should run this suite in addition to their engine-specific tests.
type Contract struct {
	Name       agentruntime.RuntimeType
	Definition agentruntime.AgentDefinition
	New        func(t *testing.T) agentruntime.AgentRuntime
	Context    func(req agentruntime.RunRequest) context.Context
	Resume     *ResumeContract
}

type ResumeContract struct {
	New     func(t *testing.T) agentruntime.AgentRuntime
	Prepare func(t *testing.T, runtime agentruntime.AgentRuntime) (context.Context, agentruntime.ResumeRequest)
}

// ValidateModelRequests is the reusable model-boundary conformance check for
// Runtime adapters. Adapter fixtures must pass every provider-bound request
// through this validator in addition to the lifecycle Contract above.
func ValidateModelRequests(requests []agentruntime.ModelInvokeRequest) error {
	if len(requests) == 0 {
		return errors.New("adapter emitted no model requests")
	}
	for i, request := range requests {
		policy, err := agentruntime.NormalizeContextCompactionPolicy(request.Package.RuntimeConstraints.CompactionPolicy)
		if err != nil {
			return fmt.Errorf("request[%d] has invalid compaction policy: %w", i, err)
		}
		if policy.PolicyHash == "" {
			return fmt.Errorf("request[%d] has no frozen compaction policy", i)
		}
		if request.PreserveManifest.ManifestHash == "" {
			return fmt.Errorf("request[%d] has no preserve manifest", i)
		}
		if !request.PreModelCompaction.Completed || request.PreModelCompaction.PolicyHash != policy.PolicyHash {
			return fmt.Errorf("request[%d] did not complete pre-model compaction for the frozen policy", i)
		}
		if err := agentruntime.ValidatePreserveManifest(request.PreserveManifest, request); err != nil {
			return fmt.Errorf("request[%d] violates preserve manifest: %w", i, err)
		}
		if request.AllowTools && len(request.Tools) == 0 {
			return fmt.Errorf("request[%d] enables tools without model-visible definitions", i)
		}
		toolNames := make(map[string]struct{}, len(request.Tools))
		for _, tool := range request.Tools {
			if tool.Name == "" || (len(tool.Schema) > 0 && !json.Valid(tool.Schema)) {
				return fmt.Errorf("request[%d] has invalid tool definition %q", i, tool.Name)
			}
			if _, duplicate := toolNames[tool.Name]; duplicate {
				return fmt.Errorf("request[%d] has duplicate model tool name %q", i, tool.Name)
			}
			toolNames[tool.Name] = struct{}{}
		}
	}
	return nil
}

func Run(t *testing.T, contract Contract) {
	t.Helper()
	if contract.New == nil || contract.Context == nil {
		t.Fatal("runtime conformance contract requires New and Context")
	}

	t.Run("metadata_and_build", func(t *testing.T) {
		runtime := contract.New(t)
		if runtime == nil {
			t.Fatal("runtime factory returned nil")
		}
		if got := agentruntime.RuntimeType(runtime.Name()); got != contract.Name {
			t.Fatalf("runtime name = %s, want %s", got, contract.Name)
		}
		descriptor := runtime.Descriptor(context.Background())
		if descriptor.Name != contract.Name || descriptor.RuntimeVersion == "" || descriptor.AdapterVersion == "" || descriptor.Governance == "" {
			t.Fatalf("runtime descriptor is incomplete: %#v", descriptor)
		}
		if descriptor.Capabilities != runtime.Capabilities(context.Background()) {
			t.Fatalf("descriptor capabilities drifted from runtime capabilities: descriptor=%#v runtime=%#v", descriptor.Capabilities, runtime.Capabilities(context.Background()))
		}
		if health := runtime.Health(context.Background()); !health.Available {
			t.Fatalf("runtime must be healthy in conformance fixture: %#v", health)
		}
		if err := runtime.ValidateConfig(context.Background(), contract.Definition); err != nil {
			t.Fatalf("valid definition rejected: %v", err)
		}
		handle, err := runtime.Build(context.Background(), contract.Definition)
		if err != nil {
			t.Fatalf("build failed: %v", err)
		}
		if handle.Runtime != contract.Name || handle.Definition.AgentID != contract.Definition.AgentID || handle.BuiltAt.IsZero() {
			t.Fatalf("invalid handle: %#v", handle)
		}
		if err := handle.Binding.Validate(); err != nil || handle.Binding.Runtime != contract.Name {
			t.Fatalf("invalid runtime binding: binding=%#v err=%v", handle.Binding, err)
		}
	})

	t.Run("invalid_identity_fails_closed", func(t *testing.T) {
		runtime := contract.New(t)
		definition := contract.Definition
		definition.AgentID = ""
		if err := runtime.ValidateConfig(context.Background(), definition); err == nil {
			t.Fatal("missing agent_id must be rejected")
		}
		definition = contract.Definition
		definition.Version = ""
		if err := runtime.ValidateConfig(context.Background(), definition); err == nil {
			t.Fatal("missing agent version must be rejected")
		}
	})

	t.Run("canonical_stream", func(t *testing.T) {
		runtime := contract.New(t)
		req := request(contract.Definition, "single")
		events, err := runtime.Run(contract.Context(req), req)
		if err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if events == nil {
			t.Fatal("runtime returned nil event stream")
		}
		var collected []observability.AgentEvent
		for event := range events {
			validateEvent(t, event)
			collected = append(collected, event)
		}
		if len(collected) == 0 {
			t.Fatal("runtime emitted no canonical events")
		}
		if err := ValidateStream(collected); err != nil {
			t.Fatalf("runtime emitted an invalid canonical lifecycle: %v", err)
		}
	})

	t.Run("concurrent_run_isolation", func(t *testing.T) {
		runtime := contract.New(t)
		const runs = 16
		var wg sync.WaitGroup
		errs := make(chan error, runs)
		for i := 0; i < runs; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := request(contract.Definition, fmt.Sprintf("%d", i))
				events, err := runtime.Run(contract.Context(req), req)
				if err != nil {
					errs <- err
					return
				}
				for event := range events {
					if event.EventType == "" {
						errs <- fmt.Errorf("run %s emitted empty event type", req.RunID)
						return
					}
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})

	t.Run("context_cancellation_closes_stream", func(t *testing.T) {
		runtime := contract.New(t)
		req := request(contract.Definition, "cancel")
		ctx, cancel := context.WithCancel(contract.Context(req))
		events, err := runtime.Run(ctx, req)
		if err != nil {
			t.Fatalf("run failed: %v", err)
		}
		cancel()
		// Do not consume immediately. An adapter blocked by a slow downstream
		// must still observe cancellation and release its producer.
		time.Sleep(25 * time.Millisecond)
		done := make(chan struct{})
		go func() {
			for range events {
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runtime stream did not close after context cancellation")
		}
	})

	t.Run("resume_matches_declared_capability", func(t *testing.T) {
		runtime := contract.New(t)
		capabilities := runtime.Capabilities(context.Background())
		if !capabilities.Resume && !capabilities.Checkpoint && !capabilities.ControlRequest {
			req := request(contract.Definition, "unsupported_resume")
			_, err := runtime.Resume(contract.Context(req), agentruntime.ResumeRequest{
				SessionID:  req.SessionID,
				RunID:      req.RunID,
				Definition: req.Definition,
			})
			if err == nil {
				t.Fatal("adapter without checkpoint/control capability must reject Resume")
			}
			return
		}
		if contract.Resume == nil || contract.Resume.New == nil || contract.Resume.Prepare == nil {
			t.Fatal("adapter declaring checkpoint/control capability requires a Resume conformance fixture")
		}
		runtime = contract.Resume.New(t)
		ctx, req := contract.Resume.Prepare(t, runtime)
		events, err := runtime.Resume(ctx, req)
		if err != nil {
			t.Fatalf("resume failed: %v", err)
		}
		count := 0
		for event := range events {
			count++
			validateEvent(t, event)
		}
		if count == 0 {
			t.Fatal("resume emitted no canonical events")
		}
	})
}

// ValidateStream checks lifecycle invariants owned by a raw Runtime Adapter.
// Event identity and persistence idempotency belong to RuntimeService/EventStore
// and are intentionally outside this validator.
func ValidateStream(events []observability.AgentEvent) error {
	if len(events) == 0 {
		return errors.New("empty runtime event stream")
	}
	var modelActive bool
	var agentActive bool
	var agentLifecycleSeen bool
	for i, event := range events {
		if event.EventType == "" {
			return fmt.Errorf("event[%d] has empty event_type", i)
		}
		if isRunTerminal(event.EventType) && i != len(events)-1 {
			return fmt.Errorf("event[%d] %s is not the final event", i, event.EventType)
		}
		switch event.EventType {
		case observability.EventModelCallStarted:
			if modelActive {
				return fmt.Errorf("event[%d] starts a model call before the previous call terminated", i)
			}
			modelActive = true
		case observability.EventModelTokenDelta, observability.EventModelThoughtDelta,
			observability.EventModelToolCallDelta, observability.EventModelUsageDelta:
			if !modelActive {
				return fmt.Errorf("event[%d] %s has no active model call", i, event.EventType)
			}
		case observability.EventModelCallCompleted, observability.EventModelCallFailed:
			if !modelActive {
				return fmt.Errorf("event[%d] terminates a model call that was not started", i)
			}
			modelActive = false
		case observability.EventAgentStarted:
			if agentActive {
				return fmt.Errorf("event[%d] starts an agent before the previous agent lifecycle terminated", i)
			}
			agentLifecycleSeen = true
			agentActive = true
		case observability.EventAgentCompleted, observability.EventAgentFailed:
			if !agentActive {
				return fmt.Errorf("event[%d] terminates an agent that was not started", i)
			}
			agentActive = false
		}
	}
	if modelActive {
		return errors.New("model call lifecycle is incomplete")
	}
	if agentLifecycleSeen && agentActive {
		return errors.New("agent lifecycle is incomplete")
	}
	return nil
}

func isRunTerminal(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventRunCompleted, observability.EventRunFailed,
		observability.EventRunCancelled, observability.EventRunExpired:
		return true
	default:
		return false
	}
}

func request(definition agentruntime.AgentDefinition, suffix string) agentruntime.RunRequest {
	return agentruntime.RunRequest{
		SessionID:  "session_" + suffix,
		RunID:      "run_" + suffix,
		Definition: definition,
		Input:      []agentruntime.Message{{Role: "user", Content: "hello " + suffix}},
		Trace:      observability.TraceContext{TraceID: "trace_" + suffix},
	}
}

func validateEvent(t *testing.T, event observability.AgentEvent) {
	t.Helper()
	if event.EventType == "" {
		t.Fatal("adapter emitted empty event type")
	}
	if event.Visibility != "" && event.Visibility != observability.VisibilityUserVisible && event.Visibility != observability.VisibilityDebug && event.Visibility != observability.VisibilityInternal && event.Visibility != observability.VisibilityRestricted {
		t.Fatalf("adapter emitted invalid visibility: %s", event.Visibility)
	}
}
